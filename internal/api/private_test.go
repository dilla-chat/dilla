package api_test

import (
	"bytes"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"sync"
	"testing"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

// seedCommunityOfSize creates n members, each with one device holding one
// KeyPackage, all holding the named role.
func seedCommunityOfSize(t *testing.T, e *env, cid id.ID, ownerTok string, role id.ID, n int) []id.ID {
	t.Helper()
	users := make([]id.ID, 0, n)
	for i := range n {
		u, tok := e.NewUser(fmt.Sprintf("m%04d", i))
		joinCommunity(t, e, cid, tok)
		grant(t, e, cid, ownerTok, u, role, http.StatusNoContent)
		for _, dev := range seedDevices(t, e, u, 1) {
			seedKeyPackage(t, e, dev)
		}
		users = append(users, u)
	}
	return users
}

// devicesOf is every device of user, in the order ListDevicesByUser returns them.
func devicesOf(t *testing.T, e *env, user id.ID) []id.ID {
	t.Helper()
	rows, err := e.Repo.ListDevicesByUser(t.Context(), user)
	if err != nil {
		t.Fatalf("ListDevicesByUser: %v", err)
	}
	out := make([]id.ID, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.ID)
	}
	return out
}

// closeToEveryoneButRole makes ch a private channel: @everyone is denied view on it and role is
// allowed it. Every new community gives @everyone view, so without the deny every member is
// eligible whatever roles they hold, and a test about roles would test nothing.
func closeToEveryoneButRole(t *testing.T, e *env, cid, ch id.ID, ownerTok string, role id.ID) {
	t.Helper()
	roles, err := e.Repo.ListRoles(t.Context(), cid)
	if err != nil {
		t.Fatalf("ListRoles: %v", err)
	}
	var everyone id.ID
	for _, r := range roles {
		if r.Position == 0 {
			everyone = r.ID
		}
	}
	for _, o := range []struct {
		target      id.ID
		allow, deny uint64
	}{
		{everyone, 0, uint64(api.PermViewChannel)},
		{role, uint64(api.PermViewChannel), 0},
	} {
		if status, body := e.Do(http.MethodPut,
			"/v1/channels/"+ch.String()+"/overwrites/0/"+o.target.String(), ownerTok,
			[]any{o.allow, o.deny}); status != http.StatusNoContent {
			t.Fatalf("PUT overwrite = %d (%x)", status, body)
		}
	}
}

