package service

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/dilla-chat/dilla/internal/bot/client"
	"github.com/dilla-chat/dilla/internal/bot/helper"
	"github.com/dilla-chat/dilla/internal/bot/native"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
)

const (
	membershipStateArea = 8
	membershipStateKey  = "dilla.bot.membership.v1"
	defaultWelcomePage  = 32
	maxMembershipGroups = 64
)

var ErrMembershipUnproven = errors.New("welcome membership target is not currently authorized")

var ErrMembershipReconcileBusy = errors.New("membership reconciliation is already active")

// MembershipPhase identifies a fixed, non-sensitive step of membership reconciliation.
type MembershipPhase uint8

const (
	MembershipPhaseUnknown MembershipPhase = iota
	MembershipPhaseWelcomeFetch
	MembershipPhaseWelcomeApply
	MembershipPhaseWelcomeRevalidation
	MembershipPhaseWelcomeAcknowledge
	MembershipPhaseTextSourceSync
	MembershipPhaseCallSourceSync
	MembershipPhaseInteractionSourceSync
)

func (p MembershipPhase) String() string {
	switch p {
	case MembershipPhaseWelcomeFetch:
		return "welcome_fetch"
	case MembershipPhaseWelcomeApply:
		return "welcome_apply"
	case MembershipPhaseWelcomeRevalidation:
		return "welcome_revalidation"
	case MembershipPhaseWelcomeAcknowledge:
		return "welcome_acknowledge"
	case MembershipPhaseTextSourceSync:
		return "text_source_sync"
	case MembershipPhaseCallSourceSync:
		return "call_source_sync"
	case MembershipPhaseInteractionSourceSync:
		return "interaction_source_sync"
	default:
		return "unknown"
	}
}

// MembershipPhaseError carries a fixed diagnostic phase while preserving the original cause
// for programmatic inspection. Its Error text intentionally excludes request, group, and body data.
type MembershipPhaseError struct {
	Phase MembershipPhase
	err   error
}

func (e *MembershipPhaseError) Error() string { return "membership reconciliation phase failed" }
func (e *MembershipPhaseError) Unwrap() error { return e.err }

func membershipPhaseError(phase MembershipPhase, err error) error {
	if err == nil {
		return nil
	}
	return &MembershipPhaseError{Phase: phase, err: err}
}

type membershipNative interface {
	sourceSyncNative
	Binding() native.Binding
	Groups(context.Context) ([]native.GroupInfo, error)
	ApplyWelcomes(context.Context, []native.GroupWelcome, []native.GroupExpected) ([]native.GroupWelcomeOutcome, error)
	StateGet(context.Context, uint64, []byte) (native.StateValue, error)
	StateTransaction(context.Context, id.ID, []native.StateChange) ([]uint64, error)
}
type membershipSession interface {
	Community(context.Context, id.ID) (client.Community, error)
	Channels(context.Context, id.ID) ([]client.Channel, error)
	Welcomes(context.Context, uint64, uint64) ([]client.Welcome, error)
	AcknowledgeWelcome(context.Context, uint64) error
	sourceSyncSession
}
type membershipOptions struct{ WelcomePageLimit uint64 }
type MembershipResult struct {
	Applied, Acknowledged, GroupsSynced, SourcePages uint64
	More                                             bool
}
type MembershipReconciler struct {
	mu        sync.Mutex
	active    bool
	native    membershipNative
	binding   native.Binding
	source    *SourceSync
	pageLimit uint64
	idlePolls *InteractionPollSchedule
	// botChannels is the community's Bot channels as the Bot's feed last applied them (Bot v2 Task 16); nil keeps v1
	// membership (every authorised channel group).
	botChannels BotChannels
	log         *slog.Logger
	// reAdd is the Bot channels last reported as needing the Bot re-added (logged once each).
	reAdd map[id.ID]bool
}

// BotChannels is the reconciler's link to the Bot's event feed (Bot v2 Task 16).
type BotChannels interface {
	// Marked reports the community's Bot channels as the feed last applied them, and whether the feed has applied a
	// snapshot (known). Until known, membership is not narrowed: an empty set before the first snapshot must not leave
	// every group.
	Marked() (marked map[id.ID]bool, known bool)
	// Membership receives, after each reconcile with the Bot channels known, the text groups the Bot has left (no
	// trust work for them, fix round 1 M-5) and the Bot channels whose group the Bot cannot join on its own (no stored
	// Welcome, or a stale one): a member's client must add the Bot again (the admin page shows it, Task 22).
	Membership(left, needsReAdd map[id.ID]bool)
}

// BotChannelsFunc is a BotChannels from a Marked function alone; Membership is dropped.
type BotChannelsFunc func() (map[id.ID]bool, bool)

func (f BotChannelsFunc) Marked() (map[id.ID]bool, bool) { return f() }
func (BotChannelsFunc) Membership(_, _ map[id.ID]bool)   {}

// Deferred Welcomes (fix round 1, I-1): a Welcome to a channel that is not a Bot channel is never applied (that would
// let the Bot read a non-Bot channel), but it is kept unapplied, the newest per group, for at most maxDeferredWelcomes
// groups, so marking the channel later joins the Bot without a second Welcome.
const (
	deferredWelcomeIndexKey = "dilla.bot.membership.deferred-welcomes"
	deferredWelcomePrefix   = "dilla.bot.membership.deferred-welcome."
	maxDeferredWelcomes     = maxMembershipGroups
	maxDeferredWelcomeBytes = 256 << 10
)

