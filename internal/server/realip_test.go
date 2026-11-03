package server_test

import (
	"net/http/httptest"
	"net/netip"
	"testing"

	"github.com/dilla-chat/dilla/internal/server"
)

func TestRealIPTrustsOnlyConfiguredProxies(t *testing.T) {
	trusted := []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}
	r := httptest.NewRequestWithContext(t.Context(), "GET", "/", nil)
	r.RemoteAddr = "127.0.0.1:5000"
	r.Header.Set("X-Forwarded-For", "203.0.113.9, 127.0.0.1")
	if got := server.RealIP(r, trusted); got.String() != "203.0.113.9" {
		t.Fatalf("RealIP = %s, want the client address through a trusted proxy", got)
	}
	r.RemoteAddr = "198.51.100.4:5000"
	if got := server.RealIP(r, trusted); got.String() != "198.51.100.4" {
		t.Fatalf("RealIP = %s; an untrusted peer must not be able to forge its address", got)
	}
}

func TestIPv6IsKeyedBySlash64(t *testing.T) {
	a := netip.MustParseAddr("2001:db8:1:2:aaaa::1")
	b := netip.MustParseAddr("2001:db8:1:2:bbbb::9")
	c := netip.MustParseAddr("2001:db8:1:3::1")
	if server.RateKey(a) != server.RateKey(b) {
		t.Fatal("two addresses in one /64 must share a bucket")
	}
	if server.RateKey(a) == server.RateKey(c) {
		t.Fatal("two different /64s must not share a bucket")
	}
}

// An IPv4-mapped IPv6 address (::ffff:a.b.c.d) is an IPv4 client. A reverse
// proxy may write that form into X-Forwarded-For — tls.mode = "behind_proxy" is
// a first-class deployment — so RateKey must reduce it to the v4 address.
// Keying it as IPv6 folds EVERY IPv4 client on the instance into the single /64
// key "::/64", and one abusive client then empties the login, register, invite
// and unauth buckets for all the others.
func TestMappedIPv4KeysAsIPv4(t *testing.T) {
	a := netip.MustParseAddr("::ffff:203.0.113.9")
	b := netip.MustParseAddr("::ffff:198.51.100.4")
	if got := server.RateKey(a); got != "203.0.113.9" {
		t.Fatalf("RateKey(%s) = %q, want %q", a, got, "203.0.113.9")
	}
	if server.RateKey(a) == server.RateKey(b) {
		t.Fatalf("two mapped IPv4 clients share the bucket key %q", server.RateKey(a))
	}
	if server.RateKey(a) != server.RateKey(netip.MustParseAddr("203.0.113.9")) {
		t.Fatal("the mapped and plain spellings of one address must share a bucket")
	}
}

// RealIP must unmap what it reads out of X-Forwarded-For too: the address it
// returns is fed straight to RateKey, and netip.Prefix.Contains is false across
// address families, so a trusted hop the proxy wrote as ::ffff:127.0.0.1 would
// otherwise be mistaken for the client.
func TestRealIPUnmapsForwardedAddresses(t *testing.T) {
	trusted := []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}
	r := httptest.NewRequestWithContext(t.Context(), "GET", "/", nil)
	r.RemoteAddr = "127.0.0.1:5000"
	r.Header.Set("X-Forwarded-For", "::ffff:203.0.113.9, ::ffff:127.0.0.1")
	got := server.RealIP(r, trusted)
	if !got.Is4() {
		t.Fatalf("RealIP = %s (Is4 = false); a mapped IPv4 address must come back unmapped", got)
	}
	if got.String() != "203.0.113.9" {
		t.Fatalf("RealIP = %s, want 203.0.113.9: the mapped proxy hop must be skipped", got)
	}
}
