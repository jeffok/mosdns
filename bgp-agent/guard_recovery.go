// SPDX-License-Identifier: GPL-3.0-or-later
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// recoveryFence is a trusted, exclusive router-side recovery session, not an
// HTTP request from an arbitrary peer. Its implementation MUST hold the same
// serialization fence as the normal writer, permanently drain all jobs that
// could have received operations before startup, reject other writer scripts,
// inspect the complete owned namespace (including unexpected objects), and
// keep that fence until Close returns. No production implementation is wired.
// A caller-provided boolean or IP address alone cannot meet this contract.
type recoveryFence interface {
	Snapshot(context.Context, string) (recoveryAttestation, error)
	Verify(context.Context) error
	Commit() error
	Close() error
}

type recoveryAttestation struct {
	Challenge         string
	Snapshot          guardRecoverySnapshot
	UnexpectedObjects int
}

// retryableRecovery admits only journaled operations whose exact identity can
// be retried. Destructive phases also require external barrier and conntrack
// reconciliation. Caller holds c.mu.
func (c *guardCoordinator) retryableRecovery() bool {
	if c.withdrawBusy {
		return false
	}
	if err := validateGuardBatchJournal(c.data); err != nil {
		return false
	}
	for _, r := range c.data.Records {
		switch r.Phase {
		case "ACTIVE", "RETIRED":
			if r.Pending != nil {
				return false
			}
		case "DRAINING":
			if r.Pending == nil || r.Pending.Action != "install_barrier" || r.Observation != nil {
				return false
			}
		case "BARRIER":
			if r.Pending == nil || (r.Pending.Action != "observe_conntrack" && r.Pending.Action != "withdraw_bgp") {
				return false
			}
			if r.Observation != nil && (r.Pending.Action != "withdraw_bgp" || r.Observation.Action != "observe_withdrawn" || r.Observation.ParentOperationID != r.Pending.ID) {
				return false
			}
		case "GUARD_REMOVED":
			if r.Pending == nil || r.Pending.Action != "remove_barrier" || r.Observation != nil {
				return false
			}
		case "ORPHAN_REMOVING":
			if r.Pending == nil || r.Pending.Action != "remove_orphan_guard" || r.Observation != nil || !r.AnswerUntil.IsZero() {
				return false
			}
			for _, pin := range c.data.Pins {
				if pin.Generations[r.Prefix] == r.Generation {
					return false
				}
			}
		case "WITHDRAWN":
			if r.Pending == nil || r.Pending.Action != "remove_guard" {
				return false
			}
		case "PREPARING":
			if r.Pending == nil || r.Pending.Action != "create_guard" {
				return false
			}
		case "GUARDED":
			if r.Pending != nil && r.Pending.Action != "confirm_bgp" {
				return false
			}
		default:
			return false
		}
	}
	if w := c.data.Writer; w != nil {
		r, ok := c.data.Records[w.Prefix]
		if !ok || ((r.Pending == nil || *r.Pending != *w) && (r.Observation == nil || *r.Observation != *w)) {
			return false
		}
		switch w.Action {
		case "create_guard", "confirm_bgp", "remove_orphan_guard", "install_barrier", "observe_conntrack", "observe_withdrawn", "remove_guard", "remove_barrier":
		default:
			return false
		}
	}
	return true
}

