package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os/exec"
	"slices"
	"sync"
	"time"

	"github.com/dilla-chat/dilla/internal/bot/client"
	"github.com/dilla-chat/dilla/internal/bot/helper"
	"github.com/dilla-chat/dilla/internal/bot/music"
	"github.com/dilla-chat/dilla/internal/bot/music/encode"
	"github.com/dilla-chat/dilla/internal/bot/music/livekit"
	"github.com/dilla-chat/dilla/internal/bot/music/persistence"
	"github.com/dilla-chat/dilla/internal/bot/music/session"
	"github.com/dilla-chat/dilla/internal/bot/music/youtube"
	"github.com/dilla-chat/dilla/internal/bot/native"
	"github.com/dilla-chat/dilla/internal/bot/service"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

const (
	musicVoiceChannelKind = 1
	maxMusicSupervisors   = 512
	botMusicCapability    = 1 << 0 // protocol/10-community-bot.md: capability bit 0
)

var errMusicPCMUnavailable = errors.New("music PCM provider is unavailable")

// musicSnapshotCommitCallback hands a committed revision to the retained
// supervisor. Only an explicit Resume or a Skip that continues existing playback starts audio.
func musicSnapshotCommitCallback(registry *musicQueueRegistry, logger *slog.Logger) func(context.Context, id.ID, id.ID, id.ID, music.State, uint64, bool) (uint64, error) {
	return func(ctx context.Context, installation, community, channel id.ID, state music.State, revision uint64, startPlayback bool) (uint64, error) {
		return registry.schedulePersistedSnapshot(ctx, installation, community, channel, state, revision, startPlayback)
	}
}

type musicQueueRuntime struct {
	lifecycle   sync.Mutex
	mu          sync.Mutex
	supervisors map[id.ID]*music.Supervisor
	community   id.ID
	store       *persistence.Store
	closed      bool
}

func (r *musicQueueRuntime) Close() error {
	if r != nil {
		r.lifecycle.Lock()
		defer r.lifecycle.Unlock()
		r.mu.Lock()
		r.closed = true
		supervisors := r.supervisors
		r.supervisors = make(map[id.ID]*music.Supervisor)
		r.mu.Unlock()
		closeMusicSupervisors(supervisors)
	}
	return nil
}

func (r *musicQueueRuntime) Len() int {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.supervisors)
}

func (r *musicQueueRuntime) CurrentState(ctx context.Context, channel id.ID) (music.State, uint64, error) {
	if ctx == nil || channel.IsZero() {
		return music.State{}, 0, errors.New("music runtime dependencies unavailable")
	}
	if err := ctx.Err(); err != nil {
		return music.State{}, 0, err
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return music.State{}, 0, music.ErrSupervisorClosed
	}
	supervisor := r.supervisors[channel]
	r.mu.Unlock()
	if supervisor == nil {
		return music.State{}, 0, errors.New("music voice session is unavailable")
	}
	return supervisor.RefreshState(ctx)
}

// ApplyPersistedSnapshot hands an exact committed revision to its retained
// supervisor. Only an explicit Resume or active Skip may start playback.
func (r *musicQueueRuntime) ApplyPersistedSnapshot(ctx context.Context, channel id.ID, committed music.State, expectedRevision uint64, startPlayback bool) (uint64, error) {
	if ctx == nil || channel.IsZero() {
		return 0, errors.New("music runtime dependencies unavailable")
	}
	r.mu.Lock()
	if r.closed || r.store == nil {
		r.mu.Unlock()
		return 0, music.ErrSupervisorClosed
	}
	current := r.supervisors[channel]
	if current == nil {
		r.mu.Unlock()
		return 0, errors.New("music voice session is unavailable")
	}
	r.mu.Unlock()
	return current.ApplyPersistedSnapshot(ctx, committed, expectedRevision, startPlayback)
}

type musicCallStarter interface {
	StartCall(context.Context, id.ID) (client.Call, error)
}

// callRTTReporter is what reports the bot's RTT to the SFU for its call (client.Session.ReportCallRTT).
type callRTTReporter interface {
	ReportCallRTT(context.Context, id.ID, uint32) error
}

type installationMusicNative interface {
	Binding() native.Binding
	livekit.EpochSource
}

// musicRuntimeDeps contains the process-wide dependencies and the channel
// identity for one supervisor. Each supervisor gets its own queue/session
// state.
type musicRuntimeDeps struct {
	Community, Channel id.ID
	Calls              musicCallStarter
	JoinCallGroup      func(context.Context) error
	YouTubePCM         youtube.PCMSourceStarter
	Encoder            session.EncoderStarter
	Publisher          session.PublisherStarter
	Epochs             livekit.EpochSource
	Lifetime           time.Duration
	// CallGuards keep each publication under the call's current epoch and authority (Bot v2 Task 19 fix round 1).
	CallGuards livekit.CallGuards
}

// newMusicQueueStarter resolves each queue item at playback time so the call
// credentials are fresh and bound to that item's voice channel. Provider PCM
// implementations are injected so this wiring cannot silently claim that a
// metadata-only YouTube resolver provides live audio.
func newMusicQueueStarter(deps musicRuntimeDeps) (session.QueueStarter, error) {
	if deps.Community.IsZero() || deps.Channel.IsZero() || deps.Calls == nil ||
		deps.Encoder == nil || deps.Epochs == nil || deps.Lifetime <= 0 || deps.Lifetime > 30*time.Minute {
		return session.QueueStarter{}, errors.New("music runtime dependencies unavailable")
	}
	prep := &musicCallPrep{}
	publisher := deps.Publisher
	var resetConnection, closeConnection, leaveConnection func() error
	var setCallEnded func(func())
	if warmer, ok := publisher.(interface {
		Warm(context.Context, client.Call) error
	}); ok {
		prep.connect = warmer.Warm
	}
	if publisher == nil {
		pool, err := livekit.NewPool(deps.Lifetime)
		if err != nil {
			return session.QueueStarter{}, err
		}
		// Signal task 8c (Ruling AU): the bot's ping on the stage, reported through dillad like a person's.
		if r, ok := deps.Calls.(callRTTReporter); ok {
			pool.SetRTTReporter(r.ReportCallRTT)
		}
		// Bot v2 Task 19 fix round 1: every publication and every epoch change is caught up, admitted and re-keyed. A
		// call group the Bot joined moments ago (its own commit was just accepted at the head) needs no catch-up before
		// its first publication; a later commit reaches the track through its hint or the monitor tick.
		guards := deps.CallGuards
		guards.FreshJoin = prep.joinedRecently
		pool.SetCallGuards(guards)
		publisher = session.RetainedLiveKitPublisher{Pool: pool}
		// A room that ended, was reset after dying, was left idle or closed forgets the call it was prepared for (I2).
		pool.SetRoomEndedReporter(prep.forget)
		setCallEnded = func(report func()) {
			pool.SetRoomEndedReporter(func() {
				prep.forget()
				if report != nil {
					report()
				}
			})
		}
		resetConnection = func() error {
			dead := pool.Dead()
			err := pool.Reset()
			if dead {
				prep.forget()
			}
			return err
		}
		closeConnection = func() error { prep.forget(); return pool.Close() }
		leaveConnection = func() error { prep.forget(); return pool.LeaveIdle() }
		prep.connect, prep.holds, prep.leave = pool.Warm, pool.Holds, pool.LeaveIdle
	}
	prep.prepare = func(ctx context.Context) (client.Call, error) {
		var joinErr error
		if deps.JoinCallGroup != nil {
			joinErr = deps.JoinCallGroup(ctx)
		}
		if err := music.StartStage(ctx, "call_group_join", joinErr); err != nil {
			return client.Call{}, err
		}
		call, err := deps.Calls.StartCall(ctx, deps.Channel)
		if err = music.StartStage(ctx, "call_token", err); err != nil {
			return client.Call{}, err
		}
		if call.CallID.IsZero() || call.GroupID.IsZero() || call.CallID != deps.Channel || call.LiveKitURL == "" || call.Token == "" {
			return client.Call{}, music.StartStage(ctx, "call_fields", session.ErrInvalid)
		}
		_ = music.StartStage(ctx, "call_fields", nil)
		return call, nil
	}
	options := func(ctx context.Context, track music.Track) (session.Options, error) {
		if ctx == nil || track.CanonicalID == "" {
			return session.Options{}, session.ErrInvalid
		}
		if track.Provider != music.ProviderYouTube || deps.YouTubePCM == nil {
			return session.Options{}, session.ErrInvalid
		}
		pcm := session.YouTubePCM{Source: deps.YouTubePCM}
		return session.Options{
			Community: deps.Community, Channel: deps.Channel, Provider: track.Provider,
			Track: track.CanonicalID, PrepareCall: prep.take,
			PCM:     pcm,
			Encoder: deps.Encoder, Publisher: publisher, Epochs: deps.Epochs,
		}, nil
	}
	return session.QueueStarter{SetCallEnded: setCallEnded, OptionsFor: options, ResetConnection: resetConnection, CloseConnection: closeConnection, LeaveConnection: leaveConnection,
		Warm: prep.warm, OnStartFailed: func(error) { prep.forget() }, Abandon: prep.abandon}, nil
}

