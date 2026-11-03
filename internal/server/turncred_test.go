package server_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pion/stun/v3"
	"github.com/pion/turn/v5"

	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/config"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/server"
)

// Re-review N3: the relay refuses every credential issued before its process started. A restart
// forgets every cut, and drops every call anyway (the SFU is in-process), so clients re-POST the
// calls route; a credential minted before the restart — cut or not — is refused on every method,
// and one minted after the start works.
func TestTheRelayRefusesEveryCredentialMintedBeforeItStarted(t *testing.T) {
	restart := time.Unix(1_790_000_000, 0)
	clk := clock.NewFake(restart)
	rev := server.NewRelayRevocations(2*time.Hour, clk).WithCredentialTTL(time.Hour)
	auth := server.TURNAuthForTest("s3cret", clk, 2*time.Hour, rev)
	dev := id.New()
	ok := func(user string, m stun.Method) bool {
		_, _, ok := auth(&turn.RequestAttributes{Username: user, Realm: "chat.example.test", Method: m})
		return ok
	}
	methods := []stun.Method{stun.MethodAllocate, stun.MethodRefresh, stun.MethodCreatePermission,
		stun.MethodChannelBind}
	for _, before := range []time.Duration{time.Millisecond, time.Second, 10 * time.Minute} {
		old, _ := server.TURNCredential("s3cret", dev, time.Hour, restart.Add(-before))
		for _, m := range methods {
			if ok(old, m) {
				t.Errorf("%s with a credential minted %v before the process started was accepted", m, before)
			}
		}
	}
	clk.Advance(time.Millisecond)
	fresh, _ := server.TURNCredential("s3cret", dev, time.Hour, clk.Now())
	for _, m := range methods {
		if !ok(fresh, m) {
			t.Errorf("%s with a credential minted after the process started was refused", m)
		}
	}
}

