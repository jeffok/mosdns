// SPDX-License-Identifier: GPL-3.0-or-later
package ros_bgp

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/IrineSistiana/mosdns/v5/pkg/query_context"
	"github.com/IrineSistiana/mosdns/v5/pkg/server"
	"github.com/miekg/dns"
	"go4.org/netipx"
)

func bgpQuery(ip string, ttl uint32, source string) *query_context.Context {
	m := new(dns.Msg)
	m.SetQuestion("example.com.", dns.TypeA)
	q := query_context.NewContext(m)
	r := new(dns.Msg)
	r.SetReply(q.Q())
	r.Answer = []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: "example.com.", Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: ttl}, A: net.ParseIP(ip)}}
	q.SetResponse(r)
	if source != "" {
		q.StoreValue(query_context.RouteSourceKey, bgpProvenance{at: time.Now()})
	}
	return q
}
func TestBGPConfirmSelectedAndFailClosed(t *testing.T) {
	var calls atomic.Int32
	var status atomic.Int32
	status.Store(204)
	var seen struct {
		Domain string   `json:"domain"`
		IPs    []string `json:"ips"`
		TTL    uint32   `json:"ttl"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if err := json.NewDecoder(r.Body).Decode(&seen); err != nil {
			t.Error(err)
		}
		w.WriteHeader(int(status.Load()))
	}))
	defer srv.Close()
	var b netipx.IPSetBuilder
	cn, _ := b.IPSet()
	p := &bgpConfirmPlugin{endpoint: srv.URL + "/confirm", cn: cn, client: srv.Client()}
	q := bgpQuery("8.8.8.8", 600, "global")
	if err := p.Exec(context.Background(), q); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || seen.Domain != "example.com." || len(seen.IPs) != 1 || seen.IPs[0] != "8.8.8.8" || seen.TTL != 300 || q.R().Answer[0].Header().Ttl != 300 {
		t.Fatalf("bad confirm payload/ttl: %+v %+v", seen, q.R())
	}
	q = bgpQuery("8.8.8.8", 60, "")
	_ = p.Exec(context.Background(), q)
	if calls.Load() != 1 || q.R().Rcode != dns.RcodeServerFailure {
		t.Fatal("missing provenance must fail closed")
	}
	q = bgpQuery("8.8.8.8", 60, "")
	q.StoreValue(query_context.RouteSourceKey, bgpDirectProvenance{})
	_ = p.Exec(context.Background(), q)
	if calls.Load() != 1 || q.R().Rcode != dns.RcodeSuccess {
		t.Fatal("explicit direct source must pass without BGP")
	}
	status.Store(503)
	q = bgpQuery("8.8.8.8", 60, "global")
	_ = p.Exec(context.Background(), q)
	if q.R().Rcode != dns.RcodeServerFailure || len(q.R().Answer) != 0 {
		t.Fatal("must fail closed")
	}
	q = bgpQuery("0.0.0.1", 60, "global")
	_ = p.Exec(context.Background(), q)
	if q.R().Rcode != dns.RcodeServerFailure || calls.Load() != 2 {
		t.Fatal("reserved address must not be forwarded or confirmed")
	}
}
func TestBGPConfirmExpiredTTL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { time.Sleep(1100 * time.Millisecond); w.WriteHeader(204) }))
	defer srv.Close()
	var b netipx.IPSetBuilder
	cn, _ := b.IPSet()
	p := &bgpConfirmPlugin{endpoint: srv.URL + "/confirm", cn: cn, client: srv.Client()}
	q := bgpQuery("8.8.8.8", 1, "global")
	_ = p.Exec(context.Background(), q)
	if q.R().Rcode != dns.RcodeServerFailure {
		t.Fatal("expired TTL must fail closed")
	}
}

func TestBGPConfirmAgesTTLFromSelectedResponse(t *testing.T) {
	var seen struct {
		TTL uint32 `json:"ttl"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&seen); err != nil {
			t.Error(err)
		}
		w.WriteHeader(204)
	}))
	defer srv.Close()
	var b netipx.IPSetBuilder
	cn, _ := b.IPSet()
	p := &bgpConfirmPlugin{endpoint: srv.URL + "/confirm", cn: cn, client: srv.Client()}
	q := bgpQuery("8.8.8.8", 10, "global")
	q.StoreValue(query_context.RouteSourceKey, bgpProvenance{at: time.Now().Add(-2 * time.Second)})
	if err := p.Exec(context.Background(), q); err != nil {
		t.Fatal(err)
	}
	if seen.TTL != 8 || q.R().Answer[0].Header().Ttl != 8 {
		t.Fatalf("request TTL %d response TTL %d", seen.TTL, q.R().Answer[0].Header().Ttl)
	}
}

