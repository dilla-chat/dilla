package dillad_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/dillad"
	"github.com/dilla-chat/dilla/internal/id"
)

// awayOf reads the next op 55 afk_away on c and answers its community and since_ts.
func awayOf(t *testing.T, ctx context.Context, c *websocket.Conn) (id.ID, uint64) {
	t.Helper()
	payload, ok := nextOp(ctx, c, 55)
	if !ok || len(payload) != 3 {
		t.Fatalf("no afk_away arrived (%#v, %v)", payload, ok)
	}
	var cid id.ID
	b, _ := payload[0].([]byte)
	copy(cid[:], b)
	since, _ := payload[2].(uint64)
	return cid, since
}

// AW2 through the composition root: R-AW1 refuses a call in the AFK channel; PUT …/afk/away marks
// the member away and the owner's open tab hears op 55; a readable post and a typing op each clear
// it again (Q1), heard as op 55 since 0. The offline clear (Q2) is TestTheOfflineGraceIsWired...
func TestTheAwayRosterIsWiredThroughTheCompositionRoot(t *testing.T) {
	cfg, code := testConfigInvite(t)
	cfg.Log.Level = "warn"
	srv, err := dillad.New(context.Background(), dillad.Options{
		Config: cfg, Clock: clock.System(), Wasm: sharedRuntime(t),
	})
	if err != nil {
		t.Fatalf("dillad.New: %v", err)
	}
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	h := srv.Handler()

	owner := accountToken(t, srv, code)
	member := namedAccount(t, srv, mintInvite(t, srv), "friend")
	rec := call(t, h, http.MethodPost, "/v1/communities", owner, []any{"fika", []byte("{}"), uint64(0), uint64(0)})
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /v1/communities = %d %q", rec.Code, errorCode(rec))
	}
	cid := idOf(t, decodeArray(t, rec)[0])
	if rec := call(t, h, http.MethodPost, "/v1/communities/"+cid.String()+"/join", member, []any{nil}); rec.Code != http.StatusOK {
		t.Fatalf("join = %d %q", rec.Code, errorCode(rec))
	}
	rec = call(t, h, http.MethodGet, "/v1/communities/"+cid.String()+"/afk", member, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET afk = %d %q", rec.Code, errorCode(rec))
	}
	snap := decodeArray(t, rec)
	if len(snap) != 5 {
		t.Fatalf("AFK snapshot has %d elements, want 5", len(snap))
	}
	afk := idOf(t, snap[0])
	if rec := call(t, h, http.MethodPost, "/v1/channels/"+afk.String()+"/calls", member, []any{}); rec.Code != http.StatusForbidden {
		t.Fatalf("start in AFK = %d %q, want 403", rec.Code, errorCode(rec))
	}
	rec = call(t, h, http.MethodPost, "/v1/communities/"+cid.String()+"/channels", owner,
		[]any{uint64(api.ChannelText), uint64(api.ModeReadable), uint64(api.VisDiscoverable), nil, "lobby", "", uint64(0), uint64(0)})
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST channel = %d %q", rec.Code, errorCode(rec))
	}
	lobby := idOf(t, decodeArray(t, rec)[0])

	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	oc := dialReady(t, ctx, ts, owner)
	away := func(mc *websocket.Conn) {
		t.Helper()
		if rec := call(t, h, http.MethodPut, "/v1/communities/"+cid.String()+"/afk/away", member, []any{}); rec.Code != http.StatusNoContent {
			t.Fatalf("PUT afk/away = %d %q", rec.Code, errorCode(rec))
		}
		if got, since := awayOf(t, ctx, oc); got != cid || since == 0 {
			t.Fatalf("afk_away = %s since %d, want %s since > 0", got, since, cid)
		}
	}
	cleared := func(what string) {
		t.Helper()
		if got, since := awayOf(t, ctx, oc); got != cid || since != 0 {
			t.Fatalf("after %s: afk_away = %s since %d, want %s since 0", what, got, since, cid)
		}
	}

	mc := dialReady(t, ctx, ts, member)
	away(mc)
	envelope, err := cborx.Marshal([]any{uint64(1), id.New(), uint64(0), nil, nil, "back", []any{}, []any{}, bytes.Repeat([]byte{0x06}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	if rec := call(t, h, http.MethodPost, "/v1/channels/"+lobby.String()+"/messages", member, []any{envelope}); rec.Code != http.StatusOK {
		t.Fatalf("POST message = %d %q", rec.Code, errorCode(rec))
	}
	cleared("a post")

	away(mc)
	typing, err := cborx.Marshal([]any{uint64(51), uint64(0), nil, []any{lobby[:], uint64(1)}})
	if err != nil {
		t.Fatal(err)
	}
	if err := mc.Write(ctx, websocket.MessageBinary, typing); err != nil {
		t.Fatal(err)
	}
	cleared("typing")
}

// Q2 as revised, through the composition root on a fake clock: closing one of two tabs keeps the
// user away; closing the last one clears it only once 30 s pass with no connection, and the AFK
// snapshot then lists nobody.
func TestTheOfflineGraceIsWiredThroughTheCompositionRoot(t *testing.T) {
	s, ts, owner, clk := newGreetedServerAt(t)
	h := s.Handler()
	// Past the boot grace first (R-AW5): its one pass clears the rows of users with no connection,
	// which would hide whether the offline grace waited.
	for range 95 {
		clk.Advance(time.Second)
		time.Sleep(2 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	rec := call(t, h, http.MethodPost, "/v1/communities", owner, []any{"fika", []byte("{}"), uint64(0), uint64(0)})
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /v1/communities = %d %q", rec.Code, errorCode(rec))
	}
	cid := idOf(t, decodeArray(t, rec)[0])
	awayCount := func() int {
		t.Helper()
		rec := call(t, h, http.MethodGet, "/v1/communities/"+cid.String()+"/afk", owner, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET afk = %d %q", rec.Code, errorCode(rec))
		}
		snap := decodeArray(t, rec)
		if len(snap) != 5 {
			t.Fatalf("AFK snapshot has %d elements, want 5", len(snap))
		}
		list, _ := snap[4].([]any)
		return len(list)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	tab1 := dialReady(t, ctx, ts, owner)
	tab2 := dialReady(t, ctx, ts, owner)
	if rec := call(t, h, http.MethodPut, "/v1/communities/"+cid.String()+"/afk/away", owner, []any{}); rec.Code != http.StatusNoContent {
		t.Fatalf("PUT afk/away = %d %q", rec.Code, errorCode(rec))
	}
	if awayCount() != 1 {
		t.Fatal("PUT did not list the owner as away")
	}
	_ = tab1.CloseNow()
	time.Sleep(100 * time.Millisecond)
	clk.Advance(31 * time.Second)
	time.Sleep(100 * time.Millisecond)
	if awayCount() != 1 {
		t.Fatal("closing one of two tabs cleared away")
	}
	_ = tab2.CloseNow()
	closed := clk.Now()
	time.Sleep(100 * time.Millisecond)
	if awayCount() != 1 {
		t.Fatal("closing the last tab cleared away at once; a reload must keep it")
	}
	advanceUntil(t, clk, time.Second, func() bool { return awayCount() == 0 })
	if waited := clk.Since(closed); waited < 30*time.Second {
		t.Fatalf("away cleared %v after the last close, want at least 30s", waited)
	}
}
