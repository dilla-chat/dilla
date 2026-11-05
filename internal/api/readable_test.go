package api_test

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/auth"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/gateway"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

// recordingFanout is the gateway double: it records every audience the handler
// sets and every frame it delivers. The real channel fan-out is
// internal/gateway's TestDeliverChannelReachesOnlySubscribers.
type recordingFanout struct {
	mu      sync.Mutex
	members map[id.ID][]id.ID
	frames  []deliveredFrame
}

type deliveredFrame struct {
	channel  id.ID
	audience []id.ID
	frame    gateway.Frame
}

func (f *recordingFanout) SetChannelMembers(ch id.ID, users []id.ID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.members == nil {
		f.members = map[id.ID][]id.ID{}
	}
	f.members[ch] = slices.Clone(users)
}

func (f *recordingFanout) DeliverChannel(ch id.ID, fr gateway.Frame) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.frames = append(f.frames, deliveredFrame{channel: ch, audience: slices.Clone(f.members[ch]), frame: fr})
}

func (f *recordingFanout) delivered() []deliveredFrame {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.frames)
}

// readableFixture is the readable routes mounted over a community the owner
// created, with the gateway double and a fixed franking key.
type readableFixture struct {
	e        *env
	cid      id.ID
	ownerTok string
	roles    *api.Roles
	fan      *recordingFanout
	keys     *api.StaticFrankingKeys
}

func newReadableFixture(t *testing.T) *readableFixture {
	t.Helper()
	return newReadableFixtureCap(t, 0)
}

// newReadableFixtureCap is the fixture with the instance's max_ciphertext_bytes set to capBytes
// (0 is the default).
func newReadableFixtureCap(t *testing.T, capBytes int64) *readableFixture {
	t.Helper()
	e, cid, ownerTok := channelEnv(t)
	log := slog.New(slog.DiscardHandler)
	roles := api.NewRoles(e.Repo, e.Clk, "dilla.example", log)
	roles.Register(e.Mux)
	fan := &recordingFanout{}
	keys := api.NewStaticFrankingKeys(api.FrankingKey{ID: id.New(), Key: bytes.Repeat([]byte{0x09}, 32)})
	api.NewReadable(e.Repo, api.NewResolver(e.Repo), fan, keys, e.Clk, log).WithRoles(roles).WithMaxCiphertextBytes(capBytes).Register(e.Mux)
	return &readableFixture{e: e, cid: cid, ownerTok: ownerTok, roles: roles, fan: fan, keys: keys}
}

// readableEnv is the fixture as (env, community, owner token), the shape task 9's
// envelope tests take.
func readableEnv(t *testing.T) (*env, id.ID, string) {
	t.Helper()
	f := newReadableFixture(t)
	return f.e, f.cid, f.ownerTok
}

// readableChannel creates a discoverable (so server-readable) text channel.
func readableChannel(t *testing.T, e *env, cid id.ID, tok string) id.ID {
	t.Helper()
	ch, mode, status := newChannel(t, e, cid, tok, uint64(api.ChannelText), 1, uint64(api.VisDiscoverable),
		"readable-"+id.New().String()[:6])
	if status != http.StatusCreated || mode != uint64(api.ModeReadable) {
		t.Fatalf("create readable channel = %d, mode %d", status, mode)
	}
	return ch
}

// envelope0 is a type-0 envelope with the given body and a fixed 32-byte k_f.
func envelope0(t *testing.T, body string) []byte {
	t.Helper()
	return envelopeOf(t, 0, body)
}

func envelopeOf(t *testing.T, typ uint64, body string) []byte {
	t.Helper()
	// protocol/04 (web-2b task 1): a type 1..6 envelope names its target. PATCH takes the target
	// from the path, so any reference does; seq 1's is used.
	var replyTo any
	if typ != 0 {
		replyTo = seqRef(1)
	}
	b, err := cborx.Marshal([]any{uint64(1), id.New(), typ, nil, replyTo, body, []any{}, []any{}, bytes.Repeat([]byte{0x06}, 32)})
	if err != nil {
		t.Fatalf("cborx.Marshal: %v", err)
	}
	return b
}

func readableReactionEnvelope(t *testing.T, typ uint64, targetSeq uint64, emoji string) []byte {
	t.Helper()
	b, err := cborx.Marshal([]any{uint64(1), id.New(), typ, nil, seqRef(targetSeq), emoji, []any{}, []any{}, bytes.Repeat([]byte{0x06}, 32)})
	if err != nil {
		t.Fatalf("encode reaction envelope: %v", err)
	}
	return b
}

