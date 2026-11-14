// SPDX-License-Identifier: GPL-3.0-or-later
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func guardTest(t *testing.T, capacity int) *guardCoordinator {
	t.Helper()
	c, e := newGuardCoordinator(filepath.Join(t.TempDir(), "journal.json"), capacity, 100*time.Millisecond, 200*time.Millisecond, 10*time.Millisecond, 5*time.Millisecond, 10*time.Millisecond)
	if e != nil {
		t.Fatal(e)
	}
	return c
}
func guardActivate(t *testing.T, c *guardCoordinator, p string) {
	t.Helper()
	r := c.data.Records[p]
	if e := c.activate(*r.Pending, guardProof{ExactOwner: true, GuardPresent: true, BGPActive: true}); e != nil {
		t.Fatal(e)
	}
}
func TestGuardMultiIPCapacityAndPinnedAnswer(t *testing.T) {
	c := guardTest(t, 2)
	if e := c.prepare([]string{"1.1.1.1/32", "8.8.8.8/32", "9.9.9.9/32"}); e == nil || len(c.data.Records) != 0 {
		t.Fatal("partial capacity reservation")
	}
	ps := []string{"1.1.1.1/32", "8.8.8.8/32"}
	if e := c.prepare(ps); e != nil {
		t.Fatal(e)
	}
	for _, p := range ps {
		guardActivate(t, c, p)
	}
	pin, e := c.reserve(ps, 0)
	if e != nil {
		t.Fatal(e)
	}
	if len(pin.Generations) != 2 || !c.maySend(pin.ID) {
		t.Fatal("missing answer pin")
	}
	time.Sleep(250 * time.Millisecond)
	if c.maySend(pin.ID) {
		t.Fatal("expired send deadline")
	}
	if e = c.retire(ps[0], 0); e == nil {
		t.Fatal("retired while pinned")
	}
	if e = c.finish(pin.ID); e != nil {
		t.Fatal(e)
	}
	if e = c.retire(ps[0], 0); e != nil {
		t.Fatal(e)
	}
	if _, e = c.reserve(ps, 0); e == nil {
		t.Fatal("admitted partial answer while draining")
	}
}
func TestGuardRestartPreservesPendingAndFreezes(t *testing.T) {
	c := guardTest(t, 1)
	p := "1.1.1.1/32"
	if e := c.prepare([]string{p}); e != nil {
		t.Fatal(e)
	}
	guardActivate(t, c, p)
	time.Sleep(12 * time.Millisecond)
	if e := c.retire(p, 0); e != nil {
		t.Fatal(e)
	}
	op := c.pending()[0]
	restored, e := newGuardCoordinator(c.path, 1, c.ttlMax, c.sendMargin, c.clockUncertainty, c.quiet, c.retention)
	if e != nil {
		t.Fatal(e)
	}
	if len(restored.pending()) != 0 {
		t.Fatal("operation exposed during recovery")
	}
	if e = restored.prepare([]string{p}); e == nil {
		t.Fatal("admitted while recovering")
	}
	if e = restored.ack(op, "ok", guardProof{ExactOwner: true, BarrierPresent: true, GuardPresent: true, BGPActive: true}); e == nil {
		t.Fatal("accepted ACK during recovery")
	}
	restored.recoveryUntil = time.Now().Add(-time.Second)
	if len(restored.pending()) != 0 {
		t.Fatal("elapsed grace bypassed external reconciliation")
	}
	if restored.data.Records[p].Pending == nil || *restored.data.Records[p].Pending != op {
		t.Fatal("pending operation identity changed on restart")
	}
}

