// SPDX-License-Identifier: GPL-3.0-or-later
package main

import "log"

// A request holds all prefixes until confirmation and pin reservation complete.
func (c *guardCoordinator) prepareForRequest(prefixes []string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.prepareLocked(prefixes); err != nil {
		return err
	}
	if c.requestRefs == nil {
		c.requestRefs = map[string]int{}
	}
	if c.orphanCandidates == nil {
		c.orphanCandidates = map[string]bool{}
	}
	for _, p := range prefixes {
		c.requestRefs[p]++
		delete(c.orphanCandidates, p)
	}
	return nil
}
func (a *app) releaseGuardRequest(prefixes []string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	c := a.guard
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.requestRefs == nil {
		c.requestRefs = map[string]int{}
	}
	if c.orphanCandidates == nil {
		c.orphanCandidates = map[string]bool{}
	}
	for _, p := range prefixes {
		if c.requestRefs[p] <= 1 {
			delete(c.requestRefs, p)
		} else {
			c.requestRefs[p]--
		}
		if c.requestRefs[p] > 0 || a.routes[p] != nil {
			continue
		}
		r, ok := c.data.Records[p]
		if !ok {
			continue
		}
		switch {
		case r.Phase == "PREPARING" && r.Pending != nil && !guardOperationAssigned(c.data, *r.Pending) && !r.CreateDispatched:
			if err := c.change(func(j *guardJournal) error { delete(j.Records, p); return nil }); err != nil {
				log.Printf("cancel unassigned guard %s: %v", p, err)
			}
		case r.Phase == "PREPARING" && r.Pending != nil && !guardOperationAssigned(c.data, *r.Pending) && r.CreateDispatched:
			// A blocked result can follow a successful ROS side effect. Retain
			// ownership and require a verified idempotent removal.
			if err := c.change(func(j *guardJournal) error {
				r := j.Records[p]
				r.Phase = "ORPHAN_REMOVING"
				c.op(j, &r, "remove_orphan_guard")
				j.Records[p] = r
				return nil
			}); err != nil {
				log.Printf("cancel dispatched guard %s: %v", p, err)
			}
		case r.Phase == "PREPARING":
			c.orphanCandidates[p] = true
		case r.Phase == "GUARDED" && r.Pending == nil:
			if err := c.change(func(j *guardJournal) error {
				r := j.Records[p]
				r.Phase = "ORPHAN_REMOVING"
				c.op(j, &r, "remove_orphan_guard")
				j.Records[p] = r
				return nil
			}); err != nil {
				log.Printf("schedule orphan guard %s: %v", p, err)
			}
		}
	}
}

// A restart loses request references. After router fencing and reconciliation,
// cleanup must use a verified router operation even for an unassigned create:
// an exact guard might already exist due to an interrupted prior effect.
func (a *app) scheduleRecoveredOrphans() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	c := a.guard
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.requestRefs == nil {
		c.requestRefs = map[string]int{}
	}
	if c.orphanCandidates == nil {
		c.orphanCandidates = map[string]bool{}
	}
	var schedule []string
	for p, r := range c.data.Records {
		if c.requestRefs[p] > 0 || a.routes[p] != nil {
			continue
		}
		if r.Phase == "PREPARING" && r.Pending != nil {
			if guardOperationAssigned(c.data, *r.Pending) {
				c.orphanCandidates[p] = true
			} else {
				schedule = append(schedule, p)
			}
		} else if r.Phase == "GUARDED" && r.Pending == nil {
			schedule = append(schedule, p)
		}
	}
	if len(schedule) == 0 {
		return nil
	}
	return c.change(func(j *guardJournal) error {
		for _, p := range schedule {
			r := j.Records[p]
			r.Phase = "ORPHAN_REMOVING"
			c.op(j, &r, "remove_orphan_guard")
			j.Records[p] = r
		}
		return nil
	})
}
