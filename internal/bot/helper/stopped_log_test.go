package helper

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/bot/protocol"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/prioritygate"
)

type diedExchange struct{ stopped bool }

func (d *diedExchange) request(context.Context, id.ID, protocol.HelperOperation, []any, time.Duration) (protocol.HelperResponse, error) {
	return protocol.HelperResponse{}, unknown(io.EOF)
}
func (d *diedExchange) stop(time.Duration) error { d.stopped = true; return nil }
func (d *diedExchange) exit() string             { return "exit status 101" }

// A helper that dies must leave its cause in the journal: on v0.1.0-bot2 it left only "helper client is closed".
func TestAStoppedHelperLogsTheOperationClassAndExitOnce(t *testing.T) {
	var out bytes.Buffer
	d := &diedExchange{}
	c := &Client{gate: prioritygate.New(1), transport: d, callTime: time.Second, openTime: time.Second, opened: true,
		log: slog.New(slog.NewTextHandler(&out, nil))}
	if _, err := c.request(context.Background(), protocol.HelperOperation(11), []any{"secret-payload"}); err == nil {
		t.Fatal("a dead helper answered")
	}
	line := out.String()
	for _, want := range []string{"bot native helper stopped", "operation=11", "error_class=helper_exited", `exit="exit status 101"`} {
		if !strings.Contains(line, want) {
			t.Fatalf("log line %q lacks %q", line, want)
		}
	}
	if strings.Contains(line, "secret-payload") || !d.stopped {
		t.Fatalf("payload leaked or helper not stopped: %q", line)
	}
	if strings.Count(line, "\n") != 1 {
		t.Fatalf("want exactly one line, got %q", line)
	}
}
