package main

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jonasthim/dilla/internal/bot/client"
	"github.com/jonasthim/dilla/internal/bot/helper"
)

func TestSafeTrustErrorAttrsUseFixedClassifications(t *testing.T) {
	if got, want := safeTrustErrorAttrs(&helper.StatusError{Status: 9}), []any{"trust_failure", "native_helper", "helper_status", "internal"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("helper attrs = %#v, want %#v", got, want)
	}
	secretCode := "E_PRIVATE_BODY_SHOULD_NOT_LOG"
	got := safeTrustErrorAttrs(&client.APIError{Status: 403, Code: secretCode})
	if want := []any{"trust_failure", "api", "api_status", 403, "api_code", "other"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("API attrs = %#v, want %#v", got, want)
	}
	if fmt.Sprint(got) == secretCode {
		t.Fatal("API code was not filtered through the fixed allowlist")
	}
}

func TestSafeMembershipErrorAttrsExposeOnlyFixedStatus(t *testing.T) {
	if got, want := safeMembershipErrorAttrs(&helper.StatusError{Status: 3}), []any{"membership_failure", "native_helper", "helper_status", "expired"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("helper attrs = %#v, want %#v", got, want)
	}
	secretCode := "E_PRIVATE_BODY_SHOULD_NOT_LOG"
	got := safeMembershipErrorAttrs(&client.APIError{Status: 403, Code: secretCode})
	if want := []any{"membership_failure", "api", "api_status", 403, "api_code", "other"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("API attrs = %#v, want %#v", got, want)
	}
	if fmt.Sprint(got) == secretCode {
		t.Fatal("API code was not filtered through the fixed allowlist")
	}
}

