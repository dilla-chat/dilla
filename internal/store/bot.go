package store

import (
	"context"

	"github.com/jonasthim/dilla/internal/id"
)

// BotRuntimeRow mirrors bot_runtimes. TokenHash is SHA-256 of the entire dbr_ token.
type BotRuntimeRow struct {
	ID        id.ID
	TokenHash []byte
	Created   int64
	Rotated   int64
	RevokedAt *int64
}

// Bot installation state values are protocol/10's structural event enum.
const (
	BotInstallationPending   uint8 = 0
	BotInstallationEnrolling uint8 = 1
	BotInstallationJoining   uint8 = 2
	BotInstallationReady     uint8 = 3
	BotInstallationDisabled  uint8 = 4
	BotInstallationError     uint8 = 5
)

// BotInstallationRow mirrors bot_installations. RemovedAt is a durable tombstone;
// removed installations are never reactivated.
type BotInstallationRow struct {
	ID          id.ID
	CommunityID id.ID
	RuntimeID   *id.ID
	UserID      *id.ID
	DeviceID    *id.ID
	RoleID      *id.ID
	State       uint8
	Revision    uint64
	// Capabilities is the approved 10-bit mask: bits 0..7 are the original byte, bits 8 (polls)
	// and 9 (reminders) live in feature_capabilities (Task 10c).
	Capabilities         uint16
	ApprovedBy           id.ID
	ApprovedAt           int64
	CredentialVerifiedAt *int64
	Created              int64
	Updated              int64
	RemovedAt            *int64
	// Task 10c. The Music page's defaults, the restart counter pair and the runtime's reported
	// version. They are written only by SetBotMusicSettings, RequestBotRestart and
	// SetBotRuntimeStatus; Create and UpdateBotInstallation leave them to the database.
	MusicVolume      uint8
	MusicIdleMinutes uint8
	RestartRequested uint64
	RestartHandled   uint64
	RuntimeVersion   *string
}

// BotKnownCapabilities is every capability bit an installation may hold (Task 10c widened it
// from eight to ten bits).
const BotKnownCapabilities uint16 = 1<<10 - 1

// Music page values (Task 10c): exactly the prototype's options.
var (
	BotMusicVolumes     = []uint8{25, 50, 75, 100}
	BotMusicIdleMinutes = []uint8{5, 15, 30}
)

// BotGroupRegistrationReceipt is the immutable acceptance record for a managed-bot
// group registration. BodySHA256 hashes the exact canonical request body with the
// protocol's versioned registration domain separator.
type BotGroupRegistrationReceipt struct {
	InstallationID id.ID
	OperationID    id.ID
	UserID         id.ID
	DeviceID       id.ID
	CommunityID    id.ID
	GroupID        id.ID
	BodySHA256     []byte
	Generation     uint64
	Epoch          uint64
	NextSeq        uint64
	AcceptedAt     int64
}

// BotClaimRow is the one outstanding provisioning claim for an installation.
type BotClaimRow struct {
	InstallationID id.ID
	RuntimeID      id.ID
	ClaimID        id.ID
	RequestID      id.ID
	Expires        int64
}

// BotOperationRow is an immutable, scoped idempotency receipt. ResultCBOR is bounded to 8192 bytes.
type BotOperationRow struct {
	CommunityID    id.ID
	RequestID      id.ID
	InstallationID id.ID
	ActorKind      uint8
	ActorID        id.ID
	ActorDevice    *id.ID
	Operation      uint8
	PayloadHash    []byte
	ResultCBOR     []byte
	Created        int64
}

// BotSessionBindingRow binds a normal device session to exactly one installation identity.
type BotSessionBindingRow struct {
	TokenHash      []byte
	InstallationID id.ID
	UserID         id.ID
	DeviceID       id.ID
	Created        int64
}

// BotRuntimes stores only credential hashes. Rotation preserves the runtime ID and
// completed operation receipts while deleting outstanding claims.
type BotRuntimes interface {
	CreateBotRuntime(context.Context, BotRuntimeRow) error
	GetBotRuntime(context.Context, id.ID) (BotRuntimeRow, error)
	GetBotRuntimeByHash(context.Context, []byte) (BotRuntimeRow, error)
	// LockBotRuntime requires a Tx; callers recheck the authenticated token hash
	// after acquiring this lock so a rotation cannot race claim creation.
	LockBotRuntime(context.Context, id.ID) error
	RotateBotRuntime(context.Context, id.ID, []byte, []byte, int64) error
	RevokeBotRuntime(context.Context, id.ID, int64) error
	ListBotRuntimes(context.Context, id.ID, int32) ([]BotRuntimeRow, error)
}

// BotInstallations provides transaction-safe state and identity mutations.
type BotInstallations interface {
	CreateBotInstallation(context.Context, BotInstallationRow) error
	GetBotInstallation(context.Context, id.ID) (BotInstallationRow, error)
	GetActiveBotInstallationByCommunity(context.Context, id.ID) (BotInstallationRow, error)
	ListBotInstallationsAfter(context.Context, id.ID, int32) ([]BotInstallationRow, error)
	ListBotInstallationsByDevice(context.Context, id.ID, id.ID, int32) ([]BotInstallationRow, error)
	// ListBotInstallationsForRuntime filters to visible pending or runtime-owned rows before limit.
	ListBotInstallationsForRuntime(context.Context, id.ID, id.ID, int32) ([]BotInstallationRow, error)
	UpdateBotInstallation(context.Context, id.ID, uint64, BotInstallationRow) error
	// SetBotMusicSettings stores the Music page's defaults on a non-removed installation.
	SetBotMusicSettings(ctx context.Context, installationID id.ID, volume, idleMinutes uint8) error
	// RequestBotRestart asks the runtime to restart the installation and returns the request
	// counter. A request while an earlier one is still unhandled coalesces into it.
	RequestBotRestart(ctx context.Context, installationID id.ID) (uint64, error)
	// SetBotRuntimeStatus records the owning runtime's version and the last restart request it
	// finished (never above the requested counter, never backwards).
	SetBotRuntimeStatus(ctx context.Context, installationID, runtimeID id.ID, version *string, handled uint64) error
	LockBotInstallation(context.Context, id.ID) error
	LockBotCommunity(context.Context, id.ID) error
}

type BotClaims interface {
	GetBotClaim(context.Context, id.ID) (BotClaimRow, error)
	// PutBotClaim atomically creates or replaces the installation's current claim.
	// The caller must hold the installation lock and verify that RuntimeID matches
	// its active runtime and that the installation is not removed in the same Tx.
	PutBotClaim(context.Context, BotClaimRow) error
	DeleteBotClaim(context.Context, id.ID) error
	DeleteBotClaimsByRuntime(context.Context, id.ID) (int64, error)
}

type BotOperations interface {
	CreateBotOperation(context.Context, BotOperationRow) error
	GetBotOperation(context.Context, id.ID, id.ID) (BotOperationRow, error)
}

type BotSessionBindings interface {
	CreateBotSessionBinding(context.Context, BotSessionBindingRow) error
	GetBotSessionBinding(context.Context, []byte) (BotSessionBindingRow, error)
	DeleteBotSessionBindingsByInstallation(context.Context, id.ID) (int64, error)
}
