package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"net/http"
	"net/netip"
	"net/url"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/fxamacker/cbor/v2"

	"github.com/jonasthim/dilla/internal/auth"
	"github.com/jonasthim/dilla/internal/cborx"
	"github.com/jonasthim/dilla/internal/clock"
	"github.com/jonasthim/dilla/internal/id"
	"github.com/jonasthim/dilla/internal/server"
	"github.com/jonasthim/dilla/internal/store"
)

// Assertions holds the short-lived, one-time enrolment assertions that carry a
// host login across to POST /v1/devices/{device_id}/sessions. An assertion is
// NOT a session token: it reaches exactly one endpoint, is spent on first use
// and lives only in memory, so a restart invalidates every in-flight login
// rather than leaving a second bearer credential in the database.
type Assertions struct {
	clk clock.Clock
	ttl time.Duration

	mu    sync.Mutex
	items map[string]assertion
}

type assertion struct {
	userID  id.ID
	needs   uint8
	expires time.Time
}

// The `needs` of protocol/09 § Second factors: what a login still owes before its assertion is
// complete.
const (
	needsNothing uint8 = 0 // complete
	needsCode    uint8 = 1 // a TOTP code (/totp/verify) or a backup code (/recovery/verify)
	needsPasskey uint8 = 2 // passkeys and no TOTP: a passkey login, or a backup code
)

// auth.Sessions spends an assertion on the pending path of POST
// /v1/devices/{device_id}/sessions through this interface — internal/api
// imports internal/auth, so the seam has to point this way. The assertion below
// is what keeps the two signatures from drifting apart.
var _ auth.AssertionSpender = (*Assertions)(nil)

func NewAssertions(clk clock.Clock, ttl time.Duration) *Assertions {
	return &Assertions{clk: clk, ttl: ttl, items: map[string]assertion{}}
}

func (a *Assertions) Issue(userID id.ID, needs uint8) string {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		panic("api: crypto/rand failed: " + err.Error())
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sweepLocked()
	a.items[token] = assertion{userID: userID, needs: needs, expires: a.clk.Now().Add(a.ttl)}
	return token
}

// Spend consumes an assertion and reports whether it still owes anything: the
// auth.AssertionSpender view, for the session endpoint, which accepts only a
// complete assertion.
func (a *Assertions) Spend(token string) (id.ID, bool, bool) {
	userID, needs, ok := a.SpendNeeds(token)
	return userID, needs != needsNothing, ok
}

// SpendNeeds consumes an assertion and returns what it still owes. It deletes
// before it checks the expiry, so a replay of an expired token cannot be
// distinguished from a replay of a live one, and neither can be retried.
func (a *Assertions) SpendNeeds(token string) (id.ID, uint8, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	item, ok := a.items[token]
	if !ok {
		return id.ID{}, 0, false
	}
	delete(a.items, token)
	if a.clk.Now().After(item.expires) {
		return id.ID{}, 0, false
	}
	return item.userID, item.needs, true
}

// Upgrade replaces an assertion that has cleared its second factor with a fresh
// one that has not, so the TOTP and recovery routes hand back something the
// session endpoint will accept.
func (a *Assertions) Upgrade(userID id.ID) string { return a.Issue(userID, needsNothing) }

func (a *Assertions) sweepLocked() {
	now := a.clk.Now()
	for k, v := range a.items {
		if now.After(v.expires) {
			delete(a.items, k)
		}
	}
}

// AssertionTTL is how long an enrolment assertion lives. It is deliberately
// short: the only thing it can do is establish one device session, and a client
// that has just logged in is already holding the next request.
const AssertionTTL = 5 * time.Minute

// The login_attempts.method column. The numbers are this file's, because no
// other file writes the column.
const (
	methodPassword uint8 = 0
	methodTOTP     uint8 = 1
	methodRecovery uint8 = 2
)

// classLogin is the §5.3 bucket every unauthenticated auth ceremony is on. The
// second-factor routes share its rate but not its keys: secondFactorKey
// prefixes the address, so a client that has just spent a login token on the
// password step is not refused the TOTP step that must follow it.
const classLogin = "login"

// classLoginFailed is the per-address FAILURE budget. RecordFailure is its one
// writer; PasswordLogin only peeks at it.
const classLoginFailed = "login_failed"

func secondFactorKey(addr string) string { return "2fa\x00" + addr }

// passwordLoginRequest is [username(tstr), password(tstr)].
type passwordLoginRequest struct {
	_        struct{} `cbor:",toarray"`
	Username string
	Password string
}

// assertionRequest is the body both second-factor routes share:
// [assertion(tstr), code(tstr)].
type assertionRequest struct {
	_         struct{} `cbor:",toarray"`
	Assertion string
	Code      string
}

// PasswordLogin is POST /v1/auth/password/login →
// [assertion(tstr), needs(uint)]: what the login still owes (loginNeeds).
//
// An unknown handle, a handle that fails normalisation, an account with no
// password credential and a wrong password are four different server-side facts
// and one client-visible answer. The Hasher burns a dummy Argon2id hash for the
// first three so they cost the same wall time as the fourth, and loginLedger
// gives the first three a failure ledger of their own so they accrue, and are
// refused by, the same lockout a real account does: a 429 that only real
// accounts can earn is an enumeration oracle on the status line.
func (d Deps) PasswordLogin(w http.ResponseWriter, r *http.Request) {
	if d.Throttle == nil || d.Assertions == nil || d.Hasher == nil || d.Config == nil {
		d.logf(r, "api: the password login route is not wired")
		server.WriteError(w, server.Errorf(server.CodeInternal, "the password login route is not wired"))
		return
	}
	var req passwordLoginRequest
	if err := server.DecodeBody(w, r, maxCBORBody, &req); err != nil {
		server.WriteError(w, err)
		return
	}
	ctx := r.Context()
	ip := server.RealIP(r, d.Config.Server.TrustedProxyCIDRs)
	addrKey := server.RateKey(ip)

	// The standing gate, before a single byte of Argon2id is computed: the
	// attempt rate from this address. The `login_failed` bucket is NOT consulted
	// here, and the omission is deliberate — RateLimiter.Allow takes a token as
	// well as reporting, so checking it on arrival would meter every SUCCESSFUL
	// login on the failure bucket and spend an address's whole failure budget
	// on people who typed their password correctly. Failures reach it through
	// Throttle.RecordFailure, which is the one writer of that bucket.
	if ok, wait := d.Throttle.Allow(classLogin, addrKey); !ok {
		server.WriteError(w, server.RateLimitedAfter(wait))
		return
	}

	// And the address's FAILURE budget, PEEKED rather than spent. This is the
	// gate that makes "fifty failures across fifty accounts from one address
	// still throttle that address" true: the `login` bucket above is keyed by
	// address and by handle spelling, so an attacker spreading guesses over
	// many handles empties no handle's bucket, and the per-account lockout
	// below never sees two failures on the same account either. Only the
	// address-wide failure ledger counts that attacker, and Peek is how a
	// bucket whose one writer is Throttle.RecordFailure can be read here
	// without charging a correct password for someone else's guesses.
	if ok, wait := d.Throttle.Peek(classLoginFailed, addrKey); !ok {
		server.WriteError(w, server.RateLimitedAfter(wait))
		return
	}

	// The per-handle gate is keyed by the SPELLING, not by a user id, so an
	// account that exists and one that does not are metered identically and the
	// bucket is not itself an enumeration oracle.
	handle, herr := auth.NormalizeHandle(req.Username)
	spelling := req.Username
	if herr == nil {
		spelling = handle
		if ok, wait := d.Throttle.Allow(classLogin, "handle\x00"+handle); !ok {
			server.WriteError(w, server.RateLimitedAfter(wait))
			return
		}
	}

	var (
		user  store.UserRow
		found bool
		phc   string
	)
	if herr == nil {
		u, err := d.Repo.GetUserByUsername(ctx, handle)
		switch {
		case err == nil && u.DisabledAt == nil:
			user, found = u, true
			if stored, perr := d.Repo.GetPasswordCredential(ctx, u.ID); perr == nil {
				phc = stored
			} else if !errors.Is(perr, store.ErrNotFound) {
				server.WriteError(w, d.storeError(r, perr))
				return
			}
		case err != nil && !errors.Is(err, store.ErrNotFound):
			server.WriteError(w, d.storeError(r, err))
			return
		}
	}

	// The standing lockout gate, before the credential is looked at. Reporting
	// a lockout on the way out of a failed verify does not enforce it: the
	// attacker whose sixth guess happens to be CORRECT while the account is
	// nominally locked would be logged in and handed an assertion. The ledger
	// key is the same shape whether the account exists or not, so this refusal
	// says nothing about which.
	ledger := loginLedger(user, found, spelling)
	if locked := d.Throttle.LockedFor(ledger); locked > 0 {
		server.WriteError(w, server.RateLimitedAfter(locked))
		return
	}

	ok, _, err := d.Hasher.Verify(ctx, req.Password, phc)
	if err != nil {
		// The only error a bounded Hasher returns is E_RATE_LIMITED, which the
		// client may retry; anything else is already a *server.Error.
		server.WriteError(w, err)
		return
	}
	if !ok || !found {
		d.recordAttempt(r, user, found, addrKey, methodPassword, false)
		server.WriteError(w, d.failure(ledger, ip))
		return
	}

	d.recordAttempt(r, user, true, addrKey, methodPassword, true)

	needs, terr := d.loginNeeds(ctx, user.ID)
	if terr != nil {
		server.WriteError(w, d.storeError(r, terr))
		return
	}
	// A complete login clears the account's ledger, both the in-memory one the
	// lockout curve reads and the durable one the admin CLI reports. One that still
	// owes a factor does not: guesses at the code that follows accumulate on the
	// same ledger, and verifySecondFactor clears it once the code is right.
	if needs == needsNothing {
		d.Throttle.Clear(user.ID)
		if cerr := d.Repo.ClearLoginFailures(ctx, user.ID); cerr != nil {
			d.logf(r, "api: ClearLoginFailures", "err", cerr)
		}
	}
	d.write(w, r, http.StatusOK, []any{d.Assertions.Issue(user.ID, needs), uint64(needs)})
}

// rpID is the instance's relying party, auth.webauthn.rp_id: a passkey counts as a factor only
// under it.
func (d Deps) rpID() string {
	if d.Config == nil {
		return ""
	}
	return d.Config.Auth.WebAuthn.RPID
}

