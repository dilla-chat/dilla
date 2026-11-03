package ds_test

// keypackage_set_test.go reads the committed directory KeyPackage set
// (testkit/fixtures/key-packages, `dilla-testkit gen-key-packages`): one real KeyPackage per
// device, each built by its own device of its own user under that device's own signing key.
//
// The delivery service binds a directory KeyPackage to its device — the credential names the
// device and its user, the leaf is keyed by the device's registered key — so a test that registers
// a device and stores a KeyPackage for it registers the device AS the package's builder: the
// package's device id, its user and its leaf key. Anything else is a dishonest device, which the
// delivery service now refuses to propose, and a test that wants that shape builds it on purpose.

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/dilla-chat/dilla/internal/id"
)

const keyPackageSetDir = "../../testkit/fixtures/key-packages"

// keyPackageSetEntry is one manifest entry, decoded: the device, user and leaf key the package's
// own credential and leaf carry, and the package's bytes.
type keyPackageSetEntry struct {
	device id.ID
	user   id.ID
	dsk    []byte
	blob   []byte
}

var (
	keyPackageSetOnce sync.Once
	keyPackageSet     []keyPackageSetEntry
	keyPackageSetErr  error
)

func loadKeyPackageSet() ([]keyPackageSetEntry, error) {
	raw, err := os.ReadFile(filepath.Join(keyPackageSetDir, "manifest.json"))
	if err != nil {
		return nil, err
	}
	var manifest struct {
		KeyPackages []struct {
			Path     string `json:"path"`
			DeviceID string `json:"device_id_hex"`
			UserID   string `json:"user_id_hex"`
			DSKPub   string `json:"dsk_pub_hex"`
		} `json:"key_packages"`
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return nil, err
	}
	out := make([]keyPackageSetEntry, 0, len(manifest.KeyPackages))
	for _, e := range manifest.KeyPackages {
		var entry keyPackageSetEntry
		device, err := hex.DecodeString(e.DeviceID)
		if err != nil {
			return nil, fmt.Errorf("%s: device_id_hex: %w", e.Path, err)
		}
		user, err := hex.DecodeString(e.UserID)
		if err != nil {
			return nil, fmt.Errorf("%s: user_id_hex: %w", e.Path, err)
		}
		if len(device) != id.Size || len(user) != id.Size {
			return nil, fmt.Errorf("%s: device %d and user %d bytes, want %d each",
				e.Path, len(device), len(user), id.Size)
		}
		copy(entry.device[:], device)
		copy(entry.user[:], user)
		if entry.dsk, err = hex.DecodeString(e.DSKPub); err != nil {
			return nil, err
		}
		if entry.blob, err = os.ReadFile(filepath.Join(keyPackageSetDir, e.Path)); err != nil {
			return nil, err
		}
		out = append(out, entry)
	}
	return out, nil
}

// nextKeyPackageDevice is the next unused entry of the committed set for this harness.
func (h *dsHarness) nextKeyPackageDevice(t *testing.T) keyPackageSetEntry {
	t.Helper()
	keyPackageSetOnce.Do(func() { keyPackageSet, keyPackageSetErr = loadKeyPackageSet() })
	if keyPackageSetErr != nil {
		t.Fatalf("testkit/fixtures/key-packages: %v; regenerate it with\n"+
			"  cargo run -p dilla-testkit --bin dilla-testkit -- gen-key-packages --out testkit/fixtures/key-packages",
			keyPackageSetErr)
	}
	if h.keyPackagesUsed >= len(keyPackageSet) {
		t.Fatalf("the test registers more than the %d devices the committed KeyPackage set holds",
			len(keyPackageSet))
	}
	entry := keyPackageSet[h.keyPackagesUsed]
	h.keyPackagesUsed++
	return entry
}
