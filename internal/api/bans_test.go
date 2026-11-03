package api_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"
	"golang.org/x/crypto/chacha20"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/ds"
	"github.com/dilla-chat/dilla/internal/ds/dstest"
	"github.com/dilla-chat/dilla/internal/gateway"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/mlswasi"
	"github.com/dilla-chat/dilla/internal/store"
)

// recordingDS records the proposals a handler sends to the delivery service.
// The delivery service's own behaviour is tested in internal/ds; here the
// subject is the membership bookkeeping that drives it.
type recordingDS struct {
	mu      sync.Mutex
	Removes []struct {
		Group id.ID
		Leaf  uint32
	}
	// LeafDevices is the device each leaf Remove in Removes named (ProposeRemoveOf), the zero id
	// for one issued through the bare ProposeRemove.
	LeafDevices []id.ID
	// DeviceRemoves is every call-group Remove, which RemoveUserFromChannelGroups issues by device.
	DeviceRemoves []struct{ Group, Device id.ID }
	Adds          []struct{ Group, Device id.ID }
	Closed        []id.ID
	// Voided is every group VoidIneligibleAdds was asked about, in call order (C2).
	Voided []id.ID
	// Commits counts the commit requests ProposeAddBatch makes: one per non-empty batch of at
	// most 256 Adds, as ds.PlanBatches splits it (task 7).
	Commits int
	// OnCommit, when set, runs after each batch's commit request — the moment a membership change
	// can land between two batches of a join storm.
	OnCommit func()
	// Repo, when set, is what ProposeAddBatch re-reads between batches: a device whose user is no
	// longer in the group's channel_members is dropped from the batches still to come, as the
	// delivery service's own drain drops a device that stopped being eligible.
	Repo store.Repository
}

// Reset forgets every recorded call.
func (d *recordingDS) Reset() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.Removes, d.Adds, d.Closed, d.Voided, d.Commits = nil, nil, nil, nil, 0
	d.DeviceRemoves, d.LeafDevices = nil, nil
}

func (d *recordingDS) VoidIneligibleAdds(_ context.Context, g id.ID) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.Voided = append(d.Voided, g)
	return nil
}

func (d *recordingDS) voided() []id.ID {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.Voided)
}

var _ api.DS = (*recordingDS)(nil)

func (d *recordingDS) ProposeAdd(_ context.Context, g, dev, _ id.ID) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.Adds = append(d.Adds, struct{ Group, Device id.ID }{g, dev})
	return nil
}

func (d *recordingDS) ProposeRemove(_ context.Context, g id.ID, leaf uint32, _ id.ID) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.Removes = append(d.Removes, struct {
		Group id.ID
		Leaf  uint32
	}{g, leaf})
	return nil
}

// ProposeRemoveOf records a leaf Remove like ProposeRemove, plus the device it names in
// LeafDevices, parallel to Removes.
func (d *recordingDS) ProposeRemoveOf(ctx context.Context, g id.ID, leaf uint32, dev, a id.ID) error {
	if err := d.ProposeRemove(ctx, g, leaf, a); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for len(d.LeafDevices) < len(d.Removes)-1 {
		d.LeafDevices = append(d.LeafDevices, id.ID{}) // a bare ProposeRemove named no device
	}
	d.LeafDevices = append(d.LeafDevices, dev)
	return nil
}

// ProposeRemoveOfMember records like ProposeRemoveOf; the delivery service's added-epoch check is
// internal/ds's to test.
func (d *recordingDS) ProposeRemoveOfMember(ctx context.Context, g id.ID, leaf uint32, dev id.ID, _ uint64, a id.ID) error {
	return d.ProposeRemoveOf(ctx, g, leaf, dev, a)
}

func (d *recordingDS) leafDevices() []id.ID {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.LeafDevices)
}

func (d *recordingDS) ProposeRemoveDevice(_ context.Context, g, dev, _ id.ID) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.DeviceRemoves = append(d.DeviceRemoves, struct{ Group, Device id.ID }{g, dev})
	return nil
}

func (d *recordingDS) deviceRemoves() []struct{ Group, Device id.ID } {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.DeviceRemoves)
}

// ProposeAddBatch records the batch the way the delivery service issues it: ds.PlanBatches at 256
// per commit, one commit request per non-empty batch, and — when Repo is set — every later batch
// re-checked against channel_members first.
//
// The split, the Commits counter and the re-check are THIS DOUBLE's contract, not production code:
// an internal/api assertion on them shows only what api handed to ProposeAddBatch (which devices,
// in one call) and, through OnCommit, what production code did to channel_members between two
// batches. The real batching and the real eligibility re-check are pinned in internal/ds
// (batch_test.go) and, with real commits, in internal/testkit.
func (d *recordingDS) ProposeAddBatch(ctx context.Context, g id.ID, devices []id.ID) error {
	for i, batch := range ds.PlanBatches(devices, ds.DefaultPolicy().MaxAddsPerCommit).Batches {
		if i > 0 && d.Repo != nil {
			var err error
			if batch, err = d.stillMembers(ctx, g, batch); err != nil {
				return err
			}
		}
		for _, dev := range batch {
			if err := d.ProposeAdd(ctx, g, dev, id.New()); err != nil {
				return err
			}
		}
		if len(batch) == 0 {
			continue
		}
		d.mu.Lock()
		d.Commits++
		d.mu.Unlock()
		if d.OnCommit != nil {
			d.OnCommit()
		}
	}
	return nil
}

