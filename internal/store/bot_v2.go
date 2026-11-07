package store

import (
	"bytes"
	"cmp"
	"context"
	"slices"

	"github.com/dilla-chat/dilla/internal/id"
)

// Bot v2 (Task 9, 051_bot_v2.sql): the command catalog, the Bot's command-key publication, Bot
// channels, the Bot state revisions and (Task 13) incoming webhooks.

// BotCatalogRow mirrors bot_catalogs. Body is the deterministic-CBOR commands list the Bot
// published; Revision only grows.
type BotCatalogRow struct {
	CommunityID id.ID
	Revision    uint64
	Body        []byte
}

// BotCommandKeyRow mirrors bot_command_keys: the Bot's signed publication, stored as received after
// dillad checked it.
type BotCommandKeyRow struct {
	CommunityID id.ID
	KeyID       id.ID
	Publication []byte
	PublishedAt int64
}

// BotChannelRow mirrors bot_channels.
type BotChannelRow struct {
	ChannelID   id.ID
	CommunityID id.ID
	MarkedAt    int64
	MarkedBy    id.ID
}

// BotWebhookRow mirrors bot_webhooks (Bot v2 Task 13). SecretHash is SHA-256 of the webhook's secret: the
// secret itself is never stored. AvatarBlob is an opaque 32-byte blob id or nil; dillad never dereferences it.
type BotWebhookRow struct {
	WebhookID     id.ID
	CommunityID   id.ID
	ChannelID     id.ID
	Name          string
	AvatarBlob    []byte
	SecretHash    []byte
	AllowEveryone bool
	CreatedAt     int64
	RotatedAt     *int64
}

// The Bot state scopes (spec "Live updates"). A call scope's id is the voice channel the call is
// in, so a member's visibility of the channel decides whether it learns the call's revision.
const (
	BotScopeUser    uint8 = 0
	BotScopeChannel uint8 = 1
	BotScopeCall    uint8 = 2
)

// BotStateRevisionRow is one stored scope revision. Every scope belongs to one community: a user
// scope's revision is that user's in CommunityID only (Bot v2 Task 10), and a channel or call
// scope's CommunityID is its channel's.
type BotStateRevisionRow struct {
	ScopeKind   uint8
	ScopeID     id.ID
	CommunityID id.ID
	Revision    uint64
}

// SortBotStateRevisions orders rows by (kind, scope id), the order BotStateRevisions answers in on
// both engines.
func SortBotStateRevisions(rows []BotStateRevisionRow) {
	slices.SortFunc(rows, func(a, b BotStateRevisionRow) int {
		if c := cmp.Compare(a.ScopeKind, b.ScopeKind); c != 0 {
			return c
		}
		if c := bytes.Compare(a.ScopeID[:], b.ScopeID[:]); c != 0 {
			return c
		}
		return bytes.Compare(a.CommunityID[:], b.CommunityID[:])
	})
}

// BotV2 is the Bot v2 server state: nothing in it is command content.
type BotV2 interface {
	// PutBotCatalog stores the community's catalog at the given revision (> 0), replacing the
	// previous one. The caller decides the revision under the community lock.
	PutBotCatalog(ctx context.Context, row BotCatalogRow) error
	// GetBotCatalog is ErrNotFound before the Bot first publishes.
	GetBotCatalog(ctx context.Context, community id.ID) (BotCatalogRow, error)
	PutBotCommandKey(ctx context.Context, row BotCommandKeyRow) error
	// GetBotCommandKey is ErrNotFound before the Bot first publishes.
	GetBotCommandKey(ctx context.Context, community id.ID) (BotCommandKeyRow, error)
	// MarkBotChannel reports whether the channel was newly marked; an existing mark is kept as is.
	MarkBotChannel(ctx context.Context, row BotChannelRow) (bool, error)
	// UnmarkBotChannel reports whether a mark was removed.
	UnmarkBotChannel(ctx context.Context, community, channel id.ID) (bool, error)
	// ListBotChannels is the community's marked channels that are still live end-to-end text
	// channels, oldest mark first.
	ListBotChannels(ctx context.Context, community id.ID) ([]BotChannelRow, error)
	// BumpBotStateRevision raises one scope's revision in one community by one (the first bump is
	// 1) and returns it.
	BumpBotStateRevision(ctx context.Context, kind uint8, community, scope id.ID) (uint64, error)
	// BotStateRevisions is the user's own revision in each community that holds one (kind 0 rows,
	// ScopeID the user) and the stored channel and call revisions of exactly the given scope ids,
	// ordered by (kind, scope id, community).
	BotStateRevisions(ctx context.Context, user id.ID, scopes []id.ID) ([]BotStateRevisionRow, error)
	// CreateBotWebhook stores a new webhook; RotatedAt is ignored (a new webhook was never rotated).
	CreateBotWebhook(ctx context.Context, row BotWebhookRow) error
	// GetBotWebhook is ErrNotFound for an unknown id.
	GetBotWebhook(ctx context.Context, webhook id.ID) (BotWebhookRow, error)
	// ListBotWebhooks is the community's webhooks, oldest first.
	ListBotWebhooks(ctx context.Context, community id.ID) ([]BotWebhookRow, error)
	// UpdateBotWebhook replaces the name, avatar and allow_everyone of a webhook of community and reports
	// whether one was found.
	UpdateBotWebhook(ctx context.Context, row BotWebhookRow) (bool, error)
	// RotateBotWebhookSecret replaces a webhook's secret hash and sets rotated_at, in one statement, and
	// reports whether one was found.
	RotateBotWebhookSecret(ctx context.Context, community, webhook id.ID, secretHash []byte, at int64) (bool, error)
	// DeleteBotWebhook reports whether a webhook of community was removed.
	DeleteBotWebhook(ctx context.Context, community, webhook id.ID) (bool, error)
	// DeleteBotWebhooksOfChannel removes every webhook that targets channel: a channel's soft delete fires no
	// cascade.
	DeleteBotWebhooksOfChannel(ctx context.Context, channel id.ID) error
	// DeleteBotCommunityState removes a deleted community's Bot channels, catalog, command key,
	// webhooks and state revisions, inside the caller's transaction, and returns the channels it
	// unmarked.
	DeleteBotCommunityState(ctx context.Context, community id.ID) ([]id.ID, error)
}