// botChannelMigrationKey marks that the Bot's first start under v2 has left every channel group that is not a Bot
// channel (state area membershipStateArea): the one-time migration is not repeated.
const botChannelMigrationKey = "dilla.bot.membership.v2.bot-channels-only"

// UseBotChannels makes the reconciler keep the Bot in Bot channel groups only (Bot v2 Task 16):
//   - a Welcome to a text channel that is not a Bot channel is acknowledged and not applied;
//   - a text channel group whose channel is not (or no longer) a Bot channel is left: never synced, so nothing of it
//     is read, applied or answered. Marking the channel joins it again: its Welcome is applied and its group synced on
//     the pass the mark asks for;
//   - the first reconcile with the Bot channels known logs "bot left non-Bot channel group" with the channel id for
//     every such group and persists botChannelMigrationKey, once.
//
// Call groups are not narrowed: the Bot is in a call group while it plays music.
func (r *MembershipReconciler) UseBotChannels(channels BotChannels, log *slog.Logger) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.botChannels = channels
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	r.log = log
}

// botChannelSet is the reconciler's Bot channels now. linked is false only with no feed link (v1 membership); with
// one, known is false until the feed has applied a snapshot.
func (r *MembershipReconciler) botChannelSet() (marked map[id.ID]bool, linked, known bool, log *slog.Logger) {
	r.mu.Lock()
	channels, log := r.botChannels, r.log
	r.mu.Unlock()
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if channels == nil {
		return nil, false, false, log
	}
	marked, known = channels.Marked()
	return marked, true, known, log
}

// NeedsReAdd is the Bot channels the last reconcile found the Bot cannot join on its own: a member's client must add
// it again (fix round 1, I-1).
func (r *MembershipReconciler) NeedsReAdd() map[id.ID]bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[id.ID]bool, len(r.reAdd))
	for k := range r.reAdd {
		out[k] = true
	}
	return out
}

// InteractionPollSchedule retains background backoff across reconciliation passes.
// A schedule belongs to one installation, outliving its helper/reconciler objects.
type InteractionPollSchedule struct {
	mu     sync.Mutex
	groups map[id.ID]interactionIdlePoll
}

// UseInteractionPollSchedule is called before polling when a reconciler is rebuilt.
func (r *MembershipReconciler) UseInteractionPollSchedule(schedule *InteractionPollSchedule) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if schedule != nil {
		r.idlePolls = schedule
	}
}

// Only background sweeps back off; a gateway message always bypasses this cache.
type interactionIdlePoll struct {
	next       time.Time
	delay      time.Duration
	descriptor native.InteractionGroupDescriptor
}

// NewMembershipReconciler binds bounded membership work to one opened installation helper.
func NewMembershipReconciler(n membershipNative, sourceOptions SourceSyncOptions, welcomePageLimit uint64) (*MembershipReconciler, error) {
	if n == nil {
		return nil, errors.New("missing native client")
	}
	return newMembershipReconciler(n, sourceSyncOptions{PageLimit: sourceOptions.PageLimit, PageBudget: sourceOptions.PageBudget}, membershipOptions{WelcomePageLimit: welcomePageLimit})
}
func newMembershipReconciler(n membershipNative, sourceOptions sourceSyncOptions, options membershipOptions) (*MembershipReconciler, error) {
	if n == nil {
		return nil, errors.New("missing native client")
	}
	b := n.Binding()
	if b.Instance.IsZero() || b.Community.IsZero() || b.Installation.IsZero() {
		return nil, errors.New("invalid membership binding")
	}
	if options.WelcomePageLimit == 0 {
		options.WelcomePageLimit = defaultWelcomePage
	}
	if options.WelcomePageLimit > 64 {
		return nil, errors.New("welcome page bound exceeds 64")
	}
	source, err := newSourceSync(n, sourceOptions)
	if err != nil {
		return nil, err
	}
	return &MembershipReconciler{native: n, binding: b, source: source, pageLimit: options.WelcomePageLimit, idlePolls: &InteractionPollSchedule{}}, nil
}

