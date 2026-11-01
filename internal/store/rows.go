package store

import "github.com/jonasthim/dilla/internal/id"

// Every Row mirrors its table's columns one for one, in declaration order
// (interfaces.md §4.1). Timestamps are int64 unix seconds; counters are uint64;
// identifiers are id.ID, or *id.ID when the column is nullable.

type InstanceRow struct {
	InstanceID          id.ID
	ExternalSenderKeyID id.ID
	KeyHistory          []byte
	FrankingKeyID       id.ID
	Generation          uint64
	PolicyVersion       uint64
	Created             int64
}

type UserRow struct {
	ID         id.ID
	Username   string
	Display    string
	Kind       uint8
	UMKPub     []byte
	SSKPub     []byte
	SigUMKSSK  []byte
	Flags      uint64
	AgeBracket uint64
	Created    int64
	DisabledAt *int64
	DeletedAt  *int64
	// FactorOwed is users.factor_owed (web3.6 S1): the account holds no second factor yet, so its
	// enrolled sessions reach only protocol/09's `F` and `P` routes.
	FactorOwed bool
	// StatusMsg (at most 190 characters) and Bio (at most 500) are the account's profile text (EOD E6):
	// readable by the server like Display; "" is none.
	StatusMsg string
	Bio       string
}

// UserFlag bits, recorded in protocol/09-http-api.md § Flags (NV8).
const (
	UserFlagInstanceAdmin uint64 = 1 << 0
	UserFlagBotOperator   uint64 = 1 << 1
)

type DeviceRow struct {
	ID               id.ID
	UserID           id.ID
	DSKPub           []byte
	Tier             uint8
	SignerTier       uint8
	CredentialBlob   []byte
	VerifiedAt       *int64
	RevokedAt        *int64
	QuarantinedAt    *int64
	QuarantineReason string
	LastSeen         int64
	Created          int64
	// Label is the owner's name for the device, 1..64 characters, nil when unnamed. It is not in the
	// signed device list (protocol/09).
	Label *string
}

type DeviceListRow struct {
	UserID       id.ID
	Version      uint64
	Blob         []byte
	SSKSignature []byte
	PrevHash     []byte
	Created      int64
}

type SessionRow struct {
	TokenHash    []byte
	DeviceID     id.ID
	UserID       id.ID
	Scope        uint8
	Tier         uint8 // copied from devices.tier at issue time (deviation ID15)
	Created      int64
	Expires      int64
	IdleExpires  int64
	WireVersion  uint64
	E2EEVersion  uint64
	MediaVersion uint64
	// FactorOwed is the session's account's users.factor_owed, read by GetSessionByHash's join
	// (web3.6 S1). CreateSession ignores it.
	FactorOwed bool
}

type TOTPRow struct {
	UserID      id.ID
	Secret      []byte
	Digits      uint64
	Period      uint64
	Algorithm   string
	ConfirmedAt *int64
	LastCounter int64
	Created     int64
}

type WebauthnCredentialRow struct {
	CredID            []byte
	RPID              string
	UserID            id.ID
	PublicKey         []byte
	SignCount         int64
	AttestationType   string
	AttestationFormat string
	Transports        string
	Flags             []byte
	ExtensionsJSON    string
	Name              string
	Created           int64
	LastUsed          *int64
}

type CeremonyRow struct {
	ID          id.ID
	Kind        uint8
	UserID      *id.ID
	SessionJSON string
	Created     int64
	Expires     int64
}

type LoginAttemptRow struct {
	UserID *id.ID
	IP     string
	Method uint8
	OK     bool
	At     int64
}

type InviteRow struct {
	ID          id.ID
	CodeHash    []byte
	CommunityID *id.ID
	CreatedBy   *id.ID
	GrantsAdmin uint8
	MaxUses     uint64
	UsedCount   uint64
	Created     int64
	ExpiresAt   int64
	RevokedAt   *int64
	// UnlimitedUses (Task 10c, "∞") makes MaxUses irrelevant: such a row stores MaxUses 1000, so
	// a reader that ignores the flag still stops at the old cap.
	UnlimitedUses uint8
}

// InviteNeverExpires is the ExpiresAt of an invite that never expires ("never", Task 10c): the
// largest signed 64-bit second, so every `expires_at > now` check keeps holding without a flag.
const InviteNeverExpires int64 = 1<<63 - 1

// NeverExpires reports whether the invite was made with no expiry.
func (i InviteRow) NeverExpires() bool { return i.ExpiresAt == InviteNeverExpires }

