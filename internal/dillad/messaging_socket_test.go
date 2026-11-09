package dillad_test

// messaging_socket_test.go: Bot v2 task 5. The group, MLS, message, DM, reaction, readable, report, search and call
// routes are served on the gateway socket only; the blob routes carry binary bodies and stay on HTTP.

import (
	"net/http"
	"slices"
	"strings"
	"testing"
)

const anID = "0102030405060708090a0b0c0d0e0f10"

// messagingSocketRoutes is every route task 5 moved: its pattern and a concrete path to ask for over HTTP.
var messagingSocketRoutes = []struct{ pattern, path string }{
	{"POST /v1/groups", "/v1/groups"},
	{"GET /v1/groups/{id}/info", "/v1/groups/" + anID + "/info"},
	{"GET /v1/groups/{id}/tree", "/v1/groups/" + anID + "/tree"},
	{"GET /v1/groups/{id}/handshakes", "/v1/groups/" + anID + "/handshakes"},
	{"POST /v1/groups/{id}/commit", "/v1/groups/" + anID + "/commit"},
	{"POST /v1/groups/{id}/proposal", "/v1/groups/" + anID + "/proposal"},
	{"GET /v1/groups/{id}/proposals", "/v1/groups/" + anID + "/proposals"},
	{"POST /v1/groups/{id}/resync", "/v1/groups/" + anID + "/resync"},
	{"POST /v1/groups/{id}/fork-report", "/v1/groups/" + anID + "/fork-report"},
	{"GET /v1/groups/{id}/heal", "/v1/groups/" + anID + "/heal"},
	{"GET /v1/groups/{id}/heal-state", "/v1/groups/" + anID + "/heal-state"},
	{"POST /v1/groups/{id}/heal", "/v1/groups/" + anID + "/heal"},
	{"POST /v1/groups/{id}/message", "/v1/groups/" + anID + "/message"},
	{"GET /v1/groups/{id}/messages", "/v1/groups/" + anID + "/messages"},
	{"DELETE /v1/groups/{id}/messages/{seq}", "/v1/groups/" + anID + "/messages/1"},
	{"POST /v1/groups/{id}/cursor", "/v1/groups/" + anID + "/cursor"},
	{"POST /v1/groups/{group}/bot/messages", "/v1/groups/" + anID + "/bot/messages"},
	{"GET /v1/communities/{community}/bot/receipts/{operation}", "/v1/communities/" + anID + "/bot/receipts/" + anID},
	{"POST /v1/communities/{community}/bot/receipts/{operation}/resolve", "/v1/communities/" + anID + "/bot/receipts/" + anID + "/resolve"},
	{"POST /v1/groups/{group}/moderation/removals", "/v1/groups/" + anID + "/moderation/removals"},
	{"GET /v1/groups/{group}/moderation/removals", "/v1/groups/" + anID + "/moderation/removals"},
	{"POST /v1/dms", "/v1/dms"},
	{"GET /v1/dms", "/v1/dms"},
	{"POST /v1/channels/{id}/messages", "/v1/channels/" + anID + "/messages"},
	{"GET /v1/channels/{id}/messages", "/v1/channels/" + anID + "/messages"},
	{"PATCH /v1/channels/{id}/messages/{seq}", "/v1/channels/" + anID + "/messages/1"},
	{"DELETE /v1/channels/{id}/messages/{seq}", "/v1/channels/" + anID + "/messages/1"},
	{"GET /v1/channels/{id}/search", "/v1/channels/" + anID + "/search"},
	{"PUT /v1/channels/{id}/read-state", "/v1/channels/" + anID + "/read-state"},
	{"POST /v1/reports", "/v1/reports"},
	{"GET /v1/reports", "/v1/reports"},
	{"PATCH /v1/reports/{id}", "/v1/reports/" + anID},
	{"POST /v1/channels/{id}/calls", "/v1/channels/" + anID + "/calls"},
	{"GET /v1/channels/{id}/call", "/v1/channels/" + anID + "/call"},
	{"GET /v1/communities/{id}/calls", "/v1/communities/" + anID + "/calls"},
	{"DELETE /v1/calls/{call_id}", "/v1/calls/" + anID},
	{"POST /v1/calls/{call_id}/share", "/v1/calls/" + anID + "/share"},
	{"DELETE /v1/calls/{call_id}/share", "/v1/calls/" + anID + "/share"},
	{"POST /v1/calls/{call_id}/rtt", "/v1/calls/" + anID + "/rtt"},
	{"POST /v1/calls/{call_id}/decline", "/v1/calls/" + anID + "/decline"},
	{"POST /v1/calls/{call_id}/stats", "/v1/calls/" + anID + "/stats"},
}

// TestMessagingAreaIsSocketOnly: every moved route is registered on the socket mux and not on the HTTP mux, answers 404
// or 405 over HTTP, and a request frame without a session is refused 401.
func TestMessagingAreaIsSocketOnly(t *testing.T) {
	srv, ts := newServer(t)
	socket := srv.Mux().SocketPatterns()
	httpPatterns := srv.Mux().Patterns()
	disp := socketHandler(srv)
	for _, r := range messagingSocketRoutes {
		t.Run(r.pattern, func(t *testing.T) {
			if !slices.Contains(socket, r.pattern) || slices.Contains(httpPatterns, r.pattern) {
				t.Fatalf("%s: on the socket mux %v, on the HTTP mux %v; want socket only",
					r.pattern, slices.Contains(socket, r.pattern), slices.Contains(httpPatterns, r.pattern))
			}
			method, _, _ := strings.Cut(r.pattern, " ")
			req, err := http.NewRequestWithContext(t.Context(), method, ts.URL+r.path, strings.NewReader(""))
			if err != nil {
				t.Fatal(err)
			}
			res, err := ts.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			res.Body.Close()
			if res.StatusCode != http.StatusNotFound && res.StatusCode != http.StatusMethodNotAllowed {
				t.Errorf("over HTTP = %d, want 404 or 405", res.StatusCode)
			}
			if rec := call(t, disp, method, r.path, "", nil); rec.Code != http.StatusUnauthorized {
				t.Errorf("over the socket without a session = %d, want 401", rec.Code)
			}
		})
	}
}

// TestBlobRoutesStayOnHTTP: blob bodies are binary and up to the blob cap, above a socket frame.
func TestBlobRoutesStayOnHTTP(t *testing.T) {
	srv, _ := newServer(t)
	socket := srv.Mux().SocketPatterns()
	for _, c := range []struct{ method, pattern, path string }{
		{http.MethodPut, "PUT /v1/channels/{id}/blobs/{blob_id}", "/v1/channels/" + anID + "/blobs/" + anID},
		{http.MethodGet, "GET /v1/channels/{id}/blobs/{blob_id}", "/v1/channels/" + anID + "/blobs/" + anID},
		{http.MethodDelete, "DELETE /v1/channels/{id}/blobs/{blob_id}", "/v1/channels/" + anID + "/blobs/" + anID},
		{http.MethodPost, "POST /v1/channels/{id}/blobs/{blob_id}/confirm", "/v1/channels/" + anID + "/blobs/" + anID + "/confirm"},
	} {
		if slices.Contains(socket, c.pattern) {
			t.Errorf("%s is a socket route; blob routes stay on HTTP", c.pattern)
		}
		req, _ := http.NewRequestWithContext(t.Context(), c.method, c.path, nil)
		if _, pattern := srv.Mux().Mux().Handler(req); pattern != c.pattern {
			t.Errorf("%s %s is served by %q on the HTTP mux, want %q", c.method, c.path, pattern, c.pattern)
		}
	}
}