// musicCallPrepFresh is how long a call prepared by an opened /play waits for its start: the /play's resolve is far
// shorter. An older one is dropped and its room left (M3).
const musicCallPrepFresh = 30 * time.Second

// musicCallPrepTimeout bounds a warm join, token and room connection.
const musicCallPrepTimeout = 30 * time.Second

// errMusicNeedsPlay pauses an automatic start (the next queued track, a source restart) that has no room to play in:
// only a member's play joins a call (Bot v2 Task 19 fix round 1, I2).
var errMusicNeedsPlay = errors.New("music: the room was left; a member's play joins again")

// musicCallPrep runs one voice room's call preparation (call-group join, call token, and through Warm also the room
// connection) at most once at a time (Bot v2 Task 19). An opened /play warms it while its track resolves; the start
// that follows takes the prepared call, or waits for the warm still running, instead of joining again. The join's
// own single-writer rule holds: a warm and a start never join the same call group at once.
//
// Fix round 1 (I2): the room's call (last) is kept without its token once the room is connected (the publisher
// needs the token only to connect), and forgotten when the room ends, dies and is reset, is left idle or closed, or a
// start fails. It is handed out again only while the pool still holds the room for it; the publication then catches
// the call group up and admits its epoch (Held roster, the Bot's connect and speak, the installation) before any
// frame, so a reused call is never published into without the same checks as a fresh one.
type musicCallPrep struct {
	prepare func(context.Context) (client.Call, error)
	connect func(context.Context, client.Call) error
	// holds reports whether the room is connected for call (livekit.Pool.Holds); nil never reuses a room.
	holds func(client.Call) bool
	// leave disconnects an idle room (livekit.Pool.LeaveIdle); nil leaves nothing.
	leave func() error

	mu       sync.Mutex
	inflight chan struct{} // closed when the running preparation ends
	ready    *preparedMusicCall
	// last is the call the room was last prepared for, without its token: the next queued track plays in that room.
	last *client.Call
	// joined is when the call group of last was joined by a preparation of this room (its own commit accepted).
	joinedGroup id.ID
	joinedAt    time.Time
}

// musicJoinFresh is how long after its own join the Bot's call group is taken as current without a catch-up.
const musicJoinFresh = 2 * time.Second

// joinedRecently reports whether this room's preparation joined group within musicJoinFresh, once: only the first
// publication after the Bot's own join skips its catch-up (fix round 2, N1); every later one, and every monitor tick and
// hint, catches up.
func (p *musicCallPrep) joinedRecently(group id.ID) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	fresh := !group.IsZero() && p.joinedGroup == group && time.Since(p.joinedAt) < musicJoinFresh
	if fresh {
		p.joinedGroup = id.ID{}
	}
	return fresh
}

// prepared records a successful preparation of call (under p.mu).
func (p *musicCallPrep) preparedLocked(call client.Call) {
	p.last = withoutToken(call)
	p.joinedGroup, p.joinedAt = call.GroupID, time.Now()
}

type preparedMusicCall struct {
	call client.Call
	at   time.Time
	// connected is a warm that connected the room itself: only such a warm leaves it when nobody takes it. A warm that
	// reused a held room (a /play while the queue still holds a room, between two tracks) never leaves it (fix round 2,
	// N5).
	connected bool
}

// withoutToken is call with its LiveKit token dropped: kept only to bind a later track to the connected room.
func withoutToken(call client.Call) *client.Call {
	call.Token = ""
	return &call
}

// forget drops the prepared and the last call (and with them any token).
func (p *musicCallPrep) forget() {
	p.mu.Lock()
	p.ready, p.last, p.joinedGroup = nil, nil, id.ID{}
	p.mu.Unlock()
}

// abandon is a /play that ended without starting (refused, failed to resolve, or only queued): a call its warm
// prepared is dropped and the room, if nothing plays in it, is left (M3).
func (p *musicCallPrep) abandon() {
	p.mu.Lock()
	ready := p.ready
	p.ready = nil
	p.mu.Unlock()
	if ready != nil && ready.connected && p.leave != nil {
		_ = p.leave()
	}
}

// expire drops a prepared call nobody took within musicCallPrepFresh and leaves the room (M3).
func (p *musicCallPrep) expire(prepared *preparedMusicCall) {
	p.mu.Lock()
	untaken := p.ready == prepared
	if untaken {
		p.ready = nil
	}
	p.mu.Unlock()
	if untaken && prepared.connected && p.leave != nil {
		_ = p.leave()
	}
}

// reusable is the room's last call when the pool still holds the room for it.
func (p *musicCallPrep) reusableLocked() *client.Call {
	if p.last == nil || (p.holds != nil && !p.holds(*p.last)) {
		return nil
	}
	call := *p.last
	return &call
}