// holdsFactor is protocol/09 § Second factors' "holds a factor": a CONFIRMED TOTP secret, or at
// least one passkey registered for rpID. An unconfirmed TOTP row is a half-finished enrolment, and
// backup codes are not a factor. It is the one definition: the login rules, the step-up rule, the
// last-factor rule and require_mod_2fa all read it, inside their transaction where they write.
func holdsFactor(ctx context.Context, repo store.Repository, userID id.ID, rpID string) (bool, error) {
	t, err := repo.GetTOTP(ctx, userID)
	switch {
	case err == nil && t.ConfirmedAt != nil:
		return true, nil
	case err != nil && !errors.Is(err, store.ErrNotFound):
		return false, err
	}
	creds, err := repo.ListWebauthnCredentials(ctx, userID, rpID)
	if err != nil {
		return false, err
	}
	return len(creds) > 0, nil
}

// loginNeeds is what a password (or OIDC) login of the account still owes: a confirmed TOTP → 1
// (a code); else any passkey for this rp_id → 2 (a passkey login, or a backup code); else 0.
//
// It is one function because every route that ends a login has to make the same decision; one
// that answered differently would be a way around the factor the other enforces.
func (d Deps) loginNeeds(ctx context.Context, userID id.ID) (uint8, error) {
	t, err := d.Repo.GetTOTP(ctx, userID)
	switch {
	case err == nil && t.ConfirmedAt != nil:
		return needsCode, nil
	case err != nil && !errors.Is(err, store.ErrNotFound):
		return 0, err
	}
	creds, err := d.Repo.ListWebauthnCredentials(ctx, userID, d.rpID())
	if err != nil {
		return 0, err
	}
	if len(creds) > 0 {
		return needsPasskey, nil
	}
	return needsNothing, nil
}

// loginLedger is the key a login attempt's failures are counted under. A found
// account uses its own id; a handle with no account behind it uses a
// deterministic pseudo-id derived from the spelling that was tried, so an
// unknown handle accrues — and is refused by — exactly the same lockout a real
// one does.
//
// That symmetry is the point. Keying only real accounts and answering plain
// E_UNAUTHENTICATED for the rest makes the fifth wrong password a 429 with a
// retry_after_ms for a handle that exists and a 401 with a null one for a handle
// that does not, on the status line and in the body, which hands an enumerator
// the whole user list four attempts in and wastes the dummy-hash equaliser.
//
// A pseudo-id colliding with a real user id is a 128-bit coincidence, and would
// cost a shared failure ledger rather than anything authenticating.
func loginLedger(user store.UserRow, found bool, spelling string) id.ID {
	if found {
		return user.ID
	}
	sum := sha256.Sum256([]byte("dilla/login-ledger\x00" + spelling))
	var v id.ID
	copy(v[:], sum[:])
	return v
}

// failure is the one refusal every wrong credential gets. When the ledger has
// crossed free_attempts the refusal carries the lockout as retry_after_ms —
// which tells a legitimate user how long to wait and tells an attacker only
// what the growing delay already told them. The ADDRESS half of the same call
// is what counts an attacker guessing a thousand handles from one address; it
// is the one writer of the login_failed bucket PasswordLogin peeks at.
func (d Deps) failure(ledger id.ID, ip netip.Addr) error {
	if locked := d.Throttle.RecordFailure(ledger, ip); locked > 0 {
		return server.RateLimitedAfter(locked)
	}
	return server.Errorf(server.CodeUnauthenticated, "")
}

// recordAttempt writes one login_attempts row. A failure for an unknown account
// is recorded with a NULL user_id, so the address is still countable.
func (d Deps) recordAttempt(r *http.Request, user store.UserRow, found bool, addr string, method uint8, ok bool) {
	row := store.LoginAttemptRow{IP: addr, Method: method, OK: ok, At: d.Clock.Now().Unix()}
	if found {
		uid := user.ID
		row.UserID = &uid
	}
	if err := d.Repo.RecordLoginAttempt(r.Context(), row); err != nil {
		d.logf(r, "api: RecordLoginAttempt", "err", err)
	}
}

// ChangePassword is POST /v1/auth/password: [old(tstr|null), new(tstr), stepup(tstr|null)?] → 204.
// The third element is optional; a two-element body is a null step-up.
//
// protocol/09-http-api.md marks this route "E (step-up)", and it has two kinds
// of account to step up. Where the account HAS a credential the old password is
// the step-up, and it is required: a stolen session token must not be enough to
// lock the owner out of their own account. Where it has none — registered
// through a passkey or OIDC — there is no credential to re-present, so the
// step-up falls back to the session's own freshness, exactly as DELETE
// /v1/accounts/me does. An enrolled session alone must NOT be enough there
// either: planting a password on a passkey-only account creates a second,
// persistent login path that survives revoking the passkey.
//
// Nor is freshness enough once the account holds a second factor (web3.6 review
// M7): a browser re-establishes a fresh session from its device key at will, so
// a stolen browser could plant a password, step up with it (method 0) and add or
// remove factors the passkey step-up was meant to guard. A first password on
// such an account also needs a step-up token (403 E_STEP_UP otherwise). It is
// checked after the freshness check and before the hash, so a stale session
// leaves the token unspent and a bad token costs no Argon2 work, and it is spent
// after the hash, so a failed hash leaves it for the retry (web3.7 R33).
func (d Deps) ChangePassword(w http.ResponseWriter, r *http.Request) {
	sess, ok := session(r)
	if !ok {
		server.WriteError(w, server.Errorf(server.CodeUnauthenticated, ""))
		return
	}
	// Config carries the step-up window, so a nil one fails CLOSED for the same
	// reason DeleteMe's does: skipping the only gate a branch has because the
	// dependency that holds it is missing is fail-open, and a handler test built
	// without a Config would exercise no gate at all.
	if d.Hasher == nil || d.Throttle == nil || d.Config == nil {
		d.logf(r, "api: the password change route is not wired")
		server.WriteError(w, server.Errorf(server.CodeInternal, "the password change route is not wired"))
		return
	}
	var req struct {
		Old    *string
		New    string
		StepUp *string
	}
	var body []cbor.RawMessage
	if err := server.DecodeBody(w, r, maxCBORBody, &body); err != nil {
		server.WriteError(w, err)
		return
	}
	if len(body) != 2 && len(body) != 3 {
		server.WriteError(w, server.Errorf(server.CodeInvalidRequest, "the body is [old, new] or [old, new, stepup]"))
		return
	}
	fields := []any{&req.Old, &req.New, &req.StepUp}
	for i, raw := range body {
		if err := cborx.Unmarshal(raw, fields[i]); err != nil {
			server.WriteError(w, server.Errorf(server.CodeInvalidRequest, "body element %d: %v", i, err))
			return
		}
	}
	if req.New == "" {
		server.WriteError(w, server.Errorf(server.CodeInvalidRequest, "the new password is empty"))
		return
	}
	ctx := r.Context()
	phc, err := d.Repo.GetPasswordCredential(ctx, sess.UserID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		server.WriteError(w, d.storeError(r, err))
		return
	}
	if phc != "" {
		if req.Old == nil {
			server.WriteError(w, server.Errorf(server.CodeForbidden, "changing a password needs the old one"))
			return
		}
		// The old password is this route's step-up, so guesses at it are counted on the step-up
		// ledgers (before the check, per account across addresses), never on the owner's login
		// ledger: a thief behind a stolen session neither guesses freely nor locks the owner out.
		try, gerr := d.stepUpGate(r, sess.UserID)
		if gerr != nil {
			server.WriteError(w, gerr)
			return
		}
		valid, _, verr := d.Hasher.Verify(ctx, *req.Old, phc)
		if verr != nil {
			try.cancel()
			server.WriteError(w, verr)
			return
		}
		if !valid {
			if locked := try.fail(d, r); locked > 0 {
				server.WriteError(w, server.RateLimitedAfter(locked))
				return
			}
			server.WriteError(w, server.Errorf(server.CodeForbidden, "the old password is wrong"))
			return
		}
		try.pass()
	} else {
		now := d.Clock.Now().Unix()
		row, serr := d.Repo.GetSessionByHash(ctx, sess.TokenHash, now)
		if serr != nil {
			server.WriteError(w, server.Errorf(server.CodeUnauthenticated, ""))
			return
		}
		if window := int64(d.Config.Auth.Session.ReauthWindow.Value().Seconds()); now-row.Created > window {
			server.WriteError(w, server.Errorf(server.CodeForbidden,
				"setting a first password needs a session established in the last %d seconds", window))
			return
		}
		// Checked, not spent, before the hash: a missing or bad token costs no Argon2 work (R33).
		if err := d.checkStepUp(ctx, sess, req.StepUp); err != nil {
			server.WriteError(w, d.storeError(r, err))
			return
		}
	}
	next, err := d.Hasher.Hash(ctx, req.New)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	if phc == "" {
		// Spent after the hash, so a failed hash leaves the token for the retry. The spend is still
		// the decision: of two requests that both passed the check, the one that finds the token
		// spent is refused 403 E_STEP_UP. A store failure in ChangePasswordCredential after the spend
		// still spends it: the person steps up again.
		if _, err := d.requireStepUp(ctx, sess, req.StepUp); err != nil {
			server.WriteError(w, d.storeError(r, err))
			return
		}
	}
	now := d.Clock.Now().Unix()
	// Task 11 (Q7): Settings → Sign-in says "Changed {relative}" from the change time, written with the hash in one statement.
	if err := d.Repo.ChangePasswordCredential(ctx, sess.UserID, next, now); err != nil {
		server.WriteError(w, d.storeError(r, err))
		return
	}
	d.Throttle.Clear(sess.UserID)
	d.noContent(w, r)
}

// totpParams reads the instance's TOTP policy. The issuer falls back to the
// instance domain, because pquerna/otp refuses to mint a key without one and an
// operator who left auth.totp.issuer empty still gets a usable QR code.
func (d Deps) totpParams() auth.TOTPParams {
	c := d.Config.Auth.TOTP
	issuer := c.Issuer
	if issuer == "" {
		issuer = d.Domain
	}
	return auth.TOTPParams{Issuer: issuer, Period: c.Period, Skew: c.Skew,
		SecretSize: c.SecretSize, Digits: c.Digits, Algorithm: c.Algorithm}
}

