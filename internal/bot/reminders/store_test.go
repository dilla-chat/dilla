package reminders

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/jonasthim/dilla/internal/bot/native"
	"github.com/jonasthim/dilla/internal/id"
)

type durableState struct {
	mu      sync.Mutex
	rows    map[string]native.StateEntry
	getHook func(context.Context, []byte, native.StateValue)
}
type pauseGetKey struct{}

func TestUnixSecondsRejectsPreEpochValues(t *testing.T) {
	if got, ok := unixSeconds(time.Unix(-1, 0)); ok || got != 0 {
		t.Fatalf("pre-epoch Unix time converted to %d, ok=%v", got, ok)
	}
	if got, ok := unixSeconds(time.Unix(1, 0)); !ok || got != 1 {
		t.Fatalf("positive Unix time converted to %d, ok=%v", got, ok)
	}
}

func (s *durableState) StateGet(ctx context.Context, _ uint64, key []byte) (native.StateValue, error) {
	s.mu.Lock()
	e := s.rows[string(key)]
	v := native.StateValue{Revision: e.Revision, Value: append([]byte(nil), e.Value...), Tombstone: e.Tombstone}
	hook := s.getHook
	s.mu.Unlock()
	if hook != nil {
		hook(ctx, append([]byte(nil), key...), v)
	}
	return v, nil
}
func (s *durableState) StateScan(_ context.Context, _ uint64, after []byte, limit uint64) (native.StatePage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ks := []string{}
	for k := range s.rows {
		if len(after) == 0 || k > string(after) {
			ks = append(ks, k)
		}
	}
	sort.Strings(ks)
	if len(ks) > int(limit) {
		ks = ks[:limit]
	}
	p := native.StatePage{}
	for _, k := range ks {
		p.Entries = append(p.Entries, s.rows[k])
	}
	if len(ks) > 0 {
		p.Next = []byte(ks[len(ks)-1])
	}
	return p, nil
}
func (s *durableState) StateTransaction(_ context.Context, _ id.ID, cs []native.StateChange) ([]uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(cs) > 64 {
		return nil, errors.New("tx too large")
	}
	if s.rows == nil {
		s.rows = map[string]native.StateEntry{}
	}
	for _, c := range cs {
		if s.rows[string(c.Key)].Revision != c.ExpectedRevision {
			return nil, ErrConflict
		}
	}
	rs := make([]uint64, len(cs))
	for i, c := range cs {
		e := s.rows[string(c.Key)]
		e.Key = append([]byte(nil), c.Key...)
		e.Revision++
		e.Value = append([]byte(nil), c.Value...)
		s.rows[string(c.Key)] = e
		rs[i] = e.Revision
	}
	return rs, nil
}
func tid(n byte) (v id.ID) { v[15] = n; return }
func TestCreateReplayRetainsAcceptedDeadlineAndRejectsChangedFingerprint(t *testing.T) {
	ctx := context.Background()
	db := &durableState{}
	s := NewStore(db, native.Binding{Community: tid(1), Installation: tid(2)})
	now := time.Unix(1000, 0)
	r := Reminder{Text: "hello", Timezone: "UTC"}
	p, e := s.Create(ctx, tid(8), tid(4), [32]byte{1}, r, now.Add(time.Hour))
	if e != nil {
		t.Fatal(e)
	}
	again, e := NewStore(db, native.Binding{Community: tid(1), Installation: tid(2)}).Create(ctx, tid(8), tid(4), [32]byte{1}, r, now.Add(2*time.Hour))
	if e != nil || again.DueAt != p.DueAt {
		t.Fatalf("replay=%+v err=%v", again, e)
	}
	_, e = s.Create(ctx, tid(8), tid(4), [32]byte{2}, r, now)
	if !errors.Is(e, ErrMutationIDConflict) {
		t.Fatalf("changed fingerprint err=%v", e)
	}
}

