package gateway

// pending_test.go pins the security review's F4: identify refuses a pending session (protocol/02
// § Device sessions item 4, L-HTTP-57: a pending session reaches its own two backup reads, its
// own device-list GET and PUT and the session routes needing no session, and nothing else). A
// holder of the password alone mints pending sessions; without this it opened gateway
// connections, received `ready` and held connection slots. It answers like a token that does not
// resolve: an E_UNAUTHENTICATED error frame and close 4003.

import (
	"context"
	"net/http"
	"testing"

	"github.com/coder/websocket"

	"github.com/dilla-chat/dilla/internal/auth"
	"github.com/dilla-chat/dilla/internal/id"
)

func pendingSession(h *harness, device id.ID) string {
	return scopedSession(h, device, auth.Session{Scope: auth.ScopePending})
}

// scopedSession makes the device's token resolve to s with the device and a fresh user filled in.
func scopedSession(h *harness, device id.ID, s auth.Session) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	s.DeviceID, s.UserID = device, id.New()
	h.tokens[h.tokenFor(device)] = s
	return h.tokenFor(device)
}

// nonEnrolled are the sessions the gateway refuses (branch review REGISTRATION-DEVICES-01): a
// pending one, and a provisional one with or without a pairing group. Before the fix a provisional
// session minted by purpose 2 on a row a password holder registered reached `ready` and was fanned
// every `message.plain` of the user's readable channels, because fan-out is keyed by user. An
// enrolled session whose account owes a second factor is refused the same way (web3.6 S1,
// protocol/02 § Device sessions item 8).
func nonEnrolled() map[string]auth.Session {
	group := id.New()
	return map[string]auth.Session{
		"pending":                  {Scope: auth.ScopePending},
		"provisional":              {Scope: auth.ScopeProvisional},
		"provisional with a group": {Scope: auth.ScopeProvisional, PairingGroup: &group},
		"enrolled, factor owed":    {Scope: auth.ScopeEnrolled, FactorOwed: true},
	}
}

// wantRefusedOnSocket reads the error frame and close 4003 off a real socket and checks the device
// never came online.
func wantRefusedOnSocket(t *testing.T, h *harness, ctx context.Context, c *websocket.Conn, device id.ID) {
	t.Helper()
	in := readWS(t, ctx, c)
	if in.Op != OpError {
		t.Fatalf("identify: op %d, want an error frame", in.Op)
	}
	if code, err := rawText(in.Payload[1]); err != nil || code != "E_UNAUTHENTICATED" {
		t.Fatalf("error code = %q (err %v), want E_UNAUTHENTICATED", code, err)
	}
	if _, _, err := c.Read(ctx); websocket.CloseStatus(err) != websocket.StatusCode(CloseUnauthenticated) {
		t.Fatalf("close status = %v, want 4003", websocket.CloseStatus(err))
	}
	if h.gw.Online(device) {
		t.Fatal("a session that is not enrolled is online")
	}
}

// The token in the identify frame, for every scope that is not enrolled.
func TestIdentifyRefusesAProvisionalSession(t *testing.T) {
	for name, sess := range nonEnrolled() {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			device := id.New()
			token := scopedSession(h, device, sess)
			s := newRecordingSink(8, false)
			go h.gw.serve(context.Background(), s, "")
			h.readFrom(t, s) // hello

			p, err := payload(token, uint64(1), uint64(1), uint64(1), uint64(0))
			if err != nil {
				t.Fatalf("payload: %v", err)
			}
			s.feed(mustEncode(t, Frame{Op: OpIdentify, Payload: p, Replay: true}, 1))

			in := h.readFrom(t, s)
			if in.Op != OpError {
				t.Fatalf("identify: op %d, want an error frame", in.Op)
			}
			if code, err := rawText(in.Payload[1]); err != nil || code != "E_UNAUTHENTICATED" {
				t.Fatalf("error code = %q (err %v), want E_UNAUTHENTICATED", code, err)
			}
			if got := h.waitCloseOn(t, s); got != CloseUnauthenticated {
				t.Fatalf("close code = %d, want 4003", got)
			}
			if h.gw.Online(device) {
				t.Fatal("a session that is not enrolled is online")
			}
		})
	}
}

// The token as a bearer on the HTTP upgrade (the native path), over a real socket.
func TestABearerForASessionThatIsNotEnrolledIsRefused(t *testing.T) {
	for name, sess := range nonEnrolled() {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			device := id.New()
			token := scopedSession(h, device, sess)
			c, ctx := dialGateway(t, h.gw, &websocket.DialOptions{
				HTTPHeader:   http.Header{"Authorization": []string{"Bearer " + token}},
				Subprotocols: []string{"dilla.v1"},
			})
			if in := readWS(t, ctx, c); in.Op != OpHello {
				t.Fatalf("first frame op %d, want hello", in.Op)
			}
			p, err := payload("", uint64(1), uint64(1), uint64(1), uint64(0))
			if err != nil {
				t.Fatalf("payload: %v", err)
			}
			if err := c.Write(ctx, websocket.MessageBinary,
				mustEncode(t, Frame{Op: OpIdentify, Payload: p, Replay: true}, 1)); err != nil {
				t.Fatalf("write: %v", err)
			}
			wantRefusedOnSocket(t, h, ctx, c, device)
		})
	}
}

