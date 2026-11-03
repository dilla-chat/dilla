package dillad

import (
	"context"

	"github.com/dilla-chat/dilla/internal/gateway"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/ops"
)

// ResetFactors is ops.ResetFactors with this server's live seams (web3.6 S3). After the commit,
// each of the person's devices has its gateway sockets closed 4003 (the code a re-identify would
// meet, not resumable, so the next connect is refused E_FACTOR_REQUIRED) and is queued to leave
// every live call, because a LiveKit token minted before the reset would otherwise stay good at
// /rtc until it expires. Sessions are kept (D5). `dillad admin` is another process and cannot
// reach this method: there the gateway's liveness sweep does the same on its next tick.
func (s *Server) ResetFactors(ctx context.Context, uid id.ID) error {
	return ops.ResetFactors(ctx, s.o.Repo, func(device id.ID) {
		s.gw.CloseDevice(device, gateway.CloseUnauthenticated, "second factor required")
		s.cutDevice(context.WithoutCancel(ctx), device)
	}, uid, s.o.Clock.Now().Unix())
}
