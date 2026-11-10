//go:build linux

package main

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/auth"
	"github.com/dilla-chat/dilla/internal/bot/client"
	"github.com/dilla-chat/dilla/internal/gateway"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

type presenceTestStore struct{}

func (presenceTestStore) GroupsForDevice(context.Context, id.ID) ([]id.ID, error) { return nil, nil }
func (presenceTestStore) GetCursor(context.Context, id.ID, id.ID) (store.CursorRow, error) {
	return store.CursorRow{}, nil
}
func (presenceTestStore) CountKeyPackages(context.Context, id.ID, int64) (int64, error) {
	return 0, nil
}

type presenceTestAuth struct{ session auth.Session }

func (a presenceTestAuth) Resolve(context.Context, string) (auth.Session, error) {
	return a.session, nil
}
func (presenceTestAuth) RecordCapabilities(context.Context, []byte, uint64, uint64, uint64) error {
	return nil
}
func TestBotGatewayRuntimePublishesPresenceAndStopsOnRemoval(t *testing.T) {
	instance, device, user, installation := id.New(), id.New(), id.New(), id.New()
	gw := gateway.New(gateway.Options{InstanceID: instance, Generation: 1, WireVersions: []uint64{6}, E2EEVersions: []uint64{1}, MediaVersions: []uint64{1}, Store: presenceTestStore{}, Auth: presenceTestAuth{auth.Session{UserID: user, DeviceID: device, Scope: auth.ScopeEnrolled, TokenHash: make([]byte, 32)}}, TrustedOrigins: []string{"*"}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	server := httptest.NewServer(gw.Handler())
	defer server.Close()
	defer gw.Shutdown(context.Background())
	api, err := client.New(client.Config{BaseURL: server.URL, InstanceID: instance, AllowLoopbackHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	runtime := newBotGatewayRuntime(t.Context(), api, true, slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer runtime.Close()
	if err := runtime.Ensure(installation, id.New(), device, user, "session"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-runtime.Wake():
	case <-time.After(3 * time.Second):
		t.Fatal("gateway did not wake durable polling on Ready")
	}
	deadline := time.Now().Add(2 * time.Second)
	for gw.PresenceStatus(user, 0) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if gw.PresenceStatus(user, 0) == 0 {
		t.Fatal("bot remained offline")
	}
	runtime.RetainOnly(nil)
	deadline = time.Now().Add(2 * time.Second)
	for gw.PresenceStatus(user, 0) != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if gw.PresenceStatus(user, 0) != 0 {
		t.Fatal("removed installation retained gateway presence")
	}
}
