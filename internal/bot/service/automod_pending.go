package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jonasthim/dilla/internal/bot/moderation"
	"github.com/jonasthim/dilla/internal/bot/native"
	"github.com/jonasthim/dilla/internal/cborx"
	"github.com/jonasthim/dilla/internal/id"
)

const MaxModerationPending = 1024
const automodStateArea = uint64(8)
const automodStatePrefix = "dilla.bot.automod-state.v1/"

var ErrModerationPendingCapacity = errors.New("automatic moderation pending capacity reached")
var ErrModerationCheckpoint = errors.New("automatic moderation checkpoint unavailable")

type pendingModeration struct {
	_                                         struct{} `cbor:",toarray"`
	Policy, Slot                              uint64
	Community, Group, Channel, Actor, Message string
	SourceRevision                            uint64
	Action                                    moderation.Action
	Rules                                     []moderation.Rule
	Timeout                                   int64
	Excerpt                                   string
	Received                                  int64
	RemovalProof                              []byte
}

func pendingFromAction(a ModerationAction, policy uint64) pendingModeration {
	return pendingModeration{Policy: policy, Community: a.Community, Group: a.Group, Channel: a.Channel, Actor: a.Actor, Message: a.Message,
		SourceRevision: a.SourceRevision, Action: a.Decision.Action, Rules: append([]moderation.Rule(nil), a.Decision.Rules...), Timeout: int64(a.Decision.Timeout), Excerpt: a.Excerpt, Received: a.ReceivedAt.Unix(), RemovalProof: append([]byte(nil), a.removalProof...)}
}
func (p pendingModeration) action() ModerationAction {
	return ModerationAction{Community: p.Community, Group: p.Group, Channel: p.Channel, Actor: p.Actor, Message: p.Message, SourceRevision: p.SourceRevision,
		Decision: moderation.Decision{Action: p.Action, Rules: append([]moderation.Rule(nil), p.Rules...), Timeout: time.Duration(p.Timeout)}, Excerpt: p.Excerpt, ReceivedAt: time.Unix(p.Received, 0), runtimeGenerated: true, removalProof: append([]byte(nil), p.RemovalProof...)}
}
func pendingStateKey(slot uint64) string { return fmt.Sprintf("%sp/%04x", automodStatePrefix, slot) }
func pendingKey(a ModerationAction) string {
	return automodStatePrefix + "p/" + a.Group + "/" + fmt.Sprintf("%016x", a.SourceRevision)
}

type automodMeta struct {
	_                                                      struct{} `cbor:",toarray"`
	Version                                                uint64
	Instance, Community, Installation                      id.ID
	Policy, ReplayThrough, ReplayReceivedThrough, Frontier uint64
}
type automodShard struct {
	_      struct{} `cbor:",toarray"`
	Policy uint64
	Data   []byte
}
type autoModerationStore struct {
	native  moderationCaseNative
	rows    map[string]native.StateValue
	pending map[string]pendingModeration
	shards  map[byte][]byte
	meta    automodMeta
}