// ReconcileOnce reads the bound community's current authorized channel view, applies one
// bounded Welcome page idempotently, then catches up currently accessible native groups.
func (r *MembershipReconciler) ReconcileOnce(ctx context.Context, session *client.Session) (MembershipResult, error) {
	if session == nil {
		return MembershipResult{}, errors.New("missing device session")
	}
	return r.reconcileOnce(ctx, session)
}
func (r *MembershipReconciler) reconcileOnce(ctx context.Context, session membershipSession) (MembershipResult, error) {
	var out MembershipResult
	if r == nil || session == nil {
		return out, errors.New("invalid membership reconciliation")
	}
	r.mu.Lock()
	if r.active {
		r.mu.Unlock()
		return MembershipResult{More: true}, ErrMembershipReconcileBusy
	}
	r.active = true
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.active = false
		r.mu.Unlock()
	}()
	if err := client.CooperativeCheckpoint(ctx); err != nil {
		return out, err
	}
	own, err := session.Community(ctx, r.binding.Community)
	if err != nil {
		return out, err
	}
	if own.ID != r.binding.Community {
		return out, errors.New("installation community is no longer accessible")
	}
	channels, err := session.Channels(ctx, r.binding.Community)
	if err != nil {
		return out, err
	}
	channelByID := make(map[id.ID]client.Channel, len(channels))
	seenChannels := make(map[id.ID]bool, len(channels))
	textGroup := make(map[id.ID]client.Channel)
	for _, ch := range channels {
		if ch.ID.IsZero() || ch.CommunityID != r.binding.Community || ch.Kind > 2 || seenChannels[ch.ID] {
			return out, client.ErrProtocol
		}
		seenChannels[ch.ID] = true
		// Categories are returned by the community channel list but have no MLS group to sync.
		if ch.Kind == 2 {
			if ch.TextGroupID != nil {
				return out, client.ErrProtocol
			}
			continue
		}
		channelByID[ch.ID] = ch
		if ch.Kind == 0 && ch.TextGroupID != nil && !ch.TextGroupID.IsZero() {
			if prior, exists := textGroup[*ch.TextGroupID]; exists && prior.ID != ch.ID {
				return out, client.ErrProtocol
			}
			textGroup[*ch.TextGroupID] = ch
		}
	}
	marked, linked, known, log := r.botChannelSet()
	// participates: a text channel's group is one the Bot is in only while the channel is a Bot channel. With a feed
	// link, no text channel participates until the feed has applied a snapshot (fix round 2, N-2, fail closed): no
	// Welcome is applied (it is kept) and no text group is synced before the Bot knows its Bot channels.
	participates := func(ch client.Channel) bool { return !linked || ch.Kind != 0 || known && marked[ch.ID] }
	groups, err := r.native.Groups(ctx)
	if err != nil {
		return out, err
	}
	if len(groups) > maxMembershipGroups {
		return out, errors.New("native membership group set exceeds bound")
	}
	seenGroups := make(map[id.ID]bool, len(groups))
	activeCall := map[id.ID]native.GroupInfo{}
	for _, g := range groups {
		if g.ID.IsZero() || seenGroups[g.ID] {
			return out, client.ErrProtocol
		}
		seenGroups[g.ID] = true
		if g.State == 2 && g.Kind == 1 && g.Community != nil && *g.Community == r.binding.Community {
			if ch, ok := channelByID[g.Target]; ok && ch.Kind == 1 {
				activeCall[g.ID] = g
			}
		}
	}
	cursor, revision, err := r.loadMembershipCursor(ctx)
	if err != nil {
		return out, err
	}
	welcomes, err := session.Welcomes(ctx, cursor, r.pageLimit)
	if err != nil {
		return out, membershipPhaseError(MembershipPhaseWelcomeFetch, err)
	}
	if uint64(len(welcomes)) > r.pageLimit {
		return out, client.ErrProtocol
	}
	// Welcomes to a channel that is not a Bot channel are acknowledged without being applied (Bot v2 Task 16).
	all := welcomes
	var refused []client.Welcome
	if linked {
		kept := make([]client.Welcome, 0, len(welcomes))
		for _, w := range welcomes {
			if c, ok := textGroup[w.GroupID]; ok && !participates(c) {
				if w.ID <= cursor || w.ID > math.MaxInt64 {
					return out, client.ErrProtocol
				}
				refused = append(refused, w)
				continue
			}
			kept = append(kept, w)
		}
		welcomes = kept
	}
	applied := map[id.ID]bool{}
	if len(welcomes) > 0 {
		apply := make([]native.GroupWelcome, 0, len(welcomes))
		expected := make([]native.GroupExpected, 0, len(welcomes))
		expectedGroups := make(map[id.ID]bool, len(welcomes))
		channelByGroup := make(map[id.ID]id.ID, len(welcomes))
		kindByGroup := make(map[id.ID]uint64, len(welcomes))
		for _, w := range welcomes {
			if w.ID <= cursor || w.ID > math.MaxInt64 || w.GroupID.IsZero() || len(w.TreeHash) != 32 || len(w.Blob) == 0 || len(w.Tree) == 0 {
				return out, client.ErrProtocol
			}
			var ch client.Channel
			var kind uint64
			if c, ok := textGroup[w.GroupID]; ok {
				ch = c
				kind = 0
			} else if g, ok := activeCall[w.GroupID]; ok {
				ch = channelByID[g.Target]
				kind = 1
			} else {
				return out, ErrMembershipUnproven
			}
			if ch.CommunityID != r.binding.Community || ch.ID.IsZero() {
				return out, ErrMembershipUnproven
			}
			apply = append(apply, native.GroupWelcome{ID: w.ID, Group: w.GroupID, Epoch: w.Epoch, CommitSequence: w.CommitSequence, Welcome: w.Blob, RatchetTree: w.Tree, TreeHash: bytes32(w.TreeHash)})
			if prior, ok := channelByGroup[w.GroupID]; ok && prior != ch.ID {
				return out, ErrMembershipUnproven
			}
			channelByGroup[w.GroupID] = ch.ID
			kindByGroup[w.GroupID] = kind
			if !expectedGroups[w.GroupID] {
				expected = append(expected, native.GroupExpected{Group: w.GroupID, Community: r.binding.Community, Channel: ch.ID, Kind: kind, Policy: own.PolicyVersion})
				expectedGroups[w.GroupID] = true
			}
		}
		outcomes, applyErr := r.native.ApplyWelcomes(ctx, apply, expected)
		if applyErr != nil {
			return out, membershipPhaseError(MembershipPhaseWelcomeApply, applyErr)
		}
		if len(outcomes) != len(welcomes) {
			return out, errors.New("native welcome outcome count mismatch")
		}
		for i, o := range outcomes {
			if o.ID != welcomes[i].ID || o.Group != welcomes[i].GroupID || (o.Outcome != 0 && o.Outcome != 1) {
				return out, fmt.Errorf("native welcome %d was not durably applied: outcome %d", o.ID, o.Outcome)
			}
			if err = r.revalidateChannelAccess(ctx, session, o.Group, channelByGroup[o.Group], kindByGroup[o.Group]); err != nil {
				return out, membershipPhaseError(MembershipPhaseWelcomeRevalidation, err)
			}
			if err = session.AcknowledgeWelcome(ctx, o.ID); err != nil {
				return out, membershipPhaseError(MembershipPhaseWelcomeAcknowledge, err)
			}
			out.Applied++
			out.Acknowledged++
			applied[o.Group] = true
		}
	}
	for _, w := range refused {
		ch := textGroup[w.GroupID]
		// Kept unapplied before it is acknowledged: a failure to keep it leaves it on the server for the next pass.
		stored, storeErr := r.storeDeferredWelcome(ctx, w, ch.ID)
		if storeErr != nil {
			return out, fmt.Errorf("keep deferred welcome: %w", storeErr)
		}
		if err = session.AcknowledgeWelcome(ctx, w.ID); err != nil {
			return out, membershipPhaseError(MembershipPhaseWelcomeAcknowledge, err)
		}
		out.Acknowledged++
		if stored {
			log.Info("bot welcome to a non-Bot channel group kept unapplied until the channel is marked", "channel_id", ch.ID.String())
		} else {
			log.Warn("bot welcome to a non-Bot channel group not kept (too large or too many); marking the channel needs the Bot re-added", "channel_id", ch.ID.String())
		}
	}
	if len(all) > 0 {
		last := all[len(all)-1].ID
		_, err = r.saveMembershipCursor(ctx, revision, last)
		if err != nil {
			return out, fmt.Errorf("persist welcome cursor: %w", err)
		}
		out.More = uint64(len(all)) == r.pageLimit
	}
	if known {
		if err = r.leaveNonBotChannelsOnce(ctx, groups, channelByID, participates, log); err != nil {
			return out, err
		}
		joined, joinErr := r.reconcileBotChannels(ctx, session, own, groups, applied, textGroup, channelByID, participates, log)
		if joinErr != nil {
			return out, joinErr
		}
		if joined > 0 {
			out.Applied += joined
			out.More = true // the next pass syncs the groups just joined
		}
	}
	for _, g := range groups {
		if err = client.CooperativeCheckpoint(ctx); err != nil {
			return out, err
		}
		if g.State != 2 || g.Community == nil || *g.Community != r.binding.Community {
			continue
		}
		if g.Kind > 1 {
			continue
		}
		ch, ok := channelByID[g.Target]
		if !ok || ch.Kind != g.Kind {
			continue
		}
		if g.Kind == 0 && (ch.TextGroupID == nil || *ch.TextGroupID != g.ID) {
			continue
		}
		if !participates(ch) {
			continue // not a Bot channel: the Bot has left its group (Bot v2 Task 16)
		}
		res, e := r.source.sync(ctx, session, native.GroupExpected{Group: g.ID, Community: r.binding.Community, Channel: ch.ID, Kind: g.Kind})
		if e != nil {
			// A locally held group can outlive its server membership. Ordinary
			// member-only reads answer 404; the managed-session gate answers 403
			// for a closed call group. Calls are optional, so neither refusal may
			// block the installation's text/interaction reconciliation. Do not
			// apply or acknowledge denied sources; retry on the next pass, with
			// fresh call admission still required before music can publish.
			var apiErr *client.APIError
			if errors.As(e, &apiErr) && (apiErr.Status == 404 && apiErr.Code == "E_NOT_FOUND" ||
				g.Kind == 1 && apiErr.Status == 403 && apiErr.Code == "E_FORBIDDEN") {
				continue
			}
			phase := MembershipPhaseTextSourceSync
			if g.Kind == 1 {
				phase = MembershipPhaseCallSourceSync
			}
			return out, membershipPhaseError(phase, e)
		}
		out.More = out.More || res.More
		out.GroupsSynced++
		out.SourcePages += res.Pages
	}
	return out, nil
}

