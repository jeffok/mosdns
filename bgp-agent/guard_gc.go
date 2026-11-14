// SPDX-License-Identifier: GPL-3.0-or-later
package main

// Experimental, deliberately unwired guard-GC coordinator. It performs no
// router operations; callers must validate exact ownership and dataplane proof.
import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

type guardOperation struct {
	Version           int    `json:"version"`
	Epoch             string `json:"epoch"`
	ID                string `json:"operation_id"`
	ParentOperationID string `json:"parent_operation_id,omitempty"`
	Generation        string `json:"generation"`
	Prefix            string `json:"prefix"`
	Action            string `json:"action"`
}
type guardRecord struct {
	Prefix        string          `json:"prefix"`
	Generation    string          `json:"generation"`
	Phase         string          `json:"phase"`
	AnswerUntil   time.Time       `json:"answer_until"`
	RetainUntil   time.Time       `json:"retain_until"`
	Pending       *guardOperation `json:"pending,omitempty"`
	LastOperation *guardOperation `json:"last_operation,omitempty"`
	Observation   *guardOperation `json:"observation,omitempty"`
	LastWriter    *guardOperation `json:"last_writer,omitempty"`
}
type guardPin struct {
	ID           string            `json:"id"`
	Prefixes     []string          `json:"prefixes"`
	Generations  map[string]string `json:"generations"`
	SendDeadline time.Time         `json:"send_deadline"`
	TTL          time.Duration     `json:"ttl_ns"`
}
type guardJournal struct {
	Version int                    `json:"version"`
	Epoch   string                 `json:"epoch"`
	Records map[string]guardRecord `json:"records"`
	Pins    map[string]guardPin    `json:"pins"`
	Writer  *guardOperation        `json:"writer,omitempty"`
}
type guardProof struct {
	ExactOwner     bool `json:"exact_owner"`
	BarrierPresent bool `json:"barrier_present"`
	BarrierAbsent  bool `json:"barrier_absent"`
	GuardPresent   bool `json:"guard_present"`
	GuardAbsent    bool `json:"guard_absent"`
	BGPActive      bool `json:"bgp_active"`
	BGPGone        bool `json:"bgp_gone"`
	NoConnections  bool `json:"no_connections"`
	Rechecked      bool `json:"rechecked"`
}
type guardCoordinator struct {
	mu                                                     sync.Mutex
	path                                                   string
	capacity                                               int
	ttlMax, sendMargin, clockUncertainty, quiet, retention time.Duration
	data                                                   guardJournal
	recoveryUntil                                          time.Time
	recoveryNeedsReconcile                                 bool
	zeroSince                                              map[string]time.Time
	pinDeadline                                            map[string]time.Time
	pinInFlight                                            map[string]bool
	horizonMono, retentionMono                             map[string]time.Time
	halted                                                 bool
	withdrawBusy                                           bool
}