func TestReadableRolePanelReactionUsesAuthenticatedMemberAndExactPanel(t *testing.T) {
	f := newReadableFixture(t)
	e := f.e
	role := createRole(t, e, f.cid, f.ownerTok, "panel member", 2, api.PermViewChannel)
	if status, body := e.Do(http.MethodPut, "/v1/communities/"+f.cid.String()+"/self-roles/policy", f.ownerTok, []any{id.New(), []any{role}}); status != http.StatusOK {
		t.Fatalf("configure self-role allowlist = %d (%x), want 200", status, body)
	}
	channel := readableChannel(t, e, f.cid, f.ownerTok)
	member, memberTok := e.NewUser("panel-reactor")
	joinCommunity(t, e, f.cid, memberTok)
	memberSession := e.sess[memberTok]
	memberSession.WireVersion, memberSession.E2EEVersion, memberSession.MediaVersion = 3, 1, 1
	e.sess[memberTok] = memberSession
	botID, deviceID, installationID := id.New(), id.New(), id.New()
	now := e.Clk.Now().Unix()
	if err := e.Repo.CreateUser(t.Context(), store.UserRow{ID: botID, Username: "panel-bot-" + botID.String()[:8], Display: "Panel Bot", Kind: 1, UMKPub: make([]byte, 32), SSKPub: make([]byte, 32), SigUMKSSK: make([]byte, 64), Created: now}); err != nil {
		t.Fatal(err)
	}
	if err := e.Repo.PutMember(t.Context(), store.MemberOfCommunityRow{CommunityID: f.cid, UserID: botID, Joined: now}); err != nil {
		t.Fatal(err)
	}
	verified := now
	if err := e.Repo.CreateDevice(t.Context(), store.DeviceRow{ID: deviceID, UserID: botID, DSKPub: append([]byte{1}, make([]byte, 31)...), CredentialBlob: []byte{1}, VerifiedAt: &verified, Created: now, LastSeen: now}); err != nil {
		t.Fatal(err)
	}
	if err := e.Repo.CreateBotInstallation(t.Context(), store.BotInstallationRow{ID: installationID, CommunityID: f.cid, UserID: &botID, DeviceID: &deviceID, State: store.BotInstallationReady, Revision: 1, Capabilities: 1 << 2, ApprovedBy: e.sess[f.ownerTok].UserID, ApprovedAt: now, CredentialVerifiedAt: &verified, Created: now, Updated: now}); err != nil {
		t.Fatal(err)
	}
	seq, err := e.Repo.NextChannelSeq(t.Context(), channel)
	if err != nil {
		t.Fatal(err)
	}
	groupID := seedTextGroup(t, e, channel, f.cid)
	requestID, attemptID := id.New(), id.New()
	seedAcceptedRolePanelBotSend(t, e, installationID, botID, deviceID, f.cid, groupID, requestID, attemptID, seq)
	panelPath := "/v1/communities/" + f.cid.String() + "/role-panels"
	if status, body := e.Do(http.MethodPut, panelPath, f.ownerTok, []any{requestID, channel, seq, []any{[]any{"🎨", role}}}); status != http.StatusOK {
		t.Fatalf("register bot panel = %d (%x), want 200", status, body)
	}
	// The explicit encrypted-panel action endpoint must evaluate current channel
	// overwrites, even though the caller remains a community member.
	if err := e.Repo.ReplaceMembers(t.Context(), groupID, 0, []store.MemberRow{{
		GroupID: groupID, LeafIndex: 0, UserID: member, DeviceID: e.sess[memberTok].DeviceID, SignatureKey: make([]byte, 32),
	}}); err != nil {
		t.Fatal(err)
	}
	eventClass := uint8(1)
	if err := e.Repo.PutAppMessage(t.Context(), store.AppMessageRow{GroupID: groupID, Seq: 99,
		UploaderDevice: e.sess[memberTok].DeviceID, Blob: []byte{1}, FrankingTag: make([]byte, 32), Created: now, Class: &eventClass}); err != nil {
		t.Fatal(err)
	}
	ordinaryClass := uint8(0)
	if err := e.Repo.PutAppMessage(t.Context(), store.AppMessageRow{GroupID: groupID, Seq: 98,
		UploaderDevice: e.sess[memberTok].DeviceID, Blob: []byte{1}, FrankingTag: make([]byte, 32), Created: now, Class: &ordinaryClass}); err != nil {
		t.Fatal(err)
	}
	if err := e.Repo.PutOverwrite(t.Context(), store.OverwriteRow{ChannelID: channel, TargetKind: 1, TargetID: member,
		Deny: uint64(api.PermAddReactions)}); err != nil {
		t.Fatal(err)
	}
	reactionAction := "/v1/communities/" + f.cid.String() + "/role-panels/" + channel.String() + "/" + fmt.Sprint(seq) + "/reaction"
	if status, body := e.Do(http.MethodPost, reactionAction, memberTok, []any{groupID, uint64(98), "🎨", uint64(1)}); status != http.StatusForbidden {
		t.Fatalf("ordinary accepted app message as panel reaction evidence = %d (%x), want 403", status, body)
	}
	if status, body := e.Do(http.MethodPost, reactionAction, memberTok, []any{groupID, uint64(99), "🎨", uint64(1)}); status != http.StatusForbidden {
		t.Fatalf("encrypted panel action after channel add_reactions denial = %d (%x), want 403", status, body)
	}
	if held, err := e.Repo.ListMemberRoles(t.Context(), f.cid, member); err != nil || slices.Contains(held, role) {
		t.Fatalf("role after channel overwrite denied panel action = %v, %v; want absent", held, err)
	}
	if err := e.Repo.DeleteOverwrite(t.Context(), channel, 1, member); err != nil {
		t.Fatal(err)
	}
	if status, body := e.Do(http.MethodPost, reactionAction, memberTok, []any{groupID, uint64(99), "🎨", uint64(1)}); status != http.StatusNoContent {
		t.Fatalf("authorized panel action for ordinary member = %d (%x), want 204", status, body)
	}
	if held, err := e.Repo.ListMemberRoles(t.Context(), f.cid, member); err != nil || !slices.Contains(held, role) {
		t.Fatalf("role after approved panel action = %v, %v; want panel role", held, err)
	}
	if status, body := e.Do(http.MethodPost, reactionAction, memberTok, []any{groupID, uint64(99), "🎨", uint64(0)}); status != http.StatusNoContent {
		t.Fatalf("remove panel action for ordinary member = %d (%x), want 204", status, body)
	}
	if held, err := e.Repo.ListMemberRoles(t.Context(), f.cid, member); err != nil || slices.Contains(held, role) {
		t.Fatalf("role after removing panel action = %v, %v; want absent", held, err)
	}
	postReadable(t, e, channel, memberTok, readableReactionEnvelope(t, 3, seq, "🎭"))
	if held, err := e.Repo.ListMemberRoles(t.Context(), f.cid, member); err != nil || slices.Contains(held, role) {
		t.Fatalf("role from unmapped emoji = %v, %v; want absent", held, err)
	}
	postReadable(t, e, channel, memberTok, readableReactionEnvelope(t, 3, seq, "🎨"))
	held, err := e.Repo.ListMemberRoles(t.Context(), f.cid, member)
	if err != nil || !slices.Contains(held, role) {
		t.Fatalf("authenticated reactor role = %v, %v; want panel role", held, err)
	}
	if held, err := e.Repo.ListMemberRoles(t.Context(), f.cid, botID); err != nil || slices.Contains(held, role) {
		t.Fatalf("panel author role = %v, %v; want absent", held, err)
	}
	if err := e.Repo.PutMemberRole(t.Context(), f.cid, member, role); err != nil {
		t.Fatal(err)
	}
	postReadable(t, e, channel, memberTok, readableReactionEnvelope(t, 4, seq, "🎨"))
	held, err = e.Repo.ListMemberRoles(t.Context(), f.cid, member)
	if err != nil || !slices.Contains(held, role) {
		t.Fatalf("role after reaction removal = %v, %v; want manual grant retained", held, err)
	}
	if err := e.Repo.DeleteMemberRole(t.Context(), f.cid, member, role); err != nil {
		t.Fatal(err)
	}
	lookalikeSeq, err := e.Repo.NextChannelSeq(t.Context(), channel)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Repo.PutReadableMessage(t.Context(), store.ReadableMessageRow{ChannelID: channel, ChannelHex: channel.String(), Seq: lookalikeSeq, Sender: member, Envelope: []byte{1}, Body: "Pick a role", FrankingTag: make([]byte, 32), Created: now, UploaderDevice: e.sess[memberTok].DeviceID}); err != nil {
		t.Fatal(err)
	}
	postReadable(t, e, channel, memberTok, readableReactionEnvelope(t, 3, lookalikeSeq, "🎨"))
	held, err = e.Repo.ListMemberRoles(t.Context(), f.cid, member)
	if err != nil || slices.Contains(held, role) {
		t.Fatalf("role from unregistered lookalike = %v, %v; want absent", held, err)
	}
	other, otherTok := e.NewUser("unapproved-panel-reactor")
	otherSession := e.sess[otherTok]
	otherSession.WireVersion, otherSession.E2EEVersion, otherSession.MediaVersion = 3, 1, 1
	e.sess[otherTok] = otherSession
	joinCommunity(t, e, f.cid, otherTok)
	if status, body := e.Do(http.MethodPut, "/v1/communities/"+f.cid.String()+"/self-roles/policy", f.ownerTok, []any{id.New(), []any{}}); status != http.StatusOK {
		t.Fatalf("remove role from allowlist = %d (%x), want 200", status, body)
	}
	postReadable(t, e, channel, otherTok, readableReactionEnvelope(t, 3, seq, "🎨"))
	if held, err := e.Repo.ListMemberRoles(t.Context(), f.cid, other); err != nil || slices.Contains(held, role) {
		t.Fatalf("role from removed allowlist = %v, %v; want absent", held, err)
	}
	if err := e.Repo.AddReactionRoleGrant(t.Context(), store.ReactionRoleGrant{CommunityID: f.cid, UserID: member, RoleID: role, ChannelID: channel, MessageSeq: seq, Reaction: "🎨"}); err != nil {
		t.Fatal(err)
	}
	// Exercise the existing readable-message tombstone path separately after
	// proving registration and reaction work from the accepted MLS receipt alone.
	if _, err := e.Repo.PutReadableMessage(t.Context(), store.ReadableMessageRow{ChannelID: channel, ChannelHex: channel.String(), Seq: seq, Sender: botID, Envelope: []byte{1}, Body: "Pick a role", FrankingTag: make([]byte, 32), Created: now, UploaderDevice: deviceID}); err != nil {
		t.Fatal(err)
	}
	botToken := "panel-bot-delete"
	e.sess[botToken] = auth.Session{UserID: botID, DeviceID: deviceID, WireVersion: 3, E2EEVersion: 1, MediaVersion: 1}
	if status, body := e.Do(http.MethodDelete, fmt.Sprintf("/v1/channels/%s/messages/%d", channel, seq), botToken, nil); status != http.StatusNoContent {
		t.Fatalf("delete bot panel = %d (%x), want 204", status, body)
	}
	if held, err := e.Repo.ListMemberRoles(t.Context(), f.cid, member); err != nil || slices.Contains(held, role) {
		t.Fatalf("role after deleting panel message = %v, %v; want absent", held, err)
	}
	postReadable(t, e, channel, otherTok, readableReactionEnvelope(t, 3, seq, "🎨"))
	if held, err := e.Repo.ListMemberRoles(t.Context(), f.cid, other); err != nil || slices.Contains(held, role) {
		t.Fatalf("role from deleted panel message = %v, %v; want absent", held, err)
	}
}

