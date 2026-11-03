package service

import (
	"context"
	"errors"
	"testing"

	"github.com/dilla-chat/dilla/internal/bot/client"
	"github.com/dilla-chat/dilla/internal/bot/native"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
)

type catchupNative struct {
	binding native.Binding
	state   native.StateValue
}

func (n *catchupNative) Binding() native.Binding { return n.binding }
func (n *catchupNative) StateGet(_ context.Context, area uint64, key []byte) (native.StateValue, error) {
	if area != memberRegistrationCatchupArea || string(key) != memberRegistrationCatchupKey {
		return native.StateValue{}, errors.New("unexpected state key")
	}
	return n.state, nil
}
func (n *catchupNative) StateTransaction(_ context.Context, tx id.ID, changes []native.StateChange) ([]uint64, error) {
	if tx.IsZero() || len(changes) != 1 || changes[0].ExpectedRevision != n.state.Revision {
		return nil, errors.New("unexpected state transaction")
	}
	n.state = native.StateValue{Revision: n.state.Revision + 1, Value: append([]byte(nil), changes[0].Value...)}
	return []uint64{n.state.Revision}, nil
}

type catchupSession struct {
	members  []client.CommunityMember
	episodes map[id.ID]id.ID
	absent   map[id.ID]bool
	requests []id.ID
	changed  map[id.ID]id.ID
	calls    map[id.ID]int
}

func (s *catchupSession) Page(_ context.Context, after id.ID) (client.CommunityMemberPage, error) {
	s.requests = append(s.requests, after)
	rows := make([]client.CommunityMember, 0, client.MaxCommunityMemberPage)
	for _, m := range s.members {
		if bytesAfter(m.UserID, after) {
			rows = append(rows, m)
			if len(rows) == client.MaxCommunityMemberPage {
				break
			}
		}
	}
	return client.CommunityMemberPage{Members: rows}, nil
}
func (s *catchupSession) ResolveCurrentEpisode(_ context.Context, user id.ID) (id.ID, bool, error) {
	if s.calls == nil {
		s.calls = map[id.ID]int{}
	}
	s.calls[user]++
	if s.calls[user] > 1 && !s.changed[user].IsZero() {
		got := s.changed[user]
		s.episodes[user] = got
		s.changed[user] = id.ID{}
		return got, true, nil
	}
	return s.episodes[user], !s.absent[user] && !s.episodes[user].IsZero(), nil
}
func bytesAfter(a, b id.ID) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] > b[i]
		}
	}
	return false
}

type catchupQueue struct {
	jobs []interactionRegistrationJob
	full map[id.ID]bool
}

func (q *catchupQueue) QueueMemberJoin(_ context.Context, user, episode id.ID) error {
	if q.full[user] {
		return ErrInteractionRegistrationQueueFull
	}
	for _, job := range q.jobs {
		if job.User == user && job.Episode == episode {
			return nil
		}
	}
	q.jobs = append(q.jobs, interactionRegistrationJob{User: user, Episode: episode})
	return nil
}

func catchupID(n uint16) id.ID { var out id.ID; out[14] = byte(n >> 8); out[15] = byte(n); return out }

func TestMemberRegistrationCatchupStagesOnlyCurrentHumanEpisodesAndCompletes(t *testing.T) {
	community := catchupID(1)
	n := &catchupNative{binding: native.Binding{Instance: catchupID(2), Community: community, Installation: catchupID(3)}}
	human, bot, absent := catchupID(10), catchupID(11), catchupID(12)
	s := &catchupSession{members: []client.CommunityMember{{UserID: human, Kind: 0}, {UserID: bot, Kind: 1}, {UserID: absent, Kind: 0}}, episodes: map[id.ID]id.ID{human: catchupID(20), absent: catchupID(21)}, absent: map[id.ID]bool{absent: true}}
	q := &catchupQueue{}
	c, err := newMemberRegistrationCatchup(n, MemberRegistrationCatchupOptions{PageBudget: 1})
	if err != nil {
		t.Fatal(err)
	}
	result, err := c.StageOnce(context.Background(), s, q)
	if err != nil {
		t.Fatal(err)
	}
	if result.Staged != 1 || result.MembersVisited != 3 || result.More || len(q.jobs) != 1 || q.jobs[0].User != human || q.jobs[0].Episode != catchupID(20) {
		t.Fatalf("result=%+v jobs=%+v", result, q.jobs)
	}
	var saved []any
	if err := cborx.Unmarshal(n.state.Value, &saved); err != nil || len(saved) != 3 || saved[0] != uint64(1) || saved[2] != uint64(1) {
		t.Fatalf("saved state=%#v err=%v", saved, err)
	}
}

