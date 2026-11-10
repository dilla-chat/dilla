package native

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/dilla-chat/dilla/internal/bot/helper"
	"github.com/dilla-chat/dilla/internal/bot/protocol"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
)

type fakeTransport struct {
	responses     [][]any
	statuses      []uint64
	err           error
	calls         []protocol.HelperOperation
	callPayloads  [][]any
	closed        int
	groupApplies  int
	groupPayloads [][]any
}

func (f *fakeTransport) Open(context.Context, id.ID, id.ID, id.ID, []byte) ([]any, error) {
	return nil, errors.New("unexpected Open")
}
func (f *fakeTransport) Call(_ context.Context, op protocol.HelperOperation, payload []any) ([]any, error) {
	f.calls = append(f.calls, op)
	f.callPayloads = append(f.callPayloads, append([]any(nil), payload...))
	if f.err != nil {
		return nil, f.err
	}
	if len(f.statuses) > 0 {
		status := f.statuses[0]
		f.statuses = f.statuses[1:]
		if status != 0 {
			return nil, &helper.StatusError{Status: status}
		}
	}
	if len(f.responses) == 0 {
		return nil, errors.New("no fake response")
	}
	out := f.responses[0]
	f.responses = f.responses[1:]
	return out, nil
}
func (f *fakeTransport) GroupApply(ctx context.Context, payload []any) ([]any, error) {
	f.groupApplies++
	f.groupPayloads = append(f.groupPayloads, append([]any(nil), payload...))
	return f.Call(ctx, protocol.HelperGroupApply, payload)
}
func (f *fakeTransport) Close() error { f.closed++; return nil }

