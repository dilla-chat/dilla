// Package catalog is the Bot's per-community command catalog (Bot v2 Task 15): the commands of the
// enabled features, published to dillad with the command key (PUT /v1/bot-runtime/communities/{c}/
// catalog and …/command-key) on start, on a feature change, and whenever the helper rotated the key.
//
// The command table below is the single list of the operations the Bot offers over sealed commands;
// scripts/check-bot-operations.mjs reads it (every protocol.Op named in it must be a codec constant).
// Every command here has a sealed handler adapter in internal/bot/service (SealedAdapters), and
// TestEveryCatalogOperationHasASealedAdapter there holds the two lists together.
package catalog

import (
	"context"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/dilla-chat/dilla/internal/bot/protocol"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/id"
)

// Feature is an installation capability bit (protocol/10-community-bot.md; BotSection.tsx): a command is
// offered while its feature is enabled.
type Feature uint64

const (
	FeatureMusic         Feature = 1 << 0
	FeatureWelcome       Feature = 1 << 1
	FeatureRoles         Feature = 1 << 2
	FeatureAnnouncements Feature = 1 << 3
	FeaturePolls         Feature = 1 << 8
	FeatureReminders     Feature = 1 << 9
	// FeatureModeration is any of the four moderation capabilities (warnings, removal, kick, ban: bits 4 to 7), as v1
	// offered moderation evidence (Task 22): a feature mask matches when any of its bits is enabled.
	FeatureModeration Feature = 1<<4 | 1<<5 | 1<<6 | 1<<7
)

// FeatureExisting marks the operations on polls and reminders that already exist: they are offered whatever the
// installation's features, as v1 kept them (protocol/10: with Polls or Reminders off only creating one is refused;
// existing polls are still voted on, closed and read, and reminders still listed, cancelled, dismissed and delivered).
const FeatureExisting Feature = 0

// AllFeatures is every feature the catalog has commands for, in bit order.
func AllFeatures() []Feature {
	return []Feature{FeatureMusic, FeatureWelcome, FeatureRoles, FeatureAnnouncements, FeatureModeration, FeaturePolls, FeatureReminders}
}

// FeaturesOf is the catalog features an installation's capability mask enables, in bit order.
func FeaturesOf(capabilities uint64) []Feature {
	var out []Feature
	for _, f := range AllFeatures() {
		if capabilities&uint64(f) != 0 {
			out = append(out, f)
		}
	}
	return out
}

// Argument type codes (dillad's args_shape grammar).
const (
	ArgText     uint8 = 0
	ArgUint     uint8 = 1
	ArgUser     uint8 = 2
	ArgChannel  uint8 = 3
	ArgDuration uint8 = 4
	ArgBool     uint8 = 5
)

// Handler kinds. 0 is a picker command, answered with UTF-8 text. 1 is an operation an app surface sends
// (a sealed read, or an app operation: Task 21b), answered with CBOR; the picker does not offer it (the client
// shows only handler kind 0).
const (
	HandlerCommand uint64 = 0
	HandlerRead    uint64 = 1
)

// Arg is one argument's shape.
type Arg struct {
	Name     string
	Type     uint8
	Required bool
}

// Command is one catalog entry.
type Command struct {
	Feature         Feature
	Name            string
	Description     string
	Operation       protocol.Operation
	Args            []Arg
	Permission      uint64
	NeedsBotChannel bool
	HandlerKind     uint64
	// Scope is where the command acts: in the request's channel (a text, Bot or voice channel), or community-wide.
	// A channel-scoped command with no channel is refused, and its permission_bit is checked in that channel.
	Scope Scope
}

// Scope is a command's authorisation scope. The zero value is no scope: a command must declare one
// (TestEveryCommandDeclaresItsScope).
type Scope uint8

const (
	ScopeCommunity Scope = 1
	ScopeChannel   Scope = 2
)

// dillad's permission bits (internal/api/perm.go).
const (
	permSendMessages    uint64 = 1 << 1
	permManageRoles     uint64 = 1 << 15
	permManageCommunity uint64 = 1 << 16
)

// AppOperationBase is what an app operation adds to the v1 operation it runs (protocol.OpApp*).
const AppOperationBase protocol.Operation = 100

