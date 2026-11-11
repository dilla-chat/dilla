package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/dilla-chat/dilla/internal/bot/client"
	"github.com/dilla-chat/dilla/internal/bot/helper"
	"github.com/dilla-chat/dilla/internal/bot/native"
	"github.com/dilla-chat/dilla/internal/bot/service"
	"github.com/dilla-chat/dilla/internal/id"
)

func TestTextIntakeDeferredLogsClass(t *testing.T) {
	var out bytes.Buffer
	installation := id.New()
	r := &botGatewayRuntime{log: slog.New(slog.NewTextHandler(&out, nil)),
		pollers: map[id.ID]func(context.Context) error{installation: func(context.Context) error { return helper.ErrClosed }}}
	r.PollText(context.Background())
	line := out.String()
	if !strings.Contains(line, "bot text command intake deferred") || !strings.Contains(line, "error_class=closed") {
		t.Fatalf("log %q", line)
	}
}

// A refused event page defers the poll with its fixed detail token and nothing of the page.
func TestTextIntakeDeferredLogsDecodeDetail(t *testing.T) {
	var out bytes.Buffer
	installation := id.New()
	refused := native.NewDetailError(native.EventsCursorAboveHighWater, errors.New("event cursors"))
	r := &botGatewayRuntime{log: slog.New(slog.NewTextHandler(&out, nil)),
		pollers: map[id.ID]func(context.Context) error{installation: func(context.Context) error { return refused }}}
	r.PollText(context.Background())
	line := out.String()
	if !strings.Contains(line, "bot text command intake deferred") || !strings.Contains(line, "error_class=other") ||
		!strings.Contains(line, "detail=events.cursor_above_high_water") || strings.Contains(line, "event cursors") {
		t.Fatalf("log %q", line)
	}
}

type stageMembership struct{ err error }

func (m stageMembership) ReconcileOnce(context.Context, *client.Session) (service.MembershipResult, error) {
	return service.MembershipResult{}, m.err
}

type stageEvents struct{ err error }

func (e stageEvents) PollOnce(context.Context, service.GroupEventHandler) (service.GroupEventPollResult, error) {
	return service.GroupEventPollResult{}, e.err
}

// Each stage of the text poller names itself on the deferred line, so an operator can tell a transient membership or
// subscribe failure from a refused event page; a failure that only waits for the gateway is deferred=true at Info.
func TestTextIntakeDeferredLogsTheFailingStage(t *testing.T) {
	failed := errors.New("stage failed")
	for _, tc := range []struct {
		name       string
		stage      string
		membership error
		subscribe  error
		events     error
		level      string
		deferred   bool
	}{
		{name: "membership", stage: "membership", membership: failed, level: "level=WARN"},
		{name: "subscribe", stage: "subscribe", subscribe: failed, level: "level=WARN"},
		{name: "events", stage: "events", events: failed, level: "level=WARN"},
		{name: "subscribe no gateway", stage: "subscribe", subscribe: client.ErrNoGateway, level: "level=INFO", deferred: true},
		{name: "membership no gateway", stage: "membership", membership: fmt.Errorf("reconcile: %w", client.ErrNoGateway), level: "level=INFO", deferred: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			installation := id.New()
			subscribe := func(context.Context) error { return tc.subscribe }
			poll := newTextPoller(stageMembership{tc.membership}, nil, subscribe, stageEvents{tc.events})
			r := &botGatewayRuntime{log: slog.New(slog.NewTextHandler(&out, nil)),
				pollers: map[id.ID]func(context.Context) error{installation: poll}}
			r.PollText(context.Background())
			line := out.String()
			if strings.Count(line, "\n") != 1 || !strings.Contains(line, "bot text command intake deferred") ||
				!strings.Contains(line, "stage="+tc.stage) || !strings.Contains(line, tc.level) ||
				strings.Contains(line, "deferred=true") != tc.deferred || strings.Contains(line, "stage failed") {
				t.Fatalf("log %q", line)
			}
		})
	}
}
