package auth

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	botaccess "github.com/jonasthim/dilla/internal/bot/access"
	"github.com/jonasthim/dilla/internal/clock"
	"github.com/jonasthim/dilla/internal/config"
	"github.com/jonasthim/dilla/internal/id"
	"github.com/jonasthim/dilla/internal/mlswasi"
	"github.com/jonasthim/dilla/internal/server"
	"github.com/jonasthim/dilla/internal/store"
)

type Scope uint8

const (
	ScopeEnrolled    Scope = 0
	ScopePending     Scope = 1
	ScopeProvisional Scope = 2
)

type Purpose uint8

const (
	PurposeSession     Purpose = 0
	PurposeRenew       Purpose = 1
	PurposeProvisional Purpose = 2
)

// domain is the 16-byte separator of the signature preimage. Its length is part
// of the format: the preimage is exactly 81 bytes and every field is at a fixed
// offset, so there is nothing to parse and nothing to confuse.
const domain = "dilla session v1"

const (
	nonceBytes = 32
	nonceTTL   = 60 * time.Second
	tokenBytes = 32

	// MaxPendingNonces caps the in-memory challenge map. The challenge route is
	// unauthenticated and answers identically for an unknown device, so any
	// caller can mint nonces for arbitrary 16-byte ids; sweeping only EXPIRED
	// entries leaves a steady-state size of (request rate x 60s) with no
	// ceiling, and the per-address bucket is defeated by a distributed caller
	// while the per-device bucket is keyed by an attacker-chosen id. At 56 bytes
	// a nonce entry this is about 6 MiB.
	MaxPendingNonces = 100_000
)

// TokenHash is what the sessions table stores for a bearer token: SHA-256 of
// the token's text (protocol/02 §2.2 point 3), so a database read never yields
// a credential. It is the one place the hash is spelled: minting, resolving and
// `dillad admin` (which finds a session by the token an operator was handed)
// all go through it.
func TokenHash(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// tierBrowser is `devices.tier` for a browser device; 0 is native. The schema's
// CHECK allows only these two.
const tierBrowser uint8 = 1

// SessionPreimage builds the 81 bytes a device signs:
//
//	"dilla session v1" || instance_id(16) || device_id(16) || nonce(32) || purpose(1)
func SessionPreimage(instanceID, deviceID id.ID, nonce []byte, p Purpose) []byte {
	out := make([]byte, 0, 81)
	out = append(out, domain...)
	out = append(out, instanceID[:]...)
	out = append(out, deviceID[:]...)
	out = append(out, nonce...)
	return append(out, byte(p))
}

type Token struct {
	Token       string
	Scope       Scope
	UserID      id.ID
	DeviceID    id.ID
	Expires     int64
	IdleExpires int64
	Generation  uint64
}

type EstablishRequest struct {
	DeviceID   id.ID
	Nonce      []byte
	Purpose    Purpose
	Sig        []byte
	Credential []byte
	// Login is an enrolment assertion from one of the six auth-ceremony routes.
	// When it is present the session is a `pending` one and the assertion is
	// spent here — this endpoint is the only thing that accepts one
	// (interfaces.md §5.1).
	Login []byte
	// Registration is read only with Login for a device without a row.
	Registration *DeviceRegistration
}

// AssertionSpender is what Sessions needs from the enrolment-assertion store.
// It is an interface, and the field holding it is nil-able, because the
// concrete type lives in internal/api (task 9) and internal/api imports
// internal/auth: taking the concrete type here would be an import cycle.
type AssertionSpender interface {
	Spend(token string) (userID id.ID, needsSecondFactor bool, ok bool)
}

// DeviceLister reads the newest verified device list through the delivery service.
type DeviceLister interface {
	// ListedDevices answers the unrevoked entries of the user's newest verified list.
	ListedDevices(ctx context.Context, userID id.ID) ([]ListedDevice, error)
}

// ListedDevice is one unrevoked entry of a user's newest verified device list: the
// (device_id, dsk_pub) pair the user's SSK signed. A device row is listed only when both its id
// and its key match one entry (security review F2): judged by the key alone, a row that copies a
// listed key under a new id would pass for listed, never expire, never be evicted, refuse the
// key-less DELETE and be unrevocable by the owner's client, which revokes list entries by id.
type ListedDevice struct {
	DeviceID id.ID
	DSKPub   []byte
}

// Listed reports whether the newest verified list names exactly this device: its id and its
// signing key in one unrevoked entry.
func Listed(entries []ListedDevice, deviceID id.ID, pub []byte) bool {
	for _, e := range entries {
		if e.DeviceID == deviceID && bytes.Equal(e.DSKPub, pub) {
			return true
		}
	}
	return false
}

// ErrNoDeviceList marks the no-list clause of protocol/02 device sessions.
var ErrNoDeviceList = errors.New("auth: the user has published no device list")

// DeviceRegistration is establish element 3 when login is present.
type DeviceRegistration struct {
	DeviceID         id.ID
	DSKPub           []byte
	Tier, SignerTier uint8
	Credential       []byte
}

// Session is a resolved bearer token.
type Session struct {
	UserID    id.ID
	DeviceID  id.ID
	Scope     Scope
	TokenHash []byte
	// PairingGroup is the one group a ScopeProvisional session may act on; it is nil for every
	// other scope (deviation B9). Without it nothing enforces E_PROVISIONAL_OUTSIDE_PAIRING and a
	// provisional session can harvest the Welcomes of every group its device was ever added to —
	// the escalation interfaces.md §2.2 point 4 forbids.
	//
	// TASK 24 ENFORCED THE RULE AND DID NOT ADD THE COLUMN. `DS.PublishKeyPackages`, `DS.Welcomes`
	// and `DS.AckWelcome` refuse a provisional session that reaches outside this field, and refuse
	// outright when it is nil — so the surface the three "E or V" routes open is closed from the
	// day they exist. What is still missing is the `sessions.pairing_group` column this would be
	// read from and the writer that sets it, which needs a migration on both engines and a pairing
	// flow to set it from; neither is in task 24's file list, and Plan 1 mints no provisional
	// session that names a group, so the field is nil for every session today and the delivery
	// service refuses accordingly. That is the FAIL-CLOSED end of the gap: a paired device cannot
	// yet collect its Welcome with a provisional session, and must be enrolled first.
	//
	// The column, the sqlc regeneration, `mint`/`Resolve` carrying the value and the pairing flow
	// that sets it are owed by an explicit task, recorded as plan deviation B27; until it lands,
	// rows 10, 15 and 16 are enrolled-only in practice.
	PairingGroup *id.ID
	Expires      int64
	IdleExpires  int64
	// FactorOwed is the account's users.factor_owed when the token was resolved (web3.6 S1): the
	// account holds no second factor yet, so an enrolled session of it reaches only protocol/09's
	// `F` and `P` routes (Middleware, MiddlewareOwedOK) and never the gateway.
	FactorOwed   bool
	WireVersion  uint64
	E2EEVersion  uint64
	MediaVersion uint64
	// Bot is non-nil only for a repository-validated session bound to a live
	// managed installation. It is not inferred from users.kind.
	Bot *BotSessionBinding
}

// BotSessionBinding is the managed installation context carried by an
// authenticated bot session. Capabilities remain installation constraints;
// API permission checks continue to use their existing vocabulary.
type BotSessionBinding = botaccess.Binding

// InPairingGroup reports whether this session may act on a group. An enrolled or admin session is
// unrestricted; a provisional session may act only inside the one group it was paired for, and a
// provisional session with no pairing group may act on nothing.
func (s Session) InPairingGroup(group id.ID) bool {
	if s.Scope != ScopeProvisional {
		return true
	}
	return s.PairingGroup != nil && *s.PairingGroup == group
}

type nonceEntry struct {
	device  id.ID
	expires time.Time
}

// Sessions issues and resolves device sessions. Nonces live in memory only:
// they are single-use, 60 seconds old at most, and writing them to the database
// would buy nothing but write amplification on the unauthenticated path.
type Sessions struct {
	repo       store.Repository
	clk        clock.Clock
	cfg        config.Session
	instanceID id.ID
	generation uint64

	// nonces holds the live challenges; order is the same keys in insertion
	// order, with head the index of the oldest one still worth looking at.
	//
	// The queue is not decoration. Every nonce is minted with the SAME 60-second
	// TTL from a monotonic clock, so insertion order IS expiry order: the sweep
	// and the eviction at the cap both only ever touch the front, and each
	// Challenge costs amortised O(1). Sweeping by iterating the whole map, as an
	// earlier draft of this file did, is O(len(nonces)) per Challenge, and the
	// map is allowed to hold MaxPendingNonces = 100 000 entries — a quadratic
	// cost on exactly the unauthenticated path the cap exists to protect.
	// An `order` entry whose key is no longer in the map was consumed by
	// takeNonce and is skipped.
	mu     sync.Mutex
	nonces map[string]nonceEntry
	order  []string
	head   int

	verifies atomic.Int64

	// Assertions is the enrolment-assertion store. It is set by dillad.New and
	// may be nil, in which case every request carrying a Login is refused
	// rather than quietly downgraded to an enrolled session.
	Assertions AssertionSpender
	// DeviceLists is set by the composition root. Nil fails every ordinary establish closed.
	DeviceLists DeviceLister

	// OnRevoke is called inside RevokeDevice/RevokeUser so the gateway closes
	// the device's connections in the same operation the rows are deleted.
	OnRevoke func(deviceID id.ID)
}

// NewSessions takes the instance generation because it is element 6 of every
// 201 from POST /v1/devices/{device_id}/sessions, and it must be the same number
// the X-Dilla-Generation header carries: a client that sees 0 in one and 3 in
// the other cannot tell a restore from a bug (R29, D12).
func NewSessions(repo store.Repository, clk clock.Clock, c config.Session, instanceID id.ID, generation uint64) *Sessions {
	return &Sessions{repo: repo, clk: clk, cfg: c, instanceID: instanceID,
		generation: generation, nonces: map[string]nonceEntry{}}
}

// PendingNonces is the number of live challenges, which one test uses to prove
// the map is capped.
func (s *Sessions) PendingNonces() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.nonces)
}

