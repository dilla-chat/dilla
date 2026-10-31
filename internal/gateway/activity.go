package gateway

import (
	"context"
	"slices"
	"time"

	"github.com/jonasthim/dilla/internal/auth"
	"github.com/jonasthim/dilla/internal/id"
	"github.com/jonasthim/dilla/internal/server"
)

const activityInterval = 5 * time.Second
const activityBudgetCap = 8192

type hashSessionResolver interface {
	ResolveHash(context.Context, []byte) (auth.Session, error)
}

// currentSignalSession revalidates the connection-derived credential, never a frame identity.
// Fail closed when an in-process integration does not implement current-session resolution.
func (g *Gateway) currentSignalSession(ctx context.Context, c *conn) (auth.Session, error) {
	c.mu.Lock()
	live := c.state == stateReady
	hash := slices.Clone(c.tokenHash)
	bot := c.isBot
	scope := c.scope
	c.mu.Unlock()
	if !live || bot || scope != auth.ScopeEnrolled || !slices.Contains(g.reg.connsOfDevice(c.deviceID), c) {
		return auth.Session{}, errNotEnrolledSession
	}
	resolver, ok := g.opts.Auth.(hashSessionResolver)
	if !ok {
		return auth.Session{}, errNotEnrolledSession
	}
	session, err := resolver.ResolveHash(ctx, hash)
	if err != nil {
		return auth.Session{}, err
	}
	if session.Scope != auth.ScopeEnrolled || session.FactorOwed || session.Bot != nil || session.UserID != c.userID || session.DeviceID != c.deviceID {
		return auth.Session{}, errNotEnrolledSession
	}
	return session, nil
}

// A bounded allowance shared by HTTP and every socket of a device. Reconnect cannot reset it.
func (p *presence) chargeActivity(device id.ID) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.clk.Now()
	if last, ok := p.activityLast[device]; ok && now.Sub(last) < activityInterval {
		return false
	}
	if len(p.activityLast) >= activityBudgetCap {
		for d, last := range p.activityLast {
			if now.Sub(last) >= activityInterval {
				delete(p.activityLast, d)
			}
		}
		if len(p.activityLast) >= activityBudgetCap {
			return false
		}
	}
	p.activityLast[device] = now
	return true
}

func (g *Gateway) handlePresenceActivity(ctx context.Context, c *conn, in Inbound) {
	if g.connectionWire(c) < 5 {
		g.signalError(c, "E_VERSION")
		return
	}
	if in.CID != 0 || in.GroupID != nil || len(in.Payload) != 0 {
		g.signalError(c, "E_FRAME_SHAPE")
		return
	}
	if !g.presence.chargeActivity(c.deviceID) {
		return
	}
	if _, err := g.currentSignalSession(ctx, c); err != nil {
		g.signalError(c, string(server.CodeUnauthenticated))
		return
	}
	if !g.markPresenceInput(c) {
		return
	}
	if g.opts.OnPresenceChanged != nil {
		g.opts.OnPresenceChanged(c.userID)
	}
}

// Serialize the small activity write with registry removal: a delayed authority read cannot
// leave activity behind after the last socket has disconnected and clearActivity has run.
func (g *Gateway) markPresenceInput(c *conn) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state != stateReady {
		return false
	}
	g.reg.mu.RLock()
	defer g.reg.mu.RUnlock()
	if !slices.Contains(g.reg.byDevice[c.deviceID], c) {
		return false
	}
	g.presence.activity(c.deviceID, c.userID)
	return true
}
