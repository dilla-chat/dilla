//go:build linux

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Bot v2 Task 17: what `dillabot doctor` can say about the running Bot's commands. The health monitor writes its last
// report to <state_dir>/<installation>/health.txt (owner-only, replaced atomically); doctor reads it. It holds times and
// one flag, no content.

const healthStateFile = "health.txt"

// healthStateStale is how old a report may be before doctor calls it a Bot that is not running.
const healthStateStale = 5 * time.Minute

type healthState struct {
	ProbeOK      bool
	ProbeAt      time.Time
	FailingSince time.Time // zero while commands work
	ReportedAt   time.Time
}

func (h healthState) encode() string {
	unix := func(t time.Time) string {
		if t.IsZero() {
			return "-"
		}
		return strconv.FormatInt(t.Unix(), 10)
	}
	ok := "0"
	if h.ProbeOK {
		ok = "1"
	}
	return "probe_ok=" + ok + "\nprobe_at=" + unix(h.ProbeAt) + "\nfailing_since=" + unix(h.FailingSince) + "\nreported_at=" + unix(h.ReportedAt) + "\n"
}

func decodeHealthState(text string) (healthState, bool) {
	var h healthState
	seen := 0
	for _, line := range strings.Split(strings.TrimSpace(text), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return healthState{}, false
		}
		at := func() (time.Time, bool) {
			if value == "-" {
				return time.Time{}, true
			}
			n, err := strconv.ParseInt(value, 10, 64)
			return time.Unix(n, 0).UTC(), err == nil && n > 0
		}
		var good bool
		switch key {
		case "probe_ok":
			h.ProbeOK, good = value == "1", value == "0" || value == "1"
		case "probe_at":
			h.ProbeAt, good = at()
		case "failing_since":
			h.FailingSince, good = at()
		case "reported_at":
			h.ReportedAt, good = at()
		}
		if !good {
			return healthState{}, false
		}
		seen++
	}
	return h, seen == 4 && !h.ReportedAt.IsZero()
}

// writeHealthState replaces dir/health.txt with h, owner-only.
func writeHealthState(dir string, h healthState) error {
	tmp, err := os.CreateTemp(dir, ".health-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.WriteString(h.encode()); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, filepath.Join(dir, healthStateFile))
}

// doctorCommandsLine is doctor's line about the Bot's commands, from the installations' health files under stateDir,
// and whether it is a failure. "commands failing since <time>" names the earliest failure among the installations.
func doctorCommandsLine(stateDir string, now time.Time) (line string, failed bool) {
	files, _ := filepath.Glob(filepath.Join(stateDir, "*", healthStateFile))
	var failingSince time.Time
	var lastProbe, newest time.Time
	reports := 0
	for _, file := range files {
		raw, err := os.ReadFile(file) //nolint:gosec // a glob of the state directory the Bot owns
		if err != nil {
			continue
		}
		h, ok := decodeHealthState(string(raw))
		if !ok || now.Sub(h.ReportedAt) > healthStateStale {
			continue
		}
		reports++
		if !h.FailingSince.IsZero() && (failingSince.IsZero() || h.FailingSince.Before(failingSince)) {
			failingSince = h.FailingSince
		}
		if h.ProbeAt.After(lastProbe) {
			lastProbe = h.ProbeAt
		}
		if h.ReportedAt.After(newest) {
			newest = h.ReportedAt
		}
	}
	switch {
	case reports == 0:
		return "commands WARN: no recent health report (the Bot is not running, or has run for under 15 s)", false
	case !failingSince.IsZero():
		return "commands failing since " + failingSince.UTC().Format(time.RFC3339), true
	}
	return fmt.Sprintf("commands OK: the last probe was answered at %s", lastProbe.UTC().Format(time.RFC3339)), false
}