func (s *Sessions) InstanceID() id.ID { return s.instanceID }

// VerifyCount is the number of Ed25519 verifications performed, which one test
// uses to prove the nonce is consumed before the signature is checked.
func (s *Sessions) VerifyCount() int64 { return s.verifies.Load() }

// Challenge mints a nonce. The answer is identical for an unknown device, so
// the endpoint is not a device-enumeration oracle.
func (s *Sessions) Challenge(ctx context.Context, deviceID id.ID) ([]byte, int64, error) {
	nonce := make([]byte, nonceBytes)
	if _, err := rand.Read(nonce); err != nil {
		return nil, 0, err
	}
	expires := s.clk.Now().Add(nonceTTL)
	s.mu.Lock()
	s.sweepNoncesLocked()
	for len(s.nonces) >= MaxPendingNonces && s.evictOldestNonceLocked() {
	}
	s.nonces[string(nonce)] = nonceEntry{device: deviceID, expires: expires}
	s.order = append(s.order, string(nonce))
	s.mu.Unlock()
	return nonce, expires.Unix(), nil
}

// evictOldestNonceLocked drops the oldest live challenge and reports whether it
// found one. The caller loops on that answer rather than on the map's size
// alone: an order queue and a map that ever disagreed would otherwise spin
// forever on the instance's unauthenticated path.
func (s *Sessions) evictOldestNonceLocked() bool {
	evicted := false
	for s.head < len(s.order) {
		k := s.order[s.head]
		s.head++
		if _, live := s.nonces[k]; live {
			delete(s.nonces, k)
			evicted = true
			break
		}
	}
	s.compactOrderLocked()
	return evicted
}

// takeNonce consumes a nonce. It runs BEFORE the signature check, so a replay
// costs a map lookup rather than an Ed25519 verification.
func (s *Sessions) takeNonce(nonce []byte, deviceID id.ID) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.nonces[string(nonce)]
	if !ok {
		return false
	}
	delete(s.nonces, string(nonce))
	if entry.device != deviceID {
		return false
	}
	return !s.clk.Now().After(entry.expires)
}

// sweepNoncesLocked drops the expired and already-consumed entries at the front
// of the queue. It stops at the first live, unexpired one: everything behind it
// was minted later and therefore expires later.
func (s *Sessions) sweepNoncesLocked() {
	now := s.clk.Now()
	for s.head < len(s.order) {
		k := s.order[s.head]
		e, live := s.nonces[k]
		if live && !now.After(e.expires) {
			break
		}
		if live {
			delete(s.nonces, k)
		}
		s.head++
	}
	s.compactOrderLocked()
}

// compactOrderLocked reclaims the consumed prefix of the queue once it is at
// least half of it, which keeps the slice O(len(nonces)) without copying on
// every call.
func (s *Sessions) compactOrderLocked() {
	if s.head == 0 || s.head*2 < len(s.order) {
		return
	}
	s.order = append(s.order[:0], s.order[s.head:]...)
	s.head = 0
}

// ProveDeviceKey checks that the caller holds the private half of dskPub, the key a device row is
// about to be registered under, by the establish route's own proof: a nonce from
// POST /v1/devices/{device_id}/sessions/challenge for that device_id, consumed here, and an Ed25519
// signature by dskPub over SessionPreimage(instance_id, device_id, nonce, purpose 0) (security
// review F2). Without it POST /v1/devices took any dsk_pub, and a stolen enrolled session could
// plant rows under a key it does not hold. A nonce is single use, so a proof spent here cannot be
// replayed as an establish. It answers 403 E_FORBIDDEN: the caller's session is good, its claim
// on the key is not.
func (s *Sessions) ProveDeviceKey(deviceID id.ID, dskPub, nonce, sig []byte) error {
	refused := server.Errorf(server.CodeForbidden, "possession of dsk_pub is not proven")
	if len(dskPub) != ed25519.PublicKeySize || len(nonce) != nonceBytes || len(sig) != ed25519.SignatureSize {
		return refused
	}
	if !s.takeNonce(nonce, deviceID) {
		return refused
	}
	s.verifies.Add(1)
	if !ed25519.Verify(ed25519.PublicKey(dskPub), SessionPreimage(s.instanceID, deviceID, nonce, PurposeSession), sig) {
		return refused
	}
	return nil
}

func (s *Sessions) Establish(ctx context.Context, r EstablishRequest) (Token, error) {
	return s.issue(ctx, r)
}

// Renew is Establish behind the purpose check. Beyond that byte the two paths
// are deliberately identical: protocol/02 § Device sessions item 5 renews a
// session by establishing a new one against a fresh nonce, and the purpose is
// bound into the signature so a renew signature cannot be replayed as an
// establish.
func (s *Sessions) Renew(ctx context.Context, r EstablishRequest) (Token, error) {
	if r.Purpose != PurposeRenew {
		return Token{}, server.Errorf(server.CodeUnauthenticated, "renew requires purpose 1")
	}
	return s.issue(ctx, r)
}

// NewDeviceSession mints a session for a device created in the SAME
// transaction, which is the one carve-out from the proof-of-possession rule:
// protocol/02 § Device sessions item 7 and deviation ID18 —
// POST /v1/accounts registers the device's key and its first session together,
// so there is no prior key to prove possession of and no nonce the client could
// have asked for yet. Every later session for that device goes through
// Challenge and Establish.
//
// tx is the transaction the user, the device and the redeemed invite were
// written in, so a rolled-back registration leaves no session behind.
func (s *Sessions) NewDeviceSession(ctx context.Context, tx store.Repository, userID, deviceID id.ID, tier uint8) (Token, error) {
	return s.mint(ctx, tx, userID, deviceID, tier, ScopeEnrolled)
}