// stillMembers keeps the devices of batch whose user is still in the channel_members of the
// channel group g is bound to.
func (d *recordingDS) stillMembers(ctx context.Context, g id.ID, batch []id.ID) ([]id.ID, error) {
	row, err := d.Repo.GetGroup(ctx, g)
	if err != nil {
		return nil, err
	}
	members, err := d.Repo.ListChannelMembers(ctx, row.TargetID)
	if err != nil {
		return nil, err
	}
	kept := make([]id.ID, 0, len(batch))
	for _, dev := range batch {
		device, err := d.Repo.GetDevice(ctx, dev)
		if err != nil {
			return nil, err
		}
		if slices.Contains(members, device.UserID) {
			kept = append(kept, dev)
		}
	}
	return kept, nil
}

func (d *recordingDS) Close(_ context.Context, g id.ID) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.Closed = append(d.Closed, g)
	return nil
}

// CloseIfAlone records like Close: no test that uses recordingDS ends a call conditionally (callDS
// checks the members; internal/ds tests the rule under the lock).
func (d *recordingDS) CloseIfAlone(ctx context.Context, g, _ id.ID) error { return d.Close(ctx, g) }

func (d *recordingDS) removes() []struct {
	Group id.ID
	Leaf  uint32
} {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.Removes)
}

func (d *recordingDS) closed() []id.ID {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.Closed)
}

func TestBanRemovesMembershipAndIssuesDSRemoves(t *testing.T) {
	e, cid, ownerTok := channelEnv(t)
	dsvc := &recordingDS{}
	api.NewBans(e.Repo, dsvc, e.Clk, slog.New(slog.DiscardHandler)).Register(e.Mux)

	ch, _, _ := newChannel(t, e, cid, ownerTok, 0, 0, 0, "secret")
	target, targetTok := e.NewUser("troll")
	joinCommunity(t, e, cid, targetTok)

	// An MLS text group bound to that channel, with the target's device at leaf 3.
	group := seedTextGroup(t, e, ch, cid)
	device := id.New()
	seedMemberDevice(t, e, group, target, device, 3)

	path := "/v1/communities/" + cid.String() + "/bans/" + target.String()
	if status, body := e.Do(http.MethodPut, path, ownerTok,
		[]any{"spam", nil}); status != http.StatusNoContent {
		t.Fatalf("PUT ban = %d (%x)", status, body)
	}

	if _, err := e.Repo.GetMember(t.Context(), cid, target); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the ban left the membership row: %v", err)
	}
	if got := dsvc.removes(); len(got) != 1 || got[0].Group != group || got[0].Leaf != 3 {
		t.Fatalf("Removes = %+v, want one for leaf 3 of %x", got, group)
	}
	// m1 of the task-9 review: the leaf was read outside the delivery service's group lock, so the
	// Remove names the device it is for, and a leaf reused meanwhile is refused, not redirected.
	if got := dsvc.leafDevices(); len(got) != 1 || got[0] != device {
		t.Fatalf("the text-group Remove named %v, want the device at leaf 3, %s", got, device)
	}
	// Re-joining is refused while the ban stands.
	if status, body := e.Do(http.MethodPost, "/v1/communities/"+cid.String()+"/join", targetTok, []any{nil}); status != http.StatusForbidden {
		t.Fatalf("join while banned = %d", status)
	} else if code := e.ErrCode(body); code != "E_FORBIDDEN" {
		t.Fatalf("code = %s", code)
	}
}

func TestABanWithAnExpiryLapses(t *testing.T) {
	e, cid, ownerTok := channelEnv(t)
	dsvc := &recordingDS{}
	api.NewBans(e.Repo, dsvc, e.Clk, slog.New(slog.DiscardHandler)).Register(e.Mux)
	target, targetTok := e.NewUser("troll")
	joinCommunity(t, e, cid, targetTok)

	until := uint64(e.Clk.Now().Add(time.Hour).Unix())
	if status, _ := e.Do(http.MethodPut,
		"/v1/communities/"+cid.String()+"/bans/"+target.String(), ownerTok,
		[]any{"cool off", until}); status != http.StatusNoContent {
		t.Fatal("PUT ban failed")
	}
	if status, _ := e.Do(http.MethodPost, "/v1/communities/"+cid.String()+"/join", targetTok, []any{nil}); status != http.StatusForbidden {
		t.Fatal("join during the ban was allowed")
	}
	e.Clk.Advance(2 * time.Hour)
	if status, body := e.Do(http.MethodPost, "/v1/communities/"+cid.String()+"/join", targetTok, []any{nil}); status != http.StatusOK {
		t.Fatalf("join after the ban lapsed = %d (%x)", status, body)
	}
	// The row survives the lapse — GET /bans still shows it, so a moderator can
	// see the history — but it no longer gates.
	status, body := e.Do(http.MethodGet, "/v1/communities/"+cid.String()+"/bans", ownerTok, nil)
	if status != http.StatusOK {
		t.Fatalf("GET bans = %d", status)
	}
	var bans [][]cbor.RawMessage
	mustUnmarshalBody(t, body, &bans)
	if len(bans) != 1 {
		t.Fatalf("bans = %d rows, want 1", len(bans))
	}
}

