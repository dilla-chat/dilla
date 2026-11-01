// Package protocol contains the pure, deterministic codecs for the private bot
// protocol. It performs structural validation only; authorization and state
// checks belong to authenticated callers and dispatchers.
package protocol

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"math"
	"strings"
	"unicode/utf8"

	"github.com/jonasthim/dilla/internal/cborx"
	"github.com/jonasthim/dilla/internal/id"
)

const (
	BodyPrefix               = "dilla.bot/1:"
	MaxBodyBytes             = 4000
	MaxEventBytes            = 4096
	MaxHelperBytes           = 1 << 20
	MaxHelperPayload         = 1048480
	MaxModerationCursorBytes = 256
)

var (
	ErrInvalid = errors.New("bot protocol: invalid payload")
	ErrContext = errors.New("bot protocol: context mismatch")
	ErrExpired = errors.New("bot protocol: expired")
	ErrLimit   = errors.New("bot protocol: size limit exceeded")
)

type Operation uint64

const (
	OpOverviewGet          Operation = 0
	OpSettingsGet          Operation = 1
	OpSettingsPut          Operation = 2
	OpMusicState           Operation = 10
	OpMusicSearch          Operation = 11
	OpMusicEnqueue         Operation = 12
	OpMusicPause           Operation = 13
	OpMusicResume          Operation = 14
	OpMusicSkip            Operation = 15
	OpMusicStop            Operation = 16
	OpMusicSeek            Operation = 17
	OpMusicVolume          Operation = 18
	OpMusicShuffle         Operation = 19
	OpMusicRepeat          Operation = 20
	OpMusicRemove          Operation = 21
	OpMusicMove            Operation = 22
	OpMusicClear           Operation = 23
	OpMusicProvidersStatus Operation = 24
	// OpMusicAnnounce posts "<requester> started music in <voice channel>" with the player marker in the text channel
	// the requester names (args[0]); the request's channel is the voice channel. Revision must be 0.
	OpMusicAnnounce          Operation = 25
	OpWelcomePreview         Operation = 30
	OpRolePanelPublish       Operation = 31
	OpRolesList              Operation = 40
	OpAnnouncementsList      Operation = 50
	OpAnnouncementsPut       Operation = 51
	OpAnnouncementsDelete    Operation = 52
	OpModerationCases        Operation = 60
	OpModerationEvidence     Operation = 61
	OpAutoModerationGet      Operation = 62
	OpAutoModerationPut      Operation = 63
	OpModerationEvidenceList Operation = 64
	OpCapabilities           Operation = 3
	OpPollCreate             Operation = 70
	OpPollGet                Operation = 71
	OpPollList               Operation = 72
	OpPollVote               Operation = 73
	OpPollClose              Operation = 74
	OpPollVoters             Operation = 75
	OpReminderCreate         Operation = 80
	OpReminderList           Operation = 81
	OpReminderCancel         Operation = 82
	OpReminderDismiss        Operation = 83
	OpReminderDue            Operation = 84
)

type Request struct {
	RequestID, CommunityID id.ID
	ChannelID              *id.ID
	Operation              Operation
	Revision, Expires      *uint64
	Args                   []any
}

type Response struct {
	RequestID, CommunityID id.ID
	Status                 uint64
	Revision               *uint64
	Data                   []byte // empty or one deterministic CBOR array
}

func u64(v uint64) *uint64 { return &v }
func ptrID(v id.ID) *id.ID { return &v }
func isID(v id.ID) bool    { return !v.IsZero() }
func invalid() error       { return ErrInvalid }

func EncodeRequest(r Request) (string, error) {
	b, err := EncodeRequestCBOR(r)
	if err != nil {
		return "", err
	}
	return encodeBody(b)
}
func DecodeRequest(body string) (Request, error) {
	b, err := decodeBody(body)
	if err != nil {
		return Request{}, err
	}
	return DecodeRequestCBOR(b)
}
func EncodeResponse(r Response) (string, error) {
	b, err := EncodeResponseCBOR(r)
	if err != nil {
		return "", err
	}
	return encodeBody(b)
}
func DecodeResponse(body string) (Response, error) {
	b, err := decodeBody(body)
	if err != nil {
		return Response{}, err
	}
	return DecodeResponseCBOR(b)
}

func encodeBody(b []byte) (string, error) {
	if len(BodyPrefix)+base64.RawURLEncoding.EncodedLen(len(b)) > MaxBodyBytes {
		return "", ErrLimit
	}
	return BodyPrefix + base64.RawURLEncoding.EncodeToString(b), nil
}
func decodeBody(s string) ([]byte, error) {
	if len(s) > MaxBodyBytes || !strings.HasPrefix(s, BodyPrefix) {
		return nil, invalid()
	}
	enc := s[len(BodyPrefix):]
	if strings.ContainsAny(enc, "= \t\r\n") {
		return nil, invalid()
	}
	b, err := base64.RawURLEncoding.DecodeString(enc)
	if err != nil || base64.RawURLEncoding.EncodeToString(b) != enc {
		return nil, invalid()
	}
	return b, nil
}

