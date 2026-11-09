package api_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"sort"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/auth"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/config"
	"github.com/dilla-chat/dilla/internal/gateway"
	"github.com/dilla-chat/dilla/internal/gateway/sockettest"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/server"
	"github.com/dilla-chat/dilla/internal/store"
	"github.com/dilla-chat/dilla/internal/store/sqlite"
	"github.com/dilla-chat/dilla/internal/store/sqlite/sqlitetest"
)

// socketEnv is the real API mux and session store, a gateway whose request frames reach that mux's
// socket routes through api.SocketDispatcher, and an HTTP server over the same mux.
type socketEnv struct {
	t    *testing.T
	deps api.Deps
	mux  *server.Mux
	// routes registers through the composition root's session middleware and rate meter.
	routes *server.Mux
	clk    *clock.Fake
	gw     *gateway.Gateway
	gwSrv  *httptest.Server
	http   *httptest.Server
	disp   *api.SocketDispatcher
}

func newSocketEnv(t *testing.T, tune func(*config.Config)) *socketEnv {
	t.Helper()
	return newSocketEnvWith(t, tune, nil, nil)
}

// newSocketEnvWith is newSocketEnv with hooks into the dispatcher's and the gateway's options.
func newSocketEnvWith(t *testing.T, tune func(*config.Config), dispatch func(*api.SocketDispatchOptions), gw func(*gateway.Options)) *socketEnv {
	t.Helper()
	path := filepath.Join(t.TempDir(), "socket.db")
	sqlitetest.MigratedFile(t, path)
	write, err := sqlite.OpenWrite(path)
	if err != nil {
		t.Fatalf("OpenWrite: %v", err)
	}
	read, err := sqlite.OpenRead(path)
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	repo := sqlite.New(write, read)
	t.Cleanup(func() { _ = repo.Close() })
	if tune == nil {
		tune = func(c *config.Config) { c.Limits.Rate.Enabled = false }
	}
	var mux *server.Mux
	h, deps := newTestAPIFullWithRepoAndMount(t, repo, tune, nil, func(m *server.Mux, _ api.Deps) { mux = m })
	clk, ok := deps.Clock.(*clock.Fake)
	if !ok {
		t.Fatal("the API harness clock is not a clock.Fake")
	}
	e := &socketEnv{t: t, deps: deps, mux: mux, clk: clk,
		routes: mux.Wrapped(api.SessionRoute(deps.Sessions, deps.Limiter))}
	dopts := api.SocketDispatchOptions{Clock: clk}
	if dispatch != nil {
		dispatch(&dopts)
	}
	e.disp = api.NewSocketDispatcher(mux, dopts)
	gopts := gateway.Options{
		Store: deps.Repo, Auth: deps.Sessions, Clock: clk, Log: deps.Log,
		WireVersions: []uint64{auth.WireVersion}, E2EEVersions: []uint64{1}, MediaVersions: []uint64{1},
		Requests:        e.disp,
		FramesPerSecond: 1 << 20, FrameBurst: 1 << 20,
	}
	if gw != nil {
		gw(&gopts)
	}
	e.gw = gateway.New(gopts)
	e.gwSrv = httptest.NewServer(e.gw.Handler())
	t.Cleanup(e.gwSrv.Close)
	e.http = httptest.NewServer(h)
	t.Cleanup(e.http.Close)
	return e
}

func (e *socketEnv) dial(bearer string) *sockettest.Client {
	e.t.Helper()
	return sockettest.Dial(e.t, e.gwSrv.URL, bearer)
}

func (e *socketEnv) httpDo(method, path, bearer string, body []byte) (int, []byte) {
	e.t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, e.http.URL+path, bytes.NewReader(body))
	if err != nil {
		e.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/cbor")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func sockDo(t *testing.T, c *sockettest.Client, method, route string, body, idem []byte) gateway.SocketResponse {
	t.Helper()
	resp, err := c.Do(method, route, body, idem)
	if err != nil {
		t.Fatalf("%s %s: %v", method, route, err)
	}
	return resp
}

func sockErrorCode(t *testing.T, body []byte) (string, any) {
	t.Helper()
	var arr []any
	if err := cborx.Unmarshal(body, &arr); err != nil || len(arr) < 3 {
		t.Fatalf("not an error body: %x (%v)", body, err)
	}
	code, _ := arr[0].(string)
	return code, arr[2]
}

// sockEchoHandler answers the matched pattern and the session's device and token hash.
func sockEchoHandler(w http.ResponseWriter, r *http.Request) {
	s, ok := auth.FromContext(r.Context())
	if !ok {
		server.WriteError(w, server.Errorf(server.CodeUnauthenticated, ""))
		return
	}
	_ = server.EncodeBody(w, http.StatusOK, []any{r.Pattern, s.DeviceID, s.TokenHash})
}

