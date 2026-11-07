package service

// Bot v2 Task 16: the Bot is a member of Bot channel groups only (and of call groups while it plays). It joins a
// channel's group when the channel is marked and leaves it when the mark is removed; on its first start under v2 it
// leaves every channel group that is not a Bot channel, once.

import (
	"context"
	"log/slog"
	"sync"
	"testing"

	"github.com/dilla-chat/dilla/internal/bot/client"
	"github.com/dilla-chat/dilla/internal/bot/native"
	"github.com/dilla-chat/dilla/internal/id"
)

// recordedLogs is a slog handler that keeps every record's message and attributes.
type recordedLogs struct {
	mu   sync.Mutex
	recs []map[string]any
}

func (h *recordedLogs) Enabled(context.Context, slog.Level) bool { return true }
func (h *recordedLogs) Handle(_ context.Context, r slog.Record) error {
	m := map[string]any{"msg": r.Message}
	r.Attrs(func(a slog.Attr) bool { m[a.Key] = a.Value.Any(); return true })
	h.mu.Lock()
	h.recs = append(h.recs, m)
	h.mu.Unlock()
	return nil
}
func (h *recordedLogs) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recordedLogs) WithGroup(string) slog.Handler      { return h }

func (h *recordedLogs) with(msg string) []map[string]any {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []map[string]any
	for _, r := range h.recs {
		if r["msg"] == msg {
			out = append(out, r)
		}
	}
	return out
}

// botChannelSet is the feed's Bot channels as a test sets them.
type botChannelSet struct {
	mu     sync.Mutex
	marked map[id.ID]bool
	known  bool
}

func (b *botChannelSet) get() (map[id.ID]bool, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := map[id.ID]bool{}
	for k, v := range b.marked {
		out[k] = v
	}
	return out, b.known
}

func (b *botChannelSet) set(channel id.ID, marked bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.marked == nil {
		b.marked = map[id.ID]bool{}
	}
	if marked {
		b.marked[channel] = true
	} else {
		delete(b.marked, channel)
	}
	b.known = true
}

// TestBotJoinsOnMarkAndLeavesOnUnmark: with the Bot channels known, a Welcome to an unmarked channel's group is not
// applied (acknowledged, so it never blocks the page) and an unmarked channel's group is not synced. Marking the channel
// applies its Welcome and syncs the group; removing the mark leaves it again (no sync, nothing applied).
func TestBotJoinsOnMarkAndLeavesOnUnmark(t *testing.T) {
	r, n, s, community, channel, group := membershipFixture(t)
	channels := &botChannelSet{known: true}
	logs := &recordedLogs{}
	r.UseBotChannels(channels.get, slog.New(logs))
	welcome := func(wid uint64) client.Welcome {
		return client.Welcome{ID: wid, GroupID: group, Blob: []byte{1}, Tree: []byte{2}, TreeHash: make([]byte, 32)}
	}

	// Not a Bot channel: the Welcome is acknowledged but not applied.
	s.welcomes = []client.Welcome{welcome(1)}
	if _, err := r.reconcileOnce(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if n.applyCalls != 0 || len(s.acked) != 1 {
		t.Fatalf("unmarked: apply %d, acked %v; want no apply and the Welcome acknowledged", n.applyCalls, s.acked)
	}

	// Marked: the next Welcome is applied and the group synced.
	channels.set(channel, true)
	s.welcomes = append(s.welcomes, welcome(2))
	n.outcomes = []native.GroupWelcomeOutcome{{ID: 2, Group: group, Outcome: 0}}
	info := native.GroupInfo{ID: group, Kind: 0, Community: &community, Target: channel, State: 2, NextSequence: 1}
	n.groups, n.inspected = []native.GroupInfo{info}, info
	res, err := r.reconcileOnce(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if n.applyCalls != 1 || res.Applied != 1 || res.GroupsSynced != 1 {
		t.Fatalf("marked: apply %d, result %+v; want the Welcome applied and the group synced", n.applyCalls, res)
	}

	// Unmarked again: the group is left (not synced) and a later Welcome is not applied.
	channels.set(channel, false)
	s.welcomes = append(s.welcomes, welcome(3))
	pages := s.groupPageCalls
	res, err = r.reconcileOnce(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if n.applyCalls != 1 || res.GroupsSynced != 0 || s.groupPageCalls != pages {
		t.Fatalf("unmarked again: apply %d, result %+v, page reads %d; want nothing applied or synced", n.applyCalls, res, s.groupPageCalls-pages)
	}
}

// TestBotChannelsUnknownKeepMembership: until the feed has applied a snapshot, the Bot channels are unknown and
// membership is not narrowed (an empty set before the snapshot must not leave every group).
func TestBotChannelsUnknownKeepMembership(t *testing.T) {
	r, n, s, community, channel, group := membershipFixture(t)
	r.UseBotChannels((&botChannelSet{}).get, nil)
	info := native.GroupInfo{ID: group, Kind: 0, Community: &community, Target: channel, State: 2, NextSequence: 1}
	n.groups, n.inspected = []native.GroupInfo{info}, info
	res, err := r.reconcileOnce(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if res.GroupsSynced != 1 || len(n.other) != 0 {
		t.Fatalf("result %+v, state %v; want the group synced and no migration marker", res, n.other)
	}
}

// TestFirstV2StartLeavesNonBotChannelGroups: the first reconcile with the Bot channels known leaves every channel
// group that is not a Bot channel, logging "bot left non-Bot channel group" with the channel id, and persists a marker.
// A second reconcile (and a new reconciler over the same state, as after a restart) logs nothing again; the Bot
// channel's group stays.
func TestFirstV2StartLeavesNonBotChannelGroups(t *testing.T) {
	r, n, s, community, channel, group := membershipFixture(t)
	other, otherGroup := membershipID(30), membershipID(31)
	s.channels = append(s.channels, client.Channel{ID: other, CommunityID: community, Kind: 0, TextGroupID: &otherGroup})
	n.groups = []native.GroupInfo{
		{ID: group, Kind: 0, Community: &community, Target: channel, State: 2, NextSequence: 1},
		{ID: otherGroup, Kind: 0, Community: &community, Target: other, State: 2, NextSequence: 1},
	}
	n.inspected = n.groups[0]
	channels := &botChannelSet{}
	channels.set(channel, true)
	logs := &recordedLogs{}
	r.UseBotChannels(channels.get, slog.New(logs))
	res, err := r.reconcileOnce(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	left := logs.with("bot left non-Bot channel group")
	if len(left) != 1 || left[0]["channel_id"] != other.String() {
		t.Fatalf("left %v; want exactly the non-Bot channel %s", left, other)
	}
	if res.GroupsSynced != 1 {
		t.Fatalf("synced %d groups, want 1 (the Bot channel's)", res.GroupsSynced)
	}
	if v := n.other[botChannelMigrationKey]; v.Revision != 1 {
		t.Fatalf("migration marker %+v, want persisted once", v)
	}
	if _, err = r.reconcileOnce(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	again, err := newMembershipReconciler(n, sourceSyncOptions{}, membershipOptions{WelcomePageLimit: 8})
	if err != nil {
		t.Fatal(err)
	}
	again.UseBotChannels(channels.get, slog.New(logs))
	if _, err = again.reconcileOnce(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if got := len(logs.with("bot left non-Bot channel group")); got != 1 {
		t.Fatalf("the migration ran %d times, want once", got)
	}
	if v := n.other[botChannelMigrationKey]; v.Revision != 1 {
		t.Fatalf("migration marker rewritten: %+v", v)
	}
}
