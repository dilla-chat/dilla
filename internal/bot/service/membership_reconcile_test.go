package service

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/dilla-chat/dilla/internal/bot/client"
	"github.com/dilla-chat/dilla/internal/bot/native"
	"github.com/dilla-chat/dilla/internal/id"
)

type membershipFakeNative struct {
	binding              native.Binding
	groups               []native.GroupInfo
	inspected            native.GroupInfo
	welcomes             []native.GroupWelcome
	expected             []native.GroupExpected
	outcomes             []native.GroupWelcomeOutcome
	state                native.StateValue
	applyCalls           int
	applyErr             error
	genericApplyCalls    int
	genericCursorCalls   int
	genericAckCalls      int
	changeGroupOnInspect bool
	other                map[string]native.StateValue // state keys other than the welcome cursor
	forgotten            []id.ID                      // GroupForget calls (Task 23, R1 d)
}

func (n *membershipFakeNative) Binding() native.Binding { return n.binding }

// GroupForget drops the group's local MLS state: a forgotten group reads back as gone (state 4).
func (n *membershipFakeNative) GroupForget(_ context.Context, group id.ID) error {
	n.forgotten = append(n.forgotten, group)
	for i := range n.groups {
		if n.groups[i].ID == group {
			n.groups[i].State = 4
		}
	}
	return nil
}
func (n *membershipFakeNative) Groups(context.Context) ([]native.GroupInfo, error) {
	return append([]native.GroupInfo(nil), n.groups...), nil // a fresh decode, as the helper answers
}
func (n *membershipFakeNative) InspectGroup(_ context.Context, group id.ID) (native.GroupInfo, error) {
	if n.inspected.ID != group {
		return native.GroupInfo{}, errors.New("unknown group")
	}
	got := n.inspected
	if n.changeGroupOnInspect {
		got.Target = membershipID(77)
	}
	return got, nil
}
func (n *membershipFakeNative) ApplyGroup(context.Context, id.ID, []byte, []byte, uint64) (native.GroupApplyResult, error) {
	n.genericApplyCalls++
	return native.GroupApplyResult{}, nil
}
func (n *membershipFakeNative) GroupCursor(context.Context, id.ID) (*native.GroupCursor, error) {
	n.genericCursorCalls++
	return nil, nil
}
func (n *membershipFakeNative) AcknowledgeGroupCursor(context.Context, id.ID, native.GroupCursor) error {
	n.genericAckCalls++
	return nil
}
func (n *membershipFakeNative) ApplyWelcomes(_ context.Context, ws []native.GroupWelcome, expected []native.GroupExpected) ([]native.GroupWelcomeOutcome, error) {
	n.applyCalls++
	n.welcomes = append(n.welcomes, ws...)
	n.expected = append([]native.GroupExpected(nil), expected...)
	return n.outcomes, n.applyErr
}
func (n *membershipFakeNative) StateGet(_ context.Context, _ uint64, key []byte) (native.StateValue, error) {
	if string(key) != membershipStateKey {
		return n.other[string(key)], nil
	}
	return n.state, nil
}
func (n *membershipFakeNative) StateTransaction(_ context.Context, _ id.ID, changes []native.StateChange) ([]uint64, error) {
	revs := make([]uint64, 0, len(changes))
	for _, ch := range changes {
		if key := string(ch.Key); key != membershipStateKey {
			if n.other == nil {
				n.other = map[string]native.StateValue{}
			}
			v := native.StateValue{Revision: n.other[key].Revision + 1, Value: ch.Value, Tombstone: ch.Value == nil}
			n.other[key] = v
			revs = append(revs, v.Revision)
			continue
		}
		n.state = native.StateValue{Revision: n.state.Revision + 1, Value: ch.Value}
		revs = append(revs, n.state.Revision)
	}
	return revs, nil
}

