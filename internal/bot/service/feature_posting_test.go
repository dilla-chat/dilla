package service

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/bot/client"
	"github.com/dilla-chat/dilla/internal/bot/intake"
	"github.com/dilla-chat/dilla/internal/bot/polls"
	"github.com/dilla-chat/dilla/internal/bot/protocol"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
)

// hintRecorder is a HintSender that remembers what it was asked to send.
type hintRecorder struct {
	mu   sync.Mutex
	sent []sentHint
	err  error
}

type sentHint struct {
	kind  uint8
	scope id.ID
}

func (r *hintRecorder) Hint(_ context.Context, kind uint8, scope id.ID) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sent = append(r.sent, sentHint{kind, scope})
	return r.err
}

func (r *hintRecorder) hints() []sentHint {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]sentHint(nil), r.sent...)
}

func pollSealed(h PollInteractions, req protocol.SealedRequest) []intake.Answer {
	d := &SealedDispatcher{Community: h.CommunityID, Handlers: h.Handlers()}
	var out []intake.Answer
	d.Dispatch(context.Background(), req, func(a intake.Answer) { out = append(out, a) })
	return out
}

func pollFixture(t *testing.T, m *marks) (PollInteractions, *pollPublisherFake) {
	t.Helper()
	community := pollServiceID(1)
	gateFor(t, community, m)
	t.Cleanup(func() { ClearNeedsBotChannel(community, "polls") })
	pub := &pollPublisherFake{}
	now := time.Unix(1000, 0)
	return PollInteractions{
		Store:       polls.NewStore(&pollTestState{}, community),
		Authority:   &pollAuthorityFake{bits: pollPermView | pollPermSend | pollPermReadHistory | pollPermManageMessages, member: true},
		CommunityID: community, Now: func() time.Time { return now }, Publisher: pub,
		ChannelName: func(_ context.Context, ch id.ID) string {
			if ch == pollServiceID(40) {
				return "bot"
			}
			return "other"
		},
	}, pub
}

func pollCreateRequest(channel id.ID, args ...any) protocol.SealedRequest {
	req := protocol.SealedRequest{RequestID: pollServiceID(30), CommunityID: pollServiceID(1), ChannelID: channel,
		Operation: uint64(protocol.OpPollCreate), Args: args, SentAt: 1_800_000_000, UserID: pollServiceID(2), DeviceID: pollServiceID(3)}
	return req
}

func TestPollCreateDefaultsToConfiguredBotChannelAndSaysWhere(t *testing.T) {
	botChannel := pollServiceID(40)
	h, pub := pollFixture(t, &marks{set: map[id.ID]bool{botChannel: true}, known: true})
	// The command ran in a channel that is not a Bot channel and named none: the poll goes to the Bot channel.
	got := pollSealed(h, pollCreateRequest(pollServiceID(41), "Lunch?", "Soup | Salad", uint64(0), nil, nil))
	if len(got) != 1 || got[0].Status != 200 || string(got[0].Body) != "Posted the poll in #bot." {
		t.Fatalf("reply = %+v, want 200 naming #bot", got)
	}
	if pub.calls != 1 {
		t.Fatalf("published %d times, want 1", pub.calls)
	}
	// Run in the Bot channel itself, it goes there too.
	other := pollCreateRequest(botChannel, "Dinner?", "Pie | Cake", uint64(0), nil, nil)
	other.RequestID = pollServiceID(31)
	if got := pollSealed(h, other); len(got) != 1 || got[0].Status != 200 || !strings.Contains(string(got[0].Body), "#bot") {
		t.Fatalf("in the Bot channel: %+v", got)
	}
}