// ReconcileInteractionGroupsOnce catches up kind-3 interaction sources after the caller has
// refreshed authenticated device-list trust. Keeping this pass separate prevents a stale
// participant snapshot from blocking the membership pass that makes that snapshot current.
func (r *MembershipReconciler) ReconcileInteractionGroupsOnce(ctx context.Context, session *client.Session) (MembershipResult, error) {
	if session == nil {
		return MembershipResult{}, errors.New("missing device session")
	}
	return r.reconcileInteractionGroupsOnce(ctx, session)
}

// ReconcileInteractionGroupOnce catches up one active kind-3 interaction source: the gateway
// fast path for a participant's message reads that group alone. Every descriptor is still
// validated as in ReconcileInteractionGroupsOnce; an unknown or inactive group syncs nothing.
func (r *MembershipReconciler) ReconcileInteractionGroupOnce(ctx context.Context, session *client.Session, group id.ID) (MembershipResult, error) {
	if session == nil {
		return MembershipResult{}, errors.New("missing device session")
	}
	return r.reconcileInteractionGroupOnce(ctx, session, group)
}

// ReconcileInteractionGroupBackgroundOnce catches up one active interaction group
// at background priority, retaining the installation's idle polling schedule.
func (r *MembershipReconciler) ReconcileInteractionGroupBackgroundOnce(ctx context.Context, session *client.Session, group id.ID) (MembershipResult, error) {
	if session == nil {
		return MembershipResult{}, errors.New("missing device session")
	}
	return r.reconcileInteractionGroupBackgroundOnce(ctx, session, group)
}

