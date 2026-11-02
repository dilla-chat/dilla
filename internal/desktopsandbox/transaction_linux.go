//go:build linux

package desktopsandbox

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"time"
)

type Maintainer struct {
	files *fixedFiles
	trust Trust
	now   func() time.Time
}

func (m *Maintainer) Close() error { return m.files.close() }

type protectedState struct {
	receipt Receipt
	anchor  *BootstrapAnchor
	install *InstallIntent
	removal *RemovalIntent
}

func (m *Maintainer) snapshot() (s protectedState, err error) {
	if e := m.files.inventory(rootDir); e != nil {
		return s, e
	}
	if e := m.files.inventory(stageDir); e != nil {
		return s, e
	}
	raw, e := m.files.read(rootDir, "sandbox-receipt.json", recordLimit, 0o644)
	if e != nil {
		return s, e
	}
	if raw != nil {
		s.receipt, e = ParseReceipt(raw)
		if e != nil {
			return s, e
		}
		if e = VerifyReceipt(s.receipt, m.trust, m.now()); e != nil {
			return s, e
		}
	}
	raw, e = m.files.read(rootDir, "sandbox-transaction.json", recordLimit, 0o644)
	if e != nil {
		return s, e
	}
	if raw != nil {
		if p, x := ParseInstallIntent(raw); x == nil {
			s.install = &p
		} else if p, x := ParseRemovalIntent(raw); x == nil {
			s.removal = &p
		} else {
			return s, ErrState
		}
	}
	raw, e = m.files.read(stageDir, "bootstrap-anchor.json", recordLimit, 0o600)
	if e != nil {
		return s, e
	}
	if raw != nil {
		a, e := ParseBootstrapAnchor(raw)
		if e != nil {
			return s, e
		}
		s.anchor = &a
	}
	return s, nil
}
func (m *Maintainer) NeedsRecovery() (bool, error) {
	s, e := m.snapshot()
	if e != nil {
		return false, e
	}
	return s.install != nil || s.removal != nil || s.anchor != nil, e
}
func (m *Maintainer) Process(ctx context.Context, input io.Reader) (Receipt, error) {
	pending, e := m.NeedsRecovery()
	if e != nil {
		return Receipt{}, e
	}
	if pending {
		return m.Recover(ctx)
	}
	s, e := m.snapshot()
	if e != nil {
		return Receipt{}, e
	}
	if absent(s.receipt) {
		return Receipt{}, ErrState
	}
	c, e := ParseCapsule(input, Limits{})
	if e != nil {
		return Receipt{}, e
	}
	// Unsigned input is never resident authority, including an empty receipt.
	if len(c.Signature) == 0 {
		return Receipt{}, ErrAuthentication
	}
	v, e := VerifyCapsule(c, m.trust, s.receipt, m.now())
	if e != nil {
		return Receipt{}, e
	}
	return m.Install(ctx, v)
}
func (m *Maintainer) current(r Receipt) error {
	if absent(r) {
		for _, n := range []string{"sandbox-maintainer", "chrome-sandbox"} {
			p, e := m.files.pin(rootDir, n, MaxMaintainer, 0o755, m.files.layout.helperMode)
			if e != nil {
				return e
			}
			if p != nil {
				closePinned(p)
				return ErrState
			}
		}
		return nil
	}
	_, _, manifest, e := receiptManifest(r)
	if e != nil {
		return e
	}
	for _, entry := range []struct {
		name      string
		component Component
		mode      uint32
	}{{"sandbox-maintainer", manifest.Maintainer, 0o755}, {"chrome-sandbox", manifest.Helper, m.files.layout.helperMode}} {
		p, e := m.files.pin(rootDir, entry.name, entry.component.Size, entry.mode)
		if e != nil {
			return e
		}
		if entry.name == "chrome-sandbox" && r.State == "removed" {
			if p != nil {
				closePinned(p)
				return ErrState
			}
			continue
		}
		if p == nil {
			return ErrState
		}
		ok := p.identity.Size == entry.component.Size && p.identity.SHA512 == entry.component.SHA512
		closePinned(p)
		if !ok {
			return ErrState
		}
	}
	return nil
}
func (m *Maintainer) Recover(ctx context.Context) (Receipt, error) {
	if e := ctx.Err(); e != nil {
		return Receipt{}, e
	}
	s, e := m.snapshot()
	if e != nil {
		return Receipt{}, e
	}
	if !absent(s.receipt) {
		terminal := s.install != nil && s.receipt.CompletedOperation == "install" && s.receipt.TransactionSHA512 == s.install.SHA512() || s.removal != nil && s.receipt.CompletedOperation == "remove" && s.receipt.TransactionSHA512 == s.removal.SHA512()
		if terminal || s.install == nil && s.removal == nil {
			if e = m.current(s.receipt); e != nil {
				return Receipt{}, e
			}
			if e = m.cleanup(s); e != nil {
				return Receipt{}, e
			}
			return s.receipt, nil
		}
	}
	if s.removal != nil {
		return m.finishRemoval(ctx, s)
	}
	if s.install != nil {
		if s.install.Authorization == "initial-anchor" {
			if _, e = m.anchoredCapsule(s); e != nil {
				return Receipt{}, e
			}
		}
		return m.finishInstall(ctx, s)
	}
	if s.anchor == nil {
		return Receipt{}, ErrState
	}
	v, e := m.anchoredCapsule(s)
	if e != nil {
		return Receipt{}, e
	}
	return m.stageInstall(ctx, s, v)
}
func (m *Maintainer) anchoredCapsule(s protectedState) (VerifiedCapsule, error) {
	if s.anchor == nil || !absent(s.receipt) {
		return VerifiedCapsule{}, ErrState
	}
	p, e := m.files.pin(stageDir, "bootstrap-capsule", MaxCapsule, 0o600)
	if e != nil {
		return VerifiedCapsule{}, e
	}
	defer closePinned(p)
	if requireIdentity(p, s.anchor.Capsule) != nil {
		return VerifiedCapsule{}, ErrState
	}
	mgr, e := m.files.pin(stageDir, "bootstrap-maintainer", MaxMaintainer, 0o700)
	if e != nil {
		return VerifiedCapsule{}, e
	}
	defer closePinned(mgr)
	if requireIdentity(mgr, s.anchor.Maintainer) != nil {
		return VerifiedCapsule{}, ErrState
	}
	raw, e := readPinned(p)
	if e != nil {
		return VerifiedCapsule{}, e
	}
	c, e := ParseCapsule(bytes.NewReader(raw), Limits{})
	if e != nil {
		return VerifiedCapsule{}, e
	}
	trust := m.trust
	trust.Bootstrap = s.anchor
	v, e := VerifyCapsule(c, trust, s.receipt, m.now())
	if e != nil {
		return VerifiedCapsule{}, e
	}
	return v, nil
}
func (m *Maintainer) Install(ctx context.Context, input VerifiedCapsule) (Receipt, error) {
	if e := ctx.Err(); e != nil {
		return Receipt{}, e
	}
	s, e := m.snapshot()
	if e != nil {
		return Receipt{}, e
	}
	if s.install != nil || s.removal != nil || s.anchor != nil {
		return m.Recover(ctx)
	}
	if absent(s.receipt) {
		return Receipt{}, ErrState
	}
	if e = m.current(s.receipt); e != nil {
		return Receipt{}, e
	}
	c := Capsule{Manifest: input.ManifestBytes(), Signature: input.Signature(), Maintainer: input.Maintainer(), Helper: input.Helper()}
	if len(c.Signature) == 0 {
		return Receipt{}, ErrAuthentication
	}
	v, e := VerifyCapsule(c, m.trust, s.receipt, m.now())
	if e != nil {
		return Receipt{}, e
	}
	return m.stageInstall(ctx, s, v)
}
func (m *Maintainer) stageInstall(ctx context.Context, s protectedState, v VerifiedCapsule) (Receipt, error) {
	if m.now().UnixMilli() < v.AcceptedAt().UnixMilli() {
		return Receipt{}, ErrTime
	}
	// Before durable authority, these exact fixed names hold only non-SUID copies.
	for _, name := range []string{"maintainer.next", "helper.next", "receipt.next", "intent.next"} {
		if e := m.files.discard(stageDir, name, MaxMaintainer, 0o755, 0o644, 0o600); e != nil {
			return Receipt{}, e
		}
	}
	manager, e := m.files.writeNew(stageDir, "maintainer.next", v.Maintainer(), 0o755)
	if e != nil {
		return Receipt{}, e
	}
	defer closePinned(manager)
	helper, e := m.files.writeNew(stageDir, "helper.next", v.Helper(), 0o755)
	if e != nil {
		return Receipt{}, e
	}
	defer closePinned(helper)
	p := InstallIntent{Format: 1, Operation: "install", Generation: v.Manifest().Generation, AcceptedAt: v.AcceptedAt().UnixMilli(), Authorization: "maintenance-signature", Manifest: base64.StdEncoding.EncodeToString(v.ManifestBytes()), Signature: base64.StdEncoding.EncodeToString(v.Signature()), NewMaintainer: manager.identity, NewHelper: helper.identity}
	if s.anchor != nil {
		p.Authorization = "initial-anchor"
	}
	if !absent(s.receipt) {
		d := s.receipt.SHA512()
		p.PreviousReceiptSHA512 = &d
		old, e := m.files.pin(rootDir, "sandbox-maintainer", MaxMaintainer, 0o755)
		if e != nil {
			return Receipt{}, e
		}
		if old == nil {
			return Receipt{}, ErrState
		}
		p.OldMaintainer = &old.identity
		closePinned(old)
		if s.receipt.State == "active" {
			old, e = m.files.pin(rootDir, "chrome-sandbox", MaxHelper, m.files.layout.helperMode)
			if e != nil {
				return Receipt{}, e
			}
			if old == nil {
				return Receipt{}, ErrState
			}
			p.OldHelper = &old.identity
			closePinned(old)
		}
	}
	raw, e := json.Marshal(p)
	if e != nil {
		return Receipt{}, e
	}
	p, e = ParseInstallIntent(raw)
	if e != nil {
		return Receipt{}, e
	}
	s.install = &p
	r := m.installReceipt(s, v)
	reserved, e := m.files.writeReceipt(r)
	if e != nil {
		return Receipt{}, e
	}
	closePinned(reserved)
	intent, e := m.files.writeNew(stageDir, "intent.next", raw, 0o644)
	if e != nil {
		return Receipt{}, e
	}
	defer closePinned(intent)
	if e = m.files.sync(stageDir); e != nil {
		return Receipt{}, e
	}
	if e = m.files.publish("sandbox-transaction.json", "intent.next", nil, intent); e != nil {
		return Receipt{}, e
	}
	return m.finishInstall(ctx, s)
}
func (m *Maintainer) installReceipt(s protectedState, v VerifiedCapsule) Receipt {
	anchor := s.receipt.InitialAnchorSHA512
	if s.anchor != nil {
		anchor = s.anchor.SHA512()
	}
	return Receipt{Format: 1, Authorization: v.Authorization(), InitialAnchorSHA512: anchor, Manifest: base64.StdEncoding.EncodeToString(v.ManifestBytes()), Signature: base64.StdEncoding.EncodeToString(v.Signature()), AcceptedAt: v.AcceptedAt().UnixMilli(), LastOperationAt: m.now().UnixMilli(), MaximumGeneration: v.Manifest().Generation, MaintainerSHA512: v.Manifest().Maintainer.SHA512, HelperSHA512: v.Manifest().Helper.SHA512, CompatibilityEpoch: 1, SandboxAPI: 1, State: "active", CompletedOperation: "install", TransactionSHA512: s.install.SHA512()}
}
func (m *Maintainer) findComponent(current, stage string, id FileIdentity, limit int64) (*pinnedFile, bool, error) {
	p, e := m.files.pin(rootDir, current, limit, 0o755, m.files.layout.helperMode)
	if e != nil {
		return nil, false, e
	}
	if p != nil && identityEqual(p.identity, id) {
		return p, true, nil
	}
	closePinned(p)
	p, e = m.files.pin(stageDir, stage, limit, 0o755, m.files.layout.helperMode)
	if e != nil {
		return nil, false, e
	}
	if requireIdentity(p, id) != nil {
		closePinned(p)
		return nil, false, ErrState
	}
	return p, false, nil
}
func (m *Maintainer) finishInstall(ctx context.Context, s protectedState) (Receipt, error) {
	p := s.install
	if m.now().UnixMilli() < p.AcceptedAt {
		return Receipt{}, ErrTime
	}
	manager, inRoot, e := m.findComponent("sandbox-maintainer", "maintainer.next", p.NewMaintainer, MaxMaintainer)
	if e != nil {
		return Receipt{}, e
	}
	defer closePinned(manager)
	helper, helperRoot, e := m.findComponent("chrome-sandbox", "helper.next", p.NewHelper, MaxHelper)
	if e != nil {
		return Receipt{}, e
	}
	defer closePinned(helper)
	raw, sig, _, e := intentManifest(*p)
	if e != nil {
		return Receipt{}, e
	}
	mb, e := readPinned(manager)
	if e != nil {
		return Receipt{}, e
	}
	hb, e := readPinned(helper)
	if e != nil {
		return Receipt{}, e
	}
	trust := m.trust
	trust.Pending = p
	trust.Bootstrap = s.anchor
	v, e := VerifyCapsule(Capsule{Manifest: raw, Signature: sig, Maintainer: mb, Helper: hb}, trust, s.receipt, m.now())
	if e != nil {
		return Receipt{}, e
	}
	if e = ctx.Err(); e != nil {
		return Receipt{}, e
	}
	if !inRoot {
		old, e := m.files.pin(rootDir, "sandbox-maintainer", MaxMaintainer, 0o755)
		if e != nil {
			return Receipt{}, e
		}
		defer closePinned(old)
		if p.OldMaintainer != nil {
			if requireIdentity(old, *p.OldMaintainer) != nil {
				return Receipt{}, ErrState
			}
		} else if old != nil {
			return Receipt{}, ErrState
		}
		if e = m.files.publish("sandbox-maintainer", "maintainer.next", old, manager); e != nil {
			return Receipt{}, e
		}
	} else {
		// Visibility can follow a successful rename whose directory flush failed.
		// Re-establish durability before revoking the previous helper inode.
		if e = m.files.sync(stageDir); e != nil {
			return Receipt{}, e
		}
		if e = m.files.sync(rootDir); e != nil {
			return Receipt{}, e
		}
	}
	if !helperRoot {
		old, e := m.files.pin(rootDir, "chrome-sandbox", MaxHelper, 0o755, m.files.layout.helperMode)
		if e != nil {
			return Receipt{}, e
		}
		defer closePinned(old)
		if p.OldHelper != nil {
			if requireIdentity(old, *p.OldHelper) != nil {
				return Receipt{}, ErrState
			}
			if e = m.files.changeMode(old, 0o755, "old-helper"); e != nil {
				return Receipt{}, e
			}
		} else if old != nil {
			return Receipt{}, ErrState
		}
		if e = m.files.changeMode(helper, m.files.layout.helperMode, "new-helper"); e != nil {
			return Receipt{}, e
		}
		if e = m.files.publish("chrome-sandbox", "helper.next", old, helper); e != nil {
			return Receipt{}, e
		}
	} else {
		if e = m.files.sync(stageDir); e != nil {
			return Receipt{}, e
		}
		if e = m.files.sync(rootDir); e != nil {
			return Receipt{}, e
		}
	}
	r := m.installReceipt(s, v)
	if e = m.commitReceipt(r); e != nil {
		return Receipt{}, e
	}
	s.receipt = r
	if e = m.cleanup(s); e != nil {
		return Receipt{}, e
	}
	return r, nil
}
func (m *Maintainer) commitReceipt(r Receipt) error {
	next, e := m.files.writeReceipt(r)
	if e != nil {
		return e
	}
	defer closePinned(next)
	old, e := m.files.pin(rootDir, "sandbox-receipt.json", recordLimit, 0o644)
	if e != nil {
		return e
	}
	defer closePinned(old)
	return m.files.publish("sandbox-receipt.json", "receipt.next", old, next)
}
func (m *Maintainer) cleanup(s protectedState) error {
	if e := m.current(s.receipt); e != nil {
		return e
	}
	// A visible receipt from an interrupted rename is not yet a completed
	// directory flush. Make that one-way publication durable before retiring
	// the older immutable intent; on failure retain all recovery authority.
	if e := m.files.sync(stageDir); e != nil {
		return e
	}
	if e := m.files.sync(rootDir); e != nil {
		return e
	}

	// Terminal authority binds this exact operation, not merely its generation.
	if s.install != nil && (s.receipt.CompletedOperation != "install" || s.receipt.TransactionSHA512 != s.install.SHA512()) {
		return ErrState
	}
	if s.removal != nil && (s.receipt.CompletedOperation != "remove" || s.receipt.TransactionSHA512 != s.removal.SHA512()) {
		return ErrState
	}
	for _, entry := range []struct {
		name  string
		id    *FileIdentity
		limit int64
	}{{"maintainer.next", nil, MaxMaintainer}, {"helper.next", nil, MaxHelper}} {
		if s.install != nil {
			if entry.name == "maintainer.next" {
				entry.id = s.install.OldMaintainer
			} else {
				entry.id = s.install.OldHelper
			}
		}
		p, e := m.files.pin(stageDir, entry.name, entry.limit, 0o755)
		if e != nil {
			return e
		}
		if p != nil {
			if entry.id == nil || requireIdentity(p, *entry.id) != nil {
				closePinned(p)
				return ErrState
			}
			e = m.files.unlink(stageDir, entry.name, p)
			closePinned(p)
			if e != nil {
				return e
			}
		}
	}
	// The exchanged previous receipt is non-executable, but must itself be a
	// valid protected record. Never recursively remove unrecognized children.
	p, e := m.files.pin(stageDir, "receipt.next", recordLimit, 0o644)
	if e != nil {
		return e
	}
	if p != nil {
		raw, e := readPinned(p)
		if e != nil {
			closePinned(p)
			return e
		}
		if _, e = ParseReceipt(raw); e != nil {
			closePinned(p)
			return e
		}
		e = m.files.unlink(stageDir, "receipt.next", p)
		closePinned(p)
		if e != nil {
			return e
		}
	}
	if s.install != nil || s.removal != nil {
		p, e = m.files.pin(rootDir, "sandbox-transaction.json", recordLimit, 0o644)
		if e != nil {
			return e
		}
		if p != nil {
			expected := s.receipt.TransactionSHA512
			if p.identity.SHA512 != expected {
				closePinned(p)
				return ErrState
			}
			e = m.files.unlink(rootDir, "sandbox-transaction.json", p)
			closePinned(p)
			if e != nil {
				return e
			}
		}
	}
	if s.anchor != nil {
		if s.anchor.SHA512() != s.receipt.InitialAnchorSHA512 {
			return ErrState
		}
		for _, entry := range []struct {
			name  string
			id    FileIdentity
			limit int64
			mode  uint32
		}{{"bootstrap-capsule", s.anchor.Capsule, MaxCapsule, 0o600}, {"bootstrap-maintainer", s.anchor.Maintainer, MaxMaintainer, 0o700}} {
			p, e = m.files.pin(stageDir, entry.name, entry.limit, entry.mode)
			if e != nil {
				return e
			}
			if p != nil {
				if requireIdentity(p, entry.id) != nil {
					closePinned(p)
					return ErrState
				}
				e = m.files.unlink(stageDir, entry.name, p)
				closePinned(p)
				if e != nil {
					return e
				}
			}
		}
		p, e = m.files.pin(stageDir, "bootstrap-anchor.json", recordLimit, 0o600)
		if e != nil {
			return e
		}
		if p != nil {
			if p.identity.SHA512 != s.anchor.SHA512() {
				closePinned(p)
				return ErrState
			}
			e = m.files.unlink(stageDir, "bootstrap-anchor.json", p)
			closePinned(p)
			if e != nil {
				return e
			}
		}
	}
	return nil
}
func (m *Maintainer) Remove(ctx context.Context) error {
	pending, e := m.NeedsRecovery()
	if e != nil {
		return e
	}
	if pending {
		_, e = m.Recover(ctx)
		return e
	}
	s, e := m.snapshot()
	if e != nil {
		return e
	}
	if absent(s.receipt) {
		return ErrState
	}
	if e = m.current(s.receipt); e != nil {
		return e
	}
	if s.receipt.State == "removed" {
		return nil
	}
	if m.now().UnixMilli() < s.receipt.LastOperationAt {
		return ErrTime
	}
	manager, e := m.files.pin(rootDir, "sandbox-maintainer", MaxMaintainer, 0o755)
	if e != nil {
		return e
	}
	defer closePinned(manager)
	helper, e := m.files.pin(rootDir, "chrome-sandbox", MaxHelper, m.files.layout.helperMode)
	if e != nil {
		return e
	}
	defer closePinned(helper)
	if manager == nil || helper == nil {
		return ErrState
	}
	for _, name := range []string{"receipt.next", "intent.next"} {
		if e = m.files.discard(stageDir, name, recordLimit, 0o600, 0o644); e != nil {
			return e
		}
	}
	p := RemovalIntent{Format: 1, Operation: "remove", MaximumGeneration: s.receipt.MaximumGeneration, AcceptedAt: m.now().UnixMilli(), PreviousReceiptSHA512: s.receipt.SHA512(), CurrentMaintainer: manager.identity, CurrentHelper: &helper.identity}
	raw, e := json.Marshal(p)
	if e != nil {
		return e
	}
	p, e = ParseRemovalIntent(raw)
	if e != nil {
		return e
	}
	s.removal = &p
	r := m.removedReceipt(s)
	reserved, e := m.files.writeReceipt(r)
	if e != nil {
		return e
	}
	closePinned(reserved)
	intent, e := m.files.writeNew(stageDir, "intent.next", raw, 0o644)
	if e != nil {
		return e
	}
	defer closePinned(intent)
	if e = m.files.sync(stageDir); e != nil {
		return e
	}
	if e = m.files.publish("sandbox-transaction.json", "intent.next", nil, intent); e != nil {
		return e
	}
	_, e = m.finishRemoval(ctx, s)
	return e
}
func (m *Maintainer) removedReceipt(s protectedState) Receipt {
	r := s.receipt
	r.State = "removed"
	r.CompletedOperation = "remove"
	r.TransactionSHA512 = s.removal.SHA512()
	r.LastOperationAt = m.now().UnixMilli()
	return r
}
func (m *Maintainer) finishRemoval(ctx context.Context, s protectedState) (Receipt, error) {
	p := s.removal
	if absent(s.receipt) || p.PreviousReceiptSHA512 != s.receipt.SHA512() || p.MaximumGeneration != s.receipt.MaximumGeneration || p.AcceptedAt < s.receipt.LastOperationAt || m.now().UnixMilli() < p.AcceptedAt {
		return Receipt{}, ErrState
	}
	manager, e := m.files.pin(rootDir, "sandbox-maintainer", MaxMaintainer, 0o755)
	if e != nil {
		return Receipt{}, e
	}
	defer closePinned(manager)
	if requireIdentity(manager, p.CurrentMaintainer) != nil || manager.identity.SHA512 != s.receipt.MaintainerSHA512 {
		return Receipt{}, ErrState
	}
	if e = ctx.Err(); e != nil {
		return Receipt{}, e
	}
	helper, e := m.files.pin(rootDir, "chrome-sandbox", MaxHelper, 0o755, m.files.layout.helperMode)
	if e != nil {
		return Receipt{}, e
	}
	defer closePinned(helper)
	if helper != nil {
		if p.CurrentHelper == nil || requireIdentity(helper, *p.CurrentHelper) != nil || helper.identity.SHA512 != s.receipt.HelperSHA512 {
			return Receipt{}, ErrState
		}
		if e = m.files.changeMode(helper, 0o755, "old-helper"); e != nil {
			return Receipt{}, e
		}
		if e = m.files.unlink(rootDir, "chrome-sandbox", helper); e != nil {
			return Receipt{}, e
		}
	} else {
		// An observed absence does not prove the interrupted unlink was durable.
		if e = m.files.sync(rootDir); e != nil {
			return Receipt{}, e
		}
	}
	r := m.removedReceipt(s)
	if e = m.commitReceipt(r); e != nil {
		return Receipt{}, e
	}
	s.receipt = r
	if e = m.cleanup(s); e != nil {
		return Receipt{}, e
	}
	return r, nil
}
