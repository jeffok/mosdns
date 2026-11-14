// SPDX-License-Identifier: GPL-3.0-or-later
package main

import (
	"context"
	"errors"
	"testing"
	"time"
)

type fakeRecoveryFence struct {
	snapshot     guardRecoverySnapshot
	unexpected   int
	stale        bool
	failSnapshot bool
	failVerify   bool
	failClose    bool
	closed       bool
}

func (f *fakeRecoveryFence) Snapshot(_ context.Context, challenge string) (recoveryAttestation, error) {
	if f.failSnapshot {
		return recoveryAttestation{}, errors.New("snapshot unavailable")
	}
	if f.stale {
		challenge = "old challenge"
	}
	return recoveryAttestation{Challenge: challenge, Snapshot: f.snapshot, UnexpectedObjects: f.unexpected}, nil
}
func (f *fakeRecoveryFence) Verify(context.Context) error {
	if f.failVerify {
		return errors.New("lost fence")
	}
	return nil
}
func (f *fakeRecoveryFence) Commit() error { return nil }
func (f *fakeRecoveryFence) Close() error {
	f.closed = true
	if f.failClose {
		return errors.New("release failed")
	}
	return nil
}

func recoveryFixture(t *testing.T) (*guardCoordinator, *fakeRecoveryFence, string) {
	t.Helper()
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
	if !c.beginSend(pin.ID) {
		t.Fatal("begin send")
	}
	r, err := newGuardCoordinator(c.path, 1, c.ttlMax, c.sendMargin, c.clockUncertainty, c.quiet, c.retention)
	if err != nil {
		t.Fatal(err)
	}
	record := r.data.Records[p]
	f := &fakeRecoveryFence{snapshot: guardRecoverySnapshot{Epoch: r.data.Epoch, ObjectsComplete: true, OldJobsQuiescent: true, WriterFenced: true, Observations: []guardRecoveryObservation{{Prefix: p, Generation: record.Generation, GuardPresent: true, GuardOwned: true, BGPPresent: true, BGPVerified: true}}}}
	return r, f, p
}
func TestRecoverStableResumesButRetainsRecoveredPinsUntilGrace(t *testing.T) {
	c, f, p := recoveryFixture(t)
	if err := c.recoverStable(context.Background(), f); err != nil {
		t.Fatal(err)
	}
	if !f.closed || !c.ready(time.Now()) || len(c.data.Pins) != 1 {
		t.Fatal("recovery failed or pin prematurely cleared")
	}
	if !c.horizonMono[p].After(time.Now()) || !c.retentionMono[p].After(time.Now()) {
		t.Fatal("monotonic safety margins missing")
	}
	if err := c.expireUnstartedPins(); err != nil {
		t.Fatal(err)
	}
	if len(c.data.Pins) != 1 {
		t.Fatal("recovered pin expired before grace")
	}
	c.recoveryUntil = time.Now().Add(-time.Millisecond)
	if err := c.expireUnstartedPins(); err != nil {
		t.Fatal(err)
	}
	if len(c.data.Pins) != 0 {
		t.Fatal("recovered pin not cleared after grace")
	}
	again, err := newGuardCoordinator(c.path, 1, c.ttlMax, c.sendMargin, c.clockUncertainty, c.quiet, c.retention)
	if err != nil {
		t.Fatal(err)
	}
	if !again.recoveryNeedsReconcile || len(again.data.Pins) != 0 {
		t.Fatal("restart did not refreeze or pin clear was not durable")
	}
}
func TestRecoverStableRejectsStaleIncompleteAndLostFence(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*fakeRecoveryFence)
	}{
		{"old challenge", func(f *fakeRecoveryFence) { f.stale = true }},
		{"unexpected owner", func(f *fakeRecoveryFence) { f.unexpected = 1 }},
		{"snapshot failure", func(f *fakeRecoveryFence) { f.failSnapshot = true }},
		{"lost fence", func(f *fakeRecoveryFence) { f.failVerify = true }},
		{"missing guard", func(f *fakeRecoveryFence) { f.snapshot.Observations[0].GuardPresent = false }},
		{"missing bgp", func(f *fakeRecoveryFence) { f.snapshot.Observations[0].BGPPresent = false }},
		{"old job alive", func(f *fakeRecoveryFence) { f.snapshot.OldJobsQuiescent = false }},
		{"incomplete", func(f *fakeRecoveryFence) { f.snapshot.ObjectsComplete = false }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, f, _ := recoveryFixture(t)
			c.recoveryUntil = time.Now().Add(-time.Millisecond)
			tc.mutate(f)
			if err := c.recoverStable(context.Background(), f); err == nil {
				t.Fatal("invalid recovery succeeded")
			}
			if !f.closed || !c.recoveryNeedsReconcile || len(c.data.Pins) != 1 {
				t.Fatal("failed recovery altered state")
			}
		})
	}
}
func TestRecoverStableDoesNotReconcileInFlightEffects(t *testing.T) {
	c := guardTest(t, 1)
	p := "1.1.1.1/32"
	if err := c.prepare([]string{p}); err != nil {
		t.Fatal(err)
	}
	c, err := newGuardCoordinator(c.path, 1, c.ttlMax, c.sendMargin, c.clockUncertainty, c.quiet, c.retention)
	if err != nil {
		t.Fatal(err)
	}
	c.recoveryUntil = time.Now().Add(-time.Millisecond)
	f := &fakeRecoveryFence{}
	if err := c.recoverStable(context.Background(), f); err == nil {
		t.Fatal("pending operation recovered without proof")
	}
	if !c.recoveryNeedsReconcile || !f.closed {
		t.Fatal("failed recovery unlocked")
	}
}

