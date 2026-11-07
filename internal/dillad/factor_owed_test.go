package dillad_test

// factor_owed_test.go is web3.6 S1's route sweep through the composition root: an account
// registered through POST /v1/accounts owes a second factor, and its enrolled session reaches only
// the `F` and `P` routes of protocol/09 and protocol/02. The route list is parsed from the two
// documents' endpoint tables, so a route added to them later is swept without a test edit. The
// control clears the flag and runs the same sweep: no route answers E_FACTOR_REQUIRED.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/dillad"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

// satisfyFactor clears the account's owed second factor through the repository, for a test about
// something else that registers through POST /v1/accounts. It stands in for the factor
// enrolment; nothing in the shipping binary or the test host offers it.
func satisfyFactor(t *testing.T, srv *dillad.Server, user id.ID) {
	t.Helper()
	if err := srv.Repo().SetFactorOwed(context.Background(), user, false); err != nil {
		t.Fatalf("SetFactorOwed(%s, false): %v", user, err)
	}
}

// docRoute is one endpoint row of a protocol document's table.
type docRoute struct {
	doc, method, path, auth string
}

// sweepClass is what an owed enrolled session should meet on a route.
type sweepClass int

const (
	sweepSkip    sweepClass = iota // no session required (`—`), or not a /v1 route
	sweepRefused                   // `E`, `E or V`, `A`: 403 E_FACTOR_REQUIRED
	sweepAllowed                   // `E, F` or `E or P`: anything but E_FACTOR_REQUIRED
)

var (
	routeCell     = regexp.MustCompile("^`([A-Z]+) ([^`]+)`")
	sectionScope  = regexp.MustCompile("[Ee]very (?:route|one)\\b[^.]*?\\bis `([A-Z])`")
	pathParameter = regexp.MustCompile(`\{[a-z_]+\}`)
)

// docRoutes reads every row of every "Method and path" table in the protocol document. A table
// without an Auth column takes the scope its section states in prose ("every route below is
// `E`"); a section that states none fails the test, so a new table cannot slip past the sweep.
func docRoutes(t *testing.T, name string) []docRoute {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "protocol", name))
	if err != nil {
		t.Fatalf("read protocol/%s: %v", name, err)
	}
	var (
		out     []docRoute
		prose   strings.Builder // the current section's text outside tables
		header  []string        // the current table's header cells; nil outside a route table
		inTable bool
	)
	for _, line := range strings.Split(string(raw), "\n") {
		switch {
		case strings.HasPrefix(line, "## "), strings.HasPrefix(line, "### "):
			prose.Reset()
			header, inTable = nil, false
			continue
		case !strings.HasPrefix(line, "|"):
			inTable = false
			prose.WriteString(" " + strings.TrimSpace(line))
			continue
		}
		cells := splitRow(line)
		if !inTable {
			inTable = true
			header = nil
			if len(cells) > 0 && cells[0] == "Method and path" {
				header = cells
			}
			continue
		}
		if header == nil || strings.HasPrefix(line, "|---") {
			continue
		}
		m := routeCell.FindStringSubmatch(cells[0])
		if m == nil {
			t.Fatalf("protocol/%s: a route table row whose first cell is not `METHOD /path`: %q", name, line)
		}
		var auth string
		if i := slices.Index(header, "Auth"); i >= 0 && i < len(cells) {
			auth = cells[i]
		} else {
			s := sectionScope.FindStringSubmatch(strings.Join(strings.Fields(prose.String()), " "))
			if s == nil {
				t.Fatalf("protocol/%s: the table holding %s %s has no Auth column and its section states no scope",
					name, m[1], m[2])
			}
			auth = s[1]
		}
		out = append(out, docRoute{doc: name, method: m[1], path: m[2], auth: auth})
	}
	return out
}

// splitRow splits a Markdown table row into trimmed cells, leaving `\|` inside a cell alone.
func splitRow(line string) []string {
	line = strings.TrimSpace(line)
	line = strings.TrimSuffix(strings.TrimPrefix(line, "|"), "|")
	var cells []string
	var cur strings.Builder
	for i := 0; i < len(line); i++ {
		if line[i] == '\\' && i+1 < len(line) && line[i+1] == '|' {
			cur.WriteString(`\|`)
			i++
			continue
		}
		if line[i] == '|' {
			cells = append(cells, strings.TrimSpace(cur.String()))
			cur.Reset()
			continue
		}
		cur.WriteByte(line[i])
	}
	return append(cells, strings.TrimSpace(cur.String()))
}

