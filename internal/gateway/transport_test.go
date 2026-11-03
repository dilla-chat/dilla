package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/id"
)

// bearerOrTicket is a credential path, and the ticket branch (gap-38) SPENDS a single-use ticket
// off Sec-WebSocket-Protocol. Every rule it implements is pinned here: the Authorization header
// wins, an unspent ticket behind a winning header stays unspent, the first ticket entry Take
// accepts is the one spent, and an unknown ticket resolves to no credential at all.
func TestBearerOrTicketTakesTheCredentialFromTheHandshake(t *testing.T) {
	const sessionToken = "session-token"

	newTickets := func(t *testing.T) (*Tickets, string) {
		t.Helper()
		tickets := NewTickets(clock.NewFake(time.Unix(1_700_000_000, 0)))
		s, _, err := tickets.Mint(id.New(), sessionToken)
		if err != nil {
			t.Fatalf("mint: %v", err)
		}
		return tickets, s
	}

	t.Run("the authorization header is the credential", func(t *testing.T) {
		tickets, _ := newTickets(t)
		r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/gateway", nil)
		r.Header.Set("Authorization", "Bearer "+sessionToken)
		if got := bearerOrTicket(r, tickets); got != sessionToken {
			t.Fatalf("credential = %q, want %q", got, sessionToken)
		}
		if tickets.len() != 1 {
			t.Fatal("the header path must not spend a ticket")
		}
	})

	t.Run("a ticket is spent once", func(t *testing.T) {
		tickets, s := newTickets(t)
		r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/gateway", nil)
		r.Header.Set("Sec-WebSocket-Protocol", "dilla.v1, dilla.ticket."+s)
		if got := bearerOrTicket(r, tickets); got != sessionToken {
			t.Fatalf("credential = %q, want %q", got, sessionToken)
		}
		if tickets.len() != 0 {
			t.Fatal("the ticket must be consumed")
		}
		// A second upgrade with the same ticket gets nothing: single use is the whole point.
		if got := bearerOrTicket(r, tickets); got != "" {
			t.Fatalf("a spent ticket resolved to %q", got)
		}
	})

	t.Run("the header wins over a ticket and leaves it unspent", func(t *testing.T) {
		tickets, s := newTickets(t)
		r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/gateway", nil)
		r.Header.Set("Authorization", "Bearer "+sessionToken)
		r.Header.Set("Sec-WebSocket-Protocol", "dilla.v1, dilla.ticket."+s)
		if got := bearerOrTicket(r, tickets); got != sessionToken {
			t.Fatalf("credential = %q, want %q", got, sessionToken)
		}
		if tickets.len() != 1 {
			t.Fatal("a ticket behind a winning Authorization header must survive the upgrade")
		}
	})

	t.Run("the first ticket Take accepts is the one spent", func(t *testing.T) {
		tickets, s := newTickets(t)
		r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/gateway", nil)
		r.Header.Set("Sec-WebSocket-Protocol", "dilla.v1, dilla.ticket.nonsense, dilla.ticket."+s)
		if got := bearerOrTicket(r, tickets); got != sessionToken {
			t.Fatalf("credential = %q, want %q", got, sessionToken)
		}
	})

	t.Run("an unknown ticket is no credential", func(t *testing.T) {
		tickets, _ := newTickets(t)
		r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/gateway", nil)
		r.Header.Set("Sec-WebSocket-Protocol", "dilla.v1, dilla.ticket.nonsense")
		if got := bearerOrTicket(r, tickets); got != "" {
			t.Fatalf("credential = %q, want the empty string", got)
		}
		if tickets.len() != 1 {
			t.Fatal("an unknown ticket must not consume a real one")
		}
	})

	t.Run("a bare upgrade is no credential", func(t *testing.T) {
		tickets, _ := newTickets(t)
		r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/gateway", nil)
		if got := bearerOrTicket(r, tickets); got != "" {
			t.Fatalf("credential = %q, want the empty string", got)
		}
	})

	t.Run("a non-bearer authorization header falls through to the ticket", func(t *testing.T) {
		tickets, s := newTickets(t)
		r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/gateway", nil)
		r.Header.Set("Authorization", "Basic ZGVhZDpiZWVm")
		r.Header.Set("Sec-WebSocket-Protocol", "dilla.ticket."+s)
		if got := bearerOrTicket(r, tickets); got != sessionToken {
			t.Fatalf("credential = %q, want %q", got, sessionToken)
		}
	})
}

// dialGateway serves g over a real HTTP server and dials it with a real WebSocket client. It is
// what puts Handler, wsSink and the upgrade credential paths under test at all: every other test
// in this package drives the recording sink instead.
func dialGateway(t *testing.T, g *Gateway, opts *websocket.DialOptions) (*websocket.Conn, context.Context) {
	t.Helper()
	srv := httptest.NewServer(g.Handler())
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), opts) //nolint:bodyclose // websocket.Dial documents that the handshake response body never needs closing
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.CloseNow() })
	return c, ctx
}