// EnrollTOTP is POST /v1/auth/totp/enroll: [stepup(tstr|null)] →
// [secret(tstr), otpauth_url(tstr)].
//
// The row is written UNCONFIRMED (confirmed_at NULL): an enrolment nobody has
// proved they can compute codes for must never make the account's second factor
// mandatory, or a mistyped QR code locks the owner out. POST
// /v1/auth/totp/confirm is what sets confirmed_at.
//
// An account that already has a CONFIRMED authenticator is refused. totp_secrets
// is one row per user, so enrolling over a confirmed secret would overwrite it
// and clear confirmed_at — a stolen session token could turn the victim's second
// factor off by starting an enrolment it never finishes. Removing one is POST
// /v1/auth/factors/remove. The refusal is taken again under the user lock, so an
// enrol racing another device's confirm cannot overwrite the secret it confirmed.
//
// When the account already holds a factor the enrolment needs a step-up token
// (403 E_STEP_UP otherwise); when it holds none the token is ignored. Whether the
// begin carried a valid step-up is recorded for the confirm, which re-checks the
// rule under the user lock (the begin/confirm window).
//
// The secret is stored as its base32 TEXT in the BLOB column, which is the same
// spelling the otpauth URL carries and the one pquerna/otp validates against;
// nothing re-encodes it on the way back out.
func (d Deps) EnrollTOTP(w http.ResponseWriter, r *http.Request) {
	sess, ok := session(r)
	if !ok {
		server.WriteError(w, server.Errorf(server.CodeUnauthenticated, ""))
		return
	}
	if d.Config == nil || d.StepUps == nil || d.enrolments == nil {
		d.logf(r, "api: the TOTP enrolment route is not wired")
		server.WriteError(w, server.Errorf(server.CodeInternal, "the TOTP enrolment route is not wired"))
		return
	}
	var req stepUpRequest
	if err := server.DecodeBody(w, r, maxCBORBody, &req); err != nil {
		server.WriteError(w, err)
		return
	}
	ctx := r.Context()
	if err := refuseConfirmedTOTP(ctx, d.Repo, sess.UserID); err != nil {
		server.WriteError(w, d.storeError(r, err))
		return
	}
	steppedUp, err := d.requireStepUp(ctx, sess, req.StepUp)
	if err != nil {
		server.WriteError(w, d.storeError(r, err))
		return
	}
	u, err := d.Repo.GetUser(ctx, sess.UserID)
	if err != nil {
		server.WriteError(w, d.storeError(r, err))
		return
	}
	p := d.totpParams()
	secret, url, err := auth.GenerateTOTP(p.Issuer, u.Username+"@"+d.Domain, p)
	if err != nil {
		d.logf(r, "api: GenerateTOTP", "err", err)
		server.WriteError(w, server.Errorf(server.CodeInternal, ""))
		return
	}
	now := d.Clock.Now().Unix()
	if err := d.Repo.Tx(ctx, func(tx store.Repository) error {
		if err := tx.LockUserForDeviceRegistration(ctx, sess.UserID); err != nil {
			return err
		}
		if err := refuseConfirmedTOTP(ctx, tx, sess.UserID); err != nil {
			return err
		}
		if err := tx.PutTOTP(ctx, store.TOTPRow{
			UserID: sess.UserID, Secret: []byte(secret), Digits: uint64(p.Digits), //nolint:gosec // G115: TOTP digits are 6 or 8
			Period: uint64(p.Period), Algorithm: p.Algorithm, Created: now,
		}); err != nil {
			return err
		}
		// Recorded under the lock, in the order the rows are written, and bound to this
		// secret: a confirm inherits this step-up only for the secret this enroll wrote, never
		// for one a racing enrolment put in the row since. A transaction that then fails to
		// commit leaves a record bound to a secret no row holds, which nothing can match.
		d.enrolments.record(totpEnrolment(sess.UserID), sess.UserID, steppedUp, []byte(secret))
		return nil
	}); err != nil {
		server.WriteError(w, d.storeError(r, err))
		return
	}
	d.write(w, r, http.StatusOK, []any{secret, url})
}

// refuseConfirmedTOTP is EnrollTOTP's 403 E_FORBIDDEN over a confirmed secret.
func refuseConfirmedTOTP(ctx context.Context, repo store.Repository, userID id.ID) error {
	existing, err := repo.GetTOTP(ctx, userID)
	switch {
	case err == nil && existing.ConfirmedAt != nil:
		return server.Errorf(server.CodeForbidden, "this account already has a confirmed authenticator")
	case err != nil && !errors.Is(err, store.ErrNotFound):
		return err
	}
	return nil
}

// ConfirmTOTP is POST /v1/auth/totp/confirm: [code(tstr)] → [backup_codes([tstr])].
//
// One transaction under the user lock: re-read the enrolment and check the code against it,
// re-check the step-up rule (a factor that appeared since an enrol that carried no step-up is 403
// E_STEP_UP, nothing written), confirm the row with the accepted counter consumed, clear
// factor_owed, and mint ten backup codes only when the account holds no unused one — re-minting on
// every confirm silently invalidated codes a person had already printed.
//
// No pending enrolment (none, or the row is already confirmed) is 404 E_NOT_FOUND; a code that
// does not match it is 400 E_INVALID_REQUEST.
func (d Deps) ConfirmTOTP(w http.ResponseWriter, r *http.Request) {
	sess, ok := session(r)
	if !ok {
		server.WriteError(w, server.Errorf(server.CodeUnauthenticated, ""))
		return
	}
	if d.Config == nil || d.enrolments == nil {
		d.logf(r, "api: the TOTP confirm route is not wired")
		server.WriteError(w, server.Errorf(server.CodeInternal, "the TOTP confirm route is not wired"))
		return
	}
	var req struct {
		_    struct{} `cbor:",toarray"`
		Code string
	}
	if err := server.DecodeBody(w, r, maxCBORBody, &req); err != nil {
		server.WriteError(w, err)
		return
	}
	ctx := r.Context()
	key := totpEnrolment(sess.UserID)
	var codes []string
	if err := d.Repo.Tx(ctx, func(tx store.Repository) error {
		if err := tx.LockUserForDeviceRegistration(ctx, sess.UserID); err != nil {
			return err
		}
		row, err := tx.GetTOTP(ctx, sess.UserID)
		if err != nil {
			return err
		}
		if row.ConfirmedAt != nil {
			return server.Errorf(server.CodeNotFound, "there is no enrolment to confirm")
		}
		counter, ok := auth.ValidateTOTP(req.Code, string(row.Secret), rowParams(row, d.totpParams()), d.Clock.Now(), row.LastCounter)
		if !ok || counter <= row.LastCounter {
			return server.Errorf(server.CodeInvalidRequest, "the code does not match this secret")
		}
		// The enroll's step-up counts only for the secret that enroll wrote (M1).
		steppedUp := d.enrolments.steppedUp(key, sess.UserID, row.Secret)
		if err := d.recheckStepUp(ctx, tx, sess.UserID, steppedUp); err != nil {
			return err
		}
		now := d.Clock.Now().Unix()
		row.ConfirmedAt = &now
		row.LastCounter = counter
		if err := tx.PutTOTP(ctx, row); err != nil {
			return err
		}
		codes, err = d.settleFirstFactor(ctx, tx, sess.UserID, now)
		return err
	}); err != nil {
		server.WriteError(w, d.storeError(r, err))
		return
	}
	d.enrolments.drop(key)
	d.write(w, r, http.StatusOK, []any{stringsAny(codes)})
}

// rowParams is the enrolled policy: digits, period and algorithm come from the
// ROW, not from config, so changing auth.totp.* does not invalidate every
// authenticator already enrolled. Only the skew, which is a tolerance rather
// than part of the secret, comes from the current config.
func rowParams(row store.TOTPRow, current auth.TOTPParams) auth.TOTPParams {
	return auth.TOTPParams{
		Issuer: current.Issuer, Period: uint(row.Period), Skew: current.Skew,
		SecretSize: current.SecretSize, Digits: int(row.Digits), Algorithm: row.Algorithm, //nolint:gosec // G115: TOTP digits are 6 or 8
	}
}

// VerifyTOTP is POST /v1/auth/totp/verify: [assertion(tstr), code(tstr)] →
// [assertion(tstr)].
//
// ConsumeTOTPCounter, not the library, is the replay guard: pquerna/otp is
// stateless and accepts the same code at every offset inside the skew window
// for the whole of that window. The store's UPDATE fires only while the
// presented counter is strictly greater than the stored one, so a code accepted
// once writes zero rows the second time and the verify is refused even though
// the code is still "valid".
func (d Deps) VerifyTOTP(w http.ResponseWriter, r *http.Request) {
	// Only an assertion owing a code (needs 1): a needs-2 assertion's account had no confirmed
	// TOTP at login, and a TOTP confirmed since must not stand in for the passkey it owes.
	accept := func(needs uint8) bool { return needs == needsCode }
	d.verifySecondFactor(w, r, methodTOTP, accept, func(userID id.ID, code string) error {
		ctx := r.Context()
		row, err := d.Repo.GetTOTP(ctx, userID)
		if err != nil {
			return err
		}
		if row.ConfirmedAt == nil {
			return store.ErrNotFound
		}
		counter, ok := auth.ValidateTOTP(code, string(row.Secret), rowParams(row, d.totpParams()), d.Clock.Now(), row.LastCounter)
		if !ok {
			return store.ErrNotFound
		}
		return d.Repo.ConsumeTOTPCounter(ctx, userID, counter)
	})
}

// VerifyRecovery is POST /v1/auth/recovery/verify: [assertion, code] →
// [assertion]. ConsumeRecoveryCode's UPDATE — `used_at IS NULL` — is the
// single-use guard, exactly as the counter is TOTP's. A backup code clears an
// assertion owing either a code or a passkey (needs 1 or 2).
func (d Deps) VerifyRecovery(w http.ResponseWriter, r *http.Request) {
	accept := func(needs uint8) bool { return needs == needsCode || needs == needsPasskey }
	d.verifySecondFactor(w, r, methodRecovery, accept, func(userID id.ID, code string) error {
		return d.Repo.ConsumeRecoveryCode(r.Context(), userID,
			auth.RecoveryCodeHash(code), d.Clock.Now().Unix())
	})
}

