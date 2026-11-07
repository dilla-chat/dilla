package api_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"net/http"
	"slices"
	"sync"
	"testing"

	"github.com/fxamacker/cbor/v2"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/auth"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/gateway"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/mlswasi"
	"github.com/dilla-chat/dilla/internal/store"
)

// catalogGateway records the frames the catalog publish delivers, per user, and answers which users
// hold a live connection.
type catalogGateway struct {
	mu        sync.Mutex
	connected map[id.ID]bool
	frames    map[id.ID][]gateway.Frame
}

func (g *catalogGateway) DeliverUser(u id.ID, f gateway.Frame) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.frames[u] = append(g.frames[u], f)
}

func (g *catalogGateway) UserConnected(u id.ID) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.connected[u]
}

func (g *catalogGateway) delivered(u id.ID) []gateway.Frame {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.frames[u])
}

// channelEvents records the Bot-channel notifications in the order they were made.
type channelEvents struct {
	mu     sync.Mutex
	events []string
}

func (e *channelEvents) BotChannelMarked(community, channel id.ID) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.events = append(e.events, "marked "+community.String()+" "+channel.String())
}

func (e *channelEvents) BotChannelUnmarked(community, channel id.ID) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.events = append(e.events, "unmarked "+community.String()+" "+channel.String())
}

func (e *channelEvents) list() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.events)
}

// catalogLists is the device-list verifier the catalog tests run behind: each user's verified list
// is the entries the test gave it, whatever blob is stored.
type catalogLists struct {
	mu      sync.Mutex
	entries map[id.ID][]mlswasi.DeviceListEntry
}

func (*catalogLists) WithVerification(ctx context.Context, fn func(context.Context) error) error {
	return fn(ctx)
}

func (l *catalogLists) Verify(_ context.Context, _, _ []byte, userID id.ID) ([]mlswasi.DeviceListEntry, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.entries[userID]), nil
}

type catalogFixture struct {
	h      http.Handler
	deps   api.Deps
	gw     *catalogGateway
	events *channelEvents
	lists  *catalogLists

	community                     id.ID
	owner, admin, member          store.UserRow
	ownerTok, adminTok, memberTok string
	memberDevice                  store.DeviceRow
	memberKey                     ed25519.PrivateKey

	botUser, botDevice id.ID
	botKey             ed25519.PrivateKey
	botTok             string

	text, voice, category, readable, afk id.ID
}

// seedKeyedSession is seedAPISession that also returns the device's signing key.
func seedKeyedSession(t *testing.T, d api.Deps) (store.UserRow, store.DeviceRow, ed25519.PrivateKey, string) {
	t.Helper()
	ctx := context.Background()
	u, dev, priv := seedAPIDevice(t, d)
	nonce, _, err := d.Sessions.Challenge(ctx, dev.ID)
	if err != nil {
		t.Fatalf("Challenge: %v", err)
	}
	pre := auth.SessionPreimage(d.Sessions.InstanceID(), dev.ID, nonce, auth.PurposeSession)
	tok, err := d.Sessions.Establish(ctx, auth.EstablishRequest{DeviceID: dev.ID, Nonce: nonce, Purpose: auth.PurposeSession, Sig: ed25519.Sign(priv, pre)})
	if err != nil {
		t.Fatalf("Establish: %v", err)
	}
	if err := d.Repo.SetSessionVersions(ctx, auth.TokenHash(tok.Token), 6, 1, 1); err != nil {
		t.Fatal(err)
	}
	return u, dev, priv, tok.Token
}

