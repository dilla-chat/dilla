// Package api is dillad's HTTP surface over internal/store and internal/auth:
// the accounts, devices, device-list, invite, session, auth-ceremony and
// delivery-service routes, all of them deterministic CBOR (interfaces.md §5).
//
// It knows nothing about transport — internal/server owns the mux, the codec,
// the error body and the token buckets — and nothing about MLS, which reaches
// it through internal/ds.
package api

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/jonasthim/dilla/internal/auth"
	"github.com/jonasthim/dilla/internal/blob"
	botidentity "github.com/jonasthim/dilla/internal/bot/identity"
	botruntime "github.com/jonasthim/dilla/internal/bot/runtime"
	"github.com/jonasthim/dilla/internal/clock"
	"github.com/jonasthim/dilla/internal/config"
	"github.com/jonasthim/dilla/internal/ds"
	"github.com/jonasthim/dilla/internal/id"
	"github.com/jonasthim/dilla/internal/obs"
	"github.com/jonasthim/dilla/internal/server"
	"github.com/jonasthim/dilla/internal/store"
)

// Deps is everything the handlers need, declared once so no handler reaches for
// a package-level variable and no test has to build a server to exercise a
// route. Handlers hang off it by value: a Deps is a bag of pointers and is
// copied per route, never per request.
//
// A field a later task fills is declared nil here and named with that task, so
// no task silently widens a struct another task owns.
type Deps struct {
	Presence     *PresenceService
	Repo         store.Repository
	Clock        clock.Clock
	Log          *slog.Logger
	Limiter      *server.RateLimiter
	Metrics      *obs.Metrics
	Instance     store.InstanceRow
	Domain       string
	Config       *config.Config
	Registration config.Registration
	Sessions     *auth.Sessions

	// LandingFont is the same-origin path of the web client's JetBrains Mono woff2 the invite
	// landing page names in its @font-face (LandingFont(webFS)); "" keeps the system stack (R43).
	LandingFont string

	// ExternalSenderPub is the public half of the instance's current external-sender signing
	// key (protocol/03 § Instance keys), elements 9 and 10 of GET /v1/instance with
	// Instance.ExternalSenderKeyID: a client creating a text or call group puts it in the
	// group's external_senders extension. The composition root derives it from the key history;
	// the history itself is never served.
	ExternalSenderPub []byte

	// SignBotInteractionAdmission signs only the versioned admission payload with the
	// current instance external-sender key. The composition root owns the secret.
	SignBotInteractionAdmission func(payload []byte) ([]byte, error)

	// SignBotInteractionContext signs a versioned context snapshot under its own
	// domain, using the same instance key as admission proofs.
	SignBotInteractionContext func(payload []byte) ([]byte, error)

	// SignBotCurrentInteractionContext signs a human-authenticated current-group context
	// proof under the dedicated current-interaction domain.
	SignBotCurrentInteractionContext func(payload []byte) ([]byte, error)

	// SignAccountKind signs an account-kind attestation message (accountkind.go) with the
	// instance's current external-sender key, the key Instance.ExternalSenderKeyID names. The
	// composition root owns the secret.
	SignAccountKind func(message []byte) ([]byte, error)

	// WireVersions is element 0 of GET /v1/instance: the wire versions the gateway's hello offers
	// and is passed from the same composition options as the gateway.
	WireVersions  []uint64
	E2EEVersions  []uint64
	MediaVersions []uint64

	// CloseGateway closes a device's gateway connections inside the same
	// transaction that revokes it. Nil until part 1b's gateway exists; the
	// composition root wires it to Sessions.OnRevoke.
	CloseGateway func(deviceID id.ID)

	// MessageDeleted publishes the replayable gateway wakeup after a signed
	// moderation removal commits its durable marker and ciphertext tombstone.
	MessageDeleted func(groupID id.ID, sequence uint64, at int64)

	// Hasher is the Argon2id password hasher. It is an interface for the same
	// reason GatewayTickets is: the concrete *auth.Hasher is task 9's, and a
	// struct cannot name a type that does not exist yet. Nil until then, and a
	// registration that carries a password is refused rather than stored
	// unhashed.
	Hasher PasswordHasher

	// Tickets mints single-use gateway tickets. Nil here; part 1b task 17
	// supplies the implementation and task 12 registers the route, which
	// answers 501 while this is nil.
	Tickets GatewayTickets

	// Throttle is the login-attempt bucket and the account lockout ledger, and
	// Assertions holds the one-time enrolment assertions the three
	// unauthenticated ceremonies mint and POST /v1/devices/{device_id}/sessions
	// spends. Both are task 9's. Every route in auth.go refuses with
	// E_INTERNAL while either is nil rather than running unmetered.
	Throttle   *auth.Throttle
	Assertions *Assertions

	// StepUps holds the step-up tokens the factor-changing routes spend (protocol/09 § Second
	// factors). Register builds one when it is nil; it is memory only, like Assertions.
	StepUps *StepUps
	// CurrentBotInteractionGroupContext reloads and verifies the persisted MLS roster for an op63 grant.
	CurrentBotInteractionGroupContext func(context.Context, id.ID) (ds.CurrentBotInteractionGroupContext, error)

	// enrolments remembers whether each enrolment in flight began with a valid step-up, for the
	// confirm and finish legs' re-check under the user lock. Register builds it.
	enrolments *enrolments

	// Passkeys runs the four WebAuthn ceremonies. It is nil when `passkey` is
	// not in auth.methods — go-webauthn refuses a config with no RPOrigins, so
	// an instance that never configured a relying party has no Passkeys to
	// hand over — and the four routes answer 501 rather than panicking.
	Passkeys *auth.Passkeys

	// OIDC runs the optional host-login path and holds the pending-login table
	// the start and callback legs share. It is nil unless auth.oidc.enabled,
	// and the two routes answer 501 rather than panicking.
	OIDC *auth.OIDC

	// DeviceLists verifies every published device list in the guest; nil refuses every publish
	// with 500, fail closed.
	DeviceLists DeviceListVerifier

	// AfterDeviceList, when set, runs after an accepted PUT /v1/users/{id}/device-list, once the
	// 204 is flushed, on a context the request's cancellation does not reach. The delivery service
	// proposes an Add only for a device its user's newest signed list names (invariant 4), so a
	// device that published its KeyPackages before the list that names it (protocol/03 § Pairing,
	// steps 2 and 5) is brought into its user's DMs here. The composition root wires it to
	// SyncUserDMs; a failure is the hook's to log. revoked is the devices this publish revoked
	// (nil when none).
	AfterDeviceList func(ctx context.Context, userID id.ID, revoked []id.ID)

	// Blobs is the content-addressed store the backup routes (protocol/09 § Backups) write the
	// sealed header objects to: the instance's one blob store, shared with Plan 2's attachment
	// routes. Nil refuses PUT and the single-object GET with E_INTERNAL rather than half-writing.
	Blobs *blob.Store

	// UploadMeter is the per-user blob upload meter (blobs.uploads_per_minute and
	// blobs.upload_bytes_per_day) PUT /v1/backups spends, the one the composition root also hands
	// the attachment routes (Blobs.WithUploadMeter). Register builds one from Config when it is nil,
	// so the route is never unmetered.
	UploadMeter *UploadMeter

	// StateMeter is the per-DEVICE daily byte budget PUT /v1/backups/1/0 spends (the state object;
	// branch review BACKUPS-RECOVERY-01 as amended): StateBytesPerDevicePerDay, keyed by the
	// session's device, so a thief exhausting its own budget or the user's upload meter cannot
	// block the owner's device. Register builds it when it is nil.
	StateMeter *UploadMeter

	// BotIdentity verifies the public credential's native bot tuple in the real
	// WASI runtime. Nil refuses bot enrollment and final credential publication.
	BotIdentity *botidentity.Verifier
	// BotRuntime authenticates operator credentials independently of human device sessions.
	BotRuntime *botruntime.Manager
	// BotSpotifyClientID and BotSpotifyRedirectURL are public OAuth configuration.
	// The redirect must be the exact HTTPS URI registered with Spotify.
	BotSpotifyClientID    string
	BotSpotifyRedirectURL string
	botSpotify            *botSpotifyBroker
	// BotRevoked runs after disable/removal commits to close online transport and prompt MLS removals.
	BotRevoked func(context.Context, store.BotInstallationRow, bool)
	// BotEnrolled runs after a new bot identity, membership and role commit, so
	// the composition root can bring its channel groups in line with the new member.
	BotEnrolled func(context.Context, store.BotInstallationRow)
	// BotProfileChanged prompts live community members to refresh the bot's display name.
	BotProfileChanged func(context.Context, id.ID)
	// BotMembershipPending repairs missing eligible MLS joins after a Ready
	// transaction has rolled back. It must never run inside Repository.Tx.
	BotMembershipPending func(context.Context, store.BotInstallationRow, []id.ID)
	// BotJoinRoleGranted refreshes derived channel eligibility and live clients
	// after an episode-bound automatic role grant commits or its accepted receipt
	// is replayed. Replay repairs a crash after commit but before refresh.
	BotJoinRoleGranted func(context.Context, id.ID, id.ID)
}

