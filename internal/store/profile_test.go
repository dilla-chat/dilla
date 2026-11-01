package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jonasthim/dilla/internal/id"
	"github.com/jonasthim/dilla/internal/store"
)

// A new account has an empty status message and bio; UpdateProfile writes all three profile fields
// in one statement, GetUser reads them back, and a tombstone clears them.
func TestUpdateProfileRoundTrips(t *testing.T) {
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			ctx := context.Background()
			user := seedUser(ctx, t, repo)
			fresh, err := repo.GetUser(ctx, user.ID)
			if err != nil || fresh.StatusMsg != "" || fresh.Bio != "" {
				t.Fatalf("a new account has status %q and bio %q, want both empty (%v)", fresh.StatusMsg, fresh.Bio, err)
			}
			if _, err := repo.UpdateProfile(ctx, user.ID, sp("Ada L"), sp("on a call"), sp("line one\nline two")); err != nil {
				t.Fatalf("UpdateProfile: %v", err)
			}
			got, err := repo.GetUser(ctx, user.ID)
			if err != nil || got.Display != "Ada L" || got.StatusMsg != "on a call" || got.Bio != "line one\nline two" {
				t.Fatalf("GetUser = %+v, %v; want the written profile", got, err)
			}
			if got.Username != fresh.Username {
				t.Fatalf("the username changed: %q", got.Username)
			}
			if _, err := repo.UpdateProfile(ctx, id.New(), sp("x"), nil, nil); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("UpdateProfile of an unknown user = %v, want ErrNotFound", err)
			}
			if err := repo.TombstoneUser(ctx, user.ID, 5); err != nil {
				t.Fatal(err)
			}
			got, err = repo.GetUser(ctx, user.ID)
			if err != nil || got.Display != "" || got.StatusMsg != "" || got.Bio != "" {
				t.Fatalf("a tombstone keeps display %q, status %q, bio %q; want all cleared (%v)", got.Display, got.StatusMsg, got.Bio, err)
			}
		})
	}
}

// The columns bound their own length in characters, so a client that skips the API's check cannot
// store more than 190 and 500.
func TestProfileColumnsBoundTheirLength(t *testing.T) {
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			ctx := context.Background()
			user := seedUser(ctx, t, repo)
			if _, err := repo.UpdateProfile(ctx, user.ID, sp("d"), sp(strings.Repeat("ä", 190)), sp(strings.Repeat("ö", 500))); err != nil {
				t.Fatalf("the maximum lengths are refused: %v", err)
			}
			if _, err := repo.UpdateProfile(ctx, user.ID, sp("d"), sp(strings.Repeat("a", 191)), nil); err == nil {
				t.Fatal("a 191-character status message was stored")
			}
			if _, err := repo.UpdateProfile(ctx, user.ID, sp("d"), nil, sp(strings.Repeat("a", 501))); err == nil {
				t.Fatal("a 501-character bio was stored")
			}
		})
	}
}

func sp(s string) *string { return &s }

// A nil field is left as it is, in the same statement: two devices that each change one field never
// overwrite the other's, and "" clears a field.
func TestUpdateProfileWritesOnlyTheFieldsGiven(t *testing.T) {
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			ctx := context.Background()
			user := seedUser(ctx, t, repo)
			if _, err := repo.UpdateProfile(ctx, user.ID, sp("Ada"), sp("status"), sp("bio")); err != nil {
				t.Fatal(err)
			}
			if _, err := repo.UpdateProfile(ctx, user.ID, nil, sp("new status"), nil); err != nil {
				t.Fatal(err)
			}
			got, _ := repo.GetUser(ctx, user.ID)
			if got.Display != "Ada" || got.StatusMsg != "new status" || got.Bio != "bio" {
				t.Fatalf("after changing only the status: %+v", got)
			}
			if _, err := repo.UpdateProfile(ctx, user.ID, nil, nil, sp("")); err != nil {
				t.Fatal(err)
			}
			got, _ = repo.GetUser(ctx, user.ID)
			if got.Bio != "" || got.StatusMsg != "new status" {
				t.Fatalf("after clearing only the bio: %+v", got)
			}
			if _, err := repo.UpdateProfile(ctx, user.ID, nil, nil, nil); err != nil {
				t.Fatalf("nothing to write: %v", err)
			}
			if _, err := repo.UpdateProfile(ctx, id.New(), nil, nil, nil); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("an unknown user with nothing to write = %v, want ErrNotFound", err)
			}
		})
	}
}

func TestUpdateProfileReportsStoredChanges(t *testing.T) {
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			ctx := t.Context()
			user := seedUser(ctx, t, repo)
			for _, tc := range []struct {
				display, status, bio *string
				changed              bool
			}{
				{nil, nil, nil, false},
				{sp("Ada"), nil, nil, true},
				{sp("Ada"), nil, nil, false},
				{nil, sp("working"), nil, true},
				{nil, sp("working"), nil, false},
				{nil, nil, sp("hello"), true},
				{nil, nil, sp("hello"), false},
				{nil, nil, sp(""), true},
				{sp("Ada"), sp("working"), sp(""), false},
			} {
				changed, err := repo.UpdateProfile(ctx, user.ID, tc.display, tc.status, tc.bio)
				if err != nil || changed != tc.changed {
					t.Fatalf("UpdateProfile changed=%v, err=%v, want %v", changed, err, tc.changed)
				}
			}
			if err := repo.TombstoneUser(ctx, user.ID, 5); err != nil {
				t.Fatal(err)
			}
			if changed, err := repo.UpdateProfile(ctx, user.ID, sp(""), nil, nil); changed || !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("tombstoned no-op = %v, %v", changed, err)
			}
		})
	}
}
