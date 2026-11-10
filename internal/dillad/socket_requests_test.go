package dillad_test

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/auth"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/gateway/sockettest"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/server"
)

// The composition root hands the gateway a dispatcher over its own mux (Bot v2 task 2): a request
// frame on /gateway reaches a socket route behind the session middleware with the connection's
// session and the upgrade request's client address, and the HTTP handler does not serve it.
func TestComposedGatewayDispatchesSocketRequests(t *testing.T) {
	cfg := testConfig(t)
	cfg.Log.Level = "warn"
	srv := startColdServer(t, cfg)
	members := seedQuietGroup(t, srv, id.New())
	srv.Mux().Wrapped(api.SessionRoute(srv.Sessions(), nil)).Socket("GET /v1/test/socket-echo",
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			s, _ := auth.FromContext(r.Context())
			_ = server.EncodeBody(w, http.StatusOK, []any{r.RemoteAddr, s.DeviceID})
		}))
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	c := sockettest.Dial(t, ts.URL+"/gateway", members[0].token)
	resp, err := c.Do(http.MethodGet, "/v1/test/socket-echo", nil, nil)
	if err != nil || resp.Status != http.StatusOK {
		t.Fatalf("socket request = %+v, %v", resp, err)
	}
	var got []any
	if err := cborx.Unmarshal(resp.Body, &got); err != nil {
		t.Fatal(err)
	}
	if got[0] != "127.0.0.1:0" || !bytes.Equal(got[1].([]byte), members[0].device[:]) {
		t.Fatalf("handler saw address %v device %x, want 127.0.0.1:0 and %s", got[0], got[1], members[0].device)
	}
	if resp, err := c.Do(http.MethodGet, "/v1/test/no-such-socket-route", nil, nil); err != nil || resp.Status != http.StatusNotFound {
		t.Fatalf("an unknown socket route = %+v, %v", resp, err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/test/socket-echo", nil)
	req.Header.Set("Authorization", "Bearer "+members[0].token)
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		t.Fatal("the HTTP handler served a socket route")
	}
}
