package service

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jonasthim/dilla/internal/bot/helper"
	"github.com/jonasthim/dilla/internal/bot/native"
	"github.com/jonasthim/dilla/internal/bot/polls"
	"github.com/jonasthim/dilla/internal/bot/protocol"
	"github.com/jonasthim/dilla/internal/cborx"
	"github.com/jonasthim/dilla/internal/id"
)

type pollTestState struct {
	mu    sync.Mutex
	rows  map[string]native.StateEntry
	txErr error
}

func (s *pollTestState) StateGet(_ context.Context, _ uint64, key []byte) (native.StateValue, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.rows[string(key)]
	if !ok {
		return native.StateValue{}, nil
	}
	return native.StateValue{Revision: e.Revision, Value: e.Value, Tombstone: e.Tombstone}, nil
}
func (s *pollTestState) StateScan(_ context.Context, _ uint64, after []byte, limit uint64) (native.StatePage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	keys := make([]string, 0, len(s.rows))
	for k := range s.rows {
		if len(after) == 0 || k > string(after) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	if len(keys) > int(limit) {
		keys = keys[:limit]
	}
	p := native.StatePage{}
	for _, k := range keys {
		p.Entries = append(p.Entries, s.rows[k])
	}
	if len(keys) > 0 {
		p.Next = []byte(keys[len(keys)-1])
	}
	return p, nil
}
func (s *pollTestState) StateTransaction(_ context.Context, _ id.ID, changes []native.StateChange) ([]uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rows == nil {
		s.rows = map[string]native.StateEntry{}
	}
	if s.txErr != nil {
		e := s.txErr
		s.txErr = nil
		return nil, e
	}
	for _, c := range changes {
		if s.rows[string(c.Key)].Revision != c.ExpectedRevision {
			return nil, polls.ErrConflict
		}
	}
	r := make([]uint64, len(changes))
	for i, c := range changes {
		e := s.rows[string(c.Key)]
		e.Key = c.Key
		e.Revision++
		e.Value = c.Value
		s.rows[string(c.Key)] = e
		r[i] = e.Revision
	}
	return r, nil
}

type pollAuthorityFake struct {
	bits   uint64
	member bool
	calls  int
}

func (a *pollAuthorityFake) PollPermissions(context.Context, id.ID, id.ID) (uint64, error) {
	a.calls++
	return a.bits, nil
}
func (a *pollAuthorityFake) PollMember(context.Context, id.ID) (bool, error) { return a.member, nil }

type pollPublisherFake struct {
	calls int
	err   error
}

func (p *pollPublisherFake) PublishPoll(context.Context, polls.Poll) error { p.calls++; return p.err }
func TestPollPermissionIsRefreshedForEveryRead(t *testing.T) {
	a := &pollAuthorityFake{bits: 1 | 64, member: true}
	store := polls.NewStore(&pollTestState{}, pollServiceID(1))
	now := time.Unix(1000, 0)
	pollID := pollServiceID(3)
	_, err := store.Create(context.Background(), pollID, [32]byte{1}, polls.Poll{ID: pollID, CommunityID: pollServiceID(1), ChannelID: pollServiceID(4), CreatorID: pollServiceID(2), Question: "Q", Answers: []string{"A", "B"}, CreatedAt: 1000, DueAt: 4600}, now)
	if err != nil {
		t.Fatal(err)
	}
	h := PollInteractions{Store: store, Authority: a, CommunityID: pollServiceID(1), Now: func() time.Time { return time.Unix(1000, 0) }}
	actor := InteractionActor{User: pollServiceID(2), Kind: 0}
	r := protocol.Request{CommunityID: pollServiceID(1), Operation: protocol.OpPollGet, Args: []any{pollID}}
	got, _ := h.get(context.Background(), actor, r)
	if got.Status != 0 {
		t.Fatalf("authorized read status=%d", got.Status)
	}
	if a.calls != 1 {
		t.Fatalf("permission checks=%d want 1", a.calls)
	}
	a.bits = 0
	got, _ = h.get(context.Background(), actor, r)
	if got.Status != interactionStatusForbidden {
		t.Fatalf("revoked read status=%d", got.Status)
	}
	if a.calls != 2 {
		t.Fatalf("permission checks after revocation=%d want 2", a.calls)
	}
}

func TestPollVotePreEpochReturnsPrivateStatusAndPreservesExactReplay(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(-1, 0)
	state := &pollTestState{rows: map[string]native.StateEntry{}}
	store := polls.NewStore(state, pollServiceID(1))
	pollID := pollServiceID(3)
	p := polls.Poll{ID: pollID, CommunityID: pollServiceID(1), ChannelID: pollServiceID(4), CreatorID: pollServiceID(2), Question: "Q", Answers: []string{"A", "B"}, CreatedAt: 1000, DueAt: 4600, Published: true}
	if _, err := store.Create(ctx, p.ID, [32]byte{1}, p, time.Unix(1000, 0)); err != nil {
		t.Fatal(err)
	}
	authority := &pollAuthorityFake{bits: pollPermView | pollPermSend, member: true}
	h := PollInteractions{Store: store, Authority: authority, CommunityID: pollServiceID(1), Now: func() time.Time { return now }}
	choices, _ := cborx.Marshal([]any{uint64(1)})
	revision, expires := uint64(1), uint64(1000)
	r := protocol.Request{RequestID: pollServiceID(8), CommunityID: pollServiceID(1), Operation: protocol.OpPollVote, Revision: &revision, Expires: &expires, Args: []any{pollID, choices}}
	snapshot := func() map[string]native.StateEntry {
		state.mu.Lock()
		defer state.mu.Unlock()
		out := make(map[string]native.StateEntry, len(state.rows))
		for key, entry := range state.rows {
			entry.Key = append([]byte(nil), entry.Key...)
			entry.Value = append([]byte(nil), entry.Value...)
			out[key] = entry
		}
		return out
	}
	before := snapshot()
	result, err := h.vote(ctx, InteractionActor{User: pollServiceID(6), Kind: 0}, r)
	if err != nil || result.Status != interactionStatusUnavailable {
		t.Fatalf("pre-epoch vote result=%+v err=%v, want private status 5", result, err)
	}
	if !reflect.DeepEqual(before, snapshot()) {
		t.Fatal("pre-epoch vote handler changed poll or receipt state")
	}
	now = time.Unix(1000, 0)
	accepted, err := h.vote(ctx, InteractionActor{User: pollServiceID(6), Kind: 0}, r)
	if err != nil || accepted.Status != 0 || accepted.Revision == nil || *accepted.Revision != 2 {
		t.Fatalf("valid vote result=%+v err=%v", accepted, err)
	}
	beforeReplay := snapshot()
	now = time.Unix(-1, 0)
	replayed, err := h.vote(ctx, InteractionActor{User: pollServiceID(6), Kind: 0}, r)
	if err != nil || replayed.Status != 0 || replayed.Revision == nil || *replayed.Revision != 2 {
		t.Fatalf("pre-epoch exact replay=%+v err=%v", replayed, err)
	}
	if !reflect.DeepEqual(beforeReplay, snapshot()) {
		t.Fatal("pre-epoch vote replay changed state")
	}
}
func pollServiceID(n byte) (v id.ID) { v[15] = n; return }

func TestCreatePublicationFailureIsRecoverableAndDoesNotReportSuccess(t *testing.T) {
	state := &pollTestState{}
	store := polls.NewStore(state, pollServiceID(1))
	authority := &pollAuthorityFake{bits: 2, member: true}
	pub := &pollPublisherFake{err: context.DeadlineExceeded}
	now := time.Unix(1000, 0)
	h := PollInteractions{Store: store, Authority: authority, CommunityID: pollServiceID(1), Now: func() time.Time { return now }, Publisher: pub}
	actor := InteractionActor{User: pollServiceID(2), Kind: 0}
	reqID := pollServiceID(30)
	ch := pollServiceID(3)
	payload, err := cborx.Marshal([]any{uint64(1), ch[:], nil, "Question", []any{"A", "B"}, uint64(0), uint64(3600)})
	if err != nil {
		t.Fatal(err)
	}
	rev := uint64(0)
	expires := uint64(1100)
	r := protocol.Request{RequestID: reqID, CommunityID: pollServiceID(1), Operation: protocol.OpPollCreate, Revision: &rev, Expires: &expires, Args: []any{payload}}
	result, err := h.create(context.Background(), actor, r)
	if err != nil || result.Status != interactionStatusUnavailable {
		t.Fatalf("failed publication result=%+v err=%v", result, err)
	}
	pub.err = nil
	if err = h.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	saved, err := store.Get(context.Background(), reqID)
	if err != nil || !saved.Published || pub.calls != 2 {
		t.Fatalf("saved=%+v publishes=%d err=%v", saved, pub.calls, err)
	}
	result, err = h.create(context.Background(), actor, r)
	if err != nil || result.Status != 0 {
		t.Fatalf("replayed create=%+v err=%v", result, err)
	}
}

func TestPollReadsRejectWrongCommunityAndNonHumanActors(t *testing.T) {
	state := &pollTestState{}
	store := polls.NewStore(state, pollServiceID(1))
	now := time.Unix(1000, 0)
	pollID := pollServiceID(3)
	if _, e := store.Create(context.Background(), pollID, [32]byte{1}, polls.Poll{ID: pollID, CommunityID: pollServiceID(1), ChannelID: pollServiceID(4), CreatorID: pollServiceID(2), Question: "Q", Answers: []string{"A", "B"}, CreatedAt: 1000, DueAt: 4600}, now); e != nil {
		t.Fatal(e)
	}
	auth := &pollAuthorityFake{bits: 1 | 64, member: true}
	h := PollInteractions{Store: store, Authority: auth, CommunityID: pollServiceID(1)}
	idBytes := pollID
	r := protocol.Request{CommunityID: pollServiceID(99), Operation: protocol.OpPollGet, Args: []any{idBytes}}
	res, _ := h.get(context.Background(), InteractionActor{User: pollServiceID(2)}, r)
	if res.Status != interactionStatusUnavailable {
		t.Fatalf("wrong community status=%d", res.Status)
	}
	r.CommunityID = pollServiceID(1)
	res, _ = h.get(context.Background(), InteractionActor{User: pollServiceID(2), Kind: 1}, r)
	if res.Status != interactionStatusForbidden || auth.calls != 0 {
		t.Fatalf("nonhuman read=%+v permission calls=%d", res, auth.calls)
	}
}

func TestPollListHandlerPaginatesByEncodedBytes(t *testing.T) {
	ctx := context.Background()
	state := &pollTestState{}
	store := polls.NewStore(state, pollServiceID(1))
	now := time.Unix(1000, 0)
	channel := pollServiceID(4)
	question := strings.Repeat("Q", 300)
	answers := make([]string, 10)
	for i := range answers {
		answers[i] = strings.Repeat(string(rune('a'+i)), 100)
	}
	ids := make([]id.ID, 10)
	for i := range ids {
		ids[i] = pollServiceID(byte(20 + i))
	}
	for _, pid := range ids {
		p := polls.Poll{ID: pid, CommunityID: pollServiceID(1), ChannelID: channel, CreatorID: pollServiceID(2), Question: question, Answers: answers, CreatedAt: 1000, DueAt: 4600}
		if _, e := store.Create(ctx, pid, [32]byte{pid[15]}, p, now); e != nil {
			t.Fatal(e)
		}
	}
	h := PollInteractions{Store: store, Authority: &pollAuthorityFake{bits: pollPermView | pollPermReadHistory, member: true}, CommunityID: pollServiceID(1), Now: func() time.Time { return now }}
	actor := InteractionActor{User: pollServiceID(2)}
	cursor := any(nil)
	var seen []id.ID
	for pageNo := 0; pageNo < 11; pageNo++ {
		result, e := h.list(ctx, actor, protocol.Request{CommunityID: pollServiceID(1), Operation: protocol.OpPollList, Args: []any{channel, nil, cursor}})
		if e != nil || result.Status != 0 || len(result.Data) > 2400 {
			t.Fatalf("page %d result=%+v err=%v", pageNo, result, e)
		}
		var response []any
		if e = cborx.Unmarshal(result.Data, &response); e != nil {
			t.Fatal(e)
		}
		rows := response[1].([]any)
		for _, item := range rows {
			summary := item.([]any)
			b := summary[0].([]byte)
			var got id.ID
			copy(got[:], b)
			seen = append(seen, got)
		}
		cursor = response[2]
		if cursor == nil {
			break
		}
		if pageNo == 10 {
			t.Fatal("pagination did not terminate")
		}
	}
	if !reflect.DeepEqual(seen, ids) {
		t.Fatalf("listed poll IDs=%v", seen)
	}
}

func TestPollVoterHandlerCursorRoundTripsAndChecksScope(t *testing.T) {
	ctx := context.Background()
	store := polls.NewStore(&pollTestState{}, pollServiceID(1))
	now := time.Unix(1000, 0)
	pid := pollServiceID(3)
	p := polls.Poll{ID: pid, CommunityID: pollServiceID(1), ChannelID: pollServiceID(4), CreatorID: pollServiceID(2), Question: "Q", Answers: []string{"A", "B"}, CreatedAt: 1000, DueAt: 4600}
	if _, e := store.Create(ctx, pid, [32]byte{1}, p, now); e != nil {
		t.Fatal(e)
	}
	for i := byte(10); i < 31; i++ {
		current, e := store.Get(ctx, pid)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = store.Vote(ctx, pid, pollServiceID(i), pollServiceID(i+40), [32]byte{i}, []uint64{0}, current.Revision, now); e != nil {
			t.Fatal(e)
		}
	}
	h := PollInteractions{Store: store, Authority: &pollAuthorityFake{bits: pollPermView | pollPermReadHistory, member: true}, CommunityID: pollServiceID(1), Now: func() time.Time { return now }}
	actor := InteractionActor{User: pollServiceID(2)}
	req := protocol.Request{CommunityID: pollServiceID(1), Operation: protocol.OpPollVoters, Args: []any{pid, uint64(0), nil}}
	first, e := h.voters(ctx, actor, req)
	if e != nil || first.Status != 0 {
		t.Fatalf("first page=%+v err=%v", first, e)
	}
	var a []any
	if e = cborx.Unmarshal(first.Data, &a); e != nil {
		t.Fatal(e)
	}
	cursor, ok := a[2].([]byte)
	if !ok || len(cursor) == 0 || len(cursor) > 256 {
		t.Fatalf("cursor type/size=%T %d", a[2], len(cursor))
	}
	req.Args[2] = cursor
	second, e := h.voters(ctx, actor, req)
	if e != nil || second.Status != 0 {
		t.Fatalf("cursor round trip status=%d err=%v", second.Status, e)
	}
	var b []any
	if e = cborx.Unmarshal(second.Data, &b); e != nil {
		t.Fatal(e)
	}
	secondIDs := b[1].([]any)
	if len(secondIDs) != 1 || b[2] != nil {
		t.Fatalf("second page=%v", b)
	}
	req.Args[1] = uint64(1)
	wrong, e := h.voters(ctx, actor, req)
	if e != nil || wrong.Status != interactionStatusUnavailable {
		t.Fatalf("cursor accepted for wrong option: %+v err=%v", wrong, e)
	}
}

func TestCreatorCloseRequiresCurrentChannelReadAccess(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(1000, 0)
	state := &pollTestState{}
	store := polls.NewStore(state, pollServiceID(1))
	pid := pollServiceID(3)
	p := polls.Poll{ID: pid, CommunityID: pollServiceID(1), ChannelID: pollServiceID(4), CreatorID: pollServiceID(2), Question: "private", Answers: []string{"A", "B"}, CreatedAt: 1000, DueAt: 4600}
	if _, e := store.Create(ctx, pid, [32]byte{1}, p, now); e != nil {
		t.Fatal(e)
	}
	auth := &pollAuthorityFake{bits: 0, member: true}
	h := PollInteractions{Store: store, Authority: auth, CommunityID: pollServiceID(1), Now: func() time.Time { return now }}
	rev, expiry := uint64(1), uint64(1030)
	result, e := h.close(ctx, InteractionActor{User: p.CreatorID}, protocol.Request{RequestID: pollServiceID(9), CommunityID: pollServiceID(1), Operation: protocol.OpPollClose, Revision: &rev, Expires: &expiry, Args: []any{pid}})
	if e != nil || result.Status != interactionStatusForbidden || len(result.Data) != 0 {
		t.Fatalf("revoked creator close=%+v err=%v", result, e)
	}
	stored, e := store.Get(ctx, pid)
	if e != nil || stored.Closed {
		t.Fatalf("revoked close mutated poll=%+v err=%v", stored, e)
	}
}

func TestPollHandlersMapNativeCASStatusFourToConflict(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(1000, 0)
	for _, op := range []protocol.Operation{protocol.OpPollCreate, protocol.OpPollVote, protocol.OpPollClose} {
		t.Run(fmt.Sprint(op), func(t *testing.T) {
			state := &pollTestState{}
			store := polls.NewStore(state, pollServiceID(1))
			auth := &pollAuthorityFake{bits: pollPermView | pollPermSend | pollPermReadHistory | pollPermManageMessages, member: true}
			h := PollInteractions{Store: store, Authority: auth, CommunityID: pollServiceID(1), Now: func() time.Time { return now }, Publisher: &pollPublisherFake{}}
			actor := InteractionActor{User: pollServiceID(2)}
			pid := pollServiceID(3)
			rev, expiry := uint64(1), uint64(1030)
			var req protocol.Request
			if op == protocol.OpPollCreate {
				channel := pollServiceID(4)
				payload, _ := cborx.Marshal([]any{uint64(1), channel[:], nil, "Q", []any{"A", "B"}, uint64(0), uint64(3600)})
				zero := uint64(0)
				req = protocol.Request{RequestID: pid, CommunityID: pollServiceID(1), Operation: op, Revision: &zero, Expires: &expiry, Args: []any{payload}}
			} else {
				if _, e := store.Create(ctx, pid, [32]byte{1}, polls.Poll{ID: pid, CommunityID: pollServiceID(1), ChannelID: pollServiceID(4), CreatorID: actor.User, Question: "Q", Answers: []string{"A", "B"}, CreatedAt: 1000, DueAt: 4600}, now); e != nil {
					t.Fatal(e)
				}
				if op == protocol.OpPollVote {
					selection, _ := cborx.Marshal([]any{uint64(0)})
					req = protocol.Request{RequestID: pollServiceID(8), CommunityID: pollServiceID(1), Operation: op, Revision: &rev, Expires: &expiry, Args: []any{pid, selection}}
				} else {
					req = protocol.Request{RequestID: pollServiceID(9), CommunityID: pollServiceID(1), Operation: op, Revision: &rev, Expires: &expiry, Args: []any{pid}}
				}
			}
			state.mu.Lock()
			state.txErr = &helper.StatusError{Status: 4}
			state.mu.Unlock()
			var result InteractionResult
			var e error
			switch op {
			case protocol.OpPollCreate:
				result, e = h.create(ctx, actor, req)
			case protocol.OpPollVote:
				result, e = h.vote(ctx, actor, req)
			case protocol.OpPollClose:
				result, e = h.close(ctx, actor, req)
			}
			if e != nil || result.Status != interactionStatusConflict {
				t.Fatalf("native CAS status result=%+v err=%v", result, e)
			}
		})
	}
}