// mintBotSession is called only after Establish has consumed a nonce and
// verified the registered native device's purpose-0/1 signature. It creates
// the session and exact token-hash binding in one transaction.
func (s *Sessions) mintBotSession(ctx context.Context, userID, deviceID id.ID) (Token, error) {
	var tok Token
	err := s.repo.Tx(ctx, func(tx store.Repository) error {
		binding, isBot, err := botaccess.ForUserDevice(ctx, tx, userID, deviceID)
		if err != nil {
			return err
		}
		if !isBot || binding.State != store.BotInstallationEnrolling && binding.State != store.BotInstallationJoining && binding.State != store.BotInstallationReady {
			return server.Errorf(server.CodeForbidden, "managed bot installation is not active")
		}
		tok, err = s.mintSession(ctx, tx, userID, deviceID, 0, ScopeEnrolled, true)
		if err != nil {
			return err
		}
		return tx.CreateBotSessionBinding(ctx, store.BotSessionBindingRow{
			TokenHash: TokenHash(tok.Token), InstallationID: binding.InstallationID,
			UserID: userID, DeviceID: deviceID, Created: s.clk.Now().Unix(),
		})
	})
	if err != nil {
		return Token{}, err
	}
	return tok, nil
}

func (s *Sessions) issue(ctx context.Context, r EstablishRequest) (Token, error) {
	unauth := server.Errorf(server.CodeUnauthenticated, "")
	if len(r.Nonce) != nonceBytes || len(r.Sig) != ed25519.SignatureSize {
		return Token{}, unauth
	}
	if !s.takeNonce(r.Nonce, r.DeviceID) {
		return Token{}, unauth
	}
	device, err := s.repo.GetDevice(ctx, r.DeviceID)
	if errors.Is(err, store.ErrNotFound) && len(r.Login) > 0 {
		return s.register(ctx, r)
	}
	if err != nil {
		return Token{}, unauth
	}
	assertionSpent, collision := s.crossUserIDCollision(r, device.UserID)
	if collision != nil {
		return Token{}, collision
	}
	if device.RevokedAt != nil {
		return Token{}, unauth
	}
	// ed25519.Verify PANICS on a key that is not exactly 32 bytes.
	if len(device.DSKPub) != ed25519.PublicKeySize {
		return Token{}, unauth
	}
	pre := SessionPreimage(s.instanceID, r.DeviceID, r.Nonce, r.Purpose)
	s.verifies.Add(1)
	if !ed25519.Verify(ed25519.PublicKey(device.DSKPub), pre, r.Sig) {
		return Token{}, unauth
	}
	user, err := s.repo.GetUser(ctx, device.UserID)
	if err != nil {
		return Token{}, unauth
	}
	if user.Kind == 1 {
		// Bot sessions use only the ordinary device challenge and exact purpose
		// 0/1 proof. Assertions, registration, and provisional purpose 2 never
		// mint bot authority.
		if len(r.Login) != 0 || r.Registration != nil || r.Purpose == PurposeProvisional || r.Purpose > PurposeRenew {
			return Token{}, unauth
		}
		return s.mintBotSession(ctx, device.UserID, device.ID)
	}
	if user.DisabledAt != nil || user.DeletedAt != nil {
		return Token{}, server.Errorf(server.CodeForbidden, "account disabled")
	}
	if len(r.Login) > 0 && s.DeviceLists != nil && device.Created <= s.clk.Now().Unix()-86400 {
		entries, listErr := s.DeviceLists.ListedDevices(ctx, device.UserID)
		if listErr != nil && !errors.Is(listErr, ErrNoDeviceList) {
			return Token{}, ListError(listErr)
		}
		if listErr == nil {
			if !Listed(entries, device.ID, device.DSKPub) {
				if err := s.RevokeDevice(ctx, device.ID); err != nil {
					return Token{}, err
				}
				return Token{}, unauth
			}
		}
	}
	scope, err := s.scopeFor(ctx, r, device, assertionSpent)
	if err != nil {
		return Token{}, err
	}
	var tok Token
	if err := s.repo.Tx(ctx, func(tx store.Repository) error {
		var err error
		tok, err = s.mint(ctx, tx, device.UserID, r.DeviceID, device.Tier, scope)
		return err
	}); err != nil {
		return Token{}, err
	}
	return tok, nil
}

// A registration assertion for an occupied id must not be mistaken for a bad
// signature by that id's existing device. Verify the proposed key and spend the
// assertion before revealing the collision in the registration error vocabulary.
func (s *Sessions) crossUserIDCollision(r EstablishRequest, existingUser id.ID) (bool, error) {
	reg := r.Registration
	if len(r.Login) == 0 || reg == nil || s.Assertions == nil || reg.DeviceID != r.DeviceID ||
		reg.Tier != 1 || reg.SignerTier != 1 || len(reg.DSKPub) != ed25519.PublicKeySize {
		return false, nil
	}
	s.verifies.Add(1)
	if !ed25519.Verify(reg.DSKPub, SessionPreimage(s.instanceID, r.DeviceID, r.Nonce, r.Purpose), r.Sig) {
		return false, nil
	}
	user, second, ok := s.Assertions.Spend(string(r.Login))
	if !ok || second {
		return false, nil
	}
	if user == existingUser {
		return true, nil
	}
	return false, server.WithStatus(http.StatusConflict, server.Errorf(server.CodeInvalidRequest, "device_id already exists"))
}

// ListError maps a verified-list failure to the session/API refusal.
func ListError(err error) error {
	var abi *mlswasi.ABIError
	if errors.As(err, &abi) && abi.Code == "E_CREDENTIAL" {
		return server.Errorf(server.CodeUnauthenticated, "")
	}
	return server.Unavailable(1000, "device list unavailable")
}

func (s *Sessions) scopeFor(ctx context.Context, r EstablishRequest, device store.DeviceRow, assertionSpent bool) (Scope, error) {
	unauth := server.Errorf(server.CodeUnauthenticated, "")
	if len(r.Login) > 0 {
		if assertionSpent {
			return ScopePending, nil
		}
		if s.Assertions == nil {
			return 0, unauth
		}
		user, second, ok := s.Assertions.Spend(string(r.Login))
		if !ok || second || user != device.UserID {
			return 0, unauth
		}
		return ScopePending, nil
	}
	// Purpose 2 goes through the same list gate as purposes 0 and 1 (branch review
	// REGISTRATION-DEVICES-01): it used to return provisional before the list or the 24-hour expiry
	// was read, so a row a password holder registered minted provisional sessions for ever.
	provisional := r.Purpose == PurposeProvisional
	if s.DeviceLists == nil {
		return 0, unauth
	}
	entries, err := s.DeviceLists.ListedDevices(ctx, device.UserID)
	if errors.Is(err, ErrNoDeviceList) {
		if provisional {
			return ScopeProvisional, nil
		}
		return ScopeEnrolled, nil
	}
	if err != nil {
		return 0, ListError(err)
	}
	listed := Listed(entries, device.ID, device.DSKPub)
	if !listed && device.Created <= s.clk.Now().Unix()-86400 {
		if err := s.RevokeDevice(ctx, device.ID); err != nil {
			return 0, err
		}
		return 0, unauth
	}
	if provisional {
		// Pairing is not built (deviation B27) and protocol/03 pairing is the native path. A browser
		// row its user's list exists for and that was never verified is what assertion registration
		// creates; nothing in it has a pairing to do, so it is refused purpose 2, listed or not.
		// No path sets verified_at today, so this is every browser row of a listed user.
		if device.Tier == tierBrowser && device.VerifiedAt == nil {
			return 0, unauth
		}
		return ScopeProvisional, nil
	}
	if listed {
		return ScopeEnrolled, nil
	}
	return ScopePending, nil
}

