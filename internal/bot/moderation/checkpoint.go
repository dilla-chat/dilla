package moderation

import (
	"crypto/sha256"
	"errors"
	"sort"
	"time"

	"github.com/jonasthim/dilla/internal/cborx"
)

// MaxCheckpointBytes bounds each of 256 fixed actor checkpoint shards. A transaction writes only shards
// touched by evaluation (including eviction), never message bodies. The byte
// bound is the native store's per-value bound; overflow must defer before ACK.
const MaxCheckpointBytes = 256 << 10

var ErrCheckpoint = errors.New("invalid or full moderation checkpoint")

type checkpointBody struct {
	_    struct{} `cbor:",toarray"`
	At   int64
	Hash []byte
}
type checkpointUser struct {
	_               struct{} `cbor:",toarray"`
	Community, User string
	Messages        []int64
	Bodies          []checkpointBody
	LastSeen        int64
}
type checkpointDedup struct {
	_                       struct{} `cbor:",toarray"`
	Community, User, Source string
	Revision, Order         uint64
	Received                int64
}
type checkpointData struct {
	_       struct{} `cbor:",toarray"`
	Version uint64
	Through int64
	Users   []checkpointUser
	Dedup   []checkpointDedup
}

func checkpointShard(k userKey) byte {
	sum := sha256.Sum256([]byte(k.community + "/" + k.user))
	return sum[0]
}

// Checkpoint returns dirty shards; callers clear them only after their atomic
// checkpoint/frontier/pending transaction is durably acknowledged.
func (e *Engine) Checkpoint() (map[byte][]byte, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	shards := make(map[byte]*checkpointData, len(e.dirty))
	for shard := range e.dirty {
		shards[shard] = &checkpointData{Version: 1, Through: e.checkpointTime.UnixNano(), Users: []checkpointUser{}, Dedup: []checkpointDedup{}}
	}
	for key, state := range e.users {
		row := shards[checkpointShard(key)]
		if row == nil {
			continue
		}
		u := checkpointUser{Community: key.community, User: key.user, Messages: []int64{}, Bodies: []checkpointBody{}, LastSeen: state.lastSeen.UnixNano()}
		for _, at := range state.messages {
			if !at.After(e.checkpointTime.Add(-RateWindow)) {
				continue
			}
			u.Messages = append(u.Messages, at.UnixNano())
		}
		for _, seen := range state.bodies {
			if !seen.at.After(e.checkpointTime.Add(-DuplicateWindow)) {
				continue
			}
			u.Bodies = append(u.Bodies, checkpointBody{At: seen.at.UnixNano(), Hash: append([]byte(nil), seen.hash[:]...)})
		}
		row.Users = append(row.Users, u)
	}
	for _, key := range e.dedupFIFO {
		if e.dedupReceived[key].Before(e.checkpointTime.Add(-DuplicateWindow)) {
			continue
		}
		owner := e.dedupOwners[key]
		row := shards[checkpointShard(owner)]
		if row == nil {
			continue
		}
		row.Dedup = append(row.Dedup, checkpointDedup{Community: key.community, User: owner.user, Source: key.source, Revision: key.revision, Order: e.dedupOrder[key], Received: e.dedupReceived[key].UnixNano()})
	}
	out := make(map[byte][]byte, len(shards))
	for shard, row := range shards {
		sort.Slice(row.Users, func(i, j int) bool {
			a, b := row.Users[i], row.Users[j]
			if a.Community != b.Community {
				return a.Community < b.Community
			}
			return a.User < b.User
		})
		raw, err := cborx.Marshal(row)
		if err != nil || len(raw) > MaxCheckpointBytes {
			return nil, ErrCheckpoint
		}
		out[shard] = raw
	}
	return out, nil
}
func (e *Engine) CommitCheckpoint() { e.mu.Lock(); defer e.mu.Unlock(); clear(e.dirty) }

// RestoreCheckpoint replaces observations only after every shard is validated.
// Config remains the current policy; old checkpoint state never grants authority.
func (e *Engine) RestoreCheckpoint(shards map[byte][]byte) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	users := make(map[userKey]*userState)
	dedup := make(map[dedupKey]struct{})
	owners := make(map[dedupKey]userKey)
	orders := make(map[dedupKey]uint64)
	received := make(map[dedupKey]time.Time)
	var through time.Time
	fifo := []dedupKey{}
	var order uint64
	for shard, raw := range shards {
		var row checkpointData
		if len(raw) > MaxCheckpointBytes || cborx.Unmarshal(raw, &row) != nil || row.Version != 1 || len(row.Users) > e.stateCap || len(row.Dedup) > e.dedupCap {
			return ErrCheckpoint
		}
		if at := time.Unix(0, row.Through); at.After(through) {
			through = at
		}
		for _, u := range row.Users {
			key := userKey{community: u.Community, user: u.User}
			if !validCheckpointID(u.Community) || !validCheckpointID(u.User) || checkpointShard(key) != shard || len(u.Messages) > 256 || len(u.Bodies) > 512 || users[key] != nil {
				return ErrCheckpoint
			}
			state := &userState{lastSeen: time.Unix(0, u.LastSeen)}
			for _, at := range u.Messages {
				state.messages = append(state.messages, time.Unix(0, at))
			}
			for _, b := range u.Bodies {
				if len(b.Hash) != 32 {
					return ErrCheckpoint
				}
				var hash [32]byte
				copy(hash[:], b.Hash)
				state.bodies = append(state.bodies, observation{at: time.Unix(0, b.At), hash: hash})
			}
			users[key] = state
		}
		for _, d := range row.Dedup {
			key := dedupKey{community: d.Community, source: d.Source, revision: d.Revision}
			owner := userKey{community: d.Community, user: d.User}
			_, exists := dedup[key]
			if !validCheckpointID(d.Community) || !validCheckpointID(d.User) || !validCheckpointID(d.Source) || d.Revision == 0 || d.Order == 0 || d.Order == ^uint64(0) || exists || checkpointShard(owner) != shard {
				return ErrCheckpoint
			}
			dedup[key] = struct{}{}
			owners[key] = owner
			orders[key] = d.Order
			received[key] = time.Unix(0, d.Received)
			fifo = append(fifo, key)
			order = max(order, d.Order)
		}
	}
	if len(users) > e.stateCap || len(dedup) > e.dedupCap {
		return ErrCheckpoint
	}
	sort.Slice(fifo, func(i, j int) bool { return orders[fifo[i]] < orders[fifo[j]] })
	for i := 1; i < len(fifo); i++ {
		if orders[fifo[i]] == orders[fifo[i-1]] {
			return ErrCheckpoint
		}
	}
	e.users, e.dedup, e.dedupOwners, e.dedupOrder, e.dedupFIFO, e.checkpointOrder = users, dedup, owners, orders, fifo, order
	e.dedupReceived, e.checkpointTime = received, through
	clear(e.dirty)
	return nil
}
func validCheckpointID(s string) bool { return len(s) > 0 && len(s) <= 128 }
