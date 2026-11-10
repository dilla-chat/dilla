//go:build linux

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/fxamacker/cbor/v2"

	"github.com/dilla-chat/dilla/internal/bot/client"
	"github.com/dilla-chat/dilla/internal/bot/protocol"
	"github.com/dilla-chat/dilla/internal/bot/service"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
)

// Bot v2 Task 17: incoming webhooks are posted through the Bot.
//
// dillad hands the Bot one op 66 bot_webhook `[delivery_id, webhook_id, community_id, channel_id, display_name,
// avatar_id|null, text, allow_everyone]` per POST and waits at most 2 s for the op 67 `[delivery_id, status]` on the
// same connection; any status but 0 (or no ack) answers the sender 503. So the Bot posts first and acknowledges 0 only
// once the message is accepted. The post goes through the same send path as every feature, so the Bot-channel gate
// applies to it: a webhook aimed at a channel that is no longer a Bot channel posts nothing.
//
// The text is never parsed as a command (a channel message is not a command intake), and:
//   - @everyone and @here are neutralised unless allow_everyone is set;
//   - markers other bodies carry for clients ([dilla-music:…], [dilla-poll:…], [dilla-webhook:…]) are defanged, so a
//     webhook cannot forge a player card, a poll or another webhook's label;
//   - the body ends in the label line the client draws "via webhook <name>" from (webhookLabel).

const (
	// webhookAckStatusOK is op 67's status for a post the Bot accepted; anything else is a refusal.
	webhookAckStatusOK uint64 = 0
	// webhookAckStatusRefused is the status for a post the Bot did not make.
	webhookAckStatusRefused uint64 = 1
	// webhookPostBudget keeps the post inside dillad's 2 s wait, so the answer can still be sent.
	webhookPostBudget = 1800 * time.Millisecond
	// webhookConcurrency bounds the deliveries in flight (dillad allows 32 waiting per Bot device).
	webhookConcurrency = 32
	// maxWebhookBody is the message body cap (internal/api maxBodyMessage).
	maxWebhookBody  = 4000
	maxWebhookText  = 4000
	maxWebhookLabel = 80
)

// webhookDelivery is a decoded op 66.
type webhookDelivery struct {
	Delivery, Webhook, Community, Channel id.ID
	DisplayName                           string
	// Avatar is the id of dillad's proxied avatar, or nil. The sender's URL never reaches the Bot.
	Avatar        []byte
	Text          string
	AllowEveryone bool
}

func decodeWebhookDelivery(payload []cbor.RawMessage) (webhookDelivery, error) {
	var d webhookDelivery
	if len(payload) != 8 {
		return d, errors.New("a webhook delivery has 8 elements")
	}
	ids := []*id.ID{&d.Delivery, &d.Webhook, &d.Community, &d.Channel}
	for i, dst := range ids {
		var b []byte
		if cborx.Unmarshal(payload[i], &b) != nil || len(b) != len(*dst) {
			return d, fmt.Errorf("webhook delivery element %d is not an id", i)
		}
		copy(dst[:], b)
		if dst.IsZero() {
			return d, fmt.Errorf("webhook delivery element %d is a zero id", i)
		}
	}
	if cborx.Unmarshal(payload[4], &d.DisplayName) != nil || d.DisplayName == "" || len(d.DisplayName) > maxWebhookLabel || !utf8.ValidString(d.DisplayName) {
		return d, errors.New("webhook display name is invalid")
	}
	var avatar any
	if cborx.Unmarshal(payload[5], &avatar) != nil {
		return d, errors.New("webhook avatar is invalid")
	}
	if avatar != nil {
		b, ok := avatar.([]byte)
		if !ok || len(b) != 32 {
			return d, errors.New("webhook avatar id is not 32 bytes")
		}
		d.Avatar = b
	}
	if cborx.Unmarshal(payload[6], &d.Text) != nil || d.Text == "" || len(d.Text) > maxWebhookText || !utf8.ValidString(d.Text) {
		return d, errors.New("webhook text is invalid")
	}
	var everyone uint64
	if cborx.Unmarshal(payload[7], &everyone) != nil || everyone > 1 {
		return d, errors.New("webhook allow_everyone is invalid")
	}
	d.AllowEveryone = everyone == 1
	return d, nil
}