// register spends an assertion after shape and signature checks, then admits only a listed
// account's browser device. Past the cap or the hourly rate it replaces the oldest unlisted row
// (AdmitDevice) rather than refuse, so a password holder cannot lock out a recovering owner.
func (s *Sessions) register(ctx context.Context, r EstablishRequest) (Token, error) {
	unauth := server.Errorf(server.CodeUnauthenticated, "")
	if s.Assertions == nil {
		return Token{}, unauth
	}
	reg := r.Registration
	if reg == nil {
		return Token{}, server.Errorf(server.CodeInvalidRequest, "registration array required")
	}
	if reg.DeviceID != r.DeviceID {
		return Token{}, server.Errorf(server.CodeInvalidRequest, "device_id does not match")
	}
	if reg.Tier != 1 || reg.SignerTier != 1 {
		return Token{}, server.Errorf(server.CodeInvalidRequest, "assertion registration is for browser devices")
	}
	if len(reg.DSKPub) != 32 {
		return Token{}, server.Errorf(server.CodeInvalidRequest, "dsk_pub is 32 bytes")
	}
	s.verifies.Add(1)
	if !ed25519.Verify(reg.DSKPub, SessionPreimage(s.instanceID, r.DeviceID, r.Nonce, r.Purpose), r.Sig) {
		return Token{}, unauth
	}
	userID, second, ok := s.Assertions.Spend(string(r.Login))
	if !ok || second {
		return Token{}, unauth
	}
	user, err := s.repo.GetUser(ctx, userID)
	if err != nil {
		return Token{}, unauth
	}
	if user.DisabledAt != nil || user.DeletedAt != nil {
		return Token{}, server.Errorf(server.CodeForbidden, "account disabled")
	}
	if s.DeviceLists == nil {
		return Token{}, unauth
	}
	entries, err := s.DeviceLists.ListedDevices(ctx, userID)
	if errors.Is(err, ErrNoDeviceList) {
		return Token{}, unauth
	}
	if err != nil {
		return Token{}, ListError(err)
	}
	now := s.clk.Now().Unix()
	var tok Token
	var evicted id.ID
	err = s.repo.Tx(ctx, func(tx store.Repository) error {
		var err error
		evicted, err = s.AdmitDevice(ctx, tx, userID, entries, false, reg.DSKPub, now)
		if err != nil {
			return err
		}
		if err := tx.CreateDevice(ctx, store.DeviceRow{ID: r.DeviceID, UserID: userID, DSKPub: reg.DSKPub,
			Tier: 1, SignerTier: 1, CredentialBlob: reg.Credential, LastSeen: now, Created: now}); err != nil {
			if errors.Is(err, store.ErrConflict) {
				return server.WithStatus(http.StatusConflict, server.Errorf(server.CodeInvalidRequest, "device_id already exists"))
			}
			return err
		}
		tok, err = s.mint(ctx, tx, userID, r.DeviceID, 1, ScopePending)
		return err
	})
	if err != nil {
		return Token{}, err
	}
	if !evicted.IsZero() && s.OnRevoke != nil {
		s.OnRevoke(evicted)
	}
	return tok, nil
}

// AdmitDevice applies the common device cap, expiry and hourly rate inside the
// caller's transaction, for assertion registration. Neither the cap nor the rate refuses while an
// unlisted live row exists: past either, the oldest unlisted live row is evicted. Only a cap of
// listed rows refuses (403). The caller inserts its new row in that same
// transaction and closes any evicted device's gateway connections after commit.
func (s *Sessions) AdmitDevice(ctx context.Context, tx store.Repository, userID id.ID, entries []ListedDevice, noList bool, dskPub []byte, now int64) (id.ID, error) {
	return s.admit(ctx, tx, userID, entries, noList, dskPub, now, true)
}

// AdmitEnrolledDevice is AdmitDevice for POST /v1/devices: the same expiry sweep, key rule, cap
// and rate, but past the cap it refuses 403 "device cap reached" and past the rate 429 with the
// wait until a creation leaves the hour, and it never evicts (branch review REGISTRATION-DEVICES-03).
// Its caller is enrolled, pays no host login, and can free a slot in Settings → Devices; were it to
// evict, a stolen enrolled session would replace a recovering owner's new row at will. Eviction is
// left to assertion registration, where a refusal would be the lockout.
func (s *Sessions) AdmitEnrolledDevice(ctx context.Context, tx store.Repository, userID id.ID, entries []ListedDevice, noList bool, dskPub []byte, now int64) error {
	_, err := s.admit(ctx, tx, userID, entries, noList, dskPub, now, false)
	return err
}

func (s *Sessions) admit(ctx context.Context, tx store.Repository, userID id.ID, entries []ListedDevice, noList bool, dskPub []byte, now int64, evict bool) (id.ID, error) {
	if err := tx.LockUserForDeviceRegistration(ctx, userID); err != nil {
		return id.ID{}, err
	}
	rows, err := tx.ListDevicesByUser(ctx, userID)
	if err != nil {
		return id.ID{}, err
	}
	listed := make([]id.ID, 0, len(rows))
	for _, row := range rows {
		if noList || Listed(entries, row.ID, row.DSKPub) {
			listed = append(listed, row.ID)
		}
	}
	cutoff := now - 86399
	for _, row := range rows {
		if row.RevokedAt == nil && row.Created < cutoff && !slices.Contains(listed, row.ID) {
			if err := tx.RevokeDevice(ctx, row.ID, now); err != nil {
				return id.ID{}, err
			}
			if _, err := tx.DeleteSessionsByDevice(ctx, row.ID); err != nil {
				return id.ID{}, err
			}
		}
	}
	// One live row per signing key (security review F2): a second row under a key a live row of
	// the user already holds would share that row's proof of possession and, judged by key, its
	// listing. Rows the sweep above just revoked hold their key no longer.
	for _, row := range rows {
		expired := row.Created < cutoff && !slices.Contains(listed, row.ID)
		if row.RevokedAt == nil && !expired && bytes.Equal(row.DSKPub, dskPub) {
			return id.ID{}, server.WithStatus(http.StatusConflict,
				server.Errorf(server.CodeInvalidRequest, "dsk_pub is already registered to a live device"))
		}
	}
	live, err := tx.CountLiveDevicesByUser(ctx, userID)
	if err != nil {
		return id.ID{}, err
	}
	creations, err := tx.ListLiveDeviceCreationsSince(ctx, userID, listed, now-3599, cutoff)
	if err != nil {
		return id.ID{}, err
	}
	atCap := live >= int64(s.cfg.MaxDevicesPerUser)
	if !atCap && len(creations) < s.cfg.EnrolmentsPerHour {
		return id.ID{}, nil
	}
	if !evict {
		if atCap {
			return id.ID{}, server.Errorf(server.CodeForbidden, "device cap reached")
		}
		return id.ID{}, server.RateLimitedAfter(time.Duration(rateWait(creations, s.cfg.EnrolmentsPerHour, now)) * time.Second)
	}
	// The cap and the hourly rate decide WHICH row this registration replaces, never WHETHER it is
	// admitted (security review F1): the oldest live unlisted row, whatever its age. At registration
	// the instance cannot tell the owner from a holder of the password alone — the recovery key that
	// tells them apart is used only after it — so a refusal that a password holder can keep
	// saturated (three logins an hour against the rate, or young unlisted rows against the cap) would
	// lock the owner's recovery out. The owner's own new row is exposed only for the seconds between
	// its registration and the list PUT that names it, at one password login per eviction, and the
	// per-address `login` and `establish` buckets bound that churn. The one refusal left is a cap of
	// rows every one of which the user's signed list names.
	var evicted id.ID
	var oldest int64
	for _, row := range rows {
		if row.RevokedAt == nil && row.Created >= cutoff && !slices.Contains(listed, row.ID) &&
			(evicted.IsZero() || row.Created < oldest || (row.Created == oldest && bytes.Compare(row.ID[:], evicted[:]) < 0)) {
			evicted, oldest = row.ID, row.Created
		}
	}
	if evicted.IsZero() {
		if atCap {
			return id.ID{}, server.Errorf(server.CodeForbidden, "device cap reached")
		}
		// Past the rate with every live row listed: nothing to replace, and an honest person
		// enrolling a fourth listed device in an hour is not refused.
		return id.ID{}, nil
	}
	if err := tx.RevokeDevice(ctx, evicted, now); err != nil {
		return id.ID{}, err
	}
	if _, err := tx.DeleteSessionsByDevice(ctx, evicted); err != nil {
		return id.ID{}, err
	}
	return evicted, nil
}

