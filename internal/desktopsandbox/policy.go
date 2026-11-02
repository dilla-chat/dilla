package desktopsandbox

import (
	"bytes"
	"crypto/ed25519"
	"strconv"
	"time"
)

// VerifyCapsule admits bytes, not paths or state ownership. Receipt and pending
// context MUST already come from the caller's pinned root namespace. A zero
// receipt denotes proven complete namespace absence, never an IO error.
func VerifyCapsule(input Capsule, trust Trust, receipt Receipt, now time.Time) (VerifiedCapsule, error) {
	if !capsuleShape(input) {
		return VerifiedCapsule{}, ErrFraming
	}
	c := copyCapsule(input)
	m, err := parseManifest(c.Manifest)
	if err != nil {
		return VerifiedCapsule{}, err
	}
	if len(c.Maintainer) != int(m.Maintainer.Size) || len(c.Helper) != int(m.Helper.Size) || !matchesDigest(c.Maintainer, m.Maintainer.SHA512) || !matchesDigest(c.Helper, m.Helper.SHA512) {
		return VerifiedCapsule{}, ErrManifest
	}
	if !validKeys(trust.PublicKeys) {
		return VerifiedCapsule{}, ErrAuthentication
	}
	if err = VerifyReceipt(receipt, trust, now); err != nil {
		return VerifiedCapsule{}, err
	}
	// Publication skew is independent of protected history. Even a one-ms
	// rollback must not become a new lower floor through repeated admissions.
	if !validMoment(now.UnixMilli()) || (!absent(receipt) && now.UnixMilli() < receipt.LastOperationAt) {
		return VerifiedCapsule{}, ErrTime
	}
	if trust.Removing != nil {
		return VerifiedCapsule{}, ErrPolicy
	}
	acceptedAt := now.UTC()
	authorization := "maintenance-signature"
	if trust.Pending != nil {
		acceptedAt, authorization, err = verifyPending(c, m, trust, receipt, now)
	} else if trust.Bootstrap != nil {
		if !absent(receipt) {
			return VerifiedCapsule{}, ErrState
		}
		err = verifyBootstrap(c, m, trust, now)
		acceptedAt = time.UnixMilli(trust.Bootstrap.AcceptedAt).UTC()
		authorization = "initial-digest"
	} else if len(c.Signature) == 0 {
		if !absent(receipt) || !matchesInitial(m, trust.Initial) {
			return VerifiedCapsule{}, ErrAuthentication
		}
		authorization = "initial-digest"
	} else if !verifySignature(c.Manifest, c.Signature, trust.PublicKeys) {
		return VerifiedCapsule{}, ErrAuthentication
	}
	if err != nil {
		return VerifiedCapsule{}, err
	}
	if err = acceptedWithin(m, acceptedAt, now); err != nil {
		return VerifiedCapsule{}, err
	}
	if !absent(receipt) {
		if m.Generation < receipt.MaximumGeneration {
			return VerifiedCapsule{}, ErrPolicy
		}
		oldBytes, _, _, _ := receiptManifest(receipt)
		if m.Generation == receipt.MaximumGeneration && (!bytes.Equal(c.Manifest, oldBytes) || m.Maintainer.SHA512 != receipt.MaintainerSHA512 || m.Helper.SHA512 != receipt.HelperSHA512) {
			return VerifiedCapsule{}, ErrPolicy
		}
	}
	if verifyELF(c.Maintainer) != nil || verifyELF(c.Helper) != nil {
		return VerifiedCapsule{}, ErrExecutable
	}
	return VerifiedCapsule{c, m, acceptedAt, authorization}, nil
}
func validKeys(keys []ed25519.PublicKey) bool {
	if len(keys) == 0 || len(keys) > 8 {
		return false
	}
	seen := map[string]bool{}
	for _, k := range keys {
		if len(k) != ed25519.PublicKeySize || seen[string(k)] {
			return false
		}
		seen[string(k)] = true
	}
	return true
}
func verifySignature(manifest, signature []byte, keys []ed25519.PublicKey) bool {
	if len(signature) != ed25519.SignatureSize || !validKeys(keys) {
		return false
	}
	message := append([]byte(signatureDomain), manifest...)
	for _, key := range keys {
		if ed25519.Verify(key, message, signature) {
			return true
		}
	}
	return false
}
func absent(r Receipt) bool     { return r == (Receipt{}) }
func validMoment(ms int64) bool { return ms > 0 && ms <= 253402300799999 }
func acceptedWithin(m Manifest, accepted, now time.Time) error {
	if !validMoment(accepted.UnixMilli()) || accepted.After(now.Add(5*time.Minute)) {
		return ErrTime
	}
	pub, exp, err := manifestTimes(m)
	if err != nil || pub.After(accepted.Add(5*time.Minute)) || !exp.After(accepted) {
		return ErrTime
	}
	return nil
}
func matchesInitial(m Manifest, i *InitialTrust) bool {
	return i != nil && i.Generation == m.Generation && i.CompatibilityEpoch == 1 && i.SandboxAPI == 1 && i.PublishedAt == m.PublishedAt && i.ExpiresAt == m.ExpiresAt && i.Helper == m.Helper && i.SelfMaintainerSHA512 == m.Maintainer.SHA512
}
func validIdentity(f FileIdentity, maximum int64) bool {
	if f.Size < 1 || f.Size > maximum || !validDigest(f.SHA512) {
		return false
	}
	for _, value := range []string{f.Device, f.Inode} {
		n, err := strconv.ParseUint(value, 10, 64)
		if err != nil || strconv.FormatUint(n, 10) != value {
			return false
		}
	}
	return f.Inode != "0"
}
func validateAnchor(a BootstrapAnchor) error {
	if a.Format != 1 || a.Kind != "dilla-linux-sandbox-bootstrap" || a.Operation != "initial" || a.Generation < 1 || a.Generation > MaxSafeInteger || !validMoment(a.AcceptedAt) || !validDigest(a.ManifestSHA512) || a.CompatibilityEpoch != 1 || a.SandboxAPI != 1 || !validIdentity(a.Capsule, MaxCapsule) || !validIdentity(a.Maintainer, MaxMaintainer) {
		return ErrState
	}
	return nil
}
func verifyBootstrap(c Capsule, m Manifest, trust Trust, now time.Time) error {
	a := trust.Bootstrap
	if a == nil || a.SHA512() == "" || validateAnchor(*a) != nil || len(c.Signature) != 0 || !matchesInitial(m, trust.Initial) || a.Generation != m.Generation || a.ManifestSHA512 != hashBytes(c.Manifest) || a.Capsule.Size != int64(32+len(c.Manifest)+len(c.Maintainer)+len(c.Helper)) || a.Capsule.SHA512 != capsuleDigest(c) || a.Maintainer.Size != int64(len(c.Maintainer)) || a.Maintainer.SHA512 != m.Maintainer.SHA512 {
		return ErrState
	}
	return acceptedWithin(m, time.UnixMilli(a.AcceptedAt), now)
}
func receiptManifest(r Receipt) ([]byte, []byte, Manifest, error) {
	raw, e := decodeBase64(r.Manifest, MaxManifest)
	if e != nil {
		return nil, nil, Manifest{}, ErrState
	}
	m, e := parseManifest(raw)
	if e != nil {
		return nil, nil, Manifest{}, ErrState
	}
	sig, e := decodeBase64(r.Signature, 64)
	if e != nil || r.Format != 1 || !validMoment(r.AcceptedAt) || !validMoment(r.LastOperationAt) || r.LastOperationAt < r.AcceptedAt || r.MaximumGeneration != m.Generation || r.MaintainerSHA512 != m.Maintainer.SHA512 || r.HelperSHA512 != m.Helper.SHA512 || r.CompatibilityEpoch != 1 || r.SandboxAPI != 1 || !validDigest(r.InitialAnchorSHA512) || !validDigest(r.TransactionSHA512) {
		return nil, nil, Manifest{}, ErrState
	}
	if (r.State == "active" && r.CompletedOperation != "install") || (r.State == "removed" && r.CompletedOperation != "remove") || (r.State != "active" && r.State != "removed") {
		return nil, nil, Manifest{}, ErrState
	}
	if (r.Authorization == "initial-digest" && len(sig) != 0) || (r.Authorization == "maintenance-signature" && len(sig) != 64) || (r.Authorization != "initial-digest" && r.Authorization != "maintenance-signature") {
		return nil, nil, Manifest{}, ErrState
	}
	// Receipt dates are checked against their ROOT-recorded admission time,
	// never current expiry: already installed bytes remain usable offline.
	at := time.UnixMilli(r.AcceptedAt)
	if acceptedWithin(m, at, at) != nil {
		return nil, nil, Manifest{}, ErrState
	}
	return raw, sig, m, nil
}

