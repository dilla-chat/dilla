package native

import (
	"context"
	"testing"

	"github.com/dilla-chat/dilla/internal/bot/protocol"
	"github.com/dilla-chat/dilla/internal/cborx"
)

func TestSignRemovalUsesBoundHelperAndChecksReturnedProof(t *testing.T) {
	binding := testBinding()
	group, request := testID(43), testID(44)
	proof, err := cborx.Marshal([]any{uint64(1), request, testID(45), testID(46), binding.Community, group, uint64(7), uint64(130)})
	if err != nil {
		t.Fatal(err)
	}
	body, err := cborx.Marshal([]any{proof, make([]byte, 64)})
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeTransport{responses: [][]any{{body}}}
	c := newFakeClient(t, f)
	got, err := c.SignRemoval(context.Background(), request, group, 7, 130, 100)
	if err != nil || string(got) != string(body) || len(f.calls) != 1 || f.calls[0] != protocol.HelperSignRemoval {
		t.Fatalf("body=%x err=%v calls=%v", got, err, f.calls)
	}
}

func TestSignRemovalRejectsMalformedOrCrossCommunityResult(t *testing.T) {
	group, request := testID(47), testID(48)
	wrong, _ := cborx.Marshal([]any{uint64(1), request, testID(49), testID(50), testID(51), group, uint64(7), uint64(130)})
	bad, _ := cborx.Marshal([]any{wrong, make([]byte, 64)})
	f := &fakeTransport{responses: [][]any{{bad}}}
	c := newFakeClient(t, f)
	if _, err := c.SignRemoval(context.Background(), request, group, 7, 130, 100); err == nil {
		t.Fatal("accepted cross-community proof")
	}
	if _, err := c.SignRemoval(context.Background(), request, group, 7, 100, 100); err == nil || len(f.calls) != 1 {
		t.Fatal("invalid expiry reached helper")
	}
}
