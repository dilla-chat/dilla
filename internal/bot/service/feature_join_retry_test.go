package service

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/bot/client"
	"github.com/dilla-chat/dilla/internal/bot/protocol"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
)

// joinDestination is a destination whose channel is marked at once but whose group the Bot joins only later: the first
// `refusals` resolves fail as "not joined", then it answers the group.
type joinDestination struct {
	group    id.ID
	refusals int
	mu       sync.Mutex
	calls    int
}

func (d *joinDestination) ResolveTextGroup(context.Context, id.ID, id.ID) (id.ID, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls++
	if d.calls <= d.refusals {
		return id.ID{}, ErrBotNotJoined
	}
	return d.group, nil
}

func logCapture(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(old) })
	return &buf
}

// A channel is marked, the panel is published before the Bot has joined the channel's group: the post is refused (and
// logged), the Bot joins, and the post is sent.
func TestRolePanelPostRefusedUntilTheBotJoinsTheMarkedChannelThenSent(t *testing.T) {
	community, actor, channel, role, group, requestID := id.New(), id.New(), id.New(), id.New(), id.New(), id.New()
	gateFor(t, community, &marks{set: map[id.ID]bool{channel: true}, known: true})
	logs := logCapture(t)
	payload, err := cborx.Marshal([]any{[]any{"🎮", role, "Gamers"}})
	if err != nil {
		t.Fatal(err)
	}
	destination := &joinDestination{group: group, refusals: 3}
	sender := &rolePanelSendFake{receipt: protocol.SendReceipt{OperationID: requestID, AttemptID: rolePanelAttemptID(requestID), CommunityID: community,
		GroupID: group, ExpectedGeneration: 7, Outcome: protocol.ReceiptAccepted, SendResponse: &protocol.OriginalSendResponse{Sequence: 42}}}
	handler := RolePanelInteractions{Authority: &rolePanelAuthorityFake{allowed: true}, Destination: destination, Sender: sender, Discovery: &rolePanelDiscoveryFake{generation: 7},
		Session: &client.Session{}, CommunityID: community, Now: func() time.Time { return time.Unix(100, 0) },
		JoinPoll: time.Millisecond, JoinWait: 5 * time.Second}.Handlers()[protocol.OpRolePanelPublish]
	request := protocol.Request{RequestID: requestID, CommunityID: community, Operation: protocol.OpRolePanelPublish,
		Revision: rolePanelU64Ptr(0), Expires: rolePanelU64Ptr(130), Args: []any{channel, payload}}
	result, err := handler.Handle(context.Background(), InteractionActor{User: actor, Kind: 0}, request)
	if err != nil || result.Status != 0 {
		t.Fatalf("publish = %+v %v, want it sent once the Bot joined", result, err)
	}
	if sender.calls != 1 || sender.input.GroupID != group {
		t.Fatalf("sent %d times into %s, want once into the joined group", sender.calls, sender.input.GroupID)
	}
	if destination.calls != 4 {
		t.Fatalf("destination resolved %d times, want 3 refusals and the join", destination.calls)
	}
	text := logs.String()
	if !strings.Contains(text, "bot post refused") || !strings.Contains(text, "feature=role_panels") || !strings.Contains(text, "channel_id="+channel.String()) || !strings.Contains(text, "not_joined") {
		t.Fatalf("the refusal was not logged with feature and channel:\n%s", text)
	}
	if strings.Contains(text, "Gamers") {
		t.Fatal("the log carries post content")
	}
}

// A Bot that never joins gives up when the wait ends (the member is told it is unavailable) and nothing is sent.
func TestRolePanelPostGivesUpWhenTheBotNeverJoins(t *testing.T) {
	community, actor, channel, role, group, requestID := id.New(), id.New(), id.New(), id.New(), id.New(), id.New()
	gateFor(t, community, &marks{set: map[id.ID]bool{channel: true}, known: true})
	payload, _ := cborx.Marshal([]any{[]any{"🎮", role, "Gamers"}})
	destination := &joinDestination{group: group, refusals: 1 << 30}
	sender := &rolePanelSendFake{}
	handler := RolePanelInteractions{Authority: &rolePanelAuthorityFake{allowed: true}, Destination: destination, Sender: sender, Discovery: &rolePanelDiscoveryFake{generation: 7},
		Session: &client.Session{}, CommunityID: community, JoinPoll: time.Millisecond, JoinWait: 30 * time.Millisecond}.Handlers()[protocol.OpRolePanelPublish]
	request := protocol.Request{RequestID: requestID, CommunityID: community, Operation: protocol.OpRolePanelPublish,
		Revision: rolePanelU64Ptr(0), Expires: rolePanelU64Ptr(130), Args: []any{channel, payload}}
	result, err := handler.Handle(context.Background(), InteractionActor{User: actor, Kind: 0}, request)
	if err != nil || result.Status != interactionStatusUnavailable || sender.calls != 0 {
		t.Fatalf("publish = %+v %v sends=%d, want unavailable and no send", result, err, sender.calls)
	}
}