// Exhausted reports whether every use has been spent; an unlimited invite never is.
func (i InviteRow) Exhausted() bool { return i.UnlimitedUses == 0 && i.UsedCount >= i.MaxUses }

type ReportRow struct {
	ID                 id.ID
	Reporter           id.ID
	GroupID            id.ID
	Seq                uint64
	RevealedEnvelope   []byte
	KF                 []byte
	FrankingKeyID      id.ID
	VerificationResult string
	Status             int32
	Created            int64
}

type AuditRow struct {
	Actor  *id.ID
	Action string
	Target string
	Detail string
	At     int64
}

// ---------------------------------------------------------------------------
// Rows for the tables part 1a does not create.
//
// AMENDED (task 3, fix round 2 — interfaces.md §4.1): these are declared HERE,
// ahead of their tables, rather than by the task that ships each table. The
// plan's deviation ID1 requires all thirteen sub-interfaces of §4.1 to be
// declared in store.go in this task ("the declarations are unchanged, so no
// later task renames a method"), and a Go interface method cannot name a type
// that does not exist — so the interfaces and their Row types stand or fall
// together. Each mirrors §4.3's columns one for one in declaration order, by
// §4.2's mapping; the task that ships the table fills the queries, and any
// column it adds is a deviation it records, exactly as it would have been for
// the method names.
//
// The tasks that own them: task 19 (`004_mls.sql` — GroupRow … ForkReportRow),
// task 23 (`005_messages.sql` — AppMessageRow, CursorRow), Plan 2 task 1
// (`006_structure.sql`), Plan 2 task 8 (`007_readable.sql`) and Plan 2 task 10
// (`008_blobs.sql`).
// ---------------------------------------------------------------------------

// GroupRow mirrors `mls_groups`. EpochUnknown and HealDeadline are deviation
// B13's spelling (bool, not the column's 0/1 integer), because §4.1's healing
// methods read them as a predicate and a deadline.
type GroupRow struct {
	GroupID             id.ID
	Binding             []byte // deterministic-CBOR dilla_binding, never JSON text
	Kind                uint8
	CommunityID         *id.ID
	TargetID            id.ID
	CallID              *id.ID
	Ciphersuite         uint64
	Epoch               uint64
	Seq                 uint64 // high-water; next_seq = seq + 1
	GroupInfoBlob       []byte
	TreeHash            []byte
	PublicGroupState    []byte
	ExternalSenderKeyID id.ID
	E2EEVersion         uint64
	MediaVersion        uint64
	PolicyVersion       uint64
	EpochUnknown        bool
	HealDeadline        *int64
	Created             int64
	ClosedAt            *int64
	// PrunedBelow and HandshakesPruned are invariant 10's retention high-waters: the highest seq
	// of an application message, and of a handshake, that retention has deleted from this group
	// (0: none). PruneAppMessages and PruneHandshakes raise them in the transaction that deletes,
	// so a catch-up from `from` has lost something exactly when `from` is at or below the mark.
	// CreateGroup leaves both at the column's 0.
	PrunedBelow      uint64
	HandshakesPruned uint64
}

// HandshakeRow mirrors `mls_handshakes`. A leaf index is uint32, the width the
// wasi ABI and OpenMLS use for one (interfaces.md §3.6); a nil SenderLeaf or
// SenderDevice is the instance external sender.
type HandshakeRow struct {
	GroupID      id.ID
	Seq          uint64
	Epoch        uint64
	Kind         uint8
	SenderLeaf   *uint32
	SenderDevice *id.ID
	Blob         []byte
	Created      int64
}

// ProposalRow mirrors `mls_pending_proposals`. ActionID is the logical action
// and survives a re-issue with a fresh KeyPackage.
type ProposalRow struct {
	GroupID      id.ID
	Ref          []byte
	Epoch        uint64
	Kind         uint8
	TargetLeaf   *uint32
	TargetDevice *id.ID
	KeyPackage   []byte
	Origin       uint8
	ActionID     id.ID
	IssuedAt     int64
	TTL          uint64
	VoidAt       *int64
}

// MemberRow mirrors `mls_members`.
type MemberRow struct {
	GroupID      id.ID
	LeafIndex    uint32
	UserID       id.ID
	DeviceID     id.ID
	SignatureKey []byte
	AddedEpoch   uint64
	RemovedEpoch *uint64
}

// KeyPackageRow mirrors `key_packages`.
type KeyPackageRow struct {
	DeviceID   id.ID
	KPRef      []byte
	Blob       []byte
	LastResort uint8
	Expires    int64
	Created    int64
	ConsumedAt *int64
}

