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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const commentPrefix = "mosdns-dyn"

type rosEntry struct {
	ID      string `json:".id"`
	Address string `json:"address"`
	Timeout string `json:"timeout"`
	Comment string `json:"comment"`
	List    string `json:"list"`
}

type rosClient struct {
	base  string
	user  string
	pass  string
	list  string
	httpc *http.Client
}

func newROSClient(server, user, pass, list string) *rosClient {
	return &rosClient{
		base: strings.TrimRight(server, "/"),
		user: user,
		pass: pass,
		list: list,
		httpc: &http.Client{
			Transport: &http.Transport{
				MaxIdleConns:        8,
				MaxIdleConnsPerHost: 4,
				IdleConnTimeout:     90 * time.Second,
			},
		},
	}
}

type errDuplicate struct{ body string }

func (e errDuplicate) Error() string { return "already have such entry" }

func (c *rosClient) do(ctx context.Context, method, path string, body any) ([]byte, int, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rdr)
	if err != nil {
		return nil, 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.SetBasicAuth(c.user, c.pass)
	resp, err := c.httpc.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return b, resp.StatusCode, nil
}

func (c *rosClient) Add(ctx context.Context, ip, timeout, comment string) (string, error) {
	b, code, err := c.do(ctx, http.MethodPut, "/rest/ip/firewall/address-list", map[string]string{
		"list":    c.list,
		"address": ip,
		"timeout": timeout,
		"comment": comment,
	})
	if err != nil {
		return "", err
	}
	if code == http.StatusBadRequest && strings.Contains(strings.ToLower(string(b)), "already") {
		return "", errDuplicate{body: string(b)}
	}
	if code < 200 || code >= 300 {
		return "", fmt.Errorf("add %s: status %d", ip, code)
	}
	var e rosEntry
	if err := json.Unmarshal(b, &e); err != nil {
		return "", fmt.Errorf("add %s: decode: %w", ip, err)
	}
	return e.ID, nil
}

func (c *rosClient) PatchTimeout(ctx context.Context, id, timeout string) error {
	_, code, err := c.do(ctx, http.MethodPatch, "/rest/ip/firewall/address-list/"+id, map[string]string{
		"timeout": timeout,
	})
	if err != nil {
		return err
	}
	if code == http.StatusNotFound {
		return errNotFound
	}
	if code < 200 || code >= 300 {
		return fmt.Errorf("patch %s: status %d", id, code)
	}
	return nil
}

var errNotFound = fmt.Errorf("not found")

func (c *rosClient) Find(ctx context.Context, ip string) ([]rosEntry, error) {
	q := url.Values{}
	q.Set("list", c.list)
	q.Set("address", ip)
	q.Set(".proplist", ".id,address,timeout,comment,list")
	b, code, err := c.do(ctx, http.MethodGet, "/rest/ip/firewall/address-list?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	if code < 200 || code >= 300 {
		return nil, fmt.Errorf("find %s: status %d", ip, code)
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return nil, nil
	}
	var list []rosEntry
	if err := json.Unmarshal(b, &list); err != nil {
		return nil, err
	}
	return list, nil
}

func (c *rosClient) List(ctx context.Context) ([]rosEntry, error) {
	q := url.Values{}
	q.Set("list", c.list)
	q.Set(".proplist", ".id,address,timeout,comment,list")
	b, code, err := c.do(ctx, http.MethodGet, "/rest/ip/firewall/address-list?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	if code < 200 || code >= 300 {
		return nil, fmt.Errorf("list: status %d", code)
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return nil, nil
	}
	var list []rosEntry
	if err := json.Unmarshal(b, &list); err != nil {
		return nil, err
	}
	return list, nil
}

func isDynamicComment(comment string) bool {
	return strings.HasPrefix(comment, commentPrefix)
}
