package catalog

import (
	"bytes"
	"context"
	"errors"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/bot/protocol"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/id"
)

type published struct {
	kind  string // "catalog" or "key"
	bytes []byte
}

type fakeSession struct {
	mu   sync.Mutex
	puts []published
	fail error
}

func (s *fakeSession) PublishCatalog(_ context.Context, _ id.ID, commands []byte) (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		return 0, s.fail
	}
	s.puts = append(s.puts, published{"catalog", append([]byte(nil), commands...)})
	return uint64(len(s.puts)), nil
}

func (s *fakeSession) PublishCommandKey(_ context.Context, _ id.ID, publication []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		return s.fail
	}
	s.puts = append(s.puts, published{"key", append([]byte(nil), publication...)})
	return nil
}

func (s *fakeSession) all() []published {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]published(nil), s.puts...)
}

type fakeKeys struct {
	mu    sync.Mutex
	key   []byte
	calls int
}

func (k *fakeKeys) CommandKey(_ context.Context, _ id.ID, _ time.Time) ([]byte, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.calls++
	return append([]byte(nil), k.key...), nil
}

func (k *fakeKeys) set(b []byte) { k.mu.Lock(); k.key = b; k.mu.Unlock() }

var commandName = regexp.MustCompile(`^[a-z0-9][a-z0-9 -]{0,31}$`)
var argName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// decoded checks one built catalog against dillad's grammar (Task 9) and the client's name rule (Task 20).
func decoded(t *testing.T, b []byte) map[string][]any {
	t.Helper()
	var rows [][]any
	if err := cborx.Unmarshal(b, &rows); err != nil {
		t.Fatalf("catalog is not CBOR: %v", err)
	}
	out := map[string][]any{}
	for _, row := range rows {
		if len(row) != 7 {
			t.Fatalf("row %v has %d fields", row, len(row))
		}
		name, _ := row[0].(string)
		if !commandName.MatchString(name) || out[name] != nil {
			t.Fatalf("bad or repeated name %q", name)
		}
		if desc, ok := row[1].(string); !ok || len(desc) > 512 {
			t.Fatalf("%s description %v", name, row[1])
		}
		args, _ := row[3].([]any)
		if len(args) > 16 {
			t.Fatalf("%s has %d args", name, len(args))
		}
		seen := map[string]bool{}
		for _, a := range args {
			f, _ := a.([]any)
			argN, _ := f[0].(string)
			code, _ := f[1].(uint64)
			req, okReq := f[2].(uint64)
			if len(f) != 3 || !argName.MatchString(argN) || seen[argN] || code > 5 || !okReq || req > 1 {
				t.Fatalf("%s arg %v", name, f)
			}
			seen[argN] = true
		}
		if bit, _ := row[4].(uint64); bit >= 1<<26 {
			t.Fatalf("%s permission %x", name, bit)
		}
		if needs, _ := row[5].(uint64); needs > 1 {
			t.Fatalf("%s needs_bot_channel %d", name, needs)
		}
		if kind, ok := row[6].(uint64); !ok || kind > 1 {
			t.Fatalf("%s handler_kind %v", name, row[6])
		}
		out[name] = row
	}
	return out
}

func TestBuildHoldsOnlyTheEnabledFeatures(t *testing.T) {
	none := decoded(t, Build(nil))
	if len(none) != 0 {
		t.Fatalf("no feature, commands %v", none)
	}
	reminders := decoded(t, Build([]Feature{FeatureReminders}))
	if reminders["remind"] == nil || reminders["play"] != nil {
		t.Fatalf("reminders catalog = %v", reminders)
	}
	if op := reminders["remind"][2]; op != uint64(protocol.OpReminderCreate) {
		t.Fatalf("remind operation %v", op)
	}
	both := decoded(t, Build([]Feature{FeatureMusic, FeatureReminders}))
	if both["play"] == nil || both["remind"] == nil {
		t.Fatalf("music and reminders catalog = %v", both)
	}
	// Deterministic: the same features give the same bytes, in any order.
	if !bytes.Equal(Build([]Feature{FeatureReminders, FeatureMusic}), Build([]Feature{FeatureMusic, FeatureReminders})) {
		t.Fatal("the catalog bytes depend on the feature order")
	}
}

