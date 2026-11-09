package api_test

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/ds"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

// structureFixture is one community with a member and an outsider, and one channel of each
// kind, written straight into the real repository StructureChannels reads.
type structureFixture struct {
	src                       api.StructureChannels
	community, other          id.ID
	member, outsider          id.ID
	text, voice, category, dm id.ID
	readable, deletedText     id.ID
	otherText                 id.ID
}

func newStructureFixture(t *testing.T) structureFixture {
	t.Helper()
	e := newEnv(t)
	ctx := context.Background()
	f := structureFixture{src: api.StructureChannels{Repo: e.Repo}}
	f.member, _ = e.NewUser("member")
	f.outsider, _ = e.NewUser("outsider")
	newCommunity := func(owner id.ID) id.ID {
		cid := id.New()
		if err := e.Repo.CreateCommunity(ctx, store.CommunityRow{
			ID: cid, Owner: owner, Name: "c", PolicyJSON: []byte(`{}`), PolicyVersion: 1, Created: 1,
		}); err != nil {
			t.Fatalf("CreateCommunity: %v", err)
		}
		if err := e.Repo.PutMember(ctx, store.MemberOfCommunityRow{CommunityID: cid, UserID: owner, Joined: 1}); err != nil {
			t.Fatalf("PutMember: %v", err)
		}
		return cid
	}
	f.community = newCommunity(f.member)
	f.other = newCommunity(f.outsider)
	newRow := func(cid *id.ID, kind, mode, vis uint8) id.ID {
		row := store.ChannelRow{
			ID: id.New(), CommunityID: cid, Kind: kind, Mode: mode, Visibility: vis,
			Name: "ch", SettingsJSON: []byte(`{}`), HostPolicyVersion: 1, Created: 1,
		}
		if err := e.Repo.CreateChannel(ctx, row); err != nil {
			t.Fatalf("CreateChannel: %v", err)
		}
		return row.ID
	}
	f.text = newRow(&f.community, api.ChannelText, api.ModeE2EE, api.VisPrivate)
	f.voice = newRow(&f.community, api.ChannelVoice, api.ModeE2EE, api.VisPrivate)
	f.category = newRow(&f.community, api.ChannelCategory, api.ModeE2EE, api.VisPrivate)
	f.readable = newRow(&f.community, api.ChannelText, api.ModeReadable, api.VisDiscoverable)
	f.deletedText = newRow(&f.community, api.ChannelText, api.ModeE2EE, api.VisPrivate)
	if err := e.Repo.DeleteChannel(ctx, f.deletedText, 2); err != nil {
		t.Fatalf("DeleteChannel: %v", err)
	}
	f.dm = newRow(nil, api.ChannelDM, api.ModeE2EE, api.VisPrivate)
	// The outsider of the communities is the DM's one participant here; the
	// community member is not in it.
	if err := e.Repo.PutChannelMember(ctx, f.dm, f.outsider, 1); err != nil {
		t.Fatalf("PutChannelMember: %v", err)
	}
	f.otherText = newRow(&f.other, api.ChannelText, api.ModeE2EE, api.VisPrivate)
	return f
}

func binding(community *id.ID, target id.ID, kind uint8) ds.Binding {
	return ds.Binding{V: 1, CommunityID: community, TargetID: target, Kind: kind, E2EEVersion: 1}
}

// C1 (fix wave), invariant 11: while a restore's heal is pending, a channel's replacement group is
// re-created by its owner's device only: the community owner for a community channel, any
// participant for a DM (which has no owner).
func TestOnlyTheCommunityOwnerMayRecreateAChannelsGroup(t *testing.T) {
	f := newStructureFixture(t)
	ctx := context.Background()
	if err := f.src.MayRecreate(ctx, f.member, binding(&f.community, f.text, api.GroupText)); err != nil {
		t.Fatalf("the community owner re-creating: %v, want nil", err)
	}
	if err := f.src.MayRecreate(ctx, f.outsider, binding(&f.community, f.text, api.GroupText)); !errors.Is(err, ds.ErrNotEligible) {
		t.Fatalf("a non-owner re-creating: %v, want ErrNotEligible", err)
	}
	if err := f.src.MayRecreate(ctx, f.outsider, binding(nil, f.dm, api.GroupText)); err != nil {
		t.Fatalf("a DM participant re-creating: %v, want nil", err)
	}
}