func TestSocketRequestReachesTheSameHandler(t *testing.T) {
	e := newSocketEnv(t, nil)
	e.routes.Socket("GET /v1/test/echo", http.HandlerFunc(sockEchoHandler))
	// The direct call: the same handler behind the same middleware on an HTTP mux.
	direct := server.NewMux()
	direct.Wrapped(api.SessionRoute(e.deps.Sessions, e.deps.Limiter)).Handle("GET /v1/test/echo", http.HandlerFunc(sockEchoHandler))

	_, dev, bearer := seedAPISession(t, e.deps)
	c := e.dial(bearer)
	resp := sockDo(t, c, http.MethodGet, "/v1/test/echo", nil, nil)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/test/echo", nil)
	req.Header.Set("Authorization", "Bearer "+bearer)
	rec := httptest.NewRecorder()
	direct.ServeHTTP(rec, req)

	if resp.Status != rec.Code || !bytes.Equal(resp.Body, rec.Body.Bytes()) {
		t.Fatalf("socket answered %d %x, the direct call %d %x", resp.Status, resp.Body, rec.Code, rec.Body.Bytes())
	}
	var got []any
	if err := cborx.Unmarshal(resp.Body, &got); err != nil {
		t.Fatal(err)
	}
	if got[0] != "GET /v1/test/echo" {
		t.Fatalf("pattern = %v", got[0])
	}
	if !bytes.Equal(got[1].([]byte), dev.ID[:]) || !bytes.Equal(got[2].([]byte), auth.TokenHash(bearer)) {
		t.Fatalf("the handler saw device %x hash %x, want the connection's own session", got[1], got[2])
	}
}

func TestSocketRouteIsNotServedOverHTTP(t *testing.T) {
	e := newSocketEnv(t, nil)
	e.routes.Socket("GET /v1/test/echo", http.HandlerFunc(sockEchoHandler))
	_, _, bearer := seedAPISession(t, e.deps)
	if status, _ := e.httpDo(http.MethodGet, "/v1/test/echo", bearer, nil); status != http.StatusNotFound {
		t.Fatalf("GET /v1/test/echo over HTTP = %d, want 404", status)
	}
	if !slices.Contains(e.mux.SocketPatterns(), "GET /v1/test/echo") || slices.Contains(e.mux.Patterns(), "GET /v1/test/echo") {
		t.Fatalf("socket patterns %v, HTTP patterns hold it: %v", e.mux.SocketPatterns(), slices.Contains(e.mux.Patterns(), "GET /v1/test/echo"))
	}
}

func TestRevokedSessionIsRefusedOnTheNextRequest(t *testing.T) {
	e := newSocketEnv(t, nil)
	e.routes.Socket("GET /v1/test/echo", http.HandlerFunc(sockEchoHandler))
	_, dev, bearer := seedAPISession(t, e.deps)
	c := e.dial(bearer)
	if resp := sockDo(t, c, http.MethodGet, "/v1/test/echo", nil, nil); resp.Status != http.StatusOK {
		t.Fatalf("first request = %d", resp.Status)
	}
	if err := e.deps.Sessions.RevokeDevice(context.Background(), dev.ID); err != nil {
		t.Fatal(err)
	}
	resp := sockDo(t, c, http.MethodGet, "/v1/test/echo", nil, nil)
	_, resolveErr := e.deps.Sessions.Resolve(context.Background(), bearer)
	var want *server.Error
	_ = errors.As(resolveErr, &want)
	if want == nil {
		t.Fatalf("Resolve after the revoke = %v", resolveErr)
	}
	if code, _ := sockErrorCode(t, resp.Body); resp.Status != http.StatusUnauthorized || code != string(want.Code) {
		t.Fatalf("after the revoke = %d %s, want 401 %s", resp.Status, code, want.Code)
	}
}

func TestBearerHeaderCannotForgeSocketCaller(t *testing.T) {
	e := newSocketEnv(t, nil)
	e.routes.Socket("GET /v1/test/echo", http.HandlerFunc(sockEchoHandler))
	e.routes.Handle("GET /v1/test/echo-http", http.HandlerFunc(sockEchoHandler))
	_, _, bearer := seedAPISession(t, e.deps)
	// The socket route does not exist over HTTP, whatever the request carries.
	for _, h := range []string{"X-Socket-Caller", "X-Dilla-Socket", "Upgrade"} {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, e.http.URL+"/v1/test/echo", nil)
		req.Header.Set(h, hex.EncodeToString(auth.TokenHash(bearer)))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("with %s the socket route answered %d over HTTP", h, resp.StatusCode)
		}
	}
	// An enrolled HTTP route without an Authorization header stays 401, with or without a token
	// hash in the query or a header.
	for _, path := range []string{"/v1/test/echo-http", "/v1/test/echo-http?socket_caller=1&token_hash=" + hex.EncodeToString(auth.TokenHash(bearer))} {
		status, body := e.httpDo(http.MethodGet, path, "", nil)
		if code, _ := sockErrorCode(t, body); status != http.StatusUnauthorized || code != string(server.CodeUnauthenticated) {
			t.Fatalf("header-less GET %s = %d %s, want 401", path, status, code)
		}
	}
}

