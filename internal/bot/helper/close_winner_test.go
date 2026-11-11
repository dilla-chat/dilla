package helper

import (
	"bytes"
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/bot/protocol"
	"github.com/dilla-chat/dilla/internal/id"
)

func shutdownFails(op protocol.HelperOperation) (protocol.HelperResponse, error) {
	return protocol.HelperResponse{}, unknown(os.ErrDeadlineExceeded)
}

// The caller's reason wins even when the shutdown exchange itself fails with an unknown outcome.
func TestCloseReasonWinsOverAFailedShutdown(t *testing.T) {
	for name, closeIt := range map[string]func(*Client) error{
		"reason": func(c *Client) error { return c.CloseWithReason(ReasonPassUnready) },
		"detailed": func(c *Client) error {
			return c.CloseDetailed(ReasonPassUnready, protocol.HelperBootstrap, "result_decode")
		},
	} {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			c := newLoggedClient(&out, &okExchange{handler: shutdownFails}, time.Second)
			if err := closeIt(c); err != nil {
				t.Fatal(err)
			}
			line := out.String()
			for _, want := range []string{"reason=pass_unready", "shutdown_error_class=timeout"} {
				if !strings.Contains(line, want) {
					t.Fatalf("log %q lacks %q", line, want)
				}
			}
			if strings.Contains(line, "outcome_unknown") || strings.Count(line, "\n") != 1 {
				t.Fatalf("want exactly one line without outcome_unknown, got %q", line)
			}
		})
	}
}

func TestCloseDetailedInvalidatedDecodeSurvivesFailedShutdown(t *testing.T) {
	var out bytes.Buffer
	c := newLoggedClient(&out, &okExchange{handler: shutdownFails}, time.Second)
	_ = c.CloseDetailed(ReasonInvalidatedDecode, protocol.HelperBootstrap, "result_decode")
	line := out.String()
	for _, want := range []string{"reason=invalidated_decode", "operation=2", "kind=result_decode", "shutdown_error_class=timeout"} {
		if !strings.Contains(line, want) {
			t.Fatalf("log %q lacks %q", line, want)
		}
	}
	if strings.Count(line, "\n") != 1 {
		t.Fatalf("want one line, got %q", line)
	}
}

// A close a group sub-op caused names that sub-op on the one close line, beside operation=group_apply.
func TestCloseDetailedCarriesTheGroupSubOp(t *testing.T) {
	var out bytes.Buffer
	c := newLoggedClient(&out, &okExchange{handler: shutdownFails}, time.Second)
	_ = c.CloseDetailed(ReasonInvalidatedDecode, protocol.HelperGroupApply, "result_decode", "sub_op", uint64(18))
	line := out.String()
	for _, want := range []string{"reason=invalidated_decode", "operation=group_apply", "kind=result_decode", "sub_op=18"} {
		if !strings.Contains(line, want) {
			t.Fatalf("log %q lacks %q", line, want)
		}
	}
	if strings.Count(line, "\n") != 1 {
		t.Fatalf("want one line, got %q", line)
	}
}

// wakeExchange blocks a call until the helper is stopped, then fails it as a dead helper would.
type wakeExchange struct {
	once sync.Once
	dead chan struct{}
}

func (w *wakeExchange) request(ctx context.Context, _ id.ID, _ protocol.HelperOperation, _ []any, _ time.Duration) (protocol.HelperResponse, error) {
	<-w.dead
	return protocol.HelperResponse{}, unknown(os.ErrClosed)
}
func (w *wakeExchange) stop(time.Duration) error { w.once.Do(func() { close(w.dead) }); return nil }
func (w *wakeExchange) exit() string             { return "killed" }

// The abort that ends a timed-out Close wakes the in-flight call; the close line must still say shutdown_timeout.
func TestCloseTimeoutReasonBeatsTheWokenCall(t *testing.T) {
	var out bytes.Buffer
	c := newLoggedClient(&out, &wakeExchange{dead: make(chan struct{})}, 20*time.Millisecond)
	done := make(chan struct{})
	go func() {
		_, _ = c.Call(context.Background(), protocol.HelperStateGet, []any{uint64(0)})
		close(done)
	}()
	time.Sleep(10 * time.Millisecond) // the call holds the gate
	if err := c.CloseWithReason(ReasonPassUnready); err != nil {
		t.Fatal(err)
	}
	<-done
	line := out.String()
	if !strings.Contains(line, "reason=shutdown_timeout") || strings.Count(line, "\n") != 1 {
		t.Fatalf("want one shutdown_timeout line, got %q", line)
	}
}

// digestExchange serves a staged transfer whose result digest never matches.
type digestExchange struct{}

func (digestExchange) request(_ context.Context, _ id.ID, _ protocol.HelperOperation, payload []any, _ time.Duration) (protocol.HelperResponse, error) {
	switch payload[0] {
	case uint64(0):
		tid := make([]byte, 16)
		tid[0] = 1
		return protocol.HelperResponse{Payload: []any{tid}}, nil
	case uint64(1):
		return protocol.HelperResponse{Payload: []any{payload[2].(uint64) + uint64(len(payload[3].([]byte)))}}, nil
	case uint64(2):
		return protocol.HelperResponse{Payload: []any{uint64(0), uint64(4), make([]byte, 32)}}, nil
	case uint64(3):
		return protocol.HelperResponse{Payload: []any{[]byte("abcd")}}, nil
	}
	return protocol.HelperResponse{}, nil
}
func (digestExchange) stop(time.Duration) error { return nil }
func (digestExchange) exit() string             { return "exit status 0" }

func TestTransferDigestMismatchLogsKind(t *testing.T) {
	var out bytes.Buffer
	c := newLoggedClient(&out, digestExchange{}, time.Second)
	if _, err := c.groupApply(context.Background(), []any{uint64(0), []byte("small payload")}); err == nil {
		t.Fatal("digest mismatch accepted")
	}
	line := out.String()
	for _, want := range []string{"reason=transfer_invalid", "operation=group_apply", "kind=digest_mismatch"} {
		if !strings.Contains(line, want) {
			t.Fatalf("log %q lacks %q", line, want)
		}
	}
	if strings.Count(line, "\n") != 1 {
		t.Fatalf("want one line, got %q", line)
	}
}
