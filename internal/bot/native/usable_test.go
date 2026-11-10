package native

import (
	"errors"
	"fmt"
	"testing"

	"github.com/dilla-chat/dilla/internal/bot/helper"
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
