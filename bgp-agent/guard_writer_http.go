// SPDX-License-Identifier: GPL-3.0-or-later
package main

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/netip"
)

type guardWriterAck struct {
	guardOperation
	Result string     `json:"result"`
	Proof  guardProof `json:"proof"`
}

// This handler is intentionally unregistered: the coordinator alone cannot
// fence old RouterOS jobs or dispatch BGP withdrawals. Source filtering is a
// deployment boundary, not cryptographic authentication.
func guardWriterHTTP(c *guardCoordinator, peer netip.Addr) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		ip, parseErr := netip.ParseAddr(host)
		if err != nil || parseErr != nil || !peer.IsValid() || ip.Unmap() != peer.Unmap() {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		switch r.URL.Path {
		case "/guardwriter/next":
			if r.Method != http.MethodGet {
				http.Error(w, "method", http.StatusMethodNotAllowed)
				return
			}
			op, err := c.writerNext()
			if err != nil {
				http.Error(w, "writer unavailable", http.StatusServiceUnavailable)
				return
			}
			if op == nil {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(op)
		case "/guardwriter/ack":
			if r.Method != http.MethodPost {
				http.Error(w, "method", http.StatusMethodNotAllowed)
				return
			}
			var ack guardWriterAck
			d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
			d.DisallowUnknownFields()
			if d.Decode(&ack) != nil || d.Decode(new(any)) != io.EOF {
				http.Error(w, "invalid request", http.StatusBadRequest)
				return
			}
			if err := c.writerAck(ack.guardOperation, ack.Result, ack.Proof); err != nil {
				http.Error(w, "ack rejected", http.StatusConflict)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	})
}
