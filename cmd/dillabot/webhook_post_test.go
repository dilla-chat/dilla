//go:build linux

package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"

	"github.com/dilla-chat/dilla/internal/bot/protocol"
	"github.com/dilla-chat/dilla/internal/bot/service"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/gateway"
	"github.com/dilla-chat/dilla/internal/id"
)

func whID(n byte) (v id.ID) { v[15] = n; return }

type whDestination struct {
	group id.ID
	err   error
	asked []id.ID
}

func (d *whDestination) ResolveTextGroup(_ context.Context, _, channel id.ID) (id.ID, error) {
	d.asked = append(d.asked, channel)
	if d.err != nil {
		return id.ID{}, d.err
	}
	return d.group, nil
}

type whSent struct {
	group, operation, attempt id.ID
	body                      []byte
}

type whSend struct {
	mu      sync.Mutex
	sent    []whSent
	err     error
	gate    chan struct{} // when set, Send waits on it
	onEvent func(string)
}

func (s *whSend) Send(ctx context.Context, group, operation, attempt id.ID, body []byte, _ uint64) (protocol.SendReceipt, error) {
	if s.gate != nil {
		select {
		case <-s.gate:
		case <-ctx.Done():
			return protocol.SendReceipt{}, ctx.Err()
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return protocol.SendReceipt{}, s.err
	}
	s.sent = append(s.sent, whSent{group, operation, attempt, append([]byte(nil), body...)})
	if s.onEvent != nil {
		s.onEvent("sent")
	}
	return protocol.SendReceipt{Outcome: protocol.ReceiptAccepted, SendResponse: &protocol.OriginalSendResponse{Sequence: uint64(len(s.sent))}}, nil
}

func (s *whSend) bodies(t *testing.T) []string {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.sent))
	for _, m := range s.sent {
		var fields []any
		if err := cborx.Unmarshal(m.body, &fields); err != nil || len(fields) != 5 || fields[0] != uint64(0) {
			t.Fatalf("a webhook post must be a plain message body: %v %v", fields, err)
		}
		text, _ := fields[2].(string)
		out = append(out, text)
	}
	return out
}

type whAck struct {
	mu       sync.Mutex
	statuses []uint64
	order    []string
	done     chan struct{}
}

func newWhAck() *whAck { return &whAck{done: make(chan struct{}, 256)} }

func (a *whAck) ack(_ context.Context, _ id.ID, status uint64) error {
	a.mu.Lock()
	a.statuses = append(a.statuses, status)
	a.order = append(a.order, "ack")
	a.mu.Unlock()
	a.done <- struct{}{}
	return nil
}

