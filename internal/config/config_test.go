package config_test

import (
	"bytes"
	"errors"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jonasthim/dilla/internal/config"
	"github.com/jonasthim/dilla/internal/exit"
)

type netipPrefix = netip.Prefix

func netipMustParse(t *testing.T, s string) netip.Addr {
	t.Helper()
	a, err := netip.ParseAddr(s)
	if err != nil {
		t.Fatalf("ParseAddr(%q): %v", s, err)
	}
	return a
}

func prefixMustParse(t *testing.T, s string) netip.Prefix {
	t.Helper()
	p, err := netip.ParsePrefix(s)
	if err != nil {
		t.Fatalf("ParsePrefix(%q): %v", s, err)
	}
	return p
}

func base() *config.Config {
	c := config.Default()
	c.Instance.Domain = "chat.example"
	c.TLS.Agreed = true
	return c
}

func writeSecrets(t *testing.T, dir string) {
	t.Helper()
	for _, name := range []string{"turn.secret", "livekit.secret"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(strings.Repeat("s", 32)), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
}

// writeSecretFile writes a 32-byte secret file and returns its path.
func writeSecretFile(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(p, []byte(strings.Repeat("s", 32)), 0o600); err != nil {
		t.Fatalf("write secret: %v", err)
	}
	return p
}

func loadTestdata(t *testing.T, name string) (*config.Config, error) {
	t.Helper()
	dir := t.TempDir()
	body, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read testdata: %v", err)
	}
	path := filepath.Join(dir, "dilla.toml")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	writeSecrets(t, dir)
	t.Chdir(dir) // restores the working directory when the test ends
	return config.Load(path)
}

func TestUnknownKeyFailsWithLineAndColumnAndExitConfig(t *testing.T) {
	for _, name := range []string{"unknown_key.toml", "unknown_table.toml"} {
		t.Run(name, func(t *testing.T) {
			_, err := loadTestdata(t, name)
			if err == nil {
				t.Fatal("strict mode accepted an unknown field")
			}
			var code exit.Code
			if !errors.As(err, &code) || code != exit.Config {
				t.Fatalf("error %v does not carry exit.Config", err)
			}
			msg := err.Error()
			if !strings.Contains(msg, "line") || !strings.Contains(msg, "column") {
				t.Fatalf("error %q names neither a line nor a column", msg)
			}
		})
	}
}

func TestMinimalFileLoadsAndValidates(t *testing.T) {
	c, err := loadTestdata(t, "minimal.toml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Instance.DataDir != "/var/lib/dilla" {
		t.Fatalf("data_dir = %q, want the default", c.Instance.DataDir)
	}
	if c.Limits.MaxCiphertextBytes != 131072 {
		t.Fatalf("max_ciphertext_bytes = %d, want 131072 (R6/R32)", c.Limits.MaxCiphertextBytes)
	}
	if c.Auth.Password.Argon2MemoryKiB != 19456 || c.Auth.Password.Argon2Iterations != 2 || c.Auth.Password.Argon2Parallelism != 1 {
		t.Fatalf("argon2 defaults = %d/%d/%d, want 19456/2/1 (R15)",
			c.Auth.Password.Argon2MemoryKiB, c.Auth.Password.Argon2Iterations, c.Auth.Password.Argon2Parallelism)
	}
	if c.Gateway.HeartbeatInterval.Value() != 30*time.Second || c.Gateway.SessionIdleClose.Value() != 90*time.Second {
		t.Fatalf("gateway timings = %s/%s, want 30s/90s (R10)",
			c.Gateway.HeartbeatInterval.Value(), c.Gateway.SessionIdleClose.Value())
	}
	if c.Auth.WebAuthn.RPID != "chat.example" {
		t.Fatalf("derived rp_id = %q", c.Auth.WebAuthn.RPID)
	}
}

