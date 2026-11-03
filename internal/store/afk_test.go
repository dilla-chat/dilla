package store_test

import (
	"context"
	"testing"

	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

func TestCommunityAFKConfiguration(t *testing.T) {
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			ctx := context.Background()
			owner := seedUser(ctx, t, repo)
			cid := id.New()
			if err := repo.CreateCommunity(ctx, store.CommunityRow{ID: cid, Owner: owner.ID, Name: "AFK", PolicyJSON: []byte("{}"), PolicyVersion: 1, Created: 1}); err != nil {
				t.Fatal(err)
			}
			initial, err := repo.GetCommunityAFK(ctx, cid)
			if err != nil || initial.ChannelID != nil || initial.TimeoutSeconds != 300 {
				t.Fatalf("initial = %+v, %v", initial, err)
			}
			room := id.New()
			if err := repo.CreateChannel(ctx, store.ChannelRow{ID: room, CommunityID: &cid, Kind: 1, Mode: 0, Visibility: 0, Name: "AFK room", SettingsJSON: []byte("{}"), HostPolicyVersion: 1, Created: 1}); err != nil {
				t.Fatal(err)
			}
			if err := repo.SetCommunityAFK(ctx, cid, store.CommunityAFK{ChannelID: &room, TimeoutSeconds: 60}); err != nil {
				t.Fatal(err)
			}
			got, err := repo.GetCommunityAFK(ctx, cid)
			if err != nil || got.ChannelID == nil || *got.ChannelID != room || got.TimeoutSeconds != 60 {
				t.Fatalf("saved = %+v, %v", got, err)
			}
			if err := repo.SetCommunityAFK(ctx, cid, store.CommunityAFK{TimeoutSeconds: 900}); err != nil {
				t.Fatal(err)
			}
			got, err = repo.GetCommunityAFK(ctx, cid)
			if err != nil || got.ChannelID != nil || got.TimeoutSeconds != 900 {
				t.Fatalf("disabled = %+v, %v", got, err)
			}
		})
	}
}
