// SPDX-License-Identifier: GPL-3.0-or-later
package main

import (
	"testing"
)

func TestGuardCancellationUnassignedAndSharedRequests(t *testing.T) {
	a, _ := testApp(t)
	a.guard = guardTest(t, 1)
	p := "8.8.4.4/32"
	if err := a.guard.prepareForRequest([]string{p}); err != nil {
		t.Fatal(err)
	}
	if err := a.guard.prepareForRequest([]string{p}); err != nil {
		t.Fatal(err)
	}
	a.releaseGuardRequest([]string{p})
	if _, ok := a.guard.data.Records[p]; !ok {
		t.Fatal("removed while shared request active")
	}
	a.releaseGuardRequest([]string{p})
	if _, ok := a.guard.data.Records[p]; ok {
		t.Fatal("unassigned create leaked")
	}
	if op, err := a.guard.writerNext(); err != nil || op != nil {
		t.Fatalf("canceled work was issued: %v %v", op, err)
	}
}
func TestGuardCancellationAssignedCreateIsDeletedOnlyAfterProof(t *testing.T) {
	a, _ := testApp(t)
	a.guard = guardTest(t, 1)
	p := "8.8.4.4/32"
	if err := a.guard.prepareForRequest([]string{p}); err != nil {
		t.Fatal(err)
	}
	op, err := a.guard.writerNext()
	if err != nil || op == nil || op.Action != "create_guard" {
		t.Fatalf("create: %v %v", op, err)
	}
	a.releaseGuardRequest([]string{p})
	if r := a.guard.data.Records[p]; r.Phase != "PREPARING" {
		t.Fatal("assigned create canceled before ROS ack")
	}
	if err := a.guard.writerAck(*op, "ok", guardProof{ExactOwner: true, GuardPresent: true}); err != nil {
		t.Fatal(err)
	}
	cleanup, err := a.guard.writerNext()
	if err != nil || cleanup == nil || cleanup.Action != "remove_orphan_guard" || cleanup.Generation != op.Generation {
		t.Fatalf("cleanup: %v %v", cleanup, err)
	}
	if err := a.guard.prepareForRequest([]string{p}); err == nil {
		t.Fatal("admitted while guard cleanup outstanding")
	}
	if err := a.guard.writerAck(*cleanup, "ok", guardProof{ExactOwner: true, GuardAbsent: true}); err == nil {
		t.Fatal("BGP absence not verified")
	}
	if err := a.guard.writerAck(*cleanup, "ok", guardProof{ExactOwner: true, GuardAbsent: true, BGPGone: true}); err != nil {
		t.Fatal(err)
	}
	if r := a.guard.data.Records[p]; r.Phase != "RETIRED" {
		t.Fatalf("phase: %s", r.Phase)
	}
	if err := a.guard.prepareForRequest([]string{p}); err != nil {
		t.Fatal(err)
	}
	if a.guard.data.Records[p].Generation == op.Generation {
		t.Fatal("old generation reused")
	}
}
func TestGuardCancellationDoesNotRemovePublishedRoute(t *testing.T) {
	a, _ := testApp(t)
	a.guard = guardTest(t, 1)
	p := "8.8.4.4/32"
	if err := a.guard.prepareForRequest([]string{p}); err != nil {
		t.Fatal(err)
	}
	guardActivate(t, a.guard, p)
	a.mu.Lock()
	a.routes[p] = &lease{Prefix: p}
	a.mu.Unlock()
	a.releaseGuardRequest([]string{p})
	if r := a.guard.data.Records[p]; r.Phase != "ACTIVE" {
		t.Fatalf("published route touched: %s", r.Phase)
	}
}

func TestGuardWarmPreparationAndConfirmationDoNotRewriteJournal(t *testing.T) {
	c := guardTest(t, 1)
	p := "8.8.4.4/32"
	if err := c.prepare([]string{p}); err != nil {
		t.Fatal(err)
	}
	guardActivate(t, c, p)
	original := c.path
	c.path = t.TempDir() // rename onto a directory fails if a rewrite is attempted
	defer func() { c.path = original }()
	if err := c.prepareForRequest([]string{p}); err != nil {
		t.Fatalf("warm prepare wrote journal: %v", err)
	}
	if err := c.requestBGPConfirmation([]string{p}); err != nil {
		t.Fatalf("warm confirm wrote journal: %v", err)
	}
}

func TestRecoveredOrphanNeverAssumesRouterGuardAbsent(t *testing.T) {
	a, _ := testApp(t)
	a.guard = guardTest(t, 1)
	p := "8.8.4.4/32"
	if err := a.guard.prepare([]string{p}); err != nil {
		t.Fatal(err)
	}
	if err := a.scheduleRecoveredOrphans(); err != nil {
		t.Fatal(err)
	}
	r := a.guard.data.Records[p]
	if r.Phase != "ORPHAN_REMOVING" || r.Pending == nil || r.Pending.Action != "remove_orphan_guard" {
		t.Fatalf("unverified guard discarded: %+v", r)
	}
}
