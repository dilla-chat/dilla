package announcements

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/bot/native"
	"github.com/dilla-chat/dilla/internal/bot/protocol"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
)

type schedulerNative struct {
	mu        sync.Mutex
	binding   native.Binding
	value     native.StateValue
	failNext  bool
	txReceipt map[id.ID][]uint64
}

func (n *schedulerNative) Binding() native.Binding { return n.binding }
func (n *schedulerNative) StateGet(_ context.Context, _ uint64, _ []byte) (native.StateValue, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.value, nil
}
func (n *schedulerNative) StateTransaction(_ context.Context, tx id.ID, changes []native.StateChange) ([]uint64, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if revisions, ok := n.txReceipt[tx]; ok {
		return append([]uint64(nil), revisions...), nil
	}
	if n.failNext {
		n.failNext = false
		return nil, errors.New("injected state failure")
	}
	if len(changes) != 1 || changes[0].ExpectedRevision != n.value.Revision {
		return nil, errors.New("revision conflict")
	}
	n.value.Revision++
	n.value.Value = append([]byte(nil), changes[0].Value...)
	revisions := []uint64{n.value.Revision}
	if n.txReceipt == nil {
		n.txReceipt = make(map[id.ID][]uint64)
	}
	n.txReceipt[tx] = append([]uint64(nil), revisions...)
	return revisions, nil
}

type schedulerDestination struct {
	group id.ID
	err   error
}

func (d schedulerDestination) ResolveTextGroup(context.Context, id.ID, id.ID) (id.ID, error) {
	return d.group, d.err
}

type schedulerAuthorization bool

func (a schedulerAuthorization) CanAnnounce(context.Context, id.ID) (bool, error) {
	return bool(a), nil
}

type schedulerAuthorizationFunc func(context.Context, id.ID) (bool, error)

func (f schedulerAuthorizationFunc) CanAnnounce(ctx context.Context, dest id.ID) (bool, error) {
	return f(ctx, dest)
}

type schedulerSender struct {
	mu       sync.Mutex
	requests []SendRequest
	lookups  []SendRequest
	started  chan struct{}
	resume   chan struct{}
	preStart chan struct{}
	startNow chan struct{}
	err      error
	onSend   func()
	receipts map[id.ID]protocol.SendReceipt
}

func (s *schedulerSender) SendAnnouncement(_ context.Context, req SendRequest) (protocol.SendReceipt, error) {
	if s.onSend != nil {
		s.onSend()
	}
	if s.preStart != nil {
		close(s.preStart)
		<-s.startNow
	}
	if req.BeforeSend == nil || !req.BeforeSend() {
		return protocol.SendReceipt{}, ErrSendNotStarted
	}
	s.mu.Lock()
	s.requests = append(s.requests, req)
	s.mu.Unlock()
	if s.started != nil {
		close(s.started)
		<-s.resume
	}
	receipt := acceptedReceipt(req)
	s.mu.Lock()
	if s.receipts == nil {
		s.receipts = make(map[id.ID]protocol.SendReceipt)
	}
	s.receipts[req.OperationID] = receipt
	s.receipts[req.AttemptID] = receipt
	s.mu.Unlock()
	return receipt, s.err
}

func (s *schedulerSender) LookupAnnouncement(_ context.Context, req SendRequest) (protocol.SendReceipt, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lookups = append(s.lookups, req)
	receipt, ok := s.receipts[req.AttemptID]
	if !ok {
		receipt, ok = s.receipts[req.OperationID]
	}
	return receipt, ok, nil
}

func acceptedReceipt(req SendRequest) protocol.SendReceipt {
	return protocol.SendReceipt{
		OperationID: req.OperationID, AttemptID: req.AttemptID,
		CommunityID: req.CommunityID, GroupID: req.GroupID,
		CiphertextHash: make([]byte, 32),
		Outcome:        protocol.ReceiptAccepted,
		SendResponse:   &protocol.OriginalSendResponse{Sequence: 1, FrankingTag: make([]byte, 32), ReceiveTimestamp: 1, Body: mustCBOR([]any{uint64(1), make([]byte, 32), uint64(1)})},
	}
}

func schedulerFixture(t *testing.T, now *time.Time) (*Scheduler, *schedulerNative, *schedulerSender, id.ID, id.ID) {
	t.Helper()
	community, destination := schedulerID(1), schedulerID(2)
	n := &schedulerNative{binding: native.Binding{Instance: schedulerID(3), Community: community, Installation: schedulerID(4)}}
	sender := &schedulerSender{}
	s, err := NewScheduler(n, SchedulerOptions{
		Destination: schedulerDestination{group: schedulerID(5)}, Authorization: schedulerAuthorization(true), Sender: sender, ReceiptLookup: sender,
		Now: func() time.Time { return *now },
	})
	if err != nil {
		t.Fatal(err)
	}
	return s, n, sender, community, destination
}

