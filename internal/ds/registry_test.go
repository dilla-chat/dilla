package ds_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jonasthim/dilla/internal/bot/capability"
	"github.com/jonasthim/dilla/internal/cborx"
	"github.com/jonasthim/dilla/internal/ds"
	"github.com/jonasthim/dilla/internal/id"
	"github.com/jonasthim/dilla/internal/store"
)

// Invariant 1: a text group is refused for an invite or discoverable channel and for a
// readable-mode channel.
//
// Each case gets its own harness because the committed fixture is ONE group with one group id
// baked into its GroupInfo, and `Register` refuses a body whose group id is not the group
// context's — so two successful registrations cannot share a database.
//
// The registration is an honest one (hardening G: a registered tree holds one leaf, the
// registering device's own): testkit/fixtures/registration's one-leaf group, by its creator.
func TestRegisterRefusesATextGroupOnAReadableOrOpenChannel(t *testing.T) {
	for _, c := range []struct {
		name       string
		visibility uint8 // 0 private, 1 invite, 2 discoverable
		mode       uint8 // 0 e2ee, 1 readable
		wantCode   string
	}{
		{"text on a private e2ee channel", 0, 0, ""},
		{"text on an invite channel", 1, 0, "E_MODE_READABLE"},
		{"text on a discoverable channel", 2, 0, "E_MODE_READABLE"},
		{"text on a readable channel", 0, 1, "E_MODE_READABLE"},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newDSHarness(t)
			h.registrationChannel(t, c.visibility, c.mode)
			_, err := h.ds.Register(context.Background(), registration(t, "one-leaf-0", h.honestRegistrant(t)))
			if c.wantCode == "" {
				if err != nil {
					t.Fatalf("Register: %v", err)
				}
				return
			}
			var dsErr *ds.Error
			if !errors.As(err, &dsErr) || dsErr.Code != c.wantCode {
				t.Fatalf("got %v, want %s", err, c.wantCode)
			}
			if dsErr.Status != http.StatusForbidden {
				t.Errorf("status = %d, want 403", dsErr.Status)
			}
		})
	}
}

// The other half of invariant 1: a CALL group is allowed on a discoverable or readable channel.
// The committed fixture is a text group, so the rule is exercised on the clause itself rather
// than through a Register that no fixture can supply.
func TestTheChannelModeRuleLetsACallGroupOntoAnyChannel(t *testing.T) {
	h := newDSHarness(t)
	target := h.channel(t, 2, 1) // discoverable AND readable: the strictest channel there is
	for _, kind := range []uint8{1 /* call */, 2 /* pairing */, 3 /* interaction */} {
		b := dsFixture(t).binding
		b.Kind = kind
		b.TargetID = target
		if err := ds.CheckChannelModeForTest(h.ds, context.Background(), b); err != nil {
			t.Errorf("kind %d on a discoverable readable channel: %v, want no refusal", kind, err)
		}
	}
	text := dsFixture(t).binding
	text.TargetID = target
	if err := ds.CheckChannelModeForTest(h.ds, context.Background(), text); err == nil {
		t.Error("a TEXT group on the same channel was allowed; invariant 1 refuses it")
	}
}

// A target with no channel row at all — a DM or a pairing group — is not subject to the mode rule.
func TestTheChannelModeRuleIgnoresATargetThatIsNotAChannel(t *testing.T) {
	h := newDSHarness(t)
	b := dsFixture(t).binding
	b.TargetID = id.New() // never declared to the Channels source
	if err := ds.CheckChannelModeForTest(h.ds, context.Background(), b); err != nil {
		t.Fatalf("a target with no channel row was refused: %v", err)
	}
}

