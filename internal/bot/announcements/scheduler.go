package announcements

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/dilla-chat/dilla/internal/bot/native"
	"github.com/dilla-chat/dilla/internal/bot/protocol"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
)

const (
	schedulerStateArea   = 8
	schedulerStateKey    = "dilla.bot.announcements.v1/"
	maxAnnouncements     = 128
	maxAnnouncementText  = 4000
	maxAnnouncementState = 262144
	maxMutationReceipts  = 512
)

var (
	ErrSchedulerConflict    = errors.New("announcement scheduler state conflict")
	ErrSchedulerMalformed   = errors.New("malformed announcement scheduler state")
	ErrAnnouncementNotFound = errors.New("announcement not found")
	ErrSendNotStarted       = errors.New("announcement send window closed before send started")
	ErrMutationIDConflict   = errors.New("announcement mutation request ID reused with different input")
	ErrMutationReceiptLimit = errors.New("announcement mutation receipt capacity reached")
	ErrMutationExpired      = errors.New("announcement mutation request expired")
	ErrInvalidRetryTime     = errors.New("invalid announcement retry time")
)

// Native exposes the helper's encrypted, community-bound application state.
type Native interface {
	Binding() native.Binding
	StateGet(context.Context, uint64, []byte) (native.StateValue, error)
	StateTransaction(context.Context, id.ID, []native.StateChange) ([]uint64, error)
}

// Destination resolves a current authorized text channel to its MLS group.
type Destination interface {
	ResolveTextGroup(context.Context, id.ID, id.ID) (id.ID, error)
}

// Authorization rechecks current permission to post each due announcement.
type Authorization interface {
	CanAnnounce(context.Context, id.ID) (bool, error)
}

// Sender must invoke BeforeSend exactly once at its send boundary and stop
// without posting when it returns false. Retries must use the same IDs and
// recover prior receipts before starting another post.
type Sender interface {
	SendAnnouncement(context.Context, SendRequest) (protocol.SendReceipt, error)
}

// ReceiptLookup resolves an earlier attempt without starting a new send.
type ReceiptLookup interface {
	LookupAnnouncement(context.Context, SendRequest) (protocol.SendReceipt, bool, error)
}

type SendRequest struct {
	OperationID, AttemptID                               id.ID
	PredecessorAttemptID                                 *id.ID
	CommunityID, InstallationID, AnnouncementID, GroupID id.ID
	Text                                                 string
	OriginalNow                                          uint64
	BeforeSend                                           func() bool
}

type Announcement struct {
	ID, DestinationID id.ID
	Text              string
	Schedule          Schedule
	Enabled           bool
}

type SchedulerOptions struct {
	Destination   Destination
	Authorization Authorization
	Sender        Sender
	ReceiptLookup ReceiptLookup
	Now           func() time.Time
}

type occurrenceStatus uint64

const (
	occurrenceNone occurrenceStatus = iota
	occurrenceReady
	occurrenceSending
	occurrenceSent
	occurrenceSkipped
)

type scheduledState struct {
	Announcement Announcement
	Occurrence   Occurrence
	Status       occurrenceStatus
	GroupID      id.ID
	Attempt      uint64
	RetryAt      uint64
}

// MutationOutcome is the stable application result of a schedule mutation.
// Revision is the encrypted scheduler document revision after storing the
// outcome and its receipt.
type MutationOutcome struct {
	Status   uint64
	Revision *uint64
	Data     []byte
}

type mutationReceipt struct {
	RequestID   id.ID
	Fingerprint [32]byte
	Expires     uint64
	Status      uint64
	Revision    *uint64
	Data        []byte
}

type Scheduler struct {
	mu            sync.Mutex
	native        Native
	binding       native.Binding
	destination   Destination
	authorization Authorization
	sender        Sender
	receiptLookup ReceiptLookup
	now           func() time.Time
	receipts      []mutationReceipt
}