func TestAnnouncementSendRequestTimestampIsStableAcrossRetry(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	s, _, _, _, _ := schedulerFixture(t, &now)
	state := scheduledState{Announcement: Announcement{ID: schedulerID(9)}, Occurrence: Occurrence{
		At: time.Date(2026, 10, 22, 11, 59, 0, 0, time.UTC), Key: "2026-10-22T11:59", Revision: 1,
	}}
	first, err := s.sendRequest(state)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(30 * time.Second)
	second, err := s.sendRequest(state)
	if err != nil {
		t.Fatal(err)
	}
	if first.OriginalNow != second.OriginalNow || first.OriginalNow != uint64(state.Occurrence.At.Unix()) {
		t.Fatalf("retry changed original timestamp: first=%d second=%d", first.OriginalNow, second.OriginalNow)
	}
}

func TestAnnouncementRetryTimeBoundsUnixConversions(t *testing.T) {
	if _, err := announcementRetryAt(time.Unix(-1, 0)); !errors.Is(err, ErrInvalidRetryTime) {
		t.Fatalf("pre-epoch retry clock error = %v", err)
	}
	if !retryNotBefore(5, time.Unix(-1, 0)) || retryNotBefore(0, time.Unix(-1, 0)) {
		t.Fatal("pre-epoch comparison did not preserve zero and positive retry timestamp behavior")
	}
	got, err := announcementRetryAt(time.Unix(10, 0))
	if err != nil || got != 15 {
		t.Fatalf("retry timestamp = %d, err = %v; want 15", got, err)
	}
}

func schedulerID(v byte) id.ID { var out id.ID; out[15] = v; return out }
func mustCBOR(v any) []byte    { b, _ := cborx.Marshal(v); return b }

func dailyAnnouncement(destination id.ID, text string, revision uint64) Announcement {
	return Announcement{ID: schedulerID(8), DestinationID: destination, Text: text,
		Schedule: Schedule{Kind: Daily, Timezone: "UTC", Hour: 10, Revision: revision}, Enabled: true}
}

func TestSchedulerDeleteRequiresCurrentScheduleRevision(t *testing.T) {
	now := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
	s, _, _, _, destination := schedulerFixture(t, &now)
	value := dailyAnnouncement(destination, "private announcement", 1)
	if err := s.Put(context.Background(), value); err != nil {
		t.Fatal(err)
	}
	value.Schedule.Revision = 2
	if err := s.Put(context.Background(), value); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(context.Background(), value.ID, 1); !errors.Is(err, ErrSchedulerConflict) {
		t.Fatalf("stale delete = %v, want conflict", err)
	}
	if rows, err := s.List(context.Background()); err != nil || len(rows) != 1 {
		t.Fatalf("stale delete changed schedules: %#v, %v", rows, err)
	}
	if err := s.Delete(context.Background(), value.ID, 2); err != nil {
		t.Fatal(err)
	}
	if rows, err := s.List(context.Background()); err != nil || len(rows) != 0 {
		t.Fatalf("deleted schedules = %#v, %v", rows, err)
	}
}

