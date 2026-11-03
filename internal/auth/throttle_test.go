package auth_test

import (
	"net/netip"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/auth"
	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/config"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/server"
)

func TestLockoutScheduleFollowsTheConfiguredCurve(t *testing.T) {
	c := config.Default()
	clk := clock.NewFake(time.Unix(1_700_000_000, 0))
	th := auth.NewThrottle(c.Limits.Rate, c.Auth.Lockout, clk)
	user := id.New()
	ip := netip.MustParseAddr("203.0.113.9")

	for i := 0; i < c.Auth.Lockout.FreeAttempts; i++ {
		if d := th.RecordFailure(user, ip); d != 0 {
			t.Fatalf("failure %d locked out after %s; the first %d are free", i+1, d, c.Auth.Lockout.FreeAttempts)
		}
	}
	first := th.RecordFailure(user, ip)
	if first != c.Auth.Lockout.FirstLockout.Value() {
		t.Fatalf("first lockout = %s, want %s", first, c.Auth.Lockout.FirstLockout.Value())
	}
	second := th.RecordFailure(user, ip)
	if second <= first {
		t.Fatalf("the lockout did not grow: %s then %s", first, second)
	}
	for i := 0; i < 20; i++ {
		last := th.RecordFailure(user, ip)
		if last > c.Auth.Lockout.LockoutCeiling.Value() {
			t.Fatalf("lockout %s exceeded the ceiling %s", last, c.Auth.Lockout.LockoutCeiling.Value())
		}
	}
}

// RecordFailure REPORTS a lockout; LockedFor is how a handler ENFORCES one,
// before it checks the credential and without adding a failure of its own.
func TestLockedForReportsTheStandingLockoutWithoutRecordingAFailure(t *testing.T) {
	c := config.Default()
	clk := clock.NewFake(time.Unix(1_700_000_000, 0))
	th := auth.NewThrottle(c.Limits.Rate, c.Auth.Lockout, clk)
	user := id.New()
	ip := netip.MustParseAddr("203.0.113.9")

	if d := th.LockedFor(user); d != 0 {
		t.Fatalf("an account that has never failed a login is locked for %s", d)
	}
	for i := 0; i < c.Auth.Lockout.FreeAttempts; i++ {
		th.RecordFailure(user, ip)
	}
	if d := th.LockedFor(user); d != 0 {
		t.Fatalf("locked for %s inside the %d free attempts", d, c.Auth.Lockout.FreeAttempts)
	}
	want := th.RecordFailure(user, ip)
	if want == 0 {
		t.Fatal("the attempt past free_attempts reported no lockout")
	}
	if got := th.LockedFor(user); got != want {
		t.Fatalf("LockedFor = %s, but RecordFailure reported %s", got, want)
	}
	// A hundred gated requests must not themselves be failures, or the gate
	// would escalate the lockout of an account nobody is even guessing at.
	for i := 0; i < 100; i++ {
		th.LockedFor(user)
	}
	if got := th.LockedFor(user); got != want {
		t.Fatalf("LockedFor = %s after a hundred peeks, want %s: the gate is recording failures", got, want)
	}
	// The lockout runs from the last failure, so it lifts.
	clk.Advance(want + time.Second)
	if d := th.LockedFor(user); d != 0 {
		t.Fatalf("the lockout did not lift once it expired: %s", d)
	}
}

func TestFailuresAreCountedPerAccountAndPerAddress(t *testing.T) {
	c := config.Default()
	clk := clock.NewFake(time.Unix(1_700_000_000, 0))
	th := auth.NewThrottle(c.Limits.Rate, c.Auth.Lockout, clk)
	victim := id.New()
	attacker := netip.MustParseAddr("203.0.113.9")
	for i := 0; i < 50; i++ {
		th.RecordFailure(id.New(), attacker) // fifty different accounts, one address
	}
	// Peek, not Allow: the route reads this bucket without spending it, so the
	// assertion has to be the same non-consuming question the route asks.
	if ok, wait := th.Peek("login_failed", server.RateKey(attacker)); ok {
		t.Fatal("an address that failed fifty logins across fifty accounts is still allowed")
	} else if wait <= 0 {
		t.Fatal("a refused peek reported no retry-after")
	}
	// And peeking twice answers the same, which is what "non-consuming" means.
	if ok, _ := th.Peek("login_failed", server.RateKey(attacker)); ok {
		t.Fatal("the second peek at an empty bucket was allowed; Peek is spending tokens")
	}
	// An unrelated address has its full budget.
	if ok, _ := th.Peek("login_failed", server.RateKey(netip.MustParseAddr("198.51.100.4"))); !ok {
		t.Fatal("an address that has never failed a login is already throttled")
	}
	// The victim's own account is untouched by someone else's address.
	if d := th.RecordFailure(victim, netip.MustParseAddr("198.51.100.4")); d != 0 {
		t.Fatalf("an unrelated account was locked out after one failure: %s", d)
	}
}

func TestClearResetsTheAccountOnSuccess(t *testing.T) {
	c := config.Default()
	clk := clock.NewFake(time.Unix(1_700_000_000, 0))
	th := auth.NewThrottle(c.Limits.Rate, c.Auth.Lockout, clk)
	user := id.New()
	ip := netip.MustParseAddr("203.0.113.9")
	for i := 0; i < 6; i++ {
		th.RecordFailure(user, ip)
	}
	th.Clear(user)
	if d := th.RecordFailure(user, ip); d != 0 {
		t.Fatalf("a successful login did not reset the counter: %s", d)
	}
}

func TestObservationWindowExpiresFailures(t *testing.T) {
	c := config.Default()
	clk := clock.NewFake(time.Unix(1_700_000_000, 0))
	th := auth.NewThrottle(c.Limits.Rate, c.Auth.Lockout, clk)
	user := id.New()
	ip := netip.MustParseAddr("203.0.113.9")
	for i := 0; i < 6; i++ {
		th.RecordFailure(user, ip)
	}
	clk.Advance(c.Auth.Lockout.ObservationWindow.Value() + time.Minute)
	th.Sweep()
	if d := th.RecordFailure(user, ip); d != 0 {
		t.Fatalf("failures outside the observation window still locked the account: %s", d)
	}
}
