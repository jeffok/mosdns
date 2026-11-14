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
	"strconv"
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

const version = "bgp-unified-20261114.1"

// Only isolated test builds may opt into the unfinished guarded writer.
var guardTestBuild string

func guardedTestAllowed(c config) bool {
	return guardTestBuild == "1" && c.GuardTestTable == "rt-bgp-test" && c.ConfirmClientIP != ""
}

type config struct {
	LocalIP         string   `json:"local_ip"`
	PeerIP          string   `json:"peer_ip"`
	ASN             uint32   `json:"asn"`
	PeerASN         uint32   `json:"peer_asn"`
	NextHop         string   `json:"next_hop"`
	DNSListen       string   `json:"dns_listen"`
	HTTPListen      string   `json:"http_listen"`
	ConfirmClientIP string   `json:"confirm_client_ip"` // empty disables /confirm
	GuardGCEnabled  bool     `json:"guard_gc_enabled"`  // enabled only in isolated test builds
	GuardTestTable  string   `json:"guard_test_table"`
	Upstreams       []string `json:"upstreams"`
	Warm            []string `json:"warm_domains"`
	CNFile          string   `json:"cn_file"`
	StateFile       string   `json:"state_file"`
	TTLMax          uint32   `json:"ttl_max"`
	RetainSeconds   int      `json:"retain_seconds"`
	ConfirmMS       int      `json:"confirm_ms"`
	AckMaxAgeMS     int      `json:"ack_max_age_ms"`
	MaxRoutes       int      `json:"max_routes"`
	GCDryRun        bool     `json:"gc_dry_run"`
}
type lease struct {
	Prefix      string               `json:"prefix"`
	LastSeen    time.Time            `json:"last_seen"`
	RetainUntil time.Time            `json:"retain_until"`
	AnswerUntil time.Time            `json:"answer_until"`
	References  map[string]time.Time `json:"references"`
	AckAt       time.Time            `json:"ack_at,omitempty"`
	UUID        []byte               `json:"-"`
	generation  uint64
}
type cached struct {
	msg             *dns.Msg
	stored, expires time.Time
}
type routeSnapshot struct {
	epoch            string
	issued           time.Time // retains Go's monotonic clock reading
	sequence         uint64
	prefixes         map[string]uint64
	guardGenerations map[string]string
	guardBulk        bool
	healthTotalPages int
	healthNextPage   int
	healthAccepted   []string
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
	durableUntil                               map[string]time.Time
	durableGeneration                          map[string]uint64
	withdrawnGuardOps                          map[string]string // locally confirmed deletions awaiting durable state
	cache                                      map[string]cached
	epoch                                      string
	snapshots                                  map[string]routeSnapshot
	snapshotSequence, lastAppliedSnapshot      uint64
	routeGeneration                            uint64
	up                                         bool
	pub                                        publication
	guard                                      *guardCoordinator // prototype only; startup still rejects guard mode
	recovery                                   *recoveryHTTP
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
	// Never run the legacy answer path with experimental cleanup requested: the
	// coordinator is not yet connected to the DNS send pin or trusted ROS writer.
	if c.GuardGCEnabled && !guardedTestAllowed(c) {
		return nil, errors.New("guard_gc_enabled is unavailable: isolated guard test build and table required")
	}
	if c.TTLMax == 0 || c.TTLMax > 300 || c.RetainSeconds < 300 || c.ConfirmMS < 1 || c.ConfirmMS > 30000 || c.AckMaxAgeMS < 1 || c.MaxRoutes < 1 {
		return nil, errors.New("invalid TTL/retention/confirmation/capacity configuration")
	}
	if len(c.Warm) > 32 || len(c.Upstreams) > 3 {
		return nil, errors.New("too many warm domains or upstreams")
	}
	if len(c.Upstreams) == 0 {
		return nil, errors.New("upstreams required")
	}
	if c.ConfirmClientIP != "" {
		ip, err := netip.ParseAddr(c.ConfirmClientIP)
		if err != nil || !ip.Is4() || ip.IsUnspecified() {
			return nil, errors.New("invalid confirm_client_ip")
		}
	}
	for _, s := range c.Upstreams {
		if _, e := netip.ParseAddrPort(s); e != nil {
			return nil, e
		}
	}
	a := &app{cfg: c, routes: map[string]*lease{}, durableUntil: map[string]time.Time{}, durableGeneration: map[string]uint64{}, cache: map[string]cached{}, snapshots: map[string]routeSnapshot{}, epoch: token(), pub: p, stop: make(chan struct{})}
	if c.GuardGCEnabled {
		peer, err := netip.ParseAddr(c.PeerIP)
		if err != nil || !peer.Is4() || peer.IsUnspecified() {
			return nil, errors.New("invalid guarded test peer")
		}
		guard, err := newGuardCoordinator(c.StateFile+".guard", c.MaxRoutes, time.Duration(c.TTLMax)*time.Second, 2*time.Second, time.Second, 2*time.Second, time.Duration(c.RetainSeconds)*time.Second)
		if err != nil {
			return nil, fmt.Errorf("guard journal: %w", err)
		}
		a.guard = guard
		a.recovery = &recoveryHTTP{c: guard, peer: peer}
	}
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
			if e := a.loadSavedLease(r, !c.GuardGCEnabled); e != nil {
				return nil, e
			}
		}
	} else if !os.IsNotExist(e) {
		return nil, e
	}
	return a, nil
}