func TestRecoverStableFenceReleaseFailureKeepsFreeze(t *testing.T) {
	c, f, _ := recoveryFixture(t)
	c.recoveryUntil = time.Now().Add(-time.Millisecond)
	f.failClose = true
	if err := c.recoverStable(context.Background(), f); err == nil {
		t.Fatal("failed release accepted")
	}
	if !c.recoveryNeedsReconcile || c.ready(time.Now()) {
		t.Fatal("failed release unlocked")
	}
}

func TestRestoreGuardedBGPRequiresGuardBeforePublishing(t *testing.T) {
	c, f, _ := recoveryFixture(t)
	c.recoveryUntil = time.Now().Add(-time.Millisecond)
	f.snapshot.Observations[0].BGPPresent = false
	f.snapshot.Observations[0].BGPVerified = false
	f.snapshot.Observations[0].GuardPresent = false
	called := false
	err := c.restoreGuardedBGP(context.Background(), f, func(context.Context, []string) error { called = true; return nil })
	if err == nil || called || !c.recoveryNeedsReconcile {
		t.Fatal("published without protective guard")
	}
}
func TestRestoreGuardedBGPRejectsUnverifiedExistingRoute(t *testing.T) {
	c, f, _ := recoveryFixture(t)
	c.recoveryUntil = time.Now().Add(-time.Millisecond)
	f.snapshot.Observations[0].BGPVerified = false
	called := false
	err := c.restoreGuardedBGP(context.Background(), f, func(context.Context, []string) error { called = true; return nil })
	if err == nil || called {
		t.Fatal("accepted unexpected BGP route")
	}
}