// Invariant 1's input, from the real table: a live channel's visibility and mode, and "no
// channel row" (not an error) for a deleted channel and for a target that was never a channel.
func TestStructureChannelsReportsTheStoredMode(t *testing.T) {
	f := newStructureFixture(t)
	ctx := context.Background()
	vis, mode, err := f.src.Channel(ctx, f.readable)
	if err != nil || vis != api.VisDiscoverable || mode != api.ModeReadable {
		t.Fatalf("Channel(readable) = %d, %d, %v", vis, mode, err)
	}
	vis, mode, err = f.src.Channel(ctx, f.text)
	if err != nil || vis != api.VisPrivate || mode != api.ModeE2EE {
		t.Fatalf("Channel(text) = %d, %d, %v", vis, mode, err)
	}
	for name, target := range map[string]id.ID{"deleted": f.deletedText, "unknown": id.New()} {
		if _, _, err := f.src.Channel(ctx, target); !errors.Is(err, ds.ErrNoChannel) {
			t.Errorf("Channel(%s) = %v, want ds.ErrNoChannel", name, err)
		}
	}
}

// The registration ACL, rule by rule (Plan 1 follow-up card 14).
func TestStructureChannelsGatesRegistrationOnMembership(t *testing.T) {
	f := newStructureFixture(t)
	ctx := context.Background()
	c, o := &f.community, &f.other
	for _, tc := range []struct {
		name string
		user id.ID
		b    ds.Binding
		want error // nil, ds.ErrNotEligible or ds.ErrBindingTarget
	}{
		{"a member's text group on a text channel", f.member, binding(c, f.text, 0), nil},
		{"a member's call group on a voice channel", f.member, binding(c, f.voice, 1), nil},
		{"an outsider's text group on the same channel", f.outsider, binding(c, f.text, 0), ds.ErrNotEligible},
		{"an outsider's call group on the same voice channel", f.outsider, binding(c, f.voice, 1), ds.ErrNotEligible},
		{"a binding naming another community than the channel's", f.outsider, binding(o, f.text, 0), ds.ErrBindingTarget},
		{"a binding naming no community for a community channel", f.member, binding(nil, f.text, 0), ds.ErrBindingTarget},
		{"a text group on a voice channel", f.member, binding(c, f.voice, 0), ds.ErrBindingTarget},
		{"a text group on a category", f.member, binding(c, f.category, 0), ds.ErrBindingTarget},
		{"a call group on a text channel", f.member, binding(c, f.text, 1), ds.ErrBindingTarget},
		{"a community text group whose target is no channel", f.member, binding(c, id.New(), 0), ds.ErrBindingTarget},
		{"a text group on a deleted channel", f.member, binding(c, f.deletedText, 0), ds.ErrBindingTarget},
		// R9: a call group's target is its voice channel, never the call; community-wide bits
		// never stand in for the channel's overwrites.
		{"a member's community call group whose target is no channel", f.member, binding(c, id.New(), 1), ds.ErrBindingTarget},
		{"an outsider's community call group whose target is no channel", f.outsider, binding(c, id.New(), 1), ds.ErrBindingTarget},
		{"a call group naming no live community", f.member, binding(new(id.New()), id.New(), 1), ds.ErrBindingTarget},
		// DMs (task 6): the participants of a live DM channel, and nobody else.
		{"a DM text group with no channel row", f.member, binding(nil, id.New(), 0), ds.ErrBindingTarget},
		{"a DM call group with no channel row", f.member, binding(nil, id.New(), 1), ds.ErrBindingTarget},
		{"a non-participant's text group on a DM", f.member, binding(nil, f.dm, 0), ds.ErrNotEligible},
		{"a non-participant's call group on a DM", f.member, binding(nil, f.dm, 1), ds.ErrNotEligible},
		{"a participant's text group on a DM", f.outsider, binding(nil, f.dm, 0), nil},
		{"a participant's call group on a DM", f.outsider, binding(nil, f.dm, 1), nil},
		{"a community channel bound as a DM", f.member, binding(nil, f.text, 0), ds.ErrBindingTarget},
		{"a DM channel row bound under a community", f.member, binding(c, f.dm, 0), ds.ErrBindingTarget},
		// Not channel groups.
		{"a pairing group", f.outsider, binding(nil, id.New(), 2), nil},
		// Bot v2 Task 23 fix round 1: the interaction kind (3) is retired; nobody registers one.
		{"an interaction group", f.outsider, binding(nil, id.New(), 3), ds.ErrBindingTarget},
		{"a member's interaction group", f.member, binding(c, f.text, 3), ds.ErrBindingTarget},
		{"an unknown group kind", f.member, binding(c, f.text, 4), ds.ErrBindingTarget},
	} {
		err := f.src.MayRegister(ctx, tc.user, tc.b)
		switch {
		case tc.want == nil && err != nil:
			t.Errorf("%s: %v, want admitted", tc.name, err)
		case tc.want != nil && !errors.Is(err, tc.want):
			t.Errorf("%s: %v, want %v", tc.name, err, tc.want)
		}
	}
}

