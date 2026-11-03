package service

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/bot/moderation"
	"github.com/dilla-chat/dilla/internal/bot/moderation/operationid"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
)

type moderationAcceptanceBackend struct {
	accepted AcceptedModerationAction
	err      error
	calls    int
}

func (b *moderationAcceptanceBackend) EnforceModerationAction(context.Context, ModerationAction) error {
	return b.err
}

func (b *moderationAcceptanceBackend) AcceptModerationAction(_ context.Context, action ModerationAction) (AcceptedModerationAction, error) {
	b.calls++
	return b.accepted, b.err
}

func TestModerationCaseRequiresMatchingServerAcceptance(t *testing.T) {
	for _, tc := range []struct {
		name       string
		accept     func(*moderationCaseNativeFake, ModerationAction) AcceptedModerationAction
		backendErr error
		wantErr    error
	}{
		{
			name: "matching accepted action",
			accept: func(n *moderationCaseNativeFake, action ModerationAction) AcceptedModerationAction {
				return AcceptedModerationAction{actionID: id.New(), serverRevision: 19, community: n.binding.Community,
					group: mustID(t, action.Group), targetSequence: action.SourceRevision, operationID: moderationRemovalOperationID(mustID(t, action.Group), mustID(t, action.Message), action.SourceRevision),
					action: moderation.ActionRemove}
			},
		},
		{
			name: "mismatched source sequence",
			accept: func(n *moderationCaseNativeFake, action ModerationAction) AcceptedModerationAction {
				return AcceptedModerationAction{actionID: id.New(), serverRevision: 19, community: n.binding.Community,
					group: mustID(t, action.Group), targetSequence: action.SourceRevision + 1, operationID: moderationRemovalOperationID(mustID(t, action.Group), mustID(t, action.Message), action.SourceRevision),
					action: moderation.ActionRemove}
			},
			wantErr: ErrModerationAcceptanceMismatch,
		},
		{
			name: "mismatched operation ID",
			accept: func(n *moderationCaseNativeFake, action ModerationAction) AcceptedModerationAction {
				return AcceptedModerationAction{actionID: id.New(), serverRevision: 19, community: n.binding.Community,
					group: mustID(t, action.Group), targetSequence: action.SourceRevision, operationID: id.New(), action: moderation.ActionRemove}
			},
			wantErr: ErrModerationAcceptanceMismatch,
		},
		{
			name: "wrong community",
			accept: func(n *moderationCaseNativeFake, action ModerationAction) AcceptedModerationAction {
				return AcceptedModerationAction{actionID: id.New(), serverRevision: 19, community: id.New(),
					group: mustID(t, action.Group), targetSequence: action.SourceRevision, operationID: moderationRemovalOperationID(mustID(t, action.Group), mustID(t, action.Message), action.SourceRevision),
					action: moderation.ActionRemove}
			},
			wantErr: ErrModerationAcceptanceMismatch,
		},
		{
			name:       "failed acceptance",
			backendErr: errors.New("server refused action"),
			wantErr:    ErrModerationEnforcementUnavailable,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := newModerationCaseNative()
			action := sampleModerationAction(n, 7)
			action.Decision.Action = moderation.ActionRemove
			action.ReceivedAt = time.Now().UTC().Truncate(time.Second)
			now := action.ReceivedAt
			backend := &moderationAcceptanceBackend{err: tc.backendErr}
			if tc.accept != nil {
				backend.accepted = tc.accept(n, action)
			}
			sink, err := NewModerationCaseSink(n, backend, func() time.Time { return now })
			if err != nil {
				t.Fatal(err)
			}
			err = sink.ApplyModerationAction(context.Background(), action)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("apply error = %v, want %v", err, tc.wantErr)
				}
				if len(n.rows) != 0 {
					t.Fatalf("failed/mismatched action stored %d cases", len(n.rows))
				}
				if len(n.evidence) != 0 {
					t.Fatalf("failed/mismatched action stored %d evidence records", len(n.evidence))
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			cases, _, err := sink.list(context.Background(), nil, 10)
			if err != nil || len(cases) != 1 {
				t.Fatalf("cases = %d, err=%v", len(cases), err)
			}
			if cases[0].ServerActionID != backend.accepted.actionID || cases[0].ServerRevision != backend.accepted.serverRevision ||
				cases[0].ServerOperationID != backend.accepted.operationID || cases[0].Group != action.Group {
				t.Fatalf("case missing matching server acceptance: %+v", cases[0])
			}
			if len(n.evidence) != 1 {
				t.Fatalf("accepted action evidence records = %d, want 1", len(n.evidence))
			}
			var evidence []any
			for _, value := range n.evidence {
				if err := cborx.Unmarshal(value.Value, &evidence); err != nil {
					t.Fatal(err)
				}
			}
			messageID := mustID(t, action.Message)
			if len(evidence) != 20 || !bytes.Equal(evidence[4].([]byte), backend.accepted.operationID[:]) ||
				!bytes.Equal(evidence[6].([]byte), messageID[:]) ||
				evidence[9] != "automatic moderation: mention_count" || evidence[10] != action.Excerpt {
				t.Fatalf("evidence is not attached to the accepted request: %#v", evidence)
			}
			if err := sink.ApplyModerationAction(context.Background(), action); err != nil {
				t.Fatalf("accepted request replay failed: %v", err)
			}
			if len(n.evidence) != 1 {
				t.Fatalf("accepted request replay created %d evidence records", len(n.evidence))
			}
		})
	}
}