func TestEveryCatalogCommandIsValidAndTheSealedReadsAreListed(t *testing.T) {
	all := decoded(t, Build(AllFeatures()))
	for _, read := range []struct {
		name string
		op   protocol.Operation
	}{{"poll get", protocol.OpReadPoll}, {"music state", protocol.OpReadMusic}, {"reminders due", protocol.OpReadRemindersDue}} {
		row := all[read.name]
		if row == nil || row[2] != uint64(read.op) || row[6] != HandlerRead {
			t.Fatalf("sealed read %s = %v", read.name, row)
		}
	}
	// One permission per operation, so the intake can authorise by operation alone.
	perms := map[uint64]uint64{}
	for name, row := range all {
		op, bit := row[2].(uint64), row[4].(uint64)
		if prev, ok := perms[op]; ok && prev != bit {
			t.Fatalf("%s: operation %d has two permission bits", name, op)
		}
		perms[op] = bit
	}
	ops := Operations(AllFeatures())
	if len(ops) != len(perms) {
		t.Fatalf("Operations = %v, catalog = %v", ops, perms)
	}
	for op, bit := range perms {
		read := all[nameOf(all, op)][6] == HandlerRead && !AppMutation(protocol.Operation(op))
		if got, ok := ops[op]; !ok || got.Permission != bit || got.Read != read {
			t.Fatalf("Operations[%d] = %+v %v, want permission %d", op, got, ok, bit)
		}
	}
	if _, ok := Operations([]Feature{FeatureReminders})[uint64(protocol.OpMusicEnqueue)]; ok {
		t.Fatal("a disabled feature's operation is offered")
	}
}

func TestFeaturesOfCapabilities(t *testing.T) {
	got := FeaturesOf(1<<0 | 1<<9 | 1<<1)
	if len(got) != 2 || got[0] != FeatureMusic || got[1] != FeatureReminders {
		t.Fatalf("features = %v", got)
	}
}

func TestCatalogPublishedOnStartAndFeatureChange(t *testing.T) {
	community := id.New()
	session := &fakeSession{}
	keys := &fakeKeys{key: []byte("key-1")}
	features := []Feature{FeatureReminders}
	var mu sync.Mutex
	clk := clock.NewFake(time.Unix(1_800_000_000, 0))
	p := NewPublisher(keys, session, clk, func() []Feature { mu.Lock(); defer mu.Unlock(); return features })
	ctx := context.Background()
	if err := p.Publish(ctx, community); err != nil {
		t.Fatal(err)
	}
	got := session.all()
	if len(got) != 2 || got[0].kind != "key" || string(got[0].bytes) != "key-1" || got[1].kind != "catalog" || !bytes.Equal(got[1].bytes, Build(features)) {
		t.Fatalf("start published %v", got)
	}
	// Nothing changed: nothing is sent (the helper's key op is local).
	if err := p.Publish(ctx, community); err != nil {
		t.Fatal(err)
	}
	if n := len(session.all()); n != 2 {
		t.Fatalf("an unchanged publish sent %d requests", n-2)
	}
	// A feature change: the catalog only.
	mu.Lock()
	features = []Feature{FeatureReminders, FeatureMusic}
	mu.Unlock()
	if err := p.Publish(ctx, community); err != nil {
		t.Fatal(err)
	}
	got = session.all()
	if len(got) != 3 || got[2].kind != "catalog" || !bytes.Equal(got[2].bytes, Build([]Feature{FeatureMusic, FeatureReminders})) {
		t.Fatalf("feature change published %v", got[2:])
	}
	// A failed publish is retried by the next one: nothing is remembered as sent.
	session.fail = errors.New("offline")
	keys.set([]byte("key-2"))
	if err := p.Publish(ctx, community); err == nil {
		t.Fatal("a failed publish reported success")
	}
	session.fail = nil
	if err := p.Publish(ctx, community); err != nil {
		t.Fatal(err)
	}
	got = session.all()
	if len(got) != 4 || got[3].kind != "key" || string(got[3].bytes) != "key-2" {
		t.Fatalf("after the failure published %v", got[3:])
	}
}

