package polls

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jonasthim/dilla/internal/bot/helper"
	"github.com/jonasthim/dilla/internal/bot/native"
	"github.com/jonasthim/dilla/internal/id"
)

type testState struct {
	mu          sync.Mutex
	rows        map[string]native.StateEntry
	getFailures map[string]int
	txErr       error
}

func newTestState() *testState { return &testState{rows: map[string]native.StateEntry{}} }

func TestUnixSecondsRejectsPreEpochValues(t *testing.T) {
	if got, ok := unixSeconds(time.Unix(-1, 0)); ok || got != 0 {
		t.Fatalf("pre-epoch Unix time converted to %d, ok=%v", got, ok)
	}
	if got, ok := unixSeconds(time.Unix(1, 0)); !ok || got != 1 {
		t.Fatalf("positive Unix time converted to %d, ok=%v", got, ok)
	}
}
func (s *testState) StateGet(_ context.Context, _ uint64, key []byte) (native.StateValue, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.getFailures != nil && s.getFailures[string(key)] > 0 {
		s.getFailures[string(key)]--
		return native.StateValue{}, errors.New("injected StateGet failure")
	}
	e, ok := s.rows[string(key)]
	if !ok {
		return native.StateValue{}, nil
	}
	return native.StateValue{Revision: e.Revision, Value: append([]byte(nil), e.Value...), Tombstone: e.Tombstone}, nil
}
func (s *testState) StateScan(_ context.Context, _ uint64, after []byte, limit uint64) (native.StatePage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var keys []string
	for k := range s.rows {
		if len(after) == 0 || k > string(after) {
			keys = append(keys, k)
		}
	}
	sortStrings(keys)
	if len(keys) > int(limit) {
		keys = keys[:limit]
	}
	out := native.StatePage{}
	for _, k := range keys {
		out.Entries = append(out.Entries, s.rows[k])
	}
	if len(keys) > 0 {
		out.Next = []byte(keys[len(keys)-1])
	}
	return out, nil
}
func (s *testState) StateTransaction(_ context.Context, _ id.ID, changes []native.StateChange) ([]uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.txErr != nil {
		e := s.txErr
		s.txErr = nil
		return nil, e
	}
	for _, c := range changes {
		e := s.rows[string(c.Key)]
		if e.Revision != c.ExpectedRevision {
			return nil, ErrConflict
		}
	}
	out := make([]uint64, len(changes))
	for i, c := range changes {
		e := s.rows[string(c.Key)]
		e.Key = append([]byte(nil), c.Key...)
		e.Revision++
		e.Value = append([]byte(nil), c.Value...)
		e.Tombstone = c.Value == nil
		s.rows[string(c.Key)] = e
		out[i] = e.Revision
	}
	return out, nil
}
func sortStrings(a []string) {
	for i := 1; i < len(a); i++ {
		for j := i; j > 0 && a[j] < a[j-1]; j-- {
			a[j], a[j-1] = a[j-1], a[j]
		}
	}
}
func pid(n byte) (v id.ID) { v[15] = n; return }

func TestCreateVoteReplaceWithdrawAndDurableReplay(t *testing.T) {
	ctx := context.Background()
	state := newTestState()
	store := NewStore(state, pid(1))
	now := time.Unix(1000, 0)
	p := Poll{ID: pid(2), CommunityID: pid(1), ChannelID: pid(3), CreatorID: pid(4), Question: "Lunch?", Answers: []string{"Soup", "Salad"}, CreatedAt: 1000, DueAt: 4600}
	got, err := store.Create(ctx, p.ID, [32]byte{1}, p, now)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != p.ID {
		t.Fatalf("create=%+v", got)
	}
	out, err := store.Vote(ctx, p.ID, pid(6), pid(7), [32]byte{2}, []uint64{0}, 1, now)
	if err != nil || len(out.Choice) != 1 {
		t.Fatalf("vote choices=%v err=%v", out.Choice, err)
	}
	out, err = store.Vote(ctx, p.ID, pid(6), pid(8), [32]byte{3}, []uint64{1}, 2, now)
	if err != nil || len(out.Choice) != 1 || out.Choice[0] != 1 {
		t.Fatalf("replace=%v err=%v", out.Choice, err)
	}
	counts, _, err := store.Counts(ctx, p.ID, pid(6))
	if err != nil || counts[0] != 0 || counts[1] != 1 {
		t.Fatalf("counts=%v err=%v", counts, err)
	}
	_, err = store.Vote(ctx, p.ID, pid(6), pid(9), [32]byte{4}, nil, 3, now)
	if err != nil {
		t.Fatal(err)
	}
	counts, _, err = store.Counts(ctx, p.ID, pid(6))
	if err != nil || counts[1] != 0 {
		t.Fatalf("withdraw counts=%v err=%v", counts, err)
	}
	replay, err := NewStore(state, pid(1)).Vote(ctx, p.ID, pid(6), pid(8), [32]byte{3}, []uint64{1}, 2, now)
	if err != nil || len(replay.Choice) != 1 || replay.Poll.Revision != 3 || replay.Poll.Counts[1] != 1 {
		t.Fatalf("durable replay=%+v err=%v", replay, err)
	}
	_, err = store.Vote(ctx, p.ID, pid(6), pid(8), [32]byte{9}, []uint64{0}, 2, now)
	if !errors.Is(err, ErrMutationIDConflict) {
		t.Fatalf("reuse err=%v", err)
	}
}

