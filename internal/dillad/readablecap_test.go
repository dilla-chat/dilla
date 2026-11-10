package dillad_test

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/dillad"
	"github.com/dilla-chat/dilla/internal/id"
)

// The readable body cap follows limits.max_ciphertext_bytes through the composition root: an
// instance that pins the pre-wave-C 131072 refuses a ten-file envelope (131643 bytes) with 413,
// where the 196608 default takes it. If routes.go stopped passing the configured cap, the readable
// route would fall back to the default and this post would be 200.
func TestTheReadableCapFollowsAConfiguredCiphertextCap(t *testing.T) {
	for _, tc := range []struct {
		name string
		cap  int64
		want int
	}{
		{"pinned at the old 131072", 131072, http.StatusRequestEntityTooLarge},
		{"the default", 0, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, code := testConfigInvite(t)
			cfg.Log.Level = "warn"
			if tc.cap != 0 {
				cfg.Limits.MaxCiphertextBytes = tc.cap
			}
			cfg.Derive()
			srv, err := dillad.New(context.Background(), dillad.Options{Config: cfg, Clock: clock.System(), Wasm: sharedRuntime(t)})
			if err != nil {
				t.Fatalf("dillad.New: %v", err)
			}
			t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
			h := socketHandler(srv)
			owner := accountToken(t, srv, code)
			rec := call(t, h, http.MethodPost, "/v1/communities", owner, []any{"cap", []byte("{}"), uint64(0), uint64(0)})
			if rec.Code != http.StatusCreated {
				t.Fatalf("POST /v1/communities = %d %q", rec.Code, errorCode(rec))
			}
			cid := idOf(t, decodeArray(t, rec)[0])
			rec = call(t, h, http.MethodPost, "/v1/communities/"+cid.String()+"/channels", owner,
				[]any{uint64(api.ChannelText), uint64(api.ModeReadable), uint64(api.VisDiscoverable), nil, "lobby", "", uint64(0), uint64(0)})
			if rec.Code != http.StatusCreated {
				t.Fatalf("POST channel = %d %q", rec.Code, errorCode(rec))
			}
			lobby := idOf(t, decodeArray(t, rec)[0])

			att := []any{bytes.Repeat([]byte{3}, 32), bytes.Repeat([]byte{4}, 32), bytes.Repeat([]byte{5}, 12),
				uint64(1 << 40), strings.Repeat("m", 255), uint64(1 << 20), uint64(1 << 20), bytes.Repeat([]byte{7}, 8192), strings.Repeat("n", 255)}
			prev := []any{strings.Repeat("u", 2048), strings.Repeat("t", 256), strings.Repeat("d", 1024), bytes.Repeat([]byte{8}, 16384)}
			env, err := cborx.Marshal([]any{uint64(1), id.New(), uint64(0), id.New(), id.New(), strings.Repeat("b", 4000),
				[]any{att, att, att, att, att, att, att, att, att, att}, []any{prev, prev}, bytes.Repeat([]byte{6}, 32)})
			if err != nil {
				t.Fatal(err)
			}
			if rec := call(t, h, http.MethodPost, "/v1/channels/"+lobby.String()+"/messages", owner, []any{env}); rec.Code != tc.want {
				t.Fatalf("a %d-byte envelope = %d %q, want %d", len(env), rec.Code, errorCode(rec), tc.want)
			}
		})
	}
}