func TestModerationCaseWarningPersistsWithoutManualRemovalEvidence(t *testing.T) {
	n := newModerationCaseNative()
	action := sampleModerationAction(n, 7)
	action.Decision.Action = moderation.ActionWarn
	action.ReceivedAt = time.Now().UTC().Truncate(time.Second)
	group, message := mustID(t, action.Group), mustID(t, action.Message)
	backend := &moderationAcceptanceBackend{accepted: AcceptedModerationAction{
		actionID: id.New(), serverRevision: 19, community: n.binding.Community, group: group,
		targetSequence: action.SourceRevision, operationID: operationid.Automatic(group, action.SourceRevision), action: moderation.ActionWarn,
	}}
	sink, err := NewModerationCaseSink(n, backend, func() time.Time { return action.ReceivedAt })
	if err != nil {
		t.Fatal(err)
	}
	if err := sink.ApplyModerationAction(context.Background(), action); err != nil {
		t.Fatalf("accepted warning case failed: %v", err)
	}
	cases, _, err := sink.list(context.Background(), nil, 10)
	if err != nil || len(cases) != 1 || cases[0].Message != message.String() || cases[0].Action != moderation.ActionWarn {
		t.Fatalf("warning cases = %+v, err=%v", cases, err)
	}
	if len(n.evidence) != 0 {
		t.Fatalf("warning created %d removal evidence records", len(n.evidence))
	}
}

func TestModerationCaseDoesNotTrustLegacyErrorOnlyBackend(t *testing.T) {
	n := newModerationCaseNative()
	action := sampleModerationAction(n, 7)
	action.Decision.Action = moderation.ActionRemove
	legacy := moderationCaseEnforcerFunc(func(context.Context, ModerationAction) error { return nil })
	sink, err := NewModerationCaseSink(n, legacy, func() time.Time { return time.Unix(1_800_000_100, 0) })
	if err != nil {
		t.Fatal(err)
	}
	if err := sink.ApplyModerationAction(context.Background(), action); !errors.Is(err, ErrModerationEnforcementUnavailable) {
		t.Fatalf("error-only backend created an action case: %v", err)
	}
	if len(n.rows) != 0 {
		t.Fatalf("error-only backend stored %d action cases", len(n.rows))
	}
}

func TestModerationCaseRejectsForgedActionWithoutRuntimeProvenance(t *testing.T) {
	n := newModerationCaseNative()
	action := sampleModerationAction(n, 7)
	action.runtimeGenerated = false
	sink, err := NewModerationCaseSink(n, nil, func() time.Time { return time.Unix(1_800_000_100, 0) })
	if err != nil {
		t.Fatal(err)
	}
	if err := sink.ApplyModerationAction(context.Background(), action); !errors.Is(err, ErrModerationCaseState) {
		t.Fatalf("forged action created case: %v", err)
	}
	if len(n.rows) != 0 {
		t.Fatalf("forged action stored %d cases", len(n.rows))
	}
	if len(n.evidence) != 0 {
		t.Fatalf("forged action stored %d evidence records", len(n.evidence))
	}
}

func mustID(t *testing.T, raw string) id.ID {
	t.Helper()
	v, err := id.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

var _ moderationCaseNative = (*moderationCaseNativeFake)(nil)
