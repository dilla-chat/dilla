package mlswasi

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/dilla-chat/dilla/internal/cborx"
)

// abi3_test.go covers the two ABI v3 additions of dillad-1 task 27a (Ruling C, deviations B32
// and B33): device_list_entries, the guest's verified decoder of a user's signed device list
// (NV-B8), and public_group_staged_group_info_validate plus public_group_process's new_leaf,
// which together make an external commit's GroupInfo checkable before the merge.

// testDeviceEntry is one element of protocol/03-identity.md's `entries` array.
type testDeviceEntry struct {
	_         struct{} `cbor:",toarray"`
	DeviceID  []byte
	DSKPub    []byte
	Tier      uint64
	AddedAt   uint64
	RevokedAt *uint64
}

// testDeviceListUnsigned is the 5-element array the SSK signs.
type testDeviceListUnsigned struct {
	_        struct{} `cbor:",toarray"`
	V        uint64
	UserID   []byte
	Version  uint64
	PrevHash []byte
	Entries  []testDeviceEntry
}

// testDeviceList is the 6-element signed list.
type testDeviceList struct {
	_        struct{} `cbor:",toarray"`
	V        uint64
	UserID   []byte
	Version  uint64
	PrevHash []byte
	Entries  []testDeviceEntry
	SigSSK   []byte
}

// signDeviceListForTest builds a signed device list the way a client does: deterministic CBOR of
// the five unsigned fields, signed by the SSK under the "dilla devices v1" domain. A list built
// here verifying in the guest is also the proof that Go's deterministic encoding of the unsigned
// array is byte-identical to dilla-core's, which the signature covers.
func signDeviceListForTest(t *testing.T, ssk ed25519.PrivateKey, userID []byte, entries []testDeviceEntry) []byte {
	t.Helper()
	unsigned, err := cborx.Marshal(testDeviceListUnsigned{
		V: 1, UserID: userID, Version: 1, PrevHash: make([]byte, 32), Entries: entries,
	})
	if err != nil {
		t.Fatalf("encode the unsigned list: %v", err)
	}
	sig := ed25519.Sign(ssk, append([]byte("dilla devices v1"), unsigned...))
	list, err := cborx.Marshal(testDeviceList{
		V: 1, UserID: userID, Version: 1, PrevHash: make([]byte, 32), Entries: entries, SigSSK: sig,
	})
	if err != nil {
		t.Fatalf("encode the signed list: %v", err)
	}
	return list
}

func TestDeviceListEntriesReturnsTheEntriesOfAVerifiedList(t *testing.T) {
	ctx := context.Background()
	r := newTestRuntime(t, Options{PoolSize: 1})
	inst, err := r.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer inst.Release()

	ssk := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x5a}, 32))
	user := bytes.Repeat([]byte{0xd4}, 16)
	revokedAt := uint64(1_758_700_000)
	list := signDeviceListForTest(t, ssk, user, []testDeviceEntry{
		{DeviceID: bytes.Repeat([]byte{0x01}, 16), DSKPub: bytes.Repeat([]byte{0x11}, 32), Tier: 0, AddedAt: 1},
		{DeviceID: bytes.Repeat([]byte{0x02}, 16), DSKPub: bytes.Repeat([]byte{0x22}, 32), Tier: 1, AddedAt: 2, RevokedAt: &revokedAt},
	})

	entries, err := inst.DeviceListEntries(ctx, list, ssk.Public().(ed25519.PublicKey), user)
	if err != nil {
		t.Fatalf("DeviceListEntries: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}
	if !bytes.Equal(entries[0].DeviceID, bytes.Repeat([]byte{0x01}, 16)) ||
		!bytes.Equal(entries[0].DSKPub, bytes.Repeat([]byte{0x11}, 32)) ||
		entries[0].Tier != 0 || entries[0].Revoked {
		t.Errorf("entries[0] = %+v", entries[0])
	}
	if !bytes.Equal(entries[1].DeviceID, bytes.Repeat([]byte{0x02}, 16)) ||
		entries[1].Tier != 1 || !entries[1].Revoked {
		t.Errorf("entries[1] = %+v, want the revoked browser device", entries[1])
	}

	// Another key, another user, a flipped signature bit: each is E_CREDENTIAL.
	other := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x77}, 32))
	tampered := append([]byte(nil), list...)
	tampered[len(tampered)-1] ^= 0x01
	for name, call := range map[string]func() error{
		"another key": func() error {
			_, err := inst.DeviceListEntries(ctx, list, other.Public().(ed25519.PublicKey), user)
			return err
		},
		"another user": func() error {
			_, err := inst.DeviceListEntries(ctx, list, ssk.Public().(ed25519.PublicKey), bytes.Repeat([]byte{0xee}, 16))
			return err
		},
		"tampered": func() error {
			_, err := inst.DeviceListEntries(ctx, tampered, ssk.Public().(ed25519.PublicKey), user)
			return err
		},
	} {
		var abiErr *ABIError
		if err := call(); !errors.As(err, &abiErr) || abiErr.Code != "E_CREDENTIAL" {
			t.Errorf("%s: got %v, want an *ABIError E_CREDENTIAL", name, err)
		}
	}
}

// A member commit names its sender and no new leaf; the staged validation checks the GroupInfo
// of epoch n+1 under the key the committer's commit brings.
func TestTheStagedGroupInfoOfAMemberCommitVerifies(t *testing.T) {
	ctx := context.Background()
	r := newTestRuntime(t, Options{PoolSize: 1})
	inst, err := r.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer inst.Release()
	f := loadDS1500(t)
	merged, err := os.ReadFile(filepath.Join(fixtureDir, "commits", "09.group_info.mls"))
	if err != nil {
		t.Fatalf("commits/09.group_info.mls: %v", err)
	}

	g, err := inst.PublicGroupFromExternal(ctx, f.ratchetTree, f.groupInfo)
	if err != nil {
		t.Fatalf("PublicGroupFromExternal: %v", err)
	}
	p, err := g.Process(ctx, f.commits[9])
	if err != nil {
		t.Fatalf("Process(commits/09.mls): %v", err)
	}
	if p.NewLeaf != nil {
		t.Errorf("NewLeaf = %d on a member commit, want nil", *p.NewLeaf)
	}
	if p.Staged == nil {
		t.Fatal("a commit must stage")
	}
	check, err := g.ValidateStagedGroupInfo(ctx, *p.Staged, merged)
	if err != nil {
		t.Fatalf("ValidateStagedGroupInfo: %v", err)
	}
	if !check.SignatureOK {
		t.Error("the committer's GroupInfo did not verify under the key its commit brings")
	}
	if check.Epoch != f.manifest.Epoch+1 {
		t.Errorf("Epoch = %d, want %d", check.Epoch, f.manifest.Epoch+1)
	}

	var abiErr *ABIError
	if _, err := g.ValidateStagedGroupInfo(ctx, 9_999, merged); !errors.As(err, &abiErr) || abiErr.Code != "E_ABI_HANDLE" {
		t.Errorf("an unknown staged handle gave %v, want E_ABI_HANDLE", err)
	}
}
