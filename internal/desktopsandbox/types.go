// Package desktopsandbox verifies the portable, offline Linux sandbox component
// contract. It performs no filesystem access, privilege changes, or networking.
package desktopsandbox

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"time"
)

const (
	MaxManifest          = 64 << 10
	MaxMaintainer        = 32 << 20
	MaxHelper            = 8 << 20
	MaxCapsule           = 48 << 20
	MaxSafeInteger int64 = 9_007_199_254_740_991
)

// Limits may tighten the hard bounds; zero fields select the fixed default.
// The privileged caller must additionally impose its 30-second input deadline.
type Limits struct{ Manifest, Maintainer, Helper, Total uint64 }
type Capsule struct{ Manifest, Signature, Maintainer, Helper []byte }
type Component struct {
	Size   int64  `json:"size"`
	SHA512 string `json:"sha512"`
}
type Manifest struct {
	Format             int       `json:"format"`
	Kind               string    `json:"kind"`
	Generation         int64     `json:"generation"`
	Platform           string    `json:"platform"`
	Arch               string    `json:"arch"`
	CompatibilityEpoch int64     `json:"compatibilityEpoch"`
	SandboxAPI         int64     `json:"sandboxApi"`
	PublishedAt        string    `json:"publishedAt"`
	ExpiresAt          string    `json:"expiresAt"`
	Maintainer         Component `json:"maintainer"`
	Helper             Component `json:"helper"`
}

// FileIdentity uses canonical uint64 decimal strings for device/inode so no
// JSON consumer can round a valid Linux identity through a floating-point value.
// No identity or record field chooses a path; paths belong to later fixed IO.
type FileIdentity struct {
	Size   int64  `json:"size"`
	SHA512 string `json:"sha512"`
	Device string `json:"device"`
	Inode  string `json:"inode"`
}
type BootstrapAnchor struct {
	record             recordCache
	Format             int          `json:"format"`
	Kind               string       `json:"kind"`
	Operation          string       `json:"operation"`
	Generation         int64        `json:"generation"`
	AcceptedAt         int64        `json:"acceptedAt"`
	ManifestSHA512     string       `json:"manifestSha512"`
	CompatibilityEpoch int64        `json:"compatibilityEpoch"`
	SandboxAPI         int64        `json:"sandboxApi"`
	Capsule            FileIdentity `json:"capsule"`
	Maintainer         FileIdentity `json:"maintainer"`
}

// InstallIntent stores Manifest and Signature as canonical base64, preserving exact
// admitted bytes across JSON serialization (json.RawMessage can compact them).
type InstallIntent struct {
	record                recordCache
	Format                int           `json:"format"`
	Operation             string        `json:"operation"`
	Generation            int64         `json:"generation"`
	AcceptedAt            int64         `json:"acceptedAt"`
	Authorization         string        `json:"authorization"`
	Manifest              string        `json:"manifest"`
	Signature             string        `json:"signature"`
	OldMaintainer         *FileIdentity `json:"oldMaintainer"`
	NewMaintainer         FileIdentity  `json:"newMaintainer"`
	OldHelper             *FileIdentity `json:"oldHelper"`
	NewHelper             FileIdentity  `json:"newHelper"`
	PreviousReceiptSHA512 *string       `json:"previousReceiptSha512"`
}
type RemovalIntent struct {
	record                recordCache
	Format                int           `json:"format"`
	Operation             string        `json:"operation"`
	MaximumGeneration     int64         `json:"maximumGeneration"`
	AcceptedAt            int64         `json:"acceptedAt"`
	PreviousReceiptSHA512 string        `json:"previousReceiptSha512"`
	CurrentMaintainer     FileIdentity  `json:"currentMaintainer"`
	CurrentHelper         *FileIdentity `json:"currentHelper"`
}
type Receipt struct {
	record              recordCache
	Format              int    `json:"format"`
	Authorization       string `json:"authorization"`
	InitialAnchorSHA512 string `json:"initialAnchorSha512"`
	Manifest            string `json:"manifest"`
	Signature           string `json:"signature"`
	AcceptedAt          int64  `json:"acceptedAt"`
	MaximumGeneration   int64  `json:"maximumGeneration"`
	MaintainerSHA512    string `json:"maintainerSha512"`
	HelperSHA512        string `json:"helperSha512"`
	CompatibilityEpoch  int64  `json:"compatibilityEpoch"`
	SandboxAPI          int64  `json:"sandboxApi"`
	State               string `json:"state"`
	CompletedOperation  string `json:"completedOperation"`
	TransactionSHA512   string `json:"transactionSha512"`
}

// InitialTrust combines compiled initial helper identity with the SHA512 of the
// executing protected maintainer, measured by the bootstrap/native IO layer.
// It is never populated from the incoming manifest or a caller environment.
type InitialTrust struct {
	Generation             int64
	CompatibilityEpoch     int64
	SandboxAPI             int64
	PublishedAt, ExpiresAt string
	Helper                 Component
	SelfMaintainerSHA512   string
}

// Trust must come from compiled public keys and validated ROOT-owned state.
// Pending/Bootstrap authorize only exact previously admitted, protected bytes;
// the IO layer must verify their pinned filesystem identities independently.
// A nonzero Receipt always forbids unsigned initial-install fallback.
type Trust struct {
	PublicKeys []ed25519.PublicKey
	Initial    *InitialTrust
	Bootstrap  *BootstrapAnchor
	Pending    *InstallIntent
	Removing   *RemovalIntent
}
type Requirements struct{ CompatibilityEpoch, SandboxAPI, MinimumGeneration int64 }

