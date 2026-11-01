package api

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"net/http"
	"reflect"

	"github.com/jonasthim/dilla/internal/cborx"
	"github.com/jonasthim/dilla/internal/ds"
	"github.com/jonasthim/dilla/internal/id"
	"github.com/jonasthim/dilla/internal/server"
	"github.com/jonasthim/dilla/internal/store"
)

type currentInteractionContextWire struct {
	_                   struct{} `cbor:",toarray"`
	Version             uint64
	InstanceID          id.ID
	InstanceKeyID       id.ID
	InstanceSignerPub   []byte
	IssuedAt            uint64
	NotAfter            uint64
	Registration        currentInteractionRegistrationWire
	Binding             currentInteractionBindingWire
	Epoch               uint64
	LeafCount           uint64
	TreeHash            []byte
	Roster              []currentInteractionLeafWire
	EligibilityRevision uint64
	PolicyVersion       uint64
	E2EEVersion         uint64
	MediaVersion        uint64
	Bot                 interactionSnapshotListWire
	Participant         interactionSnapshotListWire
}

type currentInteractionRegistrationWire struct {
	_              struct{} `cbor:",toarray"`
	InstallationID id.ID
	OperationID    id.ID
	UserID         id.ID
	DeviceID       id.ID
	CommunityID    id.ID
	GroupID        id.ID
	BodySHA256     []byte
	Generation     uint64
	Epoch          uint64
	NextSeq        uint64
	AcceptedAt     uint64
}

type currentInteractionBindingWire struct {
	_                   struct{} `cbor:",toarray"`
	InstanceID          id.ID
	InstallationID      id.ID
	CommunityID         id.ID
	BotUserID           id.ID
	ParticipantUserID   id.ID
	ParticipantDeviceID id.ID
	PolicyVersion       uint64
	E2EEVersion         uint64
	MediaVersion        uint64
}

type currentInteractionLeafWire struct {
	_            struct{} `cbor:",toarray"`
	LeafIndex    uint32
	UserID       id.ID
	DeviceID     id.ID
	SignatureKey []byte
}

type currentInteractionLiveState struct {
	User, Bot                store.UserRow
	Device, BotDevice        store.DeviceRow
	ParticipantList, BotList store.DeviceListRow
	EligibilityRevision      uint64
}

