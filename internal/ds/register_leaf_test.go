package ds_test

// register_leaf_test.go is finding G2 of the second hardening review: group registration adopts
// exactly one leaf. A registration used to build the public group from whatever tree the creator
// uploaded and write a member row per leaf, so a stolen enrolled session could register a group
// whose one leaf named the session's device under a key of the attacker's own, and a registering
// member could seed leaves in other devices' names - each of them "a member" from then on, whose
// later external commit is a resync that skips the ACL, the device list and revocation.
//
// The honest registrations come from testkit/fixtures/registration (gen-registration-groups):
// one-leaf groups created by the 1,500-leaf fixture's creator, which is how every honest client
// registers (`TestClient::create_group_with`, `ClientCore::group_create`: `DillaGroup::create`,
// then the register call, with nobody added yet).

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/auth"
	"github.com/dilla-chat/dilla/internal/ds"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

const registrationFixtureDir = "../../testkit/fixtures/registration"

// registrationGroup is one group of the registration fixture, decoded.
type registrationGroup struct {
	groupID     id.ID
	binding     []byte
	groupInfo   []byte
	ratchetTree []byte
	leaves      int
	// commit and commitGroupInfo are set for `self-update` and `gce-swap` only: one commit by the
	// creator at the creation epoch, and the GroupInfo after it.
	commit          []byte
	commitGroupInfo []byte
}

// registrationFixtureData is the creator every group of the fixture was created by, and the groups.
type registrationFixtureData struct {
	userID, deviceID id.ID
	dsk              []byte
	targetID         id.ID
	groups           map[string]registrationGroup
}

var (
	registrationOnce sync.Once
	registrationVal  registrationFixtureData
	registrationErr  error
)

func registrationFixture(tb testing.TB) registrationFixtureData {
	tb.Helper()
	registrationOnce.Do(func() { registrationVal, registrationErr = loadRegistrationFixture() })
	if registrationErr != nil {
		tb.Fatalf("testkit/fixtures/registration: %v; regenerate it with\n"+
			"  cargo run -p dilla-testkit --bin dilla-testkit -- gen-registration-groups "+
			"--out testkit/fixtures/registration/", registrationErr)
	}
	return registrationVal
}

