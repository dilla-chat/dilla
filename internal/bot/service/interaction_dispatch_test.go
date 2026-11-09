package service

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/dilla-chat/dilla/internal/bot/catalog"
	"github.com/dilla-chat/dilla/internal/bot/intake"
	"github.com/dilla-chat/dilla/internal/bot/protocol"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
)

func interactionID(n byte) id.ID { var v id.ID; v[15] = n; return v }

// ---- Bot v2 Task 15: sealed commands through the same feature handlers ----

type sealedCall struct {
	actor InteractionActor
	req   protocol.Request
}

type sealedHandlers struct {
	mu      sync.Mutex
	calls   []sealedCall
	results map[protocol.Operation][]InteractionResult
	err     error
}

func (s *sealedHandlers) spec(op protocol.Operation) InteractionHandlerSpec {
	return InteractionHandlerSpec{Handle: func(_ context.Context, actor InteractionActor, req protocol.Request) (InteractionResult, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.calls = append(s.calls, sealedCall{actor, req})
		if s.err != nil {
			return InteractionResult{}, s.err
		}
		if rs := s.results[op]; len(rs) > 0 {
			r := rs[0]
			s.results[op] = rs[1:]
			return r, nil
		}
		return InteractionResult{Data: []byte{0x80}}, nil
	}}
}

func sealedFixture(ops ...protocol.Operation) (*SealedDispatcher, *sealedHandlers) {
	h := &sealedHandlers{results: map[protocol.Operation][]InteractionResult{}}
	handlers := map[protocol.Operation]InteractionHandlerSpec{}
	for _, op := range ops {
		handlers[op] = h.spec(op)
	}
	return &SealedDispatcher{Community: interactionID(1), Handlers: handlers}, h
}

func sealedRequest(op protocol.Operation, args ...any) protocol.SealedRequest {
	return protocol.SealedRequest{RequestID: interactionID(9), CommunityID: interactionID(1), Operation: uint64(op), Args: args,
		SentAt: 1_800_000_000, UserID: interactionID(5), DeviceID: interactionID(6)}
}

func collect(d *SealedDispatcher, req protocol.SealedRequest) []intake.Answer {
	var out []intake.Answer
	d.Dispatch(context.Background(), req, func(a intake.Answer) { out = append(out, a) })
	return out
}

func TestSealedRemindCreatesThroughTheReminderHandler(t *testing.T) {
	d, h := sealedFixture(protocol.OpReminderCreate)
	got := collect(d, sealedRequest(protocol.OpReminderCreate, uint64(600), "stretch"))
	if len(got) != 1 || got[0].Status != 200 || !got[0].Final || string(got[0].Body) != "Saved. I’ll remind you in 10 minutes: stretch" {
		t.Fatalf("answers = %+v", got)
	}
	c := h.calls[0]
	if c.actor.User != interactionID(5) || c.actor.Device != interactionID(6) || c.actor.Kind != 0 {
		t.Fatalf("actor = %+v", c.actor)
	}
	r := c.req
	if r.RequestID != interactionID(9) || r.CommunityID != interactionID(1) || r.ChannelID != nil || r.Revision == nil || *r.Revision != 0 || r.Expires == nil || *r.Expires != 1_800_000_120 {
		t.Fatalf("v1 request = %+v", r)
	}
	var payload []any
	if err := cborx.Unmarshal(r.Args[0].([]byte), &payload); err != nil || len(payload) != 7 || payload[1] != uint64(0) || payload[2] != uint64(600) || payload[3] != "stretch" || payload[4] != "UTC" || payload[5] != nil {
		t.Fatalf("reminder payload = %#v %v", payload, err)
	}
	if bad := collect(d, sealedRequest(protocol.OpReminderCreate, uint64(600), nil)); len(bad) != 1 || bad[0].Status != 400 || len(h.calls) != 1 {
		t.Fatalf("a reminder without text = %+v", bad)
	}
}