// BotInteractionGroupReadAllowed proves that a Bot session may read the exact
// active kind-3 group after rechecking the accepted MLS roster and live account state.
// verifyCurrentInteractionLiveState is shared by the human snapshot route and the
// Bot's narrowly-scoped kind-3 read gate. It rechecks the accepted registration
// against the current Ready installation, membership, device rows, signed lists,
// bans, policy and eligibility revision. A non-nil lockedTx is the same repository
// transaction that holds the DS interaction-group fence.
func (d Deps) verifyCurrentInteractionLiveState(ctx context.Context, current ds.CurrentBotInteractionGroupContext, now int64,
	installationID, communityID, botUserID, botDeviceID, participantUserID, participantDeviceID id.ID, readyOnly bool, lockedTx store.Repository) (currentInteractionLiveState, error) {
	var state currentInteractionLiveState
	if now < 0 || d.Repo == nil || d.DeviceLists == nil || d.Instance.InstanceID.IsZero() ||
		current.Binding.InstallationID != installationID || current.Binding.CommunityID != communityID || current.Binding.BotUserID != botUserID ||
		current.Binding.ParticipantUserID != participantUserID || current.Binding.ParticipantDeviceID != participantDeviceID ||
		current.Registration.InstallationID != installationID || current.Registration.CommunityID != communityID || current.Registration.UserID != botUserID ||
		current.Registration.DeviceID != botDeviceID || current.Registration.GroupID.IsZero() || current.Registration.AcceptedAt < 0 {
		return state, errors.New("current interaction identity mismatch")
	}
	check := func(ctx context.Context, tx store.Repository) error {
		if err := tx.LockCommunity(ctx, communityID); err != nil {
			return err
		}
		if err := tx.LockBotInstallation(ctx, installationID); err != nil {
			return err
		}
		install, err := tx.GetBotInstallation(ctx, installationID)
		validState := install.State == store.BotInstallationReady || (!readyOnly && install.State == store.BotInstallationJoining)
		if err != nil || !validState || install.RemovedAt != nil || install.CommunityID != communityID || install.UserID == nil || install.DeviceID == nil || *install.UserID != botUserID || *install.DeviceID != botDeviceID {
			return errors.New("inactive Bot installation")
		}
		lockedUsers := []id.ID{botUserID, participantUserID}
		if bytes.Compare(lockedUsers[0][:], lockedUsers[1][:]) > 0 {
			lockedUsers[0], lockedUsers[1] = lockedUsers[1], lockedUsers[0]
		}
		var previousUser id.ID
		havePreviousUser := false
		for _, uid := range lockedUsers {
			if havePreviousUser && uid == previousUser {
				continue
			}
			if err := tx.LockUserForDeviceRegistration(ctx, uid); err != nil {
				return err
			}
			previousUser, havePreviousUser = uid, true
		}
		state.Bot, err = tx.GetUser(ctx, botUserID)
		if err != nil || state.Bot.Kind != 1 || state.Bot.Flags != 0 || state.Bot.DisabledAt != nil || state.Bot.DeletedAt != nil {
			return errors.New("inactive Bot account")
		}
		state.BotDevice, err = tx.GetDevice(ctx, botDeviceID)
		if err != nil || state.BotDevice.UserID != botUserID || state.BotDevice.VerifiedAt == nil || state.BotDevice.RevokedAt != nil || state.BotDevice.QuarantinedAt != nil {
			return errors.New("inactive Bot device")
		}
		state.User, err = tx.GetUser(ctx, participantUserID)
		if err != nil || state.User.Kind != kindHuman || state.User.DisabledAt != nil || state.User.DeletedAt != nil {
			return errors.New("inactive human participant")
		}
		state.Device, err = tx.GetDevice(ctx, participantDeviceID)
		if err != nil || !ds.ActiveHumanInteractionDevice(participantUserID, state.Device) {
			return errors.New("inactive participant device")
		}
		if _, err := tx.GetMember(ctx, communityID, participantUserID); err != nil {
			return err
		}
		if _, err := tx.GetMember(ctx, communityID, botUserID); err != nil {
			return err
		}
		community, err := tx.GetCommunity(ctx, communityID)
		if err != nil || community.PolicyVersion != current.Binding.PolicyVersion {
			return errors.New("community policy changed")
		}
		for _, uid := range []id.ID{participantUserID, botUserID} {
			ban, banErr := tx.GetBan(ctx, communityID, uid)
			if banErr == nil && (ban.Expires == nil || *ban.Expires > now) {
				return errors.New("current member is banned")
			}
			if banErr != nil && !errors.Is(banErr, store.ErrNotFound) {
				return banErr
			}
		}
		state.ParticipantList, err = tx.GetDeviceList(ctx, participantUserID)
		if err != nil {
			return err
		}
		state.BotList, err = tx.GetDeviceList(ctx, botUserID)
		if err != nil {
			return err
		}
		participantEntries, verifyErr := d.DeviceLists.Verify(ctx, state.ParticipantList.Blob, state.User.SSKPub, state.User.ID)
		if verifyErr != nil || !ds.EligibleHumanInteractionDevice(state.User.ID, state.Device, participantEntries) {
			return errors.New("participant device absent from signed list")
		}
		botEntries, verifyErr := d.DeviceLists.Verify(ctx, state.BotList.Blob, state.Bot.SSKPub, state.Bot.ID)
		if verifyErr != nil || !deviceListContains(botEntries, state.BotDevice) {
			return errors.New("bot device absent from signed list")
		}
		eligibility, ok := tx.(store.BotInteractionEligibility)
		if !ok {
			return errors.New("eligibility revision unavailable")
		}
		state.EligibilityRevision, err = eligibility.GetBotInteractionEligibilityRevision(ctx, communityID, participantUserID)
		if err != nil || state.EligibilityRevision == 0 {
			return errors.New("eligibility revision unavailable")
		}
		instance, err := tx.GetInstance(ctx)
		if err != nil || instance.InstanceID != current.Binding.InstanceID || instance.InstanceID != d.Instance.InstanceID || instance.ExternalSenderKeyID != d.Instance.ExternalSenderKeyID {
			return errors.New("instance key context stale")
		}
		return nil
	}
	var err error
	if lockedTx != nil {
		err = check(ctx, lockedTx)
	} else {
		err = d.withDeviceListTransaction(ctx, check)
	}
	if err != nil {
		return currentInteractionLiveState{}, err
	}
	if len(state.BotList.Blob) == 0 || len(state.ParticipantList.Blob) == 0 || len(state.BotList.Blob) > maxInteractionListBlob || len(state.ParticipantList.Blob) > maxInteractionListBlob ||
		!validSnapshotList(interactionSnapshotDeviceList{UserID: state.Bot.ID, Version: state.BotList.Version, UMKPub: state.Bot.UMKPub, SSKPub: state.Bot.SSKPub, SSKBindingSignature: state.Bot.SigUMKSSK, Blob: state.BotList.Blob, Signature: state.BotList.SSKSignature, PrevHash: state.BotList.PrevHash}) ||
		!validSnapshotList(interactionSnapshotDeviceList{UserID: state.User.ID, Version: state.ParticipantList.Version, UMKPub: state.User.UMKPub, SSKPub: state.User.SSKPub, SSKBindingSignature: state.User.SigUMKSSK, Blob: state.ParticipantList.Blob, Signature: state.ParticipantList.SSKSignature, PrevHash: state.ParticipantList.PrevHash}) {
		return currentInteractionLiveState{}, errors.New("invalid current interaction signed lists")
	}
	return state, nil
}

