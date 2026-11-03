package ds_test

// register_external_senders_test.go is DS-MEMBERSHIP-01 of the web-1 branch review: registration
// and heal's reseed adopt a group whose `external_senders` extension the registrant chose. OpenMLS
// verifies every external proposal against that extension, so a `text` or `call` group naming a key
// other than the instance's is one the instance can never propose into: the registration Add storm,
// every kick, ban, revocation, inactivity and election Remove fail at `ProposalPut`, and a member who
// is later banned keeps its leaf for the group's life. Protocol/02 invariant 1 now refuses it: a
// `text` or `call` group must carry exactly one external sender, this instance's key under its
// `[1, "instance", instance_id]` credential, and a `pairing` or `interaction` group none.
//
// The relation the attack creates - the group's extension names one key, the instance signs with
// another - is reproduced here without a patched client: the committed fixtures name the fixture
// generator's instance key (fixtureExternalSenderKey), and the delivery service under test is built
// with that key perturbed by one byte. The same request under the honest key is the positive
// control in each test.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"net/http"
	"testing"

	"github.com/dilla-chat/dilla/internal/ds"
	"github.com/dilla-chat/dilla/internal/mlswasi"
	"github.com/dilla-chat/dilla/internal/store"
)

// otherSenderKeys is testInstanceKeys with the external-sender signing key perturbed by one byte:
// the instance this DS is no longer matches the key the fixtures' groups name.
func otherSenderKeys(tb testing.TB) ds.InstanceKeys {
	tb.Helper()
	keys := testInstanceKeys(tb)
	keys.ExternalSenderPriv[0] ^= 0x01
	return keys
}

// restartDSWithKeys is restartDS with other instance keys: the same database, clock, wasm runtime,
// gateway, channel source and ACL, as a process restart that loaded another key would leave them.
func (h *dsHarness) restartDSWithKeys(keys ds.InstanceKeys) {
	h.t.Helper()
	if err := h.ds.Shutdown(context.Background()); err != nil {
		h.t.Fatalf("Shutdown: %v", err)
	}
	d, err := ds.New(ds.Options{
		Store: h.repo, Wasm: h.wasm, Gateway: h.gw, Clock: h.clk,
		Keys: keys, Policy: ds.DefaultPolicy(), Channels: h.channels, ACL: h.acl,
	})
	if err != nil {
		h.t.Fatalf("ds.New: %v", err)
	}
	h.t.Cleanup(func() { _ = d.Shutdown(context.Background()) })
	h.ds = d
}

// refusedForItsExternalSenders asserts 400 E_BINDING_INVALID and that nothing was written.
func (h *dsHarness) refusedForItsExternalSenders(t *testing.T, what string, r ds.RegisterRequest) {
	t.Helper()
	_, err := h.ds.Register(context.Background(), r)
	var dsErr *ds.Error
	if !errors.As(err, &dsErr) || dsErr.Code != "E_BINDING_INVALID" || dsErr.Status != http.StatusBadRequest {
		t.Fatalf("%s: got %v, want 400 E_BINDING_INVALID", what, err)
	}
	if _, err := h.repo.GetGroup(context.Background(), r.GroupID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("%s: a refused registration left a group row (%v)", what, err)
	}
	members, err := h.repo.ListMembers(context.Background(), r.GroupID)
	if err != nil {
		t.Fatalf("ListMembers: %v", err)
	}
	if len(members) != 0 {
		t.Fatalf("%s: a refused registration left %d member rows", what, len(members))
	}
}

// A text group whose external_senders names a key that is not this instance's is refused with
// 400 E_BINDING_INVALID, and nothing is stored. The positive control is the same honest one-leaf
// registration on an instance whose key the extension does name.
func TestRegisterRefusesATextGroupWhoseExternalSenderIsNotThisInstancesKey(t *testing.T) {
	honest := newDSHarness(t)
	if _, err := honest.ds.Register(context.Background(),
		registration(t, "one-leaf-0", honest.honestRegistrant(t))); err != nil {
		t.Fatalf("the instance whose key the group names must accept it: %v", err)
	}

	h := newDSHarness(t)
	h.restartDSWithKeys(otherSenderKeys(t))
	h.refusedForItsExternalSenders(t, "another instance key in external_senders",
		registration(t, "one-leaf-0", h.honestRegistrant(t)))
}

