package gateway

import (
	"context"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"time"

	"github.com/coder/websocket"
	"github.com/fxamacker/cbor/v2"
)

// BotLaunch is the supervisor's side of the Bot listener (Bot v2 task 11, internal/botsupervisor): who the current
// Bot child is, and its single-use launch token.
type BotLaunch interface {
	// Peer reports whether a connecting process is the current Bot child: its pid is the one the keeper started and
	// its uid is the Bot's (dillad's own with dev_same_user). It is asked at accept, before any byte is read.
	Peer(pid int32, uid uint32) bool
	// Spend accepts the current child's launch token, once, comparing in constant time.
	Spend(pid int32, token []byte) bool
	// Authenticated reports whether the current child has already spent its token.
	Authenticated(pid int32) bool
}

// ErrBotListenerNotUnix is ServeBot's refusal of any listener but a Unix socket: the Bot path has no network form.
var ErrBotListenerNotUnix = errors.New("gateway: the Bot listener must be a Unix socket")

// botPeerKey carries the admitted peer's pid from the accepted connection into its requests (BotConnContext).
type botPeerKey struct{}

// botPeerConn is a connection BotPeerListener admitted.
type botPeerConn struct {
	net.Conn
	pid int32
}

// botPeerListener admits, from a Unix listener, only connections whose SO_PEERCRED names the current Bot child.
type botPeerListener struct {
	net.Listener
	launch BotLaunch
}

// BotPeerListener wraps a Unix listener so that Accept returns only the current Bot child's connections: any other
// peer (another uid, another process of the Bot's uid, a previous child) is closed before a byte is read. With no
// BotLaunch nobody is admitted.
func BotPeerListener(l net.Listener, launch BotLaunch) (net.Listener, error) {
	if _, ok := l.(*net.UnixListener); !ok {
		return nil, ErrBotListenerNotUnix
	}
	return &botPeerListener{Listener: l, launch: launch}, nil
}

func (l *botPeerListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		uc, ok := c.(*net.UnixConn)
		if !ok {
			_ = c.Close()
			continue
		}
		pid, uid, err := peerCredentials(uc)
		if err != nil || l.launch == nil || !l.launch.Peer(pid, uid) {
			_ = c.Close()
			continue
		}
		return &botPeerConn{Conn: c, pid: pid}, nil
	}
}

// BotConnContext is the http.Server ConnContext of the Bot listener: it records the admitted peer's pid for
// BotHandler. A connection that did not come through BotPeerListener carries none.
func BotConnContext(ctx context.Context, c net.Conn) context.Context {
	if bc, ok := c.(*botPeerConn); ok {
		return context.WithValue(ctx, botPeerKey{}, bc.pid)
	}
	return ctx
}

// BotHandler is the Bot's /gateway upgrade. It serves only a request that arrived through BotPeerListener (the pid
// BotConnContext recorded): mounted anywhere else it answers 404. The first identify of a child carries its launch
// token (base64url, no padding) as session_token, and the connection's session is the upgrade's Authorization bearer,
// which must be a managed Bot session.
func (g *Gateway) BotHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pid, ok := r.Context().Value(botPeerKey{}).(int32)
		if !ok || g.opts.BotLaunch == nil || r.URL.Path != "/gateway" || r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			Subprotocols:    []string{"dilla.v1"},
			CompressionMode: websocket.CompressionDisabled,
		})
		if err != nil {
			return
		}
		c.SetReadLimit(int64(max(maxRequestFrameBytes, int(g.opts.ReadLimit))) + 1)
		bearer := ""
		if h := r.Header.Get("Authorization"); len(h) > len("Bearer ") && h[:len("Bearer ")] == "Bearer " {
			bearer = h[len("Bearer "):]
		}
		ctx := withRemoteAddr(r.Context(), "bot@local")
		g.serveConn(ctx, newLimitedWSSink(c, g.opts.ReadLimit), bearer, &botPeer{pid: pid})
	})
}

// ServeBot serves the Bot's /gateway upgrade on its Unix socket until l closes, with the peer check at accept. It
// refuses any listener but a Unix socket. The composition root serves the same socket with its HTTP routes beside it
// (dillad.Server.ServeBot); this is the gateway alone.
func (g *Gateway) ServeBot(l net.Listener) error {
	pl, err := BotPeerListener(l, g.opts.BotLaunch)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: g.BotHandler(), ConnContext: BotConnContext, ReadHeaderTimeout: 10 * time.Second}
	if err := srv.Serve(pl); err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
		return err
	}
	return nil
}

// botPeer is a connection that came in through the Bot listener.
type botPeer struct{ pid int32 }

// readBotLaunch takes an identify's session_token on the Bot listener apart: empty, or a launch token (base64url, no
// padding, 32 bytes). The payload's session_token is emptied either way, so the session comes from the upgrade's
// bearer. A malformed one is refused.
func readBotLaunch(in *Inbound) bool {
	text, err := rawText(in.Payload[0])
	if err != nil {
		return false
	}
	if text != "" {
		token, err := base64.RawURLEncoding.Strict().DecodeString(text)
		if err != nil || len(token) != 32 {
			return false
		}
		in.botToken = token
	}
	in.Payload[0] = cbor.RawMessage{0x60} // ""
	return true
}

// admitBotSession is the Bot listener's last check of an identify, once its session resolved to a managed Bot's: the
// launch token it carried must be the current child's unspent one, which it spends; without one, the child must have
// spent its token already. A refused identify before this point (a bad session) spends nothing.
func (g *Gateway) admitBotSession(peer *botPeer, in *Inbound) bool {
	if in.botToken == nil {
		return g.opts.BotLaunch.Authenticated(peer.pid)
	}
	ok := g.opts.BotLaunch.Spend(peer.pid, in.botToken)
	clear(in.botToken)
	return ok
}