// classify reads an Auth cell: the parenthetical notes ("(step-up)", "(own list only for P)") say
// nothing about the scope.
func classify(t *testing.T, r docRoute) sweepClass {
	t.Helper()
	if !strings.HasPrefix(r.path, "/v1/") {
		return sweepSkip
	}
	auth := r.auth
	if i := strings.Index(auth, "("); i >= 0 {
		auth = auth[:i]
	}
	auth = strings.TrimSpace(auth)
	switch {
	case strings.HasPrefix(auth, "—"):
		return sweepSkip
	case auth == "R":
		return sweepSkip // runtime bearer auth is independent of a human session
	case auth == "B":
		return sweepSkip // a human bearer cannot exercise a route requiring a Bot-bound session
	case auth == "E, F", auth == "E or P":
		return sweepAllowed
	case auth == "E", auth == "E or V", auth == "E or B", auth == "A":
		return sweepRefused
	}
	t.Fatalf("protocol/%s: %s %s has an Auth the sweep does not know: %q", r.doc, r.method, r.path, r.auth)
	return sweepSkip
}

// concrete fills a documented path with well-formed identifiers and drops its query.
func concrete(t *testing.T, r docRoute) string {
	t.Helper()
	path, _, _ := strings.Cut(r.path, "?")
	const hex32 = "0102030405060708090a0b0c0d0e0f10"
	return pathParameter.ReplaceAllStringFunc(path, func(p string) string {
		switch p {
		case "{gif_id}":
			return "abc123"
		case "{id}", "{user_id}", "{device_id}", "{role_id}", "{target_id}", "{call_id}", "{channel_id}", "{community}", "{installation}", "{operation}", "{group}", "{actor}", "{member}", "{attempt}", "{request}":
			return hex32
		case "{blob_id}", "{avatar_id}": // a webhook avatar id is a SHA-256 in hex, like a blob id
			return hex32 + hex32
		case "{kind}", "{chunk_seq}":
			return "0"
		case "{seq}", "{message_seq}", "{welcome_id}":
			return "1"
		}
		t.Fatalf("protocol/%s: %s %s names a path parameter the sweep cannot fill: %s", r.doc, r.method, r.path, p)
		return p
	})
}

// sweepRoutes is every documented /v1 route an owed session is swept over, with its class.
func sweepRoutes(t *testing.T) map[docRoute]sweepClass {
	t.Helper()
	out := map[docRoute]sweepClass{}
	for _, doc := range []string{"09-http-api.md", "02-delivery-service.md"} {
		for _, r := range docRoutes(t, doc) {
			if c := classify(t, r); c != sweepSkip {
				out[r] = c
			}
		}
	}
	return out
}

func TestTheSweepReadsTheDocumentedRoutes(t *testing.T) {
	refused := map[string]bool{}
	allowed := map[string]bool{}
	for r, c := range sweepRoutes(t) {
		key := r.method + " " + concrete(t, r)
		if c == sweepRefused {
			refused[key] = true
		} else {
			allowed[key] = true
		}
	}
	const x = "0102030405060708090a0b0c0d0e0f10"
	for _, want := range []string{
		"POST /v1/gateway/ticket", "POST /v1/communities", "POST /v1/keypackages", "GET /v1/welcomes",
		"PUT /v1/backups/0/0", "POST /v1/groups", "GET /v1/devices", "DELETE /v1/accounts/me",
		"POST /v1/auth/password", "POST /v1/reports", "POST /v1/dms",
		"POST /v1/auth/stepup", "POST /v1/auth/stepup/passkey/begin", "POST /v1/auth/stepup/passkey/finish",
		"POST /v1/auth/factors/remove", "POST /v1/auth/recovery/codes",
	} {
		if !refused[want] {
			t.Errorf("the sweep does not refuse %s", want)
		}
	}
	for _, want := range []string{
		"GET /v1/accounts/me", "DELETE /v1/devices/" + x + "/sessions", "PUT /v1/users/" + x + "/device-list",
		"GET /v1/users/" + x + "/device-list", "GET /v1/backups", "GET /v1/backups/0/0",
		"POST /v1/auth/totp/enroll", "POST /v1/auth/totp/confirm",
		"POST /v1/auth/passkey/register/begin", "POST /v1/auth/passkey/register/finish",
		"GET /v1/auth/factors",
	} {
		if !allowed[want] {
			t.Errorf("the sweep does not admit %s", want)
		}
	}
}

