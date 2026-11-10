package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/bot/moderation"
	"github.com/dilla-chat/dilla/internal/bot/native"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
)

type moderationCaseNativeFake struct {
	binding        native.Binding
	rows           map[string]native.StateValue
	evidence       map[string]native.StateValue
	cursor         native.StateValue
	evidenceCursor native.StateValue
	failAfterWrite bool
}

func newModerationCaseNative() *moderationCaseNativeFake {
	return &moderationCaseNativeFake{binding: native.Binding{Instance: id.New(), Community: id.New(), Installation: id.New()}, rows: map[string]native.StateValue{}}
}
func (n *moderationCaseNativeFake) Binding() native.Binding { return n.binding }
func (n *moderationCaseNativeFake) StateGet(_ context.Context, area uint64, key []byte) (native.StateValue, error) {
	if area == moderationPurgeCursorArea && string(key) == string(moderationPurgeCursorKey) {
		return n.cursor, nil
	}
	if area == moderationPurgeCursorArea {
		return n.evidenceCursor, nil
	}
	if area == 9 {
		v := n.evidence[string(key)]
		v.Value = append([]byte(nil), v.Value...)
		return v, nil
	}
	if area != moderationCaseStateArea {
		return native.StateValue{}, errors.New("wrong area")
	}
	v, ok := n.rows[string(key)]
	if !ok {
		return native.StateValue{}, nil
	}
	v.Value = append([]byte(nil), v.Value...)
	return v, nil
}
func (n *moderationCaseNativeFake) StateTransaction(_ context.Context, _ id.ID, changes []native.StateChange) ([]uint64, error) {
	if len(changes) != 1 {
		return nil, errors.New("bad transaction")
	}
	c := changes[0]
	if c.Area == moderationPurgeCursorArea && string(c.Key) == string(moderationPurgeCursorKey) {
		if n.cursor.Revision != c.ExpectedRevision {
			return nil, errors.New("revision conflict")
		}
		n.cursor = native.StateValue{Revision: n.cursor.Revision + 1, Tombstone: c.Value == nil, Value: append([]byte(nil), c.Value...)}
		return []uint64{n.cursor.Revision}, nil
	}
	if c.Area == moderationPurgeCursorArea {
		if n.evidenceCursor.Revision != c.ExpectedRevision {
			return nil, errors.New("evidence cursor revision conflict")
		}
		n.evidenceCursor = native.StateValue{Revision: n.evidenceCursor.Revision + 1, Tombstone: c.Value == nil, Value: append([]byte(nil), c.Value...)}
		return []uint64{n.evidenceCursor.Revision}, nil
	}
	if c.Area == 9 {
		if n.evidence == nil {
			n.evidence = make(map[string]native.StateValue)
		}
		old := n.evidence[string(c.Key)]
		if old.Revision != c.ExpectedRevision {
			return nil, errors.New("revision conflict")
		}
		v := native.StateValue{Revision: old.Revision + 1, Value: append([]byte(nil), c.Value...), Tombstone: c.Value == nil}
		n.evidence[string(c.Key)] = v
		return []uint64{v.Revision}, nil
	}
	if c.Area != moderationCaseStateArea {
		return nil, errors.New("wrong area")
	}
	old := n.rows[string(c.Key)]
	if old.Revision != c.ExpectedRevision {
		return nil, errors.New("revision conflict")
	}
	v := native.StateValue{Revision: old.Revision + 1}
	if c.Value == nil {
		v.Tombstone = true
	} else {
		v.Value = append([]byte(nil), c.Value...)
	}
	n.rows[string(c.Key)] = v
	if n.failAfterWrite {
		n.failAfterWrite = false
		return nil, errors.New("response lost")
	}
	return []uint64{v.Revision}, nil
}
func (n *moderationCaseNativeFake) StateScan(_ context.Context, area uint64, after []byte, limit uint64) (native.StatePage, error) {
	if area != moderationCaseStateArea && area != 9 || limit == 0 || limit > 100 {
		return native.StatePage{}, errors.New("bad scan")
	}
	rows := n.rows
	if area == 9 {
		rows = n.evidence
	}
	keys := make([]string, 0, len(rows))
	for key := range rows {
		if key > string(after) {
			keys = append(keys, key)
		}
	}
	// Keys are fixed-width hexadecimal and therefore already range-sortable.
	for i := 0; i < len(keys); i++ {
		for j := i + 1; j < len(keys); j++ {
			if keys[j] < keys[i] {
				keys[i], keys[j] = keys[j], keys[i]
			}
		}
	}
	if len(keys) > int(limit) {
		keys = keys[:limit]
	}
	page := native.StatePage{}
	for _, key := range keys {
		v := rows[key]
		page.Entries = append(page.Entries, native.StateEntry{Key: []byte(key), Revision: v.Revision, Value: append([]byte(nil), v.Value...), Tombstone: v.Tombstone})
	}
	if len(keys) > 0 {
		page.Next = []byte(keys[len(keys)-1])
	}
	return page, nil
}

