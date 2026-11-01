// Package store is dillad's one persistence contract: a hand-written repository
// over two generated query sets. Nothing above this package ever sees a *sql.DB,
// because sqlc's two DBTX interfaces do not unify (facts-storage §1.4).
package store

import (
	"context"
	"errors"

	"github.com/jonasthim/dilla/internal/bot/protocol"
	"github.com/jonasthim/dilla/internal/id"
)

var (
	ErrNotFound  = errors.New("store: not found")
	ErrConflict  = errors.New("store: conflict")  // unique violation, optimistic-lock miss
	ErrExhausted = errors.New("store: exhausted") // invite max_uses, quota
)

// Repository is what dillad persists through. Tx runs fn in one transaction and
// the Repository fn receives IS that transaction; nesting is an error. Close
// releases both pools.
//
// The embed list grows as the schema does, one named step per task (ID1):
// task 19 step 1a adds MLS with 00002_mls.sql, task 23 step 1a adds Messages and
// Cursors with 00003_messages.sql, Plan 2 task 1 step 9 adds Communities (the
// part of Structure whose tables 00004_structure.sql ships), Plan 2 task 2 adds
// Channels (the part 00005_channels.sql ships), Plan 2 task 3 adds Overwrites (the
// part 00006_overwrites.sql ships), Plan 2 task 4 adds Bans (the part
// 00007_bans.sql ships), Plan 2 task 6 adds ChannelMembers (the part
// 00008_channel_members.sql ships), Plan 2 task 8 adds Readable with
// 00009_readable.sql, Plan 2 task 10 adds Blobs and OpsBackups with
// 00010_blobs.sql, and Plan 2's later tasks add the rest of Structure.
type Repository interface {
	Tx(ctx context.Context, fn func(Repository) error) error
	Close() error
	Instance
	Accounts
	Devices
	Sessions
	Auth
	Invites
	Ops
	MLS         // task 19 step 1a — 00002_mls.sql
	Messages    // added HERE — ID1; its table is 00003_messages.sql, written in step 1
	Cursors     // added HERE — ID1; device_cursors ships with 00002_mls.sql, its queries here
	Communities // Plan 2 task 1 step 9 (P2-D23) — 00004_structure.sql
	Channels    // Plan 2 task 2 (P2-D23) — 00005_channels.sql
	Overwrites  // Plan 2 task 3 (P2-D23) — 00006_overwrites.sql
	Bans        // Plan 2 task 4 (P2-D23) — 00007_bans.sql
	MemberTimeouts
	ManualWarningActions
	// Plan 2 task 6 (P2-D23) — 00008_channel_members.sql
	ChannelMembers
	// Plan 2 task 8 (P2-D23) — 00009_readable.sql
	Readable
	// Plan 2 task 10 (P2-D23, P2-D5) — 00010_blobs.sql
	Blobs
	OpsBackups
	// Plan 2 task 16 (P2-D22) — 00011_voice.sql
	VoiceSessions
	RolePanels
	ReactionRoleGrantSources
	BotJoinRoleGrants
	BotRuntimes
	BotInstallations
	BotClaims
	BotOperations
	BotSessionBindings
	BotKeyPackagePublications
	BotGroupRegistrationReceipts
	BotSendOperations
	BotStructuralEvents
	BotInteractionEligibility
	BotInteractionAdmissionReceipts
	BotGroupRegistrationAdmissionReceipts
	BotAutoModerationGrants
	ModerationRemovals
}

// BotAutoModerationGrantRow stores only a digest of the opaque bearer grant and a
// digest of the encrypted op63 document. Plaintext moderation config never enters storage.
type BotAutoModerationGrantRow struct {
	TokenHash, RequestHash, ConfigHash                    []byte
	CommunityID, InstallationID, GroupID, RequestID       id.ID
	ActorUser, ActorDevice                                id.ID
	ExpectedRevision, RequestExpires, Expires, ConsumedAt int64
	Receipt                                               []byte
}

type BotAutoModerationGrants interface {
	CreateBotAutoModerationGrant(context.Context, BotAutoModerationGrantRow) error
	GetBotAutoModerationGrant(context.Context, []byte) (BotAutoModerationGrantRow, error)
	// Accept is atomic: one grant accepts one exact installation/request/ciphertext tuple.
	// Exact retries return the original receipt; any changed tuple conflicts.
	AcceptBotAutoModerationGrant(context.Context, BotAutoModerationGrantRow) (BotAutoModerationGrantRow, bool, error)
}

// BotInteractionEligibility exposes the monotonic eligibility fence for one
// user in one community. Reads happen in a transaction and lock the row on
// PostgreSQL so admission issuance and group registration can share a fence.
// Database triggers advance it whenever membership, bans, or device state
// changes. This primitive does not itself authorize interaction admission.
type BotInteractionEligibility interface {
	GetBotInteractionEligibilityRevision(context.Context, id.ID, id.ID) (uint64, error)
}

type BotInteractionAdmissionReceipt struct {
	InstallationID id.ID
	RequestID      id.ID
	RequestHash    []byte
	Payload        []byte
	Signature      []byte
	NotAfter       int64
}

type BotInteractionAdmissionReceipts interface {
	GetBotInteractionAdmissionReceipt(context.Context, id.ID, id.ID) (BotInteractionAdmissionReceipt, error)
	CreateBotInteractionAdmissionReceipt(context.Context, BotInteractionAdmissionReceipt) error
	UpdateBotInteractionAdmissionReceipt(context.Context, BotInteractionAdmissionReceipt) error
	// Compaction removes expired payloads immediately and forgets request hashes after
	// 30 days past expiry. IDs are immutable during that window; after it, reuse is allowed.
	// One call forgets at most BotInteractionAdmissionTombstoneBatch hashes, oldest first, so a
	// backlog (after an upgrade or a long Bot outage) drains over later admissions instead of
	// holding the single writer; a hash may therefore outlive its 30 days, never fall short of them.
	CompactExpiredBotInteractionAdmissionReceipts(context.Context, id.ID, int64) error
}

// BotInteractionAdmissionTombstoneBatch bounds the tombstones one compaction deletes.
const BotInteractionAdmissionTombstoneBatch = 512

// BotGroupRegistrationAdmissionReceipts atomically records the accepted
// registration consumed by one kind-3 admission request. The bool is true for
// an exact retry; reusing a request ID for different registration data conflicts.
type BotGroupRegistrationAdmissionReceipts interface {
	AcceptBotGroupRegistrationAdmission(context.Context, id.ID, BotGroupRegistrationReceipt) (BotGroupRegistrationReceipt, bool, error)
	GetBotGroupRegistrationAdmission(context.Context, id.ID, id.ID) (BotGroupRegistrationReceipt, error)
}

// BotJoinRolePolicy stores the single human-approved role a ready bot may
// assign when a member joins. It only configures policy; assignment is a
// separate capability and must enforce membership episode binding.
type BotJoinRolePolicy interface {
	SetBotJoinRolePolicy(ctx context.Context, installationID, roleID, approverID id.ID, approvedAt int64) error
	GetBotJoinRolePolicy(ctx context.Context, installationID id.ID) (BotJoinRolePolicyRow, error)
}

// BotJoinRoleGrants binds one bot-triggered role assignment to a stable
// membership episode. The receipt is immutable for a request and episode.
type BotJoinRoleGrants interface {
	GrantBotJoinRoleForEpisode(context.Context, BotJoinRoleGrantRequest) (BotJoinRoleGrantReceipt, bool, error)
}

type BotJoinRoleGrantRequest struct {
	RequestID      id.ID
	InstallationID id.ID
	CommunityID    id.ID
	UserID         id.ID
	EpisodeID      id.ID
}

type BotJoinRoleGrantReceipt struct {
	RequestID      id.ID
	InstallationID id.ID
	CommunityID    id.ID
	UserID         id.ID
	EpisodeID      id.ID
	RoleID         id.ID
}

type BotJoinRolePolicyRow struct {
	InstallationID id.ID
	CommunityID    id.ID
	RoleID         id.ID
	ApprovedBy     id.ID
	ApprovedAt     int64
}

