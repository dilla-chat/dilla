// Package config is dillad's dilla.toml: 16 top-level tables, 8 sub-tables and
// about 155 keys, decoded in strict mode so a misspelling refuses to start
// rather than running with a default the operator did not choose.
//
// interfaces.md §6.4 says "14 top-level tables, 5 sub-tables, 128 keys", but its
// own key listing enumerates sixteen tables (instance server http gateway tls
// turn livekit db blobs limits retention registration auth log metrics doctor)
// and eight sub-tables (tls.dns limits.rate auth.password auth.totp
// auth.webauthn auth.oidc auth.session auth.lockout). The counts here are the
// ones this file actually declares (deviation ID13).
//
// Two shapes in here are deliberate and easy to get wrong. Durations are a
// named string type, never time.Duration: a TOML integer decodes into a
// time.Duration as NANOSECONDS, so `idle_timeout = 120` would mean 120ns and
// nothing would ever time out (gap-74 §2.1). And behind_proxy is not a key: it
// is tls.mode = "behind_proxy", because two keys that must agree is exactly the
// drift strict mode cannot catch (gap-74 §4.1).
package config

import (
	"fmt"
	"net/netip"
	"time"
)

// Duration is a TOML string parsed by time.ParseDuration.
type Duration string

// Value returns the parsed duration. Validate has already proved it parses, so
// a late failure returns 0 rather than panicking in a request path.
func (d Duration) Value() time.Duration {
	v, err := time.ParseDuration(string(d))
	if err != nil {
		return 0
	}
	return v
}

func (d Duration) check(key string) error {
	if _, err := time.ParseDuration(string(d)); err != nil {
		return fmt.Errorf("%s: %q is not a duration (e.g. \"30s\", \"12h\"): %w", key, string(d), err)
	}
	return nil
}

// TLSMode is the one mode switch.
type TLSMode string

const (
	TLSModeACMETLSALPN TLSMode = "acme_tls_alpn"
	TLSModeACMEDNS     TLSMode = "acme_dns"
	TLSModeACMEIP      TLSMode = "acme_ip"
	TLSModeBehindProxy TLSMode = "behind_proxy"
)

// UnmarshalText refuses anything but the four modes, naming all four.
func (m *TLSMode) UnmarshalText(b []byte) error {
	switch TLSMode(b) {
	case TLSModeACMETLSALPN, TLSModeACMEDNS, TLSModeACMEIP, TLSModeBehindProxy:
		*m = TLSMode(b)
		return nil
	default:
		return fmt.Errorf("tls.mode: %q is not one of acme_tls_alpn, acme_dns, acme_ip, behind_proxy", string(b))
	}
}

func (m TLSMode) MarshalText() ([]byte, error) { return []byte(m), nil }

// Config is dilla.toml.
type Config struct {
	Instance     Instance     `toml:"instance"`
	Server       Server       `toml:"server"`
	HTTP         HTTP         `toml:"http"`
	Gateway      Gateway      `toml:"gateway"`
	TLS          TLS          `toml:"tls"`
	TURN         TURN         `toml:"turn"`
	LiveKit      LiveKit      `toml:"livekit"`
	DB           DB           `toml:"db"`
	Blobs        Blobs        `toml:"blobs"`
	Limits       Limits       `toml:"limits"`
	Retention    Retention    `toml:"retention"`
	Registration Registration `toml:"registration"`
	Auth         Auth         `toml:"auth"`
	Log          Log          `toml:"log"`
	Metrics      Metrics      `toml:"metrics"`
	Doctor       Doctor       `toml:"doctor"`
	BotSpotify   BotSpotify   `toml:"bot_spotify"`

	// turnExplicitlyEnabled records whether the operator wrote turn.enabled,
	// set by Load from a second, non-strict decode (see write-up in defaults.go).
	turnExplicitlyEnabled bool `toml:"-"`
	// maxFrameBytes is derived by Derive; see MaxFrameBytes.
	maxFrameBytes uint64 `toml:"-"`
}

// BotSpotify contains the public Spotify OAuth application and the exact
// HTTPS callback URI registered for this instance. No provider secret belongs here.
type BotSpotify struct {
	ClientID    string `toml:"client_id"`
	RedirectURL string `toml:"redirect_url"`
}