func TestGuardRestartNeverReusesAnswerPinForSending(t *testing.T) {
	c := guardTest(t, 1)
	p := "1.1.1.1/32"
	if err := c.prepare([]string{p}); err != nil {
		t.Fatal(err)
	}
	guardActivate(t, c, p)
	pin, err := c.reserve([]string{p}, 20*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := newGuardCoordinator(c.path, 1, c.ttlMax, c.sendMargin, c.clockUncertainty, c.quiet, c.retention)
	if err != nil {
		t.Fatal(err)
	}
	if restored.maySend(pin.ID) || restored.finish(pin.ID) == nil {
		t.Fatal("recovered pin allowed during freeze")
	}
	restored.recoveryUntil = time.Now().Add(-time.Second)
	if restored.maySend(pin.ID) {
		t.Fatal("recovered pin reused without monotonic send deadline")
	}
	// Simulate a future externally verified reconciliation; no production
	// unlock implementation exists in this standalone coordinator.
	restored.recoveryNeedsReconcile = false
	if err := restored.finish(pin.ID); err != nil {
		t.Fatal(err)
	}
}

func TestGuardSendCompletionExtendsTTLAndPersistsBeforeUnpin(t *testing.T) {
	c := guardTest(t, 1)
	prefix := "1.1.1.1/32"
	if err := c.prepare([]string{prefix}); err != nil {
		t.Fatal(err)
	}
	guardActivate(t, c, prefix)
	pin, err := c.reserve([]string{prefix}, 80*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if !c.beginSend(pin.ID) || c.beginSend(pin.ID) {
		t.Fatal("pin did not enforce one in-flight write")
	}
	// Simulate a write that started in time but has not returned by its send
	// admission deadline. While it remains in flight, retirement is forbidden.
	c.pinDeadline[pin.ID] = time.Now().Add(-time.Second)
	c.horizonMono[prefix] = time.Now().Add(-time.Second)
	c.retentionMono[prefix] = time.Now().Add(-time.Second)
	if c.maySend(pin.ID) {
		t.Fatal("expired pin permitted a new send")
	}
	if err := c.retire(prefix, 0); err == nil {
		t.Fatal("in-flight answer was retired")
	}
	start := time.Now()
	if err := c.finishSent(pin.ID); err != nil {
		t.Fatal(err)
	}
	if c.horizonMono[prefix].Before(start.Add(pin.TTL)) {
		t.Fatal("completion did not extend the monotonic TTL horizon")
	}
	if err := c.retire(prefix, 0); err == nil {
		t.Fatal("retired before the completed answer's TTL")
	}
	data, err := os.ReadFile(c.path)
	if err != nil {
		t.Fatal(err)
	}
	var saved guardJournal
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	if _, ok := saved.Pins[pin.ID]; ok || saved.Records[prefix].AnswerUntil.Before(start.Add(pin.TTL)) {
		t.Fatal("durable unpin and completion horizon were not committed together")
	}
}

func TestGuardExpiresOnlyUnstartedPins(t *testing.T) {
	c := guardTest(t, 1)
	p := "1.1.1.1/32"
	if err := c.prepare([]string{p}); err != nil {
		t.Fatal(err)
	}
	guardActivate(t, c, p)
	unused, err := c.reserve([]string{p}, 0)
	if err != nil {
		t.Fatal(err)
	}
	started, err := c.reserve([]string{p}, 0)
	if err != nil || !c.beginSend(started.ID) {
		t.Fatal("begin failed", err)
	}
	c.pinDeadline[unused.ID] = time.Now().Add(-time.Second)
	c.pinDeadline[started.ID] = time.Now().Add(-time.Second)
	if err := c.expireUnstartedPins(); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.data.Pins[unused.ID]; ok {
		t.Fatal("unused expired pin was retained")
	}
	if _, ok := c.data.Pins[started.ID]; !ok {
		t.Fatal("in-flight write lost its pin")
	}
}
func TestGuardFullLifecycleAndStaleOps(t *testing.T) {
	c := guardTest(t, 1)
	c.quiet = time.Second
	p := "1.1.1.1/32"
	if e := c.prepare([]string{p}); e != nil {
		t.Fatal(e)
	}
	guardActivate(t, c, p)
	oldGen := c.data.Records[p].Generation
	time.Sleep(12 * time.Millisecond)
	if e := c.retire(p, 0); e != nil {
		t.Fatal(e)
	}
	op := c.pending()[0]
	tampered := op
	tampered.Generation = token()
	pr := guardProof{ExactOwner: true, BarrierPresent: true, GuardPresent: true, BGPActive: true}
	if e := c.ack(tampered, "ok", pr); e == nil {
		t.Fatal("accepted stale generation")
	}
	if e := c.ack(op, "blocked", guardProof{}); e != nil || c.pending()[0] != op {
		t.Fatal("blocked changed intent")
	}
	if e := c.ack(op, "ok", guardProof{}); e == nil {
		t.Fatal("accepted missing proof")
	}
	if e := c.ack(op, "ok", pr); e != nil {
		t.Fatal(e)
	}
	if e := c.ack(op, "ok", pr); e != nil {
		t.Fatal("retry not idempotent", e)
	}
	op = c.pending()[0]
	pr.NoConnections = true
	if e := c.ack(op, "ok", pr); e != nil {
		t.Fatal(e)
	}
	op = c.pending()[0]
	if e := c.ack(op, "ok", pr); e == nil {
		t.Fatal("did not wait quiet interval")
	}
	c.zeroSince[p] = time.Now().Add(-2 * time.Second)
	if e := c.ack(op, "ok", pr); e != nil {
		t.Fatal(e)
	}
	op = c.pending()[0]
	pr.BGPGone = true
	if e := c.ack(op, "ok", pr); e != nil {
		t.Fatal(e)
	}
	op = c.pending()[0]
	pr.GuardAbsent = true
	if e := c.ack(op, "ok", pr); e != nil {
		t.Fatal(e)
	}
	op = c.pending()[0]
	pr.BarrierAbsent = true
	pr.Rechecked = true
	if e := c.ack(op, "ok", pr); e != nil {
		t.Fatal(e)
	}
	if e := c.ack(op, "ok", pr); e != nil {
		t.Fatal("final retry not idempotent", e)
	}
	if e := c.prepare([]string{p}); e != nil {
		t.Fatal(e)
	}
	if c.data.Records[p].Generation == oldGen {
		t.Fatal("generation reused")
	}
	if e := c.ack(op, "ok", pr); e == nil {
		t.Fatal("old removal accepted after re-add")
	}
}

func TestGuardBlockedConntrackInvalidatesZeroObservation(t *testing.T) {
	c := guardTest(t, 1)
	p := "1.1.1.1/32"
	if err := c.prepare([]string{p}); err != nil {
		t.Fatal(err)
	}
	guardActivate(t, c, p)
	time.Sleep(12 * time.Millisecond)
	if err := c.retire(p, 0); err != nil {
		t.Fatal(err)
	}
	proof := guardProof{ExactOwner: true, BarrierPresent: true, GuardPresent: true, BGPActive: true, NoConnections: true}
	if err := c.ack(c.pending()[0], "ok", proof); err != nil {
		t.Fatal(err)
	}
	if err := c.ack(c.pending()[0], "ok", proof); err != nil {
		t.Fatal(err)
	}
	op := c.pending()[0]
	if err := c.ack(op, "blocked", guardProof{}); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.zeroSince[p]; ok {
		t.Fatal("blocked observation retained earlier zero")
	}
	if err := c.ack(op, "ok", proof); err != nil {
		t.Fatal(err)
	}
	if c.pending()[0].Action != "observe_conntrack" {
		t.Fatal("withdrawal after only one new zero observation")
	}
}
func TestGuardConcurrentCapacityAndStorageFailure(t *testing.T) {
	c := guardTest(t, 1)
	var wg sync.WaitGroup
	wg.Add(2)
	errs := make(chan error, 2)
	for _, p := range []string{"1.1.1.1/32", "8.8.8.8/32"} {
		go func(p string) { defer wg.Done(); errs <- c.prepare([]string{p}) }(p)
	}
	wg.Wait()
	close(errs)
	n := 0
	for e := range errs {
		if e == nil {
			n++
		}
	}
	if n != 1 || len(c.data.Records) != 1 {
		t.Fatal("concurrent capacity not atomic")
	}
	c.path = t.TempDir()
	if e := c.finish("does-not-exist"); e == nil {
		t.Fatal("unknown pin accepted")
	}
	p := c.pending()[0].Prefix
	if e := c.activate(*c.data.Records[p].Pending, guardProof{ExactOwner: true, GuardPresent: true, BGPActive: true}); e == nil {
		t.Fatal("storage failure accepted")
	}
	if !c.halted || len(c.pending()) != 0 {
		t.Fatal("storage failure not fail closed")
	}
}

func TestGuardRecoveryAuditNeverUnlocksAndRejectsBadClaims(t *testing.T) {
	c := guardTest(t, 1)
	p := "1.1.1.1/32"
	if err := c.prepare([]string{p}); err != nil {
		t.Fatal(err)
	}
	guardActivate(t, c, p)
	restored, err := newGuardCoordinator(c.path, 1, c.ttlMax, c.sendMargin, c.clockUncertainty, c.quiet, c.retention)
	if err != nil {
		t.Fatal(err)
	}
	r := restored.data.Records[p]
	good := guardRecoverySnapshot{Epoch: restored.data.Epoch, ObjectsComplete: true, OldJobsQuiescent: true, WriterFenced: true, Observations: []guardRecoveryObservation{{Prefix: p, Generation: r.Generation, GuardPresent: true, GuardOwned: true, BGPPresent: true, BGPVerified: true}}}
	if err := restored.auditRecoveryClaims(good); err != nil {
		t.Fatal(err)
	}
	restored.recoveryUntil = time.Now().Add(-time.Second)
	if restored.ready(time.Now()) {
		t.Fatal("read-only audit unlocked recovery")
	}
	if err := restored.prepare([]string{p}); err == nil {
		t.Fatal("audit admitted work")
	}
	cases := []guardRecoverySnapshot{}
	x := good
	x.OldJobsQuiescent = false
	cases = append(cases, x)
	x = good
	x.WriterFenced = false
	cases = append(cases, x)
	x = good
	x.ObjectsComplete = false
	cases = append(cases, x)
	x = good
	x.Epoch = token()
	cases = append(cases, x)
	x = good
	x.Observations = nil
	cases = append(cases, x)
	x = good
	x.Observations = append([]guardRecoveryObservation(nil), good.Observations...)
	x.Observations[0].Generation = token()
	cases = append(cases, x)
	x = good
	x.Observations = append([]guardRecoveryObservation(nil), good.Observations...)
	x.Observations[0].GuardOwned = false
	cases = append(cases, x)
	x = good
	x.Observations = append([]guardRecoveryObservation(nil), good.Observations...)
	x.Observations[0].BGPPresent = false
	cases = append(cases, x)
	x = good
	x.Observations = append([]guardRecoveryObservation(nil), good.Observations...)
	x.Observations[0].BarrierPresent = true
	x.Observations[0].BarrierOwned = true
	cases = append(cases, x)
	x = good
	x.Observations = []guardRecoveryObservation{good.Observations[0], good.Observations[0]}
	cases = append(cases, x)
	for i, bad := range cases {
		if err := restored.auditRecoveryClaims(bad); err == nil {
			t.Errorf("bad claim %d passed", i)
		}
	}
}

func TestGuardRecoveryAuditPendingIdentity(t *testing.T) {
	c := guardTest(t, 1)
	p := "1.1.1.1/32"
	if err := c.prepare([]string{p}); err != nil {
		t.Fatal(err)
	}
	restored, err := newGuardCoordinator(c.path, 1, c.ttlMax, c.sendMargin, c.clockUncertainty, c.quiet, c.retention)
	if err != nil {
		t.Fatal(err)
	}
	r := restored.data.Records[p]
	s := guardRecoverySnapshot{Epoch: restored.data.Epoch, ObjectsComplete: true, OldJobsQuiescent: true, WriterFenced: true, Observations: []guardRecoveryObservation{{Prefix: p, Generation: r.Generation, PendingID: r.Pending.ID}}}
	if err := restored.auditRecoveryClaims(s); err != nil {
		t.Fatal(err)
	}
	s.Observations[0].PendingID = token()
	if err := restored.auditRecoveryClaims(s); err == nil {
		t.Fatal("wrong pending ID accepted")
	}
}

func TestGuardRecoveryAuditPhaseCrashWindows(t *testing.T) {
	cases := []struct {
		phase, action       string
		guard, bgp, barrier bool
		valid               bool
	}{
		{"PREPARING", "create_guard", false, false, false, true},
		{"PREPARING", "create_guard", false, true, false, false},
		{"DRAINING", "install_barrier", true, true, false, true},
		{"DRAINING", "install_barrier", true, true, true, true},
		{"BARRIER", "observe_conntrack", true, false, true, false},
		{"BARRIER", "withdraw_bgp", true, false, true, true},
		{"WITHDRAWN", "remove_guard", true, false, true, true},
		{"WITHDRAWN", "remove_guard", false, false, true, true},
		{"WITHDRAWN", "remove_guard", false, true, true, false},
		{"GUARD_REMOVED", "remove_barrier", false, false, true, true},
		{"GUARD_REMOVED", "remove_barrier", false, false, false, true},
		{"RETIRED", "", false, false, false, true},
		{"RETIRED", "", false, false, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.phase+"/"+tc.action+fmt.Sprint(tc.guard, tc.bgp, tc.barrier), func(t *testing.T) {
			c := guardTest(t, 1)
			p := "1.1.1.1/32"
			gen := token()
			r := guardRecord{Prefix: p, Generation: gen, Phase: tc.phase}
			if tc.action != "" {
				r.Pending = &guardOperation{Version: 1, Epoch: c.data.Epoch, ID: token(), Generation: gen, Prefix: p, Action: tc.action}
			}
			c.data.Records[p] = r
			if err := c.persist(c.data); err != nil {
				t.Fatal(err)
			}
			restored, err := newGuardCoordinator(c.path, 1, c.ttlMax, c.sendMargin, c.clockUncertainty, c.quiet, c.retention)
			if err != nil {
				t.Fatal(err)
			}
			id := ""
			if r.Pending != nil {
				id = r.Pending.ID
			}
			s := guardRecoverySnapshot{Epoch: c.data.Epoch, ObjectsComplete: true, OldJobsQuiescent: true, WriterFenced: true, Observations: []guardRecoveryObservation{{Prefix: p, Generation: gen, PendingID: id, GuardPresent: tc.guard, GuardOwned: tc.guard, BGPPresent: tc.bgp, BGPVerified: tc.bgp, BarrierPresent: tc.barrier, BarrierOwned: tc.barrier}}}
			err = restored.auditRecoveryClaims(s)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v error=%v", tc.valid, err)
			}
			restored.recoveryUntil = time.Now().Add(-time.Second)
			if restored.ready(time.Now()) {
				t.Fatal("claim check unlocked recovery")
			}
		})
	}
}

func TestGuardGCUnavailableDefaultAndMistakenEnable(t *testing.T) {
	a, pub := testApp(t)
	before := len(a.routes)
	cases := []struct{ path, method string }{{"/guardgc/next", "GET"}, {"/guardgc/ack", "POST"}, {"/guardwriter/next", "GET"}, {"/guardwriter/ack", "POST"}}
	for _, tc := range cases {
		h := a.httpHandler()
		r := httptest.NewRequest(tc.method, tc.path, nil)
		r.RemoteAddr = "192.168.88.254:1234"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusNotFound {
			t.Errorf("disabled %s: %d", tc.path, w.Code)
		}
		a.cfg.GuardGCEnabled = true
		r = httptest.NewRequest(tc.method, tc.path, nil)
		r.RemoteAddr = "192.168.88.65:1234"
		w = httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusForbidden {
			t.Errorf("untrusted %s: %d", tc.path, w.Code)
		}
		r = httptest.NewRequest(tc.method, tc.path, nil)
		r.RemoteAddr = "192.168.88.254:1234"
		w = httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusServiceUnavailable {
			t.Errorf("unsupported %s: %d", tc.path, w.Code)
		}
		a.cfg.GuardGCEnabled = false
	}
	if adds, deletes := pub.counts(); adds != 0 || deletes != 0 || len(a.routes) != before {
		t.Fatal("unavailable endpoint changed routes")
	}
	cfg := testConfig(t)
	cfg.GuardGCEnabled = true
	if _, err := newApp(cfg, pub); err == nil {
		t.Fatal("guard GC started before integration")
	}
	// The full entrypoint must reject before starting BGP or even parsing its
	// addresses; malformed addresses prove the enable check runs first.
	cfg.LocalIP = "invalid"
	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	if err := run(path); err == nil || !strings.Contains(err.Error(), "guard_gc_enabled is unavailable") {
		t.Fatalf("entrypoint did not reject before BGP: %v", err)
	}
}