func TestPostingCommandWithNoBotChannelIsRefusedWithReason(t *testing.T) {
	h, pub := pollFixture(t, &marks{set: map[id.ID]bool{}, known: true})
	got := pollSealed(h, pollCreateRequest(pollServiceID(41), "Lunch?", "Soup | Salad", uint64(0), nil, nil))
	if len(got) != 1 || got[0].Status != 400 || !strings.Contains(string(got[0].Body), "Bot channel") {
		t.Fatalf("reply = %+v, want a 400 saying a Bot channel is needed", got)
	}
	if pub.calls != 0 {
		t.Fatalf("published %d times with no Bot channel, want 0", pub.calls)
	}
	if flags := NeedsBotChannelFlags(pollServiceID(1)); len(flags) != 1 || flags[0].Feature != "polls" {
		t.Fatalf("flags %+v, want polls flagged needs_bot_channel", flags)
	}
	// Naming an unmarked channel is refused too, and nothing is posted.
	unmarked := pollServiceID(42)
	req := pollCreateRequest(pollServiceID(41), "Lunch?", "Soup | Salad", uint64(0), nil, unmarked[:])
	req.RequestID = pollServiceID(32)
	if got := pollSealed(h, req); len(got) != 1 || got[0].Status != 400 || pub.calls != 0 {
		t.Fatalf("named unmarked channel: %+v published=%d", got, pub.calls)
	}
}

func TestPollVoteSendsChannelHint(t *testing.T) {
	botChannel := pollServiceID(40)
	h, _ := pollFixture(t, &marks{set: map[id.ID]bool{botChannel: true}, known: true})
	rec := &hintRecorder{}
	h.Hints = rec
	pid := pollServiceID(3)
	now := time.Unix(1000, 0)
	if _, err := h.Store.Create(context.Background(), pid, [32]byte{1}, polls.Poll{ID: pid, CommunityID: h.CommunityID, ChannelID: botChannel, CreatorID: pollServiceID(2), Question: "Q", Answers: []string{"A", "B"}, CreatedAt: 1000, DueAt: 4600, Published: true}, now); err != nil {
		t.Fatal(err)
	}
	selection, _ := cborx.Marshal([]any{uint64(0)})
	rev, expires := uint64(1), uint64(1100)
	vote := protocol.Request{RequestID: pollServiceID(8), CommunityID: h.CommunityID, Operation: protocol.OpPollVote, Revision: &rev, Expires: &expires, Args: []any{pid, selection}}
	if res, err := h.vote(context.Background(), InteractionActor{User: pollServiceID(6)}, vote); err != nil || res.Status != 0 {
		t.Fatalf("vote: %+v %v", res, err)
	}
	if got := rec.hints(); len(got) != 1 || got[0].kind != client.StateScopeChannel || got[0].scope != botChannel {
		t.Fatalf("hints after a vote = %+v, want one channel hint for the poll's channel", got)
	}
	// A refused vote (a conflict on a stale revision) hints nothing.
	if res, _ := h.vote(context.Background(), InteractionActor{User: pollServiceID(7)}, protocol.Request{RequestID: pollServiceID(9), CommunityID: h.CommunityID, Operation: protocol.OpPollVote, Revision: &rev, Expires: &expires, Args: []any{pid, selection}}); res.Status == 0 && len(rec.hints()) != 2 {
		t.Fatalf("second vote accepted but hints = %+v", rec.hints())
	}
	current, err := h.Store.Get(context.Background(), pid)
	if err != nil {
		t.Fatal(err)
	}
	closeRev := current.Revision
	closeReq := protocol.Request{RequestID: pollServiceID(10), CommunityID: h.CommunityID, Operation: protocol.OpPollClose, Revision: &closeRev, Expires: &expires, Args: []any{pid}}
	before := len(rec.hints())
	if res, err := h.close(context.Background(), InteractionActor{User: pollServiceID(2)}, closeReq); err != nil || res.Status != 0 {
		t.Fatalf("close: %+v %v", res, err)
	}
	if got := rec.hints(); len(got) != before+1 || got[before].kind != client.StateScopeChannel || got[before].scope != botChannel {
		t.Fatalf("hints after close = %+v", got)
	}
}

func TestHintFailureNeverFailsTheCommand(t *testing.T) {
	rec := &hintRecorder{err: context.DeadlineExceeded}
	hint(context.Background(), rec, client.StateScopeUser, pollServiceID(5))
	if len(rec.hints()) != 1 {
		t.Fatal("the hint was not attempted")
	}
	hint(context.Background(), nil, client.StateScopeUser, pollServiceID(5)) // a nil sender is a no-op
}
