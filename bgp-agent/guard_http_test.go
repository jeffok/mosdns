// SPDX-License-Identifier: GPL-3.0-or-later
package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func guardRequest(h http.Handler, method, path string, body []byte, remote string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, bytes.NewReader(body))
	r.RemoteAddr = remote
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestGuardedConfirmWaitsForWriterAndReturnsPin(t *testing.T) {
	a, pub := testApp(t)
	a.cfg.ConfirmClientIP = "127.0.0.1"
	a.cfg.PeerIP = "192.168.88.254"
	a.guard = guardTest(t, 2) // Explicit test injection; production enable is refused.
	h := a.httpHandler()
	r := httptest.NewRequest(http.MethodPost, "/confirm", strings.NewReader(`{"domain":"test.example.","ips":["8.8.8.8"],"ttl":0}`))
	r.RemoteAddr = "127.0.0.1:1234"
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { h.ServeHTTP(w, r); close(done) }()
	select {
	case <-pub.notify:
	case <-time.After(time.Second):
		t.Fatal("BGP publication did not start")
	}
	select {
	case <-done:
		t.Fatal("confirmation returned before ROS guard proof")
	default:
	}
	next := guardRequest(h, http.MethodGet, "/guardwriter/next", nil, "192.168.88.254:5")
	var op guardOperation
	if next.Code != http.StatusOK || json.Unmarshal(next.Body.Bytes(), &op) != nil {
		t.Fatalf("no guard operation: %d %s", next.Code, next.Body.String())
	}
	body, _ := json.Marshal(guardWriterAck{guardOperation: op, Result: "ok", Proof: guardProof{ExactOwner: true, GuardPresent: true, BGPActive: true}})
	if got := guardRequest(h, http.MethodPost, "/guardwriter/ack", body, "192.168.88.254:5").Code; got != http.StatusNoContent {
		t.Fatalf("guard acknowledgement: %d", got)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("guarded confirmation timed out")
	}
	var result struct {
		ID string `json:"pin_id"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &result) != nil || !guardID(result.ID) {
		t.Fatalf("invalid pin result: %d %s", w.Code, w.Body.String())
	}
	if !a.guard.maySend(result.ID) {
		t.Fatal("confirmation did not reserve an answer pin")
	}
}

func TestGuardWriterHTTPProtocol(t *testing.T) {
	c := guardTest(t, 1)
	if err := c.prepare([]string{"1.1.1.1/32"}); err != nil {
		t.Fatal(err)
	}
	h := guardWriterHTTP(c, netip.MustParseAddr("192.168.88.254"))
	if got := guardRequest(h, http.MethodGet, "/guardwriter/next", nil, "192.168.88.1:5").Code; got != http.StatusForbidden {
		t.Fatalf("untrusted peer: %d", got)
	}
	w := guardRequest(h, http.MethodGet, "/guardwriter/next", nil, "192.168.88.254:5")
	var op guardOperation
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &op) != nil || op.Action != "create_guard" {
		t.Fatalf("invalid next: %d %s", w.Code, w.Body.String())
	}
	body, err := json.Marshal(guardWriterAck{guardOperation: op, Result: "ok", Proof: guardProof{ExactOwner: true, GuardPresent: true, BGPActive: true}})
	if err != nil {
		t.Fatal(err)
	}
	if got := guardRequest(h, http.MethodPost, "/guardwriter/ack", body, "192.168.88.254:5").Code; got != http.StatusNoContent {
		t.Fatalf("ack rejected: %d, body=%s", got, body)
	}
	if got := guardRequest(h, http.MethodGet, "/guardwriter/next", nil, "192.168.88.254:5").Code; got != http.StatusNoContent {
		t.Fatalf("expected idle: %d", got)
	}
}

func TestGuardPinHTTPSingleSendAndCompletion(t *testing.T) {
	c := guardTest(t, 1)
	p := "1.1.1.1/32"
	if err := c.prepare([]string{p}); err != nil {
		t.Fatal(err)
	}
	guardActivate(t, c, p)
	pin, err := c.reserve([]string{p}, 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]string{"pin_id": pin.ID})
	h := guardPinHTTP(c, netip.MustParseAddr("127.0.0.1"))
	post := func(path string) int { return guardRequest(h, http.MethodPost, path, body, "127.0.0.1:5").Code }
	if got := post("/guardpin/may-send"); got != http.StatusNoContent {
		t.Fatalf("begin: %d", got)
	}
	if got := post("/guardpin/may-send"); got != http.StatusConflict {
		t.Fatalf("duplicate begin: %d", got)
	}
	if got := post("/guardpin/finish-sent"); got != http.StatusNoContent {
		t.Fatalf("finish: %d", got)
	}
	if got := post("/guardpin/finish-sent"); got != http.StatusConflict {
		t.Fatalf("duplicate finish: %d", got)
	}
}
