package dillad_test

// community_socket_test.go: Bot v2 task 6. The community, channel, role, invite, moderation, admin, AFK, GIF, presence and
// human-facing Bot admin routes are served on the gateway socket only. The routes that stay on HTTP are pinned by
// TestHTTPOnlyListIsExactlyTheStaysOnHTTPList.

import (
	"net/http"
	"slices"
	"strings"
	"testing"
)

// communitySocketRoutes is every route task 6 moved: its pattern and a concrete path to ask for over HTTP.
var communitySocketRoutes = func() []struct{ pattern, path string } {
	c := "/v1/communities/" + anID
	ch := "/v1/channels/" + anID
	u := "/members/" + anID
	return []struct{ pattern, path string }{
		{"POST /v1/communities", "/v1/communities"},
		{"GET /v1/communities", "/v1/communities"},
		{"GET /v1/communities/{id}", c},
		{"PATCH /v1/communities/{id}", c},
		{"DELETE /v1/communities/{id}", c},
		{"GET /v1/communities/{id}/members", c + "/members"},
		{"GET /v1/communities/{id}/members/{user_id}", c + u},
		{"DELETE /v1/communities/{id}/members/{user_id}", c + u},
		{"GET /v1/communities/{id}/permissions", c + "/permissions"},
		{"POST /v1/communities/{id}/join", c + "/join"},
		{"POST /v1/communities/{id}/leave", c + "/leave"},
		{"POST /v1/communities/{id}/owner", c + "/owner"},
		{"GET /v1/communities/{id}/afk/settings", c + "/afk/settings"},
		{"PUT /v1/communities/{id}/afk/settings", c + "/afk/settings"},
		{"GET /v1/communities/{id}/afk", c + "/afk"},
		{"POST /v1/communities/{id}/afk/activity", c + "/afk/activity"},
		{"PUT /v1/communities/{id}/afk/away", c + "/afk/away"},
		{"POST /v1/communities/{id}/channels", c + "/channels"},
		{"GET /v1/communities/{id}/channels", c + "/channels"},
		{"GET /v1/channels/{id}", ch},
		{"PATCH /v1/channels/{id}", ch},
		{"DELETE /v1/channels/{id}", ch},
		{"GET /v1/channels/{id}/permissions", ch + "/permissions"},
		{"GET /v1/channels/{id}/members", ch + "/members"},
		{"PUT /v1/channels/{id}/members/{user_id}", ch + u},
		{"DELETE /v1/channels/{id}/members/{user_id}", ch + u},
		{"PUT /v1/channels/{id}/overwrites/{kind}/{target_id}", ch + "/overwrites/role/" + anID},
		{"DELETE /v1/channels/{id}/overwrites/{kind}/{target_id}", ch + "/overwrites/role/" + anID},
		{"GET /v1/communities/{id}/roles", c + "/roles"},
		{"POST /v1/communities/{id}/roles", c + "/roles"},
		{"GET /v1/communities/{id}/overwrites", c + "/overwrites"},
		{"PATCH /v1/roles/{id}", "/v1/roles/" + anID},
		{"DELETE /v1/roles/{id}", "/v1/roles/" + anID},
		{"PUT /v1/communities/{id}/members/{user_id}/roles/{role_id}", c + u + "/roles/" + anID},
		{"DELETE /v1/communities/{id}/members/{user_id}/roles/{role_id}", c + u + "/roles/" + anID},
		{"GET /v1/communities/{id}/self-roles", c + "/self-roles"},
		{"PUT /v1/communities/{id}/self-roles/policy", c + "/self-roles/policy"},
		{"PUT /v1/communities/{id}/self-roles/{role_id}", c + "/self-roles/" + anID},
		{"PUT /v1/communities/{id}/role-panels", c + "/role-panels"},
		{"POST /v1/communities/{id}/role-panels/{channel_id}/{message_seq}/reaction", c + "/role-panels/" + anID + "/1/reaction"},
		{"PUT /v1/communities/{id}/bot-join-role", c + "/bot-join-role"},
		{"GET /v1/communities/{id}/bot-join-role", c + "/bot-join-role"},
		{"PUT /v1/communities/{id}/bans/{user_id}", c + "/bans/" + anID},
		{"DELETE /v1/communities/{id}/bans/{user_id}", c + "/bans/" + anID},
		{"GET /v1/communities/{id}/bans", c + "/bans"},
		{"PUT /v1/communities/{id}/members/{user_id}/timeout", c + u + "/timeout"},
		{"DELETE /v1/communities/{id}/members/{user_id}/timeout", c + u + "/timeout"},
		{"GET /v1/communities/{id}/members/{user_id}/timeout", c + u + "/timeout"},
		{"POST /v1/communities/{id}/members/{user_id}/warning", c + u + "/warning"},
		{"POST /v1/communities/{id}/invites", c + "/invites"},
		{"GET /v1/communities/{id}/invites", c + "/invites"},
		{"DELETE /v1/invites/{id}", "/v1/invites/" + anID},
		{"GET /v1/communities/{id}/gifs", c + "/gifs"},
		{"PUT /v1/communities/{id}/gifs/key", c + "/gifs/key"},
		{"DELETE /v1/communities/{id}/gifs/key", c + "/gifs/key"},
		{"PUT /v1/communities/{id}/gifs/rating", c + "/gifs/rating"},
		{"GET /v1/communities/{id}/gifs/search", c + "/gifs/search"},
		{"GET /v1/communities/{community_id}/presence", c + "/presence"},
		{"DELETE /v1/admin/blobs/{blob_id}", "/v1/admin/blobs/" + anID},
		{"GET /v1/admin/audit", "/v1/admin/audit"},
		{"POST /v1/admin/users/{id}/disable", "/v1/admin/users/" + anID + "/disable"},
		{"GET /v1/admin/diagnostics", "/v1/admin/diagnostics"},
		{"GET /v1/communities/{community}/bot", c + "/bot"},
		{"PUT /v1/communities/{community}/bot", c + "/bot"},
		{"DELETE /v1/communities/{community}/bot", c + "/bot"},
		{"PATCH /v1/communities/{community}/bot/profile", c + "/bot/profile"},
		{"POST /v1/communities/{community}/bot/disable", c + "/bot/disable"},
		{"GET /v1/communities/{community}/bot/command-status", c + "/bot/command-status"},
		{"GET /v1/communities/{community}/bot/actors/{actor}/authority", c + "/bot/actors/" + anID + "/authority"},
		{"GET /v1/communities/{community}/bot/actors/{actor}/channels/{channel}/authority", c + "/bot/actors/" + anID + "/channels/" + anID + "/authority"},
		{"POST /v1/communities/{community}/bot/join-role-grants", c + "/bot/join-role-grants"},
		{"GET /v1/communities/{community}/bot/members/{member}/episode", c + "/bot/members/" + anID + "/episode"},
		{"PUT /v1/communities/{community}/bot/music-settings", c + "/bot/music-settings"},
		{"POST /v1/communities/{community}/bot/restart", c + "/bot/restart"},
		{"GET /v1/communities/{community}/bot/events", c + "/bot/events"},
		{"PUT /v1/communities/{community}/bot/events/ack", c + "/bot/events/ack"},
	}
}()

