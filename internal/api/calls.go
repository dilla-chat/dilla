package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fxamacker/cbor/v2"
	"github.com/livekit/protocol/livekit"

	"github.com/jonasthim/dilla/internal/auth"
	botaccess "github.com/jonasthim/dilla/internal/bot/access"
	"github.com/jonasthim/dilla/internal/cborx"
	"github.com/jonasthim/dilla/internal/clock"
	"github.com/jonasthim/dilla/internal/ds"
	"github.com/jonasthim/dilla/internal/id"
	"github.com/jonasthim/dilla/internal/server"
	"github.com/jonasthim/dilla/internal/sfu"
	"github.com/jonasthim/dilla/internal/store"
)

// CallTokens is the SFU the call routes drive; internal/sfu.(*Server) is one. Token mints a
// room-join JWT whose grants are exactly the permission it is handed (sfu.PublishGrant's mirror of
// speak, video and screen_share) — the gates in front of it are this file's. CreateRoom opens a room
// before any token for it exists (room.auto_create is false). UpdatePermission pushes a complete
// permission to a connected participant, answering sfu.ErrNoParticipant for one the room does not
// hold. RemoveParticipants disconnects a device and its "#" shadows; RemoveParticipant disconnects
// one participant by its exact identity (one that is no device). Participants is the room's list
// for the advisory E_CALL_FULL count and the grant sync. DeleteRoom closes a room and disconnects
// everyone in it, answering nil for a room the SFU does not know. Rooms lists the names of every
// room the SFU holds, for the room sweep (SweepRooms).
type CallTokens interface {
	Rooms(ctx context.Context) ([]string, error)
	Token(room, identity string, perm *livekit.ParticipantPermission, attrs map[string]string) (string, error)
	DeleteRoom(ctx context.Context, room string) error
	CreateRoom(ctx context.Context, room string) error
	UpdatePermission(ctx context.Context, room, identity string, perm *livekit.ParticipantPermission) error
	RemoveParticipants(ctx context.Context, room string, device id.ID) error
	RemoveParticipant(ctx context.Context, room, identity string) error
	Participants(ctx context.Context, room string) ([]*livekit.ParticipantInfo, error)
}

// CallCounters is the metric surface the call routes move; *obs.Metrics is one. Every counter is
// label-free.
type CallCounters interface {
	CallFull()
	ShareRefused()
	// CallCut counts a participant the grant sync took out of a call room: a device whose user lost
	// view_channel or connect, or an identity that is no device.
	CallCut()
	// CallGrantRetry counts one retry of a cut or demotion that had not landed in the SFU.
	CallGrantRetry()
	// CallRepairsPending sets the gauge of repairs outstanding across every call.
	CallRepairsPending(n int)
}

// vdecAttribute is the participant attribute that carries a device's video decode list (DEV-07),
// the calls request's vdec written into its token.
const vdecAttribute = "dilla.vdec"

// defaultMaxSharers is livekit.max_publishers' default, applied when CallsConfig leaves it at zero.
const defaultMaxSharers = 10

// roomCloseTimeout bounds the SFU call that closes an ended call's room.
const roomCloseTimeout = 5 * time.Second

const (
	// sfuCallTimeout bounds every SFU call made under a call's lock, whatever the caller's context.
	sfuCallTimeout = 2 * time.Second
	// callLockWait bounds how long a request (a share, an unshare, a start, the /rtc gate) waits for
	// a call's lock before it answers 503 E_UNAVAILABLE.
	callLockWait = 2 * time.Second
	// callHoldRequest bounds how long a request holds a call's lock once it has it: its own repair
	// (a removal and a fallback demotion) and its own push, each bounded by sfuCallTimeout. With
	// callLockWait, no request — the /rtc gate included — waits on a call longer than their sum.
	callHoldRequest = 3 * sfuCallTimeout
	// callLockWaitBackground bounds the wait of the work that runs after an answer — a grant sync
	// after a role change, a DELETE's slot drop — which leaves its work to the retry loop instead.
	callLockWaitBackground = 10 * time.Second
	// callHoldBackground bounds how long one hold of a call's lock by background work (a grant
	// sync, a retry pass, a room sweep, a device cut) lasts in total, however many participants and
	// repairs it meets; what it has not reached by then is left pending for the next pass.
	callHoldBackground = 10 * time.Second
	// maxRepairsPerTick caps the repairs one retry pass, or one grant sync of a call, attempts.
	maxRepairsPerTick = 32
	// maxDeferredPerTick caps the deferred leaves one retry pass looks at, after its repairs
	// (CallEvents.completeDeferredLeaves); the rest wait for the next pass. Each room is listed once
	// per pass, so a pass makes at most this many SFU calls.
	maxDeferredPerTick = 8
	// deferredPassBudget bounds the deferred-leave step of one retry pass, on its own: two SFU calls'
	// worth, so a slow SFU delays the next pass's repairs by at most this.
	deferredPassBudget = 2 * sfuCallTimeout
	// CallRetryInterval is the retry loop's cadence (StartRetries).
	CallRetryInterval = 5 * time.Second
	// RoomSweepInterval is how often, at most, the retry loop sweeps the SFU's rooms (SweepRooms).
	RoomSweepInterval = 30 * time.Second
	// maxRoomsPerSweep caps the rooms one sweep pass visits; the next pass goes on where it stopped.
	maxRoomsPerSweep = 64
	// sweepPassBudget bounds one whole sweep pass; rooms it does not reach roll to the next.
	sweepPassBudget = 20 * time.Second
	// cutDeviceBudget bounds one pass over the cut-request set; the sweep covers what it misses.
	cutDeviceBudget = 15 * time.Second
	// retryPassBudget bounds one RetryPending pass; the calls it does not reach wait for the next.
	retryPassBudget = 15 * time.Second
	// startRetryAfterMS is the retry_after_ms of a start whose call ended as it was opened.
	startRetryAfterMS = 250
)

// lock takes call's lock for at most wait (or until ctx ends) and returns a context bounded by hold
// for the work done under it — one deadline per hold, so no holder keeps the call longer — and its
// release, which also reports the pending-repair gauge. A lock it cannot take is
// 503 E_UNAVAILABLE with retry_after_ms.
func (h *Calls) lock(ctx context.Context, call id.ID, wait, hold time.Duration) (context.Context, func(), error) {
	unlock, err := h.leases.lockCall(ctx, call, wait)
	if err != nil {
		return nil, nil, server.Unavailable(uint64(callLockWait/time.Millisecond), "the call is busy; retry shortly")
	}
	hctx, cancel := context.WithTimeout(ctx, hold)
	return hctx, func() {
		cancel()
		unlock()
		h.reportPending()
	}, nil
}

// reportPending sets the pending-repair gauge.
func (h *Calls) reportPending() {
	if h.counters != nil {
		h.counters.CallRepairsPending(h.leases.pendingCount())
	}
}

// sfuCtx is ctx bounded by sfuCallTimeout.
func sfuCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, sfuCallTimeout)
}

// dropRoom forgets an ended call's slots, repairs and F11 penalties of its room under its lock. The
// call is already over in the record, so a lock that cannot be taken is logged and left to the retry
// loop, which drops them for an ended call; the wait does not end with the caller's context.
func (h *Calls) dropRoom(ctx context.Context, row store.VoiceSessionRow) {
	h.forgetMembers(row.CallID, row.LivekitRoom)
	_, unlock, err := h.lock(context.WithoutCancel(ctx), row.CallID, callLockWaitBackground, callHoldBackground)
	if err != nil {
		h.log.WarnContext(ctx, "dropping an ended call's sharing slots is left to the retry loop", "call", row.CallID, "err", err)
		return
	}
	h.leases.dropRoom(row.CallID, row.LivekitRoom)
	unlock()
}

