//go:build linux

package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jonasthim/dilla/internal/bot/client"
	"github.com/jonasthim/dilla/internal/bot/helper"
	"github.com/jonasthim/dilla/internal/bot/music"
	"github.com/jonasthim/dilla/internal/bot/music/persistence"
	"github.com/jonasthim/dilla/internal/bot/native"
	"github.com/jonasthim/dilla/internal/bot/protocol"
	"github.com/jonasthim/dilla/internal/bot/service"
	"github.com/jonasthim/dilla/internal/cborx"
	"github.com/jonasthim/dilla/internal/id"
)

type musicCallMembershipReaderFake struct{ err error }

func (f musicCallMembershipReaderFake) GroupPage(context.Context, id.ID, string, uint64, uint64) (client.Page, error) {
	return client.Page{}, f.err
}

type musicCallMembershipForgetterFake struct {
	calls  int
	groups []id.ID
	err    error
}

func (f *musicCallMembershipForgetterFake) CallGroupForget(_ context.Context, group id.ID) error {
	f.calls++
	f.groups = append(f.groups, group)
	return f.err
}

func TestMusicCallResumeForgetsOnlyDefinitivelyRemovedLeaf(t *testing.T) {
	group := id.New()
	t.Run("current member keeps local keys", func(t *testing.T) {
		forgetter := &musicCallMembershipForgetterFake{}
		current, err := prepareMusicCallGroupMembership(context.Background(), musicCallMembershipReaderFake{}, forgetter, group)
		if err != nil || !current || forgetter.calls != 0 {
			t.Fatalf("current=%v forgets=%d err=%v", current, forgetter.calls, err)
		}
	})
	t.Run("removed leaf discards stale keys before rejoin", func(t *testing.T) {
		forgetter := &musicCallMembershipForgetterFake{}
		reader := musicCallMembershipReaderFake{err: &client.APIError{Status: 404, Code: "E_NOT_FOUND"}}
		current, err := prepareMusicCallGroupMembership(context.Background(), reader, forgetter, group)
		if err != nil || current || forgetter.calls != 1 {
			t.Fatalf("current=%v forgets=%d err=%v", current, forgetter.calls, err)
		}
	})
	t.Run("transient or pruned errors never trigger forget", func(t *testing.T) {
		for _, probeErr := range []error{
			&client.APIError{Status: 503, Code: "E_UNAVAILABLE"},
			&client.APIError{Status: 410, Code: "E_PRUNED"},
		} {
			forgetter := &musicCallMembershipForgetterFake{}
			current, err := prepareMusicCallGroupMembership(context.Background(), musicCallMembershipReaderFake{err: probeErr}, forgetter, group)
			var apiErr *client.APIError
			_ = errors.As(probeErr, &apiErr)
			wantCurrent := apiErr != nil && apiErr.Code == "E_PRUNED"
			wantErr := !wantCurrent
			if (err != nil) != wantErr || current != wantCurrent || forgetter.calls != 0 {
				t.Fatalf("probe=%v current=%v forgets=%d err=%v", probeErr, current, forgetter.calls, err)
			}
		}
	})
}

func TestMusicCallJoinForgetsSupersededGroupForSameChannel(t *testing.T) {
	community, channel, current, old := id.New(), id.New(), id.New(), id.New()
	otherCommunity, otherChannel := id.New(), id.New()
	groups := []native.GroupInfo{
		{ID: old, Kind: 1, Community: &community, Target: channel, State: 2},
		{ID: current, Kind: 1, Community: &community, Target: channel, State: 2},
		{ID: id.New(), Kind: 1, Community: &community, Target: otherChannel, State: 2},
		{ID: id.New(), Kind: 1, Community: &otherCommunity, Target: channel, State: 2},
		{ID: id.New(), Kind: 0, Community: &community, Target: channel, State: 2},
		{ID: id.New(), Kind: 3, Community: &community, Target: channel, State: 2},
		{ID: id.New(), Kind: 1, Community: &community, Target: channel, State: 4},
	}
	f := &musicCallMembershipForgetterFake{}
	if err := forgetSupersededMusicCallGroups(context.Background(), f, groups, community, channel, current); err != nil {
		t.Fatal(err)
	}
	if len(f.groups) != 1 || f.groups[0] != old {
		t.Fatalf("forgot groups %v, want only superseded call %v", f.groups, old)
	}
	f.err = errors.New("storage unavailable")
	if err := forgetSupersededMusicCallGroups(context.Background(), f, groups, community, channel, current); !errors.Is(err, f.err) {
		t.Fatalf("forget failure ignored: %v", err)
	}
}