// neutralizeEveryone keeps @everyone and @here (and their <@…> spellings) from notifying anyone: a zero-width space
// after the @ breaks the mention token and leaves the text readable.
func neutralizeEveryone(text string) string {
	return everyoneMention.ReplaceAllString(text, "@\u200b$1")
}

var (
	everyoneMention = regexp.MustCompile(`(?i)@(everyone|here)`)
	// memberMention is a client's user or role mention token (packages/client-core/src/state/mentions.ts TOKEN).
	memberMention = regexp.MustCompile(`<@([0-9a-f]{32})>`)
)

// webhookMentions makes a webhook's text ping exactly what its admin allowed (fix round 2, M5). Clients notify only on
// the exact tokens <@everyone>, <@here> and <@{user or role hex}> (mentions.ts):
//   - a user or role token is always broken with a zero-width space: a webhook pings no user and no role;
//   - without allow_everyone, every @everyone and @here, bare or as a token and in any case, is broken the same way;
//   - with allow_everyone, a bare @everyone or @here at a mention boundary (as the client's encodeMentions finds one:
//     after the start, white space or one of ( [ { " ', and ending where a name cannot continue) becomes the token, so
//     an integration that writes the bare form, as Discord's do, pings.
func webhookMentions(text string, allowEveryone bool) string {
	text = memberMention.ReplaceAllString(text, "<@\u200b$1>")
	if !allowEveryone {
		return neutralizeEveryone(text)
	}
	var b strings.Builder
	for i := 0; i < len(text); {
		if text[i] == '@' && mentionBoundary(text, i) {
			if handle, ok := broadcastHandle(text[i+1:]); ok {
				b.WriteString("<@" + handle + ">")
				i += 1 + len(handle)
				continue
			}
		}
		b.WriteByte(text[i])
		i++
	}
	return b.String()
}

// mentionBoundary is the client's boundary(): the start, or after white space or one of ( [ { " '.
func mentionBoundary(text string, at int) bool {
	if at == 0 {
		return true
	}
	switch text[at-1] {
	case ' ', '\t', '\n', '\r', '\f', '\v', '(', '[', '{', '"', '\'':
		return true
	}
	r, _ := utf8.DecodeLastRuneInString(text[:at])
	return unicode.IsSpace(r)
}

// broadcastHandle is "everyone" or "here" when rest starts with one (any case) that ends a mention.
func broadcastHandle(rest string) (string, bool) {
	for _, handle := range []string{"everyone", "here"} {
		if len(rest) >= len(handle) && strings.EqualFold(rest[:len(handle)], handle) && mentionEnds(rest[len(handle):]) {
			return handle, true
		}
	}
	return "", false
}

// mentionEnds is the client's endsMention(): the end, CJK text, a dot run no name continues past, or any character
// a name cannot hold.
func mentionEnds(rest string) bool {
	if rest == "" {
		return true
	}
	r, _ := utf8.DecodeRuneInString(rest)
	if isCJK(r) {
		return true
	}
	if r == '.' {
		dots := strings.TrimLeft(rest, ".")
		if dots == "" {
			return true
		}
		next, _ := utf8.DecodeRuneInString(dots)
		return !isNameRune(next) || isCJK(next)
	}
	return !isNameRune(r)
}

func isNameRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsMark(r) || unicode.IsNumber(r) || r == '_' || r == '-'
}

func isCJK(r rune) bool {
	return unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Hangul)
}