// GatewayTickets is the one-method view api needs of internal/gateway's ticket
// store. The interface is declared here so Deps can carry a nil of it; the
// implementation is part 1b's (interfaces.md §1, §6.1).
//
// Its signature is *gateway.Tickets' own (deviation B35): a ticket stands in
// for the SESSION TOKEN on an upgrade that cannot carry an Authorization
// header, so the store keeps the token it redeems to, and the gateway resolves
// that token through the same auth.Sessions every other route does. The
// 1a-era `Mint(userID, deviceID) (string, int64)` had nothing to redeem to.
type GatewayTickets interface {
	Mint(deviceID id.ID, sessionToken string) (ticket string, expires time.Time, err error)
}

// PasswordHasher is the view api needs of internal/auth's Argon2id hasher. Both
// methods are bounded by a weighted semaphore inside the implementation and
// refuse with E_RATE_LIMITED over it, rather than queueing: Argon2id at the
// configured 19 456 KiB is a memory bomb an unauthenticated route must not be
// able to aim at the instance.
type PasswordHasher interface {
	Hash(ctx context.Context, pw string) (string, error)
	Verify(ctx context.Context, pw, phc string) (ok bool, needsRehash bool, err error)
}

// Body caps. interfaces.md §5.3: 64 KiB on every CBOR route but the two the
// delivery service and Plan 2's readable channels own.
const maxCBORBody = 64 << 10

