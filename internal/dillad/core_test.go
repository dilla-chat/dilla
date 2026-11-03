package dillad

import (
	"bytes"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/config"
	"github.com/dilla-chat/dilla/internal/ds"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

// The four values dilla.toml carries reach the delivery service's policy, each in its own field —
// the two retention windows are given distinct values so a transposition is visible — and every
// other tunable keeps its protocol/02 default. Task 24 asked for the keypackage cap to be checked
// here in particular: the refusal and GET /v1/instance/limits must read one number.
func TestThePolicyCarriesTheConfiguredValues(t *testing.T) {
	c := config.Default()
	c.Retention.HandshakeDays = 14
	c.Retention.CiphertextDays = 90
	c.Limits.MaxCiphertextBytes = 65536
	c.Limits.MaxKeypackagesPerDevice = 48

	p := policyFromConfig(c)
	if p.HandshakeRetention != 14*24*time.Hour {
		t.Errorf("HandshakeRetention = %v, want 14 days", p.HandshakeRetention)
	}
	if p.MessageRetention != 90*24*time.Hour {
		t.Errorf("MessageRetention = %v, want 90 days", p.MessageRetention)
	}
	if p.MaxCiphertextBytes != 65536 {
		t.Errorf("MaxCiphertextBytes = %d, want 65536", p.MaxCiphertextBytes)
	}
	if p.MaxKeyPackagesPerDevice != 48 {
		t.Errorf("MaxKeyPackagesPerDevice = %d, want 48", p.MaxKeyPackagesPerDevice)
	}
	d := ds.DefaultPolicy()
	if p.Backoff != d.Backoff || p.WatchdogInterval != d.WatchdogInterval || p.HealWindow != d.HealWindow {
		t.Error("a tunable the config does not carry moved off its default")
	}

	// And the shipped defaults are the policy's own defaults, so an instance that sets nothing
	// runs protocol/02's numbers.
	if got := policyFromConfig(config.Default()); got != d {
		t.Errorf("the default config's policy %+v differs from ds.DefaultPolicy %+v", got, d)
	}
}

func keyHistoryFor(t *testing.T, entries ...[]any) []byte {
	t.Helper()
	list := make([]any, 0, len(entries))
	for _, e := range entries {
		list = append(list, e)
	}
	b, err := cborx.Marshal([]any{uint64(1), list})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return b
}

// The instance keys are the CURRENT entries the instance row names, and nothing else: a retired
// entry of the same kind, or an entry under another id, is not the key the DS signs with.
func TestTheInstanceKeysAreTheEntriesTheInstanceRowNames(t *testing.T) {
	sender, frank, old := id.New(), id.New(), id.New()
	retired := uint64(5)
	row := store.InstanceRow{
		InstanceID: id.New(), ExternalSenderKeyID: sender, FrankingKeyID: frank,
		KeyHistory: keyHistoryFor(t,
			[]any{uint64(0), old, make([]byte, 32), bytes.Repeat([]byte{0xee}, 32), uint64(1), retired},
			[]any{uint64(0), sender, make([]byte, 32), bytes.Repeat([]byte{0x01}, 32), uint64(2), nil},
			[]any{uint64(1), frank, []byte{}, bytes.Repeat([]byte{0x02}, 32), uint64(2), nil},
		),
	}
	k, err := instanceKeys(row)
	if err != nil {
		t.Fatalf("instanceKeys: %v", err)
	}
	if k.ExternalSenderPriv != [32]byte(bytes.Repeat([]byte{0x01}, 32)) {
		t.Errorf("ExternalSenderPriv = %x, want the current entry's seed", k.ExternalSenderPriv)
	}
	if k.FrankingKey != [32]byte(bytes.Repeat([]byte{0x02}, 32)) {
		t.Errorf("FrankingKey = %x, want the current K_frank", k.FrankingKey)
	}
	if k.InstanceID != row.InstanceID || k.ExternalSenderKeyID != sender || k.FrankingKeyID != frank {
		t.Error("the key ids are not the instance row's")
	}
}

// An instance whose history lacks either key, or does not decode, does not start: a delivery
// service without its external-sender key fails on its first proposal, and one without K_frank
// cannot frank.
func TestAnInstanceWithoutItsKeysDoesNotStart(t *testing.T) {
	sender, frank := id.New(), id.New()
	for name, history := range map[string][]byte{
		"undecodable": {0x01},
		"no franking key": keyHistoryFor(t,
			[]any{uint64(0), sender, make([]byte, 32), make([]byte, 32), uint64(1), nil}),
		"the sender key under another id": keyHistoryFor(t,
			[]any{uint64(0), id.New(), make([]byte, 32), make([]byte, 32), uint64(1), nil},
			[]any{uint64(1), frank, []byte{}, make([]byte, 32), uint64(1), nil}),
	} {
		_, err := instanceKeys(store.InstanceRow{
			ExternalSenderKeyID: sender, FrankingKeyID: frank, KeyHistory: history,
		})
		if err == nil {
			t.Errorf("%s: instanceKeys accepted it", name)
		}
	}
}

// The franking keys the readable and report routes hold are every kind-1 entry of the history,
// the current one (the id the instance row names) first and every retired one kept (Plan 2 task
// 17): a report against a message franked before a rotation must still find the key it was made
// under. Entries of another kind, or with no 32-byte secret, are not franking keys.
func TestTheFrankingKeysAreEveryKindOneEntryCurrentFirst(t *testing.T) {
	sender, current, older, oldest := id.New(), id.New(), id.New(), id.New()
	retired := uint64(5)
	row := store.InstanceRow{
		InstanceID: id.New(), ExternalSenderKeyID: sender, FrankingKeyID: current,
		KeyHistory: keyHistoryFor(t,
			[]any{uint64(1), oldest, []byte{}, bytes.Repeat([]byte{0x0a}, 32), uint64(1), retired},
			[]any{uint64(0), sender, make([]byte, 32), bytes.Repeat([]byte{0x01}, 32), uint64(2), nil},
			[]any{uint64(1), older, []byte{}, bytes.Repeat([]byte{0x0b}, 32), uint64(2), retired},
			[]any{uint64(1), current, []byte{}, bytes.Repeat([]byte{0x0c}, 32), uint64(3), nil},
			[]any{uint64(1), id.New(), []byte{}, []byte{0x0d}, uint64(3), nil},
		),
	}
	keys, err := frankingKeys(row)
	if err != nil {
		t.Fatalf("frankingKeys: %v", err)
	}
	gotID, gotKey := keys.Current()
	if gotID != current || !bytes.Equal(gotKey, bytes.Repeat([]byte{0x0c}, 32)) {
		t.Fatalf("Current() = %v %x, want the key the instance row names", gotID, gotKey)
	}
	all := keys.All()
	if len(all) != 3 || all[0].ID != current || all[1].ID != oldest || all[2].ID != older {
		t.Fatalf("All() = %v, want current, then the two retired keys in history order", all)
	}
	if k, ok := keys.ByID(oldest); !ok || !bytes.Equal(k, bytes.Repeat([]byte{0x0a}, 32)) {
		t.Fatalf("ByID(the oldest retired key) = %x %v", k, ok)
	}
	if _, ok := keys.ByID(sender); ok {
		t.Fatal("the external-sender key was offered as a franking key")
	}

	row.FrankingKeyID = id.New()
	if _, err := frankingKeys(row); err == nil {
		t.Fatal("a history without the current franking key was accepted")
	}
}
