package web

import (
	"net/netip"
	"slices"
	"strings"

	"github.com/cstupi/arr-trickle/internal/config"
)

func networks(values []string) []netip.Prefix {
	out := make([]netip.Prefix, 0, len(values))
	for _, v := range values {
		if p, err := config.ParseNetwork(v); err == nil {
			out = append(out, p)
		}
	}
	return out
}

// parseIP parses an address without its zone, unwrapping IPv4-mapped IPv6 (::ffff:192.168.1.5 from a
// dual-stack socket).
func parseIP(s string) (netip.Addr, bool) {
	a, err := netip.ParseAddr(strings.Trim(strings.TrimSpace(s), "[]"))
	return a.WithZone("").Unmap(), err == nil
}

func inAny(ip netip.Addr, nets []netip.Prefix) bool {
	return slices.ContainsFunc(nets, func(n netip.Prefix) bool { return n.Contains(ip) })
}

// clientIP is the real client address. X-Forwarded-For is only believed when the peer is a trusted
// proxy, and then only up to the first hop that isn't one (the rightmost untrusted entry).
// false means "deny".
func clientIP(peer, forwardedFor string, trusted []netip.Prefix) (netip.Addr, bool) {
	ip, ok := parseIP(peer)
	if !ok || forwardedFor == "" || !inAny(ip, trusted) {
		return ip, ok
	}
	for _, hop := range slices.Backward(strings.Split(forwardedFor, ",")) {
		if ip, ok = parseIP(hop); !ok || !inAny(ip, trusted) {
			return ip, ok // a malformed chain denies: don't guess
		}
	}
	return ip, true
}