func TestAKickRemovesMembershipButWritesNoBanRow(t *testing.T) {
	e, cid, ownerTok := channelEnv(t)
	dsvc := &recordingDS{}
	api.NewBans(e.Repo, dsvc, e.Clk, slog.New(slog.DiscardHandler)).Register(e.Mux)
	target, targetTok := e.NewUser("noisy")
	joinCommunity(t, e, cid, targetTok)

	if status, _ := e.Do(http.MethodDelete,
		"/v1/communities/"+cid.String()+"/members/"+target.String(), ownerTok, nil); status != http.StatusNoContent {
		t.Fatal("kick failed")
	}
	if _, err := e.Repo.GetMember(t.Context(), cid, target); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("the kick left the membership row")
	}
	if _, err := e.Repo.GetBan(t.Context(), cid, target); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("a kick wrote a ban row")
	}
	// A kicked user may rejoin immediately.
	if status, _ := e.Do(http.MethodPost, "/v1/communities/"+cid.String()+"/join", targetTok, []any{nil}); status != http.StatusOK {
		t.Fatal("a kicked user could not rejoin")
	}
}

func TestEveryModerationActionWritesAnAuditRow(t *testing.T) {
	e, cid, ownerTok := channelEnv(t)
	owner := ownerOf(t, e, cid)
	dsvc := &recordingDS{}
	api.NewBans(e.Repo, dsvc, e.Clk, slog.New(slog.DiscardHandler)).Register(e.Mux)
	target, targetTok := e.NewUser("troll")
	joinCommunity(t, e, cid, targetTok)
	noisy, noisyTok := e.NewUser("noisy")
	joinCommunity(t, e, cid, noisyTok)

	base := "/v1/communities/" + cid.String()
	steps := []struct {
		method, path string
		body         any
		action       string
	}{
		{http.MethodPut, base + "/bans/" + target.String(), []any{"spam", nil}, "ban.create"},
		{http.MethodDelete, base + "/bans/" + target.String(), nil, "ban.delete"},
		{http.MethodDelete, base + "/members/" + noisy.String(), nil, "member.kick"},
	}
	for _, s := range steps {
		if status, _ := e.Do(s.method, s.path, ownerTok, s.body); status != http.StatusNoContent {
			t.Fatalf("%s %s = %d", s.method, s.path, status)
		}
	}
	rows, err := e.Repo.ListAudit(t.Context(), 0, 100)
	if err != nil {
		t.Fatalf("ListAudit: %v", err)
	}
	var seen []string
	for _, row := range rows {
		if row.Actor == nil && row.Detail == "afk.builtin" {
			continue // the system row of the community's built-in AFK channel (web3.9 R1)
		}
		if row.Actor == nil || *row.Actor != owner {
			t.Fatalf("audit row %+v does not name the actor", row)
		}
		seen = append(seen, row.Action)
	}
	for _, want := range []string{"ban.create", "ban.delete", "member.kick"} {
		if !slices.Contains(seen, want) {
			t.Fatalf("no audit row for %s: %v", want, seen)
		}
	}
}

// GET /bans is the header table's [[user_id, reason, by_user, created, expires|null]],
// newest first, and a lifted ban is gone from it.
func TestBanListShapeAndLift(t *testing.T) {
	e, cid, ownerTok := channelEnv(t)
	owner := ownerOf(t, e, cid)
	api.NewBans(e.Repo, &recordingDS{}, e.Clk, slog.New(slog.DiscardHandler)).Register(e.Mux)
	a, _ := e.NewUser("a")
	b, bTok := e.NewUser("b")
	base := "/v1/communities/" + cid.String()

	if status, _ := e.Do(http.MethodPut, base+"/bans/"+a.String(), ownerTok, []any{"first", nil}); status != http.StatusNoContent {
		t.Fatal("ban a failed")
	}
	e.Clk.Advance(time.Minute)
	until := uint64(e.Clk.Now().Add(24 * time.Hour).Unix())
	if status, _ := e.Do(http.MethodPut, base+"/bans/"+b.String(), ownerTok, []any{"", until}); status != http.StatusNoContent {
		t.Fatal("ban b failed")
	}
	status, body := e.Do(http.MethodGet, base+"/bans", ownerTok, nil)
	if status != http.StatusOK {
		t.Fatalf("GET bans = %d", status)
	}
	var bans []struct {
		_       struct{} `cbor:",toarray"`
		UserID  id.ID
		Reason  string
		ByUser  id.ID
		Created int64
		Expires *uint64
		Name    string
		Display string
	}
	mustUnmarshalBody(t, body, &bans)
	if len(bans) != 2 || bans[0].UserID != b || bans[0].Expires == nil || *bans[0].Expires != until ||
		bans[1].UserID != a || bans[1].Reason != "first" || bans[1].ByUser != owner || bans[1].Expires != nil ||
		bans[1].Created != e.Clk.Now().Add(-time.Minute).Unix() {
		t.Fatalf("GET bans = %+v", bans)
	}

	// Lifting a ban deletes the row and lets the user join; lifting it again is 404.
	if status, _ := e.Do(http.MethodDelete, base+"/bans/"+b.String(), ownerTok, nil); status != http.StatusNoContent {
		t.Fatal("DELETE ban failed")
	}
	if status, _ := e.Do(http.MethodDelete, base+"/bans/"+b.String(), ownerTok, nil); status != http.StatusNotFound {
		t.Fatalf("second DELETE ban = %d, want 404", status)
	}
	joinCommunity(t, e, cid, bTok)
	// An unbanned user is not re-added to any group: they re-join.
}

// userGoneRepo reads one user as missing.
type userGoneRepo struct {
	store.Repository
	gone id.ID
}

func (r userGoneRepo) GetUser(ctx context.Context, userID id.ID) (store.UserRow, error) {
	if userID == r.gone {
		return store.UserRow{}, store.ErrNotFound
	}
	return r.Repository.GetUser(ctx, userID)
}

