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

// TestBotJoinsOnMarkAndLeavesOnUnmark (Task 23, R1 d): with the Bot channels known, a Welcome to an unmarked channel's
// group is refused: acknowledged, not applied, not kept. Marking the channel does not join the Bot from it; the mark's
// fresh Add does (its Welcome is applied and the next pass syncs the group). Removing the mark leaves the group again:
// its local MLS state is forgotten, it is not synced, and it is reported as left.
func TestBotJoinsOnMarkAndLeavesOnUnmark(t *testing.T) {
	r, n, s, community, channel, group := membershipFixture(t)
	channels := &botChannelSet{known: true}
	logs := &recordedLogs{}
	r.UseBotChannels(channels, slog.New(logs))
	s.welcomes = []client.Welcome{{ID: 1, GroupID: group, Epoch: 3, CommitSequence: 7, Blob: []byte{1}, Tree: []byte{2}, TreeHash: make([]byte, 32)}}

	if _, err := r.reconcileOnce(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if n.applyCalls != 0 || len(s.acked) != 1 {
		t.Fatalf("unmarked: apply %d, acked %v; want no apply and the Welcome acknowledged", n.applyCalls, s.acked)
	}
	if got := len(logs.with("bot welcome to a non-Bot channel group refused")); got != 1 {
		t.Fatalf("refusal logged %d times", got)
	}

	// Marked: nothing is joined from the refused Welcome; the channel needs the Bot added.
	channels.set(channel, true)
	if _, err := r.reconcileOnce(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if n.applyCalls != 0 || !r.NeedsReAdd()[channel] {
		t.Fatalf("marked: apply %d, needs re-add %v; want nothing applied and the channel reported", n.applyCalls, r.NeedsReAdd())
	}

	// The mark's Add: its Welcome joins the Bot.
	s.welcomes = append(s.welcomes, client.Welcome{ID: 2, GroupID: group, Epoch: 5, CommitSequence: 9, Blob: []byte{1}, Tree: []byte{2}, TreeHash: make([]byte, 32)})
	n.outcomes = []native.GroupWelcomeOutcome{{ID: 2, Group: group, Outcome: 0}}
	info := native.GroupInfo{ID: group, Kind: 0, Community: &community, Target: channel, State: 2, NextSequence: 1}
	n.inspected = info // revalidation after the join
	res, err := r.reconcileOnce(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if n.applyCalls != 1 || res.Applied != 1 || len(n.welcomes) != 1 || n.welcomes[0].ID != 2 {
		t.Fatalf("marked: apply %d, result %+v, applied %+v; want the new Welcome applied", n.applyCalls, res, n.welcomes)
	}
	if len(n.expected) != 1 || n.expected[0].Channel != channel || n.expected[0].Kind != 0 {
		t.Fatalf("expected binding %+v", n.expected)
	}
	// The next pass syncs the joined group.
	n.groups = []native.GroupInfo{info}
	res, err = r.reconcileOnce(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if n.applyCalls != 1 || res.GroupsSynced != 1 {
		t.Fatalf("after the join: apply %d, result %+v; want the group synced and nothing applied again", n.applyCalls, res)
	}

	// Unmarked again: the group is forgotten, not synced, and reported left.
	channels.set(channel, false)
	pages := s.groupPageCalls
	res, err = r.reconcileOnce(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if n.applyCalls != 1 || res.GroupsSynced != 0 || s.groupPageCalls != pages {
		t.Fatalf("unmarked again: apply %d, result %+v, page reads %d; want nothing applied or synced", n.applyCalls, res, s.groupPageCalls-pages)
	}
	if len(n.forgotten) != 1 || n.forgotten[0] != group {
		t.Fatalf("forgotten %v, want the unmarked channel's group", n.forgotten)
	}
	if left, _ := channels.reported(); !left[group] {
		t.Fatalf("left groups %v, want the unmarked channel's group", left)
	}
}

// TestAKeptWelcomeIsDroppedNeverApplied (Task 23): a Welcome an earlier version kept for a non-Bot channel is dropped on
// the first pass with the Bot channels known, even when its channel is marked by then; the channel is reported as
// needing the Bot added, logged once.
func TestAKeptWelcomeIsDroppedNeverApplied(t *testing.T) {
	r, n, s, _, channel, group := membershipFixture(t)
	channels := &botChannelSet{known: true}
	channels.set(channel, true)
	logs := &recordedLogs{}
	r.UseBotChannels(channels, slog.New(logs))
	index, err := encodeDeferredIndex([]id.ID{group})
	if err != nil {
		t.Fatal(err)
	}
	n.other = map[string]native.StateValue{
		deferredWelcomeIndexKey:           {Revision: 1, Value: index},
		string(deferredWelcomeKey(group)): {Revision: 1, Value: []byte{0x80}},
	}
	for range 2 {
		if _, err := r.reconcileOnce(context.Background(), s); err != nil {
			t.Fatal(err)
		}
	}
	if n.applyCalls != 0 {
		t.Fatalf("apply calls %d, want none", n.applyCalls)
	}
	if idx, _, err := r.loadDeferredIndex(context.Background()); err != nil || len(idx) != 0 {
		t.Fatalf("the kept Welcome is still kept: %v %v", idx, err)
	}
	if v := n.other[string(deferredWelcomeKey(group))]; !v.Tombstone {
		t.Fatalf("the kept Welcome's value %+v, want deleted", v)
	}
	if _, reAdd := channels.reported(); !reAdd[channel] {
		t.Fatalf("needs re-add %v, want the marked channel", reAdd)
	}
	if got := len(logs.with("bot channel group needs the Bot re-added: no Welcome to join it from")); got != 1 {
		t.Fatalf("needs re-add logged %d times, want once", got)
	}
}

// TestBeforeTheSnapshotNoTextGroupIsSyncedOrJoined (fix round 2, N-2; Task 23): after a start, until the feed has
// applied a snapshot, the Bot does not know its Bot channels and fails closed. A held text group is not synced or
// forgotten, and no Welcome is read or acknowledged (it waits on the server). Nothing is left or migrated yet. Once the
// snapshot says the second channel is a Bot channel, its Welcome joins the Bot, and the other channel's group is
// forgotten.
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
	if res.GroupsSynced != 0 || s.groupPageCalls != 0 || n.applyCalls != 0 || len(s.acked) != 0 || len(n.forgotten) != 0 {
		t.Fatalf("before the snapshot: result %+v, page reads %d, applies %d, acked %v, forgotten %v; want nothing done",
			res, s.groupPageCalls, n.applyCalls, s.acked, n.forgotten)
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
		t.Fatalf("after the snapshot: apply %d, result %+v, page reads %d; want the Welcome applied and the left group unsynced", n.applyCalls, res, s.groupPageCalls)
	}
	if len(n.forgotten) != 1 || n.forgotten[0] != group {
		t.Fatalf("forgotten %v, want the non-Bot channel's group", n.forgotten)
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

// Task 23 (R1 d): the Bot keeps no MLS state for a text group whose channel is not a Bot channel. A group it still holds
// there (a commit that carried it in across an unmark) is forgotten, logged once with its channel id, and a Welcome into
// such a group is refused: acknowledged, never applied and not kept, so a later mark does not join the Bot from it (the
// mark's fresh Add does).
func TestTheBotForgetsANonBotChannelGroupAndRefusesItsWelcome(t *testing.T) {
	r, n, s, community, channel, group := membershipFixture(t)
	channels := &botChannelSet{known: true}
	logs := &recordedLogs{}
	r.UseBotChannels(channels, slog.New(logs))
	n.groups = []native.GroupInfo{{ID: group, Kind: 0, Community: &community, Target: channel, State: 2, NextSequence: 1}}
	s.welcomes = []client.Welcome{{ID: 1, GroupID: group, Epoch: 3, CommitSequence: 7, Blob: []byte{1}, Tree: []byte{2}, TreeHash: make([]byte, 32)}}
	res, err := r.reconcileOnce(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if len(n.forgotten) != 1 || n.forgotten[0] != group {
		t.Fatalf("forgotten %v, want the non-Bot channel's group", n.forgotten)
	}
	if n.applyCalls != 0 || len(s.acked) != 1 || res.GroupsSynced != 0 || s.groupPageCalls != 0 {
		t.Fatalf("apply %d, acked %v, result %+v, page reads %d; want the Welcome refused and nothing synced", n.applyCalls, s.acked, res, s.groupPageCalls)
	}
	if idx, _, err := r.loadDeferredIndex(context.Background()); err != nil || len(idx) != 0 {
		t.Fatalf("the refused Welcome was kept: %v %v", idx, err)
	}
	if got := logs.with("bot forgot non-Bot channel group"); len(got) != 1 || got[0]["channel_id"] != channel.String() {
		t.Fatalf("forget logged %v", got)
	}
	// Marked later: nothing is applied from the refused Welcome; the channel needs the Bot added (the mark's Add).
	channels.set(channel, true)
	if _, err = r.reconcileOnce(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if n.applyCalls != 0 || len(n.forgotten) != 1 {
		t.Fatalf("after the mark: apply %d, forgotten %v; want nothing applied or forgotten again", n.applyCalls, n.forgotten)
	}
	if !r.NeedsReAdd()[channel] {
		t.Fatal("the marked channel the Bot is not in is not reported")
	}
}

// Task 23 (R1 d): a Bot channel's group whose source the server no longer serves the Bot (404: its leaf was removed
// while the Bot was offline) is forgotten, so the Bot keeps no keys for it.
func TestTheBotForgetsATextGroupItWasRemovedFrom(t *testing.T) {
	r, n, s, community, channel, group := membershipFixture(t)
	channels := &botChannelSet{known: true}
	channels.set(channel, true)
	r.UseBotChannels(channels, nil)
	held := native.GroupInfo{ID: group, Kind: 0, Community: &community, Target: channel, State: 2, NextSequence: 1}
	n.groups, n.inspected = []native.GroupInfo{held}, held
	gone := &client.APIError{Status: 404, Code: "E_NOT_FOUND", Outcome: client.OutcomeDefinitive}
	s.groupInfo, s.groupInfoErr, s.groupPageErr = client.GroupInfo{NextSequence: 1}, gone, gone
	if _, err := r.reconcileOnce(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if len(n.forgotten) != 1 || n.forgotten[0] != group {
		t.Fatalf("forgotten %v, want the group the Bot was removed from", n.forgotten)
	}
}
