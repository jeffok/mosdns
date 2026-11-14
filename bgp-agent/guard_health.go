// SPDX-License-Identifier: GPL-3.0-or-later
package main

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"sort"
	"strconv"
	"time"
)

type guardHealthEntry struct {
	Prefix     string `json:"prefix"`
	Generation string `json:"generation"`
}
type guardHealthSnapshot struct {
	Epoch      string             `json:"epoch"`
	SnapshotID string             `json:"snapshot_id"`
	Page       int                `json:"page"`
	TotalPages int                `json:"total_pages"`
	Entries    []guardHealthEntry `json:"entries"`
}

func (a *app) issueGuardHealth() (guardHealthSnapshot, error) {
	started := time.Now()
	a.mu.Lock()
	aAcquired := time.Now()
	a.guard.mu.Lock()
	guardAcquired := time.Now()
	defer func() {
		a.guard.mu.Unlock()
		a.mu.Unlock()
		aWait, gWait, hold := aAcquired.Sub(started), guardAcquired.Sub(aAcquired), time.Since(guardAcquired)
		if aWait >= 100*time.Millisecond || gWait >= 100*time.Millisecond || hold >= 100*time.Millisecond {
			log.Printf("guard health lock_app_wait=%s lock_guard_wait=%s lock_hold=%s", aWait, gWait, hold)
		}
	}()
	now := time.Now()
	if !a.guard.ready(now) || !a.up {
		return guardHealthSnapshot{}, errors.New("guard unavailable")
	}
	for id, s := range a.snapshots {
		if now.Sub(s.issued) >= time.Duration(a.cfg.AckMaxAgeMS)*time.Millisecond {
			delete(a.snapshots, id)
		}
	}
	if len(a.snapshots) >= maxPendingSnapshots {
		return guardHealthSnapshot{}, errors.New("snapshot capacity")
	}
	result := guardHealthSnapshot{Epoch: a.epoch, SnapshotID: token(), Entries: []guardHealthEntry{}}
	want := map[string]uint64{}
	generations := map[string]string{}
	for p, route := range a.routes {
		r, ok := a.guard.data.Records[p]
		if !ok || (r.Phase != "ACTIVE" && r.Phase != "GUARDED") {
			continue
		}
		result.Entries = append(result.Entries, guardHealthEntry{p, r.Generation})
		want[p] = route.generation
		generations[p] = r.Generation
	}
	sort.Slice(result.Entries, func(i, j int) bool { return result.Entries[i].Prefix < result.Entries[j].Prefix })
	a.snapshotSequence++
	a.snapshots[result.SnapshotID] = routeSnapshot{epoch: a.epoch, issued: now, sequence: a.snapshotSequence, prefixes: want, guardGenerations: generations}
	return result, nil
}
func paginateGuardHealth(s guardHealthSnapshot, page int) (guardHealthSnapshot, error) {
	total := (len(s.Entries) + 63) / 64
	if total == 0 {
		total = 1
	}
	if page < 0 || page >= total || total > 8 {
		return guardHealthSnapshot{}, errors.New("invalid health page")
	}
	start := page * 64
	end := start + 64
	if end > len(s.Entries) {
		end = len(s.Entries)
	}
	s.Page = page
	s.TotalPages = total
	s.Entries = s.Entries[start:end]
	return s, nil
}
func (a *app) readGuardHealthPage(id string, page int) (guardHealthSnapshot, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	snap, ok := a.snapshots[id]
	if !ok || snap.guardGenerations == nil || snap.epoch != a.epoch || snap.sequence <= a.lastAppliedSnapshot || time.Since(snap.issued) >= time.Duration(a.cfg.AckMaxAgeMS)*time.Millisecond {
		return guardHealthSnapshot{}, errors.New("unknown or expired health snapshot")
	}
	out := guardHealthSnapshot{Epoch: snap.epoch, SnapshotID: id, Entries: []guardHealthEntry{}}
	for p, g := range snap.guardGenerations {
		out.Entries = append(out.Entries, guardHealthEntry{Prefix: p, Generation: g})
	}
	sort.Slice(out.Entries, func(i, j int) bool { return out.Entries[i].Prefix < out.Entries[j].Prefix })
	return paginateGuardHealth(out, page)
}