func newGuardCoordinator(path string, capacity int, ttl, margin, uncertainty, quiet, retention time.Duration) (*guardCoordinator, error) {
	if path == "" || capacity < 1 || ttl <= 0 || ttl > 300*time.Second || margin <= 0 || uncertainty < 0 || quiet <= 0 || retention <= 0 {
		return nil, errors.New("invalid configuration")
	}
	c := &guardCoordinator{path: path, capacity: capacity, ttlMax: ttl, sendMargin: margin, clockUncertainty: uncertainty, quiet: quiet, retention: retention, zeroSince: map[string]time.Time{}, pinDeadline: map[string]time.Time{}, pinInFlight: map[string]bool{}, horizonMono: map[string]time.Time{}, retentionMono: map[string]time.Time{}}
	b, e := os.ReadFile(path)
	if os.IsNotExist(e) {
		c.data = guardJournal{Version: 1, Epoch: token(), Records: map[string]guardRecord{}, Pins: map[string]guardPin{}}
		if e = c.persist(c.data); e != nil {
			return nil, e
		}
		return c, nil
	}
	if e != nil {
		return nil, e
	}
	if e = json.Unmarshal(b, &c.data); e != nil {
		return nil, e
	}
	if c.data.Version != 1 || !guardID(c.data.Epoch) || c.data.Records == nil || c.data.Pins == nil {
		return nil, errors.New("invalid journal")
	}
	occupied := 0
	for k, r := range c.data.Records {
		p, e := netip.ParsePrefix(k)
		if e != nil || !p.Addr().Is4() || p.Bits() != 32 || p.String() != k || r.Prefix != k || !guardID(r.Generation) {
			return nil, errors.New("invalid record")
		}
		switch r.Phase {
		case "PREPARING", "ACTIVE", "DRAINING", "BARRIER", "WITHDRAWN", "GUARD_REMOVED", "RETIRED":
		default:
			return nil, errors.New("invalid phase")
		}
		if r.Phase != "RETIRED" {
			occupied++
		}
		if r.Pending != nil && (r.Pending.Version != 1 || r.Pending.Epoch != c.data.Epoch || r.Pending.Generation != r.Generation || r.Pending.Prefix != k || !guardID(r.Pending.ID)) {
			return nil, errors.New("invalid operation")
		}
		if r.LastOperation != nil && (r.LastOperation.Version != 1 || r.LastOperation.Epoch != c.data.Epoch || r.LastOperation.Generation != r.Generation || r.LastOperation.Prefix != k || !guardID(r.LastOperation.ID)) {
			return nil, errors.New("invalid completed operation")
		}
		if r.Observation != nil && (r.Pending == nil || r.Pending.Action != "withdraw_bgp" || r.Observation.Version != 1 || r.Observation.Epoch != c.data.Epoch || r.Observation.Generation != r.Generation || r.Observation.Prefix != k || r.Observation.Action != "observe_withdrawn" || r.Observation.ParentOperationID != r.Pending.ID || !guardID(r.Observation.ID)) {
			return nil, errors.New("invalid withdrawal observation")
		}
		if (r.Phase == "ACTIVE" || r.Phase == "RETIRED") && r.Pending != nil {
			return nil, errors.New("unexpected pending operation")
		}
		if r.Phase != "ACTIVE" && r.Phase != "RETIRED" {
			if r.Pending == nil {
				return nil, errors.New("missing pending operation")
			}
			action := r.Pending.Action
			valid := (r.Phase == "PREPARING" && action == "create_guard") || (r.Phase == "DRAINING" && action == "install_barrier") || (r.Phase == "BARRIER" && (action == "observe_conntrack" || action == "withdraw_bgp")) || (r.Phase == "WITHDRAWN" && action == "remove_guard") || (r.Phase == "GUARD_REMOVED" && action == "remove_barrier")
			if !valid {
				return nil, errors.New("operation phase mismatch")
			}
		}
	}
	if occupied > capacity {
		return nil, errors.New("journal exceeds capacity")
	}
	if w := c.data.Writer; w != nil {
		r, ok := c.data.Records[w.Prefix]
		if !ok || (r.Pending == nil || *r.Pending != *w) && (r.Observation == nil || *r.Observation != *w) {
			return nil, errors.New("writer operation mismatch")
		}
	}
	for id, p := range c.data.Pins {
		if id != p.ID || !guardID(id) || len(p.Prefixes) == 0 {
			return nil, errors.New("invalid pin")
		}
		for _, k := range p.Prefixes {
			r, ok := c.data.Records[k]
			if !ok || p.Generations[k] != r.Generation {
				return nil, errors.New("pin mismatch")
			}
		}
	}
	grace := ttl + margin + uncertainty
	if retention > grace {
		grace = retention
	}
	c.recoveryUntil = time.Now().Add(grace)
	// Wall-time grace alone cannot establish that ROS still has the exact
	// guards, barriers and BGP routes after a reboot. No unsafe unlock exists;
	// a future integration must implement and test full external reconciliation.
	c.recoveryNeedsReconcile = len(c.data.Records) != 0 || len(c.data.Pins) != 0
	return c, nil
}
func guardID(s string) bool {
	b, err := hex.DecodeString(s)
	return len(s) == 24 && err == nil && len(b) == 12 && hex.EncodeToString(b) == s
}
func (c *guardCoordinator) persist(j guardJournal) error {
	b, e := json.Marshal(j)
	if e != nil {
		return e
	}
	dir := filepath.Dir(c.path)
	if e = os.MkdirAll(dir, 0700); e != nil {
		return e
	}
	f, e := os.CreateTemp(dir, ".guard-gc-")
	if e != nil {
		return e
	}
	name := f.Name()
	defer os.Remove(name)
	if e = f.Chmod(0600); e == nil {
		_, e = f.Write(b)
	}
	if e == nil {
		e = f.Sync()
	}
	if x := f.Close(); e == nil {
		e = x
	}
	if e != nil {
		return e
	}
	if e = os.Rename(name, c.path); e != nil {
		return e
	}
	d, e := os.Open(dir)
	if e != nil {
		return e
	}
	e = d.Sync()
	if x := d.Close(); e == nil {
		e = x
	}
	return e
}
func (c *guardCoordinator) change(fn func(*guardJournal) error) error {
	if c.halted {
		return errors.New("coordinator halted")
	}
	b, e := json.Marshal(c.data)
	if e != nil {
		return e
	}
	var next guardJournal
	if e = json.Unmarshal(b, &next); e != nil {
		return e
	}
	if e = fn(&next); e != nil {
		return e
	}
	if e = c.persist(next); e != nil {
		c.halted = true
		return fmt.Errorf("journal: %w", e)
	}
	c.data = next
	return nil
}
func (c *guardCoordinator) ready(now time.Time) bool {
	return !c.halted && !c.recoveryNeedsReconcile && !now.Before(c.recoveryUntil)
}
func guardPrefixes(in []string) ([]string, error) {
	if len(in) == 0 {
		return nil, errors.New("empty prefixes")
	}
	s := map[string]bool{}
	for _, k := range in {
		p, e := netip.ParsePrefix(k)
		if e != nil || !p.Addr().Is4() || p.Bits() != 32 || p.String() != k {
			return nil, errors.New("invalid prefix")
		}
		s[k] = true
	}
	a := make([]string, 0, len(s))
	for k := range s {
		a = append(a, k)
	}
	sort.Strings(a)
	return a, nil
}
func (c *guardCoordinator) prepare(in []string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.ready(time.Now()) {
		return errors.New("recovering")
	}
	ps, e := guardPrefixes(in)
	if e != nil {
		return e
	}
	return c.change(func(j *guardJournal) error {
		n, occupied := 0, 0
		for _, r := range j.Records {
			if r.Phase != "RETIRED" {
				occupied++
			}
		}
		for _, p := range ps {
			r, ok := j.Records[p]
			if !ok || r.Phase == "RETIRED" {
				n++
			} else if r.Phase != "ACTIVE" && r.Phase != "PREPARING" {
				return errors.New("draining")
			}
		}
		if occupied+n > c.capacity {
			return errors.New("capacity")
		}
		for _, p := range ps {
			if r, ok := j.Records[p]; !ok || r.Phase == "RETIRED" {
				r := guardRecord{Prefix: p, Generation: token(), Phase: "PREPARING"}
				c.op(j, &r, "create_guard")
				j.Records[p] = r
			}
		}
		return nil
	})
}
func (c *guardCoordinator) activate(op guardOperation, proof guardProof) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.activateLocked(op, proof)
}
func (c *guardCoordinator) activateLocked(op guardOperation, proof guardProof) error {
	now := time.Now()
	if !c.ready(now) {
		return errors.New("recovering")
	}
	if r, ok := c.data.Records[op.Prefix]; ok && r.Phase == "ACTIVE" && r.LastOperation != nil && *r.LastOperation == op {
		return nil
	}
	err := c.change(func(j *guardJournal) error {
		r, ok := j.Records[op.Prefix]
		if !ok || r.Phase != "PREPARING" || r.Pending == nil || *r.Pending != op || op.Action != "create_guard" || !proof.ExactOwner || !proof.GuardPresent || !proof.BGPActive || proof.BarrierPresent {
			return errors.New("activation unverified")
		}
		r.Phase = "ACTIVE"
		r.RetainUntil = now.Add(c.retention)
		r.LastOperation = r.Pending
		r.Pending = nil
		if j.Writer != nil && *j.Writer == op {
			r.LastWriter = j.Writer
			j.Writer = nil
		}
		j.Records[op.Prefix] = r
		return nil
	})
	if err == nil {
		c.retentionMono[op.Prefix] = now.Add(c.retention)
	}
	return err
}