// What this test pins about PRODUCTION code is the materialiser: EligibleUsers resolves the role
// (and the @everyone deny) for a thousand members, channel_members holds exactly the insiders and
// the owner, and SyncGroupMembers hands every insider's device to ONE ProposeAddBatch call. The
// split into four commits of at most 256 is the recording double's contract (it runs
// ds.PlanBatches and counts a commit per batch), so `Commits == 4` checks that the call carried
// all 1,000 devices, not how the delivery service commits them: that is pinned against the real
// service by internal/ds's batch tests and, with real clients and real commits, by
// internal/testkit's TestAThousandDeviceJoinStormCompletesInExactlyFourCommits.
func TestAPrivateChannelInA1000MemberCommunityBatchesAt256(t *testing.T) {
	e, cid, ownerTok := channelEnv(t)
	api.NewRoles(e.Repo, e.Clk, "dilla.example", slog.New(slog.DiscardHandler)).Register(e.Mux)
	dsvc := &recordingDS{}

	role := createRole(t, e, cid, ownerTok, "insiders", 10, api.PermViewChannel|api.PermSendMessages)
	seedCommunityOfSize(t, e, cid, ownerTok, role, 1000)
	// A member without the role, with a device that could be added: the channel is private, so
	// the deny on @everyone keeps them out.
	outsider, outsiderTok := e.NewUser("outsider")
	joinCommunity(t, e, cid, outsiderTok)
	outsiderDevice := seedDevices(t, e, outsider, 1)[0]
	seedKeyPackage(t, e, outsiderDevice)

	ch, _, _ := newChannel(t, e, cid, ownerTok, 0, 0, 0, "secret")
	closeToEveryoneButRole(t, e, cid, ch, ownerTok, role)
	group := seedTextGroup(t, e, ch, cid)

	if err := api.MaterialiseChannelMembers(t.Context(), e.Repo, dsvc,
		mustChannel(t, e, ch), e.Clk.Now().Unix()); err != nil {
		t.Fatalf("MaterialiseChannelMembers: %v", err)
	}

	members, err := e.Repo.ListChannelMembers(t.Context(), ch)
	if err != nil {
		t.Fatalf("ListChannelMembers: %v", err)
	}
	if len(members) != 1001 { // 1000 plus the owner
		t.Fatalf("channel_members = %d, want 1001", len(members))
	}
	if slices.Contains(members, outsider) {
		t.Fatal("a member without the role is in a private channel's channel_members")
	}
	// 1000, not 1001: channel_members holds the owner too, but channelEnv's owner
	// device published no KeyPackage, and SyncGroupMembers skips any device with
	// CountKeyPackages == 0 because the delivery service would refuse the Add and
	// the election round would be wasted. The assertion below states that rule
	// rather than hiding it behind an off-by-one.
	if len(dsvc.Adds) != 1000 {
		t.Fatalf("Adds = %d, want 1000", len(dsvc.Adds))
	}
	ownerDevices := devicesOf(t, e, ownerOf(t, e, cid))
	for _, a := range dsvc.Adds {
		if a.Group != group {
			t.Fatalf("Add on %s, want %s", a.Group, group)
		}
		if slices.Contains(ownerDevices, a.Device) {
			t.Fatal("the owner's device was proposed although it published no KeyPackage")
		}
		if a.Device == outsiderDevice {
			t.Fatal("the device of a member without the role was proposed for a private channel")
		}
	}
	// The double's contract (see above): one batch call of 1,000 devices is four batches.
	if dsvc.Commits != 4 {
		t.Fatalf("commit requests = %d; 1000 devices at 256 per commit is 4", dsvc.Commits)
	}
}

// As above, `Commits == 2` is the recording double's split of the one batch call; what production
// code decides here is that exactly the 300 new insiders are handed over.
func TestARoleGrantThatOpensTheChannelTo300MoreBatchesTheSameWay(t *testing.T) {
	e, cid, ownerTok := channelEnv(t)
	api.NewRoles(e.Repo, e.Clk, "dilla.example", slog.New(slog.DiscardHandler)).Register(e.Mux)
	dsvc := &recordingDS{}
	role := createRole(t, e, cid, ownerTok, "insiders", 10, api.PermViewChannel)
	ch, _, _ := newChannel(t, e, cid, ownerTok, 0, 0, 0, "secret")
	closeToEveryoneButRole(t, e, cid, ch, ownerTok, role)
	seedTextGroup(t, e, ch, cid)
	if err := api.MaterialiseChannelMembers(t.Context(), e.Repo, dsvc, mustChannel(t, e, ch), e.Clk.Now().Unix()); err != nil {
		t.Fatalf("first materialise: %v", err)
	}
	dsvc.Reset()

	seedCommunityOfSize(t, e, cid, ownerTok, role, 300)
	if err := api.MaterialiseChannelMembers(t.Context(), e.Repo, dsvc, mustChannel(t, e, ch), e.Clk.Now().Unix()); err != nil {
		t.Fatalf("second materialise: %v", err)
	}
	if len(dsvc.Adds) != 300 {
		t.Fatalf("Adds = %d, want 300", len(dsvc.Adds))
	}
	if dsvc.Commits != 2 {
		t.Fatalf("commit requests = %d, want 2", dsvc.Commits)
	}
}

