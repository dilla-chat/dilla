// Package native is a narrow typed adapter over the private dilla-bot-core
// helper protocol. It deliberately exposes no generic operation dispatcher.
package native

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"unicode/utf8"

	"github.com/jonasthim/dilla/internal/bot/client"
	"github.com/jonasthim/dilla/internal/bot/helper"
	"github.com/jonasthim/dilla/internal/bot/protocol"
	"github.com/jonasthim/dilla/internal/cborx"
	"github.com/jonasthim/dilla/internal/id"
	"github.com/jonasthim/dilla/internal/prioritygate"
)

var ErrOutcomeUnknown = helper.ErrOutcomeUnknown

const (
	storeVersion  = 1
	maxStateValue = 262144
)

type Binding struct{ Instance, Community, Installation id.ID }

type transport interface {
	Open(context.Context, id.ID, id.ID, id.ID, []byte) ([]any, error)
	Call(context.Context, protocol.HelperOperation, []any) ([]any, error)
	GroupApply(context.Context, []any) ([]any, error)
	Close() error
}

type Client struct {
	mu      sync.Mutex
	gate    *prioritygate.Gate
	t       transport
	binding Binding
	usable  bool
	closed  bool
	sources map[id.ID]bool
	trust   map[id.ID]bool
}

// Binding reports the immutable identity tuple this helper was opened for.
func (c *Client) Binding() Binding {
	if c == nil {
		return Binding{}
	}
	return c.binding
}

// Start starts one compiled native helper and opens the encrypted store for
// this exact identity tuple. databaseKey is consumed and cleared by helper.Open.
func Start(ctx context.Context, options helper.Options, binding Binding, databaseKey []byte) (*Client, error) {
	if !binding.valid() {
		clear(databaseKey)
		return nil, errors.New("invalid native identity binding")
	}
	h, err := helper.Start(options)
	if err != nil {
		clear(databaseKey)
		return nil, err
	}
	return newOpenedClient(h, binding, func() ([]any, error) {
		return h.Open(ctx, binding.Instance, binding.Community, binding.Installation, databaseKey)
	})
}

// newClient is the package-private seam used by boundary tests after a valid
// open. Production callers must use Start.
func newClient(t transport, binding Binding) (*Client, error) {
	if t == nil || !binding.valid() {
		return nil, errors.New("invalid native client binding")
	}
	return &Client{gate: prioritygate.New(1), t: t, binding: binding, usable: true}, nil
}

func newOpenedClient(t transport, binding Binding, open ...func() ([]any, error)) (*Client, error) {
	if t == nil || !binding.valid() {
		if t != nil {
			_ = t.Close()
		}
		return nil, errors.New("invalid native client binding")
	}
	var result []any
	var err error
	if len(open) > 0 {
		result, err = open[0]()
	} else {
		return nil, errors.New("missing native open response")
	}
	if err != nil {
		_ = t.Close()
		return nil, err
	}
	if len(result) != 2 || result[0] != uint64(storeVersion) {
		_ = t.Close()
		return nil, errors.New("native open returned an unsupported store response")
	}
	body, ok := result[1].([]byte)
	if !ok {
		_ = t.Close()
		return nil, errors.New("native open returned an invalid identity response")
	}
	if _, err := decodeIdentity(body, binding); err != nil {
		_ = t.Close()
		return nil, err
	}
	return &Client{gate: prioritygate.New(1), t: t, binding: binding, usable: true}, nil
}

func (b Binding) valid() bool {
	return !b.Instance.IsZero() && !b.Community.IsZero() && !b.Installation.IsZero()
}

