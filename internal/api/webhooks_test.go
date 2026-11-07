package api_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"image"
	"image/color"
	"image/png"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/gateway"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/server"
	"github.com/dilla-chat/dilla/internal/store"
	"github.com/dilla-chat/dilla/internal/store/sqlite"
	"github.com/dilla-chat/dilla/internal/store/sqlite/sqlitetest"
	"github.com/dilla-chat/dilla/internal/webhookavatar"
)

const avatarURL = "https://cdn.example.test/a.png"

// avatarFetch is the avatar fetcher of the fixture: no network. It answers avatarURL with png (already through
// webhookavatar.Process) and anything else with an error, and counts what it was asked.
type avatarFetch struct {
	mu   sync.Mutex
	png  []byte
	urls []string
}

func (a *avatarFetch) Fetch(_ context.Context, raw string) ([]byte, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.urls = append(a.urls, raw)
	if raw != avatarURL {
		return nil, webhookavatar.ErrAddress
	}
	return a.png, nil
}

func (a *avatarFetch) asked() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.urls)
}

func testAvatarPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	img.Set(2, 2, color.RGBA{G: 180, A: 255})
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	out, err := webhookavatar.Process(b.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// Bot v2 Task 13: incoming webhooks. Admins create them on the socket; anything that can send an HTTPS POST
// posts through /hooks/{webhook_id}/{secret}; dillad hands the text to the Bot's live connection (op 66) and
// answers once the Bot acks (op 67). Nothing of the body is stored or logged.

// webhookDelivery is one op 66 the fake gateway was asked to send.
type webhookDelivery struct {
	device, delivery id.ID
	payload          []cbor.RawMessage
}

// webhookGateway is the fake api.BotWebhookGateway: it records each delivery and answers with status, or
// with err, or hangs until the caller's deadline.
type webhookGateway struct {
	mu     sync.Mutex
	sent   []webhookDelivery
	status uint64
	err    error
	hang   bool
}

func (g *webhookGateway) DeliverBotWebhook(ctx context.Context, device, delivery id.ID, p cbor.RawMessage) (uint64, error) {
	var elems []cbor.RawMessage
	if err := cborx.Unmarshal(p, &elems); err != nil {
		return 0, err
	}
	g.mu.Lock()
	status, err, hang := g.status, g.err, g.hang
	if err == nil {
		g.sent = append(g.sent, webhookDelivery{device: device, delivery: delivery, payload: elems})
	}
	g.mu.Unlock()
	if err != nil {
		return 0, err
	}
	if hang {
		<-ctx.Done()
		return 0, gateway.ErrBotWebhookTimeout
	}
	return status, nil
}

func (g *webhookGateway) set(status uint64, err error, hang bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.status, g.err, g.hang = status, err, hang
}

func (g *webhookGateway) deliveries() []webhookDelivery {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.sent)
}

// webhookLookupRepo counts the webhook lookups the public route makes, so the bad-id and bad-secret paths can be
// shown to do the same store work.
type webhookLookupRepo struct {
	store.Repository
	mu      sync.Mutex
	lookups int
}

func (c *webhookLookupRepo) GetBotWebhook(ctx context.Context, webhook id.ID) (store.BotWebhookRow, error) {
	c.mu.Lock()
	c.lookups++
	c.mu.Unlock()
	return c.Repository.GetBotWebhook(ctx, webhook)
}

func (c *webhookLookupRepo) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lookups
}

type webhookFixture struct {
	*catalogFixture
	gw *webhookGateway
	// avatars is the fake fetcher behind the real avatar cache: it answers avatarURL with avatarPNG.
	avatars *avatarFetch
	path    string
	logs    *lockedBuffer
	counts  *webhookLookupRepo
	// public is the HTTP surface as dillad serves it: the mux behind the request log.
	public http.Handler
}