func loadRegistrationFixture() (registrationFixtureData, error) {
	raw, err := os.ReadFile(filepath.Join(registrationFixtureDir, "manifest.json"))
	if err != nil {
		return registrationFixtureData{}, err
	}
	var m struct {
		UserIDHex   string `json:"user_id_hex"`
		DeviceIDHex string `json:"device_id_hex"`
		DSKPubHex   string `json:"dsk_pub_hex"`
		TargetIDHex string `json:"target_id_hex"`
		NotAfter    uint64 `json:"not_after"`
		Groups      []struct {
			Name           string `json:"name"`
			GroupIDHex     string `json:"group_id_hex"`
			BindingHex     string `json:"binding_hex"`
			GroupInfoHex   string `json:"group_info_hex"`
			RatchetTreeHex string `json:"ratchet_tree_hex"`
			Leaves         int    `json:"leaves"`
			CommitHex      string `json:"commit_hex"`
			CommitGIHex    string `json:"commit_group_info_hex"`
		} `json:"groups"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return registrationFixtureData{}, err
	}
	if now := uint64(time.Now().Unix()); now >= m.NotAfter {
		return registrationFixtureData{}, errors.New("the fixture's leaf lifetimes have expired")
	}
	var out registrationFixtureData
	if out.userID, err = id.Parse(m.UserIDHex); err != nil {
		return registrationFixtureData{}, err
	}
	if out.deviceID, err = id.Parse(m.DeviceIDHex); err != nil {
		return registrationFixtureData{}, err
	}
	if out.targetID, err = id.Parse(m.TargetIDHex); err != nil {
		return registrationFixtureData{}, err
	}
	if out.dsk, err = hex.DecodeString(m.DSKPubHex); err != nil {
		return registrationFixtureData{}, err
	}
	out.groups = map[string]registrationGroup{}
	for _, g := range m.Groups {
		var rg registrationGroup
		if rg.groupID, err = id.Parse(g.GroupIDHex); err != nil {
			return registrationFixtureData{}, err
		}
		for _, field := range []struct {
			dst *[]byte
			src string
		}{
			{&rg.binding, g.BindingHex}, {&rg.groupInfo, g.GroupInfoHex}, {&rg.ratchetTree, g.RatchetTreeHex},
			{&rg.commit, g.CommitHex}, {&rg.commitGroupInfo, g.CommitGIHex},
		} {
			if *field.dst, err = hex.DecodeString(field.src); err != nil {
				return registrationFixtureData{}, err
			}
		}
		rg.leaves = g.Leaves
		out.groups[g.Name] = rg
	}
	return out, nil
}

// honestRegistrant makes the fixture's creator a real device of this instance, as signup leaves
// it: its user (with an SSK its device list verifies under), its device registered under the key
// its leaf carries, and a signed device list that lists it. The fixture's channel target is a
// private end-to-end-encrypted channel unless the test declared it otherwise
// (registrationChannel). It answers the device's enrolled session.
func (h *dsHarness) honestRegistrant(t *testing.T) auth.Session {
	t.Helper()
	f := registrationFixture(t)
	if _, declared := h.channels.modes[f.targetID]; !declared {
		h.channels.modes[f.targetID] = [2]uint8{0, 0}
	}
	ssk := testSSK(0x71)
	if _, err := h.repo.GetUser(context.Background(), f.userID); err != nil {
		h.userWithSSK(t, f.userID, ssk)
	}
	h.accountWithKey(t, f.userID, f.deviceID, f.dsk)
	h.publishDeviceList(t, f.userID, signedDeviceList(t, ssk, f.userID, []listEntry{
		{DeviceID: f.deviceID[:], DSKPub: f.dsk, AddedAt: 1},
	}))
	return auth.Session{UserID: f.userID, DeviceID: f.deviceID, Scope: auth.ScopeEnrolled}
}

// registrationChannel declares the visibility and mode of the registration fixture's target, as
// `channel` does for the 1,500-leaf fixture's, and returns the target.
func (h *dsHarness) registrationChannel(t *testing.T, visibility, mode uint8) id.ID {
	t.Helper()
	target := registrationFixture(t).targetID
	h.channels.modes[target] = [2]uint8{visibility, mode}
	return target
}

// registration is the request an honest device sends for the fixture's group `name`.
func registration(t *testing.T, name string, s auth.Session) ds.RegisterRequest {
	t.Helper()
	g, ok := registrationFixture(t).groups[name]
	if !ok {
		t.Fatalf("the registration fixture has no group %q", name)
	}
	return ds.RegisterRequest{
		Session: s, GroupID: g.groupID, Binding: g.binding, GroupInfo: g.groupInfo, RatchetTree: g.ratchetTree,
	}
}

// mustRegisterOneLeaf registers the fixture's group `name` through Register, as its creator.
func (h *dsHarness) mustRegisterOneLeaf(t *testing.T, name string) (ds.RegisterResult, auth.Session) {
	t.Helper()
	s := h.honestRegistrant(t)
	got, err := h.ds.Register(context.Background(), registration(t, name, s))
	if err != nil {
		t.Fatalf("Register %s: %v", name, err)
	}
	return got, s
}

// refusedRegistration asserts the refusal G2 asks for and that nothing was written: no group row,
// no member rows.
func (h *dsHarness) refusedRegistration(t *testing.T, what string, r ds.RegisterRequest) {
	t.Helper()
	_, err := h.ds.Register(context.Background(), r)
	var dsErr *ds.Error
	if !errors.As(err, &dsErr) || dsErr.Code != "E_INVALID_REQUEST" {
		t.Fatalf("%s: got %v, want E_INVALID_REQUEST", what, err)
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

// The honest registration: a one-leaf group whose leaf is the registering device, under its
// registered key, listed in its user's signed device list. Accepted, and the one member row is
// that device.
func TestRegisterAdoptsTheRegisteringDevicesOneLeaf(t *testing.T) {
	h := newDSHarness(t)
	got, s := h.mustRegisterOneLeaf(t, "one-leaf-0")
	members, err := h.repo.ListMembers(context.Background(), got.GroupID)
	if err != nil {
		t.Fatalf("ListMembers: %v", err)
	}
	if len(members) != 1 || members[0].DeviceID != s.DeviceID || members[0].UserID != s.UserID {
		t.Fatalf("members = %+v, want the registering device alone", members)
	}
	// And Register publishes the member set to the gateway: the device is the group's one fan-out
	// target, at its leaf, before any commit (the gateway learns nothing from SQL until a restart).
	if g := h.gw.Debug(s.DeviceID, got.GroupID); !g.InMembers || g.Members != 1 || g.Leaf == nil || *g.Leaf != 0 {
		t.Fatalf("the gateway after Register: %+v, want the registering device as the one member, at leaf 0", g)
	}
}

// A tree with more than one leaf is refused, even when every check on the registering device's
// own leaf would pass: the 1,500-leaf fixture uploaded by its leaf 0, whose device is registered
// under its leaf key and listed.
func TestRegisterRefusesATreeWithMoreThanOneLeaf(t *testing.T) {
	h := newDSHarness(t)
	f := dsFixture(t)
	target := h.channel(t, 0, 0)
	reg := registrationFixture(t) // the 1,500-leaf fixture's leaf 0 is this creator
	h.honestRegistrant(t)
	req := h.registerRequest(t, target)
	req.Session = auth.Session{UserID: reg.userID, DeviceID: reg.deviceID, Scope: auth.ScopeEnrolled}
	if req.GroupID != f.groupID {
		t.Fatal("the request is not the 1,500-leaf fixture's")
	}
	h.refusedRegistration(t, "1,500 leaves", req)
}

// The leaf count is the tree's, not the member list's: `hidden` holds the registering device's
// leaf and a second leaf whose credential is no dilla identity, which the member list leaves out.
func TestRegisterRefusesATreeThatHidesALeafTheMemberListCannotName(t *testing.T) {
	h := newDSHarness(t)
	s := h.honestRegistrant(t)
	if got := registrationFixture(t).groups["hidden"].leaves; got != 2 {
		t.Fatalf("the hidden group holds %d leaves, want 2", got)
	}
	h.refusedRegistration(t, "a hidden second leaf", registration(t, "hidden", s))
}

// The one leaf must be the registering session's own device and user, keyed by that device's
// registered key, and the device must pass the checks an external joiner passes: not revoked,
// listed in its user's newest signed device list. Each case is the honest registration with one
// thing changed; the honest one is accepted at the end, so the refusals are not the fixture's.
func TestRegisterRefusesALeafThatIsNotTheRegisteringDevicesOwn(t *testing.T) {
	f := registrationFixture(t)
	cases := []struct {
		name  string
		setup func(t *testing.T, h *dsHarness) auth.Session
	}{
		{"the leaf names another device of the session's user", func(t *testing.T, h *dsHarness) auth.Session {
			h.honestRegistrant(t)
			other := id.New()
			h.accountWithKey(t, f.userID, other, f.dsk)
			return auth.Session{UserID: f.userID, DeviceID: other, Scope: auth.ScopeEnrolled}
		}},
		{"the leaf names the session's device under another user", func(t *testing.T, h *dsHarness) auth.Session {
			h.honestRegistrant(t)
			return auth.Session{UserID: id.New(), DeviceID: f.deviceID, Scope: auth.ScopeEnrolled}
		}},
		{"the leaf's key is not the device's registered key (a stolen session token)", func(t *testing.T, h *dsHarness) auth.Session {
			h.channels.modes[f.targetID] = [2]uint8{0, 0}
			ssk := testSSK(0x72)
			h.userWithSSK(t, f.userID, ssk)
			registered := make([]byte, 32)
			registered[0] = 0x42
			h.accountWithKey(t, f.userID, f.deviceID, registered)
			h.publishDeviceList(t, f.userID, signedDeviceList(t, ssk, f.userID, []listEntry{
				{DeviceID: f.deviceID[:], DSKPub: registered, AddedAt: 1},
			}))
			return auth.Session{UserID: f.userID, DeviceID: f.deviceID, Scope: auth.ScopeEnrolled}
		}},
		{"the device is unknown to the instance", func(t *testing.T, h *dsHarness) auth.Session {
			h.channels.modes[f.targetID] = [2]uint8{0, 0}
			return auth.Session{UserID: f.userID, DeviceID: f.deviceID, Scope: auth.ScopeEnrolled}
		}},
		{"the device is revoked", func(t *testing.T, h *dsHarness) auth.Session {
			s := h.honestRegistrant(t)
			if err := h.repo.RevokeDevice(context.Background(), s.DeviceID, h.clk.Now().Unix()); err != nil {
				t.Fatalf("RevokeDevice: %v", err)
			}
			return s
		}},
		{"the user signed no device list", func(t *testing.T, h *dsHarness) auth.Session {
			h.channels.modes[f.targetID] = [2]uint8{0, 0}
			h.accountWithKey(t, f.userID, f.deviceID, f.dsk)
			return auth.Session{UserID: f.userID, DeviceID: f.deviceID, Scope: auth.ScopeEnrolled}
		}},
		{"the newest device list does not list the device", func(t *testing.T, h *dsHarness) auth.Session {
			h.channels.modes[f.targetID] = [2]uint8{0, 0}
			ssk := testSSK(0x73)
			h.userWithSSK(t, f.userID, ssk)
			h.accountWithKey(t, f.userID, f.deviceID, f.dsk)
			someoneElse := id.New()
			h.publishDeviceList(t, f.userID, signedDeviceList(t, ssk, f.userID, []listEntry{
				{DeviceID: someoneElse[:], DSKPub: make([]byte, 32), AddedAt: 1},
			}))
			return auth.Session{UserID: f.userID, DeviceID: f.deviceID, Scope: auth.ScopeEnrolled}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newDSHarness(t)
			h.refusedRegistration(t, c.name, registration(t, "one-leaf-0", c.setup(t, h)))
		})
	}
	h := newDSHarness(t)
	if _, err := h.ds.Register(context.Background(), registration(t, "one-leaf-0", h.honestRegistrant(t))); err != nil {
		t.Fatalf("the honest registration: %v", err)
	}
}
