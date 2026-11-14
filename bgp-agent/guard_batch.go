// SPDX-License-Identifier: GPL-3.0-or-later
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"sort"
	"time"
)

const guardBatchMax = 8

type guardBatch struct {
	Version    int              `json:"version"`
	ID         string           `json:"batch_id"`
	Operations []guardOperation `json:"operations"`
}
type guardBatchResult struct {
	Operation guardOperation `json:"operation"`
	Result    string         `json:"result"`
	Proof     guardProof     `json:"proof"`
}
type guardBatchAck struct {
	Version int                `json:"version"`
	ID      string             `json:"batch_id"`
	Results []guardBatchResult `json:"results"`
}
type guardBatchReceipt struct {
	Ack guardBatchAck `json:"ack"`
}

func guardOperationAssigned(j guardJournal, op guardOperation) bool {
	if j.Writer != nil && *j.Writer == op {
		return true
	}
	if j.Batch != nil {
		for _, x := range j.Batch.Operations {
			if x == op {
				return true
			}
		}
	}
	return false
}
func validateGuardBatchJournal(j guardJournal) error {
	if j.Batch != nil {
		if j.Writer != nil || j.Batch.Version != 1 || !guardID(j.Batch.ID) || len(j.Batch.Operations) < 1 || len(j.Batch.Operations) > guardBatchMax {
			return errors.New("invalid batch assignment")
		}
		seen := map[string]bool{}
		for _, op := range j.Batch.Operations {
			r, ok := j.Records[op.Prefix]
			if !ok || r.Pending == nil || *r.Pending != op || (op.Action != "create_guard" && op.Action != "confirm_bgp") || seen[op.Prefix] {
				return errors.New("invalid batch operation")
			}
			seen[op.Prefix] = true
		}
	}
	if j.LastBatch != nil && (j.LastBatch.Ack.Version != 1 || !guardID(j.LastBatch.Ack.ID) || len(j.LastBatch.Ack.Results) < 1 || len(j.LastBatch.Ack.Results) > guardBatchMax) {
		return errors.New("invalid batch receipt")
	}
	return nil
}
func (c *guardCoordinator) writerBatchNext() (*guardBatch, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.ready(time.Now()) || c.withdrawBusy {
		return nil, errors.New("writer unavailable")
	}
	if c.data.Batch != nil {
		b := *c.data.Batch
		b.Operations = append([]guardOperation(nil), b.Operations...)
		return &b, nil
	}
	if c.data.Writer != nil {
		return nil, nil
	}
	var ops []guardOperation
	for _, r := range c.data.Records {
		if r.Pending != nil && (r.Pending.Action == "create_guard" || r.Pending.Action == "confirm_bgp") {
			ops = append(ops, *r.Pending)
		}
	}
	if len(ops) == 0 {
		return nil, nil
	}
	sort.Slice(ops, func(i, j int) bool { return ops[i].Prefix < ops[j].Prefix })
	// Rotate at each completed assignment so repeatedly blocked prefixes
	// cannot permanently starve later requests.
	start := sort.Search(len(ops), func(i int) bool { return ops[i].Prefix > c.writerCursor })
	if start == len(ops) {
		start = 0
	}
	rotated := append(append([]guardOperation(nil), ops[start:]...), ops[:start]...)
	if len(rotated) > guardBatchMax {
		rotated = rotated[:guardBatchMax]
	}
	batch := guardBatch{Version: 1, ID: token(), Operations: rotated}
	if err := c.change(func(j *guardJournal) error {
		for _, op := range batch.Operations {
			if op.Action == "create_guard" {
				r := j.Records[op.Prefix]
				r.CreateDispatched = true
				j.Records[op.Prefix] = r
			}
		}
		j.Batch = &batch
		return nil
	}); err != nil {
		return nil, err
	}
	c.writerCursor = batch.Operations[len(batch.Operations)-1].Prefix
	return &batch, nil
}
func (c *guardCoordinator) writerBatchAck(ack guardBatchAck) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.ready(time.Now()) {
		return errors.New("recovering")
	}
	if c.data.LastBatch != nil && c.data.LastBatch.Ack.ID == ack.ID {
		a, _ := json.Marshal(c.data.LastBatch.Ack)
		b, _ := json.Marshal(ack)
		if bytes.Equal(a, b) {
			return nil
		}
		return errors.New("batch receipt mismatch")
	}
	batch := c.data.Batch
	if batch == nil || ack.Version != 1 || batch.ID != ack.ID || len(ack.Results) != len(batch.Operations) {
		return errors.New("stale batch")
	}
	for i, x := range ack.Results {
		op := batch.Operations[i]
		if x.Operation != op {
			return errors.New("batch identity mismatch")
		}
		if x.Result == "blocked" {
			continue
		}
		if x.Result != "ok" || !x.Proof.ExactOwner || !x.Proof.GuardPresent || x.Proof.BarrierPresent || (op.Action == "confirm_bgp" && !x.Proof.BGPActive) {
			return errors.New("batch proof incomplete")
		}
	}
	now := time.Now()
	var activated []string
	err := c.change(func(j *guardJournal) error {
		if j.Batch == nil || j.Batch.ID != ack.ID {
			return errors.New("batch changed")
		}
		for _, x := range ack.Results {
			op := x.Operation
			r, ok := j.Records[op.Prefix]
			if !ok || r.Pending == nil || *r.Pending != op {
				return errors.New("batch operation changed")
			}
			if x.Result == "blocked" {
				continue
			}
			if op.Action == "create_guard" {
				if r.Phase != "PREPARING" {
					return errors.New("invalid create phase")
				}
				r.Phase = "GUARDED"
			} else {
				if r.Phase != "GUARDED" {
					return errors.New("invalid confirm phase")
				}
				r.Phase = "ACTIVE"
				r.RetainUntil = now.Add(c.retention)
				activated = append(activated, op.Prefix)
			}
			cp := op
			r.LastOperation = &cp
			r.LastWriter = &cp
			r.Pending = nil
			if op.Action == "create_guard" && c.orphanCandidates[op.Prefix] && c.requestRefs[op.Prefix] == 0 {
				r.Phase = "ORPHAN_REMOVING"
				c.op(j, &r, "remove_orphan_guard")
			}
			j.Records[op.Prefix] = r
		}
		j.LastBatch = &guardBatchReceipt{Ack: ack}
		j.Batch = nil
		return nil
	})
	if err != nil {
		return err
	}
	for _, p := range activated {
		c.retentionMono[p] = now.Add(c.retention)
	}
	return nil
}
