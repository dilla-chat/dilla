// Package helper owns the private process boundary to dilla-bot-core.
package helper

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os/exec"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/dilla-chat/dilla/internal/bot/client"
	"github.com/dilla-chat/dilla/internal/bot/protocol"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/prioritygate"
)

var (
	ErrUnsupportedPlatform = errors.New("bot helper is supported only on Linux")
	ErrInvalidOperation    = errors.New("operation is outside the frozen helper API")
	ErrOutcomeUnknown      = errors.New("helper operation outcome is unknown")
	ErrClosed              = errors.New("helper client is closed")
	ErrNotOpened           = errors.New("helper client is not open")
)

// StatusError is a definitive, sanitized helper rejection. Status values are
// the frozen protocol 10 helper statuses 1 through 9.
type StatusError struct{ Status uint64 }

func (e *StatusError) Error() string {
	name := [...]string{"ok", "invalid", "forbidden", "expired", "conflict", "unavailable", "busy", "not_found", "upgrade_required", "internal"}
	if e.Status < uint64(len(name)) {
		return "helper status: " + name[e.Status]
	}
	return "helper status: unknown"
}

// OutcomeUnknownError means the request may have reached the helper, but the
// client did not receive a fully validated response. Resolve it using native
// intent/source progress before any retry of a crypto-producing operation.
type OutcomeUnknownError struct{ cause error }

func (e *OutcomeUnknownError) Error() string        { return ErrOutcomeUnknown.Error() }
func (e *OutcomeUnknownError) Is(target error) bool { return target == ErrOutcomeUnknown }
func (e *OutcomeUnknownError) Unwrap() error        { return e.cause }

type Options struct {
	Executable      string
	StateDir        string
	CallTimeout     time.Duration
	ShutdownTimeout time.Duration
	KillTimeout     time.Duration

	launcher func(*exec.Cmd) error // test seam; production always uses os/exec.
}

type exchange interface {
	request(context.Context, id.ID, protocol.HelperOperation, []any, time.Duration) (protocol.HelperResponse, error)
	stop(time.Duration) error
}

type Client struct {
	mu        sync.Mutex
	gate      *prioritygate.Gate
	transport exchange
	callTime  time.Duration
	stopTime  time.Duration
	closed    bool
	opened    bool
}

// Start launches one native helper process for one community identity. The
// helper receives only its nonsecret state directory on argv; the database key
// is sent later by Open over the inherited private socket.
func Start(options Options) (*Client, error) {
	if options.CallTimeout <= 0 {
		options.CallTimeout = 15 * time.Second
	}
	if options.ShutdownTimeout <= 0 {
		options.ShutdownTimeout = 2 * time.Second
	}
	if options.KillTimeout <= 0 {
		options.KillTimeout = 2 * time.Second
	}
	t, err := launch(options)
	if err != nil {
		return nil, err
	}
	return &Client{gate: prioritygate.New(1), transport: t, callTime: options.CallTimeout, stopTime: options.ShutdownTimeout}, nil
}

// Open binds the helper's encrypted store to the supplied instance,
// community, and installation. databaseKey is passed only in the op1 IPC
// request. Open consumes and best-effort zeroes the supplied slice on every
// return path, so callers that need it later must pass a disposable copy.
func (c *Client) Open(ctx context.Context, instance, community, installation id.ID, databaseKey []byte) ([]any, error) {
	defer clear(databaseKey)
	if len(databaseKey) != 32 || instance.IsZero() || community.IsZero() || installation.IsZero() {
		return nil, errors.New("invalid helper open parameters")
	}
	callCtx, cancel := boundedContext(ctx, c.callTime)
	defer cancel()
	release, err := c.acquire(callCtx)
	if err != nil {
		return nil, err
	}
	defer release()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, ErrClosed
	}
	if c.opened {
		return nil, errors.New("helper client is already open")
	}
	payload := []any{instance, community, installation, databaseKey}
	response, err := c.request(callCtx, protocol.HelperOpen, payload)
	if err != nil {
		return nil, err
	}
	c.opened = true
	return response, nil
}