// The caller must keep every pin until DNS write finishes or is abandoned.
func (c *guardCoordinator) reserve(in []string, ttl time.Duration) (guardPin, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if !c.ready(now) || ttl < 0 || ttl > c.ttlMax {
		return guardPin{}, errors.New("not ready or invalid TTL")
	}
	ps, e := guardPrefixes(in)
	if e != nil {
		return guardPin{}, e
	}
	p := guardPin{ID: token(), Prefixes: ps, Generations: map[string]string{}, SendDeadline: now.Add(c.sendMargin), TTL: ttl}
	e = c.change(func(j *guardJournal) error {
		for _, k := range ps {
			r, ok := j.Records[k]
			if !ok || r.Phase != "ACTIVE" || r.Pending != nil {
				return errors.New("not active")
			}
			p.Generations[k] = r.Generation
			until := now.Add(ttl + c.sendMargin)
			if until.After(r.AnswerUntil) {
				r.AnswerUntil = until
			}
			if until := now.Add(c.retention); until.After(r.RetainUntil) {
				r.RetainUntil = until
			}
			j.Records[k] = r
		}
		j.Pins[p.ID] = p
		return nil
	})
	if e == nil {
		c.pinDeadline[p.ID] = now.Add(c.sendMargin)
		for _, k := range ps {
			if h := now.Add(ttl + c.sendMargin); h.After(c.horizonMono[k]) {
				c.horizonMono[k] = h
			}
			if h := now.Add(c.retention); h.After(c.retentionMono[k]) {
				c.retentionMono[k] = h
			}
		}
	}
	return p, e
}
func (c *guardCoordinator) maySend(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.data.Pins[id]
	deadline, local := c.pinDeadline[id]
	now := time.Now()
	return ok && local && c.ready(now) && !now.After(deadline)
}