type postResp struct {
	_           struct{} `cbor:",toarray"`
	Seq         uint64
	FrankingTag []byte
	RecvTS      uint64
}

// postReadable posts an envelope and returns its seq, failing on anything but 200.
func postReadable(t *testing.T, e *env, ch id.ID, tok string, envelope []byte) uint64 {
	t.Helper()
	status, body := e.Do(http.MethodPost, "/v1/channels/"+ch.String()+"/messages", tok, []any{envelope})
	if status != http.StatusOK {
		t.Fatalf("POST message = %d (%x)", status, body)
	}
	var out postResp
	mustUnmarshalBody(t, body, &out)
	return out.Seq
}

func sessionDevice(t *testing.T, e *env, tok string) id.ID {
	t.Helper()
	s, ok := e.sess[tok]
	if !ok {
		t.Fatal("no session for token")
	}
	return s.DeviceID
}

// A readable upload stores the envelope, indexes its body, and franks it with
// protocol/04's real C and T: channel_id for group_id, epoch 0.
func TestPostStoresAndFranksAReadableMessage(t *testing.T) {
	f := newReadableFixture(t)
	e := f.e
	ch := readableChannel(t, e, f.cid, f.ownerTok)
	env := envelope0(t, "hej vad händer med krypteringen")
	status, body := e.Do(http.MethodPost, "/v1/channels/"+ch.String()+"/messages", f.ownerTok, []any{env})
	if status != http.StatusOK {
		t.Fatalf("POST = %d (%x)", status, body)
	}
	var out postResp
	mustUnmarshalBody(t, body, &out)
	now := e.Clk.Now().Unix()
	if out.Seq != 1 || out.RecvTS != uint64(now) {
		t.Fatalf("response = %+v, want seq 1 at %d", out, now)
	}
	c, err := api.Commitment(env, bytes.Repeat([]byte{0x06}, 32))
	if err != nil {
		t.Fatalf("Commitment: %v", err)
	}
	keyID, key := f.keys.Current()
	want := api.Tag(key, ch, 0, 1, sessionDevice(t, e, f.ownerTok), c, now)
	if !bytes.Equal(out.FrankingTag, want) {
		t.Fatalf("franking tag %x, want protocol/04's T over channel_id and epoch 0: %x", out.FrankingTag, want)
	}
	rows, err := e.Repo.ListReadableMessages(t.Context(), ch, 1, 10)
	if err != nil || len(rows) != 1 {
		t.Fatalf("ListReadableMessages = %d, %v", len(rows), err)
	}
	r := rows[0]
	if r.Sender != userOf(t, e, f.ownerTok) || !bytes.Equal(r.Envelope, env) || r.Body != "hej vad händer med krypteringen" ||
		!bytes.Equal(r.FrankingTag, want) || r.FrankingKeyID != keyID || r.ChannelHex != ch.String() || r.Created != now {
		t.Fatalf("stored row = %+v", r)
	}
}

// P2-D28: the envelope routes take 96 KiB, because protocol/04's own worst-case
// legal envelope (~74 KiB) would be refused by the 64 KiB every other route takes.
func TestAMaximalEnvelopeIsAccepted(t *testing.T) {
	f := newReadableFixture(t)
	e := f.e
	ch := readableChannel(t, e, f.cid, f.ownerTok)
	att := []any{bytes.Repeat([]byte{3}, 32), bytes.Repeat([]byte{4}, 32), bytes.Repeat([]byte{5}, 12),
		uint64(1 << 40), strings.Repeat("m", 255), uint64(1 << 20), uint64(1 << 20), bytes.Repeat([]byte{7}, 8192), strings.Repeat("n", 255)}
	prev := []any{strings.Repeat("u", 2048), strings.Repeat("t", 256), strings.Repeat("d", 1024), bytes.Repeat([]byte{8}, 16384)}
	env, err := cborx.Marshal([]any{uint64(1), id.New(), uint64(0), id.New(), id.New(), strings.Repeat("b", 4000),
		[]any{att, att, att, att}, []any{prev, prev}, bytes.Repeat([]byte{6}, 32)})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if len(env) <= 64*1024 {
		t.Fatalf("the maximal envelope is %d bytes; it must exceed the 64 KiB default to test anything", len(env))
	}
	status, body := e.Do(http.MethodPost, "/v1/channels/"+ch.String()+"/messages", f.ownerTok, []any{env})
	if status != http.StatusOK {
		t.Fatalf("a maximal legal envelope (%d bytes) = %d (%x), want 200", len(env), status, body)
	}
}

// maximalTenFileEnvelope is the largest legal type-0 envelope: ten attachments with
// maximal mime, name and thumbnail, two maximal previews and a 4000-byte body.
func maximalTenFileEnvelope(t *testing.T) []byte {
	t.Helper()
	att := []any{bytes.Repeat([]byte{3}, 32), bytes.Repeat([]byte{4}, 32), bytes.Repeat([]byte{5}, 12),
		uint64(1 << 40), strings.Repeat("m", 255), uint64(1 << 20), uint64(1 << 20), bytes.Repeat([]byte{7}, 8192), strings.Repeat("n", 255)}
	prev := []any{strings.Repeat("u", 2048), strings.Repeat("t", 256), strings.Repeat("d", 1024), bytes.Repeat([]byte{8}, 16384)}
	env, err := cborx.Marshal([]any{uint64(1), id.New(), uint64(0), id.New(), id.New(), strings.Repeat("b", 4000),
		[]any{att, att, att, att, att, att, att, att, att, att}, []any{prev, prev}, bytes.Repeat([]byte{6}, 32)})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return env
}