// The registration ACL (Plan 1 follow-up card 14, closed by Plan 2 task 2): Register asks the
// channel source whether the SESSION's user may register a group under this binding, and maps
// the two refusals to the protocol's codes. A source that cannot answer is a refusal too, never
// a pass.
func TestRegisterAsksTheChannelSourceWhoMayRegister(t *testing.T) {
	for _, c := range []struct {
		name       string
		refuse     error
		wantCode   string
		wantStatus int
	}{
		{"admitted", nil, "", 0},
		{"not a member", fmt.Errorf("%w: not a member", ds.ErrNotEligible), "E_FORBIDDEN", http.StatusForbidden},
		{"a binding that names no registrable target", fmt.Errorf("%w: wrong community", ds.ErrBindingTarget), "E_BINDING_INVALID", http.StatusBadRequest},
		{"a source that cannot answer", errors.New("database is closed"), "", 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newDSHarness(t)
			h.channels.refuse = c.refuse
			// A refused case registers under a user who is neither the creator nor any leaf's, so
			// asking about anybody but the session's user fails it. The admitted case must be an
			// honest registration (hardening G: the one leaf is the session's own device and user),
			// so there the session's user is the leaf's; the refused cases carry the distinction.
			req := h.registerRequest(t, h.channel(t, 0, 0))
			req.Session.UserID = id.New()
			if c.refuse == nil {
				req = registration(t, "one-leaf-0", h.honestRegistrant(t))
			}
			want, err := ds.DecodeBindingForTest(req.Binding)
			if err != nil {
				t.Fatalf("decode the request's binding: %v", err)
			}
			_, err = h.ds.Register(context.Background(), req)

			wantCalls := 1
			if c.refuse == nil {
				wantCalls = 2 // early refusal check plus the transaction-fenced acceptance recheck
			}
			if len(h.channels.asked) != wantCalls {
				t.Fatalf("MayRegister was asked %d times, want %d", len(h.channels.asked), wantCalls)
			}
			q := h.channels.asked[0]
			if q.user != req.Session.UserID {
				t.Errorf("MayRegister was asked about %v, want the session's user %v", q.user, req.Session.UserID)
			}
			if q.binding != want {
				t.Errorf("MayRegister was asked about %+v, want the decoded binding %+v", q.binding, want)
			}

			switch {
			case c.refuse == nil:
				if err != nil {
					t.Fatalf("Register: %v", err)
				}
				return
			case c.wantCode == "":
				// Not a *ds.Error: the HTTP layer answers E_INTERNAL, and nothing is written.
				var dsErr *ds.Error
				if err == nil || errors.As(err, &dsErr) {
					t.Fatalf("got %v, want the source's own error", err)
				}
			default:
				var dsErr *ds.Error
				if !errors.As(err, &dsErr) || dsErr.Code != c.wantCode || dsErr.Status != c.wantStatus {
					t.Fatalf("got %v, want %d %s", err, c.wantStatus, c.wantCode)
				}
			}
			if n := h.countRows(t, "mls_groups"); n != 0 {
				t.Fatalf("a refused registration wrote %d group rows", n)
			}
		})
	}
}

