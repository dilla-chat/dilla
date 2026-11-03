package dilladtest_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/dilla-chat/dilla/internal/dillad/dilladtest"
	"github.com/dilla-chat/dilla/internal/ds"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

func newHost(t *testing.T) *dilladtest.Host {
	t.Helper()
	core, err := filepath.Abs(filepath.Join("..", "..", "mlswasi", "testdata", "dilla_core_wasi.wasm"))
	if err != nil {
		t.Fatalf("core path: %v", err)
	}
	if _, err := os.Stat(core); err != nil {
		t.Fatalf("%s is missing: build it with\n"+
			"  cargo build -p dilla-core-wasi --target wasm32-wasip1 --release --locked\n"+
			"and copy it there (CI downloads the rust-wasi job's artifact): %v", core, err)
	}
	h, err := dilladtest.NewHost(context.Background(), dilladtest.HostOptions{
		DataDir: t.TempDir(), CorePath: core,
	})
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	t.Cleanup(func() { _ = h.Close(context.Background()) })
	return h
}

// POST /debug/seed creates the account and its devices from key material the caller holds, and
// answers a session token per device that the instance itself resolves — the seeding convenience
// that stays out of `dillad serve`.
func TestTheSeedRouteCreatesAnAccountWhoseSessionsResolve(t *testing.T) {
	h := newHost(t)
	control := httptest.NewServer(dilladtest.ControlHandler(h))
	t.Cleanup(control.Close)

	pub := func() string {
		p, _, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatalf("GenerateKey: %v", err)
		}
		return hex.EncodeToString(p)
	}
	device := id.New()
	req := []dilladtest.SeedRequest{{
		Username: "seeded", Display: "Seeded",
		UMKPub: pub(), SSKPub: pub(), SigUMKSSK: hex.EncodeToString(make([]byte, 64)),
		Devices: []dilladtest.Device{{
			DeviceID: device.String(), DSKPub: pub(), Credential: hex.EncodeToString([]byte{0x01}),
		}},
	}}
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	res, err := httpPost(t, control.URL+"/debug/seed", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /debug/seed: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	var out []dilladtest.SeedResponse
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out) != 1 || out[0].Tokens[device.String()] == "" {
		t.Fatalf("got %+v, want one account with a token for the seeded device", out)
	}
	session, err := h.Server().Sessions().Resolve(context.Background(), out[0].Tokens[device.String()])
	if err != nil {
		t.Fatalf("the seeded token does not resolve: %v", err)
	}
	if session.DeviceID != device || session.UserID.String() != out[0].UserID {
		t.Fatalf("the session names %s/%s, want the seeded device and user", session.UserID, session.DeviceID)
	}
	// Fixture reachability (web3.6 S1): a seeded human owes a second factor, as a sign-up does.
	if !session.FactorOwed {
		t.Fatal("a seeded human account does not owe a second factor; a real sign-up does")
	}
}

// postJSON posts v to the control listener and answers the status.
func postJSON(t *testing.T, url string, v any) int {
	t.Helper()
	body, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	res, err := httpPost(t, url, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer res.Body.Close()
	return res.StatusCode
}

// seedDevice creates one account with one device through SeedUsers and answers the device and
// its session token.
func seedDevice(t *testing.T, h *dilladtest.Host, username string) (id.ID, string) {
	t.Helper()
	pub := func() string {
		p, _, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatalf("GenerateKey: %v", err)
		}
		return hex.EncodeToString(p)
	}
	device := id.New()
	out, err := dilladtest.SeedUsers(context.Background(), h.Server(), []dilladtest.SeedRequest{{
		Username: username, Display: username,
		UMKPub: pub(), SSKPub: pub(), SigUMKSSK: hex.EncodeToString(make([]byte, 64)),
		Devices: []dilladtest.Device{{
			DeviceID: device.String(), DSKPub: pub(), Credential: hex.EncodeToString([]byte{0x01}),
		}},
	}})
	if err != nil {
		t.Fatalf("SeedUsers: %v", err)
	}
	return device, out[0].Tokens[device.String()]
}

