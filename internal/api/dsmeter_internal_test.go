package api

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/auth"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/config"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/server"
)

// dsTestRate gives every delivery-service class a burst of its own, so a test can tell the buckets
// apart: message 2, commit 3, proposal 4 (and so the KeyPackage pair bucket), read 5, write 6.
func dsTestRate() config.Rate {
	r := config.Default().Limits.Rate
	r.Enabled = true
	r.MessagePerSecond, r.MessageBurst = 0.001, 2
	r.CommitPerSecond, r.CommitBurst = 0.001, 3
	r.ProposalPerSecond, r.ProposalBurst = 0.001, 4
	r.ReadPerSecond, r.ReadBurst = 0.001, 5
	r.WritePerSecond, r.WriteBurst = 0.001, 6
	return r
}

func meteredCall(t *testing.T, l *server.RateLimiter, class string, device id.ID) *httptest.ResponseRecorder {
	t.Helper()
	h := dsMeter(l, class, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/x", nil)
	r = r.WithContext(auth.WithSession(r.Context(), auth.Session{DeviceID: device, UserID: id.New()}))
	w := httptest.NewRecorder()
	h(w, r)
	return w
}

// Each delivery-service class is its own bucket, sized from its own [limits.rate] pair, keyed by
// the device session: the burst is served, the next request is 429 E_RATE_LIMITED with
// retry_after_ms, another device is unaffected, and spending one class leaves the others full.
func TestEveryDeliveryServiceClassIsMeteredPerDevice(t *testing.T) {
	for _, tc := range []struct {
		class string
		burst int
	}{
		{dsClassMessage, 2}, {dsClassCommit, 3}, {dsClassProposal, 4}, {dsClassRead, 5}, {dsClassWrite, 6},
	} {
		t.Run(tc.class, func(t *testing.T) {
			l := server.NewRateLimiter(dsTestRate(), clock.NewFake(time.Unix(1_700_000_000, 0)))
			device := id.New()
			for i := range tc.burst {
				if w := meteredCall(t, l, tc.class, device); w.Code != http.StatusNoContent {
					t.Fatalf("request %d of a burst of %d: status %d", i+1, tc.burst, w.Code)
				}
			}
			w := meteredCall(t, l, tc.class, device)
			if w.Code != http.StatusTooManyRequests {
				t.Fatalf("past the burst: status %d, want 429", w.Code)
			}
			var body []any
			if err := cborx.Unmarshal(w.Body.Bytes(), &body); err != nil || len(body) < 3 || body[0] != "E_RATE_LIMITED" {
				t.Fatalf("body %v (%v), want [E_RATE_LIMITED, detail, retry_after_ms]", body, err)
			}
			if ms, ok := body[2].(uint64); !ok || ms == 0 {
				t.Errorf("retry_after_ms = %v, want a positive wait", body[2])
			}
			if w.Header().Get("Retry-After") == "" {
				t.Error("no Retry-After header")
			}
			if w := meteredCall(t, l, tc.class, id.New()); w.Code != http.StatusNoContent {
				t.Errorf("another device was refused: %d", w.Code)
			}
			other := dsClassRead
			if tc.class == dsClassRead {
				other = dsClassWrite
			}
			if w := meteredCall(t, l, other, device); w.Code != http.StatusNoContent {
				t.Errorf("an empty %s bucket refused a %s request: %d", tc.class, other, w.Code)
			}
		})
	}
}

// KeyPackage fetches are bucketed per (requester, target): one requester draining one target is
// refused after the proposal burst, while the same requester fetching another target, and another
// requester fetching the same target, are not.
func TestKeyPackageFetchesAreBucketedPerRequesterAndTarget(t *testing.T) {
	l := server.NewRateLimiter(dsTestRate(), clock.NewFake(time.Unix(1_700_000_000, 0)))
	requester, target := id.New(), id.New()
	for i := range 4 {
		if err := allowDS(l, dsClassKeyPackage, keyPackageBucket(requester, target)); err != nil {
			t.Fatalf("fetch %d: %v", i+1, err)
		}
	}
	err := allowDS(l, dsClassKeyPackage, keyPackageBucket(requester, target))
	var se *server.Error
	if !errors.As(err, &se) || se.Code != server.CodeRateLimited || se.RetryAfterMS == nil {
		t.Fatalf("the fifth fetch of one target: %v, want E_RATE_LIMITED with retry_after_ms", err)
	}
	if err := allowDS(l, dsClassKeyPackage, keyPackageBucket(requester, id.New())); err != nil {
		t.Errorf("another target: %v", err)
	}
	if err := allowDS(l, dsClassKeyPackage, keyPackageBucket(id.New(), target)); err != nil {
		t.Errorf("another requester: %v", err)
	}
}

// A Plan 2 route is metered on the class its pattern names: a read for GET and HEAD, the message
// bucket for the readable upload (the one route that is the plaintext twin of POST
// /v1/groups/{id}/message), and the write bucket for every other change.
func TestAPlanTwoRouteIsMeteredOnTheClassItsPatternNames(t *testing.T) {
	for pattern, want := range map[string]string{
		"GET /v1/communities/{id}":                       dsClassRead,
		"HEAD /v1/channels/{id}/blobs/{b}":               dsClassRead,
		"POST /v1/channels/{id}/messages":                dsClassMessage,
		"PATCH /v1/channels/{id}/messages/{s}":           dsClassWrite,
		"PUT /v1/channels/{id}/blobs/{blob_id}":          dsClassWrite,
		"POST /v1/channels/{id}/blobs/{blob_id}/confirm": dsClassWrite,
		"POST /v1/communities":                           dsClassWrite,
		"DELETE /v1/admin/blobs/{blob_id}":               dsClassWrite,
	} {
		if got := routeClass(pattern); got != want {
			t.Errorf("routeClass(%q) = %q, want %q", pattern, got, want)
		}
	}
}