func (c *Client) call(ctx context.Context, op protocol.HelperOperation, payload []any, mutation bool, decode func([]any) (any, error)) (any, error) {
	if err := client.CooperativeCheckpoint(ctx); err != nil {
		return nil, err
	}
	release, err := c.gate.Acquire(ctx, client.RequestPriority(ctx))
	if err != nil {
		if errors.Is(err, prioritygate.ErrClosed) {
			err = helper.ErrClosed
		}
		return nil, err
	}
	defer release()
	if err := client.CooperativeCheckpoint(ctx); err != nil {
		return nil, err
	}
	c.mu.Lock()
	if !c.usable || c.closed {
		c.mu.Unlock()
		return nil, helper.ErrClosed
	}
	c.mu.Unlock()
	var result []any
	if op == protocol.HelperGroupApply {
		result, err = c.t.GroupApply(ctx, payload)
	} else {
		result, err = c.t.Call(ctx, op, payload)
	}
	if err != nil {
		return nil, err
	}
	value, err := decode(result)
	if err != nil {
		if mutation {
			c.invalidateLocked()
		}
		return nil, fmt.Errorf("native helper returned an invalid result: %w", outcomeOrValidation(err, mutation))
	}
	return value, nil
}

func outcomeOrValidation(err error, mutation bool) error {
	if mutation {
		return errors.Join(ErrOutcomeUnknown, err)
	}
	return err
}
func (c *Client) invalidateLocked() {
	c.gate.Close()
	c.mu.Lock()
	c.usable = false
	c.closed = true
	c.mu.Unlock()
	_ = c.t.Close()
}

func resultAs[T any](value any) (T, error) {
	result, ok := value.(T)
	if !ok {
		var zero T
		return zero, fmt.Errorf("native result type mismatch: expected %T", zero)
	}
	return result, nil
}

func (c *Client) Bootstrap(ctx context.Context, externalSenderKey [32]byte) (BootstrapResult, error) {
	if allZeroCallKey(externalSenderKey[:]) {
		return BootstrapResult{}, errors.New("invalid native bootstrap instance key")
	}
	p := []any{c.binding.Instance, c.binding.Community, c.binding.Installation, externalSenderKey[:]}
	v, err := c.call(ctx, protocol.HelperBootstrap, p, true, func(a []any) (any, error) { return decodeBootstrap(a) })
	if err != nil {
		return BootstrapResult{}, err
	}
	return resultAs[BootstrapResult](v)
}
func (c *Client) CompleteEnrollment(ctx context.Context, user id.ID, username string, now uint64) (EnrollmentResult, error) {
	if user.IsZero() || !utf8.ValidString(username) || len([]byte(username)) < 1 || len([]byte(username)) > 256 || now > math.MaxInt64 {
		return EnrollmentResult{}, errors.New("invalid enrollment completion input")
	}
	v, err := c.call(ctx, protocol.HelperEnrollComplete, []any{user, username, now}, true, func(a []any) (any, error) {
		if len(a) != 2 {
			return nil, errors.New("result shape")
		}
		cred, ok := a[0].([]byte)
		if !ok || len(cred) == 0 || len(cred) > 8192 {
			return nil, errors.New("credential shape")
		}
		list, ok := a[1].([]byte)
		if !ok || len(list) == 0 || len(list) > protocol.MaxHelperPayload {
			return nil, errors.New("device list shape")
		}
		return EnrollmentResult{append([]byte(nil), cred...), append([]byte(nil), list...)}, nil
	})
	if err != nil {
		return EnrollmentResult{}, err
	}
	return resultAs[EnrollmentResult](v)
}
func (c *Client) Identity(ctx context.Context) (Identity, error) {
	v, err := c.call(ctx, protocol.HelperSession, []any{uint64(0)}, false, wrappedBody(func(b []byte) (any, error) { return decodeIdentity(b, c.binding) }))
	if err != nil {
		return Identity{}, err
	}
	return resultAs[Identity](v)
}
func (c *Client) SignSession(ctx context.Context, nonce [32]byte, purpose uint64) (SessionSignature, error) {
	if purpose > 1 {
		return SessionSignature{}, errors.New("invalid session purpose")
	}
	v, err := c.call(ctx, protocol.HelperSession, []any{uint64(1), nonce[:], purpose}, false, wrappedBody(func(b []byte) (any, error) { return decodeSessionSignature(b, nonce, purpose) }))
	if err != nil {
		return SessionSignature{}, err
	}
	return resultAs[SessionSignature](v)
}
func (c *Client) StoreSession(ctx context.Context, token string, expires, idleExpires uint64) error {
	if !utf8.ValidString(token) || len([]byte(token)) < 1 || len([]byte(token)) > 256 || expires > math.MaxInt64 || idleExpires > math.MaxInt64 {
		return errors.New("invalid session input")
	}
	_, err := c.call(ctx, protocol.HelperSession, []any{uint64(2), token, expires, idleExpires}, true, voidResult)
	return err
}
func (c *Client) Session(ctx context.Context) (Session, error) {
	v, err := c.call(ctx, protocol.HelperSession, []any{uint64(3)}, false, wrappedBody(func(b []byte) (any, error) { return decodeSession(b) }))
	if err != nil {
		return Session{}, err
	}
	return resultAs[Session](v)
}
func (c *Client) ClearSession(ctx context.Context) error {
	_, err := c.call(ctx, protocol.HelperSession, []any{uint64(4)}, true, voidResult)
	return err
}
func (c *Client) DeviceList(ctx context.Context) ([]byte, error) {
	v, err := c.call(ctx, protocol.HelperSession, []any{uint64(5)}, false, wrappedBytes)
	if err != nil {
		return nil, err
	}
	return resultAs[[]byte](v)
}
func (c *Client) DeviceListPublished(ctx context.Context) error {
	_, err := c.call(ctx, protocol.HelperSession, []any{uint64(6)}, true, voidResult)
	return err
}

