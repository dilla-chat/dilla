package dillad_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/livekit/protocol/livekit"

	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/dillad"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/sfu"
)

// plan2_test.go is Plan 2's wiring through the composition root (task 19): every handler group
// Plan 2 built in internal/api is mounted by dillad.New behind the session middleware and the
// [limits.rate] meter, with the real delivery service, gateway, blob store and franking keys
// behind it. A route that exists in protocol/09 and not in the binary answers 404 from the mux;
// these tests exist so that can never ship again.

// planTwoRoutes is one route of every Plan 2 handler group, as protocol/09 lists them.
func planTwoRoutes() []struct{ group, method, path string } {
	const x = "0102030405060708090a0b0c0d0e0f10"
	const blobID = "00000000000000000000000000000000000000000000000000000000000000aa"
	return []struct{ group, method, path string }{
		{"communities", http.MethodPost, "/v1/communities"},
		{"communities", http.MethodGet, "/v1/communities"},
		{"communities", http.MethodGet, "/v1/communities/" + x},
		{"communities", http.MethodPatch, "/v1/communities/" + x},
		{"communities", http.MethodDelete, "/v1/communities/" + x},
		{"communities", http.MethodGet, "/v1/communities/" + x + "/members"},
		{"communities", http.MethodDelete, "/v1/communities/" + x + "/members/" + x},
		{"communities", http.MethodPost, "/v1/communities/" + x + "/join"},
		{"communities", http.MethodPost, "/v1/communities/" + x + "/leave"},
		{"channels", http.MethodPost, "/v1/communities/" + x + "/channels"},
		{"channels", http.MethodGet, "/v1/communities/" + x + "/channels"},
		{"channels", http.MethodGet, "/v1/channels/" + x},
		{"channels", http.MethodPatch, "/v1/channels/" + x},
		{"channels", http.MethodDelete, "/v1/channels/" + x},
		{"channel members", http.MethodGet, "/v1/channels/" + x + "/members"},
		{"channel members", http.MethodPut, "/v1/channels/" + x + "/members/" + x},
		{"channel members", http.MethodDelete, "/v1/channels/" + x + "/members/" + x},
		{"roles", http.MethodPost, "/v1/communities/" + x + "/roles"},
		{"roles", http.MethodPatch, "/v1/roles/" + x},
		{"roles", http.MethodDelete, "/v1/roles/" + x},
		{"roles", http.MethodPut, "/v1/communities/" + x + "/members/" + x + "/roles/" + x},
		{"roles", http.MethodDelete, "/v1/communities/" + x + "/members/" + x + "/roles/" + x},
		{"roles", http.MethodPut, "/v1/channels/" + x + "/overwrites/0/" + x},
		{"roles", http.MethodDelete, "/v1/channels/" + x + "/overwrites/0/" + x},
		{"bans", http.MethodPut, "/v1/communities/" + x + "/bans/" + x},
		{"bans", http.MethodDelete, "/v1/communities/" + x + "/bans/" + x},
		{"bans", http.MethodGet, "/v1/communities/" + x + "/bans"},
		{"invites", http.MethodPost, "/v1/communities/" + x + "/invites"},
		{"invites", http.MethodGet, "/v1/communities/" + x + "/invites"},
		{"invites", http.MethodDelete, "/v1/invites/" + x},
		{"dms", http.MethodPost, "/v1/dms"},
		{"dms", http.MethodGet, "/v1/dms"},
		{"readable", http.MethodPost, "/v1/channels/" + x + "/messages"},
		{"readable", http.MethodGet, "/v1/channels/" + x + "/messages"},
		{"readable", http.MethodPatch, "/v1/channels/" + x + "/messages/1"},
		{"readable", http.MethodDelete, "/v1/channels/" + x + "/messages/1"},
		{"readable", http.MethodGet, "/v1/channels/" + x + "/search?q=a"},
		{"readable", http.MethodPut, "/v1/channels/" + x + "/read-state"},
		{"blobs", http.MethodPut, "/v1/channels/" + x + "/blobs/" + blobID},
		{"blobs", http.MethodGet, "/v1/channels/" + x + "/blobs/" + blobID},
		{"blobs", http.MethodDelete, "/v1/channels/" + x + "/blobs/" + blobID},
		{"reports", http.MethodPost, "/v1/reports"},
		{"reports", http.MethodGet, "/v1/reports"},
		{"reports", http.MethodPatch, "/v1/reports/" + x},
		{"calls", http.MethodPost, "/v1/channels/" + x + "/calls"},
		{"calls", http.MethodDelete, "/v1/calls/" + x},
		{"calls", http.MethodPost, "/v1/calls/" + x + "/share"},
		{"calls", http.MethodDelete, "/v1/calls/" + x + "/share"},
		{"calls", http.MethodPost, "/v1/calls/" + x + "/stats"},
		{"admin", http.MethodDelete, "/v1/admin/blobs/" + blobID},
		{"admin", http.MethodGet, "/v1/admin/audit"},
		{"admin", http.MethodPost, "/v1/admin/users/" + x + "/disable"},
		{"admin", http.MethodGet, "/v1/admin/diagnostics"},
	}
}

