package ds

import (
	"testing"

	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/mlswasi"
)

func interactionID(b byte) id.ID { var v id.ID; v[0] = b; return v }

func validInteractionRosterFixture() (InteractionAdmissionTuple, InteractionAdmission, []AuthenticatedInteractionLeaf, []mlswasi.DeviceListEntry, []mlswasi.DeviceListEntry) {
	botUser, botDevice := interactionID(1), interactionID(2)
	participantUser, participantDevice := interactionID(3), interactionID(4)
	botDSK := make([]byte, 32)
	botDSK[0] = 5
	participantDSK := make([]byte, 32)
	participantDSK[0] = 6
	tuple := InteractionAdmissionTuple{InstanceID: interactionID(7), InstallationID: interactionID(8), CommunityID: interactionID(9), BotUserID: botUser, BotDeviceID: botDevice, ParticipantUserID: participantUser, ParticipantDeviceID: participantDevice, GroupID: interactionID(10)}
	admission := InteractionAdmission{Version: 1, AdmissionID: interactionID(11), InteractionAdmissionTuple: tuple, EligibilityRevision: 1, NotAfter: 200}
	leaves := []AuthenticatedInteractionLeaf{{UserID: botUser, DeviceID: botDevice, DSKPub: botDSK, Tier: 0, Kind: 1}, {UserID: participantUser, DeviceID: participantDevice, DSKPub: participantDSK, Tier: 1, Kind: 0}}
	botList := []mlswasi.DeviceListEntry{{DeviceID: botDevice[:], DSKPub: botDSK, Tier: 0}}
	participantList := []mlswasi.DeviceListEntry{{DeviceID: participantDevice[:], DSKPub: participantDSK, Tier: 1}}
	return tuple, admission, leaves, botList, participantList
}

func TestVerifyInteractionRosterAcceptsExactAuthenticatedTwoLeafRoster(t *testing.T) {
	tuple, admission, leaves, botList, participantList := validInteractionRosterFixture()
	if err := VerifyInteractionRoster(tuple, 1, admission, leaves, botList, participantList, 100); err != nil {
		t.Fatalf("valid exact roster refused: %v", err)
	}
}

func TestVerifyInteractionRosterFailsClosedOnRosterAndAuthorityMismatches(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*InteractionAdmissionTuple, *InteractionAdmission, *[]AuthenticatedInteractionLeaf, *[]mlswasi.DeviceListEntry, *[]mlswasi.DeviceListEntry)
	}{
		{name: "extra leaf", mutate: func(_ *InteractionAdmissionTuple, _ *InteractionAdmission, leaves *[]AuthenticatedInteractionLeaf, _, _ *[]mlswasi.DeviceListEntry) {
			*leaves = append(*leaves, (*leaves)[1])
		}},
		{name: "participant user mismatch", mutate: func(tuple *InteractionAdmissionTuple, _ *InteractionAdmission, _ *[]AuthenticatedInteractionLeaf, _ *[]mlswasi.DeviceListEntry, _ *[]mlswasi.DeviceListEntry) {
			tuple.ParticipantUserID = interactionID(12)
		}},
		{name: "wrong bot device", mutate: func(_ *InteractionAdmissionTuple, _ *InteractionAdmission, leaves *[]AuthenticatedInteractionLeaf, _, _ *[]mlswasi.DeviceListEntry) {
			(*leaves)[0].DeviceID = interactionID(13)
		}},
		{name: "revoked participant", mutate: func(_ *InteractionAdmissionTuple, _ *InteractionAdmission, _ *[]AuthenticatedInteractionLeaf, _ *[]mlswasi.DeviceListEntry, list *[]mlswasi.DeviceListEntry) {
			(*list)[0].Revoked = true
		}},
		{name: "wrong participant DSK", mutate: func(_ *InteractionAdmissionTuple, _ *InteractionAdmission, _ *[]AuthenticatedInteractionLeaf, _ *[]mlswasi.DeviceListEntry, list *[]mlswasi.DeviceListEntry) {
			(*list)[0].DSKPub = make([]byte, 32)
		}},
		{name: "wrong tier", mutate: func(_ *InteractionAdmissionTuple, _ *InteractionAdmission, _ *[]AuthenticatedInteractionLeaf, _ *[]mlswasi.DeviceListEntry, list *[]mlswasi.DeviceListEntry) {
			(*list)[0].Tier = 0
		}},
		{name: "expired admission", mutate: func(_ *InteractionAdmissionTuple, admission *InteractionAdmission, _ *[]AuthenticatedInteractionLeaf, _ *[]mlswasi.DeviceListEntry, _ *[]mlswasi.DeviceListEntry) {
			admission.NotAfter = 100
		}},
		{name: "zero admission id", mutate: func(_ *InteractionAdmissionTuple, admission *InteractionAdmission, _ *[]AuthenticatedInteractionLeaf, _ *[]mlswasi.DeviceListEntry, _ *[]mlswasi.DeviceListEntry) {
			admission.AdmissionID = id.Zero
		}},
		{name: "admission tuple mismatch", mutate: func(_ *InteractionAdmissionTuple, admission *InteractionAdmission, _ *[]AuthenticatedInteractionLeaf, _ *[]mlswasi.DeviceListEntry, _ *[]mlswasi.DeviceListEntry) {
			admission.GroupID = interactionID(14)
		}},
		{name: "duplicate participant list entry", mutate: func(_ *InteractionAdmissionTuple, _ *InteractionAdmission, _ *[]AuthenticatedInteractionLeaf, _ *[]mlswasi.DeviceListEntry, list *[]mlswasi.DeviceListEntry) {
			*list = append(*list, (*list)[0])
		}},
		{name: "revision exceeds signed integer bound", mutate: func(_ *InteractionAdmissionTuple, admission *InteractionAdmission, _ *[]AuthenticatedInteractionLeaf, _ *[]mlswasi.DeviceListEntry, _ *[]mlswasi.DeviceListEntry) {
			admission.EligibilityRevision = ^uint64(0)
		}},
		{name: "admission revision is stale", mutate: func(_ *InteractionAdmissionTuple, admission *InteractionAdmission, _ *[]AuthenticatedInteractionLeaf, _ *[]mlswasi.DeviceListEntry, _ *[]mlswasi.DeviceListEntry) {
			admission.EligibilityRevision = 1
		}},
		{name: "wrong Bot account kind", mutate: func(_ *InteractionAdmissionTuple, _ *InteractionAdmission, leaves *[]AuthenticatedInteractionLeaf, _ *[]mlswasi.DeviceListEntry, _ *[]mlswasi.DeviceListEntry) {
			(*leaves)[0].Kind = 0
		}},
		{name: "wrong participant account kind", mutate: func(_ *InteractionAdmissionTuple, _ *InteractionAdmission, leaves *[]AuthenticatedInteractionLeaf, _ *[]mlswasi.DeviceListEntry, _ *[]mlswasi.DeviceListEntry) {
			(*leaves)[1].Kind = 1
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tuple, admission, leaves, botList, participantList := validInteractionRosterFixture()
			tc.mutate(&tuple, &admission, &leaves, &botList, &participantList)
			minimumRevision := uint64(1)
			if tc.name == "admission revision is stale" {
				minimumRevision = 2
			}
			if err := VerifyInteractionRoster(tuple, minimumRevision, admission, leaves, botList, participantList, 100); err == nil {
				t.Fatal("invalid interaction roster accepted")
			}
		})
	}
}
