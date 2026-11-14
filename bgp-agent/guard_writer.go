// SPDX-License-Identifier: GPL-3.0-or-later
package main

import (
	"errors"
	"log"
	"sort"
	"time"
)

// writerNext durably assigns exactly one existing operation. An unanswered
// assignment is never replaced or timed out. Router-side serialization and
// authentication remain mandatory: this queue cannot fence a stale ROS job
// that has already received an operation. This is not exposed by the app yet.
func (c *guardCoordinator) writerNext() (*guardOperation, error) {
	lockStarted := time.Now()
	c.mu.Lock()
	lockAcquired := time.Now()
	defer func() {
		c.mu.Unlock()
		wait, hold := lockAcquired.Sub(lockStarted), time.Since(lockAcquired)
		if wait >= 100*time.Millisecond || hold >= 100*time.Millisecond {
			log.Printf("guard writer writerNext lock_wait=%s lock_hold=%s", wait, hold)
		}
	}()
	if !c.ready(time.Now()) {
		return nil, errors.New("recovering")
	}
	if c.withdrawBusy {
		return nil, errors.New("withdrawal in progress")
	}
	if c.data.Batch != nil {
		return nil, errors.New("batch assigned")
	}
	if c.data.Writer != nil {
		op := *c.data.Writer
		return &op, nil
	}
	var pending []guardOperation
	for _, r := range c.data.Records {
		if r.Pending != nil {
			if r.Pending.Action == "withdraw_bgp" && r.Observation != nil {
				pending = append(pending, *r.Observation)
			} else {
				pending = append(pending, *r.Pending)
			}
		}
	}
	if len(pending) == 0 {
		return nil, nil
	}
	priority := func(op guardOperation) int {
		switch op.Action {
		case "create_guard", "confirm_bgp":
			return 0
		case "observe_conntrack":
			return 2
		default:
			return 1
		}
	}
	sort.Slice(pending, func(i, j int) bool {
		if priority(pending[i]) != priority(pending[j]) {
			return priority(pending[i]) < priority(pending[j])
		}
		return pending[i].Prefix < pending[j].Prefix
	})
	// Rotate equally important operations so a persistent connection cannot
	// monopolize the writer by continually requesting a fresh observation.
	op := pending[0]
	for _, candidate := range pending {
		if priority(candidate) != priority(op) {
			break
		}
		if candidate.Prefix > c.writerCursor {
			op = candidate
			break
		}
	}
	// BGP withdrawal is an agent-side effect. Do not ask ROS to observe a
	// withdrawal until a durable local dispatcher has actually executed it.
	if op.Action == "withdraw_bgp" {
		return nil, errors.New("BGP withdrawal dispatcher unavailable")
	}
	if err := c.change(func(j *guardJournal) error {
		if op.Action == "create_guard" {
			r := j.Records[op.Prefix]
			r.CreateDispatched = true
			j.Records[op.Prefix] = r
		}
		copy := op
		j.Writer = &copy
		return nil
	}); err != nil {
		return nil, err
	}
	c.writerCursor = op.Prefix
	return &op, nil
}

// dispatchWithdrawal runs one already-journaled withdrawal behind the
// installed barrier, then persists a distinct observation operation. A
// crash or an uncertain callback result leaves the journal frozen/pending;
// no ROS observation is issued on an uncertain local withdrawal. The caller
// must serialize its publication mutation with all other BGP writers.
func (c *guardCoordinator) dispatchWithdrawal(withdraw func(guardOperation) error) error {
	c.mu.Lock()
	if !c.ready(time.Now()) || c.withdrawBusy || c.data.Writer != nil || c.data.Batch != nil {
		c.mu.Unlock()
		return errors.New("withdrawal unavailable")
	}
	var candidates []guardOperation
	for _, r := range c.data.Records {
		if r.Pending != nil && r.Pending.Action == "withdraw_bgp" && r.Observation == nil {
			candidates = append(candidates, *r.Pending)
		}
	}
	if len(candidates) == 0 {
		c.mu.Unlock()
		return nil
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].Prefix < candidates[j].Prefix })
	op := candidates[0]
	c.withdrawBusy = true
	c.mu.Unlock()
	err := withdraw(op)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.withdrawBusy = false
	if err != nil {
		return err
	}
	if !c.ready(time.Now()) {
		return errors.New("withdrawal completed while frozen")
	}
	return c.change(func(j *guardJournal) error {
		r, ok := j.Records[op.Prefix]
		if !ok || r.Phase != "BARRIER" || r.Pending == nil || *r.Pending != op || r.Observation != nil {
			return errors.New("withdrawal changed")
		}
		r.Observation = &guardOperation{Version: 1, Epoch: op.Epoch, ID: token(), ParentOperationID: op.ID, Generation: op.Generation, Prefix: op.Prefix, Action: "observe_withdrawn"}
		j.Records[op.Prefix] = r
		return nil
	})
}

// writerAck checks the whole durable operation identity. Only a verified
// transition can release the assignment; failed, blocked and stale responses
// cannot advance the queue. Trust in a proof requires external ROS readback.
func (c *guardCoordinator) writerAck(op guardOperation, result string, proof guardProof) error {
	lockStarted := time.Now()
	c.mu.Lock()
	lockAcquired := time.Now()
	defer func() {
		c.mu.Unlock()
		wait, hold := lockAcquired.Sub(lockStarted), time.Since(lockAcquired)
		if wait >= 100*time.Millisecond || hold >= 100*time.Millisecond {
			log.Printf("guard writer writerAck lock_wait=%s lock_hold=%s", wait, hold)
		}
	}()
	if !c.ready(time.Now()) {
		return errors.New("recovering")
	}
	// A reply can be lost after the transition was committed. A duplicate
	// acknowledgement for the exact completed operation is harmless, even if
	// the next operation has already been assigned.
	if r, ok := c.data.Records[op.Prefix]; ok {
		if (r.LastOperation != nil && *r.LastOperation == op) || (r.LastWriter != nil && *r.LastWriter == op) {
			return nil
		}
	}
	if c.data.Writer == nil || *c.data.Writer != op {
		return errors.New("stale writer acknowledgement")
	}
	if result == "blocked" {
		if op.Action != "observe_conntrack" {
			return nil
		}
		delete(c.zeroSince, op.Prefix)
		// The old operation must never be accepted after this release: create a
		// new ID before another prefix can use the writer.
		return c.change(func(j *guardJournal) error {
			r := j.Records[op.Prefix]
			if j.Writer == nil || *j.Writer != op || r.Pending == nil || *r.Pending != op {
				return errors.New("stale observation")
			}
			c.op(j, &r, "observe_conntrack")
			j.Records[op.Prefix] = r
			j.Writer = nil
			return nil
		})
	}
	if result != "ok" {
		return errors.New("invalid writer result")
	}
	if op.Action == "create_guard" || op.Action == "confirm_bgp" {
		return c.activateLocked(op, proof)
	}
	if op.Action == "observe_withdrawn" {
		r := c.data.Records[op.Prefix]
		if r.Pending == nil || r.Pending.Action != "withdraw_bgp" || r.Pending.ID != op.ParentOperationID {
			return errors.New("withdrawal parent mismatch")
		}
		return c.ackLocked(*r.Pending, result, proof)
	}
	return c.ackLocked(op, result, proof)
}
