package service

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/dilla-chat/dilla/internal/bot/catalog"
	"github.com/dilla-chat/dilla/internal/bot/intake"
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
)

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
	// Extra, for a sealed read only, is answered as a third element after [revision, data] (Task 21b fix round 1: the
	// poll card's voters with its poll.get).
	Extra any
}

type pollVotersKey struct{}

// withPollVoters marks ctx as the sealed poll.get (91): the poll handler answers the voters too.
func withPollVoters(ctx context.Context) context.Context {
	return context.WithValue(ctx, pollVotersKey{}, true)
}

func wantPollVoters(ctx context.Context) bool { v, _ := ctx.Value(pollVotersKey{}).(bool); return v }

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
	// Feed, when set, is the feed-state permission view: the role panel (131) needs view and send in the Bot channel it
	// posts into (Task 21b fix round 1, review M4); nil refuses it. Moderation evidence (161) and the Bot's status (94)
	// read community-wide bits through it too (Task 22), when it offers them (CommunityPermissions); otherwise they are
	// refused.
	Feed FeedPermissions
	// Status, when set, is what the Bot page's status read (94) answers (Task 22); nil answers it unavailable.
	Status func() BotStatus
}

// CommunityPermissions is a feed view's community-wide bits (intake.Authz.Permissions): 0 for a non-member, every bit for
// the owner and an administrator.
type CommunityPermissions interface {
	Permissions(user id.ID) uint64
}

// BotStatus is the Bot page's status (Task 22): the Bot channels whose group the Bot cannot join on its own (Task 16's
// NeedsReAdd: a member's client must add it again) and the last music starts.
type BotStatus struct {
	NeedsReAdd  []id.ID
	MusicStarts []MusicStart
}

// MusicStart is one track start's first-frame time; Cold when its lookup ran yt-dlp.
type MusicStart struct {
	FirstFrame time.Duration
	Cold       bool
}

// Permission bits the dispatcher checks itself (internal/api/perm.go).
const (
	evidencePermManageMessages = uint64(1) << 2
	evidencePermViewAuditLog   = uint64(1) << 17
	statusPermManageCommunity  = uint64(1) << 16
	// botStatusStartsKept is how many music starts the status read carries (the Bot keeps 20 per voice room).
	botStatusStartsKept = 20
	// botStatusChannelsKept bounds the needs-re-add list (dillad allows 64 Bot channels per community).
	botStatusChannelsKept = 256
)

// rolePanelChannelBits is view_channel and send_messages: what a role panel's poster needs in its target channel.
const rolePanelChannelBits = pollPermView | pollPermSend

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
	// v1, set for an app operation (Task 21b), builds the whole v1 request from the sealed one; problem is a bare
	// code. It replaces request.
	v1 func(req protocol.SealedRequest, community id.ID) (v1 protocol.Request, status uint64)
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
		// The established client copy (bot-handlers.ts): a /play that started playback answers "Now playing: T — A."
		// once its first frame is published (Bot v2 Task 19); one queued behind a playing track is added.
		render: func(_ protocol.SealedRequest, r InteractionResult) string {
			if r.Note != "" {
				return r.Note
			}
			return "Track added to the music queue."
		}},
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
	protocol.OpReadRemindersDue: {handler: protocol.OpReadRemindersDue, read: true,
		request: func(req protocol.SealedRequest) (*id.ID, []any, bool, string) {
			cursor, ok := sealedCursor(req.Args, 0)
			if !ok || len(req.Args) > 1 {
				return nil, nil, false, "E_INVALID"
			}
			return nil, []any{cursor}, false, ""
		}},
}