// POST /debug/mark-revoked revokes the device ROW and nothing else: its session still resolves.
// That is the window a real revocation — which deletes the sessions in the same transaction — can
// race, where a request already authenticated reaches the delivery service after the row changed.
// Invariant 4's external-joiner clause refuses a revoked joiner, and this is the only way a
// scenario can reach that refusal.
func TestTheMarkRevokedRouteRevokesTheRowAndLeavesTheSession(t *testing.T) {
	h := newHost(t)
	control := httptest.NewServer(dilladtest.ControlHandler(h))
	t.Cleanup(control.Close)
	device, token := seedDevice(t, h, "revoked")

	if status := postJSON(t, control.URL+"/debug/mark-revoked",
		map[string]string{"device": device.String()}); status != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", status)
	}
	row, err := h.Server().Repo().GetDevice(context.Background(), device)
	if err != nil {
		t.Fatalf("GetDevice: %v", err)
	}
	if row.RevokedAt == nil {
		t.Fatal("the device row is not revoked")
	}
	if _, err := h.Server().Sessions().Resolve(context.Background(), token); err != nil {
		t.Fatalf("the session was deleted with the row: %v", err)
	}
	if status := postJSON(t, control.URL+"/debug/mark-revoked",
		map[string]string{"device": "zz"}); status != http.StatusBadRequest {
		t.Fatalf("a malformed device id: status = %d, want 400", status)
	}
}

// POST /debug/channel records a channel's visibility and mode under its target id, which is what
// invariant 1's registration check reads (ds.Channels). A target the route never named is no
// channel at all — a DM or a pairing group — exactly as ds.PermissiveChannels answers it.
func TestTheChannelRouteRecordsWhatInvariantOneReads(t *testing.T) {
	h := newHost(t)
	control := httptest.NewServer(dilladtest.ControlHandler(h))
	t.Cleanup(control.Close)
	target := id.New()

	for _, c := range []struct {
		visibility, mode string
		wantV, wantM     uint8
	}{
		{"private", "e2ee", 0, 0},
		{"private", "readable", 0, 1},
		{"invite", "readable", 1, 1},
		{"discoverable", "readable", 2, 1},
	} {
		if status := postJSON(t, control.URL+"/debug/channel", map[string]string{
			"target": target.String(), "visibility": c.visibility, "mode": c.mode,
		}); status != http.StatusNoContent {
			t.Fatalf("%s/%s: status = %d, want 204", c.visibility, c.mode, status)
		}
		v, m, err := h.Channels().Channel(context.Background(), target)
		if err != nil || v != c.wantV || m != c.wantM {
			t.Fatalf("%s/%s: Channel = %d, %d, %v; want %d, %d", c.visibility, c.mode, v, m, err,
				c.wantV, c.wantM)
		}
	}
	if _, _, err := h.Channels().Channel(context.Background(), id.New()); !errors.Is(err, ds.ErrNoChannel) {
		t.Fatalf("an unnamed target: %v, want ds.ErrNoChannel", err)
	}
	for _, bad := range []map[string]string{
		{"target": target.String(), "visibility": "secret", "mode": "e2ee"},
		{"target": target.String(), "visibility": "private", "mode": "plain"},
		{"target": "zz", "visibility": "private", "mode": "e2ee"},
	} {
		if status := postJSON(t, control.URL+"/debug/channel", bad); status != http.StatusBadRequest {
			t.Fatalf("%v: status = %d, want 400", bad, status)
		}
	}
}

// `channel … members=` also writes the channel row, community-less, and its channel_members: the
// shape api.SyncRegisteredGroup reads when the channel's text group is registered, so a scenario
// can drive "creating a private channel" (protocol/01 § Joining) through the wired instance. An
// empty members list writes neither, as before.
func TestTheChannelRouteWritesTheChannelAndItsMembersWhenNamed(t *testing.T) {
	h := newHost(t)
	control := httptest.NewServer(dilladtest.ControlHandler(h))
	t.Cleanup(control.Close)
	ctx := context.Background()
	repo := h.Server().Repo()
	var users []string
	var want []id.ID
	for _, name := range []string{"alice", "bob"} {
		u := id.New()
		if err := repo.CreateUser(ctx, store.UserRow{
			ID: u, Username: name, Display: name, UMKPub: make([]byte, 32), SSKPub: make([]byte, 32),
			SigUMKSSK: make([]byte, 64), Created: 1,
		}); err != nil {
			t.Fatalf("CreateUser: %v", err)
		}
		users = append(users, u.String())
		want = append(want, u)
	}

	bare := id.New()
	if status := postJSON(t, control.URL+"/debug/channel", map[string]any{
		"target": bare.String(), "visibility": "private", "mode": "e2ee", "members": []string{},
	}); status != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", status)
	}
	if _, err := repo.GetChannel(ctx, bare); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a channel named without members got a row: %v", err)
	}

	target := id.New()
	for range 2 { // naming it twice is not a conflict
		if status := postJSON(t, control.URL+"/debug/channel", map[string]any{
			"target": target.String(), "visibility": "private", "mode": "e2ee", "members": users,
		}); status != http.StatusNoContent {
			t.Fatalf("status = %d, want 204", status)
		}
	}
	ch, err := repo.GetChannel(ctx, target)
	if err != nil {
		t.Fatalf("GetChannel: %v", err)
	}
	if ch.CommunityID != nil || ch.Visibility != dilladtest.VisibilityPrivate || ch.Mode != dilladtest.ModeE2EE {
		t.Fatalf("channel row = %+v, want a community-less private e2ee channel", ch)
	}
	got, err := repo.ListChannelMembers(ctx, target)
	if err != nil {
		t.Fatalf("ListChannelMembers: %v", err)
	}
	slices.SortFunc(got, func(a, b id.ID) int { return bytes.Compare(a[:], b[:]) })
	slices.SortFunc(want, func(a, b id.ID) int { return bytes.Compare(a[:], b[:]) })
	if !slices.Equal(got, want) {
		t.Fatalf("channel_members = %v, want %v", got, want)
	}
	if v, m, err := h.Channels().Channel(ctx, target); err != nil || v != 0 || m != 0 {
		t.Fatalf("Channel = %d, %d, %v; invariant 1's source must still record it", v, m, err)
	}
	if status := postJSON(t, control.URL+"/debug/channel", map[string]any{
		"target": id.New().String(), "visibility": "private", "mode": "e2ee", "members": []string{"zz"},
	}); status != http.StatusBadRequest {
		t.Fatalf("a malformed member id: status = %d, want 400", status)
	}
}