// R39 / D23: TURN is off by default in behind_proxy. This goes through Load,
// not through Default()+Derive(), because the bug it guards against lives in
// Load: a substring test for "enabled" over the whole document is true for
// nearly every real file, and Derive's default then never fires.
func TestTURNDefaultsOffBehindProxyEvenWhenOtherTablesSayEnabled(t *testing.T) {
	c, err := loadTestdata(t, "turn_presence.toml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.TURN.Enabled {
		t.Fatal("turn.enabled is true in behind_proxy mode although [turn] carries no enabled key")
	}
}

// The other direction: an operator who DID write turn.enabled is obeyed.
func TestAnExplicitTURNEnabledSurvivesBehindProxy(t *testing.T) {
	dir := t.TempDir()
	body, err := os.ReadFile(filepath.Join("testdata", "turn_presence.toml"))
	if err != nil {
		t.Fatalf("read testdata: %v", err)
	}
	body = append(body, []byte("\nenabled            = true\nlisten             = \"127.0.0.1:3478\"\nshared_secret_file = \"turn.secret\"\nrelay_ip           = \"203.0.113.7\"\n")...)
	path := filepath.Join(dir, "dilla.toml")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	writeSecrets(t, dir)
	t.Chdir(dir) // restores the working directory when the test ends
	c, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !c.TURN.Enabled {
		t.Fatal("an explicit turn.enabled = true was overridden by the behind_proxy default")
	}
}

// R15 / §6.3: there is no pepper, so a set pepper_file is refused rather than
// silently ignored.
func TestAPepperFileIsRefused(t *testing.T) {
	c := base()
	c.Auth.Password.PepperFile = "/etc/dilla/pepper"
	err := c.Validate()
	if err == nil {
		t.Fatal("auth.password.pepper_file was accepted")
	}
	if !strings.Contains(err.Error(), "peppering is not implemented") {
		t.Fatalf("error %q does not say why the key is refused", err)
	}
}

func TestBotSpotifyOAuthConfigRequiresPairedExactHTTPSRedirect(t *testing.T) {
	for _, tc := range []struct {
		name, client, redirect string
		wantErr                bool
	}{
		{"disabled", "", "", false},
		{"complete", "public-id", "https://chat.example/settings/music/callback", false},
		{"missing redirect", "public-id", "", true},
		{"insecure redirect", "public-id", "http://chat.example/callback", true},
		{"userinfo redirect", "public-id", "https://user@chat.example/callback", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := base()
			c.BotSpotify.ClientID = tc.client
			c.BotSpotify.RedirectURL = tc.redirect
			err := c.Validate()
			gotErr := err != nil && strings.Contains(err.Error(), "bot_spotify.")
			if gotErr != tc.wantErr {
				t.Fatalf("Bot Spotify validation error=%v wantErr=%v; all errors: %v", gotErr, tc.wantErr, err)
			}
		})
	}
}

func TestNoFieldIsATimeDuration(t *testing.T) {
	var forbidden = reflect.TypeOf(time.Duration(0))
	var walk func(reflect.Type, string)
	walk = func(t2 reflect.Type, path string) {
		if t2.Kind() != reflect.Struct {
			return
		}
		for i := 0; i < t2.NumField(); i++ {
			f := t2.Field(i)
			if f.Type == forbidden {
				t.Fatalf("%s.%s is a time.Duration: a TOML integer decodes into it as NANOSECONDS (gap-74 §2.1)", path, f.Name)
			}
			walk(f.Type, path+"."+f.Name)
		}
	}
	walk(reflect.TypeOf(config.Config{}), "Config")
}

func TestEveryDurationRoundTripsThroughWriteToAndLoad(t *testing.T) {
	dir := t.TempDir()
	writeSecrets(t, dir)
	t.Chdir(dir) // restores the working directory when the test ends

	c := config.Default()
	c.Instance.Domain = "chat.example"
	c.Instance.PublicIP = netipMustParse(t, "203.0.113.7")
	c.TLS.Agreed = true
	c.TURN.SharedSecretFile = "turn.secret"
	c.TURN.RelayIP = "203.0.113.7"
	c.LiveKit.APISecretFile = "livekit.secret"
	var buf bytes.Buffer
	if err := c.WriteConfig(&buf); err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}
	path := filepath.Join(dir, "written.toml")
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	back, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load(WriteConfig output): %v", err)
	}
	if back.Server.ShutdownGrace.Value() != c.Server.ShutdownGrace.Value() ||
		back.Auth.Session.NativeLifetime.Value() != c.Auth.Session.NativeLifetime.Value() ||
		back.Blobs.GCInterval.Value() != c.Blobs.GCInterval.Value() {
		t.Fatal("a duration key did not survive WriteConfig -> Load")
	}
}