// newCatalogFixture is one community with an owner, an administrator, a plain member (whose
// @everyone grants no control_music), a ready Bot whose device list names its one device, and a
// text, a voice, a category, a readable text and the AFK channel.
func newCatalogFixture(t *testing.T) *catalogFixture {
	t.Helper()
	f := &catalogFixture{
		gw:     &catalogGateway{connected: map[id.ID]bool{}, frames: map[id.ID][]gateway.Frame{}},
		events: &channelEvents{},
		lists:  &catalogLists{entries: map[id.ID][]mlswasi.DeviceListEntry{}},
	}
	f.h, f.deps = newTestAPIWired(t, func(d *api.Deps) {
		d.DeviceLists = f.lists
		d.BotCatalogGateway = f.gw
		d.BotChannelEvents = f.events
	})
	ctx := context.Background()
	repo := f.deps.Repo
	now := f.deps.Clock.Now().Unix()
	f.owner, _, _, f.ownerTok = seedKeyedSession(t, f.deps)
	f.admin, _, _, f.adminTok = seedKeyedSession(t, f.deps)
	f.member, f.memberDevice, f.memberKey, f.memberTok = seedKeyedSession(t, f.deps)
	f.community = id.New()
	if err := repo.CreateCommunity(ctx, store.CommunityRow{ID: f.community, Owner: f.owner.ID, Name: "catalog", PolicyJSON: []byte(`{}`), PolicyVersion: 1, Created: now}); err != nil {
		t.Fatal(err)
	}
	everyone := store.RoleRow{ID: id.New(), CommunityID: f.community, Name: "@everyone", Position: 0, Created: now,
		Allow: uint64(api.PermViewChannel | api.PermSendMessages | api.PermReadHistory | api.PermConnect | api.PermSpeak)}
	admins := store.RoleRow{ID: id.New(), CommunityID: f.community, Name: "admins", Position: 5, Created: now, Allow: uint64(api.PermAdministrator)}
	for _, role := range []store.RoleRow{everyone, admins} {
		if err := repo.PutRole(ctx, role); err != nil {
			t.Fatal(err)
		}
	}
	for _, u := range []id.ID{f.owner.ID, f.admin.ID, f.member.ID} {
		if err := repo.PutMember(ctx, store.MemberOfCommunityRow{CommunityID: f.community, UserID: u, Joined: now}); err != nil {
			t.Fatal(err)
		}
	}
	if err := repo.PutMemberRole(ctx, f.community, f.admin.ID, admins.ID); err != nil {
		t.Fatal(err)
	}
	cid := f.community
	channel := func(kind, mode uint8, name string) id.ID {
		t.Helper()
		ch := store.ChannelRow{ID: id.New(), CommunityID: &cid, Kind: kind, Mode: mode, Name: name, SettingsJSON: []byte(`{}`), HostPolicyVersion: 1, Created: now}
		if mode == api.ModeReadable {
			ch.Visibility = 2
		}
		if err := repo.CreateChannel(ctx, ch); err != nil {
			t.Fatal(err)
		}
		return ch.ID
	}
	f.text = channel(api.ChannelText, api.ModeE2EE, "bot")
	f.voice = channel(api.ChannelVoice, api.ModeE2EE, "music")
	f.category = channel(api.ChannelCategory, api.ModeE2EE, "cat")
	f.readable = channel(api.ChannelText, api.ModeReadable, "news")
	f.afk = channel(api.ChannelVoice, api.ModeE2EE, "AFK")
	if err := repo.SetCommunityAFK(ctx, f.community, store.CommunityAFK{ChannelID: &f.afk, TimeoutSeconds: 300}); err != nil {
		t.Fatal(err)
	}

	// The ready Bot: a kind-1 user whose one device is its managed device, a bound session and a
	// stored device list (the fake verifier answers its entries).
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	f.botUser, f.botDevice, f.botKey = id.New(), id.New(), priv
	installation, runtime := id.New(), id.New()
	verified := now
	if err := repo.CreateUser(ctx, store.UserRow{ID: f.botUser, Username: "bot" + f.botUser.String()[:8], Display: "Bot", Kind: 1, UMKPub: make([]byte, 32), SSKPub: make([]byte, 32), SigUMKSSK: make([]byte, 64), Created: now}); err != nil {
		t.Fatal(err)
	}
	if err := repo.CreateDevice(ctx, store.DeviceRow{ID: f.botDevice, UserID: f.botUser, DSKPub: pub, CredentialBlob: []byte{1}, VerifiedAt: &verified, Created: now, LastSeen: now}); err != nil {
		t.Fatal(err)
	}
	if err := repo.PutMember(ctx, store.MemberOfCommunityRow{CommunityID: f.community, UserID: f.botUser, Joined: now}); err != nil {
		t.Fatal(err)
	}
	if err := repo.CreateBotRuntime(ctx, store.BotRuntimeRow{ID: runtime, TokenHash: bytes.Repeat([]byte{0x29}, 32), Created: now, Rotated: now}); err != nil {
		t.Fatal(err)
	}
	if err := repo.CreateBotInstallation(ctx, store.BotInstallationRow{ID: installation, CommunityID: f.community, RuntimeID: &runtime, UserID: &f.botUser, DeviceID: &f.botDevice,
		State: store.BotInstallationReady, Revision: 1, ApprovedBy: f.owner.ID, ApprovedAt: now, CredentialVerifiedAt: &verified, Created: now, Updated: now}); err != nil {
		t.Fatal(err)
	}
	f.botTok = "catalog-ready-bot-session-" + installation.String()
	if err := repo.CreateSession(ctx, store.SessionRow{TokenHash: auth.TokenHash(f.botTok), DeviceID: f.botDevice, UserID: f.botUser, Scope: uint8(auth.ScopeEnrolled), Created: now, Expires: now + 3600, IdleExpires: now + 3600}); err != nil {
		t.Fatal(err)
	}
	if err := repo.SetSessionVersions(ctx, auth.TokenHash(f.botTok), 6, 1, 1); err != nil {
		t.Fatal(err)
	}
	if err := repo.CreateBotSessionBinding(ctx, store.BotSessionBindingRow{TokenHash: auth.TokenHash(f.botTok), InstallationID: installation, UserID: f.botUser, DeviceID: f.botDevice, Created: now}); err != nil {
		t.Fatal(err)
	}
	if err := repo.PutDeviceList(ctx, store.DeviceListRow{UserID: f.botUser, Version: 1, Blob: []byte{0x80}, SSKSignature: make([]byte, 64), PrevHash: make([]byte, 32), Created: now}); err != nil {
		t.Fatal(err)
	}
	f.lists.entries[f.botUser] = []mlswasi.DeviceListEntry{{DeviceID: f.botDevice[:], DSKPub: pub, Tier: 0}}
	return f
}