// verifySecondFactor is the body both second-factor routes share: spend the
// assertion, run check, and on success hand back a fresh assertion that no
// longer needs a second factor.
//
// The assertion is spent BEFORE check runs, so a wrong code costs the client its
// assertion and a fresh password login. That is the point: without it the
// assertion is a bearer token an attacker may brute-force six digits against.
func (d Deps) verifySecondFactor(w http.ResponseWriter, r *http.Request, method uint8,
	accept func(needs uint8) bool, check func(id.ID, string) error) {
	if d.Throttle == nil || d.Assertions == nil || d.Config == nil {
		d.logf(r, "api: a second-factor route is not wired")
		server.WriteError(w, server.Errorf(server.CodeInternal, "the second-factor routes are not wired"))
		return
	}
	var req assertionRequest
	if err := server.DecodeBody(w, r, maxCBORBody, &req); err != nil {
		server.WriteError(w, err)
		return
	}
	ip := server.RealIP(r, d.Config.Server.TrustedProxyCIDRs)
	addrKey := server.RateKey(ip)
	if ok, wait := d.Throttle.Allow(classLogin, secondFactorKey(addrKey)); !ok {
		server.WriteError(w, server.RateLimitedAfter(wait))
		return
	}
	userID, needs, ok := d.Assertions.SpendNeeds(req.Assertion)
	if !ok || !accept(needs) {
		// An unknown, expired, replayed or already-cleared assertion, or one owing
		// something this route cannot clear, is one answer: there is nothing here
		// a client can act on but "log in again".
		server.WriteError(w, server.Errorf(server.CodeUnauthenticated, ""))
		return
	}
	// The guess is counted on the account ledger BEFORE the code is compared, in one step
	// with the lockout check (auth.Throttle.Begin), so a burst of assertions minted while the
	// account was unlocked cannot all reach the comparison, and a locked account is refused
	// before its code is looked at. The assertion is already spent.
	attempt, locked := d.Throttle.Begin(userID)
	if locked > 0 {
		server.WriteError(w, server.RateLimitedAfter(locked))
		return
	}
	if err := check(userID, req.Code); err != nil {
		if !errors.Is(err, store.ErrNotFound) && !errors.Is(err, store.ErrConflict) {
			// Undecided: a store failure is not a guess, and refunds only itself.
			attempt.Pass()
			server.WriteError(w, d.storeError(r, err))
			return
		}
		d.recordAttempt(r, store.UserRow{ID: userID}, true, addrKey, method, false)
		if locked := attempt.Fail(ip); locked > 0 {
			server.WriteError(w, server.RateLimitedAfter(locked))
			return
		}
		server.WriteError(w, server.Errorf(server.CodeUnauthenticated, ""))
		return
	}
	// A complete login: the guess is refunded and the account's ledger cleared.
	attempt.Pass(userID)
	d.recordAttempt(r, store.UserRow{ID: userID}, true, addrKey, method, true)
	d.write(w, r, http.StatusOK, []any{d.Assertions.Upgrade(userID)})
}

// Passkeys (protocol/09-http-api.md § Auth ceremonies).
//
// internal/auth owns the ceremonies; the four handlers below are the CBOR skin
// over them. `options` and `response` cross the wire as opaque `tstr`: they are
// the WebAuthn JSON navigator.credentials produces and consumes, and dillad
// neither re-encodes nor inspects them.

// ceremonyRequest is the body both finish routes share:
// [ceremony_id(bstr16), response(tstr)].
type ceremonyRequest struct {
	_          struct{} `cbor:",toarray"`
	CeremonyID id.ID
	Response   string
}

// methodPasskey is the login_attempts.method value of a passkey assertion. It
// follows recovery because this file is the only writer of that column and the
// three values above it are already spent; it is NOT the instance document's
// `auth_methods` numbering, which is a different enumeration in a different
// message and where 2, not 3, is the passkey.
const methodPasskey uint8 = 3

// passkeyFailureLedger is the account ledger a failed discoverable login is
// charged to. A discoverable ceremony never names an account, so there is no
// per-account lockout to earn and no handle to enumerate; the entry exists only
// because RecordFailure is also what charges the ADDRESS, which is the budget
// every unauthenticated ceremony in this file peeks at and the one an attacker
// spraying assertions empties. Nothing gates on this key.
var passkeyFailureLedger = loginLedger(store.UserRow{}, false, "\x00passkey")

// passkeys returns the ceremony runner, or the refusal a route answers when the
// instance has none. A nil Passkeys is not a composition bug: `passkey` may
// simply be absent from auth.methods, and go-webauthn refuses to build at all
// without an RPOrigins, so an instance that configured no relying party
// legitimately has nothing here.
func (d Deps) passkeys() (*auth.Passkeys, error) {
	if d.Passkeys == nil {
		return nil, notImplemented("passkeys are not enabled on this instance")
	}
	return d.Passkeys, nil
}

// BeginPasskeyRegistration is POST /v1/auth/passkey/register/begin:
// [stepup(tstr|null)] → [ceremony_id(bstr16), options(tstr)].
//
// The step-up rule is EnrollTOTP's, and what the begin carried is recorded under the ceremony id
// for the finish. The account's own credentials are excluded in the options. An account already
// holding MaxPasskeys passkeys for this rp_id is 409 E_INVALID_REQUEST, before any token is spent.
func (d Deps) BeginPasskeyRegistration(w http.ResponseWriter, r *http.Request) {
	sess, ok := session(r)
	if !ok {
		server.WriteError(w, server.Errorf(server.CodeUnauthenticated, ""))
		return
	}
	p, err := d.passkeys()
	if err != nil {
		server.WriteError(w, err)
		return
	}
	if d.StepUps == nil || d.enrolments == nil {
		d.logf(r, "api: the passkey registration route is not wired")
		server.WriteError(w, server.Errorf(server.CodeInternal, "the passkey registration route is not wired"))
		return
	}
	var req stepUpRequest
	if err := server.DecodeBody(w, r, maxCBORBody, &req); err != nil {
		server.WriteError(w, err)
		return
	}
	if err := d.refuseAtPasskeyCap(r.Context(), d.Repo, sess.UserID); err != nil {
		server.WriteError(w, d.storeError(r, err))
		return
	}
	steppedUp, err := d.requireStepUp(r.Context(), sess, req.StepUp)
	if err != nil {
		server.WriteError(w, d.storeError(r, err))
		return
	}
	ceremonyID, options, err := p.BeginRegistration(r.Context(), sess.UserID)
	if err != nil {
		server.WriteError(w, d.storeError(r, err))
		return
	}
	d.enrolments.record(passkeyEnrolment(ceremonyID), sess.UserID, steppedUp, nil)
	d.write(w, r, http.StatusOK, []any{ceremonyID, string(options)})
}

// FinishPasskeyRegistration is POST /v1/auth/passkey/register/finish:
// [ceremony_id(bstr16), response(tstr), name(tstr)] → [cred_id(bstr), backup_codes([tstr])].
//
// `name` is checked first, so a refused name does not spend the ceremony. A ceremony this
// instance recorded as begun by another account is refused before it is taken, so the account
// that began it can still finish it; past a restart, when no record is left, the ceremony row's
// own user decides after the take. The credential is then written in ConfirmTOTP's one
// transaction under the user lock: the step-up re-check, the MaxPasskeys re-check (409), the
// insert, factor_owed cleared, backup codes minted only when none is unused.
func (d Deps) FinishPasskeyRegistration(w http.ResponseWriter, r *http.Request) {
	sess, ok := session(r)
	if !ok {
		server.WriteError(w, server.Errorf(server.CodeUnauthenticated, ""))
		return
	}
	p, err := d.passkeys()
	if err != nil {
		server.WriteError(w, err)
		return
	}
	if d.Config == nil || d.enrolments == nil {
		d.logf(r, "api: the passkey registration route is not wired")
		server.WriteError(w, server.Errorf(server.CodeInternal, "the passkey registration route is not wired"))
		return
	}
	var req struct {
		_          struct{} `cbor:",toarray"`
		CeremonyID id.ID
		Response   string
		Name       string
	}
	if err := server.DecodeBody(w, r, maxCBORBody, &req); err != nil {
		server.WriteError(w, err)
		return
	}
	if !validPasskeyName(req.Name) {
		server.WriteError(w, server.Errorf(server.CodeInvalidRequest,
			"name is 1-64 bytes of UTF-8 without control characters"))
		return
	}
	ctx := r.Context()
	key := passkeyEnrolment(req.CeremonyID)
	if owner, ok := d.enrolments.owner(key); ok && owner != sess.UserID {
		d.logf(r, "api: finish passkey registration", "err", "the ceremony was begun by another account")
		server.WriteError(w, server.Errorf(server.CodeInvalidRequest,
			"the registration response was not accepted"))
		return
	}
	steppedUp := d.enrolments.steppedUp(key, sess.UserID, nil)
	d.enrolments.drop(key)
	row, err := p.FinishRegistration(ctx, req.CeremonyID, []byte(req.Response), req.Name)
	if err == nil && row.UserID != sess.UserID {
		err = errors.New("api: the ceremony was begun by another account")
	}
	if err != nil {
		// One answer for an unknown ceremony, an expired one, a replayed one, another
		// account's, and a response the authenticator got wrong: none of them is something a
		// client can act on but "start the ceremony again".
		d.logf(r, "api: finish passkey registration", "err", err)
		server.WriteError(w, server.Errorf(server.CodeInvalidRequest,
			"the registration response was not accepted"))
		return
	}
	var codes []string
	if err := d.Repo.Tx(ctx, func(tx store.Repository) error {
		if err := tx.LockUserForDeviceRegistration(ctx, sess.UserID); err != nil {
			return err
		}
		if err := d.recheckStepUp(ctx, tx, sess.UserID, steppedUp); err != nil {
			return err
		}
		if err := d.refuseAtPasskeyCap(ctx, tx, sess.UserID); err != nil {
			return err
		}
		if err := tx.PutWebauthnCredential(ctx, row); err != nil {
			return err
		}
		codes, err = d.settleFirstFactor(ctx, tx, sess.UserID, d.Clock.Now().Unix())
		return err
	}); err != nil {
		server.WriteError(w, d.storeError(r, err))
		return
	}
	d.write(w, r, http.StatusOK, []any{row.CredID, stringsAny(codes)})
}

// MaxPasskeys is how many passkeys an account may hold for this rp_id: the bound the client's
// decode of GET /v1/auth/factors enforces.
const MaxPasskeys = 1024

// refuseAtPasskeyCap answers 409 E_INVALID_REQUEST when the account already holds MaxPasskeys
// passkeys for this rp_id.
func (d Deps) refuseAtPasskeyCap(ctx context.Context, repo store.Repository, userID id.ID) error {
	creds, err := repo.ListWebauthnCredentials(ctx, userID, d.rpID())
	if err != nil {
		return err
	}
	if len(creds) >= MaxPasskeys {
		return server.WithStatus(http.StatusConflict, server.Errorf(server.CodeInvalidRequest,
			"this account already holds %d passkeys", MaxPasskeys))
	}
	return nil
}