// beginSend consumes the one send admission associated with a pin. Reusing a
// pin for concurrent writes could let the first completion release the second.
func (c *guardCoordinator) beginSend(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.data.Pins[id]
	deadline, local := c.pinDeadline[id]
	now := time.Now()
	if !ok || !local || !c.ready(now) || now.After(deadline) || c.pinInFlight[id] {
		return false
	}
	c.pinInFlight[id] = true
	return true
}

// expireUnstartedPins releases only locally known pins whose one-time send
// admission has expired and was never consumed. Lost /confirm replies would
// otherwise consume capacity forever. An admitted or recovered pin is never
// reaped by this method because its write outcome can be unknown.
func (c *guardCoordinator) expireUnstartedPins() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if !c.ready(now) {
		return errors.New("recovering")
	}
	var expired []string
	for id, deadline := range c.pinDeadline {
		if !c.pinInFlight[id] && now.After(deadline) {
			expired = append(expired, id)
		}
	}
	if len(expired) == 0 {
		return nil
	}
	if err := c.change(func(j *guardJournal) error {
		for _, id := range expired {
			delete(j.Pins, id)
		}
		return nil
	}); err != nil {
		return err
	}
	for _, id := range expired {
		delete(c.pinDeadline, id)
	}
	return nil
}

// finish is only for an answer whose transmission was definitely abandoned.
// A successful or possibly successful write must use finishSent instead.
func (c *guardCoordinator) finish(id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.ready(time.Now()) {
		return errors.New("recovering")
	}
	err := c.change(func(j *guardJournal) error {
		if _, ok := j.Pins[id]; !ok {
			return errors.New("unknown pin")
		}
		delete(j.Pins, id)
		return nil
	})
	if err == nil {
		delete(c.pinDeadline, id)
		delete(c.pinInFlight, id)
	}
	return err
}

