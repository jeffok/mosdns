// SPDX-License-Identifier: GPL-3.0-or-later
package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
)

type fakePublication struct {
	mu      sync.Mutex
	added   []string
	deleted []string
	notify  chan string
	addErr  error
	delErr  error
}

func (p *fakePublication) Add(prefix string) ([]byte, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.addErr != nil {
		return nil, p.addErr
	}
	p.added = append(p.added, prefix)
	if p.notify != nil {
		select {
		case p.notify <- prefix:
		default:
		}
	}
	return []byte(fmt.Sprintf("path-%d", len(p.added))), nil
}

func (p *fakePublication) Delete(id []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.delErr != nil {
		return p.delErr
	}
	p.deleted = append(p.deleted, string(id))
	return nil
}

func (p *fakePublication) counts() (int, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.added), len(p.deleted)
}

func testConfig(t *testing.T) config {
	t.Helper()
	dir := t.TempDir()
	cn := filepath.Join(dir, "cn.txt")
	if err := os.WriteFile(cn, []byte("223.5.0.0/16\n114.114.0.0/16\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return config{
		LocalIP: "192.168.88.223", PeerIP: "192.168.88.254",
		ASN: 64513, PeerASN: 64512, NextHop: "10.0.201.9",
		Upstreams: []string{"127.0.0.1:9"}, CNFile: cn,
		StateFile: filepath.Join(dir, "state.json"), TTLMax: 300,
		RetainSeconds: 86400, ConfirmMS: 120, AckMaxAgeMS: 1000,
		MaxRoutes: 16, GCDryRun: true,
	}
}

func testApp(t *testing.T) (*app, *fakePublication) {
	t.Helper()
	p := &fakePublication{notify: make(chan string, 8)}
	a, err := newApp(testConfig(t), p)
	if err != nil {
		t.Fatal(err)
	}
	a.peer(true)
	return a, p
}

func currentEpoch(a *app) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.epoch
}

