package service

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jonasthim/dilla/internal/bot/client"
	"github.com/jonasthim/dilla/internal/bot/moderation"
	"github.com/jonasthim/dilla/internal/bot/native"
	"github.com/jonasthim/dilla/internal/cborx"
	"github.com/jonasthim/dilla/internal/id"
)

func TestReviewTransientModerationActionIsRetried(t *testing.T) {
	now := time.Unix(1800000000, 0)
	calls := 0
	r := NewAutoModerationRuntime(moderation.Config{Enabled: true, Actions: map[moderation.Rule]moderation.Action{moderation.RuleMentions: moderation.ActionWarn}}, automodAuthorityFunc(ordinaryAutomodAuthority), automodSinkFunc(func(context.Context, ModerationAction) error {
		calls++
		if calls == 1 {
			return errors.New("temporary persistence failure")
		}
		return nil
	}), func() time.Time { return now })
	body := "message"
	e := native.GroupEvent{DeliveryID: 1, Community: id.New(), Group: id.New(), Channel: id.New(), SourceSequence: 1, ReceivedAt: uint64(now.Unix()), SenderUser: id.New(), SenderDevice: id.New(), Body: &body, Message: id.New()}
	mentions := []string{"1", "2", "3", "4", "5", "6"}
	if _, err := r.Handle(t.Context(), e, false, mentions); err == nil {
		t.Fatal("first action must fail")
	}
	decision, err := r.Handle(t.Context(), e, false, mentions)
	if calls != 2 || err != nil {
		t.Fatalf("transient failure was not retried: sink calls=%d, duplicate=%v, retry error=%v", calls, decision.DuplicateDelivery, err)
	}
}

// This store models the native atomic CAS and transaction receipt semantics.
// It intentionally supports losing a reply after committing every changed row.
type automodStateFake struct {
	binding               native.Binding
	rows                  map[string]native.StateValue
	receipts              map[id.ID][]uint64
	failBefore, loseReply bool
	scanLimit             uint64
}

