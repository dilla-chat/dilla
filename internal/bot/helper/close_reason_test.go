package helper

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/bot/protocol"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/prioritygate"
)

// okExchange answers every request with an empty success, or with what handler says.
type okExchange struct {
	handler func(op protocol.HelperOperation) (protocol.HelperResponse, error)
}

func (o *okExchange) request(_ context.Context, _ id.ID, op protocol.HelperOperation, _ []any, _ time.Duration) (protocol.HelperResponse, error) {
	if o.handler != nil {
		return o.handler(op)
	}
	return protocol.HelperResponse{}, nil
}
func (o *okExchange) stop(time.Duration) error { return nil }
func (o *okExchange) exit() string             { return "exit status 0" }

func newLoggedClient(out *bytes.Buffer, x exchange, stop time.Duration) *Client {
	return &Client{gate: prioritygate.New(1), transport: x, callTime: time.Second, openTime: time.Second, stopTime: stop, opened: true,
		log: slog.New(slog.NewTextHandler(out, nil))}
}

func TestCloseLogsReasonOnce(t *testing.T) {
	var out bytes.Buffer
	c := newLoggedClient(&out, &okExchange{}, time.Second)
	if err := c.CloseWithReason(ReasonPassUnready); err != nil {
		t.Fatal(err)
	}
	if err := c.CloseWithReason(ReasonMusicRestoreFailed); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	line := out.String()
	if strings.Count(line, "bot native helper closed") != 1 || !strings.Contains(line, "reason=pass_unready") || strings.Count(line, "\n") != 1 {
		t.Fatalf("want one closed line with reason=pass_unready, got %q", line)
	}
}

func TestPlainCloseLogsShutdown(t *testing.T) {
	var out bytes.Buffer
	c := newLoggedClient(&out, &okExchange{}, time.Second)
	_ = c.Close()
	if !strings.Contains(out.String(), "reason=shutdown") {
		t.Fatalf("log %q lacks reason=shutdown", out.String())
	}
}

func TestTransferInvalidLogsClose(t *testing.T) {
	var out bytes.Buffer
	x := &okExchange{handler: func(protocol.HelperOperation) (protocol.HelperResponse, error) {
		// A begin answer without a transfer id breaks the staged transfer.
		return protocol.HelperResponse{Payload: []any{}}, nil
	}}
	c := newLoggedClient(&out, x, time.Second)
	if _, err := c.groupApply(context.Background(), []any{uint64(0), []byte("small payload")}); err == nil {
		t.Fatal("invalid transfer accepted")
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	line := out.String()
	for _, want := range []string{"bot native helper closed", "reason=transfer_invalid", "operation=group_apply", "kind=begin_response", "error_class=outcome_unknown"} {
		if !strings.Contains(line, want) {
			t.Fatalf("log %q lacks %q", line, want)
		}
	}
	if strings.Count(line, "\n") != 1 {
		t.Fatalf("want exactly one line, got %q", line)
	}
}

// A call that holds the gate past the shutdown bound makes Close give up and abort; that is its own reason.
func TestCloseTimeoutLogsShutdownTimeout(t *testing.T) {
	var out bytes.Buffer
	c := newLoggedClient(&out, &okExchange{}, 20*time.Millisecond)
	release, err := c.gate.Acquire(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if err := c.CloseWithReason(ReasonPassUnready); err != nil {
		t.Fatal(err)
	}
	line := out.String()
	if !strings.Contains(line, "reason=shutdown_timeout") || strings.Count(line, "\n") != 1 {
		t.Fatalf("want one shutdown_timeout line, got %q", line)
	}
}