func TestEnsureWaitsForROSActiveRouteAcknowledgement(t *testing.T) {
	a, pub := testApp(t)
	ip := netip.MustParseAddr("8.8.8.8")
	done := make(chan error, 1)
	go func() { done <- a.ensure(context.Background(), "github.com.", []netip.Addr{ip}, 300, true) }()
	select {
	case prefix := <-pub.notify:
		if prefix != "8.8.8.8/32" {
			t.Fatal(prefix)
		}
	case <-time.After(time.Second):
		t.Fatal("route was not locally published")
	}
	select {
	case err := <-done:
		t.Fatalf("DNS route gate returned before ROS acknowledgement: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	if err := a.acknowledge(currentEpoch(a), []string{"8.8.8.8/32"}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("fresh ROS acknowledgement did not release DNS gate")
	}
	if err := a.ensure(context.Background(), "api.github.com.", []netip.Addr{ip}, 60, true); err != nil {
		t.Fatal(err)
	}
	adds, _ := pub.counts()
	if adds != 1 {
		t.Fatalf("same IP republished %d times", adds)
	}
	a.mu.Lock()
	r := a.routes["8.8.8.8/32"]
	if len(r.References) != 2 || time.Until(r.RetainUntil) < 23*time.Hour {
		t.Fatalf("domain references or retention not updated: %+v", r)
	}
	a.mu.Unlock()
}

func TestStaleAcknowledgementsCannotReleaseDNS(t *testing.T) {
	for _, mode := range []string{"wrong-epoch", "expired", "reconnected"} {
		t.Run(mode, func(t *testing.T) {
			a, _ := testApp(t)
			a.cfg.ConfirmMS = 35
			ip := netip.MustParseAddr("8.8.4.4")
			a.routes["8.8.4.4/32"] = &lease{Prefix: "8.8.4.4/32", UUID: []byte("old"), References: map[string]time.Time{}}
			epoch := currentEpoch(a)
			switch mode {
			case "wrong-epoch":
				if err := a.acknowledge("old-epoch", []string{"8.8.4.4/32"}); err == nil {
					t.Fatal("old epoch was accepted")
				}
			case "expired":
				if err := a.acknowledge(epoch, []string{"8.8.4.4/32"}); err != nil {
					t.Fatal(err)
				}
				a.routes["8.8.4.4/32"].AckAt = time.Now().Add(-2 * time.Second)
			case "reconnected":
				if err := a.acknowledge(epoch, []string{"8.8.4.4/32"}); err != nil {
					t.Fatal(err)
				}
				a.peer(false)
				a.peer(true)
				if err := a.acknowledge(epoch, []string{"8.8.4.4/32"}); err == nil {
					t.Fatal("previous session acknowledgement survived reconnection")
				}
			}
			if err := a.ensure(context.Background(), "github.com.", []netip.Addr{ip}, 300, true); err == nil {
				t.Fatal("DNS succeeded without a fresh current-session acknowledgement")
			}
		})
	}
}

func TestAcknowledgementSnapshotInvalidatesOmittedRoutes(t *testing.T) {
	a, _ := testApp(t)
	a.routes["8.8.8.8/32"] = &lease{Prefix: "8.8.8.8/32"}
	a.routes["8.8.4.4/32"] = &lease{Prefix: "8.8.4.4/32"}
	e := currentEpoch(a)
	if err := a.acknowledge(e, []string{"8.8.8.8/32", "8.8.4.4/32"}); err != nil {
		t.Fatal(err)
	}
	if err := a.acknowledge(e, []string{"8.8.8.8/32"}); err != nil {
		t.Fatal(err)
	}
	if !a.routes["8.8.4.4/32"].AckAt.IsZero() || a.routes["8.8.8.8/32"].AckAt.IsZero() {
		t.Fatal("omitted ROS route retained a usable acknowledgement")
	}
}

func TestGCPreservesSharedIPAndOutstandingAnswerLifetime(t *testing.T) {
	a, pub := testApp(t)
	a.cfg.GCDryRun = false
	now := time.Now()
	a.routes["8.8.8.8/32"] = &lease{Prefix: "8.8.8.8/32", UUID: []byte("shared"), RetainUntil: now.Add(-time.Hour), AnswerUntil: now.Add(-time.Hour), References: map[string]time.Time{"old.example.": now.Add(-time.Minute), "current.example.": now.Add(time.Minute)}}
	a.gc(now)
	if _, deletes := pub.counts(); deletes != 0 {
		t.Fatal("shared route withdrawn while another domain still references it")
	}
	if _, ok := a.routes["8.8.8.8/32"].References["old.example."]; ok {
		t.Fatal("expired domain reference was not removed")
	}
	a.routes["8.8.8.8/32"].References = map[string]time.Time{}
	a.routes["8.8.8.8/32"].AnswerUntil = now
	a.gc(now.Add(4 * time.Minute))
	if _, deletes := pub.counts(); deletes != 0 {
		t.Fatal("route withdrawn before client answer lifetime plus safety window")
	}
	a.gc(now.Add(6 * time.Minute))
	if _, deletes := pub.counts(); deletes != 1 || len(a.routes) != 0 {
		t.Fatalf("eligible cold route was not withdrawn: deletes=%d routes=%d", deletes, len(a.routes))
	}
}

func TestGCDryRunAndWithdrawalFailure(t *testing.T) {
	a, pub := testApp(t)
	now := time.Now()
	a.routes["8.8.8.8/32"] = &lease{Prefix: "8.8.8.8/32", UUID: []byte("cold"), RetainUntil: now.Add(-time.Hour), AnswerUntil: now.Add(-time.Hour), References: map[string]time.Time{}}
	a.gc(now)
	if _, deletes := pub.counts(); deletes != 0 || len(a.routes) != 1 || a.candidates.Load() != 1 {
		t.Fatal("dry-run altered BGP publication or failed to record candidate")
	}
	a.cfg.GCDryRun = false
	pub.delErr = errors.New("BGP unavailable")
	a.gc(now)
	if len(a.routes) != 1 {
		t.Fatal("failed withdrawal discarded local record")
	}
	pub.delErr = nil
	a.gc(now)
	if _, deletes := pub.counts(); deletes != 1 || len(a.routes) != 0 {
		t.Fatal("successful withdrawal did not remove cold route")
	}
}

func TestPersistenceReplaysRoutesButNeverTrustsSavedAck(t *testing.T) {
	a, _ := testApp(t)
	now := time.Now()
	a.routes["8.8.8.8/32"] = &lease{Prefix: "8.8.8.8/32", UUID: []byte("old-uuid"), LastSeen: now, RetainUntil: now.Add(24 * time.Hour), AnswerUntil: now.Add(300 * time.Second), References: map[string]time.Time{"github.com.": now.Add(300 * time.Second)}, AckAt: now}
	if err := a.save(); err != nil {
		t.Fatal(err)
	}
	pub := &fakePublication{}
	restored, err := newApp(a.cfg, pub)
	if err != nil {
		t.Fatal(err)
	}
	r := restored.routes["8.8.8.8/32"]
	if r == nil || !r.AckAt.IsZero() || string(r.UUID) == "old-uuid" || len(r.References) != 1 || restored.up {
		t.Fatalf("invalid recovered state: %+v up=%v", r, restored.up)
	}
	if adds, _ := pub.counts(); adds != 1 {
		t.Fatalf("recovery published %d routes, want 1", adds)
	}
	if restored.epoch == a.epoch {
		t.Fatal("recovered process reused acknowledgement epoch")
	}
}

func TestPublicForeignIPv4Eligibility(t *testing.T) {
	a, _ := testApp(t)
	for _, ip := range []string{"223.5.5.5", "114.114.114.114", "10.0.0.1", "192.168.88.1", "172.16.0.1", "127.0.0.1", "169.254.1.1", "100.64.0.1", "192.0.2.1", "198.18.0.1", "198.51.100.1", "203.0.113.1", "224.0.0.1", "240.0.0.1", "255.255.255.255", "::1", "2001:4860:4860::8888"} {
		if a.eligible(netip.MustParseAddr(ip)) {
			t.Errorf("must not publish %s", ip)
		}
	}
	for _, ip := range []string{"8.8.8.8", "1.1.1.1", "20.205.243.166"} {
		if !a.eligible(netip.MustParseAddr(ip)) {
			t.Errorf("foreign public IPv4 rejected: %s", ip)
		}
	}
}

func TestCapacityRejectsNewRoutesWithoutEvictingExisting(t *testing.T) {
	a, pub := testApp(t)
	a.cfg.MaxRoutes = 1
	a.cfg.ConfirmMS = 1
	_ = a.ensure(context.Background(), "one.example.", []netip.Addr{netip.MustParseAddr("8.8.8.8")}, 60, true)
	err := a.ensure(context.Background(), "two.example.", []netip.Addr{netip.MustParseAddr("8.8.4.4")}, 60, true)
	if err == nil || !strings.Contains(err.Error(), "capacity") {
		t.Fatalf("new route did not fail explicitly at capacity: %v", err)
	}
	if adds, _ := pub.counts(); adds != 1 || len(a.routes) != 1 || a.routes["8.8.8.8/32"] == nil {
		t.Fatal("capacity limit evicted an existing route or published beyond limit")
	}
}

func testDNSUpstream(t *testing.T, ttl uint32) (string, *atomic.Int32) {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	count := &atomic.Int32{}
	started := make(chan struct{})
	s := &dns.Server{PacketConn: pc, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, q *dns.Msg) {
		count.Add(1)
		m := new(dns.Msg)
		m.SetReply(q)
		m.Answer = []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: q.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: ttl}, A: net.ParseIP("8.8.8.8")}}
		_ = w.WriteMsg(m)
	}), NotifyStartedFunc: func() { close(started) }}
	go func() { _ = s.ActivateAndServe() }()
	<-started
	t.Cleanup(func() { _ = s.Shutdown() })
	return pc.LocalAddr().String(), count
}

