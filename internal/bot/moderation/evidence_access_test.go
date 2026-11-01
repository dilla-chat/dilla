package moderation

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jonasthim/dilla/internal/bot/client"
	"github.com/jonasthim/dilla/internal/bot/music/counter"
	"github.com/jonasthim/dilla/internal/bot/native"
	"github.com/jonasthim/dilla/internal/cborx"
	"github.com/jonasthim/dilla/internal/id"
)

type evidenceState struct {
	binding  native.Binding
	value    native.StateValue
	reads    int
	rows     map[string]native.StateValue
	writes   int
	writeErr error
	cursor   native.StateValue
}

func (s *evidenceState) Binding() native.Binding { return s.binding }
func (s *evidenceState) StateGet(_ context.Context, area uint64, key []byte) (native.StateValue, error) {
	s.reads++
	if area == evidencePurgeCursorArea && string(key) == string(evidencePurgeCursorKey) {
		return s.cursor, nil
	}
	if area != evidenceStateArea {
		return native.StateValue{}, errors.New("unexpected evidence state address")
	}
	if value, ok := s.rows[string(key)]; ok {
		return value, nil
	}
	if string(key) == string(evidenceKey(s.binding, testEvidenceID)) {
		return s.value, nil
	}
	return native.StateValue{}, nil
}
func (s *evidenceState) StateScan(_ context.Context, area uint64, after []byte, limit uint64) (native.StatePage, error) {
	if area != evidenceStateArea || limit == 0 || limit > 100 || len(after) > 128 {
		return native.StatePage{}, errors.New("invalid evidence scan")
	}
	keys := make([]string, 0, len(s.rows))
	for key := range s.rows {
		if key > string(after) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	if len(keys) > int(limit) {
		keys = keys[:limit]
	}
	page := native.StatePage{}
	for _, key := range keys {
		value := s.rows[key]
		page.Entries = append(page.Entries, native.StateEntry{Key: []byte(key), Revision: value.Revision, Value: append([]byte(nil), value.Value...), Tombstone: value.Tombstone})
	}
	if len(keys) > 0 {
		page.Next = []byte(keys[len(keys)-1])
	}
	return page, nil
}
func (s *evidenceState) StateTransaction(_ context.Context, _ id.ID, changes []native.StateChange) ([]uint64, error) {
	s.writes++
	if s.writeErr != nil {
		return nil, s.writeErr
	}
	if s.rows == nil {
		s.rows = make(map[string]native.StateValue)
	}
	if len(changes) != 1 {
		return nil, errors.New("unexpected evidence state transaction")
	}
	change := changes[0]
	if change.Area == evidencePurgeCursorArea && string(change.Key) == string(evidencePurgeCursorKey) {
		if s.cursor.Revision != change.ExpectedRevision {
			return nil, errors.New("cursor revision conflict")
		}
		s.cursor = native.StateValue{Revision: s.cursor.Revision + 1, Value: append([]byte(nil), change.Value...), Tombstone: change.Value == nil}
		return []uint64{s.cursor.Revision}, nil
	}
	if change.Area != evidenceStateArea {
		return nil, errors.New("unexpected evidence state area")
	}
	key := string(change.Key)
	old := s.rows[key]
	if old.Revision != change.ExpectedRevision {
		return nil, errors.New("evidence state conflict")
	}
	value := native.StateValue{Revision: old.Revision + 1, Tombstone: change.Value == nil, Value: append([]byte(nil), change.Value...)}
	s.rows[key] = value
	return []uint64{value.Revision}, nil
}

type evidenceAuthority struct {
	result  client.ActorAuthority
	present bool
	err     error
	calls   int
}

func (a *evidenceAuthority) BotActorAuthority(context.Context, id.ID, id.ID) (client.ActorAuthority, bool, error) {
	a.calls++
	return a.result, a.present, a.err
}

var testEvidenceID = id.ID{1}

func encodedEvidence(t *testing.T, binding native.Binding, evidenceID id.ID) native.StateValue {
	t.Helper()
	created := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	body, err := cborx.Marshal([]any{uint64(1), binding.Instance, binding.Community, binding.Installation,
		evidenceID, evidenceID, id.ID{2}, "remove", string(RuleDuplicate), "accepted reason", "bounded excerpt", uint64(created.Unix()), uint64(created.Add(EvidenceRetention).Unix())})
	if err != nil {
		t.Fatal(err)
	}
	return native.StateValue{Revision: 1, Value: body}
}

func TestEvidenceReadRequiresFreshViewAuditLogAuthority(t *testing.T) {
	binding := native.Binding{Instance: id.ID{3}, Community: id.ID{4}, Installation: id.ID{5}}
	state := &evidenceState{binding: binding, value: encodedEvidence(t, binding, testEvidenceID)}
	authority := &evidenceAuthority{result: client.ActorAuthority{EffectiveBits: permViewAuditLog}, present: true}
	reader := NewEvidenceReader(state, authority)
	got, err := reader.Read(context.Background(), id.ID{6}, testEvidenceID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Excerpt != "bounded excerpt" || got.MessageID != (id.ID{2}) || authority.calls != 1 || state.reads != 1 {
		t.Fatalf("evidence=%+v authority calls=%d state reads=%d", got, authority.calls, state.reads)
	}
	// A previously allowed actor can lose the permission; the next read must
	// resolve again before consulting the cached encrypted row.
	authority.result.EffectiveBits = 0
	if _, err := reader.Read(context.Background(), id.ID{6}, testEvidenceID); !errors.Is(err, ErrEvidenceAccess) {
		t.Fatalf("read after permission revocation = %v", err)
	}
	if authority.calls != 2 || state.reads != 1 {
		t.Fatalf("permission was not freshly checked: authority calls=%d state reads=%d", authority.calls, state.reads)
	}
}

func TestEvidenceReadFailsClosedBeforeEncryptedStateRead(t *testing.T) {
	for _, tc := range []struct {
		name      string
		authority *evidenceAuthority
	}{
		{name: "permission missing", authority: &evidenceAuthority{present: true}},
		{name: "actor absent", authority: &evidenceAuthority{}},
		{name: "lookup error", authority: &evidenceAuthority{present: true, err: errors.New("lookup unavailable")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			binding := native.Binding{Instance: id.ID{3}, Community: id.ID{4}, Installation: id.ID{5}}
			state := &evidenceState{binding: binding, value: encodedEvidence(t, binding, testEvidenceID)}
			_, err := NewEvidenceReader(state, tc.authority).Read(context.Background(), id.ID{6}, testEvidenceID)
			if err == nil {
				t.Fatal("unauthorized evidence read succeeded")
			}
			if state.reads != 0 {
				t.Fatalf("encrypted evidence read %d times before authorization", state.reads)
			}
		})
	}
}

func TestEvidenceWriterRoundTripsOnlyInstallationBoundEncryptedState(t *testing.T) {
	binding := native.Binding{Instance: id.ID{3}, Community: id.ID{4}, Installation: id.ID{5}}
	state := &evidenceState{binding: binding}
	input := validEvidenceInput()
	if err := NewEvidenceWriter(state).Save(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	reader := NewEvidenceReader(state, &evidenceAuthority{result: client.ActorAuthority{EffectiveBits: permViewAuditLog}, present: true})
	got, err := reader.Read(context.Background(), id.ID{6}, input.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != input.OperationID || got.OperationID != input.OperationID || got.MessageID != input.MessageID || got.Action != input.Action || got.Rule != input.Rule || got.Reason != input.Reason || got.Excerpt != input.Excerpt || !got.ExpiresAt.Equal(input.CreatedAt.Add(EvidenceRetention)) {
		t.Fatalf("evidence round trip = %+v", got)
	}
	wrongBindingValue := state.rows[string(evidenceKey(binding, input.OperationID))]
	wrongBinding := &evidenceState{binding: native.Binding{Instance: binding.Instance, Community: binding.Community, Installation: id.ID{9}}, rows: map[string]native.StateValue{
		string(evidenceKey(native.Binding{Instance: binding.Instance, Community: binding.Community, Installation: id.ID{9}}, input.OperationID)): wrongBindingValue,
	}}
	if _, err := NewEvidenceReader(wrongBinding, &evidenceAuthority{result: client.ActorAuthority{EffectiveBits: permViewAuditLog}, present: true}).Read(context.Background(), id.ID{6}, input.OperationID); !errors.Is(err, ErrEvidenceAccess) {
		t.Fatalf("reader with different installation binding = %v", err)
	}
	var expiredRow []any
	if err := cborx.Unmarshal(wrongBindingValue.Value, &expiredRow); err != nil {
		t.Fatal(err)
	}
	created := uint64(time.Now().UTC().Add(-EvidenceRetention - time.Hour).Unix())
	expiredRow[11] = created
	expiredRow[12] = created + uint64(EvidenceRetention/time.Second)
	expiredBody, err := cborx.Marshal(expiredRow)
	if err != nil {
		t.Fatal(err)
	}
	expiredState := &evidenceState{binding: binding, rows: map[string]native.StateValue{
		string(evidenceKey(binding, input.OperationID)): {Revision: 1, Value: expiredBody},
	}}
	if _, err := NewEvidenceReader(expiredState, &evidenceAuthority{result: client.ActorAuthority{EffectiveBits: permViewAuditLog}, present: true}).Read(context.Background(), id.ID{6}, input.OperationID); !errors.Is(err, ErrEvidenceAccess) {
		t.Fatalf("expired evidence read = %v", err)
	}
}

func TestManualEvidencePersistsAcceptedReceiptAndActorBinding(t *testing.T) {
	binding := native.Binding{Instance: id.ID{3}, Community: id.ID{4}, Installation: id.ID{5}}
	state := &evidenceState{binding: binding}
	input := EvidenceInput{OperationID: id.ID{7}, Action: ActionRemove, Rule: RuleManual,
		Reason: "reviewed by moderator", Excerpt: "bounded excerpt", CreatedAt: time.Now().UTC(),
		AcceptedActionID: id.ID{8}, AcceptedRevision: 12, ActorUserID: id.ID{9}, ActorDeviceID: id.ID{10}, GroupID: id.ID{11}, TargetSequence: 123}
	writer := NewEvidenceWriter(state)
	if !writer.BoundTo(binding.Installation, binding.Community) || writer.BoundTo(binding.Installation, id.ID{11}) {
		t.Fatal("evidence writer reported an incorrect binding")
	}
	if err := writer.Save(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	reader := NewEvidenceReader(state, &evidenceAuthority{result: client.ActorAuthority{EffectiveBits: permViewAuditLog}, present: true})
	got, err := reader.Read(context.Background(), id.ID{6}, input.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Rule != RuleManual || got.AcceptedActionID != input.AcceptedActionID || got.AcceptedRevision != input.AcceptedRevision ||
		got.ActorUserID != input.ActorUserID || got.ActorDeviceID != input.ActorDeviceID || got.GroupID != input.GroupID || got.TargetSequence != input.TargetSequence || got.Reason != input.Reason || got.Excerpt != input.Excerpt {
		t.Fatalf("manual accepted evidence round trip = %+v", got)
	}
	input.AcceptedActionID = id.ID{}
	if err := writer.Save(context.Background(), input); !errors.Is(err, ErrEvidenceWrite) {
		t.Fatalf("manual evidence without accepted action receipt = %v, want ErrEvidenceWrite", err)
	}
}

func TestManualWarningEvidencePersistsTargetAndAuthoritativeReceipt(t *testing.T) {
	binding := native.Binding{Instance: id.ID{3}, Community: id.ID{4}, Installation: id.ID{5}}
	state := &evidenceState{binding: binding}
	input := EvidenceInput{OperationID: id.ID{7}, Action: ActionWarn, Rule: RuleManual, Reason: "human warning reason",
		CreatedAt: time.Now().UTC(), AcceptedActionID: id.ID{8}, AcceptedRevision: 12, ActorUserID: id.ID{9},
		ActorDeviceID: id.ID{10}, TargetUserID: id.ID{11}}
	if err := NewEvidenceWriter(state).Save(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	reader := NewEvidenceReader(state, &evidenceAuthority{result: client.ActorAuthority{EffectiveBits: permViewAuditLog}, present: true})
	got, err := reader.Read(context.Background(), id.ID{6}, input.OperationID)
	if err != nil || got.Action != ActionWarn || got.Rule != RuleManual || got.TargetUserID != input.TargetUserID ||
		got.AcceptedActionID != input.AcceptedActionID || got.AcceptedRevision != input.AcceptedRevision ||
		got.ActorUserID != input.ActorUserID || got.ActorDeviceID != input.ActorDeviceID || got.Reason != input.Reason || got.MessageID != (id.ID{}) {
		t.Fatalf("manual warning evidence=%+v err=%v", got, err)
	}
	input.TargetUserID = id.ID{}
	if err := NewEvidenceWriter(state).Save(context.Background(), input); !errors.Is(err, ErrEvidenceWrite) {
		t.Fatalf("warning evidence without target = %v, want ErrEvidenceWrite", err)
	}
}

func TestEvidenceReaderRejectsAnotherCommunityBinding(t *testing.T) {
	storedBinding := native.Binding{Instance: id.ID{3}, Community: id.ID{4}, Installation: id.ID{5}}
	state := &evidenceState{value: encodedEvidence(t, storedBinding, testEvidenceID)}
	otherCommunity := native.Binding{Instance: storedBinding.Instance, Community: id.ID{8}, Installation: storedBinding.Installation}
	reader := NewEvidenceReader(&evidenceState{binding: otherCommunity, value: state.value}, &evidenceAuthority{
		result: client.ActorAuthority{EffectiveBits: permViewAuditLog}, present: true,
	})
	if _, err := reader.Read(context.Background(), id.ID{6}, testEvidenceID); !errors.Is(err, ErrEvidenceAccess) {
		t.Fatalf("evidence from another community = %v", err)
	}
}

func TestEvidenceWriterBoundsSensitiveFieldsAndHidesStateErrors(t *testing.T) {
	state := &evidenceState{binding: native.Binding{Instance: id.ID{3}, Community: id.ID{4}, Installation: id.ID{5}}}
	for _, tc := range []struct {
		name string
		edit func(*EvidenceInput)
	}{
		{"empty reason", func(e *EvidenceInput) { e.Reason = "" }},
		{"oversized reason", func(e *EvidenceInput) { e.Reason = string(make([]byte, MaxEvidenceReasonBytes+1)) }},
		{"oversized excerpt", func(e *EvidenceInput) { e.Excerpt = string(make([]byte, ExcerptMaxBytes+1)) }},
		{"invalid action", func(e *EvidenceInput) { e.Action = Action("ban") }},
		{"missing message", func(e *EvidenceInput) { e.MessageID = id.ID{} }},
		{"expired input", func(e *EvidenceInput) { e.CreatedAt = time.Now().UTC().Add(-EvidenceRetention - time.Second) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := validEvidenceInput()
			tc.edit(&input)
			if err := NewEvidenceWriter(state).Save(context.Background(), input); err == nil {
				t.Fatal("invalid evidence was accepted")
			}
		})
	}
	secret := "sensitive reason text"
	state.writeErr = errors.New(secret)
	input := validEvidenceInput()
	input.Reason = secret
	err := NewEvidenceWriter(state).Save(context.Background(), input)
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("write failure leaked sensitive text: %v", err)
	}
}

func TestEvidenceWriterUsesOperationIDIdempotently(t *testing.T) {
	state := &evidenceState{binding: native.Binding{Instance: id.ID{3}, Community: id.ID{4}, Installation: id.ID{5}}}
	writer := NewEvidenceWriter(state)
	input := validEvidenceInput()
	if err := writer.Save(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	if err := writer.Save(context.Background(), input); err != nil {
		t.Fatalf("retry failed: %v", err)
	}
	if state.writes != 1 {
		t.Fatalf("state writes after identical retry = %d, want 1", state.writes)
	}
	input.Reason = "different reason"
	if err := writer.Save(context.Background(), input); !errors.Is(err, ErrEvidenceConflict) {
		t.Fatalf("operation ID reused with different evidence = %v", err)
	}
}

func TestEvidenceRetentionNinetyDayBoundary(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	boundary := now.Add(-90 * 24 * time.Hour)
	if EvidenceRetention != 90*24*time.Hour || !withinEvidenceRetention(boundary, now) {
		t.Fatalf("exact 90-day evidence age was rejected: retention=%s", EvidenceRetention)
	}
	if withinEvidenceRetention(boundary.Add(-time.Second), now) {
		t.Fatal("evidence older than 90 days was accepted")
	}
}

func TestEvidencePurgeAcceptsManualWarningV3Envelope(t *testing.T) {
	binding := native.Binding{Instance: id.ID{3}, Community: id.ID{4}, Installation: id.ID{5}}
	legacyV2ID := id.ID{29}
	if _, err := evidenceExpiry(encodedEvidence(t, binding, legacyV2ID).Value, binding, evidenceKey(binding, legacyV2ID)); err != nil {
		t.Fatalf("v2 removal expiry compatibility: %v", err)
	}
	state := &evidenceState{binding: binding, rows: make(map[string]native.StateValue)}
	input := EvidenceInput{OperationID: id.ID{30}, Action: ActionWarn, Rule: RuleManual, Reason: "manual warning",
		CreatedAt: time.Now().UTC().Truncate(time.Second), AcceptedActionID: id.ID{31}, AcceptedRevision: 2,
		ActorUserID: id.ID{32}, ActorDeviceID: id.ID{33}, TargetUserID: id.ID{34}}
	value, err := encodeEvidence(binding, input)
	if err != nil {
		t.Fatal(err)
	}
	key := evidenceKey(binding, input.OperationID)
	if _, err := evidenceExpiry(value, binding, key); err != nil {
		t.Fatalf("v3 warning expiry: %v", err)
	}
	state.rows[string(key)] = native.StateValue{Revision: 1, Value: value}
	if _, err := NewEvidenceWriter(state).PurgeExpiredResumable(context.Background()); err != nil {
		t.Fatalf("purge with live v3 warning: %v", err)
	}
}

func TestEvidencePurgeFreshEmptyStateSucceeds(t *testing.T) {
	state := &evidenceState{binding: native.Binding{Instance: id.ID{3}, Community: id.ID{4}, Installation: id.ID{5}}, rows: make(map[string]native.StateValue)}
	if removed, err := NewEvidenceWriter(state).PurgeExpiredResumable(context.Background()); err != nil || removed != 0 {
		t.Fatalf("empty-state purge removed=%d err=%v", removed, err)
	}
}

func TestEvidencePurgeResumesAcrossRestartAndKeepsUnexpiredRecords(t *testing.T) {
	binding := native.Binding{Instance: id.ID{3}, Community: id.ID{4}, Installation: id.ID{5}}
	state := &evidenceState{binding: binding, rows: make(map[string]native.StateValue)}
	now := time.Now().UTC().Truncate(time.Second)
	for i := 1; i <= 101; i++ {
		input := EvidenceInput{OperationID: id.ID{byte(i)}, MessageID: id.ID{byte(i + 1)}, Action: ActionRemove, Rule: RuleDuplicate,
			Reason: "expired", CreatedAt: now.Add(-EvidenceRetention - time.Second)}
		value, err := encodeEvidence(binding, input)
		if err != nil {
			t.Fatal(err)
		}
		state.rows[string(evidenceKey(binding, input.OperationID))] = native.StateValue{Revision: 1, Value: value}
	}
	fresh := EvidenceInput{OperationID: id.ID{250}, MessageID: id.ID{251}, Action: ActionRemove, Rule: RuleDuplicate,
		Reason: "fresh", CreatedAt: now.Add(-EvidenceRetention + time.Second)}
	freshValue, err := encodeEvidence(binding, fresh)
	if err != nil {
		t.Fatal(err)
	}
	freshKey := evidenceKey(binding, fresh.OperationID)
	state.rows[string(freshKey)] = native.StateValue{Revision: 1, Value: freshValue}
	if len(freshKey) > 128 {
		t.Fatalf("evidence key length %d exceeds resumable scan cursor limit", len(freshKey))
	}

	writer := NewEvidenceWriter(state)
	removed, err := writer.PurgeExpiredResumable(context.Background())
	firstRemoved := removed
	if err != nil || removed == 0 || removed > int(evidenceScanLimit) || state.cursor.Tombstone || len(state.cursor.Value) == 0 {
		t.Fatalf("first purge = %d cursor=%+v err=%v", removed, state.cursor, err)
	}
	restarted := NewEvidenceWriter(state)
	removed, err = restarted.PurgeExpiredResumable(context.Background())
	if err != nil || removed+firstRemoved != 101 || state.cursor.Tombstone {
		t.Fatalf("resumed purge = %d cursor=%+v err=%v", removed, state.cursor, err)
	}
	removed, err = restarted.PurgeExpiredResumable(context.Background())
	if err != nil || removed != 0 || !state.cursor.Tombstone {
		t.Fatalf("terminal purge = %d cursor=%+v err=%v", removed, state.cursor, err)
	}
	for key, value := range state.rows {
		if key == string(freshKey) {
			if value.Tombstone || value.Revision != 1 {
				t.Fatalf("unexpired evidence was tombstoned: %+v", value)
			}
			continue
		}
		if !value.Tombstone || value.Revision != 2 || len(value.Value) != 0 {
			t.Fatalf("expired evidence was not tombstoned: %+v", value)
		}
	}
}

func validEvidenceInput() EvidenceInput {
	return EvidenceInput{OperationID: id.ID{1}, MessageID: id.ID{2}, Action: ActionRemove, Rule: RuleDuplicate,
		Reason: "Repeated message", Excerpt: "bounded excerpt", CreatedAt: time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)}
}

func TestEvidenceListReturnsBoundedManualRowsAndRechecksAuthority(t *testing.T) {
	binding := native.Binding{Instance: id.ID{3}, Community: id.ID{4}, Installation: id.ID{5}}
	state := &evidenceState{binding: binding}
	writer := NewEvidenceWriter(state)
	for i := byte(20); i < 23; i++ {
		input := validEvidenceInput()
		input.OperationID = id.ID{i}
		input.Rule = RuleManual
		input.CreatedAt = time.Now().UTC().Truncate(time.Second)
		input.AcceptedActionID, input.AcceptedRevision = id.ID{i + 10}, 1
		input.ActorUserID, input.ActorDeviceID = id.ID{6}, id.ID{7}
		input.GroupID, input.TargetSequence = id.ID{8}, uint64(i)
		if err := writer.Save(context.Background(), input); err != nil {
			t.Fatal(err)
		}
	}
	authority := &evidenceAuthority{result: client.ActorAuthority{EffectiveBits: permViewAuditLog}, present: true}
	reader := NewEvidenceReader(state, authority)
	rows, next, err := reader.List(context.Background(), id.ID{9}, nil, 2)
	if err != nil || len(rows) != 2 || len(next) == 0 || authority.calls < 3 {
		t.Fatalf("rows=%d next=%x authority calls=%d err=%v", len(rows), next, authority.calls, err)
	}
	authority.result.EffectiveBits = 0
	before := state.reads
	if _, _, err := reader.List(context.Background(), id.ID{9}, next, 2); !errors.Is(err, ErrEvidenceAccess) || state.reads != before {
		t.Fatalf("revoked page err=%v state reads=%d before=%d", err, state.reads, before)
	}
}

func TestEvidenceListAdvancesPastExpiredAndTombstonedRowsAndStops(t *testing.T) {
	binding := native.Binding{Instance: id.ID{3}, Community: id.ID{4}, Installation: id.ID{5}}
	input := EvidenceInput{OperationID: id.ID{20}, Action: ActionRemove, Rule: RuleManual, Reason: "expired reason",
		CreatedAt: time.Now().UTC().Add(-EvidenceRetention - time.Hour), AcceptedActionID: id.ID{30}, AcceptedRevision: 1,
		ActorUserID: id.ID{6}, ActorDeviceID: id.ID{7}, GroupID: id.ID{8}, TargetSequence: 5}
	body, err := encodeEvidence(binding, input)
	if err != nil {
		t.Fatal(err)
	}
	state := &evidenceState{binding: binding, rows: map[string]native.StateValue{
		"000-tombstone": {Revision: 2, Tombstone: true},
		string(evidenceKey(binding, input.OperationID)): {Revision: 1, Value: body},
	}}
	authority := &evidenceAuthority{result: client.ActorAuthority{EffectiveBits: permViewAuditLog}, present: true}
	reader := NewEvidenceReader(state, authority)
	rows, next, err := reader.List(context.Background(), id.ID{9}, nil, 1)
	if err != nil || len(rows) != 0 || len(next) == 0 {
		t.Fatalf("first page rows=%v next=%x err=%v", rows, next, err)
	}
	rows, next, err = reader.List(context.Background(), id.ID{9}, next, 1)
	if err != nil || len(rows) != 0 || next != nil {
		t.Fatalf("terminal page rows=%v next=%x err=%v", rows, next, err)
	}
}

func TestEvidenceListFailsClosedOnMalformedStoredRow(t *testing.T) {
	binding := native.Binding{Instance: id.ID{3}, Community: id.ID{4}, Installation: id.ID{5}}
	state := &evidenceState{binding: binding, rows: map[string]native.StateValue{"ev2-corrupt": {Revision: 1, Value: []byte{0xff}}}}
	authority := &evidenceAuthority{result: client.ActorAuthority{EffectiveBits: permViewAuditLog}, present: true}
	if _, _, err := NewEvidenceReader(state, authority).List(context.Background(), id.ID{9}, nil, 1); !errors.Is(err, ErrEvidenceAccess) {
		t.Fatalf("malformed evidence list error = %v", err)
	}
}

func TestEvidenceListFailsClosedWhenScannedKeyDoesNotMatchStoredEvidenceID(t *testing.T) {
	binding := native.Binding{Instance: id.ID{3}, Community: id.ID{4}, Installation: id.ID{5}}
	state := &evidenceState{binding: binding}
	input := validEvidenceInput()
	input.Rule = RuleManual
	input.CreatedAt = time.Now().UTC().Truncate(time.Second)
	input.AcceptedActionID, input.AcceptedRevision = id.ID{30}, 1
	input.ActorUserID, input.ActorDeviceID = id.ID{6}, id.ID{7}
	input.GroupID, input.TargetSequence = id.ID{8}, 5
	if err := NewEvidenceWriter(state).Save(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	correct := state.rows[string(evidenceKey(binding, input.OperationID))]
	state.rows["000-mismatched-key"] = correct
	authority := &evidenceAuthority{result: client.ActorAuthority{EffectiveBits: permViewAuditLog}, present: true}
	if _, _, err := NewEvidenceReader(state, authority).List(context.Background(), id.ID{9}, nil, 1); !errors.Is(err, ErrEvidenceAccess) {
		t.Fatalf("mismatched scanned key error = %v", err)
	}
}

func TestEvidenceListAndPurgeSkipOtherStateNamespaces(t *testing.T) {
	binding := native.Binding{Instance: id.ID{3}, Community: id.ID{4}, Installation: id.ID{5}}
	state := &evidenceState{binding: binding, rows: map[string]native.StateValue{
		memberRegistrationCatchupStateKey: {Revision: 1, Value: []byte{0xff}},
	}}
	group := id.New()
	reserved, err := counter.Reserve(t.Context(), state, group, 1, 2)
	if err != nil {
		t.Fatal(err)
	}

	input := validEvidenceInput()
	input.Rule = RuleManual
	input.CreatedAt = time.Now().UTC().Truncate(time.Second)
	input.AcceptedActionID, input.AcceptedRevision = id.ID{10}, 1
	input.ActorUserID, input.ActorDeviceID = id.ID{11}, id.ID{12}
	input.GroupID, input.TargetSequence = id.ID{13}, 14
	writer := NewEvidenceWriter(state)
	if err := writer.Save(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	authority := &evidenceAuthority{result: client.ActorAuthority{EffectiveBits: permViewAuditLog}, present: true}
	rows, cursor, err := NewEvidenceReader(state, authority).List(context.Background(), id.ID{15}, nil, 1)
	if err != nil || len(rows) != 1 || rows[0].OperationID != input.OperationID {
		t.Fatalf("manual evidence rows=%d err=%v", len(rows), err)
	}
	if _, _, err := NewEvidenceReader(state, authority).List(t.Context(), id.ID{15}, cursor, 1); err != nil {
		t.Fatal("audio counter broke evidence pagination")
	}

	if _, err := writer.PurgeExpiredResumable(context.Background()); err != nil {
		t.Fatalf("purge failed on member-registration cursor: %v", err)
	}
	if row := state.rows[memberRegistrationCatchupStateKey]; row.Tombstone || row.Revision != 1 {
		t.Fatalf("member-registration cursor was changed: %+v", row)
	}
	next, err := counter.Reserve(t.Context(), state, group, 1, 2)
	if err != nil || next.Start != reserved.End {
		t.Fatal("evidence purge modified audio counter reservation")
	}

}