func sampleModerationAction(n *moderationCaseNativeFake, revision uint64) ModerationAction {
	return ModerationAction{Community: n.binding.Community.String(), Group: id.New().String(), Channel: id.New().String(), Actor: id.New().String(), Message: id.New().String(),
		SourceRevision: revision, Rules: []moderation.Rule{moderation.RuleMentions}, Excerpt: "example", ReceivedAt: time.Unix(1_800_000_000, 0), runtimeGenerated: true}
}

func TestModerationCasePurgeExpiresRowsInBoundedScans(t *testing.T) {
	n := newModerationCaseNative()
	now := time.Unix(1_800_000_000, 0)
	sink, _ := NewModerationCaseSink(n, func() time.Time { return now })
	old := sampleModerationAction(n, 1)
	old.ReceivedAt = now.Add(-91 * 24 * time.Hour)
	fresh := sampleModerationAction(n, 2)
	fresh.ReceivedAt = now.Add(-89 * 24 * time.Hour)
	if err := sink.ApplyModerationAction(context.Background(), old); err != nil {
		t.Fatal(err)
	}
	if err := sink.ApplyModerationAction(context.Background(), fresh); err != nil {
		t.Fatal(err)
	}
	removed, next, err := sink.PurgeExpired(context.Background(), nil)
	if err != nil || removed != 1 || next == nil {
		t.Fatalf("purge = %d, cursor %q, err %v", removed, next, err)
	}
	removed, next, err = sink.PurgeExpired(context.Background(), next)
	if err != nil || removed != 0 || next != nil {
		t.Fatalf("final purge page = %d, cursor %q, err %v", removed, next, err)
	}
	cases, _, err := sink.list(context.Background(), nil, 10)
	if err != nil || len(cases) != 1 || cases[0].Message != fresh.Message {
		t.Fatalf("remaining cases = %#v, %v", cases, err)
	}
}

func TestModerationCasePurgeResumesAfterRestartFromEncryptedCursor(t *testing.T) {
	n := newModerationCaseNative()
	now := time.Unix(1_800_000_000, 0)
	sink, _ := NewModerationCaseSink(n, func() time.Time { return now })
	for i := uint64(0); i < moderationCaseScanLimit+1; i++ {
		action := sampleModerationAction(n, i+1)
		action.ReceivedAt = now.Add(-91 * 24 * time.Hour)
		if err := sink.ApplyModerationAction(context.Background(), action); err != nil {
			t.Fatal(err)
		}
	}
	removed, err := sink.PurgeExpiredResumable(context.Background())
	if err != nil || removed == 0 || n.cursor.Tombstone || len(n.cursor.Value) == 0 {
		t.Fatalf("first resumable purge = %d, cursor=%+v, err=%v", removed, n.cursor, err)
	}

	// A new sink represents a process restart. It must continue from encrypted
	// state rather than begin at the first page again.
	restarted, _ := NewModerationCaseSink(n, func() time.Time { return now })
	removed, err = restarted.PurgeExpiredResumable(context.Background())
	if err != nil || removed != 1 {
		t.Fatalf("resumed purge = %d, cursor=%+v, err=%v", removed, n.cursor, err)
	}
	// The page cursor is cleared only after an empty terminal scan.
	removed, err = restarted.PurgeExpiredResumable(context.Background())
	if err != nil || removed != 0 || !n.cursor.Tombstone {
		t.Fatalf("terminal purge = %d, cursor=%+v, err=%v", removed, n.cursor, err)
	}
	cases, _, err := restarted.list(context.Background(), nil, 10)
	if err != nil || len(cases) != 0 {
		t.Fatalf("remaining cases = %d, err=%v", len(cases), err)
	}
}

func TestModerationCaseMaintenanceAlsoTombstonesExpiredEvidence(t *testing.T) {
	n := newModerationCaseNative()
	now := time.Now().UTC().Truncate(time.Second)
	sink, err := NewModerationCaseSink(n, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	op := moderation.EvidenceInput{OperationID: id.New(), MessageID: id.New(), Action: moderation.ActionRemove,
		Rule: moderation.RuleDuplicate, Reason: "retained reason", Excerpt: "retained excerpt", CreatedAt: now}
	if err := moderation.NewEvidenceWriter(n).Save(context.Background(), op); err != nil {
		t.Fatal(err)
	}
	var evidenceKey string
	for key := range n.evidence {
		evidenceKey = key
	}
	stored := n.evidence[evidenceKey]
	var row []any
	if err := cborx.Unmarshal(stored.Value, &row); err != nil {
		t.Fatal(err)
	}
	created := uint64(now.Add(-moderation.EvidenceRetention - time.Second).Unix())
	row[11] = created
	row[12] = created + uint64(moderation.EvidenceRetention/time.Second)
	stored.Value, err = cborx.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	n.evidence[evidenceKey] = stored

	removed, err := sink.PurgeExpiredResumable(context.Background())
	if err != nil || removed != 1 || !n.evidence[evidenceKey].Tombstone {
		t.Fatalf("maintenance evidence purge = %d, row=%+v, err=%v", removed, n.evidence[evidenceKey], err)
	}
}
