package api

import (
	"bytes"
	"crypto/ed25519"
	"errors"
	"math"
	"net/http"
	"strconv"

	"github.com/jonasthim/dilla/internal/auth"
	botaccess "github.com/jonasthim/dilla/internal/bot/access"
	"github.com/jonasthim/dilla/internal/bot/protocol"
	"github.com/jonasthim/dilla/internal/id"
	"github.com/jonasthim/dilla/internal/server"
	"github.com/jonasthim/dilla/internal/store"
)

const moderationRemovalPageLimit = 250

type moderationRemovalAPI struct{ deps Deps }

type moderationRemovalRequest struct {
	_         struct{} `cbor:",toarray"`
	Proof     []byte
	Signature []byte
}

type moderationRemovalResponse struct {
	_        struct{} `cbor:",toarray"`
	ActionID id.ID
	Revision uint64
}

type moderationRemovalPageResponse struct {
	_            struct{} `cbor:",toarray"`
	Rows         [][]any
	NextRevision uint64
}

func registerModerationRemovals(mux *server.Mux, d Deps) {
	h := &moderationRemovalAPI{deps: d}
	enrolled := func(f http.HandlerFunc) http.Handler { return d.Sessions.Middleware(f, auth.ScopeEnrolled) }
	mux.Handle("POST /v1/groups/{group}/moderation/removals", enrolled(dsMeter(d.Limiter, dsClassWrite, h.accept)))
	mux.Handle("GET /v1/groups/{group}/moderation/removals", enrolled(dsMeter(d.Limiter, dsClassRead, h.list)))
}

