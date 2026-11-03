package service

import (
	"context"
	"testing"

	"github.com/dilla-chat/dilla/internal/bot/client"
	"github.com/dilla-chat/dilla/internal/bot/native"
	"github.com/dilla-chat/dilla/internal/id"
)

func TestStructuralGapRecoveryStagesCurrentEpisodesAndResumes(t *testing.T) {
	n := &gapRecoveryNative{binding: native.Binding{Instance: testID(7), Community: testID(1), Installation: testID(8)}}
	s := &catchupSession{episodes: map[id.ID]id.ID{}, absent: map[id.ID]bool{}}
	for i := uint16(1); i <= client.MaxCommunityMemberPage+1; i++ {
		user := catchupID(i)
		s.members = append(s.members, client.CommunityMember{UserID: user, Kind: 0})
		s.episodes[user] = catchupID(i + 1000)
	}
	q := &catchupQueue{}
	r, _ := newStructuralGapRecovery(n, 1)
	got, err := r.StageOnce(context.Background(), s, q, 11, 15)
	if err != nil || got.Done || len(q.jobs) != client.MaxCommunityMemberPage {
		t.Fatalf("first: result=%+v jobs=%v err=%v", got, q.jobs, err)
	}
	r, _ = newStructuralGapRecovery(n, 1)
	got, err = r.StageOnce(context.Background(), s, q, 11, 16)
	if err != nil || !got.Done || len(q.jobs) != client.MaxCommunityMemberPage+1 {
		t.Fatalf("resumed: result=%+v jobs=%v err=%v", got, q.jobs, err)
	}
	if got.HighWater != 15 {
		t.Fatalf("recovery target changed during resume: %+v", got)
	}
	got, err = r.StageOnce(context.Background(), s, q, 16, 20)
	if err != nil || got.Done || !got.More || got.HighWater != 20 {
		t.Fatalf("retention restart: result=%+v err=%v", got, err)
	}
	if len(s.requests) != 3 || !s.requests[2].IsZero() {
		t.Fatalf("retention restart did not rescan from the beginning: requests=%v", s.requests)
	}
}

type gapRecoveryNative struct {
	binding native.Binding
	value   native.StateValue
}

func (n *gapRecoveryNative) Binding() native.Binding { return n.binding }
func (n *gapRecoveryNative) StateGet(context.Context, uint64, []byte) (native.StateValue, error) {
	return n.value, nil
}
func (n *gapRecoveryNative) StateTransaction(_ context.Context, tx id.ID, changes []native.StateChange) ([]uint64, error) {
	if tx.IsZero() || len(changes) != 1 || changes[0].ExpectedRevision != n.value.Revision {
		return nil, testingError("bad transaction")
	}
	n.value = native.StateValue{Revision: n.value.Revision + 1, Value: append([]byte(nil), changes[0].Value...)}
	return []uint64{n.value.Revision}, nil
}

type testingError string

func (e testingError) Error() string { return string(e) }
