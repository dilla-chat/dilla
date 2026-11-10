//go:build linux

package main

import (
	"context"
	"fmt"

	"github.com/fxamacker/cbor/v2"

	"github.com/dilla-chat/dilla/internal/bot/intake"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
)

// Bot v2 Task 15 (security review ruling): the two op 65 kinds that let the Bot authorise a sealed command in its
// channel, as dillad would. They are decoded and applied here; feed.go hands them over with a three-line hook (a
// feedEvent field, a decode branch and an apply branch), so Task 16's feed.go and feed_handlers.go stay theirs.
const (
	// feedChannelOverwrites is `[community_id, channel_id, [[target_kind, target_id, allow, deny]…], deleted]`, for a
	// Bot channel or a voice channel only. target_kind is 0 @everyone, 1 role, 2 member; deleted 1 means the channel
	// is gone or is no longer a Bot or voice channel.
	feedChannelOverwrites uint64 = 9
	// feedMemberTimeout is `[community_id, user_id, until_unix | 0]`.
	feedMemberTimeout uint64 = 10
	// maxFeedOverwrites bounds one channel's entries: dillad holds one per role and member at most, and a community
	// has far fewer than this.
	maxFeedOverwrites = 4096
)

// authzFeedEvent is a decoded kind 9 or 10.
type authzFeedEvent struct {
	channel, user id.ID
	overwrites    []intake.Overwrite
	deleted       bool
	until         uint64
}

// authzFeed is the handler side: sealedFeed (command_worker.go) applies both to the installation's Authz.
type authzFeed interface {
	ChannelOverwrites(ctx context.Context, community, channel id.ID, overwrites []intake.Overwrite, deleted, snapshot bool)
	MemberTimeout(ctx context.Context, community, user id.ID, until uint64, snapshot bool)
}

// decodeAuthzFeedEvent reads a kind 9 or 10 body strictly.
func decodeAuthzFeedEvent(kind uint64, body []cbor.RawMessage) (feedEvent, error) {
	ev := feedEvent{kind: kind}
	idAt := func(raw cbor.RawMessage) (id.ID, error) {
		v, err := feedField[id.ID](raw)
		if err != nil || v.IsZero() {
			return id.ID{}, errFeedShape
		}
		return v, nil
	}
	switch kind {
	case feedChannelOverwrites:
		if len(body) != 4 {
			return ev, fmt.Errorf("%w: kind %d with %d elements", errFeedShape, kind, len(body))
		}
		community, err := idAt(body[0])
		if err != nil {
			return ev, err
		}
		channel, err := idAt(body[1])
		if err != nil {
			return ev, err
		}
		var rows []cbor.RawMessage
		if cborx.ExpectMajor(body[2], cborx.MajorArray) != nil || cborx.Unmarshal(body[2], &rows) != nil || len(rows) > maxFeedOverwrites {
			return ev, errFeedShape
		}
		deleted, err := feedFlag(body[3])
		if err != nil {
			return ev, err
		}
		out := make([]intake.Overwrite, 0, len(rows))
		for _, raw := range rows {
			var row []cbor.RawMessage
			if cborx.ExpectMajor(raw, cborx.MajorArray) != nil || cborx.Unmarshal(raw, &row) != nil || len(row) != 4 {
				return ev, errFeedShape
			}
			targetKind, err := feedField[uint64](row[0])
			if err != nil || targetKind > uint64(intake.OverwriteMember) {
				return ev, errFeedShape
			}
			target, err := idAt(row[1])
			if err != nil {
				return ev, err
			}
			allow, errAllow := feedField[uint64](row[2])
			deny, errDeny := feedField[uint64](row[3])
			if errAllow != nil || errDeny != nil {
				return ev, errFeedShape
			}
			out = append(out, intake.Overwrite{Kind: intake.OverwriteKind(targetKind), Target: target, Allow: allow, Deny: deny})
		}
		if deleted && len(out) != 0 {
			return ev, errFeedShape
		}
		ev.community = community
		ev.authz = authzFeedEvent{channel: channel, overwrites: out, deleted: deleted}
	case feedMemberTimeout:
		if len(body) != 3 {
			return ev, fmt.Errorf("%w: kind %d with %d elements", errFeedShape, kind, len(body))
		}
		community, err := idAt(body[0])
		if err != nil {
			return ev, err
		}
		user, err := idAt(body[1])
		if err != nil {
			return ev, err
		}
		until, err := feedField[uint64](body[2])
		if err != nil {
			return ev, err
		}
		ev.community = community
		ev.authz = authzFeedEvent{user: user, until: until}
	default:
		return ev, errFeedShape
	}
	return ev, nil
}

// applyAuthz hands a kind 9 or 10 to h when it keeps an Authz; a handler without one ignores it.
func (ev feedEvent) applyAuthz(ctx context.Context, h Feed) {
	a, ok := h.(authzFeed)
	if !ok {
		return
	}
	switch ev.kind {
	case feedChannelOverwrites:
		a.ChannelOverwrites(ctx, ev.community, ev.authz.channel, ev.authz.overwrites, ev.authz.deleted, ev.fromSnapshot)
	case feedMemberTimeout:
		a.MemberTimeout(ctx, ev.community, ev.authz.user, ev.authz.until, ev.fromSnapshot)
	}
}
