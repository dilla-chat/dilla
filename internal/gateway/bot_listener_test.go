package gateway

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/dilla-chat/dilla/internal/auth"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

// fakeLaunch is the supervisor's side of the Bot listener: one current child (this test process), its uid, and its
// single-use launch token.
type fakeLaunch struct {
	mu     sync.Mutex
	uid    uint32
	pid    int32
	token  []byte
	spent  bool
	peers  int
	spends int
}

func (f *fakeLaunch) Peer(pid int32, uid uint32) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.peers++
	return pid == f.pid && uid == f.uid
}

func (f *fakeLaunch) Spend(pid int32, token []byte) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.spends++
	match := subtle.ConstantTimeCompare(token, f.token) == 1
	if !match || pid != f.pid || f.spent {
		return false
	}
	f.spent = true
	return true
}

func (f *fakeLaunch) Authenticated(pid int32) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return pid == f.pid && f.spent
}

// newStart is the next child start: a fresh token, nothing spent.
func (f *fakeLaunch) newStart(t *testing.T) []byte {
	t.Helper()
	tok := make([]byte, 32)
	if _, err := rand.Read(tok); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.token, f.spent = tok, false
	return tok
}

func newBotListenerHarness(t *testing.T, launch BotLaunch) *harness {
	t.Helper()
	h := newHarness(t)
	h.gw = New(Options{
		Clock: h.clk, Generation: 1, Store: h, Auth: &capabilityRecordingAuth{Authenticator: h}, ScopeResolver: h, CommunityCompatibility: h,
		WireVersions: []uint64{auth.WireVersion}, BotLaunch: launch,
	})
	return h
}

// botSessionToken registers a managed Bot session for a new device.
func botSessionToken(h *harness, device id.ID) string {
	bot := auth.BotSessionBinding{InstallationID: id.New(), CommunityID: id.New(), State: store.BotInstallationReady}
	return scopedSession(h, device, auth.Session{Scope: auth.ScopeEnrolled, WireVersion: auth.WireVersion, Bot: &bot, TokenHash: tokenHash()})
}

// serveBotSocket serves h.gw's Bot listener on a Unix socket in a temp dir and returns its path.
func serveBotSocket(t *testing.T, h *harness) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gateway.sock")
	l, err := (&net.ListenConfig{}).Listen(t.Context(), "unix", path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- h.gw.ServeBot(l) }()
	t.Cleanup(func() {
		_ = l.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("ServeBot did not return after its listener closed")
		}
	})
	return path
}

func dialBotSocket(ctx context.Context, path, bearer string) (*websocket.Conn, error) {
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", path)
	}}}
	c, _, err := websocket.Dial(ctx, "ws://localhost/gateway", &websocket.DialOptions{ //nolint:bodyclose // websocket.Dial documents that the handshake response body never needs closing
		HTTPClient: client, Subprotocols: []string{"dilla.v1"},
		HTTPHeader: http.Header{"Authorization": []string{"Bearer " + bearer}},
	})
	return c, err
}

// identifyOverBot walks hello and an identify whose session_token is launch (base64url, or empty), and returns the
// answer frame, or the close code when the connection closed instead.
func identifyOverBot(t *testing.T, path, bearer, launch string) (Inbound, websocket.StatusCode) {
	t.Helper()
	return identifyOverBotWire(t, path, bearer, launch, auth.WireVersion)
}

// identifyOverBotWire is identifyOverBot choosing the wire version.
func identifyOverBotWire(t *testing.T, path, bearer, launch string, wire uint64) (Inbound, websocket.StatusCode) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := dialBotSocket(ctx, path, bearer)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.CloseNow() })
	if in := readWS(t, ctx, c); in.Op != OpHello {
		t.Fatalf("first frame op %d, want hello", in.Op)
	}
	p, err := payload(launch, wire, uint64(1), uint64(1), uint64(0))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Write(ctx, websocket.MessageBinary, mustEncode(t, Frame{Op: OpIdentify, Payload: p, Replay: true}, 1)); err != nil {
		t.Fatalf("write identify: %v", err)
	}
	in := readWS(t, ctx, c)
	if in.Op == OpError {
		_, _, err := c.Read(ctx)
		return in, websocket.CloseStatus(err)
	}
	return in, -1
}

func tokenHash() []byte {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return b
}

func encodeLaunch(tok []byte) string { return base64.RawURLEncoding.EncodeToString(tok) }