// finishSent is called only after the actual DNS write has returned. Keep the
// pin while the write is in progress, even past its admission deadline. The
// deadline prevents starting a late send; it does not bound scheduling or a
// blocked write. Extend the horizon from completion before releasing the pin,
// so neither delay can shorten the client's advertised TTL. Unknown write
// outcomes are treated as sent. If persistence fails, change halts the
// coordinator and the original durable pin remains for recovery.
func (c *guardCoordinator) finishSent(id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if !c.ready(now) {
		return errors.New("recovering")
	}
	pin, ok := c.data.Pins[id]
	if !ok || !c.pinInFlight[id] || pin.TTL < 0 || pin.TTL > c.ttlMax {
		return errors.New("unknown pin or invalid TTL")
	}
	until := now.Add(pin.TTL + c.sendMargin + c.clockUncertainty)
	err := c.change(func(j *guardJournal) error {
		p, ok := j.Pins[id]
		if !ok {
			return errors.New("unknown pin")
		}
		for _, prefix := range p.Prefixes {
			r, ok := j.Records[prefix]
			if !ok || r.Generation != p.Generations[prefix] || r.Phase != "ACTIVE" {
				return errors.New("pin generation changed")
			}
			if until.After(r.AnswerUntil) {
				r.AnswerUntil = until
			}
			j.Records[prefix] = r
		}
		delete(j.Pins, id)
		return nil
	})
	if err != nil {
		return err
	}
	delete(c.pinDeadline, id)
	delete(c.pinInFlight, id)
	for _, prefix := range pin.Prefixes {
		if until.After(c.horizonMono[prefix]) {
			c.horizonMono[prefix] = until
		}
	}
	return nil
}
func (c *guardCoordinator) op(j *guardJournal, r *guardRecord, action string) {
	r.Pending = &guardOperation{Version: 1, Epoch: j.Epoch, ID: token(), Generation: r.Generation, Prefix: r.Prefix, Action: action}
}
func (c *guardCoordinator) retire(prefix string, safety time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if !c.ready(now) || safety < 0 || now.Before(c.horizonMono[prefix].Add(safety)) || now.Before(c.retentionMono[prefix]) {
		return errors.New("not ready")
	}
	return c.change(func(j *guardJournal) error {
		r, ok := j.Records[prefix]
		if !ok || r.Phase != "ACTIVE" || r.Pending != nil || now.Before(r.RetainUntil) || now.Before(r.AnswerUntil.Add(safety)) {
			return errors.New("not eligible")
		}
		for _, p := range j.Pins {
			if p.Generations[prefix] == r.Generation {
				return errors.New("pinned")
			}
		}
		r.Phase = "DRAINING"
		c.op(j, &r, "install_barrier")
		j.Records[prefix] = r
		return nil
	})
}
func (c *guardCoordinator) pending() []guardOperation {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.ready(time.Now()) {
		return nil
	}
	var a []guardOperation
	for _, r := range c.data.Records {
		if r.Pending != nil {
			a = append(a, *r.Pending)
		}
	}
	sort.Slice(a, func(i, j int) bool { return a[i].Prefix < a[j].Prefix })
	return a
}

