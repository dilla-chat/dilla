//go:build linux

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/bot/catalog"
	"github.com/dilla-chat/dilla/internal/bot/client"
	"github.com/dilla-chat/dilla/internal/bot/helper"
	"github.com/dilla-chat/dilla/internal/bot/native"
	"github.com/dilla-chat/dilla/internal/bot/polls"
	"github.com/dilla-chat/dilla/internal/bot/protocol"
	"github.com/dilla-chat/dilla/internal/bot/reminders"
	"github.com/dilla-chat/dilla/internal/bot/service"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
)

const utilityPollSendPermission = uint64(api.PermViewChannel | api.PermSendMessages | api.PermReadHistory)
const pollPublicationPrefix = "dilla.bot.poll-publication.v1/"

type utilityActorAuthority struct {
	session   utilityAuthoritySession
	community id.ID
}

type utilityAuthoritySession interface {
	BotActorAuthority(context.Context, id.ID, id.ID) (client.ActorAuthority, bool, error)
	BotActorChannelAuthority(context.Context, id.ID, id.ID, id.ID) (uint64, bool, error)
}

func (a utilityActorAuthority) pollMember(ctx context.Context, actor id.ID) (bool, error) {
	_, present, err := a.session.BotActorAuthority(ctx, a.community, actor)
	return present, err
}
func (a utilityActorAuthority) PollMember(ctx context.Context, actor id.ID) (bool, error) {
	return a.pollMember(ctx, actor)
}
func (a utilityActorAuthority) PollPermissions(ctx context.Context, actor, channel id.ID) (uint64, error) {
	bits, present, err := a.session.BotActorChannelAuthority(ctx, a.community, actor, channel)
	if err != nil {
		return 0, err
	}
	if !present {
		return 0, nil
	}
	return bits, nil
}
func (a utilityActorAuthority) ReminderMember(ctx context.Context, actor id.ID) (bool, error) {
	return a.pollMember(ctx, actor)
}
func (a utilityActorAuthority) ReminderSource(ctx context.Context, actor, channel id.ID) (bool, error) {
	bits, present, err := a.session.BotActorChannelAuthority(ctx, a.community, actor, channel)
	return present && api.Bits(bits).Has(api.PermViewChannel|api.PermReadHistory), err
}

type utilityPublisherState struct {
	operation, attempt, group id.ID
	predecessor               *id.ID
	generation, originalNow   uint64
	phase                     uint64 // 0 prepared, 1 accepted, 2 unknown terminal
	receipt                   []byte
}

type pollPublicationState interface {
	Binding() native.Binding
	StateGet(context.Context, uint64, []byte) (native.StateValue, error)
	StateTransaction(context.Context, id.ID, []native.StateChange) ([]uint64, error)
	ConfirmSendReceipt(context.Context, id.ID, []byte) error
}
type pollPublicationCoordinator interface {
	SendAuthorized(context.Context, service.SendSession, service.SendInput, func(context.Context) error) (protocol.SendReceipt, error)
}
type pollPublicationDestination interface {
	ResolveTextGroup(context.Context, id.ID, id.ID) (id.ID, error)
}
type pollPublicationAuthority interface {
	DiscoverObserved(context.Context) (client.Discovery, client.ResponseObservation, error)
}
type pollPublicationReceiptSession interface {
	BotMessageReceipt(context.Context, id.ID, id.ID, id.ID, id.ID) (protocol.SendReceipt, error)
}

// pollThreadSync pages a text group's newer handshakes and messages into the helper (service.SourceSync bound to the
// Bot's session): a thread poll whose root the helper does not hold yet waits on it (Task 21b fix round 2).
type pollThreadSync func(context.Context, native.GroupExpected) (service.SourceSyncResult, error)

// pollThreadRootWait bounds how long a thread poll waits for its root to reach the helper; pollThreadRootRetry is the
// pause between catch-up passes. Variables so tests can shorten them.
var (
	pollThreadRootWait  = 10 * time.Second
	pollThreadRootRetry = 200 * time.Millisecond
)