// CallsConfig is what the call routes hand a client besides the token.
type CallsConfig struct {
	// LiveKitURL is the signalling URL a client connects to.
	LiveKitURL string
	// TURNSecret is turn.shared_secret_file's content; "" when TURN is off.
	TURNSecret string
	// TURNURLs are the relay URLs a client may use (a turns: URL on 443 behind
	// the demux, or a turn: URL on turn.listen behind a proxy); empty when
	// TURN is off.
	TURNURLs []string
	// CredentialTTL is turn.credential_ttl.
	CredentialTTL time.Duration
	// MaxVoiceParticipants is livekit.max_voice_participants, the advisory E_CALL_FULL count's
	// limit; 0 counts nothing. LiveKit's room.max_participants stays authoritative.
	MaxVoiceParticipants int
	// MaxPublishers is livekit.max_publishers: how many devices of one call may hold a sharing slot.
	MaxPublishers int
	// MaxAudioBitrateKbps and MaxShareBitrateKbps are livekit.max_audio_bitrate_kbps and
	// livekit.max_share_bitrate_kbps, handed to clients in the calls response's caps (DEV-26).
	MaxAudioBitrateKbps int
	MaxShareBitrateKbps int
	// VP9 is livekit.vp9 (F4: default off), the caps element's third field.
	VP9 bool
}

// Calls serves protocol/09 § Voice. Register mounts the handlers bare, as the other Plan 2 route
// groups do, and each requires an enrolled session itself.
type Calls struct {
	repo     store.Repository
	res      *Resolver
	sfu      CallTokens
	cfg      CallsConfig
	clk      clock.Clock
	log      *slog.Logger
	leases   *shareLeases
	counters CallCounters
	// rtts limits POST /v1/calls/{call_id}/rtt per device (callrtt.go; task 8c).
	rtts rttLimiter

	// sweepMu keeps room sweeps from overlapping and guards sweepCursor, the last room a sweep pass
	// visited, where the next one goes on. sweepEvery is the retry loop's sweep cadence.
	sweepMu     sync.Mutex
	sweepCursor string
	sweepEvery  time.Duration

	// cutMu guards the cut-request set (CutDevice, CutUser): device and user ids the retry loop cuts
	// from every live call on its own goroutine. cutWake wakes it (one pending wake is enough);
	// cutPasses counts the passes that took a non-empty set.
	cutMu      sync.Mutex
	cutDevices map[id.ID]bool
	cutUsers   map[id.ID]bool
	cutWake    chan struct{}
	cutPasses  atomic.Int64
	// The rest of the work queue (callqueue.go), also under cutMu: queued grant syncs, evictions by
	// room, teardowns of ended calls by room, and resyncAll, raised when a request did not fit.
	syncs     map[syncKey]bool
	evictions map[string]*queuedEviction
	teardowns map[string]queuedTeardown
	resyncAll bool

	// events and dsvc are set by NewCallEvents (dilla-media task 12): a call that ends closes its
	// call group through the delivery service — which also drops the group's cached state — and
	// announces voice_state 0 to the devices the SFU's webhooks reported in the room. Without them
	// (a test, an instance built without the call events) the group is closed on the store.
	events *CallEvents
	dsvc   DS

	// relay is the TURN relay's revocation state (task 13 review I1): a cut device's relay
	// allocations end with its call session. nil when the instance runs no relay.
	relay RelayRevoker

	// memMu guards the call-group membership state (callmembers.go): the memberships devices were
	// admitted to rooms with, the absences the sweep tracks, the Removes it decided and when it last
	// proposed each, and the first consecutive bot-only observation.
	memMu        sync.Mutex
	admissions   map[admitKey][]admission
	absentSince  map[memberKey]time.Time
	botOnlySince map[id.ID]time.Time
	leafProposed map[memberKey]time.Time
	leafRemovals []leafRemoval

	// ringer rings the other participants of a DM or group-DM call a start opens (web-3, L-GW-33);
	// nil rings nobody.
	ringer *Ringer

	// sfuClk is the clock the SFU stamps a participant's join with (WithSFUClock); nil means clk.
	sfuClk clock.Clock

	// startHook, set by a test only, runs at named points of start (startStage); endHook likewise at
	// named points of endCall.
	startHook func(stage string)
	endHook   func(stage string)

	// AFK state is per device: activity in another tab or device cannot reset its voice timer.
	// afkPending is the moves the sweep made, which only the sweep writes (R33); afkExempt is the
	// room each device that reported itself exempt (a phone, R5) is never moved out of.
	afkMu      sync.Mutex
	afkTracked map[id.ID]afkTracked
	afkPending map[id.ID]afkPendingEntry
	afkExempt  map[id.ID]string
	// afkCutRetries is each user whose chosen-AFK cut did not finish (a failed SFU listing or
	// removal; final review I3), with the retries made so far. AFKMaintenance retries the cut while
	// the user is still away, at most afkCutRetryMax times. Under afkMu.
	afkCutRetries map[id.ID]int
	afkNotify     *StructureNotifier
	// afkAdmitSeq numbers every admission (afkaway.go); afkAdmits is each device's latest one per
	// user, which the away mark checks under afkMu so that an admission newer than the decision
	// wins (R-AW7) and a device still joining keeps its user busy (R-AW2).
	afkAdmitSeq uint64
	afkAdmits   map[id.ID]map[id.ID]afkAdmit

	// awayMu serialises every write of the stored away roster (afkaway.go). It is taken before
	// afkMu, awayIdxMu and awayQMu, never inside them, and nothing waits on a send or an audience
	// read while holding it.
	awayMu        sync.Mutex
	awayGW        VoiceGateway
	awayConnected func(id.ID) bool
	awayDraining  func() bool
	// awayIdxMu guards awaySet, the in-memory index of who may be away, and awaySeeded: readers
	// (every typing op, upload and post) take the read lock alone, writers the write lock briefly,
	// with no I/O under it.
	awayIdxMu  sync.RWMutex
	awaySet    map[id.ID]map[id.ID]struct{}
	awaySeeded bool
	// awayQMu guards the op 55 FIFO the writers append to under awayMu and the drainer goroutine
	// sends from with no lock held; awaySending is set while the drainer sends a batch.
	awayQMu     sync.Mutex
	awayQueue   []awayNote
	awaySending bool
	awayWake    chan struct{}
	// afkCtx is cancelled by StopAFK; afkWG tracks the drainer and the maintenance passes, and
	// afkStopped refuses new ones after StopAFK. drainerOn is set once the drainer runs.
	afkCtx     context.Context
	afkCancel  context.CancelFunc
	afkWG      sync.WaitGroup
	afkLifeMu  sync.Mutex
	afkStopped bool
	drainerOn  bool
	// awayGraceUntil is when the boot grace ends (R-AW5); awayGraceDone is set once the rows of
	// users with no connection were cleared after it; seedNextAt and seedBackoff pace the retries of
	// a failed seed (R5). All under awayMu.
	awayGraceUntil time.Time
	awayGraceDone  bool
	seedNextAt     time.Time
	seedBackoff    time.Duration
	// awayEndPending is set while a boot-time AFK-call end is unfinished (M4); awayMaintaining
	// keeps AFKMaintenance to one pass at a time.
	awayEndPending  atomic.Bool
	awayMaintaining atomic.Bool
	// awayOff is each user whose last gateway connection closed, waiting out afkOfflineGrace
	// before their away rows are cleared (Q2 as revised: a reload or a short blip keeps away). One
	// entry and one timer per user, however often the user reconnects; awayOffMu guards the map and
	// each entry's deadline, and is taken after awayMu, never before it. awayOffWaiters counts the
	// goroutines waiting on those timers.
	awayOffMu      sync.Mutex
	awayOff        map[id.ID]*awayOffline
	awayOffWaiters atomic.Int32
	// afkHook, set by a test only, runs at named points of the AFK paths.
	afkHook func(stage string)
}