func TestRateLimitsApplyPerRoute(t *testing.T) {
	e := newSocketEnv(t, func(c *config.Config) {
		c.Limits.Rate.Enabled = true
		c.Limits.Rate.ReadPerSecond = 0.01
		c.Limits.Rate.ReadBurst = 2
	})
	e.routes.Socket("GET /v1/test/echo", http.HandlerFunc(sockEchoHandler))
	e.routes.Handle("GET /v1/test/echo-http", http.HandlerFunc(sockEchoHandler))
	_, _, bearer := seedAPISession(t, e.deps)
	c := e.dial(bearer)
	// Spend the device's read bucket over HTTP until it refuses.
	var httpStatus int
	var httpBody []byte
	for range 10 {
		httpStatus, httpBody = e.httpDo(http.MethodGet, "/v1/test/echo-http", bearer, nil)
		if httpStatus != http.StatusOK {
			break
		}
	}
	if httpStatus != http.StatusTooManyRequests {
		t.Fatalf("HTTP never refused: %d", httpStatus)
	}
	resp := sockDo(t, c, http.MethodGet, "/v1/test/echo", nil, nil)
	if resp.Status != httpStatus || !bytes.Equal(resp.Body, httpBody) {
		t.Fatalf("socket refusal %d %x, HTTP refusal %d %x", resp.Status, resp.Body, httpStatus, httpBody)
	}
	if code, retry := sockErrorCode(t, resp.Body); code != string(server.CodeRateLimited) || retry == nil {
		t.Fatalf("socket refusal = %s retry %v", code, retry)
	}
}

func TestResponsesMayArriveOutOfOrder(t *testing.T) {
	e := newSocketEnv(t, nil)
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	e.routes.Socket("GET /v1/test/slow", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		<-release
		_ = server.EncodeBody(w, http.StatusOK, "slow")
	}))
	e.routes.Socket("GET /v1/test/echo", http.HandlerFunc(sockEchoHandler))
	_, _, bearer := seedAPISession(t, e.deps)
	c := e.dial(bearer)
	slow, err := c.Send(http.MethodGet, "/v1/test/slow", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	<-entered
	fast := sockDo(t, c, http.MethodGet, "/v1/test/echo", nil, nil)
	if fast.Status != http.StatusOK {
		t.Fatalf("fast = %d", fast.Status)
	}
	close(release)
	if resp, err := c.Await(slow, 10*time.Second); err != nil || resp.Status != http.StatusOK {
		t.Fatalf("slow = %+v, %v", resp, err)
	}
	if order := c.Order(); len(order) != 2 || order[0] != fast.ID || order[1] != slow {
		t.Fatalf("arrival order %v, want fast %d before slow %d", order, fast.ID, slow)
	}
}

// blockingRoute registers POST route; each request waits on release and counts its entry.
func sockBlockingRoute(e *socketEnv, route string) (entered *atomic.Int64, release chan struct{}) {
	entered = &atomic.Int64{}
	release = make(chan struct{}, 1024)
	e.routes.Socket("POST "+route, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered.Add(1)
		<-release
		w.WriteHeader(http.StatusNoContent)
	}))
	return entered, release
}

func sockWaitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestSixtyFifthInFlightRequestGetsBusy(t *testing.T) {
	e := newSocketEnv(t, nil)
	entered, release := sockBlockingRoute(e, "/v1/test/block")
	_, _, bearer := seedAPISession(t, e.deps)
	c := e.dial(bearer)
	ids := make([]uint64, 0, gateway.MaxInFlightRequests)
	for range gateway.MaxInFlightRequests {
		reqID, err := c.Send(http.MethodPost, "/v1/test/block", nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, reqID)
	}
	sockWaitFor(t, "64 requests in the handler", func() bool { return entered.Load() == gateway.MaxInFlightRequests })
	busy := sockDo(t, c, http.MethodPost, "/v1/test/block", nil, nil)
	if code, _ := sockErrorCode(t, busy.Body); busy.Status != http.StatusTooManyRequests || code != string(server.CodeBusy) {
		t.Fatalf("65th = %d %s, want 429 E_BUSY", busy.Status, code)
	}
	release <- struct{}{}
	sockWaitFor(t, "one request to finish", func() bool {
		for _, reqID := range ids {
			if slices.Contains(c.Order(), reqID) {
				return true
			}
		}
		return false
	})
	next, err := c.Send(http.MethodPost, "/v1/test/block", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	sockWaitFor(t, "the next request to be accepted", func() bool { return entered.Load() == gateway.MaxInFlightRequests+1 })
	for range gateway.MaxInFlightRequests {
		release <- struct{}{}
	}
	if resp, err := c.Await(next, 10*time.Second); err != nil || resp.Status != http.StatusNoContent {
		t.Fatalf("next = %+v, %v", resp, err)
	}
}

func TestRequestOverTheConnectionByteBudgetGetsBusy(t *testing.T) {
	e := newSocketEnv(t, nil)
	entered := &atomic.Int64{}
	release := make(chan struct{}, 8)
	e.routes.Socket("POST /v1/test/big", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := server.ReadBody(w, r, gateway.MaxRequestBodyBytes); err != nil {
			server.WriteError(w, err)
			return
		}
		entered.Add(1)
		<-release
		w.WriteHeader(http.StatusNoContent)
	}))
	_, _, bearer := seedAPISession(t, e.deps)
	c := e.dial(bearer)
	big := bytes.Repeat([]byte{7}, 15<<20)
	first, err := c.Send(http.MethodPost, "/v1/test/big", big, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := c.Send(http.MethodPost, "/v1/test/big", big, nil)
	if err != nil {
		t.Fatal(err)
	}
	sockWaitFor(t, "two 15 MiB requests in the handler", func() bool { return entered.Load() == 2 })
	busy := sockDo(t, c, http.MethodPost, "/v1/test/big", bytes.Repeat([]byte{7}, 3<<20), nil)
	if code, _ := sockErrorCode(t, busy.Body); busy.Status != http.StatusTooManyRequests || code != string(server.CodeBusy) {
		t.Fatalf("over the 32 MiB budget = %d %s, want 429 E_BUSY", busy.Status, code)
	}
	release <- struct{}{}
	release <- struct{}{}
	for _, reqID := range []uint64{first, second} {
		if resp, err := c.Await(reqID, 30*time.Second); err != nil || resp.Status != http.StatusNoContent {
			t.Fatalf("request %d = %+v, %v", reqID, resp.Status, err)
		}
	}
}

