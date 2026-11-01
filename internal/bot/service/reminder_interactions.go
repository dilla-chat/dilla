package service

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"time"
	_ "time/tzdata"
	"unicode/utf8"

	"github.com/jonasthim/dilla/internal/bot/helper"
	"github.com/jonasthim/dilla/internal/bot/protocol"
	"github.com/jonasthim/dilla/internal/bot/reminders"
	"github.com/jonasthim/dilla/internal/cborx"
	"github.com/jonasthim/dilla/internal/id"
)

const reminderPageBytes = 2400

type ReminderAuthority interface {
	ReminderMember(context.Context, id.ID) (bool, error)
}
type reminderSourceAuthority interface {
	ReminderSource(context.Context, id.ID, id.ID) (bool, error)
}
type ReminderInteractions struct {
	Store       *reminders.Store
	Authority   ReminderAuthority
	CommunityID id.ID
	Now         func() time.Time
	// CreateEnabled reads the installation's current Reminders capability (bit 9, Task 10c) when
	// a reminder is created. Off refuses only creation: due reminders are still delivered and
	// members can list, cancel and dismiss theirs. Nil allows creation.
	CreateEnabled func(context.Context) (bool, error)
}

func (h ReminderInteractions) Handlers() map[protocol.Operation]InteractionHandlerSpec {
	return map[protocol.Operation]InteractionHandlerSpec{
		protocol.OpReminderCreate:  {RevisionInHandler: true, Handle: h.create},
		protocol.OpReminderList:    {RecomputeOnReplay: true, Handle: h.list},
		protocol.OpReminderCancel:  {RevisionInHandler: true, Handle: h.cancel},
		protocol.OpReminderDismiss: {RevisionInHandler: true, Handle: h.dismiss},
		protocol.OpReminderDue:     {RecomputeOnReplay: true, Handle: h.due},
	}
}
func (h ReminderInteractions) now() time.Time {
	if h.Now != nil {
		return h.Now().UTC()
	}
	return time.Now().UTC()
}
func (h ReminderInteractions) valid(r protocol.Request, op protocol.Operation, mutation bool) bool {
	return h.Store != nil && h.Authority != nil && !h.CommunityID.IsZero() && r.CommunityID == h.CommunityID && r.ChannelID == nil && r.Operation == op && (!mutation || (r.RequestID != id.ID{} && r.Revision != nil && r.Expires != nil))
}
func (h ReminderInteractions) member(ctx context.Context, actor InteractionActor) (bool, error) {
	if actor.Kind != 0 || actor.User.IsZero() {
		return false, nil
	}
	return h.Authority.ReminderMember(ctx, actor.User)
}
func reminderFingerprint(r protocol.Request) ([32]byte, bool) {
	if r.RequestID.IsZero() {
		return [32]byte{}, false
	}
	s, e := protocol.EncodeRequest(r)
	if e != nil {
		return [32]byte{}, false
	}
	return sha256.Sum256([]byte(s)), true
}
func reminderStatus(e error) uint64 {
	var nativeStatus *helper.StatusError
	if errors.As(e, &nativeStatus) && nativeStatus.Status == 4 {
		return interactionStatusConflict
	}
	switch {
	case errors.Is(e, reminders.ErrForbidden):
		return interactionStatusForbidden
	case errors.Is(e, reminders.ErrConflict), errors.Is(e, reminders.ErrMutationIDConflict), errors.Is(e, reminders.ErrQuota):
		return interactionStatusConflict
	case errors.Is(e, reminders.ErrNotFound), errors.Is(e, reminders.ErrInvalid):
		return interactionStatusUnavailable
	default:
		return interactionStatusUnavailable
	}
}
func reminderID(v any) (id.ID, bool) {
	switch x := v.(type) {
	case id.ID:
		return x, !x.IsZero()
	case []byte:
		if len(x) != 16 {
			return id.ID{}, false
		}
		var out id.ID
		copy(out[:], x)
		return out, !out.IsZero()
	default:
		return id.ID{}, false
	}
}
func reminderSummary(r reminders.Reminder) []any {
	return []any{r.ID, r.Text, r.DueAt, r.Timezone, r.SourceChannel, r.SourceThread, r.State, r.Revision}
}
func reminderData(v any) ([]byte, error) {
	b, e := cborx.Marshal(v)
	if e != nil || len(b) > reminderPageBytes {
		return nil, reminders.ErrInvalid
	}
	return b, nil
}
func (h ReminderInteractions) checkSource(ctx context.Context, actor id.ID, r reminders.Reminder) (bool, error) {
	if r.SourceChannel == nil {
		return true, nil
	}
	a, ok := h.Authority.(reminderSourceAuthority)
	if !ok {
		return false, nil
	}
	return a.ReminderSource(ctx, actor, *r.SourceChannel)
}
func (h ReminderInteractions) withVisibleSource(ctx context.Context, actor id.ID, r reminders.Reminder) reminders.Reminder {
	if r.SourceChannel == nil {
		return r
	}
	allowed, err := h.checkSource(ctx, actor, r)
	if err != nil || !allowed {
		r.SourceChannel = nil
		r.SourceThread = nil
	}
	return r
}
func (h ReminderInteractions) create(ctx context.Context, actor InteractionActor, r protocol.Request) (InteractionResult, error) {
	if h.CreateEnabled != nil {
		enabled, err := h.CreateEnabled(ctx)
		if err != nil {
			return InteractionResult{}, err
		}
		if !enabled {
			return InteractionResult{Status: interactionStatusUnavailable}, nil
		}
	}
	if !h.valid(r, protocol.OpReminderCreate, true) || len(r.Args) != 1 {
		return InteractionResult{Status: interactionStatusUnavailable}, nil
	}
	ok, e := h.member(ctx, actor)
	if e != nil {
		return InteractionResult{}, e
	}
	if !ok {
		return InteractionResult{Status: interactionStatusForbidden}, nil
	}
	fp, ok := reminderFingerprint(r)
	if !ok {
		return InteractionResult{Status: interactionStatusUnavailable}, nil
	}
	if saved, found, replayErr := h.Store.ReplayCreate(ctx, r.RequestID, actor.User, fp); replayErr != nil {
		return InteractionResult{Status: reminderStatus(replayErr)}, nil
	} else if found {
		return reminderCreateResult(h.withVisibleSource(ctx, actor.User, saved))
	}
	raw, ok := r.Args[0].([]byte)
	if !ok || len(raw) < 1 || len(raw) > 2200 {
		return InteractionResult{Status: interactionStatusUnavailable}, nil
	}
	var a []any
	if cborx.Unmarshal(raw, &a) != nil || len(a) != 7 || a[0] != uint64(1) {
		return unavailableInteraction()
	}
	kind, ok := a[1].(uint64)
	if !ok || kind > 1 {
		return InteractionResult{Status: interactionStatusUnavailable}, nil
	}
	when, ok := a[2].(uint64)
	if !ok {
		return InteractionResult{Status: interactionStatusUnavailable}, nil
	}
	txt, ok := a[3].(string)
	if !ok {
		return InteractionResult{Status: interactionStatusUnavailable}, nil
	}
	zone, ok := a[4].(string)
	if !ok || len(zone) > 100 {
		return InteractionResult{Status: interactionStatusUnavailable}, nil
	}
	if _, e = time.LoadLocation(zone); e != nil {
		return unavailableInteraction()
	}
	var channel, thread *id.ID
	if a[5] != nil {
		x, ok := reminderID(a[5])
		if !ok {
			return InteractionResult{Status: interactionStatusUnavailable}, nil
		}
		channel = &x
	}
	if a[6] != nil {
		x, ok := reminderID(a[6])
		if !ok || channel == nil {
			return InteractionResult{Status: interactionStatusUnavailable}, nil
		}
		thread = &x
	}
	rr := reminders.Reminder{Text: txt, Timezone: zone, SourceChannel: channel, SourceThread: thread}
	if channel != nil {
		allowed, e := h.checkSource(ctx, actor.User, rr)
		if e != nil {
			return InteractionResult{}, e
		}
		if !allowed {
			return InteractionResult{Status: interactionStatusForbidden}, nil
		}
	}
	now := h.now()
	var due time.Time
	if kind == 0 {
		if when < 60 || when > 31536000 {
			return InteractionResult{Status: interactionStatusUnavailable}, nil
		}
		due = now.Add(time.Duration(when) * time.Second)
	} else {
		if when > 253402300799 {
			return InteractionResult{Status: interactionStatusUnavailable}, nil
		}
		due = time.Unix(int64(when), 0).UTC()
		delta := due.Sub(now)
		if delta < time.Minute || delta > 365*24*time.Hour {
			return InteractionResult{Status: interactionStatusUnavailable}, nil
		}
	}
	if len(txt) < 1 || len(txt) > 1500 || !utf8String(txt) {
		return InteractionResult{Status: interactionStatusUnavailable}, nil
	}
	out, e := h.Store.Create(ctx, r.RequestID, actor.User, fp, rr, due)
	if e != nil {
		return InteractionResult{Status: reminderStatus(e)}, nil
	}
	return reminderCreateResult(out)
}
func reminderCreateResult(out reminders.Reminder) (InteractionResult, error) {
	data, err := reminderData([]any{uint64(1), reminderSummary(out)})
	if err != nil {
		return InteractionResult{}, err
	}
	rev := out.Revision
	return InteractionResult{Revision: &rev, Data: data}, nil
}
func utf8String(s string) bool { return utf8.ValidString(s) && len([]rune(s)) > 0 }
func (h ReminderInteractions) cursor(r protocol.Request) (string, bool) {
	if len(r.Args) != 1 {
		return "", false
	}
	if r.Args[0] == nil {
		return "", true
	}
	raw, ok := r.Args[0].([]byte)
	if !ok || len(raw) > 256 {
		return "", false
	}
	return base64.RawURLEncoding.EncodeToString(raw), true
}
func (h ReminderInteractions) list(ctx context.Context, actor InteractionActor, r protocol.Request) (InteractionResult, error) {
	return h.readPage(ctx, actor, r, protocol.OpReminderList, false)
}
func (h ReminderInteractions) due(ctx context.Context, actor InteractionActor, r protocol.Request) (InteractionResult, error) {
	return h.readPage(ctx, actor, r, protocol.OpReminderDue, true)
}
func (h ReminderInteractions) readPage(ctx context.Context, actor InteractionActor, r protocol.Request, op protocol.Operation, dueOnly bool) (InteractionResult, error) {
	if !h.valid(r, op, false) {
		return InteractionResult{Status: interactionStatusUnavailable}, nil
	}
	ok, e := h.member(ctx, actor)
	if e != nil {
		return InteractionResult{}, e
	}
	if !ok {
		return InteractionResult{Status: interactionStatusForbidden}, nil
	}
	cur, ok := h.cursor(r)
	if !ok {
		return InteractionResult{Status: interactionStatusUnavailable}, nil
	}
	rows, next, e := h.Store.List(ctx, actor.User, dueOnly, cur, h.now())
	if e != nil {
		return InteractionResult{Status: reminderStatus(e)}, nil
	}
	summaries := make([]any, len(rows))
	for i, row := range rows {
		row = h.withVisibleSource(ctx, actor.User, row)
		summaries[i] = reminderSummary(row)
	}
	for count := len(summaries); count >= 0; count-- {
		pageNext := next
		if count < len(rows) && count > 0 {
			pageNext = reminders.EncodeCursor(actor.User, rows[count-1])
		}
		var n any
		if pageNext != "" {
			b, e := base64.RawURLEncoding.DecodeString(pageNext)
			if e != nil {
				return InteractionResult{}, e
			}
			n = b
		}
		data, e := cborx.Marshal([]any{uint64(1), summaries[:count], n})
		if e == nil && len(data) <= reminderPageBytes {
			return InteractionResult{Data: data}, nil
		}
	}
	return InteractionResult{}, reminders.ErrInvalid
}
func (h ReminderInteractions) mutation(ctx context.Context, actor InteractionActor, r protocol.Request, op protocol.Operation) (InteractionResult, error) {
	if !h.valid(r, op, true) || len(r.Args) != 1 {
		return InteractionResult{Status: interactionStatusUnavailable}, nil
	}
	ok, e := h.member(ctx, actor)
	if e != nil {
		return InteractionResult{}, e
	}
	if !ok {
		return InteractionResult{Status: interactionStatusForbidden}, nil
	}
	pid, ok := reminderID(r.Args[0])
	if !ok {
		return InteractionResult{Status: interactionStatusUnavailable}, nil
	}
	fp, ok := reminderFingerprint(r)
	if !ok {
		return InteractionResult{Status: interactionStatusUnavailable}, nil
	}
	expected := *r.Revision
	var out reminders.Reminder
	if op == protocol.OpReminderCancel {
		out, e = h.Store.Cancel(ctx, pid, actor.User, r.RequestID, fp, expected, h.now())
	} else {
		out, e = h.Store.Dismiss(ctx, pid, actor.User, r.RequestID, fp, expected, h.now())
	}
	if e != nil {
		return InteractionResult{Status: reminderStatus(e)}, nil
	}
	out = h.withVisibleSource(ctx, actor.User, out)
	data, e := reminderData([]any{uint64(1), reminderSummary(out)})
	if e != nil {
		return InteractionResult{}, e
	}
	rev := out.Revision
	return InteractionResult{Revision: &rev, Data: data}, nil
}
func (h ReminderInteractions) cancel(ctx context.Context, a InteractionActor, r protocol.Request) (InteractionResult, error) {
	return h.mutation(ctx, a, r, protocol.OpReminderCancel)
}
func (h ReminderInteractions) dismiss(ctx context.Context, a InteractionActor, r protocol.Request) (InteractionResult, error) {
	return h.mutation(ctx, a, r, protocol.OpReminderDismiss)
}
