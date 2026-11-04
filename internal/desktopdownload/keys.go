package desktopdownload

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
	"fmt"
)

// pinnedMetadataKeys are the desktop release metadata public keys, exactly as
// packages/desktop/release-trust.json (metadataPublicKeys) and
// packages/desktop/src/update-keys.ts pin them. TestPinnedKeysMatchTheDesktopApp
// fails when they differ, so a key rotation updates all three together.
var pinnedMetadataKeys = []string{
	"-----BEGIN PUBLIC KEY-----\nMCowBQYDK2VwAyEAPT530pTGlCCbjSNbXeajnERzU1k9JL2KjSYupHPZjrw=\n-----END PUBLIC KEY-----\n",
}

// PinnedKeys returns the pinned metadata keys. A malformed pin is a build defect.
func PinnedKeys() []ed25519.PublicKey {
	keys, err := ParseKeys(pinnedMetadataKeys)
	if err != nil {
		panic(err)
	}
	return keys
}

// ParseKeys reads SPKI PEM Ed25519 public keys.
func ParseKeys(pems []string) ([]ed25519.PublicKey, error) {
	keys := make([]ed25519.PublicKey, 0, len(pems))
	for _, p := range pems {
		block, rest := pem.Decode([]byte(p))
		if block == nil || block.Type != "PUBLIC KEY" || len(rest) != 0 {
			return nil, fmt.Errorf("desktop metadata key is not one PEM public key")
		}
		parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("desktop metadata key: %w", err)
		}
		key, ok := parsed.(ed25519.PublicKey)
		if !ok {
			return nil, fmt.Errorf("desktop metadata key is not Ed25519")
		}
		keys = append(keys, key)
	}
	return keys, nil
}
