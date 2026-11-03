package api_test

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/ds"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

func dmEnv(t *testing.T) (*env, *recordingDS) {
	t.Helper()
	e := newEnv(t)
	dsvc := &recordingDS{}
	// newEnv migrates but does not initialise the instance; `dillad init` would.
	if err := e.Repo.CreateInstance(t.Context(), store.InstanceRow{
		InstanceID: id.New(), ExternalSenderKeyID: id.New(), KeyHistory: []byte{1},
		FrankingKeyID: id.New(), Generation: 1, PolicyVersion: 1, Created: e.Clk.Now().Unix(),
	}); err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	inst, err := e.Repo.GetInstance(t.Context())
	if err != nil {
		t.Fatalf("GetInstance: %v", err)
	}
	api.NewDMs(e.Repo, dsvc, e.Clk, inst.InstanceID, 10, slog.New(slog.DiscardHandler)).Register(e.Mux)
	return e, dsvc
}

func openDM(t *testing.T, e *env, tok string, with ...id.ID) (id.ID, int) {
	t.Helper()
	status, body := e.Do(http.MethodPost, "/v1/dms", tok, []any{with})
	if status != http.StatusOK && status != http.StatusCreated {
		return id.ID{}, status
	}
	var out []cbor.RawMessage
	mustUnmarshalBody(t, body, &out)
	var ch id.ID
	mustUnmarshal(t, out[0], &ch)
	return ch, status
}

func TestADMIsIdempotent(t *testing.T) {
	e, _ := dmEnv(t)
	alice, aliceTok := e.NewUser("alice")
	bob, bobTok := e.NewUser("bob")

	first, status := openDM(t, e, aliceTok, bob)
	if status != http.StatusCreated {
		t.Fatalf("first POST /v1/dms = %d", status)
	}
	second, status := openDM(t, e, aliceTok, bob)
	if status != http.StatusOK {
		t.Fatalf("second POST /v1/dms = %d, want 200", status)
	}
	if first != second {
		t.Fatalf("second DM = %x, want %x", second, first)
	}
	// Bob opening it from his side lands on the same channel.
	fromBob, status := openDM(t, e, bobTok, alice)
	if status != http.StatusOK || fromBob != first {
		t.Fatalf("bob's DM = %x (%d), want %x", fromBob, status, first)
	}
	if first != api.DMChannelID([]id.ID{alice, bob}) {
		t.Fatalf("channel id is not the derived DM id")
	}
	row, err := e.Repo.GetChannel(t.Context(), first)
	if err != nil {
		t.Fatalf("GetChannel: %v", err)
	}
	if row.Kind != api.ChannelDM || row.CommunityID != nil || row.Mode != api.ModeE2EE ||
		row.Visibility != api.VisPrivate {
		t.Fatalf("DM channel = %+v", row)
	}
}

func TestAGroupDMCapsAtItsConfiguredSize(t *testing.T) {
	e, _ := dmEnv(t) // maxGroupDM = 10
	_, tok := e.NewUser("alice")
	others := make([]id.ID, 0, 12)
	for i := range 12 {
		u, _ := e.NewUser(fmt.Sprintf("u%02d", i))
		others = append(others, u)
	}
	if _, status := openDM(t, e, tok, others[:9]...); status != http.StatusCreated {
		t.Fatalf("10-member group DM = %d", status)
	}
	_, status := openDM(t, e, tok, others...)
	if status != http.StatusBadRequest {
		t.Fatalf("13-member group DM = %d, want 400", status)
	}
	if _, status := openDM(t, e, tok); status != http.StatusBadRequest {
		t.Fatal("a DM with no recipient was accepted")
	}
	if _, status := openDM(t, e, tok, userOf(t, e, tok)); status != http.StatusBadRequest {
		t.Fatal("a DM with yourself was accepted")
	}
}

// A device that had no KeyPackage when its DM's group was populated is proposed once it publishes
// one: the composition root runs SyncUserDMs for the publishing user after every accepted POST
// /v1/keypackages (Plan 2 task 6), over every DM that user is a participant of, and a DM the user
// is not in is left alone.
func TestSyncUserDMsProposesADeviceThatHasSincePublished(t *testing.T) {
	e, dsvc := dmEnv(t)
	alice, aliceTok := e.NewUser("alice")
	bob, _ := e.NewUser("bob")
	_, carolTok := e.NewUser("carol")
	late := seedDevices(t, e, bob, 1)[0]
	ch, _ := openDM(t, e, aliceTok, bob)
	group := seedTextGroup(t, e, ch, id.ID{})
	other, _ := openDM(t, e, carolTok, alice)
	seedTextGroup(t, e, other, id.ID{})
	if err := api.SyncGroupMembers(t.Context(), e.Repo, dsvc, mustChannel(t, e, ch), e.Clk.Now().Unix()); err != nil {
		t.Fatalf("SyncGroupMembers: %v", err)
	}
	for _, a := range dsvc.Adds {
		if a.Device == late {
			t.Fatal("a device with no KeyPackage was proposed")
		}
	}
	dsvc.Reset()

	seedKeyPackage(t, e, late)
	if err := api.SyncUserDMs(t.Context(), e.Repo, dsvc, bob, e.Clk.Now().Unix()); err != nil {
		t.Fatalf("SyncUserDMs: %v", err)
	}
	if len(dsvc.Adds) != 1 || dsvc.Adds[0].Group != group || dsvc.Adds[0].Device != late {
		t.Fatalf("Adds = %+v, want exactly bob's late device into the DM's group", dsvc.Adds)
	}
}