// loadSavedLease in guarded mode never publishes on its own. Replay is permitted
// only through replayGuardedPrefixes after recovery has verified the guard.
func (a *app) loadSavedLease(r lease, publish bool) error {
	r.AckAt = time.Time{}
	r.UUID = nil
	if publish {
		id, err := a.pub.Add(r.Prefix)
		if err != nil {
			return err
		}
		r.UUID = id
	}
	a.routeGeneration++
	r.generation = a.routeGeneration
	if r.References == nil {
		r.References = map[string]time.Time{}
	}
	a.routes[r.Prefix] = &r
	a.durableUntil[r.Prefix] = r.AnswerUntil
	a.durableGeneration[r.Prefix] = r.generation
	return nil
}

// The HTTP write deadline starts after reading request headers and covers the
// remainder of the request and handler. Allow a bounded body-read window and
// the configured route-confirmation budget, rather than cutting a successful
// confirmation short at the previous fixed three seconds.
func newHTTPServer(c config, handler http.Handler) *http.Server {
	writeTimeout := time.Duration(c.ConfirmMS)*time.Millisecond + 4*time.Second
	if c.GuardGCEnabled && guardedTestAllowed(c) {
		writeTimeout = 65 * time.Second
	}
	return &http.Server{Addr: c.HTTPListen, Handler: handler, ReadHeaderTimeout: 2 * time.Second,
		ReadTimeout: 3 * time.Second, WriteTimeout: writeTimeout}
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
		clear(a.snapshots)
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
	keys := make([]string, 0, len(ips))
	for _, ip := range ips {
		keys = append(keys, netip.PrefixFrom(ip, 32).String())
	}
	if a.guard != nil {
		tick := time.NewTicker(10 * time.Millisecond)
		defer tick.Stop()
		for !a.guard.guardsReady(keys) {
			select {
			case <-ctx.Done():
				return fmt.Errorf("ROS guard confirmation: %w", ctx.Err())
			case <-tick.C:
			}
		}
	}
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
	// Check the whole answer while publication/GC work is serialized. A large
	// answer must not consume the last slots and then fail partway through.
	a.mu.Lock()
	newPrefixes := make(map[string]struct{}, len(ips))
	for _, ip := range ips {
		key := netip.PrefixFrom(ip, 32).String()
		r := a.routes[key]
		if r == nil {
			newPrefixes[key] = struct{}{}
			continue
		}
		if _, exists := r.References[name]; !exists {
			live := 0
			for _, until := range r.References {
				if time.Now().Before(until) {
					live++
				}
			}
			if live >= 256 {
				a.mu.Unlock()
				return errors.New("reference capacity reached")
			}
		}
	}
	if len(a.routes)+len(newPrefixes) > a.cfg.MaxRoutes {
		a.mu.Unlock()
		return errors.New("route capacity reached")
	}
	a.mu.Unlock()
	if a.guard != nil && !a.guard.guardsReady(keys) {
		return errors.New("ROS guard changed before publication")
	}
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
			a.mu.Lock()
			if a.routeGeneration == ^uint64(0) {
				a.mu.Unlock()
				return errors.New("route generation exhausted")
			}
			a.routeGeneration++
			generation := a.routeGeneration
			a.mu.Unlock()
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
			r = &lease{Prefix: key, References: map[string]time.Time{}, UUID: id, generation: generation}
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
			// Retain a little longer than the advertised TTL so repeated cache
			// hits share a durable lease and do not fsync on every DNS request.
			until := now.Add(time.Duration(ttl) * time.Second).Truncate(time.Minute).Add(time.Minute)
			if until.After(r.AnswerUntil) {
				r.AnswerUntil = until
			}
		}
		a.mu.Unlock()
	}
	if a.guard != nil {
		if err := a.guard.requestBGPConfirmation(keys); err != nil {
			return err
		}
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
		if a.guard != nil && !a.guard.allActive(keys) {
			ready = false
		}
		if ready && advertised {
			// Every positive response must be recoverable after an ungraceful
			// process exit. A previous persisted lease can cover repeated hits.
			a.mu.Lock()
			needsSave := false
			for _, k := range keys {
				if r := a.routes[k]; r == nil || r.generation != a.durableGeneration[k] || r.AnswerUntil.After(a.durableUntil[k]) {
					needsSave = true
				}
			}
			a.mu.Unlock()
			if needsSave {
				if err := a.save(); err != nil {
					return fmt.Errorf("persist answered routes: %w", err)
				}
			}
			// The peer or active-route confirmation can disappear during I/O.
			a.mu.Lock()
			ready = a.up
			for _, k := range keys {
				r := a.routes[k]
				if r == nil || r.AckAt.IsZero() || time.Since(r.AckAt) > time.Duration(a.cfg.AckMaxAgeMS)*time.Millisecond || r.generation != a.durableGeneration[k] || r.AnswerUntil.After(a.durableUntil[k]) {
					ready = false
				}
			}
			a.mu.Unlock()
			if a.guard != nil && !a.guard.allActive(keys) {
				ready = false
			}
		}
		if ready {
			if err := ctx.Err(); err != nil {
				return err
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("ROS route confirmation: %w", ctx.Err())
		case <-tick.C:
		}
	}
}

