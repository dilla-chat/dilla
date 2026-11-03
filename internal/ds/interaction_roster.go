package ds

import (
	"bytes"
	"fmt"
	"math"

	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/mlswasi"
)

const maxInteractionDeviceListEntries = 4096

// InteractionAdmissionTuple is the immutable identity and scope carried by the
// signed kind-3 admission payload.
type InteractionAdmissionTuple struct {
	InstanceID          id.ID
	InstallationID      id.ID
	CommunityID         id.ID
	BotUserID           id.ID
	BotDeviceID         id.ID
	ParticipantUserID   id.ID
	ParticipantDeviceID id.ID
	GroupID             id.ID
}

// InteractionAdmission contains the non-tuple fields needed for bounded
// admission freshness checks. Its authenticity must already have been checked
// against the pinned instance key by the caller.
type InteractionAdmission struct {
	Version     uint64
	AdmissionID id.ID
	InteractionAdmissionTuple
	EligibilityRevision uint64
	NotAfter            uint64
}

// AuthenticatedInteractionLeaf is a descriptor extracted from an authenticated
// MLS roster. UserID and DeviceID must come from the verified MLS credential;
// DSKPub and Tier must be bound to that same leaf by the authenticated credential
// verification path.
type AuthenticatedInteractionLeaf struct {
	UserID   id.ID
	DeviceID id.ID
	DSKPub   []byte
	Tier     uint8
	Kind     uint8
}

// VerifyInteractionRoster checks a short-lived admission against trusted
// installation context, exactly two authenticated MLS leaf descriptors, and
// each user's latest signature-verified device-list entries. The caller must
// authenticate the admission signature and its pinned instance key, obtain the
// trusted tuple from the current installation and request, extract descriptors
// from authenticated MLS state, and supply entries from each user's latest
// signature-verified list. The minimumEligibilityRevision must be read from the
// current trusted installation/participant eligibility fence; a lower or zero
// value fails closed. This function does not parse MLS state, verify
// credentials or list signatures, check Bot/human account kinds, current
// membership or ban state, persist receipts, or authorize later membership
// changes. Kind-3 registration must remain gated until those surrounding
// checks are connected end to end.
func VerifyInteractionRoster(expected InteractionAdmissionTuple, minimumEligibilityRevision uint64, admission InteractionAdmission, leaves []AuthenticatedInteractionLeaf, botList, participantList []mlswasi.DeviceListEntry, now uint64) error {
	if err := validateInteractionTuple(expected); err != nil {
		return err
	}
	if minimumEligibilityRevision == 0 || admission.Version != 1 || admission.AdmissionID.IsZero() || admission.EligibilityRevision < minimumEligibilityRevision || admission.EligibilityRevision > math.MaxInt64 || admission.NotAfter == 0 || admission.NotAfter > math.MaxInt64 || admission.NotAfter <= now || now > math.MaxInt64 {
		return fmt.Errorf("ds: invalid or expired interaction admission")
	}
	if admission.InteractionAdmissionTuple != expected {
		return fmt.Errorf("ds: interaction admission tuple does not match trusted context")
	}
	if len(leaves) != 2 {
		return fmt.Errorf("ds: interaction roster must contain exactly two leaves")
	}
	if len(botList) > maxInteractionDeviceListEntries || len(participantList) > maxInteractionDeviceListEntries {
		return fmt.Errorf("ds: interaction device list exceeds verification bound")
	}
	for _, leaf := range leaves {
		if leaf.UserID.IsZero() || leaf.DeviceID.IsZero() || len(leaf.DSKPub) != 32 {
			return fmt.Errorf("ds: interaction roster has an invalid authenticated leaf descriptor")
		}
	}
	if leaves[0].DeviceID == leaves[1].DeviceID || leaves[0].UserID == leaves[1].UserID {
		return fmt.Errorf("ds: interaction roster leaves must name distinct Bot and participant identities")
	}
	bot, participant, ok := matchInteractionLeaves(expected, leaves)
	if !ok {
		return fmt.Errorf("ds: interaction roster does not contain the admitted Bot and participant devices")
	}
	if !interactionDeviceListContains(botList, expected.BotDeviceID, bot.DSKPub, bot.Tier) {
		return fmt.Errorf("ds: Bot leaf is absent from its active signed device list")
	}
	if !interactionDeviceListContains(participantList, expected.ParticipantDeviceID, participant.DSKPub, participant.Tier) {
		return fmt.Errorf("ds: participant leaf is absent from its active signed device list")
	}
	if bot.Kind != 1 || participant.Kind != 0 {
		return fmt.Errorf("ds: interaction roster must contain a Bot leaf and a human participant leaf")
	}
	return nil
}

func validateInteractionTuple(t InteractionAdmissionTuple) error {
	if t.InstanceID.IsZero() || t.InstallationID.IsZero() || t.CommunityID.IsZero() || t.BotUserID.IsZero() || t.BotDeviceID.IsZero() || t.ParticipantUserID.IsZero() || t.ParticipantDeviceID.IsZero() || t.GroupID.IsZero() {
		return fmt.Errorf("ds: interaction admission tuple contains a zero identifier")
	}
	if t.BotUserID == t.ParticipantUserID || t.BotDeviceID == t.ParticipantDeviceID {
		return fmt.Errorf("ds: interaction admission must identify distinct Bot and participant principals")
	}
	return nil
}

func matchInteractionLeaves(t InteractionAdmissionTuple, leaves []AuthenticatedInteractionLeaf) (AuthenticatedInteractionLeaf, AuthenticatedInteractionLeaf, bool) {
	var bot, participant AuthenticatedInteractionLeaf
	for _, leaf := range leaves {
		switch {
		case leaf.UserID == t.BotUserID && leaf.DeviceID == t.BotDeviceID:
			bot = leaf
		case leaf.UserID == t.ParticipantUserID && leaf.DeviceID == t.ParticipantDeviceID:
			participant = leaf
		default:
			return AuthenticatedInteractionLeaf{}, AuthenticatedInteractionLeaf{}, false
		}
	}
	return bot, participant, bot.DeviceID == t.BotDeviceID && participant.DeviceID == t.ParticipantDeviceID
}

func interactionDeviceListContains(entries []mlswasi.DeviceListEntry, device id.ID, dsk []byte, tier uint8) bool {
	found := false
	for _, entry := range entries {
		if !bytes.Equal(entry.DeviceID, device[:]) {
			continue
		}
		if found || entry.Revoked || entry.Tier != tier || !bytes.Equal(entry.DSKPub, dsk) {
			return false
		}
		found = true
	}
	return found
}
