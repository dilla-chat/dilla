package store_test

import (
	"bytes"
	"context"
	"slices"
	"testing"

	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

// Plan 1 follow-up card 8 (deviation B22, ruling 43), taken by Plan 2 task 7: the tail of a join
// storm is a table, pending_joins, so a restart between a 1,000-device batch and the commit that
// applies its first 256 Adds no longer drops the other 744 silently. On both engines.
func TestPendingJoinsQueueDurablyAndDrainInOrder(t *testing.T) {
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			ctx := context.Background()
			g := seedGroup(ctx, t, repo, 1_000)
			other := seedGroup(ctx, t, repo, 1_000)

			early := []id.ID{id.New(), id.New(), id.New()}
			late := []id.ID{id.New(), id.New()}
			if err := repo.QueuePendingJoins(ctx, g.GroupID, early, 100); err != nil {
				t.Fatalf("QueuePendingJoins: %v", err)
			}
			// A device already queued keeps its place: a second queue of it is a no-op, not a
			// conflict and not a move to the back.
			if err := repo.QueuePendingJoins(ctx, g.GroupID, append([]id.ID{early[0]}, late...), 200); err != nil {
				t.Fatalf("QueuePendingJoins (again): %v", err)
			}
			if err := repo.QueuePendingJoins(ctx, other.GroupID, []id.ID{id.New()}, 100); err != nil {
				t.Fatalf("QueuePendingJoins (other group): %v", err)
			}
			if n, err := repo.CountPendingJoins(ctx, g.GroupID); err != nil || n != 5 {
				t.Fatalf("CountPendingJoins = %d, %v; want 5", n, err)
			}

			// Oldest first, ties broken by device id, so the order is the same on both engines.
			byID := func(ids []id.ID) []id.ID {
				out := slices.Clone(ids)
				slices.SortFunc(out, func(a, b id.ID) int { return bytes.Compare(a[:], b[:]) })
				return out
			}
			want := append(byID(early), byID(late)...)

			first, err := repo.ListPendingJoins(ctx, g.GroupID, 4)
			if err != nil {
				t.Fatalf("ListPendingJoins: %v", err)
			}
			if !slices.Equal(first, want[:4]) {
				t.Fatalf("first read = %x, want %x", first, want[:4])
			}
			// A read removes NOTHING: a drain that dies after reading a slice must leave every
			// device it had not resolved in the queue.
			if again, err := repo.ListPendingJoins(ctx, g.GroupID, 4); err != nil || !slices.Equal(again, want[:4]) {
				t.Fatalf("a second read = %x, %v; want the same %x", again, err, want[:4])
			}
			// A device leaves only when it is deleted, one resolved device at a time or several.
			if err := repo.DeletePendingJoins(ctx, g.GroupID, first[:1]); err != nil {
				t.Fatalf("DeletePendingJoins: %v", err)
			}
			if err := repo.DeletePendingJoins(ctx, g.GroupID, first[1:]); err != nil {
				t.Fatalf("DeletePendingJoins: %v", err)
			}
			rest, err := repo.ListPendingJoins(ctx, g.GroupID, 4)
			if err != nil {
				t.Fatalf("ListPendingJoins: %v", err)
			}
			if !slices.Equal(rest, want[4:]) {
				t.Fatalf("after the deletes = %x, want %x", rest, want[4:])
			}
			// Deleting a device that is not queued is not an error.
			if err := repo.DeletePendingJoins(ctx, g.GroupID, append(rest, first[0])); err != nil {
				t.Fatalf("DeletePendingJoins (with one already gone): %v", err)
			}
			if got, err := repo.ListPendingJoins(ctx, g.GroupID, 4); err != nil || len(got) != 0 {
				t.Fatalf("a drained queue read %x, %v; want nothing", got, err)
			}
			if n, err := repo.CountPendingJoins(ctx, other.GroupID); err != nil || n != 1 {
				t.Fatalf("the other group's queue = %d, %v; want its own 1", n, err)
			}
		})
	}
}

// The sweeper finds the groups with a queue in pages, so a stalled storm is re-driven on an
// instance with more groups than one page.
func TestListPendingJoinGroupsPagesByGroupID(t *testing.T) {
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			ctx := context.Background()
			var queued []id.ID
			for range 3 {
				g := seedGroup(ctx, t, repo, 1_000)
				if err := repo.QueuePendingJoins(ctx, g.GroupID, []id.ID{id.New(), id.New()}, 100); err != nil {
					t.Fatalf("QueuePendingJoins: %v", err)
				}
				queued = append(queued, g.GroupID)
			}
			seedGroup(ctx, t, repo, 1_000) // no queue: never listed
			slices.SortFunc(queued, func(a, b id.ID) int { return bytes.Compare(a[:], b[:]) })

			page, err := repo.ListPendingJoinGroups(ctx, id.ID{}, 2)
			if err != nil {
				t.Fatalf("ListPendingJoinGroups: %v", err)
			}
			if !slices.Equal(page, queued[:2]) {
				t.Fatalf("first page = %x, want %x (one row per group, not per device)", page, queued[:2])
			}
			page, err = repo.ListPendingJoinGroups(ctx, page[len(page)-1], 2)
			if err != nil {
				t.Fatalf("ListPendingJoinGroups: %v", err)
			}
			if !slices.Equal(page, queued[2:]) {
				t.Fatalf("second page = %x, want %x", page, queued[2:])
			}
		})
	}
}

// The queue references its group (ON DELETE CASCADE), so a queue for a group the instance does not
// hold is refused rather than stored where no drain will ever look.
func TestPendingJoinsReferenceTheirGroup(t *testing.T) {
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			ctx := context.Background()
			if err := repo.QueuePendingJoins(ctx, id.New(), []id.ID{id.New()}, 100); err == nil {
				t.Fatal("a queue for a group that does not exist was accepted")
			}
		})
	}
}

var _ interface {
	QueuePendingJoins(ctx context.Context, groupID id.ID, devices []id.ID, at int64) error
} = store.MLS(nil)