func newWebhookFixture(t *testing.T) *webhookFixture {
	t.Helper()
	path := filepath.Join(t.TempDir(), "webhooks.db")
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
	f := &webhookFixture{gw: &webhookGateway{}, path: path, logs: &lockedBuffer{}}
	f.catalogFixture = newCatalogFixtureOn(t, repo, func(d *api.Deps) {
		f.counts = &webhookLookupRepo{Repository: d.Repo}
		d.Repo = f.counts
		d.BotWebhooks = f.gw
		d.WebhookAckTimeout = 100 * time.Millisecond
		f.avatars = &avatarFetch{png: testAvatarPNG(t)}
		d.WebhookAvatars = webhookavatar.New(webhookavatar.Options{Clock: d.Clock, Limiter: d.Limiter, Fetcher: f.avatars})
		d.Log = slog.New(slog.NewTextHandler(f.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	})
	logger := slog.New(slog.NewTextHandler(f.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	f.public = server.RequestLog(logger, f.deps.Clock, nil)(muxOf(t, f.h))
	if rec := putCBORAuth(f.h, f.botChannelPath(f.text), nil, f.adminTok); rec.Code != http.StatusNoContent {
		t.Fatalf("marking the Bot channel = %d %x", rec.Code, rec.Body.Bytes())
	}
	return f
}

func (f *webhookFixture) webhooksPath() string {
	return "/v1/communities/" + f.community.String() + "/bot/webhooks"
}

// create makes a webhook on channel and returns its id and the URL shown once.
func (f *webhookFixture) create(t *testing.T, channel id.ID, name string, allowEveryone uint64) (id.ID, string) {
	t.Helper()
	rec := postCBORAuth(f.h, f.webhooksPath(), mustCBOR(t, []any{channel[:], name, nil, allowEveryone}), f.adminTok)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create webhook = %d %x, want 201", rec.Code, rec.Body.Bytes())
	}
	var out []cbor.RawMessage
	if err := cborx.Unmarshal(rec.Body.Bytes(), &out); err != nil || len(out) != 2 {
		t.Fatalf("create answer %x: %v", rec.Body.Bytes(), err)
	}
	var wid id.ID
	var url string
	if cborx.Unmarshal(out[0], &wid) != nil || cborx.Unmarshal(out[1], &url) != nil {
		t.Fatalf("create answer %x, want [webhook_id, url]", rec.Body.Bytes())
	}
	return wid, url
}

// hookPath is the path part of a webhook URL.
func hookPath(t *testing.T, url string) string {
	t.Helper()
	const prefix = "https://dilla.test"
	if !strings.HasPrefix(url, prefix+"/hooks/") {
		t.Fatalf("url %q, want https://dilla.test/hooks/…", url)
	}
	return strings.TrimPrefix(url, prefix)
}

func secretOf(t *testing.T, url string) string {
	t.Helper()
	parts := strings.Split(hookPath(t, url), "/")
	if len(parts) != 4 {
		t.Fatalf("url %q has %d parts", url, len(parts))
	}
	return parts[3]
}

// post sends a webhook POST from addr and returns the recorder.
func (f *webhookFixture) post(t *testing.T, path, contentType string, body []byte, addr string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, path, bytes.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.RemoteAddr = addr
	rec := httptest.NewRecorder()
	f.public.ServeHTTP(rec, req)
	return rec
}

func (f *webhookFixture) postJSON(t *testing.T, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	return f.post(t, path, "application/json", []byte(body), "198.51.100.7:4000")
}

func (f *webhookFixture) dump(t *testing.T) []byte {
	t.Helper()
	write, err := sqlite.OpenWrite(f.path)
	if err != nil {
		t.Fatal(err)
	}
	defer write.Close()
	out := filepath.Join(t.TempDir(), "dump.db")
	if err := store.VacuumInto(context.Background(), write, out); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func wantHookStatus(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("POST = %d %s, want %d", rec.Code, rec.Body.String(), status)
	}
	if code != "" && !strings.Contains(rec.Body.String(), `"code":"`+code+`"`) {
		t.Fatalf("POST body %s, want code %s", rec.Body.String(), code)
	}
}

func payloadString(t *testing.T, raw cbor.RawMessage) string {
	t.Helper()
	var s string
	if err := cborx.Unmarshal(raw, &s); err != nil {
		t.Fatalf("payload %x is not text: %v", raw, err)
	}
	return s
}

func payloadID(t *testing.T, raw cbor.RawMessage) id.ID {
	t.Helper()
	var v id.ID
	if err := cborx.Unmarshal(raw, &v); err != nil {
		t.Fatalf("payload %x is not an id: %v", raw, err)
	}
	return v
}

func TestWebhookPostIsDeliveredAndAnswered204(t *testing.T) {
	f := newWebhookFixture(t)
	wid, url := f.create(t, f.text, "Terraria", 0)
	rec := f.postJSON(t, hookPath(t, url), `{"text":"Ada joined the server"}`)
	wantHookStatus(t, rec, http.StatusNoContent, "")
	got := f.gw.deliveries()
	if len(got) != 1 || got[0].device != f.botDevice {
		t.Fatalf("deliveries %+v, want one to the Bot's device", got)
	}
	p := got[0].payload
	if len(p) != 8 || payloadID(t, p[0]) != got[0].delivery || payloadID(t, p[1]) != wid || payloadID(t, p[2]) != f.community ||
		payloadID(t, p[3]) != f.text || payloadString(t, p[4]) != "Terraria" || !bytes.Equal(p[5], []byte{0xf6}) ||
		payloadString(t, p[6]) != "Ada joined the server" || rawUint(t, p[7]) != 0 {
		t.Fatalf("op 66 payload %x", p)
	}
	if got[0].delivery.IsZero() {
		t.Fatal("the delivery id is zero")
	}
}

func TestWebhookBadSecretAndUnknownIDLookTheSame(t *testing.T) {
	f := newWebhookFixture(t)
	wid, url := f.create(t, f.text, "Terraria", 0)
	secret := secretOf(t, url)
	wrong := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x42}, 32))
	cases := map[string]string{
		"bad secret":       "/hooks/" + wid.String() + "/" + wrong,
		"unknown id":       "/hooks/" + id.New().String() + "/" + secret,
		"malformed id":     "/hooks/not-an-id/" + secret,
		"malformed secret": "/hooks/" + wid.String() + "/%21%21",
		// Fix round 1 M-1: a second base64url spelling of the right secret, and the right secret with one character
		// percent-escaped, are refused like a wrong one.
		"other spelling": "/hooks/" + wid.String() + "/" + otherSpelling(secret),
		"escaped path":   "/hooks/" + wid.String() + "/" + "%" + strings.ToUpper(hex.EncodeToString([]byte{secret[0]})) + secret[1:],
	}
	var firstBody []byte
	var firstType string
	for name, path := range cases {
		before := f.counts.count()
		rec := f.postJSON(t, path, `{"text":"x"}`)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s = %d %s, want 401", name, rec.Code, rec.Body.String())
		}
		if firstBody == nil {
			firstBody, firstType = rec.Body.Bytes(), rec.Header().Get("Content-Type")
		} else if !bytes.Equal(rec.Body.Bytes(), firstBody) || rec.Header().Get("Content-Type") != firstType {
			t.Fatalf("%s answered %q (%s), unlike %q (%s)", name, rec.Body.Bytes(), rec.Header().Get("Content-Type"), firstBody, firstType)
		}
		// The same store work: exactly one webhook lookup, whether or not the id exists or even parses.
		if n := f.counts.count() - before; n != 1 {
			t.Fatalf("%s made %d webhook lookups, want 1", name, n)
		}
	}
	if len(f.gw.deliveries()) != 0 {
		t.Fatal("a refused POST reached the Bot")
	}
	// The same timing class: the median of each path is within a small factor of the other's (both run one
	// lookup, one SHA-256 and one constant-time compare). The bound is loose so a loaded runner passes.
	median := func(path string) time.Duration {
		var ds []time.Duration
		for i := range 15 {
			start := time.Now()
			rec := f.post(t, path, "application/json", []byte(`{"text":"x"}`), "203.0.113."+string(rune('1'+i%9))+":1")
			ds = append(ds, time.Since(start))
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("%s = %d", path, rec.Code)
			}
		}
		slices.Sort(ds)
		return ds[len(ds)/2]
	}
	a, b := median(cases["bad secret"]), median(cases["unknown id"])
	if a > 5*b || b > 5*a {
		t.Fatalf("median bad secret %s vs unknown id %s: not one timing class", a, b)
	}
}

