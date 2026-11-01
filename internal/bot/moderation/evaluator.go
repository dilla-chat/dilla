// Package moderation evaluates automatic-moderation rules over authenticated,
// decrypted message events. It deliberately has no persistence or action hooks.
package moderation

import (
	"crypto/sha256"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	RateWindow      = 5 * time.Second
	DuplicateWindow = 15 * time.Second
	DefaultTimeout  = 10 * time.Minute
	MaxTimeout      = 28 * 24 * time.Hour
	ExcerptMaxBytes = 512
	defaultStateCap = 4096
	defaultDedupCap = 16384
)

type Rule string

const (
	RuleRate      Rule = "message_rate"
	RuleDuplicate Rule = "duplicate_body"
	RuleMentions  Rule = "mention_count"
	RuleManual    Rule = "manual_moderator"
)

type Action string

const (
	ActionLog     Action = "log"
	ActionRemove  Action = "remove"
	ActionWarn    Action = "warn"
	ActionTimeout Action = "timeout"
)

// Config is immutable after New. Each unspecified or invalid rule action is
// log-only. Automatic bans are intentionally not represented.
type Config struct {
	Enabled     bool
	Actions     map[Rule]Action
	ExemptUsers []string
	Timeout     time.Duration
	StateLimit  int
	DedupLimit  int
}

// Input must be constructed only after authentication and successful content
// decryption. ReceivedAt is the server receipt timestamp, shared across devices.
type Input struct {
	CommunityID string
	SourceID    string
	Revision    uint64
	UserID      string
	Bot         bool
	Owner       bool
	Admin       bool
	Edit        bool
	Replay      bool // trusted delivery classification; replay rebuilds state without punitive action
	ReceivedAt  time.Time
	Body        string
	MentionIDs  []string // explicit mention IDs supplied by the decrypted message
}

type Decision struct {
	Action            Action
	Rules             []Rule
	Timeout           time.Duration
	DuplicateDelivery bool
	// EnforcementRefused means a punitive decision could not be applied. It must
	// not authorize downstream actions merely because intake can advance.
	EnforcementRefused bool
	Ignored            bool
}

type observation struct {
	at   time.Time
	hash [32]byte
}

type userState struct {
	messages []time.Time
	bodies   []observation
	lastSeen time.Time
}

type dedupKey struct {
	community string
	source    string
	revision  uint64
}

type Engine struct {
	mu              sync.Mutex
	enabled         bool
	actions         map[Rule]Action
	exempt          map[string]struct{}
	timeout         time.Duration
	stateCap        int
	dedupCap        int
	users           map[userKey]*userState
	dedup           map[dedupKey]struct{}
	dedupFIFO       []dedupKey
	dedupOwners     map[dedupKey]userKey
	dedupOrder      map[dedupKey]uint64
	checkpointOrder uint64
	checkpointTime  time.Time
	dedupReceived   map[dedupKey]time.Time
	dirty           map[byte]bool
}

type userKey struct {
	community string
	user      string
}

func New(config Config) *Engine {
	stateCap := config.StateLimit
	if stateCap <= 0 || stateCap > defaultStateCap {
		stateCap = defaultStateCap
	}
	dedupCap := config.DedupLimit
	if dedupCap <= 0 || dedupCap > defaultDedupCap {
		dedupCap = defaultDedupCap
	}
	timeout := config.Timeout
	if timeout <= 0 || timeout > MaxTimeout {
		timeout = DefaultTimeout
	}
	actions := make(map[Rule]Action, len(config.Actions))
	for rule, action := range config.Actions {
		if validRule(rule) && validAction(action) {
			actions[rule] = action
		}
	}
	exempt := make(map[string]struct{}, len(config.ExemptUsers))
	for _, id := range config.ExemptUsers {
		if id != "" && len(id) <= 128 {
			exempt[id] = struct{}{}
		}
	}
	return &Engine{enabled: config.Enabled, actions: actions, exempt: exempt, timeout: timeout,
		stateCap: stateCap, dedupCap: dedupCap, users: make(map[userKey]*userState), dedup: make(map[dedupKey]struct{}),
		dedupOwners: make(map[dedupKey]userKey), dedupOrder: make(map[dedupKey]uint64), dedupReceived: make(map[dedupKey]time.Time), dirty: make(map[byte]bool)}
}