func TestEveryRecipientDeviceIsAddedAndGetsAWelcome(t *testing.T) {
	e, dsvc := dmEnv(t)
	_, aliceTok := e.NewUser("alice")
	bob, _ := e.NewUser("bob")
	// Bob has three devices, each with an available KeyPackage.
	bobDevices := seedDevices(t, e, bob, 3)
	for _, d := range bobDevices {
		seedKeyPackage(t, e, d)
	}
	ch, _ := openDM(t, e, aliceTok, bob)

	// The DM channel exists before the group does: the creating client registers
	// the MLS group, and the instance then adds every eligible device.
	group := seedTextGroup(t, e, ch, id.ID{})
	if err := api.SyncGroupMembers(t.Context(), e.Repo, dsvc, mustChannel(t, e, ch), e.Clk.Now().Unix()); err != nil {
		t.Fatalf("SyncGroupMembers: %v", err)
	}
	if len(dsvc.Adds) != len(bobDevices) {
		t.Fatalf("Adds = %d, want %d", len(dsvc.Adds), len(bobDevices))
	}
	for _, a := range dsvc.Adds {
		if a.Group != group {
			t.Fatalf("Add on the wrong group: %x", a.Group)
		}
		if !slices.Contains(bobDevices, a.Device) {
			t.Fatalf("Add for a device that is not bob's: %x", a.Device)
		}
	}
	// A device with no KeyPackage is not proposed; the delivery service would
	// refuse it, and proposing it would burn an election round.
	quiet := seedDevices(t, e, bob, 1)[0]
	dsvc.Adds = nil
	if err := api.SyncGroupMembers(t.Context(), e.Repo, dsvc, mustChannel(t, e, ch), e.Clk.Now().Unix()); err != nil {
		t.Fatalf("SyncGroupMembers: %v", err)
	}
	for _, a := range dsvc.Adds {
		if a.Device == quiet {
			t.Fatal("a device with no KeyPackage was proposed")
		}
	}
}

func TestTheBindingTargetIsTheDMAndTheCommunityIsNull(t *testing.T) {
	e, _ := dmEnv(t)
	_, aliceTok := e.NewUser("alice")
	bob, _ := e.NewUser("bob")
	ch, _ := openDM(t, e, aliceTok, bob)
	row, err := e.Repo.GetChannel(t.Context(), ch)
	if err != nil {
		t.Fatalf("GetChannel: %v", err)
	}
	inst, err := e.Repo.GetInstance(t.Context())
	if err != nil {
		t.Fatalf("GetInstance: %v", err)
	}
	b := api.ExpectedBinding(inst.InstanceID, row)
	if b.TargetID != ch {
		t.Fatalf("binding target_id = %x, want %x", b.TargetID, ch)
	}
	if b.CommunityID != nil {
		t.Fatalf("binding community_id = %x, want null", *b.CommunityID)
	}
	if b.Kind != api.GroupText {
		t.Fatalf("binding kind = %d, want 0", b.Kind)
	}
	// protocol/01 rule 1: instance_id is the first thing the delivery service
	// compares, so a zero one makes the whole check vacuous.
	if b.InstanceID != inst.InstanceID {
		t.Fatalf("binding instance_id = %x, want %x", b.InstanceID, inst.InstanceID)
	}
	if b.InstanceID == (id.ID{}) {
		t.Fatal("binding instance_id is the zero id")
	}
	if b.V != 1 || b.E2EEVersion != 1 || b.MediaVersion != 0 {
		t.Fatalf("binding versions = (v %d, e2ee %d, media %d), want (1, 1, 0)",
			b.V, b.E2EEVersion, b.MediaVersion)
	}
}

func TestABlockedUserCannotOpenADM(t *testing.T) {
	e, _ := dmEnv(t)
	_, aliceTok := e.NewUser("alice")
	bob, _ := e.NewUser("bob")
	// A disabled account is the v1 block: interfaces.md §4.3 gives users a
	// disabled_at column and no per-user block list, so "blocked" is the account
	// state the instance actually stores.
	if err := e.Repo.SetUserDisabled(t.Context(), bob, ptr(e.Clk.Now().Unix())); err != nil {
		t.Fatalf("SetUserDisabled: %v", err)
	}
	_, status := openDM(t, e, aliceTok, bob)
	if status != http.StatusForbidden {
		t.Fatalf("DM to a disabled account = %d, want 403", status)
	}
}

