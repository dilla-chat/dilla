package api

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"errors"
	"fmt"

	"github.com/jonasthim/dilla/internal/auth"
	"github.com/jonasthim/dilla/internal/cborx"
	"github.com/jonasthim/dilla/internal/ds"
	"github.com/jonasthim/dilla/internal/id"
	"github.com/jonasthim/dilla/internal/mlswasi"
	"github.com/jonasthim/dilla/internal/store"
)

// BotInteractionPreflightRequest names the already authenticated Bot session and the
// durable admission request whose kind-3 MLS group is about to be inspected.
type BotInteractionPreflightRequest struct {
	Session             auth.Session
	InstallationID      id.ID
	AdmissionRequestID  id.ID
	GroupID             id.ID
	ParticipantUserID   id.ID
	ParticipantDeviceID id.ID
}

// LoadBotInteractionRegistrationPreflight obtains every trusted input needed by
// ds.PreflightKind3Registration under one transaction and eligibility fence. It
// only reads durable state; callers must not treat loading or MLS preflight as
// authorization to persist a group registration.
func (d Deps) LoadBotInteractionRegistrationPreflight(ctx context.Context, req BotInteractionPreflightRequest) (ds.InteractionRegistrationPreflight, error) {
	return d.loadBotInteractionRegistrationPreflight(ctx, req, nil)
}

