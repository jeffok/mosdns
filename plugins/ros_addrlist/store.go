/*
 * Copyright (C) 2020-2022, IrineSistiana
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
	"os"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"
	"net/netip"
)

// sourceStore 记录「这个域名的这个 IP 来自全局/AI 解析路径」。
// 只在最终应答里出现、且仍在表中的 IP 才会写 RouterOS。
// fallback 没被选中的分支可以写入本表，但不会因此写 ROS。
type sourceStore struct {
	mu    sync.Mutex
	key   string
	file  string
	items map[string]sourceItem // qname + " " + ip
}

type sourceItem struct {
	Q   string    `json:"q"`
	IP  string    `json:"ip"`
	Exp time.Time `json:"exp"`
}

var (
	storesMu sync.Mutex
	stores   = map[string]*sourceStore{}
)

func sharedStore(key, file string) *sourceStore {
	if key == "" {
		key = "default"
	}
	storesMu.Lock()
	defer storesMu.Unlock()
	if s, ok := stores[key]; ok {
		return s
	}
	s := &sourceStore{key: key, file: file, items: map[string]sourceItem{}}
	if file != "" {
		_ = s.load()
	}
	stores[key] = s
	return s
}

func resetStores() {
	storesMu.Lock()
	stores = map[string]*sourceStore{}
	storesMu.Unlock()
}

func sourceKey(qname string, ip netip.Addr) string {
	return normalizeQName(qname) + " " + ip.String()
}

func normalizeQName(q string) string {
	return strings.ToLower(dns.Fqdn(q))
}

func (s *sourceStore) Remember(qname string, ip netip.Addr, exp time.Time) {
	if !ip.IsValid() {
		return
	}
	s.mu.Lock()
	s.items[sourceKey(qname, ip)] = sourceItem{Q: normalizeQName(qname), IP: ip.String(), Exp: exp}
	file := s.file
	snap := s.snapshotLocked()
	s.mu.Unlock()
	if file != "" {
		_ = writeSourceFile(file, snap)
	}
}

func (s *sourceStore) Has(qname string, ip netip.Addr, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	it, ok := s.items[sourceKey(qname, ip)]
	return ok && it.Exp.After(now)
}

func (s *sourceStore) snapshotLocked() []sourceItem {
	out := make([]sourceItem, 0, len(s.items))
	now := time.Now()
	for k, it := range s.items {
		if !it.Exp.After(now) {
			delete(s.items, k)
			continue
		}
		out = append(out, it)
	}
	return out
}

func (s *sourceStore) load() error {
	b, err := os.ReadFile(s.file)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var items []sourceItem
	if err := json.Unmarshal(b, &items); err != nil {
		return err
	}
	now := time.Now()
	for _, it := range items {
		if !it.Exp.After(now) {
			continue
		}
		ip, err := netip.ParseAddr(it.IP)
		if err != nil {
			continue
		}
		s.items[sourceKey(it.Q, ip)] = it
	}
	return nil
}

func writeSourceFile(file string, items []sourceItem) error {
	b, err := json.Marshal(items)
	if err != nil {
		return err
	}
	tmp := file + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, file)
}