// warm starts a preparation in the background unless one runs or a fresh one waits. It never blocks. A /play into
// the room the Bot is still connected to, for the call group the /play found current (music.PlaybackGroup of ctx),
// prepares nothing: the room's call is used again (a repeated track); its publication catches up and admits first.
func (p *musicCallPrep) warm(ctx context.Context) {
	group := music.PlaybackGroup(ctx)
	p.mu.Lock()
	if p.inflight != nil || (p.ready != nil && time.Since(p.ready.at) < musicCallPrepFresh) {
		p.mu.Unlock()
		return
	}
	if last := p.reusableLocked(); last != nil && !group.IsZero() && last.GroupID == group {
		prepared := &preparedMusicCall{call: *last, at: time.Now()}
		p.ready = prepared
		p.mu.Unlock()
		time.AfterFunc(musicCallPrepFresh, func() { p.expire(prepared) })
		return
	}
	done := make(chan struct{})
	p.inflight = done
	p.mu.Unlock()
	go func() {
		warmCtx, cancel := context.WithTimeout(client.WithImmediateControl(ctx), musicCallPrepTimeout)
		defer cancel()
		call, err := p.prepare(warmCtx)
		connected := false
		if err == nil && p.connect != nil {
			// The room connection is best effort: the start connects itself if this one failed.
			if connectErr := p.connect(warmCtx, call); connectErr != nil {
				music.PlaybackLogger(warmCtx).InfoContext(warmCtx, "bot music warm room connection failed", "class", music.FailureClass(connectErr))
			} else {
				connected = p.holds == nil || p.holds(call)
			}
		}
		var prepared *preparedMusicCall
		p.mu.Lock()
		p.inflight = nil
		if err == nil {
			// Once the room is connected the token is not needed again: it is not kept.
			if connected {
				call = *withoutToken(call)
			}
			prepared = &preparedMusicCall{call: call, at: time.Now(), connected: connected}
			p.ready = prepared
			p.preparedLocked(call)
		}
		p.mu.Unlock()
		close(done)
		if prepared != nil {
			time.AfterFunc(musicCallPrepFresh, func() { p.expire(prepared) })
		}
	}()
}

// take is the start's PrepareCall: the call a warm prepared (once, while fresh), else a preparation of its own. It
// waits for a running warm rather than overlapping it. A failed warm is prepared again here, so its error is the
// start's to report.
//
// The automatic advance to the next queued track (and a source restart) joins nothing and asks for no new call token:
// the Bot stays joined while the queue is non-empty (spec, Music), so the next track is published into the room the
// last one played in. It is handed out only while the pool still holds that room; otherwise the automatic start
// pauses (errMusicNeedsPlay), since only a member's play joins a call. The publication catches the call group up and
// admits the epoch before its first frame.
func (p *musicCallPrep) take(ctx context.Context) (client.Call, error) {
	if !session.ExplicitStart(ctx) {
		p.mu.Lock()
		last := p.reusableLocked()
		p.mu.Unlock()
		if last == nil {
			return client.Call{}, music.StartStage(ctx, "call_group_join", errMusicNeedsPlay)
		}
		_ = music.StartStage(ctx, "call_group_join", nil)
		return *last, nil
	}
	for {
		p.mu.Lock()
		if done := p.inflight; done != nil {
			p.mu.Unlock()
			select {
			case <-done:
				continue
			case <-ctx.Done():
				return client.Call{}, ctx.Err()
			}
		}
		ready := p.ready
		p.ready = nil
		if ready != nil && time.Since(ready.at) < musicCallPrepFresh && (ready.call.Token != "" || p.holds == nil || p.holds(ready.call)) {
			p.mu.Unlock()
			_ = music.StartStage(ctx, "call_group_join", nil)
			return ready.call, nil
		}
		done := make(chan struct{})
		p.inflight = done
		p.mu.Unlock()
		call, err := p.prepare(ctx)
		p.mu.Lock()
		p.inflight = nil
		if err == nil {
			p.preparedLocked(call)
		}
		p.mu.Unlock()
		close(done)
		return call, err
	}
}

func newInstallationYouTubeQueueStarter(instance, installation, community, channel id.ID, calls *client.Session, source installationMusicNative) (session.QueueStarter, error) {
	return newInstallationMusicQueueStarter(instance, installation, community, channel, calls, source, nil, livekit.CallGuards{})
}

func newInstallationMusicQueueStarter(instance, installation, community, channel id.ID, calls *client.Session, source installationMusicNative, youtubeSource youtube.PCMSourceStarter, guards livekit.CallGuards, joinCallGroup ...func(context.Context) error) (session.QueueStarter, error) {
	if instance.IsZero() || installation.IsZero() || community.IsZero() || channel.IsZero() || calls == nil || source == nil || calls.InstanceID() != instance {
		return session.QueueStarter{}, errors.New("music runtime dependencies unavailable")
	}
	binding := source.Binding()
	if binding.Instance != instance || binding.Installation != installation || binding.Community != community {
		return session.QueueStarter{}, errors.New("music native binding does not match installation")
	}
	var join func(context.Context) error
	if len(joinCallGroup) > 1 {
		return session.QueueStarter{}, errors.New("music call-group join configuration is invalid")
	}
	if len(joinCallGroup) == 1 {
		join = joinCallGroup[0]
	}
	return newMusicQueueStarter(musicRuntimeDeps{
		Community: community, Channel: channel, Calls: calls,
		JoinCallGroup: join, CallGuards: guards,
		YouTubePCM: func() youtube.PCMSourceStarter {
			if youtubeSource != nil {
				return youtubeSource
			}
			return youtubeStreamsFor(community, installation)
		}(),
		Encoder: session.RuntimeEncoder{Config: session.EncoderConfig{
			Capacity: 50, Options: encode.Options{},
		}},
		Epochs: source, Lifetime: 30 * time.Minute,
	})
}

type persistedMusicQueue struct{ store *persistence.Store }

func (p persistedMusicQueue) Load(ctx context.Context, channel id.ID) (music.State, uint64, error) {
	snapshot, err := p.store.Load(ctx, channel)
	return snapshot.State, snapshot.Revision, err
}

func (p persistedMusicQueue) Save(ctx context.Context, channel id.ID, expected uint64, state music.State) (uint64, error) {
	snapshot, err := p.store.Save(ctx, channel, expected, state)
	return snapshot.Revision, err
}

// notifyingMusicQueue is the supervisor's persisted queue that tells changed after each successful save: the playing
// track or the queue changed (Bot v2 Task 17: a call hint for the room's members).
type notifyingMusicQueue struct {
	persistedMusicQueue
	changed func(context.Context)
}

func (q notifyingMusicQueue) Save(ctx context.Context, channel id.ID, expected uint64, state music.State) (uint64, error) {
	revision, err := q.persistedMusicQueue.Save(ctx, channel, expected, state)
	if err == nil {
		q.changed(ctx)
	}
	return revision, err
}

// runtimeMusicAuthorityStore resolves installation and DJ policy from current
// bot APIs on every authorization check. It never falls back to cached roles.
type runtimeMusicAuthorityStore struct {
	runtime      *client.Runtime
	session      *client.Session
	installation id.ID
	community    id.ID
	// recent keeps the installation row until the installation's next op 54 (Bot v2 Task 19 fix round 1, M1): dillad
	// pushes op 54 when the Bot's installation changes, so the row is read again then and not per check. Nil reads
	// every time.
	recent *recentMusicInstallation
}

type recentMusicInstallation struct {
	mu         sync.Mutex
	row        store.BotInstallationRow
	generation uint64
	at         time.Time
	valid      bool
}

// musicInstallationMaxAge bounds a reused installation row even when no op 54 arrived (Bot v2 Task 19 fix round 2:
// an op 54 lost without a reconnect is not waited for forever).
const musicInstallationMaxAge = 30 * time.Second

