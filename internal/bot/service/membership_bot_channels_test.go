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
	"github.com/dilla-chat/dilla/internal/bot/helper"
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
	mu          sync.Mutex
	marked      map[id.ID]bool
	known       bool
	left, reAdd map[id.ID]bool
}

func (b *botChannelSet) Marked() (map[id.ID]bool, bool) {
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

func (b *botChannelSet) Membership(left, needsReAdd map[id.ID]bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.left, b.reAdd = left, needsReAdd
}

func (b *botChannelSet) reported() (left, reAdd map[id.ID]bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.left, b.reAdd
}

// TestBotJoinsOnMarkAndLeavesOnUnmark (fix round 1, I-1): with the Bot channels known, the one Welcome to an unmarked
// channel's group is kept unapplied and acknowledged; the group is neither joined nor synced. Marking the channel later
// joins the Bot from that kept Welcome, with no second Welcome, and the next pass syncs the group. Removing the mark
// leaves it again: not synced, and reported as left.
func TestBotJoinsOnMarkAndLeavesOnUnmark(t *testing.T) {
	r, n, s, community, channel, group := membershipFixture(t)
	channels := &botChannelSet{known: true}
	logs := &recordedLogs{}
	r.UseBotChannels(channels, slog.New(logs))
	s.welcomes = []client.Welcome{{ID: 1, GroupID: group, Epoch: 3, CommitSequence: 7, Blob: []byte{1}, Tree: []byte{2}, TreeHash: make([]byte, 32)}}

	// Not a Bot channel: the Welcome is kept and acknowledged, not applied.
	if _, err := r.reconcileOnce(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if n.applyCalls != 0 || len(s.acked) != 1 {
		t.Fatalf("unmarked: apply %d, acked %v; want no apply and the Welcome acknowledged", n.applyCalls, s.acked)
	}
	if _, reAdd := channels.reported(); len(reAdd) != 0 {
		t.Fatalf("an unmarked channel reported as needing re-add: %v", reAdd)
	}

	// Marked, and no second Welcome: the kept one joins the Bot.
	channels.set(channel, true)
	n.outcomes = []native.GroupWelcomeOutcome{{ID: 1, Group: group, Outcome: 0}}
	info := native.GroupInfo{ID: group, Kind: 0, Community: &community, Target: channel, State: 2, NextSequence: 1}
	n.inspected = info // revalidation after the join
	res, err := r.reconcileOnce(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if n.applyCalls != 1 || res.Applied != 1 || !res.More || len(n.welcomes) != 1 || n.welcomes[0].ID != 1 || n.welcomes[0].Epoch != 3 || n.welcomes[0].CommitSequence != 7 {
		t.Fatalf("marked: apply %d, result %+v, applied %+v; want the kept Welcome applied", n.applyCalls, res, n.welcomes)
	}
	if len(n.expected) != 1 || n.expected[0].Channel != channel || n.expected[0].Kind != 0 {
		t.Fatalf("expected binding %+v", n.expected)
	}
	if got := len(logs.with("bot joined Bot channel group from its kept Welcome")); got != 1 {
		t.Fatalf("join logged %d times", got)
	}
	// The next pass syncs the joined group; the kept Welcome is gone.
	n.groups = []native.GroupInfo{info}
	res, err = r.reconcileOnce(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if n.applyCalls != 1 || res.GroupsSynced != 1 {
		t.Fatalf("after the join: apply %d, result %+v; want the group synced and nothing applied again", n.applyCalls, res)
	}

	// Unmarked again: the group is left (not synced, reported left) and nothing is applied.
	channels.set(channel, false)
	pages := s.groupPageCalls
	res, err = r.reconcileOnce(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if n.applyCalls != 1 || res.GroupsSynced != 0 || s.groupPageCalls != pages {
		t.Fatalf("unmarked again: apply %d, result %+v, page reads %d; want nothing applied or synced", n.applyCalls, res, s.groupPageCalls-pages)
	}
	if left, _ := channels.reported(); !left[group] {
		t.Fatalf("left groups %v, want the unmarked channel's group", left)
	}
}

// TestAStaleOrMissingKeptWelcomeNeedsReAdd (fix round 1, I-1): a kept Welcome the helper refuses at mark time (its key
// package spent) cannot join the Bot: it is dropped and the channel is reported as needing the Bot re-added. A marked
// channel with no kept Welcome is reported the same way. Nothing is applied early.
func TestAStaleOrMissingKeptWelcomeNeedsReAdd(t *testing.T) {
	r, n, s, _, channel, group := membershipFixture(t)
	channels := &botChannelSet{known: true}
	logs := &recordedLogs{}
	r.UseBotChannels(channels, slog.New(logs))
	s.welcomes = []client.Welcome{{ID: 1, GroupID: group, Blob: []byte{1}, Tree: []byte{2}, TreeHash: make([]byte, 32)}}
	if _, err := r.reconcileOnce(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	channels.set(channel, true)
	n.applyErr = &helper.StatusError{Status: 3} // expired
	if _, err := r.reconcileOnce(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if _, reAdd := channels.reported(); !reAdd[channel] || !r.NeedsReAdd()[channel] {
		t.Fatalf("needs re-add %v, want the marked channel", reAdd)
	}
	if v := n.other[deferredWelcomeIndexKey]; v.Value == nil {
		t.Fatal("the deferred index is missing")
	} else if idx, _, err := r.loadDeferredIndex(context.Background()); err != nil || len(idx) != 0 {
		t.Fatalf("the stale Welcome is still kept: %v %v", idx, err)
	}
	// Again: no Welcome left to try, still reported, logged once.
	if _, err := r.reconcileOnce(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if n.applyCalls != 1 {
		t.Fatalf("apply calls %d, want 1 (the stale Welcome tried once)", n.applyCalls)
	}
	if got := len(logs.with("bot channel group needs the Bot re-added: no Welcome to join it from")); got != 1 {
		t.Fatalf("needs re-add logged %d times, want once", got)
	}
}

// TestAKeptWelcomeIsReplacedByANewerOne: per group only the newest Welcome is kept.
func TestAKeptWelcomeIsReplacedByANewerOne(t *testing.T) {
	r, _, s, _, _, group := membershipFixture(t)
	r.UseBotChannels(&botChannelSet{known: true}, nil)
	s.welcomes = []client.Welcome{
		{ID: 1, GroupID: group, Blob: []byte{1}, Tree: []byte{2}, TreeHash: make([]byte, 32)},
		{ID: 2, GroupID: group, Blob: []byte{3}, Tree: []byte{4}, TreeHash: make([]byte, 32)},
	}
	if _, err := r.reconcileOnce(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	w, _, _, ok, err := r.loadDeferredWelcome(context.Background(), group)
	if err != nil || !ok || w.ID != 2 || string(w.Blob) != string([]byte{3}) {
		t.Fatalf("kept %+v ok %v err %v; want Welcome 2", w, ok, err)
	}
}

// TestBeforeTheSnapshotNoTextGroupIsSyncedOrJoined (fix round 2, N-2): after a start, until the feed has applied a
// snapshot, the Bot does not know its Bot channels and fails closed. A held text group (one the Bot left) is not
// synced, and a Welcome to an unmarked channel is kept, not applied. Nothing is left or migrated yet. Once the snapshot
// says the second channel is a Bot channel, its kept Welcome joins the Bot, and the left group stays unsynced.
func TestBeforeTheSnapshotNoTextGroupIsSyncedOrJoined(t *testing.T) {
	r, n, s, community, channel, group := membershipFixture(t)
	other, otherGroup := membershipID(30), membershipID(31)
	s.channels = append(s.channels, client.Channel{ID: other, CommunityID: community, Kind: 0, TextGroupID: &otherGroup})
	channels := &botChannelSet{} // linked, not known
	r.UseBotChannels(channels, nil)
	held := native.GroupInfo{ID: group, Kind: 0, Community: &community, Target: channel, State: 2, NextSequence: 1}
	n.groups, n.inspected = []native.GroupInfo{held}, held
	s.welcomes = []client.Welcome{{ID: 1, GroupID: otherGroup, Blob: []byte{1}, Tree: []byte{2}, TreeHash: make([]byte, 32)}}
	res, err := r.reconcileOnce(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if res.GroupsSynced != 0 || s.groupPageCalls != 0 || n.applyCalls != 0 {
		t.Fatalf("before the snapshot: result %+v, page reads %d, applies %d; want nothing synced or applied", res, s.groupPageCalls, n.applyCalls)
	}
	if _, _, _, kept, err := r.loadDeferredWelcome(context.Background(), otherGroup); err != nil || !kept {
		t.Fatalf("the Welcome was not kept: %v %v", kept, err)
	}
	if v := n.other[botChannelMigrationKey]; v.Revision != 0 {
		t.Fatal("the first-start migration ran before the Bot channels were known")
	}
	// The snapshot: the second channel is a Bot channel.
	channels.set(other, true)
	n.outcomes = []native.GroupWelcomeOutcome{{ID: 1, Group: otherGroup, Outcome: 0}}
	n.inspected = native.GroupInfo{ID: otherGroup, Kind: 0, Community: &community, Target: other, State: 2, NextSequence: 1}
	if res, err = r.reconcileOnce(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if n.applyCalls != 1 || res.Applied != 1 || s.groupPageCalls != 0 {
		t.Fatalf("after the snapshot: apply %d, result %+v, page reads %d; want the kept Welcome applied and the left group unsynced", n.applyCalls, res, s.groupPageCalls)
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
	r.UseBotChannels(channels, slog.New(logs))
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
	again.UseBotChannels(channels, slog.New(logs))
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
