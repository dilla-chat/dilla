package ds

import (
	"context"
	"crypto/ed25519"
	"testing"

	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/mlswasi"
)

type interactionReceiptWire struct {
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

func interactionBindingForTest(t *testing.T, tuple InteractionAdmissionTuple) []byte {
	t.Helper()
	b, err := cborx.Marshal(interactionBindingV2{Version: 2, InstanceID: tuple.InstanceID, BotUserID: tuple.BotUserID, Kind: 3, PolicyVersion: 1, E2EEVersion: 1, MediaVersion: 0, InstallationID: tuple.InstallationID, CommunityID: tuple.CommunityID, ParticipantUserID: tuple.ParticipantUserID, ParticipantDeviceID: tuple.ParticipantDeviceID})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func alteredInteractionBinding(tuple InteractionAdmissionTuple, field string) []byte {
	b := interactionBindingV2{Version: 2, InstanceID: tuple.InstanceID, BotUserID: tuple.BotUserID, Kind: 3, PolicyVersion: 1, E2EEVersion: 1, InstallationID: tuple.InstallationID, CommunityID: tuple.CommunityID, ParticipantUserID: tuple.ParticipantUserID, ParticipantDeviceID: tuple.ParticipantDeviceID}
	switch field {
	case "installation":
		b.InstallationID = interactionID(80)
	case "community":
		b.CommunityID = interactionID(81)
	case "participant device":
		b.ParticipantDeviceID = interactionID(82)
	case "participant user":
		b.ParticipantUserID = interactionID(83)
	case "bot":
		b.BotUserID = interactionID(84)
	case "policy version":
		b.PolicyVersion = 2
	case "E2EE version":
		b.E2EEVersion = 2
	case "media version":
		b.MediaVersion = 1
	}
	encoded, _ := cborx.Marshal(b)
	return encoded
}

type rosterExtractorFunc func(context.Context, []byte, []byte) (AuthenticatedInteractionRegistration, error)

func (f rosterExtractorFunc) ExtractInteractionRegistration(ctx context.Context, tree, info []byte) (AuthenticatedInteractionRegistration, error) {
	return f(ctx, tree, info)
}

func TestPreflightKind3RegistrationVerifiesSignedReceiptAndAuthenticatedRoster(t *testing.T) {
	tuple, admission, leaves, botList, participantList := validInteractionRosterFixture()
	private := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	wire := interactionReceiptWire{Version: admission.Version, AdmissionID: admission.AdmissionID, InstanceID: tuple.InstanceID, InstallationID: tuple.InstallationID, CommunityID: tuple.CommunityID, BotUserID: tuple.BotUserID, BotDeviceID: tuple.BotDeviceID, ParticipantUserID: tuple.ParticipantUserID, ParticipantDeviceID: tuple.ParticipantDeviceID, GroupID: tuple.GroupID, EligibilityRevision: admission.EligibilityRevision, NotAfter: admission.NotAfter}
	payload, err := cborx.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	sig := ed25519.Sign(private, append([]byte("dilla bot interaction admission v1\x00"), payload...))
	binding := interactionBindingForTest(t, tuple)
	req := InteractionRegistrationPreflight{Expected: tuple, MinimumEligibilityRevision: 1, ExpectedPolicyVersion: 1, ExpectedE2EEVersion: 1, ExpectedMediaVersion: 0, ReceiptPayload: payload, ReceiptSignature: sig, PinnedInstanceKey: private.Public().(ed25519.PublicKey), BotDeviceList: botList, ParticipantDeviceList: participantList, Now: 100, Registration: RegisterRequest{GroupID: tuple.GroupID, Binding: binding, GroupInfo: []byte{1}, RatchetTree: []byte{2}}}
	extraLeaf := false
	wrongBotKind := false
	extractor := rosterExtractorFunc(func(_ context.Context, tree, info []byte) (AuthenticatedInteractionRegistration, error) {
		if string(tree) != "\x02" || string(info) != "\x01" {
			t.Fatal("extractor did not receive submitted tree and GroupInfo")
		}
		out := make([]mlswasi.RosterLeaf, len(leaves))
		for i, leaf := range leaves {
			out[i] = mlswasi.RosterLeaf{UserID: leaf.UserID[:], DeviceID: leaf.DeviceID[:], DSKPub: leaf.DSKPub, Tier: leaf.Tier, Kind: leaf.Kind}
		}
		if wrongBotKind {
			out[0].Kind = 0
		}
		count := uint64(2)
		if extraLeaf {
			user, device := interactionID(90), interactionID(91)
			//nolint:makezero // The two existing roster leaves must remain before the optional third.
			out = append(out, mlswasi.RosterLeaf{UserID: user[:], DeviceID: device[:], DSKPub: make([]byte, 32)})
			count++
		}
		return AuthenticatedInteractionRegistration{GroupID: tuple.GroupID[:], Binding: binding, LeafCount: count, Roster: out}, nil
	})
	if err := PreflightKind3Registration(context.Background(), extractor, req); err != nil {
		t.Fatalf("preflight: %v", err)
	}
	extraLeaf = true
	if err := PreflightKind3Registration(context.Background(), extractor, req); err == nil {
		t.Fatal("preflight accepted an MLS group with an extra leaf")
	}
	extraLeaf = false
	wrongBotKind = true
	if err := PreflightKind3Registration(context.Background(), extractor, req); err == nil {
		t.Fatal("preflight accepted a human-kind Bot leaf")
	}
}

func TestPreflightKind3RegistrationRejectsWrongGroupAndStaleOrInvalidReceipt(t *testing.T) {
	tuple, admission, _, botList, participantList := validInteractionRosterFixture()
	private := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	wire := interactionReceiptWire{Version: admission.Version, AdmissionID: admission.AdmissionID, InstanceID: tuple.InstanceID, InstallationID: tuple.InstallationID, CommunityID: tuple.CommunityID, BotUserID: tuple.BotUserID, BotDeviceID: tuple.BotDeviceID, ParticipantUserID: tuple.ParticipantUserID, ParticipantDeviceID: tuple.ParticipantDeviceID, GroupID: tuple.GroupID, EligibilityRevision: admission.EligibilityRevision, NotAfter: admission.NotAfter}
	payload, _ := cborx.Marshal(wire)
	sig := ed25519.Sign(private, append([]byte("dilla bot interaction admission v1\x00"), payload...))
	binding := interactionBindingForTest(t, tuple)
	base := InteractionRegistrationPreflight{Expected: tuple, MinimumEligibilityRevision: 1, ExpectedPolicyVersion: 1, ExpectedE2EEVersion: 1, ExpectedMediaVersion: 0, ReceiptPayload: payload, ReceiptSignature: sig, PinnedInstanceKey: private.Public().(ed25519.PublicKey), BotDeviceList: botList, ParticipantDeviceList: participantList, Now: 100, Registration: RegisterRequest{GroupID: tuple.GroupID, Binding: binding, GroupInfo: []byte{1}, RatchetTree: []byte{2}}}
	extractor := rosterExtractorFunc(func(context.Context, []byte, []byte) (AuthenticatedInteractionRegistration, error) {
		return AuthenticatedInteractionRegistration{GroupID: tuple.GroupID[:], Binding: binding, LeafCount: 2}, nil
	})
	for _, tc := range []struct {
		name   string
		mutate func(*InteractionRegistrationPreflight)
	}{
		{"wrong group", func(r *InteractionRegistrationPreflight) { r.Registration.GroupID = interactionID(99) }},
		{"stale revision", func(r *InteractionRegistrationPreflight) {
			r.MinimumEligibilityRevision = admission.EligibilityRevision + 1
		}},
		{"legacy v1 kind-3 binding", func(r *InteractionRegistrationPreflight) {
			r.Registration.Binding, _ = cborx.Marshal(Binding{V: 1, InstanceID: tuple.InstanceID, Kind: 3, TargetID: tuple.ParticipantUserID})
		}},
		{"cross-installation binding", func(r *InteractionRegistrationPreflight) {
			r.Registration.Binding = alteredInteractionBinding(tuple, "installation")
		}},
		{"cross-community binding", func(r *InteractionRegistrationPreflight) {
			r.Registration.Binding = alteredInteractionBinding(tuple, "community")
		}},
		{"wrong participant device binding", func(r *InteractionRegistrationPreflight) {
			r.Registration.Binding = alteredInteractionBinding(tuple, "participant device")
		}},
		{"wrong participant user binding", func(r *InteractionRegistrationPreflight) {
			r.Registration.Binding = alteredInteractionBinding(tuple, "participant user")
		}},
		{"wrong Bot binding", func(r *InteractionRegistrationPreflight) {
			r.Registration.Binding = alteredInteractionBinding(tuple, "bot")
		}},
		{"wrong policy version", func(r *InteractionRegistrationPreflight) {
			r.Registration.Binding = alteredInteractionBinding(tuple, "policy version")
		}},
		{"wrong E2EE version", func(r *InteractionRegistrationPreflight) {
			r.Registration.Binding = alteredInteractionBinding(tuple, "E2EE version")
		}},
		{"wrong media version", func(r *InteractionRegistrationPreflight) {
			r.Registration.Binding = alteredInteractionBinding(tuple, "media version")
		}},
		{"bad receipt signature", func(r *InteractionRegistrationPreflight) {
			r.ReceiptSignature = append([]byte(nil), r.ReceiptSignature...)
			r.ReceiptSignature[0] ^= 1
		}},
		{"oversized receipt payload", func(r *InteractionRegistrationPreflight) {
			r.ReceiptPayload = make([]byte, maxInteractionAdmissionPayload+1)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := base
			tc.mutate(&r)
			if err := PreflightKind3Registration(context.Background(), extractor, r); err == nil {
				t.Fatal("preflight unexpectedly accepted invalid input")
			}
		})
	}
}