// newRuntimeMusicInteractions builds the music handlers. installationStore is the installation row reused until the
// next op 54; authz is the feed-state permission cache and botUser the Bot's user (Bot v2 Task 19 fix round 1: every
// permission check is in memory).
func newRuntimeMusicInteractions(installation, community id.ID, capabilities uint64, runtime *client.Runtime, session *client.Session, registry *musicQueueRegistry, logger *slog.Logger, installationStore runtimeMusicAuthorityStore, authz service.MusicAuthz, botUser id.ID) (*service.MusicInteractions, error) {
	if capabilities&botMusicCapability == 0 {
		return nil, nil
	}
	if installation.IsZero() || community.IsZero() || runtime == nil || session == nil || registry == nil {
		return nil, errors.New("music interaction authority or queue store unavailable")
	}
	stateStore, ok := registry.QueueStore(installation, community)
	if !ok {
		return nil, errors.New("encrypted music queue store unavailable")
	}
	authority := &service.CommunityMusicInteractionAuthority{
		Session:        session,
		Store:          installationStore,
		InstallationID: installation, CommunityID: community,
		Authz: authz, BotUser: botUser,
	}

	handlers := &service.MusicInteractions{
		Store: stateStore, Authority: authority, Logger: logger,
		InstallationID: installation, CommunityID: community,
		CurrentState:        registry.CurrentState,
		OnImmediateControl:  registry.applyImmediateControl,
		OnPersistedSnapshot: musicSnapshotCommitCallback(registry, logger),
		Hints:               client.NewAsyncHints(client.SessionHints{Session: session}), // never delays the reply (I4)
		// Bot v2 Task 19: an opened /play joins the room while its track resolves, and answers once audio starts.
		OnPlayOpened:    registry.warmMusicCall,
		OnPlay:          registry.playPersistedSnapshot,
		OnPlayAbandoned: registry.abandonMusicPlay,
		// A call hint makes the call's members read the state at once: one computation answers them for 100 ms, each
		// member still authorised and answered on their own (Bot v2 Task 19 fix round 4).
		StateReads: service.NewMusicStateReads(),
	}
	if view, err := youtubeViewFor(community); err == nil {
		handlers.Resolver = service.LoggingMusicTrackResolver{Resolver: view.Tracks, Logger: logger, InstallationID: installation}
	} else if logger != nil {
		logger.Info("YouTube music search and enqueue are unavailable", "installation_id", installation.String(), "error", err.Error())
	}
	return handlers, nil
}

// youtubeSandboxConfig finds the sandboxed lookup's tools. A missing one makes YouTube music unavailable.
func youtubeSandboxConfig() (youtube.SandboxConfig, error) {
	var cfg youtube.SandboxConfig
	for _, tool := range []struct {
		name string
		dst  *string
	}{{"yt-dlp", &cfg.YTDLP}, {"node", &cfg.Node}, {"bwrap", &cfg.Bubblewrap}, {"python3", &cfg.Python}, {"ffmpeg", &cfg.FFmpeg}} {
		if path := testhostTool(tool.name); tool.name == "yt-dlp" && path != "" {
			*tool.dst = path
			continue
		}
		path, err := exec.LookPath(tool.name)
		if err != nil {
			return youtube.SandboxConfig{}, err
		}
		*tool.dst = path
	}
	// A new lookup worker warms its challenge solver with one public lookup before a member's (Bot v2 Task 19 fix
	// round 4); a test host's fixture yt-dlp has nothing to warm.
	cfg.WarmUpLookup = testhostTool("yt-dlp") == ""
	return cfg, nil
}

// The warm YouTube backend is one per Bot process (Task 18): one sandboxed worker and one cache, partitioned by
// community through Backend.For, so no community reaches another's cache or pre-buffered track.
var (
	youtubeBackendOnce sync.Once
	youtubeBackend     *youtube.Backend
	youtubeBackendErr  error
)

func sharedYouTubeBackend() (*youtube.Backend, error) {
	youtubeBackendOnce.Do(func() {
		cfg, err := youtubeSandboxConfig()
		if err != nil && youtube.FixturesConfigured() {
			// A test-host Bot serving fixtures needs ffmpeg only (a shipped Bot refuses fixtures in NewBackend).
			var ffmpeg string
			if ffmpeg, err = exec.LookPath("ffmpeg"); err == nil {
				cfg = youtube.SandboxConfig{FFmpeg: ffmpeg}
			}
		}
		if err != nil {
			youtubeBackendErr = err
			return
		}
		cfg.Logger = slog.Default().With("component", "youtube")
		youtubeBackend, youtubeBackendErr = youtube.NewBackend(cfg, 0)
	})
	return youtubeBackend, youtubeBackendErr
}

// youtubeViewFor is the community's own view of the shared backend.
func youtubeViewFor(community id.ID) (*youtube.OwnerBackend, error) {
	backend, err := sharedYouTubeBackend()
	if err != nil {
		return nil, err
	}
	return backend.For(community.String())
}

// youtubeStreamsFor is the community's stream source; with no backend, a source whose every start fails, so the
// queue reports music unavailable instead of the runtime failing to build.
func youtubeStreamsFor(community, installation id.ID) youtube.PCMSourceStarter {
	view, err := youtubeViewFor(community)
	if err != nil {
		slog.Default().Info("YouTube playback is unavailable", "installation_id", installation.String(), "error", err.Error())
		return unavailableYouTubeSource{err: err}
	}
	return view.Streams
}

type unavailableYouTubeSource struct{ err error }

func (u unavailableYouTubeSource) Start(context.Context, string) (io.ReadCloser, error) {
	return nil, u.err
}

func (s runtimeMusicAuthorityStore) GetActiveBotInstallationByCommunity(ctx context.Context, community id.ID) (store.BotInstallationRow, error) {
	if s.runtime == nil || community.IsZero() || community != s.community {
		return store.BotInstallationRow{}, store.ErrNotFound
	}
	if s.recent != nil {
		generation := musicSignals.generation(s.installation)
		s.recent.mu.Lock()
		row, valid := s.recent.row, s.recent.valid && s.recent.generation == generation && time.Since(s.recent.at) < musicInstallationMaxAge
		s.recent.mu.Unlock()
		if valid {
			return row, nil
		}
		row, err := s.readInstallation(ctx, community)
		if err == nil {
			s.recent.mu.Lock()
			s.recent.row, s.recent.generation, s.recent.at, s.recent.valid = row, generation, time.Now(), true
			s.recent.mu.Unlock()
		}
		return row, err
	}
	return s.readInstallation(ctx, community)
}

func (s runtimeMusicAuthorityStore) readInstallation(ctx context.Context, community id.ID) (store.BotInstallationRow, error) {
	installations, err := s.runtime.ListInstallations(ctx)
	if err != nil {
		return store.BotInstallationRow{}, err
	}
	var found *client.Installation
	for i := range installations {
		if installations[i].CommunityID != community {
			continue
		}
		if found != nil {
			return store.BotInstallationRow{}, errors.New("multiple runtime installations found for community")
		}
		found = &installations[i]
	}
	if found == nil {
		return store.BotInstallationRow{}, store.ErrNotFound
	}
	if found.ID != s.installation || found.State > 255 || found.Capabilities > uint64(store.BotKnownCapabilities) {
		return store.BotInstallationRow{}, store.ErrNotFound
	}
	return store.BotInstallationRow{ID: found.ID, CommunityID: found.CommunityID, State: uint8(found.State), Capabilities: uint16(found.Capabilities)}, nil
}