func TestBanListNamesEachAccount(t *testing.T) {
	e, cid, ownerTok := channelEnv(t)
	a, _ := e.NewUser("alice")
	gone, _ := e.NewUser("gone")
	// A ban row's foreign key keeps its account row alive in storage, so the
	// vanished account is simulated: GetUser answers ErrNotFound for it.
	api.NewBans(userGoneRepo{Repository: e.Repo, gone: gone}, &recordingDS{}, e.Clk, slog.New(slog.DiscardHandler)).Register(e.Mux)
	base := "/v1/communities/" + cid.String()
	owner := ownerOf(t, e, cid)
	for i, u := range []id.ID{a, gone} {
		if err := e.Repo.PutBan(t.Context(), store.BanRow{CommunityID: cid, UserID: u, ByUser: owner,
			Created: e.Clk.Now().Add(time.Duration(i) * time.Minute).Unix()}); err != nil {
			t.Fatalf("PutBan: %v", err)
		}
	}
	status, body := e.Do(http.MethodGet, base+"/bans", ownerTok, nil)
	if status != http.StatusOK {
		t.Fatalf("GET bans = %d", status)
	}
	var raw []cbor.RawMessage
	mustUnmarshalBody(t, body, &raw)
	if len(raw) != 2 {
		t.Fatalf("bans = %d rows, want 2", len(raw))
	}
	var rows [2][]cbor.RawMessage
	for i := range raw {
		mustUnmarshalBody(t, raw[i], &rows[i])
		if len(rows[i]) != 7 {
			t.Fatalf("row %d has %d fields, want 7", i, len(rows[i]))
		}
	}
	str := func(m cbor.RawMessage) string {
		var s string
		mustUnmarshalBody(t, m, &s)
		return s
	}
	// Newest first: the account with no user row, then alice.
	if u, d := str(rows[0][5]), str(rows[0][6]); u != "" || d != "" {
		t.Errorf("deleted account names = %q, %q, want empty", u, d)
	}
	user, err := e.Repo.GetUser(t.Context(), a)
	if err != nil {
		t.Fatalf("GetUser: %v", err)
	}
	if u, d := str(rows[1][5]), str(rows[1][6]); u != user.Username || d != "alice" {
		t.Errorf("alice names = %q, %q, want %q, alice", u, d, user.Username)
	}
}

// A deleted account is tombstoned: it keeps its username (R36) and its display name is blanked, so
// its ban row reads [username, ""]. Both are "" only when the account row itself is gone.
func TestBanListNamesATombstonedAccountByUsernameOnly(t *testing.T) {
	e, cid, ownerTok := channelEnv(t)
	api.NewBans(e.Repo, &recordingDS{}, e.Clk, slog.New(slog.DiscardHandler)).Register(e.Mux)
	banned, _ := e.NewUser("bertil")
	user, err := e.Repo.GetUser(t.Context(), banned)
	if err != nil {
		t.Fatalf("GetUser: %v", err)
	}
	owner := ownerOf(t, e, cid)
	if err := e.Repo.PutBan(t.Context(), store.BanRow{CommunityID: cid, UserID: banned, ByUser: owner,
		Created: e.Clk.Now().Unix()}); err != nil {
		t.Fatalf("PutBan: %v", err)
	}
	if err := e.Repo.TombstoneUser(t.Context(), banned, e.Clk.Now().Unix()); err != nil {
		t.Fatalf("TombstoneUser: %v", err)
	}
	status, body := e.Do(http.MethodGet, "/v1/communities/"+cid.String()+"/bans", ownerTok, nil)
	if status != http.StatusOK {
		t.Fatalf("GET bans = %d", status)
	}
	var rows []struct {
		_        struct{} `cbor:",toarray"`
		UserID   id.ID
		Reason   string
		ByUser   id.ID
		Created  int64
		Expires  *int64
		Username string
		Display  string
	}
	mustUnmarshalBody(t, body, &rows)
	if len(rows) != 1 || rows[0].UserID != banned {
		t.Fatalf("bans = %+v, want the one tombstoned account", rows)
	}
	if rows[0].Username != user.Username || rows[0].Display != "" {
		t.Fatalf("tombstoned account names = %q, %q, want %q, \"\"", rows[0].Username, rows[0].Display, user.Username)
	}
}

