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

	"github.com/jonasthim/dilla/internal/bot/client"
	"github.com/jonasthim/dilla/internal/bot/music"
	"github.com/jonasthim/dilla/internal/bot/music/encode"
	"github.com/jonasthim/dilla/internal/bot/music/livekit"
	"github.com/jonasthim/dilla/internal/bot/music/persistence"
	"github.com/jonasthim/dilla/internal/bot/music/session"
	"github.com/jonasthim/dilla/internal/bot/music/spotify"
	"github.com/jonasthim/dilla/internal/bot/music/youtube"
	"github.com/jonasthim/dilla/internal/bot/native"
	"github.com/jonasthim/dilla/internal/bot/service"
	"github.com/jonasthim/dilla/internal/bot/spotifyauth"
	"github.com/jonasthim/dilla/internal/id"
	"github.com/jonasthim/dilla/internal/store"
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

type installationSpotifyCredentials interface {
	Load(context.Context) (spotifyauth.Token, error)
}

type installationSpotifyCatalogResolver struct {
	credentials installationSpotifyCredentials
	catalog     *spotify.Catalog
}

func (r installationSpotifyCatalogResolver) ResolveSpotify(ctx context.Context, input string, requester id.ID) ([]music.Track, error) {
	if r.credentials == nil || r.catalog == nil {
		return nil, service.ErrSpotifyCredentials
	}
	token, err := r.credentials.Load(ctx)
	if err != nil {
		return nil, err
	}
	return r.catalog.Expand(ctx, token.AccessToken, input, requester)
}

