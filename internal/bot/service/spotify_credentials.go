package service

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/jonasthim/dilla/internal/bot/client"
	"github.com/jonasthim/dilla/internal/bot/native"
	"github.com/jonasthim/dilla/internal/bot/spotifyauth"
	"github.com/jonasthim/dilla/internal/cborx"
	"github.com/jonasthim/dilla/internal/id"
)

const (
	spotifyCredentialArea     = uint64(8)
	spotifyCredentialVersion  = uint64(2)
	maxSpotifyCredentialState = 20 << 10
)

var (
	ErrSpotifyCredentials       = errors.New("spotify credentials unavailable")
	ErrSpotifyNotLinked         = fmt.Errorf("%w: no linked account", ErrSpotifyCredentials)
	ErrSpotifyState             = errors.New("invalid spotify credential state")
	ErrSpotifyCredentialBinding = errors.New("spotify credential installation binding mismatch")
	ErrSpotifyConflict          = errors.New("spotify credential state changed")
	ErrSpotifyNoPending         = errors.New("no pending spotify authorization")
	ErrSpotifyRelinkRequired    = errors.New("spotify authorization was consumed; relink required")
)

type spotifyCredentialRuntime interface {
	ClaimSpotifyCode(context.Context, id.ID) (client.SpotifyCodeClaim, error)
}

type spotifyCredentialNative interface {
	Binding() native.Binding
	StateGet(context.Context, uint64, []byte) (native.StateValue, error)
	StateTransaction(context.Context, id.ID, []native.StateChange) ([]uint64, error)
}

type spotifyTokenExchanger interface {
	Exchange(context.Context, string, string) (spotifyauth.Token, error)
	Refresh(context.Context, string) (spotifyauth.Token, error)
}

type spotifyAccountProfiler interface {
	AccountID(context.Context, string) (string, error)
}
type spotifyHTTPProfiler struct{}

func (spotifyHTTPProfiler) AccountID(ctx context.Context, access string) (string, error) {
	return spotifyauth.Profile(ctx, (*http.Client)(nil), access)
}

type spotifyHTTPExchanger struct{ clientID, redirect string }

func (e spotifyHTTPExchanger) Exchange(ctx context.Context, code, verifier string) (spotifyauth.Token, error) {
	return spotifyauth.Exchange(ctx, nil, e.clientID, e.redirect, code, verifier)
}
func (e spotifyHTTPExchanger) Refresh(ctx context.Context, refresh string) (spotifyauth.Token, error) {
	return spotifyauth.Refresh(ctx, nil, e.clientID, refresh)
}

// SpotifyCredentials owns one encrypted, installation-bound Spotify token.
type SpotifyCredentials struct {
	runtime   spotifyCredentialRuntime
	native    spotifyCredentialNative
	exchanger spotifyTokenExchanger
	clock     func() time.Time
	binding   native.Binding
	profile   spotifyAccountProfiler
	guard     *SpotifyAccountGuard
}

func NewSpotifyCredentials(runtime spotifyCredentialRuntime, n spotifyCredentialNative, clientID, redirectURL string, guard *SpotifyAccountGuard) (*SpotifyCredentials, error) {
	b := native.Binding{}
	if n != nil {
		b = n.Binding()
	}
	if runtime == nil || n == nil || guard == nil || b.Instance.IsZero() || b.Community.IsZero() || b.Installation.IsZero() || strings.TrimSpace(clientID) == "" || strings.ContainsAny(clientID, "\r\n\x00") || !strings.HasPrefix(redirectURL, "https://") {
		return nil, ErrSpotifyCredentials
	}
	s := newSpotifyCredentials(runtime, n, spotifyHTTPExchanger{clientID: clientID, redirect: redirectURL})
	s.profile, s.guard = spotifyHTTPProfiler{}, guard
	return s, nil
}

// NewSpotifyCredentialStatusReader binds the read-only credential inspector to
// the actual installation without requiring OAuth configuration.
func NewSpotifyCredentialStatusReader(runtime spotifyCredentialRuntime, n spotifyCredentialNative, guard *SpotifyAccountGuard) (*SpotifyCredentials, error) {
	if runtime == nil || n == nil || guard == nil {
		return nil, ErrSpotifyCredentials
	}
	binding := n.Binding()
	if binding.Instance.IsZero() || binding.Community.IsZero() || binding.Installation.IsZero() {
		return nil, ErrSpotifyCredentials
	}
	return &SpotifyCredentials{runtime: runtime, native: n, binding: binding, guard: guard}, nil
}