func EncodeRequestCBOR(r Request) ([]byte, error) {
	if validateRequest(r) != nil {
		return nil, invalid()
	}
	var ch any
	if r.ChannelID != nil {
		ch = *r.ChannelID
	}
	var rev, exp any
	if r.Revision != nil {
		rev = *r.Revision
	}
	if r.Expires != nil {
		exp = *r.Expires
	}
	args := r.Args
	if args == nil {
		args = []any{}
	}
	b, err := cborx.Marshal([]any{uint64(1), r.RequestID, r.CommunityID, ch, uint64(r.Operation), rev, exp, args})
	if err != nil {
		return nil, invalid()
	}
	if len(b) > MaxBodyBytes {
		return nil, ErrLimit
	}
	return b, nil
}
func DecodeRequestCBOR(b []byte) (Request, error) {
	var z Request
	if len(b) > MaxBodyBytes {
		return z, ErrLimit
	}
	n, err := parse(b)
	if err != nil || len(n.a) != 8 || n.a[0].u != 1 {
		return z, invalid()
	}
	a := n.a
	r := Request{RequestID: nodeID(a[1]), CommunityID: nodeID(a[2]), Operation: Operation(a[4].u)}
	if a[0].major != 0 || a[0].u != 1 || a[1].major != 2 || a[2].major != 2 || a[4].major != 0 || !isNullableIDNode(a[3]) || !isNullableUintNode(a[5]) || !isNullableUintNode(a[6]) || a[7].major != 4 {
		return z, invalid()
	}
	if !a[3].null {
		v := nodeID(a[3])
		r.ChannelID = &v
	}
	if !a[5].null {
		v := a[5].u
		r.Revision = &v
	}
	if !a[6].null {
		v := a[6].u
		r.Expires = &v
	}
	r.Args = nodesToAny(a[7].a)
	requestIDs(r.Operation, r.Args)
	if err := validateRequest(r); err != nil {
		return z, invalid()
	}
	return r, nil
}
func validateRequest(r Request) error {
	if !isID(r.RequestID) || !isID(r.CommunityID) {
		return invalid()
	}
	mut, channel, count := requestShape(r.Operation)
	if count < 0 || len(r.Args) != count && (r.Operation != OpMusicSkip || len(r.Args) != 1) {
		return invalid()
	}
	if channel != (r.ChannelID != nil) || (r.ChannelID != nil && !isID(*r.ChannelID)) {
		return invalid()
	}
	if mut != (r.Revision != nil && r.Expires != nil) {
		return invalid()
	}
	if !mut && (r.Revision != nil || r.Expires != nil) {
		return invalid()
	}
	if (r.Operation == OpRolePanelPublish || r.Operation == OpPollCreate || r.Operation == OpReminderCreate || r.Operation == OpMusicAnnounce) && *r.Revision != 0 {
		return invalid()
	}
	if err := validateArgs(r.Operation, r.Args); err != nil {
		return invalid()
	}
	return nil
}
func requestShape(op Operation) (mutation, channel bool, count int) {
	switch op {
	case OpOverviewGet, OpCapabilities, OpMusicProvidersStatus:
		return false, false, 0
	case OpMusicState:
		return false, true, 0
	case OpMusicPause, OpMusicResume, OpMusicSkip, OpMusicStop, OpMusicShuffle, OpMusicClear:
		return true, true, 0
	case OpSettingsGet:
		return false, false, 1
	case OpSettingsPut:
		return true, false, 2
	case OpMusicSearch, OpWelcomePreview:
		return false, op == OpMusicSearch, 1
	case OpRolePanelPublish:
		return true, false, 2
	case OpMusicEnqueue:
		return true, true, 1
	case OpMusicSeek, OpMusicVolume, OpMusicRepeat:
		return true, true, 1
	case OpMusicRemove, OpMusicAnnounce:
		return true, true, 1
	case OpAnnouncementsDelete:
		return true, false, 2
	case OpMusicMove, OpAnnouncementsPut:
		return true, op == OpMusicMove, 2
	case OpRolesList, OpAnnouncementsList, OpModerationCases, OpModerationEvidenceList:
		return false, false, 1
	case OpModerationEvidence:
		return true, false, 3
	case OpAutoModerationGet:
		return false, false, 0
	case OpAutoModerationPut:
		return true, false, 2
	case OpPollCreate, OpReminderCreate:
		return true, false, 1
	case OpPollGet, OpPollClose, OpReminderCancel, OpReminderDismiss:
		return op == OpPollClose || op == OpReminderCancel || op == OpReminderDismiss, false, 1
	case OpPollList, OpPollVoters:
		return false, false, 3
	case OpPollVote:
		return true, false, 2
	case OpReminderList, OpReminderDue:
		return false, false, 1
	default:
		return false, false, -1
	}
}
func validateArgs(op Operation, a []any) error {
	u := func(i int, maximum uint64) bool { v, ok := a[i].(uint64); return ok && v <= maximum }
	idarg := func(i int) bool { v, ok := a[i].(id.ID); return ok && isID(v) }
	str := func(i, minimum, maximum int, plain bool) bool {
		v, ok := a[i].(string)
		if !ok || !utf8.ValidString(v) || len([]byte(v)) < minimum || len([]byte(v)) > maximum {
			return false
		}
		if plain {
			if strings.TrimSpace(v) == "" {
				return false
			}
			for _, r := range v {
				if r < 32 || r == 127 {
					return false
				}
			}
		}
		return true
	}
	data := func(i, minimum, maximum int) bool {
		v, ok := a[i].([]byte)
		return ok && len(v) >= minimum && len(v) <= maximum && validArray(v)
	}
	switch op {
	case OpOverviewGet, OpCapabilities, OpMusicState, OpMusicProvidersStatus, OpMusicPause, OpMusicResume, OpMusicStop, OpMusicShuffle, OpMusicClear:
		return nil
	case OpMusicSkip:
		if len(a) == 0 {
			return nil
		}
		expected, ok := a[0].(string)
		if !ok {
			return invalid()
		}
		if expected == "" {
			return nil
		}
		item, err := id.Parse(expected)
		return boolErr(err == nil && !item.IsZero())
	case OpSettingsGet:
		return boolErr(u(0, 5))
	case OpSettingsPut:
		return boolErr(u(0, 5) && data(1, 1, 2400))
	case OpMusicSearch:
		return boolErr(str(0, 1, 200, true))
	case OpMusicEnqueue:
		return boolErr(str(0, 1, 2048, true))
	case OpMusicSeek, OpRolesList, OpAnnouncementsList:
		return boolErr(isUint(a[0]))
	case OpModerationEvidenceList:
		if a[0] == nil {
			return nil
		}
		cursor, ok := a[0].([]byte)
		return boolErr(ok && len(cursor) > 0 && len(cursor) <= MaxModerationCursorBytes)
	case OpModerationCases:
		if a[0] == nil {
			return nil
		}
		cursor, ok := a[0].([]byte)
		return boolErr(ok && len(cursor) > 0 && len(cursor) <= MaxModerationCursorBytes)
	case OpMusicVolume:
		return boolErr(u(0, 100))
	case OpMusicRepeat:
		return boolErr(u(0, 2))
	case OpMusicRemove, OpMusicAnnounce:
		return boolErr(idarg(0))
	case OpAnnouncementsDelete:
		if len(a) != 2 {
			return boolErr(false)
		}
		value, ok := a[1].(uint64)
		return boolErr(idarg(0) && ok && value <= math.MaxInt64 && value > 0)
	case OpMusicMove:
		return boolErr(idarg(0) && nullableID(a[1]))
	case OpWelcomePreview:
		return boolErr(str(0, 1, 2000, false))
	case OpRolePanelPublish:
		channel, ok := a[0].(id.ID)
		payload, payloadOK := a[1].([]byte)
		return boolErr(ok && isID(channel) && payloadOK && validateRolePanelMappings(payload) == nil)
	case OpAnnouncementsPut:
		return boolErr(nullableID(a[0]) && data(1, 1, 2200))
	case OpModerationEvidence:
		return boolErr(idarg(0) && str(1, 0, 1024, false) && str(2, 0, 512, false))
	case OpAutoModerationGet:
		return nil
	case OpAutoModerationPut:
		return boolErr(str(0, 1, 512, true) && data(1, 1, 2400))
	case OpPollCreate:
		return boolErr(validateUtilityPayload(a[0], true) == nil)
	case OpReminderCreate:
		return boolErr(validateUtilityPayload(a[0], false) == nil)
	case OpPollGet, OpPollClose, OpReminderCancel, OpReminderDismiss:
		return boolErr(idarg(0))
	case OpPollList:
		return boolErr(idarg(0) && nullableID(a[1]) && validCursor(a[2]))
	case OpPollVote:
		return boolErr(idarg(0) && validatePollSelection(a[1]) == nil)
	case OpPollVoters:
		return boolErr(idarg(0) && u(1, 9) && validCursor(a[2]))
	case OpReminderList, OpReminderDue:
		return boolErr(validCursor(a[0]))
	default:
		return invalid()
	}
}

