package client

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
)

func testID(n byte) id.ID { var v id.ID; v[15] = n; return v }
func cbor(t *testing.T, v any) []byte {
	t.Helper()
	b, e := cborx.Marshal(v)
	if e != nil {
		t.Fatal(e)
	}
	return b
}
func reply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/cbor")
	w.WriteHeader(status)
	if v != nil {
		b, _ := cborx.Marshal(v)
		_, _ = w.Write(b)
	}
}
func discovery(idv id.ID) any {
	return []any{[]any{uint64(6)}, []any{uint64(1)}, []any{uint64(1)}, idv, uint64(1), "chat.example", uint64(0), []any{uint64(0)}, uint64(1), testID(44), make([]byte, 32)}
}
func newTestClient(t *testing.T, handler http.HandlerFunc, timeout time.Duration) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	c, e := New(Config{BaseURL: "https://example.invalid", HTTPClient: &http.Client{Transport: rewriteTransport{base: srv.Client().Transport, host: u.Host}}, InstanceID: testID(99), Timeout: timeout})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = c.Discover(context.Background()); e != nil {
		t.Fatal(e)
	}
	return c
}

type rewriteTransport struct {
	base http.RoundTripper
	host string
}

func (r rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	u := *req.URL
	u.Scheme = "http"
	u.Host = r.host
	clone.URL = &u
	clone.Host = r.host
	return r.base.RoundTrip(clone)
}

func TestNewRejectsUnsafeBaseURLsAndAllowsExplicitLoopback(t *testing.T) {
	for _, base := range []string{"http://example.org", "https://u:p@example.org", "https://example.org/path", "https://example.org/?token=x", "https://example.org/#frag", "ftp://example.org", "https://example.org%2f.evil"} {
		if _, e := New(Config{BaseURL: base, InstanceID: testID(1), AllowLoopbackHTTP: true}); e == nil {
			t.Errorf("New(%q) unexpectedly succeeded", base)
		}
	}
	if _, e := New(Config{BaseURL: "http://127.0.0.1:8080", InstanceID: testID(1), AllowLoopbackHTTP: true}); e != nil {
		t.Fatal(e)
	}
	if _, e := New(Config{BaseURL: "http://localhost:8080", InstanceID: testID(1), AllowLoopbackHTTP: true}); e != nil {
		t.Fatal(e)
	}
}

func TestDiscoveryPinsInstanceAndRequiresWireThreeBeforeOtherCalls(t *testing.T) {
	var instance = testID(2)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reply(w, 200, discovery(instance)) }))
	defer s.Close()
	c, e := New(Config{BaseURL: s.URL, HTTPClient: s.Client(), InstanceID: testID(1), AllowLoopbackHTTP: true})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = c.Session("secret"); e != nil {
		t.Fatal(e)
	}
	if _, _, e = c.do(context.Background(), "secret", http.MethodGet, "communities", nil, nil, MaxSmallBody, false); e == nil {
		t.Fatal("API request before discovery accepted")
	}
	if _, e = c.Discover(context.Background()); !errors.Is(e, ErrInstanceChanged) {
		t.Fatalf("instance mismatch = %v", e)
	}
	instance = testID(1)
	if _, e = c.Discover(context.Background()); e != nil {
		t.Fatal(e)
	}
}

func TestDiscoveryRefusesWireTwoOnly(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d := discovery(testID(1)).([]any)
		d[0] = []any{uint64(2)}
		reply(w, 200, d)
	}))
	defer s.Close()
	c, err := New(Config{BaseURL: s.URL, HTTPClient: s.Client(), InstanceID: testID(1), AllowLoopbackHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.Discover(context.Background()); !errors.Is(err, ErrProtocol) {
		t.Fatalf("wire2-only discovery=%v", err)
	}
}

func TestRuntimePaginationAdvancesAfterLastIDAndKeepsScope(t *testing.T) {
	var calls atomic.Int32
	var runtimeAuth atomic.Int32
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/instance" {
			reply(w, 200, discovery(testID(99)))
			return
		}
		if r.Header.Get("Authorization") != "Bearer runtime-secret" {
			t.Errorf("runtime auth = %q", r.Header.Get("Authorization"))
		}
		runtimeAuth.Add(1)
		calls.Add(1)
		start := 1
		if r.URL.Query().Get("after") != "" {
			if r.URL.Query().Get("after") != testID(100).String() {
				t.Errorf("cursor = %q", r.URL.Query().Get("after"))
			}
			start = 101
		}
		rows := make([]any, 0, 100)
		end := start + 100
		if end > 102 {
			end = 102
		}
		for n := start; n < end; n++ {
			rows = append(rows, []any{testID(byte(n)), testID(250), uint64(0), uint64(1), nil, nil, uint64(0), uint64(n)})
		}
		reply(w, 200, rows)
	}, time.Second)
	r, e := c.Runtime("runtime-secret")
	if e != nil {
		t.Fatal(e)
	}
	items, e := r.ListInstallations(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	if len(items) != 101 || calls.Load() != 2 || runtimeAuth.Load() != 2 {
		t.Fatalf("items=%d calls=%d auth=%d", len(items), calls.Load(), runtimeAuth.Load())
	}
}