// call is one request through the composition root's handler. body is CBOR-encoded unless it is
// already a []byte, which is sent as application/octet-stream (a blob upload).
func call(t *testing.T, h http.Handler, method, path, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var raw []byte
	ctype := "application/cbor"
	switch b := body.(type) {
	case nil:
	case []byte:
		raw, ctype = b, "application/octet-stream"
	default:
		var err error
		if raw, err = cborx.Marshal(b); err != nil {
			t.Fatalf("marshal %s %s: %v", method, path, err)
		}
	}
	req := httptest.NewRequestWithContext(t.Context(), method, path, bytes.NewReader(raw))
	if raw != nil {
		req.Header.Set("Content-Type", ctype)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// errorCode is element 0 of a /v1 error body, or "" when the body is not one — the stdlib mux's
// own 404 is text/plain, which is how an unmounted route is told apart from a handler's 404.
func errorCode(rec *httptest.ResponseRecorder) string {
	if !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/cbor") {
		return ""
	}
	var e []any
	if err := cborx.Unmarshal(rec.Body.Bytes(), &e); err != nil || len(e) == 0 {
		return ""
	}
	code, _ := e[0].(string)
	return code
}

// Every Plan 2 route is MOUNTED, behind the session middleware: without a bearer token each answers
// 401 E_UNAUTHENTICATED from the middleware, never the mux's 404.
func TestEveryPlanTwoRouteIsMountedBehindTheSessionMiddleware(t *testing.T) {
	_, h, _ := newInstance(t)
	for _, r := range planTwoRoutes() {
		rec := call(t, h, r.method, r.path, "", nil)
		if rec.Code != http.StatusUnauthorized || errorCode(rec) != "E_UNAUTHENTICATED" {
			t.Errorf("%s: %s %s = %d %q (%q), want 401 E_UNAUTHENTICATED",
				r.group, r.method, r.path, rec.Code, errorCode(rec), rec.Body.String())
		}
	}
}

// decodeArray decodes a fixed-position CBOR array response.
func decodeArray(t *testing.T, rec *httptest.ResponseRecorder) []any {
	t.Helper()
	var out []any
	if err := cborx.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %x: %v", rec.Body.Bytes(), err)
	}
	return out
}

func idOf(t *testing.T, v any) id.ID {
	t.Helper()
	b, ok := v.([]byte)
	if !ok || len(b) != len(id.ID{}) {
		t.Fatalf("%#v is not a 16-byte id", v)
	}
	return id.ID(b)
}