// newMusicSupervisor is the per-channel persistence integration point.
func newMusicSupervisor(ctx context.Context, channel id.ID, store *persistence.Store, starter session.QueueStarter) (*music.Supervisor, error) {
	if ctx == nil || store == nil || starter.OptionsFor == nil {
		return nil, errors.New("music runtime dependencies unavailable")
	}
	options := []music.SupervisorOption{music.WithSkippedTrack(starter.OnSkipped), music.WithPlaybackLog(starter.Logger), music.WithAutomaticPlaybackFailure(func(ctx context.Context, err error) {
		slog.WarnContext(ctx, "bot automatic music playback failed", "channel_id", channel.String(), "class", music.FailureClass(err), "stderr", youtube.FailureStderr(err))
	})}
	if starter.IdleLeaveAfter != nil {
		options = append(options, music.WithIdleLeave(starter.IdleLeaveAfter, nil))
	}
	var queue music.QueueStore = persistedMusicQueue{store: store}
	if starter.OnStateChange != nil {
		queue = notifyingMusicQueue{persistedMusicQueue: persistedMusicQueue{store: store}, changed: starter.OnStateChange}
	}
	return music.NewSupervisor(client.WithoutCooperativeStop(client.WithBackground(ctx)), channel, queue, starter, options...)
}

// restoreMusicSupervisors binds one paused queue supervisor to each voice
// channel in an installation. Its caller owns and closes the returned set.
func restoreMusicSupervisors(ctx context.Context, community id.ID, channels []client.Channel, store *persistence.Store, starter session.QueueStarter) (*musicQueueRuntime, error) {
	return restoreMusicSupervisorsWithStarterFactory(ctx, community, channels, store, func(id.ID) (session.QueueStarter, error) { return starter, nil })
}

type musicStarterFactory func(id.ID) (session.QueueStarter, error)

func restoreMusicSupervisorsWithStarterFactory(ctx context.Context, community id.ID, channels []client.Channel, store *persistence.Store, starterFor musicStarterFactory) (*musicQueueRuntime, error) {
	if ctx == nil || community.IsZero() || store == nil || starterFor == nil || len(channels) > maxMusicSupervisors*2 {
		return nil, errors.New("music runtime dependencies unavailable")
	}
	runtime := &musicQueueRuntime{supervisors: make(map[id.ID]*music.Supervisor), community: community, store: store}
	seen := make(map[id.ID]struct{}, len(channels))
	voiceChannels := 0
	for _, channel := range channels {
		if channel.ID.IsZero() || channel.CommunityID != community {
			_ = runtime.Close()
			return nil, errors.New("music channel inventory is invalid")
		}
		if _, ok := seen[channel.ID]; ok {
			_ = runtime.Close()
			return nil, errors.New("music channel inventory is invalid")
		}
		seen[channel.ID] = struct{}{}
		if channel.Kind != musicVoiceChannelKind {
			continue
		}
		voiceChannels++
		if voiceChannels > maxMusicSupervisors {
			_ = runtime.Close()
			return nil, errors.New("music voice channel limit exceeded")
		}
		starter, err := starterFor(channel.ID)
		if err != nil || starter.OptionsFor == nil {
			_ = runtime.Close()
			if err != nil {
				return nil, err
			}
			return nil, errors.New("music runtime dependencies unavailable")
		}
		supervisor, err := newMusicSupervisor(ctx, channel.ID, store, starter)
		if err != nil {
			_ = starter.Close()
			if errors.Is(err, persistence.ErrUnsupportedProvider) {
				// Quarantine only this channel. Preserve its encrypted row for recovery
				// and retain healthy channels regardless of inventory order.
				slog.WarnContext(ctx, "bot music queue channel unavailable", "channel_id", channel.ID.String(), "class", "unsupported_provider")
				continue
			}
			_ = runtime.Close()
			return nil, err
		}
		runtime.supervisors[channel.ID] = supervisor
	}
	return runtime, nil
}

type musicRuntimeNative interface {
	persistence.Native
	Close() error
}

type retainedMusicRuntime struct {
	community    id.ID
	queues       *musicQueueRuntime
	native       musicRuntimeNative
	interactions bool
	borrowers    uint64 // protected by the registry mutex
	retired      bool
	closeDone    chan struct{} // initialized under the registry mutex before retirement
	closeOnce    sync.Once
	closeErr     error  // published by closing closeDone
	closeReason  string // why the helper is closed; set by retire, logged once by the helper client
}

// musicQueueRegistry keeps queue supervisors attached to the native helper
// that backs their persistence store across reconciliation iterations. Private command
// intake also retains that helper when music is disabled.
type musicQueueRegistry struct {
	lifecycle installationLifecycle
	mu        sync.Mutex
	runtimes  map[id.ID]*retainedMusicRuntime
	retiring  map[id.ID][]*retainedMusicRuntime // pending native closes, including older generations
	closed    bool
	// defaults are the Music page's settings per installation (Task 10c). They have their own
	// lock: supervisors read them while holding their own lock, which r.mu holders also take.
	defaultsMu sync.Mutex
	defaults   map[id.ID]musicDefaults
	noticesMu  sync.Mutex
	notices    map[id.ID]*service.MusicSkipNotices
}

// musicDefaults is one installation's default volume and idle-leave time, already checked.
type musicDefaults struct {
	volume uint8
	idle   time.Duration
}

// The instance defaults (the prototype's Music page: volume 50, leave when idle after 15 minutes), used
// until the server reports settings or when it reports a value outside the Music page's options. A
// saved setting is never rewritten: an installation that stored 100 and 30 keeps them.
const (
	defaultMusicVolume uint8 = 50
	defaultMusicIdle         = 15 * time.Minute
)

func newMusicQueueRegistry() *musicQueueRegistry {
	return &musicQueueRegistry{runtimes: make(map[id.ID]*retainedMusicRuntime), defaults: make(map[id.ID]musicDefaults)}
}

// SetMusicDefaults records an installation's Music page settings as the server reported them.
// The Bot enforces the options itself: anything outside 25/50/75/100 or 5/15/30 minutes falls
// back to the instance defaults (50, 15 minutes), as does a server that does not report them.
func (r *musicQueueRegistry) SetMusicDefaults(installation id.ID, reported bool, volume, idleMinutes uint64) {
	if r == nil || installation.IsZero() {
		return
	}
	d := musicDefaults{volume: defaultMusicVolume, idle: defaultMusicIdle}
	if reported {
		if volume <= 255 && slices.Contains(store.BotMusicVolumes, uint8(volume)) {
			d.volume = uint8(volume)
		}
		if idleMinutes <= 255 && slices.Contains(store.BotMusicIdleMinutes, uint8(idleMinutes)) {
			d.idle = time.Duration(idleMinutes) * time.Minute
		}
	}
	r.defaultsMu.Lock()
	r.defaults[installation] = d
	r.defaultsMu.Unlock()
}

func (r *musicQueueRegistry) musicDefaults(installation id.ID) musicDefaults {
	if r == nil {
		return musicDefaults{volume: defaultMusicVolume, idle: defaultMusicIdle}
	}
	r.defaultsMu.Lock()
	defer r.defaultsMu.Unlock()
	d, ok := r.defaults[installation]
	if !ok {
		return musicDefaults{volume: defaultMusicVolume, idle: defaultMusicIdle}
	}
	return d
}

// DefaultVolume is the volume a new music session in this installation starts at.
func (r *musicQueueRegistry) DefaultVolume(installation id.ID) uint8 {
	return r.musicDefaults(installation).volume
}

// IdleLeave is how long Glenn stays in a voice room with nothing playing.
func (r *musicQueueRegistry) IdleLeave(installation id.ID) time.Duration {
	return r.musicDefaults(installation).idle
}

func (r *musicQueueRegistry) Native(installation, community id.ID) musicRuntimeNative {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	retained := r.runtimes[installation]
	if retained == nil {
		return nil
	}
	if retained.community != community {
		return nil
	}
	return retained.native
}

// errHelperUnusable refuses to register a helper client that is closed or whose helper died.
var errHelperUnusable = errors.New("native helper client is not usable")