// appMutations are the app operations that change state: they carry an expected revision and, unlike the sealed reads,
// are refused to a timed-out member.
var appMutations = map[protocol.Operation]bool{
	protocol.OpAppMusicEnqueue: true, protocol.OpAppMusicPause: true, protocol.OpAppMusicResume: true, protocol.OpAppMusicSkip: true,
	protocol.OpAppMusicStop: true, protocol.OpAppMusicSeek: true, protocol.OpAppMusicVolume: true, protocol.OpAppMusicShuffle: true,
	protocol.OpAppMusicRepeat: true, protocol.OpAppMusicRemove: true, protocol.OpAppMusicMove: true, protocol.OpAppMusicClear: true,
	protocol.OpAppMusicAnnounce: true, protocol.OpAppRolePanelPublish: true, protocol.OpAppAnnouncementsPut: true,
	protocol.OpAppAnnouncementsDelete: true, protocol.OpAppPollCreate: true, protocol.OpAppPollVote: true, protocol.OpAppPollClose: true,
	protocol.OpAppReminderCreate: true, protocol.OpAppReminderCancel: true, protocol.OpAppReminderDismiss: true,
	protocol.OpAppSettingsPut: true, protocol.OpAppModerationEvidence: true,
}

// AppMutation reports whether op is an app operation that changes state.
func AppMutation(op protocol.Operation) bool { return appMutations[op] }

// app is an app operation's hidden catalog entry (Task 21b).
func app(f Feature, name string, op protocol.Operation, perm uint64, scope Scope) Command {
	return Command{Feature: f, Name: name, Description: "For the app", Operation: op, Permission: perm, HandlerKind: HandlerRead, Scope: scope}
}