// The integrated Plan 2 flow, through the production composition root with a real session: the
// bootstrap account (an instance admin) creates a community, a server-readable text channel and a
// role, mints an invite, posts a readable message, uploads and fetches an attachment, reports the
// message — which verifies only if the franking keys the Readable and Reports groups hold are the
// instance's own, read from instances.key_history — lists its DMs, asks for a call on a text
// channel, and reads the admin audit log and the diagnostics report. Each step reaches its handler:
// a 2xx, or the handler's own CBOR refusal, and never the mux's 404 or a 501.
func TestThePlanTwoFlowRunsThroughTheCompositionRoot(t *testing.T) {
	srv, h, code := newInstance(t)
	tok := accountToken(t, srv, code)
	must := func(rec *httptest.ResponseRecorder, want int, what string) *httptest.ResponseRecorder {
		t.Helper()
		if rec.Code != want {
			t.Fatalf("%s = %d %q (%x), want %d", what, rec.Code, errorCode(rec), rec.Body.Bytes(), want)
		}
		return rec
	}

	created := decodeArray(t, must(call(t, h, http.MethodPost, "/v1/communities", tok,
		[]any{"lounge", []byte("{}"), uint64(0), uint64(0)}), http.StatusCreated, "POST /v1/communities"))
	cid := idOf(t, created[0])
	must(call(t, h, http.MethodGet, "/v1/communities/"+cid.String(), tok, nil), http.StatusOK, "GET community")
	must(call(t, h, http.MethodGet, "/v1/communities/"+cid.String()+"/members", tok, nil), http.StatusOK, "GET members")
	mine := decodeArray(t, must(call(t, h, http.MethodGet, "/v1/communities", tok, nil), http.StatusOK, "GET /v1/communities"))
	if len(mine) != 1 {
		t.Fatalf("GET /v1/communities = %#v, want the one community this account created", mine)
	}
	if row, ok := mine[0].([]any); !ok || len(row) != 4 || idOf(t, row[0]) != cid || row[1] != "lounge" || row[3] != uint64(1) {
		t.Fatalf("GET /v1/communities row = %#v, want [%s, \"lounge\", owner, 1]", mine[0], cid)
	}

	// kind text (0), mode readable (1), visibility discoverable (2).
	channel := decodeArray(t, must(call(t, h, http.MethodPost, "/v1/communities/"+cid.String()+"/channels", tok,
		[]any{uint64(0), uint64(1), uint64(2), nil, "general", "", uint64(0), uint64(0)}),
		http.StatusCreated, "POST channel"))
	ch := idOf(t, channel[0])
	chPath := "/v1/channels/" + ch.String()
	if doc := decodeArray(t, must(call(t, h, http.MethodGet, chPath, tok, nil), http.StatusOK, "GET channel")); len(doc) != 12 || doc[11] != nil {
		t.Fatalf("GET channel = %#v, want twelve elements with text_group_id null for a server-readable channel", doc)
	}
	must(call(t, h, http.MethodGet, chPath+"/members", tok, nil), http.StatusOK, "GET channel members")

	must(call(t, h, http.MethodPost, "/v1/communities/"+cid.String()+"/roles", tok,
		[]any{"mods", uint64(0), uint64(1), uint64(1 << 1), uint64(0), uint64(0), uint64(0)}),
		http.StatusCreated, "POST role")
	must(call(t, h, http.MethodGet, "/v1/communities/"+cid.String()+"/bans", tok, nil), http.StatusOK, "GET bans")
	must(call(t, h, http.MethodPost, "/v1/communities/"+cid.String()+"/invites", tok,
		[]any{uint64(5), uint64(3600), uint64(0)}), http.StatusCreated, "POST community invite")

	kf := bytes.Repeat([]byte{0x06}, 32)
	envelope, err := cborx.Marshal([]any{uint64(1), id.New(), uint64(0), nil, nil, "hello, lounge",
		[]any{}, []any{}, kf})
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	posted := decodeArray(t, must(call(t, h, http.MethodPost, chPath+"/messages", tok, []any{envelope}),
		http.StatusOK, "POST readable message"))
	seq, ok := posted[0].(uint64)
	if !ok {
		t.Fatalf("POST message answered %#v", posted)
	}
	must(call(t, h, http.MethodGet, chPath+"/messages", tok, nil), http.StatusOK, "GET readable messages")

	payload := bytes.Repeat([]byte{0x5a}, 513)
	sum := sha256.Sum256(payload)
	blobPath := chPath + "/blobs/" + hex.EncodeToString(sum[:])
	must(call(t, h, http.MethodPut, blobPath, tok, payload), http.StatusCreated, "PUT blob")
	if got := must(call(t, h, http.MethodGet, blobPath, tok, nil), http.StatusOK, "GET blob"); !bytes.Equal(got.Body.Bytes(), payload) {
		t.Fatalf("GET blob returned %d bytes, want the %d uploaded", got.Body.Len(), len(payload))
	}

	report := decodeArray(t, must(call(t, h, http.MethodPost, "/v1/reports", tok, []any{ch, seq, envelope, kf}),
		http.StatusCreated, "POST report"))
	if report[1] != "verified" {
		t.Fatalf("the report of a message this instance franked verified as %q: the franking keys "+
			"the report route holds are not the ones the readable route franked under", report[1])
	}
	must(call(t, h, http.MethodGet, "/v1/reports", tok, nil), http.StatusOK, "GET reports")

	must(call(t, h, http.MethodGet, "/v1/dms", tok, nil), http.StatusOK, "GET dms")

	// A call on a text channel is the call route's own 400, not the mux's 404.
	if rec := call(t, h, http.MethodPost, chPath+"/calls", tok, []any{}); rec.Code != http.StatusBadRequest || errorCode(rec) == "" {
		t.Fatalf("POST calls on a text channel = %d %q, want the handler's 400", rec.Code, errorCode(rec))
	}

	must(call(t, h, http.MethodGet, "/v1/admin/audit", tok, nil), http.StatusOK, "GET admin audit")
	legs := decodeArray(t, must(call(t, h, http.MethodGet, "/v1/admin/diagnostics", tok, nil),
		http.StatusOK, "GET admin diagnostics"))
	status := map[string]uint64{}
	for _, l := range legs {
		leg, ok := l.([]any)
		if !ok || len(leg) != 4 {
			t.Fatalf("a diagnostics leg is %#v, want [name, status, detail, fix]", l)
		}
		name, _ := leg[0].(string)
		status[name], _ = leg[1].(uint64)
	}
	// t.TempDir is 0755 and nothing reflects UDP here, so data_dir and udp only have to be present;
	// the database, the wasi core and the blob just uploaded are this process's own and must be OK.
	// webauthn (web3.6) only has to be present: its status is the test config's rp_origins.
	for _, name := range []string{"database", "data_dir", "wasi", "udp", "blobs", "calls", "turn_relay", "webauthn"} {
		st, ok := status[name]
		if !ok || (st != 0 && name != "data_dir" && name != "udp" && name != "webauthn") {
			t.Errorf("diagnostics leg %q: present %v, status %d (%v)", name, ok, st, legs)
		}
	}
}

