package ds

import (
	"testing"

	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
)

func healBindingV1ForTest(t *testing.T, b Binding) []byte {
	t.Helper()
	b.V = 1
	encoded, err := cborx.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func healBindingV2ForTest(t *testing.T, b interactionBindingV2) []byte {
	t.Helper()
	b.Version = 2
	encoded, err := cborx.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func TestCheckReseededBindingPreservesV1Semantics(t *testing.T) {
	community, target := id.New(), id.New()
	want := Binding{InstanceID: id.New(), CommunityID: &community, TargetID: target, Kind: 2, PolicyVersion: 1, E2EEVersion: 1}
	got := want
	got.PolicyVersion = 9
	if err := checkReseededBinding(healBindingV1ForTest(t, want), healBindingV1ForTest(t, got)); err != nil {
		t.Fatalf("v1 policy snapshot change should remain compatible: %v", err)
	}
}

func TestCheckReseededBindingRejectsV2MarkerInLegacyShape(t *testing.T) {
	want := Binding{InstanceID: id.New(), TargetID: id.New(), Kind: 3, E2EEVersion: 1}
	stored := healBindingV1ForTest(t, want)
	wrongVersion := want
	wrongVersion.V = 2
	uploaded, err := cborx.Marshal(wrongVersion)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkReseededBinding(stored, uploaded); err == nil {
		t.Fatal("accepted an eight-field v2 marker as a legacy v1 binding")
	}
}

func TestCheckReseededBindingAcceptsSameV2IdentityAndPolicySnapshotChange(t *testing.T) {
	want := interactionBindingV2{InstanceID: id.New(), BotUserID: id.New(), Kind: 3, PolicyVersion: 4, E2EEVersion: 1, InstallationID: id.New(), CommunityID: id.New(), ParticipantUserID: id.New(), ParticipantDeviceID: id.New()}
	got := want
	got.PolicyVersion++ // Policy version is a creation snapshot, as for v1 bindings.
	if err := checkReseededBinding(healBindingV2ForTest(t, want), healBindingV2ForTest(t, got)); err != nil {
		t.Fatalf("v2 binding with unchanged identity and protocol versions refused: %v", err)
	}
}

func TestCheckReseededBindingRefusesChangedV2ImmutableField(t *testing.T) {
	want := interactionBindingV2{InstanceID: id.New(), BotUserID: id.New(), Kind: 3, PolicyVersion: 4, E2EEVersion: 1, InstallationID: id.New(), CommunityID: id.New(), ParticipantUserID: id.New(), ParticipantDeviceID: id.New()}
	for _, change := range []struct {
		name string
		edit func(*interactionBindingV2)
	}{
		{"instance", func(b *interactionBindingV2) { b.InstanceID = id.New() }},
		{"bot", func(b *interactionBindingV2) { b.BotUserID = id.New() }},
		{"kind", func(b *interactionBindingV2) { b.Kind = 2 }},
		{"e2ee version", func(b *interactionBindingV2) { b.E2EEVersion++ }},
		{"media version", func(b *interactionBindingV2) { b.MediaVersion++ }},
		{"installation", func(b *interactionBindingV2) { b.InstallationID = id.New() }},
		{"community", func(b *interactionBindingV2) { b.CommunityID = id.New() }},
		{"participant user", func(b *interactionBindingV2) { b.ParticipantUserID = id.New() }},
		{"participant device", func(b *interactionBindingV2) { b.ParticipantDeviceID = id.New() }},
	} {
		t.Run(change.name, func(t *testing.T) {
			got := want
			change.edit(&got)
			if err := checkReseededBinding(healBindingV2ForTest(t, want), healBindingV2ForTest(t, got)); err == nil {
				t.Fatal("accepted reseed with changed v2 identity or protocol version")
			}
		})
	}
}

func TestCheckReseededBindingRefusesMalformedOrMixedVersions(t *testing.T) {
	v1 := healBindingV1ForTest(t, Binding{InstanceID: id.New(), TargetID: id.New(), Kind: 3, E2EEVersion: 1})
	v2 := healBindingV2ForTest(t, interactionBindingV2{InstanceID: id.New(), BotUserID: id.New(), Kind: 3, E2EEVersion: 1, InstallationID: id.New(), CommunityID: id.New(), ParticipantUserID: id.New(), ParticipantDeviceID: id.New()})
	for _, c := range []struct {
		name             string
		stored, uploaded []byte
	}{
		{"v1 stored and v2 uploaded", v1, v2},
		{"v2 stored and v1 uploaded", v2, v1},
		{"malformed stored", []byte{0x81, 0x02}, v2},
		{"malformed uploaded", v2, []byte{0x81, 0x02}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if err := checkReseededBinding(c.stored, c.uploaded); err == nil {
				t.Fatal("accepted malformed or mixed binding versions")
			}
		})
	}
}

func TestCheckReseededBindingRejectsZeroV2PolicyVersion(t *testing.T) {
	b := interactionBindingV2{InstanceID: id.New(), BotUserID: id.New(), Kind: 3, E2EEVersion: 1, InstallationID: id.New(), CommunityID: id.New(), ParticipantUserID: id.New(), ParticipantDeviceID: id.New()}
	encoded := healBindingV2ForTest(t, b)
	if err := checkReseededBinding(encoded, encoded); err == nil {
		t.Fatal("accepted a v2 interaction binding with no policy version")
	}
}
