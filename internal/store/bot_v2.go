package store

import (
	"bytes"
	"cmp"
	"context"
	"slices"

	"github.com/dilla-chat/dilla/internal/id"
)

// Bot v2 (Task 9, 051_bot_v2.sql): the command catalog, the Bot's command-key publication, Bot
// channels and the Bot state revisions. bot_webhooks ships in the same migration; its methods come
// with the webhook routes.

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

// The Bot state scopes (spec "Live updates"). A call scope's id is the voice channel the call is
// in, so a member's visibility of the channel decides whether it learns the call's revision.
const (
	BotScopeUser    uint8 = 0
	BotScopeChannel uint8 = 1
	BotScopeCall    uint8 = 2
)

// BotStateRevisionRow is one stored scope revision.
type BotStateRevisionRow struct {
	ScopeKind uint8
	ScopeID   id.ID
	Revision  uint64
}

// SortBotStateRevisions orders rows by (kind, scope id), the order BotStateRevisions answers in on
// both engines.
func SortBotStateRevisions(rows []BotStateRevisionRow) {
	slices.SortFunc(rows, func(a, b BotStateRevisionRow) int {
		if c := cmp.Compare(a.ScopeKind, b.ScopeKind); c != 0 {
			return c
		}
		return bytes.Compare(a.ScopeID[:], b.ScopeID[:])
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
	// BumpBotStateRevision raises one scope's revision by one (the first bump is 1) and returns it.
	BumpBotStateRevision(ctx context.Context, kind uint8, scope id.ID) (uint64, error)
	// BotStateRevisions is the user's own revision (0 when none) and the stored channel and call
	// revisions of exactly the given scope ids, ordered by (kind, id).
	BotStateRevisions(ctx context.Context, user id.ID, scopes []id.ID) (uint64, []BotStateRevisionRow, error)
	// DeleteBotCommunityState removes a deleted community's Bot channels, catalog, command key and
	// webhooks, inside the caller's transaction, and returns the channels it unmarked.
	DeleteBotCommunityState(ctx context.Context, community id.ID) ([]id.ID, error)
}