func newSpotifyCredentials(runtime spotifyCredentialRuntime, n spotifyCredentialNative, x spotifyTokenExchanger) *SpotifyCredentials {
	return &SpotifyCredentials{runtime: runtime, native: n, exchanger: x, profile: spotifyHTTPProfiler{}, clock: time.Now, binding: n.Binding()}
}

// Link claims the brokered PKCE receipt and saves the exchanged token with revision CAS.
// A previously persisted attempt is returned without repeating its one-use exchange.
func (s *SpotifyCredentials) Link(ctx context.Context) error {
	if s == nil || ctx == nil || s.runtime == nil || s.native == nil || s.exchanger == nil {
		return ErrSpotifyCredentials
	}
	unlock, err := s.guard.lockInstallation(ctx, s.binding.Installation.String())
	if err != nil {
		return ErrSpotifyCredentials
	}
	defer unlock()
	state, revision, err := s.read(ctx)
	if err != nil {
		return err
	}
	if state != nil && state.legacy {
		state, revision, err = s.migrateLegacy(ctx, state, revision)
		if err != nil {
			return ErrSpotifyCredentials
		}
	}
	claim, err := s.runtime.ClaimSpotifyCode(ctx, s.binding.Installation)
	if errors.Is(err, client.ErrSpotifyClaimNotFound) {
		if state != nil {
			return s.validateAccount(state)
		}
		return ErrSpotifyNoPending
	}
	if err != nil {
		return ErrSpotifyCredentials
	}
	if claim.Attempt.IsZero() || claim.Code == "" || claim.Verifier == "" {
		return ErrSpotifyCredentials
	}
	if state != nil && state.attempt == claim.Attempt {
		return s.validateAccount(state)
	}
	tok, err := s.exchanger.Exchange(ctx, claim.Code, claim.Verifier)
	if err != nil {
		return ErrSpotifyCredentials
	}
	if s.profile == nil || s.guard == nil {
		return ErrSpotifyCredentials
	}
	accountID, err := s.profile.AccountID(ctx, tok.AccessToken)
	if err != nil || !validSpotifyAccountID(accountID) {
		return ErrSpotifyCredentials
	}
	if err = s.guard.Bind(accountID); err != nil {
		return err
	}
	newState, err := s.fromToken(claim.Attempt, tok, accountID)
	if err != nil {
		return err
	}
	if err = s.save(ctx, newState, revision); err != nil {
		return ErrSpotifyRelinkRequired
	}
	return nil
}

// Load returns a valid access token, refreshing and safely persisting rotation when expired.
func (s *SpotifyCredentials) Load(ctx context.Context) (spotifyauth.Token, error) {
	if s == nil || ctx == nil || s.native == nil || s.exchanger == nil {
		return spotifyauth.Token{}, ErrSpotifyCredentials
	}
	unlock, err := s.guard.lockInstallation(ctx, s.binding.Installation.String())
	if err != nil {
		return spotifyauth.Token{}, ErrSpotifyCredentials
	}
	defer unlock()
	state, revision, err := s.read(ctx)
	if err != nil {
		return spotifyauth.Token{}, err
	}
	if state == nil {
		return spotifyauth.Token{}, ErrSpotifyNotLinked
	}
	if state.legacy {
		state, revision, err = s.migrateLegacy(ctx, state, revision)
		if err != nil {
			return spotifyauth.Token{}, ErrSpotifyCredentials
		}
	}
	if err := s.validateAccount(state); err != nil {
		return spotifyauth.Token{}, err
	}
	if time.Unix(state.expires, 0).After(s.clock().Add(30 * time.Second)) {
		return spotifyauth.Token{AccessToken: state.access, RefreshToken: state.refresh, TokenType: "Bearer", ExpiresIn: int64(time.Unix(state.expires, 0).Sub(s.clock()).Seconds())}, nil
	}
	tok, err := s.exchanger.Refresh(ctx, state.refresh)
	if err != nil {
		return spotifyauth.Token{}, ErrSpotifyCredentials
	}
	updated, err := s.fromToken(state.attempt, tok, state.accountID)
	if err != nil {
		return spotifyauth.Token{}, err
	}
	if err = s.save(ctx, updated, revision); err != nil {
		latest, _, readErr := s.read(ctx)
		if readErr == nil && latest != nil && latest.attempt == state.attempt && latest.refresh == updated.refresh && latest.access == updated.access && latest.accountID == updated.accountID && latest.expires == updated.expires {
			return tok, nil
		}
		return spotifyauth.Token{}, err
	}
	return tok, nil
}

