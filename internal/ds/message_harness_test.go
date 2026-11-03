package ds_test

// message_harness_test.go is task 23's half of the delivery service's harness: the fixture group
// seen as one uploading device, the accounts rows the uploader-only delete rule reads, and a
// hand-framed `PrivateMessage` whose authenticated_data, epoch and total size a test chooses.
//
// It is a separate file for the same reason election_harness_test.go is: `dsHarness` is one type
// and harness_test.go is already the longest file in the package.

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"testing"

	"github.com/dilla-chat/dilla/internal/auth"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

// dsMessageGroup is the registered fixture group plus the one member device the message tests
// upload from. Epoch is a method rather than a field so a test reads "the group's epoch" at the
// call site; the value is the one `mls_groups` holds, which is what Upload compares the message's
// own epoch against.
type dsMessageGroup struct {
	id      id.ID
	device  id.ID
	session auth.Session
	epoch   uint64
}

func (g *dsMessageGroup) Epoch() uint64 { return g.epoch }

// group registers the committed fixture and returns it as one driveable uploader.
//
// It also writes the `users` and `devices` rows for that device. They are not decoration: R29's
// delete rule resolves the uploading DEVICE to its user through `store.Devices.GetDevice`, and
// the fixture's members exist only as MLS leaves — the guest's credentials, replayed into
// `mls_members` by Register — so without these rows DeleteMessage answers store.ErrNotFound and
// the rule under test never runs.
func (h *dsHarness) group(t *testing.T) *dsMessageGroup {
	t.Helper()
	ctx := context.Background()
	got, session := h.mustRegister(t)
	row, err := h.repo.GetGroup(ctx, got.GroupID)
	if err != nil {
		t.Fatalf("GetGroup: %v", err)
	}
	g := &dsMessageGroup{
		id: got.GroupID, device: session.DeviceID, session: session, epoch: row.Epoch,
	}
	if h.sessions == nil {
		h.sessions = map[id.ID]auth.Session{}
	}
	h.sessions[g.device] = session
	h.account(t, session.UserID, session.DeviceID)
	return g
}

// account writes one user and one of its devices. Both are real rows in the real store: `devices`
// has a foreign key onto `users`, so a device with no account is not a state the database can
// hold and a test that pretended otherwise would be testing a shape production never sees.
func (h *dsHarness) account(t *testing.T, userID, deviceID id.ID) {
	t.Helper()
	h.accountWithKey(t, userID, deviceID, bytes.Repeat([]byte{4}, 32))
}

// accountWithKey is account with the device registered under dsk: the key a device whose real
// MLS material a test drives (a fixture KeyPackage, a fixture member's leaf) registered, which is
// what the delivery service binds every new leaf to.
func (h *dsHarness) accountWithKey(t *testing.T, userID, deviceID id.ID, dsk []byte) {
	t.Helper()
	ctx := context.Background()
	if _, err := h.repo.GetUser(ctx, userID); err != nil {
		if err := h.repo.CreateUser(ctx, store.UserRow{
			ID:        userID,
			Username:  "u" + userID.String(),
			Display:   "u" + userID.String()[:8],
			Kind:      0,
			UMKPub:    bytes.Repeat([]byte{1}, 32),
			SSKPub:    bytes.Repeat([]byte{2}, 32),
			SigUMKSSK: bytes.Repeat([]byte{3}, 64),
			Created:   h.clk.Now().Unix(),
		}); err != nil {
			t.Fatalf("CreateUser: %v", err)
		}
	}
	if _, err := h.repo.GetDevice(ctx, deviceID); err == nil {
		return
	}
	if err := h.repo.CreateDevice(ctx, store.DeviceRow{
		ID:             deviceID,
		UserID:         userID,
		DSKPub:         dsk,
		Tier:           0,
		SignerTier:     0,
		CredentialBlob: []byte{0x01},
		LastSeen:       h.clk.Now().Unix(),
		Created:        h.clk.Now().Unix(),
	}); err != nil {
		t.Fatalf("CreateDevice: %v", err)
	}
}

