package ds

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/mlswasi"
)

type interactionRosterFixtureMetadata struct {
	BindingHex              string `json:"binding_cbor_hex"`
	GroupIDHex              string `json:"group_id_hex"`
	InstanceIDHex           string `json:"instance_id_hex"`
	BotUserIDHex            string `json:"bot_user_id_hex"`
	BotDeviceIDHex          string `json:"bot_device_id_hex"`
	BotDSKPublicHex         string `json:"bot_dsk_public_hex"`
	BotSSKPublicHex         string `json:"bot_ssk_public_hex"`
	InstallationIDHex       string `json:"installation_id_hex"`
	CommunityIDHex          string `json:"community_id_hex"`
	ParticipantUserIDHex    string `json:"participant_user_id_hex"`
	ParticipantDeviceIDHex  string `json:"participant_device_id_hex"`
	ParticipantDSKPublicHex string `json:"participant_dsk_public_hex"`
	ParticipantSSKPublicHex string `json:"participant_ssk_public_hex"`
	PolicyVersion           uint64 `json:"policy_version"`
	ExpectedRosterLeaves    int    `json:"expected_roster_leaves"`
}

func readInteractionFixture(t *testing.T, leaf string) ([]byte, []byte, interactionRosterFixtureMetadata) {
	t.Helper()
	root := filepath.Join("dstest", "testdata", "kind3-interaction")
	read := func(name string) []byte {
		b, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatalf("read kind-3 fixture %s: %v", name, err)
		}
		return b
	}
	var meta interactionRosterFixtureMetadata
	if err := json.Unmarshal(read("metadata.json"), &meta); err != nil {
		t.Fatalf("decode fixture metadata: %v", err)
	}
	prefix := ""
	if leaf != "" {
		prefix = leaf + "."
	}
	return read(prefix + "ratchet_tree.mls"), read(prefix + "group_info.mls"), meta
}

func fixtureID(t *testing.T, name, value string) id.ID {
	t.Helper()
	b, err := hex.DecodeString(value)
	if err != nil || len(b) != 16 {
		t.Fatalf("fixture %s id is invalid", name)
	}
	var out id.ID
	copy(out[:], b)
	return out
}

func fixtureBytes(t *testing.T, name, value string) []byte {
	t.Helper()
	b, err := hex.DecodeString(value)
	if err != nil {
		t.Fatalf("fixture %s: %v", name, err)
	}
	return b
}

