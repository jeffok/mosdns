// SPDX-License-Identifier: GPL-3.0-or-later
package main

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/netip"
)

// guardPinHTTP is deliberately not registered in the production app until
// the DNS writer and router guard lifecycle are both connected. Its caller
// must supply the same explicitly configured client used for /confirm.
func guardPinHTTP(c *guardCoordinator, client netip.Addr) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		ip, parseErr := netip.ParseAddr(host)
		if err != nil || parseErr != nil || !client.IsValid() || ip.Unmap() != client.Unmap() {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "method", http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			ID string `json:"pin_id"`
		}
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&body) != nil || !guardID(body.ID) || decoder.Decode(new(any)) != io.EOF {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		switch r.URL.Path {
		case "/guardpin/may-send":
			if !c.beginSend(body.ID) {
				http.Error(w, "pin unavailable", http.StatusConflict)
				return
			}
		case "/guardpin/finish-sent":
			if c.finishSent(body.ID) != nil {
				http.Error(w, "pin unavailable", http.StatusConflict)
				return
			}
		case "/guardpin/abandon":
			if c.finish(body.ID) != nil {
				http.Error(w, "pin unavailable", http.StatusConflict)
				return
			}
		default:
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}
