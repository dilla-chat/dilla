package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dilla-chat/dilla/internal/botsupervisor"
	"github.com/dilla-chat/dilla/internal/config"
	"github.com/dilla-chat/dilla/internal/exit"
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
	if leg := doctorBotLeg(c); leg.Status != ops.Red || !strings.Contains(leg.Detail, "runs as root") {
		t.Fatalf("doctor as root = %+v", leg)
	}
	c.Bot.DevSameUser = true
	if err := refuseRootKeeper(c.Bot); err != nil {
		t.Fatalf("dev_same_user as root = %v", err)
	}
	// The one way past it, botsupervisor.UnsafeTestBuild, is a link-time flag only TestServingDilladHoldsNoSetuidCapability
	// sets: never this binary, and never a workflow or script that builds a release.
	if botsupervisor.UnsafeTestBuild() {
		t.Fatal("the test binary is linked as an unsafe test build")
	}
	for _, glob := range []string{"../../.github/workflows/*.yml", "../../scripts/*", "../../deploy/*.sh"} {
		files, _ := filepath.Glob(glob)
		for _, f := range files {
			body, err := os.ReadFile(f)
			if err == nil && strings.Contains(string(body), "unsafeTestBuild") {
				t.Errorf("%s links unsafeTestBuild", f)
			}
		}
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