// otherSpelling is secret with the two unused low bits of its last character flipped: the same 32 bytes to a
// lenient decoder.
func otherSpelling(secret string) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	last := strings.IndexByte(alphabet, secret[len(secret)-1])
	return secret[:len(secret)-1] + string(alphabet[last^1])
}

func TestWebhookSecretIsStoredHashed(t *testing.T) {
	f := newWebhookFixture(t)
	_, url := f.create(t, f.text, "Terraria", 0)
	encoded := secretOf(t, url)
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || len(raw) != 32 {
		t.Fatalf("secret %q is not 32 base64url bytes: %v", encoded, err)
	}
	dump := f.dump(t)
	for name, needle := range map[string][]byte{"raw": raw, "base64url": []byte(encoded), "hex": []byte(hex.EncodeToString(raw))} {
		if bytes.Contains(dump, needle) {
			t.Fatalf("the store dump holds the secret (%s)", name)
		}
	}
	sum := sha256.Sum256(raw)
	if !bytes.Contains(dump, sum[:]) {
		t.Fatal("the store dump does not hold SHA-256(secret): the dump check is not reading the table")
	}
	// GET never returns the secret or its hash.
	rec := getAuth(f.h, f.webhooksPath(), f.adminTok)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET webhooks = %d", rec.Code)
	}
	if bytes.Contains(rec.Body.Bytes(), raw) || bytes.Contains(rec.Body.Bytes(), sum[:]) || bytes.Contains(rec.Body.Bytes(), []byte(encoded)) {
		t.Fatal("GET webhooks returns the secret or its hash")
	}
	var rows [][]cbor.RawMessage
	if err := cborx.Unmarshal(rec.Body.Bytes(), &rows); err != nil || len(rows) != 1 || len(rows[0]) != 7 {
		t.Fatalf("GET webhooks %x: %v, want one row of 7", rec.Body.Bytes(), err)
	}
}

func TestWebhookRateLimit30PerMinuteBurst5(t *testing.T) {
	f := newWebhookFixture(t)
	_, url := f.create(t, f.text, "Terraria", 0)
	path := hookPath(t, url)
	for i := range 5 {
		wantHookStatus(t, f.postJSON(t, path, `{"text":"burst"}`), http.StatusNoContent, "")
		_ = i
	}
	rec := f.postJSON(t, path, `{"text":"sixth"}`)
	wantHookStatus(t, rec, http.StatusTooManyRequests, "E_RATE_LIMITED")
	if got := rec.Header().Get("Retry-After"); got != "2" {
		t.Fatalf("Retry-After %q, want 2 (one token per 2 s)", got)
	}
	fake := f.deps.Clock.(*clock.Fake)
	fake.Advance(2 * time.Second)
	wantHookStatus(t, f.postJSON(t, path, `{"text":"recovered"}`), http.StatusNoContent, "")
	wantHookStatus(t, f.postJSON(t, path, `{"text":"again"}`), http.StatusTooManyRequests, "E_RATE_LIMITED")
	if n := len(f.gw.deliveries()); n != 6 {
		t.Fatalf("%d deliveries, want 6", n)
	}
}

// The per-address bucket is separate from the per-webhook one: one address cannot enumerate or flood ids,
// and another address is not affected.
func TestWebhookPerAddressLimit(t *testing.T) {
	f := newWebhookFixture(t)
	burst := server.WebhookAddressClass.Burst
	for i := range burst {
		rec := f.post(t, "/hooks/"+id.New().String()+"/x", "application/json", []byte(`{"text":"x"}`), "192.0.2.10:1")
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d = %d, want 401", i, rec.Code)
		}
	}
	rec := f.post(t, "/hooks/"+id.New().String()+"/x", "application/json", []byte(`{"text":"x"}`), "192.0.2.10:1")
	wantHookStatus(t, rec, http.StatusTooManyRequests, "E_RATE_LIMITED")
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("a per-address 429 carries no Retry-After")
	}
	_, url := f.create(t, f.text, "Terraria", 0)
	wantHookStatus(t, f.post(t, hookPath(t, url), "application/json", []byte(`{"text":"x"}`), "192.0.2.11:1"), http.StatusNoContent, "")
}

func TestWebhookBodyNeverStored(t *testing.T) {
	f := newWebhookFixture(t)
	_, url := f.create(t, f.text, "Terraria", 0)
	text := "secret-plans-" + strings.Repeat("zq", 40)
	wantHookStatus(t, f.postJSON(t, hookPath(t, url), `{"text":"`+text+`","username":"Ada"}`), http.StatusNoContent, "")
	if n := len(f.gw.deliveries()); n != 1 {
		t.Fatalf("%d deliveries, want 1", n)
	}
	for name, haystack := range map[string][]byte{"store dump": f.dump(t), "logs": f.logs.Bytes()} {
		if bytes.Contains(haystack, []byte(text)) {
			t.Fatalf("the %s holds the webhook text", name)
		}
	}
	// The gateway half (internal/gateway TestWebhookReachesTheLiveBotUnringedAndWaitsForItsAck) shows op 66
	// is sent with n = 0 and leaves nothing in the Bot connection's ring.
}