func TestTwoMiBBodyRoundTripsAndOverTheRouteCapMatchesHTTP(t *testing.T) {
	e := newSocketEnv(t, nil)
	const dsCap = 2 << 20
	echoBody := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := server.ReadBody(w, r, dsCap)
		if err != nil {
			server.WriteError(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/cbor")
		_, _ = w.Write(b)
	})
	e.routes.Socket("POST /v1/test/body", echoBody)
	e.routes.Handle("POST /v1/test/body-http", echoBody)
	_, _, bearer := seedAPISession(t, e.deps)
	c := e.dial(bearer)

	body := bytes.Repeat([]byte{0x5a}, dsCap)
	resp := sockDo(t, c, http.MethodPost, "/v1/test/body", body, nil)
	if resp.Status != http.StatusOK || !bytes.Equal(resp.Body, body) {
		t.Fatalf("2 MiB round trip = %d, %d bytes", resp.Status, len(resp.Body))
	}

	over := append(body, 0)
	sock := sockDo(t, c, http.MethodPost, "/v1/test/body", over, nil)
	httpStatus, httpBody := e.httpDo(http.MethodPost, "/v1/test/body-http", bearer, over)
	if sock.Status != httpStatus || !bytes.Equal(sock.Body, httpBody) {
		t.Fatalf("over the cap: socket %d %x, HTTP %d %x", sock.Status, sock.Body, httpStatus, httpBody)
	}
	if code, _ := sockErrorCode(t, sock.Body); sock.Status != http.StatusRequestEntityTooLarge || code != string(server.CodeTooLarge) {
		t.Fatalf("over the cap = %d %s", sock.Status, code)
	}
}

func TestResponseOverTheRouteCapIsFrameLimit(t *testing.T) {
	e := newSocketEnv(t, nil)
	e.routes.SocketWithCap("GET /v1/test/huge", 1024, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(bytes.Repeat([]byte{1}, 2048))
	}))
	_, _, bearer := seedAPISession(t, e.deps)
	c := e.dial(bearer)
	resp := sockDo(t, c, http.MethodGet, "/v1/test/huge", nil, nil)
	if code, _ := sockErrorCode(t, resp.Body); code != "E_FRAME_LIMIT" {
		t.Fatalf("over-cap response = %d %s, want E_FRAME_LIMIT", resp.Status, code)
	}
}

func TestMutationRetryWithSameIdemIsAnsweredFromTheReplayRecord(t *testing.T) {
	e := newSocketEnv(t, nil)
	var runs atomic.Int64
	e.routes.Socket("POST /v1/test/count", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := runs.Add(1)
		_ = server.EncodeBody(w, http.StatusCreated, n)
	}))
	_, _, bearer := seedAPISession(t, e.deps)
	_, _, other := seedAPISession(t, e.deps)
	idem := []byte("0123456789abcdef")

	first := sockDo(t, e.dial(bearer), http.MethodPost, "/v1/test/count", nil, idem)
	if first.Status != http.StatusCreated || runs.Load() != 1 {
		t.Fatalf("first = %d, runs %d", first.Status, runs.Load())
	}
	// The same device on a new connection, inside the window.
	e.clk.Advance(4 * time.Minute)
	retry := sockDo(t, e.dial(bearer), http.MethodPost, "/v1/test/count", nil, idem)
	if retry.Status != first.Status || !bytes.Equal(retry.Body, first.Body) || runs.Load() != 1 {
		t.Fatalf("retry = %d %x (runs %d), want the recorded %d %x", retry.Status, retry.Body, runs.Load(), first.Status, first.Body)
	}
	// Another device's identical idem is its own request.
	if resp := sockDo(t, e.dial(other), http.MethodPost, "/v1/test/count", nil, idem); resp.Status != http.StatusCreated || runs.Load() != 2 {
		t.Fatalf("other device = %d, runs %d", resp.Status, runs.Load())
	}
	// Past the five-minute window the record is gone.
	e.clk.Advance(time.Minute + time.Second)
	if resp := sockDo(t, e.dial(bearer), http.MethodPost, "/v1/test/count", nil, idem); resp.Status != http.StatusCreated || runs.Load() != 3 {
		t.Fatalf("after the window = %d, runs %d", resp.Status, runs.Load())
	}
}

