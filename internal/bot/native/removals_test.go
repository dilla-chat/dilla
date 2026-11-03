package native

import (
	"context"
	"testing"

	"github.com/dilla-chat/dilla/internal/bot/protocol"
	"github.com/dilla-chat/dilla/internal/cborx"
)

func removalEntryForTest(t *testing.T, group, community [16]byte, revision uint64) RemovalFeedEntry {
	t.Helper()
	p := protocol.RemovalProof{
		RequestID: testID(21), ActorUser: testID(22), ActorDevice: testID(23),
		CommunityID: community, GroupID: group, TargetSequence: 7, Expires: 130,
	}
	b, err := cborx.Marshal([]any{uint64(1), p.RequestID, p.ActorUser, p.ActorDevice, p.CommunityID, p.GroupID, p.TargetSequence, p.Expires})
	if err != nil {
		t.Fatal(err)
	}
	return RemovalFeedEntry{Revision: revision, Proof: b, Signature: make([]byte, 64), AcceptedAt: 100}
}

func TestIngestRemovalsValidatesBoundProofAndUsesTypedOperation(t *testing.T) {
	binding := testBinding()
	group := testID(24)
	entry := removalEntryForTest(t, group, binding.Community, 3)
	f := &fakeTransport{responses: [][]any{{uint64(3)}}}
	c := newFakeClient(t, f)
	cursor, err := c.IngestRemovals(context.Background(), group, []RemovalFeedEntry{entry})
	if err != nil || cursor != 3 || len(f.calls) != 1 || f.calls[0] != protocol.HelperIngestRemovals {
		t.Fatalf("cursor=%d err=%v calls=%v", cursor, err, f.calls)
	}
}

func TestIngestRemovalsRejectsMalformedOrUnboundPagesBeforeDispatch(t *testing.T) {
	binding := testBinding()
	group := testID(25)
	valid := removalEntryForTest(t, group, binding.Community, 3)
	wrongGroup := removalEntryForTest(t, testID(26), binding.Community, 3)
	f := &fakeTransport{responses: [][]any{{uint64(0)}}}
	c := newFakeClient(t, f)
	for _, page := range [][]RemovalFeedEntry{
		{{Revision: 1, Proof: []byte{0x80}, Signature: make([]byte, 64), AcceptedAt: 1}},
		{wrongGroup},
		{valid, func() RemovalFeedEntry { e := valid; e.Revision = 5; return e }()},
	} {
		if _, err := c.IngestRemovals(context.Background(), group, page); err == nil {
			t.Fatalf("accepted invalid page %#v", page)
		}
	}
	if _, err := c.IngestRemovals(context.Background(), group, nil); err != nil {
		t.Fatalf("empty page: %v", err)
	}
	if len(f.calls) != 1 {
		t.Fatalf("invalid pages dispatched: %v", f.calls)
	}
}

func TestRemovalActorHistoryUsesNarrowOperationAndRejectsOversizedPayload(t *testing.T) {
	binding := testBinding()
	group := testID(27)
	entry := removalEntryForTest(t, group, binding.Community, 1)
	f := &fakeTransport{responses: [][]any{{uint64(4)}, {uint64(5)}}}
	c := newFakeClient(t, f)

	version, err := c.RemovalActorHistoryVersion(context.Background(), group, entry)
	if err != nil || version != 4 {
		t.Fatalf("cached version=%d err=%v", version, err)
	}
	version, err = c.RefreshRemovalActorHistory(context.Background(), group, entry, []byte{1, 2, 3}, []byte{4})
	if err != nil || version != 5 {
		t.Fatalf("refreshed version=%d err=%v", version, err)
	}
	if len(f.calls) != 2 || f.calls[0] != protocol.HelperRefreshRemovalActorHistory || f.calls[1] != protocol.HelperRefreshRemovalActorHistory {
		t.Fatalf("calls=%v", f.calls)
	}
	if _, err := c.RefreshRemovalActorHistory(context.Background(), group, entry, make([]byte, maxRemovalActorHistoryBytes), []byte{1}); err == nil {
		t.Fatal("accepted history exceeding the helper frame budget")
	}
	if len(f.calls) != 2 {
		t.Fatalf("oversized history reached helper: %v", f.calls)
	}
}
