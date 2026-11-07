package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dilla-chat/dilla/internal/config"
)

// Bot v2 task 11: the [bot] table and its defaults. The Bot is off until an operator turns it on, and every path
// default is where the server package installs it.
func TestConfigBotDefaults(t *testing.T) {
	want := config.Bot{
		Enabled:          false,
		Executable:       "/usr/local/bin/dillabot",
		HelperExecutable: "/usr/local/lib/dilla/dilla-bot-core",
		WASIModule:       "",
		StateDir:         "/var/lib/dillabot",
		User:             "dillabot",
		SocketDir:        "/run/dillad/bot",
		DevSameUser:      false,
	}
	if got := config.Default().Bot; got != want {
		t.Fatalf("Default().Bot = %+v, want %+v", got, want)
	}
	c, err := loadTestdata(t, "minimal.toml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Bot != want {
		t.Fatalf("a file without [bot] loads %+v, want the defaults %+v", c.Bot, want)
	}
}

func TestConfigBotTableLoadsAndIsValidated(t *testing.T) {
	dir := t.TempDir()
	body, err := os.ReadFile(filepath.Join("testdata", "minimal.toml"))
	if err != nil {
		t.Fatalf("read testdata: %v", err)
	}
	writeSecrets(t, dir)
	t.Chdir(dir)
	load := func(t *testing.T, bot string) (*config.Config, error) {
		t.Helper()
		path := filepath.Join(dir, "dilla.toml")
		if err := os.WriteFile(path, append(append([]byte(nil), body...), []byte("\n[bot]\n"+bot)...), 0o600); err != nil {
			t.Fatalf("write config: %v", err)
		}
		return config.Load(path)
	}
	c, err := load(t, "enabled = true\nuser = \"bot\"\nsocket_dir = \"/run/dillad/b\"\ndev_same_user = true\n")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !c.Bot.Enabled || c.Bot.User != "bot" || c.Bot.SocketDir != "/run/dillad/b" || !c.Bot.DevSameUser ||
		c.Bot.Executable != "/usr/local/bin/dillabot" {
		t.Fatalf("[bot] = %+v", c.Bot)
	}
	for _, tc := range []struct{ body, key string }{
		{"enabled = true\nexecutable = \"dillabot\"\n", "bot.executable"},
		{"enabled = true\nhelper_executable = \"core\"\n", "bot.helper_executable"},
		{"enabled = true\nstate_dir = \"state\"\n", "bot.state_dir"},
		{"enabled = true\nsocket_dir = \"sock\"\n", "bot.socket_dir"},
		{"enabled = true\nwasi_module = \"core.wasm\"\n", "bot.wasi_module"},
		{"enabled = true\nuser = \"\"\n", "bot.user"},
		// sun_path holds 108 bytes; the socket is <socket_dir>/gateway.sock.
		{"enabled = true\nsocket_dir = \"/" + strings.Repeat("s", 100) + "\"\n", "bot.socket_dir"},
		{"enabled = true\nunknown = 1\n", "unknown key"},
	} {
		if _, err := load(t, tc.body); err == nil || !strings.Contains(err.Error(), tc.key) {
			t.Errorf("[bot] %q: Load = %v, want an error naming %s", tc.body, err, tc.key)
		}
	}
	// A disabled Bot's paths are not read, so they are not checked either.
	if _, err := load(t, "enabled = false\nexecutable = \"relative\"\n"); err != nil {
		t.Fatalf("a disabled [bot] with a relative path: %v", err)
	}
}