// pollThreadCatchUpPasses bounds one catch-up's sync passes (each is itself bounded to two paired page reads).
const pollThreadCatchUpPasses = 16

type pollChannelAuthority interface {
	BotActorChannelAuthority(context.Context, id.ID, id.ID, id.ID) (uint64, bool, error)
	BotActorAuthority(context.Context, id.ID, id.ID) (client.ActorAuthority, bool, error)
}

type durablePollPublisher struct {
	state                   pollPublicationState
	coordinator             pollPublicationCoordinator
	destination             pollPublicationDestination
	authority               pollPublicationAuthority
	session                 service.SendSession
	receipts                pollPublicationReceiptSession
	actorAuthority          pollChannelAuthority
	community, installation id.ID
	now                     func() time.Time
	// threadSync catches the poll's text group up when a thread poll's root is not held yet; nil sends once.
	threadSync pollThreadSync
	// threadWaiting is set while one thread poll of this installation waits for its root (Task 21b fix round 3, I2):
	// another one gets one catch-up and one send, never the wait, so thread ids cannot hold the command workers.
	threadWaiting atomic.Bool
}

func publicationKey(poll id.ID) []byte {
	return []byte(pollPublicationPrefix + fmt.Sprintf("%x", poll[:]))
}
func encodePublication(v utilityPublisherState) ([]byte, error) {
	return cborx.Marshal([]any{uint64(2), v.operation, v.attempt, v.predecessor, v.group, v.generation, v.originalNow, v.phase, v.receipt})
}
func decodePublication(raw []byte) (utilityPublisherState, error) {
	var a []any
	if err := cborx.Unmarshal(raw, &a); err != nil || (len(a) != 8 || a[0] != uint64(1)) && (len(a) != 9 || a[0] != uint64(2)) {
		return utilityPublisherState{}, errors.New("invalid poll publication state")
	}
	idAt := func(i int) (id.ID, bool) {
		switch x := a[i].(type) {
		case id.ID:
			return x, !x.IsZero()
		case []byte:
			if len(x) == 16 {
				var v id.ID
				copy(v[:], x)
				return v, !v.IsZero()
			}
		}
		return id.ID{}, false
	}
	v := utilityPublisherState{}
	var ok bool
	if v.operation, ok = idAt(1); !ok {
		return v, errors.New("invalid poll publication operation")
	}
	if v.attempt, ok = idAt(2); !ok {
		return v, errors.New("invalid poll publication attempt")
	}
	if a[3] != nil {
		x, b := idAt(3)
		if !b {
			return v, errors.New("invalid poll publication predecessor")
		}
		v.predecessor = &x
	}
	versionTwo := len(a) == 9
	generationAt, originalAt, phaseAt, receiptAt := 4, 5, 6, 7
	if versionTwo {
		if v.group, ok = idAt(4); !ok {
			return v, errors.New("invalid poll publication group")
		}
		generationAt, originalAt, phaseAt, receiptAt = 5, 6, 7, 8
	}
	if v.generation, ok = a[generationAt].(uint64); !ok || v.generation == 0 {
		return v, errors.New("invalid poll publication generation")
	}
	if v.originalNow, ok = a[originalAt].(uint64); !ok {
		return v, errors.New("invalid poll publication time")
	}
	if v.phase, ok = a[phaseAt].(uint64); !ok || v.phase > 2 {
		return v, errors.New("invalid poll publication phase")
	}
	if a[receiptAt] != nil {
		v.receipt, ok = a[receiptAt].([]byte)
		if !ok {
			return v, errors.New("invalid poll publication receipt")
		}
	}
	if v.phase == 0 && len(v.receipt) != 0 || v.phase != 0 && len(v.receipt) == 0 {
		return v, errors.New("poll publication receipt does not match its phase")
	}
	if v.group.IsZero() && len(v.receipt) > 0 {
		receipt, err := protocol.DecodeReceipt(v.receipt)
		if err != nil {
			return v, err
		}
		v.group = receipt.GroupID
	}
	return v, nil
}
func (p *durablePollPublisher) persist(ctx context.Context, poll id.ID, snapshot native.StateValue, value utilityPublisherState) (utilityPublisherState, native.StateValue, error) {
	body, err := encodePublication(value)
	if err != nil {
		return value, snapshot, err
	}
	_, err = p.state.StateTransaction(ctx, id.New(), []native.StateChange{{Area: 8, Key: publicationKey(poll), ExpectedRevision: snapshot.Revision, Value: body}})
	if err != nil {
		latest, e := p.state.StateGet(ctx, 8, publicationKey(poll))
		if e != nil {
			return value, snapshot, err
		}
		if latest.Revision == 0 || latest.Tombstone {
			return value, latest, err
		}
		current, e := decodePublication(latest.Value)
		return current, latest, e
	}
	snapshot.Revision++
	snapshot.Value = body
	return value, snapshot, nil
}
func (p *durablePollPublisher) authorize(ctx context.Context, poll polls.Poll) error {
	if p.actorAuthority == nil || p.session == nil {
		return errors.New("poll publication authority unavailable")
	}
	if _, present, err := p.actorAuthority.BotActorAuthority(ctx, p.community, poll.CreatorID); err != nil || !present {
		if err != nil {
			return err
		}
		return errors.New("poll creator no longer has community access")
	}
	bits, present, err := p.actorAuthority.BotActorChannelAuthority(ctx, p.community, poll.CreatorID, poll.ChannelID)
	if err != nil {
		return err
	}
	if !present || bits&utilityPollSendPermission != utilityPollSendPermission {
		return errors.New("poll creator no longer has channel send access")
	}
	return nil
}