// VerifiedCapsule owns its bytes; accessors return copies for safe staging.
type VerifiedCapsule struct {
	capsule       Capsule
	manifest      Manifest
	acceptedAt    time.Time
	authorization string
}

func (v VerifiedCapsule) Manifest() Manifest    { return v.manifest }
func (v VerifiedCapsule) ManifestBytes() []byte { return append([]byte(nil), v.capsule.Manifest...) }
func (v VerifiedCapsule) Signature() []byte     { return append([]byte(nil), v.capsule.Signature...) }
func (v VerifiedCapsule) Maintainer() []byte    { return append([]byte(nil), v.capsule.Maintainer...) }
func (v VerifiedCapsule) Helper() []byte        { return append([]byte(nil), v.capsule.Helper...) }
func (v VerifiedCapsule) AcceptedAt() time.Time { return v.acceptedAt }
func (v VerifiedCapsule) Authorization() string { return v.authorization }

// A parsed record retains its exact on-disk digest without trusting a stale
// digest after a caller changes exported fields. Ownership/durability are IO-layer obligations.
type recordCache struct{ raw, canonical string }

func cacheRecord(raw []byte, v any) recordCache {
	encoded, _ := json.Marshal(v)
	return recordCache{hashBytes(raw), hashBytes(encoded)}
}
func (c recordCache) digest(v any) string {
	encoded, err := json.Marshal(v)
	if err != nil || c.raw == "" || c.canonical != hashBytes(encoded) {
		return ""
	}
	return c.raw
}
func (r Receipt) SHA512() string         { return r.record.digest(r) }
func (r BootstrapAnchor) SHA512() string { return r.record.digest(r) }
func (r InstallIntent) SHA512() string   { return r.record.digest(r) }
func (r RemovalIntent) SHA512() string   { return r.record.digest(r) }

// Decoders validate bytes and schema only. The caller must first establish root
// ownership/identity/durability; parsing a record does not authenticate its origin.
func decodeRecord(raw []byte, limit int, keys []string, target any) (map[string]json.RawMessage, error) {
	if len(raw) == 0 || len(raw) > limit || validateJSON(raw) != nil {
		return nil, ErrState
	}
	obj, err := exactObject(raw, keys...)
	if err != nil || json.Unmarshal(raw, target) != nil {
		return nil, ErrState
	}
	return obj, nil
}
func exactIdentity(raw json.RawMessage, nullable bool) bool {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nullable
	}
	_, err := exactObject(raw, "size", "sha512", "device", "inode")
	return err == nil
}
func ParseBootstrapAnchor(raw []byte) (BootstrapAnchor, error) {
	var a BootstrapAnchor
	obj, err := decodeRecord(raw, 4096, []string{"format", "kind", "operation", "generation", "acceptedAt", "manifestSha512", "compatibilityEpoch", "sandboxApi", "capsule", "maintainer"}, &a)
	if err != nil || !exactIdentity(obj["capsule"], false) || !exactIdentity(obj["maintainer"], false) || validateAnchor(a) != nil {
		return BootstrapAnchor{}, ErrState
	}
	a.record = cacheRecord(raw, a)
	return a, nil
}
func ParseReceipt(raw []byte) (Receipt, error) {
	var r Receipt
	obj, err := decodeRecord(raw, 128<<10, []string{"format", "authorization", "initialAnchorSha512", "manifest", "signature", "acceptedAt", "maximumGeneration", "maintainerSha512", "helperSha512", "compatibilityEpoch", "sandboxApi", "state", "completedOperation", "transactionSha512"}, &r)
	if err != nil || !jsonString(obj["signature"]) {
		return Receipt{}, ErrState
	}
	if _, _, _, err = receiptManifest(r); err != nil {
		return Receipt{}, ErrState
	}
	r.record = cacheRecord(raw, r)
	return r, nil
}
func ParseInstallIntent(raw []byte) (InstallIntent, error) {
	var p InstallIntent
	obj, err := decodeRecord(raw, 128<<10, []string{"format", "operation", "generation", "acceptedAt", "authorization", "manifest", "signature", "oldMaintainer", "newMaintainer", "oldHelper", "newHelper", "previousReceiptSha512"}, &p)
	if err != nil || !jsonString(obj["signature"]) {
		return InstallIntent{}, ErrState
	}
	for _, key := range []string{"oldMaintainer", "newMaintainer", "oldHelper", "newHelper"} {
		if !exactIdentity(obj[key], key == "oldMaintainer" || key == "oldHelper") {
			return InstallIntent{}, ErrState
		}
	}
	if _, _, _, err = intentManifest(p); err != nil {
		return InstallIntent{}, ErrState
	}
	p.record = cacheRecord(raw, p)
	return p, nil
}
func ParseRemovalIntent(raw []byte) (RemovalIntent, error) {
	var p RemovalIntent
	obj, err := decodeRecord(raw, 4096, []string{"format", "operation", "maximumGeneration", "acceptedAt", "previousReceiptSha512", "currentMaintainer", "currentHelper"}, &p)
	if err != nil || !exactIdentity(obj["currentMaintainer"], false) || !exactIdentity(obj["currentHelper"], true) || validateRemoval(p) != nil {
		return RemovalIntent{}, ErrState
	}
	p.record = cacheRecord(raw, p)
	return p, nil
}

func jsonString(raw json.RawMessage) bool {
	value := bytes.TrimSpace(raw)
	return len(value) > 0 && value[0] == '"'
}
