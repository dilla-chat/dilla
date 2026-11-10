package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dilla-chat/dilla/internal/auth"
	"github.com/dilla-chat/dilla/internal/id"
)

// One commit upload per device at a time: a second while the first is in flight is 429
// E_RATE_LIMITED, another device is not affected, and the slot frees when the first ends. The
// refusal comes before the delivery service is asked anything (this Groups has no DS at all).
func TestASecondConcurrentCommitFromOneDeviceIsRefused(t *testing.T) {
	h := &Groups{}
	device := id.New()
	done, err := h.beginCommit(device)
	if err != nil {
		t.Fatalf("first commit: %v", err)
	}

	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost,
		"/v1/groups/0102030405060708090a0b0c0d0e0f10/commit", bytes.NewReader([]byte{0x85}))
	r.SetPathValue("id", "0102030405060708090a0b0c0d0e0f10")
	r = r.WithContext(auth.WithSession(r.Context(), auth.Session{DeviceID: device, Scope: auth.ScopeEnrolled}))
	w := httptest.NewRecorder()
	h.commit(w, r)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 while the device's first commit is in flight", w.Code)
	}

	if release, err := h.beginCommit(id.New()); err != nil {
		t.Errorf("another device: %v", err)
	} else {
		release()
	}
	done()
	if release, err := h.beginCommit(device); err != nil {
		t.Errorf("after the first commit ended: %v", err)
	} else {
		release()
	}
}
