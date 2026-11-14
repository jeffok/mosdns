// SPDX-License-Identifier: GPL-3.0-or-later
package main

import (
	"errors"
	"sync"
	"testing"
)

func TestGuardWriterSerializesAndPersistsAssignment(t *testing.T) {
	c := guardTest(t, 2)
	if err := c.prepare([]string{"1.1.1.1/32", "8.8.8.8/32"}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	ops := make(chan guardOperation, 20)
	for i := 0; i < cap(ops); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			op, err := c.writerNext()
			if err != nil || op == nil {
				t.Errorf("writerNext: %v", err)
				return
			}
			ops <- *op
		}()
	}
	wg.Wait()
	close(ops)
	var first guardOperation
	for op := range ops {
		if first.ID == "" {
			first = op
		} else if op != first {
			t.Fatal("concurrent requests assigned different operations")
		}
	}
	restored, err := newGuardCoordinator(c.path, 2, c.ttlMax, c.sendMargin, c.clockUncertainty, c.quiet, c.retention)
	if err != nil {
		t.Fatal(err)
	}
	if restored.data.Writer == nil || *restored.data.Writer != first {
		t.Fatal("assignment was not durable")
	}
	if _, err := restored.writerNext(); err == nil {
		t.Fatal("restart allowed writer without external reconciliation")
	}
	wrong := first
	wrong.ID = token()
	proof := guardProof{ExactOwner: true, GuardPresent: true, BGPActive: true}
	if err := c.writerAck(wrong, "ok", proof); err == nil {
		t.Fatal("accepted stale operation")
	}
	if err := c.writerAck(first, "ok", guardProof{}); err == nil {
		t.Fatal("accepted unverified operation")
	}
	again, err := c.writerNext()
	if err != nil || again == nil || *again != first {
		t.Fatal("failed acknowledgement released writer")
	}
	if err := c.writerAck(first, "ok", proof); err != nil {
		t.Fatal(err)
	}
	next, err := c.writerNext()
	if err != nil || next == nil || *next == first {
		t.Fatal("successful acknowledgement did not advance writer")
	}
	if err := c.writerAck(first, "ok", proof); err != nil {
		t.Fatal("lost acknowledgement could not be retried:", err)
	}
	still, err := c.writerNext()
	if err != nil || still == nil || *still != *next {
		t.Fatal("duplicate acknowledgement advanced another operation")
	}
}

func TestGuardWriterWithdrawalNeedsLocalSuccessAndROSAcknowledgement(t *testing.T) {
	c := guardTest(t, 1)
	p := "1.1.1.1/32"
	if err := c.prepare([]string{p}); err != nil {
		t.Fatal(err)
	}
	guardActivate(t, c, p)
	c.mu.Lock()
	err := c.change(func(j *guardJournal) error {
		r := j.Records[p]
		r.Phase = "BARRIER"
		c.op(j, &r, "withdraw_bgp")
		j.Records[p] = r
		return nil
	})
	c.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	parent := *c.data.Records[p].Pending
	if _, err := c.writerNext(); err == nil {
		t.Fatal("exposed observation before withdrawal")
	}
	if err := c.dispatchWithdrawal(func(guardOperation) error { return errors.New("uncertain") }); err == nil {
		t.Fatal("ignored uncertain withdrawal")
	}
	if _, err := c.writerNext(); err == nil {
		t.Fatal("exposed observation after uncertain withdrawal")
	}
	if err := c.dispatchWithdrawal(func(op guardOperation) error {
		if op != parent {
			t.Fatal("wrong withdrawal identity")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	child, err := c.writerNext()
	if err != nil || child == nil || child.Action != "observe_withdrawn" || child.ParentOperationID != parent.ID {
		t.Fatalf("invalid observation: %+v %v", child, err)
	}
	proof := guardProof{ExactOwner: true, BarrierPresent: true, GuardPresent: true, BGPGone: true, NoConnections: true}
	if err := c.writerAck(*child, "ok", proof); err != nil {
		t.Fatal(err)
	}
	if c.data.Records[p].Phase != "WITHDRAWN" {
		t.Fatal("valid observation did not advance withdrawal")
	}
	if err := c.writerAck(*child, "ok", proof); err != nil {
		t.Fatal("lost child acknowledgement was not idempotent:", err)
	}
}