type Instance struct {
	Domain    string     `toml:"domain" comment:"REQUIRED. Public DNS name this instance answers on."`
	PublicIP  netip.Addr `toml:"public_ip" comment:"REQUIRED unless tls.mode = \"behind_proxy\". IPv4/IPv6 literal."`
	DataDir   string     `toml:"data_dir" comment:"State root. systemd StateDirectory=dilla."`
	Discovery string     `toml:"discovery" comment:"\"public\" | \"session\" — auth on GET /v1/instance."`
}

type Server struct {
	Listen            string         `toml:"listen"`
	PlainListen       string         `toml:"plain_listen"`
	TrustedProxyCIDRs []netip.Prefix `toml:"trusted_proxy_cidrs"`
	ReadHeaderTimeout Duration       `toml:"read_header_timeout"`
	ReadBodyTimeout   Duration       `toml:"read_body_timeout"`
	IdleTimeout       Duration       `toml:"idle_timeout"`
	ShutdownGrace     Duration       `toml:"shutdown_grace"`
}

type HTTP struct {
	TrustedOrigins []string `toml:"trusted_origins"`
}

type Gateway struct {
	HeartbeatInterval Duration `toml:"heartbeat_interval"`
	SessionIdleClose  Duration `toml:"session_idle_close"`
	ReadLimitBytes    int64    `toml:"read_limit_bytes"`
	FrameBurst        int      `toml:"frame_burst"`
	FrameBurstMax     int      `toml:"frame_burst_max"`
}

type TLS struct {
	Mode               TLSMode `toml:"mode"`
	Email              string  `toml:"email"`
	Agreed             bool    `toml:"agreed"`
	CA                 string  `toml:"ca"`
	TestCA             string  `toml:"test_ca"`
	Profile            string  `toml:"profile"`
	RenewalWindowRatio float64 `toml:"renewal_window_ratio"`
	StorageDir         string  `toml:"storage_dir"`
	DNS                TLSDNS  `toml:"dns"`
}

type TLSDNS struct {
	Provider           string   `toml:"provider"`
	CredentialsFile    string   `toml:"credentials_file"`
	TTL                Duration `toml:"ttl"`
	PropagationDelay   Duration `toml:"propagation_delay"`
	PropagationTimeout Duration `toml:"propagation_timeout"`
	Resolvers          []string `toml:"resolvers"`
	OverrideDomain     string   `toml:"override_domain"`
}

type TURN struct {
	Enabled          bool     `toml:"enabled"`
	Listen           string   `toml:"listen"`
	Realm            string   `toml:"realm"`
	SharedSecretFile string   `toml:"shared_secret_file"`
	RelayIP          string   `toml:"relay_ip"`
	CredentialTTL    Duration `toml:"credential_ttl"`
	// MaxAllocationAge bounds a relay allocation: every request but Allocate is refused once this
	// long has passed since the credential was issued (F3, G33). Never shorter than CredentialTTL.
	MaxAllocationAge     Duration `toml:"max_allocation_age"`
	AllocationsPerDevice int      `toml:"allocations_per_device"`
	ProxyProtocol        bool     `toml:"proxy_protocol"`
	// PublicURL is the relay URL clients are handed instead of the derived one ("turns:" on
	// server.listen's port, or "turn:" on turn.listen's behind a proxy): set it when the port the
	// world reaches differs from the listen port, or when a proxy terminates TLS for the relay.
	PublicURL string `toml:"public_url"`
}

type LiveKit struct {
	Enabled bool   `toml:"enabled"`
	Mode    string `toml:"mode"`
	// BindAddress is where LiveKit listens: a loopback IP literal, never a hostname (Validate).
	BindAddress          string   `toml:"bind_address"`
	Port                 int      `toml:"port"`
	UDPPort              int      `toml:"udp_port"`
	TCPPort              int      `toml:"tcp_port"`
	NodeIP               string   `toml:"node_ip"`
	UseExternalIP        bool     `toml:"use_external_ip"`
	AdvertiseInternalIP  bool     `toml:"advertise_internal_ip"`
	STUNServers          []string `toml:"stun_servers"`
	APIKey               string   `toml:"api_key"`
	APISecretFile        string   `toml:"api_secret_file"`
	MaxVoiceParticipants int      `toml:"max_voice_participants"`
	MaxPublishers        int      `toml:"max_publishers"`
	WebhookListen        string   `toml:"webhook_listen"`
	// VP9 is refused by Validate until VP9 has an SFrame test vector and a run through LiveKit.
	VP9                 bool     `toml:"vp9"`
	MaxShareBitrateKbps int      `toml:"max_share_bitrate_kbps"`
	MaxAudioBitrateKbps int      `toml:"max_audio_bitrate_kbps"`
	LimitNumTracks      int      `toml:"limit_num_tracks"`
	LimitBytesPerSec    int      `toml:"limit_bytes_per_sec"`
	IPsExcludes         []string `toml:"ips_excludes"`
	ExtraConfigFile     string   `toml:"extra_config_file"`
}

