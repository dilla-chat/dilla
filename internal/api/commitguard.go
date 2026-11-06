package api

import (
	"bufio"
	"encoding/binary"
	"io"
	"net/http"

	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/server"
)

// The bounds on POST /v1/groups/{id}/commit that deviation B37's 16 MiB body needs (final review):
// who may upload and for which epoch is answered before the body is read, a commit carries at most
// maxWelcomesPerCommit Welcomes, a device has one commit upload in flight at a time. (The route is a gateway socket
// request, so its body arrives whole in one frame; there is no HTTP read deadline to extend.)
const (
	// maxWelcomesPerCommit is protocol/01 § Joining's MAX_ADDS: at most 256 Adds are batched into
	// one commit, and each Welcome is addressed to one added device.
	maxWelcomesPerCommit = 256
	// commitBusyRetry is the retry_after_ms a device's second concurrent commit is told.
	commitBusyRetry = 1000
)

// beginCommit reserves the device's one in-flight commit upload, or refuses with E_RATE_LIMITED.
// The returned function releases it.
func (h *Groups) beginCommit(device id.ID) (func(), error) {
	if _, busy := h.commitsInFlight.LoadOrStore(device, struct{}{}); busy {
		return nil, server.RateLimited(commitBusyRetry)
	}
	return func() { h.commitsInFlight.Delete(device) }, nil
}

// peekCommitEpoch reads element 0 of the commit body — `[epoch, commit, group_info, welcomes,
// ratchet_tree]`, deterministic CBOR, so an array head 0x85 and then the epoch's uint head — from
// the head of the stream WITHOUT consuming it: r.Body is replaced by a reader that replays the
// peeked bytes. ok is false when the head is not that shape, and the handler then refuses the body unread.
func peekCommitEpoch(r *http.Request) (epoch uint64, ok bool) {
	br := bufio.NewReaderSize(r.Body, 16)
	head, _ := br.Peek(10)
	r.Body = struct {
		io.Reader
		io.Closer
	}{br, r.Body}
	if len(head) < 2 || head[0] != 0x85 || head[1]>>5 != 0 {
		return 0, false
	}
	info := head[1] & 0x1f
	rest := head[2:]
	switch {
	case info < 24:
		return uint64(info), true
	case info == 24 && len(rest) >= 1:
		return uint64(rest[0]), true
	case info == 25 && len(rest) >= 2:
		return uint64(binary.BigEndian.Uint16(rest)), true
	case info == 26 && len(rest) >= 4:
		return uint64(binary.BigEndian.Uint32(rest)), true
	case info == 27 && len(rest) >= 8:
		return binary.BigEndian.Uint64(rest), true
	default:
		return 0, false
	}
}
