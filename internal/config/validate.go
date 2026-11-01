package config

import (
	"errors"
	"fmt"
	"math"
	"net"
	"net/netip"
	"net/url"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"

	"github.com/jonasthim/dilla/internal/exit"
)

// minCredentialTTL is the shortest turn.credential_ttl: the relay accepts a credential issued up to
// 2 s ahead of its clock and `dillad doctor` mints a 10 s one, so a lifetime near that is refused at
// random.
const minCredentialTTL = time.Minute

// runtimeNumCPU is runtime.NumCPU behind a variable so a test can pin it.
var runtimeNumCPU = runtime.NumCPU

// Load reads path in strict mode over the pre-populated defaults, derives the
// documented empties and validates. Every failure carries exit.Config (78) and,
// for a parse failure, a line and a column.
func Load(path string) (*Config, error) {
	body, err := os.ReadFile(path) //nolint:gosec // G304: path is the operator's own --config argument
	if err != nil {
		return nil, fmt.Errorf("config: read %s: %w: %w", path, err, exit.Config)
	}
	c := Default()
	dec := toml.NewDecoder(strings.NewReader(string(body)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(c); err != nil {
		var strict *toml.StrictMissingError
		if errors.As(err, &strict) {
			// Position is a METHOD on *DecodeError, not a field: go-toml v2
			// v2.4.3 errors.go:82 declares
			// `func (e *DecodeError) Position() (row int, column int)` over
			// unexported line/column fields. And the message is String(), not
			// Error(): StrictMissingError.Error() returns only the fixed
			// sentence "strict mode: fields in the document are missing in the
			// target struct", while String() names each field in context.
			row, col := strict.Errors[0].Position()
			return nil, fmt.Errorf("config: %s: unknown key or table at line %d, column %d: %s: %w",
				path, row, col, strict.String(), exit.Config)
		}
		var derr *toml.DecodeError
		if errors.As(err, &derr) {
			row, col := derr.Position()
			return nil, fmt.Errorf("config: %s: line %d, column %d: %s: %w", path, row, col, derr.Error(), exit.Config)
		}
		return nil, fmt.Errorf("config: %s: %w: %w", path, err, exit.Config)
	}
	// Whether the operator WROTE turn.enabled, decoded rather than grepped: a
	// substring test for "enabled" matches [livekit], [metrics], [limits.rate]
	// and [auth.oidc] too, so it is true for essentially every real file and
	// Derive's behind_proxy default would never fire (R39 / D23).
	var presence struct {
		TURN struct {
			Enabled *bool `toml:"enabled"`
		} `toml:"turn"`
	}
	if err := toml.Unmarshal(body, &presence); err != nil {
		return nil, fmt.Errorf("config: %s: %w: %w", path, err, exit.Config)
	}
	c.turnExplicitlyEnabled = presence.TURN.Enabled != nil
	c.Derive()
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("config: %s: %w: %w", path, err, exit.Config)
	}
	return c, nil
}

// durations returns every duration key by its dotted TOML path.
func (c *Config) durations() map[string]Duration {
	return map[string]Duration{
		"server.read_header_timeout":      c.Server.ReadHeaderTimeout,
		"server.read_body_timeout":        c.Server.ReadBodyTimeout,
		"server.idle_timeout":             c.Server.IdleTimeout,
		"server.shutdown_grace":           c.Server.ShutdownGrace,
		"gateway.heartbeat_interval":      c.Gateway.HeartbeatInterval,
		"gateway.session_idle_close":      c.Gateway.SessionIdleClose,
		"tls.dns.ttl":                     c.TLS.DNS.TTL,
		"tls.dns.propagation_delay":       c.TLS.DNS.PropagationDelay,
		"tls.dns.propagation_timeout":     c.TLS.DNS.PropagationTimeout,
		"turn.credential_ttl":             c.TURN.CredentialTTL,
		"turn.max_allocation_age":         c.TURN.MaxAllocationAge,
		"db.conn_max_lifetime":            c.DB.ConnMaxLifetime,
		"blobs.pending_ttl":               c.Blobs.PendingTTL,
		"blobs.gc_grace":                  c.Blobs.GCGrace,
		"blobs.gc_interval":               c.Blobs.GCInterval,
		"blobs.upload_timeout":            c.Blobs.UploadTimeout,
		"limits.rate.sweep_interval":      c.Limits.Rate.SweepInterval,
		"limits.rate.max_retry_after":     c.Limits.Rate.MaxRetryAfter,
		"auth.session.native_lifetime":    c.Auth.Session.NativeLifetime,
		"auth.session.browser_lifetime":   c.Auth.Session.BrowserLifetime,
		"auth.session.browser_idle":       c.Auth.Session.BrowserIdle,
		"auth.session.reauth_window":      c.Auth.Session.ReauthWindow,
		"auth.lockout.observation_window": c.Auth.Lockout.ObservationWindow,
		"auth.lockout.first_lockout":      c.Auth.Lockout.FirstLockout,
		"auth.lockout.lockout_ceiling":    c.Auth.Lockout.LockoutCeiling,
		"doctor.clock_skew_max":           c.Doctor.ClockSkewMax,
	}
}

type rateBucket struct {
	perSecond float64
	burst     int
}

// rateBuckets returns the twelve (per_second, burst) pairs by name.
func (c *Config) rateBuckets() map[string]rateBucket {
	r := c.Limits.Rate
	return map[string]rateBucket{
		"message":      {r.MessagePerSecond, r.MessageBurst},
		"commit":       {r.CommitPerSecond, r.CommitBurst},
		"proposal":     {r.ProposalPerSecond, r.ProposalBurst},
		"read":         {r.ReadPerSecond, r.ReadBurst},
		"write":        {r.WritePerSecond, r.WriteBurst},
		"login":        {r.LoginPerSecond, r.LoginBurst},
		"login_failed": {r.LoginFailedPerSecond, r.LoginFailedBurst},
		"register":     {r.RegisterPerSecond, r.RegisterBurst},
		"invite":       {r.InvitePerSecond, r.InviteBurst},
		"unauth":       {r.UnauthPerSecond, r.UnauthBurst},
		"invalid":      {r.InvalidPerSecond, r.InvalidBurst},
	}
}

// Validate is the semantic half: strict mode catches spelling, this catches
// meaning. serve exits on the first finding; doctor calls it and prints all.
func (c *Config) Validate() error {
	var problems []error
	add := func(format string, args ...any) { problems = append(problems, fmt.Errorf(format, args...)) }

	for key, d := range c.durations() {
		if err := d.check(key); err != nil {
			add("%v", err)
		}
	}
	if c.Server.ReadBodyTimeout.Value() < 0 {
		add("server.read_body_timeout must not be negative")
	}

	if c.Instance.Domain == "" {
		add("instance.domain is required: certmagic, WebAuthn's RPID, the TURN realm and the OIDC redirect all derive from it")
	}
	if (c.BotSpotify.ClientID == "") != (c.BotSpotify.RedirectURL == "") {
		add("bot_spotify.client_id and bot_spotify.redirect_url must be configured together")
	} else if len(c.BotSpotify.ClientID) > 256 || strings.TrimSpace(c.BotSpotify.ClientID) != c.BotSpotify.ClientID || strings.ContainsAny(c.BotSpotify.ClientID, "\r\n\t ") {
		add("bot_spotify.client_id must be a bounded public application identifier")
	} else if c.BotSpotify.RedirectURL != "" {
		u, err := url.Parse(c.BotSpotify.RedirectURL)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" {
			add("bot_spotify.redirect_url must be an exact HTTPS URI registered with Spotify")
		}
	}
	if !c.BehindProxy() && !c.Instance.PublicIP.IsValid() {
		add("instance.public_ip is required unless tls.mode = \"behind_proxy\": LiveKit would otherwise STUN-discover its own address")
	}
	if c.BehindProxy() && len(c.Server.TrustedProxyCIDRs) == 0 {
		add("server.trusted_proxy_cidrs must be non-empty in behind_proxy mode, or every per-address rate bucket collapses into one")
	}
	if !c.BehindProxy() && len(c.Server.TrustedProxyCIDRs) != 0 {
		add("server.trusted_proxy_cidrs is only read in behind_proxy mode; a direct listener that honours X-Forwarded-For lets a client forge its own address")
	}
	if c.TLS.Mode != TLSModeBehindProxy && !c.TLS.Agreed {
		add("tls.agreed must be true for any acme_* mode")
	}
	if c.TLS.Mode == TLSModeACMEIP && c.TLS.Profile != "" && c.TLS.Profile != "shortlived" {
		add("tls.profile must be \"\" or \"shortlived\" in acme_ip mode: only shortlived permits IP SANs")
	}
	if c.TLS.Mode != TLSModeBehindProxy && c.TURN.Listen != "" {
		add("turn.listen is only read in behind_proxy mode; elsewhere TURN shares 443 through the demux")
	}
	if c.BehindProxy() && c.TURN.Enabled && c.TURN.Listen == "" {
		add("turn.listen is required when turn.enabled is true in behind_proxy mode")
	}
	if c.TURN.Enabled {
		if err := secretFile(c.TURN.SharedSecretFile, 32); err != nil {
			add("turn.shared_secret_file: %v", err)
		}
		if c.TURN.RelayIP == "" {
			add("turn.relay_ip is required when turn.enabled is true")
		} else if _, err := netip.ParseAddr(c.TURN.RelayIP); err != nil && c.TURN.RelayIP != RelayIPAuto {
			add("turn.relay_ip %q is neither an IP address of this host nor %q", c.TURN.RelayIP, RelayIPAuto)
		}
	}
	if c.TURN.PublicURL != "" && !validTURNURL(c.TURN.PublicURL) {
		add("turn.public_url %q is not a turn: or turns: URL with a host and a port, e.g. \"turns:turn.example.org:5349?transport=tcp\"",
			c.TURN.PublicURL)
	}
	if c.TURN.ProxyProtocol && c.TURN.Listen == "" {
		add("turn.proxy_protocol requires turn.listen")
	}
	// G34: a browser holds T × N × U allocations (T = 1 under max-bundle, N = its networks, U = 1
	// relay URL); 4 covers two networks through one ICE-restart overlap. 16 is far past any device.
	if n := c.TURN.AllocationsPerDevice; n < 1 || n > 16 {
		add("turn.allocations_per_device is %d; the range is 1..16", n)
	}
	if ttl := c.TURN.CredentialTTL.Value(); ttl < minCredentialTTL {
		add("turn.credential_ttl is %s; the minimum is %s (the relay judges a credential to the millisecond, with up to 2s of clock skew allowed between hosts)",
			c.TURN.CredentialTTL, minCredentialTTL)
	}
	if age, ttl := c.TURN.MaxAllocationAge.Value(), c.TURN.CredentialTTL.Value(); age < ttl {
		add("turn.max_allocation_age (%s) is shorter than turn.credential_ttl (%s): an allocation could not outlive the credential it was made with",
			c.TURN.MaxAllocationAge, c.TURN.CredentialTTL)
	}
	if c.LiveKit.Enabled {
		if c.LiveKit.Mode != "in_process" && c.LiveKit.Mode != "sidecar" {
			add("livekit.mode %q is not supported; choose in_process or sidecar", c.LiveKit.Mode)
		}
		if err := secretFile(c.LiveKit.APISecretFile, 32); err != nil {
			add("livekit.api_secret_file: %v", err)
		}
		if len(c.LiveKit.STUNServers) == 0 {
			add("livekit.stun_servers is empty after defaulting; LiveKit would append its own public STUN hosts")
		}
	}
	// Reserved until wired: dillad renders LiveKit's YAML itself, and a key that never reaches it must
	// fail loudly rather than be read and ignored.
	if c.LiveKit.ExtraConfigFile != "" {
		add("livekit.extra_config_file is reserved and not read yet; remove it")
	}
	// The publisher lease (dilla-media task 10) enforces any cap up to the room's own.
	if c.LiveKit.MaxPublishers < 1 || c.LiveKit.MaxPublishers > c.LiveKit.MaxVoiceParticipants {
		add("livekit.max_publishers is %d; the range is 1..livekit.max_voice_participants (%d)",
			c.LiveKit.MaxPublishers, c.LiveKit.MaxVoiceParticipants)
	}
	// The /rtc proxy is the only way into LiveKit's signalling and RoomService (protocol/09): a
	// non-loopback bind lets any holder of a LiveKit token skip the join gate, and a hostname binds
	// one resolved address while dillad's dials may reach another a local process holds (SFU-3).
	if a, err := netip.ParseAddr(c.LiveKit.BindAddress); err != nil || !a.IsLoopback() || a.Zone() != "" {
		add("livekit.bind_address is %q; it must be a loopback IP literal such as 127.0.0.1 or ::1 (not a hostname): "+
			"LiveKit's signalling and RoomService are reached only through dillad's /rtc gate", c.LiveKit.BindAddress)
	}
	// VP9 has a frame-prefix rule (0 clear bytes, protocol/05) but no SFrame test vector, and it has
	// never been measured through LiveKit; until both exist it stays off (crypto lens, parked VP9).
	if c.LiveKit.VP9 {
		add("livekit.vp9 = true is refused: VP9 has no SFrame test vector yet and has not been measured through LiveKit; " +
			"remove the key until the VP9 follow-up lands")
	}
	if c.LiveKit.UseExternalIP {
		add("livekit.use_external_ip is reserved and must stay false: livekit.node_ip is the address LiveKit advertises")
	}
	if err := validWebhookListen(c.LiveKit.WebhookListen); err != nil {
		add("livekit.webhook_listen: %v", err)
	}
	if !c.LiveKit.Enabled && c.LiveKit.WebhookListen != DefaultWebhookListen {
		add("livekit.webhook_listen is set while livekit.enabled is false: no SFU would post to it and no receiver would start")
	}
	if v := c.LiveKit.MaxShareBitrateKbps; v < 100 || v > 20000 {
		add("livekit.max_share_bitrate_kbps is %d; the range is 100..20000", v)
	}
	if v := c.LiveKit.MaxAudioBitrateKbps; v < 16 || v > 510 {
		add("livekit.max_audio_bitrate_kbps is %d; the range is 16..510 (Opus)", v)
	}
	if v := c.LiveKit.LimitNumTracks; v < 0 || v > math.MaxInt32 {
		add("livekit.limit_num_tracks is %d; the range is 0..%d", v, math.MaxInt32)
	}
	if c.LiveKit.LimitBytesPerSec < 0 {
		add("livekit.limit_bytes_per_sec is %d; it cannot be negative", c.LiveKit.LimitBytesPerSec)
	}
	for _, p := range c.LiveKit.IPsExcludes {
		if _, err := netip.ParsePrefix(p); err != nil {
			add("livekit.ips_excludes entry %q is not a CIDR prefix such as \"172.17.0.0/16\"", p)
		}
	}
	if c.Retention.HandshakeDays > 30 || c.Retention.HandshakeDays < 1 {
		add("retention.handshake_days is %d; the range is 1..30 (protocol/02 § Retention)", c.Retention.HandshakeDays)
	}
	if c.Retention.CiphertextDays > 30 || c.Retention.CiphertextDays < 1 {
		add("retention.ciphertext_days is %d; the range is 1..30", c.Retention.CiphertextDays)
	}
	if c.Limits.MaxCiphertextBytes < 4096 || c.Limits.MaxCiphertextBytes > 1<<20 {
		add("limits.max_ciphertext_bytes is %d; the range is 4096..1048576", c.Limits.MaxCiphertextBytes)
	}
	p := c.Auth.Password
	if p.Argon2Iterations < 2 {
		add("auth.password.argon2_iterations is %d; the floor is 2 (OWASP 2026) and 0 panics inside x/crypto/argon2", p.Argon2Iterations)
	}
	if p.Argon2Parallelism < 1 || p.Argon2Parallelism > 16 {
		add("auth.password.argon2_parallelism is %d; the range is 1..16", p.Argon2Parallelism)
	}
	if p.Argon2KeyBytes != 32 {
		add("auth.password.argon2_key_bytes must be 32")
	}
	if p.Argon2SaltBytes < 16 {
		add("auth.password.argon2_salt_bytes is %d; the floor is 16", p.Argon2SaltBytes)
	}
	if p.Argon2MemoryKiB < 19456 {
		add("auth.password.argon2_memory_kib is %d; the OWASP floor is 19456", p.Argon2MemoryKiB)
	}
	if p.PepperFile != "" {
		// R15 and §6.3: there is no pepper. x/crypto/argon2 exposes no secret
		// input, so a pepper_file that is read and ignored is worse than one
		// that is refused — the operator believes they have a defence they do
		// not have. The key stays in the struct so strict mode still names it.
		add("auth.password.pepper_file is set, but peppering is not implemented: x/crypto/argon2 exposes no secret input (R15). Remove the key")
	}
	if int(p.Argon2MemoryKiB) > p.HashMemoryBudgetMiB*1024 {
		add("auth.password.argon2_memory_kib (%d) exceeds hash_memory_budget_mib (%d MiB); the hashing semaphore would block every login until its context expired",
			p.Argon2MemoryKiB, p.HashMemoryBudgetMiB)
	}
	if n := c.Auth.Session.MaxDevicesPerUser; n < 1 || n > 64 {
		add("auth.session.max_devices_per_user is %d; the range is 1..64", n)
	}
	if n := c.Auth.Session.EnrolmentsPerHour; n < 1 || n > 60 {
		add("auth.session.enrolments_per_hour is %d; the range is 1..60", n)
	}
	if c.Limits.Rate.Enabled {
		for name, pair := range c.rateBuckets() {
			if pair.perSecond <= 0 {
				add("limits.rate.%s_per_second must be > 0: a zero rate.Limit allows no events", name)
			}
			if pair.burst < 1 {
				add("limits.rate.%s_burst must be >= 1", name)
			}
		}
		if s := c.Limits.Rate.SweepInterval.Value(); s < 5*time.Second || s > 5*time.Minute {
			add("limits.rate.sweep_interval is %s; the range is 5s..5m", s)
		}
	}
	if c.Limits.Rate.PasswordConcurrency > 2*runtimeNumCPU() {
		add("limits.rate.password_concurrency is %d; the ceiling is 2 x NumCPU", c.Limits.Rate.PasswordConcurrency)
	}
	if len(c.Auth.Methods) == 0 {
		add("auth.methods is empty")
	}
	for _, m := range c.Auth.Methods {
		if !slices.Contains([]string{"password", "totp", "passkey", "oidc"}, m) {
			add("auth.methods: %q is not one of password, totp, passkey, oidc", m)
		}
	}
	if len(c.Auth.Methods) > 0 && !slices.Contains(c.Auth.Methods, "totp") && !slices.Contains(c.Auth.Methods, "passkey") {
		add("auth.methods must include totp or passkey: every account needs a second factor")
	}
	// The client decodes at most 16 backup codes (web3.6), and a set of none would mint nothing.
	if n := c.Auth.TOTP.RecoveryCodes; n < 1 || n > 16 {
		add("auth.totp.recovery_codes is %d; the range is 1..16", n)
	}
	if slices.Contains(c.Auth.Methods, "oidc") && !c.Auth.OIDC.Enabled {
		add("auth.methods lists oidc but auth.oidc.enabled is false")
	}
	if c.Auth.OIDC.Enabled {
		if c.Auth.OIDC.Issuer == "" || c.Auth.OIDC.ClientID == "" {
			add("auth.oidc.issuer and auth.oidc.client_id are required when OIDC is enabled")
		}
		if err := secretFile(c.Auth.OIDC.ClientSecretFile, 1); err != nil {
			add("auth.oidc.client_secret_file: %v", err)
		}
	}
	if len(c.Auth.WebAuthn.RPOrigins) == 0 {
		add("auth.webauthn.rp_origins is empty after defaulting; go-webauthn refuses a config with no origin")
	}
	if c.Auth.TOTP.Digits != 6 && c.Auth.TOTP.Digits != 8 {
		add("auth.totp.digits is %d; the values are 6 and 8", c.Auth.TOTP.Digits)
	}
	if !slices.Contains([]string{"SHA1", "SHA256", "SHA512"}, c.Auth.TOTP.Algorithm) {
		add("auth.totp.algorithm is %q; the values are SHA1, SHA256, SHA512", c.Auth.TOTP.Algorithm)
	}
	if c.Auth.TOTP.Skew > 2 {
		add("auth.totp.skew is %d; a skew above 2 accepts more than five codes at once", c.Auth.TOTP.Skew)
	}
	switch c.DB.Driver {
	case "sqlite":
		if c.DB.Path == "" || c.DB.DSN != "" {
			add("db.driver = \"sqlite\" needs db.path and no db.dsn")
		}
	case "postgres":
		if c.DB.DSN == "" || c.DB.Path != "" {
			add("db.driver = \"postgres\" needs db.dsn and no db.path")
		}
	default:
		add("db.driver is %q; the values are sqlite and postgres", c.DB.Driver)
	}
	if !slices.Contains([]string{"public", "session"}, c.Instance.Discovery) {
		add("instance.discovery is %q; the values are public and session", c.Instance.Discovery)
	}
	if c.Doctor.ClockSkewMax.Value() < 2*time.Second {
		add("doctor.clock_skew_max is %s; HTTP-date has one-second granularity, so anything under 2s is noise", c.Doctor.ClockSkewMax.Value())
	}
	return errors.Join(problems...)
}

