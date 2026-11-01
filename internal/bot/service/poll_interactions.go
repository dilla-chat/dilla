package service

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/jonasthim/dilla/internal/bot/helper"
	"github.com/jonasthim/dilla/internal/bot/polls"
	"github.com/jonasthim/dilla/internal/bot/protocol"
	"github.com/jonasthim/dilla/internal/cborx"
	"github.com/jonasthim/dilla/internal/id"
)

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
	channel, ok := pollIDValue(a[1])
	if !ok {
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
		return unavailableInteraction()
	}
	if e = h.Store.MarkPublished(ctx, p.ID); e != nil {
		return unavailableInteraction()
	}
	p.Published = true
	data, e := pollData([]any{uint64(1), pollSummary(h.present(p), nil)})
	if e != nil {
		return InteractionResult{}, e
	}
	rev := p.Revision
	return InteractionResult{Revision: &rev, Data: data}, nil
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
	data, e := pollData([]any{uint64(1), pollSummary(h.present(p), out.Choice)})
	rev := p.Revision
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
	_, my, e := h.Store.Counts(ctx, p.ID, actor.User)
	if e != nil {
		return InteractionResult{}, e
	}
	data, e := pollData([]any{uint64(1), pollSummary(h.present(p), my)})
	rev := p.Revision
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
