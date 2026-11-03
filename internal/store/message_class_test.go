package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

func classPtr(v uint8) *uint8 { return &v }

// LastClassZeroUploadAt reads slow mode's durable mark for end-to-end encrypted posts (web3.7 SB1,
// fix round 1): PutSlowmodeMark raises it, never lowers it, per (channel, user). No app message row
// is involved, so neither a tombstone nor a retention prune of the post moves it.
func TestTheSlowModeMark(t *testing.T) {
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			ctx := context.Background()
			channel, other := seedChannel(ctx, t, repo), seedChannel(ctx, t, repo)
			user := seedUser(ctx, t, repo).ID
			stranger := seedUser(ctx, t, repo).ID
			want := func(label string, ch, u id.ID, at int64) {
				t.Helper()
				got, err := repo.LastClassZeroUploadAt(ctx, ch, u)
				if err != nil || got != at {
					t.Fatalf("%s: LastClassZeroUploadAt = %d, %v; want %d", label, got, err, at)
				}
			}
			if _, err := repo.LastClassZeroUploadAt(ctx, channel, user); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("no mark yet: err = %v, want ErrNotFound", err)
			}
			put := func(ch, u id.ID, at int64) {
				t.Helper()
				if err := repo.PutSlowmodeMark(ctx, ch, u, at); err != nil {
					t.Fatalf("PutSlowmodeMark: %v", err)
				}
			}
			put(channel, user, 100)
			want("the first mark", channel, user, 100)
			put(channel, user, 140)
			want("a later post raises it", channel, user, 140)
			put(channel, user, 120)
			want("an older time never lowers it", channel, user, 140)
			put(channel, stranger, 500)
			want("another user's mark is their own", channel, user, 140)
			if _, err := repo.LastClassZeroUploadAt(ctx, other, user); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("another channel: err = %v, want ErrNotFound", err)
			}
		})
	}
}

// The class column round-trips through PutAppMessage and both reads: 0, 1 and NULL.
func TestAppMessageClassRoundTrips(t *testing.T) {
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			ctx := context.Background()
			g := seedGroup(ctx, t, repo, 1)
			for i, c := range []*uint8{classPtr(0), classPtr(1), nil} {
				if err := repo.PutAppMessage(ctx, store.AppMessageRow{
					GroupID: g.GroupID, Seq: uint64(i + 1), Epoch: 1, UploaderDevice: id.New(), Blob: []byte("ct"),
					FrankingTag: make([]byte, 32), Size: 2, Created: 1, Class: c,
				}); err != nil {
					t.Fatalf("PutAppMessage: %v", err)
				}
			}
			rows, err := repo.ListAppMessages(ctx, g.GroupID, 1, 10)
			if err != nil || len(rows) != 3 {
				t.Fatalf("ListAppMessages = %d rows, %v", len(rows), err)
			}
			if rows[0].Class == nil || *rows[0].Class != 0 || rows[1].Class == nil || *rows[1].Class != 1 || rows[2].Class != nil {
				t.Fatalf("classes = %v %v %v, want 0 1 nil", rows[0].Class, rows[1].Class, rows[2].Class)
			}
			one, err := repo.GetAppMessage(ctx, g.GroupID, 2)
			if err != nil || one.Class == nil || *one.Class != 1 {
				t.Fatalf("GetAppMessage = %v, %v; want class 1", one.Class, err)
			}
		})
	}
}