// A member who leaves loses the right to register in the community with the membership.
func TestLeavingACommunityEndsTheRightToRegisterInIt(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	owner, _ := e.NewUser("owner")
	member, _ := e.NewUser("member")
	cid := id.New()
	if err := e.Repo.CreateCommunity(ctx, store.CommunityRow{
		ID: cid, Owner: owner, Name: "c", PolicyJSON: []byte(`{}`), PolicyVersion: 1, Created: 1,
	}); err != nil {
		t.Fatalf("CreateCommunity: %v", err)
	}
	if err := e.Repo.PutMember(ctx, store.MemberOfCommunityRow{CommunityID: cid, UserID: member, Joined: 1}); err != nil {
		t.Fatalf("PutMember: %v", err)
	}
	// Every community has @everyone at position 0 (POST /v1/communities makes
	// it); a member's right to register reads it through the resolver (task 3).
	if err := e.Repo.PutRole(ctx, store.RoleRow{
		ID: id.New(), CommunityID: cid, Name: "@everyone", Position: 0,
		Allow: uint64(api.PermViewChannel | api.PermConnect), Created: 1,
	}); err != nil {
		t.Fatalf("PutRole: %v", err)
	}
	ch := store.ChannelRow{
		ID: id.New(), CommunityID: &cid, Name: "general", SettingsJSON: []byte(`{}`),
		HostPolicyVersion: 1, Created: 1,
	}
	if err := e.Repo.CreateChannel(ctx, ch); err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	src := api.StructureChannels{Repo: e.Repo}
	if err := src.MayRegister(ctx, member, binding(&cid, ch.ID, 0)); err != nil {
		t.Fatalf("a member was refused: %v", err)
	}
	if err := e.Repo.DeleteMember(ctx, cid, member); err != nil {
		t.Fatalf("DeleteMember: %v", err)
	}
	if err := src.MayRegister(ctx, member, binding(&cid, ch.ID, 0)); !errors.Is(err, ds.ErrNotEligible) {
		t.Fatalf("a former member = %v, want ds.ErrNotEligible", err)
	}
}