const maxPendingSnapshots = 32

// issueSnapshot records issuance before handing the route list to RouterOS.
// Its monotonic timestamp bounds the age of the resulting confirmation.
func (a *app) issueSnapshot() (string, string, []string, error) {
	id := token()
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	maxAge := time.Duration(a.cfg.AckMaxAgeMS) * time.Millisecond
	for key, snap := range a.snapshots {
		if now.Sub(snap.issued) >= maxAge {
			delete(a.snapshots, key)
		}
	}
	if len(a.snapshots) >= maxPendingSnapshots {
		return "", "", nil, errors.New("too many pending route snapshots")
	}
	prefixes := make([]string, 0, len(a.routes))
	wanted := make(map[string]uint64, len(a.routes))
	for prefix, route := range a.routes {
		prefixes = append(prefixes, prefix)
		wanted[prefix] = route.generation
	}
	a.snapshotSequence++
	a.snapshots[id] = routeSnapshot{epoch: a.epoch, issued: now, sequence: a.snapshotSequence, prefixes: wanted}
	return a.epoch, id, prefixes, nil
}

func (a *app) acknowledge(epoch, snapshotID string, prefixes []string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.acknowledgeLocked(epoch, snapshotID, prefixes)
}
func (a *app) acknowledgeLocked(epoch, snapshotID string, prefixes []string) error {
	if !a.up || epoch != a.epoch {
		return errors.New("stale BGP epoch")
	}
	snap, ok := a.snapshots[snapshotID]
	if !ok || snap.epoch != epoch || snap.sequence <= a.lastAppliedSnapshot {
		return errors.New("unknown, replayed or superseded snapshot")
	}
	if time.Since(snap.issued) >= time.Duration(a.cfg.AckMaxAgeMS)*time.Millisecond {
		delete(a.snapshots, snapshotID)
		return errors.New("expired snapshot")
	}
	if len(prefixes) > len(snap.prefixes) {
		return errors.New("invalid snapshot prefix count")
	}
	for _, p := range prefixes {
		generation, ok := snap.prefixes[p]
		if !ok {
			return errors.New("prefix absent from snapshot")
		}
		r := a.routes[p]
		if r == nil || r.generation != generation {
			return errors.New("route generation changed since snapshot")
		}
	}
	delete(a.snapshots, snapshotID)
	a.lastAppliedSnapshot = snap.sequence
	for prefix, r := range a.routes {
		// A route created after this snapshot was never observed by it.
		// Do not erase a newer generation's independently acquired ACK.
		if gen, ok := snap.prefixes[prefix]; ok && gen == r.generation {
			r.AckAt = time.Time{}
		}
	}
	for _, p := range prefixes {
		if r := a.routes[p]; r != nil {
			r.AckAt = snap.issued
		}
	}
	return nil
}
func (a *app) save() (err error) {
	a.persistMu.Lock()
	defer a.persistMu.Unlock()
	defer func() {
		if err != nil {
			a.mu.Lock()
			a.dirty = true
			a.mu.Unlock()
		}
	}()
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
	f, e := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if e != nil {
		return e
	}
	if _, e = f.Write(b); e == nil {
		e = f.Sync()
	}
	if closeErr := f.Close(); e == nil {
		e = closeErr
	}
	if e != nil {
		return e
	}
	if e = os.Rename(tmp, a.cfg.StateFile); e != nil {
		return e
	}
	dir, e := os.Open(filepath.Dir(a.cfg.StateFile))
	if e != nil {
		return e
	}
	e = dir.Sync()
	if closeErr := dir.Close(); e == nil {
		e = closeErr
	}
	if e != nil {
		return e
	}
	a.mu.Lock()
	a.durableUntil = make(map[string]time.Time, len(rows))
	a.durableGeneration = make(map[string]uint64, len(rows))
	for _, row := range rows {
		a.durableUntil[row.Prefix] = row.AnswerUntil
		a.durableGeneration[row.Prefix] = row.generation
	}
	a.mu.Unlock()
	return nil
}
func (a *app) gc(now time.Time) {
	if a.guard != nil {
		a.guardGC(now)
		return
	}
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
		a.mu.Unlock()
		<-a.work
		log.Printf("withdraw prefix=%s", k)
	}
}