func testBinding() Binding {
	return Binding{Instance: testID(1), Community: testID(2), Installation: testID(3)}
}
func testID(n byte) (v id.ID) { v[15] = n; return }
func newFakeClient(t *testing.T, f *fakeTransport) *Client {
	t.Helper()
	c, err := newClient(f, testBinding())
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestBootstrapUsesDirectTupleAndRejectsWrappedTuple(t *testing.T) {
	device := []any{make([]byte, 16), make([]byte, 32), uint64(0), uint64(0), []byte{0x81, 0}}
	device[0].([]byte)[15] = 9
	device[1].([]byte)[0] = 1
	direct := []any{make([]byte, 32), make([]byte, 32), make([]byte, 64), device}
	direct[0].([]byte)[0] = 2
	direct[1].([]byte)[0] = 3
	direct[2].([]byte)[0] = 4
	f := &fakeTransport{responses: [][]any{direct}}
	c := newFakeClient(t, f)
	instanceKey := [32]byte{0x77}
	got, err := c.Bootstrap(context.Background(), instanceKey)
	if err != nil {
		t.Fatal(err)
	}
	if got.Device.DeviceID[15] != 9 || got.Device.Tier != 0 || got.Device.SignerTier != 0 || got.UMKPublic[0] != 2 {
		t.Fatalf("unexpected decoded bootstrap: %#v", got)
	}

	wrapper, err := cborx.Marshal([]any{[]byte{0x80}})
	if err != nil {
		t.Fatal(err)
	}
	f = &fakeTransport{responses: [][]any{{wrapper}}}
	c = newFakeClient(t, f)
	_, err = c.Bootstrap(context.Background(), instanceKey)
	if !errors.Is(err, ErrOutcomeUnknown) {
		t.Fatalf("wrapped mutation result: got %v", err)
	}
	if f.closed != 1 {
		t.Fatalf("malformed mutation did not close transport: %d", f.closed)
	}
	if _, err = c.Identity(context.Background()); !errors.Is(err, helper.ErrClosed) {
		t.Fatalf("malformed mutation left client usable: %v", err)
	}
	if len(f.calls) != 1 {
		t.Fatalf("request after malformed mutation: calls=%d", len(f.calls))
	}
}

func TestOpenRejectsInstanceMismatch(t *testing.T) {
	info, _ := cborx.Marshal([]any{uint64(1), testID(99), nil, testID(8), "", uint64(0)})
	f := &fakeTransport{}
	c, err := newOpenedClient(f, testBinding(), func() ([]any, error) { return []any{uint64(1), info}, nil })
	if err == nil || c != nil {
		t.Fatalf("mismatched open accepted: client=%v err=%v", c, err)
	}
	if f.closed != 1 {
		t.Fatalf("invalid opened helper left alive: %d", f.closed)
	}
	if !errors.Is(err, helper.ErrOpenResponse) || helper.Class(err) != "open_response" {
		t.Fatalf("mismatched open = %v (class %q), want an open_response", err, helper.Class(err))
	}
}

// Every answer to Open the client cannot accept is an open_response for helper.Class (fix round, M-1).
func TestOpenResponseFailuresAreClassified(t *testing.T) {
	for _, answer := range [][]any{{uint64(99), []byte{}}, {uint64(1), "not bytes"}} {
		_, err := newOpenedClient(&fakeTransport{}, testBinding(), func() ([]any, error) { return answer, nil })
		if !errors.Is(err, helper.ErrOpenResponse) || helper.Class(err) != "open_response" || !strings.HasPrefix(err.Error(), "native open returned") {
			t.Fatalf("open answer %v = %v (class %q), want an open_response keeping its text", answer, err, helper.Class(err))
		}
	}
}

func TestSessionFormattingRedactsToken(t *testing.T) {
	body, _ := cborx.Marshal([]any{"secret-bearer", uint64(10), uint64(9)})
	f := &fakeTransport{responses: [][]any{{body}}}
	c := newFakeClient(t, f)
	s, err := c.Session(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, format := range []string{"%v", "%+v", "%#v"} {
		if got := fmt.Sprintf(format, s); strings.Contains(got, "secret-bearer") {
			t.Fatalf("%s leaked token: %s", format, got)
		}
	}
	if s.Token() != "secret-bearer" {
		t.Fatal("explicit token accessor lost token")
	}
}

func TestSessionAcceptsCanonicalNullForEmptyStore(t *testing.T) {
	f := &fakeTransport{responses: [][]any{{[]byte{0xf6}}}}
	c := newFakeClient(t, f)
	s, err := c.Session(context.Background())
	if err != nil || s.Token() != "" || s.Expires != 0 || s.IdleExpires != 0 {
		t.Fatalf("empty session: %#v, %v", s, err)
	}
	if len(f.calls) != 1 || f.calls[0] != protocol.HelperSession {
		t.Fatalf("unexpected helper calls: %v", f.calls)
	}
}

func TestStateTombstoneAndTransactionConflictDoNotRetry(t *testing.T) {
	f := &fakeTransport{responses: [][]any{{uint64(4), nil}}}
	c := newFakeClient(t, f)
	v, err := c.StateGet(context.Background(), 2, []byte("key"))
	if err != nil || v.Revision != 4 || !v.Tombstone {
		t.Fatalf("tombstone: %#v, %v", v, err)
	}

	f = &fakeTransport{statuses: []uint64{4}}
	c = newFakeClient(t, f)
	_, err = c.StateTransaction(context.Background(), testID(7), []StateChange{{Area: 1, Key: []byte("k"), ExpectedRevision: 2, Value: []byte{0x80}}})
	if !errors.As(err, new(*helper.StatusError)) {
		t.Fatalf("conflict lost: %v", err)
	}
	if len(f.calls) != 1 {
		t.Fatalf("transaction retried %d calls", len(f.calls))
	}
}

func TestStateAreaNineSupportsMemberRegistrationCatchup(t *testing.T) {
	f := &fakeTransport{responses: [][]any{{uint64(0), nil}, {[]any{}, nil}}}
	c := newFakeClient(t, f)
	if _, err := c.StateGet(context.Background(), 9, []byte("dilla.bot.member-registration-catchup.v1")); err != nil {
		t.Fatalf("state area 9 refused: %v", err)
	}
	if _, err := c.StateScan(context.Background(), 9, nil, 10); err != nil {
		t.Fatalf("state area 9 scan refused: %v", err)
	}
}

func TestStateGetRejectsValueAtRevisionZeroWithoutMutationOutcomeUnknown(t *testing.T) {
	f := &fakeTransport{responses: [][]any{{uint64(0), []byte{0x80}}, {uint64(0), nil}}}
	c := newFakeClient(t, f)
	got, err := c.StateGet(context.Background(), 1, []byte("missing"))
	if err == nil || !reflect.DeepEqual(got, StateValue{}) {
		t.Fatalf("revision-zero value was accepted: %#v, %v", got, err)
	}
	if errors.Is(err, ErrOutcomeUnknown) || f.closed != 0 {
		t.Fatalf("read shape error got mutation semantics: err=%v closes=%d", err, f.closed)
	}
	missing, err := c.StateGet(context.Background(), 1, []byte("missing"))
	if err != nil || missing.Revision != 0 || missing.Value != nil || missing.Tombstone {
		t.Fatalf("client was not usable for canonical missing result: %#v, %v", missing, err)
	}
}

func TestStateTransactionRequiresExpectedRevisionPlusOne(t *testing.T) {
	f := &fakeTransport{responses: [][]any{{uint64(9)}}}
	c := newFakeClient(t, f)
	_, err := c.StateTransaction(context.Background(), testID(8), []StateChange{{Area: 1, Key: []byte("k"), ExpectedRevision: 0, Value: []byte{0x80}}})
	if err == nil || !errors.Is(err, ErrOutcomeUnknown) {
		t.Fatalf("revision mismatch was not outcome unknown: %v", err)
	}
	if f.closed != 1 || len(f.calls) != 1 {
		t.Fatalf("malformed transaction response not quarantined: closes=%d calls=%d", f.closed, len(f.calls))
	}
	if _, err = c.Identity(context.Background()); !errors.Is(err, helper.ErrClosed) {
		t.Fatalf("client remained usable after malformed transaction response: %v", err)
	}
}

func TestStateTransactionRejectsUnincrementableRevisionLocally(t *testing.T) {
	f := &fakeTransport{}
	c := newFakeClient(t, f)
	_, err := c.StateTransaction(context.Background(), testID(8), []StateChange{{Area: 1, Key: []byte("k"), ExpectedRevision: uint64(1<<63 - 1), Value: []byte{0x80}}})
	if err == nil || len(f.calls) != 0 {
		t.Fatalf("unincrementable revision dispatched: calls=%d err=%v", len(f.calls), err)
	}
}

func TestStateTransactionRequiresCanonicalCBORForPersistedCursor(t *testing.T) {
	f := &fakeTransport{}
	c := newFakeClient(t, f)
	_, err := c.StateTransaction(context.Background(), testID(8), []StateChange{{Area: 8, Key: []byte("cursor"), ExpectedRevision: 0, Value: []byte("raw-state-key")}})
	if err == nil || len(f.calls) != 0 {
		t.Fatalf("raw state cursor was dispatched: calls=%d err=%v", len(f.calls), err)
	}

	value, err := cborx.Marshal([]any{uint64(1), []byte("raw-state-key")})
	if err != nil {
		t.Fatal(err)
	}
	f = &fakeTransport{responses: [][]any{{uint64(1)}}}
	c = newFakeClient(t, f)
	if _, err := c.StateTransaction(context.Background(), testID(8), []StateChange{{Area: 8, Key: []byte("cursor"), ExpectedRevision: 0, Value: value}}); err != nil {
		t.Fatalf("canonical CBOR cursor rejected: %v", err)
	}
}

func TestCanceledRequestDoesNotRetry(t *testing.T) {
	f := &fakeTransport{err: context.Canceled}
	c := newFakeClient(t, f)
	_, _ = c.Identity(context.Background())
	if len(f.calls) != 1 {
		t.Fatalf("request count %d", len(f.calls))
	}
}

func TestSessionEchoMismatchIsRejected(t *testing.T) {
	nonce := [32]byte{1}
	body, _ := cborx.Marshal([]any{[]byte{2}, uint64(0), make([]byte, 64), nil, nil})
	f := &fakeTransport{responses: [][]any{{body}}}
	c := newFakeClient(t, f)
	if _, err := c.SignSession(context.Background(), nonce, 0); err == nil {
		t.Fatal("mismatched nonce accepted")
	}
	if len(f.calls) != 1 {
		t.Fatalf("call count = %d", len(f.calls))
	}
}

func TestStateScanRejectsOrderingAndLocalInvalidInputDoesNotDispatch(t *testing.T) {
	rows := []any{
		[]any{[]byte("b"), uint64(1), []byte{0x80}},
		[]any{[]byte("a"), uint64(2), []byte{0x80}},
	}
	f := &fakeTransport{responses: [][]any{{rows, []byte("a")}}}
	c := newFakeClient(t, f)
	if _, err := c.StateScan(context.Background(), 1, nil, 10); err == nil {
		t.Fatal("unordered scan accepted")
	}
	if _, err := c.StateGet(context.Background(), 10, []byte("k")); err == nil {
		t.Fatal("invalid local area accepted")
	}
	if len(f.calls) != 1 {
		t.Fatalf("invalid local input dispatched: calls=%d", len(f.calls))
	}
}
