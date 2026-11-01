// Package gatewayclient implements the bounded, typed bot-side gateway transport.
package gatewayclient

import (
	"errors"
	"fmt"

	"github.com/fxamacker/cbor/v2"

	"github.com/jonasthim/dilla/internal/cborx"
	"github.com/jonasthim/dilla/internal/id"
)

const MaxGatewayFrameBytes = (1 << 20) + 512

type Opcode uint64

const (
	OpHello            Opcode = 0
	OpIdentify         Opcode = 1
	OpResume           Opcode = 2
	OpReady            Opcode = 3
	OpResumed          Opcode = 4
	OpInvalidSession   Opcode = 5
	OpHeartbeat        Opcode = 6
	OpHeartbeatAck     Opcode = 7
	OpReconnect        Opcode = 8
	OpError            Opcode = 9
	OpSubscribe        Opcode = 10
	OpUnsubscribe      Opcode = 11
	OpCommitAck        Opcode = 12
	OpHandshake        Opcode = 16
	OpCommitNeeded     Opcode = 17
	OpEpochChanged     Opcode = 18
	OpMessageCT        Opcode = 19
	OpWelcome          Opcode = 20
	OpMessageDeleted   Opcode = 21
	OpMessagePlain     Opcode = 32
	OpInteraction      Opcode = 33
	OpPresence         Opcode = 48
	OpTyping           Opcode = 49
	OpVoiceState       Opcode = 50
	OpCallRing         Opcode = 53
	OpCommunityChanged Opcode = 54
)

var (
	ErrProtocol         = errors.New("bot gateway: invalid protocol frame")
	ErrVersion          = errors.New("bot gateway: unsupported protocol version")
	ErrIdentity         = errors.New("bot gateway: gateway identity mismatch")
	ErrBackpressure     = errors.New("bot gateway: event queue is full; authoritative sync required")
	ErrAckOrder         = errors.New("bot gateway: replay acknowledgement is not the next durable frame")
	ErrStaleDelivery    = errors.New("bot gateway: delivery belongs to an obsolete stream")
	ErrTerminal         = errors.New("bot gateway: session is terminal or requires reauthentication")
	errHeartbeatOverdue = errors.New("bot gateway: heartbeat overdue; re-identification required")
	errReplayGap        = errors.New("bot gateway: replay gap; authoritative sync required")
	errTransport        = errors.New("bot gateway: transport disconnected")
)

type Hello struct {
	WireVersions, E2EEVersions, MediaVersions []uint64
	HeartbeatMS, MaxFrameBytes, Generation    uint64
	InstanceID                                id.ID
	BackoffMS, BackoffJitterMS                uint64
}

type Ready struct {
	DeviceID, UserID                       id.ID
	Generation                             uint64
	ResumeToken                            []byte
	WireVersion, E2EEVersion, MediaVersion uint64
	KeyPackagesRemaining                   uint64
	Groups                                 []ReadyGroup
}
type ReadyGroup struct {
	GroupID                              id.ID
	Epoch, LastSeq, ProposalsOutstanding uint64
}
type Resumed struct {
	ReplayedFrom, ReplayedTo uint64
	ResumeToken              []byte
}
type InvalidSession struct {
	Resumable bool
}
type Handshake struct {
	Seq, Epoch, Kind uint64
	Sender           *uint64
	Blob             []byte
}
type CommitNeeded struct {
	Epoch             uint64
	ProposalRefs      [][]byte
	DeadlineMS, Round uint64
}
type EpochChanged struct{ Epoch, Seq uint64 }
type MessageCT struct {
	Seq, Epoch        uint64
	UploaderDevice    id.ID
	Blob, FrankingTag []byte
	ReceivedAt, Class uint64
	ClassNull         bool
}
type Welcome struct {
	WelcomeID, Epoch, CommitSeq uint64
	Blob, RatchetTree, TreeHash []byte
}
type MessageDeleted struct{ Seq, DeletedAt uint64 }
type MessagePlain struct {
	ChannelID             id.ID
	Seq                   uint64
	Sender                id.ID
	Envelope, FrankingTag []byte
	Edited, Deleted       bool
}
type Interaction struct {
	ID, BotUserID id.ID
	Kind          uint64
	Blob          []byte
}
type Presence struct {
	UserID          id.ID
	Status, SinceTS uint64
	StatusMessage   string
}
type Typing struct {
	ChannelID, UserID, DeviceID id.ID
	IsTyping                    bool
	ExpiresTS                   uint64
}
type VoiceState struct {
	UserID, DeviceID, CallID id.ID
	Flags                    uint64
}
type CallRing struct {
	CallID, ChannelID, UserID id.ID
	State                     uint64
	ExpiresTS                 uint64
}