// Wave C: a message carries ten files, so the largest legal envelope is above the
// old 131072-byte cap and must be accepted at the default configuration.
func TestAMaximalTenFileEnvelopeIsAccepted(t *testing.T) {
	f := newReadableFixture(t)
	e := f.e
	ch := readableChannel(t, e, f.cid, f.ownerTok)
	env := maximalTenFileEnvelope(t)
	t.Logf("maximal ten-file envelope: %d bytes", len(env))
	if len(env) <= 131072 {
		t.Fatalf("the maximal envelope is %d bytes; it must exceed the old 131072 cap to test anything", len(env))
	}
	status, body := e.Do(http.MethodPost, "/v1/channels/"+ch.String()+"/messages", f.ownerTok, []any{env})
	if status != http.StatusOK && status != http.StatusCreated {
		t.Fatalf("a maximal ten-file envelope (%d bytes) = %d (%x), want 201", len(env), status, body)
	}
}

// The readable cap follows the instance's ciphertext cap: past it is 413.
func TestAReadableBodyOverTheCapIs413(t *testing.T) {
	f := newReadableFixture(t)
	e := f.e
	ch := readableChannel(t, e, f.cid, f.ownerTok)
	const capBytes = 196608
	over, _ := cborx.Marshal([]any{uint64(1), id.New(), uint64(0), nil, nil, "x", []any{}, []any{}, bytes.Repeat([]byte{6}, 32),
		bytes.Repeat([]byte{0}, capBytes)})
	if len(over) <= capBytes {
		t.Fatalf("test body is %d bytes, want over %d", len(over), capBytes)
	}
	if status, body := e.Do(http.MethodPost, "/v1/channels/"+ch.String()+"/messages", f.ownerTok, []any{over}); status != http.StatusRequestEntityTooLarge {
		t.Fatalf("a %d-byte envelope = %d (%x), want 413", len(over), status, body)
	}
}

// A configured max_ciphertext_bytes moves the readable cap: at 131072 the ten-file maximum is
// refused with 413, and an instance whose cap is just above the envelope's size accepts it, with
// one byte over that cap refused.
func TestTheReadableCapFollowsTheConfiguredCiphertextCap(t *testing.T) {
	env := maximalTenFileEnvelope(t)
	post := func(f *readableFixture, body []byte) int {
		ch := readableChannel(t, f.e, f.cid, f.ownerTok)
		status, _ := f.e.Do(http.MethodPost, "/v1/channels/"+ch.String()+"/messages", f.ownerTok, []any{body})
		return status
	}
	if got := post(newReadableFixtureCap(t, 131072), env); got != http.StatusRequestEntityTooLarge {
		t.Fatalf("a %d-byte envelope at max_ciphertext_bytes 131072 = %d, want 413", len(env), got)
	}
	tight := int64(len(env)) + 8 // the request wraps the envelope in a 6-byte array head and byte-string head
	if got := post(newReadableFixtureCap(t, tight), env); got != http.StatusOK && got != http.StatusCreated {
		t.Fatalf("a %d-byte envelope at cap %d = %d, want 201", len(env), tight, got)
	}
	if got := post(newReadableFixtureCap(t, tight-16), env); got != http.StatusRequestEntityTooLarge {
		t.Fatalf("a %d-byte envelope at cap %d = %d, want 413", len(env), tight-16, got)
	}
}

// A readable message at the cap must still fit the gateway frame that fans it out:
// the frame is the ciphertext cap plus 512 bytes, and the frame's own fields (the
// channel, seq, sender, tag, timestamps) must fit inside those 512.
func TestACapSizedReadableMessageFitsItsFanOutFrame(t *testing.T) {
	f := newReadableFixture(t)
	e := f.e
	ch := readableChannel(t, e, f.cid, f.ownerTok)
	env := maximalTenFileEnvelope(t)
	if status, body := e.Do(http.MethodPost, "/v1/channels/"+ch.String()+"/messages", f.ownerTok, []any{env}); status != http.StatusOK && status != http.StatusCreated {
		t.Fatalf("post = %d (%x)", status, body)
	}
	frames := f.fan.delivered()
	if len(frames) != 1 {
		t.Fatalf("delivered %d frames, want 1", len(frames))
	}
	// The delivered frame has seq 1; re-encode it with the widest numbers (a seq and timestamps
	// that need the 9-byte CBOR head) so this is the largest frame shape the route can produce.
	widest, err := gateway.MessagePlainPayload(ch, 1<<32, userOf(t, e, f.ownerTok), env, bytes.Repeat([]byte{0xAB}, 32), 1<<40, 1<<40)
	if err != nil {
		t.Fatalf("payload: %v", err)
	}
	frame := frames[0].frame
	frame.Payload = widest
	raw, err := gateway.Encode(frame, 1<<40)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	overhead := len(raw) - len(env)
	t.Logf("fan-out frame: %d bytes for a %d-byte envelope, overhead %d", len(raw), len(env), overhead)
	const ciphertextCap, frameBudget = 196608, 196608 + 512
	if overhead > frameBudget-ciphertextCap {
		t.Fatalf("frame overhead %d exceeds the %d bytes between the ciphertext cap and the frame budget; the readable cap needs headroom",
			overhead, frameBudget-ciphertextCap)
	}
}

// An end-to-end encrypted channel's messages go through the delivery service:
// the readable routes answer 403 E_CHANNEL_MODE, never E_MODE_READABLE (which
// means the opposite condition).
func TestAnE2EEChannelRefusesTheReadableRoutes(t *testing.T) {
	f := newReadableFixture(t)
	e := f.e
	ch, _, status := newChannel(t, e, f.cid, f.ownerTok, uint64(api.ChannelText), uint64(api.ModeE2EE), uint64(api.VisPrivate), "secret")
	if status != http.StatusCreated {
		t.Fatalf("create e2ee channel = %d", status)
	}
	for _, req := range []struct {
		method, path string
		body         any
	}{
		{http.MethodPost, "/messages", []any{envelope0(t, "hej")}},
		{http.MethodGet, "/messages?from=1", nil},
		{http.MethodGet, "/search?q=hej", nil},
		{http.MethodPatch, "/messages/1", []any{envelope0(t, "hej")}},
		{http.MethodDelete, "/messages/1", nil},
		{http.MethodPut, "/read-state", []any{uint64(1)}},
	} {
		status, body := e.Do(req.method, "/v1/channels/"+ch.String()+req.path, f.ownerTok, req.body)
		if status != http.StatusForbidden || e.ErrCode(body) != "E_CHANNEL_MODE" {
			t.Fatalf("%s %s = %d %s, want 403 E_CHANNEL_MODE", req.method, req.path, status, e.ErrCode(body))
		}
	}
}

