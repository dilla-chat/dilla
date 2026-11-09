package service

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/dilla-chat/dilla/internal/bot/client"
	"github.com/dilla-chat/dilla/internal/bot/native"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
)

const (
	defaultSourcePageLimit  = 256
	defaultSourcePageBudget = 2
)

var (
	ErrSourceNeedsResync = errors.New("source group needs resynchronization")
	ErrSourceGroupGone   = errors.New("source group is gone")
	ErrSourceSyncBusy    = errors.New("source group synchronization is already active")
)

type sourceSyncNative interface {
	Binding() native.Binding
	InspectGroup(context.Context, id.ID) (native.GroupInfo, error)
	ApplyGroup(context.Context, id.ID, []byte, []byte, uint64) (native.GroupApplyResult, error)
	GroupCursor(context.Context, id.ID) (*native.GroupCursor, error)
	AcknowledgeGroupCursor(context.Context, id.ID, native.GroupCursor) error
}

type sourceSyncSession interface {
	GroupInfo(context.Context, id.ID) (client.GroupInfo, error)
	GroupPage(context.Context, id.ID, string, uint64, uint64) (client.Page, error)
	AdvanceCursor(context.Context, id.ID, uint64, uint64) error
}

type sourceSyncOptions struct {
	PageLimit, PageBudget uint64
	now                   func() time.Time
}

// SourceSyncOptions bounds one serialized catch-up pass. Zero values use a 256-row page and two
// paired page reads. Each paired read requests one page from each append-only source.
type SourceSyncOptions struct{ PageLimit, PageBudget uint64 }

// SourceSyncResult reports durable source progress only. It contains no source bodies or plaintext.
type SourceSyncResult struct {
	AppliedThrough       uint64
	Handshakes, Messages uint64
	Pages                uint64
	More                 bool
}

type SourceSync struct {
	native   sourceSyncNative
	binding  native.Binding
	options  sourceSyncOptions
	activeMu sync.Mutex
	active   map[id.ID]bool
}

// NewSourceSync binds synchronization to one opened native identity. Independent groups may
// progress concurrently; overlapping passes for one group defer without waiting for network I/O.
func NewSourceSync(n *native.Client, options SourceSyncOptions) (*SourceSync, error) {
	if n == nil {
		return nil, errors.New("missing native client")
	}
	return newSourceSync(n, sourceSyncOptions{PageLimit: options.PageLimit, PageBudget: options.PageBudget})
}

func newSourceSync(n sourceSyncNative, options sourceSyncOptions) (*SourceSync, error) {
	if n == nil {
		return nil, errors.New("missing native client")
	}
	binding := n.Binding()
	if binding.Instance.IsZero() || binding.Community.IsZero() || binding.Installation.IsZero() {
		return nil, errors.New("invalid native binding")
	}
	if options.PageLimit == 0 {
		options.PageLimit = defaultSourcePageLimit
	}
	if options.PageBudget == 0 {
		options.PageBudget = defaultSourcePageBudget
	}
	if options.now == nil {
		options.now = time.Now
	}
	if options.PageLimit > defaultSourcePageLimit || options.PageBudget > 2 {
		return nil, errors.New("source sync budget exceeds bound")
	}
	return &SourceSync{native: n, binding: binding, options: options}, nil
}

// Sync performs one bounded pass for an active own-community text or call group.
// It does not create groups, commit membership, acknowledge authenticated events, or dispatch them.
func (s *SourceSync) Sync(ctx context.Context, session *client.Session, expected native.GroupExpected) (SourceSyncResult, error) {
	if session == nil {
		return SourceSyncResult{}, errors.New("missing device session")
	}
	return s.sync(ctx, session, expected)
}

