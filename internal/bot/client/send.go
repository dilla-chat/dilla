package client

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"net/http"
	"net/url"

	botprotocol "github.com/jonasthim/dilla/internal/bot/protocol"
	"github.com/jonasthim/dilla/internal/cborx"
	"github.com/jonasthim/dilla/internal/id"
)

var ErrBotSendReceiptNotFound = errors.New("bot client: send receipt not found")

// BotMessageUpload is the exact input accepted by the managed Bot message
// endpoint. PrivateMessage is the ciphertext bstr produced by native MLS.
type BotMessageUpload struct {
	OperationID, AttemptID    id.ID
	PredecessorAttemptID      *id.ID
	ExpectedGeneration, Epoch uint64
	PrivateMessage            []byte
	Class                     uint64
}

func (BotMessageUpload) String() string { return "BotMessageUpload{private_message:[redacted]}" }
func (BotMessageUpload) GoString() string {
	return "client.BotMessageUpload{private_message:[redacted]}"
}

// BotMessageResolution closes or recovers one exact send attempt without
// retransmitting its ciphertext.
type BotMessageResolution struct {
	AttemptID                 id.ID
	PredecessorAttemptID      *id.ID
	GroupID                   id.ID
	ExpectedGeneration, Epoch uint64
	Class                     uint64
	CiphertextSHA256          [32]byte
}

func validSendPredecessor(v *id.ID) bool { return v == nil || !v.IsZero() }

func (s *Session) PostBotMessage(ctx context.Context, community, group id.ID, upload BotMessageUpload) (botprotocol.SendReceipt, error) {
	if _, err := pathID(community); err != nil {
		return botprotocol.SendReceipt{}, err
	}
	groupPath, err := pathID(group)
	if err != nil {
		return botprotocol.SendReceipt{}, err
	}
	if upload.OperationID.IsZero() || upload.AttemptID.IsZero() || !validSendPredecessor(upload.PredecessorAttemptID) || upload.ExpectedGeneration > maxStoredSequence || upload.Epoch > maxStoredSequence || upload.Class > 1 || len(upload.PrivateMessage) == 0 || len(upload.PrivateMessage) > MaxCommitBody {
		return botprotocol.SendReceipt{}, ErrInvalidConfig
	}
	var predecessor any
	if upload.PredecessorAttemptID != nil {
		predecessor = *upload.PredecessorAttemptID
	}
	body, err := cborx.Marshal([]any{upload.OperationID, upload.AttemptID, predecessor, upload.ExpectedGeneration, upload.Epoch, append([]byte(nil), upload.PrivateMessage...), upload.Class})
	if err != nil || len(body) > MaxBody {
		return botprotocol.SendReceipt{}, ErrInvalidConfig
	}
	raw, _, err := s.do(ctx, http.MethodPost, "groups/"+groupPath+"/bot/messages", nil, body, MaxBody, true)
	if err != nil {
		return botprotocol.SendReceipt{}, err
	}
	receipt, err := botprotocol.DecodeReceipt(raw)
	if err != nil || receipt.OperationID != upload.OperationID || receipt.AttemptID != upload.AttemptID || receipt.PredecessorAttemptID != nil && (upload.PredecessorAttemptID == nil || *receipt.PredecessorAttemptID != *upload.PredecessorAttemptID) || receipt.PredecessorAttemptID == nil && upload.PredecessorAttemptID != nil || receipt.ExpectedGeneration != upload.ExpectedGeneration || receipt.Epoch != upload.Epoch || receipt.Class != upload.Class || botprotocol.ValidateReceiptContext(receipt, community, group) != nil {
		return botprotocol.SendReceipt{}, unknown()
	}
	hash := sha256.Sum256(upload.PrivateMessage)
	if !bytes.Equal(receipt.CiphertextHash, hash[:]) {
		return botprotocol.SendReceipt{}, unknown()
	}
	return receipt, nil
}

func (s *Session) BotMessageReceipt(ctx context.Context, community, operation, attempt, expectedGroup id.ID) (botprotocol.SendReceipt, error) {
	communityPath, err := pathID(community)
	if err != nil {
		return botprotocol.SendReceipt{}, err
	}
	opPath, err := pathID(operation)
	if err != nil {
		return botprotocol.SendReceipt{}, err
	}
	attemptPath, err := pathID(attempt)
	if err != nil {
		return botprotocol.SendReceipt{}, err
	}
	if _, err = pathID(expectedGroup); err != nil {
		return botprotocol.SendReceipt{}, err
	}
	query := url.Values{"attempt": []string{attemptPath}}
	raw, status, err := s.do(ctx, http.MethodGet, "communities/"+communityPath+"/bot/receipts/"+opPath, query, nil, MaxSmallBody, false)
	if err != nil {
		return botprotocol.SendReceipt{}, sendReceiptError(err, status)
	}
	receipt, err := botprotocol.DecodeReceipt(raw)
	if err != nil || receipt.OperationID != operation || receipt.AttemptID != attempt || botprotocol.ValidateReceiptContext(receipt, community, expectedGroup) != nil {
		return botprotocol.SendReceipt{}, ErrProtocol
	}
	return receipt, nil
}

func (s *Session) ResolveBotMessage(ctx context.Context, community, operation id.ID, resolution BotMessageResolution) (botprotocol.SendReceipt, error) {
	communityPath, err := pathID(community)
	if err != nil {
		return botprotocol.SendReceipt{}, err
	}
	opPath, err := pathID(operation)
	if err != nil {
		return botprotocol.SendReceipt{}, err
	}
	if resolution.AttemptID.IsZero() || resolution.GroupID.IsZero() || !validSendPredecessor(resolution.PredecessorAttemptID) || resolution.ExpectedGeneration > maxStoredSequence || resolution.Epoch > maxStoredSequence || resolution.Class > 1 {
		return botprotocol.SendReceipt{}, ErrInvalidConfig
	}
	var predecessor any
	if resolution.PredecessorAttemptID != nil {
		predecessor = *resolution.PredecessorAttemptID
	}
	body, err := cborx.Marshal([]any{resolution.AttemptID, predecessor, resolution.GroupID, resolution.ExpectedGeneration, resolution.Epoch, resolution.Class, resolution.CiphertextSHA256[:]})
	if err != nil {
		return botprotocol.SendReceipt{}, ErrInvalidConfig
	}
	raw, _, err := s.do(ctx, http.MethodPost, "communities/"+communityPath+"/bot/receipts/"+opPath+"/resolve", nil, body, MaxSmallBody, true)
	if err != nil {
		return botprotocol.SendReceipt{}, err
	}
	receipt, err := botprotocol.DecodeReceipt(raw)
	if err != nil || receipt.OperationID != operation || receipt.AttemptID != resolution.AttemptID || receipt.PredecessorAttemptID != nil && (resolution.PredecessorAttemptID == nil || *receipt.PredecessorAttemptID != *resolution.PredecessorAttemptID) || receipt.PredecessorAttemptID == nil && resolution.PredecessorAttemptID != nil || receipt.ExpectedGeneration != resolution.ExpectedGeneration || receipt.Epoch != resolution.Epoch || receipt.Class != resolution.Class || !bytes.Equal(receipt.CiphertextHash, resolution.CiphertextSHA256[:]) || botprotocol.ValidateReceiptContext(receipt, community, resolution.GroupID) != nil {
		return botprotocol.SendReceipt{}, unknown()
	}
	return receipt, nil
}

func sendReceiptError(err error, status int) error {
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound || status == http.StatusNotFound {
		return ErrBotSendReceiptNotFound
	}
	return err
}