// GET /debug/state carries the counters and the three things scenarios read: the two lists and the
// external-sender key every text and call group needs.
func TestTheStateRouteCarriesTheListsAndTheExternalSenderKey(t *testing.T) {
	h := newHost(t)
	control := httptest.NewServer(dilladtest.ControlHandler(h))
	t.Cleanup(control.Close)

	res, err := httpGet(t, control.URL+"/debug/state")
	if err != nil {
		t.Fatalf("GET /debug/state: %v", err)
	}
	defer res.Body.Close()
	var state map[string]any
	if err := json.NewDecoder(res.Body).Decode(&state); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, key := range []string{"quarantined_devices", "closed_groups"} {
		if _, ok := state[key].([]any); !ok {
			t.Errorf("%s is %T, want a JSON array", key, state[key])
		}
	}
	key, _ := state["external_sender_pub"].(string)
	if raw, err := hex.DecodeString(key); err != nil || len(raw) != ed25519.PublicKeySize {
		t.Errorf("external_sender_pub = %q, want 64 hex digits", key)
	}
	if _, ok := state["now_unix"].(float64); !ok {
		t.Error("now_unix is missing")
	}
}

// GET /debug/conn answers both sides of one device in one group; for a device and a group the
// instance has never seen, that is no connection, no fan-out membership and no group. A malformed
// id is a 400.
func TestTheConnRouteReportsBothSidesOfADeviceInAGroup(t *testing.T) {
	h := newHost(t)
	control := httptest.NewServer(dilladtest.ControlHandler(h))
	t.Cleanup(control.Close)

	device, group := id.New(), id.New()
	res, err := httpGet(t, control.URL+"/debug/conn?device="+device.String()+"&group="+group.String())
	if err != nil {
		t.Fatalf("GET /debug/conn: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	var report dilladtest.ConnReport
	if err := json.NewDecoder(res.Body).Decode(&report); err != nil {
		t.Fatalf("decode: %v", err)
	}
	gw := report.Gateway
	if gw.Device != device.String() || gw.Group != group.String() || len(gw.Conns) != 0 ||
		gw.InMembers || gw.Leaf != nil {
		t.Errorf("gateway side = %+v, want the ids echoed and nothing else", gw)
	}
	if report.DS.Found || len(report.DS.Leaves) != 0 {
		t.Errorf("ds side = %+v, want an unknown group", report.DS)
	}

	bad, err := httpGet(t, control.URL+"/debug/conn?device=zz&group="+group.String())
	if err != nil {
		t.Fatalf("GET /debug/conn: %v", err)
	}
	defer bad.Body.Close()
	if bad.StatusCode != http.StatusBadRequest {
		t.Fatalf("a malformed device id: status = %d, want 400", bad.StatusCode)
	}
}

// httpGet is http.Get bound to the test's context.
func httpGet(t *testing.T, url string) (*http.Response, error) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	return http.DefaultClient.Do(req)
}

// httpPost is http.Post bound to the test's context.
func httpPost(t *testing.T, url, contentType string, body io.Reader) (*http.Response, error) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url, body)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", contentType)
	return http.DefaultClient.Do(req)
}
