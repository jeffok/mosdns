// SPDX-License-Identifier: GPL-3.0-or-later
package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"sync"
	"time"
)

// recoveryHTTP is bound to the controlled ROS source. IP filtering is not
// cryptographic authentication; deployment must restrict that L2 network and
// prohibit other administrative writers during recovery. The final POST stays
// open, holding the ROS script job until the journal is durably reconciled.
type recoveryHTTP struct {
	c                *guardCoordinator
	peer             netip.Addr
	mu               sync.Mutex
	challenge, epoch string
	records          []recoveryRequestRecord
	pages            []string
	total            int
	snapshot         chan recoverySubmitted
	done             chan bool
	finalCtx         context.Context
	finalDeadline    time.Time
	committed        bool
}
type recoveryRequestRecord struct {
	Prefix        string `json:"prefix"`
	Generation    string `json:"generation"`
	Phase         string `json:"phase"`
	PendingID     string `json:"pending_id"`
	PendingAction string `json:"pending_action"`
}
type recoveryObservation struct {
	Prefix              string `json:"prefix"`
	Generation          string `json:"generation"`
	PendingID           string `json:"pending_id"`
	GuardPresent        bool   `json:"guard_present"`
	GuardOwned          bool   `json:"guard_owned"`
	BGPPresent          bool   `json:"bgp_present"`
	BGPVerified         bool   `json:"bgp_verified"`
	BarrierPresent      bool   `json:"barrier_present"`
	BarrierOwned        bool   `json:"barrier_owned"`
	ConnectionsComplete bool   `json:"connections_complete"`
	NoConnections       bool   `json:"no_connections"`
}
type recoveryPage struct {
	Version           int                   `json:"version"`
	Challenge         string                `json:"challenge"`
	Epoch             string                `json:"epoch"`
	Sequence          int                   `json:"sequence"`
	TotalPages        int                   `json:"total_pages"`
	ObjectsComplete   bool                  `json:"objects_complete"`
	OldJobsQuiescent  bool                  `json:"old_jobs_quiescent"`
	WriterFenced      bool                  `json:"writer_fenced"`
	UnexpectedObjects int                   `json:"unexpected_objects"`
	Observations      []recoveryObservation `json:"observations"`
}
type recoverySubmitted struct {
	att recoveryAttestation
	ctx context.Context
}

func (h *recoveryHTTP) Snapshot(ctx context.Context, challenge string) (recoveryAttestation, error) {
	h.c.mu.Lock()
	epoch := h.c.data.Epoch
	records := make([]recoveryRequestRecord, 0)
	for _, r := range h.c.data.Records {
		pending, action := "", ""
		if r.Pending != nil {
			pending, action = r.Pending.ID, r.Pending.Action
		}
		records = append(records, recoveryRequestRecord{r.Prefix, r.Generation, r.Phase, pending, action})
	}
	h.c.mu.Unlock()
	h.mu.Lock()
	if h.challenge != "" {
		h.mu.Unlock()
		return recoveryAttestation{}, errors.New("recovery already in progress")
	}
	h.challenge = challenge
	h.epoch = epoch
	h.records = records
	h.pages = nil
	h.total = 0
	h.snapshot = make(chan recoverySubmitted, 1)
	h.done = make(chan bool, 1)
	h.committed = false
	h.finalCtx = nil
	ch := h.snapshot
	h.mu.Unlock()
	select {
	case submitted := <-ch:
		h.mu.Lock()
		h.finalCtx = submitted.ctx
		h.finalDeadline = time.Now().Add(40 * time.Second)
		h.mu.Unlock()
		return submitted.att, nil
	case <-ctx.Done():
		return recoveryAttestation{}, ctx.Err()
	}
}
func (h *recoveryHTTP) Verify(ctx context.Context) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.finalCtx == nil || h.finalCtx.Err() != nil || ctx.Err() != nil || !time.Now().Before(h.finalDeadline) {
		return errors.New("router recovery job disconnected")
	}
	return nil
}
func (h *recoveryHTTP) Commit() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.finalCtx == nil || h.finalCtx.Err() != nil || !time.Now().Before(h.finalDeadline) {
		return errors.New("router recovery job disconnected")
	}
	h.committed = true
	return nil
}
func (h *recoveryHTTP) Close() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.done != nil {
		h.done <- h.committed
		h.done = nil
	}
	h.challenge = ""
	return nil
}

