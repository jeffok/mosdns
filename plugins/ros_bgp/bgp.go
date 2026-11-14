/*
 * Copyright (C) 2026 jeffok
 * SPDX-License-Identifier: GPL-3.0-or-later
 */
package ros_bgp

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/IrineSistiana/mosdns/v5/pkg/query_context"
	"github.com/IrineSistiana/mosdns/v5/pkg/server"
	"github.com/IrineSistiana/mosdns/v5/plugin/executable/sequence"
	"github.com/miekg/dns"
	"go4.org/netipx"
)

// Provenance belongs to the winning response, not to a qname/IP side table.
type bgpProvenance struct{ at time.Time }
type bgpTagPlugin struct{}

func (*bgpTagPlugin) Exec(_ context.Context, q *query_context.Context) error {
	if q.R() != nil {
		q.StoreValue(query_context.RouteSourceKey, bgpProvenance{at: time.Now()})
	}
	return nil
}

// Direct provenance marks hosts and the accepted CN branch explicitly.
type bgpDirectProvenance struct{}
type bgpDirectTagPlugin struct{}

func (*bgpDirectTagPlugin) Exec(_ context.Context, q *query_context.Context) error {
	if q.R() != nil {
		q.StoreValue(query_context.RouteSourceKey, bgpDirectProvenance{})
	}
	return nil
}

type bgpConfirmPlugin struct {
	endpoint    string
	cn          *netipx.IPSet
	client      *http.Client
	guardWriter bool
}

func newBGPConfirm(a *Args) (*bgpConfirmPlugin, error) {
	u, err := url.Parse(a.Endpoint)
	if err != nil || u.Scheme != "http" || u.Host == "" || u.User != nil || u.Path != "/confirm" || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("ros_bgp: invalid BGP confirmation endpoint")
	}
	cn, err := loadCN(a.CNIPFile)
	if err != nil {
		return nil, err
	}
	budget, err := time.ParseDuration(defaultStr(a.SyncBudget, "6500ms"))
	if err != nil || budget <= 0 || budget > 30*time.Second {
		return nil, fmt.Errorf("ros_bgp: invalid BGP confirmation budget")
	}
	return &bgpConfirmPlugin{endpoint: a.Endpoint, cn: cn, guardWriter: a.GuardWriter, client: &http.Client{Timeout: budget, Transport: &http.Transport{Proxy: nil}}}, nil
}

// These special-use prefixes must never be returned as a usable global route.
var bgpInvalid = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"), netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"), netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"), netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("224.0.0.0/4"), netip.MustParsePrefix("240.0.0.0/4"),
}

func bgpFail(q *query_context.Context) {
	m := new(dns.Msg)
	m.SetRcode(q.Q(), dns.RcodeServerFailure)
	q.SetResponse(m)
}

// capResponseTTL applies to all selected responses, including CN and hosts.
// EDNS OPT TTL carries protocol flags rather than a DNS record lifetime.
func capResponseTTL(r *dns.Msg, max uint32) {
	if r == nil {
		return
	}
	for _, group := range [][]dns.RR{r.Answer, r.Ns, r.Extra} {
		for _, rr := range group {
			if rr.Header().Rrtype != dns.TypeOPT && rr.Header().Ttl > max {
				rr.Header().Ttl = max
			}
		}
	}
}

// responseIPv4 visits address records and SVCB/HTTPS address hints in every section.
func responseIPv4(r *dns.Msg, visit func(net.IP, uint32) bool) bool {
	for _, group := range [][]dns.RR{r.Answer, r.Ns, r.Extra} {
		for _, rr := range group {
			var svc *dns.SVCB
			switch x := rr.(type) {
			case *dns.A:
				if !visit(x.A, x.Hdr.Ttl) {
					return false
				}
			case *dns.SVCB:
				svc = x
			case *dns.HTTPS:
				svc = &x.SVCB
			}
			if svc != nil {
				for _, v := range svc.Value {
					if h, ok := v.(*dns.SVCBIPv4Hint); ok {
						for _, ip := range h.Hint {
							if !visit(ip, svc.Hdr.Ttl) {
								return false
							}
						}
					}
				}
			}
		}
	}
	return true
}

