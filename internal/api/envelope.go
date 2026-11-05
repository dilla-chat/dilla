package api

import (
	"regexp"

	"github.com/fxamacker/cbor/v2"

	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/server"
)

// Envelope types, protocol/04 "Envelope".
const (
	EnvMessage        uint8 = 0
	EnvEdit           uint8 = 1
	EnvDelete         uint8 = 2
	EnvReactionAdd    uint8 = 3
	EnvReactionRemove uint8 = 4
	EnvPin            uint8 = 5
	EnvUnpin          uint8 = 6
)

// The limits of protocol/04 as amended by interfaces.md §2.8. Every one of them
// has a reject vector in protocol/vectors/envelope.json.
const (
	maxBodyMessage     = 4000
	maxBodyReaction    = 32
	maxAttachments     = 10
	maxPreviews        = 2
	maxMimeBytes       = 255
	maxNameBytes       = 255
	maxThumbBytes      = 8192
	maxPreviewURL      = 2048
	maxPreviewTitle    = 256
	maxPreviewDesc     = 1024
	maxPreviewImage    = 16384
	frankingKeyBytes   = 32
	attachmentElements = 9
	previewElements    = 4
	blobIDBytes        = 32
	attachmentKeyBytes = 32
	attachmentNonce    = 12
)

// cborNull is the one-byte CBOR null (major 7, simple value 22).
const cborNull = 0xf6

// Attachment is protocol/04's 9-element fixed-position array, in element order:
// blob_id, key, nonce, size, mime, w, h, thumb, name. W, H and Thumb are nil for a
// CBOR null.
//
// The `cbor:",toarray"` marker keeps the type encodable as protocol/04's array
// (internal/cborx forbids maps). ParseEnvelope does NOT decode into it through
// fxamacker/cbor, though: a toarray struct silently zero-fills a SHORT array,
// and a Go []byte accepts a CBOR array of small uints, so every field is read
// from its own element with its major type checked (decodeAttachment).
type Attachment struct {
	_      struct{} `cbor:",toarray"`
	BlobID []byte
	Key    []byte
	Nonce  []byte
	Size   uint64
	Mime   string
	W, H   *uint64
	Thumb  []byte
	Name   string
}

// Preview is protocol/04's 4-element array: url, title, description, image.
// Image is nil for a CBOR null.
type Preview struct {
	_                       struct{} `cbor:",toarray"`
	URL, Title, Description string
	Image                   []byte
}

// Envelope is one validated protocol/04 envelope. MentionCount is computed by
// ParseEnvelope, not carried on the wire.
type Envelope struct {
	V            uint64
	MsgID        id.ID
	Type         uint8
	ThreadID     *id.ID
	ReplyTo      *id.ID
	Body         string
	Attachments  []Attachment
	Previews     []Preview
	KF           []byte
	MentionCount int
}

// envErr builds a refusal carrying one of protocol/04's three receiver codes.
// The three rows are in protocol/02's § Errors table and the constants in
// internal/server (P2-D30); all three are 400.
//
//	400 E_ENVELOPE_SHAPE  the envelope is not a 9-element deterministic CBOR array,
//	                      or a field has the wrong type or length,
//	                      or a type 1..6 envelope has no reply_to or carries files
//	400 E_ENVELOPE_TYPE  the type byte names no envelope type this version knows
//	400 E_ENVELOPE_LIMIT  a field or list exceeds a protocol/04 § Limits bound
func envErr(code server.Code, format string, a ...any) error {
	return server.Errorf(code, format, a...)
}

func shapeErr(format string, a ...any) error { return envErr(server.CodeEnvelopeShape, format, a...) }
func limitErr(format string, a ...any) error { return envErr(server.CodeEnvelopeLimit, format, a...) }