// dispatchGuardWithdrawal is deliberately not scheduled by run yet. The
// protected retirement loop and restart reconciliation must be integrated
// before this method can run in production.
func (a *app) dispatchGuardWithdrawal() error {
	if a.guard == nil {
		return errors.New("guard unavailable")
	}
	return a.guard.dispatchWithdrawal(func(op guardOperation) error {
		a.workOnce.Do(func() { a.work = make(chan struct{}, 1) })
		a.work <- struct{}{}
		defer func() { <-a.work }()
		a.mu.Lock()
		r := a.routes[op.Prefix]
		deletedByThisOp := a.withdrawnGuardOps[op.Prefix] == op.ID
		a.mu.Unlock()
		if r == nil {
			if !deletedByThisOp {
				return errors.New("withdrawal route unavailable")
			}
			// The previous attempt deleted the path but failed to save. Retry
			// persistence, never infer deletion from an arbitrary missing lease.
			return a.save()
		}
		if err := a.pub.Delete(r.UUID); err != nil {
			return err
		}
		a.mu.Lock()
		if a.withdrawnGuardOps == nil {
			a.withdrawnGuardOps = make(map[string]string)
		}
		a.withdrawnGuardOps[op.Prefix] = op.ID
		delete(a.routes, op.Prefix)
		a.dirty = true
		a.mu.Unlock()
		return a.save()
	})
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
	// EDNS options (for example ECS and cookies) can change the upstream
	// response. Never share a cached response between different OPT records.
	if opt := q.IsEdns0(); opt != nil {
		key += "/" + opt.String()
	}
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

var errAnswerExpired = errors.New("DNS answer expired while waiting for route")

func (a *app) resolve(ctx context.Context, q *dns.Msg, refresh, advertised bool) (*dns.Msg, error) {
	m, err := a.resolveOnce(ctx, q, refresh, advertised)
	if errors.Is(err, errAnswerExpired) && ctx.Err() == nil {
		// The first answer aged out while waiting for its route. Refresh once,
		// within the original request deadline, without increasing its TTL.
		return a.resolveOnce(ctx, q, true, advertised)
	}
	return m, err
}

func (a *app) resolveOnce(ctx context.Context, q *dns.Msg, refresh, advertised bool) (*dns.Msg, error) {
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
		return nil, errAnswerExpired
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

// writeDNSResponse prepares an independent response for the current client;
// the cached full message is never truncated or otherwise modified in place.
func writeDNSResponse(w dns.ResponseWriter, q, response *dns.Msg) error {
	m := response.Copy()
	if q.IsEdns0() == nil {
		extra := m.Extra[:0]
		for _, rr := range m.Extra {
			if rr.Header().Rrtype != dns.TypeOPT {
				extra = append(extra, rr)
			}
		}
		m.Extra = extra
	}
	if _, udp := w.RemoteAddr().(*net.UDPAddr); udp {
		size := dns.MinMsgSize
		if opt := q.IsEdns0(); opt != nil && int(opt.UDPSize()) > size {
			size = int(opt.UDPSize())
		}
		// Cap the UDP payload to avoid routine IP fragmentation. Clients can
		// obtain the full message over TCP when the TC bit is set.
		if size > 1232 {
			size = 1232
		}
		m.Truncate(size)
		// An unusually large upstream OPT may itself exceed the budget. Keep
		// the EDNS header but remove optional data before retrying truncation.
		if m.Len() > size {
			if opt := m.IsEdns0(); opt != nil {
				opt.Option = nil
			}
			m.Truncated = true
			m.Truncate(size)
		}
	}
	return w.WriteMsg(m)
}

func (a *app) ServeDNS(w dns.ResponseWriter, q *dns.Msg) {
	// The standalone DNS server has no post-socket-write pin hook yet.
	if a.guard != nil {
		m := new(dns.Msg)
		m.SetRcode(q, dns.RcodeServerFailure)
		_ = writeDNSResponse(w, q, m)
		return
	}
	a.requests.Add(1)
	a.dnsOnce.Do(func() { a.slots = make(chan struct{}, 32) })
	select {
	case a.slots <- struct{}{}:
		defer func() { <-a.slots }()
	default:
		m := new(dns.Msg)
		m.SetRcode(q, dns.RcodeServerFailure)
		_ = writeDNSResponse(w, q, m)
		return
	}
	if len(q.Question) != 1 || q.Opcode != dns.OpcodeQuery || q.Question[0].Qclass != dns.ClassINET {
		m := new(dns.Msg)
		m.SetRcode(q, dns.RcodeRefused)
		_ = writeDNSResponse(w, q, m)
		return
	}
	if q.Question[0].Qtype == dns.TypeAAAA {
		m := new(dns.Msg)
		m.SetReply(q)
		_ = writeDNSResponse(w, q, m)
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
	_ = writeDNSResponse(w, q, m)
}

// Report only slow handlers, without URL/query or DNS names.
func guardMeasured(label string, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		if guardTestBuild == "1" {
			log.Printf("guard HTTP %s start=%s", label, started.Format(time.RFC3339Nano))
		}
		h.ServeHTTP(w, r)
		if elapsed := time.Since(started); elapsed >= 100*time.Millisecond || guardTestBuild == "1" {
			log.Printf("guard HTTP %s elapsed=%s", label, elapsed)
		}
	})
}

func (a *app) httpHandler() http.Handler {
	mux := http.NewServeMux()
	if a.guard != nil {
		client, _ := netip.ParseAddr(a.cfg.ConfirmClientIP)
		peer, _ := netip.ParseAddr(a.cfg.PeerIP)
		pin := guardPinHTTP(a.guard, client)
		mux.Handle("/guardpin/may-send", pin)
		mux.Handle("/guardpin/finish-sent", pin)
		mux.Handle("/guardpin/abandon", pin)
		writer := guardWriterHTTP(a.guard, peer)
		mux.Handle("/guardwriter/batch-next", guardMeasured("batch-next", writer))
		mux.Handle("/guardwriter/batch-ack", guardMeasured("batch-ack", writer))
		mux.Handle("/guardwriter/next", guardMeasured("next", writer))
		mux.Handle("/guardwriter/ack", guardMeasured("ack", writer))
		mux.Handle("/guardwriter/health", guardMeasured("health", http.HandlerFunc(a.guardHealthHTTP)))
		mux.Handle("/guardwriter/health-ack", guardMeasured("health-ack", http.HandlerFunc(a.guardHealthHTTP)))
		if a.recovery != nil {
			mux.Handle("/guardrecovery/next", guardMeasured("recovery-next", a.recovery))
			mux.Handle("/guardrecovery/attest", guardMeasured("recovery-attest", a.recovery))
		}
	}
	// Reserved fail-closed interfaces only; no state machine operation is
	// exposed before writer authentication, DNS pinning and recovery exist.
	guardUnavailable := func(method string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if !a.cfg.GuardGCEnabled {
				http.NotFound(w, r)
				return
			}
			host, _, err := net.SplitHostPort(r.RemoteAddr)
			if err != nil || host != a.cfg.PeerIP {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			if r.Method != method {
				http.Error(w, "method", http.StatusMethodNotAllowed)
				return
			}
			http.Error(w, "protected guard writer is unavailable", http.StatusServiceUnavailable)
		}
	}
	mux.HandleFunc("/guardgc/next", guardUnavailable(http.MethodGet))
	mux.HandleFunc("/guardgc/ack", guardUnavailable(http.MethodPost))
	if a.guard == nil {
		mux.HandleFunc("/guardwriter/next", guardUnavailable(http.MethodGet))
		mux.HandleFunc("/guardwriter/ack", guardUnavailable(http.MethodPost))
	}
	mux.HandleFunc("/confirm", func(w http.ResponseWriter, r *http.Request) {
		if a.cfg.ConfirmClientIP == "" {
			http.NotFound(w, r)
			return
		}
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		remote, parseErr := netip.ParseAddr(host)
		allowed, _ := netip.ParseAddr(a.cfg.ConfirmClientIP)
		if err != nil || parseErr != nil || remote.Unmap() != allowed {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "method", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Domain string   `json:"domain"`
			IPs    []string `json:"ips"`
			TTL    *uint32  `json:"ttl"`
		}
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil || dec.Decode(new(any)) != io.EOF {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		_, valid := dns.IsDomainName(req.Domain)
		if !valid || !dns.IsFqdn(req.Domain) || len(req.Domain) > 255 || len(req.IPs) == 0 || len(req.IPs) > 64 || req.TTL == nil || *req.TTL > a.cfg.TTLMax {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		ips := make([]netip.Addr, 0, len(req.IPs))
		seen := make(map[netip.Addr]bool, len(req.IPs))
		zeroNet := netip.MustParsePrefix("0.0.0.0/8")
		for _, raw := range req.IPs {
			ip, err := netip.ParseAddr(raw)
			if err != nil || !ip.Is4() || !ip.IsGlobalUnicast() || ip.IsPrivate() || zeroNet.Contains(ip) {
				http.Error(w, "invalid IPv4 answer", http.StatusBadRequest)
				return
			}
			for _, prefix := range excluded {
				if prefix.Contains(ip) {
					http.Error(w, "invalid IPv4 answer", http.StatusBadRequest)
					return
				}
			}
			// The caller sends only addresses that it needs routed. A domestic
			// or bootstrap address here may indicate rule-set skew; do not report
			// it as confirmed without a dynamically acknowledged route.
			if !a.eligible(ip) {
				http.Error(w, "unconfirmed IPv4 answer", http.StatusServiceUnavailable)
				return
			}
			if !seen[ip] {
				seen[ip] = true
				ips = append(ips, ip)
			}
		}
		a.dnsOnce.Do(func() { a.slots = make(chan struct{}, 32) })
		select {
		case a.slots <- struct{}{}:
			defer func() { <-a.slots }()
		default:
			http.Error(w, "busy", http.StatusServiceUnavailable)
			return
		}
		var prefixes []string
		if a.guard != nil {
			for _, ip := range ips {
				prefixes = append(prefixes, netip.PrefixFrom(ip, 32).String())
			}
			if err := a.guard.prepareForRequest(prefixes); err != nil {
				http.Error(w, "guard preparation failed", http.StatusServiceUnavailable)
				return
			}
			defer a.releaseGuardRequest(prefixes)
		}
		if err := a.ensure(r.Context(), strings.ToLower(req.Domain), ips, *req.TTL, true); err != nil {
			log.Printf("confirmation blocked domain=%s error=%v", req.Domain, err)
			http.Error(w, "route confirmation failed", http.StatusServiceUnavailable)
			return
		}
		if a.guard != nil {
			pin, err := a.guard.reserve(prefixes, time.Duration(*req.TTL)*time.Second)
			if err != nil {
				http.Error(w, "guard pin failed", http.StatusServiceUnavailable)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(struct {
				ID string `json:"pin_id"`
			}{pin.ID})
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/state", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			http.Error(w, "method", 405)
			return
		}
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		remote, parseErr := netip.ParseAddr(host)
		peer, peerErr := netip.ParseAddr(a.cfg.PeerIP)
		if err != nil || parseErr != nil || (!remote.IsLoopback() && (peerErr != nil || remote.Unmap() != peer.Unmap())) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"version": version, "peer_up": a.up, "epoch": a.epoch, "routes": a.routes, "requests": a.requests.Load(), "cache_hits": a.hits.Load(), "failures": a.failures.Load(), "publications": a.adds.Load(), "gc_dry_run": a.cfg.GCDryRun, "gc_candidates": a.candidates.Load()})
	})
	mux.HandleFunc("/desired", func(w http.ResponseWriter, r *http.Request) {
		if a.guard != nil {
			http.Error(w, "legacy confirmation disabled", http.StatusServiceUnavailable)
			return
		}
		if r.Method != "GET" {
			http.Error(w, "method", 405)
			return
		}
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil || (host != a.cfg.PeerIP && host != "127.0.0.1") {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		epoch, snapshotID, p, err := a.issueSnapshot()
		if err != nil {
			http.Error(w, "snapshot capacity exceeded", http.StatusServiceUnavailable)
			return
		}
		body, err := json.Marshal(map[string]any{"epoch": epoch, "snapshot_id": snapshotID, "prefixes": p})
		if err != nil {
			http.Error(w, "snapshot encoding failed", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		_, _ = w.Write(body)
	})
	mux.HandleFunc("/ack", func(w http.ResponseWriter, r *http.Request) {
		if a.guard != nil {
			http.Error(w, "legacy confirmation disabled", http.StatusServiceUnavailable)
			return
		}
		host, _, _ := net.SplitHostPort(r.RemoteAddr)
		if r.Method != "POST" || host != a.cfg.PeerIP {
			http.Error(w, "forbidden", 403)
			return
		}
		var b struct {
			Epoch      string   `json:"epoch"`
			SnapshotID string   `json:"snapshot_id"`
			Prefixes   []string `json:"prefixes"`
		}
		if e := json.NewDecoder(io.LimitReader(r.Body, 1024*1024)).Decode(&b); e != nil {
			http.Error(w, "bad JSON", 400)
			return
		}
		if e := a.acknowledge(b.Epoch, b.SnapshotID, b.Prefixes); e != nil {
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
	if a.guard != nil && a.recovery != nil {
		a.startGuardRecovery()
	}
	// Guard mode dispatches at most one already journaled local withdrawal per
	// tick. A failure retains its barrier and operation for safe retry.
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for {
			select {
			case <-a.stop:
				return
			case <-tick.C:
				if a.guard != nil && !a.cfg.GCDryRun {
					if err := a.dispatchGuardWithdrawal(); err != nil {
						log.Printf("guard withdrawal deferred: %v", err)
					}
				}
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
	if c.GuardGCEnabled && !guardedTestAllowed(c) {
		return errors.New("guard_gc_enabled is unavailable: isolated guard test build and table required")
	}
	if c.GuardGCEnabled {
		lock, err := acquireGuardProcessLock(c.StateFile)
		if err != nil {
			return err
		}
		defer lock.Close()
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
	hs := newHTTPServer(c, a.httpHandler())
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