// Who may ban, kick and read the list, and what a request may carry.
func TestModerationGates(t *testing.T) {
	e, cid, ownerTok := channelEnv(t)
	api.NewBans(e.Repo, &recordingDS{}, e.Clk, slog.New(slog.DiscardHandler)).Register(e.Mux)
	api.NewRoles(e.Repo, e.Clk, "dilla.example", slog.New(slog.DiscardHandler)).Register(e.Mux)
	base := "/v1/communities/" + cid.String()
	owner := ownerOf(t, e, cid)

	mod, modTok := e.NewUser("mod")
	joinCommunity(t, e, cid, modTok)
	modRole := createRole(t, e, cid, ownerTok, "mods", 10, api.PermViewChannel|api.PermKickMembers|api.PermBanMembers)
	grant(t, e, cid, ownerTok, mod, modRole, http.StatusNoContent)

	peer, peerTok := e.NewUser("peer")
	joinCommunity(t, e, cid, peerTok)
	grant(t, e, cid, ownerTok, peer, modRole, http.StatusNoContent)

	plain, plainTok := e.NewUser("plain")
	joinCommunity(t, e, cid, plainTok)
	_, outsiderTok := e.NewUser("outsider")

	for _, c := range []struct {
		name, method, path, tok string
		body                    any
		want                    int
		code                    string
	}{
		{"a non-member bans", http.MethodPut, base + "/bans/" + plain.String(), outsiderTok, []any{"", nil}, http.StatusNotFound, "E_NOT_FOUND"},
		{"a non-member lists bans", http.MethodGet, base + "/bans", outsiderTok, nil, http.StatusNotFound, "E_NOT_FOUND"},
		{"a non-member kicks", http.MethodDelete, base + "/members/" + plain.String(), outsiderTok, nil, http.StatusNotFound, "E_NOT_FOUND"},
		{"a member without ban_members bans", http.MethodPut, base + "/bans/" + mod.String(), plainTok, []any{"", nil}, http.StatusForbidden, "E_FORBIDDEN"},
		{"a member without ban_members lists bans", http.MethodGet, base + "/bans", plainTok, nil, http.StatusForbidden, "E_FORBIDDEN"},
		{"a member without ban_members lifts a ban", http.MethodDelete, base + "/bans/" + mod.String(), plainTok, nil, http.StatusForbidden, "E_FORBIDDEN"},
		{"a member without kick_members kicks", http.MethodDelete, base + "/members/" + mod.String(), plainTok, nil, http.StatusForbidden, "E_FORBIDDEN"},
		{"anyone bans the owner", http.MethodPut, base + "/bans/" + owner.String(), modTok, []any{"", nil}, http.StatusForbidden, "E_FORBIDDEN"},
		{"anyone kicks the owner", http.MethodDelete, base + "/members/" + owner.String(), modTok, nil, http.StatusForbidden, "E_FORBIDDEN"},
		{"the owner bans themself", http.MethodPut, base + "/bans/" + owner.String(), ownerTok, []any{"", nil}, http.StatusForbidden, "E_FORBIDDEN"},
		{"a moderator bans an equal", http.MethodPut, base + "/bans/" + peer.String(), modTok, []any{"", nil}, http.StatusForbidden, "E_FORBIDDEN"},
		{"a moderator kicks an equal", http.MethodDelete, base + "/members/" + peer.String(), modTok, nil, http.StatusForbidden, "E_FORBIDDEN"},
		{"a moderator bans themself", http.MethodPut, base + "/bans/" + mod.String(), modTok, []any{"", nil}, http.StatusForbidden, "E_FORBIDDEN"},
		{"a reason over 512 bytes", http.MethodPut, base + "/bans/" + plain.String(), modTok, []any{strings.Repeat("x", 513), nil}, http.StatusBadRequest, "E_INVALID_REQUEST"},
		{"an expiry in the past", http.MethodPut, base + "/bans/" + plain.String(), modTok, []any{"", uint64(e.Clk.Now().Unix())}, http.StatusBadRequest, "E_INVALID_REQUEST"},
		{"an unknown user", http.MethodPut, base + "/bans/" + id.New().String(), modTok, []any{"", nil}, http.StatusNotFound, "E_NOT_FOUND"},
		{"a kick of a non-member", http.MethodDelete, base + "/members/" + id.New().String(), modTok, nil, http.StatusNotFound, "E_NOT_FOUND"},
		{"a malformed user id", http.MethodPut, base + "/bans/NOT-HEX", modTok, []any{"", nil}, http.StatusBadRequest, "E_INVALID_REQUEST"},
	} {
		status, body := e.Do(c.method, c.path, c.tok, c.body)
		if status != c.want {
			t.Errorf("%s: %d (%x), want %d", c.name, status, body, c.want)
			continue
		}
		if code := e.ErrCode(body); code != c.code {
			t.Errorf("%s: code %s, want %s", c.name, code, c.code)
		}
	}
	if status, _ := e.Do(http.MethodGet, base+"/bans", "", nil); status != http.StatusUnauthorized {
		t.Errorf("GET bans without a session = %d, want 401", status)
	}

	// A moderator acts on a member strictly below them, and on a non-member
	// (a ban may precede a join).
	if status, body := e.Do(http.MethodDelete, base+"/members/"+plain.String(), modTok, nil); status != http.StatusNoContent {
		t.Fatalf("moderator kick = %d (%x)", status, body)
	}
	if status, body := e.Do(http.MethodPut, base+"/bans/"+plain.String(), modTok, []any{"", nil}); status != http.StatusNoContent {
		t.Fatalf("moderator ban of a non-member = %d (%x)", status, body)
	}
	if status, _ := e.Do(http.MethodGet, base+"/bans", modTok, nil); status != http.StatusOK {
		t.Fatalf("moderator GET bans = %d", status)
	}
	if status, _ := e.Do(http.MethodPost, base+"/join", plainTok, []any{nil}); status != http.StatusForbidden {
		t.Fatal("a user banned before joining could join")
	}
}

