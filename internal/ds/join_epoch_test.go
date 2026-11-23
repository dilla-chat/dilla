package ds_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"net/http"
	"strconv"
	"testing"

	"github.com/dilla-chat/dilla/internal/ds"
)

// withEpoch is a copy of a PublicMessage commit whose framed content names `epoch` (RFC 9420
// §6.2: version, wire_format, then FramedContent's group_id<V> and the uint64 epoch). The signature
// no longer verifies, which is beside the point: the epoch is answered before the commit is read.
func withEpoch(t *testing.T, msg []byte, epoch uint64) []byte {
	t.Helper()
	out := append([]byte(nil), msg...)
	if len(out) < 5 || out[4]>>6 != 0 {
		t.Fatal("the fixture's group_id length is not a one-byte varint")
	}
	at := 5 + int(out[4])
	binary.BigEndian.PutUint64(out[at:at+8], epoch)
	return out
}

// An external commit built on an epoch the group has since left (another joiner's or a member's
// commit landed between the joiner's GroupInfo read and its POST /resync) is a race, not a bad
// commit: it is refused under rule "epoch", the client contract for "the group changed while this
// commit was being prepared", so the joiner stages again on the group as it is now. /resync takes no
// epoch from the client, so commitPreflight's 409 E_COMMIT_CONFLICT can never answer it, and
// without this check the parse refuses it as "structural" (openmls's WrongEpoch), which the client
// rightly never retries: the open of a channel failed with E_COMMIT_INVALID whenever two devices
// joined its group at once. A commit for an epoch the group has not reached is "epoch_ahead".
func TestAnExternalCommitForAnEpochTheGroupLeftIsRefusedUnderRuleEpoch(t *testing.T) {
	h := newDSHarness(t)
	reg, _ := h.mustRegister(t) // the fixture group is at epoch 6
	joiner := h.sessionOf(t, h.device(t))
	h.acl.allow(joiner.UserID)
	commit := fixtureFile(t, "commits/09.mls")

	for _, c := range []struct {
		epoch uint64
		rule  string
	}{{5, "epoch"}, {0, "epoch"}, {7, "epoch_ahead"}} {
		_, err := h.ds.Resync(context.Background(), joiner, reg.GroupID, ds.ResyncRequest{
			ExternalCommit: withEpoch(t, commit, c.epoch), GroupInfo: dsFixture(t).groupInfo,
		})
		var dsErr *ds.Error
		if !errors.As(err, &dsErr) || dsErr.Code != "E_COMMIT_INVALID" || dsErr.Rule != c.rule {
			t.Fatalf("an external commit for epoch %d at epoch 6: got %v (rule %q), want E_COMMIT_INVALID/%s",
				c.epoch, err, ruleOf(err), c.rule)
		}
	}
	// Control: at the group's own epoch the commit reaches the parse, which judges it (a member
	// commit on the external path is structural).
	_, err := h.ds.Resync(context.Background(), joiner, reg.GroupID, ds.ResyncRequest{
		ExternalCommit: commit, GroupInfo: dsFixture(t).groupInfo,
	})
	if ruleOf(err) != "structural" {
		t.Fatalf("control at the group's epoch: got %v, want the parse's structural refusal", err)
	}
}

func ruleOf(err error) string {
	var dsErr *ds.Error
	if errors.As(err, &dsErr) {
		return dsErr.Rule
	}
	return ""
}

// A closed group is answered 409 E_GROUP_CLOSED before any epoch (protocol/02: "after the
// membership answer ... and before the epoch check"), the external commit's framed epoch included:
// a call joiner whose stale commit arrives after the call ended must read the call again and
// register a fresh group, never be sent round a stale-epoch retry of a group that is final.
func TestAStaleExternalCommitToAClosedGroupIsAnsweredClosed(t *testing.T) {
	h := newDSHarness(t)
	ctx := context.Background()
	reg, _ := h.mustRegister(t) // epoch 6
	joiner := h.sessionOf(t, h.device(t))
	h.acl.allow(joiner.UserID)
	if err := h.ds.Close(ctx, reg.GroupID); err != nil {
		t.Fatalf("Close: %v", err)
	}
	for _, epoch := range []uint64{5, 7} {
		_, err := h.ds.Resync(ctx, joiner, reg.GroupID, ds.ResyncRequest{
			ExternalCommit: withEpoch(t, fixtureFile(t, "commits/09.mls"), epoch), GroupInfo: dsFixture(t).groupInfo,
		})
		var dsErr *ds.Error
		if !errors.As(err, &dsErr) || dsErr.Code != "E_GROUP_CLOSED" || dsErr.Status != http.StatusConflict {
			t.Fatalf("a commit framed for epoch %d to a closed group: got %v (rule %q), want 409 E_GROUP_CLOSED",
				epoch, err, ruleOf(err))
		}
	}
}

// A device the channel ACL does not admit learns neither the group's existence nor its epoch: its
// stale external commit is E_NOT_FOUND, as every read answers it, never a 422 naming the epoch.
func TestAStaleExternalCommitByAnIneligibleDeviceIsNotFound(t *testing.T) {
	h := newDSHarness(t)
	reg, _ := h.mustRegister(t)
	stranger := h.sessionOf(t, h.device(t))
	for _, epoch := range []uint64{5, 7} {
		_, err := h.ds.Resync(context.Background(), stranger, reg.GroupID, ds.ResyncRequest{
			ExternalCommit: withEpoch(t, fixtureFile(t, "commits/09.mls"), epoch), GroupInfo: dsFixture(t).groupInfo,
		})
		var dsErr *ds.Error
		if !errors.As(err, &dsErr) || dsErr.Code != "E_NOT_FOUND" {
			t.Fatalf("a stranger's commit framed for epoch %d: got %v, want E_NOT_FOUND", epoch, err)
		}
	}
}

