package api_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jonasthim/dilla/internal/api"
	"github.com/jonasthim/dilla/internal/auth"
	"github.com/jonasthim/dilla/internal/gateway"
	"github.com/jonasthim/dilla/internal/id"
	"github.com/jonasthim/dilla/internal/server"
)

type fakeCallMetrics struct {
	mu      sync.Mutex
	reports int
	relayed int
	rtts    []uint32
	failed  uint64
}

func (m *fakeCallMetrics) StatsReport(relay bool, rttMs uint32, decryptFailures uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reports++
	if relay {
		m.relayed++
	}
	m.rtts = append(m.rtts, rttMs)
	m.failed += decryptFailures
}

// statsEnv is an open call with the owner's device a leaf and the stats route mounted.
func statsEnv(t *testing.T) (*env, string, string, id.ID, *api.CallStats, *fakeCallMetrics) {
	t.Helper()
	e, ch, tok, group, _, calls := callEnvCalls(t, api.CallsConfig{LiveKitURL: testLiveKitURL})
	seedLeaf(t, e, group, deviceOf(t, e, tok), 3, nil)
	_, body := e.Do(http.MethodPost, "/v1/channels/"+ch.String()+"/calls", tok, []any{})
	callID := decodeCall(t, body).CallID
	m := &fakeCallMetrics{}
	stats := api.NewCallStats(e.Repo, calls, m, e.Clk)
	stats.Register(e.Mux)
	return e, tok, "/v1/calls/" + callID.String() + "/stats", group, stats, m
}

func report(candidate uint64, relayProto any, rtt, lost, failed, encrypted uint64) []any {
	return []any{candidate, relayProto, rtt, lost, failed, encrypted}
}

// DEV-59 / F10: one report per device per 5 s, into the metrics and the in-memory window.
func TestAStatsReportIsAcceptedOncePerFiveSeconds(t *testing.T) {
	e, tok, path, _, stats, m := statsEnv(t)
	if status, body := e.Do(http.MethodPost, path, tok, report(3, uint64(2), 80, 12, 4, 1500)); status != http.StatusNoContent {
		t.Fatalf("a stats report = %d %s", status, e.ErrCode(body))
	}
	status, body := e.Do(http.MethodPost, path, tok, report(3, uint64(2), 90, 0, 0, 1500))
	if status != http.StatusTooManyRequests || e.ErrCode(body) != "E_RATE_LIMITED" {
		t.Fatalf("a second report at once = %d %s, want 429 E_RATE_LIMITED", status, e.ErrCode(body))
	}
	var refusal []any
	mustUnmarshalBody(t, body, &refusal)
	if len(refusal) < 3 || refusal[2] != uint64(5000) {
		t.Fatalf("the refusal = %v, want retry_after_ms 5000", refusal)
	}
	e.Clk.Advance(5 * time.Second)
	if status, _ := e.Do(http.MethodPost, path, tok, report(3, uint64(2), 80, 0, 4, 1500)); status != http.StatusNoContent {
		t.Fatalf("a report 5 s later = %d", status)
	}
	m.mu.Lock()
	if m.reports != 2 || m.relayed != 2 || m.failed != 8 {
		t.Errorf("metrics saw %d reports, %d relayed, %d failures; want 2, 2, 8", m.reports, m.relayed, m.failed)
	}
	m.mu.Unlock()
	s := stats.Summary(t.Context(), 15*time.Minute)
	if s.LiveCalls != 1 || s.Reports != 2 || s.RelayReports != 2 || s.DecryptFailures != 8 || s.FramesEncrypted != 3000 || s.P50RTTms != 80 {
		t.Fatalf("summary = %+v", s)
	}
	e.Clk.Advance(16 * time.Minute)
	if s := stats.Summary(t.Context(), 15*time.Minute); s.Reports != 0 || s.LiveCalls != 0 {
		t.Fatalf("a summary after the window = %+v, want nothing", s)
	}
}

