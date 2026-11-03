package testkit_test

// dilla-web-1 task 17: `dilla-testkit web-driver`, the native peer of the browser tests, against an
// in-process instance wired as production wires it. Two driver processes enrol through the
// bootstrap invite, build a community through the public routes, meet in its channel's text group
// and exchange messages. Then one upload is relabelled in flight, so the delivery service records
// the other device as the uploader of a ciphertext it did not make, and the receiving driver must
// refuse it: the sender a browser test reads from `sync` is the MLS-authenticated one, never the
// delivery service's label (C22).

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
	"github.com/dilla-chat/dilla/internal/testkit"
)

var hex32 = regexp.MustCompile(`^[0-9a-f]{32}$`)

// driverWait bounds one answer and one polling loop. Generous on purpose: CI runs this package
// under the race detector on a 4-vCPU runner (lesson c); nothing here asserts a duration.
const driverWait = 90 * time.Second

// lockedBuffer collects a driver's stderr; exec's copier writes it while a failing test reads it.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func TestWebDriverTypingRespectsAudience(t *testing.T) {
	h := testkit.Start(t, testkit.Options{DataDir: t.TempDir(), ProductionACL: true})
	t.Cleanup(h.Stop)
	alice := startWebDriver(t, h, h.BaseURL(), "alice", 0xa11ce9)
	bob := startWebDriver(t, h, h.BaseURL(), "bob", 0xb0b9)
	outsider := startWebDriver(t, h, h.BaseURL(), "outsider", 0xc0de9)
	sa := alice.ok("setup", map[string]any{"community": "private typing", "channel": "general"})
	ra := alice.ok("register", nil)
	sb := bob.ok("setup", map[string]any{"community": "bob", "channel": "other"})
	outsider.ok("setup", map[string]any{"community": "outsider", "channel": "other"})
	bob.ok("join", map[string]any{
		"community_id": sa["community_id"], "channel_id": sa["channel_id"],
		"group_id": ra["group_id"], "invite_code": sa["invite_code"],
	})
	channel := mustID(t, sa["channel_id"])
	aDev, aUser := mustID(t, sa["device_id"]), mustID(t, sa["user_id"])
	bDev, bUser := mustID(t, sb["device_id"]), mustID(t, sb["user_id"])
	if got := alice.ok("typing", map[string]any{"channel": channel.String(), "on": true}); got["accepted"] != true {
		t.Fatalf("typing answer = %v", got)
	}
	if !h.Gateway().IsTyping(channel, aUser, aDev) {
		t.Fatal("production gateway did not record alice typing")
	}
	if h.Gateway().IsTyping(channel, bUser, bDev) {
		t.Fatal("bob was impersonated")
	}
	denied := outsider.call("typing", map[string]any{"channel": channel.String(), "on": true})
	if denied["ok"] != false || !strings.Contains(fmt.Sprint(denied["error"]), "E_NOT_FOUND") {
		t.Fatalf("outsider typing = %v, want E_NOT_FOUND", denied)
	}
	if denied["error"] != "scenario: E_NOT_FOUND" {
		t.Fatalf("outsider typing code = %v, want scenario: E_NOT_FOUND", denied["error"])
	}
	if got := alice.ok("search_seed", map[string]any{"channel": channel.String(), "body": "needle lantern"}); got["seq"] == nil {
		t.Fatalf("search_seed has no real DS sequence: %v", got)
	}
	if row := bob.waitFor("needle lantern"); row["sender_user"] != sa["user_id"] {
		t.Fatalf("native peer received %v", row)
	}
}

func TestWebDriverTrustListAndRotationUseSignedHistory(t *testing.T) {
	h := testkit.Start(t, testkit.Options{DataDir: t.TempDir(), ProductionACL: true})
	t.Cleanup(h.Stop)
	alice := startWebDriver(t, h, h.BaseURL(), "alice", 0xa11cea)
	bob := startWebDriver(t, h, h.BaseURL(), "bob", 0xb0ba)
	sa := alice.ok("setup", map[string]any{"community": "trust history", "channel": "general"})
	ra := alice.ok("register", nil)
	bob.ok("setup", map[string]any{"community": "bob", "channel": "other"})
	bob.ok("join", map[string]any{
		"community_id": sa["community_id"], "channel_id": sa["channel_id"],
		"group_id": ra["group_id"], "invite_code": sa["invite_code"],
	})
	before := bob.ok("trust_list", map[string]any{"user": sa["user_id"]})
	if before["version"] != float64(1) || before["pin"] == "" || before["changed"] != nil {
		t.Fatalf("first signed list = %v", before)
	}
	rotated := alice.ok("rotate_umk", map[string]any{"user": sa["user_id"]})
	if rotated["user"] != sa["user_id"] || rotated["version"] != float64(2) {
		t.Fatalf("rotation = %v", rotated)
	}
	after := bob.ok("trust_list", map[string]any{"user": sa["user_id"]})
	if after["version"] != float64(2) || after["pin"] != before["pin"] || after["changed"] == nil || after["verified"] != false {
		t.Fatalf("changed identity = %v; before = %v", after, before)
	}
}

// After rotate_umk the peer acts as its replacement device: rotate_umk removed the old leaf from the group, so a send
// from the old device is refused with E_LEAF_NOT_CURRENT. The browser specs (task 10) have the changed contact send
// after its rotation and read the browser's answer, and the witness pendingUmk is the replacement's public UMK.
func TestWebDriverRotatedPeerSendsAndReadsFromItsNewDevice(t *testing.T) {
	h := testkit.Start(t, testkit.Options{DataDir: t.TempDir(), ProductionACL: true})
	t.Cleanup(h.Stop)
	alice := startWebDriver(t, h, h.BaseURL(), "alice", 0xa11ced)
	bob := startWebDriver(t, h, h.BaseURL(), "bob", 0xb0bb)
	sa := alice.ok("setup", map[string]any{"community": "rotated sender", "channel": "general"})
	ra := alice.ok("register", nil)
	bob.ok("setup", map[string]any{"community": "bob", "channel": "other"})
	bob.ok("join", map[string]any{
		"community_id": sa["community_id"], "channel_id": sa["channel_id"],
		"group_id": ra["group_id"], "invite_code": sa["invite_code"],
	})
	alice.ok("sync", nil) // bob's external join moved the epoch
	alice.ok("send", map[string]any{"body": "before rotation"})
	if row := bob.waitFor("before rotation"); row["sender_device"] != sa["device_id"] {
		t.Fatalf("before rotation came from %v, want %v", row["sender_device"], sa["device_id"])
	}
	if first := bob.ok("trust_list", map[string]any{"user": sa["user_id"]}); first["changed"] != nil {
		t.Fatalf("first sight = %v", first)
	}
	alice.ok("rotate_umk", nil)
	alice.ok("send", map[string]any{"body": "after rotation"})
	row := bob.waitFor("after rotation")
	if row["sender_user"] != sa["user_id"] || row["sender_device"] == sa["device_id"] {
		t.Fatalf("after rotation = %v; the old device is %v", row, sa["device_id"])
	}
	bob.ok("send", map[string]any{"channel_id": sa["channel_id"], "body": "answer to the new device"})
	alice.waitFor("answer to the new device")
	changed := bob.ok("trust_list", map[string]any{"user": sa["user_id"]})
	own := alice.ok("trust_list", map[string]any{"user": sa["user_id"]})
	if changed["changed"] == nil || own["pendingUmk"] != changed["changed"] {
		t.Fatalf("the rotated peer's own witness %v differs from the changed key bob holds %v", own, changed)
	}
}