// KeyPackageIntent returns an exact native upload intent. It does not verify a
// server receipt; the caller owns HTTP receipt authentication and lifecycle.
func (c *Client) KeyPackageIntent(ctx context.Context, v protocol.KeyPackageIntent) (protocol.KeyPackageIntentResult, error) {
	b, err := protocol.EncodeKeyPackageIntent(v)
	if err != nil {
		return protocol.KeyPackageIntentResult{}, err
	}
	a, err := unmarshalArray(b)
	if err != nil {
		return protocol.KeyPackageIntentResult{}, err
	}
	x, err := c.call(ctx, protocol.HelperKeyPackages, a, true, func(a []any) (any, error) {
		raw, e := cborx.Marshal(a)
		if e != nil {
			return nil, e
		}
		return protocol.DecodeKeyPackageIntentResult(raw)
	})
	if err != nil {
		return protocol.KeyPackageIntentResult{}, err
	}
	return resultAs[protocol.KeyPackageIntentResult](x)
}

// ResolveKeyPackageIntent records accepted/obsolete server evidence in native
// state. It does not authenticate that evidence; the caller must do so first.
func (c *Client) ResolveKeyPackageIntent(ctx context.Context, v protocol.KeyPackageResolution) error {
	b, err := protocol.EncodeKeyPackageResolution(v)
	if err != nil {
		return err
	}
	a, err := unmarshalArray(b)
	if err != nil {
		return err
	}
	_, err = c.call(ctx, protocol.HelperKeyPackagesUploaded, a, true, voidResult)
	return err
}

type StateValue struct {
	Revision  uint64
	Value     []byte
	Tombstone bool
}
type StateEntry struct {
	Key       []byte
	Revision  uint64
	Value     []byte
	Tombstone bool
}
type StatePage struct {
	Entries []StateEntry
	Next    []byte
}
type StateChange struct {
	Area             uint64
	Key              []byte
	ExpectedRevision uint64
	Value            []byte
}

