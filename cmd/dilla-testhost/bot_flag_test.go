package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// GPUI Task 28: the desktop client's Bot tests start the host with -bot (clients/desktop/dilla-client/src/testing/host/bot.rs).
// The flags reach botOptions, and botConfig is the [bot] section dillad would run: dev_same_user, every path absolute, the
// state directory defaulting under the data directory and the socket directory beside it.
func TestTheBotFlagsReachTheBotConfig(t *testing.T) {
	dir := t.TempDir()
	o, err := parseFlags([]string{"-bot", "-bot-executable", "bin/dillabot", "-bot-helper", "bin/dilla-bot-core",
		"-bot-wasi", "core.wasm"})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if !o.bot.enabled || o.bot.executable != "bin/dillabot" || o.bot.helper != "bin/dilla-bot-core" ||
		o.bot.wasi != "core.wasm" || o.bot.stateDir != "" {
		t.Fatalf("bot options %+v", o.bot)
	}
	data := filepath.Join(dir, "data")
	c, err := o.bot.botConfig(data)
	if err != nil {
		t.Fatalf("botConfig: %v", err)
	}
	if !c.Enabled || !c.DevSameUser || c.User == "" {
		t.Fatalf("config %+v", c)
	}
	for _, p := range []string{c.Executable, c.HelperExecutable, c.WASIModule, c.StateDir, c.SocketDir} {
		if !filepath.IsAbs(p) {
			t.Fatalf("%q is not absolute: %+v", p, c)
		}
	}
	if !strings.HasSuffix(c.Executable, filepath.Join("bin", "dillabot")) {
		t.Fatalf("executable %q", c.Executable)
	}
	if c.StateDir != filepath.Join(data, "bot-state") {
		t.Fatalf("default state dir %q", c.StateDir)
	}
	if c.SocketDir != data+"-bot" {
		t.Fatalf("socket dir %q", c.SocketDir)
	}

	explicit, err := parseFlags([]string{"-bot", "-bot-executable", "a", "-bot-helper", "b", "-bot-wasi", "c",
		"-bot-state-dir", filepath.Join(dir, "state")})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	c, err = explicit.bot.botConfig(data)
	if err != nil || c.StateDir != filepath.Join(dir, "state") {
		t.Fatalf("explicit state dir %q %v", c.StateDir, err)
	}
}

// Every binary is required: a -bot host with one missing refuses to start, naming the flag.
func TestABotHostNeedsEachBinary(t *testing.T) {
	for _, missing := range []string{"-bot-executable", "-bot-helper", "-bot-wasi"} {
		args := []string{"-bot"}
		for _, f := range []string{"-bot-executable", "-bot-helper", "-bot-wasi"} {
			if f != missing {
				args = append(args, f, "x")
			}
		}
		o, err := parseFlags(args)
		if err != nil {
			t.Fatalf("parseFlags %v: %v", args, err)
		}
		if _, err := o.bot.botConfig(t.TempDir()); err == nil || !strings.Contains(err.Error(), missing) {
			t.Fatalf("missing %s: %v", missing, err)
		}
	}
}

// Without -bot the host runs no Bot: the option is off and the host options carry no launch gate.
func TestWithoutTheBotFlagTheHostRunsNoBot(t *testing.T) {
	o, err := parseFlags(nil)
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if o.bot.enabled {
		t.Fatalf("bot on by default: %+v", o.bot)
	}
	if ho := o.hostOptions("/data", "/core.wasm", ""); ho.BotLaunch != nil {
		t.Fatalf("a launch gate without -bot: %+v", ho)
	}
}
