package api_test

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/auth"
	"github.com/dilla-chat/dilla/internal/config"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

// putUnsized PUTs body with no Content-Length (a chunked upload), straight into the mux, and
// returns the status.
func putUnsized(t *testing.T, e *env, ch id.ID, tok string, sum, body []byte) int {
	t.Helper()
	req := httptest.NewRequestWithContext(auth.WithSession(t.Context(), e.sess[tok]),
		http.MethodPut, blobURL(ch, sum), io.NopCloser(bytes.NewReader(body)))
	req.ContentLength = -1
	req.Header.Set("Content-Type", "application/octet-stream")
	rec := httptest.NewRecorder()
	e.Mux.ServeHTTP(rec, req)
	return rec.Code
}

// putBytes PUTs body (with its Content-Length) and returns the status and the answer.
func putBytes(t *testing.T, e *env, ch id.ID, tok string, body []byte) (int, []byte, []byte) {
	t.Helper()
	sum := sha256.Sum256(body)
	status, raw := e.DoRaw(http.MethodPut, blobURL(ch, sum[:]), tok, "application/octet-stream", body)
	return status, raw, sum[:]
}

// C7 (fix wave): blobs.uploads_per_minute is a per-user request bucket, checked before a byte of
// the body is read. The 21st upload in a minute is 429 E_RATE_LIMITED; a minute later it passes.
func TestTheUploadRateIsBoundedPerUser(t *testing.T) {
	e, ch, tok := blobEnv(t) // uploads_per_minute = 20 by default
	for i := range 20 {
		if status, _, _ := putBytes(t, e, ch, tok, []byte{byte(i), 1, 2, 3}); status != http.StatusCreated {
			t.Fatalf("upload %d = %d, want 201", i+1, status)
		}
	}
	status, raw, _ := putBytes(t, e, ch, tok, []byte{0xee, 1, 2, 3})
	if status != http.StatusTooManyRequests || e.ErrCode(raw) != "E_RATE_LIMITED" {
		t.Fatalf("the 21st upload in a minute = %d (%x), want 429 E_RATE_LIMITED", status, raw)
	}
	e.Clk.Advance(time.Minute)
	if status, _, _ := putBytes(t, e, ch, tok, []byte{0xee, 1, 2, 3}); status != http.StatusCreated {
		t.Fatalf("an upload a minute later = %d, want 201", status)
	}
}

// blobs.upload_bytes_per_day is a per-user byte budget. Deleting a reference frees the quota but
// not the budget, so upload-then-delete churn is bounded; and bytes the instance read and then
// refused (a hash mismatch) spend it too.
func TestTheDailyUploadBudgetCountsChurnAndRefusals(t *testing.T) {
	e, ch, tok := blobEnvWithConfig(t, func(c *config.Blobs) {
		c.MaxBlobBytes, c.UploadBytesPerDay = 1<<20, 10_000
	})
	first := bytes.Repeat([]byte{'a'}, 6000)
	status, _, sum := putBytes(t, e, ch, tok, first)
	if status != http.StatusCreated {
		t.Fatalf("first upload = %d", status)
	}
	if status, _ := e.Do(http.MethodDelete, blobURL(ch, sum), tok, nil); status != http.StatusNoContent {
		t.Fatalf("DELETE = %d", status)
	}
	status, raw, _ := putBytes(t, e, ch, tok, bytes.Repeat([]byte{'b'}, 6000))
	if status != http.StatusTooManyRequests || e.ErrCode(raw) != "E_RATE_LIMITED" {
		t.Fatalf("a second 6000 bytes on a 10000-byte daily budget after a delete = %d (%x), want 429",
			status, raw)
	}

	e2, ch2, tok2 := blobEnvWithConfig(t, func(c *config.Blobs) {
		c.MaxBlobBytes, c.UploadBytesPerDay = 1<<20, 10_000
	})
	wrong := bytes.Repeat([]byte{'c'}, 6000)
	other := sha256.Sum256([]byte("not these bytes"))
	if status, _ := e2.DoRaw(http.MethodPut, blobURL(ch2, other[:]), tok2, "application/octet-stream", wrong); status != http.StatusUnprocessableEntity {
		t.Fatalf("a hash mismatch = %d, want 422", status)
	}
	if status, _, _ := putBytes(t, e2, ch2, tok2, bytes.Repeat([]byte{'d'}, 6000)); status != http.StatusTooManyRequests {
		t.Fatalf("an upload after 6000 refused bytes on a 10000-byte budget = %d, want 429", status)
	}
	// A day later the budget is back.
	e2.Clk.Advance(24 * time.Hour)
	if status, _, _ := putBytes(t, e2, ch2, tok2, bytes.Repeat([]byte{'d'}, 6000)); status != http.StatusCreated {
		t.Fatalf("an upload a day later = %d, want 201", status)
	}
}

// The quota pre-check counts the body the request announces: used + Content-Length over the quota
// is 507 before a byte is read, so no file is written and nothing is orphaned.
func TestTheQuotaPreCheckCountsTheBody(t *testing.T) {
	e, ch, tok := blobEnvWithLimits(t, 1<<20, 4096)
	if status, _, _ := putBytes(t, e, ch, tok, bytes.Repeat([]byte{'a'}, 3000)); status != http.StatusCreated {
		t.Fatal("first upload failed")
	}
	status, _, sum := putBytes(t, e, ch, tok, bytes.Repeat([]byte{'b'}, 3000))
	if status != http.StatusInsufficientStorage {
		t.Fatalf("an upload that would pass the quota = %d, want 507", status)
	}
	if _, err := e.Repo.GetBlob(t.Context(), sum); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the refused upload left a row: %v", err)
	}
	if _, err := e.Blobs.Stat(sum); err == nil {
		t.Fatal("the refused upload left a file")
	}
}

// blobs.store_max_bytes bounds the instance: an upload that would take every blob row, referenced
// or not, past it is 507 before the body is read.
func TestTheInstanceStoreLimitIsEnforced(t *testing.T) {
	e, ch, tok := blobEnvWithConfig(t, func(c *config.Blobs) {
		c.MaxBlobBytes, c.StoreMaxBytes = 1<<20, 5000
	})
	status, _, sum := putBytes(t, e, ch, tok, bytes.Repeat([]byte{'a'}, 3000))
	if status != http.StatusCreated {
		t.Fatalf("first upload = %d", status)
	}
	// Deleting the reference does not free the disk until the sweeper collects the file.
	if status, _ := e.Do(http.MethodDelete, blobURL(ch, sum), tok, nil); status != http.StatusNoContent {
		t.Fatalf("DELETE = %d", status)
	}
	status, raw, _ := putBytes(t, e, ch, tok, bytes.Repeat([]byte{'b'}, 3000))
	if status != http.StatusInsufficientStorage || e.ErrCode(raw) != "E_STORAGE_FULL" {
		t.Fatalf("an upload past store_max_bytes = %d (%x), want 507 E_STORAGE_FULL", status, raw)
	}
}