func (h *Groups) currentInteractionAdmissionContext(w http.ResponseWriter, r *http.Request) {
	d := h.Deps
	s, err := sessionOf(r)
	if err != nil || s.Bot != nil || s.Scope != 0 || s.UserID.IsZero() || s.DeviceID.IsZero() {
		server.WriteError(w, server.Errorf(server.CodeForbidden, "enrolled human session required"))
		return
	}
	groupID, err := server.PathID(r, "id")
	if err != nil {
		server.WriteError(w, err)
		return
	}
	if h.DS == nil || d.Repo == nil || d.Clock == nil || d.DeviceLists == nil || d.SignBotCurrentInteractionContext == nil || len(d.ExternalSenderPub) != ed25519.PublicKeySize {
		server.WriteError(w, server.Errorf(server.CodeUnavailable, "current interaction context unavailable"))
		return
	}
	current, err := h.DS.CurrentBotInteractionGroupContext(r.Context(), groupID)
	if err != nil || current.Binding.ParticipantUserID != s.UserID || current.Binding.ParticipantDeviceID != s.DeviceID {
		server.WriteError(w, server.Errorf(server.CodeForbidden, "current interaction context unavailable"))
		return
	}
	now := d.Clock.Now().Unix()
	nowU, nowOK := checkedUint64(now)
	acceptedAt, acceptedAtOK := checkedUint64(current.Registration.AcceptedAt)
	if !nowOK || !acceptedAtOK {
		server.WriteError(w, server.Errorf(server.CodeUnavailable, "current interaction context unavailable"))
		return
	}
	live, err := d.verifyCurrentInteractionLiveState(r.Context(), current, now,
		current.Binding.InstallationID, current.Binding.CommunityID, current.Binding.BotUserID, current.Registration.DeviceID,
		s.UserID, s.DeviceID, false, nil)
	if err != nil {
		server.WriteError(w, server.Errorf(server.CodeForbidden, "current interaction context unavailable"))
		return
	}
	user, bot := live.User, live.Bot
	participantList, botList, revision := live.ParticipantList, live.BotList, live.EligibilityRevision
	wire := currentInteractionContextWire{Version: 1, InstanceID: current.Binding.InstanceID, InstanceKeyID: d.Instance.ExternalSenderKeyID, InstanceSignerPub: append([]byte(nil), d.ExternalSenderPub...), IssuedAt: nowU, NotAfter: nowU + 30,
		Registration: currentInteractionRegistrationWire{InstallationID: current.Registration.InstallationID, OperationID: current.Registration.OperationID, UserID: current.Registration.UserID, DeviceID: current.Registration.DeviceID, CommunityID: current.Registration.CommunityID, GroupID: current.Registration.GroupID, BodySHA256: current.Registration.BodySHA256, Generation: current.Registration.Generation, Epoch: current.Registration.Epoch, NextSeq: current.Registration.NextSeq, AcceptedAt: acceptedAt},
		Binding:      currentInteractionBindingWire{InstanceID: current.Binding.InstanceID, InstallationID: current.Binding.InstallationID, CommunityID: current.Binding.CommunityID, BotUserID: current.Binding.BotUserID, ParticipantUserID: current.Binding.ParticipantUserID, ParticipantDeviceID: current.Binding.ParticipantDeviceID, PolicyVersion: current.Binding.PolicyVersion, E2EEVersion: current.Binding.E2EEVersion, MediaVersion: current.Binding.MediaVersion},
		Epoch:        current.Epoch, LeafCount: current.LeafCount, TreeHash: current.TreeHash, EligibilityRevision: revision, PolicyVersion: current.Binding.PolicyVersion, E2EEVersion: current.Binding.E2EEVersion, MediaVersion: current.Binding.MediaVersion,
		Bot:         snapshotListWire(interactionSnapshotDeviceList{UserID: bot.ID, UMKPub: bot.UMKPub, SSKPub: bot.SSKPub, SSKBindingSignature: bot.SigUMKSSK, Version: botList.Version, Blob: botList.Blob, Signature: botList.SSKSignature, PrevHash: botList.PrevHash}),
		Participant: snapshotListWire(interactionSnapshotDeviceList{UserID: user.ID, UMKPub: user.UMKPub, SSKPub: user.SSKPub, SSKBindingSignature: user.SigUMKSSK, Version: participantList.Version, Blob: participantList.Blob, Signature: participantList.SSKSignature, PrevHash: participantList.PrevHash})}
	for _, leaf := range current.Roster {
		wire.Roster = append(wire.Roster, currentInteractionLeafWire{LeafIndex: leaf.LeafIndex, UserID: leaf.UserID, DeviceID: leaf.DeviceID, SignatureKey: leaf.SignatureKey})
	}
	payload, err := cborx.Marshal(wire)
	if err != nil || len(payload) > maxInteractionSnapshot {
		server.WriteError(w, server.Errorf(server.CodeUnavailable, "current interaction context exceeds response limit"))
		return
	}
	var sig []byte
	err = h.DS.WithCurrentBotInteractionGroupContext(r.Context(), groupID, func(latest ds.CurrentBotInteractionGroupContext) error {
		// Device lists and policy above were read in their own transaction. Bind the
		// signature to that exact MLS state, then keep the group's lock through signing.
		if !reflect.DeepEqual(current.Registration, latest.Registration) || current.Binding != latest.Binding ||
			current.Epoch != latest.Epoch || current.LeafCount != latest.LeafCount || !bytes.Equal(current.TreeHash, latest.TreeHash) ||
			!reflect.DeepEqual(current.Roster, latest.Roster) {
			return errors.New("current interaction group changed before signing")
		}
		var signErr error
		sig, signErr = d.SignBotCurrentInteractionContext(payload)
		return signErr
	})
	if err != nil || len(sig) != ed25519.SignatureSize {
		server.WriteError(w, server.Errorf(server.CodeUnavailable, "current interaction context signing failed"))
		return
	}
	d.write(w, r, http.StatusOK, []any{payload, sig})
}

// withDeviceListTransaction reserves a native verifier before the write transaction.
// The verifier and all current authorization checks remain inside that transaction.
func (d Deps) withDeviceListTransaction(ctx context.Context, fn func(context.Context, store.Repository) error) error {
	if d.DeviceLists == nil {
		// No native verifier exists to reserve. Preserve readiness's original
		// transaction-scoped dependency refusal (pending prerequisites), including
		// its earlier authority and replay checks. Non-nil providers must scope.
		return d.Repo.Tx(ctx, func(tx store.Repository) error { return fn(ctx, tx) })
	}
	return d.DeviceLists.WithVerification(ctx, func(scoped context.Context) error {
		return d.Repo.Tx(scoped, func(tx store.Repository) error { return fn(scoped, tx) })
	})
}