type upstreamSFU struct{ url string }

func (u upstreamSFU) Token(room, identity string, _ *livekit.ParticipantPermission, _ map[string]string) (string, error) {
	return room + "/" + identity, nil
}
func (u upstreamSFU) DeleteRoom(context.Context, string) error { return nil }
func (u upstreamSFU) CreateRoom(context.Context, string) error { return nil }
func (u upstreamSFU) UpdatePermission(context.Context, string, string, *livekit.ParticipantPermission) error {
	return nil
}
func (u upstreamSFU) RemoveParticipants(context.Context, string, id.ID) error { return nil }
func (u upstreamSFU) RemoveParticipant(context.Context, string, string) error { return nil }
func (u upstreamSFU) Participants(context.Context, string) ([]*livekit.ParticipantInfo, error) {
	return nil, nil
}
func (u upstreamSFU) Rooms(context.Context) ([]string, error) { return nil, nil }
func (u upstreamSFU) HTTPURL() string                         { return u.url }
func (u upstreamSFU) VerifyToken(string) (sfu.RoomToken, error) {
	return sfu.RoomToken{}, errors.New("upstreamSFU verifies nothing")
}

// With an SFU (`dillad serve` with livekit.enabled), New mounts LiveKit's signalling paths on the
// instance's own origin; without one they are not routes at all.
func TestTheRTCPathsAreMountedOnlyWithAnSFU(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))
	defer upstream.Close()
	cfg := testConfig(t)
	srv, err := dillad.New(context.Background(), dillad.Options{
		Config: cfg, Clock: clock.System(), Wasm: sharedRuntime(t), SFU: upstreamSFU{url: upstream.URL},
	})
	if err != nil {
		t.Fatalf("dillad.New: %v", err)
	}
	defer srv.Shutdown(context.Background())
	if rec := call(t, srv.Handler(), http.MethodGet, "/rtc/validate?access_token=x", "", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("GET /rtc/validate with a token nobody minted = %d, want the join gate's 403 (the route exists)", rec.Code)
	}
	_, h, _ := newInstance(t)
	if rec := call(t, h, http.MethodGet, "/rtc/validate", "", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("GET /rtc/validate with no SFU = %d, want 404", rec.Code)
	}
}