// webhookLabelEscape percent-encodes what would end or split a label field: % : [ ] and control characters.
func webhookLabelEscape(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r == '%' || r == ':' || r == '[' || r == ']' || r < 0x20 || r == 0x7f:
			b.WriteString(url.QueryEscape(string(r)))
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// webhookLabel is the metadata line a webhook post ends in:
//
//	[dilla-webhook:v1:<webhook_id>:<display_name>]            (no avatar)
//	[dilla-webhook:v1:<webhook_id>:<display_name>:<avatar_id>]  (avatar_id: 64 hex digits)
//
// webhook_id is 32 hex digits and display_name is percent-escaped for % : [ ] and control characters (QueryEscape
// style, so "%3A" is a colon). A client shows "via webhook <display_name>" and, with an avatar_id, loads dillad's
// proxied avatar for it (GET …/bot/webhook-avatars/{avatar_id}).
func webhookLabel(d webhookDelivery) string {
	label := "[dilla-webhook:v1:" + hex.EncodeToString(d.Webhook[:]) + ":" + webhookLabelEscape(d.DisplayName)
	if d.Avatar != nil {
		label += ":" + hex.EncodeToString(d.Avatar)
	}
	return label + "]"
}

// webhookBody is the message text posted for d: the sender's text, made inert, then the label line. A long text is
// cut at a rune boundary to fit the message cap with its label.
func webhookBody(d webhookDelivery) string {
	text := webhookMentions(service.DefangMarkers(d.Text), d.AllowEveryone)
	label := "\n" + webhookLabel(d)
	if room := maxWebhookBody - len(label); len(text) > room {
		cut := room - len("…")
		for cut > 0 && !utf8.RuneStart(text[cut]) {
			cut--
		}
		text = text[:cut] + "…"
	}
	return text + label
}

// webhookPostIDs derives the post's send operation from the delivery, so a frame delivered twice (a resume) posts once.
func webhookPostIDs(d webhookDelivery) (operation, attempt id.ID) {
	seed := append([]byte("dilla webhook post v1\x00"), d.Delivery[:]...)
	digest := sha256.Sum256(seed)
	copy(operation[:], digest[:len(operation)])
	next := sha256.Sum256(append([]byte("dilla webhook post attempt v1\x00"), operation[:]...))
	copy(attempt[:], next[:len(attempt)])
	return operation, attempt
}

// webhookSend is the Bot's send path (SendCoordinator.Send with the installation's session) for one message, and the
// current server generation it must be sent for.
type webhookSend interface {
	Send(ctx context.Context, group id.ID, operation, attempt id.ID, body []byte, now uint64) (protocol.SendReceipt, error)
}

type webhookDestination interface {
	ResolveTextGroup(ctx context.Context, community, channel id.ID) (id.ID, error)
}

// webhookPoster posts one installation's webhook deliveries.
type webhookPoster struct {
	community   id.ID
	destination webhookDestination
	send        webhookSend
	now         func() time.Time
	log         *slog.Logger
	slots       chan struct{}
	wg          sync.WaitGroup
}

func newWebhookPoster(community id.ID, destination webhookDestination, send webhookSend, now func() time.Time, log *slog.Logger) *webhookPoster {
	if now == nil {
		now = time.Now
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &webhookPoster{community: community, destination: destination, send: send, now: now, log: log, slots: make(chan struct{}, webhookConcurrency)}
}

// deliver posts payload (an op 66) and answers it with ack, off the caller's goroutine (the delivery loop must keep
// reading). With every slot taken the delivery is refused at once, so dillad answers its sender 503 rather than wait.
func (p *webhookPoster) deliver(ctx context.Context, payload []cbor.RawMessage, ack func(ctx context.Context, delivery id.ID, status uint64) error) {
	d, err := decodeWebhookDelivery(payload)
	if err != nil {
		p.log.Warn("bot webhook delivery malformed", "error", err.Error())
		return
	}
	select {
	case p.slots <- struct{}{}:
	default:
		p.log.Warn("bot webhook deliveries are all busy; refused", "webhook_id", d.Webhook.String())
		p.answer(ctx, ack, d.Delivery, webhookAckStatusRefused)
		return
	}
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		defer func() { <-p.slots }()
		postCtx, cancel := context.WithTimeout(ctx, webhookPostBudget)
		defer cancel()
		status := webhookAckStatusRefused
		err := p.post(postCtx, d)
		if ctx.Err() != nil {
			// The connection the delivery came on ended (or the Bot is stopping): dillad takes an ack only from that
			// connection, so there is nobody to answer, and nothing to log but a quiet debug line. The send is a durable
			// attempt keyed by the delivery; a post cut off before its upload is simply not made.
			p.log.Debug("bot webhook delivery cancelled with its connection", "webhook_id", d.Webhook.String())
			return
		}
		if err != nil {
			p.log.Info("bot webhook not posted", "webhook_id", d.Webhook.String(), "channel_id", d.Channel.String(), "reason", webhookReason(err))
		} else {
			status = webhookAckStatusOK
		}
		p.answer(ctx, ack, d.Delivery, status)
	}()
}

// answer sends op 67. It uses its own short deadline: the post's budget may be spent, and dillad is waiting.
func (p *webhookPoster) answer(ctx context.Context, ack func(context.Context, id.ID, uint64) error, delivery id.ID, status uint64) {
	ackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
	defer cancel()
	if err := ack(ackCtx, delivery, status); err != nil && ctx.Err() == nil {
		p.log.Warn("bot webhook ack not sent", "error", err.Error())
	}
}

func webhookReason(err error) string {
	switch {
	case errors.Is(err, service.ErrNeedsBotChannel):
		return service.NeedsBotChannelCode
	case errors.Is(err, service.ErrBotChannelsUnknown):
		return "bot_channels_unknown"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	}
	return "send_failed"
}

// post sends d's message into its channel's group and returns once the message is accepted. Nothing is sent for a
// channel that is not a Bot channel (the destination refuses it, and so would the send layer below).
func (p *webhookPoster) post(ctx context.Context, d webhookDelivery) error {
	if d.Community != p.community {
		return errors.New("webhook is for another community")
	}
	group, err := p.destination.ResolveTextGroup(ctx, p.community, d.Channel)
	if err != nil {
		return err
	}
	body, err := cborx.Marshal([]any{uint64(0), nil, webhookBody(d), []any{}, nil})
	if err != nil {
		return err
	}
	operation, attempt := webhookPostIDs(d)
	created := p.now().UTC().Unix()
	if created < 0 {
		return errors.New("clock before the epoch")
	}
	receipt, err := p.send.Send(ctx, group, operation, attempt, body, uint64(created))
	if err != nil {
		return err
	}
	if receipt.Outcome != protocol.ReceiptAccepted || receipt.SendResponse == nil || receipt.SendResponse.Sequence == 0 {
		return errors.New("the send was not accepted")
	}
	return nil
}

// wait blocks until the deliveries in flight have answered (shutdown and tests).
func (p *webhookPoster) wait() { p.wg.Wait() }

// webhookRegistry is the runtime's webhook posters by installation. Its zero value is ready.
type webhookRegistry struct {
	mu      sync.Mutex
	posters map[id.ID]*webhookPoster
}

func (w *webhookRegistry) set(installation id.ID, p *webhookPoster) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.posters == nil {
		w.posters = map[id.ID]*webhookPoster{}
	}
	if p == nil {
		delete(w.posters, installation)
		return
	}
	w.posters[installation] = p
}

