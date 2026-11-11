package native

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/dilla-chat/dilla/internal/bot/helper"
	"github.com/dilla-chat/dilla/internal/bot/protocol"
)

// TestAClosedHelperIsNotUsable (Bot v2 Task 16, R2-3): a client whose helper reported closed, or that was closed, is
// not usable, so the next pass replaces it instead of reusing it; a transient failure leaves it usable.
func TestAClosedHelperIsNotUsable(t *testing.T) {
	f := &fakeTransport{err: errors.New("timeout")}
	c := newFakeClient(t, f)
	if _, err := c.Groups(t.Context()); err == nil || !c.Usable() {
		t.Fatalf("a transient failure: err %v usable %v; want usable", err, c.Usable())
	}
	f.err = fmt.Errorf("call: %w", helper.ErrClosed)
	if _, err := c.Groups(t.Context()); !errors.Is(err, helper.ErrClosed) || c.Usable() {
		t.Fatalf("a closed helper: err %v usable %v; want not usable", err, c.Usable())
	}
	closed := newFakeClient(t, &fakeTransport{})
	_ = closed.Close()
	if closed.Usable() {
		t.Fatal("a closed client is usable")
	}
	var none *Client
	if none.Usable() {
		t.Fatal("a nil client is usable")
	}
}

// abortingTransport is the helper client that aborts itself when a call's outcome is unknown, as
// helper.requestWithin does: from then on it reports Closed().
type abortingTransport struct {
	fakeTransport
	aborted bool
}

func (a *abortingTransport) Call(context.Context, protocol.HelperOperation, []any) ([]any, error) {
	a.aborted = true
	return nil, helper.ErrOutcomeUnknown
}
func (a *abortingTransport) GroupApply(ctx context.Context, payload []any) ([]any, error) {
	return a.Call(ctx, protocol.HelperGroupApply, payload)
}
func (a *abortingTransport) Closed() bool { return a.aborted }

// v0.1.0-bot2.2 fix round 1: a helper that aborted itself is not usable at once. Before, Usable() stayed true until a
// later call met ErrClosed, so a pass could keep and retain a dead helper.
func TestAnAbortedHelperIsNotUsableWithoutAnotherCall(t *testing.T) {
	a := &abortingTransport{}
	c, err := newClient(a, testBinding())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Groups(t.Context()); !errors.Is(err, helper.ErrOutcomeUnknown) {
		t.Fatalf("err = %v, want outcome unknown", err)
	}
	if c.Usable() {
		t.Fatal("a client whose helper aborted itself is still usable")
	}
}
