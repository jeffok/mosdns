package main

import (
	"net"
	"testing"
	"time"

	"github.com/miekg/dns"
)

func TestTruncatedUDPUsesTCP(t *testing.T) {
	tcp, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer tcp.Close()
	udp, err := net.ListenPacket("udp", tcp.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer udp.Close()
	handler := dns.HandlerFunc(func(w dns.ResponseWriter, q *dns.Msg) {
		m := new(dns.Msg)
		m.SetReply(q)
		if _, ok := w.RemoteAddr().(*net.UDPAddr); ok {
			m.Truncated = true
		} else {
			m.Answer = []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: q.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 42}, A: net.IPv4(192, 0, 2, 1)}}
		}
		_ = w.WriteMsg(m)
	})
	ts := &dns.Server{Listener: tcp, Handler: handler}
	us := &dns.Server{PacketConn: udp, Handler: handler}
	go ts.ActivateAndServe()
	go us.ActivateAndServe()
	defer ts.Shutdown()
	defer us.Shutdown()
	r := check("example.test", tcp.Addr().String(), time.Second, false)
	if r.Error != "" || !r.UDPTruncated || r.Transport != "udp+tcp" || r.DNS != "NOERROR" || len(r.IPs) != 1 || r.IPs[0] != "192.0.2.1" || r.MinAnswerTTL == nil || *r.MinAnswerTTL != 42 {
		t.Fatalf("unexpected result: %+v", r)
	}
}