// S1 review minor 1: the sweep reads the documents, so a route mounted without a document row
// would escape it. Every /v1 pattern the composition root registers must be a row of protocol/09
// or protocol/02 (path parameters compared by position, not by name).
func TestEveryMountedRouteIsADocumentedRoute(t *testing.T) {
	srv, _, _ := newInstance(t)
	defer srv.Shutdown(context.Background())
	documented := map[string]bool{}
	for _, doc := range []string{"09-http-api.md", "02-delivery-service.md"} {
		for _, r := range docRoutes(t, doc) {
			path, _, _ := strings.Cut(r.path, "?")
			documented[r.method+" "+pathParameter.ReplaceAllString(path, "{}")] = true
		}
	}
	// protocol/10-community-bot.md defines this new installation-scoped route.
	documented["POST /v1/bot-runtime/installations/{}/interaction-admissions"] = true
	anyParameter := regexp.MustCompile(`\{[A-Za-z_.]+\}`)
	mounted := 0
	for _, pattern := range srv.Mux().Patterns() {
		method, path, ok := strings.Cut(pattern, " ")
		if !ok || !strings.HasPrefix(path, "/v1/") {
			continue
		}
		mounted++
		if key := method + " " + anyParameter.ReplaceAllString(path, "{}"); !documented[key] {
			t.Errorf("%s is mounted but no protocol/09 or protocol/02 table lists it, so the owed sweep never sends it", pattern)
		}
	}
	// Bot v2 tasks 4-6 and 14 moved most routes to the socket mux: about 40 /v1 HTTP routes remain.
	if mounted < 30 {
		t.Fatalf("only %d /v1 patterns were recorded; the mux is not reporting its routes", mounted)
	}
}

func TestHumanSessionIsNotABotSession(t *testing.T) {
	if got := classify(t, docRoute{
		doc: "09-http-api.md", method: http.MethodPut,
		path: "/v1/bot-runtime/installations/{installation}/credential", auth: "B",
	}); got != sweepSkip {
		t.Fatalf("a Bot-only route classified for a human bearer = %d, want skipped", got)
	}

	srv, h, code := newInstance(t)
	defer srv.Shutdown(context.Background())
	dev := newTestDevice(t)
	user, token := accountWithKeys(t, h, code, "human-bot-route", "correct horse battery staple", dev,
		ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x7e}, 32)))
	satisfyFactor(t, srv, user)

	rec := call(t, h, http.MethodPut,
		"/v1/bot-runtime/installations/0102030405060708090a0b0c0d0e0f10/credential", token, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("human session on Bot-only credential route = %d %q, want 401", rec.Code, errorCode(rec))
	}
}

// sweep sends every documented route with the bearer and an empty body, in a fixed order with
// DELETE /v1/accounts/me last (an empty body is a valid one for it, and it ends the account), and
// returns the routes that answered E_FACTOR_REQUIRED. alive asks that every route before that last
// one find the session still valid (no 401), so a sweep whose session died part-way cannot pass
// vacuously.
func sweep(t *testing.T, h http.Handler, token string, alive bool) map[docRoute]bool {
	t.Helper()
	routes := slices.Collect(maps.Keys(sweepRoutes(t)))
	last := func(r docRoute) bool { return r.method == http.MethodDelete && r.path == "/v1/accounts/me" }
	slices.SortFunc(routes, func(a, b docRoute) int {
		if last(a) != last(b) {
			if last(a) {
				return 1
			}
			return -1
		}
		return strings.Compare(a.method+" "+a.path+" "+a.doc, b.method+" "+b.path+" "+b.doc)
	})
	if !last(routes[len(routes)-1]) {
		t.Fatal("DELETE /v1/accounts/me is not among the swept routes")
	}
	factor := map[docRoute]bool{}
	for i, r := range routes {
		rec := call(t, h, r.method, concrete(t, r), token, nil)
		if errorCode(rec) == "E_FACTOR_REQUIRED" {
			if rec.Code != http.StatusForbidden {
				t.Errorf("%s %s: E_FACTOR_REQUIRED with %d, want 403", r.method, r.path, rec.Code)
			}
			factor[r] = true
		}
		if alive && i < len(routes)-1 && rec.Code == http.StatusUnauthorized {
			t.Fatalf("%s %s answered 401 %q: the session died before the sweep finished", r.method, r.path, errorCode(rec))
		}
	}
	return factor
}