// validPasskeyName is register/finish's rule for `name`: 1-64 bytes of UTF-8, no control
// character.
func validPasskeyName(name string) bool {
	if len(name) == 0 || len(name) > 64 || !utf8.ValidString(name) {
		return false
	}
	for _, c := range name {
		if unicode.IsControl(c) {
			return false
		}
	}
	return true
}

// BeginPasskeyLogin is POST /v1/auth/passkey/login/begin:
// [] → [ceremony_id(bstr16), options(tstr)].
//
// It is unauthenticated and discoverable — no username is presented and none is
// revealed — so it meters itself on the `login` bucket by address, exactly as
// the password and second-factor ceremonies above do.
func (d Deps) BeginPasskeyLogin(w http.ResponseWriter, r *http.Request) {
	if d.Throttle == nil || d.Config == nil {
		d.logf(r, "api: the passkey login route is not wired")
		server.WriteError(w, server.Errorf(server.CodeInternal, "the passkey login route is not wired"))
		return
	}
	p, err := d.passkeys()
	if err != nil {
		server.WriteError(w, err)
		return
	}
	addrKey := server.RateKey(server.RealIP(r, d.Config.Server.TrustedProxyCIDRs))
	if ok, wait := d.Throttle.Allow(classLogin, addrKey); !ok {
		server.WriteError(w, server.RateLimitedAfter(wait))
		return
	}
	ceremonyID, options, err := p.BeginLogin(r.Context())
	if err != nil {
		server.WriteError(w, d.storeError(r, err))
		return
	}
	d.write(w, r, http.StatusOK, []any{ceremonyID, string(options)})
}

// FinishPasskeyLogin is POST /v1/auth/passkey/login/finish:
// [ceremony_id(bstr16), response(tstr)] → [assertion(tstr), needs(uint 0|1)].
//
// What the assertion is worth is decided by THIS assertion's UV flag, for the
// reason facts-auth.md §3.6 gives: a passkey is two factors only when user
// verification actually happened. With UV it is the whole login (needs 0).
// Without it — possible only where auth.webauthn.user_verification is not
// "required", because the library refuses a non-UV assertion outright there —
// it proved possession alone and owes a code (needs 1).
//
// A passkey this instance does not hold is 401 E_PASSKEY_UNKNOWN; every other
// failure is 401 E_UNAUTHENTICATED. Both charge the address. A step-up
// ceremony (kind 1 with a user) is refused like any other bad ceremony.
//
// The ceremony row is single-use in the store, so a replayed body finds no row
// and is refused before a signature is looked at.
func (d Deps) FinishPasskeyLogin(w http.ResponseWriter, r *http.Request) {
	if d.Throttle == nil || d.Assertions == nil || d.Config == nil {
		d.logf(r, "api: the passkey login route is not wired")
		server.WriteError(w, server.Errorf(server.CodeInternal, "the passkey login route is not wired"))
		return
	}
	p, err := d.passkeys()
	if err != nil {
		server.WriteError(w, err)
		return
	}
	var req ceremonyRequest
	if err := server.DecodeBody(w, r, maxCBORBody, &req); err != nil {
		server.WriteError(w, err)
		return
	}
	ctx := r.Context()
	ip := server.RealIP(r, d.Config.Server.TrustedProxyCIDRs)
	addrKey := server.RateKey(ip)
	if ok, wait := d.Throttle.Allow(classLogin, addrKey); !ok {
		server.WriteError(w, server.RateLimitedAfter(wait))
		return
	}
	// The address's FAILURE budget, peeked rather than spent, for the same
	// reason PasswordLogin peeks it: taking a token here would meter every
	// successful login on the failure bucket.
	if ok, wait := d.Throttle.Peek(classLoginFailed, addrKey); !ok {
		server.WriteError(w, server.RateLimitedAfter(wait))
		return
	}
	userID, uv, err := p.FinishLogin(ctx, req.CeremonyID, []byte(req.Response))
	if err != nil {
		d.logf(r, "api: finish passkey login", "err", err)
		d.recordAttempt(r, store.UserRow{}, false, addrKey, methodPasskey, false)
		d.Throttle.RecordFailure(passkeyFailureLedger, ip)
		code := server.CodeUnauthenticated
		if errors.Is(err, auth.ErrPasskeyUnknown) {
			code = server.CodePasskeyUnknown
		}
		server.WriteError(w, server.Errorf(code, ""))
		return
	}
	needs := needsNothing
	if !uv {
		needs = needsCode
	}
	// The account's failed-login ledger is cleared only by a complete login. A presence-only
	// assertion still owes a code, and verifySecondFactor clears the ledger once that is given;
	// clearing it here would hand someone holding a PIN-less key a fresh lockout curve per login.
	if needs != needsNothing {
		if locked := d.Throttle.LockedFor(userID); locked > 0 {
			// It would owe a code the lockout forbids guessing: no assertion is issued.
			server.WriteError(w, server.RateLimitedAfter(locked))
			return
		}
	}
	if needs == needsNothing {
		d.Throttle.Clear(userID)
		if cerr := d.Repo.ClearLoginFailures(ctx, userID); cerr != nil {
			d.logf(r, "api: ClearLoginFailures", "err", cerr)
		}
	}
	d.recordAttempt(r, store.UserRow{ID: userID}, true, addrKey, methodPasskey, true)
	d.write(w, r, http.StatusOK, []any{d.Assertions.Issue(userID, needs), uint64(needs)})
}

// OIDC (protocol/09-http-api.md § Auth ceremonies).
//
// These two routes are the only ones in this package a BROWSER drives rather
// than a CBOR client: they are top-level navigations, so they answer 302 and
// carry their state in a cookie. internal/auth owns discovery, PKCE, the
// id_token verification and the pending-login table; the pair below is the
// HTTP skin over it.

// oidcStateCookie is the cookie protocol/09 names. Deviation ID10 explains why
// the prefix is __Secure- and not __Host-; auth.StateCookie sets the flags.
const oidcStateCookie = "__Secure-dilla-oidc"

// oidcStateTTL is how long a started login may take to come back. It matches
// AssertionTTL for the same reason: a login leg nobody finished promptly is a
// login nobody is waiting on.
const oidcStateTTL = 5 * time.Minute

// methodOIDC is the login_attempts.method value of an OIDC assertion. It
// follows methodPasskey because this file is the only writer of that column; it
// is NOT the instance document's `auth_methods` numbering, where oidc is 3.
const methodOIDC uint8 = 4

// oidcFailureLedger is the account ledger a failed callback is charged to. The
// callback never names an account before the exchange succeeds, so there is no
// per-account lockout to earn; the entry exists only because RecordFailure is
// also what charges the ADDRESS. Nothing gates on this key.
var oidcFailureLedger = loginLedger(store.UserRow{}, false, "\x00oidc")

// oidc returns the login runner, or the refusal both routes answer when the
// instance has none. A nil OIDC is not a composition bug: auth.oidc.enabled
// defaults to false and most instances never configure an identity provider.
func (d Deps) oidc() (*auth.OIDC, error) {
	if d.OIDC == nil {
		return nil, notImplemented("oidc is not enabled on this instance")
	}
	return d.OIDC, nil
}

// oidcReturnURL is where a finished callback sends the browser. The assertion
// travels in the FRAGMENT, never the query: a fragment is never sent to a
// server, never reaches a Referer header and never lands in an access log, and
// this one-time token is the whole login.
func (d Deps) oidcReturnURL(assertion string) string {
	return (&url.URL{Scheme: "https", Host: d.Domain, Path: "/",
		Fragment: "assertion=" + assertion}).String()
}

// StartOIDC is GET /v1/auth/oidc/start: 302 to the identity provider, PKCE
// S256, state cookie.
//
// The state, the nonce and the verifier are minted here and the last two are
// kept server-side under the first, so the browser holds nothing but an opaque
// value it cannot use anywhere else. Discovery happens on this path — lazily,
// on first use — so an IdP that is down costs a refusal on this one route
// rather than a server that will not start.
func (d Deps) StartOIDC(w http.ResponseWriter, r *http.Request) {
	if d.Throttle == nil || d.Config == nil {
		d.logf(r, "api: the oidc start route is not wired")
		server.WriteError(w, server.Errorf(server.CodeInternal, "the oidc start route is not wired"))
		return
	}
	o, err := d.oidc()
	if err != nil {
		server.WriteError(w, err)
		return
	}
	addrKey := server.RateKey(server.RealIP(r, d.Config.Server.TrustedProxyCIDRs))
	if ok, wait := d.Throttle.Allow(classLogin, addrKey); !ok {
		server.WriteError(w, server.RateLimitedAfter(wait))
		return
	}
	state, nonce, verifier := auth.NewVerifierAndState()
	target, err := o.AuthURL(r.Context(), state, nonce, verifier)
	if err != nil {
		d.logf(r, "api: oidc authorization url", "err", err)
		server.WriteError(w, server.Errorf(server.CodeInternal, "oidc discovery failed"))
		return
	}
	o.Stash(state, auth.Pending{Nonce: nonce, Verifier: verifier}, oidcStateTTL)
	http.SetCookie(w, auth.StateCookie(oidcStateCookie, state, int(oidcStateTTL/time.Second)))
	http.Redirect(w, r, target, http.StatusFound)
}