func TestRuntimeAndSessionTokensNeverCrossScopes(t *testing.T) {
	var seen []string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/instance" {
			reply(w, 200, discovery(testID(99)))
			return
		}
		seen = append(seen, r.URL.Path+" "+r.Header.Get("Authorization"))
		if strings.HasSuffix(r.URL.Path, "/claim") {
			reply(w, 200, []any{testID(9), testID(8), uint64(100)})
			return
		}
		reply(w, 200, []any{uint64(6), uint64(1), uint64(1)})
	}, time.Second)
	r, _ := c.Runtime("runtime-A")
	s, _ := c.Session("device-B")
	if _, e := r.Claim(context.Background(), testID(9), testID(7)); e != nil {
		t.Fatal(e)
	}
	if _, e := s.Negotiate(context.Background(), []uint64{6}, []uint64{1}, []uint64{1}); e != nil {
		t.Fatal(e)
	}
	if len(seen) != 2 || seen[0] != "/v1/bot-runtime/installations/"+testID(9).String()+"/claim Bearer runtime-A" || seen[1] != "/v1/sessions/negotiate Bearer device-B" {
		t.Fatalf("requests: %v", seen)
	}
}

func TestRefusalClassificationAndUnknownAcceptedMutation(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   any
		want   Outcome
		code   string
	}{{422, []any{"E_COMMIT_INVALID", "bad", nil, "structural"}, OutcomeDefinitive, "E_COMMIT_INVALID"}, {400, []any{"E_VERSION", "upgrade", nil, []any{uint64(3)}, []any{uint64(1)}, []any{uint64(1)}}, OutcomeDefinitive, "E_VERSION"}, {403, []any{"E_FORBIDDEN", "denied", nil}, OutcomeDefinitive, "E_FORBIDDEN"}, {409, []any{"E_INVALID_REQUEST", "conflict", nil}, OutcomeDefinitive, "E_INVALID_REQUEST"}, {503, []any{"E_UNAVAILABLE", "busy", uint64(100)}, OutcomeUnknown, "E_OUTCOME_UNKNOWN"}} {
		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/v1/instance" {
				reply(w, 200, discovery(testID(99)))
				return
			}
			reply(w, tc.status, tc.body)
		}, time.Second)
		s, _ := c.Session("device-token")
		_, e := s.Proposal(context.Background(), testID(7), 4, []byte("cipher"))
		var ae *APIError
		if !errors.As(e, &ae) || ae.Outcome != tc.want || ae.Code != tc.code {
			t.Fatalf("status %d error %#v", tc.status, e)
		}
	}
	accepted := atomic.Bool{}
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/instance" {
			reply(w, 200, discovery(testID(99)))
			return
		}
		accepted.Store(true)
		h, ok := w.(http.Hijacker)
		if !ok {
			t.Fatal("hijack unavailable")
		}
		conn, _, e := h.Hijack()
		if e != nil {
			t.Fatal(e)
		}
		_ = conn.Close()
	}, time.Second)
	s, _ := c.Session("device-token")
	_, e := s.Proposal(context.Background(), testID(7), 4, []byte("cipher"))
	var ae *APIError
	if !accepted.Load() || !errors.As(e, &ae) || ae.Outcome != OutcomeUnknown {
		t.Fatalf("accepted=%v error=%v", accepted.Load(), e)
	}
}