// Kicking and leaving reach the delivery service exactly as a ban does: one
// Remove per live leaf of the user in every text and call group of the
// community's channels, and nothing for a leaf already removed or another user.
func TestKickAndLeaveIssueDSRemoves(t *testing.T) {
	e, cid, ownerTok := channelEnv(t)
	text, _, _ := newChannel(t, e, cid, ownerTok, 0, 0, 0, "secret")
	voice, _, status := newChannel(t, e, cid, ownerTok, 1, 0, 0, "call")
	if status != http.StatusCreated {
		t.Fatalf("voice channel = %d", status)
	}
	kicked, kickedTok := e.NewUser("kicked")
	joinCommunity(t, e, cid, kickedTok)
	leaver, leaverTok := e.NewUser("leaver")
	joinCommunity(t, e, cid, leaverTok)
	bystander, bystanderTok := e.NewUser("bystander")
	joinCommunity(t, e, cid, bystanderTok)

	tg := seedTextGroup(t, e, text, cid)
	cg := seedGroupOfKind(t, e, voice, cid, 1)
	seedMember(t, e, tg, kicked, 1)
	seedMember(t, e, tg, kicked, 2) // a second device
	seedMember(t, e, tg, bystander, 3)
	seedMember(t, e, cg, kicked, 4)
	cgMembers, err := e.Repo.ListMembers(t.Context(), cg)
	if err != nil || len(cgMembers) != 1 {
		t.Fatalf("call group members %+v, %v", cgMembers, err)
	}
	callDevice := cgMembers[0].DeviceID
	seedMember(t, e, tg, leaver, 5)
	// A leaf of the kicked user an earlier commit already removed.
	members, _ := e.Repo.ListMembers(t.Context(), tg)
	gone := uint64(1)
	members = append(members, store.MemberRow{
		GroupID: tg, LeafIndex: 6, UserID: kicked, DeviceID: id.New(), SignatureKey: make([]byte, 32),
		RemovedEpoch: &gone,
	})
	if err := e.Repo.ReplaceMembers(t.Context(), tg, 1, members); err != nil {
		t.Fatalf("ReplaceMembers: %v", err)
	}
	// A group of another channel's target that happens to hold the user is not touched.
	stray := seedTextGroup(t, e, id.New(), cid)
	seedMember(t, e, stray, kicked, 7)

	if status, _ := e.Do(http.MethodDelete, "/v1/communities/"+cid.String()+"/members/"+kicked.String(), ownerTok, nil); status != http.StatusNoContent {
		t.Fatal("kick failed")
	}
	type rm = struct {
		Group id.ID
		Leaf  uint32
	}
	got := e.DS.removes()
	want := []rm{{tg, 1}, {tg, 2}}
	if len(got) != len(want) {
		t.Fatalf("Removes after the kick = %+v, want %+v", got, want)
	}
	for _, w := range want {
		if !slices.Contains(got, w) {
			t.Fatalf("Removes after the kick = %+v, missing %+v", got, w)
		}
	}
	// The call leaf is removed by device (DEV-45).
	if dr := e.DS.deviceRemoves(); len(dr) != 1 || dr[0].Group != cg || dr[0].Device != callDevice {
		t.Fatalf("device Removes after the kick = %+v, want the call leaf's device in %x", dr, cg)
	}

	if status, _ := e.Do(http.MethodPost, "/v1/communities/"+cid.String()+"/leave", leaverTok, []any{}); status != http.StatusNoContent {
		t.Fatal("leave failed")
	}
	got = e.DS.removes()
	if len(got) != 3 || got[2] != (rm{tg, 5}) {
		t.Fatalf("Removes after the leave = %+v, want one more for leaf 5", got)
	}
}

// C2 (fix wave): a user removed while one of their devices' Adds is still outstanding would leave
// the group uncommittable (clause 1 demands the Add, the ACL clause refuses it), so every group a
// removal visits is asked to void its ineligible Adds: the kick path, and SyncGroupMembers, which
// is also the group-DM removal path, before it issues its Removes.
func TestRemovalsVoidOutstandingAddsOfIneligibleUsers(t *testing.T) {
	e, cid, ownerTok := channelEnv(t)
	text, _, _ := newChannel(t, e, cid, ownerTok, 0, 0, 0, "secret")
	voice, _, status := newChannel(t, e, cid, ownerTok, 1, 0, 0, "call")
	if status != http.StatusCreated {
		t.Fatalf("voice channel = %d", status)
	}
	kicked, kickedTok := e.NewUser("kicked")
	joinCommunity(t, e, cid, kickedTok)
	tg := seedTextGroup(t, e, text, cid)
	cg := seedGroupOfKind(t, e, voice, cid, 1)

	if status, _ := e.Do(http.MethodDelete, "/v1/communities/"+cid.String()+"/members/"+kicked.String(), ownerTok, nil); status != http.StatusNoContent {
		t.Fatal("kick failed")
	}
	got := e.DS.voided()
	if !slices.Contains(got, tg) || !slices.Contains(got, cg) {
		t.Fatalf("VoidIneligibleAdds after the kick visited %v, want both %x and %x", got, tg, cg)
	}

	e.DS.Reset()
	row, err := e.Repo.GetChannel(t.Context(), text)
	if err != nil {
		t.Fatalf("GetChannel: %v", err)
	}
	if err := api.SyncGroupMembers(t.Context(), e.Repo, e.DS, row, e.Clk.Now().Unix()); err != nil {
		t.Fatalf("SyncGroupMembers: %v", err)
	}
	if got := e.DS.voided(); !slices.Contains(got, tg) {
		t.Fatalf("SyncGroupMembers asked VoidIneligibleAdds about %v, want %x", got, tg)
	}
}

// Deleting a channel closes its groups, and deleting a community closes every
// group of its channels — after the transaction that tombstones them.
func TestDeletingAChannelOrCommunityClosesItsGroups(t *testing.T) {
	e, cid, ownerTok := channelEnv(t)
	a, _, _ := newChannel(t, e, cid, ownerTok, 0, 0, 0, "a")
	b, _, _ := newChannel(t, e, cid, ownerTok, 0, 0, 0, "b")
	voice, _, _ := newChannel(t, e, cid, ownerTok, 1, 0, 0, "call")
	ga := seedTextGroup(t, e, a, cid)
	gb := seedTextGroup(t, e, b, cid)
	gv := seedGroupOfKind(t, e, voice, cid, 1)

	if status, _ := e.Do(http.MethodDelete, "/v1/channels/"+a.String(), ownerTok, nil); status != http.StatusNoContent {
		t.Fatal("DELETE channel failed")
	}
	if got := e.DS.closed(); len(got) != 1 || got[0] != ga {
		t.Fatalf("Closed after the channel delete = %v, want [%v]", got, ga)
	}
	if status, _ := e.Do(http.MethodDelete, "/v1/communities/"+cid.String(), ownerTok, nil); status != http.StatusNoContent {
		t.Fatal("DELETE community failed")
	}
	got := e.DS.closed()
	if len(got) != 3 || !slices.Contains(got, gb) || !slices.Contains(got, gv) {
		t.Fatalf("Closed after the community delete = %v, want %v, %v and %v", got, ga, gb, gv)
	}
}