func TestRecoverReplaysExactPendingGuardCreation(t *testing.T) {
	c := guardTest(t, 1)
	prefix := "1.1.1.1/32"
	if err := c.prepare([]string{prefix}); err != nil {
		t.Fatal(err)
	}
	op, err := c.writerNext()
	if err != nil || op == nil {
		t.Fatalf("writer assignment: %v", err)
	}
	c, err = newGuardCoordinator(c.path, 1, c.ttlMax, c.sendMargin, c.clockUncertainty, c.quiet, c.retention)
	if err != nil {
		t.Fatal(err)
	}
	c.recoveryUntil = time.Now().Add(-time.Millisecond)
	record := c.data.Records[prefix]
	snapshot := guardRecoverySnapshot{Epoch: c.data.Epoch, ObjectsComplete: true, OldJobsQuiescent: true, WriterFenced: true, Observations: []guardRecoveryObservation{{Prefix: prefix, Generation: record.Generation, PendingID: op.ID}}}
	f := &fakeRecoveryFence{snapshot: snapshot}
	if err := c.restoreGuardedBGP(context.Background(), f, func(_ context.Context, p []string) error {
		if len(p) != 0 {
			t.Fatalf("published guardless prefix: %v", p)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	f = &fakeRecoveryFence{snapshot: snapshot}
	if err := c.recoverStable(context.Background(), f); err != nil {
		t.Fatal(err)
	}
	retried, err := c.writerNext()
	if err != nil || retried == nil || *retried != *op {
		t.Fatal("did not preserve exact pending operation")
	}
}
func TestRecoverRejectsDestructivePendingOperation(t *testing.T) {
	c, _, prefix := recoveryFixture(t)
	// Isolate a known destructive operation; no router interaction occurs.
	r := c.data.Records[prefix]
	r.Phase = "WITHDRAWN"
	r.Pending = &guardOperation{Version: 1, Epoch: c.data.Epoch, ID: token(), Generation: r.Generation, Prefix: prefix, Action: "remove_guard"}
	c.data.Records[prefix] = r
	c.recoveryUntil = time.Now().Add(-time.Millisecond)
	f := &fakeRecoveryFence{}
	if err := c.recoverStable(context.Background(), f); err == nil {
		t.Fatal("destructive operation accepted without reconciliation")
	}
}

func TestRecoverRemoveGuardAfterLostAckWithBarrierAndConntrackProof(t *testing.T) {
	for _, guardExists := range []bool{true, false} {
		t.Run(map[bool]string{true: "before_delete", false: "after_delete"}[guardExists], func(t *testing.T) {
			c := guardTest(t, 1)
			prefix := "1.1.1.1/32"
			if err := c.prepare([]string{prefix}); err != nil {
				t.Fatal(err)
			}
			guardActivate(t, c, prefix)
			r := c.data.Records[prefix]
			r.Phase = "WITHDRAWN"
			r.Pending = &guardOperation{Version: 1, Epoch: c.data.Epoch, ID: token(), Generation: r.Generation, Prefix: prefix, Action: "remove_guard"}
			op := *r.Pending
			if err := c.change(func(j *guardJournal) error { j.Records[prefix] = r; j.Writer = &op; return nil }); err != nil {
				t.Fatal(err)
			}
			c, err := newGuardCoordinator(c.path, 1, c.ttlMax, c.sendMargin, c.clockUncertainty, c.quiet, c.retention)
			if err != nil {
				t.Fatal(err)
			}
			c.recoveryUntil = time.Now().Add(-time.Millisecond)
			obs := guardRecoveryObservation{Prefix: prefix, Generation: r.Generation, PendingID: op.ID, GuardPresent: guardExists, GuardOwned: guardExists, BarrierPresent: true, BarrierOwned: true, ConnectionsComplete: true, NoConnections: true}
			snap := guardRecoverySnapshot{Epoch: c.data.Epoch, ObjectsComplete: true, OldJobsQuiescent: true, WriterFenced: true, Observations: []guardRecoveryObservation{obs}}
			f := &fakeRecoveryFence{snapshot: snap}
			c.zeroSince[prefix] = time.Now().Add(-c.quiet - time.Second)
			if err := c.restoreGuardedBGP(context.Background(), f, func(_ context.Context, p []string) error {
				if len(p) != 0 {
					t.Fatal("withdrawing prefix republished")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			f = &fakeRecoveryFence{snapshot: snap}
			if err := c.recoverStable(context.Background(), f); err != nil {
				t.Fatal(err)
			}
			retried, err := c.writerNext()
			if err != nil || retried == nil || *retried != op {
				t.Fatal("lost removal operation identity")
			}
		})
	}
}
func TestRecoverRemoveGuardRequiresFullEmptyConntrack(t *testing.T) {
	c, _, prefix := recoveryFixture(t)
	r := c.data.Records[prefix]
	r.Phase = "WITHDRAWN"
	r.Pending = &guardOperation{Version: 1, Epoch: c.data.Epoch, ID: token(), Generation: r.Generation, Prefix: prefix, Action: "remove_guard"}
	c.data.Records[prefix] = r
	c.recoveryUntil = time.Now().Add(-time.Millisecond)
	obs := guardRecoveryObservation{Prefix: prefix, Generation: r.Generation, PendingID: r.Pending.ID, BarrierPresent: true, BarrierOwned: true}
	snap := guardRecoverySnapshot{Epoch: c.data.Epoch, ObjectsComplete: true, OldJobsQuiescent: true, WriterFenced: true, Observations: []guardRecoveryObservation{obs}}
	for _, mode := range []int{0, 1} {
		obs.ConnectionsComplete = mode == 1
		obs.NoConnections = false
		snap.Observations[0] = obs
		f := &fakeRecoveryFence{snapshot: snap}
		if err := c.recoverStable(context.Background(), f); err == nil {
			t.Fatal("missing or active connections permitted")
		}
	}
}

func TestRecoveryDestructivePhasesRequireRebuiltBarrierAndQuiet(t *testing.T) {
	for _, phase := range []struct{ phase, action string }{{"BARRIER", "withdraw_bgp"}, {"WITHDRAWN", "remove_guard"}, {"GUARD_REMOVED", "remove_barrier"}} {
		t.Run(phase.phase, func(t *testing.T) {
			c, _, p := recoveryFixture(t)
			r := c.data.Records[p]
			r.Phase = phase.phase
			r.Pending = &guardOperation{Version: 1, Epoch: c.data.Epoch, ID: token(), Generation: r.Generation, Prefix: p, Action: phase.action}
			c.data.Records[p] = r
			o := guardRecoveryObservation{Prefix: p, Generation: r.Generation, PendingID: r.Pending.ID, ConnectionsComplete: true, NoConnections: true}
			s := guardRecoverySnapshot{Observations: []guardRecoveryObservation{o}}
			if err := c.auditRecoveryTraffic(s); err == nil {
				t.Fatal("missing barrier accepted")
			}
			o.BarrierPresent = true
			o.BarrierOwned = true
			s.Observations[0] = o
			if err := c.auditRecoveryTraffic(s); err == nil {
				t.Fatal("first empty scan bypassed quiet")
			}
			c.zeroSince[p] = time.Now().Add(-c.quiet - time.Second)
			if err := c.auditRecoveryTraffic(s); err != nil {
				t.Fatal(err)
			}
			o.NoConnections = false
			s.Observations[0] = o
			if err := c.auditRecoveryTraffic(s); err == nil {
				t.Fatal("live conntrack accepted")
			}
			if _, ok := c.zeroSince[p]; ok {
				t.Fatal("quiet period not reset after new connection")
			}
		})
	}
}

func TestRecoverOrphanGuardRemovalRetainsOperationIdentity(t *testing.T) {
	for _, present := range []bool{false, true} {
		t.Run(map[bool]string{false: "guard_already_deleted", true: "guard_still_present"}[present], func(t *testing.T) {
			c := guardTest(t, 1)
			prefix := "1.1.1.1/32"
			if err := c.prepare([]string{prefix}); err != nil {
				t.Fatal(err)
			}
			r := c.data.Records[prefix]
			r.Phase = "ORPHAN_REMOVING"
			r.Pending = &guardOperation{Version: 1, Epoch: c.data.Epoch, ID: token(), Generation: r.Generation, Prefix: prefix, Action: "remove_orphan_guard"}
			op := *r.Pending
			if err := c.change(func(j *guardJournal) error { j.Records[prefix] = r; j.Writer = &op; return nil }); err != nil {
				t.Fatal(err)
			}
			c, err := newGuardCoordinator(c.path, 1, c.ttlMax, c.sendMargin, c.clockUncertainty, c.quiet, c.retention)
			if err != nil {
				t.Fatal(err)
			}
			snap := guardRecoverySnapshot{Epoch: c.data.Epoch, ObjectsComplete: true, OldJobsQuiescent: true, WriterFenced: true, Observations: []guardRecoveryObservation{{Prefix: prefix, Generation: r.Generation, PendingID: op.ID, GuardPresent: present, GuardOwned: present}}}
			f := &fakeRecoveryFence{snapshot: snap}
			if err := c.restoreGuardedBGP(context.Background(), f, func(_ context.Context, p []string) error {
				if len(p) != 0 {
					t.Fatal("orphan BGP was replayed")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			f = &fakeRecoveryFence{snapshot: snap}
			if err := c.recoverStable(context.Background(), f); err != nil {
				t.Fatal(err)
			}
			got, err := c.writerNext()
			if err != nil || got == nil || *got != op {
				t.Fatal("pending operation identity lost")
			}
			if err = c.writerAck(op, "ok", guardProof{ExactOwner: true, GuardAbsent: true, BGPGone: true}); err != nil {
				t.Fatal(err)
			}
			if c.data.Records[prefix].Phase != "RETIRED" {
				t.Fatal("orphan not retired")
			}
		})
	}
}
func TestRecoverOrphanRejectsAnyBGPOrBarrierAndSentAnswer(t *testing.T) {
	for _, mode := range []string{"bgp", "barrier", "answer", "pin"} {
		t.Run(mode, func(t *testing.T) {
			c := guardTest(t, 1)
			prefix := "1.1.1.1/32"
			if err := c.prepare([]string{prefix}); err != nil {
				t.Fatal(err)
			}
			r := c.data.Records[prefix]
			r.Phase = "ORPHAN_REMOVING"
			r.Pending = &guardOperation{Version: 1, Epoch: c.data.Epoch, ID: token(), Generation: r.Generation, Prefix: prefix, Action: "remove_orphan_guard"}
			if mode == "answer" {
				r.AnswerUntil = time.Now()
			}
			c.data.Records[prefix] = r
			if mode == "pin" {
				id := token()
				c.data.Pins[id] = guardPin{ID: id, Prefixes: []string{prefix}, Generations: map[string]string{prefix: r.Generation}}
			}
			c.recoveryNeedsReconcile = true
			obs := guardRecoveryObservation{Prefix: prefix, Generation: r.Generation, PendingID: r.Pending.ID, GuardPresent: true, GuardOwned: true}
			if mode == "bgp" {
				obs.BGPPresent = true
				obs.BGPVerified = true
			}
			if mode == "barrier" {
				obs.BarrierPresent = true
				obs.BarrierOwned = true
			}
			f := &fakeRecoveryFence{snapshot: guardRecoverySnapshot{Epoch: c.data.Epoch, ObjectsComplete: true, OldJobsQuiescent: true, WriterFenced: true, Observations: []guardRecoveryObservation{obs}}}
			if err := c.recoverStable(context.Background(), f); err == nil {
				t.Fatal("unsafe orphan recovery allowed")
			}
		})
	}
}