func (a *app) acknowledgeGuardHealth(epoch, id string, prefixes []string) error {
	return a.acknowledgeGuardHealthPage(epoch, id, 0, 1, prefixes)
}

// Pages never update freshness until the complete ordered snapshot is present.
// Lost pages therefore fail closed rather than accepting a partial view.
func (a *app) acknowledgeGuardHealthPage(epoch, id string, sequence, total int, prefixes []string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.guard.mu.Lock()
	defer a.guard.mu.Unlock()
	if !a.guard.ready(time.Now()) || !a.up || epoch != a.epoch {
		return errors.New("guard unavailable or stale epoch")
	}
	snap, ok := a.snapshots[id]
	if !ok || snap.guardGenerations == nil || snap.epoch != epoch || snap.sequence <= a.lastAppliedSnapshot {
		return errors.New("unknown guard snapshot")
	}
	if time.Since(snap.issued) >= time.Duration(a.cfg.AckMaxAgeMS)*time.Millisecond {
		delete(a.snapshots, id)
		return errors.New("expired snapshot")
	}
	if total < 1 || total > 8 || sequence < 0 || sequence >= total || len(prefixes) > 160 || sequence != snap.healthNextPage || (snap.healthTotalPages != 0 && snap.healthTotalPages != total) {
		return errors.New("invalid health page")
	}
	seen := make(map[string]bool, len(snap.healthAccepted)+len(prefixes))
	for _, p := range snap.healthAccepted {
		seen[p] = true
	}
	for _, p := range prefixes {
		r, ok := a.guard.data.Records[p]
		if !ok || snap.guardGenerations[p] != r.Generation || (r.Phase != "ACTIVE" && r.Phase != "GUARDED") || seen[p] {
			return errors.New("guard changed or duplicate prefix")
		}
		seen[p] = true
	}
	snap.healthTotalPages = total
	snap.healthNextPage++
	snap.healthAccepted = append(snap.healthAccepted, prefixes...)
	if sequence+1 == total {
		// Earlier pages may have waited while their guard changed. Recheck the
		// entire accepted set at the atomic commit, not just the last page.
		for _, p := range snap.healthAccepted {
			r, ok := a.guard.data.Records[p]
			if !ok || snap.guardGenerations[p] != r.Generation || (r.Phase != "ACTIVE" && r.Phase != "GUARDED") {
				return errors.New("guard changed before final page")
			}
		}
		return a.acknowledgeLocked(epoch, id, snap.healthAccepted)
	}
	a.snapshots[id] = snap
	return nil
}

// The indexes refer to the sorted entries of one exact bulk snapshot.
// They cannot be applied to another snapshot, omitted/duplicate indexes are
// rejected, and all affected route/guard generations are revalidated.
func (a *app) acknowledgeGuardHealthIndexes(epoch, id string, indexes []int) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.guard.mu.Lock()
	defer a.guard.mu.Unlock()
	if !a.guard.ready(time.Now()) || !a.up || epoch != a.epoch {
		return errors.New("guard unavailable or stale epoch")
	}
	snap, ok := a.snapshots[id]
	if !ok || !snap.guardBulk || snap.guardGenerations == nil || snap.epoch != epoch || snap.healthNextPage != 0 {
		return errors.New("invalid bulk snapshot")
	}
	if len(indexes) > len(snap.guardGenerations) || len(indexes) > 512 {
		return errors.New("invalid index count")
	}
	keys := make([]string, 0, len(snap.guardGenerations))
	for p := range snap.guardGenerations {
		keys = append(keys, p)
	}
	sort.Strings(keys)
	seen := map[int]bool{}
	prefixes := make([]string, 0, len(indexes))
	for _, i := range indexes {
		if i < 0 || i >= len(keys) || seen[i] {
			return errors.New("duplicate or invalid index")
		}
		seen[i] = true
		p := keys[i]
		r, ok := a.guard.data.Records[p]
		if !ok || r.Generation != snap.guardGenerations[p] || (r.Phase != "ACTIVE" && r.Phase != "GUARDED") {
			return errors.New("guard changed since snapshot")
		}
		prefixes = append(prefixes, p)
	}
	return a.acknowledgeLocked(epoch, id, prefixes)
}

