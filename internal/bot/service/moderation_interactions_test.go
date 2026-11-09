package service

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/bot/client"
	"github.com/dilla-chat/dilla/internal/bot/moderation"
	"github.com/dilla-chat/dilla/internal/bot/protocol"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
)

type moderationInteractionReaderFake struct {
	actor id.ID
	limit uint64
	rows  []ModerationCase
	after []byte
	next  []byte
	err   error
}

func (f *moderationInteractionReaderFake) List(_ context.Context, actor id.ID, after []byte, limit uint64) ([]ModerationCase, []byte, error) {
	f.actor, f.limit, f.after = actor, limit, append([]byte(nil), after...)
	return f.rows, append([]byte(nil), f.next...), f.err
}

func TestModerationInteractionsListsBoundedCasesForAuthenticatedActor(t *testing.T) {
	reader := &moderationInteractionReaderFake{rows: []ModerationCase{{
		Community: interactionID(2).String(), Group: interactionID(4).String(), Channel: interactionID(5).String(),
		Actor: interactionID(6).String(), Message: interactionID(7).String(), SourceRevision: 42,
		Rules: []moderation.Rule{moderation.RuleDuplicate}, Action: moderation.ActionRemove, Excerpt: "bounded preview", ReceivedAt: time.Unix(1_800_000_000, 0),
	}}}
	h := ModerationInteractions{Reader: reader, CommunityID: interactionID(2)}
	handler := h.Handlers()[protocol.OpModerationCases]
	result, err := handler.Handle(context.Background(), InteractionActor{User: interactionID(6)}, protocol.Request{
		CommunityID: interactionID(2), Args: []any{nil},
	})
	if err != nil || result.Status != 0 || reader.actor != interactionID(6) || reader.limit != moderationInteractionPageSize || len(reader.after) != 0 {
		t.Fatalf("result=%+v reader=%+v err=%v", result, reader, err)
	}
	var got []any
	if err := cborx.Unmarshal(result.Data, &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0] != uint64(1) || len(got[1].([]any)) != 1 {
		t.Fatalf("encoded page = %#v", got)
	}
}

type evidenceListFake struct {
	actor id.ID
	after []byte
	limit uint64
	rows  []moderation.Evidence
	next  []byte
}

func (f *evidenceListFake) List(_ context.Context, actor id.ID, after []byte, limit uint64) ([]moderation.Evidence, []byte, error) {
	f.actor, f.after, f.limit = actor, append([]byte(nil), after...), limit
	return f.rows, append([]byte(nil), f.next...), nil
}

func TestModerationInteractionsListsEncryptedManualEvidenceUnderBoundedCursor(t *testing.T) {
	reader := &evidenceListFake{rows: []moderation.Evidence{{ID: interactionID(50), Action: moderation.ActionWarn, Rule: moderation.RuleManual, Reason: "moderator reason", CreatedAt: time.Unix(1_800_000_000, 0)}}, next: []byte("state-key")}
	handler := (ModerationInteractions{EvidenceReader: reader, CommunityID: interactionID(2), InstallationID: interactionID(3)}).Handlers()[protocol.OpModerationEvidenceList]
	result, err := handler.Handle(context.Background(), InteractionActor{User: interactionID(6)}, protocol.Request{CommunityID: interactionID(2), Args: []any{nil}})
	var data []any
	if err != nil || result.Status != 0 || len(result.Data) > moderationInteractionDataLimit || cborx.Unmarshal(result.Data, &data) != nil || len(data) != 3 || reader.actor != interactionID(6) || reader.limit != 1 || data[2] == nil {
		t.Fatalf("result=%+v data=%#v reader=%+v err=%v", result, data, reader, err)
	}
	row := data[1].([]any)[0].([]any)
	wantID := interactionID(50)
	if len(row) != 5 || !bytes.Equal(row[0].([]byte), wantID[:]) || row[1] != "warn" || row[2] != "moderator reason" {
		t.Fatalf("manual evidence response row = %#v", row)
	}
}

func TestManualEvidenceCursorIsInstallationAndCommunityBound(t *testing.T) {
	community, installation := interactionID(2), interactionID(3)
	cursor, ok := encodeEvidenceInteractionCursor(community, installation, []byte("state-key"))
	if !ok {
		t.Fatal("cursor encoding failed")
	}
	if after, valid := decodeEvidenceInteractionCursor(cursor, community, installation); !valid || string(after) != "state-key" {
		t.Fatalf("decoded cursor=%q valid=%v", after, valid)
	}
	if _, valid := decodeEvidenceInteractionCursor(cursor, interactionID(4), installation); valid {
		t.Fatal("cross-community cursor accepted")
	}
	if _, valid := decodeEvidenceInteractionCursor(cursor, community, interactionID(5)); valid {
		t.Fatal("cross-installation cursor accepted")
	}
}

