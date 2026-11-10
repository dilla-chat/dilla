package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/botsupervisor"
	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/config"
	"github.com/dilla-chat/dilla/internal/exit"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/ops"
)

// A process that cannot switch users must not quietly run the Bot as itself: that would hand the Bot's keys to the
// server's user. Only the development switch allows it, and doctor flags it.
func TestWithoutSetuidCapabilityBotNeedsDevSameUser(t *testing.T) {
	old := holdsSetuid
	holdsSetuid = func() bool { return false }
	t.Cleanup(func() { holdsSetuid = old })

	cfgPath := bootstrapServeConfig(t)
	rewriteConfig(t, cfgPath, func(c *config.Config) {
		c.Bot.Enabled = true
		c.Bot.DevSameUser = false
		c.Bot.SocketDir = filepath.Join(t.TempDir(), "bot")
	})
	_, _, err := run(t, "serve", "--config="+cfgPath)
	const want = "bot: cannot switch to user dillabot; set [bot] dev_same_user = true for development only"
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("serve = %v, want %q", err, want)
	}
	if code, ok := exitCode(err); !ok || code != exit.Config {
		t.Fatalf("serve exit = %v, want exit.Config (78)", err)
	}

	c := config.Default()
	c.Bot.Enabled = true
	c.Bot.DevSameUser = true
	c.Bot.Executable = os.Args[0]
	c.Bot.HelperExecutable = os.Args[0]
	c.Bot.StateDir = t.TempDir()
	c.Bot.User = currentUserName(t)
	leg := doctorBotLeg(c)
	if leg.Status != ops.Yellow || !strings.Contains(leg.Detail, "dev_same_user") {
		t.Fatalf("doctor's bot leg with dev_same_user = %+v, want WARN naming dev_same_user", leg)
	}
	if text := (ops.Report{Legs: []ops.Leg{leg}}).Text(); !strings.HasPrefix(text, "WARN") {
		t.Fatalf("the leg prints %q, want WARN", text)
	}
}

func currentUserName(t *testing.T) string {
	t.Helper()
	name, err := lookupCurrentUser()
	if err != nil {
		t.Skipf("no name for the current user: %v", err)
	}
	return name
}

func TestDoctorBotLegReportsMissingUserAndExecutable(t *testing.T) {
	c := config.Default()
	if leg := doctorBotLeg(c); leg.Status != ops.Green || !strings.Contains(leg.Detail, "disabled") {
		t.Fatalf("a disabled Bot's leg = %+v, want OK and disabled", leg)
	}
	c.Bot.Enabled = true
	c.Bot.User = "dilla-no-such-user-xyz"
	c.Bot.Executable = filepath.Join(t.TempDir(), "dillabot")
	c.Bot.HelperExecutable = filepath.Join(t.TempDir(), "dilla-bot-core")
	leg := doctorBotLeg(c)
	if leg.Status != ops.Red {
		t.Fatalf("leg = %+v, want FAIL", leg)
	}
	for _, want := range []string{"dilla-no-such-user-xyz", c.Bot.Executable, c.Bot.HelperExecutable} {
		if !strings.Contains(leg.Detail, want) {
			t.Errorf("the leg's detail %q does not name %s", leg.Detail, want)
		}
	}
	if leg.Fix == "" {
		t.Error("a red Bot leg must say how to fix it")
	}
}

// A serving process that runs as root defeats the user separation: with [bot] enabled and no dev_same_user, serve
// and the keeper refuse it and doctor's leg is red (M-8).
func TestRootServeRefusesTheBot(t *testing.T) {
	old := geteuid
	geteuid = func() int { return 0 }
	t.Cleanup(func() { geteuid = old })
	c := config.Default()
	c.Bot.Enabled = true
	if err := refuseRootKeeper(c.Bot); !errors.Is(err, errRootServe) {
		t.Fatalf("refuseRootKeeper as root = %v", err)
	}
	if _, err := startBotKeeper(c, "/nonexistent/dilla.toml"); !errors.Is(err, errRootServe) {
		t.Fatalf("startBotKeeper as root = %v", err)
	}
	oldOwner := dataDirOwner
	t.Cleanup(func() { dataDirOwner = oldOwner })
	dataDirOwner = func(string) (int, bool) { return 0, true }
	if leg := doctorBotLeg(c); leg.Status != ops.Red || !strings.Contains(leg.Detail, "runs as root") {
		t.Fatalf("doctor as root over a root-owned data directory = %+v", leg)
	}
	dataDirOwner = func(string) (int, bool) { return 0, false }
	if leg := doctorBotLeg(c); !strings.Contains(leg.Detail, "runs as root") {
		t.Fatalf("doctor as root over a data directory it cannot judge = %+v", leg)
	}
	// An operator's root shell on a normal install (v0.1.0-bot2 on CT 144): the data directory is dillad's, so the
	// serve is not root and doctor must not send the operator after a problem that does not exist.
	dataDirOwner = func(string) (int, bool) { return 997, true }
	if leg := doctorBotLeg(c); strings.Contains(leg.Detail, "runs as root") {
		t.Fatalf("doctor as root over dillad's data directory blamed the serve: %+v", leg)
	}
	c.Bot.DevSameUser = true
	if err := refuseRootKeeper(c.Bot); err != nil {
		t.Fatalf("dev_same_user as root = %v", err)
	}
	// The one way past it, botsupervisor.UnsafeTestBuild, is the dilla_unsafe_test build tag, which only
	// TestServingDilladHoldsNoSetuidCapability builds with: never this binary.
	if botsupervisor.UnsafeTestBuild() {
		t.Fatal("the test binary was built with the dilla_unsafe_test tag")
	}
}