// Review M3: the 5-second check comes before any store read, so a flooding device costs no
// database round trip — a second report at once is 429 even for a call id that names no call.
func TestTheStatsRateCheckComesFirst(t *testing.T) {
	e, tok, path, _, _, _ := statsEnv(t)
	if status, _ := e.Do(http.MethodPost, path, tok, report(0, nil, 10, 0, 0, 0)); status != http.StatusNoContent {
		t.Fatalf("a stats report = %d", status)
	}
	status, body := e.Do(http.MethodPost, "/v1/calls/"+id.New().String()+"/stats", tok, report(0, nil, 10, 0, 0, 0))
	if status != http.StatusTooManyRequests || e.ErrCode(body) != "E_RATE_LIMITED" {
		t.Fatalf("a second report at once, to an unknown call = %d %s, want 429 E_RATE_LIMITED", status, e.ErrCode(body))
	}
	// A refused report records nothing: the device may report again once 5 s have passed since the
	// accepted one.
	e.Clk.Advance(5 * time.Second)
	if status, _ := e.Do(http.MethodPost, path, tok, report(0, nil, 10, 0, 0, 0)); status != http.StatusNoContent {
		t.Fatalf("a report 5 s after the accepted one = %d", status)
	}
}

// Review I2: decrypt_failures and frames_encrypted are bounded per report, so no report can wrap a
// sum — one leaf zeroing another's failures — or poison the Prometheus counter.
func TestTheStatsCountersAreBounded(t *testing.T) {
	e, tok, path, _, stats, m := statsEnv(t)
	for name, body := range map[string][]any{
		"decrypt_failures 1048577":  report(0, nil, 10, 0, 1<<20+1, 0),
		"frames_encrypted 1048577":  report(0, nil, 10, 0, 0, 1<<20+1),
		"decrypt_failures 2^64 - 1": report(0, nil, 10, 0, ^uint64(0), 0),
	} {
		if status, resp := e.Do(http.MethodPost, path, tok, body); status != http.StatusBadRequest || e.ErrCode(resp) != "E_INVALID_REQUEST" {
			t.Errorf("%s = %d %s, want 400 E_INVALID_REQUEST", name, status, e.ErrCode(resp))
		}
	}
	if status, resp := e.Do(http.MethodPost, path, tok, report(0, nil, 10, 0, 1<<20, 1<<20)); status != http.StatusNoContent {
		t.Fatalf("a report at the bound (1048576) = %d %s, want 204", status, e.ErrCode(resp))
	}
	if s := stats.Summary(t.Context(), 15*time.Minute); s.DecryptFailures != 1<<20 || s.FramesEncrypted != 1<<20 {
		t.Fatalf("summary = %+v, want the bound counted once", s)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failed != 1<<20 {
		t.Fatalf("metrics saw %d decrypt failures, want 1048576", m.failed)
	}
}

// The window sums saturate rather than wrap.
func TestTheStatsSumsSaturate(t *testing.T) {
	if got := api.SaturatingAddForTest(^uint64(0)-1, 5); got != ^uint64(0) {
		t.Fatalf("saturating add = %d, want 2^64 - 1", got)
	}
	if got := api.SaturatingAddForTest(2, 3); got != 5 {
		t.Fatalf("saturating add = %d, want 5", got)
	}
}

// Review I4 and I5: the route needs connect (403 E_FORBIDDEN without it, as a share does); a caller
// who cannot see the channel finds no call (404), nor does anyone once it has ended; the body is
// capped.
func TestTheStatsRouteNeedsConnectAndALiveVisibleCall(t *testing.T) {
	e, tok, path, _, _, _ := statsEnv(t)
	_, outsiderTok := e.NewUser("outsider")
	if status, resp := e.Do(http.MethodPost, path, outsiderTok, report(0, nil, 10, 0, 0, 0)); status != http.StatusNotFound || e.ErrCode(resp) != "E_NOT_FOUND" {
		t.Errorf("a report from a user who cannot see the channel = %d %s, want 404 E_NOT_FOUND", status, e.ErrCode(resp))
	}
	big := make([]byte, 65<<10)
	if status, resp := e.DoRaw(http.MethodPost, path, tok, "application/cbor", big); status != http.StatusRequestEntityTooLarge || e.ErrCode(resp) != "E_TOO_LARGE" {
		t.Errorf("a 65 KiB body = %d %s, want 413 E_TOO_LARGE", status, e.ErrCode(resp))
	}

	e2, ch, tok2, group, _, calls := callEnvCalls(t, api.CallsConfig{LiveKitURL: testLiveKitURL})
	seedLeaf(t, e2, group, deviceOf(t, e2, tok2), 3, nil)
	_, body := e2.Do(http.MethodPost, "/v1/channels/"+ch.String()+"/calls", tok2, []any{})
	callID := decodeCall(t, body).CallID
	api.NewCallStats(e2.Repo, calls, &fakeCallMetrics{}, e2.Clk).Register(e2.Mux)
	path2 := "/v1/calls/" + callID.String() + "/stats"
	member, _, memberTok := joinedMember(t, e2, ch, group, "member")
	denyInChannel(t, e2, ch, member, api.PermConnect)
	if status, resp := e2.Do(http.MethodPost, path2, memberTok, report(0, nil, 10, 0, 0, 0)); status != http.StatusForbidden || e2.ErrCode(resp) != "E_FORBIDDEN" {
		t.Errorf("a report from a leaf whose user lost connect = %d %s, want 403 E_FORBIDDEN", status, e2.ErrCode(resp))
	}
	if err := e2.Repo.EndVoiceSession(t.Context(), callID, e2.Clk.Now().Unix()); err != nil {
		t.Fatalf("EndVoiceSession: %v", err)
	}
	if status, resp := e2.Do(http.MethodPost, path2, tok2, report(0, nil, 10, 0, 0, 0)); status != http.StatusNotFound || e2.ErrCode(resp) != "E_NOT_FOUND" {
		t.Errorf("a report once the call has ended = %d %s, want 404 E_NOT_FOUND", status, e2.ErrCode(resp))
	}
}

// Review M2: "live calls" counts the calls that are still live; an ended call's reports stay in the
// window's report counts.
func TestTheSummaryCountsLiveCallsOnly(t *testing.T) {
	e, tok, path, _, stats, _ := statsEnv(t)
	if status, _ := e.Do(http.MethodPost, path, tok, report(0, nil, 10, 0, 0, 0)); status != http.StatusNoContent {
		t.Fatalf("a stats report = %d", status)
	}
	if s := stats.Summary(t.Context(), 15*time.Minute); s.LiveCalls != 1 {
		t.Fatalf("summary of a live call = %+v, want one live call", s)
	}
	callID, err := id.Parse(path[len("/v1/calls/") : len("/v1/calls/")+32])
	if err != nil {
		t.Fatalf("call id: %v", err)
	}
	if err := e.Repo.EndVoiceSession(t.Context(), callID, e.Clk.Now().Unix()); err != nil {
		t.Fatalf("EndVoiceSession: %v", err)
	}
	if s := stats.Summary(t.Context(), 15*time.Minute); s.LiveCalls != 0 || s.Reports != 1 {
		t.Fatalf("summary after the call ended = %+v, want no live call and its one report", s)
	}
}

// The route holds what nobody asks a summary for at most an hour: an accepted report sweeps every
// device whose last report is older (statsKeep), at most once a minute.
func TestTheStatsRouteSweepsReportsOlderThanAnHour(t *testing.T) {
	e, tok, path, group, stats, _ := statsEnv(t)
	if status, _ := e.Do(http.MethodPost, path, tok, report(0, nil, 10, 0, 0, 0)); status != http.StatusNoContent {
		t.Fatalf("owner report = %d", status)
	}
	e.Clk.Advance(61 * time.Minute)
	user := userOf(t, e, tok)
	dev := seedDevices(t, e, user, 1)[0]
	e.sess["later"] = auth.Session{UserID: user, DeviceID: dev, Scope: auth.ScopeEnrolled}
	seedLeaf(t, e, group, dev, 3, nil)
	if status, _ := e.Do(http.MethodPost, path, "later", report(0, nil, 20, 0, 0, 0)); status != http.StatusNoContent {
		t.Fatalf("later report = %d", status)
	}
	if s := stats.Summary(t.Context(), 2*time.Hour); s.Reports != 1 || s.P50RTTms != 20 {
		t.Fatalf("summary over two hours = %+v, want only the later report: the hour-old one was swept", s)
	}
}

// The report is range-checked, kept to a current leaf of a live call, and 404 for anything else.
func TestAStatsReportIsCheckedAndGated(t *testing.T) {
	e, tok, path, _, _, _ := statsEnv(t)
	for name, body := range map[string][]any{
		"candidate_type 4":             report(4, nil, 10, 0, 0, 0),
		"relay_protocol without relay": report(0, uint64(1), 10, 0, 0, 0),
		"relay_protocol 3":             report(3, uint64(3), 10, 0, 0, 0),
		"rtt_ms 60001":                 report(1, nil, 60_001, 0, 0, 0),
		"fraction_lost_permille 1001":  report(1, nil, 10, 1001, 0, 0),
		"five elements":                {uint64(1), nil, uint64(10), uint64(0), uint64(0)},
	} {
		if status, resp := e.Do(http.MethodPost, path, tok, body); status != http.StatusBadRequest || e.ErrCode(resp) != "E_INVALID_REQUEST" {
			t.Errorf("%s = %d %s, want 400 E_INVALID_REQUEST", name, status, e.ErrCode(resp))
		}
	}
	user := userOf(t, e, tok)
	other := seedDevices(t, e, user, 1)[0]
	e.sess["other"] = auth.Session{UserID: user, DeviceID: other, Scope: auth.ScopeEnrolled}
	if status, resp := e.Do(http.MethodPost, path, "other", report(0, nil, 10, 0, 0, 0)); status != http.StatusForbidden || e.ErrCode(resp) != "E_LEAF_NOT_CURRENT" {
		t.Errorf("a report from a device that is no leaf = %d %s, want 403 E_LEAF_NOT_CURRENT", status, e.ErrCode(resp))
	}
	if status, _ := e.Do(http.MethodPost, "/v1/calls/"+id.New().String()+"/stats", tok, report(0, nil, 10, 0, 0, 0)); status != http.StatusNotFound {
		t.Errorf("a report for an unknown call = %d, want 404", status)
	}
	// Task 10's rule for every call path: a revoked or quarantined device takes part in no call,
	// even while its leaf is still in the call group's epoch.
	if err := e.Repo.RevokeDevice(t.Context(), deviceOf(t, e, tok), e.Clk.Now().Unix()); err != nil {
		t.Fatalf("RevokeDevice: %v", err)
	}
	if status, resp := e.Do(http.MethodPost, path, tok, report(0, nil, 10, 0, 0, 0)); status != http.StatusForbidden || e.ErrCode(resp) != "E_FORBIDDEN" {
		t.Errorf("a report from a revoked device = %d %s, want 403 E_FORBIDDEN", status, e.ErrCode(resp))
	}
}

// Summary's percentiles are over the devices' latest round-trip times.
func TestTheSummaryPercentilesAreOverTheDevices(t *testing.T) {
	e, tok, path, group, stats, _ := statsEnv(t)
	user := userOf(t, e, tok)
	devs := seedDevices(t, e, user, 2)
	if status, _ := e.Do(http.MethodPost, path, tok, report(0, nil, 10, 0, 0, 0)); status != http.StatusNoContent {
		t.Fatalf("owner report = %d", status)
	}
	for i, rtt := range []uint64{50, 200} {
		name := []string{"a", "b"}[i]
		e.sess[name] = auth.Session{UserID: user, DeviceID: devs[i], Scope: auth.ScopeEnrolled}
		seedLeaf(t, e, group, devs[i], 3, nil)
		if status, _ := e.Do(http.MethodPost, path, name, report(1, nil, rtt, 0, 0, 0)); status != http.StatusNoContent {
			t.Fatalf("device %s report = %d", name, status)
		}
	}
	if s := stats.Summary(t.Context(), 15*time.Minute); s.Reports != 3 || s.P50RTTms != 50 || s.P95RTTms != 200 || s.RelayReports != 0 {
		t.Fatalf("summary = %+v, want 3 reports, p50 50, p95 200, none relayed", s)
	}
}

func TestStatsAcceptanceSharesAllowanceAcrossHTTPAndGateway(t *testing.T) {
	e, tok, path, _, stats, m := statsEnv(t)
	callID, err := id.Parse(strings.Split(path, "/")[3])
	if err != nil {
		t.Fatal(err)
	}
	sample := gateway.CallStatsReport{RTTms: 42, FramesEncrypted: 10}
	session := e.sess[tok]
	if err := stats.AcceptCallStats(t.Context(), session, callID, sample); err != nil {
		t.Fatal(err)
	}
	status, _ := e.Do(http.MethodPost, path, tok, report(0, nil, 42, 0, 0, 10))
	if status != http.StatusTooManyRequests {
		t.Fatalf("HTTP bypassed WSS budget: %d", status)
	}
	e.Clk.Advance(5 * time.Second)
	if status, _ = e.Do(http.MethodPost, path, tok, report(0, nil, 42, 0, 0, 10)); status != 204 {
		t.Fatalf("HTTP=%d", status)
	}
	if err := stats.AcceptCallStats(t.Context(), session, callID, sample); err == nil {
		t.Fatal("WSS bypassed HTTP budget")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.reports != 2 {
		t.Fatalf("reports=%d", m.reports)
	}
}

func TestStatsAcceptanceAtomicallyAdmitsOneAcrossTransports(t *testing.T) {
	e, tok, path, _, stats, m := statsEnv(t)
	callID, _ := id.Parse(strings.Split(path, "/")[3])
	sample := gateway.CallStatsReport{RTTms: 42}
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			if i%2 == 0 {
				_ = stats.AcceptCallStats(t.Context(), e.sess[tok], callID, sample)
			} else {
				e.Do(http.MethodPost, path, tok, report(0, nil, 42, 0, 0, 0))
			}
		}(i)
	}
	close(start)
	wg.Wait()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.reports != 1 {
		t.Fatalf("reports=%d", m.reports)
	}
}