// The send layer logs every refusal (feature unknown there: the group's channel), with no content.
func TestSendLayerLogsARefusalAtInfo(t *testing.T) {
	logs := logCapture(t)
	c, _, s, in := sendFixture()
	gateFor(t, sendBinding().Community, &marks{set: map[id.ID]bool{}, known: true})
	if _, err := c.send(context.Background(), s, in); err == nil {
		t.Fatal("not refused")
	}
	text := logs.String()
	if !strings.Contains(text, "bot post refused") || !strings.Contains(text, "channel_id="+sendID(5).String()) {
		t.Fatalf("send-layer refusal not logged:\n%s", text)
	}
}

// closingSender closes the first `closed` attempts (the Bot is still at its kept Welcome's epoch), then accepts.
type closingSender struct {
	closed    int
	inputs    []SendInput
	community id.ID
}

func (s *closingSender) Send(_ context.Context, _ *client.Session, in SendInput) (protocol.SendReceipt, error) {
	s.inputs = append(s.inputs, in)
	receipt := protocol.SendReceipt{OperationID: in.OperationID, AttemptID: in.AttemptID, PredecessorAttemptID: in.PredecessorAttemptID,
		CommunityID: s.community, GroupID: in.GroupID, ExpectedGeneration: in.ExpectedGeneration, Outcome: protocol.ReceiptClosedNotAccepted}
	if len(s.inputs) > s.closed {
		receipt.Outcome, receipt.SendResponse = protocol.ReceiptAccepted, &protocol.OriginalSendResponse{Sequence: 42}
	}
	return receipt, nil
}

// Follow-up :465: right after joining a newly marked channel from its kept Welcome the Bot is at an old epoch, and its
// first post is closed, not accepted. The panel is sent again as a successor of the closed attempt, and posts once it is
// accepted; a replay of the same request walks the same attempt chain.
func TestRolePanelSendsASuccessorWhenTheFirstAttemptIsClosed(t *testing.T) {
	community, actor, channel, role, group, requestID := id.New(), id.New(), id.New(), id.New(), id.New(), id.New()
	gateFor(t, community, &marks{set: map[id.ID]bool{channel: true}, known: true})
	payload, _ := cborx.Marshal([]any{[]any{"🎮", role, "Gamers"}})
	sender := &closingSender{closed: 2, community: community}
	handler := RolePanelInteractions{Authority: &rolePanelAuthorityFake{allowed: true}, Destination: &joinDestination{group: group}, Sender: sender,
		Discovery: &rolePanelDiscoveryFake{generation: 7}, Session: &client.Session{}, CommunityID: community,
		JoinPoll: time.Millisecond, JoinWait: 5 * time.Second}
	request := protocol.Request{RequestID: requestID, CommunityID: community, Operation: protocol.OpRolePanelPublish,
		Revision: rolePanelU64Ptr(0), Expires: rolePanelU64Ptr(130), Args: []any{channel, payload}}
	publish := handler.Handlers()[protocol.OpRolePanelPublish]
	result, err := publish.Handle(context.Background(), InteractionActor{User: actor, Kind: 0}, request)
	if err != nil || result.Status != 0 {
		t.Fatalf("publish = %+v %v, want accepted on the successor", result, err)
	}
	if len(sender.inputs) != 3 {
		t.Fatalf("%d attempts, want 2 closed and 1 accepted", len(sender.inputs))
	}
	first := sender.inputs[0]
	if first.AttemptID != rolePanelAttemptID(requestID) || first.PredecessorAttemptID != nil {
		t.Fatalf("first attempt = %+v", first)
	}
	for i := 1; i < 3; i++ {
		in := sender.inputs[i]
		if in.AttemptID != rolePanelSuccessorID(requestID, uint64(i)) || in.PredecessorAttemptID == nil || *in.PredecessorAttemptID != sender.inputs[i-1].AttemptID ||
			in.OperationID != requestID || in.GroupID != group {
			t.Fatalf("attempt %d = %+v, want the successor of attempt %d", i, in, i-1)
		}
	}
	var data []any
	if cborx.Unmarshal(result.Data, &data) != nil || len(data) != 2 || data[1] != uint64(42) {
		t.Fatalf("answer data = %x, want [channel, 42]", result.Data)
	}
}