// WithRinger sets the DM call ring and returns h.
func (h *Calls) WithRinger(r *Ringer) *Calls {
	h.ringer = r
	return h
}

// RelayRevoker cuts a device from the TURN relay as of at; *server.RelayRevocations is one. Revoke
// closes sockets and writes a map, and never blocks on the store or the SFU. Mint records a relay
// credential minted for device by a request that began at since and answers its issue time, the
// clock's, chosen under the lock Revoke takes (and makes a later cut of the device worth
// recording); when the device was cut in the current millisecond or since the request began it
// mints nothing and answers how long to wait (wait > 0).
type RelayRevoker interface {
	Revoke(device id.ID, at time.Time)
	Mint(device id.ID, since time.Time) (issued time.Time, wait time.Duration)
}

// WithSFUClock sets the clock the SFU stamps a participant's join with and returns h. The
// in-process LiveKit reads the process's wall clock whatever the instance's clock is; a session's
// leave is bound to its admission by comparing the two stamps, so they must come from one clock.
// Unset, the instance's clock stands in (an instance whose clock is the wall clock, a test).
//
// An SFU on another host stamps joins with that host's clock: keep both hosts on NTP, or a leave binds
// to no admission and the room sweep removes the leaf instead, after admissionJoinWindow.
func (h *Calls) WithSFUClock(c clock.Clock) *Calls {
	h.sfuClk = c
	return h
}

// sfuNow is the SFU clock's now (WithSFUClock), else the instance clock's.
func (h *Calls) sfuNow() time.Time {
	if h.sfuClk != nil {
		return h.sfuClk.Now()
	}
	return h.clk.Now()
}

// WithRelay sets the relay revocation state the cuts feed and returns h.
func (h *Calls) WithRelay(r RelayRevoker) *Calls {
	h.relay = r
	return h
}

// revokeRelay cuts device from the relay now, when there is one.
func (h *Calls) revokeRelay(device id.ID) {
	if h.relay != nil {
		h.relay.Revoke(device, h.clk.Now())
	}
}

// BarredDevices is the relay's barred lookup over the store (server.BarredLookup): what the call
// routes call barred. An id with no device row is not barred: device rows are never deleted (a
// revoked device keeps its row), the call routes mint relay credentials only for a session's device,
// and a credential naming any other id can be minted only by a holder of turn.shared_secret_file —
// `dillad doctor`'s allocation probe, which uses a fresh id.
type BarredDevices struct{ Repo store.Repository }

// DeviceBarred reports whether device may no longer take part in calls.
func (b BarredDevices) DeviceBarred(ctx context.Context, device id.ID) (bool, error) {
	dv, err := b.Repo.GetDevice(ctx, device)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return barred(ctx, b.Repo, dv)
}

// NewCalls wires the call routes over the SFU.
func NewCalls(repo store.Repository, res *Resolver, sfu CallTokens, cfg CallsConfig, clk clock.Clock, log *slog.Logger) *Calls {
	afkCtx, afkCancel := context.WithCancel(context.Background())
	return &Calls{repo: repo, res: res, sfu: sfu, cfg: cfg, clk: clk, log: log, leases: newShareLeases(),
		sweepEvery: RoomSweepInterval, cutWake: make(chan struct{}, 1),
		awayWake: make(chan struct{}, 1), afkCtx: afkCtx, afkCancel: afkCancel}
}

// WithCounters sets the label-free counters the routes move and returns h.
func (h *Calls) WithCounters(c CallCounters) *Calls {
	h.counters = c
	return h
}

func (h *Calls) Register(mux *server.Mux) {
	h.registerAFK(mux)
	mux.HandleFunc("POST /v1/channels/{id}/calls", h.start)
	mux.HandleFunc("DELETE /v1/calls/{call_id}", h.end)
	mux.HandleFunc("POST /v1/calls/{call_id}/share", h.share)
	mux.HandleFunc("DELETE /v1/calls/{call_id}/share", h.unshare)
	mux.HandleFunc("POST /v1/calls/{call_id}/rtt", h.rtt)
	mux.HandleFunc("POST /v1/calls/{call_id}/decline", h.decline)
	h.registerReads(mux)
}

// callResponse is `[call_id, group_id, livekit_url, token, ice_servers, caps]`.
type callResponse struct {
	_          struct{} `cbor:",toarray"`
	CallID     id.ID
	GroupID    id.ID
	LiveKitURL string
	Token      string
	ICEServers []iceServer
	Caps       callCaps
}

// callRungResponse is callResponse with a seventh element, `rung([user_id])`: the users the start
// rang. Only a start that opens a DM or group-DM call answers it (WB product walk I6), so the caller
// shows "ringing" only for a ring that went out; any other start keeps the six elements.
type callRungResponse struct {
	_          struct{} `cbor:",toarray"`
	CallID     id.ID
	GroupID    id.ID
	LiveKitURL string
	Token      string
	ICEServers []iceServer
	Caps       callCaps
	Rung       []id.ID
}

// callCaps is `[max_audio_bitrate_bps, max_share_bitrate_bps, vp9]` (DEV-26, MD-10).
type callCaps struct {
	_                  struct{} `cbor:",toarray"`
	MaxAudioBitrateBPS uint64
	MaxShareBitrateBPS uint64
	VP9                uint64
}

// iceServer is `[urls([tstr]), username(tstr), credential(tstr)]`: the three
// members of an RTCIceServer a client passes to its peer connection.
type iceServer struct {
	_          struct{} `cbor:",toarray"`
	URLs       []string
	Username   string
	Credential string
}

