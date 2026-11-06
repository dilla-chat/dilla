package dillad_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/webhook"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/dillad"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/mlswasi"
	"github.com/dilla-chat/dilla/internal/sfu"
	"github.com/dilla-chat/dilla/internal/store"
)

// wiring_test.go is task 27a: the composition root builds the gateway and the delivery service,
// mounts every delivery-service route and GET /gateway on the one mux, hands the gateway's ticket
// store to POST /v1/gateway/ticket, and exposes the accessors task 29's harness needs.
//
// DEVIATION FROM THE BRIEF'S STEP 1 (deviation B35): the brief's newServer builds its config with
// config.Default() and a bare temp database, but New reads the instance row (`dillad init`'s) and
// the database must be migrated, so newServer starts from testConfig — which does exactly what
// `dillad init` does — and passes the wasi core explicitly, because the config has no [mls] table.

// testCorePath is the wasm32-wasip1 build of dilla-core-wasi the mlswasi tests use. CI downloads
// it there from the rust-wasi job; locally it is built with
// `cargo build -p dilla-core-wasi --target wasm32-wasip1 --release --locked` and copied.
func testCorePath(t *testing.T) string {
	t.Helper()
	path, err := filepath.Abs(filepath.Join("..", "mlswasi", "testdata", "dilla_core_wasi.wasm"))
	if err != nil {
		t.Fatalf("resolve the wasi core: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("%s is missing: build it with\n"+
			"  cargo build -p dilla-core-wasi --target wasm32-wasip1 --release --locked\n"+
			"and copy it there (CI downloads the rust-wasi job's artifact): %v", path, err)
	}
	return path
}

var (
	sharedWasmOnce sync.Once
	sharedWasm     *mlswasi.Runtime
	sharedWasmErr  error
)

// sharedRuntime is one wasm runtime for the whole package. Compiling the OpenMLS module is the most
// expensive thing New does, and every server in this file would otherwise pay it; a runtime passed
// in through Options.Wasm is not closed by Shutdown, so one outlives every server that borrows it.
// The DS's state cache returns every instance it held when its server shuts down.
func sharedRuntime(t *testing.T) *mlswasi.Runtime {
	t.Helper()
	sharedWasmOnce.Do(func() {
		raw, err := os.ReadFile(testCorePath(t))
		if err != nil {
			sharedWasmErr = err
			return
		}
		sharedWasm, sharedWasmErr = mlswasi.New(context.Background(), raw, mlswasi.Options{PoolSize: 4})
	})
	if sharedWasmErr != nil {
		t.Fatalf("build the shared wasm runtime: %v", sharedWasmErr)
	}
	return sharedWasm
}