func TestPreEpochCancelFailsClosedAndExactReplayStillWins(t *testing.T) {
	ctx := context.Background()
	db := &durableState{}
	s := NewStore(db, native.Binding{Community: tid(1), Installation: tid(2)}, func() time.Time { return time.Unix(-1, 0) })
	r, err := s.Create(ctx, tid(8), tid(4), [32]byte{1}, Reminder{Text: "x", Timezone: "UTC"}, time.Unix(4600, 0))
	if err != nil {
		t.Fatal(err)
	}
	snapshot := func() map[string]native.StateEntry {
		db.mu.Lock()
		defer db.mu.Unlock()
		out := make(map[string]native.StateEntry, len(db.rows))
		for key, entry := range db.rows {
			entry.Key = append([]byte(nil), entry.Key...)
			entry.Value = append([]byte(nil), entry.Value...)
			out[key] = entry
		}
		return out
	}
	before := snapshot()
	if err := s.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, snapshot()) {
		t.Fatal("pre-epoch scheduler changed reminder or quota state")
	}
	if _, err := s.Cancel(ctx, r.ID, tid(4), tid(9), [32]byte{2}, 1, time.Unix(-1, 0)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("pre-epoch cancel err=%v, want ErrInvalid", err)
	}
	if !reflect.DeepEqual(before, snapshot()) {
		t.Fatal("pre-epoch cancellation changed reminder, receipt, or quota state")
	}
	accepted, err := s.Cancel(ctx, r.ID, tid(4), tid(10), [32]byte{3}, 1, time.Unix(1000, 0))
	if err != nil || accepted.State != StateCancelled || accepted.Revision != 2 {
		t.Fatalf("valid cancel=%+v err=%v", accepted, err)
	}
	beforeReplay := snapshot()
	replayed, err := s.Cancel(ctx, r.ID, tid(4), tid(10), [32]byte{3}, 1, time.Unix(-1, 0))
	if err != nil || replayed.State != StateCancelled || replayed.Revision != 2 {
		t.Fatalf("pre-epoch exact replay=%+v err=%v", replayed, err)
	}
	if !reflect.DeepEqual(beforeReplay, snapshot()) {
		t.Fatal("pre-epoch exact replay changed state")
	}
}
func TestUserOwnershipQuotaAndCancelRelease(t *testing.T) {
	ctx := context.Background()
	s := NewStore(&durableState{}, native.Binding{Community: tid(1), Installation: tid(2)})
	now := time.Unix(1000, 0)
	for i := byte(10); i < 110; i++ {
		if _, e := s.Create(ctx, tid(i), tid(4), [32]byte{i}, Reminder{Text: "x", Timezone: "UTC"}, now.Add(time.Hour)); e != nil {
			t.Fatalf("create %d: %v", i, e)
		}
	}
	if _, e := s.Create(ctx, tid(200), tid(4), [32]byte{200}, Reminder{Text: "x", Timezone: "UTC"}, now.Add(time.Hour)); !errors.Is(e, ErrQuota) {
		t.Fatalf("quota err=%v", e)
	}
	if _, e := s.Cancel(ctx, tid(10), tid(4), tid(201), [32]byte{201}, 1, now); e != nil {
		t.Fatal(e)
	}
	if _, e := s.Create(ctx, tid(200), tid(4), [32]byte{200}, Reminder{Text: "x", Timezone: "UTC"}, now.Add(time.Hour)); e != nil {
		t.Fatalf("quota not released: %v", e)
	}
	if _, e := s.Get(ctx, tid(10), tid(5)); !errors.Is(e, ErrNotFound) {
		t.Fatalf("cross-owner get=%v", e)
	}
}
func TestListCatchesUpAfterOfflineTickAndDismissesOnlyDue(t *testing.T) {
	ctx := context.Background()
	db := &durableState{}
	s := NewStore(db, native.Binding{Community: tid(1), Installation: tid(2)}, func() time.Time { return time.Unix(1200, 0) })
	due := time.Unix(1200, 0)
	r, e := s.Create(ctx, tid(8), tid(4), [32]byte{1}, Reminder{Text: "x", Timezone: "UTC"}, due)
	if e != nil {
		t.Fatal(e)
	}
	rows, _, e := s.List(ctx, tid(4), false, "", due)
	if e != nil || len(rows) != 1 || rows[0].State != StateDue || rows[0].ID != r.ID {
		t.Fatalf("catchup rows=%+v err=%v", rows, e)
	}
	if _, e = s.Dismiss(ctx, r.ID, tid(4), tid(9), [32]byte{9}, 1, due); e != nil {
		t.Fatal(e)
	}
	rows, _, e = s.List(ctx, tid(4), true, "", due)
	if e != nil || len(rows) != 0 {
		t.Fatalf("due after dismiss=%+v err=%v", rows, e)
	}
}

