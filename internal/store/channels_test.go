package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

// seedCommunity creates one live community owned by a fresh user.
func seedCommunity(ctx context.Context, t *testing.T, repo store.Repository) id.ID {
	t.Helper()
	cid := id.New()
	if err := repo.CreateCommunity(ctx, store.CommunityRow{
		ID: cid, Owner: seedUser(ctx, t, repo).ID, Name: "c", PolicyJSON: []byte(`{}`),
		PolicyVersion: 1, Created: 1,
	}); err != nil {
		t.Fatalf("CreateCommunity: %v", err)
	}
	return cid
}

func channelIn(cid id.ID, kind, mode, visibility uint8, name string, position uint64) store.ChannelRow {
	c := cid
	return store.ChannelRow{
		ID: id.New(), CommunityID: &c, Kind: kind, Mode: mode, Visibility: visibility,
		Name: name, Position: position, SettingsJSON: []byte(`{}`), HostPolicyVersion: 1,
		Created: 1_700_000_000,
	}
}

func TestChannelRoundTrip(t *testing.T) {
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			ctx := context.Background()
			cid := seedCommunity(ctx, t, repo)

			cat := channelIn(cid, 2, 0, 0, "cat", 0)
			if err := repo.CreateChannel(ctx, cat); err != nil {
				t.Fatalf("CreateChannel(category): %v", err)
			}
			in := channelIn(cid, 0, 1, 2, "Allmänt", 7)
			in.ParentID = &cat.ID
			in.Topic = "om allt"
			in.SlowmodeSeconds = 30
			if err := repo.CreateChannel(ctx, in); err != nil {
				t.Fatalf("CreateChannel: %v", err)
			}
			got, err := repo.GetChannel(ctx, in.ID)
			if err != nil {
				t.Fatalf("GetChannel: %v", err)
			}
			if got.ID != in.ID || got.CommunityID == nil || *got.CommunityID != cid ||
				got.ParentID == nil || *got.ParentID != cat.ID || got.Kind != 0 || got.Mode != 1 ||
				got.Visibility != 2 || got.Name != "Allmänt" || got.Topic != "om allt" ||
				got.Position != 7 || got.SlowmodeSeconds != 30 || got.HostPolicyVersion != 1 ||
				got.Seq != 0 || got.Created != in.Created || got.DeletedAt != nil ||
				string(got.SettingsJSON) != `{}` {
				t.Fatalf("GetChannel = %+v", got)
			}

			// UpdateChannel writes the mutable columns and nothing else: kind,
			// community, seq and created are immutable here.
			upd := got
			upd.Kind = 1 // ignored
			upd.Seq = 99 // ignored
			upd.Name = "general"
			upd.Visibility = 0
			upd.Mode = 0
			upd.ParentID = nil
			upd.Position = 3
			upd.SettingsJSON = []byte(`{"a":1}`)
			upd.HostPolicyVersion = 2
			upd.SlowmodeSeconds = 0
			if err := repo.UpdateChannel(ctx, upd); err != nil {
				t.Fatalf("UpdateChannel: %v", err)
			}
			got, err = repo.GetChannel(ctx, in.ID)
			if err != nil {
				t.Fatalf("GetChannel after update: %v", err)
			}
			if got.Kind != 0 || got.Seq != 0 || got.Name != "general" || got.Mode != 0 ||
				got.Visibility != 0 || got.ParentID != nil || got.Position != 3 ||
				string(got.SettingsJSON) != `{"a":1}` || got.HostPolicyVersion != 2 {
				t.Fatalf("after update = %+v", got)
			}

			// The per-channel sequencer: one step per call, and gone with the channel.
			for want := uint64(1); want <= 3; want++ {
				seq, err := repo.NextChannelSeq(ctx, in.ID)
				if err != nil {
					t.Fatalf("NextChannelSeq: %v", err)
				}
				if seq != want {
					t.Fatalf("NextChannelSeq = %d, want %d", seq, want)
				}
			}
			if _, err := repo.NextChannelSeq(ctx, id.New()); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("NextChannelSeq(unknown) = %v, want ErrNotFound", err)
			}

			if err := repo.UpdateChannel(ctx, channelIn(cid, 0, 0, 0, "ghost", 0)); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("UpdateChannel(unknown) = %v, want ErrNotFound", err)
			}
			if _, err := repo.GetChannel(ctx, id.New()); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("GetChannel(unknown) = %v, want ErrNotFound", err)
			}

			if err := repo.DeleteChannel(ctx, in.ID, 2_000); err != nil {
				t.Fatalf("DeleteChannel: %v", err)
			}
			if _, err := repo.GetChannel(ctx, in.ID); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("GetChannel after delete = %v, want ErrNotFound", err)
			}
			if err := repo.DeleteChannel(ctx, in.ID, 2_001); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("second DeleteChannel = %v, want ErrNotFound", err)
			}
			if _, err := repo.NextChannelSeq(ctx, in.ID); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("NextChannelSeq on a deleted channel = %v, want ErrNotFound", err)
			}
			if err := repo.UpdateChannel(ctx, upd); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("UpdateChannel on a deleted channel = %v, want ErrNotFound", err)
			}
		})
	}
}

