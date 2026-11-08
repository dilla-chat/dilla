package service

import (
	"container/list"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"

	"github.com/dilla-chat/dilla/internal/bot/helper"
	"github.com/dilla-chat/dilla/internal/bot/intake"
	"github.com/dilla-chat/dilla/internal/bot/native"
	"github.com/dilla-chat/dilla/internal/bot/protocol"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
)

const (
	interactionStatusInvalid         = 1
	interactionStatusForbidden       = 2
	interactionStatusExpired         = 3
	interactionStatusConflict        = 4
	interactionStatusUnavailable     = 5
	interactionStatusBusy            = 6
	interactionStatusNotFound        = 7
	interactionStatusUpgradeRequired = 8
	interactionStatusInternal        = 9
	interactionReplayLimit           = 4096
)

var ErrInteractionRequestIDConflict = errors.New("bot interaction request id reused with different input")
var ErrInteractionDispatcherBusy = errors.New("bot interaction dispatcher replay cache is full")

// ErrInteractionReadReplayNotAuthorized is a definitive refusal of a recomputed read (Jonas
// 2026-10-30): status 1 invalid, 2 forbidden (the actor lacks the permission or is no longer a
// member), 3 expired, 7 not_found or 8 upgrade_required. Replaying the same immutable request
// would answer the same, so the event handler acknowledges the event as undeliverable.
var ErrInteractionReadReplayNotAuthorized = errors.New("interaction read replay is no longer authorized")

// ErrInteractionReadReplayPending is a refusal that may clear on retry: 4 conflict,
// 5 unavailable, 6 busy, 9 internal (including a handler error, which Dispatch answers as
// internal), or a response that does not match the request. The event stays pending; the
// poller's age limit (InteractionEventMaxPendingAge for interaction groups) still bounds how long.
var ErrInteractionReadReplayPending = errors.New("interaction read replay is not currently confirmed")

// InteractionNative is the authenticated native event and group-inspection
// boundary used by the dispatcher.
type InteractionNative interface {
	Binding() native.Binding
	InspectGroup(context.Context, id.ID) (native.GroupInfo, error)
	InspectInteractionGroup(context.Context, native.InteractionGroupExpected, []byte) (native.InteractionGroupInspectResult, error)
}

type interactionNative = InteractionNative

type InteractionSnapshotProvider interface {
	InteractionSnapshot(context.Context, native.GroupEvent) (native.InteractionGroupExpected, []byte, error)
}

// InteractionActor is copied solely from the authenticated native event.
type InteractionActor struct {
	User, Device id.ID
	Kind, Tier   uint64
	Group        id.ID
	Channel      id.ID
}

type InteractionResult struct {
	Status   uint64
	Revision *uint64
	Data     []byte
	// Note is text for the member, for the sealed path (Bot v2 Task 17): a command's reply when it succeeds ("Posted
	// the poll in #bot.") and the reason when it is refused ("The Bot posts only in Bot channels.").
	Note string
}

// InteractionHandlerSpec allows mutations to check the current operator
// configuration revision before performing their effect.
type InteractionHandlerSpec struct {
	// AllowMusicChannelContext lets a handler authorize Request.ChannelID as a
	// voice-channel target for a snapshot-validated kind-3 participant event.
	// The handler must independently bind that channel to current call state.
	AllowMusicChannelContext bool
	// RecomputeOnReplay marks read handlers whose authorization and response
	// must be refreshed for an exact duplicate request. Mutations must leave
	// this false so their effects remain deduplicated.
	RecomputeOnReplay bool
	// CurrentRevision is a preflight check. For mutations, Handle must
	// atomically compare Request.Revision with the current revision in the same
	// transaction as its write and return status 4 on a race.
	CurrentRevision func(context.Context, protocol.Request) (uint64, error)
	// RevisionInHandler means Handle authenticates the actor before reading
	// revision state and performs the atomic comparison itself.
	RevisionInHandler bool
	Handle            func(context.Context, InteractionActor, protocol.Request) (InteractionResult, error)
}

type interactionReplayKey struct {
	requestID   id.ID
	actorUser   id.ID
	actorDevice id.ID
	group       id.ID
	channel     id.ID
}

type interactionReplay struct {
	key         interactionReplayKey
	fingerprint [32]byte
	response    string
	done        chan struct{}
}

// InteractionDispatcher validates a native event, then passes its decoded
// request and native-derived actor to one registered handler. Dispatch trusts
// event fields only when callers obtained the event from the authenticated
// native helper; a Go GroupEvent value does not prove its own provenance.
// Replays are retained in a bounded, process-local cache.
type InteractionDispatcher struct {
	mu        sync.Mutex
	native    interactionNative
	binding   native.Binding
	now       func() uint64
	snapshots InteractionSnapshotProvider
	handlers  map[protocol.Operation]InteractionHandlerSpec
	entries   map[interactionReplayKey]*list.Element
	order     list.List
}