func TestValidationRules(t *testing.T) {
	base := func() *config.Config {
		c := config.Default()
		c.Instance.Domain = "chat.example"
		c.Instance.PublicIP = netipMustParse(t, "203.0.113.7")
		c.TLS.Agreed = true
		c.TURN.Enabled = false
		c.LiveKit.Enabled = false
		return c
	}
	// web3.6: every account needs a second factor, so an instance must offer one.
	t.Run("auth.methods must include totp or passkey", func(t *testing.T) {
		c := base()
		c.Derive()
		c.Auth.Methods = []string{"password"}
		err := c.Validate()
		if err == nil || !strings.Contains(err.Error(), "auth.methods must include totp or passkey: every account needs a second factor") {
			t.Fatalf("auth.methods = [password]: Validate = %v, want the second-factor error", err)
		}
		c.Auth.Methods = []string{"password", "totp"}
		if err := c.Validate(); err != nil {
			t.Fatalf("auth.methods = [password, totp]: Validate = %v, want nil", err)
		}
	})
	// web3.6 review (wire 7): the client decodes at most 16 backup codes.
	t.Run("auth.totp.recovery_codes is at most 16", func(t *testing.T) {
		c := base()
		c.Derive()
		c.Auth.TOTP.RecoveryCodes = 17
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "auth.totp.recovery_codes") {
			t.Fatalf("recovery_codes = 17: Validate = %v, want an error naming the key", err)
		}
		c.Auth.TOTP.RecoveryCodes = 16
		if err := c.Validate(); err != nil {
			t.Fatalf("recovery_codes = 16: Validate = %v, want nil", err)
		}
	})
	t.Run("behind_proxy needs trusted_proxy_cidrs", func(t *testing.T) {
		c := base()
		c.TLS.Mode = config.TLSModeBehindProxy
		c.Server.TrustedProxyCIDRs = nil
		if err := c.Validate(); err == nil {
			t.Fatal("empty trusted_proxy_cidrs accepted in behind_proxy")
		}
	})
	t.Run("direct TLS refuses trusted_proxy_cidrs", func(t *testing.T) {
		c := base()
		c.Server.TrustedProxyCIDRs = []netipPrefix{prefixMustParse(t, "127.0.0.1/32")}
		if err := c.Validate(); err == nil {
			t.Fatal("trusted_proxy_cidrs accepted outside behind_proxy")
		}
	})
	t.Run("turn defaults off under behind_proxy and needs listen when on", func(t *testing.T) {
		c := base()
		c.TLS.Mode = config.TLSModeBehindProxy
		c.Server.TrustedProxyCIDRs = []netipPrefix{prefixMustParse(t, "127.0.0.1/32")}
		c.Derive()
		if c.TURN.Enabled {
			t.Fatal("turn.enabled must default false under behind_proxy (R39)")
		}
		c.TURN.Enabled = true
		c.TURN.Listen = ""
		if err := c.Validate(); err == nil {
			t.Fatal("turn.enabled without turn.listen accepted in behind_proxy")
		}
	})
	t.Run("acme without agreement fails", func(t *testing.T) {
		c := base()
		c.TLS.Agreed = false
		if err := c.Validate(); err == nil {
			t.Fatal("an acme_* mode was accepted without tls.agreed")
		}
	})
	t.Run("retention over thirty days fails", func(t *testing.T) {
		for _, set := range []func(*config.Config){
			func(c *config.Config) { c.Retention.HandshakeDays = 31 },
			func(c *config.Config) { c.Retention.CiphertextDays = 31 },
		} {
			c := base()
			set(c)
			if err := c.Validate(); err == nil {
				t.Fatal("a retention window over 30 days was accepted")
			}
		}
	})
	t.Run("argon2 floors", func(t *testing.T) {
		c := base()
		c.Auth.Password.Argon2Iterations = 1
		if err := c.Validate(); err == nil {
			t.Fatal("argon2_iterations = 1 accepted; the floor is 2")
		}
		c = base()
		c.Auth.Password.Argon2MemoryKiB = 4096
		if err := c.Validate(); err == nil {
			t.Fatal("argon2_memory_kib below 19456 accepted")
		}
		c = base()
		c.Auth.Password.Argon2MemoryKiB = 1 << 20
		c.Auth.Password.HashMemoryBudgetMiB = 256
		if err := c.Validate(); err == nil {
			t.Fatal("argon2_memory_kib above the hash memory budget accepted")
		}
	})
	// I13 (fix wave): a livekit.* key that never reaches LiveKit is refused rather than silently
	// ignored. dillad renders its own YAML, so only the defaults of extra_config_file and
	// use_external_ip are accepted until they are wired.
	t.Run("livekit keys that reach nothing are refused", func(t *testing.T) {
		for name, set := range map[string]func(*config.Config){
			"extra_config_file": func(c *config.Config) { c.LiveKit.ExtraConfigFile = "/etc/dilla/livekit.yaml" },
			"use_external_ip":   func(c *config.Config) { c.LiveKit.UseExternalIP = true },
		} {
			c := base()
			set(c)
			c.Derive()
			err := c.Validate()
			if err == nil || !strings.Contains(err.Error(), "livekit."+name) {
				t.Errorf("livekit.%s set: Validate = %v, want an error naming the key", name, err)
			}
		}
		c := base()
		c.Derive()
		if err := c.Validate(); err != nil {
			t.Fatalf("the defaults: %v", err)
		}
	})
	// dilla-media task 10: the publisher lease enforces livekit.max_publishers, so any cap from one
	// device up to the room's own is accepted.
	t.Run("livekit.max_publishers is within the room cap", func(t *testing.T) {
		for publishers, ok := range map[int]bool{0: false, 1: true, 4: true, 25: true, 26: false} {
			c := base()
			c.LiveKit.MaxPublishers = publishers
			c.Derive()
			err := c.Validate()
			if ok && err != nil {
				t.Errorf("max_publishers %d refused: %v", publishers, err)
			}
			if !ok && (err == nil || !strings.Contains(err.Error(), "livekit.max_publishers")) {
				t.Errorf("max_publishers %d: Validate = %v, want an error naming the key", publishers, err)
			}
		}
	})
	// I12 (fix wave): turn.relay_ip is an IP address or "auto"; anything else is refused at load, not
	// at the first Allocate.
	t.Run("turn.relay_ip is an address or auto", func(t *testing.T) {
		for relay, ok := range map[string]bool{"auto": true, "10.0.0.5": true, "2001:db8::5": true, "chat.example": false} {
			c := base()
			c.TURN.Enabled = true
			c.TURN.SharedSecretFile = writeSecretFile(t)
			c.TURN.RelayIP = relay
			c.Derive()
			err := c.Validate()
			if ok && err != nil {
				t.Errorf("relay_ip %q refused: %v", relay, err)
			}
			if !ok && (err == nil || !strings.Contains(err.Error(), "turn.relay_ip")) {
				t.Errorf("relay_ip %q: Validate = %v, want an error naming turn.relay_ip", relay, err)
			}
		}
	})
	t.Run("the turn allocation knobs", func(t *testing.T) {
		c := base()
		c.Derive()
		if c.TURN.AllocationsPerDevice != 4 || c.TURN.MaxAllocationAge != "2h" || c.TURN.CredentialTTL != "1h" {
			t.Fatalf("defaults = %d, %s, %s; want 4, 2h, 1h", c.TURN.AllocationsPerDevice, c.TURN.MaxAllocationAge, c.TURN.CredentialTTL)
		}
		for name, set := range map[string]func(*config.Config){
			"turn.allocations_per_device": func(c *config.Config) { c.TURN.AllocationsPerDevice = 0 },
			"turn.max_allocation_age":     func(c *config.Config) { c.TURN.MaxAllocationAge = "30m" },
		} {
			c := base()
			set(c)
			c.Derive()
			if err := c.Validate(); err == nil || !strings.Contains(err.Error(), name) {
				t.Errorf("%s out of range: Validate = %v, want an error naming the key", name, err)
			}
		}
		c = base()
		c.TURN.AllocationsPerDevice = 17
		c.Derive()
		if err := c.Validate(); err == nil {
			t.Error("turn.allocations_per_device 17 was accepted")
		}
	})
	// The relay's credential is judged to the millisecond against clock skew of up to 2 s, so a
	// lifetime under a minute is not a working configuration.
	t.Run("turn.credential_ttl is at least a minute", func(t *testing.T) {
		for ttl, ok := range map[config.Duration]bool{"59s": false, "1s": false, "1m": true, "1h": true} {
			c := base()
			c.TURN.CredentialTTL = ttl
			c.TURN.MaxAllocationAge = "2h"
			c.Derive()
			err := c.Validate()
			if ok && err != nil {
				t.Errorf("turn.credential_ttl %s refused: %v", ttl, err)
			}
			if !ok && (err == nil || !strings.Contains(err.Error(), "turn.credential_ttl") || !strings.Contains(err.Error(), "1m")) {
				t.Errorf("turn.credential_ttl %s: Validate = %v, want an error naming the key and the minimum", ttl, err)
			}
		}
	})
	// I14 (fix wave): turn.public_url is a turn: or turns: URL with a host and a port, or empty.
	t.Run("turn.public_url is a TURN URL", func(t *testing.T) {
		for u, ok := range map[string]bool{
			"": true, "turns:turn.example:5349?transport=tcp": true, "turn:[2001:db8::1]:3478?transport=tcp": true,
			"https://turn.example": false, "turns:turn.example": false, "turn:turn.example:x": false, "turns::5349": false,
		} {
			c := base()
			c.TURN.Enabled = true
			c.TURN.SharedSecretFile = writeSecretFile(t)
			c.TURN.RelayIP = "auto"
			c.TURN.PublicURL = u
			c.Derive()
			err := c.Validate()
			if ok && err != nil {
				t.Errorf("public_url %q refused: %v", u, err)
			}
			if !ok && (err == nil || !strings.Contains(err.Error(), "turn.public_url")) {
				t.Errorf("public_url %q: Validate = %v, want an error naming turn.public_url", u, err)
			}
		}
	})
	t.Run("a zero rate limit is refused", func(t *testing.T) {
		c := base()
		c.Limits.Rate.MessageBurst = 0
		if err := c.Validate(); err == nil {
			t.Fatal("a zero burst was accepted; rate.Limit(0) allows no events")
		}
	})
}