func TestWebhookWhileBotOfflineIs503AndNotStored(t *testing.T) {
	f := newWebhookFixture(t)
	_, url := f.create(t, f.text, "Terraria", 0)
	path := hookPath(t, url)
	text := "offline-text-" + strings.Repeat("k", 30)
	for name, set := range map[string]func(){
		"offline":     func() { f.gw.set(0, gateway.ErrBotNotLive, false) },
		"busy":        func() { f.gw.set(0, gateway.ErrBotWebhookBusy, false) },
		"ack timeout": func() { f.gw.set(0, nil, true) },
		"not posted":  func() { f.gw.set(1, nil, false) },
	} {
		set()
		rec := f.postJSON(t, path, `{"text":"`+text+`"}`)
		if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "5" {
			t.Fatalf("%s = %d Retry-After %q, want 503 with Retry-After 5", name, rec.Code, rec.Header().Get("Retry-After"))
		}
		f.deps.Clock.(*clock.Fake).Advance(2 * time.Second)
	}
	// No ready installation is offline too.
	f.gw.set(0, nil, false)
	ctx := context.Background()
	inst, err := f.deps.Repo.GetActiveBotInstallationByCommunity(ctx, f.community)
	if err != nil {
		t.Fatal(err)
	}
	disabled := inst
	disabled.State, disabled.Revision, disabled.Updated = store.BotInstallationDisabled, inst.Revision+1, inst.Updated+1
	if err := f.deps.Repo.Tx(ctx, func(tx store.Repository) error {
		return tx.UpdateBotInstallation(ctx, inst.ID, inst.Revision, disabled)
	}); err != nil {
		t.Fatalf("disable installation: %v", err)
	}
	rec := f.postJSON(t, path, `{"text":"`+text+`"}`)
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "5" {
		t.Fatalf("disabled Bot = %d, want 503 with Retry-After 5", rec.Code)
	}
	for name, haystack := range map[string][]byte{"store dump": f.dump(t), "logs": f.logs.Bytes()} {
		if bytes.Contains(haystack, []byte(text)) {
			t.Fatalf("the %s holds the text of a refused POST", name)
		}
	}
}

func TestWebhookTargetMustBeBotChannel(t *testing.T) {
	f := newWebhookFixture(t)
	// Create: a channel that is not a Bot channel is refused, whatever kind it is.
	for name, ch := range map[string]id.ID{"unmarked voice": f.voice, "readable": f.readable, "unknown": id.New()} {
		rec := postCBORAuth(f.h, f.webhooksPath(), mustCBOR(t, []any{ch[:], "x", nil, uint64(0)}), f.adminTok)
		if rec.Code != http.StatusConflict || bodyCode(t, rec.Body.Bytes()) != "E_NOT_BOT_CHANNEL" {
			t.Fatalf("create on %s = %d %x, want 409 E_NOT_BOT_CHANNEL", name, rec.Code, rec.Body.Bytes())
		}
	}
	other := f.secondTextChannel(t)
	rec := postCBORAuth(f.h, f.webhooksPath(), mustCBOR(t, []any{other[:], "x", nil, uint64(0)}), f.adminTok)
	if rec.Code != http.StatusConflict || bodyCode(t, rec.Body.Bytes()) != "E_NOT_BOT_CHANNEL" {
		t.Fatalf("create on an unmarked text channel = %d, want 409 E_NOT_BOT_CHANNEL", rec.Code)
	}
	// POST: unmarking the channel after creation makes every POST 409; marking it again restores it.
	_, url := f.create(t, f.text, "Terraria", 0)
	path := hookPath(t, url)
	if rec := deleteAuthCBOR(f.h, f.botChannelPath(f.text), f.adminTok); rec.Code != http.StatusNoContent {
		t.Fatalf("unmark = %d", rec.Code)
	}
	wantHookStatus(t, f.postJSON(t, path, `{"text":"x"}`), http.StatusConflict, "E_NOT_BOT_CHANNEL")
	if rec := putCBORAuth(f.h, f.botChannelPath(f.text), nil, f.adminTok); rec.Code != http.StatusNoContent {
		t.Fatalf("mark again = %d", rec.Code)
	}
	wantHookStatus(t, f.postJSON(t, path, `{"text":"x"}`), http.StatusNoContent, "")
	if n := len(f.gw.deliveries()); n != 1 {
		t.Fatalf("%d deliveries, want 1 (the 409 reached nobody)", n)
	}
}

