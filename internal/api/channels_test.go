package api_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/fxamacker/cbor/v2"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

func channelEnv(t *testing.T) (*env, id.ID, string) {
	t.Helper()
	e := newEnv(t)
	log := slog.New(slog.DiscardHandler)
	api.NewCommunities(e.Repo, e.DS, e.Clk, log).Register(e.Mux)
	api.NewChannels(e.Repo, e.DS, e.Clk, 10, log).Register(e.Mux)
	_, tok := e.NewUser("owner")
	return e, createCommunity(t, e, tok), tok
}

// newChannel posts one channel and returns its id and the decoded mode.
func newChannel(t *testing.T, e *env, cid id.ID, tok string, kind, mode, vis uint64, name string) (id.ID, uint64, int) {
	t.Helper()
	status, body := e.Do(http.MethodPost, "/v1/communities/"+cid.String()+"/channels", tok,
		[]any{kind, mode, vis, nil, name, "", uint64(0), uint64(0)})
	if status != http.StatusCreated {
		return id.ID{}, 0, status
	}
	var out []cbor.RawMessage
	if err := cborx.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var chID id.ID
	var gotMode uint64
	mustUnmarshal(t, out[0], &chID)
	mustUnmarshal(t, out[1], &gotMode)
	return chID, gotMode, status
}

// mustUnmarshalBody is mustUnmarshal over a whole response body.
func mustUnmarshalBody(t *testing.T, body []byte, v any) {
	t.Helper()
	mustUnmarshal(t, body, v)
}

func TestInviteAndDiscoverableChannelsAreForcedReadable(t *testing.T) {
	e, cid, tok := channelEnv(t)
	for _, vis := range []uint64{1, 2} { // invite, discoverable
		chID, mode, status := newChannel(t, e, cid, tok, 0, 0 /* asks for e2ee */, vis, "public")
		if status != http.StatusCreated {
			t.Fatalf("visibility %d: status %d", vis, status)
		}
		if mode != 1 {
			t.Fatalf("visibility %d: mode = %d, want 1 (readable)", vis, mode)
		}
		row, err := e.Repo.GetChannel(t.Context(), chID)
		if err != nil {
			t.Fatalf("GetChannel: %v", err)
		}
		if row.Mode != api.ModeReadable {
			t.Fatalf("stored mode = %d", row.Mode)
		}
	}
}

func TestChannelPermissionsReturnsEffectiveVisibleBits(t *testing.T) {
	e, cid, ownerTok := channelEnv(t)
	ch, _, _ := newChannel(t, e, cid, ownerTok, 0, 1, 0, "announcements")
	api.NewRoles(e.Repo, e.Clk, "dilla.example", slog.New(slog.DiscardHandler)).Register(e.Mux)
	roles, err := e.Repo.ListRoles(t.Context(), cid)
	if err != nil || len(roles) == 0 {
		t.Fatalf("ListRoles = %v, %v", roles, err)
	}
	if status, body := e.Do(http.MethodPut, "/v1/channels/"+ch.String()+"/overwrites/0/"+roles[0].ID.String(), ownerTok,
		[]any{uint64(0), uint64(api.PermSendMessages)}); status != http.StatusNoContent {
		t.Fatalf("PUT overwrite = %d (%x)", status, body)
	}
	_, memberTok := e.NewUser("member")
	joinCommunity(t, e, cid, memberTok)
	read := func(tok string) (int, uint64) {
		t.Helper()
		status, body := e.Do(http.MethodGet, "/v1/channels/"+ch.String()+"/permissions", tok, nil)
		var bits uint64
		if status == http.StatusOK {
			mustUnmarshalBody(t, body, &bits)
		}
		return status, bits
	}
	if status, bits := read(ownerTok); status != http.StatusOK || bits != uint64(api.PermAll) {
		t.Fatalf("owner permissions = %d, %x; want 200 and all bits", status, bits)
	}
	if status, bits := read(memberTok); status != http.StatusOK || api.Bits(bits).Has(api.PermSendMessages) || !api.Bits(bits).Has(api.PermViewChannel) {
		t.Fatalf("member permissions = %d, %x; want visible without send_messages", status, bits)
	}
	_, outsiderTok := e.NewUser("outsider")
	if status, _ := read(outsiderTok); status != http.StatusNotFound {
		t.Fatalf("foreign channel permissions = %d, want 404", status)
	}
	missing := id.New()
	if status, _ := e.Do(http.MethodGet, "/v1/channels/"+missing.String()+"/permissions", ownerTok, nil); status != http.StatusNotFound {
		t.Fatalf("absent channel permissions = %d, want 404", status)
	}
}

// I10 (fix wave): a member discovers a community's channels in band. GET
// /v1/communities/{id}/channels lists the live channels the caller may view, by position then
// channel id, each [channel_id, kind, mode, visibility, parent_id|null, name, topic, position,
// slowmode_seconds, seq, text_group_id|null]; a category is listed only when the caller can view
// it (R44, TestListingHidesAHiddenParent); a deleted channel never is; a non-member gets 404, as
// for an unknown community.
func TestAMemberListsTheCommunitysVisibleChannels(t *testing.T) {
	e, cid, ownerTok := channelEnv(t)
	cat, _, _ := newChannel(t, e, cid, ownerTok, uint64(api.ChannelCategory), 0, 0, "zeta")
	general, _, _ := newChannel(t, e, cid, ownerTok, 0, 1, 2, "general")
	hidden, _, _ := newChannel(t, e, cid, ownerTok, 0, 0, 0, "hidden")
	emptyCat, _, _ := newChannel(t, e, cid, ownerTok, uint64(api.ChannelCategory), 0, 0, "empty")
	gone, _, _ := newChannel(t, e, cid, ownerTok, 0, 0, 0, "gone")
	// Move "general" under the category, and order by position: general 1, hidden 2, cat 3.
	for _, p := range []struct {
		ch   id.ID
		body []any
	}{
		{general, []any{nil, nil, nil, nil, cat, uint64(1), nil}},
		{hidden, []any{nil, nil, nil, nil, nil, uint64(2), nil}},
		{cat, []any{nil, nil, nil, nil, nil, uint64(3), nil}},
		{emptyCat, []any{nil, nil, nil, nil, nil, uint64(4), nil}},
	} {
		if status, body := e.Do(http.MethodPatch, "/v1/channels/"+p.ch.String(), ownerTok, p.body); status != http.StatusNoContent {
			t.Fatalf("PATCH = %d (%x)", status, body)
		}
	}
	if status, _ := e.Do(http.MethodDelete, "/v1/channels/"+gone.String(), ownerTok, nil); status != http.StatusNoContent {
		t.Fatal("DELETE failed")
	}
	api.NewRoles(e.Repo, e.Clk, "dilla.example", slog.New(slog.DiscardHandler)).Register(e.Mux)
	roles, _ := e.Repo.ListRoles(t.Context(), cid)
	// @everyone may not view "hidden" or the empty category.
	for _, ch := range []id.ID{hidden, emptyCat} {
		if status, _ := e.Do(http.MethodPut, "/v1/channels/"+ch.String()+"/overwrites/0/"+roles[0].ID.String(), ownerTok,
			[]any{uint64(0), uint64(api.PermViewChannel)}); status != http.StatusNoContent {
			t.Fatal("PUT deny-view failed")
		}
	}
	_, memberTok := e.NewUser("member")
	joinCommunity(t, e, cid, memberTok)

	afk, _ := afkPointer(t, e, cid)
	list := func(tok string) []id.ID {
		t.Helper()
		status, body := e.Do(http.MethodGet, "/v1/communities/"+cid.String()+"/channels", tok, nil)
		if status != http.StatusOK {
			t.Fatalf("GET channels = %d (%x)", status, body)
		}
		var rows [][]cbor.RawMessage
		if err := cborx.Unmarshal(body, &rows); err != nil {
			t.Fatalf("decode: %v", err)
		}
		var out []id.ID
		for _, r := range rows {
			if len(r) != 11 {
				t.Fatalf("an element has %d fields, want 11", len(r))
			}
			var chID id.ID
			mustUnmarshal(t, r[0], &chID)
			if chID == afk {
				continue // every community's built-in AFK channel (web3.9 R1)
			}
			out = append(out, chID)
			if chID == general {
				var kind, mode, vis, pos, slow, seq uint64
				var parent *id.ID
				var name, topic string
				mustUnmarshal(t, r[1], &kind)
				mustUnmarshal(t, r[2], &mode)
				mustUnmarshal(t, r[3], &vis)
				mustUnmarshal(t, r[4], &parent)
				mustUnmarshal(t, r[5], &name)
				mustUnmarshal(t, r[6], &topic)
				mustUnmarshal(t, r[7], &pos)
				mustUnmarshal(t, r[8], &slow)
				mustUnmarshal(t, r[9], &seq)
				if kind != 0 || mode != 1 || vis != 2 || parent == nil || *parent != cat || name != "general" || pos != 1 {
					t.Fatalf("general = %d %d %d %v %q %d", kind, mode, vis, parent, name, pos)
				}
			}
		}
		return out
	}
	if got := list(memberTok); len(got) != 2 || got[0] != general || got[1] != cat {
		t.Fatalf("the member's list = %v, want [general %s, category %s]", got, general, cat)
	}
	if got := list(ownerTok); len(got) != 4 || got[0] != general || got[1] != hidden || got[2] != cat || got[3] != emptyCat {
		t.Fatalf("the owner's list = %v, want general, hidden, category, empty category", got)
	}
	_, outsiderTok := e.NewUser("outsider")
	if status, _ := e.Do(http.MethodGet, "/v1/communities/"+cid.String()+"/channels", outsiderTok, nil); status != http.StatusNotFound {
		t.Fatalf("a non-member's GET = %d, want 404", status)
	}
}