func (r *MembershipReconciler) reconcileInteractionGroupBackgroundOnce(ctx context.Context, session membershipSession, group id.ID) (MembershipResult, error) {
	if group.IsZero() {
		return MembershipResult{}, errors.New("invalid interaction group")
	}
	return r.reconcileInteractionGroups(client.WithBackground(ctx), session, &group, false)
}

// InteractionGroupDelay reports the remaining background idle delay without any
// helper or HTTP work. A changed descriptor is immediately due.
func (r *MembershipReconciler) InteractionGroupDelay(descriptor native.InteractionGroupDescriptor) time.Duration {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	schedule := r.idlePolls
	r.mu.Unlock()
	schedule.mu.Lock()
	idle := schedule.groups[descriptor.Group]
	schedule.mu.Unlock()
	if idle.descriptor != descriptor {
		return 0
	}
	return max(0, idle.next.Sub(r.source.options.now()))
}

func (r *MembershipReconciler) reconcileInteractionGroupOnce(ctx context.Context, session membershipSession, group id.ID) (MembershipResult, error) {
	if group.IsZero() {
		return MembershipResult{}, errors.New("invalid interaction group")
	}
	return r.reconcileInteractionGroups(client.WithInteractive(ctx), session, &group, true)
}

func (r *MembershipReconciler) reconcileInteractionGroupsOnce(ctx context.Context, session membershipSession) (MembershipResult, error) {
	return r.reconcileInteractionGroups(ctx, session, nil, false)
}

func (r *MembershipReconciler) reconcileInteractionGroups(ctx context.Context, session membershipSession, only *id.ID, bypassIdle bool) (MembershipResult, error) {
	var out MembershipResult
	if r == nil || session == nil {
		return out, errors.New("invalid interaction membership reconciliation")
	}
	r.mu.Lock()
	schedule := r.idlePolls
	r.mu.Unlock()
	if err := client.CooperativeCheckpoint(ctx); err != nil {
		return out, err
	}
	groups, err := r.native.Groups(ctx)
	if err != nil {
		return out, err
	}
	if len(groups) > maxMembershipGroups {
		return out, errors.New("native membership group set exceeds bound")
	}
	seenGroups := make(map[id.ID]bool, len(groups))
	for _, g := range groups {
		if g.ID.IsZero() || seenGroups[g.ID] {
			return out, client.ErrProtocol
		}
		seenGroups[g.ID] = true
	}
	interactionNative, ok := r.native.(interface {
		InteractionGroups(context.Context) ([]native.InteractionGroupDescriptor, error)
	})
	if !ok {
		return out, nil
	}
	interactionGroups, err := interactionNative.InteractionGroups(ctx)
	if err != nil {
		return out, err
	}
	if len(interactionGroups) > maxMembershipGroups {
		return out, errors.New("native interaction group set exceeds bound")
	}
	schedule.mu.Lock()
	// Discard removed groups so the schedule stays bounded by the current set.
	active := make(map[id.ID]bool, len(interactionGroups))
	for _, g := range interactionGroups {
		active[g.Group] = true
	}
	for group := range schedule.groups {
		if !active[group] {
			delete(schedule.groups, group)
		}
	}
	schedule.mu.Unlock()
	for i, g := range interactionGroups {
		if err := client.CooperativeCheckpoint(ctx); err != nil {
			return out, err
		}
		if g.Group.IsZero() || seenGroups[g.Group] || i > 0 && string(interactionGroups[i-1].Group[:]) >= string(g.Group[:]) {
			return out, client.ErrProtocol
		}
		seenGroups[g.Group] = true
		b := g.Binding
		if b.Instance != r.binding.Instance || b.Installation != r.binding.Installation || b.Community != r.binding.Community ||
			b.BotUser.IsZero() || g.BotDevice.IsZero() || b.ParticipantUser.IsZero() || b.ParticipantDevice.IsZero() || b.PolicyVersion == 0 ||
			b.E2EEVersion != 1 || b.MediaVersion != 0 {
			return out, client.ErrProtocol
		}
		if g.State != 2 || only != nil && g.Group != *only {
			continue
		}
		now := r.source.options.now()
		schedule.mu.Lock()
		idle := schedule.groups[g.Group]
		schedule.mu.Unlock()
		if !bypassIdle && idle.descriptor == g && now.Before(idle.next) {
			continue
		}
		res, syncErr := r.source.sync(ctx, session, native.GroupExpected{Group: g.Group, Community: r.binding.Community, Channel: b.ParticipantUser, Kind: 3})
		if syncErr != nil {
			return out, membershipPhaseError(MembershipPhaseInteractionSourceSync, syncErr)
		}
		out.More = out.More || res.More
		out.GroupsSynced++
		out.SourcePages += res.Pages
		schedule.mu.Lock()
		if schedule.groups == nil {
			schedule.groups = make(map[id.ID]interactionIdlePoll)
		}
		if res.Pages == 0 {
			delay := min(max(idle.delay*2, time.Second), 30*time.Second)
			schedule.groups[g.Group] = interactionIdlePoll{next: now.Add(delay), delay: delay, descriptor: g}
		} else {
			delete(schedule.groups, g.Group)
		}
		schedule.mu.Unlock()
	}
	return out, nil
}