func TestRedactedNeverEmitsASecret(t *testing.T) {
	c := config.Default()
	c.Instance.Domain = "chat.example"
	c.DB.DSN = "postgres://dilla:hunter2@localhost/dilla"
	c.LiveKit.APIKey = "dilla"
	var buf bytes.Buffer
	h := slog.NewJSONHandler(&buf, nil)
	slog.New(h).Info("config", slog.Any("config", c.Redacted()))
	out := buf.String()
	for _, secret := range []string{"hunter2", "postgres://"} {
		if strings.Contains(out, secret) {
			t.Fatalf("Redacted leaked %q: %s", secret, out)
		}
	}
	if !strings.Contains(out, "chat.example") {
		t.Fatalf("Redacted dropped a non-secret field: %s", out)
	}
}

func TestConfigDoesNotImplementLogValuerByAccident(t *testing.T) {
	// slog.Any(c) must not silently serialise the whole struct: the only
	// supported path is c.Redacted(). A LogValue method on *Config would make
	// slog.Any safe and is therefore deliberately absent, so this asserts the
	// method set rather than the output.
	if _, ok := any(config.Default()).(slog.LogValuer); ok {
		t.Fatal("*Config implements slog.LogValuer; R20 requires the explicit Redacted() call")
	}
}

// dilla-media task 8: the new [livekit] keys, their defaults and their ranges.
func TestTheLiveKitMediaKeys(t *testing.T) {
	d := config.Default().LiveKit
	if d.WebhookListen != "127.0.0.1:7883" || d.VP9 || d.MaxShareBitrateKbps != 8000 || d.MaxAudioBitrateKbps != 64 ||
		d.LimitNumTracks != 0 || d.LimitBytesPerSec != 0 || len(d.IPsExcludes) != 0 {
		t.Fatalf("defaults = %+v", d)
	}
	valid := func(t *testing.T) *config.Config {
		t.Helper()
		c := base()
		c.Instance.PublicIP = netipMustParse(t, "203.0.113.7")
		c.TURN.Enabled = false
		c.LiveKit.APISecretFile = writeSecretFile(t)
		c.Derive()
		return c
	}
	if err := valid(t).Validate(); err != nil {
		t.Fatalf("the defaults: %v", err)
	}
	sidecar := valid(t)
	sidecar.LiveKit.Mode = "sidecar"
	if err := sidecar.Validate(); err != nil {
		t.Fatalf("sidecar mode: %v", err)
	}
	for name, set := range map[string]func(*config.Config){
		"livekit.mode unsupported":                func(c *config.Config) { c.LiveKit.Mode = "external" },
		"livekit.webhook_listen not loopback":     func(c *config.Config) { c.LiveKit.WebhookListen = "0.0.0.0:7883" },
		"livekit.webhook_listen without a port":   func(c *config.Config) { c.LiveKit.WebhookListen = "127.0.0.1" },
		"livekit.webhook_listen a hostname":       func(c *config.Config) { c.LiveKit.WebhookListen = "dilla.example:7883" },
		"livekit.max_share_bitrate_kbps too low":  func(c *config.Config) { c.LiveKit.MaxShareBitrateKbps = 99 },
		"livekit.max_share_bitrate_kbps too high": func(c *config.Config) { c.LiveKit.MaxShareBitrateKbps = 20001 },
		"livekit.max_audio_bitrate_kbps too low":  func(c *config.Config) { c.LiveKit.MaxAudioBitrateKbps = 15 },
		"livekit.max_audio_bitrate_kbps too high": func(c *config.Config) { c.LiveKit.MaxAudioBitrateKbps = 511 },
		"livekit.limit_num_tracks negative":       func(c *config.Config) { c.LiveKit.LimitNumTracks = -1 },
		"livekit.limit_num_tracks above int32":    func(c *config.Config) { c.LiveKit.LimitNumTracks = 1 << 31 },
		"livekit.limit_bytes_per_sec negative":    func(c *config.Config) { c.LiveKit.LimitBytesPerSec = -1 },
		"livekit.ips_excludes not a prefix":       func(c *config.Config) { c.LiveKit.IPsExcludes = []string{"172.17.0.1"} },
		"livekit.bind_address a hostname":         func(c *config.Config) { c.LiveKit.BindAddress = "localhost" },
		"livekit.bind_address the wildcard":       func(c *config.Config) { c.LiveKit.BindAddress = "0.0.0.0" },
		"livekit.bind_address the IPv6 wildcard":  func(c *config.Config) { c.LiveKit.BindAddress = "::" },
		"livekit.bind_address a LAN address":      func(c *config.Config) { c.LiveKit.BindAddress = "192.168.1.5" },
		"livekit.bind_address empty":              func(c *config.Config) { c.LiveKit.BindAddress = "" },
		"livekit.bind_address with a port":        func(c *config.Config) { c.LiveKit.BindAddress = "127.0.0.1:7880" },
		"livekit.bind_address a YAML injection":   func(c *config.Config) { c.LiveKit.BindAddress = "127.0.0.1\n  - 0.0.0.0" },
		"livekit.vp9 on":                          func(c *config.Config) { c.LiveKit.VP9 = true },
	} {
		t.Run(name, func(t *testing.T) {
			c := valid(t)
			set(c)
			key, _, _ := strings.Cut(name, " ")
			if err := c.Validate(); err == nil || !strings.Contains(err.Error(), key) {
				t.Fatalf("Validate = %v, want an error naming %s", err, key)
			}
		})
	}
	for name, set := range map[string]func(*config.Config){
		"localhost":            func(c *config.Config) { c.LiveKit.WebhookListen = "localhost:7883" },
		"IPv6 loopback":        func(c *config.Config) { c.LiveKit.WebhookListen = "[::1]:7883" },
		"port 0 (tests)":       func(c *config.Config) { c.LiveKit.WebhookListen = "127.0.0.1:0" },
		"a Docker exclude":     func(c *config.Config) { c.LiveKit.IPsExcludes = []string{"172.17.0.0/16", "fd00::/8"} },
		"a loopback bind":      func(c *config.Config) { c.LiveKit.BindAddress = "127.0.0.2" },
		"the IPv6 loopback":    func(c *config.Config) { c.LiveKit.BindAddress = "::1" },
		"host limits":          func(c *config.Config) { c.LiveKit.LimitNumTracks, c.LiveKit.LimitBytesPerSec = 4000, 125_000_000 },
		"bitrates at the ends": func(c *config.Config) { c.LiveKit.MaxShareBitrateKbps, c.LiveKit.MaxAudioBitrateKbps = 100, 510 },
	} {
		t.Run("accepted: "+name, func(t *testing.T) {
			c := valid(t)
			set(c)
			if err := c.Validate(); err != nil {
				t.Fatalf("Validate = %v", err)
			}
		})
	}
	// The VP9 follow-up (crypto lens, parked): the refusal says why, so an operator knows it is not a
	// typo — no frame vector and no measurement through LiveKit for the 0-byte prefix rule yet.
	vp9 := valid(t)
	vp9.LiveKit.VP9 = true
	if err := vp9.Validate(); err == nil || !strings.Contains(err.Error(), "test vector") ||
		!strings.Contains(err.Error(), "measured through LiveKit") {
		t.Fatalf("livekit.vp9 = true: Validate = %v, want a refusal naming the missing vector and measurement", err)
	}
	// A webhook listener nothing would start: a non-default webhook_listen with LiveKit off.
	c := valid(t)
	c.LiveKit.Enabled = false
	if err := c.Validate(); err != nil {
		t.Fatalf("LiveKit off with the default webhook_listen: %v", err)
	}
	c.LiveKit.WebhookListen = "127.0.0.1:9999"
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "livekit.webhook_listen") {
		t.Fatalf("LiveKit off with webhook_listen set: Validate = %v, want a refusal naming the key", err)
	}
}

