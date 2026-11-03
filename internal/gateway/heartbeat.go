package gateway

import (
	"context"
	"errors"
	"math/rand/v2"
	"time"

	"github.com/fxamacker/cbor/v2"

	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/store"
)

// heartbeatPolicy is the liveness contract of protocol/02: the client beats every
// gateway.heartbeat_interval (30 s), the instance closes 4009 when a beat is later than
// interval*2 + 5 s, and the instance's own ping runs on a 20 s period with a 10 s context.
type heartbeatPolicy struct {
	interval time.Duration
	grace    time.Duration
	ping     time.Duration
	pingCtx  time.Duration
}

func newHeartbeatPolicy(interval time.Duration) heartbeatPolicy {
	return heartbeatPolicy{
		interval: interval,
		grace:    interval*2 + 5*time.Second,
		ping:     20 * time.Second,
		pingCtx:  10 * time.Second,
	}
}

// firstDelay jitters the first beat into [0, interval) so 1,500 clients reconnecting after a
// restart do not synchronise onto one second.
func (p heartbeatPolicy) firstDelay() time.Duration {
	return time.Duration(rand.Float64() * float64(p.interval)) //nolint:gosec // G404: start-up jitter spreads reconnects; it is not a secret
}

// rebaseDeadline rewrites element 2 (deadline_ms) of an mls.commit_needed payload, subtracting the
// time the frame spent in the queue. A deadline that has already passed becomes 0, which the
// client reads as "commit now".
func rebaseDeadline(p cbor.RawMessage, waited time.Duration) (cbor.RawMessage, error) {
	var elems []cbor.RawMessage
	if err := cborx.Unmarshal(p, &elems); err != nil {
		return nil, err
	}
	if len(elems) != 4 {
		return nil, frameErr("E_FRAME_SHAPE", "commit_needed payload", CloseDecode)
	}
	deadline, err := rawUint(elems[2])
	if err != nil {
		return nil, err
	}
	waitedMS := uint64(waited / time.Millisecond) //nolint:gosec // G115: a non-negative duration in milliseconds
	if waitedMS >= deadline {
		deadline = 0
	} else {
		deadline -= waitedMS
	}
	epoch, err := rawUint(elems[0])
	if err != nil {
		return nil, err
	}
	round, err := rawUint(elems[3])
	if err != nil {
		return nil, err
	}
	var refs [][]byte
	if err := cborx.Unmarshal(elems[1], &refs); err != nil {
		return nil, err
	}
	return CommitNeededPayload(epoch, refs, deadline, round)
}

// sessionReader is the second optional store method the gateway calls (deviceToucher is the
// first): store.Repository's GetSessionByHash. It is not part of Store, so the narrow interface -
// and every double that satisfies it - is unchanged, and a store without it skips the check.
type sessionReader interface {
	GetSessionByHash(ctx context.Context, tokenHash []byte, now int64) (store.SessionRow, error)
}

type botSessionValidator interface {
	ValidateSessionHash(ctx context.Context, tokenHash []byte) error
}

// sweepSessions is the other half of protocol/02 §2.2 point 6. `dillad admin` is a separate process
// from `dillad serve`: disabling a user or revoking a device there deletes the session rows and
// cannot close a socket, because it holds no handle on this registry. What makes the socket end is
// this check, made once per ready connection on every liveness tick (one query by the unique
// token-hash index, on a tick that already walks every connection for the heartbeat deadline):
// a connection whose session no longer resolves - deleted by an operator, pruned, or past its hard
// expiry - is closed 4004 session_revoked, which is not resumable, so the token it holds dies with
// the socket. A live socket therefore outlives its revocation by at most one heartbeat interval,
// never indefinitely.
//
// A store error is not a revocation. A database that cannot answer must not disconnect every client
// at once, so the connection stays and the next tick asks again.
func (g *Gateway) sweepSessions(ctx context.Context) {
	sessions, ok := g.opts.Store.(sessionReader)
	if !ok {
		return
	}
	now := g.opts.Clock.Now().Unix()
	for _, c := range g.reg.all() {
		c.mu.Lock()
		ready := c.state == stateReady
		hash := c.tokenHash
		isBot := c.isBot
		c.mu.Unlock()
		if !ready || len(hash) == 0 {
			continue
		}
		row, err := sessions.GetSessionByHash(ctx, hash, now)
		if err == nil && isBot {
			validator, ok := g.opts.Auth.(botSessionValidator)
			if !ok {
				err = store.ErrNotFound
			} else {
				err = validator.ValidateSessionHash(ctx, hash)
			}
		}
		if err == nil && row.FactorOwed {
			// The account owes a second factor now (the operator's reset keeps the session, web3.6
			// D5): the socket ends 4003, the code a re-identify would meet, and is not resumable.
			g.closeConn(c, CloseUnauthenticated, "second factor required")
			g.suspend(c)
			if g.opts.OnFactorOwed != nil {
				g.opts.OnFactorOwed(c.deviceID)
			}
			continue
		}
		if !errors.Is(err, store.ErrNotFound) {
			if err != nil && g.opts.Log != nil {
				g.opts.Log.Warn("reading a connection's session failed; leaving it open",
					"device", c.deviceID.String()[:8], "err", err)
			}
			continue
		}
		// 4004 is not resumable, so closeConn marks the connection before suspend looks.
		g.closeConn(c, CloseSessionRevoked, "session ended")
		g.suspend(c)
	}
}