// Evaluate counts each source/revision once. now is the local processing time;
// receipt timestamps determine windows, and events older than the longest
// window are ignored so historical backlog cannot cause punishment.
func (e *Engine) Evaluate(in Input, now time.Time) Decision {
	d := Decision{Action: ActionLog}
	if in.CommunityID == "" || len(in.CommunityID) > 128 || in.SourceID == "" || in.UserID == "" || len(in.SourceID) > 128 || len(in.UserID) > 128 || in.Revision == 0 || in.ReceivedAt.IsZero() || in.ReceivedAt.After(now) {
		d.Ignored = true
		return d
	}
	if now.Sub(in.ReceivedAt) > DuplicateWindow {
		d.Ignored = true
		return d
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	key := dedupKey{community: in.CommunityID, source: in.SourceID, revision: in.Revision}
	if _, ok := e.dedup[key]; ok {
		d.DuplicateDelivery = true
		return d
	}
	if now.After(e.checkpointTime) {
		e.checkpointTime = now
	}
	e.remember(key, userKey{community: in.CommunityID, user: in.UserID}, in.ReceivedAt)
	if !e.enabled || in.Bot || in.Owner || in.Admin {
		return d
	}
	if _, ok := e.exempt[in.UserID]; ok {
		return d
	}

	user := userKey{community: in.CommunityID, user: in.UserID}
	state := e.users[user]
	if state == nil {
		if len(e.users) >= e.stateCap {
			e.evictOldestUser()
		}
		state = &userState{}
		e.users[user] = state
	}
	state.lastSeen = now
	cutoffRate := in.ReceivedAt.Add(-RateWindow)
	state.messages = keepTimes(state.messages, cutoffRate)
	cutoffDuplicate := in.ReceivedAt.Add(-DuplicateWindow)
	state.bodies = keepObservations(state.bodies, cutoffDuplicate)

	if !in.Edit {
		state.messages = append(state.messages, in.ReceivedAt)
		sort.Slice(state.messages, func(i, j int) bool { return state.messages[i].Before(state.messages[j]) })
		if len(state.messages) > 0 && len(state.messages) > 256 {
			state.messages = state.messages[len(state.messages)-256:]
		}
		if countTimesThrough(state.messages, in.ReceivedAt) > 5 {
			d.Rules = append(d.Rules, RuleRate)
		}
	}
	normalized := normalize(in.Body)
	if normalized != "" {
		hash := sha256.Sum256([]byte(normalized))
		identical := 0
		for _, seen := range state.bodies {
			if seen.hash == hash && !seen.at.After(in.ReceivedAt) {
				identical++
			}
		}
		state.bodies = append(state.bodies, observation{at: in.ReceivedAt, hash: hash})
		sort.Slice(state.bodies, func(i, j int) bool { return state.bodies[i].at.Before(state.bodies[j].at) })
		if len(state.bodies) > 512 {
			state.bodies = state.bodies[len(state.bodies)-512:]
		}
		if identical >= 2 {
			d.Rules = append(d.Rules, RuleDuplicate)
		}
	}
	if hasMoreThanFiveDistinctMentions(in.MentionIDs) {
		d.Rules = append(d.Rules, RuleMentions)
	}
	if !in.Replay {
		d.Action = e.actionFor(d.Rules)
	}
	if d.Action == ActionTimeout {
		d.Timeout = e.timeout
	}
	return d
}

// Excerpt returns a valid UTF-8 prefix no longer than 512 bytes. Callers own
// any retention or authorization policy for the returned plaintext.
func Excerpt(text string) string {
	text = strings.ToValidUTF8(text, "�")
	if len(text) <= ExcerptMaxBytes {
		return text
	}
	end := ExcerptMaxBytes
	for end > 0 && !utf8.ValidString(text[:end]) {
		end--
	}
	return text[:end]
}

func (e *Engine) actionFor(rules []Rule) Action {
	best := ActionLog
	for _, rule := range rules {
		action := e.actions[rule]
		if severity(action) > severity(best) {
			best = action
		}
	}
	return best
}

func (e *Engine) remember(key dedupKey, owner userKey, receivedAt time.Time) {
	e.dedupReceived[key] = receivedAt
	e.dedup[key] = struct{}{}
	e.checkpointOrder++
	e.dedupOwners[key], e.dedupOrder[key] = owner, e.checkpointOrder
	e.dirty[checkpointShard(owner)] = true
	e.dedupFIFO = append(e.dedupFIFO, key)
	if len(e.dedupFIFO) > e.dedupCap {
		old := e.dedupFIFO[0]
		e.dirty[checkpointShard(e.dedupOwners[old])] = true
		delete(e.dedupOwners, old)
		delete(e.dedupOrder, old)
		delete(e.dedupReceived, old)
		e.dedupFIFO = e.dedupFIFO[1:]
		delete(e.dedup, old)
	}
}

func (e *Engine) evictOldestUser() {
	var oldestID userKey
	haveOldest := false
	var oldest time.Time
	for id, state := range e.users {
		if !haveOldest || state.lastSeen.Before(oldest) {
			oldestID, oldest, haveOldest = id, state.lastSeen, true
		}
	}
	if haveOldest {
		e.dirty[checkpointShard(oldestID)] = true
		delete(e.users, oldestID)
	}
}

func keepTimes(values []time.Time, cutoff time.Time) []time.Time {
	out := values[:0]
	for _, at := range values {
		if at.After(cutoff) {
			out = append(out, at)
		}
	}
	return out
}

func keepObservations(values []observation, cutoff time.Time) []observation {
	out := values[:0]
	for _, item := range values {
		if item.at.After(cutoff) {
			out = append(out, item)
		}
	}
	return out
}

func countTimesThrough(values []time.Time, through time.Time) int {
	count := 0
	for _, at := range values {
		if !at.After(through) {
			count++
		}
	}
	return count
}

func normalize(body string) string {
	return strings.Join(strings.FieldsFunc(strings.ToLower(body), unicode.IsSpace), " ")
}

func hasMoreThanFiveDistinctMentions(ids []string) bool {
	seen := make(map[string]struct{}, 6)
	for _, id := range ids {
		if id != "" {
			seen[id] = struct{}{}
			if len(seen) > 5 {
				return true
			}
		}
	}
	return false
}

func validRule(rule Rule) bool {
	return rule == RuleRate || rule == RuleDuplicate || rule == RuleMentions
}
func validAction(action Action) bool {
	return action == ActionLog || action == ActionRemove || action == ActionWarn || action == ActionTimeout
}
func severity(action Action) int {
	switch action {
	case ActionTimeout:
		return 3
	case ActionRemove:
		return 2
	case ActionWarn:
		return 1
	default:
		return 0
	}
}