var commands = []Command{
	// Music: the voice room is the request's channel. The handlers check the call and DJ rights.
	{FeatureMusic, "play", "Play a song or add it to the queue", protocol.OpMusicEnqueue, []Arg{{"query-or-url", ArgText, true}}, 0, false, HandlerCommand, ScopeChannel},
	{FeatureMusic, "pause", "Pause the music", protocol.OpMusicPause, nil, 0, false, HandlerCommand, ScopeChannel},
	{FeatureMusic, "resume", "Resume the music", protocol.OpMusicResume, nil, 0, false, HandlerCommand, ScopeChannel},
	{FeatureMusic, "skip", "Skip to the next song", protocol.OpMusicSkip, nil, 0, false, HandlerCommand, ScopeChannel},
	{FeatureMusic, "stop", "Stop the music and leave", protocol.OpMusicStop, nil, 0, false, HandlerCommand, ScopeChannel},
	{FeatureMusic, "shuffle", "Shuffle the queue", protocol.OpMusicShuffle, nil, 0, false, HandlerCommand, ScopeChannel},
	{FeatureMusic, "volume", "Set the volume, 0 to 100", protocol.OpMusicVolume, []Arg{{"level", ArgUint, true}}, 0, false, HandlerCommand, ScopeChannel},
	{FeatureMusic, "repeat", "Repeat off, one or all", protocol.OpMusicRepeat, []Arg{{"mode", ArgText, true}}, 0, false, HandlerCommand, ScopeChannel},
	{FeatureMusic, "queue clear", "Clear the queue", protocol.OpMusicClear, nil, 0, false, HandlerCommand, ScopeChannel},
	{FeatureMusic, "queue", "Show the queue", protocol.OpMusicState, nil, 0, false, HandlerCommand, ScopeChannel},
	{FeatureMusic, "nowplaying", "Show the song that is playing", protocol.OpMusicState, nil, 0, false, HandlerCommand, ScopeChannel},
	{FeatureMusic, "music state", "The player's state, for the app", protocol.OpReadMusic, nil, 0, false, HandlerRead, ScopeChannel},
	// Polls: create posts into a Bot channel; the card reads through the sealed read.
	{FeaturePolls, "poll create", "Ask a question with up to 10 answers", protocol.OpPollCreate, []Arg{
		{"question", ArgText, false}, {"answers", ArgText, false}, {"multiple", ArgBool, false}, {"duration", ArgDuration, false}, {"channel", ArgChannel, false},
	}, permSendMessages, true, HandlerCommand, ScopeChannel},
	{FeatureExisting, "poll get", "A poll's state, for the app", protocol.OpReadPoll, []Arg{{"poll", ArgText, true}}, 0, false, HandlerRead, ScopeCommunity},
	// Reminders.
	{FeatureReminders, "remind", "Remind me later", protocol.OpReminderCreate, []Arg{{"when", ArgDuration, true}, {"text", ArgText, true}}, 0, false, HandlerCommand, ScopeCommunity},
	{FeatureReminders, "reminders list", "Your upcoming reminders", protocol.OpReminderList, nil, 0, false, HandlerCommand, ScopeCommunity},
	{FeatureExisting, "reminders due", "Your due reminders, for the app", protocol.OpReadRemindersDue, []Arg{{"cursor", ArgText, false}}, 0, false, HandlerRead, ScopeCommunity},
	// App operations (Task 21b): the player and its dock act in the voice channel; the handlers check the call and DJ
	// rights as before.
	app(FeatureMusic, "music search", protocol.OpAppMusicSearch, 0, ScopeChannel),
	app(FeatureMusic, "music enqueue", protocol.OpAppMusicEnqueue, 0, ScopeChannel),
	app(FeatureMusic, "music pause", protocol.OpAppMusicPause, 0, ScopeChannel),
	app(FeatureMusic, "music resume", protocol.OpAppMusicResume, 0, ScopeChannel),
	app(FeatureMusic, "music skip", protocol.OpAppMusicSkip, 0, ScopeChannel),
	app(FeatureMusic, "music stop", protocol.OpAppMusicStop, 0, ScopeChannel),
	app(FeatureMusic, "music seek", protocol.OpAppMusicSeek, 0, ScopeChannel),
	app(FeatureMusic, "music volume", protocol.OpAppMusicVolume, 0, ScopeChannel),
	app(FeatureMusic, "music shuffle", protocol.OpAppMusicShuffle, 0, ScopeChannel),
	app(FeatureMusic, "music repeat", protocol.OpAppMusicRepeat, 0, ScopeChannel),
	app(FeatureMusic, "music remove", protocol.OpAppMusicRemove, 0, ScopeChannel),
	app(FeatureMusic, "music move", protocol.OpAppMusicMove, 0, ScopeChannel),
	app(FeatureMusic, "music clear", protocol.OpAppMusicClear, 0, ScopeChannel),
	app(FeatureMusic, "music announce", protocol.OpAppMusicAnnounce, 0, ScopeChannel),
	// The provider status names no channel; its handler admits only those who manage the community.
	app(FeatureMusic, "music providers", protocol.OpAppMusicProvidersStatus, 0, ScopeCommunity),
	app(FeatureRoles, "rolepanel publish", protocol.OpAppRolePanelPublish, permManageRoles, ScopeCommunity),
	app(FeatureAnnouncements, "announcement rows", protocol.OpAppAnnouncementsList, permManageCommunity, ScopeCommunity),
	app(FeatureAnnouncements, "announcement save", protocol.OpAppAnnouncementsPut, permManageCommunity, ScopeCommunity),
	app(FeatureAnnouncements, "announcement remove", protocol.OpAppAnnouncementsDelete, permManageCommunity, ScopeCommunity),
	// The poll form posts into the command's channel (the dispatcher refuses a payload naming another one), so the
	// intake authorises send_messages there.
	app(FeaturePolls, "poll form", protocol.OpAppPollCreate, permSendMessages, ScopeChannel),
	app(FeatureExisting, "poll list", protocol.OpAppPollList, 0, ScopeCommunity),
	app(FeatureExisting, "poll vote", protocol.OpAppPollVote, 0, ScopeCommunity),
	app(FeatureExisting, "poll close", protocol.OpAppPollClose, 0, ScopeCommunity),
	app(FeatureExisting, "poll voters", protocol.OpAppPollVoters, 0, ScopeCommunity),
	app(FeatureReminders, "reminder save", protocol.OpAppReminderCreate, 0, ScopeCommunity),
	app(FeatureExisting, "reminders page", protocol.OpAppReminderList, 0, ScopeCommunity),
	app(FeatureExisting, "reminder cancel", protocol.OpAppReminderCancel, 0, ScopeCommunity),
	app(FeatureExisting, "reminder dismiss", protocol.OpAppReminderDismiss, 0, ScopeCommunity),
	// Task 22: the admin settings pages and moderation evidence. The welcome handlers check manage_community again; the
	// evidence's view_audit_log-or-manage_messages check is the dispatcher's (one permission_bit says "all of").
	app(FeatureWelcome, "welcome settings", protocol.OpAppSettingsGet, permManageCommunity, ScopeCommunity),
	app(FeatureWelcome, "welcome save", protocol.OpAppSettingsPut, permManageCommunity, ScopeCommunity),
	app(FeatureModeration, "moderation evidence", protocol.OpAppModerationEvidence, 0, ScopeCommunity),
	// The Bot page's status: the Bot channels it needs re-adding to and the last music starts.
	app(FeatureExisting, "bot status", protocol.OpReadBotStatus, permManageCommunity, ScopeCommunity),
}

