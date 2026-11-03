package moderation

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/bot/native"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
)

type configNative struct {
	binding  native.Binding
	value    native.StateValue
	readErr  error
	conflict bool
	receipts map[id.ID][]uint64
}

func (n *configNative) Binding() native.Binding { return n.binding }
func (n *configNative) StateGet(_ context.Context, area uint64, key []byte) (native.StateValue, error) {
	if n.readErr != nil {
		return native.StateValue{}, n.readErr
	}
	if area != configStateArea || string(key) != string(configKey(n.binding)) {
		return native.StateValue{}, errors.New("unexpected state address")
	}
	return n.value, nil
}

func TestConfigStoreClassifiesStateReadFailureWithoutLeakingDetails(t *testing.T) {
	n := testConfigNative()
	n.readErr = errors.New("private native state detail")
	_, _, err := NewStore(n).Load(context.Background())
	if err == nil || !errors.Is(err, ErrConfigState) || !strings.Contains(err.Error(), "state read") || strings.Contains(err.Error(), n.readErr.Error()) {
		t.Fatalf("state read error = %v, want a stage label without the underlying detail", err)
	}
}

func TestConfigKeyFitsNativeStateAddress(t *testing.T) {
	binding := testConfigNative().binding
	want := "dilla.bot.moderation.v1/" + binding.Instance.String() + "/" + binding.Community.String() + "/" + binding.Installation.String()
	got := string(configKey(binding))
	if got != want || len(got) > 128 {
		t.Fatalf("config key = %q (%d bytes), want %q (at most 128 bytes)", got, len(got), want)
	}
}
func (n *configNative) StateTransaction(_ context.Context, tx id.ID, changes []native.StateChange) ([]uint64, error) {
	if revisions, ok := n.receipts[tx]; ok {
		return append([]uint64(nil), revisions...), nil
	}
	if n.conflict || len(changes) != 1 || changes[0].Area != configStateArea || string(changes[0].Key) != string(configKey(n.binding)) || changes[0].ExpectedRevision != n.value.Revision {
		return nil, errors.New("revision conflict")
	}
	n.value = native.StateValue{Revision: n.value.Revision + 1, Value: append([]byte(nil), changes[0].Value...)}
	revisions := []uint64{n.value.Revision}
	if n.receipts == nil {
		n.receipts = make(map[id.ID][]uint64)
	}
	n.receipts[tx] = append([]uint64(nil), revisions...)
	return revisions, nil
}

func testConfigNative() *configNative {
	return &configNative{binding: native.Binding{Instance: id.New(), Community: id.New(), Installation: id.New()}}
}

func TestConfigStoreMissingReturnsDisabledLogOnlyDefaults(t *testing.T) {
	n := testConfigNative()
	cfg, rev, err := NewStore(n).Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rev != 0 || cfg.Enabled || cfg.Timeout != DefaultTimeout {
		t.Fatalf("defaults/revision = %#v, %d", cfg, rev)
	}
	for _, rule := range []Rule{RuleRate, RuleDuplicate, RuleMentions} {
		if got := cfg.Actions[rule]; got != ActionLog {
			t.Errorf("action %s = %q", rule, got)
		}
	}
}

func TestConfigStoreSaveUsesRevisionCASAndRoundTrips(t *testing.T) {
	n := testConfigNative()
	s := NewStore(n)
	ctx := context.Background()
	cfg := Config{Enabled: true, Actions: map[Rule]Action{RuleRate: ActionWarn}, ExemptUsers: []string{"user-42"}, Timeout: 3 * time.Hour}
	if err := s.Save(ctx, cfg, 0); err != nil {
		t.Fatal(err)
	}
	got, rev, err := s.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rev != 1 || !got.Enabled || got.Actions[RuleRate] != ActionWarn || got.Actions[RuleDuplicate] != ActionLog || got.Timeout != cfg.Timeout || len(got.ExemptUsers) != 1 || got.ExemptUsers[0] != "user-42" {
		t.Fatalf("round trip = %#v rev=%d", got, rev)
	}
	if err := s.Save(ctx, cfg, 0); !errors.Is(err, ErrConfigConflict) {
		t.Fatalf("stale save error = %v", err)
	}
}

type unknownConfigTransactionOutcome struct {
	*configNative
	calls int
}

func (n *unknownConfigTransactionOutcome) StateTransaction(ctx context.Context, tx id.ID, changes []native.StateChange) ([]uint64, error) {
	n.calls++
	revisions, err := n.configNative.StateTransaction(ctx, tx, changes)
	if n.calls == 1 && err == nil {
		return nil, errors.New("state receipt response lost")
	}
	return revisions, err
}

func TestConfigStoreRetriesUnknownTransactionOutcomeWithSameID(t *testing.T) {
	base := testConfigNative()
	nativeState := &unknownConfigTransactionOutcome{configNative: base}
	store := NewStore(nativeState)
	transactionID := id.New()
	cfg := Config{Enabled: true, Actions: map[Rule]Action{RuleRate: ActionWarn}}
	if err := store.SaveWithID(context.Background(), transactionID, cfg, 0); err != nil {
		t.Fatalf("save after lost receipt = %v", err)
	}
	loaded, revision, err := store.Load(context.Background())
	if err != nil || revision != 1 || !loaded.Enabled || nativeState.calls != 2 {
		t.Fatalf("retry result config=%+v revision=%d calls=%d err=%v", loaded, revision, nativeState.calls, err)
	}
}

func TestConfigStoreRejectsInvalidConfig(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
	}{
		{"action", Config{Actions: map[Rule]Action{RuleRate: "ban"}}},
		{"timeout", Config{Timeout: MaxTimeout + time.Nanosecond}},
		{"exempt count", Config{ExemptUsers: make([]string, MaxExemptUsers+1)}},
		{"exempt id", Config{ExemptUsers: []string{string(make([]byte, MaxExemptUserIDBytes+1))}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := NewStore(testConfigNative()).Save(context.Background(), tc.cfg, 0); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestConfigStoreRejectsWrongBindingMalformedAndTombstone(t *testing.T) {
	n := testConfigNative()
	body, err := cborx.Marshal([]any{uint64(1), n.binding.Instance, n.binding.Community, id.New(), uint64(0), string(ActionLog), string(ActionLog), string(ActionLog), uint64(DefaultTimeout), []any{}})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		value native.StateValue
	}{
		{"binding", native.StateValue{Revision: 1, Value: body}},
		{"malformed", native.StateValue{Revision: 1, Value: []byte{0x81, 0x01}}},
		{"tombstone", native.StateValue{Revision: 1, Tombstone: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n.value = tc.value
			if _, _, err := NewStore(n).Load(context.Background()); err == nil {
				t.Fatal("expected state error")
			}
		})
	}
}