// TestBanAgainstARealPoolDoesNotDeadlock drives the ban, and then the community
// delete, against a real single-connection SQLite write pool and a real
// delivery service writing through the SAME repository, over the committed
// 1,500-leaf fixture group and the real wasm core. A delivery-service write
// inside the ban's transaction would wait forever on the pool the transaction
// holds; this is the test that would hang.
func TestBanAgainstARealPoolDoesNotDeadlock(t *testing.T) {
	e := newEnv(t)
	discard := slog.New(slog.DiscardHandler)
	dsvc, group := newRealDS(t, e)
	api.NewCommunities(e.Repo, dsvc, e.Clk, discard).Register(e.Mux)
	api.NewBans(e.Repo, dsvc, e.Clk, discard).Register(e.Mux)
	_, ownerTok := e.NewUser("owner")
	cid := createCommunity(t, e, ownerTok)

	// The fixture's binding targets the group's own id, so a text channel of
	// this community with that id is the channel the group is bound to.
	c := cid
	if err := e.Repo.CreateChannel(t.Context(), store.ChannelRow{
		ID: group, CommunityID: &c, Kind: 0, Mode: 0, Visibility: 0, Name: "secret",
		SettingsJSON: []byte(`{}`), HostPolicyVersion: 1, Created: e.Clk.Now().Unix(),
	}); err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	members, err := e.Repo.ListMembers(t.Context(), group)
	if err != nil {
		t.Fatalf("ListMembers: %v", err)
	}
	var leaf store.MemberRow
	for _, m := range members {
		if m.LeafIndex == 1 {
			leaf = m
		}
	}
	target, targetTok := e.NewUserWithID(leaf.UserID, "troll")
	joinCommunity(t, e, cid, targetTok)

	status, err := doBounded(t, e, 30*time.Second, http.MethodPut,
		"/v1/communities/"+cid.String()+"/bans/"+target.String(), ownerTok, []any{"spam", nil})
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		t.Fatal("the ban path deadlocked: a delivery-service write is inside the ban transaction")
	case err != nil:
		t.Fatalf("PUT ban: %v", err)
	case status != http.StatusNoContent:
		t.Fatalf("PUT ban = %d", status)
	}

	// The real delivery service holds a Remove for every leaf of the banned user,
	// and the group is frozen behind it.
	g, err := e.Repo.GetGroup(t.Context(), group)
	if err != nil {
		t.Fatalf("GetGroup: %v", err)
	}
	pending, err := e.Repo.ListProposals(t.Context(), group, g.Epoch, false)
	if err != nil {
		t.Fatalf("ListProposals: %v", err)
	}
	want := 0
	for _, m := range members {
		if m.UserID == target && m.RemovedEpoch == nil {
			want++
		}
	}
	removed := 0
	for _, p := range pending {
		if p.TargetLeaf != nil && p.Origin == 0 {
			removed++
		}
	}
	if want == 0 || removed != want {
		t.Fatalf("%d instance Removes pending, want %d (one per leaf of the banned user)", removed, want)
	}

	// Deleting the community closes the group through the same real delivery
	// service, after its own transaction.
	status, err = doBounded(t, e, 30*time.Second, http.MethodDelete, "/v1/communities/"+cid.String(), ownerTok, nil)
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		t.Fatal("the community delete deadlocked: ds.Close is inside the delete transaction")
	case err != nil:
		t.Fatalf("DELETE community: %v", err)
	case status != http.StatusNoContent:
		t.Fatalf("DELETE community = %d", status)
	}
	if g, err := e.Repo.GetGroup(t.Context(), group); err != nil || g.ClosedAt == nil {
		t.Fatalf("the deleted community's group is still open: %+v, %v", g, err)
	}
}

// doBounded is env.Do with a deadline: it answers the status, or the error that
// ended the request — context.DeadlineExceeded when the handler never answered.
func doBounded(t *testing.T, e *env, within time.Duration, method, path, token string, body any) (int, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), within)
	defer cancel()
	var rdr *bytes.Reader
	if body != nil {
		b, err := cborx.Marshal(body)
		if err != nil {
			return 0, err
		}
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req, err := http.NewRequestWithContext(ctx, method, e.Srv.URL+path, rdr)
	if err != nil {
		return 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/cbor")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := e.Srv.Client().Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		return 0, err
	}
	defer resp.Body.Close()
	return resp.StatusCode, nil
}

