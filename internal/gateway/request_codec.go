package gateway

import (
	"encoding/binary"
	"errors"
)

// The request and response frames are written and read by hand rather than through the
// reflection codec: they carry every socket request, and ruling 2 holds a socket request to the
// cost of its HTTP form. The bytes are exactly cborx's deterministic encoding (request_test.go
// compares the two, and the frame vectors pin both), and the reader refuses everything the
// deterministic subset refuses for these two shapes.

// appendHead appends the minimal CBOR head of major type major with argument n.
func appendHead(b []byte, major byte, n uint64) []byte {
	m := major << 5
	switch {
	case n < 24:
		return append(b, m|byte(n))
	case n <= 0xff:
		return append(b, m|24, byte(n))
	case n <= 0xffff:
		return binary.BigEndian.AppendUint16(append(b, m|25), uint16(n))
	case n <= 0xffffffff:
		return binary.BigEndian.AppendUint32(append(b, m|26), uint32(n))
	default:
		return binary.BigEndian.AppendUint64(append(b, m|27), n)
	}
}

const cborNull = 0xf6

func appendBytes(b []byte, major byte, v []byte) []byte {
	return append(appendHead(b, major, uint64(len(v))), v...)
}

// appendRequestFrame is EncodeRequestFrame's bytes: [59, 0, null, [id, method, route, body, idem]].
func appendRequestFrame(b []byte, r SocketRequest) []byte {
	b = appendHead(b, 4, 4)
	b = appendHead(b, 0, uint64(OpRequest))
	b = append(b, 0x00, cborNull)
	b = appendHead(b, 4, 5)
	b = appendHead(b, 0, r.ID)
	b = appendBytes(b, 3, []byte(r.Method))
	b = appendBytes(b, 3, []byte(r.Route))
	b = appendBytes(b, 2, r.Body)
	if r.Idem == nil {
		return append(b, cborNull)
	}
	return appendBytes(b, 2, r.Idem)
}

// responseFrame is the whole op 60 frame [60, 0, null, [id, status, body]].
func responseFrame(r SocketResponse) ([]byte, error) {
	if r.Status < 100 || r.Status > 599 {
		return nil, errors.New("gateway: response status out of range")
	}
	b := make([]byte, 0, len(r.Body)+32)
	b = appendHead(b, 4, 4)
	b = appendHead(b, 0, uint64(OpResponse))
	b = append(b, 0x00, cborNull)
	b = appendHead(b, 4, 3)
	b = appendHead(b, 0, r.ID)
	b = appendHead(b, 0, uint64(r.Status))
	return appendBytes(b, 2, r.Body), nil
}

var errNotDeterministic = errors.New("gateway: not a deterministic response frame")

// readHead reads one minimal CBOR head at b[off:]; an indefinite length, a reserved additional
// value or a non-minimal argument is refused, as cborx refuses them.
func readHead(b []byte, off int) (major byte, arg uint64, next int, err error) {
	if off >= len(b) {
		return 0, 0, 0, errNotDeterministic
	}
	major, info := b[off]>>5, b[off]&0x1f
	off++
	switch {
	case info < 24:
		return major, uint64(info), off, nil
	case info == 24 && off+1 <= len(b):
		arg = uint64(b[off])
		next = off + 1
		if arg < 24 {
			return 0, 0, 0, errNotDeterministic
		}
	case info == 25 && off+2 <= len(b):
		arg = uint64(binary.BigEndian.Uint16(b[off:]))
		next = off + 2
		if arg <= 0xff {
			return 0, 0, 0, errNotDeterministic
		}
	case info == 26 && off+4 <= len(b):
		arg = uint64(binary.BigEndian.Uint32(b[off:]))
		next = off + 4
		if arg <= 0xffff {
			return 0, 0, 0, errNotDeterministic
		}
	case info == 27 && off+8 <= len(b):
		arg = binary.BigEndian.Uint64(b[off:])
		next = off + 8
		if arg <= 0xffffffff {
			return 0, 0, 0, errNotDeterministic
		}
	default:
		return 0, 0, 0, errNotDeterministic
	}
	return major, arg, next, nil
}

// frameOp reads the op of a frame from its head alone, without decoding the rest.
func frameOp(b []byte) (Op, bool) {
	major, n, off, err := readHead(b, 0)
	if err != nil || major != 4 || n != 4 {
		return 0, false
	}
	major, op, _, err := readHead(b, off)
	if err != nil || major != 0 || op > 255 {
		return 0, false
	}
	return Op(op), true
}

// FrameOp is frameOp for clients and tests.
func FrameOp(b []byte) (Op, bool) { return frameOp(b) }

// decodeResponseFrame reads [60, n, null, [id, status, body]] and nothing after it.
func decodeResponseFrame(b []byte) (SocketResponse, error) {
	want := func(off int, major byte) (uint64, int, error) {
		m, arg, next, err := readHead(b, off)
		if err != nil || m != major {
			return 0, 0, errNotDeterministic
		}
		return arg, next, nil
	}
	n, off, err := want(0, 4)
	if err != nil || n != 4 {
		return SocketResponse{}, errNotDeterministic
	}
	if op, next, err := want(off, 0); err != nil || op != uint64(OpResponse) {
		return SocketResponse{}, errNotDeterministic
	} else {
		off = next
	}
	if _, off, err = want(off, 0); err != nil {
		return SocketResponse{}, err
	}
	if off >= len(b) || b[off] != cborNull {
		return SocketResponse{}, errNotDeterministic
	}
	off++
	if n, off, err = want(off, 4); err != nil || n != 3 {
		return SocketResponse{}, errNotDeterministic
	}
	reqID, off, err := want(off, 0)
	if err != nil {
		return SocketResponse{}, err
	}
	status, off, err := want(off, 0)
	if err != nil || status < 100 || status > 599 {
		return SocketResponse{}, errNotDeterministic
	}
	size, off, err := want(off, 2)
	if err != nil || uint64(len(b)-off) != size {
		return SocketResponse{}, errNotDeterministic
	}
	return SocketResponse{ID: reqID, Status: int(status), Body: append([]byte(nil), b[off:]...)}, nil
}