func (p *durablePollPublisher) validateReceipt(poll polls.Poll, record utilityPublisherState) (protocol.SendReceipt, error) {
	receipt, err := protocol.DecodeReceipt(record.receipt)
	if err != nil {
		return protocol.SendReceipt{}, err
	}
	if receipt.OperationID != poll.ID || receipt.OperationID != record.operation || receipt.AttemptID != record.attempt ||
		receipt.CommunityID != p.community || receipt.GroupID != record.group || receipt.ExpectedGeneration != record.generation ||
		!sameOptionalID(receipt.PredecessorAttemptID, record.predecessor) || protocol.ValidateReceiptContext(receipt, p.community, record.group) != nil {
		return protocol.SendReceipt{}, errors.New("poll publication receipt binding changed")
	}
	if record.phase == 2 && receipt.Outcome != protocol.ReceiptUnknownAfterRestore ||
		record.phase == 1 && receipt.Outcome != protocol.ReceiptAccepted && receipt.Outcome != protocol.ReceiptClosedNotAccepted {
		return protocol.SendReceipt{}, errors.New("poll publication receipt phase changed")
	}
	return receipt, nil
}

func sameOptionalID(a, b *id.ID) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// recoverRecordedReceipt checks live installation/session state using the
// immutable original group. Historical receipts do not depend on the channel's
// current group or on the creator still having access.
func (p *durablePollPublisher) recoverRecordedReceipt(ctx context.Context, poll polls.Poll, record utilityPublisherState) (protocol.SendReceipt, error) {
	if record.phase == 0 {
		return protocol.SendReceipt{}, nil
	}
	stored, err := p.validateReceipt(poll, record)
	if err != nil {
		return protocol.SendReceipt{}, err
	}
	discovery, _, err := p.authority.DiscoverObserved(ctx)
	if err != nil {
		return protocol.SendReceipt{}, err
	}
	if discovery.InstanceID != p.state.Binding().Instance || discovery.Generation == 0 {
		return protocol.SendReceipt{}, client.ErrInstanceChanged
	}
	if p.receipts == nil {
		return protocol.SendReceipt{}, errors.New("poll receipt recovery session unavailable")
	}
	current, err := p.receipts.BotMessageReceipt(ctx, p.community, record.operation, record.attempt, record.group)
	if errors.Is(err, client.ErrBotSendReceiptNotFound) {
		return stored, nil
	}
	if err != nil {
		return protocol.SendReceipt{}, err
	}
	encoded, err := protocol.EncodeReceipt(current)
	if err != nil {
		return protocol.SendReceipt{}, err
	}
	if !bytes.Equal(encoded, record.receipt) {
		return protocol.SendReceipt{}, client.ErrProtocol
	}
	if err = p.state.ConfirmSendReceipt(ctx, record.operation, encoded); err != nil {
		return protocol.SendReceipt{}, err
	}
	return current, nil
}