// rateWait is the seconds until enough of the hour's live creations leave the window that one more
// is under rate: a creation at c counts while c >= now-3599, so it leaves at c+3600.
func rateWait(creations []int64, rate int, now int64) int64 {
	sorted := slices.Clone(creations)
	slices.Sort(sorted)
	i := len(sorted) - rate
	switch {
	case len(sorted) == 0:
		return 3600
	case i < 0:
		i = 0
	case i >= len(sorted):
		i = len(sorted) - 1
	}
	if wait := sorted[i] + 3600 - now; wait > 0 {
		return wait
	}
	return 1
}

// mint writes one session row through tx and returns the bearer token. It is
// the single place a session's token, lifetime and idle window are decided, so
// the registration carve-out and the ordinary challenge path cannot drift.
func (s *Sessions) mint(ctx context.Context, tx store.Repository, userID, deviceID id.ID, tier uint8, scope Scope) (Token, error) {
	return s.mintSession(ctx, tx, userID, deviceID, tier, scope, false)
}

func (s *Sessions) mintSession(ctx context.Context, tx store.Repository, userID, deviceID id.ID, tier uint8, scope Scope, managedBot bool) (Token, error) {
	user, err := tx.GetUser(ctx, userID)
	if err != nil {
		return Token{}, err
	}
	if user.Kind == 1 && !managedBot || user.Kind != 1 && managedBot {
		return Token{}, server.Errorf(server.CodeForbidden, "managed bot sessions require an installation binding")
	}
	raw := make([]byte, tokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return Token{}, err
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	sum := TokenHash(token)

	now := s.clk.Now()
	lifetime := s.cfg.NativeLifetime.Value()
	idle := lifetime
	if tier == tierBrowser {
		lifetime = s.cfg.BrowserLifetime.Value()
		idle = s.cfg.BrowserIdle.Value()
	}
	row := store.SessionRow{
		TokenHash: sum, DeviceID: deviceID, UserID: userID, Scope: uint8(scope),
		// The tier is carried on the session so Resolve can slide the idle
		// window by it without a second query (deviation ID15).
		Tier:    tier,
		Created: now.Unix(), Expires: now.Add(lifetime).Unix(), IdleExpires: now.Add(idle).Unix(),
	}
	n, err := tx.CountSessionsByDevice(ctx, deviceID)
	if err != nil {
		return Token{}, err
	}
	for ; n >= int64(s.cfg.MaxPerDevice); n-- {
		if err := tx.DeleteOldestSessionForDevice(ctx, deviceID); err != nil {
			return Token{}, err
		}
	}
	if err := tx.CreateSession(ctx, row); err != nil {
		return Token{}, err
	}
	return Token{
		Token: token, Scope: scope, UserID: userID, DeviceID: deviceID,
		Expires: row.Expires, IdleExpires: row.IdleExpires, Generation: s.generation,
	}, nil
}

// touchGranularity is how far, in seconds, Resolve lets a session's idle expiry lag behind its
// maximum before it writes the slide.
const touchGranularity = 60

// Resolve turns a bearer token into a session and slides its idle window.
func (s *Sessions) Resolve(ctx context.Context, bearer string) (Session, error) {
	if bearer == "" {
		return Session{}, server.Errorf(server.CodeUnauthenticated, "")
	}
	return s.ResolveHash(ctx, TokenHash(bearer))
}

// ResolveHash applies the same current-session authority checks to a gateway's stored opaque
// hash. It is an internal transport adapter, never an HTTP credential or a client-supplied value.
func (s *Sessions) ResolveHash(ctx context.Context, sum []byte) (Session, error) {
	if len(sum) != 32 {
		return Session{}, server.Errorf(server.CodeUnauthenticated, "")
	}
	now := s.clk.Now()
	row, err := s.repo.GetSessionByHash(ctx, sum, now.Unix())
	if err != nil {
		return Session{}, server.Errorf(server.CodeUnauthenticated, "")
	}
	// The lookup is by a unique index, so this compare is redundant today; it
	// stays so that a future non-unique index cannot reintroduce a timing side
	// channel without someone deleting this line on purpose.
	if subtle.ConstantTimeCompare(row.TokenHash, sum) != 1 {
		return Session{}, server.Errorf(server.CodeUnauthenticated, "")
	}
	botBinding, botBound, err := s.currentBotBinding(ctx, row, sum)
	if err != nil {
		if errors.Is(err, botaccess.ErrIneligible) || errors.Is(err, store.ErrNotFound) {
			return Session{}, server.Errorf(server.CodeUnauthenticated, "")
		}
		return Session{}, server.Unavailable(1000, "session authorization could not be checked")
	}
	if Scope(row.Scope) == ScopePending && s.DeviceLists != nil {
		device, err := s.repo.GetDevice(ctx, row.DeviceID)
		if err != nil || device.RevokedAt != nil {
			return Session{}, server.Errorf(server.CodeUnauthenticated, "")
		}
		if device.Created <= now.Unix()-86400 {
			entries, err := s.DeviceLists.ListedDevices(ctx, row.UserID)
			if err != nil && !errors.Is(err, ErrNoDeviceList) {
				return Session{}, ListError(err)
			}
			if err == nil && !Listed(entries, device.ID, device.DSKPub) {
				if err := s.RevokeDevice(ctx, device.ID); err != nil {
					return Session{}, err
				}
				return Session{}, server.Errorf(server.CodeUnauthenticated, "")
			}
		}
	}
	// The idle window follows the DEVICE TIER, exactly as issue() picks it, and
	// never the session scope: a browser device holds an ordinary `enrolled`
	// session, so keying on the scope would slide it 30 days on every request
	// and defeat protocol/02 § Device sessions item 5. The new value is clamped
	// to the session's hard expiry, so idle_expires can never run past expires.
	idle := s.cfg.NativeLifetime.Value()
	if row.Tier == tierBrowser {
		idle = s.cfg.BrowserIdle.Value()
	}
	idleExpires := now.Add(idle).Unix()
	if idleExpires > row.Expires {
		idleExpires = row.Expires
	}
	// One write per request would put every authenticated read on the single SQLite writer; the
	// window only has to move when it moves by more than touchGranularity, and an idle window
	// that is a minute short of its maximum is still hours or days long.
	if idleExpires-row.IdleExpires > touchGranularity {
		_ = s.repo.TouchSession(ctx, sum, idleExpires)
	} else {
		idleExpires = row.IdleExpires
	}
	session := Session{
		UserID: row.UserID, DeviceID: row.DeviceID, Scope: Scope(row.Scope),
		TokenHash: row.TokenHash, Expires: row.Expires, IdleExpires: idleExpires,
		FactorOwed:  row.FactorOwed,
		WireVersion: row.WireVersion, E2EEVersion: row.E2EEVersion, MediaVersion: row.MediaVersion,
	}
	if botBound {
		session.Bot = &botBinding
	}
	return session, nil
}

// ValidateSessionHash is the gateway's heartbeat-time revocation check for a
// bound bot connection. It uses the stored opaque token hash and reads current
// account/device/installation/runtime state without accepting client headers.
func (s *Sessions) ValidateSessionHash(ctx context.Context, tokenHash []byte) error {
	if len(tokenHash) != 32 {
		return store.ErrNotFound
	}
	row, err := s.repo.GetSessionByHash(ctx, tokenHash, s.clk.Now().Unix())
	if err != nil {
		return err
	}
	if subtle.ConstantTimeCompare(row.TokenHash, tokenHash) != 1 {
		return store.ErrNotFound
	}
	_, bound, err := s.currentBotBinding(ctx, row, tokenHash)
	if err != nil {
		if errors.Is(err, botaccess.ErrIneligible) {
			return store.ErrNotFound
		}
		return err
	}
	if !bound || Scope(row.Scope) != ScopeEnrolled {
		return store.ErrNotFound
	}
	return nil
}

// currentBotBinding treats the DB binding as a claim to verify, not authority:
// every account/device/installation/runtime condition is read again per request.
func (s *Sessions) currentBotBinding(ctx context.Context, session store.SessionRow, tokenHash []byte) (BotSessionBinding, bool, error) {
	stored, storedErr := s.repo.GetBotSessionBinding(ctx, tokenHash)
	if storedErr != nil && !errors.Is(storedErr, store.ErrNotFound) {
		return BotSessionBinding{}, false, storedErr
	}
	binding, isBot, eligibilityErr := botaccess.ForUserDevice(ctx, s.repo, session.UserID, session.DeviceID)
	if eligibilityErr != nil {
		return BotSessionBinding{}, false, eligibilityErr
	}
	if !isBot {
		if storedErr == nil {
			return BotSessionBinding{}, false, botaccess.ErrIneligible
		}
		return BotSessionBinding{}, false, nil
	}
	if errors.Is(storedErr, store.ErrNotFound) || stored.InstallationID != binding.InstallationID ||
		stored.UserID != session.UserID || stored.DeviceID != session.DeviceID ||
		!bytes.Equal(stored.TokenHash, tokenHash) || Scope(session.Scope) != ScopeEnrolled {
		return BotSessionBinding{}, false, botaccess.ErrIneligible
	}
	return binding, true, nil
}

// RecordCapabilities persists a negotiated selection against the exact bearer session hash.
// The caller must first resolve the bearer; a deleted/revoked row then makes this write fail.
func (s *Sessions) RecordCapabilities(ctx context.Context, tokenHash []byte, wire, e2ee, media uint64) error {
	if len(tokenHash) != 32 || wire < 2 || wire > 5 || e2ee != 1 || media != 1 {
		return errors.New("auth: invalid negotiated capabilities")
	}
	return s.repo.SetSessionVersions(ctx, tokenHash, wire, e2ee, media)
}

func (s *Sessions) RevokeDevice(ctx context.Context, deviceID id.ID) error {
	if err := s.repo.Tx(ctx, func(tx store.Repository) error {
		if err := tx.RevokeDevice(ctx, deviceID, s.clk.Now().Unix()); err != nil {
			return err
		}
		_, err := tx.DeleteSessionsByDevice(ctx, deviceID)
		return err
	}); err != nil {
		return err
	}
	if s.OnRevoke != nil {
		s.OnRevoke(deviceID)
	}
	return nil
}

// DeleteDeviceSessions drops every session row of one device and closes its
// gateway connections, WITHOUT revoking the device: it is "log this device
// out", where RevokeDevice is "this device is no longer trusted". It returns
// how many session rows went.
//
// It exists so the two logout paths cannot drift. protocol/02 § Device sessions
// item 6 ties deleting session rows to closing the sockets those rows
// authenticated; a caller that reached store.DeleteSessionsByDevice directly
// would remove the HTTP credential and leave the gateway connection open.
func (s *Sessions) DeleteDeviceSessions(ctx context.Context, deviceID id.ID) (int64, error) {
	n, err := s.repo.DeleteSessionsByDevice(ctx, deviceID)
	if err != nil {
		return 0, err
	}
	if s.OnRevoke != nil {
		s.OnRevoke(deviceID)
	}
	return n, nil
}

func (s *Sessions) RevokeUser(ctx context.Context, userID id.ID) error {
	devices, err := s.repo.ListDevicesByUser(ctx, userID)
	if err != nil {
		return err
	}
	if err := s.repo.Tx(ctx, func(tx store.Repository) error {
		now := s.clk.Now().Unix()
		if err := tx.SetUserDisabled(ctx, userID, &now); err != nil {
			return err
		}
		_, err := tx.DeleteSessionsByUser(ctx, userID)
		return err
	}); err != nil {
		return err
	}
	if s.OnRevoke != nil {
		for _, d := range devices {
			s.OnRevoke(d.ID)
		}
	}
	return nil
}

type ctxKey struct{}

// Middleware resolves the bearer token and enforces the wanted scope. An enrolled session whose
// account owes a second factor is refused with E_FACTOR_REQUIRED on every route that does not also
// admit a pending session (protocol/02 § Device sessions item 8): default-deny, so a route added
// later is closed to it unless it is mounted through MiddlewareOwedOK.
func (s *Sessions) Middleware(next http.Handler, want Scope) http.Handler {
	return s.middleware(next, want, false)
}

// MiddlewareOwedOK is Middleware for the short allow-list protocol/09 marks `F`: the routes an
// account that owes a second factor needs in order to add one (its own profile, the factor
// enrolments, logging a device out).
func (s *Sessions) MiddlewareOwedOK(next http.Handler, want Scope) http.Handler {
	return s.middleware(next, want, true)
}

func (s *Sessions) middleware(next http.Handler, want Scope, owedOK bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		sess, err := s.Resolve(r.Context(), bearer)
		if err != nil {
			server.WriteError(w, err)
			return
		}
		if sess.Bot != nil {
			allowed, routeErr := s.botRouteAllowed(r.Context(), r, *sess.Bot, sess)
			if routeErr != nil {
				if errors.Is(routeErr, botaccess.ErrIneligible) || errors.Is(routeErr, store.ErrNotFound) {
					allowed = false
				} else {
					server.WriteError(w, server.Unavailable(1000, "authorization state unavailable"))
					return
				}
			}
			if !allowed {
				server.WriteError(w, server.Errorf(server.CodeForbidden, "managed bot session is not allowed on this route or resource"))
				return
			}
			if r.Pattern != "POST /v1/sessions/negotiate" && sess.WireVersion < 3 {
				server.WriteError(w, server.Version([]uint64{3}, []uint64{1}, []uint64{1}))
				return
			}
		}
		if sess.Scope != want && sess.Scope != ScopeEnrolled {
			server.WriteError(w, scopeRefusal(sess.Scope))
			return
		}
		if want == ScopeEnrolled && sess.Scope != ScopeEnrolled {
			server.WriteError(w, scopeRefusal(sess.Scope))
			return
		}
		// `want != ScopePending`, not `want == ScopeEnrolled`: the "E or V" routes (want =
		// ScopeProvisional: POST /v1/keypackages and the welcomes) admit an enrolled session too,
		// and an owed one must not reach them. Only the `P` routes and the `F` allow-list do.
		// Every scope of an owed account is checked, not only an enrolled one: a provisional
		// session reaches the "E or V" routes as well (S1 review). A pending session never gets
		// past the scope checks above to a non-`P` route anyway.
		if sess.FactorOwed && want != ScopePending && !owedOK {
			server.WriteError(w, server.Errorf(server.CodeFactorRequired, "the account owes a second factor"))
			return
		}
		next.ServeHTTP(w, r.WithContext(WithSession(r.Context(), sess)))
	})
}