// otherDeviceOfSameUser is a second device of the uploading user. R29 is a rule about the USER,
// so this session must be allowed to delete what g.device uploaded.
func (h *dsHarness) otherDeviceOfSameUser(t *testing.T, g *dsMessageGroup) auth.Session {
	t.Helper()
	device := id.New()
	h.account(t, g.session.UserID, device)
	return auth.Session{UserID: g.session.UserID, DeviceID: device, Scope: auth.ScopeEnrolled}
}

// sessionOfAnotherUser is a whole other account. It is the negative half of R29.
func (h *dsHarness) sessionOfAnotherUser(t *testing.T, _ *dsMessageGroup) auth.Session {
	t.Helper()
	userID, device := id.New(), id.New()
	h.account(t, userID, device)
	return auth.Session{UserID: userID, DeviceID: device, Scope: auth.ScopeEnrolled}
}

// removeLeafOfDevice marks the device's leaf removed, which is what a committed Remove leaves
// behind in SQL: `ListMembers` filters `removed_epoch IS NULL`, so `d.leafOf` stops finding it.
// The whole member set is rewritten because that is the only shape `ReplaceMembers` has, and it
// runs in one transaction so the 1,500-leaf fixture is one commit rather than 1,500.
func (h *dsHarness) removeLeafOfDevice(t *testing.T, g *dsMessageGroup, device id.ID) {
	t.Helper()
	ctx := context.Background()
	members, err := h.repo.ListMembers(ctx, g.id)
	if err != nil {
		t.Fatalf("ListMembers: %v", err)
	}
	found := false
	epoch := g.epoch
	for i := range members {
		if members[i].DeviceID == device {
			members[i].RemovedEpoch = &epoch
			found = true
		}
	}
	if !found {
		t.Fatalf("device %s is not a member of the group", device.String()[:8])
	}
	if err := h.repo.Tx(ctx, func(tx store.Repository) error {
		return tx.ReplaceMembers(ctx, g.id, g.epoch, members)
	}); err != nil {
		t.Fatalf("ReplaceMembers: %v", err)
	}
}

// cursorOf is the device's acknowledged seq in this group. A device that has acknowledged nothing
// reads 0 — the store answers an absent row with a zero cursor, not with ErrNotFound.
func (h *dsHarness) cursorOf(t *testing.T, device, groupID id.ID) uint64 {
	t.Helper()
	row, err := h.repo.GetCursor(context.Background(), device, groupID)
	if err != nil {
		t.Fatalf("GetCursor: %v", err)
	}
	return row.LastSeq
}

// ------------------------------------------------------- hand-framed PrivateMessages

// message is one well-formed application message at `epoch`, with the 32-byte commitment the
// delivery service requires.
func (h *dsHarness) message(t *testing.T, _ *dsMessageGroup, epoch uint64) []byte {
	t.Helper()
	return privateMessage(t, 32, epoch, 64)
}

// messageWithAAD varies only the length of authenticated_data, which is where the commitment C
// travels and the only place invariant 8 reads it from.
func (h *dsHarness) messageWithAAD(t *testing.T, g *dsMessageGroup, n int) []byte {
	t.Helper()
	return privateMessage(t, n, g.epoch, 64)
}

// messageOfContentType frames a message whose MLS content_type byte is `ct` — 1 application,
// 2 proposal, 3 commit. Everything else is well formed: a 32-byte commitment, the group's own
// epoch, an ordinary ciphertext. Only the one byte invariant 8's parse reads differs.
func (h *dsHarness) messageOfContentType(t *testing.T, g *dsMessageGroup, ct byte) []byte {
	t.Helper()
	return privateMessageOfContentType(t, 32, g.epoch, 64, ct)
}

// messageOfSize is a well-formed message whose SERIALISED length is exactly `total` bytes, which
// is the number the 131072-byte cap is measured against.
func (h *dsHarness) messageOfSize(t *testing.T, g *dsMessageGroup, total int) []byte {
	t.Helper()
	ctLen, err := ciphertextLenForTotal(total)
	if err != nil {
		t.Fatalf("messageOfSize(%d): %v", total, err)
	}
	pm := privateMessage(t, 32, g.epoch, ctLen)
	if len(pm) != total {
		t.Fatalf("framed %d bytes, want exactly %d", len(pm), total)
	}
	return pm
}

