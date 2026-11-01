package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/jonasthim/dilla/internal/bot/client"
	"github.com/jonasthim/dilla/internal/bot/music"
	"github.com/jonasthim/dilla/internal/bot/music/persistence"
	"github.com/jonasthim/dilla/internal/bot/native"
	"github.com/jonasthim/dilla/internal/bot/protocol"
	botstrings "github.com/jonasthim/dilla/internal/bot/strings"
	"github.com/jonasthim/dilla/internal/cborx"
	"github.com/jonasthim/dilla/internal/id"
)

const musicNoticeQueueLimit = 32
const musicNoticeSeenLimit = 1024
const musicNoticeVoiceLimit = 128
const musicNoticeBatchLimit = 5
const musicNoticeGap = 10 * time.Second

// Reserve each attempt in the journal before touching native or HTTP state.
const musicNoticeMaxAttempts = 6

type musicNoticeDestination struct {
	text  id.ID
	group id.ID // Active call group at registration; zero only for legacy callers.
	actor InteractionActor
	grant id.ID
}
type musicNoticeJob struct {
	Voice       id.ID
	Destination musicNoticeDestination
	Items       []string
	Reason      string
	Input       *SendInput
	Attempts    uint32
	ready       time.Time
}
type musicNoticePreparedResolver interface {
	ResolvePrepared(context.Context, SendSession, SendInput) (protocol.SendReceipt, error)
}
type musicNoticeAuthorizedSender interface {
	SendAuthorized(context.Context, SendSession, SendInput, func(context.Context) error) (protocol.SendReceipt, error)
}

// MusicSkipNotices owns one bounded worker per installation. Notify never waits
// for HTTP or MLS work. Prepared sends retain their immutable input until the
// coordinator can resolve them, including through shutdown and reconfiguration.
type MusicSkipNotices struct {
	lifecycle          sync.Mutex
	mu                 sync.Mutex
	destinations       map[id.ID]musicNoticeDestination
	seen               map[string]bool
	seenOrder          []string
	jobs               []*musicNoticeJob
	last               map[id.ID]time.Time
	send               func(context.Context, *musicNoticeJob) error
	logger             *slog.Logger
	clock              func() time.Time
	wake               chan struct{}
	cancel             context.CancelFunc
	done               chan struct{}
	closed             bool
	state              persistence.Native
	journalRevision    uint64
	journalKey         []byte
	journalFailed      bool
	pendingTransaction *musicNoticeTransaction
}

// SetState supplies the existing installation-bound encrypted native state. It
// must be set before Configure; no new protocol or native operation is used.
func (n *MusicSkipNotices) SetState(state persistence.Native) {
	n.lifecycle.Lock()
	defer n.lifecycle.Unlock()
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.cancel == nil {
		n.state = state
	}
}

