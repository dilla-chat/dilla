package gateway

import (
	"github.com/fxamacker/cbor/v2"

	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
)

// payload encodes a fixed-position payload array once. Every producer in the delivery service
// calls one of the helpers below, so no call site hand-rolls an array length.
func payload(items ...any) (cbor.RawMessage, error) {
	b, err := cborx.Marshal(items)
	if err != nil {
		return nil, err
	}
	return cbor.RawMessage(b), nil
}

// HelloPayload is the first frame the instance sends on every connection. Its last two elements are
// invariant 7's back-off window in milliseconds (deviation B23).
func HelloPayload(wire, e2ee, media []uint64, heartbeatMS, maxFrameBytes uint64, instanceID id.ID, generation, backoffMS, backoffJitterMS uint64) (cbor.RawMessage, error) {
	return payload(wire, e2ee, media, heartbeatMS, maxFrameBytes, instanceID, generation,
		backoffMS, backoffJitterMS)
}

// GroupDigest is one entry of ready's per-group catch-up list.
type GroupDigest struct {
	GroupID              id.ID
	Epoch                uint64
	LastSeq              uint64
	ProposalsOutstanding uint64
}

func ReadyPayload(deviceID, userID id.ID, generation uint64, resumeToken []byte, wire, e2ee, media uint64, keypackagesRemaining uint64, groups []GroupDigest, bootID id.ID) (cbor.RawMessage, error) {
	rows := make([]any, 0, len(groups))
	for _, g := range groups {
		rows = append(rows, []any{g.GroupID, g.Epoch, g.LastSeq, g.ProposalsOutstanding})
	}
	return payload(deviceID, userID, generation, resumeToken, wire, e2ee, media, keypackagesRemaining, rows, bootID)
}

// ResumedPayload answers a resume. resumeToken is the ROTATED token, not the one the client just
// spent: the instance discards a token the moment it is used, so this frame is the only place a
// client learns the credential its next resume must carry.
func ResumedPayload(from, to uint64, resumeToken []byte) (cbor.RawMessage, error) {
	return payload(from, to, resumeToken)
}

func InvalidSessionPayload(resumable bool, reason string) (cbor.RawMessage, error) {
	return payload(boolUint(resumable), reason)
}

func HeartbeatAckPayload(serverTS uint64) (cbor.RawMessage, error) { return payload(serverTS) }

func ReconnectPayload(reason string, afterMS uint64) (cbor.RawMessage, error) {
	return payload(reason, afterMS)
}

func ErrorPayload(cid uint64, code, detail string) (cbor.RawMessage, error) {
	return payload(cid, code, detail)
}

// HandshakePayload is op 16. sender is the leaf index, or nil for the instance's external sender
// (protocol/02, "Handshake records").
func HandshakePayload(seq, epoch uint64, kind uint8, sender *uint32, blob []byte) (cbor.RawMessage, error) {
	return payload(seq, epoch, uint64(kind), optUint(sender), blob)
}

// CommitNeededPayload is op 17. deadlineMS is relative to the moment the frame reaches the
// connection's writer; the writer re-bases it (R31, interfaces §0.1 D11).
func CommitNeededPayload(epoch uint64, refs [][]byte, deadlineMS, round uint64) (cbor.RawMessage, error) {
	return payload(epoch, refs, deadlineMS, round)
}

func EpochChangedPayload(epoch, seq uint64) (cbor.RawMessage, error) { return payload(epoch, seq) }

// MessageCTPayload is op 19: [seq, epoch, uploader_device, blob, franking_tag, recv_ts, class]. class
// is the upload class (web3.7 SB1, protocol/02 § Upload class), null for a legacy upload.
func MessageCTPayload(seq, epoch uint64, uploader id.ID, blob, frankingTag []byte, recvTS uint64, class *uint8) (cbor.RawMessage, error) {
	return payload(seq, epoch, uploader, blob, frankingTag, recvTS, optUint8(class))
}

func WelcomePayload(welcomeID, epoch, commitSeq uint64, blob, ratchetTree, treeHash []byte) (cbor.RawMessage, error) {
	return payload(welcomeID, epoch, commitSeq, blob, ratchetTree, treeHash)
}

func MessageDeletedPayload(seq, deletedAt uint64) (cbor.RawMessage, error) {
	return payload(seq, deletedAt)
}

// MessagePlainPayload is op 32's body (protocol/02): [channel_id(bstr16), seq(uint),
// sender(bstr16), envelope(bstr), franking_tag(bstr32), edited(uint), deleted(uint)]. A deleted
// message carries an empty envelope, never a null: the element is a bstr.
func MessagePlainPayload(channelID id.ID, seq uint64, sender id.ID, envelope, frankingTag []byte, edited, deleted uint64) (cbor.RawMessage, error) {
	if envelope == nil {
		envelope = []byte{}
	}
	return payload(channelID, seq, sender, envelope, frankingTag, edited, deleted)
}

func PresencePayload(userID id.ID, status uint64, sinceTS uint64, statusMsg string) (cbor.RawMessage, error) {
	return payload(userID, status, sinceTS, statusMsg)
}

func TypingPayload(channelID, userID, deviceID id.ID, typing bool, expiresTS uint64) (cbor.RawMessage, error) {
	return payload(channelID, userID, deviceID, boolUint(typing), expiresTS)
}

func VoiceStatePayload(userID, deviceID, callID id.ID, flags uint64) (cbor.RawMessage, error) {
	return payload(userID, deviceID, callID, flags)
}

// CallRingPayload is op 53 call_ring's `[call_id, channel_id, user_id, state, expires_ts]` (web-3,
// L-GW-33): state 1 ringing and 0 ring over name the caller, state 2 names the participant who
// declined; expires_ts is the ring's end in unix seconds, the same in all three.
func CallRingPayload(callID, channelID, userID id.ID, state, expires uint64) (cbor.RawMessage, error) {
	return payload(callID, channelID, userID, state, expires)
}

// BotCatalogPayload is op 61 bot_catalog's `[community_id, revision, filtered_catalog]` (Bot v2
// task 9). commands is the CBOR array of the commands this one member may use, already encoded.
func BotCatalogPayload(communityID id.ID, revision uint64, commands cbor.RawMessage) (cbor.RawMessage, error) {
	return payload(communityID, revision, commands)
}

// CommunityChangedPayload is op 54 community_changed's `[community_id]` (dilla-web-3.1): the
// community whose channels or settings changed. It names nothing else; a member re-reads.
func CommunityChangedPayload(communityID id.ID) (cbor.RawMessage, error) {
	return payload(communityID)
}

// AFKAwayPayload is op 55 afk_away's `[community_id, user_id, since_ts]` (AW3): the member is away
// in the community's AFK channel since sinceTS (unix seconds), or no longer away when it is 0.
func AFKAwayPayload(communityID, userID id.ID, sinceTS uint64) (cbor.RawMessage, error) {
	return payload(communityID, userID, sinceTS)
}

// The deterministic-CBOR subset has no boolean; flags travel as uint 0 or 1.
func boolUint(b bool) uint64 {
	if b {
		return 1
	}
	return 0
}

func optUint8(v *uint8) any {
	if v == nil {
		return nil
	}
	return uint64(*v)
}

func optUint(v *uint32) any {
	if v == nil {
		return nil
	}
	return uint64(*v)
}
