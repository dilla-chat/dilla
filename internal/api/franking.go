package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"

	"github.com/fxamacker/cbor/v2"

	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/server"
)

// There is exactly ONE commitment and ONE tag in dilla, both defined by
// protocol/04 "Franking":
//
//	C = HMAC-SHA256(k_f, "dilla frank v1" || CBOR(envelope with k_f blanked))
//	T = HMAC-SHA256(K_frank, "dilla frank tag v1" || group_id || epoch(8, BE)
//	                || seq(8) || uploader_device(16) || C || recv_ts(8))
//
// The readable path uses both unchanged. It does NOT substitute SHA-256(envelope)
// for C: a readable envelope carries k_f at element 8 in the clear, so the server
// can compute the real commitment, and a report against a readable message would
// otherwise never verify. Two substitutions in T are unavoidable (protocol/09
// § Readable channels): a readable channel has no MLS group, so channel_id stands
// in for group_id, and it has no epoch, so the epoch field is 0.
//
// The plan places Commitment and Tag in task 17's reports.go; task 8 is the first
// caller, so they land here and task 17 uses them. Plan 1b's ds.FrankingTag is the
// same function over a [32]byte key and stays where it is (follow-up card 7);
// TestTagIsTheDeliveryServicesTag pins the two to one answer.

// FrankingKeys is the instance franking key and its id (protocol/04 "Franking",
// protocol/03 § Instance keys).
type FrankingKeys interface {
	Current() (keyID id.ID, key []byte)
	ByID(keyID id.ID) ([]byte, bool)
	// All returns the current key and every retained one, current first. Task
	// 17's verifier needs it for a message franked before a key id was recorded.
	All() []FrankingKey
}

// FrankingKey pairs a franking key with its id.
type FrankingKey struct {
	ID  id.ID
	Key []byte
}

// StaticFrankingKeys is a FrankingKeys over a fixed list: the current key first,
// then the retained ones. The composition root builds it from the instance's key
// history; a rotation builds a new one.
type StaticFrankingKeys struct{ keys []FrankingKey }

// NewStaticFrankingKeys returns the key set with current as the key new tags are
// made under.
func NewStaticFrankingKeys(current FrankingKey, retained ...FrankingKey) *StaticFrankingKeys {
	return &StaticFrankingKeys{keys: append([]FrankingKey{current}, retained...)}
}

func (k *StaticFrankingKeys) Current() (id.ID, []byte) { return k.keys[0].ID, k.keys[0].Key }

func (k *StaticFrankingKeys) ByID(keyID id.ID) ([]byte, bool) {
	for _, key := range k.keys {
		if key.ID == keyID {
			return key.Key, true
		}
	}
	return nil, false
}

func (k *StaticFrankingKeys) All() []FrankingKey { return append([]FrankingKey(nil), k.keys...) }

// envelopeElements is protocol/04's fixed envelope length.
const envelopeElements = 9

// Commitment is protocol/04's C. The envelope is re-encoded with k_f replaced by
// an EMPTY bstr, which is what makes the commitment bind the content without
// binding the key that opens it. The envelope is deterministic CBOR (cborx
// refuses anything else), so the re-encoded prefix is byte-identical to the one
// the sender committed to.
func Commitment(envelope []byte, kf []byte) ([]byte, error) {
	var raw []cbor.RawMessage
	if err := cborx.Unmarshal(envelope, &raw); err != nil {
		return nil, server.Errorf(server.CodeEnvelopeShape, "envelope is not deterministic CBOR")
	}
	if len(raw) != envelopeElements {
		return nil, server.Errorf(server.CodeEnvelopeShape, "envelope has %d elements, want %d", len(raw), envelopeElements)
	}
	blank, err := cborx.Marshal([]byte{})
	if err != nil {
		return nil, err
	}
	raw[8] = blank
	body, err := cborx.Marshal(raw)
	if err != nil {
		return nil, err
	}
	mac := hmac.New(sha256.New, kf)
	mac.Write([]byte("dilla frank v1"))
	mac.Write(body)
	return mac.Sum(nil), nil
}

// Tag is protocol/04's T. recvTS is unix seconds and never negative.
func Tag(kFrank []byte, groupID id.ID, epoch, seq uint64, uploader id.ID, c []byte, recvTS int64) []byte {
	mac := hmac.New(sha256.New, kFrank)
	mac.Write([]byte("dilla frank tag v1"))
	mac.Write(groupID[:])
	var scratch [8]byte
	binary.BigEndian.PutUint64(scratch[:], epoch)
	mac.Write(scratch[:])
	binary.BigEndian.PutUint64(scratch[:], seq)
	mac.Write(scratch[:])
	mac.Write(uploader[:])
	mac.Write(c)
	binary.BigEndian.PutUint64(scratch[:], uint64(recvTS)) //nolint:gosec // G115: a unix second from the instance clock
	mac.Write(scratch[:])
	return mac.Sum(nil)
}
