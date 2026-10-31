package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jonasthim/dilla/internal/bot/client"
	"github.com/jonasthim/dilla/internal/bot/moderation"
	"github.com/jonasthim/dilla/internal/bot/native"
	"github.com/jonasthim/dilla/internal/id"
)

type removalSignerFake struct {
	binding native.Binding
	groups  []id.ID
	targets []uint64
	ids     []id.ID
	expires []uint64
}

func (f *removalSignerFake) Binding() native.Binding { return f.binding }
func (f *removalSignerFake) SignRemoval(_ context.Context, request, group id.ID, target, expires, now uint64) ([]byte, error) {
	f.ids = append(f.ids, request)
	f.groups = append(f.groups, group)
	f.targets = append(f.targets, target)
	f.expires = append(f.expires, expires)
	return []byte{request[0], group[0], byte(target), byte(expires >> 24), byte(expires >> 16), byte(expires >> 8), byte(expires)}, nil
}

type removalSessionFake struct {
	bodies       [][]byte
	botExpires   []uint64
	botNow       []uint64
	err          error
	errs         []error
	receipt      client.ModerationRemovalReceipt
	emptyReceipt bool
}

func (f *removalSessionFake) AcceptBotModerationAction(_ context.Context, _ id.ID, _ id.ID, _ id.ID, _ uint64, _ string, _ []string, _ uint64, expires uint64) (client.ModerationActionReceipt, error) {
	f.botExpires = append(f.botExpires, expires)
	f.botNow = append(f.botNow, uint64(time.Now().Unix()))
	if f.err != nil {
		return client.ModerationActionReceipt{}, f.err
	}
	if f.emptyReceipt {
		return client.ModerationActionReceipt{}, nil
	}
	if !f.receipt.ActionID.IsZero() {
		return client.ModerationActionReceipt{ActionID: f.receipt.ActionID, Revision: f.receipt.Revision}, nil
	}
	return client.ModerationActionReceipt{ActionID: testID(88), Revision: 23}, nil
}

func (f *removalSessionFake) AcceptModerationRemoval(_ context.Context, _ id.ID, body []byte) (client.ModerationRemovalReceipt, error) {
	f.bodies = append(f.bodies, append([]byte(nil), body...))
	if len(f.errs) > 0 {
		err := f.errs[0]
		f.errs = f.errs[1:]
		if err != nil {
			return client.ModerationRemovalReceipt{}, err
		}
	}
	if f.err != nil {
		return client.ModerationRemovalReceipt{}, f.err
	}
	if f.receipt.ActionID.IsZero() {
		if f.emptyReceipt {
			return client.ModerationRemovalReceipt{}, nil
		}
		return client.ModerationRemovalReceipt{ActionID: testID(88), Revision: 23}, nil
	}
	return f.receipt, nil
}

func removalAction(action moderation.Action) ModerationAction {
	now := time.Unix(1_800_000_000, 0)
	return ModerationAction{Community: testID(1).String(), Group: testID(2).String(), Channel: testID(3).String(),
		Actor: testID(4).String(), Message: testID(5).String(), SourceRevision: 17,
		Decision: moderation.Decision{Action: action, Rules: []moderation.Rule{moderation.RuleRate}}, ReceivedAt: now, runtimeGenerated: true}
}

func TestModerationRemovalBackendRefusesUnsupportedActions(t *testing.T) {
	n := &removalSignerFake{binding: native.Binding{Instance: testID(8), Community: testID(1), Installation: testID(9)}}
	s := &removalSessionFake{}
	b, err := newModerationRemovalBackend(n, s)
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range []moderation.Action{moderation.ActionLog} {
		if err := b.EnforceModerationAction(context.Background(), removalAction(action)); !errors.Is(err, ErrModerationActionUnsupported) {
			t.Fatalf("action %q error = %v, want unsupported", action, err)
		}
	}
	if len(n.ids) != 0 || len(s.bodies) != 0 {
		t.Fatalf("unsupported action reached signer/server: signs=%d posts=%d", len(n.ids), len(s.bodies))
	}
}

