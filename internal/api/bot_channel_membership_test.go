package api_test

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"testing"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

// botMembershipFixture is the catalog fixture with a recording delivery service as Deps.BotMembership, a text group for
// its text channel, and a KeyPackage for the Bot's device and the member's.
func botMembershipFixture(t *testing.T) (*catalogFixture, *voidingDS, id.ID) {
	t.Helper()
	dsvc := &voidingDS{recordingDS: &recordingDS{}}
	f := newCatalogFixtureOn(t, newAPIRepo(t), func(d *api.Deps) { d.BotMembership = dsvc })
	ctx := context.Background()
	now := f.deps.Clock.Now().Unix()
	cid := f.community
	group := store.GroupRow{GroupID: id.New(), Binding: []byte{0x80}, Kind: 0, CommunityID: &cid, TargetID: f.text,
		Ciphersuite: 1, ExternalSenderKeyID: id.New(), E2EEVersion: 1, MediaVersion: 1, PolicyVersion: 1, Created: now}
	if err := f.deps.Repo.CreateGroup(ctx, group); err != nil {
		t.Fatal(err)
	}
	for _, device := range []id.ID{f.botDevice, f.memberDevice.ID} {
		ref := id.New()
		if err := f.deps.Repo.PutKeyPackages(ctx, device, []store.KeyPackageRow{{DeviceID: device, KPRef: ref[:], Blob: []byte{1}, Expires: now + 86_400, Created: now}}); err != nil {
			t.Fatal(err)
		}
	}
	return f, dsvc, group.GroupID
}

// voidingDS is the recording delivery service with ds.DS's VoidAddsOf (Task 22 fix round 1, M1).
type voidingDS struct {
	*recordingDS
	voided []struct {
		Group   id.ID
		Devices []id.ID
	}
}

func (d *voidingDS) VoidAddsOf(_ context.Context, g id.ID, devices []id.ID) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.voided = append(d.voided, struct {
		Group   id.ID
		Devices []id.ID
	}{g, slices.Clone(devices)})
	return len(devices), nil
}

func addedDevices(dsvc *voidingDS, group id.ID) []id.ID {
	dsvc.mu.Lock()
	defer dsvc.mu.Unlock()
	var out []id.ID
	for _, a := range dsvc.Adds {
		if a.Group == group {
			out = append(out, a.Device)
		}
	}
	return out
}

// Task 22 (Task 16 ruling): the instance proposes the community's Bot only into the groups of its Bot channels. Members
// are still proposed into every channel they can view; marking a channel proposes the Bot into its group at once, and
// the marking member's client commits that Add.
func TestTheBotIsProposedOnlyIntoBotChannelGroups(t *testing.T) {
	f, dsvc, group := botMembershipFixture(t)
	ctx := context.Background()
	ch, err := f.deps.Repo.GetChannel(ctx, f.text)
	if err != nil {
		t.Fatal(err)
	}
	if err := api.MaterialiseChannelMembers(ctx, f.deps.Repo, dsvc, ch, f.deps.Clock.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	added := addedDevices(dsvc, group)
	if !slices.Contains(added, f.memberDevice.ID) {
		t.Fatalf("Adds %v leave out the member's device", added)
	}
	if slices.Contains(added, f.botDevice) {
		t.Fatalf("the Bot was proposed into a channel that is not a Bot channel: %v", added)
	}
	if rec := putCBORAuth(f.h, f.botChannelPath(f.text), nil, f.ownerTok); rec.Code != http.StatusNoContent {
		t.Fatalf("mark = %d %x", rec.Code, rec.Body.Bytes())
	}
	if added := addedDevices(dsvc, group); !slices.Contains(added, f.botDevice) {
		t.Fatalf("marking the channel did not propose the Bot: %v", added)
	}
}

func (f *catalogFixture) reAdd(t *testing.T, channel id.ID, token string) (int, []uint64) {
	t.Helper()
	rec := postCBORAuth(f.h, f.botChannelPath(channel)+"/re-add", nil, token)
	if rec.Code != http.StatusOK {
		return rec.Code, nil
	}
	var body []uint64
	if err := cborx.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("re-add body %x: %v", rec.Body.Bytes(), err)
	}
	return rec.Code, body
}

