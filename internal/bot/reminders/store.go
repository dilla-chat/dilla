// Package reminders persists account-owned reminders in encrypted native state.
package reminders

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jonasthim/dilla/internal/bot/helper"
	"github.com/jonasthim/dilla/internal/bot/native"
	"github.com/jonasthim/dilla/internal/cborx"
	"github.com/jonasthim/dilla/internal/id"
)

const (
	stateArea      uint64 = 8
	reminderPrefix        = "dilla.bot.reminder.v1/"
	userPrefix            = "dilla.bot.reminder-user.v1/"
	receiptPrefix         = "dilla.bot.reminder-receipt.v1/"
	maxStateValue         = 262144
	maxOutstanding        = 100
	maxTx                 = 64
)

var (
	ErrConflict           = errors.New("reminder state conflict")
	ErrNotFound           = errors.New("reminder not found")
	ErrMutationIDConflict = errors.New("reminder request ID reused with different input")
	ErrQuota              = errors.New("reminder quota reached")
	ErrInvalid            = errors.New("invalid reminder")
	ErrForbidden          = errors.New("reminder actor does not own record")
)

type State interface {
	StateGet(context.Context, uint64, []byte) (native.StateValue, error)
	StateScan(context.Context, uint64, []byte, uint64) (native.StatePage, error)
	StateTransaction(context.Context, id.ID, []native.StateChange) ([]uint64, error)
}
type Reminder struct {
	ID, CommunityID, InstallationID, OwnerID id.ID
	Text, Timezone                           string
	DueAt                                    uint64
	SourceChannel, SourceThread              *id.ID
	State                                    uint64
	Revision                                 uint64
}

const (
	StateScheduled uint64 = iota
	StateDue
	StateCancelled
	StateDismissed
)

type Store struct {
	state                   State
	community, installation id.ID
	Now                     func() time.Time
}

func NewStore(s State, binding native.Binding, clock ...func() time.Time) *Store {
	now := func() time.Time { return time.Now().UTC() }
	if len(clock) > 0 && clock[0] != nil {
		now = clock[0]
	}
	return &Store{state: s, community: binding.Community, installation: binding.Installation, Now: now}
}

