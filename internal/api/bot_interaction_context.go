package api

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"errors"
	"net/http"

	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/ds"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/server"
)

const (
	interactionContextDomain = "dilla bot interaction context snapshot v1\x00"
	maxInteractionListBlob   = 64 << 10
	maxInteractionSnapshot   = 160 << 10
)

type interactionSnapshotTuple struct {
	_                   struct{} `cbor:",toarray"`
	InstanceID          id.ID
	InstallationID      id.ID
	CommunityID         id.ID
	BotUserID           id.ID
	BotDeviceID         id.ID
	ParticipantUserID   id.ID
	ParticipantDeviceID id.ID
	GroupID             id.ID
}

type interactionSnapshotDeviceList struct {
	UserID              id.ID
	UMKPub              []byte
	SSKPub              []byte
	SSKBindingSignature []byte
	Version             uint64
	Blob                []byte
	Signature           []byte
	PrevHash            []byte
}

type botInteractionContextSnapshot struct {
	RequestID                  id.ID
	AdmissionPayload           []byte
	AdmissionSignature         []byte
	Now                        uint64
	MinimumEligibilityRevision uint64
	PolicyVersion              uint64
	E2EEVersion                uint64
	MediaVersion               uint64
	InstanceID                 id.ID
	InstanceKeyID              id.ID
	InstanceSignerPub          []byte
	Expected                   ds.InteractionAdmissionTuple
	Bot                        interactionSnapshotDeviceList
	Participant                interactionSnapshotDeviceList
}

type botInteractionContextWire struct {
	_                          struct{} `cbor:",toarray"`
	Version                    uint64
	RequestID                  id.ID
	AdmissionID                id.ID
	AdmissionPayload           []byte
	AdmissionSignature         []byte
	Now                        uint64
	MinimumEligibilityRevision uint64
	PolicyVersion              uint64
	E2EEVersion                uint64
	MediaVersion               uint64
	InstanceID                 id.ID
	InstanceKeyID              id.ID
	InstanceSignerPub          []byte
	Expected                   interactionSnapshotTuple
	Bot                        interactionSnapshotListWire
	Participant                interactionSnapshotListWire
}

type interactionSnapshotListWire struct {
	_                   struct{} `cbor:",toarray"`
	UserID              id.ID
	UMKPub              []byte
	SSKPub              []byte
	SSKBindingSignature []byte
	Version             uint64
	Blob                []byte
	Signature           []byte
	PrevHash            []byte
	Hash                []byte
}