type musicJoinCallsFake struct {
	musicCallJoinReader
	channel, group      id.ID
	reads, infos, trees int
	epoch               uint64
}

func (f *musicJoinCallsFake) ChannelCall(context.Context, id.ID) (client.ChannelCallState, error) {
	f.reads++
	return client.ChannelCallState{CallID: &f.channel, GroupID: &f.group}, nil
}
func (f *musicJoinCallsFake) GroupInfo(context.Context, id.ID) (client.GroupInfo, error) {
	f.infos++
	f.epoch++
	return client.GroupInfo{Epoch: f.epoch, TreeHash: []byte{1}, Info: []byte{2}}, nil
}
func (f *musicJoinCallsFake) GroupTree(context.Context, id.ID) (client.GroupTree, error) {
	f.trees++
	return client.GroupTree{Epoch: f.epoch, TreeHash: []byte{1}, Tree: []byte{3}}, nil
}
func (f *musicJoinCallsFake) Resync(context.Context, id.ID, []byte, []byte) (client.SequenceResult, error) {
	return client.SequenceResult{Sequence: 1}, nil
}

type musicJoinCoreFake struct {
	musicCallJoinCore
	group                                                id.ID
	conflicts, joins, reads, resolved, discards, forgets int
	terminal                                             bool
	operations                                           []id.ID
}

func (f *musicJoinCoreFake) Groups(context.Context) ([]native.GroupInfo, error) {
	f.reads++
	if f.terminal {
		return []native.GroupInfo{{ID: f.group, State: 1, Epoch: 2}}, nil
	}
	return nil, nil
}
func (f *musicJoinCoreFake) GroupIntentResult(_ context.Context, op id.ID) (*native.GroupIntentResult, error) {
	if f.terminal {
		if len(f.operations) == 0 {
			f.operations = append(f.operations, op)
		}
		if f.operations[0] != op {
			return nil, nil
		}
		return &native.GroupIntentResult{Kind: 5, Group: f.group, State: 2}, nil
	}
	return nil, nil
}
func (f *musicJoinCoreFake) ExternalJoin(_ context.Context, op, _, _ id.ID, _, _ uint64, _, _ []byte) (native.GroupExternalJoinResult, error) {
	f.joins++
	f.operations = append(f.operations, op)
	if f.joins <= f.conflicts {
		return native.GroupExternalJoinResult{}, &helper.StatusError{Status: 4}
	}
	return native.GroupExternalJoinResult{Commit: []byte{1}, GroupInfo: []byte{2}}, nil
}
func (f *musicJoinCoreFake) PendingLeaves(context.Context) ([]native.GroupPendingLeaf, error) {
	return nil, nil
}
func (f *musicJoinCoreFake) CallJoinCheck(context.Context, id.ID) error { return nil }
func (f *musicJoinCoreFake) GroupJoined(context.Context, id.ID, id.ID, uint64) error {
	f.resolved++
	return nil
}
func (f *musicJoinCoreFake) CallMediaEpoch(context.Context, id.ID) (native.CallMediaEpoch, error) {
	return native.CallMediaEpoch{}, nil
}
func (f *musicJoinCoreFake) DiscardGroup(context.Context, id.ID, id.ID) error {
	f.discards++
	return nil
}
func (f *musicJoinCoreFake) CallGroupForget(context.Context, id.ID) error {
	f.forgets++
	f.terminal = false
	return nil
}