// I3 (fix wave): a channel that stops being end-to-end encrypted is never allowed an MLS text
// group (spec, trust boundaries; protocol/01). A PATCH to mode=readable, or to a visibility that
// forces it, closes the channel's open text group after the commit, and the ACL admits nobody to
// a text group whose channel carries none, so no Add or join lands in it. A voice channel's call
// group is not a text group and survives a visibility change.
func TestLeavingEndToEndEncryptionClosesTheTextGroup(t *testing.T) {
	for _, c := range []struct {
		name  string
		patch []any
	}{
		{"mode=readable", []any{nil, nil, uint64(1), nil, nil, nil, nil}},
		{"visibility=discoverable", []any{nil, nil, nil, uint64(2), nil, nil, nil}},
	} {
		t.Run(c.name, func(t *testing.T) {
			e, cid, tok := channelEnv(t)
			ch, _, _ := newChannel(t, e, cid, tok, 0, 0, 0, "secret")
			group := seedTextGroup(t, e, ch, cid)
			owner := e.sess[tok].UserID
			seedMember(t, e, group, owner, 0)
			if ok, err := (api.ResolverACL{Repo: e.Repo}).Eligible(t.Context(), group, owner); err != nil || !ok {
				t.Fatalf("before the PATCH the owner is eligible = %v, %v; want true", ok, err)
			}
			if status, body := e.Do(http.MethodPatch, "/v1/channels/"+ch.String(), tok, c.patch); status != http.StatusNoContent {
				t.Fatalf("PATCH %s = %d (%x)", c.name, status, body)
			}
			if closed := e.DS.closed(); len(closed) != 1 || closed[0] != group {
				t.Fatalf("closed groups = %v, want the text group %s", closed, group)
			}
			if ok, err := (api.ResolverACL{Repo: e.Repo}).Eligible(t.Context(), group, owner); err != nil || ok {
				t.Fatalf("after the PATCH the owner is eligible for the text group = %v, %v; want false", ok, err)
			}
		})
	}

	e, cid, tok := channelEnv(t)
	voice, _, _ := newChannel(t, e, cid, tok, 1, 0, 0, "call")
	seedGroupOfKind(t, e, voice, cid, 1)
	if status, _ := e.Do(http.MethodPatch, "/v1/channels/"+voice.String(), tok,
		[]any{nil, nil, nil, uint64(2), nil, nil, nil}); status != http.StatusNoContent {
		t.Fatal("PATCH of the voice channel's visibility failed")
	}
	if closed := e.DS.closed(); len(closed) != 0 {
		t.Fatalf("a voice channel's visibility change closed %v, want its call group left open", closed)
	}
}

func TestPatchToE2EEOnAVisibleChannelIsRefused(t *testing.T) {
	e, cid, tok := channelEnv(t)
	chID, _, _ := newChannel(t, e, cid, tok, 0, 1, 2, "public")
	status, body := e.Do(http.MethodPatch, "/v1/channels/"+chID.String(), tok,
		[]any{nil, nil, uint64(0) /* e2ee */, nil, nil, nil, nil})
	if status != http.StatusBadRequest {
		t.Fatalf("PATCH mode=e2ee = %d, want 400", status)
	}
	if code := e.ErrCode(body); code != "E_INVALID_REQUEST" {
		t.Fatalf("code = %s", code)
	}

	// The same PATCH is accepted once the channel is private again, and both
	// changes may not be made in one request either, because the order would
	// decide the outcome.
	if status, _ := e.Do(http.MethodPatch, "/v1/channels/"+chID.String(), tok,
		[]any{nil, nil, uint64(0), uint64(0), nil, nil, nil}); status != http.StatusBadRequest {
		t.Fatalf("combined PATCH = %d, want 400", status)
	}
	if status, _ := e.Do(http.MethodPatch, "/v1/channels/"+chID.String(), tok,
		[]any{nil, nil, nil, uint64(0), nil, nil, nil}); status != http.StatusNoContent {
		t.Fatalf("PATCH visibility=private failed")
	}
	if status, _ := e.Do(http.MethodPatch, "/v1/channels/"+chID.String(), tok,
		[]any{nil, nil, uint64(0), nil, nil, nil, nil}); status != http.StatusNoContent {
		t.Fatalf("PATCH mode=e2ee after going private failed")
	}
}

func TestACategoryMayNotHaveAParent(t *testing.T) {
	e, cid, tok := channelEnv(t)
	parent, _, _ := newChannel(t, e, cid, tok, 2, 1, 0, "cat")
	status, body := e.Do(http.MethodPost, "/v1/communities/"+cid.String()+"/channels", tok,
		[]any{uint64(2), uint64(1), uint64(0), parent, "nested", "", uint64(0), uint64(0)})
	if status != http.StatusBadRequest {
		t.Fatalf("nested category = %d, want 400", status)
	}
	if code := e.ErrCode(body); code != "E_INVALID_REQUEST" {
		t.Fatalf("code = %s", code)
	}

	// A text channel under that category is fine; a text channel under a text
	// channel is not.
	text, _, _ := newChannel(t, e, cid, tok, 0, 1, 0, "general")
	if status, _ := e.Do(http.MethodPatch, "/v1/channels/"+text.String(), tok,
		[]any{nil, nil, nil, nil, parent, nil, nil}); status != http.StatusNoContent {
		t.Fatalf("reparent under a category failed")
	}
	if status, _ := e.Do(http.MethodPost, "/v1/communities/"+cid.String()+"/channels", tok,
		[]any{uint64(0), uint64(1), uint64(0), text, "bad", "", uint64(0), uint64(0)}); status != http.StatusBadRequest {
		t.Fatalf("text under text = %d, want 400", status)
	}
}

func TestAPositionChangeMovesTheChannel(t *testing.T) {
	e, cid, tok := channelEnv(t)
	// Three channels at distinct positions, so the assertion is about position
	// and not about the id tie-break.
	var ids []id.ID
	for i, n := range []string{"c", "a", "b"} {
		status, body := e.Do(http.MethodPost, "/v1/communities/"+cid.String()+"/channels", tok,
			[]any{uint64(0), uint64(1), uint64(0), nil, n, "", uint64(10 * (i + 1)), uint64(0)})
		if status != http.StatusCreated {
			t.Fatalf("POST channel %s = %d (%x)", n, status, body)
		}
		var out []cbor.RawMessage
		mustUnmarshalBody(t, body, &out)
		var chID id.ID
		mustUnmarshal(t, out[0], &chID)
		ids = append(ids, chID)
	}
	rows, err := e.Repo.ListChannels(t.Context(), cid)
	if err != nil {
		t.Fatalf("ListChannels: %v", err)
	}
	rows = withoutBuiltinAFK(rows)
	if len(rows) != 3 || rows[0].ID != ids[0] || rows[1].ID != ids[1] || rows[2].ID != ids[2] {
		t.Fatalf("initial order = %+v, want positions 10, 20, 30", rows)
	}
	// Move the last one in front of the first: a real reorder, not a no-op.
	if status, _ := e.Do(http.MethodPatch, "/v1/channels/"+ids[2].String(), tok,
		[]any{nil, nil, nil, nil, nil, uint64(5), nil}); status != http.StatusNoContent {
		t.Fatal("PATCH position failed")
	}
	again, err := e.Repo.ListChannels(t.Context(), cid)
	if err != nil {
		t.Fatalf("ListChannels: %v", err)
	}
	if again[0].ID != ids[2] {
		t.Fatalf("the reordered channel did not move to the front: %+v", again)
	}
}

