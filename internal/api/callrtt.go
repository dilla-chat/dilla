package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/fxamacker/cbor/v2"

	"github.com/jonasthim/dilla/internal/cborx"
	"github.com/jonasthim/dilla/internal/id"
	"github.com/jonasthim/dilla/internal/server"
	"github.com/jonasthim/dilla/internal/sfu"
)

// rttAttribute is the participant attribute a call member's round-trip time to the SFU travels in (Signal task 8c): other
// clients show it on that member's tile. Only dillad writes it, from POST /v1/calls/{call_id}/rtt: no call token may update
// its own attributes (sfu.PublishGrant), because LiveKit bounds one update's size but not the merged map (security review).
const rttAttribute = "dilla.rtt"

// RTTMaxMS is the largest RTT a device may report, in milliseconds.
const RTTMaxMS = 5000

// RTTEvery is how often a device's RTT may change: one accepted report per device per this long (a client reports every
// 2 s at most, and only on a change of 5 ms or more; the slack covers its timer's jitter).
const RTTEvery = 1500 * time.Millisecond

// CallAttributes is the SFU call the RTT route needs beside CallTokens; *sfu.Server and *sfu.Remote are one. An instance
// whose SFU lacks it answers 501.
type CallAttributes interface {
	UpdateAttributes(ctx context.Context, room, identity string, attrs map[string]string) error
}

// rttReport is a device's last accepted report: when, and whether it set a value (a clear sets none).
type rttReport struct {
	at  time.Time
	set bool
}

// rttLimiter remembers each device's last accepted RTT report. Old entries are dropped as the map grows.
type rttLimiter struct {
	mu   sync.Mutex
	last map[id.ID]rttReport
}

// allow decides a report: a value at most once per RTTEvery (else how long to wait); a clear at once when a value is set
// (a pause must not leave a stale ping for the rate limit's sake), and as a no-op (write false) when none is. An allowed
// report is recorded.
func (l *rttLimiter) allow(dev id.ID, set bool, now time.Time) (ok, write bool, wait time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.last == nil {
		l.last = map[id.ID]rttReport{}
	}
	prev, seen := l.last[dev]
	if !set {
		if !seen || !prev.set {
			return true, false, 0
		}
		l.last[dev] = rttReport{at: prev.at, set: false}
		return true, true, 0
	}
	if seen && now.Sub(prev.at) < RTTEvery {
		return false, false, RTTEvery - now.Sub(prev.at)
	}
	if len(l.last) > 4096 {
		for d, r := range l.last {
			if now.Sub(r.at) >= RTTEvery && !r.set {
				delete(l.last, d)
			}
		}
	}
	l.last[dev] = rttReport{at: now, set: true}
	return true, true, 0
}

// rtt is POST /v1/calls/{call_id}/rtt `[ms]` or `[]`: 204 once the SFU carries ms (an integer 0..5000) as this device's
// dilla.rtt attribute in the call's room, or, with `[]`, no longer carries one (the client is paused or reconnecting). The
// device must be a current leaf of the call's group and in its room (else 404); one report per RTTEvery per device
// (429 E_RATE_LIMITED with retry_after_ms). Only the caller's own identity and only that one key are ever written.
func (h *Calls) rtt(w http.ResponseWriter, r *http.Request) {
	s, row, ch, _, err := h.callOf(r)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	var body []cbor.RawMessage
	if err := server.DecodeBody(w, r, maxCBORBody, &body); err != nil {
		server.WriteError(w, err)
		return
	}
	value := ""
	switch len(body) {
	case 0:
	case 1:
		var ms uint64
		if err := cborx.Unmarshal(body[0], &ms); err != nil || ms > RTTMaxMS {
			server.WriteError(w, server.Errorf(server.CodeInvalidRequest, "the body is [] or [ms], ms an integer 0..%d", RTTMaxMS))
			return
		}
		value = strconv.FormatUint(ms, 10)
	default:
		server.WriteError(w, server.Errorf(server.CodeInvalidRequest, "the body is [] or [ms]"))
		return
	}
	if row.Ended != nil {
		server.WriteError(w, server.Errorf(server.CodeNotFound, "the call has ended"))
		return
	}
	if err := requireCommunityWire(r.Context(), h.repo, s.WireVersion, ch.CommunityID); err != nil {
		server.WriteError(w, err)
		return
	}
	if err := h.requireLeafOfCall(r.Context(), row, s.DeviceID); err != nil {
		server.WriteError(w, err)
		return
	}
	attrs, ok := h.sfu.(CallAttributes)
	if h.sfu == nil || !ok {
		server.WriteError(w, notImplemented("this instance runs no SFU (livekit.enabled is false)"))
		return
	}
	allowed, write, wait := h.rtts.allow(s.DeviceID, value != "", h.clk.Now())
	if !allowed {
		server.WriteError(w, server.RateLimitedAfter(wait))
		return
	}
	if !write {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	pctx, cancel := sfuCtx(r.Context())
	err = attrs.UpdateAttributes(pctx, row.LivekitRoom, s.DeviceID.String(), map[string]string{rttAttribute: value})
	cancel()
	if errors.Is(err, sfu.ErrNoParticipant) {
		server.WriteError(w, server.Errorf(server.CodeNotFound, "your device is not in the call's room"))
		return
	}
	if err != nil {
		h.log.ErrorContext(r.Context(), "setting a device's rtt failed", "room", row.LivekitRoom, "device", s.DeviceID, "err", err)
		server.WriteError(w, server.Errorf(server.CodeInternal, "the SFU could not update the attribute"))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
