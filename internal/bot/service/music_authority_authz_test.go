package service

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/bot/client"
	"github.com/dilla-chat/dilla/internal/bot/intake"
	"github.com/dilla-chat/dilla/internal/bot/music"
	"github.com/dilla-chat/dilla/internal/bot/music/persistence"
	"github.com/dilla-chat/dilla/internal/bot/native"
	"github.com/dilla-chat/dilla/internal/bot/protocol"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

// Bot v2 Task 19 fix round 1 (I1, I3): with the feed-state Authz the music authority makes no permission read; the
// only request is the call read, once per command, shared by CurrentCall, the in-call check and the recheck.

type countingCallSession struct {
	mu          sync.Mutex
	call        client.ChannelCallState
	callReads   int
	permissions int
}

func (s *countingCallSession) ChannelCall(context.Context, id.ID) (client.ChannelCallState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.callReads++
	return s.call, nil
}
func (s *countingCallSession) BotActorAuthority(context.Context, id.ID, id.ID) (client.ActorAuthority, bool, error) {
	s.mu.Lock()
	s.permissions++
	s.mu.Unlock()
	return client.ActorAuthority{}, false, nil
}
func (s *countingCallSession) ChannelPermissions(context.Context, id.ID) (uint64, error) {
	s.mu.Lock()
	s.permissions++
	s.mu.Unlock()
	return 0, nil
}
func (s *countingCallSession) BotActorChannelAuthority(context.Context, id.ID, id.ID, id.ID) (uint64, bool, error) {
	s.mu.Lock()
	s.permissions++
	s.mu.Unlock()
	return 0, false, nil
}

type authzRig struct {
	authz                                 *intake.Authz
	community, channel, bot, dj, listener id.ID
	djRole                                id.ID
	session                               *countingCallSession
	authority                             *CommunityMusicInteractionAuthority
}

func newAuthzRig(t *testing.T) *authzRig {
	t.Helper()
	r := &authzRig{community: id.New(), channel: id.New(), bot: id.New(), dj: id.New(), listener: id.New(), djRole: id.New()}
	r.authz = intake.NewAuthz(r.community)
	everyone := id.New()
	r.authz.SnapshotBegin()
	r.authz.Role(everyone, intake.PermViewChannel|1<<7|1<<8, 0, 0, false, true)
	r.authz.Role(r.djRole, intake.PermControlMusic, 0, 1, false, true)
	r.authz.Member(r.bot, true, nil, true)
	r.authz.Member(r.dj, true, []id.ID{r.djRole}, true)
	r.authz.Member(r.listener, true, nil, true)
	r.authz.ChannelOverwrites(r.channel, nil, false, true)
	r.authz.SnapshotDone()
	callID := r.channel
	r.session = &countingCallSession{call: client.ChannelCallState{CallID: &callID, GroupID: ptrID(id.New()),
		Participants: []client.CallParticipant{{UserID: r.dj}, {UserID: r.listener}}}}
	installation := id.New()
	r.authority = &CommunityMusicInteractionAuthority{Session: r.session,
		Store:          &musicAuthorityStoreFake{installation: store.BotInstallationRow{ID: installation, CommunityID: r.community, State: store.BotInstallationReady, Capabilities: musicBotCapability}},
		InstallationID: installation, CommunityID: r.community, Authz: r.authz, BotUser: r.bot}
	return r
}

func TestMusicAuthorityResolvesFromTheFeedStateWithOneCallRead(t *testing.T) {
	r := newAuthzRig(t)
	ctx := withMusicCallMemo(t.Context())
	call, err := r.authority.CurrentCall(ctx, r.channel)
	if err != nil || !call.Active {
		t.Fatalf("current call = %+v, %v", call, err)
	}
	if ok, err := r.authority.CanUse(ctx, InteractionActor{User: r.listener}, call); !ok || err != nil {
		t.Fatalf("listener CanUse = %v, %v", ok, err)
	}
	if ok, _ := r.authority.CanDJ(ctx, InteractionActor{User: r.listener}, call); ok {
		t.Fatal("a member without control_music is a DJ")
	}
	if ok, err := r.authority.CanDJ(ctx, InteractionActor{User: r.dj}, call); !ok || err != nil {
		t.Fatalf("DJ CanDJ = %v, %v", ok, err)
	}
	if again, _ := r.authority.CurrentCall(ctx, r.channel); again != call {
		t.Fatal("the recheck saw another call")
	}
	if r.session.callReads != 1 || r.session.permissions != 0 {
		t.Fatalf("call reads=%d permission reads=%d, want 1 and 0", r.session.callReads, r.session.permissions)
	}
}