// The dilla_unsafe_test build tag appears in no tracked file but the ones that define it, the one test that builds
// with it, this guard, and the release jobs that fail a dillad carrying it (and the checker that pins those steps).
// A Makefile, a package.json script, a workflow's GOFLAGS or a deploy script that named it would be refused here.
func TestUnsafeTestTagStaysInItsTests(t *testing.T) {
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git on PATH")
	}
	root, err := exec.CommandContext(t.Context(), gitBin, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Skipf("not a git checkout: %v", err)
	}
	top := strings.TrimSpace(string(root))
	out, err := exec.CommandContext(t.Context(), gitBin, "-C", top, "ls-files", "-z").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	allowed := map[string]bool{
		"internal/botsupervisor/unsafe.go":       true,
		"internal/botsupervisor/unsafe_on.go":    true,
		"internal/botsupervisor/unsafe_off.go":   true,
		"internal/botsupervisor/keeper_linux.go": true,
		"cmd/dillad/botkeeper.go":                true,
		"cmd/dillad/botkeeper_linux_test.go":     true,
		"cmd/dillad/botkeeper_test.go":           true,
		".github/workflows/release.yml":          true,
		".github/workflows/server.yml":           true,
		"scripts/check-ci-workflow.mjs":          true,
		"scripts/check-ci-workflow.test.mjs":     true,
	}
	const tag = "dilla_unsafe_test"
	scanned := 0
	for _, f := range strings.Split(string(out), "\x00") {
		if f == "" || allowed[f] || strings.HasPrefix(f, ".superpowers/") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(top, f))
		if err != nil {
			continue // a tracked file deleted in the working tree, or a submodule
		}
		scanned++
		if strings.Contains(string(body), tag) {
			t.Errorf("%s names the %s build tag", f, tag)
		}
	}
	if scanned < 100 {
		t.Fatalf("scanned only %d tracked files", scanned)
	}
}

// The internal verbs are dispatched, but are not offered in the usage listing: they are run by dillad itself.
func TestBotVerbsAreInternal(t *testing.T) {
	out, _, err := run(t, "help")
	if err != nil {
		t.Fatal(err)
	}
	for _, verb := range []string{"bot-keeper", "bot-launch"} {
		if strings.Contains(out, verb) {
			t.Errorf("usage lists the internal verb %s", verb)
		}
		if _, ok := internalVerbs()[verb]; !ok {
			t.Errorf("%s is not dispatched", verb)
		}
	}
	// bot-keeper refuses to run without its control descriptor, rather than serving stdin.
	_, _, err = run(t, "bot-keeper", "--config=/nonexistent/dilla.toml")
	if err == nil || errors.Is(err, errHelp) {
		t.Fatalf("bot-keeper without a config = %v", err)
	}
}

// Bot v2 Task 17 fix round 2 (I2): dillad doctor prints "commands failing since <time>" from the failing-since file the
// running dillad keeps, dillad's own stamp.
func TestDoctorBotHealthLegPrintsCommandsFailingSince(t *testing.T) {
	c := config.Default()
	if leg := doctorBotHealthLeg(c); leg.Status != ops.Green || !strings.Contains(leg.Detail, "disabled") {
		t.Fatalf("a disabled Bot's health leg = %+v, want OK and disabled", leg)
	}
	c.Bot.Enabled = true
	c.Instance.DataDir = t.TempDir()
	if leg := doctorBotHealthLeg(c); leg.Status != ops.Green {
		t.Fatalf("no failing-since file: %+v, want OK", leg)
	}
	clk := clock.NewFake(time.Date(2026, 11, 8, 9, 30, 0, 0, time.UTC))
	tracker := api.NewBotHealth(clk)
	tracker.Persist(filepath.Join(c.Instance.DataDir, api.BotHealthFile), nil)
	community := id.New()
	for range 2 {
		tracker.Record(community, api.BotHealthReport{ProbeAt: uint64(clk.Now().Unix())})
		clk.Advance(time.Minute)
	}
	leg := doctorBotHealthLeg(c)
	want := "commands failing since 2026-11-08T09:31:00Z (community " + community.String() + ")"
	if leg.Status != ops.Yellow || leg.Detail != want {
		t.Fatalf("leg = %+v, want WARN %q", leg, want)
	}
}
