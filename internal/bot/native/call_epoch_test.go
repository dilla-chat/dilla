package native

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/dilla-chat/dilla/internal/bot/protocol"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
)

func TestCallMediaEpochUsesTypedCallAndValidatesBoundedResult(t *testing.T) {
	body, _ := cborx.Marshal([]any{uint64(3), []byte{1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}, uint64(2), []any{[]any{uint64(2), testID(8), testID(9)}}, "12345 23456 34567 45678 56789 67890"})
	f := &fakeTransport{responses: [][]any{{body}}}
	c := newFakeClient(t, f)
	got, err := c.CallMediaEpoch(context.Background(), testID(7))
	if err != nil {
		t.Fatal(err)
	}
	if f.calls[0] != protocol.HelperCallEpoch || got.Epoch != 3 || got.BaseKey[0] != 1 || len(got.Roster) != 1 || got.Roster[0].Leaf != 2 {
		t.Fatalf("unexpected call epoch: %#v, calls=%v", got, f.calls)
	}
	for _, format := range []string{"%v", "%+v", "%#v"} {
		if output := fmt.Sprintf(format, got); strings.Contains(output, "[1 0 0") {
			t.Fatalf("formatted call epoch leaked key bytes: %s", output)
		}
	}
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "AQAAAAAAAAAAAAAAAAAAAA") || !strings.Contains(string(encoded), "[redacted]") {
		t.Fatalf("JSON did not redact call key: %s", encoded)
	}

	bad, _ := cborx.Marshal([]any{uint64(3), []byte{1}, uint64(2), []any{}, "12345 23456 34567 45678 56789 67890"})
	f = &fakeTransport{responses: [][]any{{bad}}}
	c = newFakeClient(t, f)
	if _, err := c.CallMediaEpoch(context.Background(), testID(7)); err == nil {
		t.Fatal("malformed key shape accepted")
	}
}

func TestCallMediaEpochRejectsZeroCallIDWithoutDispatch(t *testing.T) {
	f := &fakeTransport{}
	c := newFakeClient(t, f)
	if _, err := c.CallMediaEpoch(context.Background(), id.Zero); err == nil {
		t.Fatal("zero call ID accepted")
	}
	if len(f.calls) != 0 {
		t.Fatalf("invalid call ID dispatched: %v", f.calls)
	}
}

func TestCallJoinCheckUsesTypedHelperAndRejectsZeroCallID(t *testing.T) {
	f := &fakeTransport{responses: [][]any{{}}}
	c := newFakeClient(t, f)
	if err := c.CallJoinCheck(context.Background(), testID(7)); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 1 || f.calls[0] != protocol.HelperCallJoinCheck {
		t.Fatalf("call join check dispatched as %v", f.calls)
	}
	f = &fakeTransport{}
	c = newFakeClient(t, f)
	if err := c.CallJoinCheck(context.Background(), id.Zero); err == nil {
		t.Fatal("zero call ID accepted")
	}
	if len(f.calls) != 0 {
		t.Fatalf("zero call ID dispatched: %v", f.calls)
	}
}

func TestCallGroupForgetUsesTypedHelperAndRejectsZeroCallID(t *testing.T) {
	f := &fakeTransport{responses: [][]any{{}}}
	c := newFakeClient(t, f)
	if err := c.CallGroupForget(context.Background(), testID(7)); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 1 || f.calls[0] != protocol.HelperCallGroupForget {
		t.Fatalf("call group forget dispatched as %v", f.calls)
	}
	f = &fakeTransport{}
	c = newFakeClient(t, f)
	if err := c.CallGroupForget(context.Background(), id.Zero); err == nil {
		t.Fatal("zero call ID accepted")
	}
	if len(f.calls) != 0 {
		t.Fatalf("zero call ID dispatched: %v", f.calls)
	}
}

func TestCallMediaEpochRejectsLeafOutsideSFrameSenderRange(t *testing.T) {
	body, _ := cborx.Marshal([]any{uint64(3), []byte{1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}, uint64(65536), []any{[]any{uint64(65536), testID(8), testID(9)}}, "12345 23456 34567 45678 56789 67890"})
	c := newFakeClient(t, &fakeTransport{responses: [][]any{{body}}})
	if _, err := c.CallMediaEpoch(context.Background(), testID(7)); err == nil {
		t.Fatal("leaf requiring uint16 truncation was accepted")
	}
}
