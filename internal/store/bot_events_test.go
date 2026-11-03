package store_test

import (
	"context"
	"errors"
	"math"
	"sync"
	"testing"

	"github.com/dilla-chat/dilla/internal/bot/protocol"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

func structuralEvent(community, eventID id.ID, timestamp int64) protocol.Event {
	return protocol.Event{ID: eventID, CommunityID: community, Timestamp: uint64(timestamp), Kind: protocol.EventMemberJoin, Data: []any{id.New(), id.New()}}
}

func appendStructuralEvent(ctx context.Context, repo store.Repository, e protocol.Event) (protocol.Event, error) {
	var got protocol.Event
	err := repo.Tx(ctx, func(tx store.Repository) error {
		var err error
		got, err = tx.AppendBotStructuralEvent(ctx, e)
		return err
	})
	return got, err
}

func TestBotStructuralEventsAppendPagesAndValidate(t *testing.T) {
	ctx := context.Background()
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			community := seedCommunity(ctx, t, repo)
			first, err := appendStructuralEvent(ctx, repo, structuralEvent(community, id.New(), 100))
			if err != nil || first.Sequence != 1 {
				t.Fatalf("first append = %+v, %v", first, err)
			}
			second, err := appendStructuralEvent(ctx, repo, structuralEvent(community, id.New(), 101))
			if err != nil || second.Sequence != 2 {
				t.Fatalf("second append = %+v, %v", second, err)
			}
			page, err := repo.ListBotStructuralEvents(ctx, community, 1, 250)
			if err != nil || len(page.Events) != 1 || page.Events[0].ID != second.ID || page.NextSequence != 2 || page.HighWater != 2 || page.Floor != 0 {
				t.Fatalf("page = %+v, %v", page, err)
			}
			duplicate := second
			duplicate.Sequence = 0
			if _, err := appendStructuralEvent(ctx, repo, duplicate); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("duplicate event id = %v, want conflict", err)
			}
			invalid := structuralEvent(community, id.New(), 102)
			invalid.Data = []any{"wrong type", id.New()}
			if _, err := appendStructuralEvent(ctx, repo, invalid); err == nil {
				t.Fatal("invalid event body was accepted")
			}
			if _, err := repo.ListBotStructuralEvents(ctx, community, 0, 251); err == nil {
				t.Fatal("oversized page limit was accepted")
			}
		})
	}
}

func TestBotStructuralEventAppendIsAtomicAndConcurrent(t *testing.T) {
	ctx := context.Background()
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			community := seedCommunity(ctx, t, repo)
			rollbackID := id.New()
			abort := errors.New("abort structural transaction")
			if err := repo.Tx(ctx, func(tx store.Repository) error {
				if _, err := tx.AppendBotStructuralEvent(ctx, structuralEvent(community, rollbackID, 100)); err != nil {
					return err
				}
				return abort
			}); !errors.Is(err, abort) {
				t.Fatalf("rollback = %v", err)
			}
			if _, err := appendStructuralEvent(ctx, repo, structuralEvent(community, rollbackID, 101)); err != nil {
				t.Fatalf("append after rollback = %v", err)
			}

			const n = 8
			start := make(chan struct{})
			results := make(chan uint64, n)
			errs := make(chan error, n)
			var wg sync.WaitGroup
			for i := 0; i < n; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					got, err := appendStructuralEvent(ctx, repo, structuralEvent(community, id.New(), 102))
					if err != nil {
						errs <- err
						return
					}
					results <- got.Sequence
				}()
			}
			close(start)
			wg.Wait()
			close(results)
			close(errs)
			for err := range errs {
				t.Fatalf("concurrent append: %v", err)
			}
			seen := map[uint64]bool{}
			for seq := range results {
				seen[seq] = true
			}
			if len(seen) != n {
				t.Fatalf("concurrent sequences = %v", seen)
			}
			for seq := uint64(2); seq <= n+1; seq++ {
				if !seen[seq] {
					t.Fatalf("concurrent sequence %d missing from %v", seq, seen)
				}
			}
		})
	}
}

func TestBotStructuralEventPruningAndInstallationAck(t *testing.T) {
	ctx := context.Background()
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			f := newSendFixture(ctx, t, repo)
			for i := int64(1); i <= 3; i++ {
				if _, err := appendStructuralEvent(ctx, repo, structuralEvent(f.community, id.New(), i*10)); err != nil {
					t.Fatal(err)
				}
			}
			if err := repo.Tx(ctx, func(tx store.Repository) error {
				return tx.AckBotStructuralEvents(ctx, f.installation.ID, f.community, 4)
			}); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("ack beyond high water = %v", err)
			}
			if err := repo.Tx(ctx, func(tx store.Repository) error {
				return tx.AckBotStructuralEvents(ctx, f.installation.ID, f.community, 2)
			}); err != nil {
				t.Fatal(err)
			}
			if err := repo.Tx(ctx, func(tx store.Repository) error {
				return tx.AckBotStructuralEvents(ctx, f.installation.ID, f.community, 1)
			}); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("ack regression = %v", err)
			}
			if got, err := repo.GetBotStructuralEventAck(ctx, f.installation.ID, f.community); err != nil || got != 2 {
				t.Fatalf("ack = %d, %v", got, err)
			}
			if _, err := repo.PruneBotStructuralEvents(ctx, 20); err == nil {
				t.Fatal("prune outside transaction succeeded")
			}
			if err := repo.Tx(ctx, func(tx store.Repository) error { _, err := tx.PruneBotStructuralEvents(ctx, 30); return err }); err != nil {
				t.Fatal(err)
			}
			page, err := repo.ListBotStructuralEvents(ctx, f.community, 0, 250)
			if err != nil || page.Floor != 2 || page.HighWater != 3 || len(page.Events) != 1 || page.Events[0].Sequence != 3 {
				t.Fatalf("after prune page = %+v, %v", page, err)
			}
			page, err = repo.ListBotStructuralEvents(ctx, f.community, 1, 250)
			if err != nil || page.Floor != 2 || len(page.Events) != 1 {
				t.Fatalf("page at floor predecessor = %+v, %v", page, err)
			}
			if err := repo.Tx(ctx, func(tx store.Repository) error {
				return tx.AckBotStructuralEvents(ctx, f.installation.ID, f.community, math.MaxUint64)
			}); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("ack invalid high water = %v", err)
			}
		})
	}
}

func TestBotStructuralPruningKeepsFloorContiguous(t *testing.T) {
	ctx := context.Background()
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			community := seedCommunity(ctx, t, repo)
			for _, timestamp := range []int64{10, 50, 20} {
				if _, err := appendStructuralEvent(ctx, repo, structuralEvent(community, id.New(), timestamp)); err != nil {
					t.Fatal(err)
				}
			}
			var deleted int64
			if err := repo.Tx(ctx, func(tx store.Repository) error {
				var err error
				deleted, err = tx.PruneBotStructuralEvents(ctx, 30)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			page, err := repo.ListBotStructuralEvents(ctx, community, 0, 250)
			if err != nil || deleted != 1 || page.Floor != 1 || len(page.Events) != 2 || page.Events[0].Sequence != 2 || page.Events[1].Sequence != 3 {
				t.Fatalf("prune deleted=%d page=%+v err=%v", deleted, page, err)
			}
		})
	}
}