// The catalog's commands, as the Bot publishes them: [name, description, operation, args_shape,
// permission_bit, needs_bot_channel (uint 0|1), handler_kind].
func catalogCommand(name string, operation uint64, perm api.Bits, needsChannel uint64, handlerKind uint64) []any {
	return []any{name, name + " description", operation, []any{[]any{"query", uint64(0)}}, uint64(perm), needsChannel, handlerKind}
}

func (f *catalogFixture) publishCatalog(t *testing.T, commands ...[]any) uint64 {
	t.Helper()
	body := mustCBOR(t, commands)
	rec := putCBORAuth(f.h, "/v1/bot-runtime/communities/"+f.community.String()+"/catalog", body, f.botTok)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT catalog = %d %x, want 200", rec.Code, rec.Body.Bytes())
	}
	var resp []uint64
	if err := cborx.Unmarshal(rec.Body.Bytes(), &resp); err != nil || len(resp) != 1 {
		t.Fatalf("PUT catalog body %x: %v", rec.Body.Bytes(), err)
	}
	return resp[0]
}

type catalogResponse struct {
	Revision    uint64
	Commands    []cbor.RawMessage
	Publication cbor.RawMessage
	Status      []cbor.RawMessage
}

func (f *catalogFixture) getCatalog(t *testing.T, token string) catalogResponse {
	t.Helper()
	rec := getAuth(f.h, "/v1/communities/"+f.community.String()+"/bot/catalog", token)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET catalog = %d %x, want 200", rec.Code, rec.Body.Bytes())
	}
	return decodeCatalog(t, rec.Body.Bytes())
}

func decodeCatalog(t *testing.T, raw []byte) catalogResponse {
	t.Helper()
	var elems []cbor.RawMessage
	if err := cborx.Unmarshal(raw, &elems); err != nil || len(elems) != 4 {
		t.Fatalf("catalog %x: %v (%d elements, want 4)", raw, err, len(elems))
	}
	var out catalogResponse
	if err := cborx.Unmarshal(elems[0], &out.Revision); err != nil {
		t.Fatal(err)
	}
	if err := cborx.Unmarshal(elems[1], &out.Commands); err != nil {
		t.Fatal(err)
	}
	out.Publication = elems[2]
	if err := cborx.Unmarshal(elems[3], &out.Status); err != nil || len(out.Status) != 2 {
		t.Fatalf("status %x: %v", elems[3], err)
	}
	return out
}