func TestSignedCommunityRegistrationRequiresWireThreeAfterAuthorization(t *testing.T) {
	for _, tc := range []struct {
		name         string
		wire         uint64
		unauthorized bool
		wantUpgrade  bool
		wantSuccess  bool
	}{
		{name: "wire zero", wire: 0, wantUpgrade: true},
		{name: "wire two", wire: 2, wantUpgrade: true},
		{name: "wire three", wire: 3, wantSuccess: true},
		{name: "unauthorized wire two", wire: 2, unauthorized: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newDSHarness(t)
			session := h.honestRegistrant(t)
			req := registration(t, "community-one-leaf", session)
			binding, err := ds.DecodeBindingForTest(req.Binding)
			if err != nil {
				t.Fatalf("decode signed community binding: %v", err)
			}
			if binding.CommunityID == nil || *binding.CommunityID != mustCommunityID(t) {
				t.Fatalf("binding community = %v, want the fixture's fixed community", binding.CommunityID)
			}
			h.channels.modes[binding.TargetID] = [2]uint8{0, 0}
			now := h.clk.Now().Unix()
			if err := h.repo.CreateCommunity(context.Background(), store.CommunityRow{
				ID: *binding.CommunityID, Owner: session.UserID, Name: "signed-registration",
				PolicyJSON: []byte(`{}`), PolicyVersion: 1, Created: now,
			}); err != nil {
				t.Fatalf("CreateCommunity: %v", err)
			}
			if err := h.repo.CreateBotInstallation(context.Background(), store.BotInstallationRow{
				ID: id.New(), CommunityID: *binding.CommunityID, State: store.BotInstallationReady,
				Revision: 1, ApprovedBy: session.UserID, ApprovedAt: now, Created: now, Updated: now,
			}); err != nil {
				t.Fatalf("CreateBotInstallation: %v", err)
			}

			req.Session.WireVersion = tc.wire
			if tc.unauthorized {
				h.channels.refuse = fmt.Errorf("%w: not a member", ds.ErrNotEligible)
			}
			_, err = h.ds.Register(context.Background(), req)
			switch {
			case tc.wantUpgrade:
				var upgrade *capability.UpgradeRequired
				if !errors.As(err, &upgrade) {
					t.Fatalf("Register wire %d = %v, want typed upgrade refusal", tc.wire, err)
				}
			case tc.wantSuccess:
				if err != nil {
					t.Fatalf("Register wire 3: %v", err)
				}
				row, getErr := h.repo.GetGroup(context.Background(), req.GroupID)
				if getErr != nil || !bytes.Equal(row.Binding, req.Binding) {
					t.Fatalf("stored signed community group = %+v, %v", row, getErr)
				}
			default:
				var dsErr *ds.Error
				if !errors.As(err, &dsErr) || dsErr.Code != "E_FORBIDDEN" {
					t.Fatalf("unauthorized registration = %v, want normal E_FORBIDDEN", err)
				}
			}
			if !tc.wantSuccess {
				if _, getErr := h.repo.GetGroup(context.Background(), req.GroupID); !errors.Is(getErr, store.ErrNotFound) {
					t.Fatalf("refused registration persisted a group: %v", getErr)
				}
				members, listErr := h.repo.ListMembers(context.Background(), req.GroupID)
				if listErr != nil || len(members) != 0 {
					t.Fatalf("refused registration persisted member rows: %d, %v", len(members), listErr)
				}
			}
		})
	}
}

func mustCommunityID(t *testing.T) id.ID {
	t.Helper()
	community, err := id.Parse("6a6a6a6a6a6a6a6a6a6a6a6a6a6a6a6a")
	if err != nil {
		t.Fatalf("parse fixture community id: %v", err)
	}
	return community
}

// Invariant 1's mode refusal comes first: a text group on a readable channel is E_MODE_READABLE
// whoever asks, and the ACL is not consulted for a registration that is refused anyway.
func TestTheModeRuleIsCheckedBeforeTheRegistrationACL(t *testing.T) {
	h := newDSHarness(t)
	h.channels.refuse = fmt.Errorf("%w: not a member", ds.ErrNotEligible)
	_, err := h.ds.Register(context.Background(), h.registerRequest(t, h.channel(t, 0, 1)))
	var dsErr *ds.Error
	if !errors.As(err, &dsErr) || dsErr.Code != "E_MODE_READABLE" {
		t.Fatalf("got %v, want E_MODE_READABLE", err)
	}
}

// A delivery service built with no channel source refuses every registration: the default is
// the conservative one, as ACL's and DeviceLists' are.
func TestADeliveryServiceWithNoChannelSourceRefusesRegistration(t *testing.T) {
	h := newDSHarness(t)
	d, err := ds.New(ds.Options{
		Store: h.repo, Wasm: h.wasm, Gateway: h.gw, Clock: h.clk, Keys: testInstanceKeys(t),
		Policy: ds.DefaultPolicy(),
	})
	if err != nil {
		t.Fatalf("ds.New: %v", err)
	}
	t.Cleanup(func() { _ = d.Shutdown(context.Background()) })
	_, err = d.Register(context.Background(), h.registerRequest(t, dsFixture(t).targetID))
	var dsErr *ds.Error
	if !errors.As(err, &dsErr) || dsErr.Code != "E_FORBIDDEN" || dsErr.Status != http.StatusForbidden {
		t.Fatalf("got %v, want 403 E_FORBIDDEN", err)
	}
}

func TestRegisterRefusesADuplicateGroupID(t *testing.T) {
	h := newDSHarness(t)
	req := registration(t, "one-leaf-0", h.honestRegistrant(t))
	if _, err := h.ds.Register(context.Background(), req); err != nil {
		t.Fatalf("first Register: %v", err)
	}
	var dsErr *ds.Error
	if _, err := h.ds.Register(context.Background(), req); !errors.As(err, &dsErr) || dsErr.Code != "E_GROUP_EXISTS" {
		t.Fatalf("got %v, want E_GROUP_EXISTS", err)
	}
}