// Register mounts every route this part of the plan owns. It is one call, so a
// route that exists in the document and not in the binary is a missing line
// here rather than a forgotten wiring in a composition root.
func Register(m *server.Mux, d Deps) {
	if d.botSpotify == nil {
		d.botSpotify = newBotSpotifyBroker(d.Clock)
	}
	if d.UploadMeter == nil && d.Config != nil {
		d.UploadMeter = NewUploadMeter(d.Clock, d.Config.Blobs)
	}
	if d.StateMeter == nil {
		d.StateMeter = NewStateMeter(d.Clock, StateBytesPerDevicePerDay)
	}
	if d.StepUps == nil {
		d.StepUps = NewStepUps(d.Clock, StepUpTTL)
	}
	if d.enrolments == nil {
		d.enrolments = newEnrolments(d.Clock, enrolmentTTL)
	}
	// Discovery. The two routes a client reads before it has anything else.
	registerInstance(m, d)
	registerBotInstallations(m, d)
	registerAutoModerationGrants(m, d)
	registerBotStructuralEvents(m, d)
	registerModerationRemovals(m, d)

	// Unauthenticated. The invite landing page and the two registration routes
	// are the only /v1 surface a caller reaches without a bearer token, so each
	// is metered here on its own §5.3 bucket, keyed by the client address,
	// rather than on the session-keyed ones every route below uses. The third
	// unauthenticated bucket §5.3 names, `unauth`, is deliberately not applied:
	// no route this task owns is on it, and the next unauthenticated surface —
	// task 12's session challenge — meters itself.
	d.RegisterPublicInvites(m)
	m.Handle("POST /v1/accounts", d.metered(classRegister, d.CreateAccount, refuseCBOR))

	// Auth ceremonies (protocol/09 § Auth ceremonies). The three
	// unauthenticated ones meter themselves on the `login` and `login_failed`
	// buckets inside the handler — they need the client address for the
	// lockout ledger anyway, so metering them here as well would take two
	// tokens per attempt.
	m.Handle("POST /v1/auth/password/login", http.HandlerFunc(d.PasswordLogin))
	m.Handle("POST /v1/auth/totp/verify", http.HandlerFunc(d.VerifyTOTP))
	m.Handle("POST /v1/auth/recovery/verify", http.HandlerFunc(d.VerifyRecovery))
	m.Handle("POST /v1/auth/password", d.enrolled(d.ChangePassword))
	m.Handle("POST /v1/auth/totp/enroll", d.owedOK(d.EnrollTOTP))
	m.Handle("POST /v1/auth/totp/confirm", d.owedOK(d.ConfirmTOTP))

	// Passkeys. The two register legs are enrolled sessions adding a credential
	// to an account that already exists; the two login legs are unauthenticated
	// and meter themselves on the `login` bucket inside the handler, exactly as
	// the password and second-factor ceremonies above do.
	m.Handle("POST /v1/auth/passkey/register/begin", d.owedOK(d.BeginPasskeyRegistration))
	m.Handle("POST /v1/auth/passkey/register/finish", d.owedOK(d.FinishPasskeyRegistration))
	m.Handle("POST /v1/auth/passkey/login/begin", http.HandlerFunc(d.BeginPasskeyLogin))
	m.Handle("POST /v1/auth/passkey/login/finish", http.HandlerFunc(d.FinishPasskeyLogin))

	// OIDC. Both legs are browser navigations rather than CBOR calls, and both
	// are unauthenticated, so they meter themselves on the `login` bucket
	// inside the handler exactly as the ceremonies above do.
	m.Handle("GET /v1/auth/oidc/start", http.HandlerFunc(d.StartOIDC))
	m.Handle("GET /v1/auth/oidc/callback", http.HandlerFunc(d.CallbackOIDC))

	// Second factors (protocol/09 § Second factors). The list is `E, F`: an owed account reads
	// what it has while it sets one up. Step-up, removal and fresh backup codes are `E`: an owed
	// account holds nothing to step up with or remove.
	m.Handle("GET /v1/auth/factors", d.owedOK(d.Factors))
	m.Handle("POST /v1/auth/stepup", d.enrolled(d.StepUp))
	m.Handle("POST /v1/auth/stepup/passkey/begin", d.enrolled(d.BeginPasskeyStepUp))
	m.Handle("POST /v1/auth/stepup/passkey/finish", d.enrolled(d.FinishPasskeyStepUp))
	m.Handle("POST /v1/auth/factors/remove", d.enrolled(d.RemoveFactor))
	m.Handle("POST /v1/auth/recovery/codes", d.enrolled(d.RecoveryCodes))

	// Accounts. Every one of these is an enrolled session; GET is also an owed one's (`E, F`).
	m.Handle("GET /v1/accounts/me", d.owedOK(d.GetMe))
	m.Handle("PATCH /v1/accounts/me/presence", d.enrolled(dsMeter(d.Limiter, dsClassWrite, d.PatchPresence)))
	m.Handle("POST /v1/accounts/me/presence/activity", d.enrolled(dsMeter(d.Limiter, dsClassWrite, d.PresenceActivity)))
	m.Handle("GET /v1/communities/{community_id}/presence", d.enrolled(dsMeter(d.Limiter, dsClassRead, d.PresenceSnapshot)))
	m.Handle("PATCH /v1/accounts/me", d.enrolled(dsMeter(d.Limiter, dsClassWrite, d.PatchMe)))
	m.Handle("DELETE /v1/accounts/me", d.enrolled(d.DeleteMe))

	// Devices. POST /v1/devices is enrolled only. The enrolling browser registers inside
	// establish; while pending it reads and publishes only its own device list. The
	// /v1/devices routes spend the device session's read and write buckets like the device-list and
	// backup routes (branch review REGISTRATION-DEVICES-03: unmetered, a stolen session polled GET
	// for a recovering owner's new row).
	m.Handle("POST /v1/devices", d.enrolled(dsMeter(d.Limiter, dsClassWrite, d.CreateDevice)))
	m.Handle("GET /v1/devices", d.enrolled(dsMeter(d.Limiter, dsClassRead, d.ListDevices)))
	m.Handle("PATCH /v1/devices/{device_id}", d.enrolled(dsMeter(d.Limiter, dsClassWrite, d.PatchDevice)))
	m.Handle("DELETE /v1/devices/{device_id}", d.enrolled(dsMeter(d.Limiter, dsClassWrite, d.DeleteDevice)))
	m.Handle("PUT /v1/users/{user_id}/device-list", d.scoped(dsMeter(d.Limiter, dsClassWrite, d.PutDeviceList), auth.ScopePending))
	m.Handle("GET /v1/users/{user_id}/device-list", d.scoped(dsMeter(d.Limiter, dsClassRead, d.GetDeviceList), auth.ScopePending))

	// Backups (protocol/09 § Backups; C14, F3, Q27). Mounted here and not through SessionRoute,
	// because the two reads admit a pending session (protocol/02 § Device sessions item 4): a device
	// entering the recovery key fetches the sealed objects before it holds a credential.
	m.Handle("PUT /v1/backups/{kind}/{chunk_seq}", d.enrolled(dsMeter(d.Limiter, dsClassWrite, d.PutBackup)))
	m.Handle("GET /v1/backups", d.scoped(dsMeter(d.Limiter, dsClassRead, d.ListBackups), auth.ScopePending))
	m.Handle("GET /v1/backups/{kind}/{chunk_seq}", d.scoped(dsMeter(d.Limiter, dsClassRead, d.GetBackup), auth.ScopePending))
	m.Handle("DELETE /v1/backups/{kind}/{chunk_seq}", d.enrolled(dsMeter(d.Limiter, dsClassWrite, d.DeleteBackup)))

	// Sessions. The challenge and establish routes carry NO session middleware:
	// they are how a session is obtained, so requiring one would be circular.
	// The challenge route meters itself on its own two keys inside the handler.
	m.Handle("POST /v1/devices/{device_id}/sessions/challenge", http.HandlerFunc(d.SessionChallenge))
	m.Handle("POST /v1/devices/{device_id}/sessions", http.HandlerFunc(d.SessionEstablish))
	m.Handle("POST /v1/sessions/negotiate", d.enrolled(d.NegotiateSession))
	// The sessions DELETE is on the device write bucket like DELETE /v1/devices/{device_id}, and
	// keeps the same ten-minute grace for another device's unlisted row (fix-wave review NEW-1).
	m.Handle("DELETE /v1/devices/{device_id}/sessions", d.owedOK(dsMeter(d.Limiter, dsClassWrite, d.SessionDelete)))

	// The gateway ticket. The route is mounted here and answers 501 until part
	// 1b task 17 fills Deps.Tickets, so the document and the binary agree on
	// which paths exist.
	m.Handle("POST /v1/gateway/ticket", d.enrolled(d.GatewayTicket))
}

