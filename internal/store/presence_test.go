package store_test

import (
	"context"
	"testing"

	"github.com/dilla-chat/dilla/internal/id"
)

func TestPresenceChoicesBatch(t *testing.T) {
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			ctx := context.Background()
			online := seedUser(ctx, t, repo).ID
			dnd := seedUser(ctx, t, repo).ID
			if err := repo.SetPresenceChoice(ctx, dnd, 3); err != nil {
				t.Fatal(err)
			}
			got, err := repo.ListPresenceChoices(ctx, []id.ID{online, dnd})
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 1 || got[dnd] != 3 {
				t.Fatalf("batch choices = %v, want only DND (Online defaults without a row)", got)
			}
			empty, err := repo.ListPresenceChoices(ctx, nil)
			if err != nil || len(empty) != 0 {
				t.Fatalf("empty batch = %v, %v", empty, err)
			}
		})
	}
}