func (p *durablePollPublisher) PublishPoll(ctx context.Context, poll polls.Poll) error {
	if p == nil || p.state == nil || p.coordinator == nil || p.authority == nil || p.session == nil || p.receipts == nil || p.community.IsZero() || p.installation.IsZero() || poll.ID.IsZero() || poll.CommunityID != p.community {
		return errors.New("missing poll publisher dependency")
	}
	key := publicationKey(poll.ID)
	snapshot, err := p.state.StateGet(ctx, 8, key)
	if err != nil {
		return err
	}
	var record utilityPublisherState
	if snapshot.Revision > 0 && !snapshot.Tombstone {
		record, err = decodePublication(snapshot.Value)
		if err != nil {
			return err
		}
	} else {
		if p.destination == nil {
			return errors.New("poll publication destination unavailable")
		}
		group, resolveErr := p.destination.ResolveTextGroup(ctx, p.community, poll.ChannelID)
		if resolveErr != nil {
			return resolveErr
		}
		discovery, _, e := p.authority.DiscoverObserved(ctx)
		if e != nil {
			return e
		}
		binding := p.state.Binding()
		if discovery.InstanceID != binding.Instance || discovery.Generation == 0 {
			return client.ErrInstanceChanged
		}
		record = utilityPublisherState{operation: poll.ID, attempt: id.New(), group: group, generation: discovery.Generation, originalNow: unixSecondsOrZero(p.nowOrTime())}
		record, snapshot, err = p.persist(ctx, poll.ID, snapshot, record)
		if err != nil {
			return err
		}
	}
	if record.operation != poll.ID {
		return errors.New("poll publication operation binding changed")
	}
	if record.group.IsZero() {
		// Upgrade an older in-progress record before using it. Its cached native
		// attempt remains authoritative and will reject a different group.
		if p.destination == nil {
			return errors.New("poll publication destination unavailable")
		}
		group, resolveErr := p.destination.ResolveTextGroup(ctx, p.community, poll.ChannelID)
		if resolveErr != nil {
			return resolveErr
		}
		record.group = group
		record, snapshot, err = p.persist(ctx, poll.ID, snapshot, record)
		if err != nil {
			return err
		}
	}
	if _, err = p.recoverRecordedReceipt(ctx, poll, record); err != nil {
		return err
	}
	if record.phase == 2 {
		return service.ErrSendTerminal
	}
	if record.phase == 1 {
		receipt, e := p.validateReceipt(poll, record)
		if e != nil {
			return e
		}
		if receipt.Outcome == protocol.ReceiptClosedNotAccepted {
			if p.destination == nil {
				return errors.New("poll publication destination unavailable")
			}
			group, resolveErr := p.destination.ResolveTextGroup(ctx, p.community, poll.ChannelID)
			if resolveErr != nil {
				return resolveErr
			}
			if group != record.group {
				return errors.New("poll publication group changed after attempt closure")
			}
			previousAttempt := record.attempt
			nextAttempt := id.New()
			discovery, _, e := p.authority.DiscoverObserved(ctx)
			if e != nil {
				return e
			}
			if discovery.Generation == 0 {
				return client.ErrInstanceChanged
			}
			record = utilityPublisherState{operation: poll.ID, attempt: nextAttempt, predecessor: &previousAttempt, group: record.group, generation: discovery.Generation, originalNow: record.originalNow}
			record, snapshot, err = p.persist(ctx, poll.ID, snapshot, record)
			if err != nil {
				return err
			}
		} else {
			return nil
		}
	}
	// The member's question and answers are defanged (fix round 2, M6); the marker line is the Bot's own.
	bodyText := fmt.Sprintf("%s\n\n%s\n\n%s\n\nCloses: <t:%d:R>\n[dilla-poll:v1:%x]", service.DefangMarkers(poll.Question), service.DefangMarkers(strings.Join(poll.Answers, "\n")), "Vote with /poll vote.", poll.DueAt, poll.ID[:])
	if len([]byte(bodyText)) > 4000 {
		return errors.New("poll publication body exceeds protocol limit")
	}
	body, err := cborx.Marshal([]any{uint64(0), nil, bodyText, []any{}, poll.ThreadID})
	if err != nil {
		return err
	}
	discovery, _, err := p.authority.DiscoverObserved(ctx)
	if err != nil {
		return err
	}
	if discovery.InstanceID != p.state.Binding().Instance || discovery.Generation == 0 {
		return client.ErrInstanceChanged
	}
	input := service.SendInput{OperationID: record.operation, AttemptID: record.attempt, PredecessorAttemptID: record.predecessor, GroupID: record.group, RequestBody: body, OriginalNow: record.originalNow, ExpectedGeneration: record.generation}
	receipt, sendErr := p.send(ctx, input, poll)
	if receipt.OperationID == (id.ID{}) && poll.ThreadID != nil && threadRootNotHeld(sendErr) {
		receipt, sendErr = p.sendAfterThreadCatchUp(ctx, input, poll, sendErr)
	}
	if receipt.OperationID != (id.ID{}) {
		encoded, e := protocol.EncodeReceipt(receipt)
		if e != nil {
			return e
		}
		record.receipt = encoded
		if receipt.Outcome == protocol.ReceiptUnknownAfterRestore {
			record.phase = 2
		} else {
			record.phase = 1
		}
		_, _, e = p.persist(ctx, poll.ID, snapshot, record)
		if e != nil {
			return e
		}
	}
	if sendErr != nil {
		return sendErr
	}
	if receipt.Outcome == protocol.ReceiptUnknownAfterRestore {
		return service.ErrSendTerminal
	}
	if receipt.Outcome == protocol.ReceiptClosedNotAccepted {
		return errors.New("poll publication attempt was closed without acceptance; retry will use a successor attempt")
	}
	return nil
}
func (p *durablePollPublisher) send(ctx context.Context, input service.SendInput, poll polls.Poll) (protocol.SendReceipt, error) {
	return p.coordinator.SendAuthorized(ctx, p.session, input, func(preflight context.Context) error { return p.authorize(preflight, poll) })
}