func TestModerationInteractionsRejectsMalformedCursorWithoutReading(t *testing.T) {
	reader := &moderationInteractionReaderFake{}
	handler := (ModerationInteractions{Reader: reader, CommunityID: interactionID(2)}).Handlers()[protocol.OpModerationCases]
	result, err := handler.Handle(context.Background(), InteractionActor{User: interactionID(6)}, protocol.Request{CommunityID: interactionID(2), Args: []any{[]byte("not a cursor")}})
	if err != nil || result.Status != interactionStatusUnavailable || reader.actor != (id.ID{}) {
		t.Fatalf("result=%+v reader=%+v err=%v", result, reader, err)
	}
}

func TestModerationInteractionsRejectsCrossCommunityCursorWithoutReading(t *testing.T) {
	reader := &moderationInteractionReaderFake{}
	cursor, ok := encodeModerationInteractionCursor(interactionID(9), []byte("other-community-key"))
	if !ok {
		t.Fatal("could not construct test cursor")
	}
	handler := (ModerationInteractions{Reader: reader, CommunityID: interactionID(2)}).Handlers()[protocol.OpModerationCases]
	result, err := handler.Handle(context.Background(), InteractionActor{User: interactionID(6)}, protocol.Request{CommunityID: interactionID(2), Args: []any{cursor}})
	if err != nil || result.Status != interactionStatusUnavailable || reader.actor != (id.ID{}) {
		t.Fatalf("result=%+v reader=%+v err=%v", result, reader, err)
	}
}

func TestModerationInteractionsReturnsOpaqueCursorAndServesSecondPage(t *testing.T) {
	first := ModerationCase{Community: interactionID(2).String(), Group: interactionID(4).String(), Channel: interactionID(5).String(), Actor: interactionID(6).String(), Message: interactionID(7).String(), SourceRevision: 1, Action: moderation.ActionLog, Excerpt: "first", ReceivedAt: time.Unix(1_800_000_000, 0)}
	second := first
	second.SourceRevision = 2
	second.Excerpt = "second"
	reader := &moderationInteractionReaderFake{rows: []ModerationCase{first}, next: []byte("next-key")}
	handler := (ModerationInteractions{Reader: reader, CommunityID: interactionID(2)}).Handlers()[protocol.OpModerationCases]
	page1, err := handler.Handle(context.Background(), InteractionActor{User: interactionID(6)}, protocol.Request{CommunityID: interactionID(2), Args: []any{nil}})
	if err != nil {
		t.Fatal(err)
	}
	var firstData []any
	if err := cborx.Unmarshal(page1.Data, &firstData); err != nil {
		t.Fatal(err)
	}
	if len(firstData) != 3 || firstData[2] == nil {
		t.Fatalf("first page has no continuation cursor: %#v", firstData)
	}
	cursor, ok := firstData[2].([]byte)
	if !ok || len(cursor) == 0 {
		t.Fatalf("continuation cursor = %#v", firstData[2])
	}
	reader.rows = []ModerationCase{second}
	reader.next = nil
	page2, err := handler.Handle(context.Background(), InteractionActor{User: interactionID(6)}, protocol.Request{CommunityID: interactionID(2), Args: []any{cursor}})
	if err != nil || string(reader.after) != "next-key" || reader.actor != interactionID(6) || reader.limit != moderationInteractionPageSize {
		t.Fatalf("second page=%+v reader=%+v err=%v", page2, reader, err)
	}
	var secondData []any
	if err := cborx.Unmarshal(page2.Data, &secondData); err != nil {
		t.Fatal(err)
	}
	if len(secondData) != 3 || secondData[2] != nil || secondData[1].([]any)[0].([]any)[6] == nil {
		t.Fatalf("second page shape = %#v", secondData)
	}
}

func TestModerationInteractionsPreservesCursorWhenScanPageHasOnlyTombstones(t *testing.T) {
	reader := &moderationInteractionReaderFake{next: []byte("tombstone-key")}
	handler := (ModerationInteractions{Reader: reader, CommunityID: interactionID(2)}).Handlers()[protocol.OpModerationCases]
	result, err := handler.Handle(context.Background(), InteractionActor{User: interactionID(6)}, protocol.Request{CommunityID: interactionID(2), Args: []any{nil}})
	if err != nil {
		t.Fatal(err)
	}
	var data []any
	if err := cborx.Unmarshal(result.Data, &data); err != nil {
		t.Fatal(err)
	}
	if len(data) != 3 || len(data[1].([]any)) != 0 || data[2] == nil {
		t.Fatalf("tombstone-only page did not preserve scan cursor: %#v", data)
	}
}