// InspectStatus checks only the already-stored encrypted token and the local
// installation binding. It never claims OAuth state, migrates, refreshes or writes.
func (s *SpotifyCredentials) InspectStatus(ctx context.Context) (ProviderAccount, ProviderReason) {
	if s == nil || ctx == nil || s.native == nil || s.guard == nil || s.binding.Installation.IsZero() {
		return ProviderAccountUnknown, ProviderReasonUnavailable
	}
	state, _, err := s.read(ctx)
	if err != nil {
		if errors.Is(err, ErrSpotifyCredentialBinding) {
			return ProviderAccountNeedsAttention, ProviderReasonBindingMismatch
		}
		if errors.Is(err, ErrSpotifyState) {
			return ProviderAccountNeedsAttention, ProviderReasonInvalidCredentials
		}
		return ProviderAccountUnknown, ProviderReasonUnavailable
	}
	if state == nil {
		return ProviderAccountNotLinked, ProviderReasonMissingCredentials
	}
	accountID, err := s.guard.read()
	if errors.Is(err, os.ErrNotExist) {
		return ProviderAccountNeedsAttention, ProviderReasonAccountMismatch
	}
	if err != nil {
		return ProviderAccountUnknown, ProviderReasonUnavailable
	}
	if accountID != state.accountID {
		return ProviderAccountNeedsAttention, ProviderReasonAccountMismatch
	}
	return ProviderAccountLinked, ProviderReasonNone
}

type spotifyCredentialState struct {
	attempt         id.ID
	access, refresh string
	accountID       string
	expires         int64
	legacy          bool
}

func (s *SpotifyCredentials) read(ctx context.Context) (*spotifyCredentialState, uint64, error) {
	v, err := s.native.StateGet(ctx, spotifyCredentialArea, s.key())
	if err != nil {
		return nil, 0, &spotifyCredentialLoadError{cause: err}
	}
	if v.Tombstone {
		return nil, v.Revision, ErrSpotifyState
	}
	if v.Revision == 0 {
		if len(v.Value) != 0 {
			return nil, 0, ErrSpotifyState
		}
		return nil, 0, nil
	}
	if len(v.Value) == 0 || len(v.Value) > maxSpotifyCredentialState {
		return nil, v.Revision, ErrSpotifyState
	}
	var a []any
	if cborx.ExpectMajor(v.Value, cborx.MajorArray) != nil || cborx.Unmarshal(v.Value, &a) != nil || len(a) == 0 {
		return nil, v.Revision, ErrSpotifyState
	}
	version, ok := a[0].(uint64)
	if !ok || (version != 1 && version != spotifyCredentialVersion) || (version == 1 && len(a) != 8) || (version == spotifyCredentialVersion && len(a) != 9) {
		return nil, v.Revision, ErrSpotifyState
	}
	instance, iok := spotifyStateID(a[1])
	community, cok := spotifyStateID(a[2])
	installation, ok := spotifyStateID(a[3])
	if !iok || !cok || !ok {
		return nil, v.Revision, ErrSpotifyState
	}
	if instance != s.binding.Instance || community != s.binding.Community || installation != s.binding.Installation {
		return nil, v.Revision, errors.Join(ErrSpotifyState, ErrSpotifyCredentialBinding)
	}
	attempt, ok := spotifyStateID(a[4])
	if !ok || attempt.IsZero() {
		return nil, v.Revision, ErrSpotifyState
	}
	access, aok := a[5].(string)
	refresh, rok := a[6].(string)
	var accountID string
	var expiryValue any
	legacy := version == 1
	if legacy {
		expiryValue = a[7]
	} else {
		accountID, _ = a[7].(string)
		expiryValue = a[8]
	}
	expiry, eok := expiryValue.(uint64)
	if !aok || !rok || (!legacy && !validSpotifyAccountID(accountID)) || !eok || !validSpotifySecret(access) || !validSpotifySecret(refresh) || expiry == 0 || expiry > math.MaxInt64 {
		return nil, v.Revision, ErrSpotifyState
	}
	return &spotifyCredentialState{attempt: attempt, access: access, refresh: refresh, accountID: accountID, expires: int64(expiry), legacy: legacy}, v.Revision, nil
}

func spotifyStateID(v any) (id.ID, bool) {
	b, ok := v.([]byte)
	if !ok || len(b) != len(id.ID{}) {
		return id.ID{}, false
	}
	var out id.ID
	copy(out[:], b)
	return out, true
}

