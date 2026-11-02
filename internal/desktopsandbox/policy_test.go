package desktopsandbox

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func manifestOf(t *testing.T, c Capsule) Manifest {
	t.Helper()
	var m Manifest
	if err := json.Unmarshal(c.Manifest, &m); err != nil {
		t.Fatal(err)
	}
	return m
}
func receiptFor(t *testing.T, c Capsule) Receipt {
	t.Helper()
	m := manifestOf(t, c)
	r := Receipt{Format: 1, Authorization: "maintenance-signature", InitialAnchorSHA512: digest([]byte("root initial anchor")), Manifest: base64.StdEncoding.EncodeToString(c.Manifest), Signature: base64.StdEncoding.EncodeToString(c.Signature), AcceptedAt: testNow.Add(-30 * time.Minute).UnixMilli(), LastOperationAt: testNow.Add(-30 * time.Minute).UnixMilli(), MaximumGeneration: m.Generation, MaintainerSHA512: m.Maintainer.SHA512, HelperSHA512: m.Helper.SHA512, CompatibilityEpoch: 1, SandboxAPI: 1, State: "active", CompletedOperation: "install", TransactionSHA512: digest([]byte("root install intent"))}
	raw, _ := json.Marshal(r)
	parsed, err := ParseReceipt(raw)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}
func identity(b []byte, ino string) FileIdentity {
	return FileIdentity{Size: int64(len(b)), SHA512: digest(b), Device: "1", Inode: ino}
}
func initialFor(t *testing.T, c Capsule) *InitialTrust {
	t.Helper()
	m := manifestOf(t, c)
	return &InitialTrust{Generation: m.Generation, CompatibilityEpoch: 1, SandboxAPI: 1, PublishedAt: m.PublishedAt, ExpiresAt: m.ExpiresAt, Helper: m.Helper, SelfMaintainerSHA512: m.Maintainer.SHA512}
}
func anchorFor(t *testing.T, c Capsule) *BootstrapAnchor {
	t.Helper()
	m := manifestOf(t, c)
	anchor := BootstrapAnchor{Format: 1, Kind: "dilla-linux-sandbox-bootstrap", Operation: "initial", Generation: m.Generation, AcceptedAt: testNow.UnixMilli(), ManifestSHA512: digest(c.Manifest), CompatibilityEpoch: 1, SandboxAPI: 1, Capsule: identity(frame(c), "100"), Maintainer: identity(c.Maintainer, "101")}
	raw, _ := json.Marshal(anchor)
	parsed, err := ParseBootstrapAnchor(raw)
	if err != nil {
		t.Fatal(err)
	}
	return &parsed
}
func pendingFor(t *testing.T, c Capsule, old *Receipt) *InstallIntent {
	t.Helper()
	m := manifestOf(t, c)
	p := &InstallIntent{Format: 1, Operation: "install", Generation: m.Generation, AcceptedAt: testNow.UnixMilli(), Authorization: "maintenance-signature", Manifest: base64.StdEncoding.EncodeToString(c.Manifest), Signature: base64.StdEncoding.EncodeToString(c.Signature), NewMaintainer: identity(c.Maintainer, "201"), NewHelper: identity(c.Helper, "202")}
	if old != nil {
		previous := old.SHA512()
		p.PreviousReceiptSHA512 = &previous
		p.OldMaintainer = &FileIdentity{Size: m.Maintainer.Size, SHA512: old.MaintainerSHA512, Device: "1", Inode: "11"}
		if old.State == "active" {
			p.OldHelper = &FileIdentity{Size: m.Helper.Size, SHA512: old.HelperSHA512, Device: "1", Inode: "12"}
		}
	}
	raw, _ := json.Marshal(p)
	parsed, err := ParseInstallIntent(raw)
	if err != nil {
		t.Fatal(err)
	}
	return &parsed
}
func withGeneration(t *testing.T, c Capsule, key ed25519.PrivateKey, generation int64) Capsule {
	t.Helper()
	candidate := copyCapsule(c)
	mutateManifest(t, &candidate, key, func(m map[string]any) { m["generation"] = generation })
	return candidate
}

