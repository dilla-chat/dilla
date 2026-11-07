package api_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/api/sockettest"
	"github.com/dilla-chat/dilla/internal/auth"
	"github.com/dilla-chat/dilla/internal/bot/protocol"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/ds"
	"github.com/dilla-chat/dilla/internal/gateway"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/mlswasi"
	"github.com/dilla-chat/dilla/internal/store"
)

// Bot v2 Task 12: the Bot's event feed (op 65) replaces its polling of the structural-event routes.
// dillad pushes device lists, members, roles, channel groups and Bot channels on the Bot's connection
// only, after each change commits, and a cold-start snapshot on each Bot identify. What the Bot learns
// is bounded: the users of its own community, its community's members and roles, and the groups of
// its Bot channels and of the calls it is in.

// feedGateway records what the feed delivers to each Bot device.
type feedGateway struct {
	mu      sync.Mutex
	offline bool
	frames  map[id.ID][]gateway.Frame
}

func (g *feedGateway) DeliverBot(device id.ID, f gateway.Frame) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.offline {
		return false
	}
	g.frames[device] = append(g.frames[device], f)
	return true
}

func (g *feedGateway) take(device id.ID) []gateway.Frame {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := g.frames[device]
	delete(g.frames, device)
	return out
}

// feedStream is a gateway.BotStream double: it records the snapshot and whether it was finished.
type feedStream struct {
	installation, community, device id.ID
	mu                              sync.Mutex
	sent                            []gateway.Frame
	finished, aborted               bool
}

func (s *feedStream) Installation() id.ID { return s.installation }
func (s *feedStream) Community() id.ID    { return s.community }
func (s *feedStream) Device() id.ID       { return s.device }
func (s *feedStream) Send(_ context.Context, f gateway.Frame) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, f)
	return true
}
func (s *feedStream) Finish(f gateway.Frame) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, f)
	s.finished = true
	return true
}
func (s *feedStream) Abort(string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.aborted = true
}

// feedEvent is one decoded op 65 frame.
type feedEvent struct {
	kind uint64
	body []cbor.RawMessage
}

func decodeFeed(t *testing.T, frames []gateway.Frame) []feedEvent {
	t.Helper()
	out := make([]feedEvent, 0, len(frames))
	for _, f := range frames {
		if f.Op != gateway.OpBotEvent {
			t.Fatalf("the feed sent op %d, want bot_event", f.Op)
		}
		var outer []cbor.RawMessage
		if err := cborx.Unmarshal(f.Payload, &outer); err != nil || len(outer) != 2 {
			t.Fatalf("bot_event payload: %v", err)
		}
		var kind uint64
		var body []cbor.RawMessage
		if err := cborx.Unmarshal(outer[0], &kind); err != nil {
			t.Fatal(err)
		}
		if err := cborx.Unmarshal(outer[1], &body); err != nil {
			t.Fatal(err)
		}
		out = append(out, feedEvent{kind: kind, body: body})
	}
	return out
}

func feedID(t *testing.T, raw cbor.RawMessage) id.ID {
	t.Helper()
	var v id.ID
	if err := cborx.Unmarshal(raw, &v); err != nil {
		t.Fatalf("id: %v", err)
	}
	return v
}

func feedUint(t *testing.T, raw cbor.RawMessage) uint64 {
	t.Helper()
	var v uint64
	if err := cborx.Unmarshal(raw, &v); err != nil {
		t.Fatalf("uint: %v", err)
	}
	return v
}

type feedFixture struct {
	h          http.Handler
	deps       api.Deps
	feed       *api.BotFeed
	gw         *feedGateway
	skipped    *skipCounter
	community  id.ID
	member     listedUser // a member with a signed device list
	stranger   listedUser // shares no community with the Bot
	owner      store.UserRow
	everyone   store.RoleRow
	mods       store.RoleRow
	botUser    id.ID
	botDevice  id.ID
	install    id.ID
	text       id.ID // a Bot channel
	other      id.ID // an ordinary text channel
	otherScope id.ID // another community, without the Bot
}

type skipCounter struct {
	mu      sync.Mutex
	reasons []string
}

func (s *skipCounter) add(reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reasons = append(s.reasons, reason)
}

func (s *skipCounter) list() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.reasons)
}