func TestLoadConfigRequiresAbsoluteProtectedCredentialPaths(t *testing.T) {
	dir := t.TempDir()
	credential := filepath.Join(dir, "runtime.token")
	if err := os.WriteFile(credential, []byte("dbr_"+base64.RawURLEncoding.EncodeToString(make([]byte, 32))), 0o600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "bot.toml")
	config := `
base_url = "https://dilla.example"
instance_id = "01010101010101010101010101010101"
runtime_credential_file = "` + credential + `"
helper_executable = "/usr/libexec/dilla-bot-core"
wasi_module = "/usr/lib/dilla/dilla_core_wasi.wasm"
state_dir = "/var/lib/dillabot"
reconcile_interval = "30s"
[rate_limits]
device_cache_limit = 4
target_cache_limit = 8
global_target_cache_limit = 16
write_per_second = 1.0
write_burst = 15
read_per_second = 6.0
read_burst = 30
message_per_second = 0.5
message_burst = 5
`
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := loadConfig(configPath)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if got.RateLimits.WritePerSecond != 1 || got.RateLimits.WriteBurst != 15 || got.RateLimits.ReadPerSecond != 6 || got.RateLimits.ReadBurst != 30 || got.RateLimits.MessagePerSecond != .5 || got.RateLimits.MessageBurst != 5 {
		t.Fatalf("rate limits not loaded: %+v", got.RateLimits)
	}
	if got.RateLimits.DeviceCacheLimit != 4 || got.RateLimits.TargetCacheLimit != 8 || got.RateLimits.GlobalTargetCacheLimit != 16 {
		t.Fatalf("cache limits not loaded: %+v", got.RateLimits)
	}
	if err := os.WriteFile(configPath, []byte(strings.Replace(config, "device_cache_limit = 4", "device_cache_limit = -1", 1)), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(configPath); !errors.Is(err, errConfigInvalid) {
		t.Fatalf("invalid limiter cache config accepted: %v", err)
	}
	if err := os.WriteFile(configPath, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	if got.InstanceID.String() != "01010101010101010101010101010101" || got.RuntimeCredentialFile != credential {
		t.Fatalf("loaded config does not preserve configured instance and credential file")
	}

	if err := os.Chmod(credential, 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(configPath); err == nil {
		t.Fatal("loadConfig accepted a runtime credential file readable by group")
	}
}

func TestLoadConfigRejectsCredentialSymlink(t *testing.T) {
	dir := t.TempDir()
	secret := filepath.Join(dir, "secret")
	if err := os.WriteFile(secret, []byte("dbr_"+base64.RawURLEncoding.EncodeToString(make([]byte, 32))), 0o600); err != nil {
		t.Fatal(err)
	}
	credential := filepath.Join(dir, "runtime.token")
	if err := os.Symlink(secret, credential); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "bot.toml")
	config := `
base_url = "https://dilla.example"
instance_id = "01010101010101010101010101010101"
runtime_credential_file = "` + credential + `"
helper_executable = "/usr/libexec/dilla-bot-core"
wasi_module = "/usr/lib/dilla/dilla_core_wasi.wasm"
state_dir = "/var/lib/dillabot"
reconcile_interval = "30s"
`
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(configPath); err == nil {
		t.Fatal("loadConfig accepted a symlink credential file")
	}
}

func TestLoadConfigRejectsUnknownKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bot.toml")
	if err := os.WriteFile(path, []byte("unexpected = true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(path); err == nil {
		t.Fatal("loadConfig accepted an unknown setting")
	}
}

func TestLoadConfigValidatesOptionalSpotifyOAuthPair(t *testing.T) {
	dir := t.TempDir()
	credential := filepath.Join(dir, "runtime.token")
	if err := os.WriteFile(credential, []byte("dbr_"+base64.RawURLEncoding.EncodeToString(make([]byte, 32))), 0o600); err != nil {
		t.Fatal(err)
	}
	base := `base_url = "https://dilla.example"
instance_id = "01010101010101010101010101010101"
runtime_credential_file = "` + credential + `"
helper_executable = "/usr/libexec/dilla-bot-core"
wasi_module = "/usr/lib/dilla/dilla_core_wasi.wasm"
state_dir = "/var/lib/dillabot"
reconcile_interval = "30s"
`
	for _, tc := range []struct {
		suffix string
		good   bool
	}{
		{``, true},
		{`spotify_helper_executable = "/usr/local/libexec/dilla-spotify-helper"
`, true},
		{`spotify_helper_executable = "dilla-spotify-helper"
`, false},
		{`spotify_client_id = "public-client"
spotify_redirect_url = "https://dilla.example/settings/bot/music/spotify/callback"
`, true},
		{`spotify_client_id = "public-client"
spotify_redirect_url = "https://dilla.example/settings/bot/music/spotify/callback"
spotify_helper_executable = "/usr/local/libexec/dilla-spotify-helper"
`, true},
		{`spotify_client_id = "public-client"
`, false},
		{`spotify_client_id = "public-client"
spotify_redirect_url = "http://dilla.example/callback"
`, false},
		{`spotify_client_id = "public-client"
spotify_redirect_url = "https://dilla.example/settings/bot/music/spotify/callback"
spotify_helper_executable = "dilla-spotify-helper"
`, false},
	} {
		path := filepath.Join(dir, "bot.toml")
		if err := os.WriteFile(path, []byte(base+tc.suffix), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := loadConfig(path)
		if (err == nil) != tc.good {
			t.Fatalf("load config with suffix %q: %v", tc.suffix, err)
		}
		if tc.good && strings.Contains(tc.suffix, "spotify_client_id") && cfg.SpotifyClientID != "public-client" {
			t.Fatal("valid Spotify client ID was not loaded")
		}
	}
}

func TestLoadDatabaseKeyCreatesPrivatePerInstallationKey(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	installation := "02020202020202020202020202020202"
	key, path, err := loadDatabaseKey(root, installation)
	if err != nil {
		t.Fatalf("loadDatabaseKey: %v", err)
	}
	if len(key) != 32 || path != filepath.Join(root, installation, "database.key") {
		t.Fatal("database key has unexpected shape or location")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("database key mode = %o, want 600", info.Mode().Perm())
	}
	want := append([]byte(nil), key...)
	clear(key)

	second, _, err := loadDatabaseKey(root, installation)
	if err != nil {
		t.Fatalf("load existing database key: %v", err)
	}
	defer clear(second)
	if len(second) != 32 {
		t.Fatal("existing database key has wrong size")
	}
	if string(second) != string(want) {
		t.Fatal("database key changed across reload")
	}
	clear(want)
}

func TestLoadDatabaseKeyRefusesMissingKeyWhenDatabaseOrSidecarsExist(t *testing.T) {
	for _, stateFile := range []string{"state.db", "state.db-wal", "state.db-shm", "state.db-journal", "state.db.init-abcdef", "state.init", "state.init-tmp-abcdef"} {
		t.Run(stateFile, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "state")
			installDir := filepath.Join(root, "03030303030303030303030303030303")
			if err := os.MkdirAll(installDir, 0o700); err != nil {
				t.Fatal(err)
			}
			statePath := filepath.Join(installDir, stateFile)
			if err := os.WriteFile(statePath, []byte("encrypted state fixture"), 0o600); err != nil {
				t.Fatal(err)
			}

			key, _, err := loadDatabaseKey(root, filepath.Base(installDir))
			if err == nil {
				clear(key)
				t.Fatal("loadDatabaseKey created a new key while database state existed")
			}
			if _, statErr := os.Lstat(filepath.Join(installDir, "database.key")); !os.IsNotExist(statErr) {
				t.Fatalf("loadDatabaseKey persisted a replacement key: stat err = %v", statErr)
			}
		})
	}
}

func TestValidateHelperExecutableRequiresRegularExecutable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "helper")
	if err := os.WriteFile(path, []byte("helper"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateHelperExecutable(path); err == nil {
		t.Fatal("helper preflight accepted a non-executable file")
	}
	if err := os.Chmod(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := validateHelperExecutable(path); err != nil {
		t.Fatalf("helper preflight rejected an executable file: %v", err)
	}
}