func TestKind3InteractionFixtureAuthenticatesExactTwoMemberRoster(t *testing.T) {
	ctx := context.Background()
	wasmBytes, err := os.ReadFile(filepath.Join("..", "mlswasi", "testdata", "dilla_core_wasi.wasm"))
	if err != nil {
		t.Fatalf("read WASI ABI module: %v", err)
	}
	wasm, err := mlswasi.New(ctx, wasmBytes, mlswasi.Options{PoolSize: 1, CacheDir: t.TempDir()})
	if err != nil {
		t.Fatalf("start WASI ABI v10: %v", err)
	}
	defer wasm.Close(ctx)
	inst, err := wasm.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire WASI instance: %v", err)
	}
	defer inst.Release()

	tree, groupInfo, meta := readInteractionFixture(t, "")
	root := filepath.Join("dstest", "testdata", "kind3-interaction")
	read := func(name string) []byte {
		b, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatalf("read fixture %s: %v", name, err)
		}
		return b
	}
	roster, err := inst.ExtractAuthenticatedRoster(ctx, tree, groupInfo)
	if err != nil {
		t.Fatalf("WASI ExtractAuthenticatedRoster: %v", err)
	}
	if len(roster) != meta.ExpectedRosterLeaves || len(roster) != 2 {
		t.Fatalf("WASI roster leaves = %d, want exactly two", len(roster))
	}
	group, err := inst.PublicGroupFromExternal(ctx, tree, groupInfo)
	if err != nil {
		t.Fatalf("WASI import kind-3 group: %v", err)
	}
	defer group.Close(ctx)
	state, err := group.State(ctx)
	if err != nil {
		t.Fatalf("WASI group state: %v", err)
	}
	if got, want := state.Binding, fixtureBytes(t, "v2 binding", meta.BindingHex); string(got) != string(want) {
		t.Fatalf("authenticated group binding = %x, want exact v2 binding %x", got, want)
	}

	leaves := make([]AuthenticatedInteractionLeaf, 0, len(roster))
	for _, leaf := range roster {
		var user, device id.ID
		copy(user[:], leaf.UserID)
		copy(device[:], leaf.DeviceID)
		leaves = append(leaves, AuthenticatedInteractionLeaf{UserID: user, DeviceID: device, DSKPub: leaf.DSKPub, Tier: leaf.Tier, Kind: leaf.Kind})
	}
	wantBotUser := fixtureID(t, "Bot user", meta.BotUserIDHex)
	wantBotDevice := fixtureID(t, "Bot device", meta.BotDeviceIDHex)
	wantParticipantUser := fixtureID(t, "participant user", meta.ParticipantUserIDHex)
	wantParticipantDevice := fixtureID(t, "participant device", meta.ParticipantDeviceIDHex)
	for _, want := range []struct {
		user, device id.ID
		dsk          string
		kind         uint8
	}{
		{wantBotUser, wantBotDevice, meta.BotDSKPublicHex, 1},
		{wantParticipantUser, wantParticipantDevice, meta.ParticipantDSKPublicHex, 0},
	} {
		found := false
		for _, got := range leaves {
			if got.UserID == want.user && got.DeviceID == want.device && got.Kind == want.kind && got.Tier == 0 && string(got.DSKPub) == string(fixtureBytes(t, "DSK", want.dsk)) {
				found = true
			}
		}
		if !found {
			t.Fatalf("expected credential-backed leaf user=%x device=%x kind=%d", want.user, want.device, want.kind)
		}
	}

	botList, err := inst.DeviceListEntries(ctx, read("bot_device_list.cbor"), fixtureBytes(t, "Bot SSK", meta.BotSSKPublicHex), wantBotUser[:])
	if err != nil {
		t.Fatalf("verify Bot signed device list: %v", err)
	}
	participantList, err := inst.DeviceListEntries(ctx, read("participant_device_list.cbor"), fixtureBytes(t, "participant SSK", meta.ParticipantSSKPublicHex), wantParticipantUser[:])
	if err != nil {
		t.Fatalf("verify participant signed device list: %v", err)
	}
	tuple := InteractionAdmissionTuple{
		InstanceID: fixtureID(t, "instance", meta.InstanceIDHex), InstallationID: fixtureID(t, "installation", meta.InstallationIDHex),
		CommunityID: fixtureID(t, "community", meta.CommunityIDHex), BotUserID: wantBotUser, BotDeviceID: wantBotDevice,
		ParticipantUserID: wantParticipantUser, ParticipantDeviceID: wantParticipantDevice, GroupID: fixtureID(t, "group", meta.GroupIDHex),
	}
	admission := InteractionAdmission{Version: 1, AdmissionID: interactionID(0x91), InteractionAdmissionTuple: tuple, EligibilityRevision: 1, NotAfter: 200}
	if err := VerifyInteractionRoster(tuple, 1, admission, leaves, botList, participantList, 100); err != nil {
		t.Fatalf("exact authenticated two-member fixture refused: %v", err)
	}
	issuer := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	proof, err := cborx.Marshal(interactionReceiptWire{
		Version: 1, AdmissionID: admission.AdmissionID, InstanceID: tuple.InstanceID,
		InstallationID: tuple.InstallationID, CommunityID: tuple.CommunityID,
		BotUserID: tuple.BotUserID, BotDeviceID: tuple.BotDeviceID,
		ParticipantUserID: tuple.ParticipantUserID, ParticipantDeviceID: tuple.ParticipantDeviceID,
		GroupID: tuple.GroupID, EligibilityRevision: admission.EligibilityRevision, NotAfter: admission.NotAfter,
	})
	if err != nil {
		t.Fatalf("encode signed fixture admission: %v", err)
	}
	registration := RegisterRequest{GroupID: tuple.GroupID, Binding: fixtureBytes(t, "v2 binding", meta.BindingHex), GroupInfo: groupInfo, RatchetTree: tree}
	preflight := InteractionRegistrationPreflight{
		Expected: tuple, MinimumEligibilityRevision: 1, ExpectedPolicyVersion: meta.PolicyVersion,
		ExpectedE2EEVersion: 1, ExpectedMediaVersion: 0,
		ReceiptPayload: proof, ReceiptSignature: ed25519.Sign(issuer, append([]byte(interactionAdmissionDomain), proof...)),
		PinnedInstanceKey: issuer.Public().(ed25519.PublicKey),
		BotDeviceList:     botList, ParticipantDeviceList: participantList,
		Now: 100, Registration: registration,
	}
	if err := PreflightKind3Registration(ctx, WASIInteractionRegistrationExtractor{Instance: inst}, preflight); err != nil {
		t.Fatalf("real WASI-backed kind-3 preflight refused fixture: %v", err)
	}

	extraTree, extraInfo, _ := readInteractionFixture(t, "extra")
	extraRoster, err := inst.ExtractAuthenticatedRoster(ctx, extraTree, extraInfo)
	if err != nil {
		t.Fatalf("WASI ExtractAuthenticatedRoster for extra-leaf fixture: %v", err)
	}
	if len(extraRoster) != 3 {
		t.Fatalf("negative fixture roster leaves = %d, want 3", len(extraRoster))
	}
	extraLeaves := make([]AuthenticatedInteractionLeaf, 0, len(extraRoster))
	for _, leaf := range extraRoster {
		var user, device id.ID
		copy(user[:], leaf.UserID)
		copy(device[:], leaf.DeviceID)
		extraLeaves = append(extraLeaves, AuthenticatedInteractionLeaf{UserID: user, DeviceID: device, DSKPub: leaf.DSKPub, Tier: leaf.Tier, Kind: leaf.Kind})
	}
	if err := VerifyInteractionRoster(tuple, 1, admission, extraLeaves, botList, participantList, 100); err == nil {
		t.Fatal("admission accepted an authenticated MLS roster containing an extra leaf")
	}
}
