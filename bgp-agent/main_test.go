// SPDX-License-Identifier: GPL-3.0-or-later
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

func TestDesiredLargeSnapshotHasContentLength(t *testing.T) {
	a, _ := testApp(t)
	a.mu.Lock()
	for i := 0; i < 512; i++ {
		prefix := fmt.Sprintf("198.18.%d.%d/32", i/256, i%256)
		a.routes[prefix] = &lease{Prefix: prefix}
	}
	a.mu.Unlock()
	s := httptest.NewServer(a.httpHandler())
	t.Cleanup(s.Close)
	resp, err := s.Client().Get(s.URL + "/desired")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) < 2048 || resp.ContentLength != int64(len(body)) || len(resp.TransferEncoding) != 0 {
		t.Fatalf("unexpected response framing: body=%d content-length=%d transfer-encoding=%v", len(body), resp.ContentLength, resp.TransferEncoding)
	}
	var snapshot struct {
		Epoch    string   `json:"epoch"`
		Prefixes []string `json:"prefixes"`
	}
	if err := json.Unmarshal(body, &snapshot); err != nil || snapshot.Epoch != currentEpoch(a) || len(snapshot.Prefixes) != 512 {
		t.Fatalf("invalid snapshot: epoch=%q prefixes=%d err=%v", snapshot.Epoch, len(snapshot.Prefixes), err)
	}
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

