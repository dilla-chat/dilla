package service

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sync"

	"github.com/dilla-chat/dilla/internal/bot/client"
	"github.com/dilla-chat/dilla/internal/bot/native"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
)

const (
	memberRegistrationCatchupArea     = 9
	memberRegistrationCatchupKey      = "dilla.bot.member-registration-catchup.v1"
	maxMemberRegistrationCatchupPages = 2
)

type memberRegistrationCatchupNative interface {
	Binding() native.Binding
	StateGet(context.Context, uint64, []byte) (native.StateValue, error)
	StateTransaction(context.Context, id.ID, []native.StateChange) ([]uint64, error)
}

type memberRegistrationCatchupSession interface {
	Page(context.Context, id.ID) (client.CommunityMemberPage, error)
	ResolveCurrentEpisode(context.Context, id.ID) (id.ID, bool, error)
}

type memberRegistrationCatchupQueue interface {
	QueueMemberJoin(context.Context, id.ID, id.ID) error
}

type MemberRegistrationCatchupOptions struct{ PageBudget uint64 }
type MemberRegistrationCatchupResult struct {
	MembersVisited, Staged, Pages uint64
	More                          bool
}

type memberRegistrationCatchupCursor struct {
	After id.ID
	Done  bool
}

type MemberRegistrationCatchup struct {
	mu         sync.Mutex
	native     memberRegistrationCatchupNative
	binding    native.Binding
	pageBudget uint64
}

func NewMemberRegistrationCatchup(n *native.Client, options MemberRegistrationCatchupOptions) (*MemberRegistrationCatchup, error) {
	if n == nil {
		return nil, errors.New("missing native client")
	}
	return newMemberRegistrationCatchup(n, options)
}

func newMemberRegistrationCatchup(n memberRegistrationCatchupNative, options MemberRegistrationCatchupOptions) (*MemberRegistrationCatchup, error) {
	if n == nil {
		return nil, errors.New("missing native client")
	}
	binding := n.Binding()
	if binding.Instance.IsZero() || binding.Community.IsZero() || binding.Installation.IsZero() {
		return nil, errors.New("invalid member registration catch-up binding")
	}
	if options.PageBudget == 0 {
		options.PageBudget = maxMemberRegistrationCatchupPages
	}
	if options.PageBudget > maxMemberRegistrationCatchupPages {
		return nil, errors.New("member registration catch-up page budget exceeds bound")
	}
	return &MemberRegistrationCatchup{native: n, binding: binding, pageBudget: options.PageBudget}, nil
}

// StageOnce scans a fixed number of server pages and durably stages current
// human member episodes. It never reads or creates historical welcome data.
// Per-member cursor writes make a crash or a full registration queue resume at
// the first member that was not durably staged.
func (c *MemberRegistrationCatchup) StageOnce(ctx context.Context, session memberRegistrationCatchupSession, queue memberRegistrationCatchupQueue) (MemberRegistrationCatchupResult, error) {
	var result MemberRegistrationCatchupResult
	if c == nil || session == nil || queue == nil {
		return result, errors.New("invalid member registration catch-up")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return result, err
	}
	cursor, revision, err := c.loadCursor(ctx)
	if err != nil {
		return result, err
	}
	if cursor.Done {
		return result, nil
	}
	for pageNo := uint64(0); pageNo < c.pageBudget; pageNo++ {
		if err = ctx.Err(); err != nil {
			return result, err
		}
		page, pageErr := session.Page(ctx, cursor.After)
		if pageErr != nil {
			return result, pageErr
		}
		if len(page.Members) > client.MaxCommunityMemberPage {
			return result, client.ErrProtocol
		}
		result.Pages++
		last := cursor.After
		for _, member := range page.Members {
			if member.UserID.IsZero() || !idAfter(member.UserID, last) || member.Kind > 1 {
				return result, client.ErrProtocol
			}
			if err = ctx.Err(); err != nil {
				return result, err
			}
			if member.Kind == 0 {
				episode, present, resolveErr := session.ResolveCurrentEpisode(ctx, member.UserID)
				if resolveErr != nil {
					return result, fmt.Errorf("resolve current member episode: %w", resolveErr)
				}
				if present {
					if episode.IsZero() {
						return result, client.ErrProtocol
					}
					if err = queue.QueueMemberJoin(ctx, member.UserID, episode); err != nil {
						return result, err
					}
					result.Staged++
					// Close a leave/rejoin race that occurs between the first
					// authoritative lookup and the durable queue write.
					current, stillPresent, recheckErr := session.ResolveCurrentEpisode(ctx, member.UserID)
					if recheckErr != nil {
						return result, fmt.Errorf("recheck current member episode: %w", recheckErr)
					}
					if stillPresent {
						if current.IsZero() {
							return result, client.ErrProtocol
						}
						if current != episode {
							if err = queue.QueueMemberJoin(ctx, member.UserID, current); err != nil {
								return result, err
							}
							result.Staged++
						}
					}
				}
			}
			cursor.After = member.UserID
			revision, err = c.saveCursor(ctx, revision, cursor)
			if err != nil {
				return result, fmt.Errorf("persist member registration progress: %w", err)
			}
			last = member.UserID
			result.MembersVisited++
		}
		if len(page.Members) < client.MaxCommunityMemberPage {
			cursor.Done = true
			_, err = c.saveCursor(ctx, revision, cursor)
			if err != nil {
				return result, fmt.Errorf("persist member registration completion: %w", err)
			}
			return result, nil
		}
	}
	result.More = true
	return result, nil
}