func TestSealedReadAnswersRevisionAndData(t *testing.T) {
	d, h := sealedFixture(protocol.OpPollGet)
	rev := uint64(7)
	h.results[protocol.OpPollGet] = []InteractionResult{{Revision: &rev, Data: []byte{0x81, 0x01}}}
	poll := interactionID(44)
	got := collect(d, sealedRequest(protocol.OpReadPoll, poll[:]))
	if len(got) != 1 || got[0].Status != 200 || !got[0].Final {
		t.Fatalf("answers = %+v", got)
	}
	var body []any
	if err := cborx.Unmarshal(got[0].Body, &body); err != nil || len(body) != 2 || body[0] != uint64(7) || string(body[1].([]byte)) != "\x81\x01" {
		t.Fatalf("read body = %#v %v", body, err)
	}
	if r := h.calls[0].req; r.Operation != protocol.OpPollGet || r.Args[0] != poll || r.Revision != nil {
		t.Fatalf("v1 request = %+v", r)
	}
	// A refusal of a read is a bare code.
	h.results[protocol.OpPollGet] = []InteractionResult{{Status: 2}}
	if got := collect(d, sealedRequest(protocol.OpReadPoll, poll[:])); len(got) != 1 || got[0].Status != 403 || string(got[0].Body) != "E_FORBIDDEN" {
		t.Fatalf("refused read = %+v", got)
	}
}

func TestSealedUnknownOperationIs404(t *testing.T) {
	d, h := sealedFixture(protocol.OpReminderCreate)
	for _, op := range []protocol.Operation{999, protocol.OpPollGet} {
		got := collect(d, sealedRequest(op))
		if len(got) != 1 || got[0].Status != 404 || string(got[0].Body) != "E_BOT_UNSUPPORTED" || !got[0].Final {
			t.Fatalf("op %d = %+v", op, got)
		}
	}
	if len(h.calls) != 0 {
		t.Fatal("a handler ran")
	}
}

func TestSealedMusicMutationRetriesAConflictOnceWithTheCurrentRevision(t *testing.T) {
	d, h := sealedFixture(protocol.OpMusicVolume)
	current := uint64(12)
	h.results[protocol.OpMusicVolume] = []InteractionResult{{Status: 4, Revision: &current}, {Revision: &current, Data: []byte{0x80}}}
	req := sealedRequest(protocol.OpMusicVolume, uint64(40))
	req.ChannelID = interactionID(30)
	got := collect(d, req)
	if len(got) != 1 || got[0].Status != 200 || string(got[0].Body) != "Volume set to 40%." {
		t.Fatalf("answers = %+v", got)
	}
	if len(h.calls) != 2 || *h.calls[1].req.Revision != 12 || *h.calls[1].req.ChannelID != interactionID(30) {
		t.Fatalf("calls = %+v", h.calls)
	}
	// Twice in conflict: answered, not looped.
	h.results[protocol.OpMusicVolume] = []InteractionResult{{Status: 4, Revision: &current}, {Status: 4, Revision: &current}}
	if got := collect(d, req); len(got) != 1 || got[0].Status != 409 || len(h.calls) != 4 {
		t.Fatalf("second conflict = %+v, calls %d", got, len(h.calls))
	}
	// Music needs the voice room as the channel.
	if got := collect(d, sealedRequest(protocol.OpMusicVolume, uint64(40))); len(got) != 1 || got[0].Status != 400 {
		t.Fatalf("no channel = %+v", got)
	}
}

func TestSealedPlayStreamsSearchingThenAnswer(t *testing.T) {
	d, _ := sealedFixture(protocol.OpMusicEnqueue)
	req := sealedRequest(protocol.OpMusicEnqueue, "never gonna")
	req.ChannelID = interactionID(30)
	got := collect(d, req)
	if len(got) != 2 || got[0].Final || string(got[0].Body) != "Searching…" || !got[1].Final || got[1].Status != 200 {
		t.Fatalf("answers = %+v", got)
	}
}