// Call invokes one non-transfer helper operation.
// Operation 6 is deliberately reserved for GroupApply, which always uses the
// staged transfer path so a large result cannot be lost to a direct frame cap.
func (c *Client) Call(ctx context.Context, operation protocol.HelperOperation, payload []any) ([]any, error) {
	if operation < protocol.HelperBootstrap || operation > protocol.HelperDeviceListIngest ||
		operation == protocol.HelperGroupApply || operation == protocol.HelperShutdown || operation == protocol.HelperGroupTransfer {
		return nil, ErrInvalidOperation
	}
	callCtx, cancel := boundedContext(ctx, c.callTime)
	defer cancel()
	return c.serial(callCtx, func() ([]any, error) { return c.request(callCtx, operation, payload) })
}

// CommandRefusal is why the helper refused a sealed command (op 22). 1-7 are the core's
// CommandRefusal codes; RefusalReplay is the helper's own.
type CommandRefusal uint8

const (
	RefusalNone CommandRefusal = iota
	RefusalMalformed
	RefusalUnknownKey
	RefusalBadSignature
	RefusalWrongDevice
	RefusalDeviceUnlisted
	RefusalDeviceRevoked
	RefusalStale
	RefusalReplay
)

// OpenedCommand is op 22's answer: the verified request when Refusal is RefusalNone.
type OpenedCommand struct {
	Request protocol.SealedRequest
	Refusal CommandRefusal
}

func unixSeconds(now time.Time) (uint64, error) {
	seconds := now.Unix()
	if seconds <= 0 {
		return 0, errors.New("invalid helper time")
	}
	return uint64(seconds), nil
}

// CommandKey returns the signed publication of community's current command key at now
// (op 21). The helper mints the first key and rotates a key that is 7 days old; within the
// week it returns the same bytes.
func (c *Client) CommandKey(ctx context.Context, community id.ID, now time.Time) ([]byte, error) {
	seconds, err := unixSeconds(now)
	if err != nil || community.IsZero() {
		return nil, errors.New("invalid command key request")
	}
	answer, err := c.Call(ctx, protocol.HelperCommandKey, []any{community, seconds})
	if err != nil {
		return nil, err
	}
	publication, ok := oneBytes(answer)
	if !ok || len(publication) == 0 {
		return nil, unknown(errors.New("invalid command key answer"))
	}
	return publication, nil
}

// OpenCommand opens and verifies a sealed command that dillad attached to user's device (op
// 22). A refusal is an answer, not an error: Refusal names it and Request is empty.
func (c *Client) OpenCommand(ctx context.Context, sealed []byte, user, device id.ID, now time.Time) (OpenedCommand, error) {
	seconds, err := unixSeconds(now)
	if err != nil || user.IsZero() || device.IsZero() || len(sealed) == 0 {
		return OpenedCommand{}, errors.New("invalid command open request")
	}
	answer, err := c.Call(ctx, protocol.HelperCommandOpen, []any{sealed, user, device, seconds})
	if err != nil {
		return OpenedCommand{}, err
	}
	code, ok := uint64At(answer, 0)
	switch {
	case ok && len(answer) == 1 && code >= uint64(RefusalMalformed) && code <= uint64(RefusalReplay):
		return OpenedCommand{Refusal: CommandRefusal(code)}, nil
	case ok && len(answer) == 2 && code == 0:
		body, isBytes := answer[1].([]byte)
		request, err := protocol.DecodeSealedRequest(body)
		if !isBytes || err != nil {
			return OpenedCommand{}, unknown(errors.New("invalid opened command"))
		}
		request.UserID, request.DeviceID = user, device
		return OpenedCommand{Request: request}, nil
	}
	return OpenedCommand{}, unknown(errors.New("invalid command open answer"))
}

