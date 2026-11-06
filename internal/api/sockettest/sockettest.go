// Package sockettest drives the socket routes of a server.Mux through the request dispatcher, with no
// WebSocket in between. A test that used to send an HTTP request to a route that moved to the gateway
// socket sends it here instead: the same session middleware, rate meter and handler run, and the answer
// is the status and CBOR body a request frame's response carries (no headers). The gateway end of the
// path (frames, the in-flight cap) has its own tests in internal/gateway and internal/api.
package sockettest

import (
	"context"
	"crypto/rand"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/auth"
	"github.com/dilla-chat/dilla/internal/gateway"
)

// RemoteAddr is the client address Do gives the request, the one a gateway connection from a local
// test client would resolve.
const RemoteAddr = "127.0.0.1:40000"

var nextID atomic.Uint64

// Do sends one request frame for sess's connection and returns the response frame's status and body.
// method and route are the REST form's ("PATCH", "/v1/accounts/me", any query string included); body
// is the CBOR body, nil for none. A mutation carries a fresh idem, as the client's every mutation does.
func Do(t testing.TB, d *api.SocketDispatcher, sess auth.Session, method, route string, body []byte) (int, []byte) {
	t.Helper()
	return DoIdem(t, d, sess, method, route, body, nil)
}

// DoIdem is Do with the request's idem named, for a test of the replay record. A nil idem on a method
// other than GET and HEAD is replaced by a fresh random one.
func DoIdem(t testing.TB, d *api.SocketDispatcher, sess auth.Session, method, route string, body, idem []byte) (int, []byte) {
	t.Helper()
	resp := Dispatch(context.Background(), d, sess, method, route, body, idem)
	return resp.Status, resp.Body
}

// Dispatch is DoIdem without a testing.TB, for a helper that runs off the test goroutine.
func Dispatch(ctx context.Context, d *api.SocketDispatcher, sess auth.Session, method, route string, body, idem []byte) gateway.SocketResponse {
	return DispatchFrom(ctx, d, sess, RemoteAddr, method, route, body, idem)
}

// DispatchFrom is Dispatch for a connection whose client address is remoteAddr ("ip:port"), for a test of a
// route that keys a budget by address.
func DispatchFrom(ctx context.Context, d *api.SocketDispatcher, sess auth.Session, remoteAddr, method, route string, body, idem []byte) gateway.SocketResponse {
	if idem == nil && method != http.MethodGet && method != http.MethodHead {
		idem = make([]byte, 16)
		_, _ = rand.Read(idem)
	}
	if body == nil {
		body = []byte{}
	}
	return d.Dispatch(ctx, gateway.SocketCaller{
		TokenHash: sess.TokenHash, DeviceID: sess.DeviceID, RemoteAddr: remoteAddr, Wire: auth.WireVersion,
	}, gateway.SocketRequest{ID: nextID.Add(1), Method: method, Route: route, Body: body, Idem: idem})
}
