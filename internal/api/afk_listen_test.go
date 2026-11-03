package api_test

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/fxamacker/cbor/v2"
	"github.com/livekit/protocol/livekit"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

// listenEnv is a community with its built-in AFK channel and an ordinary voice channel lounge, each
// with a call group the owner and a member are leaves of, the call, community, channel and role
// routes over one stub SFU, and the role routes wired to re-push live grants.
type listenEnv struct {
	e                   *env
	stub                *stubSFU
	calls               *api.Calls
	cid, afk, lounge    id.ID
	afkGroup, lgGroup   id.ID
	ownerTok, memberTok string
	member, memberDev   id.ID
	everyone            id.ID
}

func newListenEnv(t *testing.T, repo func(store.Repository) store.Repository) *listenEnv {
	t.Helper()
	e := newEnv(t)
	log := slog.New(slog.DiscardHandler)
	stub := &stubSFU{}
	callsRepo := e.Repo
	if repo != nil {
		callsRepo = repo(e.Repo)
	}
	calls := api.NewCalls(callsRepo, api.NewResolver(e.Repo), stub,
		api.CallsConfig{LiveKitURL: testLiveKitURL, MaxPublishers: 10}, e.Clk, log)
	calls.Register(e.Mux)
	api.NewCommunities(e.Repo, e.DS, e.Clk, log).WithCalls(calls).Register(e.Mux)
	channels := api.NewChannels(e.Repo, e.DS, e.Clk, 10, log).WithCalls(calls)
	channels.Register(e.Mux)
	channels.RegisterMembers(e.Mux)
	api.NewRoles(e.Repo, e.Clk, "dilla.example", log).WithDS(e.DS).WithCalls(stub, calls).Register(e.Mux)
	_, ownerTok := e.NewUser("owner")
	cid := createCommunity(t, e, ownerTok)
	afk, _ := afkPointer(t, e, cid)
	lounge, _, status := newChannel(t, e, cid, ownerTok, uint64(api.ChannelVoice), 0, 0, "lounge")
	if status != http.StatusCreated {
		t.Fatalf("lounge = %d", status)
	}
	member, memberTok := e.NewUser("member")
	joinCommunity(t, e, cid, memberTok)
	memberDev := deviceOf(t, e, memberTok)
	afkGroup := seedCallGroup(t, e, afk, cid, callGroupEpoch)
	lgGroup := seedCallGroup(t, e, lounge, cid, callGroupEpoch)
	for _, g := range []id.ID{afkGroup, lgGroup} {
		seedLeaf(t, e, g, deviceOf(t, e, ownerTok), 3, nil)
		seedLeaf(t, e, g, memberDev, 3, nil)
	}
	roles, err := e.Repo.ListRoles(t.Context(), cid)
	if err != nil || len(roles) != 1 {
		t.Fatalf("ListRoles = %v, %v", roles, err)
	}
	return &listenEnv{e: e, stub: stub, calls: calls, cid: cid, afk: afk, lounge: lounge, afkGroup: afkGroup,
		lgGroup: lgGroup, ownerTok: ownerTok, memberTok: memberTok, member: member, memberDev: memberDev,
		everyone: roles[0].ID}
}

// setEveryone rewrites @everyone's allow.
func (l *listenEnv) setEveryone(t *testing.T, allow api.Bits) {
	t.Helper()
	if err := l.e.Repo.PutRole(t.Context(), store.RoleRow{ID: l.everyone, CommunityID: l.cid, Name: "@everyone",
		Allow: uint64(allow), Created: 1}); err != nil {
		t.Fatal(err)
	}
}

// join starts or joins ch's call as tok and answers the call id and the room the token names.
func (l *listenEnv) join(t *testing.T, ch id.ID, tok string) (id.ID, mint) {
	t.Helper()
	status, body := l.e.Do(http.MethodPost, "/v1/channels/"+ch.String()+"/calls", tok, []any{})
	if status != http.StatusCreated && status != http.StatusOK {
		t.Fatalf("join %s = %d (%x)", ch, status, body)
	}
	var out []cbor.RawMessage
	mustUnmarshal(t, body, &out)
	var call id.ID
	mustUnmarshal(t, out[0], &call)
	return call, l.stub.lastMint()
}

