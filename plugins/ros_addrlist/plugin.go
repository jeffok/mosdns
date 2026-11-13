/*
 * Copyright (C) 2020-2022, IrineSistiana
 * Copyright (C) 2026, jeffok
 *
 * 参考未合并的上游 PR https://github.com/IrineSistiana/mosdns/pull/864 （qiudaomao），
 * 重写为：先选定最终应答，再按来源逐个 IP 确认 RouterOS 地址池租约。
 *
 * This file is part of mosdns.
 *
 * mosdns is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * mosdns is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <https://www.gnu.org/licenses/>.
 */

package ros_addrlist

import (
	"bufio"
	"context"
	"fmt"
	"net/netip"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/IrineSistiana/mosdns/v5/coremain"
	"github.com/IrineSistiana/mosdns/v5/pkg/query_context"
	"github.com/IrineSistiana/mosdns/v5/plugin/executable/sequence"
	"github.com/miekg/dns"
	"github.com/prometheus/client_golang/prometheus"
	"go4.org/netipx"
	"golang.org/x/sync/singleflight"
)

const PluginType = "ros_addrlist"

func init() {
	coremain.RegNewPluginFunc(PluginType, Init, func() any { return new(Args) })
	registerMetrics()
}

// Args 由 entrypoint 渲染。密码只从环境变量读取，不写进配置文件。
type Args struct {
	Mode        string `yaml:"mode"` // tag 或 confirm
	SourceKey   string `yaml:"source_key"`
	SourceTTL   string `yaml:"source_ttl"`
	StateFile   string `yaml:"state_file"`
	Server      string `yaml:"server"`
	User        string `yaml:"user"`
	PasswdEnv   string `yaml:"passwd_env"`
	AddrList    string `yaml:"addrlist"`
	Timeout     string `yaml:"timeout"`
	SyncBudget  string `yaml:"sync_budget"`
	Concurrency int    `yaml:"concurrency"`
	Queue       int    `yaml:"queue"`
	CNIPFile    string `yaml:"cn_ip_file"`
}

type lease struct {
	id      string
	expires time.Time
	static  bool
}

type confirmPlugin struct {
	store      *sourceStore
	client     *rosClient
	cn         *netipx.IPSet
	timeout    time.Duration
	rosTimeout string
	budget     time.Duration
	sem        chan struct{}
	queue      chan writeJob
	stop       chan struct{}
	stopOnce   sync.Once
	flight     singleflight.Group
	leases     sync.Map // string(ip) -> lease
	br         breaker
	logf       func(string, ...any)
	wg         sync.WaitGroup
}

type writeJob struct {
	ip      netip.Addr
	qname   string
	refresh bool
	id      string
}

type breaker struct {
	mu        sync.Mutex
	fails     int
	openUntil time.Time
}

func (b *breaker) allow(now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return now.After(b.openUntil)
}

func (b *breaker) ok() {
	b.mu.Lock()
	b.fails = 0
	b.openUntil = time.Time{}
	b.mu.Unlock()
}

func (b *breaker) fail(now time.Time) {
	b.mu.Lock()
	b.fails++
	if b.fails >= 5 {
		b.openUntil = now.Add(30 * time.Second)
		b.fails = 0
		statBreaker.Add(1)
	}
	b.mu.Unlock()
}

var (
	statSyncWrite   atomic.Uint64
	statSyncTimeout atomic.Uint64
	statRefreshDrop atomic.Uint64
	statBreaker     atomic.Uint64
	statLeaseHit    atomic.Uint64
	metricsOnce     sync.Once
)

func registerMetrics() {
	metricsOnce.Do(func() {
		pairs := []struct {
			name string
			c    *atomic.Uint64
		}{
			{"mosdns_ros_addrlist_sync_writes_total", &statSyncWrite},
			{"mosdns_ros_addrlist_sync_timeouts_total", &statSyncTimeout},
			{"mosdns_ros_addrlist_refresh_drops_total", &statRefreshDrop},
			{"mosdns_ros_addrlist_breaker_opens_total", &statBreaker},
			{"mosdns_ros_addrlist_lease_hits_total", &statLeaseHit},
		}
		for _, p := range pairs {
			c := p.c
			prometheus.MustRegister(prometheus.NewCounterFunc(prometheus.CounterOpts{Name: p.name}, func() float64 {
				return float64(c.Load())
			}))
		}
	})
}