func TestManagedBotGroupRegistrationUsesTypedCommunityBinding(t *testing.T) {
	f := newStructureFixture(t)
	repo := f.src.Repo
	ctx := context.Background()
	botID, deviceID := id.New(), id.New()
	botUser := newAPITestUser(botID, "bot", 1)
	botUser.Kind = 1
	if err := repo.CreateUser(ctx, botUser); err != nil {
		t.Fatal(err)
	}
	device := newAPITestDevice(deviceID, botID, 1)
	device.DSKPub = uniqueAPITestDSK(t)
	device.Tier, device.SignerTier = 0, 0
	if err := repo.CreateDevice(ctx, device); err != nil {
		t.Fatal(err)
	}
	if err := repo.PutMember(ctx, store.MemberOfCommunityRow{CommunityID: f.community, UserID: botID, Joined: 1}); err != nil {
		t.Fatal(err)
	}
	for _, cid := range []id.ID{f.community, f.other} {
		if err := repo.PutRole(ctx, store.RoleRow{ID: id.New(), CommunityID: cid, Name: "@everyone", Position: 0,
			Allow: uint64(api.PermViewChannel | api.PermConnect), Created: 1}); err != nil {
			t.Fatal(err)
		}
	}
	runtime := store.BotRuntimeRow{ID: id.New(), TokenHash: make([]byte, 32), Created: 1, Rotated: 1}
	runtime.TokenHash[0] = 1
	if err := repo.CreateBotRuntime(ctx, runtime); err != nil {
		t.Fatal(err)
	}
	uid, did, rid := botID, deviceID, runtime.ID
	verified := int64(1)
	installation := store.BotInstallationRow{ID: id.New(), CommunityID: f.community,
		RuntimeID: &rid, UserID: &uid, DeviceID: &did, State: store.BotInstallationJoining, Revision: 1,
		ApprovedBy: f.member, ApprovedAt: 1, CredentialVerifiedAt: &verified, Created: 1, Updated: 1}
	if err := repo.CreateBotInstallation(ctx, installation); err != nil {
		t.Fatal(err)
	}
	if err := f.src.MayRegister(ctx, botID, binding(&f.community, f.text, api.GroupText)); err != nil {
		t.Fatalf("joining bot own-community typed registration: %v", err)
	}
	for _, tc := range []struct {
		name string
		b    ds.Binding
	}{
		{name: "foreign community", b: binding(&f.other, f.otherText, api.GroupText)},
		{name: "DM", b: binding(nil, f.dm, api.GroupText)},
	} {
		if err := f.src.MayRegister(ctx, botID, tc.b); !errors.Is(err, ds.ErrNotEligible) {
			t.Errorf("%s typed registration = %v, want ds.ErrNotEligible", tc.name, err)
		}
	}
	if err := f.src.MayRegister(ctx, botID, binding(nil, id.New(), 3)); !errors.Is(err, ds.ErrBindingTarget) {
		t.Errorf("interaction typed registration = %v, want ds.ErrBindingTarget (kind 3 is retired)", err)
	}
	installation.State = store.BotInstallationReady
	installation.Revision++
	installation.Updated++
	if err := repo.UpdateBotInstallation(ctx, installation.ID, installation.Revision-1, installation); err != nil {
		t.Fatalf("UpdateBotInstallation ready: %v", err)
	}
	if err := f.src.MayRegister(ctx, botID, binding(&f.community, f.text, api.GroupText)); err != nil {
		t.Fatalf("ready bot own-community typed registration: %v", err)
	}
	for _, tc := range []struct {
		name string
		b    ds.Binding
	}{
		{name: "ready foreign community", b: binding(&f.other, f.otherText, api.GroupText)},
		{name: "ready DM", b: binding(nil, f.dm, api.GroupText)},
	} {
		if err := f.src.MayRegister(ctx, botID, tc.b); !errors.Is(err, ds.ErrNotEligible) {
			t.Errorf("%s typed registration = %v, want ds.ErrNotEligible", tc.name, err)
		}
	}
	if err := f.src.MayRegister(ctx, botID, binding(nil, id.New(), 3)); !errors.Is(err, ds.ErrBindingTarget) {
		t.Errorf("ready interaction typed registration = %v, want ds.ErrBindingTarget (kind 3 is retired)", err)
	}
}

// End to end: the real delivery service, the real wasm core and the committed fixture, with
// StructureChannels over the real repository as the composition root injects it. The fixture's
// binding is a DM-shaped text group (no community, target = its group id), so the three answers
// here are the three the store can give it.
func TestRegisterThroughTheRealChannelSource(t *testing.T) {
	for _, c := range []struct {
		name       string
		seed       func(t *testing.T, h *groupsAPI)
		wantStatus int
		wantCode   string
	}{
		{
			// Card 14: before task 2 any enrolled device registered this group. Since task 6 a
			// group with no community must name a live DM.
			name:       "no channel row: a DM-shaped group that names no DM",
			seed:       func(*testing.T, *groupsAPI) {},
			wantStatus: http.StatusBadRequest, wantCode: "E_BINDING_INVALID",
		},
		{
			name: "the target is a DM the registering user is not in",
			seed: func(t *testing.T, h *groupsAPI) {
				seedDMAt(t, h, false)
			},
			wantStatus: http.StatusForbidden, wantCode: "E_FORBIDDEN",
		},
		{
			name: "the target is a discoverable channel: invariant 1 from the real table",
			seed: func(t *testing.T, h *groupsAPI) {
				seedChannelAt(t, h, api.ModeReadable, api.VisDiscoverable)
			},
			wantStatus: http.StatusForbidden, wantCode: "E_MODE_READABLE",
		},
		{
			name: "the target is a community channel the binding does not name",
			seed: func(t *testing.T, h *groupsAPI) {
				seedChannelAt(t, h, api.ModeE2EE, api.VisPrivate)
			},
			wantStatus: http.StatusBadRequest, wantCode: "E_BINDING_INVALID",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newGroupsAPIWith(t, func(repo store.Repository) ds.Channels {
				return api.StructureChannels{Repo: repo}
			})
			c.seed(t, h)
			res := h.do(t, http.MethodPost, "/v1/groups", h.session, h.createBody(t))
			if res.Code != c.wantStatus {
				t.Fatalf("status = %d, want %d: %x", res.Code, c.wantStatus, res.Body.Bytes())
			}
			var body []any
			if err := cborx.Unmarshal(res.Body.Bytes(), &body); err != nil || len(body) == 0 {
				t.Fatalf("error body: %v (%x)", err, res.Body.Bytes())
			}
			if body[0] != c.wantCode {
				t.Fatalf("code = %v, want %s", body[0], c.wantCode)
			}
			if _, err := h.deps.Repo.GetGroup(context.Background(), h.groupID); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("a refused registration left a group row: %v", err)
			}
		})
	}
}

