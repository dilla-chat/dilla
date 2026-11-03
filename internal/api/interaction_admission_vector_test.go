package api

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
)

func TestInteractionAdmissionSharedVector(t *testing.T) {
	data, err := os.ReadFile("../../protocol/vectors/bot.json")
	if err != nil {
		t.Fatal(err)
	}
	var corpus struct {
		AdmissionProof struct {
			Seed      string            `json:"seed"`
			PublicKey string            `json:"public_key"`
			Payload   string            `json:"payload_hex"`
			Proof     string            `json:"proof_hex"`
			IDs       map[string]string `json:"ids"`
			Revision  uint64            `json:"minimum_eligibility_revision"`
			NotAfter  uint64            `json:"not_after"`
		} `json:"admission_proof"`
	}
	if err := json.Unmarshal(data, &corpus); err != nil {
		t.Fatal(err)
	}
	v := corpus.AdmissionProof
	decode := func(name, value string) []byte {
		t.Helper()
		out, err := hex.DecodeString(value)
		if err != nil {
			t.Fatalf("decode %s: %v", name, err)
		}
		return out
	}
	parseID := func(name string) id.ID {
		t.Helper()
		value, err := id.Parse(v.IDs[name])
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		return value
	}
	seed := decode("seed", v.Seed)
	if len(seed) != ed25519.SeedSize {
		t.Fatalf("seed length = %d", len(seed))
	}
	private := ed25519.NewKeyFromSeed(seed)
	if got := hex.EncodeToString(private.Public().(ed25519.PublicKey)); got != v.PublicKey {
		t.Fatalf("derived public key %s != vector %s", got, v.PublicKey)
	}
	payload, err := cborx.Marshal(botInteractionAdmissionPayload{
		Version: 1, AdmissionID: parseID("admission"), InstanceID: parseID("instance"),
		InstallationID: parseID("installation"), CommunityID: parseID("community"),
		BotUserID: parseID("bot_user"), BotDeviceID: parseID("bot_device"),
		ParticipantUserID: parseID("participant_user"), ParticipantDeviceID: parseID("participant_device"),
		GroupID: parseID("group"), EligibilityRevision: v.Revision, NotAfter: v.NotAfter,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(payload); got != v.Payload {
		t.Fatalf("issuer payload %s != vector %s", got, v.Payload)
	}
	signature := ed25519.Sign(private, append([]byte("dilla bot interaction admission v1\x00"), payload...))
	proof, err := cborx.Marshal([]any{payload, signature})
	if err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(proof); got != v.Proof {
		t.Fatalf("issuer proof %s != vector %s", got, v.Proof)
	}
}

func TestInteractionContextSnapshotSharedVector(t *testing.T) {
	data, err := os.ReadFile("../../protocol/vectors/bot.json")
	if err != nil {
		t.Fatal(err)
	}
	var corpus struct {
		Context struct {
			Seed      string `json:"seed"`
			PublicKey string `json:"public_key"`
			Payload   string `json:"payload_hex"`
			Signature string `json:"signature_hex"`
		} `json:"interaction_context_snapshot"`
	}
	if err := json.Unmarshal(data, &corpus); err != nil {
		t.Fatal(err)
	}
	seed, err := hex.DecodeString(corpus.Context.Seed)
	if err != nil || len(seed) != ed25519.SeedSize {
		t.Fatalf("decode context seed: %v", err)
	}
	private := ed25519.NewKeyFromSeed(seed)
	if got := hex.EncodeToString(private.Public().(ed25519.PublicKey)); got != corpus.Context.PublicKey {
		t.Fatalf("context public key %s != vector %s", got, corpus.Context.PublicKey)
	}
	fixtureID := func(n byte) id.ID {
		value, _ := id.Parse(strings.Repeat(fmt.Sprintf("%02x", n), id.Size))
		return value
	}
	instance, installation, community := fixtureID(2), fixtureID(3), fixtureID(4)
	botUser, botDevice, participant, participantDevice, group := fixtureID(5), fixtureID(6), fixtureID(7), fixtureID(8), fixtureID(9)
	request, admission := fixtureID(10), fixtureID(1)
	list := func(uid id.ID, salt byte) interactionSnapshotListWire {
		blob := []byte{0x82, salt, 0x01}
		return snapshotListWire(interactionSnapshotDeviceList{UserID: uid, UMKPub: bytes.Repeat([]byte{salt}, 32), SSKPub: bytes.Repeat([]byte{salt + 1}, 32), SSKBindingSignature: bytes.Repeat([]byte{salt + 2}, 64), Version: 3, Blob: blob, Signature: bytes.Repeat([]byte{salt + 3}, 64), PrevHash: bytes.Repeat([]byte{salt + 4}, 32)})
	}
	admissionPayload, err := cborx.Marshal(botInteractionAdmissionPayload{Version: 1, AdmissionID: admission, InstanceID: instance, InstallationID: installation, CommunityID: community, BotUserID: botUser, BotDeviceID: botDevice, ParticipantUserID: participant, ParticipantDeviceID: participantDevice, GroupID: group, EligibilityRevision: 7, NotAfter: 130})
	if err != nil {
		t.Fatal(err)
	}
	admissionSignature := ed25519.Sign(private, append([]byte("dilla bot interaction admission v1\x00"), admissionPayload...))
	wire := botInteractionContextWire{Version: 1, RequestID: request, AdmissionID: admission, AdmissionPayload: admissionPayload, AdmissionSignature: admissionSignature, Now: 100, MinimumEligibilityRevision: 7, PolicyVersion: 9, E2EEVersion: 1, MediaVersion: 0, InstanceID: instance, InstanceKeyID: fixtureID(11), InstanceSignerPub: private.Public().(ed25519.PublicKey), Expected: interactionSnapshotTuple{InstanceID: instance, InstallationID: installation, CommunityID: community, BotUserID: botUser, BotDeviceID: botDevice, ParticipantUserID: participant, ParticipantDeviceID: participantDevice, GroupID: group}, Bot: list(botUser, 0x10), Participant: list(participant, 0x20)}
	payload, err := cborx.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	signature := ed25519.Sign(private, append([]byte(interactionContextDomain), payload...))
	if got := hex.EncodeToString(payload); got != corpus.Context.Payload {
		t.Fatalf("snapshot payload %s != vector %s", got, corpus.Context.Payload)
	}
	if got := hex.EncodeToString(signature); got != corpus.Context.Signature {
		t.Fatalf("snapshot signature %s != vector %s", got, corpus.Context.Signature)
	}
}