func TestSealedHandlerRefusalMapsStatus(t *testing.T) {
	d, h := sealedFixture(protocol.OpReminderCreate)
	for status, want := range map[uint64]uint16{1: 400, 2: 403, 3: 408, 4: 409, 5: 503, 6: 429, 7: 404, 8: 426, 9: 500} {
		h.results[protocol.OpReminderCreate] = []InteractionResult{{Status: status}}
		got := collect(d, sealedRequest(protocol.OpReminderCreate, uint64(600), "x"))
		if len(got) != 1 || got[0].Status != want || !got[0].Final {
			t.Fatalf("status %d = %+v, want %d", status, got, want)
		}
	}
	h.err = errors.New("boom")
	if got := collect(d, sealedRequest(protocol.OpReminderCreate, uint64(600), "x")); len(got) != 1 || got[0].Status != 500 {
		t.Fatalf("handler error = %+v", got)
	}
}

func TestSealedPollCreatePostsWithTheCommandChannel(t *testing.T) {
	d, h := sealedFixture(protocol.OpPollCreate)
	req := sealedRequest(protocol.OpPollCreate, "Lunch?", "Soup | Salad", uint64(1), nil, nil)
	req.ChannelID = interactionID(31)
	got := collect(d, req)
	if len(got) != 1 || got[0].Status != 200 || len(got[0].Body) != 0 {
		t.Fatalf("answers = %+v", got)
	}
	r := h.calls[0].req
	var payload []any
	if err := cborx.Unmarshal(r.Args[0].([]byte), &payload); err != nil || r.ChannelID != nil || *r.Revision != 0 {
		t.Fatalf("request = %+v %v", r, err)
	}
	ch := interactionID(31)
	if b, _ := payload[1].([]byte); string(b) != string(ch[:]) || payload[3] != "Lunch?" || len(payload[4].([]any)) != 2 || payload[5] != uint64(1) || payload[6] != uint64(86400) {
		t.Fatalf("poll payload = %#v", payload)
	}
}

func TestSealedMusicStateRendersNowPlaying(t *testing.T) {
	d, h := sealedFixture(protocol.OpMusicState)
	track := []any{"yt:x", "youtube", "x", "Song", "Band", "", uint64(1000), "u"}
	next := []any{"yt:y", "youtube", "y", "Next", "Other", "", uint64(1000), "u"}
	data, _ := cborx.Marshal([]any{uint64(1), uint64(3), track, []any{next}, uint64(0), uint64(0), uint64(50), uint64(0), uint64(0), uint64(1), uint64(0), uint64(0)})
	h.results[protocol.OpMusicState] = []InteractionResult{{Data: data}, {Data: data}}
	req := sealedRequest(protocol.OpMusicState)
	req.ChannelID = interactionID(30)
	got := collect(d, req)
	if len(got) != 1 || string(got[0].Body) != "Now playing: Song — Band\n1. Next — Other" {
		t.Fatalf("queue = %+v", got)
	}
	// The established copy (bot-handlers.ts): an empty player says so.
	empty, _ := cborx.Marshal([]any{uint64(1), uint64(3), nil, []any{}, uint64(0), uint64(0), uint64(50), uint64(0), uint64(0), uint64(1), uint64(0), uint64(0)})
	h.results[protocol.OpMusicState] = []InteractionResult{{Data: empty}}
	if got := collect(d, req); len(got) != 1 || string(got[0].Body) != "The music queue is empty." {
		t.Fatalf("empty queue = %+v", got)
	}
}

func TestEveryCatalogOperationHasASealedAdapter(t *testing.T) {
	for _, c := range catalog.Commands(catalog.AllFeatures()) {
		if !SealedOperationSupported(c.Operation) {
			t.Fatalf("catalog command %q (operation %d) has no sealed adapter", c.Name, c.Operation)
		}
		if read := c.HandlerKind == catalog.HandlerRead; read != sealedAdapters[c.Operation].read {
			t.Fatalf("catalog command %q: handler kind %d but the adapter's read is %v", c.Name, c.HandlerKind, !read)
		}
	}
}