// CallbackOIDC is GET /v1/auth/oidc/callback: 302 back to the client with a
// one-time assertion.
//
// The order is the security property. The cookie is cleared first, whatever
// happens next, so a failed callback leaves nothing a browser will replay. The
// state parameter is compared against the cookie in constant time, and the
// pending row is DELETED BEFORE the authorization code is spent, so a replayed
// callback finds no verifier and never reaches the token endpoint at all.
//
// The account is looked up by (issuer, subject), never by email: an address is
// something an identity provider lets a user change, and mapping on it would
// hand somebody else's account to whoever claimed a freed address.
func (d Deps) CallbackOIDC(w http.ResponseWriter, r *http.Request) {
	if d.Throttle == nil || d.Assertions == nil || d.Config == nil {
		d.logf(r, "api: the oidc callback route is not wired")
		server.WriteError(w, server.Errorf(server.CodeInternal, "the oidc callback route is not wired"))
		return
	}
	o, err := d.oidc()
	if err != nil {
		server.WriteError(w, err)
		return
	}
	ctx := r.Context()
	ip := server.RealIP(r, d.Config.Server.TrustedProxyCIDRs)
	addrKey := server.RateKey(ip)
	if ok, wait := d.Throttle.Allow(classLogin, addrKey); !ok {
		server.WriteError(w, server.RateLimitedAfter(wait))
		return
	}
	// Peeked rather than spent, for the same reason every other ceremony in
	// this file peeks it: taking a token here would meter every successful
	// login on the failure bucket.
	if ok, wait := d.Throttle.Peek(classLoginFailed, addrKey); !ok {
		server.WriteError(w, server.RateLimitedAfter(wait))
		return
	}
	http.SetCookie(w, auth.StateCookie(oidcStateCookie, "", -1))

	// One answer for a cancelled consent, a forged state, a replayed callback
	// and an id_token that did not verify: none of them is something a client
	// can act on but "start the login again".
	refuse := func(msg string, args ...any) {
		d.logf(r, msg, args...)
		d.recordAttempt(r, store.UserRow{}, false, addrKey, methodOIDC, false)
		d.Throttle.RecordFailure(oidcFailureLedger, ip)
		server.WriteError(w, server.Errorf(server.CodeUnauthenticated, ""))
	}
	q := r.URL.Query()
	if e := q.Get("error"); e != "" {
		refuse("api: oidc callback carried an error", "error", e)
		return
	}
	cookie, cerr := r.Cookie(oidcStateCookie)
	state := q.Get("state")
	if cerr != nil || state == "" ||
		subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(state)) != 1 {
		refuse("api: oidc callback state does not match the cookie")
		return
	}
	pend, ok := o.Spend(state)
	if !ok {
		refuse("api: oidc callback has no pending login")
		return
	}
	issuer, subject, email, err := o.Exchange(ctx, q.Get("code"), pend.Verifier, pend.Nonce)
	if err != nil {
		refuse("api: oidc exchange", "err", err)
		return
	}
	userID, err := d.Repo.GetOIDCIdentity(ctx, issuer, subject)
	switch {
	case errors.Is(err, store.ErrNotFound):
		// No account is mapped to this subject. Creating one here is not
		// something this route can do on its own: users.umk_pub, ssk_pub and
		// sig_umk_ssk are NOT NULL and are the account's own key material,
		// which only the client holds — a server that invented them would be
		// inventing the identity the whole protocol is keyed on. So the
		// refusal is E_FORBIDDEN either way, and auto_create changes only the
		// detail and what is logged: turning it on says the operator wants
		// these logins to become accounts, and the client-completed
		// registration leg that spends one does not exist yet. The key is
		// therefore reserved: deviation ID19 and ruling 40 record that, and
		// config.OIDC.AutoCreate says so where an operator reads it.
		if !d.Config.Auth.OIDC.AutoCreate {
			server.WriteError(w, server.Errorf(server.CodeForbidden,
				"this instance does not create accounts from the identity provider"))
			return
		}
		d.logf(r, "api: oidc login has no account and auto_create needs a client-completed registration",
			"issuer", issuer, "subject", subject, "email", email)
		server.WriteError(w, server.Errorf(server.CodeForbidden,
			"register first: an account carries key material only the client can generate"))
		return
	case err != nil:
		server.WriteError(w, d.storeError(r, err))
		return
	}
	d.Throttle.Clear(userID)
	if clearErr := d.Repo.ClearLoginFailures(ctx, userID); clearErr != nil {
		d.logf(r, "api: ClearLoginFailures", "err", clearErr)
	}
	d.recordAttempt(r, store.UserRow{ID: userID}, true, addrKey, methodOIDC, true)
	// An OIDC login is exactly as strong as the identity provider made it, and
	// dillad cannot tell from an id_token whether a second factor was involved.
	// So the assertion owes a factor whenever the account holds one, exactly as
	// after a password: a login path that walked past the account's own second
	// factor would be a downgrade of that factor, not a property of the provider.
	needs, terr := d.loginNeeds(ctx, userID)
	if terr != nil {
		server.WriteError(w, d.storeError(r, terr))
		return
	}
	http.Redirect(w, r, d.oidcReturnURL(d.Assertions.Issue(userID, needs)), http.StatusFound)
}

// Second factors (protocol/09 § Second factors): step-up, the factor list, removal and fresh
// backup codes.

// StepUpTTL is how long a step-up token lives.
const StepUpTTL = 300 * time.Second

// StepUps holds the short-lived step-up tokens POST /v1/auth/stepup and its passkey twin mint, and
// the four factor-changing routes spend. A token is bound to the session that minted it (by the
// session token's hash), is single use and lives StepUpTTL; like an assertion it lives only in
// memory, so a restart asks the person to confirm again.
type StepUps struct {
	clk clock.Clock
	ttl time.Duration

	mu    sync.Mutex
	items map[string]stepUp
}

type stepUp struct {
	userID  id.ID
	session []byte // the minting session's token hash
	expires time.Time
}

func NewStepUps(clk clock.Clock, ttl time.Duration) *StepUps {
	return &StepUps{clk: clk, ttl: ttl, items: map[string]stepUp{}}
}

// Issue mints a token for userID bound to the session whose token hashes to sessHash, and returns
// it with its expiry in unix seconds.
func (s *StepUps) Issue(userID id.ID, sessHash []byte) (string, int64) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		panic("api: crypto/rand failed: " + err.Error())
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	expires := s.clk.Now().Add(s.ttl)
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.clk.Now()
	for k, v := range s.items {
		if now.After(v.expires) {
			delete(s.items, k)
		}
	}
	s.items[token] = stepUp{userID: userID, session: append([]byte(nil), sessHash...), expires: expires}
	return token, expires.Unix()
}

// Spend consumes a token presented by the session whose token hashes to sessHash. The session is
// compared in constant time. A token presented by another session is refused and NOT consumed: it
// stays good for the session that minted it, which a holder of the other session cannot be.
func (s *StepUps) Spend(token string, sessHash []byte) (id.ID, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	item, ok := s.items[token]
	if !ok || subtle.ConstantTimeCompare(item.session, sessHash) != 1 {
		return id.ID{}, false
	}
	delete(s.items, token)
	if s.clk.Now().After(item.expires) {
		return id.ID{}, false
	}
	return item.userID, true
}

// Valid is Spend's lookup without the spend: whether the session whose token hashes to sessHash
// holds token, unexpired. It answers a route that must refuse a bad token before costly work;
// Spend, after that work, stays the decision, so of two requests that both found the token valid
// only one spends it.
func (s *StepUps) Valid(token string, sessHash []byte) (id.ID, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	item, ok := s.items[token]
	if !ok || subtle.ConstantTimeCompare(item.session, sessHash) != 1 || s.clk.Now().After(item.expires) {
		return id.ID{}, false
	}
	return item.userID, true
}

// enrolmentTTL is how long the record of an enrolment's begin is kept: a passkey ceremony's
// lifetime (auth's ceremonyTTL), and the same for a TOTP enrolment.
const enrolmentTTL = 5 * time.Minute

// enrolments remembers, per enrolment in flight, whether its begin carried a valid step-up, so the
// confirm or finish can re-check the step-up rule under the user lock. A TOTP enrolment is keyed by
// its user (a new enrol replaces it), a passkey registration by its ceremony id. Memory only: a
// missing record — after a restart, or past enrolmentTTL — counts as not stepped up.
type enrolments struct {
	clk clock.Clock
	ttl time.Duration

	mu    sync.Mutex
	items map[string]enrolment
}

type enrolment struct {
	userID    id.ID
	steppedUp bool
	secret    [32]byte // SHA-256 of the TOTP secret the enroll wrote; zero for a passkey
	expires   time.Time
}

// secretHash is what a TOTP enrolment record is bound to; nil (a passkey's) is the zero hash.
func secretHash(secret []byte) [32]byte {
	if secret == nil {
		return [32]byte{}
	}
	return sha256.Sum256(secret)
}

func newEnrolments(clk clock.Clock, ttl time.Duration) *enrolments {
	return &enrolments{clk: clk, ttl: ttl, items: map[string]enrolment{}}
}

func totpEnrolment(userID id.ID) string        { return "totp\x00" + userID.String() }
func passkeyEnrolment(ceremonyID id.ID) string { return "passkey\x00" + ceremonyID.String() }

func (e *enrolments) record(key string, userID id.ID, steppedUp bool, secret []byte) {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.clk.Now()
	for k, v := range e.items {
		if now.After(v.expires) {
			delete(e.items, k)
		}
	}
	e.items[key] = enrolment{userID: userID, steppedUp: steppedUp, secret: secretHash(secret), expires: now.Add(e.ttl)}
}

// steppedUp reports whether the enrolment under key was begun by userID with a valid step-up, for
// this secret (nil for a passkey), and has not expired.
func (e *enrolments) steppedUp(key string, userID id.ID, secret []byte) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	item, ok := e.items[key]
	return ok && item.userID == userID && item.steppedUp && item.secret == secretHash(secret) &&
		!e.clk.Now().After(item.expires)
}

// owner is the account a live enrolment record under key was begun by.
func (e *enrolments) owner(key string) (id.ID, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	item, ok := e.items[key]
	if !ok || e.clk.Now().After(item.expires) {
		return id.ID{}, false
	}
	return item.userID, true
}

func (e *enrolments) drop(key string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.items, key)
}

// stepUpRequest is the body of the two enrolment begins and of POST /v1/auth/recovery/codes:
// [stepup(tstr|null)].
type stepUpRequest struct {
	_      struct{} `cbor:",toarray"`
	StepUp *string
}

func stepUpRefusal() error {
	return server.Errorf(server.CodeStepUp, "a valid step-up token is required")
}

// spendStepUp spends token for sess, or answers 403 E_STEP_UP.
func (d Deps) spendStepUp(sess auth.Session, token *string) error {
	if token == nil {
		return stepUpRefusal()
	}
	if uid, ok := d.StepUps.Spend(*token, sess.TokenHash); !ok || uid != sess.UserID {
		return stepUpRefusal()
	}
	return nil
}

// requireStepUp is the step-up rule at an enrolment's begin: when the account holds a factor a
// valid token is spent (403 E_STEP_UP otherwise) and the begin is stepped up; when it holds none
// the token is ignored and left unspent.
func (d Deps) requireStepUp(ctx context.Context, sess auth.Session, token *string) (bool, error) {
	holds, err := holdsFactor(ctx, d.Repo, sess.UserID, d.rpID())
	if err != nil || !holds {
		return false, err
	}
	if err := d.spendStepUp(sess, token); err != nil {
		return false, err
	}
	return true, nil
}

