package ds_test

// devicelist_test.go is NV-B8 resolved (task 27a, Ruling C, deviation B32): invariant 4's clause
// "its DSK is in the newest signed device list" now reads the user's stored list and has the guest
// decode it and verify its ssk_signature, so a valid Add passes the clause and every list that
// does not verify still refuses it.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"testing"

	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/ds"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/mlswasi"
	"github.com/dilla-chat/dilla/internal/store"
)

// listEntry is one element of protocol/03-identity.md's device-list `entries` array.
type listEntry struct {
	_         struct{} `cbor:",toarray"`
	DeviceID  []byte
	DSKPub    []byte
	Tier      uint64
	AddedAt   uint64
	RevokedAt *uint64
}

type listUnsigned struct {
	_        struct{} `cbor:",toarray"`
	V        uint64
	UserID   []byte
	Version  uint64
	PrevHash []byte
	Entries  []listEntry
}

type listSigned struct {
	_        struct{} `cbor:",toarray"`
	V        uint64
	UserID   []byte
	Version  uint64
	PrevHash []byte
	Entries  []listEntry
	SigSSK   []byte
}

// signedDeviceList is what a client publishes with PUT /v1/users/{id}/device-list: the
// 6-element list, signed by the SSK over "dilla devices v1" || the 5-element array.
func signedDeviceList(t *testing.T, ssk ed25519.PrivateKey, userID id.ID, entries []listEntry) []byte {
	t.Helper()
	unsigned, err := cborx.Marshal(listUnsigned{
		V: 1, UserID: userID[:], Version: 1, PrevHash: make([]byte, 32), Entries: entries,
	})
	if err != nil {
		t.Fatalf("encode the unsigned list: %v", err)
	}
	signed, err := cborx.Marshal(listSigned{
		V: 1, UserID: userID[:], Version: 1, PrevHash: make([]byte, 32), Entries: entries,
		SigSSK: ed25519.Sign(ssk, append([]byte("dilla devices v1"), unsigned...)),
	})
	if err != nil {
		t.Fatalf("encode the signed list: %v", err)
	}
	return signed
}

