// SPDX-License-Identifier: GPL-3.0-or-later
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/netip"
	"os"
	"testing"
)

func TestGuardBatchAtomicPartialRetryAndReceipt(t *testing.T) {
	c := guardTest(t, 9)
	for i := 1; i <= 9; i++ {
		p := fmt.Sprintf("8.8.4.%d/32", i)
		if err := c.prepareForRequest([]string{p}); err != nil {
			t.Fatal(err)
		}
	}
	batch, err := c.writerBatchNext()
	if err != nil || batch == nil || len(batch.Operations) != 8 {
		t.Fatalf("batch: %+v %v", batch, err)
	}
	again, err := c.writerBatchNext()
	if err != nil || again.ID != batch.ID {
		t.Fatal("unacked batch not replayed")
	}
	if _, err := c.writerNext(); err == nil {
		t.Fatal("single writer ran concurrently")
	}
	ack := guardBatchAck{Version: 1, ID: batch.ID}
	for i, op := range batch.Operations {
		result := "ok"
		proof := guardProof{ExactOwner: true, GuardPresent: true}
		if i == 0 {
			result = "blocked"
			proof = guardProof{}
		}
		ack.Results = append(ack.Results, guardBatchResult{op, result, proof})
	}
	wrong := ack
	wrong.Results = append([]guardBatchResult(nil), ack.Results...)
	wrong.Results[1].Operation.ID = token()
	if err := c.writerBatchAck(wrong); err == nil {
		t.Fatal("accepted wrong identity")
	}
	if err := c.writerBatchAck(ack); err != nil {
		t.Fatal(err)
	}
	if err := c.writerBatchAck(ack); err != nil {
		t.Fatal("lost ack retry", err)
	}
	for i, op := range batch.Operations {
		r := c.data.Records[op.Prefix]
		if i == 0 && r.Phase != "PREPARING" {
			t.Fatal("blocked transitioned")
		}
		if i > 0 && r.Phase != "GUARDED" {
			t.Fatal("success not committed")
		}
	}
	next, err := c.writerBatchNext()
	if err != nil || next == nil || next.ID == batch.ID {
		t.Fatalf("no next batch %v %v", next, err)
	}
}
func TestGuardBatchCancellationAndCrashAssignment(t *testing.T) {
	a, _ := testApp(t)
	a.guard = guardTest(t, 2)
	p := "8.8.4.4/32"
	if err := a.guard.prepareForRequest([]string{p}); err != nil {
		t.Fatal(err)
	}
	batch, err := a.guard.writerBatchNext()
	if err != nil {
		t.Fatal(err)
	}
	a.releaseGuardRequest([]string{p})
	if _, ok := a.guard.data.Records[p]; !ok {
		t.Fatal("removed assigned batch operation")
	}
	ack := guardBatchAck{Version: 1, ID: batch.ID, Results: []guardBatchResult{{Operation: batch.Operations[0], Result: "ok", Proof: guardProof{ExactOwner: true, GuardPresent: true}}}}
	if err := a.guard.writerBatchAck(ack); err != nil {
		t.Fatal(err)
	}
	r := a.guard.data.Records[p]
	if r.Phase != "ORPHAN_REMOVING" {
		t.Fatalf("orphan phase %s", r.Phase)
	}
	b, err := os.ReadFile(a.guard.path)
	if err != nil || len(b) == 0 {
		t.Fatal("journal missing")
	}
}

func TestGuardBatchHTTPPayloadBoundAndRestart(t *testing.T) {
	c := guardTest(t, 8)
	for i := 0; i < 8; i++ {
		p := fmt.Sprintf("223.255.255.%d/32", i)
		if err := c.prepare([]string{p}); err != nil {
			t.Fatal(err)
		}
	}
	h := guardWriterHTTP(c, netip.MustParseAddr("192.168.88.254"))
	w := guardRequest(h, http.MethodGet, "/guardwriter/batch-next", nil, "192.168.88.254:1")
	var b guardBatch
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &b) != nil || len(b.Operations) != 8 {
		t.Fatalf("bad batch: %d %s", w.Code, w.Body.String())
	}
	restored, err := newGuardCoordinator(c.path, 8, c.ttlMax, c.sendMargin, c.clockUncertainty, c.quiet, c.retention)
	if err != nil {
		t.Fatal(err)
	}
	if restored.data.Batch == nil || restored.data.Batch.ID != b.ID {
		t.Fatal("batch not durable on crash")
	}
	ack := guardBatchAck{Version: 1, ID: b.ID}
	for _, op := range b.Operations {
		ack.Results = append(ack.Results, guardBatchResult{Operation: op, Result: "ok", Proof: guardProof{ExactOwner: true, GuardPresent: true}})
	}
	payload, err := json.Marshal(ack)
	if err != nil {
		t.Fatal(err)
	}
	if len(payload) > 3900 {
		t.Fatalf("batch ACK exceeds RouterOS bound: %d", len(payload))
	}
	w = guardRequest(h, http.MethodPost, "/guardwriter/batch-ack", payload, "192.168.88.254:1")
	if w.Code != 204 {
		t.Fatalf("batch ack: %d %s", w.Code, w.Body.String())
	}
	w = guardRequest(h, http.MethodPost, "/guardwriter/batch-ack", payload, "192.168.88.254:1")
	if w.Code != 204 {
		t.Fatalf("lost batch ack retry: %d", w.Code)
	}
}