func TestDNSCacheClampsTTLAndPreservesShorterTTL(t *testing.T) {
	for _, upstreamTTL := range []uint32{7200, 30} {
		t.Run(fmt.Sprint(upstreamTTL), func(t *testing.T) {
			a, _ := testApp(t)
			addr, count := testDNSUpstream(t, upstreamTTL)
			a.cfg.Upstreams = []string{addr}
			q := new(dns.Msg)
			q.SetQuestion("github.com.", dns.TypeA)
			m, err := a.lookup(context.Background(), q, false)
			if err != nil {
				t.Fatal(err)
			}
			want := upstreamTTL
			if want > 300 {
				want = 300
			}
			if got := m.Answer[0].Header().Ttl; got != want {
				t.Fatalf("TTL=%d want=%d", got, want)
			}
			key := "github.com./1/1/false/false"
			a.mu.Lock()
			c := a.cache[key]
			c.stored = c.stored.Add(-10 * time.Second)
			c.expires = c.expires.Add(-10 * time.Second)
			a.cache[key] = c
			a.mu.Unlock()
			q.Id++
			cached, err := a.lookup(context.Background(), q, false)
			if err != nil {
				t.Fatal(err)
			}
			got := cached.Answer[0].Header().Ttl
			if got > want-10 || got < want-11 || cached.Id != q.Id {
				t.Fatalf("cached response TTL/id invalid: ttl=%d original=%d id=%d want=%d", got, want, cached.Id, q.Id)
			}
			if count.Load() != 1 || a.hits.Load() != 1 {
				t.Fatalf("cache hit made upstream query: queries=%d hits=%d", count.Load(), a.hits.Load())
			}
			if m.Answer[0].Header().Ttl != want {
				t.Fatal("cache hit mutated previously returned answer")
			}
		})
	}
}