func TestEpochZeroGroupMutationsAreSentUnchanged(t *testing.T) {
	seen := map[string]bool{}
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/instance" {
			reply(w, 200, discovery(testID(99)))
			return
		}
		b, _ := io.ReadAll(r.Body)
		var a []any
		if err := cborx.Unmarshal(b, &a); err != nil {
			t.Errorf("decode %s: %v", r.URL.Path, err)
			reply(w, 500, []any{"E_INTERNAL", "", nil})
			return
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/commit"):
			seen["commit"] = a[0] == uint64(0)
			reply(w, 200, []any{uint64(1), uint64(1)})
		case strings.HasSuffix(r.URL.Path, "/proposal"):
			seen["proposal"] = a[0] == uint64(0)
			reply(w, 200, []any{uint64(2)})
		case strings.HasSuffix(r.URL.Path, "/message"):
			seen["message"] = a[0] == uint64(0)
			reply(w, 200, []any{uint64(3), make([]byte, 32), uint64(4)})
		default:
			t.Errorf("unexpected route %s", r.URL.Path)
			reply(w, 500, []any{"E_INTERNAL", "", nil})
		}
	}, time.Second)
	s, _ := c.Session("device")
	if _, err := s.Commit(context.Background(), testID(7), 0, []byte("commit"), []byte("info"), nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Proposal(context.Background(), testID(7), 0, []byte("proposal")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SendMessage(context.Background(), testID(7), 0, []byte("message"), 0); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"commit", "proposal", "message"} {
		if !seen[name] {
			t.Errorf("%s did not carry epoch zero", name)
		}
	}
}

func TestVoidMutationsRequireNoContent(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/instance" {
			reply(w, 200, discovery(testID(99)))
			return
		}
		reply(w, 200, []any{})
	}, time.Second)
	s, _ := c.Session("device")
	for name, call := range map[string]func() error{
		"device list": func() error {
			return s.PublishDeviceList(context.Background(), testID(2), 1, []byte("blob"), make([]byte, 64), make([]byte, 32))
		},
		"welcome": func() error { return s.AcknowledgeWelcome(context.Background(), 1) },
		"cursor":  func() error { return s.AdvanceCursor(context.Background(), testID(7), 1, 0) },
	} {
		err := call()
		var ae *APIError
		if !errors.As(err, &ae) || ae.Outcome != OutcomeUnknown {
			t.Errorf("%s wrong success status error=%v", name, err)
		}
	}
}

func TestTypedRecoveryRoutesAndResponseStatuses(t *testing.T) {
	var healUpload, report bool
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/instance" {
			reply(w, 200, discovery(testID(99)))
			return
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/heal") && r.Method == http.MethodGet:
			reply(w, 200, []any{uint64(2), uint64(9), uint64(3), uint64(8)})
		case strings.HasSuffix(r.URL.Path, "/heal") && r.Method == http.MethodPost:
			healUpload = true
			reply(w, 200, []any{uint64(2), uint64(10)})
		case strings.HasSuffix(r.URL.Path, "/fork-report"):
			report = true
			reply(w, 202, []any{})
		default:
			t.Errorf("unexpected route %s %s", r.Method, r.URL.Path)
			reply(w, 404, []any{"E_NOT_FOUND", "", nil})
		}
	}, time.Second)
	s, _ := c.Session("device")
	status, err := s.HealStatus(context.Background(), testID(7))
	if err != nil || status.NeedFromSequence != 8 || status.Generation != 3 {
		t.Fatalf("status=%+v err=%v", status, err)
	}
	healed, err := s.Heal(context.Background(), testID(7), []byte("info"), nil, nil)
	if err != nil || healed.NextSequence != 10 || !healUpload {
		t.Fatalf("heal=%+v upload=%v err=%v", healed, healUpload, err)
	}
	if err = s.ForkReport(context.Background(), testID(7), 2, 9, "diverged"); err != nil || !report {
		t.Fatalf("report=%v err=%v", report, err)
	}
}