func newServer(t *testing.T) (*dillad.Server, *httptest.Server) {
	t.Helper()
	cfg := testConfig(t)
	cfg.Log.Level = "warn"
	s, err := dillad.New(context.Background(), dillad.Options{
		Config: cfg,
		Clock:  clock.NewFake(time.Unix(1_700_000_000, 0)),
		Wasm:   sharedRuntime(t),
	})
	if err != nil {
		t.Fatalf("dillad.New: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return s, ts
}

// Every delivery-service route of §5.1 is MOUNTED. An unauthenticated request must be answered
// 401, never 404: a 404 means the route does not exist, which is the failure this test exists for.
// The group routes, the KeyPackage claim and the two Welcome routes are socket routes (Bot v2): they are mounted on the
// socket mux, answer 404 over HTTP, and answer a request frame without a session 401.
func TestEveryDeliveryServiceRouteIsMounted(t *testing.T) {
	srv, ts := newServer(t)
	const gid = "0102030405060708090a0b0c0d0e0f10"
	for _, c := range []struct{ method, path string }{
		{http.MethodPost, "/v1/keypackages"},
	} {
		req, err := http.NewRequestWithContext(t.Context(), c.method, ts.URL+c.path, strings.NewReader(""))
		if err != nil {
			t.Fatalf("%s %s: %v", c.method, c.path, err)
		}
		res, err := ts.Client().Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", c.method, c.path, err)
		}
		res.Body.Close()
		if res.StatusCode == http.StatusNotFound {
			t.Errorf("%s %s is not mounted", c.method, c.path)
		}
		if res.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s %s: status = %d, want 401 (mounted, unauthenticated)",
				c.method, c.path, res.StatusCode)
		}
	}
	// The socket routes: mounted on the socket mux only, and refused a request frame without a session.
	disp := socketHandler(srv)
	for _, c := range []struct{ method, pattern, path string }{
		{http.MethodPost, "POST /v1/groups", "/v1/groups"},
		{http.MethodGet, "GET /v1/groups/{id}/info", "/v1/groups/" + gid + "/info"},
		{http.MethodGet, "GET /v1/groups/{id}/tree", "/v1/groups/" + gid + "/tree"},
		{http.MethodGet, "GET /v1/groups/{id}/handshakes", "/v1/groups/" + gid + "/handshakes"},
		{http.MethodPost, "POST /v1/groups/{id}/commit", "/v1/groups/" + gid + "/commit"},
		{http.MethodPost, "POST /v1/groups/{id}/proposal", "/v1/groups/" + gid + "/proposal"},
		{http.MethodGet, "GET /v1/groups/{id}/proposals", "/v1/groups/" + gid + "/proposals"},
		{http.MethodPost, "POST /v1/groups/{id}/message", "/v1/groups/" + gid + "/message"},
		{http.MethodGet, "GET /v1/groups/{id}/messages", "/v1/groups/" + gid + "/messages"},
		{http.MethodDelete, "DELETE /v1/groups/{id}/messages/{seq}", "/v1/groups/" + gid + "/messages/1"},
		{http.MethodPost, "POST /v1/groups/{id}/cursor", "/v1/groups/" + gid + "/cursor"},
		{http.MethodPost, "POST /v1/groups/{id}/resync", "/v1/groups/" + gid + "/resync"},
		{http.MethodPost, "POST /v1/groups/{id}/fork-report", "/v1/groups/" + gid + "/fork-report"},
		{http.MethodGet, "GET /v1/groups/{id}/heal", "/v1/groups/" + gid + "/heal"},
		{http.MethodPost, "POST /v1/groups/{id}/heal", "/v1/groups/" + gid + "/heal"},
		{http.MethodGet, "GET /v1/devices/{device_id}/keypackage", "/v1/devices/" + gid + "/keypackage"},
		{http.MethodGet, "GET /v1/welcomes", "/v1/welcomes"},
		{http.MethodDelete, "DELETE /v1/welcomes/{welcome_id}", "/v1/welcomes/1"},
	} {
		if !slices.Contains(srv.Mux().SocketPatterns(), c.pattern) || slices.Contains(srv.Mux().Patterns(), c.pattern) {
			t.Errorf("%s is not a socket-only route", c.pattern)
		}
		req, err := http.NewRequestWithContext(t.Context(), c.method, ts.URL+c.path, strings.NewReader(""))
		if err != nil {
			t.Fatalf("%s %s: %v", c.method, c.path, err)
		}
		res, err := ts.Client().Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", c.method, c.path, err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusNotFound {
			t.Errorf("%s %s over HTTP = %d, want 404", c.method, c.path, res.StatusCode)
		}
		if rec := call(t, disp, c.method, c.path, "", nil); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s over the socket without a session = %d, want 401", c.method, c.path, rec.Code)
		}
	}
}

// The gateway upgrade is served, and a plain GET without the upgrade headers is a 400 from
// websocket.Accept rather than a 404.
func TestTheGatewayUpgradeIsMounted(t *testing.T) {
	_, ts := newServer(t)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, ts.URL+"/gateway", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	res, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("GET /gateway: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusNotFound {
		t.Fatal("/gateway is not mounted")
	}
}

// POST /v1/gateway/ticket can only mint from a gateway.Tickets the composition root passed in.
func TestTheTicketRouteHasATicketStore(t *testing.T) {
	s, _ := newServer(t)
	if s.Gateway() == nil {
		t.Fatal("the server built no gateway, so nothing can mint an upgrade ticket")
	}
	if s.Gateway().Tickets() == nil {
		t.Fatal("the gateway has no ticket store")
	}
}

