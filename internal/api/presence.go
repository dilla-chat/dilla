package api

import (
	"context"
	"errors"
	"log/slog"
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
	repo store.Repository
	gw   PresenceGateway
	clk  clock.Clock
	log  *slog.Logger
	mu   sync.Mutex
	last map[id.ID]uint8

	// Profile scheduling is independent of the presence broadcast lock. All editors
	// (and all their devices) share one fixed window for each community.
	profileMu       sync.Mutex
	profilePending  map[id.ID]map[id.ID]struct{}
	profileEdits    map[id.ID][]time.Time
	profileSweep    time.Time
	profileWork     inFlight
	profileCtx      context.Context
	profileCancel   context.CancelFunc
	profileFlush    chan struct{}
	profileDraining bool
}

func NewPresenceService(repo store.Repository, gw PresenceGateway, clk clock.Clock, log *slog.Logger) *PresenceService {
	ctx, cancel := context.WithCancel(context.Background())
	return &PresenceService{
		repo: repo, gw: gw, clk: clk, log: log, last: map[id.ID]uint8{},
		profilePending: map[id.ID]map[id.ID]struct{}{}, profileEdits: map[id.ID][]time.Time{},
		profileCtx: ctx, profileCancel: cancel, profileFlush: make(chan struct{}),
	}
}

// ProfileChanged schedules a committed edit's hints. Six changed edits per rolling
// minute per editor may produce hints; further edits still save normally. Timers
// and rate accounting use wall time, independent of the presence/AFK instance clock.
// Each community receives at most one profile-caused op 54 per fixed ten-second
// window, regardless of the number of editors or devices.
func (p *PresenceService) ProfileChanged(_ context.Context, user id.ID) {
	if p == nil {
		return
	}
	p.profileMu.Lock()
	defer p.profileMu.Unlock()
	if p.profileDraining {
		return
	}
	now := time.Now()
	// Expire inactive editors opportunistically, without a goroutine per editor.
	if !now.Before(p.profileSweep) {
		for editor, edits := range p.profileEdits {
			if now.Sub(edits[len(edits)-1]) >= time.Minute {
				delete(p.profileEdits, editor)
			}
		}
		p.profileSweep = now.Add(time.Minute)
	}
	edits := p.profileEdits[user]
	for len(edits) > 0 && now.Sub(edits[0]) >= time.Minute {
		edits = edits[1:]
	}
	if len(edits) >= 6 {
		return
	}
	p.profileEdits[user] = append(edits, now)
	p.profileWork.start()
	go func() {
		defer p.profileWork.done()
		ctx, cancel := context.WithTimeout(p.profileCtx, 10*time.Second)
		defer cancel()
		if !p.profileVisible(ctx, user) {
			return
		}
		communities, err := p.repo.ListCommunitiesForUser(ctx, user)
		if err != nil {
			p.log.ErrorContext(ctx, "listing communities for profile hint failed", "user", user, "err", err)
			return
		}
		p.profileMu.Lock()
		defer p.profileMu.Unlock()
		for _, community := range communities {
			if editors, pending := p.profilePending[community.ID]; pending {
				editors[user] = struct{}{}
				continue
			}
			p.profilePending[community.ID] = map[id.ID]struct{}{user: {}}
			timer := clock.System().NewTimer(10 * time.Second)
			p.profileWork.start()
			go p.profileHint(community.ID, timer)
		}
	}()
}

func (p *PresenceService) profileVisible(ctx context.Context, user id.ID) bool {
	choice, err := p.repo.GetPresenceChoice(ctx, user)
	if err != nil {
		p.log.ErrorContext(ctx, "reading presence for profile hint failed", "user", user, "err", err)
		return false
	}
	return choice != 4 // Invisible edits appear only on the next normal member-list read.
}

func (p *PresenceService) profileHint(community id.ID, timer clock.Timer) {
	defer p.profileWork.done()
	defer timer.Stop()
	select {
	case <-timer.C():
	case <-p.profileFlush: // Flush pending hints while the gateway and store are still alive.
	case <-p.profileCtx.Done():
		return
	}
	p.profileMu.Lock()
	editors := p.profilePending[community]
	delete(p.profilePending, community)
	p.profileMu.Unlock()
	ctx, cancel := context.WithTimeout(p.profileCtx, 10*time.Second)
	defer cancel()
	// At least one editor must still belong to this live community and remain
	// visible. Membership of recipients is also read at delivery by StructureNotifier.
	if _, err := p.repo.GetCommunity(ctx, community); err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			p.log.ErrorContext(ctx, "reading community for profile hint failed", "community", community, "err", err)
		}
		return
	}
	for editor := range editors {
		if _, err := p.repo.GetMember(ctx, community, editor); err != nil {
			if !errors.Is(err, store.ErrNotFound) {
				p.log.ErrorContext(ctx, "reading membership for profile hint failed", "community", community, "user", editor, "err", err)
			}
			continue
		}
		if !p.profileVisible(ctx, editor) {
			continue
		}
		// Share StructureNotifier's store/encode error logging and live-only fan-out.
		NewStructureNotifier(p.gw, p.repo, p.log).Changed(ctx, community)
		return
	}
}

// Drain flushes pending hints and waits within ctx before Shutdown stops their
// gateway/repository. Late handlers cannot add work after draining begins.
func (p *PresenceService) Drain(ctx context.Context) error {
	if p == nil {
		return nil
	}
	p.profileMu.Lock()
	if !p.profileDraining {
		p.profileDraining = true
		close(p.profileFlush)
	}
	p.profileMu.Unlock()
	err := p.profileWork.wait(ctx)
	p.profileCancel()
	return err
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