func validateRolePanelMappings(raw []byte) error {
	if len(raw) == 0 || len(raw) > 2200 || !validArray(raw) {
		return invalid()
	}
	var rows []any
	if cborx.Unmarshal(raw, &rows) != nil || len(rows) == 0 || len(rows) > 100 {
		return invalid()
	}
	emojis := make(map[string]bool, len(rows))
	roles := make(map[id.ID]bool, len(rows))
	for _, value := range rows {
		row, ok := value.([]any)
		if !ok || len(row) != 3 {
			return invalid()
		}
		emoji, emojiOK := row[0].(string)
		roleBytes, roleOK := row[1].([]byte)
		name, nameOK := row[2].(string)
		var role id.ID
		if roleOK && len(roleBytes) == len(role) {
			copy(role[:], roleBytes)
		}
		if !emojiOK || !roleOK || !nameOK || !isID(role) || !utf8.ValidString(emoji) || len([]byte(emoji)) == 0 || len([]byte(emoji)) > 32 || strings.ContainsAny(emoji, "\x00\r\n\x7f") || emojis[emoji] || roles[role] || !utf8.ValidString(name) || len([]byte(name)) == 0 || len([]byte(name)) > 100 || strings.TrimSpace(name) == "" {
			return invalid()
		}
		for _, r := range emoji {
			if r < 32 {
				return invalid()
			}
		}
		for _, r := range name {
			if r < 32 || r == 127 {
				return invalid()
			}
		}
		emojis[emoji] = true
		roles[role] = true
	}
	return nil
}
func boolErr(ok bool) error {
	if !ok {
		return invalid()
	}
	return nil
}
func isUint(v any) bool { _, ok := v.(uint64); return ok }
func nullableID(v any) bool {
	if v == nil {
		return true
	}
	x, ok := v.(id.ID)
	return ok && isID(x)
}
func isNullableIDNode(n node) bool   { return n.null || (n.major == 2 && len(n.b) == 16) }
func isNullableUintNode(n node) bool { return n.null || n.major == 0 }
func requestIDs(op Operation, a []any) {
	switch op {
	case OpMusicRemove, OpAnnouncementsDelete, OpMusicAnnounce:
		initIDs(a, 0)
	case OpMusicMove:
		initIDs(a, 0, 1)
	case OpAnnouncementsPut:
		initIDs(a, 0)
	case OpModerationEvidence:
		initIDs(a, 0)
	case OpRolePanelPublish:
		initIDs(a, 0)
	case OpPollGet, OpPollClose, OpPollVote, OpPollVoters, OpReminderCancel, OpReminderDismiss:
		initIDs(a, 0)
	case OpPollList:
		initIDs(a, 0, 1)
	}
}