func TestSchedulerPutMutationReceiptSurvivesRestartAndEpochRetry(t *testing.T) {
	now := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
	s, n, _, _, destination := schedulerFixture(t, &now)
	requestID := schedulerID(61)
	fingerprint := [32]byte{1, 2, 3}
	expires := uint64(now.Unix() + 20)
	value := dailyAnnouncement(destination, "private announcement", 1)

	first, err := s.PutWithReceipt(context.Background(), requestID, fingerprint, expires, value)
	if err != nil || first.Status != 0 || first.Revision == nil || *first.Revision != 1 || string(first.Data) != string([]byte{0x80}) {
		t.Fatalf("first put result=%+v err=%v", first, err)
	}

	// A restarted scheduler sees the durable receipt. A new MLS epoch changes
	// the event that carries the request, but not its operation ID or request
	// fingerprint, so the original outcome must be returned without a second
	// schedule write.
	restarted, err := NewScheduler(n, SchedulerOptions{
		Destination: schedulerDestination{group: schedulerID(5)}, Authorization: schedulerAuthorization(true),
		Sender: &schedulerSender{}, ReceiptLookup: &schedulerSender{}, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := restarted.PutWithReceipt(context.Background(), requestID, fingerprint, expires, value)
	if err != nil || replayed.Status != first.Status || replayed.Revision == nil || *replayed.Revision != *first.Revision || string(replayed.Data) != string(first.Data) {
		t.Fatalf("epoch retry result=%+v err=%v, want original result %+v", replayed, err, first)
	}
	if n.value.Revision != 1 {
		t.Fatalf("epoch retry wrote scheduler state again: revision=%d", n.value.Revision)
	}
	if _, err := restarted.PutWithReceipt(context.Background(), requestID, [32]byte{9}, expires, value); !errors.Is(err, ErrMutationIDConflict) {
		t.Fatalf("same request ID with changed fingerprint = %v, want conflict", err)
	}
}

func TestSchedulerPutWithReceiptChecksDocumentRevisionAndReplaysReceipt(t *testing.T) {
	now := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
	s, n, _, _, destination := schedulerFixture(t, &now)
	requestID := schedulerID(65)
	fingerprint := [32]byte{13, 14, 15}
	expires := uint64(now.Unix() + 20)
	value := dailyAnnouncement(destination, "first", 1)
	first, err := s.PutWithReceiptAtRevision(context.Background(), 0, requestID, fingerprint, expires, value)
	if err != nil || first.Status != 0 || first.Revision == nil || *first.Revision != 1 || n.value.Revision != 1 {
		t.Fatalf("first put result=%+v state revision=%d err=%v", first, n.value.Revision, err)
	}

	// A changed request with a stale document revision records a stable conflict
	// without replacing the schedule.
	staleID := schedulerID(66)
	stale := dailyAnnouncement(destination, "stale", 1)
	conflict, err := s.PutWithReceiptAtRevision(context.Background(), 0, staleID, [32]byte{1}, expires, stale)
	if err != nil || conflict.Status != 4 || conflict.Revision == nil || *conflict.Revision != 2 {
		t.Fatalf("stale put result=%+v state revision=%d err=%v", conflict, n.value.Revision, err)
	}
	restarted, err := NewScheduler(n, SchedulerOptions{
		Destination: schedulerDestination{group: schedulerID(5)}, Authorization: schedulerAuthorization(true),
		Sender: &schedulerSender{}, ReceiptLookup: &schedulerSender{}, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	replay, err := restarted.PutWithReceiptAtRevision(context.Background(), 0, staleID, [32]byte{1}, expires, stale)
	if err != nil || replay.Status != conflict.Status || replay.Revision == nil || *replay.Revision != *conflict.Revision || n.value.Revision != 2 {
		t.Fatalf("stale retry result=%+v state revision=%d err=%v", replay, n.value.Revision, err)
	}
	rows, err := s.List(context.Background())
	if err != nil || len(rows) != 1 || rows[0].Text != "first" {
		t.Fatalf("stale put changed schedules: %+v err=%v", rows, err)
	}
}

func TestSchedulerListReturnsAuthoritativeDocumentRevisionWithRows(t *testing.T) {
	now := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
	s, _, _, _, destination := schedulerFixture(t, &now)
	if err := s.Put(context.Background(), dailyAnnouncement(destination, "one", 1)); err != nil {
		t.Fatal(err)
	}
	rows, revision, err := s.ListAtRevision(context.Background())
	if err != nil || revision != 1 || len(rows) != 1 || rows[0].Text != "one" {
		t.Fatalf("list rows=%+v revision=%d err=%v, want document revision 1", rows, revision, err)
	}
}

func TestSchedulerDeleteMutationReceiptSurvivesRestart(t *testing.T) {
	now := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
	s, n, _, _, destination := schedulerFixture(t, &now)
	value := dailyAnnouncement(destination, "private announcement", 1)
	if err := s.Put(context.Background(), value); err != nil {
		t.Fatal(err)
	}
	requestID := schedulerID(62)
	fingerprint := [32]byte{4, 5, 6}
	expires := uint64(now.Unix() + 20)
	first, err := s.DeleteWithReceipt(context.Background(), requestID, fingerprint, expires, value.ID, 1)
	if err != nil || first.Status != 0 || first.Revision == nil || *first.Revision != 2 {
		t.Fatalf("first delete result=%+v err=%v", first, err)
	}
	restarted, err := NewScheduler(n, SchedulerOptions{
		Destination: schedulerDestination{group: schedulerID(5)}, Authorization: schedulerAuthorization(true),
		Sender: &schedulerSender{}, ReceiptLookup: &schedulerSender{}, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := restarted.DeleteWithReceipt(context.Background(), requestID, fingerprint, expires, value.ID, 1)
	if err != nil || replayed.Status != first.Status || replayed.Revision == nil || *replayed.Revision != *first.Revision || string(replayed.Data) != string(first.Data) || n.value.Revision != 2 {
		t.Fatalf("delete retry result=%+v state revision=%d err=%v", replayed, n.value.Revision, err)
	}
}

func TestSchedulerMutationReceiptReplaysOriginalConflictAfterStateChanges(t *testing.T) {
	now := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
	s, _, _, _, destination := schedulerFixture(t, &now)
	value := dailyAnnouncement(destination, "revision one", 1)
	if err := s.Put(context.Background(), value); err != nil {
		t.Fatal(err)
	}
	requestID := schedulerID(63)
	fingerprint := [32]byte{7, 8, 9}
	expires := uint64(now.Unix() + 20)
	stale := dailyAnnouncement(destination, "revision three", 3)
	first, err := s.PutWithReceipt(context.Background(), requestID, fingerprint, expires, stale)
	if err != nil || first.Status != 4 || first.Revision == nil || *first.Revision != 2 {
		t.Fatalf("first stale put result=%+v err=%v, want conflict", first, err)
	}
	updated := dailyAnnouncement(destination, "revision two", 2)
	if err := s.Put(context.Background(), updated); err != nil {
		t.Fatal(err)
	}
	retry, err := s.PutWithReceipt(context.Background(), requestID, fingerprint, expires, stale)
	if err != nil || retry.Status != first.Status || retry.Revision == nil || *retry.Revision != *first.Revision || string(retry.Data) != string(first.Data) {
		t.Fatalf("retry result=%+v err=%v, want original conflict %+v", retry, err, first)
	}
	rows, err := s.List(context.Background())
	if err != nil || len(rows) != 1 || rows[0].Schedule.Revision != 2 || rows[0].Text != "revision two" {
		t.Fatalf("conflict retry changed schedule: rows=%+v err=%v", rows, err)
	}
}

func TestSchedulerMutationReceiptReuseAfterExpiryIgnoresOldFingerprint(t *testing.T) {
	for _, tt := range []struct {
		name             string
		reuseFingerprint bool
	}{
		{name: "same fingerprint", reuseFingerprint: true},
		{name: "changed fingerprint", reuseFingerprint: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			now := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
			s, n, _, _, destination := schedulerFixture(t, &now)
			requestID := schedulerID(64)
			fingerprint := [32]byte{10, 11, 12}
			if err := s.Put(context.Background(), dailyAnnouncement(destination, "revision one", 1)); err != nil {
				t.Fatal(err)
			}
			first, err := s.PutWithReceipt(context.Background(), requestID, fingerprint, uint64(now.Unix()+1), dailyAnnouncement(destination, "receipt result", 2))
			if err != nil || first.Status != 0 || first.Revision == nil || *first.Revision != 2 {
				t.Fatalf("first mutation result=%+v err=%v", first, err)
			}

			now = now.Add(2 * time.Second)
			newFingerprint := fingerprint
			if !tt.reuseFingerprint {
				newFingerprint = [32]byte{13, 14, 15}
			}
			second, err := s.PutWithReceipt(context.Background(), requestID, newFingerprint, uint64(now.Unix()+20), dailyAnnouncement(destination, "revision three", 3))
			if err != nil || second.Status != 0 || second.Revision == nil || *second.Revision != 3 {
				t.Fatalf("reused request ID result=%+v err=%v, want new revision 3", second, err)
			}
			states, receipts, err := decodeStateDocument(n.value.Value)
			if err != nil || len(states) != 1 || states[0].Announcement.Schedule.Revision != 3 || states[0].Announcement.Text != "revision three" {
				t.Fatalf("state after expired receipt reuse=%+v err=%v", states, err)
			}
			if len(receipts) != 1 || receipts[0].RequestID != requestID || receipts[0].Fingerprint != newFingerprint || receipts[0].Expires != uint64(now.Unix()+20) {
				t.Fatalf("expired receipt was not pruned/replaced: %+v", receipts)
			}
		})
	}
}

func TestSchedulerLateOccurrenceSendsOnceWithStableOperationAcrossRestart(t *testing.T) {
	now := time.Date(2026, 10, 22, 9, 59, 0, 0, time.UTC)
	s, n, sender, _, destination := schedulerFixture(t, &now)
	if err := s.Put(context.Background(), dailyAnnouncement(destination, "private announcement", 1)); err != nil {
		t.Fatal(err)
	}
	now = time.Date(2026, 10, 22, 10, 10, 0, 0, time.UTC)
	if err := s.RunDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(sender.requests) != 1 {
		t.Fatalf("send calls = %d, want 1", len(sender.requests))
	}
	first := sender.requests[0]
	if first.Text != "private announcement" || first.OperationID.IsZero() || first.AttemptID.IsZero() {
		t.Fatalf("invalid send request: %+v", first)
	}
	if len(n.value.Value) == 0 {
		t.Fatal("accepted state was not persisted")
	}

	restarted, _, restartedSender, _, _ := schedulerFixture(t, &now)
	restarted.native = n
	if err := restarted.RunDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(restartedSender.requests) != 0 {
		t.Fatalf("accepted occurrence resent %d times", len(restartedSender.requests))
	}
}

func TestSchedulerSkipsOccurrenceMoreThanFifteenMinutesLate(t *testing.T) {
	now := time.Date(2026, 10, 22, 9, 59, 0, 0, time.UTC)
	s, _, sender, _, destination := schedulerFixture(t, &now)
	if err := s.Put(context.Background(), dailyAnnouncement(destination, "skip me", 1)); err != nil {
		t.Fatal(err)
	}
	now = time.Date(2026, 10, 22, 10, 16, 0, 0, time.UTC)
	if err := s.RunDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(sender.requests) != 0 {
		t.Fatalf("sent a stale occurrence: %d calls", len(sender.requests))
	}
}

func TestSchedulerSendsDueAnnouncement(t *testing.T) {
	now := time.Date(2026, 10, 22, 9, 59, 0, 0, time.UTC)
	s, _, sender, _, destination := schedulerFixture(t, &now)
	if err := s.Put(context.Background(), dailyAnnouncement(destination, "authorized", 1)); err != nil {
		t.Fatal(err)
	}
	now = time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC)
	if err := s.RunDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(sender.requests) != 1 {
		t.Fatalf("send calls = %d, want 1", len(sender.requests))
	}
}

func TestSchedulerResolvesChannelToMLSGroupBeforeSending(t *testing.T) {
	now := time.Date(2026, 10, 22, 9, 59, 0, 0, time.UTC)
	s, _, sender, community, channel := schedulerFixture(t, &now)
	group := schedulerID(5)
	if group == channel {
		t.Fatal("test requires distinct channel and group IDs")
	}
	if err := s.Put(context.Background(), dailyAnnouncement(channel, "authorized", 1)); err != nil {
		t.Fatal(err)
	}
	now = time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC)
	if err := s.RunDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(sender.requests) != 1 || sender.requests[0].CommunityID != community || sender.requests[0].GroupID != group {
		t.Fatalf("send did not use the resolved MLS group: %+v", sender.requests)
	}
}

func TestSchedulerFailsClosedWhenDestinationHasNoMLSGroup(t *testing.T) {
	now := time.Date(2026, 10, 22, 9, 59, 0, 0, time.UTC)
	s, _, sender, _, channel := schedulerFixture(t, &now)
	s.destination = schedulerDestination{}
	if err := s.Put(context.Background(), dailyAnnouncement(channel, "must not send", 1)); err != nil {
		t.Fatal(err)
	}
	now = time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC)
	if err := s.RunDue(context.Background()); err == nil {
		t.Fatal("expected unavailable destination error")
	}
	if len(sender.requests) != 0 || len(sender.lookups) != 0 {
		t.Fatalf("destination failure reached sender or receipt lookup: sends=%+v lookups=%+v", sender.requests, sender.lookups)
	}
}

func TestSchedulerRecoversAcceptedReceiptAfterLostStateCommit(t *testing.T) {
	now := time.Date(2026, 10, 22, 9, 59, 0, 0, time.UTC)
	s, n, sender, _, destination := schedulerFixture(t, &now)
	if err := s.Put(context.Background(), dailyAnnouncement(destination, "recover accepted", 1)); err != nil {
		t.Fatal(err)
	}
	now = time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC)
	sender.onSend = func() { n.mu.Lock(); n.failNext = true; n.mu.Unlock() }
	if err := s.RunDue(context.Background()); err == nil {
		t.Fatal("expected state commit failure after accepted send")
	}
	first := sender.requests[0]
	now = time.Date(2026, 10, 22, 10, 16, 0, 0, time.UTC)
	restarted, _, retry, _, _ := schedulerFixture(t, &now)
	restarted.native = n
	restarted.receiptLookup = sender
	restarted.destination = schedulerDestination{err: errors.New("channel was deleted after send")}
	if err := restarted.RunDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(retry.requests) != 0 || len(sender.lookups) != 2 || sender.lookups[1].OperationID != first.OperationID || sender.lookups[1].AttemptID != first.AttemptID || sender.lookups[1].GroupID != first.GroupID {
		t.Fatalf("accepted send recovery did not resolve same IDs: first=%+v retries=%+v lookups=%+v", first, retry.requests, sender.lookups)
	}
}

func TestSchedulerRecoversAcceptedReceiptInWindowAfterDestinationDeletion(t *testing.T) {
	now := time.Date(2026, 10, 22, 9, 59, 0, 0, time.UTC)
	s, n, sender, _, destination := schedulerFixture(t, &now)
	if err := s.Put(context.Background(), dailyAnnouncement(destination, "recover before retry", 1)); err != nil {
		t.Fatal(err)
	}
	now = time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC)
	sender.onSend = func() { n.mu.Lock(); n.failNext = true; n.mu.Unlock() }
	if err := s.RunDue(context.Background()); err == nil {
		t.Fatal("expected state commit failure after accepted send")
	}
	first := sender.requests[0]
	now = time.Date(2026, 10, 22, 10, 5, 0, 0, time.UTC)
	restarted, _, retry, _, _ := schedulerFixture(t, &now)
	restarted.native = n
	restarted.receiptLookup = sender
	restarted.destination = schedulerDestination{err: errors.New("channel was deleted after send")}
	if err := restarted.RunDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(retry.requests) != 0 || len(sender.lookups) != 2 || sender.lookups[1].GroupID != first.GroupID || sender.lookups[1].OperationID != first.OperationID {
		t.Fatalf("accepted receipt was not recovered before destination validation: first=%+v retries=%+v lookups=%+v", first, retry.requests, sender.lookups)
	}
}