type membershipFakeSession struct {
	communities         []client.Community
	community           client.Community
	communityCalls      int
	communitiesCalls    int
	channels            []client.Channel
	welcomes            []client.Welcome
	acked               []uint64
	ackErr              error
	channelsCalls       int
	groupInfo           client.GroupInfo
	groupInfoErr        error
	groupInfoCalls      int
	groupPageErr        error
	groupPageCalls      int
	removeOnSecond      bool
	changeGroupOnSecond bool
}

func (s *membershipFakeSession) Communities(context.Context) ([]client.Community, error) {
	s.communitiesCalls++
	return s.communities, nil
}
func (s *membershipFakeSession) Community(_ context.Context, community id.ID) (client.Community, error) {
	s.communityCalls++
	return s.community, nil
}
func (s *membershipFakeSession) Channels(context.Context, id.ID) ([]client.Channel, error) {
	s.channelsCalls++
	if s.removeOnSecond && s.channelsCalls > 1 {
		return nil, nil
	}
	if s.changeGroupOnSecond && s.channelsCalls > 1 {
		channels := append([]client.Channel(nil), s.channels...)
		other := membershipID(88)
		channels[0].TextGroupID = &other
		return channels, nil
	}
	return s.channels, nil
}
func (s *membershipFakeSession) Welcomes(_ context.Context, after, limit uint64) ([]client.Welcome, error) {
	out := []client.Welcome{}
	for _, w := range s.welcomes {
		if w.ID > after && uint64(len(out)) < limit {
			out = append(out, w)
		}
	}
	return out, nil
}
func (s *membershipFakeSession) AcknowledgeWelcome(_ context.Context, w uint64) error {
	s.acked = append(s.acked, w)
	return s.ackErr
}
func (s *membershipFakeSession) GroupInfo(context.Context, id.ID) (client.GroupInfo, error) {
	s.groupInfoCalls++
	if s.groupInfo.NextSequence != 0 {
		return s.groupInfo, s.groupInfoErr
	}
	return client.GroupInfo{NextSequence: 1}, nil
}
func (s *membershipFakeSession) GroupPage(context.Context, id.ID, string, uint64, uint64) (client.Page, error) {
	s.groupPageCalls++
	if s.groupPageErr != nil {
		return client.Page{}, s.groupPageErr
	}
	return client.Page{Next: 1}, nil
}
func (s *membershipFakeSession) AdvanceCursor(context.Context, id.ID, uint64, uint64) error {
	return nil
}

func membershipID(n byte) id.ID { var v id.ID; v[15] = n; return v }

func requireMembershipPhase(t *testing.T, err error, phase MembershipPhase, cause error) {
	t.Helper()
	var phaseErr *MembershipPhaseError
	if !errors.As(err, &phaseErr) || phaseErr.Phase != phase {
		t.Fatalf("membership phase error=%#v, want phase %q", err, phase.String())
	}
	if !errors.Is(err, cause) {
		t.Fatalf("wrapped error %v does not preserve cause %v", err, cause)
	}
	if err.Error() != "membership reconciliation phase failed" {
		t.Fatalf("diagnostic error leaked non-fixed text: %q", err.Error())
	}
}

