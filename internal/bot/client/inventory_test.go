package client

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/id"
)

func TestDiscoverObservedUsesSamePinnedResponseAndLeavesBadDateUnavailable(t *testing.T) {
	instance := testID(99)
	var date string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/instance" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		w.Header().Set("X-Dilla-Generation", "7")
		w.Header().Set("Date", date)
		reply(w, http.StatusOK, discovery(instance))
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	c, err := New(Config{BaseURL: "https://instance.invalid", HTTPClient: &http.Client{Transport: rewriteTransport{base: srv.Client().Transport, host: u.Host}}, InstanceID: instance})
	if err != nil {
		t.Fatal(err)
	}

	date = time.Unix(1_800_000_000, 0).UTC().Format(http.TimeFormat)
	d, observation, err := c.DiscoverObserved(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if d.Generation != 1 || !observation.HasGeneration || observation.Generation != 7 || !observation.HasServerTime || observation.ServerTime != 1_800_000_000 {
		t.Fatalf("discovery/observation mismatch: generation=%d observation=%+v", d.Generation, observation)
	}

	date = "not-a-date"
	_, observation, err = c.DiscoverObserved(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if observation.HasServerTime || !observation.HasGeneration || observation.Generation != 7 {
		t.Fatalf("malformed Date was treated as authority: %+v", observation)
	}
}

func TestKeyPackageInventoryUsesScopedRouteAndStrictTuple(t *testing.T) {
	instance, installation := testID(99), testID(12)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/instance" {
			reply(w, http.StatusOK, discovery(instance))
			return
		}
		if r.URL.Path != "/v1/bot-runtime/installations/"+installation.String()+"/keypackages" || r.Method != http.MethodGet {
			t.Fatalf("unexpected inventory request %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer device-session" {
			t.Fatalf("inventory did not use device bearer")
		}
		reply(w, http.StatusOK, []any{uint64(4), uint64(1_800_000_001), uint64(6), uint64(1)})
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	c, err := New(Config{BaseURL: "https://instance.invalid", HTTPClient: &http.Client{Transport: rewriteTransport{base: srv.Client().Transport, host: u.Host}}, InstanceID: instance})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.Discover(context.Background()); err != nil {
		t.Fatal(err)
	}
	s, err := c.Session("device-session")
	if err != nil {
		t.Fatal(err)
	}
	inventory, err := s.KeyPackageInventory(context.Background(), installation)
	if err != nil {
		t.Fatal(err)
	}
	if inventory.Generation != 4 || inventory.ServerTime != 1_800_000_001 || inventory.OrdinaryActive != 6 || !inventory.HasCurrentFallback {
		t.Fatalf("unexpected inventory %#v", inventory)
	}
	if _, err := s.KeyPackageInventory(context.Background(), id.Zero); err == nil {
		t.Fatal("zero installation was accepted")
	}
}
