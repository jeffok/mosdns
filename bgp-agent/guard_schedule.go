// SPDX-License-Identifier: GPL-3.0-or-later
package main

import (
	"log"
	"time"
)

// guardGC never directly withdraws a path. It journals barrier installation
// while holding the same publication lock used by ensure. The writer and
// withdrawal dispatcher finish the operation only after router proofs.
func (a *app) guardGC(now time.Time) {
	if err := a.guard.expireUnstartedPins(); err != nil {
		log.Printf("guard pin expiration deferred: %v", err)
		return
	}
	a.workOnce.Do(func() { a.work = make(chan struct{}, 1) })
	select {
	case a.work <- struct{}{}:
	default:
		return
	}
	defer func() { <-a.work }()
	a.mu.Lock()
	var candidates []string
	for prefix, r := range a.routes {
		live := false
		for _, until := range r.References {
			if until.After(now) {
				live = true
				break
			}
		}
		if !live && !now.Before(r.RetainUntil) && !now.Before(r.AnswerUntil.Add(5*time.Minute)) {
			candidates = append(candidates, prefix)
		}
	}
	a.mu.Unlock()
	for _, prefix := range candidates {
		a.candidates.Add(1)
		if a.cfg.GCDryRun {
			log.Printf("guard GC candidate (dry-run) prefix=%s", prefix)
			continue
		}
		if err := a.guard.retire(prefix, 5*time.Minute); err != nil {
			log.Printf("guard retirement deferred prefix=%s: %v", prefix, err)
		}
	}
}