// start is POST /v1/channels/{id}/calls `[]` or `[vdec]`: 201 with a fresh call, or 200 joining the
// call already live in the channel's call group.
//
// The gate is the spec's: a token is minted only for a device whose leaf is present in the call
// group's CURRENT epoch — added at or before it and not removed. A device that has not yet joined
// the group's current epoch and a removed one are refused E_LEAF_NOT_CURRENT, so a device that
// could not decrypt the call's media keys cannot join its room. Then the room is opened in the SFU
// (DEV-44), the advisory participant count may refuse E_CALL_FULL (DEV-01), and the token carries
// the base grant only (baseGrant: the microphone for speak, never a camera or screen source, even
// for a device that holds a sharing slot). Camera and screen arrive only as the push a share makes
// (DEV-02, DEV-03), so a token replayed after an unshare or a demotion confers nothing more; a
// reconnecting sharer re-POSTs share, which is idempotent for a slot it holds.
func (h *Calls) start(w http.ResponseWriter, r *http.Request) {
	begun := h.clk.Now()
	s, ch, bits, err := h.channel(r)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	// R-AW1: the AFK channel never hosts a call; being there means having no voice connection.
	if h.isAFKChannel(r.Context(), ch) {
		server.WriteError(w, errAFKCall())
		return
	}
	var body []cbor.RawMessage
	if err := server.DecodeBody(w, r, maxCBORBody, &body); err != nil {
		server.WriteError(w, err)
		return
	}
	vdec, err := decodeVdec(body)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	group, err := h.callGroup(r.Context(), ch)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	// R9: the call is keyed by the call group's call id, which every call group of the channel
	// shares. While a call is live, the group it was opened on (voice_sessions.group_id) is the one
	// its token gate reads.
	callID := callIDOfGroup(group)
	now := h.clk.Now().Unix()
	prev, err := h.repo.GetVoiceSession(r.Context(), callID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		server.WriteError(w, err)
		return
	}
	live := err == nil && prev.Ended == nil
	endStale := false
	if live && prev.GroupID != nil && *prev.GroupID != group.GroupID {
		recorded, gerr := h.repo.GetGroup(r.Context(), *prev.GroupID)
		switch {
		case gerr == nil && recorded.ClosedAt == nil:
			group = recorded
		case gerr == nil || errors.Is(gerr, store.ErrNotFound):
			// The live call's group is closed or gone, so that call cannot go on: once this request
			// passes the gates it ends it and opens the next call on the channel's newest call group.
			live, endStale = false, true
		default:
			server.WriteError(w, gerr)
			return
		}
	}
	if err := h.requireCurrentLeaf(r.Context(), group, s.DeviceID); err != nil {
		server.WriteError(w, err)
		return
	}
	// A revoked or quarantined device, or one of a disabled or deleted user, gets no token.
	if err := h.refuseBarred(r.Context(), h.repo, s.DeviceID); err != nil {
		server.WriteError(w, err)
		return
	}
	if err := requireCommunityWire(r.Context(), h.repo, s.WireVersion, ch.CommunityID); err != nil {
		server.WriteError(w, err)
		return
	}
	// An instance with livekit.enabled = false has no SFU to mint a room token from: every gate
	// above answers as it does elsewhere, and a call that would open is 501, before any voice
	// session is recorded for a room nobody can join.
	if h.sfu == nil {
		server.WriteError(w, notImplemented("this instance runs no SFU (livekit.enabled is false)"))
		return
	}
	if endStale {
		if err := h.endCall(context.WithoutCancel(r.Context()), callID, prev.LivekitRoom, "stale_group"); err != nil {
			server.WriteError(w, err)
			return
		}
	}

	// PutVoiceSession leaves a live call as it is, so the row read back afterwards is the one call
	// every device of this group lands in, however many start it at once.
	groupID := group.GroupID
	// A fresh room per call, so a device from the previous call cannot linger in this one. The random
	// suffix keeps a call that ends and restarts within one second from reusing its predecessor's
	// room name, which an end's room deletion or the sweep would otherwise take for the new room's.
	room := fmt.Sprintf("%s-%d-%s", callID, now, id.New().String()[:8])
	status := http.StatusCreated
	if live {
		status = http.StatusOK
	}
	h.startStage("put")
	if err := h.repo.PutVoiceSession(r.Context(), store.VoiceSessionRow{
		CallID: callID, ChannelID: ch.ID, GroupID: &groupID, LivekitRoom: room, Started: now,
	}); err != nil {
		server.WriteError(w, err)
		return
	}
	row, err := h.repo.GetVoiceSession(r.Context(), callID)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	// The row may name a group an end has closed since this start read it (CALLS-2): the write above
	// reopens an ended row, and an end closes the call group BEFORE it ends the row (endCall), so a
	// start whose write landed after an end finds the group closed here. Such a call cannot go on — no
	// commit of a closed group lands, so no leaf could ever be added or removed — and it is ended at
	// once; the client retries, and the retry opens on the channel's newest open call group (or is
	// 404 until one is registered).
	var kept store.GroupRow
	if row.GroupID != nil {
		kept, err = h.repo.GetGroup(r.Context(), *row.GroupID)
		switch {
		case errors.Is(err, store.ErrNotFound) || (err == nil && kept.ClosedAt != nil):
			if err := h.endCall(context.WithoutCancel(r.Context()), callID, row.LivekitRoom, "closed_group"); err != nil {
				server.WriteError(w, err)
				return
			}
			server.WriteError(w, server.Unavailable(startRetryAfterMS, "the call ended as it was opened; retry"))
			return
		case err != nil:
			server.WriteError(w, err)
			return
		}
	}
	// Another device may have opened the call on another call group of the channel between the read
	// above and the write: the room belongs to the group the row names.
	if row.GroupID != nil && *row.GroupID != groupID {
		if err := h.requireCurrentLeaf(r.Context(), kept, s.DeviceID); err != nil {
			server.WriteError(w, err)
			return
		}
		groupID = kept.GroupID
	}
	// DEV-44: the SFU never creates a room on a join (room.auto_create: false), so every start opens
	// it first. CreateRoom is idempotent on a live room, and re-opens one LiveKit reaped (20 s after
	// the last leave, 300 s when nobody joined) while the row was still live.
	if err := h.sfu.CreateRoom(r.Context(), row.LivekitRoom); err != nil {
		h.log.ErrorContext(r.Context(), "opening a LiveKit room failed", "room", row.LivekitRoom, "err", err)
		server.WriteError(w, server.Errorf(server.CodeInternal, "the SFU could not open the room"))
		return
	}
	// A device whose cut or demotion in this call has not landed in the SFU gets no token for it until
	// the repair does; the start drives that device's own repair first.
	if err := h.retryOwn(r.Context(), callID, s.DeviceID); err != nil {
		server.WriteError(w, err)
		return
	}
	if h.leases.isPending(callID, s.DeviceID) {
		server.WriteError(w, server.Errorf(server.CodeForbidden, "your device's access to this call is being revoked"))
		return
	}
	if h.callFull(r.Context(), row.LivekitRoom, s.DeviceID) {
		server.WriteError(w, server.Errorf(server.CodeCallFull, "the call is full"))
		return
	}
	var attrs map[string]string
	if vdec != "" {
		attrs = map[string]string{vdecAttribute: vdec}
	}
	perm, err := h.admitGrant(r.Context(), bits, row, s.DeviceID)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	token, err := h.sfu.Token(row.LivekitRoom, s.DeviceID.String(), perm, attrs)
	if err != nil {
		h.log.ErrorContext(r.Context(), "minting a LiveKit token failed", "err", err)
		server.WriteError(w, server.Errorf(server.CodeInternal, "the SFU could not mint a token"))
		return
	}
	h.startStage("mint")
	// The token opens the device's join window (the sweep leaves its leaf alone while it connects).
	h.noteAdmission(r.Context(), row, s.DeviceID, false)
	ice, minted, err := h.iceServers(s.DeviceID, begun)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	if minted {
		// The gates above ran before the credential was minted: a device barred or demoted in
		// between must not leave with it (commit review, mint race). The mint is ordered against
		// every relay cut, and refuses after a cut made while this start was served
		// (RelayRevoker.Mint), so what remains is re-reading the entitlement. A refusal here cuts
		// the relay as of now, which covers the credential just minted and moves no one else's.
		if err := h.stillEntitled(r.Context(), s, ch, groupID); err != nil {
			h.revokeRelay(s.DeviceID)
			server.WriteError(w, err)
			return
		}
	}
	h.startStage("respond")
	// L-GW-33: only a start that opens the call rings; a 200 start of a live call never does, and
	// neither does a start that read the call not live but lost the race to open it (its row names
	// another start's room; task-3 review M2).
	var answer any = callResponse{
		CallID: callID, GroupID: groupID, LiveKitURL: h.cfg.LiveKitURL, Token: token,
		ICEServers: ice, Caps: h.caps(),
	}
	if status == http.StatusCreated && row.LivekitRoom == room && (ch.Kind == ChannelDM || ch.Kind == ChannelGroupDM) {
		rung := h.ring(r.Context(), ch, row, s.UserID)
		if rung == nil {
			rung = []id.ID{}
		}
		answer = callRungResponse{
			CallID: callID, GroupID: groupID, LiveKitURL: h.cfg.LiveKitURL, Token: token,
			ICEServers: ice, Caps: h.caps(), Rung: rung,
		}
	}
	if err := server.EncodeBody(w, status, answer); err != nil {
		h.log.WarnContext(r.Context(), "write call response", "err", err)
	}
}

