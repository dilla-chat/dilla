package api_test

import (
	"bytes"
	"log/slog"
	"net/http"
	"slices"
	"testing"

	"github.com/fxamacker/cbor/v2"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

// memberEnv is dmEnv plus the channel routes, the three member routes among
// them, sharing dmEnv's recording delivery service.
func memberEnv(t *testing.T) (*env, *recordingDS) {
	t.Helper()
	e, dsvc := dmEnv(t)
	c := api.NewChannels(e.Repo, dsvc, e.Clk, 10, slog.New(slog.DiscardHandler))
	c.Register(e.Mux)
	c.RegisterMembers(e.Mux)
	return e, dsvc
}

// channelMembers GETs /v1/channels/{id}/members and returns the status and the
// user ids, each the only element of its row.
func channelMembers(t *testing.T, e *env, ch id.ID, tok string) ([]id.ID, int) {
	t.Helper()
	status, body := e.Do(http.MethodGet, "/v1/channels/"+ch.String()+"/members", tok, nil)
	if status != http.StatusOK {
		return nil, status
	}
	var rows [][]cbor.RawMessage
	mustUnmarshalBody(t, body, &rows)
	out := make([]id.ID, 0, len(rows))
	for _, row := range rows {
		if len(row) != 1 {
			t.Fatalf("member row has %d elements, want 1", len(row))
		}
		var u id.ID
		mustUnmarshal(t, row[0], &u)
		out = append(out, u)
	}
	return out, status
}

func memberPath(ch, user id.ID) string {
	return "/v1/channels/" + ch.String() + "/members/" + user.String()
}

func sortedIDs(ids ...id.ID) []id.ID {
	out := slices.Clone(ids)
	slices.SortFunc(out, func(a, b id.ID) int { return bytes.Compare(a[:], b[:]) })
	return out
}

func TestGroupDMMembersRoundTrip(t *testing.T) {
	e, dsvc := memberEnv(t)
	alice, aliceTok := e.NewUser("alice")
	bob, bobTok := e.NewUser("bob")
	carol, _ := e.NewUser("carol")
	dave, daveTok := e.NewUser("dave")
	daveDevice := seedDevices(t, e, dave, 1)[0]
	seedKeyPackage(t, e, daveDevice)

	ch, status := openDM(t, e, aliceTok, bob, carol)
	if status != http.StatusCreated {
		t.Fatalf("POST /v1/dms = %d", status)
	}
	if row := mustChannel(t, e, ch); row.Kind != api.ChannelGroupDM || row.CommunityID != nil {
		t.Fatalf("group DM row = %+v", row)
	}
	group := seedTextGroup(t, e, ch, id.ID{})

	if got, status := channelMembers(t, e, ch, bobTok); status != http.StatusOK ||
		!slices.Equal(got, sortedIDs(alice, bob, carol)) {
		t.Fatalf("GET members = %x (%d)", got, status)
	}

	// Add dave: the row lands, then the instance proposes his device.
	if status, body := e.Do(http.MethodPut, memberPath(ch, dave), aliceTok, nil); status != http.StatusNoContent {
		t.Fatalf("PUT member = %d (%x)", status, body)
	}
	if got, _ := channelMembers(t, e, ch, aliceTok); !slices.Equal(got, sortedIDs(alice, bob, carol, dave)) {
		t.Fatalf("GET members after the add = %x", got)
	}
	if len(dsvc.Adds) != 1 || dsvc.Adds[0].Group != group || dsvc.Adds[0].Device != daveDevice {
		t.Fatalf("Adds = %+v, want dave's device on the group", dsvc.Adds)
	}
	// A second PUT changes nothing and, with the Add still to be committed,
	// issues nothing: the proposal the instance already holds stands.
	if err := e.Repo.PutProposal(t.Context(), store.ProposalRow{
		GroupID: group, Ref: []byte{1}, Kind: 1, TargetDevice: &daveDevice, ActionID: id.New(),
		IssuedAt: e.Clk.Now().Unix(), TTL: 3600,
	}); err != nil {
		t.Fatalf("PutProposal: %v", err)
	}
	dsvc.Adds = nil
	if status, _ := e.Do(http.MethodPut, memberPath(ch, dave), aliceTok, nil); status != http.StatusNoContent {
		t.Fatalf("second PUT member = %d", status)
	}
	if len(dsvc.Adds) != 0 {
		t.Fatalf("a second PUT re-proposed an outstanding Add: %+v", dsvc.Adds)
	}

	// Dave's device is committed into the group at leaf 3; removing him
	// proposes the Remove of that leaf.
	if err := e.Repo.ReplaceMembers(t.Context(), group, 1, []store.MemberRow{{
		GroupID: group, LeafIndex: 3, UserID: dave, DeviceID: daveDevice, SignatureKey: make([]byte, 32),
		AddedEpoch: 1,
	}}); err != nil {
		t.Fatalf("ReplaceMembers: %v", err)
	}
	if status, body := e.Do(http.MethodDelete, memberPath(ch, dave), aliceTok, nil); status != http.StatusNoContent {
		t.Fatalf("DELETE member = %d (%x)", status, body)
	}
	if got, _ := channelMembers(t, e, ch, aliceTok); !slices.Equal(got, sortedIDs(alice, bob, carol)) {
		t.Fatalf("GET members after the removal = %x", got)
	}
	removes := dsvc.removes()
	if len(removes) != 1 || removes[0].Group != group || removes[0].Leaf != 3 {
		t.Fatalf("Removes = %+v, want leaf 3 of the group", removes)
	}

	// Dave is out: he no longer sees the group DM or its members.
	if _, status := channelMembers(t, e, ch, daveTok); status != http.StatusNotFound {
		t.Fatalf("a removed participant's GET members = %d, want 404", status)
	}
	if status, _ := e.Do(http.MethodDelete, memberPath(ch, carol), daveTok, nil); status != http.StatusNotFound {
		t.Fatalf("a removed participant removing another = %d, want 404", status)
	}
	// Removing someone who is not a participant is 404.
	if status, _ := e.Do(http.MethodDelete, memberPath(ch, dave), aliceTok, nil); status != http.StatusNotFound {
		t.Fatalf("removing a non-participant = %d, want 404", status)
	}
	// A participant may always remove themself.
	if status, _ := e.Do(http.MethodDelete, memberPath(ch, bob), bobTok, nil); status != http.StatusNoContent {
		t.Fatalf("bob leaving = %d", status)
	}
	if got, _ := channelMembers(t, e, ch, aliceTok); !slices.Equal(got, sortedIDs(alice, carol)) {
		t.Fatalf("GET members after bob left = %x", got)
	}

	// A 1:1 DM's pair is fixed.
	dm, _ := openDM(t, e, aliceTok, carol)
	if status, _ := e.Do(http.MethodPut, memberPath(dm, dave), aliceTok, nil); status != http.StatusForbidden {
		t.Fatalf("adding to a 1:1 DM = %d, want 403", status)
	}
	if status, _ := e.Do(http.MethodDelete, memberPath(dm, carol), aliceTok, nil); status != http.StatusForbidden {
		t.Fatalf("removing from a 1:1 DM = %d, want 403", status)
	}
}

func TestAGroupDMMemberAddRespectsTheCapAndBlockedAccounts(t *testing.T) {
	e, _ := memberEnv(t) // maxGroupDM = 10
	_, tok := e.NewUser("alice")
	others := make([]id.ID, 0, 9)
	for range 9 {
		u, _ := e.NewUser("u")
		others = append(others, u)
	}
	ch, status := openDM(t, e, tok, others...)
	if status != http.StatusCreated {
		t.Fatalf("10-member group DM = %d", status)
	}
	extra, _ := e.NewUser("extra")
	if status, _ := e.Do(http.MethodPut, memberPath(ch, extra), tok, nil); status != http.StatusBadRequest {
		t.Fatalf("an 11th member = %d, want 400", status)
	}
	// Re-adding a participant of a full group DM is not growth.
	if status, _ := e.Do(http.MethodPut, memberPath(ch, others[0]), tok, nil); status != http.StatusNoContent {
		t.Fatalf("re-adding a participant of a full group DM = %d, want 204", status)
	}

	small, _ := openDM(t, e, tok, others[0], others[1])
	if err := e.Repo.SetUserDisabled(t.Context(), extra, ptr(e.Clk.Now().Unix())); err != nil {
		t.Fatalf("SetUserDisabled: %v", err)
	}
	if status, _ := e.Do(http.MethodPut, memberPath(small, extra), tok, nil); status != http.StatusForbidden {
		t.Fatalf("adding a disabled account = %d, want 403", status)
	}
	if status, _ := e.Do(http.MethodPut, memberPath(small, id.New()), tok, nil); status != http.StatusNotFound {
		t.Fatalf("adding an unknown account = %d, want 404", status)
	}
}

func TestAddingAMemberToACommunityChannelIsRefused(t *testing.T) {
	e, cid, ownerTok := channelEnv(t)
	api.NewChannels(e.Repo, e.DS, e.Clk, 10, slog.New(slog.DiscardHandler)).RegisterMembers(e.Mux)
	text, _, status := newChannel(t, e, cid, ownerTok, 0, 0, 0, "general")
	if status != http.StatusCreated {
		t.Fatalf("create channel = %d", status)
	}
	member, memberTok := e.NewUser("member")
	joinCommunity(t, e, cid, memberTok)

	status, body := e.Do(http.MethodPut, memberPath(text, member), ownerTok, nil)
	if status != http.StatusForbidden || e.ErrCode(body) != "E_FORBIDDEN" {
		t.Fatalf("PUT member of a community channel = %d %x, want 403 E_FORBIDDEN", status, body)
	}
	status, body = e.Do(http.MethodDelete, memberPath(text, member), ownerTok, nil)
	if status != http.StatusForbidden || e.ErrCode(body) != "E_FORBIDDEN" {
		t.Fatalf("DELETE member of a community channel = %d %x, want 403 E_FORBIDDEN", status, body)
	}
	// Reading the derived membership is allowed to anyone who may view the channel.
	if _, status := channelMembers(t, e, text, memberTok); status != http.StatusOK {
		t.Fatalf("GET members of a visible community channel = %d, want 200", status)
	}
}

func TestANonParticipantCannotSeeAGroupDMsMembers(t *testing.T) {
	e, _ := memberEnv(t)
	_, aliceTok := e.NewUser("alice")
	bob, _ := e.NewUser("bob")
	carol, _ := e.NewUser("carol")
	_, eveTok := e.NewUser("eve")
	ch, _ := openDM(t, e, aliceTok, bob, carol)

	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/v1/channels/" + ch.String() + "/members"},
		{http.MethodGet, "/v1/channels/" + ch.String()},
		{http.MethodPut, memberPath(ch, bob)},
		{http.MethodDelete, memberPath(ch, bob)},
	} {
		status, body := e.Do(c.method, c.path, eveTok, nil)
		if status != http.StatusNotFound || e.ErrCode(body) != "E_NOT_FOUND" {
			t.Fatalf("%s %s by a non-participant = %d %x, want 404 E_NOT_FOUND", c.method, c.path, status, body)
		}
	}
	// A participant reads the channel itself.
	if status, body := e.Do(http.MethodGet, "/v1/channels/"+ch.String(), aliceTok, nil); status != http.StatusOK {
		t.Fatalf("GET /v1/channels/{id} by a participant = %d (%x)", status, body)
	}
}