func TestEqualPositionsBreakOnID(t *testing.T) {
	e, cid, tok := channelEnv(t)
	// Both created at position 0; ties break on id, so the listing is
	// deterministic across runs and across engines.
	a, _, _ := newChannel(t, e, cid, tok, 0, 1, 0, "a")
	b, _, _ := newChannel(t, e, cid, tok, 0, 1, 0, "b")
	_, _ = a, b
	rows, err := e.Repo.ListChannels(t.Context(), cid)
	if err != nil {
		t.Fatalf("ListChannels: %v", err)
	}
	rows = withoutBuiltinAFK(rows)
	if len(rows) != 2 {
		t.Fatalf("channels = %d", len(rows))
	}
	if rows[0].Position != rows[1].Position {
		t.Fatalf("both channels should be at position 0: %+v", rows)
	}
	if string(rows[0].ID[:]) >= string(rows[1].ID[:]) {
		t.Fatalf("not ordered by id within a position: %x then %x", rows[0].ID, rows[1].ID)
	}
	for range 4 {
		again, _ := e.Repo.ListChannels(t.Context(), cid)
		if again[0].ID != rows[0].ID || again[1].ID != rows[1].ID {
			t.Fatal("ListChannels is not deterministic for equal positions")
		}
	}
}

func TestAVoiceChannelIsCreatedInAReadableCommunity(t *testing.T) {
	e, cid, tok := channelEnv(t)
	// Every text channel readable; the voice channel is still created and its
	// calls get a call group (kind 1), which protocol/01 requires regardless of
	// the community's text mode.
	chID, mode, status := newChannel(t, e, cid, tok, 1 /* voice */, 1, 2, "Allmänt")
	if status != http.StatusCreated {
		t.Fatalf("voice channel = %d", status)
	}
	if mode != 1 {
		t.Fatalf("voice channel mode = %d", mode)
	}
	row, err := e.Repo.GetChannel(t.Context(), chID)
	if err != nil {
		t.Fatalf("GetChannel: %v", err)
	}
	if row.Kind != api.ChannelVoice {
		t.Fatalf("kind = %d", row.Kind)
	}
	// The channel's mode never reaches a call group: task 16 mints the call
	// group with kind 1 and the DS refuses only kind 0 on a readable channel.
	if api.TextGroupAllowed(row) {
		t.Fatal("a readable channel must not allow an MLS text group")
	}
	if !api.CallGroupAllowed(row) {
		t.Fatal("a voice channel must always allow a call group")
	}
}

func TestDeleteCommunityTombstonesChannels(t *testing.T) {
	e, cid, tok := channelEnv(t)
	chID, _, _ := newChannel(t, e, cid, tok, 0, 1, 0, "general")
	if status, _ := e.Do(http.MethodDelete, "/v1/communities/"+cid.String(), tok, nil); status != http.StatusNoContent {
		t.Fatal("DELETE community failed")
	}
	if _, err := e.Repo.GetChannel(t.Context(), chID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetChannel after community delete = %v, want ErrNotFound", err)
	}
}

// ---------------------------------------------------------------- beyond the brief

// A text channel that is private and end-to-end encrypted is the one channel an
// MLS text group may be registered for; everything else about the two predicates
// follows from protocol/01's group-kind table.
func TestTheGroupPredicatesFollowTheKindTable(t *testing.T) {
	deleted := int64(1)
	for _, c := range []struct {
		name       string
		row        store.ChannelRow
		text, call bool
	}{
		{"private e2ee text", store.ChannelRow{Kind: api.ChannelText, Mode: api.ModeE2EE}, true, false},
		{"readable text", store.ChannelRow{Kind: api.ChannelText, Mode: api.ModeReadable}, false, false},
		{"e2ee voice", store.ChannelRow{Kind: api.ChannelVoice, Mode: api.ModeE2EE}, false, true},
		{"category", store.ChannelRow{Kind: api.ChannelCategory, Mode: api.ModeE2EE}, false, false},
		{"dm", store.ChannelRow{Kind: api.ChannelDM, Mode: api.ModeE2EE}, true, true},
		{"group dm", store.ChannelRow{Kind: api.ChannelGroupDM, Mode: api.ModeE2EE}, true, true},
		{"deleted text", store.ChannelRow{Kind: api.ChannelText, DeletedAt: &deleted}, false, false},
		{"deleted voice", store.ChannelRow{Kind: api.ChannelVoice, DeletedAt: &deleted}, false, false},
	} {
		if got := api.TextGroupAllowed(c.row); got != c.text {
			t.Errorf("%s: TextGroupAllowed = %v, want %v", c.name, got, c.text)
		}
		if got := api.CallGroupAllowed(c.row); got != c.call {
			t.Errorf("%s: CallGroupAllowed = %v, want %v", c.name, got, c.call)
		}
	}
}

// GET /v1/channels/{id} is the twelve-element channel document; a non-member
// is told the channel does not exist, and only the owner may change it.
func TestChannelReadAndManageGates(t *testing.T) {
	e, cid, tok := channelEnv(t)
	chID, _, _ := newChannel(t, e, cid, tok, 0, 0, 0, "general")

	status, body := e.Do(http.MethodGet, "/v1/channels/"+chID.String(), tok, nil)
	if status != http.StatusOK {
		t.Fatalf("GET channel = %d (%x)", status, body)
	}
	var doc []cbor.RawMessage
	mustUnmarshalBody(t, body, &doc)
	if len(doc) != 12 {
		t.Fatalf("channel document has %d elements, want 12", len(doc))
	}
	var gotID, gotCommunity id.ID
	var name string
	mustUnmarshal(t, doc[0], &gotID)
	mustUnmarshal(t, doc[1], &gotCommunity)
	mustUnmarshal(t, doc[6], &name)
	if gotID != chID || gotCommunity != cid || name != "general" {
		t.Fatalf("channel document = %v/%v/%q", gotID, gotCommunity, name)
	}

	// A stranger: 404 on every route, never 403, so the id reveals nothing.
	_, stranger := e.NewUser("stranger")
	for _, req := range []struct {
		method string
		body   any
	}{
		{http.MethodGet, nil},
		{http.MethodPatch, []any{"x", nil, nil, nil, nil, nil, nil}},
		{http.MethodDelete, nil},
	} {
		status, body := e.Do(req.method, "/v1/channels/"+chID.String(), stranger, req.body)
		if status != http.StatusNotFound || e.ErrCode(body) != "E_NOT_FOUND" {
			t.Fatalf("stranger %s = %d, want 404 E_NOT_FOUND", req.method, status)
		}
	}
	if status, _ := e.Do(http.MethodPost, "/v1/communities/"+cid.String()+"/channels", stranger,
		[]any{uint64(0), uint64(0), uint64(0), nil, "x", "", uint64(0), uint64(0)}); status != http.StatusNotFound {
		t.Fatalf("stranger POST channel = %d, want 404", status)
	}

	// A member who is not the owner reads, and may not manage.
	memberID, member := e.NewUser("member")
	if status, _ := e.Do(http.MethodPost, "/v1/communities/"+cid.String()+"/join", member, []any{nil}); status != http.StatusOK {
		t.Fatalf("join = %d", status)
	}
	if _, err := e.Repo.GetMember(t.Context(), cid, memberID); err != nil {
		t.Fatalf("the member did not join: %v", err)
	}
	if status, _ := e.Do(http.MethodGet, "/v1/channels/"+chID.String(), member, nil); status != http.StatusOK {
		t.Fatalf("member GET = %d, want 200", status)
	}
	for _, req := range []struct {
		method, path string
		body         any
	}{
		{http.MethodPatch, "/v1/channels/" + chID.String(), []any{"x", nil, nil, nil, nil, nil, nil}},
		{http.MethodDelete, "/v1/channels/" + chID.String(), nil},
		{http.MethodPost, "/v1/communities/" + cid.String() + "/channels",
			[]any{uint64(0), uint64(0), uint64(0), nil, "x", "", uint64(0), uint64(0)}},
	} {
		status, body := e.Do(req.method, req.path, member, req.body)
		if status != http.StatusForbidden || e.ErrCode(body) != "E_FORBIDDEN" {
			t.Fatalf("member %s %s = %d, want 403 E_FORBIDDEN", req.method, req.path, status)
		}
	}

	// No session at all.
	if status, _ := e.Do(http.MethodGet, "/v1/channels/"+chID.String(), "", nil); status != http.StatusUnauthorized {
		t.Fatalf("anonymous GET = %d, want 401", status)
	}
}