func (s *SourceSync) sync(ctx context.Context, session sourceSyncSession, expected native.GroupExpected) (SourceSyncResult, error) {
	var out SourceSyncResult
	if err := ctx.Err(); err != nil {
		return out, err
	}
	if session == nil || expected.Group.IsZero() || expected.Community != s.binding.Community || expected.Community.IsZero() || expected.Channel.IsZero() ||
		(expected.Kind != 0 && expected.Kind != 1) {
		return out, errors.New("invalid source sync target")
	}
	release, claimed := s.claimGroup(expected.Group)
	if !claimed {
		return SourceSyncResult{More: true}, ErrSourceSyncBusy
	}
	defer release()
	local, err := s.native.InspectGroup(ctx, expected.Group)
	if err != nil {
		return out, err
	}
	if err = validateSourceGroup(local, s.binding, expected); err != nil {
		return out, err
	}
	remote, err := session.GroupInfo(ctx, expected.Group)
	if err != nil {
		return out, err
	}
	if remote.NextSequence == 0 || remote.NextSequence > math.MaxInt64 {
		return out, errors.New("invalid source head")
	}
	head := remote.NextSequence - 1
	if local.NextSequence > math.MaxInt64 {
		return out, errors.New("invalid native next sequence")
	}
	if local.NextSequence > remote.NextSequence {
		return out, errors.New("native source cursor is beyond current server head")
	}
	if err = s.flushCursor(ctx, session, expected, head); err != nil {
		return out, err
	}

	from := local.NextSequence
	for pages := uint64(0); pages < s.options.PageBudget && from <= head; pages++ {
		if err = ctx.Err(); err != nil {
			return out, err
		}
		hs, e := session.GroupPage(ctx, expected.Group, "handshakes", from, s.options.PageLimit)
		if e != nil {
			return out, e
		}
		if err = ctx.Err(); err != nil {
			return out, err
		}
		ms, e := session.GroupPage(ctx, expected.Group, "messages", from, s.options.PageLimit)
		if e != nil {
			return out, e
		}
		if err = validateSourcePage(hs, from, s.options.PageLimit, "handshakes"); err != nil {
			return out, err
		}
		if err = validateSourcePage(ms, from, s.options.PageLimit, "messages"); err != nil {
			return out, err
		}
		out.Pages++
		hsThrough, e := sourceFrontier(hs, from, head, s.options.PageLimit)
		if e != nil {
			return out, e
		}
		msThrough, e := sourceFrontier(ms, from, head, s.options.PageLimit)
		if e != nil {
			return out, e
		}
		through := minUint64(head, minUint64(hsThrough, msThrough))
		if through < from {
			return out, errors.New("source pages made no common progress")
		}
		hsRows, e := rowsThrough(hs.Rows, through)
		if e != nil {
			return out, e
		}
		msRows, e := rowsThrough(ms.Rows, through)
		if e != nil {
			return out, e
		}
		if err = rejectCrossStreamDuplicates(hsRows, msRows); err != nil {
			return out, err
		}
		hsBody, e := cborx.Marshal(hsRows)
		if e != nil {
			return out, e
		}
		msBody, e := cborx.Marshal(msRows)
		if e != nil {
			return out, e
		}
		if err = ctx.Err(); err != nil {
			return out, err
		}
		applied, applyErr := s.native.ApplyGroup(ctx, expected.Group, hsBody, msBody, through)
		if applyErr != nil {
			// The helper can commit before its response is lost. Re-read durable state, but leave
			// process replacement and retry to the installation supervisor.
			if _, readErr := s.native.InspectGroup(ctx, expected.Group); readErr != nil {
				return out, fmt.Errorf("apply result uncertain (%w); native state unavailable: %w", applyErr, readErr)
			}
			return out, applyErr
		}
		if applied.State == 3 {
			return out, ErrSourceNeedsResync
		}
		if applied.State == 4 {
			return out, ErrSourceGroupGone
		}
		if applied.State != 2 || applied.NextSequence == 0 || applied.NextSequence > math.MaxInt64 || applied.NextSequence <= from || applied.NextSequence > through+1 {
			return out, errors.New("native apply made impossible progress")
		}
		out.AppliedThrough = applied.NextSequence - 1
		out.Handshakes += uint64(len(hsRows))
		out.Messages += uint64(len(msRows))
		if err = s.flushCursor(ctx, session, expected, head); err != nil {
			return out, err
		}
		local, err = s.native.InspectGroup(ctx, expected.Group)
		if err != nil {
			return out, err
		}
		if err = validateSourceGroup(local, s.binding, expected); err != nil {
			return out, err
		}
		if local.NextSequence <= from {
			return out, errors.New("native apply cursor made no progress")
		}
		from = local.NextSequence
	}
	out.More = from <= head
	if out.AppliedThrough == 0 && local.NextSequence > 1 {
		out.AppliedThrough = local.NextSequence - 1
	}
	return out, nil
}

