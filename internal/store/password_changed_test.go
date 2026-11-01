package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jonasthim/dilla/internal/id"
	"github.com/jonasthim/dilla/internal/store"
)

// Task 11 (Jonas, Q7), migration 00052: a password's change time is unknown (nil) after sign-up (PutPasswordCredential), and
// ChangePasswordCredential writes the new hash and the change time in one statement (fix round 2, security L1), so neither is
// ever stored without the other. GetPasswordChangedAt of an account without a password is ErrNotFound.
func TestPasswordChangedAt(t *testing.T) {
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			ctx := context.Background()
			user := seedUser(ctx, t, repo)
			if _, err := repo.GetPasswordChangedAt(ctx, user.ID); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("no password: err = %v, want ErrNotFound", err)
			}
			if err := repo.PutPasswordCredential(ctx, user.ID, "$argon2id$x", 1); err != nil {
				t.Fatal(err)
			}
			if at, err := repo.GetPasswordChangedAt(ctx, user.ID); err != nil || at != nil {
				t.Fatalf("after sign-up: at = %v, err = %v, want nil, nil", at, err)
			}
			if err := repo.ChangePasswordCredential(ctx, user.ID, "$argon2id$y", 1_800_000_000); err != nil {
				t.Fatal(err)
			}
			phc, err := repo.GetPasswordCredential(ctx, user.ID)
			if err != nil || phc != "$argon2id$y" {
				t.Fatalf("after a change: phc = %q, err = %v, want the new hash", phc, err)
			}
			at, err := repo.GetPasswordChangedAt(ctx, user.ID)
			if err != nil || at == nil || *at != 1_800_000_000 {
				t.Fatalf("after a change: at = %v, err = %v, want 1800000000", at, err)
			}
			// A change for an account with no password row yet stores both as well.
			other := seedUser(ctx, t, repo)
			if err := repo.ChangePasswordCredential(ctx, other.ID, "$argon2id$z", 1_800_000_100); err != nil {
				t.Fatal(err)
			}
			if at, err := repo.GetPasswordChangedAt(ctx, other.ID); err != nil || at == nil || *at != 1_800_000_100 {
				t.Fatalf("first password by a change: at = %v, err = %v", at, err)
			}
			if _, err := repo.GetPasswordChangedAt(ctx, id.New()); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("an unknown user: err = %v, want ErrNotFound", err)
			}
		})
	}
}
