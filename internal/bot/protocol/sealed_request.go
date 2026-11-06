package protocol

import "github.com/dilla-chat/dilla/internal/id"

// MaxSealedRequestArgs mirrors the core's MAX_COMMAND_ARGS.
const MaxSealedRequestArgs = 32

// SealedRequest is a user's command as the Bot helper opened and verified it (helper op 22,
// Bot v2 task 8): signed by UserID's device DeviceID, sealed to the Bot's command key.
type SealedRequest struct {
	RequestID   id.ID
	CommunityID id.ID
	// ChannelID is the zero ID when the command names no channel.
	ChannelID id.ID
	Operation uint64
	// Args holds uint64, []byte, string or nil values.
	Args   []any
	SentAt uint64
	// ReplyKey is the request's one-time X25519 public key; answers are sealed to it.
	ReplyKey [32]byte
	// UserID and DeviceID are the identity dillad attached, which the signed command matched.
	UserID   id.ID
	DeviceID id.ID
}

// DecodeSealedRequest decodes the helper's deterministic CBOR `[request_id, community_id,
// channel_id|null, operation, args, sent_at, reply_key]`. UserID and DeviceID are left for the
// caller, which attached them.
func DecodeSealedRequest(b []byte) (SealedRequest, error) {
	var r SealedRequest
	n, err := parse(b)
	if err != nil || n.major != 4 || len(n.a) != 7 {
		return r, invalid()
	}
	a := n.a
	fixed := func(x node, size int) bool { return x.major == 2 && len(x.b) == size && !allZero(x.b) }
	if !fixed(a[0], 16) || !fixed(a[1], 16) || a[3].major != 0 || a[4].major != 4 || a[5].major != 0 || !fixed(a[6], 32) {
		return r, invalid()
	}
	r.RequestID, r.CommunityID = nodeID(a[0]), nodeID(a[1])
	switch {
	case a[2].null:
	case fixed(a[2], 16):
		r.ChannelID = nodeID(a[2])
	default:
		return r, invalid()
	}
	if len(a[4].a) > MaxSealedRequestArgs {
		return r, invalid()
	}
	r.Args = make([]any, 0, len(a[4].a))
	for _, arg := range a[4].a {
		switch {
		case arg.null:
			r.Args = append(r.Args, nil)
		case arg.major == 0:
			r.Args = append(r.Args, arg.u)
		case arg.major == 2:
			r.Args = append(r.Args, append([]byte(nil), arg.b...))
		case arg.major == 3:
			r.Args = append(r.Args, arg.s)
		default:
			return SealedRequest{}, invalid()
		}
	}
	r.Operation, r.SentAt = a[3].u, a[5].u
	copy(r.ReplyKey[:], a[6].b)
	return r, nil
}

func allZero(b []byte) bool {
	var v byte
	for _, x := range b {
		v |= x
	}
	return v == 0
}