func TestPagesUseUserBoundCursorAndIncludeEveryOutstanding(t *testing.T) {
	ctx := context.Background()
	s := NewStore(&durableState{}, native.Binding{Community: tid(1), Installation: tid(2)})
	now := time.Unix(1000, 0)
	for i := byte(20); i < 45; i++ {
		if _, e := s.Create(ctx, tid(i), tid(4), [32]byte{i}, Reminder{Text: "page", Timezone: "UTC"}, now.Add(time.Duration(i)*time.Minute)); e != nil {
			t.Fatal(e)
		}
	}
	cursor := ""
	count := 0
	for {
		rows, next, e := s.List(ctx, tid(4), false, cursor, now)
		if e != nil {
			t.Fatal(e)
		}
		count += len(rows)
		if next == "" {
			break
		}
		cursor = next
	}
	if count != 25 {
		t.Fatalf("paged rows=%d want 25", count)
	}
	if _, _, e := s.List(ctx, tid(5), false, cursor, now); !errors.Is(e, ErrInvalid) {
		t.Fatalf("foreign cursor err=%v", e)
	}
}
func TestTickPersistsOfflineDueState(t *testing.T) {
	ctx := context.Background()
	db := &durableState{}
	s := NewStore(db, native.Binding{Community: tid(1), Installation: tid(2)}, func() time.Time { return time.Unix(1200, 0) })
	due := time.Unix(1200, 0)
	r, e := s.Create(ctx, tid(8), tid(4), [32]byte{1}, Reminder{Text: "tick", Timezone: "UTC"}, due)
	if e != nil {
		t.Fatal(e)
	}
	if e = s.Tick(ctx); e != nil {
		t.Fatal(e)
	}
	got, e := s.Get(ctx, r.ID, tid(4))
	if e != nil || got.State != StateDue || got.Revision != 2 {
		t.Fatalf("tick reminder=%+v err=%v", got, e)
	}
}
func TestDismissRequiresDueAndCancelledCannotBeDismissed(t *testing.T) {
	ctx := context.Background()
	s := NewStore(&durableState{}, native.Binding{Community: tid(1), Installation: tid(2)})
	now := time.Unix(1000, 0)
	due := now.Add(time.Hour)
	r, e := s.Create(ctx, tid(8), tid(4), [32]byte{1}, Reminder{Text: "x", Timezone: "UTC"}, due)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.Dismiss(ctx, r.ID, tid(4), tid(9), [32]byte{9}, 1, now); !errors.Is(e, ErrConflict) {
		t.Fatalf("early dismiss=%v", e)
	}
	if _, e = s.Cancel(ctx, r.ID, tid(4), tid(10), [32]byte{10}, 1, due); e != nil {
		t.Fatalf("late cancel=%v", e)
	}
	if _, e = s.Dismiss(ctx, r.ID, tid(4), tid(11), [32]byte{11}, 2, due); !errors.Is(e, ErrConflict) {
		t.Fatalf("due dismiss=%v", e)
	}
}

func TestConcurrentDueDismissalsCommitOnce(t *testing.T) {
	ctx := context.Background()
	s := NewStore(&durableState{}, native.Binding{Community: tid(1), Installation: tid(2)})
	now := time.Unix(1000, 0)
	due := now.Add(time.Hour)
	r, e := s.Create(ctx, tid(8), tid(4), [32]byte{1}, Reminder{Text: "race", Timezone: "UTC"}, due)
	if e != nil {
		t.Fatal(e)
	}
	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for i := byte(9); i < 11; i++ {
		wg.Add(1)
		go func(i byte) {
			defer wg.Done()
			<-start
			_, err := s.Dismiss(ctx, r.ID, tid(4), tid(i), [32]byte{i}, 1, due)
			errs <- err
		}(i)
	}
	close(start)
	wg.Wait()
	close(errs)
	wins := 0
	for err := range errs {
		if err == nil {
			wins++
		} else if !errors.Is(err, ErrConflict) {
			t.Fatalf("unexpected dismissal error: %v", err)
		}
	}
	if wins != 1 {
		t.Fatalf("successful dismissals=%d want 1", wins)
	}
	got, e := s.Get(ctx, r.ID, tid(4))
	if e != nil || got.State != StateDismissed {
		t.Fatalf("final reminder=%+v err=%v", got, e)
	}
}