func TestLargeCommitRequiredRefusalsRetainProposalRefs(t *testing.T) {
	refs := make([]any, 10000)
	for i := range refs {
		refs[i] = bytes.Repeat([]byte{byte(i)}, 16)
	}
	response := cbor(t, []any{"E_COMMIT_REQUIRED", "outstanding proposals", uint64(25), refs})
	if len(response) <= MaxSmallBody || len(response) > MaxDSBody {
		t.Fatalf("fixture size %d outside expected bounded range", len(response))
	}
	for _, route := range []string{"message", "resync"} {
		t.Run(route, func(t *testing.T) {
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/instance" {
					reply(w, 200, discovery(testID(99)))
					return
				}
				w.Header().Set("Content-Type", "application/cbor")
				w.WriteHeader(425)
				_, _ = w.Write(response)
			}, time.Second)
			s, _ := c.Session("device")
			var err error
			if route == "message" {
				_, err = s.SendMessage(context.Background(), testID(7), 0, []byte("message"), 0)
			} else {
				_, err = s.Resync(context.Background(), testID(7), []byte("commit"), []byte("info"))
			}
			var ae *APIError
			if !errors.As(err, &ae) || ae.Code != "E_COMMIT_REQUIRED" || ae.Outcome != OutcomeDefinitive || ae.RetryAfterMS == nil || *ae.RetryAfterMS != 25 || len(ae.Proposals) != len(refs) {
				t.Fatalf("large typed refusal=%#v err=%v", ae, err)
			}
		})
	}
}

func TestWelcomePageRejectsZeroStaleAndRegressingIDs(t *testing.T) {
	for _, tc := range []struct {
		name  string
		after uint64
		ids   []uint64
	}{
		{name: "zero", ids: []uint64{0}},
		{name: "stale", after: 4, ids: []uint64{4}},
		{name: "mixed page", after: 4, ids: []uint64{5, 6, 5}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows := make([]any, len(tc.ids))
			for i, idv := range tc.ids {
				rows[i] = []any{idv, testID(7), uint64(1), uint64(1), []byte("welcome"), []byte("tree"), make([]byte, 32)}
			}
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/instance" {
					reply(w, 200, discovery(testID(99)))
					return
				}
				reply(w, 200, rows)
			}, time.Second)
			s, _ := c.Session("device")
			if _, err := s.Welcomes(context.Background(), tc.after, 64); !errors.Is(err, ErrProtocol) {
				t.Fatalf("malformed page error=%v", err)
			}
		})
	}
}

func TestRecoveryErrorsRetainTypedDataAndSanitizeUnknownCode(t *testing.T) {
	winner := []byte("winner-secret")
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/instance" {
			reply(w, 200, discovery(testID(99)))
			return
		}
		reply(w, 409, []any{"E_COMMIT_CONFLICT", "private detail", nil, winner, []any{[]byte("proposal")}})
	}, time.Second)
	s, _ := c.Session("device")
	_, err := s.Proposal(context.Background(), testID(7), 0, []byte("p"))
	var ae *APIError
	if !errors.As(err, &ae) || ae.Outcome != OutcomeDefinitive || string(ae.WinningCommit) != string(winner) || len(ae.Proposals) != 1 {
		t.Fatalf("typed conflict=%#v", err)
	}
	if strings.Contains(err.Error(), "private") || strings.Contains(err.Error(), "winner-secret") {
		t.Fatalf("error leaked server payload: %s", err)
	}
	c2 := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/instance" {
			reply(w, 200, discovery(testID(99)))
			return
		}
		reply(w, 400, []any{"E_SECRET", "private", nil})
	}, time.Second)
	s2, _ := c2.Session("device")
	_, err = s2.Proposal(context.Background(), testID(7), 0, []byte("p"))
	if !errors.As(err, &ae) || ae.Code != "E_HTTP_ERROR" || strings.Contains(err.Error(), "E_SECRET") {
		t.Fatalf("unknown error was reflected: %v", err)
	}
}

func TestReadinessConflictKeepsOnlyKnownDiscriminator(t *testing.T) {
	for _, tc := range []struct {
		detail string
		want   string
	}{
		{"installation prerequisites are still pending", "installation prerequisites are still pending"},
		{"installation revision or state changed", "installation revision or state changed"},
		{"managed role is missing", "managed role is missing"},
		{"request id was used for a different readiness operation", "request id was used for a different readiness operation"},
		{"arbitrary private server detail", ""},
	} {
		t.Run(tc.detail, func(t *testing.T) {
			e := parseAPIError(http.StatusConflict, "E_INVALID_REQUEST", []any{"E_INVALID_REQUEST", tc.detail, nil})
			if e.ReadinessConflict != tc.want {
				t.Fatalf("readiness conflict = %q, want %q", e.ReadinessConflict, tc.want)
			}
			if strings.Contains(e.Error(), tc.detail) {
				t.Fatalf("error leaked server detail: %s", e)
			}
		})
	}
}

