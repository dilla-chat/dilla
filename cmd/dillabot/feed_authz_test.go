//go:build linux

package main

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"

	"github.com/dilla-chat/dilla/internal/bot/intake"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/gateway"
	"github.com/dilla-chat/dilla/internal/id"
)

func authzPayload(t *testing.T, kind uint64, body any) []cbor.RawMessage {
	t.Helper()
	raw, err := gateway.BotEventPayload(kind, body)
	if err != nil {
		t.Fatal(err)
	}
	var out []cbor.RawMessage
	if err := cborx.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// Kinds 9 and 10 decode as dillad encodes them and reach the intake's Authz through the installation's teed feed.
func TestFeedAuthzKindsDecodeAsDilladEncodesAndReachTheAuthz(t *testing.T) {
	if gateway.BotEventChannelOverwrites != feedChannelOverwrites || gateway.BotEventMemberTimeout != feedMemberTimeout {
		t.Fatal("the Bot and dillad disagree on the kind numbers")
	}
	ctx := context.Background()
	installation, community, everyone, user, channel := id.New(), id.New(), id.New(), id.New(), id.New()
	r := newBotGatewayRuntime(ctx, nil, true, slog.New(slog.NewTextHandler(io.Discard, nil)))
	feed := r.feedFor(installation, community, id.New(), feedDeps{})
	tee := r.sealed.feed(installation, community, feed)
	authz := r.sealed.lookup(installation).authz
	apply := func(kind uint64, body any, snapshot bool) {
		t.Helper()
		ev, err := decodeFeedEvent(authzPayload(t, kind, body))
		if err != nil {
			t.Fatal(err)
		}
		ev.fromSnapshot = snapshot
		if err := ev.apply(ctx, tee); err != nil {
			t.Fatal(err)
		}
	}
	tee.SnapshotBegin(ctx)
	apply(feedRole, []any{community, everyone, []uint64{intake.PermViewChannel | intake.PermSendMessages, 0, 0}, uint64(0)}, true)
	apply(feedMember, []any{community, user, uint64(1), []id.ID{}}, true)
	apply(feedChannelOverwrites, []any{community, channel, []any{[]any{uint64(0), everyone, uint64(0), intake.PermSendMessages}}, uint64(0)}, true)
	tee.SnapshotDone(ctx)
	if authz.Allowed(community, channel, user, intake.PermSendMessages, false) || !authz.Allowed(community, id.ID{}, user, intake.PermSendMessages, true) {
		t.Fatal("the snapshot's channel deny did not apply")
	}
	apply(feedChannelOverwrites, []any{community, channel, []any{}, uint64(1)}, false)
	if authz.Allowed(community, channel, user, intake.PermSendMessages, false) {
		t.Fatal("a deleted channel is unknown: a non-zero bit is refused")
	}
	until := uint64(time.Now().Unix()) + 600
	apply(feedMemberTimeout, []any{community, user, until}, false)
	if !authz.TimedOut(user, time.Now()) {
		t.Fatal("the timeout did not apply")
	}
	apply(feedMemberTimeout, []any{community, user, uint64(0)}, false)
	if authz.TimedOut(user, time.Now()) {
		t.Fatal("the cleared timeout did not apply")
	}
}

func TestFeedAuthzKindsAreDecodedStrictly(t *testing.T) {
	c, ch, u := id.New(), id.New(), id.New()
	row := []any{uint64(1), id.New(), uint64(1), uint64(2)}
	for _, bad := range []struct {
		kind uint64
		body any
	}{
		{feedChannelOverwrites, []any{c, ch, []any{row}}},                                                         // three elements
		{feedChannelOverwrites, []any{c, ch, []any{[]any{uint64(3), id.New(), uint64(0), uint64(0)}}, uint64(0)}}, // target kind 3
		{feedChannelOverwrites, []any{c, ch, []any{[]any{uint64(1), id.New(), uint64(0)}}, uint64(0)}},            // a short row
		{feedChannelOverwrites, []any{c, ch, []any{row}, uint64(2)}},                                              // deleted 2
		{feedChannelOverwrites, []any{c, ch, []any{row}, uint64(1)}},                                              // deleted with entries
		{feedChannelOverwrites, []any{c, id.ID{}, []any{}, uint64(0)}},                                            // zero channel
		{feedMemberTimeout, []any{c, u}},
		{feedMemberTimeout, []any{c, u, "soon"}},
	} {
		if _, err := decodeFeedEvent(authzPayload(t, bad.kind, bad.body)); err == nil {
			t.Fatalf("accepted kind %d %v", bad.kind, bad.body)
		}
	}
}