func (c *Client) StateGet(ctx context.Context, area uint64, key []byte) (StateValue, error) {
	if err := validAreaKey(area, key); err != nil {
		return StateValue{}, err
	}
	v, err := c.call(ctx, protocol.HelperStateGet, []any{uint64(0), area, append([]byte(nil), key...)}, false, func(a []any) (any, error) {
		if len(a) != 2 {
			return nil, errors.New("result shape")
		}
		rev, ok := uintValue(a[0])
		if !ok || rev > math.MaxInt64 {
			return nil, errors.New("revision shape")
		}
		if a[1] == nil {
			return StateValue{Revision: rev, Tombstone: rev != 0}, nil
		}
		b, ok := a[1].([]byte)
		if rev == 0 || !ok || !validStateValue(b) {
			return nil, errors.New("value shape")
		}
		return StateValue{Revision: rev, Value: append([]byte(nil), b...)}, nil
	})
	if err != nil {
		return StateValue{}, err
	}
	return resultAs[StateValue](v)
}
func (c *Client) StateScan(ctx context.Context, area uint64, after []byte, limit uint64) (StatePage, error) {
	if area > 9 || (after != nil && (len(after) < 1 || len(after) > 128)) || limit < 1 || limit > 100 {
		return StatePage{}, errors.New("invalid state scan input")
	}
	var cur any
	if after != nil {
		cur = append([]byte(nil), after...)
	}
	v, err := c.call(ctx, protocol.HelperStateGet, []any{uint64(1), area, cur, limit}, false, func(a []any) (any, error) { return decodeStatePage(a, after, limit) })
	if err != nil {
		return StatePage{}, err
	}
	return resultAs[StatePage](v)
}
func (c *Client) StateTransaction(ctx context.Context, transactionID id.ID, changes []StateChange) ([]uint64, error) {
	if transactionID.IsZero() || len(changes) < 1 || len(changes) > 64 {
		return nil, errors.New("invalid state transaction input")
	}
	seen := make(map[string]bool, len(changes))
	rawChanges := make([]any, 0, len(changes))
	expectedRevisions := make([]uint64, 0, len(changes))
	for _, ch := range changes {
		if err := validAreaKey(ch.Area, ch.Key); err != nil {
			return nil, err
		}
		if ch.ExpectedRevision >= math.MaxInt64 {
			return nil, errors.New("invalid expected revision")
		}
		key := fmt.Sprintf("%d:%x", ch.Area, ch.Key)
		if seen[key] {
			return nil, errors.New("duplicate state change")
		}
		seen[key] = true
		if ch.Value != nil && !validStateValue(ch.Value) {
			return nil, errors.New("state value must be a canonical CBOR array no larger than 256 KiB")
		}
		rawChanges = append(rawChanges, []any{ch.Area, append([]byte(nil), ch.Key...), ch.ExpectedRevision, cloneNullable(ch.Value)})
		expectedRevisions = append(expectedRevisions, ch.ExpectedRevision)
	}
	v, err := c.call(ctx, protocol.HelperStateTransaction, []any{transactionID, rawChanges}, true, func(a []any) (any, error) {
		if len(a) != len(expectedRevisions) {
			return nil, errors.New("revision count")
		}
		revs := make([]uint64, len(a))
		for i, x := range a {
			n, ok := uintValue(x)
			if !ok || n != expectedRevisions[i]+1 || n > math.MaxInt64 {
				return nil, errors.New("revision shape")
			}
			revs[i] = n
		}
		return revs, nil
	})
	if err != nil {
		return nil, err
	}
	return resultAs[[]uint64](v)
}

func (c *Client) Close() error {
	<-c.gate.Close()
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	c.usable = false
	c.mu.Unlock()
	return c.t.Close()
}