// TestCommandKeyRotatesAndRepublishesWithoutRestart (Task 8, I-2): the publisher asks the helper for the
// key at start and every hour, and republishes as soon as the helper rotated it, with no restart.
func TestCommandKeyRotatesAndRepublishesWithoutRestart(t *testing.T) {
	community := id.New()
	session := &fakeSession{}
	keys := &fakeKeys{key: []byte("week-1")}
	clk := clock.NewFake(time.Unix(1_800_000_000, 0))
	p := NewPublisher(keys, session, clk, func() []Feature { return []Feature{FeatureReminders} })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { p.Run(ctx, community); close(done) }()
	waitFor(t, func() bool { return len(session.all()) == 2 })
	// Within the hour nothing is asked for; at the hour the helper is asked and the key is unchanged.
	keys.set([]byte("week-2"))
	waitFor(t, func() bool { keys.mu.Lock(); defer keys.mu.Unlock(); return keys.calls == 1 })
	// The hour passes (again each poll, in case the timer was armed after an advance).
	waitFor(t, func() bool {
		clk.Advance(time.Hour)
		got := session.all()
		return len(got) == 3 && string(got[2].bytes) == "week-2"
	})
	if got := session.all(); got[2].kind != "key" {
		t.Fatalf("rotation published %v", got[2:])
	}
	// A feature change wakes it at once.
	p.Changed()
	waitFor(t, func() bool { keys.mu.Lock(); defer keys.mu.Unlock(); return keys.calls >= 3 })
	cancel()
	<-done
}

func TestKeyStaleChecksTheKeyAtOnce(t *testing.T) {
	community := id.New()
	session := &fakeSession{}
	keys := &fakeKeys{key: []byte("k1")}
	clk := clock.NewFake(time.Unix(1_800_000_000, 0))
	p := NewPublisher(keys, session, clk, func() []Feature { return nil })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { p.Run(ctx, community); close(done) }()
	waitFor(t, func() bool { return len(session.all()) == 2 })
	keys.set([]byte("k2"))
	p.Changed()
	waitFor(t, func() bool { got := session.all(); return len(got) == 3 && string(got[2].bytes) == "k2" })
	cancel()
	<-done
}

func nameOf(rows map[string][]any, op uint64) string {
	for name, row := range rows {
		if row[2] == op {
			return name
		}
	}
	return ""
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met")
		}
		time.Sleep(time.Millisecond)
	}
}

// Every command declares its scope; one operation has one scope (the intake authorises by operation).
func TestEveryCommandDeclaresItsScope(t *testing.T) {
	scopes := map[protocol.Operation]Scope{}
	for _, c := range Commands(AllFeatures()) {
		if c.Scope != ScopeCommunity && c.Scope != ScopeChannel {
			t.Fatalf("%s declares no scope", c.Name)
		}
		if prev, ok := scopes[c.Operation]; ok && prev != c.Scope {
			t.Fatalf("%s: operation %d has two scopes", c.Name, c.Operation)
		}
		scopes[c.Operation] = c.Scope
	}
	rules := Operations(AllFeatures())
	if r := rules[uint64(protocol.OpPollCreate)]; !r.ChannelScoped || len(r.ChannelArgs) != 1 || r.ChannelArgs[0] != 4 {
		t.Fatalf("poll create rule = %+v", r)
	}
	if r := rules[uint64(protocol.OpReminderCreate)]; r.ChannelScoped {
		t.Fatalf("remind rule = %+v", r)
	}
}

// Fix round 2, N-3: a command sealed to an unknown key forces the key and catalog out again at most once a minute per
// community; in between, Stale only asks for an ordinary publish, which sends nothing when the bytes are unchanged.
func TestStaleKeyRepublishIsThrottled(t *testing.T) {
	community := id.New()
	session := &fakeSession{}
	keys := &fakeKeys{key: []byte("k1")}
	clk := clock.NewFake(time.Unix(1_800_000_000, 0))
	p := NewPublisher(keys, session, clk, func() []Feature { return []Feature{FeatureReminders} })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { p.Run(ctx, community); close(done) }()
	waitFor(t, func() bool { return len(session.all()) == 2 })
	calls := func() int { keys.mu.Lock(); defer keys.mu.Unlock(); return keys.calls }
	p.Stale()
	waitFor(t, func() bool { return len(session.all()) == 4 })
	// A burst of stale-key commands within the minute: each asks for a publish, none forces one.
	for range 5 {
		before := calls()
		p.Stale()
		waitFor(t, func() bool { return calls() > before })
	}
	if n := len(session.all()); n != 4 {
		t.Fatalf("puts after the burst = %d, want 4", n)
	}
	clk.Advance(time.Minute)
	p.Stale()
	waitFor(t, func() bool { return len(session.all()) == 6 })
	cancel()
	<-done
}

