package dillad_test

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/dillad"
)

// namedAccount registers username with code and returns its first device session, the account's
// second factor satisfied.
func namedAccount(t *testing.T, srv *dillad.Server, code, username string) string {
	t.Helper()
	rec := call(t, srv.Handler(), http.MethodPost, "/v1/accounts", "", newAccountRequest(code, username, newTestDevice(t)))
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /v1/accounts %s = %d %q", username, rec.Code, errorCode(rec))
	}
	out := decodeArray(t, rec)
	satisfyFactor(t, srv, idOf(t, out[0]))
	token, _ := out[2].(string)
	return token
}

// nextOp reads frames until one with op arrives and returns its payload, or reports false when the
// connection yields nothing more before ctx ends.
func nextOp(ctx context.Context, c *websocket.Conn, op uint64) ([]any, bool) {
	for {
		typ, b, err := c.Read(ctx)
		if err != nil || typ != websocket.MessageBinary {
			return nil, false
		}
		var frame []any
		if cborx.Unmarshal(b, &frame) != nil || len(frame) != 4 {
			return nil, false
		}
		if got, _ := frame[0].(uint64); got == op {
			payload, _ := frame[3].([]any)
			return payload, true
		}
	}
}

// dilla-web-3.1 (WB walk, flagged item): a member's open tab learns of a new channel. Through the
// composition root, a channel create sends op 54 community_changed [community_id] to every live
// gateway session of every member, and to nobody outside the community.
func TestAChannelCreateTellsEveryMemberAndNoOneElse(t *testing.T) {
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
	stranger := namedAccount(t, srv, mintInvite(t, srv), "stranger")

	rec := call(t, h, http.MethodPost, "/v1/communities", owner, []any{"fika", []byte("{}"), uint64(0), uint64(0)})
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /v1/communities = %d %q", rec.Code, errorCode(rec))
	}
	cid := idOf(t, decodeArray(t, rec)[0])
	if rec := call(t, h, http.MethodPost, "/v1/communities/"+cid.String()+"/join", member, []any{nil}); rec.Code != http.StatusOK {
		t.Fatalf("join = %d %q", rec.Code, errorCode(rec))
	}

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	mc := dialReady(t, ctx, ts, member)
	sc := dialReady(t, ctx, ts, stranger)

	rec = call(t, h, http.MethodPost, "/v1/communities/"+cid.String()+"/channels", owner,
		[]any{uint64(api.ChannelText), uint64(0), uint64(0), nil, "planering", "", uint64(0), uint64(0)})
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST channel = %d %q", rec.Code, errorCode(rec))
	}

	payload, ok := nextOp(ctx, mc, 54)
	if !ok {
		t.Fatal("the member's gateway session received no community_changed for the new channel")
	}
	if len(payload) != 1 {
		t.Fatalf("community_changed payload %#v, want [community_id]", payload)
	}
	if got, _ := payload[0].([]byte); !bytes.Equal(got, cid[:]) {
		t.Fatalf("community_changed names %x, want %x", payload[0], cid[:])
	}
	// The member's frame was queued before the create answered, and the stranger's would have been
	// queued beside it, so a short wait is enough to see that none was.
	quiet, stop := context.WithTimeout(ctx, 300*time.Millisecond)
	defer stop()
	if _, got := nextOp(quiet, sc, 54); got {
		t.Fatal("a non-member's gateway session received community_changed")
	}
	if quiet.Err() == nil || !errors.Is(quiet.Err(), context.DeadlineExceeded) {
		t.Fatalf("the non-member's connection ended early: %v", quiet.Err())
	}
}
