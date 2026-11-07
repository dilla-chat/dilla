package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/dilla-chat/dilla/internal/gateway"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/server"
	"github.com/dilla-chat/dilla/internal/store"
)

// Bot v2 Task 10: the Bot's state hints. A hint names a scope (a user, a channel, or the call in a
// voice channel) of the Bot's own community and nothing else. dillad raises that scope's stored
// revision and pushes op 63 bot_state_changed [scope_kind, scope_id, revision] to the scope's
// audience only: the user's connections, the connections subscribed to the channel's MLS group, or
// the call group's online devices. A client that sees a newer revision fetches once with a sealed
// read. Hints are not replayed: a client compares revisions at connect sync (GET /v1/bot/revisions).

// PostBotStateHint is POST /v1/bot-runtime/state-hints, body [scope_kind(0 user, 1 channel, 2
// call), scope_id]: 200 [revision]. A scope outside the Bot's community is 404.
func (d Deps) PostBotStateHint(w http.ResponseWriter, r *http.Request) {
	_, installation, err := readyBotSession(r, d)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	body, err := server.ReadBody(w, r, botEnvelopeSlack)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	invalid := server.Errorf(server.CodeInvalidRequest, "a hint is [scope_kind(0 user, 1 channel, 2 call), scope_id(bstr 16)]")
	elems, ok := rawArrayOf(body, 2)
	if !ok {
		server.WriteError(w, invalid)
		return
	}
	kind, okKind := rawUintOf(elems[0])
	scope, okScope := rawIDOf(elems[1])
	if !okKind || kind > uint64(store.BotScopeCall) || !okScope {
		server.WriteError(w, invalid)
		return
	}
	ctx := r.Context()
	cid := installation.CommunityID
	if err := botHintScopeInCommunity(ctx, d.Repo, cid, uint8(kind), scope); err != nil {
		server.WriteError(w, d.storeError(r, err))
		return
	}
	revision, err := d.Repo.BumpBotStateRevision(ctx, uint8(kind), scope)
	if err != nil {
		server.WriteError(w, d.storeError(r, err))
		return
	}
	d.pushBotStateHint(ctx, uint8(kind), scope, revision)
	d.write(w, r, http.StatusOK, []uint64{revision})
}

// botHintScopeInCommunity checks that the scope belongs to the community: a member (user), a live
// text channel (channel) or a live voice channel (call: the scope id is the voice channel, as GET
// /v1/bot/revisions reads it). Anything else is 404, so a Bot learns nothing of other communities.
func botHintScopeInCommunity(ctx context.Context, repo store.Repository, cid id.ID, kind uint8, scope id.ID) error {
	missing := server.Errorf(server.CodeNotFound, "no such scope in the Bot's community")
	if kind == store.BotScopeUser {
		if _, err := repo.GetMember(ctx, cid, scope); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return missing
			}
			return err
		}
		return nil
	}
	ch, err := repo.GetChannel(ctx, scope)
	if errors.Is(err, store.ErrNotFound) {
		return missing
	}
	if err != nil {
		return err
	}
	want := ChannelText
	if kind == store.BotScopeCall {
		want = ChannelVoice
	}
	if ch.CommunityID == nil || *ch.CommunityID != cid || ch.DeletedAt != nil || ch.Kind != want {
		return missing
	}
	return nil
}

// pushBotStateHint sends op 63 to the scope's audience. A failure to find the audience is logged:
// the revision is stored, and connect sync recovers a missed hint.
func (d Deps) pushBotStateHint(ctx context.Context, kind uint8, scope id.ID, revision uint64) {
	if d.BotRelay == nil {
		return
	}
	payload, err := gateway.BotStateChangedPayload(kind, scope, revision)
	if err != nil {
		d.logCtx(ctx, "encoding a Bot state hint failed", "err", err)
		return
	}
	f := gateway.Frame{Op: gateway.OpBotStateChanged, Payload: payload}
	switch kind {
	case store.BotScopeUser:
		d.BotRelay.DeliverUser(scope, f)
	case store.BotScopeChannel:
		groups, err := d.Repo.GroupsForTarget(ctx, scope, GroupText)
		if err != nil {
			d.logCtx(ctx, "reading a channel's group for a Bot state hint failed", "channel", scope, "err", err)
			return
		}
		for _, g := range groups {
			d.BotRelay.DeliverSubscribed(g.GroupID, f)
		}
	case store.BotScopeCall:
		calls, err := d.Repo.ListLiveVoiceSessions(ctx, scope)
		if err != nil {
			d.logCtx(ctx, "reading a channel's call for a Bot state hint failed", "channel", scope, "err", err)
			return
		}
		seen := map[id.ID]bool{}
		for _, call := range calls {
			if call.GroupID == nil {
				continue
			}
			for _, dev := range d.BotRelay.OnlineIn(*call.GroupID) {
				if !seen[dev.DeviceID] {
					seen[dev.DeviceID] = true
					d.BotRelay.DeliverDevice(dev.DeviceID, f)
				}
			}
		}
	}
}
