package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/dilla-chat/dilla/internal/bot/helper"
	"github.com/dilla-chat/dilla/internal/bot/native"
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
	refused := &native.DetailError{Detail: native.EventsCursorAboveHighWater, Err: errors.New("event cursors")}
	r := &botGatewayRuntime{log: slog.New(slog.NewTextHandler(&out, nil)),
		pollers: map[id.ID]func(context.Context) error{installation: func(context.Context) error { return refused }}}
	r.PollText(context.Background())
	line := out.String()
	if !strings.Contains(line, "bot text command intake deferred") || !strings.Contains(line, "error_class=other") ||
		!strings.Contains(line, "detail=events.cursor_above_high_water") || strings.Contains(line, "event cursors") {
		t.Fatalf("log %q", line)
	}
}