func wrappedBody(decode func([]byte) (any, error)) func([]any) (any, error) {
	return func(a []any) (any, error) {
		if len(a) != 1 {
			return nil, errors.New("wrapped body shape")
		}
		b, ok := a[0].([]byte)
		if !ok {
			return nil, errors.New("wrapped body type")
		}
		return decode(b)
	}
}
func wrappedBytes(a []any) (any, error) {
	if len(a) != 1 {
		return nil, errors.New("wrapped body shape")
	}
	b, ok := a[0].([]byte)
	if !ok || len(b) == 0 || len(b) > protocol.MaxHelperPayload {
		return nil, errors.New("body type")
	}
	return append([]byte(nil), b...), nil
}
func voidResult(a []any) (any, error) {
	if len(a) != 0 {
		return nil, errors.New("void result shape")
	}
	return struct{}{}, nil
}
func decodeIdentity(b []byte, binding Binding) (Identity, error) {
	a, err := decodeArrayBytes(b, 6)
	if err != nil {
		return Identity{}, err
	}
	phase, ok := uintValue(a[0])
	if !ok || phase > 3 {
		return Identity{}, errors.New("phase")
	}
	instance, err := nullableID(a[1])
	if err != nil {
		return Identity{}, err
	}
	user, err := nullableID(a[2])
	if err != nil {
		return Identity{}, err
	}
	device, err := nullableID(a[3])
	if err != nil {
		return Identity{}, err
	}
	name, ok := a[4].(string)
	if !ok || !utf8.ValidString(name) || len([]byte(name)) > 256 {
		return Identity{}, errors.New("username")
	}
	published, ok := uintValue(a[5])
	if !ok || published > 1 {
		return Identity{}, errors.New("published")
	}
	valid := false
	switch phase {
	case 0:
		valid = instance == nil && user == nil && device == nil && name == "" && published == 0
	case 1:
		valid = instance != nil && user == nil && device != nil && name == "" && published == 0
	case 2:
		valid = instance != nil && user != nil && device != nil && len(name) > 0
	case 3:
		valid = instance != nil && device != nil && name == "" && published == 0
	}
	if !valid || instance != nil && *instance != binding.Instance {
		return Identity{}, errors.New("identity shape or binding")
	}
	return Identity{Phase: phase, Instance: instance, User: user, Device: device, Username: name, DeviceListPublished: published == 1}, nil
}
func decodeBootstrap(a []any) (BootstrapResult, error) {
	if len(a) != 4 {
		return BootstrapResult{}, errors.New("bootstrap shape")
	}
	var out BootstrapResult
	umk, ok := bytesN(a[0], 32)
	if !ok {
		return out, errors.New("umk")
	}
	copy(out.UMKPublic[:], umk)
	ssk, ok := bytesN(a[1], 32)
	if !ok {
		return out, errors.New("ssk")
	}
	copy(out.SSKPublic[:], ssk)
	sig, ok := bytesN(a[2], 64)
	if !ok {
		return out, errors.New("signature")
	}
	copy(out.SigUMKSSK[:], sig)
	d, ok := a[3].([]any)
	if !ok || len(d) != 5 {
		return out, errors.New("device")
	}
	device, ok := bytesN(d[0], 16)
	if !ok || zero(device) {
		return out, errors.New("device id")
	}
	copy(out.Device.DeviceID[:], device)
	dsk, ok := bytesN(d[1], 32)
	if !ok {
		return out, errors.New("dsk")
	}
	copy(out.Device.DSKPublic[:], dsk)
	tier, tok := uintValue(d[2])
	signer, sok := uintValue(d[3])
	cred, cok := d[4].([]byte)
	if !tok || tier != 0 || !sok || signer != 0 || !cok || len(cred) == 0 || len(cred) > 8192 {
		return out, errors.New("device registration")
	}
	out.Device.Tier = tier
	out.Device.SignerTier = signer
	out.Device.Credential = append([]byte(nil), cred...)
	return out, nil
}
func decodeSessionSignature(b []byte, nonce [32]byte, purpose uint64) (SessionSignature, error) {
	a, e := decodeArrayBytes(b, 5)
	if e != nil {
		return SessionSignature{}, e
	}
	got, ok := bytesN(a[0], 32)
	if !ok || string(got) != string(nonce[:]) {
		return SessionSignature{}, errors.New("nonce echo")
	}
	p, ok := uintValue(a[1])
	if !ok || p != purpose || p > 1 {
		return SessionSignature{}, errors.New("purpose echo")
	}
	sig, ok := bytesN(a[2], 64)
	if !ok || a[3] != nil || a[4] != nil {
		return SessionSignature{}, errors.New("signature shape")
	}
	var out SessionSignature
	out.Nonce = nonce
	out.Purpose = purpose
	copy(out.Signature[:], sig)
	return out, nil
}
func decodeSession(b []byte) (Session, error) {
	if len(b) == 1 && b[0] == 0xf6 { // canonical CBOR null: no bearer stored yet.
		return Session{}, nil
	}
	a, e := decodeArray(b)
	if e != nil {
		return Session{}, e
	}
	if len(a) != 3 {
		return Session{}, errors.New("session shape")
	}
	token, ok := a[0].(string)
	if !ok || !utf8.ValidString(token) || len([]byte(token)) < 1 || len([]byte(token)) > 256 {
		return Session{}, errors.New("session token")
	}
	expires, eok := uintValue(a[1])
	idle, iok := uintValue(a[2])
	if !eok || !iok || expires > math.MaxInt64 || idle > math.MaxInt64 {
		return Session{}, errors.New("session expiry")
	}
	return Session{token: token, Expires: expires, IdleExpires: idle}, nil
}
func decodeStatePage(a []any, after []byte, limit uint64) (any, error) {
	if len(a) != 2 {
		return nil, errors.New("scan shape")
	}
	rows, ok := a[0].([]any)
	if !ok || uint64(len(rows)) > limit {
		return nil, errors.New("rows")
	}
	page := StatePage{}
	var prev []byte
	for _, x := range rows {
		r, ok := x.([]any)
		if !ok || len(r) != 3 {
			return nil, errors.New("row shape")
		}
		key, kok := r[0].([]byte)
		rev, rok := uintValue(r[1])
		if !kok || len(key) < 1 || len(key) > 128 || !rok || rev == 0 || rev > math.MaxInt64 || after != nil && string(key) <= string(after) || prev != nil && string(key) <= string(prev) {
			return nil, errors.New("row ordering")
		}
		entry := StateEntry{Key: append([]byte(nil), key...), Revision: rev}
		if r[2] == nil {
			entry.Tombstone = true
		} else {
			value, vok := r[2].([]byte)
			if !vok || !validStateValue(value) {
				return nil, errors.New("row value")
			}
			entry.Value = append([]byte(nil), value...)
		}
		page.Entries = append(page.Entries, entry)
		prev = key
	}
	next, err := nullableBytes(a[1])
	if err != nil {
		return nil, err
	}
	if len(page.Entries) == 0 && next != nil || len(page.Entries) > 0 && (next == nil || string(next) != string(page.Entries[len(page.Entries)-1].Key)) {
		return nil, errors.New("next cursor")
	}
	page.Next = next
	return page, nil
}