func (p *bgpConfirmPlugin) Exec(ctx context.Context, q *query_context.Context) error {
	r := q.R()
	capResponseTTL(r, 300)
	if r == nil || r.Rcode != dns.RcodeSuccess {
		return nil
	}
	source, ok := q.GetValue(query_context.RouteSourceKey)
	if _, direct := source.(bgpDirectProvenance); ok && direct {
		return nil
	}
	provenance, global := source.(bgpProvenance)
	if !ok || !global || provenance.at.IsZero() {
		// Missing provenance cannot authorize addresses from any DNS section or hint.
		responseIPv4(r, func(_ net.IP, _ uint32) bool { bgpFail(q); return false })
		return nil
	}
	ips := []string{}
	seen := map[netip.Addr]bool{}
	// An answer can have a short-lived SRV/MX/HTTPS record and longer-lived
	// address records in its additional section. The route must outlive the
	// longest eligible A TTL, not just the shortest answer TTL.
	answerTTL := uint32(300)
	for _, rr := range r.Answer {
		if rr.Header().Ttl < answerTTL {
			answerTTL = rr.Header().Ttl
		}
	}
	var routeTTL uint32
	valid := responseIPv4(r, func(raw net.IP, ttl uint32) bool {
		ip, ok := netip.AddrFromSlice(raw.To4())
		if !ok {
			return false
		}
		ip = ip.Unmap()
		for _, invalid := range bgpInvalid {
			if invalid.Contains(ip) {
				return false
			}
		}
		if ip.IsPrivate() || cgnat.Contains(ip) || p.cn.Contains(ip) {
			return true
		}
		if !ip.IsGlobalUnicast() {
			return false
		}
		if ttl > routeTTL {
			routeTTL = ttl
		}
		if !seen[ip] {
			seen[ip] = true
			ips = append(ips, ip.String())
		}
		return true
	})
	if !valid {
		bgpFail(q)
		return nil
	}
	if len(ips) == 0 {
		return nil
	}
	if p.guardWriter && !server.HasSendHooks(ctx) {
		bgpFail(q)
		return nil
	}
	before := uint32(time.Since(provenance.at) / time.Second)
	if answerTTL > 0 && before >= answerTTL {
		bgpFail(q)
		return nil
	}
	requestTTL := routeTTL
	if before >= requestTTL {
		requestTTL = 0
	} else {
		requestTTL -= before
	}
	payload, err := json.Marshal(struct {
		Domain string   `json:"domain"`
		IPs    []string `json:"ips"`
		TTL    uint32   `json:"ttl"`
	}{q.QQuestion().Name, ips, requestTTL})
	if err != nil {
		bgpFail(q)
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint, bytes.NewReader(payload))
	if err != nil {
		bgpFail(q)
		return nil
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := p.client.Do(req)
	if err != nil {
		bgpFail(q)
		return nil
	}
	if p.guardWriter {
		var pin struct {
			PinID string `json:"pin_id"`
		}
		err = json.NewDecoder(io.LimitReader(res.Body, 1024)).Decode(&pin)
		res.Body.Close()
		raw, hexErr := hex.DecodeString(pin.PinID)
		if res.StatusCode != http.StatusOK || err != nil || hexErr != nil || len(raw) != 12 || pin.PinID != strings.ToLower(pin.PinID) || ctx.Err() != nil {
			bgpFail(q)
			return nil
		}
		base := strings.TrimSuffix(p.endpoint, "/confirm")
		callback := func(path string) bool {
			body, _ := json.Marshal(struct {
				PinID string `json:"pin_id"`
			}{pin.PinID})
			callCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(callCtx, http.MethodPost, base+path, bytes.NewReader(body))
			if err != nil {
				return false
			}
			req.Header.Set("Content-Type", "application/json")
			resp, err := p.client.Do(req)
			if err != nil {
				return false
			}
			resp.Body.Close()
			return resp.StatusCode == http.StatusNoContent
		}
		if !server.RegisterSendHooks(ctx, func() bool { return callback("/guardpin/may-send") }, func() { callback("/guardpin/finish-sent") }) {
			bgpFail(q)
			return nil
		}
	} else {
		res.Body.Close()
		if res.StatusCode != http.StatusNoContent || ctx.Err() != nil {
			bgpFail(q)
			return nil
		}
	}
	age := uint32(time.Since(provenance.at) / time.Second)
	if answerTTL > 0 && age >= answerTTL {
		bgpFail(q)
		return nil
	}
	for _, group := range [][]dns.RR{r.Answer, r.Ns, r.Extra} {
		for _, rr := range group {
			if rr.Header().Rrtype == dns.TypeOPT {
				continue
			}
			t := rr.Header().Ttl
			if t > 300 {
				t = 300
			}
			if age >= t {
				t = 0
			} else {
				t -= age
			}
			rr.Header().Ttl = t
		}
	}
	return nil
}

var _ sequence.Executable = (*bgpTagPlugin)(nil)
var _ sequence.Executable = (*bgpDirectTagPlugin)(nil)
var _ sequence.Executable = (*bgpConfirmPlugin)(nil)