// ModerationRemovalRow is a permanent signed deletion marker. The feed retains
// the canonical unsigned proof and detached signature independently of message
// and structural-event retention.
type ModerationRemovalRow struct {
	RequestID      id.ID
	ActionID       id.ID
	CommunityID    id.ID
	GroupID        id.ID
	TargetSequence uint64
	Revision       uint64
	Proof          []byte
	Signature      []byte
	AcceptedAt     int64
}

type ModerationRemovalPage struct {
	Rows         []ModerationRemovalRow
	NextRevision uint64
}

// ModerationRemovals requires Accept to run inside the same transaction as
// the action event. It allocates a durable per-group revision and tombstones
// the target application message before returning.
type ModerationRemovals interface {
	AcceptModerationRemoval(context.Context, ModerationRemovalRow) (ModerationRemovalRow, error)
	GetModerationRemoval(context.Context, id.ID, id.ID) (ModerationRemovalRow, error)
	GetModerationRemovalByRequest(context.Context, id.ID) (ModerationRemovalRow, error)
	ListModerationRemovals(context.Context, id.ID, uint64, int32) (ModerationRemovalPage, error)
}

// BotStructuralEventPage is a bounded snapshot of the community event stream.
// Floor is the greatest sequence removed by retention; a cursor below it has a gap.
type BotStructuralEventPage struct {
	Events       []protocol.Event
	NextSequence uint64
	HighWater    uint64
	Floor        uint64
}

// BotStructuralEvents stores canonical structural events and per-installation
// acknowledgements. Append, prune, and ack require the caller's transaction.
type BotStructuralEvents interface {
	AppendBotStructuralEvent(context.Context, protocol.Event) (protocol.Event, error)
	ListBotStructuralEvents(context.Context, id.ID, uint64, int32) (BotStructuralEventPage, error)
	PruneBotStructuralEvents(context.Context, int64) (int64, error)
	AckBotStructuralEvents(context.Context, id.ID, id.ID, uint64) error
	GetBotStructuralEventAck(context.Context, id.ID, id.ID) (uint64, error)
}

type Instance interface {
	GetInstance(ctx context.Context) (InstanceRow, error)
	// LockInstanceGeneration serializes publishers with restore while their transaction is open.
	LockInstanceGeneration(ctx context.Context) error
	CreateInstance(ctx context.Context, in InstanceRow) error
	BumpGeneration(ctx context.Context) (uint64, error)
	// SetGeneration is invariant 11's restore half (deviation B13). `dillad
	// restore` reads the generation out of the backup's manifest and names it,
	// rather than blind-bumping, so an operator who restores twice from the same
	// manifest lands on the same number both times. It is MONOTONE: a manifest
	// older than the instance's current generation cannot walk the number
	// backwards, because the generation is exactly what invalidates outstanding
	// resume tokens and a backwards step would revive the ones the last restore
	// killed.
	SetGeneration(ctx context.Context, generation uint64) error
	GetSetting(ctx context.Context, key string) ([]byte, error)
	// PutSetting carries its own timestamp: instance_settings.updated is
	// NOT NULL, and a repository with no clock has nothing to write there
	// (deviation ID11).
	PutSetting(ctx context.Context, key string, value []byte, updated int64) error
}

// KeyPackagePublicationReceipt is the compact accepted-operation record for a managed Bot. It
// deliberately stores no upload or package bytes.
type KeyPackagePublicationReceipt struct {
	InstallationID id.ID
	UserID         id.ID
	DeviceID       id.ID
	OperationID    id.ID
	Generation     uint64
	UploadSHA256   []byte
	Count          uint64
	AcceptedAt     int64
}

// BotKeyPackagePublications provides transaction-scoped serialization and compact receipts.
type BotKeyPackagePublications interface {
	LockKeyPackageDevice(context.Context, id.ID) error
	GetBotKeyPackagePublicationReceipt(context.Context, id.ID, id.ID) (KeyPackagePublicationReceipt, error)
	CreateBotKeyPackagePublicationReceipt(context.Context, KeyPackagePublicationReceipt) error
	DeleteBotKeyPackagePublicationReceipts(context.Context, id.ID) error
	GetKeyPackage(context.Context, id.ID, []byte) (KeyPackageRow, error)
}

// BotGroupRegistrationReceipts stores immutable, installation-scoped acceptance
// proofs. Creation requires the caller's transaction so DS can commit the proof
// atomically with the group and initial MLS state.
type BotGroupRegistrationReceipts interface {
	GetBotGroupRegistrationReceipt(context.Context, id.ID, id.ID) (BotGroupRegistrationReceipt, error)
	// GetBotGroupRegistrationReceiptByGroup resolves the immutable accepted registration for a
	// globally unique MLS group ID. It is a read-only lookup for authorization against an existing
	// group; callers must still validate the live group state and installation authority.
	GetBotGroupRegistrationReceiptByGroup(context.Context, id.ID) (BotGroupRegistrationReceipt, error)
	CreateBotGroupRegistrationReceipt(context.Context, BotGroupRegistrationReceipt) error
	DeleteBotGroupRegistrationReceiptsByInstallation(context.Context, id.ID) error
}

// BotSendOperationRow is the stable, installation-scoped side effect. Attempt
// IDs and pointers are opaque identifiers; state is BotSendOperationState.
type BotSendOperationRow struct {
	InstallationID  id.ID
	OperationID     id.ID
	UserID          id.ID
	DeviceID        id.ID
	CommunityID     id.ID
	GroupID         id.ID
	CurrentAttempt  id.ID
	AcceptedAttempt *id.ID
	State           uint8
}

// BotSendAttemptRow stores only metadata and the canonical receipt. It never
// contains plaintext or provider data. Outcome is BotSendAttemptOutcome.
type BotSendAttemptRow struct {
	InstallationID    id.ID
	OperationID       id.ID
	AttemptID         id.ID
	PredecessorID     *id.ID
	Generation        uint64
	Epoch             uint64
	Class             uint8
	CiphertextSHA256  []byte
	FingerprintSHA256 []byte
	Outcome           uint8
	Receipt           []byte
}

type BotSendOperationState uint8

const (
	BotSendOperationActive BotSendOperationState = iota
	BotSendOperationAccepted
	BotSendOperationUncertain
)

type BotSendAttemptOutcome uint8

const (
	BotSendAttemptClosed BotSendAttemptOutcome = iota
	BotSendAttemptAccepted
	BotSendAttemptUnknown
)

// BotSendOperations is a narrow transaction-scoped persistence API for durable
// send attempts. It exposes immutable inserts and guarded pointer transitions,
// not arbitrary row updates.
type BotSendOperations interface {
	CreateBotSendOperationIfAbsent(context.Context, BotSendOperationRow) (BotSendOperationRow, bool, error)
	LockBotSendOperation(context.Context, id.ID, id.ID) (BotSendOperationRow, error)
	GetBotSendAttempt(context.Context, id.ID, id.ID, id.ID) (BotSendAttemptRow, error)
	InsertBotSendAttempt(context.Context, BotSendAttemptRow) error
	AdvanceBotSendAttempt(context.Context, id.ID, id.ID, id.ID, id.ID) error
	SealBotSendOperation(context.Context, id.ID, id.ID, id.ID, BotSendOperationState) error
	DeleteBotSendsByInstallation(context.Context, id.ID) error
}