func TestVersionRefusalPreservesSupportedVersions(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/instance" {
			reply(w, 200, discovery(testID(99)))
			return
		}
		reply(w, 400, []any{"E_VERSION", "upgrade", nil, []any{uint64(3), uint64(2)}, []any{uint64(1)}, []any{uint64(1)}})
	}, time.Second)
	s, _ := c.Session("device")
	_, err := s.Negotiate(context.Background(), []uint64{3}, []uint64{1}, []uint64{1})
	var ae *APIError
	if !errors.As(err, &ae) || ae.Code != "E_VERSION" || len(ae.WireVersions) != 2 || ae.WireVersions[0] != 3 || ae.WireVersions[1] != 2 {
		t.Fatalf("version refusal=%#v", err)
	}
}

// Bot v2 Task 1: a wire 6 instance tells a Bot offering an older wire to update. The refusal is a
// typed, definitive APIError, not an unknown outcome to retry.
func TestUpgradeRequiredRefusalIsTypedAndDefinitive(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/instance" {
			reply(w, 200, discovery(testID(99)))
			return
		}
		reply(w, 426, []any{"E_UPGRADE_REQUIRED", "update", nil})
	}, time.Second)
	s, _ := c.Session("device")
	_, err := s.Negotiate(context.Background(), []uint64{WireVersion}, []uint64{1}, []uint64{1})
	var ae *APIError
	if !errors.As(err, &ae) || ae.Code != "E_UPGRADE_REQUIRED" || ae.Status != 426 || ae.Outcome != OutcomeDefinitive {
		t.Fatalf("upgrade refusal=%#v", err)
	}
}

func TestRepresentativeTypedRoutesMatchWireStatusesAndShapes(t *testing.T) {
	installation := []any{testID(9), testID(8), uint64(1), uint64(2), nil, nil, uint64(0), uint64(100)}
	message := []any{uint64(1), uint64(0), testID(2), nil, nil, make([]byte, 32), uint64(100), uint64(0), nil}
	welcome := []any{uint64(1), testID(7), uint64(1), uint64(2), []byte("welcome"), []byte("tree"), make([]byte, 32)}
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/instance" {
			reply(w, 200, discovery(testID(99)))
			return
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/credential"):
			reply(w, 200, installation)
		case strings.HasSuffix(r.URL.Path, "/device-list") && r.Method == http.MethodGet:
			reply(w, 200, []any{uint64(1), []byte("list"), make([]byte, 64), make([]byte, 32)})
		case strings.HasSuffix(r.URL.Path, "/device-list") && r.Method == http.MethodPut:
			w.WriteHeader(204)
		case strings.HasSuffix(r.URL.Path, "/keypackages") && r.Method == http.MethodPost:
			reply(w, 201, []any{uint64(1)})
		case strings.Contains(r.URL.Path, "/keypackages/") && r.Method == http.MethodGet:
			reply(w, 200, []any{uint64(2), make([]byte, 32), uint64(1)})
		case strings.HasSuffix(r.URL.Path, "/keypackage"):
			reply(w, 200, []any{[]byte("kp"), uint64(0), []byte("ref")})
		case r.URL.Path == "/v1/welcomes":
			reply(w, 200, []any{welcome})
		case strings.HasSuffix(r.URL.Path, "/messages"):
			reply(w, 200, []any{message})
		case strings.HasSuffix(r.URL.Path, "/handshakes"):
			reply(w, 200, []any{})
		default:
			t.Errorf("unexpected route %s %s", r.Method, r.URL.Path)
			reply(w, 404, []any{"E_NOT_FOUND", "", nil})
		}
	}, time.Second)
	s, _ := c.Session("device")
	if _, err := s.FinalizeCredential(context.Background(), testID(9), testID(10), 1, []byte("credential")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.OwnDeviceList(context.Background(), testID(2)); err != nil {
		t.Fatal(err)
	}
	if err := s.PublishDeviceList(context.Background(), testID(2), 2, []byte("list"), make([]byte, 64), make([]byte, 32)); err != nil {
		t.Fatal(err)
	}
	upload, _ := encode([]any{[]any{[]byte("kp")}, nil})
	if n, err := s.PublishKeyPackages(context.Background(), testID(9), testID(10), 2, upload); err != nil || n != 1 {
		t.Fatalf("published=%d err=%v", n, err)
	}
	if receipt, err := s.KeyPackagePublicationReceipt(context.Background(), testID(9), testID(10)); err != nil || receipt.Generation != 2 || receipt.Count != 1 {
		t.Fatalf("publication receipt=%+v err=%v", receipt, err)
	}
	keyPackage, err := s.KeyPackage(context.Background(), testID(2))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(keyPackage, []byte("kp")) {
		t.Fatalf("KeyPackage returned %x, want exact TLS body %x", keyPackage, []byte("kp"))
	}
	if rows, err := s.Welcomes(context.Background(), 0, 10); err != nil || len(rows) != 1 {
		t.Fatalf("welcomes=%v err=%v", rows, err)
	}
	if page, err := s.GroupPage(context.Background(), testID(7), "messages", 0, 10); err != nil || len(page.Rows) != 1 {
		t.Fatalf("page=%v err=%v", page, err)
	}
	if page, err := s.GroupPage(context.Background(), testID(7), "handshakes", 0, 10); err != nil || len(page.Rows) != 0 {
		t.Fatalf("handshakes=%v err=%v", page, err)
	}
}