// Every bounded field is refused past its bound, with E_INVALID_REQUEST.
func TestChannelFieldBounds(t *testing.T) {
	e, cid, tok := channelEnv(t)
	post := func(kind, mode, vis uint64, name, topic string, position, slowmode uint64) int {
		status, _ := e.Do(http.MethodPost, "/v1/communities/"+cid.String()+"/channels", tok,
			[]any{kind, mode, vis, nil, name, topic, position, slowmode})
		return status
	}
	for _, c := range []struct {
		name   string
		status int
	}{
		{name: "empty name", status: post(0, 0, 0, "", "", 0, 0)},
		{name: "long name", status: post(0, 0, 0, strings.Repeat("n", 101), "", 0, 0)},
		{name: "control character in name", status: post(0, 0, 0, "a\x00b", "", 0, 0)},
		{name: "non-UTF-8 name", status: post(0, 0, 0, "a\xffb", "", 0, 0)},
		{name: "long topic", status: post(0, 0, 0, "ok", strings.Repeat("t", 1025), 0, 0)},
		{name: "NUL in topic", status: post(0, 0, 0, "ok", "a\x00b", 0, 0)},
		{name: "slowmode past six hours", status: post(0, 0, 0, "ok", "", 0, 21601)},
		{name: "position past the bound", status: post(0, 0, 0, "ok", "", 1<<31, 0)},
		{name: "unknown kind", status: post(5, 0, 0, "ok", "", 0, 0)},
		{name: "a DM through the community route", status: post(3, 0, 0, "ok", "", 0, 0)},
		{name: "a group DM through the community route", status: post(4, 0, 0, "ok", "", 0, 0)},
		{name: "unknown mode", status: post(0, 2, 0, "ok", "", 0, 0)},
		{name: "unknown visibility", status: post(0, 0, 3, "ok", "", 0, 0)},
	} {
		if c.status != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", c.name, c.status)
		}
	}
	// The bounds themselves are legal, and a topic may carry line breaks. A create's position bound is
	// 2^30-1: 2^30 and above is the built-in AFK channel's (R-AFK8, TestAFKReservedPositionRefusedOnCreate).
	if status := post(0, 0, 0, strings.Repeat("n", 100), "line one\nline two", api.AFKBuiltinPosition-1, 21600); status != http.StatusCreated {
		t.Fatalf("a channel at every bound = %d, want 201", status)
	}
}

// Deleting a category leaves its channels in place at the top level, and a
// channel moves to the top level by naming the all-zero parent.
func TestCategoryDeleteAndTopLevelMove(t *testing.T) {
	e, cid, tok := channelEnv(t)
	cat, _, _ := newChannel(t, e, cid, tok, 2, 0, 0, "cat")
	var child id.ID
	{
		status, body := e.Do(http.MethodPost, "/v1/communities/"+cid.String()+"/channels", tok,
			[]any{uint64(0), uint64(0), uint64(0), cat, "child", "", uint64(0), uint64(0)})
		if status != http.StatusCreated {
			t.Fatalf("POST child = %d", status)
		}
		var out []cbor.RawMessage
		mustUnmarshalBody(t, body, &out)
		mustUnmarshal(t, out[0], &child)
	}
	other, _, _ := newChannel(t, e, cid, tok, 0, 0, 0, "other")
	if status, _ := e.Do(http.MethodPatch, "/v1/channels/"+other.String(), tok,
		[]any{nil, nil, nil, nil, cat, nil, nil}); status != http.StatusNoContent {
		t.Fatal("reparent under the category failed")
	}
	// Move `other` back to the top level with the all-zero id.
	if status, _ := e.Do(http.MethodPatch, "/v1/channels/"+other.String(), tok,
		[]any{nil, nil, nil, nil, id.ID{}, nil, nil}); status != http.StatusNoContent {
		t.Fatal("move to the top level failed")
	}
	if row, err := e.Repo.GetChannel(t.Context(), other); err != nil || row.ParentID != nil {
		t.Fatalf("after the top-level move: parent = %v, err %v", row.ParentID, err)
	}

	if status, _ := e.Do(http.MethodDelete, "/v1/channels/"+cat.String(), tok, nil); status != http.StatusNoContent {
		t.Fatal("DELETE category failed")
	}
	row, err := e.Repo.GetChannel(t.Context(), child)
	if err != nil {
		t.Fatalf("the category's child went with it: %v", err)
	}
	if row.ParentID != nil {
		t.Fatalf("the child still names the deleted category %v", *row.ParentID)
	}
	// And the child stays editable: its parent is not a dangling tombstone.
	if status, _ := e.Do(http.MethodPatch, "/v1/channels/"+child.String(), tok,
		[]any{"renamed", nil, nil, nil, nil, nil, nil}); status != http.StatusNoContent {
		t.Fatal("PATCH the former child failed")
	}
	if status, _ := e.Do(http.MethodGet, "/v1/channels/"+cat.String(), tok, nil); status != http.StatusNotFound {
		t.Fatalf("GET the deleted category = %d, want 404", status)
	}
	if status, _ := e.Do(http.MethodDelete, "/v1/channels/"+cat.String(), tok, nil); status != http.StatusNotFound {
		t.Fatalf("DELETE the deleted category again = %d, want 404", status)
	}
}

// A parent in another community is refused like any other bad parent.
func TestAParentFromAnotherCommunityIsRefused(t *testing.T) {
	e, cid, tok := channelEnv(t)
	otherCommunity := createCommunity(t, e, tok)
	foreign, _, _ := newChannel(t, e, otherCommunity, tok, 2, 0, 0, "foreign")
	status, body := e.Do(http.MethodPost, "/v1/communities/"+cid.String()+"/channels", tok,
		[]any{uint64(0), uint64(0), uint64(0), foreign, "x", "", uint64(0), uint64(0)})
	if status != http.StatusBadRequest || e.ErrCode(body) != "E_INVALID_REQUEST" {
		t.Fatalf("foreign parent = %d, want 400", status)
	}
	if status, _ := e.Do(http.MethodPost, "/v1/communities/"+cid.String()+"/channels", tok,
		[]any{uint64(0), uint64(0), uint64(0), id.New(), "x", "", uint64(0), uint64(0)}); status != http.StatusBadRequest {
		t.Fatalf("unknown parent = %d, want 400", status)
	}
}

// errDetail decodes an E_* error body [code, detail, …] and returns its detail.
func errDetail(t *testing.T, body []byte) string {
	t.Helper()
	var arr []cbor.RawMessage
	if err := cborx.Unmarshal(body, &arr); err != nil || len(arr) < 2 {
		t.Fatalf("error body %x: %v", body, err)
	}
	var detail string
	mustUnmarshal(t, arr[1], &detail)
	return detail
}

// ow is one entry of a channel create's ninth element: [kind, target_id, allow, deny].
func ow(kind uint64, target id.ID, allow, deny api.Bits) []any {
	return []any{kind, target, uint64(allow), uint64(deny)}
}

// createWith posts a channel of kind named name carrying the ninth element overwrites.
func createWith(e *env, cid id.ID, tok, name string, kind uint64, overwrites any) (int, []byte) {
	return e.Do(http.MethodPost, "/v1/communities/"+cid.String()+"/channels", tok,
		[]any{kind, uint64(0), uint64(0), nil, name, "", uint64(0), uint64(0), overwrites})
}

// createdID decodes a 201's channel id.
func createdID(t *testing.T, body []byte) id.ID {
	t.Helper()
	var out []cbor.RawMessage
	mustUnmarshal(t, body, &out)
	var ch id.ID
	mustUnmarshal(t, out[0], &ch)
	return ch
}

// lists reports whether ch is among the channels GET /v1/communities/{id}/channels answers tok.
func lists(t *testing.T, e *env, cid id.ID, tok string, ch id.ID) bool {
	t.Helper()
	status, body := e.Do(http.MethodGet, "/v1/communities/"+cid.String()+"/channels", tok, nil)
	if status != http.StatusOK {
		t.Fatalf("GET channels = %d (%x)", status, body)
	}
	var rows [][]cbor.RawMessage
	mustUnmarshal(t, body, &rows)
	for _, row := range rows {
		var got id.ID
		mustUnmarshal(t, row[0], &got)
		if got == ch {
			return true
		}
	}
	return false
}

