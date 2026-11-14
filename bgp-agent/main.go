// SPDX-License-Identifier: GPL-3.0-or-later
// BGP validation service. Does not contain RouterOS management credentials.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/miekg/dns"
	api "github.com/osrg/gobgp/v3/api"
	"github.com/osrg/gobgp/v3/pkg/server"
	"golang.org/x/sync/singleflight"
	"google.golang.org/protobuf/types/known/anypb"
)

const version = "bgp-validation-20261114.1"

type config struct {
	LocalIP       string   `json:"local_ip"`
	PeerIP        string   `json:"peer_ip"`
	ASN           uint32   `json:"asn"`
	PeerASN       uint32   `json:"peer_asn"`
	NextHop       string   `json:"next_hop"`
	DNSListen     string   `json:"dns_listen"`
	HTTPListen    string   `json:"http_listen"`
	Upstreams     []string `json:"upstreams"`
	Warm          []string `json:"warm_domains"`
	CNFile        string   `json:"cn_file"`
	StateFile     string   `json:"state_file"`
	TTLMax        uint32   `json:"ttl_max"`
	RetainSeconds int      `json:"retain_seconds"`
	ConfirmMS     int      `json:"confirm_ms"`
	AckMaxAgeMS   int      `json:"ack_max_age_ms"`
	MaxRoutes     int      `json:"max_routes"`
	GCDryRun      bool     `json:"gc_dry_run"`
}
type lease struct {
	Prefix      string               `json:"prefix"`
	LastSeen    time.Time            `json:"last_seen"`
	RetainUntil time.Time            `json:"retain_until"`
	AnswerUntil time.Time            `json:"answer_until"`
	References  map[string]time.Time `json:"references"`
	AckAt       time.Time            `json:"ack_at,omitempty"`
	UUID        []byte               `json:"-"`
}
type cached struct {
	msg             *dns.Msg
	stored, expires time.Time
}
type publication interface {
	Add(string) ([]byte, error)
	Delete([]byte) error
}
type bgpPublisher struct {
	s   *server.BgpServer
	hop string
}

func (p bgpPublisher) Add(prefix string) ([]byte, error) {
	ctx, c := context.WithTimeout(context.Background(), time.Second)
	defer c()
	return p.AddContext(ctx, prefix)
}
func (p bgpPublisher) AddContext(ctx context.Context, prefix string) ([]byte, error) {
	n := netip.MustParsePrefix(prefix)
	nlri, _ := anypb.New(&api.IPAddressPrefix{Prefix: n.Addr().String(), PrefixLen: uint32(n.Bits())})
	origin, _ := anypb.New(&api.OriginAttribute{Origin: 0})
	nh, _ := anypb.New(&api.NextHopAttribute{NextHop: p.hop})
	r, e := p.s.AddPath(ctx, &api.AddPathRequest{Path: &api.Path{Family: &api.Family{Afi: api.Family_AFI_IP, Safi: api.Family_SAFI_UNICAST}, Nlri: nlri, Pattrs: []*anypb.Any{origin, nh}}})
	if e != nil {
		return nil, e
	}
	return r.Uuid, nil
}
func (p bgpPublisher) Delete(id []byte) error {
	ctx, c := context.WithTimeout(context.Background(), time.Second)
	defer c()
	return p.s.DeletePath(ctx, &api.DeletePathRequest{Uuid: id, Family: &api.Family{Afi: api.Family_AFI_IP, Safi: api.Family_SAFI_UNICAST}})
}

type app struct {
	cfg                                        config
	mu                                         sync.Mutex
	persistMu                                  sync.Mutex
	workOnce                                   sync.Once
	work                                       chan struct{}
	dnsOnce                                    sync.Once
	slots                                      chan struct{}
	dirty                                      bool
	routes                                     map[string]*lease
	cache                                      map[string]cached
	epoch                                      string
	up                                         bool
	pub                                        publication
	cn                                         []netip.Prefix
	flight                                     singleflight.Group
	stop                                       chan struct{}
	wg                                         sync.WaitGroup
	requests, hits, failures, adds, candidates atomic.Uint64
}

