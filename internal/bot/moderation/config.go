package moderation

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/dilla-chat/dilla/internal/bot/native"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
)

const (
	configStateArea      = uint64(8)
	configVersion        = uint64(1)
	MaxExemptUsers       = 256
	MaxExemptUserIDBytes = 128
	maxConfigStateBytes  = 16 << 10
)

var (
	ErrConfigState    = errors.New("invalid moderation configuration state")
	ErrConfigConflict = errors.New("moderation configuration revision conflict")
)

type configStateClient interface {
	Binding() native.Binding
	StateGet(context.Context, uint64, []byte) (native.StateValue, error)
	StateTransaction(context.Context, id.ID, []native.StateChange) ([]uint64, error)
}

// Store reads and writes installation-bound automatic moderation settings in
// the helper's encrypted application-state area.
type Store struct {
	native  configStateClient
	binding native.Binding
}

func NewStore(n configStateClient) *Store {
	if n == nil {
		return &Store{}
	}
	return &Store{native: n, binding: n.Binding()}
}

// Binding identifies the installation scope whose encrypted configuration this
// store reads and writes.
func (s *Store) Binding() native.Binding {
	if s == nil {
		return native.Binding{}
	}
	return s.binding
}

// Load returns evaluator-ready settings and the revision to use for the next
// Save. An absent row is safely disabled with every rule set to log-only.
func (s *Store) Load(ctx context.Context) (Config, uint64, error) {
	if s == nil || s.native == nil || !validConfigBinding(s.binding) {
		return Config{}, 0, ErrConfigState
	}
	v, err := s.native.StateGet(ctx, configStateArea, configKey(s.binding))
	if err != nil {
		return Config{}, 0, fmt.Errorf("%w: state read", ErrConfigState)
	}
	if v.Tombstone {
		return Config{}, v.Revision, ErrConfigState
	}
	if v.Revision == 0 {
		if len(v.Value) != 0 {
			return Config{}, 0, ErrConfigState
		}
		return defaultConfig(), 0, nil
	}
	if len(v.Value) == 0 || len(v.Value) > maxConfigStateBytes || cborx.ExpectMajor(v.Value, cborx.MajorArray) != nil {
		return Config{}, v.Revision, ErrConfigState
	}
	var row []any
	if cborx.Unmarshal(v.Value, &row) != nil || len(row) != 10 {
		return Config{}, v.Revision, ErrConfigState
	}
	version, vok := row[0].(uint64)
	instance, iok := configID(row[1])
	community, cok := configID(row[2])
	installation, inok := configID(row[3])
	if !vok || version != configVersion || !iok || !cok || !inok || instance != s.binding.Instance || community != s.binding.Community || installation != s.binding.Installation {
		return Config{}, v.Revision, ErrConfigState
	}
	enabled, ok := row[4].(uint64)
	if !ok || enabled > 1 {
		return Config{}, v.Revision, ErrConfigState
	}
	var cfg Config
	cfg.Enabled = enabled == 1
	cfg.Actions = make(map[Rule]Action, 3)
	for i, rule := range []Rule{RuleRate, RuleDuplicate, RuleMentions} {
		action, ok := row[5+i].(string)
		if !ok || !validAction(Action(action)) {
			return Config{}, v.Revision, ErrConfigState
		}
		cfg.Actions[rule] = Action(action)
	}
	timeout, ok := row[8].(uint64)
	if !ok || timeout == 0 || timeout > math.MaxInt64 || time.Duration(timeout) > MaxTimeout {
		return Config{}, v.Revision, ErrConfigState
	}
	cfg.Timeout = time.Duration(timeout)
	users, ok := row[9].([]any)
	if !ok || len(users) > MaxExemptUsers {
		return Config{}, v.Revision, ErrConfigState
	}
	cfg.ExemptUsers = make([]string, 0, len(users))
	for _, item := range users {
		user, ok := item.(string)
		if !ok || !validExemptUser(user) {
			return Config{}, v.Revision, ErrConfigState
		}
		cfg.ExemptUsers = append(cfg.ExemptUsers, user)
	}
	return cfg, v.Revision, nil
}