type Accounts interface {
	// PresenceChoice is the account-wide visibility/alert preference (1 online by default).
	GetPresenceChoice(ctx context.Context, userID id.ID) (uint8, error)
	ListPresenceChoices(ctx context.Context, users []id.ID) (map[id.ID]uint8, error)
	SetPresenceChoice(ctx context.Context, userID id.ID, choice uint8) error
	CreateUser(ctx context.Context, u UserRow) error
	GetUser(ctx context.Context, userID id.ID) (UserRow, error)
	GetUserByUsername(ctx context.Context, username string) (UserRow, error)
	ListUsers(ctx context.Context, after id.ID, limit int32) ([]UserRow, error)
	SetUserDisabled(ctx context.Context, userID id.ID, at *int64) error
	TombstoneUser(ctx context.Context, userID id.ID, at int64) error // keeps username (R36)
	// UpdateProfile writes the display name, status message and bio in one statement (EOD E6); the
	// caller has validated them. A nil field is left as it is (COALESCE in SQL, so two writers that
	// change different fields never overwrite each other); "" clears one. ErrNotFound for an unknown
	// or tombstoned user.
	UpdateProfile(ctx context.Context, userID id.ID, display, statusMsg, bio *string) error
	// SetFactorOwed writes users.factor_owed (web3.6 S1); ErrNotFound for an unknown user.
	SetFactorOwed(ctx context.Context, userID id.ID, owed bool) error
	// MarkFactorOwedWithoutFactor sets factor_owed on every live human account that holds no
	// confirmed TOTP secret and no passkey registered for rpID, and returns how many it set (the
	// boot reconcile's first half).
	MarkFactorOwedWithoutFactor(ctx context.Context, rpID string) (int64, error)
	// ClearFactorOwedWithFactor clears factor_owed on every live human account that holds a
	// confirmed TOTP secret or a passkey registered for rpID, and returns how many it cleared (the
	// boot reconcile's second half, web3.6 review I2): owed holds exactly when no usable factor is
	// held. A concurrent operator reset deletes the factors in the transaction that sets the flag
	// and under the user's row lock, so it never leaves a row this clears.
	ClearFactorOwedWithFactor(ctx context.Context, rpID string) (int64, error)
}

type Devices interface {
	CreateDevice(ctx context.Context, d DeviceRow) error
	// CreateBotDevice rejects any previously registered DSK and must run inside Tx.
	// CreateDevice uses the same DSK lock and rejects reuse whenever either account is a Bot.
	CreateBotDevice(ctx context.Context, d DeviceRow) error
	GetDevice(ctx context.Context, deviceID id.ID) (DeviceRow, error)
	// UpdateDeviceCredential replaces only the validated final enrollment credential.
	UpdateDeviceCredential(ctx context.Context, deviceID id.ID, credential []byte) error
	// FinalizeBotDeviceCredential stores the validated final credential and stamps the Bot
	// native device as verified at the same server time used by credential finalization.
	FinalizeBotDeviceCredential(ctx context.Context, deviceID id.ID, credential []byte, verifiedAt int64) error
	ListDevicesByUser(ctx context.Context, userID id.ID) ([]DeviceRow, error)
	TouchDevice(ctx context.Context, deviceID id.ID, lastSeen int64) error
	// SetDeviceLabel names a device of userID (nil clears it); ErrNotFound unless the device is the user's.
	SetDeviceLabel(ctx context.Context, userID, deviceID id.ID, label *string) error
	RevokeDevice(ctx context.Context, deviceID id.ID, at int64) error
	PutDeviceList(ctx context.Context, l DeviceListRow) error
	GetDeviceList(ctx context.Context, userID id.ID) (DeviceListRow, error)
	// ListDeviceListsAfter is the history read of GET /v1/users/{id}/device-list?after= (L-HTTP-56).
	ListDeviceListsAfter(ctx context.Context, userID id.ID, after uint64, limit int32) ([]DeviceListRow, error)
	// CountLiveDevicesByUser counts the user's unrevoked devices (the per-user cap, Q04).
	CountLiveDevicesByUser(ctx context.Context, userID id.ID) (int64, error)
	// ListDeviceCreationsSince lists the creation times of the user's devices since a cutoff, revoked ones included (the enrolment rate, Q04).
	ListDeviceCreationsSince(ctx context.Context, userID id.ID, since int64) ([]int64, error)
	// LockUserForDeviceRegistration serializes a user's count and insert within Tx.
	LockUserForDeviceRegistration(ctx context.Context, userID id.ID) error
	// CountLiveUnlistedDevicesByUser excludes listed, revoked, and expired unlisted rows.
	CountLiveUnlistedDevicesByUser(ctx context.Context, userID id.ID, listed []id.ID, unlistedCutoff int64) (int64, error)
	// ListLiveDeviceCreationsSince includes listed live rows and unexpired unlisted live rows.
	ListLiveDeviceCreationsSince(ctx context.Context, userID id.ID, listed []id.ID, since, unlistedCutoff int64) ([]int64, error)
}

type Sessions interface {
	CreateSession(ctx context.Context, s SessionRow) error
	GetSessionByHash(ctx context.Context, tokenHash []byte, now int64) (SessionRow, error)
	SetSessionVersions(ctx context.Context, tokenHash []byte, wire, e2ee, media uint64) error
	TouchSession(ctx context.Context, tokenHash []byte, idleExpires int64) error
	DeleteSession(ctx context.Context, tokenHash []byte) error
	DeleteSessionsByDevice(ctx context.Context, deviceID id.ID) (int64, error)
	DeleteSessionsByUser(ctx context.Context, userID id.ID) (int64, error)
	CountSessionsByDevice(ctx context.Context, deviceID id.ID) (int64, error)
	DeleteOldestSessionForDevice(ctx context.Context, deviceID id.ID) error
	PruneSessions(ctx context.Context, now int64) (int64, error)
}

type Auth interface {
	PutPasswordCredential(ctx context.Context, userID id.ID, phc string, updated int64) error
	GetPasswordCredential(ctx context.Context, userID id.ID) (string, error)
	// ChangePasswordCredential stores a changed password and records when it changed (Task 11, Q7), in one statement, so the
	// hash is never written without its change time (fix round 2, security L1).
	ChangePasswordCredential(ctx context.Context, userID id.ID, phc string, at int64) error
	// GetPasswordChangedAt is that time, or nil when it is unknown (set before migration 00052, or never changed since sign-up);
	// ErrNotFound when the account has no password.
	GetPasswordChangedAt(ctx context.Context, userID id.ID) (*int64, error)
	PutTOTP(ctx context.Context, t TOTPRow) error
	GetTOTP(ctx context.Context, userID id.ID) (TOTPRow, error)
	// DeleteTOTP drops the user's TOTP secret, confirmed or not; no row is not an error.
	DeleteTOTP(ctx context.Context, userID id.ID) error
	ConsumeTOTPCounter(ctx context.Context, userID id.ID, counter int64) error
	PutRecoveryCodes(ctx context.Context, userID id.ID, hashes [][]byte, created int64) error
	// DeleteRecoveryCodes drops every recovery code of the user, used or not.
	DeleteRecoveryCodes(ctx context.Context, userID id.ID) error
	ConsumeRecoveryCode(ctx context.Context, userID id.ID, hash []byte, at int64) error
	CountRecoveryCodes(ctx context.Context, userID id.ID) (int64, error)
	// PutWebauthnUser is INSERT ... ON CONFLICT (rp_id, user_id) DO NOTHING: a
	// handle is minted once and never rotated, because the authenticator keeps
	// the handle it saw at registration and a rotation makes discoverable login
	// impossible for that account. GetWebauthnUserHandle is the read that makes
	// minting idempotent (deviation ID11).
	PutWebauthnUser(ctx context.Context, userID id.ID, rpID string, handle []byte, created int64) error
	GetWebauthnUserHandle(ctx context.Context, rpID string, userID id.ID) ([]byte, error)
	GetWebauthnUserByHandle(ctx context.Context, rpID string, handle []byte) (id.ID, error)
	PutWebauthnCredential(ctx context.Context, c WebauthnCredentialRow) error
	ListWebauthnCredentials(ctx context.Context, userID id.ID, rpID string) ([]WebauthnCredentialRow, error)
	GetWebauthnCredential(ctx context.Context, credID []byte) (WebauthnCredentialRow, error)
	UpdateWebauthnCredential(ctx context.Context, credID []byte, signCount int64, flags []byte, lastUsed int64) error
	// DeleteWebauthnCredential deletes one credential, only when userID holds it, and returns how
	// many rows went (0 or 1): another account's credential id deletes nothing.
	DeleteWebauthnCredential(ctx context.Context, userID id.ID, credID []byte) (int64, error)
	// DeleteWebauthnCredentialsByUser deletes every credential of the user, under every rp_id.
	DeleteWebauthnCredentialsByUser(ctx context.Context, userID id.ID) error
	PutCeremony(ctx context.Context, c CeremonyRow) error
	TakeCeremony(ctx context.Context, ceremonyID id.ID, now int64) (CeremonyRow, error) // single use
	PruneCeremonies(ctx context.Context, now int64) (int64, error)
	PutOIDCIdentity(ctx context.Context, issuer, subject string, userID id.ID, created int64) error
	GetOIDCIdentity(ctx context.Context, issuer, subject string) (id.ID, error)
	RecordLoginAttempt(ctx context.Context, a LoginAttemptRow) error
	CountLoginFailures(ctx context.Context, userID id.ID, since int64) (int64, error)
	ClearLoginFailures(ctx context.Context, userID id.ID) error
}

