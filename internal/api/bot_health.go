package api

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
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
//
// Fix round 2 (I2): when the judgement first becomes degraded dillad stamps failing_since with its own clock (the
// report's failing_since is the Bot's and is ignored). The catalog's since shows it while the Bot stays degraded, and
// it is kept in a 0600 file in dillad's data directory (BotHealthFile) so `dillad doctor`, a separate process, prints
// "commands failing since <time>". A restarted dillad reads the file back: a Bot still degraded keeps its time.
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
	// failing is when each degraded community's judgement first became degraded, by dillad's clock; file keeps it.
	failing map[id.ID]time.Time
	file    string
	log     *slog.Logger
	// onChange hears a report that changed the judged state: dillad sends op 54, so members re-read the catalog's
	// status instead of keeping the one they read before (the picker would show the Bot offline until a reload).
	onChange func(community id.ID)
}

// OnChange sets the hook a state-changing report calls, after the tracker's lock is released. Set it before serving.
func (h *BotHealth) OnChange(f func(community id.ID)) { h.mu.Lock(); h.onChange = f; h.mu.Unlock() }

// BotHealthFile is the name of dillad's failing-since file in its data directory.
const BotHealthFile = "bot-health"

// NewBotHealth builds an empty tracker; clk is dillad's clock.
func NewBotHealth(clk clock.Clock) *BotHealth {
	if clk == nil {
		clk = clock.System()
	}
	return &BotHealth{clk: clk, start: clk.Now(), state: map[id.ID]*botHealthState{}, failing: map[id.ID]time.Time{}}
}

// Persist keeps the failing-since times in path (0600, written whole on every change) and reads back what a previous
// run left there: a community whose Bot is judged degraded again keeps its time; one judged ready drops it. A missing
// file is an empty one; an unreadable one is logged and replaced on the next change.
func (h *BotHealth) Persist(path string, log *slog.Logger) {
	failing, err := ReadBotHealthFile(path)
	if err != nil && log != nil {
		log.Warn("bot health: the failing-since file is unreadable; it is rewritten on the next change", "error", err.Error())
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.file, h.log = path, log
	for community, since := range failing {
		h.failing[community] = since
	}
}

// ReadBotHealthFile reads a failing-since file: one "<community hex> <unix seconds>" line per community whose Bot was
// degraded when dillad last judged it. A missing file is empty.
func ReadBotHealthFile(path string) (map[id.ID]time.Time, error) {
	out := map[id.ID]time.Time{}
	raw, err := os.ReadFile(path) //nolint:gosec // G304: dillad's own data directory joined with a constant name
	if errors.Is(err, fs.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	for scanner.Scan() {
		community, at, ok := strings.Cut(strings.TrimSpace(scanner.Text()), " ")
		if !ok {
			return out, fmt.Errorf("%s: malformed line", path)
		}
		cid, err := id.Parse(community)
		if err != nil || cid.IsZero() {
			return out, fmt.Errorf("%s: malformed community id", path)
		}
		seconds, err := strconv.ParseInt(at, 10, 64)
		if err != nil || seconds <= 0 {
			return out, fmt.Errorf("%s: malformed time", path)
		}
		out[cid] = time.Unix(seconds, 0).UTC()
	}
	return out, scanner.Err()
}

// writeLocked replaces the file with the failing set (h.mu held). A failure is logged: doctor then reads the last
// file written, the judgement itself is unaffected.
func (h *BotHealth) writeLocked() {
	if h.file == "" {
		return
	}
	ids := make([]id.ID, 0, len(h.failing))
	for community := range h.failing {
		ids = append(ids, community)
	}
	slices.SortFunc(ids, func(a, b id.ID) int { return bytes.Compare(a[:], b[:]) })
	var b strings.Builder
	for _, community := range ids {
		fmt.Fprintf(&b, "%s %d\n", community.String(), h.failing[community].Unix())
	}
	if err := writeFileAtomic(h.file, []byte(b.String())); err != nil && h.log != nil {
		h.log.Warn("bot health: the failing-since file was not written", "error", err.Error())
	}
}

// writeFileAtomic writes data to path through a 0600 temporary file in the same directory and a rename.
func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

// Record takes one report from the community's Bot.
func (h *BotHealth) Record(community id.ID, r BotHealthReport) {
	now := h.clk.Now()
	h.mu.Lock()
	before, _ := h.statusLocked(community, now)
	defer func() {
		after, _ := h.statusLocked(community, now)
		hook := h.onChange
		h.mu.Unlock()
		if after != before && hook != nil {
			hook(community)
		}
	}()
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
	// failing_since is dillad's own stamp of when the judgement first became degraded (r.FailingSince is the Bot's
	// clock and is never used). A previous run's time stands while the Bot is still degraded.
	at, kept := h.failing[community]
	switch {
	case degraded && !kept:
		h.failing[community] = now
		h.writeLocked()
	case degraded:
		st.since = at
	case kept && r.ProbeOK:
		// Only a passing probe ends it: right after a restart one failed probe is not yet "degraded", but nor is it
		// a recovery.
		delete(h.failing, community)
		h.writeLocked()
	}
}

// FailingSince is when the community's Bot was first judged degraded, by dillad's clock, while it still is.
func (h *BotHealth) FailingSince(community id.ID) (time.Time, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	at, ok := h.failing[community]
	return at, ok
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
	return h.statusLocked(community, now)
}

func (h *BotHealth) statusLocked(community id.ID, now time.Time) (state, since uint64) {
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
