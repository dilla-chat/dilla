package service

import (
	"context"
	"errors"
	"testing"

	"github.com/dilla-chat/dilla/internal/bot/client"
	"github.com/dilla-chat/dilla/internal/bot/native"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
)

type fakeRemovalNative struct {
	binding   native.Binding
	group     native.GroupInfo
	cursor    uint64
	ingests   [][]native.RemovalFeedEntry
	refreshes int
}

func (n *fakeRemovalNative) Binding() native.Binding { return n.binding }
func (n *fakeRemovalNative) InspectGroup(context.Context, id.ID) (native.GroupInfo, error) {
	return n.group, nil
}
func (n *fakeRemovalNative) IngestRemovals(_ context.Context, _ id.ID, entries []native.RemovalFeedEntry) (uint64, error) {
	if len(entries) == 0 {
		return n.cursor, nil
	}
	if entries[0].Revision != n.cursor+1 {
		return 0, errors.New("fake native revision gap")
	}
	n.ingests = append(n.ingests, entries)
	n.cursor = entries[len(entries)-1].Revision
	return n.cursor, nil
}
func (n *fakeRemovalNative) RemovalActorHistoryVersion(context.Context, id.ID, native.RemovalFeedEntry) (uint64, error) {
	return 1, nil
}
func (n *fakeRemovalNative) RefreshRemovalActorHistory(_ context.Context, _ id.ID, _ native.RemovalFeedEntry, history, head []byte) (uint64, error) {
	if len(history) == 0 || len(head) == 0 {
		return 0, errors.New("empty actor history")
	}
	n.refreshes++
	return 1, nil
}

type fakeRemovalSession struct {
	pages map[uint64]client.ModerationRemovalPage
	reads []uint64
	head  []byte
}

func (s *fakeRemovalSession) ModerationRemovals(_ context.Context, _ id.ID, after, limit uint64) (client.ModerationRemovalPage, error) {
	if limit != removalSyncPageLimit {
		return client.ModerationRemovalPage{}, errors.New("unexpected page limit")
	}
	s.reads = append(s.reads, after)
	return s.pages[after], nil
}
func (s *fakeRemovalSession) OwnDeviceList(context.Context, id.ID) ([]byte, error) {
	return s.head, nil
}
func (s *fakeRemovalSession) DeviceListHistory(context.Context, id.ID, uint64) ([][]any, error) {
	return nil, errors.New("unexpected history page")
}

func removalProof(community, group, actor id.ID, sequence uint64) []byte {
	b, err := cborx.Marshal([]any{uint64(1), id.New(), actor, id.New(), community, group, sequence, uint64(50)})
	if err != nil {
		panic(err)
	}
	return b
}
func removalHead() []byte {
	b, err := cborx.Marshal([]any{uint64(1), []byte{0x80}, make([]byte, 64), make([]byte, 32)})
	if err != nil {
		panic(err)
	}
	return b
}

func TestRemovalSyncAppliesOrderedProofRowsAndPersistsCursor(t *testing.T) {
	community, group := id.New(), id.New()
	n := &fakeRemovalNative{binding: native.Binding{Instance: id.New(), Community: community, Installation: id.New()},
		group: native.GroupInfo{ID: group, Kind: 0, Community: &community, State: 2}, cursor: 5}
	s := &fakeRemovalSession{head: removalHead(), pages: map[uint64]client.ModerationRemovalPage{5: {Rows: []client.ModerationRemoval{
		{Revision: 6, Proof: removalProof(community, group, id.New(), 10), Signature: make([]byte, 64), AcceptedAt: 20},
		{Revision: 7, Proof: removalProof(community, group, id.New(), 11), Signature: make([]byte, 64), AcceptedAt: 21},
	}, NextRevision: 7}}}
	syncer, err := newRemovalSync(n)
	if err != nil {
		t.Fatal(err)
	}
	cursor, more, err := syncer.sync(t.Context(), s, group)
	if err != nil || cursor != 7 || more || len(n.ingests) != 1 || len(n.ingests[0]) != 2 || n.ingests[0][1].AcceptedAt != 21 || n.refreshes != 2 {
		t.Fatalf("sync = cursor %d more %v err %v ingests %v", cursor, more, err, n.ingests)
	}
	if len(s.reads) != 1 || s.reads[0] != 5 {
		t.Fatalf("feed reads = %v", s.reads)
	}
}

func TestRemovalSyncRejectsUnboundGroupAndGapBeforeIngest(t *testing.T) {
	community, group := id.New(), id.New()
	n := &fakeRemovalNative{binding: native.Binding{Instance: id.New(), Community: community, Installation: id.New()},
		group: native.GroupInfo{ID: group, Kind: 0, Community: &community, State: 2}}
	s := &fakeRemovalSession{pages: map[uint64]client.ModerationRemovalPage{0: {Rows: []client.ModerationRemoval{{Revision: 2}}, NextRevision: 2}}}
	syncer, err := newRemovalSync(n)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := syncer.sync(t.Context(), s, group); err == nil || len(n.ingests) != 0 {
		t.Fatalf("gap accepted: err=%v ingests=%v", err, n.ingests)
	}
	n.group.Community = nil
	if _, _, err := syncer.sync(t.Context(), s, group); err == nil || len(n.ingests) != 0 {
		t.Fatalf("unbound group accepted: err=%v ingests=%v", err, n.ingests)
	}
}

func TestRemovalSyncRefusesUntrustedActorHistoryBeforeIngest(t *testing.T) {
	community, group := id.New(), id.New()
	n := &fakeRemovalNative{binding: native.Binding{Instance: id.New(), Community: community, Installation: id.New()},
		group: native.GroupInfo{ID: group, Kind: 0, Community: &community, State: 2}}
	s := &fakeRemovalSession{pages: map[uint64]client.ModerationRemovalPage{0: {Rows: []client.ModerationRemoval{{
		Revision: 1, Proof: removalProof(community, group, id.New(), 4), Signature: make([]byte, 64), AcceptedAt: 20,
	}}, NextRevision: 1}}}
	syncer, err := newRemovalSync(n)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := syncer.sync(t.Context(), s, group); err == nil || len(n.ingests) != 0 || n.cursor != 0 {
		t.Fatalf("untrusted history reached ingest: err=%v ingests=%v cursor=%d", err, n.ingests, n.cursor)
	}
}