// dilla-web-2a (L-HTTP-54, Q04): the per-user device cap and enrolment rate default to 8 and 3,
// and a file written before the keys existed loads with those defaults.
func TestTheEnrolmentLimitsDefault(t *testing.T) {
	d := config.Default().Auth.Session
	if d.MaxDevicesPerUser != 8 || d.EnrolmentsPerHour != 3 {
		t.Fatalf("defaults = %d devices, %d enrolments an hour; want 8 and 3 (Q04)", d.MaxDevicesPerUser, d.EnrolmentsPerHour)
	}
	c, err := loadTestdata(t, "minimal.toml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Auth.Session.MaxDevicesPerUser != 8 || c.Auth.Session.EnrolmentsPerHour != 3 {
		t.Fatalf("a file without the keys = %d, %d; want the defaults 8 and 3",
			c.Auth.Session.MaxDevicesPerUser, c.Auth.Session.EnrolmentsPerHour)
	}
}

func TestTheEnrolmentLimitsAreValidated(t *testing.T) {
	valid := func(t *testing.T) *config.Config {
		t.Helper()
		c := base()
		c.Instance.PublicIP = netipMustParse(t, "203.0.113.7")
		c.TURN.Enabled = false
		c.LiveKit.APISecretFile = writeSecretFile(t)
		c.Derive()
		return c
	}
	if err := valid(t).Validate(); err != nil {
		t.Fatalf("the defaults: %v", err)
	}
	for name, set := range map[string]func(*config.Config){
		"auth.session.max_devices_per_user 0":  func(c *config.Config) { c.Auth.Session.MaxDevicesPerUser = 0 },
		"auth.session.max_devices_per_user 65": func(c *config.Config) { c.Auth.Session.MaxDevicesPerUser = 65 },
		"auth.session.enrolments_per_hour 0":   func(c *config.Config) { c.Auth.Session.EnrolmentsPerHour = 0 },
		"auth.session.enrolments_per_hour 61":  func(c *config.Config) { c.Auth.Session.EnrolmentsPerHour = 61 },
	} {
		t.Run(name, func(t *testing.T) {
			c := valid(t)
			set(c)
			key, _, _ := strings.Cut(name, " ")
			err := c.Validate()
			if err == nil || !strings.Contains(err.Error(), key) || !strings.Contains(err.Error(), "the range is") {
				t.Fatalf("Validate = %v, want an error naming %s and its range", err, key)
			}
		})
	}
	for name, set := range map[string]func(*config.Config){
		"the lower ends": func(c *config.Config) { c.Auth.Session.MaxDevicesPerUser, c.Auth.Session.EnrolmentsPerHour = 1, 1 },
		"the upper ends": func(c *config.Config) { c.Auth.Session.MaxDevicesPerUser, c.Auth.Session.EnrolmentsPerHour = 64, 60 },
	} {
		t.Run("accepted: "+name, func(t *testing.T) {
			c := valid(t)
			set(c)
			if err := c.Validate(); err != nil {
				t.Fatalf("Validate = %v", err)
			}
		})
	}
}