// VerifyReceipt rechecks signatures at recorded acceptance time. It cannot
// establish root-file ownership or actual current component bytes by itself.
func VerifyReceipt(r Receipt, trust Trust, _ time.Time) error {
	if absent(r) {
		return nil
	}
	raw, sig, _, err := receiptManifest(r)
	if err != nil {
		return err
	}
	if !validKeys(trust.PublicKeys) {
		return ErrAuthentication
	}
	if r.Authorization == "maintenance-signature" && !verifySignature(raw, sig, trust.PublicKeys) {
		return ErrAuthentication
	}
	return nil
}
func intentManifest(p InstallIntent) ([]byte, []byte, Manifest, error) {
	raw, e := decodeBase64(p.Manifest, MaxManifest)
	if e != nil {
		return nil, nil, Manifest{}, ErrState
	}
	m, e := parseManifest(raw)
	if e != nil {
		return nil, nil, Manifest{}, ErrState
	}
	sig, e := decodeBase64(p.Signature, 64)
	if e != nil || p.Format != 1 || p.Operation != "install" || p.Generation != m.Generation || !validMoment(p.AcceptedAt) || !validIdentity(p.NewMaintainer, MaxMaintainer) || !validIdentity(p.NewHelper, MaxHelper) || p.NewMaintainer.Size != m.Maintainer.Size || p.NewMaintainer.SHA512 != m.Maintainer.SHA512 || p.NewHelper.Size != m.Helper.Size || p.NewHelper.SHA512 != m.Helper.SHA512 {
		return nil, nil, Manifest{}, ErrState
	}
	if p.OldMaintainer != nil && !validIdentity(*p.OldMaintainer, MaxMaintainer) || p.OldHelper != nil && !validIdentity(*p.OldHelper, MaxHelper) || p.OldMaintainer == nil && p.OldHelper != nil || p.PreviousReceiptSHA512 != nil && !validDigest(*p.PreviousReceiptSHA512) {
		return nil, nil, Manifest{}, ErrState
	}
	if (p.Authorization == "initial-anchor" && len(sig) != 0) || (p.Authorization == "maintenance-signature" && len(sig) != 64) || (p.Authorization != "initial-anchor" && p.Authorization != "maintenance-signature") {
		return nil, nil, Manifest{}, ErrState
	}
	at := time.UnixMilli(p.AcceptedAt)
	if acceptedWithin(m, at, at) != nil {
		return nil, nil, Manifest{}, ErrState
	}
	return raw, sig, m, nil
}
func verifyPending(c Capsule, m Manifest, trust Trust, r Receipt, now time.Time) (time.Time, string, error) {
	p := trust.Pending
	raw, sig, pm, err := intentManifest(*p)
	if err != nil || p.SHA512() == "" || pm.Generation != m.Generation || !bytes.Equal(raw, c.Manifest) || !bytes.Equal(sig, c.Signature) {
		return time.Time{}, "", ErrState
	}
	if absent(r) {
		if p.PreviousReceiptSHA512 != nil || p.OldMaintainer != nil || p.OldHelper != nil {
			return time.Time{}, "", ErrState
		}
	} else {
		if p.AcceptedAt < r.LastOperationAt {
			return time.Time{}, "", ErrTime
		}
		_, _, oldManifest, _ := receiptManifest(r)
		if p.PreviousReceiptSHA512 == nil || r.SHA512() == "" || *p.PreviousReceiptSHA512 != r.SHA512() || p.OldMaintainer == nil || p.OldMaintainer.SHA512 != r.MaintainerSHA512 || p.OldMaintainer.Size != oldManifest.Maintainer.Size || m.Generation < r.MaximumGeneration {
			return time.Time{}, "", ErrState
		}
		if r.State == "active" && (p.OldHelper == nil || p.OldHelper.SHA512 != r.HelperSHA512 || p.OldHelper.Size != oldManifest.Helper.Size) || r.State == "removed" && p.OldHelper != nil {
			return time.Time{}, "", ErrState
		}
	}
	at := time.UnixMilli(p.AcceptedAt).UTC()
	if p.Authorization == "initial-anchor" {
		if !absent(r) || trust.Bootstrap == nil || p.AcceptedAt != trust.Bootstrap.AcceptedAt || verifyBootstrap(c, m, trust, now) != nil {
			return time.Time{}, "", ErrState
		}
		return at, "initial-digest", nil
	}
	if trust.Bootstrap != nil || !verifySignature(raw, sig, trust.PublicKeys) {
		return time.Time{}, "", ErrAuthentication
	}
	return at, "maintenance-signature", nil
}
func validateRemoval(p RemovalIntent) error {
	if p.Format != 1 || p.Operation != "remove" || p.MaximumGeneration < 1 || p.MaximumGeneration > MaxSafeInteger || !validMoment(p.AcceptedAt) || !validDigest(p.PreviousReceiptSHA512) || !validIdentity(p.CurrentMaintainer, MaxMaintainer) || p.CurrentHelper != nil && !validIdentity(*p.CurrentHelper, MaxHelper) {
		return ErrState
	}
	return nil
}

// CheckCompatibility considers only an already validated committed receipt.
// The caller must reject pending/unsafe filesystem states separately and prove
// actual Electron sandboxing. Complete namespace absence needs no system helper.
func CheckCompatibility(req Requirements, r Receipt) error {
	if req.CompatibilityEpoch != 1 || req.SandboxAPI != 1 || req.MinimumGeneration < 1 || req.MinimumGeneration > MaxSafeInteger {
		return ErrCompatibility
	}
	if absent(r) {
		return nil
	}
	if _, _, _, err := receiptManifest(r); err != nil || r.State != "active" || r.MaximumGeneration < req.MinimumGeneration {
		return ErrCompatibility
	}
	return nil
}
