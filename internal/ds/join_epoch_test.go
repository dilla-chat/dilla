package ds_test

import (
	"context"
	"encoding/binary"
	"errors"
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
