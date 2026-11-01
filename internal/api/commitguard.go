package api

import (
	"bufio"
	"encoding/binary"
	"io"
	"net/http"
	"time"

	"github.com/jonasthim/dilla/internal/id"
	"github.com/jonasthim/dilla/internal/server"
)

// The bounds on POST /v1/groups/{id}/commit that deviation B37's 16 MiB body needs (final review):
// who may upload and for which epoch is answered before the body is read, a commit carries at most
// maxWelcomesPerCommit Welcomes, a device has one commit upload in flight at a time, and the body
// must arrive within commitReadTimeout.
const (
	// maxWelcomesPerCommit is protocol/01 § Joining's MAX_ADDS: at most 256 Adds are batched into
	// one commit, and each Welcome is addressed to one added device.
	maxWelcomesPerCommit = 256
	// commitReadTimeout bounds how long the body of one commit upload may take to arrive. 16 MiB
	// in a minute is about 2.2 Mbit/s; a client slower than that is holding a group lock's worth
	// of the instance's memory open, not uploading.
	commitReadTimeout = 60 * time.Second
	// commitBusyRetry is the retry_after_ms a device's second concurrent commit is told.
	commitBusyRetry = 1000
)

// withReadDeadline gives one route's body a read deadline of its own. The server-wide
// ordinary body deadline is installed before routing; this extends it for a larger commit.
// The production boundary fails closed if deadlines are unsupported. Direct handler tests may
// use a recorder without deadline support.
func withReadDeadline(d time.Duration, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(d))
		next.ServeHTTP(w, r)
	})
}

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
