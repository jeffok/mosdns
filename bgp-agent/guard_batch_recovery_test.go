// SPDX-License-Identifier: GPL-3.0-or-later
package main

import (
	"context"
	"testing"
)

func TestGuardBatchRecoveryBeforeAndAfterAckLoss(t *testing.T) {
	c := guardTest(t, 2)
	if err := c.prepare([]string{"1.1.1.1/32", "1.1.1.2/32"}); err != nil {
		t.Fatal(err)
	}
	batch, err := c.writerBatchNext()
	if err != nil || batch == nil || len(batch.Operations) != 2 {
		t.Fatalf("batch: %v", err)
	}
	saved := *batch
	saved.Operations = append([]guardOperation(nil), batch.Operations...)
	// First operation took effect on ROS, second has not yet run; no ACK reached agent.
	c, err = newGuardCoordinator(c.path, 2, c.ttlMax, c.sendMargin, c.clockUncertainty, c.quiet, c.retention)
	if err != nil {
		t.Fatal(err)
	}
	if b, e := c.writerBatchNext(); e == nil || b != nil {
		t.Fatal("batch escaped before writer fence")
	}
	var obs []guardRecoveryObservation
	for i, op := range saved.Operations {
		obs = append(obs, guardRecoveryObservation{Prefix: op.Prefix, Generation: op.Generation, PendingID: op.ID, GuardPresent: i == 0, GuardOwned: i == 0})
	}
	snap := guardRecoverySnapshot{Epoch: c.data.Epoch, ObjectsComplete: true, OldJobsQuiescent: true, WriterFenced: true, Observations: obs}
	bad := snap
	bad.OldJobsQuiescent = false
	if err = c.restoreGuardedBGP(context.Background(), &fakeRecoveryFence{snapshot: bad}, func(context.Context, []string) error { return nil }); err == nil {
		t.Fatal("old job not fenced")
	}
	if err = c.restoreGuardedBGP(context.Background(), &fakeRecoveryFence{snapshot: snap}, func(_ context.Context, p []string) error {
		if len(p) != 0 {
			t.Fatal("unconfirmed guards published")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err = c.recoverStable(context.Background(), &fakeRecoveryFence{snapshot: snap}); err != nil {
		t.Fatal(err)
	}
	retry, err := c.writerBatchNext()
	if err != nil || retry == nil || retry.ID != saved.ID || len(retry.Operations) != len(saved.Operations) {
		t.Fatal("batch assignment lost")
	}
	for i, op := range retry.Operations {
		if op != saved.Operations[i] {
			t.Fatal("operation identity lost")
		}
	}
	ack := guardBatchAck{Version: 1, ID: saved.ID}
	for _, op := range saved.Operations {
		ack.Results = append(ack.Results, guardBatchResult{Operation: op, Result: "ok", Proof: guardProof{ExactOwner: true, GuardPresent: true}})
	}
	if err = c.writerBatchAck(ack); err != nil {
		t.Fatal(err)
	}
	// ACK was persisted but ROS did not receive HTTP response before reboot.
	c, err = newGuardCoordinator(c.path, 2, c.ttlMax, c.sendMargin, c.clockUncertainty, c.quiet, c.retention)
	if err != nil {
		t.Fatal(err)
	}
	obs = nil
	for _, op := range saved.Operations {
		obs = append(obs, guardRecoveryObservation{Prefix: op.Prefix, Generation: op.Generation, GuardPresent: true, GuardOwned: true})
	}
	snap = guardRecoverySnapshot{Epoch: c.data.Epoch, ObjectsComplete: true, OldJobsQuiescent: true, WriterFenced: true, Observations: obs}
	if err = c.restoreGuardedBGP(context.Background(), &fakeRecoveryFence{snapshot: snap}, func(context.Context, []string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err = c.recoverStable(context.Background(), &fakeRecoveryFence{snapshot: snap}); err != nil {
		t.Fatal(err)
	}
	if err = c.writerBatchAck(ack); err != nil {
		t.Fatalf("persisted receipt did not accept retry: %v", err)
	}
	wrong := ack
	wrong.Results = append([]guardBatchResult(nil), ack.Results...)
	wrong.Results[0].Proof.GuardPresent = false
	if err = c.writerBatchAck(wrong); err == nil {
		t.Fatal("modified receipt accepted")
	}
}

func TestGuardBatchRecoveryRejectsCorruptAssignment(t *testing.T) {
	c := guardTest(t, 1)
	if err := c.prepare([]string{"1.1.1.1/32"}); err != nil {
		t.Fatal(err)
	}
	batch, err := c.writerBatchNext()
	if err != nil {
		t.Fatal(err)
	}
	c.recoveryNeedsReconcile = true
	c.data.Batch.Operations[0].Generation = token()
	f := &fakeRecoveryFence{}
	if err := c.recoverStable(context.Background(), f); err == nil {
		t.Fatal("corrupted batch accepted")
	}
	if c.data.Batch.ID != batch.ID || !c.recoveryNeedsReconcile {
		t.Fatal("invalid batch mutated")
	}
}