func TestPreEpochVoteFailsClosedAndExactReplayStillWins(t *testing.T) {
	ctx := context.Background()
	state := newTestState()
	store := NewStore(state, pid(1))
	p := Poll{ID: pid(2), CommunityID: pid(1), ChannelID: pid(3), CreatorID: pid(4), Question: "Lunch?", Answers: []string{"Soup", "Salad"}, CreatedAt: 1000, DueAt: 4600, Published: true}
	if _, err := store.Create(ctx, p.ID, [32]byte{1}, p, time.Unix(1000, 0)); err != nil {
		t.Fatal(err)
	}
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
	if _, err := store.Vote(ctx, p.ID, pid(6), pid(7), [32]byte{2}, []uint64{0}, 1, time.Unix(-1, 0)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("pre-epoch vote err=%v, want ErrInvalid", err)
	}
	if !reflect.DeepEqual(before, snapshot()) {
		t.Fatal("pre-epoch vote changed poll, vote, quota, or receipt state")
	}
	accepted, err := store.Vote(ctx, p.ID, pid(6), pid(8), [32]byte{3}, []uint64{1}, 1, time.Unix(1000, 0))
	if err != nil || accepted.Poll.Revision != 2 {
		t.Fatalf("valid vote=%+v err=%v", accepted, err)
	}
	beforeReplay := snapshot()
	replayed, err := store.Vote(ctx, p.ID, pid(6), pid(8), [32]byte{3}, []uint64{1}, 1, time.Unix(-1, 0))
	if err != nil || replayed.Poll.Revision != 2 || replayed.Poll.Counts[1] != 1 {
		t.Fatalf("pre-epoch exact replay=%+v err=%v", replayed, err)
	}
	if !reflect.DeepEqual(beforeReplay, snapshot()) {
		t.Fatal("pre-epoch exact replay changed state")
	}
	if err := store.Reconcile(ctx, time.Unix(-1, 0), &publisherTest{}); err != nil {
		t.Fatal(err)
	}
	afterScheduler, err := store.Get(ctx, p.ID)
	if err != nil || afterScheduler.Closed || afterScheduler.Revision != 2 {
		t.Fatalf("pre-epoch scheduler poll=%+v err=%v", afterScheduler, err)
	}
}

func TestPollDeadlineAndClose(t *testing.T) {
	ctx := context.Background()
	s := NewStore(newTestState(), pid(1))
	now := time.Unix(100, 0)
	p := Poll{ID: pid(2), CommunityID: pid(1), ChannelID: pid(3), CreatorID: pid(4), Question: "Q", Answers: []string{"A", "B"}, CreatedAt: 100, DueAt: 200}
	if _, e := s.Create(ctx, p.ID, [32]byte{1}, p, now); e != nil {
		t.Fatal(e)
	}
	if _, e := s.Vote(ctx, p.ID, pid(6), pid(7), [32]byte{2}, []uint64{0}, 1, time.Unix(200, 0)); !errors.Is(e, ErrClosed) {
		t.Fatalf("deadline vote=%v", e)
	}
	p, e := s.Close(ctx, p.ID, pid(4), pid(8), [32]byte{3}, 1, now, false)
	if e != nil || !p.Closed {
		t.Fatalf("close=%+v err=%v", p, e)
	}
}