func NewInteractionDispatcher(n *native.Client, handlers map[protocol.Operation]InteractionHandlerSpec, now func() uint64) (*InteractionDispatcher, error) {
	return newInteractionDispatcher(n, handlers, now)
}

// NewInteractionDispatcherWithSnapshots enables kind-3 dispatch after the provider
// obtains a fresh server-signed snapshot for the event's recorded participant.
func NewInteractionDispatcherWithSnapshots(n InteractionNative, handlers map[protocol.Operation]InteractionHandlerSpec, now func() uint64, snapshots InteractionSnapshotProvider) (*InteractionDispatcher, error) {
	if snapshots == nil {
		return nil, errors.New("missing interaction snapshot provider")
	}
	return newInteractionDispatcherWithSnapshots(n, handlers, now, snapshots)
}

func newInteractionDispatcher(n interactionNative, handlers map[protocol.Operation]InteractionHandlerSpec, now func() uint64) (*InteractionDispatcher, error) {
	return newInteractionDispatcherWithSnapshots(n, handlers, now, nil)
}

func newInteractionDispatcherWithSnapshots(n interactionNative, handlers map[protocol.Operation]InteractionHandlerSpec, now func() uint64, snapshots InteractionSnapshotProvider) (*InteractionDispatcher, error) {
	if n == nil || now == nil {
		return nil, errors.New("missing interaction dispatcher dependency")
	}
	binding := n.Binding()
	if binding.Instance.IsZero() || binding.Community.IsZero() || binding.Installation.IsZero() {
		return nil, errors.New("invalid interaction dispatcher binding")
	}
	registered := make(map[protocol.Operation]InteractionHandlerSpec, len(handlers))
	for operation, handler := range handlers {
		if handler.Handle == nil {
			return nil, errors.New("interaction handler is missing")
		}
		registered[operation] = handler
	}
	return &InteractionDispatcher{native: n, binding: binding, now: now, snapshots: snapshots, handlers: registered, entries: make(map[interactionReplayKey]*list.Element)}, nil
}

