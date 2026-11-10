package native

import (
	"context"
	"testing"

	"github.com/dilla-chat/dilla/internal/bot/protocol"
)

// IngestDeviceList hands helper op 24 the pushed body unchanged and reads its [changed, version, state] answer
// (Bot v2 task 14): state 0 current, 1 held (a key change pending), 2 behind (the push does not chain to the cache).
func TestIngestDeviceListPassesThePushAndReadsTheState(t *testing.T) {
	user := testID(40)
	push := []byte{0x84, 1, 2, 3}
	for _, tc := range []struct {
		answer  []any
		state   DeviceListState
		version uint64
		changed bool
	}{
		{[]any{uint64(1), uint64(3), uint64(0)}, DeviceListCurrent, 3, true},
		{[]any{uint64(0), uint64(2), uint64(1)}, DeviceListHeld, 2, false},
		{[]any{uint64(0), uint64(1), uint64(2)}, DeviceListBehind, 1, false},
	} {
		f := &fakeTransport{responses: [][]any{tc.answer}}
		c := newFakeClient(t, f)
		got, err := c.IngestDeviceList(context.Background(), user, push)
		if err != nil {
			t.Fatalf("IngestDeviceList: %v", err)
		}
		if got.State != tc.state || got.Version != tc.version || got.Changed != tc.changed {
			t.Fatalf("result %+v, want state %d version %d changed %v", got, tc.state, tc.version, tc.changed)
		}
		if len(f.calls) != 1 || f.calls[0] != protocol.HelperDeviceListIngest {
			t.Fatalf("calls %v, want one op 24", f.calls)
		}
		p := f.callPayloads[0]
		if len(p) != 3 || p[0] != user || string(p[1].([]byte)) != string(push) {
			t.Fatalf("payload %v, want [user, push, now]", p)
		}
		if now, ok := p[2].(uint64); !ok || now == 0 {
			t.Fatalf("payload time %v", p[2])
		}
	}
	for _, bad := range [][]any{{uint64(2), uint64(1), uint64(0)}, {uint64(0), uint64(1), uint64(3)}, {uint64(0), uint64(1)}} {
		c := newFakeClient(t, &fakeTransport{responses: [][]any{bad}})
		if _, err := c.IngestDeviceList(context.Background(), user, push); err == nil {
			t.Fatalf("answer %v accepted", bad)
		}
	}
	c := newFakeClient(t, &fakeTransport{})
	if _, err := c.IngestDeviceList(context.Background(), user, nil); err == nil {
		t.Fatal("an empty push was sent")
	}
}
