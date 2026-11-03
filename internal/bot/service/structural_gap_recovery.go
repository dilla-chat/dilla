package service

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"math"
	"sync"

	"github.com/dilla-chat/dilla/internal/bot/client"
	"github.com/dilla-chat/dilla/internal/bot/native"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
)

const (
	structuralGapRecoveryArea     = 9
	structuralGapRecoveryKey      = "dilla.bot.structural-gap-recovery.v1"
	maxStructuralGapRecoveryPages = 2
)

type structuralGapRecoveryState struct {
	HighWater uint64
	After     id.ID
	Done      bool
}
type StructuralGapRecoveryResult struct {
	HighWater, MembersVisited, Staged, Pages uint64
	More, Done                               bool
}
type StructuralGapRecovery struct {
	mu         sync.Mutex
	native     memberRegistrationCatchupNative
	binding    native.Binding
	pageBudget uint64
}

func NewStructuralGapRecovery(n *native.Client, pageBudget uint64) (*StructuralGapRecovery, error) {
	if n == nil {
		return nil, errors.New("missing native client")
	}
	return newStructuralGapRecovery(n, pageBudget)
}

func newStructuralGapRecovery(n memberRegistrationCatchupNative, pageBudget uint64) (*StructuralGapRecovery, error) {
	if n == nil {
		return nil, errors.New("missing native client")
	}
	b := n.Binding()
	if b.Instance.IsZero() || b.Community.IsZero() || b.Installation.IsZero() {
		return nil, errors.New("invalid structural gap recovery binding")
	}
	if pageBudget == 0 {
		pageBudget = maxStructuralGapRecoveryPages
	}
	if pageBudget > maxStructuralGapRecoveryPages {
		return nil, errors.New("structural gap recovery page budget exceeds bound")
	}
	return &StructuralGapRecovery{native: n, binding: b, pageBudget: pageBudget}, nil
}

// StageOnce scans current membership after an expired structural cursor. The
// persisted high-water is a fixed snapshot; later events remain pollable after
// the cursor is advanced. No historical welcome or join event is synthesized.
func (r *StructuralGapRecovery) StageOnce(ctx context.Context, session memberRegistrationCatchupSession, queue memberRegistrationCatchupQueue, floor, highWater uint64) (StructuralGapRecoveryResult, error) {
	var out StructuralGapRecoveryResult
	if r == nil || session == nil || queue == nil || floor > highWater || highWater > math.MaxInt64 {
		return out, client.ErrProtocol
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	state, revision, err := r.load(ctx)
	if err != nil {
		return out, err
	}
	// If retention moved beyond the prior snapshot, restart from the current
	// member set and take the newer high-water as the new recovery target.
	if revision == 0 || floor > state.HighWater {
		state = structuralGapRecoveryState{HighWater: highWater}
		revision, err = r.save(ctx, revision, state)
		if err != nil {
			return out, err
		}
	}
	out.HighWater = state.HighWater
	if state.Done {
		out.Done = true
		return out, nil
	}
	for pageNo := uint64(0); pageNo < r.pageBudget; pageNo++ {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		page, err := session.Page(ctx, state.After)
		if err != nil {
			return out, err
		}
		if len(page.Members) > client.MaxCommunityMemberPage {
			return out, client.ErrProtocol
		}
		out.Pages++
		last := state.After
		for _, member := range page.Members {
			if member.UserID.IsZero() || !idAfter(member.UserID, last) || member.Kind > 1 {
				return out, client.ErrProtocol
			}
			if member.Kind == 0 {
				episode, present, err := session.ResolveCurrentEpisode(ctx, member.UserID)
				if err != nil {
					return out, err
				}
				if present {
					if episode.IsZero() {
						return out, client.ErrProtocol
					}
					if err = queue.QueueMemberJoin(ctx, member.UserID, episode); err != nil {
						return out, err
					}
					out.Staged++
					current, stillPresent, err := session.ResolveCurrentEpisode(ctx, member.UserID)
					if err != nil {
						return out, err
					}
					if stillPresent {
						if current.IsZero() {
							return out, client.ErrProtocol
						}
						if current != episode {
							if err = queue.QueueMemberJoin(ctx, member.UserID, current); err != nil {
								return out, err
							}
							out.Staged++
						}
					}
				}
			}
			state.After = member.UserID
			revision, err = r.save(ctx, revision, state)
			if err != nil {
				return out, err
			}
			last = state.After
			out.MembersVisited++
		}
		if len(page.Members) < client.MaxCommunityMemberPage {
			state.Done = true
			_, err = r.save(ctx, revision, state)
			if err != nil {
				return out, err
			}
			out.Done = true
			return out, nil
		}
	}
	out.More = true
	return out, nil
}

func (r *StructuralGapRecovery) load(ctx context.Context) (structuralGapRecoveryState, uint64, error) {
	v, err := r.native.StateGet(ctx, structuralGapRecoveryArea, []byte(structuralGapRecoveryKey))
	if err != nil {
		return structuralGapRecoveryState{}, 0, err
	}
	if v.Tombstone {
		return structuralGapRecoveryState{}, 0, errors.New("structural gap recovery state tombstoned")
	}
	if v.Revision == 0 {
		return structuralGapRecoveryState{}, 0, nil
	}
	var a []any
	if cborx.Unmarshal(v.Value, &a) != nil || len(a) != 4 || a[0] != uint64(1) {
		return structuralGapRecoveryState{}, 0, errors.New("invalid structural gap recovery state")
	}
	h, ok1 := a[1].(uint64)
	raw, ok2 := a[2].([]byte)
	done, ok3 := a[3].(uint64)
	if !ok1 || !ok2 || len(raw) != 16 || !ok3 || h > math.MaxInt64 || done > 1 {
		return structuralGapRecoveryState{}, 0, errors.New("invalid structural gap recovery state")
	}
	var after id.ID
	copy(after[:], raw)
	return structuralGapRecoveryState{HighWater: h, After: after, Done: done == 1}, v.Revision, nil
}
func (r *StructuralGapRecovery) save(ctx context.Context, revision uint64, state structuralGapRecoveryState) (uint64, error) {
	if revision >= math.MaxInt64 {
		return revision, errors.New("structural gap recovery revision exhausted")
	}
	done := uint64(0)
	if state.Done {
		done = 1
	}
	value, err := cborx.Marshal([]any{uint64(1), state.HighWater, state.After, done})
	if err != nil {
		return revision, err
	}
	var rev [8]byte
	binary.BigEndian.PutUint64(rev[:], revision)
	seed := append([]byte("dilla.bot.structural-gap-recovery.tx.v1\x00"), r.binding.Installation[:]...)
	seed = append(seed, rev[:]...)
	seed = append(seed, value...)
	h := sha256.Sum256(seed)
	tx := id.ID(h[:16])
	if tx.IsZero() {
		return revision, errors.New("invalid structural gap recovery transaction ID")
	}
	revisions, err := r.native.StateTransaction(ctx, tx, []native.StateChange{{Area: structuralGapRecoveryArea, Key: []byte(structuralGapRecoveryKey), ExpectedRevision: revision, Value: value}})
	if err != nil {
		return revision, err
	}
	if len(revisions) != 1 || revisions[0] != revision+1 {
		return revision, errors.New("invalid structural gap recovery state revision")
	}
	return revisions[0], nil
}
