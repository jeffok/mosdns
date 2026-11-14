// SPDX-License-Identifier: GPL-3.0-or-later
package fallback

import (
	"context"
	"errors"
	"github.com/IrineSistiana/mosdns/v5/pkg/query_context"
	"github.com/miekg/dns"
	"go.uber.org/zap"
	"testing"
	"time"
)

type responseExec struct {
	delay  time.Duration
	source string
	fail   bool
}

func (e responseExec) Exec(_ context.Context, q *query_context.Context) error {
	if e.source != "" {
		q.StoreValue(query_context.RouteSourceKey, e.source)
	}
	time.Sleep(e.delay)
	if e.fail {
		return errors.New("fail")
	}
	r := new(dns.Msg)
	r.SetReply(q.Q())
	q.SetResponse(r)
	return nil
}

func TestFallbackProvenanceFollowsWinner(t *testing.T) {
	for _, tc := range []struct {
		name               string
		primary, secondary responseExec
		want               string
	}{
		{"cn_wins_even_if_global_is_ready", responseExec{source: "", delay: 10 * time.Millisecond}, responseExec{source: "global", delay: 50 * time.Millisecond}, ""},
		{"global_wins_when_cn_fails", responseExec{fail: true}, responseExec{source: "global"}, "global"},
		{"cn_wins_after_global_fails", responseExec{fail: true, source: "global"}, responseExec{}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := new(dns.Msg)
			m.SetQuestion("example.com.", dns.TypeA)
			q := query_context.NewContext(m)
			f := &fallback{logger: zap.NewNop(), primary: tc.primary, secondary: tc.secondary, fastFallbackDuration: time.Second, alwaysStandby: true}
			if err := f.doFallback(context.Background(), q); err != nil {
				t.Fatal(err)
			}
			v, _ := q.GetValue(query_context.RouteSourceKey)
			got, _ := v.(string)
			if got != tc.want {
				t.Fatalf("source %q, want %q", got, tc.want)
			}
		})
	}
}