func (s *SpotifyCredentials) fromToken(attempt id.ID, t spotifyauth.Token, accountID string) (*spotifyCredentialState, error) {
	now := s.clock()
	if attempt.IsZero() || !validSpotifyAccountID(accountID) || !validSpotifySecret(t.AccessToken) || !validSpotifySecret(t.RefreshToken) || t.ExpiresIn <= 0 || t.ExpiresIn > 365*24*60*60 || now.Unix() > math.MaxInt64-t.ExpiresIn {
		return nil, ErrSpotifyCredentials
	}
	return &spotifyCredentialState{attempt: attempt, access: t.AccessToken, refresh: t.RefreshToken, accountID: accountID, expires: now.Unix() + t.ExpiresIn}, nil
}

func (s *SpotifyCredentials) save(ctx context.Context, st *spotifyCredentialState, revision uint64) error {
	if st == nil || st.expires <= 0 {
		return ErrSpotifyState
	}
	body, err := cborx.Marshal([]any{spotifyCredentialVersion, s.binding.Instance, s.binding.Community, s.binding.Installation, st.attempt, st.access, st.refresh, st.accountID, uint64(st.expires)})
	if err != nil || len(body) > maxSpotifyCredentialState {
		return ErrSpotifyState
	}
	// Native transaction receipts outlive this call. A fresh operation ID keeps
	// a later stale write with identical bytes from replaying an earlier receipt.
	tx := id.New()
	_, err = s.native.StateTransaction(ctx, tx, []native.StateChange{{Area: spotifyCredentialArea, Key: s.key(), ExpectedRevision: revision, Value: body}})
	if err != nil {
		return ErrSpotifyConflict
	}
	return nil
}

func (s *SpotifyCredentials) migrateLegacy(ctx context.Context, legacy *spotifyCredentialState, revision uint64) (*spotifyCredentialState, uint64, error) {
	if legacy == nil || !legacy.legacy || s.profile == nil || s.guard == nil || s.exchanger == nil {
		return nil, revision, ErrSpotifyCredentials
	}
	tok := spotifyauth.Token{AccessToken: legacy.access, RefreshToken: legacy.refresh, ExpiresIn: legacy.expires - s.clock().Unix()}
	if tok.ExpiresIn <= 30 {
		var err error
		tok, err = s.exchanger.Refresh(ctx, legacy.refresh)
		if err != nil {
			return nil, revision, ErrSpotifyCredentials
		}
	}
	accountID, err := s.profile.AccountID(ctx, tok.AccessToken)
	if err != nil || !validSpotifyAccountID(accountID) {
		return nil, revision, ErrSpotifyCredentials
	}
	if err = s.guard.Bind(accountID); err != nil {
		return nil, revision, ErrSpotifyCredentials
	}
	updated, err := s.fromToken(legacy.attempt, tok, accountID)
	if err != nil {
		return nil, revision, ErrSpotifyCredentials
	}
	if err = s.save(ctx, updated, revision); err != nil {
		return nil, revision, ErrSpotifyCredentials
	}
	updated.legacy = false
	return updated, revision + 1, nil
}

// ValidateAccount verifies persisted installation state against the global account binding.
func (s *SpotifyCredentials) ValidateAccount(ctx context.Context) error {
	if s == nil || ctx == nil {
		return ErrSpotifyCredentials
	}
	unlock, err := s.guard.lockInstallation(ctx, s.binding.Installation.String())
	if err != nil {
		return ErrSpotifyCredentials
	}
	defer unlock()
	state, revision, err := s.read(ctx)
	if err != nil {
		return err
	}
	if state == nil {
		return nil
	}
	if state.legacy {
		state, _, err = s.migrateLegacy(ctx, state, revision)
		if err != nil {
			return ErrSpotifyCredentials
		}
	}
	return s.validateAccount(state)
}

func (s *SpotifyCredentials) validateAccount(state *spotifyCredentialState) error {
	if s.guard == nil || state == nil {
		return ErrSpotifyCredentials
	}
	return s.guard.Bind(state.accountID)
}

func (s *SpotifyCredentials) key() []byte {
	return []byte(fmt.Sprintf("dilla.bot.spotify.v1/%s/%s/%s", s.binding.Instance, s.binding.Community, s.binding.Installation))
}
func validSpotifySecret(v string) bool {
	return v != "" && len(v) <= 8192 && !strings.ContainsAny(v, "\r\n\x00")
}

type spotifyCredentialLoadError struct{ cause error }

func (e *spotifyCredentialLoadError) Error() string   { return ErrSpotifyCredentials.Error() }
func (e *spotifyCredentialLoadError) Unwrap() []error { return []error{ErrSpotifyCredentials, e.cause} }
