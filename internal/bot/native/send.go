package native

import (
	"context"
	"errors"
	"math"

	"github.com/dilla-chat/dilla/internal/bot/protocol"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
)

const maxSendRequestBody = 32 << 20

// PreparedSend is the durable helper result for one logical operation attempt.
// UploadBody is retained byte-for-byte; Ciphertext is its exact ciphertext field.
type PreparedSend struct {
	MessageID  id.ID
	Epoch      uint64
	Class      uint64
	Ciphertext []byte
	UploadBody []byte
}

func (PreparedSend) String() string   { return "PreparedSend{ciphertext:[redacted]}" }
func (PreparedSend) GoString() string { return "native.PreparedSend{ciphertext:[redacted]}" }

// PrepareSend asks the helper to durably encrypt one immutable send attempt.
// The caller must reuse the same operation, attempt and input after an unknown
// outcome; this method never invents identifiers or retries a new attempt.
func (c *Client) PrepareSend(ctx context.Context, operation, attempt, group id.ID, request []byte, originalNow, expectedGeneration uint64) (PreparedSend, error) {
	if operation.IsZero() || attempt.IsZero() || group.IsZero() || len(request) == 0 || len(request) > maxSendRequestBody || !validSendRequest(request) || originalNow > math.MaxInt64 || expectedGeneration > math.MaxInt64 {
		return PreparedSend{}, errors.New("invalid native send input")
	}
	v, err := c.call(ctx, protocol.HelperPrepareSend, []any{operation, attempt, group, append([]byte(nil), request...), originalNow, expectedGeneration}, true, decodePreparedSend)
	if err != nil {
		return PreparedSend{}, err
	}
	return resultAs[PreparedSend](v)
}

func validSendRequest(body []byte) bool {
	if cborx.CheckDeterministic(body) != nil {
		return false
	}
	var request []any
	if cborx.Unmarshal(body, &request) != nil || len(request) != 4 && len(request) != 5 {
		return false
	}
	if _, ok := nativeUint(request[0]); !ok {
		return false
	}
	if !validOptionalSendBytes(request[1], 16) {
		return false
	}
	if _, ok := request[2].(string); !ok {
		return false
	}
	attachments, ok := request[3].([]any)
	if !ok {
		return false
	}
	for _, value := range attachments {
		row, ok := value.([]any)
		if !ok || len(row) != 9 || !validSendBytes(row[0], 32) || !validSendBytes(row[1], 32) || !validSendBytes(row[2], 12) {
			return false
		}
		if _, ok := nativeUint(row[3]); !ok {
			return false
		}
		if _, ok := row[4].(string); !ok || !validOptionalSendUint(row[5]) || !validOptionalSendUint(row[6]) || !validOptionalSendBytes(row[7], -1) {
			return false
		}
		if _, ok := row[8].(string); !ok {
			return false
		}
	}
	return len(request) == 4 || validOptionalSendBytes(request[4], 16)
}

func validSendBytes(value any, length int) bool {
	b, ok := value.([]byte)
	return ok && len(b) == length
}

func validOptionalSendBytes(value any, length int) bool {
	return value == nil || validSendBytes(value, length) || length == -1 && isByteString(value)
}

func isByteString(value any) bool { _, ok := value.([]byte); return ok }

func validOptionalSendUint(value any) bool {
	if value == nil {
		return true
	}
	_, ok := nativeUint(value)
	return ok
}

func decodePreparedSend(tuple []any) (any, error) {
	if len(tuple) != 2 {
		return nil, errors.New("send result shape")
	}
	messageBytes, ok := tuple[0].([]byte)
	if !ok || len(messageBytes) != 16 {
		return nil, errors.New("message id")
	}
	var message id.ID
	copy(message[:], messageBytes)
	if message.IsZero() {
		return nil, errors.New("message id")
	}
	upload, ok := tuple[1].([]byte)
	if !ok || len(upload) == 0 || len(upload) > maxGroupBody || cborx.CheckDeterministic(upload) != nil {
		return nil, errors.New("upload body")
	}
	var fields []any
	if err := cborx.Unmarshal(upload, &fields); err != nil || len(fields) != 3 {
		return nil, errors.New("upload shape")
	}
	epoch, ok := nativeUint(fields[0])
	if !ok || epoch > math.MaxInt64 {
		return nil, errors.New("upload epoch")
	}
	ciphertext, ok := fields[1].([]byte)
	if !ok || len(ciphertext) == 0 || len(ciphertext) > maxGroupBody {
		return nil, errors.New("upload ciphertext")
	}
	class, ok := nativeUint(fields[2])
	if !ok || class > 1 {
		return nil, errors.New("upload class")
	}
	return PreparedSend{MessageID: message, Epoch: epoch, Class: class, Ciphertext: append([]byte(nil), ciphertext...), UploadBody: append([]byte(nil), upload...)}, nil
}

func nativeUint(v any) (uint64, bool) {
	n, ok := v.(uint64)
	return n, ok
}

// ConfirmSendReceipt confirms a server receipt in the native durable outbox.
// The exact canonical receipt body is passed through unchanged.
func (c *Client) ConfirmSendReceipt(ctx context.Context, operation id.ID, receiptBody []byte) error {
	if operation.IsZero() || len(receiptBody) == 0 || len(receiptBody) > 512 || cborx.CheckDeterministic(receiptBody) != nil {
		return errors.New("invalid native send receipt input")
	}
	receipt, err := protocol.DecodeReceipt(receiptBody)
	if err != nil || receipt.OperationID != operation || receipt.CommunityID != c.binding.Community {
		return errors.New("native send receipt scope mismatch")
	}
	_, err = c.call(ctx, protocol.HelperSendReceipt, []any{operation, append([]byte(nil), receiptBody...)}, true, voidResult)
	return err
}