// end is DELETE /v1/calls/{call_id}: 204 once the call is over, including when
// it already was. Ending a call ends it for everyone, so the caller must be a
// current leaf of the call's group — a participant, not merely someone who may
// see the channel — whose user still holds connect. With ?if_alone=1 it ends a
// live call only while the caller's device is the call group's only current
// leaf, decided under the group lock (409 E_CALL_NOT_ALONE otherwise): a person
// who leaves believing they are alone never ends the call of a device that has
// just joined (web-3 task 10 fix, f10app I1).
func (h *Calls) end(w http.ResponseWriter, r *http.Request) {
	s, err := enrolledSession(r)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	callID, err := server.PathID(r, "call_id")
	if err != nil {
		server.WriteError(w, err)
		return
	}
	ifAlone, err := ifAloneParam(r)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	row, err := h.repo.GetVoiceSession(r.Context(), callID)
	if err != nil {
		server.WriteError(w, notFound(err))
		return
	}
	ch, err := h.repo.GetChannel(r.Context(), row.ChannelID)
	if err != nil {
		server.WriteError(w, notFound(err))
		return
	}
	bits, err := h.res.Resolve(r.Context(), s.UserID, ch)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	if !bits.Has(PermViewChannel) {
		server.WriteError(w, server.Errorf(server.CodeNotFound, "no such object"))
		return
	}
	if row.Ended != nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	// A leaf whose user lost connect is being cut from the call; it does not end it for everyone.
	if !bits.Has(PermConnect) {
		server.WriteError(w, server.Errorf(server.CodeForbidden, "missing permission"))
		return
	}
	if row.GroupID == nil {
		server.WriteError(w, server.Errorf(server.CodeLeafNotCurrent, "the call has no group to be a leaf of"))
		return
	}
	group, err := h.repo.GetGroup(r.Context(), *row.GroupID)
	if err != nil {
		server.WriteError(w, notFound(err))
		return
	}
	// A call whose group is closed is over already (nobody is a leaf of it, so nobody is admitted to
	// its room): the DELETE only finishes its end. Of a call that can go on, only a current leaf may
	// end it.
	closed := group.ClosedAt != nil
	if !closed {
		if err := h.requireCurrentLeaf(r.Context(), group, s.DeviceID); err != nil {
			server.WriteError(w, err)
			return
		}
	}
	// A barred device (revoked, quarantined, of a disabled or deleted user) ends no call, as it
	// starts, shares and joins none (CALLS-4); neither does a device penalised in the call's room for
	// media that is not dilla's (F11), whose penalty ends with the room (CALLS-M1).
	if err := h.refuseBarred(r.Context(), h.repo, s.DeviceID); err != nil {
		server.WriteError(w, err)
		return
	}
	if h.leases.penalised(callID, row.LivekitRoom, s.DeviceID) {
		server.WriteError(w, server.Errorf(server.CodeForbidden, "your device published media that is not dilla-sframe/1 in this call"))
		return
	}
	if !closed {
		if err := requireCommunityWire(r.Context(), h.repo, s.WireVersion, ch.CommunityID); err != nil {
			server.WriteError(w, err)
			return
		}
	}
	// The conditional form closes the group first, under its lock and only while the caller is its
	// only current non-bot leaf; endCall then finds it closed and finishes the end. A joiner whose commit
	// comes after the close is refused for the closed group and registers a fresh one.
	if ifAlone && !closed {
		if err := h.closeIfAlone(r.Context(), group, s.DeviceID); err != nil {
			server.WriteError(w, err)
			return
		}
	}
	// protocol/09: DELETE ends the call for everyone — the room is closed, every participant still in
	// it disconnected, and the call group closed, so the next call registers a fresh one. The next
	// call keeps this call id (R9), so the sharing slots go with this one.
	if err := h.endCall(context.WithoutCancel(r.Context()), callID, row.LivekitRoom, "delete"); err != nil {
		server.WriteError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ifAloneParam reads DELETE /v1/calls/{call_id}'s one optional query parameter: absent, or
// if_alone=1 exactly once.
func ifAloneParam(r *http.Request) (bool, error) {
	q := r.URL.Query()
	if !q.Has("if_alone") {
		return false, nil
	}
	if v := q["if_alone"]; len(v) != 1 || v[0] != "1" {
		return false, server.Errorf(server.CodeInvalidRequest, "if_alone takes the one value 1")
	}
	return true, nil
}

// closeIfAlone closes the call's group while dev is its only current non-bot leaf: through the delivery
// service, under the group lock, when the call events handed one over; on the store otherwise (no
// commit lands there without a delivery service).
func (h *Calls) closeIfAlone(ctx context.Context, group store.GroupRow, dev id.ID) error {
	ctx = context.WithoutCancel(ctx)
	var err error
	if h.dsvc != nil {
		err = h.dsvc.CloseIfAlone(ctx, group.GroupID, dev)
	} else {
		var members []store.MemberRow
		if members, err = h.repo.ListMembers(ctx, group.GroupID); err == nil {
			var sole bool
			if sole, err = ds.SoleLeaf(ctx, h.repo, members, group.Epoch, dev); err != nil {
				return err
			}
			if !sole {
				err = ds.ErrNotAlone
			} else {
				err = h.repo.CloseGroup(ctx, group.GroupID, h.clk.Now().Unix())
			}
		}
	}
	if errors.Is(err, ds.ErrNotAlone) {
		return server.Errorf(server.CodeCallNotAlone, "another device is in the call")
	}
	return err
}

// closeRoom closes an ended call's LiveKit room. The call is already over in the record, so a
// failure is logged and never turns the answer into an error. A failure matters: LiveKit re-mints a
// connected participant's token every five minutes, so a room that stays open keeps its
// participants until a later DeleteRoom; room.auto_create is false, so nobody can rejoin it once it
// is gone (the room sweep retries a close that failed). The close runs on a context detached from
// the caller's, so a client that hangs up once it has sent the DELETE cannot leave the room open.
func (h *Calls) closeRoom(ctx context.Context, room string) {
	if h.sfu == nil || room == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), roomCloseTimeout)
	defer cancel()
	if err := h.sfu.DeleteRoom(ctx, room); err != nil {
		h.log.WarnContext(ctx, "closing an ended call's LiveKit room failed", "room", room, "err", err)
	}
}