func validCursor(v any) bool {
	if v == nil {
		return true
	}
	b, ok := v.([]byte)
	return ok && len(b) >= 1 && len(b) <= MaxModerationCursorBytes
}

func validatePollSelection(v any) error {
	b, ok := v.([]byte)
	if !ok || len(b) == 0 || len(b) > 2200 || !validArray(b) {
		return invalid()
	}
	var values []any
	if cborx.Unmarshal(b, &values) != nil || len(values) > 10 {
		return invalid()
	}
	seen := map[uint64]bool{}
	for _, value := range values {
		n, ok := value.(uint64)
		if !ok || n > 9 || seen[n] {
			return invalid()
		}
		seen[n] = true
	}
	return nil
}

func validateUtilityPayload(v any, poll bool) error {
	b, ok := v.([]byte)
	maxBytes := 2200
	if poll {
		maxBytes = 1800
	}
	if !ok || len(b) == 0 || len(b) > maxBytes || !validArray(b) {
		return invalid()
	}
	var fields []any
	if cborx.Unmarshal(b, &fields) != nil {
		return invalid()
	}
	if poll {
		if len(fields) != 7 || fields[0] != uint64(1) {
			return invalid()
		}
		if _, ok := asNonzeroID(fields[1]); !ok {
			return invalid()
		}
		if fields[2] != nil {
			if _, ok := asNonzeroID(fields[2]); !ok {
				return invalid()
			}
		}
		question, ok := fields[3].(string)
		if !ok || !utf8.ValidString(question) || strings.TrimSpace(question) == "" || utf8.RuneCountInString(question) > 300 {
			return invalid()
		}
		answers, ok := fields[4].([]any)
		if !ok || len(answers) < 2 || len(answers) > 10 {
			return invalid()
		}
		seen := map[string]bool{}
		for _, item := range answers {
			s, ok := item.(string)
			if !ok || !utf8.ValidString(s) || strings.TrimSpace(s) == "" || utf8.RuneCountInString(s) > 100 || seen[s] {
				return invalid()
			}
			seen[s] = true
		}
		multi, ok := fields[5].(uint64)
		if !ok || multi > 1 {
			return invalid()
		}
		duration, ok := fields[6].(uint64)
		if !ok {
			return invalid()
		}
		switch duration {
		case 3600, 14400, 28800, 86400, 259200, 604800:
		default:
			return invalid()
		}
		return nil
	}
	if len(fields) != 7 || fields[0] != uint64(1) {
		return invalid()
	}
	kind, ok := fields[1].(uint64)
	if !ok || kind > 1 {
		return invalid()
	}
	when, ok := fields[2].(uint64)
	if !ok {
		return invalid()
	}
	if (kind == 0 && (when < 60 || when > 31536000)) || (kind == 1 && (when < 1 || when > 253402300799)) {
		return invalid()
	}
	text, ok := fields[3].(string)
	if !ok || !utf8.ValidString(text) || len([]byte(text)) < 1 || len([]byte(text)) > 1500 {
		return invalid()
	}
	tz, ok := fields[4].(string)
	if !ok || !utf8.ValidString(tz) || len([]byte(tz)) < 1 || len([]byte(tz)) > 100 {
		return invalid()
	}
	channel, channelOK := asNullableID(fields[5])
	thread, threadOK := asNullableID(fields[6])
	if !channelOK || !threadOK || (thread != nil && channel == nil) {
		return invalid()
	}
	return nil
}

func asNonzeroID(v any) (id.ID, bool) {
	b, ok := v.([]byte)
	if !ok || len(b) != 16 {
		return id.ID{}, false
	}
	var out id.ID
	copy(out[:], b)
	return out, isID(out)
}
func asNullableID(v any) (*id.ID, bool) {
	if v == nil {
		return nil, true
	}
	out, ok := asNonzeroID(v)
	if !ok {
		return nil, false
	}
	return &out, true
}
func validArray(b []byte) bool { n, e := parse(b); return e == nil && n.major == 4 }

func ValidateRequestContext(r Request, expected id.ID, now uint64) error {
	if r.CommunityID != expected || !isID(expected) {
		return ErrContext
	}
	mut, _, _ := requestShape(r.Operation)
	if !mut {
		return nil
	}
	if r.Expires == nil || *r.Expires <= now {
		return ErrExpired
	}
	if now > math.MaxUint64-30 || *r.Expires > now+30 {
		return ErrExpired
	}
	return nil
}

func EncodeResponseCBOR(r Response) ([]byte, error) {
	if !isID(r.RequestID) || !isID(r.CommunityID) || r.Status > 9 || (r.Revision == nil && false) || len(r.Data) > 2400 || (len(r.Data) > 0 && !validArray(r.Data)) {
		return nil, invalid()
	}
	var rev any
	if r.Revision != nil {
		rev = *r.Revision
	}
	data := any(r.Data)
	b, e := cborx.Marshal([]any{uint64(1), r.RequestID, r.CommunityID, r.Status, rev, data})
	if e != nil {
		return nil, invalid()
	}
	if len(b) > MaxBodyBytes {
		return nil, ErrLimit
	}
	return b, nil
}
func DecodeResponseCBOR(b []byte) (Response, error) {
	var z Response
	if len(b) > MaxBodyBytes {
		return z, ErrLimit
	}
	n, e := parse(b)
	if e != nil || len(n.a) != 6 {
		return z, invalid()
	}
	a := n.a
	if a[0].major != 0 || a[0].u != 1 || a[1].major != 2 || a[2].major != 2 || a[3].major != 0 || !isNullableUintNode(a[4]) || a[5].major != 2 {
		return z, invalid()
	}
	r := Response{RequestID: nodeID(a[1]), CommunityID: nodeID(a[2]), Status: a[3].u}
	if !a[4].null {
		v := a[4].u
		r.Revision = &v
	}
	r.Data = make([]byte, len(a[5].b))
	copy(r.Data, a[5].b)
	if !isID(r.RequestID) || !isID(r.CommunityID) || r.Status > 9 || len(r.Data) > 2400 || (len(r.Data) > 0 && !validArray(r.Data)) {
		return z, invalid()
	}
	return r, nil
}
func ValidateResponseContext(r Response, expected id.ID) error {
	if !isID(expected) || r.CommunityID != expected {
		return ErrContext
	}
	return nil
}