// botRouteAllowed is the explicit HTTP surface for bound bot sessions. Every
// resource path is resolved against the current repository before it is allowed.
func (s *Sessions) botRouteAllowed(ctx context.Context, r *http.Request, bot BotSessionBinding, session Session) (bool, error) {
	pattern := r.Pattern
	if pattern == "" {
		return false, nil
	}
	phase := bot.State
	joining := phase == store.BotInstallationJoining || phase == store.BotInstallationReady
	ready := phase == store.BotInstallationReady
	communityPath := func(name string) bool { return pathIDEquals(r, name, bot.CommunityID) }
	groupPath := func(name string) (bool, error) {
		allowed, err := communityPathForBot(ctx, s.repo, bot)
		if err != nil || !allowed {
			return false, err
		}
		groupID, ok := pathID(r, name)
		if !ok {
			return false, nil
		}
		group, err := s.repo.GetGroup(ctx, groupID)
		if errors.Is(err, store.ErrNotFound) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if group.CommunityID == nil || *group.CommunityID != bot.CommunityID || group.ClosedAt != nil {
			return false, nil
		}
		return group.Kind <= 1, nil
	}
	interactionGroupPath := func() (bool, error) {
		if !ready || !pathIDEquals(r, "installation", bot.InstallationID) {
			return false, nil
		}
		allowed, err := communityPathForBot(ctx, s.repo, bot)
		if err != nil || !allowed {
			return false, err
		}
		groupID, ok := pathID(r, "group")
		if !ok {
			return false, nil
		}
		group, err := s.repo.GetGroup(ctx, groupID)
		if errors.Is(err, store.ErrNotFound) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		return group.CommunityID != nil && *group.CommunityID == bot.CommunityID && group.Kind == 3 && group.ClosedAt == nil, nil
	}
	interactionSendGroupPath := func() (bool, error) {
		if !ready {
			return false, nil
		}
		allowed, err := communityPathForBot(ctx, s.repo, bot)
		if err != nil || !allowed {
			return false, err
		}
		groupID, ok := pathID(r, "group")
		if !ok {
			return false, nil
		}
		group, err := s.repo.GetGroup(ctx, groupID)
		if errors.Is(err, store.ErrNotFound) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if group.CommunityID == nil || *group.CommunityID != bot.CommunityID || group.ClosedAt != nil {
			return false, nil
		}
		if group.Kind <= 1 {
			return true, nil
		}
		if group.Kind != 3 {
			return false, nil
		}
		receipt, err := s.repo.GetBotGroupRegistrationReceiptByGroup(ctx, groupID)
		if errors.Is(err, store.ErrNotFound) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		return receipt.InstallationID == bot.InstallationID && receipt.UserID == session.UserID &&
			receipt.DeviceID == session.DeviceID && receipt.CommunityID == bot.CommunityID && receipt.GroupID == groupID, nil
	}
	channelPath := func(name string) (bool, error) {
		allowed, err := communityPathForBot(ctx, s.repo, bot)
		if err != nil || !allowed {
			return false, err
		}
		channelID, ok := pathID(r, name)
		if !ok {
			return false, nil
		}
		channel, err := s.repo.GetChannel(ctx, channelID)
		if errors.Is(err, store.ErrNotFound) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		return channel.CommunityID != nil && *channel.CommunityID == bot.CommunityID && channel.DeletedAt == nil, nil
	}
	callPath := func() (bool, error) {
		allowed, err := communityPathForBot(ctx, s.repo, bot)
		if err != nil || !allowed {
			return false, err
		}
		callID, ok := pathID(r, "call_id")
		if !ok {
			return false, nil
		}
		call, err := s.repo.GetVoiceSession(ctx, callID)
		if errors.Is(err, store.ErrNotFound) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if call.Ended != nil {
			return false, nil
		}
		channel, err := s.repo.GetChannel(ctx, call.ChannelID)
		if errors.Is(err, store.ErrNotFound) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		return channel.DeletedAt == nil && channel.CommunityID != nil && *channel.CommunityID == bot.CommunityID, nil
	}
	switch pattern {
	case "POST /v1/sessions/negotiate":
		return phase == store.BotInstallationEnrolling || phase == store.BotInstallationJoining || phase == store.BotInstallationReady, nil
	case "DELETE /v1/devices/{device_id}/sessions":
		return (phase == store.BotInstallationEnrolling || phase == store.BotInstallationJoining || phase == store.BotInstallationReady) && pathIDEquals(r, "device_id", session.DeviceID), nil
	case "PUT /v1/bot-runtime/installations/{installation}/credential":
		return (phase == store.BotInstallationEnrolling || phase == store.BotInstallationJoining || phase == store.BotInstallationReady) && pathIDEquals(r, "installation", bot.InstallationID), nil
	case "POST /v1/bot-runtime/installations/{installation}/ready":
		return (phase == store.BotInstallationJoining || phase == store.BotInstallationReady) && pathIDEquals(r, "installation", bot.InstallationID), nil
	case "GET /v1/bot-runtime/installations/{installation}/interaction-groups/{group}/info",
		"GET /v1/bot-runtime/installations/{installation}/interaction-groups/{group}/handshakes",
		"GET /v1/bot-runtime/installations/{installation}/interaction-groups/{group}/messages",
		"POST /v1/bot-runtime/installations/{installation}/interaction-groups/{group}/cursor",
		"GET /v1/bot-runtime/installations/{installation}/interaction-groups/{group}/heal",
		"POST /v1/bot-runtime/installations/{installation}/interaction-groups/{group}/heal":
		return interactionGroupPath()
	case "POST /v1/bot-runtime/installations/{installation}/moderation/actions":
		return ready && pathIDEquals(r, "installation", bot.InstallationID), nil
	case "GET /v1/bot-runtime/installations/{installation}/moderation/removals/{request}",
		"GET /v1/bot-runtime/installations/{installation}/moderation/warnings/{request}":
		return ready && pathIDEquals(r, "installation", bot.InstallationID), nil
	case "POST /v1/bot-runtime/installations/{installation}/automod-grants/redeem":
		return ready && pathIDEquals(r, "installation", bot.InstallationID), nil
	case "GET /v1/communities/{community}/bot/actors/{actor}/authority":
		if !ready || !communityPath("community") {
			return false, nil
		}
		actorID, ok := pathID(r, "actor")
		return ok && !actorID.IsZero(), nil
	case "GET /v1/communities/{community}/bot/actors/{actor}/channels/{channel}/authority":
		if !ready || !communityPath("community") {
			return false, nil
		}
		actorID, ok := pathID(r, "actor")
		if !ok || actorID.IsZero() {
			return false, nil
		}
		return channelPath("channel")
	case "POST /v1/communities/{community}/bot/join-role-grants":
		return ready && communityPath("community"), nil
	case "GET /v1/communities/{community}/bot/members/{member}/episode":
		if !ready || !communityPath("community") {
			return false, nil
		}
		memberID, ok := pathID(r, "member")
		return ok && !memberID.IsZero(), nil
	case "POST /v1/bot-runtime/installations/{installation}/keypackages",
		"GET /v1/bot-runtime/installations/{installation}/keypackages",
		"GET /v1/bot-runtime/installations/{installation}/keypackages/{operation}":
		return (phase == store.BotInstallationJoining || phase == store.BotInstallationReady) && pathIDEquals(r, "installation", bot.InstallationID), nil
	case "POST /v1/bot-runtime/installations/{installation}/groups",
		"GET /v1/bot-runtime/installations/{installation}/groups/{operation}":
		// The handler turns a foreign installation path into the protocol's indistinguishable 404.
		// Letting the session reach it reveals no receipt and keeps the cross-installation result
		// independent of middleware's otherwise-global 403 refusal.
		return phase == store.BotInstallationJoining || phase == store.BotInstallationReady, nil
	case "POST /v1/bot-runtime/installations/{installation}/interaction-admissions",
		"POST /v1/bot-runtime/installations/{installation}/interaction-admissions/context":
		return joining && pathIDEquals(r, "installation", bot.InstallationID), nil
	case "GET /v1/bot-runtime/installations/{installation}/interaction-admissions/{request}/group-absence":
		return ready && pathIDEquals(r, "installation", bot.InstallationID), nil
	case "PUT /v1/users/{user_id}/device-list":
		return joining && pathIDEquals(r, "user_id", session.UserID), nil
	case "GET /v1/users/{user_id}/device-list":
		if !joining {
			return false, nil
		}
		userID, ok := pathID(r, "user_id")
		if !ok {
			return false, nil
		}
		err := botaccess.EligibleCommunityMember(ctx, s.repo, bot.CommunityID, userID)
		if errors.Is(err, botaccess.ErrIneligible) || errors.Is(err, store.ErrNotFound) {
			return false, nil
		}
		return err == nil, err
	case "GET /v1/devices/{device_id}/keypackage":
		if !joining {
			return false, nil
		}
		deviceID, ok := pathID(r, "device_id")
		if !ok {
			return false, nil
		}
		device, err := s.repo.GetDevice(ctx, deviceID)
		if errors.Is(err, store.ErrNotFound) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if device.RevokedAt != nil || device.QuarantinedAt != nil {
			return false, nil
		}
		err = botaccess.EligibleCommunityMember(ctx, s.repo, bot.CommunityID, device.UserID)
		if errors.Is(err, botaccess.ErrIneligible) || errors.Is(err, store.ErrNotFound) {
			return false, nil
		}
		return err == nil, err
	case "GET /v1/welcomes", "DELETE /v1/welcomes/{welcome_id}":
		// These resources are device-scoped by the delivery service. In particular,
		// KeyPackage publication is for session.DeviceID and Welcome acknowledgement
		// checks that the welcome belongs to that same device.
		if !joining {
			return false, nil
		}
		return communityPathForBot(ctx, s.repo, bot)
	case "POST /v1/groups":
		// The typed binding is in the request body; StructureChannels.MayRegister
		// validates its target and current community eligibility inside the DS.
		if !joining && !ready {
			return false, nil
		}
		return communityPathForBot(ctx, s.repo, bot)
	case "GET /v1/groups/{id}/info", "GET /v1/groups/{id}/tree", "GET /v1/groups/{id}/handshakes",
		"GET /v1/groups/{id}/proposals", "POST /v1/groups/{id}/commit", "POST /v1/groups/{id}/proposal",
		"POST /v1/groups/{id}/resync", "POST /v1/groups/{id}/fork-report",
		"GET /v1/groups/{id}/messages", "POST /v1/groups/{id}/cursor",
		"GET /v1/groups/{id}/heal", "POST /v1/groups/{id}/heal":
		if !joining {
			return false, nil
		}
		return groupPath("id")
	case "POST /v1/groups/{id}/message":
		if !ready {
			return false, nil
		}
		return groupPath("id")
	case "DELETE /v1/groups/{id}/messages/{seq}":
		if !ready {
			return false, nil
		}
		return groupPath("id")
	case "GET /v1/communities/{id}", "GET /v1/communities/{id}/members",
		"GET /v1/communities/{id}/permissions", "GET /v1/communities/{id}/channels":
		if !joining || !communityPath("id") {
			return false, nil
		}
		return communityPathForBot(ctx, s.repo, bot)
	case "GET /v1/channels/{id}", "GET /v1/channels/{id}/members", "GET /v1/channels/{id}/messages",
		"GET /v1/channels/{id}/permissions":
		if !joining {
			return false, nil
		}
		return channelPath("id")
	case "POST /v1/channels/{id}/messages":
		if !ready {
			return false, nil
		}
		return channelPath("id")
	case "POST /v1/channels/{id}/calls", "GET /v1/channels/{id}/call":
		if !ready {
			return false, nil
		}
		return channelPath("id")
	case "GET /v1/communities/{id}/calls":
		if !ready || !communityPath("id") {
			return false, nil
		}
		return communityPathForBot(ctx, s.repo, bot)
	case "DELETE /v1/calls/{call_id}", "POST /v1/calls/{call_id}/share",
		"DELETE /v1/calls/{call_id}/share", "POST /v1/calls/{call_id}/decline", "POST /v1/calls/{call_id}/rtt":
		if !ready {
			return false, nil
		}
		return callPath()
	case "POST /v1/gateway/ticket":
		if !joining {
			return false, nil
		}
		return communityPathForBot(ctx, s.repo, bot)
	case "POST /v1/groups/{group}/bot/messages":
		return interactionSendGroupPath()
	case "GET /v1/communities/{community}/bot/events", "PUT /v1/communities/{community}/bot/events/ack",
		"GET /v1/communities/{community}/bot/receipts/{operation}",
		"POST /v1/communities/{community}/bot/receipts/{operation}/resolve",
		"POST /v1/groups/{group}/moderation/removals",
		"GET /v1/groups/{group}/moderation/removals", "GET /v1/communities/{community}/moderation/actions":
		if !ready {
			return false, nil
		}
		allowed, err := communityPathForBot(ctx, s.repo, bot)
		if err != nil || !allowed {
			return false, err
		}
		if strings.Contains(pattern, "/groups/{group}/") {
			return groupPath("group")
		}
		return communityPath("community"), nil
	default:
		return false, nil
	}
}