func (c *guardCoordinator) allActive(prefixes []string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.ready(time.Now()) {
		return false
	}
	for _, prefix := range prefixes {
		r, ok := c.data.Records[prefix]
		if !ok || r.Phase != "ACTIVE" || r.Pending != nil {
			return false
		}
	}
	return len(prefixes) != 0
}

// ack performs no external mutations; blocked or uncertain results retain the
// operation, guard, and barrier. proof is only trusted after real ROS readback.
func (c *guardCoordinator) ack(op guardOperation, result string, p guardProof) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ackLocked(op, result, p)
}
func (c *guardCoordinator) ackLocked(op guardOperation, result string, p guardProof) error {
	now := time.Now()
	if !c.ready(now) {
		return errors.New("recovering")
	}
	r, ok := c.data.Records[op.Prefix]
	if !ok {
		return errors.New("unknown prefix")
	}
	if r.LastOperation != nil && *r.LastOperation == op {
		return nil
	}
	if r.Pending == nil || *r.Pending != op {
		return errors.New("stale operation")
	}
	if result == "blocked" {
		if op.Action == "observe_conntrack" {
			delete(c.zeroSince, op.Prefix)
		}
		return nil
	}
	if result != "ok" || !p.ExactOwner {
		if op.Action == "observe_conntrack" {
			delete(c.zeroSince, op.Prefix)
		}
		return errors.New("unverified")
	}
	first, clear := false, false
	e := c.change(func(j *guardJournal) error {
		r := j.Records[op.Prefix]
		switch op.Action {
		case "install_barrier":
			if r.Phase != "DRAINING" || !p.BarrierPresent || !p.GuardPresent || !p.BGPActive {
				return errors.New("unverified barrier")
			}
			r.Phase = "BARRIER"
			c.op(j, &r, "observe_conntrack")
		case "observe_conntrack":
			if r.Phase != "BARRIER" || !p.BarrierPresent || !p.GuardPresent || !p.BGPActive {
				return errors.New("unverified drain")
			}
			if !p.NoConnections {
				clear = true
				c.op(j, &r, "observe_conntrack")
			} else if t, ok := c.zeroSince[r.Prefix]; !ok {
				first = true
				c.op(j, &r, "observe_conntrack")
			} else if now.Sub(t) < c.quiet {
				return errors.New("quiescence pending")
			} else {
				clear = true
				c.op(j, &r, "withdraw_bgp")
			}
		case "withdraw_bgp":
			if r.Phase != "BARRIER" || !p.BarrierPresent || !p.GuardPresent || !p.BGPGone || !p.NoConnections {
				return errors.New("unverified withdrawal")
			}
			r.Phase = "WITHDRAWN"
			c.op(j, &r, "remove_guard")
		case "remove_guard":
			if r.Phase != "WITHDRAWN" || !p.BarrierPresent || !p.GuardAbsent || !p.BGPGone || !p.NoConnections {
				return errors.New("unverified guard")
			}
			r.Phase = "GUARD_REMOVED"
			c.op(j, &r, "remove_barrier")
		case "remove_barrier":
			if r.Phase != "GUARD_REMOVED" || !p.BarrierAbsent || !p.GuardAbsent || !p.BGPGone || !p.NoConnections || !p.Rechecked || now.Before(r.AnswerUntil) {
				return errors.New("unverified removal")
			}
			r.Phase = "RETIRED"
			r.Pending = nil
		default:
			return errors.New("unknown action")
		}
		r.LastOperation = &op
		if j.Writer != nil && (*j.Writer == op || (j.Writer.Action == "observe_withdrawn" && j.Writer.ParentOperationID == op.ID)) {
			r.LastWriter = j.Writer
			r.Observation = nil
			j.Writer = nil
		}
		j.Records[r.Prefix] = r
		return nil
	})
	if e == nil {
		if first {
			c.zeroSince[op.Prefix] = now
		}
		if clear {
			delete(c.zeroSince, op.Prefix)
		}
	} else if op.Action == "observe_conntrack" && (!p.BarrierPresent || !p.GuardPresent || !p.BGPActive) {
		delete(c.zeroSince, op.Prefix)
	}
	return e
}