// Save validates and persists settings only when expectedRevision still
// matches the encrypted state row. Zero timeout is normalized to ten minutes.
func (s *Store) Save(ctx context.Context, cfg Config, expectedRevision uint64) error {
	return s.SaveWithID(ctx, id.New(), cfg, expectedRevision)
}

// SaveWithID persists settings with a stable transaction ID so exact
// interaction retries can recover the original encrypted-state receipt.
func (s *Store) SaveWithID(ctx context.Context, transactionID id.ID, cfg Config, expectedRevision uint64) error {
	if s == nil || s.native == nil || !validConfigBinding(s.binding) {
		return ErrConfigState
	}
	if transactionID.IsZero() || expectedRevision >= math.MaxInt64 {
		return ErrConfigState
	}
	cfg, err := normalizeConfig(cfg)
	if err != nil {
		return err
	}
	if cfg.Timeout < 0 {
		return ErrConfigState
	}
	users := make([]any, len(cfg.ExemptUsers))
	for i, user := range cfg.ExemptUsers {
		users[i] = user
	}
	var enabled uint64
	if cfg.Enabled {
		enabled = 1
	}
	body, err := cborx.Marshal([]any{configVersion, s.binding.Instance, s.binding.Community, s.binding.Installation, enabled,
		string(cfg.Actions[RuleRate]), string(cfg.Actions[RuleDuplicate]), string(cfg.Actions[RuleMentions]), uint64(cfg.Timeout), users})
	if err != nil || len(body) > maxConfigStateBytes {
		return ErrConfigState
	}
	key := configKey(s.binding)
	change := []native.StateChange{{Area: configStateArea, Key: key, ExpectedRevision: expectedRevision, Value: body}}
	revisions, err := s.native.StateTransaction(ctx, transactionID, change)
	if err != nil {
		// The helper may have committed but lost the response. Reusing this exact
		// transaction ID recovers that receipt without applying the change twice.
		revisions, err = s.native.StateTransaction(ctx, transactionID, change)
	}
	if err != nil {
		return ErrConfigConflict
	}
	if len(revisions) != 1 || revisions[0] != expectedRevision+1 {
		return ErrConfigConflict
	}
	return nil
}

func defaultConfig() Config {
	return Config{Actions: map[Rule]Action{RuleRate: ActionLog, RuleDuplicate: ActionLog, RuleMentions: ActionLog}, Timeout: DefaultTimeout}
}

func normalizeConfig(cfg Config) (Config, error) {
	out := defaultConfig()
	out.Enabled = cfg.Enabled
	if cfg.Timeout == 0 {
		cfg.Timeout = DefaultTimeout
	}
	if cfg.Timeout < 0 || cfg.Timeout > MaxTimeout {
		return Config{}, fmt.Errorf("invalid moderation timeout")
	}
	out.Timeout = cfg.Timeout
	for rule, action := range cfg.Actions {
		if !validRule(rule) || !validAction(action) {
			return Config{}, fmt.Errorf("invalid moderation rule action")
		}
		out.Actions[rule] = action
	}
	if len(cfg.ExemptUsers) > MaxExemptUsers {
		return Config{}, fmt.Errorf("too many moderation exemptions")
	}
	seen := make(map[string]struct{}, len(cfg.ExemptUsers))
	for _, user := range cfg.ExemptUsers {
		if !validExemptUser(user) {
			return Config{}, fmt.Errorf("invalid moderation exemption")
		}
		if _, ok := seen[user]; ok {
			return Config{}, fmt.Errorf("duplicate moderation exemption")
		}
		seen[user] = struct{}{}
		out.ExemptUsers = append(out.ExemptUsers, user)
	}
	return out, nil
}

func validExemptUser(user string) bool { return user != "" && len(user) <= MaxExemptUserIDBytes }

func validConfigBinding(b native.Binding) bool {
	return !b.Instance.IsZero() && !b.Community.IsZero() && !b.Installation.IsZero()
}

func configKey(b native.Binding) []byte {
	return []byte("dilla.bot.moderation.v1/" + b.Instance.String() + "/" + b.Community.String() + "/" + b.Installation.String())
}

func configID(v any) (id.ID, bool) {
	b, ok := v.([]byte)
	if !ok || len(b) != len(id.ID{}) {
		return id.ID{}, false
	}
	var out id.ID
	copy(out[:], b)
	return out, true
}