// A channel delete deletes the webhooks that target it, in its own transaction (a soft delete fires no cascade),
// and a community delete deletes all of the community's (Task 9's DeleteBotCommunityState).
func TestWebhookGoesWithItsChannelAndCommunity(t *testing.T) {
	e := newEnv(t)
	log := slog.New(slog.DiscardHandler)
	api.NewCommunities(e.Repo, e.DS, e.Clk, log).Register(e.Mux)
	api.NewChannels(e.Repo, e.DS, e.Clk, 10, log).Register(e.Mux)
	owner, ownerTok := e.NewUser("owner")
	cid := createCommunity(t, e, ownerTok)
	ctx := context.Background()
	hook := func(name string) (id.ID, id.ID) {
		t.Helper()
		ch, _, status := newChannel(t, e, cid, ownerTok, uint64(api.ChannelText), uint64(api.ModeE2EE), 0, name)
		if status != http.StatusCreated {
			t.Fatalf("create %s = %d", name, status)
		}
		if _, err := e.Repo.MarkBotChannel(ctx, store.BotChannelRow{ChannelID: ch, CommunityID: cid, MarkedAt: 1, MarkedBy: owner}); err != nil {
			t.Fatal(err)
		}
		wid := id.New()
		if err := e.Repo.CreateBotWebhook(ctx, store.BotWebhookRow{WebhookID: wid, CommunityID: cid, ChannelID: ch, Name: name,
			SecretHash: bytes.Repeat([]byte{3}, 32), CreatedAt: 1}); err != nil {
			t.Fatal(err)
		}
		return ch, wid
	}
	deletedCh, deletedHook := hook("bot")
	_, keptHook := hook("news")
	if status, body := e.Do(http.MethodDelete, "/v1/channels/"+deletedCh.String(), ownerTok, nil); status != http.StatusNoContent {
		t.Fatalf("delete channel = %d %x", status, body)
	}
	if _, err := e.Repo.GetBotWebhook(ctx, deletedHook); err == nil {
		t.Fatal("the deleted channel's webhook survived")
	}
	if _, err := e.Repo.GetBotWebhook(ctx, keptHook); err != nil {
		t.Fatalf("another channel's webhook went too: %v", err)
	}
	if status, body := e.Do(http.MethodDelete, "/v1/communities/"+cid.String(), ownerTok, nil); status != http.StatusNoContent {
		t.Fatalf("delete community = %d %x", status, body)
	}
	if _, err := e.Repo.GetBotWebhook(ctx, keptHook); err == nil {
		t.Fatal("the deleted community's webhook survived")
	}
}

func (f *webhookFixture) secondTextChannel(t *testing.T) id.ID {
	t.Helper()
	cid := f.community
	ch := store.ChannelRow{ID: id.New(), CommunityID: &cid, Kind: api.ChannelText, Mode: api.ModeE2EE, Name: "general",
		SettingsJSON: []byte(`{}`), HostPolicyVersion: 1, Created: f.deps.Clock.Now().Unix()}
	if err := f.deps.Repo.CreateChannel(context.Background(), ch); err != nil {
		t.Fatal(err)
	}
	return ch.ID
}

func TestWebhookTextOver4000BytesIs413(t *testing.T) {
	f := newWebhookFixture(t)
	_, url := f.create(t, f.text, "Terraria", 0)
	path := hookPath(t, url)
	wantHookStatus(t, f.postJSON(t, path, `{"text":"`+strings.Repeat("é", 2000)+`"}`), http.StatusNoContent, "")
	wantHookStatus(t, f.postJSON(t, path, `{"text":"`+strings.Repeat("a", 4001)+`"}`), http.StatusRequestEntityTooLarge, "E_TOO_LARGE")
	// A body over 8 KiB is refused before it is parsed.
	wantHookStatus(t, f.postJSON(t, path, `{"text":"a","pad":"`+strings.Repeat("p", 8<<10)+`"}`), http.StatusRequestEntityTooLarge, "E_TOO_LARGE")
	if n := len(f.gw.deliveries()); n != 1 {
		t.Fatalf("%d deliveries, want 1", n)
	}
}

func TestRotateInvalidatesOldURL(t *testing.T) {
	f := newWebhookFixture(t)
	wid, oldURL := f.create(t, f.text, "Terraria", 0)
	rec := postCBORAuth(f.h, f.webhooksPath()+"/"+wid.String()+"/rotate", nil, f.adminTok)
	if rec.Code != http.StatusOK {
		t.Fatalf("rotate = %d %x", rec.Code, rec.Body.Bytes())
	}
	var out []string
	if err := cborx.Unmarshal(rec.Body.Bytes(), &out); err != nil || len(out) != 1 || out[0] == oldURL {
		t.Fatalf("rotate answer %x: %v, want a new [url]", rec.Body.Bytes(), err)
	}
	newURL := out[0]
	if !strings.Contains(newURL, wid.String()) {
		t.Fatalf("rotated url %q changed the webhook id", newURL)
	}
	wantHookStatus(t, f.postJSON(t, hookPath(t, oldURL), `{"text":"x"}`), http.StatusUnauthorized, "E_UNAUTHENTICATED")
	wantHookStatus(t, f.postJSON(t, hookPath(t, newURL), `{"text":"x"}`), http.StatusNoContent, "")
	row, err := f.deps.Repo.GetBotWebhook(context.Background(), wid)
	if err != nil || row.RotatedAt == nil {
		t.Fatalf("after rotate = %+v, %v; want rotated_at set", row, err)
	}
	// Rotating an unknown webhook is 404.
	if rec := postCBORAuth(f.h, f.webhooksPath()+"/"+id.New().String()+"/rotate", nil, f.adminTok); rec.Code != http.StatusNotFound {
		t.Fatalf("rotate unknown = %d, want 404", rec.Code)
	}
}