// ParseEnvelope decodes and validates one envelope. It is the server side of
// protocol/04's receiver rules: a readable channel's instance is a receiver, so
// it refuses exactly what a client refuses and stores nothing partial.
func ParseEnvelope(b []byte) (Envelope, error) {
	var raw []cbor.RawMessage
	if err := cborx.ExpectMajor(b, cborx.MajorArray); err != nil {
		return Envelope{}, shapeErr("envelope must be a CBOR array")
	}
	if err := cborx.Unmarshal(b, &raw); err != nil {
		return Envelope{}, shapeErr("envelope is not deterministic CBOR: %v", err)
	}
	if len(raw) != envelopeElements {
		return Envelope{}, shapeErr("envelope has %d elements, want %d", len(raw), envelopeElements)
	}
	var e Envelope
	var err error
	if e.V, err = decodeUint(raw[0]); err != nil || e.V != 1 {
		return Envelope{}, shapeErr("v must be 1")
	}
	if err := decodeID(raw[1], &e.MsgID); err != nil {
		return Envelope{}, shapeErr("msg_id must be 16 bytes")
	}
	typ, err := decodeUint(raw[2])
	if err != nil {
		return Envelope{}, shapeErr("type must be a uint")
	}
	if typ > uint64(EnvUnpin) {
		return Envelope{}, envErr(server.CodeEnvelopeType, "unknown type %d", typ)
	}
	e.Type = uint8(typ)
	if e.ThreadID, err = decodeNullableID(raw[3]); err != nil {
		return Envelope{}, shapeErr("thread_id must be 16 bytes or null")
	}
	if e.ReplyTo, err = decodeNullableID(raw[4]); err != nil {
		return Envelope{}, shapeErr("reply_to must be 16 bytes or null")
	}
	if e.Body, err = decodeText(raw[5]); err != nil {
		return Envelope{}, shapeErr("body must be a text string")
	}
	switch e.Type {
	case EnvMessage, EnvEdit:
		if len(e.Body) > maxBodyMessage {
			return Envelope{}, limitErr("body is %d bytes, at most %d", len(e.Body), maxBodyMessage)
		}
	case EnvReactionAdd, EnvReactionRemove:
		if len(e.Body) > maxBodyReaction {
			return Envelope{}, limitErr("a reaction body is %d bytes, at most %d", len(e.Body), maxBodyReaction)
		}
	default: // delete, pin, unpin
		if len(e.Body) != 0 {
			return Envelope{}, limitErr("type %d carries no body", e.Type)
		}
	}
	if e.Attachments, err = decodeAttachments(raw[6]); err != nil {
		return Envelope{}, err
	}
	if e.Previews, err = decodePreviews(raw[7]); err != nil {
		return Envelope{}, err
	}
	if e.KF, err = decodeBytes(raw[8]); err != nil || len(e.KF) != frankingKeyBytes {
		return Envelope{}, shapeErr("k_f must be %d bytes", frankingKeyBytes)
	}
	if e.Type != EnvMessage {
		// protocol/04 § Envelope (web-2b task 1): a fold names its target and carries no files.
		if e.ReplyTo == nil {
			return Envelope{}, shapeErr("type %d names its target in reply_to", e.Type)
		}
		if len(e.Attachments) > 0 || len(e.Previews) > 0 {
			return Envelope{}, shapeErr("type %d carries no attachments or previews", e.Type)
		}
	}
	if e.Type == EnvMessage || e.Type == EnvEdit {
		// Only a message's or an edit's body is rendered, so only there is a
		// <@…> a mention; a reaction's "emoji" pings no one.
		e.MentionCount = countMentions(e.Body)
	}
	return e, nil
}

