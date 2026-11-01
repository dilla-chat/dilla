package service

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"time"
	"unicode/utf8"

	"github.com/jonasthim/dilla/internal/bot/client"
	"github.com/jonasthim/dilla/internal/bot/native"
	"github.com/jonasthim/dilla/internal/bot/protocol"
	"github.com/jonasthim/dilla/internal/cborx"
	"github.com/jonasthim/dilla/internal/id"
)

const (
	interactionResponseOutboxArea    = uint64(3)
	interactionResponseOutboxVersion = uint64(2)
	interactionResponseOutboxPrefix  = "dilla.bot.interaction-response.v1/"
	maxInteractionResponseBody       = 4000
)

var (
	ErrInteractionResponseConflict = errors.New("interaction event already has a different request identity")
	ErrInteractionResponseState    = errors.New("interaction response outbox state is invalid or changed")
)

// InteractionResponseNative is the durable native state boundary for private
// interaction replies.
type InteractionResponseNative interface {
	Binding() native.Binding
	StateGet(context.Context, uint64, []byte) (native.StateValue, error)
	StateTransaction(context.Context, id.ID, []native.StateChange) ([]uint64, error)
}

type interactionResponseOutboxNative = InteractionResponseNative

// InteractionResponseOutbox durably records an event's exact response body and
// send identity before invoking the send coordinator. Callers must still make
// handler effects idempotent: a crash after a handler's side effect but before
// this record is committed can cause that handler to run again on redelivery.
type InteractionResponseOutbox struct {
	native               interactionResponseOutboxNative
	binding              native.Binding
	sendFn               func(context.Context, SendInput) (protocol.SendReceipt, error)
	resumeFn             func(context.Context, SendInput, func(context.Context) error) (protocol.SendReceipt, error)
	successorCoordinates func(context.Context) (uint64, uint64, error)
}

// NewInteractionResponseOutbox wires the durable response outbox to the
// existing native state store and SendCoordinator.
func NewInteractionResponseOutbox(n *native.Client, coordinator *SendCoordinator, session *client.Session) (*InteractionResponseOutbox, error) {
	return newInteractionResponseOutboxWithCoordinator(n, coordinator, session)
}

// NewInteractionResponseOutboxWithAdapters wires the production outbox and
// coordinator around testable native and transport boundaries.
func NewInteractionResponseOutboxWithAdapters(n InteractionResponseNative, coordinator *SendCoordinator, session SendSession) (*InteractionResponseOutbox, error) {
	return newInteractionResponseOutboxWithCoordinator(n, coordinator, session)
}

func newInteractionResponseOutboxWithCoordinator(n interactionResponseOutboxNative, coordinator *SendCoordinator, session SendSession) (*InteractionResponseOutbox, error) {
	if n == nil || coordinator == nil || session == nil {
		return nil, errors.New("missing interaction response outbox dependency")
	}
	if n.Binding() != coordinator.binding {
		return nil, errors.New("interaction response outbox and send coordinator bindings differ")
	}
	o, err := newInteractionResponseOutbox(n, nil)
	if err != nil {
		return nil, err
	}
	o.successorCoordinates = func(ctx context.Context) (uint64, uint64, error) {
		discovery, _, err := coordinator.authority.DiscoverObserved(ctx)
		if err != nil {
			return 0, 0, err
		}
		now := time.Now().Unix()
		if discovery.InstanceID != coordinator.binding.Instance || discovery.Generation == 0 || discovery.Generation > math.MaxInt64 || now <= 0 {
			return 0, 0, client.ErrInstanceChanged
		}
		return uint64(now), discovery.Generation, nil
	}
	o.sendFn = func(ctx context.Context, in SendInput) (protocol.SendReceipt, error) {
		return coordinator.send(ctx, session, in)
	}
	o.resumeFn = func(ctx context.Context, in SendInput, preflight func(context.Context) error) (protocol.SendReceipt, error) {
		return coordinator.sendWithPreflight(ctx, session, in, preflight)
	}
	return o, nil
}