// Task 21b: an app operation runs its v1 operation with the v1 arguments (ids as 16 bytes, normalised as the v1 decoder
// does), a mutation's expected revision first; it answers [revision, data] like a sealed read, a refusal as a bare code.
func TestSealedAppOperationsRunTheV1Handler(t *testing.T) {
	d, h := sealedFixture(protocol.OpMusicRemove, protocol.OpPollVote, protocol.OpPollVoters, protocol.OpMusicSearch, protocol.OpMusicEnqueue)
	voice, item, poll := interactionID(30), interactionID(31), interactionID(32)
	rev := uint64(8)
	h.results[protocol.OpMusicRemove] = []InteractionResult{{Revision: &rev, Data: []byte{0x80}}}
	req := sealedRequest(protocol.OpAppMusicRemove, uint64(7), item[:])
	req.ChannelID = voice
	got := collect(d, req)
	var body []any
	if len(got) != 1 || got[0].Status != 200 || !got[0].Final || cborx.Unmarshal(got[0].Body, &body) != nil || len(body) != 2 || body[0] != uint64(8) {
		t.Fatalf("remove answers = %+v", got)
	}
	r := h.calls[0].req
	if r.Operation != protocol.OpMusicRemove || r.ChannelID == nil || *r.ChannelID != voice || r.Revision == nil || *r.Revision != 7 ||
		r.Expires == nil || *r.Expires != 1_800_000_120 || len(r.Args) != 1 || r.Args[0] != item {
		t.Fatalf("v1 remove = %+v", r)
	}
	// A poll vote: no channel, the revision first; the selection passes as given.
	selection, _ := cborx.Marshal([]any{uint64(1)})
	if got := collect(d, sealedRequest(protocol.OpAppPollVote, uint64(3), poll[:], selection)); len(got) != 1 || got[0].Status != 200 {
		t.Fatalf("vote = %+v", got)
	}
	if r := h.calls[1].req; r.Operation != protocol.OpPollVote || r.ChannelID != nil || *r.Revision != 3 || r.Args[0] != poll {
		t.Fatalf("v1 vote = %+v", r)
	}
	// A read has no revision.
	if got := collect(d, sealedRequest(protocol.OpAppPollVoters, poll[:], uint64(0), nil)); len(got) != 1 || got[0].Status != 200 {
		t.Fatalf("voters = %+v", got)
	}
	if r := h.calls[2].req; r.Operation != protocol.OpPollVoters || r.Revision != nil || r.Expires != nil {
		t.Fatalf("v1 voters = %+v", r)
	}
	// The panel's enqueue only queues, as the v1 operation did (no sealed /play), and answers data.
	enqueue := sealedRequest(protocol.OpAppMusicEnqueue, uint64(0), "https://youtu.be/x")
	enqueue.ChannelID = voice
	if got := collect(d, enqueue); len(got) != 1 || got[0].Status != 200 {
		t.Fatalf("enqueue = %+v", got)
	}
	// Refusals: a v1 status is a bare code; malformed arguments and a player operation outside a channel are E_INVALID.
	h.results[protocol.OpPollVote] = []InteractionResult{{Status: 4}}
	if got := collect(d, sealedRequest(protocol.OpAppPollVote, uint64(3), poll[:], selection)); len(got) != 1 || got[0].Status != 409 || string(got[0].Body) != "E_CONFLICT" {
		t.Fatalf("conflict = %+v", got)
	}
	calls := len(h.calls)
	for name, bad := range map[string]protocol.SealedRequest{
		"no revision":         sealedRequest(protocol.OpAppPollVote, poll[:], selection),
		"short id":            sealedRequest(protocol.OpAppPollVote, uint64(3), poll[:15], selection),
		"music with no voice": sealedRequest(protocol.OpAppMusicRemove, uint64(7), item[:]),
		"extra argument":      sealedRequest(protocol.OpAppPollVoters, poll[:], uint64(0), nil, nil),
	} {
		if got := collect(d, bad); len(got) != 1 || got[0].Status != 400 || string(got[0].Body) != "E_INVALID" {
			t.Fatalf("%s = %+v", name, got)
		}
	}
	if len(h.calls) != calls {
		t.Fatal("a malformed app operation reached its handler")
	}
}

