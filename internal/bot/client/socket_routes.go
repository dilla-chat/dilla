package client

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/dilla-chat/dilla/internal/id"
)

// Transport sends one app request over the Bot's gateway socket (Bot v2): a request frame and its answer. idem is the
// request's idempotency key, required for every method but GET. A non-nil error means no answer was received.
// gatewayclient.Client implements it.
type Transport interface {
	Do(ctx context.Context, method, route string, body, idem []byte) (status int, resp []byte, err error)
}

// SocketRoutes lists the routes, as Go ServeMux patterns ("METHOD /v1/..."), that the Bot sends over its gateway socket
// instead of HTTPS. scripts/check-socket-routes.mjs holds every entry to a dillad Socket registration. Tasks 4-6 add
// entries as they move routes.
var SocketRoutes = []string{
	"GET /v1/devices/{device_id}/keypackage",
	"GET /v1/welcomes",
	"DELETE /v1/welcomes/{welcome_id}",
}

type socketRoute struct {
	method   string
	segments []string
}

type socketRouteTable []socketRoute

func compileSocketRoutes(patterns []string) socketRouteTable {
	table := make(socketRouteTable, 0, len(patterns))
	for _, pattern := range patterns {
		method, path, ok := strings.Cut(pattern, " ")
		if !ok || !strings.HasPrefix(path, "/") {
			continue
		}
		table = append(table, socketRoute{method: method, segments: strings.Split(path, "/")[1:]})
	}
	return table
}

// match applies ServeMux matching: {name} is one non-empty segment, {name...} the rest, {$} the end of a trailing slash.
func (t socketRouteTable) match(method, path string) bool {
	if len(t) == 0 || !strings.HasPrefix(path, "/") {
		return false
	}
	parts := strings.Split(path, "/")[1:]
	for _, route := range t {
		if route.method == method && matchSegments(route.segments, parts) {
			return true
		}
	}
	return false
}

func matchSegments(segments, parts []string) bool {
	for i, segment := range segments {
		if strings.HasPrefix(segment, "{") && strings.HasSuffix(segment, "...}") {
			return i < len(parts)
		}
		if i >= len(parts) {
			return false
		}
		if segment == "{$}" {
			return i == len(parts)-1 && parts[i] == ""
		}
		if strings.HasPrefix(segment, "{") && strings.HasSuffix(segment, "}") {
			if parts[i] == "" {
				return false
			}
			continue
		}
		if segment != parts[i] {
			return false
		}
	}
	return len(segments) == len(parts)
}

// UseTransport sends the socket routes of device's sessions (SessionForDevice) through t; nil goes back to HTTPS. A
// runtime credential and a session that names no device always use HTTPS: the socket is the device session's.
func (c *Client) UseTransport(device id.ID, t Transport) {
	if c == nil || device.IsZero() {
		return
	}
	key := "device/" + device.String()
	c.transportMu.Lock()
	defer c.transportMu.Unlock()
	if t == nil {
		delete(c.transports, key)
		return
	}
	if c.transports == nil {
		c.transports = make(map[string]Transport)
	}
	c.transports[key] = t
}

// ReleaseTransport withdraws t from device when it is still the device's transport, so a stopped gateway never
// removes its replacement.
func (c *Client) ReleaseTransport(device id.ID, t Transport) {
	if c == nil || device.IsZero() || t == nil {
		return
	}
	key := "device/" + device.String()
	c.transportMu.Lock()
	defer c.transportMu.Unlock()
	if c.transports[key] == t {
		delete(c.transports, key)
	}
}

// ErrNoGateway is a socket route's refusal when the device session has no gateway connection registered (the gateway
// failed to start, or stopped and the next Ensure has not run yet). Nothing was sent. dillad serves the route on the
// socket only, so HTTPS would answer a misleading 404; the caller retries after the gateway is back.
var ErrNoGateway = errors.New("bot client: no gateway connection for a socket-only route; retry when the gateway is running")

// socketTransport is the transport a request goes through, or nil for HTTPS. A socket route of a device session with no
// registered transport is ErrNoGateway, never HTTPS.
func (c *Client) socketTransport(ctx context.Context, method, path string) (Transport, error) {
	if ctx == nil || !c.socketRoutes.match(method, "/v1/"+strings.TrimPrefix(path, "/")) {
		return nil, nil
	}
	key, _ := ctx.Value(deviceRateContextKey{}).(string)
	if key == "" {
		return nil, nil
	}
	c.transportMu.Lock()
	defer c.transportMu.Unlock()
	t := c.transports[key]
	if t == nil {
		return nil, ErrNoGateway
	}
	return t, nil
}

type idempotencyContextKey struct{}

// WithIdempotencyKey names the idempotency key of the mutation made with ctx, for a route whose request carries its
// own id (F3: a sealed command's key is the SHA-256 of its sealed bytes). Without one a mutation gets a fresh random key,
// kept by every retry of that call.
func WithIdempotencyKey(ctx context.Context, key []byte) context.Context {
	return context.WithValue(ctx, idempotencyContextKey{}, append([]byte(nil), key...))
}

func idempotencyKey(ctx context.Context, method string) []byte {
	if method == http.MethodGet || method == http.MethodHead {
		return nil
	}
	if ctx != nil {
		if key, ok := ctx.Value(idempotencyContextKey{}).([]byte); ok && len(key) > 0 {
			return key
		}
	}
	key := make([]byte, 16)
	_, _ = rand.Read(key)
	return key
}

// socketRoundTrip sends the request through t and shapes the answer as an HTTP response for the shared checks. A
// socket answer carries no headers: its body is CBOR, and there is no Date or X-Dilla-Generation to observe.
func socketRoundTrip(ctx context.Context, t Transport, method, route string, body, idem []byte) (*http.Response, error) {
	status, resp, err := t.Do(ctx, method, route, body, idem)
	if err != nil {
		return nil, err
	}
	header := http.Header{}
	if status != http.StatusNoContent {
		header.Set("Content-Type", "application/cbor")
	}
	return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(bytes.NewReader(resp))}, nil
}