// privateFixture is notifyEnv plus a member "b" and a moderator "mod" holding a role at position 5
// with view, send, manage_messages, manage_channels and manage_roles, and @everyone's id.
type privateFixture struct {
	e                    *env
	gw                   *changedGW
	cid, owner, b, mod   id.ID
	ownerTok, bTok, mTok string
	everyone, modRole    id.ID
}

func privateEnv(t *testing.T) privateFixture {
	t.Helper()
	e, gw, cid, owner, ownerTok := notifyEnv(t)
	f := privateFixture{e: e, gw: gw, cid: cid, owner: owner, ownerTok: ownerTok}
	f.b, f.bTok = joinAs(t, e, cid, "b")
	f.mod, f.mTok = joinAs(t, e, cid, "mod")
	roles, err := e.Repo.ListRoles(t.Context(), cid)
	if err != nil || len(roles) != 1 {
		t.Fatalf("ListRoles = %v, %v", roles, err)
	}
	f.everyone = roles[0].ID
	f.modRole = createRole(t, e, cid, ownerTok, "mod", 5,
		api.PermViewChannel|api.PermSendMessages|api.PermManageMessages|api.PermManageChannels|api.PermManageRoles)
	grant(t, e, cid, ownerTok, f.mod, f.modRole, http.StatusNoContent)
	gw.reset()
	return f
}

// S2: the owner creates a channel private from birth. B never lists it, its members are the
// ones it allows, every entry is audited beside channel.create, and the members are told once.
func TestAChannelIsPrivateFromBirth(t *testing.T) {
	f := privateEnv(t)
	status, body := createWith(f.e, f.cid, f.ownerTok, "styrelse", 0,
		[]any{ow(0, f.everyone, 0, api.PermViewChannel), ow(1, f.mod, api.PermViewChannel, 0)})
	if status != http.StatusCreated {
		t.Fatalf("private create = %d (%x)", status, body)
	}
	ch := createdID(t, body)
	if lists(t, f.e, f.cid, f.bTok, ch) {
		t.Fatal("B lists a channel @everyone may not view")
	}
	if !lists(t, f.e, f.cid, f.mTok, ch) || !lists(t, f.e, f.cid, f.ownerTok, ch) {
		t.Fatal("the owner or the allowed mod does not list the channel")
	}
	members, err := listChannelMembers(t, f.e, ch)
	if err != nil {
		t.Fatalf("ListChannelMembers: %v", err)
	}
	want := []id.ID{f.owner, f.mod}
	slices.SortFunc(want, func(a, b id.ID) int { return bytes.Compare(a[:], b[:]) })
	if !slices.Equal(members, want) {
		t.Fatalf("channel_members = %v, want %v", members, want)
	}
	rows, err := f.e.Repo.ListAudit(t.Context(), 0, 100)
	if err != nil {
		t.Fatalf("ListAudit: %v", err)
	}
	var actions []string
	for _, row := range rows {
		if row.Target == ch.String() {
			actions = append(actions, row.Action)
		}
	}
	slices.Sort(actions)
	if !slices.Equal(actions, []string{"channel.create", "channel.overwrite.put", "channel.overwrite.put"}) {
		t.Fatalf("audit actions = %v", actions)
	}
	told(t, f.gw, "private create", map[id.ID]int{f.owner: 1, f.b: 1, f.mod: 1})

	// With only @everyone denied, the owner is the one member.
	status, body = createWith(f.e, f.cid, f.ownerTok, "ensam", 0, []any{ow(0, f.everyone, 0, api.PermViewChannel)})
	if status != http.StatusCreated {
		t.Fatalf("owner-only create = %d (%x)", status, body)
	}
	if members, _ := listChannelMembers(t, f.e, createdID(t, body)); !slices.Equal(members, []id.ID{f.owner}) {
		t.Fatalf("channel_members = %v, want the owner only", members)
	}
	told(t, f.gw, "second create", map[id.ID]int{f.owner: 2, f.b: 2, f.mod: 2})
}

// S2: a moderator with manage_channels and manage_roles creates a private channel only with an
// entry that keeps it visible to them, and every entry follows the overwrite PUT rules against
// the moderator's community-wide bits.
func TestAPrivateCreateFollowsTheOverwriteRules(t *testing.T) {
	f := privateEnv(t)
	deny := ow(0, f.everyone, 0, api.PermViewChannel)
	self := ow(1, f.mod, api.PermViewChannel, 0)

	status, body := createWith(f.e, f.cid, f.mTok, "mods", 0, []any{deny, self})
	if status != http.StatusCreated {
		t.Fatalf("mod create with a self entry = %d (%x)", status, body)
	}
	if !lists(t, f.e, f.cid, f.mTok, createdID(t, body)) {
		t.Fatal("the mod does not list the channel it made")
	}
	told(t, f.gw, "mod create", map[id.ID]int{f.owner: 1, f.b: 1, f.mod: 1})

	cases := []struct {
		name       string
		kind       uint64
		overwrites any
		status     int
		detail     string
	}{
		{"no self entry", 0, []any{deny}, http.StatusBadRequest, "the channel would be hidden from you"},
		{"the mod's own role", 0, []any{deny, self, ow(0, f.modRole, api.PermViewChannel, 0)}, http.StatusForbidden, ""},
		{"a self deny", 0, []any{ow(1, f.mod, 0, api.PermSendMessages)}, http.StatusForbidden, ""},
		// I-1: the mod holds manage_messages and send, so only the narrowed exemption refuses these.
		{"a self entry beyond view", 0, []any{deny, ow(1, f.mod, api.PermViewChannel|api.PermManageMessages, 0)}, http.StatusForbidden, ""},
		// web3.8 R5 admits a self send entry (TestCreatorMayAllowThemselfSend); a held bit beyond view
		// and send still takes the rank rule.
		{"a self entry beyond view and send", 0, []any{ow(1, f.mod, api.PermSendMessages|api.PermManageMessages, 0)}, http.StatusForbidden, ""},
		{"kick_members", 0, []any{ow(0, f.everyone, api.PermKickMembers, 0)}, http.StatusBadRequest, ""},
		{"a bit the mod lacks", 0, []any{ow(0, f.everyone, api.PermPinMessages, 0)}, http.StatusForbidden, ""},
		{"a self allow of a bit the mod lacks", 0, []any{ow(1, f.mod, api.PermPinMessages, 0)}, http.StatusForbidden, ""},
		{"an undefined bit", 0, []any{ow(0, f.everyone, 1<<40, 0)}, http.StatusBadRequest, ""},
		{"allow and deny", 0, []any{ow(0, f.everyone, api.PermSendMessages, api.PermSendMessages)}, http.StatusBadRequest, ""},
		{"kind 2", 0, []any{ow(2, f.everyone, 0, api.PermSendMessages)}, http.StatusBadRequest, ""},
		{"an unknown role", 0, []any{ow(0, id.New(), 0, api.PermSendMessages)}, http.StatusNotFound, ""},
		{"a non-member", 0, []any{ow(1, id.New(), 0, api.PermSendMessages)}, http.StatusNotFound, ""},
		{"a category", uint64(api.ChannelCategory), []any{deny, self}, http.StatusBadRequest, "a category's overwrites have no effect"},
		{"a duplicate", 0, []any{deny, self, ow(0, f.everyone, 0, api.PermSendMessages)}, http.StatusBadRequest, ""},
		{"no entries", 0, []any{}, http.StatusBadRequest, "overwrites must hold 1 to 100 entries"},
		{"101 entries", 0, slices.Repeat([]any{deny}, 101), http.StatusBadRequest, "overwrites must hold 1 to 100 entries"},
	}
	for _, c := range cases {
		status, body := createWith(f.e, f.cid, f.mTok, "refused", c.kind, c.overwrites)
		if status != c.status {
			t.Fatalf("%s = %d, want %d (%x)", c.name, status, c.status, body)
		}
		if c.detail != "" && errDetail(t, body) != c.detail {
			t.Fatalf("%s detail = %q, want %q", c.name, errDetail(t, body), c.detail)
		}
	}
	// No refused create wrote a channel or told anyone.
	told(t, f.gw, "refused creates", map[id.ID]int{f.owner: 1, f.b: 1, f.mod: 1})

	// Without manage_roles, overwrites are refused even to a channel manager.
	chanRole := createRole(t, f.e, f.cid, f.ownerTok, "channels", 4, api.PermViewChannel|api.PermManageChannels)
	grant(t, f.e, f.cid, f.ownerTok, f.b, chanRole, http.StatusNoContent)
	if status, body := createWith(f.e, f.cid, f.bTok, "nope", 0, []any{ow(1, f.b, api.PermViewChannel, 0)}); status != http.StatusForbidden {
		t.Fatalf("create with overwrites without manage_roles = %d, want 403 (%x)", status, body)
	}
	if _, _, status := newChannel(t, f.e, f.cid, f.bTok, 0, 0, 0, "plain"); status != http.StatusCreated {
		t.Fatalf("the same manager's plain create = %d, want 201", status)
	}
}

