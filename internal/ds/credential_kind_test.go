package ds_test

// Task S1: a leaf credential's `kind` is self-signed by its own user's SSK, so the delivery service
// binds it to the account it names at every point a new leaf reaches it. A human account may not
// present a Bot-kind leaf (it would render as a Bot to every client that reads the sender's kind
// from its credential), and a Bot account may not present a human-kind one. Each entry point is
// pinned twice: the mismatched kind is refused, the matching kind still passes.

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"

	"github.com/jonasthim/dilla/internal/auth"
	"github.com/jonasthim/dilla/internal/cborx"
	"github.com/jonasthim/dilla/internal/ds"
	"github.com/jonasthim/dilla/internal/id"
	"github.com/jonasthim/dilla/internal/mlswasi"
	"github.com/jonasthim/dilla/internal/store"
)

// withCredentialKind re-encodes a ten-element CredentialIdentity with element 4 (`kind`) replaced:
// the shape a modified client signs with its own SSK. The delivery service does not verify the
// credential's signatures at these clauses (the guest did), so the bytes need only decode.
func withCredentialKind(t *testing.T, identity []byte, kind any) []byte {
	t.Helper()
	var elems []cbor.RawMessage
	if err := cborx.Unmarshal(identity, &elems); err != nil || len(elems) != 10 {
		t.Fatalf("decode credential identity: %v (%d elements)", err, len(elems))
	}
	k, err := cborx.Marshal(kind)
	if err != nil {
		t.Fatalf("encode kind: %v", err)
	}
	elems[4] = k
	out, err := cborx.Marshal(elems)
	if err != nil {
		t.Fatalf("encode credential identity: %v", err)
	}
	return out
}

