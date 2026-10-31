package api

import (
	"context"
	"errors"
	"math"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/jonasthim/dilla/internal/auth"
	"github.com/jonasthim/dilla/internal/clock"
	"github.com/jonasthim/dilla/internal/gateway"
	"github.com/jonasthim/dilla/internal/id"
	"github.com/jonasthim/dilla/internal/server"
	"github.com/jonasthim/dilla/internal/store"
)

// CallMetrics is the stats route's metric surface; *obs.Metrics is one.
type CallMetrics interface {
	StatsReport(relay bool, rttMs uint32, decryptFailures uint64)
}

// StatsSummary is what the diagnostics' calls leg reports over a window: the calls still live among
// those that reported, the reports seen (an ended call's included), how many had a relay selected,
// the decrypt failures and the frames encrypted that devices reported (saturating sums), and the
// median and 95th percentile of the devices' latest round-trip times.
type StatsSummary struct {
	LiveCalls, Reports, RelayReports int
	DecryptFailures, FramesEncrypted uint64
	P50RTTms, P95RTTms               uint32
}

// statsReportInterval is the least time between two accepted reports of one device.
const statsReportInterval = 5 * time.Second

// statsKeep bounds what the route holds when nothing asks for a summary: a device whose last report
// is older is dropped by the next report's sweep, at most once per statsSweepEvery. The diagnostics
// read a 15-minute window, well inside it.
const (
	statsKeep       = time.Hour
	statsSweepEvery = time.Minute
)

const candidateRelay = 3

// statsReport is `[candidate_type, relay_protocol|null, rtt_ms, fraction_lost_permille,
// decrypt_failures, frames_encrypted]`; the last two count since the device's previous report.
type statsReport = gateway.CallStatsReport

// saturatingAdd is a + b, or the largest uint64 where that would wrap.
func saturatingAdd(a, b uint64) uint64 {
	if a > math.MaxUint64-b {
		return math.MaxUint64
	}
	return a + b
}

// deviceStats is one device's reports in one call.
type deviceStats struct {
	at              time.Time
	reports         int
	relayReports    int
	decryptFailures uint64
	framesEncrypted uint64
	rtt             uint32
}

// CallStats is POST /v1/calls/{call_id}/stats (DEV-59, ruling F10): what a device in a call reports
// about its connection, kept in memory only — per call, per device — for the admin diagnostics, and
// counted into label-free series. Nothing is persisted and nothing names a device outside this
// process. The route pushes nothing to the SFU and frees no sharing slot, so it never takes the
// call's lock.
type CallStats struct {
	repo  store.Repository
	calls *Calls
	m     CallMetrics
	clk   clock.Clock

	mu             sync.Mutex
	byCall         map[id.ID]map[id.ID]*deviceStats
	last           map[id.ID]time.Time // device → its last accepted report
	swept          time.Time
	pending        map[id.ID]bool
	resolveSession func(context.Context, []byte) (auth.Session, error)
}

// NewCallStats builds the stats route over the call routes' gates.
func NewCallStats(repo store.Repository, calls *Calls, m CallMetrics, clk clock.Clock) *CallStats {
	return &CallStats{repo: repo, calls: calls, m: m, clk: clk,
		byCall: map[id.ID]map[id.ID]*deviceStats{}, last: map[id.ID]time.Time{}, pending: map[id.ID]bool{}}
}

func (s *CallStats) Register(mux *server.Mux) {
	mux.HandleFunc("POST /v1/calls/{call_id}/stats", s.report)
}

// WithSessionResolver installs the production current-session recheck. Bare handler integrations
// may supply an already-authorized context as before; the composition root always installs it.
func (s *CallStats) WithSessionResolver(resolve func(context.Context, []byte) (auth.Session, error)) *CallStats {
	s.resolveSession = resolve
	return s
}