// newRealDS builds the real *ds.DS over e.Repo — the same repository, and so
// the same single-connection write pool, the handlers write through — and
// seeds the committed fixture group in it (dstest). It returns the delivery service
// and the group's id.
func newRealDS(t *testing.T, e *env) (*ds.DS, id.ID) {
	t.Helper()
	f := apiFixtureData(t)
	ctx := context.Background()
	// The guest validates the fixture's KeyPackage lifetimes against its own
	// clock, which must sit inside their validity window; the handlers' clock is
	// the harness's.
	wasmClk := clock.NewFake(time.Unix(time.Now().Unix(), 0))
	wasm, err := mlswasi.New(ctx, f.wasm, mlswasi.Options{
		PoolSize: 2, CacheDir: apiWasmCacheDir(t), Now: wasmClk.Now,
	})
	if err != nil {
		t.Fatalf("mlswasi.New: %v", err)
	}
	t.Cleanup(func() { _ = wasm.Close(context.Background()) })
	gw := gateway.New(gateway.Options{Clock: e.Clk, Store: apiGatewayStore{e.Repo}, Generation: 1})
	t.Cleanup(func() { _ = gw.Shutdown(context.Background()) })

	var keys ds.InstanceKeys
	for i := range keys.InstanceID {
		keys.InstanceID[i] = 0x11 // testkit/src/fixtures.rs:98
	}
	keys.ExternalSenderPriv = fixtureExternalSenderKey()
	d, err := ds.New(ds.Options{
		Store: e.Repo, Wasm: wasm, Gateway: gw, Clock: e.Clk,
		Policy: ds.DefaultPolicy(), Keys: keys, Channels: openChannels{},
	})
	if err != nil {
		t.Fatalf("ds.New: %v", err)
	}
	t.Cleanup(func() { _ = d.Shutdown(context.Background()) })
	// Seeded, not registered: POST /v1/groups adopts one leaf, the registering device's own
	// (hardening G), and this is the 1,500-member fixture group at epoch 6. The seed writes through
	// the same repository, which is what this test is about.
	if err := dstest.SeedGroup(ctx, d, e.Repo, wasm, keys.ExternalSenderKeyID, e.Clk.Now().Unix(), dstest.Group{
		GroupID: f.groupID, Binding: f.binding, GroupInfo: f.groupInfo, RatchetTree: f.ratchetTree,
	}); err != nil {
		t.Fatalf("seed the fixture group: %v", err)
	}
	return d, f.groupID
}

// fixtureExternalSenderKey is the committed fixture's external-sender signing
// key, re-derived exactly as internal/ds/proposal_test.go derives it: the
// guest verifies an external proposal against the group's external_senders, so
// a key of the test's own invention would fail every Remove at the guest.
func fixtureExternalSenderKey() [32]byte {
	const seed = uint64(0x5eed) ^ 0x0d15_0d15
	var material [32]byte
	binary.BigEndian.PutUint64(material[0:8], seed)
	binary.BigEndian.PutUint64(material[8:16], seed*0x9e37_79b9)
	var nonce [12]byte
	c, err := chacha20.NewUnauthenticatedCipher(material[:], nonce[:])
	if err != nil {
		panic(err)
	}
	var out [32]byte
	c.XORKeyStream(out[:], out[:])
	return out
}

// banRacer is the Communities handler's repository with a ban that lands the
// first time the handler reads the ban row. Outside a transaction the ban lands
// at once, between the join gate's read and the membership write, which is the
// window a check-then-act join leaves open. Inside a transaction it lands from a
// goroutine: a join that reads and writes under one lock makes the ban's own
// transaction wait for the join to commit, and the ban then removes the member
// it finds.
type banRacer struct {
	store.Repository
	inTx bool
	once *sync.Once
	land func()
	done chan struct{}
}

func (b *banRacer) GetBan(ctx context.Context, cid, uid id.ID) (store.BanRow, error) {
	row, err := b.Repository.GetBan(ctx, cid, uid)
	b.once.Do(func() {
		if b.inTx {
			go func() {
				defer close(b.done)
				b.land()
			}()
			return
		}
		defer close(b.done)
		b.land()
	})
	return row, err
}

func (b *banRacer) Tx(ctx context.Context, fn func(store.Repository) error) error {
	return b.Repository.Tx(ctx, func(tx store.Repository) error {
		return fn(&banRacer{Repository: tx, inTx: true, once: b.once, land: b.land, done: b.done})
	})
}

// A ban that commits while a join is between its ban check and its membership
// write must not leave a banned member: either the join sees the ban and is
// refused, or the join commits first and the ban removes the row it wrote.
func TestABanRacingAJoinLeavesNoBannedMember(t *testing.T) {
	e := newEnv(t)
	discard := slog.New(slog.DiscardHandler)
	api.NewBans(e.Repo, &recordingDS{}, e.Clk, discard).Register(e.Mux)
	_, ownerTok := e.NewUser("owner")
	target, targetTok := e.NewUser("troll")

	racer := &banRacer{Repository: e.Repo, once: &sync.Once{}, done: make(chan struct{})}
	api.NewCommunities(racer, e.DS, e.Clk, discard).Register(e.Mux)
	cid := createCommunity(t, e, ownerTok)

	racer.land = func() {
		status, err := doBounded(t, e, 30*time.Second, http.MethodPut,
			"/v1/communities/"+cid.String()+"/bans/"+target.String(), ownerTok, []any{"raid", nil})
		if err != nil || status != http.StatusNoContent {
			t.Errorf("PUT ban during the join = %d, %v", status, err)
		}
	}

	status, body := e.Do(http.MethodPost, "/v1/communities/"+cid.String()+"/join", targetTok, []any{nil})
	if status != http.StatusOK && status != http.StatusForbidden {
		t.Fatalf("join = %d (%x)", status, body)
	}
	select {
	case <-racer.done:
	case <-time.After(30 * time.Second):
		t.Fatal("the join never read the ban row, or the ban never landed")
	}

	if _, err := e.Repo.GetBan(t.Context(), cid, target); err != nil {
		t.Fatalf("GetBan after the race: %v", err)
	}
	if _, err := e.Repo.GetMember(t.Context(), cid, target); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a banned user is a member after racing a ban with a join (join = %d): %v", status, err)
	}
}