// The two §5.3 buckets this task's routes are on. The names are config's own
// spelling (internal/config/validate.go's rateBuckets), so the knob an operator
// writes in dilla.toml and the class a route is metered on are one string.
const (
	classRegister = "register"
	classInvite   = "invite"
)

// metered wraps an unauthenticated handler in the named token bucket, keyed by
// the client address (IPv6 by /64 — server.RateKey). refuse writes the refusal
// in the shape that route answers in: CBOR for a /v1 route, and content
// negotiation for the landing page, which a browser renders.
func (d Deps) metered(class string, h http.HandlerFunc, refuse func(http.ResponseWriter, *http.Request, error)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := d.meter(class, r); err != nil {
			refuse(w, r, err)
			return
		}
		h(w, r)
	})
}

// meter takes one token from class's bucket for this caller and returns the
// refusal when there is none. The refusal carries the bucket's own deficit as
// retry_after_ms; server.WriteError turns that into the Retry-After header.
//
// It fails CLOSED. A nil Limiter or a nil Config is a composition-root bug, and
// the outcome of treating it as "no limit configured" is precisely what these
// buckets exist to prevent: an unmetered registration route. Every handler
// below this line is behind a session; these three are not.
func (d Deps) meter(class string, r *http.Request) error {
	if d.Limiter == nil || d.Config == nil {
		return server.Errorf(server.CodeInternal, "the %s rate bucket is not wired", class)
	}
	c, err := rateClass(d.Config.Limits.Rate, class)
	if err != nil {
		return err
	}
	key := server.RateKey(server.RealIP(r, d.Config.Server.TrustedProxyCIDRs))
	if ok, wait := d.Limiter.Allow(c, key); !ok {
		return server.RateLimitedAfter(wait)
	}
	return nil
}