// endCall ends one call (DEV-46, gap G30): DELETE /v1/calls/{call_id}, the room_finished webhook,
// and a start that finds the live call's group closed all come here. It is keyed on the room: an
// event for an older room of the same call, or for a call already over, changes nothing. In order it
// closes the call group — so the next call registers a fresh group and no device of this one stays a
// leaf of it, instead of a Remove for every leaf that no member could commit (OpenMLS refuses a
// commit removing its own committer) — then ends the voice session if it is still this call (endRow),
// drops every sharing slot, repair and F11 penalty of the room under the call's lock, deletes the
// room (disconnecting everyone still in it) and announces voice_state 0 for every device the call
// events saw in the room. The group is closed FIRST (CALLS-2): a start that read it open and writes
// the row after this end reopens it on a group that is closed by then, which the start's own
// re-read finds and ends; and both gates hold no leaf in a closed group. It is closed before the
// call's lock is taken: nothing under a call's lock calls the delivery service. An end that finds
// the call ended by another in between leaves the rest to that one.
func (h *Calls) endCall(ctx context.Context, callID id.ID, room string, reason string) error {
	row, err := h.repo.GetVoiceSession(ctx, callID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if row.Ended != nil || row.LivekitRoom != room {
		return nil
	}
	if row.GroupID != nil {
		h.closeCallGroup(ctx, *row.GroupID)
	}
	h.endStage("closed")
	ended, err := h.endRow(ctx, row)
	if err != nil || !ended {
		return err
	}
	h.ringer.End(callID)
	h.dropRoom(ctx, row)
	h.closeRoom(ctx, room)
	if h.events != nil {
		h.events.ended(ctx, row)
	}
	h.log.InfoContext(ctx, "call ended", "call_id", callID.String(), "reason", reason)
	return nil
}

// endStage runs the test hook at a named point of endCall ("closed"); nil in production.
func (h *Calls) endStage(stage string) {
	if f := h.endHook; f != nil {
		f(stage)
	}
}

// closeCallGroup closes an ended call's group, through the delivery service when the call events
// handed one over and on the store otherwise. A group already closed (the stale-group path) is left
// as it is; a failure is logged, because the call is over in the record either way.
func (h *Calls) closeCallGroup(ctx context.Context, groupID id.ID) {
	g, err := h.repo.GetGroup(ctx, groupID)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			h.log.WarnContext(ctx, "reading an ended call's group failed", "group_id", groupID.String(), "err", err)
		}
		return
	}
	if g.ClosedAt != nil {
		return
	}
	if h.dsvc != nil {
		err = h.dsvc.Close(ctx, groupID)
	} else {
		err = h.repo.CloseGroup(ctx, groupID, h.clk.Now().Unix())
	}
	if err != nil {
		h.log.WarnContext(ctx, "closing an ended call's group failed", "group_id", groupID.String(), "err", err)
	}
}

// channel is the {id} a caller may place a call in: a channel it can see (404 otherwise, as for an
// unknown one), that carries calls (400 otherwise) and in which it holds connect (403 otherwise),
// with the caller's resolved bits there.
func (h *Calls) channel(r *http.Request) (auth.Session, store.ChannelRow, Bits, error) {
	s, err := enrolledSession(r)
	if err != nil {
		return s, store.ChannelRow{}, 0, err
	}
	chID, err := server.PathID(r, "id")
	if err != nil {
		return s, store.ChannelRow{}, 0, err
	}
	ch, err := h.repo.GetChannel(r.Context(), chID)
	if err != nil {
		return s, store.ChannelRow{}, 0, notFound(err)
	}
	if err := h.botCanUseSession(r.Context(), s, ch); err != nil {
		return s, store.ChannelRow{}, 0, err
	}
	bits, err := h.res.Resolve(r.Context(), s.UserID, ch)
	if err != nil {
		return s, store.ChannelRow{}, 0, err
	}
	if !bits.Has(PermViewChannel) {
		return s, store.ChannelRow{}, 0, server.Errorf(server.CodeNotFound, "no such object")
	}
	if !CallGroupAllowed(ch) {
		return s, store.ChannelRow{}, 0, server.Errorf(server.CodeInvalidRequest, "this channel carries no calls")
	}
	if !bits.Has(PermConnect) {
		return s, store.ChannelRow{}, 0, server.Errorf(server.CodeForbidden, "missing permission")
	}
	return s, ch, bits, nil
}

// callGroup is the channel's open call group, the newest when a restore's
// re-creation left more than one open. Its clients register it; with none
// registered yet there is nothing to gate a token on.
func (h *Calls) callGroup(ctx context.Context, ch store.ChannelRow) (store.GroupRow, error) {
	groups, err := h.repo.GroupsForTarget(ctx, ch.ID, GroupCall)
	if err != nil {
		return store.GroupRow{}, err
	}
	if len(groups) == 0 {
		return store.GroupRow{}, server.Errorf(server.CodeNotFound, "no call group is registered for this channel")
	}
	return groups[len(groups)-1], nil
}

// requireCurrentLeaf is the epoch gate: dev holds a leaf of group that was
// added at or before the group's current epoch and has not been removed, in a group that is neither
// closed nor epoch-unknown (leafIn).
func (h *Calls) requireCurrentLeaf(ctx context.Context, group store.GroupRow, dev id.ID) error {
	switch {
	case group.ClosedAt != nil:
		// No commit of a closed group lands (CALLS-2): its member list is final, and a call on it is
		// over.
		return server.Errorf(server.CodeLeafNotCurrent, "the call group is closed")
	case group.EpochUnknown:
		// After a restore the stored epoch is not known to be the group's, so
		// neither is its member list; the group heals first.
		return server.Errorf(server.CodeLeafNotCurrent, "the call group is epoch-unknown until a member heals it")
	}
	leaf, err := leafIn(ctx, h.repo, group, dev)
	if err != nil {
		return err
	}
	if !leaf {
		return server.Errorf(server.CodeLeafNotCurrent, "your device is not a leaf of this call group's current epoch")
	}
	return nil
}

// leafIn is the leaf rule both gates (requireCurrentLeaf, leafOfCall) and the room's reconcile share:
// dev holds a leaf of group added at or before its current epoch and not removed, and the group is
// neither closed (no commit of it lands any more, so neither an Add nor a leave's Remove could) nor
// epoch-unknown (its member list is not known to be current).
func leafIn(ctx context.Context, repo store.Repository, group store.GroupRow, dev id.ID) (bool, error) {
	if group.ClosedAt != nil || group.EpochUnknown {
		return false, nil
	}
	members, err := repo.ListMembers(ctx, group.GroupID)
	if err != nil {
		return false, err
	}
	for _, m := range members {
		if m.DeviceID == dev && m.RemovedEpoch == nil && m.AddedEpoch <= group.Epoch {
			return true, nil
		}
	}
	return false, nil
}

// callIDOfGroup is R9's companion column: the call id the delivery service
// recorded when the call group was registered. A call group always has one;
// the group id stands in only for a row written without it.
func callIDOfGroup(g store.GroupRow) id.ID {
	if g.CallID != nil {
		return *g.CallID
	}
	return g.GroupID
}

// iceServers is the relay list for dev: one entry carrying a fresh REST
// credential ("<expiry>:<device_id>:<issued>", HMAC-SHA1 under the TURN secret), or an
// empty list when TURN is off — in behind_proxy without turn.listen the client
// shows its "relay unavailable" dialog and the call is UDP or nothing.
//
// The issue time comes from the relay's Mint, chosen under the lock its cuts take, so a cut is
// ordered either before the credential (which is then newer than it) or after it (which then covers
// it). A device cut in the current millisecond, or since the start began (begun), gets no credential:
// a transient 429 E_RATE_LIMITED with the few milliseconds to wait in retry_after_ms, after which
// the start's own gates answer whether the cut still applies. minted reports whether a credential
// was minted.
func (h *Calls) iceServers(dev id.ID, begun time.Time) (servers []iceServer, minted bool, err error) {
	if h.cfg.TURNSecret == "" || len(h.cfg.TURNURLs) == 0 {
		return []iceServer{}, false, nil
	}
	ttl := h.cfg.CredentialTTL
	if ttl <= 0 {
		ttl = time.Hour
	}
	issued := h.clk.Now()
	if h.relay != nil {
		var wait time.Duration
		if issued, wait = h.relay.Mint(dev, begun); wait > 0 {
			// This 429 can follow CreateRoom and admission writes; call start is
			// retry-safe, so replaying the same start remains safe.
			e := server.RateLimitedAfter(wait)
			e.Detail = "your device was cut from the relay a moment ago; retry"
			return nil, false, e
		}
	}
	user, pass := server.TURNCredential(h.cfg.TURNSecret, dev, ttl, issued)
	return []iceServer{{URLs: h.cfg.TURNURLs, Username: user, Credential: pass}}, true, nil
}