func TestStatsAcceptanceTransportAuthorityParity(t *testing.T) {
	for _, kind := range []string{"ended", "unknown-call", "absent-leaf", "missing-connect", "hidden", "revoked-device", "factor-owed", "malformed"} {
		t.Run(kind, func(t *testing.T) {
			e, tok, path, group, stats, m := statsEnv(t)
			callID, _ := id.Parse(strings.Split(path, "/")[3])
			sample := gateway.CallStatsReport{RTTms: 42}
			switch kind {
			case "ended":
				if err := e.Repo.EndVoiceSession(t.Context(), callID, e.Clk.Now().Unix()); err != nil {
					t.Fatal(err)
				}
			case "unknown-call":
				callID = id.New()
				path = "/v1/calls/" + callID.String() + "/stats"
			case "absent-leaf":
				s := e.sess[tok]
				s.DeviceID = seedDevices(t, e, s.UserID, 1)[0]
				e.sess[tok] = s
			case "missing-connect":
				row, err := e.Repo.GetVoiceSession(t.Context(), callID)
				if err != nil {
					t.Fatal(err)
				}
				uid, _, memberTok := joinedMember(t, e, row.ChannelID, group, "member")
				denyInChannel(t, e, row.ChannelID, uid, api.PermConnect)
				tok = memberTok
			case "hidden":
				_, tok = e.NewUser("outside")
			case "revoked-device":
				if err := e.Repo.RevokeDevice(t.Context(), deviceOf(t, e, tok), e.Clk.Now().Unix()); err != nil {
					t.Fatal(err)
				}
			case "factor-owed":
				s := e.sess[tok]
				s.FactorOwed = true
				e.sess[tok] = s
			case "malformed":
				sample.RTTms = 60001
			}
			err := stats.AcceptCallStats(t.Context(), e.sess[tok], callID, sample)
			if err == nil {
				t.Fatal("gateway adapter accepted ineligible sample")
			}
			rec := httptest.NewRecorder()
			server.WriteError(rec, err)
			status, body := e.Do(http.MethodPost, path, tok, report(0, nil, uint64(sample.RTTms), 0, 0, 0))
			if status != rec.Code || e.ErrCode(body) != e.ErrCode(rec.Body.Bytes()) {
				t.Fatalf("HTTP %d %s vs WSS %d %s", status, e.ErrCode(body), rec.Code, e.ErrCode(rec.Body.Bytes()))
			}
			m.mu.Lock()
			defer m.mu.Unlock()
			if m.reports != 0 {
				t.Fatal("refused sample counted")
			}
		})
	}
}