// privateMessage builds the RFC 9420 §6.3.2 MLSMessage/PrivateMessage framing by hand, exactly as
// core/dilla-core-wasi/src/private_message.rs's own `tests_support::message` does — the guest
// parses the header without decrypting anything, so a real ciphertext is neither needed nor
// possible here (the delivery service holds no group secrets, by design).
func privateMessage(t *testing.T, aadLen int, epoch uint64, ctLen int) []byte {
	t.Helper()
	return privateMessageOfContentType(t, aadLen, epoch, ctLen, contentTypeApplication)
}

// contentTypeApplication is the one content_type endpoint 7 accepts. 2 is proposal and 3 is
// commit; both are handshakes, which reach the instance through endpoints 5 and 6 and nowhere
// else.
const contentTypeApplication = 1

// privateMessageOfContentType is privateMessage with the content_type byte chosen by the caller,
// so a test can frame the handshake an upload must refuse.
func privateMessageOfContentType(t *testing.T, aadLen int, epoch uint64, ctLen int, contentType byte) []byte {
	t.Helper()
	var out []byte
	out = binary.BigEndian.AppendUint16(out, 1) // protocol_version: MLS 1.0
	out = binary.BigEndian.AppendUint16(out, 2) // wire_format: PrivateMessage
	out = appendVLBytes(t, out, []byte("group-id"))
	out = binary.BigEndian.AppendUint64(out, epoch)
	out = append(out, contentType)
	out = appendVLBytes(t, out, bytes.Repeat([]byte{5}, aadLen))
	out = appendVLBytes(t, out, bytes.Repeat([]byte{7}, 16))
	out = appendVLBytes(t, out, bytes.Repeat([]byte{9}, ctLen))
	return out
}

// privateMessageFixedBytes is everything privateMessage frames except the ciphertext field: the
// two 16-bit headers, the 8-byte "group-id" with its one-byte length, the epoch, the content type,
// the 32-byte authenticated_data with its length and the 16-byte sender data with its length.
const privateMessageFixedBytes = 2 + 2 + (1 + 8) + 8 + 1 + (1 + 32) + (1 + 16)

// ciphertextLenForTotal solves for the ciphertext length that makes the framed message exactly
// `total` bytes. The variable-length header's own width depends on the value it carries and MLS
// forbids a non-minimal encoding, so the three widths are tried in order and the first whose
// length falls in its own range wins. A few totals sit in the gap between two widths and are
// unreachable; the tests use none of them, and an error says so rather than framing a message the
// guest would refuse for a reason the test does not mean.
func ciphertextLenForTotal(total int) (int, error) {
	for _, header := range []int{1, 2, 4} {
		n := total - privateMessageFixedBytes - header
		if n < 0 {
			continue
		}
		if vlHeaderLen(n) == header {
			return n, nil
		}
	}
	return 0, fmt.Errorf("no ciphertext length frames a message of exactly %d bytes", total)
}

// vlHeaderLen is the width of RFC 9000's variable-length integer for n, under MLS's minimum-size
// rule: the shortest form that can carry the value.
func vlHeaderLen(n int) int {
	switch {
	case n < 64:
		return 1
	case n < 16384:
		return 2
	default:
		return 4
	}
}

// appendVLBytes writes the RFC 9420 §2.1.3 variable-length header — RFC 9000's varint, whose two
// top bits give the width — and then the payload.
func appendVLBytes(t *testing.T, dst, v []byte) []byte {
	t.Helper()
	switch vlHeaderLen(len(v)) {
	case 1:
		dst = append(dst, byte(len(v)))
	case 2:
		dst = binary.BigEndian.AppendUint16(dst, uint16(len(v))|0x4000)
	default:
		if len(v) >= 1<<30 {
			t.Fatalf("appendVLBytes: %d bytes is past the four-byte form", len(v))
		}
		dst = binary.BigEndian.AppendUint32(dst, uint32(len(v))|0x80000000)
	}
	return append(dst, v...)
}