func unixSeconds(now time.Time) (uint64, bool) {
	seconds := now.Unix()
	if seconds < 0 {
		return 0, false
	}
	return uint64(seconds), true
}
func hx(v id.ID) string          { return fmt.Sprintf("%x", v[:]) }
func reminderKey(v id.ID) []byte { return []byte(reminderPrefix + hx(v)) }
func userKey(v id.ID) []byte     { return []byte(userPrefix + hx(v)) }
func receiptKey(actor, request id.ID) []byte {
	return []byte(receiptPrefix + hx(actor) + "/" + hx(request))
}
func (s *Store) get(ctx context.Context, k []byte) (native.StateValue, error) {
	return s.state.StateGet(ctx, stateArea, k)
}
func (s *Store) transact(ctx context.Context, tx id.ID, changes []native.StateChange) ([]uint64, error) {
	revisions, err := s.state.StateTransaction(ctx, tx, changes)
	if err != nil {
		var status *helper.StatusError
		if errors.As(err, &status) && status.Status == 4 {
			return nil, ErrConflict
		}
	}
	return revisions, err
}
func idValue(x any) (id.ID, bool) {
	switch v := x.(type) {
	case id.ID:
		return v, !v.IsZero()
	case []byte:
		if len(v) != 16 {
			return id.ID{}, false
		}
		var o id.ID
		copy(o[:], v)
		return o, !o.IsZero()
	default:
		return id.ID{}, false
	}
}
func validate(r Reminder) error {
	if r.ID.IsZero() || r.OwnerID.IsZero() || r.CommunityID.IsZero() || r.InstallationID.IsZero() || len(r.Text) < 1 || len(r.Text) > 1500 || !utf8Valid(r.Text) || len(r.Timezone) < 1 || len(r.Timezone) > 100 || r.DueAt == 0 || r.State > StateDismissed || r.Revision == 0 {
		return ErrInvalid
	}
	if r.SourceThread != nil && (r.SourceChannel == nil || r.SourceThread.IsZero()) {
		return ErrInvalid
	}
	if r.SourceChannel != nil && r.SourceChannel.IsZero() {
		return ErrInvalid
	}
	return nil
}
func utf8Valid(s string) bool { return strings.ToValidUTF8(s, "\uFFFD") == s }
func encodeReminder(r Reminder) ([]byte, error) {
	b, e := cborx.Marshal([]any{uint64(1), r.ID, r.CommunityID, r.InstallationID, r.OwnerID, r.Text, r.DueAt, r.Timezone, r.SourceChannel, r.SourceThread, r.State, r.Revision})
	if e != nil || len(b) > maxStateValue {
		return nil, ErrInvalid
	}
	return b, nil
}
func decodeReminder(b []byte) (Reminder, error) {
	var a []any
	if cborx.Unmarshal(b, &a) != nil || len(a) != 12 || a[0] != uint64(1) {
		return Reminder{}, ErrInvalid
	}
	var r Reminder
	var ok bool
	if r.ID, ok = idValue(a[1]); !ok {
		return r, ErrInvalid
	}
	if r.CommunityID, ok = idValue(a[2]); !ok {
		return r, ErrInvalid
	}
	if r.InstallationID, ok = idValue(a[3]); !ok {
		return r, ErrInvalid
	}
	if r.OwnerID, ok = idValue(a[4]); !ok {
		return r, ErrInvalid
	}
	if r.Text, ok = a[5].(string); !ok {
		return r, ErrInvalid
	}
	if r.DueAt, ok = a[6].(uint64); !ok {
		return r, ErrInvalid
	}
	if r.Timezone, ok = a[7].(string); !ok {
		return r, ErrInvalid
	}
	if a[8] != nil {
		v, b := idValue(a[8])
		if !b {
			return r, ErrInvalid
		}
		r.SourceChannel = &v
	}
	if a[9] != nil {
		v, b := idValue(a[9])
		if !b {
			return r, ErrInvalid
		}
		r.SourceThread = &v
	}
	if r.State, ok = a[10].(uint64); !ok {
		return r, ErrInvalid
	}
	if r.Revision, ok = a[11].(uint64); !ok {
		return r, ErrInvalid
	}
	if e := validate(r); e != nil {
		return r, e
	}
	return r, nil
}
func encodeIDs(v []id.ID) ([]byte, error) {
	a := make([]any, len(v))
	for i := range v {
		a[i] = v[i]
	}
	return cborx.Marshal(a)
}
func decodeIDs(b []byte) ([]id.ID, error) {
	var a []any
	if cborx.Unmarshal(b, &a) != nil || len(a) > maxOutstanding {
		return nil, ErrInvalid
	}
	o := make([]id.ID, 0, len(a))
	seen := map[id.ID]bool{}
	for _, x := range a {
		v, ok := idValue(x)
		if !ok || seen[v] {
			return nil, ErrInvalid
		}
		seen[v] = true
		o = append(o, v)
	}
	return o, nil
}

type receipt struct {
	fingerprint [32]byte
	result      Reminder
}