type Invites interface {
	CreateInvite(ctx context.Context, i InviteRow) error
	GetInviteByHash(ctx context.Context, codeHash []byte) (InviteRow, error)
	// RedeemInvite: one UPDATE guarded by used_count < max_uses AND revoked_at IS NULL
	// AND expires_at > now; ErrExhausted unless RowsAffected == 1.
	RedeemInvite(ctx context.Context, codeHash []byte, now int64) (InviteRow, error)
	RevokeInvite(ctx context.Context, inviteID id.ID, at int64) error
	ListInvites(ctx context.Context, communityID *id.ID) ([]InviteRow, error)
	// PutInviteSignup remembers that userID registered with the community invite
	// inviteID, so that person's join with it spends no second use (protocol/09
	// § Invites). One row per user.
	PutInviteSignup(ctx context.Context, userID, inviteID id.ID, at int64) error
	// TakeInviteSignup deletes that row: ErrNotFound unless exactly one row went,
	// so of two racing joins only one is free.
	TakeInviteSignup(ctx context.Context, userID, inviteID id.ID) error
}

// MLS is the delivery service's own table set (004_mls.sql), implemented from
// task 19 onward. Declared here by deviation ID1: the method names are fixed
// once, so no later task invents one.
type MLS interface {
	CreateGroup(ctx context.Context, g GroupRow) error
	GetGroup(ctx context.Context, groupID id.ID) (GroupRow, error)
	// LockGroup serializes message acceptance against every mutation of this group's
	// epoch, membership, freeze state and sequence. It must run inside Tx before
	// reading policy or allocating the next sequence.
	LockGroup(ctx context.Context, groupID id.ID) error
	ListOpenGroups(ctx context.Context, after id.ID, limit int32) ([]GroupRow, error)
	// ListGroupsForRetention is the same paged walk WITHOUT the `closed_at IS NULL`
	// filter. Invariant 10 caps application ciphertext at thirty days for every
	// group, and a group invariant 11 closed is still ciphertext on the disk: a
	// sweep over open groups alone would leave a closed group's blobs unswept by
	// both halves of retention forever, which inverts the invariant into "kept
	// indefinitely" at the moment a group stops being useful. Deviation ID1 fixes
	// the method set once, so it is declared here rather than invented per caller.
	ListGroupsForRetention(ctx context.Context, after id.ID, limit int32) ([]GroupRow, error)
	CloseGroup(ctx context.Context, groupID id.ID, at int64) error
	// The three statements invariant 11 runs, added by deviation B13. Plan 2
	// records the first two as P2-D19 for the same work, so they are declared
	// ONCE, here, rather than twice with two spellings.
	//
	// MarkAllGroupsEpochUnknown is `dillad restore`'s "every group becomes
	// epoch-unknown", with the heal deadline the window gives it, as ONE
	// statement over every open group: a paged loop that stopped at a fixed
	// batch would leave every group past the batch serving state the restored
	// database no longer matches.
	MarkAllGroupsEpochUnknown(ctx context.Context, healDeadline int64) error
	// ClearEpochUnknown is an adopted heal. It clears the deadline with the
	// flag: a healed group that kept its deadline would look overdue to the
	// next sweep that read the pair.
	ClearEpochUnknown(ctx context.Context, groupID id.ID) error
	// EndAllVoiceSessions is invariant 11's "Live calls end." On a Plan-1
	// database a live call IS its call group -- R9 puts the call id in the
	// companion column and `voice_sessions` is Plan 2's table -- so this closes
	// every open call group. Plan 2 task 1 widens the same method rather than
	// declaring a second one.
	EndAllVoiceSessions(ctx context.Context, at int64) error
	NextSeq(ctx context.Context, groupID id.ID) (uint64, error) // one space, both streams
	PutGroupState(ctx context.Context, groupID id.ID, epoch uint64, state, groupInfo, treeHash []byte) error
	AppendHandshake(ctx context.Context, h HandshakeRow) error
	ListHandshakes(ctx context.Context, groupID id.ID, fromSeq uint64, limit int32) ([]HandshakeRow, error)
	OldestHandshakeSeq(ctx context.Context, groupID id.ID) (uint64, error)
	// GetCommitAtEpoch is the ONE handshake that carried a group into `epoch`:
	// the lowest-seq row of kind 1 (commit) or 2 (external_commit) at that
	// epoch, over `mls_handshakes_by_epoch`. Deviation B13 adds it because
	// `E_COMMIT_CONFLICT` must name the winning commit, and paging the log from
	// seq 0 misses it on any group with more than one page of live handshakes —
	// which would put a null where protocol/02 declares a `bstr` and break the
	// conflict-recovery loop invariant 3 exists for. ErrNotFound means the
	// epoch's handshake has been pruned, not that the epoch never happened.
	GetCommitAtEpoch(ctx context.Context, groupID id.ID, epoch uint64) (HandshakeRow, error)
	// PruneHandshakes deletes every handshake created before `before` and raises each affected
	// group's HandshakesPruned to the highest seq it lost, in one transaction.
	PruneHandshakes(ctx context.Context, before int64) (int64, error)
	PutProposal(ctx context.Context, p ProposalRow) error
	ListProposals(ctx context.Context, groupID id.ID, epoch uint64, includeVoid bool) ([]ProposalRow, error)
	VoidProposal(ctx context.Context, groupID id.ID, ref []byte, at int64) error
	// ListClosedGroupLiveProposals is up to limit non-void proposals, of any epoch, of closed
	// groups, ordered by group and ref: the sweep voids them, since no commit lands in a closed
	// group and none is reopened.
	ListClosedGroupLiveProposals(ctx context.Context, limit int32) ([]ProposalRow, error)
	// CountLiveInstanceProposals counts non-void instance proposals (origin 0) by kind at open
	// groups' current epoch, plus every epoch of closed groups: the outstanding gauge's seed.
	CountLiveInstanceProposals(ctx context.Context) (map[uint8]int64, error)
	DeleteProposals(ctx context.Context, groupID id.ID, refs [][]byte) error
	ReissueProposal(ctx context.Context, oldRef []byte, p ProposalRow) error // keeps action_id
	ReplaceMembers(ctx context.Context, groupID id.ID, epoch uint64, m []MemberRow) error
	ListMembers(ctx context.Context, groupID id.ID) ([]MemberRow, error)
	// ListBarredMembers is the group's live leaves whose device is quarantined or revoked: the
	// reconcile sweeper's one query per group for the Removes those devices are owed.
	ListBarredMembers(ctx context.Context, groupID id.ID) ([]MemberRow, error)
	GroupsForDevice(ctx context.Context, deviceID id.ID) ([]id.ID, error)
	// GroupsForTarget is Plan 2's P2-D3: the OPEN groups of one kind bound to one
	// target (a channel's text and call groups), oldest first, over
	// mls_groups_by_target. A membership change finds the groups to issue its
	// Adds and Removes in here, rather than scanning ListOpenGroups, which is
	// O(instance) per change.
	GroupsForTarget(ctx context.Context, targetID id.ID, kind uint8) ([]GroupRow, error)
	// ListIdleCallGroups is web-3's idle call-group close (L-HTTP-105, RV-2): the open call groups
	// (kind 1) no live voice session names, created at or before cutoff, oldest first, at most
	// limit of them.
	ListIdleCallGroups(ctx context.Context, cutoff int64, limit int32) ([]GroupRow, error)
	PutKeyPackages(ctx context.Context, deviceID id.ID, kps []KeyPackageRow) error
	TakeKeyPackage(ctx context.Context, deviceID id.ID, now int64) (KeyPackageRow, error)
	CountKeyPackages(ctx context.Context, deviceID id.ID, now int64) (int64, error)
	HasCurrentLastResortKeyPackage(ctx context.Context, deviceID id.ID, now int64) (bool, error)
	// DeleteKeyPackage removes one package from a device's directory, consumed or not, last
	// resort or not. Deleting a package that is not there is not an error.
	DeleteKeyPackage(ctx context.Context, deviceID id.ID, kpRef []byte) error
	PurgeKeyPackages(ctx context.Context, keepLastResort bool) (int64, error)
	PutWelcomePayload(ctx context.Context, w WelcomePayloadRow) error
	PutEpochTree(ctx context.Context, t EpochTreeRow) error
	PutWelcomes(ctx context.Context, w []WelcomeRow) error
	ListWelcomes(ctx context.Context, deviceID id.ID, afterID int64, limit int32) ([]WelcomeFull, error)
	DeleteWelcome(ctx context.Context, deviceID id.ID, welcomeID int64, at int64) error
	PruneWelcomes(ctx context.Context, before int64) (int64, error)
	PutForkReport(ctx context.Context, f ForkReportRow) error
	CountForkReporters(ctx context.Context, groupID id.ID, seq uint64) (int64, error)
	QuarantineDevice(ctx context.Context, deviceID id.ID, at int64, reason string) error
	// The pending-join queue (deviation B13 names the pair, B22 / ruling 43 defers it, Plan 1
	// follow-up card 8 hands it to Plan 2 task 7): the tail of a join storm beyond the 256 Adds
	// one commit may carry, durable so a restart between two slices loses no device.
	//
	// QueuePendingJoins queues devices for groupID at `at`. A device already queued keeps its row
	// and its place. ListPendingJoins READS the oldest `limit` devices, ties broken by device id,
	// and removes nothing: a drain deletes a device's row with DeletePendingJoins only once the
	// device is resolved (its Add stored, or judged ineligible), so a process that dies in the
	// middle of a slice leaves every unresolved device queued. (B13 named a take-and-delete pair;
	// Plan 2 task 7 fix round 1 split it, because a take commits the delete before the Adds exist.)
	// CountPendingJoins is the queue's length, and ListPendingJoinGroups pages the groups holding
	// a queue by group id after `after`, one row per group, for the sweeper that re-drives a
	// stalled storm.
	QueuePendingJoins(ctx context.Context, groupID id.ID, devices []id.ID, at int64) error
	ListPendingJoins(ctx context.Context, groupID id.ID, limit int32) ([]id.ID, error)
	DeletePendingJoins(ctx context.Context, groupID id.ID, devices []id.ID) error
	CountPendingJoins(ctx context.Context, groupID id.ID) (int64, error)
	ListPendingJoinGroups(ctx context.Context, after id.ID, limit int32) ([]id.ID, error)
}