func TestMemberRegistrationCatchupIsBoundedAndResumesAfterRestart(t *testing.T) {
	n := &catchupNative{binding: native.Binding{Instance: catchupID(2), Community: catchupID(1), Installation: catchupID(3)}}
	s := &catchupSession{episodes: map[id.ID]id.ID{}, absent: map[id.ID]bool{}}
	for i := uint16(1); i <= client.MaxCommunityMemberPage+1; i++ {
		user := catchupID(i)
		s.members = append(s.members, client.CommunityMember{UserID: user, Kind: 0})
		s.episodes[user] = catchupID(i + 1000)
	}
	q := &catchupQueue{}
	first, err := newMemberRegistrationCatchup(n, MemberRegistrationCatchupOptions{PageBudget: 1})
	if err != nil {
		t.Fatal(err)
	}
	result, err := first.StageOnce(context.Background(), s, q)
	if err != nil {
		t.Fatal(err)
	}
	if result.MembersVisited != client.MaxCommunityMemberPage || !result.More || len(s.requests) != 1 || len(q.jobs) != client.MaxCommunityMemberPage {
		t.Fatalf("first result=%+v requests=%d jobs=%d", result, len(s.requests), len(q.jobs))
	}
	second, err := newMemberRegistrationCatchup(n, MemberRegistrationCatchupOptions{PageBudget: 1})
	if err != nil {
		t.Fatal(err)
	}
	result, err = second.StageOnce(context.Background(), s, q)
	if err != nil {
		t.Fatal(err)
	}
	if result.MembersVisited != 1 || result.More || len(s.requests) != 2 || s.requests[1] != catchupID(client.MaxCommunityMemberPage) || len(q.jobs) != client.MaxCommunityMemberPage+1 {
		t.Fatalf("resumed result=%+v requests=%v jobs=%d", result, s.requests, len(q.jobs))
	}
}

func TestMemberRegistrationCatchupDoesNotAdvancePastFullQueueAndRechecksRejoin(t *testing.T) {
	n := &catchupNative{binding: native.Binding{Instance: catchupID(2), Community: catchupID(1), Installation: catchupID(3)}}
	a, b := catchupID(10), catchupID(11)
	s := &catchupSession{members: []client.CommunityMember{{UserID: a, Kind: 0}, {UserID: b, Kind: 0}}, episodes: map[id.ID]id.ID{a: catchupID(20), b: catchupID(21)}, absent: map[id.ID]bool{}, changed: map[id.ID]id.ID{a: catchupID(22)}}
	q := &catchupQueue{full: map[id.ID]bool{b: true}}
	c, _ := newMemberRegistrationCatchup(n, MemberRegistrationCatchupOptions{PageBudget: 1})
	if _, err := c.StageOnce(context.Background(), s, q); !errors.Is(err, ErrInteractionRegistrationQueueFull) {
		t.Fatalf("error=%v", err)
	}
	cur, _, err := c.loadCursor(context.Background())
	if err != nil || cur.After != a {
		t.Fatalf("cursor=%+v err=%v", cur, err)
	}
	if len(q.jobs) != 2 || q.jobs[0].Episode != catchupID(20) || q.jobs[1].Episode != catchupID(22) {
		t.Fatalf("jobs after episode change=%+v", q.jobs)
	}
	delete(q.full, b)
	result, err := c.StageOnce(context.Background(), s, q)
	if err != nil {
		t.Fatal(err)
	}
	if result.Staged != 1 || len(q.jobs) != 3 || q.jobs[2].User != b || q.jobs[2].Episode != catchupID(21) {
		t.Fatalf("retry result=%+v jobs=%+v", result, q.jobs)
	}
	if len(s.requests) != 2 || s.requests[1] != a {
		t.Fatalf("retry started after=%v", s.requests)
	}
}