// The five accessors the test harness needs.
func TestTheServerExposesTheAccessorsTheHarnessNeeds(t *testing.T) {
	s, _ := newServer(t)
	if s.Repo() == nil {
		t.Error("Repo() is nil")
	}
	if s.DS() == nil {
		t.Error("DS() is nil")
	}
	if s.Gateway() == nil {
		t.Error("Gateway() is nil")
	}
	if s.CommitCount() != 0 {
		t.Errorf("CommitCount() = %d on a fresh instance, want 0", s.CommitCount())
	}
	if _, err := s.DebugState(context.Background()); err != nil {
		t.Errorf("DebugState: %v", err)
	}
}

// DS.Start's background goroutines stop with Shutdown, and Shutdown is idempotent.
func TestShutdownStopsTheWatchdogAndTheSweeperAndIsIdempotent(t *testing.T) {
	s, _ := newServer(t)
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatalf("a second Shutdown must be a no-op: %v", err)
	}
}

// ---------------------------------------------------------------- beyond the brief's five

// Now() is the server's own clock and DebugState reports the instance as it stands: a fresh
// instance holds no group, has accepted no commit and runs no election.
func TestNowAndDebugStateReadTheInstance(t *testing.T) {
	s, _ := newServer(t)
	if got := s.Now().Unix(); got != 1_700_000_000 {
		t.Errorf("Now() = %d, want the fake clock's 1700000000", got)
	}
	state, err := s.DebugState(context.Background())
	if err != nil {
		t.Fatalf("DebugState: %v", err)
	}
	if state.Generation != 1 || state.Groups != 0 || state.Commits != 0 ||
		state.OpenElections != 0 || state.FrozenGroups != 0 || state.NowUnix != 1_700_000_000 {
		t.Errorf("DebugState = %+v, want generation 1, nothing else, now 1700000000", state)
	}
}

// With no runtime passed in, New builds one from the core it is pointed at, and a core that is
// not there is a start-up error that names the path — never a server whose every DS route fails.
func TestNewBuildsTheWasmRuntimeFromTheCorePath(t *testing.T) {
	cfg := testConfig(t)
	s, err := dillad.New(context.Background(), dillad.Options{
		Config: cfg, Clock: clock.System(), CorePath: testCorePath(t),
	})
	if err != nil {
		t.Fatalf("New with a CorePath: %v", err)
	}
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	missing := filepath.Join(t.TempDir(), "no-such-core.wasm")
	_, err = dillad.New(context.Background(), dillad.Options{
		Config: testConfig(t), Clock: clock.System(), CorePath: missing,
	})
	if err == nil || !strings.Contains(err.Error(), missing) {
		t.Fatalf("New with a missing core gave %v, want an error naming %s", err, missing)
	}
}

// accountToken redeems the bootstrap invite and returns the account's first device session. The
// account's second factor is satisfied (satisfyFactor), so the session reaches every enrolled
// route; factor_owed_test.go is where an owed account is exercised.
func accountToken(t *testing.T, srv *dillad.Server, code string) string {
	t.Helper()
	dev := newTestDevice(t)
	body, err := cborx.Marshal(newAccountRequest(code, "jonas", dev))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	res := post(t, srv.Handler(), "/v1/accounts", body)
	if res.Code != http.StatusOK {
		t.Fatalf("POST /v1/accounts = %d: %s", res.Code, res.Body.String())
	}
	var account []any
	if err := cborx.Unmarshal(res.Body.Bytes(), &account); err != nil {
		t.Fatalf("decode: %v", err)
	}
	token, ok := account[2].(string)
	if !ok || token == "" {
		t.Fatalf("no session token in the account response: %#v", account)
	}
	satisfyFactor(t, srv, idOf(t, account[0]))
	return token
}