func TestOnlyEnrollmentAllowsIdempotentHTTP200Replay(t *testing.T) {
	enroll := "bot-runtime/installations/" + testID(9).String() + "/enroll"
	keypackages := "bot-runtime/installations/" + testID(9).String() + "/keypackages"
	if !statusAllowed(http.MethodPost, enroll, http.StatusOK, http.StatusCreated) {
		t.Fatal("exact enrollment replay status 200 was rejected")
	}
	if statusAllowed(http.MethodPost, keypackages, http.StatusOK, http.StatusCreated) {
		t.Fatal("unexpected KeyPackage status 200 was accepted")
	}
}

func TestRedirectDoesNotForwardCredentials(t *testing.T) {
	var leaked atomic.Bool
	dest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			leaked.Store(true)
		}
		reply(w, 200, []any{uint64(1)})
	}))
	defer dest.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/instance" {
			reply(w, 200, discovery(testID(99)))
			return
		}
		http.Redirect(w, r, dest.URL+"/steal", http.StatusFound)
	}))
	defer origin.Close()
	hc := origin.Client()
	c, e := New(Config{BaseURL: origin.URL, HTTPClient: hc, InstanceID: testID(99), AllowLoopbackHTTP: true})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = c.Discover(context.Background()); e != nil {
		t.Fatal(e)
	}
	s, _ := c.Session("private-device-token")
	_, e = s.Proposal(context.Background(), testID(7), 4, []byte("cipher"))
	var ae *APIError
	if !errors.As(e, &ae) || ae.Outcome != OutcomeUnknown {
		t.Fatalf("redirect error = %v", e)
	}
	if leaked.Load() {
		t.Fatal("credential followed redirect")
	}
}

func TestTimeoutAndMalformedAcceptedResponseAreUnknown(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/instance" {
			reply(w, 200, discovery(testID(99)))
			return
		}
		time.Sleep(100 * time.Millisecond)
		reply(w, 200, []any{uint64(1)})
	}, 20*time.Millisecond)
	s, _ := c.Session("device-token")
	_, e := s.Proposal(context.Background(), testID(7), 4, []byte("cipher"))
	var ae *APIError
	if !errors.As(e, &ae) || ae.Outcome != OutcomeUnknown {
		t.Fatalf("timeout = %v", e)
	}
	c2 := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/instance" {
			reply(w, 200, discovery(testID(99)))
			return
		}
		w.Header().Set("Content-Type", "application/cbor")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte{0x82, 0x01, 0x02}) // deterministic array with wrong response arity
	}, time.Second)
	s2, _ := c2.Session("device-token")
	_, e = s2.Proposal(context.Background(), testID(7), 4, []byte("cipher"))
	if !errors.As(e, &ae) || ae.Outcome != OutcomeUnknown {
		t.Fatalf("malformed success = %v", e)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type closeBody struct {
	*strings.Reader
	closed *atomic.Bool
}

func (b closeBody) Close() error { b.closed.Store(true); return nil }

func TestOversizedMutationResponseIsUnknownAndBodyCloses(t *testing.T) {
	var closed atomic.Bool
	instanceBody := cbor(t, discovery(testID(99)))
	hc := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body := []byte{1, 2, 3, 4}
		if r.URL.Path == "/v1/instance" {
			body = instanceBody
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/cbor"}}, Body: closeBody{Reader: strings.NewReader(string(body)), closed: &closed}, Request: r}, nil
	})}
	c, e := New(Config{BaseURL: "https://example.invalid", HTTPClient: hc, InstanceID: testID(99)})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = c.Discover(context.Background()); e != nil {
		t.Fatal(e)
	}
	closed.Store(false)
	_, _, e = c.do(context.Background(), "secret", http.MethodPost, "test", nil, []byte{0x80}, 2, true)
	var ae *APIError
	if !errors.As(e, &ae) || ae.Outcome != OutcomeUnknown {
		t.Fatalf("oversized mutation response = %v", e)
	}
	if !closed.Load() {
		t.Fatal("response body was not closed after oversize")
	}
}

