package api_test

import (
	"context"
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
func botMembershipFixture(t *testing.T) (*catalogFixture, *recordingDS, id.ID) {
	t.Helper()
	dsvc := &recordingDS{}
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

func addedDevices(dsvc *recordingDS, group id.ID) []id.ID {
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