func (h *moderationRemovalAPI) accept(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	groupID, err := server.PathID(r, "group")
	if err != nil {
		server.WriteError(w, err)
		return
	}
	var body moderationRemovalRequest
	if err := server.DecodeBody(w, r, 512, &body); err != nil {
		server.WriteError(w, err)
		return
	}
	if len(body.Signature) != ed25519.SignatureSize {
		server.WriteError(w, server.Errorf(server.CodeInvalidRequest, "signature must be 64 bytes"))
		return
	}
	proof, err := protocol.DecodeRemovalProof(body.Proof)
	if err != nil {
		server.WriteError(w, server.Errorf(server.CodeInvalidRequest, "invalid removal proof"))
		return
	}
	s, err := enrolledSession(r)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	if err := requireBotWire(s.WireVersion); err != nil {
		server.WriteError(w, err)
		return
	}
	group, err := h.deps.Repo.GetGroup(r.Context(), groupID)
	if err != nil {
		server.WriteError(w, notFound(err))
		return
	}
	if group.ClosedAt != nil || group.Kind != GroupText || group.CommunityID == nil {
		server.WriteError(w, notFound(store.ErrNotFound))
		return
	}
	communityID := *group.CommunityID
	requestNow := h.deps.Clock.Now().Unix()
	if requestNow < 0 {
		server.WriteError(w, server.Errorf(server.CodeUnavailable, "server clock is invalid"))
		return
	}
	requestNowUnix, ok := checkedUint64(requestNow)
	if !ok {
		server.WriteError(w, server.Errorf(server.CodeUnavailable, "server clock is unavailable"))
		return
	}
	if err := protocol.ValidateRemovalContext(proof, communityID, groupID, s.UserID, s.DeviceID, requestNowUnix); errors.Is(err, protocol.ErrContext) {
		server.WriteError(w, server.Errorf(server.CodeForbidden, "removal proof does not match this session and group"))
		return
	} else if err != nil && !errors.Is(err, protocol.ErrExpired) {
		server.WriteError(w, server.Errorf(server.CodeInvalidRequest, "invalid removal proof context"))
		return
	}
	device, err := h.deps.Repo.GetDevice(r.Context(), s.DeviceID)
	if err != nil || device.UserID != s.UserID || device.RevokedAt != nil || device.QuarantinedAt != nil || len(device.DSKPub) != ed25519.PublicKeySize {
		server.WriteError(w, server.Errorf(server.CodeUnauthenticated, "registered session device unavailable"))
		return
	}
	preimage, err := protocol.RemovalPreimage(proof)
	if err != nil || !ed25519.Verify(ed25519.PublicKey(device.DSKPub), preimage, body.Signature) {
		server.WriteError(w, server.Errorf(server.CodeUnauthenticated, "invalid removal signature"))
		return
	}
	// An exact replay returns the immutable original receipt even after the
	// acceptance window has expired or the actor's role has since changed.
	prior, priorErr := h.deps.Repo.GetModerationRemoval(r.Context(), groupID, proof.RequestID)
	if priorErr == nil {
		if bytes.Equal(prior.Proof, body.Proof) && bytes.Equal(prior.Signature, body.Signature) {
			writeRemovalResult(w, prior)
			return
		}
		server.WriteError(w, removalConflict())
		return
	}
	if !errors.Is(priorErr, store.ErrNotFound) {
		server.WriteError(w, h.deps.storeError(r, priorErr))
		return
	}
	if r.URL.Query().Get("receipt_only") == "1" {
		server.WriteError(w, server.Errorf(server.CodeNotFound, "moderation receipt not found"))
		return
	}
	now := h.deps.Clock.Now().Unix()
	nowUnix, ok := checkedUint64(now)
	if !ok || protocol.ValidateRemovalContext(proof, communityID, groupID, s.UserID, s.DeviceID, nowUnix) != nil {
		server.WriteError(w, server.Errorf(server.CodeRemovalProofExpired, "removal proof has expired or exceeds the acceptance window"))
		return
	}
	if user, err := h.deps.Repo.GetUser(r.Context(), s.UserID); err != nil {
		server.WriteError(w, h.deps.storeError(r, err))
		return
	} else if user.Kind == 1 {
		if err := h.requireBotRemoval(r, h.deps.Repo, s, communityID); err != nil {
			server.WriteError(w, err)
			return
		}
	}
	actionID := id.New()
	var accepted store.ModerationRemovalRow
	created := false
	err = h.deps.Repo.Tx(r.Context(), func(tx store.Repository) error {
		if err := tx.LockCommunity(r.Context(), communityID); err != nil {
			return notFound(err)
		}
		lockedGroup, err := tx.GetGroup(r.Context(), groupID)
		if err != nil {
			return notFound(err)
		}
		if lockedGroup.ClosedAt != nil || lockedGroup.Kind != GroupText || lockedGroup.CommunityID == nil || *lockedGroup.CommunityID != communityID {
			return notFound(store.ErrNotFound)
		}
		prior, err := tx.GetModerationRemoval(r.Context(), groupID, proof.RequestID)
		if err == nil {
			if !bytes.Equal(prior.Proof, body.Proof) || !bytes.Equal(prior.Signature, body.Signature) {
				return removalConflict()
			}
			accepted = prior
			return nil
		}
		if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		lockedDevice, err := tx.GetDevice(r.Context(), s.DeviceID)
		if err != nil || lockedDevice.UserID != s.UserID || lockedDevice.RevokedAt != nil || lockedDevice.QuarantinedAt != nil ||
			!bytes.Equal(lockedDevice.DSKPub, device.DSKPub) || !ed25519.Verify(ed25519.PublicKey(lockedDevice.DSKPub), preimage, body.Signature) {
			return server.Errorf(server.CodeUnauthenticated, "registered session device unavailable")
		}
		actorUser, err := tx.GetUser(r.Context(), s.UserID)
		if err != nil {
			return err
		}
		if actorUser.Kind == 1 {
			if err := h.requireBotRemoval(r, tx, s, communityID); err != nil {
				return err
			}
		}
		targetUser, err := authorizeMessageRemoval(r, tx, communityID, groupID, lockedGroup.TargetID, s.UserID, proof.TargetSequence)
		if err != nil {
			return err
		}
		now := h.deps.Clock.Now().Unix()
		nowUnix, ok := checkedUint64(now)
		if !ok || protocol.ValidateRemovalContext(proof, communityID, groupID, s.UserID, s.DeviceID, nowUnix) != nil {
			return server.Errorf(server.CodeRemovalProofExpired, "removal proof has expired or exceeds the acceptance window")
		}
		accepted, err = tx.AcceptModerationRemoval(r.Context(), store.ModerationRemovalRow{
			RequestID: proof.RequestID, ActionID: actionID, CommunityID: communityID, GroupID: groupID,
			TargetSequence: proof.TargetSequence, Proof: append([]byte(nil), body.Proof...),
			Signature: append([]byte(nil), body.Signature...), AcceptedAt: now,
		})
		if err != nil {
			return notFound(err)
		}
		created = true
		event := protocol.Event{ID: actionID, CommunityID: communityID, Timestamp: nowUnix, Kind: protocol.EventModeration,
			Data: []any{actionID, proof.RequestID, s.UserID, s.DeviceID, targetUser, groupID, proof.TargetSequence, uint64(4), nil}}
		_, err = tx.AppendBotStructuralEvent(r.Context(), event)
		return err
	})
	if err != nil {
		server.WriteError(w, h.deps.storeError(r, err))
		return
	}
	if created && h.deps.MessageDeleted != nil {
		h.deps.MessageDeleted(groupID, proof.TargetSequence, accepted.AcceptedAt)
	}
	writeRemovalResult(w, accepted)
}