func TestMusicCallJoinConflictRefetchesAndSucceeds(t *testing.T) {
	calls := &musicJoinCallsFake{channel: id.New(), group: id.New()}
	core := &musicJoinCoreFake{conflicts: 1}
	var delays []time.Duration
	wait := func(_ context.Context, d time.Duration) error { delays = append(delays, d); return nil }
	err := retryMusicCallGroupJoin(t.Context(), calls, core, id.New(), id.New(), calls.channel, 1, wait)
	if err != nil || core.joins != 2 || core.resolved != 1 || calls.reads != 2 || calls.infos != 2 || calls.trees != 2 || core.reads != 2 || len(delays) != 1 || delays[0] != 250*time.Millisecond {
		t.Fatalf("join err=%v core=%+v calls=%+v delays=%v", err, core, calls, delays)
	}
	if core.operations[0] == core.operations[1] {
		t.Fatal("retry did not use refreshed epoch")
	}
}
func TestMusicCallJoinPersistentConflictRemainsRetryable(t *testing.T) {
	calls := &musicJoinCallsFake{channel: id.New(), group: id.New()}
	core := &musicJoinCoreFake{conflicts: 4}
	var delays []time.Duration
	wait := func(_ context.Context, d time.Duration) error { delays = append(delays, d); return nil }
	installation, community := id.New(), id.New()
	err := retryMusicCallGroupJoin(t.Context(), calls, core, installation, community, calls.channel, 1, wait)
	var conflict *errMusicCallJoinConflict
	if !errors.As(err, &conflict) || !conflict.Retryable() || core.joins != 4 || len(delays) != 3 || delays[0] != 250*time.Millisecond || delays[1] != time.Second || delays[2] != 2*time.Second {
		t.Fatalf("err=%v joins=%d delays=%v", err, core.joins, delays)
	}
	if err = retryMusicCallGroupJoin(t.Context(), calls, core, installation, community, calls.channel, 1, wait); err != nil || core.joins != 5 || core.resolved != 1 {
		t.Fatalf("next Resume: err=%v core=%+v", err, core)
	}
}
func TestMusicCallJoinDiscardsNonResumableJoiningRecordOnce(t *testing.T) {
	calls := &musicJoinCallsFake{channel: id.New(), group: id.New()}
	core := &musicJoinCoreFake{group: calls.group, terminal: true}
	err := retryMusicCallGroupJoin(t.Context(), calls, core, id.New(), id.New(), calls.channel, 1, func(context.Context, time.Duration) error { return nil })
	if err != nil || core.discards != 1 || core.forgets != 1 || core.joins != 1 || calls.reads != 2 {
		t.Fatalf("err=%v core=%+v reads=%d", err, core, calls.reads)
	}
}

// A failed trust gate leaves a durable join. Resume must recover it even when an
// older operation at the same base epoch was discarded before this attempt.
type musicJoinRecoveryCore struct {
	musicJoinCoreFake
	initial, staged id.ID
	gateCalls       int
}

func (f *musicJoinRecoveryCore) Groups(context.Context) ([]native.GroupInfo, error) {
	if !f.staged.IsZero() {
		return []native.GroupInfo{{ID: f.group, State: 1, Epoch: 2}}, nil
	}
	return nil, nil
}
func (f *musicJoinRecoveryCore) GroupIntentResult(_ context.Context, op id.ID) (*native.GroupIntentResult, error) {
	if f.initial.IsZero() {
		f.initial = op
	}
	if op == f.initial {
		return &native.GroupIntentResult{Kind: 5, Group: f.group, State: 2}, nil
	}
	if op == f.staged {
		body, err := cborx.Marshal([]any{[]byte{1}, []byte{2}})
		return &native.GroupIntentResult{Kind: 5, Group: f.group, Result: body}, err
	}
	return nil, nil
}
func (f *musicJoinRecoveryCore) ExternalJoin(_ context.Context, op, _, _ id.ID, _, _ uint64, _, _ []byte) (native.GroupExternalJoinResult, error) {
	f.staged = op
	f.joins++
	return native.GroupExternalJoinResult{Commit: []byte{1}, GroupInfo: []byte{2}}, nil
}
func (f *musicJoinRecoveryCore) CallJoinCheck(context.Context, id.ID) error {
	f.gateCalls++
	if f.gateCalls == 1 {
		return errors.New("trust refresh required")
	}
	return nil
}
func TestMusicCallJoinResumeRecoversRetryOperation(t *testing.T) {
	calls := &musicJoinCallsFake{channel: id.New(), group: id.New()}
	core := &musicJoinRecoveryCore{musicJoinCoreFake: musicJoinCoreFake{group: calls.group}}
	installation, community := id.New(), id.New()
	wait := func(context.Context, time.Duration) error { return nil }
	if err := retryMusicCallGroupJoin(t.Context(), calls, core, installation, community, calls.channel, 1, wait); err == nil {
		t.Fatal("expected staged media gate failure")
	}
	if err := retryMusicCallGroupJoin(t.Context(), calls, core, installation, community, calls.channel, 1, wait); err != nil {
		t.Fatal(err)
	}
	if core.joins != 1 || core.resolved != 1 || core.discards != 0 || core.forgets != 0 || calls.trees != 1 {
		t.Fatalf("did not recover staged upload: core=%+v trees=%d", core, calls.trees)
	}
}

func TestMusicCallJoinBackoffHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := waitMusicCallJoin(ctx, 2*time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("wait=%v", err)
	}
}

type unusableMusicJoinCore struct{ musicJoinCoreFake }

func (f *unusableMusicJoinCore) GroupIntentResult(context.Context, id.ID) (*native.GroupIntentResult, error) {
	return &native.GroupIntentResult{Kind: 5, Group: f.group, State: 0}, nil
}
func TestMusicCallJoinUnusablePendingOutsideJoiningIsTerminal(t *testing.T) {
	calls := &musicJoinCallsFake{channel: id.New(), group: id.New()}
	core := &unusableMusicJoinCore{musicJoinCoreFake: musicJoinCoreFake{group: calls.group}}
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	defer slog.SetDefault(previous)
	waits := 0
	err := retryMusicCallGroupJoin(t.Context(), calls, core, id.New(), id.New(), calls.channel, 1, func(context.Context, time.Duration) error { waits++; return nil })
	var terminal *errMusicCallJoinUnusable
	if !errors.As(err, &terminal) || terminal.Retryable() || waits != 0 || calls.reads != 1 || core.discards != 0 || core.forgets != 0 {
		t.Fatalf("error=%v waits=%d reads=%d discards=%d forgets=%d", err, waits, calls.reads, core.discards, core.forgets)
	}
	if logs.Len() != 0 {
		t.Fatalf("logs=%s", logs.String())
	}
}

type joinFailureMusicAuthority struct{ channel id.ID }

func (a joinFailureMusicAuthority) CurrentCall(context.Context, id.ID) (service.MusicCallBinding, error) {
	return service.MusicCallBinding{CallID: a.channel, ChannelID: a.channel, Active: true}, nil
}
func (joinFailureMusicAuthority) CanUse(context.Context, service.InteractionActor, service.MusicCallBinding) (bool, error) {
	return true, nil
}
func (joinFailureMusicAuthority) CanDJ(context.Context, service.InteractionActor, service.MusicCallBinding) (bool, error) {
	return true, nil
}
func (joinFailureMusicAuthority) CanEmergencyStop(context.Context, service.InteractionActor, service.MusicCallBinding) (bool, error) {
	return true, nil
}

func TestMusicCallJoinUnusablePendingLogsOnceAtInteraction(t *testing.T) {
	channel, community, installation := id.New(), id.New(), id.New()
	n := &runtimeStateNative{binding: native.Binding{Instance: id.New(), Community: community, Installation: installation}, rows: map[string]native.StateValue{}}
	store, err := persistence.New(n, community)
	if err != nil {
		t.Fatal(err)
	}
	queued, err := music.Apply(music.InitialState(), 0, music.Operation{Kind: music.OpEnqueue, Tracks: []music.Track{{ID: "track", Provider: music.ProviderYouTube, CanonicalID: "abcdefghijk", Title: "A", RequesterID: "member"}}})
	if err != nil {
		t.Fatal(err)
	}
	saved, err := store.Save(t.Context(), channel, 0, queued)
	if err != nil {
		t.Fatal(err)
	}
	calls := &musicJoinCallsFake{channel: channel, group: id.New()}
	core := &unusableMusicJoinCore{musicJoinCoreFake: musicJoinCoreFake{group: calls.group}}
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	previous := slog.Default()
	slog.SetDefault(logger)
	defer slog.SetDefault(previous)
	h := service.MusicInteractions{Store: store, Authority: joinFailureMusicAuthority{channel}, CommunityID: community, InstallationID: installation, Logger: logger,
		OnPersistedSnapshot: func(ctx context.Context, _, _, _ id.ID, _ music.State, revision uint64, _ bool) (uint64, error) {
			return revision, retryMusicCallGroupJoin(ctx, calls, core, installation, community, channel, 1, func(context.Context, time.Duration) error { t.Fatal("terminal error retried"); return nil })
		}}
	result, err := h.Handlers()[protocol.OpMusicResume].Handle(t.Context(), service.InteractionActor{User: id.New()}, protocol.Request{CommunityID: community, ChannelID: &channel, Revision: &saved.Revision, Args: []any{}})
	if err != nil || result.Status != 5 {
		t.Fatalf("result=%+v error=%v", result, err)
	}
	if strings.Count(logs.String(), "\n") != 1 || !strings.Contains(logs.String(), "unusable_pending") {
		t.Fatalf("logs=%s", logs.String())
	}
}
