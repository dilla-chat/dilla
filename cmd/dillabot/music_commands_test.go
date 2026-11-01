//go:build linux

package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"testing"

	"github.com/jonasthim/dilla/internal/bot/native"
	"github.com/jonasthim/dilla/internal/bot/protocol"
	"github.com/jonasthim/dilla/internal/cborx"
	"github.com/jonasthim/dilla/internal/id"
)

type commandState struct{ values map[string]native.StateValue }

func (s *commandState) StateGet(_ context.Context, area uint64, key []byte) (native.StateValue, error) {
	if area != 8 || len(key) > 128 {
		return native.StateValue{}, errors.New("invalid key")
	}
	return s.values[string(key)], nil
}
func (s *commandState) StateTransaction(_ context.Context, _ id.ID, changes []native.StateChange) ([]uint64, error) {
	out := make([]uint64, len(changes))
	for i, c := range changes {
		v := s.values[string(c.Key)]
		if v.Revision != c.ExpectedRevision {
			return nil, errors.New("conflict")
		}
		v.Revision++
		v.Value = append([]byte(nil), c.Value...)
		s.values[string(c.Key)] = v
		out[i] = v.Revision
	}
	return out, nil
}

type replyCalls struct{ calls int }

func (s *replyCalls) ChannelPermissions(context.Context, id.ID) (uint64, error) {
	s.calls++
	return 3, nil
}

func recoveryFixture(t *testing.T, phase uint64) (*musicReplyRecovery, native.GroupEvent, *commandState, *[]string) {
	t.Helper()
	community := id.New()
	body := "/play historic song"
	event := native.GroupEvent{DeliveryID: 1, Group: id.New(), Community: community, Channel: id.New(), Message: id.New(), SenderUser: id.New(), SenderDevice: id.New(), ReceivedAt: 1, Body: &body}
	state := &commandState{values: map[string]native.StateValue{}}
	fingerprintBytes, err := cborx.Marshal([]any{event.Group, event.Message, event.SenderUser, event.SenderDevice, body})
	if err != nil {
		t.Fatal(err)
	}
	record := musicCommandRecord{Version: 1, Phase: phase, Fingerprint: sha256Sum(fingerprintBytes), Text: "Saved informational reply", OriginalNow: 1, OperationID: id.New(), AttemptID: id.New()}
	if phase == 1 {
		record.Text = ""
	}
	data, err := cborx.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	key := []byte("dilla.bot.music-command.v1/" + event.Group.String() + "/" + event.Message.String())
	state.values[string(key)] = native.StateValue{Revision: 1, Value: data}
	var replies []string
	h := &musicReplyRecovery{community: community, state: state, calls: &replyCalls{}, generation: func(context.Context) (uint64, error) { return 1, nil }, send: func(_ context.Context, _ native.GroupEvent, _ musicCommandRecord, text string) (protocol.SendOutcome, error) {
		replies = append(replies, text)
		return protocol.ReceiptAccepted, nil
	}}
	return h, event, state, &replies
}

func sha256Sum(data []byte) [32]byte { return sha256.Sum256(data) }