func TestTheEnrolmentLimitsRoundTripThroughWriteToAndLoad(t *testing.T) {
	dir := t.TempDir()
	writeSecrets(t, dir)
	t.Chdir(dir) // restores the working directory when the test ends

	c := config.Default()
	c.Instance.Domain = "chat.example"
	c.Instance.PublicIP = netipMustParse(t, "203.0.113.7")
	c.TLS.Agreed = true
	c.TURN.SharedSecretFile = "turn.secret"
	c.TURN.RelayIP = "203.0.113.7"
	c.LiveKit.APISecretFile = "livekit.secret"
	c.Auth.Session.MaxDevicesPerUser = 5
	c.Auth.Session.EnrolmentsPerHour = 7
	var buf bytes.Buffer
	if err := c.WriteConfig(&buf); err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}
	for _, line := range []string{"max_devices_per_user = 5", "enrolments_per_hour = 7"} {
		if !strings.Contains(buf.String(), line) {
			t.Fatalf("WriteConfig output lacks %q", line)
		}
	}
	path := filepath.Join(dir, "written.toml")
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	back, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load(WriteConfig output): %v", err)
	}
	if back.Auth.Session.MaxDevicesPerUser != 5 || back.Auth.Session.EnrolmentsPerHour != 7 {
		t.Fatalf("after Load = %d, %d; want 5, 7", back.Auth.Session.MaxDevicesPerUser, back.Auth.Session.EnrolmentsPerHour)
	}
}

func TestOrdinaryBodyReadBudgetIsConfigurableAndValidated(t *testing.T) {
	body, err := os.ReadFile("testdata/minimal.toml")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	writeSecrets(t, dir)
	t.Chdir(dir)
	path := filepath.Join(dir, "dilla.toml")
	for _, budget := range []string{"40ms", "-1s", "invalid"} {
		if err := os.WriteFile(path, append(append([]byte{}, body...), []byte("\n[server]\nread_body_timeout = \""+budget+"\"\n")...), 0600); err != nil {
			t.Fatal(err)
		}
		_, err := config.Load(path)
		if budget == "40ms" && err != nil {
			t.Fatalf("valid body budget refused: %v", err)
		}
		if budget != "40ms" && err == nil {
			t.Fatalf("invalid body budget %q accepted", budget)
		}
	}
}