// rivalGroup stores another open group of the fixture's kind (text) on the fixture's target, as a
// member who registered first would have left it; epochUnknown marks it as a restore leaves it.
func (h *dsHarness) rivalGroup(t *testing.T, epochUnknown bool) id.ID {
	t.Helper()
	return h.rivalGroupOn(t, dsFixture(t).targetID, epochUnknown)
}

// rivalGroupOn is rivalGroup on another target: the registration fixture's, for a test whose
// successful registration is an honest one-leaf group.
func (h *dsHarness) rivalGroupOn(t *testing.T, target id.ID, epochUnknown bool) id.ID {
	t.Helper()
	ctx := context.Background()
	gid := id.New()
	if err := h.repo.CreateGroup(ctx, store.GroupRow{
		GroupID: gid, Binding: []byte{0xf6}, Kind: 0, TargetID: target,
		Ciphersuite: 1, Epoch: 3, Created: h.clk.Now().Unix(),
	}); err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}
	if epochUnknown {
		if err := h.repo.MarkAllGroupsEpochUnknown(ctx, h.clk.Now().Add(24*time.Hour).Unix()); err != nil {
			t.Fatalf("MarkAllGroupsEpochUnknown: %v", err)
		}
	}
	return gid
}

// C1 (fix wave): one text group per end-to-end-encrypted channel and one call group per voice
// channel (protocol/01 § Group kinds). A second registration for a target that already has an
// open group of the kind forks the channel (text) or hijacks the live call (call), and every
// registration starts an Add storm that spends one KeyPackage of every eligible device.
func TestRegisterRefusesASecondOpenGroupForOneTarget(t *testing.T) {
	h := newDSHarness(t)
	rival := h.rivalGroup(t, false)
	_, err := h.ds.Register(context.Background(), h.registerRequest(t, h.channel(t, 0, 0)))
	var dsErr *ds.Error
	if !errors.As(err, &dsErr) || dsErr.Code != "E_GROUP_EXISTS" || dsErr.Status != http.StatusConflict {
		t.Fatalf("a second text group for the target: %v, want 409 E_GROUP_EXISTS", err)
	}
	if _, err := h.repo.GetGroup(context.Background(), dsFixture(t).groupID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the refused group was stored: %v", err)
	}
	if groups, _ := h.repo.GroupsForTarget(context.Background(), dsFixture(t).targetID, 0); len(groups) != 1 || groups[0].GroupID != rival {
		t.Fatalf("open groups for the target = %+v, want only the first", groups)
	}
}

// Invariant 11's one exception: while every open group of the target is epoch-unknown (a restore
// whose heal is pending), the channel owner's device may re-create the group. Anyone the channel
// source does not name is still refused.
func TestRegisterAdmitsTheOwnersReCreationOfAnEpochUnknownGroup(t *testing.T) {
	refused := newDSHarness(t)
	refused.rivalGroup(t, true)
	refused.channels.recreate = fmt.Errorf("%w: not the channel owner", ds.ErrNotEligible)
	_, err := refused.ds.Register(context.Background(), refused.registerRequest(t, refused.channel(t, 0, 0)))
	var dsErr *ds.Error
	if !errors.As(err, &dsErr) || dsErr.Code != "E_GROUP_EXISTS" {
		t.Fatalf("a non-owner's re-creation: %v, want E_GROUP_EXISTS", err)
	}

	// The owner's re-creation is an honest registration: a one-leaf group by its creator.
	h := newDSHarness(t)
	h.rivalGroupOn(t, h.registrationChannel(t, 0, 0), true)
	if _, err := h.ds.Register(context.Background(), registration(t, "one-leaf-1", h.honestRegistrant(t))); err != nil {
		t.Fatalf("the owner's re-creation of an epoch-unknown group: %v", err)
	}
}