func commandNames(t *testing.T, commands []cbor.RawMessage) []string {
	t.Helper()
	names := make([]string, 0, len(commands))
	for _, c := range commands {
		var fields []cbor.RawMessage
		if err := cborx.Unmarshal(c, &fields); err != nil || len(fields) != 7 {
			t.Fatalf("command %x: %v", c, err)
		}
		var name string
		if err := cborx.Unmarshal(fields[0], &name); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	return names
}

func TestCatalogIsFilteredPerMember(t *testing.T) {
	f := newCatalogFixture(t)
	rev := f.publishCatalog(t,
		catalogCommand("play", 11, api.PermControlMusic, 0, 0),
		catalogCommand("remind", 20, 0, 0, 0),
		catalogCommand("ban", 30, api.PermBanMembers, 0, 0),
	)
	if rev != 1 {
		t.Fatalf("first publish revision = %d, want 1", rev)
	}
	member := f.getCatalog(t, f.memberTok)
	if got := commandNames(t, member.Commands); !slices.Equal(got, []string{"remind"}) {
		t.Fatalf("member without control_music sees %v, want only [remind]", got)
	}
	if member.Revision != 1 || !bytes.Equal(member.Publication, []byte{0xf6}) {
		t.Fatalf("member catalog revision %d publication %x, want 1 and null", member.Revision, member.Publication)
	}
	var state uint64
	if err := cborx.Unmarshal(member.Status[0], &state); err != nil || state != 0 {
		t.Fatalf("status state %x: %v, want 0 ready", member.Status[0], err)
	}
	for name, token := range map[string]string{"administrator": f.adminTok, "owner": f.ownerTok} {
		if got := commandNames(t, f.getCatalog(t, token).Commands); !slices.Equal(got, []string{"play", "remind", "ban"}) {
			t.Fatalf("%s sees %v, want every command", name, got)
		}
	}

	// Granting control_music in the community shows /play; a deny overwrite on every voice channel
	// hides it again, since there is nowhere left the member could use it.
	ctx := context.Background()
	djs := store.RoleRow{ID: id.New(), CommunityID: f.community, Name: "dj", Position: 2, Created: f.deps.Clock.Now().Unix(), Allow: uint64(api.PermControlMusic)}
	if err := f.deps.Repo.PutRole(ctx, djs); err != nil {
		t.Fatal(err)
	}
	if err := f.deps.Repo.PutMemberRole(ctx, f.community, f.member.ID, djs.ID); err != nil {
		t.Fatal(err)
	}
	if got := commandNames(t, f.getCatalog(t, f.memberTok).Commands); !slices.Equal(got, []string{"play", "remind"}) {
		t.Fatalf("member with control_music sees %v, want [play remind]", got)
	}
	for _, ch := range []id.ID{f.text, f.voice, f.category, f.readable, f.afk} {
		if err := f.deps.Repo.PutOverwrite(ctx, store.OverwriteRow{ChannelID: ch, TargetKind: 0, TargetID: djs.ID, Deny: uint64(api.PermControlMusic)}); err != nil {
			t.Fatal(err)
		}
	}
	if got := commandNames(t, f.getCatalog(t, f.memberTok).Commands); !slices.Equal(got, []string{"remind"}) {
		t.Fatalf("member denied control_music in every channel sees %v, want [remind]", got)
	}

	// A non-member learns nothing, not even that the community exists.
	_, _, _, outsider := seedKeyedSession(t, f.deps)
	if rec := getAuth(f.h, "/v1/communities/"+f.community.String()+"/bot/catalog", outsider); rec.Code != http.StatusNotFound {
		t.Fatalf("outsider GET catalog = %d, want 404", rec.Code)
	}
}

func TestCatalogPublishPushesOneFramePerMember(t *testing.T) {
	f := newCatalogFixture(t)
	f.gw.connected[f.member.ID] = true
	f.gw.connected[f.admin.ID] = true
	f.gw.connected[f.botUser] = true
	rev := f.publishCatalog(t,
		catalogCommand("play", 11, api.PermControlMusic, 0, 0),
		catalogCommand("remind", 20, 0, 0, 0),
	)
	for user, want := range map[id.ID][]string{f.member.ID: {"remind"}, f.admin.ID: {"play", "remind"}} {
		frames := f.gw.delivered(user)
		if len(frames) != 1 {
			t.Fatalf("user %s got %d frames, want exactly one", user, len(frames))
		}
		fr := frames[0]
		if fr.Op != gateway.OpBotCatalog || uint8(gateway.OpBotCatalog) != 61 || fr.Replay || fr.GroupID != nil {
			t.Fatalf("frame = op %d replay %v group %v, want op 61, not replayable, ungrouped", fr.Op, fr.Replay, fr.GroupID)
		}
		var payload []cbor.RawMessage
		if err := cborx.Unmarshal(fr.Payload, &payload); err != nil || len(payload) != 3 {
			t.Fatalf("payload %x: %v", fr.Payload, err)
		}
		var community id.ID
		var revision uint64
		var commands []cbor.RawMessage
		if err := cborx.Unmarshal(payload[0], &community); err != nil || community != f.community {
			t.Fatalf("payload community %x: %v", payload[0], err)
		}
		if err := cborx.Unmarshal(payload[1], &revision); err != nil || revision != rev {
			t.Fatalf("payload revision %x: %v, want %d", payload[1], err, rev)
		}
		if err := cborx.Unmarshal(payload[2], &commands); err != nil {
			t.Fatal(err)
		}
		if got := commandNames(t, commands); !slices.Equal(got, want) {
			t.Fatalf("user %s pushed %v, want %v", user, got, want)
		}
	}
	if frames := f.gw.delivered(f.owner.ID); len(frames) != 0 {
		t.Fatalf("an unconnected member got %d frames; it reads the catalog at connect sync", len(frames))
	}
	if frames := f.gw.delivered(f.botUser); len(frames) != 0 {
		t.Fatalf("the Bot got %d catalog frames, want none", len(frames))
	}

	// The same commands again change nothing: no new revision, no frame.
	if again := f.publishCatalog(t, catalogCommand("play", 11, api.PermControlMusic, 0, 0), catalogCommand("remind", 20, 0, 0, 0)); again != rev {
		t.Fatalf("an unchanged publish moved the revision to %d, want %d", again, rev)
	}
	if frames := f.gw.delivered(f.member.ID); len(frames) != 1 {
		t.Fatalf("an unchanged publish pushed again (%d frames)", len(frames))
	}
	if next := f.publishCatalog(t, catalogCommand("remind", 20, 0, 0, 0)); next != rev+1 {
		t.Fatalf("a changed publish revision = %d, want %d", next, rev+1)
	}

	// Only the community's ready Bot publishes.
	body := mustCBOR(t, [][]any{catalogCommand("remind", 20, 0, 0, 0)})
	if rec := putCBORAuth(f.h, "/v1/bot-runtime/communities/"+f.community.String()+"/catalog", body, f.adminTok); rec.Code != http.StatusForbidden {
		t.Fatalf("a human PUT catalog = %d, want 403", rec.Code)
	}
}

func TestCatalogHasRoomForHandlerKinds(t *testing.T) {
	f := newCatalogFixture(t)
	odd := []any{"weather", "Ask an outgoing integration", uint64(900), []any{[]any{"city", uint64(0)}, []any{"days", uint64(1)}, nil}, uint64(0), uint64(1), uint64(7)}
	want := mustCBOR(t, odd)
	f.publishCatalog(t, odd)
	got := f.getCatalog(t, f.memberTok)
	if len(got.Commands) != 1 || !bytes.Equal(got.Commands[0], want) {
		t.Fatalf("the handler_kind 7 command came back as %x, want %x unchanged", got.Commands, want)
	}
	for name, cmd := range map[string][]any{
		"six elements":          {"x", "d", uint64(1), []any{}, uint64(0), uint64(0)},
		"unknown permission":    {"x", "d", uint64(1), []any{}, uint64(1) << 40, uint64(0), uint64(0)},
		"needs_bot_channel 2":   {"x", "d", uint64(1), []any{}, uint64(0), uint64(2), uint64(0)},
		"empty name":            {"", "d", uint64(1), []any{}, uint64(0), uint64(0), uint64(0)},
		"name is not text":      {uint64(5), "d", uint64(1), []any{}, uint64(0), uint64(0), uint64(0)},
		"description not text":  {"x", []byte{1}, uint64(1), []any{}, uint64(0), uint64(0), uint64(0)},
		"handler_kind not uint": {"x", "d", uint64(1), []any{}, uint64(0), uint64(0), "k"},
	} {
		rec := putCBORAuth(f.h, "/v1/bot-runtime/communities/"+f.community.String()+"/catalog", mustCBOR(t, []any{cmd}), f.botTok)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: PUT catalog = %d, want 400", name, rec.Code)
		}
	}
	dup := mustCBOR(t, []any{catalogCommand("remind", 20, 0, 0, 0), catalogCommand("remind", 21, 0, 0, 0)})
	if rec := putCBORAuth(f.h, "/v1/bot-runtime/communities/"+f.community.String()+"/catalog", dup, f.botTok); rec.Code != http.StatusBadRequest {
		t.Fatalf("a duplicate command name = %d, want 400", rec.Code)
	}
}