// guardRecoveryObservation is an untrusted read-back claim. These fields do not
// authenticate a ROS host, fence another writer, prove flash persistence, or
// prove packet-path quiescence. In particular, a passing audit is NEVER an
// authorization to unlock recovery or execute the pending operation.
type guardRecoveryObservation struct {
	Prefix, Generation, PendingID string
	GuardPresent, GuardOwned      bool
	BGPPresent, BGPVerified       bool
	BarrierPresent, BarrierOwned  bool
}
type guardRecoverySnapshot struct {
	Epoch            string
	ObjectsComplete  bool
	OldJobsQuiescent bool
	WriterFenced     bool
	Observations     []guardRecoveryObservation
}

// auditRecoveryClaims compares caller-supplied observations against the durable
// journal without mutating it. It accepts the possible before/after states of a
// single already-journaled operation. Identity, source authenticity, old-job
// termination and exclusive ownership remain external preconditions. This is a
// consistency filter, not a recovery protocol and not a production unlock.
func (c *guardCoordinator) auditRecoveryClaims(s guardRecoverySnapshot) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.halted || !c.recoveryNeedsReconcile {
		return errors.New("no auditable frozen recovery")
	}
	if s.Epoch != c.data.Epoch || !s.ObjectsComplete || !s.OldJobsQuiescent || !s.WriterFenced {
		return errors.New("incomplete recovery claims")
	}
	if len(s.Observations) != len(c.data.Records) {
		return errors.New("incomplete observation set")
	}
	seen := make(map[string]bool, len(s.Observations))
	for _, o := range s.Observations {
		if seen[o.Prefix] {
			return errors.New("duplicate prefix observation")
		}
		seen[o.Prefix] = true
		r, ok := c.data.Records[o.Prefix]
		if !ok || r.Generation != o.Generation {
			return errors.New("unknown prefix or generation")
		}
		wantID := ""
		if r.Pending != nil {
			wantID = r.Pending.ID
		}
		if o.PendingID != wantID {
			return errors.New("pending operation identity mismatch")
		}
		if (o.GuardPresent && !o.GuardOwned) || (o.BarrierPresent && !o.BarrierOwned) || (o.BGPPresent && !o.BGPVerified) {
			return errors.New("ambiguous ownership or route")
		}
		switch r.Phase {
		case "PREPARING":
			// Creation may have stopped before the guard, or after guard+BGP setup.
			if o.BarrierPresent || (o.BGPPresent && !o.GuardPresent) {
				return errors.New("unsafe preparation state")
			}
		case "ACTIVE":
			if !o.GuardPresent || !o.BGPPresent || o.BarrierPresent {
				return errors.New("active state mismatch")
			}
		case "DRAINING":
			// The journaled install may or may not have taken effect before the crash.
			if !o.GuardPresent || !o.BGPPresent {
				return errors.New("draining state mismatch")
			}
		case "BARRIER":
			if !o.BarrierPresent || !o.GuardPresent || (r.Pending.Action != "withdraw_bgp" && !o.BGPPresent) {
				return errors.New("barrier state mismatch")
			}
		case "WITHDRAWN":
			// The journaled guard removal may have happened before the last ACK.
			if !o.BarrierPresent || o.BGPPresent {
				return errors.New("withdrawn state mismatch")
			}
		case "GUARD_REMOVED":
			// An absent barrier here requires explicit effect reconciliation; this
			// method cannot infer whether the pre-removal conntrack check was valid.
			if o.GuardPresent || o.BGPPresent {
				return errors.New("guard-removed state mismatch")
			}
		case "RETIRED":
			if o.GuardPresent || o.BGPPresent || o.BarrierPresent {
				return errors.New("retired state mismatch")
			}
		default:
			return errors.New("unknown journal phase")
		}
	}
	return nil
}
