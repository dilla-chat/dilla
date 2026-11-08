//go:build linux

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/dilla-chat/dilla/internal/bot/client"
	"github.com/dilla-chat/dilla/internal/bot/helper"
	"github.com/dilla-chat/dilla/internal/bot/music"
	"github.com/dilla-chat/dilla/internal/bot/native"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
)

const (
	maxMusicCallJoinLeaves = 1024
	maxMusicCallJoinBytes  = 4 << 20
)

type musicCallMembershipReader interface {
	GroupPage(context.Context, id.ID, string, uint64, uint64) (client.Page, error)
}

type musicCallMembershipForgetter interface {
	CallGroupForget(context.Context, id.ID) error
}

// The call join reads no device list (Bot v2 Task 19): its leaves are judged against the pushed device-list cache.
type musicCallJoinReader interface {
	musicCallMembershipReader
	ChannelCall(context.Context, id.ID) (client.ChannelCallState, error)
	GroupInfo(context.Context, id.ID) (client.GroupInfo, error)
	GroupTree(context.Context, id.ID) (client.GroupTree, error)
	Resync(context.Context, id.ID, []byte, []byte) (client.SequenceResult, error)
}
type musicCallJoinCore interface {
	musicCallMembershipForgetter
	Groups(context.Context) ([]native.GroupInfo, error)
	GroupIntentResult(context.Context, id.ID) (*native.GroupIntentResult, error)
	ExternalJoin(context.Context, id.ID, id.ID, id.ID, uint64, uint64, []byte, []byte) (native.GroupExternalJoinResult, error)
	DiscardGroup(context.Context, id.ID, id.ID) error
	PendingLeaves(context.Context) ([]native.GroupPendingLeaf, error)
	CallJoinCheck(context.Context, id.ID) error
	GroupJoined(context.Context, id.ID, id.ID, uint64) error
	CallMediaEpoch(context.Context, id.ID) (native.CallMediaEpoch, error)
}

// musicJoinTrust is the installation's membership trust (trust.Trust, Task 16) as the call join uses it: Held refuses a
// user whose pushed list has not applied (fail closed), Changed re-judges one user's leaves whose list or pin changed
// since they were judged. Neither reads a device list.
type musicJoinTrust interface {
	Held(id.ID) bool
	Changed(context.Context, id.ID) error
}

// This carries no core free text or MLS bytes. A later Resume always gets a fresh retry budget.
type errMusicCallJoinConflict struct {
	group id.ID
	state int64
	epoch uint64
}

func (e *errMusicCallJoinConflict) Error() string {
	return "music call-group external_join: retryable conflict"
}
func (e *errMusicCallJoinConflict) Retryable() bool { return true }

// An unusable pending upload outside JOINING cannot be replayed or safely forgotten.
type errMusicCallJoinUnusable struct{}

func (*errMusicCallJoinUnusable) Error() string {
	return "music call-group unusable_pending: pending join intent cannot be resumed"
}
func (*errMusicCallJoinUnusable) Retryable() bool { return false }

var errMusicCallJoinDiscarded = errors.New("music call-group non-resumable join discarded")

// musicJoinTrust is installation's membership trust as its music call join sees it: the trust of the installation's
// feed (installationFeed.trust), read at each call so a feed created after the starter is used. Without a feed nothing
// has been applied, so every user is held and nothing can be re-judged (fail closed).
func (r *botGatewayRuntime) musicJoinTrust(installation id.ID) musicJoinTrust {
	return feedJoinTrust{r: r, installation: installation}
}

type feedJoinTrust struct {
	r            *botGatewayRuntime
	installation id.ID
}

var errMusicCallJoinNoTrust = errors.New("music call-group trust: the installation has no feed yet")

func (t feedJoinTrust) Held(user id.ID) bool {
	f := t.r.feedHandler(t.installation)
	return f == nil || f.trust.Held(user)
}