// publicFrame is an MLSMessage header: version, wire_format, group_id<V> behind the given length
// prefix, then the uint64 epoch.
func publicFrame(version, wireFormat uint16, prefix, groupID []byte, epoch uint64) []byte {
	out := binary.BigEndian.AppendUint16(nil, version)
	out = binary.BigEndian.AppendUint16(out, wireFormat)
	out = append(out, prefix...)
	out = append(out, groupID...)
	return binary.BigEndian.AppendUint64(out, epoch)
}

type frameCase struct {
	name string
	msg  []byte
}

// Every width of RFC 9420's variable-length integer (§2.1.2) reads the same epoch, and anything
// that is not a whole mls10 PublicMessage header is not read (the parse then judges it).
func TestPublicMessageEpochReadsEveryLengthWidthAndNothingElse(t *testing.T) {
	const epoch = uint64(0x0102030405060708)
	gid16 := bytes.Repeat([]byte{0x66}, 16)
	gid100 := bytes.Repeat([]byte{0x77}, 100)
	gid70000 := bytes.Repeat([]byte{0x88}, 70000)
	for _, c := range []frameCase{
		{"1-byte length", publicFrame(1, 1, []byte{16}, gid16, epoch)},
		{"1-byte length, empty group id", publicFrame(1, 1, []byte{0}, nil, epoch)},
		{"2-byte length", publicFrame(1, 1, []byte{0x40, 100}, gid100, epoch)},
		{"2-byte length, not minimal", publicFrame(1, 1, []byte{0x40, 16}, gid16, epoch)},
		{"4-byte length", publicFrame(1, 1, []byte{0x80, 0x01, 0x11, 0x70}, gid70000, epoch)},
		{"content after the epoch", append(publicFrame(1, 1, []byte{16}, gid16, epoch), 1, 2, 3)},
	} {
		if got, read := ds.PublicMessageEpochForTest(c.msg); !read || got != epoch {
			t.Errorf("%s: got (%#x, %v), want (%#x, true)", c.name, got, read, epoch)
		}
	}
	whole := publicFrame(1, 1, []byte{16}, gid16, epoch)
	refused := []frameCase{
		{"nil", nil},
		{"version 2", publicFrame(2, 1, []byte{16}, gid16, epoch)},
		{"version 0", publicFrame(0, 1, []byte{16}, gid16, epoch)},
		{"PrivateMessage", publicFrame(1, 2, []byte{16}, gid16, epoch)},
		{"Welcome", publicFrame(1, 3, []byte{16}, gid16, epoch)},
		{"GroupInfo", publicFrame(1, 4, []byte{16}, gid16, epoch)},
		{"the reserved 8-byte length prefix", publicFrame(1, 1, []byte{0xc0, 0, 0, 0, 0, 0, 0, 16}, gid16, epoch)},
		{"a 2-byte prefix's second byte missing", []byte{0, 1, 0, 1, 0x40}},
		{"a 4-byte prefix's last bytes missing", []byte{0, 1, 0, 1, 0x80, 0, 0}},
		{"a 2-byte length past the end", publicFrame(1, 1, []byte{0x40, 100}, gid16, epoch)},
		{"a 4-byte length past the end", publicFrame(1, 1, []byte{0x80, 0x00, 0x11, 0x70}, gid100, epoch)},
	}
	// Cut short at every byte: the header, the length, the group id and the epoch.
	for i := range len(whole) {
		refused = append(refused, frameCase{"cut at " + strconv.Itoa(i), whole[:i]})
	}
	for _, c := range refused {
		if got, read := ds.PublicMessageEpochForTest(c.msg); read {
			t.Errorf("%s: read epoch %#x, want no read", c.name, got)
		}
	}
}

// FuzzPublicMessageEpoch: no input panics, and an epoch is read only from a whole mls10
// PublicMessage header, where it is the uint64 right after the group id.
func FuzzPublicMessageEpoch(f *testing.F) {
	f.Add(publicFrame(1, 1, []byte{16}, bytes.Repeat([]byte{1}, 16), 6))
	f.Add(publicFrame(1, 1, []byte{0x40, 3}, []byte{1, 2, 3}, 9))
	f.Add(publicFrame(1, 1, []byte{0x80, 0, 0, 1}, []byte{1}, 1))
	f.Add([]byte{0, 1, 0, 1, 0xc0})
	f.Fuzz(func(t *testing.T, msg []byte) {
		got, read := ds.PublicMessageEpochForTest(msg)
		if !read {
			return
		}
		if len(msg) < 5 || binary.BigEndian.Uint16(msg) != 1 || binary.BigEndian.Uint16(msg[2:]) != 1 {
			t.Fatalf("read an epoch from a header that is not an mls10 PublicMessage: % x", msg)
		}
		width := 1 << (msg[4] >> 6)
		if width == 8 {
			t.Fatalf("read an epoch behind the reserved 8-byte length prefix: % x", msg)
		}
		n := uint64(msg[4] & 0x3f)
		for _, b := range msg[5 : 4+width] {
			n = n<<8 | uint64(b)
		}
		at := 4 + uint64(width) + n
		if want := binary.BigEndian.Uint64(msg[at : at+8]); got != want {
			t.Fatalf("read epoch %d, want the uint64 after the group id, %d", got, want)
		}
	})
}
