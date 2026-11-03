//go:build linux

package main

import (
	"context"
	"errors"
	"testing"

	"github.com/dilla-chat/dilla/internal/bot/client"
	"github.com/dilla-chat/dilla/internal/bot/native"
	"github.com/dilla-chat/dilla/internal/id"
)

type restoreNativeFake struct {
	binding  native.Binding
	groups   []native.InteractionGroupDescriptor
	snapshot native.InteractionHealSnapshot
	exports  int
}

func (f *restoreNativeFake) Binding() native.Binding { return f.binding }
func (f *restoreNativeFake) InteractionGroups(context.Context) ([]native.InteractionGroupDescriptor, error) {
	return f.groups, nil
}
func (f *restoreNativeFake) InteractionHealSnapshot(context.Context, id.ID) (native.InteractionHealSnapshot, error) {
	f.exports++
	return f.snapshot, nil
}

type restoreSessionFake struct {
	installation id.ID
	group        id.ID
	statuses     []client.InteractionHealStatus
	gets, posts  int
}

func (f *restoreSessionFake) BotInteractionHealStatus(_ context.Context, installation, group id.ID) (client.InteractionHealStatus, error) {
	if installation != f.installation || group != f.group || f.gets >= len(f.statuses) {
		return client.InteractionHealStatus{}, errors.New("wrong heal status scope")
	}
	status := f.statuses[f.gets]
	f.gets++
	return status, nil
}
func (f *restoreSessionFake) BotInteractionHeal(_ context.Context, installation, group id.ID, info []byte, tail [][]any, tree []byte) (client.HealResult, error) {
	if installation != f.installation || group != f.group || len(info) == 0 || len(tail) != 0 || len(tree) == 0 {
		return client.HealResult{}, errors.New("wrong heal payload or scope")
	}
	f.posts++
	return client.HealResult{Epoch: 3, NextSequence: 10}, nil
}

func restoreFixture() (*restoreNativeFake, *restoreSessionFake) {
	binding := native.Binding{Instance: id.New(), Community: id.New(), Installation: id.New()}
	group, botUser, botDevice, participant, participantDevice := id.New(), id.New(), id.New(), id.New(), id.New()
	d := native.InteractionGroupDescriptor{Group: group, BotDevice: botDevice, Epoch: 3, State: 2,
		Binding: native.InteractionGroupBinding{Instance: binding.Instance, Installation: binding.Installation, Community: binding.Community,
			BotUser: botUser, ParticipantUser: participant, ParticipantDevice: participantDevice, PolicyVersion: 1, E2EEVersion: 1}}
	n := &restoreNativeFake{binding: binding, groups: []native.InteractionGroupDescriptor{d},
		snapshot: native.InteractionHealSnapshot{GroupInfo: []byte{1}, Tail: [][]any{}, RatchetTree: []byte{2}}}
	s := &restoreSessionFake{installation: binding.Installation, group: group, statuses: []client.InteractionHealStatus{
		{HealStatus: client.HealStatus{Epoch: 2, NextSequence: 8, Generation: 5, NeedFromSequence: 8}, NeedsHeal: true},
		{HealStatus: client.HealStatus{Epoch: 3, NextSequence: 10, Generation: 5, NeedFromSequence: 10}},
	}}
	return n, s
}

func TestHealRestoredInteractionGroupsOnlyWhenNeeded(t *testing.T) {
	n, s := restoreFixture()
	healed, err := healRestoredInteractionGroups(context.Background(), n, s)
	if err != nil || healed != 1 || n.exports != 1 || s.posts != 1 || s.gets != 2 {
		t.Fatalf("healed=%d exports=%d posts=%d gets=%d err=%v", healed, n.exports, s.posts, s.gets, err)
	}
	n, s = restoreFixture()
	s.statuses = s.statuses[1:]
	healed, err = healRestoredInteractionGroups(context.Background(), n, s)
	if err != nil || healed != 0 || n.exports != 0 || s.posts != 0 || s.gets != 1 {
		t.Fatalf("healthy: healed=%d exports=%d posts=%d gets=%d err=%v", healed, n.exports, s.posts, s.gets, err)
	}
}

func TestHealRestoredInteractionGroupsRefusesMismatchedNativeBinding(t *testing.T) {
	n, s := restoreFixture()
	n.groups[0].Binding.Installation = id.New()
	if _, err := healRestoredInteractionGroups(context.Background(), n, s); err == nil || s.gets != 0 || n.exports != 0 {
		t.Fatalf("mismatched native binding reached network/helper: gets=%d exports=%d err=%v", s.gets, n.exports, err)
	}
}

func TestHealRestoredInteractionGroupsSkipsPendingEpochZero(t *testing.T) {
	n, s := restoreFixture()
	n.groups[0].State, n.groups[0].Epoch = 1, 0
	healed, err := healRestoredInteractionGroups(context.Background(), n, s)
	if err != nil || healed != 0 || s.gets != 0 || n.exports != 0 {
		t.Fatalf("pending group was healed: healed=%d gets=%d exports=%d err=%v", healed, s.gets, n.exports, err)
	}
}