const removalDomain = "dilla moderation removal v1"

type RemovalProof struct {
	RequestID, ActorUser, ActorDevice, CommunityID, GroupID id.ID
	TargetSequence, Expires                                 uint64
}

func RemovalPreimage(p RemovalProof) ([]byte, error) {
	b, e := encodeRemoval(p)
	if e != nil {
		return nil, e
	}
	return append([]byte(removalDomain), b...), nil
}
func encodeRemoval(p RemovalProof) ([]byte, error) {
	if !isID(p.RequestID) || !isID(p.ActorUser) || !isID(p.ActorDevice) || !isID(p.CommunityID) || !isID(p.GroupID) || p.TargetSequence == 0 || p.Expires == 0 {
		return nil, invalid()
	}
	return cborx.Marshal([]any{uint64(1), p.RequestID, p.ActorUser, p.ActorDevice, p.CommunityID, p.GroupID, p.TargetSequence, p.Expires})
}
func DecodeRemovalProof(b []byte) (RemovalProof, error) {
	var z RemovalProof
	// The fixed proof shape cannot exceed 105 bytes: array/version heads, five
	// 16-byte identifiers, and two uint64 values at their maximum encoding size.
	if len(b) > 105 {
		return z, ErrLimit
	}
	n, e := parse(b)
	if e != nil || len(n.a) != 8 {
		return z, invalid()
	}
	a := n.a
	if a[0].major != 0 || a[0].u != 1 || a[6].major != 0 || a[7].major != 0 {
		return z, invalid()
	}
	for _, i := range []int{1, 2, 3, 4, 5} {
		if a[i].major != 2 || len(a[i].b) != 16 {
			return z, invalid()
		}
	}
	p := RemovalProof{nodeID(a[1]), nodeID(a[2]), nodeID(a[3]), nodeID(a[4]), nodeID(a[5]), a[6].u, a[7].u}
	if _, e = encodeRemoval(p); e != nil {
		return z, invalid()
	}
	return p, nil
}
func ValidateRemovalContext(p RemovalProof, community, group, actorUser, actorDevice id.ID, now uint64) error {
	if p.CommunityID != community || p.GroupID != group || p.ActorUser != actorUser || p.ActorDevice != actorDevice {
		return ErrContext
	}
	if p.Expires <= now || now > math.MaxUint64-30 || p.Expires > now+30 {
		return ErrExpired
	}
	return nil
}

type EventKind uint64

const (
	EventMemberJoin EventKind = 1 + iota
	EventMemberLeave
	EventInstallation
	EventPermission
	EventChannel
	EventModeration
	EventRolePanel
	EventDeviceList
)

type Event struct {
	ID, CommunityID     id.ID
	Sequence, Timestamp uint64
	Kind                EventKind
	Data                []any
}