// Commands is the catalog's commands for features, in table order.
func Commands(features []Feature) []Command {
	var mask Feature
	for _, f := range features {
		mask |= f
	}
	var out []Command
	for _, c := range commands {
		if mask&c.Feature != 0 || c.Feature == FeatureExisting {
			out = append(out, c)
		}
	}
	return out
}

// Build is the deterministic CBOR catalog of features' commands: [[name, description, operation,
// args_shape, permission_bit, needs_bot_channel, handler_kind]…], args_shape [[name, type_code,
// required]…].
func Build(features []Feature) []byte {
	cmds := Commands(features)
	rows := make([]any, 0, len(cmds))
	for _, c := range cmds {
		args := make([]any, 0, len(c.Args))
		for _, a := range c.Args {
			args = append(args, []any{a.Name, uint64(a.Type), boolUint(a.Required)})
		}
		rows = append(rows, []any{c.Name, c.Description, uint64(c.Operation), args, c.Permission, boolUint(c.NeedsBotChannel), c.HandlerKind})
	}
	b, err := cborx.Marshal(rows)
	if err != nil {
		// Strings, uints and arrays only.
		panic(err)
	}
	return b
}

// Rule is how the intake authorises an offered operation: its permission_bit, whether it is a sealed read, whether
// it acts in the request's channel, and the indices of its channel-typed arguments (which must name that channel).
type Rule struct {
	Permission    uint64
	Read          bool
	ChannelScoped bool
	ChannelArgs   []int
}

// Operations is each offered operation's rule for features: what the intake authorises by.
func Operations(features []Feature) map[uint64]Rule {
	out := map[uint64]Rule{}
	for _, c := range Commands(features) {
		r := Rule{Permission: c.Permission, Read: c.HandlerKind == HandlerRead && !AppMutation(c.Operation), ChannelScoped: c.Scope != ScopeCommunity}
		for i, a := range c.Args {
			if a.Type == ArgChannel {
				r.ChannelArgs = append(r.ChannelArgs, i)
			}
		}
		out[uint64(c.Operation)] = r
	}
	return out
}

func boolUint(b bool) uint64 {
	if b {
		return 1
	}
	return 0
}

// Session is the Bot runtime routes the publisher sends (internal/bot/client.Session).
type Session interface {
	PublishCatalog(ctx context.Context, community id.ID, commands []byte) (uint64, error)
	PublishCommandKey(ctx context.Context, community id.ID, publication []byte) error
}

// KeySource is the helper's op 21 (helper.Client.CommandKey): it mints and rotates the key.
type KeySource interface {
	CommandKey(ctx context.Context, community id.ID, now time.Time) ([]byte, error)
}

// KeyCheckEvery is how often the publisher asks the helper for the current key. The helper rotates a
// key at 7 days; clients refuse it at 8 days less 10 minutes, so an hourly check leaves about 22 h 50
// min of the ~23 h 50 min window to publish in. The check is a local helper op: a request is sent only
// when the key's bytes changed.
const KeyCheckEvery = time.Hour

