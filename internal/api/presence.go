package api

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/jonasthim/dilla/internal/clock"
	"github.com/jonasthim/dilla/internal/gateway"
	"github.com/jonasthim/dilla/internal/id"
	"github.com/jonasthim/dilla/internal/server"
	"github.com/jonasthim/dilla/internal/store"
)

// PresenceGateway is the live connection authority. Storage holds only the account's choice.
type PresenceGateway interface {
	PresenceStatus(id.ID, uint8) uint8
	Activity(id.ID)
	DeliverUser(id.ID, gateway.Frame)
}

type PresenceService struct {
	repo           store.Repository
	gw             PresenceGateway
	clk            clock.Clock
	mu             sync.Mutex
	last           map[id.ID]uint8
	profilePending map[id.ID]struct{}
}

func NewPresenceService(repo store.Repository, gw PresenceGateway, clk clock.Clock) *PresenceService {
	return &PresenceService{repo: repo, gw: gw, clk: clk, last: map[id.ID]uint8{}, profilePending: map[id.ID]struct{}{}}
}

// ProfileChanged coalesces a user's committed profile edits over a fixed two-second window,
// across devices. The trailing hint sees the final profile and current membership; continuously
// editing cannot postpone delivery indefinitely or trigger more than one fan-out per window.
// DeliverUser sends to live connections only, with no persistence or offline replay.
func (p *PresenceService) ProfileChanged(ctx context.Context, user id.ID) {
	if p == nil {
		return
	}
	p.mu.Lock()
	if _, pending := p.profilePending[user]; pending {
		p.mu.Unlock()
		return
	}
	p.profilePending[user] = struct{}{}
	// This transport debounce follows wall time, independently of the instance clock
	// used for presence/AFK expiry (which the testhost can freeze or jump).
	timer := clock.System().NewTimer(2 * time.Second)
	p.mu.Unlock()
	ctx = context.WithoutCancel(ctx)
	go func() {
		defer timer.Stop()
		<-timer.C()
		p.mu.Lock()
		delete(p.profilePending, user)
		p.mu.Unlock()
		// Request cancellation must not lose the committed hint, but failed storage must not
		// leave its fan-out running without a bound.
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		communities, err := p.repo.ListCommunitiesForUser(ctx, user)
		if err != nil {
			return
		}
		for _, community := range communities {
			payload, err := gateway.CommunityChangedPayload(community.ID)
			if err != nil {
				continue
			}
			members, err := communityMemberIDs(ctx, p.repo, community.ID)
			if err != nil {
				continue
			}
			frame := gateway.Frame{Op: gateway.OpCommunityChanged, Payload: payload}
			seen := make(map[id.ID]struct{}, len(members))
			for _, member := range members {
				if _, sent := seen[member]; sent {
					continue
				}
				seen[member] = struct{}{}
				p.gw.DeliverUser(member, frame)
			}
		}
	}()
}

func (p *PresenceService) status(ctx context.Context, user id.ID) (uint8, error) {
	choice, err := p.repo.GetPresenceChoice(ctx, user)
	if err != nil {
		return 0, err
	}
	return p.gw.PresenceStatus(user, choice), nil
}

// Changed sends only a changed visible state, to users who currently share a community.
// Every recipient is read after the membership change, so a removed member receives no update.
func (p *PresenceService) Changed(ctx context.Context, user id.ID) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	status, err := p.status(ctx, user)
	if err != nil {
		return
	}
	if old, ok := p.last[user]; ok && old == status {
		return
	}
	communities, err := p.repo.ListCommunitiesForUser(ctx, user)
	if err != nil {
		return
	}
	// The account's other devices need the event even without a shared community (for DND alerts).
	audience := map[id.ID]struct{}{user: {}}
	for _, community := range communities {
		members, err := communityMemberIDs(ctx, p.repo, community.ID)
		if err != nil {
			return
		}
		for _, member := range members {
			audience[member] = struct{}{}
		}
	}
	timestamp := uint64(p.clk.Now().Unix()) //nolint:gosec // G115: an instance clock's Unix second, never before 1970
	payload, err := gateway.PresencePayload(user, uint64(status), timestamp, "")
	if err != nil {
		return
	}
	p.last[user] = status
	frame := gateway.Frame{Op: gateway.OpPresence, Payload: payload}
	for recipient := range audience {
		p.gw.DeliverUser(recipient, frame)
	}
}