func TestDiscordFieldNamesAccepted(t *testing.T) {
	f := newWebhookFixture(t)
	_, url := f.create(t, f.text, "Terraria", 1)
	path := hookPath(t, url)
	wantHookStatus(t, f.postJSON(t, path,
		`{"text":"boss down","username":"Ada\u0007 \u202eLovelace","avatar_url":"`+avatarURL+`","embeds":[{"title":"x"}],"tts":false}`),
		http.StatusNoContent, "")
	// Discord's own body field is content; it is read when text is absent.
	wantHookStatus(t, f.postJSON(t, path, `{"content":"from discord","username":"Ada"}`), http.StatusNoContent, "")
	got := f.gw.deliveries()
	if len(got) != 2 {
		t.Fatalf("%d deliveries, want 2", len(got))
	}
	p := got[0].payload
	if name := payloadString(t, p[4]); name != "Ada Lovelace" {
		t.Fatalf("display name %q, want control and format characters stripped", name)
	}
	var avatarID []byte
	if err := cborx.Unmarshal(p[5], &avatarID); err != nil || len(avatarID) != 32 {
		t.Fatalf("avatar element %x, want the 32-byte cached avatar id", p[5])
	}
	if sum := sha256.Sum256(f.avatars.png); !bytes.Equal(avatarID, sum[:]) {
		t.Fatalf("avatar id %x, want SHA-256 of the re-encoded image", avatarID)
	}
	for _, d := range got {
		for _, elem := range d.payload {
			if bytes.Contains(elem, []byte("cdn.example.test")) {
				t.Fatal("op 66 carries the sender's avatar URL")
			}
		}
	}
	if payloadString(t, p[6]) != "boss down" || rawUint(t, p[7]) != 1 {
		t.Fatalf("text %q allow_everyone %d", payloadString(t, p[6]), rawUint(t, p[7]))
	}
	if payloadString(t, got[1].payload[6]) != "from discord" {
		t.Fatalf("content alias text %q", payloadString(t, got[1].payload[6]))
	}
}

func TestWebhookMalformedBodiesAre400(t *testing.T) {
	f := newWebhookFixture(t)
	_, url := f.create(t, f.text, "Terraria", 0)
	path := hookPath(t, url)
	for name, body := range map[string]string{
		"not json":          `text=hello`,
		"array":             `["hello"]`,
		"text a number":     `{"text":5}`,
		"text null":         `{"text":null}`,
		"no text":           `{"username":"Ada"}`,
		"empty text":        `{"text":""}`,
		"username a number": `{"text":"x","username":7}`,
		"username too long": `{"text":"x","username":"` + strings.Repeat("u", 81) + `"}`,
		"avatar http":       `{"text":"x","avatar_url":"http://example.test/a.png"}`,
		"avatar javascript": `{"text":"x","avatar_url":"javascript:alert(1)"}`,
		"avatar long":       `{"text":"x","avatar_url":"https://example.test/` + strings.Repeat("a", 2048) + `"}`,
		"avatar port":       `{"text":"x","avatar_url":"https://example.test:8443/a.png"}`,
		"avatar userinfo":   `{"text":"x","avatar_url":"https://u:p@example.test/a.png"}`,
		"text NUL":          `{"text":"a\u0000b"}`,
		"text bell":         `{"text":"a\u0007b"}`,
		"text DEL":          `{"text":"a\u007fb"}`,
		"text C1":           `{"text":"a\u0085b"}`,
		"avatar not text":   `{"text":"x","avatar_url":{"u":1}}`,
		"trailing data":     `{"text":"x"} {"text":"y"}`,
	} {
		wantHookStatus(t, f.postJSON(t, path, body), http.StatusBadRequest, "E_INVALID_REQUEST")
		f.deps.Clock.(*clock.Fake).Advance(2 * time.Second)
		_ = name
	}
	wantHookStatus(t, f.post(t, path, "application/json", []byte("{\"text\":\"\xff\xfe\"}"), "198.51.100.7:4000"), http.StatusBadRequest, "E_INVALID_REQUEST")
	f.deps.Clock.(*clock.Fake).Advance(2 * time.Second)
	wantHookStatus(t, f.post(t, path, "text/plain", []byte(`{"text":"x"}`), "198.51.100.7:4000"), http.StatusBadRequest, "E_INVALID_REQUEST")
	if n := len(f.gw.deliveries()); n != 0 {
		t.Fatalf("%d malformed bodies reached the Bot", n)
	}
}

// The secret travels in the path; no log line, the access log included, may carry it.
func TestWebhookSecretNeverLogged(t *testing.T) {
	f := newWebhookFixture(t)
	wid, url := f.create(t, f.text, "Terraria", 0)
	secret := secretOf(t, url)
	wantHookStatus(t, f.postJSON(t, hookPath(t, url), `{"text":"x"}`), http.StatusNoContent, "")
	wantHookStatus(t, f.postJSON(t, "/hooks/"+wid.String()+"/"+secret+"x", `{"text":"x"}`), http.StatusUnauthorized, "")
	f.gw.set(0, gateway.ErrBotNotLive, false)
	f.deps.Clock.(*clock.Fake).Advance(2 * time.Second)
	wantHookStatus(t, f.postJSON(t, hookPath(t, url), `{"text":"x"}`), http.StatusServiceUnavailable, "")
	logs := f.logs.Bytes()
	if bytes.Contains(logs, []byte(secret)) {
		t.Fatalf("the logs hold the secret:\n%s", logs)
	}
	if !bytes.Contains(logs, []byte("route=\"POST /hooks/{webhook_id}/{secret}\"")) {
		t.Fatalf("the access log does not name the route pattern:\n%s", logs)
	}
}

