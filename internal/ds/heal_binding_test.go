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
