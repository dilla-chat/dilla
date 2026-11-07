package gateway

import (
	"context"
	"errors"
	"sync"

	"github.com/fxamacker/cbor/v2"

	"github.com/dilla-chat/dilla/internal/id"
)

// Incoming webhooks on the Bot runtime connection (Bot v2 task 13). An op 66 bot_webhook goes to the Bot's
// live connection only, with n = 0: it is never ringed, held for a parked connection or queued behind a
// resume or a snapshot. The text therefore sits in no ring, and a replay can never post it twice. The POST
// that caused it waits for the op 67 ack from that same connection; the wait lives in a bounded in-memory
// table and is dropped when the POST's own deadline ends it.

// BotWebhookPendingCap bounds the deliveries waiting for an ack across the instance. Each waits at most the
// caller's deadline (2 s for the webhook route), so the cap is reached only by a flood the per-webhook and
// per-address buckets already refuse.
const BotWebhookPendingCap = 1024

var (
	// ErrBotNotLive: the device has no Bot runtime connection that is live (not parked, resuming or
	// streaming its snapshot). Nothing was sent.
	ErrBotNotLive = errors.New("gateway: the Bot has no live connection")
	// ErrBotWebhookBusy: BotWebhookPendingCap deliveries are already waiting. Nothing was sent.
	ErrBotWebhookBusy = errors.New("gateway: too many webhook deliveries are waiting for the Bot")
	// ErrBotWebhookTimeout: the frame was sent but no ack came before the caller's deadline. The Bot may
	// still post it.
	ErrBotWebhookTimeout = errors.New("gateway: the Bot did not acknowledge the webhook in time")
)

// webhookWait is one delivery waiting for its ack: the connection it was sent on and where the status goes.
type webhookWait struct {
	c    *conn
	done chan uint64
}

// webhookWaits is the table of deliveries waiting for an ack, by delivery id.
type webhookWaits struct {
	mu   sync.Mutex
	byID map[id.ID]*webhookWait
}

func (w *webhookWaits) len() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.byID)
}

func (w *webhookWaits) add(delivery id.ID) (*webhookWait, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.byID == nil {
		w.byID = map[id.ID]*webhookWait{}
	}
	if len(w.byID) >= BotWebhookPendingCap {
		return nil, ErrBotWebhookBusy
	}
	if _, dup := w.byID[delivery]; dup {
		return nil, ErrBotWebhookBusy
	}
	wait := &webhookWait{done: make(chan uint64, 1)}
	w.byID[delivery] = wait
	return wait, nil
}

func (w *webhookWaits) bind(wait *webhookWait, c *conn) {
	w.mu.Lock()
	wait.c = c
	w.mu.Unlock()
}

func (w *webhookWaits) forget(delivery id.ID) {
	w.mu.Lock()
	delete(w.byID, delivery)
	w.mu.Unlock()
}

// ack hands status to the delivery's waiter when c is the connection it was sent on. Anything else, an
// unknown id, another connection's ack or a second ack, changes nothing.
func (w *webhookWaits) ack(c *conn, delivery id.ID, status uint64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	wait, ok := w.byID[delivery]
	if !ok || wait.c != c {
		return
	}
	delete(w.byID, delivery)
	wait.done <- status
}

// DeliverBotWebhook sends one op 66 frame, whose payload names deliveryID, to a live Bot runtime connection of
// deviceID and waits until that connection acks it or ctx ends. It returns the ack's status, or
// ErrBotNotLive (nothing sent), ErrBotWebhookBusy (nothing sent) or ErrBotWebhookTimeout (sent, not acked).
func (g *Gateway) DeliverBotWebhook(ctx context.Context, deviceID, deliveryID id.ID, p cbor.RawMessage) (uint64, error) {
	wait, err := g.webhooks.add(deliveryID)
	if err != nil {
		return 0, err
	}
	defer g.webhooks.forget(deliveryID)
	f := Frame{Op: OpBotWebhook, Payload: p}
	sent := false
	for _, c := range g.bots.of(deviceID) {
		// Bound before the send, so an ack that races the send's return still finds its connection.
		g.webhooks.bind(wait, c)
		if c.sendLiveBot(f) {
			sent = true
			break
		}
	}
	if !sent {
		return 0, ErrBotNotLive
	}
	g.countFrame(f)
	select {
	case status := <-wait.done:
		return status, nil
	case <-ctx.Done():
		return 0, ErrBotWebhookTimeout
	}
}

// sendLiveBot sends an unringed frame when this is a live Bot runtime connection with no snapshot or resume in
// progress, and reports whether it did.
func (c *conn) sendLiveBot(f Frame) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.isBot || c.botDead || c.botHolding || c.state != stateReady || c.writer == nil {
		return false
	}
	if c.botSnapshot && c.botSnapshotEnd == 0 {
		return false
	}
	f.Replay = false
	c.sendLocked(f)
	return true
}

// handleWebhookAck takes an op 67 from a Bot runtime connection. A human connection's op 67 is ignored.
func (g *Gateway) handleWebhookAck(c *conn, in Inbound) {
	c.mu.Lock()
	bot := c.isBot
	c.mu.Unlock()
	if !bot {
		return
	}
	a, err := webhookAckFields(in.Payload)
	if err != nil {
		return
	}
	g.webhooks.ack(c, a.delivery, a.status)
}

type webhookAck struct {
	delivery id.ID
	status   uint64
}

// webhookAckFields reads op 67's [delivery_id(bstr 16), status(uint)].
func webhookAckFields(p []cbor.RawMessage) (webhookAck, error) {
	if len(p) != 2 {
		return webhookAck{}, errors.New("want 2 elements")
	}
	b, err := rawBytes(p[0])
	if err != nil || len(b) != len(id.ID{}) {
		return webhookAck{}, errors.New("delivery_id must be 16 bytes")
	}
	status, err := rawUint(p[1])
	if err != nil {
		return webhookAck{}, errors.New("status must be a uint")
	}
	var a webhookAck
	copy(a.delivery[:], b)
	a.status = status
	return a, nil
}

// BotWebhookPayload is op 66 bot_webhook's `[delivery_id, webhook_id, community_id, channel_id, display_name,
// avatar_url|null, text, allow_everyone]` (Bot v2 task 13). allow_everyone travels as uint 0 or 1.
func BotWebhookPayload(deliveryID, webhookID, communityID, channelID id.ID, displayName string, avatarURL *string, text string, allowEveryone bool) (cbor.RawMessage, error) {
	var avatar any
	if avatarURL != nil {
		avatar = *avatarURL
	}
	return payload(deliveryID, webhookID, communityID, channelID, displayName, avatar, text, boolUint(allowEveryone))
}