func TestMembershipPhaseErrorsClassifyWelcomeFailuresWithoutLeakingCauses(t *testing.T) {
	t.Run("apply", func(t *testing.T) {
		r, n, s, _, _, group := membershipFixture(t)
		cause := errors.New("private apply detail")
		n.applyErr = cause
		s.welcomes = []client.Welcome{{ID: 1, GroupID: group, Blob: []byte{1}, Tree: []byte{2}, TreeHash: make([]byte, 32)}}
		_, err := r.reconcileOnce(context.Background(), s)
		requireMembershipPhase(t, err, MembershipPhaseWelcomeApply, cause)
	})
	t.Run("revalidation", func(t *testing.T) {
		r, n, s, _, _, group := membershipFixture(t)
		s.welcomes = []client.Welcome{{ID: 2, GroupID: group, Blob: []byte{1}, Tree: []byte{2}, TreeHash: make([]byte, 32)}}
		n.outcomes = []native.GroupWelcomeOutcome{{ID: 2, Group: group, Outcome: 0}}
		s.removeOnSecond = true
		_, err := r.reconcileOnce(context.Background(), s)
		requireMembershipPhase(t, err, MembershipPhaseWelcomeRevalidation, ErrMembershipUnproven)
	})
	t.Run("acknowledgement", func(t *testing.T) {
		r, n, s, _, _, group := membershipFixture(t)
		cause := errors.New("private acknowledgement detail")
		s.welcomes = []client.Welcome{{ID: 3, GroupID: group, Blob: []byte{1}, Tree: []byte{2}, TreeHash: make([]byte, 32)}}
		n.outcomes = []native.GroupWelcomeOutcome{{ID: 3, Group: group, Outcome: 0}}
		s.ackErr = cause
		_, err := r.reconcileOnce(context.Background(), s)
		requireMembershipPhase(t, err, MembershipPhaseWelcomeAcknowledge, cause)
	})
}

func membershipFixture(t *testing.T) (*MembershipReconciler, *membershipFakeNative, *membershipFakeSession, id.ID, id.ID, id.ID) {
	t.Helper()
	community, install, channel, group := membershipID(1), membershipID(2), membershipID(3), membershipID(4)
	n := &membershipFakeNative{binding: native.Binding{Instance: membershipID(9), Community: community, Installation: install}}
	s := &membershipFakeSession{communities: []client.Community{{ID: community, Name: "own", OwnerID: membershipID(8)}}, community: client.Community{ID: community, Name: "own", OwnerID: membershipID(8)}, channels: []client.Channel{{ID: channel, CommunityID: community, Kind: 0, TextGroupID: &group}}}
	r, err := newMembershipReconciler(n, sourceSyncOptions{}, membershipOptions{WelcomePageLimit: 8})
	if err != nil {
		t.Fatal(err)
	}
	return r, n, s, community, channel, group
}

func TestMembershipPhaseErrorsClassifyEachSourceSyncKind(t *testing.T) {
	tests := []struct {
		name  string
		kind  uint64
		phase MembershipPhase
	}{
		{name: "text", kind: 0, phase: MembershipPhaseTextSourceSync},
		{name: "call", kind: 1, phase: MembershipPhaseCallSourceSync},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r, n, s, community, channel, group := membershipFixture(t)
			if tc.kind == 1 {
				s.channels[0].Kind = 1
				s.channels[0].TextGroupID = nil
			}
			info := native.GroupInfo{ID: group, Kind: tc.kind, Community: &community, Target: channel, State: 2, NextSequence: 1}
			n.groups, n.inspected = []native.GroupInfo{info}, info
			s.groupInfo = client.GroupInfo{NextSequence: 2}
			cause := &client.APIError{Status: 503, Code: "E_UNAVAILABLE"}
			s.groupPageErr = cause
			_, err := r.reconcileOnce(context.Background(), s)
			requireMembershipPhase(t, err, tc.phase, cause)
		})
	}
}

func TestMembershipIgnoresCategoriesInCommunityChannelList(t *testing.T) {
	r, n, s, community, _, group := membershipFixture(t)
	s.channels = append(s.channels,
		client.Channel{ID: membershipID(5), CommunityID: community, Kind: 2},
		client.Channel{ID: membershipID(6), CommunityID: community, Kind: 2},
	)
	s.welcomes = []client.Welcome{{ID: 1, GroupID: group, Blob: []byte{1}, Tree: []byte{2}, TreeHash: make([]byte, 32)}}
	n.outcomes = []native.GroupWelcomeOutcome{{ID: 1, Group: group, Outcome: 0}}
	result, err := r.reconcileOnce(context.Background(), s)
	if err != nil {
		t.Fatalf("valid categories blocked membership: %v", err)
	}
	if result.Applied != 1 || len(s.acked) != 1 || n.applyCalls != 1 {
		t.Fatalf("text welcome not applied with categories present: result=%+v ack=%v apply=%d", result, s.acked, n.applyCalls)
	}
}

