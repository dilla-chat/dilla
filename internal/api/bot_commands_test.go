package api_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
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
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/gateway"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
	"github.com/dilla-chat/dilla/internal/store/sqlite"
	"github.com/dilla-chat/dilla/internal/store/sqlite/sqlitetest"
)

// Bot v2 Task 10: the sealed command relay, the Bot's replies and its state hints.

// relayGateway records what the relay routes deliver, by target.
type relayGateway struct {
	mu         sync.Mutex
	botOnline  bool
	bot        map[id.ID][]gateway.Frame
	device     map[id.ID][]gateway.Frame
	user       map[id.ID][]gateway.Frame
	subscribed map[id.ID][]gateway.Frame
	online     map[id.ID][]gateway.OnlineDevice
}

func newRelayGateway() *relayGateway {
	return &relayGateway{botOnline: true, bot: map[id.ID][]gateway.Frame{}, device: map[id.ID][]gateway.Frame{},
		user: map[id.ID][]gateway.Frame{}, subscribed: map[id.ID][]gateway.Frame{}, online: map[id.ID][]gateway.OnlineDevice{}}
}

func (g *relayGateway) DeliverBot(device id.ID, f gateway.Frame) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.botOnline {
		return false
	}
	g.bot[device] = append(g.bot[device], f)
	return true
}

func (g *relayGateway) DeliverDevice(device id.ID, f gateway.Frame) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.device[device] = append(g.device[device], f)
}

func (g *relayGateway) DeliverUser(user id.ID, f gateway.Frame) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.user[user] = append(g.user[user], f)
}

func (g *relayGateway) DeliverSubscribed(group id.ID, f gateway.Frame) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.subscribed[group] = append(g.subscribed[group], f)
}

func (g *relayGateway) OnlineIn(group id.ID) []gateway.OnlineDevice {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.online[group])
}

func (g *relayGateway) setBotOnline(on bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.botOnline = on
}

func (g *relayGateway) frames(m map[id.ID][]gateway.Frame, k id.ID) []gateway.Frame {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(m[k])
}

// total is every frame the relay delivered, to any target.
func (g *relayGateway) total() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := 0
	for _, m := range []map[id.ID][]gateway.Frame{g.bot, g.device, g.user, g.subscribed} {
		for _, fs := range m {
			n += len(fs)
		}
	}
	return n
}

type relayFixture struct {
	*catalogFixture
	relay  *relayGateway
	path   string
	logs   *lockedBuffer
	disp   *api.SocketDispatcher
	member store.UserRow
	// device2 is a second device of the same member, with its own session.
	device2    store.DeviceRow
	device2Tok string
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) Bytes() []byte {
	l.mu.Lock()
	defer l.mu.Unlock()
	return bytes.Clone(l.b.Bytes())
}

