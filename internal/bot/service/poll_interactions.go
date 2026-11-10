package service

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/dilla-chat/dilla/internal/bot/client"
	"github.com/dilla-chat/dilla/internal/bot/helper"
	"github.com/dilla-chat/dilla/internal/bot/polls"
	"github.com/dilla-chat/dilla/internal/bot/protocol"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
)

// pollDefaultChannel stands for "the feature's configured Bot channel" in a poll create payload (the v1 payload needs a
// non-zero channel id; no real channel id is all ones).
var pollDefaultChannel = id.ID{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}

const (
	pollPermView           uint64 = 1 << 0
	pollPermSend           uint64 = 1 << 1
	pollPermManageMessages uint64 = 1 << 2 // internal/api.PermManageMessages and protocol/vectors/permissions.json
	pollPermReadHistory    uint64 = 1 << 6
	pollPageBytes                 = 2400
)

type PollAuthority interface {
	PollPermissions(context.Context, id.ID, id.ID) (uint64, error)
	PollMember(context.Context, id.ID) (bool, error)
}
type PollPublisher interface {
	PublishPoll(context.Context, polls.Poll) error
}
type PollInteractions struct {
	Store       *polls.Store
	Authority   PollAuthority
	CommunityID id.ID
	Now         func() time.Time
	Publisher   PollPublisher
	// CreateEnabled reads the installation's current Polls capability (bit 8, Task 10c) when a
	// poll is created. Off refuses only creation: existing polls stay votable and closable. Nil
	// allows creation (tests and older wiring).
	CreateEnabled func(context.Context) (bool, error)
	// DefaultChannel is the feature's configured Bot channel, where a poll goes when the command names none (Bot v2
	// Task 17). Nil uses the community's only Bot channel, when it has exactly one.
	DefaultChannel func(context.Context) (id.ID, bool)
	// ChannelName names a channel for the reply ("Posted the poll in #bot."). Nil, or an empty name, leaves it out.
	ChannelName func(context.Context, id.ID) string
	// Hints tells members' clients that a poll in a Bot channel changed (vote or close): channel scope.
	Hints HintSender
	// Reads, when set, coalesces the reads of one poll that arrive within PollReadWindow from many members into one
	// computation of the poll and its voters (Task 21b fix round 1). Each member is still authorised and answered with
	// their own choices. A vote, a close or a create through these handlers ends the window for that poll.
	Reads *PollReadCache
}

// PollReadWindow is how long one computation of a poll and its voters serves the reads that arrive after it: a hint
// reaches every member's card at once, and their reads land within it.
const PollReadWindow = 100 * time.Millisecond

// pollVoterPreview and pollVoterLimit bound the voters a poll read carries: the first five of each answer (the card's
// faces), and distinct voters counted up to 2000 (beyond that the card says "at least").
const (
	pollVoterPreview = 5
	pollVoterLimit   = 2000
)

// PollReadCache holds the shared part of recent poll reads: the poll and, once asked for, its voters.
type PollReadCache struct {
	mu      sync.Mutex
	now     func() time.Time
	entries map[id.ID]*pollReadEntry
}

type pollReadEntry struct {
	at     time.Time
	done   chan struct{}
	poll   polls.Poll
	err    error
	voters *pollVoterSummary
}

type pollVoterSummary struct {
	previews [][]id.ID
	distinct int
	capped   bool
	err      error
	done     chan struct{}
}

// NewPollReadCache is an empty cache on the system clock.
func NewPollReadCache() *PollReadCache {
	return &PollReadCache{now: time.Now, entries: map[id.ID]*pollReadEntry{}}
}

