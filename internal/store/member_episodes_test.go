package store_test

import (
	"context"
	"sync"
	"testing"

	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

func TestMemberEpisodePersistsAcrossIdempotentPutAndChangesAfterRejoin(t *testing.T) {
	ctx := context.Background()
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			community := seedCommunity(ctx, t, repo)
			user := seedUser(ctx, t, repo).ID
			member := store.MemberOfCommunityRow{CommunityID: community, UserID: user, Joined: 100, Nick: "first"}
			if err := repo.Tx(ctx, func(tx store.Repository) error { return tx.PutMember(ctx, member) }); err != nil {
				t.Fatal(err)
			}
			first, err := repo.GetMember(ctx, community, user)
			if err != nil || first.EpisodeID.IsZero() {
				t.Fatalf("new membership episode = %v, err=%v", first.EpisodeID, err)
			}
			member.Nick = "updated"
			if err := repo.Tx(ctx, func(tx store.Repository) error { return tx.PutMember(ctx, member) }); err != nil {
				t.Fatal(err)
			}
			updated, err := repo.GetMember(ctx, community, user)
			if err != nil || updated.EpisodeID != first.EpisodeID || updated.Nick != "updated" {
				t.Fatalf("idempotent membership update = %+v, err=%v; want episode %s", updated, err, first.EpisodeID)
			}
			if err := repo.Tx(ctx, func(tx store.Repository) error { return tx.DeleteMember(ctx, community, user) }); err != nil {
				t.Fatal(err)
			}
			member.EpisodeID = id.New()
			if err := repo.Tx(ctx, func(tx store.Repository) error { return tx.PutMember(ctx, member) }); err != nil {
				t.Fatal(err)
			}
			rejoined, err := repo.GetMember(ctx, community, user)
			if err != nil || rejoined.EpisodeID.IsZero() || rejoined.EpisodeID == first.EpisodeID {
				t.Fatalf("rejoined episode = %v, err=%v; want new episode after %s", rejoined.EpisodeID, err, first.EpisodeID)
			}
		})
	}
}

func TestConcurrentMemberPutsKeepOneEpisodeForEachLiveMembership(t *testing.T) {
	ctx := context.Background()
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			community := seedCommunity(ctx, t, repo)
			user := seedUser(ctx, t, repo).ID
			const writers = 8
			start := make(chan struct{})
			errs := make(chan error, writers)
			var wg sync.WaitGroup
			for i := 0; i < writers; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					<-start
					err := repo.Tx(ctx, func(tx store.Repository) error {
						return tx.PutMember(ctx, store.MemberOfCommunityRow{
							CommunityID: community, UserID: user, EpisodeID: id.New(), Joined: int64(i), Nick: "race",
						})
					})
					if err != nil {
						errs <- err
					}
				}(i)
			}
			close(start)
			wg.Wait()
			close(errs)
			for err := range errs {
				t.Fatalf("concurrent member put: %v", err)
			}
			member, err := repo.GetMember(ctx, community, user)
			if err != nil || member.EpisodeID.IsZero() {
				t.Fatalf("membership after concurrent puts = %+v, %v", member, err)
			}
			if err := repo.Tx(ctx, func(tx store.Repository) error { return tx.DeleteMember(ctx, community, user) }); err != nil {
				t.Fatal(err)
			}
			if _, err := repo.GetMember(ctx, community, user); err == nil {
				t.Fatal("member survived delete")
			}
		})
	}
}

func TestPutMemberEpisodeInsertFailureRollsBackMembership(t *testing.T) {
	ctx := context.Background()
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			community := seedCommunity(ctx, t, repo)
			firstUser := seedUser(ctx, t, repo).ID
			secondUser := seedUser(ctx, t, repo).ID
			sharedEpisode := id.New()
			if err := repo.PutMember(ctx, store.MemberOfCommunityRow{CommunityID: community, UserID: firstUser, EpisodeID: sharedEpisode, Joined: 1}); err != nil {
				t.Fatal(err)
			}
			err := repo.PutMember(ctx, store.MemberOfCommunityRow{CommunityID: community, UserID: secondUser, EpisodeID: sharedEpisode, Joined: 2})
			if err == nil {
				t.Fatal("duplicate episode id was accepted")
			}
			members, err := repo.ListMembersOfCommunity(ctx, community, id.ID{}, 20)
			if err != nil {
				t.Fatal(err)
			}
			for _, member := range members {
				if member.UserID == secondUser {
					t.Fatal("membership survived failed episode insert")
				}
			}
		})
	}
}