func TestBotListenerRefusesWrongPeerUid(t *testing.T) {
	launch := &fakeLaunch{uid: uint32(os.Getuid()) + 1, pid: int32(os.Getpid())}
	h := newBotListenerHarness(t, launch)
	tok := launch.newStart(t)
	path := serveBotSocket(t, h)
	device := id.New()
	bearer := botSessionToken(h, device)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if c, err := dialBotSocket(ctx, path, bearer); err == nil {
		_ = c.CloseNow()
		t.Fatal("a peer whose uid is not the Bot's completed the upgrade")
	}
	launch.mu.Lock()
	peers, spends := launch.peers, launch.spends
	launch.mu.Unlock()
	if peers == 0 || spends != 0 {
		t.Fatalf("Peer was asked %d times and Spend %d: the peer check runs at accept, before any token is read", peers, spends)
	}
	// The same socket admits the right uid.
	launch.mu.Lock()
	launch.uid = uint32(os.Getuid())
	launch.mu.Unlock()
	if in, code := identifyOverBot(t, path, bearer, encodeLaunch(tok)); in.Op != OpReady {
		t.Fatalf("the Bot's own uid got op %d (close %d), want ready", in.Op, code)
	}
	// Another pid of the same uid is not the child.
	launch.mu.Lock()
	launch.pid++
	launch.mu.Unlock()
	if c, err := dialBotSocket(ctx, path, bearer); err == nil {
		_ = c.CloseNow()
		t.Fatal("a process that is not the current child completed the upgrade")
	}
}

// TestLaunchTokenIsSpentOnlyByAnIdentifyThatReachesReady (Bot v2 task 14 fix round 1, I-4): the token is spent last,
// after every other check. An identify refused after its session resolved (here: a managed Bot on an older wire the
// instance still advertises) leaves the token unspent, so the next identify with it reaches ready.
func TestLaunchTokenIsSpentOnlyByAnIdentifyThatReachesReady(t *testing.T) {
	launch := &fakeLaunch{uid: uint32(os.Getuid()), pid: int32(os.Getpid())}
	h := newBotListenerHarness(t, launch)
	h.gw.opts.WireVersions = []uint64{auth.WireVersion - 1, auth.WireVersion}
	tok := launch.newStart(t)
	path := serveBotSocket(t, h)
	bearer := botSessionToken(h, id.New())
	if in, _ := identifyOverBotWire(t, path, bearer, encodeLaunch(tok), auth.WireVersion-1); in.Op != OpError {
		t.Fatalf("a managed Bot on wire %d got op %d, want an error", auth.WireVersion-1, in.Op)
	}
	if in, code := identifyOverBot(t, path, bearer, encodeLaunch(tok)); in.Op != OpReady {
		t.Fatalf("the token after a refused identify got op %d close %d, want ready (it was spent by the refusal)", in.Op, code)
	}
}