func Init(bp *coremain.BP, args any) (any, error) {
	a := args.(*Args)
	switch a.Mode {
	case "tag":
		return newTag(a)
	case "confirm":
		p, err := newConfirm(a)
		if err != nil {
			return nil, err
		}
		if bp != nil && bp.L() != nil {
			p.logf = func(format string, v ...any) {
				bp.L().Sugar().Infof(format, v...)
			}
		}
		return p, nil
	default:
		return nil, fmt.Errorf("ros_addrlist: mode 必须是 tag 或 confirm，得到 %q", a.Mode)
	}
}

type tagPlugin struct {
	store *sourceStore
	ttl   time.Duration
}

func newTag(a *Args) (*tagPlugin, error) {
	ttl, err := time.ParseDuration(defaultStr(a.SourceTTL, "600s"))
	if err != nil {
		return nil, fmt.Errorf("source_ttl: %w", err)
	}
	return &tagPlugin{store: sharedStore(a.SourceKey, a.StateFile), ttl: ttl}, nil
}

func (p *tagPlugin) Exec(_ context.Context, qCtx *query_context.Context) error {
	r := qCtx.R()
	if r == nil {
		return nil
	}
	exp := time.Now().Add(p.ttl)
	for _, ip := range answerIPv4(r) {
		if skipIP(ip) {
			continue
		}
		p.store.Remember(qCtx.QQuestion().Name, ip, exp)
	}
	return nil
}

func newConfirm(a *Args) (*confirmPlugin, error) {
	if a.Server == "" {
		return nil, fmt.Errorf("ros_addrlist: server 为空")
	}
	env := a.PasswdEnv
	if env == "" {
		env = "ROS_PASS"
	}
	pass := os.Getenv(env)
	if pass == "" {
		return nil, fmt.Errorf("ros_addrlist: 环境变量 %s 为空", env)
	}
	user := a.User
	if user == "" {
		user = "admin"
	}
	list := a.AddrList
	if list == "" {
		list = "gfw-proxy"
	}
	timeout, err := time.ParseDuration(defaultStr(a.Timeout, "1h"))
	if err != nil {
		return nil, fmt.Errorf("timeout: %w", err)
	}
	budget, err := time.ParseDuration(defaultStr(a.SyncBudget, "80ms"))
	if err != nil {
		return nil, fmt.Errorf("sync_budget: %w", err)
	}
	n := a.Concurrency
	if n <= 0 {
		n = 4
	}
	qlen := a.Queue
	if qlen <= 0 {
		qlen = 1024
	}
	cn, err := loadCN(a.CNIPFile)
	if err != nil {
		return nil, err
	}
	p := &confirmPlugin{
		store:      sharedStore(a.SourceKey, a.StateFile),
		client:     newROSClient(a.Server, user, pass, list),
		cn:         cn,
		timeout:    timeout,
		rosTimeout: defaultStr(a.Timeout, "1h"),
		budget:     budget,
		sem:        make(chan struct{}, n),
		queue:      make(chan writeJob, qlen),
		stop:       make(chan struct{}),
		logf:       func(string, ...any) {},
	}
	for i := 0; i < n; i++ {
		p.wg.Add(1)
		go p.worker()
	}
	p.preload()
	return p, nil
}

func (p *confirmPlugin) Close() error {
	p.stopOnce.Do(func() { close(p.stop) })
	p.wg.Wait()
	return nil
}