func TestSchedulerReportsUnresolvedLegacySendWhenDestinationIsMissing(t *testing.T) {
	now := time.Date(2026, 10, 22, 9, 59, 0, 0, time.UTC)
	s, n, sender, _, destination := schedulerFixture(t, &now)
	if err := s.Put(context.Background(), dailyAnnouncement(destination, "legacy pending", 1)); err != nil {
		t.Fatal(err)
	}
	states, err := decodeStates(n.value.Value)
	if err != nil {
		t.Fatal(err)
	}
	states[0].Status = occurrenceSending
	body, err := encodeStates(states)
	if err != nil {
		t.Fatal(err)
	}
	n.mu.Lock()
	n.value.Revision++
	n.value.Value = body
	n.mu.Unlock()
	s.destination = schedulerDestination{err: errors.New("channel was deleted before group ID was persisted")}
	now = time.Date(2026, 10, 22, 10, 5, 0, 0, time.UTC)
	err = s.RunDue(context.Background())
	if err == nil || !strings.Contains(err.Error(), "legacy announcement receipt recovery") {
		t.Fatalf("missing destination did not produce an unresolved legacy receipt error: %v", err)
	}
	if len(sender.requests) != 0 || len(sender.lookups) != 0 {
		t.Fatalf("legacy unresolved send reached sender or lookup without a group ID: sends=%+v lookups=%+v", sender.requests, sender.lookups)
	}
}