// The Bot page's "Add the Bot again" (Task 16's needs re-add: the Bot's leaf is in the group but it holds no state for
// it): a Bot still holding a leaf is proposed out first ([1], ask again once that commits), then proposed in ([0]).
func TestReAddProposesTheBotOutThenIn(t *testing.T) {
	f, dsvc, group := botMembershipFixture(t)
	ctx := context.Background()
	if code, _ := f.reAdd(t, f.text, f.ownerTok); code != http.StatusConflict {
		t.Fatalf("re-add into a channel that is not a Bot channel = %d, want 409", code)
	}
	if rec := putCBORAuth(f.h, f.botChannelPath(f.text), nil, f.ownerTok); rec.Code != http.StatusNoContent {
		t.Fatalf("mark = %d", rec.Code)
	}
	if code, _ := f.reAdd(t, f.text, f.memberTok); code != http.StatusForbidden {
		t.Fatalf("a plain member's re-add = %d, want 403", code)
	}
	if err := f.deps.Repo.ReplaceMembers(ctx, group, 3, []store.MemberRow{
		{GroupID: group, LeafIndex: 0, UserID: f.owner.ID, DeviceID: id.New(), SignatureKey: make([]byte, 32), AddedEpoch: 1},
		{GroupID: group, LeafIndex: 1, UserID: f.botUser, DeviceID: f.botDevice, SignatureKey: make([]byte, 32), AddedEpoch: 2},
	}); err != nil {
		t.Fatal(err)
	}
	dsvc.Adds = nil
	code, body := f.reAdd(t, f.text, f.adminTok)
	if code != http.StatusOK || !slices.Equal(body, []uint64{1}) {
		t.Fatalf("re-add with the Bot's leaf in the group = %d %v, want 200 [1]", code, body)
	}
	if devices := dsvc.leafDevices(); !slices.Contains(devices, f.botDevice) {
		t.Fatalf("the Bot's leaf was not proposed out: %v", devices)
	}
	if added := addedDevices(dsvc, group); len(added) != 0 {
		t.Fatalf("an Add went with the Remove: %v", added)
	}
	removed := uint64(3)
	if err := f.deps.Repo.ReplaceMembers(ctx, group, 4, []store.MemberRow{
		{GroupID: group, LeafIndex: 0, UserID: f.owner.ID, DeviceID: id.New(), SignatureKey: make([]byte, 32), AddedEpoch: 1},
		{GroupID: group, LeafIndex: 1, UserID: f.botUser, DeviceID: f.botDevice, SignatureKey: make([]byte, 32), AddedEpoch: 2, RemovedEpoch: &removed},
	}); err != nil {
		t.Fatal(err)
	}
	code, body = f.reAdd(t, f.text, f.adminTok)
	if code != http.StatusOK || !slices.Equal(body, []uint64{0}) {
		t.Fatalf("re-add once the Bot is out = %d %v, want 200 [0]", code, body)
	}
	if added := addedDevices(dsvc, group); !slices.Contains(added, f.botDevice) {
		t.Fatalf("the Bot was not proposed in: %v", added)
	}
}

// Task 22 fix round 1 (M1): an unmark voids the Bot's Add still outstanding in the channel's group, so no member's commit
// carries the Bot into a channel that is no longer a Bot channel, and proposes out a leaf the Bot already holds there.
func TestUnmarkVoidsTheBotsPendingAddAndProposesItsLeafOut(t *testing.T) {
	f, dsvc, group := botMembershipFixture(t)
	if rec := putCBORAuth(f.h, f.botChannelPath(f.text), nil, f.ownerTok); rec.Code != http.StatusNoContent {
		t.Fatalf("mark = %d", rec.Code)
	}
	if err := f.deps.Repo.ReplaceMembers(context.Background(), group, 3, []store.MemberRow{
		{GroupID: group, LeafIndex: 0, UserID: f.owner.ID, DeviceID: id.New(), SignatureKey: make([]byte, 32), AddedEpoch: 1},
		{GroupID: group, LeafIndex: 1, UserID: f.botUser, DeviceID: f.botDevice, SignatureKey: make([]byte, 32), AddedEpoch: 2},
	}); err != nil {
		t.Fatal(err)
	}
	if rec := deleteAuthCBOR(f.h, f.botChannelPath(f.text), f.ownerTok); rec.Code != http.StatusNoContent {
		t.Fatalf("unmark = %d", rec.Code)
	}
	if len(dsvc.voided) != 1 || dsvc.voided[0].Group != group || !slices.Equal(dsvc.voided[0].Devices, []id.ID{f.botDevice}) {
		t.Fatalf("voided = %+v, want the Bot's device in %s", dsvc.voided, group)
	}
	if devices := dsvc.leafDevices(); !slices.Contains(devices, f.botDevice) {
		t.Fatalf("the Bot's leaf was not proposed out on unmark: %v", devices)
	}
}