func TestModerationInteractionsTruncatesAtEncodedLimitWithoutSkippingRows(t *testing.T) {
	reader := &moderationInteractionReaderFake{next: []byte("last-scanned-key")}
	for i := byte(1); i <= moderationInteractionPageSize; i++ {
		reader.rows = append(reader.rows, ModerationCase{
			Community: interactionID(2).String(), Group: interactionID(4).String(), Channel: interactionID(5).String(),
			Actor: interactionID(6).String(), Message: interactionID(i + 10).String(), SourceRevision: uint64(i),
			Action: moderation.ActionLog, Excerpt: strings.Repeat("x", moderation.ExcerptMaxBytes), ReceivedAt: time.Unix(1_800_000_000, 0),
		})
	}
	handler := (ModerationInteractions{Reader: reader, CommunityID: interactionID(2)}).Handlers()[protocol.OpModerationCases]
	result, err := handler.Handle(context.Background(), InteractionActor{User: interactionID(6)}, protocol.Request{CommunityID: interactionID(2), Args: []any{nil}})
	if err != nil || len(result.Data) > moderationInteractionDataLimit {
		t.Fatalf("encoded result bytes=%d err=%v", len(result.Data), err)
	}
	var data []any
	if err := cborx.Unmarshal(result.Data, &data); err != nil {
		t.Fatal(err)
	}
	rows := data[1].([]any)
	if len(rows) == 0 || len(rows) >= len(reader.rows) {
		t.Fatalf("encoded-size limit did not truncate a nonempty page: %d of %d", len(rows), len(reader.rows))
	}
	last := reader.rows[len(rows)-1]
	gotCursor, ok := decodeModerationInteractionCursor(data[2], interactionID(2))
	wantCursor := moderationCaseKey(last.Message, last.SourceRevision)
	if !ok || string(gotCursor) != string(wantCursor) {
		t.Fatalf("continuation cursor = %x, want last returned row key %x", gotCursor, wantCursor)
	}
}

func TestModerationInteractionsRechecksPermissionBeforeReturningSecondPage(t *testing.T) {
	state := &countedCaseState{moderationCaseNativeFake: newModerationCaseNative()}
	state.binding.Community = interactionID(2)
	sink, err := NewModerationCaseSink(state, func() time.Time { return time.Unix(1_800_000_100, 0) })
	if err != nil {
		t.Fatal(err)
	}
	for revision := uint64(1); revision <= moderationInteractionPageSize+1; revision++ {
		if err := sink.ApplyModerationAction(context.Background(), sampleModerationAction(state.moderationCaseNativeFake, revision)); err != nil {
			t.Fatal(err)
		}
	}
	authority := &caseReadAuthority{result: client.ActorAuthority{EffectiveBits: moderationViewAuditLogBit}, present: true}
	reader, err := NewModerationCaseReader(sink, authority)
	if err != nil {
		t.Fatal(err)
	}
	handler := (ModerationInteractions{Reader: reader, CommunityID: state.binding.Community}).Handlers()[protocol.OpModerationCases]
	actor := InteractionActor{User: interactionID(6)}
	page1, err := handler.Handle(context.Background(), actor, protocol.Request{CommunityID: state.binding.Community, Args: []any{nil}})
	if err != nil {
		t.Fatal(err)
	}
	var firstData []any
	if err := cborx.Unmarshal(page1.Data, &firstData); err != nil {
		t.Fatal(err)
	}
	cursor, ok := firstData[2].([]byte)
	if !ok || len(cursor) == 0 {
		t.Fatalf("first page cursor = %#v", firstData[2])
	}
	authority.result.EffectiveBits = 0
	page2, err := handler.Handle(context.Background(), actor, protocol.Request{CommunityID: state.binding.Community, Args: []any{cursor}})
	if !errors.Is(err, ErrModerationCaseUnauthorized) || page2.Data != nil || authority.calls != 2 || state.scans != 1 {
		t.Fatalf("revoked second page=%+v err=%v lookups=%d scans=%d", page2, err, authority.calls, state.scans)
	}
}

func TestModerationInteractionsFailClosedForMissingActorOrReaderError(t *testing.T) {
	for _, tc := range []struct {
		name  string
		actor id.ID
		err   error
	}{
		{name: "missing actor"},
		{name: "authorization denied", actor: interactionID(6), err: ErrModerationCaseUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reader := &moderationInteractionReaderFake{err: tc.err}
			handler := (ModerationInteractions{Reader: reader, CommunityID: interactionID(2)}).Handlers()[protocol.OpModerationCases]
			result, err := handler.Handle(context.Background(), InteractionActor{User: tc.actor}, protocol.Request{CommunityID: interactionID(2), Args: []any{nil}})
			if tc.actor.IsZero() {
				if err == nil {
					t.Fatal("missing actor was accepted")
				}
			} else if !errors.Is(err, ErrModerationCaseUnauthorized) || result.Data != nil {
				t.Fatalf("result=%+v err=%v", result, err)
			}
		})
	}
}

func TestModerationInteractionsEvidenceMutationFailsClosedWithoutReceiptLookup(t *testing.T) {
	handlers := (ModerationInteractions{Reader: &moderationInteractionReaderFake{}, CommunityID: interactionID(2)}).Handlers()
	handler, ok := handlers[protocol.OpModerationEvidence]
	if !ok {
		t.Fatal("manual evidence operation is not registered")
	}
	result, err := handler.Handle(context.Background(), InteractionActor{User: interactionID(3), Device: interactionID(4)}, protocol.Request{
		CommunityID: interactionID(2), Args: []any{interactionID(5), "reason", "excerpt"},
	})
	if err != nil || result.Status == 0 {
		t.Fatalf("unwired manual evidence handler result=%+v err=%v, want fail-closed refusal", result, err)
	}
}
