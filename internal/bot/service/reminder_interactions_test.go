package service

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jonasthim/dilla/internal/bot/helper"
	"github.com/jonasthim/dilla/internal/bot/native"
	"github.com/jonasthim/dilla/internal/bot/protocol"
	"github.com/jonasthim/dilla/internal/bot/reminders"
	"github.com/jonasthim/dilla/internal/cborx"
	"github.com/jonasthim/dilla/internal/id"
)

type reminderStateFake struct {
	mu    sync.Mutex
	rows  map[string]native.StateEntry
	txErr error
}

func (s *reminderStateFake) StateGet(_ context.Context, _ uint64, k []byte) (native.StateValue, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.rows[string(k)]
	return native.StateValue{Revision: e.Revision, Value: e.Value, Tombstone: e.Tombstone}, nil
}
func (s *reminderStateFake) StateScan(_ context.Context, _ uint64, after []byte, limit uint64) (native.StatePage, error) {
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
func (s *reminderStateFake) StateTransaction(_ context.Context, _ id.ID, cs []native.StateChange) ([]uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.txErr != nil {
		return nil, s.txErr
	}
	if s.rows == nil {
		s.rows = map[string]native.StateEntry{}
	}
	if len(cs) > 64 {
		return nil, errors.New("tx too large")
	}
	for _, c := range cs {
		if s.rows[string(c.Key)].Revision != c.ExpectedRevision {
			return nil, reminders.ErrConflict
		}
	}
	out := make([]uint64, len(cs))
	for i, c := range cs {
		v := s.rows[string(c.Key)]
		v.Key = append([]byte(nil), c.Key...)
		v.Revision++
		v.Value = append([]byte(nil), c.Value...)
		s.rows[string(c.Key)] = v
		out[i] = v.Revision
	}
	return out, nil
}

type reminderAuthorityFake struct {
	member bool
	source bool
	checks int
}

func (a *reminderAuthorityFake) ReminderMember(context.Context, id.ID) (bool, error) {
	a.checks++
	return a.member, nil
}
func (a *reminderAuthorityFake) ReminderSource(context.Context, id.ID, id.ID) (bool, error) {
	a.checks++
	return a.source, nil
}
func reminderTestID(n byte) (v id.ID) { v[15] = n; return }
func newReminderTest(now *time.Time) (ReminderInteractions, *reminderAuthorityFake, *reminderStateFake) {
	auth := &reminderAuthorityFake{member: true, source: true}
	state := &reminderStateFake{}
	store := reminders.NewStore(state, native.Binding{Community: reminderTestID(1), Installation: reminderTestID(2)})
	return ReminderInteractions{Store: store, Authority: auth, CommunityID: reminderTestID(1), Now: func() time.Time { return *now }}, auth, state
}
func TestReminderCreateReplayAcrossDevicesKeepsOriginalDeadline(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(1000, 0).UTC()
	h, a, _ := newReminderTest(&now)
	payload, _ := cborx.Marshal([]any{uint64(1), uint64(0), uint64(60), "call", "UTC", nil, nil})
	rev := uint64(0)
	expires := uint64(1100)
	r := protocol.Request{RequestID: reminderTestID(8), CommunityID: reminderTestID(1), Operation: protocol.OpReminderCreate, Revision: &rev, Expires: &expires, Args: []any{payload}}
	one, e := h.create(ctx, InteractionActor{User: reminderTestID(4), Device: reminderTestID(5)}, r)
	if e != nil || one.Status != 0 {
		t.Fatalf("create=%+v err=%v", one, e)
	}
	now = now.Add(time.Hour)
	two, e := h.create(ctx, InteractionActor{User: reminderTestID(4), Device: reminderTestID(6)}, r)
	if e != nil || two.Status != 0 || string(one.Data) != string(two.Data) {
		t.Fatalf("replay=%+v err=%v", two, e)
	}
	if a.checks != 2 {
		t.Fatalf("membership not refreshed across replay: %d", a.checks)
	}
}

func TestReminderCancelPreEpochReturnsPrivateStatusAndPreservesExactReplay(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(1000, 0)
	h, _, state := newReminderTest(&now)
	owner := reminderTestID(4)
	reminder, err := h.Store.Create(ctx, reminderTestID(3), owner, [32]byte{1}, reminders.Reminder{Text: "x", Timezone: "UTC"}, time.Unix(4600, 0))
	if err != nil {
		t.Fatal(err)
	}
	revision, expires := uint64(1), uint64(1100)
	r := protocol.Request{RequestID: reminderTestID(8), CommunityID: reminderTestID(1), Operation: protocol.OpReminderCancel, Revision: &revision, Expires: &expires, Args: []any{reminder.ID}}
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
	now = time.Unix(-1, 0)
	before := snapshot()
	result, err := h.cancel(ctx, InteractionActor{User: owner, Kind: 0}, r)
	if err != nil || result.Status != interactionStatusUnavailable {
		t.Fatalf("pre-epoch cancellation result=%+v err=%v, want private status 5", result, err)
	}
	if !reflect.DeepEqual(before, snapshot()) {
		t.Fatal("pre-epoch cancellation handler changed reminder, quota, or receipt state")
	}
	now = time.Unix(1000, 0)
	accepted, err := h.cancel(ctx, InteractionActor{User: owner, Kind: 0}, r)
	if err != nil || accepted.Status != 0 || accepted.Revision == nil || *accepted.Revision != 2 {
		t.Fatalf("valid cancellation result=%+v err=%v", accepted, err)
	}
	beforeReplay := snapshot()
	now = time.Unix(-1, 0)
	replayed, err := h.cancel(ctx, InteractionActor{User: owner, Kind: 0}, r)
	if err != nil || replayed.Status != 0 || replayed.Revision == nil || *replayed.Revision != 2 {
		t.Fatalf("pre-epoch exact replay=%+v err=%v", replayed, err)
	}
	if !reflect.DeepEqual(beforeReplay, snapshot()) {
		t.Fatal("pre-epoch cancellation replay changed state")
	}
}
func TestReminderMembershipRevocationAndCrossUserAreDenied(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(1000, 0).UTC()
	h, a, _ := newReminderTest(&now)
	payload, _ := cborx.Marshal([]any{uint64(1), uint64(0), uint64(60), "private text", "UTC", nil, nil})
	rev := uint64(0)
	exp := uint64(1100)
	r := protocol.Request{RequestID: reminderTestID(8), CommunityID: reminderTestID(1), Operation: protocol.OpReminderCreate, Revision: &rev, Expires: &exp, Args: []any{payload}}
	res, _ := h.create(ctx, InteractionActor{User: reminderTestID(4)}, r)
	if res.Status != 0 {
		t.Fatal(res.Status)
	}
	list := protocol.Request{CommunityID: reminderTestID(1), Operation: protocol.OpReminderList, Args: []any{nil}}
	res, _ = h.list(ctx, InteractionActor{User: reminderTestID(7)}, list)
	if res.Status != 0 {
		t.Fatalf("other account should see empty list: %d", res.Status)
	}
	var data []any
	if cborx.Unmarshal(res.Data, &data) != nil || len(data) != 3 || len(data[1].([]any)) != 0 {
		t.Fatalf("cross-user rows leaked: %v", data)
	}
	a.member = false
	res, _ = h.list(ctx, InteractionActor{User: reminderTestID(4)}, list)
	if res.Status != interactionStatusForbidden {
		t.Fatalf("revoked status=%d", res.Status)
	}
}
func TestReminderCreateBoundsTimezoneAndSourceMembership(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(1000, 0).UTC()
	h, a, _ := newReminderTest(&now)
	rev := uint64(0)
	exp := uint64(1100)
	makeReq := func(when uint64, zone string, channel any) protocol.Request {
		p, _ := cborx.Marshal([]any{uint64(1), uint64(0), when, "x", zone, channel, nil})
		return protocol.Request{RequestID: reminderTestID(byte(when)), CommunityID: reminderTestID(1), Operation: protocol.OpReminderCreate, Revision: &rev, Expires: &exp, Args: []any{p}}
	}
	actor := InteractionActor{User: reminderTestID(4)}
	res, _ := h.create(ctx, actor, makeReq(59, "UTC", nil))
	if res.Status != interactionStatusUnavailable {
		t.Fatalf("short interval status=%d", res.Status)
	}
	res, _ = h.create(ctx, actor, makeReq(60, "Not/AZone", nil))
	if res.Status != interactionStatusUnavailable {
		t.Fatalf("invalid zone status=%d", res.Status)
	}
	a.source = false
	res, _ = h.create(ctx, actor, makeReq(61, "UTC", reminderTestID(9)))
	if res.Status != interactionStatusForbidden {
		t.Fatalf("source denial status=%d", res.Status)
	}
}

func TestReminderListPagesStayWithinProtocolLimit(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(1000, 0).UTC()
	h, _, _ := newReminderTest(&now)
	actor := InteractionActor{User: reminderTestID(4)}
	rev := uint64(0)
	exp := uint64(1100)
	for i := byte(20); i < 24; i++ {
		p, _ := cborx.Marshal([]any{uint64(1), uint64(0), uint64(60), strings.Repeat("x", 1500), "UTC", nil, nil})
		r := protocol.Request{RequestID: reminderTestID(i), CommunityID: reminderTestID(1), Operation: protocol.OpReminderCreate, Revision: &rev, Expires: &exp, Args: []any{p}}
		out, e := h.create(ctx, actor, r)
		if e != nil || out.Status != 0 {
			t.Fatalf("create status=%d err=%v", out.Status, e)
		}
	}
	req := protocol.Request{CommunityID: reminderTestID(1), Operation: protocol.OpReminderList, Args: []any{nil}}
	out, e := h.list(ctx, actor, req)
	if e != nil || out.Status != 0 || len(out.Data) > reminderPageBytes {
		t.Fatalf("page bytes=%d status=%d err=%v", len(out.Data), out.Status, e)
	}
	var data []any
	if cborx.Unmarshal(out.Data, &data) != nil || data[2] == nil {
		t.Fatalf("expected continuation cursor: %v", data)
	}
}

func TestAbsoluteCreateReplayAfterAcceptanceWindowAndRejectsChangedFingerprint(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(1000, 0).UTC()
	h, a, state := newReminderTest(&now)
	actor := InteractionActor{User: reminderTestID(4), Device: reminderTestID(5)}
	rev := uint64(0)
	exp := uint64(1100)
	payload, _ := cborx.Marshal([]any{uint64(1), uint64(1), uint64(1060), "absolute", "UTC", nil, nil})
	req := protocol.Request{RequestID: reminderTestID(8), CommunityID: reminderTestID(1), Operation: protocol.OpReminderCreate, Revision: &rev, Expires: &exp, Args: []any{payload}}
	first, e := h.create(ctx, actor, req)
	if e != nil || first.Status != 0 {
		t.Fatalf("initial status=%d err=%v", first.Status, e)
	}
	now = time.Unix(1001, 0).UTC()
	restarted := ReminderInteractions{Store: reminders.NewStore(state, native.Binding{Community: reminderTestID(1), Installation: reminderTestID(2)}), Authority: a, CommunityID: reminderTestID(1), Now: func() time.Time { return now }}
	replay, e := restarted.create(ctx, InteractionActor{User: actor.User, Device: reminderTestID(6)}, req)
	if e != nil || replay.Status != 0 || string(first.Data) != string(replay.Data) {
		t.Fatalf("absolute replay=%+v err=%v", replay, e)
	}
	changed, _ := cborx.Marshal([]any{uint64(1), uint64(1), uint64(1060), "different", "UTC", nil, nil})
	req.Args = []any{changed}
	conflict, e := restarted.create(ctx, actor, req)
	if e != nil || conflict.Status != interactionStatusConflict {
		t.Fatalf("changed replay status=%d err=%v", conflict.Status, e)
	}
}
func TestRevokedSourceDoesNotPoisonInboxAndOwnerCanCancelAtQuota(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(1000, 0).UTC()
	h, a, _ := newReminderTest(&now)
	actor := InteractionActor{User: reminderTestID(4)}
	due := now.Add(time.Hour)
	source := reminderTestID(7)
	for i := byte(20); i < 120; i++ {
		var ch *id.ID
		if i == 20 || i == 21 {
			ch = &source
		}
		if _, e := h.Store.Create(ctx, reminderTestID(i), actor.User, [32]byte{i}, reminders.Reminder{Text: "reminder", Timezone: "UTC", SourceChannel: ch}, due); e != nil {
			t.Fatalf("seed %d: %v", i, e)
		}
	}
	a.source = false
	list := protocol.Request{CommunityID: reminderTestID(1), Operation: protocol.OpReminderList, Args: []any{nil}}
	page, e := h.list(ctx, actor, list)
	if e != nil || page.Status != 0 {
		t.Fatalf("list status=%d err=%v", page.Status, e)
	}
	var data []any
	if cborx.Unmarshal(page.Data, &data) != nil || len(data[1].([]any)) != 20 {
		t.Fatalf("list page shape=%v", data)
	}
	summaries := data[1].([]any)
	first := summaries[0].([]any)
	firstID, idOK := reminderID(first[0])
	if !idOK || firstID != reminderTestID(20) || first[4] != nil || first[5] != nil {
		t.Fatalf("revoked source was exposed: %v", first)
	}
	if summaries[1].([]any)[1] != "reminder" {
		t.Fatalf("unrelated reminder missing: %v", summaries[1])
	}
	row, e := h.Store.Get(ctx, reminderTestID(20), actor.User)
	if e != nil {
		t.Fatal(e)
	}
	rev := row.Revision
	exp := uint64(1100)
	cancel := protocol.Request{RequestID: reminderTestID(121), CommunityID: reminderTestID(1), Operation: protocol.OpReminderCancel, Revision: &rev, Expires: &exp, Args: []any{reminderTestID(20)}}
	out, e := h.cancel(ctx, actor, cancel)
	if e != nil || out.Status != 0 {
		t.Fatalf("owner cancel status=%d err=%v", out.Status, e)
	}
	var canceledData []any
	if cborx.Unmarshal(out.Data, &canceledData) != nil || canceledData[1].([]any)[4] != nil || canceledData[1].([]any)[5] != nil {
		t.Fatalf("cancel response exposed revoked source: %v", canceledData)
	}
	createPayload, _ := cborx.Marshal([]any{uint64(1), uint64(0), uint64(60), "quota released", "UTC", nil, nil})
	zero := uint64(0)
	create := protocol.Request{RequestID: reminderTestID(122), CommunityID: reminderTestID(1), Operation: protocol.OpReminderCreate, Revision: &zero, Expires: &exp, Args: []any{createPayload}}
	out, e = h.create(ctx, actor, create)
	if e != nil || out.Status != 0 {
		t.Fatalf("create after source cancel status=%d err=%v", out.Status, e)
	}
	now = due
	dismissRow, e := h.Store.Get(ctx, reminderTestID(21), actor.User)
	if e != nil {
		t.Fatal(e)
	}
	dismissRevision := dismissRow.Revision
	dismiss := protocol.Request{RequestID: reminderTestID(124), CommunityID: reminderTestID(1), Operation: protocol.OpReminderDismiss, Revision: &dismissRevision, Expires: &exp, Args: []any{reminderTestID(21)}}
	out, e = h.dismiss(ctx, actor, dismiss)
	if e != nil || out.Status != 0 {
		t.Fatalf("owner dismiss after source revocation status=%d err=%v", out.Status, e)
	}
	var dismissedData []any
	if cborx.Unmarshal(out.Data, &dismissedData) != nil || dismissedData[1].([]any)[4] != nil || dismissedData[1].([]any)[5] != nil {
		t.Fatalf("dismiss response exposed revoked source: %v", dismissedData)
	}
}
func TestNativeStatusFourIsReturnedAsInteractionConflict(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(1000, 0).UTC()
	state := &reminderStateFake{txErr: &helper.StatusError{Status: 4}}
	auth := &reminderAuthorityFake{member: true, source: true}
	h := ReminderInteractions{Store: reminders.NewStore(state, native.Binding{Community: reminderTestID(1), Installation: reminderTestID(2)}), Authority: auth, CommunityID: reminderTestID(1), Now: func() time.Time { return now }}
	payload, _ := cborx.Marshal([]any{uint64(1), uint64(0), uint64(60), "x", "UTC", nil, nil})
	rev := uint64(0)
	exp := uint64(1100)
	req := protocol.Request{RequestID: reminderTestID(8), CommunityID: reminderTestID(1), Operation: protocol.OpReminderCreate, Revision: &rev, Expires: &exp, Args: []any{payload}}
	out, e := h.create(ctx, InteractionActor{User: reminderTestID(4)}, req)
	if e != nil || out.Status != interactionStatusConflict {
		t.Fatalf("native conflict status=%d err=%v", out.Status, e)
	}
}

func TestFinalReviewLongRemindersRequireSixPages(t *testing.T) {
	now := time.Unix(1000, 0)
	h, _, _ := newReminderTest(&now)
	owner := reminderTestID(4)
	for i := byte(10); i < 16; i++ {
		_, err := h.Store.Create(t.Context(), reminderTestID(i), owner, [32]byte{i}, reminders.Reminder{Text: strings.Repeat("x", 1500), Timezone: "UTC"}, time.Unix(2000+int64(i), 0))
		if err != nil {
			t.Fatal(err)
		}
	}
	var cursor any
	for page := 0; page < 6; page++ {
		result, err := h.list(t.Context(), InteractionActor{User: owner}, protocol.Request{RequestID: reminderTestID(byte(30 + page)), CommunityID: reminderTestID(1), Operation: protocol.OpReminderList, Args: []any{cursor}})
		if err != nil || result.Status != 0 {
			t.Fatalf("page=%d result=%+v error=%v", page, result, err)
		}
		var data []any
		if err = cborx.Unmarshal(result.Data, &data); err != nil {
			t.Fatal(err)
		}
		rows := data[1].([]any)
		if len(rows) != 1 {
			t.Fatalf("page%d rows%d", page, len(rows))
		}
		cursor = data[2]
		t.Logf("page=%d rows=%d bytes=%d hasMore=%v", page+1, len(rows), len(result.Data), cursor != nil)
		if (page < 5) != (cursor != nil) {
			t.Fatal("unexpected cursor")
		}
	}
}
func TestFinalReviewDueReminderCancel(t *testing.T) {
	now := time.Unix(1000, 0)
	h, _, _ := newReminderTest(&now)
	owner := reminderTestID(4)
	row, err := h.Store.Create(t.Context(), reminderTestID(10), owner, [32]byte{10}, reminders.Reminder{Text: "due", Timezone: "UTC"}, time.Unix(1060, 0))
	if err != nil {
		t.Fatal(err)
	}
	now = time.Unix(1061, 0)
	revision, expires := row.Revision, uint64(1120)
	result, err := h.cancel(t.Context(), InteractionActor{User: owner}, protocol.Request{RequestID: reminderTestID(20), CommunityID: reminderTestID(1), Operation: protocol.OpReminderCancel, Revision: &revision, Expires: &expires, Args: []any{row.ID}})
	if err != nil || result.Status != 0 {
		t.Fatalf("due cancellation should succeed per UI contract; status=%d error=%v", result.Status, err)
	}
}
