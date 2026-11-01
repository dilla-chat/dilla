package api

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"errors"
	"math"
	"net/http"

	"github.com/jonasthim/dilla/internal/cborx"
	"github.com/jonasthim/dilla/internal/ds"
	"github.com/jonasthim/dilla/internal/id"
	"github.com/jonasthim/dilla/internal/server"
	"github.com/jonasthim/dilla/internal/store"
)

type botInteractionAdmissionRequest struct {
	_                 struct{} `cbor:",toarray"`
	RequestID         id.ID
	GroupID           id.ID
	ParticipantUser   id.ID
	ParticipantDevice id.ID
}

type botInteractionAdmissionPayload struct {
	_                   struct{} `cbor:",toarray"`
	Version             uint64
	AdmissionID         id.ID
	InstanceID          id.ID
	InstallationID      id.ID
	CommunityID         id.ID
	BotUserID           id.ID
	BotDeviceID         id.ID
	ParticipantUserID   id.ID
	ParticipantDeviceID id.ID
	GroupID             id.ID
	EligibilityRevision uint64
	NotAfter            uint64
}

func (d Deps) registerBotInteractionAdmissions(m *server.Mux) {
	botSession := BotSessionRoute(d.Sessions, d.Limiter)
	m.Handle("POST /v1/bot-runtime/installations/{installation}/interaction-admissions", botSession("POST /v1/bot-runtime/installations/{installation}/interaction-admissions", http.HandlerFunc(d.issueBotInteractionAdmission)))
	m.Handle("GET /v1/bot-runtime/installations/{installation}/interaction-admissions/{request}/group-absence", botSession("GET /v1/bot-runtime/installations/{installation}/interaction-admissions/{request}/group-absence", http.HandlerFunc(d.proveBotInteractionGroupAbsent)))
	d.registerBotInteractionContext(m)
}

// proveBotInteractionGroupAbsent is a narrowly scoped retry-safety proof. It is
// available only for an unexpired admission issued to this exact Bot session,
// and only reports whether that admission's exact group id is absent.
func (d Deps) proveBotInteractionGroupAbsent(w http.ResponseWriter, r *http.Request) {
	s, ok := session(r)
	if !ok || s.Bot == nil {
		server.WriteError(w, server.Errorf(server.CodeForbidden, "Bot session required"))
		return
	}
	installationID, err := server.PathID(r, "installation")
	if err != nil {
		server.WriteError(w, err)
		return
	}
	requestID, err := server.PathID(r, "request")
	if err != nil {
		server.WriteError(w, err)
		return
	}
	if installationID != s.Bot.InstallationID || s.Bot.State != store.BotInstallationReady {
		server.WriteError(w, server.Errorf(server.CodeForbidden, "session is not bound to this ready Bot installation"))
		return
	}
	installation, err := d.Repo.GetBotInstallation(r.Context(), installationID)
	if err != nil {
		server.WriteError(w, d.storeError(r, err))
		return
	}
	if installation.CommunityID != s.Bot.CommunityID || installation.State != store.BotInstallationReady || installation.RemovedAt != nil || installation.UserID == nil || installation.DeviceID == nil || *installation.UserID != s.UserID || *installation.DeviceID != s.DeviceID || installation.CredentialVerifiedAt == nil {
		server.WriteError(w, server.Errorf(server.CodeForbidden, "session is not bound to this current Bot installation"))
		return
	}
	receipts, ok := d.Repo.(store.BotInteractionAdmissionReceipts)
	if !ok {
		server.WriteError(w, server.Errorf(server.CodeUnavailable, "interaction admission receipts unavailable"))
		return
	}
	receipt, err := receipts.GetBotInteractionAdmissionReceipt(r.Context(), installationID, requestID)
	if err != nil {
		server.WriteError(w, d.storeError(r, err))
		return
	}
	var proof botInteractionAdmissionPayload
	if err := cborx.Unmarshal(receipt.Payload, &proof); err != nil {
		server.WriteError(w, server.Errorf(server.CodeForbidden, "interaction admission is unavailable or expired"))
		return
	}
	nowUnix, nowOK := checkedUint64(d.Clock.Now().Unix())
	proofNotAfter, proofNotAfterOK := checkedInt64(proof.NotAfter)
	if !nowOK || !proofNotAfterOK || receipt.InstallationID != installationID || receipt.RequestID != requestID || len(receipt.Signature) != ed25519.SignatureSize || len(d.ExternalSenderPub) != ed25519.PublicKeySize || proof.Version != 1 || proof.AdmissionID.IsZero() || proof.InstanceID != d.Instance.InstanceID || proof.InstallationID != installationID || proof.CommunityID != installation.CommunityID || proof.BotUserID != s.UserID || proof.BotDeviceID != s.DeviceID || proof.NotAfter <= nowUnix || receipt.NotAfter != proofNotAfter || !ed25519.Verify(ed25519.PublicKey(d.ExternalSenderPub), append([]byte("dilla bot interaction admission v1\x00"), receipt.Payload...), receipt.Signature) {
		server.WriteError(w, server.Errorf(server.CodeForbidden, "interaction admission is unavailable or expired"))
		return
	}
	tuple, err := cborx.Marshal([]any{proof.GroupID, proof.ParticipantUserID, proof.ParticipantDeviceID})
	if err != nil {
		server.WriteError(w, server.Errorf(server.CodeUnavailable, "interaction admission validation unavailable"))
		return
	}
	hash := sha256.Sum256(tuple)
	if !bytes.Equal(hash[:], receipt.RequestHash) {
		server.WriteError(w, server.Errorf(server.CodeForbidden, "interaction admission binding is invalid"))
		return
	}
	_, err = d.Repo.GetGroup(r.Context(), proof.GroupID)
	if err == nil {
		_ = server.EncodeBody(w, http.StatusOK, false)
		return
	}
	if !errors.Is(err, store.ErrNotFound) {
		server.WriteError(w, d.storeError(r, err))
		return
	}
	if err := server.EncodeBody(w, http.StatusOK, true); err != nil {
		server.WriteError(w, err)
	}
}

