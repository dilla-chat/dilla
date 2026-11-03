package service

import (
	"context"
	"errors"
	"math"

	"github.com/dilla-chat/dilla/internal/bot/client"
	"github.com/dilla-chat/dilla/internal/bot/native"
	"github.com/dilla-chat/dilla/internal/bot/protocol"
	"github.com/dilla-chat/dilla/internal/id"
)

const removalSyncPageLimit = uint64(250)
const removalSyncPageBudget = 4
const removalHistoryHelperBudget = 900 << 10

type removalSyncNative interface {
	Binding() native.Binding
	InspectGroup(context.Context, id.ID) (native.GroupInfo, error)
	IngestRemovals(context.Context, id.ID, []native.RemovalFeedEntry) (uint64, error)
	RemovalActorHistoryVersion(context.Context, id.ID, native.RemovalFeedEntry) (uint64, error)
	RefreshRemovalActorHistory(context.Context, id.ID, native.RemovalFeedEntry, []byte, []byte) (uint64, error)
}

type removalSyncSession interface {
	senderHistorySession
	ModerationRemovals(context.Context, id.ID, uint64, uint64) (client.ModerationRemovalPage, error)
}

// RemovalSync applies signed, server-accepted tombstones before a caller
// publishes a recovered timeline. The native core verifies each actor's
// signed device list and proof before persisting the cursor.
type RemovalSync struct {
	native  removalSyncNative
	binding native.Binding
}

func NewRemovalSync(n *native.Client) (*RemovalSync, error) { return newRemovalSync(n) }

func newRemovalSync(n removalSyncNative) (*RemovalSync, error) {
	if n == nil {
		return nil, errors.New("missing removal sync source")
	}
	b := n.Binding()
	if b.Instance.IsZero() || b.Community.IsZero() || b.Installation.IsZero() {
		return nil, errors.New("invalid removal sync binding")
	}
	return &RemovalSync{native: n, binding: b}, nil
}

// Sync makes a bounded pass over one active community text group. More means a
// full page could remain; the caller must run another pass before publishing.
func (s *RemovalSync) Sync(ctx context.Context, session *client.Session, group id.ID) (cursor uint64, more bool, err error) {
	if session == nil {
		return 0, false, errors.New("missing removal feed session")
	}
	return s.sync(ctx, session, group)
}

func (s *RemovalSync) sync(ctx context.Context, session removalSyncSession, group id.ID) (cursor uint64, more bool, err error) {
	if s == nil || ctx == nil || session == nil || group.IsZero() {
		return 0, false, errors.New("invalid removal sync target")
	}
	info, err := s.native.InspectGroup(ctx, group)
	if err != nil {
		return 0, false, err
	}
	if info.ID != group || info.Kind != 0 || info.State != 2 || info.Community == nil || *info.Community != s.binding.Community || info.PendingCommit {
		return 0, false, errors.New("removal group is not an active bound text group")
	}
	cursor, err = s.native.IngestRemovals(ctx, group, nil)
	if err != nil || cursor > math.MaxInt64 {
		return 0, false, errors.New("removal cursor unavailable")
	}
	for pageNo := 0; pageNo < removalSyncPageBudget; pageNo++ {
		if err := ctx.Err(); err != nil {
			return cursor, false, err
		}
		page, err := session.ModerationRemovals(ctx, group, cursor, removalSyncPageLimit)
		if err != nil {
			return cursor, false, err
		}
		if len(page.Rows) == 0 {
			if page.NextRevision != cursor {
				return cursor, false, errors.New("empty removal page advanced cursor")
			}
			return cursor, false, nil
		}
		if len(page.Rows) > int(removalSyncPageLimit) || page.NextRevision != cursor+uint64(len(page.Rows)) {
			return cursor, false, errors.New("removal page has an invalid revision range")
		}
		entries := make([]native.RemovalFeedEntry, len(page.Rows))
		for i, row := range page.Rows {
			if row.Revision != cursor+uint64(i)+1 {
				return cursor, false, errors.New("removal page has a revision gap")
			}
			entries[i] = native.RemovalFeedEntry{Revision: row.Revision, Proof: row.Proof, Signature: row.Signature, AcceptedAt: row.AcceptedAt}
		}
		if err := s.refreshActors(ctx, session, group, entries); err != nil {
			return cursor, false, err
		}
		persisted, err := s.native.IngestRemovals(ctx, group, entries)
		if err != nil {
			return cursor, false, err
		}
		if persisted != page.NextRevision {
			return cursor, false, errors.New("native removal cursor did not match server page")
		}
		cursor = persisted
		if len(page.Rows) < int(removalSyncPageLimit) {
			return cursor, false, nil
		}
	}
	return cursor, true, nil
}

// refreshActors extends only an existing native trust anchor. The server's
// history response is untrusted until the core validates its complete signed
// chain and the proof. A missing anchor therefore blocks the page.
func (s *RemovalSync) refreshActors(ctx context.Context, session removalSyncSession, group id.ID, entries []native.RemovalFeedEntry) error {
	seen := make(map[id.ID]bool, len(entries))
	for _, entry := range entries {
		proof, err := protocol.DecodeRemovalProof(entry.Proof)
		if err != nil || proof.GroupID != group || proof.CommunityID != s.binding.Community {
			return errors.New("removal proof has an invalid binding")
		}
		if seen[proof.ActorUser] {
			continue
		}
		seen[proof.ActorUser] = true
		version, err := s.native.RemovalActorHistoryVersion(ctx, group, entry)
		if err != nil {
			return err
		}
		history, head, _, err := readSenderHistory(ctx, session, proof.ActorUser, version)
		if err != nil {
			return err
		}
		if len(history)+len(head)+len(entry.Proof) > removalHistoryHelperBudget {
			return ErrSenderHistoryStale
		}
		if _, err := s.native.RefreshRemovalActorHistory(ctx, group, entry, history, head); err != nil {
			return err
		}
	}
	return nil
}