func TestCommitBodyExceedsOneMiBCapAndAllows256Welcomes(t *testing.T) {
	var got int
	var welcomeCount int
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/instance" {
			reply(w, 200, discovery(testID(99)))
			return
		}
		body, _ := io.ReadAll(r.Body)
		got = len(body)
		var request []any
		if err := cborx.Unmarshal(body, &request); err != nil {
			t.Error(err)
		} else if rows, ok := request[3].([]any); ok {
			welcomeCount = len(rows)
		}
		reply(w, 200, []any{uint64(8), uint64(3)})
	}, 5*time.Second)
	s, _ := c.Session("device-token")
	payload := make([]byte, (1<<20)+4096)
	welcomes := make([][]any, 256)
	for n := range welcomes {
		var device id.ID
		device[14] = byte((n + 1) >> 8)
		device[15] = byte(n + 1)
		welcomes[n] = []any{device, []byte{byte(n)}}
	}
	_, e := s.Commit(context.Background(), testID(7), 2, payload, []byte{1}, welcomes, nil)
	if e != nil {
		t.Fatal(e)
	}
	if got <= 1<<20 {
		t.Fatalf("commit request length %d did not exceed one MiB", got)
	}
	if welcomeCount != 256 {
		t.Fatalf("commit welcomed %d devices, want valid maximum of 256", welcomeCount)
	}
}

type socketCall struct {
	method, route string
	body, idem    []byte
}

type fakeSocketTransport struct {
	mu     sync.Mutex
	calls  []socketCall
	status int
	reply  []byte
}

func (f *fakeSocketTransport) Do(_ context.Context, method, route string, body, idem []byte) (int, []byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, socketCall{method: method, route: route, body: append([]byte(nil), body...), idem: append([]byte(nil), idem...)})
	return f.status, f.reply, nil
}