func TestMultiChoiceBoundsAndNamedVoters(t *testing.T) {
	ctx := context.Background()
	state := newTestState()
	s := NewStore(state, pid(1))
	now := time.Unix(1000, 0)
	p := Poll{ID: pid(2), CommunityID: pid(1), ChannelID: pid(3), CreatorID: pid(4), Question: "Choose", Answers: []string{"A", "B", "C"}, Multi: true, CreatedAt: 1000, DueAt: 4600}
	_, err := s.Create(ctx, p.ID, [32]byte{1}, p, now)
	if err != nil {
		t.Fatal(err)
	}
	out, err := s.Vote(ctx, p.ID, pid(5), pid(6), [32]byte{2}, []uint64{0, 2}, 1, now)
	if err != nil || len(out.Choice) != 2 {
		t.Fatalf("multi vote=%+v err=%v", out, err)
	}
	if _, err = s.Vote(ctx, p.ID, pid(7), pid(8), [32]byte{3}, []uint64{0, 0}, 2, now); !errors.Is(err, ErrInvalid) {
		t.Fatalf("duplicate option error=%v", err)
	}
	if _, err = s.Vote(ctx, p.ID, pid(7), pid(9), [32]byte{4}, []uint64{3}, 2, now); !errors.Is(err, ErrInvalid) {
		t.Fatalf("out of range error=%v", err)
	}
	voters, _, err := s.Voters(ctx, p.ID, 2, nil, 20)
	if err != nil || len(voters) != 1 || voters[0] != pid(5) {
		t.Fatalf("voters=%v err=%v", voters, err)
	}
	if _, _, err = s.Voters(ctx, p.ID, 2, []byte(votePrefix+idhex(pid(9))+"/"), 20); !errors.Is(err, ErrInvalid) {
		t.Fatalf("foreign cursor error=%v", err)
	}
}

func TestListCursorPagesStayBoundToCommunityChannelAndThread(t *testing.T) {
	ctx := context.Background()
	s := NewStore(newTestState(), pid(1))
	now := time.Unix(1000, 0)
	thread := pid(8)
	for i := byte(10); i < 32; i++ {
		p := Poll{ID: pid(i), CommunityID: pid(1), ChannelID: pid(3), ThreadID: &thread, CreatorID: pid(4), Question: "Q", Answers: []string{"A", "B"}, CreatedAt: 1000, DueAt: 4600}
		if _, e := s.Create(ctx, p.ID, [32]byte{i}, p, now); e != nil {
			t.Fatal(e)
		}
	}
	first, cursor, e := s.List(ctx, pid(3), &thread, "")
	if e != nil || len(first) != pageSize || cursor == "" {
		t.Fatalf("first len=%d cursor=%q err=%v", len(first), cursor, e)
	}
	second, next, e := s.List(ctx, pid(3), &thread, cursor)
	if e != nil || len(second) != 2 || next != "" {
		t.Fatalf("second len=%d next=%q err=%v", len(second), next, e)
	}
	if _, _, e = s.List(ctx, pid(9), &thread, cursor); !errors.Is(e, ErrInvalid) {
		t.Fatalf("wrong channel cursor err=%v", e)
	}
	if len(cursor) > 256 {
		t.Fatalf("cursor bytes=%d", len(cursor))
	}
}

func TestOpenQuotaReleasedByEarlyClose(t *testing.T) {
	ctx := context.Background()
	s := NewStore(newTestState(), pid(1))
	now := time.Unix(1000, 0)
	p := Poll{ID: pid(2), CommunityID: pid(1), ChannelID: pid(3), CreatorID: pid(4), Question: "Q", Answers: []string{"A", "B"}, CreatedAt: 1000, DueAt: 604900}
	if _, e := s.Create(ctx, p.ID, [32]byte{1}, p, now); e != nil {
		t.Fatal(e)
	}
	closed, e := s.Close(ctx, p.ID, pid(4), pid(5), [32]byte{2}, 1, now, false)
	if e != nil || !closed.Closed {
		t.Fatalf("close=%+v err=%v", closed, e)
	}
	p.ID = pid(6)
	if _, e = s.Create(ctx, p.ID, [32]byte{6}, p, now); e != nil {
		t.Fatalf("create after close err=%v", e)
	}
}

type publisherTest struct {
	calls int
	fail  bool
}