// userWithSSK writes a user whose users.ssk_pub is ssk's public half, the key a list is verified
// against. It must run before h.account for the same user, which creates the row only when absent.
func (h *dsHarness) userWithSSK(t *testing.T, userID id.ID, ssk ed25519.PrivateKey) {
	t.Helper()
	if err := h.repo.CreateUser(context.Background(), store.UserRow{
		ID:        userID,
		Username:  "u" + userID.String(),
		Display:   "u" + userID.String()[:8],
		UMKPub:    bytes.Repeat([]byte{1}, 32),
		SSKPub:    ssk.Public().(ed25519.PublicKey),
		SigUMKSSK: bytes.Repeat([]byte{3}, 64),
		Created:   h.clk.Now().Unix(),
	}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
}

// publishDeviceList stores a list as PUT /v1/users/{id}/device-list does: the blob verbatim.
func (h *dsHarness) publishDeviceList(t *testing.T, userID id.ID, blob []byte) {
	t.Helper()
	if err := h.repo.PutDeviceList(context.Background(), store.DeviceListRow{
		UserID: userID, Version: 1, Blob: blob,
		SSKSignature: blob[len(blob)-64:], PrevHash: make([]byte, 32), Created: h.clk.Now().Unix(),
	}); err != nil {
		t.Fatalf("PutDeviceList: %v", err)
	}
}

func testSSK(seed byte) ed25519.PrivateKey {
	return ed25519.NewKeyFromSeed(bytes.Repeat([]byte{seed}, 32))
}

// The verifier reports the dsk_pub of every UNREVOKED entry of a list that verifies.
func TestTheDeviceListVerifierReturnsTheUnrevokedKeysOfAVerifiedList(t *testing.T) {
	h := newDSHarness(t)
	user := id.New()
	ssk := testSSK(0x5a)
	h.userWithSSK(t, user, ssk)
	revoked := uint64(1_758_700_000)
	h.publishDeviceList(t, user, signedDeviceList(t, ssk, user, []listEntry{
		{DeviceID: bytes.Repeat([]byte{1}, 16), DSKPub: bytes.Repeat([]byte{0x11}, 32), AddedAt: 1},
		{DeviceID: bytes.Repeat([]byte{2}, 16), DSKPub: bytes.Repeat([]byte{0x22}, 32), AddedAt: 2, RevokedAt: &revoked},
	}))

	entries, err := ds.DeviceListsForTest(h.ds).Entries(context.Background(), nil, user)
	if err != nil {
		t.Fatalf("Entries: %v", err)
	}
	// The pair, not the key alone (security review F2): a row is listed only under both.
	if len(entries) != 1 || entries[0].DeviceID != id.ID(bytes.Repeat([]byte{1}, 16)) ||
		!bytes.Equal(entries[0].DSKPub, bytes.Repeat([]byte{0x11}, 32)) {
		t.Fatalf("entries = %v, want only the unrevoked device's id and key", entries)
	}
}

// Every list that does not verify is an error, and an error is a refusal: no list at all, a list
// signed by a key that is not the user's SSK, and a list naming another user.
func TestTheDeviceListVerifierFailsClosed(t *testing.T) {
	h := newDSHarness(t)
	ctx := context.Background()
	lists := ds.DeviceListsForTest(h.ds)
	entry := []listEntry{{DeviceID: bytes.Repeat([]byte{1}, 16), DSKPub: bytes.Repeat([]byte{0x11}, 32)}}

	noList := id.New()
	h.userWithSSK(t, noList, testSSK(0x01))
	if got, err := lists.Entries(ctx, nil, noList); err == nil || got != nil {
		t.Errorf("a user with no list: entries %x, err %v; want nil and an error", got, err)
	}

	wrongKey := id.New()
	h.userWithSSK(t, wrongKey, testSSK(0x02))
	h.publishDeviceList(t, wrongKey, signedDeviceList(t, testSSK(0x03), wrongKey, entry))
	if got, err := lists.Entries(ctx, nil, wrongKey); err == nil || got != nil {
		t.Errorf("a list another key signed: entries %x, err %v; want nil and an error", got, err)
	}

	otherUser := id.New()
	h.userWithSSK(t, otherUser, testSSK(0x04))
	h.publishDeviceList(t, otherUser, signedDeviceList(t, testSSK(0x04), id.New(), entry))
	if got, err := lists.Entries(ctx, nil, otherUser); err == nil || got != nil {
		t.Errorf("a list naming another user: entries %x, err %v; want nil and an error", got, err)
	}
}

// memberIdentity is the credential identity of the fixture member at `leaf`, read out of the
// guest's own tree, together with the device and user it names.
func (h *dsHarness) memberIdentity(t *testing.T, groupID id.ID, leaf uint32) ([]byte, id.ID, id.ID) {
	t.Helper()
	var identity []byte
	err := ds.WithGroupForTest(h.ds, context.Background(), groupID, func(g *mlswasi.PublicGroup) error {
		state, err := g.State(context.Background())
		if err != nil {
			return err
		}
		for _, m := range state.Members {
			if m.LeafIndex == leaf {
				identity = m.CredentialIdentity
			}
		}
		return nil
	})
	if err != nil || identity == nil {
		t.Fatalf("no member at leaf %d: %v", leaf, err)
	}
	device, user, err := ds.DecodeCredentialIdentityForTest(identity)
	if err != nil {
		t.Fatalf("decode the credential identity: %v", err)
	}
	return identity, device, user
}

// memberLeafKey is the signature key the fixture member at `leaf` carries in the guest's own tree:
// the key that member's device registered, for a test that models it honestly.
func (h *dsHarness) memberLeafKey(t *testing.T, groupID id.ID, leaf uint32) []byte {
	t.Helper()
	var key []byte
	err := ds.WithGroupForTest(h.ds, context.Background(), groupID, func(g *mlswasi.PublicGroup) error {
		state, err := g.State(context.Background())
		if err != nil {
			return err
		}
		for _, m := range state.Members {
			if m.LeafIndex == leaf {
				key = m.SignatureKey
			}
		}
		return nil
	})
	if err != nil || len(key) != 32 {
		t.Fatalf("no 32-byte leaf key at leaf %d: %v", leaf, err)
	}
	return key
}

// The clause the ruling asks for: an Add of a device that is known, unrevoked, in its user's newest
// signed device list and eligible under the ACL PASSES invariant 4's Add clause. The added device
// here is a real fixture member, so DenyUnlessMember (NV-B6) sees its user in the group.
func TestAValidAddPassesInvariant4sAddClause(t *testing.T) {
	h := newDSHarness(t)
	reg, _ := h.mustRegister(t)
	identity, device, user := h.memberIdentity(t, reg.GroupID, 1)
	ssk := testSSK(0x5b)
	h.userWithSSK(t, user, ssk)
	// The device is registered, and listed, under the key its leaf really carries: an honest Add
	// (hardening C binds the added leaf to the device's registered key).
	dsk := h.memberLeafKey(t, reg.GroupID, 1)
	h.accountWithKey(t, user, device, dsk)
	add := mlswasi.AppliedProposal{Kind: mlswasi.ProposalAdd, CredentialIdentity: identity, SignatureKey: dsk}

	// No list yet: refused, as before this task.
	if err := ds.CheckAddedMemberForTest(h.ds, context.Background(), reg.GroupID, add); !hasRule(err, "add_key_package") {
		t.Fatalf("with no device list: got %v, want add_key_package", err)
	}

	h.publishDeviceList(t, user, signedDeviceList(t, ssk, user, []listEntry{
		{DeviceID: device[:], DSKPub: dsk, AddedAt: 1},
	}))
	if err := ds.CheckAddedMemberForTest(h.ds, context.Background(), reg.GroupID, add); err != nil {
		t.Fatalf("a listed, eligible device was refused: %v", err)
	}
}

// And the same device, revoked in the newest list, is refused by the same clause.
func TestAnAddOfADeviceTheNewestListRevokesIsRefused(t *testing.T) {
	h := newDSHarness(t)
	reg, _ := h.mustRegister(t)
	identity, device, user := h.memberIdentity(t, reg.GroupID, 1)
	ssk := testSSK(0x5c)
	h.userWithSSK(t, user, ssk)
	h.account(t, user, device)
	revoked := uint64(1_758_700_000)
	h.publishDeviceList(t, user, signedDeviceList(t, ssk, user, []listEntry{
		{DeviceID: device[:], DSKPub: bytes.Repeat([]byte{4}, 32), AddedAt: 1, RevokedAt: &revoked},
	}))
	add := mlswasi.AppliedProposal{Kind: mlswasi.ProposalAdd, CredentialIdentity: identity}
	if err := ds.CheckAddedMemberForTest(h.ds, context.Background(), reg.GroupID, add); !hasRule(err, "add_key_package") {
		t.Fatalf("got %v, want add_key_package", err)
	}
}

// Through the real commit path: commits/00.mls adds 256 devices. With every added device known to
// the instance and listed in a list its user signed, all 256 Adds pass invariant 4's Add clause —
// each list verified inside the instance the commit already holds — and the commit is refused one
// step later, by the GroupInfo check (the base request carries the epoch-n GroupInfo). Before
// NV-B8 was resolved no Add could get past the device-list half of the clause at all.
func TestACommitWhoseAddsAreAllListedPassesTheAddClause(t *testing.T) {
	h := newDSHarness(t)
	reg, session := h.mustRegister(t)
	commit := fixtureFile(t, "commits/00.mls")

	var identities, keys [][]byte
	err := ds.WithGroupForTest(h.ds, context.Background(), reg.GroupID, func(g *mlswasi.PublicGroup) error {
		p, err := g.Process(context.Background(), commit)
		if err != nil {
			return err
		}
		for _, a := range p.Applied {
			identities = append(identities, a.CredentialIdentity)
			keys = append(keys, a.SignatureKey)
		}
		return g.Discard(context.Background(), *p.Staged)
	})
	if err != nil {
		t.Fatalf("read the Adds: %v", err)
	}
	if len(identities) != 256 {
		t.Fatalf("commits/00.mls applies %d proposals, want 256 Adds", len(identities))
	}

	// One signed list per added user, naming every device of that user the commit adds. Each
	// device is registered and listed under the key its KeyPackage's leaf carries, as the honest
	// device that built the package registered it (hardening C).
	byUser := map[id.ID][]listEntry{}
	var order []id.ID
	for i, identity := range identities {
		device, user, err := ds.DecodeCredentialIdentityForTest(identity)
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if _, seen := byUser[user]; !seen {
			order = append(order, user)
			h.userWithSSK(t, user, testSSK(0x60))
		}
		h.accountWithKey(t, user, device, keys[i])
		byUser[user] = append(byUser[user], listEntry{
			DeviceID: device[:], DSKPub: keys[i], AddedAt: 1,
		})
	}
	for _, user := range order {
		h.publishDeviceList(t, user, signedDeviceList(t, testSSK(0x60), user, byUser[user]))
	}

	before := h.wasmCalls("device_list_entries")
	_, err = h.ds.Commit(context.Background(), session, reg.GroupID, ds.CommitRequest{
		Epoch: 6, Commit: commit, GroupInfo: dsFixture(t).groupInfo,
	})
	if !hasRule(err, "group_info_epoch") {
		t.Fatalf("got %v, want every Add to pass and E_COMMIT_INVALID/group_info_epoch", err)
	}
	if got := h.wasmCalls("device_list_entries") - before; got != 256 {
		t.Errorf("device_list_entries ran %d times, want once per Add (256)", got)
	}
}

// The ruling of hardening C, point (b): an Add's leaf is bound to the registered key of the device
// its credential names. commits/00.mls adds 256 devices; here every one of them is known, listed
// and eligible, but registered (and listed) under a key that is NOT the key its KeyPackage's leaf
// carries — the shape a committer produces by minting a KeyPackage in another device's name under
// its own key. The Add clause refuses the commit before the GroupInfo is looked at.
func TestACommitWhoseAddedLeafKeysAreNotTheDevicesRegisteredKeysIsRefused(t *testing.T) {
	h := newDSHarness(t)
	reg, session := h.mustRegister(t)
	commit := fixtureFile(t, "commits/00.mls")

	var identities [][]byte
	err := ds.WithGroupForTest(h.ds, context.Background(), reg.GroupID, func(g *mlswasi.PublicGroup) error {
		p, err := g.Process(context.Background(), commit)
		if err != nil {
			return err
		}
		for _, a := range p.Applied {
			identities = append(identities, a.CredentialIdentity)
		}
		return g.Discard(context.Background(), *p.Staged)
	})
	if err != nil {
		t.Fatalf("read the Adds: %v", err)
	}
	byUser := map[id.ID][]listEntry{}
	var order []id.ID
	for _, identity := range identities {
		device, user, err := ds.DecodeCredentialIdentityForTest(identity)
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if _, seen := byUser[user]; !seen {
			order = append(order, user)
			h.userWithSSK(t, user, testSSK(0x61))
		}
		h.account(t, user, device) // devices.dsk_pub = 0x04…, not the KeyPackage's leaf key
		byUser[user] = append(byUser[user], listEntry{
			DeviceID: device[:], DSKPub: bytes.Repeat([]byte{4}, 32), AddedAt: 1,
		})
	}
	for _, user := range order {
		h.publishDeviceList(t, user, signedDeviceList(t, testSSK(0x61), user, byUser[user]))
	}
	before := h.handshakeCount(t, reg.GroupID)

	_, err = h.ds.Commit(context.Background(), session, reg.GroupID, ds.CommitRequest{
		Epoch: 6, Commit: commit, GroupInfo: dsFixture(t).groupInfo,
	})
	if !hasRule(err, "add_key_package") {
		t.Fatalf("got %v, want E_COMMIT_INVALID/add_key_package", err)
	}
	if got := h.handshakeCount(t, reg.GroupID); got != before {
		t.Errorf("a refused commit wrote %d handshake rows", got-before)
	}
}

// G3 of the second hardening review: POST /commit checks the uploaded GroupInfo against the group
// it merges to, as heal does: its group id and its tree hash, not only its epoch and signature.
// commits/00.mls (256 Adds by leaf 0) is uploaded with commits/09.group_info.mls: the GroupInfo of
// epoch 7, of this group, signed by leaf 0 - but of the tree the fixture's self-update produced,
// not of the tree these Adds produce. Every Add is honest (as in the test above), so the commit
// passes every other clause; the GroupInfo would be served to every device that resyncs or joins
// until the next commit, and none could build an external commit from it. Refused with the route's
// rule for a bad GroupInfo, and nothing is written: the epoch, the handshake log and the stored
// GroupInfo stay as they were. commits/09.mls with its own GroupInfo (TestARefusedCommitEvictsNobody)
// is the accepted control.
func TestACommitWhoseGroupInfoDescribesAnotherTreeIsRefused(t *testing.T) {
	h := newDSHarness(t)
	reg, session := h.mustRegister(t)
	commit := fixtureFile(t, "commits/00.mls")

	var identities, keys [][]byte
	err := ds.WithGroupForTest(h.ds, context.Background(), reg.GroupID, func(g *mlswasi.PublicGroup) error {
		p, err := g.Process(context.Background(), commit)
		if err != nil {
			return err
		}
		for _, a := range p.Applied {
			identities = append(identities, a.CredentialIdentity)
			keys = append(keys, a.SignatureKey)
		}
		return g.Discard(context.Background(), *p.Staged)
	})
	if err != nil {
		t.Fatalf("read the Adds: %v", err)
	}
	byUser := map[id.ID][]listEntry{}
	var order []id.ID
	for i, identity := range identities {
		device, user, err := ds.DecodeCredentialIdentityForTest(identity)
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if _, seen := byUser[user]; !seen {
			order = append(order, user)
			h.userWithSSK(t, user, testSSK(0x62))
		}
		h.accountWithKey(t, user, device, keys[i])
		byUser[user] = append(byUser[user], listEntry{DeviceID: device[:], DSKPub: keys[i], AddedAt: 1})
	}
	for _, user := range order {
		h.publishDeviceList(t, user, signedDeviceList(t, testSSK(0x62), user, byUser[user]))
	}
	before, err := h.repo.GetGroup(context.Background(), reg.GroupID)
	if err != nil {
		t.Fatalf("GetGroup: %v", err)
	}
	handshakes := h.handshakeCount(t, reg.GroupID)

	_, err = h.ds.Commit(context.Background(), session, reg.GroupID, ds.CommitRequest{
		Epoch: 6, Commit: commit, GroupInfo: fixtureFile(t, "commits/09.group_info.mls"),
	})
	if !hasRule(err, "group_info") {
		t.Fatalf("got %v, want E_COMMIT_INVALID/group_info: the GroupInfo's tree hash is not the merged tree's", err)
	}
	after, err := h.repo.GetGroup(context.Background(), reg.GroupID)
	if err != nil {
		t.Fatalf("GetGroup: %v", err)
	}
	if after.Epoch != before.Epoch || !bytes.Equal(after.GroupInfoBlob, before.GroupInfoBlob) ||
		!bytes.Equal(after.TreeHash, before.TreeHash) {
		t.Errorf("a refused commit moved the group: epoch %d -> %d", before.Epoch, after.Epoch)
	}
	if got := h.handshakeCount(t, reg.GroupID); got != handshakes {
		t.Errorf("a refused commit wrote %d handshake rows", got-handshakes)
	}
	// The instance's own view is unchanged too: the honest self-update of epoch 6 still lands.
	if _, err := h.ds.Commit(context.Background(), session, reg.GroupID, ds.CommitRequest{
		Epoch: 6, Commit: fixtureFile(t, "commits/09.mls"), GroupInfo: fixtureFile(t, "commits/09.group_info.mls"),
	}); err != nil {
		t.Fatalf("the honest commit of epoch 6 after the refusal: %v", err)
	}
}

func hasRule(err error, rule string) bool {
	var dsErr *ds.Error
	return errors.As(err, &dsErr) && dsErr.Code == "E_COMMIT_INVALID" && dsErr.Rule == rule
}

// Verify checks a CANDIDATE list — one PUT /v1/users/{id}/device-list has not stored yet — in the
// guest: it reads no stored list, reports every entry with its revoked flag, and refuses with
// E_CREDENTIAL what does not decode or verify.
func TestVerifyChecksACandidateListInTheGuest(t *testing.T) {
	h := newDSHarness(t)
	ctx := context.Background()
	lists := ds.DeviceListsForTest(h.ds)
	user := id.New()
	ssk := testSSK(0x5a)
	sskPub := ssk.Public().(ed25519.PublicKey)
	revoked := uint64(1_758_700_000)
	blob := signedDeviceList(t, ssk, user, []listEntry{
		{DeviceID: bytes.Repeat([]byte{1}, 16), DSKPub: bytes.Repeat([]byte{0x11}, 32), AddedAt: 1},
		{DeviceID: bytes.Repeat([]byte{2}, 16), DSKPub: bytes.Repeat([]byte{0x22}, 32), Tier: 1, AddedAt: 2, RevokedAt: &revoked},
	})

	entries, err := lists.Verify(ctx, blob, sskPub, user)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if len(entries) != 2 || !bytes.Equal(entries[0].DeviceID, bytes.Repeat([]byte{1}, 16)) || entries[0].Revoked ||
		!bytes.Equal(entries[1].DeviceID, bytes.Repeat([]byte{2}, 16)) || !entries[1].Revoked || entries[1].Tier != 1 {
		t.Fatalf("entries = %+v, want the listed device and the revoked browser", entries)
	}
	if _, err := h.repo.GetDeviceList(ctx, user); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Verify stored or found a list: %v", err)
	}
	for name, call := range map[string]func() error{
		"another key": func() error {
			_, err := lists.Verify(ctx, blob, testSSK(0x77).Public().(ed25519.PublicKey), user)
			return err
		},
		"another user": func() error { _, err := lists.Verify(ctx, blob, sskPub, id.New()); return err },
		"not a list":   func() error { _, err := lists.Verify(ctx, []byte{0x80}, sskPub, user); return err },
	} {
		var abiErr *mlswasi.ABIError
		if err := call(); !errors.As(err, &abiErr) || abiErr.Code != "E_CREDENTIAL" {
			t.Errorf("%s: got %v, want an *mlswasi.ABIError E_CREDENTIAL", name, err)
		}
	}
	if _, err := ds.NewDeviceLists(h.repo, nil).Verify(ctx, blob, sskPub, user); !errors.Is(err, ds.ErrDeviceListUnavailable) {
		t.Fatalf("no runtime: %v, want ErrDeviceListUnavailable", err)
	}
}