func (r *MembershipReconciler) revalidateChannelAccess(ctx context.Context, session membershipSession, group, channel id.ID, kind uint64) error {
	if group.IsZero() || channel.IsZero() || kind > 1 {
		return ErrMembershipUnproven
	}
	own, err := session.Community(ctx, r.binding.Community)
	if err != nil {
		return err
	}
	if own.ID != r.binding.Community {
		return errors.New("installation community is no longer accessible")
	}
	channels, err := session.Channels(ctx, r.binding.Community)
	if err != nil {
		return err
	}
	for _, current := range channels {
		if current.ID != channel || current.CommunityID != r.binding.Community || current.Kind != kind {
			continue
		}
		if kind == 0 {
			if current.TextGroupID != nil && *current.TextGroupID == group {
				return nil
			}
			return ErrMembershipUnproven
		}
		bound, inspectErr := r.native.InspectGroup(ctx, group)
		if inspectErr != nil {
			return inspectErr
		}
		if bound.ID == group && bound.State == 2 && bound.Kind == 1 && bound.Community != nil && *bound.Community == r.binding.Community && bound.Target == channel && !bound.PendingCommit {
			return nil
		}
		return ErrMembershipUnproven
	}
	return ErrMembershipUnproven
}
func bytes32(b []byte) (out [32]byte) { copy(out[:], b); return }

func deferredWelcomeKey(group id.ID) []byte {
	return []byte(deferredWelcomePrefix + hex.EncodeToString(group[:]))
}

// loadDeferredIndex reads the groups with a kept Welcome.
func (r *MembershipReconciler) loadDeferredIndex(ctx context.Context) ([]id.ID, uint64, error) {
	v, err := r.native.StateGet(ctx, membershipStateArea, []byte(deferredWelcomeIndexKey))
	if err != nil {
		return nil, 0, err
	}
	if v.Tombstone || len(v.Value) == 0 {
		return nil, v.Revision, nil
	}
	var a []any
	if cborx.Unmarshal(v.Value, &a) != nil || len(a) != 2 || a[0] != uint64(1) {
		return nil, 0, errors.New("invalid deferred welcome index")
	}
	raw, ok := a[1].([]any)
	if !ok || len(raw) > maxDeferredWelcomes {
		return nil, 0, errors.New("invalid deferred welcome index")
	}
	out := make([]id.ID, 0, len(raw))
	for _, x := range raw {
		b, ok := x.([]byte)
		if !ok || len(b) != id.Size {
			return nil, 0, errors.New("invalid deferred welcome index")
		}
		out = append(out, id.ID(b))
	}
	return out, v.Revision, nil
}

func encodeDeferredIndex(groups []id.ID) ([]byte, error) {
	raw := make([]any, 0, len(groups))
	for _, g := range groups {
		raw = append(raw, append([]byte(nil), g[:]...))
	}
	return cborx.Marshal([]any{uint64(1), raw})
}

// loadDeferredWelcome reads group's kept Welcome; ok is false when there is none.
func (r *MembershipReconciler) loadDeferredWelcome(ctx context.Context, group id.ID) (client.Welcome, id.ID, uint64, bool, error) {
	v, err := r.native.StateGet(ctx, membershipStateArea, deferredWelcomeKey(group))
	if err != nil {
		return client.Welcome{}, id.ID{}, 0, false, err
	}
	if v.Tombstone || len(v.Value) == 0 {
		return client.Welcome{}, id.ID{}, v.Revision, false, nil
	}
	var a []any
	if cborx.Unmarshal(v.Value, &a) != nil || len(a) != 9 || a[0] != uint64(1) {
		return client.Welcome{}, id.ID{}, 0, false, errors.New("invalid deferred welcome")
	}
	wid, ok1 := a[1].(uint64)
	g, ok2 := a[2].([]byte)
	ch, ok3 := a[3].([]byte)
	epoch, ok4 := a[4].(uint64)
	seq, ok5 := a[5].(uint64)
	blob, ok6 := a[6].([]byte)
	tree, ok7 := a[7].([]byte)
	hash, ok8 := a[8].([]byte)
	if !ok1 || !ok2 || !ok3 || !ok4 || !ok5 || !ok6 || !ok7 || !ok8 || len(g) != id.Size || len(ch) != id.Size || id.ID(g) != group || len(hash) != 32 {
		return client.Welcome{}, id.ID{}, 0, false, errors.New("invalid deferred welcome")
	}
	return client.Welcome{ID: wid, GroupID: group, Epoch: epoch, CommitSequence: seq, Blob: blob, Tree: tree, TreeHash: hash}, id.ID(ch), v.Revision, true, nil
}

// stateOperation is a deterministic transaction id for one state change of the reconciler.
func (r *MembershipReconciler) stateOperation(label string, parts ...[]byte) id.ID {
	seed := append([]byte("dilla.bot.membership."+label+".v1\x00"), r.binding.Installation[:]...)
	for _, p := range parts {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(p)))
		seed = append(append(seed, n[:]...), p...)
	}
	sum := sha256.Sum256(seed)
	return id.ID(sum[:16])
}

func revBytes(v uint64) []byte {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], v)
	return b[:]
}