func newRelayFixture(t *testing.T) *relayFixture {
	t.Helper()
	path := filepath.Join(t.TempDir(), "relay.db")
	sqlitetest.MigratedFile(t, path)
	write, err := sqlite.OpenWrite(path)
	if err != nil {
		t.Fatal(err)
	}
	read, err := sqlite.OpenRead(path)
	if err != nil {
		t.Fatal(err)
	}
	repo := sqlite.New(write, read)
	t.Cleanup(func() { repo.Close() })
	r := &relayFixture{relay: newRelayGateway(), path: path, logs: &lockedBuffer{}}
	r.catalogFixture = newCatalogFixtureOn(t, repo, func(d *api.Deps) {
		d.BotRelay = r.relay
		d.Log = slog.New(slog.NewTextHandler(r.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	})
	r.member = r.catalogFixture.member
	r.device2, r.device2Tok = seedDeviceOf(t, r.deps, r.member.ID)
	r.disp = api.NewSocketDispatcher(muxOf(t, r.h), api.SocketDispatchOptions{Clock: r.deps.Clock})
	return r
}

// seedDeviceOf adds a device to an existing user and establishes an enrolled wire-6 session for it.
func seedDeviceOf(t *testing.T, d api.Deps, user id.ID) (store.DeviceRow, string) {
	t.Helper()
	ctx := context.Background()
	now := d.Clock.Now().Unix()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	verified := now
	dev := store.DeviceRow{ID: id.New(), UserID: user, DSKPub: pub, CredentialBlob: []byte{1}, VerifiedAt: &verified, LastSeen: now, Created: now}
	if err := d.Repo.CreateDevice(ctx, dev); err != nil {
		t.Fatal(err)
	}
	nonce, _, err := d.Sessions.Challenge(ctx, dev.ID)
	if err != nil {
		t.Fatal(err)
	}
	pre := auth.SessionPreimage(d.Sessions.InstanceID(), dev.ID, nonce, auth.PurposeSession)
	tok, err := d.Sessions.Establish(ctx, auth.EstablishRequest{DeviceID: dev.ID, Nonce: nonce, Purpose: auth.PurposeSession, Sig: ed25519.Sign(priv, pre)})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Repo.SetSessionVersions(ctx, auth.TokenHash(tok.Token), 6, 1, 1); err != nil {
		t.Fatal(err)
	}
	return dev, tok.Token
}

// sealedCommand is the outer form dillad reads, [1, key_id(16), request_id(16), enc(32),
// ciphertext], around a random ciphertext.
func sealedCommand(t *testing.T, requestID id.ID, ciphertextLen int) []byte {
	t.Helper()
	keyID, enc, ct := make([]byte, 16), make([]byte, 32), make([]byte, ciphertextLen)
	for _, b := range [][]byte{keyID, enc, ct} {
		if _, err := rand.Read(b); err != nil {
			t.Fatal(err)
		}
	}
	return mustCBOR(t, []any{uint64(1), keyID, requestID[:], enc, ct})
}

func (r *relayFixture) commandsPath() string {
	return "/v1/communities/" + r.community.String() + "/bot/commands"
}

func (r *relayFixture) send(t *testing.T, token string, requestID id.ID, sealed []byte) (int, []byte) {
	t.Helper()
	rec := postCBORAuth(r.h, r.commandsPath(), mustCBOR(t, []any{requestID[:], sealed}), token)
	return rec.Code, rec.Body.Bytes()
}

func (r *relayFixture) sendIdem(t *testing.T, token string, device id.ID, requestID id.ID, sealed, idem []byte) (int, []byte) {
	t.Helper()
	sess := auth.Session{TokenHash: auth.TokenHash(token), DeviceID: device}
	return sockettest.DoIdem(t, r.disp, sess, http.MethodPost, r.commandsPath(), mustCBOR(t, []any{requestID[:], sealed}), idem)
}

func (r *relayFixture) reply(t *testing.T, token string, device, requestID id.ID, seq uint64, sealed []byte, final uint64) (int, []byte) {
	t.Helper()
	rec := postCBORAuth(r.h, "/v1/bot-runtime/replies", mustCBOR(t, []any{device[:], requestID[:], seq, sealed, final}), token)
	return rec.Code, rec.Body.Bytes()
}

func (r *relayFixture) hint(t *testing.T, token string, kind uint64, scope id.ID) (int, []byte) {
	t.Helper()
	rec := postCBORAuth(r.h, "/v1/bot-runtime/state-hints", mustCBOR(t, []any{kind, scope[:]}), token)
	return rec.Code, rec.Body.Bytes()
}

func bodyCode(t *testing.T, body []byte) string {
	t.Helper()
	var out []any
	if err := cborx.Unmarshal(body, &out); err != nil || len(out) < 1 {
		t.Fatalf("error body %x: %v", body, err)
	}
	code, _ := out[0].(string)
	return code
}

func wantAccepted(t *testing.T, status int, body []byte) {
	t.Helper()
	if status != http.StatusAccepted || !bytes.Equal(body, mustCBOR(t, []any{uint64(0)})) {
		t.Fatalf("command = %d %x, want 202 [0]", status, body)
	}
}

func framePayload(t *testing.T, f gateway.Frame) []cbor.RawMessage {
	t.Helper()
	var elems []cbor.RawMessage
	if err := cborx.Unmarshal(f.Payload, &elems); err != nil {
		t.Fatalf("payload %x: %v", f.Payload, err)
	}
	return elems
}

func rawBstr(t *testing.T, raw cbor.RawMessage) []byte {
	t.Helper()
	var b []byte
	if err := cborx.Unmarshal(raw, &b); err != nil {
		t.Fatalf("bstr %x: %v", raw, err)
	}
	return b
}

func rawUint(t *testing.T, raw cbor.RawMessage) uint64 {
	t.Helper()
	var n uint64
	if err := cborx.Unmarshal(raw, &n); err != nil {
		t.Fatalf("uint %x: %v", raw, err)
	}
	return n
}

func TestCommandIsRelayedWithAuthenticatedIdentity(t *testing.T) {
	r := newRelayFixture(t)
	requestID := id.New()
	sealed := sealedCommand(t, requestID, 200)
	status, body := r.send(t, r.memberTok, requestID, sealed)
	wantAccepted(t, status, body)

	got := r.relay.frames(r.relay.bot, r.botDevice)
	if len(got) != 1 || got[0].Op != gateway.OpBotCommand || got[0].Replay {
		t.Fatalf("Bot frames = %+v, want one non-replayable bot_command", got)
	}
	p := framePayload(t, got[0])
	if len(p) != 5 {
		t.Fatalf("bot_command has %d elements, want 5", len(p))
	}
	for i, want := range [][]byte{requestID[:], r.community[:], r.member.ID[:], r.memberDevice.ID[:], sealed} {
		if g := rawBstr(t, p[i]); !bytes.Equal(g, want) {
			t.Fatalf("bot_command element %d = %x, want %x", i, g, want)
		}
	}

	// The body names no identity: an extra element is refused, whatever it claims.
	other := id.New()
	rec := postCBORAuth(r.h, r.commandsPath(), mustCBOR(t, []any{other[:], sealedCommand(t, other, 8), r.admin.ID[:]}), r.memberTok)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("a body with an identity element = %d, want 400", rec.Code)
	}
	// The outer request_id must be the body's.
	if status, _ := r.send(t, r.memberTok, id.New(), sealedCommand(t, other, 8)); status != http.StatusBadRequest {
		t.Fatalf("mismatched request_id = %d, want 400", status)
	}
	// Strict outer form: version, lengths, size.
	bad := map[string][]byte{
		"version 2":       mustCBOR(t, []any{uint64(2), make([]byte, 16), other[:], make([]byte, 32), []byte{1}}),
		"short key_id":    mustCBOR(t, []any{uint64(1), make([]byte, 15), other[:], make([]byte, 32), []byte{1}}),
		"short enc":       mustCBOR(t, []any{uint64(1), make([]byte, 16), other[:], make([]byte, 31), []byte{1}}),
		"four elements":   mustCBOR(t, []any{uint64(1), make([]byte, 16), other[:], make([]byte, 32)}),
		"over 16384":      sealedCommand(t, other, 16384),
		"ciphertext text": mustCBOR(t, []any{uint64(1), make([]byte, 16), other[:], make([]byte, 32), "x"}),
	}
	for name, s := range bad {
		if status, _ := r.send(t, r.memberTok, other, s); status != http.StatusBadRequest {
			t.Fatalf("%s = %d, want 400", name, status)
		}
	}
	// A non-member is not told the community exists.
	_, _, _, strangerTok := seedKeyedSession(t, r.deps)
	if status, _ := r.send(t, strangerTok, other, sealedCommand(t, other, 8)); status != http.StatusNotFound {
		t.Fatalf("non-member = %d, want 404", status)
	}
	if n := len(r.relay.frames(r.relay.bot, r.botDevice)); n != 1 {
		t.Fatalf("Bot frames after refusals = %d, want 1", n)
	}
}

// The core's vector sealed command passes the outer decode untouched.
func TestRelayAcceptsTheCoreVectorSealedCommand(t *testing.T) {
	r := newRelayFixture(t)
	sealed, err := hex.DecodeString("850150333333333333333333333333333333335044444444444444444444444444444444582041852320ff367495fa522c94cb83af391e4e89018392725bbf2098dd931bc424590101011dd955bb1b48604fbaf0d5daad32d4b5dfbfceaaed74a8cdc17f1c2046d305a9217c6c0472c6f6127ed68c8ce4bf5aa1b0262f5e64bd3aa7742c9220cb1437120481d654388b280d012736ee6e158877fb70e93c118b3eef9ad5a479210cf36b21713779a34f050ab5f0695c3f0752f5b71d526954e3cda4e1483efc5f9bf2be4fe494cc0cafd13845722e648bd72e519bfcaf5c1ed6cc59a6297eec316a035d4a10ce73f430af1c2e259276cd97f43c0f7b381cb48158a0b9e50ab402cf25e03fec673638cd675ec88e198f3b1c4ed140c54c370f67b3d205d5d055f1c7d0c1fd4876b15e614154a8ffe3e6d11d8c8e36344eab56fb2400067cad1642e9a8f1")
	if err != nil {
		t.Fatal(err)
	}
	var requestID id.ID
	copy(requestID[:], bytes.Repeat([]byte{0x44}, 16))
	status, body := r.send(t, r.memberTok, requestID, sealed)
	wantAccepted(t, status, body)
}

func TestDillaStoresNoCommandContent(t *testing.T) {
	r := newRelayFixture(t)
	requestID := id.New()
	sealed := sealedCommand(t, requestID, 4096)
	status, body := r.send(t, r.memberTok, requestID, sealed)
	wantAccepted(t, status, body)
	if status, _ := r.reply(t, r.botTok, r.memberDevice.ID, requestID, 0, bytes.Repeat([]byte{0x77}, 64), 1); status != http.StatusNoContent {
		t.Fatalf("reply = %d, want 204", status)
	}

	write, err := sqlite.OpenWrite(r.path)
	if err != nil {
		t.Fatal(err)
	}
	defer write.Close()
	out := filepath.Join(t.TempDir(), "dump.db")
	if err := store.VacuumInto(context.Background(), write, out); err != nil {
		t.Fatal(err)
	}
	dump, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	logs := r.logs.Bytes()
	for name, haystack := range map[string][]byte{"store dump": dump, "logs": logs} {
		if bytes.Contains(haystack, sealed) {
			t.Fatalf("the %s holds the sealed command", name)
		}
		for i := 0; i+32 <= len(sealed); i++ {
			if bytes.Contains(haystack, sealed[i:i+32]) {
				t.Fatalf("the %s holds the sealed command's 32-byte window at %d", name, i)
			}
			if bytes.Contains(haystack, []byte(hex.EncodeToString(sealed[i:i+32]))) {
				t.Fatalf("the %s holds the sealed command's window at %d in hex", name, i)
			}
		}
	}
}

func TestReplyGoesOnlyToTheRequestingDevice(t *testing.T) {
	r := newRelayFixture(t)
	requestID := id.New()
	status, body := r.send(t, r.memberTok, requestID, sealedCommand(t, requestID, 64))
	wantAccepted(t, status, body)

	// The member's second device was not the requester: a reply addressed to it is refused.
	if status, _ := r.reply(t, r.botTok, r.device2.ID, requestID, 0, []byte{0xaa}, 0); status != http.StatusNotFound {
		t.Fatalf("reply to the other device = %d, want 404", status)
	}
	sealedReply := bytes.Repeat([]byte{0x31}, 300)
	for seq, final := range []uint64{0, 1} {
		if status, body := r.reply(t, r.botTok, r.memberDevice.ID, requestID, uint64(seq), sealedReply, final); status != http.StatusNoContent {
			t.Fatalf("reply %d = %d %x, want 204", seq, status, body)
		}
	}
	got := r.relay.frames(r.relay.device, r.memberDevice.ID)
	if len(got) != 2 {
		t.Fatalf("requesting device got %d frames, want 2", len(got))
	}
	for i, f := range got {
		if f.Op != gateway.OpBotReply || !f.Replay {
			t.Fatalf("frame %d = op %d replay %v, want a replayable bot_reply", i, f.Op, f.Replay)
		}
		p := framePayload(t, f)
		if len(p) != 4 || !bytes.Equal(rawBstr(t, p[0]), requestID[:]) || rawUint(t, p[1]) != uint64(i) ||
			!bytes.Equal(rawBstr(t, p[2]), sealedReply) || rawUint(t, p[3]) != uint64(i) {
			t.Fatalf("bot_reply %d payload %x", i, f.Payload)
		}
	}
	if n := len(r.relay.frames(r.relay.device, r.device2.ID)); n != 0 {
		t.Fatalf("the second device got %d frames, want 0", n)
	}
	if n := len(r.relay.frames(r.relay.user, r.member.ID)); n != 0 {
		t.Fatalf("a reply went to the whole user (%d frames)", n)
	}
	// Only the Bot's runtime session may reply.
	if status, _ := r.reply(t, r.memberTok, r.memberDevice.ID, requestID, 2, []byte{1}, 1); status != http.StatusForbidden {
		t.Fatalf("a member's reply = %d, want 403", status)
	}
	// A reply is at most 65536 sealed bytes.
	if status, _ := r.reply(t, r.botTok, r.memberDevice.ID, requestID, 2, make([]byte, 65537), 1); status != http.StatusBadRequest && status != http.StatusRequestEntityTooLarge {
		t.Fatalf("an oversized reply = %d, want 400 or 413", status)
	}
}

func TestReplyToUnknownRequestIs404(t *testing.T) {
	r := newRelayFixture(t)
	if status, body := r.reply(t, r.botTok, r.memberDevice.ID, id.New(), 0, []byte{1}, 1); status != http.StatusNotFound || bodyCode(t, body) != "E_NOT_FOUND" {
		t.Fatalf("reply to an unknown request = %d, want 404", status)
	}
	requestID := id.New()
	status, body := r.send(t, r.memberTok, requestID, sealedCommand(t, requestID, 16))
	wantAccepted(t, status, body)
	clk, ok := r.deps.Clock.(*clock.Fake)
	if !ok {
		t.Fatalf("the harness clock is %T", r.deps.Clock)
	}
	clk.Advance(5*time.Minute + time.Second)
	if status, _ := r.reply(t, r.botTok, r.memberDevice.ID, requestID, 0, []byte{1}, 1); status != http.StatusNotFound {
		t.Fatalf("reply after five minutes = %d, want 404", status)
	}
	if n := len(r.relay.frames(r.relay.device, r.memberDevice.ID)); n != 0 {
		t.Fatalf("refused replies delivered %d frames", n)
	}
}

func TestDuplicateRequestIDFromAnotherDeviceIsRefused(t *testing.T) {
	r := newRelayFixture(t)
	requestID := id.New()
	sealed := sealedCommand(t, requestID, 32)
	idem := bytes.Repeat([]byte{0x11}, 16)
	status, body := r.sendIdem(t, r.memberTok, r.memberDevice.ID, requestID, sealed, idem)
	wantAccepted(t, status, body)

	// Another device, even of the same user, reusing the request_id is refused.
	status, body = r.send(t, r.device2Tok, requestID, sealed)
	if status != http.StatusConflict || bodyCode(t, body) != "E_BOT_DUPLICATE" {
		t.Fatalf("another device's duplicate = %d %x, want 409 E_BOT_DUPLICATE", status, body)
	}
	// A socket retry of the same request (same idem) is answered from the replay record: no new push.
	status, body = r.sendIdem(t, r.memberTok, r.memberDevice.ID, requestID, sealed, idem)
	wantAccepted(t, status, body)
	if n := len(r.relay.frames(r.relay.bot, r.botDevice)); n != 1 {
		t.Fatalf("Bot frames after an idem retry = %d, want 1", n)
	}
	// A re-send (same request_id, new idem) from the same device is accepted again and re-pushed.
	status, body = r.sendIdem(t, r.memberTok, r.memberDevice.ID, requestID, sealed, bytes.Repeat([]byte{0x12}, 16))
	wantAccepted(t, status, body)
	if n := len(r.relay.frames(r.relay.bot, r.botDevice)); n != 2 {
		t.Fatalf("Bot frames after a re-send = %d, want 2", n)
	}
	// After five minutes the entry is gone and the id is free again.
	clk, ok := r.deps.Clock.(*clock.Fake)
	if !ok {
		t.Fatalf("the harness clock is %T", r.deps.Clock)
	}
	clk.Advance(5*time.Minute + time.Second)
	status, body = r.send(t, r.device2Tok, requestID, sealed)
	wantAccepted(t, status, body)
}

func TestRelayWithNoBotIsOffline(t *testing.T) {
	r := newRelayFixture(t)
	r.relay.setBotOnline(false)
	requestID := id.New()
	sealed := sealedCommand(t, requestID, 16)
	status, body := r.send(t, r.memberTok, requestID, sealed)
	if status != http.StatusServiceUnavailable || bodyCode(t, body) != "E_BOT_OFFLINE" {
		t.Fatalf("command with no Bot connection = %d %x, want 503 E_BOT_OFFLINE", status, body)
	}
	// Nothing was recorded for the refused command: another device may use the id once the Bot is back.
	r.relay.setBotOnline(true)
	status, body = r.send(t, r.device2Tok, requestID, sealed)
	wantAccepted(t, status, body)
	if status, _ := r.reply(t, r.botTok, r.memberDevice.ID, requestID, 0, []byte{1}, 1); status != http.StatusNotFound {
		t.Fatalf("a reply to the refused command's device = %d, want 404", status)
	}

	// A disabled installation refuses with 403.
	ctx := context.Background()
	row, err := r.deps.Repo.GetActiveBotInstallationByCommunity(ctx, r.community)
	if err != nil {
		t.Fatal(err)
	}
	prev := row.Revision
	row.State = store.BotInstallationDisabled
	row.Revision++
	row.Updated++
	if err := r.deps.Repo.Tx(ctx, func(tx store.Repository) error {
		return tx.UpdateBotInstallation(ctx, row.ID, prev, row)
	}); err != nil {
		t.Fatal(err)
	}
	next := id.New()
	if status, _ := r.send(t, r.memberTok, next, sealedCommand(t, next, 16)); status != http.StatusForbidden {
		t.Fatalf("command to a disabled Bot = %d, want 403", status)
	}
}

func TestRelayIsRateLimitedPerDevice(t *testing.T) {
	r := newRelayFixture(t)
	limited := false
	for i := 0; i < 40 && !limited; i++ {
		requestID := id.New()
		status, _ := r.send(t, r.memberTok, requestID, sealedCommand(t, requestID, 8))
		switch status {
		case http.StatusAccepted:
		case http.StatusTooManyRequests:
			limited = true
		default:
			t.Fatalf("command %d = %d", i, status)
		}
	}
	if !limited {
		t.Fatal("40 commands at once from one device were never rate limited")
	}
	// The other device keeps its own allowance.
	requestID := id.New()
	status, body := r.send(t, r.device2Tok, requestID, sealedCommand(t, requestID, 8))
	wantAccepted(t, status, body)
}

// The Bot answers one request at most BotRepliesPerRequest times: the next reply is 413 and is not
// delivered (fix round 1, M4).
func TestReplyPastTheCapIs413(t *testing.T) {
	r := newRelayFixture(t)
	requestID := id.New()
	status, body := r.send(t, r.memberTok, requestID, sealedCommand(t, requestID, 16))
	wantAccepted(t, status, body)
	for seq := range api.BotRepliesPerRequest {
		if status, body := r.reply(t, r.botTok, r.memberDevice.ID, requestID, uint64(seq), []byte{1}, 0); status != http.StatusNoContent {
			t.Fatalf("reply %d = %d %x", seq, status, body)
		}
	}
	status, body = r.reply(t, r.botTok, r.memberDevice.ID, requestID, uint64(api.BotRepliesPerRequest), []byte{1}, 1)
	if status != http.StatusRequestEntityTooLarge || bodyCode(t, body) != "E_TOO_LARGE" {
		t.Fatalf("reply past the cap = %d %x, want 413 E_TOO_LARGE", status, body)
	}
	if n := len(r.relay.frames(r.relay.device, r.memberDevice.ID)); n != api.BotRepliesPerRequest {
		t.Fatalf("delivered %d replies, want %d", n, api.BotRepliesPerRequest)
	}
}

// An account that still owes a second factor cannot send a sealed command (fix round 1, M7).
func TestRelayRefusesAnAccountThatOwesASecondFactor(t *testing.T) {
	r := newRelayFixture(t)
	owe(t, r.deps, r.member.ID)
	requestID := id.New()
	status, body := r.send(t, r.memberTok, requestID, sealedCommand(t, requestID, 16))
	if status != http.StatusForbidden || bodyCode(t, body) != "E_FACTOR_REQUIRED" {
		t.Fatalf("command from an owed account = %d %x, want 403 E_FACTOR_REQUIRED", status, body)
	}
	if n := r.relay.total(); n != 0 {
		t.Fatalf("an owed account's command reached the Bot (%d frames)", n)
	}
}

func TestHintCarriesNoContentAndReachesOnlyScope(t *testing.T) {
	r := newRelayFixture(t)
	ctx := context.Background()
	now := r.deps.Clock.Now().Unix()
	cid := r.community
	textGroup := store.GroupRow{GroupID: id.New(), Binding: []byte{0x80}, Kind: api.GroupText, CommunityID: &cid, TargetID: r.text,
		Ciphersuite: 1, Epoch: 1, ExternalSenderKeyID: id.New(), E2EEVersion: 1, MediaVersion: 1, PolicyVersion: 1, Created: now}
	callGroup := store.GroupRow{GroupID: id.New(), Binding: []byte{0x80}, Kind: api.GroupCall, CommunityID: &cid, TargetID: r.voice,
		Ciphersuite: 1, Epoch: 1, ExternalSenderKeyID: id.New(), E2EEVersion: 1, MediaVersion: 1, PolicyVersion: 1, Created: now}
	for _, g := range []store.GroupRow{textGroup, callGroup} {
		if err := r.deps.Repo.CreateGroup(ctx, g); err != nil {
			t.Fatal(err)
		}
	}
	call := id.New()
	if err := r.deps.Repo.PutVoiceSession(ctx, store.VoiceSessionRow{CallID: call, ChannelID: r.voice, GroupID: &callGroup.GroupID,
		LivekitRoom: call.String() + "-1-abcdef01", Started: now}); err != nil {
		t.Fatal(err)
	}
	r.relay.mu.Lock()
	r.relay.online[callGroup.GroupID] = []gateway.OnlineDevice{{DeviceID: r.memberDevice.ID}}
	r.relay.mu.Unlock()
	// The text channel is a Bot channel; a second text channel is not.
	if _, err := r.deps.Repo.MarkBotChannel(ctx, store.BotChannelRow{ChannelID: r.text, CommunityID: cid, MarkedAt: now, MarkedBy: r.owner.ID}); err != nil {
		t.Fatal(err)
	}
	unmarked := store.ChannelRow{ID: id.New(), CommunityID: &cid, Kind: api.ChannelText, Mode: api.ModeE2EE, Name: "general", SettingsJSON: []byte(`{}`), HostPolicyVersion: 1, Created: now}
	if err := r.deps.Repo.CreateChannel(ctx, unmarked); err != nil {
		t.Fatal(err)
	}

	check := func(frames []gateway.Frame, kind uint64, scope id.ID, revision uint64) {
		t.Helper()
		if len(frames) != 1 || frames[0].Op != gateway.OpBotStateChanged || frames[0].Replay {
			t.Fatalf("hint frames = %+v, want one non-replayable bot_state_changed", frames)
		}
		p := framePayload(t, frames[0])
		if len(p) != 3 || rawUint(t, p[0]) != kind || !bytes.Equal(rawBstr(t, p[1]), scope[:]) || rawUint(t, p[2]) != revision {
			t.Fatalf("hint payload %x, want exactly [%d, %s, %d]", frames[0].Payload, kind, scope, revision)
		}
	}
	wantRevision := func(status int, body []byte, revision uint64) {
		t.Helper()
		if status != http.StatusOK || !bytes.Equal(body, mustCBOR(t, []any{revision})) {
			t.Fatalf("hint = %d %x, want 200 [%d]", status, body, revision)
		}
	}

	// A user hint is the user's in this community: op 63 names the community, the user being the
	// receiving connection's own.
	status, body := r.hint(t, r.botTok, 0, r.member.ID)
	wantRevision(status, body, 1)
	check(r.relay.frames(r.relay.user, r.member.ID), 0, r.community, 1)

	status, body = r.hint(t, r.botTok, 1, r.text)
	wantRevision(status, body, 1)
	check(r.relay.frames(r.relay.subscribed, textGroup.GroupID), 1, r.text, 1)

	status, body = r.hint(t, r.botTok, 2, r.voice)
	wantRevision(status, body, 1)
	check(r.relay.frames(r.relay.device, r.memberDevice.ID), 2, r.voice, 1)

	if n := r.relay.total(); n != 3 {
		t.Fatalf("hints delivered %d frames in all, want 3", n)
	}
	// The revision bumps, and the revisions route reads the stored value.
	status, body = r.hint(t, r.botTok, 0, r.member.ID)
	wantRevision(status, body, 2)

	// Out of scope: another community's member, channel and a non-voice call scope; an unknown kind.
	other, _, _, _ := seedKeyedSession(t, r.deps)
	otherCommunity := id.New()
	if err := r.deps.Repo.CreateCommunity(ctx, store.CommunityRow{ID: otherCommunity, Owner: other.ID, Name: "other", PolicyJSON: []byte(`{}`), PolicyVersion: 1, Created: now}); err != nil {
		t.Fatal(err)
	}
	foreign := store.ChannelRow{ID: id.New(), CommunityID: &otherCommunity, Kind: api.ChannelText, Mode: api.ModeE2EE, Name: "x", SettingsJSON: []byte(`{}`), HostPolicyVersion: 1, Created: now}
	if err := r.deps.Repo.CreateChannel(ctx, foreign); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		kind  uint64
		scope id.ID
		want  int
	}{
		"a non-member user":                     {0, other.ID, http.StatusNotFound},
		"another community's channel":           {1, foreign.ID, http.StatusNotFound},
		"another community's call scope":        {2, foreign.ID, http.StatusNotFound},
		"a call scope on a text channel":        {2, r.text, http.StatusNotFound},
		"a channel scope on a voice one":        {1, r.voice, http.StatusNotFound},
		"a text channel that is no Bot channel": {1, unmarked.ID, http.StatusNotFound},
		"the catalog kind":                      {3, r.community, http.StatusBadRequest},
	} {
		if status, _ := r.hint(t, r.botTok, tc.kind, tc.scope); status != tc.want {
			t.Fatalf("%s = %d, want %d", name, status, tc.want)
		}
	}
	// Only the Bot's runtime session sends hints.
	if status, _ := r.hint(t, r.memberTok, 0, r.member.ID); status != http.StatusForbidden {
		t.Fatalf("a member's hint = %d, want 403", status)
	}
	if n := r.relay.total(); n != 4 {
		t.Fatalf("refused hints delivered frames: %d in all, want 4", n)
	}
	for _, who := range []id.ID{other.ID, r.member.ID} {
		scopes, err := r.deps.Repo.BotStateRevisions(ctx, who, []id.ID{foreign.ID, unmarked.ID})
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range scopes {
			if row.ScopeKind != store.BotScopeUser || row.CommunityID != r.community {
				t.Fatalf("a refused hint stored revision %+v", row)
			}
		}
	}
}
