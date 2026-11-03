package service

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/dilla-chat/dilla/internal/bot/client"
	"github.com/dilla-chat/dilla/internal/bot/native"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
)

type fakeSourceSyncNative struct {
	binding                        native.Binding
	group                          native.GroupInfo
	applyThrough                   []uint64
	applyHandshakes, applyMessages [][][]any
	cursor                         *native.GroupCursor
	applyErr                       error
	cursorErr                      error
	ackErr                         error
	ackCalls                       int
}

func (n *fakeSourceSyncNative) Binding() native.Binding { return n.binding }
func (n *fakeSourceSyncNative) InspectGroup(_ context.Context, group id.ID) (native.GroupInfo, error) {
	if group != n.group.ID {
		return native.GroupInfo{}, errors.New("wrong group")
	}
	return n.group, nil
}
func (n *fakeSourceSyncNative) ApplyGroup(_ context.Context, _ id.ID, handshakes, messages []byte, through uint64) (native.GroupApplyResult, error) {
	if n.applyErr != nil {
		return native.GroupApplyResult{}, n.applyErr
	}
	var hs, ms [][]any
	if err := cborx.Unmarshal(handshakes, &hs); err != nil {
		return native.GroupApplyResult{}, err
	}
	if err := cborx.Unmarshal(messages, &ms); err != nil {
		return native.GroupApplyResult{}, err
	}
	n.applyThrough = append(n.applyThrough, through)
	n.applyHandshakes = append(n.applyHandshakes, hs)
	n.applyMessages = append(n.applyMessages, ms)
	n.group.NextSequence = through + 1
	n.cursor = &native.GroupCursor{Sequence: through, Epoch: 3, Body: []byte{0x82, 0x01, 0x03}}
	return native.GroupApplyResult{State: 2, NextSequence: through + 1}, nil
}
func (n *fakeSourceSyncNative) GroupCursor(context.Context, id.ID) (*native.GroupCursor, error) {
	if n.cursorErr != nil {
		return nil, n.cursorErr
	}
	return n.cursor, nil
}
func (n *fakeSourceSyncNative) AcknowledgeGroupCursor(context.Context, id.ID, native.GroupCursor) error {
	n.ackCalls++
	if n.ackErr != nil {
		return n.ackErr
	}
	n.cursor = nil
	return nil
}

type sourcePageKey struct {
	kind string
	from uint64
}
type fakeSourceSyncSession struct {
	info       client.GroupInfo
	pages      map[sourcePageKey]client.Page
	pageCalls  []sourcePageKey
	advanceErr error
	advances   [][2]uint64
	pageErrors map[sourcePageKey]error
	onPage     func(sourcePageKey)
}

func (s *fakeSourceSyncSession) GroupInfo(context.Context, id.ID) (client.GroupInfo, error) {
	return s.info, nil
}
func (s *fakeSourceSyncSession) BotInteractionGroupInfo(context.Context, id.ID, id.ID) (client.GroupInfo, error) {
	return s.info, nil
}
func (s *fakeSourceSyncSession) GroupPage(_ context.Context, _ id.ID, kind string, from, limit uint64) (client.Page, error) {
	k := sourcePageKey{kind, from}
	s.pageCalls = append(s.pageCalls, k)
	if err := s.pageErrors[k]; err != nil {
		return client.Page{}, err
	}
	if s.onPage != nil {
		s.onPage(k)
	}
	return s.pages[k], nil
}
func (s *fakeSourceSyncSession) BotInteractionGroupPage(ctx context.Context, _ id.ID, _ id.ID, kind string, from, limit uint64) (client.Page, error) {
	return s.GroupPage(ctx, id.ID{}, kind, from, limit)
}
func (s *fakeSourceSyncSession) AdvanceCursor(_ context.Context, _ id.ID, seq, epoch uint64) error {
	s.advances = append(s.advances, [2]uint64{seq, epoch})
	return s.advanceErr
}
func (s *fakeSourceSyncSession) AdvanceBotInteractionGroupCursor(ctx context.Context, _, _ id.ID, seq, epoch uint64) error {
	return s.AdvanceCursor(ctx, id.ID{}, seq, epoch)
}

