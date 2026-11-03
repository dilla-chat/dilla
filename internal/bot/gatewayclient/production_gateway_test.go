package gatewayclient

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/auth"
	"github.com/dilla-chat/dilla/internal/bot/client"
	"github.com/dilla-chat/dilla/internal/gateway"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

type productionGatewayAuth struct{ session auth.Session }

var errTestSession = errors.New("unknown integration session")

func (a productionGatewayAuth) Resolve(_ context.Context, bearer string) (auth.Session, error) {
	if bearer != "integration-session" {
		return auth.Session{}, errTestSession
	}
	return a.session, nil
}
func (productionGatewayAuth) RecordCapabilities(context.Context, []byte, uint64, uint64, uint64) error {
	return nil
}

type productionGatewayStore struct{}

func (productionGatewayStore) GroupsForDevice(context.Context, id.ID) ([]id.ID, error) {
	return nil, nil
}
func (productionGatewayStore) GetCursor(context.Context, id.ID, id.ID) (store.CursorRow, error) {
	return store.CursorRow{}, nil
}
func (productionGatewayStore) CountKeyPackages(context.Context, id.ID, int64) (int64, error) {
	return 0, nil
}

// This crosses the public production Handler and Gateway. The peer is not a
// hand-built wire fixture: it runs the actual hello/identify/ready server path.
func TestProductionGatewayHandlerInteroperates(t *testing.T) {
	instance, device, user := testID(81), testID(82), testID(83)
	session := auth.Session{UserID: user, DeviceID: device, Scope: auth.ScopeEnrolled, TokenHash: bytes32(84)}
	gw := gateway.New(gateway.Options{
		InstanceID: instance, Generation: 19, WireVersions: []uint64{3}, E2EEVersions: []uint64{1}, MediaVersions: []uint64{1},
		Store: productionGatewayStore{}, Auth: productionGatewayAuth{session}, TrustedOrigins: []string{"*"},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), HeartbeatMS: 200,
	})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/gateway" {
			http.NotFound(w, r)
			return
		}
		gw.Handler().ServeHTTP(w, r)
	}))
	defer server.Close()
	defer func() { _ = gw.Shutdown(context.Background()) }()
	instanceID, _ := id.Parse(instance.String())
	httpClient, err := client.New(client.Config{BaseURL: server.URL, InstanceID: instanceID, AllowLoopbackHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	bot, err := New(Config{Client: httpClient, AllowLoopbackHTTP: true, ExpectedDeviceID: device, ExpectedUserID: user})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- bot.Run(ctx, func(context.Context) (string, error) { return "integration-session", nil })
	}()
	select {
	case delivery := <-bot.Events():
		if delivery.Frame.Op != OpReady || delivery.Frame.N != 1 || delivery.Frame.Ready.DeviceID != device || delivery.Frame.Ready.UserID != user {
			t.Fatalf("production ready frame = %+v", delivery.Frame)
		}
		if err := delivery.Ack(); err != nil {
			t.Fatalf("ack ready: %v", err)
		}
	case err := <-done:
		t.Fatalf("Run exited before production ready: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("production gateway did not complete identify/ready")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run after cancellation: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("production gateway run did not cancel")
	}
}

func TestProductionGatewayUnauthenticatedRefusalIsTypedTerminal(t *testing.T) {
	instance, device, user := testID(85), testID(86), testID(87)
	session := auth.Session{UserID: user, DeviceID: device, Scope: auth.ScopeEnrolled, TokenHash: bytes32(88)}
	gw := gateway.New(gateway.Options{
		InstanceID: instance, Generation: 20, WireVersions: []uint64{3}, E2EEVersions: []uint64{1}, MediaVersions: []uint64{1},
		Store: productionGatewayStore{}, Auth: productionGatewayAuth{session}, TrustedOrigins: []string{"*"},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { gw.Handler().ServeHTTP(w, r) }))
	defer server.Close()
	defer func() { _ = gw.Shutdown(context.Background()) }()
	instanceID, _ := id.Parse(instance.String())
	httpClient, err := client.New(client.Config{BaseURL: server.URL, InstanceID: instanceID, AllowLoopbackHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	bot, err := New(Config{Client: httpClient, AllowLoopbackHTTP: true, ExpectedDeviceID: device, ExpectedUserID: user})
	if err != nil {
		t.Fatal(err)
	}
	if err := bot.Run(context.Background(), func(context.Context) (string, error) { return "revoked-session", nil }); !errors.Is(err, ErrTerminal) {
		t.Fatalf("Run error=%v, want sanitized terminal reauthentication category", err)
	}
}