// A role revoked between two batches of a join storm drops the user from the batches still to
// come. The revoke goes through the PRODUCTION route (DELETE …/roles/{role_id}), whose resolver and
// materialiser must find that the user no longer sees the channel — which they can only because
// @everyone is denied view on it — and take the user out of channel_members. The between-batch
// re-read of channel_members is the recording double's contract (it stands in for the delivery
// service's eligibility re-check); the real re-check, over the channel ACL, is pinned by
// internal/ds's TestADrainDropsWhatStoppedBeingEligibleSinceTheBatchWasPlanned.
func TestAMemberRemovedMidBatchIsDroppedFromTheRest(t *testing.T) {
	e, cid, ownerTok := channelEnv(t)
	api.NewRoles(e.Repo, e.Clk, "dilla.example", slog.New(slog.DiscardHandler)).Register(e.Mux)
	role := createRole(t, e, cid, ownerTok, "insiders", 10, api.PermViewChannel)
	users := seedCommunityOfSize(t, e, cid, ownerTok, role, 300)
	ch, _, _ := newChannel(t, e, cid, ownerTok, 0, 0, 0, "secret")
	closeToEveryoneButRole(t, e, cid, ch, ownerTok, role)
	seedTextGroup(t, e, ch, cid)

	// The victim must be in the LAST batch, or the test is a coin flip: batch
	// membership follows ListChannelMembers order, which is user_id order over 16
	// random bytes, so users[len(users)-1] (creation order) lands in the first 256
	// of 300 about 85% of the time and is already added before OnCommit fires.
	// The member whose user_id sorts last is in the final batch by construction.
	victim := slices.MaxFunc(users, func(a, b id.ID) int { return bytes.Compare(a[:], b[:]) })
	first := slices.MinFunc(users, func(a, b id.ID) int { return bytes.Compare(a[:], b[:]) })
	var once sync.Once
	revokeStatus := 0
	dsvc := &recordingDS{Repo: e.Repo, OnCommit: func() {
		once.Do(func() {
			revokeStatus, _ = e.Do(http.MethodDelete,
				"/v1/communities/"+cid.String()+"/members/"+victim.String()+"/roles/"+role.String(),
				ownerTok, nil)
		})
	}}
	if err := api.MaterialiseChannelMembers(t.Context(), e.Repo, dsvc, mustChannel(t, e, ch), e.Clk.Now().Unix()); err != nil {
		t.Fatalf("MaterialiseChannelMembers: %v", err)
	}
	if revokeStatus != http.StatusNoContent {
		t.Fatalf("the revoke between the batches answered %d, want 204", revokeStatus)
	}
	if members, _ := e.Repo.ListChannelMembers(t.Context(), ch); slices.Contains(members, victim) {
		t.Fatal("the revoke route left the victim in channel_members: the channel is not private")
	}
	victimDevices := devicesOf(t, e, victim)
	for _, a := range dsvc.Adds {
		if slices.Contains(victimDevices, a.Device) {
			t.Fatal("a user removed during the first batch was still added by a later one")
		}
	}
	// And the positive half: the first batch really did run, so the assertion
	// above is about eligibility and not about nothing having happened.
	firstDevices := devicesOf(t, e, first)
	var sawFirst bool
	for _, a := range dsvc.Adds {
		if slices.Contains(firstDevices, a.Device) {
			sawFirst = true
		}
	}
	if !sawFirst {
		t.Fatal("no device from the first batch was added; the test proved nothing")
	}
}

// privateChannel is a channel @everyone cannot see and the returned role can: the shape of a
// private channel, with its text group seeded.
func privateChannel(t *testing.T, e *env, cid id.ID, ownerTok string) (ch, role, group id.ID) {
	t.Helper()
	role = createRole(t, e, cid, ownerTok, "insiders", 10, api.PermViewChannel)
	ch, _, _ = newChannel(t, e, cid, ownerTok, 0, 0, 0, "secret")
	closeToEveryoneButRole(t, e, cid, ch, ownerTok, role)
	return ch, role, seedTextGroup(t, e, ch, cid)
}