func TestWebhookAdminRoutes(t *testing.T) {
	f := newWebhookFixture(t)
	wid, _ := f.create(t, f.text, "Terraria", 0)
	base := f.webhooksPath()
	one := base + "/" + wid.String()
	// Only someone who may manage the Bot.
	for name, rec := range map[string]*httptest.ResponseRecorder{
		"create": postCBORAuth(f.h, base, mustCBOR(t, []any{f.text[:], "x", nil, uint64(0)}), f.memberTok),
		"list":   getAuth(f.h, base, f.memberTok),
		"rotate": postCBORAuth(f.h, one+"/rotate", nil, f.memberTok),
		"patch":  doBotCBOR(f.h, http.MethodPatch, one, mustCBOR(t, []any{"y", nil, uint64(1)}), "Bearer "+f.memberTok),
		"delete": doBotCBOR(f.h, http.MethodDelete, one, nil, "Bearer "+f.memberTok),
	} {
		if rec.Code != http.StatusForbidden {
			t.Fatalf("a plain member's %s = %d, want 403", name, rec.Code)
		}
	}
	// Bad create bodies.
	for name, body := range map[string][]any{
		"empty name":     {f.text[:], "", nil, uint64(0)},
		"control name":   {f.text[:], "a\u0007b", nil, uint64(0)},
		"short avatar":   {f.text[:], "x", []byte{1, 2}, uint64(0)},
		"allow_everyone": {f.text[:], "x", nil, uint64(2)},
	} {
		if rec := postCBORAuth(f.h, base, mustCBOR(t, body), f.adminTok); rec.Code != http.StatusBadRequest {
			t.Fatalf("create with %s = %d, want 400", name, rec.Code)
		}
	}
	avatar := bytes.Repeat([]byte{9}, 32)
	if rec := doBotCBOR(f.h, http.MethodPatch, one, mustCBOR(t, []any{"Weather", avatar, uint64(1)}), "Bearer "+f.adminTok); rec.Code != http.StatusNoContent {
		t.Fatalf("patch = %d %x", rec.Code, rec.Body.Bytes())
	}
	rec := getAuth(f.h, base, f.adminTok)
	var rows [][]cbor.RawMessage
	if err := cborx.Unmarshal(rec.Body.Bytes(), &rows); err != nil || len(rows) != 1 || len(rows[0]) != 7 {
		t.Fatalf("list %x: %v", rec.Body.Bytes(), err)
	}
	r := rows[0]
	var gotAvatar []byte
	if payloadID(t, r[0]) != wid || payloadID(t, r[1]) != f.text || payloadString(t, r[2]) != "Weather" ||
		cborx.Unmarshal(r[3], &gotAvatar) != nil || !bytes.Equal(gotAvatar, avatar) || rawUint(t, r[4]) != 1 || !bytes.Equal(r[6], []byte{0xf6}) {
		t.Fatalf("row %x", r)
	}
	if rec := doBotCBOR(f.h, http.MethodPatch, base+"/"+id.New().String(), mustCBOR(t, []any{"x", nil, uint64(0)}), "Bearer "+f.adminTok); rec.Code != http.StatusNotFound {
		t.Fatalf("patch unknown = %d, want 404", rec.Code)
	}
	if rec := doBotCBOR(f.h, http.MethodDelete, one, nil, "Bearer "+f.adminTok); rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d", rec.Code)
	}
	if rec := doBotCBOR(f.h, http.MethodDelete, one, nil, "Bearer "+f.adminTok); rec.Code != http.StatusNotFound {
		t.Fatalf("delete again = %d, want 404", rec.Code)
	}
}

// Fix round 1 M-2: tab, line feed and carriage return are the text's only control characters.
func TestWebhookTextKeepsTabsAndLineBreaks(t *testing.T) {
	f := newWebhookFixture(t)
	_, url := f.create(t, f.text, "Terraria", 0)
	wantHookStatus(t, f.postJSON(t, hookPath(t, url), `{"text":"a\tb\r\nc"}`), http.StatusNoContent, "")
	if got := payloadString(t, f.gw.deliveries()[0].payload[6]); got != "a\tb\r\nc" {
		t.Fatalf("text %q", got)
	}
}

// Fix round 1 (Jonas, 2026-11-07): the sender's avatar is fetched by dillad, cached, and served from the instance to
// members of the webhook's community only.
func TestWebhookAvatarIsCachedAndServedOnlyToMembers(t *testing.T) {
	f := newWebhookFixture(t)
	_, url := f.create(t, f.text, "Terraria", 0)
	for range 2 {
		wantHookStatus(t, f.postJSON(t, hookPath(t, url), `{"text":"x","avatar_url":"`+avatarURL+`"}`), http.StatusNoContent, "")
	}
	if asked := f.avatars.asked(); len(asked) != 1 {
		t.Fatalf("the avatar was fetched %d times for two posts, want once", len(asked))
	}
	sum := sha256.Sum256(f.avatars.png)
	path := "/v1/communities/" + f.community.String() + "/bot/webhook-avatars/" + hex.EncodeToString(sum[:])
	rec := getAuth(f.h, path, f.memberTok)
	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), f.avatars.png) {
		t.Fatalf("member GET avatar = %d (%d bytes)", rec.Code, rec.Body.Len())
	}
	h := rec.Header()
	if h.Get("Content-Type") != "image/png" || h.Get("X-Content-Type-Options") != "nosniff" || !strings.Contains(h.Get("Cache-Control"), "private") {
		t.Fatalf("avatar headers %v", h)
	}
	_, _, _, strangerTok := seedKeyedSession(t, f.deps)
	if rec := getAuth(f.h, path, strangerTok); rec.Code != http.StatusNotFound {
		t.Fatalf("non-member GET avatar = %d, want 404", rec.Code)
	}
	if rec := getAuth(f.h, path, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous GET avatar = %d, want 401", rec.Code)
	}
	other := sha256.Sum256([]byte("nothing"))
	if rec := getAuth(f.h, "/v1/communities/"+f.community.String()+"/bot/webhook-avatars/"+hex.EncodeToString(other[:]), f.memberTok); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown avatar = %d, want 404", rec.Code)
	}
	// A URL that fails is posted without an avatar, and not fetched again on the next post.
	for range 2 {
		wantHookStatus(t, f.postJSON(t, hookPath(t, url), `{"text":"x","avatar_url":"https://internal.test/a.png"}`), http.StatusNoContent, "")
	}
	if n := len(f.avatars.asked()); n != 2 {
		t.Fatalf("%d fetches, want 2 (the failing URL once)", n)
	}
	d := f.gw.deliveries()
	if last := d[len(d)-1].payload[5]; !bytes.Equal(last, []byte{0xf6}) {
		t.Fatalf("a failed avatar posted %x, want null", last)
	}
}