func TestSchedulerDoesNotSendAfterPermissionLossOrDeletedDestination(t *testing.T) {
	for _, tc := range []struct {
		name               string
		available, allowed bool
	}{{"permission lost", true, false}, {"destination deleted", false, true}} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Date(2026, 10, 22, 9, 59, 0, 0, time.UTC)
			s, _, sender, _, destination := schedulerFixture(t, &now)
			if !tc.available {
				s.destination = schedulerDestination{err: errors.New("destination unavailable")}
			}
			s.authorization = schedulerAuthorizationFunc(func(context.Context, id.ID) (bool, error) { return tc.allowed, nil })
			if err := s.Put(context.Background(), dailyAnnouncement(destination, "should not send", 1)); err != nil {
				t.Fatal(err)
			}
			now = time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC)
			err := s.RunDue(context.Background())
			if tc.available && err != nil {
				t.Fatal(err)
			}
			if len(sender.requests) != 0 {
				t.Fatalf("unauthorized send count=%d", len(sender.requests))
			}
		})
	}
}

func TestSchedulerRechecksLateWindowAtSendStartAfterSlowAuthorization(t *testing.T) {
	now := time.Date(2026, 10, 22, 9, 59, 0, 0, time.UTC)
	s, _, sender, _, destination := schedulerFixture(t, &now)
	s.authorization = schedulerAuthorizationFunc(func(context.Context, id.ID) (bool, error) {
		now = time.Date(2026, 10, 22, 10, 16, 0, 0, time.UTC)
		return true, nil
	})
	if err := s.Put(context.Background(), dailyAnnouncement(destination, "too late after policy", 1)); err != nil {
		t.Fatal(err)
	}
	now = time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC)
	if err := s.RunDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(sender.requests) != 0 {
		t.Fatalf("send began after late window closed: %+v", sender.requests)
	}
}

