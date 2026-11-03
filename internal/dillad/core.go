package dillad

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/config"
	"github.com/dilla-chat/dilla/internal/ds"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

// CoreFileName is the wasm32-wasip1 build of dilla-core-wasi, looked for beside the dillad binary
// unless Options.CorePath names it. `dillad doctor`'s wasi leg reads the same conventional path.
const CoreFileName = "dilla_core_wasi.wasm"

// defaultCorePath is CoreFileName next to the running executable.
//
// The plan's step text reads `o.Config.MLS.CorePath`, but dilla.toml has no [mls] table (task 4's
// schema never grew one), so the path follows the convention `dillad doctor` already checks
// (deviation B35). Options.CorePath overrides it, which is how a test or a packager that keeps the
// artefact elsewhere points at it.
func defaultCorePath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("dillad: locate the dillad binary to find %s beside it: %w", CoreFileName, err)
	}
	return filepath.Join(filepath.Dir(exe), CoreFileName), nil
}

// keyHistoryEntry is one entry of protocol/03-identity.md § Instance keys' key_history:
// [kind, key_id, public, secret, created, retired|null].
type keyHistoryEntry struct {
	_       struct{} `cbor:",toarray"`
	Kind    uint64
	KeyID   []byte
	Public  []byte
	Secret  []byte
	Created uint64
	Retired *uint64
}

type keyHistory struct {
	_       struct{} `cbor:",toarray"`
	V       uint64
	Entries []keyHistoryEntry
}

// instanceKeys reads the two current instance secrets out of instances.key_history: the
// external-sender Ed25519 seed (kind 0) and K_frank (kind 1), each the entry the instance row
// names by key id. `dillad init` writes both; an instance without them cannot sign an external
// proposal or frank a message, so New refuses to start rather than run a delivery service that
// fails on its first proposal.
func instanceKeys(row store.InstanceRow) (ds.InstanceKeys, error) {
	var h keyHistory
	if err := cborx.Unmarshal(row.KeyHistory, &h); err != nil {
		return ds.InstanceKeys{}, fmt.Errorf("dillad: instances.key_history does not decode: %w", err)
	}
	if h.V != 1 {
		return ds.InstanceKeys{}, fmt.Errorf("dillad: instances.key_history is version %d, want 1", h.V)
	}
	k := ds.InstanceKeys{
		InstanceID:          row.InstanceID,
		ExternalSenderKeyID: row.ExternalSenderKeyID,
		FrankingKeyID:       row.FrankingKeyID,
	}
	var haveSender, haveFranking bool
	for _, e := range h.Entries {
		if len(e.KeyID) != len(id.ID{}) || e.Retired != nil {
			continue
		}
		keyID := id.ID(e.KeyID)
		switch {
		case e.Kind == 0 && keyID == row.ExternalSenderKeyID && len(e.Secret) == 32:
			copy(k.ExternalSenderPriv[:], e.Secret)
			haveSender = true
		case e.Kind == 1 && keyID == row.FrankingKeyID && len(e.Secret) == 32:
			copy(k.FrankingKey[:], e.Secret)
			haveFranking = true
		}
	}
	if !haveSender || !haveFranking {
		return ds.InstanceKeys{}, errors.New("dillad: instances.key_history holds no current " +
			"external-sender key or franking key under the ids the instance row names; " +
			"`dillad init` writes both")
	}
	return k, nil
}

// frankingKeys is every K_frank the instance has held (kind 1 of key_history), the current one —
// the id the instance row names — first, then every other one, retired or not, in history order.
// The readable routes frank under the current key; the report route verifies under whichever key
// the stored tag names, so a key is never dropped from this list by a rotation (protocol/04
// "Franking": old keys are kept for verification). An entry without a 32-byte secret is not a key.
func frankingKeys(row store.InstanceRow) (*api.StaticFrankingKeys, error) {
	var h keyHistory
	if err := cborx.Unmarshal(row.KeyHistory, &h); err != nil {
		return nil, fmt.Errorf("dillad: instances.key_history does not decode: %w", err)
	}
	var current *api.FrankingKey
	var retained []api.FrankingKey
	for _, e := range h.Entries {
		if e.Kind != 1 || len(e.KeyID) != len(id.ID{}) || len(e.Secret) != 32 {
			continue
		}
		k := api.FrankingKey{ID: id.ID(e.KeyID), Key: append([]byte(nil), e.Secret...)}
		if k.ID == row.FrankingKeyID && e.Retired == nil && current == nil {
			current = &k
			continue
		}
		retained = append(retained, k)
	}
	if current == nil {
		return nil, errors.New("dillad: instances.key_history holds no current franking key " +
			"under the id the instance row names")
	}
	return api.NewStaticFrankingKeys(*current, retained...), nil
}

// policyFromConfig is ds.DefaultPolicy with the four values dilla.toml carries. internal/ds does not
// import internal/config (ds.Policy's own comment), so the composition root copies them across;
// every value the config does not carry keeps its protocol/02 default.
func policyFromConfig(c *config.Config) ds.Policy {
	p := ds.DefaultPolicy()
	if c.Retention.HandshakeDays > 0 {
		p.HandshakeRetention = time.Duration(c.Retention.HandshakeDays) * 24 * time.Hour
	}
	if c.Retention.CiphertextDays > 0 {
		p.MessageRetention = time.Duration(c.Retention.CiphertextDays) * 24 * time.Hour
	}
	if c.Limits.MaxCiphertextBytes > 0 {
		p.MaxCiphertextBytes = int(c.Limits.MaxCiphertextBytes)
	}
	if c.Limits.MaxKeypackagesPerDevice > 0 {
		p.MaxKeyPackagesPerDevice = c.Limits.MaxKeypackagesPerDevice
	}
	return p
}