// TestCommunityAreaIsSocketOnly: every moved route is registered on the socket mux and not on the HTTP mux, answers 404
// or 405 over HTTP, and a request frame without a session is refused 401.
func TestCommunityAreaIsSocketOnly(t *testing.T) {
	srv, ts := newServer(t)
	socket := srv.Mux().SocketPatterns()
	httpPatterns := srv.Mux().Patterns()
	disp := socketHandler(srv)
	for _, r := range communitySocketRoutes {
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

// stayOnHTTP is the stays-on-HTTP list of the Bot v2 spec ("What stays on HTTP") plus the three plan rulings
// (POST /v1/sessions/negotiate, the `F` allow-list routes an owed account uses before it has a socket, and the backup
// chunk and GIF media routes) and what the app calls while no gateway carries requests (PRE_GATEWAY_ROUTES).
var stayOnHTTP = []string{
	// Before a session exists: sign-up, sign-in, passkey, TOTP, recovery, enrolment, the session ticket for the socket.
	"POST /v1/accounts",
	"GET /v1/auth/oidc/start",
	"GET /v1/auth/oidc/callback",
	"POST /v1/auth/passkey/login/begin",
	"POST /v1/auth/passkey/login/finish",
	"POST /v1/auth/password/login",
	"POST /v1/auth/recovery/verify",
	"POST /v1/auth/totp/verify",
	"POST /v1/devices/{device_id}/sessions/challenge",
	"POST /v1/devices/{device_id}/sessions",
	"POST /v1/gateway/ticket",
	// Ruling: negotiate runs before the socket opens.
	"POST /v1/sessions/negotiate",
	// Ruling: what an owed account, or a tab with no gateway yet, calls (PRE_GATEWAY_ROUTES).
	"GET /v1/accounts/me",
	"GET /v1/auth/factors",
	"POST /v1/auth/totp/enroll",
	"POST /v1/auth/totp/confirm",
	"POST /v1/auth/passkey/register/begin",
	"POST /v1/auth/passkey/register/finish",
	"GET /v1/users/{user_id}/device-list",
	"PUT /v1/users/{user_id}/device-list",
	"GET /v1/keypackages",
	"POST /v1/keypackages",
	"DELETE /v1/devices/{device_id}/sessions", // forgetBrowser ends the sessions after quiesce
	// Unauthenticated or public.
	"GET /v1/instance",
	"GET /v1/instance/limits",
	// Large binary bodies: blobs, backup chunks and GIF media.
	"GET /v1/channels/{id}/blobs/{blob_id}",
	"PUT /v1/channels/{id}/blobs/{blob_id}",
	"DELETE /v1/channels/{id}/blobs/{blob_id}",
	"POST /v1/channels/{id}/blobs/{blob_id}/confirm",
	"GET /v1/backups",
	"GET /v1/backups/{kind}/{chunk_seq}",
	"PUT /v1/backups/{kind}/{chunk_seq}",
	"DELETE /v1/backups/{kind}/{chunk_seq}", // no client caller; kept with its backup-chunk family (ruling 3)
	"GET /v1/communities/{id}/gifs/media/{gif_id}",
}

// TestHTTPOnlyListIsExactlyTheStaysOnHTTPList: every /v1 pattern on the HTTP mux is in the fixed list above or under
// /v1/bot-runtime/ (until Bot v2 task 14 moves it), and every entry of the list is served.
func TestHTTPOnlyListIsExactlyTheStaysOnHTTPList(t *testing.T) {
	srv, _ := newServer(t)
	want := map[string]bool{}
	for _, p := range stayOnHTTP {
		want[p] = true
	}
	seen := map[string]bool{}
	for _, pattern := range srv.Mux().Patterns() {
		_, path, _ := strings.Cut(pattern, " ")
		if !strings.HasPrefix(path, "/v1/") || strings.HasPrefix(path, "/v1/bot-runtime/") {
			continue
		}
		seen[pattern] = true
		if !want[pattern] {
			t.Errorf("%s is served on HTTP but is not in the stays-on-HTTP list", pattern)
		}
	}
	for _, p := range stayOnHTTP {
		if !seen[p] {
			t.Errorf("%s is in the stays-on-HTTP list but the HTTP mux does not serve it", p)
		}
	}
}