// Nothing is fetched for a POST that did not authenticate.
func TestWebhookWrongSecretFetchesNoAvatar(t *testing.T) {
	f := newWebhookFixture(t)
	wid, _ := f.create(t, f.text, "Terraria", 0)
	wrong := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x42}, 32))
	wantHookStatus(t, f.postJSON(t, "/hooks/"+wid.String()+"/"+wrong, `{"text":"x","avatar_url":"`+avatarURL+`"}`), http.StatusUnauthorized, "")
	wantHookStatus(t, f.postJSON(t, "/hooks/"+id.New().String()+"/"+wrong, `{"text":"x","avatar_url":"`+avatarURL+`"}`), http.StatusUnauthorized, "")
	if asked := f.avatars.asked(); len(asked) != 0 {
		t.Fatalf("an unauthenticated POST fetched %v", asked)
	}
}

// Fix round 1 M-3: one network meets one bucket however many /64s it spreads attempts over.
func TestWebhookPerNetworkLimit(t *testing.T) {
	f := newWebhookFixture(t)
	burst := server.WebhookPrefixClass.Burst
	for i := range burst {
		addr := "[2001:db8:7:" + strconv.FormatInt(int64(i), 16) + "::1]:1"
		if rec := f.post(t, "/hooks/"+id.New().String()+"/x", "application/json", []byte(`{"text":"x"}`), addr); rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d = %d, want 401", i, rec.Code)
		}
	}
	rec := f.post(t, "/hooks/"+id.New().String()+"/x", "application/json", []byte(`{"text":"x"}`), "[2001:db8:7:ffff::1]:1")
	wantHookStatus(t, rec, http.StatusTooManyRequests, "E_RATE_LIMITED")
	if rec := f.post(t, "/hooks/"+id.New().String()+"/x", "application/json", []byte(`{"text":"x"}`), "[2001:db8:8::1]:1"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("another /48 = %d, want 401", rec.Code)
	}
}

// Fix round 2 S1: junk from a network spends only the failure buckets, so a valid sender on the same network still
// posts, and the junk itself is still limited.
func TestWebhookJunkFromANetworkDoesNotThrottleAValidSender(t *testing.T) {
	f := newWebhookFixture(t)
	_, url := f.create(t, f.text, "Terraria", 0)
	for i := range server.WebhookPrefixClass.Burst {
		addr := "[2001:db8:9:" + strconv.FormatInt(int64(i), 16) + "::1]:1"
		if rec := f.post(t, "/hooks/"+id.New().String()+"/x", "application/json", []byte(`{"text":"x"}`), addr); rec.Code != http.StatusUnauthorized {
			t.Fatalf("junk %d = %d, want 401", i, rec.Code)
		}
	}
	// The network's failures are now limited...
	rec := f.post(t, "/hooks/"+id.New().String()+"/x", "application/json", []byte(`{"text":"x"}`), "[2001:db8:9:fffe::1]:1")
	wantHookStatus(t, rec, http.StatusTooManyRequests, "E_RATE_LIMITED")
	// ...and a valid sender on the same /48 is not.
	wantHookStatus(t, f.post(t, hookPath(t, url), "application/json", []byte(`{"text":"still here"}`), "[2001:db8:9:ffff::1]:1"), http.StatusNoContent, "")
}

// Fix round 2 S1: an address that keeps presenting wrong secrets is refused before any work, whatever it presents
// next; a bad id and a bad secret spend the failure bucket alike.
func TestWebhookBadSecretFloodIsStillLimited(t *testing.T) {
	f := newWebhookFixture(t)
	wid, url := f.create(t, f.text, "Terraria", 0)
	wrong := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x42}, 32))
	const flooder = "198.51.100.99:1"
	for i := range server.WebhookAddressClass.Burst {
		path := "/hooks/" + wid.String() + "/" + wrong
		if i%2 == 1 {
			path = "/hooks/" + id.New().String() + "/" + wrong
		}
		if rec := f.post(t, path, "application/json", []byte(`{"text":"x"}`), flooder); rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d = %d, want 401", i, rec.Code)
		}
	}
	before := f.counts.count()
	rec := f.post(t, hookPath(t, url), "application/json", []byte(`{"text":"x"}`), flooder)
	wantHookStatus(t, rec, http.StatusTooManyRequests, "E_RATE_LIMITED")
	if f.counts.count() != before {
		t.Fatal("an address over its failure bucket still caused a webhook lookup")
	}
}

// Fix round 2 S1: every request, valid secrets included, spends the address's all-requests bucket.
func TestWebhookAllRequestsBucketBoundsAnAddress(t *testing.T) {
	f := newWebhookFixture(t)
	_, url := f.create(t, f.text, "Terraria", 0)
	const sender = "192.0.2.77:1"
	for range server.WebhookAddressAllClass.Burst {
		f.deps.Limiter.Allow(server.WebhookAddressAllClass, "192.0.2.77")
	}
	wantHookStatus(t, f.post(t, hookPath(t, url), "application/json", []byte(`{"text":"x"}`), sender), http.StatusTooManyRequests, "E_RATE_LIMITED")
	wantHookStatus(t, f.post(t, hookPath(t, url), "application/json", []byte(`{"text":"x"}`), "192.0.2.78:1"), http.StatusNoContent, "")
}