func testAcknowledge(a *app, epoch string, prefixes []string) error {
	_, id, _, err := a.issueSnapshot()
	if err != nil {
		return err
	}
	return a.acknowledge(epoch, id, prefixes)
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
	if err := testAcknowledge(a, currentEpoch(a), []string{"8.8.8.8/32"}); err != nil {
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
				if err := testAcknowledge(a, "old-epoch", []string{"8.8.4.4/32"}); err == nil {
					t.Fatal("old epoch was accepted")
				}
			case "expired":
				if err := testAcknowledge(a, epoch, []string{"8.8.4.4/32"}); err != nil {
					t.Fatal(err)
				}
				a.routes["8.8.4.4/32"].AckAt = time.Now().Add(-2 * time.Second)
			case "reconnected":
				if err := testAcknowledge(a, epoch, []string{"8.8.4.4/32"}); err != nil {
					t.Fatal(err)
				}
				a.peer(false)
				a.peer(true)
				if err := testAcknowledge(a, epoch, []string{"8.8.4.4/32"}); err == nil {
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
	if err := testAcknowledge(a, e, []string{"8.8.8.8/32", "8.8.4.4/32"}); err != nil {
		t.Fatal(err)
	}
	if err := testAcknowledge(a, e, []string{"8.8.8.8/32"}); err != nil {
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

func TestWholeAnswerCapacityIsCheckedBeforePublication(t *testing.T) {
	a, pub := testApp(t)
	a.cfg.MaxRoutes = 1
	err := a.ensure(context.Background(), "two.example.", []netip.Addr{
		netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("8.8.4.4"),
	}, 60, true)
	if err == nil || !strings.Contains(err.Error(), "route capacity reached") {
		t.Fatalf("expected capacity rejection, got %v", err)
	}
	if n, _ := pub.counts(); n != 0 {
		t.Fatalf("partially published %d routes", n)
	}
}

func TestAnsweredRouteIsPersistedBeforeEnsureReturns(t *testing.T) {
	a, pub := testApp(t)
	result := make(chan error, 1)
	go func() {
		result <- a.ensure(context.Background(), "recover.example.", []netip.Addr{netip.MustParseAddr("8.8.8.8")}, 60, true)
	}()
	select {
	case <-pub.notify:
	case <-time.After(time.Second):
		t.Fatal("publish timed out")
	}
	if err := testAcknowledge(a, currentEpoch(a), []string{"8.8.8.8/32"}); err != nil {
		t.Fatal(err)
	}
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	// Simulate process loss: create a new app directly from disk, without a
	// graceful shutdown or an explicit save on the old process.
	p := &fakePublication{}
	recovered, err := newApp(a.cfg, p)
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := p.counts(); n != 1 {
		t.Fatalf("replayed %d routes, want 1", n)
	}
	r := recovered.routes["8.8.8.8/32"]
	if r == nil || !r.AnswerUntil.After(time.Now()) {
		t.Fatalf("missing durable answer lease: %+v", r)
	}
	if !r.AckAt.IsZero() {
		t.Fatal("restart reused stale route acknowledgement")
	}
}

func TestNewGenerationCannotReuseOldDurableLease(t *testing.T) {
	a, pub := testApp(t)
	const prefix = "8.8.8.8/32"
	// Simulate an old durable entry, e.g. a GC/save race or a wall-clock step.
	// Even a later old expiry cannot establish durability of the new path.
	a.durableUntil[prefix] = time.Now().Add(24 * time.Hour)
	a.durableGeneration[prefix] = 1
	a.routeGeneration = 1
	done := make(chan error, 1)
	go func() {
		done <- a.ensure(context.Background(), "readded.example.", []netip.Addr{netip.MustParseAddr("8.8.8.8")}, 60, true)
	}()
	select {
	case <-pub.notify:
	case <-time.After(time.Second):
		t.Fatal("publish timed out")
	}
	deadline := time.Now().Add(time.Second)
	for {
		a.mu.Lock()
		r := a.routes[prefix]
		a.mu.Unlock()
		if r != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("route was not installed")
		}
		time.Sleep(time.Millisecond)
	}
	if err := testAcknowledge(a, currentEpoch(a), []string{prefix}); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(a.cfg.StateFile); err != nil {
		t.Fatalf("new generation not persisted: %v", err)
	}
}

func TestPersistenceFailurePreventsPositiveAnswer(t *testing.T) {
	a, pub := testApp(t)
	a.cfg.StateFile = filepath.Join(t.TempDir(), "missing", "not-a-file")
	if err := os.MkdirAll(a.cfg.StateFile, 0700); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		result <- a.ensure(context.Background(), "diskfail.example.", []netip.Addr{netip.MustParseAddr("8.8.8.8")}, 60, true)
	}()
	select {
	case <-pub.notify:
	case <-time.After(time.Second):
		t.Fatal("publish timed out")
	}
	if err := testAcknowledge(a, currentEpoch(a), []string{"8.8.8.8/32"}); err != nil {
		t.Fatal(err)
	}
	if err := <-result; err == nil || !strings.Contains(err.Error(), "persist answered routes") {
		t.Fatalf("expected persistence failure, got %v", err)
	}
}

func TestConfirmEndpointEnforcesSourceAndAddressPolicy(t *testing.T) {
	a, _ := testApp(t)
	a.cfg.ConfirmClientIP = "192.168.88.222"
	a.cfg.Upstreams = append(a.cfg.Upstreams, "1.1.1.1:53")
	h := a.httpHandler()
	for _, tc := range []struct {
		source, body string
		want         int
	}{
		{"192.168.88.65:1234", `{"domain":"test.example.","ips":["8.8.8.8"],"ttl":30}`, 403},
		{"192.168.88.222:1234", `{"domain":"test.example.","ips":["0.0.0.1"],"ttl":30}`, 400},
		{"192.168.88.222:1234", `{"domain":"test.example.","ips":["10.0.0.1"],"ttl":30}`, 400},
		{"192.168.88.222:1234", `{"domain":"test.example.","ips":["1.1.1.1"],"ttl":30}`, 503},
		{"192.168.88.222:1234", `{"domain":"test.example.","ips":["223.5.5.5","1.1.1.1"],"ttl":30}`, 503},
		{"192.168.88.222:1234", `{"domain":"test.example.","ips":["8.8.8.8"],"ttl":301}`, 400},
		{"192.168.88.222:1234", `{"domain":"not-fqdn","ips":["8.8.8.8"],"ttl":30}`, 400},
		{"192.168.88.222:1234", `{"domain":"test.example.","ips":["223.5.5.5"],"ttl":30}`, 503},
	} {
		r := httptest.NewRequest("POST", "/confirm", strings.NewReader(tc.body))
		r.RemoteAddr = tc.source
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Fatalf("source=%s body=%s code=%d want=%d", tc.source, tc.body, w.Code, tc.want)
		}
	}
	a.cfg.ConfirmClientIP = ""
	r := httptest.NewRequest("POST", "/confirm", strings.NewReader("{}"))
	r.RemoteAddr = "192.168.88.222:1234"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 404 {
		t.Fatalf("disabled endpoint returned %d", w.Code)
	}
}

func TestConfirmEndpointWaitsForAckAndDurableState(t *testing.T) {
	a, pub := testApp(t)
	a.cfg.ConfirmClientIP = "192.168.88.222"
	r := httptest.NewRequest("POST", "/confirm", strings.NewReader(`{"domain":"test.example.","ips":["8.8.8.8"],"ttl":60}`))
	r.RemoteAddr = "192.168.88.222:1234"
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { a.httpHandler().ServeHTTP(w, r); close(done) }()
	select {
	case <-pub.notify:
	case <-time.After(time.Second):
		t.Fatal("publish timeout")
	}
	select {
	case <-done:
		t.Fatal("confirmed before ROS ACK")
	default:
	}
	if err := testAcknowledge(a, currentEpoch(a), []string{"8.8.8.8/32"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("confirmation timed out")
	}
	if w.Code != 204 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	p := &fakePublication{}
	recovered, err := newApp(a.cfg, p)
	if err != nil || recovered.routes["8.8.8.8/32"] == nil {
		t.Fatalf("route not persisted: %v", err)
	}
}

func TestHTTPConfirmSurvivesThreeSecondWait(t *testing.T) {
	a, pub := testApp(t)
	a.cfg.ConfirmClientIP = "127.0.0.1"
	a.cfg.ConfirmMS = 4500
	a.cfg.AckMaxAgeMS = 6000
	s := httptest.NewUnstartedServer(a.httpHandler())
	s.Config = newHTTPServer(a.cfg, a.httpHandler())
	s.Start()
	t.Cleanup(s.Close)
	done := make(chan error, 1)
	go func() {
		resp, err := s.Client().Post(s.URL+"/confirm", "application/json", strings.NewReader(`{"domain":"wait.example.","ips":["8.8.8.8"],"ttl":60}`))
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode != http.StatusNoContent {
				err = fmt.Errorf("unexpected HTTP %d", resp.StatusCode)
			}
		}
		done <- err
	}()
	select {
	case <-pub.notify:
	case <-time.After(time.Second):
		t.Fatal("publish timed out")
	}
	time.Sleep(3200 * time.Millisecond)
	if err := testAcknowledge(a, currentEpoch(a), []string{"8.8.8.8/32"}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("HTTP confirmation did not return")
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
		_, id, _, err := a.issueSnapshot()
		if err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest("POST", "/ack", strings.NewReader(fmt.Sprintf(`{"epoch":%q,"snapshot_id":%q,"prefixes":[]}`, tc.epoch, id)))
		r.RemoteAddr = tc.remote
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Fatalf("remote=%s epoch=%s got=%d want=%d body=%s", tc.remote, tc.epoch, w.Code, tc.want, w.Body.String())
		}
	}
}