// Messages is 005_messages.sql, implemented from task 23 onward.
type Messages interface {
	PutAppMessage(ctx context.Context, m AppMessageRow) error
	ListAppMessages(ctx context.Context, groupID id.ID, fromSeq uint64, limit int32) ([]AppMessageRow, error)
	GetAppMessage(ctx context.Context, groupID id.ID, seq uint64) (AppMessageRow, error)
	TombstoneAppMessage(ctx context.Context, groupID id.ID, seq uint64, at int64) error
	// PruneAppMessages applies invariant 10's TWO independent deletion triggers (R28/D14) in one
	// statement: delivery retention (`cursorFloor`, the lowest seq every ELIGIBLE device has
	// passed -- 0 meaning no eligible cursor exists and so no deletion -- or `deliveryFloor`,
	// now - MessageRetention) and archival retention (`expires` against `now`, where NULL means
	// retained indefinitely). Deviation D14: three parameters, not two, and `now` is not the
	// delivery floor. In the same transaction it raises the group's PrunedBelow to the highest
	// seq the DELIVERY triggers delete, so the high-water is exact for delivery retention. An
	// archival deletion never moves it (Plan 2 task 8's retention ruling): `expires` is not
	// monotone in seq, so a mark raised to an expired seq could stand above surviving messages
	// and turn a servable catch-up into E_PRUNED.
	PruneAppMessages(ctx context.Context, groupID id.ID, cursorFloor uint64, deliveryFloor, now int64) (int64, error)
	// RaisePrunedBelow raises the group's MONOTONE message high-water (`mls_groups.pruned_below`)
	// to `below`; a lower value is ignored. PruneAppMessages calls it itself with the exact seq it
	// deletes; it stays on the interface for a caller that must record a deletion it made some
	// other way.
	RaisePrunedBelow(ctx context.Context, groupID id.ID, below uint64) error
}

// Cursors is `device_cursors`, implemented from task 23 onward.
type Cursors interface {
	PutCursor(ctx context.Context, deviceID, groupID id.ID, lastSeq, lastEpoch uint64, at int64) error
	GetCursor(ctx context.Context, deviceID, groupID id.ID) (CursorRow, error)
	MinCursor(ctx context.Context, groupID id.ID, activeSince int64) (uint64, error) // eligible devices only
}

// Structure is 006_structure.sql and its successors, implemented from Plan 2
// task 1 onward. §4.1 declares it as one interface, but its tables land across
// six tasks (communities, members and roles in task 1; channels in 2;
// overwrites in 3; bans in 4; channel members in 6; voice sessions in 16), and
// ID1 forbids both stub methods and an embed whose methods do not exist yet. So
// the part task 1 ships is its own interface, Communities, which Structure
// embeds and Repository embeds on its own. Structure's method set is §4.1's
// plus the plan's additions; a later task that ships the rest of it either
// embeds its own slice the same way or, once every method exists, swaps
// Communities for Structure in Repository's embed list. Task 2's slice is
// Channels, task 3's Overwrites, task 4's Bans, task 6's ChannelMembers.
type Structure interface {
	Communities
	Channels
	Overwrites
	Bans
	ChannelMembers
	VoiceSessions
}

// RolePanels stores server-validated bot-authored role panels and their emoji mappings.
type RolePanels interface {
	SetRolePanel(ctx context.Context, panel RolePanelRow, options []RolePanelOptionRow) error
	GetRolePanel(ctx context.Context, channelID id.ID, messageSeq uint64) (RolePanelRow, error)
	ListRolePanelOptions(ctx context.Context, channelID id.ID, messageSeq uint64) ([]RolePanelOptionRow, error)
	DeleteRolePanel(ctx context.Context, channelID id.ID, messageSeq uint64) error
}

type RolePanelRow struct {
	CommunityID     id.ID
	ChannelID       id.ID
	MessageSeq      uint64
	AuthorID        id.ID
	InstallationID  id.ID
	SendOperationID id.ID
}

type RolePanelOptionRow struct {
	Emoji  string
	RoleID id.ID
}

// VoiceSessions is the part of Structure whose table is 00011_voice.sql (Plan 2 task 16,
// P2-D22): §4.1's two voice methods plus GetVoiceSession, which DELETE /v1/calls/{call_id}
// reads before it ends a call, and ListLiveVoiceSessions, which POST /v1/channels/{id}/calls
// reads to join a call already under way. A call is keyed by its call group's call id (R9).
type VoiceSessions interface {
	// PutVoiceSession records a call; putting an ended call's id again reopens it (the room,
	// the group and the start are rewritten and ended is cleared), and putting a live call's id
	// changes nothing, so concurrent starts of one call agree on its room.
	PutVoiceSession(ctx context.Context, v VoiceSessionRow) error
	// GetVoiceSession answers ErrNotFound for an unknown call.
	GetVoiceSession(ctx context.Context, callID id.ID) (VoiceSessionRow, error)
	// EndVoiceSession ends a live call; ErrNotFound when there is none by that id or it has
	// already ended.
	EndVoiceSession(ctx context.Context, callID id.ID, at int64) error
	// ListLiveVoiceSessions is the channel's calls with ended NULL, oldest first.
	ListLiveVoiceSessions(ctx context.Context, channelID id.ID) ([]VoiceSessionRow, error)
}