// The body's binding must equal extension 0xF001 inside the uploaded GroupInfo's group context.
//
// Two separate refusals share that sentence and they are pinned separately, because a single
// case cannot pin both: mutating the encoding takes the decoder's branch and never reaches the
// equality check. The fixture's last byte is `media_version`'s 0x00 and 0x00^0xff is 0xff, CBOR's
// break byte, so the "flip a byte" case alone would still pass with the equality check deleted.
func TestRegisterRefusesABindingThatDoesNotMatchTheGroupInfo(t *testing.T) {
	// A WELL-FORMED eight-element binding that differs from the signed one in exactly one field.
	// This is the case the byte-for-byte equality check in Register exists for: a client that
	// signs one binding into the group context and declares another in the body.
	t.Run("a well formed binding that differs in one field", func(t *testing.T) {
		h := newDSHarness(t)
		req := h.registerRequest(t, h.channel(t, 0, 0))
		altered := dsFixture(t).binding
		altered.PolicyVersion = 99 // the fixture signs policy_version = 1
		encoded, err := cborx.Marshal(altered)
		if err != nil {
			t.Fatalf("encode the altered binding: %v", err)
		}
		if bytes.Equal(encoded, req.Binding) {
			t.Fatal("the altered binding encodes to the fixture's own bytes; the case proves nothing")
		}
		if _, err := ds.DecodeBindingForTest(encoded); err != nil {
			t.Fatalf("the altered binding must stay decodable, or the decode branch refuses it: %v", err)
		}
		req.Binding = encoded
		mustRefuseWithBindingInvalid(t, h, req)
	})

	// And the decode branch: bytes that are not a dilla_binding at all.
	t.Run("undecodable binding bytes", func(t *testing.T) {
		h := newDSHarness(t)
		req := h.registerRequest(t, h.channel(t, 0, 0))
		req.Binding = append([]byte(nil), req.Binding...)
		req.Binding[len(req.Binding)-1] ^= 0xff
		if _, err := ds.DecodeBindingForTest(req.Binding); err == nil {
			t.Fatal("the mutated bytes still decode; this case no longer pins the decode branch")
		}
		mustRefuseWithBindingInvalid(t, h, req)
	})
}

// A group whose dilla_binding names ANOTHER instance is that instance's group: the binding is
// consistent with its own GroupInfo, and still refused, because this instance's id is not the one
// it carries (invariant 1). The same fixture registers on an instance whose id it does carry.
func TestRegisterRefusesABindingForAnotherInstance(t *testing.T) {
	h := newDSHarness(t)
	keys := testInstanceKeys(t)
	keys.InstanceID[0] ^= 0xff
	other, err := ds.New(ds.Options{
		Store: h.repo, Wasm: h.wasm, Gateway: h.gw, Clock: h.clk, Keys: keys,
		Channels: h.channels, Policy: ds.DefaultPolicy(),
	})
	if err != nil {
		t.Fatalf("ds.New: %v", err)
	}
	// An honest registration (hardening G), so the instance it names can accept it.
	req := registration(t, "one-leaf-0", h.honestRegistrant(t))
	var dsErr *ds.Error
	if _, err := other.Register(context.Background(), req); !errors.As(err, &dsErr) ||
		dsErr.Code != "E_BINDING_INVALID" || !strings.Contains(dsErr.Detail, "instance") {
		t.Fatalf("got %v, want E_BINDING_INVALID naming the instance", err)
	}
	if _, err := h.ds.Register(context.Background(), req); err != nil {
		t.Fatalf("the instance the binding names must accept it: %v", err)
	}
}

func mustRefuseWithBindingInvalid(t *testing.T, h *dsHarness, req ds.RegisterRequest) {
	t.Helper()
	var dsErr *ds.Error
	if _, err := h.ds.Register(context.Background(), req); !errors.As(err, &dsErr) || dsErr.Code != "E_BINDING_INVALID" {
		t.Fatalf("got %v, want E_BINDING_INVALID", err)
	}
	if dsErr.Status != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", dsErr.Status)
	}
}

func TestRegisterAnswersNextSeqOneAndInfoAgrees(t *testing.T) {
	h := newDSHarness(t)
	got, session := h.mustRegisterOneLeaf(t, "one-leaf-0")
	if got.NextSeq != 1 {
		t.Fatalf("NextSeq = %d, want 1 (high-water 0 + 1)", got.NextSeq)
	}
	info, err := h.ds.Info(context.Background(), got.GroupID, session)
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if info.NextSeq != got.NextSeq {
		t.Errorf("Info.NextSeq = %d, want %d", info.NextSeq, got.NextSeq)
	}
	if len(info.TreeHash) != 32 {
		t.Errorf("tree_hash is %d bytes, want 32", len(info.TreeHash))
	}
}