func TestWarmRefreshBypassesCache(t *testing.T) {
	a, _ := testApp(t)
	addr, count := testDNSUpstream(t, 300)
	a.cfg.Upstreams = []string{addr}
	q := new(dns.Msg)
	q.SetQuestion("github.com.", dns.TypeA)
	for _, refresh := range []bool{false, false, true} {
		if _, err := a.lookup(context.Background(), q, refresh); err != nil {
			t.Fatal(err)
		}
	}
	if count.Load() != 2 {
		t.Fatalf("explicit refresh did not query upstream through cache: %d", count.Load())
	}
}

func TestCachedAnswerStillRequiresCurrentROSAck(t *testing.T) {
	a, _ := testApp(t)
	a.cfg.ConfirmMS = 25
	q := new(dns.Msg)
	q.SetQuestion("github.com.", dns.TypeA)
	m := new(dns.Msg)
	m.SetReply(q)
	m.Answer = []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: "github.com.", Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 300}, A: net.ParseIP("8.8.8.8")}}
	now := time.Now()
	a.cache["github.com./1/1/false/false"] = cached{m, now, now.Add(300 * time.Second)}
	if answer, err := a.resolve(context.Background(), q, false, true); err == nil || answer != nil {
		t.Fatalf("cached DNS bypassed ROS route acknowledgement: answer=%v err=%v", answer, err)
	}
}

func TestAckHTTPRejectsOtherSourcesAndOldEpoch(t *testing.T) {
	a, _ := testApp(t)
	h := a.httpHandler()
	for _, tc := range []struct {
		remote, epoch string
		want          int
	}{
		{"192.168.88.65:1234", currentEpoch(a), http.StatusForbidden},
		{"192.168.88.254:1234", "old-epoch", http.StatusConflict},
		{"192.168.88.254:1234", currentEpoch(a), http.StatusNoContent},
	} {
		r := httptest.NewRequest("POST", "/ack", strings.NewReader(fmt.Sprintf(`{"epoch":%q,"prefixes":[]}`, tc.epoch)))
		r.RemoteAddr = tc.remote
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Fatalf("remote=%s epoch=%s got=%d want=%d body=%s", tc.remote, tc.epoch, w.Code, tc.want, w.Body.String())
		}
	}
}