// decodeAttachments reads element 6. The list's length is bounded before any
// member is decoded, so an abusive list costs one header read.
func decodeAttachments(raw cbor.RawMessage) ([]Attachment, error) {
	items, err := decodeArray(raw)
	if err != nil {
		return nil, shapeErr("attachments must be an array")
	}
	if len(items) > maxAttachments {
		return nil, limitErr("%d attachments, at most %d", len(items), maxAttachments)
	}
	out := make([]Attachment, 0, len(items))
	for i, it := range items {
		a, err := decodeAttachment(it)
		if err != nil {
			return nil, shapeErr("attachment %d: %v", i, err)
		}
		switch {
		case len(a.Mime) > maxMimeBytes:
			return nil, limitErr("attachment %d: mime is %d bytes, at most %d", i, len(a.Mime), maxMimeBytes)
		case len(a.Name) > maxNameBytes:
			return nil, limitErr("attachment %d: name is %d bytes, at most %d", i, len(a.Name), maxNameBytes)
		case len(a.Thumb) > maxThumbBytes:
			return nil, limitErr("attachment %d: thumb is %d bytes, at most %d", i, len(a.Thumb), maxThumbBytes)
		}
		out = append(out, a)
	}
	return out, nil
}

// fieldErr is a shape detail; its caller wraps it in E_ENVELOPE_SHAPE.
type fieldErr string

func (f fieldErr) Error() string { return string(f) }

func decodeAttachment(raw cbor.RawMessage) (Attachment, error) {
	f, err := decodeArray(raw)
	if err != nil {
		return Attachment{}, fieldErr("must be an array")
	}
	if len(f) != attachmentElements {
		return Attachment{}, fieldErr("has the wrong number of elements")
	}
	var a Attachment
	if a.BlobID, err = decodeBytes(f[0]); err != nil || len(a.BlobID) != blobIDBytes {
		return Attachment{}, fieldErr("blob_id must be 32 bytes")
	}
	if a.Key, err = decodeBytes(f[1]); err != nil || len(a.Key) != attachmentKeyBytes {
		return Attachment{}, fieldErr("key must be 32 bytes")
	}
	if a.Nonce, err = decodeBytes(f[2]); err != nil || len(a.Nonce) != attachmentNonce {
		return Attachment{}, fieldErr("nonce must be 12 bytes")
	}
	if a.Size, err = decodeUint(f[3]); err != nil {
		return Attachment{}, fieldErr("size must be a uint")
	}
	if a.Mime, err = decodeText(f[4]); err != nil {
		return Attachment{}, fieldErr("mime must be a text string")
	}
	if a.W, err = decodeNullableUint(f[5]); err != nil {
		return Attachment{}, fieldErr("w must be a uint or null")
	}
	if a.H, err = decodeNullableUint(f[6]); err != nil {
		return Attachment{}, fieldErr("h must be a uint or null")
	}
	if a.Thumb, err = decodeNullableBytes(f[7]); err != nil {
		return Attachment{}, fieldErr("thumb must be a byte string or null")
	}
	if a.Name, err = decodeText(f[8]); err != nil {
		return Attachment{}, fieldErr("name must be a text string")
	}
	return a, nil
}

// decodePreviews reads element 7, bounding the list before its members.
func decodePreviews(raw cbor.RawMessage) ([]Preview, error) {
	items, err := decodeArray(raw)
	if err != nil {
		return nil, shapeErr("previews must be an array")
	}
	if len(items) > maxPreviews {
		return nil, limitErr("%d previews, at most %d", len(items), maxPreviews)
	}
	out := make([]Preview, 0, len(items))
	for i, it := range items {
		f, err := decodeArray(it)
		if err != nil || len(f) != previewElements {
			return nil, shapeErr("preview %d must be a %d-element array", i, previewElements)
		}
		var p Preview
		var e1, e2, e3, e4 error
		p.URL, e1 = decodeText(f[0])
		p.Title, e2 = decodeText(f[1])
		p.Description, e3 = decodeText(f[2])
		p.Image, e4 = decodeNullableBytes(f[3])
		if e1 != nil || e2 != nil || e3 != nil || e4 != nil {
			return nil, shapeErr("preview %d: url, title and description are text strings, image a byte string or null", i)
		}
		switch {
		case len(p.URL) > maxPreviewURL:
			return nil, limitErr("preview %d: url is %d bytes, at most %d", i, len(p.URL), maxPreviewURL)
		case len(p.Title) > maxPreviewTitle:
			return nil, limitErr("preview %d: title is %d bytes, at most %d", i, len(p.Title), maxPreviewTitle)
		case len(p.Description) > maxPreviewDesc:
			return nil, limitErr("preview %d: description is %d bytes, at most %d", i, len(p.Description), maxPreviewDesc)
		case len(p.Image) > maxPreviewImage:
			return nil, limitErr("preview %d: image is %d bytes, at most %d", i, len(p.Image), maxPreviewImage)
		}
		out = append(out, p)
	}
	return out, nil
}