func TestStateHTTPRequiresLoopbackOrConfiguredPeer(t *testing.T) {
	a, _ := testApp(t)
	h := a.httpHandler()
	for _, tc := range []struct {
		remote string
		want   int
	}{
		{"127.0.0.1:1234", http.StatusOK},
		{"[::1]:1234", http.StatusOK},
		{"192.168.88.254:1234", http.StatusOK},
		{"192.168.88.222:1234", http.StatusForbidden},
		{"192.168.88.65:1234", http.StatusForbidden},
		{"invalid", http.StatusForbidden},
	} {
		r := httptest.NewRequest("GET", "/state", nil)
		r.RemoteAddr = tc.remote
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Errorf("remote=%q status=%d want=%d", tc.remote, w.Code, tc.want)
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
	if err := testAcknowledge(a, currentEpoch(a), []string{"8.8.8.8/32"}); err != nil {
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
	_, oldSnapshot, _, err := a.issueSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	a.gc(now)
	if _, deletes := pub.counts(); deletes != 1 || a.routes["8.8.8.8/32"] != nil {
		t.Fatal("old path was not withdrawn")
	}
	if currentEpoch(a) != oldEpoch || !a.routes["8.8.4.4/32"].AckAt.Equal(now) {
		t.Fatal("withdraw incorrectly invalidated unrelated route acknowledgement")
	}
	done := make(chan error, 1)
	go func() { done <- a.ensure(context.Background(), "github.com.", []netip.Addr{ip}, 300, true) }()
	select {
	case <-pub.notify:
	case <-time.After(time.Second):
		t.Fatal("withdrawn prefix was not republished")
	}
	if err := a.acknowledge(oldEpoch, oldSnapshot, []string{"8.8.8.8/32"}); err == nil {
		t.Fatal("pre-withdraw acknowledgement was accepted for a new path")
	}
	select {
	case err := <-done:
		t.Fatalf("new path passed gate using old acknowledgement: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	if err := testAcknowledge(a, currentEpoch(a), []string{"8.8.8.8/32", "8.8.4.4/32"}); err != nil {
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

func TestOlderSnapshotCannotClearNewGenerationAcknowledgement(t *testing.T) {
	a, _ := testApp(t)
	now := time.Now()
	a.routes["8.8.8.8/32"] = &lease{Prefix: "8.8.8.8/32", generation: 1, AckAt: now}
	epoch, id, _, err := a.issueSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	// A route unknown to this snapshot acquired its own confirmation after it
	// was issued. Applying the older complete snapshot must not erase it.
	a.routes["8.8.4.4/32"] = &lease{Prefix: "8.8.4.4/32", generation: 2, AckAt: now}
	if err := a.acknowledge(epoch, id, []string{"8.8.8.8/32"}); err != nil {
		t.Fatal(err)
	}
	if !a.routes["8.8.4.4/32"].AckAt.Equal(now) {
		t.Fatal("unseen new generation acknowledgement was cleared")
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
		m, err := a.resolveOnce(ctx, q, false, true)
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
	if err := testAcknowledge(a, currentEpoch(a), []string{"8.8.8.8/32"}); err != nil {
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

func TestDNSResponseUDPTruncationAndTCPFallback(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", pc.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	handler := dns.HandlerFunc(func(w dns.ResponseWriter, q *dns.Msg) {
		m := new(dns.Msg)
		m.SetReply(q)
		for i := 0; i < 30; i++ {
			m.Answer = append(m.Answer, &dns.TXT{Hdr: dns.RR_Header{Name: q.Question[0].Name, Rrtype: dns.TypeTXT, Class: dns.ClassINET, Ttl: 60}, Txt: []string{strings.Repeat("a", 80)}})
		}
		m.SetEdns0(4096, false) // an upstream OPT must not leak to a non-EDNS client
		_ = writeDNSResponse(w, q, m)
	})
	startedUDP, startedTCP := make(chan struct{}), make(chan struct{})
	u := &dns.Server{PacketConn: pc, Handler: handler, NotifyStartedFunc: func() { close(startedUDP) }}
	s := &dns.Server{Listener: ln, Handler: handler, NotifyStartedFunc: func() { close(startedTCP) }}
	go func() { _ = u.ActivateAndServe() }()
	go func() { _ = s.ActivateAndServe() }()
	<-startedUDP
	<-startedTCP
	t.Cleanup(func() { _ = u.Shutdown(); _ = s.Shutdown() })
	for _, size := range []uint16{0, 512, 1232, 4096} {
		q := new(dns.Msg)
		q.SetQuestion("large.example.", dns.TypeTXT)
		if size > 0 {
			q.SetEdns0(size, false)
		}
		m, _, err := (&dns.Client{Timeout: time.Second}).Exchange(q, pc.LocalAddr().String())
		if err != nil {
			t.Fatalf("size %d UDP: %v", size, err)
		}
		limit := int(size)
		if limit < 512 {
			limit = 512
		}
		if limit > 1232 {
			limit = 1232
		}
		m.Compress = true
		wire, err := m.Pack()
		if err != nil {
			t.Fatal(err)
		}
		if len(wire) > limit || !m.Truncated {
			t.Fatalf("size %d: UDP wire=%d TC=%v", size, len(wire), m.Truncated)
		}
		if size == 0 && m.IsEdns0() != nil {
			t.Fatal("OPT leaked to non-EDNS client")
		}
		full, _, err := (&dns.Client{Net: "tcp", Timeout: time.Second}).Exchange(q, pc.LocalAddr().String())
		if err != nil || full.Truncated || len(full.Answer) != 30 {
			t.Fatalf("size %d TCP: answer=%v err=%v", size, full, err)
		}
		if size == 0 && full.IsEdns0() != nil {
			t.Fatal("OPT leaked to non-EDNS TCP client")
		}
	}
}

func TestDNSCacheSeparatesEDNSOptions(t *testing.T) {
	a, _ := testApp(t)
	addr, count := testDNSUpstream(t, 60)
	a.cfg.Upstreams = []string{addr}
	plain := new(dns.Msg)
	plain.SetQuestion("cache.example.", dns.TypeA)
	edns := plain.Copy()
	edns.SetEdns0(1232, false)
	other := plain.Copy()
	other.SetEdns0(4096, false)
	for _, q := range []*dns.Msg{plain, edns, other, plain, edns, other} {
		if _, err := a.lookup(context.Background(), q, false); err != nil {
			t.Fatal(err)
		}
	}
	if got := count.Load(); got != 3 {
		t.Fatalf("upstream requests = %d, want 3 distinct EDNS cache entries", got)
	}
}

func TestEndToEndLargeAnswerFromUpstream(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", pc.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	upstream := dns.HandlerFunc(func(w dns.ResponseWriter, q *dns.Msg) {
		m := new(dns.Msg)
		m.SetReply(q)
		for i := 0; i < 30; i++ {
			m.Answer = append(m.Answer, &dns.TXT{Hdr: dns.RR_Header{Name: q.Question[0].Name, Rrtype: dns.TypeTXT, Class: dns.ClassINET, Ttl: 60}, Txt: []string{strings.Repeat("b", 80)}})
		}
		if _, udp := w.RemoteAddr().(*net.UDPAddr); udp {
			m.Truncate(512)
		}
		_ = w.WriteMsg(m)
	})
	ready1, ready2 := make(chan struct{}), make(chan struct{})
	u := &dns.Server{PacketConn: pc, Handler: upstream, NotifyStartedFunc: func() { close(ready1) }}
	s := &dns.Server{Listener: ln, Handler: upstream, NotifyStartedFunc: func() { close(ready2) }}
	go func() { _ = u.ActivateAndServe() }()
	go func() { _ = s.ActivateAndServe() }()
	<-ready1
	<-ready2
	t.Cleanup(func() { _ = u.Shutdown(); _ = s.Shutdown() })
	a, _ := testApp(t)
	a.cfg.Upstreams = []string{pc.LocalAddr().String()}
	frontPC, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	frontLN, err := net.Listen("tcp", frontPC.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	ready3, ready4 := make(chan struct{}), make(chan struct{})
	fudp := &dns.Server{PacketConn: frontPC, Handler: a, NotifyStartedFunc: func() { close(ready3) }}
	ftcp := &dns.Server{Listener: frontLN, Handler: a, NotifyStartedFunc: func() { close(ready4) }}
	go func() { _ = fudp.ActivateAndServe() }()
	go func() { _ = ftcp.ActivateAndServe() }()
	<-ready3
	<-ready4
	t.Cleanup(func() { _ = fudp.Shutdown(); _ = ftcp.Shutdown() })
	q := new(dns.Msg)
	q.SetQuestion("large.example.", dns.TypeTXT)
	m, _, err := (&dns.Client{Timeout: time.Second}).Exchange(q, frontPC.LocalAddr().String())
	if err != nil || !m.Truncated {
		t.Fatalf("UDP result=%v err=%v", m, err)
	}
	full, _, err := (&dns.Client{Net: "tcp", Timeout: time.Second}).Exchange(q, frontPC.LocalAddr().String())
	if err != nil || full.Truncated || len(full.Answer) != 30 {
		t.Fatalf("TCP result=%v err=%v", full, err)
	}
}

func TestSnapshotsBoundAgeReplayAndOrdering(t *testing.T) {
	a, _ := testApp(t)
	a.routes["8.8.8.8/32"] = &lease{Prefix: "8.8.8.8/32"}
	a.routes["8.8.4.4/32"] = &lease{Prefix: "8.8.4.4/32"}
	epoch, older, _, err := a.issueSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	_, newer, _, err := a.issueSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	a.mu.Lock()
	issued := a.snapshots[newer].issued
	a.mu.Unlock()
	if err := a.acknowledge(epoch, newer, []string{"8.8.8.8/32"}); err != nil {
		t.Fatal(err)
	}
	if got := a.routes["8.8.8.8/32"].AckAt; got != issued {
		t.Fatalf("ack freshness not based on issuance: %v != %v", got, issued)
	}
	for _, id := range []string{newer, older, "unknown", ""} {
		if err := a.acknowledge(epoch, id, []string{"8.8.4.4/32"}); err == nil {
			t.Fatalf("accepted replay or older id %q", id)
		}
		if a.routes["8.8.8.8/32"].AckAt != issued || !a.routes["8.8.4.4/32"].AckAt.IsZero() {
			t.Fatal("rejected ACK modified existing snapshot")
		}
	}
	_, expired, _, err := a.issueSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	a.mu.Lock()
	snap := a.snapshots[expired]
	snap.issued = snap.issued.Add(-2 * time.Second)
	a.snapshots[expired] = snap
	a.mu.Unlock()
	if err := a.acknowledge(epoch, expired, []string{"8.8.4.4/32"}); err == nil {
		t.Fatal("accepted expired snapshot")
	}
	if a.routes["8.8.8.8/32"].AckAt != issued {
		t.Fatal("expired ACK modified existing snapshot")
	}
	_, restricted, _, err := a.issueSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if err := a.acknowledge(epoch, restricted, []string{"1.1.1.1/32"}); err == nil {
		t.Fatal("accepted prefix not in issued snapshot")
	}
	if a.routes["8.8.8.8/32"].AckAt != issued {
		t.Fatal("invalid ACK modified existing snapshot")
	}
}

func TestPendingSnapshotBoundAndExpiryCleanup(t *testing.T) {
	a, _ := testApp(t)
	for i := 0; i < maxPendingSnapshots; i++ {
		if _, _, _, err := a.issueSnapshot(); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, _, err := a.issueSnapshot(); err == nil {
		t.Fatal("snapshot capacity was not enforced")
	}
	a.mu.Lock()
	for id, snap := range a.snapshots {
		snap.issued = snap.issued.Add(-2 * time.Second)
		a.snapshots[id] = snap
	}
	a.mu.Unlock()
	if _, _, _, err := a.issueSnapshot(); err != nil {
		t.Fatalf("expired snapshot cleanup failed: %v", err)
	}
	a.mu.Lock()
	count := len(a.snapshots)
	a.mu.Unlock()
	if count != 1 {
		t.Fatalf("pending snapshots %d, want 1", count)
	}
}

func TestDesiredRestrictsSnapshotIssuance(t *testing.T) {
	a, _ := testApp(t)
	h := a.httpHandler()
	for _, tc := range []struct {
		remote string
		want   int
	}{
		{"192.168.88.65:1234", http.StatusForbidden},
		{"192.168.88.254:1234", http.StatusOK},
		{"127.0.0.1:1234", http.StatusOK},
		{"malformed", http.StatusForbidden},
	} {
		r := httptest.NewRequest("GET", "/desired", nil)
		r.RemoteAddr = tc.remote
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Fatalf("remote=%s got=%d want=%d", tc.remote, w.Code, tc.want)
		}
	}
	a.mu.Lock()
	pending := len(a.snapshots)
	a.mu.Unlock()
	if pending != 2 {
		t.Fatalf("unauthorized requests issued snapshot: pending=%d", pending)
	}
}

func TestResolveRefreshesOnceWhenAnswerExpiresWaitingForAck(t *testing.T) {
	a, pub := testApp(t)
	a.cfg.ConfirmMS = 2000
	a.cfg.AckMaxAgeMS = 3000
	addr, count := testDNSUpstream(t, 1)
	a.cfg.Upstreams = []string{addr}
	ackDone := make(chan error, 1)
	go func() {
		select {
		case <-pub.notify:
		case <-time.After(time.Second):
			ackDone <- errors.New("publication not observed")
			return
		}
		time.Sleep(1100 * time.Millisecond)
		ackDone <- testAcknowledge(a, currentEpoch(a), []string{"8.8.8.8/32"})
	}()
	q := new(dns.Msg)
	q.SetQuestion("short.example.", dns.TypeA)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	m, err := a.resolve(ctx, q, false, true)
	if ackErr := <-ackDone; ackErr != nil {
		t.Fatal(ackErr)
	}
	if err != nil {
		t.Fatalf("expired answer was not refreshed: %v", err)
	}
	if count.Load() != 2 {
		t.Fatalf("upstream queries=%d want first+one refresh", count.Load())
	}
	if len(m.Answer) != 1 || m.Answer[0].Header().Ttl > 1 {
		t.Fatalf("unexpected answer or raised TTL: %v", m.Answer)
	}
}