func TestStaleReminderSnapshotCannotCommitAfterConcurrentTick(t *testing.T) {
	for _, tc := range []struct {
		name string
		op   string
	}{{"cancel", "cancel"}, {"dismiss", "dismiss"}} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			db := &durableState{}
			now := time.Unix(1000, 0)
			due := now.Add(time.Minute)
			s := NewStore(db, native.Binding{Community: tid(1), Installation: tid(2)}, func() time.Time { return due })
			r, e := s.Create(ctx, tid(8), tid(4), [32]byte{1}, Reminder{Text: "race", Timezone: "UTC"}, due)
			if e != nil {
				t.Fatal(e)
			}
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			db.getHook = func(c context.Context, key []byte, _ native.StateValue) {
				if c.Value(pauseGetKey{}) == true && string(key) == string(reminderKey(r.ID)) {
					once.Do(func() { close(entered); <-release })
				}
			}
			result := make(chan error, 1)
			go func() {
				pause := context.WithValue(ctx, pauseGetKey{}, true)
				var err error
				if tc.op == "cancel" {
					_, err = s.Cancel(pause, r.ID, tid(4), tid(9), [32]byte{9}, 1, now)
				} else {
					_, err = s.Dismiss(pause, r.ID, tid(4), tid(9), [32]byte{9}, 1, due)
				}
				result <- err
			}()
			<-entered
			if e = s.Tick(ctx); e != nil {
				t.Fatal(e)
			}
			close(release)
			if e = <-result; !errors.Is(e, ErrConflict) {
				t.Fatalf("stale mutation error=%v", e)
			}
			got, e := s.Get(ctx, r.ID, tid(4))
			if e != nil || got.State != StateDue || got.Revision != 2 {
				t.Fatalf("post-race row=%+v err=%v", got, e)
			}
		})
	}
}
func TestTwoDismissalsUsingSameSnapshotHaveOneWinner(t *testing.T) {
	ctx := context.Background()
	db := &durableState{}
	now := time.Unix(1000, 0)
	due := now.Add(time.Minute)
	s := NewStore(db, native.Binding{Community: tid(1), Installation: tid(2)})
	r, e := s.Create(ctx, tid(8), tid(4), [32]byte{1}, Reminder{Text: "race", Timezone: "UTC"}, due)
	if e != nil {
		t.Fatal(e)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	db.getHook = func(c context.Context, key []byte, _ native.StateValue) {
		if c.Value(pauseGetKey{}) == true && string(key) == string(reminderKey(r.ID)) {
			once.Do(func() { close(entered); <-release })
		}
	}
	first := make(chan error, 1)
	go func() {
		pause := context.WithValue(ctx, pauseGetKey{}, true)
		_, err := s.Dismiss(pause, r.ID, tid(4), tid(9), [32]byte{9}, 1, due)
		first <- err
	}()
	<-entered
	if _, e = s.Dismiss(ctx, r.ID, tid(4), tid(10), [32]byte{10}, 1, due); e != nil {
		t.Fatal(e)
	}
	close(release)
	if e = <-first; !errors.Is(e, ErrConflict) {
		t.Fatalf("stale dismissal error=%v", e)
	}
	got, e := s.Get(ctx, r.ID, tid(4))
	if e != nil || got.State != StateDismissed || got.Revision != 2 {
		t.Fatalf("final row=%+v err=%v", got, e)
	}
}

func TestCancelScheduledOrDueReplaysAndReleasesQuotaOnce(t *testing.T) {
	for _, state := range []string{"scheduled", "crossing", "due"} {
		t.Run(state, func(t *testing.T) {
			ctx := t.Context()
			s := NewStore(&durableState{}, native.Binding{Community: tid(1), Installation: tid(2)})
			now, due := time.Unix(1000, 0), time.Unix(1060, 0)
			for i := byte(10); i < 110; i++ {
				if _, err := s.Create(ctx, tid(i), tid(4), [32]byte{i}, Reminder{Text: "x", Timezone: "UTC"}, due); err != nil {
					t.Fatal(err)
				}
			}
			if state != "scheduled" {
				now = due
			}
			if state == "due" {
				s.Now = func() time.Time { return now }
				if err := s.Tick(ctx); err != nil {
					t.Fatal(err)
				}
			}
			row, err := s.Get(ctx, tid(10), tid(4))
			if err != nil {
				t.Fatal(err)
			}
			if state == "due" && row.State != StateDue {
				t.Fatalf("not persisted due: %+v", row)
			}
			if _, err = s.Cancel(ctx, row.ID, tid(5), tid(200), [32]byte{200}, row.Revision, now); !errors.Is(err, ErrNotFound) {
				t.Fatalf("foreign cancel: %v", err)
			}
			accepted, err := s.Cancel(ctx, row.ID, tid(4), tid(201), [32]byte{201}, row.Revision, now)
			if err != nil || accepted.State != StateCancelled || accepted.Revision != row.Revision+1 {
				t.Fatalf("cancel=%+v error=%v", accepted, err)
			}
			for i := 0; i < 2; i++ {
				replay, err := s.Cancel(ctx, row.ID, tid(4), tid(201), [32]byte{201}, row.Revision, now.Add(time.Hour))
				if err != nil || !reflect.DeepEqual(accepted, replay) {
					t.Fatalf("replay=%+v error=%v", replay, err)
				}
			}
			if _, err = s.Create(ctx, tid(202), tid(4), [32]byte{202}, Reminder{Text: "released", Timezone: "UTC"}, due.Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
			if _, err = s.Create(ctx, tid(203), tid(4), [32]byte{203}, Reminder{Text: "full again", Timezone: "UTC"}, due.Add(time.Hour)); !errors.Is(err, ErrQuota) {
				t.Fatalf("replay released quota twice: %v", err)
			}
		})
	}
}