func NewScheduler(n Native, options SchedulerOptions) (*Scheduler, error) {
	if n == nil || options.Destination == nil || options.Authorization == nil || options.Sender == nil || options.ReceiptLookup == nil {
		return nil, errors.New("missing announcement scheduler dependency")
	}
	b := n.Binding()
	if b.Instance.IsZero() || b.Community.IsZero() || b.Installation.IsZero() {
		return nil, errors.New("invalid announcement scheduler binding")
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	return &Scheduler{native: n, binding: b, destination: options.Destination, authorization: options.Authorization, sender: options.Sender, receiptLookup: options.ReceiptLookup, now: options.Now}, nil
}

// Put creates or revises a schedule. Revising it replaces the occurrence
// cursor, so an occurrence that has not been claimed for sending is invalidated.
func (s *Scheduler) Put(ctx context.Context, value Announcement) error {
	if s == nil {
		return errors.New("missing announcement scheduler")
	}
	if err := validateAnnouncement(value); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	states, revision, err := s.load(ctx)
	if err != nil {
		return err
	}
	states, err = putAnnouncement(states, value, s.now().UTC())
	if err != nil {
		return err
	}
	return s.save(ctx, revision, states, s.receipts)
}

// PutWithReceipt atomically stores the Put outcome and its request receipt in
// the same encrypted state transaction. The fingerprint covers the complete
// canonical application request; MLS event epoch/delivery coordinates do not
// participate, so retries carried by a later epoch return the original result.
func (s *Scheduler) PutWithReceipt(ctx context.Context, requestID id.ID, fingerprint [32]byte, expires uint64, value Announcement) (MutationOutcome, error) {
	return s.putWithReceipt(ctx, nil, requestID, fingerprint, expires, value)
}

// PutWithReceiptAtRevision applies a schedule mutation only when the encrypted
// scheduler document still has expectedRevision. A mismatch is itself stored
// as a durable conflict receipt, so retrying the same request returns the same
// outcome without another state write.
func (s *Scheduler) PutWithReceiptAtRevision(ctx context.Context, expectedRevision uint64, requestID id.ID, fingerprint [32]byte, expires uint64, value Announcement) (MutationOutcome, error) {
	return s.putWithReceipt(ctx, &expectedRevision, requestID, fingerprint, expires, value)
}

func (s *Scheduler) putWithReceipt(ctx context.Context, expectedRevision *uint64, requestID id.ID, fingerprint [32]byte, expires uint64, value Announcement) (MutationOutcome, error) {
	if s == nil {
		return MutationOutcome{}, errors.New("missing announcement scheduler")
	}
	if err := validateAnnouncement(value); err != nil {
		return MutationOutcome{}, err
	}
	if err := s.validateMutationInput(requestID, expires); err != nil {
		return MutationOutcome{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	states, revision, err := s.load(ctx)
	if err != nil {
		return MutationOutcome{}, err
	}
	now := s.now().UTC().Unix()
	if now < 0 {
		return MutationOutcome{}, ErrMutationExpired
	}
	if outcome, found, lookupErr := findMutationReceipt(s.receipts, requestID, fingerprint, uint64(now)); found || lookupErr != nil {
		return outcome, lookupErr
	}
	if expectedRevision != nil && revision != *expectedRevision {
		return s.saveMutationOutcome(ctx, revision, states, requestID, fingerprint, expires, MutationOutcome{Status: 4, Data: []byte{0x80}})
	}
	states, err = putAnnouncement(states, value, s.now().UTC())
	if err != nil {
		if errors.Is(err, ErrSchedulerConflict) {
			return s.saveMutationOutcome(ctx, revision, states, requestID, fingerprint, expires, MutationOutcome{Status: 4, Data: []byte{0x80}})
		}
		return MutationOutcome{}, err
	}
	return s.saveMutationOutcome(ctx, revision, states, requestID, fingerprint, expires,
		MutationOutcome{Data: []byte{0x80}})
}

func putAnnouncement(states []scheduledState, value Announcement, now time.Time) ([]scheduledState, error) {
	i := findAnnouncement(states, value.ID)
	if i < 0 {
		if len(states) >= maxAnnouncements {
			return states, errors.New("announcement schedule limit reached")
		}
		if value.Schedule.Revision == 0 {
			return states, errors.New("new announcement revision must be positive")
		}
		next, nextErr := NextOccurrence(value.Schedule, now)
		if nextErr != nil && !errors.Is(nextErr, ErrNoOccurrence) {
			return states, nextErr
		}
		state := scheduledState{Announcement: value}
		if nextErr == nil {
			state.Occurrence, state.Status = next, occurrenceReady
		}
		states = append(states, state)
	} else {
		old := states[i]
		if value.Schedule.Revision != old.Announcement.Schedule.Revision+1 {
			return states, ErrSchedulerConflict
		}
		next, nextErr := NextOccurrence(value.Schedule, now)
		if nextErr != nil && !errors.Is(nextErr, ErrNoOccurrence) {
			return states, nextErr
		}
		state := scheduledState{Announcement: value}
		if nextErr == nil {
			state.Occurrence, state.Status = next, occurrenceReady
		}
		states[i] = state
	}
	return states, nil
}

// Delete removes a schedule only at its expected schedule revision. The check
// and encrypted state transaction run under the same scheduler lock as Put.
func (s *Scheduler) Delete(ctx context.Context, announcementID id.ID, expectedRevision uint64) error {
	if s == nil || announcementID.IsZero() || expectedRevision == 0 {
		return errors.New("invalid announcement ID or revision")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	states, revision, err := s.load(ctx)
	if err != nil {
		return err
	}
	i := findAnnouncement(states, announcementID)
	if i < 0 {
		return ErrAnnouncementNotFound
	}
	if states[i].Announcement.Schedule.Revision != expectedRevision {
		return ErrSchedulerConflict
	}
	states = append(states[:i], states[i+1:]...)
	return s.save(ctx, revision, states, s.receipts)
}

// DeleteWithReceipt atomically stores the Delete outcome and its request
// receipt in the same encrypted state transaction.
func (s *Scheduler) DeleteWithReceipt(ctx context.Context, requestID id.ID, fingerprint [32]byte, expires uint64, announcementID id.ID, expectedRevision uint64) (MutationOutcome, error) {
	return s.deleteWithReceipt(ctx, nil, requestID, fingerprint, expires, announcementID, expectedRevision)
}

// DeleteWithReceiptAtRevision adds an atomic scheduler-document revision check
// to DeleteWithReceipt.
func (s *Scheduler) DeleteWithReceiptAtRevision(ctx context.Context, documentRevision uint64, requestID id.ID, fingerprint [32]byte, expires uint64, announcementID id.ID, expectedRevision uint64) (MutationOutcome, error) {
	return s.deleteWithReceipt(ctx, &documentRevision, requestID, fingerprint, expires, announcementID, expectedRevision)
}

func (s *Scheduler) deleteWithReceipt(ctx context.Context, documentRevision *uint64, requestID id.ID, fingerprint [32]byte, expires uint64, announcementID id.ID, expectedRevision uint64) (MutationOutcome, error) {
	if s == nil || announcementID.IsZero() || expectedRevision == 0 {
		return MutationOutcome{}, errors.New("invalid announcement ID or revision")
	}
	if err := s.validateMutationInput(requestID, expires); err != nil {
		return MutationOutcome{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	states, revision, err := s.load(ctx)
	if err != nil {
		return MutationOutcome{}, err
	}
	now := s.now().UTC().Unix()
	if now < 0 {
		return MutationOutcome{}, ErrMutationExpired
	}
	if outcome, found, lookupErr := findMutationReceipt(s.receipts, requestID, fingerprint, uint64(now)); found || lookupErr != nil {
		return outcome, lookupErr
	}
	if documentRevision != nil && revision != *documentRevision {
		return s.saveMutationOutcome(ctx, revision, states, requestID, fingerprint, expires, MutationOutcome{Status: 4, Data: []byte{0x80}})
	}
	i := findAnnouncement(states, announcementID)
	if i < 0 {
		return s.saveMutationOutcome(ctx, revision, states, requestID, fingerprint, expires, MutationOutcome{Status: 5, Data: []byte{0x80}})
	}
	if states[i].Announcement.Schedule.Revision != expectedRevision {
		return s.saveMutationOutcome(ctx, revision, states, requestID, fingerprint, expires, MutationOutcome{Status: 4, Data: []byte{0x80}})
	}
	states = append(states[:i], states[i+1:]...)
	return s.saveMutationOutcome(ctx, revision, states, requestID, fingerprint, expires, MutationOutcome{Data: []byte{0x80}})
}

func (s *Scheduler) saveMutationOutcome(ctx context.Context, revision uint64, states []scheduledState, requestID id.ID, fingerprint [32]byte, expires uint64, outcome MutationOutcome) (MutationOutcome, error) {
	if revision >= math.MaxInt64 {
		return MutationOutcome{}, errors.New("announcement state revision exhausted")
	}
	resultRevision := revision + 1
	outcome.Revision = &resultRevision
	receipts, err := s.nextMutationReceipts(requestID, fingerprint, expires, outcome)
	if err != nil {
		return MutationOutcome{}, err
	}
	if err := s.save(ctx, revision, states, receipts); err != nil {
		return MutationOutcome{}, err
	}
	return outcome, nil
}

func (s *Scheduler) validateMutationInput(requestID id.ID, expires uint64) error {
	if requestID.IsZero() || expires == 0 || expires > math.MaxInt64 {
		return errors.New("invalid announcement mutation receipt input")
	}
	now := s.now().UTC().Unix()
	if now < 0 || uint64(now) >= expires || uint64(now) > math.MaxInt64-30 || expires > uint64(now)+30 {
		return ErrMutationExpired
	}
	return nil
}

func (s *Scheduler) nextMutationReceipts(requestID id.ID, fingerprint [32]byte, expires uint64, outcome MutationOutcome) ([]mutationReceipt, error) {
	nowSeconds := s.now().UTC().Unix()
	if nowSeconds < 0 {
		return nil, ErrMutationExpired
	}
	now := uint64(nowSeconds)
	kept := make([]mutationReceipt, 0, len(s.receipts)+1)
	for _, receipt := range s.receipts {
		if receipt.Expires > now {
			kept = append(kept, receipt)
		}
	}
	if len(kept) >= maxMutationReceipts {
		return nil, ErrMutationReceiptLimit
	}
	kept = append(kept, mutationReceipt{RequestID: requestID, Fingerprint: fingerprint, Expires: expires,
		Status: outcome.Status, Revision: cloneUint64(outcome.Revision), Data: append([]byte(nil), outcome.Data...)})
	return kept, nil
}

func findMutationReceipt(receipts []mutationReceipt, requestID id.ID, fingerprint [32]byte, now uint64) (MutationOutcome, bool, error) {
	for _, receipt := range receipts {
		if receipt.RequestID != requestID || receipt.Expires <= now {
			continue
		}
		if receipt.Fingerprint != fingerprint {
			return MutationOutcome{}, true, ErrMutationIDConflict
		}
		return MutationOutcome{Status: receipt.Status, Revision: cloneUint64(receipt.Revision), Data: append([]byte(nil), receipt.Data...)}, true, nil
	}
	return MutationOutcome{}, false, nil
}

func cloneUint64(value *uint64) *uint64 {
	if value == nil {
		return nil
	}
	out := *value
	return &out
}

// List returns the current bounded schedule set in stable ID order.
func (s *Scheduler) List(ctx context.Context) ([]Announcement, error) {
	rows, _, err := s.ListAtRevision(ctx)
	return rows, err
}

// ListAtRevision returns schedules and the encrypted document revision from
// the same native state read. Mutations use this revision as their request CAS.
func (s *Scheduler) ListAtRevision(ctx context.Context) ([]Announcement, uint64, error) {
	if s == nil {
		return nil, 0, errors.New("missing announcement scheduler")
	}
	s.mu.Lock()
	states, revision, err := s.load(ctx)
	s.mu.Unlock()
	if err != nil {
		return nil, 0, err
	}
	out := make([]Announcement, 0, len(states))
	for _, state := range states {
		out = append(out, state.Announcement)
	}
	sort.Slice(out, func(i, j int) bool { return bytes.Compare(out[i].ID[:], out[j].ID[:]) < 0 })
	return out, revision, nil
}

// RunDue delivers due occurrences. It stores the operation ID inputs before
// calling the sender and never holds the state lock across authorization or I/O.
func (s *Scheduler) RunDue(ctx context.Context) error {
	if s == nil {
		return errors.New("missing announcement scheduler")
	}
	s.mu.Lock()
	states, revision, err := s.load(ctx)
	s.mu.Unlock()
	if err != nil {
		return err
	}
	for i := range states {
		if err := ctx.Err(); err != nil {
			return err
		}
		state := states[i]
		if !state.Announcement.Enabled {
			continue
		}
		if state.Status == occurrenceSent || state.Status == occurrenceSkipped {
			next, nextErr := NextOccurrence(state.Announcement.Schedule, state.Occurrence.At)
			if errors.Is(nextErr, ErrNoOccurrence) {
				continue
			}
			if nextErr != nil {
				return nextErr
			}
			state.Occurrence, state.Status, state.GroupID, state.Attempt, state.RetryAt = next, occurrenceReady, id.ID{}, 0, 0
			if err := s.replaceOne(ctx, state, revision); err != nil {
				return err
			}
			states, revision, err = s.snapshot(ctx)
			if err != nil {
				return err
			}
		}
		if state.Status == occurrenceNone {
			next, nextErr := NextOccurrence(state.Announcement.Schedule, s.now().UTC())
			if nextErr != nil {
				if errors.Is(nextErr, ErrNoOccurrence) {
					continue
				}
				return nextErr
			}
			state.Occurrence, state.Status, state.Attempt, state.RetryAt = next, occurrenceReady, 0, 0
			if err := s.replaceOne(ctx, state, revision); err != nil {
				return err
			}
			states, revision, err = s.snapshot(ctx)
			if err != nil {
				return err
			}
		}
		decision := DecideMissed(state.Occurrence.At, s.now().UTC())
		if decision == WaitOccurrence {
			continue
		}
		if decision == SkipOccurrence {
			wasSending := state.Status == occurrenceSending
			if state.Status == occurrenceSending {
				if state.GroupID.IsZero() {
					groupID, resolveErr := s.destination.ResolveTextGroup(ctx, s.binding.Community, state.Announcement.DestinationID)
					if resolveErr != nil {
						return fmt.Errorf("cannot resolve MLS group for legacy announcement receipt recovery: %w", resolveErr)
					}
					if groupID.IsZero() {
						return errors.New("cannot resolve MLS group for legacy announcement receipt recovery: destination has no MLS group")
					}
					state.GroupID = groupID
					if err := s.replaceOne(ctx, state, revision); err != nil {
						return err
					}
					_, revision, err = s.snapshot(ctx)
					if err != nil {
						return err
					}
				}
				req, err := s.sendRequest(state)
				if err != nil {
					return err
				}
				receipt, found, lookupErr := s.receiptLookup.LookupAnnouncement(ctx, req)
				if lookupErr != nil {
					return lookupErr
				}
				if found {
					if validClosedReceipt(receipt, req) {
						state.Status = occurrenceSkipped
						if err := s.replaceOne(ctx, state, revision); err != nil {
							return err
						}
						return nil
					}
					if !validAcceptedReceipt(receipt, req) {
						return errors.New("announcement receipt lookup returned an invalid receipt")
					}
					state.Status = occurrenceSent
				}
			}
			if !wasSending || state.Status != occurrenceSent {
				state.Status = occurrenceSkipped
			}
			if err := s.replaceOne(ctx, state, revision); err != nil {
				return err
			}
			states, revision, err = s.snapshot(ctx)
			if err != nil {
				return err
			}
			continue
		}
		if state.Status == occurrenceSending && retryNotBefore(state.RetryAt, s.now().UTC()) {
			continue
		}
		if state.Status == occurrenceReady {
			state.Status = occurrenceSending
			if err := s.replaceOne(ctx, state, revision); err != nil {
				return err
			}
			states, revision, err = s.snapshot(ctx)
			if err != nil {
				return err
			}
		}
		// A concurrent edit may have replaced this occurrence while it was being
		// prepared. Match both schedule revision and occurrence key before sending.
		current, ok := stateByID(states, state.Announcement.ID)
		if !ok || current.Announcement.Schedule.Revision != state.Announcement.Schedule.Revision || current.Occurrence.Key != state.Occurrence.Key || current.Status != occurrenceSending {
			continue
		}
		if state.GroupID.IsZero() {
			groupID, resolveErr := s.destination.ResolveTextGroup(ctx, s.binding.Community, state.Announcement.DestinationID)
			if resolveErr != nil {
				return fmt.Errorf("cannot resolve MLS group for legacy announcement receipt recovery: %w", resolveErr)
			}
			if groupID.IsZero() {
				return errors.New("cannot resolve MLS group for legacy announcement receipt recovery: destination has no MLS group")
			}
			state.GroupID = groupID
			if err := s.replaceOne(ctx, state, revision); err != nil {
				return err
			}
			states, revision, err = s.snapshot(ctx)
			if err != nil {
				return err
			}
			current, ok = stateByID(states, state.Announcement.ID)
			if !ok || current.Announcement.Schedule.Revision != state.Announcement.Schedule.Revision || current.Occurrence.Key != state.Occurrence.Key || current.Status != occurrenceSending || current.GroupID != groupID {
				continue
			}
			state = current
		}
		req, err := s.sendRequest(state)
		if err != nil {
			return err
		}
		receipt, found, lookupErr := s.receiptLookup.LookupAnnouncement(ctx, req)
		if lookupErr != nil {
			return lookupErr
		}
		if found {
			if validClosedReceipt(receipt, req) {
				state.Attempt++
				state.RetryAt, err = announcementRetryAt(s.now().UTC())
				if err != nil {
					return err
				}
				if err := s.replaceOne(ctx, state, revision); err != nil {
					return err
				}
				return nil
			}
			if !validAcceptedReceipt(receipt, req) {
				return errors.New("announcement receipt lookup returned an invalid receipt")
			}
			state.Status = occurrenceSent
			if err := s.replaceOne(ctx, state, revision); err != nil {
				return err
			}
			states, revision, err = s.snapshot(ctx)
			if err != nil {
				return err
			}
			continue
		}
		groupID, err := s.destination.ResolveTextGroup(ctx, s.binding.Community, state.Announcement.DestinationID)
		if err != nil {
			return err
		}
		if groupID.IsZero() {
			return errors.New("announcement destination has no MLS group")
		}
		if !state.GroupID.IsZero() && state.GroupID != groupID {
			return errors.New("announcement destination MLS group changed")
		}
		allowed, err := s.authorization.CanAnnounce(ctx, state.Announcement.DestinationID)
		if err != nil {
			return err
		}
		if !allowed {
			// Leave the durable claim unresolved; a subsequent retry rechecks current
			// policy and the same idempotency key before any send can be accepted.
			continue
		}
		if state.GroupID.IsZero() {
			state.GroupID = groupID
			if err := s.replaceOne(ctx, state, revision); err != nil {
				return err
			}
			states, revision, err = s.snapshot(ctx)
			if err != nil {
				return err
			}
			current, ok = stateByID(states, state.Announcement.ID)
			if !ok || current.Announcement.Schedule.Revision != state.Announcement.Schedule.Revision || current.Occurrence.Key != state.Occurrence.Key || current.Status != occurrenceSending || current.GroupID != groupID {
				continue
			}
			state = current
		}
		req, err = s.sendRequest(state)
		if err != nil {
			return err
		}
		gate := make(chan struct {
			allowed  bool
			revision uint64
			err      error
		}, 1)
		req.BeforeSend = func() bool {
			latestGroup, destinationErr := s.destination.ResolveTextGroup(ctx, s.binding.Community, state.Announcement.DestinationID)
			if destinationErr == nil && latestGroup.IsZero() {
				destinationErr = errors.New("announcement destination has no MLS group")
			}
			if destinationErr == nil && latestGroup != req.GroupID {
				destinationErr = errors.New("announcement destination MLS group changed")
			}
			latestAllowed := false
			if destinationErr == nil {
				var authorizationErr error
				latestAllowed, authorizationErr = s.authorization.CanAnnounce(ctx, state.Announcement.DestinationID)
				destinationErr = authorizationErr
			}
			// This locked check is the send-start linearization point relative to
			// Put: an edit either commits first and invalidates this occurrence, or
			// follows an occurrence that has already started.
			s.mu.Lock()
			latest, latestRevision, gateErr := s.load(ctx)
			latestState, exists := stateByID(latest, state.Announcement.ID)
			allowedToStart := destinationErr == nil && latestAllowed && gateErr == nil && exists && latestState.Announcement.Schedule.Revision == state.Announcement.Schedule.Revision && latestState.Occurrence.Key == state.Occurrence.Key && latestState.Status == occurrenceSending && latestState.GroupID == req.GroupID && DecideMissed(state.Occurrence.At, s.now().UTC()) == SendOccurrence
			s.mu.Unlock()
			gate <- struct {
				allowed  bool
				revision uint64
				err      error
			}{allowed: allowedToStart, revision: latestRevision, err: errors.Join(destinationErr, gateErr)}
			return allowedToStart
		}
		receipt, sendErr := s.sender.SendAnnouncement(ctx, req)
		var start struct {
			allowed  bool
			revision uint64
			err      error
		}
		select {
		case start = <-gate:
		case <-ctx.Done():
			return ctx.Err()
		default:
			if sendErr != nil {
				return sendErr
			}
			return errors.New("announcement sender returned without invoking start gate")
		}
		if start.err != nil {
			return start.err
		}
		if !start.allowed {
			if sendErr != nil && !errors.Is(sendErr, ErrSendNotStarted) {
				return sendErr
			}
			if err = s.resolveUnstarted(ctx, state, start.revision, req); err != nil {
				return err
			}
			states, revision, err = s.snapshot(ctx)
			if err != nil {
				return err
			}
			continue
		}
		if sendErr != nil {
			return sendErr
		}
		if validClosedReceipt(receipt, req) {
			state.Attempt++
			state.RetryAt, err = announcementRetryAt(s.now().UTC())
			if err != nil {
				return err
			}
			if err = s.replaceOne(ctx, state, start.revision); err != nil {
				return err
			}
			return nil
		}
		if !validAcceptedReceipt(receipt, req) {
			return errors.New("announcement send has no authoritative accepted receipt")
		}
		current.Status = occurrenceSent
		if err = s.replaceOne(ctx, current, start.revision); err != nil {
			if errors.Is(err, ErrSchedulerConflict) {
				continue
			}
			return err
		}
		states, revision, err = s.snapshot(ctx)
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *Scheduler) resolveUnstarted(ctx context.Context, state scheduledState, revision uint64, req SendRequest) error {
	state.Status = occurrenceSkipped
	receipt, found, err := s.receiptLookup.LookupAnnouncement(ctx, req)
	if err != nil {
		return err
	}
	if found {
		if !validAcceptedReceipt(receipt, req) {
			return errors.New("announcement receipt lookup returned an invalid receipt")
		}
		state.Status = occurrenceSent
	}
	err = s.replaceOne(ctx, state, revision)
	if errors.Is(err, ErrSchedulerConflict) {
		return nil
	}
	return err
}

func (s *Scheduler) sendRequest(state scheduledState) (SendRequest, error) {
	seconds := state.Occurrence.At.Unix()
	if seconds < 0 {
		return SendRequest{}, ErrInvalidSchedule
	}
	request := SendRequest{OperationID: operationID(s.binding.Installation, state.Announcement.ID, state.Occurrence), AttemptID: attemptID(s.binding.Installation, state.Announcement.ID, state.Occurrence, state.Attempt), CommunityID: s.binding.Community, InstallationID: s.binding.Installation, AnnouncementID: state.Announcement.ID, GroupID: state.GroupID, Text: state.Announcement.Text, OriginalNow: uint64(seconds)}
	if state.Attempt > 0 {
		previous := attemptID(s.binding.Installation, state.Announcement.ID, state.Occurrence, state.Attempt-1)
		request.PredecessorAttemptID = &previous
	}
	return request, nil
}

func (s *Scheduler) snapshot(ctx context.Context) ([]scheduledState, uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.load(ctx)
}

func (s *Scheduler) replaceOne(ctx context.Context, state scheduledState, revision uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	states, actual, err := s.load(ctx)
	if err != nil {
		return err
	}
	if actual != revision {
		return ErrSchedulerConflict
	}
	i := findAnnouncement(states, state.Announcement.ID)
	if i < 0 || states[i].Announcement.Schedule.Revision != state.Announcement.Schedule.Revision {
		return ErrSchedulerConflict
	}
	states[i] = state
	return s.save(ctx, revision, states, s.receipts)
}

func (s *Scheduler) load(ctx context.Context) ([]scheduledState, uint64, error) {
	v, err := s.native.StateGet(ctx, schedulerStateArea, s.key())
	if err != nil {
		return nil, 0, err
	}
	if v.Tombstone {
		return nil, v.Revision, ErrSchedulerMalformed
	}
	if v.Revision == 0 {
		if len(v.Value) != 0 {
			return nil, 0, ErrSchedulerMalformed
		}
		return nil, 0, nil
	}
	if v.Revision > math.MaxInt64 || len(v.Value) == 0 || len(v.Value) > maxAnnouncementState {
		return nil, 0, ErrSchedulerMalformed
	}
	states, receipts, err := decodeStateDocument(v.Value)
	if err != nil {
		return nil, 0, err
	}
	s.receipts = receipts
	return states, v.Revision, nil
}

func (s *Scheduler) save(ctx context.Context, revision uint64, states []scheduledState, receipts []mutationReceipt) error {
	if revision >= math.MaxInt64 {
		return errors.New("announcement state revision exhausted")
	}
	body, err := encodeStateDocument(states, receipts)
	if err != nil {
		return err
	}
	key := s.key()
	// State transaction IDs must be fresh per save call. The native layer replays
	// a prior transaction receipt before checking the current revision, so a
	// deterministic ID could make a stale save look successful. Send idempotency
	// is carried separately by the occurrence-derived operation and attempt IDs.
	tx := id.New()
	revisions, txErr := s.native.StateTransaction(ctx, tx, []native.StateChange{{Area: schedulerStateArea, Key: key, ExpectedRevision: revision, Value: body}})
	if txErr == nil && len(revisions) == 1 && revisions[0] == revision+1 {
		s.receipts = append([]mutationReceipt(nil), receipts...)
		return nil
	}
	actual, readErr := s.native.StateGet(ctx, schedulerStateArea, key)
	if readErr == nil && !actual.Tombstone && actual.Revision == revision+1 && bytes.Equal(actual.Value, body) {
		s.receipts = append([]mutationReceipt(nil), receipts...)
		return nil
	}
	if readErr != nil {
		return errors.Join(txErr, fmt.Errorf("announcement state readback: %w", readErr))
	}
	if actual.Revision != revision {
		return errors.Join(ErrSchedulerConflict, txErr)
	}
	return txErr
}

func (s *Scheduler) key() []byte { return []byte(schedulerStateKey + s.binding.Community.String()) }

func validateAnnouncement(a Announcement) error {
	if a.ID.IsZero() || a.DestinationID.IsZero() || len(a.Text) == 0 || len(a.Text) > maxAnnouncementText || !utf8.ValidString(a.Text) || strings.TrimSpace(a.Text) == "" {
		return errors.New("invalid announcement")
	}
	if a.Schedule.Revision == 0 {
		return errors.New("announcement revision must be positive")
	}
	_, err := NextOccurrence(a.Schedule, time.Unix(0, 0))
	if errors.Is(err, ErrNoOccurrence) {
		return nil
	}
	return err
}

func findAnnouncement(states []scheduledState, id id.ID) int {
	for i := range states {
		if states[i].Announcement.ID == id {
			return i
		}
	}
	return -1
}
func stateByID(states []scheduledState, id id.ID) (scheduledState, bool) {
	i := findAnnouncement(states, id)
	if i < 0 {
		return scheduledState{}, false
	}
	return states[i], true
}

func operationID(installation, announcement id.ID, occurrence Occurrence) id.ID {
	h := sha256.Sum256(append(append(append([]byte("dilla.bot.announcement.operation.v1\x00"), installation[:]...), announcement[:]...), []byte(occurrence.Key)...))
	return nonzeroID(h[:16])
}
func attemptID(installation, announcement id.ID, occurrence Occurrence, sequence ...uint64) id.ID {
	op := operationID(installation, announcement, occurrence)
	h := sha256.Sum256(append([]byte("dilla.bot.announcement.attempt.v1\x00"), op[:]...))
	if len(sequence) > 0 && sequence[0] > 0 {
		var counter [8]byte
		binary.BigEndian.PutUint64(counter[:], sequence[0])
		h = sha256.Sum256(append(append([]byte("dilla.bot.announcement.attempt.retry.v1\x00"), op[:]...), counter[:]...))
	}
	return nonzeroID(h[:16])
}
func announcementRetryAt(now time.Time) (uint64, error) {
	seconds := now.UTC().Unix()
	if seconds < 0 || seconds > math.MaxInt64-5 {
		return 0, ErrInvalidRetryTime
	}
	return uint64(seconds + 5), nil
}

func retryNotBefore(retryAt uint64, now time.Time) bool {
	seconds := now.UTC().Unix()
	if seconds < 0 {
		return retryAt > 0
	}
	return retryAt > uint64(seconds)
}

func nonzeroID(b []byte) id.ID {
	var out id.ID
	copy(out[:], b)
	if out.IsZero() {
		out[15] = 1
	}
	return out
}
func validAcceptedReceipt(r protocol.SendReceipt, req SendRequest) bool {
	if _, err := protocol.EncodeReceipt(r); err != nil {
		return false
	}
	return r.Outcome == protocol.ReceiptAccepted && r.OperationID == req.OperationID && r.AttemptID == req.AttemptID && r.CommunityID == req.CommunityID && r.GroupID == req.GroupID && r.SendResponse != nil && protocol.ValidateReceiptContext(r, req.CommunityID, req.GroupID) == nil
}
func validClosedReceipt(r protocol.SendReceipt, req SendRequest) bool {
	if _, err := protocol.EncodeReceipt(r); err != nil {
		return false
	}
	return r.Outcome == protocol.ReceiptClosedNotAccepted && r.OperationID == req.OperationID && r.AttemptID == req.AttemptID && r.CommunityID == req.CommunityID && r.GroupID == req.GroupID && protocol.ValidateReceiptContext(r, req.CommunityID, req.GroupID) == nil
}

func encodeStates(states []scheduledState) ([]byte, error) {
	return encodeStateDocument(states, nil)
}

func encodeStateDocument(states []scheduledState, receipts []mutationReceipt) ([]byte, error) {
	if len(states) > maxAnnouncements {
		return nil, errors.New("announcement schedule limit reached")
	}
	if len(receipts) > maxMutationReceipts {
		return nil, ErrMutationReceiptLimit
	}
	a := make([]any, 0, len(states))
	for _, s := range states {
		if err := validateAnnouncement(s.Announcement); err != nil {
			return nil, err
		}
		sched := s.Announcement.Schedule
		occ := []any{uint64(0)}
		if s.Occurrence.Key != "" {
			occ = []any{uint64(1), s.Occurrence.At.UTC().Unix(), s.Occurrence.LocalDate, s.Occurrence.Revision, s.Occurrence.Key}
		}
		enabled := uint64(0)
		if s.Announcement.Enabled {
			enabled = 1
		}
		row := []any{uint64(1), s.Announcement.ID, s.Announcement.DestinationID, s.Announcement.Text, string(sched.Kind), sched.Timezone, sched.Date, uint64(max(sched.Hour, 0)), uint64(max(sched.Minute, 0)), uint64(max(int(sched.Weekday), 0)), sched.Revision, enabled, occ, uint64(s.Status), s.GroupID, s.Attempt, s.RetryAt}
		a = append(a, row)
	}
	encodedReceipts := make([]any, 0, len(receipts))
	seenReceipts := make(map[id.ID]bool, len(receipts))
	for _, receipt := range receipts {
		if receipt.RequestID.IsZero() || seenReceipts[receipt.RequestID] || receipt.Expires == 0 || receipt.Expires > math.MaxInt64 {
			return nil, ErrSchedulerMalformed
		}
		seenReceipts[receipt.RequestID] = true
		var resultRevision any
		if receipt.Revision != nil {
			if *receipt.Revision == 0 || *receipt.Revision > math.MaxInt64 {
				return nil, ErrSchedulerMalformed
			}
			resultRevision = *receipt.Revision
		}
		if receipt.Status > 9 || len(receipt.Data) > 2400 || len(receipt.Data) > 0 && cborx.ExpectMajor(receipt.Data, cborx.MajorArray) != nil {
			return nil, ErrSchedulerMalformed
		}
		encodedReceipts = append(encodedReceipts, []any{receipt.RequestID, receipt.Fingerprint[:], receipt.Expires, receipt.Status, resultRevision, receipt.Data})
	}
	b, err := cborx.Marshal([]any{uint64(2), a, encodedReceipts})
	if err != nil {
		return nil, err
	}
	if len(b) > maxAnnouncementState {
		return nil, errors.New("announcement scheduler state exceeds size limit")
	}
	return b, nil
}

func decodeStates(data []byte) ([]scheduledState, error) {
	states, _, err := decodeStateDocument(data)
	return states, err
}

func decodeStateDocument(data []byte) ([]scheduledState, []mutationReceipt, error) {
	var root []any
	if cborx.Unmarshal(data, &root) != nil || len(root) < 2 || root[0] != uint64(1) && root[0] != uint64(2) {
		return nil, nil, ErrSchedulerMalformed
	}
	rows, ok := root[1].([]any)
	if !ok || len(rows) > maxAnnouncements {
		return nil, nil, ErrSchedulerMalformed
	}
	var receipts []mutationReceipt
	if root[0] == uint64(1) {
		if len(root) != 2 {
			return nil, nil, ErrSchedulerMalformed
		}
	} else {
		if len(root) != 3 {
			return nil, nil, ErrSchedulerMalformed
		}
		encodedReceipts, receiptsOK := root[2].([]any)
		if !receiptsOK || len(encodedReceipts) > maxMutationReceipts {
			return nil, nil, ErrSchedulerMalformed
		}
		receipts = make([]mutationReceipt, 0, len(encodedReceipts))
		seenReceipts := make(map[id.ID]bool, len(encodedReceipts))
		for _, rawReceipt := range encodedReceipts {
			fields, fieldsOK := rawReceipt.([]any)
			requestBytes, requestOK := []byte(nil), false
			fingerprintBytes, fingerprintOK := []byte(nil), false
			var expires uint64
			var expiresOK bool
			var status uint64
			var statusOK bool
			if fieldsOK && len(fields) == 6 {
				requestBytes, requestOK = fields[0].([]byte)
				fingerprintBytes, fingerprintOK = fields[1].([]byte)
				expires, expiresOK = fields[2].(uint64)
				status, statusOK = fields[3].(uint64)
			}
			if !fieldsOK || len(fields) != 6 || !requestOK || len(requestBytes) != 16 || !fingerprintOK || len(fingerprintBytes) != 32 || !expiresOK || expires == 0 || expires > math.MaxInt64 || !statusOK || status > 9 {
				return nil, nil, ErrSchedulerMalformed
			}
			var receipt mutationReceipt
			copy(receipt.RequestID[:], requestBytes)
			copy(receipt.Fingerprint[:], fingerprintBytes)
			receipt.Expires = expires
			receipt.Status = status
			if receipt.RequestID.IsZero() || seenReceipts[receipt.RequestID] {
				return nil, nil, ErrSchedulerMalformed
			}
			seenReceipts[receipt.RequestID] = true
			if fields[4] != nil {
				revision, revisionOK := fields[4].(uint64)
				if !revisionOK || revision == 0 || revision > math.MaxInt64 {
					return nil, nil, ErrSchedulerMalformed
				}
				receipt.Revision = cloneUint64(&revision)
			}
			data, dataOK := fields[5].([]byte)
			if !dataOK || len(data) > 2400 || len(data) > 0 && cborx.ExpectMajor(data, cborx.MajorArray) != nil {
				return nil, nil, ErrSchedulerMalformed
			}
			receipt.Data = append([]byte(nil), data...)
			receipts = append(receipts, receipt)
		}
	}
	out := make([]scheduledState, 0, len(rows))
	seen := map[id.ID]bool{}
	for _, raw := range rows {
		a, ok := raw.([]any)
		if !ok || (len(a) != 14 && len(a) != 15 && len(a) != 16 && len(a) != 17) || a[0] != uint64(1) {
			return nil, nil, ErrSchedulerMalformed
		}
		idb, iok := a[1].([]byte)
		destb, dok := a[2].([]byte)
		txt, tok := a[3].(string)
		kind, kok := a[4].(string)
		zone, zok := a[5].(string)
		date, daok := a[6].(string)
		hour, hok := a[7].(uint64)
		minute, mok := a[8].(uint64)
		weekday, wok := a[9].(uint64)
		rev, rok := a[10].(uint64)
		enabledValue, eok := a[11].(uint64)
		rawOcc, ook := a[12].([]any)
		status, sok := a[13].(uint64)
		var groupID id.ID
		var attempt uint64
		if len(a) >= 15 {
			groupBytes, groupOK := a[14].([]byte)
			if !groupOK || len(groupBytes) != 16 {
				return nil, nil, ErrSchedulerMalformed
			}
			copy(groupID[:], groupBytes)
		}
		if len(a) >= 16 {
			var attemptOK bool
			attempt, attemptOK = a[15].(uint64)
			if !attemptOK || attempt > math.MaxInt64 {
				return nil, nil, ErrSchedulerMalformed
			}
		}
		var retryAt uint64
		if len(a) == 17 {
			var retryOK bool
			retryAt, retryOK = a[16].(uint64)
			if !retryOK || retryAt > math.MaxInt64 {
				return nil, nil, ErrSchedulerMalformed
			}
		}
		if !iok || len(idb) != 16 || !dok || len(destb) != 16 || !tok || !kok || !zok || !daok || !hok || hour > 23 || !mok || minute > 59 || !wok || weekday > 6 || !rok || !eok || enabledValue > 1 || !ook || !sok || status > uint64(occurrenceSkipped) {
			return nil, nil, ErrSchedulerMalformed
		}
		var aid, did id.ID
		copy(aid[:], idb)
		copy(did[:], destb)
		if seen[aid] {
			return nil, nil, ErrSchedulerMalformed
		}
		seen[aid] = true
		value := Announcement{ID: aid, DestinationID: did, Text: txt, Schedule: Schedule{Kind: ScheduleKind(kind), Timezone: zone, Date: date, Hour: int(hour), Minute: int(minute), Weekday: time.Weekday(weekday), Revision: rev}, Enabled: enabledValue == 1}
		if err := validateAnnouncement(value); err != nil {
			return nil, nil, ErrSchedulerMalformed
		}
		state := scheduledState{Announcement: value, Status: occurrenceStatus(status), GroupID: groupID, Attempt: attempt, RetryAt: retryAt}
		if len(rawOcc) == 1 && rawOcc[0] == uint64(0) {
			if state.Status != occurrenceNone {
				return nil, nil, ErrSchedulerMalformed
			}
		} else {
			if len(rawOcc) != 5 || rawOcc[0] != uint64(1) {
				return nil, nil, ErrSchedulerMalformed
			}
			ts, tok := rawOcc[1].(uint64)
			local, lok := rawOcc[2].(string)
			oRev, orok := rawOcc[3].(uint64)
			key, kok := rawOcc[4].(string)
			if !tok || !lok || !orok || !kok || oRev != rev || key == "" || (state.Status == occurrenceNone) {
				return nil, nil, ErrSchedulerMalformed
			}
			if ts > math.MaxInt64 {
				return nil, nil, ErrSchedulerMalformed
			}
			state.Occurrence = Occurrence{At: time.Unix(int64(ts), 0).UTC(), LocalDate: local, Revision: oRev, Key: key}
		}
		out = append(out, state)
	}
	return out, receipts, nil
}