// RetainInteractions keeps the helper alive for gateway-driven private command intake,
// including installations without the music capability.
func (r *musicQueueRegistry) RetainInteractions(installation, community id.ID, source musicRuntimeNative) error {
	if r == nil || source == nil || installation.IsZero() || community.IsZero() {
		return errors.New("interaction runtime dependencies unavailable")
	}
	binding := source.Binding()
	if binding.Installation != installation || binding.Community != community {
		return errors.New("interaction native binding does not match installation")
	}
	unlock := r.lifecycle.lock(installation)
	defer unlock()
	// Usable is read outside the registry mutex: the client takes its own lock.
	usable := nativeUsable(source)
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return errors.New("music registry is closed")
	}
	current := r.runtimes[installation]
	if !usable {
		// v0.1.0-bot2.1: a pass that closed the helper re-registered the closed client here, and the due tick met it
		// every 10 s. A closed client is never registered; a retained one that died is retired.
		r.mu.Unlock()
		if current != nil && current.native == source {
			_ = r.disable(installation, helper.ReasonRetainedUnusable)
		}
		return errHelperUnusable
	}
	defer r.mu.Unlock()
	if current != nil {
		if current.community != community || current.native != source {
			return errors.New("interaction runtime differs from retained helper")
		}
		current.interactions = true
	} else {
		r.runtimes[installation] = &retainedMusicRuntime{community: community, native: source, interactions: true}
	}
	return nil
}

// nativeUsable is native.Client's Usable: false once the client is closed or its helper died. A client that cannot
// tell is taken as usable.
func nativeUsable(n musicRuntimeNative) bool {
	u, ok := n.(interface{ Usable() bool })
	return !ok || u.Usable()
}

// KeepAfterPassError answers a pass error that is not about the helper (a dillad 409 or 5xx, a failed music restore):
// a usable retained helper stays registered with its music paused, so the consumers wired to it keep working. Only
// the helper of a gone or disabled installation (installation_gone) or an unusable one (retained_unusable) is closed.
func (r *musicQueueRegistry) KeepAfterPassError(installation id.ID, gone bool) error {
	if r == nil {
		return nil
	}
	unlock := r.lifecycle.lock(installation)
	defer unlock()
	if gone {
		return r.disable(installation, helper.ReasonInstallationGone)
	}
	r.mu.Lock()
	current := r.runtimes[installation]
	r.mu.Unlock()
	if current == nil {
		// Nothing retained: the pass closes its own unretained helper (pass_unretained).
		return nil
	}
	if !nativeUsable(current.native) {
		return r.disable(installation, helper.ReasonRetainedUnusable)
	}
	return r.pauseMusic(installation)
}

func (r *musicQueueRegistry) DisableMusic(installation id.ID) error {
	if r == nil {
		return nil
	}
	unlock := r.lifecycle.lock(installation)
	defer unlock()
	r.mu.Lock()
	current := r.runtimes[installation]
	keepNative := current != nil && current.interactions
	r.mu.Unlock()
	if keepNative {
		return r.pauseMusic(installation)
	}
	return r.disable(installation, helper.ReasonInstallationGone)
}

// AcquireNative pins the exact native generation used by a callback. Retirement
// removes it from the registry immediately and closes it after its last borrower
// releases, so replacing a handler never closes a helper underneath a command.
func (r *musicQueueRegistry) AcquireNative(installation, community id.ID, expected musicRuntimeNative) (func(), bool) {
	if r == nil || expected == nil {
		return nil, false
	}
	r.mu.Lock()
	retained := r.runtimes[installation]
	if r.closed || retained == nil || retained.community != community || retained.native != expected || retained.retired {
		r.mu.Unlock()
		return nil, false
	}
	retained.borrowers++
	r.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			r.mu.Lock()
			retained.borrowers--
			closeNative := retained.retired && retained.borrowers == 0
			r.mu.Unlock()
			if closeNative {
				if err := r.finishNativeClose(installation, retained); err != nil {
					slog.Warn("bot retired native helper close failed", "installation_id", installation.String(), "error", err.Error())
				}
			}
		})
	}, true
}

// QueueCount takes a stable queue snapshot without exposing registry internals.
func (r *musicQueueRegistry) QueueCount(installation, community id.ID) int {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	retained := r.runtimes[installation]
	var queues *musicQueueRuntime
	if retained != nil && retained.community == community {
		queues = retained.queues
	}
	r.mu.Unlock()
	return queues.Len()
}

// StartP95 is the 95th percentile of the first-frame time of the installation's last 20 track starts in each of its
// voice rooms, the worst room's (zero without a start): the health report's music_start_p95_ms.
func (r *musicQueueRegistry) StartP95(installation, community id.ID) time.Duration {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	retained := r.runtimes[installation]
	var queues *musicQueueRuntime
	if retained != nil && retained.community == community {
		queues = retained.queues
	}
	r.mu.Unlock()
	if queues == nil {
		return 0
	}
	queues.mu.Lock()
	supervisors := make([]*music.Supervisor, 0, len(queues.supervisors))
	for _, supervisor := range queues.supervisors {
		supervisors = append(supervisors, supervisor)
	}
	queues.mu.Unlock()
	var worst time.Duration
	for _, supervisor := range supervisors {
		worst = max(worst, music.StartP95(supervisor.Timings()))
	}
	return worst
}