func TestWebDriverVoiceSelfRefusesUnknownCall(t *testing.T) {
	h := testkit.Start(t, testkit.Options{DataDir: t.TempDir(), ProductionACL: true})
	t.Cleanup(h.Stop)
	alice := startWebDriver(t, h, h.BaseURL(), "alice", 0xa11ceb)
	alice.ok("setup", map[string]any{"community": "voice refusal", "channel": "general"})
	answer := alice.call("voice_self", map[string]any{"call": id.New().String(), "flags": 1})
	if answer["ok"] != false || !strings.Contains(fmt.Sprint(answer["error"]), "E_NOT_FOUND") {
		t.Fatalf("voice_self unknown call = %v", answer)
	}
}

func TestWebDriverVoiceSelfUsesStoredLiveParticipant(t *testing.T) {
	h := testkit.Start(t, testkit.Options{DataDir: t.TempDir(), ProductionACL: true, SFU: true})
	t.Cleanup(h.Stop)
	alice := startWebDriver(t, h, h.BaseURL(), "alice", 0xa11cec)
	sa := alice.ok("setup", map[string]any{"community": "voice witness", "channel": "general"})
	alice.ok("register", nil)
	call := id.New()
	channel := mustID(t, sa["channel_id"])
	device := mustID(t, sa["device_id"])
	h.InstallVoiceSession(t, call, channel, id.New())
	denied := alice.call("voice_self", map[string]any{"call": call.String(), "flags": 1})
	if denied["ok"] != false || !strings.Contains(fmt.Sprint(denied["error"]), "E_NOT_FOUND") {
		t.Fatalf("voice_self absent participant = %v, want E_NOT_FOUND", denied)
	}
	h.AddVoiceParticipant(call, device)
	if got := alice.ok("voice_self", map[string]any{"call": call.String(), "flags": 1}); got["accepted"] != true {
		t.Fatalf("voice_self answer = %v", got)
	}
}

// dilla-web-3 task 4 (L-E2E-30): the native peer joins a voice channel's call group through the real delivery
// service, a second actor (the device `enrol` added) joins and leaves it, and call_members reads the roster
// after a sync — the sync that also commits the leaver's own Remove.
func TestWebDriverCallVerbs(t *testing.T) {
	h := testkit.Start(t, testkit.Options{DataDir: t.TempDir(), ProductionACL: true})
	t.Cleanup(h.Stop)
	ctx := context.Background()
	peer := startWebDriver(t, h, h.BaseURL(), "peer", 0xca11a)
	sp := peer.ok("setup", map[string]any{"community": "voice verbs", "channel": "general",
		"password": "correct horse battery staple", "voice": true})
	voice := mustID(t, sp["voice_channel_id"])
	if ch, err := h.Repo().GetChannel(ctx, voice); err != nil || ch.Kind != api.ChannelVoice ||
		ch.CommunityID == nil || *ch.CommunityID != mustID(t, sp["community_id"]) {
		t.Fatalf("the voice channel row = %+v, %v; want a voice channel of the peer's community", ch, err)
	}
	peer.ok("register", nil)
	en := peer.ok("enrol", nil)
	peerDev, secondDev := sp["device_id"].(string), en["device_id"].(string)

	// 1. The peer finds no call group (L-HTTP-100 answers null) and registers one: leaf 0.
	j1 := peer.ok("call_join", map[string]any{"actor": "peer", "channel": voice.String()})
	group := mustID(t, j1["group"])
	if j1["leaf"] != float64(0) {
		t.Fatalf("the peer's call_join answered %v, want leaf 0", j1)
	}
	if groups, err := h.Repo().GroupsForTarget(ctx, voice, api.GroupCall); err != nil || len(groups) != 1 || groups[0].GroupID != group {
		t.Fatalf("open call groups of the voice channel = %v, %v; want exactly %s", groups, err, group)
	}
	if m := peer.ok("call_members", map[string]any{"actor": "peer", "channel": voice.String()}); m["group"] != j1["group"] {
		t.Fatalf("call_members answered group %v, want %v", m["group"], j1["group"])
	}
	waitCallMembers(t, peer, "peer", voice, peerDev)
	// web-3 task 10: call_read answers GET /v1/channels/{id}/call (L-HTTP-100) as the actor sees it: the group, no
	// call yet, and nobody in the room.
	if r := peer.ok("call_read", map[string]any{"actor": "peer", "channel": voice.String()}); r["group"] != j1["group"] ||
		r["call"] != nil || len(r["participants"].([]any)) != 0 {
		t.Fatalf("call_read answered %v, want group %v, call null and no participants", r, j1["group"])
	}

	// 2. The second actor reads the same group from the route and joins it by external commit: leaf 1.
	j2 := peer.ok("call_join", map[string]any{"actor": "second", "channel": voice.String()})
	if j2["group"] != j1["group"] || j2["leaf"] != float64(1) {
		t.Fatalf("the second actor's call_join answered %v, want group %v at leaf 1", j2, j1["group"])
	}
	waitCallMembers(t, peer, "peer", voice, peerDev, secondDev)

	// 3. The second actor proposes its own Remove; the peer's call_members commits it.
	if got := peer.ok("call_leave", map[string]any{"actor": "second", "channel": voice.String()}); len(got) != 2 {
		t.Fatalf("call_leave answered %v, want {}", got)
	}
	waitCallMembers(t, peer, "peer", voice, peerDev)
}