// LoadBotInteractionContextSnapshot returns a canonical, signed copy of the
// exact inputs checked by the read-only preflight transaction. The registrar
// still reloads and revalidates all mutable state before it consumes a receipt.
func (d Deps) LoadBotInteractionContextSnapshot(ctx context.Context, req BotInteractionPreflightRequest) ([]byte, []byte, error) {
	var snapshot botInteractionContextSnapshot
	if _, err := d.loadBotInteractionRegistrationPreflight(ctx, req, &snapshot); err != nil {
		return nil, nil, err
	}
	if d.SignBotInteractionContext == nil {
		return nil, nil, errors.New("api: interaction context signer unavailable")
	}
	if len(snapshot.Bot.Blob) == 0 || len(snapshot.Participant.Blob) == 0 || len(snapshot.Bot.Blob) > maxInteractionListBlob || len(snapshot.Participant.Blob) > maxInteractionListBlob ||
		!validSnapshotList(snapshot.Bot) || !validSnapshotList(snapshot.Participant) ||
		len(snapshot.InstanceSignerPub) != ed25519.PublicKeySize || snapshot.InstanceKeyID.IsZero() {
		return nil, nil, errors.New("api: interaction device list exceeds snapshot limit")
	}
	var admission botInteractionAdmissionPayload
	if err := cborx.Unmarshal(snapshot.AdmissionPayload, &admission); err != nil || admission.AdmissionID.IsZero() {
		return nil, nil, errors.New("api: malformed admission in interaction snapshot")
	}
	wire := botInteractionContextWire{
		Version: 1, RequestID: snapshot.RequestID, AdmissionID: admission.AdmissionID,
		AdmissionPayload: snapshot.AdmissionPayload, AdmissionSignature: snapshot.AdmissionSignature,
		Now: snapshot.Now, MinimumEligibilityRevision: snapshot.MinimumEligibilityRevision,
		PolicyVersion: snapshot.PolicyVersion, E2EEVersion: snapshot.E2EEVersion,
		MediaVersion: snapshot.MediaVersion, InstanceID: snapshot.InstanceID,
		InstanceKeyID: snapshot.InstanceKeyID, InstanceSignerPub: snapshot.InstanceSignerPub,
		Expected: interactionSnapshotTuple{
			InstanceID: snapshot.Expected.InstanceID, InstallationID: snapshot.Expected.InstallationID,
			CommunityID: snapshot.Expected.CommunityID, BotUserID: snapshot.Expected.BotUserID,
			BotDeviceID: snapshot.Expected.BotDeviceID, ParticipantUserID: snapshot.Expected.ParticipantUserID,
			ParticipantDeviceID: snapshot.Expected.ParticipantDeviceID, GroupID: snapshot.Expected.GroupID,
		},
		Bot: snapshotListWire(snapshot.Bot), Participant: snapshotListWire(snapshot.Participant),
	}
	payload, err := cborx.Marshal(wire)
	if err != nil || len(payload) > maxInteractionSnapshot {
		return nil, nil, errors.New("api: interaction context snapshot exceeds response limit")
	}
	signature, err := d.SignBotInteractionContext(payload)
	if err != nil || len(signature) != ed25519.SignatureSize {
		return nil, nil, errors.New("api: interaction context signing failed")
	}
	return payload, signature, nil
}

func validSnapshotList(list interactionSnapshotDeviceList) bool {
	return !list.UserID.IsZero() && list.Version > 0 && len(list.UMKPub) == 32 &&
		len(list.SSKPub) == 32 && len(list.SSKBindingSignature) == ed25519.SignatureSize &&
		len(list.Signature) == ed25519.SignatureSize && len(list.PrevHash) == sha256.Size
}

func snapshotListWire(list interactionSnapshotDeviceList) interactionSnapshotListWire {
	hash := sha256.Sum256(list.Blob)
	return interactionSnapshotListWire{UserID: list.UserID, UMKPub: list.UMKPub, SSKPub: list.SSKPub,
		SSKBindingSignature: list.SSKBindingSignature, Version: list.Version, Blob: list.Blob,
		Signature: list.Signature, PrevHash: list.PrevHash, Hash: hash[:]}
}

func (d Deps) registerBotInteractionContext(m *server.Mux) {
	m.SocketAuthed("POST /v1/bot-runtime/installations/{installation}/interaction-admissions/context", botSocketChain(d.Sessions, d.Limiter, "POST /v1/bot-runtime/installations/{installation}/interaction-admissions/context"), http.HandlerFunc(d.getBotInteractionContext))
}

func (d Deps) getBotInteractionContext(w http.ResponseWriter, r *http.Request) {
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
		server.WriteError(w, server.Errorf(server.CodeInvalidRequest, "context identifiers must be nonzero"))
		return
	}
	preflightReq := BotInteractionPreflightRequest{Session: s, InstallationID: installationID,
		AdmissionRequestID: req.RequestID, GroupID: req.GroupID,
		ParticipantUserID: req.ParticipantUser, ParticipantDeviceID: req.ParticipantDevice}
	payload, signature, err := d.LoadBotInteractionContextSnapshot(r.Context(), preflightReq)
	if err != nil {
		server.WriteError(w, server.Errorf(server.CodeForbidden, "interaction context is unavailable or stale"))
		return
	}
	encoded, err := cborx.Marshal([]any{payload, signature})
	if err != nil || len(encoded) > maxInteractionSnapshot+256 {
		server.WriteError(w, server.Errorf(server.CodeUnavailable, "interaction context response exceeds limit"))
		return
	}
	d.write(w, r, http.StatusOK, []any{payload, signature})
}
