// Package gatewayclient implements the bounded, typed bot-side gateway transport.
package gatewayclient

import (
	"errors"
	"fmt"

	"github.com/fxamacker/cbor/v2"

	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
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
	// Bot v2: one HTTP-shaped app request over the socket (client to server) and its answer (server to client).
	OpRequest  Opcode = 59
	OpResponse Opcode = 60
	// Bot v2 tasks 10 and 12: a member's sealed command and the Bot's event feed, on the Bot runtime's connection only.
	// Both are sequenced in its ring; a cold-start snapshot's op 65 body frames are sent with n = 0.
	OpBotCommand Opcode = 64
	OpBotEvent   Opcode = 65
	// Bot v2 task 13: an incoming webhook for the Bot to post, never ringed (n = 0). Its consumer, which answers
	// with op 67 bot_webhook_ack, comes in a later task; until then the frame is decoded and dropped.
	OpBotWebhook    Opcode = 66
	OpBotWebhookAck Opcode = 67
)

const (
	// MaxRequestBodyBytes is the instance's request body cap (gateway.MaxRequestBodyBytes).
	MaxRequestBodyBytes = 16 << 20
	// MaxResponseFrameBytes bounds a response frame: a route's answer (16 MiB by default) plus the frame's own bytes,
	// the instance's request-frame bound. Only op 60 may be this large; every other frame keeps the hello's bound.
	MaxResponseFrameBytes = MaxRequestBodyBytes + 16<<10
	maxRequestMethod      = 16
	maxRequestRoute       = 8192
	maxRequestIdem        = 64
)

// ResponseFrame is one decoded op 60 answer: the request's id, its HTTP status and its body.
type ResponseFrame struct {
	ID     uint64
	Status int
	Body   []byte
}

// EncodeRequestFrame builds op 59 [id, method, route, body, idem|null] with n 0 and no group. It refuses what the
// instance would refuse: an empty or long method or route, an idem of 0 or more than 64 bytes, a body over the cap.
func EncodeRequestFrame(requestID uint64, method, route string, body, idem []byte) ([]byte, error) {
	if len(method) == 0 || len(method) > maxRequestMethod || len(route) == 0 || len(route) > maxRequestRoute || len(body) > MaxRequestBodyBytes {
		return nil, ErrProtocol
	}
	var key any
	if idem != nil {
		if len(idem) == 0 || len(idem) > maxRequestIdem {
			return nil, ErrProtocol
		}
		key = idem
	}
	if body == nil {
		body = []byte{}
	}
	b, err := cborx.Marshal([]any{uint64(OpRequest), uint64(0), nil, []any{requestID, method, route, body, key}})
	if err != nil {
		return nil, ErrProtocol
	}
	return b, nil
}

// isResponseFrame reports whether raw begins a four-element frame whose op is 60, without decoding the rest.
func isResponseFrame(raw []byte) bool {
	return len(raw) >= 3 && raw[0] == 0x84 && raw[1] == 0x18 && raw[2] == byte(OpResponse)
}

// DecodeResponseFrame checks op 60 strictly: deterministic CBOR, n 0, no group, [id uint, status 100..599, body bytes].
func DecodeResponseFrame(raw []byte) (ResponseFrame, error) {
	var out ResponseFrame
	if len(raw) > MaxResponseFrameBytes {
		return out, ErrProtocol
	}
	var elems []cbor.RawMessage
	if err := cborx.Unmarshal(raw, &elems); err != nil || len(elems) != 4 {
		return out, ErrProtocol
	}
	if op, err := uintAt(elems[0]); err != nil || Opcode(op) != OpResponse {
		return out, ErrProtocol
	}
	if n, err := uintAt(elems[1]); err != nil || n != 0 || !isNull(elems[2]) {
		return out, ErrProtocol
	}
	var payload []cbor.RawMessage
	if cborx.ExpectMajor(elems[3], cborx.MajorArray) != nil || cborx.Unmarshal(elems[3], &payload) != nil || len(payload) != 3 {
		return out, ErrProtocol
	}
	var err error
	if out.ID, err = uintAt(payload[0]); err != nil {
		return ResponseFrame{}, ErrProtocol
	}
	status, err := boundedUint(payload[1], 100, 599)
	if err != nil {
		return ResponseFrame{}, ErrProtocol
	}
	out.Status = int(status)
	if out.Body, err = bytesAt(payload[2], 0, MaxResponseFrameBytes); err != nil {
		return ResponseFrame{}, ErrProtocol
	}
	return out, nil
}

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
	// BootID is random per dillad process (ready element 9): a restart changes it.
	BootID id.ID
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
	// BotPayload is an op 64, 65 or 66 frame's payload elements, shape-checked only: op 64 is [request_id, community_id,
	// user_id, device_id, sealed], op 65 is [kind, payload], op 66 is [delivery_id, webhook_id, community_id,
	// channel_id, display_name, avatar_url|null, text, allow_everyone]. Their consumers decode them.
	BotPayload []cbor.RawMessage
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
	// Op 65 is the one opcode with both forms: ringed live, unringed inside a cold-start snapshot.
	if out.Op != OpBotEvent && (replayable && out.N == 0 || !replayable && out.N != 0) {
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
		if r.BootID, err = idAt(payload[9]); err != nil {
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
	case OpBotCommand, OpBotEvent, OpBotWebhook:
		out.BotPayload = payload
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
		return 10, false, true, true
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
	case OpBotCommand:
		return 5, false, true, true
	case OpBotEvent:
		return 2, false, true, true
	case OpBotWebhook:
		return 8, false, false, true
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

// opAppBuild is the gateway's app_build notice (op 56): the build the instance serves, pushed to web clients on wire 6.
// The instance does not send it to managed Bot connections, but an older or other instance might; the Bot has no web
// client to update, so it drops the frame (an unknown opcode would end the socket as a protocol error).
const opAppBuild Opcode = 56

// dropMalformedEphemeral mirrors protocol/02: malformed voice_state, call_ring,
// and community_changed notifications are dropped without terminating the socket. So is the app_build notice, which the
// Bot never reads.
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
	case OpVoiceState, OpCallRing, OpCommunityChanged, opAppBuild:
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