func (n *MusicSkipNotices) Configure(h MusicAnnouncements, logger *slog.Logger) {
	n.lifecycle.Lock()
	defer n.lifecycle.Unlock()
	n.mu.Lock()
	running := n.cancel != nil
	n.mu.Unlock()
	if running {
		n.stop(false)
	}
	if logger == nil {
		logger = slog.Default()
	}
	n.mu.Lock()
	n.logger = logger
	n.clock = h.Now
	if n.clock == nil {
		n.clock = time.Now
	}
	n.closed = false
	n.wake = make(chan struct{}, 1)
	n.done = make(chan struct{})
	if n.last == nil {
		n.last = make(map[id.ID]time.Time)
	}
	if n.seen == nil {
		n.seen = make(map[string]bool)
	}
	if n.destinations == nil {
		n.destinations = make(map[id.ID]musicNoticeDestination)
	}
	ctx, cancel := context.WithCancel(context.Background())
	n.cancel = cancel
	n.journalKey = []byte("dilla.bot.music.skip-notices.v1/" + h.CommunityID.String())
	if !running {
		n.journalRevision = 0
		n.journalFailed = false
	}
	n.send = func(ctx context.Context, job *musicNoticeJob) error {
		// Reserve a restored/retried attempt even if current configuration is
		// incomplete; repeated restarts must not reset its recovery budget.
		reserved := job.Input != nil
		if reserved {
			if err := n.persist(ctx, job); err != nil {
				return err
			}
		}
		sender, ok := h.Sender.(musicNoticeAuthorizedSender)
		if !ok || h.Music == nil || h.Music.Authority == nil || h.Channels == nil || h.Destination == nil || h.Discovery == nil || h.Session == nil {
			return errors.New("music notice unavailable")
		}
		preflight := func(ctx context.Context) error {
			n.mu.Lock()
			destination, known := n.destinations[job.Voice]
			closed := n.closed
			n.mu.Unlock()
			if closed || !known || destination != job.Destination {
				return errors.New("music notice registration unavailable")
			}
			call, err := h.Music.Authority.CurrentCall(ctx, job.Voice)
			if err != nil {
				return err
			}
			if !validMusicCall(call, job.Voice) || !destination.group.IsZero() && call.GroupID != destination.group {
				n.Clear(job.Voice)
				return errors.New("music notice call unavailable")
			}
			allowed, err := h.Music.Authority.CanDJ(ctx, destination.actor, call)
			if err != nil {
				return err
			}
			if !allowed {
				n.Clear(job.Voice)
				return errors.New("music notice authority unavailable")
			}
			bits, visible, err := h.Channels.BotActorChannelAuthority(ctx, h.CommunityID, destination.actor.User, destination.text)
			if err != nil {
				return err
			}
			if !visible || bits&musicAnnounceChannelSend != musicAnnounceChannelSend {
				n.Clear(job.Voice)
				return errors.New("music notice destination unavailable")
			}
			group, err := h.Destination.ResolveTextGroup(ctx, h.CommunityID, destination.text)
			if err != nil {
				return err
			}
			if group.IsZero() || job.Input != nil && group != job.Input.GroupID {
				n.Clear(job.Voice)
				return errors.New("music notice destination changed")
			}
			return nil
		}
		if job.Input == nil {
			// The bounded memory cache is an optimization. Durable per-item reservations
			// prevent an overlapping old batch from reposting after cache eviction.
			if n.state != nil {
				fresh := make([]string, 0, len(job.Items))
				for _, item := range job.Items {
					value, err := n.state.StateGet(ctx, 8, musicNoticeItemKey(job.Voice, item))
					if err != nil {
						return err
					}
					if value.Revision == 0 && !value.Tombstone {
						fresh = append(fresh, item)
					}
				}
				job.Items = fresh
				if len(fresh) == 0 {
					return ErrSendTerminal
				}
			}
			if err := preflight(ctx); err != nil {
				return err
			}
			group, err := h.Destination.ResolveTextGroup(ctx, h.CommunityID, job.Destination.text)
			if err != nil {
				return err
			}
			discovery, _, err := h.Discovery.DiscoverObserved(ctx)
			if err != nil {
				return err
			}
			if discovery.Generation == 0 {
				return client.ErrProtocol
			}
			body, err := cborx.Marshal([]any{uint64(0), nil, botstrings.MusicSkippedBatch(len(job.Items), job.Reason), []any{}, nil})
			if err != nil {
				return err
			}
			clock := h.Now
			if clock == nil {
				clock = time.Now
			}
			created := clock().UTC().Unix()
			if created < 0 {
				return errors.New("music notice clock unavailable")
			}
			operation, attempt := musicSkipNoticeIDs(h.CommunityID, job.Voice, job.Items)
			job.Input = &SendInput{OperationID: operation, AttemptID: attempt, GroupID: group, RequestBody: body, OriginalNow: uint64(created), ExpectedGeneration: discovery.Generation}
		}
		// Write the exact input before a native PrepareSend can persist ciphertext.
		if !reserved {
			if err := n.persist(ctx, job); err != nil {
				return err
			}
		}
		resolve := func() (protocol.SendReceipt, error) {
			resolver, ok := h.Sender.(musicNoticePreparedResolver)
			if !ok {
				return protocol.SendReceipt{}, errors.New("music notice resolution unavailable")
			}
			return resolver.ResolvePrepared(ctx, h.Session, *job.Input)
		}
		var receipt protocol.SendReceipt
		var err error
		if n.registrationCurrent(job) {
			receipt, err = sender.SendAuthorized(ctx, h.Session, *job.Input, preflight)
			// A definitive revocation during preparation still settles the same
			// attempt without granting its old ciphertext a fresh upload opportunity.
			if err != nil && !n.registrationCurrent(job) {
				receipt, err = resolve()
			}
		} else {
			receipt, err = resolve()
		}
		if err != nil {
			return err
		}
		in := job.Input
		if receipt.OperationID != in.OperationID || receipt.AttemptID != in.AttemptID || receipt.GroupID != in.GroupID || receipt.CommunityID != h.CommunityID {
			return client.ErrProtocol
		}
		if receipt.Outcome != protocol.ReceiptAccepted {
			return ErrSendTerminal
		}
		return nil
	}
	n.mu.Unlock()
	// Loading precedes the worker, so a restarted instance resumes the stored
	// timestamp, body, group and generation rather than sampling replacements.
	if err := func() error {
		if running {
			return nil
		}
		return n.restore(ctx, h.CommunityID)
	}(); err != nil {
		n.mu.Lock()
		n.journalFailed = true
		n.mu.Unlock()
		logger.WarnContext(ctx, "bot music skip notice state unavailable")
	}
	go n.run(ctx)
}

