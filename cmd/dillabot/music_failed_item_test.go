//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/bot/client"
	"github.com/dilla-chat/dilla/internal/bot/helper"
	"github.com/dilla-chat/dilla/internal/bot/music"
	"github.com/dilla-chat/dilla/internal/bot/music/persistence"
	"github.com/dilla-chat/dilla/internal/bot/music/session"
	"github.com/dilla-chat/dilla/internal/bot/native"
	"github.com/dilla-chat/dilla/internal/id"
)

// dyingStateNative is a helper whose queue store is on the helper itself (as native.Client's StateTransaction is): once
// dead, every read and write fails with helper.ErrClosed and Usable is false.
type dyingStateNative struct {
	*runtimeStateNative
	dead atomic.Bool
}

func (n *dyingStateNative) Usable() bool { return !n.dead.Load() }

func (n *dyingStateNative) StateGet(ctx context.Context, area uint64, key []byte) (native.StateValue, error) {
	if n.dead.Load() {
		return native.StateValue{}, helper.ErrClosed
	}
	return n.runtimeStateNative.StateGet(ctx, area, key)
}

func (n *dyingStateNative) StateTransaction(ctx context.Context, tx id.ID, changes []native.StateChange) ([]uint64, error) {
	if n.dead.Load() {
		return nil, helper.ErrClosed
	}
	return n.runtimeStateNative.StateTransaction(ctx, tx, changes)
}

// killingPCM fails the broken track by killing the helper under it (the production wrong-track case); others play.
type killingPCM struct{ kill func() }

func (p killingPCM) Start(ctx context.Context, input string) (io.ReadCloser, error) {
	if strings.Contains(input, "bbcdefghijk") {
		p.kill()
		return nil, helper.ErrClosed
	}
	return (liveRuntimePCM{}).Start(ctx, input)
}

type failedItemRig struct {
	installation, community, channel id.ID
	channels                         []client.Channel
	rows                             map[string]native.StateValue
	registry                         *musicQueueRegistry
	current                          atomic.Pointer[dyingStateNative]
	starterFor                       musicStarterFactory
}

func newFailedItemRig(t *testing.T) *failedItemRig {
	t.Helper()
	r := &failedItemRig{installation: id.New(), community: id.New(), channel: id.New(), rows: map[string]native.StateValue{}, registry: newMusicQueueRegistry()}
	t.Cleanup(func() { _ = r.registry.Close() })
	r.channels = []client.Channel{{ID: r.channel, CommunityID: r.community, Kind: musicVoiceChannelKind}}
	r.starterFor = func(channel id.ID) (session.QueueStarter, error) {
		calls := &runtimeCalls{call: client.Call{CallID: channel, GroupID: id.New(), LiveKitURL: "wss://media.example", Token: "secret"}}
		return newMusicQueueStarter(musicRuntimeDeps{Community: r.community, Channel: channel, Calls: calls,
			JoinCallGroup: func(context.Context) error { return nil }, YouTubePCM: killingPCM{kill: func() { r.current.Load().dead.Store(true) }},
			Encoder: liveRuntimeEncoderStarter{}, Publisher: &liveRuntimePublisherStarter{starts: map[id.ID]int{}}, Epochs: runtimeEpochs{}, Lifetime: time.Minute})
	}
	return r
}