// rankedCreator adds a member holding one new role at position 5, the moderator's rank, with
// bits, and returns the member and their token. The role is their only role, so it is their top.
func rankedCreator(t *testing.T, f privateFixture, name string, bits api.Bits) (id.ID, string) {
	t.Helper()
	uid, tok := joinAs(t, f.e, f.cid, name)
	role := createRole(t, f.e, f.cid, f.ownerTok, name, 5, bits)
	grant(t, f.e, f.cid, f.ownerTok, uid, role, http.StatusNoContent)
	return uid, tok
}

// hasOverwrite reports whether channel ch stores exactly the user entry target allow/deny.
func hasOverwrite(t *testing.T, e *env, ch, target id.ID, allow, deny api.Bits) bool {
	t.Helper()
	for _, o := range mustOverwrites(t, e, ch) {
		if o.TargetKind == 1 && o.TargetID == target {
			return o.Allow == uint64(allow) && o.Deny == uint64(deny)
		}
	}
	return false
}

// web3.8 S2 (R5): a moderator whose only role is their top role, holding manage channels, manage
// roles and send, makes an announcement channel and gives themself send in the same create. The
// control differs in one bit (pin_messages, which they lack) and is refused.
func TestCreatorMayAllowThemselfSend(t *testing.T) {
	f := privateEnv(t)
	everyoneNoSend := ow(0, f.everyone, 0, api.PermSendMessages)
	status, body := createWith(f.e, f.cid, f.mTok, "news", 0,
		[]any{everyoneNoSend, ow(1, f.mod, api.PermSendMessages, 0)})
	if status != http.StatusCreated {
		t.Fatalf("announcement create with a self send entry = %d (%x), want 201", status, body)
	}
	ch := createdID(t, body)
	if !hasOverwrite(t, f.e, ch, f.mod, api.PermSendMessages, 0) {
		t.Fatalf("the self send entry was not stored: %+v", mustOverwrites(t, f.e, ch))
	}
	// view and send together, on a private announcement channel.
	status, body = createWith(f.e, f.cid, f.mTok, "private news", 0,
		[]any{ow(0, f.everyone, 0, api.PermViewChannel|api.PermSendMessages),
			ow(1, f.mod, api.PermViewChannel|api.PermSendMessages, 0)})
	if status != http.StatusCreated {
		t.Fatalf("create with a self view|send entry = %d (%x), want 201", status, body)
	}
	// Control: one more bit, and it is refused.
	status, body = createWith(f.e, f.cid, f.mTok, "refused", 0,
		[]any{everyoneNoSend, ow(1, f.mod, api.PermSendMessages|api.PermPinMessages, 0)})
	if status != http.StatusForbidden {
		t.Fatalf("self send|pin_messages = %d (%x), want 403", status, body)
	}
}

// web3.8 S2 (R5): a creator who holds pin_messages still may not give it to themself. The rank
// rule refuses every self entry beyond view and send, or with any deny, though checkBits passes.
func TestCreatorSelfPinIsStillRanked(t *testing.T) {
	f := privateEnv(t)
	pinner, tok := rankedCreator(t, f, "pinner",
		api.PermViewChannel|api.PermSendMessages|api.PermPinMessages|api.PermManageChannels|api.PermManageRoles)
	everyoneNoSend := ow(0, f.everyone, 0, api.PermSendMessages)
	// Control: the same creator, self send only.
	if status, body := createWith(f.e, f.cid, tok, "news", 0,
		[]any{everyoneNoSend, ow(1, pinner, api.PermSendMessages, 0)}); status != http.StatusCreated {
		t.Fatalf("self send by a pin holder = %d (%x), want 201", status, body)
	}
	cases := []struct {
		name        string
		allow, deny api.Bits
	}{
		{"self pin", api.PermPinMessages, 0},
		{"self send|pin", api.PermSendMessages | api.PermPinMessages, 0},
		{"self view|send|pin", api.PermViewChannel | api.PermSendMessages | api.PermPinMessages, 0},
		{"self send, deny pin", api.PermSendMessages, api.PermPinMessages},
		{"self empty", 0, 0},
	}
	for _, c := range cases {
		status, body := createWith(f.e, f.cid, tok, "refused", 0,
			[]any{everyoneNoSend, ow(1, pinner, c.allow, c.deny)})
		if status != http.StatusForbidden {
			t.Fatalf("%s = %d (%x), want 403", c.name, status, body)
		}
	}
}

// web3.8 S2 (R5): a self deny is not exempt. Control: the self allow of view is.
func TestCreatorSelfDenyIsStillRanked(t *testing.T) {
	f := privateEnv(t)
	cases := []struct {
		name        string
		allow, deny api.Bits
	}{
		{"self deny view", 0, api.PermViewChannel},
		{"self deny send", 0, api.PermSendMessages},
		{"self allow send, deny view", api.PermSendMessages, api.PermViewChannel},
	}
	for _, c := range cases {
		status, body := createWith(f.e, f.cid, f.mTok, "refused", 0, []any{ow(1, f.mod, c.allow, c.deny)})
		if status != http.StatusForbidden {
			t.Fatalf("%s = %d (%x), want 403", c.name, status, body)
		}
	}
	if status, body := createWith(f.e, f.cid, f.mTok, "ok", 0, []any{ow(1, f.mod, api.PermViewChannel, 0)}); status != http.StatusCreated {
		t.Fatalf("control self allow view = %d (%x), want 201", status, body)
	}
}

// web3.8 S2 (R5): the exemption is the creator's own user entry only. Send for another member at
// the creator's rank, or for the creator's own role, is still refused; each control is a target
// below the creator's rank and succeeds.
func TestCreatorSelfSendReachesNoOtherTarget(t *testing.T) {
	f := privateEnv(t)
	peer, _ := joinAs(t, f.e, f.cid, "peer")
	grant(t, f.e, f.cid, f.ownerTok, peer, f.modRole, http.StatusNoContent)
	everyoneNoSend := ow(0, f.everyone, 0, api.PermSendMessages)
	refused := []struct {
		name  string
		entry []any
		want  int
	}{
		{"another member at the creator's rank", ow(1, peer, api.PermSendMessages, 0), http.StatusForbidden},
		{"the owner", ow(1, f.owner, api.PermSendMessages, 0), http.StatusForbidden},
		{"the creator's own role", ow(0, f.modRole, api.PermSendMessages, 0), http.StatusForbidden},
		// The creator's user id given as a role target is no role.
		{"the creator's id as a role", ow(0, f.mod, api.PermSendMessages, 0), http.StatusNotFound},
	}
	for _, c := range refused {
		status, body := createWith(f.e, f.cid, f.mTok, "refused", 0, []any{everyoneNoSend, c.entry})
		if status != c.want {
			t.Fatalf("%s = %d (%x), want %d", c.name, status, body, c.want)
		}
	}
	// Controls: B (no role) and @everyone are below the creator.
	for name, entry := range map[string][]any{
		"B":         ow(1, f.b, api.PermSendMessages, 0),
		"@everyone": ow(0, f.everyone, api.PermSendMessages, 0),
	} {
		if status, body := createWith(f.e, f.cid, f.mTok, "ok", 0, []any{entry}); status != http.StatusCreated {
			t.Fatalf("send for %s = %d (%x), want 201", name, status, body)
		}
	}
}