func (s *autoModerationStore) load(ctx context.Context) error {
	rows := map[string]native.StateValue{}
	pending := map[string]pendingModeration{}
	shards := map[byte][]byte{}
	binding := s.native.Binding()
	meta := automodMeta{Version: 1, Instance: binding.Instance, Community: binding.Community, Installation: binding.Installation}
	after := []byte(automodStatePrefix)
	for pages := 0; ; pages++ {
		if pages > MaxModerationPending+257 {
			return ErrModerationCheckpoint
		}
		page, err := s.native.StateScan(ctx, automodStateArea, after, 100)
		if err != nil {
			return err
		}
		done := false
		for _, row := range page.Entries {
			key := string(row.Key)
			if !strings.HasPrefix(key, automodStatePrefix) {
				done = true
				break
			}
			if key <= string(after) || row.Revision == 0 {
				return ErrModerationCheckpoint
			}
			if len(rows) >= MaxModerationPending+257 {
				return ErrModerationCheckpoint
			}
			rows[key] = native.StateValue{Revision: row.Revision, Value: row.Value, Tombstone: row.Tombstone}
			if row.Tombstone {
				continue
			}
			if key == automodStatePrefix+"meta" {
				if cborx.Unmarshal(row.Value, &meta) != nil || meta.Version != 1 || meta.Instance != binding.Instance || meta.Community != binding.Community || meta.Installation != binding.Installation {
					return ErrModerationCheckpoint
				}
			} else if strings.HasPrefix(key, automodStatePrefix+"p/") {
				var p pendingModeration
				if cborx.Unmarshal(row.Value, &p) != nil || p.Community != binding.Community.String() || p.SourceRevision == 0 || p.Received <= 0 || len(p.Excerpt) > moderation.ExcerptMaxBytes || len(p.Rules) == 0 || len(p.Rules) > 3 || p.Slot >= MaxModerationPending || pendingStateKey(p.Slot) != key || len(p.RemovalProof) > 1024 || (p.Action != moderation.ActionRemove && len(p.RemovalProof) != 0) {
					return ErrModerationCheckpoint
				}
				if len(pending) >= MaxModerationPending {
					return ErrModerationPendingCapacity
				}
				if _, exists := pending[pendingKey(p.action())]; exists {
					return ErrModerationCheckpoint
				}
				pending[pendingKey(p.action())] = p
			}
		}
		if done || len(page.Next) == 0 {
			break
		}
		if bytes.Compare(page.Next, after) <= 0 {
			return ErrModerationCheckpoint
		}
		after = page.Next
	}
	// Shards are fixed keys, including tombstones, so they cannot grow with actors.
	for key, row := range rows {
		if row.Tombstone || !strings.HasPrefix(key, automodStatePrefix+"s/") {
			continue
		}
		var shard byte
		if _, err := fmt.Sscanf(strings.TrimPrefix(key, automodStatePrefix+"s/"), "%02x", &shard); err != nil {
			return ErrModerationCheckpoint
		}
		if key != fmt.Sprintf("%ss/%02x", automodStatePrefix, shard) {
			return ErrModerationCheckpoint
		}
		var data automodShard
		if cborx.Unmarshal(row.Value, &data) != nil {
			return ErrModerationCheckpoint
		}
		if data.Policy == meta.Policy {
			shards[shard] = data.Data
		}
	}
	s.rows, s.pending, s.shards, s.meta = rows, pending, shards, meta
	return nil
}
func (s *autoModerationStore) commit(ctx context.Context, values map[string][]byte) error {
	changes := make([]native.StateChange, 0, len(values))
	for key, value := range values {
		changes = append(changes, native.StateChange{Area: automodStateArea, Key: []byte(key), ExpectedRevision: s.rows[key].Revision, Value: value})
	}
	if len(changes) == 0 {
		return nil
	}
	if len(changes) > 64 {
		return ErrModerationCheckpoint
	}
	tx := id.New()
	_, err := s.native.StateTransaction(ctx, tx, changes)
	if err != nil {
		// Recover a lost mutation reply by replaying the identical native transaction.
		_, err = s.native.StateTransaction(ctx, tx, changes)
	}
	if err != nil {
		// A transport may lose both replies. Read every changed row before deciding
		// whether evaluation can advance; never infer success from only the frontier.
		for _, ch := range changes {
			actual, readErr := s.native.StateGet(ctx, automodStateArea, ch.Key)
			if readErr != nil || actual.Revision != ch.ExpectedRevision+1 || actual.Tombstone != (ch.Value == nil) || !bytes.Equal(actual.Value, ch.Value) {
				return ErrModerationCheckpoint
			}
		}
	}
	for _, ch := range changes {
		s.rows[string(ch.Key)] = native.StateValue{Revision: ch.ExpectedRevision + 1, Value: ch.Value, Tombstone: ch.Value == nil}
	}
	return nil
}
func (s *autoModerationStore) saveEvaluation(ctx context.Context, engine *moderation.Engine, meta automodMeta, pending *pendingModeration) error {
	shards, err := engine.Checkpoint()
	if err != nil {
		return err
	}
	values := map[string][]byte{}
	for shard, data := range shards {
		raw, encodeErr := cborx.Marshal(automodShard{Policy: meta.Policy, Data: data})
		if encodeErr != nil || len(raw) > moderation.MaxCheckpointBytes {
			return ErrModerationCheckpoint
		}
		values[fmt.Sprintf("%ss/%02x", automodStatePrefix, shard)] = raw
	}
	raw, err := cborx.Marshal(meta)
	if err != nil {
		return err
	}
	values[automodStatePrefix+"meta"] = raw
	if pending != nil {
		used := map[uint64]bool{}
		for _, p := range s.pending {
			used[p.Slot] = true
		}
		for pending.Slot = 0; pending.Slot < MaxModerationPending && used[pending.Slot]; pending.Slot++ {
		}
		if pending.Slot >= MaxModerationPending {
			return ErrModerationPendingCapacity
		}
		raw, err = cborx.Marshal(pending)
		if err != nil {
			return err
		}
		values[pendingStateKey(pending.Slot)] = raw
	}
	if err = s.commit(ctx, values); err != nil {
		return err
	}
	for shard, data := range shards {
		s.shards[shard] = data
	}
	s.meta = meta
	if pending != nil {
		s.pending[pendingKey(pending.action())] = *pending
	}
	engine.CommitCheckpoint()
	return nil
}
func (s *autoModerationStore) complete(ctx context.Context, key string) error {
	if err := s.commit(ctx, map[string][]byte{pendingStateKey(s.pending[key].Slot): nil}); err != nil {
		return err
	}
	delete(s.pending, key)
	return nil
}