func (d Deps) PatchPresence(w http.ResponseWriter, r *http.Request) {
	if d.Presence == nil {
		server.WriteError(w, notImplemented("presence unavailable"))
		return
	}
	var req struct {
		_      struct{} `cbor:",toarray"`
		Choice uint8
	}
	if err := server.DecodeBody(w, r, maxCBORBody, &req); err != nil {
		server.WriteError(w, err)
		return
	}
	if req.Choice < 1 || req.Choice > 4 {
		server.WriteError(w, server.Errorf(server.CodeInvalidRequest, "invalid presence choice"))
		return
	}
	sess, ok := session(r)
	if !ok {
		server.WriteError(w, server.Errorf(server.CodeUnauthenticated, ""))
		return
	}
	if err := d.Repo.SetPresenceChoice(r.Context(), sess.UserID, req.Choice); err != nil {
		server.WriteError(w, d.storeError(r, err))
		return
	}
	d.Presence.Changed(r.Context(), sess.UserID)
	// A choice can change while the visible state stays Idle (Online on an inactive device).
	// The account's other devices still need to re-read the choice for alert suppression.
	if status, err := d.Presence.status(r.Context(), sess.UserID); err == nil {
		timestamp := uint64(d.Clock.Now().Unix()) //nolint:gosec // G115: an instance clock's Unix second, never before 1970
		if payload, err := gateway.PresencePayload(sess.UserID, uint64(status), timestamp, ""); err == nil {
			d.Presence.gw.DeliverUser(sess.UserID, gateway.Frame{Op: gateway.OpPresence, Payload: payload})
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func (d Deps) PresenceActivity(w http.ResponseWriter, r *http.Request) {
	if d.Presence == nil {
		server.WriteError(w, notImplemented("presence unavailable"))
		return
	}
	sess, ok := session(r)
	if !ok {
		server.WriteError(w, server.Errorf(server.CodeUnauthenticated, ""))
		return
	}
	d.Presence.gw.Activity(sess.DeviceID)
	w.WriteHeader(http.StatusNoContent)
}

// PresenceSnapshot authenticates membership before reading any other user's status.
func (d Deps) PresenceSnapshot(w http.ResponseWriter, r *http.Request) {
	if d.Presence == nil {
		server.WriteError(w, notImplemented("presence unavailable"))
		return
	}
	sess, ok := session(r)
	if !ok {
		server.WriteError(w, server.Errorf(server.CodeUnauthenticated, ""))
		return
	}
	community, err := id.Parse(r.PathValue("community_id"))
	if err != nil {
		server.WriteError(w, server.Errorf(server.CodeInvalidRequest, "invalid community"))
		return
	}
	if _, err := d.Repo.GetMember(r.Context(), community, sess.UserID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			server.WriteError(w, server.Errorf(server.CodeForbidden, ""))
		} else {
			server.WriteError(w, d.storeError(r, err))
		}
		return
	}
	if _, err := d.Repo.GetCommunity(r.Context(), community); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			server.WriteError(w, server.Errorf(server.CodeForbidden, ""))
		} else {
			server.WriteError(w, d.storeError(r, err))
		}
		return
	}
	var out [][]any
	after := id.ID{}
	for {
		rows, err := d.Repo.ListMembersOfCommunity(r.Context(), community, after, memberPage)
		if err != nil {
			server.WriteError(w, d.storeError(r, err))
			return
		}
		users := make([]id.ID, len(rows))
		for i, member := range rows {
			users[i] = member.UserID
		}
		choices, err := d.Repo.ListPresenceChoices(r.Context(), users)
		if err != nil {
			server.WriteError(w, d.storeError(r, err))
			return
		}
		for _, member := range rows {
			choice := choices[member.UserID]
			if choice == 0 {
				choice = 1
			}
			status := d.Presence.gw.PresenceStatus(member.UserID, choice)
			out = append(out, []any{member.UserID, status})
		}
		if len(rows) < int(memberPage) {
			break
		}
		after = rows[len(rows)-1].UserID
	}
	if out == nil {
		out = [][]any{}
	}
	d.write(w, r, http.StatusOK, out)
}