func (d Deps) issueBotInteractionAdmission(w http.ResponseWriter, r *http.Request) {
	s, ok := session(r)
	if !ok || s.Bot == nil {
		server.WriteError(w, server.Errorf(server.CodeForbidden, "Bot session required"))
		return
	}
	if err := requireBotWire(s.WireVersion); err != nil {
		server.WriteError(w, err)
		return
	}
	installationID, err := server.PathID(r, "installation")
	if err != nil {
		server.WriteError(w, err)
		return
	}
	var req botInteractionAdmissionRequest
	if err := server.DecodeBody(w, r, maxCBORBody, &req); err != nil {
		refuseCBOR(w, r, err)
		return
	}
	if req.RequestID.IsZero() || req.GroupID.IsZero() || req.ParticipantUser.IsZero() || req.ParticipantDevice.IsZero() {
		server.WriteError(w, server.Errorf(server.CodeInvalidRequest, "admission identifiers must be nonzero"))
		return
	}
	if d.SignBotInteractionAdmission == nil || d.DeviceLists == nil {
		server.WriteError(w, server.Errorf(server.CodeUnavailable, "interaction admission signing or device-list verification unavailable"))
		return
	}
	requestTuple, err := cborx.Marshal([]any{req.GroupID, req.ParticipantUser, req.ParticipantDevice})
	if err != nil {
		server.WriteError(w, server.Errorf(server.CodeInvalidRequest, "invalid admission request"))
		return
	}
	requestHash := sha256.Sum256(requestTuple)
	initial, err := d.Repo.GetBotInstallation(r.Context(), installationID)
	if err != nil {
		server.WriteError(w, d.storeError(r, err))
		return
	}
	var payload []byte
	var signature []byte
	err = d.withDeviceListTransaction(r.Context(), func(ctx context.Context, tx store.Repository) error {
		r := r.WithContext(ctx)
		// Use the community-before-installation lock order shared by lifecycle
		// mutations so PostgreSQL transactions do not deadlock on admission.
		if err := tx.LockCommunity(r.Context(), initial.CommunityID); err != nil {
			return err
		}
		if err := tx.LockBotInstallation(r.Context(), installationID); err != nil {
			return err
		}
		installation, err := tx.GetBotInstallation(r.Context(), installationID)
		if err != nil {
			return err
		}
		if installation.ID != s.Bot.InstallationID || installation.CommunityID != s.Bot.CommunityID || installation.State != store.BotInstallationJoining && installation.State != store.BotInstallationReady || installation.UserID == nil || installation.DeviceID == nil || installation.CredentialVerifiedAt == nil || *installation.UserID != s.UserID || *installation.DeviceID != s.DeviceID {
			return server.Errorf(server.CodeForbidden, "session is not bound to this current installation")
		}
		if s.Bot.State != installation.State || s.Bot.CommunityID != installation.CommunityID {
			return server.Errorf(server.CodeForbidden, "Bot session installation state is stale")
		}
		// Disable and tombstone mutations take this same user-row lock before
		// their eligibility trigger advances the revision fence.
		if err := tx.LockUserForDeviceRegistration(r.Context(), req.ParticipantUser); err != nil {
			return err
		}
		// Lock eligibility before reading device/list state. Device and signed-list
		// triggers use this row, so a concurrent change either precedes these reads
		// or advances the fence after this transaction commits.
		eligibility, ok := tx.(store.BotInteractionEligibility)
		if !ok {
			return server.Errorf(server.CodeUnavailable, "interaction eligibility fencing unavailable")
		}
		revision, err := eligibility.GetBotInteractionEligibilityRevision(r.Context(), installation.CommunityID, req.ParticipantUser)
		if err != nil {
			return err
		}
		if revision == 0 {
			return server.Errorf(server.CodeUnavailable, "participant eligibility revision is not established")
		}
		participant, err := tx.GetUser(r.Context(), req.ParticipantUser)
		if err != nil || participant.Kind != kindHuman || participant.DisabledAt != nil || participant.DeletedAt != nil {
			return server.Errorf(server.CodeForbidden, "active human participant required")
		}
		if _, err := tx.GetMember(r.Context(), installation.CommunityID, participant.ID); err != nil {
			return server.Errorf(server.CodeForbidden, "participant is not a current community member")
		}
		ban, err := tx.GetBan(r.Context(), installation.CommunityID, participant.ID)
		if err == nil && (ban.Expires == nil || *ban.Expires > d.Clock.Now().Unix()) {
			return server.Errorf(server.CodeForbidden, "participant is banned")
		}
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		device, err := tx.GetDevice(r.Context(), req.ParticipantDevice)
		if err != nil || !ds.ActiveHumanInteractionDevice(participant.ID, device) {
			return server.Errorf(server.CodeForbidden, "participant device is not active")
		}
		list, err := tx.GetDeviceList(r.Context(), participant.ID)
		if err != nil {
			return server.Errorf(server.CodeForbidden, "participant has no active signed device list")
		}
		entries, err := d.DeviceLists.Verify(r.Context(), list.Blob, participant.SSKPub, participant.ID)
		if err != nil {
			return server.Errorf(server.CodeForbidden, "participant device list is invalid")
		}
		if !ds.EligibleHumanInteractionDevice(participant.ID, device, entries) {
			return server.Errorf(server.CodeForbidden, "participant device is not in the active signed device list")
		}
		now := d.Clock.Now().Unix()
		nowUnix, nowOK := checkedUint64(now)
		if !nowOK || now > math.MaxInt64-30 {
			return server.Errorf(server.CodeUnavailable, "server time outside admission range")
		}
		receipts, ok := tx.(store.BotInteractionAdmissionReceipts)
		if !ok {
			return server.Errorf(server.CodeUnavailable, "interaction admission receipts unavailable")
		}
		if err := receipts.CompactExpiredBotInteractionAdmissionReceipts(r.Context(), installation.ID, now); err != nil {
			return err
		}
		receipt, receiptErr := receipts.GetBotInteractionAdmissionReceipt(r.Context(), installation.ID, req.RequestID)
		reissue := false
		if receiptErr == nil {
			if !bytes.Equal(receipt.RequestHash, requestHash[:]) {
				return server.WithStatus(http.StatusConflict, server.Errorf(server.CodeInvalidRequest, "request id was already used for a different admission"))
			}
			if len(receipt.Payload) == 0 && len(receipt.Signature) == 0 {
				reissue = true
			} else {
				var prior botInteractionAdmissionPayload
				if err := cborx.Unmarshal(receipt.Payload, &prior); err != nil || prior.EligibilityRevision != revision || prior.NotAfter <= nowUnix {
					return server.WithStatus(http.StatusConflict, server.Errorf(server.CodeInvalidRequest, "admission receipt is no longer current"))
				}
				payload, signature = receipt.Payload, receipt.Signature
				return nil
			}
		}
		if receiptErr != nil && !errors.Is(receiptErr, store.ErrNotFound) {
			return receiptErr
		}
		v := botInteractionAdmissionPayload{Version: 1, AdmissionID: id.New(), InstanceID: d.Instance.InstanceID, InstallationID: installation.ID, CommunityID: installation.CommunityID, BotUserID: *installation.UserID, BotDeviceID: *installation.DeviceID, ParticipantUserID: participant.ID, ParticipantDeviceID: device.ID, GroupID: req.GroupID, EligibilityRevision: revision, NotAfter: nowUnix + 30}
		payload, err = cborx.Marshal(v)
		if err != nil {
			return err
		}
		signature, err = d.SignBotInteractionAdmission(payload)
		if err != nil || len(signature) != 64 {
			return server.Errorf(server.CodeUnavailable, "interaction admission signing failed")
		}
		newReceipt := store.BotInteractionAdmissionReceipt{InstallationID: installation.ID, RequestID: req.RequestID, RequestHash: requestHash[:], Payload: payload, Signature: signature, NotAfter: now + 30}
		if reissue {
			return receipts.UpdateBotInteractionAdmissionReceipt(r.Context(), newReceipt)
		}
		return receipts.CreateBotInteractionAdmissionReceipt(r.Context(), newReceipt)
	})
	if err != nil {
		server.WriteError(w, d.storeError(r, err))
		return
	}
	d.write(w, r, http.StatusOK, []any{payload, signature})
}
