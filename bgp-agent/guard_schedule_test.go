// SPDX-License-Identifier: GPL-3.0-or-later
package main

import (
	"net/http"
	"path/filepath"
	"testing"
	"time"
)

func TestGuardGCDoesNotWithdrawBeforeRouterProofs(t *testing.T) {
	a, pub := testApp(t)
	a.cfg.GCDryRun = false
	a.guard = guardTest(t, 1)
	prefix := "8.8.8.8/32"
	if err := a.guard.prepare([]string{prefix}); err != nil {
		t.Fatal(err)
	}
	guardActivate(t, a.guard, prefix)
	a.guard.mu.Lock()
	r := a.guard.data.Records[prefix]
	r.RetainUntil = time.Now().Add(-time.Hour)
	r.AnswerUntil = time.Now().Add(-time.Hour)
	a.guard.data.Records[prefix] = r
	a.guard.retentionMono[prefix] = time.Now().Add(-time.Hour)
	a.guard.mu.Unlock()
	a.mu.Lock()
	a.routes[prefix] = &lease{Prefix: prefix, UUID: []byte("route"), RetainUntil: time.Now().Add(-time.Hour), AnswerUntil: time.Now().Add(-time.Hour), References: map[string]time.Time{}}
	a.mu.Unlock()
	a.gc(time.Now())
	if _, n := pub.counts(); n != 0 {
		t.Fatal("GC withdrew before barrier verification")
	}
	op, err := a.guard.writerNext()
	if err != nil || op == nil || op.Action != "install_barrier" {
		t.Fatalf("expected barrier: %v %v", op, err)
	}
	if err := a.dispatchGuardWithdrawal(); err == nil {
		t.Fatal("dispatcher did not respect the outstanding writer")
	}
	if _, n := pub.counts(); n != 0 {
		t.Fatal("dispatcher bypassed barrier")
	}
}

func TestGuardModeRejectsLegacyAcknowledgements(t *testing.T) {
	a, _ := testApp(t)
	a.guard = guardTest(t, 1)
	h := a.httpHandler()
	for _, p := range []string{"/desired", "/ack"} {
		if got := guardRequest(h, http.MethodGet, p, nil, "192.168.88.254:5").Code; got != http.StatusServiceUnavailable {
			t.Fatalf("%s: %d", p, got)
		}
	}
}

func TestGuardWriterBlockedObservationDoesNotStarveNewGuard(t *testing.T) {
	c := guardTest(t, 2)
	a, b := "1.1.1.1/32", "8.8.8.8/32"
	if err := c.prepare([]string{a}); err != nil {
		t.Fatal(err)
	}
	guardActivate(t, c, a)
	c.mu.Lock()
	err := c.change(func(j *guardJournal) error {
		r := j.Records[a]
		r.Phase = "BARRIER"
		c.op(j, &r, "observe_conntrack")
		j.Records[a] = r
		return nil
	})
	c.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	old, err := c.writerNext()
	if err != nil || old == nil || old.Action != "observe_conntrack" {
		t.Fatalf("%v %v", old, err)
	}
	if err := c.prepare([]string{b}); err != nil {
		t.Fatal(err)
	}
	if err := c.writerAck(*old, "blocked", guardProof{}); err != nil {
		t.Fatal(err)
	}
	next, err := c.writerNext()
	if err != nil || next == nil || next.Prefix != b || next.Action != "create_guard" {
		t.Fatalf("new guard starved: %v %v", next, err)
	}
	if err := c.writerAck(*old, "ok", guardProof{ExactOwner: true}); err == nil {
		t.Fatal("stale observation accepted")
	}
}

func TestGuardedSavedLeaseNeverPublishesBeforeRecovery(t *testing.T) {
	a, p := testApp(t)
	a.guard = guardTest(t, 1)
	r := lease{Prefix: "8.8.8.8/32", References: map[string]time.Time{}}
	if err := a.loadSavedLease(r, false); err != nil {
		t.Fatal(err)
	}
	if adds, _ := p.counts(); adds != 0 {
		t.Fatal("saved route published before guard recovery")
	}
	if len(a.routes[r.Prefix].UUID) != 0 {
		t.Fatal("loaded route unexpectedly owns BGP path")
	}
}

func TestGuardWithdrawalRetriesDurabilityAfterSuccessfulDelete(t *testing.T) {
	a, pub := testApp(t)
	a.guard = guardTest(t, 1)
	prefix := "8.8.8.8/32"
	if err := a.guard.prepare([]string{prefix}); err != nil {
		t.Fatal(err)
	}
	guardActivate(t, a.guard, prefix)
	a.mu.Lock()
	a.routes[prefix] = &lease{Prefix: prefix, UUID: []byte("path"), References: map[string]time.Time{}}
	a.mu.Unlock()
	a.guard.mu.Lock()
	err := a.guard.change(func(j *guardJournal) error {
		r := j.Records[prefix]
		r.Phase = "BARRIER"
		a.guard.op(j, &r, "withdraw_bgp")
		j.Records[prefix] = r
		return nil
	})
	a.guard.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	valid := a.cfg.StateFile
	a.cfg.StateFile = t.TempDir()
	if err := a.dispatchGuardWithdrawal(); err == nil {
		t.Fatal("expected persistence failure")
	}
	if _, n := pub.counts(); n != 1 {
		t.Fatalf("deletes: %d", n)
	}
	a.cfg.StateFile = valid
	if err := a.dispatchGuardWithdrawal(); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if _, n := pub.counts(); n != 1 {
		t.Fatalf("repeated path delete: %d", n)
	}
	op, err := a.guard.writerNext()
	if err != nil || op == nil || op.Action != "observe_withdrawn" {
		t.Fatalf("missing ROS observation: %v %v", op, err)
	}
}

func TestGuardTestBuildRequiresExplicitIsolatedTable(t *testing.T) {
	cfg := testConfig(t)
	cfg.GuardGCEnabled = true
	cfg.ConfirmClientIP = "127.0.0.1"
	cfg.GuardTestTable = "rt-bgp-test"
	old := guardTestBuild
	defer func() { guardTestBuild = old }()
	guardTestBuild = ""
	if _, err := newApp(cfg, &fakePublication{}); err == nil {
		t.Fatal("default production binary accepted guard mode")
	}
	guardTestBuild = "1"
	cfg.GuardTestTable = "main"
	if _, err := newApp(cfg, &fakePublication{}); err == nil {
		t.Fatal("test binary accepted main table")
	}
	cfg.GuardTestTable = "rt-bgp-test"
	a, err := newApp(cfg, &fakePublication{})
	if err != nil {
		t.Fatal(err)
	}
	if a.guard == nil || a.recovery == nil {
		t.Fatal("test guard components not initialized")
	}
}

func TestGuardProcessLockExcludesConcurrentAgent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "routes.json")
	first, err := acquireGuardProcessLock(path)
	if err != nil {
		t.Fatal(err)
	}
	if other, err := acquireGuardProcessLock(path); err == nil {
		other.Close()
		t.Fatal("second process lock succeeded")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	next, err := acquireGuardProcessLock(path)
	if err != nil {
		t.Fatal(err)
	}
	next.Close()
}