func (p *publisherTest) PublishPoll(context.Context, Poll) error {
	p.calls++
	if p.fail {
		return errors.New("transport unavailable")
	}
	return nil
}
func TestReconcileRetriesPublicationAndStoresReceiptState(t *testing.T) {
	ctx := context.Background()
	s := NewStore(newTestState(), pid(1))
	now := time.Unix(1000, 0)
	p := Poll{ID: pid(2), CommunityID: pid(1), ChannelID: pid(3), CreatorID: pid(4), Question: "Q", Answers: []string{"A", "B"}, CreatedAt: 1000, DueAt: 4600}
	if _, e := s.Create(ctx, p.ID, [32]byte{1}, p, now); e != nil {
		t.Fatal(e)
	}
	pub := &publisherTest{fail: true}
	if e := s.Reconcile(ctx, now, pub); e == nil {
		t.Fatal("failed publisher was treated as success")
	}
	pub.fail = false
	if e := NewStore(s.state, pid(1)).Reconcile(ctx, now, pub); e != nil {
		t.Fatal(e)
	}
	got, e := s.Get(ctx, p.ID)
	if e != nil || !got.Published || pub.calls != 2 {
		t.Fatalf("poll=%+v calls=%d err=%v", got, pub.calls, e)
	}
}

func TestConcurrentVotesPreserveCountsWithRevisionCAS(t *testing.T) {
	ctx := context.Background()
	s := NewStore(newTestState(), pid(1))
	now := time.Unix(1000, 0)
	p := Poll{ID: pid(2), CommunityID: pid(1), ChannelID: pid(3), CreatorID: pid(4), Question: "Q", Answers: []string{"A", "B"}, CreatedAt: 1000, DueAt: 4600}
	if _, e := s.Create(ctx, p.ID, [32]byte{1}, p, now); e != nil {
		t.Fatal(e)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := byte(10); i < 26; i++ {
		wg.Add(1)
		go func(user byte) {
			defer wg.Done()
			req := pid(user + 40)
			for retry := 0; retry < 50; retry++ {
				current, e := s.Get(ctx, p.ID)
				if e != nil {
					errs <- e
					return
				}
				_, e = s.Vote(ctx, p.ID, pid(user), req, [32]byte{user}, []uint64{uint64(user % 2)}, current.Revision, now)
				if e == nil {
					return
				}
				if !errors.Is(e, ErrConflict) {
					errs <- e
					return
				}
			}
			errs <- ErrConflict
		}(i)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Fatal(e)
	}
	counts, _, e := s.Counts(ctx, p.ID, pid(70))
	if e != nil {
		t.Fatal(e)
	}
	if counts[0]+counts[1] != 16 {
		t.Fatalf("counts=%v", counts)
	}
}

func TestVoteRejectsStaleRevisionAndClosedPollRemainsReadable(t *testing.T) {
	ctx := context.Background()
	s := NewStore(newTestState(), pid(1))
	now := time.Unix(1000, 0)
	p := Poll{ID: pid(2), CommunityID: pid(1), ChannelID: pid(3), CreatorID: pid(4), Question: "Q", Answers: []string{"A", "B"}, CreatedAt: 1000, DueAt: 4600}
	if _, e := s.Create(ctx, p.ID, [32]byte{1}, p, now); e != nil {
		t.Fatal(e)
	}
	if _, e := s.Vote(ctx, p.ID, pid(5), pid(6), [32]byte{2}, []uint64{0}, 0, now); !errors.Is(e, ErrConflict) {
		t.Fatalf("stale vote=%v", e)
	}
	closed, e := s.Close(ctx, p.ID, pid(4), pid(7), [32]byte{3}, 1, now, false)
	if e != nil {
		t.Fatal(e)
	}
	read, e := s.Get(ctx, p.ID)
	if e != nil || !read.Closed || read.ID != closed.ID {
		t.Fatalf("closed poll read=%+v err=%v", read, e)
	}
}

func TestOpenPollQuotaIsAtomicAndReleasedByClose(t *testing.T) {
	ctx := context.Background()
	s := NewStore(newTestState(), pid(1))
	now := time.Unix(1000, 0)
	for n := byte(2); n < 130; n++ {
		p := Poll{ID: pid(n), CommunityID: pid(1), ChannelID: pid(3), CreatorID: pid(4), Question: "Q", Answers: []string{"A", "B"}, CreatedAt: 1000, DueAt: 4600}
		if _, e := s.Create(ctx, p.ID, [32]byte{n}, p, now); e != nil {
			t.Fatalf("create %d: %v", n, e)
		}
	}
	extra := Poll{ID: pid(130), CommunityID: pid(1), ChannelID: pid(3), CreatorID: pid(4), Question: "Q", Answers: []string{"A", "B"}, CreatedAt: 1000, DueAt: 4600}
	if _, e := s.Create(ctx, extra.ID, [32]byte{130}, extra, now); !errors.Is(e, ErrQuota) {
		t.Fatalf("129th poll error=%v", e)
	}
	if _, e := s.Close(ctx, pid(2), pid(4), pid(131), [32]byte{131}, 1, now, false); e != nil {
		t.Fatal(e)
	}
	if _, e := s.Create(ctx, extra.ID, [32]byte{130}, extra, now); e != nil {
		t.Fatalf("create after quota release: %v", e)
	}
}

func TestSingleChoiceRejectsMultipleSelections(t *testing.T) {
	ctx := context.Background()
	s := NewStore(newTestState(), pid(1))
	now := time.Unix(1000, 0)
	p := Poll{ID: pid(2), CommunityID: pid(1), ChannelID: pid(3), CreatorID: pid(4), Question: "Q", Answers: []string{"A", "B"}, CreatedAt: 1000, DueAt: 4600}
	if _, e := s.Create(ctx, p.ID, [32]byte{1}, p, now); e != nil {
		t.Fatal(e)
	}
	if _, e := s.Vote(ctx, p.ID, pid(5), pid(6), [32]byte{2}, []uint64{0, 1}, 1, now); !errors.Is(e, ErrInvalid) {
		t.Fatalf("multiple selections on single choice=%v", e)
	}
}

func TestVoteReplacementFailsClosedOnPreviousVoteReadError(t *testing.T) {
	ctx := context.Background()
	state := newTestState()
	s := NewStore(state, pid(1))
	now := time.Unix(1000, 0)
	p := Poll{ID: pid(2), CommunityID: pid(1), ChannelID: pid(3), CreatorID: pid(4), Question: "Q", Answers: []string{"A", "B"}, CreatedAt: 1000, DueAt: 4600}
	if _, e := s.Create(ctx, p.ID, [32]byte{1}, p, now); e != nil {
		t.Fatal(e)
	}
	if _, e := s.Vote(ctx, p.ID, pid(5), pid(6), [32]byte{2}, []uint64{0}, 1, now); e != nil {
		t.Fatal(e)
	}
	key := voteKey(p.ID, pid(5))
	state.mu.Lock()
	state.getFailures = map[string]int{string(key): 1}
	state.mu.Unlock()
	if _, e := s.Vote(ctx, p.ID, pid(5), pid(7), [32]byte{3}, []uint64{1}, 2, now); e == nil {
		t.Fatal("replacement hid transient previous-vote read failure")
	}
	counts, choices, e := s.Counts(ctx, p.ID, pid(5))
	if e != nil || counts[0] != 1 || counts[1] != 0 || len(choices) != 1 || choices[0] != 0 {
		t.Fatalf("after failed read counts=%v choices=%v err=%v", counts, choices, e)
	}
}

func TestVoteReplacementFailsClosedOnCorruptPreviousVote(t *testing.T) {
	ctx := context.Background()
	state := newTestState()
	s := NewStore(state, pid(1))
	now := time.Unix(1000, 0)
	p := Poll{ID: pid(2), CommunityID: pid(1), ChannelID: pid(3), CreatorID: pid(4), Question: "Q", Answers: []string{"A", "B"}, CreatedAt: 1000, DueAt: 4600}
	if _, e := s.Create(ctx, p.ID, [32]byte{1}, p, now); e != nil {
		t.Fatal(e)
	}
	if _, e := s.Vote(ctx, p.ID, pid(5), pid(6), [32]byte{2}, []uint64{0}, 1, now); e != nil {
		t.Fatal(e)
	}
	state.mu.Lock()
	row := state.rows[string(voteKey(p.ID, pid(5)))]
	row.Value = []byte{0xff}
	state.rows[string(row.Key)] = row
	state.mu.Unlock()
	if _, e := s.Vote(ctx, p.ID, pid(5), pid(7), [32]byte{3}, []uint64{1}, 2, now); !errors.Is(e, ErrCorruptVote) {
		t.Fatalf("corrupt prior vote error=%v", e)
	}
	poll, e := s.Get(ctx, p.ID)
	if e != nil || poll.Counts[0] != 1 || poll.Counts[1] != 0 {
		t.Fatalf("corrupt vote changed tally=%v err=%v", poll.Counts, e)
	}
}

func TestNativeStatusConflictNormalizesAtStoreBoundary(t *testing.T) {
	ctx := context.Background()
	state := newTestState()
	state.txErr = &helper.StatusError{Status: 4}
	s := NewStore(state, pid(1))
	now := time.Unix(1000, 0)
	p := Poll{ID: pid(2), CommunityID: pid(1), ChannelID: pid(3), CreatorID: pid(4), Question: "Q", Answers: []string{"A", "B"}, CreatedAt: 1000, DueAt: 4600}
	if _, e := s.Create(ctx, p.ID, [32]byte{1}, p, now); !errors.Is(e, ErrConflict) {
		t.Fatalf("helper status4 error=%v", e)
	}
}

type selectivePollPublisher struct {
	failID id.ID
	calls  []id.ID
}

func (p *selectivePollPublisher) PublishPoll(_ context.Context, poll Poll) error {
	p.calls = append(p.calls, poll.ID)
	if poll.ID == p.failID {
		return errors.New("injected publication outage")
	}
	return nil
}

func TestReconcileExpiresAllDuePollsAfterPublicationFailure(t *testing.T) {
	ctx := context.Background()
	state := newTestState()
	store := NewStore(state, pid(1))
	created := time.Unix(1000, 0)
	for _, pollID := range []id.ID{pid(2), pid(3)} {
		poll := Poll{ID: pollID, CommunityID: pid(1), ChannelID: pid(4), CreatorID: pid(5), Question: "Lunch?", Answers: []string{"Soup", "Salad"}, CreatedAt: 1000, DueAt: 1100}
		if _, err := store.Create(ctx, poll.ID, [32]byte{pollID[15]}, poll, created); err != nil {
			t.Fatal(err)
		}
	}
	publisher := &selectivePollPublisher{failID: pid(2)}
	err := store.Reconcile(ctx, time.Unix(2000, 0), publisher)
	if err == nil || !strings.Contains(err.Error(), "injected publication outage") {
		t.Fatalf("publication failure was not reported: %v", err)
	}
	for _, pollID := range []id.ID{pid(2), pid(3)} {
		got, getErr := store.Get(ctx, pollID)
		if getErr != nil || !got.Closed {
			t.Fatalf("poll %x was not closed after catch-up: poll=%+v err=%v", pollID, got, getErr)
		}
	}
	if len(publisher.calls) != 2 || publisher.calls[0] != pid(2) || publisher.calls[1] != pid(3) {
		t.Fatalf("reconcile did not continue publishing later records: %v", publisher.calls)
	}
	quota, quotaErr := store.get(ctx, quotaKey(pid(1)))
	if quotaErr != nil {
		t.Fatal(quotaErr)
	}
	open, decodeErr := decodeIDs(quota.Value)
	if decodeErr != nil || len(open) != 0 {
		t.Fatalf("expired poll quota not released: open=%v err=%v", open, decodeErr)
	}
}

type reviewCloseConflictPublisher struct{ state *testState }

func (p reviewCloseConflictPublisher) PublishPoll(context.Context, Poll) error {
	p.state.txErr = ErrConflict
	return errors.New("publication failed")
}

func TestReconcileRetriesExpiryConflictWithoutReleasingQuotaEarly(t *testing.T) {
	ctx := context.Background()
	state := newTestState()
	store := NewStore(state, pid(1))
	poll := Poll{ID: pid(2), CommunityID: pid(1), ChannelID: pid(3), CreatorID: pid(4), Question: "Q?", Answers: []string{"A", "B"}, CreatedAt: 1000, DueAt: 1100}
	if _, err := store.Create(ctx, poll.ID, [32]byte{1}, poll, time.Unix(1000, 0)); err != nil {
		t.Fatal(err)
	}
	err := store.Reconcile(ctx, time.Unix(2000, 0), reviewCloseConflictPublisher{state})
	if err == nil || !strings.Contains(err.Error(), "publication failed") {
		t.Fatalf("publication failure should remain observable after successful expiry: %v", err)
	}
	closed, err := store.Get(ctx, poll.ID)
	if err != nil || !closed.Closed {
		t.Fatalf("expiry did not durably close after transient conflict: %+v err=%v", closed, err)
	}
	quota, err := store.get(ctx, quotaKey(pid(1)))
	if err != nil {
		t.Fatal(err)
	}
	open, err := decodeIDs(quota.Value)
	if err != nil || len(open) != 0 {
		t.Fatalf("closed poll quota remains reserved: open=%v err=%v", open, err)
	}
}
