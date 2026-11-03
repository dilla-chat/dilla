package dilladtest

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/livekit/protocol/auth"
	lksdk "github.com/livekit/server-sdk-go/v2"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

// freePorts asks the kernel for an unused TCP port and an unused UDP port on loopback, so this
// package's SFU never collides with internal/sfu's fixed test ports when `go test ./...` runs the
// packages in parallel.
func freePorts(t *testing.T) (tcp, udp int) {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen tcp: %v", err)
	}
	tcp = ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	pc, err := lc.ListenPacket(t.Context(), "udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	udp = pc.LocalAddr().(*net.UDPAddr).Port
	_ = pc.Close()
	return tcp, udp
}

// corePath is the committed wasi core the hosts here run.
func corePath(t *testing.T) string {
	t.Helper()
	core, err := filepath.Abs(filepath.Join("..", "..", "mlswasi", "testdata", "dilla_core_wasi.wasm"))
	if err != nil {
		t.Fatalf("core path: %v", err)
	}
	if _, err := os.Stat(core); err != nil {
		t.Fatalf("%s is missing: build it with\n"+
			"  cargo build -p dilla-core-wasi --target wasm32-wasip1 --release --locked\n"+
			"and copy it there: %v", core, err)
	}
	return core
}

func sfuHost(t *testing.T, withSFU bool) *Host {
	t.Helper()
	core := corePath(t)
	tcp, udp := freePorts(t)
	h, err := NewHost(context.Background(), HostOptions{
		DataDir: t.TempDir(), CorePath: core, SFU: withSFU, SFUPort: tcp, SFUUDPPort: udp,
	})
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	t.Cleanup(func() { _ = h.Close(context.Background()) })
	return h
}

// RIGS-02: the host's /metrics answers the scrape token it was given (dilla-testhost passes
// DILLA_METRICS_TOKEN), so the capacity rig's LAN and VPS-hop legs can read the SFU counters, and
// refuses a scrape without it. Without a token the host keeps dillad's refusal of every scrape.
func TestTheHostServesMetricsToItsScrapeTokenOnly(t *testing.T) {
	core, err := filepath.Abs(filepath.Join("..", "..", "mlswasi", "testdata", "dilla_core_wasi.wasm"))
	if err != nil {
		t.Fatalf("core path: %v", err)
	}
	const token = "rig-scrape-token-0123456789"
	for _, tc := range []struct {
		hostToken string
		want      map[string]int // bearer -> status
	}{
		{token, map[string]int{"": http.StatusUnauthorized, "wrong": http.StatusUnauthorized, token: http.StatusOK}},
		{"", map[string]int{"": http.StatusUnauthorized, token: http.StatusUnauthorized}},
	} {
		h, err := NewHost(context.Background(), HostOptions{DataDir: t.TempDir(), CorePath: core, ScrapeToken: tc.hostToken})
		if err != nil {
			t.Fatalf("NewHost: %v", err)
		}
		public := httptest.NewServer(h.Handler())
		for bearer, want := range tc.want {
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, public.URL+"/metrics", nil)
			if err != nil {
				t.Fatal(err)
			}
			if bearer != "" {
				req.Header.Set("Authorization", "Bearer "+bearer)
			}
			res, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("GET /metrics: %v", err)
			}
			_ = res.Body.Close()
			if res.StatusCode != want {
				t.Errorf("host token %q, bearer %q: %d, want %d", tc.hostToken, bearer, res.StatusCode, want)
			}
		}
		public.Close()
		_ = h.Close(context.Background())
	}
}