// recoverStable reconciles operations after exact readback and fencing.
// Unverified in-flight effects deliberately remain frozen. Expired pins can
// only be dropped after the monotonic restart grace:
// their prior DNS write completion is unknown. This helper is deliberately not
// exposed through an HTTP handler or enabled in production until a real router
// fence, complete namespace enumeration and operational integration are proven.
func (c *guardCoordinator) recoverStable(ctx context.Context, fence recoveryFence) error {
	if fence == nil {
		return errors.New("missing recovery fence")
	}
	closed := false
	defer func() {
		if !closed {
			_ = fence.Close()
		}
	}()
	if err := ctx.Err(); err != nil {
		return err
	}

	c.mu.Lock()
	if c.halted || !c.recoveryNeedsReconcile {
		c.mu.Unlock()
		return errors.New("recovery grace or frozen state")
	}
	if !c.retryableRecovery() {
		c.mu.Unlock()
		return errors.New("destructive operation requires effect reconciliation")
	}
	expected, err := json.Marshal(c.data)
	c.mu.Unlock()
	if err != nil {
		return err
	}

	challenge := token()
	a, err := fence.Snapshot(ctx, challenge)
	if err != nil {
		return fmt.Errorf("recovery snapshot: %w", err)
	}
	if a.Challenge != challenge || a.UnexpectedObjects != 0 {
		return errors.New("stale or incomplete router snapshot")
	}
	if err := c.auditRecoveryClaims(a.Snapshot); err != nil {
		return err
	}
	if err := c.auditRecoveryTraffic(a.Snapshot); err != nil {
		return err
	}
	if err := fence.Verify(ctx); err != nil {
		return fmt.Errorf("recovery fence: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	current, err := json.Marshal(c.data)
	if err != nil {
		return err
	}
	if string(current) != string(expected) || c.halted || !c.recoveryNeedsReconcile {
		return errors.New("journal changed during recovery")
	}
	// Retain recovered pins in the journal until the monotonic restart grace
	// expires. New DNS can safely run while these pins prevent retirement.
	// Do not unlock the coordinator if relinquishing the exclusive fence fails.
	if err := fence.Commit(); err != nil {
		return fmt.Errorf("commit recovery fence: %w", err)
	}
	if err := fence.Close(); err != nil {
		closed = true
		return fmt.Errorf("release recovery fence: %w", err)
	}
	closed = true
	now := time.Now()
	for prefix, r := range c.data.Records {
		if r.Phase == "ACTIVE" {
			c.horizonMono[prefix] = now.Add(c.ttlMax + c.clockUncertainty)
			c.retentionMono[prefix] = now.Add(c.retention)
		}
	}
	c.recoveryNeedsReconcile = false
	return nil
}

// restoreGuardedBGP performs the first of two recovery writer cycles. It
// confirms every journaled ACTIVE prefix has its exact guard before invoking
// publish. The coordinator remains frozen; recoverStable must independently
// verify the resulting BGP state in a second writer cycle before serving DNS.
// Publication failure or an uncertain writer response keeps recovery frozen.
func (c *guardCoordinator) restoreGuardedBGP(ctx context.Context, fence recoveryFence, publish func(context.Context, []string) error) error {
	if fence == nil || publish == nil {
		return errors.New("missing recovery dependency")
	}
	defer fence.Close()
	c.mu.Lock()
	if c.halted || !c.recoveryNeedsReconcile || !c.retryableRecovery() {
		c.mu.Unlock()
		return errors.New("not ready for guarded replay")
	}
	var prefixes []string
	for _, r := range c.data.Records {
		if r.Phase == "ACTIVE" || r.Phase == "DRAINING" || (r.Phase == "GUARDED" && r.Pending != nil && r.Pending.Action == "confirm_bgp") || (r.Phase == "BARRIER" && r.Pending != nil && r.Pending.Action == "observe_conntrack") {
			prefixes = append(prefixes, r.Prefix)
		}
	}
	expected, err := json.Marshal(c.data)
	c.mu.Unlock()
	if err != nil {
		return err
	}
	challenge := token()
	a, err := fence.Snapshot(ctx, challenge)
	if err != nil {
		return err
	}
	if a.Challenge != challenge || a.UnexpectedObjects != 0 {
		return errors.New("stale or incomplete router snapshot")
	}
	// The first pass admits absent BGP, but never absent guard or a present
	// unverified route. Audit all other ownership and completeness rules normally.
	first := a.Snapshot
	first.Observations = append([]guardRecoveryObservation(nil), a.Snapshot.Observations...)
	c.mu.Lock()
	for i := range first.Observations {
		o := &first.Observations[i]
		if r, ok := c.data.Records[o.Prefix]; ok && (r.Phase == "ACTIVE" || r.Phase == "DRAINING" || (r.Phase == "BARRIER" && r.Pending != nil && r.Pending.Action == "observe_conntrack")) && !o.BGPPresent {
			o.BGPPresent = true
			o.BGPVerified = true
		}
	}
	current, marshalErr := json.Marshal(c.data)
	c.mu.Unlock()
	if marshalErr != nil {
		return marshalErr
	}
	if string(current) != string(expected) {
		return errors.New("journal changed")
	}
	if err := c.auditRecoveryClaims(first); err != nil {
		return err
	}
	if err := c.auditRecoveryTraffic(a.Snapshot); err != nil {
		return err
	}
	if err := fence.Verify(ctx); err != nil {
		return err
	}
	// Bound route replay to less than the ROS final POST wait; publisher must
	// honor cancellation so a timed-out job cannot be treated as fenced.
	replayCtx, cancel := context.WithTimeout(ctx, 35*time.Second)
	defer cancel()
	if err := publish(replayCtx, prefixes); err != nil {
		return err
	}
	if err := replayCtx.Err(); err != nil {
		return err
	}
	if err := fence.Verify(ctx); err != nil {
		return err
	}
	return fence.Commit()
}

// auditRecoveryTraffic requires fresh full conntrack evidence for any pending
// destructive effect. A persistent receipt or an absent guard alone cannot
// authorize deletion or prove that traffic has drained.
func (c *guardCoordinator) auditRecoveryTraffic(s guardRecoverySnapshot) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, o := range s.Observations {
		r, ok := c.data.Records[o.Prefix]
		if !ok {
			return errors.New("unknown recovery prefix")
		}
		needsEmpty := r.Phase == "WITHDRAWN" || r.Phase == "GUARD_REMOVED" || (r.Phase == "BARRIER" && r.Pending != nil && r.Pending.Action == "withdraw_bgp")
		if needsEmpty {
			if !o.BarrierPresent || !o.BarrierOwned || !o.ConnectionsComplete || !o.NoConnections {
				delete(c.zeroSince, o.Prefix)
				return errors.New("destructive recovery lacks barrier and full empty conntrack proof")
			}
			now := time.Now()
			if since, ok := c.zeroSince[o.Prefix]; !ok {
				c.zeroSince[o.Prefix] = now
				return errors.New("waiting for fresh conntrack quiet period")
			} else if now.Sub(since) < c.quiet {
				return errors.New("waiting for fresh conntrack quiet period")
			}
		}
		if (r.Phase == "WITHDRAWN" || r.Phase == "GUARD_REMOVED" || (r.Observation != nil)) && o.BGPPresent {
			return errors.New("withdrawn route is still advertised")
		}
		if r.Phase == "GUARD_REMOVED" && o.GuardPresent {
			return errors.New("deleted guard unexpectedly present")
		}
	}
	return nil
}