// SealReply seals answer number seq of requestID to the request's one-time replyKey (op 23).
func (c *Client) SealReply(ctx context.Context, replyKey [32]byte, requestID id.ID, seq uint32, body []byte) ([]byte, error) {
	if requestID.IsZero() || replyKey == [32]byte{} {
		return nil, errors.New("invalid reply seal request")
	}
	if body == nil {
		body = []byte{}
	}
	answer, err := c.Call(ctx, protocol.HelperReplySeal, []any{replyKey[:], requestID, uint64(seq), body})
	if err != nil {
		return nil, err
	}
	sealed, ok := oneBytes(answer)
	if !ok || len(sealed) == 0 {
		return nil, unknown(errors.New("invalid reply seal answer"))
	}
	return sealed, nil
}

// IngestDeviceList hands the helper a device list pushed for user, as the device-list PUT body
// (op 24). It is the only way the helper's device-list cache changes. A list that does not chain
// from the user's cached, trusted list is a StatusError and leaves the cache unchanged.
func (c *Client) IngestDeviceList(ctx context.Context, user id.ID, body []byte) (changed bool, version uint64, err error) {
	if user.IsZero() || len(body) == 0 {
		return false, 0, errors.New("invalid device list ingest request")
	}
	answer, err := c.Call(ctx, protocol.HelperDeviceListIngest, []any{user, body})
	if err != nil {
		return false, 0, err
	}
	flag, okFlag := uint64At(answer, 0)
	version, okVersion := uint64At(answer, 1)
	if len(answer) != 2 || !okFlag || !okVersion || flag > 1 || version == 0 {
		return false, 0, unknown(errors.New("invalid device list ingest answer"))
	}
	return flag == 1, version, nil
}

func oneBytes(answer []any) ([]byte, bool) {
	if len(answer) != 1 {
		return nil, false
	}
	b, ok := answer[0].([]byte)
	return b, ok
}

func uint64At(answer []any, i int) (uint64, bool) {
	if i >= len(answer) {
		return 0, false
	}
	v, ok := answer[i].(uint64)
	return v, ok
}

// GroupApply sends the complete op6 payload array through op16, including for
// small input, because a successful operation may return a result larger than
// a direct envelope. Repeated execute is used only when transport has validated
// no failure; an interrupted execute is never retried by this client.
func (c *Client) GroupApply(ctx context.Context, payload []any) ([]any, error) {
	callCtx, cancel := boundedContext(ctx, c.callTime)
	defer cancel()
	return c.serial(callCtx, func() ([]any, error) { return c.groupApply(callCtx, payload) })
}

func (c *Client) serial(ctx context.Context, fn func() ([]any, error)) ([]any, error) {
	release, err := c.acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, ErrClosed
	}
	if !c.opened {
		return nil, ErrNotOpened
	}
	return fn()
}

func (c *Client) request(ctx context.Context, operation protocol.HelperOperation, payload []any) ([]any, error) {
	var requestID id.ID
	for requestID.IsZero() {
		requestID = id.New()
	}
	if err := validateBounded(payload, protocol.MaxHelperPayload); err != nil {
		return nil, errors.New("invalid helper payload")
	}
	response, err := c.transport.request(ctx, requestID, operation, payload, c.callTime)
	if err != nil {
		if errors.Is(err, ErrOutcomeUnknown) {
			c.abortCurrent()
		}
		return nil, err
	}
	if err := validateStatus(response.Status); err != nil {
		return nil, err
	}
	return response.Payload, nil
}

const (
	maxLogicalTransfer = 32 << 20
	maxTransferChunk   = 768 << 10
)

