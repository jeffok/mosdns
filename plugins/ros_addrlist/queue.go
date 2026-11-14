package ros_addrlist

import (
	"context"
	"sync"
	"time"
)

// dedupCache 记录已入队 IP，短窗内去重，防止同一 IP 高频重复回写。
type dedupCache struct {
	mu    sync.Mutex
	ttl   time.Duration
	items map[string]time.Time
}

func newDedupCache(ttl time.Duration) *dedupCache {
	c := &dedupCache{ttl: ttl, items: make(map[string]time.Time)}
	go c.gc()
	return c
}

// Check 若 IP 在去重窗口内则返回 true（跳过），否则记录并返回 false。
func (c *dedupCache) Check(ip string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if t, ok := c.items[ip]; ok && now.Sub(t) < c.ttl {
		return true
	}
	c.items[ip] = now
	return false
}

func (c *dedupCache) gc() {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for range t.C {
		c.mu.Lock()
		now := time.Now()
		for ip, ts := range c.items {
			if now.Sub(ts) >= c.ttl {
				delete(c.items, ip)
			}
		}
		c.mu.Unlock()
	}
}

// runWorker 从队列取 IP 并写入 ROS，直到 ctx 取消。
func (w *addrListWriter) runWorker(ctx context.Context) {
	defer w.wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case ip := <-w.queue:
			w.writeOne(ctx, ip)
		}
	}
}

func (w *addrListWriter) writeOne(ctx context.Context, ip string) {
	w.rcMu.Lock()
	rc := w.rc
	if rc == nil {
		nrc, err := newROSClient(&w.cfg, w.logger)
		if err != nil {
			w.rcMu.Unlock()
			w.stats.mu.Lock()
			w.stats.writeErr++
			w.stats.mu.Unlock()
			return
		}
		w.rc = nrc
		rc = nrc
	}
	w.rcMu.Unlock()

	if err := rc.Add(ctx, ip); err != nil {
		w.stats.mu.Lock()
		w.stats.writeErr++
		w.stats.mu.Unlock()
		return
	}
	w.stats.mu.Lock()
	w.stats.written++
	w.stats.mu.Unlock()
}
