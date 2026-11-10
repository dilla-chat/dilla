package main

import (
	"errors"
	"fmt"
	"testing"

	"github.com/dilla-chat/dilla/internal/bot/client"
)

// A gateway that failed to start fails the installation's pass even when the socket routes it tried answered
// ErrNoGateway, which on its own is a deferred (transient) refusal.
func TestAGatewayStartFailureIsNotDeferred(t *testing.T) {
	noGateway := fmt.Errorf("welcome fetch: %w", client.ErrNoGateway)
	if !client.IsDeferred(noGateway) {
		t.Fatal("ErrNoGateway alone must stay deferred: the gateway stopped and the next Ensure has not run yet")
	}
	startup := errors.New("gatewayclient: invalid protocol response")
	joined := withGatewayStartup(noGateway, startup)
	if client.IsDeferred(joined) {
		t.Fatal("a failed gateway start joined with ErrNoGateway must count as failed, not deferred")
	}
	if !errors.Is(joined, startup) {
		t.Fatal("the startup error is lost")
	}
	if got := withGatewayStartup(noGateway, nil); !errors.Is(got, noGateway) {
		t.Fatal("no startup failure leaves the pass result alone")
	}
}