func (d Deps) loadBotInteractionRegistrationPreflight(ctx context.Context, req BotInteractionPreflightRequest, snapshot *botInteractionContextSnapshot) (ds.InteractionRegistrationPreflight, error) {
	var out ds.InteractionRegistrationPreflight
	if req.Session.Bot == nil || req.InstallationID.IsZero() || req.AdmissionRequestID.IsZero() || req.GroupID.IsZero() || req.ParticipantUserID.IsZero() || req.ParticipantDeviceID.IsZero() {
		return out, errors.New("api: incomplete managed Bot interaction preflight request")
	}
	if d.DeviceLists == nil || d.Clock == nil || len(d.ExternalSenderPub) != ed25519.PublicKeySize {
		return out, errors.New("api: trusted interaction preflight dependencies unavailable")
	}
	if req.Session.Bot.InstallationID != req.InstallationID || req.Session.UserID.IsZero() || req.Session.DeviceID.IsZero() || req.Session.Bot.CommunityID.IsZero() {
		return out, errors.New("api: Bot session does not match interaction installation")
	}
	if req.Session.Bot.State != store.BotInstallationJoining && req.Session.Bot.State != store.BotInstallationReady {
		return out, errors.New("api: Bot session role is not eligible for interaction registration")
	}
	var now int64
	var minimumRevision uint64
	err := d.withDeviceListTransaction(ctx, func(ctx context.Context, tx store.Repository) error {
		if err := tx.LockCommunity(ctx, req.Session.Bot.CommunityID); err != nil {
			return err
		}
		if err := tx.LockBotInstallation(ctx, req.InstallationID); err != nil {
			return err
		}
		installation, err := tx.GetBotInstallation(ctx, req.InstallationID)
		if err != nil {
			return err
		}
		if installation.ID != req.Session.Bot.InstallationID || installation.CommunityID != req.Session.Bot.CommunityID || installation.State != req.Session.Bot.State || (installation.State != store.BotInstallationJoining && installation.State != store.BotInstallationReady) || installation.RemovedAt != nil || installation.UserID == nil || installation.DeviceID == nil || installation.CredentialVerifiedAt == nil || *installation.UserID != req.Session.UserID || *installation.DeviceID != req.Session.DeviceID {
			return errors.New("api: Bot session is stale or not bound to the current installation")
		}
		// Serialize account/device-list changes with admission and installation
		// lifecycle changes. Sorting gives both user locks one stable order.
		users := []id.ID{*installation.UserID, req.ParticipantUserID}
		if bytes.Compare(users[0][:], users[1][:]) > 0 {
			users[0], users[1] = users[1], users[0]
		}
		var previousUser id.ID
		havePreviousUser := false
		for _, userID := range users {
			if havePreviousUser && userID == previousUser {
				continue
			}
			if err := tx.LockUserForDeviceRegistration(ctx, userID); err != nil {
				return err
			}
			previousUser, havePreviousUser = userID, true
		}
		now = d.Clock.Now().Unix()
		nowUnix, ok := checkedUint64(now)
		if !ok {
			return errors.New("api: server time outside interaction admission range")
		}
		bot, err := tx.GetUser(ctx, *installation.UserID)
		if err != nil || bot.Kind != 1 || bot.Flags != 0 || bot.DisabledAt != nil || bot.DeletedAt != nil {
			return errors.New("api: active managed Bot account required")
		}
		botDevice, err := tx.GetDevice(ctx, *installation.DeviceID)
		if err != nil || botDevice.UserID != bot.ID || botDevice.VerifiedAt == nil || botDevice.RevokedAt != nil || botDevice.QuarantinedAt != nil {
			return errors.New("api: Bot installation device is not active")
		}
		if _, err := tx.GetMember(ctx, installation.CommunityID, bot.ID); err != nil {
			return errors.New("api: Bot is not a current community member")
		}
		botBan, err := tx.GetBan(ctx, installation.CommunityID, bot.ID)
		if err == nil && (botBan.Expires == nil || *botBan.Expires > now) {
			return errors.New("api: Bot is currently banned")
		}
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		participant, err := tx.GetUser(ctx, req.ParticipantUserID)
		if err != nil || participant.Kind != kindHuman || participant.DisabledAt != nil || participant.DeletedAt != nil {
			return errors.New("api: active human participant required")
		}
		if _, err := tx.GetMember(ctx, installation.CommunityID, participant.ID); err != nil {
			return errors.New("api: participant is not a current community member")
		}
		ban, err := tx.GetBan(ctx, installation.CommunityID, participant.ID)
		if err == nil && (ban.Expires == nil || *ban.Expires > now) {
			return errors.New("api: participant is currently banned")
		}
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		participantDevice, err := tx.GetDevice(ctx, req.ParticipantDeviceID)
		if err != nil || !ds.ActiveHumanInteractionDevice(participant.ID, participantDevice) {
			return errors.New("api: participant device is not active")
		}
		botList, err := tx.GetDeviceList(ctx, bot.ID)
		if err != nil {
			return fmt.Errorf("api: Bot has no current signed device list: %w", err)
		}
		botEntries, err := d.DeviceLists.Verify(ctx, botList.Blob, bot.SSKPub, bot.ID)
		if err != nil || !deviceListContains(botEntries, botDevice) {
			return errors.New("api: Bot device is not in its current signed device list")
		}
		participantList, err := tx.GetDeviceList(ctx, participant.ID)
		if err != nil {
			return fmt.Errorf("api: participant has no current signed device list: %w", err)
		}
		participantEntries, err := d.DeviceLists.Verify(ctx, participantList.Blob, participant.SSKPub, participant.ID)
		if err != nil || !ds.EligibleHumanInteractionDevice(participant.ID, participantDevice, participantEntries) {
			return errors.New("api: participant device is not in its current signed device list")
		}
		eligibility, ok := tx.(store.BotInteractionEligibility)
		if !ok {
			return errors.New("api: interaction eligibility fencing unavailable")
		}
		minimumRevision, err = eligibility.GetBotInteractionEligibilityRevision(ctx, installation.CommunityID, participant.ID)
		if err != nil {
			return err
		}
		if minimumRevision == 0 {
			return errors.New("api: participant eligibility revision is not established")
		}
		community, err := tx.GetCommunity(ctx, installation.CommunityID)
		if err != nil || community.PolicyVersion == 0 {
			return errors.New("api: current community policy is unavailable")
		}
		instance, err := tx.GetInstance(ctx)
		if err != nil || instance.InstanceID != d.Instance.InstanceID || instance.ExternalSenderKeyID != d.Instance.ExternalSenderKeyID {
			return errors.New("api: trusted instance key context is stale")
		}
		receipts, ok := tx.(store.BotInteractionAdmissionReceipts)
		if !ok {
			return errors.New("api: interaction admission receipts unavailable")
		}
		receipt, err := receipts.GetBotInteractionAdmissionReceipt(ctx, installation.ID, req.AdmissionRequestID)
		if err != nil {
			return err
		}
		if receipt.InstallationID != installation.ID || receipt.RequestID != req.AdmissionRequestID || receipt.NotAfter <= now || len(receipt.Payload) == 0 || len(receipt.Signature) != ed25519.SignatureSize {
			return errors.New("api: interaction admission receipt is stale or malformed")
		}
		var payload botInteractionAdmissionPayload
		receiptNotAfter, receiptNotAfterOK := checkedUint64(receipt.NotAfter)
		if err := cborx.Unmarshal(receipt.Payload, &payload); err != nil || !receiptNotAfterOK || payload.Version != 1 || payload.InstanceID != instance.InstanceID || payload.InstallationID != installation.ID || payload.CommunityID != installation.CommunityID || payload.BotUserID != bot.ID || payload.BotDeviceID != botDevice.ID || payload.ParticipantUserID != participant.ID || payload.ParticipantDeviceID != participantDevice.ID || payload.GroupID != req.GroupID || payload.EligibilityRevision != minimumRevision || payload.NotAfter <= nowUnix || payload.NotAfter != receiptNotAfter {
			return errors.New("api: interaction admission receipt does not match current trusted context")
		}
		canonicalPayload, err := cborx.Marshal(payload)
		if err != nil || !bytes.Equal(canonicalPayload, receipt.Payload) || !ed25519.Verify(d.ExternalSenderPub, append([]byte("dilla bot interaction admission v1\x00"), receipt.Payload...), receipt.Signature) {
			return errors.New("api: interaction admission proof is noncanonical or invalid")
		}
		requestBytes, err := cborx.Marshal([]any{req.GroupID, participant.ID, participantDevice.ID})
		if err != nil {
			return err
		}
		requestHash := sha256Bytes(requestBytes)
		if !bytes.Equal(requestHash, receipt.RequestHash) {
			return errors.New("api: interaction admission request identifiers conflict with durable receipt")
		}
		out = ds.InteractionRegistrationPreflight{
			Expected:                   ds.InteractionAdmissionTuple{InstanceID: instance.InstanceID, InstallationID: installation.ID, CommunityID: installation.CommunityID, BotUserID: bot.ID, BotDeviceID: botDevice.ID, ParticipantUserID: participant.ID, ParticipantDeviceID: participantDevice.ID, GroupID: req.GroupID},
			MinimumEligibilityRevision: minimumRevision, ExpectedPolicyVersion: community.PolicyVersion, ExpectedE2EEVersion: 1, ExpectedMediaVersion: 0,
			ReceiptPayload: append([]byte(nil), receipt.Payload...), ReceiptSignature: append([]byte(nil), receipt.Signature...), PinnedInstanceKey: append(ed25519.PublicKey(nil), d.ExternalSenderPub...),
			BotDeviceList: botEntries, ParticipantDeviceList: participantEntries, Now: nowUnix,
		}
		if snapshot != nil {
			*snapshot = botInteractionContextSnapshot{
				RequestID: req.AdmissionRequestID, AdmissionPayload: append([]byte(nil), receipt.Payload...),
				AdmissionSignature: append([]byte(nil), receipt.Signature...), Now: nowUnix,
				MinimumEligibilityRevision: minimumRevision, PolicyVersion: community.PolicyVersion,
				E2EEVersion: 1, MediaVersion: 0, InstanceID: instance.InstanceID,
				InstanceKeyID: instance.ExternalSenderKeyID, InstanceSignerPub: append([]byte(nil), d.ExternalSenderPub...),
				Expected:    out.Expected,
				Bot:         interactionSnapshotDeviceList{UserID: bot.ID, UMKPub: append([]byte(nil), bot.UMKPub...), SSKPub: append([]byte(nil), bot.SSKPub...), SSKBindingSignature: append([]byte(nil), bot.SigUMKSSK...), Version: botList.Version, Blob: append([]byte(nil), botList.Blob...), Signature: append([]byte(nil), botList.SSKSignature...), PrevHash: append([]byte(nil), botList.PrevHash...)},
				Participant: interactionSnapshotDeviceList{UserID: participant.ID, UMKPub: append([]byte(nil), participant.UMKPub...), SSKPub: append([]byte(nil), participant.SSKPub...), SSKBindingSignature: append([]byte(nil), participant.SigUMKSSK...), Version: participantList.Version, Blob: append([]byte(nil), participantList.Blob...), Signature: append([]byte(nil), participantList.SSKSignature...), PrevHash: append([]byte(nil), participantList.PrevHash...)},
			}
		}
		return nil
	})
	return out, err
}

func deviceListContains(entries []mlswasi.DeviceListEntry, device store.DeviceRow) bool {
	for _, entry := range entries {
		if bytes.Equal(entry.DeviceID, device.ID[:]) && !entry.Revoked && entry.Tier == device.Tier && bytes.Equal(entry.DSKPub, device.DSKPub) {
			return true
		}
	}
	return false
}

func sha256Bytes(data []byte) []byte {
	sum := sha256.Sum256(data)
	return sum[:]
}