func newInteractionResponseOutbox(n interactionResponseOutboxNative, send func(context.Context, SendInput) (protocol.SendReceipt, error)) (*InteractionResponseOutbox, error) {
	if n == nil {
		return nil, errors.New("missing interaction response outbox state")
	}
	b := n.Binding()
	if b.Instance.IsZero() || b.Community.IsZero() || b.Installation.IsZero() {
		return nil, errors.New("invalid interaction response outbox binding")
	}
	o := &InteractionResponseOutbox{native: n, binding: b, sendFn: send}
	o.resumeFn = func(ctx context.Context, in SendInput, preflight func(context.Context) error) (protocol.SendReceipt, error) {
		if preflight != nil {
			if err := preflight(ctx); err != nil {
				return protocol.SendReceipt{}, err
			}
		}
		return send(ctx, in)
	}
	return o, nil
}

type interactionResponseRecord struct {
	EventFingerprint     [32]byte
	RequestFingerprint   [32]byte
	Group                id.ID
	ResponseBody         []byte
	OperationID          id.ID
	AttemptID            id.ID
	PredecessorAttemptID *id.ID
	OriginalNow          uint64
	ExpectedGeneration   uint64
}

// InteractionResponse is a copy of the durable response and its current durable
// send coordinates. It lets an event consumer resume delivery without re-running a
// handler after the response record has been committed.
type InteractionResponse struct {
	Body                 []byte
	OperationID          id.ID
	AttemptID            id.ID
	PredecessorAttemptID *id.ID
	Group                id.ID
	OriginalNow          uint64
	ExpectedGeneration   uint64
}

// Send persists a response before sending it. If the event already has a
// matching request identity, the previously persisted response and send IDs
// are used verbatim, even if this invocation generated a different candidate
// response. Reuse of the event delivery ID for different event/request content
// fails closed.
func (o *InteractionResponseOutbox) Send(ctx context.Context, event native.GroupEvent, responseBody []byte, operationID, attemptID id.ID, originalNow, expectedGeneration uint64) (protocol.SendReceipt, error) {
	if o == nil || ctx == nil || o.sendFn == nil || event.DeliveryID == 0 || event.Community != o.binding.Community || event.Group.IsZero() || event.Channel.IsZero() || event.SenderUser.IsZero() || event.SenderDevice.IsZero() || event.Body == nil || operationID.IsZero() || attemptID.IsZero() || len(responseBody) == 0 || len(responseBody) > maxInteractionResponseBody || !utf8.Valid(responseBody) || originalNow > math.MaxInt64 || expectedGeneration == 0 || expectedGeneration > math.MaxInt64 {
		return protocol.SendReceipt{}, errors.New("invalid interaction response outbox input")
	}
	request, err := protocol.DecodeRequest(*event.Body)
	if err != nil {
		return protocol.SendReceipt{}, err
	}
	canonicalRequest, err := protocol.EncodeRequest(request)
	if err != nil {
		return protocol.SendReceipt{}, err
	}
	requestFingerprint := sha256.Sum256([]byte(canonicalRequest))
	eventFingerprint, err := interactionResponseEventFingerprint(event)
	if err != nil {
		return protocol.SendReceipt{}, err
	}
	key := interactionResponseOutboxKey(o.binding.Installation, event.DeliveryID)
	record, err := o.loadOrCreate(ctx, key, interactionResponseRecord{
		EventFingerprint: eventFingerprint, RequestFingerprint: requestFingerprint,
		Group: event.Group, ResponseBody: append([]byte(nil), responseBody...),
		OperationID: operationID, AttemptID: attemptID, OriginalNow: originalNow,
		ExpectedGeneration: expectedGeneration,
	})
	if err != nil {
		return protocol.SendReceipt{}, err
	}
	return o.sendRecord(ctx, event, record, nil)
}

// Resume sends the saved response for an event, or returns found=false when
// no response has been committed yet. A found record is identity-checked
// against the redelivered event before any send occurs.
func (o *InteractionResponseOutbox) Resume(ctx context.Context, event native.GroupEvent) (receipt protocol.SendReceipt, found bool, err error) {
	return o.ResumeWithPreflight(ctx, event, nil)
}

// ResumeWithPreflight resumes an immutable response. The preflight is passed
// into the send coordinator, which checks for an already accepted server
// receipt first and calls the preflight only when the saved attempt remains
// unaccepted and may still be sent.
func (o *InteractionResponseOutbox) ResumeWithPreflight(ctx context.Context, event native.GroupEvent, preflight func(context.Context) error) (receipt protocol.SendReceipt, found bool, err error) {
	if o == nil || ctx == nil || o.sendFn == nil {
		return protocol.SendReceipt{}, false, errors.New("invalid interaction response outbox")
	}
	record, found, err := o.lookup(ctx, event)
	if err != nil || !found {
		return protocol.SendReceipt{}, found, err
	}
	receipt, err = o.sendRecord(ctx, event, record, preflight)
	return receipt, true, err
}