func TestStatsReservationPrecedesAuthorityReadsAndRefusalReleasesIt(t *testing.T) {
	e, tok, path, _, stats, _ := statsEnv(t)
	callID, _ := id.Parse(strings.Split(path, "/")[3])
	session := e.sess[tok]
	entered, release := make(chan struct{}), make(chan struct{})
	var reads atomic.Int32
	stats.WithSessionResolver(func(context.Context, []byte) (auth.Session, error) {
		if reads.Add(1) == 1 {
			close(entered)
			<-release
			return auth.Session{}, server.Errorf(server.CodeUnauthenticated, "")
		}
		return session, nil
	})
	done := make(chan error, 1)
	go func() { done <- stats.AcceptCallStats(t.Context(), session, callID, gateway.CallStatsReport{}) }()
	<-entered
	if status, _ := e.Do(http.MethodPost, path, tok, report(0, nil, 0, 0, 0, 0)); status != 429 {
		t.Fatalf("parallel HTTP=%d", status)
	}
	if reads.Load() != 1 {
		t.Fatal("parallel sample performed authority work")
	}
	close(release)
	if err := <-done; err == nil {
		t.Fatal("lost authority accepted")
	}
	if err := stats.AcceptCallStats(t.Context(), session, callID, gateway.CallStatsReport{}); err != nil {
		t.Fatalf("failed reservation was not released: %v", err)
	}
}

func TestStatsRechecksAuthorityAndCallEndBeforeRecording(t *testing.T) {
	for _, change := range []string{"revoked", "owed", "call-end"} {
		t.Run(change, func(t *testing.T) {
			e, tok, path, _, stats, m := statsEnv(t)
			callID, _ := id.Parse(strings.Split(path, "/")[3])
			session := e.sess[tok]
			reads := 0
			stats.WithSessionResolver(func(context.Context, []byte) (auth.Session, error) {
				reads++
				if reads == 2 {
					switch change {
					case "revoked":
						return auth.Session{}, server.Errorf(server.CodeUnauthenticated, "")
					case "owed":
						s := session
						s.FactorOwed = true
						return s, nil
					case "call-end":
						if err := e.Repo.EndVoiceSession(t.Context(), callID, e.Clk.Now().Unix()); err != nil {
							t.Fatal(err)
						}
					}
				}
				return session, nil
			})
			if err := stats.AcceptCallStats(t.Context(), session, callID, gateway.CallStatsReport{}); err == nil {
				t.Fatal("changed authority/call was recorded")
			}
			m.mu.Lock()
			defer m.mu.Unlock()
			if m.reports != 0 {
				t.Fatal("stale sample counted")
			}
		})
	}
}