// Fix wave I19: POST /v1/communities/{id}/join is metered on the ("invite", client address) bucket
// GET /i/{code} is on, so a join is no way round the limit on guessing codes: one address is
// refused 429 after invite_burst joins, long before the per-device write burst would refuse it.
func TestTheJoinIsMeteredOnTheInviteBucket(t *testing.T) {
	srv, h, code := newInstance(t)
	tok := accountToken(t, srv, code)
	rate := testConfig(t).Limits.Rate
	if rate.InviteBurst >= rate.WriteBurst {
		t.Fatalf("invite burst %d is not below the write burst %d: the test could not tell the buckets apart",
			rate.InviteBurst, rate.WriteBurst)
	}
	path := "/v1/communities/" + id.New().String() + "/join"
	for i := range rate.WriteBurst {
		rec := call(t, h, http.MethodPost, path, tok, []any{nil})
		if rec.Code != http.StatusTooManyRequests {
			continue
		}
		if i < rate.InviteBurst {
			t.Fatalf("join %d of an invite burst of %d was refused", i+1, rate.InviteBurst)
		}
		if errorCode(rec) != "E_RATE_LIMITED" {
			t.Fatalf("429 carried %q, want E_RATE_LIMITED", errorCode(rec))
		}
		return
	}
	t.Fatalf("%d joins from one address were never refused; the invite burst is %d", rate.WriteBurst, rate.InviteBurst)
}

// Plan 2's routes are metered on the instance's [limits.rate] buckets, per device session: past the
// read burst a GET is refused 429 E_RATE_LIMITED, whatever it would have answered.
func TestThePlanTwoRoutesAreMeteredPerDevice(t *testing.T) {
	srv, h, code := newInstance(t)
	tok := accountToken(t, srv, code)
	burst := testConfig(t).Limits.Rate.ReadBurst
	for i := range 2 * burst {
		rec := call(t, h, http.MethodGet, "/v1/dms", tok, nil)
		if rec.Code == http.StatusTooManyRequests {
			if i < burst {
				t.Fatalf("request %d of a read burst of %d was refused", i+1, burst)
			}
			if errorCode(rec) != "E_RATE_LIMITED" {
				t.Fatalf("429 carried %q, want E_RATE_LIMITED", errorCode(rec))
			}
			return
		}
	}
	t.Fatalf("%d reads of GET /v1/dms in a row were never refused; the read burst is %d", 2*burst, burst)
}