func TestSchedulerRetriesUncertainSendWithSameOperationID(t *testing.T) {
	now := time.Date(2026, 10, 22, 9, 59, 0, 0, time.UTC)
	s, n, sender, _, destination := schedulerFixture(t, &now)
	sender.err = errors.New("reply lost")
	if err := s.Put(context.Background(), dailyAnnouncement(destination, "recover me", 1)); err != nil {
		t.Fatal(err)
	}
	now = time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC)
	if err := s.RunDue(context.Background()); err == nil {
		t.Fatal("expected uncertain send error")
	}
	first := sender.requests[0]
	restarted, _, retry, _, _ := schedulerFixture(t, &now)
	restarted.native = n
	if err := restarted.RunDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(retry.requests) != 1 || retry.requests[0].OperationID != first.OperationID || retry.requests[0].AttemptID != first.AttemptID {
		t.Fatalf("retry did not reuse durable IDs: first=%+v retry=%+v", first, retry.requests)
	}
}

func TestSchedulerStartsNewAttemptAfterClosedNotAcceptedReceipt(t *testing.T) {
	now := time.Date(2026, 10, 22, 9, 59, 0, 0, time.UTC)
	s, _, sender, community, destination := schedulerFixture(t, &now)
	if err := s.Put(context.Background(), dailyAnnouncement(destination, "retry closed attempt", 1)); err != nil {
		t.Fatal(err)
	}
	if err := s.RunDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	now = time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC)
	states, err := decodeStates(s.native.(*schedulerNative).value.Value)
	if err != nil {
		t.Fatal(err)
	}
	states[0].Status = occurrenceSending
	states[0].GroupID = schedulerID(5)
	req, err := s.sendRequest(states[0])
	if err != nil {
		t.Fatal(err)
	}
	first := req
	closed := protocol.SendReceipt{OperationID: first.OperationID, AttemptID: first.AttemptID, CommunityID: community, GroupID: first.GroupID, ExpectedGeneration: 7, CiphertextHash: make([]byte, 32), Outcome: protocol.ReceiptClosedNotAccepted}
	sender.mu.Lock()
	if sender.receipts == nil {
		sender.receipts = make(map[id.ID]protocol.SendReceipt)
	}
	sender.receipts[first.AttemptID] = closed
	sender.mu.Unlock()
	if err := s.replaceOne(context.Background(), states[0], 1); err != nil {
		t.Fatal(err)
	}
	if err := s.RunDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	restarted, _, retrySender, _, _ := schedulerFixture(t, &now)
	restarted.native = s.native
	restarted.receiptLookup = sender
	if err := restarted.RunDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(retrySender.requests) != 0 {
		t.Fatalf("retry bypassed durable not-before time: requests=%+v", retrySender.requests)
	}
	now = now.Add(5 * time.Second)
	if err := restarted.RunDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(retrySender.requests) != 1 {
		t.Fatalf("closed attempt did not trigger a new send after restart: requests=%+v", retrySender.requests)
	}
	second := retrySender.requests[0]
	if second.OperationID != first.OperationID || second.AttemptID == first.AttemptID || second.PredecessorAttemptID == nil || *second.PredecessorAttemptID != first.AttemptID {
		t.Fatalf("retry did not preserve operation and chain to closed attempt: first=%+v second=%+v", first, second)
	}
}

