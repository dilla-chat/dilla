package service

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/jonasthim/dilla/internal/bot/client"
	"github.com/jonasthim/dilla/internal/bot/native"
	"github.com/jonasthim/dilla/internal/cborx"
	"github.com/jonasthim/dilla/internal/id"
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
		}
		last := welcomes[len(welcomes)-1].ID
		_, err = r.saveMembershipCursor(ctx, revision, last)
		if err != nil {
			return out, fmt.Errorf("persist welcome cursor: %w", err)
		}
		out.More = uint64(len(welcomes)) == r.pageLimit
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