func (p *confirmPlugin) Exec(ctx context.Context, qCtx *query_context.Context) error {
	r := qCtx.R()
	if r == nil {
		return nil
	}
	if !p.br.allow(time.Now()) {
		return nil
	}
	qname := qCtx.QQuestion().Name
	now := time.Now()
	var syncJobs []writeJob
	for _, ip := range answerIPv4(r) {
		if !p.store.Has(qname, ip, now) || skipIP(ip) || (p.cn != nil && p.cn.Contains(ip)) {
			continue
		}
		if l, ok := p.leaseOf(ip); ok && l.static {
			statLeaseHit.Add(1)
			continue
		}
		if l, ok := p.leaseOf(ip); ok && l.expires.After(now.Add(p.timeout/2)) {
			statLeaseHit.Add(1)
			continue
		}
		if l, ok := p.leaseOf(ip); ok && l.expires.After(now) {
			p.enqueue(writeJob{ip: ip, qname: qname, refresh: true, id: l.id})
			continue
		}
		syncJobs = append(syncJobs, writeJob{ip: ip, qname: qname})
	}
	if len(syncJobs) == 0 {
		return nil
	}
	p.waitSync(ctx, syncJobs)
	return nil
}

func (p *confirmPlugin) waitSync(ctx context.Context, jobs []writeJob) {
	done := make(chan struct{})
	go func() {
		var wg sync.WaitGroup
		for _, job := range jobs {
			wg.Add(1)
			go func(job writeJob) {
				defer wg.Done()
				_, _, _ = p.flight.Do(job.ip.String(), func() (any, error) {
					p.writeOne(context.Background(), job)
					return nil, nil
				})
			}(job)
		}
		wg.Wait()
		close(done)
	}()
	timer := time.NewTimer(p.budget)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		statSyncTimeout.Add(1)
	case <-ctx.Done():
	}
}

func (p *confirmPlugin) enqueue(job writeJob) {
	select {
	case <-p.stop:
	case p.queue <- job:
	default:
		statRefreshDrop.Add(1)
	}
}

func (p *confirmPlugin) worker() {
	defer p.wg.Done()
	for {
		select {
		case <-p.stop:
			return
		case job := <-p.queue:
			p.writeOne(context.Background(), job)
		}
	}
}

func (p *confirmPlugin) writeOne(ctx context.Context, job writeJob) {
	if !p.br.allow(time.Now()) {
		return
	}
	select {
	case p.sem <- struct{}{}:
		defer func() { <-p.sem }()
	case <-p.stop:
		return
	}
	ip := job.ip.String()
	comment := commentPrefix + " " + trimComment(normalizeQName(job.qname))
	ctx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	if job.refresh && job.id != "" {
		err := p.client.PatchTimeout(ctx, job.id, p.rosTimeout)
		if err == nil {
			p.rememberLease(job.ip, lease{id: job.id, expires: time.Now().Add(p.timeout)})
			p.br.ok()
			return
		}
		if err != errNotFound {
			p.noteErr("patch %s: %v", ip, err)
			return
		}
	}
	id, err := p.client.Add(ctx, ip, p.rosTimeout, comment)
	if err == nil {
		statSyncWrite.Add(1)
		p.rememberLease(job.ip, lease{id: id, expires: time.Now().Add(p.timeout)})
		p.br.ok()
		return
	}
	if _, dup := err.(errDuplicate); dup {
		entries, ferr := p.client.Find(ctx, ip)
		if ferr != nil {
			p.noteErr("find %s: %v", ip, ferr)
			return
		}
		if len(entries) == 0 {
			p.noteErr("add %s duplicate but find empty", ip)
			return
		}
		e := entries[0]
		if !isDynamicComment(e.Comment) {
			p.rememberLease(job.ip, lease{id: e.ID, static: true})
			p.br.ok()
			return
		}
		exp := time.Now().Add(p.timeout)
		if d, ok := parseROSDuration(e.Timeout); ok {
			exp = time.Now().Add(d)
		}
		if e.ID != "" {
			_ = p.client.PatchTimeout(ctx, e.ID, p.rosTimeout)
			exp = time.Now().Add(p.timeout)
		}
		p.rememberLease(job.ip, lease{id: e.ID, expires: exp})
		p.br.ok()
		return
	}
	p.noteErr("add %s: %v", ip, err)
}