// stillEntitled re-reads, after a relay credential was minted, what start checked before it: the
// device not barred, a current leaf of the call's group, view_channel and connect. It answers the
// refusal the first check would have.
func (h *Calls) stillEntitled(ctx context.Context, s auth.Session, ch store.ChannelRow, groupID id.ID) error {
	if err := h.botCanUseSession(ctx, s, ch); err != nil {
		return err
	}
	if err := h.refuseBarred(ctx, h.repo, s.DeviceID); err != nil {
		return err
	}
	group, err := h.repo.GetGroup(ctx, groupID)
	if err != nil {
		return notFound(err)
	}
	if err := h.requireCurrentLeaf(ctx, group, s.DeviceID); err != nil {
		return err
	}
	bits, err := h.res.Resolve(ctx, s.UserID, ch)
	if err != nil {
		return err
	}
	if !bits.Has(PermViewChannel) {
		return server.Errorf(server.CodeNotFound, "no such object")
	}
	if !bits.Has(PermConnect) {
		return server.Errorf(server.CodeForbidden, "missing permission")
	}
	return nil
}

// botCanUseChannel re-reads the installation/runtime for every call gate and
// room sweep. A bot can use calls only in its own ready community; humans keep
// the existing permission path unchanged.
func (h *Calls) botCanUseChannel(ctx context.Context, userID id.ID, ch store.ChannelRow) error {
	if ch.CommunityID == nil {
		bot, err := botaccess.IsBotUser(ctx, h.repo, userID)
		if err != nil {
			return err
		}
		if bot {
			return server.Errorf(server.CodeForbidden, "managed bots cannot join direct-message calls")
		}
		return nil
	}
	binding, bot, err := botaccess.ForUserCommunity(ctx, h.repo, userID, *ch.CommunityID)
	if err != nil {
		if errors.Is(err, botaccess.ErrIneligible) {
			return server.Errorf(server.CodeForbidden, "managed bot installation is not eligible for this call")
		}
		return err
	}
	if bot && (binding.State != store.BotInstallationReady || binding.CommunityID != *ch.CommunityID) {
		return server.Errorf(server.CodeForbidden, "managed bot installation is not ready for this call")
	}
	return nil
}

func (h *Calls) botCanUseSession(ctx context.Context, session auth.Session, ch store.ChannelRow) error {
	if ch.CommunityID == nil {
		bot, err := botaccess.IsBotUser(ctx, h.repo, session.UserID)
		if err != nil {
			return err
		}
		if bot || session.Bot != nil {
			return server.Errorf(server.CodeForbidden, "managed bots cannot join direct-message calls")
		}
		return nil
	}
	binding, bot, err := botaccess.ForUserCommunity(ctx, h.repo, session.UserID, *ch.CommunityID)
	if err != nil {
		if errors.Is(err, botaccess.ErrIneligible) {
			return server.Errorf(server.CodeForbidden, "managed bot installation is not eligible for this call")
		}
		return err
	}
	if !bot {
		if session.Bot != nil {
			return server.Errorf(server.CodeForbidden, "bot session binding is no longer valid")
		}
		return nil
	}
	if session.Bot == nil || binding.State != store.BotInstallationReady || binding.InstallationID != session.Bot.InstallationID ||
		binding.CommunityID != session.Bot.CommunityID || binding.CommunityID != *ch.CommunityID {
		return server.Errorf(server.CodeForbidden, "managed bot installation is not ready for this call")
	}
	return nil
}

// startStage runs the test hook at a named point of start ("mint", "respond"); nil in production.
func (h *Calls) startStage(stage string) {
	if f := h.startHook; f != nil {
		f(stage)
	}
}

// videoDecoders are the names a vdec list may carry (protocol/05's negotiable video codecs).
var videoDecoders = map[string]bool{"vp8": true, "h264": true, "vp9": true}

// decodeVdec is the calls request body: [] or [vdec], vdec being the comma-separated lowercase
// decoders of the requesting device, each at most once (MD-10).
func decodeVdec(body []cbor.RawMessage) (string, error) {
	switch len(body) {
	case 0:
		return "", nil
	case 1:
	default:
		return "", server.Errorf(server.CodeInvalidRequest, "the body is [] or [vdec]")
	}
	var vdec string
	if err := cborx.Unmarshal(body[0], &vdec); err != nil {
		return "", server.Errorf(server.CodeInvalidRequest, "vdec is a text string")
	}
	seen := map[string]bool{}
	for _, name := range strings.Split(vdec, ",") {
		if !videoDecoders[name] || seen[name] {
			return "", server.Errorf(server.CodeInvalidRequest,
				"vdec is a comma-separated list of vp8, h264 and vp9, each at most once")
		}
		seen[name] = true
	}
	return vdec, nil
}

// caps is the calls response's sixth element: the bitrate ceilings a client applies to its publish
// options, in bits per second, and whether VP9 is enabled at all (DEV-26, F4).
func (h *Calls) caps() callCaps {
	c := callCaps{
		MaxAudioBitrateBPS: uint64(max(h.cfg.MaxAudioBitrateKbps, 0)) * 1000,
		MaxShareBitrateBPS: uint64(max(h.cfg.MaxShareBitrateKbps, 0)) * 1000,
	}
	if h.cfg.VP9 {
		c.VP9 = 1
	}
	return c
}

// grantFor is dev's permission in the call: its speak/video/screen_share bits, the video sources
// only while it holds a sharing slot of the call (ruling F1), and nothing while it is penalised in
// the call's room for publishing media that is not dilla-sframe/1 (ruling F11) or the call is the
// built-in AFK channel's (R3, R34).
func (h *Calls) grantFor(ctx context.Context, bits Bits, row store.VoiceSessionRow, dev id.ID) (*livekit.ParticipantPermission, error) {
	if h.leases.penalised(row.CallID, row.LivekitRoom, dev) || h.inAFK(ctx, row) {
		return restrictBotCallGrant(ctx, h.repo, dev, sfu.PublishGrant(false, false, false))
	}
	leased := h.leases.held(row.CallID, row.LivekitRoom, dev)
	return restrictBotCallGrant(ctx, h.repo, dev, sfu.PublishGrant(bits.Has(PermSpeak), bits.Has(PermVideo) && leased, bits.Has(PermScreenShare) && leased))
}

// admitGrant is what dev's token for row's room is minted with and what the /rtc gate admits: the
// base grant, or listen-only while dev is penalised in that room (ruling F11) and in every call of
// the built-in AFK channel (R3, R34).
func (h *Calls) admitGrant(ctx context.Context, bits Bits, row store.VoiceSessionRow, dev id.ID) (*livekit.ParticipantPermission, error) {
	if h.leases.penalised(row.CallID, row.LivekitRoom, dev) || h.inAFK(ctx, row) {
		return restrictBotCallGrant(ctx, h.repo, dev, sfu.PublishGrant(false, false, false))
	}
	return restrictBotCallGrant(ctx, h.repo, dev, baseGrant(bits))
}

// callFull is the advisory E_CALL_FULL count (DEV-01, gap G27): the room's participants other than
// the caller's own device and its "#" shadows (a rejoin replaces the old session before LiveKit's
// own count), disconnected ones, agents and egress. It fails open: LiveKit's room.max_participants
// is the authoritative cap, and a device that passes this count in a race gets LiveKit's upgrade
// refusal instead.
func (h *Calls) callFull(ctx context.Context, room string, dev id.ID) bool {
	limit := h.cfg.MaxVoiceParticipants
	if limit <= 0 {
		return false
	}
	parts, err := h.sfu.Participants(ctx, room)
	if err != nil {
		h.log.WarnContext(ctx, "counting a call's participants failed; minting anyway", "room", room, "err", err)
		return false
	}
	self := dev.String()
	n := 0
	for _, p := range parts {
		identity := p.GetIdentity()
		if identity == self || strings.HasPrefix(identity, self+"#") {
			continue
		}
		if p.GetState() == livekit.ParticipantInfo_DISCONNECTED {
			continue
		}
		if k := p.GetKind(); k == livekit.ParticipantInfo_AGENT || k == livekit.ParticipantInfo_EGRESS {
			continue
		}
		n++
	}
	if n < limit {
		return false
	}
	if h.counters != nil {
		h.counters.CallFull()
	}
	return true
}

