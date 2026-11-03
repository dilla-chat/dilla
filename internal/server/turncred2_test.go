package server_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/pion/stun/v3"
	"github.com/pion/turn/v5"

	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/server"
)

// Re-review N9: a credential's Allocate-validity is compared with the clock in milliseconds, so it
// never outlives its device's mint record (kept while now <= issued + ttl). The reviewer's probe: a
// credential minted at X000 + 1 ms, its device cut 1 ms after the mint record expired (a cut the relay
// does not record, since the device holds nothing it could refuse), is refused at Allocate 500 ms
// later; before, it was accepted until the end of the expiry's whole second. The boundary is
// inclusive: Allocate is accepted while now <= expiry_s * 1000, refused from the next millisecond.
func TestACutCredentialCannotAllocateAfterItsMintRecordExpired(t *testing.T) {
	base := time.Unix(1_790_000_000, 0)
	setup := func() (*clock.Fake, *server.RelayRevocations, func(string) bool, id.ID, string, time.Time) {
		clk := clock.NewFake(base)
		rev := server.NewRelayRevocations(time.Hour, clk).WithCredentialTTL(time.Hour)
		auth := server.TURNAuthForTest("s3cret", clk, time.Hour, rev)
		clk.Advance(time.Millisecond) // the mint is at X000 + 1 ms
		dev := id.New()
		issued, wait := rev.Mint(dev, clk.Now())
		if wait != 0 {
			t.Fatalf("Mint wait %v", wait)
		}
		user, _ := server.TURNCredential("s3cret", dev, time.Hour, issued)
		allocate := func(u string) bool {
			_, _, ok := auth(&turn.RequestAttributes{Username: u, Method: stun.MethodAllocate})
			return ok
		}
		return clk, rev, allocate, dev, user, issued
	}

	// The probe.
	clk, rev, allocate, dev, user, issued := setup()
	clk.Advance(time.Hour + time.Millisecond) // t + ttl + 1 ms
	rev.Revoke(dev, clk.Now())
	clk.Advance(500 * time.Millisecond) // t + ttl + 501 ms
	if allocate(user) {
		t.Fatalf("Allocate at issued+ttl+501ms was accepted (issued %v)", issued)
	}

	// The boundary, both sides: expiry_s * 1000 is issued + ttl - 1 ms here.
	clk, _, allocate, _, user, issued = setup()
	expiryMs := issued.Add(time.Hour).Unix() * 1000
	clk.Advance(time.Duration(expiryMs-issued.UnixMilli()) * time.Millisecond)
	if !allocate(user) {
		t.Fatal("Allocate in the millisecond of its expiry second's start was refused")
	}
	clk.Advance(time.Millisecond)
	if allocate(user) {
		t.Fatal("Allocate one millisecond after expiry_s * 1000 was accepted")
	}

	// The invariant: whenever the relay accepts an Allocate, a cut of a socket-less device at that
	// moment is recorded, in every millisecond around the expiry.
	for off := -3; off <= 1003; off++ {
		clk, rev, allocate, dev, user, _ = setup()
		clk.Advance(time.Hour + time.Duration(off)*time.Millisecond)
		if !allocate(user) {
			continue
		}
		rev.Revoke(dev, clk.Now())
		if rev.RelayCutsForTest() != 1 {
			t.Fatalf("at issued+ttl%+dms the relay accepts the credential, but a cut of its device was not recorded", off)
		}
	}
}

// Re-review N8: neither number of the username has a leading zero.
func TestTheRelayUsernameRefusesLeadingZeros(t *testing.T) {
	now := time.Unix(1_790_000_000, 0)
	clk := clock.NewFake(now)
	auth := server.TURNAuthForTest("s3cret", clk, time.Hour, nil)
	dev := id.New().String()
	exp, ms := now.Add(time.Hour).Unix(), now.UnixMilli()
	for name, user := range map[string]string{
		"a leading zero on the expiry":     fmt.Sprintf("0%d:%s:%d", exp, dev, ms),
		"a leading zero on the issue time": fmt.Sprintf("%d:%s:0%d", exp, dev, ms),
	} {
		if _, _, ok := auth(&turn.RequestAttributes{Username: user, Method: stun.MethodAllocate}); ok {
			t.Errorf("%s: the username %q was accepted", name, user)
		}
	}
}

// Re-review m1, m7: after a backwards wall-clock step a device's cut can be ahead of the clock, with
// the request's start (taken before the step) later than the cut. Mint then refuses on the cut alone,
// and answers the wait that lands one millisecond past it (a cut in the clock's own millisecond
// waits 1 ms).
func TestMintAfterABackwardsClockStepWaitsPastTheCut(t *testing.T) {
	base := time.Unix(1_790_000_000, 0)
	clk := clock.NewFake(base)
	rev := server.NewRelayRevocations(2*time.Hour, clk).WithCredentialTTL(time.Hour)
	dev := id.New()
	clk.Advance(time.Millisecond)
	rev.Mint(dev, clk.Now())
	clk.Advance(999 * time.Millisecond) // B+1000
	rev.Revoke(dev, clk.Now())
	clk.Advance(200 * time.Millisecond) // B+1200: the request begins
	since := clk.Now()
	clk.Advance(-300 * time.Millisecond) // the wall clock steps back to B+900
	if _, wait := rev.Mint(dev, since); wait != 101*time.Millisecond {
		t.Fatalf("a mint 100 ms behind the cut waited %v, want 101ms (to one millisecond past it)", wait)
	}
	clk.Advance(100 * time.Millisecond) // B+1000, the cut's own millisecond; since is later than it
	if issued, wait := rev.Mint(dev, since); wait != time.Millisecond {
		t.Fatalf("a mint in the cut's millisecond, after a step back, = %v, wait %v; want wait 1ms", issued, wait)
	}
	clk.Advance(time.Millisecond)
	// One millisecond past the cut, and the request began after it: nothing orders them by luck.
	issued, wait := rev.Mint(dev, since)
	if wait != 0 || !issued.Equal(clk.Now()) {
		t.Fatalf("a mint after the wait = %v, wait %v; want the clock's time", issued, wait)
	}
}