// The DS serves the tree from its own PublicGroup; a committer uploads a GroupInfo WITHOUT one
// (invariant 2).
func TestTreeIsServedFromTheDSsOwnPublicGroup(t *testing.T) {
	h := newDSHarness(t)
	reg, session := h.mustRegister(t)
	tree, err := h.ds.Tree(context.Background(), reg.GroupID, session)
	if err != nil {
		t.Fatalf("Tree: %v", err)
	}
	if len(tree.RatchetTree) == 0 {
		t.Fatal("the DS served an empty ratchet tree")
	}
	info, err := h.ds.Info(context.Background(), reg.GroupID, session)
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if string(tree.TreeHash) != string(info.TreeHash) {
		t.Error("the served tree hash and the stored GroupInfo's disagree")
	}
}

// Both reads are refused to a non-member the channel ACL does not admit, and the refusal is
// E_NOT_FOUND rather than E_FORBIDDEN: a 403 would tell any authenticated device on the instance
// which group ids are live. The stranger is another user entirely: a second device of a member's
// USER is eligible under Plan 1's ACL and may read both to join by external commit (join_test.go,
// deviation B36).
func TestInfoAndTreeAnswerNotFoundToANonMember(t *testing.T) {
	h := newDSHarness(t)
	reg, _ := h.mustRegister(t)
	stranger := h.memberSession(t, reg.GroupID, 0)
	stranger.DeviceID = id.New()
	stranger.UserID = id.New()

	var dsErr *ds.Error
	if _, err := h.ds.Info(context.Background(), reg.GroupID, stranger); !errors.As(err, &dsErr) ||
		dsErr.Code != "E_NOT_FOUND" {
		t.Errorf("Info: got %v, want E_NOT_FOUND", err)
	}
	if _, err := h.ds.Tree(context.Background(), reg.GroupID, stranger); !errors.As(err, &dsErr) ||
		dsErr.Code != "E_NOT_FOUND" {
		t.Errorf("Tree: got %v, want E_NOT_FOUND", err)
	}
}

// Registration records the group's leaves in the SAME transaction as the group row: a registered
// group with no member rows would refuse every later commit from its own creator. A registration
// is a one-leaf group since hardening G, so the leaf recorded is the registering device's; the
// 1,500-leaf fixture is seeded, not registered (dstest).
func TestRegisterRecordsTheGroupsLeaves(t *testing.T) {
	h := newDSHarness(t)
	reg, session := h.mustRegisterOneLeaf(t, "one-leaf-0")
	members, err := h.repo.ListMembers(context.Background(), reg.GroupID)
	if err != nil {
		t.Fatalf("ListMembers: %v", err)
	}
	if want := registrationFixture(t).groups["one-leaf-0"].leaves; len(members) != want {
		t.Fatalf("mls_members holds %d leaves, want the tree's %d", len(members), want)
	}
	if members[0].DeviceID != session.DeviceID {
		t.Fatalf("mls_members records device %s, want the registering device %s", members[0].DeviceID, session.DeviceID)
	}
	groups, err := h.repo.GroupsForDevice(context.Background(), members[0].DeviceID)
	if err != nil {
		t.Fatalf("GroupsForDevice: %v", err)
	}
	if len(groups) != 1 || groups[0] != reg.GroupID {
		t.Fatalf("GroupsForDevice = %v, want [%s]", groups, reg.GroupID)
	}
}

// Close marks the group closed and drops the cached handle; the row survives, because the
// handshake log is still readable to a device catching up.
func TestCloseMarksTheGroupClosed(t *testing.T) {
	h := newDSHarness(t)
	reg, _ := h.mustRegister(t)
	if err := h.ds.Close(context.Background(), reg.GroupID); err != nil {
		t.Fatalf("Close: %v", err)
	}
	row, err := h.repo.GetGroup(context.Background(), reg.GroupID)
	if err != nil {
		t.Fatalf("GetGroup: %v", err)
	}
	if row.ClosedAt == nil {
		t.Fatal("closed_at is still NULL after Close")
	}
}