// AdmitRoom is the /rtc join gate (DEV-25, DEV-44): room must be the live room of a call —
// "<call_id hex>-<unix>-<8 hex>", equal to voice_sessions.livekit_room with the call not ended — device a
// current leaf of the call group that room was opened on, neither it nor its user may be barred
// (barred: revoked, quarantined, disabled or deleted), its user must hold view_channel and connect
// in the call's channel now, and no cut or demotion of it may be pending in the call. It answers the
// device's current base permission — the microphone when it holds speak, never a camera or screen
// source, exactly what a token is minted with — which the gate compares the token's grants against,
// so a token can never confer more at admission than the device holds now (a microphone after speak
// was revoked, a camera after an unshare). A refusal is a *server.Error: E_LEAF_NOT_CURRENT for a
// room that is no live call's or a device that is no current leaf of it, E_FORBIDDEN for a device
// that is barred, lost access or whose repair is pending, and for a room of the AFK channel (R-AW1).
// Anything else is a store failure.
func (h *Calls) AdmitRoom(ctx context.Context, room string, device id.ID) (*livekit.ParticipantPermission, error) {
	notLeaf := server.Errorf(server.CodeLeafNotCurrent, "your device is not a current leaf of this call")
	callHex, _, ok := strings.Cut(room, "-")
	if !ok {
		return nil, notLeaf
	}
	callID, err := id.Parse(callHex)
	if err != nil {
		return nil, notLeaf
	}
	row, err := h.repo.GetVoiceSession(ctx, callID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, notLeaf
	}
	if err != nil {
		return nil, err
	}
	if row.Ended != nil || row.LivekitRoom != room {
		return nil, notLeaf
	}
	if err := h.requireLeafOfCall(ctx, row, device); err != nil {
		var se *server.Error
		if errors.As(err, &se) {
			return nil, notLeaf
		}
		return nil, err
	}
	// A device whose cut or demotion has not landed in the SFU does not rejoin until it has. The gate
	// retries that device's own repair only, waiting at most callLockWait for the call's lock and
	// holding it at most callHoldRequest; every other repair is the retry loop's.
	if err := h.retryOwn(ctx, callID, device); err != nil {
		return nil, err
	}
	if h.leases.isPending(callID, device) {
		return nil, server.Errorf(server.CodeForbidden, "your device's access to this call is being revoked")
	}
	dev, err := h.repo.GetDevice(ctx, device)
	if errors.Is(err, store.ErrNotFound) {
		return nil, notLeaf
	}
	if err != nil {
		return nil, err
	}
	if bar, err := barred(ctx, h.repo, dev); err != nil {
		return nil, err
	} else if bar {
		return nil, errBarred()
	}
	ch, err := h.repo.GetChannel(ctx, row.ChannelID)
	if errors.Is(err, store.ErrNotFound) {
		// The channel is deleted: its call is over, whatever the record still says (CALLS-1).
		return nil, notLeaf
	}
	if err != nil {
		return nil, err
	}
	if err := h.botCanUseChannel(ctx, dev.UserID, ch); err != nil {
		return nil, err
	}
	bits, err := h.res.Resolve(ctx, dev.UserID, ch)
	if err != nil {
		return nil, err
	}
	if !bits.Has(PermViewChannel) || !bits.Has(PermConnect) {
		return nil, server.Errorf(server.CodeForbidden, "your device may no longer join this call")
	}
	// R-AW1: a token minted for the AFK channel before it stopped hosting calls admits nobody.
	if h.isAFKChannel(ctx, ch) {
		return nil, errAFKCall()
	}
	return h.admitGrant(ctx, bits, row, device)
}

// Admitted records that the /rtc proxy let device's join of room through: the gate (AdmitRoom)
// passed and so did every check the proxy makes after it (the token's grants, the client's
// signalling protocol). It is the admission a leave of the session is bound to (DS-7, N2) and what
// opens the device's join window for the sweep (ruling (a)). Recording it only here (M-2 of the
// integration re-review) means a request the proxy refuses neither holds a join window open nor
// pushes a session's real admission out of the few remembered. A room that is no live call's
// records nothing. resume is a LiveKit signal resume of the session already in the room: it opens
// the join window like any admission, but it is no newer session (newerAdmission).
func (h *Calls) Admitted(ctx context.Context, room string, device id.ID, resume bool) {
	callHex, _, ok := strings.Cut(room, "-")
	if !ok {
		return
	}
	callID, err := id.Parse(callHex)
	if err != nil {
		return
	}
	row, err := h.repo.GetVoiceSession(ctx, callID)
	if err != nil || row.Ended != nil || row.LivekitRoom != room {
		return
	}
	h.noteAdmission(ctx, row, device, resume)
	dv, devErr := h.repo.GetDevice(ctx, device)
	// Any admission of the device answers a pending AFK move (R33): it joined somewhere. It is also
	// recorded for the away mark (R-AW7) before the user's away rows are cleared below.
	h.afkMu.Lock()
	delete(h.afkPending, device)
	if devErr == nil {
		h.noteAFKAdmissionLocked(dv.UserID, device)
	}
	h.afkMu.Unlock()
	h.trackAFKAdmission(ctx, row, device, resume)
	// R-AW2, R-AW4: an admission anywhere clears every away row of the user.
	if devErr == nil {
		h.afkAdmissionClears(ctx, dv.UserID)
	}
}

// barred reports whether dv may take part in no call at all: the device is revoked or quarantined,
// or its user is disabled, deleted, gone or owes a second factor. It reads the device row it is handed and the user row.
func barred(ctx context.Context, repo store.Repository, dv store.DeviceRow) (bool, error) {
	if dv.RevokedAt != nil || dv.QuarantinedAt != nil {
		return true, nil
	}
	u, err := repo.GetUser(ctx, dv.UserID)
	if errors.Is(err, store.ErrNotFound) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	// An account that owes a second factor (an operator's reset, web3.6 S3) is barred like a
	// disabled one: its sessions survive, but a call token minted before the reset must not last.
	return u.DisabledAt != nil || u.DeletedAt != nil || u.FactorOwed, nil
}

func errBarred() error {
	return server.Errorf(server.CodeForbidden, "your device or account may not take part in calls")
}

// refuseBarred is 403 E_FORBIDDEN for a barred device (barred), nil otherwise, or a store failure.
func (h *Calls) refuseBarred(ctx context.Context, repo store.Repository, device id.ID) error {
	dv, err := repo.GetDevice(ctx, device)
	if errors.Is(err, store.ErrNotFound) {
		return errBarred()
	}
	if err != nil {
		return err
	}
	bar, err := barred(ctx, repo, dv)
	if err != nil {
		return err
	}
	if bar {
		return errBarred()
	}
	return nil
}

// baseGrant is the human baseline for call tokens and /rtc admission: the microphone when
// the device holds speak, subscribe always, never a camera or screen source. Bots are further
// restricted by restrictBotCallGrant at every grant seam. Promotion exists only
// as the push a share makes (DEV-02, ruling F1), so a token can never be replayed for video.
func baseGrant(bits Bits) *livekit.ParticipantPermission {
	return sfu.PublishGrant(bits.Has(PermSpeak), false, false)
}