type DB struct {
	Driver             string   `toml:"driver"`
	Path               string   `toml:"path"`
	DSN                string   `toml:"dsn"`
	MaxOpenConns       int      `toml:"max_open_conns"`
	ConnMaxLifetime    Duration `toml:"conn_max_lifetime"`
	AutoMigrate        bool     `toml:"auto_migrate"`
	PreMigrationBackup bool     `toml:"pre_migration_backup"`
}

type Blobs struct {
	Dir               string   `toml:"dir"`
	Backend           string   `toml:"backend"`
	MaxBlobBytes      int64    `toml:"max_blob_bytes"`
	QuotaBytesPerUser int64    `toml:"quota_bytes_per_user"`
	StoreMaxBytes     int64    `toml:"store_max_bytes"`
	PendingTTL        Duration `toml:"pending_ttl"`
	GCGrace           Duration `toml:"gc_grace"`
	GCInterval        Duration `toml:"gc_interval"`
	UploadTimeout     Duration `toml:"upload_timeout"`
	UploadsPerMinute  int      `toml:"uploads_per_minute"`
	UploadBytesPerDay int64    `toml:"upload_bytes_per_day"`
}

type Limits struct {
	MaxCiphertextBytes        int64 `toml:"max_ciphertext_bytes"`
	MaxAttachmentBytes        int64 `toml:"max_attachment_bytes"`
	MaxKeypackagesPerDevice   int   `toml:"max_keypackages_per_device"`
	KeypackageRefillThreshold int   `toml:"keypackage_refill_threshold"`
	Rate                      Rate  `toml:"rate"`
}

type Rate struct {
	Enabled              bool     `toml:"enabled"`
	SweepInterval        Duration `toml:"sweep_interval"`
	MaxKeys              int      `toml:"max_keys"`
	MaxRetryAfter        Duration `toml:"max_retry_after"`
	MessagePerSecond     float64  `toml:"message_per_second"`
	MessageBurst         int      `toml:"message_burst"`
	CommitPerSecond      float64  `toml:"commit_per_second"`
	CommitBurst          int      `toml:"commit_burst"`
	ProposalPerSecond    float64  `toml:"proposal_per_second"`
	ProposalBurst        int      `toml:"proposal_burst"`
	ReadPerSecond        float64  `toml:"read_per_second"`
	ReadBurst            int      `toml:"read_burst"`
	WritePerSecond       float64  `toml:"write_per_second"`
	WriteBurst           int      `toml:"write_burst"`
	LoginPerSecond       float64  `toml:"login_per_second"`
	LoginBurst           int      `toml:"login_burst"`
	LoginFailedPerSecond float64  `toml:"login_failed_per_second"`
	LoginFailedBurst     int      `toml:"login_failed_burst"`
	RegisterPerSecond    float64  `toml:"register_per_second"`
	RegisterBurst        int      `toml:"register_burst"`
	InvitePerSecond      float64  `toml:"invite_per_second"`
	InviteBurst          int      `toml:"invite_burst"`
	UnauthPerSecond      float64  `toml:"unauth_per_second"`
	UnauthBurst          int      `toml:"unauth_burst"`
	InvalidPerSecond     float64  `toml:"invalid_per_second"`
	InvalidBurst         int      `toml:"invalid_burst"`
	PasswordConcurrency  int      `toml:"password_concurrency"`
}

type Retention struct {
	HandshakeDays  int `toml:"handshake_days"`
	CiphertextDays int `toml:"ciphertext_days"`
}

type Registration struct {
	Mode          string `toml:"mode"`
	EmailRequired bool   `toml:"email_required"`
	Captcha       bool   `toml:"captcha"`
	AdminInvite   string `toml:"admin_invite" comment:"The 8-hex reference of the bootstrap invite dillad init minted. Never the code itself."`
}