func (c *Client) groupApply(ctx context.Context, payload []any) ([]any, error) {
	input, err := marshalBounded(payload, maxLogicalTransfer)
	if err != nil || len(input) == 0 {
		return nil, errors.New("invalid or oversized group apply payload")
	}
	defer clear(input)
	digest := sha256.Sum256(input)
	begin, err := c.request(ctx, protocol.HelperGroupTransfer, []any{uint64(0), uint64(protocol.HelperGroupApply), uint64(len(input)), digest[:]})
	if err != nil {
		return nil, err
	}
	if len(begin) != 1 {
		c.abortCurrent()
		return nil, unknown(errors.New("invalid transfer begin response"))
	}
	transferID, ok := begin[0].([]byte)
	if !ok || len(transferID) != 16 || allZero(transferID) {
		c.abortCurrent()
		return nil, unknown(errors.New("invalid transfer id"))
	}
	transferID = append([]byte(nil), transferID...)
	defer clear(transferID)
	defer func() {
		_, _ = c.request(ctx, protocol.HelperGroupTransfer, []any{uint64(4), transferID})
	}()

	for offset := 0; offset < len(input); {
		end := offset + maxTransferChunk
		if end > len(input) {
			end = len(input)
		}
		chunk := input[offset:end]
		ack, err := c.request(ctx, protocol.HelperGroupTransfer, []any{uint64(1), transferID, uint64(offset), chunk})
		if err != nil {
			return nil, err
		}
		if len(ack) != 1 || ack[0] != uint64(end) {
			c.abortCurrent()
			return nil, unknown(errors.New("invalid transfer offset acknowledgement"))
		}
		offset = end
	}

	// Execute is sent exactly once. The native operation may commit before its
	// response is lost; the caller must resolve that through the source intent.
	descriptor, err := c.request(ctx, protocol.HelperGroupTransfer, []any{uint64(2), transferID})
	clear(input)
	if err != nil {
		return nil, err
	}
	if len(descriptor) != 3 {
		c.abortCurrent()
		return nil, unknown(errors.New("invalid transfer result descriptor"))
	}
	status, okStatus := descriptor[0].(uint64)
	length, okLength := descriptor[1].(uint64)
	resultDigest, okDigest := descriptor[2].([]byte)
	if !okStatus || !okLength || !okDigest || len(resultDigest) != sha256.Size || status > 9 || length == 0 || length > maxLogicalTransfer {
		c.abortCurrent()
		return nil, unknown(errors.New("invalid transfer result descriptor"))
	}
	if status != 0 {
		return nil, &StatusError{Status: status}
	}
	result := make([]byte, int(length))
	defer clear(result)
	for offset := 0; offset < len(result); {
		n := maxTransferChunk
		if remain := len(result) - offset; remain < n {
			n = remain
		}
		part, err := c.request(ctx, protocol.HelperGroupTransfer, []any{uint64(3), transferID, intToUint64(offset), intToUint64(n)})
		if err != nil {
			return nil, unknown(err)
		}
		if len(part) != 1 {
			c.abortCurrent()
			return nil, unknown(errors.New("invalid transfer chunk response"))
		}
		chunk, ok := part[0].([]byte)
		if !ok || len(chunk) != n {
			c.abortCurrent()
			return nil, unknown(errors.New("invalid transfer chunk length"))
		}
		copy(result[offset:offset+n], chunk)
		clear(chunk)
		offset += n
	}
	gotDigest := sha256.Sum256(result)
	if !equalDigest(gotDigest[:], resultDigest) {
		c.abortCurrent()
		return nil, unknown(errors.New("transfer result digest mismatch"))
	}
	var values []any
	if err := cborx.Unmarshal(result, &values); err != nil {
		c.abortCurrent()
		return nil, unknown(errors.New("invalid transfer result payload"))
	}
	return values, nil
}

func allZero(b []byte) bool {
	var v byte
	for _, x := range b {
		v |= x
	}
	return v == 0
}
func equalDigest(a, b []byte) bool {
	if len(a) != sha256.Size || len(b) != sha256.Size {
		return false
	}
	var different byte
	for i := range a {
		different |= a[i] ^ b[i]
	}
	return different == 0
}

func unknown(cause error) error { return &OutcomeUnknownError{cause: cause} }

func marshalBounded(values []any, maximum int) ([]byte, error) {
	if values == nil {
		values = []any{}
	}
	size, err := cborSize(values, 0)
	if err != nil || maximum < 0 || size > intToUint64(maximum) {
		return nil, errors.New("CBOR payload exceeds its limit")
	}
	b, err := cborx.Marshal(values)
	if err != nil || len(b) > maximum {
		clear(b)
		return nil, errors.New("invalid CBOR payload")
	}
	return b, nil
}

func validateBounded(values []any, maximum int) error {
	if values == nil {
		values = []any{}
	}
	size, err := cborSize(values, 0)
	if err != nil || maximum < 0 || size > intToUint64(maximum) {
		return errors.New("CBOR payload exceeds its limit")
	}
	return nil
}