// Register adds or replaces a handler. Missing operations return the protocol's
// unavailable status, so unfinished features remain explicitly unavailable.
func (d *InteractionDispatcher) Register(operation protocol.Operation, handler InteractionHandlerSpec) error {
	if d == nil || handler.Handle == nil {
		return errors.New("invalid interaction handler")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.handlers[operation] = handler
	return nil
}

func (d *InteractionDispatcher) Dispatch(ctx context.Context, event native.GroupEvent) (string, error) {
	if d == nil || event.DeliveryID == 0 || event.Group.IsZero() || event.Community.IsZero() || event.Channel.IsZero() || event.SourceSequence == 0 || event.SenderUser.IsZero() || event.SenderDevice.IsZero() || event.SenderKind != 0 || event.SenderTier > 1 || event.EnvelopeType != 0 || event.Body == nil {
		return "", protocol.ErrInvalid
	}
	if event.Community != d.binding.Community {
		return "", protocol.ErrContext
	}
	group, err := d.native.InspectGroup(ctx, event.Group)
	proofInspected := false
	if err != nil {
		var status *helper.StatusError
		if !errors.As(err, &status) || status.Status != interactionStatusForbidden {
			return "", err
		}
		group, err = d.inspectInteractionEventGroup(ctx, event)
		if err != nil {
			return "", err
		}
		proofInspected = true
	}
	// GroupEvent fields alone do not prove a kind-3 installation or participant
	// binding. Require a fresh signed context and native MLS inspection first.
	if group.ID != event.Group || group.State != 2 || group.Community == nil || *group.Community != d.binding.Community || group.Target != event.Channel {
		return "", protocol.ErrContext
	}
	if group.Kind == 3 {
		if !proofInspected {
			if _, err = d.inspectInteractionEventGroup(ctx, event); err != nil {
				return "", err
			}
		}
	} else if group.Kind != 0 {
		return "", protocol.ErrContext
	}
	request, err := protocol.DecodeRequest(*event.Body)
	if err != nil {
		return "", err
	}
	encoded, err := protocol.EncodeRequest(request)
	if err != nil {
		return "", err
	}
	d.mu.Lock()
	handler, ok := d.handlers[request.Operation]
	d.mu.Unlock()
	response := protocol.Response{RequestID: request.RequestID, CommunityID: d.binding.Community, Data: []byte{}}
	key := interactionReplayKey{requestID: request.RequestID, actorUser: event.SenderUser, actorDevice: event.SenderDevice, group: event.Group, channel: event.Channel}
	fingerprint := interactionFingerprint(encoded)
	entry, owner, err := d.begin(ctx, key, fingerprint)
	if err != nil {
		if errors.Is(err, ErrInteractionDispatcherBusy) {
			response.Status = interactionStatusBusy
			return protocol.EncodeResponse(response)
		}
		return "", err
	}
	if !owner {
		if !ok || !handler.RecomputeOnReplay {
			select {
			case <-entry.done:
				return entry.response, nil
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}
		select {
		case <-entry.done:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	finish := func() (string, error) {
		body, encodeErr := protocol.EncodeResponse(response)
		if encodeErr != nil {
			response.Status = interactionStatusInternal
			response.Revision = nil
			response.Data = []byte{}
			body, encodeErr = protocol.EncodeResponse(response)
		}
		if encodeErr != nil {
			return "", encodeErr
		}
		if owner {
			d.complete(entry, body)
		}
		return body, nil
	}

	if request.CommunityID != d.binding.Community {
		response.Status = interactionStatusForbidden
		return finish()
	}
	if err := protocol.ValidateRequestContext(request, d.binding.Community, d.now()); err != nil {
		if errors.Is(err, protocol.ErrExpired) {
			response.Status = interactionStatusExpired
			return finish()
		}
		response.Status = interactionStatusForbidden
		return finish()
	}
	if request.ChannelID != nil && *request.ChannelID != event.Channel && (group.Kind != 3 || !handler.AllowMusicChannelContext) {
		response.Status = interactionStatusForbidden
		return finish()
	}

	if !ok {
		response.Status = interactionStatusUnavailable
		return finish()
	}
	actor := InteractionActor{User: event.SenderUser, Device: event.SenderDevice, Kind: event.SenderKind, Tier: event.SenderTier, Group: event.Group, Channel: event.Channel}
	if request.Revision != nil {
		if handler.CurrentRevision == nil && !handler.RevisionInHandler {
			response.Status = interactionStatusUnavailable
			return finish()
		}
		if handler.CurrentRevision != nil {
			current, revisionErr := handler.CurrentRevision(ctx, request)
			if revisionErr != nil || current > math.MaxInt64 {
				response.Status = interactionStatusInternal
				return finish()
			}
			if *request.Revision != current {
				response.Status = interactionStatusConflict
				response.Revision = &current
				return finish()
			}
		}
	}
	result, handlerErr := handler.Handle(ctx, actor, request)
	if handlerErr != nil || result.Status > 9 || result.Revision != nil && *result.Revision > math.MaxInt64 {
		response.Status = interactionStatusInternal
		return finish()
	}
	response.Status = result.Status
	response.Revision = result.Revision
	response.Data = append([]byte{}, result.Data...)
	return finish()
}

func (d *InteractionDispatcher) inspectInteractionEventGroup(ctx context.Context, event native.GroupEvent) (native.GroupInfo, error) {
	if d.snapshots == nil || event.SenderKind != 0 {
		return native.GroupInfo{}, protocol.ErrContext
	}
	expected, snapshot, err := d.snapshots.InteractionSnapshot(ctx, event)
	if err != nil {
		return native.GroupInfo{}, protocol.ErrContext
	}
	if len(snapshot) == 0 || expected.Instance != d.binding.Instance || expected.Installation != d.binding.Installation || expected.Community != d.binding.Community ||
		expected.Group != event.Group || expected.ParticipantUser != event.SenderUser || expected.ParticipantUser != event.Channel || expected.ParticipantDevice != event.SenderDevice {
		return native.GroupInfo{}, protocol.ErrContext
	}
	inspected, err := d.native.InspectInteractionGroup(ctx, expected, snapshot)
	if err != nil {
		return native.GroupInfo{}, protocol.ErrContext
	}
	if inspected.Binding.Instance != expected.Instance || inspected.Binding.Installation != expected.Installation || inspected.Binding.Community != expected.Community ||
		inspected.Binding.BotUser != expected.BotUser || inspected.BotDevice != expected.BotDevice || inspected.Binding.ParticipantUser != expected.ParticipantUser ||
		inspected.Binding.ParticipantDevice != expected.ParticipantDevice || inspected.Binding.PolicyVersion != expected.PolicyVersion || inspected.Binding.E2EEVersion != 1 ||
		inspected.Binding.MediaVersion != 0 || inspected.Epoch == 0 || inspected.State != 2 || inspected.PendingCommit || inspected.NextSequence == 0 {
		return native.GroupInfo{}, protocol.ErrContext
	}
	community := expected.Community
	return native.GroupInfo{ID: expected.Group, Kind: 3, Community: &community, Target: expected.ParticipantUser, State: 2, Epoch: inspected.Epoch, NextSequence: inspected.NextSequence}, nil
}

// ReauthorizeReadReplay reruns a registered read handler and discards its
// response, leaving the response already bound to the event unchanged. It is
// used before resuming a durable response whose send has not yet been accepted.
// It returns nil when the read is still authorized (status 0), and
// ErrInteractionReadReplayNotAuthorized only for a deterministic refusal (status
// 1 invalid, 2 forbidden, 3 expired, 7 not_found, 8 upgrade_required); the event
// handler then acknowledges the event as undeliverable. Status 4 conflict,
// 5 unavailable, 6 busy, 9 internal, a mismatched response, a Dispatch error and
// a cancelled context return an error that keeps the event pending.
func (d *InteractionDispatcher) ReauthorizeReadReplay(ctx context.Context, event native.GroupEvent) error {
	if d == nil || event.Body == nil {
		return protocol.ErrInvalid
	}
	request, err := protocol.DecodeRequest(*event.Body)
	if err != nil {
		return err
	}
	d.mu.Lock()
	handler, ok := d.handlers[request.Operation]
	d.mu.Unlock()
	if !ok || !handler.RecomputeOnReplay {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	body, err := d.Dispatch(ctx, event)
	if err != nil {
		return err
	}
	// A handler that observed cancellation may have answered with any status; never let that
	// answer become a definitive denial.
	if err := ctx.Err(); err != nil {
		return err
	}
	response, err := protocol.DecodeResponse(body)
	if err != nil {
		return err
	}
	if response.RequestID != request.RequestID || response.CommunityID != d.binding.Community {
		return fmt.Errorf("%w: response does not match the request", ErrInteractionReadReplayPending)
	}
	switch response.Status {
	case 0:
		return nil
	case interactionStatusInvalid, interactionStatusForbidden, interactionStatusExpired, interactionStatusNotFound, interactionStatusUpgradeRequired:
		// Dispatch itself never answers a read with status 3: ValidateRequestContext checks
		// expiry only for mutating operations. A read handler may still return it, and an
		// expired read can never become valid again, so the rule is kept as harmless.
		return ErrInteractionReadReplayNotAuthorized
	default:
		return fmt.Errorf("%w: status %d", ErrInteractionReadReplayPending, response.Status)
	}
}

func (d *InteractionDispatcher) begin(ctx context.Context, key interactionReplayKey, fingerprint [32]byte) (*interactionReplay, bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if element := d.entries[key]; element != nil {
		entry, ok := element.Value.(*interactionReplay)
		if !ok || entry == nil {
			return nil, false, ErrInteractionDispatcherBusy
		}
		if entry.fingerprint != fingerprint {
			return nil, false, ErrInteractionRequestIDConflict
		}
		return entry, false, nil
	}
	for d.order.Len() >= interactionReplayLimit {
		oldest := d.order.Front()
		if oldest == nil {
			break
		}
		entry, ok := oldest.Value.(*interactionReplay)
		if !ok || entry == nil {
			d.order.Remove(oldest)
			return nil, false, ErrInteractionDispatcherBusy
		}
		select {
		case <-entry.done:
			d.order.Remove(oldest)
			delete(d.entries, entry.key)
		default:
			return nil, false, ErrInteractionDispatcherBusy
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	entry := &interactionReplay{key: key, fingerprint: fingerprint, done: make(chan struct{})}
	d.entries[key] = d.order.PushBack(entry)
	return entry, true, nil
}

func (d *InteractionDispatcher) complete(entry *interactionReplay, response string) {
	d.mu.Lock()
	entry.response = response
	close(entry.done)
	d.mu.Unlock()
}

func interactionFingerprint(body string) [32]byte {
	return sha256.Sum256([]byte(body))
}

// ---- Bot v2 Task 15: sealed commands ----

// SealedDispatcher runs a sealed command the intake opened, verified and authorised through the same
// feature handlers the interaction dispatcher registers (the InteractionHandlerSpec map). The member
// identity is the one the helper verified (the device's signature, matched to the identity dillad
// attached); the community-wide permission_bit was checked from the feed state by the intake; each
// handler still applies its own checks (a call's DJ rights, a poll's channel).
//
// An adapter per operation turns the catalog's argument shapes into the v1 handler's request and the
// handler's result into answers. A picker command (catalog handler_kind 0) answers UTF-8 text. A sealed
// read (handler_kind 1: OpReadPoll, OpReadMusic, OpReadRemindersDue) takes the v1 operation's arguments
// (ids as 16-byte byte strings or 32 hex digits, a cursor as bytes or null) and answers the CBOR
// [revision uint|null, data bstr], data being the v1 response data unchanged, so the app's existing
// decoders read it. A refusal's body is a bare code for a read, and text a member can act on for a
// picker command (the client maps E_FORBIDDEN itself).
//
// Mutations carry Revision 0 and Expires sent_at + 120 s (the sent_at window). A picker command does not
// know the entity's revision: for the operations marked retryConflict a 409 that names the current
// revision is tried once more with it (last writer wins, as a typed command means), and never looped.
type SealedDispatcher struct {
	Community id.ID
	Handlers  map[protocol.Operation]InteractionHandlerSpec
}

type sealedAdapter struct {
	// handler is the v1 operation whose handler runs.
	handler protocol.Operation
	// request builds the v1 request's channel, args and mutation flag; problem is the 400 text.
	request func(req protocol.SealedRequest) (channel *id.ID, args []any, mutation bool, problem string)
	// progress, when set, is answered before the handler runs ("Searching…").
	progress string
	// read answers [revision, data]; otherwise render makes the text of a success.
	read          bool
	render        func(req protocol.SealedRequest, result InteractionResult) string
	retryConflict bool
}

const (
	sealedCommandWindow = 120
	musicNeedsRoom      = "Join a voice channel with the Bot first."
)

// SealedOperationSupported reports whether op has a sealed adapter.
func SealedOperationSupported(op protocol.Operation) bool {
	_, ok := sealedAdapters[op]
	return ok
}

func sealedText(args []any, i int) (string, bool) {
	if i >= len(args) {
		return "", false
	}
	s, ok := args[i].(string)
	return s, ok && strings.TrimSpace(s) != ""
}

func sealedUint(args []any, i int) (uint64, bool) {
	if i >= len(args) {
		return 0, false
	}
	v, ok := args[i].(uint64)
	return v, ok
}

// sealedID reads an id given as 16 bytes (an app surface) or 32 hex digits (typed in the picker).
func sealedID(args []any, i int) (id.ID, bool) {
	if i >= len(args) {
		return id.ID{}, false
	}
	switch v := args[i].(type) {
	case []byte:
		if len(v) != len(id.ID{}) {
			return id.ID{}, false
		}
		var out id.ID
		copy(out[:], v)
		return out, !out.IsZero()
	case string:
		out, err := id.Parse(strings.TrimSpace(v))
		return out, err == nil && !out.IsZero()
	}
	return id.ID{}, false
}

// sealedCursor reads a page cursor: null, or 1..256 bytes.
func sealedCursor(args []any, i int) (any, bool) {
	if i >= len(args) || args[i] == nil {
		return nil, true
	}
	b, ok := args[i].([]byte)
	return b, ok && len(b) > 0 && len(b) <= protocol.MaxModerationCursorBytes
}

func musicRoom(req protocol.SealedRequest) (*id.ID, bool) {
	if req.ChannelID.IsZero() {
		return nil, false
	}
	ch := req.ChannelID
	return &ch, true
}

// musicControl is a music mutation with no arguments, answered with done.
func musicControl(op protocol.Operation, done string, retry bool) sealedAdapter {
	return sealedAdapter{handler: op, retryConflict: retry,
		request: func(req protocol.SealedRequest) (*id.ID, []any, bool, string) {
			ch, ok := musicRoom(req)
			if !ok {
				return nil, nil, false, musicNeedsRoom
			}
			return ch, []any{}, true, ""
		},
		render: func(protocol.SealedRequest, InteractionResult) string { return done }}
}

func musicState(op protocol.Operation, withQueue bool) sealedAdapter {
	return sealedAdapter{handler: protocol.OpMusicState,
		request: func(req protocol.SealedRequest) (*id.ID, []any, bool, string) {
			ch, ok := musicRoom(req)
			if !ok {
				return nil, nil, false, musicNeedsRoom
			}
			return ch, []any{}, false, ""
		},
		read: op == protocol.OpReadMusic,
		render: func(_ protocol.SealedRequest, r InteractionResult) string {
			return renderMusicState(r.Data, withQueue)
		}}
}

// musicTrackText is "Title — Artist" from an encodeMusicTrack row.
func musicTrackText(v any) (string, bool) {
	row, ok := v.([]any)
	if !ok || len(row) < 5 {
		return "", false
	}
	title, _ := row[3].(string)
	artist, _ := row[4].(string)
	if title == "" {
		return "", false
	}
	if artist == "" {
		return title, true
	}
	return title + " — " + artist, true
}

// renderMusicState is the text of a music snapshot (encodeMusicSnapshot) in the established /queue copy
// (web bot-handlers.ts): "Now playing: T — A (paused)", then "1. T — A" per queued track, "Showing n of m queued
// tracks." when the snapshot was cut, and "The music queue is empty." with neither. /queue and /nowplaying share
// OpMusicState, so both answer this form.
func renderMusicState(data []byte, _ bool) string {
	var fields []any
	if cborx.Unmarshal(data, &fields) != nil || len(fields) < 4 {
		return "The music queue is empty."
	}
	var lines []string
	if now, ok := musicTrackText(fields[2]); ok {
		if len(fields) > 5 && fields[5] == uint64(1) {
			now += " (paused)"
		}
		lines = append(lines, "Now playing: "+now)
	}
	queue, _ := fields[3].([]any)
	for i, row := range queue {
		if text, ok := musicTrackText(row); ok {
			lines = append(lines, fmt.Sprintf("%d. %s", i+1, text))
		}
	}
	if len(fields) > 12 {
		if total, ok := fields[12].(uint64); ok {
			lines = append(lines, fmt.Sprintf("Showing %d of %d queued tracks.", len(queue), total))
		}
	}
	if len(lines) == 0 {
		return "The music queue is empty."
	}
	return strings.Join(lines, "\n")
}

// pollDurations are the poll lengths the handler accepts; a picker duration rounds up to the next one.
var pollDurations = []uint64{3600, 14400, 28800, 86400, 259200, 604800}

func pollAnswers(text string) []any {
	sep := "|"
	if !strings.Contains(text, sep) {
		sep = ","
	}
	var out []any
	for _, part := range strings.Split(text, sep) {
		if s := strings.TrimSpace(part); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func humanSeconds(s uint64) string {
	unit := func(n uint64, one string) string {
		if n == 1 {
			return "1 " + one
		}
		return fmt.Sprintf("%d %ss", n, one)
	}
	switch {
	case s%86400 == 0:
		return unit(s/86400, "day")
	case s%3600 == 0:
		return unit(s/3600, "hour")
	case s%60 == 0:
		return unit(s/60, "minute")
	}
	return unit(s, "second")
}

var sealedAdapters = map[protocol.Operation]sealedAdapter{
	protocol.OpMusicEnqueue: {handler: protocol.OpMusicEnqueue, progress: "Searching…",
		request: func(req protocol.SealedRequest) (*id.ID, []any, bool, string) {
			ch, ok := musicRoom(req)
			if !ok {
				return nil, nil, false, musicNeedsRoom
			}
			query, ok := sealedText(req.Args, 0)
			if !ok {
				return nil, nil, false, "Say what to play: a song or a link."
			}
			return ch, []any{strings.TrimSpace(query)}, true, ""
		},
		render: func(protocol.SealedRequest, InteractionResult) string { return "Track added to the music queue." }},
	// The established client copy (packages/web/src/screens/commands/bot-handlers.ts; Task 17 fix round 2 ruling: every
	// sealed music, poll and reminder answer reads as the v1 client said it).
	protocol.OpMusicPause:  musicControl(protocol.OpMusicPause, "Music paused.", false),
	protocol.OpMusicResume: musicControl(protocol.OpMusicResume, "Music resumed.", true),
	protocol.OpMusicSkip:   musicControl(protocol.OpMusicSkip, "Skipped the current track.", false),
	protocol.OpMusicStop:   musicControl(protocol.OpMusicStop, "Music stopped.", true),
	protocol.OpMusicShuffle: func() sealedAdapter {
		a := musicControl(protocol.OpMusicShuffle, "", true)
		a.render = func(_ protocol.SealedRequest, r InteractionResult) string {
			// The handler answers the state after the toggle; element 7 is shuffle.
			var fields []any
			if cborx.Unmarshal(r.Data, &fields) == nil && len(fields) > 7 && fields[7] == uint64(0) {
				return "Shuffle disabled."
			}
			return "Shuffle enabled."
		}
		return a
	}(),
	protocol.OpMusicClear: musicControl(protocol.OpMusicClear, "Music queue cleared.", true),
	protocol.OpMusicVolume: {handler: protocol.OpMusicVolume, retryConflict: true,
		request: func(req protocol.SealedRequest) (*id.ID, []any, bool, string) {
			ch, ok := musicRoom(req)
			if !ok {
				return nil, nil, false, musicNeedsRoom
			}
			level, ok := sealedUint(req.Args, 0)
			if !ok || level > 100 {
				return nil, nil, false, "The volume is a number from 0 to 100."
			}
			return ch, []any{level}, true, ""
		},
		render: func(req protocol.SealedRequest, _ InteractionResult) string {
			level, _ := sealedUint(req.Args, 0)
			return fmt.Sprintf("Volume set to %d%%.", level)
		}},
	protocol.OpMusicRepeat: {handler: protocol.OpMusicRepeat, retryConflict: true,
		request: func(req protocol.SealedRequest) (*id.ID, []any, bool, string) {
			ch, ok := musicRoom(req)
			if !ok {
				return nil, nil, false, musicNeedsRoom
			}
			mode, _ := sealedText(req.Args, 0)
			value, ok := map[string]uint64{"off": 0, "one": 1, "all": 2}[strings.ToLower(strings.TrimSpace(mode))]
			if !ok {
				return nil, nil, false, "Repeat is off, one or all."
			}
			return ch, []any{value}, true, ""
		},
		render: func(req protocol.SealedRequest, _ InteractionResult) string {
			mode, _ := sealedText(req.Args, 0)
			return "Repeat set to " + strings.ToLower(strings.TrimSpace(mode)) + "."
		}},
	protocol.OpMusicState: musicState(protocol.OpMusicState, true),
	protocol.OpReadMusic:  musicState(protocol.OpReadMusic, false),
	protocol.OpPollCreate: {handler: protocol.OpPollCreate,
		request: func(req protocol.SealedRequest) (*id.ID, []any, bool, string) {
			question, ok := sealedText(req.Args, 0)
			if !ok {
				return nil, nil, false, "Ask a question."
			}
			text, _ := sealedText(req.Args, 1)
			answers := pollAnswers(text)
			if len(answers) < 2 || len(answers) > 10 {
				return nil, nil, false, "Give 2 to 10 answers, separated by |."
			}
			multiple, _ := sealedUint(req.Args, 2)
			duration := uint64(86400)
			if d, ok := sealedUint(req.Args, 3); ok {
				duration = pollDurations[len(pollDurations)-1]
				for _, allowed := range pollDurations {
					if d <= allowed {
						duration = allowed
						break
					}
				}
			}
			// Bot v2 Task 17: a channel named in the command is used as given (the handler refuses one that is not a Bot
			// channel). Without one the poll goes where the command ran if that is a Bot channel, else to the
			// feature's configured Bot channel, which the handler resolves (pollDefaultChannel stands for it: the v1
			// payload needs a non-zero channel).
			target := pollDefaultChannel[:]
			if channel, ok := sealedID(req.Args, 4); ok {
				target = channel[:]
			} else if !req.ChannelID.IsZero() && CheckBotChannel(req.CommunityID, req.ChannelID) == nil {
				target = req.ChannelID[:]
			}
			payload, err := cborx.Marshal([]any{uint64(1), target, nil, strings.TrimSpace(question), answers, min(multiple, 1), duration})
			if err != nil {
				return nil, nil, false, "That poll is too long."
			}
			return nil, []any{payload}, true, ""
		},
		// The reply names the channel the poll went to; a handler with no note leaves the client to say where.
		render: func(_ protocol.SealedRequest, result InteractionResult) string { return result.Note }},
	protocol.OpReadPoll: {handler: protocol.OpPollGet, read: true,
		request: func(req protocol.SealedRequest) (*id.ID, []any, bool, string) {
			poll, ok := sealedID(req.Args, 0)
			if !ok || len(req.Args) != 1 {
				return nil, nil, false, "E_INVALID"
			}
			return nil, []any{poll}, false, ""
		}},
	protocol.OpReminderCreate: {handler: protocol.OpReminderCreate,
		request: func(req protocol.SealedRequest) (*id.ID, []any, bool, string) {
			when, okWhen := sealedUint(req.Args, 0)
			text, okText := sealedText(req.Args, 1)
			if !okWhen || when < 60 || when > 31536000 {
				return nil, nil, false, "Choose a time at least a minute from now and within a year."
			}
			if !okText {
				return nil, nil, false, "Say what to remind you of."
			}
			payload, err := cborx.Marshal([]any{uint64(1), uint64(0), when, text, "UTC", nil, nil})
			if err != nil {
				return nil, nil, false, "That reminder is too long."
			}
			return nil, []any{payload}, true, ""
		},
		// The established copy (web strings reminder.created): "Saved. I’ll remind you {time}: {text}".
		render: func(req protocol.SealedRequest, _ InteractionResult) string {
			when, _ := sealedUint(req.Args, 0)
			text, _ := sealedText(req.Args, 1)
			return "Saved. I’ll remind you in " + humanSeconds(when) + ": " + text
		}},
	protocol.OpReminderList: {handler: protocol.OpReminderList,
		request: func(protocol.SealedRequest) (*id.ID, []any, bool, string) {
			return nil, []any{nil}, false, ""
		},
		render: func(_ protocol.SealedRequest, r InteractionResult) string {
			var page []any
			if cborx.Unmarshal(r.Data, &page) == nil && len(page) >= 2 {
				if items, ok := page[1].([]any); ok {
					switch len(items) {
					case 0:
						return "You have no upcoming reminders."
					case 1:
						return "You have 1 upcoming reminder. Open Reminders to see it."
					}
					return fmt.Sprintf("You have %d upcoming reminders. Open Reminders to see them.", len(items))
				}
			}
			return "Open Reminders to see your reminders."
		}},
	protocol.OpReadRemindersDue: {handler: protocol.OpReminderDue, read: true,
		request: func(req protocol.SealedRequest) (*id.ID, []any, bool, string) {
			cursor, ok := sealedCursor(req.Args, 0)
			if !ok || len(req.Args) > 1 {
				return nil, nil, false, "E_INVALID"
			}
			return nil, []any{cursor}, false, ""
		}},
}

// sealedRefusal maps a v1 handler status to an answer: a bare code for a read, text for a command.
func sealedRefusal(status uint64, read bool) intake.Answer {
	type pair struct {
		code   uint16
		bare   string
		member string
	}
	m := map[uint64]pair{
		interactionStatusInvalid:         {400, "E_INVALID", "That command's arguments aren't right."},
		interactionStatusForbidden:       {403, "E_FORBIDDEN", "E_FORBIDDEN"},
		interactionStatusExpired:         {408, "E_BOT_STALE", "E_BOT_STALE"},
		interactionStatusConflict:        {409, "E_CONFLICT", "Something changed meanwhile. Try again."},
		interactionStatusUnavailable:     {503, "E_UNAVAILABLE", "The Bot can't do that here right now."},
		interactionStatusBusy:            {429, "E_BUSY", "The Bot is busy. Try again in a moment."},
		interactionStatusNotFound:        {404, "E_NOT_FOUND", "That wasn't found."},
		interactionStatusUpgradeRequired: {426, "E_UPGRADE_REQUIRED", "E_UPGRADE_REQUIRED"},
	}
	p, ok := m[status]
	if !ok {
		p = pair{500, intake.CodeInternal, intake.CodeInternal}
	}
	body := p.member
	if read {
		body = p.bare
	}
	return intake.Answer{Status: p.code, Body: []byte(body), Final: true}
}

// Dispatch answers req through its adapter and feature handler. It always ends with a final answer.
func (d *SealedDispatcher) Dispatch(ctx context.Context, req protocol.SealedRequest, reply func(intake.Answer)) {
	unsupported := intake.Answer{Status: intake.StatusUnsupported, Body: []byte(intake.CodeUnsupported), Final: true}
	adapter, ok := sealedAdapters[protocol.Operation(req.Operation)]
	if !ok || d == nil {
		reply(unsupported)
		return
	}
	handler, ok := d.Handlers[adapter.handler]
	if !ok || handler.Handle == nil {
		reply(unsupported)
		return
	}
	channel, args, mutation, problem := adapter.request(req)
	if problem != "" {
		reply(intake.Answer{Status: 400, Body: []byte(problem), Final: true})
		return
	}
	v1 := protocol.Request{RequestID: req.RequestID, CommunityID: d.Community, ChannelID: channel, Operation: adapter.handler, Args: args}
	if mutation {
		revision, expires := uint64(0), req.SentAt+sealedCommandWindow
		v1.Revision, v1.Expires = &revision, &expires
	}
	// The v1 request's own validation (argument shapes and bounds) applies to every sealed request.
	if _, err := protocol.EncodeRequestCBOR(v1); err != nil || req.CommunityID != d.Community {
		reply(sealedRefusal(interactionStatusInvalid, adapter.read))
		return
	}
	if adapter.progress != "" {
		reply(intake.Answer{Status: 202, Body: []byte(adapter.progress)})
	}
	actor := InteractionActor{User: req.UserID, Device: req.DeviceID, Channel: req.ChannelID}
	result, err := handler.Handle(ctx, actor, v1)
	if err == nil && result.Status == interactionStatusConflict && adapter.retryConflict && mutation && result.Revision != nil {
		current := *result.Revision
		v1.Revision = &current
		result, err = handler.Handle(ctx, actor, v1)
	}
	if err != nil || result.Status > interactionStatusInternal {
		reply(sealedRefusal(interactionStatusInternal, adapter.read))
		return
	}
	if result.Status != 0 {
		answer := sealedRefusal(result.Status, adapter.read)
		if result.Note != "" && !adapter.read {
			answer.Body = []byte(result.Note)
		}
		reply(answer)
		return
	}
	if adapter.read {
		var revision any
		if result.Revision != nil {
			revision = *result.Revision
		}
		data := result.Data
		if data == nil {
			data = []byte{}
		}
		body, err := cborx.Marshal([]any{revision, data})
		if err != nil {
			reply(sealedRefusal(interactionStatusInternal, true))
			return
		}
		reply(intake.Answer{Status: intake.StatusOK, Body: body, Final: true})
		return
	}
	text := ""
	if adapter.render != nil {
		text = adapter.render(req, result)
	}
	reply(intake.Answer{Status: intake.StatusOK, Body: []byte(text), Final: true})
}