// web3.8 S2 (R5): the exemption is at create only. After the create, a PUT of the creator's own
// send entry takes putOverwrite's rank rule (unchanged): 403, and a PATCH carries no overwrites.
// Control: the same PUT for B, below the creator, is 204. Guard, green at start: putOverwrite
// never had the exemption, and S2 does not touch it.
func TestPutOverwriteSelfSendIsStillRanked(t *testing.T) {
	f := privateEnv(t)
	// A plain channel, so the mod holds send in it and only the rank rule can refuse the PUT.
	ch, _, status := newChannel(t, f.e, f.cid, f.mTok, 0, 0, 0, "news")
	if status != http.StatusCreated {
		t.Fatalf("create = %d", status)
	}
	path := func(target id.ID) string {
		return "/v1/channels/" + ch.String() + "/overwrites/1/" + target.String()
	}
	if status, body := f.e.Do(http.MethodPut, path(f.mod), f.mTok, []any{uint64(api.PermSendMessages), uint64(0)}); status != http.StatusForbidden {
		t.Fatalf("PUT self send after the create = %d (%x), want 403", status, body)
	}
	if status, body := f.e.Do(http.MethodPut, path(f.mod), f.mTok, []any{uint64(api.PermViewChannel), uint64(0)}); status != http.StatusForbidden {
		t.Fatalf("PUT self view after the create = %d (%x), want 403", status, body)
	}
	// A PATCH with an overwrites element appended is not a PATCH body.
	if status, body := f.e.Do(http.MethodPatch, "/v1/channels/"+ch.String(), f.mTok,
		[]any{nil, nil, nil, nil, nil, nil, nil, []any{ow(1, f.mod, api.PermSendMessages, 0)}}); status != http.StatusBadRequest {
		t.Fatalf("PATCH carrying a self send entry = %d (%x), want 400", status, body)
	}
	if hasOverwrite(t, f.e, ch, f.mod, api.PermSendMessages, 0) {
		t.Fatal("a refused request stored a self send entry")
	}
	if status, body := f.e.Do(http.MethodPut, path(f.b), f.mTok, []any{uint64(api.PermSendMessages), uint64(0)}); status != http.StatusNoContent {
		t.Fatalf("control PUT send for B = %d (%x), want 204", status, body)
	}
}

// web3.8 S2 (R5): the exemption lifts the rank rule only. checkBits still refuses a self send
// entry to a creator who does not hold send; the control is their self view entry.
func TestCreatorWithoutSendCannotAllowIt(t *testing.T) {
	f := privateEnv(t)
	quiet, tok := rankedCreator(t, f, "quiet", api.PermViewChannel|api.PermManageChannels|api.PermManageRoles)
	// @everyone grants send by default; take it away so the creator truly lacks it.
	if status, body := f.e.Do(http.MethodPatch, "/v1/roles/"+f.everyone.String(), f.ownerTok,
		[]any{nil, nil, nil, uint64(api.PermViewChannel), nil, nil, nil}); status != http.StatusNoContent {
		t.Fatalf("PATCH @everyone = %d (%x)", status, body)
	}
	// The self entry alone: no other entry carries send, so only checkBits on the self entry can
	// refuse it.
	if status, body := createWith(f.e, f.cid, tok, "refused", 0,
		[]any{ow(1, quiet, api.PermSendMessages, 0)}); status != http.StatusForbidden {
		t.Fatalf("self send without holding send = %d (%x), want 403", status, body)
	}
	// Control: the same entry allowing view, a bit they hold.
	if status, body := createWith(f.e, f.cid, tok, "ok", 0,
		[]any{ow(1, quiet, api.PermViewChannel, 0)}); status != http.StatusCreated {
		t.Fatalf("control self view = %d (%x), want 201", status, body)
	}
}

// beforeTx is a Repository whose next Tx first runs a change through the inner repository: it is a
// concurrent request that commits after a handler's checks and before its transaction.
type beforeTx struct {
	store.Repository
	mu   sync.Mutex
	next func()
}

func (b *beforeTx) Tx(ctx context.Context, fn func(store.Repository) error) error {
	b.mu.Lock()
	run := b.next
	b.next = nil
	b.mu.Unlock()
	if run != nil {
		run()
	}
	return b.Repository.Tx(ctx, fn)
}

func (b *beforeTx) arm(run func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.next = run
}

// S2, Wave 1's lesson: a moderator demoted after the create's checks and before its transaction
// creates nothing, because every check is taken again inside the transaction under the lock.
func TestAPrivateCreateRechecksUnderTheLock(t *testing.T) {
	e := newEnv(t)
	log := slog.New(slog.DiscardHandler)
	hooked := &beforeTx{Repository: e.Repo}
	api.NewCommunities(e.Repo, e.DS, e.Clk, log).Register(e.Mux)
	api.NewChannels(hooked, e.DS, e.Clk, 10, log).Register(e.Mux)
	api.NewRoles(e.Repo, e.Clk, "dilla.example", log).Register(e.Mux)
	_, ownerTok := e.NewUser("owner")
	cid := createCommunity(t, e, ownerTok)
	mod, modTok := joinAs(t, e, cid, "mod")
	roles, err := e.Repo.ListRoles(t.Context(), cid)
	if err != nil {
		t.Fatalf("ListRoles: %v", err)
	}
	modRole := createRole(t, e, cid, ownerTok, "mod", 5,
		api.PermViewChannel|api.PermManageChannels|api.PermManageRoles)
	grant(t, e, cid, ownerTok, mod, modRole, http.StatusNoContent)

	hooked.arm(func() {
		if err := e.Repo.DeleteMemberRole(t.Context(), cid, mod, modRole); err != nil {
			t.Errorf("demote: %v", err)
		}
	})
	status, body := createWith(e, cid, modTok, "sen", 0,
		[]any{ow(0, roles[0].ID, 0, api.PermViewChannel), ow(1, mod, api.PermViewChannel, 0)})
	if status != http.StatusForbidden {
		t.Fatalf("create by a mod demoted before the transaction = %d, want 403 (%x)", status, body)
	}
	if chans, _ := e.Repo.ListChannels(t.Context(), cid); len(withoutBuiltinAFK(chans)) != 0 {
		t.Fatalf("a refused create wrote %+v", chans)
	}
	// Control: the same create with no demotion in between passes every check before the
	// transaction, so the 403 above could only come from the check inside it.
	grant(t, e, cid, ownerTok, mod, modRole, http.StatusNoContent)
	if status, body := createWith(e, cid, modTok, "sen", 0,
		[]any{ow(0, roles[0].ID, 0, api.PermViewChannel), ow(1, mod, api.PermViewChannel, 0)}); status != http.StatusCreated {
		t.Fatalf("the same create undisturbed = %d (%x), want 201", status, body)
	}
}

// web3.8 S2 (R5): the exemption's checkBits is taken again inside the transaction. A creator whose
// role loses only send_messages between the create's checks and its transaction cannot allow
// themself send: 403, and nothing is written. The control is the same create undisturbed.
func TestCreatorSelfSendRecheckedUnderTheLock(t *testing.T) {
	e := newEnv(t)
	log := slog.New(slog.DiscardHandler)
	hooked := &beforeTx{Repository: e.Repo}
	api.NewCommunities(e.Repo, e.DS, e.Clk, log).Register(e.Mux)
	api.NewChannels(hooked, e.DS, e.Clk, 10, log).Register(e.Mux)
	api.NewRoles(e.Repo, e.Clk, "dilla.example", log).Register(e.Mux)
	_, ownerTok := e.NewUser("owner")
	cid := createCommunity(t, e, ownerTok)
	mod, modTok := joinAs(t, e, cid, "mod")
	roles, err := e.Repo.ListRoles(t.Context(), cid)
	if err != nil {
		t.Fatalf("ListRoles: %v", err)
	}
	// @everyone grants send by default; the mod's role is then their only source of it.
	if status, body := e.Do(http.MethodPatch, "/v1/roles/"+roles[0].ID.String(), ownerTok,
		[]any{nil, nil, nil, uint64(api.PermViewChannel), nil, nil, nil}); status != http.StatusNoContent {
		t.Fatalf("PATCH @everyone = %d (%x)", status, body)
	}
	modRole := createRole(t, e, cid, ownerTok, "mod", 5,
		api.PermViewChannel|api.PermSendMessages|api.PermManageChannels|api.PermManageRoles)
	grant(t, e, cid, ownerTok, mod, modRole, http.StatusNoContent)
	setSend := func(on bool) {
		r, err := e.Repo.GetRole(t.Context(), modRole)
		if err != nil {
			t.Errorf("GetRole: %v", err)
			return
		}
		if on {
			r.Allow |= uint64(api.PermSendMessages)
		} else {
			r.Allow &^= uint64(api.PermSendMessages)
		}
		if err := e.Repo.PutRole(t.Context(), r); err != nil {
			t.Errorf("PutRole: %v", err)
		}
	}
	hooked.arm(func() { setSend(false) })
	self := []any{ow(1, mod, api.PermSendMessages, 0)}
	if status, body := createWith(e, cid, modTok, "news", 0, self); status != http.StatusForbidden {
		t.Fatalf("self send after losing send before the transaction = %d (%x), want 403", status, body)
	}
	if chans, _ := e.Repo.ListChannels(t.Context(), cid); len(withoutBuiltinAFK(chans)) != 0 {
		t.Fatalf("a refused create wrote %+v", chans)
	}
	// Control: send restored and nothing in between, so the 403 above could only come from the
	// check inside the transaction.
	setSend(true)
	if status, body := createWith(e, cid, modTok, "news", 0, self); status != http.StatusCreated {
		t.Fatalf("the same create undisturbed = %d (%x), want 201", status, body)
	}
}