func (h *recoveryHTTP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	ip, e := netip.ParseAddr(host)
	if err != nil || e != nil || !h.peer.IsValid() || ip.Unmap() != h.peer.Unmap() {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	switch r.URL.Path {
	case "/guardrecovery/next":
		if r.Method != http.MethodGet {
			http.Error(w, "method", 405)
			return
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.challenge == "" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		page := 0
		if raw := r.URL.Query().Get("page"); raw != "" {
			var err error
			page, err = strconv.Atoi(raw)
			if err != nil || page < 0 {
				http.Error(w, "invalid page", 400)
				return
			}
		}
		total := (len(h.records) + 127) / 128
		if total == 0 {
			total = 1
		}
		if page >= total {
			http.Error(w, "invalid page", 400)
			return
		}
		start := page * 128
		end := start + 128
		if end > len(h.records) {
			end = len(h.records)
		}
		out := struct {
			Version    int                     `json:"version"`
			Challenge  string                  `json:"challenge"`
			Epoch      string                  `json:"epoch"`
			Page       int                     `json:"page"`
			TotalPages int                     `json:"total_pages"`
			Records    []recoveryRequestRecord `json:"records"`
		}{1, h.challenge, h.epoch, page, total, h.records[start:end]}
		b, err := json.Marshal(out)
		if err != nil || len(b) > 60000 {
			http.Error(w, "snapshot request too large", 503)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(b)
	case "/guardrecovery/attest":
		if r.Method != http.MethodPost {
			http.Error(w, "method", 405)
			return
		}
		var p recoveryPage
		d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
		d.DisallowUnknownFields()
		if d.Decode(&p) != nil || d.Decode(new(any)) != io.EOF {
			http.Error(w, "bad page", 400)
			return
		}
		h.mu.Lock()
		if h.challenge == "" || p.Version != 1 || p.Challenge != h.challenge || p.Epoch != h.epoch || p.TotalPages < 1 || p.TotalPages > 64 || p.Sequence < 0 || p.Sequence >= p.TotalPages || len(p.Observations) > 8 || p.UnexpectedObjects < 0 || !p.ObjectsComplete || !p.OldJobsQuiescent || !p.WriterFenced {
			h.mu.Unlock()
			http.Error(w, "invalid page", 409)
			return
		}
		if h.total != 0 && h.total != p.TotalPages {
			h.mu.Unlock()
			http.Error(w, "page count changed", 409)
			return
		}
		b, _ := json.Marshal(p)
		if p.Sequence < len(h.pages) {
			if h.pages[p.Sequence] != string(b) || p.Sequence == p.TotalPages-1 {
				h.mu.Unlock()
				http.Error(w, "duplicate page mismatch", 409)
				return
			}
			h.mu.Unlock()
			w.WriteHeader(204)
			return
		}
		if p.Sequence != len(h.pages) {
			h.mu.Unlock()
			http.Error(w, "out of order", 409)
			return
		}
		h.total = p.TotalPages
		h.pages = append(h.pages, string(b))
		if p.Sequence != p.TotalPages-1 {
			h.mu.Unlock()
			w.WriteHeader(204)
			return
		}
		var obs []guardRecoveryObservation
		extra := 0
		for _, raw := range h.pages {
			var page recoveryPage
			_ = json.Unmarshal([]byte(raw), &page)
			if page.UnexpectedObjects > extra {
				extra = page.UnexpectedObjects
			}
			for _, o := range page.Observations {
				obs = append(obs, guardRecoveryObservation{Prefix: o.Prefix, Generation: o.Generation, PendingID: o.PendingID, GuardPresent: o.GuardPresent, GuardOwned: o.GuardOwned, BGPPresent: o.BGPPresent, BGPVerified: o.BGPVerified, BarrierPresent: o.BarrierPresent, BarrierOwned: o.BarrierOwned, ConnectionsComplete: o.ConnectionsComplete, NoConnections: o.NoConnections})
			}
		}
		s := recoverySubmitted{att: recoveryAttestation{Challenge: p.Challenge, UnexpectedObjects: extra, Snapshot: guardRecoverySnapshot{Epoch: p.Epoch, ObjectsComplete: true, OldJobsQuiescent: true, WriterFenced: true, Observations: obs}}, ctx: r.Context()}
		done := h.done
		ch := h.snapshot
		h.mu.Unlock()
		ch <- s
		select {
		case ok := <-done:
			if ok {
				w.WriteHeader(204)
			} else {
				http.Error(w, "recovery rejected", 409)
			}
		case <-r.Context().Done():
			return
		}
	default:
		http.NotFound(w, r)
	}
}
