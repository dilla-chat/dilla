package gateway

import (
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/jonasthim/dilla/internal/clock"
	"github.com/jonasthim/dilla/internal/id"
)

// typingTTL is how long one typing signal lives. Ephemeral state is never persisted and never
// replayed: op 48-50 frames are n = 0 by construction.
const typingTTL = 10 * time.Second
const typingInterval = 3 * time.Second
const typingChannelCap = 64

// New channels stop being admitted at 64 recent stops. Up to 64 already-live
// channels may then stop, so live and recent entries together stay at most 128 per device.
const typingRecentCap = 64

type typingState struct {
	user    id.ID
	expires time.Time
	last    time.Time
}

type presence struct {
	mu           sync.Mutex
	clk          clock.Clock
	typing       map[[2]id.ID]typingState
	recentTyping map[[2]id.ID]time.Time
	// cooldown survives socket removal; live/recent state does not.
	cooldown     map[[2]id.ID]time.Time
	signalBudget map[id.ID]*rate.Limiter
	signalSeen   map[id.ID]time.Time
	signalSweep  time.Time
	active       map[id.ID]deviceActivity
	activityLast map[id.ID]time.Time
}

const presenceIdleAfter = 10 * time.Minute

type deviceActivity struct {
	user id.ID
	at   time.Time
}

func newPresence(clk clock.Clock) *presence {
	return &presence{clk: clk, typing: map[[2]id.ID]typingState{}, recentTyping: map[[2]id.ID]time.Time{}, cooldown: map[[2]id.ID]time.Time{}, signalBudget: map[id.ID]*rate.Limiter{}, signalSeen: map[id.ID]time.Time{}, active: map[id.ID]deviceActivity{}, activityLast: map[id.ID]time.Time{}}
}

func (p *presence) activity(device, user id.ID) {
	p.mu.Lock()
	p.active[device] = deviceActivity{user: user, at: p.clk.Now()}
	p.mu.Unlock()
}

func (p *presence) clearActivity(device id.ID) {
	p.mu.Lock()
	delete(p.active, device)
	p.mu.Unlock()
}

func (p *presence) expireActivity(now time.Time) []id.ID {
	p.mu.Lock()
	defer p.mu.Unlock()
	users := map[id.ID]struct{}{}
	for device, state := range p.active {
		if now.Sub(state.at) >= presenceIdleAfter {
			delete(p.active, device)
			users[state.user] = struct{}{}
		}
	}
	out := make([]id.ID, 0, len(users))
	for user := range users {
		out = append(out, user)
	}
	return out
}

// Activity refreshes one live device's activity clock. It cannot make a disconnected device online.
func (g *Gateway) Activity(device id.ID) {
	conns := g.reg.connsOfDevice(device)
	if len(conns) == 0 || conns[0].isBot || !g.presence.chargeActivity(device) {
		return
	}
	user := conns[0].userID
	if !g.markPresenceInput(conns[0]) {
		return
	}
	if g.opts.OnPresenceChanged != nil {
		g.opts.OnPresenceChanged(user)
	}
}

// PresenceStatus derives the visible status from the account choice and live active devices.
// 0 offline, 1 online, 2 idle, 3 do not disturb. Invisible is always seen as offline.
func (g *Gateway) PresenceStatus(user id.ID, choice uint8) uint8 {
	connections := g.reg.connsOfUser(user)
	if choice == 4 || len(connections) == 0 {
		return 0
	}
	if choice == 2 || choice == 3 {
		return choice
	}
	g.presence.mu.Lock()
	defer g.presence.mu.Unlock()
	now := g.opts.Clock.Now()
	for _, c := range connections {
		if state, ok := g.presence.active[c.deviceID]; ok && now.Sub(state.at) < presenceIdleAfter {
			return 1
		}
	}
	return 2
}

// chargeSignal bounds all ephemeral work and fan-out across a user's sockets.
// The 128-frame burst preserves the documented 64-channel set/clear sequence.
func (p *presence) chargeSignal(user id.ID) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.clk.Now()
	if now.Sub(p.signalSweep) >= time.Minute {
		p.expireTyping(now)
		for u, last := range p.signalSeen {
			if now.Sub(last) >= 3*time.Minute {
				delete(p.signalSeen, u)
				delete(p.signalBudget, u)
			}
		}
		p.signalSweep = now
	}
	limiter := p.signalBudget[user]
	if limiter == nil {
		limiter = rate.NewLimiter(1, 128)
		p.signalBudget[user] = limiter
	}
	p.signalSeen[user] = now
	return limiter.AllowN(now, 1)
}

func (p *presence) expireTyping(now time.Time) {
	for key, state := range p.typing {
		if !now.Before(state.expires) {
			delete(p.typing, key)
		}
	}
	for key, last := range p.recentTyping {
		if now.Sub(last) >= typingInterval {
			delete(p.recentTyping, key)
		}
	}
	for key, last := range p.cooldown {
		if now.Sub(last) >= typingInterval {
			delete(p.cooldown, key)
		}
	}
}

func (p *presence) clearDeviceTyping(deviceID id.ID) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for key := range p.typing {
		if key[1] == deviceID {
			delete(p.typing, key)
		}
	}
	for key := range p.recentTyping {
		if key[1] == deviceID {
			delete(p.recentTyping, key)
		}
	}
}

// SetTyping records or clears one device's signal in one channel.
func (g *Gateway) SetTyping(channelID, userID, deviceID id.ID, typing bool) {
	g.acceptTyping(channelID, userID, deviceID, typing)
}

// acceptTyping admits at most one live signal per device and channel in three seconds.
// Expired entries are removed before the per-device channel cap is checked.
func (g *Gateway) acceptTyping(channelID, userID, deviceID id.ID, typing bool) bool {
	p := g.presence
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.clk.Now()
	p.expireTyping(now)
	key := [2]id.ID{channelID, deviceID}
	state, exists := p.typing[key]
	if typing {
		if _, cooling := p.cooldown[key]; cooling {
			return false
		}
		if exists && now.Sub(state.last) < typingInterval {
			return false
		}
		if !exists {
			if _, recent := p.recentTyping[key]; recent {
				return false
			}
			recentCount := 0
			for k := range p.recentTyping {
				if k[1] == deviceID {
					recentCount++
				}
			}
			if recentCount >= typingRecentCap {
				return false
			}
		}
		if !exists {
			count := 0
			for k := range p.typing {
				if k[1] == deviceID {
					count++
				}
			}
			if count >= typingChannelCap {
				return false
			}
		}
		p.typing[key] = typingState{user: userID, expires: now.Add(typingTTL), last: now}
		p.cooldown[key] = now
		return true
	}
	if !exists || state.user != userID {
		return false
	}
	delete(p.typing, key)
	if now.Sub(state.last) < typingInterval {
		p.recentTyping[key] = state.last
	}
	return true
}

// IsTyping reports whether a live typing signal exists. An expired entry is dropped on read, so
// the map does not grow with every keystroke of every user.
func (g *Gateway) IsTyping(channelID, userID, deviceID id.ID) bool {
	p := g.presence
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.clk.Now()
	p.expireTyping(now)
	key := [2]id.ID{channelID, deviceID}
	state, ok := p.typing[key]
	if !ok {
		return false
	}
	return state.user == userID
}