// Task 22 fix round 1: marking or unmarking a channel needs view_channel there too, so a manager cannot put the Bot into
// (or take it out of) a channel they cannot see. A hidden channel answers like an unknown one.
func TestMarkingNeedsViewOfTheChannel(t *testing.T) {
	f, _, _ := botMembershipFixture(t)
	ctx := context.Background()
	managers := store.RoleRow{ID: id.New(), CommunityID: f.community, Name: "managers", Position: 3, Created: f.deps.Clock.Now().Unix(),
		Allow: uint64(api.PermViewChannel | api.PermManageCommunity)}
	if err := f.deps.Repo.PutRole(ctx, managers); err != nil {
		t.Fatal(err)
	}
	if err := f.deps.Repo.PutMemberRole(ctx, f.community, f.member.ID, managers.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.deps.Repo.PutOverwrite(ctx, store.OverwriteRow{ChannelID: f.text, TargetKind: 1, TargetID: f.member.ID,
		Deny: uint64(api.PermViewChannel)}); err != nil {
		t.Fatal(err)
	}
	if rec := putCBORAuth(f.h, f.botChannelPath(f.text), nil, f.memberTok); rec.Code != http.StatusNotFound {
		t.Fatalf("a manager who cannot view the channel marking it = %d, want 404", rec.Code)
	}
	if rec := putCBORAuth(f.h, f.botChannelPath(f.text), nil, f.ownerTok); rec.Code != http.StatusNoContent {
		t.Fatalf("the owner marking it = %d", rec.Code)
	}
	if rec := deleteAuthCBOR(f.h, f.botChannelPath(f.text), f.memberTok); rec.Code != http.StatusNotFound {
		t.Fatalf("a manager who cannot view the channel unmarking it = %d, want 404", rec.Code)
	}
	if got := f.listBotChannels(t, f.ownerTok); !slices.Equal(got, []id.ID{f.text}) {
		t.Fatalf("Bot channels = %v, want the mark kept", got)
	}
}

// Task 22 fix round 1 (L2): a re-add is audited like a mark and an unmark.
func TestReAddIsAudited(t *testing.T) {
	f, _, _ := botMembershipFixture(t)
	if rec := putCBORAuth(f.h, f.botChannelPath(f.text), nil, f.ownerTok); rec.Code != http.StatusNoContent {
		t.Fatalf("mark = %d", rec.Code)
	}
	if code, _ := f.reAdd(t, f.text, f.ownerTok); code != http.StatusOK {
		t.Fatalf("re-add = %d", code)
	}
	rows, err := f.deps.Repo.ListAudit(context.Background(), 0, 1000)
	var readd []store.AuditRow
	for _, row := range rows {
		if row.Action == "bot.channel_readd" {
			readd = append(readd, row)
		}
	}
	if err != nil || len(readd) != 1 || readd[0].Target != f.text.String() || readd[0].Actor == nil || *readd[0].Actor != f.owner.ID {
		t.Fatalf("audit rows = %+v, %v; want one bot.channel_readd by the owner", readd, err)
	}
}

// failingBotChannels is a repository whose Bot-channel read fails (a transient store error).
type failingBotChannels struct{ store.Repository }

func (failingBotChannels) ListBotChannels(context.Context, id.ID) ([]store.BotChannelRow, error) {
	return nil, errors.New("transient")
}

// Task 22 fix round 1 (L1): when the Bot-channel rule cannot be read, the sync still proposes the Removes of members who
// lost access, and leaves the Bot out of the Adds (fail closed for the Bot only).
func TestABotChannelReadFailureStillRunsTheRemoves(t *testing.T) {
	f, dsvc, group := botMembershipFixture(t)
	ctx := context.Background()
	gone, goneDevice := id.New(), id.New()
	now := f.deps.Clock.Now().Unix()
	if err := f.deps.Repo.CreateUser(ctx, store.UserRow{ID: gone, Username: "gone" + gone.String()[:8], Kind: 0, UMKPub: make([]byte, 32),
		SSKPub: make([]byte, 32), SigUMKSSK: make([]byte, 64), Created: now}); err != nil {
		t.Fatal(err)
	}
	if err := f.deps.Repo.CreateDevice(ctx, store.DeviceRow{ID: goneDevice, UserID: gone, DSKPub: make([]byte, 32), CredentialBlob: []byte{1},
		Created: now, LastSeen: now}); err != nil {
		t.Fatal(err)
	}
	if err := f.deps.Repo.ReplaceMembers(ctx, group, 1, []store.MemberRow{
		{GroupID: group, LeafIndex: 0, UserID: gone, DeviceID: goneDevice, SignatureKey: make([]byte, 32), AddedEpoch: 1},
	}); err != nil {
		t.Fatal(err)
	}
	ch, err := f.deps.Repo.GetChannel(ctx, f.text)
	if err != nil {
		t.Fatal(err)
	}
	if err := api.MaterialiseChannelMembers(ctx, failingBotChannels{f.deps.Repo}, dsvc, ch, now); err != nil {
		t.Logf("sync answered %v (the Bot-channel read failed)", err)
	}
	if devices := dsvc.leafDevices(); !slices.Contains(devices, goneDevice) {
		t.Fatalf("a member who lost access was not proposed out: %v", devices)
	}
	if added := addedDevices(dsvc, group); slices.Contains(added, f.botDevice) || !slices.Contains(added, f.memberDevice.ID) {
		t.Fatalf("Adds = %v, want the member's device and not the Bot's", added)
	}
}