func token() string {
	b := make([]byte, 12)
	if _, e := rand.Read(b); e != nil {
		panic(e)
	}
	return hex.EncodeToString(b)
}
func newApp(c config, p publication) (*app, error) {
	if c.TTLMax == 0 || c.TTLMax > 300 || c.RetainSeconds < 300 || c.ConfirmMS < 1 || c.AckMaxAgeMS < 1 || c.MaxRoutes < 1 {
		return nil, errors.New("invalid TTL/retention/confirmation/capacity configuration")
	}
	if len(c.Warm) > 32 || len(c.Upstreams) > 3 {
		return nil, errors.New("too many warm domains or upstreams")
	}
	if len(c.Upstreams) == 0 {
		return nil, errors.New("upstreams required")
	}
	for _, s := range c.Upstreams {
		if _, e := netip.ParseAddrPort(s); e != nil {
			return nil, e
		}
	}
	a := &app{cfg: c, routes: map[string]*lease{}, cache: map[string]cached{}, epoch: token(), pub: p, stop: make(chan struct{})}
	b, e := os.ReadFile(c.CNFile)
	if e != nil {
		return nil, e
	}
	for _, l := range strings.Split(string(b), "\n") {
		f := strings.Fields(l)
		if len(f) == 0 || strings.HasPrefix(f[0], "#") {
			continue
		}
		n, e := netip.ParsePrefix(f[0])
		if e != nil {
			return nil, fmt.Errorf("invalid CN prefix %q", f[0])
		}
		a.cn = append(a.cn, n)
	}
	if len(a.cn) == 0 {
		return nil, errors.New("empty CN set")
	}
	b, e = os.ReadFile(c.StateFile)
	if e == nil {
		var saved []lease
		if e = json.Unmarshal(b, &saved); e != nil {
			return nil, e
		}
		for _, r := range saved {
			n, e := netip.ParsePrefix(r.Prefix)
			if e != nil || n.Bits() != 32 || !a.eligible(n.Addr()) {
				continue
			}
			if len(a.routes) >= c.MaxRoutes {
				return nil, errors.New("saved routes exceed limit")
			}
			r.AckAt = time.Time{}
			id, e := p.Add(r.Prefix)
			if e != nil {
				return nil, e
			}
			r.UUID = id
			if r.References == nil {
				r.References = map[string]time.Time{}
			}
			a.routes[r.Prefix] = &r
		}
	} else if !os.IsNotExist(e) {
		return nil, e
	}
	return a, nil
}

var excluded = []netip.Prefix{netip.MustParsePrefix("100.64.0.0/10"), netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("192.0.2.0/24"), netip.MustParsePrefix("198.18.0.0/15"), netip.MustParsePrefix("198.51.100.0/24"), netip.MustParsePrefix("203.0.113.0/24"), netip.MustParsePrefix("240.0.0.0/4")}