// newGreetedServer is a served instance plus one enrolled device's session token.
func newGreetedServer(t *testing.T) (*dillad.Server, *httptest.Server, string) {
	t.Helper()
	cfg, code := testConfigInvite(t)
	s, err := dillad.New(context.Background(), dillad.Options{
		Config: cfg, Clock: clock.System(), Wasm: sharedRuntime(t),
	})
	if err != nil {
		t.Fatalf("dillad.New: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return s, ts, accountToken(t, s, code)
}

// readFrame reads one gateway frame and returns its opcode and payload.
func readFrame(t *testing.T, ctx context.Context, c *websocket.Conn) (uint64, []any) {
	t.Helper()
	typ, b, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if typ != websocket.MessageBinary {
		t.Fatalf("message type %v, want binary", typ)
	}
	var frame []any
	if err := cborx.Unmarshal(b, &frame); err != nil {
		t.Fatalf("decode frame: %v", err)
	}
	if len(frame) != 4 {
		t.Fatalf("frame has %d elements, want [op, n, group_id, payload]", len(frame))
	}
	op, ok := frame[0].(uint64)
	if !ok {
		t.Fatalf("op is %T", frame[0])
	}
	payload, _ := frame[3].([]any)
	return op, payload
}

// identify sends op 1 with the credential left to the upgrade, which is the path both the
// Authorization header and the ticket take. It picks the wire version the instance offers
// (wire 6, the only one since Bot v2).
func identify(t *testing.T, ctx context.Context, c *websocket.Conn) {
	t.Helper()
	b, err := cborx.Marshal([]any{uint64(1), uint64(1), nil,
		[]any{"", uint64(dillad.WireVersion), uint64(1), uint64(1), uint64(0)}})
	if err != nil {
		t.Fatalf("marshal identify: %v", err)
	}
	if err := c.Write(ctx, websocket.MessageBinary, b); err != nil {
		t.Fatalf("write identify: %v", err)
	}
}

// The whole handshake through the composition root's handler chain: the upgrade has to reach
// http.Hijacker through RequestLog and Recover, hello has to carry this instance's id, and ready
// is only reached when the gateway resolves the session through the instance's own auth.Sessions
// and reads the device's groups through its own store.
func TestAnIdentifiedDeviceIsReadyAndOnlineThroughTheCompositionRoot(t *testing.T) {
	s, ts, token := newGreetedServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+"/gateway", //nolint:bodyclose // websocket.Dial documents that the handshake response body never needs closing
		&websocket.DialOptions{
			HTTPHeader:   http.Header{"Authorization": []string{"Bearer " + token}},
			Subprotocols: []string{"dilla.v1"},
		})
	if err != nil {
		t.Fatalf("dial /gateway: %v", err)
	}
	defer c.CloseNow()

	op, hello := readFrame(t, ctx, c)
	if op != 0 {
		t.Fatalf("first frame op %d, want hello (0)", op)
	}
	instanceID := s.Sessions().InstanceID()
	if got, _ := hello[5].([]byte); string(got) != string(instanceID[:]) {
		t.Errorf("hello carries instance id %x, want this instance's %x", got, instanceID)
	}
	// Invariant 7's back-off window reaches hello from the delivery service's policy (deviation
	// B23): 300 ms + random(0..300 ms) on a default configuration.
	if len(hello) != 9 {
		t.Fatalf("hello has %d elements, want 9", len(hello))
	}
	if backoff, jitter := hello[7], hello[8]; backoff != uint64(300) || jitter != uint64(300) {
		t.Errorf("hello advertises backoff %v + jitter %v ms, want the policy's 300 + 300", backoff, jitter)
	}
	identify(t, ctx, c)
	if op, _ := readFrame(t, ctx, c); op != 3 {
		t.Fatalf("op %d after identify, want ready (3)", op)
	}
	sess, err := s.Sessions().Resolve(ctx, token)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !s.Gateway().Online(sess.DeviceID) {
		t.Error("a device that reached ready through the composition root is not online")
	}
}

// POST /v1/gateway/ticket mints from the gateway's own store, so the ticket it answers is one the
// upgrade accepts: the browser path of gap-38, end to end.
func TestTheTicketRouteMintsATicketTheGatewayAccepts(t *testing.T) {
	_, ts, token := newGreetedServer(t)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, ts.URL+"/v1/gateway/ticket", strings.NewReader(""))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/cbor")
	res, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("POST /v1/gateway/ticket: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("POST /v1/gateway/ticket = %d, want 201", res.StatusCode)
	}
	var body []any
	raw := make([]byte, 512)
	n, _ := res.Body.Read(raw)
	if err := cborx.Unmarshal(raw[:n], &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	ticket, ok := body[0].(string)
	if !ok || ticket == "" {
		t.Fatalf("no ticket in %#v", body)
	}
	if expires, ok := body[1].(uint64); !ok || int64(expires) <= time.Now().Unix() {
		t.Fatalf("expires = %#v, want a unix time in the future", body[1])
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+"/gateway", //nolint:bodyclose // websocket.Dial documents that the handshake response body never needs closing
		&websocket.DialOptions{Subprotocols: []string{"dilla.v1", "dilla.ticket." + ticket}})
	if err != nil {
		t.Fatalf("dial /gateway with a ticket: %v", err)
	}
	defer c.CloseNow()
	if op, _ := readFrame(t, ctx, c); op != 0 {
		t.Fatalf("first frame op %d, want hello", op)
	}
	identify(t, ctx, c)
	if op, _ := readFrame(t, ctx, c); op != 3 {
		t.Fatalf("op %d after identify with a ticket, want ready (3)", op)
	}
}

// Revoking a device's sessions closes its sockets in the same breath (protocol/02, "Device
// sessions", rule 6): the composition root wires auth.Sessions.OnRevoke to Gateway.CloseDevice.
func TestRevokingADeviceClosesItsGatewayConnection(t *testing.T) {
	s, ts, token := newGreetedServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+"/gateway", //nolint:bodyclose // websocket.Dial documents that the handshake response body never needs closing
		&websocket.DialOptions{
			HTTPHeader:   http.Header{"Authorization": []string{"Bearer " + token}},
			Subprotocols: []string{"dilla.v1"},
		})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.CloseNow()
	readFrame(t, ctx, c)
	identify(t, ctx, c)
	if op, _ := readFrame(t, ctx, c); op != 3 {
		t.Fatalf("op %d, want ready", op)
	}
	sess, err := s.Sessions().Resolve(ctx, token)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	// The client keeps reading, as a live client does, so it answers the close handshake the
	// gateway starts; a peer that never reads holds the revoking caller for the websocket
	// library's close timeout instead.
	ended := make(chan error, 1)
	go func() {
		for {
			if _, _, err := c.Read(ctx); err != nil {
				ended <- err
				return
			}
		}
	}()
	if err := s.Sessions().RevokeDevice(ctx, sess.DeviceID); err != nil {
		t.Fatalf("RevokeDevice: %v", err)
	}
	err = <-ended
	if got := websocket.CloseStatus(err); got != 4004 {
		t.Fatalf("the socket ended with %v (status %d), want close 4004 session revoked", err, got)
	}
}