// ChannelMembers is the part of Structure whose table is
// 00008_channel_members.sql (Plan 2 task 6): §4.1's three channel-member methods
// plus P2-D11's listing. For a DM or group DM the rows are the participant list;
// for a community channel they are what the permission resolver derives.
type ChannelMembers interface {
	// PutChannelMember is idempotent: a second call for the same pair keeps the
	// first row and its added time.
	PutChannelMember(ctx context.Context, channelID, userID id.ID, at int64) error
	// DeleteChannelMember answers ErrNotFound when the user was not a member.
	DeleteChannelMember(ctx context.Context, channelID, userID id.ID) error
	// DeleteCommunityChannelMembers removes userID from channel_members of every channel of
	// communityID, live or tombstoned, and answers how many rows went. A kick, ban or leave runs it
	// in the transaction that removes the membership (fix wave C3). A DM is never matched.
	DeleteCommunityChannelMembers(ctx context.Context, communityID, userID id.ID) (int64, error)
	// ListChannelMembers is the channel's members ordered by user id, so the
	// order is the same on both engines.
	ListChannelMembers(ctx context.Context, channelID id.ID) ([]id.ID, error)
	// ListChannelsForUser is P2-D11: GET /v1/dms. The live DMs and group DMs the
	// user is a participant of, newest first, ties broken by channel id. A
	// community channel is never listed, whatever channel_members says.
	ListChannelsForUser(ctx context.Context, userID id.ID) ([]ChannelRow, error)
}

// Communities is the part of Structure whose tables are 00004_structure.sql:
// communities, members, roles and member_roles (Plan 2 task 1).
type Communities interface {
	GetCommunityAFK(ctx context.Context, communityID id.ID) (CommunityAFK, error)
	SetCommunityAFK(ctx context.Context, communityID id.ID, config CommunityAFK) error
	// SetAFKAway marks a member away in the community (in its AFK channel, with no voice
	// connection), since a unix time > 0. It answers false and keeps the stored since when the
	// member is already away (R-AW7), and answers ErrNotFound for a non-member (the row
	// references members).
	SetAFKAway(ctx context.Context, communityID, userID id.ID, since int64) (bool, error)
	// ClearAFKAway clears one away row and answers whether there was one.
	ClearAFKAway(ctx context.Context, communityID, userID id.ID) (bool, error)
	// ClearAFKAwayUser clears the user's away rows in every community and answers those
	// communities, ordered by id (R-AW4: an admission anywhere, or the last gateway connection
	// closing).
	ClearAFKAwayUser(ctx context.Context, userID id.ID) ([]id.ID, error)
	// ListAFKAway is the community's away roster ordered by user id. It is bounded by the
	// community's member count.
	ListAFKAway(ctx context.Context, communityID id.ID) ([]AFKAway, error)
	// ListAFKAwayFirst is the first limit rows of ListAFKAway, bounded in the query (the
	// snapshot's cap).
	ListAFKAwayFirst(ctx context.Context, communityID id.ID, limit int32) ([]AFKAway, error)
	// AFKAwayExists and AFKAwayExistsUser are read-only lookups: an away row of the user in the
	// community, and in any community (the clears' check while the in-memory index is unseeded).
	AFKAwayExists(ctx context.Context, communityID, userID id.ID) (bool, error)
	AFKAwayExistsUser(ctx context.Context, userID id.ID) (bool, error)
	CreateCommunity(ctx context.Context, c CommunityRow) error
	// GetCommunity answers ErrNotFound for a soft-deleted community too.
	GetCommunity(ctx context.Context, communityID id.ID) (CommunityRow, error)
	// ListCommunities is `dillad admin community list`: the live communities
	// with id > after, ordered by id (so the order is the same on both engines),
	// at most limit. A soft-deleted community is left out, as GetCommunity does.
	ListCommunities(ctx context.Context, after id.ID, limit int32) ([]CommunityRow, error)
	// ListCommunitiesForUser is GET /v1/communities: the live communities userID is a member of,
	// ordered by community id (the same order on both engines). A soft-deleted community is left out.
	ListCommunitiesForUser(ctx context.Context, userID id.ID) ([]CommunityRow, error)
	// UpdateCommunityPolicy writes the policy under version, which must be
	// greater than the stored one. The version is monotone, so a writer that
	// lost a race to the same successor gets ErrConflict rather than landing a
	// second policy under a number clients already hold. ErrNotFound is an
	// unknown or deleted community.
	UpdateCommunityPolicy(ctx context.Context, communityID id.ID, policy []byte, version int64) error
	// UpdateCommunityMeta and SoftDeleteCommunity are P2-D7: §4.3 declares
	// name, min_account_age_seconds, require_mod_2fa and deleted_at mutable and
	// §4.1 had no way to write them.
	UpdateCommunityMeta(ctx context.Context, communityID id.ID, name string, minAge uint64, requireMod2FA uint8) error
	SoftDeleteCommunity(ctx context.Context, communityID id.ID, at int64) error
	// SetCommunityOwner hands a live community to owner (POST /v1/communities/{id}/owner). The
	// caller holds LockCommunity in the same transaction, which already answered ErrNotFound for an
	// unknown or deleted community, and has checked that owner is a member.
	SetCommunityOwner(ctx context.Context, communityID, owner id.ID) error
	// LockCommunity is Plan 2 task 4 fix round 1: it holds the community row
	// until the transaction ends, so a join's ban check and member insert and a
	// ban's ban write and member delete serialise rather than interleave (a ban
	// landing between the check and the insert would leave a banned member).
	// Postgres takes SELECT ... FOR NO KEY UPDATE; a SQLite write transaction is
	// exclusive already. It is refused outside a Tx, where the lock would end
	// with the statement, and answers ErrNotFound for an unknown or deleted
	// community.
	LockCommunity(ctx context.Context, communityID id.ID) error
	// PutMember is an upsert: a second call rewrites the nick and nothing else.
	PutMember(ctx context.Context, m MemberOfCommunityRow) error
	// GetMember is P2-D7, every membership gate's read: ErrNotFound for a
	// non-member.
	GetMember(ctx context.Context, communityID, userID id.ID) (MemberOfCommunityRow, error)
	// DeleteMember drops the member's role grants with the row (member_roles
	// cascades), so a re-join does not resurrect them.
	DeleteMember(ctx context.Context, communityID, userID id.ID) error
	ListMembersOfCommunity(ctx context.Context, communityID, after id.ID, limit int32) ([]MemberOfCommunityRow, error)
	PutRole(ctx context.Context, r RoleRow) error
	// GetRole is P2-D9, written in task 1 because the role-grant route needs it.
	GetRole(ctx context.Context, roleID id.ID) (RoleRow, error)
	ListRoles(ctx context.Context, communityID id.ID) ([]RoleRow, error)
	// DeleteRole is P2-D9: DELETE /v1/roles/{id}. The role's grants go with it
	// (member_roles cascades). ErrNotFound is an unknown role or one of another
	// community.
	DeleteRole(ctx context.Context, communityID, roleID id.ID) error
	PutMemberRole(ctx context.Context, communityID, userID, roleID id.ID) error
	DeleteMemberRole(ctx context.Context, communityID, userID, roleID id.ID) error
	// AddMemberRoleGrant and RemoveMemberRoleGrant track the reason a role is held.
	// The effective member_roles row remains while any source grant exists.
	AddMemberRoleGrant(ctx context.Context, communityID, userID, roleID id.ID, source RoleGrantSource) error
	RemoveMemberRoleGrant(ctx context.Context, communityID, userID, roleID id.ID, source RoleGrantSource) error
	HasMemberRoleGrant(ctx context.Context, communityID, userID, roleID id.ID, source RoleGrantSource) (bool, error)
	ListSelfRoleGrantUsers(ctx context.Context, communityID, roleID id.ID) ([]id.ID, error)
	// RevokeMemberRole atomically removes the effective role and all of its sources.
	RevokeMemberRole(ctx context.Context, communityID, userID, roleID id.ID) error
	// SetSelfRolePolicy atomically replaces the approved self-assignable role list.
	// Callers hold LockCommunity in the same transaction.
	SetSelfRolePolicy(ctx context.Context, communityID id.ID, approver id.ID, roles []id.ID, approvedAt int64) error
	ListSelfRolePolicy(ctx context.Context, communityID id.ID) ([]SelfRolePolicyRow, error)
	// ListMemberRoles is Plan 2's P2-D7b: GET /v1/communities/{id}/members must
	// list each member's roles and the permission resolver needs the set a user
	// holds, which deriving from ListRoles would make a full scan per member.
	ListMemberRoles(ctx context.Context, communityID, userID id.ID) ([]id.ID, error)
}

