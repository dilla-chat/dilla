package native

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/dilla-chat/dilla/internal/bot/helper"
	"github.com/dilla-chat/dilla/internal/bot/protocol"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
)

type sendTransport struct {
	response     []any
	payload      []any
	groupPayload []any
	op           protocol.HelperOperation
	closed       int
}

func (f *sendTransport) Open(context.Context, id.ID, id.ID, id.ID, []byte) ([]any, error) {
	panic("unexpected Open")
}
func (f *sendTransport) Call(_ context.Context, op protocol.HelperOperation, payload []any) ([]any, error) {
	f.op, f.payload = op, payload
	return f.response, nil
}
func (f *sendTransport) GroupApply(_ context.Context, payload []any) ([]any, error) {
	f.op, f.groupPayload = protocol.HelperGroupApply, payload
	return f.response, nil
}
func (f *sendTransport) Close() error { f.closed++; return nil }

func TestPrepareSendUsesTypedABIAndPreservesExactCiphertext(t *testing.T) {
	request, _ := cborx.Marshal([]any{uint64(0), nil, "private plaintext", []any{}})
	ciphertext := []byte{0x00, 0xff, 0x80, 0x01}
	upload, _ := cborx.Marshal([]any{uint64(12), ciphertext, uint64(0)})
	message := testID(31)
	f := &sendTransport{response: []any{message[:], upload}}
	c, _ := newClient(f, testBinding())
	got, err := c.PrepareSend(context.Background(), testID(21), testID(22), testID(23), request, 1_800_000_000, 7)
	if err != nil {
		t.Fatal(err)
	}
	if got.MessageID != message || got.Epoch != 12 || got.Class != 0 || string(got.Ciphertext) != string(ciphertext) || string(got.UploadBody) != string(upload) {
		t.Fatalf("prepared send = %#v; ciphertext/upload changed", got)
	}
	if f.op != protocol.HelperPrepareSend || len(f.payload) != 6 || f.payload[0] != testID(21) || f.payload[1] != testID(22) || f.payload[2] != testID(23) || string(f.payload[3].([]byte)) != string(request) || f.payload[4] != uint64(1_800_000_000) || f.payload[5] != uint64(7) {
		t.Fatalf("helper ABI = op %d payload %#v", f.op, f.payload)
	}
	got.Ciphertext[0] = 99
	if ciphertext[0] != 0 {
		t.Fatal("result aliases helper response ciphertext")
	}
}

func TestPrepareInteractionSendCarriesSignedAdmissionToNativeCore(t *testing.T) {
	request, _ := cborx.Marshal([]any{uint64(0), nil, "private response", []any{}})
	snapshot := []byte{0x81, 0x01}
	message := testID(31)
	upload, _ := cborx.Marshal([]any{uint64(12), []byte{0x00, 0xff}, uint64(0)})
	responseBody, _ := cborx.Marshal([]any{message[:], upload})
	f := &sendTransport{response: []any{responseBody}}
	c, _ := newClient(f, testBinding())
	expected := InteractionGroupExpected{
		Instance: testBinding().Instance, Installation: testBinding().Installation, Community: testBinding().Community,
		BotUser: testID(41), BotDevice: testID(42), ParticipantUser: testID(43), ParticipantDevice: testID(44),
		Group: testID(23), MinimumEligibilityRevision: 1, ServerNow: 1_800_000_000, PolicyVersion: 1,
	}
	got, err := c.PrepareInteractionSend(context.Background(), expected, snapshot, testID(21), testID(22), request, 1_800_000_000, 7)
	if err != nil {
		t.Fatal(err)
	}
	if got.MessageID != message || got.Epoch != 12 || string(got.UploadBody) != string(upload) {
		t.Fatalf("prepared interaction send = %#v", got)
	}
	if f.op != protocol.HelperGroupApply || len(f.groupPayload) != 8 || f.groupPayload[0] != uint64(26) || string(f.groupPayload[2].([]byte)) != string(snapshot) || f.groupPayload[3] != testID(21) || f.groupPayload[4] != testID(22) {
		t.Fatalf("helper ABI = op %d payload %#v", f.op, f.groupPayload)
	}
}

func TestPrepareSendRejectsMalformedMutationResultAndClosesClient(t *testing.T) {
	f := &sendTransport{response: []any{[]byte("not an array")}}
	c, _ := newClient(f, testBinding())
	if _, err := c.PrepareSend(context.Background(), testID(21), testID(22), testID(23), []byte{0x84, 0, 0xf6, 0x60, 0x80}, 1, 1); !errors.Is(err, ErrOutcomeUnknown) || f.closed != 1 {
		t.Fatalf("malformed mutation response err=%v closed=%d", err, f.closed)
	}
	if _, err := c.Identity(context.Background()); !errors.Is(err, helper.ErrClosed) {
		t.Fatalf("client remained usable: %v", err)
	}
}

func TestPrepareSendRejectsInvalidLocalInputsWithoutDispatch(t *testing.T) {
	f := &sendTransport{}
	c, _ := newClient(f, testBinding())
	malformedRequest, _ := cborx.Marshal([]any{uint64(0)})
	for _, tc := range []struct {
		name                      string
		operation, attempt, group id.ID
		request                   []byte
		now, generation           uint64
	}{
		{"zero operation", id.Zero, testID(2), testID(3), []byte{0x80}, 1, 1},
		{"bad canonical request schema", testID(1), testID(2), testID(3), malformedRequest, 1, 1},
		{"time overflow", testID(1), testID(2), testID(3), []byte{0x80}, math.MaxInt64 + 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := c.PrepareSend(context.Background(), tc.operation, tc.attempt, tc.group, tc.request, tc.now, tc.generation); err == nil {
				t.Fatal("invalid input accepted")
			}
		})
	}
	if f.op != 0 {
		t.Fatalf("invalid inputs dispatched as op %d", f.op)
	}
}

func TestConfirmSendReceiptValidatesBindingBeforeMutation(t *testing.T) {
	receipt := protocol.SendReceipt{OperationID: testID(10), AttemptID: testID(11), CommunityID: testBinding().Community, GroupID: testID(12), CiphertextHash: make([]byte, 32), Outcome: protocol.ReceiptClosedNotAccepted, ExpectedGeneration: 7, Epoch: 2}
	body, _ := protocol.EncodeReceipt(receipt)
	f := &sendTransport{response: []any{}}
	c, _ := newClient(f, testBinding())
	if err := c.ConfirmSendReceipt(context.Background(), receipt.OperationID, body); err != nil {
		t.Fatal(err)
	}
	if f.op != protocol.HelperSendReceipt || len(f.payload) != 2 || f.payload[0] != receipt.OperationID || string(f.payload[1].([]byte)) != string(body) {
		t.Fatalf("receipt ABI = %#v", f.payload)
	}
	receipt.CommunityID = testID(99)
	foreign, _ := protocol.EncodeReceipt(receipt)
	if err := c.ConfirmSendReceipt(context.Background(), receipt.OperationID, foreign); err == nil {
		t.Fatal("foreign receipt accepted")
	}
}