func TestSchedulerResetsRetryAttemptForNextRecurringOccurrence(t *testing.T) {
	now := time.Date(2026, 10, 22, 9, 59, 0, 0, time.UTC)
	s, _, sender, _, destination := schedulerFixture(t, &now)
	if err := s.Put(context.Background(), dailyAnnouncement(destination, "next occurrence", 1)); err != nil {
		t.Fatal(err)
	}
	now = time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC)
	if err := s.RunDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	states, revision, err := s.snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	states[0].Attempt = 1
	if err := s.replaceOne(context.Background(), states[0], revision); err != nil {
		t.Fatal(err)
	}
	now = time.Date(2026, 10, 23, 10, 0, 0, 0, time.UTC)
	if err := s.RunDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(sender.requests) != 2 {
		t.Fatalf("expected next recurring occurrence to send, got %d requests", len(sender.requests))
	}
	second := sender.requests[1]
	if second.AttemptID == sender.requests[0].AttemptID || second.PredecessorAttemptID != nil {
		t.Fatalf("next occurrence inherited prior retry chain: previous=%+v next=%+v", sender.requests[0], second)
	}
	if persisted, _, err := s.snapshot(context.Background()); err != nil || persisted[0].Attempt != 0 || persisted[0].RetryAt != 0 {
		t.Fatalf("next occurrence attempt counter was not reset: state=%+v err=%v", persisted, err)
	}
}