// RoleGrantSource identifies an independent reason a member holds a role.
type RoleGrantSource string

// SelfRolePolicyRow is a manager-approved role in a community's current self-service allowlist.
type SelfRolePolicyRow struct {
	CommunityID id.ID
	RoleID      id.ID
	ApprovedBy  id.ID
	ApprovedAt  int64
}

const (
	RoleGrantManual   RoleGrantSource = "manual"
	RoleGrantSelf     RoleGrantSource = "self"
	RoleGrantReaction RoleGrantSource = "reaction"
)

// Channels is the part of Structure whose table is 00005_channels.sql (Plan 2
// task 2): §4.1's five channel methods plus P2-D8's two.
type Channels interface {
	CreateChannel(ctx context.Context, c ChannelRow) error
	// GetChannel answers ErrNotFound for a deleted channel too.
	GetChannel(ctx context.Context, channelID id.ID) (ChannelRow, error)
	// ListChannels is the community's live channels ordered by position, ties
	// broken by id, so the order is the same on both engines and every call.
	ListChannels(ctx context.Context, communityID id.ID) ([]ChannelRow, error)
	// UpdateChannel writes mode, visibility, parent, name, topic, position,
	// settings, host policy version and slow mode. Kind, community, seq and
	// created are immutable here. ErrNotFound is an unknown or deleted channel.
	UpdateChannel(ctx context.Context, c ChannelRow) error
	// DeleteChannel tombstones the channel; a second call is ErrNotFound.
	DeleteChannel(ctx context.Context, channelID id.ID, at int64) error
	// DeleteChannelsOfCommunity (P2-D8) tombstones every live channel of the
	// community in one statement and returns how many it tombstoned.
	DeleteChannelsOfCommunity(ctx context.Context, communityID id.ID, at int64) (int64, error)
	// NextChannelSeq (P2-D8) is the per-channel sequencer: it raises
	// channels.seq by one and returns the new value. ErrNotFound is an unknown or
	// deleted channel.
	NextChannelSeq(ctx context.Context, channelID id.ID) (uint64, error)
}

// Overwrites is the part of Structure whose table is 00006_overwrites.sql (Plan
// 2 task 3): §4.1's two overwrite methods plus P2-D9's delete.
type Overwrites interface {
	// PutOverwrite is an upsert on (channel, target kind, target): a second
	// call replaces allow and deny.
	PutOverwrite(ctx context.Context, o OverwriteRow) error
	// ListOverwrites is the channel's overwrites ordered by (target_kind,
	// target_id), so the order is the same on both engines.
	ListOverwrites(ctx context.Context, channelID id.ID) ([]OverwriteRow, error)
	// DeleteOverwrite is P2-D9: DELETE /v1/channels/{id}/overwrites/{kind}/{target_id}.
	// ErrNotFound when there was no such overwrite.
	DeleteOverwrite(ctx context.Context, channelID id.ID, targetKind uint8, targetID id.ID) error
	// DeleteUserOverwrites removes userID's own (kind 1) overwrites in every channel of
	// communityID and answers how many went: a kick, ban or leave runs it with the membership
	// delete, so a user overwrite never outlives the membership it was written for (fix wave I2).
	DeleteUserOverwrites(ctx context.Context, communityID, userID id.ID) (int64, error)
}

// MemberTimeouts is the part of Structure whose table is 00007_bans.sql (Plan 2 task 4):
// §4.1's three ban methods plus P2-D10's listing.
type MemberTimeouts interface {
	PutMemberTimeout(context.Context, MemberTimeoutRow) error
	GetMemberTimeout(context.Context, id.ID, id.ID, int64) (MemberTimeoutRow, error)
	DeleteMemberTimeout(context.Context, id.ID, id.ID) error
}

type Bans interface {
	// PutBan is an upsert on (community, user): a second ban of the same user
	// replaces the reason, the moderator, the time and the expiry.
	PutBan(ctx context.Context, b BanRow) error
	// GetBan answers ErrNotFound when there is no row. A row whose Expires has
	// passed is still returned: whether it gates is the caller's decision.
	GetBan(ctx context.Context, communityID, userID id.ID) (BanRow, error)
	// ListBans is P2-D10: GET /v1/communities/{id}/bans. Newest first, ties
	// broken by user id, lapsed bans included.
	ListBans(ctx context.Context, communityID id.ID) ([]BanRow, error)
	// DeleteBan lifts a ban. ErrNotFound when there was none.
	DeleteBan(ctx context.Context, communityID, userID id.ID) error
}

// Readable is 007_readable.sql (migration 00009_readable.sql), implemented by
// Plan 2 task 8.
type Readable interface {
	// PutReadableMessage appends one message and returns its rowid. A second row
	// at the same (channel, seq) is ErrConflict.
	PutReadableMessage(ctx context.Context, m ReadableMessageRow) (int64, error)
	// ListReadableMessages is the channel's messages from fromSeq upward, at most
	// limit, deleted ones included (with an empty envelope and body).
	ListReadableMessages(ctx context.Context, channelID id.ID, fromSeq uint64, limit int32) ([]ReadableMessageRow, error)
	// EditReadableMessage is P2-D13: body is a parameter of its own, because
	// the indexed column cannot be filled from the envelope without the store
	// parsing envelopes, which is internal/api's job. Plan 2 task 9 adds the
	// franking tag and the id of the key that made it: an edit re-franks the new
	// bytes, and a tag that committed to text the row no longer holds could
	// never verify. Plan 2 task 17 widens that to the whole tuple (the editing
	// device and the new C). ErrNotFound for an unknown or deleted message.
	EditReadableMessage(ctx context.Context, channelID id.ID, seq uint64,
		envelope []byte, body string, f ReadableFranking, at int64) error
	// DeleteReadableMessage empties the envelope and the body (so the row leaves
	// the index) and keeps the franking tuple. ErrNotFound for an unknown or an
	// already deleted message.
	DeleteReadableMessage(ctx context.Context, channelID id.ID, seq uint64, at int64) error
	// PutReadState is monotone: a lower lastReadSeq than the stored one is
	// ignored, so a stale tab cannot un-read a channel.
	PutReadState(ctx context.Context, userID, channelID id.ID, lastReadSeq uint64) error
	// GetReadState answers ErrNotFound when the user has never read the channel.
	GetReadState(ctx context.Context, userID, channelID id.ID) (uint64, error)
	// LastReadableMessageAt is the slowmode gate's read, added by Plan 2 task 8
	// beside P2-D13: the created time of userID's latest message in the channel,
	// deleted ones included (a delete must not reset the gate), or ErrNotFound.
	LastReadableMessageAt(ctx context.Context, channelID, userID id.ID) (int64, error)
	// LastClassZeroUploadAt is slow mode's read for end-to-end encrypted posts
	// (web3.7 SB1): userID's durable mark in channelID, the time of their last
	// accepted class-0 upload into any of its text groups from any device, or
	// ErrNotFound. Neither a tombstone nor a retention prune moves it (fix
	// round 1): the mark lives in slowmode_marks, not in mls_app_messages.
	LastClassZeroUploadAt(ctx context.Context, channelID, userID id.ID) (int64, error)
	// PutSlowmodeMark raises userID's mark in channelID to at, never lowering
	// it. The delivery service calls it in an accepted class-0 upload's own
	// transaction (web3.7 SB1 fix round 1).
	PutSlowmodeMark(ctx context.Context, channelID, userID id.ID, at int64) error
	// ListReadableAudience is who a readable channel's message.plain frames
	// reach, added by Plan 2 task 8 (P2-D14's fan-out): the channel's
	// materialised channel_members that are still members of its community,
	// ordered by user id.
	ListReadableAudience(ctx context.Context, channelID id.ID) ([]id.ID, error)
	ReadableSearch
}

// ReadableSearch is hand-written database/sql per engine: sqlc can generate
// neither side, for opposite reasons (gap-69 claims 18 and 19).
type ReadableSearch interface {
	SearchReadable(ctx context.Context, q ReadableSearchQuery) ([]ReadableSearchHit, error)
}