func (p *confirmPlugin) noteErr(format string, args ...any) {
	p.br.fail(time.Now())
	p.logf("ros_addrlist "+format, args...)
}

func (p *confirmPlugin) rememberLease(ip netip.Addr, l lease) {
	p.leases.Store(ip.String(), l)
}

func (p *confirmPlugin) leaseOf(ip netip.Addr) (lease, bool) {
	v, ok := p.leases.Load(ip.String())
	if !ok {
		return lease{}, false
	}
	return v.(lease), true
}

func (p *confirmPlugin) preload() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	entries, err := p.client.List(ctx)
	if err != nil {
		p.logf("ros_addrlist preload: %v", err)
		return
	}
	now := time.Now()
	for _, e := range entries {
		ip, err := netip.ParseAddr(e.Address)
		if err != nil || !ip.Is4() {
			continue
		}
		if !isDynamicComment(e.Comment) {
			p.rememberLease(ip, lease{id: e.ID, static: true})
			continue
		}
		exp := now.Add(time.Second)
		if d, ok := parseROSDuration(e.Timeout); ok {
			exp = now.Add(d)
		}
		p.rememberLease(ip, lease{id: e.ID, expires: exp})
	}
}

func answerIPv4(r *dns.Msg) []netip.Addr {
	var out []netip.Addr
	for _, rr := range r.Answer {
		a, ok := rr.(*dns.A)
		if !ok || a.A == nil {
			continue
		}
		ip, ok := netip.AddrFromSlice(a.A.To4())
		if !ok {
			continue
		}
		out = append(out, ip.Unmap())
	}
	return out
}

func skipIP(ip netip.Addr) bool {
	if !ip.IsValid() || !ip.IsGlobalUnicast() {
		return true
	}
	// CGNAT 100.64.0.0/10
	if cgnat.Contains(ip) {
		return true
	}
	return false
}

var cgnat = netip.MustParsePrefix("100.64.0.0/10")

func loadCN(path string) (*netipx.IPSet, error) {
	if path == "" {
		return nil, fmt.Errorf("ros_addrlist: cn_ip_file 为空")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("ros_addrlist: 打开 %s: %w", path, err)
	}
	defer f.Close()
	var b netipx.IPSetBuilder
	n := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.Fields(line)[0]
		p, err := netip.ParsePrefix(line)
		if err != nil {
			addr, err2 := netip.ParseAddr(line)
			if err2 != nil {
				continue
			}
			p = netip.PrefixFrom(addr, addr.BitLen())
		}
		b.AddPrefix(p)
		n++
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, fmt.Errorf("ros_addrlist: %s 没有可用前缀", path)
	}
	return b.IPSet()
}

func parseROSDuration(s string) (time.Duration, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	if d, err := time.ParseDuration(s); err == nil {
		return d, true
	}
	if strings.HasSuffix(s, "d") {
		var days int
		if _, err := fmt.Sscanf(s, "%dd", &days); err == nil {
			return time.Duration(days) * 24 * time.Hour, true
		}
	}
	parts := strings.Split(s, ":")
	if len(parts) == 3 {
		var h, m, sec int
		if _, err := fmt.Sscanf(parts[0], "%d", &h); err == nil {
			if _, err := fmt.Sscanf(parts[1], "%d", &m); err == nil {
				if _, err := fmt.Sscanf(parts[2], "%d", &sec); err == nil {
					return time.Duration(h)*time.Hour + time.Duration(m)*time.Minute + time.Duration(sec)*time.Second, true
				}
			}
		}
	}
	return 0, false
}

func trimComment(q string) string {
	if len(q) > 120 {
		return q[:120]
	}
	return q
}

func defaultStr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

var _ sequence.Executable = (*tagPlugin)(nil)
var _ sequence.Executable = (*confirmPlugin)(nil)
