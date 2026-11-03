package api_test

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/config"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/server"
)

const testGiphyKey = "kSECRETgiphyKey0123"

// fakeGiphy is Giphy, faked: the search API and the media host in one httptest server. No test in this file
// reaches the real API.
type fakeGiphy struct {
	srv *httptest.Server

	mu       sync.Mutex
	searches []map[string]string // the query of every search and trending call
	paths    []string            // every path the media side served
	hits     atomic.Int32

	// media decides what /media/... answers; nil serves a small valid GIF.
	media func(w http.ResponseWriter, r *http.Request)
}

var tinyGIF = append([]byte("GIF89a"), bytes.Repeat([]byte{0}, 64)...)

// bigGIF is larger than a loopback socket can buffer, so a client that does not read pins the handler's write.
var bigGIF = append([]byte("GIF89a"), bytes.Repeat([]byte{0}, 48<<20)...)

func newFakeGiphy(t *testing.T) *fakeGiphy {
	t.Helper()
	f := &fakeGiphy{}
	mux := http.NewServeMux()
	search := func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		q := map[string]string{"path": r.URL.Path}
		for k, v := range r.URL.Query() {
			q[k] = v[0]
		}
		f.mu.Lock()
		f.searches = append(f.searches, q)
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[
			{"type":"gif","id":"abc123XYZ","title":"cat typing","images":{"fixed_width":{"url":"https://media2.giphy.com/x","width":"200","height":"113"},"original":{"url":"https://media2.giphy.com/y","width":"480","height":"270"}}},
			{"type":"gif","id":"../../etc/passwd","title":"evil id","images":{"fixed_width":{"width":"1","height":"1"}}},
			{"type":"gif","id":"def456","title":"dog","images":{"fixed_width":{"width":"200","height":"200"}}}
		],"pagination":{"total_count":100,"count":3,"offset":0},"meta":{"status":200}}`)
	}
	mux.HandleFunc("/v1/gifs/search", search)
	mux.HandleFunc("/v1/gifs/trending", search)
	mux.HandleFunc("/media/", func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		f.mu.Lock()
		f.paths = append(f.paths, r.URL.Path)
		f.mu.Unlock()
		if f.media != nil {
			f.media(w, r)
			return
		}
		w.Header().Set("Content-Type", "image/gif")
		_, _ = w.Write(tinyGIF)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// lastSearch is the query of the newest search call.
func (f *fakeGiphy) lastSearch() map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.searches) == 0 {
		return nil
	}
	return f.searches[len(f.searches)-1]
}

type gifEnv struct {
	*env
	g        *api.Gifs
	fake     *fakeGiphy
	cid      id.ID
	memberID id.ID
	base     string // /v1/communities/{cid}
	owner    string
	member   string
	member2  string
	outside  string // not in the community
	logs     *bytes.Buffer
}

// newGifEnv is a community with an owner and two ordinary members; withKey has the owner save the key through
// the API, the way a person does.
func newGifEnv(t *testing.T, withKey bool) *gifEnv {
	t.Helper()
	e := newEnv(t)
	fake := newFakeGiphy(t)
	logs := &bytes.Buffer{}
	log := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	api.NewCommunities(e.Repo, e.DS, e.Clk, slog.New(slog.DiscardHandler)).Register(e.Mux)
	api.NewRoles(e.Repo, e.Clk, "dilla.example", slog.New(slog.DiscardHandler)).Register(e.Mux)
	limiter := server.NewRateLimiter(config.Default().Limits.Rate, e.Clk)
	g := api.NewGifs(e.Repo, limiter, e.Clk, log)
	api.UseFakeGiphyForTest(g, fake.srv)
	g.Register(e.Mux)
	_, owner := e.NewUser("owner")
	cid := createCommunity(t, e, owner)
	memberID, member := e.NewUser("member")
	_, member2 := e.NewUser("member2")
	_, outside := e.NewUser("outside")
	joinCommunity(t, e, cid, member)
	joinCommunity(t, e, cid, member2)
	ge := &gifEnv{env: e, g: g, fake: fake, cid: cid, memberID: memberID, base: "/v1/communities/" + cid.String(),
		owner: owner, member: member, member2: member2, outside: outside, logs: logs}
	if withKey {
		ge.setKey(t, testGiphyKey)
	}
	return ge
}

func (ge *gifEnv) setKey(t *testing.T, key string) {
	t.Helper()
	if status, body := ge.Do(http.MethodPut, ge.base+"/gifs/key", ge.owner, []any{key}); status != http.StatusNoContent {
		t.Fatalf("PUT key = %d %x", status, body)
	}
}

func (ge *gifEnv) getAs(tok, path string) (int, []byte, http.Header) {
	ge.t.Helper()
	resp := ge.Request(ge.t, http.MethodGet, path, tok, nil, nil)
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		ge.t.Fatal(err)
	}
	return resp.StatusCode, b, resp.Header
}

// get is a GET as an ordinary member, on the community's gif routes.
func (ge *gifEnv) get(sub string) (int, []byte, http.Header) {
	ge.t.Helper()
	return ge.getAs(ge.member, ge.base+"/gifs"+sub)
}

// stuckGet sends a request on a raw connection and never reads the reply.
func (ge *gifEnv) stuckGet(tok, sub string) net.Conn {
	ge.t.Helper()
	u, _ := url.Parse(ge.Srv.URL)
	conn, err := (&net.Dialer{}).DialContext(ge.t.Context(), "tcp", u.Host)
	if err != nil {
		ge.t.Fatal(err)
	}
	ge.t.Cleanup(func() { _ = conn.Close() })
	fmt.Fprintf(conn, "GET %s/gifs%s HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer %s\r\n\r\n", ge.base, sub, u.Host, tok)
	return conn
}

func (ge *gifEnv) waitHeld(want int, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if api.GiphyHeldForTest(ge.g) == want {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return api.GiphyHeldForTest(ge.g) == want
}

func decodeStatus(t *testing.T, body []byte) (uint64, string) {
	t.Helper()
	var arr []cbor.RawMessage
	mustUnmarshalBody(t, body, &arr)
	var on uint64
	var rating string
	mustUnmarshal(t, arr[0], &on)
	mustUnmarshal(t, arr[1], &rating)
	return on, rating
}

// ---- the per-server setting ----

func TestTheGifStatusSaysOnOrOffAndTheRatingAndNeverTheKey(t *testing.T) {
	ge := newGifEnv(t, false)
	status, body, _ := ge.get("")
	if on, rating := decodeStatus(t, body); status != http.StatusOK || on != 0 || rating != "pg-13" {
		t.Fatalf("no key = %d on=%d rating=%q; want 200, 0, pg-13", status, on, rating)
	}
	ge.setKey(t, testGiphyKey)
	status, body, hdr := ge.get("")
	if on, _ := decodeStatus(t, body); status != http.StatusOK || on != 1 {
		t.Fatalf("with a key = %d on=%d", status, on)
	}
	if bytes.Contains(body, []byte(testGiphyKey)) || strings.Contains(fmt.Sprint(hdr), testGiphyKey) {
		t.Fatal("the status read carries the key")
	}
	if status, _, _ := ge.getAs(ge.outside, ge.base+"/gifs"); status != http.StatusNotFound {
		t.Fatalf("a non-member's status read = %d, want 404", status)
	}
	resp := ge.Request(t, http.MethodGet, ge.base+"/gifs", "", nil, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous status read = %d, want 401", resp.StatusCode)
	}
}

func TestOnlyAManagerSetsReplacesOrRemovesTheKey(t *testing.T) {
	ge := newGifEnv(t, false)
	for name, tc := range map[string]struct {
		tok, method string
		body        any
		want        int
	}{
		"member sets":      {ge.member, http.MethodPut, []any{testGiphyKey}, http.StatusForbidden},
		"outsider sets":    {ge.outside, http.MethodPut, []any{testGiphyKey}, http.StatusNotFound},
		"member removes":   {ge.member, http.MethodDelete, nil, http.StatusForbidden},
		"outsider removes": {ge.outside, http.MethodDelete, nil, http.StatusNotFound},
		"member rating":    {ge.member, http.MethodPut, []any{"g"}, http.StatusForbidden},
	} {
		path := ge.base + "/gifs/key"
		if name == "member rating" {
			path = ge.base + "/gifs/rating"
		}
		if status, body := ge.Do(tc.method, path, tc.tok, tc.body); status != tc.want {
			t.Fatalf("%s = %d %x, want %d", name, status, body, tc.want)
		}
	}
	if on, _ := decodeStatusOf(t, ge); on != 0 {
		t.Fatal("a refused request changed the key")
	}
	ge.setKey(t, testGiphyKey)
	ge.setKey(t, "anotherKEY12345")
	if status, body := ge.Do(http.MethodDelete, ge.base+"/gifs/key", ge.owner, nil); status != http.StatusNoContent || len(body) != 0 {
		t.Fatalf("DELETE key = %d %x", status, body)
	}
	if on, _ := decodeStatusOf(t, ge); on != 0 {
		t.Fatal("the key is still on after DELETE")
	}
}

func decodeStatusOf(t *testing.T, ge *gifEnv) (uint64, string) {
	t.Helper()
	_, body, _ := ge.get("")
	return decodeStatus(t, body)
}

func TestAMemberHoldingAManageServerRoleMayEnterTheKeyUntilTheyLoseIt(t *testing.T) {
	ge := newGifEnv(t, false)
	put := func() int {
		status, _ := ge.Do(http.MethodPut, ge.base+"/gifs/key", ge.member, []any{testGiphyKey})
		return status
	}
	if status := put(); status != http.StatusForbidden {
		t.Fatalf("a plain member's PUT = %d, want 403", status)
	}
	role := createRole(t, ge.env, ge.cid, ge.owner, "keepers", 10, api.PermViewChannel|api.PermManageCommunity)
	grant(t, ge.env, ge.cid, ge.owner, ge.memberID, role, http.StatusNoContent)
	if status := put(); status != http.StatusNoContent {
		t.Fatalf("a non-owner with Manage Server: PUT = %d, want 204", status)
	}
	if on, _ := decodeStatusOf(t, ge); on != 1 {
		t.Fatal("the manager's key was not stored")
	}
	// The role is taken away: the very next change is refused, though the key stays (it belongs to the server).
	path := ge.base + "/members/" + ge.memberID.String() + "/roles/" + role.String()
	if status, body := ge.Do(http.MethodDelete, path, ge.owner, nil); status != http.StatusNoContent {
		t.Fatalf("revoke role = %d %x", status, body)
	}
	if status := put(); status != http.StatusForbidden {
		t.Fatalf("after losing the role: PUT = %d, want 403", status)
	}
	if status, _ := ge.Do(http.MethodDelete, ge.base+"/gifs/key", ge.member, nil); status != http.StatusForbidden {
		t.Fatalf("after losing the role: DELETE = %d, want 403", status)
	}
	if status, _ := ge.Do(http.MethodPut, ge.base+"/gifs/rating", ge.member, []any{"g"}); status != http.StatusForbidden {
		t.Fatalf("after losing the role: rating = %d, want 403", status)
	}
	if on, _ := decodeStatusOf(t, ge); on != 1 {
		t.Fatal("losing the role removed the server's key")
	}
}

func TestTheKeyShapeIsRefusedBeforeItIsStored(t *testing.T) {
	ge := newGifEnv(t, false)
	for name, key := range map[string]string{
		"empty": "", "short": "abc", "space": "abcd efgh1234", "ampersand": "abcdefgh&x=1", "slash": "abcdefgh/../x",
		"newline": "abcdefgh\n12", "long": strings.Repeat("a", 65), "unicode": "abcdefghé123",
	} {
		if status, body := ge.Do(http.MethodPut, ge.base+"/gifs/key", ge.owner, []any{key}); status != http.StatusBadRequest || ge.ErrCode(body) != "E_INVALID_REQUEST" {
			t.Fatalf("key %s = %d %x; want 400 E_INVALID_REQUEST", name, status, body)
		}
	}
	for name, body := range map[string]any{"two elements": []any{"abcdefgh123", "x"}, "not an array": "abcdefgh123", "wrong type": []any{uint64(5)}} {
		if status, _ := ge.Do(http.MethodPut, ge.base+"/gifs/key", ge.owner, body); status != http.StatusBadRequest {
			t.Fatalf("body %s = %d, want 400", name, status)
		}
	}
	if on, _ := decodeStatusOf(t, ge); on != 0 {
		t.Fatal("a refused key was stored")
	}
}

func TestTheRatingIsPerServerValidatedAndSentUpstream(t *testing.T) {
	ge := newGifEnv(t, true)
	if status, _ := ge.Do(http.MethodPut, ge.base+"/gifs/rating", ge.owner, []any{"x-rated"}); status != http.StatusBadRequest {
		t.Fatalf("bad rating = %d, want 400", status)
	}
	if status, body := ge.Do(http.MethodPut, ge.base+"/gifs/rating", ge.owner, []any{"g"}); status != http.StatusNoContent {
		t.Fatalf("rating g = %d %x", status, body)
	}
	if _, rating := decodeStatusOf(t, ge); rating != "g" {
		t.Fatalf("rating = %q, want g", rating)
	}
	if status, _, _ := ge.get("/search?q=cat"); status != http.StatusOK {
		t.Fatal("search failed")
	}
	if got := ge.fake.lastSearch()["rating"]; got != "g" {
		t.Fatalf("upstream rating = %q, want g", got)
	}
	// Removing the key keeps the rating the server chose.
	ge.Do(http.MethodDelete, ge.base+"/gifs/key", ge.owner, nil)
	if on, rating := decodeStatusOf(t, ge); on != 0 || rating != "g" {
		t.Fatalf("after remove: on=%d rating=%q; want 0 and g", on, rating)
	}
}

func TestEveryChangeIsAuditedWithoutTheKey(t *testing.T) {
	ge := newGifEnv(t, false)
	ge.setKey(t, testGiphyKey)
	ge.Do(http.MethodPut, ge.base+"/gifs/rating", ge.owner, []any{"pg"})
	ge.Do(http.MethodDelete, ge.base+"/gifs/key", ge.owner, nil)
	rows, err := ge.Repo.ListAudit(t.Context(), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, row := range rows {
		if !strings.HasPrefix(row.Action, "community.gifs.") {
			continue
		}
		seen[row.Action] = true
		if row.Target != ge.cid.String() || row.Actor == nil {
			t.Fatalf("audit row %+v: want the community as target and an actor", row)
		}
		if strings.Contains(row.Target+row.Detail+row.Action, testGiphyKey) {
			t.Fatalf("audit row carries the key: %+v", row)
		}
	}
	for _, want := range []string{"community.gifs.key_set", "community.gifs.rating", "community.gifs.key_remove"} {
		if !seen[want] {
			t.Fatalf("no audit row %s (saw %v)", want, seen)
		}
	}
}

func TestDeletingTheServerForgetsItsKey(t *testing.T) {
	ge := newGifEnv(t, true)
	if status, body := ge.Do(http.MethodDelete, ge.base, ge.owner, nil); status != http.StatusNoContent {
		t.Fatalf("DELETE community = %d %x", status, body)
	}
	raw, err := ge.Repo.GetSetting(t.Context(), "community_gifs:"+ge.cid.String())
	if err == nil && bytes.Contains(raw, []byte(testGiphyKey)) {
		t.Fatalf("the key survived the server: %s", raw)
	}
}

func TestEachServerUsesItsOwnKey(t *testing.T) {
	ge := newGifEnv(t, true)
	other := createCommunity(t, ge.env, ge.owner)
	joinCommunity(t, ge.env, other, ge.member)
	otherBase := "/v1/communities/" + other.String()
	// The other server has no key yet: 404, and Giphy is not contacted for it.
	if status, _, _ := ge.getAs(ge.member, otherBase+"/gifs/search?q=x"); status != http.StatusNotFound {
		t.Fatalf("a server without a key = %d, want 404", status)
	}
	if status, body := ge.Do(http.MethodPut, otherBase+"/gifs/key", ge.owner, []any{"secondSERVERkey99"}); status != http.StatusNoContent {
		t.Fatalf("PUT = %d %x", status, body)
	}
	ge.getAs(ge.member, otherBase+"/gifs/search?q=x")
	if got := ge.fake.lastSearch()["api_key"]; got != "secondSERVERkey99" {
		t.Fatalf("the second server searched with %q", got)
	}
	ge.get("/search?q=x")
	if got := ge.fake.lastSearch()["api_key"]; got != testGiphyKey {
		t.Fatalf("the first server searched with %q", got)
	}
}

// ---- the routes ----

func TestGifRoutesAnswer404WithoutAKey(t *testing.T) {
	ge := newGifEnv(t, false)
	for _, sub := range []string{"/search?q=cat", "/search", "/media/abc123?r=preview", "/media/abc123?r=full"} {
		status, body, _ := ge.get(sub)
		if status != http.StatusNotFound || ge.ErrCode(body) != "E_NOT_FOUND" {
			t.Fatalf("%s = %d %x; want 404 E_NOT_FOUND", sub, status, body)
		}
	}
	if n := ge.fake.hits.Load(); n != 0 {
		t.Fatalf("Giphy was contacted %d times without a key", n)
	}
}

func TestGifRoutesNeedASessionAndMembership(t *testing.T) {
	ge := newGifEnv(t, true)
	for _, sub := range []string{"/search?q=cat", "/media/abc123?r=preview"} {
		resp := ge.Request(t, http.MethodGet, ge.base+"/gifs"+sub, "", nil, nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s without a session = %d, want 401", sub, resp.StatusCode)
		}
		if status, _, _ := ge.getAs(ge.outside, ge.base+"/gifs"+sub); status != http.StatusNotFound {
			t.Fatalf("%s as a non-member = %d, want 404", sub, status)
		}
	}
	if n := ge.fake.hits.Load(); n != 0 {
		t.Fatalf("Giphy was contacted %d times for a caller who may not use it", n)
	}
}

func TestGifSearchAnswersIdsTitlesSizesAndProxyPathsAndHidesTheKey(t *testing.T) {
	ge := newGifEnv(t, true)
	status, body, hdr := ge.get("/search?q=cat+typing&offset=24")
	if status != http.StatusOK {
		t.Fatalf("search = %d %x", status, body)
	}
	if got := hdr.Get("Content-Type"); got != "application/cbor" {
		t.Fatalf("content type = %q", got)
	}
	var out []cbor.RawMessage
	mustUnmarshalBody(t, body, &out)
	var items [][]any
	mustUnmarshal(t, out[0], &items)
	if len(items) != 2 {
		t.Fatalf("results = %v; want the two with a sane id (the traversal id is dropped)", items)
	}
	first := items[0]
	wantPath := "/v1/communities/" + ge.cid.String() + "/gifs/media/abc123XYZ?r=preview"
	if first[0] != "abc123XYZ" || first[1] != "cat typing" || first[2] != uint64(200) || first[3] != uint64(113) || first[4] != wantPath {
		t.Fatalf("first result = %v", first)
	}
	var next uint64
	mustUnmarshal(t, out[1], &next)
	if next != 27 { // offset 24 + count 3 of 100
		t.Fatalf("next offset = %d, want 27", next)
	}
	if bytes.Contains(body, []byte(testGiphyKey)) || bytes.Contains(body, []byte("giphy.com")) {
		t.Fatalf("the reply carries the key or a giphy URL: %q", body)
	}
	for k, v := range hdr {
		if strings.Contains(strings.Join(v, ","), testGiphyKey) {
			t.Fatalf("header %s carries the key", k)
		}
	}
	q := ge.fake.lastSearch()
	if q["path"] != "/v1/gifs/search" || q["api_key"] != testGiphyKey || q["q"] != "cat typing" ||
		q["offset"] != "24" || q["rating"] != "pg-13" || q["limit"] != "24" {
		t.Fatalf("upstream query = %v", q)
	}
}

func TestGifSearchWithoutAQueryIsTrending(t *testing.T) {
	ge := newGifEnv(t, true)
	if status, body, _ := ge.get("/search"); status != http.StatusOK {
		t.Fatalf("trending = %d %x", status, body)
	}
	if path := ge.fake.lastSearch()["path"]; path != "/v1/gifs/trending" {
		t.Fatalf("an empty query hit %q, want trending", path)
	}
}

func TestGifSearchRefusesAMalformedQuery(t *testing.T) {
	ge := newGifEnv(t, true)
	for name, sub := range map[string]string{
		"q too long":        "/search?q=" + strings.Repeat("a", 101),
		"offset not a num":  "/search?q=a&offset=x",
		"offset negative":   "/search?q=a&offset=-1",
		"offset past limit": "/search?q=a&offset=5000",
		"control char":      "/search?q=a%00b",
	} {
		status, body, _ := ge.get(sub)
		if status != http.StatusBadRequest || ge.ErrCode(body) != "E_INVALID_REQUEST" {
			t.Fatalf("%s = %d %x; want 400 E_INVALID_REQUEST", name, status, body)
		}
	}
	if n := ge.fake.hits.Load(); n != 0 {
		t.Fatalf("a malformed query reached Giphy %d times", n)
	}
}

func TestGifMediaFetchesOnlyTheGiphyPathForTheIdAndReturnsTheBytes(t *testing.T) {
	ge := newGifEnv(t, true)
	for r, want := range map[string]string{"preview": "/media/abc123XYZ/200w.gif", "full": "/media/abc123XYZ/giphy.gif"} {
		status, body, hdr := ge.get("/media/abc123XYZ?r=" + r)
		if status != http.StatusOK || !bytes.Equal(body, tinyGIF) {
			t.Fatalf("%s = %d %x", r, status, body)
		}
		if hdr.Get("Content-Type") != "image/gif" || hdr.Get("X-Content-Type-Options") != "nosniff" ||
			!strings.HasPrefix(hdr.Get("Cache-Control"), "private") {
			t.Fatalf("%s headers = %v", r, hdr)
		}
		ge.fake.mu.Lock()
		got := ge.fake.paths[len(ge.fake.paths)-1]
		ge.fake.mu.Unlock()
		if got != want {
			t.Fatalf("%s fetched %q, want %q", r, got, want)
		}
	}
}

func TestGifMediaRefusesABadIdOrVariantBeforeAnyFetch(t *testing.T) {
	ge := newGifEnv(t, true)
	for name, sub := range map[string]string{
		"dot dot":          "/media/..?r=full",
		"encoded slash":    "/media/a%2Fb?r=full",
		"encoded traverse": "/media/..%2F..%2Fetc?r=full",
		"host in the id":   "/media/a.evil.com?r=full",
		"colon":            "/media/a:80?r=full",
		"at sign":          "/media/x@evil?r=full",
		"too long":         "/media/" + strings.Repeat("a", 65) + "?r=full",
		"no variant":       "/media/abc123",
		"bad variant":      "/media/abc123?r=original",
		"two variants":     "/media/abc123?r=full&r=preview",
	} {
		status, body, _ := ge.get(sub)
		// The ServeMux cleans a literal ".." away before the handler, so it never reaches the media route.
		if name == "dot dot" && status != http.StatusBadRequest {
			continue
		}
		if status != http.StatusBadRequest || ge.ErrCode(body) != "E_INVALID_REQUEST" {
			t.Fatalf("%s = %d %x; want 400 E_INVALID_REQUEST", name, status, body)
		}
	}
	if n := ge.fake.hits.Load(); n != 0 {
		t.Fatalf("a refused id reached Giphy %d times", n)
	}
}

func TestNoUserSuppliedURLReachesTheFetch(t *testing.T) {
	ge := newGifEnv(t, true)
	var evil atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { evil.Add(1) }))
	t.Cleanup(other.Close)
	for _, extra := range []string{"url", "u", "host", "src", "media_url"} {
		sub := "/media/abc123XYZ?r=full&" + extra + "=" + other.URL + "/x.gif"
		if status, _, _ := ge.get(sub); status != http.StatusOK {
			t.Fatalf("%s = %d", sub, status)
		}
	}
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, ge.Srv.URL+ge.base+"/gifs/media/abc123XYZ?r=full", nil)
	req.Header.Set("Authorization", "Bearer "+ge.member)
	req.Header.Set("X-Forwarded-Host", other.Listener.Addr().String())
	req.Header.Set("Referer", other.URL)
	req.Host = other.Listener.Addr().String()
	resp, err := ge.Srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if evil.Load() != 0 {
		t.Fatalf("a host outside the allow-list was fetched %d times", evil.Load())
	}
}

func TestGifMediaRefusesARedirectOffTheAllowList(t *testing.T) {
	ge := newGifEnv(t, true)
	var evil atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		evil.Add(1)
		w.Header().Set("Content-Type", "image/gif")
		_, _ = w.Write(tinyGIF)
	}))
	t.Cleanup(other.Close)
	ge.fake.media = func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/steal.gif", http.StatusFound)
	}
	status, body, _ := ge.get("/media/abc123XYZ?r=full")
	if status == http.StatusOK || evil.Load() != 0 {
		t.Fatalf("redirect off the allow-list = %d (%d fetches of the other host), body %x", status, evil.Load(), body)
	}
}

func TestGifMediaFollowsARedirectInsideTheAllowListButNotForever(t *testing.T) {
	ge := newGifEnv(t, true)
	ge.fake.media = func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/media/abc123XYZ/giphy.gif" {
			http.Redirect(w, r, "/media/hop/giphy.gif", http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "image/gif")
		_, _ = w.Write(tinyGIF)
	}
	if status, _, _ := ge.get("/media/abc123XYZ?r=full"); status != http.StatusOK {
		t.Fatalf("one redirect inside the allow-list = %d, want 200", status)
	}
	ge.fake.media = func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, r.URL.Path, http.StatusFound) // a loop
	}
	if status, _, _ := ge.get("/media/abc123XYZ?r=full"); status != http.StatusServiceUnavailable {
		t.Fatalf("a redirect loop = %d, want 503", status)
	}
}

func TestGifMediaEnforcesTheSizeCapOnTheBytesNotTheHeader(t *testing.T) {
	ge := newGifEnv(t, true)
	api.SetGiphyCapsForTest(ge.g, 100, 200)
	big := append([]byte("GIF89a"), bytes.Repeat([]byte{1}, 300)...)
	ge.fake.media = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/gif")
		_, _ = w.Write(big) // chunked: no honest Content-Length is promised
	}
	for r, cap := range map[string]int{"preview": 100, "full": 200} {
		status, body, _ := ge.get("/media/abc123XYZ?r=" + r)
		if status == http.StatusOK || bytes.Contains(body, []byte("GIF89a")) {
			t.Fatalf("%s over the %d-byte cap = %d, want a refusal with no GIF bytes", r, cap, status)
		}
	}
	ge.fake.media = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/gif")
		w.Header().Set("Content-Length", "1000000")
		_, _ = w.Write(tinyGIF[:10])
	}
	if status, _, _ := ge.get("/media/abc123XYZ?r=full"); status == http.StatusOK {
		t.Fatalf("a declared length over the cap was accepted")
	}
}

func TestGifMediaChecksTheContentTypeAndTheGIFMagic(t *testing.T) {
	ge := newGifEnv(t, true)
	for name, serve := range map[string]func(w http.ResponseWriter){
		"html":    func(w http.ResponseWriter) { w.Header().Set("Content-Type", "text/html"); _, _ = w.Write(tinyGIF) },
		"png":     func(w http.ResponseWriter) { w.Header().Set("Content-Type", "image/png"); _, _ = w.Write(tinyGIF) },
		"no type": func(w http.ResponseWriter) { w.Header()["Content-Type"] = nil; _, _ = w.Write(tinyGIF) },
		"gif type, html": func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "image/gif")
			_, _ = w.Write([]byte("<html>"))
		},
	} {
		ge.fake.media = func(w http.ResponseWriter, r *http.Request) { serve(w) }
		status, body, _ := ge.get("/media/abc123XYZ?r=full")
		if status == http.StatusOK || bytes.Contains(body, []byte("html")) {
			t.Fatalf("%s = %d %q; want a refusal", name, status, body)
		}
	}
}

func TestGifMediaMapsUpstreamStatusesAndEvery503CarriesRetryAfter(t *testing.T) {
	ge := newGifEnv(t, true)
	for upstream, want := range map[int]int{
		http.StatusNotFound:        http.StatusNotFound,
		http.StatusTooManyRequests: http.StatusServiceUnavailable,
		http.StatusBadGateway:      http.StatusServiceUnavailable,
		http.StatusForbidden:       http.StatusServiceUnavailable,
	} {
		ge.fake.media = func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(upstream) }
		status, _, hdr := ge.get("/media/abc123XYZ?r=full")
		if status != want {
			t.Fatalf("upstream %d = %d, want %d", upstream, status, want)
		}
		if want == http.StatusServiceUnavailable && hdr.Get("Retry-After") == "" {
			t.Fatalf("upstream %d: a 503 without Retry-After", upstream)
		}
	}
}

func TestGifMediaTimesOut(t *testing.T) {
	ge := newGifEnv(t, true)
	api.SetGiphyTimeoutForTest(ge.g, 50*time.Millisecond)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	ge.fake.media = func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}
	start := time.Now()
	status, _, _ := ge.get("/media/abc123XYZ?r=full")
	if status != http.StatusServiceUnavailable || time.Since(start) > 5*time.Second {
		t.Fatalf("a hung upstream = %d after %s, want 503 within the timeout", status, time.Since(start))
	}
}

// ---- memory and fairness (review H1, M1) ----

func TestASlotIsHeldUntilTheReplyIsWrittenSoAClientThatDoesNotReadCannotGrowMemory(t *testing.T) {
	ge := newGifEnv(t, true)
	api.SetGiphyCapsForTest(ge.g, 64<<20, 64<<20)
	api.SetGiphySlotsForTest(ge.g, 1, 3, 50*time.Millisecond)
	ge.fake.media = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/gif")
		_, _ = w.Write(bigGIF)
	}
	stuck := ge.stuckGet(ge.member, "/media/big?r=full")
	if !ge.waitHeld(1, 10*time.Second) {
		t.Fatal("the fetch never took the slot")
	}
	// The reply is far larger than the socket can buffer and nobody reads it: the slot stays held.
	time.Sleep(500 * time.Millisecond)
	if api.GiphyHeldForTest(ge.g) != 1 {
		t.Fatal("the slot was freed while the reply was still unwritten")
	}
	status, body, hdr := ge.getAs(ge.member2, ge.base+"/gifs/media/other?r=preview")
	if status != http.StatusServiceUnavailable || ge.ErrCode(body) != "E_UNAVAILABLE" || hdr.Get("Retry-After") == "" {
		t.Fatalf("a second fetch while the first is unread = %d %x; want 503 E_UNAVAILABLE with Retry-After", status, body)
	}
	// The client going away frees the slot.
	_ = stuck.Close()
	if !ge.waitHeld(0, 10*time.Second) {
		t.Fatal("the slot was not freed after the client left")
	}
}

func TestAReplyThatCannotBeWrittenInTimeIsDroppedAndItsSlotFreed(t *testing.T) {
	ge := newGifEnv(t, true)
	api.SetGiphyCapsForTest(ge.g, 64<<20, 64<<20)
	api.SetGiphyWriteTimeoutForTest(ge.g, 300*time.Millisecond)
	ge.fake.media = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/gif")
		_, _ = w.Write(bigGIF)
	}
	_ = ge.stuckGet(ge.member, "/media/big?r=full") // never read, never closed
	if !ge.waitHeld(1, 10*time.Second) {
		t.Fatal("the fetch never took the slot")
	}
	if !ge.waitHeld(0, 10*time.Second) {
		t.Fatal("the write deadline did not free the slot of a client that never reads")
	}
}

func TestOneMemberCannotTakeMoreThanItsShareOfTheSlots(t *testing.T) {
	ge := newGifEnv(t, true)
	api.SetGiphyCapsForTest(ge.g, 64<<20, 64<<20)
	api.SetGiphySlotsForTest(ge.g, 8, 2, 50*time.Millisecond)
	ge.fake.media = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/gif")
		if strings.Contains(r.URL.Path, "/big") {
			_, _ = w.Write(bigGIF)
			return
		}
		_, _ = w.Write(tinyGIF)
	}
	ge.stuckGet(ge.member, "/media/big1?r=full")
	ge.stuckGet(ge.member, "/media/big2?r=full")
	if !ge.waitHeld(2, 10*time.Second) {
		t.Fatal("the two fetches never took their slots")
	}
	status, body, hdr := ge.get("/media/small?r=preview")
	if status != http.StatusServiceUnavailable || ge.ErrCode(body) != "E_UNAVAILABLE" || hdr.Get("Retry-After") == "" {
		t.Fatalf("a third fetch of the same member = %d %x; want 503 with Retry-After", status, body)
	}
	if status, _, _ := ge.getAs(ge.member2, ge.base+"/gifs/media/small?r=preview"); status != http.StatusOK {
		t.Fatalf("another member's fetch = %d; one member's slots must not starve the others", status)
	}
}

func TestARequestWaitsBrieflyForAFreeSlotInsteadOfBeingRefusedAtOnce(t *testing.T) {
	ge := newGifEnv(t, true)
	api.SetGiphyCapsForTest(ge.g, 64<<20, 64<<20)
	api.SetGiphySlotsForTest(ge.g, 1, 3, 5*time.Second)
	ge.fake.media = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/gif")
		if strings.Contains(r.URL.Path, "/big") {
			_, _ = w.Write(bigGIF)
			return
		}
		_, _ = w.Write(tinyGIF)
	}
	stuck := ge.stuckGet(ge.member, "/media/big?r=full")
	if !ge.waitHeld(1, 10*time.Second) {
		t.Fatal("the fetch never took the slot")
	}
	time.AfterFunc(300*time.Millisecond, func() { _ = stuck.Close() })
	status, _, _ := ge.getAs(ge.member2, ge.base+"/gifs/media/small?r=preview")
	if status != http.StatusOK {
		t.Fatalf("a request that could wait for the slot = %d, want 200", status)
	}
}

// ---- rate limits ----

func TestGifSearchIsRateLimitedPerUser(t *testing.T) {
	ge := newGifEnv(t, true)
	var last int
	var lastBody []byte
	var lastHdr http.Header
	for i := 0; i < 11; i++ {
		last, lastBody, lastHdr = ge.get("/search?q=cat")
	}
	if last != http.StatusTooManyRequests || ge.ErrCode(lastBody) != "E_RATE_LIMITED" || lastHdr.Get("Retry-After") == "" {
		t.Fatalf("the 11th search = %d %x, want 429 E_RATE_LIMITED with Retry-After", last, lastBody)
	}
	hitsAtLimit := ge.fake.hits.Load()
	ge.get("/search?q=cat")
	if ge.fake.hits.Load() != hitsAtLimit {
		t.Fatal("a limited search still reached Giphy")
	}
	if status, _, _ := ge.getAs(ge.member2, ge.base+"/gifs/search?q=cat"); status != http.StatusOK {
		t.Fatalf("another user's search = %d; the bucket must be per user", status)
	}
}

func TestGifMediaIsRateLimitedPerUser(t *testing.T) {
	ge := newGifEnv(t, true)
	var limited bool
	for i := 0; i < 80 && !limited; i++ {
		status, _, _ := ge.get("/media/abc123XYZ?r=preview")
		limited = status == http.StatusTooManyRequests
	}
	if !limited {
		t.Fatal("80 media fetches in a row were never limited")
	}
	if status, _, _ := ge.getAs(ge.member2, ge.base+"/gifs/media/abc123XYZ?r=preview"); status != http.StatusOK {
		t.Fatalf("another user's fetch = %d; the media bucket must be per user", status)
	}
}

// ---- the key never leaks ----

func TestTheKeyNeverReachesALogOrAnErrorBodyWhenGiphyIsGone(t *testing.T) {
	ge := newGifEnv(t, true)
	ge.fake.srv.Close()
	for _, sub := range []string{"/search?q=cat", "/media/abc123XYZ?r=full"} {
		status, body, hdr := ge.get(sub)
		if status != http.StatusServiceUnavailable {
			t.Fatalf("%s with Giphy gone = %d %x, want 503", sub, status, body)
		}
		if bytes.Contains(body, []byte(testGiphyKey)) || strings.Contains(fmt.Sprint(hdr), testGiphyKey) {
			t.Fatalf("%s leaked the key to the caller", sub)
		}
	}
	if strings.Contains(ge.logs.String(), testGiphyKey) {
		t.Fatalf("the key reached the log:\n%s", ge.logs.String())
	}
	if ge.logs.Len() == 0 {
		t.Fatal("an upstream failure left no log line for the operator")
	}
}

func TestTheKeyNeverReachesALogWhenGiphyAnswersAnError(t *testing.T) {
	ge := newGifEnv(t, true)
	ge.fake.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad key "+r.URL.Query().Get("api_key"), http.StatusUnauthorized)
	})
	status, body, _ := ge.get("/search?q=cat")
	if status != http.StatusServiceUnavailable || bytes.Contains(body, []byte(testGiphyKey)) {
		t.Fatalf("Giphy 401 = %d %q; want 503 without the key", status, body)
	}
	if strings.Contains(ge.logs.String(), testGiphyKey) {
		t.Fatalf("the key reached the log:\n%s", ge.logs.String())
	}
}

func TestNoGifRouteEverEchoesTheKey(t *testing.T) {
	ge := newGifEnv(t, true)
	every := [][2]string{{http.MethodGet, ""}, {http.MethodGet, "/search?q=a"}, {http.MethodGet, "/media/abc123?r=full"},
		{http.MethodDelete, "/key"}, {http.MethodPut, "/rating"}}
	for _, c := range every {
		status, body := ge.Do(c[0], ge.base+"/gifs"+c[1], ge.owner, []any{"pg"})
		if bytes.Contains(body, []byte(testGiphyKey)) {
			t.Fatalf("%s %s = %d echoes the key", c[0], c[1], status)
		}
	}
	for _, line := range strings.Split(ge.logs.String(), "\n") {
		if strings.Contains(line, testGiphyKey) {
			t.Fatalf("log line carries the key: %s", line)
		}
	}
}

// ---- the dial guard and the production client ----

func TestGiphyDialsNeverReachANonPublicAddress(t *testing.T) {
	for addr, refused := range map[string]bool{
		"127.0.0.1:443": true, "[::1]:443": true, "10.1.2.3:443": true, "192.168.0.9:443": true, "172.16.0.1:443": true,
		"169.254.169.254:443": true, "100.64.0.1:443": true, "0.0.0.0:443": true, "0.1.2.3:443": true,
		"[fe80::1]:443": true, "[fc00::1]:443": true, "[fec0::1]:443": true, "[::ffff:10.0.0.1]:443": true,
		"[2002:7f00:1::1]:443": true, "[64:ff9b:1::1]:443": true, "[2001::1]:443": true, "[100::1]:443": true,
		"198.51.100.7:443": true, "203.0.113.7:443": true, "192.0.2.1:443": true, "224.0.0.1:443": true, "255.255.255.255:443": true,
		"151.101.2.1:443": false, "[2606:4700::1]:443": false,
	} {
		err := api.GiphyDialGuardForTest(addr)
		if (err != nil) != refused {
			t.Fatalf("dial guard for %s = %v, refused want %v", addr, err, refused)
		}
	}
}

// The production client, not a test double: pointed at a server on loopback it must refuse to connect.
func TestTheProductionClientRefusesToDialAPrivateAddress(t *testing.T) {
	ge := newGifEnv(t, true)
	api.UseFakeGiphyKeepingTransportForTest(ge.g, ge.fake.srv)
	status, _, _ := ge.get("/search?q=cat")
	if status != http.StatusServiceUnavailable {
		t.Fatalf("search through the production client to loopback = %d, want 503", status)
	}
	if n := ge.fake.hits.Load(); n != 0 {
		t.Fatalf("the production client connected to a loopback address (%d requests arrived)", n)
	}
}

func TestTheProductionClientHasNoProxyAndChecksRedirects(t *testing.T) {
	ge := newGifEnv(t, true)
	g := api.NewGifs(ge.Repo, nil, ge.Clk, slog.New(slog.DiscardHandler))
	noProxy, redirects := api.GiphyTransportForTest(g)
	if !noProxy || !redirects {
		t.Fatalf("production client: no proxy and a dial guard = %v, redirect check = %v; want both", noProxy, redirects)
	}
}

func TestTheProductionClientTalksOnlyToTheGiphyHosts(t *testing.T) {
	hosts, scheme := api.GiphyHostsForTest()
	if scheme != "https" {
		t.Fatalf("scheme = %q", scheme)
	}
	for _, h := range hosts {
		if !strings.HasSuffix(h, ".giphy.com") || strings.ContainsAny(h, "/:@") {
			t.Fatalf("allow-list entry %q is not a bare giphy.com host", h)
		}
	}
	if len(hosts) == 0 {
		t.Fatal("empty allow-list")
	}
	for u, ok := range map[string]bool{
		"https://media.giphy.com/media/a/giphy.gif": true, "https://media3.giphy.com/x": true, "http://media.giphy.com/x": false,
		"https://giphy.com.evil.test/x": false, "https://evilgiphy.com/x": false, "https://media.giphy.com.evil.test/x": false,
		"https://media.giphy.com:8443/x": false, "https://user@media.giphy.com/x": false, "https://127.0.0.1/x": false,
	} {
		if got := api.GiphyAllowedForTest(u); got != ok {
			t.Fatalf("allowed(%s) = %v, want %v", u, got, ok)
		}
	}
}

// ---- N1: capacity is shared fairly across servers ----

// secondServer is another server of the same owner, with a key and the ordinary member in it.
func (ge *gifEnv) secondServer(t *testing.T) string {
	t.Helper()
	other := createCommunity(t, ge.env, ge.owner)
	joinCommunity(t, ge.env, other, ge.member)
	base := "/v1/communities/" + other.String()
	if status, body := ge.Do(http.MethodPut, base+"/gifs/key", ge.owner, []any{"secondSERVERkey99"}); status != http.StatusNoContent {
		t.Fatalf("PUT = %d %x", status, body)
	}
	return base
}

func TestTheGifRateIsPerMemberNotPerServerAndMember(t *testing.T) {
	ge := newGifEnv(t, true)
	other := ge.secondServer(t)
	for i := 0; i < 10; i++ {
		if status, _, _ := ge.get("/search?q=cat"); status != http.StatusOK {
			t.Fatalf("search %d = %d", i, status)
		}
	}
	// The same member in a second server shares the one bucket, so joining servers does not multiply the rate.
	if status, _, _ := ge.getAs(ge.member, other+"/gifs/search?q=cat"); status != http.StatusTooManyRequests {
		t.Fatalf("the 11th search, in another server = %d, want 429", status)
	}
	var limited bool
	for i := 0; i < 80 && !limited; i++ {
		status, _, _ := ge.getAs(ge.member, other+"/gifs/media/abc123XYZ?r=preview")
		limited = status == http.StatusTooManyRequests
	}
	if !limited {
		t.Fatal("media fetches across two servers were never limited by one member bucket")
	}
}

func TestOneServerCannotTakeMoreThanItsShareOfTheSlots(t *testing.T) {
	ge := newGifEnv(t, true)
	other := ge.secondServer(t)
	api.SetGiphyCapsForTest(ge.g, 64<<20, 64<<20)
	api.SetGiphySlotsForTest(ge.g, 8, 2, 50*time.Millisecond)
	api.SetGiphyServerShareForTest(ge.g, 3)
	ge.fake.media = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/gif")
		if strings.Contains(r.URL.Path, "/big") {
			_, _ = w.Write(bigGIF)
			return
		}
		_, _ = w.Write(tinyGIF)
	}
	// Three slots of the first server held by readers that never read: two by one member, one by another.
	ge.stuckGet(ge.member, "/media/big1?r=full")
	ge.stuckGet(ge.member, "/media/big2?r=full")
	ge.stuckGet(ge.member2, "/media/big3?r=full")
	if !ge.waitHeld(3, 10*time.Second) {
		t.Fatal("the fetches never took their slots")
	}
	// A fourth in the same server is refused although five slots are free and this member holds none...
	status, body, hdr := ge.getAs(ge.owner, ge.base+"/gifs/media/small?r=preview")
	if status != http.StatusServiceUnavailable || ge.ErrCode(body) != "E_UNAVAILABLE" || hdr.Get("Retry-After") == "" {
		t.Fatalf("a fourth fetch in a server at its share = %d %x; want 503 with Retry-After", status, body)
	}
	// ...and another server is not starved.
	if status, _, _ := ge.getAs(ge.owner, other+"/gifs/media/small?r=preview"); status != http.StatusOK {
		t.Fatalf("a fetch in another server = %d; one server's readers must not starve the rest", status)
	}
}