func (t feedJoinTrust) Changed(ctx context.Context, user id.ID) error {
	f := t.r.feedHandler(t.installation)
	if f == nil {
		return errMusicCallJoinNoTrust
	}
	return f.trust.Changed(ctx, user)
}

// musicPolicyFresh is how long a community's policy version read for a call join is used again (Bot v2 Task 19: the
// read is one of the rate-limited requests on a cold /play's path).
const musicPolicyFresh = 30 * time.Second

// musicPolicyCache keeps a community's policy version for the music call joins of one pass.
type musicPolicyCache struct {
	mu      sync.Mutex
	version uint64
	at      time.Time
}

func (c *musicPolicyCache) get(ctx context.Context, read func(context.Context) (uint64, error)) (uint64, error) {
	c.mu.Lock()
	if c.version != 0 && time.Since(c.at) < musicPolicyFresh {
		version := c.version
		c.mu.Unlock()
		return version, nil
	}
	c.mu.Unlock()
	version, err := read(ctx)
	if err != nil {
		return 0, err
	}
	c.mu.Lock()
	c.version, c.at = version, time.Now()
	c.mu.Unlock()
	return version, nil
}

// forget makes the next join read the policy version again (after a join that failed, which a newer policy can cause).
func (c *musicPolicyCache) forget() {
	c.mu.Lock()
	c.version = 0
	c.mu.Unlock()
}

// errMusicCallJoinHeld refuses a join while a user in the call is held by membership trust (their newest pushed list
// has not applied). It carries no user id. A held user is usually released by the push applying within moments, so the
// join retries on the conflict schedule.
type errMusicCallJoinHeld struct{}

func (*errMusicCallJoinHeld) Error() string {
	return "music call-group trust: a user in the call is held"
}
func (*errMusicCallJoinHeld) Retryable() bool { return true }

// errMusicCallJoinUnjudged refuses a join whose staged group still has a leaf the trust cache could not judge.
var errMusicCallJoinUnjudged = errors.New("music call-group trust: a leaf is still unjudged")

