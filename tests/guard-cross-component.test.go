// SPDX-License-Identifier: GPL-3.0-or-later
// Copied into the agent module by guard-cross-component.sh. This test starts
// an actual patched mosdns binary, real UDP/TCP sockets, the agent HTTP stack,
// and a deterministic RouterOS writer simulator. It does not simulate the ROS
// forwarding plane, its script interpreter, or BGP peering.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
)

func TestCrossComponentGuardDNS(t *testing.T) {
	bin := os.Getenv("MOSDNS_GUARD_E2E_BIN")
	if bin == "" {
		t.Skip("MOSDNS_GUARD_E2E_BIN not set")
	}
	a, pub := testApp(t)
	var err error
	a.cfg.ConfirmMS = 4000
	a.cfg.AckMaxAgeMS = 3000
	a.cfg.PeerIP = "127.0.0.1"
	a.cfg.ConfirmClientIP = "127.0.0.1"
	a.guard, err = newGuardCoordinator(filepath.Join(t.TempDir(), "guard.json"), 8, 300*time.Second, 2*time.Second, time.Second, time.Second, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	agent := httptest.NewServer(a.httpHandler())
	defer agent.Close()
	upsock, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	up := &dns.Server{PacketConn: upsock, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, q *dns.Msg) {
		m := new(dns.Msg)
		m.SetReply(q)
		m.Answer = []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: q.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 30}, A: net.ParseIP("8.8.4.4")}}
		_ = w.WriteMsg(m)
	})}
	go up.ActivateAndServe()
	defer up.Shutdown()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dnsAddr := l.Addr().String()
	l.Close()
	dir := t.TempDir()
	cn := filepath.Join(dir, "cn.txt")
	if err := os.WriteFile(cn, []byte("223.5.0.0/16\n"), 0600); err != nil {
		t.Fatal(err)
	}
	config := fmt.Sprintf(`log:
  level: error
plugins:
- tag: upstream
  type: forward
  args:
    upstreams:
    - addr: udp://%s
- tag: bgp_tag
  type: ros_bgp
  args:
    mode: tag
- tag: bgp_confirm
  type: ros_bgp
  args:
    mode: confirm
    endpoint: %s/confirm
    guard_writer: true
    sync_budget: 4000ms
    cn_ip_file: %s
- tag: entry
  type: sequence
  args:
  - exec: $upstream
  - exec: $bgp_tag
  - exec: $bgp_confirm
- tag: udp
  type: udp_server
  args:
    entry: entry
    listen: %s
- tag: tcp
  type: tcp_server
  args:
    entry: entry
    listen: %s
`, upsock.LocalAddr(), agent.URL, cn, dnsAddr, dnsAddr)
	cfg := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfg, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "start", "-c", cfg)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	var guardAcked, lostRetry atomic.Bool
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		client := &http.Client{Timeout: time.Second}
		for {
			select {
			case <-stop:
				return
			case <-time.After(10 * time.Millisecond):
			}
			resp, err := client.Get(agent.URL + "/guardwriter/batch-next")
			if err == nil {
				body, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				if resp.StatusCode == 200 {
					var batch guardBatch
					if json.Unmarshal(body, &batch) == nil {
						ack := guardBatchAck{Version: 1, ID: batch.ID}
						sawCreate := false
						for _, op := range batch.Operations {
							proof := guardProof{ExactOwner: true, GuardPresent: true}
							if op.Action == "confirm_bgp" {
								adds, _ := pub.counts()
								if adds == 0 || !guardAcked.Load() {
									t.Errorf("BGP confirmed before guard/publish")
									return
								}
								proof.BGPActive = true
							}
							if op.Action == "create_guard" {
								adds, _ := pub.counts()
								if adds != 0 {
									t.Errorf("BGP published before guard")
									return
								}
								guardAcked.Store(true)
								sawCreate = true
							}
							ack.Results = append(ack.Results, guardBatchResult{Operation: op, Result: "ok", Proof: proof})
						}
						data, _ := json.Marshal(ack)
						post := func() int {
							r, e := client.Post(agent.URL+"/guardwriter/batch-ack", "application/json", bytes.NewReader(data))
							if e != nil {
								return 0
							}
							io.Copy(io.Discard, r.Body)
							r.Body.Close()
							return r.StatusCode
						}
						if code := post(); code != 204 {
							t.Errorf("batch ack: %d", code)
							return
						}
						if sawCreate {
							if code := post(); code != 204 {
								t.Errorf("lost batch ACK retry: %d", code)
								return
							}
							lostRetry.Store(true)
						}
					}
				}
			}

			resp, err = client.Get(agent.URL + "/guardwriter/health")
			if err == nil {
				body, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				if resp.StatusCode == 200 {
					var s guardHealthSnapshot
					if json.Unmarshal(body, &s) == nil {
						accepted := []string{}
						if guardAcked.Load() {
							adds, _ := pub.counts()
							if adds > 0 {
								for _, r := range s.Entries {
									accepted = append(accepted, r.Prefix)
								}
							}
						}
						data, _ := json.Marshal(map[string]any{"epoch": s.Epoch, "snapshot_id": s.SnapshotID, "prefixes": accepted})
						r, e := client.Post(agent.URL+"/guardwriter/health-ack", "application/json", bytes.NewReader(data))
						if e == nil {
							io.Copy(io.Discard, r.Body)
							r.Body.Close()
						}
					}
				}
			}
		}
	}()
	var reply *dns.Msg
	for i := 0; i < 50; i++ {
		q := new(dns.Msg)
		q.SetQuestion("cross-test.example.", dns.TypeA)
		reply, _, err = (&dns.Client{Timeout: 5 * time.Second}).Exchange(q, dnsAddr)
		if err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil || reply == nil || reply.Rcode != dns.RcodeSuccess || len(reply.Answer) != 1 {
		t.Fatalf("real DNS failed: %v reply=%v", err, reply)
	}
	tcpQuery := new(dns.Msg)
	tcpQuery.SetQuestion("cross-tcp.example.", dns.TypeA)
	tcpReply, _, tcpErr := (&dns.Client{Net: "tcp", Timeout: 5 * time.Second}).Exchange(tcpQuery, dnsAddr)
	if tcpErr != nil || tcpReply.Rcode != dns.RcodeSuccess || len(tcpReply.Answer) != 1 {
		t.Fatalf("real TCP DNS failed: %v %v", tcpErr, tcpReply)
	}
	if !guardAcked.Load() || !lostRetry.Load() {
		t.Fatal("guard or lost ACK retry not covered")
	}
	deadline := time.Now().Add(time.Second)
	for {
		a.guard.mu.Lock()
		n := len(a.guard.data.Pins)
		a.guard.mu.Unlock()
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("socket completion did not release guard pin")
		}
		time.Sleep(time.Millisecond)
	}
	restored, err := newGuardCoordinator(a.guard.path, 8, a.guard.ttlMax, a.guard.sendMargin, a.guard.clockUncertainty, a.guard.quiet, a.guard.retention)
	if err != nil {
		t.Fatal(err)
	}
	if restored.guardsReady([]string{"8.8.4.4/32"}) {
		t.Fatal("restart served before router recovery")
	}
	// Restart both sides without simulating a router recovery. A cached guard
	// journal is not sufficient authorization for a new BGP publication.
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	a2, p2 := testApp(t)
	a2.cfg.ConfirmMS = 300
	a2.cfg.PeerIP = "127.0.0.1"
	a2.cfg.ConfirmClientIP = "127.0.0.1"
	a2.guard = restored
	a.mu.Lock()
	saved := *a.routes["8.8.4.4/32"]
	a.mu.Unlock()
	if err := a2.loadSavedLease(saved, false); err != nil {
		t.Fatal(err)
	}
	a2.recovery = &recoveryHTTP{c: restored, peer: netip.MustParseAddr("127.0.0.1")}
	agent2 := httptest.NewServer(a2.httpHandler())
	defer agent2.Close()
	config2 := strings.ReplaceAll(config, agent.URL, agent2.URL)
	config2 = strings.ReplaceAll(config2, "sync_budget: 4000ms", "sync_budget: 400ms")
	cfg2 := filepath.Join(dir, "restart.yaml")
	if err := os.WriteFile(cfg2, []byte(config2), 0600); err != nil {
		t.Fatal(err)
	}
	cmd2 := exec.Command(bin, "start", "-c", cfg2)
	cmd2.Stdout = io.Discard
	cmd2.Stderr = io.Discard
	if err := cmd2.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd2.Process.Kill(); _ = cmd2.Wait() }()
	var restarted *dns.Msg
	for i := 0; i < 50; i++ {
		q := new(dns.Msg)
		q.SetQuestion("restart.example.", dns.TypeA)
		restarted, _, err = (&dns.Client{Timeout: time.Second}).Exchange(q, dnsAddr)
		if err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil || restarted == nil || restarted.Rcode != dns.RcodeServerFailure || len(restarted.Answer) != 0 {
		t.Fatalf("restart did not fail closed: %v %v", err, restarted)
	}
	if adds, _ := p2.counts(); adds != 0 {
		t.Fatal("restart published BGP without recovery")
	}
	// Force the already-tested restart grace to have elapsed; both real HTTP
	// recovery passes still execute with a RouterOS job simulator holding the
	// final POST until the agent commits.
	restored.mu.Lock()
	restored.recoveryUntil = time.Now().Add(-time.Second)
	restored.mu.Unlock()
	recoveryDone := make(chan struct{})
	recoveryStop := make(chan struct{})
	go func() {
		defer close(recoveryDone)
		client := &http.Client{Timeout: 5 * time.Second}
		for {
			select {
			case <-recoveryStop:
				return
			case <-time.After(10 * time.Millisecond):
			}
			res, e := client.Get(agent2.URL + "/guardrecovery/next")
			if e != nil {
				continue
			}
			b, _ := io.ReadAll(res.Body)
			res.Body.Close()
			if res.StatusCode != 200 {
				continue
			}
			var req struct {
				Version   int                     `json:"version"`
				Challenge string                  `json:"challenge"`
				Epoch     string                  `json:"epoch"`
				Records   []recoveryRequestRecord `json:"records"`
			}
			if json.Unmarshal(b, &req) != nil || len(req.Records) == 0 {
				continue
			}
			adds, _ := p2.counts()
			obs := []recoveryObservation{}
			for _, r := range req.Records {
				obs = append(obs, recoveryObservation{Prefix: r.Prefix, Generation: r.Generation, PendingID: r.PendingID, GuardPresent: true, GuardOwned: true, BGPPresent: adds > 0, BGPVerified: adds > 0})
			}
			page := recoveryPage{Version: 1, Challenge: req.Challenge, Epoch: req.Epoch, Sequence: 0, TotalPages: 1, ObjectsComplete: true, OldJobsQuiescent: true, WriterFenced: true, Observations: obs}
			payload, _ := json.Marshal(page)
			post, e := client.Post(agent2.URL+"/guardrecovery/attest", "application/json", bytes.NewReader(payload))
			if e == nil {
				io.Copy(io.Discard, post.Body)
				post.Body.Close()
			}
		}
	}()
	a2.startGuardRecovery()
	until := time.Now().Add(8 * time.Second)
	for {
		restored.mu.Lock()
		ready := !restored.recoveryNeedsReconcile
		restored.mu.Unlock()
		if ready {
			break
		}
		if time.Now().After(until) {
			t.Fatal("two-pass HTTP recovery failed")
		}
		time.Sleep(20 * time.Millisecond)
	}
	close(recoveryStop)
	<-recoveryDone
	close(a2.stop)
	a2.wg.Wait()
	if adds, _ := p2.counts(); adds != 1 {
		t.Fatalf("expected one guarded replay, got %d", adds)
	}
	// Restore batch liveness after recovery, then send a new DNS request.
	health := guardRequest(a2.httpHandler(), http.MethodGet, "/guardwriter/health", nil, "127.0.0.1:1")
	var hs guardHealthSnapshot
	if health.Code != 200 || json.Unmarshal(health.Body.Bytes(), &hs) != nil || len(hs.Entries) != 1 {
		t.Fatalf("post-recovery health: %d %s", health.Code, health.Body.String())
	}
	ack, _ := json.Marshal(map[string]any{"epoch": hs.Epoch, "snapshot_id": hs.SnapshotID, "prefixes": []string{hs.Entries[0].Prefix}})
	if w := guardRequest(a2.httpHandler(), http.MethodPost, "/guardwriter/health-ack", ack, "127.0.0.1:1"); w.Code != 204 {
		t.Fatalf("health ack: %d", w.Code)
	}
	q := new(dns.Msg)
	q.SetQuestion("recovered.example.", dns.TypeA)
	okReply, _, err := (&dns.Client{Timeout: 3 * time.Second}).Exchange(q, dnsAddr)
	if err != nil || okReply.Rcode != dns.RcodeSuccess || len(okReply.Answer) != 1 {
		t.Fatalf("recovered DNS: %v %v", err, okReply)
	}
	t.Log("actual mosdns UDP/TCP DNS, batched guard-before-BGP, lost ACK retry, send completion, restart SERVFAIL and simulated two-pass HTTP recovery passed; ROS forwarding not simulated")
}
