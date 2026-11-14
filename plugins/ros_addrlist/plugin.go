// Package ros_addrlist 把最终应答里的海外 IPv4 写入 RouterOS address-list。
// 用法：挂在 main_sequence 末端（只读已定应答，旁路回写，不改应答）。
package ros_addrlist

import (
	"bufio"
	"context"
	"fmt"
	"net/netip"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/IrineSistiana/mosdns/v5/coremain"
	"github.com/IrineSistiana/mosdns/v5/pkg/query_context"
	"github.com/IrineSistiana/mosdns/v5/plugin/executable/sequence"
	"github.com/miekg/dns"
	"go.uber.org/zap"
)

const PluginType = "ros_addrlist"

type Args struct {
	Address    string `yaml:"address"`     // ROS API 主机:端口，例如 192.168.88.254:8728
	Username   string `yaml:"username"`    // ROS API 账号
	Password   string `yaml:"password"`    // ROS API 密码
	ListName   string `yaml:"list_name"`   // 目标 address-list，例如 gfw-proxy
	Timeout    string `yaml:"timeout"`     // 条目租约，例如 24h（ROS duration 语法）
	Comment    string `yaml:"comment"`     // 条目备注
	QueueSize  int    `yaml:"queue_size"`  // 回写队列容量
	DedupTTL   string `yaml:"dedup_ttl"`   // 同 IP 去重窗口，例如 5m
	CNIPFile   string `yaml:"cn_ip_file"`  // 国内 IP 段文件（china_ip_list）
	Workers    int    `yaml:"workers"`     // 并发写入 worker 数
	DialTimeout  string `yaml:"dial_timeout"`
	WriteTimeout string `yaml:"write_timeout"`
	ReconnDelay  string `yaml:"reconn_delay"`
}

func init() {
	coremain.RegNewPluginFunc(PluginType, Init, func() any { return new(Args) })
}

func Init(bp *coremain.BP, args any) (any, error) {
	a := args.(*Args)
	return newAddrListWriter(bp, a)
}

func defaultStr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
func defaultInt(s, def int) int {
	if s <= 0 {
		return def
	}
	return s
}

var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// 不可写网段：私网、保留、组播、环回、链路本地、benchmark、文档示例段等
var invalidNets = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("255.255.255.255/32"),
}