func encodeReceipt(fp [32]byte, r Reminder) ([]byte, error) {
	b, e := encodeReminder(r)
	if e != nil {
		return nil, e
	}
	return cborx.Marshal([]any{uint64(1), fp[:], b})
}
func decodeReceipt(b []byte) (receipt, error) {
	var a []any
	if cborx.Unmarshal(b, &a) != nil || len(a) != 3 || a[0] != uint64(1) {
		return receipt{}, ErrInvalid
	}
	f, ok := a[1].([]byte)
	raw, ok2 := a[2].([]byte)
	if !ok || len(f) != 32 || !ok2 {
		return receipt{}, ErrInvalid
	}
	r, e := decodeReminder(raw)
	if e != nil {
		return receipt{}, e
	}
	var out receipt
	copy(out.fingerprint[:], f)
	out.result = r
	return out, nil
}
func (s *Store) replay(ctx context.Context, actor, request id.ID, fp [32]byte) (Reminder, bool, error) {
	v, e := s.get(ctx, receiptKey(actor, request))
	if e != nil || v.Revision == 0 || v.Tombstone {
		return Reminder{}, false, e
	}
	r, e := decodeReceipt(v.Value)
	if e != nil {
		return Reminder{}, true, e
	}
	if r.fingerprint != fp {
		return Reminder{}, true, ErrMutationIDConflict
	}
	if r.result.OwnerID != actor || r.result.CommunityID != s.community || r.result.InstallationID != s.installation {
		return Reminder{}, true, ErrConflict
	}
	return r.result, true, nil
}
func (s *Store) ReplayCreate(ctx context.Context, request, actor id.ID, fp [32]byte) (Reminder, bool, error) {
	if s == nil || s.state == nil || request.IsZero() || actor.IsZero() {
		return Reminder{}, false, ErrInvalid
	}
	return s.replay(ctx, actor, request, fp)
}
func (s *Store) Create(ctx context.Context, request, actor id.ID, fp [32]byte, r Reminder, due time.Time) (Reminder, error) {
	if s == nil || s.state == nil || s.community.IsZero() || s.installation.IsZero() || request.IsZero() || actor.IsZero() {
		return Reminder{}, ErrInvalid
	}
	if old, ok, e := s.replay(ctx, actor, request, fp); ok {
		return old, e
	}
	r.ID = request
	r.OwnerID = actor
	r.CommunityID = s.community
	r.InstallationID = s.installation
	if due.Before(time.Unix(0, 0)) || due.Unix() < 0 {
		return Reminder{}, ErrInvalid
	}
	dueAt, ok := unixSeconds(due.UTC())
	if !ok {
		return Reminder{}, ErrInvalid
	}
	r.DueAt = dueAt
	r.State = StateScheduled
	r.Revision = 1
	if r.DueAt == 0 || validate(r) != nil {
		return Reminder{}, ErrInvalid
	}
	uv, e := s.get(ctx, userKey(actor))
	if e != nil {
		return Reminder{}, e
	}
	var ids []id.ID
	if uv.Revision > 0 && !uv.Tombstone {
		ids, e = decodeIDs(uv.Value)
		if e != nil {
			return Reminder{}, e
		}
	}
	for _, x := range ids {
		if x == request {
			return Reminder{}, ErrConflict
		}
	}
	if len(ids) >= maxOutstanding {
		return Reminder{}, ErrQuota
	}
	ids = append(ids, request)
	sort.Slice(ids, func(i, j int) bool { return strings.Compare(hx(ids[i]), hx(ids[j])) < 0 })
	ub, e := encodeIDs(ids)
	if e != nil {
		return Reminder{}, e
	}
	rb, e := encodeReminder(r)
	if e != nil {
		return Reminder{}, e
	}
	rec, e := encodeReceipt(fp, r)
	if e != nil {
		return Reminder{}, e
	}
	rv, e := s.get(ctx, receiptKey(actor, request))
	if e != nil {
		return Reminder{}, e
	}
	_, e = s.transact(ctx, request, []native.StateChange{{Area: 8, Key: reminderKey(request), ExpectedRevision: 0, Value: rb}, {Area: 8, Key: userKey(actor), ExpectedRevision: uv.Revision, Value: ub}, {Area: 8, Key: receiptKey(actor, request), ExpectedRevision: rv.Revision, Value: rec}})
	if e != nil {
		return Reminder{}, e
	}
	return r, nil
}
func (s *Store) record(ctx context.Context, reminder, actor id.ID) (Reminder, native.StateValue, error) {
	if s == nil || s.state == nil {
		return Reminder{}, native.StateValue{}, ErrInvalid
	}
	v, err := s.get(ctx, reminderKey(reminder))
	if err != nil {
		return Reminder{}, v, err
	}
	if v.Revision == 0 || v.Tombstone {
		return Reminder{}, v, ErrNotFound
	}
	r, err := decodeReminder(v.Value)
	if err != nil {
		return Reminder{}, v, err
	}
	if r.OwnerID != actor || r.CommunityID != s.community || r.InstallationID != s.installation {
		return Reminder{}, v, ErrNotFound
	}
	return r, v, nil
}
func (s *Store) Get(ctx context.Context, reminder, actor id.ID) (Reminder, error) {
	r, _, err := s.record(ctx, reminder, actor)
	return r, err
}
func (s *Store) mutate(ctx context.Context, pid, actor, request id.ID, fp [32]byte, expected uint64, now time.Time, target uint64) (Reminder, error) {
	if old, ok, e := s.replay(ctx, actor, request, fp); ok {
		return old, e
	}
	if _, ok := unixSeconds(now); !ok {
		return Reminder{}, ErrInvalid
	}
	r, v, e := s.record(ctx, pid, actor)
	if e != nil {
		return Reminder{}, e
	}
	if r.Revision != expected {
		return Reminder{}, ErrConflict
	}
	if nowSeconds, ok := unixSeconds(now); r.State == StateScheduled && ok && nowSeconds >= r.DueAt {
		r.State = StateDue
	}
	if target == StateCancelled && r.State != StateScheduled && r.State != StateDue {
		return Reminder{}, ErrConflict
	}
	if target == StateDismissed && r.State != StateDue {
		return Reminder{}, ErrConflict
	}
	r.State = target
	r.Revision++
	rb, e := encodeReminder(r)
	if e != nil {
		return Reminder{}, e
	}
	rec, e := encodeReceipt(fp, r)
	if e != nil {
		return Reminder{}, e
	}
	rv, e := s.get(ctx, receiptKey(actor, request))
	if e != nil {
		return Reminder{}, e
	}
	changes := []native.StateChange{{Area: 8, Key: reminderKey(pid), ExpectedRevision: v.Revision, Value: rb}, {Area: 8, Key: receiptKey(actor, request), ExpectedRevision: rv.Revision, Value: rec}}
	if target == StateCancelled || target == StateDismissed {
		uv, e := s.get(ctx, userKey(actor))
		if e != nil {
			return Reminder{}, e
		}
		ids, e := decodeIDs(uv.Value)
		if e != nil {
			return Reminder{}, e
		}
		out := ids[:0]
		for _, x := range ids {
			if x != pid {
				out = append(out, x)
			}
		}
		ub, e := encodeIDs(out)
		if e != nil {
			return Reminder{}, e
		}
		changes = append(changes, native.StateChange{Area: 8, Key: userKey(actor), ExpectedRevision: uv.Revision, Value: ub})
	}
	if len(changes) > maxTx {
		return Reminder{}, ErrInvalid
	}
	_, e = s.transact(ctx, request, changes)
	return r, e
}
func (s *Store) Cancel(ctx context.Context, pid, actor, request id.ID, fp [32]byte, expected uint64, now time.Time) (Reminder, error) {
	return s.mutate(ctx, pid, actor, request, fp, expected, now, StateCancelled)
}
func (s *Store) Dismiss(ctx context.Context, pid, actor, request id.ID, fp [32]byte, expected uint64, now time.Time) (Reminder, error) {
	return s.mutate(ctx, pid, actor, request, fp, expected, now, StateDismissed)
}
func EncodeCursor(actor id.ID, r Reminder) string {
	return base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf("%s/%020d/%s", hx(actor), r.DueAt, hx(r.ID))))
}
func (s *Store) List(ctx context.Context, actor id.ID, dueOnly bool, cursor string, now time.Time) ([]Reminder, string, error) {
	if actor.IsZero() {
		return nil, "", ErrInvalid
	}
	var startDue uint64
	startID := ""
	if cursor != "" {
		raw, e := base64.RawURLEncoding.DecodeString(cursor)
		if e != nil {
			return nil, "", ErrInvalid
		}
		parts := strings.Split(string(raw), "/")
		if len(parts) != 3 || parts[0] != hx(actor) || len(parts[2]) != 32 {
			return nil, "", ErrInvalid
		}
		if _, e = fmt.Sscanf(parts[1], "%d", &startDue); e != nil {
			return nil, "", ErrInvalid
		}
		startID = parts[2]
	}
	uv, e := s.get(ctx, userKey(actor))
	if e != nil {
		return nil, "", e
	}
	if uv.Revision == 0 || uv.Tombstone {
		return []Reminder{}, "", nil
	}
	ids, e := decodeIDs(uv.Value)
	if e != nil {
		return nil, "", e
	}
	rows := make([]Reminder, 0, len(ids))
	for _, x := range ids {
		r, e := s.Get(ctx, x, actor)
		if e != nil {
			if errors.Is(e, ErrNotFound) {
				continue
			}
			return nil, "", e
		}
		if nowSeconds, ok := unixSeconds(now); r.State == StateScheduled && ok && nowSeconds >= r.DueAt {
			r.State = StateDue
		}
		if dueOnly && r.State != StateDue {
			continue
		}
		if !dueOnly && r.State != StateScheduled && r.State != StateDue {
			continue
		}
		rows = append(rows, r)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].DueAt == rows[j].DueAt {
			return hx(rows[i].ID) < hx(rows[j].ID)
		}
		return rows[i].DueAt < rows[j].DueAt
	})
	if startID != "" {
		i := 0
		for i < len(rows) && (rows[i].DueAt < startDue || (rows[i].DueAt == startDue && hx(rows[i].ID) <= startID)) {
			i++
		}
		rows = rows[i:]
	}
	limit := 20
	next := ""
	if len(rows) > limit {
		last := rows[limit-1]
		next = EncodeCursor(actor, last)
		rows = rows[:limit]
	}
	return rows, next, nil
}