func TestBGPConfirmConfiguredBudget(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { time.Sleep(150 * time.Millisecond); w.WriteHeader(204) }))
	defer srv.Close()
	pth := filepath.Join(t.TempDir(), "cn.txt")
	if err := os.WriteFile(pth, []byte("1.0.1.0/24\n"), 0600); err != nil {
		t.Fatal(err)
	}
	p, err := newBGPConfirm(&Args{Endpoint: srv.URL + "/confirm", CNIPFile: pth, SyncBudget: "40ms"})
	if err != nil {
		t.Fatal(err)
	}
	q := bgpQuery("8.8.8.8", 60, "global")
	start := time.Now()
	if err := p.Exec(context.Background(), q); err != nil {
		t.Fatal(err)
	}
	if q.R().Rcode != dns.RcodeServerFailure || time.Since(start) > time.Second {
		t.Fatal("confirmation timeout must fail closed promptly")
	}
	if _, err := newBGPConfirm(&Args{Endpoint: srv.URL + "/confirm", CNIPFile: pth, SyncBudget: "31s"}); err == nil {
		t.Fatal("unbounded confirm budget accepted")
	}
}

func TestBGPFinalAnswerCapsAllTTLWithoutRaisingShortOrChangingEDNS(t *testing.T) {
	var b netipx.IPSetBuilder
	cn, _ := b.IPSet()
	p := &bgpConfirmPlugin{cn: cn}
	for _, tc := range []struct {
		name      string
		ttl, want uint32
	}{
		{"domestic", 315, 300}, {"static_host", 604800, 300}, {"short_upstream", 42, 42}, {"zero_upstream", 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := bgpQuery("192.168.1.1", tc.ttl, "")
			q.StoreValue(query_context.RouteSourceKey, bgpDirectProvenance{})
			if err := p.Exec(context.Background(), q); err != nil {
				t.Fatal(err)
			}
			if got := q.R().Answer[0].Header().Ttl; got != tc.want {
				t.Fatalf("got TTL %d want %d", got, tc.want)
			}
		})
	}
	qmsg := new(dns.Msg)
	qmsg.SetQuestion("nonexistent.example.", dns.TypeA)
	qmsg.SetEdns0(1232, true)
	q := query_context.NewContext(qmsg)
	beforeOPT := q.RespOpt().Hdr.Ttl
	r := new(dns.Msg)
	r.SetRcode(q.Q(), dns.RcodeNameError)
	r.Ns = []dns.RR{&dns.SOA{Hdr: dns.RR_Header{Name: "example.", Rrtype: dns.TypeSOA, Class: dns.ClassINET, Ttl: 900}, Ns: "ns.example.", Mbox: "host.example.", Minttl: 120}}
	q.SetResponse(r)
	if err := p.Exec(context.Background(), q); err != nil {
		t.Fatal(err)
	}
	if q.R().Ns[0].Header().Ttl != 300 || q.RespOpt().Hdr.Ttl != beforeOPT {
		t.Fatal("negative response TTL or EDNS flags changed incorrectly")
	}
}

func TestBGPConfirmNonAResponses(t *testing.T) {
	for _, tc := range []struct {
		name, record string
		qtype        uint16
	}{
		{"SRV", "example.com. 20 IN SRV 0 0 443 target.example.", dns.TypeSRV},
		{"MX", "example.com. 20 IN MX 10 target.example.", dns.TypeMX},
		{"HTTPS", "example.com. 20 IN HTTPS 1 target.example. ipv4hint=8.8.4.4", dns.TypeHTTPS},
		{"SVCB", "example.com. 20 IN SVCB 1 target.example. ipv4hint=8.8.4.4", dns.TypeSVCB},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls int
			var got struct {
				IPs []string `json:"ips"`
				TTL uint32   `json:"ttl"`
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
					t.Error(err)
				}
				w.WriteHeader(204)
			}))
			defer srv.Close()
			var b netipx.IPSetBuilder
			cn, _ := b.IPSet()
			p := &bgpConfirmPlugin{endpoint: srv.URL + "/confirm", cn: cn, client: srv.Client()}
			msg := new(dns.Msg)
			msg.SetQuestion("example.com.", tc.qtype)
			q := query_context.NewContext(msg)
			resp := new(dns.Msg)
			resp.SetReply(q.Q())
			rr, err := dns.NewRR(tc.record)
			if err != nil {
				t.Fatal(err)
			}
			resp.Answer = []dns.RR{rr}
			for _, ttl := range []uint32{100, 600} {
				resp.Extra = append(resp.Extra, &dns.A{Hdr: dns.RR_Header{Name: "target.example.", Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: ttl}, A: net.ParseIP("8.8.8.8")})
			}
			q.SetResponse(resp)
			q.StoreValue(query_context.RouteSourceKey, bgpProvenance{at: time.Now()})
			if err := p.Exec(context.Background(), q); err != nil {
				t.Fatal(err)
			}
			want := 1
			if tc.qtype == dns.TypeHTTPS || tc.qtype == dns.TypeSVCB {
				want = 2
			}
			if calls != 1 || len(got.IPs) != want || got.TTL != 300 || q.R().Rcode != dns.RcodeSuccess || q.R().Answer[0].Header().Ttl != 20 || q.R().Extra[1].Header().Ttl != 300 {
				t.Fatalf("calls=%d payload=%+v response=%+v", calls, got, q.R())
			}
		})
	}
}