// The replay record lives in the route's innermost leaf, behind the whole middleware chain: a
// revoked device's retry with the same idem gets the middleware's 401, not the record, and so does
// a retry whose body differs (the 409 for a reused idem answers only once the middleware passed).
func TestRevokedSessionRetryWithSameIdemIsRefused(t *testing.T) {
	e := newSocketEnv(t, nil)
	var runs atomic.Int64
	e.routes.Socket("POST /v1/test/count", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = server.EncodeBody(w, http.StatusCreated, runs.Add(1))
	}))
	_, dev, bearer := seedAPISession(t, e.deps)
	c := e.dial(bearer)
	idem := []byte("idem-then-revoked")
	if resp := sockDo(t, c, http.MethodPost, "/v1/test/count", nil, idem); resp.Status != http.StatusCreated {
		t.Fatalf("first = %d", resp.Status)
	}
	if err := e.deps.Sessions.RevokeDevice(context.Background(), dev.ID); err != nil {
		t.Fatal(err)
	}
	resp := sockDo(t, c, http.MethodPost, "/v1/test/count", nil, idem)
	if code, _ := sockErrorCode(t, resp.Body); resp.Status != http.StatusUnauthorized || code != string(server.CodeUnauthenticated) {
		t.Fatalf("revoked retry = %d %s, want 401", resp.Status, code)
	}
	mismatch := sockDo(t, c, http.MethodPost, "/v1/test/count", []byte{1}, idem)
	if code, _ := sockErrorCode(t, mismatch.Body); mismatch.Status != http.StatusUnauthorized || code != string(server.CodeUnauthenticated) {
		t.Fatalf("revoked retry with another body = %d %s, want the middleware's 401 before any 409", mismatch.Status, code)
	}
	if runs.Load() != 1 {
		t.Fatalf("runs = %d", runs.Load())
	}
}

// A live session that reuses an idem for a different request gets 409.
func TestIdemReusedForAnotherRequestIsConflict(t *testing.T) {
	e := newSocketEnv(t, nil)
	e.routes.Socket("POST /v1/test/count", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
	}))
	_, _, bearer := seedAPISession(t, e.deps)
	c := e.dial(bearer)
	idem := []byte("idem-reused")
	if resp := sockDo(t, c, http.MethodPost, "/v1/test/count", nil, idem); resp.Status != http.StatusCreated {
		t.Fatalf("first = %d", resp.Status)
	}
	if resp := sockDo(t, c, http.MethodPost, "/v1/test/count", []byte{1}, idem); resp.Status != http.StatusConflict {
		t.Fatalf("reuse = %d, want 409", resp.Status)
	}
}

func TestFactorOwedRetryWithSameIdemGetsTheMiddlewareRefusal(t *testing.T) {
	e := newSocketEnv(t, nil)
	var runs atomic.Int64
	e.routes.Socket("POST /v1/test/count", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = server.EncodeBody(w, http.StatusCreated, runs.Add(1))
	}))
	user, _, bearer := seedAPISession(t, e.deps)
	c := e.dial(bearer)
	idem := []byte("idem-then-owed")
	if resp := sockDo(t, c, http.MethodPost, "/v1/test/count", nil, idem); resp.Status != http.StatusCreated {
		t.Fatalf("first = %d", resp.Status)
	}
	if err := e.deps.Repo.SetFactorOwed(context.Background(), user.ID, true); err != nil {
		t.Fatal(err)
	}
	resp := sockDo(t, c, http.MethodPost, "/v1/test/count", nil, idem)
	if code, _ := sockErrorCode(t, resp.Body); resp.Status != http.StatusForbidden || code != string(server.CodeFactorRequired) || runs.Load() != 1 {
		t.Fatalf("owed retry = %d %s (runs %d), want 403 E_FACTOR_REQUIRED", resp.Status, code, runs.Load())
	}
}