func waitMusicCallJoin(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func retryMusicCallGroupJoin(ctx context.Context, calls musicCallJoinReader, core musicCallJoinCore, trust musicJoinTrust, installation, community, channel id.ID, policy uint64, wait func(context.Context, time.Duration) error) error {
	delays := [...]time.Duration{250 * time.Millisecond, time.Second, 2 * time.Second}
	conflicts, discarded := 0, false
	for {
		err := musicCallGroupJoinAttempt(ctx, calls, core, trust, installation, community, channel, policy)
		if errors.Is(err, errMusicCallJoinDiscarded) && !discarded {
			discarded = true
			continue
		}
		var conflict *errMusicCallJoinConflict
		var held *errMusicCallJoinHeld
		if (!errors.As(err, &conflict) && !errors.As(err, &held)) || conflicts == len(delays) {
			return err
		}
		if err = wait(ctx, delays[conflicts]); err != nil {
			return err
		}
		conflicts++
	}
}

// ensureMusicCallGroupJoined performs the same security sequence as the browser call join:
// stage an external join, judge the staged group's leaves against the pushed device-list cache (Bot v2 Task 19: no
// device-list reads), gate the staged call epoch, post the exact commit, and resolve the helper intent from the
// accepted resync sequence.
func ensureMusicCallGroupJoined(ctx context.Context, calls *client.Session, core *native.Client, trust musicJoinTrust, installation, community, channel id.ID, policy uint64) error {
	if ctx == nil || calls == nil || core == nil || trust == nil || installation.IsZero() || community.IsZero() || channel.IsZero() || policy == 0 {
		return errors.New("music call-group join dependencies unavailable")
	}
	return retryMusicCallGroupJoin(ctx, calls, core, trust, installation, community, channel, policy, waitMusicCallJoin)
}

func musicCallGroupJoinAttempt(ctx context.Context, calls musicCallJoinReader, core musicCallJoinCore, trust musicJoinTrust, installation, community, channel id.ID, policy uint64) error {
	branch := "external_join"
	timing := &musicJoinTimings{}
	defer func() {
		music.RecordCallJoinBranch(ctx, branch)
		for _, entry := range timing.entries {
			music.PlaybackLogger(ctx).DebugContext(ctx, "music call-group join sub-call", "branch", branch, "stage", entry.stage, "elapsed_ms", entry.ms)
		}
	}()
	call, err := timedMusicJoinCall(timing, "ChannelCall", func() (client.ChannelCallState, error) { return calls.ChannelCall(ctx, channel) })
	if err != nil {
		return musicCallJoinStageError("channel_call", err)
	}
	if call.CallID == nil || call.GroupID == nil || *call.CallID != channel || call.GroupID.IsZero() {
		return errors.New("music channel has no active call group")
	}
	group := *call.GroupID
	groups, err := timedMusicJoinCall(timing, "Groups", func() ([]native.GroupInfo, error) { return core.Groups(ctx) })
	if err != nil {
		return musicCallJoinStageError("native_groups", err)
	}
	if err := timing.run("forgetSupersededMusicCallGroups", func() error { return forgetSupersededMusicCallGroups(ctx, core, groups, community, channel, group) }); err != nil {
		return musicCallJoinStageError("forget_superseded_group", err)
	}
	var localJoinEpoch, localEpoch uint64
	var localState int64 = -1
	for _, existing := range groups {
		if existing.ID != group {
			continue
		}
		if existing.State > 4 {
			return errors.New("music call-group state is not joinable")
		}
		localState, localEpoch = int64(existing.State), existing.Epoch
		if existing.State == 4 {
			branch = "forgot_stale_leaf"
			if err = timing.run("CallGroupForget", func() error { return core.CallGroupForget(ctx, group) }); err != nil {
				return musicCallJoinStageError("forget_gone_group", err)
			}
			continue
		}
		if existing.State == 2 {
			current, membershipErr := timedMusicJoinCall(timing, "prepareMusicCallGroupMembership", func() (bool, error) { return prepareMusicCallGroupMembership(ctx, calls, core, group) })
			if membershipErr != nil {
				return musicCallJoinStageError("active_membership_check", membershipErr)
			}
			if current {
				branch = "already_member"
				_, err = timedMusicJoinCall(timing, "CallMediaEpoch", func() (native.CallMediaEpoch, error) { return core.CallMediaEpoch(ctx, group) })
				return musicCallJoinStageError("active_media_gate", err)
			}
			branch = "forgot_stale_leaf"
			break
		}
		if existing.State != 1 {
			return errors.New("music call-group state is not joinable")
		}
		localJoinEpoch = existing.Epoch
	}
	info, err := timedMusicJoinCall(timing, "GroupInfo", func() (client.GroupInfo, error) { return calls.GroupInfo(ctx, group) })
	if err != nil {
		return musicCallJoinStageError("group_info", err)
	}
	baseEpoch := info.Epoch
	if localJoinEpoch > 0 {
		baseEpoch = localJoinEpoch - 1
	}
	operation := musicCallJoinOperation(installation, group, baseEpoch)

	var joined native.GroupExternalJoinResult
	var intent *native.GroupIntentResult
	var nonResumable id.ID
	for generation := 0; ; generation++ {
		if generation >= 64 {
			return errors.New("music call-group join intent retry limit exceeded")
		}
		intent, err = timedMusicJoinCall(timing, "GroupIntentResult", func() (*native.GroupIntentResult, error) { return core.GroupIntentResult(ctx, operation) })
		if err != nil {
			return musicCallJoinStageError("intent_read", err)
		}
		if intent == nil {
			break
		}
		if intent.Kind != 5 || intent.Group != group {
			return errors.New("music call-group join intent binding mismatch")
		}
		if intent.State != 0 || len(intent.Result) == 0 {
			nonResumable = operation
			if intent.State == 0 {
				break
			}
			// Follow a deterministic chain past terminal operations. A later Resume
			// follows the same chain and recovers the exact staged upload after a lost reply.
			digest := sha256.Sum256(append([]byte("dilla.bot.music-call-retry.v1\x00"), operation[:]...))
			copy(operation[:], digest[:len(operation)])
			continue
		}
		break
	}
	if intent != nil && intent.State == 0 && len(intent.Result) == 0 && localState != 1 {
		// The interaction handler logs this terminal failure once with operation context.
		return &errMusicCallJoinUnusable{}
	}
	// session.QueueStarter is owned by one music.Supervisor per channel; the
	// supervisor serializes starts. No competing join can stage a new upload
	// between this snapshot, discard and forget.
	if (intent == nil || len(intent.Result) == 0) && !nonResumable.IsZero() && localState == 1 {
		if err = timing.run("DiscardGroup", func() error { return core.DiscardGroup(ctx, nonResumable, group) }); err != nil {
			return musicCallJoinStageError("discard_non_resumable", err)
		}
		// An already resolved discard may leave a historical JOINING row behind.
		if err = timing.run("CallGroupForget", func() error { return core.CallGroupForget(ctx, group) }); err != nil {
			return musicCallJoinStageError("forget_non_resumable", err)
		}
		slog.WarnContext(ctx, "music call-group join retry", "stage", "discard_non_resumable", "group_id", group.String(), "local_state", localState, "epoch", localEpoch)
		return errMusicCallJoinDiscarded
	}
	if intent != nil {
		branch = "resumed_intent"
		joined, err = decodeMusicExternalJoinResult(intent.Result)
		if err != nil {
			return musicCallJoinStageError("intent_decode", err)
		}
	} else {
		tree, treeErr := timedMusicJoinCall(timing, "GroupTree", func() (client.GroupTree, error) { return calls.GroupTree(ctx, group) })
		if treeErr != nil {
			return musicCallJoinStageError("group_tree", treeErr)
		}
		if info.Epoch != tree.Epoch || !bytes.Equal(info.TreeHash, tree.TreeHash) {
			return errors.New("music call-group snapshot changed during join")
		}
		infoBody, encodeErr := cborx.Marshal([]any{info.Epoch, info.Info, info.TreeHash, info.NextSequence})
		if encodeErr != nil {
			return client.ErrProtocol
		}
		treeBody, encodeErr := cborx.Marshal([]any{tree.Epoch, tree.Tree, tree.TreeHash})
		if encodeErr != nil {
			return client.ErrProtocol
		}
		joined, err = timedMusicJoinCall(timing, "ExternalJoin", func() (native.GroupExternalJoinResult, error) {
			return core.ExternalJoin(ctx, operation, group, channel, 1, policy, infoBody, treeBody)
		})
		if err != nil {
			var status *helper.StatusError
			if errors.As(err, &status) && status.Status == 4 {
				slog.WarnContext(ctx, "music call-group join retry", "stage", "external_join", "group_id", group.String(), "local_state", localState, "epoch", localEpoch)
				return &errMusicCallJoinConflict{group: group, state: localState, epoch: localEpoch}
			}
			return musicCallJoinStageError("external_join", err)
		}
	}
	if err = timing.run("verifyMusicCallJoinLeaves", func() error { return verifyMusicCallJoinLeaves(ctx, core, trust, group, call.Participants) }); err != nil {
		return musicCallJoinStageError("trust_refresh", err)
	}
	if err = timing.run("CallJoinCheck", func() error { return core.CallJoinCheck(ctx, group) }); err != nil {
		return musicCallJoinStageError("staged_media_gate", err)
	}
	accepted, err := timedMusicJoinCall(timing, "Resync", func() (client.SequenceResult, error) {
		return calls.Resync(ctx, group, joined.Commit, joined.GroupInfo)
	})
	if err != nil {
		var apiErr *client.APIError
		if errors.As(err, &apiErr) && apiErr.Outcome == client.OutcomeDefinitive {
			if discardErr := timing.run("DiscardGroup", func() error { return core.DiscardGroup(ctx, operation, group) }); discardErr != nil {
				return errors.Join(musicCallJoinStageError("resync", err), musicCallJoinStageError("discard", discardErr))
			}
			return musicCallJoinStageError("resync", err)
		}
		// A lost response is resolved only when the exact staged GroupInfo is now current.
		current, readErr := timedMusicJoinCall(timing, "GroupInfo", func() (client.GroupInfo, error) { return calls.GroupInfo(ctx, group) })
		if readErr != nil || current.NextSequence == 0 || !bytes.Equal(current.Info, joined.GroupInfo) {
			return musicCallJoinStageError("resync", err)
		}
		accepted.Sequence = current.NextSequence - 1
	}
	if accepted.Sequence == 0 || accepted.Sequence > math.MaxInt64 {
		return errors.New("music call-group resync returned an invalid sequence")
	}
	if err = timing.run("GroupJoined", func() error { return core.GroupJoined(ctx, operation, group, accepted.Sequence) }); err != nil {
		return musicCallJoinStageError("resolve_join_intent", err)
	}
	if _, err = timedMusicJoinCall(timing, "CallMediaEpoch", func() (native.CallMediaEpoch, error) { return core.CallMediaEpoch(ctx, group) }); err != nil {
		return musicCallJoinStageError("joined_media_gate", err)
	}
	return nil
}

func musicCallGroupMembershipCurrent(ctx context.Context, reader musicCallMembershipReader, group id.ID) (bool, error) {
	_, err := reader.GroupPage(ctx, group, "handshakes", 1, 1)
	if err == nil {
		return true, nil
	}
	var apiErr *client.APIError
	if errors.As(err, &apiErr) {
		if apiErr.Status == 404 && apiErr.Code == "E_NOT_FOUND" {
			return false, nil
		}
		// E_PRUNED is returned only after the member check succeeds.
		if apiErr.Code == "E_PRUNED" {
			return true, nil
		}
	}
	return false, err
}

func prepareMusicCallGroupMembership(ctx context.Context, reader musicCallMembershipReader, forgetter musicCallMembershipForgetter, group id.ID) (bool, error) {
	current, err := musicCallGroupMembershipCurrent(ctx, reader, group)
	if err != nil || current {
		return current, err
	}
	// A reader can still fetch GroupInfo after its leaf was removed, but the
	// member-only handshake route returns E_NOT_FOUND. Drop the stale local key
	// state before staging a fresh external join at the current server epoch.
	if forgetter == nil {
		return false, errors.New("call-group local state cannot be forgotten")
	}
	if err := forgetter.CallGroupForget(ctx, group); err != nil {
		return false, err
	}
	return false, nil
}

// ChannelCall has identified the server's current group for this channel. A
// different local call group for the same binding cannot be used by that call;
// retire it before the core's one-active-group-per-channel join gate is entered.
func forgetSupersededMusicCallGroups(ctx context.Context, core musicCallMembershipForgetter, groups []native.GroupInfo, community, channel, current id.ID) error {
	for _, previous := range groups {
		if previous.ID == current || previous.Kind != 1 || previous.State == 4 ||
			previous.Community == nil || *previous.Community != community || previous.Target != channel {
			continue
		}
		if err := core.CallGroupForget(ctx, previous.ID); err != nil {
			return err
		}
	}
	return nil
}

func musicCallJoinStageError(stage string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("music call-group %s: %w", stage, err)
}

func musicCallJoinOperation(installation, group id.ID, epoch uint64) id.ID {
	input := append([]byte("dilla.bot.music-call-join.v1\x00"), installation[:]...)
	input = append(input, group[:]...)
	var encodedEpoch [8]byte
	for i := range encodedEpoch {
		encodedEpoch[len(encodedEpoch)-1-i] = byte((epoch >> (8 * i)) & 0xff)
	}
	input = append(input, encodedEpoch[:]...)
	digest := sha256.Sum256(input)
	var operation id.ID
	copy(operation[:], digest[:len(operation)])
	return operation
}

func decodeMusicExternalJoinResult(body []byte) (native.GroupExternalJoinResult, error) {
	var row []any
	if len(body) == 0 || cborx.Unmarshal(body, &row) != nil || len(row) != 2 {
		return native.GroupExternalJoinResult{}, client.ErrProtocol
	}
	commit, ok := row[0].([]byte)
	if !ok || len(commit) == 0 {
		return native.GroupExternalJoinResult{}, client.ErrProtocol
	}
	info, ok := row[1].([]byte)
	if !ok || len(info) == 0 {
		return native.GroupExternalJoinResult{}, client.ErrProtocol
	}
	return native.GroupExternalJoinResult{Commit: append([]byte(nil), commit...), GroupInfo: append([]byte(nil), info...)}, nil
}

// verifyMusicCallJoinLeaves judges the staged call group's leaves through membership trust (Task 16), with no
// device-list read:
//   - every user in the call (the call's participants and the users of the group's pending leaves) must not be Held:
//     a user whose newest pushed list has not applied is refused, fail closed;
//   - only the users with a pending leaf in this group (for a Bot core: no cached list, or a list or pin that changed
//     since the leaf was judged) are re-judged, once each, through trust.Changed;
//   - a leaf of the group still pending afterwards refuses the join.
//
// The cost is one pending-leaves read, one Changed per changed user and, when any was re-judged, one more read: O(the
// call), never O(the community), and no device-list read.
func verifyMusicCallJoinLeaves(ctx context.Context, core musicCallJoinCore, trust musicJoinTrust, group id.ID, participants []client.CallParticipant) error {
	if trust == nil {
		return errors.New("music call-group trust unavailable")
	}
	if len(participants) > maxMusicCallJoinLeaves {
		return errors.New("music call-group trust refresh exceeds limit")
	}
	leaves, err := core.PendingLeaves(ctx)
	if err != nil {
		return err
	}
	var changed []id.ID
	seen := make(map[id.ID]bool)
	pending := 0
	for _, leaf := range leaves {
		if leaf.Group != group {
			continue
		}
		if pending++; pending > maxMusicCallJoinLeaves {
			return errors.New("music call-group trust refresh exceeds limit")
		}
		if !seen[leaf.User] {
			seen[leaf.User] = true
			changed = append(changed, leaf.User)
		}
	}
	inCall := append([]id.ID(nil), changed...)
	for _, p := range participants {
		if !p.UserID.IsZero() && !seen[p.UserID] {
			seen[p.UserID] = true
			inCall = append(inCall, p.UserID)
		}
	}
	for _, user := range inCall {
		if trust.Held(user) {
			return &errMusicCallJoinHeld{}
		}
	}
	for _, user := range changed {
		if err := trust.Changed(ctx, user); err != nil {
			return err
		}
		// A push that arrived while the user was re-judged may hold them now.
		if trust.Held(user) {
			return &errMusicCallJoinHeld{}
		}
	}
	if len(changed) == 0 {
		return nil
	}
	after, err := core.PendingLeaves(ctx)
	if err != nil {
		return err
	}
	for _, leaf := range after {
		if leaf.Group == group {
			return errMusicCallJoinUnjudged
		}
	}
	return nil
}

type musicJoinTiming struct {
	stage string
	ms    int64
}
type musicJoinTimings struct{ entries []musicJoinTiming }

func timedMusicJoinCall[T any](timing *musicJoinTimings, stage string, call func() (T, error)) (T, error) {
	start := time.Now()
	defer func() {
		timing.entries = append(timing.entries, musicJoinTiming{stage, time.Since(start).Milliseconds()})
	}()
	return call()
}
func (t *musicJoinTimings) run(stage string, call func() error) error {
	_, err := timedMusicJoinCall(t, stage, func() (struct{}, error) { return struct{}{}, call() })
	return err
}