// validTURNURL is RFC 7065's shape as clients take it: "turn:" or "turns:", a host (an IPv6 literal
// in brackets), a numeric port, and an optional "?transport=..." query.
func validTURNURL(s string) bool {
	rest, ok := strings.CutPrefix(s, "turns:")
	if !ok {
		if rest, ok = strings.CutPrefix(s, "turn:"); !ok {
			return false
		}
	}
	hostport, _, _ := strings.Cut(rest, "?")
	host, port, err := net.SplitHostPort(hostport)
	if err != nil || host == "" || strings.ContainsAny(host, "/@ ") {
		return false
	}
	n, err := strconv.Atoi(port)
	return err == nil && n > 0 && n < 65536
}

// secretFile checks a *_file key: present, readable, mode 0600, at least minBytes bytes.
func secretFile(path string, minBytes int) error {
	if path == "" {
		return errors.New("is required")
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf("%s has mode %04o; a secret file must be 0600", path, perm)
	}
	if info.Size() < int64(minBytes) {
		return fmt.Errorf("%s is %d bytes; at least %d are required", path, info.Size(), minBytes)
	}
	return nil
}

// validWebhookListen: a loopback IP literal or localhost, with a port. LiveKit signs every webhook,
// but the receiver is loopback-only so nothing off the host can even try.
func validWebhookListen(v string) error {
	host, port, err := net.SplitHostPort(v)
	if err != nil {
		return fmt.Errorf("%q is not host:port", v)
	}
	if n, err := strconv.Atoi(port); err != nil || n < 0 || n > 65535 {
		return fmt.Errorf("%q has no valid port", v)
	}
	if host == "localhost" {
		return nil
	}
	if a, err := netip.ParseAddr(host); err != nil || !a.IsLoopback() {
		return fmt.Errorf("%q is not a loopback address (127.0.0.0/8, ::1 or localhost)", v)
	}
	return nil
}