func TestAFKViewAndConnectForEveryMember(t *testing.T) {
	l := newListenEnv(t, nil)
	// The member's only role denies view and connect community-wide, and allows nothing.
	if err := l.e.Repo.PutRole(t.Context(), store.RoleRow{ID: l.everyone, CommunityID: l.cid, Name: "@everyone",
		Deny: uint64(api.PermViewChannel | api.PermConnect), Created: 1}); err != nil {
		t.Fatal(err)
	}
	res := api.NewResolver(l.e.Repo)
	for _, tc := range []struct {
		name string
		ch   id.ID
		want api.Bits
	}{{"AFK", l.afk, api.PermViewChannel | api.PermConnect}, {"lounge (control)", l.lounge, 0}} {
		row, err := l.e.Repo.GetChannel(t.Context(), tc.ch)
		if err != nil {
			t.Fatal(err)
		}
		got, err := res.Resolve(t.Context(), l.member, row)
		if err != nil || got != tc.want {
			t.Fatalf("Resolve(member, %s) = %#x, %v; want %#x", tc.name, uint64(got), err, uint64(tc.want))
		}
	}
	// A non-member holds nothing in AFK; the owner keeps everything.
	stranger, _ := l.e.NewUser("stranger")
	row, _ := l.e.Repo.GetChannel(t.Context(), l.afk)
	if got, err := res.Resolve(t.Context(), stranger, row); err != nil || got != 0 {
		t.Fatalf("Resolve(stranger, AFK) = %#x, %v; want 0", uint64(got), err)
	}
	owner := l.e.sess[l.ownerTok].UserID
	if got, err := res.Resolve(t.Context(), owner, row); err != nil || got != api.PermAll {
		t.Fatalf("Resolve(owner, AFK) = %#x, %v; want PermAll", uint64(got), err)
	}
}

// legacyCall writes a live call in ch on group, as a web3.10 instance left one before the AFK
// channel stopped hosting calls (AW2, R-AW1), and answers its id and room. Only such a call can be
// live in the AFK channel now; the listen-only grant stays the backstop for it.
func (l *listenEnv) legacyCall(t *testing.T, ch, group id.ID) (id.ID, string) {
	t.Helper()
	call := id.New()
	room := call.String() + "-1-abcdef01"
	if err := l.e.Repo.PutVoiceSession(t.Context(), store.VoiceSessionRow{CallID: call, ChannelID: ch, GroupID: &group,
		LivekitRoom: room, Started: l.e.Clk.Now().Unix()}); err != nil {
		t.Fatal(err)
	}
	return call, room
}

// R-AW1: a start in the AFK channel mints no token at all; the lounge (control) mints the microphone.
func TestAFKStartMintsNoToken(t *testing.T) {
	l := newListenEnv(t, nil)
	status, body := l.e.Do(http.MethodPost, "/v1/channels/"+l.afk.String()+"/calls", l.memberTok, []any{})
	if status != http.StatusForbidden || l.e.ErrCode(body) != "E_FORBIDDEN" || len(l.stub.minted()) != 0 {
		t.Fatalf("start in AFK = %d (%x), %d tokens; want 403 E_FORBIDDEN and none", status, body, len(l.stub.minted()))
	}
	if _, m := l.join(t, l.lounge, l.memberTok); !hasMic(m.Perm) {
		t.Fatalf("lounge token grant = %+v, want the microphone (control)", m.Perm)
	}
}

// R-AW1: the /rtc gate admits nobody to a call left live in the AFK channel.
func TestAFKAdmitIsRefused(t *testing.T) {
	l := newListenEnv(t, nil)
	_, afkRoom := l.legacyCall(t, l.afk, l.afkGroup)
	_, lgMint := l.join(t, l.lounge, l.memberTok)
	if perm, err := l.calls.AdmitRoom(t.Context(), afkRoom, l.memberDev); codeOf(err) != "E_FORBIDDEN" {
		t.Fatalf("AdmitRoom(AFK) = %+v, %v; want E_FORBIDDEN", perm, err)
	}
	perm, err := l.calls.AdmitRoom(t.Context(), lgMint.Room, l.memberDev)
	if err != nil || !hasMic(perm) {
		t.Fatalf("AdmitRoom(lounge) = %+v, %v; want the microphone (control)", perm, err)
	}
}