func TestGenerationFloorCannotBeSplitByChannelOrResetByTombstone(t *testing.T) {
	old, trust, key := fixture(t, 5)
	receipt := receiptFor(t, old)
	for _, state := range []string{"active", "removed"} {
		t.Run(state, func(t *testing.T) {
			r := receipt
			r.State = state
			if state == "removed" {
				r.CompletedOperation = "remove"
			}
			if _, err := VerifyCapsule(withGeneration(t, old, key, 4), trust, r, testNow); err == nil {
				t.Fatal("downgraded machine floor")
			}
			if _, err := VerifyCapsule(old, trust, r, testNow); err != nil {
				t.Fatal("exact current generation rejected", err)
			}
			for _, component := range []string{"helper", "maintainer"} {
				changed := copyCapsule(old)
				target := changed.Helper
				if component == "maintainer" {
					target = changed.Maintainer
				}
				target[128] ^= 1
				mutateManifest(t, &changed, key, func(m map[string]any) { m[component].(map[string]any)["sha512"] = digest(target) })
				if _, err := VerifyCapsule(changed, trust, r, testNow); err == nil {
					t.Fatal("same-generation bytes changed", component)
				}
			}
			whitespace := copyCapsule(old)
			whitespace.Manifest = append(whitespace.Manifest, ' ')
			resign(&whitespace, key)
			if _, err := VerifyCapsule(whitespace, trust, r, testNow); err == nil {
				t.Fatal("same-generation exact manifest changed")
			}
			if _, err := VerifyCapsule(withGeneration(t, old, key, 6), trust, r, testNow); err != nil {
				t.Fatal("signed higher generation rejected", err)
			}
		})
	}
}