// appOperation is an app operation's adapter (Task 21b): it runs v1 with the sealed arguments, a mutation's expected
// revision first. The arguments are checked and normalised (ids from 16-byte byte strings) by the v1 request decoder
// itself, so an app operation admits exactly what the v1 operation admitted. channelled operations act in the request's
// channel (the voice room); the rest carry none.
func appOperation(v1 protocol.Operation, channelled bool) sealedAdapter {
	op := v1 + catalog.AppOperationBase
	mutation := catalog.AppMutation(op)
	return sealedAdapter{handler: v1, read: true,
		v1: func(req protocol.SealedRequest, community id.ID) (protocol.Request, uint64) {
			args := req.Args
			var revision, expires any
			if mutation {
				r, ok := sealedUint(args, 0)
				if !ok {
					return protocol.Request{}, interactionStatusInvalid
				}
				revision, expires, args = r, req.SentAt+sealedCommandWindow, args[1:]
			}
			var channel any
			if channelled {
				if req.ChannelID.IsZero() {
					return protocol.Request{}, interactionStatusInvalid
				}
				channel = req.ChannelID[:]
			}
			if args == nil {
				args = []any{}
			}
			raw, err := cborx.Marshal([]any{uint64(1), req.RequestID[:], community[:], channel, uint64(v1), revision, expires, args})
			if err != nil {
				return protocol.Request{}, interactionStatusInvalid
			}
			out, err := protocol.DecodeRequestCBOR(raw)
			if err != nil {
				return protocol.Request{}, interactionStatusInvalid
			}
			if v1 == protocol.OpPollCreate && !pollPayloadTargets(out.Args, req.ChannelID) {
				return protocol.Request{}, interactionStatusForbidden
			}
			return out, 0
		}}
}

// pollPayloadTargets reports whether a poll.create payload, [1, channel, thread|null, …], posts into channel: the poll
// form's create may post only where the intake authorised send_messages.
func pollPayloadTargets(args []any, channel id.ID) bool {
	if len(args) != 1 || channel.IsZero() {
		return false
	}
	raw, ok := args[0].([]byte)
	var payload []any
	if !ok || cborx.Unmarshal(raw, &payload) != nil || len(payload) < 2 {
		return false
	}
	target, ok := payload[1].([]byte)
	return ok && len(target) == len(id.ID{}) && id.ID(target) == channel
}