// seedSocketBot is a ready managed Bot with a bound enrolled session: its token and installation.
func seedSocketBot(t *testing.T, d api.Deps) (string, store.BotInstallationRow) {
	t.Helper()
	ctx := context.Background()
	now := d.Clock.Now().Unix()
	owner, _, _ := seedAPIDevice(t, d)
	communityID := id.New()
	if err := d.Repo.CreateCommunity(ctx, store.CommunityRow{ID: communityID, Owner: owner.ID, Name: "socket-bot", PolicyJSON: []byte(`{}`), PolicyVersion: 1, Created: now}); err != nil {
		t.Fatal(err)
	}
	botID, deviceID, installationID, runtimeID := id.New(), id.New(), id.New(), id.New()
	verified := now
	if err := d.Repo.CreateUser(ctx, store.UserRow{ID: botID, Username: "bot" + botID.String()[:8], Display: "Bot", Kind: 1, UMKPub: make([]byte, 32), SSKPub: make([]byte, 32), SigUMKSSK: make([]byte, 64), Created: now}); err != nil {
		t.Fatal(err)
	}
	if err := d.Repo.CreateDevice(ctx, store.DeviceRow{ID: deviceID, UserID: botID, DSKPub: uniqueAPITestDSK(t), CredentialBlob: []byte{1}, VerifiedAt: &verified, Created: now, LastSeen: now}); err != nil {
		t.Fatal(err)
	}
	if err := d.Repo.PutMember(ctx, store.MemberOfCommunityRow{CommunityID: communityID, UserID: botID, Joined: now}); err != nil {
		t.Fatal(err)
	}
	if err := d.Repo.CreateBotRuntime(ctx, store.BotRuntimeRow{ID: runtimeID, TokenHash: bytes.Repeat([]byte{0x29}, 32), Created: now, Rotated: now}); err != nil {
		t.Fatal(err)
	}
	inst := store.BotInstallationRow{ID: installationID, CommunityID: communityID, RuntimeID: &runtimeID, UserID: &botID, DeviceID: &deviceID, State: store.BotInstallationReady, Revision: 1, ApprovedBy: owner.ID, ApprovedAt: now, CredentialVerifiedAt: &verified, Created: now, Updated: now}
	if err := d.Repo.CreateBotInstallation(ctx, inst); err != nil {
		t.Fatal(err)
	}
	token := "socket-ready-bot-session-" + botID.String()
	if err := d.Repo.CreateSession(ctx, store.SessionRow{TokenHash: auth.TokenHash(token), DeviceID: deviceID, UserID: botID, Scope: uint8(auth.ScopeEnrolled), Created: now, Expires: now + 3600, IdleExpires: now + 3600}); err != nil {
		t.Fatal(err)
	}
	if err := d.Repo.CreateBotSessionBinding(ctx, store.BotSessionBindingRow{TokenHash: auth.TokenHash(token), InstallationID: installationID, UserID: botID, DeviceID: deviceID, Created: now}); err != nil {
		t.Fatal(err)
	}
	return token, inst
}

// A Bot whose installation stopped being ready retries with the same idem: the middleware's Bot
// eligibility and allow-list checks refuse it exactly as they refuse the same request over HTTP.
func TestIneligibleBotRetryWithSameIdemGetsTheMiddlewareRefusal(t *testing.T) {
	e := newSocketEnv(t, nil)
	var runs atomic.Int64
	// The route is the real socket-only ready route (Bot v2 task 14); the HTTP twin mounts a stub behind the same
	// middleware, to read the refusal an ineligible Bot gets before any handler runs.
	const pattern = "POST /v1/bot-runtime/installations/{installation}/ready"
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = server.EncodeBody(w, http.StatusCreated, runs.Add(1))
	})
	twin := server.NewMux()
	twin.Wrapped(api.SessionRoute(e.deps.Sessions, e.deps.Limiter)).Handle(pattern, h)
	token, inst := seedSocketBot(t, e.deps)
	route := "/v1/bot-runtime/installations/" + inst.ID.String() + "/ready"
	c := e.dial(token)
	idem := []byte("idem-then-disabled")
	first := sockDo(t, c, http.MethodPost, route, nil, idem)
	if first.Status >= 500 || first.Status == http.StatusUnauthorized || first.Status == http.StatusForbidden {
		t.Fatalf("first = %d %x, want an answer the handler gave (recorded for a retry)", first.Status, first.Body)
	}
	next := inst
	next.Revision, next.State, next.Updated = 2, store.BotInstallationDisabled, inst.Updated+1
	if err := e.deps.Repo.UpdateBotInstallation(context.Background(), inst.ID, 1, next); err != nil {
		t.Fatal(err)
	}
	resp := sockDo(t, c, http.MethodPost, route, nil, idem)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, route, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/cbor")
	rec := httptest.NewRecorder()
	twin.ServeHTTP(rec, req)
	if resp.Status == first.Status || resp.Status != rec.Code || !bytes.Equal(resp.Body, rec.Body.Bytes()) || runs.Load() != 0 {
		t.Fatalf("disabled Bot retry = %d %x (first %d), HTTP %d %x, runs %d", resp.Status, resp.Body, first.Status, rec.Code, rec.Body.Bytes(), runs.Load())
	}
}