type Auth struct {
	Methods  []string `toml:"methods"`
	Password Password `toml:"password"`
	TOTP     TOTP     `toml:"totp"`
	WebAuthn WebAuthn `toml:"webauthn"`
	OIDC     OIDC     `toml:"oidc"`
	Session  Session  `toml:"session"`
	Lockout  Lockout  `toml:"lockout"`
}

type Password struct {
	Argon2MemoryKiB     uint32 `toml:"argon2_memory_kib"`
	Argon2Iterations    uint32 `toml:"argon2_iterations"`
	Argon2Parallelism   uint8  `toml:"argon2_parallelism"`
	Argon2SaltBytes     uint32 `toml:"argon2_salt_bytes"`
	Argon2KeyBytes      uint32 `toml:"argon2_key_bytes"`
	HashMemoryBudgetMiB int    `toml:"hash_memory_budget_mib"`
	PepperFile          string `toml:"pepper_file"`
}

type TOTP struct {
	Issuer        string `toml:"issuer"`
	Period        uint   `toml:"period"`
	Skew          uint   `toml:"skew"`
	Digits        int    `toml:"digits"`
	Algorithm     string `toml:"algorithm"`
	SecretSize    uint   `toml:"secret_size"`
	RecoveryCodes int    `toml:"recovery_codes"`
}

type WebAuthn struct {
	RPID             string   `toml:"rp_id"`
	RPDisplayName    string   `toml:"rp_display_name"`
	RPOrigins        []string `toml:"rp_origins"`
	UserVerification string   `toml:"user_verification"`
}

type OIDC struct {
	Enabled          bool     `toml:"enabled"`
	Issuer           string   `toml:"issuer"`
	ClientID         string   `toml:"client_id"`
	ClientSecretFile string   `toml:"client_secret_file"`
	RedirectURL      string   `toml:"redirect_url"`
	Scopes           []string `toml:"scopes"`
	// AutoCreate is RESERVED and creates nothing today (deviation ID19,
	// ruling 40). An OIDC login whose subject maps to no account is
	// 403 E_FORBIDDEN whatever this says, because users.umk_pub,
	// users.ssk_pub and users.sig_umk_ssk are NOT NULL client-held key
	// material the server cannot invent. Setting it true changes only the
	// refusal's detail and what is logged, until a client-completed
	// registration leg (a one-time OIDC ticket POST /v1/accounts accepts in
	// place of an invite code) exists.
	AutoCreate bool `toml:"auto_create"`
}

type Session struct {
	NativeLifetime  Duration `toml:"native_lifetime"`
	BrowserLifetime Duration `toml:"browser_lifetime"`
	BrowserIdle     Duration `toml:"browser_idle"`
	MaxPerDevice    int      `toml:"max_per_device"`
	ReauthWindow    Duration `toml:"reauth_window"`
	// MaxDevicesPerUser bounds a user's unrevoked devices; a registration at the bound replaces the
	// oldest unlisted one and is refused only when every one is listed (L-HTTP-54, Q04, review F1).
	MaxDevicesPerUser int `toml:"max_devices_per_user"`
	// EnrolmentsPerHour bounds the live devices a user registers in any hour; past it a
	// registration replaces the oldest unlisted device instead of being refused (review F1).
	EnrolmentsPerHour int `toml:"enrolments_per_hour"`
}

type Lockout struct {
	ObservationWindow Duration `toml:"observation_window"`
	FreeAttempts      int      `toml:"free_attempts"`
	FirstLockout      Duration `toml:"first_lockout"`
	LockoutCeiling    Duration `toml:"lockout_ceiling"`
	HardCeiling       int      `toml:"hard_ceiling"`
}

type Log struct {
	Level     string `toml:"level"`
	Format    string `toml:"format"`
	AddSource bool   `toml:"add_source"`
}

type Metrics struct {
	Enabled      bool   `toml:"enabled"`
	Path         string `toml:"path"`
	RequireAdmin bool   `toml:"require_admin" comment:"When true, a scrape must carry Authorization: Bearer $DILLA_METRICS_TOKEN (an environment variable, never a dilla.toml key); unset, no scrape succeeds."`
}

type Doctor struct {
	ClockPeers   []string `toml:"clock_peers"`
	ClockSkewMax Duration `toml:"clock_skew_max"`
	TURNProbe    bool     `toml:"turn_probe"`
	UDPProbe     bool     `toml:"udp_probe"`
}

// BehindProxy is the derived predicate. There is no behind_proxy key.
func (c *Config) BehindProxy() bool { return c.TLS.Mode == TLSModeBehindProxy }