// commandKeyPublication signs [1, community, key_id, public_key, not_after, device] with key, as
// dilla_core::bot::command::CommandKeyPublication::sign does.
func commandKeyPublication(t *testing.T, community, keyID, device id.ID, notAfter uint64, key ed25519.PrivateKey) []byte {
	t.Helper()
	pk := bytes.Repeat([]byte{0x5a}, 32)
	unsigned := mustCBOR(t, []any{uint64(1), community, keyID, pk, notAfter, device})
	sig := ed25519.Sign(key, append([]byte("dilla bot command key v1"), unsigned...))
	return mustCBOR(t, []any{uint64(1), community, keyID, pk, notAfter, device, sig})
}

func TestCommandKeyFromNonBotDeviceIsRefused(t *testing.T) {
	f := newCatalogFixture(t)
	now := uint64(f.deps.Clock.Now().Unix())
	path := "/v1/bot-runtime/communities/" + f.community.String() + "/command-key"
	// A member's own listed device signs a publication for the community: dillad refuses it, because
	// the signer is not a device of the community's Bot user.
	f.lists.entries[f.member.ID] = []mlswasi.DeviceListEntry{{DeviceID: f.memberDevice.ID[:], DSKPub: f.memberDevice.DSKPub}}
	forged := commandKeyPublication(t, f.community, id.New(), f.memberDevice.ID, now+3600, f.memberKey)
	if rec := putCBORAuth(f.h, path, forged, f.botTok); rec.Code != http.StatusForbidden {
		t.Fatalf("a publication signed by a member's device = %d %x, want 403", rec.Code, rec.Body.Bytes())
	}
	// The Bot's device id with someone else's signature.
	wrongSig := commandKeyPublication(t, f.community, id.New(), f.botDevice, now+3600, f.memberKey)
	if rec := putCBORAuth(f.h, path, wrongSig, f.botTok); rec.Code != http.StatusForbidden {
		t.Fatalf("a Bot device id under a member's signature = %d, want 403", rec.Code)
	}
	// A human session cannot publish even a valid publication.
	valid := commandKeyPublication(t, f.community, id.New(), f.botDevice, now+3600, f.botKey)
	if rec := putCBORAuth(f.h, path, valid, f.adminTok); rec.Code != http.StatusForbidden {
		t.Fatalf("a human PUT command-key = %d, want 403", rec.Code)
	}
	// A revoked Bot device no longer signs.
	f.lists.entries[f.botUser][0].Revoked = true
	if rec := putCBORAuth(f.h, path, valid, f.botTok); rec.Code != http.StatusForbidden {
		t.Fatalf("a revoked Bot device's publication = %d, want 403", rec.Code)
	}
	f.lists.entries[f.botUser][0].Revoked = false
	for name, body := range map[string][]byte{
		"another community":       commandKeyPublication(t, id.New(), id.New(), f.botDevice, now+3600, f.botKey),
		"not_after in the past":   commandKeyPublication(t, f.community, id.New(), f.botDevice, now-1, f.botKey),
		"not_after beyond 8 days": commandKeyPublication(t, f.community, id.New(), f.botDevice, now+691200+1, f.botKey),
		"trailing bytes":          append(slices.Clone(valid), 0x00),
		"six elements":            mustCBOR(t, []any{uint64(1), f.community, id.New(), make([]byte, 32), now + 60, f.botDevice}),
	} {
		if rec := putCBORAuth(f.h, path, body, f.botTok); rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: PUT command-key = %d %x, want 400", name, rec.Code, rec.Body.Bytes())
		}
	}
	if got := f.getCatalog(t, f.memberTok); !bytes.Equal(got.Publication, []byte{0xf6}) {
		t.Fatalf("a refused publication was stored: %x", got.Publication)
	}
	// The Bot's own device, exactly 8 days ahead: stored and served with the catalog.
	edge := commandKeyPublication(t, f.community, id.New(), f.botDevice, now+691200, f.botKey)
	if rec := putCBORAuth(f.h, path, edge, f.botTok); rec.Code != http.StatusNoContent {
		t.Fatalf("the Bot's publication = %d %x, want 204", rec.Code, rec.Body.Bytes())
	}
	got := f.getCatalog(t, f.memberTok)
	var served []byte
	if err := cborx.Unmarshal(got.Publication, &served); err != nil || !bytes.Equal(served, edge) {
		t.Fatalf("served publication %x: %v, want the stored bytes", got.Publication, err)
	}
}