// The poll form's create posts only into the channel the command names, the one the intake authorised send_messages in.
func TestSealedAppPollCreateMustTargetTheCommandChannel(t *testing.T) {
	d, h := sealedFixture(protocol.OpPollCreate)
	here, there := interactionID(40), interactionID(41)
	payload := func(channel id.ID) []byte {
		b, err := cborx.Marshal([]any{uint64(1), channel[:], nil, "Lunch?", []any{"Soup", "Salad"}, uint64(0), uint64(3600)})
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	req := sealedRequest(protocol.OpAppPollCreate, uint64(0), payload(there))
	req.ChannelID = here
	if got := collect(d, req); len(got) != 1 || got[0].Status != 403 || string(got[0].Body) != "E_FORBIDDEN" || len(h.calls) != 0 {
		t.Fatalf("a poll for another channel = %+v (%d calls)", got, len(h.calls))
	}
	req = sealedRequest(protocol.OpAppPollCreate, uint64(0), payload(here))
	req.ChannelID = here
	if got := collect(d, req); len(got) != 1 || got[0].Status != 200 || len(h.calls) != 1 || h.calls[0].req.ChannelID != nil {
		t.Fatalf("a poll for the command's channel = %+v", got)
	}
}

type channelPermsFake map[[2]id.ID]uint64

func (f channelPermsFake) IsMember(id.ID) bool { return true }
func (f channelPermsFake) ChannelPermissions(user, channel id.ID) uint64 {
	return f[[2]id.ID{user, channel}]
}

// Task 21b fix round 1, review M4: a role panel (131) posts into the channel its first argument names. The dispatcher
// checks view and send there from the feed (a Bot channel), before the handler applies its manage_roles check.
func TestSealedRolePanelNeedsViewAndSendInItsTargetChannel(t *testing.T) {
	d, h := sealedFixture(protocol.OpRolePanelPublish)
	target, user, role := interactionID(50), interactionID(5), interactionID(51)
	payload, _ := cborx.Marshal([]any{[]any{"📚", role[:], "Reader"}})
	request := func() protocol.SealedRequest {
		return sealedRequest(protocol.OpAppRolePanelPublish, uint64(0), target[:], payload)
	}
	d.Feed = channelPermsFake{{user, target}: 1}
	if got := collect(d, request()); len(got) != 1 || got[0].Status != 403 || len(h.calls) != 0 {
		t.Fatalf("a panel where the member cannot send = %+v (%d calls)", got, len(h.calls))
	}
	d.Feed = channelPermsFake{{user, target}: 1 | 2}
	if got := collect(d, request()); len(got) != 1 || got[0].Status != 200 || len(h.calls) != 1 {
		t.Fatalf("a panel where the member may view and send = %+v", got)
	}
}

// Bot v2 Task 23: the moderation log's two pages are sealed app operations (160, 164), no longer kind-3 requests.
func TestSealedModerationLogPagesRunTheirReaders(t *testing.T) {
	d, h := sealedFixture(protocol.OpModerationCases, protocol.OpModerationEvidenceList)
	page, _ := cborx.Marshal([]any{uint64(1), []any{}, nil})
	h.results[protocol.OpModerationCases] = []InteractionResult{{Data: page}}
	h.results[protocol.OpModerationEvidenceList] = []InteractionResult{{Data: page}}
	if got := collect(d, sealedRequest(protocol.OpAppModerationCases, nil)); len(got) != 1 || got[0].Status != 200 || !got[0].Final {
		t.Fatalf("cases = %+v", got)
	}
	if r := h.calls[0].req; r.Operation != protocol.OpModerationCases || r.Revision != nil || len(r.Args) != 1 || r.Args[0] != nil {
		t.Fatalf("v1 cases = %+v", r)
	}
	cursor := []byte{0x84, 1, 2, 3}
	if got := collect(d, sealedRequest(protocol.OpAppModerationEvidenceList, cursor)); len(got) != 1 || got[0].Status != 200 {
		t.Fatalf("evidence = %+v", got)
	}
	if r := h.calls[1].req; r.Operation != protocol.OpModerationEvidenceList || len(r.Args) != 1 || !bytes.Equal(r.Args[0].([]byte), cursor) {
		t.Fatalf("v1 evidence = %+v", r)
	}
}
