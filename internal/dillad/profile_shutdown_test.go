package dillad_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/gateway"
)

func TestShutdownFlushesProfileHintsBeforeGatewayStops(t *testing.T) {
	srv, ts, token := newGreetedServer(t)
	rec := call(t, srv.Handler(), http.MethodPost, "/v1/communities", token, []any{"profiles", []byte("{}"), uint64(0), uint64(0)})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create community = %d: %s", rec.Code, rec.Body.String())
	}
	community := idOf(t, decodeArray(t, rec)[0])
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	conn := dialReady(t, ctx, ts, token)
	rec = call(t, socketHandler(srv), http.MethodPatch, "/v1/accounts/me", token, []any{"Updated", nil, nil})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("patch profile = %d: %s", rec.Code, rec.Body.String())
	}
	done := make(chan error, 1)
	go func() { done <- srv.Shutdown(ctx) }()
	sawHint := false
	for {
		op, payload := readFrame(t, ctx, conn)
		if op == uint64(gateway.OpCommunityChanged) {
			if len(payload) != 1 || idOf(t, payload[0]) != community {
				t.Fatalf("profile hint payload = %v", payload)
			}
			sawHint = true
		}
		if op == uint64(gateway.OpReconnect) {
			break
		}
	}
	if !sawHint {
		t.Fatal("gateway stopped before the pending profile hint was flushed")
	}
	if err := <-done; err != nil {
		t.Fatalf("shutdown = %v", err)
	}
}