// S2 round 2: a creator's self entry is a member overwrite, and like any other it goes with the
// membership. A moderator who made a private channel visible to themself by a self entry, then is
// kicked, banned (and unbanned) or leaves, and rejoins, does not see the channel again.
func TestASelfEntryEndsWithTheMembership(t *testing.T) {
	for _, how := range []string{"kick", "ban", "leave"} {
		t.Run(how, func(t *testing.T) {
			f := privateEnv(t)
			status, body := createWith(f.e, f.cid, f.mTok, "mods", 0,
				[]any{ow(0, f.everyone, 0, api.PermViewChannel), ow(1, f.mod, api.PermViewChannel, 0)})
			if status != http.StatusCreated {
				t.Fatalf("mod create = %d (%x)", status, body)
			}
			ch := createdID(t, body)
			if !lists(t, f.e, f.cid, f.mTok, ch) {
				t.Fatal("the creator does not list its channel")
			}
			member := "/v1/communities/" + f.cid.String() + "/members/" + f.mod.String()
			ban := "/v1/communities/" + f.cid.String() + "/bans/" + f.mod.String()
			type step struct {
				method, path, tok string
				body              any
			}
			steps := map[string][]step{
				"kick": {{http.MethodDelete, member, f.ownerTok, nil}},
				// Banned, then unbanned so the rejoin is admitted.
				"ban":   {{http.MethodPut, ban, f.ownerTok, []any{"", nil}}, {http.MethodDelete, ban, f.ownerTok, nil}},
				"leave": {{http.MethodPost, "/v1/communities/" + f.cid.String() + "/leave", f.mTok, []any{}}},
			}[how]
			for _, s := range steps {
				if status, body := f.e.Do(s.method, s.path, s.tok, s.body); status != http.StatusNoContent {
					t.Fatalf("%s %s = %d (%x)", s.method, s.path, status, body)
				}
			}
			for _, o := range mustOverwrites(t, f.e, ch) {
				if o.TargetKind == 1 && o.TargetID == f.mod {
					t.Fatalf("the self entry outlived the %s: %+v", how, o)
				}
			}
			if members, _ := listChannelMembers(t, f.e, ch); slices.Contains(members, f.mod) {
				t.Fatalf("channel_members still lists the creator after the %s", how)
			}
			joinCommunity(t, f.e, f.cid, f.mTok)
			if lists(t, f.e, f.cid, f.mTok, ch) {
				t.Fatalf("after the %s and a rejoin the creator lists the private channel", how)
			}
			if status, _ := f.e.Do(http.MethodGet, "/v1/channels/"+ch.String(), f.mTok, nil); status != http.StatusNotFound {
				t.Fatalf("after the %s and a rejoin GET channel = %d, want 404", how, status)
			}
		})
	}
}

// mustOverwrites reads a channel's stored overwrites.
func mustOverwrites(t *testing.T, e *env, ch id.ID) []store.OverwriteRow {
	t.Helper()
	ow, err := e.Repo.ListOverwrites(t.Context(), ch)
	if err != nil {
		t.Fatalf("ListOverwrites: %v", err)
	}
	return ow
}

// S2 round 2: channel PATCH and DELETE take manage_channels again inside their transaction under
// the community row lock. A moderator demoted between the route's checks and its transaction is
// 403 and nothing is written; the same request undisturbed (the control) succeeds. The PATCH is
// the sensitive one: a private channel made invite-visible.
func TestChannelPatchAndDeleteRecheckUnderTheLock(t *testing.T) {
	e := newEnv(t)
	log := slog.New(slog.DiscardHandler)
	hooked := &beforeTx{Repository: e.Repo}
	api.NewCommunities(e.Repo, e.DS, e.Clk, log).Register(e.Mux)
	api.NewChannels(hooked, e.DS, e.Clk, 10, log).Register(e.Mux)
	api.NewRoles(e.Repo, e.Clk, "dilla.example", log).Register(e.Mux)
	_, ownerTok := e.NewUser("owner")
	cid := createCommunity(t, e, ownerTok)
	mod, modTok := joinAs(t, e, cid, "mod")
	modRole := createRole(t, e, cid, ownerTok, "mod", 5, api.PermViewChannel|api.PermManageChannels)
	grant(t, e, cid, ownerTok, mod, modRole, http.StatusNoContent)
	ch, _, _ := newChannel(t, e, cid, ownerTok, 0, 0, uint64(api.VisPrivate), "privat")
	path := "/v1/channels/" + ch.String()

	gone := func() bool {
		_, err := e.Repo.GetChannel(t.Context(), ch)
		return err != nil
	}
	cases := []struct {
		name, method string
		body         any
		unchanged    func() bool
	}{
		{"patch private to invite", http.MethodPatch, []any{nil, nil, nil, uint64(api.VisInvite), nil, nil, nil},
			func() bool { return mustChannel(t, e, ch).Visibility == api.VisPrivate }},
		{"delete", http.MethodDelete, nil, func() bool { return !gone() }},
	}
	for _, c := range cases {
		hooked.arm(func() {
			if err := e.Repo.DeleteMemberRole(t.Context(), cid, mod, modRole); err != nil {
				t.Errorf("demote: %v", err)
			}
		})
		if status, body := e.Do(c.method, path, modTok, c.body); status != http.StatusForbidden {
			t.Fatalf("%s by a mod demoted before the transaction = %d (%x), want 403", c.name, status, body)
		}
		if !c.unchanged() {
			t.Fatalf("the refused %s wrote", c.name)
		}
		if err := e.Repo.PutMemberRole(t.Context(), cid, mod, modRole); err != nil {
			t.Fatalf("re-promote: %v", err)
		}
		if status, body := e.Do(c.method, path, modTok, c.body); status != http.StatusNoContent {
			t.Fatalf("control: %s undisturbed = %d (%x), want 204", c.name, status, body)
		}
		if c.unchanged() {
			t.Fatalf("control: the %s wrote nothing", c.name)
		}
	}
}

// S2: the create body is 8 or 9 elements; any other length is the shape refusal it always was.
func TestAChannelCreateIsEightOrNineElements(t *testing.T) {
	f := privateEnv(t)
	path := "/v1/communities/" + f.cid.String() + "/channels"
	if status, body := f.e.Do(http.MethodPost, path, f.ownerTok,
		[]any{uint64(0), uint64(0), uint64(0), nil, "eight", "", uint64(0), uint64(0)}); status != http.StatusCreated {
		t.Fatalf("8 elements = %d (%x)", status, body)
	}
	for _, body := range [][]any{
		{uint64(0), uint64(0), uint64(0), nil, "seven", "", uint64(0)},
		{uint64(0), uint64(0), uint64(0), nil, "ten", "", uint64(0), uint64(0), []any{ow(0, f.everyone, 0, 1)}, nil},
	} {
		status, resp := f.e.Do(http.MethodPost, path, f.ownerTok, body)
		if status != http.StatusBadRequest || f.e.ErrCode(resp) != "E_INVALID_REQUEST" ||
			!strings.HasPrefix(errDetail(t, resp), "body is not deterministic CBOR") {
			t.Fatalf("%d elements = %d %q, want 400 with the shape refusal", len(body), status, errDetail(t, resp))
		}
	}
	// A ninth element that is not an array of four-element entries is the same refusal.
	status, resp := f.e.Do(http.MethodPost, path, f.ownerTok,
		[]any{uint64(0), uint64(0), uint64(0), nil, "bad", "", uint64(0), uint64(0), []any{[]any{uint64(0)}}})
	if status != http.StatusBadRequest || !strings.HasPrefix(errDetail(t, resp), "body is not deterministic CBOR") {
		t.Fatalf("bad entry = %d %q", status, errDetail(t, resp))
	}
}