// A closed receipt proves this exact ciphertext was not accepted. Only then may
// the saved response move to a durable, explicitly linked successor attempt.
func (o *InteractionResponseOutbox) sendRecord(ctx context.Context, event native.GroupEvent, record interactionResponseRecord, preflight func(context.Context) error) (protocol.SendReceipt, error) {
	input := func(r interactionResponseRecord) SendInput {
		return SendInput{OperationID: r.OperationID, AttemptID: r.AttemptID, PredecessorAttemptID: cloneID(r.PredecessorAttemptID), GroupID: r.Group, RequestBody: encodeInteractionResponseSendRequest(r.ResponseBody), OriginalNow: r.OriginalNow, ExpectedGeneration: r.ExpectedGeneration}
	}
	receipt, err := o.resumeFn(ctx, input(record), preflight)
	if err != nil || receipt.Outcome != protocol.ReceiptClosedNotAccepted || o.successorCoordinates == nil {
		return receipt, err
	}
	if !receiptMatchesInput(receipt, o.binding.Community, input(record)) {
		return protocol.SendReceipt{}, client.ErrProtocol
	}
	if preflight != nil {
		if err := preflight(ctx); err != nil {
			return protocol.SendReceipt{}, err
		}
	}
	now, generation, err := o.successorCoordinates(ctx)
	if err != nil {
		return protocol.SendReceipt{}, err
	}
	next := record
	next.PredecessorAttemptID = cloneID(&record.AttemptID)
	next.AttemptID = id.New()
	next.OriginalNow = now
	next.ExpectedGeneration = generation
	encoded, err := encodeInteractionResponseRecord(next)
	if err != nil {
		return protocol.SendReceipt{}, err
	}
	key := interactionResponseOutboxKey(o.binding.Installation, event.DeliveryID)
	for tries := 0; tries < 4; tries++ {
		current, readErr := o.native.StateGet(ctx, interactionResponseOutboxArea, key)
		if readErr != nil {
			return protocol.SendReceipt{}, readErr
		}
		actual, decodeErr := decodeInteractionResponseRecord(current.Value)
		if current.Tombstone || decodeErr != nil || actual.EventFingerprint != record.EventFingerprint || actual.RequestFingerprint != record.RequestFingerprint || actual.Group != record.Group || actual.OperationID != record.OperationID || string(actual.ResponseBody) != string(record.ResponseBody) {
			return protocol.SendReceipt{}, ErrInteractionResponseConflict
		}
		if actual.AttemptID != record.AttemptID {
			// A concurrent caller or a lost CAS reply already selected the successor.
			return o.resumeFn(ctx, input(actual), preflight)
		}
		_, txErr := o.native.StateTransaction(ctx, id.New(), []native.StateChange{{Area: interactionResponseOutboxArea, Key: key, ExpectedRevision: current.Revision, Value: encoded}})
		if txErr == nil {
			return o.resumeFn(ctx, input(next), preflight)
		}
	}
	return protocol.SendReceipt{}, ErrInteractionResponseState
}

func encodeInteractionResponseSendRequest(response []byte) []byte {
	if len(response) == 0 || len(response) > maxInteractionResponseBody || !utf8.Valid(response) {
		return nil
	}
	body, err := cborx.Marshal([]any{uint64(0), nil, string(response), []any{}})
	if err != nil {
		return nil
	}
	return body
}

// Lookup returns a previously committed response without sending it.
func (o *InteractionResponseOutbox) Lookup(ctx context.Context, event native.GroupEvent) (InteractionResponse, bool, error) {
	if o == nil || ctx == nil {
		return InteractionResponse{}, false, errors.New("invalid interaction response outbox")
	}
	r, found, err := o.lookup(ctx, event)
	if err != nil || !found {
		return InteractionResponse{}, found, err
	}
	return InteractionResponse{Body: append([]byte(nil), r.ResponseBody...), OperationID: r.OperationID,
		AttemptID: r.AttemptID, PredecessorAttemptID: cloneID(r.PredecessorAttemptID), Group: r.Group, OriginalNow: r.OriginalNow,
		ExpectedGeneration: r.ExpectedGeneration}, true, nil
}