func authorizeMessageRemoval(r *http.Request, repo store.Repository, communityID, groupID, channelID, actorID id.ID, targetSeq uint64) (id.ID, error) {
	if targetSeq == 0 || targetSeq > math.MaxInt64 {
		return id.ID{}, server.Errorf(server.CodeInvalidRequest, "invalid target sequence")
	}
	if _, err := repo.GetMember(r.Context(), communityID, actorID); err != nil {
		return id.ID{}, notFound(err)
	}
	actor, err := LoadSnapshot(r.Context(), repo, communityID, actorID, &channelID)
	if err != nil {
		return id.ID{}, err
	}
	if !actor.Resolve(actorID).Has(PermManageMessages) {
		return id.ID{}, server.Errorf(server.CodeForbidden, "manage messages permission required")
	}
	message, err := repo.GetAppMessage(r.Context(), groupID, targetSeq)
	if err != nil {
		return id.ID{}, notFound(err)
	}
	if message.DeletedAt != nil {
		return id.ID{}, notFound(store.ErrNotFound)
	}
	uploader, err := repo.GetDevice(r.Context(), message.UploaderDevice)
	if err != nil {
		return id.ID{}, err
	}
	actorTop, _ := actor.Highest(actorID)
	targetTop := uint64(0)
	if _, memberErr := repo.GetMember(r.Context(), communityID, uploader.UserID); memberErr == nil {
		target, err := LoadSnapshot(r.Context(), repo, communityID, uploader.UserID, nil)
		if err != nil {
			return id.ID{}, err
		}
		targetTop, _ = target.Highest(uploader.UserID)
	} else if !errors.Is(memberErr, store.ErrNotFound) {
		return id.ID{}, memberErr
	}
	if actorTop <= targetTop {
		return id.ID{}, server.Errorf(server.CodeForbidden, "target is not below the moderator in the community hierarchy")
	}
	return uploader.UserID, nil
}

func (h *moderationRemovalAPI) requireBotRemoval(r *http.Request, repo store.Repository, s auth.Session, communityID id.ID) error {
	if err := h.requireBotFeed(r, repo, s, communityID); err != nil {
		return err
	}
	installation, err := repo.GetActiveBotInstallationByCommunity(r.Context(), communityID)
	if err != nil || installation.Capabilities&uint16(botCapRemoval) == 0 {
		return server.Errorf(server.CodeForbidden, "ready removal-capable Bot installation required")
	}
	return nil
}

// Bot requests must come from the ready installation bound to this community;
// human clients read the proof feed through current group membership instead.
func (h *moderationRemovalAPI) requireBotFeed(r *http.Request, repo store.Repository, s auth.Session, communityID id.ID) error {
	if err := requireBotWire(s.WireVersion); err != nil {
		return err
	}
	binding, _, err := botaccess.ForUserCommunity(r.Context(), repo, s.UserID, communityID)
	if err != nil || binding.State != store.BotInstallationReady {
		return server.Errorf(server.CodeForbidden, "ready removal-capable Bot installation required")
	}
	installation, err := repo.GetActiveBotInstallationByCommunity(r.Context(), communityID)
	if err != nil || installation.UserID == nil || *installation.UserID != s.UserID || installation.DeviceID == nil || *installation.DeviceID != s.DeviceID {
		return server.Errorf(server.CodeForbidden, "ready Bot installation required")
	}
	return nil
}

