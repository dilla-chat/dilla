package api_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/fxamacker/cbor/v2"
	"github.com/livekit/protocol/livekit"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

// afkPointer is the community's stored AFK pointer, failing the test when there is none.
func afkPointer(t *testing.T, e *env, cid id.ID) (id.ID, uint64) {
	t.Helper()
	cfg, err := e.Repo.GetCommunityAFK(t.Context(), cid)
	if err != nil {
		t.Fatalf("GetCommunityAFK: %v", err)
	}
	if cfg.ChannelID == nil {
		t.Fatalf("community %s has no AFK pointer", cid)
	}
	return *cfg.ChannelID, cfg.TimeoutSeconds
}

// afkSnapshotOf reads GET /v1/communities/{id}/afk as tok: the channel, the timeout, the length of the
// retired third element and pending, which must be a uint (W-1: the subset has no booleans). The
// fifth element, the away roster (AW2), is read by afkAwayOf.
func afkSnapshotOf(t *testing.T, e *env, cid id.ID, tok string) (*id.ID, uint64, int, uint64) {
	t.Helper()
	status, body := e.Do(http.MethodGet, "/v1/communities/"+cid.String()+"/afk", tok, nil)
	if status != http.StatusOK {
		t.Fatalf("GET afk = %d (%x)", status, body)
	}
	var raw []cbor.RawMessage
	mustUnmarshal(t, body, &raw)
	if len(raw) != 5 {
		t.Fatalf("AFK snapshot has %d elements, want 5 (%x)", len(raw), body)
	}
	var ch *id.ID
	var timeout, pending uint64
	var retired []cbor.RawMessage
	mustUnmarshal(t, raw[0], &ch)
	mustUnmarshal(t, raw[1], &timeout)
	mustUnmarshal(t, raw[2], &retired)
	mustUnmarshal(t, raw[3], &pending)
	if pending > 2 {
		t.Fatalf("pending = %d, want a reason 0, 1 or 2", pending)
	}
	return ch, timeout, len(retired), pending
}

// seedStoreCommunity writes a community the way a web3.8 instance left it: the community, its owner
// as a member and @everyone, with no AFK channel. It returns the community and @everyone's id.
func seedStoreCommunity(t *testing.T, e *env, owner id.ID, name string) (id.ID, id.ID) {
	t.Helper()
	cid, everyone := id.New(), id.New()
	now := e.Clk.Now().Unix()
	if err := e.Repo.CreateCommunity(t.Context(), store.CommunityRow{ID: cid, Owner: owner, Name: name,
		PolicyJSON: []byte("{}"), PolicyVersion: 1, Created: now}); err != nil {
		t.Fatalf("CreateCommunity: %v", err)
	}
	if err := e.Repo.PutMember(t.Context(), store.MemberOfCommunityRow{CommunityID: cid, UserID: owner, Joined: now}); err != nil {
		t.Fatalf("PutMember: %v", err)
	}
	if err := e.Repo.PutRole(t.Context(), store.RoleRow{ID: everyone, CommunityID: cid, Name: "@everyone",
		Allow: uint64(api.DefaultEveryoneAllow), Created: now}); err != nil {
		t.Fatalf("PutRole: %v", err)
	}
	return cid, everyone
}

func seedVoice(t *testing.T, e *env, cid id.ID, name string) id.ID {
	t.Helper()
	ch := id.New()
	if err := e.Repo.CreateChannel(t.Context(), store.ChannelRow{ID: ch, CommunityID: &cid, Kind: api.ChannelVoice,
		Name: name, SettingsJSON: []byte("{}"), HostPolicyVersion: 1, Created: e.Clk.Now().Unix()}); err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	return ch
}

// builtinAudit reports whether the audit log holds the system row that marks ch as built in.
func builtinAudit(t *testing.T, e *env, ch id.ID) bool {
	t.Helper()
	rows, err := e.Repo.ListAudit(t.Context(), 0, 10_000)
	if err != nil {
		t.Fatalf("ListAudit: %v", err)
	}
	for _, a := range rows {
		if a.Actor == nil && a.Action == "channel.create" && a.Detail == "afk.builtin" && a.Target == ch.String() {
			return true
		}
	}
	return false
}