func newFeedFixture(t *testing.T) *feedFixture {
	t.Helper()
	wasm, err := os.ReadFile(filepath.Join("..", "mlswasi", "testdata", "dilla_core_wasi.wasm"))
	if err != nil {
		t.Fatalf("read the wasi core: %v", err)
	}
	rt, err := mlswasi.New(context.Background(), wasm, mlswasi.Options{PoolSize: 1, CacheDir: apiWasmCacheDir(t)})
	if err != nil {
		t.Fatalf("mlswasi.New: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close(context.Background()) })
	f := &feedFixture{gw: &feedGateway{frames: map[id.ID][]gateway.Frame{}}, skipped: &skipCounter{}}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	f.h, f.deps = newTestAPIWired(t, func(d *api.Deps) {
		d.DeviceLists = ds.NewDeviceLists(d.Repo, rt)
		f.feed = api.NewBotFeed(ctx, api.BotFeedOptions{Repo: d.Repo, Gateway: f.gw, Log: d.Log, Skipped: f.skipped.add, Clock: d.Clock})
		d.BotFeed = f.feed
		d.BotChannelEvents = f.feed
	})
	repo := f.deps.Repo
	now := f.deps.Clock.Now().Unix()
	f.member = seedListedUser(t, f.deps, 0x5a, 1)
	f.stranger = seedListedUser(t, f.deps, 0x6b, 1)
	f.owner, _, _ = seedAPIDevice(t, f.deps)
	f.community, f.otherScope = id.New(), id.New()
	for _, c := range []id.ID{f.community, f.otherScope} {
		if err := repo.CreateCommunity(ctx, store.CommunityRow{ID: c, Owner: f.owner.ID, Name: "feed-" + c.String()[:8], PolicyJSON: []byte(`{}`), PolicyVersion: 1, Created: now}); err != nil {
			t.Fatal(err)
		}
	}
	f.everyone = store.RoleRow{ID: id.New(), CommunityID: f.community, Name: "@everyone", Position: 0, Created: now, Allow: uint64(api.PermViewChannel | api.PermSendMessages)}
	f.mods = store.RoleRow{ID: id.New(), CommunityID: f.community, Name: "mods", Position: 3, Created: now, Allow: uint64(api.PermKickMembers), Deny: uint64(api.PermSendMessages)}
	for _, r := range []store.RoleRow{f.everyone, f.mods} {
		if err := repo.PutRole(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	for _, u := range []id.ID{f.owner.ID, f.member.User.ID} {
		if err := repo.PutMember(ctx, store.MemberOfCommunityRow{CommunityID: f.community, UserID: u, EpisodeID: id.New(), Joined: now}); err != nil {
			t.Fatal(err)
		}
	}
	// The stranger shares only the other community, which has no Bot.
	if err := repo.PutMember(ctx, store.MemberOfCommunityRow{CommunityID: f.otherScope, UserID: f.stranger.User.ID, EpisodeID: id.New(), Joined: now}); err != nil {
		t.Fatal(err)
	}
	if err := repo.PutMemberRole(ctx, f.community, f.member.User.ID, f.mods.ID); err != nil {
		t.Fatal(err)
	}
	cid := f.community
	for _, ch := range []*id.ID{&f.text, &f.other} {
		*ch = id.New()
		if err := repo.CreateChannel(ctx, store.ChannelRow{ID: *ch, CommunityID: &cid, Kind: api.ChannelText, Mode: api.ModeE2EE, Name: "c" + ch.String()[:6], SettingsJSON: []byte(`{}`), HostPolicyVersion: 1, Created: now}); err != nil {
			t.Fatal(err)
		}
	}
	// The ready Bot.
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	f.botUser, f.botDevice, f.install = id.New(), id.New(), id.New()
	runtime := id.New()
	verified := now
	if err := repo.CreateUser(ctx, store.UserRow{ID: f.botUser, Username: "bot" + f.botUser.String()[:8], Display: "Bot", Kind: 1, UMKPub: make([]byte, 32), SSKPub: make([]byte, 32), SigUMKSSK: make([]byte, 64), Created: now}); err != nil {
		t.Fatal(err)
	}
	if err := repo.CreateDevice(ctx, store.DeviceRow{ID: f.botDevice, UserID: f.botUser, DSKPub: pub, CredentialBlob: []byte{1}, VerifiedAt: &verified, Created: now, LastSeen: now}); err != nil {
		t.Fatal(err)
	}
	if err := repo.PutMember(ctx, store.MemberOfCommunityRow{CommunityID: f.community, UserID: f.botUser, EpisodeID: id.New(), Joined: now}); err != nil {
		t.Fatal(err)
	}
	if err := repo.CreateBotRuntime(ctx, store.BotRuntimeRow{ID: runtime, TokenHash: bytes.Repeat([]byte{0x31}, 32), Created: now, Rotated: now}); err != nil {
		t.Fatal(err)
	}
	if err := repo.CreateBotInstallation(ctx, store.BotInstallationRow{ID: f.install, CommunityID: f.community, RuntimeID: &runtime, UserID: &f.botUser, DeviceID: &f.botDevice,
		State: store.BotInstallationReady, Revision: 1, ApprovedBy: f.owner.ID, ApprovedAt: now, CredentialVerifiedAt: &verified, Created: now, Updated: now}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.MarkBotChannel(ctx, store.BotChannelRow{ChannelID: f.text, CommunityID: f.community, MarkedAt: now, MarkedBy: f.owner.ID}); err != nil {
		t.Fatal(err)
	}
	return f
}

// publish PUTs the next version of a listed user's device list through the route and returns its body.
func (f *feedFixture) publish(t *testing.T, u listedUser, version uint64, prev []byte) ([]any, []byte) {
	t.Helper()
	body, blob := signedListPut(t, u.SSK, u.User.ID, version, prev, []apiListEntry{entryOf(u, 0, nil)})
	path := "/v1/users/" + u.User.ID.String() + "/device-list"
	if rec := cborCall(t, f.h, http.MethodPut, path, u.Tokens[0], body); rec.Code != http.StatusNoContent {
		t.Fatalf("publish v%d = %d %x, want 204", version, rec.Code, rec.Body.Bytes())
	}
	return body, blob
}

func (f *feedFixture) drain(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := f.feed.Drain(ctx); err != nil {
		t.Fatalf("Drain: %v", err)
	}
}

// identify runs the Bot's cold start and returns the snapshot it streamed.
func (f *feedFixture) identify(t *testing.T) *feedStream {
	t.Helper()
	s := &feedStream{installation: f.install, community: f.community, device: f.botDevice}
	f.feed.BotIdentified(s)
	f.drain(t)
	return s
}

func pushOf(t *testing.T, ev feedEvent) (id.ID, protocol.DeviceListPush) {
	t.Helper()
	if ev.kind != gateway.BotEventDeviceList || len(ev.body) != 2 {
		t.Fatalf("event kind %d with %d elements, want a device_list", ev.kind, len(ev.body))
	}
	var raw []byte
	if err := cborx.Unmarshal(ev.body[1], &raw); err != nil {
		t.Fatalf("device_list push: %v", err)
	}
	push, err := protocol.DecodeDeviceListPush(raw)
	if err != nil {
		t.Fatalf("DecodeDeviceListPush: %v", err)
	}
	return feedID(t, ev.body[0]), push
}

func TestDeviceListWriteEmitsOneFeedEventWithTheSignedBody(t *testing.T) {
	f := newFeedFixture(t)
	_, blob1 := f.publish(t, f.member, 1, nil)
	f.identify(t)
	f.gw.take(f.botDevice)

	body2, _ := f.publish(t, f.member, 2, blob1)
	f.drain(t)
	events := decodeFeed(t, f.gw.take(f.botDevice))
	if len(events) != 1 {
		t.Fatalf("the device-list write sent %d feed events, want 1", len(events))
	}
	user, push := pushOf(t, events[0])
	if user != f.member.User.ID {
		t.Fatalf("the push names user %s, want %s", user, f.member.User.ID)
	}
	if !bytes.Equal(push.UMK[:], f.member.User.UMKPub) || !bytes.Equal(push.SSK[:], f.member.User.SSKPub) || !bytes.Equal(push.SigUMKSSK[:], f.member.User.SigUMKSSK) {
		t.Fatal("the push does not carry the user's stored UMK, SSK and certificate")
	}
	// The whole history from version 1, in order, the head being the PUT body exactly.
	if len(push.History) != 2 || push.History[0].Version != 1 || !bytes.Equal(push.History[0].Blob, blob1) {
		t.Fatalf("history %+v, want versions 1 and 2", push.History)
	}
	head := push.History[1]
	if head.Version != body2[0].(uint64) || !bytes.Equal(head.Blob, body2[1].([]byte)) ||
		!bytes.Equal(head.SSKSignature[:], body2[2].([]byte)) || !bytes.Equal(head.PrevHash[:], body2[3].([]byte)) {
		t.Fatal("the pushed head is not the signed body the user PUT")
	}
}

// A device-list write that does not commit (a version conflict) sends nothing.
func TestFeedEmitsNothingOnRollback(t *testing.T) {
	f := newFeedFixture(t)
	body, _ := f.publish(t, f.member, 1, nil)
	f.identify(t)
	f.gw.take(f.botDevice)
	path := "/v1/users/" + f.member.User.ID.String() + "/device-list"
	if rec := cborCall(t, f.h, http.MethodPut, path, f.member.Tokens[0], body); rec.Code != http.StatusConflict {
		t.Fatalf("replayed v1 = %d, want 409", rec.Code)
	}
	f.drain(t)
	if got := f.gw.take(f.botDevice); len(got) != 0 {
		t.Fatalf("a refused write sent %d feed events", len(got))
	}
}

func TestColdStartSnapshotIsBracketed(t *testing.T) {
	f := newFeedFixture(t)
	f.publish(t, f.member, 1, nil)
	f.publish(t, f.stranger, 1, nil)
	s := f.identify(t)
	if !s.finished || s.aborted {
		t.Fatalf("snapshot finished %v aborted %v, want a finished snapshot", s.finished, s.aborted)
	}
	events := decodeFeed(t, s.sent)
	if len(events) < 2 || events[0].kind != gateway.BotEventSnapshotBegin || events[len(events)-1].kind != gateway.BotEventSnapshotEnd {
		t.Fatalf("the snapshot is not bracketed by snapshot_begin and snapshot_end: %+v", events)
	}
	for _, ev := range []feedEvent{events[0], events[len(events)-1]} {
		if feedID(t, ev.body[0]) != f.community {
			t.Fatal("snapshot_begin or snapshot_end names another community")
		}
	}
	members := map[id.ID][]id.ID{}
	roles := map[id.ID][]uint64{}
	var lists []id.ID
	var marked, overwritten []id.ID
	var owner id.ID
	for _, ev := range events[1 : len(events)-1] {
		switch ev.kind {
		case gateway.BotEventDeviceList:
			user, push := pushOf(t, ev)
			if push.History[0].Version != 1 {
				t.Fatal("a snapshot push does not start at version 1")
			}
			lists = append(lists, user)
		case gateway.BotEventMember:
			if feedID(t, ev.body[0]) != f.community || feedUint(t, ev.body[2]) != 1 {
				t.Fatal("a snapshot member event is not a join in the Bot's community")
			}
			var held []id.ID
			if err := cborx.Unmarshal(ev.body[3], &held); err != nil {
				t.Fatal(err)
			}
			members[feedID(t, ev.body[1])] = held
		case gateway.BotEventRole:
			var bits []uint64
			if err := cborx.Unmarshal(ev.body[2], &bits); err != nil {
				t.Fatal(err)
			}
			roles[feedID(t, ev.body[1])] = bits
		case gateway.BotEventBotChannel:
			if feedUint(t, ev.body[2]) != 1 {
				t.Fatal("a snapshot bot_channel event is not a mark")
			}
			marked = append(marked, feedID(t, ev.body[1]))
		case gateway.BotEventCommunity:
			owner = feedID(t, ev.body[1])
		case gateway.BotEventChannelOverwrites:
			overwritten = append(overwritten, feedID(t, ev.body[1]))
		default:
			t.Fatalf("unexpected kind %d in the snapshot", ev.kind)
		}
	}
	// Task 15: the overwrites of the Bot channel only (the fixture has no voice channel; the unmarked one is not named).
	if !slices.Equal(overwritten, []id.ID{f.text}) {
		t.Fatalf("snapshot channel_overwrites for %v, want only the Bot channel %v", overwritten, f.text)
	}
	if !slices.Equal(lists, []id.ID{f.member.User.ID}) {
		t.Fatalf("snapshot device lists %v, want only the member's (the owner and the Bot have none, the stranger is not in scope)", lists)
	}
	if len(members) != 3 || !slices.Equal(members[f.member.User.ID], []id.ID{f.mods.ID}) {
		t.Fatalf("snapshot members %v, want owner, member (with mods) and the Bot", members)
	}
	if _, ok := members[f.stranger.User.ID]; ok {
		t.Fatal("the snapshot names a user outside the Bot's community")
	}
	if want := []uint64{f.mods.Allow, f.mods.Deny, f.mods.Position}; len(roles) != 2 || !slices.Equal(roles[f.mods.ID], want) {
		t.Fatalf("snapshot roles %v, want everyone and mods %v", roles, want)
	}
	if !slices.Equal(marked, []id.ID{f.text}) {
		t.Fatalf("snapshot Bot channels %v, want %v", marked, []id.ID{f.text})
	}
	if owner != f.owner.ID {
		t.Fatalf("snapshot owner %s, want %s", owner, f.owner.ID)
	}
	if got := f.gw.take(f.botDevice); len(got) != 0 {
		t.Fatalf("the cold start also sent %d live frames", len(got))
	}
}

func TestUserOutsideBotCommunitiesIsNotFed(t *testing.T) {
	f := newFeedFixture(t)
	f.identify(t)
	f.gw.take(f.botDevice)
	// The stranger publishes a device list, and their other community changes: neither is the Bot's.
	f.publish(t, f.stranger, 1, nil)
	ctx := context.Background()
	if err := f.deps.Repo.PutRole(ctx, store.RoleRow{ID: id.New(), CommunityID: f.otherScope, Name: "x", Position: 1, Created: 1}); err != nil {
		t.Fatal(err)
	}
	f.feed.CommunityChanged(f.otherScope)
	f.feed.BotChannelMarked(f.otherScope, id.New())
	f.feed.GroupEpoch(ds.EpochChange{GroupID: id.New(), Kind: 0, CommunityID: &f.otherScope, TargetID: id.New(), Epoch: 3, Devices: []id.ID{f.botDevice}})
	f.drain(t)
	if got := f.gw.take(f.botDevice); len(got) != 0 {
		t.Fatalf("the Bot was fed %d events about users and communities outside its own", len(got))
	}
	// The member's own write in the Bot's community is fed: the control.
	f.publish(t, f.member, 1, nil)
	f.drain(t)
	if got := decodeFeed(t, f.gw.take(f.botDevice)); len(got) != 1 || got[0].kind != gateway.BotEventDeviceList {
		t.Fatalf("the member's device list was not fed: %+v", got)
	}
}

// A join, a role grant, a role change and a leave reach the Bot as member and role events, and a
// joiner's device list follows their join. A leave is an event.
func TestMembershipAndRoleChangesAreFed(t *testing.T) {
	f := newFeedFixture(t)
	f.identify(t)
	f.gw.take(f.botDevice)
	ctx := context.Background()
	repo := f.deps.Repo
	joiner := seedListedUser(t, f.deps, 0x7c, 1)
	_, joinerBlob := f.publish(t, joiner, 1, nil) // not in scope yet: nothing
	f.drain(t)
	if got := f.gw.take(f.botDevice); len(got) != 0 {
		t.Fatalf("a non-member's device list was fed: %d events", len(got))
	}
	if err := repo.PutMember(ctx, store.MemberOfCommunityRow{CommunityID: f.community, UserID: joiner.User.ID, EpisodeID: id.New(), Joined: 2}); err != nil {
		t.Fatal(err)
	}
	if err := repo.PutMemberRole(ctx, f.community, joiner.User.ID, f.mods.ID); err != nil {
		t.Fatal(err)
	}
	f.feed.CommunityChanged(f.community)
	f.drain(t)
	events := decodeFeed(t, f.gw.take(f.botDevice))
	if len(events) != 2 || events[0].kind != gateway.BotEventMember || events[1].kind != gateway.BotEventDeviceList {
		t.Fatalf("a join sent %+v, want a member event then the joiner's device list", events)
	}
	if feedID(t, events[0].body[1]) != joiner.User.ID || feedUint(t, events[0].body[2]) != 1 {
		t.Fatal("the member event is not the joiner's join")
	}
	if user, _ := pushOf(t, events[1]); user != joiner.User.ID {
		t.Fatal("the device list after the join is not the joiner's")
	}

	// A role's bits change, and the joiner leaves.
	changed := f.mods
	changed.Allow |= uint64(api.PermBanMembers)
	if err := repo.PutRole(ctx, changed); err != nil {
		t.Fatal(err)
	}
	if err := repo.DeleteMember(ctx, f.community, joiner.User.ID); err != nil {
		t.Fatal(err)
	}
	f.feed.CommunityChanged(f.community)
	f.drain(t)
	events = decodeFeed(t, f.gw.take(f.botDevice))
	var sawRole, sawLeave bool
	for _, ev := range events {
		switch ev.kind {
		case gateway.BotEventRole:
			var bits []uint64
			if err := cborx.Unmarshal(ev.body[2], &bits); err != nil {
				t.Fatal(err)
			}
			sawRole = feedID(t, ev.body[1]) == f.mods.ID && bits[0] == changed.Allow && feedUint(t, ev.body[3]) == 0
		case gateway.BotEventMember:
			sawLeave = feedID(t, ev.body[1]) == joiner.User.ID && feedUint(t, ev.body[2]) == 0
		default:
			t.Fatalf("unexpected kind %d", ev.kind)
		}
	}
	if !sawRole || !sawLeave || len(events) != 2 {
		t.Fatalf("role change and leave sent %+v, want one role event and one leave", events)
	}
	// After the leave, the former member's device list is no longer fed.
	f.publish(t, joiner, 2, joinerBlob)
	f.drain(t)
	if got := f.gw.take(f.botDevice); len(got) != 0 {
		t.Fatalf("a former member's device list was fed: %d events", len(got))
	}
	// A deleted role is an event too.
	if err := repo.DeleteRole(ctx, f.community, f.mods.ID); err != nil {
		t.Fatal(err)
	}
	f.feed.CommunityChanged(f.community)
	f.drain(t)
	events = decodeFeed(t, f.gw.take(f.botDevice))
	var deleted bool
	for _, ev := range events {
		if ev.kind == gateway.BotEventRole && feedID(t, ev.body[1]) == f.mods.ID && feedUint(t, ev.body[3]) == 1 {
			deleted = true
		}
	}
	if !deleted {
		t.Fatalf("a role delete sent %+v, want a deleted role event", events)
	}
}

// Marks and unmarks reach the Bot as bot_channel events, and an epoch of a Bot channel's group or of a
// call the Bot is in reaches it as channel_group; an ordinary channel's or a call without the Bot does not.
func TestBotChannelsAndChannelGroupsAreFed(t *testing.T) {
	f := newFeedFixture(t)
	f.identify(t)
	f.gw.take(f.botDevice)
	ctx := context.Background()
	if _, err := f.deps.Repo.UnmarkBotChannel(ctx, f.community, f.text); err != nil {
		t.Fatal(err)
	}
	f.feed.BotChannelUnmarked(f.community, f.text)
	if _, err := f.deps.Repo.MarkBotChannel(ctx, store.BotChannelRow{ChannelID: f.other, CommunityID: f.community, MarkedAt: 9, MarkedBy: f.owner.ID}); err != nil {
		t.Fatal(err)
	}
	f.feed.BotChannelMarked(f.community, f.other)
	f.drain(t)
	got, overwrites := map[id.ID]uint64{}, map[id.ID]uint64{}
	for _, ev := range decodeFeed(t, f.gw.take(f.botDevice)) {
		switch ev.kind {
		case gateway.BotEventBotChannel:
			got[feedID(t, ev.body[1])] = feedUint(t, ev.body[2])
		case gateway.BotEventChannelOverwrites:
			// Task 15: a channel that stops being a Bot channel is deleted from the Bot's view, a new one is named.
			overwrites[feedID(t, ev.body[1])] = feedUint(t, ev.body[3])
		default:
			t.Fatalf("unexpected kind %d", ev.kind)
		}
	}
	if len(got) != 2 || got[f.text] != 0 || got[f.other] != 1 {
		t.Fatalf("bot_channel events %v, want text unmarked and other marked", got)
	}
	if len(overwrites) != 2 || overwrites[f.text] != 1 || overwrites[f.other] != 0 {
		t.Fatalf("channel_overwrites %v, want text deleted and other named", overwrites)
	}

	cid := f.community
	botChannelGroup, plainGroup, botCall, otherCall := id.New(), id.New(), id.New(), id.New()
	human := id.New()
	f.feed.GroupEpoch(ds.EpochChange{GroupID: botChannelGroup, Kind: 0, CommunityID: &cid, TargetID: f.other, Epoch: 4, Devices: []id.ID{human}})
	f.feed.GroupEpoch(ds.EpochChange{GroupID: plainGroup, Kind: 0, CommunityID: &cid, TargetID: f.text, Epoch: 5, Devices: []id.ID{human}})
	f.feed.GroupEpoch(ds.EpochChange{GroupID: botCall, Kind: 1, CommunityID: &cid, TargetID: id.New(), Epoch: 6, Devices: []id.ID{human, f.botDevice}})
	f.feed.GroupEpoch(ds.EpochChange{GroupID: otherCall, Kind: 1, CommunityID: &cid, TargetID: id.New(), Epoch: 7, Devices: []id.ID{human}})
	f.drain(t)
	groups := map[id.ID]uint64{}
	for _, ev := range decodeFeed(t, f.gw.take(f.botDevice)) {
		if ev.kind != gateway.BotEventChannelGroup || feedID(t, ev.body[0]) != f.community {
			t.Fatalf("unexpected event kind %d", ev.kind)
		}
		groups[feedID(t, ev.body[2])] = feedUint(t, ev.body[3])
	}
	if len(groups) != 2 || groups[botChannelGroup] != 4 || groups[botCall] != 6 {
		t.Fatalf("channel_group events %v, want the Bot channel's group at 4 and the Bot's call at 6", groups)
	}
}

// TestColdStartSnapshotCarriesChannelGroups (Bot v2 task 14, Task 12 review N-1): the gateway drops a live op 65 while
// a snapshot is owed, so the snapshot itself must carry every channel_group the Bot is fed: the current group of each
// Bot channel and each open call group the Bot's device is a member of, in its community. An ordinary channel's group,
// a call without the Bot and a closed call are not carried.
func TestColdStartSnapshotCarriesChannelGroups(t *testing.T) {
	f := newFeedFixture(t)
	ctx := context.Background()
	repo := f.deps.Repo
	now := f.deps.Clock.Now().Unix()
	cid := f.community
	group := func(kind uint8, target id.ID, epoch uint64, botMember, closed bool) id.ID {
		t.Helper()
		g := id.New()
		row := store.GroupRow{GroupID: g, Binding: []byte{0x80}, Kind: kind, CommunityID: &cid, TargetID: target, Ciphersuite: 1, Epoch: epoch,
			ExternalSenderKeyID: id.New(), E2EEVersion: 1, MediaVersion: 1, PolicyVersion: 1, Created: now}
		if kind == 1 {
			call := id.New()
			row.CallID = &call
		}
		if closed {
			row.ClosedAt = &now
		}
		if err := repo.CreateGroup(ctx, row); err != nil {
			t.Fatal(err)
		}
		members := []store.MemberRow{{GroupID: g, LeafIndex: 0, UserID: f.owner.ID, DeviceID: id.New(), SignatureKey: make([]byte, 32), AddedEpoch: 1}}
		if botMember {
			members = append(members, store.MemberRow{GroupID: g, LeafIndex: 1, UserID: f.botUser, DeviceID: f.botDevice, SignatureKey: make([]byte, 32), AddedEpoch: 1})
		}
		if err := repo.ReplaceMembers(ctx, g, epoch, members); err != nil {
			t.Fatal(err)
		}
		return g
	}
	botChannelText := group(0, f.text, 7, true, false)
	plainText := group(0, f.other, 3, true, false)
	voice := id.New()
	botCall := group(1, voice, 5, true, false)
	_ = group(1, voice, 9, false, false)  // a call without the Bot
	_ = group(1, id.New(), 2, true, true) // a closed call
	s := f.identify(t)
	if !s.finished {
		t.Fatal("the snapshot did not finish")
	}
	got := map[id.ID][2]uint64{}
	var targets = map[id.ID]id.ID{}
	events := decodeFeed(t, s.sent)
	endAt := -1
	for i, ev := range events {
		switch ev.kind {
		case gateway.BotEventChannelGroup:
			if feedID(t, ev.body[0]) != f.community {
				t.Fatalf("channel_group of community %v", feedID(t, ev.body[0]))
			}
			g := feedID(t, ev.body[2])
			got[g] = [2]uint64{uint64(i), feedUint(t, ev.body[3])}
			targets[g] = feedID(t, ev.body[1])
		case gateway.BotEventSnapshotEnd:
			endAt = i
		}
	}
	if len(got) != 2 || got[botChannelText][1] != 7 || got[botCall][1] != 5 || targets[botChannelText] != f.text || targets[botCall] != voice {
		t.Fatalf("snapshot channel_group events %v (targets %v), want the Bot channel's group at 7 and the Bot's call at 5; not %v", got, targets, plainText)
	}
	for g, at := range got {
		if endAt >= 0 && int(at[0]) > endAt {
			t.Fatalf("channel_group %v follows snapshot_end", g)
		}
	}
}

// A user whose push would exceed the 1 MiB frame cap is skipped with a metric; the feed goes on.
func TestOversizedDeviceListIsSkippedWithAMetric(t *testing.T) {
	f := newFeedFixture(t)
	ctx := context.Background()
	now := f.deps.Clock.Now().Unix()
	big := seedListedUser(t, f.deps, 0x2d, 1)
	if err := f.deps.Repo.PutMember(ctx, store.MemberOfCommunityRow{CommunityID: f.community, UserID: big.User.ID, EpisodeID: id.New(), Joined: now}); err != nil {
		t.Fatal(err)
	}
	// Stored straight: four 300 KiB rows, more than the cap together.
	prev := make([]byte, 32)
	for v := uint64(1); v <= 4; v++ {
		blob := bytes.Repeat([]byte{byte(v)}, 300<<10)
		if err := f.deps.Repo.PutDeviceList(ctx, store.DeviceListRow{UserID: big.User.ID, Version: v, Blob: blob, SSKSignature: make([]byte, 64), PrevHash: prev, Created: now}); err != nil {
			t.Fatal(err)
		}
		prev = blob[:32]
	}
	f.publish(t, f.member, 1, nil)
	s := f.identify(t)
	var lists []id.ID
	for _, ev := range decodeFeed(t, s.sent) {
		if ev.kind == gateway.BotEventDeviceList {
			user, _ := pushOf(t, ev)
			lists = append(lists, user)
		}
	}
	if !slices.Equal(lists, []id.ID{f.member.User.ID}) || !s.finished {
		t.Fatalf("snapshot device lists %v (finished %v), want only the member's", lists, s.finished)
	}
	if !slices.Contains(f.skipped.list(), "too_large") {
		t.Fatalf("skip reasons %v, want too_large", f.skipped.list())
	}
}

// A Bot that is offline (no connection takes the frame) is not fed until its next identify, which
// streams a fresh snapshot.
func TestOfflineBotIsResyncedByItsNextIdentify(t *testing.T) {
	f := newFeedFixture(t)
	f.identify(t)
	f.gw.mu.Lock()
	f.gw.offline = true
	f.gw.mu.Unlock()
	_, blob1 := f.publish(t, f.member, 1, nil)
	f.drain(t)
	f.gw.mu.Lock()
	f.gw.offline = false
	f.gw.mu.Unlock()
	f.publish(t, f.member, 2, blob1)
	f.drain(t)
	if got := f.gw.take(f.botDevice); len(got) != 0 {
		t.Fatalf("an offline Bot was fed %d events before identifying again", len(got))
	}
	s := f.identify(t)
	if !s.finished {
		t.Fatal("the next identify streamed no snapshot")
	}
}

func TestStructuralEventRoutesAreGone(t *testing.T) {
	f := newFeedFixture(t)
	botTok := "feed-bot-session-" + f.install.String()
	now := f.deps.Clock.Now().Unix()
	ctx := context.Background()
	if err := f.deps.Repo.CreateSession(ctx, store.SessionRow{TokenHash: auth.TokenHash(botTok), DeviceID: f.botDevice, UserID: f.botUser, Scope: uint8(auth.ScopeEnrolled), Created: now, Expires: now + 3600, IdleExpires: now + 3600}); err != nil {
		t.Fatal(err)
	}
	if err := f.deps.Repo.SetSessionVersions(ctx, auth.TokenHash(botTok), 6, 1, 1); err != nil {
		t.Fatal(err)
	}
	if err := f.deps.Repo.CreateBotSessionBinding(ctx, store.BotSessionBindingRow{TokenHash: auth.TokenHash(botTok), InstallationID: f.install, UserID: f.botUser, DeviceID: f.botDevice, Created: now}); err != nil {
		t.Fatal(err)
	}
	disp := api.NewSocketDispatcher(muxOf(t, f.h), api.SocketDispatchOptions{Clock: f.deps.Clock})
	base := "/v1/communities/" + f.community.String() + "/bot/events"
	for _, route := range []struct{ method, path string }{{http.MethodGet, base + "?after=0"}, {http.MethodPut, base + "/ack"}} {
		if rec := doAuth(f.h, route.method, route.path, botTok); rec.Code != http.StatusNotFound {
			t.Fatalf("HTTP %s %s = %d, want 404", route.method, route.path, rec.Code)
		}
		status, _ := sockettest.Do(t, disp, auth.Session{TokenHash: auth.TokenHash(botTok), DeviceID: f.botDevice}, route.method, route.path, nil)
		if status != http.StatusNotFound {
			t.Fatalf("socket %s %s = %d, want 404", route.method, route.path, status)
		}
	}
}

type heardCommunities struct {
	mu  sync.Mutex
	got []id.ID
}

func (h *heardCommunities) DeviceListChanged(id.ID) {}
func (h *heardCommunities) CommunityChanged(c id.ID) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.got = append(h.got, c)
}

// Every structure change the members are told of (op 54, after its commit) reaches the Bot feed too:
// a change, a removal and a deleted community's last notice.
func TestStructureNoticesReachTheBotFeed(t *testing.T) {
	_, deps := newTestAPIWired(t, nil)
	heard := &heardCommunities{}
	n := api.NewStructureNotifier(newRelayGateway(), deps.Repo, deps.Log).WithBotFeed(heard)
	a, b, c := id.New(), id.New(), id.New()
	n.Changed(context.Background(), a)
	n.Removed(context.Background(), b, id.New())
	n.ChangedTo(context.Background(), c, nil)
	heard.mu.Lock()
	defer heard.mu.Unlock()
	if !slices.Equal(heard.got, []id.ID{a, b, c}) {
		t.Fatalf("the feed heard %v, want %v", heard.got, []id.ID{a, b, c})
	}
}

// --- Fix round 1 ---------------------------------------------------------------------------------

// blockingStream is a snapshot stream that stops on one chosen Send until released, then answers ok.
type blockingStream struct {
	feedStream
	stopAt  func(gateway.Frame) bool
	reached chan struct{}
	release chan struct{}
	ok      bool
	once    sync.Once
}

func (s *blockingStream) Send(ctx context.Context, f gateway.Frame) bool {
	if s.stopAt(f) {
		s.once.Do(func() {
			close(s.reached)
			<-s.release
		})
		if !s.ok {
			return false
		}
	}
	return s.feedStream.Send(ctx, f)
}

func isDeviceListOf(t *testing.T, user id.ID) func(gateway.Frame) bool {
	return func(f gateway.Frame) bool {
		evs := decodeFeed(t, []gateway.Frame{f})
		if evs[0].kind != gateway.BotEventDeviceList {
			return false
		}
		u, _ := pushOf(t, evs[0])
		return u == user
	}
}

// I-1: the Bot re-identifies while the worker is still failing on its old connection. The failure
// must not leave the feed offline through the new snapshot: a device-list change committed during
// that snapshot, after it read the user's list, still reaches the Bot after snapshot_end.
func TestReidentifyWhileTheOldConnectionFailsKeepsTheFeedOnline(t *testing.T) {
	f := newFeedFixture(t)
	_, blob1 := f.publish(t, f.member, 1, nil)
	stops := isDeviceListOf(t, f.member.User.ID)
	s1 := &blockingStream{feedStream: feedStream{installation: f.install, community: f.community, device: f.botDevice},
		stopAt: stops, reached: make(chan struct{}), release: make(chan struct{}), ok: false}
	f.feed.BotIdentified(s1)
	<-s1.reached // the worker is blocked on the old connection
	s2 := &blockingStream{feedStream: feedStream{installation: f.install, community: f.community, device: f.botDevice},
		stopAt: stops, reached: make(chan struct{}), release: make(chan struct{}), ok: true}
	f.feed.BotIdentified(s2)
	close(s1.release) // the old connection's send fails now, after the new identify
	<-s2.reached      // the new snapshot has read the member's list (v1) and is sending it
	f.publish(t, f.member, 2, blob1)
	close(s2.release)
	f.drain(t)
	if !s2.finished {
		t.Fatal("the new snapshot did not finish")
	}
	var heads []uint64
	for _, ev := range decodeFeed(t, f.gw.take(f.botDevice)) {
		if ev.kind == gateway.BotEventDeviceList {
			_, push := pushOf(t, ev)
			heads = append(heads, push.History[len(push.History)-1].Version)
		}
	}
	if !slices.Equal(heads, []uint64{2}) {
		t.Fatalf("live device-list heads after the new snapshot = %v, want [2]", heads)
	}
}

// M-4: a revoked installation's feed is forgotten: its worker stops and nothing more is queued for it.
func TestForgottenInstallationIsNotFed(t *testing.T) {
	f := newFeedFixture(t)
	_, blob1 := f.publish(t, f.member, 1, nil)
	f.identify(t)
	f.gw.take(f.botDevice)
	f.feed.Forget(f.install)
	f.publish(t, f.member, 2, blob1)
	f.feed.CommunityChanged(f.community)
	f.drain(t)
	if got := f.gw.take(f.botDevice); len(got) != 0 {
		t.Fatalf("a forgotten installation was fed %d events", len(got))
	}
}

// --- Task 15: channel overwrites and member timeouts -------------------------------------------------

// TestChannelOverwritesAreFedForBotAndVoiceChannelsOnly: an overwrite change reaches the Bot for a Bot channel and a
// voice channel, with @everyone, role and member targets told apart; an ordinary channel's overwrites never do.
func TestChannelOverwritesAreFedForBotAndVoiceChannelsOnly(t *testing.T) {
	f := newFeedFixture(t)
	f.identify(t)
	f.gw.take(f.botDevice)
	ctx := context.Background()
	repo := f.deps.Repo
	cid := f.community
	voice := id.New()
	if err := repo.CreateChannel(ctx, store.ChannelRow{ID: voice, CommunityID: &cid, Kind: api.ChannelVoice, Mode: api.ModeE2EE, Name: "voice", SettingsJSON: []byte(`{}`), HostPolicyVersion: 1, Created: 1}); err != nil {
		t.Fatal(err)
	}
	for _, o := range []store.OverwriteRow{
		{ChannelID: f.text, TargetKind: 0, TargetID: f.everyone.ID, Deny: uint64(api.PermSendMessages)},
		{ChannelID: f.text, TargetKind: 0, TargetID: f.mods.ID, Allow: uint64(api.PermSendMessages)},
		{ChannelID: f.text, TargetKind: 1, TargetID: f.member.User.ID, Deny: uint64(api.PermAddReactions)},
		{ChannelID: f.other, TargetKind: 0, TargetID: f.everyone.ID, Deny: uint64(api.PermSendMessages)},
	} {
		if err := repo.PutOverwrite(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	f.feed.CommunityChanged(f.community)
	f.drain(t)
	seen := map[id.ID][][]any{}
	for _, ev := range decodeFeed(t, f.gw.take(f.botDevice)) {
		if ev.kind != gateway.BotEventChannelOverwrites {
			continue
		}
		var rows [][]any
		if err := cborx.Unmarshal(ev.body[2], &rows); err != nil || feedUint(t, ev.body[3]) != 0 || feedID(t, ev.body[0]) != f.community {
			t.Fatalf("channel_overwrites %v %v", ev.body, err)
		}
		seen[feedID(t, ev.body[1])] = rows
	}
	if _, ok := seen[f.other]; ok {
		t.Fatal("an ordinary channel's overwrites reached the Bot")
	}
	if rows, ok := seen[voice]; !ok || len(rows) != 0 {
		t.Fatalf("the voice channel = %v %v, want named with no overwrites", rows, ok)
	}
	kinds := map[uint64]uint64{}
	for _, row := range seen[f.text] {
		kinds[row[0].(uint64)]++
	}
	if len(seen[f.text]) != 3 || kinds[0] != 1 || kinds[1] != 1 || kinds[2] != 1 {
		t.Fatalf("the Bot channel's overwrites = %v, want one @everyone, one role and one member entry", seen[f.text])
	}
	// Nothing changed: nothing is sent again.
	f.feed.CommunityChanged(f.community)
	f.drain(t)
	for _, ev := range decodeFeed(t, f.gw.take(f.botDevice)) {
		if ev.kind == gateway.BotEventChannelOverwrites {
			t.Fatal("unchanged overwrites were sent again")
		}
	}
}

// TestMemberTimeoutIsFedInTheSnapshotAndOnChange: a timed-out member's end is in the snapshot, and a change made
// through the timeout routes' notice (StructureNotifier.ChangedUser) reaches the Bot, 0 once cleared.
func TestMemberTimeoutIsFedInTheSnapshotAndOnChange(t *testing.T) {
	f := newFeedFixture(t)
	ctx := context.Background()
	repo := f.deps.Repo
	until := time.Now().Unix() + 3600
	if err := repo.PutMemberTimeout(ctx, store.MemberTimeoutRow{CommunityID: f.community, UserID: f.member.User.ID, ByUser: f.owner.ID, Created: 1, Expires: until}); err != nil {
		t.Fatal(err)
	}
	s := f.identify(t)
	timeouts := map[id.ID]uint64{}
	for _, ev := range decodeFeed(t, s.sent) {
		if ev.kind == gateway.BotEventMemberTimeout {
			timeouts[feedID(t, ev.body[1])] = feedUint(t, ev.body[2])
		}
	}
	if len(timeouts) != 1 || timeouts[f.member.User.ID] != uint64(until) {
		t.Fatalf("snapshot member_timeout %v, want the member until %d", timeouts, until)
	}
	f.gw.take(f.botDevice)
	if err := repo.DeleteMemberTimeout(ctx, f.community, f.member.User.ID); err != nil {
		t.Fatal(err)
	}
	api.NewStructureNotifier(newRelayGateway(), repo, f.deps.Log).WithBotFeed(f.feed).ChangedUser(ctx, f.community, f.member.User.ID)
	f.drain(t)
	var got []uint64
	for _, ev := range decodeFeed(t, f.gw.take(f.botDevice)) {
		if ev.kind == gateway.BotEventMemberTimeout && feedID(t, ev.body[1]) == f.member.User.ID {
			got = append(got, feedUint(t, ev.body[2]))
		}
	}
	if !slices.Equal(got, []uint64{0}) {
		t.Fatalf("member_timeout after clearing = %v, want [0]", got)
	}
}

// The feed reads timeouts at its own clock (fix round 1, M-5), as requireNotTimedOut does at the handler's: a timeout
// ending a minute after the instance's now is current, whatever the machine's wall clock says.
func TestMemberTimeoutIsReadAtTheFeedsClock(t *testing.T) {
	f := newFeedFixture(t)
	until := f.deps.Clock.Now().Unix() + 60
	if err := f.deps.Repo.PutMemberTimeout(context.Background(), store.MemberTimeoutRow{CommunityID: f.community, UserID: f.member.User.ID,
		ByUser: f.owner.ID, Created: f.deps.Clock.Now().Unix(), Expires: until}); err != nil {
		t.Fatal(err)
	}
	s := f.identify(t)
	var got []uint64
	for _, ev := range decodeFeed(t, s.sent) {
		if ev.kind == gateway.BotEventMemberTimeout {
			got = append(got, feedUint(t, ev.body[2]))
		}
	}
	if !slices.Equal(got, []uint64{uint64(until)}) {
		t.Fatalf("snapshot member_timeout = %v, want [%d]", got, until)
	}
}