// Task 21b: the app's surfaces act through sealed app operations (100 + the v1 operation). Each is a hidden entry
// (handler_kind 1, never shown in the picker); a mutation among them is not a read for the intake, so a timed-out member
// cannot vote, dismiss or steer the music through one; the player's operations act in the voice channel.
func TestAppOperationsAreHiddenAndOnlyTheirReadsAreReads(t *testing.T) {
	rules := Operations(AllFeatures())
	want := map[protocol.Operation]struct {
		v1                   protocol.Operation
		mutation, channelled bool
	}{
		protocol.OpAppMusicSearch: {protocol.OpMusicSearch, false, true}, protocol.OpAppMusicEnqueue: {protocol.OpMusicEnqueue, true, true},
		protocol.OpAppMusicPause: {protocol.OpMusicPause, true, true}, protocol.OpAppMusicResume: {protocol.OpMusicResume, true, true},
		protocol.OpAppMusicSkip: {protocol.OpMusicSkip, true, true}, protocol.OpAppMusicStop: {protocol.OpMusicStop, true, true},
		protocol.OpAppMusicSeek: {protocol.OpMusicSeek, true, true}, protocol.OpAppMusicVolume: {protocol.OpMusicVolume, true, true},
		protocol.OpAppMusicShuffle: {protocol.OpMusicShuffle, true, true}, protocol.OpAppMusicRepeat: {protocol.OpMusicRepeat, true, true},
		protocol.OpAppMusicRemove: {protocol.OpMusicRemove, true, true}, protocol.OpAppMusicMove: {protocol.OpMusicMove, true, true},
		protocol.OpAppMusicClear: {protocol.OpMusicClear, true, true}, protocol.OpAppMusicAnnounce: {protocol.OpMusicAnnounce, true, true},
		protocol.OpAppMusicProvidersStatus: {protocol.OpMusicProvidersStatus, false, false},
		protocol.OpAppRolePanelPublish:     {protocol.OpRolePanelPublish, true, false},
		protocol.OpAppAnnouncementsList:    {protocol.OpAnnouncementsList, false, false},
		protocol.OpAppAnnouncementsPut:     {protocol.OpAnnouncementsPut, true, false},
		protocol.OpAppAnnouncementsDelete:  {protocol.OpAnnouncementsDelete, true, false},
		protocol.OpAppPollCreate:           {protocol.OpPollCreate, true, true}, protocol.OpAppPollList: {protocol.OpPollList, false, false},
		protocol.OpAppPollVote: {protocol.OpPollVote, true, false}, protocol.OpAppPollClose: {protocol.OpPollClose, true, false},
		protocol.OpAppPollVoters: {protocol.OpPollVoters, false, false}, protocol.OpAppReminderCreate: {protocol.OpReminderCreate, true, false},
		protocol.OpAppReminderList: {protocol.OpReminderList, false, false}, protocol.OpAppReminderCancel: {protocol.OpReminderCancel, true, false},
		protocol.OpAppReminderDismiss: {protocol.OpReminderDismiss, true, false},
	}
	for op, w := range want {
		if op != w.v1+AppOperationBase {
			t.Fatalf("app operation %d does not run v1 operation %d", op, w.v1)
		}
		var c *Command
		for _, cmd := range Commands(AllFeatures()) {
			if cmd.Operation == op {
				c = &cmd
			}
		}
		if c == nil || c.HandlerKind != HandlerRead {
			t.Fatalf("app operation %d is not a hidden catalog entry: %+v", op, c)
		}
		r := rules[uint64(op)]
		if r.Read == w.mutation || AppMutation(op) != w.mutation || r.ChannelScoped != w.channelled {
			t.Fatalf("app operation %d rule %+v, want read %v channel-scoped %v", op, r, !w.mutation, w.channelled)
		}
	}
	if r := rules[uint64(protocol.OpAppAnnouncementsPut)]; r.Permission != permManageCommunity {
		t.Fatalf("announcement put rule = %+v, want manage_community", r)
	}
	if r := rules[uint64(protocol.OpAppRolePanelPublish)]; r.Permission != permManageRoles {
		t.Fatalf("role panel rule = %+v, want manage_roles", r)
	}
	if r := rules[uint64(protocol.OpAppPollCreate)]; r.Permission != permSendMessages {
		t.Fatalf("app poll create rule = %+v, want send_messages", r)
	}
	if _, ok := Operations([]Feature{FeatureMusic})[uint64(protocol.OpAppAnnouncementsList)]; ok {
		t.Fatal("announcements are offered without their feature")
	}
	if got := FeaturesOf(1<<2 | 1<<3); len(got) != 2 || got[0] != FeatureRoles || got[1] != FeatureAnnouncements {
		t.Fatalf("features of roles and announcements = %v", got)
	}
}