func TestBGPConfirmAdditionalAndHintFailClosed(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls++; w.WriteHeader(503) }))
	defer srv.Close()
	var b netipx.IPSetBuilder
	cn, _ := b.IPSet()
	p := &bgpConfirmPlugin{endpoint: srv.URL + "/confirm", cn: cn, client: srv.Client()}
	for _, tc := range []struct {
		name, rr, source string
		wantCalls        int
	}{
		{"missing provenance additional", "target.example. 60 IN A 8.8.8.8", "", 0},
		{"missing provenance hint", "target.example. 60 IN HTTPS 1 . ipv4hint=8.8.8.8", "", 0},
		{"invalid additional", "target.example. 60 IN A 127.0.0.1", "global", 0},
		{"invalid hint", "target.example. 60 IN HTTPS 1 . ipv4hint=127.0.0.1", "global", 0},
		{"confirm failure", "target.example. 60 IN A 8.8.8.8", "global", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls = 0
			msg := new(dns.Msg)
			msg.SetQuestion("example.com.", dns.TypeMX)
			q := query_context.NewContext(msg)
			resp := new(dns.Msg)
			resp.SetReply(q.Q())
			rr, err := dns.NewRR(tc.rr)
			if err != nil {
				t.Fatal(err)
			}
			resp.Extra = []dns.RR{rr}
			q.SetResponse(resp)
			if tc.source != "" {
				q.StoreValue(query_context.RouteSourceKey, bgpProvenance{at: time.Now()})
			}
			if err := p.Exec(context.Background(), q); err != nil {
				t.Fatal(err)
			}
			if calls != tc.wantCalls || q.R().Rcode != dns.RcodeServerFailure || len(q.R().Extra) != 0 {
				t.Fatalf("calls=%d response=%+v", calls, q.R())
			}
		})
	}
}

func TestBGPConfirmBlocksUntilAckAndFailsOnCancellation(t *testing.T) {
	for _, cancelBeforeAck := range []bool{false, true} {
		name := "ack_before_return"
		if cancelBeforeAck {
			name = "cancel_before_ack"
		}
		t.Run(name, func(t *testing.T) {
			entered := make(chan struct{})
			release := make(chan struct{})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				close(entered)
				<-release
				w.WriteHeader(http.StatusNoContent)
			}))
			defer srv.Close()
			var b netipx.IPSetBuilder
			cn, _ := b.IPSet()
			p := &bgpConfirmPlugin{endpoint: srv.URL + "/confirm", cn: cn, client: srv.Client()}
			q := bgpQuery("8.8.8.8", 60, "global")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- p.Exec(ctx, q) }()
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("confirm was not called")
			}
			select {
			case <-done:
				t.Fatal("returned before route confirmation")
			default:
			}
			if cancelBeforeAck {
				cancel()
			}
			close(release)
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("confirmation did not return")
			}
			want := dns.RcodeSuccess
			if cancelBeforeAck {
				want = dns.RcodeServerFailure
			}
			if q.R().Rcode != want {
				t.Fatalf("got rcode %d, want %d", q.R().Rcode, want)
			}
		})
	}
}

func TestGuardWriterHooks(t *testing.T) {
	for _, tc := range []struct {
		name      string
		hook, may bool
	}{{"no_hook", false, true}, {"successful_write", true, true}, {"may_send_denied", true, false}} {
		t.Run(tc.name, func(t *testing.T) {
			calls := []string{}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls = append(calls, r.URL.Path)
				switch r.URL.Path {
				case "/confirm":
					w.WriteHeader(200)
					_, _ = w.Write([]byte(`{"pin_id":"0123456789abcdef01234567"}`))
				case "/guardpin/may-send":
					if tc.may {
						w.WriteHeader(204)
					} else {
						w.WriteHeader(503)
					}
				case "/guardpin/finish-sent":
					w.WriteHeader(204)
				default:
					w.WriteHeader(404)
				}
			}))
			defer srv.Close()
			var b netipx.IPSetBuilder
			cn, _ := b.IPSet()
			p := &bgpConfirmPlugin{endpoint: srv.URL + "/confirm", cn: cn, client: srv.Client(), guardWriter: true}
			q := bgpQuery("8.8.8.8", 60, "global")
			ctx := context.Background()
			var hooks *server.SendHooks
			if tc.hook {
				ctx, hooks = server.WithSendHooks(ctx)
			}
			if err := p.Exec(ctx, q); err != nil {
				t.Fatal(err)
			}
			if !tc.hook {
				if q.R().Rcode != dns.RcodeServerFailure || len(calls) != 0 {
					t.Fatalf("no writer must fail closed: %v", calls)
				}
				return
			}
			if q.R().Rcode != dns.RcodeSuccess || len(calls) != 1 {
				t.Fatalf("bad reserve: %v", calls)
			}
			if allowed := hooks.Before(); allowed != tc.may {
				t.Fatalf("before = %v", allowed)
			}
			if tc.may {
				hooks.After()
			}
			want := 2
			if tc.may {
				want = 3
			}
			if len(calls) != want {
				t.Fatalf("calls=%v", calls)
			}
		})
	}
}