func readCommandRecord(t *testing.T, state *commandState, event native.GroupEvent) musicCommandRecord {
	t.Helper()
	key := []byte("dilla.bot.music-command.v1/" + event.Group.String() + "/" + event.Message.String())
	var r musicCommandRecord
	if err := cborx.Unmarshal(state.values[string(key)].Value, &r); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestMusicTextWithoutHistoricalReceiptHasNoEffectsOrRecord(t *testing.T) {
	state := &commandState{values: map[string]native.StateValue{}}
	body := "/play a song"
	event := native.GroupEvent{Group: id.New(), Community: id.New(), Channel: id.New(), Message: id.New(), SenderUser: id.New(), SenderDevice: id.New(), Body: &body}
	sends, generations := 0, 0
	h := &musicReplyRecovery{community: event.Community, state: state, calls: &replyCalls{}, generation: func(context.Context) (uint64, error) { generations++; return 1, nil }, send: func(context.Context, native.GroupEvent, musicCommandRecord, string) (protocol.SendOutcome, error) {
		sends++
		return protocol.ReceiptAccepted, nil
	}}
	for _, e := range []native.GroupEvent{event, func() native.GroupEvent { x := event; x.EnvelopeType = 1; return x }(), func() native.GroupEvent { x := event; x.SenderKind = 1; return x }()} {
		if err := h.HandleGroupEvent(t.Context(), e); err != nil {
			t.Fatal(err)
		}
	}
	if len(state.values) != 0 || sends != 0 || generations != 0 {
		t.Fatalf("state=%d sends=%d generations=%d", len(state.values), sends, generations)
	}
}

func TestHistoricalPendingInformationalReplyCompletesExactlyOnce(t *testing.T) {
	h, event, state, replies := recoveryFixture(t, 2)
	if err := h.HandleGroupEvent(t.Context(), event); err != nil {
		t.Fatal(err)
	}
	if len(*replies) != 1 || (*replies)[0] != "Saved informational reply" || readCommandRecord(t, state, event).Phase != 3 {
		t.Fatalf("replies=%v record=%+v", *replies, readCommandRecord(t, state, event))
	}
	if err := h.HandleGroupEvent(t.Context(), event); err != nil {
		t.Fatal(err)
	}
	if len(*replies) != 1 {
		t.Fatalf("historical reply repeated: %v", *replies)
	}
}

func TestHistoricalPendingReplyIsBoundToItsSourceMessage(t *testing.T) {
	h, event, _, replies := recoveryFixture(t, 2)
	event.Message = id.New()
	if err := h.HandleGroupEvent(t.Context(), event); err != nil {
		t.Fatal(err)
	}
	if len(*replies) != 0 {
		t.Fatalf("reply for another source message was sent: %v", *replies)
	}
}

func TestHistoricalUncertainEffectOnlySendsUncertaintyReply(t *testing.T) {
	h, event, state, replies := recoveryFixture(t, 1)
	if err := h.HandleGroupEvent(t.Context(), event); err != nil {
		t.Fatal(err)
	}
	if len(*replies) != 1 || (*replies)[0] == "Saved informational reply" || readCommandRecord(t, state, event).Phase != 3 {
		t.Fatalf("replies=%v record=%+v", *replies, readCommandRecord(t, state, event))
	}
}

func TestHistoricalClosedReplyAttemptUsesReceiptRetryProtocol(t *testing.T) {
	h, event, state, _ := recoveryFixture(t, 2)
	h.send = func(context.Context, native.GroupEvent, musicCommandRecord, string) (protocol.SendOutcome, error) {
		return protocol.ReceiptClosedNotAccepted, nil
	}
	if err := h.HandleGroupEvent(t.Context(), event); err == nil {
		t.Fatal("closed reply attempt should wait for source reconciliation")
	}
	r := readCommandRecord(t, state, event)
	if r.Phase != 2 || r.ClosedAttempts != 1 || r.PredecessorAttemptID == nil || r.AttemptID.IsZero() {
		t.Fatalf("closed receipt was not retained: %+v", r)
	}
}

func TestHistoricalUnknownReplyIsNotRetried(t *testing.T) {
	h, event, state, replies := recoveryFixture(t, 2)
	h.send = func(context.Context, native.GroupEvent, musicCommandRecord, string) (protocol.SendOutcome, error) {
		return protocol.ReceiptUnknownAfterRestore, nil
	}
	if err := h.HandleGroupEvent(t.Context(), event); err != nil {
		t.Fatal(err)
	}
	if readCommandRecord(t, state, event).Phase != 4 {
		t.Fatal("unknown reply was not retained as terminal uncertainty")
	}
	if err := h.HandleGroupEvent(t.Context(), event); err != nil || len(*replies) != 0 {
		t.Fatalf("unknown reply was retried: err=%v replies=%v", err, *replies)
	}
}