func voiceNamed(t *testing.T, e *env, cid id.ID, name string) []store.ChannelRow {
	t.Helper()
	all, err := e.Repo.ListChannels(t.Context(), cid)
	if err != nil {
		t.Fatalf("ListChannels: %v", err)
	}
	var out []store.ChannelRow
	for _, c := range all {
		if c.Kind == api.ChannelVoice && c.Name == name {
			out = append(out, c)
		}
	}
	return out
}

func TestNewCommunityHasOneAFK(t *testing.T) {
	e, cid, tok := channelEnv(t)
	owner := e.sess[tok].UserID
	all, err := e.Repo.ListChannels(t.Context(), cid)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("a new community has %d channels, want exactly the AFK channel", len(all))
	}
	afk := all[0]
	if afk.Kind != api.ChannelVoice || afk.Name != "AFK" || afk.Position != api.AFKBuiltinPosition ||
		afk.Mode != api.ModeE2EE || afk.Visibility != api.VisPrivate || afk.ParentID != nil || afk.Topic != "" ||
		afk.SlowmodeSeconds != 0 {
		t.Fatalf("AFK row = %+v", afk)
	}
	if ow, err := e.Repo.ListOverwrites(t.Context(), afk.ID); err != nil || len(ow) != 0 {
		t.Fatalf("AFK overwrites = %v, %v", ow, err)
	}
	ptr, timeout := afkPointer(t, e, cid)
	if ptr != afk.ID || timeout != 300 {
		t.Fatalf("pointer = %s %d, want %s 300", ptr, timeout, afk.ID)
	}
	if !builtinAudit(t, e, afk.ID) {
		t.Fatal("no channel.create audit row with a nil actor and detail afk.builtin")
	}
	members, err := e.Repo.ListChannelMembers(t.Context(), afk.ID)
	if err != nil || !slices.Contains(members, owner) {
		t.Fatalf("AFK channel members = %v, %v; want the creator", members, err)
	}
}

