// SPDX-License-Identifier: GPL-3.0-or-later
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"
)

func recoveryRequest(t *testing.T, url string) struct {
	Challenge string                  `json:"challenge"`
	Epoch     string                  `json:"epoch"`
	Records   []recoveryRequestRecord `json:"records"`
} {
	t.Helper()
	var result struct {
		Challenge string                  `json:"challenge"`
		Epoch     string                  `json:"epoch"`
		Records   []recoveryRequestRecord `json:"records"`
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		r, err := http.Get(url + "/guardrecovery/next")
		if err != nil {
			t.Fatal(err)
		}
		if r.StatusCode == 200 {
			err = json.NewDecoder(r.Body).Decode(&result)
			r.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			return result
		}
		r.Body.Close()
		time.Sleep(time.Millisecond)
	}
	t.Fatal("recovery request never became available")
	return result
}
func postRecovery(t *testing.T, url string, p recoveryPage) int {
	t.Helper()
	b, e := json.Marshal(p)
	if e != nil {
		t.Fatal(e)
	}
	r, e := http.Post(url+"/guardrecovery/attest", "application/json", bytes.NewReader(b))
	if e != nil {
		t.Fatal(e)
	}
	defer r.Body.Close()
	return r.StatusCode
}
func TestRecoveryHTTPGuardBeforeReplayAndSecondReadback(t *testing.T) {
	c, _, p := recoveryFixture(t)
	c.recoveryUntil = time.Now().Add(-time.Millisecond)
	h := &recoveryHTTP{c: c, peer: netip.MustParseAddr("127.0.0.1")}
	server := httptest.NewServer(h)
	defer server.Close()
	result := make(chan error, 1)
	published := false
	go func() {
		result <- c.restoreGuardedBGP(context.Background(), h, func(_ context.Context, prefixes []string) error {
			if len(prefixes) != 1 || prefixes[0] != p {
				t.Errorf("bad prefixes: %v", prefixes)
			}
			published = true
			return nil
		})
	}()
	q := recoveryRequest(t, server.URL)
	if published {
		t.Fatal("publication occurred before snapshot")
	}
	record := q.Records[0]
	page := recoveryPage{Version: 1, Challenge: q.Challenge, Epoch: q.Epoch, TotalPages: 1, ObjectsComplete: true, OldJobsQuiescent: true, WriterFenced: true, Observations: []recoveryObservation{{Prefix: p, Generation: record.Generation, GuardPresent: true, GuardOwned: true}}}
	if code := postRecovery(t, server.URL, page); code != 204 {
		t.Fatalf("first pass status %d", code)
	}
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	if !published || c.ready(time.Now()) {
		t.Fatal("premature recovery or no publication")
	}
	go func() { result <- c.recoverStable(context.Background(), h) }()
	q2 := recoveryRequest(t, server.URL)
	if q2.Challenge == q.Challenge {
		t.Fatal("challenge reused")
	}
	page.Challenge = q2.Challenge
	page.Observations[0].BGPPresent = true
	page.Observations[0].BGPVerified = true
	if code := postRecovery(t, server.URL, page); code != 204 {
		t.Fatalf("second pass status %d", code)
	}
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	if !c.ready(time.Now()) {
		t.Fatal("verified replay did not unlock")
	}
}
func TestRecoveryHTTPRejectsOutOfOrderAndOversizedPages(t *testing.T) {
	c, _, _ := recoveryFixture(t)
	c.recoveryUntil = time.Now().Add(-time.Millisecond)
	h := &recoveryHTTP{c: c, peer: netip.MustParseAddr("127.0.0.1")}
	server := httptest.NewServer(h)
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- c.recoverStable(ctx, h) }()
	q := recoveryRequest(t, server.URL)
	page := recoveryPage{Version: 1, Challenge: q.Challenge, Epoch: q.Epoch, Sequence: 1, TotalPages: 2, ObjectsComplete: true, OldJobsQuiescent: true, WriterFenced: true}
	if code := postRecovery(t, server.URL, page); code != 409 {
		t.Fatalf("out of order status %d", code)
	}
	page.Sequence = 0
	page.Observations = make([]recoveryObservation, 9)
	if code := postRecovery(t, server.URL, page); code != 409 {
		t.Fatalf("oversized page status %d", code)
	}
	cancel()
	if <-result == nil {
		t.Fatal("cancelled recovery succeeded")
	}
}

func TestRecoveryHTTPDisconnectDuringReplayDoesNotUnlock(t *testing.T) {
	c, _, p := recoveryFixture(t)
	c.recoveryUntil = time.Now().Add(-time.Millisecond)
	h := &recoveryHTTP{c: c, peer: netip.MustParseAddr("127.0.0.1")}
	server := httptest.NewServer(h)
	defer server.Close()
	entered := make(chan struct{})
	release := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		result <- c.restoreGuardedBGP(context.Background(), h, func(ctx context.Context, prefixes []string) error { close(entered); <-release; return nil })
	}()
	q := recoveryRequest(t, server.URL)
	page := recoveryPage{Version: 1, Challenge: q.Challenge, Epoch: q.Epoch, TotalPages: 1, ObjectsComplete: true, OldJobsQuiescent: true, WriterFenced: true, Observations: []recoveryObservation{{Prefix: p, Generation: q.Records[0].Generation, GuardPresent: true, GuardOwned: true}}}
	b, _ := json.Marshal(page)
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/guardrecovery/attest", bytes.NewReader(b))
	posted := make(chan error, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if resp != nil {
			resp.Body.Close()
		}
		posted <- err
	}()
	<-entered
	cancel()
	<-posted
	time.Sleep(20 * time.Millisecond)
	close(release)
	if err := <-result; err == nil {
		t.Fatal("disconnected writer accepted as fenced")
	}
	if !c.recoveryNeedsReconcile {
		t.Fatal("disconnected recovery unlocked")
	}
}

func TestRecoveryGetPagesBoundedAndDeadlineFence(t *testing.T) {
	c := guardTest(t, 512)
	h := &recoveryHTTP{c: c, peer: netip.MustParseAddr("127.0.0.1"), challenge: token(), epoch: token()}
	for i := 0; i < 512; i++ {
		h.records = append(h.records, recoveryRequestRecord{Prefix: fmt.Sprintf("1.1.%d.%d/32", i/256, i%256), Generation: token(), Phase: "ACTIVE"})
	}
	server := httptest.NewServer(h)
	defer server.Close()
	for page := 0; page < 4; page++ {
		r, err := http.Get(fmt.Sprintf("%s/guardrecovery/next?page=%d", server.URL, page))
		if err != nil {
			t.Fatal(err)
		}
		var response struct {
			Page    int                     `json:"page"`
			Total   int                     `json:"total_pages"`
			Records []recoveryRequestRecord `json:"records"`
		}
		if err = json.NewDecoder(r.Body).Decode(&response); err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		if response.Page != page || response.Total != 4 || len(response.Records) != 128 {
			t.Fatal("incorrect pagination")
		}
	}
	h.finalCtx = context.Background()
	h.finalDeadline = time.Now().Add(-time.Millisecond)
	if h.Verify(context.Background()) == nil || h.Commit() == nil {
		t.Fatal("expired fence accepted")
	}
}