func EncodeEvent(e Event) ([]byte, error) {
	if !isID(e.ID) || !isID(e.CommunityID) || e.Sequence == 0 || e.Kind < 1 || e.Kind > 8 || validateEventData(e.Kind, e.Data, e.Timestamp) != nil {
		return nil, invalid()
	}
	b, er := cborx.Marshal([]any{uint64(1), e.ID, e.Sequence, e.CommunityID, e.Timestamp, uint64(e.Kind), e.Data})
	if er != nil || len(b) > MaxEventBytes {
		return nil, invalid()
	}
	return b, nil
}
func DecodeEvent(b []byte) (Event, error) {
	var z Event
	if len(b) > MaxEventBytes {
		return z, ErrLimit
	}
	n, e := parse(b)
	if e != nil || len(n.a) != 7 {
		return z, invalid()
	}
	a := n.a
	for _, i := range []int{0, 2, 4, 5} {
		if a[i].major != 0 {
			return z, invalid()
		}
	}
	if a[0].u != 1 || a[1].major != 2 || a[3].major != 2 || a[6].major != 4 {
		return z, invalid()
	}
	ev := Event{ID: nodeID(a[1]), Sequence: a[2].u, CommunityID: nodeID(a[3]), Timestamp: a[4].u, Kind: EventKind(a[5].u), Data: nodesToAny(a[6].a)}
	eventIDs(ev.Kind, ev.Data)
	if _, e = EncodeEvent(ev); e != nil {
		return z, invalid()
	}
	return ev, nil
}
func ValidateEventContext(e Event, expected id.ID) error {
	if !isID(expected) || e.CommunityID != expected {
		return ErrContext
	}
	return nil
}
func validateEventData(k EventKind, d []any, ts uint64) error {
	u := func(i int, maximum uint64) bool { v, ok := d[i].(uint64); return ok && v <= maximum }
	ii := func(i int) bool { v, ok := d[i].(id.ID); return ok && isID(v) }
	ni := func(i int) bool { return nullableID(d[i]) }
	count := map[EventKind]int{1: 2, 2: 2, 3: 3, 4: 3, 5: 3, 6: 9, 7: 3, 8: 2}[k]
	if len(d) != count {
		return invalid()
	}
	switch k {
	case 1, 2:
		if !ii(0) || !ii(1) {
			return invalid()
		}
	case 3:
		if !ii(0) || !u(1, 5) || !isUint(d[2]) {
			return invalid()
		}
	case 4:
		if !u(0, 3) || !ii(1) || !isUint(d[2]) {
			return invalid()
		}
	case 5:
		if !ii(0) || !u(1, 2) || !isUint(d[2]) {
			return invalid()
		}
	case 6:
		if !ii(0) || !ii(1) || !ii(2) || !ii(3) || !ni(4) || !ni(5) || !nullableUint(d[6]) || !u(7, 6) || !nullableUint(d[8]) {
			return invalid()
		}
		act, ok := d[7].(uint64)
		if !ok {
			return invalid()
		}
		if act == 4 {
			if d[5] == nil || d[6] == nil {
				return invalid()
			}
		} else if d[4] == nil || d[5] != nil || d[6] != nil {
			return invalid()
		}
		if act == 1 {
			expires, ok := d[8].(uint64)
			if d[8] == nil || !ok || expires <= ts {
				return invalid()
			}
		} else if act != 3 && d[8] != nil {
			return invalid()
		}
		if act == 3 && d[8] != nil {
			expires, ok := d[8].(uint64)
			if !ok || expires <= ts {
				return invalid()
			}
		}
	case 7:
		if !ii(0) || !isUint(d[1]) || !u(2, 1) {
			return invalid()
		}
	case 8:
		version, ok := d[1].(uint64)
		if !ii(0) || !ok || version == 0 || version > math.MaxInt64 {
			return invalid()
		}
	}
	return nil
}
func nullableUint(v any) bool { return v == nil || isUint(v) }
func eventIDs(k EventKind, d []any) {
	positions := map[EventKind][]int{1: {0, 1}, 2: {0, 1}, 3: {0}, 4: {1}, 5: {0}, 6: {0, 1, 2, 3}, 7: {0}, 8: {0}}[k]
	for _, i := range positions {
		if b, ok := d[i].([]byte); ok && len(b) == 16 {
			var v id.ID
			copy(v[:], b)
			d[i] = v
		}
	}
	if k == 6 {
		for _, i := range []int{4, 5} {
			if b, ok := d[i].([]byte); ok && len(b) == 16 {
				var v id.ID
				copy(v[:], b)
				d[i] = v
			}
		}
	}
}

type SendOutcome uint64

const (
	ReceiptClosedNotAccepted SendOutcome = iota
	ReceiptAccepted
	ReceiptUnknownAfterRestore
)

type OriginalSendResponse struct {
	Sequence         uint64
	FrankingTag      []byte
	ReceiveTimestamp uint64
	Body             []byte
}

type SendReceipt struct {
	OperationID, AttemptID id.ID
	PredecessorAttemptID   *id.ID
	CommunityID, GroupID   id.ID
	ExpectedGeneration     uint64
	CiphertextHash         []byte
	Class                  uint64
	Outcome                SendOutcome
	Epoch                  uint64
	SendResponse           *OriginalSendResponse
}

func decodeOriginalSendResponse(body []byte) (*OriginalSendResponse, error) {
	n, err := parse(body)
	if err != nil || n.major != 4 || len(n.a) != 3 || n.a[0].major != 0 || n.a[1].major != 2 || len(n.a[1].b) != 32 || n.a[2].major != 0 {
		return nil, invalid()
	}
	if n.a[0].u == 0 || n.a[0].u > math.MaxInt64 || n.a[2].u > math.MaxInt64 {
		return nil, invalid()
	}
	return &OriginalSendResponse{Sequence: n.a[0].u, FrankingTag: append([]byte(nil), n.a[1].b...), ReceiveTimestamp: n.a[2].u, Body: append([]byte(nil), body...)}, nil
}

