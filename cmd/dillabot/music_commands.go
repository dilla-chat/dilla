//go:build linux

package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"log/slog"

	"github.com/jonasthim/dilla/internal/bot/client"
	"github.com/jonasthim/dilla/internal/bot/native"
	"github.com/jonasthim/dilla/internal/bot/protocol"
	"github.com/jonasthim/dilla/internal/bot/service"
	"github.com/jonasthim/dilla/internal/cborx"
	"github.com/jonasthim/dilla/internal/id"
)

type musicCommandState interface {
	StateGet(context.Context, uint64, []byte) (native.StateValue, error)
	StateTransaction(context.Context, id.ID, []native.StateChange) ([]uint64, error)
}
type musicReplyCalls interface {
	ChannelPermissions(context.Context, id.ID) (uint64, error)
}
type musicCommandRecord struct {
	_                    struct{} `cbor:",toarray"`
	Version              uint64
	Phase                uint64
	Fingerprint          [32]byte
	Text                 string
	OriginalNow          uint64
	OperationID          id.ID
	AttemptID            id.ID
	ExpectedGeneration   uint64
	PredecessorAttemptID *id.ID
	ClosedAttempts       uint64
}

// musicReplyRecovery can only finish informational replies recorded by older
// runtimes. It has no music service or command parser, and never creates a record.
type musicReplyRecovery struct {
	community  id.ID
	state      musicCommandState
	calls      musicReplyCalls
	send       func(context.Context, native.GroupEvent, musicCommandRecord, string) (protocol.SendOutcome, error)
	generation func(context.Context) (uint64, error)
}

func (h *musicReplyRecovery) HandleGroupEvent(ctx context.Context, e native.GroupEvent) error {
	if e.Body == nil || e.EnvelopeType != 0 || e.SenderKind != 0 || e.SenderUser.IsZero() || e.SenderDevice.IsZero() || e.Message.IsZero() || e.Community != h.community || e.Thread != nil {
		return nil
	}
	if h.state == nil || h.calls == nil || h.send == nil || h.generation == nil {
		return errors.New("music reply recovery runtime unavailable")
	}
	key := []byte("dilla.bot.music-command.v1/" + e.Group.String() + "/" + e.Message.String())
	stored, err := h.state.StateGet(ctx, 8, key)
	if err != nil {
		return err
	}
	if stored.Revision == 0 || stored.Tombstone {
		return nil
	}
	fingerprintBytes, err := cborx.Marshal([]any{e.Group, e.Message, e.SenderUser, e.SenderDevice, *e.Body})
	if err != nil {
		return err
	}
	fingerprint := sha256.Sum256(fingerprintBytes)
	var record musicCommandRecord
	if cborx.Unmarshal(stored.Value, &record) != nil || record.Version != 1 || record.Phase < 1 || record.Phase > 5 || record.Fingerprint != fingerprint || record.OperationID.IsZero() || record.AttemptID.IsZero() {
		return errors.New("invalid historical music reply receipt")
	}
	if record.Phase >= 3 {
		return nil
	}
	revision := stored.Revision
	persist := func() error {
		data, err := cborx.Marshal(record)
		if err != nil {
			return err
		}
		rows, err := h.state.StateTransaction(ctx, id.New(), []native.StateChange{{Area: 8, Key: key, ExpectedRevision: revision, Value: data}})
		if err != nil {
			return err
		}
		if len(rows) != 1 || rows[0] != revision+1 {
			return errors.New("historical music reply receipt revision mismatch")
		}
		revision = rows[0]
		return nil
	}
	if record.Phase == 1 {
		record.Text = "The previous music command was interrupted and its outcome is uncertain. Use the structured music controls to inspect playback before trying again."
		record.Phase = 2
		if err := persist(); err != nil {
			return err
		}
	}
	allowed, err := h.canReply(ctx, e.Channel)
	if err != nil {
		return err
	}
	if !allowed {
		record.Phase = 5
		return persist()
	}
	if record.ExpectedGeneration == 0 {
		generation, err := h.generation(ctx)
		if err != nil {
			return err
		}
		if generation == 0 {
			return errors.New("music reply generation unavailable")
		}
		record.ExpectedGeneration = generation
		if err := persist(); err != nil {
			return err
		}
	}
	outcome, err := h.send(ctx, e, record, record.Text)
	if err != nil {
		return err
	}
	switch outcome {
	case protocol.ReceiptAccepted:
		record.Phase = 3
	case protocol.ReceiptClosedNotAccepted:
		record.ClosedAttempts++
		allowed, err := h.canReply(ctx, e.Channel)
		if err != nil {
			return err
		}
		if !allowed || record.ClosedAttempts >= 3 {
			record.Phase = 5
			return persist()
		}
		predecessor := record.AttemptID
		record.PredecessorAttemptID = &predecessor
		record.AttemptID = id.New()
		record.ExpectedGeneration = 0
		if err := persist(); err != nil {
			return err
		}
		return errors.New("historical music reply attempt closed; waiting for source reconciliation")
	case protocol.ReceiptUnknownAfterRestore:
		record.Phase = 4
		slog.Warn("historical music reply delivery unknown after restore", "installation_group", e.Group.String(), "message_id", e.Message.String())
	default:
		return errors.New("invalid historical music reply send outcome")
	}
	return persist()
}

func (h *musicReplyRecovery) canReply(ctx context.Context, channel id.ID) (bool, error) {
	bits, err := h.calls.ChannelPermissions(ctx, channel)
	var refused *client.APIError
	if errors.As(err, &refused) && (refused.Status == 403 || refused.Status == 404) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	const viewAndSend uint64 = 1<<0 | 1<<1
	return bits&viewAndSend == viewAndSend, nil
}

func newMusicReplyRecovery(community id.ID, n *native.Client, session *client.Session, api *client.Client, coordinator *service.SendCoordinator) service.GroupEventHandler {
	return &musicReplyRecovery{community: community, state: n, calls: session, generation: func(ctx context.Context) (uint64, error) {
		discovery, _, err := api.DiscoverObserved(ctx)
		return discovery.Generation, err
	}, send: func(ctx context.Context, e native.GroupEvent, r musicCommandRecord, text string) (protocol.SendOutcome, error) {
		body, err := cborx.Marshal([]any{uint64(0), e.Message, text, []any{}, nil})
		if err != nil {
			return protocol.ReceiptUnknownAfterRestore, err
		}
		receipt, err := coordinator.Send(ctx, session, service.SendInput{OperationID: r.OperationID, AttemptID: r.AttemptID, PredecessorAttemptID: r.PredecessorAttemptID, GroupID: e.Group, RequestBody: body, OriginalNow: r.OriginalNow, ExpectedGeneration: r.ExpectedGeneration})
		if errors.Is(err, service.ErrSendTerminal) && receipt.Outcome == protocol.ReceiptUnknownAfterRestore && receipt.OperationID == r.OperationID {
			return receipt.Outcome, nil
		}
		return receipt.Outcome, err
	}}
}