// A pairing group carries no external sender (protocol/01 § External senders): one that does is
// refused with 400 E_BINDING_INVALID. The positive control is the same creator's pairing group
// without one, which registers.
func TestRegisterRefusesAPairingGroupThatCarriesAnExternalSender(t *testing.T) {
	h := newDSHarness(t)
	s := h.honestRegistrant(t)
	h.refusedForItsExternalSenders(t, "a pairing group with the instance as external sender",
		registration(t, "pairing-sender", s))
	if _, err := h.ds.Register(context.Background(), registration(t, "pairing", s)); err != nil {
		t.Fatalf("a pairing group without an external sender: %v", err)
	}
}

// The rule's every branch, over the entries a patched client could build. The honest entry is the
// instance's: basic, [1, "instance", 0x11 * 16], the public half of the DS's signing key.
func TestTheExternalSenderRuleAdmitsOnlyTheInstancesOneEntry(t *testing.T) {
	h := newDSHarness(t)
	seed := testInstanceKeys(t).ExternalSenderPriv
	key := []byte(ed25519.NewKeyFromSeed(seed[:])[ed25519.SeedSize:])
	credential := append([]byte{0x83, 0x01, 0x68}, "instance"...)
	credential = append(credential, 0x50)
	credential = append(credential, bytes.Repeat([]byte{0x11}, 16)...)
	honest := mlswasi.ExternalSender{CredentialType: 1, Credential: credential, SignatureKey: key}
	with := func(change func(s *mlswasi.ExternalSender)) mlswasi.ExternalSender {
		s := honest
		s.Credential = bytes.Clone(honest.Credential)
		s.SignatureKey = bytes.Clone(honest.SignatureKey)
		change(&s)
		return s
	}
	const text, call, pairing, interaction = 0, 1, 2, 3
	for _, c := range []struct {
		name    string
		kind    uint8
		senders []mlswasi.ExternalSender
		ok      bool
	}{
		{"text, the instance's one entry", text, []mlswasi.ExternalSender{honest}, true},
		{"call, the instance's one entry", call, []mlswasi.ExternalSender{honest}, true},
		{"pairing, none", pairing, nil, true},
		{"interaction, none", interaction, nil, true},
		{"text, none", text, nil, false},
		{"call, none", call, nil, false},
		{"text, the instance and a second sender", text, []mlswasi.ExternalSender{honest, with(func(s *mlswasi.ExternalSender) { s.SignatureKey[0] ^= 1 })}, false},
		{"text, another key", text, []mlswasi.ExternalSender{with(func(s *mlswasi.ExternalSender) { s.SignatureKey[31] ^= 1 })}, false},
		{"text, another instance's credential", text, []mlswasi.ExternalSender{with(func(s *mlswasi.ExternalSender) { s.Credential[len(s.Credential)-1] ^= 1 })}, false},
		{"text, the right identity in a non-basic credential", text, []mlswasi.ExternalSender{with(func(s *mlswasi.ExternalSender) { s.CredentialType = 2 })}, false},
		{"pairing, the instance's entry", pairing, []mlswasi.ExternalSender{honest}, false},
		{"interaction, the instance's entry", interaction, []mlswasi.ExternalSender{honest}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			problem := ds.ExternalSendersProblemForTest(h.ds, c.kind, c.senders)
			if c.ok && problem != "" {
				t.Fatalf("refused: %s", problem)
			}
			if !c.ok && problem == "" {
				t.Fatal("admitted")
			}
		})
	}
}

// Heal's reseed adopts a member's uploaded tree, so it applies invariant 1's external-sender rule
// too: a reseeded text group whose extension names another key than this instance's is refused
// with E_COMMIT_INVALID, rule "reseed" (the answer the reseed gives a binding that is not the
// group's), and the group stays as the restore left it. The positive control is the same reseed
// on an instance whose key the tree names, restarted the same way.
func TestAReseedRefusesATreeWhoseExternalSenderIsNotThisInstancesKey(t *testing.T) {
	for _, c := range []struct {
		name   string
		keys   func(tb testing.TB) ds.InstanceKeys
		refuse bool
	}{
		{"the instance's own key", testInstanceKeys, false},
		{"another instance key", otherSenderKeys, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newDSHarness(t)
			g := h.enrolledGroup(t)
			members, err := h.repo.ListMembers(context.Background(), g.id)
			if err != nil {
				t.Fatalf("ListMembers: %v", err)
			}
			tree, groupInfo := h.restoreWithoutBlob(t, g)
			h.restartDSWithKeys(c.keys(t))

			_, err = h.ds.Heal(context.Background(), g.session, g.id, ds.HealRequest{
				GroupInfo: groupInfo, RatchetTree: tree,
			})
			if !c.refuse {
				if err != nil {
					t.Fatalf("the honest reseed: %v", err)
				}
				if h.epochUnknown(t, g.id) {
					t.Error("an accepted reseed clears epoch_unknown")
				}
				return
			}
			if !hasRule(err, "reseed") {
				t.Fatalf("got %v, want E_COMMIT_INVALID/reseed", err)
			}
			h.refusedReseed(t, g, len(members))
		})
	}
}