// retryAfter spaces publishes after a failure (dillad unreachable, the Bot not ready yet).
const (
	retryFirst = 2 * time.Second
	retryMax   = 2 * time.Minute
)

// Publisher keeps one community's catalog and command key published. Publish sends only what changed
// since the last successful publish.
type Publisher struct {
	keys     KeySource
	session  Session
	clk      clock.Clock
	features func() []Feature
	log      *slog.Logger
	wake     chan struct{}

	mu      sync.Mutex
	key     []byte
	catalog []byte
	// forced is when Stale last forced a republish (fix round 2, N-3).
	forced time.Time
}

// StaleRepublishEvery throttles Stale: a forced republish at most once a minute per community, so commands sealed to
// an unknown key cannot make the Bot rewrite its publication at dillad on every command.
const StaleRepublishEvery = time.Minute

// NewPublisher publishes the catalog of features() and the helper's current key through session.
func NewPublisher(keys KeySource, session Session, clk clock.Clock, features func() []Feature) *Publisher {
	if clk == nil {
		clk = clock.System()
	}
	return &Publisher{keys: keys, session: session, clk: clk, features: features, log: slog.New(slog.DiscardHandler), wake: make(chan struct{}, 1)}
}

// SetLogger sets the publisher's log.
func (p *Publisher) SetLogger(l *slog.Logger) {
	if l != nil {
		p.log = l
	}
}

// Changed asks Run to publish now: a feature changed, or a command was sealed to a key the helper no
// longer holds. Asks coalesce.
func (p *Publisher) Changed() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// Publish asks the helper for the current key and sends the key, then the catalog, each only when its
// bytes differ from the last ones dillad accepted. A new key bumps dillad's catalog revision itself.
func (p *Publisher) Publish(ctx context.Context, community id.ID) error {
	key, err := p.keys.CommandKey(ctx, community, p.clk.Now())
	if err != nil {
		return err
	}
	catalog := Build(p.features())
	p.mu.Lock()
	keyChanged, catalogChanged := !slices.Equal(key, p.key), !slices.Equal(catalog, p.catalog)
	p.mu.Unlock()
	if keyChanged {
		if err := p.session.PublishCommandKey(ctx, community, key); err != nil {
			return err
		}
		p.mu.Lock()
		p.key = key
		p.mu.Unlock()
	}
	if catalogChanged {
		if _, err := p.session.PublishCatalog(ctx, community, catalog); err != nil {
			return err
		}
		p.mu.Lock()
		p.catalog = catalog
		p.mu.Unlock()
	}
	return nil
}

// Stale is a command sealed to a key the helper does not hold: dillad may hold an older publication, or none after a
// restore, so the key and the catalog are sent again even with unchanged bytes, at most once per
// StaleRepublishEvery; in between it only asks for an ordinary publish.
func (p *Publisher) Stale() {
	now := p.clk.Now()
	p.mu.Lock()
	force := p.forced.IsZero() || now.Sub(p.forced) >= StaleRepublishEvery
	if force {
		p.forced = now
		p.key, p.catalog = nil, nil
	}
	p.mu.Unlock()
	p.Changed()
}

// Forget makes the next Publish send both again (a new Bot session: dillad may have dropped them).
func (p *Publisher) Forget() {
	p.mu.Lock()
	p.key, p.catalog = nil, nil
	p.mu.Unlock()
}

// Run publishes at once, then every KeyCheckEvery and on Changed, until ctx ends. A failed publish is
// tried again after a backoff (2 s doubling to 2 min) instead of waiting for the hour.
func (p *Publisher) Run(ctx context.Context, community id.ID) {
	retry := time.Duration(0)
	for {
		wait := KeyCheckEvery
		if err := p.Publish(ctx, community); err != nil {
			if ctx.Err() != nil {
				return
			}
			retry = min(max(retry*2, retryFirst), retryMax)
			wait = retry
			p.log.WarnContext(ctx, "bot command catalog not published; trying again", "after", wait, "error", err.Error())
		} else {
			retry = 0
		}
		timer := p.clk.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C():
		case <-p.wake:
			timer.Stop()
		}
	}
}
