package client

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jonasthim/dilla/internal/cborx"
	"github.com/jonasthim/dilla/internal/id"
)

func TestRuntimeClaimSpotifyCodeStrictlyDecodesBrokerReceipt(t *testing.T) {
	attempt := id.New()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/bot-runtime/installations/"+testID(7).String()+"/spotify/claim" || r.Header.Get("Authorization") != "Bearer runtime" || r.ContentLength != 0 {
			t.Errorf("request = %s %s auth=%q length=%d", r.Method, r.URL.Path, r.Header.Get("Authorization"), r.ContentLength)
		}
		w.Header().Set("Content-Type", "application/cbor")
		b, _ := cborx.Marshal([]any{attempt, "auth-code", strings.Repeat("v", 43)})
		_, _ = w.Write(b)
	}))
	defer srv.Close()
	c, err := New(Config{BaseURL: srv.URL, HTTPClient: srv.Client(), InstanceID: testID(1), AllowLoopbackHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	c.discovered.Store(true)
	r, err := c.Runtime("runtime")
	if err != nil {
		t.Fatal(err)
	}
	got, err := r.ClaimSpotifyCode(context.Background(), testID(7))
	if err != nil {
		t.Fatal(err)
	}
	if got.Attempt != attempt || got.Code != "auth-code" || got.Verifier != strings.Repeat("v", 43) {
		t.Fatalf("claim = %#v", got)
	}
	serialized, err := json.Marshal(got)
	if err != nil || strings.Contains(string(serialized), got.Code) || strings.Contains(string(serialized), got.Verifier) {
		t.Fatalf("claim JSON exposed credentials")
	}
}

func TestRuntimeSpotifyConfigurationReadsCommunitySetting(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/bot-runtime/installations/"+testID(7).String()+"/spotify/config" || r.Header.Get("Authorization") != "Bearer runtime" {
			t.Errorf("request = %s %s auth=%q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/cbor")
		b, _ := cborx.Marshal([]any{"publicclient", "https://dilla.test/settings/bot/music/spotify/callback"})
		_, _ = w.Write(b)
	}))
	defer srv.Close()
	c, err := New(Config{BaseURL: srv.URL, HTTPClient: srv.Client(), InstanceID: testID(1), AllowLoopbackHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	c.discovered.Store(true)
	r, err := c.Runtime("runtime")
	if err != nil {
		t.Fatal(err)
	}
	got, err := r.SpotifyConfiguration(context.Background(), testID(7))
	if err != nil || got.ClientID != "publicclient" || got.RedirectURL != "https://dilla.test/settings/bot/music/spotify/callback" {
		t.Fatalf("config = %+v, %v", got, err)
	}
}

func TestRuntimeClaimSpotifyCodeRejectsMalformedReceipt(t *testing.T) {
	for _, payload := range [][]any{{id.New(), "code", "verifier", "extra"}, {id.New(), "", "verifier"}, {id.ID{}, "code", "verifier"}, {id.New(), "code", ""}, {id.New(), "code", strings.Repeat("v", 42)}, {id.New(), "code", strings.Repeat("v", 42) + " "}} {
		t.Run("malformed", func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/cbor")
				b, _ := cborx.Marshal(payload)
				_, _ = w.Write(b)
			}))
			defer srv.Close()
			c, _ := New(Config{BaseURL: srv.URL, HTTPClient: srv.Client(), InstanceID: testID(1), AllowLoopbackHTTP: true})
			c.discovered.Store(true)
			r, _ := c.Runtime("runtime")
			if _, err := r.ClaimSpotifyCode(context.Background(), testID(7)); err == nil {
				t.Fatal("malformed receipt accepted")
			}
		})
	}
}

func TestRuntimeClaimSpotifyCodeDistinguishesNoPendingReceipt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/cbor")
		w.WriteHeader(http.StatusNotFound)
		body, _ := cborx.Marshal([]any{"E_NOT_FOUND", "", nil})
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	c, err := New(Config{BaseURL: srv.URL, HTTPClient: srv.Client(), InstanceID: testID(1), AllowLoopbackHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	c.discovered.Store(true)
	r, err := c.Runtime("runtime")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.ClaimSpotifyCode(context.Background(), testID(7)); !errors.Is(err, ErrSpotifyClaimNotFound) {
		t.Fatalf("got %v", err)
	}
}
