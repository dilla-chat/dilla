package native

import (
	"context"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/bot/helper"
	"github.com/dilla-chat/dilla/internal/bot/protocol"
)

// Bot v2 Task 15: ops 21, 22 and 23 through the installation's native helper.
func TestCommandOpsCallTheHelperAndDecode(t *testing.T) {
	key := make([]byte, 32)
	key[0] = 0xaa
	request := testID(41)
	f := &fakeTransport{responses: [][]any{{[]byte{0x87, 1}}, {uint64(8), key, request[:]}, {[]byte{0x83, 2}}, {uint64(2), key}}}
	c := newFakeClient(t, f)
	ctx := context.Background()
	now := time.Unix(1_800_000_000, 0)
	if pub, err := c.CommandKey(ctx, testID(2), now); err != nil || string(pub) != "\x87\x01" {
		t.Fatalf("CommandKey = %x %v", pub, err)
	}
	opened, err := c.OpenCommand(ctx, []byte{0x85}, testID(5), testID(6), now)
	if err != nil || opened.Refusal != helper.RefusalReplay || opened.ReplyKey[0] != 0xaa || opened.ReplayRequestID != request {
		t.Fatalf("OpenCommand = %+v %v", opened, err)
	}
	var reply [32]byte
	reply[0] = 1
	if sealed, err := c.SealReply(ctx, reply, testID(9), 3, nil); err != nil || string(sealed) != "\x83\x02" {
		t.Fatalf("SealReply = %x %v", sealed, err)
	}
	if _, err := c.OpenCommand(ctx, []byte{0x85}, testID(5), testID(6), now); err == nil {
		t.Fatal("an unknown key with a reply key was accepted")
	}
	want := []protocol.HelperOperation{protocol.HelperCommandKey, protocol.HelperCommandOpen, protocol.HelperReplySeal, protocol.HelperCommandOpen}
	for i, op := range want {
		if f.calls[i] != op {
			t.Fatalf("calls = %v, want %v", f.calls, want)
		}
	}
	if p := f.callPayloads[2]; len(p) != 4 || string(p[0].([]byte)) != string(reply[:]) || p[1] != testID(9) || p[2] != uint64(3) || len(p[3].([]byte)) != 0 {
		t.Fatalf("seal payload = %v", p)
	}
}