func (s *SourceSync) claimGroup(group id.ID) (func(), bool) {
	// Production claims belong to the opened native identity: a reconciler can
	// be rebuilt while an older command callback is still catching up a group.
	if shared, ok := s.native.(interface{ TryClaimSource(id.ID) (func(), bool) }); ok {
		return shared.TryClaimSource(group)
	}
	s.activeMu.Lock()
	defer s.activeMu.Unlock()
	if s.active[group] {
		return nil, false
	}
	if s.active == nil {
		s.active = make(map[id.ID]bool)
	}
	s.active[group] = true
	return func() {
		s.activeMu.Lock()
		delete(s.active, group)
		s.activeMu.Unlock()
	}, true
}

func validateSourceGroup(g native.GroupInfo, binding native.Binding, expected native.GroupExpected) error {
	if g.ID == expected.Group && g.State == 3 {
		return ErrSourceNeedsResync
	}
	if g.ID == expected.Group && g.State == 4 {
		return ErrSourceGroupGone
	}
	if g.ID != expected.Group || g.State != 2 || g.Kind != expected.Kind || g.Community == nil || *g.Community != binding.Community || *g.Community != expected.Community || g.Target != expected.Channel || g.PendingCommit {
		return errors.New("native group does not match active source sync target")
	}
	if g.NextSequence == 0 || g.NextSequence > math.MaxInt64 {
		return errors.New("invalid native group sequence")
	}
	return nil
}

func validateSourcePage(page client.Page, from, limit uint64, kind string) error {
	if uint64(len(page.Rows)) > limit {
		return fmt.Errorf("%s page exceeds requested limit", kind)
	}
	var previous uint64
	for i, row := range page.Rows {
		if len(row) == 0 {
			return fmt.Errorf("malformed %s row", kind)
		}
		seq, ok := row[0].(uint64)
		if !ok || seq < from || seq > math.MaxInt64 || (i > 0 && seq <= previous) {
			return fmt.Errorf("malformed %s sequence order", kind)
		}
		if kind == "handshakes" && len(row) != 5 || kind == "messages" && len(row) != 8 && len(row) != 9 {
			return fmt.Errorf("malformed %s row shape (%d fields)", kind, len(row))
		}
		previous = seq
	}
	if len(page.Rows) == 0 && page.Next != from {
		return fmt.Errorf("%s empty page has an impossible next cursor", kind)
	}
	if len(page.Rows) > 0 && (previous == math.MaxUint64 || page.Next != previous+1) {
		return fmt.Errorf("%s page has an impossible next cursor", kind)
	}
	if uint64(len(page.Rows)) == limit && (len(page.Rows) == 0 || previous < from || page.Next <= from) {
		return fmt.Errorf("%s page made no progress", kind)
	}
	return nil
}

func sourceFrontier(page client.Page, from, head, limit uint64) (uint64, error) {
	if uint64(len(page.Rows)) < limit {
		return head, nil
	}
	last, ok := page.Rows[len(page.Rows)-1][0].(uint64)
	if !ok {
		return 0, errors.New("malformed source sequence")
	}
	return last, nil
}

func rowsThrough(rows [][]any, through uint64) ([][]any, error) {
	end := 0
	for end < len(rows) {
		seq, ok := rows[end][0].(uint64)
		if !ok {
			return nil, errors.New("malformed source sequence")
		}
		if seq > through {
			break
		}
		end++
	}
	return rows[:end], nil
}

func rejectCrossStreamDuplicates(handshakes, messages [][]any) error {
	i, j := 0, 0
	for i < len(handshakes) && j < len(messages) {
		h, hok := handshakes[i][0].(uint64)
		m, mok := messages[j][0].(uint64)
		if !hok || !mok {
			return errors.New("malformed source sequence")
		}
		if h == m {
			return errors.New("duplicate source sequence across streams")
		}
		if h < m {
			i++
		} else {
			j++
		}
	}
	return nil
}

func minUint64(a, b uint64) uint64 {
	if a < b {
		return a
	}
	return b
}

func (s *SourceSync) flushCursor(ctx context.Context, session sourceSyncSession, expected native.GroupExpected, head uint64) error {
	cursor, err := s.native.GroupCursor(ctx, expected.Group)
	if err != nil {
		return err
	}
	if cursor == nil {
		return nil
	}
	if cursor.Sequence == 0 || cursor.Sequence > head || cursor.Sequence > math.MaxInt64 || cursor.Epoch > math.MaxInt64 {
		return errors.New("native group cursor is beyond source head")
	}
	if err = session.AdvanceCursor(ctx, expected.Group, cursor.Sequence, cursor.Epoch); err != nil {
		return err
	}
	return s.native.AcknowledgeGroupCursor(ctx, expected.Group, *cursor)
}