func (n *MusicSkipNotices) registrationCurrent(job *musicNoticeJob) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	current, known := n.destinations[job.Voice]
	return !n.closed && known && current == job.Destination
}

func (n *MusicSkipNotices) Register(voice, text id.ID, actor InteractionActor, callGroups ...id.ID) {
	if n == nil {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closed {
		return
	}
	if n.destinations == nil {
		n.destinations = make(map[id.ID]musicNoticeDestination)
	}
	if _, known := n.destinations[voice]; !known && len(n.destinations) >= musicNoticeVoiceLimit {
		for oldest := range n.destinations {
			delete(n.destinations, oldest)
			break
		}
	}
	var callGroup id.ID
	if len(callGroups) > 0 {
		callGroup = callGroups[0]
	}
	grant := id.New()
	if current, known := n.destinations[voice]; known && current.text == text && current.actor == actor && current.group == callGroup {
		grant = current.grant
	}
	n.destinations[voice] = musicNoticeDestination{text: text, group: callGroup, actor: actor, grant: grant}
	n.signalLocked()
}

// Clear removes authority immediately. An unresolved prepared attempt remains
// journaled for receipt recovery, but its preflight cannot upload new content.
func (n *MusicSkipNotices) Clear(voice id.ID) {
	if n == nil {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	delete(n.destinations, voice)
	for i := len(n.jobs) - 1; i >= 0; i-- {
		if n.jobs[i].Voice == voice && n.jobs[i].Input == nil {
			n.jobs = append(n.jobs[:i], n.jobs[i+1:]...)
		}
	}
	n.signalLocked()
}

func (n *MusicSkipNotices) Close() {
	if n == nil {
		return
	}
	n.lifecycle.Lock()
	defer n.lifecycle.Unlock()
	n.stop(true)
}

func (n *MusicSkipNotices) stop(clearRoutes bool) {
	if n == nil {
		return
	}
	n.mu.Lock()
	n.closed = true
	if clearRoutes {
		n.destinations = make(map[id.ID]musicNoticeDestination)
	}
	cancel, done := n.cancel, n.done
	if cancel != nil {
		cancel()
	}
	n.mu.Unlock()
	if done != nil {
		<-done
	}
	n.mu.Lock()
	n.cancel = nil
	if clearRoutes {
		kept := n.jobs[:0]
		for _, job := range n.jobs {
			if job.Input != nil {
				kept = append(kept, job)
			}
		}
		n.jobs = kept
	}
	n.mu.Unlock()
}

func (n *MusicSkipNotices) Notify(ctx context.Context, voice id.ID, skips []music.SkippedTrack) {
	if n == nil || len(skips) == 0 {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	logger := n.logger
	if logger == nil {
		logger = slog.Default()
	}
	destination, known := n.destinations[voice]
	for _, skip := range skips {
		logger.InfoContext(ctx, "bot music track skipped", "channel_id", voice.String(), "class", music.FailureClass(skip.Failure), "notice_destination_known", known)
	}
	if !known || n.send == nil || n.closed || n.journalFailed {
		return
	}
	if n.seen == nil {
		n.seen = make(map[string]bool)
	}
	items := make([]string, 0, len(skips))
	for _, skip := range skips {
		item := skip.Track.ID
		key := voice.String() + "/" + item
		if item == "" || n.seen[key] {
			continue
		}
		n.rememberLocked(key)
		items = append(items, item)
		if len(items) >= musicNoticeBatchLimit {
			break
		}
	}
	if len(items) == 0 {
		return
	}
	sort.Strings(items)
	reason := "provider unavailable"
	if music.FailureClass(skips[0].Failure) == "pcm_start" {
		reason = "audio unavailable"
	}
	n.enqueueLocked(&musicNoticeJob{Voice: voice, Destination: destination, Items: items, Reason: reason})
	n.signalLocked()
}
func (n *MusicSkipNotices) rememberLocked(key string) {
	if n.seen[key] {
		return
	}
	if len(n.seenOrder) >= musicNoticeSeenLimit {
		delete(n.seen, n.seenOrder[0])
		n.seenOrder = n.seenOrder[1:]
	}
	n.seen[key] = true
	n.seenOrder = append(n.seenOrder, key)
}
func musicNoticeItemKey(voice id.ID, item string) []byte {
	hash := sha256.Sum256([]byte(item))
	return []byte("dilla.bot.music.skip-item.v1/" + voice.String() + "/" + hex.EncodeToString(hash[:]))
}
func (n *MusicSkipNotices) signalLocked() {
	if n.wake != nil {
		select {
		case n.wake <- struct{}{}:
		default:
		}
	}
}
func (n *MusicSkipNotices) enqueueLocked(job *musicNoticeJob) {
	if len(n.jobs) >= musicNoticeQueueLimit {
		// Never discard an unresolved native attempt. Drop the oldest unprepared
		// entry; if all slots are prepared, drop the new unprepared notification.
		dropped := -1
		for i, old := range n.jobs {
			if old.Input == nil {
				dropped = i
				break
			}
		}
		if dropped < 0 {
			n.logger.Warn("bot music skip notice queue full")
			if job.Input != nil {
				n.jobs = append(n.jobs, job)
			}
			return
		}
		n.jobs = append(n.jobs[:dropped], n.jobs[dropped+1:]...)
		n.logger.Warn("bot music skip notice queue full")
	}
	n.jobs = append(n.jobs, job)
}
func (n *MusicSkipNotices) run(ctx context.Context) {
	defer close(n.done)
	for {
		if ctx.Err() != nil {
			return
		}
		now := n.clock()
		n.mu.Lock()
		var job *musicNoticeJob
		delay := musicNoticeGap
		for i := 0; i < len(n.jobs); i++ {
			candidate := n.jobs[i]
			if wait := candidate.ready.Sub(now); wait > 0 {
				if wait < delay {
					delay = wait
				}
				continue
			}
			if _, known := n.last[candidate.Voice]; !known && len(n.last) >= musicNoticeVoiceLimit {
				// Retain every unexpired slot: eviction could let that voice post twice
				// inside the gap. New unprepared work is best effort; prepared work waits.
				for voice, at := range n.last {
					if now.Sub(at) >= musicNoticeGap {
						delete(n.last, voice)
					}
				}
				if len(n.last) >= musicNoticeVoiceLimit {
					if candidate.Input == nil {
						n.jobs = append(n.jobs[:i], n.jobs[i+1:]...)
						i--
					} else {
						candidate.ready = now.Add(musicNoticeGap)
					}
					continue
				}
			}
			if wait := n.last[candidate.Voice].Add(musicNoticeGap).Sub(now); wait > 0 && candidate.Input != nil {
				candidate.ready = now.Add(wait)
				if wait < delay {
					delay = wait
				}
				continue
			}
			n.jobs = append(n.jobs[:i], n.jobs[i+1:]...)
			i--
			if candidate.Input == nil && now.Sub(n.last[candidate.Voice]) < musicNoticeGap {
				continue
			}
			job = candidate
			n.last[job.Voice] = now
			break
		}
		n.mu.Unlock()
		if job == nil {
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-n.wake:
				timer.Stop()
			case <-timer.C:
			}
			continue
		}
		var err error
		if job.Attempts >= musicNoticeMaxAttempts {
			err = ErrSendTerminal
			n.logger.WarnContext(ctx, "bot music skip notice dropped", "channel_id", job.Voice.String(), "reason", "retry_exhausted")
		} else {
			job.Attempts++
			sendCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			err = n.send(sendCtx, job)
			cancel()
		}
		if err != nil {
			disposition := "dropped"
			terminal := errors.Is(err, ErrSendTerminal) || errors.Is(err, ErrSendNotPrepared)
			if terminal {
				disposition = "send_terminal"
			}
			if !terminal && job.Attempts >= musicNoticeMaxAttempts {
				disposition = "retry_exhausted"
			}
			if (job.Input != nil || ctx.Err() != nil) && !terminal && job.Attempts < musicNoticeMaxAttempts {
				disposition = "retry"
				job.ready = n.clock().Add(musicNoticeGap)
				n.mu.Lock()
				n.enqueueLocked(job)
				n.mu.Unlock()
			}
			n.logger.WarnContext(ctx, "bot music skip notice failed", "channel_id", job.Voice.String(), "class", music.FailureClass(err), "disposition", disposition)
		}
		// If cancellation interrupted the HTTP boundary, keep the pre-send journal
		// unchanged; it includes this unresolved job even when the queue was full.
		if ctx.Err() != nil {
			return
		}
		saveCtx, saveCancel := context.WithTimeout(ctx, 5*time.Second)
		if err := n.persist(saveCtx, nil); err != nil {
			n.logger.WarnContext(ctx, "bot music skip notice state unavailable")
		}
		saveCancel()
	}
}

// musicSkipNoticeIDs binds a notice to a set of queue item IDs, without depending
// on batch order, queue metadata or a randomly generated request identity.
func musicSkipNoticeIDs(community, voice id.ID, items []string) (operation, attempt id.ID) {
	sorted := append([]string(nil), items...)
	sort.Strings(sorted)
	seed := append(append([]byte("dilla music skip notice v1\x00"), community[:]...), voice[:]...)
	previous := ""
	for _, item := range sorted {
		if item == previous {
			continue
		}
		previous = item
		seed = binary.BigEndian.AppendUint64(seed, uint64(len(item)))
		seed = append(seed, item...)
	}
	digest := sha256.Sum256(seed)
	copy(operation[:], digest[:len(operation)])
	next := sha256.Sum256(append([]byte("dilla music skip notice attempt v1\x00"), operation[:]...))
	copy(attempt[:], next[:len(attempt)])
	return operation, attempt
}

type musicNoticeRecord struct {
	Voice, Text id.ID
	CallGroup   id.ID
	Grant       id.ID
	Actor       InteractionActor
	Items       []string
	Reason      string
	Input       SendInput
	Attempts    uint32
}
type musicNoticeJournal struct {
	Pending []musicNoticeRecord
	Seen    []string
}

type musicNoticeTransaction struct {
	ID       id.ID
	Changes  []native.StateChange
	Revision uint64
	Body     []byte
}

// flushPending replays exactly one ambiguous native transaction. New Notify
// calls may extend seen meanwhile; their snapshot waits until this outcome is
// known, so neither the transaction body nor its expected revision can drift.
func (n *MusicSkipNotices) flushPending(ctx context.Context) error {
	n.mu.Lock()
	pending, state, key := n.pendingTransaction, n.state, append([]byte(nil), n.journalKey...)
	n.mu.Unlock()
	if pending == nil {
		return nil
	}
	revisions, err := state.StateTransaction(ctx, pending.ID, pending.Changes)
	accepted := err == nil && len(revisions) == len(pending.Changes) && revisions[0] == pending.Revision+1
	if !accepted {
		actual, readErr := state.StateGet(ctx, 8, key)
		accepted = readErr == nil && !actual.Tombstone && actual.Revision == pending.Revision+1 && bytes.Equal(actual.Value, pending.Body)
	}
	if !accepted {
		return errors.New("music notice journal update unavailable")
	}
	n.mu.Lock()
	n.journalRevision = pending.Revision + 1
	n.pendingTransaction = nil
	n.mu.Unlock()
	return nil
}

// persist runs only on the installation's worker. A versioned canonical CBOR
// array wraps a private JSON document in the existing encrypted area 8.
func (n *MusicSkipNotices) persist(ctx context.Context, active *musicNoticeJob) error {
	if err := n.flushPending(ctx); err != nil {
		return err
	}
	n.mu.Lock()
	state, key, revision, failed := n.state, append([]byte(nil), n.journalKey...), n.journalRevision, n.journalFailed
	if state == nil {
		n.mu.Unlock()
		return nil
	}
	journal := musicNoticeJournal{}
	for seen := range n.seen {
		journal.Seen = append(journal.Seen, seen)
	}
	sort.Strings(journal.Seen)
	jobs := append([]*musicNoticeJob(nil), n.jobs...)
	if active != nil {
		jobs = append(jobs, active)
	}
	for _, job := range jobs {
		if job.Input != nil {
			journal.Pending = append(journal.Pending, musicNoticeRecord{Voice: job.Voice, Text: job.Destination.text, CallGroup: job.Destination.group, Grant: job.Destination.grant, Actor: job.Destination.actor, Items: job.Items, Reason: job.Reason, Input: *job.Input, Attempts: job.Attempts})
		}
	}
	n.mu.Unlock()
	if failed {
		return errors.New("music notice journal unavailable")
	}
	document, err := json.Marshal(journal)
	if err != nil {
		return err
	}
	body, err := cborx.Marshal([]any{uint64(1), document})
	if len(body) > 256*1024 {
		return errors.New("music notice journal exceeds state bound")
	}
	if err != nil {
		return err
	}
	seed := append([]byte("dilla music skip state v1\x00"), key...)
	seed = binary.BigEndian.AppendUint64(seed, revision)
	seed = append(seed, body...)
	hash := sha256.Sum256(seed)
	transaction := id.ID(hash[:16])
	changes := []native.StateChange{{Area: 8, Key: key, ExpectedRevision: revision, Value: body}}
	if active != nil {
		marker, _ := cborx.Marshal([]any{uint64(1)})
		for _, item := range active.Items {
			itemKey := musicNoticeItemKey(active.Voice, item)
			prior, err := state.StateGet(ctx, 8, itemKey)
			if err != nil {
				return err
			}
			if prior.Revision == 0 {
				changes = append(changes, native.StateChange{Area: 8, Key: itemKey, ExpectedRevision: 0, Value: marker})
			}
		}
	}
	n.mu.Lock()
	n.pendingTransaction = &musicNoticeTransaction{ID: transaction, Changes: changes, Revision: revision, Body: body}
	n.mu.Unlock()
	return n.flushPending(ctx)
}
func (n *MusicSkipNotices) restore(ctx context.Context, community id.ID) error {
	n.mu.Lock()
	state, key := n.state, append([]byte(nil), n.journalKey...)
	n.mu.Unlock()
	if state == nil {
		return nil
	}
	if state.Binding().Community != community {
		return errors.New("music notice journal binding mismatch")
	}
	value, err := state.StateGet(ctx, 8, key)
	if err != nil {
		return err
	}
	if value.Tombstone {
		return errors.New("music notice journal deleted")
	}
	if value.Revision == 0 {
		return nil
	}
	var envelope []any
	if err := cborx.Unmarshal(value.Value, &envelope); err != nil {
		return err
	}
	if len(envelope) != 2 || envelope[0] != uint64(1) {
		return errors.New("invalid music notice journal version")
	}
	document, ok := envelope[1].([]byte)
	if !ok {
		return errors.New("invalid music notice journal document")
	}
	var journal musicNoticeJournal
	if err := json.Unmarshal(document, &journal); err != nil {
		return err
	}
	if len(journal.Pending) > musicNoticeQueueLimit+1 || len(journal.Seen) > musicNoticeSeenLimit {
		return errors.New("music notice journal exceeds queue bound")
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	n.journalRevision = value.Revision
	n.pendingTransaction = nil
	n.jobs = nil
	for _, key := range journal.Seen {
		n.rememberLocked(key)
	}
	for _, record := range journal.Pending {
		if record.Voice.IsZero() || record.Text.IsZero() || record.Grant.IsZero() || record.Input.OperationID.IsZero() || record.Input.AttemptID.IsZero() || record.Input.GroupID.IsZero() || len(record.Items) == 0 || len(record.Items) > musicNoticeBatchLimit || record.Attempts > musicNoticeMaxAttempts {
			return errors.New("invalid music notice journal")
		}
		operation, attempt := musicSkipNoticeIDs(community, record.Voice, record.Items)
		if operation != record.Input.OperationID || attempt != record.Input.AttemptID {
			return errors.New("music notice journal identity mismatch")
		}
		in := record.Input
		n.jobs = append(n.jobs, &musicNoticeJob{Voice: record.Voice, Destination: musicNoticeDestination{text: record.Text, group: record.CallGroup, actor: record.Actor, grant: record.Grant}, Items: record.Items, Reason: record.Reason, Input: &in, Attempts: record.Attempts})
	}
	return nil
}
