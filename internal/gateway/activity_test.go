package gateway

import (
	"context"
	"testing"
	"time"

	"github.com/jonasthim/dilla/internal/auth"
	"github.com/jonasthim/dilla/internal/id"
)

type activityAuth struct {
	*capabilityRecordingAuth
	session      auth.Session
	err          error
	afterResolve func()
}

func (a *activityAuth) ResolveHash(context.Context, []byte) (auth.Session, error) {
	if a.afterResolve != nil {
		a.afterResolve()
	}
	return a.session, a.err
}

func TestPresenceActivityUsesRealInputAndSharedBudget(t *testing.T) {
	h := newHarness(t)
	h.gw.opts.WireVersions = []uint64{5}
	a := &activityAuth{capabilityRecordingAuth: &capabilityRecordingAuth{Authenticator: h}}
	h.gw.opts.Auth = a
	c := h.connect(t, id.New())
	h.drainReady(t, c)
	a.session = auth.Session{DeviceID: c.deviceID, UserID: c.userID, Scope: auth.ScopeEnrolled}
	if got := h.gw.PresenceStatus(c.userID, 1); got != 2 {
		t.Fatalf("connect fabricated input: %d", got)
	}
	for i := 0; i < 40; i++ {
		h.clk.Advance(15 * time.Second)
		p, _ := payload(uint64(1), uint64(1))
		in, err := Decode(mustEncode(t, Frame{Op: OpHeartbeat, Payload: p}, 0), 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		h.gw.handle(context.Background(), c, in)
	}
	if got := h.gw.PresenceStatus(c.userID, 1); got != 2 {
		t.Fatalf("heartbeat fabricated input: %d", got)
	}
	h.gw.handle(context.Background(), c, Inbound{Op: OpPresenceActivity})
	if got := h.gw.PresenceStatus(c.userID, 1); got != 1 {
		t.Fatalf("input did not activate: %d", got)
	}
	h.clk.Advance(4 * time.Second)
	h.gw.Activity(c.deviceID) // HTTP shares the same budget.
	h.clk.Advance(presenceIdleAfter - 4*time.Second)
	if got := h.gw.PresenceStatus(c.userID, 1); got != 2 {
		t.Fatalf("HTTP bypassed WSS allowance: %d", got)
	}
	h.gw.Activity(c.deviceID)
	h.clk.Advance(4 * time.Second)
	h.gw.handle(context.Background(), c, Inbound{Op: OpPresenceActivity})
	h.clk.Advance(presenceIdleAfter - 4*time.Second)
	if got := h.gw.PresenceStatus(c.userID, 1); got != 2 {
		t.Fatalf("WSS bypassed HTTP allowance: %d", got)
	}
}

func TestPresenceActivityRejectsIneligibleSessions(t *testing.T) {
	for _, kind := range []string{"legacy", "bot", "owed", "pending", "revoked", "malformed", "disconnected"} {
		t.Run(kind, func(t *testing.T) {
			h := newHarness(t)
			h.gw.opts.WireVersions = []uint64{5}
			a := &activityAuth{capabilityRecordingAuth: &capabilityRecordingAuth{Authenticator: h}}
			h.gw.opts.Auth = a
			c := h.connect(t, id.New())
			h.drainReady(t, c)
			a.session = auth.Session{DeviceID: c.deviceID, UserID: c.userID, Scope: auth.ScopeEnrolled}
			in := Inbound{Op: OpPresenceActivity}
			switch kind {
			case "legacy":
				c.wire = 4
			case "bot":
				c.isBot = true
			case "owed":
				a.session.FactorOwed = true
			case "pending":
				a.session.Scope = auth.ScopePending
			case "revoked":
				a.err = errNotEnrolledSession
			case "malformed":
				in.Payload = append(in.Payload, []byte{0})
			case "disconnected":
				h.gw.removeLive(c)
			}
			h.gw.handle(context.Background(), c, in)
			if got := h.gw.PresenceStatus(c.userID, 1); got == 1 {
				t.Fatal("ineligible signal fabricated activity")
			}
		})
	}
}

func TestPresenceActivityDiscardedWhenDisconnectsDuringAuthorityRead(t *testing.T) {
	h := newHarness(t)
	h.gw.opts.WireVersions = []uint64{5}
	a := &activityAuth{capabilityRecordingAuth: &capabilityRecordingAuth{Authenticator: h}}
	h.gw.opts.Auth = a
	c := h.connect(t, id.New())
	h.drainReady(t, c)
	a.session = auth.Session{DeviceID: c.deviceID, UserID: c.userID, Scope: auth.ScopeEnrolled}
	a.afterResolve = func() { h.gw.removeLive(c) }
	h.gw.handle(context.Background(), c, Inbound{Op: OpPresenceActivity})
	h.gw.presence.mu.Lock()
	_, exists := h.gw.presence.active[c.deviceID]
	h.gw.presence.mu.Unlock()
	if exists {
		t.Fatal("disconnected input retained for a later socket")
	}
}

func TestPresenceActivityBudgetSurvivesSocketChurnAndIsBounded(t *testing.T) {
	h := newHarness(t)
	device := id.New()
	if !h.gw.presence.chargeActivity(device) {
		t.Fatal("first input refused")
	}
	if h.gw.presence.chargeActivity(device) {
		t.Fatal("another socket reset allowance")
	}
	for i := 1; i < activityBudgetCap; i++ {
		if !h.gw.presence.chargeActivity(id.New()) {
			t.Fatal("premature capacity")
		}
	}
	if h.gw.presence.chargeActivity(id.New()) {
		t.Fatal("activity cache unbounded")
	}
	h.clk.Advance(activityInterval)
	if !h.gw.presence.chargeActivity(id.New()) {
		t.Fatal("expired allowance did not release capacity")
	}
}