func TestInitialDigestOnlyAuthorizesGenuineFirstInstallation(t *testing.T) {
	c, trust, _ := fixture(t, 1)
	signed := copyCapsule(c)
	c.Signature = nil
	trust.Initial = initialFor(t, c)
	v, err := VerifyCapsule(c, trust, Receipt{}, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if v.Authorization() != "initial-digest" {
		t.Fatal("lost initial trust distinction")
	}
	for _, kind := range []string{"no initial tuple", "wrong own executable", "wrong helper", "wrong generation", "wrong dates", "missing public trust", "partial receipt", "resident active", "resident removed"} {
		t.Run(kind, func(t *testing.T) {
			tr := trust
			init := *trust.Initial
			tr.Initial = &init
			r := Receipt{}
			switch kind {
			case "no initial tuple":
				tr.Initial = nil
			case "wrong own executable":
				init.SelfMaintainerSHA512 = digest([]byte("different executing image"))
			case "wrong helper":
				init.Helper.SHA512 = digest([]byte("different pinned helper"))
			case "wrong generation":
				init.Generation = 2
			case "wrong dates":
				init.ExpiresAt = testNow.Add(time.Hour).Format(time.RFC3339)
			case "missing public trust":
				tr.PublicKeys = nil
			case "partial receipt":
				r.State = "removed"
			case "resident active", "resident removed":
				r = receiptFor(t, signed)
				r.Authorization = "initial-digest"
				r.Signature = ""
				if kind == "resident removed" {
					r.State = "removed"
					r.CompletedOperation = "remove"
				}
			}
			if _, err := VerifyCapsule(c, tr, r, testNow); err == nil {
				t.Fatal("unsigned resident/incorrect initial tuple accepted")
			}
		})
	}
}

func TestPendingAndInitialAnchorResumeOnlyExactHistoricallyAcceptedBytes(t *testing.T) {
	c, trust, key := fixture(t, 8)
	p := pendingFor(t, c, nil)
	trust.Pending = p
	later := testNow.Add(48 * time.Hour)
	if _, err := VerifyCapsule(c, trust, Receipt{}, later); err != nil {
		t.Fatal("exact accepted pending bytes could not resume", err)
	}
	if _, err := VerifyCapsule(withGeneration(t, c, key, 9), trust, Receipt{}, later); err == nil {
		t.Fatal("different input inherited pending acceptance time")
	}
	changed := copyCapsule(c)
	changed.Helper[128] ^= 1
	if _, err := VerifyCapsule(changed, trust, Receipt{}, later); err == nil {
		t.Fatal("changed protected bytes resumed")
	}
	for _, kind := range []string{"expired before acceptance", "future acceptance", "wrong signature", "wrong new inode hash", "mismatched manifest generation"} {
		t.Run(kind, func(t *testing.T) {
			tr := trust
			pp := *p
			tr.Pending = &pp
			switch kind {
			case "expired before acceptance":
				pp.AcceptedAt = later.UnixMilli()
			case "future acceptance":
				pp.AcceptedAt = testNow.Add(10 * time.Minute).UnixMilli()
			case "wrong signature":
				pp.Signature = base64.StdEncoding.EncodeToString(make([]byte, 64))
			case "wrong new inode hash":
				pp.NewHelper.SHA512 = digest([]byte("wrong protected stage"))
			case "mismatched manifest generation":
				pp.Generation = 9
			}
			if _, err := VerifyCapsule(c, tr, Receipt{}, testNow); err == nil {
				t.Fatal("invalid root admission accepted")
			}
		})
	}
	unsigned := copyCapsule(c)
	unsigned.Signature = nil
	tr := trust
	tr.Pending = nil
	tr.Initial = initialFor(t, unsigned)
	tr.Bootstrap = anchorFor(t, unsigned)
	if _, err := VerifyCapsule(unsigned, tr, Receipt{}, later); err != nil {
		t.Fatal("accepted initial anchor could not resume", err)
	}
	bad := *tr.Bootstrap
	bad.Capsule.SHA512 = digest([]byte("substitute root stage"))
	tr.Bootstrap = &bad
	if _, err := VerifyCapsule(unsigned, tr, Receipt{}, later); err == nil {
		t.Fatal("mismatched bootstrap stage accepted")
	}
}

func TestPendingHigherFloorAndRemovalCannotBeDiscarded(t *testing.T) {
	old, trust, key := fixture(t, 2)
	r := receiptFor(t, old)
	next := withGeneration(t, old, key, 3)
	trust.Pending = pendingFor(t, next, &r)
	if _, err := VerifyCapsule(old, trust, r, testNow); err == nil {
		t.Fatal("older receipt suppressed durable newer intent")
	}
	if _, err := VerifyCapsule(next, trust, r, testNow); err != nil {
		t.Fatal(err)
	}
	trust.Pending = nil
	trust.Removing = &RemovalIntent{Format: 1, Operation: "remove", MaximumGeneration: 2, AcceptedAt: testNow.UnixMilli(), PreviousReceiptSHA512: digest([]byte("root receipt")), CurrentMaintainer: identity(old.Maintainer, "301"), CurrentHelper: func() *FileIdentity { i := identity(old.Helper, "302"); return &i }()}
	if _, err := VerifyCapsule(next, trust, r, testNow); err == nil {
		t.Fatal("new installation suppressed an outstanding removal")
	}
}

func TestHistoricalSignatureVerificationRequiresOverlappingOldKey(t *testing.T) {
	c, trust, _ := fixture(t, 1)
	r := receiptFor(t, c)
	p := pendingFor(t, c, &r)
	trust.Pending = p
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tr := trust
	tr.PublicKeys = []ed25519.PublicKey{pub}
	if _, err := VerifyCapsule(c, tr, r, testNow.Add(48*time.Hour)); err == nil {
		t.Fatal("new-key-only maintainer accepted old-key intent")
	}
	tr.PublicKeys = append(tr.PublicKeys, trust.PublicKeys...)
	if _, err := VerifyCapsule(c, tr, r, testNow.Add(48*time.Hour)); err != nil {
		t.Fatal("key overlap did not allow exact recovery", err)
	}
}

func TestFreshDatesAndClockRollbackDoNotBorrowHistoricalAuthority(t *testing.T) {
	for _, kind := range []string{"expired", "future publication", "too long", "timezone alias", "invalid calendar", "clock rollback"} {
		t.Run(kind, func(t *testing.T) {
			c, trust, key := fixture(t, 2)
			r := Receipt{}
			mutateManifest(t, &c, key, func(m map[string]any) {
				switch kind {
				case "expired":
					m["expiresAt"] = testNow.Format(time.RFC3339)
				case "future publication":
					m["publishedAt"] = testNow.Add(5*time.Minute + time.Second).Format(time.RFC3339)
				case "too long":
					m["expiresAt"] = testNow.Add(94 * 24 * time.Hour).Format(time.RFC3339)
				case "timezone alias":
					m["publishedAt"] = "2026-11-02T11:00:00+00:00"
				case "invalid calendar":
					m["publishedAt"] = "2026-02-30T11:00:00Z"
				}
			})
			now := testNow
			if kind == "clock rollback" {
				r = receiptFor(t, c)
				now = testNow.Add(-time.Hour)
			}
			if _, err := VerifyCapsule(c, trust, r, now); err == nil {
				t.Fatal("invalid fresh time accepted")
			}
		})
	}
}

func TestAdmissionTimeNeverMovesBehindProtectedReceipt(t *testing.T) {
	c, trust, key := fixture(t, 2)
	r := receiptFor(t, c)
	for _, candidate := range []Capsule{c, withGeneration(t, c, key, 3)} {
		for _, delta := range []time.Duration{time.Millisecond, 4 * time.Minute} {
			now := time.UnixMilli(r.AcceptedAt).Add(-delta)
			if _, err := VerifyCapsule(candidate, trust, r, now); !errors.Is(err, ErrTime) {
				t.Fatalf("clock %s behind protected admission must fail: %v", delta, err)
			}
		}
		for _, delta := range []time.Duration{0, time.Millisecond} {
			now := time.UnixMilli(r.AcceptedAt).Add(delta)
			v, err := VerifyCapsule(candidate, trust, r, now)
			if err != nil || v.AcceptedAt().UnixMilli() < r.AcceptedAt {
				t.Fatalf("monotonic admission rejected or lowered history: %v", err)
			}
		}
	}
}

func TestPendingAdmissionCannotPrecedeItsPreviousReceipt(t *testing.T) {
	c, trust, key := fixture(t, 2)
	r := receiptFor(t, c)
	next := withGeneration(t, c, key, 3)
	for _, delta := range []int64{-4 * 60 * 1000, -1, 0, 1} {
		p := pendingFor(t, next, &r)
		p.AcceptedAt = r.AcceptedAt + delta
		raw, err := json.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := ParseInstallIntent(raw)
		if err != nil {
			t.Fatal(err)
		}
		tr := trust
		tr.Pending = &parsed
		v, err := VerifyCapsule(next, tr, r, testNow)
		if delta < 0 {
			if !errors.Is(err, ErrTime) {
				t.Fatalf("historical admission %dms behind prior receipt must fail: %v", -delta, err)
			}
		} else if err != nil || v.AcceptedAt().UnixMilli() != parsed.AcceptedAt {
			t.Fatalf("valid pending acceptance time was not preserved: %v", err)
		}
	}
}

func TestCompatibilityIsCommittedReceiptOnlyAndNeverAnActualSandboxProof(t *testing.T) {
	c, _, _ := fixture(t, 7)
	r := receiptFor(t, c)
	req := Requirements{CompatibilityEpoch: 1, SandboxAPI: 1, MinimumGeneration: 7}
	if err := CheckCompatibility(req, Receipt{}); err != nil {
		t.Fatal("proven absent namespace needs no helper", err)
	}
	if err := CheckCompatibility(req, r); err != nil {
		t.Fatal(err)
	}
	// No current-time expiry check here: the already installed signed bytes stay
	// usable offline; this does not admit a fresh expired capsule or prove OS sandbox.
	for _, kind := range []string{"below minimum", "removed", "pending status", "wrong API", "wrong epoch", "partial absence"} {
		t.Run(kind, func(t *testing.T) {
			rr := r
			q := req
			switch kind {
			case "below minimum":
				q.MinimumGeneration = 8
			case "removed":
				rr.State = "removed"
				rr.CompletedOperation = "remove"
			case "pending status":
				rr.State = "maintenance"
			case "wrong API":
				q.SandboxAPI = 2
			case "wrong epoch":
				q.CompatibilityEpoch = 2
			case "partial absence":
				rr = Receipt{MaximumGeneration: 7}
			}
			if CheckCompatibility(q, rr) == nil {
				t.Fatal("incompatible managed receipt accepted")
			}
		})
	}
}

func TestPendingOldIdentitiesMatchCommittedComponentSizes(t *testing.T) {
	old, trust, key := fixture(t, 1)
	r := receiptFor(t, old)
	next := withGeneration(t, old, key, 2)
	for _, target := range []string{"maintainer", "helper"} {
		t.Run(target, func(t *testing.T) {
			p := pendingFor(t, next, &r)
			if target == "maintainer" {
				p.OldMaintainer.Size = 1
			} else {
				p.OldHelper.Size = 1
			}
			raw, _ := json.Marshal(p)
			parsed, err := ParseInstallIntent(raw)
			if err != nil {
				t.Fatal(err)
			}
			tr := trust
			tr.Pending = &parsed
			if _, err = VerifyCapsule(next, tr, r, testNow); err == nil {
				t.Fatal("pending identity size did not match prior admitted bytes")
			}
		})
	}
}

func TestInitialReceiptStillRequiresInstalledMaintenanceTrust(t *testing.T) {
	c, trust, _ := fixture(t, 1)
	r := receiptFor(t, c)
	r.Authorization = "initial-digest"
	r.Signature = ""
	if err := VerifyReceipt(r, trust, testNow); err != nil {
		t.Fatal(err)
	}
	trust.PublicKeys = nil
	if err := VerifyReceipt(r, trust, testNow); err == nil {
		t.Fatal("managed receipt accepted without future maintenance trust")
	}
}

func TestInitialInstallIntentNeedsMatchingBootstrapAuthority(t *testing.T) {
	c, trust, _ := fixture(t, 1)
	p := pendingFor(t, c, nil)
	p.Authorization = "initial-anchor"
	p.Signature = ""
	raw, _ := json.Marshal(p)
	parsed, err := ParseInstallIntent(raw)
	if err != nil {
		t.Fatal(err)
	}
	c.Signature = nil
	trust.Initial = initialFor(t, c)
	trust.Bootstrap = anchorFor(t, c)
	trust.Pending = &parsed
	if _, err := VerifyCapsule(c, trust, Receipt{}, testNow.Add(48*time.Hour)); err != nil {
		t.Fatal("exact initial intent could not recover", err)
	}
	trust.Bootstrap = nil
	if _, err := VerifyCapsule(c, trust, Receipt{}, testNow); err == nil {
		t.Fatal("unsigned intent substituted for the bootstrap anchor")
	}
}

func operationReceipt(t *testing.T, c Capsule, last int64, state string) Receipt {
	t.Helper()
	r := receiptFor(t, c)
	r.LastOperationAt = last
	r.State = state
	if state == "removed" {
		r.CompletedOperation = "remove"
	}
	raw, _ := json.Marshal(r)
	parsed, err := ParseReceipt(raw)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}
func freshGenerationAt(t *testing.T, c Capsule, key ed25519.PrivateKey, generation int64, at time.Time) Capsule {
	t.Helper()
	next := copyCapsule(c)
	mutateManifest(t, &next, key, func(m map[string]any) {
		m["generation"] = generation
		m["publishedAt"] = at.Add(-time.Hour).Format(time.RFC3339)
		m["expiresAt"] = at.Add(24 * time.Hour).Format(time.RFC3339)
	})
	return next
}

func TestTombstoneOperationClockSurvivesExpiredComponentAndRejectsReinstallRollback(t *testing.T) {
	old, trust, key := fixture(t, 1)
	removedAt := testNow.Add(48 * time.Hour)
	r := operationReceipt(t, old, removedAt.UnixMilli(), "removed")
	candidate := freshGenerationAt(t, old, key, 2, removedAt)
	if VerifyReceipt(r, trust, removedAt) != nil {
		t.Fatal("removal after component expiry required fresh signing")
	}
	if _, err := VerifyCapsule(candidate, trust, r, removedAt.Add(-time.Millisecond)); err == nil {
		t.Fatal("clock rollback behind tombstone operation admitted new code")
	}
	if _, err := VerifyCapsule(candidate, trust, r, removedAt); err != nil {
		t.Fatal("signed fresh reinstall at preserved floor rejected", err)
	}
}

func TestPendingAdmissionCannotPrecedePriorOperationClock(t *testing.T) {
	old, trust, key := fixture(t, 1)
	removedAt := testNow.Add(48 * time.Hour)
	r := operationReceipt(t, old, removedAt.UnixMilli(), "removed")
	candidate := freshGenerationAt(t, old, key, 2, removedAt)
	p := pendingFor(t, old, &r)
	p.Generation = 2
	p.Manifest = base64.StdEncoding.EncodeToString(candidate.Manifest)
	p.Signature = base64.StdEncoding.EncodeToString(candidate.Signature)
	p.AcceptedAt = removedAt.Add(-time.Millisecond).UnixMilli()
	raw, _ := json.Marshal(p)
	parsed, err := ParseInstallIntent(raw)
	if err != nil {
		t.Fatal(err)
	}
	trust.Pending = &parsed
	if _, err = VerifyCapsule(candidate, trust, r, removedAt.Add(time.Hour)); err == nil {
		t.Fatal("pending admission borrowed older component timestamp instead of operation floor")
	}
}

func TestHistoricalCompletionRecordsNewOperationTimeWithoutChangingComponentAcceptance(t *testing.T) {
	c, trust, _ := fixture(t, 1)
	completedAt := testNow.Add(48 * time.Hour)
	r := operationReceipt(t, c, completedAt.UnixMilli(), "active")
	if r.AcceptedAt >= r.LastOperationAt {
		t.Fatal("fixture did not separate admission and completion")
	}
	if err := VerifyReceipt(r, trust, completedAt); err != nil {
		t.Fatal("historical accepted bytes expired before completion", err)
	}
	if CheckCompatibility(Requirements{1, 1, 1}, r) != nil {
		t.Fatal("committed helper stopped working offline")
	}
}