// The decoders below check each element's CBOR major type before decoding it.
// The raw elements are sub-slices of an input cborx.Unmarshal already proved
// deterministic, so each is one well-formed item.

func isNull(raw cbor.RawMessage) bool { return len(raw) == 1 && raw[0] == cborNull }

func decodeArray(raw cbor.RawMessage) ([]cbor.RawMessage, error) {
	if err := cborx.ExpectMajor(raw, cborx.MajorArray); err != nil {
		return nil, err
	}
	var out []cbor.RawMessage
	if err := cborx.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func decodeUint(raw cbor.RawMessage) (uint64, error) {
	if err := cborx.ExpectMajor(raw, cborx.MajorUint); err != nil {
		return 0, err
	}
	var v uint64
	return v, cborx.Unmarshal(raw, &v)
}

func decodeNullableUint(raw cbor.RawMessage) (*uint64, error) {
	if isNull(raw) {
		return nil, nil
	}
	v, err := decodeUint(raw)
	if err != nil {
		return nil, err
	}
	return &v, nil
}

func decodeText(raw cbor.RawMessage) (string, error) {
	if err := cborx.ExpectMajor(raw, cborx.MajorText); err != nil {
		return "", err
	}
	var s string
	return s, cborx.Unmarshal(raw, &s)
}

func decodeBytes(raw cbor.RawMessage) ([]byte, error) {
	if err := cborx.ExpectMajor(raw, cborx.MajorBytes); err != nil {
		return nil, err
	}
	var b []byte
	if err := cborx.Unmarshal(raw, &b); err != nil {
		return nil, err
	}
	if b == nil {
		b = []byte{}
	}
	return b, nil
}

func decodeNullableBytes(raw cbor.RawMessage) ([]byte, error) {
	if isNull(raw) {
		return nil, nil
	}
	return decodeBytes(raw)
}

// decodeID reads a 16-byte bstr; id.ID's own UnmarshalCBOR refuses any other
// length, and the major check refuses a null it would never be handed.
func decodeID(raw cbor.RawMessage, out *id.ID) error {
	if err := cborx.ExpectMajor(raw, cborx.MajorBytes); err != nil {
		return err
	}
	return cborx.Unmarshal(raw, out)
}

func decodeNullableID(raw cbor.RawMessage) (*id.ID, error) {
	if isNull(raw) {
		return nil, nil
	}
	var v id.ID
	if err := decodeID(raw, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

// mentionRE matches the mention spellings a readable body may carry: <@hex32>
// for a user or role, <@everyone> and <@here>. It is counted at write time
// (gap-69 §7) so AutoMod's mention-spam query is one dialect-neutral column
// rather than a JSON extraction that differs per engine.
var mentionRE = regexp.MustCompile(`<@(everyone|here|[0-9a-f]{32})>`)

// countMentions returns the number of DISTINCT mention targets, so pasting the
// same handle ten times is one mention, not ten.
func countMentions(body string) int {
	seen := map[string]bool{}
	for _, m := range mentionRE.FindAllStringSubmatch(body, -1) {
		seen[m[1]] = true
	}
	return len(seen)
}