func TestMusicAuthorityFollowsTheFeedState(t *testing.T) {
	t.Run("a role removed from the feed is no longer a DJ", func(t *testing.T) {
		r := newAuthzRig(t)
		ctx := withMusicCallMemo(t.Context())
		call, _ := r.authority.CurrentCall(ctx, r.channel)
		r.authz.Member(r.dj, true, nil, false)
		if ok, _ := r.authority.CanDJ(ctx, InteractionActor{User: r.dj}, call); ok {
			t.Fatal("the DJ check ignored the role removal")
		}
	})
	t.Run("a voice channel overwrite denying the Bot speak", func(t *testing.T) {
		r := newAuthzRig(t)
		ctx := withMusicCallMemo(t.Context())
		call, _ := r.authority.CurrentCall(ctx, r.channel)
		r.authz.ChannelOverwrites(r.channel, []intake.Overwrite{{Kind: intake.OverwriteMember, Target: r.bot, Deny: 1 << 8}}, false, false)
		if ok, _ := r.authority.CanDJ(ctx, InteractionActor{User: r.dj}, call); ok || r.authority.BotCanSpeak(r.channel) {
			t.Fatal("the Bot plays where it may not speak")
		}
	})
	t.Run("a timed-out member", func(t *testing.T) {
		r := newAuthzRig(t)
		ctx := withMusicCallMemo(t.Context())
		call, _ := r.authority.CurrentCall(ctx, r.channel)
		r.authz.MemberTimeout(r.dj, uint64(time.Now().Add(time.Hour).Unix()), false)
		if ok, _ := r.authority.CanDJ(ctx, InteractionActor{User: r.dj}, call); ok {
			t.Fatal("a timed-out member is a DJ")
		}
	})
	t.Run("not ready before the first snapshot", func(t *testing.T) {
		r := newAuthzRig(t)
		r.authority.Authz = intake.NewAuthz(r.community)
		ctx := withMusicCallMemo(t.Context())
		call, _ := r.authority.CurrentCall(ctx, r.channel)
		if _, err := r.authority.CanUse(ctx, InteractionActor{User: r.dj}, call); err == nil {
			t.Fatal("a check passed before the feed state was ready")
		}
	})
}

// A whole /play with the feed-state authority: no permission read; the call is read for the check and read again for
// the recheck before the commit (fix round 2).
func TestSealedPlayMakesNoPermissionReadAndRereadsTheCallBeforeTheCommit(t *testing.T) {
	r := newAuthzRig(t)
	if got := sealedPlayStatus(t, r, staticMusicResolver{}); got != intake.StatusOK {
		t.Fatalf("/play = %d", got)
	}
	if r.session.callReads != 2 || r.session.permissions != 0 {
		t.Fatalf("call reads=%d permission reads=%d, want 2 (check and recheck) and 0", r.session.callReads, r.session.permissions)
	}
}

// An actor who leaves the call while the track resolves cannot commit it: the recheck reads the call again.
func TestSealedPlayByAnActorWhoLeftDuringTheResolveIsRefused(t *testing.T) {
	r := newAuthzRig(t)
	leave := func() {
		r.session.mu.Lock()
		r.session.call.Participants = []client.CallParticipant{{UserID: r.listener}}
		r.session.mu.Unlock()
	}
	if got := sealedPlayStatus(t, r, leavingResolver{leave: leave}); got != 403 {
		t.Fatalf("/play by an actor who left = %d, want 403", got)
	}
}

func sealedPlayStatus(t *testing.T, r *authzRig, resolver MusicTrackResolver) uint16 {
	t.Helper()
	n := &playlistAcceptanceNative{binding: native.Binding{Instance: id.New(), Community: r.community, Installation: r.authority.InstallationID}, rows: map[string]native.StateValue{}}
	stateStore, err := persistence.New(n, r.community)
	if err != nil {
		t.Fatal(err)
	}
	h := MusicInteractions{Store: stateStore, Authority: r.authority, InstallationID: r.authority.InstallationID, CommunityID: r.community, Resolver: resolver}
	d := &SealedDispatcher{Community: r.community, Handlers: h.Handlers()}
	req := protocol.SealedRequest{RequestID: id.New(), CommunityID: r.community, ChannelID: r.channel, Operation: uint64(protocol.OpMusicEnqueue),
		Args: []any{"song"}, SentAt: uint64(time.Now().Unix()), UserID: r.dj, DeviceID: id.New()}
	var last intake.Answer
	d.Dispatch(t.Context(), req, func(a intake.Answer) { last = a })
	return last.Status
}

type leavingResolver struct{ leave func() }

func (l leavingResolver) Resolve(ctx context.Context, input string) ([]music.Track, error) {
	l.leave()
	return staticMusicResolver{}.Resolve(ctx, input)
}

type staticMusicResolver struct{}

func (staticMusicResolver) Resolve(context.Context, string) ([]music.Track, error) {
	return []music.Track{{ID: "x", Provider: music.ProviderYouTube, CanonicalID: "abcdefghijk", Title: "Song", Artist: "Band", DurationMS: 1000}}, nil
}