func EncodeReceipt(r SendReceipt) ([]byte, error) {
	if !isID(r.OperationID) || !isID(r.AttemptID) || !isID(r.CommunityID) || !isID(r.GroupID) || len(r.CiphertextHash) != 32 || r.Class > 1 || r.Outcome > ReceiptUnknownAfterRestore || r.ExpectedGeneration > math.MaxInt64 || r.Epoch > math.MaxInt64 {
		return nil, invalid()
	}
	if r.PredecessorAttemptID != nil && !isID(*r.PredecessorAttemptID) {
		return nil, invalid()
	}
	var predecessor, response any
	if r.PredecessorAttemptID != nil {
		predecessor = *r.PredecessorAttemptID
	}
	if r.Outcome == ReceiptAccepted {
		if r.SendResponse == nil {
			return nil, invalid()
		}
		decoded, err := decodeOriginalSendResponse(r.SendResponse.Body)
		if err != nil || decoded.Sequence != r.SendResponse.Sequence || decoded.ReceiveTimestamp != r.SendResponse.ReceiveTimestamp || !bytes.Equal(decoded.FrankingTag, r.SendResponse.FrankingTag) {
			return nil, invalid()
		}
		response = r.SendResponse.Body
	} else if r.SendResponse != nil {
		return nil, invalid()
	}
	b, er := cborx.Marshal([]any{uint64(1), r.OperationID, r.AttemptID, predecessor, r.CommunityID, r.GroupID, r.ExpectedGeneration, r.CiphertextHash, r.Class, uint64(r.Outcome), r.Epoch, response})
	if er != nil || len(b) > 512 {
		return nil, invalid()
	}
	return b, nil
}
func DecodeReceipt(b []byte) (SendReceipt, error) {
	var z SendReceipt
	if len(b) > 512 {
		return z, ErrLimit
	}
	n, e := parse(b)
	if e != nil || n.major != 4 || len(n.a) != 12 {
		return z, invalid()
	}
	a := n.a
	if a[0].major != 0 || a[0].u != 1 || !isNullableIDNode(a[3]) || a[4].major != 2 || a[5].major != 2 || a[6].major != 0 || a[7].major != 2 || len(a[7].b) != 32 || a[8].major != 0 || a[9].major != 0 || a[10].major != 0 || (!a[11].null && a[11].major != 2) {
		return z, invalid()
	}
	for _, i := range []int{1, 2, 4, 5} {
		if a[i].major != 2 || len(a[i].b) != 16 {
			return z, invalid()
		}
	}
	if a[3].major == 2 && len(a[3].b) != 16 || a[8].u > 1 || a[9].u > uint64(ReceiptUnknownAfterRestore) || a[6].u > math.MaxInt64 || a[10].u > math.MaxInt64 {
		return z, invalid()
	}
	r := SendReceipt{OperationID: nodeID(a[1]), AttemptID: nodeID(a[2]), CommunityID: nodeID(a[4]), GroupID: nodeID(a[5]), ExpectedGeneration: a[6].u, CiphertextHash: append([]byte(nil), a[7].b...), Class: a[8].u, Outcome: SendOutcome(a[9].u), Epoch: a[10].u}
	if a[3].major == 2 {
		v := nodeID(a[3])
		r.PredecessorAttemptID = &v
	}
	if !a[11].null {
		r.SendResponse, e = decodeOriginalSendResponse(a[11].b)
		if e != nil {
			return z, invalid()
		}
	}
	if _, e = EncodeReceipt(r); e != nil {
		return z, invalid()
	}
	return r, nil
}
func ValidateReceiptContext(r SendReceipt, community, group id.ID) error {
	if !isID(community) || !isID(group) || r.CommunityID != community || r.GroupID != group {
		return ErrContext
	}
	return nil
}

type HelperOperation uint64

const (
	HelperOpen HelperOperation = 1 + iota
	HelperBootstrap
	HelperEnrollComplete
	HelperSession
	HelperKeyPackages
	HelperGroupApply
	HelperPrepareSend
	HelperSendReceipt
	HelperCallEpoch
	HelperSignRemoval
	HelperStateGet
	HelperStateTransaction
	HelperCursorAck
	HelperKeyPackagesUploaded
	HelperShutdown
	HelperGroupTransfer
	HelperIngestRemovals
	HelperRefreshRemovalActorHistory
	HelperCallJoinCheck
	HelperCallGroupForget
)

type HelperRequest struct {
	RequestID id.ID
	Operation HelperOperation
	Payload   []any
}
type HelperResponse struct {
	RequestID id.ID
	Status    uint64
	Payload   []any
}

func EncodeHelperRequest(r HelperRequest) ([]byte, error) {
	return encodeHelper(true, r.RequestID, uint64(r.Operation), r.Payload)
}
func EncodeHelperResponse(r HelperResponse) ([]byte, error) {
	return encodeHelper(false, r.RequestID, r.Status, r.Payload)
}
func encodeHelper(req bool, request id.ID, code uint64, payload []any) ([]byte, error) {
	if !isID(request) {
		return nil, invalid()
	}
	if req && (code < 1 || code > 20) || !req && code > 9 {
		return nil, invalid()
	}
	if payload == nil {
		payload = []any{}
	}
	pb, e := cborx.Marshal(payload)
	if e != nil || len(pb) > MaxHelperPayload {
		return nil, invalid()
	}
	b, e := cborx.Marshal([]any{uint64(1), request, code, pb})
	if e != nil || len(b) > MaxHelperBytes {
		return nil, invalid()
	}
	frame := make([]byte, 4+len(b))
	binary.BigEndian.PutUint32(frame, uint32FromInt(len(b)))
	copy(frame[4:], b)
	return frame, nil
}
func EncodeHelperRequestCBOR(r HelperRequest) ([]byte, error) {
	f, e := EncodeHelperRequest(r)
	if e != nil {
		return nil, e
	}
	return f[4:], nil
}
func EncodeHelperResponseCBOR(r HelperResponse) ([]byte, error) {
	f, e := EncodeHelperResponse(r)
	if e != nil {
		return nil, e
	}
	return f[4:], nil
}
func DecodeHelperRequestFrame(frame []byte) (HelperRequest, error) {
	var z HelperRequest
	b, e := helperBody(frame)
	if e != nil {
		return z, e
	}
	n, e := parse(b)
	if e != nil || len(n.a) != 4 {
		return z, invalid()
	}
	a := n.a
	if a[0].major != 0 || a[0].u != 1 || a[1].major != 2 || a[2].major != 0 || a[3].major != 2 {
		return z, invalid()
	}
	if len(a[3].b) > MaxHelperPayload {
		return z, ErrLimit
	}
	payload, err := parse(a[3].b)
	if err != nil || payload.major != 4 {
		return z, invalid()
	}
	r := HelperRequest{RequestID: nodeID(a[1]), Operation: HelperOperation(a[2].u), Payload: nodesToAny(payload.a)}
	if !isID(r.RequestID) || r.Operation < 1 || r.Operation > 20 || len(b) > MaxHelperBytes || len(frame)-4 > MaxHelperBytes {
		return z, invalid()
	}
	return r, nil
}
func DecodeHelperResponseFrame(frame []byte) (HelperResponse, error) {
	var z HelperResponse
	b, e := helperBody(frame)
	if e != nil {
		return z, e
	}
	n, e := parse(b)
	if e != nil || len(n.a) != 4 {
		return z, invalid()
	}
	a := n.a
	if a[0].major != 0 || a[0].u != 1 || a[1].major != 2 || a[2].major != 0 || a[3].major != 2 {
		return z, invalid()
	}
	if len(a[3].b) > MaxHelperPayload {
		return z, ErrLimit
	}
	payload, err := parse(a[3].b)
	if err != nil || payload.major != 4 {
		return z, invalid()
	}
	r := HelperResponse{RequestID: nodeID(a[1]), Status: a[2].u, Payload: nodesToAny(payload.a)}
	if !isID(r.RequestID) || r.Status > 9 || len(frame)-4 > MaxHelperBytes {
		return z, invalid()
	}
	return r, nil
}
func DecodeHelperRequestCBOR(b []byte) (HelperRequest, error) {
	if len(b) > MaxHelperBytes {
		return HelperRequest{}, ErrLimit
	}
	f := make([]byte, 4+len(b))
	binary.BigEndian.PutUint32(f, uint32FromInt(len(b)))
	copy(f[4:], b)
	return DecodeHelperRequestFrame(f)
}
func DecodeHelperResponseCBOR(b []byte) (HelperResponse, error) {
	if len(b) > MaxHelperBytes {
		return HelperResponse{}, ErrLimit
	}
	f := make([]byte, 4+len(b))
	binary.BigEndian.PutUint32(f, uint32FromInt(len(b)))
	copy(f[4:], b)
	return DecodeHelperResponseFrame(f)
}
func helperBody(b []byte) ([]byte, error) {
	if len(b) < 4 {
		return nil, invalid()
	}
	n := binary.BigEndian.Uint32(b[:4])
	if n == 0 || n > MaxHelperBytes || uint64(n) != intToUint64(len(b)-4) {
		return nil, ErrLimit
	}
	return b[4:], nil
}