func sourceGroupInfo(binding native.Binding, group, target id.ID, next uint64) native.GroupInfo {
	community := binding.Community
	return native.GroupInfo{ID: group, Kind: 0, Community: &community, Target: target, State: 2, NextSequence: next}
}
func sourceHandshake(seq uint64) []any {
	return []any{seq, uint64(0), uint64(1), nil, []byte{byte(seq)}}
}
func sourceMessage(seq uint64) []any {
	return []any{seq, uint64(0), sid(byte(seq)), []byte{byte(seq)}, nil, make([]byte, 32), uint64(1), uint64(0), nil}
}
func sourceExpected(group, community, target id.ID) native.GroupExpected {
	return native.GroupExpected{Group: group, Community: community, Channel: target, Kind: 0}
}

func TestSourceSyncUsesSharedFrontierWhenOneStreamIsShort(t *testing.T) {
	binding := native.Binding{Instance: sid(1), Community: sid(2), Installation: sid(3)}
	group, target := sid(4), sid(5)
	n := &fakeSourceSyncNative{binding: binding, group: sourceGroupInfo(binding, group, target, 1)}
	s := &fakeSourceSyncSession{info: client.GroupInfo{Epoch: 3, NextSequence: 6}, pages: map[sourcePageKey]client.Page{
		{kind: "handshakes", from: 1}: {Rows: [][]any{sourceHandshake(1), sourceHandshake(3)}, Next: 4},
		{kind: "messages", from: 1}:   {Rows: [][]any{sourceMessage(2)}, Next: 3},
	}}
	syncer, err := newSourceSync(n, sourceSyncOptions{PageLimit: 2, PageBudget: 1})
	if err != nil {
		t.Fatal(err)
	}
	got, err := syncer.sync(context.Background(), s, sourceExpected(group, binding.Community, target))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(n.applyThrough, []uint64{3}) {
		t.Fatalf("applied through %v, want shared frontier [3]", n.applyThrough)
	}
	if !reflect.DeepEqual(sourceSequences(n.applyHandshakes[0]), []uint64{1, 3}) || !reflect.DeepEqual(sourceSequences(n.applyMessages[0]), []uint64{2}) {
		t.Fatalf("applied rows hs=%v messages=%v", n.applyHandshakes[0], n.applyMessages[0])
	}
	if !got.More || got.AppliedThrough != 3 {
		t.Fatalf("result %#v, want bounded progress through 3 with more=true", got)
	}
}

func TestSourceSyncBothFullPagesUseTheEarlierStreamFrontier(t *testing.T) {
	binding := native.Binding{Instance: sid(1), Community: sid(2), Installation: sid(3)}
	group, target := sid(4), sid(5)
	n := &fakeSourceSyncNative{binding: binding, group: sourceGroupInfo(binding, group, target, 1)}
	s := &fakeSourceSyncSession{info: client.GroupInfo{Epoch: 3, NextSequence: 9}, pages: map[sourcePageKey]client.Page{
		{kind: "handshakes", from: 1}: {Rows: [][]any{sourceHandshake(1), sourceHandshake(3)}, Next: 4},
		{kind: "messages", from: 1}:   {Rows: [][]any{sourceMessage(2), sourceMessage(4)}, Next: 5},
	}}
	syncer, err := newSourceSync(n, sourceSyncOptions{PageLimit: 2, PageBudget: 1})
	if err != nil {
		t.Fatal(err)
	}
	got, err := syncer.sync(context.Background(), s, sourceExpected(group, binding.Community, target))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(n.applyThrough, []uint64{3}) || !reflect.DeepEqual(sourceSequences(n.applyMessages[0]), []uint64{2}) || !got.More {
		t.Fatalf("one full stream outran the other stream's frontier: result=%#v through=%v messages=%v", got, n.applyThrough, sourceSequences(n.applyMessages[0]))
	}
}

func sourceSequences(rows [][]any) []uint64 {
	sequences := make([]uint64, len(rows))
	for i, row := range rows {
		sequences[i], _ = row[0].(uint64)
	}
	return sequences
}