func TestEnsureAFKChannels(t *testing.T) {
	e, _, ownerTok := channelEnv(t)
	log := slog.New(slog.DiscardHandler)
	stub := &stubSFU{}
	calls := api.NewCalls(e.Repo, api.NewResolver(e.Repo), stub, api.CallsConfig{LiveKitURL: testLiveKitURL}, e.Clk, log)
	calls.Register(e.Mux)
	owner := e.sess[ownerTok].UserID
	ownerDev := deviceOf(t, e, ownerTok)
	member, _ := e.NewUser("member")

	// 1. no community_afk row.
	none, _ := seedStoreCommunity(t, e, owner, "none")
	// 2. a NULL pointer.
	null, _ := seedStoreCommunity(t, e, owner, "null")
	if err := e.Repo.SetCommunityAFK(t.Context(), null, store.CommunityAFK{TimeoutSeconds: 60}); err != nil {
		t.Fatal(err)
	}
	// 3. a web3.8 pick: lounge, timeout 900, two overwrites and a live call.
	legacy, everyone := seedStoreCommunity(t, e, owner, "legacy")
	if err := e.Repo.PutMember(t.Context(), store.MemberOfCommunityRow{CommunityID: legacy, UserID: member, Joined: 1}); err != nil {
		t.Fatal(err)
	}
	lounge := seedVoice(t, e, legacy, "lounge")
	overwrites := []store.OverwriteRow{
		{ChannelID: lounge, TargetKind: 0, TargetID: everyone, Deny: uint64(api.PermVideo)},
		{ChannelID: lounge, TargetKind: 1, TargetID: member, Allow: uint64(api.PermPinMessages)},
	}
	for _, o := range overwrites {
		if err := e.Repo.PutOverwrite(t.Context(), o); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.Repo.SetCommunityAFK(t.Context(), legacy, store.CommunityAFK{ChannelID: &lounge, TimeoutSeconds: 900}); err != nil {
		t.Fatal(err)
	}
	group := seedCallGroup(t, e, lounge, legacy, callGroupEpoch)
	seedLeaf(t, e, group, ownerDev, 3, nil)
	room := lounge.String() + "-1-abcdef01"
	if err := e.Repo.PutVoiceSession(t.Context(), store.VoiceSessionRow{CallID: lounge, ChannelID: lounge, GroupID: &group,
		LivekitRoom: room, Started: e.Clk.Now().Unix()}); err != nil {
		t.Fatal(err)
	}
	// 4. already built in: a community created through the route.
	builtin := createCommunity(t, e, ownerTok)
	builtinAFK, _ := afkPointer(t, e, builtin)

	created, err := api.EnsureAFKChannels(t.Context(), e.Repo, e.DS, e.Clk, log)
	if err != nil || created != 3 {
		t.Fatalf("EnsureAFKChannels = %d, %v; want 3, nil", created, err)
	}
	for _, tc := range []struct {
		name    string
		cid     id.ID
		timeout uint64
	}{{"no row", none, 300}, {"null pointer", null, 60}, {"web3.8 pick", legacy, 900}} {
		afks := voiceNamed(t, e, tc.cid, "AFK")
		if len(afks) != 1 {
			t.Fatalf("%s: %d AFK channels, want 1", tc.name, len(afks))
		}
		ptr, timeout := afkPointer(t, e, tc.cid)
		if ptr != afks[0].ID || timeout != tc.timeout || afks[0].Position != api.AFKBuiltinPosition {
			t.Fatalf("%s: pointer %s %d, want %s %d", tc.name, ptr, timeout, afks[0].ID, tc.timeout)
		}
		if !builtinAudit(t, e, ptr) {
			t.Fatalf("%s: no built-in audit row", tc.name)
		}
	}
	// The web3.8 pick is untouched and ordinary: its name, overwrites and live call stay, and it is
	// joinable (no "AFK is a silent channel").
	row, err := e.Repo.GetChannel(t.Context(), lounge)
	if err != nil || row.Name != "lounge" {
		t.Fatalf("lounge = %+v, %v", row, err)
	}
	got, err := e.Repo.ListOverwrites(t.Context(), lounge)
	if err != nil || len(got) != 2 {
		t.Fatalf("lounge overwrites = %v, %v", got, err)
	}
	if vs, err := e.Repo.GetVoiceSession(t.Context(), lounge); err != nil || vs.Ended != nil || vs.LivekitRoom != room {
		t.Fatalf("lounge call = %+v, %v", vs, err)
	}
	if status, body := e.Do(http.MethodPost, "/v1/channels/"+lounge.String()+"/calls", ownerTok, []any{}); status != http.StatusOK {
		t.Fatalf("join lounge = %d (%x), want 200", status, body)
	}
	if !stub.lastMint().Perm.GetCanPublish() {
		t.Fatal("lounge is ordinary again, yet its token is listen-only")
	}
	// The already built-in community is unchanged.
	if ptr, _ := afkPointer(t, e, builtin); ptr != builtinAFK || len(voiceNamed(t, e, builtin, "AFK")) != 1 {
		t.Fatalf("built-in community changed: pointer %s, want %s", ptr, builtinAFK)
	}
}

func TestEnsureAFKChannelsIsIdempotent(t *testing.T) {
	e, _, ownerTok := channelEnv(t)
	owner := e.sess[ownerTok].UserID
	log := slog.New(slog.DiscardHandler)
	a, _ := seedStoreCommunity(t, e, owner, "a")
	b, _ := seedStoreCommunity(t, e, owner, "b")
	pick := seedVoice(t, e, b, "pick")
	if err := e.Repo.SetCommunityAFK(t.Context(), b, store.CommunityAFK{ChannelID: &pick, TimeoutSeconds: 60}); err != nil {
		t.Fatal(err)
	}
	if n, err := api.EnsureAFKChannels(t.Context(), e.Repo, e.DS, e.Clk, log); err != nil || n != 2 {
		t.Fatalf("first run = %d, %v; want 2", n, err)
	}
	count := func() int {
		n := 0
		for _, c := range []id.ID{a, b} {
			all, err := e.Repo.ListChannels(t.Context(), c)
			if err != nil {
				t.Fatal(err)
			}
			n += len(all)
		}
		return n
	}
	before := count()
	pa, _ := afkPointer(t, e, a)
	pb, _ := afkPointer(t, e, b)
	if n, err := api.EnsureAFKChannels(t.Context(), e.Repo, e.DS, e.Clk, log); err != nil || n != 0 {
		t.Fatalf("second run = %d, %v; want 0", n, err)
	}
	if after := count(); after != before {
		t.Fatalf("channels %d after the second run, want %d", after, before)
	}
	if qa, _ := afkPointer(t, e, a); qa != pa {
		t.Fatal("the second run moved a pointer")
	}
	if qb, _ := afkPointer(t, e, b); qb != pb {
		t.Fatal("the second run moved a pointer")
	}
}

// failCreateRepo fails CreateChannel for one community, inside a transaction too.
type failCreateRepo struct {
	store.Repository
	bad id.ID
}

func (f failCreateRepo) CreateChannel(ctx context.Context, c store.ChannelRow) error {
	if c.CommunityID != nil && *c.CommunityID == f.bad {
		return errors.New("injected CreateChannel failure")
	}
	return f.Repository.CreateChannel(ctx, c)
}

func (f failCreateRepo) Tx(ctx context.Context, fn func(store.Repository) error) error {
	return f.Repository.Tx(ctx, func(tx store.Repository) error { return fn(failCreateRepo{tx, f.bad}) })
}

func TestEnsureAFKChannelsSkipsAFailure(t *testing.T) {
	e, _, ownerTok := channelEnv(t)
	owner := e.sess[ownerTok].UserID
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	a, _ := seedStoreCommunity(t, e, owner, "a")
	bad, _ := seedStoreCommunity(t, e, owner, "bad")
	c, _ := seedStoreCommunity(t, e, owner, "c")
	n, err := api.EnsureAFKChannels(t.Context(), failCreateRepo{e.Repo, bad}, e.DS, e.Clk, log)
	if err != nil || n != 2 {
		t.Fatalf("EnsureAFKChannels = %d, %v; want 2, nil", n, err)
	}
	for _, ok := range []id.ID{a, c} {
		if len(voiceNamed(t, e, ok, "AFK")) != 1 {
			t.Fatalf("community %s has no AFK", ok)
		}
	}
	if len(voiceNamed(t, e, bad, "AFK")) != 0 {
		t.Fatal("the failing community got an AFK channel")
	}
	if cfg, err := e.Repo.GetCommunityAFK(t.Context(), bad); err != nil || cfg.ChannelID != nil {
		t.Fatalf("the failing community's pointer = %+v, %v; want none (rolled back)", cfg, err)
	}
	if !strings.Contains(logs.String(), "afk backfill") || !strings.Contains(logs.String(), bad.String()) {
		t.Fatalf("the failure was not logged: %s", logs.String())
	}
}

func TestEnsureAFKMaterialisesEveryMember(t *testing.T) {
	e, _, ownerTok := channelEnv(t)
	owner := e.sess[ownerTok].UserID
	cid, everyone := seedStoreCommunity(t, e, owner, "closed")
	member, _ := e.NewUser("member")
	if err := e.Repo.PutMember(t.Context(), store.MemberOfCommunityRow{CommunityID: cid, UserID: member, Joined: 1}); err != nil {
		t.Fatal(err)
	}
	// The member's only role denies view_channel community-wide.
	if err := e.Repo.PutRole(t.Context(), store.RoleRow{ID: everyone, CommunityID: cid, Name: "@everyone",
		Allow: uint64(api.DefaultEveryoneAllow &^ api.PermViewChannel), Deny: uint64(api.PermViewChannel), Created: 1}); err != nil {
		t.Fatal(err)
	}
	if n, err := api.EnsureAFKChannels(t.Context(), e.Repo, e.DS, e.Clk, slog.New(slog.DiscardHandler)); err != nil || n != 1 {
		t.Fatalf("EnsureAFKChannels = %d, %v", n, err)
	}
	afk, _ := afkPointer(t, e, cid)
	members, err := e.Repo.ListChannelMembers(t.Context(), afk)
	if err != nil || !slices.Contains(members, member) || !slices.Contains(members, owner) {
		t.Fatalf("AFK channel members = %v, %v; want the owner and the member", members, err)
	}
}

func TestSnapshotEnsuresAMissingAFK(t *testing.T) {
	e, cid, tok := channelEnv(t)
	api.NewCalls(e.Repo, api.NewResolver(e.Repo), &stubSFU{}, api.CallsConfig{LiveKitURL: testLiveKitURL}, e.Clk,
		slog.New(slog.DiscardHandler)).Register(e.Mux)
	old, _ := afkPointer(t, e, cid)
	if err := e.Repo.SetCommunityAFK(t.Context(), cid, store.CommunityAFK{TimeoutSeconds: 900}); err != nil {
		t.Fatal(err)
	}
	ch, timeout, retired, pending := afkSnapshotOf(t, e, cid, tok)
	if ch == nil || *ch == old || timeout != 900 || retired != 0 || pending != 0 {
		t.Fatalf("snapshot = %v %d %d %d, want a new AFK id, 900, [], 0", ch, timeout, retired, pending)
	}
	ptr, _ := afkPointer(t, e, cid)
	if ptr != *ch || !builtinAudit(t, e, ptr) {
		t.Fatalf("pointer %s, snapshot %s", ptr, *ch)
	}
}

// withoutBuiltinAFK drops the built-in AFK channel every community now has (R1), for the tests that
// count the channels they made themselves.
func withoutBuiltinAFK(rows []store.ChannelRow) []store.ChannelRow {
	var out []store.ChannelRow
	for _, r := range rows {
		if r.Kind == api.ChannelVoice && r.Name == "AFK" && r.Position == api.AFKBuiltinPosition {
			continue
		}
		out = append(out, r)
	}
	return out
}

// listenOnly is sfu.PublishGrant(false, false, false): subscribe, publish nothing.
func listenOnly(p *livekit.ParticipantPermission) bool {
	return p != nil && p.GetCanSubscribe() && !p.GetCanPublish() && len(p.GetCanPublishSources()) == 0
}

// hasMic is a grant that publishes the microphone.
func hasMic(p *livekit.ParticipantPermission) bool {
	return p != nil && p.GetCanPublish() && slices.Contains(p.GetCanPublishSources(), livekit.TrackSource_MICROPHONE)
}

// The backfill on both engines (Review Focus 3): DILLA_TEST_PG adds a migrated Postgres repository.
// Every case is seeded through the store; the second run creates nothing.
func TestEnsureAFKChannelsOnBothEngines(t *testing.T) {
	for engine, repo := range policyEngines(t) {
		t.Run(engine, func(t *testing.T) {
			ctx := t.Context()
			log := slog.New(slog.DiscardHandler)
			clk := newEnv(t).Clk
			owner, member := id.New(), id.New()
			for _, u := range []id.ID{owner, member} {
				if err := repo.CreateUser(ctx, newAPITestUser(u, "u", policyT0)); err != nil {
					t.Fatalf("CreateUser: %v", err)
				}
			}
			seed := func() (id.ID, id.ID) {
				cid, everyone := id.New(), id.New()
				if err := repo.CreateCommunity(ctx, store.CommunityRow{ID: cid, Owner: owner, Name: "c",
					PolicyJSON: []byte("{}"), PolicyVersion: 1, Created: policyT0}); err != nil {
					t.Fatalf("CreateCommunity: %v", err)
				}
				for _, u := range []id.ID{owner, member} {
					if err := repo.PutMember(ctx, store.MemberOfCommunityRow{CommunityID: cid, UserID: u, Joined: policyT0}); err != nil {
						t.Fatalf("PutMember: %v", err)
					}
				}
				if err := repo.PutRole(ctx, store.RoleRow{ID: everyone, CommunityID: cid, Name: "@everyone",
					Allow: uint64(api.DefaultEveryoneAllow), Created: policyT0}); err != nil {
					t.Fatalf("PutRole: %v", err)
				}
				return cid, everyone
			}
			pointer := func(cid id.ID) store.CommunityAFK {
				cfg, err := repo.GetCommunityAFK(ctx, cid)
				if err != nil {
					t.Fatalf("GetCommunityAFK: %v", err)
				}
				return cfg
			}
			// Already built in: a community the first run handles, before the cases below exist.
			builtin, _ := seed()
			if n, err := api.EnsureAFKChannels(ctx, repo, nil, clk, log); err != nil || n != 1 {
				t.Fatalf("seeding the built-in case = %d, %v", n, err)
			}
			builtinAFK := *pointer(builtin).ChannelID
			none, _ := seed()
			null, _ := seed()
			if err := repo.SetCommunityAFK(ctx, null, store.CommunityAFK{TimeoutSeconds: 60}); err != nil {
				t.Fatal(err)
			}
			legacy, everyone := seed()
			lounge := id.New()
			if err := repo.CreateChannel(ctx, store.ChannelRow{ID: lounge, CommunityID: &legacy, Kind: api.ChannelVoice,
				Name: "lounge", SettingsJSON: []byte("{}"), HostPolicyVersion: 1, Created: policyT0}); err != nil {
				t.Fatal(err)
			}
			for _, o := range []store.OverwriteRow{
				{ChannelID: lounge, TargetKind: 0, TargetID: everyone, Deny: uint64(api.PermVideo)},
				{ChannelID: lounge, TargetKind: 1, TargetID: member, Allow: uint64(api.PermPinMessages)},
			} {
				if err := repo.PutOverwrite(ctx, o); err != nil {
					t.Fatal(err)
				}
			}
			if err := repo.PutVoiceSession(ctx, store.VoiceSessionRow{CallID: lounge, ChannelID: lounge,
				LivekitRoom: lounge.String() + "-1-abcdef01", Started: policyT0}); err != nil {
				t.Fatal(err)
			}
			if err := repo.SetCommunityAFK(ctx, legacy, store.CommunityAFK{ChannelID: &lounge, TimeoutSeconds: 900}); err != nil {
				t.Fatal(err)
			}
			if n, err := api.EnsureAFKChannels(ctx, repo, nil, clk, log); err != nil || n != 3 {
				t.Fatalf("EnsureAFKChannels = %d, %v; want 3", n, err)
			}
			for _, tc := range []struct {
				cid     id.ID
				timeout uint64
			}{{none, 300}, {null, 60}, {legacy, 900}} {
				cfg := pointer(tc.cid)
				if cfg.ChannelID == nil || *cfg.ChannelID == lounge || cfg.TimeoutSeconds != tc.timeout {
					t.Fatalf("pointer of %s = %+v, want a new AFK with %d", tc.cid, cfg, tc.timeout)
				}
				ch, err := repo.GetChannel(ctx, *cfg.ChannelID)
				if err != nil || ch.Name != "AFK" || ch.Position != api.AFKBuiltinPosition {
					t.Fatalf("AFK of %s = %+v, %v", tc.cid, ch, err)
				}
				members, err := repo.ListChannelMembers(ctx, ch.ID)
				if err != nil || !slices.Contains(members, member) {
					t.Fatalf("AFK members of %s = %v, %v", tc.cid, members, err)
				}
			}
			if row, err := repo.GetChannel(ctx, lounge); err != nil || row.Name != "lounge" {
				t.Fatalf("lounge = %+v, %v", row, err)
			}
			if ow, err := repo.ListOverwrites(ctx, lounge); err != nil || len(ow) != 2 {
				t.Fatalf("lounge overwrites = %v, %v", ow, err)
			}
			if vs, err := repo.GetVoiceSession(ctx, lounge); err != nil || vs.Ended != nil {
				t.Fatalf("lounge call = %+v, %v", vs, err)
			}
			if cfg := pointer(builtin); *cfg.ChannelID != builtinAFK {
				t.Fatal("the built-in community's pointer moved")
			}
			if n, err := api.EnsureAFKChannels(ctx, repo, nil, clk, log); err != nil || n != 0 {
				t.Fatalf("second run = %d, %v; want 0", n, err)
			}
		})
	}
}