func TestGETDMsListsTheCallersDMsNewestFirst(t *testing.T) {
	e, _ := dmEnv(t)
	alice, aliceTok := e.NewUser("alice")
	bob, bobTok := e.NewUser("bob")
	carol, carolTok := e.NewUser("carol")

	dm, _ := openDM(t, e, aliceTok, bob)
	e.Clk.Advance(time.Second)
	group, _ := openDM(t, e, aliceTok, bob, carol)

	list := func(tok string) [][]cbor.RawMessage {
		t.Helper()
		status, body := e.Do(http.MethodGet, "/v1/dms", tok, nil)
		if status != http.StatusOK {
			t.Fatalf("GET /v1/dms = %d (%x)", status, body)
		}
		var out [][]cbor.RawMessage
		mustUnmarshalBody(t, body, &out)
		return out
	}
	decode := func(row []cbor.RawMessage) (id.ID, uint64, []id.ID) {
		t.Helper()
		if len(row) != 3 {
			t.Fatalf("DM row has %d elements, want 3", len(row))
		}
		var ch id.ID
		var kind uint64
		var members []id.ID
		mustUnmarshal(t, row[0], &ch)
		mustUnmarshal(t, row[1], &kind)
		mustUnmarshal(t, row[2], &members)
		return ch, kind, members
	}

	rows := list(aliceTok)
	if len(rows) != 2 {
		t.Fatalf("alice's DMs = %d, want 2", len(rows))
	}
	ch, kind, members := decode(rows[0])
	if ch != group || kind != uint64(api.ChannelGroupDM) || !slices.Equal(members, sortedIDs(alice, bob, carol)) {
		t.Fatalf("first DM = %x kind %d members %x, want the group DM", ch, kind, members)
	}
	ch, kind, members = decode(rows[1])
	if ch != dm || kind != uint64(api.ChannelDM) || !slices.Equal(members, sortedIDs(alice, bob)) {
		t.Fatalf("second DM = %x kind %d members %x, want the 1:1 DM", ch, kind, members)
	}
	if rows := list(bobTok); len(rows) != 2 {
		t.Fatalf("bob's DMs = %d, want 2", len(rows))
	}
	if rows := list(carolTok); len(rows) != 1 {
		t.Fatalf("carol's DMs = %d, want 1", len(rows))
	}
	_, daveTok := e.NewUser("dave")
	if rows := list(daveTok); len(rows) != 0 {
		t.Fatalf("dave's DMs = %d, want none", len(rows))
	}
}

// A DM's participants, and only they, may register its text and call groups
// (02 invariant 1) and be added to them (invariant 4).
func TestDMParticipantsMayRegisterAndBeAdded(t *testing.T) {
	e, _ := dmEnv(t)
	alice, aliceTok := e.NewUser("alice")
	bob, _ := e.NewUser("bob")
	eve, _ := e.NewUser("eve")
	ch, _ := openDM(t, e, aliceTok, bob)

	src := api.StructureChannels{Repo: e.Repo}
	dmBinding := func(kind uint8) ds.Binding {
		return ds.Binding{V: 1, TargetID: ch, Kind: kind, E2EEVersion: 1}
	}
	for _, kind := range []uint8{api.GroupText, api.GroupCall} {
		if err := src.MayRegister(t.Context(), alice, dmBinding(kind)); err != nil {
			t.Fatalf("a participant registering kind %d: %v", kind, err)
		}
		if err := src.MayRegister(t.Context(), eve, dmBinding(kind)); !errors.Is(err, ds.ErrNotEligible) {
			t.Fatalf("a non-participant registering kind %d = %v, want ds.ErrNotEligible", kind, err)
		}
	}
	cid := id.New()
	underCommunity := dmBinding(api.GroupText)
	underCommunity.CommunityID = &cid
	if err := src.MayRegister(t.Context(), alice, underCommunity); !errors.Is(err, ds.ErrBindingTarget) {
		t.Fatalf("a DM bound under a community = %v, want ds.ErrBindingTarget", err)
	}
	noSuchDM := dmBinding(api.GroupText)
	noSuchDM.TargetID = id.New()
	if err := src.MayRegister(t.Context(), alice, noSuchDM); !errors.Is(err, ds.ErrBindingTarget) {
		t.Fatalf("a DM group naming no DM = %v, want ds.ErrBindingTarget", err)
	}

	g := store.GroupRow{
		GroupID: id.New(), Binding: []byte{0x80}, Kind: api.GroupText, TargetID: ch,
		Ciphersuite: 1, ExternalSenderKeyID: id.New(), E2EEVersion: 1, PolicyVersion: 1,
		Created: e.Clk.Now().Unix(),
	}
	if err := e.Repo.CreateGroup(t.Context(), g); err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}
	acl := api.ResolverACL{Repo: e.Repo}
	for user, want := range map[id.ID]bool{alice: true, bob: true, eve: false} {
		got, err := acl.Eligible(t.Context(), g.GroupID, user)
		if err != nil || got != want {
			t.Fatalf("Eligible(%x) = %v, %v; want %v", user, got, err, want)
		}
	}
}