// A grant that opens a private channel to one member materialises the row and proposes the
// member's devices for the channel's text group; the revoke drops the row and proposes the
// Remove of the leaf the member came to hold.
func TestAGrantProposesTheGranteesDevicesAndARevokeRemovesThem(t *testing.T) {
	e, cid, ownerTok := channelEnv(t)
	dsvc := &recordingDS{}
	api.NewRoles(e.Repo, e.Clk, "dilla.example", slog.New(slog.DiscardHandler)).WithDS(dsvc).Register(e.Mux)
	ch, role, group := privateChannel(t, e, cid, ownerTok)

	member, memberTok := e.NewUser("insider")
	joinCommunity(t, e, cid, memberTok)
	device := seedDevices(t, e, member, 1)[0]
	seedKeyPackage(t, e, device)
	if members, _ := e.Repo.ListChannelMembers(t.Context(), ch); slices.Contains(members, member) {
		t.Fatal("a member @everyone's deny hides the channel from is in channel_members")
	}
	dsvc.Reset()

	grant(t, e, cid, ownerTok, member, role, http.StatusNoContent)
	if members, _ := e.Repo.ListChannelMembers(t.Context(), ch); !slices.Contains(members, member) {
		t.Fatal("the grant did not materialise channel_members")
	}
	if len(dsvc.Adds) != 1 || dsvc.Adds[0].Group != group || dsvc.Adds[0].Device != device {
		t.Fatalf("Adds = %+v, want the grantee's one device with a KeyPackage in %s", dsvc.Adds, group)
	}

	// The commit the Add asked for lands: the member holds a leaf.
	seedMemberDevice(t, e, group, member, device, 1)
	dsvc.Reset()
	if status, _ := e.Do(http.MethodDelete,
		"/v1/communities/"+cid.String()+"/members/"+member.String()+"/roles/"+role.String(),
		ownerTok, nil); status != http.StatusNoContent {
		t.Fatal("DELETE role grant failed")
	}
	if members, _ := e.Repo.ListChannelMembers(t.Context(), ch); slices.Contains(members, member) {
		t.Fatal("channel_members still lists a user whose role was revoked")
	}
	removes := dsvc.removes()
	if len(removes) != 1 || removes[0].Group != group || removes[0].Leaf != 1 {
		t.Fatalf("Removes = %+v, want leaf 1 of %s", removes, group)
	}
}

// A grant that changes nothing about who may see a channel does not re-derive it: the role
// routes call EligibleUsers, which is O(members), once per channel whose verdict for the user
// actually moved.
func TestAGrantThatChangesNoVerdictProposesNothing(t *testing.T) {
	e, cid, ownerTok := channelEnv(t)
	dsvc := &recordingDS{}
	api.NewRoles(e.Repo, e.Clk, "dilla.example", slog.New(slog.DiscardHandler)).WithDS(dsvc).Register(e.Mux)
	_, role, _ := privateChannel(t, e, cid, ownerTok)
	other := createRole(t, e, cid, ownerTok, "colour", 5, api.PermAddReactions)

	// An insider whose Add the recording double never turns into a leaf: any re-derivation of the
	// channel would propose that device again, so an empty record below means none ran.
	insider, insiderTok := e.NewUser("insider")
	joinCommunity(t, e, cid, insiderTok)
	seedKeyPackage(t, e, seedDevices(t, e, insider, 1)[0])
	grant(t, e, cid, ownerTok, insider, role, http.StatusNoContent)
	if len(dsvc.Adds) == 0 {
		t.Fatal("the insider's grant proposed nothing; the assertion below would be vacuous")
	}

	member, memberTok := e.NewUser("bystander")
	joinCommunity(t, e, cid, memberTok)
	seedKeyPackage(t, e, seedDevices(t, e, member, 1)[0])
	dsvc.Reset()
	grant(t, e, cid, ownerTok, member, other, http.StatusNoContent)
	if len(dsvc.Adds) != 0 || len(dsvc.removes()) != 0 {
		t.Fatalf("a grant that opens nothing issued Adds %+v and Removes %+v", dsvc.Adds, dsvc.removes())
	}
}