// threadRootNotHeld is the helper's not_found for a send: a thread poll's root the helper has not applied yet (a member
// starts a thread and runs /poll in it before the Bot's group sync reaches the root).
func threadRootNotHeld(err error) bool {
	var status *helper.StatusError
	return errors.As(err, &status) && status.Status == 7
}

// sendAfterThreadCatchUp catches the poll's text group up into the helper and sends again, until the root is held or
// pollThreadRootWait passes (Task 21b fix round 2). Each retry is the same native attempt: PrepareSend failed before
// it queued or encrypted anything, so the attempt resumes unchanged. Without a group sync it returns first.
//
// The thread id is the member's (Task 21b fix round 3, I2), so it may name no root at all. The publisher waits only
// while the group advances: a catch-up that applied nothing, followed by a send that still finds no root, answers
// the helper's not_found at once. And only one thread poll of the installation waits at a time; another gets its one
// catch-up and send. A thread id that names nothing therefore costs one catch-up, never a command worker for 10 s.
func (p *durablePollPublisher) sendAfterThreadCatchUp(ctx context.Context, input service.SendInput, poll polls.Poll, first error) (protocol.SendReceipt, error) {
	if p.threadSync == nil {
		return protocol.SendReceipt{}, first
	}
	waitCtx, cancel := context.WithTimeout(ctx, pollThreadRootWait)
	defer cancel()
	expected := native.GroupExpected{Group: input.GroupID, Community: p.community, Channel: poll.ChannelID, Kind: 0}
	last := first
	waiting := false
	defer func() {
		if waiting {
			p.threadWaiting.Store(false)
		}
	}()
	for {
		advanced, err := p.catchUpThreadGroup(waitCtx, expected)
		if err != nil {
			if ctx.Err() == nil && waitCtx.Err() != nil {
				return protocol.SendReceipt{}, fmt.Errorf("thread root not held after %s: %w", pollThreadRootWait, last)
			}
			return protocol.SendReceipt{}, fmt.Errorf("thread poll group catch-up: %w", err)
		}
		receipt, err := p.send(ctx, input, poll)
		if receipt.OperationID != (id.ID{}) || !threadRootNotHeld(err) {
			return receipt, err
		}
		last = err
		if !advanced {
			return protocol.SendReceipt{}, fmt.Errorf("thread root not held, the group is caught up: %w", last)
		}
		if !waiting {
			if !p.threadWaiting.CompareAndSwap(false, true) {
				return protocol.SendReceipt{}, fmt.Errorf("thread root not held, another thread poll is waiting: %w", last)
			}
			waiting = true
		}
		timer := time.NewTimer(pollThreadRootRetry)
		select {
		case <-waitCtx.Done():
			timer.Stop()
			if ctx.Err() != nil {
				return protocol.SendReceipt{}, ctx.Err()
			}
			return protocol.SendReceipt{}, fmt.Errorf("thread root not held after %s: %w", pollThreadRootWait, last)
		case <-timer.C:
		}
	}
}