// I1: a socket route registered without the session middleware fails closed: the innermost guard
// answers 401 and the handler never runs.
func TestRawSocketRouteWithoutSessionMiddlewareIsRefused(t *testing.T) {
	e := newSocketEnv(t, nil)
	var ran atomic.Bool
	e.mux.Socket("POST /v1/test/raw", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ran.Store(true)
		w.WriteHeader(http.StatusNoContent)
	}))
	_, _, bearer := seedAPISession(t, e.deps)
	resp := sockDo(t, e.dial(bearer), http.MethodPost, "/v1/test/raw", nil, []byte("raw-idem"))
	if code, _ := sockErrorCode(t, resp.Body); resp.Status != http.StatusUnauthorized || code != string(server.CodeUnauthenticated) || ran.Load() {
		t.Fatalf("raw socket route = %d %s, ran %v; want 401 and never run", resp.Status, code, ran.Load())
	}
}

// M1: a duplicate that outwaits the dispatch deadline is answered 503 E_UNAVAILABLE and nothing is
// recorded for it.
func TestDuplicateIdemPastTheDeadlineIsUnavailable(t *testing.T) {
	e := newSocketEnvWith(t, nil, func(o *api.SocketDispatchOptions) { o.Deadline = 300 * time.Millisecond }, nil)
	release := make(chan struct{})
	var runs atomic.Int64
	e.routes.Socket("POST /v1/test/stuck", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		runs.Add(1)
		<-release
		w.WriteHeader(http.StatusCreated)
	}))
	_, _, bearer := seedAPISession(t, e.deps)
	a, b := e.dial(bearer), e.dial(bearer)
	idem := []byte("idem-stuck")
	first, err := a.Send(http.MethodPost, "/v1/test/stuck", nil, idem)
	if err != nil {
		t.Fatal(err)
	}
	sockWaitFor(t, "the first request in the handler", func() bool { return runs.Load() == 1 })
	resp := sockDo(t, b, http.MethodPost, "/v1/test/stuck", nil, idem)
	if code, _ := sockErrorCode(t, resp.Body); resp.Status != http.StatusServiceUnavailable || code != string(server.CodeUnavailable) {
		t.Fatalf("duplicate past the deadline = %d %s, want 503 E_UNAVAILABLE", resp.Status, code)
	}
	close(release)
	if r, err := a.Await(first, 10*time.Second); err != nil || r.Status != http.StatusCreated {
		t.Fatalf("first = %+v, %v", r, err)
	}
}

// M1: a dispatcher that panics answers 500 and frees its slot: more than 64 panicking requests in a
// row are each answered, never E_BUSY.
func TestAPanickingDispatchAnswers500AndFreesItsSlot(t *testing.T) {
	e := newSocketEnvWith(t, nil, nil, func(o *gateway.Options) { o.Requests = panicDispatcher{} })
	_, _, bearer := seedAPISession(t, e.deps)
	c := e.dial(bearer)
	for i := range gateway.MaxInFlightRequests + 6 {
		if resp := sockDo(t, c, http.MethodGet, "/v1/test/any", nil, nil); resp.Status != http.StatusInternalServerError {
			t.Fatalf("request %d = %d, want 500", i, resp.Status)
		}
	}
}

type panicDispatcher struct{}

func (panicDispatcher) Dispatch(context.Context, gateway.SocketCaller, gateway.SocketRequest) gateway.SocketResponse {
	panic("dispatcher fault")
}

// M1: Shutdown waits for the requests still running on the gateway's workers.
func TestShutdownWaitsForRunningSocketRequests(t *testing.T) {
	e := newSocketEnv(t, nil)
	entered, release := sockBlockingRoute(e, "/v1/test/block")
	_, _, bearer := seedAPISession(t, e.deps)
	c := e.dial(bearer)
	if _, err := c.Send(http.MethodPost, "/v1/test/block", nil, nil); err != nil {
		t.Fatal(err)
	}
	sockWaitFor(t, "the request in the handler", func() bool { return entered.Load() == 1 })
	done := make(chan struct{})
	go func() {
		_ = e.gw.Shutdown(context.Background())
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("Shutdown returned while a socket request was still running")
	case <-time.After(200 * time.Millisecond):
	}
	release <- struct{}{}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Shutdown did not return once the request finished")
	}
}

// Controller ruling M2: request frames do not spend the inbound frame bucket (60/s, burst 120);
// their limits are the in-flight cap, the byte budget and each route's meter. Other ops still do.
func TestRequestFramesAreExemptFromTheInboundFrameBucket(t *testing.T) {
	e := newSocketEnvWith(t, nil, nil, func(o *gateway.Options) { o.FramesPerSecond, o.FrameBurst = 60, 120 })
	e.routes.Socket("GET /v1/test/echo", http.HandlerFunc(sockEchoHandler))
	_, _, bearer := seedAPISession(t, e.deps)
	c := e.dial(bearer)
	for i := range 200 {
		if resp := sockDo(t, c, http.MethodGet, "/v1/test/echo", nil, nil); resp.Status != http.StatusOK {
			t.Fatalf("request %d = %d", i, resp.Status)
		}
	}
	// The fake clock never refills the bucket, so 121 heartbeats spend it and close the socket.
	for i := range 121 {
		hb, _ := cborx.MarshalFrame(uint64(gateway.OpHeartbeat), uint64(i+1), nil, mustCBOR(t, []any{uint64(0), uint64(0)}))
		if err := c.SendRaw(hb); err != nil {
			break
		}
	}
	select {
	case <-c.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("121 heartbeats did not spend the inbound bucket")
	}
	if got := c.CloseStatus(); got != websocket.StatusCode(gateway.CloseRateLimited) {
		t.Fatalf("closed with %d, want %d (inbound frame rate)", got, gateway.CloseRateLimited)
	}
}