func communityPathForBot(ctx context.Context, repo store.Repository, bot BotSessionBinding) (bool, error) {
	userID, err := mustBotUserID(ctx, repo, bot)
	if err != nil {
		if errors.Is(err, botaccess.ErrIneligible) || errors.Is(err, store.ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	binding, isBot, err := botaccess.ForUserCommunity(ctx, repo, userID, bot.CommunityID)
	if errors.Is(err, botaccess.ErrIneligible) || errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return isBot && binding.InstallationID == bot.InstallationID && binding.State == bot.State, nil
}

func mustBotUserID(ctx context.Context, repo store.Repository, bot BotSessionBinding) (id.ID, error) {
	// Resolve the installation on the indexed community key; no global scan.
	row, err := repo.GetActiveBotInstallationByCommunity(ctx, bot.CommunityID)
	if err != nil {
		return id.ID{}, err
	}
	if row.UserID == nil || row.ID != bot.InstallationID {
		return id.ID{}, botaccess.ErrIneligible
	}
	return *row.UserID, nil
}

func pathID(r *http.Request, name string) (id.ID, bool) {
	value := r.PathValue(name)
	if value == "" {
		return id.ID{}, false
	}
	got, err := id.Parse(value)
	return got, err == nil
}

func pathIDEquals(r *http.Request, name string, want id.ID) bool {
	value := r.PathValue(name)
	if value == "" {
		return false
	}
	got, err := id.Parse(value)
	return err == nil && got == want
}

// FromContext returns the session a Middleware put there. Parts 1b and 2 read
// the session through this, not through a server.SessionOf: internal/auth
// imports internal/server, so the reverse would be an import cycle (ID14).
func FromContext(ctx context.Context) (Session, bool) {
	s, ok := ctx.Value(ctxKey{}).(Session)
	return s, ok
}

// SessionFrom is FromContext under the name internal/api uses (interfaces.md
// §6.3). One of the two spellings had to win; both are kept so neither plan has
// to rewrite every handler.
func SessionFrom(ctx context.Context) (Session, bool) { return FromContext(ctx) }

// WithSession puts a session in a context. It is the exported setter a handler
// test needs to exercise a route without minting a bearer token first.
func WithSession(ctx context.Context, s Session) context.Context {
	return context.WithValue(ctx, ctxKey{}, s)
}

// scopeRefusal names the code protocol/02 § Device sessions item 4 fixes for a
// provisional session outside its pairing surface, and E_FORBIDDEN otherwise. A
// document that names a code the instance never emits is worse than no document.
func scopeRefusal(got Scope) *server.Error {
	if got == ScopeProvisional {
		return server.Errorf(server.CodeProvisionalOutsidePairing,
			"a provisional session reaches only its pairing group")
	}
	return server.Errorf(server.CodeForbidden, "session scope %d cannot reach this endpoint", got)
}