// waitCallMembers asks actor's call_members until its devices are exactly want, sorted.
func waitCallMembers(t *testing.T, d *webDriver, actor string, channel id.ID, want ...string) {
	t.Helper()
	sorted := slices.Clone(want)
	slices.Sort(sorted)
	deadline := time.Now().Add(driverWait)
	for {
		got := d.ok("call_members", map[string]any{"actor": actor, "channel": channel.String()})["devices"].([]any)
		match := len(got) == len(sorted)
		for i := 0; match && i < len(got); i++ {
			match = got[i] == any(sorted[i])
		}
		if match {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s's call roster is %v, want exactly %v in that order", actor, got, sorted)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// dilla-web-3 task 4: the peer starts a DM call; the browserless second actor (another driver) rings with op 53,
// declines with the expires_ts its own gateway received, and the peer's device hears the decline. The harness reads
// both devices' rings over gateway connections of its own.
func TestWebDriverCallStartRingsADMPeer(t *testing.T) {
	h := testkit.Start(t, testkit.Options{DataDir: t.TempDir(), ProductionACL: true, SFU: true})
	t.Cleanup(h.Stop)
	peer := startWebDriver(t, h, h.BaseURL(), "peer", 0xca11b)
	second := startWebDriver(t, h, h.BaseURL(), "second", 0xca11c)
	sp := peer.ok("setup", map[string]any{"community": "ringing", "channel": "general"})
	ss := second.ok("setup", map[string]any{"community": "second's own", "channel": "elsewhere"})
	peerUser, secondUser := mustID(t, sp["user_id"]), mustID(t, ss["user_id"])
	dm := peer.ok("open_dm", map[string]any{"user_id": ss["user_id"]})
	channel := mustID(t, dm["channel_id"])
	peerGW := dialGatewayAs(t, h, "peer", peerUser, mustID(t, sp["device_id"]))
	secondGW := dialGatewayAs(t, h, "second", secondUser, mustID(t, ss["device_id"]))

	if j := peer.ok("call_join", map[string]any{"actor": "peer", "channel": channel.String()}); j["leaf"] != float64(0) {
		t.Fatalf("the peer's call_join in the DM answered %v, want leaf 0", j)
	}
	started := peer.ok("call_start", map[string]any{"actor": "peer", "channel": channel.String()})
	call := mustID(t, started["call"])
	if started["created"] != true {
		t.Fatalf("the first call_start answered %v, want created", started)
	}
	ring := secondGW.nextRing(t)
	if ring.call != call || ring.channel != channel || ring.user != peerUser || ring.state != 1 || ring.expires == 0 {
		t.Fatalf("the second actor's device got %+v; want state 1 of call %s in %s from %s", ring, call, channel, peerUser)
	}

	if got := second.ok("call_decline", map[string]any{"actor": "peer", "call": call.String()}); len(got) != 2 {
		t.Fatalf("call_decline answered %v, want {}", got)
	}
	declined := peerGW.nextRing(t)
	if declined.call != call || declined.user != secondUser || declined.state != 2 || declined.expires != ring.expires {
		t.Fatalf("the peer's device got %+v; want state 2 of call %s from %s with expires %d", declined, call, secondUser, ring.expires)
	}
	if over := secondGW.nextRing(t); over.state != 0 || over.user != peerUser || over.expires != ring.expires {
		t.Fatalf("the second actor's device got %+v after its decline, want state 0", over)
	}

	again := peer.ok("call_start", map[string]any{"actor": "peer", "channel": channel.String()})
	if again["created"] != false || again["call"] != started["call"] {
		t.Fatalf("the second call_start answered %v, want the same call, not created", again)
	}
	secondGW.noRing(t)
	peerGW.noRing(t)
}

// Task 4 review I1: rotate_umk replaces the peer's acting device. The old device forgets every call group it held
// but proposes no Remove there (a Remove another member commits would move the epoch under the replacement's
// external join); its leaf stays in the roster until the replacement rejoins that channel, and that rejoin commits
// the Remove of the old leaf itself. The voice channel (with bob in it) and the DM's call group (the old device
// alone) both end without the old device.
func TestWebDriverRotationLeavesTheCallGroups(t *testing.T) {
	h := testkit.Start(t, testkit.Options{DataDir: t.TempDir(), ProductionACL: true})
	t.Cleanup(h.Stop)
	peer := startWebDriver(t, h, h.BaseURL(), "peer", 0xca11d)
	bob := startWebDriver(t, h, h.BaseURL(), "bob", 0xb0bca11)
	sp := peer.ok("setup", map[string]any{"community": "rotating voice", "channel": "general", "voice": true})
	voice := mustID(t, sp["voice_channel_id"])
	old := sp["device_id"].(string)
	rp := peer.ok("register", nil)
	sb := bob.ok("setup", map[string]any{"community": "bob's own", "channel": "elsewhere"})
	bobDev := sb["device_id"].(string)
	bob.ok("join", map[string]any{"community_id": sp["community_id"], "channel_id": sp["channel_id"],
		"group_id": rp["group_id"], "invite_code": sp["invite_code"]})
	dm := mustID(t, peer.ok("open_dm", map[string]any{"user_id": sb["user_id"]})["channel_id"])

	if j := peer.ok("call_join", map[string]any{"actor": "peer", "channel": voice.String()}); j["leaf"] != float64(0) {
		t.Fatalf("the peer's call_join answered %v, want leaf 0", j)
	}
	if j := bob.ok("call_join", map[string]any{"actor": "peer", "channel": voice.String()}); j["leaf"] != float64(1) {
		t.Fatalf("bob's call_join answered %v, want leaf 1", j)
	}
	waitCallMembers(t, bob, "peer", voice, old, bobDev)
	if j := peer.ok("call_join", map[string]any{"actor": "peer", "channel": dm.String()}); j["leaf"] != float64(0) {
		t.Fatalf("the peer's call_join in the DM answered %v, want leaf 0", j)
	}

	peer.ok("sync", nil) // bob's external join moved the text group's epoch
	peer.ok("rotate_umk", nil)
	// Rotation posts no Remove in the voice call group: bob's call_members (which commits any outstanding
	// proposal) still sees the old leaf next to its own.
	waitCallMembers(t, bob, "peer", voice, old, bobDev)
	// The replacement's rejoin commits the Remove of the old leaf; bob's roster is then bob and the new device.
	if j := peer.ok("call_join", map[string]any{"actor": "peer", "channel": voice.String()}); j["leaf"] != float64(2) {
		t.Fatalf("the replacement's call_join answered %v, want leaf 2 beside the old leaf 0 and bob's leaf 1", j)
	}
	waitRosterWithout(t, bob, voice, old, bobDev)
	// Alone in the DM's call group: the replacement's rejoin takes the old leaf out.
	peer.ok("call_join", map[string]any{"actor": "peer", "channel": dm.String()})
	waitRosterWithout(t, peer, dm, old)
}

// waitRosterWithout polls the driver's call roster of the channel until it is exactly len(keep)+1 devices: every
// device in keep plus one device that is not gone (the replacement, whose id the harness never learns directly).
func waitRosterWithout(t *testing.T, d *webDriver, channel id.ID, gone string, keep ...string) {
	t.Helper()
	deadline := time.Now().Add(driverWait)
	for {
		got := d.ok("call_members", map[string]any{"actor": "peer", "channel": channel.String()})["devices"].([]any)
		others := 0
		kept := 0
		for _, device := range got {
			if device == any(gone) {
				others = -1 << 30
			} else if slices.Contains(keep, device.(string)) {
				kept++
			} else {
				others++
			}
		}
		if len(got) == len(keep)+1 && kept == len(keep) && others == 1 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("after rotate_umk and a rejoin the call roster of %s is %v; want %v and one new device, not %s", channel, got, keep, gone)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// Task 4 fix round N1 (for task 10's forged-list case): forge_device gives the peer a device whose signed-list entry
// does not match its leaf. The instance admits its call_join; a validating client (the peer's trust core, after the
// signed-list refresh a browser runs) sees the leaf fail and blocks the call group.
func TestWebDriverForgedDeviceIsAdmittedButFailsTheSignedListCheck(t *testing.T) {
	h := testkit.Start(t, testkit.Options{DataDir: t.TempDir(), ProductionACL: true})
	t.Cleanup(h.Stop)
	peer := startWebDriver(t, h, h.BaseURL(), "peer", 0xca11e)
	sp := peer.ok("setup", map[string]any{"community": "forged voice", "channel": "general",
		"password": "correct horse battery staple", "voice": true})
	voice := mustID(t, sp["voice_channel_id"])
	peerDev := sp["device_id"].(string)
	peer.ok("call_join", map[string]any{"actor": "peer", "channel": voice.String()})
	if c := peer.ok("call_check", map[string]any{"actor": "peer", "channel": voice.String()}); c["code"] != nil {
		t.Fatalf("call_check of the peer's own call group answered %v, want no block", c)
	}
	forged := peer.ok("forge_device", nil)
	forgedDev := forged["device_id"].(string)
	if dev, err := h.Repo().GetDevice(context.Background(), mustID(t, forgedDev)); err != nil || dev.RevokedAt != nil {
		t.Fatalf("the forged device row = %+v, %v; want a live device of the peer", dev, err)
	}
	if j := peer.ok("call_join", map[string]any{"actor": "second", "channel": voice.String()}); j["leaf"] != float64(1) {
		t.Fatalf("the forged device's call_join answered %v, want leaf 1 (the instance admits it)", j)
	}
	waitCallMembers(t, peer, "peer", voice, peerDev, forgedDev)
	c := peer.ok("call_check", map[string]any{"actor": "peer", "channel": voice.String()})
	if c["code"] != "E_TIER_MISMATCH" {
		t.Fatalf("call_check after the forged join answered %v, want the call group blocked with E_TIER_MISMATCH", c)
	}
	failed := false
	for _, l := range c["leaves"].([]any) {
		leaf := l.(map[string]any)
		failed = failed || (leaf["leaf"] == float64(1) && leaf["outcome"] == "E_TIER_MISMATCH")
	}
	if !failed {
		t.Fatalf("call_check answered %v; want the forged leaf 1 refreshed to E_TIER_MISMATCH", c)
	}
}

// callRing is one op 53 payload: [call_id, channel_id, user_id, state, expires_ts] (L-GW-33).
type callRing struct {
	call, channel, user id.ID
	state, expires      uint64
}

// gatewayReader is a gateway connection of the harness's own, opened as a device the drivers also act as: it keeps
// the op 53 frames that device receives and answers a heartbeat barrier.
type gatewayReader struct {
	name  string
	conn  *websocket.Conn
	rings chan callRing
	acked atomic.Uint64
	sent  atomic.Uint64
	ack   chan struct{}
	done  chan struct{}
	mu    sync.Mutex
	err   error
}

func dialGatewayAs(t *testing.T, h *testkit.Harness, name string, user, device id.ID) *gatewayReader {
	t.Helper()
	ctx := context.Background()
	dev, err := h.Repo().GetDevice(ctx, device)
	if err != nil {
		t.Fatalf("%s's device row: %v", name, err)
	}
	token, err := h.Server().Sessions().NewDeviceSession(ctx, h.Repo(), user, device, dev.Tier)
	if err != nil {
		t.Fatalf("a session for %s's device: %v", name, err)
	}
	dialCtx, cancel := context.WithTimeout(ctx, driverWait)
	defer cancel()
	c, _, err := websocket.Dial(dialCtx, "ws"+strings.TrimPrefix(h.BaseURL(), "http")+"/gateway", //nolint:bodyclose // websocket.Dial documents that the handshake response body never needs closing
		&websocket.DialOptions{
			HTTPHeader:   http.Header{"Authorization": []string{"Bearer " + token.Token}},
			Subprotocols: []string{"dilla.v1"},
		})
	if err != nil {
		t.Fatalf("dial the gateway as %s: %v", name, err)
	}
	c.SetReadLimit(8 << 20)
	g := &gatewayReader{name: name, conn: c, rings: make(chan callRing, 64), ack: make(chan struct{}, 1), done: make(chan struct{})}
	t.Cleanup(func() { _ = c.CloseNow(); <-g.done })
	if op, _ := g.read(dialCtx, t); op != 0 {
		t.Fatalf("%s's first gateway frame is op %d, want hello", name, op)
	}
	// Wire version 2, the one the instance offers (web3.8 S1, dillad.WireVersion).
	g.write(dialCtx, t, []any{uint64(1), uint64(1), nil, []any{"", uint64(2), uint64(1), uint64(1), uint64(0)}})
	for {
		op, _ := g.read(dialCtx, t)
		if op == 3 {
			break
		}
	}
	go g.loop()
	return g
}

func (g *gatewayReader) read(ctx context.Context, t *testing.T) (uint64, []any) {
	t.Helper()
	_, b, err := g.conn.Read(ctx)
	if err != nil {
		t.Fatalf("%s's gateway read: %v", g.name, err)
	}
	var frame []any
	if err := cborx.Unmarshal(b, &frame); err != nil || len(frame) != 4 {
		t.Fatalf("%s's gateway frame %x: %v", g.name, b, err)
	}
	op, _ := frame[0].(uint64)
	payload, _ := frame[3].([]any)
	return op, payload
}

func (g *gatewayReader) write(ctx context.Context, t *testing.T, frame []any) {
	t.Helper()
	b, err := cborx.Marshal(frame)
	if err != nil {
		t.Fatalf("marshal a frame for %s: %v", g.name, err)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.conn.Write(ctx, websocket.MessageBinary, b); err != nil {
		t.Fatalf("%s's gateway write: %v", g.name, err)
	}
}

// loop keeps every op 53 and counts heartbeat acks, in the order the connection delivers them.
func (g *gatewayReader) loop() {
	defer close(g.done)
	for {
		_, b, err := g.conn.Read(context.Background())
		if err != nil {
			g.mu.Lock()
			g.err = err
			g.mu.Unlock()
			return
		}
		var frame []any
		if err := cborx.Unmarshal(b, &frame); err != nil || len(frame) != 4 {
			continue
		}
		payload, _ := frame[3].([]any)
		switch op, _ := frame[0].(uint64); op {
		case 7:
			g.acked.Add(1)
			select {
			case g.ack <- struct{}{}:
			default:
			}
		case 53:
			g.rings <- decodeRing(payload)
		}
	}
}

func decodeRing(p []any) callRing {
	if len(p) != 5 {
		return callRing{}
	}
	asID := func(v any) id.ID {
		var out id.ID
		if b, ok := v.([]byte); ok && len(b) == len(out) {
			copy(out[:], b)
		}
		return out
	}
	state, _ := p[3].(uint64)
	expires, _ := p[4].(uint64)
	return callRing{call: asID(p[0]), channel: asID(p[1]), user: asID(p[2]), state: state, expires: expires}
}

func (g *gatewayReader) nextRing(t *testing.T) callRing {
	t.Helper()
	select {
	case r := <-g.rings:
		return r
	case <-time.After(driverWait):
		g.mu.Lock()
		defer g.mu.Unlock()
		t.Fatalf("%s's device received no op 53 within %v (reader error: %v)", g.name, driverWait, g.err)
	}
	return callRing{}
}

// barrier sends a heartbeat and waits for every ack owed: the gateway writes one connection's frames in order, so
// an op 53 queued for this device before the heartbeat is in rings when it returns.
func (g *gatewayReader) barrier(t *testing.T) {
	t.Helper()
	target := g.sent.Add(1)
	g.write(context.Background(), t, []any{uint64(6), uint64(0), nil, []any{uint64(0), uint64(1)}})
	deadline := time.After(driverWait)
	for g.acked.Load() < target {
		select {
		case <-g.ack:
		case <-deadline:
			t.Fatalf("%s's gateway never acknowledged the heartbeat", g.name)
		}
	}
}

// noRing fails when an op 53 reached the device before a heartbeat barrier sent now.
func (g *gatewayReader) noRing(t *testing.T) {
	t.Helper()
	g.barrier(t)
	select {
	case r := <-g.rings:
		t.Fatalf("%s's device got another op 53: %+v", g.name, r)
	default:
	}
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// webDriver is one `dilla-testkit web-driver` process: one JSON request per stdin line, one answer
// per stdout line, in order.
type webDriver struct {
	t      *testing.T
	name   string
	stdin  io.WriteCloser
	lines  chan string
	stderr *lockedBuffer
	next   int
}

func startWebDriver(t *testing.T, h *testkit.Harness, base, name string, seed uint64) *webDriver {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, os.Getenv("DILLA_TESTKIT"), "web-driver", "--ds", base, "--seed", fmt.Sprint(seed))
	cmd.Env = append(os.Environ(),
		"DILLA_TESTKIT_INVITE="+h.Invite(),
		"DILLA_TESTKIT_CONTROL="+h.ControlURL(),
	)
	stderr := &lockedBuffer{}
	cmd.Stderr = stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", name, err)
	}
	lines := make(chan string, 16)
	go func() {
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 0, 64*1024), 8<<20)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	t.Cleanup(func() {
		defer cancel()
		_ = stdin.Close()
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
		if t.Failed() {
			t.Logf("%s stderr:\n%s", name, stderr.String())
		}
	})
	return &webDriver{t: t, name: name, stdin: stdin, lines: lines, stderr: stderr}
}

// call sends one request and returns its answer, whatever "ok" says.
func (d *webDriver) call(op string, fields map[string]any) map[string]any {
	d.t.Helper()
	d.next++
	req := map[string]any{"id": d.next, "op": op}
	for k, v := range fields {
		req[k] = v
	}
	line, err := json.Marshal(req)
	if err != nil {
		d.t.Fatal(err)
	}
	if _, err := d.stdin.Write(append(line, '\n')); err != nil {
		d.t.Fatalf("%s %s: write: %v", d.name, op, err)
	}
	select {
	case got, open := <-d.lines:
		if !open {
			d.t.Fatalf("%s exited before answering %s:\n%s", d.name, op, d.stderr.String())
		}
		var answer map[string]any
		if err := json.Unmarshal([]byte(got), &answer); err != nil {
			d.t.Fatalf("%s %s: not JSON: %q", d.name, op, got)
		}
		if answer["id"] != float64(d.next) {
			d.t.Fatalf("%s %s: the answer %v carries another id", d.name, op, answer)
		}
		return answer
	case <-time.After(driverWait):
		d.t.Fatalf("%s did not answer %s within %v:\n%s", d.name, op, driverWait, d.stderr.String())
	}
	return nil
}

// ok is call for an op that must succeed.
func (d *webDriver) ok(op string, fields map[string]any) map[string]any {
	d.t.Helper()
	a := d.call(op, fields)
	if a["ok"] != true {
		d.t.Fatalf("%s %s refused: %v", d.name, op, a["error"])
	}
	return a
}

// waitFor syncs until a message with body arrives and returns it.
func (d *webDriver) waitFor(body string) map[string]any {
	d.t.Helper()
	deadline := time.Now().Add(driverWait)
	for {
		for _, item := range d.ok("sync", nil)["received"].([]any) {
			if m := item.(map[string]any); m["body"] == body {
				return m
			}
		}
		if time.Now().After(deadline) {
			d.t.Fatalf("%s never received %q within %v", d.name, body, driverWait)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// relabelProxy relays every request to the instance unchanged, except that once armed it sends
// the next POST /v1/groups/{id}/message with another device's bearer. httputil.ReverseProxy also
// relays the gateway's WebSocket upgrade, so a driver can live entirely behind it.
type relabelProxy struct {
	mu    sync.Mutex
	token string
	srv   *httptest.Server
}

func newRelabelProxy(t *testing.T, target string) *relabelProxy {
	t.Helper()
	u, err := url.Parse(target)
	if err != nil {
		t.Fatal(err)
	}
	relay := httputil.NewSingleHostReverseProxy(u)
	p := &relabelProxy{}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v1/groups/") &&
			strings.HasSuffix(r.URL.Path, "/message") {
			p.mu.Lock()
			token := p.token
			p.token = ""
			p.mu.Unlock()
			if token != "" {
				r.Header.Set("Authorization", "Bearer "+token)
			}
		}
		relay.ServeHTTP(w, r)
	}))
	t.Cleanup(func() {
		p.srv.CloseClientConnections()
		p.srv.Close()
	})
	return p
}

func (p *relabelProxy) arm(token string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.token = token
}

func (p *relabelProxy) armed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.token != ""
}

func mustID(t *testing.T, v any) id.ID {
	t.Helper()
	s, _ := v.(string)
	if !hex32.MatchString(s) {
		t.Fatalf("%v is not 32 lowercase hex", v)
	}
	parsed, err := id.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

func TestTheWebDriverPeersExchangeMessagesUnderTheProductionACL(t *testing.T) {
	h := testkit.Start(t, testkit.Options{DataDir: t.TempDir(), ProductionACL: true})
	t.Cleanup(h.Stop)
	ctx := context.Background()
	repo := h.Repo()

	proxy := newRelabelProxy(t, h.BaseURL())
	alice := startWebDriver(t, h, proxy.srv.URL, "alice", 0xa11ce)
	bob := startWebDriver(t, h, h.BaseURL(), "bob", 0xb0b)

	// setup: an account, a device list, KeyPackages, a community, a channel and an invite, all
	// through the public routes, read back from the instance's own store.
	sa := alice.ok("setup", map[string]any{"community": "web driver", "channel": "general"})
	if sa["username"] != "peer0a11ce" || sa["display"] != "peer0a11ce" {
		t.Fatalf("setup answered username %v display %v, want peer0a11ce twice", sa["username"], sa["display"])
	}
	aliceUser, aliceDev := mustID(t, sa["user_id"]), mustID(t, sa["device_id"])
	community, channel := mustID(t, sa["community_id"]), mustID(t, sa["channel_id"])
	if code, _ := sa["invite_code"].(string); code == "" {
		t.Fatalf("setup answered no invite_code: %v", sa)
	}
	c, err := repo.GetCommunity(ctx, community)
	if err != nil || c.Owner != aliceUser || c.Name != "web driver" {
		t.Fatalf("community row = %+v, %v; want owner %s named \"web driver\"", c, err, aliceUser)
	}
	ch, err := repo.GetChannel(ctx, channel)
	if err != nil || ch.CommunityID == nil || *ch.CommunityID != community || ch.Kind != api.ChannelText ||
		ch.Mode != 0 || ch.Visibility != 0 || ch.Name != "general" {
		t.Fatalf("channel row = %+v, %v; want an e2ee private text channel \"general\" of %s", ch, err, community)
	}
	dev, err := repo.GetDevice(ctx, aliceDev)
	if err != nil || dev.UserID != aliceUser || dev.Tier != 0 {
		t.Fatalf("device row = %+v, %v; want a native-tier device of %s", dev, err, aliceUser)
	}
	if n, err := repo.CountKeyPackages(ctx, aliceDev, time.Now().Unix()); err != nil || n != 32 {
		t.Fatalf("KeyPackages of alice's device = %d, %v; want 32", n, err)
	}

	// register: the channel's text group, under the production registration ACL.
	ra := alice.ok("register", nil)
	group := mustID(t, ra["group_id"])
	if ra["epoch"] != float64(0) {
		t.Fatalf("register answered epoch %v, want 0", ra["epoch"])
	}
	groups, err := repo.GroupsForTarget(ctx, channel, api.GroupText)
	if err != nil || len(groups) != 1 || groups[0].GroupID != group {
		t.Fatalf("open text groups of the channel = %v, %v; want exactly %s", groups, err, group)
	}

	// join: a second peer enters the community with its invite and the group by external commit.
	sb := bob.ok("setup", map[string]any{"community": "bob's own", "channel": "elsewhere"})
	bobUser, bobDev := mustID(t, sb["user_id"]), mustID(t, sb["device_id"])
	jb := bob.ok("join", map[string]any{
		"community_id": sa["community_id"], "channel_id": sa["channel_id"],
		"group_id": ra["group_id"], "invite_code": sa["invite_code"],
	})
	if jb["group_id"] != ra["group_id"] || jb["epoch"] != float64(1) {
		t.Fatalf("join answered %v, want group %v at epoch 1", jb, ra["group_id"])
	}
	if _, err := repo.GetMember(ctx, community, bobUser); err != nil {
		t.Fatalf("bob is not a member of alice's community after join: %v", err)
	}

	// members: alice applies bob's external commit and sees both devices.
	deadline := time.Now().Add(driverWait)
	for {
		devices := alice.ok("members", nil)["devices"].([]any)
		if len(devices) == 2 && slices.Contains(devices, sa["device_id"]) && slices.Contains(devices, sb["device_id"]) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("alice's roster never held both devices: %v", devices)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if s := alice.ok("sync", nil); s["epoch"] != float64(1) || s["members"] != float64(2) {
		t.Fatalf("alice's sync after the join = %v, want epoch 1 with 2 members", s)
	}

	// send and sync, both ways: the sender is the MLS credential's, with its user and tier.
	sent := alice.ok("send", map[string]any{"body": "hello from alice"})
	if seq, _ := sent["seq"].(float64); seq < 1 {
		t.Fatalf("send answered seq %v", sent["seq"])
	}
	got := bob.waitFor("hello from alice")
	if got["seq"] != sent["seq"] || got["sender_user"] != sa["user_id"] || got["sender_device"] != sa["device_id"] || got["tier"] != float64(0) {
		t.Fatalf("bob received %v; want seq %v from alice's user and device at tier 0", got, sent["seq"])
	}
	bob.ok("send", map[string]any{"body": "hello from bob"})
	back := alice.waitFor("hello from bob")
	if back["sender_user"] != sb["user_id"] || back["sender_device"] != sb["device_id"] || back["tier"] != float64(0) {
		t.Fatalf("alice received %v; want bob's user and device at tier 0", back)
	}

	// The relabelled upload: alice's ciphertext reaches the delivery service under bob's session.
	bobRow, err := repo.GetDevice(ctx, bobDev)
	if err != nil {
		t.Fatal(err)
	}
	token, err := h.Server().Sessions().NewDeviceSession(ctx, repo, bobRow.UserID, bobRow.ID, bobRow.Tier)
	if err != nil {
		t.Fatalf("a session for bob's device: %v", err)
	}
	proxy.arm(token.Token)
	relabelled := alice.ok("send", map[string]any{"body": "relabelled in flight"})
	if proxy.armed() {
		t.Fatal("the proxy never saw alice's upload, so nothing was relabelled")
	}
	want := fmt.Sprintf("the MLS sender %s is not the uploader %s", aliceDev, bobDev)
	deadline = time.Now().Add(driverWait)
	for {
		a := bob.call("sync", nil)
		if a["ok"] == false {
			if msg := fmt.Sprint(a["error"]); !strings.Contains(msg, want) {
				t.Fatalf("bob's sync failed for another reason: %s (want it to contain %q)", msg, want)
			}
			break
		}
		if items := a["received"].([]any); len(items) > 0 {
			t.Fatalf("bob accepted a message whose MLS sender (alice) is not its uploader of record (bob): %v", items)
		}
		if time.Now().After(deadline) {
			t.Fatalf("bob never saw the relabelled upload (seq %v) within %v", relabelled["seq"], driverWait)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// waitRoster asks d for its roster until it holds exactly want; with commit set it first runs `sync`,
// which commits any outstanding instance proposal (the Remove a revocation proposes).
func waitRoster(t *testing.T, d *webDriver, commit bool, want ...string) {
	t.Helper()
	deadline := time.Now().Add(driverWait)
	for {
		if commit {
			d.ok("sync", nil)
		}
		got := d.ok("members", nil)["devices"].([]any)
		match := len(got) == len(want)
		for _, w := range want {
			match = match && slices.Contains(got, any(w))
		}
		if match {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s's roster is %v, want exactly %v", d.name, got, want)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// waitForDM runs sync_dm on channel until a message with body arrives and returns it.
func (d *webDriver) waitForDM(channel any, body string) map[string]any {
	d.t.Helper()
	deadline := time.Now().Add(driverWait)
	for {
		for _, item := range d.ok("sync_dm", map[string]any{"channel_id": channel})["received"].([]any) {
			if m := item.(map[string]any); m["body"] == body {
				return m
			}
		}
		if time.Now().After(deadline) {
			d.t.Fatalf("%s never received %q in DM %v within %v", d.name, body, channel, driverWait)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// dilla-web-2a task 9: the peer enrols a second device with its password and its SSK, revokes it, and
// opens a DM that another peer joins by its Welcome, all against the production wiring.
func TestTheWebDriverPeerEnrolsRevokesAndOpensDMs(t *testing.T) {
	h := testkit.Start(t, testkit.Options{DataDir: t.TempDir(), ProductionACL: true})
	t.Cleanup(h.Stop)
	ctx := context.Background()
	repo := h.Repo()
	alice := startWebDriver(t, h, h.BaseURL(), "alice", 0xa11ce2)
	bob := startWebDriver(t, h, h.BaseURL(), "bob", 0xb0b2)

	// setup: a password and three channels, each with its registered text group.
	sa := alice.ok("setup", map[string]any{"community": "enrol", "channel": "general",
		"password": "correct horse battery staple", "channels": 3})
	aliceUser := mustID(t, sa["user_id"])
	ids, _ := sa["channel_ids"].([]any)
	if len(ids) != 3 || ids[0] != sa["channel_id"] {
		t.Fatalf("setup answered channel_ids %v with channel_id %v; want three, the first being channel_id", ids, sa["channel_id"])
	}
	var firstGroup id.ID
	for i, v := range ids {
		ch := mustID(t, v)
		row, err := repo.GetChannel(ctx, ch)
		want := "general"
		if i > 0 {
			want = fmt.Sprintf("general-%d", i+1)
		}
		if err != nil || row.Name != want {
			t.Fatalf("channel %d = %+v, %v; want it named %q", i, row, err, want)
		}
		groups, err := repo.GroupsForTarget(ctx, ch, api.GroupText)
		if err != nil || len(groups) != 1 {
			t.Fatalf("text groups of channel %d = %v, %v; want exactly one, registered by setup", i, groups, err)
		}
		if i == 0 {
			firstGroup = groups[0].GroupID
		}
	}
	if phc, err := repo.GetPasswordCredential(ctx, aliceUser); err != nil || phc == "" {
		t.Fatalf("alice's password credential = %q, %v; want one stored", phc, err)
	}
	ra := alice.ok("register", nil)
	if mustID(t, ra["group_id"]) != firstGroup || ra["epoch"] != float64(0) {
		t.Fatalf("register after setup{channels: 3} answered %v; want the first channel's group %s at epoch 0", ra, firstGroup)
	}

	// A peer set up without a password cannot enrol.
	sb := bob.ok("setup", map[string]any{"community": "bob's own", "channel": "elsewhere"})
	bobUser := mustID(t, sb["user_id"])
	if a := bob.call("enrol", nil); a["ok"] != false || !strings.Contains(fmt.Sprint(a["error"]), "enrol needs setup with a password") {
		t.Fatalf("enrol without a password answered %v", a)
	}

	// bob joins alice's first channel: two leaves.
	bob.ok("join", map[string]any{"community_id": sa["community_id"], "channel_id": sa["channel_id"],
		"group_id": ra["group_id"], "invite_code": sa["invite_code"]})
	waitRoster(t, alice, false, sa["device_id"].(string), sb["device_id"].(string))

	// members takes the channel it addresses: bob joined the first channel only, so channel 3's group
	// holds alice alone (the instance's Add of bob there is a proposal nobody committed; members never
	// commits). A driver that ignored channel_id would answer the first channel's two devices.
	if got := alice.ok("members", map[string]any{"channel_id": ids[2]})["devices"].([]any); len(got) != 1 || got[0] != sa["device_id"] {
		t.Fatalf("members of channel 3: got %d devices, want 1 (%v)", len(got), got)
	}
	if got := alice.ok("members", nil)["devices"].([]any); len(got) != 2 {
		t.Fatalf("members of the first channel: got %d devices, want 2 (%v)", len(got), got)
	}

	// enrol: pending first, enrolled after the PUT, the third leaf in the group.
	en := alice.ok("enrol", nil)
	enrolDev := mustID(t, en["device_id"])
	if en["version"] != float64(2) || fmt.Sprint(en["scopes"]) != "[1 0]" {
		t.Fatalf("enrol answered %v; want version 2 and scopes [1 0] (a pending session, then an enrolled one)", en)
	}
	if epoch, _ := en["epoch"].(float64); epoch < 2 {
		t.Fatalf("enrol answered epoch %v; the external join must land above the two-member epoch", en["epoch"])
	}
	dev, err := repo.GetDevice(ctx, enrolDev)
	if err != nil || dev.UserID != aliceUser || dev.Tier != 1 || dev.SignerTier != 1 || dev.RevokedAt != nil {
		t.Fatalf("the enrolled device row = %+v, %v; want a live browser-tier (1) device of alice", dev, err)
	}
	if list, err := repo.GetDeviceList(ctx, aliceUser); err != nil || list.Version != 2 {
		t.Fatalf("alice's newest device list = %+v, %v; want version 2", list, err)
	}
	if n, err := repo.CountSessionsByDevice(ctx, enrolDev); err != nil || n != 2 {
		t.Fatalf("sessions of the enrolled device = %d, %v; want 2 (the pending one and the enrolled one)", n, err)
	}
	waitRoster(t, alice, false, sa["device_id"].(string), sb["device_id"].(string), en["device_id"].(string))

	// revoke: the row is revoked, its sessions are gone, and its leaf leaves after alice's next commit.
	rv := alice.ok("revoke", map[string]any{"device_id": en["device_id"]})
	if rv["version"] != float64(3) {
		t.Fatalf("revoke answered %v; want version 3", rv)
	}
	if dev, err := repo.GetDevice(ctx, enrolDev); err != nil || dev.RevokedAt == nil {
		t.Fatalf("the revoked device row = %+v, %v; want revoked_at set", dev, err)
	}
	if n, err := repo.CountSessionsByDevice(ctx, enrolDev); err != nil || n != 0 {
		t.Fatalf("sessions of the revoked device = %d, %v; want 0", n, err)
	}
	if list, err := repo.GetDeviceList(ctx, aliceUser); err != nil || list.Version != 3 {
		t.Fatalf("alice's newest device list = %+v, %v; want version 3", list, err)
	}
	waitRoster(t, alice, true, sa["device_id"].(string), sb["device_id"].(string))
	if a := alice.call("revoke", map[string]any{"device_id": en["device_id"]}); a["ok"] != false ||
		!strings.Contains(fmt.Sprint(a["error"]), "device_id is not an unrevoked entry of the device list") {
		t.Fatalf("a second revoke of the same device answered %v", a)
	}

	// DMs: alice opens one and registers its group; bob joins by the Welcome of alice's commit.
	od := alice.ok("open_dm", map[string]any{"user_id": sb["user_id"]})
	dmCh, dmGroup := mustID(t, od["channel_id"]), mustID(t, od["group_id"])
	if dmCh != api.DMChannelID([]id.ID{aliceUser, bobUser}) || od["created"] != true || od["epoch"] != float64(0) {
		t.Fatalf("open_dm answered %v; want the derived 1:1 channel, created, epoch 0", od)
	}
	if ch, err := repo.GetChannel(ctx, dmCh); err != nil || ch.CommunityID != nil || ch.Kind != api.ChannelDM {
		t.Fatalf("the DM channel row = %+v, %v; want kind 3 with no community", ch, err)
	}
	if groups, err := repo.GroupsForTarget(ctx, dmCh, api.GroupText); err != nil || len(groups) != 1 || groups[0].GroupID != dmGroup {
		t.Fatalf("text groups of the DM = %v, %v; want exactly %s", groups, err, dmGroup)
	}
	deadline := time.Now().Add(driverWait)
	for joined := false; !joined; {
		alice.ok("sync_dm", map[string]any{"channel_id": od["channel_id"]})
		for _, item := range bob.ok("dms", nil)["dms"].([]any) {
			m := item.(map[string]any)
			if m["channel_id"] != od["channel_id"] {
				continue
			}
			if m["group_id"] != od["group_id"] {
				t.Fatalf("bob holds DM %v in group %v; want %v", m["channel_id"], m["group_id"], od["group_id"])
			}
			if epoch, _ := m["epoch"].(float64); epoch < 1 {
				t.Fatalf("bob joined the DM at epoch %v; a Welcome lands at 1 or above", m["epoch"])
			}
			joined = true
		}
		if !joined && time.Now().After(deadline) {
			t.Fatalf("bob never joined the DM within %v", driverWait)
		}
		if !joined {
			time.Sleep(100 * time.Millisecond)
		}
	}
	sent := alice.ok("send_dm", map[string]any{"channel_id": od["channel_id"], "body": "dm from alice"})
	got := bob.waitForDM(od["channel_id"], "dm from alice")
	if got["seq"] != sent["seq"] || got["sender_user"] != sa["user_id"] || got["sender_device"] != sa["device_id"] {
		t.Fatalf("bob received %v; want seq %v from alice's user and device", got, sent["seq"])
	}
	bob.ok("send_dm", map[string]any{"channel_id": od["channel_id"], "body": "dm from bob"})
	back := alice.waitForDM(od["channel_id"], "dm from bob")
	if back["sender_user"] != sb["user_id"] || back["sender_device"] != sb["device_id"] {
		t.Fatalf("alice received %v; want bob's user and device", back)
	}
	again := alice.ok("open_dm", map[string]any{"user_id": sb["user_id"]})
	if again["channel_id"] != od["channel_id"] || again["group_id"] != od["group_id"] || again["created"] != false {
		t.Fatalf("a second open_dm answered %v; want the same DM and group, not created", again)
	}
}

// waitAll syncs until every predicate has matched a received row and returns the first match of each, in order.
// Rows a sync answers are consumed, so a test that waits for two rows waits for them together.
func (d *webDriver) waitAll(what string, preds ...func(map[string]any) bool) []map[string]any {
	d.t.Helper()
	found := make([]map[string]any, len(preds))
	deadline := time.Now().Add(driverWait)
	for {
		for _, item := range d.ok("sync", nil)["received"].([]any) {
			m := item.(map[string]any)
			for i, p := range preds {
				if found[i] == nil && p(m) {
					found[i] = m
				}
			}
		}
		done := true
		for _, f := range found {
			done = done && f != nil
		}
		if done {
			return found
		}
		if time.Now().After(deadline) {
			d.t.Fatalf("%s never received %s within %v (matched so far: %v)", d.name, what, driverWait, found)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// seededSHA256 is L-E2E-20's plaintext rule computed independently of the driver: byte i is
// SHA-256(seed BE || (i/32) BE)[i mod 32]; answers the hex SHA-256 of the whole plaintext.
func seededSHA256(seed uint64, size int) string {
	plain := make([]byte, 0, size+sha256.Size)
	for k := uint64(0); len(plain) < size; k++ {
		var in [16]byte
		binary.BigEndian.PutUint64(in[:8], seed)
		binary.BigEndian.PutUint64(in[8:], k)
		block := sha256.Sum256(in[:])
		plain = append(plain, block[:]...)
	}
	sum := sha256.Sum256(plain[:size])
	return hex.EncodeToString(sum[:])
}

// dilla-web-2b task 5: the native peer sends every envelope type, deletes end to end and attaches files against the
// production wiring; bob is the receiving peer whose `sync` the browser tests read (lesson f).
func TestTheWebDriverPeersFoldDeleteAndAttach(t *testing.T) {
	dataDir := t.TempDir()
	h := testkit.Start(t, testkit.Options{DataDir: dataDir, ProductionACL: true})
	t.Cleanup(h.Stop)
	ctx := context.Background()
	repo := h.Repo()
	alice := startWebDriver(t, h, h.BaseURL(), "alice", 0xa11ce5)
	bob := startWebDriver(t, h, h.BaseURL(), "bob", 0xb0b5)

	sa := alice.ok("setup", map[string]any{"community": "say more", "channel": "general"})
	ra := alice.ok("register", nil)
	sb := bob.ok("setup", map[string]any{"community": "bob's own", "channel": "elsewhere"})
	bob.ok("join", map[string]any{"community_id": sa["community_id"], "channel_id": sa["channel_id"],
		"group_id": ra["group_id"], "invite_code": sa["invite_code"]})
	waitRoster(t, alice, false, sa["device_id"].(string), sb["device_id"].(string))
	channel := mustID(t, sa["channel_id"])

	// send: type 0 answers its msg_id; the receiver reads type 0, no reply and no attachments.
	orig := alice.ok("send", map[string]any{"body": "the original"})
	if !hex32.MatchString(fmt.Sprint(orig["msg_id"])) {
		t.Fatalf("send answered msg_id %v, want 32 lowercase hex", orig["msg_id"])
	}
	got := bob.waitFor("the original")
	if got["msg_id"] != orig["msg_id"] || got["type"] != float64(0) || got["reply_to"] != nil ||
		got["deleted"] != false || len(got["attachments"].([]any)) != 0 {
		t.Fatalf("bob received %v; want msg_id %v, type 0, no reply, not deleted, no attachments", got, orig["msg_id"])
	}

	// type 1 with reply_to: the edit reaches the other peer naming its target.
	alice.ok("send", map[string]any{"type": 1, "reply_to": orig["msg_id"], "body": "the edit"})
	edit := bob.waitFor("the edit")
	if edit["type"] != float64(1) || edit["reply_to"] != orig["msg_id"] {
		t.Fatalf("bob received the edit as %v; want type 1 with reply_to %v", edit, orig["msg_id"])
	}
	// The verb refuses an edit without its target before anything is framed.
	if a := alice.call("send", map[string]any{"type": 1, "body": "no target"}); a["ok"] != false ||
		a["error"] != "scenario: reply_to is required" {
		t.Fatalf("an edit without reply_to answered %v", a)
	}

	// attach from bytes: PUT, confirm, then the envelope. The reference is confirmed on the instance.
	small := []byte("hello attachment")
	smallSum := sha256.Sum256(small)
	at := alice.ok("attach", map[string]any{"name": "hello.txt", "mime": "text/plain", "body": "with a file",
		"bytes_hex": hex.EncodeToString(small)})
	if at["sha256"] != hex.EncodeToString(smallSum[:]) {
		t.Fatalf("attach answered sha256 %v, want %x", at["sha256"], smallSum)
	}
	blob1, err := hex.DecodeString(fmt.Sprint(at["blob_id"]))
	if err != nil || len(blob1) != 32 {
		t.Fatalf("attach answered blob_id %v, want 64 lowercase hex", at["blob_id"])
	}
	ref, err := repo.GetBlobRef(ctx, blob1, channel)
	if err != nil || !ref.Confirmed {
		t.Fatalf("the reference of the first attachment = %+v, %v; want it confirmed before the message was sent", ref, err)
	}
	row := bob.waitFor("with a file")
	wantAtt := []any{map[string]any{"index": float64(0), "blob_id": at["blob_id"], "size": float64(len(small)),
		"mime": "text/plain", "name": "hello.txt", "thumb": false}}
	if row["msg_id"] != at["msg_id"] || !reflect.DeepEqual(row["attachments"], wantAtt) {
		t.Fatalf("bob received %v; want msg_id %v and attachments %v", row, at["msg_id"], wantAtt)
	}
	fa := bob.ok("fetch_attachment", map[string]any{"seq": row["seq"], "index": 0})
	if fa["sha256"] != at["sha256"] || fa["size"] != float64(len(small)) || fa["mime"] != "text/plain" ||
		fa["name"] != "hello.txt" || fa["thumb_sha256"] != nil {
		t.Fatalf("bob's fetch_attachment answered %v; want the sender's sha256 %v, size %d, no thumbnail",
			fa, at["sha256"], len(small))
	}

	// attach by size and seed with a thumbnail and no body: the receiver reproduces the plaintext and the thumbnail.
	stub := append([]byte("RIFF\x12\x00\x00\x00WEBP"), make([]byte, 14)...)
	stubSum := sha256.Sum256(stub)
	big := alice.ok("attach", map[string]any{"name": "map.png", "mime": "image/png", "size": 70000, "seed": 7,
		"w": 640, "h": 480, "thumb_hex": hex.EncodeToString(stub)})
	if want := seededSHA256(7, 70000); big["sha256"] != want {
		t.Fatalf("attach{size 70000, seed 7} answered sha256 %v; the plaintext rule gives %s", big["sha256"], want)
	}
	bigRow := bob.waitAll("the seeded attachment", func(m map[string]any) bool { return m["seq"] == big["seq"] })[0]
	atts, _ := bigRow["attachments"].([]any)
	if bigRow["body"] != "" || len(atts) != 1 || atts[0].(map[string]any)["thumb"] != true ||
		atts[0].(map[string]any)["size"] != float64(70000) {
		t.Fatalf("bob received %v; want an attachment-only message with one 70000-byte attachment and a thumbnail", bigRow)
	}
	fb := bob.ok("fetch_attachment", map[string]any{"seq": big["seq"], "index": 0})
	if fb["sha256"] != big["sha256"] || fb["thumb_sha256"] != hex.EncodeToString(stubSum[:]) {
		t.Fatalf("bob's fetch_attachment of the seeded file answered %v; want sha256 %v and thumb_sha256 %x",
			fb, big["sha256"], stubSum)
	}
	// TESTS-01: a 25 MiB file goes through the peer's download (ureq's default body limit is 10 MiB).
	huge := alice.ok("attach", map[string]any{"name": "huge.bin", "mime": "application/octet-stream", "size": 26214400, "seed": 9})
	if want := seededSHA256(9, 26214400); huge["sha256"] != want {
		t.Fatalf("attach{size 26214400, seed 9} answered sha256 %v; the plaintext rule gives %s", huge["sha256"], want)
	}
	bob.waitAll("the 25 MiB attachment", func(m map[string]any) bool { return m["seq"] == huge["seq"] })
	fh := bob.ok("fetch_attachment", map[string]any{"seq": huge["seq"], "index": 0})
	if fh["sha256"] != huge["sha256"] || fh["size"] != float64(26214400) {
		t.Fatalf("bob's fetch_attachment of the 25 MiB file answered %v; want sha256 %v and size 26214400", fh, huge["sha256"])
	}
	if st := bob.ok("blob_status", map[string]any{"blob_id": huge["blob_id"]}); st["status"] != float64(200) {
		t.Fatalf("blob_status of the 25 MiB attachment = %v, want 200", st)
	}

	// delete: type 2, the delivery service's tombstone (op 21) and the reference deletes.
	del := alice.ok("delete", map[string]any{"msg_id": at["msg_id"], "seq": at["seq"]})
	seen := bob.waitAll("the delete and its tombstone",
		func(m map[string]any) bool { return m["seq"] == del["seq"] },
		func(m map[string]any) bool { return m["seq"] == at["seq"] && m["deleted"] == true })
	if seen[0]["type"] != float64(2) || seen[0]["reply_to"] != at["msg_id"] || seen[0]["body"] != "" {
		t.Fatalf("bob received the delete as %v; want type 2 naming %v with an empty body", seen[0], at["msg_id"])
	}
	if seen[1]["body"] != "" || seen[1]["msg_id"] != at["msg_id"] || len(seen[1]["attachments"].([]any)) != 0 {
		t.Fatalf("bob's deleted row is %v; want body \"\", msg_id %v and no attachments", seen[1], at["msg_id"])
	}
	if st := bob.ok("blob_status", map[string]any{"blob_id": at["blob_id"]}); st["status"] != float64(404) {
		t.Fatalf("blob_status of the deleted message's attachment = %v, want 404", st)
	}
	if _, err := repo.GetBlobRef(ctx, blob1, channel); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the reference of the deleted message's attachment: %v, want store.ErrNotFound", err)
	}
	if st := bob.ok("blob_status", map[string]any{"blob_id": big["blob_id"]}); st["status"] != float64(200) {
		t.Fatalf("blob_status of the kept attachment = %v, want 200", st)
	}
	if a := bob.call("fetch_attachment", map[string]any{"seq": at["seq"], "index": 0}); a["ok"] != false ||
		a["error"] != "scenario: the message at seq was deleted" {
		t.Fatalf("fetch_attachment of a deleted message answered %v", a)
	}

	// Bytes replaced on disk under their name: the receiver refuses them by hash before any AEAD.
	name := fmt.Sprint(big["blob_id"])
	path := filepath.Join(dataDir, "blobs", "att", name[0:2], name[2:4], name)
	stored, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the stored blob: %v", err)
	}
	stored[0] ^= 0xff
	if err := os.WriteFile(path, stored, 0o600); err != nil {
		t.Fatalf("rewrite the stored blob: %v", err)
	}
	if a := bob.call("fetch_attachment", map[string]any{"seq": big["seq"], "index": 0}); a["ok"] != false ||
		a["error"] != "scenario: E_BLOB_HASH" {
		t.Fatalf("fetch_attachment of rewritten bytes answered %v, want the error scenario: E_BLOB_HASH", a)
	}
}