func TestBotRevisionsReportVisibleScopes(t *testing.T) {
	f := newCatalogFixture(t)
	ctx := context.Background()
	other := id.New()
	now := f.deps.Clock.Now().Unix()
	if err := f.deps.Repo.CreateCommunity(ctx, store.CommunityRow{ID: other, Owner: f.owner.ID, Name: "elsewhere", PolicyJSON: []byte(`{}`), PolicyVersion: 1, Created: now}); err != nil {
		t.Fatal(err)
	}
	foreign := store.ChannelRow{ID: id.New(), CommunityID: &other, Kind: api.ChannelText, Name: "x", SettingsJSON: []byte(`{}`), HostPolicyVersion: 1, Created: now}
	if err := f.deps.Repo.CreateChannel(ctx, foreign); err != nil {
		t.Fatal(err)
	}
	for _, bump := range []struct {
		kind  uint8
		scope id.ID
	}{{store.BotScopeUser, f.member.ID}, {store.BotScopeUser, f.member.ID}, {store.BotScopeChannel, f.text}, {store.BotScopeCall, f.voice}, {store.BotScopeChannel, foreign.ID}, {store.BotScopeUser, f.admin.ID}} {
		if _, err := f.deps.Repo.BumpBotStateRevision(ctx, bump.kind, bump.scope); err != nil {
			t.Fatal(err)
		}
	}
	catalogRev := f.publishCatalog(t, catalogCommand("remind", 20, 0, 0, 0))
	rec := getAuth(f.h, "/v1/bot/revisions", f.memberTok)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET revisions = %d %x", rec.Code, rec.Body.Bytes())
	}
	var resp []cbor.RawMessage
	if err := cborx.Unmarshal(rec.Body.Bytes(), &resp); err != nil || len(resp) != 2 {
		t.Fatalf("revisions %x: %v", rec.Body.Bytes(), err)
	}
	var user uint64
	if err := cborx.Unmarshal(resp[0], &user); err != nil || user != 2 {
		t.Fatalf("user revision %x: %v, want 2", resp[0], err)
	}
	var scopes [][]cbor.RawMessage
	if err := cborx.Unmarshal(resp[1], &scopes); err != nil {
		t.Fatal(err)
	}
	seen := map[id.ID]uint64{}
	revs := map[id.ID]uint64{}
	for _, s := range scopes {
		var kind, rev uint64
		var scope id.ID
		if len(s) != 3 || cborx.Unmarshal(s[0], &kind) != nil || cborx.Unmarshal(s[1], &scope) != nil || cborx.Unmarshal(s[2], &rev) != nil {
			t.Fatalf("scope row %x", s)
		}
		seen[scope], revs[scope] = kind, rev
	}
	if len(seen) != 3 || seen[f.text] != uint64(store.BotScopeChannel) || seen[f.voice] != uint64(store.BotScopeCall) || seen[f.community] != 3 {
		t.Fatalf("scopes = %v, want the text channel (1), the voice call (2) and the community catalog (3) only", seen)
	}
	if revs[f.community] != catalogRev {
		t.Fatalf("catalog revision = %d, want %d", revs[f.community], catalogRev)
	}
	// A channel the member can no longer view drops out.
	if err := f.deps.Repo.PutOverwrite(ctx, store.OverwriteRow{ChannelID: f.text, TargetKind: 1, TargetID: f.member.ID, Deny: uint64(api.PermViewChannel)}); err != nil {
		t.Fatal(err)
	}
	rec = getAuth(f.h, "/v1/bot/revisions", f.memberTok)
	if err := cborx.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if err := cborx.Unmarshal(resp[1], &scopes); err != nil {
		t.Fatal(err)
	}
	for _, s := range scopes {
		var scope id.ID
		if cborx.Unmarshal(s[1], &scope) == nil && scope == f.text {
			t.Fatal("a channel the member cannot view still reports its revision")
		}
	}
}