// checkStepUp is requireStepUp without the spend: when the account holds a factor the token must
// be one Spend would accept (403 E_STEP_UP otherwise); when it holds none the token is ignored.
// A route that checks before costly work still spends with requireStepUp after it.
func (d Deps) checkStepUp(ctx context.Context, sess auth.Session, token *string) error {
	holds, err := holdsFactor(ctx, d.Repo, sess.UserID, d.rpID())
	if err != nil || !holds {
		return err
	}
	if token == nil {
		return stepUpRefusal()
	}
	if uid, ok := d.StepUps.Valid(*token, sess.TokenHash); !ok || uid != sess.UserID {
		return stepUpRefusal()
	}
	return nil
}

// recheckStepUp is the same rule inside the confirm or finish transaction, under the user lock: a
// factor that appeared since a begin that carried no step-up refuses the write with E_STEP_UP.
func (d Deps) recheckStepUp(ctx context.Context, tx store.Repository, userID id.ID, steppedUp bool) error {
	if steppedUp {
		return nil
	}
	holds, err := holdsFactor(ctx, tx, userID, d.rpID())
	if err != nil {
		return err
	}
	if holds {
		return stepUpRefusal()
	}
	return nil
}

// settleFirstFactor is the tail of every factor write, inside its transaction: the account no
// longer owes a factor, and when it holds no unused backup code it gets a fresh set, which is
// returned; otherwise nothing is minted and the printed codes keep working.
func (d Deps) settleFirstFactor(ctx context.Context, tx store.Repository, userID id.ID, now int64) ([]string, error) {
	if err := tx.SetFactorOwed(ctx, userID, false); err != nil {
		return nil, err
	}
	left, err := tx.CountRecoveryCodes(ctx, userID)
	if err != nil || left > 0 {
		return nil, err
	}
	codes, hashes := auth.GenerateRecoveryCodes(d.Config.Auth.TOTP.RecoveryCodes)
	if err := tx.PutRecoveryCodes(ctx, userID, hashes, now); err != nil {
		return nil, err
	}
	return codes, nil
}

// settleOwedHolder clears the owed mark of an account that holds a usable factor and reports
// whether it did. The check and the write are one transaction under the user lock, which the
// operator reset takes too before it deletes the factors and sets the mark: a reset is wholly
// before the check (no factor, nothing cleared) or waits for the commit (and then sets the mark
// again), so it never ends with an owed account cleared.
func (d Deps) settleOwedHolder(ctx context.Context, userID id.ID) (bool, error) {
	settled := false
	err := d.Repo.Tx(ctx, func(tx store.Repository) error {
		if err := tx.LockUserForDeviceRegistration(ctx, userID); err != nil {
			return err
		}
		u, err := tx.GetUser(ctx, userID)
		if err != nil || !u.FactorOwed {
			settled = err == nil
			return err
		}
		holds, err := holdsFactor(ctx, tx, userID, d.rpID())
		if err != nil || !holds {
			return err
		}
		if err := tx.SetFactorOwed(ctx, userID, false); err != nil {
			return err
		}
		settled = true
		return nil
	})
	return settled, err
}

// stringsAny is a []string as the CBOR array the wire carries ([] for none, never null).
func stringsAny(in []string) []any {
	out := make([]any, 0, len(in))
	for _, s := range in {
		out = append(out, s)
	}
	return out
}

// The step-up methods of POST /v1/auth/stepup.
const (
	stepUpPassword uint64 = 0
	stepUpTOTP     uint64 = 1
	stepUpBackup   uint64 = 2
)

// stepUpLedger is the failure ledger step-ups of userID are charged to. It is NOT the account's
// own (loginLedger's user id): a thief failing step-ups behind a stolen session must not lock the
// owner's password login.
//
// It is keyed by the account AND the client address (web3.6 review M2): the owner stepping up
// from elsewhere clears only the owner's own count, never a thief's.
func stepUpLedger(userID id.ID, addrKey string) id.ID {
	return loginLedger(store.UserRow{}, false, "\x00stepup"+userID.String()+"\x00"+addrKey)
}

// stepUpAccountLedger is the account-wide step-up ledger: every step-up guess at the account,
// from any address, is counted on it, so guesses spread over many addresses are capped per
// account. A success never clears it (it ages out over the observation window), so the owner's
// success cannot reset a thief's count either. The price: a thief can hold the owner's step-ups
// locked, for at most lockout_ceiling; the owner's login ledger is never touched.
func stepUpAccountLedger(userID id.ID) id.ID {
	return loginLedger(store.UserRow{}, false, "\x00stepup-acct"+userID.String())
}

// stepUpTry is one guess at a step-up secret in flight: counted on both step-up ledgers before
// the secret is checked (auth.Throttle.Begin), so concurrent guesses count against each other.
type stepUpTry struct {
	attempt *auth.Attempt
	address id.ID // the account+address ledger, the one a success clears
}

// stepUpGate is the standing refusal before a step-up secret is looked at: the address's failure
// budget, PEEKED as PasswordLogin peeks it, then the account-wide and the account+address step-up
// ledgers, checked and counted in one step.
func (d Deps) stepUpGate(r *http.Request, userID id.ID) (stepUpTry, error) {
	addrKey := server.RateKey(server.RealIP(r, d.Config.Server.TrustedProxyCIDRs))
	if ok, wait := d.Throttle.Peek(classLoginFailed, addrKey); !ok {
		return stepUpTry{}, server.RateLimitedAfter(wait)
	}
	address := stepUpLedger(userID, addrKey)
	attempt, locked := d.Throttle.Begin(stepUpAccountLedger(userID), address)
	if locked > 0 {
		return stepUpTry{}, server.RateLimitedAfter(locked)
	}
	return stepUpTry{attempt: attempt, address: address}, nil
}

// fail keeps the guess counted and charges the address; it reports the lockout the guess earned.
func (s stepUpTry) fail(d Deps, r *http.Request) time.Duration {
	return s.attempt.Fail(server.RealIP(r, d.Config.Server.TrustedProxyCIDRs))
}

// pass refunds the guess and clears the account+address ledger; the account-wide one ages out.
func (s stepUpTry) pass() { s.attempt.Pass(s.address) }

// cancel refunds a guess that was never decided (a store failure).
func (s stepUpTry) cancel() { s.attempt.Pass() }

// stepUpFailure charges a failed step-up and answers 403 E_STEP_UP, or 429 E_RATE_LIMITED once a
// step-up ledger is past its free attempts.
func (d Deps) stepUpFailure(r *http.Request, try stepUpTry) error {
	if locked := try.fail(d, r); locked > 0 {
		return server.RateLimitedAfter(locked)
	}
	return stepUpRefusal()
}

// StepUp is POST /v1/auth/stepup: [method(uint), secret(tstr)] → 201 [stepup(tstr), expires(uint)].
// Method 0 is the password, 1 a TOTP code (its counter consumed, as at /totp/verify), 2 a backup
// code (spent). A wrong secret is 403 E_STEP_UP; past the step-up ledger's free attempts every
// attempt, right or wrong, is 429 until the lockout runs out.
func (d Deps) StepUp(w http.ResponseWriter, r *http.Request) {
	sess, ok := session(r)
	if !ok {
		server.WriteError(w, server.Errorf(server.CodeUnauthenticated, ""))
		return
	}
	if d.Config == nil || d.Throttle == nil || d.Hasher == nil || d.StepUps == nil {
		d.logf(r, "api: the step-up route is not wired")
		server.WriteError(w, server.Errorf(server.CodeInternal, "the step-up route is not wired"))
		return
	}
	var req struct {
		_      struct{} `cbor:",toarray"`
		Method uint64
		Secret string
	}
	if err := server.DecodeBody(w, r, maxCBORBody, &req); err != nil {
		server.WriteError(w, err)
		return
	}
	if req.Method > stepUpBackup {
		server.WriteError(w, server.Errorf(server.CodeInvalidRequest, "method is 0, 1 or 2"))
		return
	}
	try, err := d.stepUpGate(r, sess.UserID)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	ctx := r.Context()
	good, err := d.checkStepUpSecret(ctx, sess.UserID, req.Method, req.Secret)
	if err != nil {
		try.cancel()
		server.WriteError(w, d.storeError(r, err))
		return
	}
	if !good {
		server.WriteError(w, d.stepUpFailure(r, try))
		return
	}
	try.pass()
	token, expires := d.StepUps.Issue(sess.UserID, sess.TokenHash)
	d.write(w, r, http.StatusCreated, []any{token, uint64(expires)}) //nolint:gosec // G115: a unix time after 1970
}

// checkStepUpSecret reports whether secret proves the account by method. A store failure is an
// error; a wrong, absent or already-spent secret is false.
func (d Deps) checkStepUpSecret(ctx context.Context, userID id.ID, method uint64, secret string) (bool, error) {
	miss := func(err error) (bool, error) {
		if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrConflict) {
			return false, nil
		}
		return false, err
	}
	switch method {
	case stepUpPassword:
		phc, err := d.Repo.GetPasswordCredential(ctx, userID)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return false, err
		}
		// An account without a password still costs a (dummy) hash, as at login.
		ok, _, verr := d.Hasher.Verify(ctx, secret, phc)
		if verr != nil {
			return false, verr
		}
		return ok && phc != "", nil
	case stepUpTOTP:
		row, err := d.Repo.GetTOTP(ctx, userID)
		if err != nil {
			return miss(err)
		}
		if row.ConfirmedAt == nil {
			return false, nil
		}
		counter, ok := auth.ValidateTOTP(secret, string(row.Secret), rowParams(row, d.totpParams()), d.Clock.Now(), row.LastCounter)
		if !ok {
			return false, nil
		}
		if err := d.Repo.ConsumeTOTPCounter(ctx, userID, counter); err != nil {
			return miss(err)
		}
		return true, nil
	default:
		if err := d.Repo.ConsumeRecoveryCode(ctx, userID, auth.RecoveryCodeHash(secret), d.Clock.Now().Unix()); err != nil {
			return miss(err)
		}
		return true, nil
	}
}