func loadIPSet(path string) (*ipSet, error) {
	if path == "" {
		return nil, fmt.Errorf("ros_addrlist: cn_ip_file empty")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var b ipSetBuilder
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.Fields(line)[0]
		p, err := netip.ParsePrefix(line)
		if err != nil {
			addr, e := netip.ParseAddr(line)
			if e != nil {
				continue
			}
			p = netip.PrefixFrom(addr, addr.BitLen())
		}
		b.Add(p)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return b.Build(), nil
}

// ipSet 简单前缀集合（足够匹配国内段；规模 ~6K 前缀）
type ipSet struct{ prefixes []netip.Prefix }
func (s *ipSet) Contains(ip netip.Addr) bool {
	for _, p := range s.prefixes {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}
type ipSetBuilder struct{ m map[netip.Prefix]struct{} }
func (b *ipSetBuilder) Add(p netip.Prefix) {
	if b.m == nil {
		b.m = map[netip.Prefix]struct{}{}
	}
	b.m[p.Masked()] = struct{}{}
}
func (b *ipSetBuilder) Build() *ipSet {
	out := &ipSet{}
	for p := range b.m {
		out.prefixes = append(out.prefixes, p)
	}
	return out
}

// ---- 插件本体 ----

type addrListWriter struct {
	cfg    Args
	logger *zap.Logger
	cn     *ipSet
	queue  chan string
	dedup  *dedupCache
	cancel context.CancelFunc
	wg     sync.WaitGroup
	rcMu   sync.Mutex
	rc     *rosClient
	stats  struct {
		mu         sync.Mutex
		enqueued   uint64
		deduped    uint64
		dropped    uint64
		written    uint64
		writeErr   uint64
	}
}

func newAddrListWriter(bp *coremain.BP, a *Args) (*addrListWriter, error) {
	if a.Address == "" {
		return nil, fmt.Errorf("ros_addrlist: address empty")
	}
	if a.Username == "" || a.Password == "" {
		return nil, fmt.Errorf("ros_addrlist: username/password empty")
	}
	a.ListName = defaultStr(a.ListName, "gfw-proxy")
	a.Timeout = defaultStr(a.Timeout, "24h")
	a.Comment = defaultStr(a.Comment, "mosdns-learned")
	a.QueueSize = defaultInt(a.QueueSize, 2048)
	a.DedupTTL = defaultStr(a.DedupTTL, "5m")
	a.DialTimeout = defaultStr(a.DialTimeout, "5s")
	a.WriteTimeout = defaultStr(a.WriteTimeout, "5s")
	a.ReconnDelay = defaultStr(a.ReconnDelay, "2s")
	a.Workers = defaultInt(a.Workers, 1)

	cn, err := loadIPSet(a.CNIPFile)
	if err != nil {
		return nil, fmt.Errorf("ros_addrlist: load cn_ip_file: %w", err)
	}

	dedupTTL, err := time.ParseDuration(a.DedupTTL)
	if err != nil {
		return nil, fmt.Errorf("ros_addrlist: dedup_ttl: %w", err)
	}

	w := &addrListWriter{
		cfg:    *a,
		logger: bp.L(),
		cn:     cn,
		queue:  make(chan string, a.QueueSize),
		dedup:  newDedupCache(dedupTTL),
	}
	ctx, cancel := context.WithCancel(context.Background())
	w.cancel = cancel
	for i := 0; i < a.Workers; i++ {
		w.wg.Add(1)
		go w.runWorker(ctx)
	}
	return w, nil
}

// Exec 只读应答、提取海外 IPv4 入队。绝不阻塞/修改应答。
func (w *addrListWriter) Exec(_ context.Context, q *query_context.Context) error {
	r := q.R()
	if r == nil || r.Rcode != dns.RcodeSuccess {
		return nil
	}
	for _, rr := range r.Answer {
		a, ok := rr.(*dns.A)
		if !ok {
			continue
		}
		raw := a.A
		if raw == nil {
			continue
		}
		ip, ok := netip.AddrFromSlice(raw.To4())
		if !ok {
			continue
		}
		ip = ip.Unmap()
		if !ip.IsGlobalUnicast() {
			continue
		}
		if w.skipIP(ip) {
			continue
		}
		w.enqueue(ip.String())
	}
	return nil
}

func (w *addrListWriter) skipIP(ip netip.Addr) bool {
	if ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return true
	}
	for _, p := range invalidNets {
		if p.Contains(ip) {
			return true
		}
	}
	if w.cn != nil && w.cn.Contains(ip) {
		return true
	}
	return false
}

func (w *addrListWriter) enqueue(ip string) {
	if w.dedup.Check(ip) {
		w.stats.mu.Lock()
		w.stats.deduped++
		w.stats.mu.Unlock()
		return
	}
	select {
	case w.queue <- ip:
		w.stats.mu.Lock()
		w.stats.enqueued++
		w.stats.mu.Unlock()
	default:
		w.stats.mu.Lock()
		w.stats.dropped++
		w.stats.mu.Unlock()
	}
}

// Close 释放 worker 并关闭 ROS 连接。
func (w *addrListWriter) Close() error {
	if w.cancel != nil {
		w.cancel()
		w.wg.Wait()
	}
	w.rcMu.Lock()
	if w.rc != nil {
		w.rc.Close()
		w.rc = nil
	}
	w.rcMu.Unlock()
	return nil
}

var _ sequence.Executable = (*addrListWriter)(nil)