func TestBlockedBatchRotatesSoLaterRequestsCanProgress(t *testing.T) {
	c := guardTest(t, 9)
	for i := 1; i <= 9; i++ {
		p := fmt.Sprintf("8.8.4.%d/32", i)
		if err := c.prepare([]string{p}); err != nil {
			t.Fatal(err)
		}
	}
	first, err := c.writerBatchNext()
	if err != nil {
		t.Fatal(err)
	}
	ack := guardBatchAck{Version: 1, ID: first.ID}
	seen := map[string]bool{}
	for _, op := range first.Operations {
		seen[op.Prefix] = true
		ack.Results = append(ack.Results, guardBatchResult{Operation: op, Result: "blocked"})
	}
	if err := c.writerBatchAck(ack); err != nil {
		t.Fatal(err)
	}
	next, err := c.writerBatchNext()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, op := range next.Operations {
		if !seen[op.Prefix] {
			found = true
		}
	}
	if !found {
		t.Fatal("repeated blocked batch starved later prefix")
	}
}

func TestBlockedCreateThenCancellationKeepsOrphanForVerifiedRemoval(t *testing.T) {
	a, _ := testApp(t)
	a.guard = guardTest(t, 1)
	p := "8.8.4.4/32"
	if err := a.guard.prepareForRequest([]string{p}); err != nil {
		t.Fatal(err)
	}
	batch, err := a.guard.writerBatchNext()
	if err != nil {
		t.Fatal(err)
	}
	op := batch.Operations[0]
	// ROS may create the guard and then fail readback, returning blocked.
	ack := guardBatchAck{Version: 1, ID: batch.ID, Results: []guardBatchResult{{Operation: op, Result: "blocked"}}}
	if err := a.guard.writerBatchAck(ack); err != nil {
		t.Fatal(err)
	}
	a.releaseGuardRequest([]string{p})
	r, ok := a.guard.data.Records[p]
	if !ok || r.Phase != "ORPHAN_REMOVING" || r.Pending == nil || r.Pending.Action != "remove_orphan_guard" {
		t.Fatalf("uncertain guard discarded: %+v", r)
	}
	restored, err := newGuardCoordinator(a.guard.path, 1, a.guard.ttlMax, a.guard.sendMargin, a.guard.clockUncertainty, a.guard.quiet, a.guard.retention)
	if err != nil {
		t.Fatal(err)
	}
	r = restored.data.Records[p]
	if r.Phase != "ORPHAN_REMOVING" || r.Pending == nil || r.Pending.Generation != op.Generation {
		t.Fatal("cleanup identity lost on restart")
	}
}
func TestLegacyPreparingJournalIsConservativelyDispatchedOnRestart(t *testing.T) {
	a, _ := testApp(t)
	a.guard = guardTest(t, 1)
	p := "8.8.4.4/32"
	if err := a.guard.prepare([]string{p}); err != nil {
		t.Fatal(err)
	}
	restored, err := newGuardCoordinator(a.guard.path, 1, a.guard.ttlMax, a.guard.sendMargin, a.guard.clockUncertainty, a.guard.quiet, a.guard.retention)
	if err != nil {
		t.Fatal(err)
	}
	if !restored.data.Records[p].CreateDispatched {
		t.Fatal("legacy unknown dispatch treated as definitely unexecuted")
	}
}

func TestBlockedBatchSharedRequestAndGenerationFence(t *testing.T) {
	a, _ := testApp(t)
	a.guard = guardTest(t, 1)
	p := "8.8.4.4/32"
	if err := a.guard.prepareForRequest([]string{p}); err != nil {
		t.Fatal(err)
	}
	if err := a.guard.prepareForRequest([]string{p}); err != nil {
		t.Fatal(err)
	}
	batch, err := a.guard.writerBatchNext()
	if err != nil {
		t.Fatal(err)
	}
	old := batch.Operations[0]
	ack := guardBatchAck{Version: 1, ID: batch.ID, Results: []guardBatchResult{{Operation: old, Result: "blocked"}}}
	if err := a.guard.writerBatchAck(ack); err != nil {
		t.Fatal(err)
	}
	a.releaseGuardRequest([]string{p})
	if a.guard.data.Records[p].Phase != "PREPARING" {
		t.Fatal("shared request lost its guard")
	}
	a.releaseGuardRequest([]string{p})
	r := a.guard.data.Records[p]
	if r.Phase != "ORPHAN_REMOVING" {
		t.Fatal("blocked create not scheduled for cleanup")
	}
	if err := a.guard.prepareForRequest([]string{p}); err == nil {
		t.Fatal("new generation admitted before cleanup")
	}
	cleanup, err := a.guard.writerNext()
	if err != nil {
		t.Fatal(err)
	}
	if err := a.guard.writerAck(*cleanup, "ok", guardProof{ExactOwner: true, GuardAbsent: true, BGPGone: true}); err != nil {
		t.Fatal(err)
	}
	if err := a.guard.prepareForRequest([]string{p}); err != nil {
		t.Fatal(err)
	}
	if a.guard.data.Records[p].Generation == old.Generation {
		t.Fatal("reused old generation")
	}
	// The old batch receipt may be retried but cannot mutate the new generation.
	if err := a.guard.writerBatchAck(ack); err != nil {
		t.Fatal(err)
	}
	if a.guard.data.Records[p].Phase != "PREPARING" {
		t.Fatal("old ACK mutated new generation")
	}
}