// rateClass reads one (per_second, burst) pair out of the configured limits.
func rateClass(r config.Rate, name string) (server.Class, error) {
	switch name {
	case classRegister:
		return server.Class{Name: classRegister, PerSecond: r.RegisterPerSecond, Burst: r.RegisterBurst}, nil
	case classInvite:
		return server.Class{Name: classInvite, PerSecond: r.InvitePerSecond, Burst: r.InviteBurst}, nil
	}
	return server.Class{}, server.Errorf(server.CodeInternal, "no rate bucket named %q", name)
}

// refuseCBOR is the refusal writer of every route whose body is CBOR.
func refuseCBOR(w http.ResponseWriter, _ *http.Request, err error) { server.WriteError(w, err) }

func (d Deps) enrolled(h http.HandlerFunc) http.Handler {
	return d.scoped(h, auth.ScopeEnrolled)
}

func (d Deps) scoped(h http.HandlerFunc, want auth.Scope) http.Handler {
	return d.Sessions.Middleware(h, want)
}

// owedOK mounts one of protocol/09's `E, F` routes: an enrolled session, also one whose account
// owes a second factor (web3.6 S1). Every other enrolled route refuses such a session with
// E_FACTOR_REQUIRED; the list stays short, because it is what an owed account can do.
func (d Deps) owedOK(h http.HandlerFunc) http.Handler {
	return d.Sessions.MiddlewareOwedOK(h, auth.ScopeEnrolled)
}

// session is the resolved session of an authenticated route. A handler that
// reaches this and finds nothing was registered without the middleware, which
// is a programming error and not a client's fault.
func session(r *http.Request) (auth.Session, bool) {
	return auth.FromContext(r.Context())
}

// logf records a server-side cause that never reaches the client. Every refusal
// a client sees is a server.Error with a code from the one table; the detail
// behind an E_INTERNAL stays here.
func (d Deps) logf(r *http.Request, msg string, args ...any) {
	if d.Log == nil {
		return
	}
	d.Log.ErrorContext(r.Context(), msg, args...)
}

// notImplemented is the refusal for a route whose backing store method this
// part of the plan does not yet have. It is 501 with E_INTERNAL, whose detail
// WriteError blanks, so a client learns "not this instance, not yet" and
// nothing about the instance's internals.
func notImplemented(detail string) *server.Error {
	return server.WithStatus(http.StatusNotImplemented, server.Errorf(server.CodeInternal, "%s", detail))
}
