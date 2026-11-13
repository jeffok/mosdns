/*
 * Copyright (C) 2026, jeffok
 *
 * This file is part of mosdns.
 *
 * mosdns is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * mosdns is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <https://www.gnu.org/licenses/>.
 */

package ros_addrlist

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/IrineSistiana/mosdns/v5/coremain"
	"github.com/IrineSistiana/mosdns/v5/pkg/query_context"
	"github.com/miekg/dns"
	"net"
)

func TestPluginRegistered(t *testing.T) {
	if _, ok := coremain.GetPluginType(PluginType); !ok {
		t.Fatal("ros_addrlist 未注册")
	}
}

func TestMixedAnswerWritesOnlyForeign(t *testing.T) {
	srv, puts := newMockROS(t, mockOpts{})
	defer srv.Close()
	tag, conf := newPair(t, srv.URL, "192.0.2.0/24")
	defer conf.Close()

	q := queryWithAnswers(t, "www.example.com.", "192.0.2.10", "8.8.8.8")
	if err := tag.Exec(t.Context(), q); err != nil {
		t.Fatal(err)
	}
	if err := conf.Exec(t.Context(), q); err != nil {
		t.Fatal(err)
	}
	got := puts.snapshot()
	if len(got) != 1 || got[0] != "8.8.8.8" {
		t.Fatalf("写入 = %v，想要只写 8.8.8.8", got)
	}
}

func TestDiscardedGlobalBranchDoesNotWrite(t *testing.T) {
	srv, puts := newMockROS(t, mockOpts{})
	defer srv.Close()
	tag, conf := newPair(t, srv.URL, "192.0.2.0/24")
	defer conf.Close()

	// 全局分支看到海外 IP，但最终应答只用了国内结果。
	global := queryWithAnswers(t, "www.example.com.", "8.8.8.8")
	if err := tag.Exec(t.Context(), global); err != nil {
		t.Fatal(err)
	}
	final := queryWithAnswers(t, "www.example.com.", "192.0.2.10")
	if err := conf.Exec(t.Context(), final); err != nil {
		t.Fatal(err)
	}
	if n := puts.snapshot(); len(n) != 0 {
		t.Fatalf("未选中的全局分支被写入 ROS: %v", n)
	}
}

func TestCacheHitRewritesMissingLease(t *testing.T) {
	srv, puts := newMockROS(t, mockOpts{})
	defer srv.Close()
	tag, conf := newPair(t, srv.URL, "192.0.2.0/24")
	defer conf.Close()

	q := queryWithAnswers(t, "www.google.com.", "1.1.1.1")
	if err := tag.Exec(t.Context(), q); err != nil {
		t.Fatal(err)
	}
	if err := conf.Exec(t.Context(), q); err != nil {
		t.Fatal(err)
	}
	if len(puts.snapshot()) != 1 {
		t.Fatalf("首次写入 = %v", puts.snapshot())
	}
	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/rest/ip/firewall/address-list", nil)
	req.SetBasicAuth("mosdns", "test-pass")
	if resp, err := http.DefaultClient.Do(req); err != nil {
		t.Fatal(err)
	} else {
		resp.Body.Close()
	}
	conf.leases.Delete("1.1.1.1")
	if err := conf.Exec(t.Context(), q); err != nil {
		t.Fatal(err)
	}
	if len(puts.snapshot()) != 2 {
		t.Fatalf("租约丢失后应再次写入，实际 %v", puts.snapshot())
	}
}

func TestSyncBudgetReleasesBeforeWriteFinishes(t *testing.T) {
	srv, puts := newMockROS(t, mockOpts{putDelay: 200 * time.Millisecond})
	defer srv.Close()
	tag, conf := newPair(t, srv.URL, "192.0.2.0/24")
	conf.budget = 40 * time.Millisecond
	defer conf.Close()

	q := queryWithAnswers(t, "www.google.com.", "1.1.1.1")
	if err := tag.Exec(t.Context(), q); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := conf.Exec(t.Context(), q); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 120*time.Millisecond {
		t.Fatalf("确认步骤等待了 %s，超过同步预算", elapsed)
	}
	deadline := time.Now().Add(time.Second)
	for len(puts.snapshot()) == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if len(puts.snapshot()) != 1 {
		t.Fatal("后台写入没有完成")
	}
}

func TestPreloadShortLeaseIsNotFresh(t *testing.T) {
	srv, _ := newMockROS(t, mockOpts{existing: []rosEntry{{
		ID: "*1", Address: "1.1.1.1", Timeout: "3s", Comment: commentPrefix + " www.google.com.",
	}}})
	defer srv.Close()
	_, conf := newPair(t, srv.URL, "192.0.2.0/24")
	defer conf.Close()
	l, ok := conf.leaseOf(netip.MustParseAddr("1.1.1.1"))
	if !ok || l.static {
		t.Fatalf("预热租约 = %+v ok=%v", l, ok)
	}
	if l.expires.After(time.Now().Add(conf.timeout / 2)) {
		t.Fatalf("只剩几秒的条目被当成完整租约: %s", time.Until(l.expires))
	}
}

func TestStaticEntryIsNotModified(t *testing.T) {
	srv, puts := newMockROS(t, mockOpts{existing: []rosEntry{{
		ID: "*9", Address: "9.9.9.9", Timeout: "1h", Comment: "GFW-IP",
	}}})
	defer srv.Close()
	tag, conf := newPair(t, srv.URL, "192.0.2.0/24")
	defer conf.Close()
	q := queryWithAnswers(t, "telegram.org.", "9.9.9.9")
	if err := tag.Exec(t.Context(), q); err != nil || conf.Exec(t.Context(), q) != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if n := puts.snapshot(); len(n) != 0 {
		t.Fatalf("静态条目被改写: %v", n)
	}
}

