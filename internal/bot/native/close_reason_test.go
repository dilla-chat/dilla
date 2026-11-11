package native

import (
	"context"
	"errors"
	"testing"

	"github.com/dilla-chat/dilla/internal/bot/helper"
	"github.com/dilla-chat/dilla/internal/bot/protocol"
	"github.com/dilla-chat/dilla/internal/cborx"
)

// reasonTransport is a transport that records the reason its close was given, as helper.Client does.
type reasonTransport struct {
	fakeTransport
	reasons []string
	ops     []protocol.HelperOperation
	kinds   []string
	extras  [][]any
}

func (r *reasonTransport) CloseWithReason(reason string) error {
	r.reasons = append(r.reasons, reason)
	return nil
}
func (r *reasonTransport) CloseDetailed(reason string, op protocol.HelperOperation, kind string, extra ...any) error {
	r.reasons = append(r.reasons, reason)
	r.extras = append(r.extras, extra)
	r.ops = append(r.ops, op)
	r.kinds = append(r.kinds, kind)
	return nil
}

func TestInvalidateOnDecodeLogsClose(t *testing.T) {
	wrapper, err := cborx.Marshal([]any{[]byte{0x80}})
	if err != nil {
		t.Fatal(err)
	}
	rt := &reasonTransport{fakeTransport: fakeTransport{responses: [][]any{{wrapper}}}}
	c, err := newClient(rt, testBinding())
	if err != nil {
		t.Fatal(err)
	}
	var key [32]byte
	key[0] = 1
	if _, err = c.Bootstrap(context.Background(), key); !errors.Is(err, ErrOutcomeUnknown) {
		t.Fatalf("malformed mutation: %v", err)
	}
	if len(rt.reasons) != 1 || rt.reasons[0] != helper.ReasonInvalidatedDecode || rt.ops[0] != protocol.HelperBootstrap || rt.kinds[0] != "result_decode" {
		t.Fatalf("close carried reasons=%v ops=%v kinds=%v", rt.reasons, rt.ops, rt.kinds)
	}
}

func TestCloseWithReasonPassesTheReasonDown(t *testing.T) {
	rt := &reasonTransport{}
	c, err := newClient(rt, testBinding())
	if err != nil {
		t.Fatal(err)
	}
	_ = c.CloseWithReason(helper.ReasonPassUnready)
	_ = c.Close()
	if len(rt.reasons) != 1 || rt.reasons[0] != helper.ReasonPassUnready {
		t.Fatalf("reasons = %v, want one pass_unready", rt.reasons)
	}
}