func cborSize(value any, depth int) (uint64, error) {
	if depth > cborx.MaxNesting {
		return 0, errors.New("CBOR nesting limit exceeded")
	}
	switch v := value.(type) {
	case uint64:
		return uintHeadLen(v), nil
	case uint:
		return uintHeadLen(uint64(v)), nil
	case uint32:
		return uintHeadLen(uint64(v)), nil
	case uint16:
		return uintHeadLen(uint64(v)), nil
	case uint8:
		return uintHeadLen(uint64(v)), nil
	case int:
		if v < 0 {
			return 0, errors.New("negative CBOR integer")
		}
		return uintHeadLen(intToUint64(v)), nil
	case int64:
		if v < 0 {
			return 0, errors.New("negative CBOR integer")
		}
		return uintHeadLen(uint64(v)), nil
	case id.ID:
		return 17, nil
	case []byte:
		return bytesHeadLen(len(v)) + intToUint64(len(v)), nil
	case string:
		if !utf8.ValidString(v) {
			return 0, errors.New("invalid UTF-8 in CBOR text")
		}
		return bytesHeadLen(len(v)) + intToUint64(len(v)), nil
	case nil:
		return 1, nil
	case []any:
		size := uintHeadLen(intToUint64(len(v)))
		for _, child := range v {
			childSize, err := cborSize(child, depth+1)
			if err != nil {
				return 0, err
			}
			size += childSize
			if size > maxLogicalTransfer {
				return 0, errors.New("CBOR payload exceeds logical transfer limit")
			}
		}
		return size, nil
	default:
		return 0, errors.New("unsupported helper CBOR value")
	}
}

func uintHeadLen(v uint64) uint64 {
	if v < 24 {
		return 1
	}
	if v <= 0xff {
		return 2
	}
	if v <= 0xffff {
		return 3
	}
	if v <= 0xffffffff {
		return 5
	}
	return 9
}
func bytesHeadLen(n int) uint64 { return uintHeadLen(intToUint64(n)) }

func intToUint64(value int) uint64 {
	if value < 0 {
		return 0
	}
	return uint64(value)
}

func (c *Client) acquire(ctx context.Context) (func(), error) {
	if err := client.CooperativeCheckpoint(ctx); err != nil {
		return nil, err
	}
	release, err := c.gate.Acquire(ctx, client.RequestPriority(ctx))
	if errors.Is(err, prioritygate.ErrClosed) {
		err = ErrClosed
	}
	if err == nil {
		if stopped := client.CooperativeCheckpoint(ctx); stopped != nil {
			release()
			return nil, stopped
		}
	}
	return release, err
}

func boundedContext(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	deadline := time.Now().Add(timeout)
	if existing, ok := ctx.Deadline(); ok && existing.Before(deadline) {
		deadline = existing
	}
	return context.WithDeadline(ctx, deadline)
}

// Close asks the helper to shut down, then always closes the inherited socket
// and reaps the child. If the bounded shutdown exchange fails, stop kills it.
func (c *Client) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), c.stopTime)
	defer cancel()
	select {
	case <-c.gate.Close():
	case <-ctx.Done():
		c.abort()
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	var shutdownErr error
	if c.opened {
		_, shutdownErr = c.request(ctx, protocol.HelperShutdown, []any{})
	}
	c.closed = true
	stopErr := c.transport.stop(c.stopTime)
	if shutdownErr != nil {
		return nil // shutdown failure is handled by kill-and-reap fallback.
	}
	return stopErr
}

func (c *Client) abort() {
	c.gate.Close()
	_ = c.transport.stop(0)
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
}

func (c *Client) abortCurrent() {
	c.gate.Close()
	c.closed = true
	_ = c.transport.stop(c.stopTime)
}

func statusError(status uint64) error {
	if status == 0 {
		return nil
	}
	return &StatusError{Status: status}
}

func validateStatus(status uint64) error {
	if status > 9 {
		return fmt.Errorf("invalid helper status")
	}
	return statusError(status)
}