func TestBotListenerRefusesReusedLaunchToken(t *testing.T) {
	launch := &fakeLaunch{uid: uint32(os.Getuid()), pid: int32(os.Getpid())}
	h := newBotListenerHarness(t, launch)
	previous := launch.newStart(t)
	tok := launch.newStart(t)
	path := serveBotSocket(t, h)
	device := id.New()
	bearer := botSessionToken(h, device)

	// Before the child has spent its token, no identify without it gets in.
	if in, code := identifyOverBot(t, path, bearer, ""); in.Op != OpError || code != websocket.StatusCode(CloseUnauthenticated) {
		t.Fatalf("an identify without the launch token got op %d close %d, want error and 4003", in.Op, code)
	}
	// A previous start's token is refused, and does not spend the current one.
	if in, code := identifyOverBot(t, path, bearer, encodeLaunch(previous)); in.Op != OpError || code != websocket.StatusCode(CloseUnauthenticated) {
		t.Fatalf("a previous start's token got op %d close %d, want error and 4003", in.Op, code)
	}
	if in, code := identifyOverBot(t, path, bearer, encodeLaunch(tok)); in.Op != OpReady {
		t.Fatalf("the current token got op %d close %d, want ready", in.Op, code)
	}
	// The connection is bound to the Bot's managed session: DeliverBot finds it.
	if !h.gw.DeliverBot(device, Frame{Op: OpBotCommand, Payload: mustPayload(t, id.New(), id.New(), id.New(), id.New(), []byte{1})}) {
		t.Fatal("DeliverBot found no Bot runtime connection for the device bound over the Bot listener")
	}
	// The token is single use.
	if in, code := identifyOverBot(t, path, bearer, encodeLaunch(tok)); in.Op != OpError || code != websocket.StatusCode(CloseUnauthenticated) {
		t.Fatalf("a reused launch token got op %d close %d, want error and 4003", in.Op, code)
	}
	// The child that spent it may open its other connections without it.
	other := id.New()
	if in, code := identifyOverBot(t, path, botSessionToken(h, other), ""); in.Op != OpReady {
		t.Fatalf("the authenticated child's second connection got op %d close %d, want ready", in.Op, code)
	}
	// A refused identify spends nothing: a bad session presenting the next start's token leaves it usable.
	next := launch.newStart(t)
	h.mu.Lock()
	h.tokens["bad"] = auth.Session{UserID: id.New(), DeviceID: id.New(), Scope: auth.ScopeEnrolled, WireVersion: auth.WireVersion, TokenHash: tokenHash()}
	h.mu.Unlock()
	if in, code := identifyOverBot(t, path, "bad", encodeLaunch(next)); in.Op != OpError || code != websocket.StatusCode(CloseUnauthenticated) {
		t.Fatalf("a person's session with the launch token got op %d close %d", in.Op, code)
	}
	if in, code := identifyOverBot(t, path, bearer, encodeLaunch(next)); in.Op != OpReady {
		t.Fatalf("the token a refused identify carried is no longer good: op %d close %d", in.Op, code)
	}
	// The listener binds managed Bot sessions only: a person's session is refused even from the child.
	person := id.New()
	h.mu.Lock()
	h.tokens[h.tokenFor(person)] = auth.Session{UserID: id.New(), DeviceID: person, Scope: auth.ScopeEnrolled, WireVersion: auth.WireVersion, TokenHash: tokenHash()}
	h.mu.Unlock()
	if in, code := identifyOverBot(t, path, h.tokenFor(person), ""); in.Op != OpError || code != websocket.StatusCode(CloseUnauthenticated) {
		t.Fatalf("a person's session over the Bot listener got op %d close %d, want error and 4003", in.Op, code)
	}
	// A malformed token is refused like a wrong one.
	if in, code := identifyOverBot(t, path, bearer, "not-a-token"); in.Op != OpError || code != websocket.StatusCode(CloseUnauthenticated) {
		t.Fatalf("a malformed launch token got op %d close %d", in.Op, code)
	}
}

func mustPayload(t *testing.T, v ...any) []byte {
	t.Helper()
	p, err := payload(v...)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// The Bot path exists on the Unix listener only: ServeBot will not serve a TCP listener, the public upgrade does not
// read a launch token (it is not a session), and the Bot handler outside ServeBot admits nobody.
func TestBotSocketHasNoTCPPath(t *testing.T) {
	launch := &fakeLaunch{uid: uint32(os.Getuid()), pid: int32(os.Getpid())}
	h := newBotListenerHarness(t, launch)
	tok := launch.newStart(t)

	tcp, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tcp.Close() }()
	if err := h.gw.ServeBot(tcp); err == nil || !errors.Is(err, ErrBotListenerNotUnix) {
		t.Fatalf("ServeBot(tcp) = %v, want ErrBotListenerNotUnix", err)
	}

	// The public upgrade over TCP: a launch token in identify is not a session, and is never spent there.
	device := id.New()
	_ = botSessionToken(h, device)
	c, ctx := dialGateway(t, h.gw, &websocket.DialOptions{Subprotocols: []string{"dilla.v1"}})
	if in := readWS(t, ctx, c); in.Op != OpHello {
		t.Fatalf("hello: op %d", in.Op)
	}
	p := mustPayload(t, encodeLaunch(tok), auth.WireVersion, uint64(1), uint64(1), uint64(0))
	if err := c.Write(ctx, websocket.MessageBinary, mustEncode(t, Frame{Op: OpIdentify, Payload: p, Replay: true}, 1)); err != nil {
		t.Fatal(err)
	}
	// The harness's Resolve answers any non-empty token with a person's session; what matters is that the connection
	// is not a Bot runtime connection and the launch token was never looked at.
	_, _, _ = c.Read(ctx)
	launch.mu.Lock()
	spends := launch.spends
	launch.mu.Unlock()
	if spends != 0 {
		t.Fatalf("the public upgrade spent a launch token %d times", spends)
	}

	// The Bot handler mounted on a TCP server (a wiring mistake) refuses every upgrade: there is no peer credential.
	srv := httptest.NewServer(h.gw.BotHandler())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/gateway") //nolint:noctx // a test request
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("the Bot handler on TCP answered %d, want 404", resp.StatusCode)
	}
	if strings.Contains(resp.Header.Get("Upgrade"), "websocket") {
		t.Fatal("the Bot handler upgraded a TCP request")
	}
}