func (h *moderationRemovalAPI) list(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	groupID, err := server.PathID(r, "group")
	if err != nil {
		server.WriteError(w, err)
		return
	}
	after, limit, err := parseModerationRemovalQuery(r)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	s, err := enrolledSession(r)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	group, err := h.deps.Repo.GetGroup(r.Context(), groupID)
	if err != nil {
		server.WriteError(w, notFound(err))
		return
	}
	if group.Kind != GroupText || group.CommunityID == nil {
		server.WriteError(w, notFound(store.ErrNotFound))
		return
	}
	members, err := h.deps.Repo.ListMembers(r.Context(), groupID)
	if err != nil {
		server.WriteError(w, h.deps.storeError(r, err))
		return
	}
	member := false
	for _, m := range members {
		if m.DeviceID == s.DeviceID && m.UserID == s.UserID {
			member = true
			break
		}
	}
	if !member {
		server.WriteError(w, server.Errorf(server.CodeNotFound, "moderation removal feed not found"))
		return
	}
	// Every current group member needs the proof feed to recover permanent local
	// tombstones. The group leaf check above is the same authorization boundary as
	// message history; bot capability gates remain on accepting removals.
	page, err := h.deps.Repo.ListModerationRemovals(r.Context(), groupID, after, limit)
	if err != nil {
		server.WriteError(w, h.deps.storeError(r, err))
		return
	}
	rows := make([][]any, 0, len(page.Rows))
	for _, row := range page.Rows {
		acceptedAt, ok := checkedUint64(row.AcceptedAt)
		if !ok {
			server.WriteError(w, server.Unavailable(1000, "stored moderation removal has an invalid timestamp"))
			return
		}
		rows = append(rows, []any{row.Revision, row.Proof, row.Signature, acceptedAt})
	}
	if err := server.EncodeBody(w, http.StatusOK, moderationRemovalPageResponse{Rows: rows, NextRevision: page.NextRevision}); err != nil {
		h.deps.logf(r, "api: encode moderation removal page", "err", err)
	}
}

func writeRemovalResult(w http.ResponseWriter, row store.ModerationRemovalRow) {
	if err := server.EncodeBody(w, http.StatusOK, moderationRemovalResponse{ActionID: row.ActionID, Revision: row.Revision}); err != nil {
		server.WriteError(w, err)
	}
}

func removalConflict() *server.Error {
	return server.Errorf(server.CodeCommitConflict, "request id was already used for another removal")
}

func parseModerationRemovalQuery(r *http.Request) (uint64, int32, error) {
	query := r.URL.Query()
	for key, values := range query {
		if key != "after" && key != "limit" || len(values) != 1 {
			return 0, 0, server.Errorf(server.CodeInvalidRequest, "invalid moderation removal query")
		}
	}
	after := uint64(0)
	if values, ok := query["after"]; ok {
		value, err := strconv.ParseUint(values[0], 10, 64)
		if err != nil || value > math.MaxInt64 {
			return 0, 0, server.Errorf(server.CodeInvalidRequest, "invalid removal cursor")
		}
		after = value
	}
	limit := uint64(100)
	if values, ok := query["limit"]; ok {
		value, err := strconv.ParseUint(values[0], 10, 64)
		if err != nil || value < 1 || value > moderationRemovalPageLimit {
			return 0, 0, server.Errorf(server.CodeInvalidRequest, "limit must be between 1 and 250")
		}
		limit = value
	}
	pageSize, ok := checkedInt32(limit)
	if !ok {
		return 0, 0, server.Errorf(server.CodeInvalidRequest, "limit exceeds the supported range")
	}
	return after, pageSize, nil
}