func TestDuplicateIdemWhileRunningWaitsForTheFirst(t *testing.T) {
	e := newSocketEnv(t, nil)
	var runs atomic.Int64
	release := make(chan struct{})
	e.routes.Socket("POST /v1/test/once", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := runs.Add(1)
		<-release
		_ = server.EncodeBody(w, http.StatusCreated, n)
	}))
	_, _, bearer := seedAPISession(t, e.deps)
	idem := []byte("idem-while-running")
	a, b := e.dial(bearer), e.dial(bearer)
	first, err := a.Send(http.MethodPost, "/v1/test/once", nil, idem)
	if err != nil {
		t.Fatal(err)
	}
	sockWaitFor(t, "the first request in the handler", func() bool { return runs.Load() == 1 })
	second, err := b.Send(http.MethodPost, "/v1/test/once", nil, idem)
	if err != nil {
		t.Fatal(err)
	}
	// The duplicate is parked on the running entry, not run and not answered yet.
	sockWaitFor(t, "the duplicate to wait on the first", func() bool { return e.disp.ReplayWaitersForTest() == 1 })
	if slices.Contains(b.Order(), second) {
		t.Fatal("the duplicate was answered before the first finished")
	}
	close(release)
	r1, err1 := a.Await(first, 10*time.Second)
	r2, err2 := b.Await(second, 10*time.Second)
	if err1 != nil || err2 != nil || r1.Status != http.StatusCreated || !bytes.Equal(r1.Body, r2.Body) || r1.Status != r2.Status || runs.Load() != 1 {
		t.Fatalf("first %+v %v, duplicate %+v %v, runs %d", r1, err1, r2, err2, runs.Load())
	}
}

// TestSocketRequestCostsNoMoreThanHTTP is ruling 2: the REST forms are gone, so the comparison is
// made here, against the same handler mounted on an httptest keep-alive server inside the test.
func TestSocketRequestCostsNoMoreThanHTTP(t *testing.T) {
	e := newSocketEnv(t, nil)
	e.routes.Socket("GET /v1/test/echo", http.HandlerFunc(sockEchoHandler))
	httpMux := server.NewMux()
	httpMux.Wrapped(api.SessionRoute(e.deps.Sessions, e.deps.Limiter)).Handle("GET /v1/test/echo", http.HandlerFunc(sockEchoHandler))
	keepAlive := httptest.NewServer(httpMux)
	t.Cleanup(keepAlive.Close)
	client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 4}}
	t.Cleanup(client.CloseIdleConnections)

	_, _, bearer := seedAPISession(t, e.deps)
	c := e.dial(bearer)
	viaHTTP := func() {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, keepAlive.URL+"/v1/test/echo", nil)
		req.Header.Set("Authorization", "Bearer "+bearer)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("HTTP = %d", resp.StatusCode)
		}
	}
	viaSocket := func() {
		if resp := sockDo(t, c, http.MethodGet, "/v1/test/echo", nil, nil); resp.Status != http.StatusOK {
			t.Fatalf("socket = %d", resp.Status)
		}
	}
	for range 20 { // warm both paths
		viaHTTP()
		viaSocket()
	}
	median := func(d []time.Duration) time.Duration {
		s := slices.Clone(d)
		sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
		return s[len(s)/2]
	}
	// One retry on a failed comparison absorbs a noisy neighbour on a shared runner; the bound
	// itself is never widened.
	var sm, hm time.Duration
	for attempt := range 2 {
		sockets, https := measureSocketAndHTTP(1000, viaSocket, viaHTTP)
		sm, hm = median(sockets), median(https)
		t.Logf("attempt %d: median socket %s, HTTP %s", attempt+1, sm, hm)
		if float64(sm) <= float64(hm)*1.05 {
			return
		}
	}
	t.Fatalf("socket median %s is above HTTP median %s x 1.05", sm, hm)
}

// measureSocketAndHTTP times n requests on each path, alternating which goes first.
func measureSocketAndHTTP(n int, viaSocket, viaHTTP func()) (sockets, https []time.Duration) {
	sockets = make([]time.Duration, 0, n)
	https = make([]time.Duration, 0, n)
	for i := range n {
		// Alternate which goes first, so neither path always runs on a warmer cache.
		first, second := viaSocket, viaHTTP
		firstOut, secondOut := &sockets, &https
		if i%2 == 1 {
			first, second = viaHTTP, viaSocket
			firstOut, secondOut = &https, &sockets
		}
		start := time.Now()
		first()
		*firstOut = append(*firstOut, time.Since(start))
		start = time.Now()
		second()
		*secondOut = append(*secondOut, time.Since(start))
	}
	return sockets, https
}
