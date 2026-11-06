package api

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"slices"

	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/server"
	"github.com/dilla-chat/dilla/internal/store"
)

// The voice snapshot routes (web-3, L-HTTP-100 and L-HTTP-101): which call group a client joins or
// registers, and who is in a channel's live call. Both are reads: they need view_channel (or DM
// participation), never connect, and they change nothing.

// participantRow is one participants element: [user_id, device_id, flags].
type participantRow struct {
	_      struct{} `cbor:",toarray"`
	User   id.ID
	Device id.ID
	Flags  uint64
}

// channelCallResponse is GET /v1/channels/{id}/call's `[group_id|null, call_id|null, participants]`.
type channelCallResponse struct {
	_            struct{} `cbor:",toarray"`
	GroupID      *id.ID
	CallID       *id.ID
	Participants []participantRow
}

// communityCallRow is one element of GET /v1/communities/{id}/calls: `[channel_id, call_id,
// participants]`.
type communityCallRow struct {
	_            struct{} `cbor:",toarray"`
	ChannelID    id.ID
	CallID       id.ID
	Participants []participantRow
}

// registerReads mounts the two snapshot routes.
func (h *Calls) registerReads(mux *server.Mux) {
	mux.Socket("GET /v1/channels/{id}/call", http.HandlerFunc(h.channelCall))
	mux.Socket("GET /v1/communities/{id}/calls", http.HandlerFunc(h.communityCalls))
}

// viewableCallChannel is the {id} whose call a caller may read: a channel it can see (404 otherwise,
// byte-identical to an unknown id's answer) that carries calls (400 otherwise). It resolves
// permissions as channel does, without requiring connect: the snapshot is what a member who may
// only listen, or not join at all, is shown.
func (h *Calls) viewableCallChannel(r *http.Request) (store.ChannelRow, error) {
	s, err := enrolledSession(r)
	if err != nil {
		return store.ChannelRow{}, err
	}
	chID, err := server.PathID(r, "id")
	if err != nil {
		return store.ChannelRow{}, err
	}
	ch, err := h.repo.GetChannel(r.Context(), chID)
	if err != nil {
		return store.ChannelRow{}, notFound(err)
	}
	if err := h.botCanUseSession(r.Context(), s, ch); err != nil {
		return store.ChannelRow{}, err
	}
	bits, err := h.res.Resolve(r.Context(), s.UserID, ch)
	if err != nil {
		return store.ChannelRow{}, err
	}
	// Attacker: a member without view_channel probing a hidden channel (or a non-participant probing
	// a DM) gets the unknown-channel answer, before anything about the channel's kind or call.
	if !bits.Has(PermViewChannel) || ch.DeletedAt != nil {
		return store.ChannelRow{}, server.Errorf(server.CodeNotFound, "no such object")
	}
	if !CallGroupAllowed(ch) {
		return store.ChannelRow{}, server.Errorf(server.CodeInvalidRequest, "this channel carries no calls")
	}
	return ch, nil
}

// channelCall is GET /v1/channels/{id}/call (L-HTTP-100).
func (h *Calls) channelCall(w http.ResponseWriter, r *http.Request) {
	ch, err := h.viewableCallChannel(r)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	s, err := enrolledSession(r)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	if err := requireCommunityWire(r.Context(), h.repo, s.WireVersion, ch.CommunityID); err != nil {
		server.WriteError(w, err)
		return
	}
	resp := channelCallResponse{Participants: []participantRow{}}
	open, err := h.openCallGroup(r.Context(), ch)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	if open != nil {
		resp.GroupID = &open.GroupID
	}
	row, live, err := h.liveCallOf(r.Context(), ch, open)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	if live {
		callID, groupID := row.CallID, *row.GroupID
		resp.CallID, resp.GroupID = &callID, &groupID
		resp.Participants = h.participantRows(row)
	}
	if err := server.EncodeBody(w, http.StatusOK, resp); err != nil {
		h.log.WarnContext(r.Context(), "write call snapshot", "err", err)
	}
}