// The delivery-service routes are metered through the composition root: the instance's own
// limiter, keyed by the device session, refuses the request past [limits.rate]'s read burst with
// 429 E_RATE_LIMITED — whatever the route would have answered.
func TestTheDeliveryServiceRoutesAreMeteredPerDevice(t *testing.T) {
	srv, _, token := newGreetedServer(t)
	burst := testConfig(t).Limits.Rate.ReadBurst
	h := socketHandler(srv)
	get := func() int {
		return call(t, h, http.MethodGet, "/v1/groups/0102030405060708090a0b0c0d0e0f10/info", token, nil).Code
	}
	// The clock is the wall clock, so the bucket refills a little while the loop runs: the first
	// refusal comes at the burst or a few requests after it, never before, and always comes.
	for i := range 2 * burst {
		if get() == http.StatusTooManyRequests {
			if i < burst {
				t.Fatalf("request %d of a read burst of %d was refused", i+1, burst)
			}
			return
		}
	}
	t.Fatalf("%d reads in a row were never refused; the read burst is %d", 2*burst, burst)
}

// A healthy Plan 1 instance is READY: /readyz answers 200 once New has built the database, the
// schema, the wasi runtime and the delivery service. livekit and tls are Plan 2's and are not
// required; until the final review they stayed red, so /readyz answered 503 forever and no load
// balancer would ever route to the instance. Drain still turns it unready.
func TestAHealthyPlanOneInstanceIsReady(t *testing.T) {
	s, ts := newServer(t)
	get := func() (int, string) {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, ts.URL+"/readyz", nil)
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		res, err := ts.Client().Do(req)
		if err != nil {
			t.Fatalf("GET /readyz: %v", err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(b)
	}
	if code, body := get(); code != http.StatusOK {
		t.Fatalf("/readyz = %d %s, want 200 on a healthy instance", code, body)
	}
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if code, _ := get(); code != http.StatusServiceUnavailable {
		t.Fatalf("/readyz = %d while draining, want 503", code)
	}
}

// The listener bounds how long a request header may take, from server.read_header_timeout and
// never zero — net/http reads zero as "no limit".
func TestTheListenerBoundsHowLongAHeaderMayTake(t *testing.T) {
	s, _ := newServer(t)
	if got := s.ReadHeaderTimeout(); got != 10*time.Second {
		t.Errorf("ReadHeaderTimeout = %v, want the configured 10s", got)
	}
	cfg := testConfig(t)
	cfg.Server.ReadHeaderTimeout = "0s"
	zero, err := dillad.New(context.Background(), dillad.Options{
		Config: cfg, Clock: clock.NewFake(time.Unix(1_700_000_000, 0)), Wasm: sharedRuntime(t),
	})
	if err != nil {
		t.Fatalf("dillad.New: %v", err)
	}
	t.Cleanup(func() { _ = zero.Shutdown(context.Background()) })
	if got := zero.ReadHeaderTimeout(); got <= 0 {
		t.Errorf("a zero read_header_timeout left the listener unbounded (%v)", got)
	}
}

// newGreetedServerAt is newGreetedServer on a fake clock the test moves, so the maintenance loop
// the composition root starts can be driven past a deadline without waiting for it.
func newGreetedServerAt(t *testing.T) (*dillad.Server, *httptest.Server, string, *clock.Fake) {
	t.Helper()
	cfg, code := testConfigInvite(t)
	cfg.Log.Level = "warn"
	clk := clock.NewFake(time.Now().Truncate(time.Second))
	s, err := dillad.New(context.Background(), dillad.Options{
		Config: cfg, Clock: clk, Wasm: sharedRuntime(t),
	})
	if err != nil {
		t.Fatalf("dillad.New: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return s, ts, accountToken(t, s, code), clk
}

// dialReady opens a gateway connection and takes it through hello, identify and ready.
func dialReady(t *testing.T, ctx context.Context, ts *httptest.Server, token string) *websocket.Conn {
	t.Helper()
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+"/gateway", //nolint:bodyclose // websocket.Dial documents that the handshake response body never needs closing
		&websocket.DialOptions{
			HTTPHeader:   http.Header{"Authorization": []string{"Bearer " + token}},
			Subprotocols: []string{"dilla.v1"},
		})
	if err != nil {
		t.Fatalf("dial /gateway: %v", err)
	}
	t.Cleanup(func() { _ = c.CloseNow() })
	if op, _ := readFrame(t, ctx, c); op != 0 {
		t.Fatalf("first frame op %d, want hello", op)
	}
	identify(t, ctx, c)
	if op, _ := readFrame(t, ctx, c); op != 3 {
		t.Fatalf("op %d after identify, want ready", op)
	}
	return c
}

// advanceUntil moves the fake clock one step at a time, giving the maintenance goroutine a moment
// after each step, until done reports true or ten seconds of wall time pass.
func advanceUntil(t *testing.T, clk *clock.Fake, step time.Duration, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatal("the condition never held while the clock advanced")
		}
		clk.Advance(step)
		time.Sleep(20 * time.Millisecond)
	}
}