func TestModerationEnforcementAcceptsRuntimeGeneratedWarnAndTimeout(t *testing.T) {
	binding := native.Binding{Instance: testID(8), Community: testID(1), Installation: testID(9)}
	n := &removalSignerFake{binding: binding}
	s := &removalSessionFake{}
	b, err := newModerationRemovalBackend(n, s)
	if err != nil {
		t.Fatal(err)
	}
	for _, actionKind := range []moderation.Action{moderation.ActionWarn, moderation.ActionTimeout} {
		a := removalAction(actionKind)
		if actionKind == moderation.ActionTimeout {
			a.Decision.Timeout = 10 * time.Minute
		}
		accepted, err := b.AcceptModerationAction(context.Background(), a)
		if err != nil {
			t.Fatalf("%s: %v", actionKind, err)
		}
		if !accepted.matches(a, binding) || accepted.action != actionKind {
			t.Fatalf("%s acceptance mismatch: %+v", actionKind, accepted)
		}
	}
	if len(s.botExpires) != 2 {
		t.Fatalf("captured automatic action expiries = %d, want 2", len(s.botExpires))
	}
	for i, expires := range s.botExpires {
		if expires <= s.botNow[i] || expires-s.botNow[i] > 25 {
			t.Fatalf("automatic request expiry delta = %d seconds, want 1..25", expires-s.botNow[i])
		}
	}
	bad := removalAction(moderation.ActionWarn)
	bad.runtimeGenerated = false
	if _, err := b.AcceptModerationAction(context.Background(), bad); !errors.Is(err, ErrModerationActionSource) {
		t.Fatalf("untrusted warning = %v", err)
	}
}