func init() {
	for _, a := range []struct {
		v1         protocol.Operation
		channelled bool
	}{
		{protocol.OpMusicSearch, true}, {protocol.OpMusicEnqueue, true}, {protocol.OpMusicPause, true}, {protocol.OpMusicResume, true},
		{protocol.OpMusicSkip, true}, {protocol.OpMusicStop, true}, {protocol.OpMusicSeek, true}, {protocol.OpMusicVolume, true},
		{protocol.OpMusicShuffle, true}, {protocol.OpMusicRepeat, true}, {protocol.OpMusicRemove, true}, {protocol.OpMusicMove, true},
		{protocol.OpMusicClear, true}, {protocol.OpMusicAnnounce, true}, {protocol.OpMusicProvidersStatus, false},
		{protocol.OpRolePanelPublish, false}, {protocol.OpAnnouncementsList, false}, {protocol.OpAnnouncementsPut, false},
		{protocol.OpAnnouncementsDelete, false}, {protocol.OpPollCreate, false}, {protocol.OpPollList, false}, {protocol.OpPollVote, false},
		{protocol.OpPollClose, false}, {protocol.OpPollVoters, false}, {protocol.OpReminderCreate, false}, {protocol.OpReminderList, false},
		{protocol.OpReminderCancel, false}, {protocol.OpReminderDismiss, false},
		{protocol.OpSettingsGet, false}, {protocol.OpSettingsPut, false}, {protocol.OpModerationEvidence, false},
		// Task 23: the moderation log's pages.
		{protocol.OpModerationCases, false}, {protocol.OpModerationEvidenceList, false},
	} {
		sealedAdapters[a.v1+catalog.AppOperationBase] = appOperation(a.v1, a.channelled)
	}
	// The Bot page's status read is answered by the dispatcher itself (botStatus), from SealedDispatcher.Status.
	sealedAdapters[protocol.OpReadBotStatus] = sealedAdapter{handler: protocol.OpReadBotStatus, read: true}
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
	if protocol.Operation(req.Operation) == protocol.OpReadBotStatus {
		d.botStatus(req, reply)
		return
	}
	handler, ok := d.Handlers[adapter.handler]
	if !ok || handler.Handle == nil {
		reply(unsupported)
		return
	}
	var v1 protocol.Request
	var mutation bool
	if adapter.v1 != nil {
		var status uint64
		if v1, status = adapter.v1(req, d.Community); status != 0 {
			reply(sealedRefusal(status, true))
			return
		}
		mutation = v1.Revision != nil
		// v1 admitted evidence only from a member holding view_audit_log or manage_messages (commandOperationAllowed);
		// the handler then binds it to that member's own accepted removal or warning.
		if v1.Operation == protocol.OpModerationEvidence && !d.communityAllows(req.UserID, evidencePermViewAuditLog|evidencePermManageMessages) {
			reply(sealedRefusal(interactionStatusForbidden, true))
			return
		}
		if v1.Operation == protocol.OpRolePanelPublish {
			target, ok := v1.Args[0].(id.ID)
			if !ok || d.Feed == nil || d.Feed.ChannelPermissions(req.UserID, target)&rolePanelChannelBits != rolePanelChannelBits {
				reply(sealedRefusal(interactionStatusForbidden, true))
				return
			}
		}
	} else {
		channel, args, mut, problem := adapter.request(req)
		if problem != "" {
			reply(intake.Answer{Status: 400, Body: []byte(problem), Final: true})
			return
		}
		mutation = mut
		v1 = protocol.Request{RequestID: req.RequestID, CommunityID: d.Community, ChannelID: channel, Operation: adapter.handler, Args: args}
		if mutation {
			revision, expires := uint64(0), req.SentAt+sealedCommandWindow
			v1.Revision, v1.Expires = &revision, &expires
		}
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
	// A picker /play plays when nothing does (Task 19); the player's own enqueue only queues, as the v1 operation did.
	if adapter.handler == protocol.OpMusicEnqueue && adapter.v1 == nil {
		ctx = withSealedPlay(ctx)
	}
	if protocol.Operation(req.Operation) == protocol.OpReadPoll {
		ctx = withPollVoters(ctx)
	}
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
		fields := []any{revision, data}
		if result.Extra != nil {
			fields = append(fields, result.Extra)
		}
		body, err := cborx.Marshal(fields)
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

// communityAllows reports whether user holds any of bits community-wide in the feed state; false with no feed view.
func (d *SealedDispatcher) communityAllows(user id.ID, anyOf uint64) bool {
	view, ok := d.Feed.(CommunityPermissions)
	return ok && view.Permissions(user)&anyOf != 0
}

// botStatus answers the Bot page's status read (94): [null, [1, needs_re_add, starts]] to a member who manages the
// community, from the feed state; a bare code otherwise.
func (d *SealedDispatcher) botStatus(req protocol.SealedRequest, reply func(intake.Answer)) {
	if len(req.Args) != 0 || req.CommunityID != d.Community {
		reply(sealedRefusal(interactionStatusInvalid, true))
		return
	}
	if !d.communityAllows(req.UserID, statusPermManageCommunity) {
		reply(sealedRefusal(interactionStatusForbidden, true))
		return
	}
	if d.Status == nil {
		reply(sealedRefusal(interactionStatusUnavailable, true))
		return
	}
	status := d.Status()
	channels := slices.Clone(status.NeedsReAdd)
	slices.SortFunc(channels, func(a, b id.ID) int { return bytes.Compare(a[:], b[:]) })
	channels = slices.Compact(channels)
	// Only the Bot channels the caller can view, as the Bot channel list (Task 22 fix round 1, L3).
	channels = slices.DeleteFunc(channels, func(c id.ID) bool { return d.Feed.ChannelPermissions(req.UserID, c)&pollPermView == 0 })
	if len(channels) > botStatusChannelsKept {
		channels = channels[:botStatusChannelsKept]
	}
	ids := make([]any, 0, len(channels))
	for _, c := range channels {
		ids = append(ids, c[:])
	}
	starts := status.MusicStarts
	if len(starts) > botStatusStartsKept {
		starts = starts[len(starts)-botStatusStartsKept:]
	}
	rows := make([]any, 0, len(starts))
	for _, s := range starts {
		cold := uint64(0)
		if s.Cold {
			cold = 1
		}
		rows = append(rows, []any{uint64(max(s.FirstFrame.Milliseconds(), 0)), cold})
	}
	data, err := cborx.Marshal([]any{uint64(1), ids, rows})
	if err != nil {
		reply(sealedRefusal(interactionStatusInternal, true))
		return
	}
	body, err := cborx.Marshal([]any{nil, data})
	if err != nil {
		reply(sealedRefusal(interactionStatusInternal, true))
		return
	}
	reply(intake.Answer{Status: intake.StatusOK, Body: body, Final: true})
}