func (a *app) guardHealthHTTP(w http.ResponseWriter, r *http.Request) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil || host != a.cfg.PeerIP {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	switch r.URL.Path {
	case "/guardwriter/health":
		if r.Method != "GET" {
			http.Error(w, "method", http.StatusMethodNotAllowed)
			return
		}
		var snap guardHealthSnapshot
		var err error
		id := r.URL.Query().Get("snapshot_id")
		if id == "" {
			bulk := r.URL.RawQuery == "bulk=1"
			if r.URL.RawQuery != "" && !bulk {
				http.Error(w, "invalid initial query", http.StatusBadRequest)
				return
			}
			snap, err = a.issueGuardHealth()
			if err == nil {
				if bulk {
					if len(snap.Entries) > 512 {
						err = errors.New("bulk health capacity exceeded")
					} else {
						snap.Page = 0
						snap.TotalPages = 1
					}
				} else {
					snap, err = paginateGuardHealth(snap, 0)
				}
			}
		} else {
			page, parseErr := strconv.Atoi(r.URL.Query().Get("page"))
			if parseErr != nil || page < 1 || !guardID(id) || len(r.URL.Query()) != 2 {
				http.Error(w, "invalid page query", http.StatusBadRequest)
				return
			}
			snap, err = a.readGuardHealthPage(id, page)
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		body, err := json.Marshal(snap)
		if err != nil {
			http.Error(w, "encoding failed", http.StatusInternalServerError)
			return
		}
		if r.URL.RawQuery == "bulk=1" && len(body) > 48000 {
			http.Error(w, "bulk health response too large", http.StatusServiceUnavailable)
			return
		}
		if r.URL.RawQuery == "bulk=1" {
			a.mu.Lock()
			v, ok := a.snapshots[snap.SnapshotID]
			if ok {
				v.guardBulk = true
				a.snapshots[snap.SnapshotID] = v
			}
			a.mu.Unlock()
			if !ok {
				http.Error(w, "bulk snapshot expired", http.StatusServiceUnavailable)
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		_, _ = w.Write(body)
	case "/guardwriter/health-ack":
		if r.Method != "POST" {
			http.Error(w, "method", http.StatusMethodNotAllowed)
			return
		}
		var b struct {
			Epoch      string   `json:"epoch"`
			SnapshotID string   `json:"snapshot_id"`
			Prefixes   []string `json:"prefixes"`
			Sequence   int      `json:"sequence"`
			TotalPages int      `json:"total_pages"`
			Indexes    *[]int   `json:"accepted_indexes"`
		}
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
		dec.DisallowUnknownFields()
		if dec.Decode(&b) != nil || dec.Decode(new(any)) != io.EOF {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		if b.Indexes != nil {
			if b.Prefixes != nil || b.Sequence != 0 || b.TotalPages != 0 {
				http.Error(w, "mixed health ACK modes", http.StatusBadRequest)
				return
			}
			if err := a.acknowledgeGuardHealthIndexes(b.Epoch, b.SnapshotID, *b.Indexes); err != nil {
				http.Error(w, err.Error(), http.StatusConflict)
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if b.TotalPages == 0 {
			b.TotalPages = 1
		} // compatibility for one-page local clients
		if err := a.acknowledgeGuardHealthPage(b.Epoch, b.SnapshotID, b.Sequence, b.TotalPages, b.Prefixes); err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		http.NotFound(w, r)
	}
}