// Minimal typed parser: cborx has already rejected noncanonical input. Keeping
// each node's major type prevents fxamacker's []byte convenience from treating
// a CBOR array as a byte string.
type node struct {
	major byte
	u     uint64
	b     []byte
	s     string
	a     []node
	null  bool
}

func parse(b []byte) (node, error) {
	if e := cborx.CheckDeterministic(b); e != nil {
		return node{}, e
	}
	n, off, e := readNode(b, 0)
	if e != nil || off != len(b) {
		return node{}, invalid()
	}
	return n, nil
}
func uint32FromInt(value int) uint32 {
	if value < 0 || uint64(value) > math.MaxUint32 {
		return 0
	}
	return uint32(value)
}

func intToUint64(value int) uint64 {
	if value < 0 {
		return 0
	}
	return uint64(value)
}

func uint64ToInt(value uint64) int {
	if value > uint64(math.MaxInt) {
		return 0
	}
	return int(value)
}

func readNode(b []byte, depth int) (node, int, error) {
	var n node
	if depth > cborx.MaxNesting || len(b) == 0 {
		return n, 0, invalid()
	}
	m, ai := b[0]>>5, b[0]&31
	n.major = m
	if m == 7 && ai == 22 {
		n.null = true
		return n, 1, nil
	}
	v, h, e := readHead(b)
	if e != nil {
		return n, 0, e
	}
	switch m {
	case 0:
		n.u = v
		return n, h, nil
	case 2, 3:
		if v > intToUint64(len(b)-h) {
			return n, 0, invalid()
		}
		n.b = append([]byte(nil), b[h:h+uint64ToInt(v)]...)
		if m == 3 {
			n.s = string(n.b)
		}
		return n, h + uint64ToInt(v), nil
	case 4:
		if v > uint64(len(b)) {
			return n, 0, invalid()
		}
		pos := h
		n.a = make([]node, 0, uint64ToInt(v))
		for i := uint64(0); i < v; i++ {
			child, k, er := readNode(b[pos:], depth+1)
			if er != nil {
				return n, 0, er
			}
			n.a = append(n.a, child)
			pos += k
		}
		return n, pos, nil
	default:
		return n, 0, invalid()
	}
}
func readHead(b []byte) (uint64, int, error) {
	ai := b[0] & 31
	if ai < 24 {
		return uint64(ai), 1, nil
	}
	switch ai {
	case 24:
		return uint64(b[1]), 2, nil
	case 25:
		return uint64(binary.BigEndian.Uint16(b[1:3])), 3, nil
	case 26:
		return uint64(binary.BigEndian.Uint32(b[1:5])), 5, nil
	case 27:
		return binary.BigEndian.Uint64(b[1:9]), 9, nil
	default:
		return 0, 0, invalid()
	}
}
func nodeID(n node) id.ID {
	var v id.ID
	if n.major == 2 && len(n.b) == 16 {
		copy(v[:], n.b)
	}
	return v
}
func nodesToAny(ns []node) []any {
	out := make([]any, len(ns))
	for i, n := range ns {
		switch n.major {
		case 0:
			out[i] = n.u
		case 2:
			out[i] = append([]byte(nil), n.b...)
		case 3:
			out[i] = n.s
		case 4:
			out[i] = nodesToAny(n.a)
		case 7:
			out[i] = nil
		}
	}
	return out
}
func initIDs(a []any, positions ...int) {
	for _, i := range positions {
		if i >= 0 && i < len(a) {
			if b, ok := a[i].([]byte); ok && len(b) == 16 {
				var v id.ID
				copy(v[:], b)
				a[i] = v
			}
		}
	}
}