func (o *InteractionResponseOutbox) lookup(ctx context.Context, event native.GroupEvent) (interactionResponseRecord, bool, error) {
	if event.DeliveryID == 0 || event.Community != o.binding.Community || event.Group.IsZero() || event.Channel.IsZero() || event.SenderUser.IsZero() || event.SenderDevice.IsZero() || event.Body == nil {
		return interactionResponseRecord{}, false, errors.New("invalid interaction response event")
	}
	request, err := protocol.DecodeRequest(*event.Body)
	if err != nil {
		return interactionResponseRecord{}, false, err
	}
	canonical, err := protocol.EncodeRequest(request)
	if err != nil {
		return interactionResponseRecord{}, false, err
	}
	requestFingerprint := sha256.Sum256([]byte(canonical))
	eventFingerprint, err := interactionResponseEventFingerprint(event)
	if err != nil {
		return interactionResponseRecord{}, false, err
	}
	current, err := o.native.StateGet(ctx, interactionResponseOutboxArea, interactionResponseOutboxKey(o.binding.Installation, event.DeliveryID))
	if err != nil {
		return interactionResponseRecord{}, false, err
	}
	if current.Tombstone {
		return interactionResponseRecord{}, false, ErrInteractionResponseState
	}
	if current.Revision == 0 {
		return interactionResponseRecord{}, false, nil
	}
	r, err := decodeInteractionResponseRecord(current.Value)
	if err != nil {
		return interactionResponseRecord{}, false, ErrInteractionResponseState
	}
	if r.EventFingerprint != eventFingerprint || r.RequestFingerprint != requestFingerprint || r.Group != event.Group {
		return interactionResponseRecord{}, false, ErrInteractionResponseConflict
	}
	return r, true, nil
}

func (o *InteractionResponseOutbox) loadOrCreate(ctx context.Context, key []byte, candidate interactionResponseRecord) (interactionResponseRecord, error) {
	want, err := encodeInteractionResponseRecord(candidate)
	if err != nil {
		return interactionResponseRecord{}, err
	}
	for tries := 0; tries < 4; tries++ {
		current, err := o.native.StateGet(ctx, interactionResponseOutboxArea, key)
		if err != nil {
			return interactionResponseRecord{}, err
		}
		if current.Tombstone {
			return interactionResponseRecord{}, ErrInteractionResponseState
		}
		if current.Revision != 0 {
			record, decodeErr := decodeInteractionResponseRecord(current.Value)
			if decodeErr != nil {
				return interactionResponseRecord{}, ErrInteractionResponseState
			}
			if record.EventFingerprint != candidate.EventFingerprint || record.RequestFingerprint != candidate.RequestFingerprint || record.Group != candidate.Group {
				return interactionResponseRecord{}, ErrInteractionResponseConflict
			}
			return record, nil
		}
		_, txErr := o.native.StateTransaction(ctx, id.New(), []native.StateChange{{Area: interactionResponseOutboxArea, Key: key, ExpectedRevision: current.Revision, Value: want}})
		if txErr == nil {
			return candidate, nil
		}
		// A transaction error can mean a revision race or a lost reply after
		// commit. Re-read: only a matching durable record authorizes sending.
		actual, readErr := o.native.StateGet(ctx, interactionResponseOutboxArea, key)
		if readErr != nil {
			return interactionResponseRecord{}, errors.Join(txErr, readErr)
		}
		if actual.Revision != 0 && !actual.Tombstone {
			record, decodeErr := decodeInteractionResponseRecord(actual.Value)
			if decodeErr != nil {
				return interactionResponseRecord{}, ErrInteractionResponseState
			}
			if record.EventFingerprint != candidate.EventFingerprint || record.RequestFingerprint != candidate.RequestFingerprint || record.Group != candidate.Group {
				return interactionResponseRecord{}, ErrInteractionResponseConflict
			}
			return record, nil
		}
	}
	return interactionResponseRecord{}, fmt.Errorf("%w: compare-and-set retries exhausted", ErrInteractionResponseState)
}

func interactionResponseOutboxKey(installation id.ID, delivery uint64) []byte {
	key := make([]byte, len(interactionResponseOutboxPrefix)+id.Size+8)
	copy(key, interactionResponseOutboxPrefix)
	copy(key[len(interactionResponseOutboxPrefix):], installation[:])
	binary.BigEndian.PutUint64(key[len(interactionResponseOutboxPrefix)+id.Size:], delivery)
	return key
}