func idAfter(a, b id.ID) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] > b[i]
		}
	}
	return false
}

func (c *MemberRegistrationCatchup) loadCursor(ctx context.Context) (memberRegistrationCatchupCursor, uint64, error) {
	v, err := c.native.StateGet(ctx, memberRegistrationCatchupArea, []byte(memberRegistrationCatchupKey))
	if err != nil {
		return memberRegistrationCatchupCursor{}, 0, err
	}
	if v.Tombstone {
		return memberRegistrationCatchupCursor{}, 0, errors.New("member registration catch-up cursor tombstoned")
	}
	if v.Revision == 0 {
		return memberRegistrationCatchupCursor{}, 0, nil
	}
	var a []any
	if cborx.Unmarshal(v.Value, &a) != nil || len(a) != 3 || a[0] != uint64(1) {
		return memberRegistrationCatchupCursor{}, 0, errors.New("invalid member registration catch-up cursor")
	}
	raw, ok := a[1].([]byte)
	if !ok || len(raw) != 16 {
		return memberRegistrationCatchupCursor{}, 0, errors.New("invalid member registration catch-up cursor")
	}
	doneFlag, ok := a[2].(uint64)
	if !ok || doneFlag > 1 {
		return memberRegistrationCatchupCursor{}, 0, errors.New("invalid member registration catch-up cursor")
	}
	done := doneFlag == 1
	var after id.ID
	copy(after[:], raw)
	if done && after.IsZero() { // An empty community is a valid completed scan.
		return memberRegistrationCatchupCursor{After: after, Done: true}, v.Revision, nil
	}
	return memberRegistrationCatchupCursor{After: after, Done: done}, v.Revision, nil
}

func (c *MemberRegistrationCatchup) saveCursor(ctx context.Context, revision uint64, cursor memberRegistrationCatchupCursor) (uint64, error) {
	if revision >= math.MaxInt64 {
		return revision, errors.New("member registration catch-up state revision exhausted")
	}
	done := uint64(0)
	if cursor.Done {
		done = 1
	}
	value, err := cborx.Marshal([]any{uint64(1), cursor.After, done})
	if err != nil {
		return revision, err
	}
	var rev [8]byte
	binary.BigEndian.PutUint64(rev[:], revision)
	seed := append([]byte("dilla.bot.member-registration-catchup.state.tx.v1\x00"), c.binding.Installation[:]...)
	seed = append(seed, rev[:]...)
	seed = append(seed, value...)
	hash := sha256.Sum256(seed)
	tx := id.ID(hash[:16])
	if tx.IsZero() {
		return revision, errors.New("invalid member registration catch-up transaction ID")
	}
	revisions, err := c.native.StateTransaction(ctx, tx, []native.StateChange{{Area: memberRegistrationCatchupArea, Key: []byte(memberRegistrationCatchupKey), ExpectedRevision: revision, Value: value}})
	if err != nil {
		return revision, err
	}
	if len(revisions) != 1 || revisions[0] != revision+1 {
		return revision, errors.New("invalid member registration catch-up state revision")
	}
	return revisions[0], nil
}
