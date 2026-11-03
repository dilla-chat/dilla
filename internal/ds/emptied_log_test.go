package ds_test

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/ds"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

// Invariant 10's catch-up when the sweep has taken EVERY row of a stream. The floor both catch-ups
// compare against is the oldest SURVIVING seq, and an emptied log has none: without this case the
// predicate reads a floor of 0, refuses nothing, and a device away for longer than the retention
// window is served an empty page — "nothing new" — for a log that has lost everything it missed.
// retention_prune_then_resync is the same device end to end.

func TestACatchUpIntoAnEmptiedHandshakeLogIsPruned(t *testing.T) {
	h := newDSHarness(t)
	ctx := context.Background()
	reg, session := h.mustRegister(t)
	burnSeq(t, h, reg.GroupID)
	h.appendHandshake(t, reg.GroupID, 1, 6, 1, []byte("swept"))
	h.clk.Advance(31 * 24 * time.Hour)
	cutoff := h.clk.Now().Add(-ds.DefaultPolicy().HandshakeRetention).Unix()
	if gone, err := h.repo.PruneHandshakes(ctx, cutoff); err != nil || gone != 1 {
		t.Fatalf("PruneHandshakes: %d, %v; the log must really be empty", gone, err)
	}

	_, err := h.ds.Handshakes(ctx, reg.GroupID, session, 1, 100)
	var dsErr *ds.Error
	if !errors.As(err, &dsErr) || dsErr.Code != "E_PRUNED" {
		t.Fatalf("a catch-up into an emptied log: got %v, want E_PRUNED", err)
	}
	// A caller already past everything the group issued has lost nothing and is served.
	if rows, err := h.ds.Handshakes(ctx, reg.GroupID, session, 2, 100); err != nil || len(rows) != 0 {
		t.Fatalf("a catch-up past the head: %d rows, %v; want an empty page", len(rows), err)
	}
}

func TestACatchUpIntoAnEmptiedMessageLogIsPruned(t *testing.T) {
	h := newDSHarness(t)
	g := h.group(t)
	ctx := context.Background()
	first, err := h.ds.Upload(ctx, g.session, g.id, g.Epoch(), h.message(t, g, g.Epoch()), 0)
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	h.clk.Advance(31 * 24 * time.Hour)
	n, err := h.repo.PruneAppMessages(ctx, g.id, 0,
		h.clk.Now().Add(-ds.DefaultPolicy().MessageRetention).Unix(), h.clk.Now().Unix())
	if err != nil || n != 1 {
		t.Fatalf("PruneAppMessages: %d, %v; the log must really be empty", n, err)
	}

	_, err = h.ds.Messages(ctx, g.id, g.session, first.Seq, 10)
	var dsErr *ds.Error
	if !errors.As(err, &dsErr) || dsErr.Code != "E_PRUNED" {
		t.Fatalf("a catch-up into an emptied log: got %v, want E_PRUNED", err)
	}
	if rows, err := h.ds.Messages(ctx, g.id, g.session, first.Seq+1, 10); err != nil || len(rows) != 0 {
		t.Fatalf("a catch-up past the head: %d rows, %v; want an empty page", len(rows), err)
	}
}

// burnSeq takes one seq of the group's space the way a handshake does, so the group's high-water
// covers the row the test then writes directly.
func burnSeq(t *testing.T, h *dsHarness, groupID id.ID) {
	t.Helper()
	if err := h.repo.Tx(context.Background(), func(tx store.Repository) error {
		_, err := tx.NextSeq(context.Background(), groupID)
		return err
	}); err != nil {
		t.Fatalf("NextSeq: %v", err)
	}
}

// `from` is the client's, a uint64 on the wire, and the floor check does `from+1`. At 2^64-1 that
// wraps to 0, which is below every floor, so a caller asking for the rows AFTER a seq no group has
// ever reached was told E_PRUNED — a "resync by external commit" for a log with no hole in it. A
// cursor past the head is an empty page, whatever its width.
func TestACatchUpFromTheTopOfTheUint64RangeIsAnEmptyPageNotPruned(t *testing.T) {
	t.Run("handshakes", func(t *testing.T) {
		h := newDSHarness(t)
		ctx := context.Background()
		reg, session := h.mustRegister(t)
		h.appendHandshake(t, reg.GroupID, 1, 6, 1, []byte("swept"))
		h.clk.Advance(31 * 24 * time.Hour)
		h.appendHandshake(t, reg.GroupID, 20, 7, 1, []byte("commit"))
		cutoff := h.clk.Now().Add(-ds.DefaultPolicy().HandshakeRetention).Unix()
		if gone, err := h.repo.PruneHandshakes(ctx, cutoff); err != nil || gone != 1 {
			t.Fatalf("PruneHandshakes: %d, %v; the floor must sit above zero", gone, err)
		}
		for _, from := range []uint64{math.MaxInt64, math.MaxInt64 + 1, math.MaxUint64} {
			rows, err := h.ds.Handshakes(ctx, reg.GroupID, session, from, 100)
			if err != nil || len(rows) != 0 {
				t.Errorf("from=%d: %d rows, %v; want an empty page", from, len(rows), err)
			}
		}
	})
	t.Run("messages", func(t *testing.T) {
		h := newDSHarness(t)
		g := h.group(t)
		ctx := context.Background()
		if _, err := h.ds.Upload(ctx, g.session, g.id, g.Epoch(), h.message(t, g, g.Epoch()), 0); err != nil {
			t.Fatalf("Upload: %v", err)
		}
		h.clk.Advance(31 * 24 * time.Hour)
		if _, err := h.ds.Upload(ctx, g.session, g.id, g.Epoch(), h.message(t, g, g.Epoch()), 0); err != nil {
			t.Fatalf("Upload: %v", err)
		}
		n, err := h.repo.PruneAppMessages(ctx, g.id, 0,
			h.clk.Now().Add(-ds.DefaultPolicy().MessageRetention).Unix(), h.clk.Now().Unix())
		if err != nil || n != 1 {
			t.Fatalf("PruneAppMessages: %d, %v; the floor must sit above zero", n, err)
		}
		for _, from := range []uint64{math.MaxInt64, math.MaxInt64 + 1, math.MaxUint64} {
			rows, err := h.ds.Messages(ctx, g.id, g.session, from, 10)
			if err != nil || len(rows) != 0 {
				t.Errorf("from=%d: %d rows, %v; want an empty page", from, len(rows), err)
			}
		}
	})
}