func (a *app) eligible(ip netip.Addr) bool {
	if !ip.Is4() || !ip.IsGlobalUnicast() || ip.IsPrivate() {
		return false
	}
	// Resolver control addresses have preinstalled bootstrap routes, not BGP
	// advertisements. Never misreport them as dynamically confirmed routes.
	for _, upstream := range a.cfg.Upstreams {
		ap, e := netip.ParseAddrPort(upstream)
		if e == nil && ap.Addr() == ip {
			return false
		}
	}
	for _, p := range excluded {
		if p.Contains(ip) {
			return false
		}
	}
	for _, p := range a.cn {
		if p.Contains(ip) {
			return false
		}
	}
	return true
}
func (a *app) peer(up bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if up != a.up {
		a.up = up
		a.epoch = token()
		for _, r := range a.routes {
			r.AckAt = time.Time{}
		}
		log.Printf("peer established=%v epoch=%s", up, a.epoch)
	}
}
func (a *app) ensure(ctx context.Context, name string, ips []netip.Addr, ttl uint32, advertised bool) error {
	if len(ips) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(a.cfg.ConfirmMS)*time.Millisecond)
	defer cancel()
	a.workOnce.Do(func() { a.work = make(chan struct{}, 1) })
	select {
	case a.work <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	held := true
	defer func() {
		if held {
			<-a.work
		}
	}()
	a.mu.Lock()
	up := a.up
	a.mu.Unlock()
	if !up {
		return errors.New("BGP peer not established")
	}
	keys := make([]string, 0, len(ips))
	for _, ip := range ips {
		if err := ctx.Err(); err != nil {
			return err
		}
		key := netip.PrefixFrom(ip, 32).String()
		a.mu.Lock()
		r := a.routes[key]
		full := len(a.routes) >= a.cfg.MaxRoutes
		a.mu.Unlock()
		if r == nil {
			if full {
				return errors.New("route capacity reached")
			}
			var id []byte
			var err error
			if p, ok := a.pub.(interface {
				AddContext(context.Context, string) ([]byte, error)
			}); ok {
				id, err = p.AddContext(ctx, key)
			} else {
				id, err = a.pub.Add(key)
			}
			if err != nil {
				return err
			}
			r = &lease{Prefix: key, References: map[string]time.Time{}, UUID: id}
			a.mu.Lock()
			a.routes[key] = r
			a.mu.Unlock()
			a.adds.Add(1)
			log.Printf("publish accepted locally prefix=%s domain=%s", key, name)
		}
		now := time.Now()
		a.mu.Lock()
		r.LastSeen = now
		r.RetainUntil = now.Add(time.Duration(a.cfg.RetainSeconds) * time.Second)
		for domain, until := range r.References {
			if now.After(until) {
				delete(r.References, domain)
			}
		}
		if _, exists := r.References[name]; !exists && len(r.References) >= 256 {
			a.mu.Unlock()
			return errors.New("reference capacity reached")
		}
		r.References[name] = now.Add(time.Duration(ttl) * time.Second)
		a.dirty = true
		if advertised {
			until := now.Add(time.Duration(ttl) * time.Second)
			if until.After(r.AnswerUntil) {
				r.AnswerUntil = until
			}
		}
		a.mu.Unlock()
		keys = append(keys, key)
	}
	<-a.work
	held = false
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		a.mu.Lock()
		ready := a.up
		for _, k := range keys {
			r := a.routes[k]
			if r == nil || r.AckAt.IsZero() || time.Since(r.AckAt) > time.Duration(a.cfg.AckMaxAgeMS)*time.Millisecond {
				ready = false
			}
		}
		a.mu.Unlock()
		if ready {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("ROS route confirmation: %w", ctx.Err())
		case <-tick.C:
		}
	}
}
func (a *app) acknowledge(epoch string, prefixes []string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.up || epoch != a.epoch {
		return errors.New("stale BGP epoch")
	}
	for _, r := range a.routes {
		r.AckAt = time.Time{}
	}
	now := time.Now()
	for _, p := range prefixes {
		if r := a.routes[p]; r != nil {
			r.AckAt = now
		}
	}
	return nil
}
func (a *app) save() error {
	a.persistMu.Lock()
	defer a.persistMu.Unlock()
	a.mu.Lock()
	a.dirty = false
	rows := make([]lease, 0, len(a.routes))
	for _, r := range a.routes {
		v := *r
		v.References = make(map[string]time.Time, len(r.References))
		for k, t := range r.References {
			v.References[k] = t
		}
		rows = append(rows, v)
	}
	a.mu.Unlock()
	b, e := json.Marshal(rows)
	if e != nil {
		return e
	}
	if e = os.MkdirAll(filepath.Dir(a.cfg.StateFile), 0700); e != nil {
		return e
	}
	tmp := a.cfg.StateFile + ".tmp"
	if e = os.WriteFile(tmp, b, 0600); e != nil {
		return e
	}
	return os.Rename(tmp, a.cfg.StateFile)
}
func (a *app) gc(now time.Time) {
	a.mu.Lock()
	var candidates []string
	for k, r := range a.routes {
		live := false
		for d, until := range r.References {
			if until.After(now) {
				live = true
			} else {
				delete(r.References, d)
			}
		}
		if !live && !now.Before(r.RetainUntil) && !now.Before(r.AnswerUntil.Add(5*time.Minute)) {
			candidates = append(candidates, k)
		}
	}
	a.mu.Unlock()
	a.workOnce.Do(func() { a.work = make(chan struct{}, 1) })
	for _, k := range candidates {
		a.candidates.Add(1)
		if a.cfg.GCDryRun {
			log.Printf("GC candidate (dry-run) prefix=%s", k)
			continue
		}
		select {
		case a.work <- struct{}{}:
		default:
			return
		}
		a.mu.Lock()
		r := a.routes[k]
		eligible := r != nil
		if r != nil {
			if now.Before(r.RetainUntil) || now.Before(r.AnswerUntil.Add(5*time.Minute)) {
				eligible = false
			}
			for _, until := range r.References {
				if until.After(now) {
					eligible = false
				}
			}
		}
		a.mu.Unlock()
		if !eligible {
			<-a.work
			continue
		}
		err := a.pub.Delete(r.UUID)
		if err != nil {
			log.Printf("withdraw failed prefix=%s error=%v", k, err)
			<-a.work
			continue
		}
		a.mu.Lock()
		delete(a.routes, k)
		a.dirty = true
		a.epoch = token()
		for _, remaining := range a.routes {
			remaining.AckAt = time.Time{}
		}
		a.mu.Unlock()
		<-a.work
		log.Printf("withdraw prefix=%s", k)
	}
}
func clamp(m *dns.Msg, max uint32) uint32 {
	ttl := max
	found := false
	for _, rrs := range [][]dns.RR{m.Answer, m.Ns, m.Extra} {
		for _, rr := range rrs {
			if rr.Header().Rrtype == dns.TypeOPT {
				continue
			}
			if rr.Header().Ttl > max {
				rr.Header().Ttl = max
			}
			if rr.Header().Ttl < ttl {
				ttl = rr.Header().Ttl
			}
			found = true
		}
	}
	if !found {
		return 0
	}
	return ttl
}
func (a *app) lookup(ctx context.Context, q *dns.Msg, refresh bool) (*dns.Msg, error) {
	do := q.IsEdns0() != nil && q.IsEdns0().Do()
	key := fmt.Sprintf("%s/%d/%d/%v/%v", strings.ToLower(q.Question[0].Name), q.Question[0].Qtype, q.Question[0].Qclass, do, q.CheckingDisabled)
	now := time.Now()
	if !refresh {
		a.mu.Lock()
		c, ok := a.cache[key]
		a.mu.Unlock()
		if ok && now.Before(c.expires) {
			a.hits.Add(1)
			m := c.msg.Copy()
			age := uint32(time.Since(c.stored) / time.Second)
			for _, rrs := range [][]dns.RR{m.Answer, m.Ns, m.Extra} {
				for _, rr := range rrs {
					if rr.Header().Rrtype != dns.TypeOPT {
						t := rr.Header().Ttl
						if t > age {
							rr.Header().Ttl = t - age
						} else {
							rr.Header().Ttl = 0
						}
					}
				}
			}
			m.Id = q.Id
			return m, nil
		}
	}
	v, e, _ := a.flight.Do(key, func() (any, error) {
		type result struct {
			m *dns.Msg
			e error
		}
		ch := make(chan result, len(a.cfg.Upstreams))
		child, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		for _, addr := range a.cfg.Upstreams {
			go func(addr string) {
				c := &dns.Client{Net: "udp", Timeout: 2 * time.Second}
				m, _, e := c.ExchangeContext(child, q.Copy(), addr)
				if e == nil && m.Truncated {
					c.Net = "tcp"
					m, _, e = c.ExchangeContext(child, q.Copy(), addr)
				}
				if e == nil && (m.Rcode != dns.RcodeSuccess && m.Rcode != dns.RcodeNameError) {
					e = fmt.Errorf("DNS rcode %d", m.Rcode)
				}
				ch <- result{m, e}
			}(addr)
		}
		var last error
		for range a.cfg.Upstreams {
			select {
			case <-child.Done():
				return nil, child.Err()
			case r := <-ch:
				if r.e != nil {
					last = r.e
					continue
				}
				ttl := clamp(r.m, a.cfg.TTLMax)
				if ttl > 0 && r.m.Rcode == dns.RcodeSuccess {
					t := time.Now()
					a.mu.Lock()
					if len(a.cache) >= a.cfg.MaxRoutes*4 {
						for k, c := range a.cache {
							if t.After(c.expires) {
								delete(a.cache, k)
							}
						}
					}
					if len(a.cache) < a.cfg.MaxRoutes*4 {
						a.cache[key] = cached{r.m.Copy(), t, t.Add(time.Duration(ttl) * time.Second)}
					}
					a.mu.Unlock()
				}
				return r.m, nil
			}
		}
		return nil, last
	})
	if e != nil {
		return nil, e
	}
	m := v.(*dns.Msg).Copy()
	m.Id = q.Id
	return m, nil
}
func (a *app) resolve(ctx context.Context, q *dns.Msg, refresh, advertised bool) (*dns.Msg, error) {
	m, e := a.lookup(ctx, q, refresh)
	if e != nil {
		return nil, e
	}
	var ips []netip.Addr
	seen := map[netip.Addr]bool{}
	for _, rr := range m.Answer {
		if v, ok := rr.(*dns.A); ok {
			ip, ok := netip.AddrFromSlice(v.A)
			ip = ip.Unmap()
			if ok && a.eligible(ip) && !seen[ip] {
				seen[ip] = true
				ips = append(ips, ip)
			}
		}
	}
	ttl := clamp(m, a.cfg.TTLMax)
	confirmedSince := time.Now()
	if e = a.ensure(ctx, strings.ToLower(q.Question[0].Name), ips, ttl, advertised); e != nil {
		return nil, e
	}

	age := uint32(time.Since(confirmedSince) / time.Second)
	if ttl > 0 && age >= ttl {
		return nil, errors.New("DNS answer expired while waiting for route")
	}
	if age > 0 {
		for _, rrs := range [][]dns.RR{m.Answer, m.Ns, m.Extra} {
			for _, rr := range rrs {
				if rr.Header().Rrtype != dns.TypeOPT {
					if rr.Header().Ttl > age {
						rr.Header().Ttl -= age
					} else {
						rr.Header().Ttl = 0
					}
				}
			}
		}
	}
	return m, nil
}
func (a *app) ServeDNS(w dns.ResponseWriter, q *dns.Msg) {
	a.requests.Add(1)
	a.dnsOnce.Do(func() { a.slots = make(chan struct{}, 32) })
	select {
	case a.slots <- struct{}{}:
		defer func() { <-a.slots }()
	default:
		m := new(dns.Msg)
		m.SetRcode(q, dns.RcodeServerFailure)
		w.WriteMsg(m)
		return
	}
	if len(q.Question) != 1 || q.Opcode != dns.OpcodeQuery || q.Question[0].Qclass != dns.ClassINET {
		m := new(dns.Msg)
		m.SetRcode(q, dns.RcodeRefused)
		w.WriteMsg(m)
		return
	}
	if q.Question[0].Qtype == dns.TypeAAAA {
		m := new(dns.Msg)
		m.SetReply(q)
		w.WriteMsg(m)
		return
	}
	ctx, c := context.WithTimeout(context.Background(), 6*time.Second)
	defer c()
	m, e := a.resolve(ctx, q, false, true)
	if e != nil {
		a.failures.Add(1)
		log.Printf("DNS blocked domain=%s error=%v", q.Question[0].Name, e)
		m = new(dns.Msg)
		m.SetRcode(q, dns.RcodeServerFailure)
	}
	w.WriteMsg(m)
}
func (a *app) httpHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/state", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			http.Error(w, "method", 405)
			return
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"version": version, "peer_up": a.up, "epoch": a.epoch, "routes": a.routes, "requests": a.requests.Load(), "cache_hits": a.hits.Load(), "failures": a.failures.Load(), "publications": a.adds.Load(), "gc_dry_run": a.cfg.GCDryRun, "gc_candidates": a.candidates.Load()})
	})
	mux.HandleFunc("/desired", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			http.Error(w, "method", 405)
			return
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		p := make([]string, 0, len(a.routes))
		for k := range a.routes {
			p = append(p, k)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"epoch": a.epoch, "prefixes": p})
	})
	mux.HandleFunc("/ack", func(w http.ResponseWriter, r *http.Request) {
		host, _, _ := net.SplitHostPort(r.RemoteAddr)
		if r.Method != "POST" || host != a.cfg.PeerIP {
			http.Error(w, "forbidden", 403)
			return
		}
		var b struct {
			Epoch    string   `json:"epoch"`
			Prefixes []string `json:"prefixes"`
		}
		if e := json.NewDecoder(io.LimitReader(r.Body, 1024*1024)).Decode(&b); e != nil {
			http.Error(w, "bad JSON", 400)
			return
		}
		if e := a.acknowledge(b.Epoch, b.Prefixes); e != nil {
			http.Error(w, e.Error(), 409)
			return
		}
		w.WriteHeader(204)
	})
	return mux
}
func (a *app) background() {
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		tick := time.NewTicker(30 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-a.stop:
				return
			case <-tick.C:
				a.mu.Lock()
				dirty := a.dirty
				a.mu.Unlock()
				if dirty {
					if e := a.save(); e != nil {
						a.mu.Lock()
						a.dirty = true
						a.mu.Unlock()
						log.Printf("persist failed: %v", e)
					}
				}
			}
		}
	}()
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		tick := time.NewTicker(time.Minute)
		defer tick.Stop()
		for {
			select {
			case <-a.stop:
				return
			case t := <-tick.C:
				a.gc(t)
			}
		}
	}()
	for _, name := range a.cfg.Warm {
		a.wg.Add(1)
		go func(name string) {
			defer a.wg.Done()
			delay := time.Duration(0)
			for {
				timer := time.NewTimer(delay)
				select {
				case <-a.stop:
					timer.Stop()
					return
				case <-timer.C:
				}
				q := new(dns.Msg)
				q.SetQuestion(dns.Fqdn(name), dns.TypeA)
				ctx, c := context.WithTimeout(context.Background(), 6*time.Second)
				m, e := a.resolve(ctx, q, true, false)
				c()
				if e != nil {
					log.Printf("prewarm %s: %v", name, e)
					delay = 15 * time.Second
					continue
				}
				ttl := clamp(m, a.cfg.TTLMax)
				delay = time.Duration(math.Max(1, float64(ttl)*0.8)) * time.Second
				if delay > 240*time.Second {
					delay = 240 * time.Second
				}
				var b [1]byte
				rand.Read(b[:])
				delay = delay * time.Duration(90+int(b[0])%10) / 100
			}
		}(name)
	}
}
func run(path string) error {
	b, e := os.ReadFile(path)
	if e != nil {
		return e
	}
	var c config
	if e = json.Unmarshal(b, &c); e != nil {
		return e
	}
	for _, ip := range []string{c.LocalIP, c.PeerIP, c.NextHop} {
		if _, e := netip.ParseAddr(ip); e != nil {
			return e
		}
	}
	s := server.NewBgpServer()
	go s.Serve()
	if e = s.StartBgp(context.Background(), &api.StartBgpRequest{Global: &api.Global{Asn: c.ASN, RouterId: c.LocalIP, ListenPort: 179, ListenAddresses: []string{c.LocalIP}}}); e != nil {
		return e
	}
	if e = s.AddPeer(context.Background(), &api.AddPeerRequest{Peer: &api.Peer{Conf: &api.PeerConf{NeighborAddress: c.PeerIP, PeerAsn: c.PeerASN}, Transport: &api.Transport{LocalAddress: c.LocalIP}, Timers: &api.Timers{Config: &api.TimersConfig{HoldTime: 30, KeepaliveInterval: 10, ConnectRetry: 5}}}}); e != nil {
		return e
	}
	a, e := newApp(c, bgpPublisher{s, c.NextHop})
	if e != nil {
		return e
	}
	a.background()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if e = s.WatchEvent(ctx, &api.WatchEventRequest{Peer: &api.WatchEventRequest_Peer{}}, func(ev *api.WatchEventResponse) {
		p := ev.GetPeer()
		if p != nil && p.Type == api.WatchEventResponse_PeerEvent_STATE && p.Peer != nil && p.Peer.State != nil && p.Peer.Conf.NeighborAddress == c.PeerIP {
			a.peer(p.Peer.State.SessionState == api.PeerState_ESTABLISHED)
		}
	}); e != nil {
		return e
	}
	go func() {
		tick := time.NewTicker(200 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				up := false
				s.ListPeer(ctx, &api.ListPeerRequest{Address: c.PeerIP}, func(p *api.Peer) { up = p.State != nil && p.State.SessionState == api.PeerState_ESTABLISHED })
				a.peer(up)
			}
		}
	}()
	hs := &http.Server{Addr: c.HTTPListen, Handler: a.httpHandler(), ReadHeaderTimeout: 2 * time.Second, ReadTimeout: 3 * time.Second, WriteTimeout: 3 * time.Second}
	udp := &dns.Server{Addr: c.DNSListen, Net: "udp", Handler: a}
	tcp := &dns.Server{Addr: c.DNSListen, Net: "tcp", Handler: a}
	errs := make(chan error, 3)
	go func() { errs <- hs.ListenAndServe() }()
	go func() { errs <- udp.ListenAndServe() }()
	go func() { errs <- tcp.ListenAndServe() }()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	log.Printf("%s DNS=%s peer=%s; require ROS acknowledgement; GC dry-run=%v", version, c.DNSListen, c.PeerIP, c.GCDryRun)
	select {
	case <-sig:
	case e = <-errs:
	}
	close(a.stop)
	a.wg.Wait()
	a.save()
	cancel()
	udp.Shutdown()
	tcp.Shutdown()
	stop, done := context.WithTimeout(context.Background(), 3*time.Second)
	defer done()
	hs.Shutdown(stop)
	s.StopBgp(stop, &api.StopBgpRequest{})
	return e
}
func main() {
	if len(os.Args) > 1 && os.Args[1] == "probe" {
		probe(os.Args[2:])
		return
	}
	f := flag.String("config", "/etc/bgp-agent/config.json", "config path")
	flag.Parse()
	if e := run(*f); e != nil {
		log.Fatal(e)
	}
}