func TestSchedulerStateSaveDoesNotReplayOldTransactionReceiptAsCurrentSuccess(t *testing.T) {
	now := time.Date(2026, 10, 22, 9, 59, 0, 0, time.UTC)
	s, _, _, _, destination := schedulerFixture(t, &now)
	if err := s.Put(context.Background(), dailyAnnouncement(destination, "revision one", 1)); err != nil {
		t.Fatal(err)
	}
	updated := dailyAnnouncement(destination, "revision two", 2)
	updated.Schedule.Hour = 11
	if err := s.Put(context.Background(), updated); err != nil {
		t.Fatal(err)
	}
	statesAtRevisionTwo, expectedRevision, err := s.snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if expectedRevision != 2 {
		t.Fatalf("revision after edit = %d, want 2", expectedRevision)
	}
	other := dailyAnnouncement(destination, "independent change", 1)
	other.ID = schedulerID(9)
	if err := s.Put(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	if err := s.save(context.Background(), expectedRevision-1, statesAtRevisionTwo, s.receipts); !errors.Is(err, ErrSchedulerConflict) {
		t.Fatalf("stale identical save error = %v, want scheduler conflict", err)
	}
	current, currentRevision, err := s.snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if currentRevision != 3 || len(current) != 2 || current[1].Announcement.Text != "independent change" {
		t.Fatalf("stale save changed current state: revision=%d state=%+v", currentRevision, current)
	}
}

func TestSchedulerDoesNotRetryWhenDestinationMLSGroupChanges(t *testing.T) {
	now := time.Date(2026, 10, 22, 9, 59, 0, 0, time.UTC)
	s, n, sender, _, channel := schedulerFixture(t, &now)
	sender.err = errors.New("reply lost")
	if err := s.Put(context.Background(), dailyAnnouncement(channel, "recover me", 1)); err != nil {
		t.Fatal(err)
	}
	now = time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC)
	if err := s.RunDue(context.Background()); err == nil {
		t.Fatal("expected uncertain send error")
	}
	first := sender.requests[0]
	restarted, _, retry, _, _ := schedulerFixture(t, &now)
	restarted.native = n
	restarted.destination = schedulerDestination{group: schedulerID(6)}
	if err := restarted.RunDue(context.Background()); err == nil {
		t.Fatal("expected changed destination group error")
	}
	if len(retry.requests) != 0 || len(retry.lookups) != 1 || retry.lookups[0].GroupID != first.GroupID || first.GroupID == schedulerID(6) {
		t.Fatalf("changed destination was used for retry or receipt lookup: first=%+v retries=%+v lookups=%+v", first, retry.requests, retry.lookups)
	}
}

func TestSchedulerEditDoesNotHoldLockAcrossNetworkAndInvalidatesUnstartedOccurrence(t *testing.T) {
	now := time.Date(2026, 10, 22, 9, 59, 0, 0, time.UTC)
	s, _, sender, _, destination := schedulerFixture(t, &now)
	sender.started, sender.resume = make(chan struct{}), make(chan struct{})
	if err := s.Put(context.Background(), dailyAnnouncement(destination, "old text", 1)); err != nil {
		t.Fatal(err)
	}
	now = time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC)
	runDone := make(chan error, 1)
	go func() { runDone <- s.RunDue(context.Background()) }()
	<-sender.started
	updated := dailyAnnouncement(destination, "new text", 2)
	updated.Schedule.Hour = 11
	putDone := make(chan error, 1)
	go func() { putDone <- s.Put(context.Background(), updated) }()
	select {
	case err := <-putDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		close(sender.resume)
		t.Fatal("schedule edit blocked behind network send")
	}
	close(sender.resume)
	if err := <-runDone; err != nil {
		t.Fatal(err)
	}
	if len(sender.requests) != 1 || sender.requests[0].Text != "old text" {
		t.Fatalf("started send changed unexpectedly: %+v", sender.requests)
	}
	if err := s.RunDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(sender.requests) != 1 {
		t.Fatalf("unstarted old occurrence sent after edit: %d", len(sender.requests))
	}
}

func TestSchedulerEditBeforeSendStartGateCancelsOldOccurrence(t *testing.T) {
	now := time.Date(2026, 10, 22, 9, 59, 0, 0, time.UTC)
	s, _, sender, _, destination := schedulerFixture(t, &now)
	sender.preStart, sender.startNow = make(chan struct{}), make(chan struct{})
	if err := s.Put(context.Background(), dailyAnnouncement(destination, "old text", 1)); err != nil {
		t.Fatal(err)
	}
	now = time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC)
	runDone := make(chan error, 1)
	go func() { runDone <- s.RunDue(context.Background()) }()
	<-sender.preStart
	updated := dailyAnnouncement(destination, "new text", 2)
	updated.Schedule.Hour = 11
	if err := s.Put(context.Background(), updated); err != nil {
		t.Fatal(err)
	}
	close(sender.startNow)
	if err := <-runDone; err != nil {
		t.Fatal(err)
	}
	if len(sender.requests) != 0 {
		t.Fatalf("old occurrence began after revision update: %+v", sender.requests)
	}
}

func TestSchedulerAggregateStateNeverExceedsNativeStateLimit(t *testing.T) {
	now := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
	s, n, _, _, destination := schedulerFixture(t, &now)
	text := strings.Repeat("x", maxAnnouncementText)
	var putErr error
	for i := 1; i <= maxAnnouncements; i++ {
		announcement := dailyAnnouncement(destination, text, 1)
		announcement.ID = schedulerID(byte(i))
		putErr = s.Put(context.Background(), announcement)
		if putErr != nil {
			break
		}
	}
	if putErr == nil {
		t.Fatal("aggregate schedules exceeded native state limit without error")
	}
	if len(n.value.Value) > 262144 {
		t.Fatalf("persisted state is %d bytes, exceeds native 262144-byte limit", len(n.value.Value))
	}
}