// communityCalls is GET /v1/communities/{id}/calls (L-HTTP-101): the live calls of the community's
// voice channels the caller may view, sorted by channel id.
func (h *Calls) communityCalls(w http.ResponseWriter, r *http.Request) {
	s, err := enrolledSession(r)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	cid, err := server.PathID(r, "id")
	if err != nil {
		server.WriteError(w, err)
		return
	}
	com, err := h.repo.GetCommunity(r.Context(), cid)
	if err != nil || com.DeletedAt != nil {
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			server.WriteError(w, err)
			return
		}
		server.WriteError(w, server.Errorf(server.CodeNotFound, "no such object"))
		return
	}
	if _, err := h.repo.GetMember(r.Context(), cid, s.UserID); err != nil {
		server.WriteError(w, notFound(err))
		return
	}
	if err := requireCommunityWire(r.Context(), h.repo, s.WireVersion, &cid); err != nil {
		server.WriteError(w, err)
		return
	}
	channels, err := h.repo.ListChannels(r.Context(), cid)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	rows := []communityCallRow{}
	for _, ch := range channels {
		if ch.Kind != ChannelVoice || !CallGroupAllowed(ch) {
			continue
		}
		bits, err := h.res.Resolve(r.Context(), s.UserID, ch)
		if err != nil {
			server.WriteError(w, err)
			return
		}
		if !bits.Has(PermViewChannel) {
			continue
		}
		row, live, err := h.liveCallOf(r.Context(), ch, nil)
		if err != nil {
			server.WriteError(w, err)
			return
		}
		if live {
			rows = append(rows, communityCallRow{ChannelID: ch.ID, CallID: row.CallID, Participants: h.participantRows(row)})
		}
	}
	slices.SortFunc(rows, func(a, b communityCallRow) int { return bytes.Compare(a.ChannelID[:], b.ChannelID[:]) })
	if err := server.EncodeBody(w, http.StatusOK, rows); err != nil {
		h.log.WarnContext(r.Context(), "write community calls", "err", err)
	}
}

// openCallGroup is the channel's newest open call group (callGroup), or nil when none is registered.
func (h *Calls) openCallGroup(ctx context.Context, ch store.ChannelRow) (*store.GroupRow, error) {
	groups, err := h.repo.GroupsForTarget(ctx, ch.ID, GroupCall)
	if err != nil {
		return nil, err
	}
	if len(groups) == 0 {
		return nil, nil
	}
	g := groups[len(groups)-1]
	return &g, nil
}

// liveCallOf is ch's live call: a voice session not ended whose call group is still open. A call is
// keyed by its call group's call id, which every call group of the channel shares (R9) and which the
// delivery service sets to the channel id; open, when known, names it. A live row whose group is
// closed or gone is over already (the next start ends it), so it is not reported.
func (h *Calls) liveCallOf(ctx context.Context, ch store.ChannelRow, open *store.GroupRow) (store.VoiceSessionRow, bool, error) {
	callID := ch.ID
	if open != nil {
		callID = callIDOfGroup(*open)
	}
	row, err := h.repo.GetVoiceSession(ctx, callID)
	if errors.Is(err, store.ErrNotFound) {
		return row, false, nil
	}
	if err != nil {
		return row, false, err
	}
	if row.Ended != nil || row.GroupID == nil || row.ChannelID != ch.ID {
		return row, false, nil
	}
	g, err := h.repo.GetGroup(ctx, *row.GroupID)
	if errors.Is(err, store.ErrNotFound) {
		return row, false, nil
	}
	if err != nil {
		return row, false, err
	}
	return row, g.ClosedAt == nil, nil
}

// participantRows is the live room's devices with their flags, at most
// livekit.max_voice_participants of them; none without the call events.
func (h *Calls) participantRows(row store.VoiceSessionRow) []participantRow {
	out := []participantRow{}
	if h.events == nil {
		return out
	}
	for _, p := range h.events.participantsIn(row.CallID, row.LivekitRoom) {
		if limit := h.cfg.MaxVoiceParticipants; limit > 0 && len(out) >= limit {
			break
		}
		out = append(out, participantRow{User: p.User, Device: p.Device, Flags: p.Flags})
	}
	return out
}
