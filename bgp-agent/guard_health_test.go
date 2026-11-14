// SPDX-License-Identifier: GPL-3.0-or-later
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"
)

func TestGuardHealthSnapshotClearsMissingAndRejectsChangedGeneration(t *testing.T) {
	a, _ := testApp(t)
	a.guard = guardTest(t, 2)
	h := a.httpHandler()
	p := "8.8.8.8/32"
	if err := a.guard.prepare([]string{p}); err != nil {
		t.Fatal(err)
	}
	guardActivate(t, a.guard, p)
	a.mu.Lock()
	a.routes[p] = &lease{Prefix: p, generation: 1, AckAt: time.Now(), References: map[string]time.Time{}}
	a.mu.Unlock()
	s, err := a.issueGuardHealth()
	if err != nil || len(s.Entries) != 1 {
		t.Fatalf("snapshot: %+v %v", s, err)
	}
	if err := a.acknowledgeGuardHealth(s.Epoch, s.SnapshotID, nil); err != nil {
		t.Fatal(err)
	}
	a.mu.Lock()
	expired := a.routes[p].AckAt.IsZero()
	a.mu.Unlock()
	if !expired {
		t.Fatal("missing route retained acknowledgement")
	}
	s, err = a.issueGuardHealth()
	if err != nil {
		t.Fatal(err)
	}
	a.guard.mu.Lock()
	r := a.guard.data.Records[p]
	r.Generation = token()
	a.guard.data.Records[p] = r
	a.guard.mu.Unlock()
	if err := a.acknowledgeGuardHealth(s.Epoch, s.SnapshotID, []string{p}); err == nil {
		t.Fatal("accepted old guard generation")
	}
	w := guardRequest(h, http.MethodGet, "/guardwriter/health", nil, "192.168.88.1:9")
	if w.Code != http.StatusForbidden {
		t.Fatalf("unauthorized health: %d", w.Code)
	}
	w = guardRequest(h, http.MethodGet, "/guardwriter/health", nil, "192.168.88.254:9")
	var got guardHealthSnapshot
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &got) != nil || len(got.Entries) != 1 {
		t.Fatalf("bad health: %d %s", w.Code, w.Body.String())
	}
}

func TestGuardHealthPagedAckCommitsAtomically(t *testing.T) {
	a, _ := testApp(t)
	a.guard = guardTest(t, 512)
	a.guard.mu.Lock()
	err := a.guard.change(func(j *guardJournal) error {
		for i := 0; i < 512; i++ {
			p := fmt.Sprintf("8.8.%d.%d/32", i/256, i%256)
			j.Records[p] = guardRecord{Prefix: p, Generation: token(), Phase: "ACTIVE"}
		}
		return nil
	})
	a.guard.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 512; i++ {
		p := fmt.Sprintf("8.8.%d.%d/32", i/256, i%256)
		a.routes[p] = &lease{Prefix: p, generation: uint64(i + 1), AckAt: time.Now().Add(-time.Minute)}
	}
	s, err := a.issueGuardHealth()
	if err != nil {
		t.Fatal(err)
	}
	prefixes := []string{}
	for _, e := range s.Entries {
		prefixes = append(prefixes, e.Prefix)
	}
	if err := a.acknowledgeGuardHealthPage(s.Epoch, s.SnapshotID, 0, 8, prefixes[:64]); err != nil {
		t.Fatal(err)
	}
	for _, p := range prefixes {
		if time.Since(a.routes[p].AckAt) < time.Second {
			t.Fatal("partial page committed")
		}
	}
	if err := a.acknowledgeGuardHealthPage(s.Epoch, s.SnapshotID, 0, 8, prefixes[:64]); err == nil {
		t.Fatal("duplicate page accepted")
	}
	for page := 1; page < 8; page++ {
		if err := a.acknowledgeGuardHealthPage(s.Epoch, s.SnapshotID, page, 8, prefixes[page*64:(page+1)*64]); err != nil {
			t.Fatalf("page %d: %v", page, err)
		}
		if page < 7 {
			for _, p := range prefixes {
				if time.Since(a.routes[p].AckAt) < time.Second {
					t.Fatal("partial page committed")
				}
			}
		}
	}
	for _, p := range prefixes {
		if time.Since(a.routes[p].AckAt) > time.Second {
			t.Fatal("final page did not commit")
		}
	}
}