// WelcomePayloadRow mirrors `mls_welcome_payloads`, the shared body of D4's
// three-table Welcome split.
type WelcomePayloadRow struct {
	BlobSHA256 []byte
	GroupID    id.ID
	Epoch      uint64
	Blob       []byte
	Created    int64
}

// EpochTreeRow mirrors `mls_epoch_trees`.
type EpochTreeRow struct {
	GroupID     id.ID
	Epoch       uint64
	RatchetTree []byte
	TreeHash    []byte
	Created     int64
}

// WelcomeRow mirrors `mls_welcomes`, the per-device pointer at a payload.
// WelcomeID is the surrogate key `ListWelcomes` and `DeleteWelcome` page by.
type WelcomeRow struct {
	WelcomeID   int64
	DeviceID    id.ID
	GroupID     id.ID
	Epoch       uint64
	CommitSeq   uint64
	BlobSHA256  []byte
	Created     int64
	Expires     int64
	DeliveredAt *int64
}

// WelcomeFull is what `MLS.ListWelcomes` returns: one `mls_welcomes` row joined
// to the `mls_welcome_payloads` body it names AND to the `mls_epoch_trees` row
// of its own epoch. The three tables are D4's split, so nothing else carries
// every half.
//
// RatchetTree and TreeHash are the tree of the WELCOMING epoch, not the live
// one: a dilla Welcome carries no ratchet tree and the group has moved on by
// the time a joiner collects, so protocol/02 row 15 puts the tree of the epoch
// that issued the Welcome in the response. They are empty only when the epoch
// tree has been pruned out from under the row, which is a torn state rather
// than a shape a joiner can act on.
type WelcomeFull struct {
	WelcomeRow
	Blob        []byte
	RatchetTree []byte
	TreeHash    []byte
}

// ForkReportRow mirrors `fork_reports`.
type ForkReportRow struct {
	GroupID        id.ID
	Seq            uint64
	ReporterDevice id.ID
	Epoch          uint64
	Reason         string
	Created        int64
}

// AppMessageRow mirrors `mls_app_messages`. Blob is NULL once tombstoned and
// Expires is NULL when the message is retained under an archival policy.
// FrankingKeyID names the instance franking key FrankingTag was made under
// (Plan 2 task 17, P2-D21), so a report still verifies after a rotation; the
// all-zero id marks a row franked before the column existed. Class is the
// upload class (web3.7 SB1, protocol/02 § Upload class): 0 a post, 1 anything
// else, nil a legacy two-element upload.
type AppMessageRow struct {
	GroupID        id.ID
	Seq            uint64
	Epoch          uint64
	UploaderDevice id.ID
	Blob           []byte
	CommitmentC    []byte
	FrankingTag    []byte
	Size           uint64
	Created        int64 // = recv_ts
	Expires        *int64
	DeletedAt      *int64
	FrankingKeyID  id.ID
	Class          *uint8
}

// CursorRow mirrors `device_cursors`.
type CursorRow struct {
	DeviceID  id.ID
	GroupID   id.ID
	LastSeq   uint64
	LastEpoch uint64
	Updated   int64
}

// CommunityRow mirrors `communities`. PolicyJSON is the JSON policy blob of
// §4.2 (TEXT on SQLite, JSONB on Postgres, []byte in Go).
type CommunityRow struct {
	ID                   id.ID
	Owner                id.ID
	Name                 string
	IconBlob             []byte
	PolicyJSON           []byte
	PolicyVersion        uint64
	MinAccountAgeSeconds uint64
	RequireMod2FA        uint8
	Created              int64
	DeletedAt            *int64
}

// AFKAway is one row of a community's away roster: the member is in the AFK channel and has
// no voice connection, since a unix time.
type AFKAway struct {
	UserID id.ID
	Since  int64
}

// CommunityAFK configures the optional silent destination for idle voice devices.
type CommunityAFK struct {
	ChannelID      *id.ID
	TimeoutSeconds uint64
}

// MemberOfCommunityRow mirrors `members`. The name keeps it apart from
// MemberRow, which is an MLS leaf.
type MemberOfCommunityRow struct {
	CommunityID id.ID
	UserID      id.ID
	EpisodeID   id.ID
	Joined      int64
	Nick        string
}

// RoleRow mirrors `roles`. Allow and Deny are permission bitfields.
type RoleRow struct {
	ID          id.ID
	CommunityID id.ID
	Name        string
	Color       uint64
	Position    uint64
	Allow       uint64
	Deny        uint64
	Hoist       uint8
	Mentionable uint8
	Created     int64
}

