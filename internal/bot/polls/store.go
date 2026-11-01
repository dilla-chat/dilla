// Package polls stores encrypted poll records and per-user vote rows in native state.
package polls

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
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
	stateArea           uint64 = 8
	pollPrefix                 = "dilla.bot.poll.v1/"
	votePrefix                 = "dilla.bot.poll-vote.v1/"
	receiptPrefix              = "dilla.bot.poll-receipt.v1/"
	maxStateValue              = 262144
	maxOpenPolls               = 128
	pageSize                   = 20
	expiryCloseAttempts        = 4
)

var (
	ErrConflict           = errors.New("poll state conflict")
	ErrCorruptVote        = errors.New("corrupt poll vote state")
	ErrNotFound           = errors.New("poll not found")
	ErrClosed             = errors.New("poll is closed")
	ErrMutationIDConflict = errors.New("poll request ID reused with different input")
	ErrQuota              = errors.New("open poll quota reached")
	ErrInvalid            = errors.New("invalid poll")
)

type State interface {
	StateGet(context.Context, uint64, []byte) (native.StateValue, error)
	StateScan(context.Context, uint64, []byte, uint64) (native.StatePage, error)
	StateTransaction(context.Context, id.ID, []native.StateChange) ([]uint64, error)
}
type Poll struct {
	ID, CommunityID, ChannelID id.ID
	ThreadID                   *id.ID
	CreatorID                  id.ID
	Question                   string
	Answers                    []string
	Multi                      bool
	CreatedAt, DueAt           uint64
	Closed                     bool
	Counts                     []uint64
	Revision                   uint64
	Published                  bool
}
type Outcome struct {
	Poll   Poll
	Choice []uint64
	Status uint64
}
type Store struct {
	state     State
	community id.ID
}