func (r *musicQueueRegistry) QueueStore(installation, community id.ID) (*persistence.Store, bool) {
	if r == nil {
		return nil, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	retained := r.runtimes[installation]
	if retained == nil || retained.community != community || retained.queues == nil || retained.queues.store == nil {
		return nil, false
	}
	return retained.queues.store, true
}

func (r *musicQueueRegistry) CurrentState(ctx context.Context, installation, community, channel id.ID) (music.State, uint64, error) {
	if r == nil {
		return music.State{}, 0, errors.New("music voice session is unavailable")
	}
	r.mu.Lock()
	retained := r.runtimes[installation]
	if retained == nil || retained.community != community || retained.queues == nil {
		r.mu.Unlock()
		return music.State{}, 0, errors.New("music voice session is unavailable")
	}
	queues := retained.queues
	r.mu.Unlock()
	return queues.CurrentState(ctx, channel)
}

// ApplyPersistedSnapshot is the runtime bridge for queue mutations already
// authorized and committed by the interaction service. A missing registry
// entry means the capability or retained native session has been torn down.
func (r *musicQueueRegistry) ApplyPersistedSnapshot(ctx context.Context, installation, community, channel id.ID, state music.State, revision uint64, startPlayback bool) (uint64, error) {
	if r == nil {
		return 0, errors.New("music voice session is unavailable")
	}
	r.mu.Lock()
	retained := r.runtimes[installation]
	if retained == nil || retained.community != community || retained.queues == nil {
		r.mu.Unlock()
		return 0, errors.New("music voice session is unavailable")
	}
	queues := retained.queues
	r.mu.Unlock()
	return queues.ApplyPersistedSnapshot(ctx, channel, state, revision, startPlayback)
}

func (r *musicQueueRegistry) supervisor(installation, community, channel id.ID) (*music.Supervisor, error) {
	if r == nil {
		return nil, errors.New("music voice session is unavailable")
	}
	r.mu.Lock()
	retained := r.runtimes[installation]
	if retained == nil || retained.community != community || retained.queues == nil {
		r.mu.Unlock()
		return nil, errors.New("music voice session is unavailable")
	}
	queues := retained.queues
	r.mu.Unlock()
	queues.mu.Lock()
	defer queues.mu.Unlock()
	if queues.closed || queues.supervisors[channel] == nil {
		return nil, music.ErrSupervisorClosed
	}
	return queues.supervisors[channel], nil
}

func (r *musicQueueRegistry) schedulePersistedSnapshot(ctx context.Context, installation, community, channel id.ID, state music.State, revision uint64, start bool) (uint64, error) {
	s, err := r.supervisor(installation, community, channel)
	if err != nil {
		return 0, err
	}
	return s.SchedulePersistedSnapshot(ctx, state, revision, start)
}

// warmMusicCall begins the voice room's call-group join and room connection for an authorised /play that was just
// opened, beside its resolve (Bot v2 Task 19). It never blocks and never starts audio; a room the Bot already plays
// in is left alone.
func (r *musicQueueRegistry) warmMusicCall(ctx context.Context, installation, community, channel id.ID) {
	if s, err := r.supervisor(installation, community, channel); err == nil {
		s.WarmCall(music.PlaybackGroup(ctx))
	}
}

// abandonMusicPlay is a /play whose warm began and which ended without starting (M3).
func (r *musicQueueRegistry) abandonMusicPlay(_ context.Context, installation, community, channel id.ID) {
	if s, err := r.supervisor(installation, community, channel); err == nil {
		s.PlayAbandoned()
	}
}

// errMusicPlayNotStarted is a /play whose start returned without a playing track (a newer pause or stop won).
var errMusicPlayNotStarted = errors.New("music playback did not start")

// playPersistedSnapshot hands a committed /play that starts playback to its supervisor and returns once the first
// frame is published, so the /play can answer "Now playing" when audio starts (Bot v2 Task 19).
func (r *musicQueueRegistry) playPersistedSnapshot(ctx context.Context, installation, community, channel id.ID, state music.State, revision uint64) (uint64, error) {
	s, err := r.supervisor(installation, community, channel)
	if err != nil {
		return 0, err
	}
	applied, err := s.ApplyPersistedSnapshot(ctx, state, revision, true)
	if err == nil && !s.Playing() {
		err = errMusicPlayNotStarted
	}
	return applied, err
}

func (r *musicQueueRegistry) applyImmediateControl(ctx context.Context, installation, community, channel id.ID, op music.Operation) (music.State, error) {
	s, err := r.supervisor(installation, community, channel)
	if err != nil {
		return music.State{}, err
	}
	return s.ApplyImmediate(ctx, op)
}

// Reconcile returns true only when ownership of nativeClient transfers to the
// registry. A capability loss closes both the retained supervisors and helper.
func (r *musicQueueRegistry) Reconcile(ctx context.Context, installation, community id.ID, capabilities uint64, channels []client.Channel, nativeClient musicRuntimeNative, starter session.QueueStarter) (bool, error) {
	return r.ReconcileWithStarterFactory(ctx, installation, community, capabilities, channels, nativeClient, func(id.ID) (session.QueueStarter, error) { return starter, nil })
}

func (r *musicQueueRegistry) ReconcileWithStarterFactory(ctx context.Context, installation, community id.ID, capabilities uint64, channels []client.Channel, nativeClient musicRuntimeNative, starterFor musicStarterFactory) (bool, error) {
	if r == nil || installation.IsZero() || community.IsZero() || nativeClient == nil || starterFor == nil {
		return false, errors.New("music runtime dependencies unavailable")
	}
	unlock := r.lifecycle.lock(installation)
	defer unlock()
	r.mu.Lock()
	existing := r.runtimes[installation]
	closed := r.closed
	r.mu.Unlock()
	if closed {
		return false, errors.New("music registry is closed")
	}
	// A retained helper that died is retired before anything keeps it, and a closed client is never registered.
	if existing != nil && !nativeUsable(existing.native) {
		_ = r.disable(installation, helper.ReasonRetainedUnusable)
		existing = nil
	}
	if capabilities&botMusicCapability == 0 {
		if existing != nil && existing.interactions {
			return false, r.pauseMusic(installation)
		}
		return false, r.disable(installation, helper.ReasonInstallationGone)
	}
	if binding := nativeClient.Binding(); binding.Installation != installation || binding.Community != community {
		_ = r.disable(installation, helper.ReasonLifecycleBind)
		return false, errors.New("music native binding does not match installation")
	}
	if !nativeUsable(nativeClient) {
		return false, errHelperUnusable
	}
	if existing != nil && existing.community != community {
		_ = r.disable(installation, helper.ReasonLifecycleBind)
		existing = nil
	}
	if existing != nil && existing.queues != nil {
		if err := syncMusicSupervisorsWithStarterFactory(ctx, existing.queues, channels, starterFor); err != nil {
			_ = r.pauseMusic(installation)
			return false, err
		}
		return false, nil
	}
	// Existing interactions own the retained helper; a newly supplied caller-owned
	// helper must never become the store behind that helper's supervisors.
	queueNative := nativeClient
	if existing != nil {
		queueNative = existing.native
	}
	queues, err := restoreMusicQueuesIfEnabledWithStarterFactory(ctx, community, capabilities, func(context.Context, id.ID) ([]client.Channel, error) { return channels, nil }, queueNative, starterFor)
	if err != nil || queues == nil {
		return false, err
	}
	// The restore can be slow; a helper that died meanwhile is not registered behind the new queues.
	if !nativeUsable(queueNative) {
		_ = queues.Close()
		if existing != nil {
			_ = r.disable(installation, helper.ReasonRetainedUnusable)
		}
		return false, errHelperUnusable
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		_ = queues.Close()
		return false, errors.New("music registry is closed")
	}
	if existing != nil {
		existing.queues = queues
	} else {
		r.runtimes[installation] = &retainedMusicRuntime{community: community, queues: queues, native: nativeClient}
	}
	r.mu.Unlock()
	return existing == nil, nil
}

func syncMusicSupervisorsWithStarterFactory(ctx context.Context, runtime *musicQueueRuntime, channels []client.Channel, starterFor musicStarterFactory) error {
	if ctx == nil || runtime == nil || runtime.store == nil || starterFor == nil || len(channels) > maxMusicSupervisors*2 {
		return errors.New("music runtime dependencies unavailable")
	}
	runtime.lifecycle.Lock()
	defer runtime.lifecycle.Unlock()
	runtime.mu.Lock()
	closed := runtime.closed
	runtime.mu.Unlock()
	if closed {
		return music.ErrSupervisorClosed
	}
	desired := make(map[id.ID]struct{}, len(channels))
	seen := make(map[id.ID]struct{}, len(channels))
	for _, channel := range channels {
		if channel.ID.IsZero() || channel.CommunityID != runtime.community {
			return errors.New("music channel inventory is invalid")
		}
		if _, ok := seen[channel.ID]; ok {
			return errors.New("music channel inventory is invalid")
		}
		seen[channel.ID] = struct{}{}
		if channel.Kind == musicVoiceChannelKind {
			desired[channel.ID] = struct{}{}
		}
	}
	if len(desired) > maxMusicSupervisors {
		return errors.New("music voice channel limit exceeded")
	}
	removed := make(map[id.ID]*music.Supervisor)
	runtime.mu.Lock()
	for channel, supervisor := range runtime.supervisors {
		if _, ok := desired[channel]; !ok {
			removed[channel] = supervisor
			delete(runtime.supervisors, channel)
		}
	}
	for channel := range runtime.supervisors {
		delete(desired, channel)
	}
	runtime.mu.Unlock()
	closeMusicSupervisors(removed)
	for _, channel := range channels {
		if _, ok := desired[channel.ID]; !ok {
			continue
		}
		starter, err := starterFor(channel.ID)
		if err != nil || starter.OptionsFor == nil {
			if err != nil {
				return err
			}
			return errors.New("music runtime dependencies unavailable")
		}
		supervisor, err := newMusicSupervisor(ctx, channel.ID, runtime.store, starter)
		if err != nil {
			_ = starter.Close()
			if errors.Is(err, persistence.ErrUnsupportedProvider) {
				slog.WarnContext(ctx, "bot music queue channel unavailable", "channel_id", channel.ID.String(), "class", "unsupported_provider")
				continue
			}
			return err
		}
		runtime.mu.Lock()
		runtime.supervisors[channel.ID] = supervisor
		runtime.mu.Unlock()
	}
	return nil
}

// Disable retires the installation's helper and queues. reason is one of the helper.Reason constants and is logged
// once, when the helper closes.
func (r *musicQueueRegistry) Disable(installation id.ID, reason string) error {
	if r == nil {
		return nil
	}
	unlock := r.lifecycle.lock(installation)
	defer unlock()
	return r.disable(installation, reason)
}

// disable and pauseMusic require the installation lifecycle lock, never mu.
func (r *musicQueueRegistry) disable(installation id.ID, reason string) error {
	retained, closeNative := r.retire(installation, reason)
	if closeNative {
		return r.finishNativeClose(installation, retained)
	}
	return nil
}

// retire detaches a generation and stops its queues before allowing the last
// borrower to close its native helper. The caller holds the installation
// lifecycle lock; no native Close runs while holding the registry mutex.
func (r *musicQueueRegistry) retire(installation id.ID, reason string) (*retainedMusicRuntime, bool) {
	r.mu.Lock()
	retained := r.runtimes[installation]
	delete(r.runtimes, installation)
	var queues *musicQueueRuntime
	if retained != nil {
		queues, retained.queues = retained.queues, nil
		if retained.closeReason == "" {
			retained.closeReason = reason
		}
		if retained.closeDone == nil {
			retained.closeDone = make(chan struct{})
		}
		if r.retiring == nil {
			r.retiring = make(map[id.ID][]*retainedMusicRuntime)
		}
		r.retiring[installation] = append(r.retiring[installation], retained)
	}
	r.mu.Unlock()
	if queues != nil {
		_ = queues.Close()
	}
	r.closeNotices(installation)
	if retained == nil {
		return nil, false
	}
	// Mark retired only after supervisors stop using the helper. A borrower may
	// finish during queue shutdown, but cannot close the helper until this point.
	r.mu.Lock()
	retained.retired = true
	closeNative := retained.borrowers == 0
	r.mu.Unlock()
	return retained, closeNative
}

// finishNativeClose is shared by immediate retirement and the last borrower.
// It closes the helper once and removes only that completed generation, even
// when a replacement is retained under the same installation. Completion covers
// both the helper exit (releasing state.lock) and removal from pending closes.
func (r *musicQueueRegistry) finishNativeClose(installation id.ID, retained *retainedMusicRuntime) error {
	retained.closeOnce.Do(func() {
		if rc, ok := retained.native.(interface{ CloseWithReason(string) error }); ok {
			retained.closeErr = rc.CloseWithReason(retained.closeReason)
		} else {
			retained.closeErr = retained.native.Close()
		}
		r.mu.Lock()
		pending := r.retiring[installation]
		for n, current := range pending {
			if current == retained {
				pending = slices.Delete(pending, n, n+1)
				break
			}
		}
		if len(pending) == 0 {
			delete(r.retiring, installation)
		} else {
			r.retiring[installation] = pending
		}
		r.mu.Unlock()
		close(retained.closeDone)
	})
	return retained.closeErr
}

func (r *musicQueueRegistry) closeNotices(installation id.ID) {
	r.noticesMu.Lock()
	notices := r.notices[installation]
	r.noticesMu.Unlock()
	if notices != nil {
		notices.Close()
	}
}

// pauseMusic disables queue state after a recoverable music error. The native
// helper also serves non-music interactions, so it stays retained.
func (r *musicQueueRegistry) pauseMusic(installation id.ID) error {
	r.mu.Lock()
	retained := r.runtimes[installation]
	var queues *musicQueueRuntime
	if retained != nil {
		queues, retained.queues = retained.queues, nil
	}
	r.mu.Unlock()
	if queues == nil {
		return nil
	}
	err := queues.Close()
	r.closeNotices(installation)
	return err
}

func (r *musicQueueRegistry) RetainOnly(installations []id.ID) error {
	if r == nil {
		return nil
	}
	active := make(map[id.ID]struct{}, len(installations))
	for _, installation := range installations {
		active[installation] = struct{}{}
	}
	r.mu.Lock()
	reason := helper.ReasonInstallationGone
	if r.closed {
		reason = helper.ReasonShutdown
	}
	var removed []id.ID
	for installation := range r.runtimes {
		if _, ok := active[installation]; !ok {
			removed = append(removed, installation)
		}
	}
	r.mu.Unlock()
	var errs []error
	for _, installation := range removed {
		if err := r.Disable(installation, reason); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (r *musicQueueRegistry) Close() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	r.closed = true
	r.mu.Unlock()
	return r.RetainOnly(nil)
}

func restoreMusicQueuesIfEnabled(ctx context.Context, community id.ID, capabilities uint64, loadChannels func(context.Context, id.ID) ([]client.Channel, error), stateNative persistence.Native, starter session.QueueStarter) (*musicQueueRuntime, error) {
	return restoreMusicQueuesIfEnabledWithStarterFactory(ctx, community, capabilities, loadChannels, stateNative, func(id.ID) (session.QueueStarter, error) { return starter, nil })
}

func restoreMusicQueuesIfEnabledWithStarterFactory(ctx context.Context, community id.ID, capabilities uint64, loadChannels func(context.Context, id.ID) ([]client.Channel, error), stateNative persistence.Native, starterFor musicStarterFactory) (*musicQueueRuntime, error) {
	if capabilities&botMusicCapability == 0 {
		return nil, nil
	}
	if loadChannels == nil || stateNative == nil {
		return nil, errors.New("music runtime dependencies unavailable")
	}
	channels, err := loadChannels(ctx, community)
	if err != nil {
		return nil, err
	}
	store, err := persistence.New(stateNative, community)
	if err != nil {
		return nil, err
	}
	return restoreMusicSupervisorsWithStarterFactory(ctx, community, channels, store, starterFor)
}

func closeMusicSupervisors(queues map[id.ID]*music.Supervisor) {
	for _, supervisor := range queues {
		_ = supervisor.Close()
	}
}

func unavailableMusicQueueStarter() session.QueueStarter {
	return session.QueueStarter{OptionsFor: func(context.Context, music.Track) (session.Options, error) {
		return session.Options{}, errMusicPCMUnavailable
	}}
}

var _ livekit.EpochSource = (*native.Client)(nil)

func (r *musicQueueRegistry) SkipNotices(installation id.ID) *service.MusicSkipNotices {
	r.noticesMu.Lock()
	defer r.noticesMu.Unlock()
	if r.notices == nil {
		r.notices = make(map[id.ID]*service.MusicSkipNotices)
	}
	if r.notices[installation] == nil {
		r.notices[installation] = &service.MusicSkipNotices{}
	}
	return r.notices[installation]
}
