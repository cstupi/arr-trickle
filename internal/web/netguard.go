package web

import (
	"net/netip"
	"strings"

	"github.com/cstupi/arr-trickle/internal/config"
)

// Networks parses validated network strings.
func Networks(values []string) []netip.Prefix {
	out := make([]netip.Prefix, 0, len(values))
	for _, v := range values {
		if p, err := config.ParseNetwork(v); err == nil {
			out = append(out, p)
		}
	}
	return out
}

// ParseIP parses an address, unwrapping IPv4-mapped IPv6 (::ffff:192.168.1.5 from a dual-stack socket).
func ParseIP(s string) (netip.Addr, bool) {
	s = strings.Trim(strings.TrimSpace(s), "[]")
	if i := strings.IndexByte(s, '%'); i >= 0 { // zone
		s = s[:i]
	}
	a, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Addr{}, false
	}
	return a.Unmap(), true
}

// InAny reports whether ip is in one of nets.
func InAny(ip netip.Addr, nets []netip.Prefix) bool {
	for _, n := range nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// ClientIP is the real client address. X-Forwarded-For is only believed when the peer is a trusted
// proxy, and then only up to the first hop that isn't one (the rightmost untrusted entry).
// false means "deny".
func ClientIP(peer, forwardedFor string, trusted []netip.Prefix) (netip.Addr, bool) {
	ip, ok := ParseIP(peer)
	if !ok || forwardedFor == "" || !InAny(ip, trusted) {
		return ip, ok
	}
	hops := strings.Split(forwardedFor, ",")
	for i := len(hops) - 1; i >= 0; i-- {
		hop, ok := ParseIP(hops[i])
		if !ok {
			return netip.Addr{}, false // malformed chain: don't guess
		}
		ip = hop
		if !InAny(ip, trusted) {
			break
		}
	}
	return ip, true
}
