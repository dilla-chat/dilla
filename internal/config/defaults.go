package config

import (
	"net/url"
	"runtime"
)

// DefaultWebhookListen is the loopback destination for LiveKit webhooks.
const DefaultWebhookListen = "127.0.0.1:7883"

// Default returns the struct that is pre-populated BEFORE decoding, so deleting
// any line from dilla.toml leaves behaviour unchanged (gap-74 §2.4). Three
// values here are a ruling's rather than the gap file's: max_ciphertext_bytes
// 131072 (R6/R32), the argon2 triple 19456/2/1 (R15, OWASP 2026) and the
// 30s/90s gateway pair (R10).
func Default() *Config {
	c := &Config{}
	c.Instance = Instance{DataDir: "/var/lib/dilla", Discovery: "public"}
	c.Server = Server{
		Listen: ":443", PlainListen: "127.0.0.1:8080",
		ReadHeaderTimeout: "10s", ReadBodyTimeout: "30s", IdleTimeout: "120s", ShutdownGrace: "30s",
	}
	c.HTTP = HTTP{TrustedOrigins: []string{"dilla://app"}}
	c.Gateway = Gateway{
		HeartbeatInterval: "30s", SessionIdleClose: "90s",
		ReadLimitBytes: 16384, FrameBurst: 60, FrameBurstMax: 120,
	}
	c.TLS = TLS{Mode: TLSModeACMETLSALPN}
	c.TLS.DNS = TLSDNS{TTL: "0s", PropagationDelay: "0s", PropagationTimeout: "2m"}
	c.TURN = TURN{Enabled: true, CredentialTTL: "1h", MaxAllocationAge: "2h", AllocationsPerDevice: 4}
	c.LiveKit = LiveKit{
		Enabled: true, Mode: "in_process", BindAddress: "127.0.0.1", Port: 7880,
		UDPPort: 7882, TCPPort: 0, AdvertiseInternalIP: true, APIKey: "dilla",
		MaxVoiceParticipants: 25, MaxPublishers: defaultMaxPublishers,
		WebhookListen: DefaultWebhookListen, MaxShareBitrateKbps: 8000, MaxAudioBitrateKbps: 64,
	}
	c.DB = DB{
		Driver: "sqlite", Path: "/var/lib/dilla/dilla.db", ConnMaxLifetime: "0s",
		AutoMigrate: true, PreMigrationBackup: true,
	}
	c.Blobs = Blobs{
		Dir: "/var/lib/dilla/blobs", Backend: "fs",
		MaxBlobBytes: 104857600, QuotaBytesPerUser: 10737418240,
		PendingTTL: "24h", GCGrace: "24h", GCInterval: "1h", UploadTimeout: "15m",
		UploadsPerMinute: 20, UploadBytesPerDay: 5368709120,
	}
	c.Limits = Limits{
		MaxCiphertextBytes: 131072, MaxAttachmentBytes: 104857600,
		MaxKeypackagesPerDevice: 32, KeypackageRefillThreshold: 8,
		Rate: Rate{
			Enabled: true, SweepInterval: "30s", MaxKeys: 100000, MaxRetryAfter: "1h",
			MessagePerSecond: 1.0, MessageBurst: 20,
			CommitPerSecond: 2.0, CommitBurst: 40,
			ProposalPerSecond: 0.5, ProposalBurst: 10,
			ReadPerSecond: 10.0, ReadBurst: 60,
			WritePerSecond: 2.0, WriteBurst: 20,
			LoginPerSecond: 0.003, LoginBurst: 5,
			LoginFailedPerSecond: 0.17, LoginFailedBurst: 3,
			RegisterPerSecond: 0.17, RegisterBurst: 3,
			InvitePerSecond: 0.1, InviteBurst: 5,
			UnauthPerSecond: 1.0, UnauthBurst: 20,
			InvalidPerSecond: 0.2, InvalidBurst: 60,
			PasswordConcurrency: 0,
		},
	}
	c.Retention = Retention{HandshakeDays: 30, CiphertextDays: 30}
	c.Registration = Registration{Mode: "invite"}
	c.Auth = Auth{
		Methods: []string{"password", "totp", "passkey"},
		Password: Password{
			Argon2MemoryKiB: 19456, Argon2Iterations: 2, Argon2Parallelism: 1,
			Argon2SaltBytes: 16, Argon2KeyBytes: 32, HashMemoryBudgetMiB: 256,
		},
		TOTP: TOTP{Period: 30, Skew: 1, Digits: 6, Algorithm: "SHA1", SecretSize: 20, RecoveryCodes: 10},
		// "required" (web3.6 D2): a passkey counts as the account's whole second factor only when
		// its assertion verified the user, so the default asks every authenticator for it.
		WebAuthn: WebAuthn{UserVerification: "required"},
		OIDC:     OIDC{Scopes: []string{"openid", "profile", "email"}},
		Session: Session{
			NativeLifetime: "720h", BrowserLifetime: "168h", BrowserIdle: "12h",
			MaxPerDevice: 8, ReauthWindow: "300s",
			MaxDevicesPerUser: 8, EnrolmentsPerHour: 3,
		},
		Lockout: Lockout{
			ObservationWindow: "15m", FreeAttempts: 4, FirstLockout: "30s",
			LockoutCeiling: "1h", HardCeiling: 100,
		},
	}
	c.Log = Log{Level: "info", Format: "json"}
	c.Metrics = Metrics{Enabled: true, Path: "/metrics", RequireAdmin: true}
	c.Doctor = Doctor{ClockSkewMax: "60s", TURNProbe: true, UDPProbe: true}
	return c
}