func TestAFKRolePushStaysListenOnly(t *testing.T) {
	for _, tc := range []struct {
		name   string
		room   func(t *testing.T, l *listenEnv) string
		wantOK func(*livekit.ParticipantPermission) bool
	}{
		// A call left live in the AFK channel (R-AW1): the re-push keeps the backstop's listen-only.
		{"AFK", func(t *testing.T, l *listenEnv) string { _, room := l.legacyCall(t, l.afk, l.afkGroup); return room }, listenOnly},
		{"lounge (control)", func(t *testing.T, l *listenEnv) string { _, m := l.join(t, l.lounge, l.memberTok); return m.Room }, hasMic},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := newListenEnv(t, nil)
			l.setEveryone(t, api.DefaultEveryoneAllow&^api.PermSpeak)
			m := mint{Room: tc.room(t, l)}
			l.stub.setPresent(m.Room, &livekit.ParticipantInfo{Identity: l.memberDev.String()})
			speaker := createRole(t, l.e, l.cid, l.ownerTok, "speaker", 3, api.PermSpeak)
			grant(t, l.e, l.cid, l.ownerTok, l.member, speaker, http.StatusNoContent)
			l.calls.ProcessQueueForTest(t.Context())
			var pushed *livekit.ParticipantPermission
			for _, u := range l.stub.permUpdates() {
				if u.Room == m.Room && u.Identity == l.memberDev.String() {
					pushed = u.Perm
				}
			}
			if pushed == nil || !tc.wantOK(pushed) {
				t.Fatalf("re-push after the speak grant = %+v", pushed)
			}
		})
	}
}

func TestAFKShareIsRefused(t *testing.T) {
	l := newListenEnv(t, nil)
	afkCall, afkRoom := l.legacyCall(t, l.afk, l.afkGroup)
	afkMint := mint{Room: afkRoom}
	l.stub.setPresent(afkMint.Room, &livekit.ParticipantInfo{Identity: l.memberDev.String()})
	status, body := l.e.Do(http.MethodPost, "/v1/calls/"+afkCall.String()+"/share", l.memberTok, []any{})
	if status != http.StatusForbidden || l.e.ErrCode(body) != "E_FORBIDDEN" {
		t.Fatalf("share in AFK = %d (%x), want 403 E_FORBIDDEN", status, body)
	}
	for _, u := range l.stub.permUpdates() {
		if u.Room == afkMint.Room {
			t.Fatalf("a refused share pushed %+v", u)
		}
	}
	lgCall, lgMint := l.join(t, l.lounge, l.memberTok)
	l.stub.setPresent(lgMint.Room, &livekit.ParticipantInfo{Identity: l.memberDev.String()})
	if status, body := l.e.Do(http.MethodPost, "/v1/calls/"+lgCall.String()+"/share", l.memberTok, []any{}); status != http.StatusNoContent {
		t.Fatalf("share in lounge = %d (%x), want 204 (control)", status, body)
	}
}

// afkErrRepo fails GetCommunityAFK while fail is set.
type afkErrRepo struct {
	store.Repository
	fail *atomic.Bool
}

func (r afkErrRepo) GetCommunityAFK(ctx context.Context, cid id.ID) (store.CommunityAFK, error) {
	if r.fail.Load() {
		return store.CommunityAFK{}, errors.New("injected GetCommunityAFK failure")
	}
	return r.Repository.GetCommunityAFK(ctx, cid)
}

func TestAFKGrantFailsClosed(t *testing.T) {
	fail := new(atomic.Bool)
	l := newListenEnv(t, func(r store.Repository) store.Repository { return afkErrRepo{r, fail} })
	if _, m := l.join(t, l.lounge, l.memberTok); !hasMic(m.Perm) {
		t.Fatalf("lounge grant with the pointer readable = %+v, want the microphone (control)", m.Perm)
	}
	fail.Store(true)
	_, m := l.join(t, l.lounge, l.memberTok)
	if !listenOnly(m.Perm) {
		t.Fatalf("lounge grant when the pointer cannot be read = %+v, want listen-only", m.Perm)
	}
	if perm, err := l.calls.AdmitRoom(t.Context(), m.Room, l.memberDev); err != nil || !listenOnly(perm) {
		t.Fatalf("AdmitRoom when the pointer cannot be read = %+v, %v; want listen-only", perm, err)
	}
}