func TestBotClientUsesSocketForSocketRoutes(t *testing.T) {
	var mu sync.Mutex
	var overHTTP []string
	device, user := testID(5), testID(6)
	head := cbor(t, []any{uint64(1), []byte{1}, make([]byte, 64), make([]byte, 32)})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		overHTTP = append(overHTTP, r.Method+" "+r.URL.Path)
		mu.Unlock()
		switch r.URL.Path {
		case "/v1/instance":
			reply(w, 200, discovery(testID(99)))
		case "/v1/users/" + user.String() + "/device-list":
			w.Header().Set("Content-Type", "application/cbor")
			_, _ = w.Write(head)
		default:
			reply(w, 404, []any{"E_NOT_FOUND", "", nil})
		}
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	c, err := New(Config{BaseURL: "https://example.invalid", HTTPClient: &http.Client{Transport: rewriteTransport{base: srv.Client().Transport, host: u.Host}},
		InstanceID: testID(99), Timeout: 5 * time.Second, SocketRoutes: []string{"DELETE /v1/welcomes/{id}"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.Discover(context.Background()); err != nil {
		t.Fatal(err)
	}
	socket := &fakeSocketTransport{status: http.StatusNoContent}
	c.UseTransport(device, socket)
	// A stopped gateway that was already replaced never withdraws its replacement.
	c.ReleaseTransport(device, &fakeSocketTransport{})
	s, err := c.SessionForDevice("device-token", device)
	if err != nil {
		t.Fatal(err)
	}
	// A socket route goes through the transport, with a fresh 16-byte idempotency key.
	if err := s.AcknowledgeWelcome(context.Background(), 7); err != nil {
		t.Fatal(err)
	}
	// An HTTP route keeps HTTPS.
	if _, err := s.OwnDeviceList(context.Background(), user); err != nil {
		t.Fatal(err)
	}
	socket.mu.Lock()
	calls := append([]socketCall(nil), socket.calls...)
	socket.mu.Unlock()
	if len(calls) != 1 || calls[0].method != http.MethodDelete || calls[0].route != "/v1/welcomes/7" || len(calls[0].body) != 0 || len(calls[0].idem) != 16 {
		t.Fatalf("socket calls = %+v, want one DELETE /v1/welcomes/7 with a 16-byte idem", calls)
	}
	mu.Lock()
	got := append([]string(nil), overHTTP...)
	mu.Unlock()
	want := []string{"GET /v1/instance", "GET /v1/users/" + user.String() + "/device-list"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("HTTP requests = %v, want %v", got, want)
	}
	// A refusal over the socket is the same APIError as over HTTP.
	socket.status, socket.reply = http.StatusNotFound, cbor(t, []any{"E_NOT_FOUND", "", nil})
	var apiErr *APIError
	if err := s.AcknowledgeWelcome(context.Background(), 8); !errors.As(err, &apiErr) || apiErr.Status != 404 || apiErr.Code != "E_NOT_FOUND" {
		t.Fatalf("socket refusal = %v, want E_NOT_FOUND (404)", err)
	}
	// A session that names no device, and a session of another device, keep HTTPS for every route.
	plain, _ := c.Session("other-token")
	if err := plain.AcknowledgeWelcome(context.Background(), 9); err == nil {
		t.Fatal("the plain session's DELETE should have reached the HTTP 404")
	}
	// With the transport withdrawn the socket route is ErrNoGateway: retryable, and it never reaches HTTPS, which
	// would answer a misleading 404 for a route dillad serves on the socket only.
	c.ReleaseTransport(device, socket)
	mu.Lock()
	before := len(overHTTP)
	mu.Unlock()
	if err := s.AcknowledgeWelcome(context.Background(), 10); !errors.Is(err, ErrNoGateway) {
		t.Fatalf("after the transport is withdrawn the DELETE = %v, want ErrNoGateway", err)
	}
	mu.Lock()
	after := len(overHTTP)
	mu.Unlock()
	if err := s.AcknowledgeWelcome(context.Background(), 11); !IsDeferred(err) {
		t.Fatalf("ErrNoGateway = %v must be a deferred (retryable) error, not a failed reconcile", err)
	}
	mu.Lock()
	after = len(overHTTP)
	mu.Unlock()
	if after != before {
		t.Fatalf("a socket route with no gateway reached the HTTP server (%d requests, was %d)", after, before)
	}
	if n := len(socket.calls); n != 2 {
		t.Fatalf("socket saw %d calls, want 2", n)
	}
}

func TestBotSocketRouteMatching(t *testing.T) {
	routes := compileSocketRoutes([]string{"DELETE /v1/welcomes/{id}", "GET /v1/groups/{id}/messages", "POST /v1/bot/{rest...}"})
	for _, tc := range []struct {
		method, path string
		want         bool
	}{
		{"DELETE", "/v1/welcomes/7", true},
		{"GET", "/v1/welcomes/7", false},
		{"DELETE", "/v1/welcomes", false},
		{"DELETE", "/v1/welcomes/7/x", false},
		{"GET", "/v1/groups/ab/messages", true},
		{"POST", "/v1/bot/a/b/c", true},
	} {
		if got := routes.match(tc.method, tc.path); got != tc.want {
			t.Errorf("%s %s matched=%v, want %v", tc.method, tc.path, got, tc.want)
		}
	}
}

// The Bot reads its Welcomes, acknowledges them and claims a KeyPackage on the socket: dillad serves those three
// routes there only, and keeps POST /v1/keypackages and the device lists on HTTPS.
func TestBotDefaultSocketRoutesCarryTheMovedDeliveryRoutes(t *testing.T) {
	routes := compileSocketRoutes(SocketRoutes)
	for _, tc := range []struct {
		method, path string
		want         bool
	}{
		{"GET", "/v1/welcomes", true},
		{"DELETE", "/v1/welcomes/7", true},
		{"GET", "/v1/devices/0102030405060708090a0b0c0d0e0f10/keypackage", true},
		{"POST", "/v1/keypackages", false},
		{"PUT", "/v1/users/0102030405060708090a0b0c0d0e0f10/device-list", false},
	} {
		if got := routes.match(tc.method, tc.path); got != tc.want {
			t.Errorf("%s %s matched=%v, want %v", tc.method, tc.path, got, tc.want)
		}
	}
}