func NewStore(s State, community id.ID) *Store { return &Store{state: s, community: community} }
func idhex(v id.ID) string                     { return fmt.Sprintf("%x", v[:]) }
func pollKey(v id.ID) []byte                   { return []byte(pollPrefix + idhex(v)) }
func voteKey(p, u id.ID) []byte                { return []byte(votePrefix + idhex(p) + "/" + idhex(u)) }
func receiptKey(a, r id.ID) []byte             { return []byte(receiptPrefix + idhex(a) + "/" + idhex(r)) }
func quotaKey(c id.ID) []byte                  { return []byte(pollPrefix + "open/" + idhex(c)) }
func (s *Store) get(ctx context.Context, key []byte) (native.StateValue, error) {
	return s.state.StateGet(ctx, stateArea, key)
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
func encodePoll(p Poll) ([]byte, error) {
	v := []any{uint64(1), p.ID, p.CommunityID, p.ChannelID, p.ThreadID, p.CreatorID, p.Question, p.Answers, boolU(p.Multi), p.CreatedAt, p.DueAt, boolU(p.Closed), p.Counts, p.Revision, boolU(p.Published)}
	b, e := cborx.Marshal(v)
	if len(b) > maxStateValue {
		return nil, ErrInvalid
	}
	return b, e
}
func boolU(v bool) uint64 {
	if v {
		return 1
	}
	return 0
}

func unixSeconds(now time.Time) (uint64, bool) {
	seconds := now.Unix()
	if seconds < 0 {
		return 0, false
	}
	return uint64(seconds), true
}
func decodePoll(b []byte) (Poll, error) {
	var a []any
	if e := cborx.Unmarshal(b, &a); e != nil || len(a) != 15 {
		return Poll{}, ErrInvalid
	}
	p := Poll{}
	u := func(i int) (uint64, bool) { v, ok := a[i].(uint64); return v, ok }
	idv := func(i int) (id.ID, bool) {
		if v, ok := a[i].(id.ID); ok {
			return v, true
		}
		raw, ok := a[i].([]byte)
		if !ok || len(raw) != 16 {
			return id.ID{}, false
		}
		var v id.ID
		copy(v[:], raw)
		return v, true
	}
	if v, ok := idv(1); ok {
		p.ID = v
	} else {
		return p, ErrInvalid
	}
	if p.CommunityID, _ = idv(2); p.CommunityID.IsZero() {
		return p, ErrInvalid
	}
	if p.ChannelID, _ = idv(3); p.ChannelID.IsZero() {
		return p, ErrInvalid
	}
	if a[4] != nil {
		v, ok := idv(4)
		if !ok {
			return p, ErrInvalid
		}
		p.ThreadID = &v
	}
	if p.CreatorID, _ = idv(5); p.CreatorID.IsZero() {
		return p, ErrInvalid
	}
	var ok bool
	if p.Question, ok = a[6].(string); !ok {
		return p, ErrInvalid
	}
	raw, ok := a[7].([]any)
	if !ok {
		return p, ErrInvalid
	}
	for _, x := range raw {
		v, ok := x.(string)
		if !ok {
			return p, ErrInvalid
		}
		p.Answers = append(p.Answers, v)
	}
	v, ok := u(8)
	if !ok || v > 1 {
		return p, ErrInvalid
	}
	p.Multi = v == 1
	p.CreatedAt, _ = u(9)
	v, ok = u(10)
	if !ok {
		return p, ErrInvalid
	}
	p.DueAt = v
	v, ok = u(11)
	if !ok || v > 1 {
		return p, ErrInvalid
	}
	p.Closed = v == 1
	p.Counts = []uint64{}
	raw, ok = a[12].([]any)
	if !ok || len(raw) != len(p.Answers) {
		return p, ErrInvalid
	}
	for _, x := range raw {
		n, ok := x.(uint64)
		if !ok {
			return p, ErrInvalid
		}
		p.Counts = append(p.Counts, n)
	}
	p.Revision, ok = u(13)
	if !ok || p.Revision == 0 {
		return p, ErrInvalid
	}
	v, ok = u(14)
	if !ok || v > 1 {
		return p, ErrInvalid
	}
	p.Published = v == 1
	return p, nil
}
func validatePoll(p Poll, community id.ID) error {
	if p.ID.IsZero() || p.CommunityID != community || p.ChannelID.IsZero() || p.CreatorID.IsZero() || len([]rune(p.Question)) < 1 || len([]rune(p.Question)) > 300 || len(p.Answers) < 2 || len(p.Answers) > 10 || p.DueAt <= p.CreatedAt || p.DueAt-p.CreatedAt > 604800 {
		return ErrInvalid
	}
	seen := map[string]bool{}
	for _, a := range p.Answers {
		if len([]rune(a)) < 1 || len([]rune(a)) > 100 || strings.TrimSpace(a) == "" || seen[a] {
			return ErrInvalid
		}
		seen[a] = true
	}
	if p.ThreadID != nil && p.ThreadID.IsZero() {
		return ErrInvalid
	}
	return nil
}
func decodeReceipt(b []byte) ([32]byte, Outcome, error) {
	var a []any
	if e := cborx.Unmarshal(b, &a); e != nil || len(a) != 4 {
		return [32]byte{}, Outcome{}, ErrInvalid
	}
	fp, ok := a[1].([]byte)
	if !ok || len(fp) != 32 {
		return [32]byte{}, Outcome{}, ErrInvalid
	}
	var out Outcome
	out.Status, _ = a[2].(uint64)
	raw, ok := a[3].([]byte)
	if !ok {
		return [32]byte{}, out, ErrInvalid
	}
	p, e := decodePoll(raw)
	if e != nil {
		return [32]byte{}, out, e
	}
	out.Poll = p
	copyChoices, ok := a[0].([]any)
	if ok {
		for _, x := range copyChoices {
			n, ok := x.(uint64)
			if !ok {
				return [32]byte{}, out, ErrInvalid
			}
			out.Choice = append(out.Choice, n)
		}
	}
	var f [32]byte
	copy(f[:], fp)
	return f, out, nil
}
func receiptBody(fp [32]byte, out Outcome) ([]byte, error) {
	p, e := encodePoll(out.Poll)
	if e != nil {
		return nil, e
	}
	return cborx.Marshal([]any{out.Choice, fp[:], out.Status, p})
}
func (s *Store) replay(ctx context.Context, actor, request id.ID, fp [32]byte) (Outcome, bool, error) {
	v, e := s.get(ctx, receiptKey(actor, request))
	if e != nil || v.Revision == 0 || v.Tombstone {
		return Outcome{}, false, e
	}
	old, out, e := decodeReceipt(v.Value)
	if e != nil {
		return Outcome{}, false, e
	}
	if old != fp {
		return Outcome{}, true, ErrMutationIDConflict
	}
	return out, true, nil
}
func (s *Store) Create(ctx context.Context, request id.ID, fp [32]byte, p Poll, now time.Time) (Poll, error) {
	if s == nil || s.state == nil || s.community.IsZero() || request.IsZero() {
		return Poll{}, ErrInvalid
	}
	if out, ok, e := s.replay(ctx, p.CreatorID, request, fp); ok {
		return out.Poll, e
	}
	if p.ID != request {
		return Poll{}, ErrInvalid
	}
	if e := validatePoll(p, s.community); e != nil {
		return Poll{}, e
	}
	p.Revision = 1
	p.Counts = make([]uint64, len(p.Answers))
	createdAt, ok := unixSeconds(now)
	if !ok {
		return Poll{}, ErrInvalid
	}
	p.CreatedAt = createdAt
	if p.DueAt <= p.CreatedAt || p.DueAt-p.CreatedAt > 604800 {
		return Poll{}, ErrInvalid
	}
	qv, e := s.get(ctx, quotaKey(s.community))
	if e != nil {
		return Poll{}, e
	}
	var open []id.ID
	if qv.Revision > 0 && !qv.Tombstone {
		open, e = decodeIDs(qv.Value)
		if e != nil {
			return Poll{}, e
		}
	}
	active := make([]id.ID, 0, len(open))
	for _, existingID := range open {
		if existingID == p.ID {
			return Poll{}, ErrConflict
		}
		existing, getErr := s.Get(ctx, existingID)
		if getErr != nil {
			if errors.Is(getErr, ErrNotFound) {
				continue
			}
			return Poll{}, getErr
		}
		if nowSeconds, ok := unixSeconds(now); !existing.Closed && (!ok || nowSeconds < existing.DueAt) {
			active = append(active, existingID)
		}
	}
	open = active
	if len(open) >= maxOpenPolls {
		return Poll{}, ErrQuota
	}
	open = append(open, p.ID)
	sort.Slice(open, func(i, j int) bool { return string(open[i][:]) < string(open[j][:]) })
	qb, e := cborx.Marshal(idAny(open))
	if e != nil {
		return Poll{}, e
	}
	pb, e := encodePoll(p)
	if e != nil {
		return Poll{}, e
	}
	out := Outcome{Poll: p}
	rb, e := receiptBody(fp, out)
	if e != nil {
		return Poll{}, e
	}
	rkey := receiptKey(p.CreatorID, request)
	rv, e := s.get(ctx, rkey)
	if e != nil {
		return Poll{}, e
	}
	revs, e := s.transact(ctx, request, []native.StateChange{{Area: 8, Key: quotaKey(s.community), ExpectedRevision: qv.Revision, Value: qb}, {Area: 8, Key: pollKey(p.ID), ExpectedRevision: 0, Value: pb}, {Area: 8, Key: rkey, ExpectedRevision: rv.Revision, Value: rb}})
	if e != nil {
		return Poll{}, e
	}
	if len(revs) != 3 {
		return Poll{}, ErrConflict
	}
	return p, nil
}
func idAny(ids []id.ID) []any {
	a := make([]any, len(ids))
	for i, v := range ids {
		a[i] = v
	}
	return a
}
func decodeIDs(b []byte) ([]id.ID, error) {
	var a []any
	if e := cborx.Unmarshal(b, &a); e != nil {
		return nil, e
	}
	r := make([]id.ID, 0, len(a))
	for _, x := range a {
		var v id.ID
		switch x := x.(type) {
		case id.ID:
			v = x
		case []byte:
			if len(x) != 16 {
				return nil, ErrInvalid
			}
			copy(v[:], x)
		default:
			return nil, ErrInvalid
		}
		r = append(r, v)
	}
	return r, nil
}
func (s *Store) Get(ctx context.Context, pid id.ID) (Poll, error) {
	if s == nil || s.state == nil {
		return Poll{}, ErrInvalid
	}
	v, e := s.get(ctx, pollKey(pid))
	if e != nil {
		return Poll{}, e
	}
	if v.Revision == 0 || v.Tombstone {
		return Poll{}, ErrNotFound
	}
	p, e := decodePoll(v.Value)
	if e == nil && p.CommunityID != s.community {
		return Poll{}, ErrNotFound
	}
	return p, e
}
func (s *Store) mutation(ctx context.Context, pid, actor, request id.ID, fp [32]byte, expected uint64, now time.Time, op func(Poll) (Poll, []uint64, error), voteSnapshot *native.StateValue) (Outcome, error) {
	if out, ok, e := s.replay(ctx, actor, request, fp); ok {
		return out, e
	}
	v, e := s.get(ctx, pollKey(pid))
	if e != nil {
		return Outcome{}, e
	}
	if v.Revision == 0 || v.Tombstone {
		return Outcome{}, ErrNotFound
	}
	p, e := decodePoll(v.Value)
	if e != nil {
		return Outcome{}, e
	}
	if p.CommunityID != s.community {
		return Outcome{}, ErrNotFound
	}
	if expected != p.Revision {
		return Outcome{}, ErrConflict
	}
	next, choice, e := op(p)
	if e != nil {
		return Outcome{}, e
	}
	next.Revision++
	pb, e := encodePoll(next)
	if e != nil {
		return Outcome{}, e
	}
	out := Outcome{Poll: next, Choice: choice}
	rb, e := receiptBody(fp, out)
	if e != nil {
		return Outcome{}, e
	}
	rkey := receiptKey(actor, request)
	rv, e := s.get(ctx, rkey)
	if e != nil {
		return Outcome{}, e
	}
	changes := []native.StateChange{{Area: 8, Key: pollKey(pid), ExpectedRevision: v.Revision, Value: pb}, {Area: 8, Key: rkey, ExpectedRevision: rv.Revision, Value: rb}}
	var voteChange *native.StateChange
	if choice != nil {
		if voteSnapshot == nil {
			return Outcome{}, ErrInvalid
		}
		key := voteKey(pid, actor)
		body, e := cborx.Marshal(choice)
		if e != nil {
			return Outcome{}, e
		}
		ch := native.StateChange{Area: 8, Key: key, ExpectedRevision: voteSnapshot.Revision, Value: body}
		voteChange = &ch
	}
	if voteChange != nil {
		changes = append(changes, *voteChange)
	}
	_, e = s.transact(ctx, request, changes)
	return out, e
}
func (s *Store) Vote(ctx context.Context, pid, actor, request id.ID, fp [32]byte, choices []uint64, expected uint64, now time.Time) (Outcome, error) {
	var voteSnapshot native.StateValue
	out, e := s.mutation(ctx, pid, actor, request, fp, expected, now, func(p Poll) (Poll, []uint64, error) {
		nowSeconds, ok := unixSeconds(now)
		if !ok {
			return p, nil, ErrInvalid
		}
		if p.Closed || nowSeconds >= p.DueAt {
			return p, nil, ErrClosed
		}
		seen := map[uint64]bool{}
		for _, i := range choices {
			if i >= uint64(len(p.Answers)) || seen[i] {
				return p, nil, ErrInvalid
			}
			seen[i] = true
		}
		if !p.Multi && len(choices) > 1 {
			return p, nil, ErrInvalid
		}
		v, readErr := s.get(ctx, voteKey(pid, actor))
		if readErr != nil {
			return p, nil, readErr
		}
		voteSnapshot = v
		var old []uint64
		if v.Revision > 0 && !v.Tombstone {
			var a []any
			if decodeErr := cborx.Unmarshal(v.Value, &a); decodeErr != nil {
				return p, nil, ErrCorruptVote
			}
			seenOld := make(map[uint64]bool, len(a))
			if len(a) > len(p.Answers) || (!p.Multi && len(a) > 1) {
				return p, nil, ErrCorruptVote
			}
			for _, item := range a {
				choice, ok := item.(uint64)
				if !ok || choice >= uint64(len(p.Answers)) || seenOld[choice] {
					return p, nil, ErrCorruptVote
				}
				seenOld[choice] = true
				old = append(old, choice)
			}
		}
		for _, i := range old {
			if i < uint64(len(p.Counts)) && p.Counts[i] > 0 {
				p.Counts[i]--
			}
		}
		for _, i := range choices {
			p.Counts[i]++
		}
		return p, append([]uint64{}, choices...), nil
	}, &voteSnapshot)
	return out, e
}
func (s *Store) Close(ctx context.Context, pid, actor, request id.ID, fp [32]byte, expected uint64, now time.Time, manage bool) (Poll, error) {
	if out, ok, e := s.replay(ctx, actor, request, fp); ok {
		return out.Poll, e
	}
	v, e := s.get(ctx, pollKey(pid))
	if e != nil {
		return Poll{}, e
	}
	if v.Revision == 0 || v.Tombstone {
		return Poll{}, ErrNotFound
	}
	p, e := decodePoll(v.Value)
	if e != nil {
		return Poll{}, e
	}
	if p.CommunityID != s.community {
		return Poll{}, ErrNotFound
	}
	if p.Revision != expected {
		return Poll{}, ErrConflict
	}
	if actor != p.CreatorID && !manage {
		return Poll{}, ErrForbidden
	}
	if p.Closed {
		return Poll{}, ErrClosed
	}
	p.Closed = true
	p.Revision++
	qkey := quotaKey(s.community)
	qv, e := s.get(ctx, qkey)
	if e != nil {
		return Poll{}, e
	}
	var open []id.ID
	if qv.Revision > 0 && !qv.Tombstone {
		open, e = decodeIDs(qv.Value)
		if e != nil {
			return Poll{}, e
		}
	}
	filtered := open[:0]
	for _, v := range open {
		if v != pid {
			filtered = append(filtered, v)
		}
	}
	qb, e := cborx.Marshal(idAny(filtered))
	if e != nil {
		return Poll{}, e
	}
	pb, e := encodePoll(p)
	if e != nil {
		return Poll{}, e
	}
	out := Outcome{Poll: p}
	rb, e := receiptBody(fp, out)
	if e != nil {
		return Poll{}, e
	}
	rkey := receiptKey(actor, request)
	rv, e := s.get(ctx, rkey)
	if e != nil {
		return Poll{}, e
	}
	_, e = s.transact(ctx, request, []native.StateChange{{Area: 8, Key: pollKey(pid), ExpectedRevision: v.Revision, Value: pb}, {Area: 8, Key: qkey, ExpectedRevision: qv.Revision, Value: qb}, {Area: 8, Key: rkey, ExpectedRevision: rv.Revision, Value: rb}})
	return p, e
}

var ErrForbidden = errors.New("poll forbidden")

func (s *Store) Counts(ctx context.Context, pid, actor id.ID) ([]uint64, []uint64, error) {
	p, e := s.Get(ctx, pid)
	if e != nil {
		return nil, nil, e
	}
	v, e := s.get(ctx, voteKey(pid, actor))
	if e != nil {
		return nil, nil, e
	}
	var choices []uint64
	if v.Revision > 0 && !v.Tombstone {
		if e = cborx.Unmarshal(v.Value, &choices); e != nil {
			return nil, nil, e
		}
	}
	return p.Counts, choices, nil
}
func (s *Store) Voters(ctx context.Context, pid id.ID, option uint64, after []byte, limit int) ([]id.ID, []byte, error) {
	if limit < 1 || limit > 100 {
		return nil, nil, ErrInvalid
	}
	if _, e := s.Get(ctx, pid); e != nil {
		return nil, nil, e
	}
	prefix := []byte(votePrefix + idhex(pid) + "/")
	cursor := after
	if cursor != nil && (len(cursor) != len(prefix)+32 || !strings.HasPrefix(string(cursor), string(prefix))) {
		return nil, nil, ErrInvalid
	}
	if cursor == nil {
		cursor = prefix
	}
	var ids []id.ID
	var next []byte
	for len(ids) < limit {
		page, e := s.state.StateScan(ctx, 8, cursor, 100)
		if e != nil {
			return nil, nil, e
		}
		if len(page.Entries) == 0 {
			break
		}
		stop := false
		for _, ent := range page.Entries {
			if !strings.HasPrefix(string(ent.Key), string(prefix)) {
				stop = true
				break
			}
			cursor = ent.Key
			if ent.Tombstone {
				continue
			}
			var choices []uint64
			if cborx.Unmarshal(ent.Value, &choices) != nil {
				continue
			}
			for _, v := range choices {
				if v == option {
					var user id.ID
					raw, e := hex.DecodeString(string(ent.Key[len(prefix):]))
					if e == nil && len(raw) == 16 {
						copy(user[:], raw)
						ids = append(ids, user)
					}
					break
				}
			}
			if len(ids) >= limit {
				next = append([]byte(nil), cursor...)
				break
			}
		}
		if stop || len(next) > 0 || len(page.Entries) < 100 {
			break
		}
	}
	return ids, next, nil
}
func readID(v any) (id.ID, bool) {
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
func (s *Store) List(ctx context.Context, channel id.ID, thread *id.ID, cursor string) ([]Poll, string, error) {
	if channel.IsZero() {
		return nil, "", ErrInvalid
	}
	after := []byte(pollPrefix)
	if cursor != "" {
		raw, e := base64.RawURLEncoding.DecodeString(cursor)
		if e != nil || len(raw) > 256 {
			return nil, "", ErrInvalid
		}
		var a []any
		if cborx.Unmarshal(raw, &a) != nil || len(a) != 5 || a[0] != uint64(1) {
			return nil, "", ErrInvalid
		}
		community, ok := readID(a[1])
		if !ok || community != s.community {
			return nil, "", ErrInvalid
		}
		cursorChannel, ok := readID(a[2])
		if !ok || cursorChannel != channel {
			return nil, "", ErrInvalid
		}
		var cursorThread *id.ID
		if a[3] != nil {
			v, ok := readID(a[3])
			if !ok {
				return nil, "", ErrInvalid
			}
			cursorThread = &v
		}
		if !sameThread(cursorThread, thread) {
			return nil, "", ErrInvalid
		}
		key, ok := a[4].([]byte)
		if !ok || len(key) != len(pollPrefix)+32 || !strings.HasPrefix(string(key), pollPrefix) {
			return nil, "", ErrInvalid
		}
		after = append([]byte(nil), key...)
	}
	rows := make([]Poll, 0, pageSize+1)
	var lastReturned []byte
	more := false
	scanAfter := after
	for len(rows) <= pageSize {
		page, e := s.state.StateScan(ctx, stateArea, scanAfter, 100)
		if e != nil {
			return nil, "", e
		}
		if len(page.Entries) == 0 {
			break
		}
		for _, ent := range page.Entries {
			if !strings.HasPrefix(string(ent.Key), pollPrefix) {
				return rows, "", nil
			}
			scanAfter = ent.Key
			if ent.Tombstone || !strings.HasPrefix(string(ent.Key), pollPrefix) || strings.Contains(string(ent.Key[len(pollPrefix):]), "/") {
				continue
			}
			p, e := decodePoll(ent.Value)
			if e != nil {
				return nil, "", e
			}
			if p.CommunityID == s.community && p.ChannelID == channel && sameThread(p.ThreadID, thread) {
				if len(rows) == pageSize {
					more = true
					break
				}
				rows = append(rows, p)
				lastReturned = append([]byte(nil), ent.Key...)
				if len(rows) > pageSize {
					more = true
					break
				}
			}
		}
		if more || len(page.Entries) < 100 {
			break
		}
	}
	if more {
		rows = rows[:pageSize]
		encoded, e := cborx.Marshal([]any{uint64(1), s.community, channel, thread, lastReturned})
		if e != nil {
			return nil, "", e
		}
		if len(encoded) > 256 {
			return nil, "", ErrInvalid
		}
		return rows, base64.RawURLEncoding.EncodeToString(encoded), nil
	}
	return rows, "", nil
}

// ListCursor returns an opaque cursor scoped to a list position. The poll must belong to that list.
func (s *Store) ListCursor(ctx context.Context, channel id.ID, thread *id.ID, afterPoll id.ID) (string, error) {
	p, err := s.Get(ctx, afterPoll)
	if err != nil {
		return "", err
	}
	if p.ChannelID != channel || !sameThread(p.ThreadID, thread) {
		return "", ErrInvalid
	}
	raw, err := cborx.Marshal([]any{uint64(1), s.community, channel, thread, pollKey(afterPoll)})
	if err != nil {
		return "", err
	}
	if len(raw) > 256 {
		return "", ErrInvalid
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func sameThread(a, b *id.ID) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// MarkPublished persists delivery state after the external publisher confirms its durable receipt.
func (s *Store) MarkPublished(ctx context.Context, pid id.ID) error {
	v, e := s.get(ctx, pollKey(pid))
	if e != nil {
		return e
	}
	if v.Revision == 0 || v.Tombstone {
		return ErrNotFound
	}
	p, e := decodePoll(v.Value)
	if e != nil {
		return e
	}
	if p.Published {
		return nil
	}
	p.Published = true
	b, e := encodePoll(p)
	if e != nil {
		return e
	}
	_, e = s.transact(ctx, id.New(), []native.StateChange{{Area: 8, Key: pollKey(pid), ExpectedRevision: v.Revision, Value: b}})
	return e
}

type Publisher interface {
	PublishPoll(context.Context, Poll) error
}

// Reconcile retries durable publication and expires polls using absolute UTC deadlines.
func (s *Store) Reconcile(ctx context.Context, now time.Time, publisher Publisher) error {
	if s == nil || s.state == nil || publisher == nil {
		return ErrInvalid
	}
	var reconcileErrors []error
	after := []byte(pollPrefix)
	for {
		page, e := s.state.StateScan(ctx, stateArea, after, 100)
		if e != nil {
			return errors.Join(append(reconcileErrors, e)...)
		}
		if len(page.Entries) == 0 {
			return errors.Join(reconcileErrors...)
		}
		for _, entry := range page.Entries {
			if err := ctx.Err(); err != nil {
				return errors.Join(append(reconcileErrors, err)...)
			}
			after = entry.Key
			key := string(entry.Key)
			if !strings.HasPrefix(key, pollPrefix) || strings.Contains(key[len(pollPrefix):], "/") {
				return errors.Join(reconcileErrors...)
			}
			if entry.Tombstone {
				continue
			}
			p, e := decodePoll(entry.Value)
			if e != nil {
				reconcileErrors = append(reconcileErrors, fmt.Errorf("decode poll state: %w", e))
				continue
			}
			if p.CommunityID != s.community {
				continue
			}
			if !p.Published {
				if e = publisher.PublishPoll(ctx, p); e != nil {
					reconcileErrors = append(reconcileErrors, fmt.Errorf("publish poll %s: %w", idhex(p.ID), e))
				} else if e = s.MarkPublished(ctx, p.ID); e != nil {
					reconcileErrors = append(reconcileErrors, fmt.Errorf("mark poll %s published: %w", idhex(p.ID), e))
				} else {
					p.Published = true
				}
			}
			if nowSeconds, ok := unixSeconds(now); !p.Closed && ok && nowSeconds >= p.DueAt {
				if e = s.expire(ctx, p.ID, now); e != nil {
					reconcileErrors = append(reconcileErrors, fmt.Errorf("expire poll %s: %w", idhex(p.ID), e))
				}
			}
		}
		if len(page.Entries) < 100 {
			return errors.Join(reconcileErrors...)
		}
	}
}

// expire reloads after a CAS loss and retries the poll+quota transaction as a
// unit. It never removes quota unless Close committed both rows atomically.
func (s *Store) expire(ctx context.Context, pid id.ID, now time.Time) error {
	for attempt := 0; attempt < expiryCloseAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		v, err := s.get(ctx, pollKey(pid))
		if err != nil {
			return err
		}
		if v.Revision == 0 || v.Tombstone {
			return ErrNotFound
		}
		p, err := decodePoll(v.Value)
		if err != nil {
			return err
		}
		if p.CommunityID != s.community {
			return ErrNotFound
		}
		if nowSeconds, ok := unixSeconds(now); p.Closed || !ok || nowSeconds < p.DueAt {
			return nil
		}
		var revision [8]byte
		binary.BigEndian.PutUint64(revision[:], p.Revision)
		fingerprint := sha256.Sum256(append(append([]byte(nil), p.ID[:]...), revision[:]...))
		_, err = s.Close(ctx, p.ID, p.CreatorID, id.New(), fingerprint, p.Revision, now, false)
		if err == nil || errors.Is(err, ErrClosed) {
			return nil
		}
		if !errors.Is(err, ErrConflict) {
			return err
		}
	}
	return ErrConflict
}
