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
