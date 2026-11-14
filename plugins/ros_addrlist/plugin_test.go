package ros_addrlist

import (
	"context"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/IrineSistiana/mosdns/v5/pkg/query_context"
	"github.com/miekg/dns"
)

func mustParse(t *testing.T, s string) netip.Addr {
	t.Helper()
	a, err := netip.ParseAddr(s)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func writeTempCN(t *testing.T, lines ...string) string {
	t.Helper()
	f := filepath.Join(t.TempDir(), "cn.txt")
	if err := os.WriteFile(f, []byte(joinLines(lines)), 0o644); err != nil {
		t.Fatal(err)
	}
	return f
}
func joinLines(l []string) string {
	s := ""
	for _, x := range l {
		s += x + "\n"
	}
	return s
}

func newTestWriter(t *testing.T, cnPath string) *addrListWriter {
	t.Helper()
	a := &Args{
		Address:    "127.0.0.1:1",
		Username:   "u",
		Password:   "p",
		ListName:   "gfw-proxy",
		QueueSize:  16,
		Workers:    0, // 手动写，不启 worker
		CNIPFile:   cnPath,
		DedupTTL:   "5m",
		DialTimeout: "1s",
		WriteTimeout: "1s",
		ReconnDelay: "10ms",
	}
	w, err := newAddrListWriter(a)
	if err != nil {
		t.Fatalf("newAddrListWriter: %v", err)
	}
	// 停掉启动的 worker（Workers=0 时已没启，但稳妥起见）
	if w.cancel != nil {
		w.cancel()
	}
	return w
}

func mkResp(ips ...string) *dns.Msg {
	m := new(dns.Msg)
	m.SetQuestion("x.com.", dns.TypeA)
	m.Rcode = dns.RcodeSuccess
	for _, s := range ips {
		m.Answer = append(m.Answer, &dns.A{
			Hdr: dns.RR_Header{Name: "x.com.", Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 300},
			A:   net.ParseIP(s).To4(),
		})
	}
	return m
}

func TestExecFiltersInvalidAndCN(t *testing.T) {
	cnFile := writeTempCN(t, "1.0.0.0/8", "110.0.0.0/8", "114.114.114.114/32")
	w := newTestWriter(t, cnFile)
	defer w.Close()

	ctx := query_context.NewContext(new(dns.Msg))
	ctx.SetResponse(mkResp(
		"8.8.8.8",       // 海外 -> 入队
		"114.114.114.114", // CN -> 跳过
		"10.0.0.1",      // 私网 -> 跳过
		"192.168.1.1",   // 私网 -> 跳过
		"127.0.0.1",     // 环回 -> 跳过
		"224.0.0.1",     // 组播 -> 跳过
	))
	if err := w.Exec(context.Background(), ctx); err != nil {
		t.Fatal(err)
	}
	got := []string{}
	for {
		select {
		case ip := <-w.queue:
			got = append(got, ip)
		default:
			goto done
		}
	}
done:
	if len(got) != 1 || got[0] != "8.8.8.8" {
		t.Fatalf("expected only 8.8.8.8 queued, got %v", got)
	}
}

func TestExecDedup(t *testing.T) {
	cnFile := writeTempCN(t, "1.0.0.0/8")
	w := newTestWriter(t, cnFile)
	defer w.Close()
	ctx := query_context.NewContext(new(dns.Msg))
	ctx.SetResponse(mkResp("8.8.8.8", "8.8.8.8", "9.9.9.9"))
	_ = w.Exec(context.Background(), ctx)
	got := 0
	for {
		select {
		case <-w.queue:
			got++
		default:
			goto d
		}
	}
d:
	if got != 2 {
		t.Fatalf("expected 2 queued (dedup), got %d", got)
	}
}

func TestExecNoAnswerNoPanic(t *testing.T) {
	cnFile := writeTempCN(t, "1.0.0.0/8")
	w := newTestWriter(t, cnFile)
	defer w.Close()
	ctx := query_context.NewContext(new(dns.Msg))
	if err := w.Exec(context.Background(), ctx); err != nil {
		t.Fatal(err)
	}
	m := new(dns.Msg)
	m.SetQuestion("x.com.", dns.TypeA)
	m.Rcode = dns.RcodeNameError
	ctx.SetResponse(m)
	if err := w.Exec(context.Background(), ctx); err != nil {
		t.Fatal(err)
	}
}

func TestSkipIP(t *testing.T) {
	cnFile := writeTempCN(t, "1.0.0.0/8", "110.0.0.0/8")
	w := newTestWriter(t, cnFile)
	defer w.Close()
	cases := []struct {
		ip   string
		skip bool
	}{
		{"8.8.8.8", false},
		{"1.1.1.1", true},  // CN
		{"110.0.0.1", true}, // CN
		{"10.0.0.1", true},
		{"192.168.0.1", true},
		{"127.0.0.1", true},
		{"169.254.1.1", true},
		{"100.64.0.1", true},
		{"224.0.0.1", true},
		{"240.1.2.3", true},
		{"198.18.0.1", true},
		{"0.0.0.1", true},
	}
	for _, c := range cases {
		ip := mustParse(t, c.ip)
		if got := w.skipIP(ip); got != c.skip {
			t.Fatalf("%s skip=%v want %v", c.ip, got, c.skip)
		}
	}
}

func TestDedupExpire(t *testing.T) {
	d := newDedupCache(30 * time.Millisecond)
	if d.Check("1.2.3.4") {
		t.Fatal("first check should not dedup")
	}
	if !d.Check("1.2.3.4") {
		t.Fatal("second check should dedup")
	}
	time.Sleep(50 * time.Millisecond)
	if d.Check("1.2.3.4") {
		t.Fatal("after ttl should not dedup")
	}
}