func TestListChannelsOrdersByPositionThenID(t *testing.T) {
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			ctx := context.Background()
			cid := seedCommunity(ctx, t, repo)
			other := seedCommunity(ctx, t, repo)

			a := channelIn(cid, 0, 0, 0, "a", 20)
			b := channelIn(cid, 0, 0, 0, "b", 10)
			c := channelIn(cid, 1, 0, 0, "c", 10)
			gone := channelIn(cid, 0, 0, 0, "gone", 0)
			elsewhere := channelIn(other, 0, 0, 0, "elsewhere", 0)
			for _, ch := range []store.ChannelRow{a, b, c, gone, elsewhere} {
				if err := repo.CreateChannel(ctx, ch); err != nil {
					t.Fatalf("CreateChannel(%s): %v", ch.Name, err)
				}
			}
			if err := repo.DeleteChannel(ctx, gone.ID, 5); err != nil {
				t.Fatalf("DeleteChannel: %v", err)
			}
			first, second := b, c
			if string(c.ID[:]) < string(b.ID[:]) {
				first, second = c, b
			}
			rows, err := repo.ListChannels(ctx, cid)
			if err != nil {
				t.Fatalf("ListChannels: %v", err)
			}
			if len(rows) != 3 || rows[0].ID != first.ID || rows[1].ID != second.ID || rows[2].ID != a.ID {
				t.Fatalf("ListChannels = %+v, want position 10 (by id), then 20, without the deleted one", rows)
			}
			none, err := repo.ListChannels(ctx, id.New())
			if err != nil {
				t.Fatalf("ListChannels(unknown): %v", err)
			}
			if none == nil || len(none) != 0 {
				t.Fatalf("ListChannels(unknown) = %#v, want an empty non-nil slice", none)
			}

			n, err := repo.DeleteChannelsOfCommunity(ctx, cid, 9)
			if err != nil {
				t.Fatalf("DeleteChannelsOfCommunity: %v", err)
			}
			if n != 3 {
				t.Fatalf("DeleteChannelsOfCommunity = %d rows, want the 3 live ones", n)
			}
			if rows, _ := repo.ListChannels(ctx, cid); len(rows) != 0 {
				t.Fatalf("channels left after the community's tombstone: %+v", rows)
			}
			if _, err := repo.GetChannel(ctx, elsewhere.ID); err != nil {
				t.Fatalf("another community's channel went too: %v", err)
			}
		})
	}
}

// The engine enforces the three structural rules itself, so no code path can
// write a channel the delivery service would have to reject.
func TestTheChannelsTableRefusesIllegalRows(t *testing.T) {
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			ctx := context.Background()
			cid := seedCommunity(ctx, t, repo)
			cat := channelIn(cid, 2, 0, 0, "cat", 0)
			if err := repo.CreateChannel(ctx, cat); err != nil {
				t.Fatalf("CreateChannel(category): %v", err)
			}

			nested := channelIn(cid, 2, 0, 0, "nested", 0)
			nested.ParentID = &cat.ID
			visibleE2EE := channelIn(cid, 0, 0, 1, "invite e2ee", 0)
			orphanText := channelIn(cid, 0, 0, 0, "no community", 0)
			orphanText.CommunityID = nil
			dmInCommunity := channelIn(cid, 3, 0, 0, "dm with a community", 0)
			badKind := channelIn(cid, 5, 0, 0, "kind 5", 0)
			for name, row := range map[string]store.ChannelRow{
				"a category with a parent":         nested,
				"an invite-visible e2ee channel":   visibleE2EE,
				"a text channel with no community": orphanText,
				"a DM that names a community":      dmInCommunity,
				"a kind outside 0..4":              badKind,
			} {
				if err := repo.CreateChannel(ctx, row); err == nil {
					t.Errorf("%s was accepted", name)
				}
			}

			// A DM has no community, and that is the one kind for which that is legal.
			dm := channelIn(cid, 3, 0, 0, "dm", 0)
			dm.CommunityID = nil
			if err := repo.CreateChannel(ctx, dm); err != nil {
				t.Fatalf("CreateChannel(dm): %v", err)
			}
			got, err := repo.GetChannel(ctx, dm.ID)
			if err != nil {
				t.Fatalf("GetChannel(dm): %v", err)
			}
			if got.CommunityID != nil {
				t.Fatalf("a DM read back with community %v", *got.CommunityID)
			}

			if err := repo.CreateChannel(ctx, cat); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("a duplicate channel id = %v, want ErrConflict", err)
			}
		})
	}
}
