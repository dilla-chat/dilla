package api

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/livekit/protocol/livekit"

	"github.com/jonasthim/dilla/internal/clock"
	"github.com/jonasthim/dilla/internal/id"
	"github.com/jonasthim/dilla/internal/store"
)

type absentMemberRepo struct {
	store.Repository
	group                store.GroupRow
	members              []store.MemberRow
	users                map[id.ID]store.UserRow
	userErrors           map[id.ID]error
	groupErr, membersErr error
}

func (r *absentMemberRepo) GetGroup(context.Context, id.ID) (store.GroupRow, error) {
	return r.group, r.groupErr
}
func (r *absentMemberRepo) ListMembers(context.Context, id.ID) ([]store.MemberRow, error) {
	return r.members, r.membersErr
}
func (r *absentMemberRepo) GetUser(_ context.Context, user id.ID) (store.UserRow, error) {
	if err := r.userErrors[user]; err != nil {
		return store.UserRow{}, err
	}
	row, ok := r.users[user]
	if !ok {
		return store.UserRow{}, store.ErrNotFound
	}
	return row, nil
}

type absentMemberDS struct{ DS }

func absentMembersFixture(t *testing.T) (*Calls, *absentMemberRepo, *clock.Fake, store.VoiceSessionRow) {
	t.Helper()
	group, bot, human := id.New(), id.New(), id.New()
	repo := &absentMemberRepo{
		group: store.GroupRow{GroupID: group, Epoch: 3},
		members: []store.MemberRow{
			{UserID: bot, DeviceID: id.New(), LeafIndex: 1, AddedEpoch: 3},
			{UserID: human, DeviceID: id.New(), LeafIndex: 2, AddedEpoch: 3},
		},
		users:      map[id.ID]store.UserRow{bot: {ID: bot, Kind: 1}, human: {ID: human}},
		userErrors: map[id.ID]error{},
	}
	clk := clock.NewFake(time.Unix(1000, 0))
	calls := &Calls{repo: repo, clk: clk, log: slog.New(slog.DiscardHandler), sweepEvery: RoomSweepInterval, dsvc: absentMemberDS{}}
	return calls, repo, clk, store.VoiceSessionRow{CallID: id.New(), GroupID: &group, LivekitRoom: "room"}
}

func TestBotOnlyEndNeedsTwoObservationsOneSweepApart(t *testing.T) {
	h, _, clk, row := absentMembersFixture(t)
	if end, err := h.trackAbsentLocked(t.Context(), row, nil, nil); err != nil || end {
		t.Fatalf("first observation = %v, %v; want live", end, err)
	}
	clk.Advance(RoomSweepInterval - time.Nanosecond)
	if end, err := h.trackAbsentLocked(t.Context(), row, nil, nil); err != nil || end {
		t.Fatalf("early second observation = %v, %v; want live", end, err)
	}
	clk.Advance(time.Nanosecond)
	if end, err := h.trackAbsentLocked(t.Context(), row, nil, nil); err != nil || !end {
		t.Fatalf("second spaced observation = %v, %v; want end", end, err)
	}
	if len(h.leafRemovals) != 0 {
		t.Fatalf("bot-only call queued uncommittable Removes: %+v", h.leafRemovals)
	}
}

func TestBotOnlyObservationResetsWhenHumanOrStateIsUnknown(t *testing.T) {
	for _, reset := range []string{"human present", "fresh admission", "user lookup", "unknown user", "members lookup", "group lookup", "unknown epoch", "closed group"} {
		t.Run(reset, func(t *testing.T) {
			h, repo, clk, row := absentMembersFixture(t)
			// This test focuses on ending, without a delivery service to queue Removes.
			h.dsvc = nil
			_, _ = h.trackAbsentLocked(t.Context(), row, nil, nil)
			clk.Advance(RoomSweepInterval)
			human := repo.members[1]
			var parts []*livekit.ParticipantInfo
			switch reset {
			case "human present":
				parts = []*livekit.ParticipantInfo{{Identity: human.DeviceID.String()}}
			case "fresh admission":
				h.admissions = map[admitKey][]admission{{room: row.LivekitRoom, dev: human.DeviceID}: {{at: clk.Now()}}}
			case "user lookup":
				repo.userErrors[human.UserID] = errors.New("unavailable")
			case "unknown user":
				delete(repo.users, human.UserID)
			case "members lookup":
				repo.membersErr = errors.New("unavailable")
			case "group lookup":
				repo.groupErr = errors.New("unavailable")
			case "unknown epoch":
				repo.group.EpochUnknown = true
			case "closed group":
				closed := clk.Now().Unix()
				repo.group.ClosedAt = &closed
			}
			if end, _ := h.trackAbsentLocked(t.Context(), row, parts, nil); end {
				t.Fatal("uncertain or human-active observation ended the call")
			}
			repo.userErrors = nil
			repo.users[human.UserID] = store.UserRow{ID: human.UserID}
			repo.membersErr, repo.groupErr = nil, nil
			repo.group.EpochUnknown, repo.group.ClosedAt = false, nil
			h.admissions = nil
			clk.Advance(RoomSweepInterval)
			if end, err := h.trackAbsentLocked(t.Context(), row, nil, nil); err != nil || end {
				t.Fatalf("first observation after reset = %v, %v; want live", end, err)
			}
			clk.Advance(RoomSweepInterval)
			if end, err := h.trackAbsentLocked(t.Context(), row, nil, nil); err != nil || !end {
				t.Fatalf("second observation after reset = %v, %v; want end", end, err)
			}
		})
	}
}

func TestMemberLookupFailureDoesNotBlockOtherAbsentRemoves(t *testing.T) {
	for _, lookupErr := range []error{store.ErrNotFound, errors.New("store unavailable")} {
		t.Run(lookupErr.Error(), func(t *testing.T) {
			h, repo, clk, row := absentMembersFixture(t)
			unknown := id.New()
			repo.members = append(repo.members, store.MemberRow{UserID: unknown, DeviceID: id.New(), LeafIndex: 3, AddedEpoch: 3})
			repo.userErrors[unknown] = lookupErr
			for range 2 {
				if end, _ := h.trackAbsentLocked(t.Context(), row, []*livekit.ParticipantInfo{{Identity: repo.members[0].DeviceID.String()}}, nil); end {
					t.Fatal("member lookup failure must prevent ending")
				}
				clk.Advance(RoomSweepInterval)
			}
			if len(h.leafRemovals) != 1 || h.leafRemovals[0].key.dev != repo.members[1].DeviceID {
				t.Fatalf("queued Removes = %+v, want only the known absent human", h.leafRemovals)
			}
		})
	}
}