// Frame contains one fully shape-checked server frame. Event fields hold opaque
// ciphertext and metadata only; callers must use authenticated MLS state for authorship.
type Frame struct {
	Op               Opcode
	N                uint64
	GroupID          *id.ID
	Replayed         bool // true for a frame delivered from the server's replay range.
	ResumeBarrier    bool // true only for the current resumed frame after its replay range.
	Hello            *Hello
	Ready            *Ready
	Resumed          *Resumed
	InvalidSession   *InvalidSession
	ReconnectAfterMS *uint64
	Handshake        *Handshake
	CommitNeeded     *CommitNeeded
	EpochChanged     *EpochChanged
	MessageCT        *MessageCT
	Welcome          *Welcome
	MessageDeleted   *MessageDeleted
	MessagePlain     *MessagePlain
	Interaction      *Interaction
	Presence         *Presence
	Typing           *Typing
	VoiceState       *VoiceState
	CallRing         *CallRing
	CommunityID      *id.ID
	ServerTimestamp  *uint64
	ErrorCode        string
}

// DecodeServerFrame applies deterministic CBOR, direction, shape, and scope checks
// before decoding any frame fields. maxBytes must itself be within the protocol ceiling.
func DecodeServerFrame(raw []byte, maxBytes uint64) (Frame, error) {
	var out Frame
	if maxBytes == 0 || maxBytes > MaxGatewayFrameBytes || uint64(len(raw)) > maxBytes {
		return out, ErrProtocol
	}
	var elems []cbor.RawMessage
	if err := cborx.Unmarshal(raw, &elems); err != nil || len(elems) != 4 {
		return out, ErrProtocol
	}
	op, err := uintAt(elems[0])
	if err != nil || op > 255 {
		return out, ErrProtocol
	}
	out.Op = Opcode(op)
	if out.N, err = uintAt(elems[1]); err != nil {
		return Frame{}, ErrProtocol
	}
	if !isNull(elems[2]) {
		gid, e := idAt(elems[2])
		if e != nil {
			return Frame{}, ErrProtocol
		}
		out.GroupID = &gid
	}
	var payload []cbor.RawMessage
	if cborx.ExpectMajor(elems[3], cborx.MajorArray) != nil || cborx.Unmarshal(elems[3], &payload) != nil {
		return Frame{}, ErrProtocol
	}
	count, grouped, replayable, ok := serverSpec(out.Op)
	if !ok || len(payload) != count {
		return Frame{}, ErrProtocol
	}
	if grouped != (out.GroupID != nil) {
		return Frame{}, ErrProtocol
	}
	if replayable && out.N == 0 || !replayable && out.N != 0 {
		return Frame{}, ErrProtocol
	}
	bad := func() (Frame, error) { return Frame{}, ErrProtocol }
	switch out.Op {
	case OpHello:
		if out.N != 0 || out.GroupID != nil {
			return bad()
		}
		h := &Hello{}
		if h.WireVersions, err = uintsAt(payload[0], 8); err != nil {
			return bad()
		}
		if h.E2EEVersions, err = uintsAt(payload[1], 8); err != nil {
			return bad()
		}
		if h.MediaVersions, err = uintsAt(payload[2], 8); err != nil {
			return bad()
		}
		if h.HeartbeatMS, err = boundedUint(payload[3], 1, 120000); err != nil {
			return bad()
		}
		if h.MaxFrameBytes, err = boundedUint(payload[4], 1, MaxGatewayFrameBytes); err != nil {
			return bad()
		}
		if h.InstanceID, err = idAt(payload[5]); err != nil {
			return bad()
		}
		if h.Generation, err = uintAt(payload[6]); err != nil {
			return bad()
		}
		if h.BackoffMS, err = boundedUint(payload[7], 0, 120000); err != nil {
			return bad()
		}
		if h.BackoffJitterMS, err = boundedUint(payload[8], 0, 120000); err != nil || h.BackoffMS+h.BackoffJitterMS > 240000 {
			return bad()
		}
		out.Hello = h
	case OpReady:
		r := &Ready{}
		if r.DeviceID, err = idAt(payload[0]); err != nil {
			return bad()
		}
		if r.UserID, err = idAt(payload[1]); err != nil {
			return bad()
		}
		if r.Generation, err = uintAt(payload[2]); err != nil {
			return bad()
		}
		if r.ResumeToken, err = bytesAt(payload[3], 32, 32); err != nil {
			return bad()
		}
		if r.WireVersion, err = uintAt(payload[4]); err != nil {
			return bad()
		}
		if r.E2EEVersion, err = uintAt(payload[5]); err != nil {
			return bad()
		}
		if r.MediaVersion, err = uintAt(payload[6]); err != nil {
			return bad()
		}
		if r.KeyPackagesRemaining, err = uintAt(payload[7]); err != nil {
			return bad()
		}
		var rows []cbor.RawMessage
		if cborx.ExpectMajor(payload[8], cborx.MajorArray) != nil || cborx.Unmarshal(payload[8], &rows) != nil || len(rows) > 10000 {
			return bad()
		}
		r.Groups = make([]ReadyGroup, 0, len(rows))
		seenGroups := make(map[id.ID]struct{}, len(rows))
		for _, row := range rows {
			var f []cbor.RawMessage
			if cborx.Unmarshal(row, &f) != nil || len(f) != 4 {
				return bad()
			}
			g := ReadyGroup{}
			if g.GroupID, err = idAt(f[0]); err != nil {
				return bad()
			}
			if _, exists := seenGroups[g.GroupID]; exists {
				return bad()
			}
			seenGroups[g.GroupID] = struct{}{}
			if g.Epoch, err = uintAt(f[1]); err != nil {
				return bad()
			}
			if g.LastSeq, err = uintAt(f[2]); err != nil {
				return bad()
			}
			if g.ProposalsOutstanding, err = boundedUint(f[3], 0, 1<<20); err != nil {
				return bad()
			}
			r.Groups = append(r.Groups, g)
		}
		out.Ready = r
	case OpResumed:
		r := &Resumed{}
		if r.ReplayedFrom, err = uintAt(payload[0]); err != nil {
			return bad()
		}
		if r.ReplayedTo, err = uintAt(payload[1]); err != nil || r.ReplayedTo == ^uint64(0) || (r.ReplayedTo+1 != r.ReplayedFrom && r.ReplayedTo < r.ReplayedFrom) {
			return bad()
		}
		if r.ResumeToken, err = bytesAt(payload[2], 32, 32); err != nil {
			return bad()
		}
		out.Resumed = r
	case OpInvalidSession:
		n, e := uintAt(payload[0])
		if e != nil || n > 1 {
			return bad()
		}
		if _, e := textAt(payload[1], 256); e != nil {
			return bad()
		}
		out.InvalidSession = &InvalidSession{Resumable: n == 1}
	case OpHeartbeatAck:
		n, e := uintAt(payload[0])
		if e != nil {
			return bad()
		}
		out.ServerTimestamp = &n
	case OpReconnect:
		s, e := textAt(payload[0], 256)
		if e != nil {
			return bad()
		}
		n, e := boundedUint(payload[1], 0, 120000)
		if e != nil {
			return bad()
		}
		out.ReconnectAfterMS = &n
		_ = s
	case OpError:
		if _, e := uintAt(payload[0]); e != nil {
			return bad()
		}
		code, e := textAt(payload[1], 128)
		if e != nil || !safeErrorCode(code) {
			return bad()
		}
		out.ErrorCode = code
		if _, e := textAt(payload[2], 512); e != nil {
			return bad()
		}
	case OpHandshake:
		x := &Handshake{}
		if x.Seq, err = boundedUint(payload[0], 1, ^uint64(0)); err != nil {
			return bad()
		}
		if x.Epoch, err = uintAt(payload[1]); err != nil {
			return bad()
		}
		if x.Kind, err = boundedUint(payload[2], 0, 4); err != nil {
			return bad()
		}
		if !isNull(payload[3]) {
			v, e := uintAt(payload[3])
			if e != nil {
				return bad()
			}
			x.Sender = &v
		}
		if x.Blob, err = bytesAt(payload[4], 1, MaxGatewayFrameBytes); err != nil {
			return bad()
		}
		out.Handshake = x
	case OpCommitNeeded:
		x := &CommitNeeded{}
		if x.Epoch, err = uintAt(payload[0]); err != nil {
			return bad()
		}
		var refs []cbor.RawMessage
		if cborx.Unmarshal(payload[1], &refs) != nil || len(refs) > 256 {
			return bad()
		}
		for _, v := range refs {
			b, e := bytesAt(v, 1, 128)
			if e != nil {
				return bad()
			}
			x.ProposalRefs = append(x.ProposalRefs, b)
		}
		if x.DeadlineMS, err = boundedUint(payload[2], 0, 600000); err != nil {
			return bad()
		}
		if x.Round, err = uintAt(payload[3]); err != nil {
			return bad()
		}
		out.CommitNeeded = x
	case OpEpochChanged:
		x := &EpochChanged{}
		if x.Epoch, err = uintAt(payload[0]); err != nil {
			return bad()
		}
		if x.Seq, err = boundedUint(payload[1], 1, ^uint64(0)); err != nil {
			return bad()
		}
		out.EpochChanged = x
	case OpMessageCT:
		x := &MessageCT{}
		if x.Seq, err = boundedUint(payload[0], 1, ^uint64(0)); err != nil {
			return bad()
		}
		if x.Epoch, err = uintAt(payload[1]); err != nil {
			return bad()
		}
		if x.UploaderDevice, err = idAt(payload[2]); err != nil {
			return bad()
		}
		if x.Blob, err = bytesAt(payload[3], 1, MaxGatewayFrameBytes); err != nil {
			return bad()
		}
		if x.FrankingTag, err = bytesAt(payload[4], 32, 32); err != nil {
			return bad()
		}
		if x.ReceivedAt, err = uintAt(payload[5]); err != nil {
			return bad()
		}
		if !isNull(payload[6]) {
			if x.Class, err = boundedUint(payload[6], 0, 1); err != nil {
				return bad()
			}
		} else {
			x.ClassNull = true
		}
		out.MessageCT = x
	case OpWelcome:
		x := &Welcome{}
		if x.WelcomeID, err = uintAt(payload[0]); err != nil {
			return bad()
		}
		if x.Epoch, err = uintAt(payload[1]); err != nil {
			return bad()
		}
		if x.CommitSeq, err = boundedUint(payload[2], 1, ^uint64(0)); err != nil {
			return bad()
		}
		if x.Blob, err = bytesAt(payload[3], 1, MaxGatewayFrameBytes); err != nil {
			return bad()
		}
		if x.RatchetTree, err = bytesAt(payload[4], 1, MaxGatewayFrameBytes); err != nil {
			return bad()
		}
		if x.TreeHash, err = bytesAt(payload[5], 32, 32); err != nil {
			return bad()
		}
		out.Welcome = x
	case OpMessageDeleted:
		x := &MessageDeleted{}
		if x.Seq, err = boundedUint(payload[0], 1, ^uint64(0)); err != nil {
			return bad()
		}
		if x.DeletedAt, err = uintAt(payload[1]); err != nil {
			return bad()
		}
		out.MessageDeleted = x
	case OpMessagePlain:
		x := &MessagePlain{}
		if x.ChannelID, err = idAt(payload[0]); err != nil {
			return bad()
		}
		if x.Seq, err = boundedUint(payload[1], 1, ^uint64(0)); err != nil {
			return bad()
		}
		if x.Sender, err = idAt(payload[2]); err != nil {
			return bad()
		}
		if x.Envelope, err = bytesAt(payload[3], 0, MaxGatewayFrameBytes); err != nil {
			return bad()
		}
		if x.FrankingTag, err = bytesAt(payload[4], 32, 32); err != nil {
			return bad()
		}
		v, e := boundedUint(payload[5], 0, 1)
		if e != nil {
			return bad()
		}
		x.Edited = v == 1
		v, e = boundedUint(payload[6], 0, 1)
		if e != nil {
			return bad()
		}
		x.Deleted = v == 1
		if x.Deleted && len(x.Envelope) != 0 || !x.Deleted && len(x.Envelope) == 0 {
			return bad()
		}
		out.MessagePlain = x
	case OpInteraction:
		x := &Interaction{}
		if x.ID, err = idAt(payload[0]); err != nil {
			return bad()
		}
		if x.BotUserID, err = idAt(payload[1]); err != nil {
			return bad()
		}
		if x.Kind, err = uintAt(payload[2]); err != nil {
			return bad()
		}
		if x.Blob, err = bytesAt(payload[3], 1, MaxGatewayFrameBytes); err != nil {
			return bad()
		}
		out.Interaction = x
	case OpPresence:
		x := &Presence{}
		if x.UserID, err = idAt(payload[0]); err != nil {
			return bad()
		}
		if x.Status, err = boundedUint(payload[1], 0, 3); err != nil {
			return bad()
		}
		if x.SinceTS, err = uintAt(payload[2]); err != nil {
			return bad()
		}
		if x.StatusMessage, err = textAt(payload[3], 256); err != nil {
			return bad()
		}
		out.Presence = x
	case OpTyping:
		x := &Typing{}
		if x.ChannelID, err = idAt(payload[0]); err != nil {
			return bad()
		}
		if x.UserID, err = idAt(payload[1]); err != nil {
			return bad()
		}
		if x.DeviceID, err = idAt(payload[2]); err != nil {
			return bad()
		}
		v, e := boundedUint(payload[3], 0, 1)
		if e != nil {
			return bad()
		}
		x.IsTyping = v == 1
		if x.ExpiresTS, err = uintAt(payload[4]); err != nil {
			return bad()
		}
		out.Typing = x
	case OpVoiceState:
		x := &VoiceState{}
		if x.UserID, err = idAt(payload[0]); err != nil {
			return bad()
		}
		if x.DeviceID, err = idAt(payload[1]); err != nil {
			return bad()
		}
		if x.CallID, err = idAt(payload[2]); err != nil {
			return bad()
		}
		if x.Flags, err = boundedUint(payload[3], 0, 31); err != nil {
			return bad()
		}
		out.VoiceState = x
	case OpCallRing:
		x := &CallRing{}
		if x.CallID, err = idAt(payload[0]); err != nil {
			return bad()
		}
		if x.ChannelID, err = idAt(payload[1]); err != nil {
			return bad()
		}
		if x.UserID, err = idAt(payload[2]); err != nil {
			return bad()
		}
		if x.State, err = boundedUint(payload[3], 0, 2); err != nil {
			return bad()
		}
		if x.ExpiresTS, err = uintAt(payload[4]); err != nil {
			return bad()
		}
		out.CallRing = x
	case OpCommunityChanged:
		v, e := idAt(payload[0])
		if e != nil {
			return bad()
		}
		out.CommunityID = &v
	default:
		return bad()
	}
	return out, nil
}