// storeDeferredWelcome keeps w for channel's group, unapplied: the newest Welcome per group wins. It answers false when
// w cannot be kept (larger than a state value, or the bound of kept groups is reached).
func (r *MembershipReconciler) storeDeferredWelcome(ctx context.Context, w client.Welcome, channel id.ID) (bool, error) {
	index, indexRev, err := r.loadDeferredIndex(ctx)
	if err != nil {
		return false, err
	}
	old, _, rev, present, err := r.loadDeferredWelcome(ctx, w.GroupID)
	if err != nil {
		return false, err
	}
	if present && old.ID >= w.ID {
		return true, nil
	}
	listed := false
	for _, g := range index {
		listed = listed || g == w.GroupID
	}
	if !listed && len(index) >= maxDeferredWelcomes {
		return false, nil
	}
	value, err := cborx.Marshal([]any{uint64(1), w.ID, append([]byte(nil), w.GroupID[:]...), append([]byte(nil), channel[:]...),
		w.Epoch, w.CommitSequence, w.Blob, w.Tree, w.TreeHash})
	if err != nil {
		return false, err
	}
	if len(value) > maxDeferredWelcomeBytes {
		return false, nil
	}
	changes := []native.StateChange{{Area: membershipStateArea, Key: deferredWelcomeKey(w.GroupID), ExpectedRevision: rev, Value: value}}
	if !listed {
		next, encErr := encodeDeferredIndex(append(index, w.GroupID))
		if encErr != nil {
			return false, encErr
		}
		changes = append(changes, native.StateChange{Area: membershipStateArea, Key: []byte(deferredWelcomeIndexKey), ExpectedRevision: indexRev, Value: next})
	}
	op := r.stateOperation("deferred-welcome.keep", w.GroupID[:], revBytes(w.ID), revBytes(rev), revBytes(indexRev))
	if _, err = r.native.StateTransaction(ctx, op, changes); err != nil {
		return false, err
	}
	return true, nil
}

// dropDeferredWelcome forgets group's kept Welcome.
func (r *MembershipReconciler) dropDeferredWelcome(ctx context.Context, group id.ID, index []id.ID, indexRev uint64) error {
	_, _, rev, _, err := r.loadDeferredWelcome(ctx, group)
	if err != nil {
		return err
	}
	rest := make([]id.ID, 0, len(index))
	for _, g := range index {
		if g != group {
			rest = append(rest, g)
		}
	}
	next, err := encodeDeferredIndex(rest)
	if err != nil {
		return err
	}
	changes := []native.StateChange{{Area: membershipStateArea, Key: []byte(deferredWelcomeIndexKey), ExpectedRevision: indexRev, Value: next}}
	if rev != 0 {
		changes = append(changes, native.StateChange{Area: membershipStateArea, Key: deferredWelcomeKey(group), ExpectedRevision: rev})
	}
	op := r.stateOperation("deferred-welcome.drop", group[:], revBytes(rev), revBytes(indexRev))
	_, err = r.native.StateTransaction(ctx, op, changes)
	return err
}

// staleWelcome reports whether ApplyWelcomes refused a kept Welcome for good (its key package was spent or expired,
// the group moved on): the Bot cannot join from it.
func staleWelcome(err error) bool {
	var status *helper.StatusError
	if !errors.As(err, &status) {
		return false
	}
	switch status.Status {
	case 1, 2, 3, 4, 7:
		return true
	}
	return false
}

// reconcileBotChannels joins marked Bot channels from their kept Welcomes, drops the kept Welcomes of channels that went
// away or that the Bot already holds, and reports the left groups and the Bot channels that need the Bot re-added (fix
// round 1, I-1 and M-5). It answers how many groups it joined.
func (r *MembershipReconciler) reconcileBotChannels(ctx context.Context, session membershipSession, own client.Community, groups []native.GroupInfo,
	applied map[id.ID]bool, textGroup map[id.ID]client.Channel, channelByID map[id.ID]client.Channel, participates func(client.Channel) bool, log *slog.Logger,
) (uint64, error) {
	held := map[id.ID]bool{}
	left := map[id.ID]bool{}
	for _, g := range groups {
		if g.State != 2 {
			continue
		}
		held[g.ID] = true
		if g.Kind != 0 || g.Community == nil || *g.Community != r.binding.Community {
			continue
		}
		if ch, ok := channelByID[g.Target]; !ok || !participates(ch) {
			left[g.ID] = true
		}
	}
	for g := range applied {
		held[g] = true
	}
	index, indexRev, err := r.loadDeferredIndex(ctx)
	if err != nil {
		return 0, err
	}
	needsReAdd := map[id.ID]bool{}
	var joined uint64
	for _, group := range index {
		if err = client.CooperativeCheckpoint(ctx); err != nil {
			return joined, err
		}
		ch, ok := textGroup[group]
		if ok && !participates(ch) && !held[group] {
			continue // still not a Bot channel: kept
		}
		if ok && !held[group] {
			w, keptFor, _, present, loadErr := r.loadDeferredWelcome(ctx, group)
			if loadErr != nil {
				return joined, loadErr
			}
			if present && keptFor == ch.ID {
				outcomes, applyErr := r.native.ApplyWelcomes(ctx,
					[]native.GroupWelcome{{ID: w.ID, Group: group, Epoch: w.Epoch, CommitSequence: w.CommitSequence, Welcome: w.Blob, RatchetTree: w.Tree, TreeHash: bytes32(w.TreeHash)}},
					[]native.GroupExpected{{Group: group, Community: r.binding.Community, Channel: ch.ID, Kind: 0, Policy: own.PolicyVersion}})
				switch {
				case applyErr != nil && !staleWelcome(applyErr):
					return joined, membershipPhaseError(MembershipPhaseWelcomeApply, applyErr)
				case applyErr == nil && len(outcomes) == 1 && outcomes[0].Group == group && (outcomes[0].Outcome == 0 || outcomes[0].Outcome == 1):
					if err = r.revalidateChannelAccess(ctx, session, group, ch.ID, 0); err != nil {
						return joined, membershipPhaseError(MembershipPhaseWelcomeRevalidation, err)
					}
					held[group] = true
					joined++
					log.Info("bot joined Bot channel group from its kept Welcome", "channel_id", ch.ID.String())
				default:
					log.Warn("bot kept Welcome no longer valid; the Bot channel needs the Bot re-added", "channel_id", ch.ID.String())
				}
			}
		}
		// Joined, already held, stale, or its channel went away: the kept Welcome is done.
		if err = r.dropDeferredWelcome(ctx, group, index, indexRev); err != nil {
			return joined, err
		}
		index, indexRev, err = r.loadDeferredIndex(ctx)
		if err != nil {
			return joined, err
		}
	}
	for _, ch := range channelByID {
		if ch.Kind == 0 && participates(ch) && ch.TextGroupID != nil && !ch.TextGroupID.IsZero() && !held[*ch.TextGroupID] {
			needsReAdd[ch.ID] = true
		}
	}
	r.mu.Lock()
	for ch := range needsReAdd {
		if !r.reAdd[ch] {
			log.Warn("bot channel group needs the Bot re-added: no Welcome to join it from", "channel_id", ch.String())
		}
	}
	r.reAdd = needsReAdd
	channels := r.botChannels
	r.mu.Unlock()
	if channels != nil {
		channels.Membership(left, needsReAdd)
	}
	return joined, nil
}