// poll is pid's record, computed at most once per window however many members read it.
func (c *PollReadCache) poll(ctx context.Context, store *polls.Store, pid id.ID) (*pollReadEntry, error) {
	c.mu.Lock()
	now := c.now()
	for k, e := range c.entries {
		if now.Sub(e.at) >= PollReadWindow {
			select {
			case <-e.done:
				delete(c.entries, k)
			default:
			}
		}
	}
	e, ok := c.entries[pid]
	if !ok || now.Sub(e.at) >= PollReadWindow {
		e = &pollReadEntry{at: now, done: make(chan struct{})}
		c.entries[pid] = e
		c.mu.Unlock()
		e.poll, e.err = store.Get(ctx, pid)
		close(e.done)
	} else {
		c.mu.Unlock()
		select {
		case <-e.done:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return e, e.err
}

// summary is the entry's voters, computed at most once.
func (c *PollReadCache) summary(ctx context.Context, store *polls.Store, e *pollReadEntry) (*pollVoterSummary, error) {
	c.mu.Lock()
	v := e.voters
	first := v == nil
	if first {
		v = &pollVoterSummary{done: make(chan struct{})}
		e.voters = v
	}
	c.mu.Unlock()
	if first {
		v.previews, v.distinct, v.capped, v.err = store.VoterSummary(ctx, e.poll.ID, len(e.poll.Answers), pollVoterPreview, pollVoterLimit)
		close(v.done)
	} else {
		select {
		case <-v.done:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return v, v.err
}

// forget ends pid's window: the next read computes the poll again.
func (c *PollReadCache) forget(pid id.ID) {
	if c == nil {
		return
	}
	c.mu.Lock()
	delete(c.entries, pid)
	c.mu.Unlock()
}

// FeedPermissions is the feed-state permission view (intake.Authz) the sealed poll handlers authorise by.
type FeedPermissions interface {
	IsMember(user id.ID) bool
	ChannelPermissions(user, channel id.ID) uint64
}

// FeedPollAuthority answers the poll handlers' authority from the Bot's feed state, with no dillad read (Task 21b fix
// round 1, as Task 15 did for reminders and Task 19 for music). A poll in a channel the feed names nothing about (not a
// Bot channel) is refused: fail closed.
type FeedPollAuthority struct{ Feed FeedPermissions }

func (a FeedPollAuthority) PollMember(_ context.Context, user id.ID) (bool, error) {
	return a.Feed.IsMember(user), nil
}

func (a FeedPollAuthority) PollPermissions(_ context.Context, user, channel id.ID) (uint64, error) {
	return a.Feed.ChannelPermissions(user, channel), nil
}

func (h PollInteractions) Handlers() map[protocol.Operation]InteractionHandlerSpec {
	return map[protocol.Operation]InteractionHandlerSpec{
		protocol.OpPollCreate: {RevisionInHandler: true, Handle: h.create},
		protocol.OpPollGet:    {RecomputeOnReplay: true, Handle: h.get},
		protocol.OpPollList:   {RecomputeOnReplay: true, Handle: h.list},
		protocol.OpPollVote:   {RevisionInHandler: true, Handle: h.vote},
		protocol.OpPollClose:  {RevisionInHandler: true, Handle: h.close},
		protocol.OpPollVoters: {RecomputeOnReplay: true, Handle: h.voters},
	}
}
func (h PollInteractions) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}
func (h PollInteractions) authorize(ctx context.Context, actor InteractionActor, channel id.ID, want uint64) (bool, error) {
	if h.Authority == nil || h.Store == nil || h.CommunityID.IsZero() || actor.User.IsZero() {
		return false, errors.New("missing poll interaction dependency")
	}
	if actor.Kind != 0 {
		return false, nil
	}
	member, e := h.Authority.PollMember(ctx, actor.User)
	if e != nil || !member {
		return false, e
	}
	bits, e := h.Authority.PollPermissions(ctx, actor.User, channel)
	if e != nil {
		return false, e
	}
	return bits&want == want, nil
}
func (h PollInteractions) validRequest(actor InteractionActor, r protocol.Request, op protocol.Operation, mutation bool) bool {
	return r.CommunityID == h.CommunityID && r.ChannelID == nil && r.Operation == op && (!mutation || (r.RequestID != id.ID{} && r.Revision != nil && r.Expires != nil))
}
func pollFingerprint(r protocol.Request) ([32]byte, bool) {
	if r.RequestID.IsZero() {
		return [32]byte{}, false
	}
	s, e := protocol.EncodeRequest(r)
	if e != nil {
		return [32]byte{}, false
	}
	return sha256.Sum256([]byte(s)), true
}
func pollResultStatus(e error) uint64 {
	var nativeStatus *helper.StatusError
	if errors.As(e, &nativeStatus) && nativeStatus.Status == 4 {
		return interactionStatusConflict
	}
	switch {
	case errors.Is(e, polls.ErrForbidden):
		return interactionStatusForbidden
	case errors.Is(e, polls.ErrClosed):
		return interactionStatusConflict
	case errors.Is(e, polls.ErrConflict), errors.Is(e, polls.ErrMutationIDConflict), errors.Is(e, polls.ErrQuota):
		return interactionStatusConflict
	case errors.Is(e, polls.ErrNotFound), errors.Is(e, polls.ErrInvalid):
		return interactionStatusUnavailable
	default:
		return interactionStatusUnavailable
	}
}
func (h PollInteractions) present(p polls.Poll) polls.Poll {
	if seconds, ok := unixSeconds(h.now()); ok && seconds >= p.DueAt {
		p.Closed = true
	}
	return p
}

func unixSeconds(now time.Time) (uint64, bool) {
	seconds := now.Unix()
	if seconds < 0 {
		return 0, false
	}
	return uint64(seconds), true
}

// unavailableInteraction intentionally encodes malformed or unavailable private
// requests as status 5 while returning no transport error to the response layer.
func unavailableInteraction() (InteractionResult, error) {
	return InteractionResult{Status: interactionStatusUnavailable}, nil
}
func pollIDValue(v any) (id.ID, bool) {
	if x, ok := v.(id.ID); ok {
		return x, !x.IsZero()
	}
	b, ok := v.([]byte)
	if !ok || len(b) != 16 {
		return id.ID{}, false
	}
	var x id.ID
	copy(x[:], b)
	return x, !x.IsZero()
}
func pollSummary(p polls.Poll, my []uint64) []any {
	return []any{p.ID, p.ChannelID, p.ThreadID, p.CreatorID, p.Question, stringsToAny(p.Answers), boolUint(p.Multi), p.CreatedAt, p.DueAt, boolUint(p.Closed), uintsToAny(p.Counts), uintsToAny(my), p.Revision}
}
func stringsToAny(v []string) []any {
	o := make([]any, len(v))
	for i, x := range v {
		o[i] = x
	}
	return o
}
func uintsToAny(v []uint64) []any {
	o := make([]any, len(v))
	for i, x := range v {
		o[i] = x
	}
	return o
}

// channelHint tells a poll's channel that the poll changed, only when the channel is a Bot channel: dillad keeps channel
// state for Bot channels alone and 404s any other (fix round 2, M7). A poll kept from v1 in an ordinary channel (G7)
// gets no hint, so its card refreshes when it is opened, not live.
func (h PollInteractions) channelHint(ctx context.Context, channel id.ID) {
	if CheckBotChannel(h.CommunityID, channel) != nil {
		return
	}
	hint(ctx, h.Hints, client.StateScopeChannel, channel)
}

func pollData(v any) ([]byte, error) {
	b, e := cborx.Marshal(v)
	if e != nil {
		return nil, e
	}
	if len(b) > pollPageBytes {
		return nil, polls.ErrInvalid
	}
	return b, nil
}
func (h PollInteractions) readAuthorized(ctx context.Context, actor InteractionActor, p polls.Poll) (bool, error) {
	return h.authorize(ctx, actor, p.ChannelID, pollPermView|pollPermReadHistory)
}
func (h PollInteractions) get(ctx context.Context, actor InteractionActor, r protocol.Request) (InteractionResult, error) {
	if !h.validRequest(actor, r, protocol.OpPollGet, false) || len(r.Args) != 1 {
		return InteractionResult{Status: interactionStatusUnavailable}, nil
	}
	pid, ok := pollIDValue(r.Args[0])
	if !ok {
		return InteractionResult{Status: interactionStatusUnavailable}, nil
	}
	if h.Reads != nil {
		return h.cachedGet(ctx, actor, pid)
	}
	p, e := h.Store.Get(ctx, pid)
	if e != nil {
		return InteractionResult{Status: pollResultStatus(e)}, nil
	}
	allowed, e := h.readAuthorized(ctx, actor, p)
	if e != nil {
		return InteractionResult{}, e
	}
	if !allowed {
		return InteractionResult{Status: interactionStatusForbidden}, nil
	}
	_, my, e := h.Store.Counts(ctx, p.ID, actor.User)
	if e != nil {
		return InteractionResult{}, e
	}
	data, e := pollData([]any{uint64(1), pollSummary(h.present(p), my)})
	if e != nil {
		return InteractionResult{}, e
	}
	rev := p.Revision
	return InteractionResult{Revision: &rev, Data: data}, nil
}

// cachedGet is get through the read cache: the poll (and, for the sealed poll.get, its voters) once per window; this
// member's authorisation and own choices every time.
func (h PollInteractions) cachedGet(ctx context.Context, actor InteractionActor, pid id.ID) (InteractionResult, error) {
	entry, e := h.Reads.poll(ctx, h.Store, pid)
	if e != nil {
		if ctx.Err() != nil {
			return InteractionResult{}, e
		}
		return InteractionResult{Status: pollResultStatus(e)}, nil
	}
	p := entry.poll
	allowed, e := h.readAuthorized(ctx, actor, p)
	if e != nil {
		return InteractionResult{}, e
	}
	if !allowed {
		return InteractionResult{Status: interactionStatusForbidden}, nil
	}
	my, e := h.Store.MyChoices(ctx, p.ID, actor.User)
	if e != nil {
		return InteractionResult{}, e
	}
	data, e := pollData([]any{uint64(1), pollSummary(h.present(p), my)})
	if e != nil {
		return InteractionResult{}, e
	}
	rev := p.Revision
	result := InteractionResult{Revision: &rev, Data: data}
	if wantPollVoters(ctx) {
		v, e := h.Reads.summary(ctx, h.Store, entry)
		if e != nil {
			return InteractionResult{}, e
		}
		previews := make([]any, len(v.previews))
		for i, ids := range v.previews {
			previews[i] = idAnyBytes(ids)
		}
		capped := uint64(0)
		if v.capped {
			capped = 1
		}
		result.Extra = []any{previews, uint64(v.distinct), capped} //nolint:gosec // G115: distinct counts voters, 0..pollVoterLimit
	}
	return result, nil
}

// idAnyBytes is ids as 16-byte byte strings, the wire form of an id in a sealed answer.
func idAnyBytes(ids []id.ID) []any {
	a := make([]any, len(ids))
	for i, v := range ids {
		a[i] = v[:]
	}
	return a
}
func (h PollInteractions) create(ctx context.Context, actor InteractionActor, r protocol.Request) (InteractionResult, error) {
	if h.CreateEnabled != nil {
		enabled, err := h.CreateEnabled(ctx)
		if err != nil {
			return InteractionResult{}, err
		}
		if !enabled {
			return InteractionResult{Status: interactionStatusUnavailable}, nil
		}
	}
	if !h.validRequest(actor, r, protocol.OpPollCreate, true) || len(r.Args) != 1 {
		return InteractionResult{Status: interactionStatusUnavailable}, nil
	}
	fp, ok := pollFingerprint(r)
	if !ok {
		return InteractionResult{Status: interactionStatusUnavailable}, nil
	}
	payload, ok := r.Args[0].([]byte)
	if !ok || len(payload) == 0 || len(payload) > 2200 {
		return InteractionResult{Status: interactionStatusUnavailable}, nil
	}
	var a []any
	if cborx.Unmarshal(payload, &a) != nil || len(a) != 7 || a[0] != uint64(1) {
		return unavailableInteraction()
	}
	var channel id.ID
	if c, isID := pollIDValue(a[1]); isID && c == pollDefaultChannel {
		// Bot v2 Task 17: no channel named, so the feature's configured Bot channel.
		channel, ok = h.defaultChannel(ctx)
		if !ok {
			return needsBotChannelResult(h.CommunityID, "polls", id.ID{}, "Polls post in a Bot channel. Mark one in Settings, or name one with channel."), nil
		}
	} else if channel, ok = pollIDValue(a[1]); !ok {
		return InteractionResult{Status: interactionStatusUnavailable}, nil
	}
	if err := CheckBotChannel(h.CommunityID, channel); err != nil {
		if errors.Is(err, ErrNeedsBotChannel) {
			return needsBotChannelResult(h.CommunityID, "polls", channel, "The Bot posts only in Bot channels. Pick a Bot channel for the poll."), nil
		}
		return InteractionResult{Status: interactionStatusUnavailable}, nil
	}
	var thread *id.ID
	if a[2] != nil {
		x, ok := pollIDValue(a[2])
		if !ok {
			return InteractionResult{Status: interactionStatusUnavailable}, nil
		}
		thread = &x
	}
	question, ok := a[3].(string)
	if !ok {
		return InteractionResult{Status: interactionStatusUnavailable}, nil
	}
	raw, ok := a[4].([]any)
	if !ok {
		return InteractionResult{Status: interactionStatusUnavailable}, nil
	}
	answers := make([]string, 0, len(raw))
	for _, x := range raw {
		v, ok := x.(string)
		if !ok {
			return InteractionResult{Status: interactionStatusUnavailable}, nil
		}
		answers = append(answers, v)
	}
	multi, ok := a[5].(uint64)
	if !ok || multi > 1 {
		return InteractionResult{Status: interactionStatusUnavailable}, nil
	}
	duration, ok := a[6].(uint64)
	if !ok || !validPollDuration(duration) {
		return InteractionResult{Status: interactionStatusUnavailable}, nil
	}
	allowed, e := h.authorize(ctx, actor, channel, pollPermSend)
	if e != nil {
		return InteractionResult{}, e
	}
	if !allowed {
		return InteractionResult{Status: interactionStatusForbidden}, nil
	}
	now := h.now()
	createdAt, ok := unixSeconds(now)
	if !ok || createdAt > math.MaxInt64-duration {
		return unavailableInteraction()
	}
	p := polls.Poll{ID: r.RequestID, CommunityID: h.CommunityID, ChannelID: channel, ThreadID: thread, CreatorID: actor.User, Question: question, Answers: answers, Multi: multi == 1, CreatedAt: createdAt, DueAt: createdAt + duration}
	p, e = h.Store.Create(ctx, r.RequestID, fp, p, now)
	if e != nil {
		return InteractionResult{Status: pollResultStatus(e)}, nil
	}
	if h.Publisher == nil {
		return InteractionResult{Status: interactionStatusUnavailable}, nil
	}
	if e = h.Publisher.PublishPoll(ctx, p); e != nil {
		// Logged with the error only (never the poll's content): a poll stored but not posted is answered unavailable.
		slog.WarnContext(ctx, "bot poll publication failed", "threaded", p.ThreadID != nil, "error", e.Error())
		return unavailableInteraction()
	}
	if e = h.Store.MarkPublished(ctx, p.ID); e != nil {
		slog.WarnContext(ctx, "bot poll publication not recorded", "error", e.Error())
		return unavailableInteraction()
	}
	p.Published = true
	data, e := pollData([]any{uint64(1), pollSummary(h.present(p), nil)})
	if e != nil {
		return InteractionResult{}, e
	}
	rev := p.Revision
	ClearNeedsBotChannel(h.CommunityID, "polls")
	return InteractionResult{Revision: &rev, Data: data, Note: h.postedNote(ctx, channel)}, nil
}

// defaultChannel is where a poll with no named channel goes.
func (h PollInteractions) defaultChannel(ctx context.Context) (id.ID, bool) {
	if h.DefaultChannel != nil {
		channel, ok := h.DefaultChannel(ctx)
		return channel, ok && !channel.IsZero()
	}
	return OnlyBotChannel(h.CommunityID)
}

// postedNote is the reply that names where the poll went.
func (h PollInteractions) postedNote(ctx context.Context, channel id.ID) string {
	if h.ChannelName != nil {
		if name := h.ChannelName(ctx, channel); name != "" {
			return "Posted the poll in #" + strings.TrimPrefix(name, "#") + "."
		}
	}
	return "Posted the poll."
}
func validPollDuration(v uint64) bool {
	switch v {
	case 3600, 14400, 28800, 86400, 259200, 604800:
		return true
	}
	return false
}
func (h PollInteractions) list(ctx context.Context, actor InteractionActor, r protocol.Request) (InteractionResult, error) {
	if !h.validRequest(actor, r, protocol.OpPollList, false) || len(r.Args) != 3 {
		return InteractionResult{Status: interactionStatusUnavailable}, nil
	}
	ch, ok := pollIDValue(r.Args[0])
	if !ok {
		return InteractionResult{Status: interactionStatusUnavailable}, nil
	}
	var th *id.ID
	if r.Args[1] != nil {
		x, ok := pollIDValue(r.Args[1])
		if !ok {
			return InteractionResult{Status: interactionStatusUnavailable}, nil
		}
		th = &x
	}
	cursor := ""
	if r.Args[2] != nil {
		b, ok := r.Args[2].([]byte)
		if !ok || len(b) > 256 {
			return InteractionResult{Status: interactionStatusUnavailable}, nil
		}
		cursor = base64.RawURLEncoding.EncodeToString(b)
	}
	allowed, e := h.authorize(ctx, actor, ch, pollPermView|pollPermReadHistory)
	if e != nil {
		return InteractionResult{}, e
	}
	if !allowed {
		return InteractionResult{Status: interactionStatusForbidden}, nil
	}
	rows, next, e := h.Store.List(ctx, ch, th, cursor)
	if e != nil {
		return InteractionResult{Status: pollResultStatus(e)}, nil
	}
	summaries := make([]any, 0, len(rows))
	var data []byte
	included := 0
	for i, p := range rows {
		_, my, countErr := h.Store.Counts(ctx, p.ID, actor.User)
		if countErr != nil {
			return InteractionResult{}, countErr
		}
		candidate := append(append([]any(nil), summaries...), pollSummary(h.present(p), my))
		more := i < len(rows)-1 || next != ""
		var candidateCursor any
		if more {
			cursor, cursorErr := h.Store.ListCursor(ctx, ch, th, p.ID)
			if cursorErr != nil {
				return InteractionResult{}, cursorErr
			}
			raw, cursorErr := base64.RawURLEncoding.DecodeString(cursor)
			if cursorErr != nil {
				return InteractionResult{}, cursorErr
			}
			candidateCursor = raw
		}
		candidateData, marshalErr := pollData([]any{uint64(1), candidate, candidateCursor})
		if marshalErr != nil {
			if included == 0 {
				return InteractionResult{Status: interactionStatusUnavailable}, nil
			}
			break
		}
		summaries = candidate
		data = candidateData
		included = i + 1
	}
	if len(rows) > 0 && included == 0 {
		return InteractionResult{Status: interactionStatusUnavailable}, nil
	}
	if included == 0 {
		data, e = pollData([]any{uint64(1), []any{}, nil})
		if e != nil {
			return InteractionResult{}, e
		}
	}
	return InteractionResult{Data: data}, nil
}
func (h PollInteractions) vote(ctx context.Context, actor InteractionActor, r protocol.Request) (InteractionResult, error) {
	if !h.validRequest(actor, r, protocol.OpPollVote, true) || len(r.Args) != 2 || r.Revision == nil {
		return InteractionResult{Status: interactionStatusUnavailable}, nil
	}
	pid, ok := pollIDValue(r.Args[0])
	raw, ok2 := r.Args[1].([]byte)
	if !ok || !ok2 {
		return unavailableInteraction()
	}
	var a []any
	if cborx.Unmarshal(raw, &a) != nil || len(a) > 10 {
		return unavailableInteraction()
	}
	choices := make([]uint64, 0, len(a))
	for _, x := range a {
		v, ok := x.(uint64)
		if !ok {
			return InteractionResult{Status: interactionStatusUnavailable}, nil
		}
		choices = append(choices, v)
	}
	p, e := h.Store.Get(ctx, pid)
	if e != nil {
		return InteractionResult{Status: pollResultStatus(e)}, nil
	}
	allowed, e := h.authorize(ctx, actor, p.ChannelID, pollPermView|pollPermSend)
	if e != nil {
		return InteractionResult{}, e
	}
	if !allowed {
		return InteractionResult{Status: interactionStatusForbidden}, nil
	}
	fp, _ := pollFingerprint(r)
	out, e := h.Store.Vote(ctx, pid, actor.User, r.RequestID, fp, choices, *r.Revision, h.now())
	if e != nil {
		return InteractionResult{Status: pollResultStatus(e)}, nil
	}
	p = out.Poll
	h.Reads.forget(pid)
	data, e := pollData([]any{uint64(1), pollSummary(h.present(p), out.Choice)})
	rev := p.Revision
	h.channelHint(ctx, p.ChannelID)
	return InteractionResult{Revision: &rev, Data: data}, e
}
func (h PollInteractions) close(ctx context.Context, actor InteractionActor, r protocol.Request) (InteractionResult, error) {
	if !h.validRequest(actor, r, protocol.OpPollClose, true) || len(r.Args) != 1 || r.Revision == nil {
		return InteractionResult{Status: interactionStatusUnavailable}, nil
	}
	pid, ok := pollIDValue(r.Args[0])
	if !ok {
		return InteractionResult{Status: interactionStatusUnavailable}, nil
	}
	p, e := h.Store.Get(ctx, pid)
	if e != nil {
		return InteractionResult{Status: pollResultStatus(e)}, nil
	}
	bits, e := h.Authority.PollPermissions(ctx, actor.User, p.ChannelID)
	if e != nil {
		return InteractionResult{}, e
	}
	member, e := h.Authority.PollMember(ctx, actor.User)
	if e != nil {
		return InteractionResult{}, e
	}
	channelAccess := pollPermView | pollPermReadHistory
	if actor.Kind != 0 || !member || bits&channelAccess != channelAccess || (actor.User != p.CreatorID && bits&pollPermManageMessages == 0) {
		return InteractionResult{Status: interactionStatusForbidden}, nil
	}
	fp, _ := pollFingerprint(r)
	p, e = h.Store.Close(ctx, pid, actor.User, r.RequestID, fp, *r.Revision, h.now(), bits&pollPermManageMessages != 0)
	if e != nil {
		return InteractionResult{Status: pollResultStatus(e)}, nil
	}
	h.Reads.forget(pid)
	_, my, e := h.Store.Counts(ctx, p.ID, actor.User)
	if e != nil {
		return InteractionResult{}, e
	}
	data, e := pollData([]any{uint64(1), pollSummary(h.present(p), my)})
	rev := p.Revision
	h.channelHint(ctx, p.ChannelID)
	return InteractionResult{Revision: &rev, Data: data}, e
}
func (h PollInteractions) voters(ctx context.Context, actor InteractionActor, r protocol.Request) (InteractionResult, error) {
	if !h.validRequest(actor, r, protocol.OpPollVoters, false) || len(r.Args) != 3 {
		return InteractionResult{Status: interactionStatusUnavailable}, nil
	}
	pid, ok := pollIDValue(r.Args[0])
	option, ok2 := r.Args[1].(uint64)
	if !ok || !ok2 {
		return InteractionResult{Status: interactionStatusUnavailable}, nil
	}
	p, e := h.Store.Get(ctx, pid)
	if e != nil {
		return InteractionResult{Status: pollResultStatus(e)}, nil
	}
	allowed, e := h.readAuthorized(ctx, actor, p)
	if e != nil {
		return InteractionResult{}, e
	}
	if !allowed {
		return InteractionResult{Status: interactionStatusForbidden}, nil
	}
	if option >= uint64(len(p.Answers)) {
		return InteractionResult{Status: interactionStatusUnavailable}, nil
	}
	var after []byte
	if r.Args[2] != nil {
		b, ok := r.Args[2].([]byte)
		if !ok || len(b) > 256 {
			return InteractionResult{Status: interactionStatusUnavailable}, nil
		}
		var a []any
		if cborx.Unmarshal(b, &a) != nil || len(a) != 4 || a[0] != uint64(1) {
			return unavailableInteraction()
		}
		cursorPoll, validID := pollIDValue(a[1])
		cursorOption, validOption := a[2].(uint64)
		if !validID || cursorPoll != p.ID || !validOption || cursorOption != option {
			return InteractionResult{Status: interactionStatusUnavailable}, nil
		}
		after, ok = a[3].([]byte)
		if !ok || !strings.HasPrefix(string(after), fmt.Sprintf("%s%x/", "dilla.bot.poll-vote.v1/", pid[:])) {
			return InteractionResult{Status: interactionStatusUnavailable}, nil
		}
	}
	ids, next, e := h.Store.Voters(ctx, pid, option, after, 20)
	if e != nil {
		return InteractionResult{Status: pollResultStatus(e)}, nil
	}
	var nextValue any
	if next != nil {
		raw, e := cborx.Marshal([]any{uint64(1), p.ID, option, next})
		if e != nil {
			return InteractionResult{}, e
		}
		if len(raw) > 256 {
			return InteractionResult{Status: interactionStatusUnavailable}, nil
		}
		nextValue = raw
	}
	data, e := pollData([]any{uint64(1), idAnyPoll(ids), nextValue})
	return InteractionResult{Data: data}, e
}
func idAnyPoll(ids []id.ID) []any {
	a := make([]any, len(ids))
	for i, v := range ids {
		a[i] = v
	}
	return a
}
func (h PollInteractions) Reconcile(ctx context.Context) error {
	if h.Store == nil || h.Publisher == nil {
		return errors.New("missing poll reconciliation dependency")
	}
	return h.Store.Reconcile(ctx, h.now(), h.Publisher)
}