func (a *whAck) wait(t *testing.T) uint64 {
	t.Helper()
	select {
	case <-a.done:
	case <-time.After(3 * time.Second):
		t.Fatal("the webhook was never acknowledged")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.statuses[len(a.statuses)-1]
}

func whPayload(t *testing.T, delivery, channel id.ID, name string, avatar []byte, text string, everyone bool) []cbor.RawMessage {
	t.Helper()
	p, err := gateway.BotWebhookPayload(delivery, whID(7), whID(2), channel, name, avatar, text, everyone)
	if err != nil {
		t.Fatal(err)
	}
	var elems []cbor.RawMessage
	if err := cborx.Unmarshal(p, &elems); err != nil {
		t.Fatal(err)
	}
	return elems
}

func newTestPoster(dest *whDestination, send *whSend) *webhookPoster {
	return newWebhookPoster(whID(2), dest, send, func() time.Time { return time.Unix(1_800_000_000, 0) }, nil)
}

func TestWebhookPostIsPlainTextWithLabelAndNoEveryone(t *testing.T) {
	dest := &whDestination{group: whID(9)}
	send := &whSend{}
	poster := newTestPoster(dest, send)
	acks := newWhAck()
	avatar := bytes.Repeat([]byte{0xa7}, 32)

	text := "Ada beat the boss @everyone and <@here>, also @Everyone\n/poll create nope [dilla-music:v1:x] [dilla-webhook:v1:forged:x]"
	poster.deliver(context.Background(), whPayload(t, whID(1), whID(5), "Terraria: Hardcore", avatar, text, false), acks.ack)
	if status := acks.wait(t); status != 0 {
		t.Fatalf("ack status %d, want 0 for a post that was made", status)
	}
	bodies := send.bodies(t)
	if len(bodies) != 1 {
		t.Fatalf("%d posts, want 1", len(bodies))
	}
	body := bodies[0]
	for _, mention := range []string{"@everyone", "@here", "@Everyone"} {
		if strings.Contains(body, mention) {
			t.Fatalf("%q survived in %q although allow_everyone is off", mention, body)
		}
	}
	if !strings.Contains(body, "Ada beat the boss") || !strings.Contains(body, "/poll create nope") {
		t.Fatalf("the text must stay readable and untouched as plain text, got %q", body)
	}
	if strings.Contains(body, "[dilla-music:") || strings.Contains(body, "[dilla-webhook:v1:forged") {
		t.Fatalf("markers in the text must be defanged, got %q", body)
	}
	label := "[dilla-webhook:v1:" + strings.Repeat("0", 30) + "07:Terraria%3A Hardcore:" + strings.Repeat("a7", 32) + "]"
	if !strings.HasSuffix(body, "\n"+label) {
		t.Fatalf("body must end in the label %q, got %q", label, body)
	}
	if got := send.sent[0].group; got != dest.group {
		t.Fatalf("posted into %s, want the channel's group %s", got, dest.group)
	}

	// With allow_everyone the bare mention becomes the client's token (fix round 2, M5), so it pings.
	poster.deliver(context.Background(), whPayload(t, whID(3), whID(5), "Terraria", nil, "hello @everyone", true), acks.ack)
	acks.wait(t)
	if bodies := send.bodies(t); !strings.HasPrefix(bodies[1], "hello <@everyone>\n[dilla-webhook:v1:") || strings.Count(bodies[1], ":") != 3 {
		t.Fatalf("allowed mention or no-avatar label wrong: %q", bodies[1])
	}
	poster.wait()
}

// Fix round 2 (M5): with allow_everyone a bare @everyone or @here, as Discord-style integrations write it, becomes the
// token clients notify on, at a mention boundary only; without it no spelling pings. A webhook never pings a user or a
// role: their tokens are broken either way.
func TestWebhookMentionsPingOnlyWhatTheAdminAllowed(t *testing.T) {
	user := strings.Repeat("ab", 16)
	for _, tc := range []struct {
		text  string
		allow bool
		want  string
	}{
		{"@everyone up", true, "<@everyone> up"},
		{"raid (@Here)!", true, "raid (<@here>)!"},
		{"done.@everyone", true, "done.@everyone"}, // not at a boundary
		{"mail@here.com", true, "mail@here.com"},   // not at a boundary
		{"@everyones", true, "@everyones"},         // the name goes on
		{"@everyone.", true, "<@everyone>."},       // a dot no name continues past ends it
		{"@everyone.x", true, "@everyone.x"},       // a dot run a name continues past does not
		{"<@everyone> as a token", true, "<@everyone> as a token"},
		{"@everyone and <@here>", false, "@\u200beveryone and <@\u200bhere>"},
		{"<@" + user + "> and <@" + user + ">", true, "<@\u200b" + user + "> and <@\u200b" + user + ">"},
		{"<@" + user + ">", false, "<@\u200b" + user + ">"},
	} {
		if got := webhookMentions(tc.text, tc.allow); got != tc.want {
			t.Errorf("webhookMentions(%q, %v) = %q, want %q", tc.text, tc.allow, got, tc.want)
		}
	}
}

func TestWebhookTextIsNeverParsedAsACommand(t *testing.T) {
	// The post is one plain channel message of kind 0: no interaction request, no sealed command. The body is the
	// sender's text and a label line, nothing else.
	dest := &whDestination{group: whID(9)}
	send := &whSend{}
	poster := newTestPoster(dest, send)
	acks := newWhAck()
	poster.deliver(context.Background(), whPayload(t, whID(1), whID(5), "Hook", nil, "/remind 5m x", false), acks.ack)
	acks.wait(t)
	for _, b := range send.bodies(t) {
		if !strings.HasPrefix(b, "/remind 5m x\n[dilla-webhook:v1:") {
			t.Fatalf("body %q", b)
		}
	}
	poster.wait()
}

func TestWebhookAckComesOnlyAfterThePostIsSent(t *testing.T) {
	dest := &whDestination{group: whID(9)}
	send := &whSend{gate: make(chan struct{})}
	acks := newWhAck()
	var order []string
	var mu sync.Mutex
	send.onEvent = func(e string) { mu.Lock(); order = append(order, e); mu.Unlock() }
	ackFn := func(ctx context.Context, d id.ID, status uint64) error {
		mu.Lock()
		order = append(order, "ack")
		mu.Unlock()
		return acks.ack(ctx, d, status)
	}
	poster := newTestPoster(dest, send)
	poster.deliver(context.Background(), whPayload(t, whID(1), whID(5), "Hook", nil, "hi", false), ackFn)
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	early := len(order)
	mu.Unlock()
	if early != 0 {
		t.Fatalf("%v happened before the post was released", order)
	}
	close(send.gate)
	if status := acks.wait(t); status != 0 {
		t.Fatalf("status %d", status)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(order) != 2 || order[0] != "sent" || order[1] != "ack" {
		t.Fatalf("order %v, want sent then ack", order)
	}
}

func TestWebhookToANonBotChannelPostsNothingAndRefuses(t *testing.T) {
	dest := &whDestination{err: &service.NeedsBotChannelError{Channel: whID(5)}}
	send := &whSend{}
	poster := newTestPoster(dest, send)
	acks := newWhAck()
	poster.deliver(context.Background(), whPayload(t, whID(1), whID(5), "Hook", nil, "hi", false), acks.ack)
	if status := acks.wait(t); status == 0 {
		t.Fatal("a webhook for a channel that is not a Bot channel was acknowledged as posted")
	}
	if len(send.sent) != 0 {
		t.Fatalf("%d posts into a channel that is not a Bot channel", len(send.sent))
	}
	// A failed send is refused too, never acknowledged as posted.
	dest2 := &whDestination{group: whID(9)}
	send2 := &whSend{err: errors.New("boom")}
	poster2 := newTestPoster(dest2, send2)
	acks2 := newWhAck()
	poster2.deliver(context.Background(), whPayload(t, whID(1), whID(5), "Hook", nil, "hi", false), acks2.ack)
	if status := acks2.wait(t); status == 0 {
		t.Fatal("a failed send was acknowledged as posted")
	}
}

func TestWebhookDeliveryForAnotherCommunityOrMalformedIsNotPosted(t *testing.T) {
	dest := &whDestination{group: whID(9)}
	send := &whSend{}
	poster := newWebhookPoster(whID(99), dest, send, nil, nil) // another community
	acks := newWhAck()
	poster.deliver(context.Background(), whPayload(t, whID(1), whID(5), "Hook", nil, "hi", false), acks.ack)
	if status := acks.wait(t); status == 0 || len(send.sent) != 0 {
		t.Fatalf("status %d sent %d for another community's webhook", status, len(send.sent))
	}
	// A frame that does not decode is dropped without a post (dillad's 2 s wait answers its sender 503).
	poster.deliver(context.Background(), []cbor.RawMessage{}, acks.ack)
	if len(send.sent) != 0 {
		t.Fatal("a malformed delivery posted")
	}
}