func setUserKind(t *testing.T, h *dsHarness, user id.ID, kind uint8) {
	t.Helper()
	db, err := sql.Open("sqlite", h.path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.ExecContext(context.Background(), `UPDATE users SET kind=? WHERE id=?`, kind, user); err != nil {
		t.Fatal(err)
	}
}

func TestDecodeCredentialKindFailsClosed(t *testing.T) {
	h := newDSHarness(t)
	reg, _ := h.mustRegister(t)
	identity, _, _ := h.memberIdentity(t, reg.GroupID, 1)

	if k, err := ds.DecodeCredentialKindForTest(identity); err != nil || k != 0 {
		t.Fatalf("the fixture member's credential: kind %d, %v; want human (0)", k, err)
	}
	if k, err := ds.DecodeCredentialKindForTest(withCredentialKind(t, identity, uint64(1))); err != nil || k != 1 {
		t.Fatalf("a Bot-kind credential: kind %d, %v; want 1", k, err)
	}
	for name, bad := range map[string][]byte{
		"kind 2":           withCredentialKind(t, identity, uint64(2)),
		"negative kind":    withCredentialKind(t, identity, int64(-1)),
		"a text kind":      withCredentialKind(t, identity, "bot"),
		"not an array":     {0x01},
		"nine elements":    mustMarshal(t, make([]any, 9)),
		"empty":            nil,
		"trailing garbage": append(append([]byte{}, identity...), 0x00),
	} {
		if _, err := ds.DecodeCredentialKindForTest(bad); err == nil {
			t.Errorf("%s: decoded, want an error", name)
		}
	}
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := cborx.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The rule on its own: a human account (users.kind 0) presents only a human-kind credential; an
// unknown account is a refusal; a Bot account is left to the managed-Bot clause (it must present
// the exact credential its installation verified), so this rule passes either kind for it.
func TestCheckCredentialKindBindsTheCredentialToTheAccountKind(t *testing.T) {
	h := newDSHarness(t)
	ctx := context.Background()
	reg, _ := h.mustRegister(t)
	identity, device, user := h.memberIdentity(t, reg.GroupID, 1)
	h.account(t, user, device) // users.kind = 0
	bot := withCredentialKind(t, identity, uint64(1))

	if err := ds.CheckCredentialKindForTest(h.ds, ctx, user, identity); err != nil {
		t.Fatalf("human account, human credential: %v", err)
	}
	if err := ds.CheckCredentialKindForTest(h.ds, ctx, user, bot); !errors.Is(err, ds.ErrCredentialKindForTest) {
		t.Fatalf("human account, Bot credential: %v, want the kind refusal", err)
	}
	if err := ds.CheckCredentialKindForTest(h.ds, ctx, user, withCredentialKind(t, identity, uint64(2))); !errors.Is(err, ds.ErrCredentialKindForTest) {
		t.Fatalf("human account, unreadable kind: %v, want the kind refusal", err)
	}
	if err := ds.CheckCredentialKindForTest(h.ds, ctx, id.New(), identity); !errors.Is(err, ds.ErrCredentialKindForTest) {
		t.Fatalf("unknown account: %v, want the kind refusal", err)
	}
	setUserKind(t, h, user, 1)
	if err := ds.CheckCredentialKindForTest(h.ds, ctx, user, bot); err != nil {
		t.Fatalf("Bot account, Bot credential: %v", err)
	}
	if err := ds.CheckCredentialKindForTest(h.ds, ctx, user, withCredentialKind(t, identity, uint64(2))); !errors.Is(err, ds.ErrCredentialKindForTest) {
		t.Fatalf("Bot account, unreadable kind: %v, want the kind refusal", err)
	}
}

// nativeBotAccount registers the committed native Bot KeyPackages' identity as an account of the
// given users.kind: its device under the fixture's DSK, listed in a device list signed under a
// test SSK the users row holds (the delivery service verifies a list against users.ssk_pub and
// never compares it with a credential's own SSK). With kind 0 it is a human account holding
// Bot-kind packages, the shape a modified client produces.
func (h *dsHarness) nativeBotAccount(t *testing.T, kind uint8) (id.ID, id.ID, []nativeBotKeyPackageBatch) {
	t.Helper()
	fixture := loadNativeBotKeyPackageFixture(t)
	device, user, _, batches := fixture.rows(t)
	ssk := testSSK(0x5e)
	h.userWithSSK(t, user, ssk)
	setUserKind(t, h, user, kind)
	dsk := fixture.decode(t, fixture.DSKPublicHex)
	h.accountWithKey(t, user, device, dsk)
	h.publishDeviceList(t, user, signedDeviceList(t, ssk, user, []listEntry{{DeviceID: device[:], DSKPub: dsk, AddedAt: 1}}))
	return device, user, batches
}

// Entry point 1: POST /v1/keypackages on the human route. The committed native Bot KeyPackages
// carry a Bot-kind credential; a HUMAN account registered under the fixture's identity and key
// publishing one is refused and nothing is stored. The same packages under a Bot-kind account on
// the same route are accepted, so the refusal is the kind.
func TestPublishRefusesABotKindKeyPackageFromAHumanAccount(t *testing.T) {
	for _, c := range []struct {
		name string
		kind uint8
	}{{"human account", 0}, {"Bot account", 1}} {
		t.Run(c.name, func(t *testing.T) {
			h := newDSHarness(t)
			device, user, batches := h.nativeBotAccount(t, c.kind)
			session := auth.Session{UserID: user, DeviceID: device, Scope: auth.ScopeEnrolled}

			_, err := h.ds.PublishKeyPackages(context.Background(), session, [][]byte{batches[0].Ordinary[0].blob(t)}, nil)
			if c.kind == 1 {
				if err != nil {
					t.Fatalf("a Bot account publishing its own Bot-kind package: %v", err)
				}
				return
			}
			var dsErr *ds.Error
			if !errors.As(err, &dsErr) || dsErr.Code != "E_COMMIT_INVALID" || dsErr.Rule != "key_package" ||
				!strings.Contains(dsErr.Detail, "kind") {
				t.Fatalf("got %v, want E_COMMIT_INVALID/key_package naming the kind", err)
			}
			if total := h.countRows(t, "key_packages"); total != 0 {
				t.Fatalf("%d rows were written for a refused publish, want 0", total)
			}
		})
	}
}

// Entry point 2: an Add a commit applies (invariant 4's Add clause). A known, listed, eligible
// human device whose Add carries a Bot-kind credential is refused, as is an unreadable kind; the
// honest Add passes.
func TestAnAddWhoseCredentialKindIsNotTheAccountsIsRefused(t *testing.T) {
	h := newDSHarness(t)
	reg, _ := h.mustRegister(t)
	identity, device, user := h.memberIdentity(t, reg.GroupID, 1)
	ssk := testSSK(0x5d)
	h.userWithSSK(t, user, ssk)
	dsk := h.memberLeafKey(t, reg.GroupID, 1)
	h.accountWithKey(t, user, device, dsk)
	h.publishDeviceList(t, user, signedDeviceList(t, ssk, user, []listEntry{{DeviceID: device[:], DSKPub: dsk, AddedAt: 1}}))

	forged := mlswasi.AppliedProposal{Kind: mlswasi.ProposalAdd, CredentialIdentity: withCredentialKind(t, identity, uint64(1)), SignatureKey: dsk}
	if err := ds.CheckAddedMemberForTest(h.ds, context.Background(), reg.GroupID, forged); !hasRule(err, "add_key_package") {
		t.Fatalf("a human's Bot-kind Add: got %v, want add_key_package", err)
	}
	unreadable := mlswasi.AppliedProposal{Kind: mlswasi.ProposalAdd, CredentialIdentity: withCredentialKind(t, identity, uint64(7)), SignatureKey: dsk}
	if err := ds.CheckAddedMemberForTest(h.ds, context.Background(), reg.GroupID, unreadable); !hasRule(err, "add_key_package") {
		t.Fatalf("an unreadable kind: got %v, want add_key_package", err)
	}
	honest := mlswasi.AppliedProposal{Kind: mlswasi.ProposalAdd, CredentialIdentity: identity, SignatureKey: dsk}
	if err := ds.CheckAddedMemberForTest(h.ds, context.Background(), reg.GroupID, honest); err != nil {
		t.Fatalf("the honest Add: %v", err)
	}
}

// Entry point 3: the leaf an external commit creates (join or resync).
func TestAnExternalJoinerWhoseCredentialKindIsNotTheAccountsIsRefused(t *testing.T) {
	h := newDSHarness(t)
	got, _ := h.mustRegister(t)
	h.enrolLeaves(t, got.GroupID, nil)
	identity, device, user := h.memberIdentity(t, got.GroupID, 3)
	dsk := h.memberLeafKey(t, got.GroupID, 3)
	s := auth.Session{UserID: user, DeviceID: device, Scope: auth.ScopeEnrolled}
	state := func(cred []byte) mlswasi.GroupState {
		return mlswasi.GroupState{Members: []mlswasi.Member{{LeafIndex: 1_500, SignatureKey: dsk, CredentialIdentity: cred}}}
	}
	for _, joining := range []bool{true, false} {
		err := ds.CheckExternalJoinerForTest(h.ds, context.Background(), s, got.GroupID, state(withCredentialKind(t, identity, uint64(1))), 1_500, joining)
		if !hasRule(err, "external_joiner") {
			t.Fatalf("joining=%t, a human's Bot-kind leaf: got %v, want external_joiner", joining, err)
		}
		if err := ds.CheckExternalJoinerForTest(h.ds, context.Background(), s, got.GroupID, state(identity), 1_500, joining); err != nil {
			t.Fatalf("joining=%t, the honest leaf: %v", joining, err)
		}
	}
}

// Entry point 4: a replayed resync in a heal (checkHealedExternalCommit). The device lands back
// on its registered key, but its credential claims Bot kind for a human account: refused; the
// honest credential passes.
func TestAReplayedResyncWhoseCredentialKindIsNotTheAccountsIsRefused(t *testing.T) {
	h := newDSHarness(t)
	got, _ := h.mustRegister(t)
	members, err := h.repo.ListMembers(context.Background(), got.GroupID)
	if err != nil {
		t.Fatalf("ListMembers: %v", err)
	}
	var joiner store.MemberRow
	before := map[uint32][2]id.ID{}
	for _, m := range members {
		if m.LeafIndex == 3 {
			joiner = m
		}
		before[m.LeafIndex] = [2]id.ID{m.DeviceID, m.UserID}
	}
	h.enrolLeaves(t, got.GroupID, nil)
	identity, _, _ := h.memberIdentity(t, got.GroupID, 3)
	removed := joiner.LeafIndex
	applied := []mlswasi.AppliedProposal{{Kind: mlswasi.ProposalRemove, TargetLeaf: &removed}}
	after := func(cred []byte) mlswasi.GroupState {
		return mlswasi.GroupState{Members: []mlswasi.Member{{LeafIndex: 1_500, SignatureKey: joiner.SignatureKey, CredentialIdentity: cred}}}
	}
	if _, err := ds.CheckHealedExternalCommitForTest(h.ds, context.Background(), got.GroupID, before,
		after(withCredentialKind(t, identity, uint64(1))), applied); !hasRule(err, "external_joiner") {
		t.Fatalf("a replayed resync with a human's Bot-kind credential: got %v, want external_joiner", err)
	}
	if device, err := ds.CheckHealedExternalCommitForTest(h.ds, context.Background(), got.GroupID, before,
		after(identity), applied); err != nil || device != joiner.DeviceID {
		t.Fatalf("the honest replayed resync: %v, %v", device, err)
	}
}

// Entry point 5: a directory KeyPackage the instance spends on its own Add. A package stored before
// the kind binding existed — a human account's Bot-kind package — is dropped before it is spent:
// the "no usable KeyPackage" refusal, and the row is deleted. The same package under a Bot account
// is proposed, so the refusal is the kind.
func TestProposeAddDropsADirectoryPackageWhoseKindIsNotTheAccounts(t *testing.T) {
	for _, c := range []struct {
		name string
		kind uint8
	}{{"human account", 0}, {"Bot account", 1}} {
		t.Run(c.name, func(t *testing.T) {
			h := newDSHarness(t)
			ctx := context.Background()
			reg, _ := h.mustRegister(t)
			device, _, batches := h.nativeBotAccount(t, c.kind)
			h.putDirectoryPackage(t, device, batches[0].Ordinary[0].blob(t), 0, 80*24*time.Hour)

			err := h.ds.ProposeAdd(ctx, reg.GroupID, device, id.New())
			if c.kind == 1 {
				if err != nil {
					t.Fatalf("the Bot account's own package: %v", err)
				}
				return
			}
			var dsErr *ds.Error
			if !errors.As(err, &dsErr) || dsErr.Code != "E_INVALID_REQUEST" ||
				!strings.Contains(dsErr.Detail, "no usable KeyPackage") {
				t.Fatalf("got %v, want E_INVALID_REQUEST: no usable KeyPackage", err)
			}
			if got := h.countRows(t, "key_packages"); got != 0 {
				t.Fatalf("key_packages holds %d rows, want 0: the mismatched package must be deleted", got)
			}
		})
	}
}

// The registered leaf (checkRegisteredLeaf) and a reseeded tree (checkReseededLeaves) call the same
// checkCredentialKind after their device clauses; no committed group fixture holds a Bot-kind
// leaf, so those two are pinned through the rule above rather than end to end.