// musicRuntimeDeps contains the process-wide dependencies and the channel
// identity for one supervisor. Each supervisor gets its own queue/session
// state; SpotifyLease is shared by all supervisors in this service process.
type musicRuntimeDeps struct {
	Community, Channel id.ID
	Calls              musicCallStarter
	JoinCallGroup      func(context.Context) error
	SpotifyPCM         session.PCMStarter
	YouTubePCM         youtube.PCMSourceStarter
	LeaseManager       *music.SpotifyLeaseManager
	Encoder            session.EncoderStarter
	Publisher          session.PublisherStarter
	Epochs             livekit.EpochSource
	Lifetime           time.Duration
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
	publisher := deps.Publisher
	var resetConnection, closeConnection, leaveConnection func() error
	var setCallEnded func(func())
	if publisher == nil {
		pool, err := livekit.NewPool(deps.Lifetime)
		if err != nil {
			return session.QueueStarter{}, err
		}
		// Signal task 8c (Ruling AU): the bot's ping on the stage, reported through dillad like a person's.
		if r, ok := deps.Calls.(callRTTReporter); ok {
			pool.SetRTTReporter(r.ReportCallRTT)
		}
		publisher = session.RetainedLiveKitPublisher{Pool: pool}
		resetConnection, closeConnection, leaveConnection = pool.Reset, pool.Close, pool.LeaveIdle
		setCallEnded = pool.SetRoomEndedReporter
	}
	options := func(ctx context.Context, track music.Track) (session.Options, error) {
		if ctx == nil || track.CanonicalID == "" {
			return session.Options{}, session.ErrInvalid
		}
		var pcm session.PCMStarter
		switch track.Provider {
		case music.ProviderSpotify:
			if deps.SpotifyPCM == nil {
				return session.Options{}, spotify.ErrProviderUnavailable
			}
			pcm = deps.SpotifyPCM
		case music.ProviderYouTube:
			if deps.YouTubePCM == nil {
				return session.Options{}, session.ErrInvalid
			}
			pcm = session.YouTubePCM{Source: deps.YouTubePCM}
		default:
			return session.Options{}, session.ErrInvalid
		}
		if pcm == nil {
			return session.Options{}, session.ErrInvalid
		}
		if track.Provider == music.ProviderSpotify && deps.LeaseManager == nil {
			return session.Options{}, session.ErrInvalid
		}
		prepareCall := func(ctx context.Context) (client.Call, error) {
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
		var acquire session.LeaseAcquire
		if track.Provider == music.ProviderSpotify {
			acquire = session.AcquireSpotify(deps.LeaseManager)
		}
		return session.Options{
			Community: deps.Community, Channel: deps.Channel, Provider: track.Provider,
			Track: track.CanonicalID, PrepareCall: prepareCall,
			Acquire: acquire, PCM: pcm,
			Encoder: deps.Encoder, Publisher: publisher, Epochs: deps.Epochs,
		}, nil
	}
	return session.QueueStarter{SetCallEnded: setCallEnded, OptionsFor: options, ResetConnection: resetConnection, CloseConnection: closeConnection, LeaveConnection: leaveConnection}, nil
}

func newInstallationYouTubeQueueStarter(instance, installation, community, channel id.ID, calls *client.Session, source installationMusicNative) (session.QueueStarter, error) {
	return newInstallationMusicQueueStarter(instance, installation, community, channel, calls, source, nil, nil, nil)
}

func newInstallationMusicQueueStarter(instance, installation, community, channel id.ID, calls *client.Session, source installationMusicNative, spotifyPCM session.PCMStarter, youtubeSource youtube.PCMSourceStarter, leases *music.SpotifyLeaseManager, joinCallGroup ...func(context.Context) error) (session.QueueStarter, error) {
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
		JoinCallGroup: join,
		SpotifyPCM:    spotifyPCM,
		YouTubePCM: func() youtube.PCMSourceStarter {
			if youtubeSource != nil {
				return youtubeSource
			}
			return youtube.PCMStarter{YTDLP: "yt-dlp", FFmpeg: "ffmpeg", Node: "node", Logger: slog.Default().With("installation_id", installation.String())}
		}(),
		LeaseManager: leases,
		Encoder: session.RuntimeEncoder{Config: session.EncoderConfig{
			Capacity: 50, Options: encode.Options{FFmpegPath: "ffmpeg"},
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

// runtimeMusicAuthorityStore resolves installation and DJ policy from current
// bot APIs on every authorization check. It never falls back to cached roles.
type runtimeMusicAuthorityStore struct {
	runtime      *client.Runtime
	session      *client.Session
	installation id.ID
	community    id.ID
}

func newRuntimeMusicInteractions(installation, community id.ID, capabilities uint64, runtime *client.Runtime, session *client.Session, registry *musicQueueRegistry, logger *slog.Logger, spotifyResolvers ...service.SpotifyMusicTrackResolver) (*service.MusicInteractions, error) {
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
		Store:          runtimeMusicAuthorityStore{runtime: runtime, session: session, installation: installation, community: community},
		InstallationID: installation, CommunityID: community,
	}

	handlers := &service.MusicInteractions{
		Store: stateStore, Authority: authority, Logger: logger,
		InstallationID: installation, CommunityID: community,
		CurrentState:        registry.CurrentState,
		OnImmediateControl:  registry.applyImmediateControl,
		OnPersistedSnapshot: musicSnapshotCommitCallback(registry, logger),
	}
	if len(spotifyResolvers) > 0 {
		handlers.SpotifyResolver = spotifyResolvers[0]
	}
	if resolver, err := newYouTubeMetadataResolver(); err == nil {
		handlers.Resolver = service.LoggingMusicTrackResolver{Resolver: resolver, Logger: logger, InstallationID: installation}
	} else if logger != nil {
		logger.Info("YouTube music search and enqueue are unavailable", "installation_id", installation.String(), "error", err.Error())
	}
	return handlers, nil
}

func newYouTubeMetadataResolver() (youtube.MetadataResolver, error) {
	ytdlp, err := exec.LookPath("yt-dlp")
	if err != nil {
		return youtube.MetadataResolver{}, err
	}
	node, err := exec.LookPath("node")
	if err != nil {
		return youtube.MetadataResolver{}, err
	}
	bubblewrap, err := exec.LookPath("bwrap")
	if err != nil {
		return youtube.MetadataResolver{}, err
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		return youtube.MetadataResolver{}, err
	}
	return youtube.NewMetadataResolver(ytdlp, node, bubblewrap, python)
}

func (s runtimeMusicAuthorityStore) GetActiveBotInstallationByCommunity(ctx context.Context, community id.ID) (store.BotInstallationRow, error) {
	if s.runtime == nil || community.IsZero() || community != s.community {
		return store.BotInstallationRow{}, store.ErrNotFound
	}
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
	return music.NewSupervisor(client.WithoutCooperativeStop(client.WithBackground(ctx)), channel, persistedMusicQueue{store: store}, starter, options...)
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
		if len(runtime.supervisors) == maxMusicSupervisors {
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
	closeErr     error // published by closing closeDone
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

// Today's behaviour, used until the server reports settings or when it reports a value outside
// the Music page's options.
const (
	defaultMusicVolume uint8 = 100
	defaultMusicIdle         = 30 * time.Minute
)

func newMusicQueueRegistry() *musicQueueRegistry {
	return &musicQueueRegistry{runtimes: make(map[id.ID]*retainedMusicRuntime), defaults: make(map[id.ID]musicDefaults)}
}

// SetMusicDefaults records an installation's Music page settings as the server reported them.
// The Bot enforces the options itself: anything outside 25/50/75/100 or 5/15/30 minutes falls
// back to today's behaviour (100, 30 minutes), as does a server that does not report them.
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
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return errors.New("music registry is closed")
	}
	if current := r.runtimes[installation]; current != nil {
		if current.community != community || current.native != source {
			return errors.New("interaction runtime differs from retained helper")
		}
		current.interactions = true
	} else {
		r.runtimes[installation] = &retainedMusicRuntime{community: community, native: source, interactions: true}
	}
	return nil
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
	return r.disable(installation)
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
	if capabilities&botMusicCapability == 0 {
		if existing != nil && existing.interactions {
			return false, r.pauseMusic(installation)
		}
		return false, r.disable(installation)
	}
	if binding := nativeClient.Binding(); binding.Installation != installation || binding.Community != community {
		_ = r.disable(installation)
		return false, errors.New("music native binding does not match installation")
	}
	if existing != nil && existing.community != community {
		_ = r.disable(installation)
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
			return err
		}
		runtime.mu.Lock()
		runtime.supervisors[channel.ID] = supervisor
		runtime.mu.Unlock()
	}
	return nil
}

func (r *musicQueueRegistry) Disable(installation id.ID) error {
	if r == nil {
		return nil
	}
	unlock := r.lifecycle.lock(installation)
	defer unlock()
	return r.disable(installation)
}

// disable and pauseMusic require the installation lifecycle lock, never mu.
func (r *musicQueueRegistry) disable(installation id.ID) error {
	retained, closeNative := r.retire(installation)
	if closeNative {
		return r.finishNativeClose(installation, retained)
	}
	return nil
}

// retire detaches a generation and stops its queues before allowing the last
// borrower to close its native helper. The caller holds the installation
// lifecycle lock; no native Close runs while holding the registry mutex.
func (r *musicQueueRegistry) retire(installation id.ID) (*retainedMusicRuntime, bool) {
	r.mu.Lock()
	retained := r.runtimes[installation]
	delete(r.runtimes, installation)
	var queues *musicQueueRuntime
	if retained != nil {
		queues, retained.queues = retained.queues, nil
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
		retained.closeErr = retained.native.Close()
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
	var removed []id.ID
	for installation := range r.runtimes {
		if _, ok := active[installation]; !ok {
			removed = append(removed, installation)
		}
	}
	r.mu.Unlock()
	var errs []error
	for _, installation := range removed {
		if err := r.Disable(installation); err != nil {
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

type unavailableProviderPCM struct{ cause error }

func (p unavailableProviderPCM) Check(context.Context) error { return p.cause }
func (p unavailableProviderPCM) Start(context.Context, string) (io.ReadCloser, error) {
	return nil, p.cause
}
