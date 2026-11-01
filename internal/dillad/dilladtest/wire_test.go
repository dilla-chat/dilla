package dilladtest

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/jonasthim/dilla/internal/cborx"
)

// wireLists reads the hello's wire_versions and GET /v1/instance's wire_versions from h.
func wireLists(t *testing.T, h *Host) (hello, instance []uint64) {
	t.Helper()
	ts := httptest.NewServer(h.Handler())
	t.Cleanup(ts.Close)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+"/gateway", //nolint:bodyclose // websocket.Dial documents that the handshake response body never needs closing
		&websocket.DialOptions{Subprotocols: []string{"dilla.v1"}})
	if err != nil {
		t.Fatalf("dial /gateway: %v", err)
	}
	defer func() { _ = c.CloseNow() }()
	_, b, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("read hello: %v", err)
	}
	var frame struct {
		_       struct{} `cbor:",toarray"`
		Op      uint64
		N       uint64
		GroupID []byte
		Payload []any
	}
	if err := cborx.Unmarshal(b, &frame); err != nil || frame.Op != 0 || len(frame.Payload) == 0 {
		t.Fatalf("first frame %x (%v), want hello", b, err)
	}
	hello = toUints(t, frame.Payload[0])

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/v1/instance", nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /v1/instance: %v", err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil || res.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/instance = %d (%v)", res.StatusCode, err)
	}
	var doc []any
	if err := cborx.Unmarshal(body, &doc); err != nil || len(doc) != 11 {
		t.Fatalf("the instance document %x: %v", body, err)
	}
	return hello, toUints(t, doc[0])
}

func toUints(t *testing.T, v any) []uint64 {
	t.Helper()
	list, ok := v.([]any)
	if !ok {
		t.Fatalf("wire_versions is %T, not an array", v)
	}
	out := make([]uint64, 0, len(list))
	for _, e := range list {
		u, ok := e.(uint64)
		if !ok {
			t.Fatalf("a wire version is %T, not a uint", e)
		}
		out = append(out, u)
	}
	return out
}

// S1 fix round 1 (m1): HostOptions.WireVersions reaches dillad.Options, so the hello and GET
// /v1/instance both say the list it names (dilla-testhost's -wire). The control is a host without
// it, which negotiates the current and legacy versions [3, 2].
func TestTheHostsWireVersionsReachTheHelloAndTheInstanceDocument(t *testing.T) {
	core := corePath(t)
	h, err := NewHost(context.Background(), HostOptions{DataDir: t.TempDir(), CorePath: core, WireVersions: []uint64{3}})
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	t.Cleanup(func() { _ = h.Close(context.Background()) })
	if hello, instance := wireLists(t, h); !slices.Equal(hello, []uint64{3}) || !slices.Equal(instance, []uint64{3}) {
		t.Fatalf("WireVersions {3}: hello %v, instance %v; want [3] and [3]", hello, instance)
	}

	// Control: no WireVersions.
	d, err := NewHost(context.Background(), HostOptions{DataDir: t.TempDir(), CorePath: core})
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	t.Cleanup(func() { _ = d.Close(context.Background()) })
	if hello, instance := wireLists(t, d); !slices.Equal(hello, []uint64{5, 4, 3, 2}) || !slices.Equal(instance, []uint64{5, 4, 3, 2}) {
		t.Fatalf("the default host: hello %v, instance %v; want [4 3 2] and [4 3 2]", hello, instance)
	}
}