func TestFailedPublicationNeverBecomesUsableLease(t *testing.T) {
	a, pub := testApp(t)
	pub.addErr = errors.New("BGP RIB rejected path")
	err := a.ensure(context.Background(), "github.com.", []netip.Addr{netip.MustParseAddr("8.8.8.8")}, 300, true)
	if err == nil || len(a.routes) != 0 || a.adds.Load() != 0 {
		t.Fatalf("failed publication remembered as a lease: err=%v routes=%v adds=%d", err, a.routes, a.adds.Load())
	}
}

func TestConcurrentSameIPPublishesOnce(t *testing.T) {
	a, pub := testApp(t)
	ip := netip.MustParseAddr("8.8.8.8")
	const n = 12
	done := make(chan error, n)
	for i := 0; i < n; i++ {
		go func(i int) {
			done <- a.ensure(context.Background(), fmt.Sprintf("name%d.example.", i), []netip.Addr{ip}, 300, true)
		}(i)
	}
	select {
	case <-pub.notify:
	case <-time.After(time.Second):
		t.Fatal("route never published")
	}
	if err := a.acknowledge(currentEpoch(a), []string{"8.8.8.8/32"}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			t.Fatal("concurrent DNS waiter did not finish")
		}
	}
	if adds, _ := pub.counts(); adds != 1 || len(a.routes["8.8.8.8/32"].References) != n {
		t.Fatalf("concurrent queries lost references or repeated publication: adds=%d refs=%d", adds, len(a.routes["8.8.8.8/32"].References))
	}
}