// Tick persists due transitions in bounded transactions. List derives due state
// from the authoritative clock too, so a delayed scheduler cannot strand work.
func (s *Store) Tick(ctx context.Context) error {
	now := time.Now().UTC()
	if s.Now != nil {
		now = s.Now().UTC()
	}
	after := []byte(reminderPrefix)
	for {
		p, e := s.state.StateScan(ctx, stateArea, after, 100)
		if e != nil {
			return e
		}
		changes := make([]native.StateChange, 0, maxTx)
		for _, entry := range p.Entries {
			if !strings.HasPrefix(string(entry.Key), reminderPrefix) {
				break
			}
			if entry.Tombstone || entry.Revision == 0 {
				continue
			}
			r, e := decodeReminder(entry.Value)
			if e != nil {
				return e
			}
			if nowSeconds, ok := unixSeconds(now); r.State == StateScheduled && ok && nowSeconds >= r.DueAt {
				r.State = StateDue
				r.Revision++
				b, e := encodeReminder(r)
				if e != nil {
					return e
				}
				changes = append(changes, native.StateChange{Area: 8, Key: entry.Key, ExpectedRevision: entry.Revision, Value: b})
				if len(changes) == maxTx {
					if _, e = s.transact(ctx, id.New(), changes); e != nil {
						return e
					}
					changes = changes[:0]
				}
			}
		}
		if len(changes) > 0 {
			if _, e = s.transact(ctx, id.New(), changes); e != nil {
				return e
			}
		}
		if len(p.Entries) == 0 || len(p.Entries) < 100 || (len(p.Entries) > 0 && !strings.HasPrefix(string(p.Entries[len(p.Entries)-1].Key), reminderPrefix)) {
			return nil
		}
		if len(p.Next) > 0 {
			after = p.Next
		} else {
			return nil
		}
	}
}