// leaveNonBotChannelsOnce is the Bot's first start under v2 (Bot v2 Task 16): every channel group it holds that is not
// a Bot channel is left, each logged once with its channel id, and botChannelMigrationKey is persisted so it is not
// repeated. Leaving is not syncing the group (reconcileOnce skips it from here on); it is idempotent, so a failure to
// persist the marker only logs the same groups again on the next pass.
func (r *MembershipReconciler) leaveNonBotChannelsOnce(ctx context.Context, groups []native.GroupInfo, channelByID map[id.ID]client.Channel, participates func(client.Channel) bool, log *slog.Logger) error {
	done, err := r.native.StateGet(ctx, membershipStateArea, []byte(botChannelMigrationKey))
	if err != nil {
		return err
	}
	if done.Revision != 0 && !done.Tombstone {
		return nil
	}
	for _, g := range groups {
		if g.State != 2 || g.Kind != 0 || g.Community == nil || *g.Community != r.binding.Community {
			continue
		}
		if ch, ok := channelByID[g.Target]; ok && participates(ch) {
			continue
		}
		log.Info("bot left non-Bot channel group", "channel_id", g.Target.String(), "reason", "first_v2_start")
	}
	value, err := cborx.Marshal([]any{uint64(1)})
	if err != nil {
		return err
	}
	sum := sha256.Sum256(append([]byte("dilla.bot.membership.v2-migration\x00"), r.binding.Installation[:]...))
	revs, err := r.native.StateTransaction(ctx, id.ID(sum[:16]), []native.StateChange{{Area: membershipStateArea, Key: []byte(botChannelMigrationKey), ExpectedRevision: done.Revision, Value: value}})
	if err != nil {
		return fmt.Errorf("persist Bot channel migration: %w", err)
	}
	if len(revs) != 1 {
		return errors.New("invalid Bot channel migration revision")
	}
	return nil
}

func (r *MembershipReconciler) loadMembershipCursor(ctx context.Context) (uint64, uint64, error) {
	v, err := r.native.StateGet(ctx, membershipStateArea, []byte(membershipStateKey))
	if err != nil {
		return 0, 0, err
	}
	if v.Tombstone {
		return 0, 0, errors.New("membership cursor tombstoned")
	}
	if v.Revision == 0 {
		return 0, 0, nil
	}
	var a []any
	if cborx.Unmarshal(v.Value, &a) != nil || len(a) != 2 || a[0] != uint64(1) {
		return 0, 0, errors.New("invalid membership cursor")
	}
	n, ok := a[1].(uint64)
	if !ok || n > math.MaxInt64 {
		return 0, 0, errors.New("invalid membership cursor")
	}
	return n, v.Revision, nil
}
func (r *MembershipReconciler) saveMembershipCursor(ctx context.Context, revision, sequence uint64) (uint64, error) {
	v, err := cborx.Marshal([]any{uint64(1), sequence})
	if err != nil {
		return revision, err
	}
	var rev [8]byte
	binary.BigEndian.PutUint64(rev[:], revision)
	seed := append([]byte("dilla.bot.membership.cursor.v1\x00"), r.binding.Installation[:]...)
	seed = append(seed, rev[:]...)
	seed = append(seed, v...)
	sum := sha256.Sum256(seed)
	op := id.ID(sum[:16])
	if op.IsZero() {
		return revision, errors.New("invalid membership cursor transaction id")
	}
	revs, err := r.native.StateTransaction(ctx, op, []native.StateChange{{Area: membershipStateArea, Key: []byte(membershipStateKey), ExpectedRevision: revision, Value: v}})
	if err != nil {
		return revision, err
	}
	if len(revs) != 1 || revs[0] != revision+1 {
		return revision, errors.New("invalid membership cursor revision")
	}
	return revs[0], nil
}