func newAutomodState() *automodStateFake {
	return &automodStateFake{binding: native.Binding{Instance: id.New(), Community: id.New(), Installation: id.New()}, rows: map[string]native.StateValue{}, receipts: map[id.ID][]uint64{}}
}
func (s *automodStateFake) Binding() native.Binding { return s.binding }
func (s *automodStateFake) StateGet(_ context.Context, _ uint64, key []byte) (native.StateValue, error) {
	return s.rows[string(key)], nil
}
func (s *automodStateFake) StateScan(_ context.Context, _ uint64, after []byte, limit uint64) (native.StatePage, error) {
	if s.scanLimit > 0 && limit > s.scanLimit {
		limit = s.scanLimit
	}
	keys := []string{}
	for k := range s.rows {
		if k > string(after) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	page := native.StatePage{}
	if len(keys) > int(limit) {
		keys = keys[:limit]
	}
	for _, k := range keys {
		v := s.rows[k]
		page.Entries = append(page.Entries, native.StateEntry{Key: []byte(k), Revision: v.Revision, Value: v.Value, Tombstone: v.Tombstone})
	}
	if len(keys) == int(limit) {
		page.Next = []byte(keys[len(keys)-1])
	}
	return page, nil
}
func (s *automodStateFake) StateTransaction(_ context.Context, tx id.ID, changes []native.StateChange) ([]uint64, error) {
	if s.failBefore {
		return nil, errors.New("before commit")
	}
	if revs, ok := s.receipts[tx]; ok {
		if s.loseReply {
			return nil, errors.New("lost reply")
		}
		return revs, nil
	}
	for _, ch := range changes {
		if s.rows[string(ch.Key)].Revision != ch.ExpectedRevision {
			return nil, errors.New("conflict")
		}
	}
	revs := []uint64{}
	for _, ch := range changes {
		v := native.StateValue{Revision: ch.ExpectedRevision + 1, Value: append([]byte(nil), ch.Value...), Tombstone: ch.Value == nil}
		s.rows[string(ch.Key)] = v
		revs = append(revs, v.Revision)
	}
	s.receipts[tx] = revs
	if s.loseReply {
		return nil, errors.New("lost reply")
	}
	return revs, nil
}
func durableRuntime(t *testing.T, s *automodStateFake, cfg moderation.Config, sink ModerationActionSink, now *time.Time) *AutoModerationRuntime {
	t.Helper()
	r := NewAutoModerationRuntime(cfg, automodAuthorityFunc(ordinaryAutomodAuthority), sink, func() time.Time { return *now })
	if err := r.AttachDurableState(t.Context(), s); err != nil {
		t.Fatal(err)
	}
	return r
}
func automodEvent(s *automodStateFake, now time.Time) native.GroupEvent {
	body := "test"
	return native.GroupEvent{DeliveryID: 1, Community: s.binding.Community, Group: id.New(), Channel: id.New(), SourceSequence: 1, ReceivedAt: uint64(now.Unix()), SenderUser: id.New(), SenderDevice: id.New(), Body: &body, Message: id.New()}
}
func TestAutoModerationPendingSurvivesRestartAndLostCASReplies(t *testing.T) {
	for _, lost := range []bool{false, true} {
		t.Run(fmt.Sprint(lost), func(t *testing.T) {
			state := newAutomodState()
			now := time.Unix(1800000000, 0)
			cfg := moderation.Config{Enabled: true, Actions: map[moderation.Rule]moderation.Action{moderation.RuleMentions: moderation.ActionWarn}}
			calls := 0
			sink := automodSinkFunc(func(context.Context, ModerationAction) error {
				calls++
				if calls == 1 {
					return errors.New("temporary")
				}
				return nil
			})
			r := durableRuntime(t, state, cfg, sink, &now)
			state.loseReply = lost
			e := automodEvent(state, now)
			mentions := []string{"1", "2", "3", "4", "5", "6"}
			if _, err := r.Handle(t.Context(), e, false, mentions); err == nil {
				t.Fatal("missing temporary failure")
			}
			r = durableRuntime(t, state, cfg, sink, &now)
			if _, err := r.Handle(t.Context(), e, true, mentions); err != nil {
				t.Fatal(err)
			}
			if calls != 2 {
				t.Fatalf("calls=%d", calls)
			}
			r = durableRuntime(t, state, cfg, sink, &now)
			if _, err := r.Handle(t.Context(), e, false, mentions); err != nil {
				t.Fatal(err)
			}
			if calls != 2 || len(r.pending) != 0 {
				t.Fatalf("completed delivery repeated: calls=%d pending=%d", calls, len(r.pending))
			}
		})
	}
}
func TestAutoModerationCheckpointFailureDoesNotCountOrACK(t *testing.T) {
	state := newAutomodState()
	now := time.Unix(1800000000, 0)
	calls := 0
	cfg := moderation.Config{Enabled: true, Actions: map[moderation.Rule]moderation.Action{moderation.RuleRate: moderation.ActionWarn}}
	sink := automodSinkFunc(func(_ context.Context, a ModerationAction) error {
		if a.Decision.Action == moderation.ActionWarn {
			calls++
		}
		return nil
	})
	r := durableRuntime(t, state, cfg, sink, &now)
	e := automodEvent(state, now)
	for i := uint64(1); i <= 5; i++ {
		e.DeliveryID = i
		e.SourceSequence = i
		e.Message = id.New()
		if _, err := r.Handle(t.Context(), e, false, nil); err != nil {
			t.Fatal(err)
		}
	}
	e.DeliveryID = 6
	e.SourceSequence = 6
	e.Message = id.New()
	state.failBefore = true
	if _, err := r.Handle(t.Context(), e, false, nil); err == nil || calls != 0 {
		t.Fatalf("unpersisted evaluation escaped: calls=%d err=%v", calls, err)
	}
	state.failBefore = false
	r = durableRuntime(t, state, cfg, sink, &now)
	d, err := r.Handle(t.Context(), e, false, nil)
	if err != nil || d.Action != moderation.ActionWarn || calls != 1 {
		t.Fatalf("restored rolling state %+v calls=%d err=%v", d, calls, err)
	}
}

type failEvidenceNative struct {
	*moderationCaseNativeFake
	fail bool
}

func (n *failEvidenceNative) StateTransaction(ctx context.Context, tx id.ID, changes []native.StateChange) ([]uint64, error) {
	for _, c := range changes {
		if n.fail && c.Area == 9 {
			return nil, errors.New("evidence unavailable")
		}
	}
	return n.moderationCaseNativeFake.StateTransaction(ctx, tx, changes)
}
func TestModerationCaseAcceptedReceiptResumesEvidenceWithoutAuthority(t *testing.T) {
	n := &failEvidenceNative{moderationCaseNativeFake: newModerationCaseNative(), fail: true}
	action := sampleModerationAction(n.moderationCaseNativeFake, 7)
	action.ReceivedAt = time.Now().UTC().Truncate(time.Second)
	action.Decision.Action = moderation.ActionRemove
	backend := &moderationAcceptanceBackend{accepted: AcceptedModerationAction{actionID: id.New(), serverRevision: 19, community: n.binding.Community, group: mustID(t, action.Group), targetSequence: action.SourceRevision, operationID: moderationRemovalOperationID(mustID(t, action.Group), mustID(t, action.Message), action.SourceRevision), action: moderation.ActionRemove}}
	sink, err := NewModerationCaseSink(n, backend, func() time.Time { return action.ReceivedAt })
	if err != nil {
		t.Fatal(err)
	}
	if err = sink.ApplyModerationAction(t.Context(), action); err == nil {
		t.Fatal("missing evidence failure")
	}
	n.fail = false
	backend.err = ErrModerationActionRefused
	action.receiptOnly = true
	if err = sink.ApplyModerationAction(t.Context(), action); err != nil {
		t.Fatal(err)
	}
	if backend.calls != 1 || len(n.rows) != 1 || len(n.evidence) != 1 {
		t.Fatalf("accepted effect repeated/lost: calls=%d cases=%d evidence=%d", backend.calls, len(n.rows), len(n.evidence))
	}
}

func TestAutoModerationPendingCapacityDefersBeforeEvaluationAcrossRestart(t *testing.T) {
	s := newAutomodState()
	now := time.Unix(1800000000, 0)
	cfg := moderation.Config{Enabled: true, Actions: map[moderation.Rule]moderation.Action{moderation.RuleMentions: moderation.ActionWarn}}
	sink := automodSinkFunc(func(context.Context, ModerationAction) error { return errors.New("offline") })
	r := durableRuntime(t, s, cfg, sink, &now)
	e := automodEvent(s, now)
	mentions := []string{"1", "2", "3", "4", "5", "6"}
	for i := uint64(1); i <= MaxModerationPending; i++ {
		e.DeliveryID = i
		e.SourceSequence = i
		e.Message = id.New()
		if _, err := r.Handle(t.Context(), e, false, mentions); err == nil {
			t.Fatal("missing offline refusal")
		}
	}
	s.scanLimit = 2 // Native pages can be byte-limited well below the entry limit.
	for restart := 0; restart < 2; restart++ {
		e.DeliveryID = MaxModerationPending + 1
		e.SourceSequence = e.DeliveryID
		e.Message = id.New()
		_, err := r.Handle(t.Context(), e, false, mentions)
		if !errors.Is(err, ErrModerationPendingCapacity) || r.state.meta.Frontier != MaxModerationPending {
			t.Fatalf("capacity advanced evaluation: %v frontier=%d", err, r.state.meta.Frontier)
		}
		r = durableRuntime(t, s, cfg, sink, &now)
	}
	if len(s.rows) > MaxModerationPending+257 {
		t.Fatalf("state grew beyond fixed slots: %d", len(s.rows))
	}
}

func TestAutoModerationActivationBoundaryIsDurableBeforeAcknowledgement(t *testing.T) {
	s := newAutomodState()
	now := time.Unix(1800000000, 0)
	cfg := moderation.Config{Enabled: true}
	sink := automodSinkFunc(func(context.Context, ModerationAction) error { return nil })
	r := NewAutoModerationRuntime(cfg, automodAuthorityFunc(ordinaryAutomodAuthority), sink, func() time.Time { return now })
	activate := func(r *AutoModerationRuntime, revision, through uint64) {
		t.Helper()
		_, err := r.ActivateConfig(t.Context(), AutoModerationActivation{Config: cfg, Revision: revision, ReplayThrough: through, ReplayReceivedThrough: uint64(now.UnixMilli()), Authority: automodAuthorityFunc(ordinaryAutomodAuthority), Sink: sink}, func() error { return nil })
		if err != nil {
			t.Fatal(err)
		}
	}
	activate(r, 7, 10)
	if err := r.AttachDurableState(t.Context(), s); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	activate(r, 8, 20)
	boundary := r.replayReceivedThrough
	now = now.Add(time.Minute)
	restarted := NewAutoModerationRuntime(cfg, automodAuthorityFunc(ordinaryAutomodAuthority), sink, func() time.Time { return now })
	activate(restarted, 8, 100)
	if err := restarted.AttachDurableState(t.Context(), s); err != nil {
		t.Fatal(err)
	}
	through, received := restarted.ReplayBoundary()
	if through != 20 || received != boundary {
		t.Fatalf("restart slid acknowledged boundary: %d/%d", through, received)
	}
}

func TestModerationCaseDurablyDistinguishesRefusedEnforcement(t *testing.T) {
	n := newModerationCaseNative()
	action := sampleModerationAction(n, 1)
	action.Decision.EnforcementRefused = true
	sink, err := NewModerationCaseSink(n, nil, func() time.Time { return action.ReceivedAt })
	if err != nil {
		t.Fatal(err)
	}
	if err = sink.ApplyModerationAction(t.Context(), action); err != nil {
		t.Fatal(err)
	}
	rows, _, err := sink.list(t.Context(), nil, 10)
	if err != nil || len(rows) != 1 || !strings.Contains(rows[0].Reason(), "refused") {
		t.Fatalf("refusal indistinguishable from log: rows=%+v err=%v", rows, err)
	}
}

type refusalBackendFunc func(context.Context, ModerationAction) (AcceptedModerationAction, error)

func (f refusalBackendFunc) AcceptModerationAction(ctx context.Context, a ModerationAction) (AcceptedModerationAction, error) {
	return f(ctx, a)
}

type membershipLossSigner struct {
	*removalSignerFake
	left bool
}

func (s *membershipLossSigner) SignRemoval(ctx context.Context, request, group id.ID, target, expires, now uint64) ([]byte, error) {
	if s.left {
		return nil, errors.New("local MLS membership is gone")
	}
	return s.removalSignerFake.SignRemoval(ctx, request, group, target, expires, now)
}

func TestAutoModerationRemovalReceiptSurvivesLocalMembershipLoss(t *testing.T) {
	n := newModerationCaseNative()
	state := newAutomodState()
	state.binding = n.binding
	now := time.Now().Truncate(time.Second)
	cfg := moderation.Config{Enabled: true, Actions: map[moderation.Rule]moderation.Action{moderation.RuleMentions: moderation.ActionRemove}}
	signer := &membershipLossSigner{removalSignerFake: &removalSignerFake{binding: n.binding}}
	session := &lookupModerationSession{removalSessionFake: removalSessionFake{errs: []error{errors.New("accepted but reply lost")}}}
	backend, err := newModerationRemovalBackend(signer, session)
	if err != nil {
		t.Fatal(err)
	}
	sink, err := NewModerationCaseSink(n, backend, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	r := durableRuntime(t, state, cfg, sink, &now)
	e := automodEvent(state, now)
	mentions := []string{"1", "2", "3", "4", "5", "6"}
	if _, err = r.Handle(t.Context(), e, false, mentions); err == nil {
		t.Fatal("missing uncertain result")
	}
	if len(session.bodies) != 1 || len(signer.ids) != 1 {
		t.Fatalf("initial effects=%d signatures=%d", len(session.bodies), len(signer.ids))
	}
	signer.left = true
	now = now.Add(16 * time.Second)
	r = durableRuntime(t, state, cfg, sink, &now)
	d, err := r.Handle(t.Context(), e, true, mentions)
	if err != nil || d.EnforcementRefused || session.lookups != 1 || len(session.bodies) != 1 || len(signer.ids) != 1 || len(r.pending) != 0 {
		t.Fatalf("accepted removal lost after local leave: %+v err=%v lookups=%d effects=%d signatures=%d pending=%d", d, err, session.lookups, len(session.bodies), len(signer.ids), len(r.pending))
	}
}
func (f refusalBackendFunc) EnforceModerationAction(ctx context.Context, a ModerationAction) error {
	_, err := f(ctx, a)
	return err
}

func TestAutoModerationRefusalSurvivesFailureBeforePendingDeletion(t *testing.T) {
	n := newModerationCaseNative()
	state := newAutomodState()
	state.binding = n.binding
	now := time.Unix(1800000000, 0)
	cfg := moderation.Config{Enabled: true, Actions: map[moderation.Rule]moderation.Action{moderation.RuleMentions: moderation.ActionWarn}}
	attempts := 0
	sink, err := NewModerationCaseSink(n, refusalBackendFunc(func(context.Context, ModerationAction) (AcceptedModerationAction, error) {
		attempts++
		state.failBefore = true // Refusal case commits, but releasing the durable pending slot fails.
		return AcceptedModerationAction{}, ErrModerationActionRefused
	}), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	e := automodEvent(state, now)
	mentions := []string{"1", "2", "3", "4", "5", "6"}
	r := durableRuntime(t, state, cfg, sink, &now)
	if _, err = r.Handle(t.Context(), e, false, mentions); err == nil {
		t.Fatal("pending deletion unexpectedly succeeded")
	}
	state.failBefore = false
	r = durableRuntime(t, state, cfg, sink, &now)
	d, err := r.Handle(t.Context(), e, true, mentions)
	if err != nil || !d.EnforcementRefused || attempts != 1 || len(r.pending) != 0 {
		t.Fatalf("durable refusal did not finish: decision=%+v attempts=%d pending=%d err=%v", d, attempts, len(r.pending), err)
	}
	rows, _, err := sink.list(t.Context(), nil, 10)
	if err != nil || len(rows) != 1 || !rows[0].EnforcementRefused {
		t.Fatalf("refusal cases=%+v err=%v", rows, err)
	}
}

func TestAutoModerationPendingRefusesChangedPolicyExpiredSourceAndPrivilegedActor(t *testing.T) {
	for _, why := range []string{"policy", "expiry", "authority"} {
		t.Run(why, func(t *testing.T) {
			s := newAutomodState()
			now := time.Unix(1800000000, 0)
			effects, refusals := 0, 0
			cfg := moderation.Config{Enabled: true, Actions: map[moderation.Rule]moderation.Action{moderation.RuleMentions: moderation.ActionWarn}}
			sink := automodSinkFunc(func(_ context.Context, a ModerationAction) error {
				if a.Decision.Action == moderation.ActionLog {
					if a.Decision.EnforcementRefused {
						refusals++
					}
					return nil
				}
				effects++
				return errors.New("uncertain")
			})
			r := durableRuntime(t, s, cfg, sink, &now)
			e := automodEvent(s, now)
			mentions := []string{"1", "2", "3", "4", "5", "6"}
			if _, err := r.Handle(t.Context(), e, false, mentions); err == nil {
				t.Fatal("expected uncertain failure")
			}
			switch why {
			case "expiry":
				now = now.Add(16 * time.Second)
			case "authority":
				r.UpdateActionDependencies(automodAuthorityFunc(func(context.Context, native.GroupEvent) (bool, bool, error) { return true, false, nil }), sink)
			case "policy":
				_, err := r.ActivateConfig(t.Context(), AutoModerationActivation{Config: moderation.Config{}, Revision: 1, Sink: sink}, func() error { return nil })
				if err != nil {
					t.Fatal(err)
				}
			}
			d, err := r.Handle(t.Context(), e, true, mentions)
			if err != nil || !d.EnforcementRefused || effects != 1 || refusals != 1 || len(r.pending) != 0 {
				t.Fatalf("old action escaped %s: %+v effects=%d refusals=%d pending=%d err=%v", why, d, effects, refusals, len(r.pending), err)
			}
		})
	}
}

func TestAutoModerationConcurrentRetryCountsOneAcceptedEffect(t *testing.T) {
	s := newAutomodState()
	now := time.Unix(1800000000, 0)
	calls, accepted := 0, 0
	cfg := moderation.Config{Enabled: true, Actions: map[moderation.Rule]moderation.Action{moderation.RuleMentions: moderation.ActionWarn}}
	sink := automodSinkFunc(func(context.Context, ModerationAction) error {
		calls++
		if calls == 1 {
			return errors.New("temporary")
		}
		accepted++
		return nil
	})
	r := durableRuntime(t, s, cfg, sink, &now)
	e := automodEvent(s, now)
	mentions := []string{"1", "2", "3", "4", "5", "6"}
	done := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() { _, err := r.Handle(t.Context(), e, false, mentions); done <- err }()
	}
	failures := 0
	for i := 0; i < 2; i++ {
		if <-done != nil {
			failures++
		}
	}
	if failures != 1 || accepted != 1 || len(r.pending) != 0 {
		t.Fatalf("concurrent retry: failures=%d accepted=%d", failures, accepted)
	}
}

type lookupModerationSession struct {
	removalSessionFake
	lookups int
	missing bool
}

func (s *lookupModerationSession) LookupModerationRemoval(context.Context, id.ID, []byte) (client.ModerationRemovalReceipt, error) {
	s.lookups++
	if s.missing {
		return client.ModerationRemovalReceipt{}, &client.APIError{Code: "E_NOT_FOUND", Outcome: client.OutcomeDefinitive}
	}
	return client.ModerationRemovalReceipt{ActionID: id.New(), Revision: 1}, nil
}
func (s *lookupModerationSession) LookupBotModerationAction(context.Context, id.ID, id.ID, id.ID, uint64, string, []string, uint64, uint64) (client.ModerationActionReceipt, error) {
	s.lookups++
	if s.missing {
		return client.ModerationActionReceipt{}, &client.APIError{Code: "E_NOT_FOUND", Outcome: client.OutcomeDefinitive}
	}
	return client.ModerationActionReceipt{ActionID: id.New(), Revision: 1}, nil
}
func TestAutoModerationPendingReconciliationNeverSubmitsNewEffect(t *testing.T) {
	for _, action := range []moderation.Action{moderation.ActionRemove, moderation.ActionWarn, moderation.ActionTimeout} {
		for _, missing := range []bool{false, true} {
			t.Run(string(action)+fmt.Sprint(missing), func(t *testing.T) {
				n := &removalSignerFake{binding: native.Binding{Instance: testID(8), Community: testID(1), Installation: testID(9)}}
				session := &lookupModerationSession{missing: missing}
				b, err := newModerationRemovalBackend(n, session)
				if err != nil {
					t.Fatal(err)
				}
				a := removalAction(action)
				a.receiptOnly = true
				if action == moderation.ActionTimeout {
					a.Decision.Timeout = time.Minute
				}
				_, err = b.AcceptModerationAction(t.Context(), a)
				if missing && !errors.Is(err, ErrModerationActionRefused) || !missing && err != nil {
					t.Fatal(err)
				}
				if session.lookups != 1 || len(session.bodies) != 0 || len(session.botExpires) != 0 {
					t.Fatalf("lookup submitted new effect: lookups=%d removes=%d actions=%d", session.lookups, len(session.bodies), len(session.botExpires))
				}
			})
		}
	}
}

func TestModerationCaseReadsLegacyV2WithoutRefusalFlag(t *testing.T) {
	n := newModerationCaseNative()
	sink, err := NewModerationCaseSink(n, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	action := sampleModerationAction(n, 1)
	action.ReceivedAt = time.Now()
	row, err := sink.makeCase(action)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := encodeModerationCase(row, n.binding)
	if err != nil {
		t.Fatal(err)
	}
	var tuple []any
	if err = cborx.Unmarshal(raw, &tuple); err != nil {
		t.Fatal(err)
	}
	tuple = tuple[:18]
	tuple[0] = uint64(2)
	raw, err = cborx.Marshal(tuple)
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := decodeModerationCase(raw, n.binding)
	if err != nil || legacy.EnforcementRefused {
		t.Fatalf("legacy case refused: %+v %v", legacy, err)
	}
}

func TestAutoModerationActivationCheckpointFailureBlocksOldPolicy(t *testing.T) {
	state := newAutomodState()
	now := time.Unix(1800000000, 0)
	effects := 0
	cfg := moderation.Config{Enabled: true, Actions: map[moderation.Rule]moderation.Action{moderation.RuleMentions: moderation.ActionWarn}}
	sink := automodSinkFunc(func(context.Context, ModerationAction) error { effects++; return nil })
	r := durableRuntime(t, state, cfg, sink, &now)
	state.failBefore = true
	_, err := r.ActivateConfig(t.Context(), AutoModerationActivation{Config: moderation.Config{}, Revision: 1, Sink: sink}, func() error { return nil })
	if err == nil || r.ConfigurationReady() {
		t.Fatal("uncheckpointed activation acknowledged")
	}
	e := automodEvent(state, now)
	_, err = r.Handle(t.Context(), e, false, []string{"1", "2", "3", "4", "5", "6"})
	if err == nil || effects != 0 {
		t.Fatalf("old policy continued after activation failure: effects=%d err=%v", effects, err)
	}
}