// A participant of the DM the fixture's binding names registers its text group through the real
// channel source, the real delivery service and the real wasm core (task 6). The registration is
// an honest one (hardening G): the registration fixture's one-leaf group, by its creator, whose
// user is a participant of the DM at that group's target.
func TestADMParticipantRegistersThroughTheRealChannelSource(t *testing.T) {
	h := newGroupsAPIWith(t, func(repo store.Repository) ds.Channels {
		return api.StructureChannels{Repo: repo}
	})
	token := h.registrantToken(t)
	f := apiRegistrationData(t)
	ctx := context.Background()
	if err := h.deps.Repo.CreateChannel(ctx, store.ChannelRow{
		ID: f.targetID, Kind: api.ChannelDM, Mode: api.ModeE2EE, Visibility: api.VisPrivate,
		SettingsJSON: []byte(`{}`), HostPolicyVersion: 1, Created: 1,
	}); err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	other, _, _ := seedAPIDevice(t, h.deps)
	for _, u := range []id.ID{other.ID, f.userID} {
		if err := h.deps.Repo.PutChannelMember(ctx, f.targetID, u, 1); err != nil {
			t.Fatalf("PutChannelMember: %v", err)
		}
	}
	body, groupID := oneLeafBody(t, "one-leaf-0")
	if res := h.do(t, http.MethodPost, "/v1/groups", token, body); res.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", res.Code, res.Body.String())
	}
	if _, err := h.deps.Repo.GetGroup(ctx, groupID); err != nil {
		t.Fatalf("the registered group: %v", err)
	}
}

// seedDMAt creates a DM channel whose id is the fixture's binding target, between a fresh user and,
// when withSessionUser, the harness's session user (otherwise a second fresh user).
func seedDMAt(t *testing.T, h *groupsAPI, withSessionUser bool) {
	t.Helper()
	ctx := context.Background()
	if err := h.deps.Repo.CreateChannel(ctx, store.ChannelRow{
		ID: h.groupID, Kind: api.ChannelDM, Mode: api.ModeE2EE, Visibility: api.VisPrivate,
		SettingsJSON: []byte(`{}`), HostPolicyVersion: 1, Created: 1,
	}); err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	other, _, _ := seedAPIDevice(t, h.deps)
	second := h.user
	if !withSessionUser {
		u, _, _ := seedAPIDevice(t, h.deps)
		second = u.ID
	}
	for _, u := range []id.ID{other.ID, second} {
		if err := h.deps.Repo.PutChannelMember(ctx, h.groupID, u, 1); err != nil {
			t.Fatalf("PutChannelMember: %v", err)
		}
	}
}

// seedChannelAt creates a community owned by the harness's session user and a text channel in it
// whose id is the fixture's binding target.
func seedChannelAt(t *testing.T, h *groupsAPI, mode, visibility uint8) {
	t.Helper()
	ctx := context.Background()
	cid := id.New()
	if err := h.deps.Repo.CreateCommunity(ctx, store.CommunityRow{
		ID: cid, Owner: h.user, Name: "c", PolicyJSON: []byte(`{}`), PolicyVersion: 1, Created: 1,
	}); err != nil {
		t.Fatalf("CreateCommunity: %v", err)
	}
	if err := h.deps.Repo.PutMember(ctx, store.MemberOfCommunityRow{CommunityID: cid, UserID: h.user, Joined: 1}); err != nil {
		t.Fatalf("PutMember: %v", err)
	}
	if err := h.deps.Repo.CreateChannel(ctx, store.ChannelRow{
		ID: h.groupID, CommunityID: &cid, Kind: api.ChannelText, Mode: mode, Visibility: visibility,
		Name: "general", SettingsJSON: []byte(`{}`), HostPolicyVersion: 1, Created: 1,
	}); err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
}
