package api

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/livekit/protocol/livekit"

	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

type absentMemberRepo struct {
	store.Repository
	group                store.GroupRow
	members              []store.MemberRow
	users                map[id.ID]store.UserRow
	userErrors           map[id.ID]error
	groupErr, membersErr error
	proposals            []store.ProposalRow
	proposalsErr         error
}

func (r *absentMemberRepo) ListProposals(_ context.Context, _ id.ID, epoch uint64, includeVoid bool) ([]store.ProposalRow, error) {
	var out []store.ProposalRow
	for _, p := range r.proposals {
		if p.Epoch == epoch && (includeVoid || p.VoidAt == nil) {
			out = append(out, p)
		}
	}
	return out, r.proposalsErr
}

// pendRemove makes m the target of a non-void instance Remove at the group's epoch, issued at.
func (r *absentMemberRepo) pendRemove(m store.MemberRow, at time.Time) {
	leaf, dev := m.LeafIndex, m.DeviceID
	r.proposals = append(r.proposals, store.ProposalRow{GroupID: r.group.GroupID, Epoch: r.group.Epoch, Kind: 3,
		TargetLeaf: &leaf, TargetDevice: &dev, Origin: 0, IssuedAt: at.Unix(), TTL: 30})
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

// Review focus 3: the dropped human comes back just as the sweep runs. Its /rtc admission after the
// Remove was issued means its session is on its way into the room, whatever the room's listing
// said a moment earlier, so it counts as active and the call is kept. Only the admission of the
// session that dropped (before the Remove) no longer does.
func TestBotsOnlyKeepsACallWhoseRemovedHumanWasAdmittedAgain(t *testing.T) {
	h, repo, clk, row := absentMembersFixture(t)
	h.dsvc = nil
	human := repo.members[1]
	key := admitKey{room: row.LivekitRoom, dev: human.DeviceID}
	h.admissions = map[admitKey][]admission{key: {{at: clk.Now()}}}
	clk.Advance(5 * time.Second)
	repo.pendRemove(human, clk.Now())
	if end, err := h.trackAbsentLocked(t.Context(), row, nil, nil); err != nil || end {
		t.Fatalf("first observation = %v, %v; want live", end, err)
	}
	clk.Advance(RoomSweepInterval)
	h.admissions[key] = append(h.admissions[key], admission{at: clk.Now(), resume: true})
	if end, err := h.trackAbsentLocked(t.Context(), row, nil, nil); err != nil || end {
		t.Fatalf("a sweep ended the call as the removed human was admitted again: %v, %v", end, err)
	}
	clk.Advance(RoomSweepInterval)
	if end, err := h.trackAbsentLocked(t.Context(), row, nil, nil); err != nil || end {
		t.Fatalf("the observation after the re-admission ended the call: %v, %v; want a fresh first", end, err)
	}
}

// Review focus 4: an account kind that cannot be read never ends the call, also when the only
// human is absent and pending removal; every sweep logs it, by ids only, so the lockout it leaves
// in place is never silent.
func TestBotsOnlyNeverEndsOnAnUnknownAccountKind(t *testing.T) {
	for _, lookupErr := range []error{store.ErrNotFound, errors.New("store unavailable")} {
		t.Run(lookupErr.Error(), func(t *testing.T) {
			h, repo, clk, row := absentMembersFixture(t)
			var logs bytes.Buffer
			h.log = slog.New(slog.NewTextHandler(&logs, nil))
			human := repo.members[1]
			repo.pendRemove(human, clk.Now())
			clk.Advance(time.Second)
			repo.userErrors[human.UserID] = lookupErr
			for i := range 3 {
				if end, _ := h.trackAbsentLocked(t.Context(), row, nil, nil); end {
					t.Fatalf("sweep %d ended a call whose member's account kind is unknown", i)
				}
				clk.Advance(RoomSweepInterval)
			}
			if n := strings.Count(logs.String(), "reading a call member's account kind failed"); n != 3 {
				t.Fatalf("logged the lookup failure %d times over 3 sweeps, want 3:\n%s", n, logs.String())
			}
			if !strings.Contains(logs.String(), "user_id="+human.UserID.String()) {
				t.Fatalf("the warning does not name the member by id:\n%s", logs.String())
			}
		})
	}
}

// A failed read of the group's outstanding proposals is no proof of a Remove: it never ends the call
// and it is logged.
func TestBotsOnlyNeverEndsWhenTheProposalsCannotBeRead(t *testing.T) {
	h, repo, clk, row := absentMembersFixture(t)
	h.dsvc = nil
	var logs bytes.Buffer
	h.log = slog.New(slog.NewTextHandler(&logs, nil))
	human := repo.members[1]
	h.admissions = map[admitKey][]admission{{room: row.LivekitRoom, dev: human.DeviceID}: {{at: clk.Now()}}}
	clk.Advance(time.Second)
	repo.pendRemove(human, clk.Now())
	repo.proposalsErr = errors.New("store unavailable")
	for i := range 2 {
		if end, _ := h.trackAbsentLocked(t.Context(), row, nil, nil); end {
			t.Fatalf("sweep %d ended the call on an unreadable proposal list", i)
		}
		clk.Advance(RoomSweepInterval / 3)
	}
	if !strings.Contains(logs.String(), "reading a call group's outstanding proposals for the sweep failed") {
		t.Fatalf("the proposal read failure was not logged:\n%s", logs.String())
	}
}
