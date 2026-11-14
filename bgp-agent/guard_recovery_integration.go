// SPDX-License-Identifier: GPL-3.0-or-later
package main

import (
	"context"
	"errors"
	"log"
	"time"
)

// replayGuardedPrefixes is invoked only from the fenced first recovery pass,
// after RouterOS has read back each exact protective route. Old state must be
// loaded without publishing; this callback never manufactures a missing lease.
func (a *app) replayGuardedPrefixes(ctx context.Context, prefixes []string) error {
	if a.guard == nil {
		return errors.New("guard unavailable")
	}
	a.workOnce.Do(func() { a.work = make(chan struct{}, 1) })
	select {
	case a.work <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-a.work }()
	for _, p := range prefixes {
		a.mu.Lock()
		r := a.routes[p]
		if r == nil {
			a.mu.Unlock()
			return errors.New("guard journal has no matching route lease")
		}
		already := len(r.UUID) > 0
		a.mu.Unlock()
		if already {
			continue
		}
		var id []byte
		var err error
		if pub, ok := a.pub.(interface {
			AddContext(context.Context, string) ([]byte, error)
		}); ok {
			id, err = pub.AddContext(ctx, p)
		} else {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			id, err = a.pub.Add(p)
		}
		if err != nil {
			return err
		}
		if len(id) == 0 {
			return errors.New("publisher returned no path identifier")
		}
		a.mu.Lock()
		if a.routes[p] != r {
			a.mu.Unlock()
			return errors.New("route changed during replay")
		}
		r.UUID = id
		r.AckAt = time.Time{}
		a.mu.Unlock()
	}
	return nil
}
func (a *app) startGuardRecovery() {
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for {
			select {
			case <-a.stop:
				return
			case <-tick.C:
			}
			a.guard.mu.Lock()
			needed := a.guard.recoveryNeedsReconcile
			a.guard.mu.Unlock()
			if !needed {
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
			err := a.guard.restoreGuardedBGP(ctx, a.recovery, a.replayGuardedPrefixes)
			if err == nil {
				err = a.guard.recoverStable(ctx, a.recovery)
			}
			cancel()
			if err != nil {
				log.Printf("guard recovery deferred: %v", err)
				continue
			}
			if err := a.scheduleRecoveredOrphans(); err != nil {
				log.Printf("recovered orphan cleanup deferred: %v", err)
			}
			return
		}
	}()
}