func (w *webhookRegistry) lookup(installation id.ID) *webhookPoster {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.posters[installation]
}

func (w *webhookRegistry) retain(keep map[id.ID]bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for installation := range w.posters {
		if !keep[installation] {
			delete(w.posters, installation)
		}
	}
}

// SetWebhookPoster installs the installation's webhook poster (nil removes it).
func (r *botGatewayRuntime) SetWebhookPoster(installation id.ID, p *webhookPoster) {
	r.webhooks.set(installation, p)
}

// deliverWebhook hands an op 66 to the installation's poster. With no poster yet (a pass has not built one) nothing is
// posted and no ack is sent, so dillad answers the sender 503 and the sender retries.
func (r *botGatewayRuntime) deliverWebhook(ctx context.Context, installation id.ID, ack func(context.Context, id.ID, uint64) error, payload []cbor.RawMessage) {
	poster := r.webhooks.lookup(installation)
	if poster == nil {
		r.log.Warn("bot webhook delivery with no poster", "installation_id", installation.String())
		return
	}
	poster.deliver(ctx, payload, ack)
}

// coordinatorWebhookSend posts through the installation's SendCoordinator, for the server generation discovery
// reports now.
type coordinatorWebhookSend struct {
	coordinator *service.SendCoordinator
	session     *client.Session
	authority   interface {
		DiscoverObserved(context.Context) (client.Discovery, client.ResponseObservation, error)
	}
}

func (s coordinatorWebhookSend) Send(ctx context.Context, group, operation, attempt id.ID, body []byte, now uint64) (protocol.SendReceipt, error) {
	discovery, _, err := s.authority.DiscoverObserved(ctx)
	if err != nil {
		return protocol.SendReceipt{}, err
	}
	return s.coordinator.Send(ctx, s.session, service.SendInput{OperationID: operation, AttemptID: attempt, GroupID: group,
		RequestBody: body, OriginalNow: now, ExpectedGeneration: discovery.Generation})
}
