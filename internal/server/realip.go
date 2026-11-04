package server

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// RealIP returns the client address. X-Forwarded-For is honoured ONLY when the
// immediate peer is inside one of the configured trusted prefixes; otherwise a
// client could name its own address and walk around every per-address bucket.
func RealIP(r *http.Request, trusted []netip.Prefix) netip.Addr {
	peer := peerAddr(r)
	if len(trusted) == 0 || !inAny(peer, trusted) {
		return peer
	}
	xff := r.Header.Get("X-Forwarded-For")
	if xff == "" {
		return peer
	}
	parts := strings.Split(xff, ",")
	for i := len(parts) - 1; i >= 0; i-- {
		addr, err := netip.ParseAddr(strings.TrimSpace(parts[i]))
		if err != nil {
			continue
		}
		// Unmap before anything looks at the address. A proxy is free to write
		// an IPv4 client as ::ffff:a.b.c.d, and netip.Prefix.Contains is false
		// across address families: without this, a trusted hop spelled
		// ::ffff:127.0.0.1 reads as the client, and the address handed back is
		// keyed as IPv6 by RateKey.
		addr = addr.Unmap()
		if inAny(addr, trusted) {
			continue // another hop of our own proxy chain
		}
		return addr
	}
	return peer
}

func peerAddr(r *http.Request) netip.Addr {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}
	}
	return addr.Unmap()
}

func inAny(a netip.Addr, prefixes []netip.Prefix) bool {
	for _, p := range prefixes {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// RateKey is the bucket key for an address: IPv4 by address, IPv6 by /64. A
// residential IPv6 customer holds a /56 or a /64, so keying by /128 would give
// one household unlimited buckets.
func RateKey(a netip.Addr) string {
	if !a.IsValid() {
		return "invalid"
	}
	// ::ffff:a.b.c.d is an IPv4 client wearing an IPv6 spelling. Unmapped, it
	// keys as itself; left mapped, its /64 is "::/64" for EVERY IPv4 address on
	// earth, and one abusive client would share — and empty — a single bucket
	// per class with every other IPv4 client of the instance.
	a = a.Unmap()
	if a.Is4() {
		return a.String()
	}
	p, err := a.Prefix(64)
	if err != nil {
		return a.String()
	}
	return p.String()
}

// RatePrefixKey is the coarser key of a network's buckets (wave B task 3): an IPv4 address by its /24, an IPv6 one by
// its /48, so one network spreading requests over many addresses still meets one bucket. Mapped IPv4 is unmapped first,
// as in RateKey.
func RatePrefixKey(a netip.Addr) string {
	if !a.IsValid() {
		return "invalid"
	}
	a = a.Unmap()
	bits := 48
	if a.Is4() {
		bits = 24
	}
	p, err := a.Prefix(bits)
	if err != nil {
		return a.String()
	}
	return "net\x00" + p.String()
}