func TestMembershipDuplicateWelcomeIsAppliedIdempotentlyAndAcked(t *testing.T) {
	r, n, s, community, channel, group := membershipFixture(t)
	s.welcomes = []client.Welcome{{ID: 1, GroupID: group, Epoch: 2, CommitSequence: 4, Blob: []byte{1}, Tree: []byte{2}, TreeHash: make([]byte, 32)}, {ID: 2, GroupID: group, Epoch: 2, CommitSequence: 4, Blob: []byte{1}, Tree: []byte{2}, TreeHash: make([]byte, 32)}}
	n.outcomes = []native.GroupWelcomeOutcome{{ID: 1, Group: group, Outcome: 0}, {ID: 2, Group: group, Outcome: 1}}
	result, err := r.reconcileOnce(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if result.Applied != 2 || len(s.acked) != 2 || n.applyCalls != 1 || len(n.expected) != 1 {
		t.Fatalf("result=%+v ack=%v apply=%d", result, s.acked, n.applyCalls)
	}
	r, err = newMembershipReconciler(n, sourceSyncOptions{}, membershipOptions{WelcomePageLimit: 8})
	if err != nil {
		t.Fatal(err)
	}
	s.welcomes = []client.Welcome{{ID: 3, GroupID: group, Epoch: 2, CommitSequence: 4, Blob: []byte{1}, Tree: []byte{2}, TreeHash: make([]byte, 32)}}
	n.outcomes = []native.GroupWelcomeOutcome{{ID: 3, Group: group, Outcome: 1}}
	if _, err = r.reconcileOnce(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if len(s.acked) != 3 || s.acked[2] != 3 || s.channels[0].ID != channel || community != n.binding.Community {
		t.Fatal("idempotent duplicate was not acknowledged")
	}
}
func TestMembershipRejectsWrongCommunityAndRemovedChannelWithoutAck(t *testing.T) {
	r, n, s, community, _, group := membershipFixture(t)
	s.welcomes = []client.Welcome{{ID: 7, GroupID: group, Blob: []byte{1}, Tree: []byte{2}, TreeHash: make([]byte, 32)}}
	s.community.ID = membershipID(99)
	if _, err := r.reconcileOnce(context.Background(), s); err == nil {
		t.Fatal("wrong community accepted")
	}
	if len(s.acked) != 0 || n.applyCalls != 0 {
		t.Fatal("wrong community mutated or acknowledged")
	}
	s.community = client.Community{ID: community, Name: "own", OwnerID: membershipID(8)}
	s.channels = nil
	if _, err := r.reconcileOnce(context.Background(), s); err == nil {
		t.Fatal("inaccessible channel accepted")
	}
	if len(s.acked) != 0 || n.applyCalls != 0 {
		t.Fatal("inaccessible channel mutated or acknowledged")
	}
}
func TestMembershipAckFailureRetriesDurableWelcomeAndCursor(t *testing.T) {
	r, n, s, _, _, group := membershipFixture(t)
	s.welcomes = []client.Welcome{{ID: 1, GroupID: group, Blob: []byte{1}, Tree: []byte{2}, TreeHash: make([]byte, 32)}}
	n.outcomes = []native.GroupWelcomeOutcome{{ID: 1, Group: group, Outcome: 0}}
	s.ackErr = errors.New("ack lost")
	if _, err := r.reconcileOnce(context.Background(), s); err == nil {
		t.Fatal("ack failure hidden")
	}
	if n.state.Revision != 0 {
		t.Fatal("cursor advanced before acknowledgement")
	}
	var err error
	r, err = newMembershipReconciler(n, sourceSyncOptions{}, membershipOptions{WelcomePageLimit: 8})
	if err != nil {
		t.Fatal(err)
	}
	s.ackErr = nil
	n.outcomes = []native.GroupWelcomeOutcome{{ID: 1, Group: group, Outcome: 1}}
	if _, err := r.reconcileOnce(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if n.applyCalls != 2 || len(s.acked) != 2 || n.state.Revision != 1 {
		t.Fatalf("apply=%d ack=%d revision=%d", n.applyCalls, len(s.acked), n.state.Revision)
	}
}

func TestMembershipSyncsOnlyGroupsWhoseNativeBindingMatchesAuthorizedChannel(t *testing.T) {
	r, n, s, community, channel, group := membershipFixture(t)
	groupInfo := native.GroupInfo{ID: group, Kind: 0, Community: &community, Target: channel, State: 2, NextSequence: 1}
	n.groups = []native.GroupInfo{groupInfo}
	n.inspected = groupInfo
	result, err := r.reconcileOnce(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if result.GroupsSynced != 1 || result.SourcePages != 0 {
		t.Fatalf("sync result=%+v", result)
	}
	n.inspected.Target = membershipID(77)
	if _, err = r.reconcileOnce(context.Background(), s); err == nil {
		t.Fatal("native group rebound to another channel was accepted")
	}
}

func TestMembershipTreatsNonmemberCallGroupSourceAsPending(t *testing.T) {
	for _, tc := range []struct {
		name    string
		kind    uint64
		code    string
		wantErr bool
	}{
		{name: "call group", kind: 1, code: "E_NOT_FOUND"},
		{name: "unexpected call error", kind: 1, code: "E_OTHER", wantErr: true},
		{name: "text group", kind: 0, code: "E_NOT_FOUND"},
		{name: "unexpected text error", kind: 0, code: "E_OTHER", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, n, s, community, channel, group := membershipFixture(t)
			channelKind := tc.kind
			s.channels = []client.Channel{{ID: channel, CommunityID: community, Kind: channelKind}}
			if tc.kind == 0 {
				s.channels[0].TextGroupID = &group
			}
			info := native.GroupInfo{ID: group, Kind: tc.kind, Community: &community, Target: channel, State: 2, NextSequence: 1}
			n.groups, n.inspected = []native.GroupInfo{info}, info
			s.groupInfo = client.GroupInfo{NextSequence: 2}
			s.groupPageErr = &client.APIError{Status: 404, Code: tc.code}
			_, err := r.reconcileOnce(context.Background(), s)
			if (err != nil) != tc.wantErr {
				t.Fatalf("reconcile error = %v, wantErr %v", err, tc.wantErr)
			}
			if s.groupPageCalls != 1 {
				t.Fatalf("group page calls = %d, want 1", s.groupPageCalls)
			}
		})
	}
}

func TestUnavailableCallSourceDoesNotBlockInstallationReconciliation(t *testing.T) {
	for _, kind := range []uint64{0, 1} {
		for _, source := range []string{"info", "page"} {
			t.Run(fmt.Sprintf("kind=%d/%s", kind, source), func(t *testing.T) {
				r, n, s, community, channel, group := membershipFixture(t)
				s.channels = []client.Channel{{ID: channel, CommunityID: community, Kind: kind}}
				if kind == 0 {
					s.channels[0].TextGroupID = &group
				}
				info := native.GroupInfo{ID: group, Kind: kind, Community: &community, Target: channel, State: 2, NextSequence: 1}
				n.groups, n.inspected = []native.GroupInfo{info}, info
				s.groupInfo = client.GroupInfo{NextSequence: 2}
				refusal := &client.APIError{Status: 403, Code: "E_FORBIDDEN"}
				if source == "info" {
					s.groupInfoErr = refusal
				} else {
					s.groupPageErr = refusal
				}
				for range 2 {
					result, err := r.reconcileOnce(context.Background(), s)
					if kind == 1 && err != nil {
						t.Fatalf("unavailable call blocked installation reconciliation: %v", err)
					}
					if kind == 0 && !errors.Is(err, refusal) {
						t.Fatalf("text refusal must remain terminal: %v", err)
					}
					if result.GroupsSynced != 0 {
						t.Fatal("denied source was counted as synchronized")
					}
				}
				if s.groupInfoCalls != 2 || source == "page" && s.groupPageCalls != 2 {
					t.Fatal("denied source was not retried on the next pass")
				}
				if n.genericApplyCalls != 0 || n.genericAckCalls != 0 {
					t.Fatal("denied call altered native source progress")
				}
			})
		}
	}
}

func TestMembershipRechecksAccessBeforeAcknowledgingWelcome(t *testing.T) {
	r, n, s, _, _, group := membershipFixture(t)
	s.welcomes = []client.Welcome{{ID: 3, GroupID: group, Blob: []byte{1}, Tree: []byte{2}, TreeHash: make([]byte, 32)}}
	n.outcomes = []native.GroupWelcomeOutcome{{ID: 3, Group: group, Outcome: 0}}
	s.removeOnSecond = true
	if _, err := r.reconcileOnce(context.Background(), s); !errors.Is(err, ErrMembershipUnproven) {
		t.Fatalf("error=%v", err)
	}
	if len(s.acked) != 0 || n.state.Revision != 0 {
		t.Fatal("revoked channel welcome was acknowledged or cursor advanced")
	}
}

func TestMembershipRechecksTextGroupAssociationBeforeAcknowledging(t *testing.T) {
	r, n, s, _, _, group := membershipFixture(t)
	s.welcomes = []client.Welcome{{ID: 4, GroupID: group, Blob: []byte{1}, Tree: []byte{2}, TreeHash: make([]byte, 32)}}
	n.outcomes = []native.GroupWelcomeOutcome{{ID: 4, Group: group, Outcome: 0}}
	s.changeGroupOnSecond = true
	if _, err := r.reconcileOnce(context.Background(), s); !errors.Is(err, ErrMembershipUnproven) {
		t.Fatalf("error=%v", err)
	}
	if len(s.acked) != 0 || n.state.Revision != 0 {
		t.Fatal("welcome acknowledged after text group association changed")
	}
}

func TestMembershipRechecksVoiceGroupTargetBeforeAcknowledging(t *testing.T) {
	r, n, s, community, channel, group := membershipFixture(t)
	s.channels[0].Kind = 1
	s.channels[0].TextGroupID = nil
	n.groups = []native.GroupInfo{{ID: group, Kind: 1, Community: &community, Target: channel, State: 2}}
	n.inspected = n.groups[0]
	n.changeGroupOnInspect = true
	s.welcomes = []client.Welcome{{ID: 5, GroupID: group, Blob: []byte{1}, Tree: []byte{2}, TreeHash: make([]byte, 32)}}
	n.outcomes = []native.GroupWelcomeOutcome{{ID: 5, Group: group, Outcome: 1}}
	if _, err := r.reconcileOnce(context.Background(), s); !errors.Is(err, ErrMembershipUnproven) {
		t.Fatalf("error=%v", err)
	}
	if len(s.acked) != 0 || n.state.Revision != 0 {
		t.Fatal("welcome acknowledged after voice group target changed")
	}
}

func TestMembershipReconcilerUsesExactBoundCommunityRead(t *testing.T) {
	r, _, s, community, _, _ := membershipFixture(t)
	s.community = client.Community{ID: community, Name: "own", OwnerID: membershipID(8)}
	result, err := r.reconcileOnce(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if s.communityCalls != 1 || s.communitiesCalls != 0 {
		t.Fatalf("community reads: exact=%d global=%d", s.communityCalls, s.communitiesCalls)
	}
	_ = result
}

func TestMembershipReconcilerRejectsMismatchedBoundCommunityResponse(t *testing.T) {
	r, n, s, _, _, _ := membershipFixture(t)
	s.community = client.Community{ID: membershipID(99), Name: "other", OwnerID: membershipID(8)}
	if _, err := r.reconcileOnce(context.Background(), s); err == nil {
		t.Fatal("mismatched community response accepted")
	}
	if n.applyCalls != 0 {
		t.Fatal("mismatched community caused a mutation")
	}
}