// Registering a channel's text group is "creating a private channel" (protocol/01 § Joining):
// every eligible device but the creator's live leaf is proposed, in batches.
func TestRegisteringAChannelGroupPopulatesIt(t *testing.T) {
	e, cid, ownerTok := channelEnv(t)
	api.NewRoles(e.Repo, e.Clk, "dilla.example", slog.New(slog.DiscardHandler)).Register(e.Mux)
	role := createRole(t, e, cid, ownerTok, "insiders", 10, api.PermViewChannel)
	users := seedCommunityOfSize(t, e, cid, ownerTok, role, 3)
	ch, _, _ := newChannel(t, e, cid, ownerTok, 0, 0, 0, "secret")
	group := seedTextGroup(t, e, ch, cid)

	dsvc := &recordingDS{}
	if err := api.SyncRegisteredGroup(t.Context(), e.Repo, dsvc, group, e.Clk.Now().Unix()); err != nil {
		t.Fatalf("SyncRegisteredGroup: %v", err)
	}
	var want []id.ID
	for _, u := range users {
		for _, d := range devicesOf(t, e, u) {
			if n, err := e.Repo.CountKeyPackages(t.Context(), d, e.Clk.Now().Unix()); err != nil {
				t.Fatalf("CountKeyPackages: %v", err)
			} else if n > 0 {
				want = append(want, d) // the one seedCommunityOfSize gave a KeyPackage
			}
		}
	}
	var got []id.ID
	for _, a := range dsvc.Adds {
		got = append(got, a.Device)
	}
	slices.SortFunc(want, func(a, b id.ID) int { return bytes.Compare(a[:], b[:]) })
	slices.SortFunc(got, func(a, b id.ID) int { return bytes.Compare(a[:], b[:]) })
	if !slices.Equal(got, want) {
		t.Fatalf("Adds = %v, want %v", got, want)
	}
	if dsvc.Commits != 1 {
		t.Fatalf("commit requests = %d, want 1", dsvc.Commits)
	}

	// A group bound to no channel (a pairing group, an interaction group) is left alone.
	stray := seedGroupOfKind(t, e, id.New(), cid, 2)
	dsvc.Reset()
	if err := api.SyncRegisteredGroup(t.Context(), e.Repo, dsvc, stray, e.Clk.Now().Unix()); err != nil {
		t.Fatalf("SyncRegisteredGroup(pairing group): %v", err)
	}
	if len(dsvc.Adds) != 0 {
		t.Fatalf("a group bound to no channel was populated: %+v", dsvc.Adds)
	}
}

// A PATCH that changes visibility re-derives channel_members.
func TestAVisibilityChangeRematerialisesTheChannel(t *testing.T) {
	e, cid, ownerTok := channelEnv(t)
	member, memberTok := e.NewUser("member")
	joinCommunity(t, e, cid, memberTok)
	ch, _, _ := newChannel(t, e, cid, ownerTok, 0, 1, 2, "lobby") // readable, discoverable
	if members, _ := e.Repo.ListChannelMembers(t.Context(), ch); !slices.Contains(members, member) {
		t.Fatalf("a new channel's members were not materialised: %v", members)
	}

	// Taken from the member behind the handler's back, so only the PATCH can notice.
	if err := e.Repo.PutOverwrite(t.Context(), store.OverwriteRow{
		ChannelID: ch, TargetKind: 1, TargetID: member, Deny: uint64(api.PermViewChannel),
	}); err != nil {
		t.Fatalf("PutOverwrite: %v", err)
	}
	if status, body := e.Do(http.MethodPatch, "/v1/channels/"+ch.String(), ownerTok,
		[]any{nil, nil, nil, uint64(api.VisPrivate), nil, nil, nil}); status != http.StatusNoContent {
		t.Fatalf("PATCH visibility = %d (%x)", status, body)
	}
	if members, _ := e.Repo.ListChannelMembers(t.Context(), ch); slices.Contains(members, member) {
		t.Fatal("the visibility change did not re-derive channel_members")
	}
}

// seedMemberDevice adds one mls_members row: user's given device at leaf in group.
func seedMemberDevice(t *testing.T, e *env, group, user, device id.ID, leaf uint32) {
	t.Helper()
	members, err := e.Repo.ListMembers(t.Context(), group)
	if err != nil {
		t.Fatalf("ListMembers: %v", err)
	}
	members = append(members, store.MemberRow{
		GroupID: group, LeafIndex: leaf, UserID: user, DeviceID: device,
		SignatureKey: make([]byte, 32),
	})
	if err := e.Repo.ReplaceMembers(t.Context(), group, 0, members); err != nil {
		t.Fatalf("ReplaceMembers: %v", err)
	}
}