// A ticket that redeems to a provisional session's token, the browser path.
func TestATicketForAProvisionalSessionIsRefused(t *testing.T) {
	h := newHarness(t)
	device := id.New()
	token := scopedSession(h, device, auth.Session{Scope: auth.ScopeProvisional})
	ticket, _, err := h.gw.Tickets().Mint(device, token)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	c, ctx := dialGateway(t, h.gw, &websocket.DialOptions{
		Subprotocols: []string{"dilla.v1", "dilla.ticket." + ticket},
	})
	if in := readWS(t, ctx, c); in.Op != OpHello {
		t.Fatalf("first frame op %d, want hello", in.Op)
	}
	p, err := payload("", uint64(1), uint64(1), uint64(1), uint64(0))
	if err != nil {
		t.Fatalf("payload: %v", err)
	}
	if err := c.Write(ctx, websocket.MessageBinary,
		mustEncode(t, Frame{Op: OpIdentify, Payload: p, Replay: true}, 1)); err != nil {
		t.Fatalf("write: %v", err)
	}
	wantRefusedOnSocket(t, h, ctx, c, device)
}

// Resume authenticates with element 0, a session token, through the same identify: a suspended
// connection whose device's token now resolves to a session that is not enrolled is not resumed.
func TestResumeRefusesASessionThatIsNotEnrolled(t *testing.T) {
	for name, sess := range nonEnrolled() {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			c := h.connect(t, id.New())
			resume := c.resumeToken()
			h.gw.suspend(c)
			token := scopedSession(h, c.deviceID, sess)

			s := newRecordingSink(8, false)
			go h.gw.serve(context.Background(), s, "")
			h.readFrom(t, s) // hello
			p, err := payload(token, resume, h.generation, uint64(0))
			if err != nil {
				t.Fatalf("payload: %v", err)
			}
			s.feed(mustEncode(t, Frame{Op: OpResume, Payload: p, Replay: true}, 2))
			if in := h.readFrom(t, s); in.Op != OpInvalidSession {
				t.Fatalf("resume: op %d, want invalid_session", in.Op)
			}
			if h.gw.Online(c.deviceID) {
				t.Fatal("a session that is not enrolled resumed a connection")
			}
		})
	}
}

// The pending token in the identify frame.
func TestIdentifyRefusesAPendingSession(t *testing.T) {
	h := newHarness(t)
	device := id.New()
	token := pendingSession(h, device)
	s := newRecordingSink(8, false)
	go h.gw.serve(context.Background(), s, "")
	h.readFrom(t, s) // hello

	p, err := payload(token, uint64(1), uint64(1), uint64(1), uint64(0))
	if err != nil {
		t.Fatalf("payload: %v", err)
	}
	s.feed(mustEncode(t, Frame{Op: OpIdentify, Payload: p, Replay: true}, 1))

	in := h.readFrom(t, s)
	if in.Op != OpError {
		t.Fatalf("a pending session's identify: op %d, want an error frame", in.Op)
	}
	if code, err := rawText(in.Payload[1]); err != nil || code != "E_UNAUTHENTICATED" {
		t.Fatalf("error code = %q (err %v), want E_UNAUTHENTICATED", code, err)
	}
	if got := h.waitCloseOn(t, s); got != CloseUnauthenticated {
		t.Fatalf("close code = %d, want 4003", got)
	}
	if h.gw.Online(device) {
		t.Fatal("a pending device is online")
	}
}

// The browser path: a ticket that redeems to a pending session's token, over a real socket.
func TestATicketForAPendingSessionIsRefused(t *testing.T) {
	h := newHarness(t)
	device := id.New()
	token := pendingSession(h, device)
	ticket, _, err := h.gw.Tickets().Mint(device, token)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	c, ctx := dialGateway(t, h.gw, &websocket.DialOptions{
		Subprotocols: []string{"dilla.v1", "dilla.ticket." + ticket},
	})
	if in := readWS(t, ctx, c); in.Op != OpHello {
		t.Fatalf("first frame op %d, want hello", in.Op)
	}
	p, err := payload("", uint64(1), uint64(1), uint64(1), uint64(0))
	if err != nil {
		t.Fatalf("payload: %v", err)
	}
	if err := c.Write(ctx, websocket.MessageBinary,
		mustEncode(t, Frame{Op: OpIdentify, Payload: p, Replay: true}, 1)); err != nil {
		t.Fatalf("write: %v", err)
	}
	if in := readWS(t, ctx, c); in.Op != OpError {
		t.Fatalf("a pending ticket's identify: op %d, want an error frame", in.Op)
	}
	if _, _, err := c.Read(ctx); websocket.CloseStatus(err) != websocket.StatusCode(CloseUnauthenticated) {
		t.Fatalf("close status = %v, want 4003", websocket.CloseStatus(err))
	}
	if h.gw.Online(device) {
		t.Fatal("a pending device is online")
	}
}