// CloseIfAlone (web-3 task 10 fix, f10app I1) closes the group only while the caller's device is
// its only current leaf, decided under the group lock: a one-leaf group of the caller closes; a
// group with any other member stays open and the answer is ErrNotAlone; a device that holds no
// leaf is never alone in it.
func TestCloseIfAloneClosesOnlyAGroupWhoseOnlyLeafIsTheCallers(t *testing.T) {
	h := newDSHarness(t)
	reg, session := h.mustRegisterOneLeaf(t, "one-leaf-0")
	if err := h.ds.CloseIfAlone(context.Background(), reg.GroupID, id.New()); !errors.Is(err, ds.ErrNotAlone) {
		t.Fatalf("CloseIfAlone by a device with no leaf = %v, want ErrNotAlone", err)
	}
	if row, _ := h.repo.GetGroup(context.Background(), reg.GroupID); row.ClosedAt != nil {
		t.Fatal("a device with no leaf closed the group")
	}
	if err := h.ds.CloseIfAlone(context.Background(), reg.GroupID, session.DeviceID); err != nil {
		t.Fatalf("CloseIfAlone by the only leaf: %v", err)
	}
	if row, _ := h.repo.GetGroup(context.Background(), reg.GroupID); row.ClosedAt == nil {
		t.Fatal("the only leaf's CloseIfAlone left the group open")
	}
	if err := h.ds.CloseIfAlone(context.Background(), reg.GroupID, session.DeviceID); err != nil {
		t.Fatalf("CloseIfAlone of a closed group = %v, want nil (it is over already)", err)
	}
}

func TestCloseIfAloneLeavesAGroupWithAnotherMemberOpen(t *testing.T) {
	h := newDSHarness(t)
	g := h.groupWithMembers(t, 2)
	if err := h.ds.CloseIfAlone(context.Background(), g.id, g.sessionOf(0).DeviceID); !errors.Is(err, ds.ErrNotAlone) {
		t.Fatalf("CloseIfAlone with other members = %v, want ErrNotAlone", err)
	}
	if row, _ := h.repo.GetGroup(context.Background(), g.id); row.ClosedAt != nil {
		t.Fatal("CloseIfAlone closed a group that has other members")
	}
}

func TestCloseIfAloneIgnoresManagedBotLeaves(t *testing.T) {
	h := newDSHarness(t)
	reg, human := h.mustRegisterOneLeaf(t, "one-leaf-0")
	botUser, botDevice := id.New(), id.New()
	h.account(t, botUser, botDevice)
	setUserKind(t, h, botUser, 1)
	row, err := h.repo.GetGroup(t.Context(), reg.GroupID)
	if err != nil {
		t.Fatal(err)
	}
	members, err := h.repo.ListMembers(t.Context(), reg.GroupID)
	if err != nil {
		t.Fatal(err)
	}
	// Closing a call reads its stored roster; no MLS operation is needed to exercise the
	// extra bot leaf. Keep the caller's genuine one-leaf registration intact.
	members = append(members, store.MemberRow{GroupID: reg.GroupID, LeafIndex: 1, UserID: botUser,
		DeviceID: botDevice, SignatureKey: bytes.Repeat([]byte{4}, 32), AddedEpoch: row.Epoch})
	if err := h.repo.ReplaceMembers(t.Context(), reg.GroupID, row.Epoch, members); err != nil {
		t.Fatal(err)
	}
	if err := h.ds.CloseIfAlone(t.Context(), reg.GroupID, id.New()); !errors.Is(err, ds.ErrNotAlone) {
		t.Fatalf("a nonmember beside a bot = %v, want ErrNotAlone", err)
	}
	if err := h.ds.CloseIfAlone(t.Context(), reg.GroupID, human.DeviceID); err != nil {
		t.Fatalf("the only human beside a bot cannot close the group: %v", err)
	}
	if row, err := h.repo.GetGroup(t.Context(), reg.GroupID); err != nil || row.ClosedAt == nil {
		t.Fatalf("the group is still open: %+v, %v", row, err)
	}
}