func readWS(t *testing.T, ctx context.Context, c *websocket.Conn) Inbound {
	t.Helper()
	typ, b, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if typ != websocket.MessageBinary {
		t.Fatalf("message type %v, want binary: dilla's wire is CBOR, never text", typ)
	}
	in, err := decodeAny(b)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return in
}

// hello -> identify -> ready over a real socket, with the credential on the Authorization header.
func TestTheHandshakeWalksHelloIdentifyReadyOverARealSocket(t *testing.T) {
	h := newHarness(t)
	device := id.New()
	h.mu.Lock()
	h.tokens[h.tokenFor(device)] = session(device)
	h.mu.Unlock()

	c, ctx := dialGateway(t, h.gw, &websocket.DialOptions{
		HTTPHeader:   http.Header{"Authorization": []string{"Bearer " + h.tokenFor(device)}},
		Subprotocols: []string{"dilla.v1"},
	})
	if got := c.Subprotocol(); got != "dilla.v1" {
		t.Fatalf("negotiated subprotocol %q, want dilla.v1", got)
	}

	if in := readWS(t, ctx, c); in.Op != OpHello {
		t.Fatalf("first frame op %d, want hello", in.Op)
	}

	// The token travelled on the handshake, so the identify payload carries the empty string —
	// the fallback tokenFromHandshake exists for.
	p, err := payload("", uint64(1), uint64(1), uint64(1), uint64(0))
	if err != nil {
		t.Fatalf("payload: %v", err)
	}
	if err := c.Write(ctx, websocket.MessageBinary,
		mustEncode(t, Frame{Op: OpIdentify, Payload: p, Replay: true}, 1)); err != nil {
		t.Fatalf("write: %v", err)
	}

	in := readWS(t, ctx, c)
	if in.Op != OpReady {
		t.Fatalf("op %d, want ready", in.Op)
	}
	if in.CID != 1 {
		t.Fatalf("ready carries n = %d, want 1", in.CID)
	}
	if !h.gw.Online(device) {
		t.Fatal("a device that reached ready over a real socket must be online")
	}
}

// The browser path of gap-38: the credential arrives as a subprotocol entry, is spent by the
// upgrade, and is never echoed back — Accept offers only dilla.v1, so the ticket cannot leak into
// the response's Sec-WebSocket-Protocol header.
func TestATicketUpgradeSpendsTheTicketAndIsNeverEchoed(t *testing.T) {
	h := newHarness(t)
	device := id.New()
	h.mu.Lock()
	h.tokens[h.tokenFor(device)] = session(device)
	h.mu.Unlock()

	ticket, _, err := h.gw.Tickets().Mint(device, h.tokenFor(device))
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	c, ctx := dialGateway(t, h.gw, &websocket.DialOptions{
		Subprotocols: []string{"dilla.v1", "dilla.ticket." + ticket},
	})
	if got := c.Subprotocol(); got != "dilla.v1" {
		t.Fatalf("negotiated subprotocol %q: the ticket must never be echoed", got)
	}

	if in := readWS(t, ctx, c); in.Op != OpHello {
		t.Fatalf("first frame op %d, want hello", in.Op)
	}
	// Dial returns once the 101 is read, and Handler spends the ticket after Accept has written it, so
	// right after Dial the ticket may still be outstanding. Handler spends it before serve sends hello:
	// once hello is read, the spend has happened.
	if h.gw.Tickets().len() != 0 {
		t.Fatal("the upgrade must spend the ticket")
	}
	p, err := payload("", uint64(1), uint64(1), uint64(1), uint64(0))
	if err != nil {
		t.Fatalf("payload: %v", err)
	}
	if err := c.Write(ctx, websocket.MessageBinary,
		mustEncode(t, Frame{Op: OpIdentify, Payload: p, Replay: true}, 1)); err != nil {
		t.Fatalf("write: %v", err)
	}
	if in := readWS(t, ctx, c); in.Op != OpReady {
		t.Fatalf("op %d, want ready", in.Op)
	}
	if !h.gw.Online(device) {
		t.Fatal("the ticket holder must be online after ready")
	}
}

// A first frame that is neither identify nor resume is close 4001, over the real transport this
// time: wsSink.close is the only thing that turns a CloseCode into a WebSocket status.
func TestAFirstFrameThatIsNotIdentifyClosesFourZeroZeroOneOverARealSocket(t *testing.T) {
	h := newHarness(t)
	c, ctx := dialGateway(t, h.gw, &websocket.DialOptions{Subprotocols: []string{"dilla.v1"}})
	if in := readWS(t, ctx, c); in.Op != OpHello {
		t.Fatalf("first frame op %d, want hello", in.Op)
	}

	p, err := payload(uint64(0), uint64(1))
	if err != nil {
		t.Fatalf("payload: %v", err)
	}
	if err := c.Write(ctx, websocket.MessageBinary,
		mustEncode(t, Frame{Op: OpHeartbeat, Payload: p, Replay: true}, 1)); err != nil {
		t.Fatalf("write: %v", err)
	}

	if _, _, err := c.Read(ctx); websocket.CloseStatus(err) != websocket.StatusCode(CloseNotIdentified) {
		t.Fatalf("close status = %v, want 4001", websocket.CloseStatus(err))
	}
}