func TestGuardHealthGetPagesShareSnapshotAndExpire(t *testing.T) {
	a, _ := testApp(t)
	a.guard = guardTest(t, 512)
	a.guard.mu.Lock()
	err := a.guard.change(func(j *guardJournal) error {
		for i := 0; i < 512; i++ {
			p := fmt.Sprintf("8.8.%d.%d/32", i/256, i%256)
			j.Records[p] = guardRecord{Prefix: p, Generation: token(), Phase: "ACTIVE"}
		}
		return nil
	})
	a.guard.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 512; i++ {
		p := fmt.Sprintf("8.8.%d.%d/32", i/256, i%256)
		a.routes[p] = &lease{Prefix: p, generation: uint64(i + 1)}
	}
	h := a.httpHandler()
	bulk := guardRequest(h, http.MethodGet, "/guardwriter/health?bulk=1", nil, "192.168.88.254:9")
	var all guardHealthSnapshot
	if bulk.Code != 200 || json.Unmarshal(bulk.Body.Bytes(), &all) != nil || len(all.Entries) != 512 || all.TotalPages != 1 || bulk.Body.Len() > 48000 {
		t.Fatalf("invalid bulk response code=%d entries=%d bytes=%d", bulk.Code, len(all.Entries), bulk.Body.Len())
	}
	first := guardRequest(h, http.MethodGet, "/guardwriter/health", nil, "192.168.88.254:9")
	var snap guardHealthSnapshot
	if first.Code != 200 || json.Unmarshal(first.Body.Bytes(), &snap) != nil || snap.TotalPages != 8 || snap.Page != 0 || len(snap.Entries) != 64 {
		t.Fatalf("bad first page: %d %+v", first.Code, snap)
	}
	for page := 1; page < 8; page++ {
		path := fmt.Sprintf("/guardwriter/health?snapshot_id=%s&page=%d", snap.SnapshotID, page)
		w := guardRequest(h, http.MethodGet, path, nil, "192.168.88.254:9")
		var next guardHealthSnapshot
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &next) != nil || next.SnapshotID != snap.SnapshotID || next.Epoch != snap.Epoch || next.Page != page || next.TotalPages != 8 || len(next.Entries) != 64 {
			t.Fatalf("bad page %d: %d %+v", page, w.Code, next)
		}
	}
	a.peer(false)
	path := fmt.Sprintf("/guardwriter/health?snapshot_id=%s&page=1", snap.SnapshotID)
	if w := guardRequest(h, http.MethodGet, path, nil, "192.168.88.254:9"); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("accepted stale epoch: %d", w.Code)
	}
}

func TestGuardHealthAckWorstCaseFitsRouterOSPost(t *testing.T) {
	prefixes := make([]string, 160)
	for i := range prefixes {
		prefixes[i] = fmt.Sprintf("255.255.255.%d/32", i)
	}
	b, err := json.Marshal(map[string]any{"epoch": "0123456789abcdef01234567", "snapshot_id": "0123456789abcdef01234567", "sequence": 3, "total_pages": 4, "prefixes": prefixes})
	if err != nil {
		t.Fatal(err)
	}
	if len(b) > 3900 {
		t.Fatalf("health ACK exceeds bound: %d", len(b))
	}
	t.Logf("160-prefix ACK %d bytes", len(b))
}

func TestGuardHealthBulkIndexAck512Atomic(t *testing.T) {
	a, _ := testApp(t)
	a.guard = guardTest(t, 512)
	a.guard.mu.Lock()
	err := a.guard.change(func(j *guardJournal) error {
		for i := 0; i < 512; i++ {
			p := fmt.Sprintf("8.8.%d.%d/32", i/256, i%256)
			j.Records[p] = guardRecord{Prefix: p, Generation: token(), Phase: "ACTIVE"}
		}
		return nil
	})
	a.guard.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 512; i++ {
		p := fmt.Sprintf("8.8.%d.%d/32", i/256, i%256)
		a.routes[p] = &lease{Prefix: p, generation: uint64(i + 1), AckAt: time.Now().Add(-time.Minute)}
	}
	h := a.httpHandler()
	w := guardRequest(h, http.MethodGet, "/guardwriter/health?bulk=1", nil, "192.168.88.254:1")
	var s guardHealthSnapshot
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &s) != nil || len(s.Entries) != 512 {
		t.Fatal("invalid bulk")
	}
	invalid := []int{0, 0}
	data, _ := json.Marshal(map[string]any{"epoch": s.Epoch, "snapshot_id": s.SnapshotID, "accepted_indexes": invalid})
	if w = guardRequest(h, http.MethodPost, "/guardwriter/health-ack", data, "192.168.88.254:1"); w.Code != 409 {
		t.Fatalf("duplicate index: %d", w.Code)
	}
	indexes := make([]int, 512)
	for i := range indexes {
		indexes[i] = i
	}
	data, _ = json.Marshal(map[string]any{"epoch": s.Epoch, "snapshot_id": s.SnapshotID, "accepted_indexes": indexes})
	if len(data) > 3900 {
		t.Fatalf("index payload size %d", len(data))
	}
	if w = guardRequest(h, http.MethodPost, "/guardwriter/health-ack", data, "192.168.88.254:1"); w.Code != 204 {
		t.Fatalf("bulk ack: %d %s", w.Code, w.Body.String())
	}
	for _, e := range s.Entries {
		if time.Since(a.routes[e.Prefix].AckAt) > time.Second {
			t.Fatal("ACK not applied")
		}
	}
}