func TestSourceSyncDoesNotApplyWhenEitherReadFails(t *testing.T) {
	binding := native.Binding{Instance: sid(1), Community: sid(2), Installation: sid(3)}
	group, target := sid(4), sid(5)
	n := &fakeSourceSyncNative{binding: binding, group: sourceGroupInfo(binding, group, target, 1)}
	s := &fakeSourceSyncSession{info: client.GroupInfo{Epoch: 3, NextSequence: 3}, pages: map[sourcePageKey]client.Page{}, pageErrors: map[sourcePageKey]error{
		{kind: "messages", from: 1}: errors.New("read failed"),
	}}
	syncer, err := newSourceSync(n, sourceSyncOptions{PageLimit: 2, PageBudget: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = syncer.sync(context.Background(), s, sourceExpected(group, binding.Community, target)); err == nil {
		t.Fatal("read failure was ignored")
	}
	if len(n.applyThrough) != 0 || n.ackCalls != 0 {
		t.Fatalf("read failure mutated/acked native state: applies=%v acks=%d", n.applyThrough, n.ackCalls)
	}
}

func TestSourceSyncShortEmptyPagesAdvanceAcrossArchivalHoles(t *testing.T) {
	binding := native.Binding{Instance: sid(1), Community: sid(2), Installation: sid(3)}
	group, target := sid(4), sid(5)
	n := &fakeSourceSyncNative{binding: binding, group: sourceGroupInfo(binding, group, target, 1)}
	s := &fakeSourceSyncSession{info: client.GroupInfo{Epoch: 3, NextSequence: 6}, pages: map[sourcePageKey]client.Page{
		{kind: "handshakes", from: 1}: {Rows: nil, Next: 1}, {kind: "messages", from: 1}: {Rows: nil, Next: 1},
	}}
	syncer, err := newSourceSync(n, sourceSyncOptions{PageLimit: 2, PageBudget: 1})
	if err != nil {
		t.Fatal(err)
	}
	got, err := syncer.sync(context.Background(), s, sourceExpected(group, binding.Community, target))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(n.applyThrough, []uint64{5}) || got.More || got.AppliedThrough != 5 {
		t.Fatalf("empty archival gap did not advance from short pages: %#v %v", got, n.applyThrough)
	}
	if len(n.applyHandshakes[0]) != 0 || len(n.applyMessages[0]) != 0 {
		t.Fatal("rows appeared in empty sources")
	}
}

func TestSourceSyncRefusesWrongGroupBeforeReadingSources(t *testing.T) {
	binding := native.Binding{Instance: sid(1), Community: sid(2), Installation: sid(3)}
	group, target := sid(4), sid(5)
	n := &fakeSourceSyncNative{binding: binding, group: sourceGroupInfo(binding, group, target, 1)}
	s := &fakeSourceSyncSession{info: client.GroupInfo{Epoch: 3, NextSequence: 1}}
	syncer, err := newSourceSync(n, sourceSyncOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = syncer.sync(context.Background(), s, sourceExpected(group, binding.Community, sid(99))); err == nil {
		t.Fatal("mismatched group target accepted")
	}
	if len(s.pageCalls) != 0 || len(n.applyThrough) != 0 {
		t.Fatal("mismatched group reached source/apply")
	}
}

func TestSourceSyncReturnsTypedNativeGroupRecoveryStates(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state uint64
		want  error
	}{
		{name: "needs resync", state: 3, want: ErrSourceNeedsResync},
		{name: "gone", state: 4, want: ErrSourceGroupGone},
	} {
		t.Run(tc.name, func(t *testing.T) {
			binding := native.Binding{Instance: sid(1), Community: sid(2), Installation: sid(3)}
			group, target := sid(4), sid(5)
			info := sourceGroupInfo(binding, group, target, 1)
			info.State = tc.state
			n := &fakeSourceSyncNative{binding: binding, group: info}
			s := &fakeSourceSyncSession{info: client.GroupInfo{Epoch: 3, NextSequence: 1}}
			syncer, err := newSourceSync(n, sourceSyncOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = syncer.sync(context.Background(), s, sourceExpected(group, binding.Community, target)); !errors.Is(err, tc.want) {
				t.Fatalf("recovery state lost its type: got %v, want %v", err, tc.want)
			}
			if len(s.pageCalls) != 0 {
				t.Fatal("inactive group reached source reads")
			}
		})
	}
}

func TestSourceSyncUsesCapturedHeadAndPersistsCursorOnlyAfterServerAccepts(t *testing.T) {
	binding := native.Binding{Instance: sid(1), Community: sid(2), Installation: sid(3)}
	group, target := sid(4), sid(5)
	n := &fakeSourceSyncNative{binding: binding, group: sourceGroupInfo(binding, group, target, 1)}
	s := &fakeSourceSyncSession{info: client.GroupInfo{Epoch: 3, NextSequence: 4}, pages: map[sourcePageKey]client.Page{
		{kind: "handshakes", from: 1}: {Rows: [][]any{sourceHandshake(1), sourceHandshake(3), sourceHandshake(5)}, Next: 6},
		{kind: "messages", from: 1}:   {Rows: [][]any{sourceMessage(2)}, Next: 3},
	}}
	syncer, err := newSourceSync(n, sourceSyncOptions{PageLimit: 3, PageBudget: 1})
	if err != nil {
		t.Fatal(err)
	}
	got, err := syncer.sync(context.Background(), s, sourceExpected(group, binding.Community, target))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(n.applyThrough, []uint64{3}) || got.More {
		t.Fatalf("captured head/source frontier violated: %#v %#v", got, n.applyThrough)
	}
	if !reflect.DeepEqual(sourceSequences(n.applyHandshakes[0]), []uint64{1, 3}) {
		t.Fatalf("row above captured head was applied: %v", sourceSequences(n.applyHandshakes[0]))
	}
	if !reflect.DeepEqual(s.advances, [][2]uint64{{3, 3}}) || n.ackCalls != 1 {
		t.Fatalf("cursor order/ack mismatch: advances=%v native acks=%d", s.advances, n.ackCalls)
	}
}

func TestSourceSyncLeavesDurableCursorWhenServerCursorResponseIsLost(t *testing.T) {
	binding := native.Binding{Instance: sid(1), Community: sid(2), Installation: sid(3)}
	group, target := sid(4), sid(5)
	n := &fakeSourceSyncNative{binding: binding, group: sourceGroupInfo(binding, group, target, 1)}
	s := &fakeSourceSyncSession{info: client.GroupInfo{Epoch: 3, NextSequence: 2}, pages: map[sourcePageKey]client.Page{
		{kind: "handshakes", from: 1}: {Rows: [][]any{sourceHandshake(1)}, Next: 2}, {kind: "messages", from: 1}: {Rows: nil, Next: 1},
	}, advanceErr: errors.New("response lost after server accepted cursor")}
	syncer, err := newSourceSync(n, sourceSyncOptions{PageLimit: 2, PageBudget: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = syncer.sync(context.Background(), s, sourceExpected(group, binding.Community, target)); err == nil {
		t.Fatal("lost cursor response was treated as success")
	}
	if n.ackCalls != 0 || n.cursor == nil {
		t.Fatalf("native cursor acked despite unknown server outcome: calls=%d cursor=%#v", n.ackCalls, n.cursor)
	}
}

func TestSourceSyncReplaysServerAckAfterNativeAckResponseIsLost(t *testing.T) {
	binding := native.Binding{Instance: sid(1), Community: sid(2), Installation: sid(3)}
	group, target := sid(4), sid(5)
	n := &fakeSourceSyncNative{binding: binding, group: sourceGroupInfo(binding, group, target, 1), ackErr: errors.New("helper ack response lost")}
	s := &fakeSourceSyncSession{info: client.GroupInfo{Epoch: 3, NextSequence: 2}, pages: map[sourcePageKey]client.Page{
		{kind: "handshakes", from: 1}: {Rows: [][]any{sourceHandshake(1)}, Next: 2}, {kind: "messages", from: 1}: {Rows: nil, Next: 1},
	}}
	syncer, err := newSourceSync(n, sourceSyncOptions{PageLimit: 2, PageBudget: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = syncer.sync(context.Background(), s, sourceExpected(group, binding.Community, target)); err == nil {
		t.Fatal("native acknowledgement loss was treated as success")
	}
	if n.cursor == nil {
		t.Fatal("native cursor was not retained after uncertain ack")
	}
	if len(s.advances) != 1 || n.ackCalls != 1 {
		t.Fatalf("first ack ordering: server=%v native=%d", s.advances, n.ackCalls)
	}
	n.ackErr = nil
	if _, err = syncer.sync(context.Background(), s, sourceExpected(group, binding.Community, target)); err != nil {
		t.Fatal(err)
	}
	if len(s.advances) != 2 || s.advances[0] != s.advances[1] {
		t.Fatalf("native ack retry did not repeat exact server cursor: %v", s.advances)
	}
}

func TestSourceSyncCancellationBetweenStreamsDoesNotApply(t *testing.T) {
	binding := native.Binding{Instance: sid(1), Community: sid(2), Installation: sid(3)}
	group, target := sid(4), sid(5)
	n := &fakeSourceSyncNative{binding: binding, group: sourceGroupInfo(binding, group, target, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	s := &fakeSourceSyncSession{info: client.GroupInfo{Epoch: 3, NextSequence: 2}, pages: map[sourcePageKey]client.Page{
		{kind: "handshakes", from: 1}: {Rows: [][]any{sourceHandshake(1)}, Next: 2},
	}, onPage: func(key sourcePageKey) {
		if key.kind == "handshakes" {
			cancel()
		}
	}}
	syncer, err := newSourceSync(n, sourceSyncOptions{PageLimit: 2, PageBudget: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = syncer.sync(ctx, s, sourceExpected(group, binding.Community, target)); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation result: %v", err)
	}
	if len(s.pageCalls) != 1 || len(n.applyThrough) != 0 {
		t.Fatalf("canceled between streams performed more work: pages=%v applies=%v", s.pageCalls, n.applyThrough)
	}
}

type generatedSourceSession struct {
	group id.ID
	info  client.GroupInfo
}

func (s generatedSourceSession) GroupInfo(_ context.Context, group id.ID) (client.GroupInfo, error) {
	if group != s.group {
		return client.GroupInfo{}, errors.New("wrong group")
	}
	return s.info, nil
}
func (s generatedSourceSession) GroupPage(_ context.Context, group id.ID, kind string, from, limit uint64) (client.Page, error) {
	if group != s.group {
		return client.Page{}, errors.New("wrong group")
	}
	page := client.Page{Rows: make([][]any, 0, limit), Next: from}
	for seq := from; seq < s.info.NextSequence && uint64(len(page.Rows)) < limit; seq++ {
		if kind == "handshakes" && seq%2 == 0 || kind == "messages" && seq%2 == 1 {
			continue
		}
		if kind == "handshakes" {
			page.Rows = append(page.Rows, sourceHandshake(seq))
		} else {
			page.Rows = append(page.Rows, sourceMessage(seq))
		}
		page.Next = seq + 1
	}
	return page, nil
}
func (generatedSourceSession) AdvanceCursor(context.Context, id.ID, uint64, uint64) error { return nil }

func TestSourceSyncProcessesLargeBacklogAcrossBoundedPasses(t *testing.T) {
	binding := native.Binding{Instance: sid(1), Community: sid(2), Installation: sid(3)}
	group, target := sid(4), sid(5)
	const head = uint64(12000)
	n := &fakeSourceSyncNative{binding: binding, group: sourceGroupInfo(binding, group, target, 1)}
	s := generatedSourceSession{group: group, info: client.GroupInfo{Epoch: 3, NextSequence: head + 1}}
	syncer, err := newSourceSync(n, sourceSyncOptions{PageLimit: 256, PageBudget: 2})
	if err != nil {
		t.Fatal(err)
	}
	var rows, passes uint64
	for {
		got, syncErr := syncer.sync(context.Background(), s, sourceExpected(group, binding.Community, target))
		if syncErr != nil {
			t.Fatal(syncErr)
		}
		if got.Pages > 2 {
			t.Fatalf("pass exceeded paired page budget: %d", got.Pages)
		}
		if got.Pages == 0 && got.More {
			t.Fatal("sync reported more work without durable progress")
		}
		rows += got.Handshakes + got.Messages
		passes++
		if !got.More {
			break
		}
		if passes > 100 {
			t.Fatal("bounded passes failed to drain a stable backlog")
		}
	}
	if rows != head || passes < 2 {
		t.Fatalf("large backlog accounting: rows=%d passes=%d", rows, passes)
	}
	if n.group.NextSequence != head+1 {
		t.Fatalf("durable next sequence=%d, want %d", n.group.NextSequence, head+1)
	}
}