// BeginPasskeyStepUp is POST /v1/auth/stepup/passkey/begin: [] → [ceremony_id, options], over this
// account's credentials only, user verification required. An account without a passkey is 400.
func (d Deps) BeginPasskeyStepUp(w http.ResponseWriter, r *http.Request) {
	sess, ok := session(r)
	if !ok {
		server.WriteError(w, server.Errorf(server.CodeUnauthenticated, ""))
		return
	}
	p, err := d.passkeys()
	if err != nil {
		server.WriteError(w, err)
		return
	}
	var req struct {
		_ struct{} `cbor:",toarray"`
	}
	if err := server.DecodeBody(w, r, maxCBORBody, &req); err != nil {
		server.WriteError(w, err)
		return
	}
	ceremonyID, options, err := p.BeginStepUp(r.Context(), sess.UserID)
	if errors.Is(err, auth.ErrNoPasskey) {
		server.WriteError(w, server.Errorf(server.CodeInvalidRequest, "this account holds no passkey"))
		return
	}
	if err != nil {
		server.WriteError(w, d.storeError(r, err))
		return
	}
	d.write(w, r, http.StatusOK, []any{ceremonyID, string(options)})
}

// FinishPasskeyStepUp is POST /v1/auth/stepup/passkey/finish: [ceremony_id, response] → 201
// [stepup, expires]. A ceremony that is not this account's step-up (a discoverable login's, or
// another account's) and an assertion that does not verify are 403 E_STEP_UP, charged to the
// step-up ledger like a wrong secret.
func (d Deps) FinishPasskeyStepUp(w http.ResponseWriter, r *http.Request) {
	sess, ok := session(r)
	if !ok {
		server.WriteError(w, server.Errorf(server.CodeUnauthenticated, ""))
		return
	}
	p, err := d.passkeys()
	if err != nil {
		server.WriteError(w, err)
		return
	}
	if d.Config == nil || d.Throttle == nil || d.StepUps == nil {
		d.logf(r, "api: the passkey step-up route is not wired")
		server.WriteError(w, server.Errorf(server.CodeInternal, "the passkey step-up route is not wired"))
		return
	}
	var req ceremonyRequest
	if err := server.DecodeBody(w, r, maxCBORBody, &req); err != nil {
		server.WriteError(w, err)
		return
	}
	try, err := d.stepUpGate(r, sess.UserID)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	if err := p.FinishStepUp(r.Context(), req.CeremonyID, []byte(req.Response), sess.UserID); err != nil {
		d.logf(r, "api: finish passkey step-up", "err", err)
		server.WriteError(w, d.stepUpFailure(r, try))
		return
	}
	try.pass()
	token, expires := d.StepUps.Issue(sess.UserID, sess.TokenHash)
	d.write(w, r, http.StatusCreated, []any{token, uint64(expires)}) //nolint:gosec // G115: a unix time after 1970
}

// Factors is GET /v1/auth/factors → [owed(uint), totp(uint|null), passkeys([[cred_id(bstr),
// name(tstr), created(uint), last_used(uint|null)]]), backup_codes_left(uint), password(uint),
// password_changed(uint|null)]. password_changed (Task 11) is when the password was last changed,
// null when unknown or there is none; an older client reads the first five elements.
// totp is the confirmed_at of a CONFIRMED secret, else null; passkeys are this rp_id's, by created.
//
// An account marked as owing a factor that holds a usable one is settled here (web3.6 review I2):
// owed means no usable factor is held, and the mark can outlive that (an rp_id changed and put
// back, before the restart whose reconcile clears it), leaving a forced set-up that cannot finish.
func (d Deps) Factors(w http.ResponseWriter, r *http.Request) {
	sess, ok := session(r)
	if !ok {
		server.WriteError(w, server.Errorf(server.CodeUnauthenticated, ""))
		return
	}
	ctx := r.Context()
	u, err := d.Repo.GetUser(ctx, sess.UserID)
	if err != nil {
		server.WriteError(w, d.storeError(r, err))
		return
	}
	if u.FactorOwed {
		settled, err := d.settleOwedHolder(ctx, sess.UserID)
		if err != nil {
			server.WriteError(w, d.storeError(r, err))
			return
		}
		u.FactorOwed = !settled
	}
	var totp any
	switch row, err := d.Repo.GetTOTP(ctx, sess.UserID); {
	case err == nil && row.ConfirmedAt != nil:
		totp = uint64(*row.ConfirmedAt) //nolint:gosec // G115: a unix time after 1970
	case err != nil && !errors.Is(err, store.ErrNotFound):
		server.WriteError(w, d.storeError(r, err))
		return
	}
	creds, err := d.Repo.ListWebauthnCredentials(ctx, sess.UserID, d.rpID())
	if err != nil {
		server.WriteError(w, d.storeError(r, err))
		return
	}
	passkeys := make([]any, 0, len(creds))
	for _, c := range creds {
		var lastUsed any
		if c.LastUsed != nil {
			lastUsed = uint64(*c.LastUsed) //nolint:gosec // G115: a unix time after 1970
		}
		passkeys = append(passkeys, []any{c.CredID, c.Name, uint64(c.Created), lastUsed}) //nolint:gosec // G115: a unix time after 1970
	}
	left, err := d.Repo.CountRecoveryCodes(ctx, sess.UserID)
	if err != nil {
		server.WriteError(w, d.storeError(r, err))
		return
	}
	password := uint64(0)
	var changed any
	switch _, err := d.Repo.GetPasswordCredential(ctx, sess.UserID); {
	case err == nil:
		password = 1
		at, err := d.Repo.GetPasswordChangedAt(ctx, sess.UserID)
		if err != nil {
			server.WriteError(w, d.storeError(r, err))
			return
		}
		if at != nil {
			changed = uint64(*at) //nolint:gosec // G115: a unix time after 1970
		}
	case !errors.Is(err, store.ErrNotFound):
		server.WriteError(w, d.storeError(r, err))
		return
	}
	owed := uint64(0)
	if u.FactorOwed {
		owed = 1
	}
	d.write(w, r, http.StatusOK, []any{owed, totp, passkeys, uint64(left), password, changed}) //nolint:gosec // G115: a count
}

// The kinds of POST /v1/auth/factors/remove.
const (
	factorTOTP    uint64 = 0
	factorPasskey uint64 = 1
)

// RemoveFactor is POST /v1/auth/factors/remove: [stepup(tstr), kind(uint), cred_id(bstr|null)] →
// 204. The token is spent first (403 E_STEP_UP). Then, in one transaction under the user lock:
// the delete (a passkey the caller does not hold, or no TOTP, is 404 E_NOT_FOUND), the recount,
// and on zero factors a rollback with 409 E_INVALID_REQUEST. Two devices removing the last two
// factors at once serialize on the lock, and the second finds nothing left to stand on. Backup
// codes are not a factor and stay.
func (d Deps) RemoveFactor(w http.ResponseWriter, r *http.Request) {
	sess, ok := session(r)
	if !ok {
		server.WriteError(w, server.Errorf(server.CodeUnauthenticated, ""))
		return
	}
	if d.StepUps == nil {
		d.logf(r, "api: the factor removal route is not wired")
		server.WriteError(w, server.Errorf(server.CodeInternal, "the factor removal route is not wired"))
		return
	}
	var req struct {
		_      struct{} `cbor:",toarray"`
		StepUp string
		Kind   uint64
		CredID []byte
	}
	if err := server.DecodeBody(w, r, maxCBORBody, &req); err != nil {
		server.WriteError(w, err)
		return
	}
	switch {
	case req.Kind == factorPasskey && len(req.CredID) == 0:
		server.WriteError(w, server.Errorf(server.CodeInvalidRequest, "a passkey removal names its cred_id"))
		return
	case req.Kind > factorPasskey:
		server.WriteError(w, server.Errorf(server.CodeInvalidRequest, "kind is 0 (totp) or 1 (passkey)"))
		return
	}
	if err := d.spendStepUp(sess, &req.StepUp); err != nil {
		server.WriteError(w, err)
		return
	}
	ctx := r.Context()
	if err := d.Repo.Tx(ctx, func(tx store.Repository) error {
		if err := tx.LockUserForDeviceRegistration(ctx, sess.UserID); err != nil {
			return err
		}
		if req.Kind == factorTOTP {
			if _, err := tx.GetTOTP(ctx, sess.UserID); err != nil {
				return err
			}
			if err := tx.DeleteTOTP(ctx, sess.UserID); err != nil {
				return err
			}
		} else {
			n, err := tx.DeleteWebauthnCredential(ctx, sess.UserID, req.CredID)
			if err != nil {
				return err
			}
			if n == 0 {
				return server.Errorf(server.CodeNotFound, "no such passkey")
			}
		}
		holds, err := holdsFactor(ctx, tx, sess.UserID, d.rpID())
		if err != nil {
			return err
		}
		if !holds {
			return server.WithStatus(http.StatusConflict,
				server.Errorf(server.CodeInvalidRequest, "this is the account's last second factor"))
		}
		return nil
	}); err != nil {
		server.WriteError(w, d.storeError(r, err))
		return
	}
	d.noContent(w, r)
}

// RecoveryCodes is POST /v1/auth/recovery/codes: [stepup(tstr)] → [backup_codes([tstr])]. The
// token is spent first (403 E_STEP_UP); the fresh set replaces every earlier code, under the user
// lock.
func (d Deps) RecoveryCodes(w http.ResponseWriter, r *http.Request) {
	sess, ok := session(r)
	if !ok {
		server.WriteError(w, server.Errorf(server.CodeUnauthenticated, ""))
		return
	}
	if d.Config == nil || d.StepUps == nil {
		d.logf(r, "api: the backup codes route is not wired")
		server.WriteError(w, server.Errorf(server.CodeInternal, "the backup codes route is not wired"))
		return
	}
	var req stepUpRequest
	if err := server.DecodeBody(w, r, maxCBORBody, &req); err != nil {
		server.WriteError(w, err)
		return
	}
	if err := d.spendStepUp(sess, req.StepUp); err != nil {
		server.WriteError(w, err)
		return
	}
	ctx := r.Context()
	codes, hashes := auth.GenerateRecoveryCodes(d.Config.Auth.TOTP.RecoveryCodes)
	if err := d.Repo.Tx(ctx, func(tx store.Repository) error {
		if err := tx.LockUserForDeviceRegistration(ctx, sess.UserID); err != nil {
			return err
		}
		return tx.PutRecoveryCodes(ctx, sess.UserID, hashes, d.Clock.Now().Unix())
	}); err != nil {
		server.WriteError(w, d.storeError(r, err))
		return
	}
	d.write(w, r, http.StatusOK, []any{stringsAny(codes)})
}