// Re-review N6: an authenticated Allocate that pion then refuses — before its quota handler (437,
// 440: the handler never runs) or in it (486, a device known to be barred) — leaves nothing pending.
// The device is cut, and its next Allocate with a credential issued after the cut gets its relay
// socket at once, inside the window those refused Allocates would otherwise have held a stale issue
// time for.
func TestARefusedAllocateLeavesNothingPending(t *testing.T) {
	clk := clock.NewFake(time.Unix(1_790_000_000, 0))
	rev := server.NewRelayRevocations(2*time.Hour, clk).WithCredentialTTL(time.Hour)
	look := &fakeBarred{}
	rev.WithBarred(look, nil)
	quota, events := server.TURNHandlersWithRevForTest(1, rev)
	dev := id.New()
	addr := func(port int) net.Addr { return &net.TCPAddr{IP: net.IPv4(192, 0, 2, 1), Port: port} }
	relayAddr := addr(3478)
	sock := func() net.PacketConn {
		pc, err := (&net.ListenConfig{}).ListenPacket(t.Context(), "udp4", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		t.Cleanup(func() { _ = pc.Close() })
		return pc
	}
	auth := func(src net.Addr, user string) {
		events.OnAuth(src, relayAddr, "UDP", user, "chat.example.test", stun.MethodAllocate.String(), true)
	}
	clk.Advance(time.Millisecond)
	old, _ := server.TURNCredential("s3cret", dev, time.Hour, clk.Now())
	// One admitted allocation holds the device's quota of one.
	auth(addr(1), old)
	if !quota(dev.String(), "chat.example.test", addr(1)) || !rev.TrackForTest(dev.String(), sock()) {
		t.Fatal("the first allocation was refused")
	}
	auth(addr(2), old) // then pion refuses it 437 or 440: the quota handler never runs
	auth(addr(3), old)
	if quota(dev.String(), "chat.example.test", addr(3)) {
		t.Fatal("an allocation past the quota of one was admitted")
	}
	// A device known to be barred is refused by the quota handler from the cache.
	barred := id.New()
	look.set(barred, true, nil)
	barredOld, _ := server.TURNCredential("s3cret", barred, time.Hour, clk.Now())
	auth(addr(4), barredOld) // OnAuth's lookup finds it barred and cuts it
	if quota(barred.String(), "chat.example.test", addr(4)) {
		t.Fatal("a device known to be barred was admitted")
	}
	clk.Advance(time.Millisecond)
	rev.Revoke(dev, clk.Now()) // closes the first allocation's socket, which frees its slot below
	events.OnAllocationDeleted(addr(1), relayAddr, "UDP", dev.String(), "chat.example.test")
	clk.Advance(time.Millisecond)
	fresh, _ := server.TURNCredential("s3cret", dev, time.Hour, clk.Now())
	auth(addr(5), fresh)
	if !quota(dev.String(), "chat.example.test", addr(5)) {
		t.Fatal("the fresh allocation was refused by the quota handler")
	}
	if !rev.TrackForTest(dev.String(), sock()) {
		t.Fatal("a fresh credential's relay socket was refused: a refused Allocate left its stale issue time pending")
	}
	look.set(barred, false, nil)
	clk.Advance(time.Minute) // the barred answer has expired; the cut stays
	barredFresh, _ := server.TURNCredential("s3cret", barred, time.Hour, clk.Now())
	auth(addr(6), barredFresh)
	if !quota(barred.String(), "chat.example.test", addr(6)) || !rev.TrackForTest(barred.String(), sock()) {
		t.Fatal("a device no longer barred, with a credential issued after its cut, was refused")
	}
}

// fakeHungBarred is a BarredLookup that hangs until its context ends, reports that it saw the
// cancellation, and then keeps the lookup in flight until release is closed.
type fakeHungBarred struct {
	entered   chan struct{}
	once      sync.Once
	cancelled chan struct{}
	release   chan struct{}
	ended     chan error
}

func (f *fakeHungBarred) DeviceBarred(ctx context.Context, _ id.ID) (bool, error) {
	f.once.Do(func() { close(f.entered) })
	<-ctx.Done()
	close(f.cancelled)
	<-f.release
	select {
	case f.ended <- ctx.Err():
	default:
	}
	return false, ctx.Err()
}

// Re-review N5: Close cancels the holders' re-check's store lookups and returns only once the
// re-check has ended. Deterministic: the lookup stays in flight after it sees the cancellation until
// the test releases it, so a Close that does not join returns while it is still held.
func TestCloseCancelsAndJoinsTheHoldersRecheck(t *testing.T) {
	look := &fakeHungBarred{entered: make(chan struct{}), cancelled: make(chan struct{}),
		release: make(chan struct{}), ended: make(chan error, 1)}
	rev := server.NewRelayRevocations(2*time.Hour, clock.System()).WithBarred(look, nil)
	rev.SetHolderCheckEveryForTest(5 * time.Millisecond)
	pc, err := (&net.ListenConfig{}).ListenPacket(t.Context(), "udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	rev.HoldSocketForTest(id.New().String(), pc)
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv, err := server.StartTURN(config.TURN{Enabled: true, Realm: "chat.example.test", RelayIP: "127.0.0.1",
		SharedSecretFile: writeFile(t, "turn.secret", "0123456789abcdef0123456789abcdef"), CredentialTTL: "1h"},
		ln, netip.MustParseAddr("127.0.0.1"), server.TURNPeers{}, nil, rev, clock.System(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("StartTURN: %v", err)
	}
	select {
	case <-look.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the holders' re-check never asked the store")
	}
	closed := make(chan error, 1)
	go func() { closed <- srv.Close() }()
	select {
	case <-look.cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not cancel the hung lookup")
	}
	select {
	case <-closed:
		t.Fatal("Close returned while the re-check's lookup was still in flight: the re-check was not joined")
	case <-time.After(200 * time.Millisecond):
	}
	close(look.release)
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return once the lookup ended")
	}
	select {
	case err := <-look.ended:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("the hung lookup ended with %v, want its context cancelled", err)
		}
	default:
		t.Fatal("Close returned before the re-check's lookup ended: the re-check was not joined")
	}
}

// Re-review N8: the username parse is strict — exactly three fields, unsigned decimal digits only
// for the two numbers, within length bounds, an issue time no more than 2 s ahead of the clock, a
// lifetime (expiry less issue time) within turn.credential_ttl plus that skew, and a device field
// that parses as a device id. The whole username is MAC-covered, so this is defence in depth: the
// handler refuses each of these before it computes a key.
func TestTheRelayUsernameParseIsStrict(t *testing.T) {
	now := time.Unix(1_790_000_000, 0)
	clk := clock.NewFake(now)
	// TURNAuthForTest's credential_ttl is its max age: one hour here.
	auth := server.TURNAuthForTest("s3cret", clk, time.Hour, nil)
	dev := id.New().String()
	exp := now.Add(time.Hour).Unix()
	ms := now.UnixMilli()
	good := fmt.Sprintf("%d:%s:%d", exp, dev, ms)
	for _, m := range []stun.Method{stun.MethodAllocate, stun.MethodRefresh} {
		if gotDev, _, ok := auth(&turn.RequestAttributes{Username: good, Method: m}); !ok || gotDev != dev {
			t.Fatalf("%s: the well-formed username %q = %q, %v; want the device", m, good, gotDev, ok)
		}
	}
	for name, user := range map[string]string{
		"two fields":                     fmt.Sprintf("%d:%s", exp, dev),
		"four fields":                    good + ":1",
		"empty":                          "",
		"empty expiry":                   fmt.Sprintf(":%s:%d", dev, ms),
		"empty device":                   fmt.Sprintf("%d::%d", exp, ms),
		"empty issue time":               fmt.Sprintf("%d:%s:", exp, dev),
		"a plus sign on the expiry":      fmt.Sprintf("+%d:%s:%d", exp, dev, ms),
		"a plus sign on the issue time":  fmt.Sprintf("%d:%s:+%d", exp, dev, ms),
		"a negative expiry":              fmt.Sprintf("-%d:%s:%d", exp, dev, ms),
		"a negative issue time":          fmt.Sprintf("%d:%s:-%d", exp, dev, ms),
		"a space":                        fmt.Sprintf(" %d:%s:%d", exp, dev, ms),
		"a trailing newline":             good + "\n",
		"hex":                            fmt.Sprintf("0x%x:%s:%d", exp, dev, ms),
		"an exponent":                    fmt.Sprintf("%d:%s:1790000000e3", exp, dev),
		"non-ASCII digits":               fmt.Sprintf("%d:%s:١٧٩٠٠٠٠٠٠٠٠٠٠", exp, dev),
		"an overlong expiry":             fmt.Sprintf("%s%d:%s:%d", strings.Repeat("0", 20), exp, dev, ms),
		"an overlong issue time":         fmt.Sprintf("%d:%s:%s%d", exp, dev, strings.Repeat("0", 20), ms),
		"a device that is no id":         fmt.Sprintf("%d:%s:%d", exp, "not-a-device", ms),
		"a device id with a suffix":      fmt.Sprintf("%d:%s#x:%d", exp, dev, ms),
		"issued 3 s ahead of the clock":  fmt.Sprintf("%d:%s:%d", exp, dev, ms+3000),
		"issued after it expires":        fmt.Sprintf("%d:%s:%d", now.Unix(), dev, ms+1000),
		"a lifetime past the TTL":        fmt.Sprintf("%d:%s:%d", now.Add(3*time.Hour).Unix(), dev, ms),
		"an issue time in seconds":       fmt.Sprintf("%d:%s:%d", exp, dev, now.Unix()),
		"an expiry in milliseconds":      fmt.Sprintf("%d:%s:%d", now.Add(time.Hour).UnixMilli(), dev, ms),
		"a lifetime past the TTL, later": fmt.Sprintf("%d:%s:%d", now.Add(time.Hour+3*time.Second).Unix(), dev, ms),
	} {
		for _, m := range []stun.Method{stun.MethodAllocate, stun.MethodRefresh} {
			if _, _, ok := auth(&turn.RequestAttributes{Username: user, Method: m}); ok {
				t.Errorf("%s: %s: the username %q was accepted", m, name, user)
			}
		}
	}
}
