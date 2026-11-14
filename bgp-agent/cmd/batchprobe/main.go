// SPDX-License-Identifier: GPL-3.0-or-later
// batchprobe sends A queries over UDP, retries TC responses over TCP, and tests
// HTTPS against the first returned IPv4. Results are JSON Lines on stdout.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"
)

type result struct {
	Domain       string   `json:"domain"`
	DNS          string   `json:"dns,omitempty"`
	IPs          []string `json:"ips,omitempty"`
	MinAnswerTTL *uint32  `json:"min_answer_ttl,omitempty"`
	Transport    string   `json:"transport"`
	UDPTruncated bool     `json:"udp_truncated"`
	DNSMs        int64    `json:"dns_ms"`
	UDPms        int64    `json:"udp_ms"`
	TCPms        int64    `json:"tcp_ms,omitempty"`
	HTTPStatus   int      `json:"http_status,omitempty"`
	HTTPMs       int64    `json:"http_ms,omitempty"`
	Error        string   `json:"error,omitempty"`
}

func check(name, server string, timeout time.Duration, https bool) result {
	r := result{Domain: name, Transport: "udp"}
	q := new(dns.Msg)
	q.SetQuestion(dns.Fqdn(name), dns.TypeA)
	start := time.Now()
	m, _, err := (&dns.Client{Net: "udp", Timeout: timeout}).Exchange(q, server)
	r.UDPms = time.Since(start).Milliseconds()
	if err == nil && m.Truncated {
		r.UDPTruncated = true
		r.Transport = "udp+tcp"
		tcpStart := time.Now()
		m, _, err = (&dns.Client{Net: "tcp", Timeout: timeout}).Exchange(q, server)
		r.TCPms = time.Since(tcpStart).Milliseconds()
	}
	r.DNSMs = time.Since(start).Milliseconds()
	if err != nil {
		r.Error = "dns: " + err.Error()
		return r
	}
	if m == nil {
		r.Error = "dns: empty response"
		return r
	}
	r.DNS = dns.RcodeToString[m.Rcode]
	for _, rr := range m.Answer {
		if rr.Header().Rrtype == dns.TypeOPT {
			continue
		}
		ttl := rr.Header().Ttl
		if r.MinAnswerTTL == nil || ttl < *r.MinAnswerTTL {
			r.MinAnswerTTL = new(uint32)
			*r.MinAnswerTTL = ttl
		}
		if a, ok := rr.(*dns.A); ok {
			r.IPs = append(r.IPs, a.A.String())
		}
	}
	if !https || m.Rcode != dns.RcodeSuccess || len(r.IPs) == 0 {
		return r
	}
	d := net.Dialer{Timeout: 5 * time.Second}
	tr := &http.Transport{DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		_, port, e := net.SplitHostPort(address)
		if e != nil {
			return nil, e
		}
		return d.DialContext(ctx, network, net.JoinHostPort(r.IPs[0], port))
	}}
	defer tr.CloseIdleConnections()
	c := http.Client{Timeout: 8 * time.Second, Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	start = time.Now()
	resp, err := c.Get("https://" + name + "/")
	r.HTTPMs = time.Since(start).Milliseconds()
	if err != nil {
		r.Error = "https: " + err.Error()
		return r
	}
	r.HTTPStatus = resp.StatusCode
	resp.Body.Close()
	return r
}

func main() {
	domains := flag.String("domains", "/data/domains-200.txt", "one domain per line")
	server := flag.String("dns", "127.0.0.1:53", "DNS server host:port")
	concurrency := flag.Int("concurrency", 3, "maximum simultaneous queries")
	timeout := flag.Duration("timeout", 7*time.Second, "per DNS transport timeout")
	https := flag.Bool("https", true, "perform HTTPS request against first returned IPv4")
	flag.Parse()
	if *concurrency < 1 || *concurrency > 64 || *timeout <= 0 {
		fmt.Fprintln(os.Stderr, "invalid concurrency or timeout")
		os.Exit(2)
	}
	f, err := os.Open(*domains)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	seen := make(map[string]bool)
	var names []string
	for scanner.Scan() {
		name := strings.TrimSpace(scanner.Text())
		if name == "" || strings.HasPrefix(name, "#") {
			continue
		}
		name = strings.TrimSuffix(strings.ToLower(name), ".")
		if !seen[name] {
			names = append(names, name)
			seen[name] = true
		}
	}
	if err := scanner.Err(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	sem := make(chan struct{}, *concurrency)
	var wg sync.WaitGroup
	var mu sync.Mutex
	enc := json.NewEncoder(os.Stdout)
	for _, name := range names {
		wg.Add(1)
		sem <- struct{}{}
		go func(name string) {
			defer wg.Done()
			defer func() { <-sem }()
			row := check(name, *server, *timeout, *https)
			mu.Lock()
			defer mu.Unlock()
			if err := enc.Encode(row); err != nil {
				fmt.Fprintln(os.Stderr, "encode:", err)
			}
		}(name)
	}
	wg.Wait()
}
