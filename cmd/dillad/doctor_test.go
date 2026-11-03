package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/config"
	"github.com/dilla-chat/dilla/internal/exit"
	"github.com/dilla-chat/dilla/internal/mlswasi"
	"github.com/dilla-chat/dilla/internal/ops"
)

// stubWasi stands in for the artifact a `go test` binary never has beside it.
func stubWasi(t *testing.T, err error) {
	t.Helper()
	prev := doctorWasi
	doctorWasi = func(context.Context) (mlswasi.ABIInfo, error) {
		return mlswasi.ABIInfo{ABIVersion: 3, CoreVersion: "test"}, err
	}
	t.Cleanup(func() { doctorWasi = prev })
}

// clockPeer is an HTTP origin whose Date header is right.
func clockPeer(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Date", time.Now().UTC().Format(http.TimeFormat))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

var (
	peersLine = regexp.MustCompile(`(?m)^  clock_peers = .*$`)
	probeLine = regexp.MustCompile(`(?m)^  turn_probe = true$`)
)

// initDoctorInstance runs `dillad init`, then points doctor.clock_peers at a
// local origin and turns the TURN probe off (there is no TURN server in a unit
// test; the TURN leg's allocation is tested against a real one in internal/ops).
func initDoctorInstance(t *testing.T) (cfgPath, dir string) {
	t.Helper()
	dir = t.TempDir()
	if _, _, err := run(t, "init", "--agree-tos", "--config="+filepath.Join(dir, "dilla.toml"), "--data-dir="+dir, "--domain=chat.example", "--public-ip=203.0.113.7"); err != nil {
		t.Fatalf("init: %v", err)
	}
	// t.TempDir is 0755; the systemd unit gives the real directory 0700 (StateDirectoryMode).
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	cfgPath = filepath.Join(dir, "dilla.toml")
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	doc := peersLine.ReplaceAllString(string(raw), fmt.Sprintf("  clock_peers = ['%s']", clockPeer(t)))
	doc = probeLine.ReplaceAllString(doc, "  turn_probe = false")
	if err := os.WriteFile(cfgPath, []byte(doc), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return cfgPath, dir
}

func exitCode(err error) (exit.Code, bool) {
	var code exit.Code
	ok := errors.As(err, &code)
	return code, ok
}

func TestDoctorExitsUnavailableOnAnyRedLeg(t *testing.T) {
	stubWasi(t, errors.New("no artifact beside the binary"))
	cfgPath, _ := initDoctorInstance(t)

	out, _, err := run(t, "doctor", "--config="+cfgPath)
	if code, ok := exitCode(err); !ok || code != exit.Unavailable {
		t.Fatalf("doctor with a red wasi leg gave %v, want exit.Unavailable (69)\n%s", err, out)
	}
	if !strings.Contains(out, "FAIL  wasi") {
		t.Fatalf("the red leg is not on the report:\n%s", out)
	}
	// The other legs still ran: one red leg must not hide the rest.
	for _, leg := range []string{"config", "database", "clock", "certificate", "turn", "udp", "blobs", "webauthn"} {
		if !strings.Contains(out, leg) {
			t.Fatalf("leg %q is missing from the report:\n%s", leg, out)
		}
	}
}

func TestDoctorExitsZeroOnAYellowOnlyReport(t *testing.T) {
	stubWasi(t, nil)
	cfgPath, _ := initDoctorInstance(t)

	out, _, err := run(t, "doctor", "--config="+cfgPath)
	if err != nil {
		t.Fatalf("doctor with only yellow legs gave %v\n%s", err, out)
	}
	// No certificate has been issued on a fresh instance, and UDP is never
	// probed: both are yellow, and neither fails the command.
	for _, want := range []string{"WARN  certificate", "WARN  udp", "OK    config", "OK    clock", "OK    wasi"} {
		if !strings.Contains(out, want) {
			t.Fatalf("report lacks %q:\n%s", want, out)
		}
	}
	if regexp.MustCompile(`(?m)^FAIL `).MatchString(out) {
		t.Fatalf("a red leg in a report that exited 0:\n%s", out)
	}
	// The report is the stable form: two runs are identical (it carries no
	// timestamps), so a runbook can diff them. The one measured value is the
	// clock leg's median skew, which an HTTP Date header's one-second
	// resolution makes read 0s or 1s from run to run, so it is masked.
	again, _, _ := run(t, "doctor", "--config="+cfgPath)
	median := regexp.MustCompile(`\(median [^)]*\)`)
	if median.ReplaceAllString(again, "(median)") != median.ReplaceAllString(out, "(median)") {
		t.Fatalf("two runs differ:\n%s\n---\n%s", out, again)
	}
}

func TestDoctorNeverTakesTheDataDirectoryLock(t *testing.T) {
	stubWasi(t, nil)
	cfgPath, dir := initDoctorInstance(t)

	// An exclusive hold on dilla.lock is what restore takes, and a serve holds
	// dilla.serve.lock exclusively. A doctor that tried either would be refused.
	lock, err := ops.AcquireLock(dir)
	if err != nil {
		t.Fatalf("AcquireLock: %v", err)
	}
	serveLock, err := ops.AcquireLock(filepath.Join(dir, "serve-stand-in"))
	if err != nil {
		t.Fatalf("AcquireLock: %v", err)
	}
	defer func() { _ = serveLock.Release() }()

	out, _, err := run(t, "doctor", "--config="+cfgPath)
	if err != nil {
		t.Fatalf("doctor beside a live holder of the data directory gave %v\n%s", err, out)
	}
	// And it left the lock as it found it: it is still ours to release, and a
	// second exclusive attempt still fails while we hold it.
	if _, err := ops.AcquireLock(dir); !errors.Is(err, ops.ErrLocked) {
		t.Fatalf("the lock was not still held: %v", err)
	}
	if err := lock.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
}

func TestDoctorEndsWithASummaryLineUnlessQuiet(t *testing.T) {
	stubWasi(t, nil)
	cfgPath, _ := initDoctorInstance(t)
	summary := regexp.MustCompile(`(?m)^(OK|WARN|FAIL) +summary +\d+ checks: \d+ OK, \d+ WARN, \d+ FAIL\n\z`)

	out, _, err := run(t, "doctor", "--config="+cfgPath)
	if err != nil {
		t.Fatalf("doctor: %v\n%s", err, out)
	}
	if !summary.MatchString(out) {
		t.Fatalf("the report does not end with a summary line:\n%s", out)
	}
	quiet, _, err := run(t, "doctor", "--quiet", "--config="+cfgPath)
	if err != nil {
		t.Fatalf("doctor --quiet: %v\n%s", err, quiet)
	}
	if strings.Contains(quiet, "summary") {
		t.Fatalf("--quiet printed the summary:\n%s", quiet)
	}
}

func TestDoctorQuietPrintsOnlyTheLegsThatAreNotOK(t *testing.T) {
	stubWasi(t, nil)
	cfgPath, _ := initDoctorInstance(t)

	out, _, err := run(t, "doctor", "--quiet", "--config="+cfgPath)
	if err != nil {
		t.Fatalf("doctor --quiet: %v\n%s", err, out)
	}
	if strings.Contains(out, "OK    ") {
		t.Fatalf("--quiet printed a green leg:\n%s", out)
	}
	if !strings.Contains(out, "WARN  udp") {
		t.Fatalf("--quiet dropped a yellow leg:\n%s", out)
	}
}

func TestDoctorClockPeerFlagReplacesTheConfiguredPeers(t *testing.T) {
	stubWasi(t, nil)
	cfgPath, _ := initDoctorInstance(t)
	// A configured peer that answers with a clock an hour off: red on its own.
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Date", time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat))
	}))
	t.Cleanup(bad.Close)
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	doc := peersLine.ReplaceAllString(string(raw), fmt.Sprintf("  clock_peers = ['%s']", bad.URL))
	if err := os.WriteFile(cfgPath, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	out, _, err := run(t, "doctor", "--config="+cfgPath)
	if code, ok := exitCode(err); !ok || code != exit.Unavailable || !strings.Contains(out, "FAIL  clock") {
		t.Fatalf("an hour of skew gave %v\n%s", err, out)
	}
	out, _, err = run(t, "doctor", "--config="+cfgPath, "--clock-peer="+clockPeer(t))
	if err != nil || !strings.Contains(out, "OK    clock") {
		t.Fatalf("--clock-peer did not replace doctor.clock_peers: %v\n%s", err, out)
	}
}

func TestTheTURNLegIsYellowWhenTURNIsDisabled(t *testing.T) {
	cfg := config.Default()
	cfg.Instance.Domain = "chat.example"
	cfg.TURN.Enabled = false
	cfg.Derive()
	leg := doctorTURNLeg(t.Context(), cfg)
	if leg.Status != ops.Yellow {
		t.Fatalf("turn.enabled=false = %v (%s), want Yellow: it is the founder's own topology, not a fault", leg.Status, leg.Detail)
	}
}

func TestTheTURNLegIsRedWhenTheSecretFileIsMissing(t *testing.T) {
	cfg := config.Default()
	cfg.Instance.Domain = "chat.example"
	cfg.TURN.Enabled = true
	cfg.TURN.SharedSecretFile = filepath.Join(t.TempDir(), "absent.secret")
	cfg.Derive()
	leg := doctorTURNLeg(t.Context(), cfg)
	if leg.Status != ops.Red || leg.Fix == "" {
		t.Fatalf("a missing secret = %v (%s) fix=%q", leg.Status, leg.Detail, leg.Fix)
	}
}