// contextChangeCases are fix wave C's two commits of the registration fixture, each by the creator
// of an honestly registered one-leaf text group at its creation epoch: an honest self-update, and a
// GroupContextExtensions commit that keeps required_capabilities and dilla_binding and swaps
// external_senders to the creator's own key - registration's rule, sidestepped after the fact.
var contextChangeCases = []struct {
	name   string
	group  string
	refuse bool
}{
	{"an honest self-update", "self-update", false},
	{"a GroupContextExtensions commit that swaps external_senders", "gce-swap", true},
}

// The group context's extensions never change after creation: POST /commit refuses a member
// commit that carries a GroupContextExtensions proposal (422 E_COMMIT_INVALID, rule "structural",
// the core's refusal at process time) and stores nothing; the honest self-update of the same shape
// of group is accepted.
func TestACommitThatChangesTheGroupContextExtensionsIsRefused(t *testing.T) {
	for _, c := range contextChangeCases {
		t.Run(c.name, func(t *testing.T) {
			h := newDSHarness(t)
			reg, s := h.mustRegisterOneLeaf(t, c.group)
			g := registrationFixture(t).groups[c.group]
			if len(g.commit) == 0 || len(g.commitGroupInfo) == 0 {
				t.Fatalf("the fixture's %s group carries no commit", c.group)
			}
			_, err := h.ds.Commit(context.Background(), s, reg.GroupID, ds.CommitRequest{
				Epoch: 0, Commit: g.commit, GroupInfo: g.commitGroupInfo,
			})
			if !c.refuse {
				if err != nil {
					t.Fatalf("the honest commit: %v", err)
				}
				if got := h.groupRow(t, reg.GroupID).Epoch; got != 1 {
					t.Fatalf("epoch after the honest commit = %d, want 1", got)
				}
				return
			}
			if !hasRule(err, "structural") {
				t.Fatalf("got %v, want E_COMMIT_INVALID/structural", err)
			}
			if got := h.groupRow(t, reg.GroupID).Epoch; got != 0 {
				t.Errorf("a refused commit moved the group to epoch %d", got)
			}
			if got := h.handshakeCount(t, reg.GroupID); got != 0 {
				t.Errorf("a refused commit wrote %d handshake rows", got)
			}
		})
	}
}

// Heal's replay (the instance kept its state blob, so no reseed) processes every tail commit
// through the same core check: a tail carrying the GroupContextExtensions commit is refused with
// rule "tail" and the group stays epoch-unknown; the honest self-update heals it.
func TestAHealsReplayedTailRefusesACommitThatChangesTheGroupContextExtensions(t *testing.T) {
	for _, c := range contextChangeCases {
		t.Run(c.name, func(t *testing.T) {
			h := newDSHarness(t)
			reg, s := h.mustRegisterOneLeaf(t, c.group)
			g := registrationFixture(t).groups[c.group]
			if err := h.ds.OnRestore(context.Background(), h.generation(t)+1); err != nil {
				t.Fatalf("OnRestore: %v", err)
			}
			leaf := uint32(0)
			_, err := h.ds.Heal(context.Background(), s, reg.GroupID, ds.HealRequest{
				GroupInfo: g.commitGroupInfo,
				Tail: []ds.HealTailItem{{
					Seq: 1, Epoch: 0, Kind: 1, Sender: &leaf, Blob: g.commit,
				}},
			})
			if !c.refuse {
				if err != nil {
					t.Fatalf("the honest heal: %v", err)
				}
				if h.epochUnknown(t, reg.GroupID) {
					t.Error("an accepted heal clears epoch_unknown")
				}
				return
			}
			if !hasRule(err, "tail") {
				t.Fatalf("got %v, want E_COMMIT_INVALID/tail", err)
			}
			if !h.epochUnknown(t, reg.GroupID) {
				t.Error("a refused heal must leave the group epoch-unknown")
			}
			if got := h.handshakeCount(t, reg.GroupID); got != 0 {
				t.Errorf("a refused heal wrote %d handshake rows", got)
			}
		})
	}
}