// report is POST /v1/calls/{call_id}/stats: 204, from a current leaf of a live call's group whose
// user holds connect and whose device may take part in calls, at most one per device per 5 s. The
// interval is checked before anything reads the store (review M3), so a device that floods the
// route costs no database round trip; only an accepted report starts the next interval.
func (s *CallStats) report(w http.ResponseWriter, r *http.Request) {
	session, err := enrolledSession(r)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	if wait := s.wait(session.DeviceID, s.clk.Now()); wait > 0 {
		server.WriteError(w, server.RateLimitedAfter(wait))
		return
	}
	callID, err := server.PathID(r, "call_id")
	if err != nil {
		server.WriteError(w, err)
		return
	}
	var req statsReport
	if err := server.DecodeBody(w, r, maxCBORBody, &req); err != nil {
		server.WriteError(w, err)
		return
	}
	if err := s.AcceptCallStats(r.Context(), session, callID, req); err != nil {
		server.WriteError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// AcceptCallStats is the sole acceptance service for HTTP and WSS. Its reservation is taken
// atomically BEFORE repository work and returned on refusal; only acceptance spends five seconds.
func (s *CallStats) AcceptCallStats(ctx context.Context, sess auth.Session, callID id.ID, req gateway.CallStatsReport) error {
	if sess.Scope != auth.ScopeEnrolled || sess.FactorOwed {
		return server.Errorf(server.CodeUnauthenticated, "")
	}
	if err := req.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	wait := s.waitLocked(sess.DeviceID, s.clk.Now())
	if wait > 0 || s.pending[sess.DeviceID] {
		s.mu.Unlock()
		if wait <= 0 {
			wait = statsReportInterval
		}
		return server.RateLimitedAfter(wait)
	}
	s.pending[sess.DeviceID] = true
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.pending, sess.DeviceID); s.mu.Unlock() }()
	fresh := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if s.resolveSession == nil {
			return nil
		}
		current, err := s.resolveSession(ctx, sess.TokenHash)
		if err != nil {
			return err
		}
		if current.UserID != sess.UserID || current.DeviceID != sess.DeviceID || current.Scope != auth.ScopeEnrolled || current.FactorOwed {
			return server.Errorf(server.CodeUnauthenticated, "")
		}
		return nil
	}
	if err := fresh(); err != nil {
		return err
	}
	_, row, ch, bits, err := s.calls.callForSession(ctx, sess, callID)
	if err != nil {
		return err
	}
	if row.Ended != nil {
		return server.Errorf(server.CodeNotFound, "the call has ended")
	}
	if !bits.Has(PermConnect) {
		return server.Errorf(server.CodeForbidden, "you may not connect to this call")
	}
	if err := s.calls.requireLeafOfCall(ctx, row, sess.DeviceID); err != nil {
		return err
	}
	if err := s.calls.refuseBarred(ctx, s.repo, sess.DeviceID); err != nil {
		return err
	}
	if err := requireCommunityWire(ctx, s.repo, sess.WireVersion, ch.CommunityID); err != nil {
		return err
	}
	// The database may have stalled: do not record after a disconnect, revocation, call end,
	// membership removal or permission change that happened while processing this sample.
	if err := fresh(); err != nil {
		return err
	}
	if _, live, err := s.calls.liveRow(ctx, s.repo, row); err != nil {
		return err
	} else if !live {
		return server.Errorf(server.CodeNotFound, "the call has ended")
	}
	if row.GroupID == nil {
		return server.Errorf(server.CodeLeafNotCurrent, "")
	}
	if err := s.calls.stillEntitled(ctx, sess, ch, *row.GroupID); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	now := s.clk.Now()
	relay := req.CandidateType == candidateRelay
	s.mu.Lock()
	// Checked again under the lock: two reports of one device may have passed the first check at once.
	if wait := s.waitLocked(sess.DeviceID, now); wait > 0 {
		s.mu.Unlock()
		return server.RateLimitedAfter(wait)
	}
	if now.Sub(s.swept) >= statsSweepEvery {
		s.dropOlderLocked(now.Add(-statsKeep))
		s.swept = now
	}
	s.last[sess.DeviceID] = now
	devs := s.byCall[row.CallID]
	if devs == nil {
		devs = map[id.ID]*deviceStats{}
		s.byCall[row.CallID] = devs
	}
	d := devs[sess.DeviceID]
	if d == nil {
		d = &deviceStats{}
		devs[sess.DeviceID] = d
	}
	d.at, d.rtt = now, req.RTTms
	d.reports++
	if relay {
		d.relayReports++
	}
	d.decryptFailures = saturatingAdd(d.decryptFailures, req.DecryptFailures)
	d.framesEncrypted = saturatingAdd(d.framesEncrypted, req.FramesEncrypted)
	s.mu.Unlock()
	if s.m != nil {
		s.m.StatsReport(relay, req.RTTms, req.DecryptFailures)
	}
	return nil
}

// wait is how long dev must still wait before its next report is accepted; 0 when it may report.
func (s *CallStats) wait(dev id.ID, now time.Time) time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.waitLocked(dev, now)
}

func (s *CallStats) waitLocked(dev id.ID, now time.Time) time.Duration {
	if last, ok := s.last[dev]; ok && now.Sub(last) < statsReportInterval {
		return statsReportInterval - now.Sub(last)
	}
	return 0
}

// dropOlderLocked forgets every device whose last report is before cutoff, and every call left with
// none. s.mu is held.
func (s *CallStats) dropOlderLocked(cutoff time.Time) {
	for call, devs := range s.byCall {
		for dev, d := range devs {
			if d.at.Before(cutoff) {
				delete(devs, dev)
			}
		}
		if len(devs) == 0 {
			delete(s.byCall, call)
		}
	}
	for dev, at := range s.last {
		if at.Before(cutoff) {
			delete(s.last, dev)
		}
	}
}

// Summary is the window's view, dropping every device whose last report is older than window. A
// call counts as live only while its record has not ended (review M2); one the store cannot answer
// for is counted as live, since the summary is advisory and an admin request must not fail on it.
func (s *CallStats) Summary(ctx context.Context, window time.Duration) StatsSummary {
	cutoff := s.clk.Now().Add(-window)
	var out StatsSummary
	var rtts []uint32
	s.mu.Lock()
	s.dropOlderLocked(cutoff)
	calls := make([]id.ID, 0, len(s.byCall))
	for call, devs := range s.byCall {
		for _, d := range devs {
			out.Reports += d.reports
			out.RelayReports += d.relayReports
			out.DecryptFailures = saturatingAdd(out.DecryptFailures, d.decryptFailures)
			out.FramesEncrypted = saturatingAdd(out.FramesEncrypted, d.framesEncrypted)
			rtts = append(rtts, d.rtt)
		}
		calls = append(calls, call)
	}
	s.mu.Unlock()
	for _, call := range calls {
		row, err := s.repo.GetVoiceSession(ctx, call)
		if errors.Is(err, store.ErrNotFound) || (err == nil && row.Ended != nil) {
			continue
		}
		out.LiveCalls++
	}
	slices.Sort(rtts)
	out.P50RTTms, out.P95RTTms = percentile(rtts, 50), percentile(rtts, 95)
	return out
}

// percentile is the nearest-rank p-th percentile of sorted.
func percentile(sorted []uint32, p int) uint32 {
	if len(sorted) == 0 {
		return 0
	}
	i := (len(sorted)*p+99)/100 - 1
	return sorted[min(max(i, 0), len(sorted)-1)]
}