// catchUpThreadGroup runs sync passes until the group has no more pages, and reports whether any pass applied a
// handshake or message (or left more to read). A pass of the same group already running (a hint and a tick at once)
// is waited for briefly, then looked at again.
func (p *durablePollPublisher) catchUpThreadGroup(ctx context.Context, expected native.GroupExpected) (bool, error) {
	advanced := false
	for pass := 0; pass < pollThreadCatchUpPasses; pass++ {
		result, err := p.threadSync(ctx, expected)
		switch {
		case errors.Is(err, service.ErrSourceSyncBusy):
			timer := time.NewTimer(20 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return advanced, ctx.Err()
			case <-timer.C:
			}
			continue
		case err != nil:
			return advanced, err
		}
		if result.Handshakes+result.Messages > 0 || result.More {
			advanced = true
		}
		if !result.More {
			return advanced, nil
		}
	}
	return advanced, nil
}

func (p *durablePollPublisher) nowOrTime() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now().UTC()
}

type utilityRuntime struct {
	polls     service.PollInteractions
	reminders service.ReminderInteractions
}

func newUtilityRuntime(n *native.Client, session *client.Session, authority *client.Client, community, installation id.ID, now func() time.Time) (*utilityRuntime, error) {
	if n == nil || session == nil || authority == nil || community.IsZero() || installation.IsZero() {
		return nil, errors.New("missing utility runtime dependency")
	}
	binding := n.Binding()
	if binding.Community != community || binding.Installation != installation {
		return nil, errors.New("utility runtime native binding does not match the live installation")
	}
	coordinator, err := service.NewSendCoordinator(n, authority)
	if err != nil {
		return nil, err
	}
	actor := utilityActorAuthority{session: session, community: community}
	threadSync, err := service.NewSourceSync(n, service.SourceSyncOptions{})
	if err != nil {
		return nil, err
	}
	publisher := &durablePollPublisher{state: n, coordinator: coordinator, destination: service.WelcomeDestinationResolver{Session: session, Native: n, Feature: "polls"}, authority: authority, session: session, receipts: session, actorAuthority: session, community: community, installation: installation, now: now,
		threadSync: func(ctx context.Context, expected native.GroupExpected) (service.SourceSyncResult, error) {
			return threadSync.Sync(ctx, session, expected)
		}}
	pollStore := polls.NewStore(n, community)
	reminderStore := reminders.NewStore(n, binding, now)
	// Bot v2 Task 17: a reminder that comes due tells its owner's clients (a user hint), and a poll vote or close tells
	// its channel's (a channel hint). Polls name their channel in the reply.
	// Fix round 2 (I4): asynchronous and coalesced, so no reply waits on a hint.
	hints := client.NewAsyncHints(client.SessionHints{Session: session})
	reminderStore.Hints = hints
	channelName := func(ctx context.Context, channel id.ID) string {
		channels, err := session.Channels(ctx, community)
		if err != nil {
			return ""
		}
		for _, ch := range channels {
			if ch.ID == channel {
				return ch.Name
			}
		}
		return ""
	}
	return &utilityRuntime{polls: service.PollInteractions{Store: pollStore, Authority: actor, CommunityID: community, Now: now, Publisher: publisher, Hints: hints, ChannelName: channelName}, reminders: service.ReminderInteractions{Store: reminderStore, Authority: actor, CommunityID: community, Now: now, Hints: hints}}, nil
}

