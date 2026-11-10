package store_test

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

// Bot v2 Task 13: a webhook round-trips with only its secret's hash, rotates in one statement, is scoped to
// its community, and goes with the channel it targets.
func TestStoreBotWebhookRoundTripRotateAndDelete(t *testing.T) {
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			ctx := context.Background()
			cid, other := seedCommunity(ctx, t, repo), seedCommunity(ctx, t, repo)
			text, second := channelIn(cid, 0, 0, 0, "bot", 1), channelIn(cid, 0, 0, 0, "hooks", 2)
			for _, ch := range []store.ChannelRow{text, second} {
				if err := repo.CreateChannel(ctx, ch); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := repo.GetBotWebhook(ctx, id.New()); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("GetBotWebhook unknown = %v, want ErrNotFound", err)
			}
			hash := bytes.Repeat([]byte{0xa1}, 32)
			row := store.BotWebhookRow{WebhookID: id.New(), CommunityID: cid, ChannelID: text.ID, Name: "terraria",
				AvatarBlob: bytes.Repeat([]byte{7}, 32), SecretHash: hash, AllowEveryone: true, CreatedAt: 40}
			if err := repo.CreateBotWebhook(ctx, row); err != nil {
				t.Fatalf("CreateBotWebhook: %v", err)
			}
			if err := repo.CreateBotWebhook(ctx, store.BotWebhookRow{WebhookID: id.New(), CommunityID: cid, ChannelID: text.ID, SecretHash: []byte{1}}); err == nil {
				t.Fatal("a webhook with a short secret hash was stored")
			}
			gone := store.BotWebhookRow{WebhookID: id.New(), CommunityID: cid, ChannelID: second.ID, Name: "weather", SecretHash: hash, CreatedAt: 41}
			if err := repo.CreateBotWebhook(ctx, gone); err != nil {
				t.Fatal(err)
			}
			got, err := repo.GetBotWebhook(ctx, row.WebhookID)
			if err != nil || got.CommunityID != cid || got.ChannelID != text.ID || got.Name != "terraria" || !got.AllowEveryone ||
				!bytes.Equal(got.AvatarBlob, row.AvatarBlob) || !bytes.Equal(got.SecretHash, hash) || got.CreatedAt != 40 || got.RotatedAt != nil {
				t.Fatalf("GetBotWebhook = %+v, %v", got, err)
			}
			row.Name, row.AvatarBlob, row.AllowEveryone = "tModLoader", nil, false
			if ok, err := repo.UpdateBotWebhook(ctx, row); err != nil || !ok {
				t.Fatalf("UpdateBotWebhook = %v, %v", ok, err)
			}
			if ok, err := repo.UpdateBotWebhook(ctx, store.BotWebhookRow{WebhookID: row.WebhookID, CommunityID: other, Name: "x"}); err != nil || ok {
				t.Fatalf("UpdateBotWebhook from another community = %v, %v; want nothing found", ok, err)
			}
			newHash := bytes.Repeat([]byte{0xb2}, 32)
			if ok, err := repo.RotateBotWebhookSecret(ctx, other, row.WebhookID, newHash, 50); err != nil || ok {
				t.Fatalf("rotate from another community = %v, %v; want nothing found", ok, err)
			}
			if ok, err := repo.RotateBotWebhookSecret(ctx, cid, row.WebhookID, newHash, 50); err != nil || !ok {
				t.Fatalf("RotateBotWebhookSecret = %v, %v", ok, err)
			}
			got, err = repo.GetBotWebhook(ctx, row.WebhookID)
			if err != nil || got.Name != "tModLoader" || got.AvatarBlob != nil || got.AllowEveryone ||
				!bytes.Equal(got.SecretHash, newHash) || got.RotatedAt == nil || *got.RotatedAt != 50 {
				t.Fatalf("after update and rotate = %+v, %v", got, err)
			}
			if rows, err := repo.ListBotWebhooks(ctx, cid); err != nil || len(rows) != 2 || rows[0].WebhookID != row.WebhookID || rows[1].WebhookID != gone.WebhookID {
				t.Fatalf("ListBotWebhooks = %+v, %v; want both, oldest first", rows, err)
			}
			if rows, err := repo.ListBotWebhooks(ctx, other); err != nil || rows == nil || len(rows) != 0 {
				t.Fatalf("ListBotWebhooks of another community = %#v, %v; want an empty non-nil slice", rows, err)
			}
			if err := repo.DeleteBotWebhooksOfChannel(ctx, second.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := repo.GetBotWebhook(ctx, gone.WebhookID); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("the deleted channel's webhook = %v, want ErrNotFound", err)
			}
			if ok, err := repo.DeleteBotWebhook(ctx, other, row.WebhookID); err != nil || ok {
				t.Fatalf("DeleteBotWebhook from another community = %v, %v", ok, err)
			}
			if ok, err := repo.DeleteBotWebhook(ctx, cid, row.WebhookID); err != nil || !ok {
				t.Fatalf("DeleteBotWebhook = %v, %v", ok, err)
			}
			if rows, err := repo.ListBotWebhooks(ctx, cid); err != nil || len(rows) != 0 {
				t.Fatalf("after delete = %+v, %v", rows, err)
			}
		})
	}
}