func interactionResponseEventFingerprint(e native.GroupEvent) ([32]byte, error) {
	var body any
	if e.Body != nil {
		body = *e.Body
	}
	var reply, thread any
	if e.ReplyTo != nil {
		reply = *e.ReplyTo
	}
	if e.Thread != nil {
		thread = *e.Thread
	}
	encoded, err := cborx.Marshal([]any{e.DeliveryID, e.Group, e.Community, e.Channel, e.SourceSequence, e.Epoch, e.ReceivedAt,
		e.SenderUser, e.SenderDevice, e.SenderKind, e.SenderTier, uint64(e.SenderLeaf), e.Message, e.EnvelopeType, body, reply, thread})
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(encoded), nil
}

func encodeInteractionResponseRecord(r interactionResponseRecord) ([]byte, error) {
	if r.Group.IsZero() || r.OperationID.IsZero() || r.AttemptID.IsZero() || len(r.ResponseBody) == 0 || len(r.ResponseBody) > maxInteractionResponseBody || r.ExpectedGeneration == 0 || r.ExpectedGeneration > math.MaxInt64 || r.OriginalNow > math.MaxInt64 || r.PredecessorAttemptID != nil && (r.PredecessorAttemptID.IsZero() || *r.PredecessorAttemptID == r.AttemptID) {
		return nil, ErrInteractionResponseState
	}
	fields := []any{uint64(1), r.EventFingerprint[:], r.RequestFingerprint[:], r.Group, append([]byte(nil), r.ResponseBody...), r.OperationID, r.AttemptID, r.OriginalNow, r.ExpectedGeneration}
	if r.PredecessorAttemptID != nil {
		fields[0] = interactionResponseOutboxVersion
		fields = append(fields, *r.PredecessorAttemptID)
	}
	return cborx.Marshal(fields)
}

func decodeInteractionResponseRecord(encoded []byte) (interactionResponseRecord, error) {
	var r interactionResponseRecord
	var n []any
	if err := cborx.Unmarshal(encoded, &n); err != nil || (len(n) != 9 && len(n) != 10) {
		return r, ErrInteractionResponseState
	}
	version, ok := n[0].(uint64)
	if !ok || (version != 1 || len(n) != 9) && (version != interactionResponseOutboxVersion || len(n) != 10) {
		return r, ErrInteractionResponseState
	}
	eventHash, ok := n[1].([]byte)
	if !ok || len(eventHash) != len(r.EventFingerprint) {
		return r, ErrInteractionResponseState
	}
	copy(r.EventFingerprint[:], eventHash)
	requestHash, ok := n[2].([]byte)
	if !ok || len(requestHash) != len(r.RequestFingerprint) {
		return r, ErrInteractionResponseState
	}
	copy(r.RequestFingerprint[:], requestHash)
	if r.Group, ok = outboxID(n[3]); !ok {
		return r, ErrInteractionResponseState
	}
	if r.ResponseBody, ok = n[4].([]byte); !ok {
		return r, ErrInteractionResponseState
	}
	r.ResponseBody = append([]byte(nil), r.ResponseBody...)
	if r.OperationID, ok = outboxID(n[5]); !ok {
		return r, ErrInteractionResponseState
	}
	if r.AttemptID, ok = outboxID(n[6]); !ok {
		return r, ErrInteractionResponseState
	}
	if r.OriginalNow, ok = n[7].(uint64); !ok {
		return r, ErrInteractionResponseState
	}
	if r.ExpectedGeneration, ok = n[8].(uint64); !ok {
		return r, ErrInteractionResponseState
	}
	if version == interactionResponseOutboxVersion {
		previous, valid := outboxID(n[9])
		if !valid {
			return r, ErrInteractionResponseState
		}
		r.PredecessorAttemptID = &previous
	}
	if _, err := encodeInteractionResponseRecord(r); err != nil {
		return r, ErrInteractionResponseState
	}
	return r, nil
}

func outboxID(value any) (id.ID, bool) {
	var result id.ID
	if typed, ok := value.(id.ID); ok {
		return typed, true
	}
	bytes, ok := value.([]byte)
	if !ok || len(bytes) != len(result) {
		return result, false
	}
	copy(result[:], bytes)
	return result, true
}