// helper starts a new helper on the same on-disk store and reconciles the registry with it, as a pass does.
func (r *failedItemRig) helper(t *testing.T) *persistence.Store {
	t.Helper()
	n := &dyingStateNative{runtimeStateNative: &runtimeStateNative{binding: native.Binding{Instance: id.New(), Community: r.community, Installation: r.installation}, rows: r.rows}}
	r.current.Store(n)
	if _, err := r.registry.ReconcileWithStarterFactory(t.Context(), r.installation, r.community, botMusicCapability, r.channels, n, r.starterFor); err != nil {
		t.Fatalf("reconcile = %v", err)
	}
	store, err := persistence.New(n, r.community)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

// play is a member's /play of track: appended last, then resumed, as the snapshot commit does.
func (r *failedItemRig) play(t *testing.T, store *persistence.Store, track music.Track) error {
	t.Helper()
	r.commit(t, store, music.Operation{Kind: music.OpEnqueue, Tracks: []music.Track{track}})
	saved := r.commit(t, store, music.Operation{Kind: music.OpResume})
	_, err := r.registry.ApplyPersistedSnapshot(t.Context(), r.installation, r.community, r.channel, saved.State, saved.Revision, true)
	return err
}

// commit applies op to the stored queue and saves it.
func (r *failedItemRig) commit(t *testing.T, store *persistence.Store, op music.Operation) persistence.Snapshot {
	t.Helper()
	stored, err := store.Load(t.Context(), r.channel)
	if err != nil {
		t.Fatal(err)
	}
	state, err := music.Apply(stored.State, stored.Revision, op)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := store.Save(t.Context(), r.channel, stored.Revision, state)
	if err != nil {
		t.Fatal(err)
	}
	return saved
}

func captureDefaultLog(t *testing.T) *lockedLog {
	t.Helper()
	logs := &lockedLog{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return logs
}

// v0.1.0-bot2.2 final review I1: the queue is stored on the helper, so when the helper died under a track its removal
// could not be saved and the replacement restored it (paused); the next /play appended B and resumed A. The registry
// keeps the failed item and removes it on the replacement before anything starts.
func TestFailedTrackRemovedAfterHelperRestart(t *testing.T) {
	logs := captureDefaultLog(t)
	r := newFailedItemRig(t)
	store := r.helper(t)
	broken := music.Track{ID: "broken-item", Provider: music.ProviderYouTube, CanonicalID: "bbcdefghijk", Title: "Broken title", RequesterID: "member"}
	if err := r.play(t, store, broken); err == nil {
		t.Fatal("the broken track started")
	}
	// The replacement helper, on the same store.
	store = r.helper(t)
	durable, err := store.Load(t.Context(), r.channel)
	if err != nil {
		t.Fatal(err)
	}
	if durable.State.Current != nil || len(durable.State.Queue) != 0 {
		t.Fatalf("durable state after the restart = %+v, want the failed track gone", durable.State)
	}
	if c := logs.count("bot music failed track removed after helper restart"); c != 1 || !strings.Contains(logs.String(), "item_id=broken-item") {
		t.Fatalf("%d removal lines:\n%s", c, logs)
	}
	if strings.Contains(logs.String(), "Broken title") || strings.Contains(logs.String(), "bbcdefghijk") {
		t.Fatalf("the log names the track:\n%s", logs)
	}
	next := music.Track{ID: "next-item", Provider: music.ProviderYouTube, CanonicalID: "abcdefghijk", Title: "Next", RequesterID: "member"}
	if err := r.play(t, store, next); err != nil {
		t.Fatalf("/play of the new track = %v", err)
	}
	supervisor, err := r.registry.supervisor(r.installation, r.community, r.channel)
	if err != nil {
		t.Fatal(err)
	}
	if state := supervisor.State(); state.Current == nil || state.Current.ID != next.ID || state.Paused || len(state.Queue) != 0 || !supervisor.Playing() {
		t.Fatalf("after /play: %+v, want the new track playing alone", state)
	}
	// The entry is spent: a later restart removes nothing.
	store = r.helper(t)
	if durable, err = store.Load(t.Context(), r.channel); err != nil || durable.State.Current == nil || durable.State.Current.ID != next.ID {
		t.Fatalf("after a further restart: %+v, %v", durable.State, err)
	}
}

// A failed item whose track is no longer current (a member skipped it, or the save went through after all) is
// forgotten without removing anything.
func TestStaleFailedItemIsClearedWithoutRemoval(t *testing.T) {
	logs := captureDefaultLog(t)
	r := newFailedItemRig(t)
	store := r.helper(t)
	kept := music.Track{ID: "kept-item", Provider: music.ProviderYouTube, CanonicalID: "abcdefghijk", Title: "Kept", RequesterID: "member"}
	r.commit(t, store, music.Operation{Kind: music.OpEnqueue, Tracks: []music.Track{kept}})
	r.commit(t, store, music.Operation{Kind: music.OpResume})
	r.registry.recordFailedItem(r.installation, r.channel, "gone-item")
	r.current.Load().dead.Store(true)
	store = r.helper(t)
	durable, err := store.Load(t.Context(), r.channel)
	if err != nil {
		t.Fatal(err)
	}
	if durable.State.Current == nil || durable.State.Current.ID != kept.ID {
		t.Fatalf("durable = %+v, want the current track kept", durable.State)
	}
	if r.registry.hasFailedItems(r.installation) {
		t.Fatal("the stale entry was not cleared")
	}
	if c := logs.count("bot music failed track removed after helper restart"); c != 0 {
		t.Fatalf("a stale entry logged a removal:\n%s", logs)
	}
	// A save failure that is not the helper's, on a usable helper, leaves no entry: the supervisor's own pause handles
	// it. A helper failure, or any failure on a helper that is no longer usable, is recorded.
	record := r.registry.failedItemRecorder(r.installation, r.channel, r.current.Load())
	record(t.Context(), "kept-item", errors.New("dillad 500"))
	if r.registry.hasFailedItems(r.installation) {
		t.Fatal("a non-helper save failure was recorded")
	}
	record(t.Context(), "kept-item", fmt.Errorf("save: %w", helper.ErrOutcomeUnknown))
	if !r.registry.hasFailedItems(r.installation) {
		t.Fatal("a helper save failure was not recorded")
	}
	r.registry.clearFailedItem(r.installation, r.channel, "kept-item")
	r.current.Load().dead.Store(true)
	record(t.Context(), "kept-item", errors.New("readback failed"))
	if !r.registry.hasFailedItems(r.installation) {
		t.Fatal("a save failure on a dead helper was not recorded")
	}
}