func TestAnAccountThatOwesASecondFactorReachesOnlyTheAllowList(t *testing.T) {
	srv, h, code := newInstance(t)
	defer srv.Shutdown(context.Background())
	ctx := context.Background()
	ssk := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x7c}, 32))
	dev := newTestDevice(t)
	user, token := accountWithKeys(t, h, code, "owing", "correct horse battery staple", dev, ssk)
	if u, err := srv.Repo().GetUser(ctx, user); err != nil || !u.FactorOwed {
		t.Fatalf("a fresh sign-up = %+v, %v; want it to owe a second factor", u, err)
	}

	factor := sweep(t, h, token, false)
	for r, c := range sweepRoutes(t) {
		switch {
		case c == sweepRefused && !factor[r]:
			t.Errorf("%s (%s) %s %s: an owed session was not refused E_FACTOR_REQUIRED", r.doc, r.auth, r.method, r.path)
		case c == sweepAllowed && factor[r]:
			t.Errorf("%s (%s) %s %s: an owed session was refused E_FACTOR_REQUIRED", r.doc, r.auth, r.method, r.path)
		}
	}

	// The allow-list does what an owed account needs: its profile, its own list and its backups.
	if rec := call(t, h, http.MethodGet, "/v1/accounts/me", token, nil); rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/accounts/me as an owed session = %d %q, want 200", rec.Code, errorCode(rec))
	}
	own := "/v1/users/" + user.String() + "/device-list"
	body, _ := signedDeviceList(t, ssk, user, 1, nil, []gateEntry{entryFor(dev, 0, nil)})
	if rec := call(t, h, http.MethodPut, own, token, body); rec.Code/100 != 2 {
		t.Fatalf("PUT its own device list as an owed session = %d %q, want 2xx", rec.Code, errorCode(rec))
	}
	for _, path := range []string{own, own + "?after=0", "/v1/backups"} {
		if rec := call(t, h, http.MethodGet, path, token, nil); rec.Code/100 != 2 {
			t.Fatalf("GET %s as an owed session = %d %q, want 2xx", path, rec.Code, errorCode(rec))
		}
	}

	// The control: the same session once the account holds a factor.
	if err := srv.Repo().SetFactorOwed(ctx, user, false); err != nil {
		t.Fatalf("SetFactorOwed: %v", err)
	}
	for r := range sweep(t, h, token, true) {
		t.Errorf("the control: %s %s answered E_FACTOR_REQUIRED for an account that owes nothing", r.method, r.path)
	}
}

// The gateway: an owed account's bearer is closed 4003 on identify; the same bearer once the
// account owes nothing reaches ready.
func TestTheGatewayRefusesAnAccountThatOwesASecondFactor(t *testing.T) {
	logs := &recordLog{}
	srv, h, ts, code := newLoggedInstance(t, logs)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	user, token := accountWithKeys(t, h, code, "socket", "correct horse battery staple", newTestDevice(t),
		ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x7d}, 32)))

	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+"/gateway", //nolint:bodyclose // websocket.Dial documents that the handshake response body never needs closing
		&websocket.DialOptions{
			HTTPHeader:   http.Header{"Authorization": []string{"Bearer " + token}},
			Subprotocols: []string{"dilla.v1"},
		})
	if err != nil {
		t.Fatalf("dial /gateway: %v", err)
	}
	defer c.CloseNow()
	if op, _ := readFrame(t, ctx, c); op != 0 {
		t.Fatalf("first frame op %d, want hello", op)
	}
	identify(t, ctx, c)
	for {
		if _, _, err := c.Read(ctx); err != nil {
			if got := websocket.CloseStatus(err); got != 4003 {
				t.Fatalf("an owed session's identify ended with %v (close %d), want close 4003", err, got)
			}
			break
		}
	}

	if err := srv.Repo().SetFactorOwed(context.Background(), user, false); err != nil {
		t.Fatalf("SetFactorOwed: %v", err)
	}
	dialReady(t, ctx, ts, token)
}

