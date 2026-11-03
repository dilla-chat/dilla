package native

import (
	"context"
	"errors"
	"math"

	"github.com/dilla-chat/dilla/internal/bot/protocol"
	"github.com/dilla-chat/dilla/internal/id"
)

// Leave room for CBOR framing, the proof tuple, and the helper request envelope.
const maxRemovalActorHistoryBytes = protocol.MaxHelperBytes - 16*1024

func validateRemovalActorEntry(c *Client, group id.ID, entry RemovalFeedEntry) error {
	if group.IsZero() || entry.AcceptedAt == 0 || entry.AcceptedAt > math.MaxInt64 || len(entry.Signature) != 64 {
		return errors.New("invalid removal actor entry")
	}
	proof, err := protocol.DecodeRemovalProof(entry.Proof)
	if err != nil || proof.GroupID != group || proof.CommunityID != c.binding.Community || proof.Expires <= entry.AcceptedAt || proof.Expires-entry.AcceptedAt > 30 {
		return errors.New("removal proof does not match the bound feed")
	}
	return nil
}

// RemovalActorHistoryVersion returns the already trusted list version to use as the starting
// point for an authenticated DeviceListHistory fetch. This is only a fetch hint; proof signature
// verification happens when RefreshRemovalActorHistory validates the returned chain.
func (c *Client) RemovalActorHistoryVersion(ctx context.Context, group id.ID, entry RemovalFeedEntry) (uint64, error) {
	if err := validateRemovalActorEntry(c, group, entry); err != nil {
		return 0, err
	}
	payload := []any{group, []any{entry.Proof, entry.Signature, entry.AcceptedAt}, nil, nil}
	v, err := c.call(ctx, protocol.HelperRefreshRemovalActorHistory, payload, false, decodeRemovalActorVersion)
	if err != nil {
		return 0, err
	}
	return resultAs[uint64](v)
}

// RefreshRemovalActorHistory validates and caches a complete signed-list chain for a previously
// trusted removal actor, including former group members whose MLS leaf is no longer present.
func (c *Client) RefreshRemovalActorHistory(ctx context.Context, group id.ID, entry RemovalFeedEntry, history, head []byte) (uint64, error) {
	if err := validateRemovalActorEntry(c, group, entry); err != nil {
		return 0, err
	}
	if len(history) == 0 || len(head) == 0 || len(history)+len(head) > maxRemovalActorHistoryBytes {
		return 0, errors.New("invalid removal actor history bounds")
	}
	payload := []any{group, []any{entry.Proof, entry.Signature, entry.AcceptedAt}, append([]byte(nil), history...), append([]byte(nil), head...)}
	v, err := c.call(ctx, protocol.HelperRefreshRemovalActorHistory, payload, true, decodeRemovalActorVersion)
	if err != nil {
		return 0, err
	}
	return resultAs[uint64](v)
}

func decodeRemovalActorVersion(result []any) (any, error) {
	if len(result) != 1 {
		return nil, errors.New("removal actor history result shape")
	}
	version, ok := result[0].(uint64)
	if !ok || version == 0 || version > math.MaxInt64 {
		return nil, errors.New("removal actor history version")
	}
	return version, nil
}

// RemovalFeedEntry is one proof-bearing row from the server-authoritative group feed.
type RemovalFeedEntry struct {
	Revision   uint64
	Proof      []byte
	Signature  []byte
	AcceptedAt uint64
}

// IngestRemovals validates group binding and proof structure before handing signed entries to
// the native core, which verifies device-list attribution and commits tombstones atomically.
func (c *Client) IngestRemovals(ctx context.Context, group id.ID, entries []RemovalFeedEntry) (uint64, error) {
	if group.IsZero() || len(entries) > 256 {
		return 0, errors.New("invalid removal feed page")
	}
	rows := make([]any, 0, len(entries))
	var previous uint64
	for i, entry := range entries {
		if entry.Revision == 0 || entry.Revision > math.MaxInt64 || entry.AcceptedAt == 0 || entry.AcceptedAt > math.MaxInt64 || len(entry.Signature) != 64 {
			return 0, errors.New("invalid removal feed entry")
		}
		if i > 0 && entry.Revision != previous+1 {
			return 0, errors.New("nonconsecutive removal feed page")
		}
		proof, err := protocol.DecodeRemovalProof(entry.Proof)
		if err != nil || proof.GroupID != group || proof.CommunityID != c.binding.Community || proof.Expires <= entry.AcceptedAt || proof.Expires-entry.AcceptedAt > 30 {
			return 0, errors.New("removal proof does not match the bound feed")
		}
		previous = entry.Revision
		rows = append(rows, []any{entry.Revision, append([]byte(nil), entry.Proof...), append([]byte(nil), entry.Signature...), entry.AcceptedAt})
	}
	payload := []any{group, rows}
	v, err := c.call(ctx, protocol.HelperIngestRemovals, payload, true, func(result []any) (any, error) {
		if len(result) != 1 {
			return nil, errors.New("removal ingest result shape")
		}
		cursor, ok := result[0].(uint64)
		if !ok || cursor > math.MaxInt64 {
			return nil, errors.New("removal ingest cursor")
		}
		return cursor, nil
	})
	if err != nil {
		return 0, err
	}
	return resultAs[uint64](v)
}
