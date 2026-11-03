package server_test

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/server"
)

func errorsNew(s string) error { return errors.New(s) }

func bytesContains(haystack, needle []byte) bool { return bytes.Contains(haystack, needle) }

func TestRateLimitedAfterReportsWholeMillisecondsAndNeverWraps(t *testing.T) {
	for _, tc := range []struct {
		wait time.Duration
		want uint64
	}{
		{1500 * time.Millisecond, 1500},
		{time.Second + 999*time.Microsecond, 1000},
		{0, 0},
		{-time.Second, 0}, // a negative wait must not wrap to a 18-quintillion-millisecond retry
	} {
		e := server.RateLimitedAfter(tc.wait)
		if e.RetryAfterMS == nil || *e.RetryAfterMS != tc.want {
			t.Errorf("RateLimitedAfter(%v).RetryAfterMS = %v, want %d", tc.wait, e.RetryAfterMS, tc.want)
		}
	}
}

// The twenty-six rows of protocol/02 § Errors, exercised one by one, plus the
// one WithStatus override 1a uses (a duplicate username is E_INVALID_REQUEST at
// 409, not at 400). The last four have no Plan 1 call site — Plan 2's readable
// channels raise them — and are asserted here because this package declares them.
func TestEveryCodeMapsToItsStatusAndArrayShape(t *testing.T) {
	cases := []struct {
		err    *server.Error
		status int
		length int // CBOR array elements
	}{
		{server.Errorf(server.CodeInvalidRequest, "bad body"), 400, 3},
		{server.Errorf(server.CodeBindingInvalid, "no binding"), 400, 3},
		{server.Errorf(server.CodeEnvelopeShape, ""), 400, 3},
		{server.Errorf(server.CodeEnvelopeType, ""), 400, 3},
		{server.Errorf(server.CodeEnvelopeLimit, ""), 400, 3},
		{server.Errorf(server.CodeUnauthenticated, ""), 401, 3},
		{server.Errorf(server.CodeForbidden, ""), 403, 3},
		{server.Errorf(server.CodeChannelMode, ""), 403, 3},
		{server.Errorf(server.CodeLeafNotCurrent, ""), 403, 3},
		{server.Errorf(server.CodeModeReadable, ""), 403, 3},
		{server.Errorf(server.CodeNotUploader, ""), 403, 3},
		{server.Errorf(server.CodeProvisionalOutsidePairing, ""), 403, 3},
		{server.Errorf(server.CodeNotFound, ""), 404, 3},
		{server.Errorf(server.CodeGroupExists, ""), 409, 3},
		{server.Errorf(server.CodeRemovePending, ""), 409, 3},
		{server.Errorf(server.CodeCallFull, ""), 409, 3},
		{server.Errorf(server.CodeCallSharersFull, ""), 409, 3},
		{server.Errorf(server.CodePruned, ""), 410, 3},
		{server.Errorf(server.CodeInviteInvalid, ""), 410, 3},
		{server.Errorf(server.CodeTooLarge, ""), 413, 3},
		{server.Errorf(server.CodeCommitmentInvalid, ""), 422, 3},
		{server.Errorf(server.CodeStorageFull, ""), 507, 3},
		{server.Unavailable(500, "the call is busy"), 503, 3},
		{server.CommitConflict([]byte{1}, [][]byte{{2}}), 409, 5},
		{server.CommitRequired([][]byte{{3}}, 250), 425, 4},
		{server.CommitInvalid("group_info_signature"), 422, 4},
		{server.RateLimited(1500), 429, 3},
		{server.Errorf(server.CodeInternal, ""), 500, 3},
		{server.Version([]uint64{1}, []uint64{1}, []uint64{1}), 400, 6},
		{server.WithStatus(http.StatusConflict, server.Errorf(server.CodeInvalidRequest, "username taken")), 409, 3},
	}
	for _, tc := range cases {
		t.Run(string(tc.err.Code), func(t *testing.T) {
			rec := httptest.NewRecorder()
			server.WriteError(rec, tc.err)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d", rec.Code, tc.status)
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/cbor" {
				t.Fatalf("content type = %q", ct)
			}
			var arr []any
			if err := cborx.Unmarshal(rec.Body.Bytes(), &arr); err != nil {
				t.Fatalf("body is not deterministic CBOR: %v", err)
			}
			if len(arr) != tc.length {
				t.Fatalf("array has %d elements, want %d", len(arr), tc.length)
			}
			if arr[0] != string(tc.err.Code) {
				t.Fatalf("element 0 = %v, want %q", arr[0], tc.err.Code)
			}
		})
	}
}

func TestRateLimitedSetsRetryAfterHeaderInWholeSeconds(t *testing.T) {
	rec := httptest.NewRecorder()
	server.WriteError(rec, server.RateLimited(1500))
	if got := rec.Header().Get("Retry-After"); got != "2" {
		t.Fatalf("Retry-After = %q, want \"2\" (1500ms rounded up)", got)
	}
	var arr []any
	if err := cborx.Unmarshal(rec.Body.Bytes(), &arr); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if arr[2] != uint64(1500) {
		t.Fatalf("retry_after_ms = %v, want 1500", arr[2])
	}
}

func TestUnknownErrorBecomesFiveHundredWithoutLeakingIt(t *testing.T) {
	rec := httptest.NewRecorder()
	server.WriteError(rec, errorsNew("database exploded at /var/lib/dilla/dilla.db"))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if bytesContains(rec.Body.Bytes(), []byte("/var/lib/dilla")) {
		t.Fatal("the internal error text reached the client")
	}
}

// protocol/02 § Errors is normative that E_INTERNAL's detail is ALWAYS empty.
// That has to hold for a *server.Error too, not only for the anonymous error
// above: server.Errorf(server.CodeInternal, "sql: %v", err) is the natural
// spelling for a handler in parts 1b and 2, and WriteError is the one place the
// whole plan funnels refusals through, so the invariant is enforced here rather
// than policed at every call site.
func TestInternalDetailIsAlwaysEmptyEvenForAServerError(t *testing.T) {
	rec := httptest.NewRecorder()
	err := server.Errorf(server.CodeInternal, "sql: no such table users (db at /var/lib/dilla/dilla.db)")
	server.WriteError(rec, err)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if bytesContains(rec.Body.Bytes(), []byte("/var/lib/dilla")) {
		t.Fatal("the internal error text reached the client")
	}
	var arr []any
	if decErr := cborx.Unmarshal(rec.Body.Bytes(), &arr); decErr != nil {
		t.Fatalf("decode: %v", decErr)
	}
	if arr[1] != "" {
		t.Fatalf("detail = %q, want the empty string for E_INTERNAL", arr[1])
	}
	// The caller's own error value is untouched: WriteError redacts the wire
	// body, it does not erase the text the server still wants to log.
	if err.Detail == "" {
		t.Fatal("WriteError blanked the caller's *Error; the log line loses its cause")
	}
}
