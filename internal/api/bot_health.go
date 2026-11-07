package api

import (
	"net/http"
	"sync"
	"time"

	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/server"
)

// Bot v2 Task 17: the Bot's health. The Bot probes its own command path every 60 s and reports every 15 s,
// PUT /v1/bot-runtime/health `[probe_ok, probe_at, queue_depth, oldest_wait_ms, music_start_p95_ms, failing_since|null]`
// (uints; probe_ok is 0 or 1). dillad judges the reports, in memory, per community:
//
//   - degraded when the probe failed twice in a row (two reports carrying distinct probe_at, both not ok), or the
//     oldest waiting command has waited over 10 s, or queue_depth rose across every report of the last 60 s;
//   - offline when no report came for 150 s (counted from dillad's start for a Bot that never reported);
//   - ready otherwise.
//
// The catalog's status (element 3 of GET …/bot/catalog) reads it. Nothing here trusts the Bot's own clock: dillad
// stamps each report when it arrives.
const (
	// BotHealthFailedProbes is the consecutive failed probes that make the Bot degraded.
	BotHealthFailedProbes = 2
	// BotHealthOldestWaitMS is the wait, in ms, over which the Bot is degraded.
	BotHealthOldestWaitMS = 10_000
	// BotHealthGrowthWindow is how long the queue must have grown, report after report, to be degraded.
	BotHealthGrowthWindow = 60 * time.Second
	// botHealthKeep is how long queue samples are kept: the growth window and a report period of slack.
	botHealthKeep = BotHealthGrowthWindow + 30*time.Second
	// BotHealthOfflineAfter is how long without a report marks the Bot offline.
	BotHealthOfflineAfter = 150 * time.Second
	// maxBotHealthSamples bounds the per-community queue samples kept.
	maxBotHealthSamples = 32
)

// The catalog status states.
const botStatusDegraded uint64 = 1

// BotHealthReport is one decoded report.
type BotHealthReport struct {
	ProbeOK         bool
	ProbeAt         uint64
	QueueDepth      uint64
	OldestWaitMS    uint64
	MusicStartP95MS uint64
	FailingSince    *uint64
}

type depthSample struct {
	at    time.Time
	depth uint64
}

type botHealthState struct {
	lastReport time.Time
	lastProbe  uint64
	probeFails int
	samples    []depthSample
	degraded   bool
	since      time.Time // when the current ready or degraded judgement began
}

// BotHealth is dillad's view of each community Bot's health.
type BotHealth struct {
	clk   clock.Clock
	start time.Time
	mu    sync.Mutex
	state map[id.ID]*botHealthState
}

// NewBotHealth builds an empty tracker; clk is dillad's clock.
func NewBotHealth(clk clock.Clock) *BotHealth {
	if clk == nil {
		clk = clock.System()
	}
	return &BotHealth{clk: clk, start: clk.Now(), state: map[id.ID]*botHealthState{}}
}

// Record takes one report from the community's Bot.
func (h *BotHealth) Record(community id.ID, r BotHealthReport) {
	now := h.clk.Now()
	h.mu.Lock()
	defer h.mu.Unlock()
	st := h.state[community]
	if st == nil {
		st = &botHealthState{since: now}
		h.state[community] = st
	}
	st.lastReport = now
	if r.ProbeAt != st.lastProbe {
		// A new probe: a failure counts once however many reports repeat it.
		st.lastProbe = r.ProbeAt
		if r.ProbeOK {
			st.probeFails = 0
		} else {
			st.probeFails++
		}
	}
	st.samples = append(st.samples, depthSample{at: now, depth: r.QueueDepth})
	cutoff := now.Add(-botHealthKeep)
	keep := 0
	for keep < len(st.samples) && st.samples[keep].at.Before(cutoff) {
		keep++
	}
	st.samples = st.samples[keep:]
	if len(st.samples) > maxBotHealthSamples {
		st.samples = st.samples[len(st.samples)-maxBotHealthSamples:]
	}
	degraded := st.probeFails >= BotHealthFailedProbes || r.OldestWaitMS > BotHealthOldestWaitMS || queueGrowing(st.samples, now)
	if degraded != st.degraded {
		st.degraded, st.since = degraded, now
	}
}

// queueGrowing is true when the queue depth rose from each report to the next over the last BotHealthGrowthWindow: from
// the newest report at least that old (so the reports compared span the whole window) up to now.
func queueGrowing(samples []depthSample, now time.Time) bool {
	base := -1
	for i, sample := range samples {
		if now.Sub(sample.at) >= BotHealthGrowthWindow {
			base = i
		}
	}
	if base < 0 || base == len(samples)-1 {
		return false
	}
	for i := base + 1; i < len(samples); i++ {
		if samples[i].depth <= samples[i-1].depth {
			return false
		}
	}
	return true
}

// Status is the community's Bot judged now: botStatusReady, botStatusDegraded or botStatusOffline, and since when. A
// ready Bot with no judgement yet has since 0 (the caller uses the installation's own time).
func (h *BotHealth) Status(community id.ID) (state, since uint64) {
	now := h.clk.Now()
	h.mu.Lock()
	defer h.mu.Unlock()
	st := h.state[community]
	last := h.start
	if st != nil && st.lastReport.After(last) {
		last = st.lastReport
	}
	if now.Sub(last) > BotHealthOfflineAfter {
		return botStatusOffline, uint64(max(last.Unix(), 0))
	}
	if st == nil {
		return botStatusReady, 0
	}
	if st.degraded {
		return botStatusDegraded, uint64(max(st.since.Unix(), 0))
	}
	return botStatusReady, uint64(max(st.since.Unix(), 0))
}

// PutBotRuntimeHealth is PUT /v1/bot-runtime/health (socket only, the community's ready Bot): the Bot's report. 204.
func (d Deps) PutBotRuntimeHealth(w http.ResponseWriter, r *http.Request) {
	_, installation, err := readyBotSession(r, d)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	body, err := server.ReadBody(w, r, botEnvelopeSlack)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	invalid := server.Errorf(server.CodeInvalidRequest, "a health report is [probe_ok(0|1), probe_at, queue_depth, oldest_wait_ms, music_start_p95_ms, failing_since|null]")
	elems, ok := rawArrayOf(body, 6)
	if !ok {
		server.WriteError(w, invalid)
		return
	}
	var fields [5]uint64
	for i := range fields {
		if fields[i], ok = rawUintOf(elems[i]); !ok {
			server.WriteError(w, invalid)
			return
		}
	}
	report := BotHealthReport{ProbeOK: fields[0] == 1, ProbeAt: fields[1], QueueDepth: fields[2], OldestWaitMS: fields[3], MusicStartP95MS: fields[4]}
	if fields[0] > 1 {
		server.WriteError(w, invalid)
		return
	}
	if !isNull(elems[5]) {
		since, ok := rawUintOf(elems[5])
		if !ok {
			server.WriteError(w, invalid)
			return
		}
		report.FailingSince = &since
	}
	d.BotHealth.Record(installation.CommunityID, report)
	w.WriteHeader(http.StatusNoContent)
}