func serverSpec(op Opcode) (int, bool, bool, bool) {
	switch op {
	case OpHello:
		return 9, false, false, true
	case OpReady:
		return 9, false, true, true
	case OpResumed:
		return 3, false, true, true
	case OpInvalidSession:
		return 2, false, false, true
	case OpHeartbeatAck:
		return 1, false, false, true
	case OpReconnect:
		return 2, false, false, true
	case OpError:
		return 3, false, false, true
	case OpHandshake:
		return 5, true, true, true
	case OpCommitNeeded:
		return 4, true, false, true
	case OpEpochChanged:
		return 2, true, true, true
	case OpMessageCT:
		return 7, true, true, true
	case OpWelcome:
		return 6, true, true, true
	case OpMessageDeleted:
		return 2, true, true, true
	case OpMessagePlain:
		return 7, false, true, true
	case OpInteraction:
		return 4, false, true, true
	case OpPresence:
		return 4, false, false, true
	case OpTyping:
		return 5, false, false, true
	case OpVoiceState:
		return 4, false, false, true
	case OpCallRing:
		return 5, false, false, true
	case OpCommunityChanged:
		return 1, false, false, true
	}
	return 0, false, false, false
}

func uintAt(raw cbor.RawMessage) (uint64, error) {
	if cborx.ExpectMajor(raw, cborx.MajorUint) != nil {
		return 0, ErrProtocol
	}
	var v uint64
	if cborx.Unmarshal(raw, &v) != nil {
		return 0, ErrProtocol
	}
	return v, nil
}
func boundedUint(raw cbor.RawMessage, minimum, maximum uint64) (uint64, error) {
	v, e := uintAt(raw)
	if e != nil || v < minimum || v > maximum {
		return 0, ErrProtocol
	}
	return v, nil
}
func bytesAt(raw cbor.RawMessage, minimum, maximum int) ([]byte, error) {
	if cborx.ExpectMajor(raw, cborx.MajorBytes) != nil {
		return nil, ErrProtocol
	}
	var b []byte
	if cborx.Unmarshal(raw, &b) != nil || len(b) < minimum || len(b) > maximum {
		return nil, ErrProtocol
	}
	return b, nil
}
func idAt(raw cbor.RawMessage) (id.ID, error) {
	var z id.ID
	b, e := bytesAt(raw, 16, 16)
	if e != nil {
		return z, e
	}
	copy(z[:], b)
	if z.IsZero() {
		return id.ID{}, ErrProtocol
	}
	return z, nil
}
func textAt(raw cbor.RawMessage, maximum int) (string, error) {
	if cborx.ExpectMajor(raw, cborx.MajorText) != nil {
		return "", ErrProtocol
	}
	var s string
	if cborx.Unmarshal(raw, &s) != nil || len(s) > maximum {
		return "", ErrProtocol
	}
	return s, nil
}
func uintsAt(raw cbor.RawMessage, maximum int) ([]uint64, error) {
	var a []cbor.RawMessage
	if cborx.Unmarshal(raw, &a) != nil || len(a) == 0 || len(a) > maximum {
		return nil, ErrProtocol
	}
	out := make([]uint64, 0, len(a))
	for _, v := range a {
		n, e := uintAt(v)
		if e != nil {
			return nil, e
		}
		out = append(out, n)
	}
	return out, nil
}
func isNull(raw cbor.RawMessage) bool { return len(raw) == 1 && raw[0] == 0xf6 }

// dropMalformedEphemeral mirrors protocol/02: malformed voice_state, call_ring,
// and community_changed notifications are dropped without terminating the socket.
func dropMalformedEphemeral(raw []byte) bool {
	var envelope []cbor.RawMessage
	if cborx.Unmarshal(raw, &envelope) != nil || len(envelope) != 4 {
		return false
	}
	op, err := uintAt(envelope[0])
	if err != nil || op > 255 {
		return false
	}
	switch Opcode(op) {
	case OpVoiceState, OpCallRing, OpCommunityChanged:
		return true
	default:
		return false
	}
}

func safeErrorCode(s string) bool {
	if len(s) < 3 || len(s) > 64 || s[0] != 'E' || s[1] != '_' {
		return false
	}
	for i := 2; i < len(s); i++ {
		if (s[i] < 'A' || s[i] > 'Z') && (s[i] < '0' || s[i] > '9') && s[i] != '_' {
			return false
		}
	}
	return true
}

func (f Frame) String() string { return fmt.Sprintf("gateway frame opcode=%d n=%d", f.Op, f.N) }
