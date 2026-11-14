/*
 * Copyright (C) 2026 jeffok
 * SPDX-License-Identifier: GPL-3.0-or-later
 */
package ros_bgp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/netip"
	"net/url"
	"time"

	"github.com/IrineSistiana/mosdns/v5/pkg/query_context"
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
	endpoint string
	cn       *netipx.IPSet
	client   *http.Client
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
	return &bgpConfirmPlugin{endpoint: a.Endpoint, cn: cn, client: &http.Client{Timeout: budget, Transport: &http.Transport{Proxy: nil}}}, nil
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
func (p *bgpConfirmPlugin) Exec(ctx context.Context, q *query_context.Context) error {
	if q.R() == nil || q.R().Rcode != dns.RcodeSuccess {
		return nil
	}
	r := q.R()
	source, ok := q.GetValue(query_context.RouteSourceKey)
	if _, direct := source.(bgpDirectProvenance); ok && direct {
		return nil
	}
	provenance, global := source.(bgpProvenance)
	if !ok || !global || provenance.at.IsZero() {
		// A selected A answer without an explicit source can never bypass routing.
		for _, rr := range r.Answer {
			if _, isA := rr.(*dns.A); isA {
				bgpFail(q)
				break
			}
		}
		return nil
	}
	ips := []string{}
	seen := map[netip.Addr]bool{}
	ttl := uint32(300)
	hasAnswer := false
	for _, rr := range r.Answer {
		hasAnswer = true
		if rr.Header().Ttl < ttl {
			ttl = rr.Header().Ttl
		}
		a, ok := rr.(*dns.A)
		if !ok {
			continue
		}
		ip, ok := netip.AddrFromSlice(a.A.To4())
		if !ok {
			bgpFail(q)
			return nil
		}
		ip = ip.Unmap()
		for _, invalid := range bgpInvalid {
			if invalid.Contains(ip) {
				bgpFail(q)
				return nil
			}
		}
		if ip.IsPrivate() || cgnat.Contains(ip) || p.cn.Contains(ip) {
			continue
		}
		if !ip.IsGlobalUnicast() {
			bgpFail(q)
			return nil
		}
		if !seen[ip] {
			seen[ip] = true
			ips = append(ips, ip.String())
		}
	}
	if !hasAnswer || len(ips) == 0 {
		return nil
	}
	before := uint32(time.Since(provenance.at) / time.Second)
	if ttl > 0 && before >= ttl {
		bgpFail(q)
		return nil
	}
	requestTTL := ttl
	if before > 0 && ttl > 0 {
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
	res.Body.Close()
	if res.StatusCode != http.StatusNoContent {
		bgpFail(q)
		return nil
	}
	age := uint32(time.Since(provenance.at) / time.Second)
	if ttl > 0 && age >= ttl {
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
