package client

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestKeyPackageExhaustedKeepsItsServerCode(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/instance") {
			reply(w, http.StatusOK, discovery(testID(99)))
			return
		}
		reply(w, http.StatusNotFound, []any{"E_KEYPACKAGES_EXHAUSTED", "key package", nil})
	}, time.Second)
	s, _ := c.Session("device")
	_, err := s.KeyPackage(context.Background(), testID(2))
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusNotFound || apiErr.Code != "E_KEYPACKAGES_EXHAUSTED" {
		t.Fatalf("KeyPackage error = %#v, want 404 E_KEYPACKAGES_EXHAUSTED", err)
	}
}