func TestSingleflightAndBreakerAndNoPasswordLog(t *testing.T) {
	const secret = "super-secret-xyz"
	t.Setenv("ROS_PASS", secret)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, ok := r.BasicAuth(); !ok || u != "mosdns" || p != secret {
			t.Errorf("认证错误")
		}
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte("[]"))
			return
		}
		hits.Add(1)
		http.Error(w, "failure", http.StatusInternalServerError)
	}))
	defer srv.Close()

	var logs strings.Builder
	conf := mustConfirm(t, srv.URL, "192.0.2.0/24", "sf")
	conf.logf = func(format string, args ...any) {
		logs.WriteString(format)
	}
	defer conf.Close()
	tag := mustTag(t, "sf")
	q := queryWithAnswers(t, "www.google.com.", "8.8.4.4")
	_ = tag.Exec(t.Context(), q)

	var wg sync.WaitGroup
	conf.budget = time.Second
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = conf.Exec(t.Context(), q)
		}()
	}
	wg.Wait()
	if hits.Load() != 1 {
		t.Fatalf("同一 IP 并发请求打了 %d 次 ROS，想要 1", hits.Load())
	}

	conf.leases.Delete("8.8.4.4")
	for i := 0; i < 5; i++ {
		_ = conf.Exec(t.Context(), q)
	}
	before := hits.Load()
	_ = conf.Exec(t.Context(), q)
	if hits.Load() != before {
		t.Fatalf("熔断后仍请求了 ROS: %d -> %d", before, hits.Load())
	}
	if strings.Contains(logs.String(), secret) {
		t.Fatalf("日志包含密码: %s", logs.String())
	}
}

func TestParseROSDuration(t *testing.T) {
	d, ok := parseROSDuration("00:00:05")
	if !ok || d != 5*time.Second {
		t.Fatalf("got %s %v", d, ok)
	}
	d, ok = parseROSDuration("1h")
	if !ok || d != time.Hour {
		t.Fatalf("got %s %v", d, ok)
	}
	if _, ok := parseROSDuration("nonsense"); ok {
		t.Fatal("非法时长不应解析成功")
	}
}

func newPair(t *testing.T, server, cidr string) (*tagPlugin, *confirmPlugin) {
	t.Helper()
	resetStores()
	t.Setenv("ROS_PASS", "test-pass")
	key := t.Name()
	return mustTag(t, key), mustConfirm(t, server, cidr, key)
}

func mustTag(t *testing.T, key string) *tagPlugin {
	t.Helper()
	p, err := newTag(&Args{Mode: "tag", SourceKey: key, SourceTTL: "600s"})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func mustConfirm(t *testing.T, server, cidr, key string) *confirmPlugin {
	t.Helper()
	dir := t.TempDir()
	cn := filepath.Join(dir, "cn.list")
	if err := os.WriteFile(cn, []byte(cidr+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := newConfirm(&Args{
		Mode: "confirm", SourceKey: key, Server: server, User: "mosdns",
		PasswdEnv: "ROS_PASS", AddrList: "gfw-proxy", Timeout: "1h",
		SyncBudget: "80ms", Concurrency: 4, Queue: 8, CNIPFile: cn,
	})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func queryWithAnswers(t *testing.T, name string, ips ...string) *query_context.Context {
	t.Helper()
	q := new(dns.Msg)
	q.SetQuestion(dns.Fqdn(name), dns.TypeA)
	ctx := query_context.NewContext(q)
	resp := new(dns.Msg)
	resp.SetReply(q)
	for _, ip := range ips {
		resp.Answer = append(resp.Answer, &dns.A{
			Hdr: dns.RR_Header{Name: dns.Fqdn(name), Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60},
			A:   net.ParseIP(ip),
		})
	}
	ctx.SetResponse(resp)
	return ctx
}

type mockOpts struct {
	putDelay time.Duration
	existing []rosEntry
}

type putLog struct {
	mu  sync.Mutex
	ips []string
}

func (p *putLog) snapshot() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := append([]string(nil), p.ips...)
	return out
}

func newMockROS(t *testing.T, opt mockOpts) (*httptest.Server, *putLog) {
	t.Helper()
	t.Setenv("ROS_PASS", "test-pass")
	log := &putLog{}
	var mu sync.Mutex
	entries := append([]rosEntry(nil), opt.existing...)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, _, ok := r.BasicAuth(); !ok {
			http.Error(w, "auth", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			mu.Lock()
			defer mu.Unlock()
			addr := r.URL.Query().Get("address")
			var out []rosEntry
			for _, e := range entries {
				if addr == "" || e.Address == addr {
					out = append(out, e)
				}
			}
			_ = json.NewEncoder(w).Encode(out)
		case http.MethodPut:
			if opt.putDelay > 0 {
				time.Sleep(opt.putDelay)
			}
			var body rosEntry
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			for _, e := range entries {
				if e.Address == body.Address {
					mu.Unlock()
					http.Error(w, "already have such entry", http.StatusBadRequest)
					return
				}
			}
			body.ID = "*new"
			entries = append(entries, body)
			mu.Unlock()
			log.mu.Lock()
			log.ips = append(log.ips, body.Address)
			log.mu.Unlock()
			_ = json.NewEncoder(w).Encode(body)
		case http.MethodDelete:
			mu.Lock()
			entries = nil
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
		case http.MethodPatch:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		default:
			http.Error(w, "method", http.StatusMethodNotAllowed)
		}
	}))
	return srv, log
}