// ReadableSearchQuery is one search. ChannelIDs is pre-filtered by the
// permission resolver and is never empty.
type ReadableSearchQuery struct {
	ChannelIDs []id.ID
	Query      ParsedQuery
	Limit      int32
	BeforeSeq  uint64
}

// ReadableSearchHit is one result row, snippet and score included, so the two
// engines' ts_headline and FTS5 snippet answers reach the API identically.
type ReadableSearchHit struct {
	ChannelID id.ID
	Seq       uint64
	Sender    id.ID
	Snippet   string
	Score     float32
	Created   int64
}

// Blobs is 008_blobs.sql, implemented from Plan 2 task 10 onward. A channel
// never deletes a blob, only its reference (gap-47 §0): the row goes when the
// last reference has been gone for the grace window, and blob_refs' ON DELETE
// RESTRICT makes DeleteBlob fail while any reference stands.
//
// PutBlob, PutBlobRef, PutPendingBlobRef and PutBlobTombstone are idempotent: the id is the hash of
// the bytes, so a second insert names the same object and keeps the first row.
// DeleteBlobRef of an absent reference is not an error; DeleteBlob of an absent
// row is ErrNotFound. MarkBlobUnreferenced sets unref_since only when no
// reference is left and it is not already set.
type Blobs interface {
	PutBlob(ctx context.Context, b BlobRow) error
	GetBlob(ctx context.Context, blobID []byte) (BlobRow, error)
	PutBlobRef(ctx context.Context, blobID []byte, channelID, uploaderDevice id.ID, mime string, created int64) error
	// PutPendingBlobRef leaves an existing reference unchanged, pending or confirmed (L-SQL-31).
	PutPendingBlobRef(ctx context.Context, blobID []byte, channelID, uploaderDevice id.ID, mime string, created int64) error
	// GetBlobRef is P2-D17: the GET's "404, never 403" rule reads the reference
	// in the requested channel, and task 11's uploader-only DELETE reads its
	// uploader. ErrNotFound when the channel holds no reference.
	GetBlobRef(ctx context.Context, blobID []byte, channelID id.ID) (BlobRefRow, error)
	// ConfirmBlobRef is idempotent and returns ErrNotFound when the reference is absent (L-SQL-31).
	ConfirmBlobRef(ctx context.Context, blobID []byte, channelID id.ID) error
	DeleteBlobRef(ctx context.Context, blobID []byte, channelID id.ID) error
	CountBlobRefs(ctx context.Context, blobID []byte) (int64, error)
	MarkBlobUnreferenced(ctx context.Context, blobID []byte, at int64) error
	// ClearBlobUnreferenced is P2-D16: every PUT that creates a reference
	// clears unref_since in the same transaction (gap-47 §8.3).
	ClearBlobUnreferenced(ctx context.Context, blobID []byte) error
	ListCollectableBlobs(ctx context.Context, before int64, limit int32) ([]BlobRow, error)
	// ListBlobs is Plan 2 task 12's backup walk: every blob row whose id sorts
	// strictly after `after` (nil starts at the beginning), in blob_id byte
	// order, at most limit.
	ListBlobs(ctx context.Context, after []byte, limit int32) ([]BlobRow, error)
	DeleteBlob(ctx context.Context, blobID []byte) error
	PutBlobTombstone(ctx context.Context, blobID []byte, reason string, by id.ID, at int64) error
	GetBlobTombstone(ctx context.Context, blobID []byte) (bool, error)
	// UserBlobBytes is what blobs.quota_bytes_per_user bounds: the size of each distinct blob the
	// user references from any device or names in one of their backups rows, counted once.
	UserBlobBytes(ctx context.Context, userID id.ID) (int64, error)
	// UserReferencesBlob reports whether the user already references the blob in any channel,
	// that is, whether its bytes already count toward their quota (M3).
	UserReferencesBlob(ctx context.Context, userID id.ID, blobID []byte) (bool, error)
	// InstanceBlobBytes is the size of every blob row, referenced or not: what
	// blobs.store_max_bytes bounds (fix wave C7).
	InstanceBlobBytes(ctx context.Context) (int64, error)
	// DeleteAllBlobRefs is P2-D18: the admin purge removes every reference to
	// the blob, in every channel, in one statement, and reports how many.
	DeleteAllBlobRefs(ctx context.Context, blobID []byte) (int64, error)
	// The sweeper's reference-expiry phase (Plan 2 task 11). A reference goes
	// when its community's archival retention (R28, the policy's
	// retention_days) has passed, or when its channel was deleted; the blob is
	// then unreferenced and the grace window collects it like any other.
	//
	// ListBlobRetentionPolicies is the stored policy of every live community
	// that holds a reference in a live channel, ordered by community id.
	ListBlobRetentionPolicies(ctx context.Context) ([]BlobRetentionRow, error)
	// ListExpiredBlobRefs is the community's references created strictly
	// before `before`, oldest first, at most limit.
	ListExpiredBlobRefs(ctx context.Context, communityID id.ID, before int64, limit int32) ([]BlobRefRow, error)
	// ListBlobRefsOfDeletedChannels is the references held by tombstoned
	// channels, oldest first, at most limit.
	ListBlobRefsOfDeletedChannels(ctx context.Context, limit int32) ([]BlobRefRow, error)
	// ListPendingBlobRefs returns unconfirmed references strictly older than before, oldest first.
	ListPendingBlobRefs(ctx context.Context, before int64, limit int32) ([]BlobRefRow, error)
}

type Ops interface {
	PutReport(ctx context.Context, r ReportRow) error
	GetReport(ctx context.Context, reportID id.ID) (ReportRow, error)
	// ListReports is GET /v1/reports (Plan 2 task 17): newest first, ties
	// broken by id descending, at most limit.
	ListReports(ctx context.Context, limit int32) ([]ReportRow, error)
	UpdateReportStatus(ctx context.Context, reportID id.ID, status int32, result string) error
	Audit(ctx context.Context, a AuditRow) error
	ListAudit(ctx context.Context, since int64, limit int32) ([]AuditRow, error)
	SchemaVersion(ctx context.Context) (int64, error)
}

// OpsBackups is §4.1's backup half of Ops. Its table is 008_blobs.sql, so
// Plan 2 task 10 implements it and adds it to Repository (deviation ID2).
type OpsBackups interface {
	PutBackup(ctx context.Context, b BackupRow) error
	ListBackups(ctx context.Context, userID id.ID, kind int32) ([]BackupRow, error)
	// InsertBackup never replaces: a taken key is ErrConflict (the write-once root object, F3).
	InsertBackup(ctx context.Context, b BackupRow) error
	// GetBackup reads one row by its full key; ErrNotFound when there is none.
	GetBackup(ctx context.Context, userID id.ID, kind int32, deviceID id.ID, chunkSeq int64) (BackupRow, error)
	// BackupRefersToBlob reports whether any backups row of any user names the blob.
	BackupRefersToBlob(ctx context.Context, blobID []byte) (bool, error)
	// LockBlob takes the per-blob lock inside Tx: the blobs row FOR UPDATE on Postgres (the caller
	// inserts it first if absent), a write claim on SQLite, whose one writer already serialises
	// every write transaction. The attachment and backup recording transactions take it before
	// their cross-table checks, so backup objects and attachments stay disjoint under concurrency.
	LockBlob(ctx context.Context, blobID []byte) error
}

// All thirteen sub-interfaces of §4.1 are declared above, as deviation ID1
// requires, and `Repository` embeds only the seven whose tables part 1a ships.
// Fix round 2 resolved the contradiction that left the last six undeclared: the
// same task must declare their Row types, because a Go interface method cannot
// name a type that does not exist. Those types are in rows.go, mirrored from
// §4.3, and `ParsedQuery`/`Term` — §6.7's engine-neutral parse, which
// `ReadableSearchQuery` names — are declared there too, without the parser
// functions Plan 2 task 8 brings. Each later task that satisfies one of the six
// edits only the embed list: task 19 (MLS), task 23 (Messages, Cursors) and Plan
// 2's tasks 1, 8 and 10 (Structure, Readable, Blobs with OpsBackups). Plan 2
// task 1 embeds Communities, the slice of Structure its tables support; see
// Structure's comment for why Structure itself cannot be embedded yet.
// contract_test.go holds §4.1 as an assertion so a rename is a test failure.
