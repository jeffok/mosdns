// SPDX-License-Identifier: GPL-3.0-or-later
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/miekg/dns"
	"net"
	"net/http"
	"os"
	"time"
)

func probe(args []string) {
	f := flag.NewFlagSet("probe", flag.ExitOnError)
	server := f.String("dns", "127.0.0.1:53", "DNS address")
	name := f.String("name", "github.com", "DNS name")
	url := f.String("url", "https://github.com/", "URL (empty for DNS only)")
	state := f.String("state", "", "state URL")
	f.Parse(args)
	q := new(dns.Msg)
	q.SetQuestion(dns.Fqdn(*name), dns.TypeA)
	start := time.Now()
	r, _, e := (&dns.Client{Timeout: 8 * time.Second}).Exchange(q, *server)
	transport := "udp"
	if e == nil && r.Truncated {
		transport = "tcp"
		r, _, e = (&dns.Client{Net: "tcp", Timeout: 8 * time.Second}).Exchange(q, *server)
	}
	if e != nil {
		fmt.Println(e)
		os.Exit(1)
	}
	fmt.Printf("DNS transport=%s elapsed=%s %s\n", transport, time.Since(start), r)
	if r.Rcode != dns.RcodeSuccess {
		os.Exit(1)
	}
	if *state != "" {
		resp, e := http.Get(*state)
		if e != nil {
			fmt.Println(e)
			os.Exit(1)
		}
		var v any
		e = json.NewDecoder(resp.Body).Decode(&v)
		resp.Body.Close()
		if e != nil {
			fmt.Println(e)
			os.Exit(1)
		}
		b, _ := json.Marshal(v)
		fmt.Println(string(b))
	}
	if *url == "" {
		return
	}
	resolver := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, network, *server)
	}}
	d := &net.Dialer{Timeout: 5 * time.Second, Resolver: resolver}
	c := &http.Client{Timeout: 12 * time.Second, Transport: &http.Transport{DialContext: d.DialContext}}
	start = time.Now()
	resp, e := c.Get(*url)
	if e != nil {
		fmt.Println(e)
		os.Exit(1)
	}
	resp.Body.Close()
	fmt.Printf("HTTPS status=%d elapsed=%s\n", resp.StatusCode, time.Since(start))
	if resp.StatusCode >= 400 {
		os.Exit(1)
	}
}