// Task-10 review M9: a webhook never reaches an instance that is shutting down. Close stops the SFU and its receiver
// first and leaves no current instance; a Restore drops the current instance before it shuts the old one down, so a
// restore that fails after the shutdown leaves none either (events in that window are dropped, not handed to a closed
// repository).
func TestNoWebhookReachesAnInstanceThatIsShuttingDown(t *testing.T) {
	h := sfuHost(t, false)
	if h.current.Load() == nil {
		t.Fatal("a running host has no current instance")
	}
	path, err := h.snapshotPath("broken")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("not an archive"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := h.Restore(t.Context(), "broken"); err == nil {
		t.Fatal("a restore of a broken archive succeeded")
	}
	if h.current.Load() != nil {
		t.Fatal("a restore that failed after the shutdown left the shut-down instance current")
	}

	closed := sfuHost(t, false)
	if err := closed.Close(t.Context()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if closed.current.Load() != nil {
		t.Fatal("a closed host still names a current instance")
	}
}

// The YAML the harness SFU renders carries the two settings Firefox needs: node_ip on loopback
// and advertise_internal_ip (G35 run A1), never sfu.DefaultConfig()'s AdvertiseInternalIP false.
func TestTheTestSFUAdvertisesInternalAddresses(t *testing.T) {
	yaml, err := testSFUConfig(7880, 7882, "dilla", strings.Repeat("s", 64)).YAML()
	if err != nil {
		t.Fatalf("YAML: %v", err)
	}
	for _, want := range []string{
		"port: 7880\n", "  node_ip: 127.0.0.1\n", "  enable_loopback_candidate: true\n",
		"  udp_port: 7882\n", "  advertise_internal_ip: true\n", "keys:\n  dilla: ",
	} {
		if !strings.Contains(yaml, want) {
			t.Errorf("the harness SFU YAML lacks %q:\n%s", want, yaml)
		}
	}
	if !strings.Contains(yaml, "auto_create: false") {
		t.Error("the test SFU auto-creates rooms")
	}
}

// Task-10 fix review N3: a voice suite's host may give its rooms a longer departure timeout, so a
// test that reloads a browser alone in a room is not raced by the room's own end on a slow runner;
// zero keeps the instance default (20 s) that every other test sees.
func TestTheHostOptionsSetTheTestSFUDepartureTimeout(t *testing.T) {
	for _, c := range []struct {
		opt  uint32
		want string
	}{{0, "  departure_timeout: 20\n"}, {60, "  departure_timeout: 60\n"}} {
		cfg := HostOptions{SFUDepartureTimeout: c.opt}.sfuConfig(7880, 7882, "dilla", strings.Repeat("s", 64))
		yaml, err := cfg.YAML()
		if err != nil {
			t.Fatalf("YAML: %v", err)
		}
		if !strings.Contains(yaml, c.want) {
			t.Errorf("SFUDepartureTimeout %d: the YAML lacks %q:\n%s", c.opt, c.want, yaml)
		}
	}
}

// GET /debug/sfu and POST /debug/sfu/token exist on the control listener only; the public
// listener — what a scenario client and a browser reach — answers 404 for both.
func TestTheDebugSFURoutesAreControlOnly(t *testing.T) {
	h := sfuHost(t, true)
	public := httptest.NewServer(h.Handler())
	t.Cleanup(public.Close)
	control := httptest.NewServer(ControlHandler(h))
	t.Cleanup(control.Close)

	body := []byte(`{"room":"r1","identity":"0123456789abcdef0123456789abcdef","create":true}`)
	for _, req := range []struct{ method, path string }{{http.MethodGet, "/debug/sfu"}, {http.MethodPost, "/debug/sfu/token"}} {
		r, err := http.NewRequestWithContext(t.Context(), req.method, public.URL+req.path, bytes.NewReader(body))
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		res, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatalf("%s %s: %v", req.method, req.path, err)
		}
		_ = res.Body.Close()
		if res.StatusCode != http.StatusNotFound {
			t.Errorf("public %s %s = %d, want 404", req.method, req.path, res.StatusCode)
		}
	}

	res, err := http.Get(control.URL + "/debug/sfu") //nolint:noctx // test against httptest
	if err != nil {
		t.Fatalf("GET /debug/sfu: %v", err)
	}
	var info map[string]string
	if err := json.NewDecoder(res.Body).Decode(&info); err != nil {
		t.Fatalf("decode: %v", err)
	}
	_ = res.Body.Close()
	if info["url"] != h.SFU().URL() || info["http_url"] != h.SFU().HTTPURL() {
		t.Fatalf("GET /debug/sfu = %v, want %s and %s", info, h.SFU().URL(), h.SFU().HTTPURL())
	}

	res, err = http.Post(control.URL+"/debug/sfu/token", "application/json", bytes.NewReader(body)) //nolint:noctx // test against httptest
	if err != nil {
		t.Fatalf("POST /debug/sfu/token: %v", err)
	}
	var tok map[string]string
	if err := json.NewDecoder(res.Body).Decode(&tok); err != nil {
		t.Fatalf("decode: %v", err)
	}
	_ = res.Body.Close()
	v, err := auth.ParseAPIToken(tok["token"])
	if err != nil {
		t.Fatalf("the debug token is not a LiveKit JWT: %v", err)
	}
	if v.Identity() != "0123456789abcdef0123456789abcdef" || v.APIKey() != h.cfg.LiveKit.APIKey || tok["url"] != h.SFU().URL() {
		t.Fatalf("token identity %q key %q url %q", v.Identity(), v.APIKey(), tok["url"])
	}

	// A host without an SFU answers 404 on the control listener too.
	bare := httptest.NewServer(ControlHandler(sfuHost(t, false)))
	t.Cleanup(bare.Close)
	res, err = http.Get(bare.URL + "/debug/sfu") //nolint:noctx // test against httptest
	if err != nil {
		t.Fatalf("GET /debug/sfu without an SFU: %v", err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("GET /debug/sfu without an SFU = %d, want 404", res.StatusCode)
	}
}

// With HostOptions.SFU the call route that answers 501 on a bare host (api's
// TestAnInstanceWithoutAnSFUAnswersNotImplemented) mints a real LiveKit token for a current leaf.
func TestAHostWithAnSFUMintsCallTokens(t *testing.T) {
	h := sfuHost(t, true)
	out, device, group := openCallOn(t, h)
	token, ok := out[3].(string)
	if !ok {
		t.Fatalf("the call response's token is %#v", out[3])
	}
	v, err := auth.ParseAPIToken(token)
	if err != nil {
		t.Fatalf("the call token is not a LiveKit JWT: %v", err)
	}
	if v.Identity() != device.String() || v.APIKey() != h.cfg.LiveKit.APIKey {
		t.Fatalf("token identity %q key %q, want %s and %q", v.Identity(), v.APIKey(), device, h.cfg.LiveKit.APIKey)
	}
	if b, _ := out[1].([]byte); id.ID(b) != group {
		t.Fatalf("group_id = %x, want %s", out[1], group)
	}
	// Without an override the call response names the instance's own wss:// origin.
	if out[2] != "wss://dilla.test" {
		t.Fatalf("livekit_url = %#v, want wss://dilla.test", out[2])
	}
}

// web-3 task 3: HostOptions.LiveKitURL replaces the call response's livekit_url, so a browser on
// 127.0.0.1 reaches the host's /rtc over ws:// instead of the instance domain's wss:// URL.
func TestLiveKitURLOverride(t *testing.T) {
	tcp, udp := freePorts(t)
	h, err := NewHost(context.Background(), HostOptions{
		DataDir: t.TempDir(), CorePath: corePath(t), SFU: true, SFUPort: tcp, SFUUDPPort: udp,
		LiveKitURL: "ws://127.0.0.1:8481",
	})
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	t.Cleanup(func() { _ = h.Close(context.Background()) })
	out, _, _ := openCallOn(t, h)
	if out[2] != "ws://127.0.0.1:8481" {
		t.Fatalf("livekit_url = %#v, want the override ws://127.0.0.1:8481", out[2])
	}
}

// web-3 task 10: the in-process LiveKit posts its webhooks to the host's instance, as dillad's own
// livekit.webhook_listen does (cmd/dillad/front.go). Without them the instance never learns who is in
// a room: no voice_state reaches a client, the voice snapshot (L-HTTP-100) stays empty and an answered
// ring never stops — everything the voice e2e waits on. A participant LiveKit admits to the call's
// room with the call's own token is listed in GET /v1/channels/{id}/call.
func TestTheHostSFUPostsItsWebhooksToTheInstance(t *testing.T) {
	h := sfuHost(t, true)
	if h.WebhookURL() == "" || !strings.HasPrefix(h.WebhookURL(), "http://127.0.0.1:") {
		t.Fatalf("the host's LiveKit posts its webhooks to %q, want a loopback http:// URL", h.WebhookURL())
	}
	out, device, group := openCallOn(t, h)
	token, ok := out[3].(string)
	if !ok {
		t.Fatalf("the call response's token is %#v", out[3])
	}
	g, err := h.Server().Repo().GetGroup(t.Context(), group)
	if err != nil {
		t.Fatalf("GetGroup: %v", err)
	}
	room, err := lksdk.ConnectToRoomWithToken(h.SFU().URL(), token, &lksdk.RoomCallback{}, lksdk.WithAutoSubscribe(false))
	if err != nil {
		t.Fatalf("connect to the call's room: %v", err)
	}
	t.Cleanup(room.Disconnect)

	path := "/v1/channels/" + g.TargetID.String() + "/call"
	deadline := time.Now().Add(30 * time.Second)
	for {
		status, raw, err := h.AsDevice(t.Context(), device, http.MethodGet, path, nil)
		if err != nil || status != http.StatusOK {
			t.Fatalf("GET %s = %d, %v", path, status, err)
		}
		var call []any
		if err := cborx.Unmarshal(raw, &call); err != nil || len(call) != 3 {
			t.Fatalf("decode %s: %v (%x)", path, err, raw)
		}
		if parts, _ := call[2].([]any); len(parts) == 1 {
			row, _ := parts[0].([]any)
			if b, _ := row[1].([]byte); len(row) == 3 && id.ID(b) == device {
				return
			}
			t.Fatalf("participants = %#v, want only %s", parts, device)
		}
		if time.Now().After(deadline) {
			t.Fatalf("the call's participants never listed the device LiveKit admitted (the instance got no participant_joined)")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// openCallOn seeds a caller with one device, a voice channel, its call group with the device a leaf
// of its current epoch, and opens the call: the 201 body, the device and the group.
func openCallOn(t *testing.T, h *Host) ([]any, id.ID, id.ID) {
	t.Helper()
	ctx := t.Context()

	key := func() string {
		p, _, err := ed25519.GenerateKey(nil)
		if err != nil {
			t.Fatalf("GenerateKey: %v", err)
		}
		return hex.EncodeToString(p)
	}
	device := id.New()
	seeded, err := SeedUsers(ctx, h.Server(), []SeedRequest{{
		Username: "caller", Display: "caller", UMKPub: key(), SSKPub: key(),
		SigUMKSSK: hex.EncodeToString(make([]byte, 64)),
		Devices:   []Device{{DeviceID: device.String(), DSKPub: key(), Credential: "01"}},
	}})
	if err != nil {
		t.Fatalf("SeedUsers: %v", err)
	}
	satisfyFactor(t, h, seeded[0].UserID)
	call := func(method, path string, body any) []any {
		t.Helper()
		status, raw, err := h.AsDevice(ctx, device, method, path, body)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		if status != http.StatusCreated {
			t.Fatalf("%s %s = %d (%x), want 201", method, path, status, raw)
		}
		var out []any
		if err := cborx.Unmarshal(raw, &out); err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
		return out
	}
	idOf := func(v any) id.ID {
		b, ok := v.([]byte)
		if !ok || len(b) != len(id.ID{}) {
			t.Fatalf("%#v is not a 16-byte id", v)
		}
		return id.ID(b)
	}
	cid := idOf(call(http.MethodPost, "/v1/communities", []any{"voice", []byte("{}"), uint64(0), uint64(0)})[0])
	// kind voice (1); a discoverable channel is forced readable (mode 1).
	ch := idOf(call(http.MethodPost, "/v1/communities/"+cid.String()+"/channels",
		[]any{uint64(api.ChannelVoice), uint64(1), uint64(2), nil, "voice", "", uint64(0), uint64(0)})[0])

	// The call group and the caller's leaf in its current epoch, as the delivery service would
	// have recorded them after the caller's external commit.
	repo := h.Server().Repo()
	group := store.GroupRow{
		GroupID: id.New(), Binding: []byte{0x80}, Kind: api.GroupCall, CommunityID: &cid, TargetID: ch,
		CallID: &ch, Ciphersuite: 1, Epoch: 7, ExternalSenderKeyID: id.New(),
		E2EEVersion: 1, MediaVersion: 1, PolicyVersion: 1, Created: h.Clock().Now().Unix(),
	}
	if err := repo.CreateGroup(ctx, group); err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}
	dev, err := repo.GetDevice(ctx, device)
	if err != nil {
		t.Fatalf("GetDevice: %v", err)
	}
	if err := repo.ReplaceMembers(ctx, group.GroupID, 7, []store.MemberRow{{
		GroupID: group.GroupID, LeafIndex: 0, UserID: dev.UserID, DeviceID: device,
		SignatureKey: make([]byte, 32), AddedEpoch: 7,
	}}); err != nil {
		t.Fatalf("ReplaceMembers: %v", err)
	}

	return call(http.MethodPost, "/v1/channels/"+ch.String()+"/calls", []any{}), device, group.GroupID
}

// A room POST /debug/sfu/token opened survives the call routes' room sweep, which deletes every room
// that belongs to no call: a browser test on raw debug tokens (plan MD-13) runs for minutes, past
// several sweeps. A room opened straight on the SFU is the control — the sweep deletes it, which
// shows a sweep ran while the debug room stayed.
func TestTheRoomSweepLeavesDebugRoomsAlone(t *testing.T) {
	h := sfuHost(t, true)
	control := httptest.NewServer(ControlHandler(h))
	t.Cleanup(control.Close)
	ctx := t.Context()

	if err := h.SFU().CreateRoom(ctx, "orphan"); err != nil {
		t.Fatalf("CreateRoom: %v", err)
	}
	body := []byte(`{"room":"sp-debug","identity":"0123456789abcdef0123456789abcdef","create":true}`)
	res, err := http.Post(control.URL+"/debug/sfu/token", "application/json", bytes.NewReader(body)) //nolint:noctx // test against httptest
	if err != nil {
		t.Fatalf("POST /debug/sfu/token: %v", err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("POST /debug/sfu/token = %d, want 200", res.StatusCode)
	}

	// The retry loop's first tick sweeps (api.CallRetryInterval after New), then every
	// api.RoomSweepInterval.
	wait := api.CallRetryInterval + 10*time.Second
	deadline := time.Now().Add(wait)
	for {
		rooms, err := h.SFU().Rooms(ctx)
		if err != nil {
			t.Fatalf("Rooms: %v", err)
		}
		if !slices.Contains(rooms, "sp-debug") {
			t.Fatalf("the room sweep deleted the debug room (rooms now %v)", rooms)
		}
		if !slices.Contains(rooms, "orphan") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no room sweep deleted the orphan room within %s (rooms %v)", wait, rooms)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// listedParticipants is how many participants the instance lists for the call of channel.
func listedParticipants(t *testing.T, h *Host, device id.ID, channel string) int {
	t.Helper()
	path := "/v1/channels/" + channel + "/call"
	status, raw, err := h.AsDevice(t.Context(), device, http.MethodGet, path, nil)
	if err != nil || status != http.StatusOK {
		t.Fatalf("GET %s = %d, %v", path, status, err)
	}
	var call []any
	if err := cborx.Unmarshal(raw, &call); err != nil || len(call) != 3 {
		t.Fatalf("decode %s: %v (%x)", path, err, raw)
	}
	parts, _ := call[2].([]any)
	return len(parts)
}

func postWithhold(t *testing.T, url string, body string) int {
	t.Helper()
	res, err := http.Post(url+"/debug/sfu/withhold", "application/json", strings.NewReader(body)) //nolint:noctx // test against httptest
	if err != nil {
		t.Fatalf("POST /debug/sfu/withhold: %v", err)
	}
	_ = res.Body.Close()
	return res.StatusCode
}

func eventually(t *testing.T, what string, within time.Duration, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("%s never happened within %s", what, within)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// POST /debug/sfu/withhold holds back one webhook event on its way to the instance: a participant_left
// never reaches CallEvents().Handle while a participant_joined still does, and on:false restores it.
// A host without an SFU answers 404.
func TestWithholdDropsParticipantLeftOnly(t *testing.T) {
	h := sfuHost(t, true)
	control := httptest.NewServer(ControlHandler(h))
	t.Cleanup(control.Close)
	public := httptest.NewServer(h.Handler())
	t.Cleanup(public.Close)

	if got := postWithhold(t, public.URL, `{"event":"participant_left","on":true}`); got != http.StatusNotFound {
		t.Fatalf("the public listener answers /debug/sfu/withhold with %d, want 404", got)
	}
	if got := postWithhold(t, control.URL, `{"event":"participant_left","on":true}`); got != http.StatusNoContent {
		t.Fatalf("withhold participant_left = %d, want 204", got)
	}

	out, device, group := openCallOn(t, h)
	token, _ := out[3].(string)
	g, err := h.Server().Repo().GetGroup(t.Context(), group)
	if err != nil {
		t.Fatalf("GetGroup: %v", err)
	}
	channel := g.TargetID.String()
	join := func() *lksdk.Room {
		room, err := lksdk.ConnectToRoomWithToken(h.SFU().URL(), token, &lksdk.RoomCallback{}, lksdk.WithAutoSubscribe(false))
		if err != nil {
			t.Fatalf("connect to the call's room: %v", err)
		}
		return room
	}

	// participant_joined still arrives while participant_left is held back.
	room := join()
	eventually(t, "the participant_joined reaching the instance", 30*time.Second, func() bool {
		return listedParticipants(t, h, device, channel) == 1
	})
	room.Disconnect()
	time.Sleep(4 * time.Second)
	if n := listedParticipants(t, h, device, channel); n != 1 {
		t.Fatalf("a withheld participant_left still removed the participant: %d listed, want 1", n)
	}

	// on:false restores it: the next departure is delivered.
	if got := postWithhold(t, control.URL, `{"event":"participant_left","on":false}`); got != http.StatusNoContent {
		t.Fatalf("restore participant_left = %d, want 204", got)
	}
	room = join()
	t.Cleanup(room.Disconnect)
	room.Disconnect()
	eventually(t, "the restored participant_left reaching the instance", 30*time.Second, func() bool {
		return listedParticipants(t, h, device, channel) == 0
	})

	bare := httptest.NewServer(ControlHandler(sfuHost(t, false)))
	t.Cleanup(bare.Close)
	if got := postWithhold(t, bare.URL, `{"event":"participant_left","on":true}`); got != http.StatusNotFound {
		t.Fatalf("withhold without an SFU = %d, want 404", got)
	}
}

// The withhold hook lives in the test host's package, so it is reachable only where that package is
// linked: no production binary (dillad) may depend on dilladtest.
func TestTheWithholdHookIsNotLinkedIntoProductionDillad(t *testing.T) {
	out, err := exec.CommandContext(t.Context(), "go", "list", "-deps", "github.com/dilla-chat/dilla/cmd/dillad").Output()
	if err != nil {
		t.Fatalf("go list -deps ./cmd/dillad: %v", err)
	}
	for _, pkg := range strings.Fields(string(out)) {
		if strings.HasSuffix(pkg, "/internal/dillad/dilladtest") {
			t.Fatalf("cmd/dillad links %s, which carries the test-only /debug/sfu/withhold hook", pkg)
		}
	}
}

// Only participant_left can be held back: any other event name is refused with 400.
func TestWithholdRefusesEveryOtherEvent(t *testing.T) {
	control := httptest.NewServer(ControlHandler(sfuHost(t, true)))
	t.Cleanup(control.Close)
	for _, event := range []string{"participant_joined", "room_finished", "bogus", ""} {
		body := `{"event":"` + event + `","on":true}`
		if got := postWithhold(t, control.URL, body); got != http.StatusBadRequest {
			t.Errorf("withhold %q = %d, want 400", event, got)
		}
	}
}