// The permission gates: a non-member learns nothing (404), a member without
// send_messages cannot post (403), and a malformed envelope is E_ENVELOPE_SHAPE.
func TestTheReadableRoutesAreGated(t *testing.T) {
	f := newReadableFixture(t)
	e := f.e
	ch := readableChannel(t, e, f.cid, f.ownerTok)
	_, strangerTok := e.NewUser("stranger")
	status, body := e.Do(http.MethodPost, "/v1/channels/"+ch.String()+"/messages", strangerTok, []any{envelope0(t, "hej")})
	if status != http.StatusNotFound {
		t.Fatalf("a non-member's post = %d (%x), want 404", status, body)
	}
	if status, _ := e.Do(http.MethodGet, "/v1/channels/"+ch.String()+"/messages", strangerTok, nil); status != http.StatusNotFound {
		t.Fatalf("a non-member's read = %d, want 404", status)
	}
	if status, _ := e.Do(http.MethodPost, "/v1/channels/"+ch.String()+"/messages", "", []any{envelope0(t, "hej")}); status != http.StatusUnauthorized {
		t.Fatalf("an unauthenticated post = %d, want 401", status)
	}

	// A member @everyone may view but not send to.
	_, memberTok := e.NewUser("member")
	joinCommunity(t, e, f.cid, memberTok)
	putEveryoneOverwrite(t, e, f.cid, ch, f.ownerTok, 0, uint64(api.PermSendMessages))
	status, body = e.Do(http.MethodPost, "/v1/channels/"+ch.String()+"/messages", memberTok, []any{envelope0(t, "hej")})
	if status != http.StatusForbidden || e.ErrCode(body) != "E_FORBIDDEN" {
		t.Fatalf("a post without send_messages = %d %s, want 403 E_FORBIDDEN", status, e.ErrCode(body))
	}

	for name, env := range map[string][]byte{
		"not an envelope":     {0x01},
		"eight elements":      {0x88, 1, 1, 1, 1, 1, 1, 1, 1},
		"short k_f":           mustMarshal(t, []any{uint64(1), id.New(), uint64(0), nil, nil, "x", []any{}, []any{}, []byte{1}}),
		"body is not a text":  mustMarshal(t, []any{uint64(1), id.New(), uint64(0), nil, nil, []byte("x"), []any{}, []any{}, make([]byte, 32)}),
		"type is not a uint":  mustMarshal(t, []any{uint64(1), id.New(), "0", nil, nil, "x", []any{}, []any{}, make([]byte, 32)}),
		"an unknown type (7)": envelopeOf(t, 7, ""),
	} {
		status, body := e.Do(http.MethodPost, "/v1/channels/"+ch.String()+"/messages", f.ownerTok, []any{env})
		code := e.ErrCode(body)
		if status != http.StatusBadRequest || !strings.HasPrefix(code, "E_ENVELOPE_") {
			t.Fatalf("%s: %d %s, want 400 E_ENVELOPE_*", name, status, code)
		}
	}
	// An edit or a delete on POST is the P2-D15 alias and must name its target's
	// seq in reply_to (task 9); without one it is malformed. envelopeOf names a target for every
	// type 1..6 since web-2b task 1, so the envelope without one is spelled out here.
	for _, typ := range []uint64{1, 2} {
		noTarget := mustMarshal(t, []any{uint64(1), id.New(), typ, nil, nil, "", []any{}, []any{}, bytes.Repeat([]byte{0x06}, 32)})
		status, body := e.Do(http.MethodPost, "/v1/channels/"+ch.String()+"/messages", f.ownerTok, []any{noTarget})
		if status != http.StatusBadRequest || e.ErrCode(body) != "E_ENVELOPE_SHAPE" {
			t.Fatalf("a type-%d envelope on POST with no reply_to = %d %s, want 400 E_ENVELOPE_SHAPE", typ, status, e.ErrCode(body))
		}
	}
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := cborx.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// putEveryoneOverwrite writes an @everyone overwrite on ch.
func putEveryoneOverwrite(t *testing.T, e *env, cid, ch id.ID, ownerTok string, allow, deny uint64) {
	t.Helper()
	roles, err := e.Repo.ListRoles(t.Context(), cid)
	if err != nil {
		t.Fatalf("ListRoles: %v", err)
	}
	var everyone id.ID
	for _, r := range roles {
		if r.Position == 0 {
			everyone = r.ID
		}
	}
	if status, body := e.Do(http.MethodPut, "/v1/channels/"+ch.String()+"/overwrites/0/"+everyone.String(), ownerTok,
		[]any{allow, deny}); status != http.StatusNoContent {
		t.Fatalf("PUT overwrite = %d (%x)", status, body)
	}
}

// message.plain (op 32) reaches the channel's audience — its channel_members
// still in the community, the author included (R30) — as protocol/02's seven
// elements, and a member who left is no longer in it.
func TestAPostFansOutMessagePlainToTheChannelAudience(t *testing.T) {
	f := newReadableFixture(t)
	e := f.e
	member, memberTok := e.NewUser("member")
	joinCommunity(t, e, f.cid, memberTok)
	leaver, leaverTok := e.NewUser("leaver")
	joinCommunity(t, e, f.cid, leaverTok)
	// Created after both joined, so its channel_members already holds them (task 7).
	ch := readableChannel(t, e, f.cid, f.ownerTok)
	if status, _ := e.Do(http.MethodPost, "/v1/communities/"+f.cid.String()+"/leave", leaverTok, []any{}); status != http.StatusNoContent {
		t.Fatalf("leave = %d", status)
	}

	env := envelope0(t, "hello everyone")
	seq := postReadable(t, e, ch, memberTok, env)
	got := f.fan.delivered()
	if len(got) != 1 {
		t.Fatalf("%d frames delivered, want 1", len(got))
	}
	d := got[0]
	owner := userOf(t, e, f.ownerTok)
	if d.channel != ch || !slices.Contains(d.audience, owner) || !slices.Contains(d.audience, member) ||
		slices.Contains(d.audience, leaver) {
		t.Fatalf("delivered to %v on %s; want the owner and the member, not the leaver", d.audience, d.channel)
	}
	if d.frame.Op != gateway.OpMessagePlain || d.frame.GroupID != nil || !d.frame.Replay {
		t.Fatalf("frame = %+v, want replayable op 32 with a null group", d.frame)
	}
	var p []cbor.RawMessage
	mustUnmarshal(t, d.frame.Payload, &p)
	if len(p) != 7 {
		t.Fatalf("message.plain has %d elements, want 7", len(p))
	}
	var gotCh, gotSender id.ID
	var gotSeq, edited, deleted uint64
	var gotEnv, tag []byte
	mustUnmarshal(t, p[0], &gotCh)
	mustUnmarshal(t, p[1], &gotSeq)
	mustUnmarshal(t, p[2], &gotSender)
	mustUnmarshal(t, p[3], &gotEnv)
	mustUnmarshal(t, p[4], &tag)
	mustUnmarshal(t, p[5], &edited)
	mustUnmarshal(t, p[6], &deleted)
	if gotCh != ch || gotSeq != seq || gotSender != member || !bytes.Equal(gotEnv, env) || len(tag) != 32 ||
		edited != 0 || deleted != 0 {
		t.Fatalf("payload = %s/%d/%s/%d-byte envelope/%d-byte tag/%d/%d", gotCh, gotSeq, gotSender, len(gotEnv), len(tag), edited, deleted)
	}
}

// A user who joins AFTER the readable channel exists is in its audience at once:
// the join materialises them into channel_members of every channel they are
// eligible for, so they receive op 32 without waiting for a role, overwrite or
// visibility change to re-derive the channel.
func TestAMemberWhoJoinsAfterTheChannelExistsReceivesMessagePlain(t *testing.T) {
	f := newReadableFixture(t)
	e := f.e
	ch := readableChannel(t, e, f.cid, f.ownerTok)
	// A second readable channel @everyone may not view: the joiner stays out of it.
	hidden := readableChannel(t, e, f.cid, f.ownerTok)
	putEveryoneOverwrite(t, e, f.cid, hidden, f.ownerTok, 0, uint64(api.PermViewChannel))
	joiner, joinerTok := e.NewUser("joiner")
	joinCommunity(t, e, f.cid, joinerTok)

	members, err := e.Repo.ListChannelMembers(t.Context(), ch)
	if err != nil || !slices.Contains(members, joiner) {
		t.Fatalf("channel_members after the join = %v (%v); want the joiner in it", members, err)
	}
	if members, err := e.Repo.ListChannelMembers(t.Context(), hidden); err != nil || slices.Contains(members, joiner) {
		t.Fatalf("channel_members of a channel the joiner may not view = %v (%v); want the joiner out of it", members, err)
	}
	postReadable(t, e, ch, f.ownerTok, envelope0(t, "welcome"))
	got := f.fan.delivered()
	if len(got) != 1 || !slices.Contains(got[0].audience, joiner) {
		t.Fatalf("delivered %d frames, audience %v; want the joiner in it", len(got), got)
	}
	// Joining again is idempotent and leaves the audience as it is.
	joinCommunity(t, e, f.cid, joinerTok)
	postReadable(t, e, ch, joinerTok, envelope0(t, "thanks"))
	got = f.fan.delivered()
	if len(got) != 2 || !slices.Contains(got[1].audience, joiner) || !slices.Contains(got[1].audience, userOf(t, e, f.ownerTok)) {
		t.Fatalf("second delivery audience = %v; want the owner and the joiner", got[len(got)-1].audience)
	}
}

// Slowmode: a member inside the window is 429 with the wait; the owner (every
// permission, bypass_slowmode included) is not held; the window passes.
func TestSlowmodeHoldsAMemberAndNotTheBypass(t *testing.T) {
	f := newReadableFixture(t)
	e := f.e
	ch := readableChannel(t, e, f.cid, f.ownerTok)
	if status, _ := e.Do(http.MethodPatch, "/v1/channels/"+ch.String(), f.ownerTok,
		[]any{nil, nil, nil, nil, nil, nil, uint64(30)}); status != http.StatusNoContent {
		t.Fatalf("PATCH slowmode = %d", status)
	}
	_, memberTok := e.NewUser("member")
	joinCommunity(t, e, f.cid, memberTok)
	postReadable(t, e, ch, memberTok, envelope0(t, "first"))
	e.Clk.Advance(10 * time.Second)
	status, body := e.Do(http.MethodPost, "/v1/channels/"+ch.String()+"/messages", memberTok, []any{envelope0(t, "second")})
	if status != http.StatusTooManyRequests || e.ErrCode(body) != "E_RATE_LIMITED" {
		t.Fatalf("a post inside slowmode = %d %s, want 429 E_RATE_LIMITED", status, e.ErrCode(body))
	}
	var errBody []cbor.RawMessage
	mustUnmarshal(t, body, &errBody)
	var retry uint64
	mustUnmarshal(t, errBody[2], &retry)
	if retry != 20_000 {
		t.Fatalf("retry_after_ms = %d, want 20000", retry)
	}
	postReadable(t, e, ch, f.ownerTok, envelope0(t, "the owner is not held"))
	postReadable(t, e, ch, f.ownerTok, envelope0(t, "nor twice"))
	e.Clk.Advance(20 * time.Second)
	postReadable(t, e, ch, memberTok, envelope0(t, "the window passed"))
}

// callOrderRepo records, in order, the post transaction's two slowmode-relevant
// calls: the channel seq bump and the slowmode read. Tx hands fn a wrapped
// transaction so the calls made inside it are recorded too.
type callOrderRepo struct {
	store.Repository
	mu    *sync.Mutex
	calls *[]string
}

func (r *callOrderRepo) record(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	*r.calls = append(*r.calls, name)
}

func (r *callOrderRepo) Tx(ctx context.Context, fn func(store.Repository) error) error {
	return r.Repository.Tx(ctx, func(tx store.Repository) error {
		return fn(&callOrderRepo{Repository: tx, mu: r.mu, calls: r.calls})
	})
}

func (r *callOrderRepo) NextChannelSeq(ctx context.Context, channelID id.ID) (uint64, error) {
	r.record("NextChannelSeq")
	return r.Repository.NextChannelSeq(ctx, channelID)
}

func (r *callOrderRepo) LastReadableMessageAt(ctx context.Context, channelID, userID id.ID) (int64, error) {
	r.record("LastReadableMessageAt")
	return r.Repository.LastReadableMessageAt(ctx, channelID, userID)
}

// The slowmode read runs AFTER NextChannelSeq inside the post transaction.
// NextChannelSeq is `UPDATE channels … RETURNING seq`, which takes the channel's
// row lock on Postgres; a read before it runs under READ COMMITTED with no lock
// held, so two concurrent posts by one user would both read the old last-post
// time and both pass. After the lock, the second post waits for the first to
// commit and its read (a fresh READ COMMITTED snapshot) sees the first message.
// A refused post rolls the bump back, so it spends no seq.
func TestTheSlowmodeReadFollowsTheChannelRowLock(t *testing.T) {
	e, cid, ownerTok := channelEnv(t)
	log := slog.New(slog.DiscardHandler)
	api.NewRoles(e.Repo, e.Clk, "dilla.example", log).Register(e.Mux)
	rec := &callOrderRepo{Repository: e.Repo, mu: &sync.Mutex{}, calls: &[]string{}}
	keys := api.NewStaticFrankingKeys(api.FrankingKey{ID: id.New(), Key: bytes.Repeat([]byte{0x09}, 32)})
	api.NewReadable(rec, api.NewResolver(e.Repo), nil, keys, e.Clk, log).Register(e.Mux)

	ch := readableChannel(t, e, cid, ownerTok)
	if status, _ := e.Do(http.MethodPatch, "/v1/channels/"+ch.String(), ownerTok,
		[]any{nil, nil, nil, nil, nil, nil, uint64(30)}); status != http.StatusNoContent {
		t.Fatalf("PATCH slowmode = %d", status)
	}
	_, memberTok := e.NewUser("member")
	joinCommunity(t, e, cid, memberTok)

	if seq := postReadable(t, e, ch, memberTok, envelope0(t, "first")); seq != 1 {
		t.Fatalf("first seq = %d, want 1", seq)
	}
	rec.mu.Lock()
	got := slices.Clone(*rec.calls)
	rec.mu.Unlock()
	if want := []string{"NextChannelSeq", "LastReadableMessageAt"}; !slices.Equal(got, want) {
		t.Fatalf("post transaction calls = %v, want %v: the slowmode read must follow the row lock", got, want)
	}

	if status, _ := e.Do(http.MethodPost, "/v1/channels/"+ch.String()+"/messages", memberTok,
		[]any{envelope0(t, "too soon")}); status != http.StatusTooManyRequests {
		t.Fatalf("a post inside slowmode = %d, want 429", status)
	}
	if seq := postReadable(t, e, ch, ownerTok, envelope0(t, "bypass")); seq != 2 {
		t.Fatalf("seq after a refused post = %d, want 2: the refusal must roll its bump back", seq)
	}
}

type listRow struct {
	_           struct{} `cbor:",toarray"`
	Seq         uint64
	Sender      id.ID
	Envelope    []byte
	FrankingTag []byte
	Created     uint64
	Edited      *uint64
	Deleted     *uint64
}

// GET /v1/channels/{id}/messages?from=&limit= pages the channel in seq order,
// deleted messages included as tombstones.
func TestListPagesTheChannel(t *testing.T) {
	f := newReadableFixture(t)
	e := f.e
	ch := readableChannel(t, e, f.cid, f.ownerTok)
	var envs [][]byte
	for i := range 3 {
		env := envelope0(t, fmt.Sprintf("message %d", i))
		envs = append(envs, env)
		postReadable(t, e, ch, f.ownerTok, env)
	}
	status, body := e.Do(http.MethodGet, "/v1/channels/"+ch.String()+"/messages?from=2&limit=5", f.ownerTok, nil)
	if status != http.StatusOK {
		t.Fatalf("GET = %d (%x)", status, body)
	}
	var rows []listRow
	mustUnmarshalBody(t, body, &rows)
	if len(rows) != 2 || rows[0].Seq != 2 || rows[1].Seq != 3 || !bytes.Equal(rows[0].Envelope, envs[1]) ||
		rows[0].Sender != userOf(t, e, f.ownerTok) || len(rows[0].FrankingTag) != 32 || rows[0].Edited != nil ||
		rows[0].Deleted != nil || rows[0].Created != uint64(e.Clk.Now().Unix()) {
		t.Fatalf("rows = %+v", rows)
	}
	if status, body := e.Do(http.MethodGet, "/v1/channels/"+ch.String()+"/messages?limit=1", f.ownerTok, nil); status != http.StatusOK {
		t.Fatalf("GET limit 1 = %d", status)
	} else {
		mustUnmarshalBody(t, body, &rows)
		if len(rows) != 1 || rows[0].Seq != 1 {
			t.Fatalf("from defaults to the first message: %+v", rows)
		}
	}
}

type searchRow struct {
	_           struct{} `cbor:",toarray"`
	ChannelID   id.ID
	Seq         uint64
	Sender      id.ID
	Snippet     string
	ScoreMicros uint64
	Created     uint64
}

// Search covers every readable channel of the community the caller may read,
// and none it may not; an empty query is 400.
func TestSearchCoversTheReadableChannelsTheCallerMayRead(t *testing.T) {
	f := newReadableFixture(t)
	e := f.e
	_, memberTok := e.NewUser("member")
	joinCommunity(t, e, f.cid, memberTok)
	a := readableChannel(t, e, f.cid, f.ownerTok)
	b := readableChannel(t, e, f.cid, f.ownerTok)
	hidden := readableChannel(t, e, f.cid, f.ownerTok)
	putEveryoneOverwrite(t, e, f.cid, hidden, f.ownerTok, 0, uint64(api.PermViewChannel))
	postReadable(t, e, a, f.ownerTok, envelope0(t, "the server never sees plaintext"))
	postReadable(t, e, b, f.ownerTok, envelope0(t, "plaintext is what a readable channel holds"))
	postReadable(t, e, hidden, f.ownerTok, envelope0(t, "plaintext the member may not read"))

	search := func(tok, q string) (int, []searchRow) {
		t.Helper()
		status, body := e.Do(http.MethodGet, "/v1/channels/"+a.String()+"/search?q="+q, tok, nil)
		var rows []searchRow
		if status == http.StatusOK {
			mustUnmarshalBody(t, body, &rows)
		}
		return status, rows
	}
	status, rows := search(memberTok, "plaintext")
	if status != http.StatusOK || len(rows) != 2 {
		t.Fatalf("member search = %d with %d rows, want 200 with 2", status, len(rows))
	}
	for _, r := range rows {
		if r.ChannelID == hidden {
			t.Fatal("a hit leaked from a channel the member may not view")
		}
		if !strings.Contains(r.Snippet, "[plaintext]") || r.ScoreMicros == 0 {
			t.Fatalf("row = %+v, want a marked snippet and a score", r)
		}
	}
	if _, rows := search(f.ownerTok, "plaintext"); len(rows) != 3 {
		t.Fatalf("owner search = %d rows, want 3 (the owner may read every channel)", len(rows))
	}
	if status, _ := search(memberTok, "%20%2B%20"); status != http.StatusBadRequest {
		t.Fatalf("an empty query = %d, want 400", status)
	}
	if _, rows := search(memberTok, "-plaintext"); len(rows) != 0 {
		t.Fatalf("-plaintext = %d rows, want none", len(rows))
	}
}

// PATCH and DELETE on /v1/channels/{id}/messages/{seq}: only the author edits,
// only the author deletes (R29, task 9), and the index follows both.
// I8 (fix wave): Postgres TEXT cannot hold 0x00 (SQLSTATE 22021), and the envelope parser accepts a
// body containing one, so the same POST was 200 on SQLite and 500 on Postgres. The search column
// holds the body with every NUL removed, for a POST and for an edit; the stored envelope (BYTEA, and
// what franking covers) is untouched.
func TestANULNeverReachesTheSearchColumn(t *testing.T) {
	f := newReadableFixture(t)
	e := f.e
	ch := readableChannel(t, e, f.cid, f.ownerTok)
	env := envelope0(t, "a\x00b")
	seq := postReadable(t, e, ch, f.ownerTok, env)
	rows, err := e.Repo.ListReadableMessages(t.Context(), ch, seq, 1)
	if err != nil || len(rows) != 1 {
		t.Fatalf("ListReadableMessages = %+v, %v", rows, err)
	}
	if rows[0].Body != "ab" || !bytes.Equal(rows[0].Envelope, env) {
		t.Fatalf("stored body %q (envelope kept: %v), want \"ab\" and the envelope as sent",
			rows[0].Body, bytes.Equal(rows[0].Envelope, env))
	}
	path := fmt.Sprintf("/v1/channels/%s/messages/%d", ch, seq)
	if status, body := e.Do(http.MethodPatch, path, f.ownerTok, []any{envelopeOf(t, 1, "c\x00\x00d")}); status != http.StatusNoContent {
		t.Fatalf("edit = %d (%x)", status, body)
	}
	rows, _ = e.Repo.ListReadableMessages(t.Context(), ch, seq, 1)
	if rows[0].Body != "cd" {
		t.Fatalf("edited body %q, want \"cd\"", rows[0].Body)
	}
}

func TestEditAndDeleteByPath(t *testing.T) {
	f := newReadableFixture(t)
	e := f.e
	ch := readableChannel(t, e, f.cid, f.ownerTok)
	_, memberTok := e.NewUser("member")
	joinCommunity(t, e, f.cid, memberTok)
	seq := postReadable(t, e, ch, memberTok, envelope0(t, "original words"))
	path := fmt.Sprintf("/v1/channels/%s/messages/%d", ch, seq)

	if status, body := e.Do(http.MethodPatch, path, f.ownerTok, []any{envelopeOf(t, 1, "hijacked")}); status != http.StatusForbidden || e.ErrCode(body) != "E_FORBIDDEN" {
		t.Fatalf("an edit by another user = %d %s, want 403", status, e.ErrCode(body))
	}
	if status, body := e.Do(http.MethodPatch, path, memberTok, []any{envelopeOf(t, 1, "edited words")}); status != http.StatusNoContent {
		t.Fatalf("the author's edit = %d (%x)", status, body)
	}
	rows, _ := e.Repo.ListReadableMessages(t.Context(), ch, seq, 1)
	if rows[0].Body != "edited words" || rows[0].Edited == nil {
		t.Fatalf("after edit = %+v", rows[0])
	}
	frames := f.fan.delivered()
	last := frames[len(frames)-1]
	var p []cbor.RawMessage
	mustUnmarshal(t, last.frame.Payload, &p)
	var edited uint64
	mustUnmarshal(t, p[5], &edited)
	if edited != 1 {
		t.Fatal("the edit's message.plain does not carry edited = 1")
	}

	if status, _ := e.Do(http.MethodPatch, fmt.Sprintf("/v1/channels/%s/messages/99", ch), memberTok, []any{envelopeOf(t, 1, "x")}); status != http.StatusNotFound {
		t.Fatalf("an edit of an unknown seq = %d, want 404", status)
	}
	if status, _ := e.Do(http.MethodPatch, fmt.Sprintf("/v1/channels/%s/messages/x", ch), memberTok, []any{envelopeOf(t, 1, "x")}); status != http.StatusBadRequest {
		t.Fatalf("a malformed seq = %d, want 400", status)
	}

	// Nobody but the uploader deletes it: neither a second member nor the owner,
	// who holds manage_messages (R29); the author can.
	_, otherTok := e.NewUser("other")
	joinCommunity(t, e, f.cid, otherTok)
	for _, tok := range []string{otherTok, f.ownerTok} {
		if status, body := e.Do(http.MethodDelete, path, tok, nil); status != http.StatusForbidden || e.ErrCode(body) != "E_NOT_UPLOADER" {
			t.Fatalf("a delete by a non-author = %d %s, want 403 E_NOT_UPLOADER", status, e.ErrCode(body))
		}
	}
	if status, _ := e.Do(http.MethodDelete, path, memberTok, nil); status != http.StatusNoContent {
		t.Fatalf("the author's delete = %d", status)
	}
	if status, _ := e.Do(http.MethodDelete, path, memberTok, nil); status != http.StatusNotFound {
		t.Fatalf("a second delete = %d, want 404", status)
	}
	rows, _ = e.Repo.ListReadableMessages(t.Context(), ch, seq, 1)
	if rows[0].Deleted == nil || len(rows[0].Envelope) != 0 || len(rows[0].FrankingTag) != 32 {
		t.Fatalf("after delete = %+v", rows[0])
	}
	hits, err := e.Repo.SearchReadable(t.Context(), store.ReadableSearchQuery{
		ChannelIDs: []id.ID{ch}, Query: mustParse(t, "words"), Limit: 10})
	if err != nil || len(hits) != 0 {
		t.Fatalf("a deleted message is still searchable: %d, %v", len(hits), err)
	}
	frames = f.fan.delivered()
	mustUnmarshal(t, frames[len(frames)-1].frame.Payload, &p)
	var deleted uint64
	var env []byte
	mustUnmarshal(t, p[3], &env)
	mustUnmarshal(t, p[6], &deleted)
	if deleted != 1 || len(env) != 0 {
		t.Fatal("the delete's message.plain does not carry deleted = 1 and an empty envelope")
	}
}

func mustParse(t *testing.T, raw string) store.ParsedQuery {
	t.Helper()
	q, err := store.ParseQuery(raw)
	if err != nil {
		t.Fatalf("ParseQuery: %v", err)
	}
	return q
}

// PUT read-state only moves forward and never past the channel's newest seq.
func TestReadStateIsMonotoneAndBounded(t *testing.T) {
	e, cid, ownerTok := readableEnv(t)
	ch := readableChannel(t, e, cid, ownerTok)
	seq := postReadable(t, e, ch, ownerTok, envelope0(t, "hello"))
	put := func(v uint64) {
		t.Helper()
		if status, body := e.Do(http.MethodPut, "/v1/channels/"+ch.String()+"/read-state", ownerTok, []any{v}); status != http.StatusNoContent {
			t.Fatalf("PUT read-state %d = %d (%x)", v, status, body)
		}
	}
	get := func() uint64 {
		t.Helper()
		v, err := e.Repo.GetReadState(t.Context(), userOf(t, e, ownerTok), ch)
		if err != nil {
			t.Fatalf("GetReadState: %v", err)
		}
		return v
	}
	put(seq)
	if got := get(); got != seq {
		t.Fatalf("read state = %d, want %d", got, seq)
	}
	put(0)
	if got := get(); got != seq {
		t.Fatalf("read state moved backwards to %d", got)
	}
	put(1 << 62)
	if got := get(); got != seq {
		t.Fatalf("read state = %d past the channel's newest seq %d", got, seq)
	}
}

func TestReadableTimeoutBlocksEditsReactionsAndPinsButAllowsOwnDeletionAndReads(t *testing.T) {
	f := newReadableFixture(t)
	e := f.e
	ch := readableChannel(t, e, f.cid, f.ownerTok)
	member, memberTok := e.NewUser("timed-out")
	joinCommunity(t, e, f.cid, memberTok)
	role := createRole(t, e, f.cid, f.ownerTok, "readable-moderations", 5, api.PermViewChannel|api.PermSendMessages|api.PermPinMessages|api.PermAddReactions)
	grant(t, e, f.cid, f.ownerTok, member, role, http.StatusNoContent)
	first := postReadable(t, e, ch, memberTok, envelope0(t, "first"))
	second := postReadable(t, e, ch, memberTok, envelope0(t, "second"))
	expires := e.Clk.Now().Unix() + 600
	if err := e.Repo.PutMemberTimeout(t.Context(), store.MemberTimeoutRow{CommunityID: f.cid, UserID: member, ByUser: mustSessionUser(t, e, f.ownerTok), Created: e.Clk.Now().Unix(), Expires: expires}); err != nil {
		t.Fatal(err)
	}

	path := fmt.Sprintf("/v1/channels/%s/messages/%d", ch, first)
	if status, _ := e.Do(http.MethodPatch, path, memberTok, []any{envelopeOf(t, 1, "edit")}); status != http.StatusForbidden {
		t.Fatalf("timed-out readable edit = %d, want 403", status)
	}
	for _, tc := range []struct {
		typ  uint64
		body string
	}{{0, "new message"}, {1, "edit"}, {3, "reaction"}, {4, "reaction"}, {5, ""}, {6, ""}} {
		status, body := e.Do(http.MethodPost, "/v1/channels/"+ch.String()+"/messages", memberTok, []any{envelopeOf(t, tc.typ, tc.body)})
		if status != http.StatusForbidden {
			t.Fatalf("timed-out readable type %d = %d (%x), want 403", tc.typ, status, body)
		}
	}
	if status, _ := e.Do(http.MethodPost, "/v1/channels/"+ch.String()+"/messages", memberTok, []any{envelopeOf(t, 2, "")}); status != http.StatusNoContent {
		t.Fatalf("timed-out author delete alias = %d, want 204", status)
	}
	if status, _ := e.Do(http.MethodDelete, fmt.Sprintf("/v1/channels/%s/messages/%d", ch, second), memberTok, nil); status != http.StatusNoContent {
		t.Fatalf("timed-out author DELETE = %d, want 204", status)
	}
	if status, _ := e.Do(http.MethodGet, "/v1/channels/"+ch.String()+"/messages?from=0", memberTok, nil); status != http.StatusOK {
		t.Fatalf("timed-out read = %d, want 200", status)
	}
}

func mustSessionUser(t *testing.T, e *env, token string) id.ID {
	t.Helper()
	s, ok := e.sess[token]
	if !ok {
		t.Fatal("session not found")
	}
	return s.UserID
}