// RelayIPAuto is the turn.relay_ip value `dillad init` writes: serve binds relay sockets on this
// host's own address (internal/server.ResolveRelayIP), never on a public IP the host may not hold.
const RelayIPAuto = "auto"

// defaultMaxPublishers is livekit.max_publishers' default, the spec's 25/10 sizing; the publisher
// lease enforces it.
const defaultMaxPublishers = 10

// LetsEncryptProductionCA is certmagic's production directory, spelled out here
// so Derive does not pull certmagic into every binary that reads config.
const LetsEncryptProductionCA = "https://acme-v02.api.letsencrypt.org/directory"

// Derive fills the documented empty values that can only be known once the file
// has been read. It runs after decoding and before Validate, and doctor prints
// the RESOLVED values, which is the only way an operator sees what "" became.
func (c *Config) Derive() {
	if c.TLS.StorageDir == "" {
		c.TLS.StorageDir = c.Instance.DataDir + "/certmagic"
	}
	if c.TLS.CA == "" {
		c.TLS.CA = LetsEncryptProductionCA
	}
	if c.BehindProxy() && !c.turnExplicitlyEnabled {
		c.TURN.Enabled = false
	}
	if c.TURN.Realm == "" {
		c.TURN.Realm = c.Instance.Domain
	}
	if c.LiveKit.NodeIP == "" && c.Instance.PublicIP.IsValid() {
		c.LiveKit.NodeIP = c.Instance.PublicIP.String()
	}
	if len(c.LiveKit.STUNServers) == 0 && c.Instance.Domain != "" {
		c.LiveKit.STUNServers = []string{c.Instance.Domain + ":3478"}
	}
	if c.Auth.TOTP.Issuer == "" {
		c.Auth.TOTP.Issuer = c.Instance.Domain
	}
	if c.Auth.WebAuthn.RPID == "" {
		c.Auth.WebAuthn.RPID = c.Instance.Domain
	}
	if c.Auth.WebAuthn.RPDisplayName == "" {
		c.Auth.WebAuthn.RPDisplayName = c.Instance.Domain
	}
	if len(c.Auth.WebAuthn.RPOrigins) == 0 && c.Instance.Domain != "" {
		origins := []string{"https://" + c.Instance.Domain}
		c.Auth.WebAuthn.RPOrigins = append(origins, c.HTTP.TrustedOrigins...)
	}
	if c.Auth.OIDC.RedirectURL == "" && c.Instance.Domain != "" {
		c.Auth.OIDC.RedirectURL = (&url.URL{Scheme: "https", Host: c.Instance.Domain, Path: "/v1/auth/oidc/callback"}).String()
	}
	if len(c.Doctor.ClockPeers) == 0 {
		c.Doctor.ClockPeers = []string{c.TLS.CA, "https://www.cloudflare.com", "https://www.google.com"}
	}
	if c.Limits.Rate.PasswordConcurrency == 0 {
		c.Limits.Rate.PasswordConcurrency = runtime.NumCPU()
	}
	// One derivation, two readers: GET /v1/instance/limits element 8 and the
	// gateway's hello frame must advertise the same number, and
	// limits.max_ciphertext_bytes is operator-settable over 4096..1048576, so a
	// literal 131584 in either place is wrong the moment an operator changes it.
	c.maxFrameBytes = uint64(c.Limits.MaxCiphertextBytes) + 512 //nolint:gosec // G115: a config value that Validate keeps positive
}

// MaxFrameBytes is the largest gateway frame the instance accepts and
// advertises: max_ciphertext_bytes plus 512 bytes of frame envelope (§2.7). It
// is derived, not configured, and is valid only after Derive.
func (c *Config) MaxFrameBytes() uint64 { return c.maxFrameBytes }
