package ds

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"

	"github.com/dilla-chat/dilla/internal/id"
)

// frankingLabel is the domain separator of protocol/04-envelope-and-franking.md's tag. It is 18
// bytes of UTF-8 and is never changed without a media_version bump.
const frankingLabel = "dilla frank tag v1"

// FrankingTag computes T = HMAC-SHA256(K_frank, "dilla frank tag v1" || group_id ||
// epoch(8 BE) || seq(8 BE) || uploader_device(16) || C || recv_ts(8 BE)).
//
// Every field is in the preimage on purpose: the tag binds a commitment to its position in one
// group's stream, so a reporter cannot move a genuine message to another place and have the
// instance confirm it.
func FrankingTag(key [32]byte, groupID id.ID, epoch, seq uint64, uploader id.ID, commitment []byte, recvTS uint64) []byte {
	mac := hmac.New(sha256.New, key[:])
	mac.Write([]byte(frankingLabel))
	mac.Write(groupID[:])
	var scratch [8]byte
	binary.BigEndian.PutUint64(scratch[:], epoch)
	mac.Write(scratch[:])
	binary.BigEndian.PutUint64(scratch[:], seq)
	mac.Write(scratch[:])
	mac.Write(uploader[:])
	mac.Write(commitment)
	binary.BigEndian.PutUint64(scratch[:], recvTS)
	mac.Write(scratch[:])
	return mac.Sum(nil)
}
