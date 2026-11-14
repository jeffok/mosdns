// SPDX-License-Identifier: GPL-3.0-or-later
package ros_bgp

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/IrineSistiana/mosdns/v5/pkg/query_context"
	"github.com/IrineSistiana/mosdns/v5/pkg/server"
	"github.com/miekg/dns"
	"go4.org/netipx"
)

type socketIntegrationHandler struct{ plugin *bgpConfirmPlugin }

func (h *socketIntegrationHandler) Handle(ctx context.Context, req *dns.Msg, _ server.QueryMeta, pack func(*dns.Msg) (*[]byte, error)) *[]byte {
	q := query_context.NewContext(req)
	response := new(dns.Msg)
	response.SetReply(req)
	response.Answer = []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: req.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 30}, A: net.ParseIP("8.8.4.4")}}
	q.SetResponse(response)
	q.StoreValue(query_context.RouteSourceKey, bgpProvenance{at: time.Now()})
	if err := h.plugin.Exec(ctx, q); err != nil {
		return nil
	}
	b, err := pack(q.R())
	if err != nil {
		return nil
	}
	return b
}

// Exercise the plugin and the patched socket writer together, including a
// blocked authorization and an authorization failure, on UDP and TCP.
func TestGuardPinRealSocket(t *testing.T) {
	for _, network := range []string{"udp", "tcp"} {
		for _, allow := range []bool{true, false} {
			name := network + "_deny"
			if allow {
				name = network + "_allow"
			}
			t.Run(name, func(t *testing.T) {
				entered := make(chan struct{})
				release := make(chan struct{})
				finished := make(chan struct{}, 1)
				api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch r.URL.Path {
					case "/confirm":
						w.WriteHeader(http.StatusOK)
						_, _ = w.Write([]byte(`{"pin_id":"0123456789abcdef01234567"}`))
					case "/guardpin/may-send":
						close(entered)
						<-release
						if allow {
							w.WriteHeader(http.StatusNoContent)
						} else {
							w.WriteHeader(http.StatusConflict)
						}
					case "/guardpin/finish-sent":
						finished <- struct{}{}
						w.WriteHeader(http.StatusNoContent)
					default:
						http.NotFound(w, r)
					}
				}))
				defer api.Close()
				var b netipx.IPSetBuilder
				cn, err := b.IPSet()
				if err != nil {
					t.Fatal(err)
				}
				h := &socketIntegrationHandler{plugin: &bgpConfirmPlugin{endpoint: api.URL + "/confirm", cn: cn, client: api.Client(), guardWriter: true}}
				var address string
				if network == "udp" {
					l, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
					if err != nil {
						t.Fatal(err)
					}
					defer l.Close()
					address = l.LocalAddr().String()
					go server.ServeUDP(l, h, server.UDPServerOpts{})
				} else {
					l, err := net.Listen("tcp", "127.0.0.1:0")
					if err != nil {
						t.Fatal(err)
					}
					defer l.Close()
					address = l.Addr().String()
					go server.ServeTCP(l, h, server.TCPServerOpts{})
				}
				result := make(chan error, 1)
				go func() {
					q := new(dns.Msg)
					q.SetQuestion("example.com.", dns.TypeA)
					c := dns.Client{Net: network, Timeout: 800 * time.Millisecond}
					r, _, err := c.Exchange(q, address)
					if err == nil {
						want := dns.RcodeServerFailure
						if allow {
							want = dns.RcodeSuccess
						}
						if r.Rcode != want || (allow && len(r.Answer) != 1) || (!allow && len(r.Answer) != 0) {
							err = &net.DNSError{Err: "bad DNS response"}
						}
					}
					result <- err
				}()
				select {
				case <-entered:
				case <-time.After(time.Second):
					t.Fatal("no send authorization")
				}
				select {
				case err := <-result:
					t.Fatalf("response escaped while authorization blocked: %v", err)
				default:
				}
				close(release)
				select {
				case err := <-result:
					if allow && err != nil {
						t.Fatalf("authorized response failed: %v", err)
					}
					if !allow && err != nil {
						t.Fatalf("denied request did not receive SERVFAIL: %v", err)
					}
				case <-time.After(2 * time.Second):
					t.Fatal("DNS client did not finish")
				}
				if allow {
					select {
					case <-finished:
					case <-time.After(time.Second):
						t.Fatal("missing write completion")
					}
				} else {
					select {
					case <-finished:
						t.Fatal("denied response must not finish as sent")
					default:
					}
				}
			})
		}
	}
}