// ChannelRow mirrors `channels`.
type ChannelRow struct {
	ID                id.ID
	CommunityID       *id.ID
	Kind              uint8
	Mode              uint8
	Visibility        uint8
	ParentID          *id.ID
	Name              string
	Topic             string
	Position          uint64
	SettingsJSON      []byte
	HostPolicyVersion uint64
	SlowmodeSeconds   uint64
	Seq               uint64
	Created           int64
	DeletedAt         *int64
}

// OverwriteRow mirrors `channel_overwrites`.
type OverwriteRow struct {
	ChannelID  id.ID
	TargetKind uint8
	TargetID   id.ID
	Allow      uint64
	Deny       uint64
}

// BanRow mirrors `bans`.
type BanRow struct {
	CommunityID id.ID
	UserID      id.ID
	Reason      string
	ByUser      id.ID
	Created     int64
	Expires     *int64
}

// VoiceSessionRow mirrors `voice_sessions`.
type VoiceSessionRow struct {
	CallID      id.ID
	ChannelID   id.ID
	GroupID     *id.ID
	LivekitRoom string
	Started     int64
	Ended       *int64
}

// ReadableMessageRow mirrors `readable_messages` (Plan 2 task 8, P2-D24). ID is
// the explicit rowid the FTS index is content-mapped to (gap-69 claim 2) and is
// assigned by the store: PutReadableMessage ignores the field and returns it.
// ChannelHex is the 32-char lowercase hex of ChannelID that the FTS5 MATCH
// filters on (P2-D1). Envelope is deterministic CBOR, not JSON text (P2-D27);
// Body is the only indexed text. FrankingKeyID names the instance franking key
// FrankingTag was made under, so a report still verifies after a rotation.
// UploaderDevice and CommitmentC complete protocol/04's stored tuple (Plan 2
// task 17): T binds the uploading device, not the sender's user id, and a
// report's first equation is checked against C. A row written before
// 00012_reports.sql has the all-zero device and a nil CommitmentC.
type ReadableMessageRow struct {
	ID             int64
	ChannelID      id.ID
	ChannelHex     string
	Seq            uint64
	Sender         id.ID
	Envelope       []byte
	Body           string
	FrankingTag    []byte
	FrankingKeyID  id.ID
	MentionCount   uint64
	Created        int64
	Edited         *int64
	Deleted        *int64
	UploaderDevice id.ID
	CommitmentC    []byte
}

// ReadableFranking is the franking tuple an edit re-franks a readable message
// with (Plan 2 tasks 9 and 17): the new tag, the id of the key that made it,
// the editing device and the edited envelope's C. They move together, or the
// stored tuple no longer recomputes to the stored tag.
type ReadableFranking struct {
	Tag            []byte
	KeyID          id.ID
	UploaderDevice id.ID
	CommitmentC    []byte
}

// BlobRow mirrors `blobs`. BlobID is the 32-byte content address.
type BlobRow struct {
	BlobID     []byte
	Size       uint64
	StorageRef string
	Created    int64
	UnrefSince *int64
}

// BlobRefRow mirrors `blob_refs` (Plan 2 task 10, P2-D17, P2-D24): one
// channel's publication of one blob. UploaderDevice is the device that made the
// reference; its user is the one the quota charges and the only one who may
// delete the reference.
type BlobRefRow struct {
	BlobID         []byte
	ChannelID      id.ID
	UploaderDevice id.ID
	Mime           string
	Created        int64
	// Confirmed is false while the uploader has not confirmed the reference (L-SQL-31).
	Confirmed bool
}

// BlobRetentionRow is one community's stored policy document, as the blob
// sweeper reads it to find the community's archival retention (Plan 2 task 11,
// R28). PolicyJSON is communities.policy_json byte for byte.
type BlobRetentionRow struct {
	CommunityID id.ID
	PolicyJSON  []byte
}

// BackupRow mirrors `backups` (interfaces.md §4.3), whose table is 008_blobs.sql
// and therefore Plan 2 task 10's to ship.
// DeviceID is NOT NULL with the all-zero id meaning "not device scoped"
// (Plan 2 P2-D32).
type BackupRow struct {
	UserID      id.ID
	Kind        uint64
	DeviceID    id.ID
	ChunkSeq    uint64
	BlobID      []byte
	ManifestSig []byte
	Created     int64
}

// MemberTimeoutRow mirrors community_member_timeouts.
type MemberTimeoutRow struct {
	CommunityID id.ID
	UserID      id.ID
	ByUser      id.ID
	Created     int64
	Expires     int64
}