func TestDNSWireReturnsSERVFAILInsteadOfUnconfirmedAddress(t *testing.T) {
	a, _ := testApp(t)
	a.cfg.ConfirmMS = 25
	addr, _ := testDNSUpstream(t, 300)
	a.cfg.Upstreams = []string{addr}
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	s := &dns.Server{PacketConn: pc, Handler: a, NotifyStartedFunc: func() { close(started) }}
	go func() { _ = s.ActivateAndServe() }()
	<-started
	t.Cleanup(func() { _ = s.Shutdown() })
	q := new(dns.Msg)
	q.SetQuestion("github.com.", dns.TypeA)
	m, _, err := (&dns.Client{Timeout: time.Second}).Exchange(q, pc.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	if m.Rcode != dns.RcodeServerFailure || len(m.Answer) != 0 {
		t.Fatalf("unconfirmed IP escaped in DNS answer: %s", m)
	}
	if a.failures.Load() != 1 {
		t.Fatalf("DNS failure not counted: %d", a.failures.Load())
	}
}

func TestWithdrawAndReaddRejectsPreviousRouteAcknowledgement(t *testing.T) {
	a, pub := testApp(t)
	a.cfg.GCDryRun = false
	now := time.Now()
	ip := netip.MustParseAddr("8.8.8.8")
	a.routes["8.8.8.8/32"] = &lease{Prefix: "8.8.8.8/32", UUID: []byte("old-path"), RetainUntil: now.Add(-time.Hour), AnswerUntil: now.Add(-time.Hour), References: map[string]time.Time{}, AckAt: now}
	a.routes["8.8.4.4/32"] = &lease{Prefix: "8.8.4.4/32", UUID: []byte("kept-path"), RetainUntil: now.Add(time.Hour), References: map[string]time.Time{}, AckAt: now}
	oldEpoch := currentEpoch(a)
	a.gc(now)
	if _, deletes := pub.counts(); deletes != 1 || a.routes["8.8.8.8/32"] != nil {
		t.Fatal("old path was not withdrawn")
	}
	if currentEpoch(a) == oldEpoch || !a.routes["8.8.4.4/32"].AckAt.IsZero() {
		t.Fatal("withdraw did not invalidate old snapshot epoch and acknowledgements")
	}
	done := make(chan error, 1)
	go func() { done <- a.ensure(context.Background(), "github.com.", []netip.Addr{ip}, 300, true) }()
	select {
	case <-pub.notify:
	case <-time.After(time.Second):
		t.Fatal("withdrawn prefix was not republished")
	}
	if err := a.acknowledge(oldEpoch, []string{"8.8.8.8/32"}); err == nil {
		t.Fatal("pre-withdraw acknowledgement was accepted for a new path")
	}
	select {
	case err := <-done:
		t.Fatalf("new path passed gate using old acknowledgement: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	if err := a.acknowledge(currentEpoch(a), []string{"8.8.8.8/32", "8.8.4.4/32"}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("new path not released by current acknowledgement")
	}
}

func TestShortTTLExpiringDuringRouteConfirmationIsNotReturned(t *testing.T) {
	a, pub := testApp(t)
	a.cfg.ConfirmMS = 2500
	addr, _ := testDNSUpstream(t, 1)
	a.cfg.Upstreams = []string{addr}
	q := new(dns.Msg)
	q.SetQuestion("short.example.", dns.TypeA)
	type result struct {
		m   *dns.Msg
		err error
	}
	done := make(chan result, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		m, err := a.resolve(ctx, q, false, true)
		done <- result{m, err}
	}()
	select {
	case <-pub.notify:
	case <-time.After(time.Second):
		t.Fatal("short-lived answer was not published")
	}
	// The real DNS TTL is one second: do not make it last longer while waiting
	// for the forwarding table. A late valid ACK must still reject that answer.
	time.Sleep(1100 * time.Millisecond)
	if err := a.acknowledge(currentEpoch(a), []string{"8.8.8.8/32"}); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-done:
		if r.err == nil || !strings.Contains(r.err.Error(), "expired") || r.m != nil {
			t.Fatalf("expired DNS result returned after late route confirmation: answer=%v err=%v", r.m, r.err)
		}
	case <-time.After(time.Second):
		t.Fatal("late acknowledgement did not finish the lookup")
	}
}

func TestDNSCacheSeparatesDNSSECAndCheckingDisabledQueries(t *testing.T) {
	a, _ := testApp(t)
	addr, count := testDNSUpstream(t, 300)
	a.cfg.Upstreams = []string{addr}
	queries := make([]*dns.Msg, 0, 3)
	plain := new(dns.Msg)
	plain.SetQuestion("github.com.", dns.TypeA)
	queries = append(queries, plain)
	do := plain.Copy()
	do.SetEdns0(1232, true)
	queries = append(queries, do)
	cd := plain.Copy()
	cd.CheckingDisabled = true
	queries = append(queries, cd)
	for _, q := range queries {
		if _, err := a.lookup(context.Background(), q, false); err != nil {
			t.Fatal(err)
		}
	}
	if count.Load() != 3 {
		t.Fatalf("DO/CD query reused incompatible cache entry: upstream queries=%d", count.Load())
	}
}

func TestConfirmationBudgetIncludesWaitingForPublicationSlot(t *testing.T) {
	a, pub := testApp(t)
	a.cfg.ConfirmMS = 25
	a.workOnce.Do(func() { a.work = make(chan struct{}, 1) })
	a.work <- struct{}{}
	defer func() { <-a.work }()
	start := time.Now()
	err := a.ensure(context.Background(), "github.com.", []netip.Addr{netip.MustParseAddr("8.8.8.8")}, 300, true)
	if err == nil || time.Since(start) > 300*time.Millisecond {
		t.Fatalf("publication queue escaped total confirmation budget: elapsed=%s err=%v", time.Since(start), err)
	}
	if adds, _ := pub.counts(); adds != 0 {
		t.Fatal("request published after timing out in queue")
	}
}

type slowContextPublication struct{ fakePublication }

func (p *slowContextPublication) AddContext(ctx context.Context, _ string) ([]byte, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestConfirmationBudgetReachesBGPAddContext(t *testing.T) {
	c := testConfig(t)
	c.ConfirmMS = 25
	p := &slowContextPublication{}
	a, err := newApp(c, p)
	if err != nil {
		t.Fatal(err)
	}
	a.peer(true)
	start := time.Now()
	err = a.ensure(context.Background(), "github.com.", []netip.Addr{netip.MustParseAddr("8.8.8.8")}, 300, true)
	if err == nil || time.Since(start) > 300*time.Millisecond || len(a.routes) != 0 {
		t.Fatalf("BGP publication escaped budget or left usable state: elapsed=%s routes=%d err=%v", time.Since(start), len(a.routes), err)
	}
}