func decodeArrayBytes(b []byte, n int) ([]any, error) {
	a, e := decodeArray(b)
	if e != nil || len(a) != n {
		return nil, errors.New("invalid native body shape")
	}
	return a, nil
}
func decodeArray(b []byte) ([]any, error) {
	if len(b) == 0 || len(b) > protocol.MaxHelperPayload || cborx.ExpectMajor(b, cborx.MajorArray) != nil {
		return nil, errors.New("invalid native body")
	}
	var a []any
	if e := cborx.Unmarshal(b, &a); e != nil {
		return nil, errors.New("invalid native body")
	}
	return a, nil
}
func unmarshalArray(b []byte) ([]any, error) {
	var a []any
	if e := cborx.Unmarshal(b, &a); e != nil {
		return nil, e
	}
	return a, nil
}
func uintValue(v any) (uint64, bool) { n, ok := v.(uint64); return n, ok }
func nullableID(v any) (*id.ID, error) {
	if v == nil {
		return nil, nil
	}
	b, ok := v.([]byte)
	if !ok || len(b) != 16 {
		return nil, errors.New("identifier shape")
	}
	var x id.ID
	copy(x[:], b)
	if x.IsZero() {
		return nil, errors.New("zero identifier")
	}
	return &x, nil
}
func nullableBytes(v any) ([]byte, error) {
	if v == nil {
		return nil, nil
	}
	b, ok := v.([]byte)
	if !ok || len(b) < 1 || len(b) > 128 {
		return nil, errors.New("cursor shape")
	}
	return append([]byte(nil), b...), nil
}
func bytesN(v any, n int) ([]byte, bool) { b, ok := v.([]byte); return b, ok && len(b) == n }
func zero(b []byte) bool {
	for _, v := range b {
		if v != 0 {
			return false
		}
	}
	return true
}
func cloneNullable(v []byte) any {
	if v == nil {
		return nil
	}
	return append([]byte(nil), v...)
}
func validAreaKey(area uint64, key []byte) error {
	if area > 9 || len(key) < 1 || len(key) > 128 {
		return errors.New("invalid state area or key")
	}
	return nil
}
func validStateValue(v []byte) bool {
	return len(v) > 0 && len(v) <= maxStateValue && cborx.ExpectMajor(v, cborx.MajorArray) == nil && cborx.CheckDeterministic(v) == nil
}