// AttachDurableState is called before the runtime is published. Old installs
// capture one conservative migration boundary; subsequent restarts reuse it.
func (r *AutoModerationRuntime) AttachDurableState(ctx context.Context, n moderationCaseNative) error {
	r.handleMu.Lock()
	defer r.handleMu.Unlock()
	r.configMu.Lock()
	defer r.configMu.Unlock()
	s := &autoModerationStore{native: n}
	if err := s.load(ctx); err != nil {
		return err
	}
	if s.meta.Policy > r.revision {
		return ErrModerationCheckpoint
	}
	if s.meta.Policy == r.revision && s.rows[automodStatePrefix+"meta"].Revision != 0 {
		if err := r.engine.RestoreCheckpoint(s.shards); err != nil {
			return err
		}
		r.replayThrough, r.replayReceivedThrough = s.meta.ReplayThrough, s.meta.ReplayReceivedThrough
	} else {
		b := n.Binding()
		meta := automodMeta{Version: 1, Instance: b.Instance, Community: b.Community, Installation: b.Installation, Policy: r.revision,
			ReplayThrough: r.replayThrough, ReplayReceivedThrough: r.replayReceivedThrough}
		// Preserve the old pending decisions even when activation changed policy.
		if err := s.saveEvaluation(ctx, r.engine, meta, nil); err != nil {
			return err
		}
		s.shards = map[byte][]byte{}
	}
	r.state = s
	r.pending = s.pending
	return nil
}

// AutoModerationStateClient is the protected, installation-scoped native state API.
type AutoModerationStateClient = moderationCaseNative

func (r *AutoModerationRuntime) ReplayBoundary() (uint64, uint64) {
	r.configMu.RLock()
	defer r.configMu.RUnlock()
	return r.replayThrough, r.replayReceivedThrough
}
func (r *AutoModerationRuntime) restoreDurable(ctx context.Context) error {
	if err := r.state.load(ctx); err != nil {
		return err
	}
	shards := r.state.shards
	if r.state.meta.Policy != r.revision {
		shards = map[byte][]byte{}
	}
	if err := r.engine.RestoreCheckpoint(shards); err != nil {
		return err
	}
	r.pending = r.state.pending
	r.stateDirty = false
	return nil
}