// gateCreation makes poll and reminder creation read the installation's current capability bit
// on every create (Task 10c): switching Polls or Reminders off takes effect at once, not at the
// next reconcile.
func (u *utilityRuntime) gateCreation(current func(context.Context) (uint16, error)) {
	if u == nil || current == nil {
		return
	}
	gate := func(bit uint16) func(context.Context) (bool, error) {
		return func(ctx context.Context) (bool, error) {
			caps, err := current(ctx)
			if err != nil {
				return false, err
			}
			return caps&bit != 0, nil
		}
	}
	u.polls.CreateEnabled = gate(uint16(catalog.FeaturePolls))
	u.reminders.CreateEnabled = gate(uint16(catalog.FeatureReminders))
}

func (u *utilityRuntime) handlers() map[protocol.Operation]service.InteractionHandlerSpec {
	if u == nil {
		return nil
	}
	out := map[protocol.Operation]service.InteractionHandlerSpec{}
	for op, h := range u.polls.Handlers() {
		out[op] = h
	}
	for op, h := range u.reminders.Handlers() {
		out[op] = h
	}
	return out
}
func (u *utilityRuntime) maintain(ctx context.Context) error {
	if u == nil {
		return errors.New("utility runtime unavailable")
	}
	var errs []error
	if err := u.reminders.Store.Tick(ctx); err != nil {
		errs = append(errs, fmt.Errorf("reminder tick: %w", err))
	}
	if err := u.polls.Reconcile(ctx); err != nil {
		errs = append(errs, fmt.Errorf("poll reconcile: %w", err))
	}
	return errors.Join(errs...)
}

// assembleUtilityInteractionHandlers adds the utility (poll and reminder) handlers to the existing handler map: the
// handlers the sealed intake dispatches to.
func assembleUtilityInteractionHandlers(existing map[protocol.Operation]service.InteractionHandlerSpec, utilities *utilityRuntime) map[protocol.Operation]service.InteractionHandlerSpec {
	handlers := make(map[protocol.Operation]service.InteractionHandlerSpec, len(existing)+12)
	for op, handler := range existing {
		handlers[op] = handler
	}
	for op, handler := range utilities.handlers() {
		handlers[op] = handler
	}
	return handlers
}