// The boot reconcile: a start marks every live human account that holds no usable factor, a
// passkey under another rp_id being no usable factor, and clears the mark from every one that
// holds a usable factor (web3.6 review I2), and says how many of each at info.
func TestTheBootReconcileMarksAnAccountWithoutAUsableFactor(t *testing.T) {
	cfg := testConfig(t)
	cfg.Log.Level = "warn"
	ctx := context.Background()
	rp := cfg.Auth.WebAuthn.RPID
	if rp == "" || rp == "old.example" {
		t.Fatalf("the test config's rp_id is %q; the test needs one that is not old.example", rp)
	}

	first := startColdServer(t, cfg)
	repo := first.Repo()
	now := first.Now().Unix()
	user := func(name string, owed bool) id.ID {
		u := store.UserRow{ID: id.New(), Username: name, Display: name, UMKPub: make([]byte, 32),
			SSKPub: make([]byte, 32), SigUMKSSK: make([]byte, 64), Created: now, FactorOwed: owed}
		if err := repo.CreateUser(ctx, u); err != nil {
			t.Fatalf("CreateUser %s: %v", name, err)
		}
		return u.ID
	}
	passkey := func(u id.ID, rpID string, b byte) {
		if err := repo.PutWebauthnCredential(ctx, store.WebauthnCredentialRow{CredID: bytes.Repeat([]byte{b}, 32),
			RPID: rpID, UserID: u, PublicKey: []byte{1}, Flags: []byte{1}, ExtensionsJSON: "{}", Created: now}); err != nil {
			t.Fatalf("PutWebauthnCredential: %v", err)
		}
	}
	moved := user("moved", false)
	passkey(moved, "old.example", 0x81)
	current := user("current", false)
	passkey(current, rp, 0x82)
	totp := user("totp", false)
	confirmed := now
	if err := repo.PutTOTP(ctx, store.TOTPRow{UserID: totp, Secret: bytes.Repeat([]byte{9}, 20), Digits: 6,
		Period: 30, Algorithm: "SHA1", ConfirmedAt: &confirmed, Created: now}); err != nil {
		t.Fatalf("PutTOTP: %v", err)
	}
	settled := user("settled", true)
	passkey(settled, rp, 0x83)
	bare := user("bare", true)
	if err := first.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	logs := &recordLog{}
	second, err := dillad.New(ctx, dillad.Options{Config: cfg, Clock: clock.System(), Wasm: sharedRuntime(t), Log: slog.New(logs)})
	if err != nil {
		t.Fatalf("dillad.New (the restart): %v", err)
	}
	t.Cleanup(func() { _ = second.Shutdown(context.Background()) })
	for _, c := range []struct {
		name string
		user id.ID
		want bool
	}{
		{"a passkey only under old.example", moved, true},
		{"a passkey under the instance's rp_id", current, false},
		{"a confirmed TOTP", totp, false},
		{"marked as owing, holding a passkey under the instance's rp_id", settled, false},
		{"marked as owing, holding nothing", bare, true},
	} {
		u, err := second.Repo().GetUser(ctx, c.user)
		if err != nil {
			t.Fatalf("GetUser: %v", err)
		}
		if u.FactorOwed != c.want {
			t.Errorf("%s: factor_owed = %v after the restart, want %v", c.name, u.FactorOwed, c.want)
		}
	}
	if !logs.has(slog.LevelInfo, "now owe one", map[string]string{"accounts": "1"}) {
		t.Fatalf("the restart did not log the one account it marked at info: %v", logs.messages())
	}
	if !logs.has(slog.LevelInfo, "no longer owe one", map[string]string{"accounts": "1"}) {
		t.Fatalf("the restart did not log the one account it cleared at info: %v", logs.messages())
	}
}
