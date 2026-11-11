package main

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/dilla-chat/dilla/internal/bot/helper"
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