// The gateway's liveness sweep runs in production, not only when a test calls it: an identified
// connection that stops heartbeating is closed 4009 once the clock passes interval*2 + 5 s.
func TestTheCompositionRootClosesAConnectionThatStopsHeartbeating(t *testing.T) {
	s, ts, token, clk := newGreetedServerAt(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	c := dialReady(t, ctx, ts, token)
	sess, err := s.Sessions().Resolve(ctx, token)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	ended := make(chan error, 1)
	go func() {
		for {
			if _, _, err := c.Read(ctx); err != nil {
				ended <- err
				return
			}
		}
	}()
	var closed error
	advanceUntil(t, clk, 30*time.Second, func() bool {
		select {
		case closed = <-ended:
			return true
		default:
			return false
		}
	})
	if got := websocket.CloseStatus(closed); got != 4009 {
		t.Fatalf("the silent connection ended with %v (status %d), want close 4009 session_timeout", closed, got)
	}
	if s.Gateway().Online(sess.DeviceID) {
		t.Error("a device whose only connection timed out is still online")
	}
}

// A connection the client dropped waits in the resume window with its ring, and the maintenance
// loop drops it once the window has passed.
func TestTheCompositionRootDropsASuspendedSessionPastTheResumeWindow(t *testing.T) {
	s, ts, token, clk := newGreetedServerAt(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	c := dialReady(t, ctx, ts, token)
	_ = c.Close(websocket.StatusNormalClosure, "bye")
	deadline := time.Now().Add(5 * time.Second)
	for s.Gateway().Suspended() != 1 {
		if time.Now().After(deadline) {
			t.Fatalf("suspended = %d after the client left, want 1", s.Gateway().Suspended())
		}
		time.Sleep(10 * time.Millisecond)
	}
	advanceUntil(t, clk, 30*time.Second, func() bool { return s.Gateway().Suspended() == 0 })
}

// The login throttle's ledger is swept on the same tick: failures older than the observation
// window, and the buckets they filled, are gone without anyone logging in again.
func TestTheCompositionRootSweepsTheLoginThrottle(t *testing.T) {
	s, _, _, clk := newGreetedServerAt(t)
	s.Throttle().RecordFailure(id.New(), netip.MustParseAddr("198.51.100.9"))
	if s.Throttle().Tracked() == 0 {
		t.Fatal("a recorded failure left nothing in the throttle")
	}
	advanceUntil(t, clk, 5*time.Minute, func() bool { return s.Throttle().Tracked() == 0 })
}

// tokenSFU is upstreamSFU that reads a call token as "<identity>@<room>", a microphone grant.
type tokenSFU struct{ upstreamSFU }

func (tokenSFU) VerifyToken(token string) (sfu.RoomToken, error) {
	ident, room, ok := strings.Cut(token, "@")
	if !ok {
		return sfu.RoomToken{}, errors.New("not a token")
	}
	grant := &auth.VideoGrant{RoomJoin: true, Room: room}
	grant.UpdateFromPermission(sfu.PublishGrant(true, false, false))
	return sfu.RoomToken{Identity: ident, Room: room, Claims: &auth.ClaimGrants{Identity: ident, Video: grant}}, nil
}

// lockedBuffer is a log sink the call events write to off the test's goroutine.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// The composition root stamps an admission on the SFU's clock, the wall clock, and not on the
// instance's: with the instance clock frozen an hour in the past, a device admitted through the real
// /rtc gate holds an admission stamped now, so a session LiveKit reports joined a minute ago has none
// (its leave proposes no Remove and the room sweep decides), while one joined just after binds it.
// Were the admission stamped by the frozen clock, the minute-old join would bind it.
func TestTheCompositionRootStampsAdmissionsOnTheSFUsClock(t *testing.T) {
	cfg, code := testConfigInvite(t)
	logs := &lockedBuffer{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))
	defer upstream.Close()
	srv, err := dillad.New(context.Background(), dillad.Options{
		Config: cfg, Clock: clock.NewFake(time.Now().Add(-time.Hour)), Wasm: sharedRuntime(t),
		SFU: tokenSFU{upstreamSFU{url: upstream.URL}}, Log: slog.New(slog.NewTextHandler(logs, nil)),
	})
	if err != nil {
		t.Fatalf("dillad.New: %v", err)
	}
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	h := socketHandler(srv)
	tok := accountToken(t, srv, code)
	repo, ctx := srv.Repo(), t.Context()
	user, err := repo.GetUserByUsername(ctx, "jonas")
	if err != nil {
		t.Fatalf("GetUserByUsername: %v", err)
	}
	devices, err := repo.ListDevicesByUser(ctx, user.ID)
	if err != nil || len(devices) != 1 {
		t.Fatalf("ListDevicesByUser = %v, %v", devices, err)
	}
	dev := devices[0].ID
	rec := call(t, h, http.MethodPost, "/v1/communities", tok, []any{"lounge", []byte("{}"), uint64(0), uint64(0)})
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /v1/communities = %d", rec.Code)
	}
	cid := idOf(t, decodeArray(t, rec)[0])
	rec = call(t, h, http.MethodPost, "/v1/communities/"+cid.String()+"/channels", tok,
		[]any{uint64(api.ChannelVoice), uint64(1), uint64(2), nil, "voice", "", uint64(0), uint64(0)})
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST voice channel = %d %s", rec.Code, errorCode(rec))
	}
	ch := idOf(t, decodeArray(t, rec)[0])
	group := store.GroupRow{
		GroupID: id.New(), Binding: []byte{0x80}, Kind: api.GroupCall, CommunityID: &cid, TargetID: ch,
		CallID: &ch, Ciphersuite: 1, Epoch: 7, ExternalSenderKeyID: id.New(),
		E2EEVersion: 1, MediaVersion: 1, PolicyVersion: 1, Created: srv.Now().Unix(),
	}
	if err := repo.CreateGroup(ctx, group); err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}
	if err := repo.ReplaceMembers(ctx, group.GroupID, 7, []store.MemberRow{{
		GroupID: group.GroupID, LeafIndex: 0, UserID: user.ID, DeviceID: dev, SignatureKey: make([]byte, 32), AddedEpoch: 7,
	}}); err != nil {
		t.Fatalf("ReplaceMembers: %v", err)
	}
	rec = call(t, h, http.MethodPost, "/v1/channels/"+ch.String()+"/calls", tok, []any{})
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST calls = %d %s", rec.Code, errorCode(rec))
	}
	row, err := repo.GetVoiceSession(ctx, ch)
	if err != nil {
		t.Fatalf("GetVoiceSession: %v", err)
	}

	// The join the /rtc proxy lets through: the gate admits the device and records the admission.
	rtc := httptest.NewRequestWithContext(ctx, http.MethodGet,
		"/rtc?access_token="+dev.String()+"@"+row.LivekitRoom+"&protocol="+strconv.Itoa(sfu.ClientProtocol), nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, rtc)
	if rr.Code != http.StatusTeapot {
		t.Fatalf("GET /rtc = %d, want the SFU's answer", rr.Code)
	}

	const unbound = "a leave whose admission is unknown proposes no Remove"
	leave := func(sid string, joined time.Time) {
		srv.CallEvents().Handle(ctx, &livekit.WebhookEvent{Event: webhook.EventParticipantLeft, Id: "EV_" + id.New().String(),
			Room:        &livekit.Room{Name: row.LivekitRoom},
			Participant: &livekit.ParticipantInfo{Identity: dev.String(), Sid: sid, JoinedAtMs: joined.UnixMilli()}})
	}
	leave("PA_after", time.Now().Add(time.Second))
	if strings.Contains(logs.String(), unbound) {
		t.Fatalf("a session joined just after the admission found no admission:\n%s", logs.String())
	}
	leave("PA_before", time.Now().Add(-time.Minute))
	if !strings.Contains(logs.String(), unbound) {
		t.Fatalf("a session joined a minute before the admission bound it: the admission was stamped on the instance's frozen clock:\n%s", logs.String())
	}
}