func TestModerationRemovalBackendUsesStableBoundOperationOnRetry(t *testing.T) {
	n := &removalSignerFake{binding: native.Binding{Instance: testID(8), Community: testID(1), Installation: testID(9)}}
	s := &removalSessionFake{err: errors.New("response lost")}
	b, err := newModerationRemovalBackend(n, s)
	if err != nil {
		t.Fatal(err)
	}
	a := removalAction(moderation.ActionRemove)
	if err = b.EnforceModerationAction(context.Background(), a); err == nil {
		t.Fatal("first uncertain result returned nil")
	}
	s.err = nil
	if err = b.EnforceModerationAction(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	if len(n.ids) != 2 || n.ids[0] != n.ids[1] || len(s.bodies) != 2 || string(s.bodies[0]) != string(s.bodies[1]) {
		t.Fatalf("retry changed operation: signer ids=%v bodies=%v", n.ids, s.bodies)
	}
	if n.groups[0] != testID(2) || n.targets[0] != 17 {
		t.Fatalf("signed wrong source: group=%s target=%d", n.groups[0], n.targets[0])
	}
}

func TestModerationRemovalBackendReturnsTypedServerAcceptance(t *testing.T) {
	binding := native.Binding{Instance: testID(8), Community: testID(1), Installation: testID(9)}
	n := &removalSignerFake{binding: binding}
	s := &removalSessionFake{receipt: client.ModerationRemovalReceipt{ActionID: testID(77), Revision: 31}}
	b, err := newModerationRemovalBackend(n, s)
	if err != nil {
		t.Fatal(err)
	}
	action := removalAction(moderation.ActionRemove)
	accepted, err := b.AcceptModerationAction(context.Background(), action)
	if err != nil {
		t.Fatal(err)
	}
	if !accepted.matches(action, binding) || accepted.actionID != s.receipt.ActionID || accepted.serverRevision != s.receipt.Revision {
		t.Fatalf("typed acceptance = %+v", accepted)
	}
	s.receipt = client.ModerationRemovalReceipt{}
	s.emptyReceipt = true
	if _, err := b.AcceptModerationAction(context.Background(), action); !errors.Is(err, client.ErrProtocol) {
		t.Fatalf("empty server receipt error = %v", err)
	}
}

func TestModerationRemovalBackendNeverExtendsExpiredPendingProof(t *testing.T) {
	n := &removalSignerFake{binding: native.Binding{Instance: testID(8), Community: testID(1), Installation: testID(9)}}
	session := &removalSessionFake{errs: []error{&client.APIError{Code: "E_REMOVAL_PROOF_EXPIRED", Outcome: client.OutcomeDefinitive}, nil}}
	b, err := newModerationRemovalBackend(n, session)
	if err != nil {
		t.Fatal(err)
	}
	action := removalAction(moderation.ActionRemove)
	action.ReceivedAt = time.Now().Add(-time.Minute)
	if err = b.EnforceModerationAction(t.Context(), action); !errors.Is(err, ErrModerationActionRefused) {
		t.Fatalf("expired proof must be terminal: %v", err)
	}
	if len(n.ids) != 1 || len(session.bodies) != 1 {
		t.Fatal("expired acceptance window extended")
	}
}

func TestModerationRemovalBackendDoesNotResignAfterUncertainOutcome(t *testing.T) {
	n := &removalSignerFake{binding: native.Binding{Instance: testID(8), Community: testID(1), Installation: testID(9)}}
	s := &removalSessionFake{err: errors.New("network outcome unknown")}
	b, err := newModerationRemovalBackend(n, s)
	if err != nil {
		t.Fatal(err)
	}
	a := removalAction(moderation.ActionRemove)
	if err := b.EnforceModerationAction(context.Background(), a); err == nil {
		t.Fatal("uncertain result returned nil")
	}
	if len(n.ids) != 1 || len(s.bodies) != 1 {
		t.Fatalf("uncertain attempt signed %d times and posted %d times", len(n.ids), len(s.bodies))
	}
}

func TestModerationRemovalBackendClassifiesOnlyDefinitiveForbiddenAsRefusal(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{name: "revoked capability", err: &client.APIError{Status: 403, Code: "E_FORBIDDEN", Outcome: client.OutcomeDefinitive}, want: true},
		{name: "unknown outcome", err: &client.APIError{Status: 503, Code: "E_OUTCOME_UNKNOWN", Outcome: client.OutcomeUnknown}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := &removalSignerFake{binding: native.Binding{Instance: testID(8), Community: testID(1), Installation: testID(9)}}
			s := &removalSessionFake{err: tc.err}
			b, err := newModerationRemovalBackend(n, s)
			if err != nil {
				t.Fatal(err)
			}
			_, err = b.AcceptModerationAction(context.Background(), removalAction(moderation.ActionRemove))
			if errors.Is(err, ErrModerationActionRefused) != tc.want {
				t.Fatalf("refusal classification = %v, want %v (err=%v)", errors.Is(err, ErrModerationActionRefused), tc.want, err)
			}
		})
	}
}

func TestModerationRemovalOperationIDChangesWithSourceRevision(t *testing.T) {
	group, message := testID(2), testID(5)
	first := moderationRemovalOperationID(group, message, 17)
	if first.IsZero() || first != moderationRemovalOperationID(group, message, 17) {
		t.Fatal("operation id is not stable for the same source revision")
	}
	if first == moderationRemovalOperationID(group, message, 18) || first == moderationRemovalOperationID(group, testID(6), 17) {
		t.Fatal("different source event/revision reused operation id")
	}
}

func TestModerationRemovalBackendRejectsMissingOrCrossBindingSource(t *testing.T) {
	n := &removalSignerFake{binding: native.Binding{Instance: testID(8), Community: testID(1), Installation: testID(9)}}
	s := &removalSessionFake{}
	b, err := newModerationRemovalBackend(n, s)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range []ModerationAction{func() ModerationAction { v := removalAction(moderation.ActionRemove); v.SourceRevision = 0; return v }(),
		func() ModerationAction { v := removalAction(moderation.ActionRemove); v.Group = ""; return v }(),
		func() ModerationAction {
			v := removalAction(moderation.ActionRemove)
			v.Community = testID(99).String()
			return v
		}()} {
		if err := b.EnforceModerationAction(context.Background(), a); !errors.Is(err, ErrModerationActionSource) {
			t.Fatalf("invalid source error = %v, want source refusal", err)
		}
	}
	if len(n.ids) != 0 || len(s.bodies) != 0 {
		t.Fatal("invalid source reached signer/server")
	}
}
