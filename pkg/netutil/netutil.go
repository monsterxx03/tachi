// Package netutil provides network helpers shared across the project,
// including reserved-address detection for deciding whether a target is
// safe to send to an external service.
package netutil

import (
	"fmt"
	"net"
	"slices"
)

// LookupIP resolves a hostname to IP addresses. It is a variable so tests
// can stub DNS resolution without touching real networks.
var LookupIP = net.LookupIP

// IsReservedIP reports whether ip is a private, loopback, link-local,
// multicast, documentation, or otherwise non-publicly-routable address.
// Such addresses must never be sent to an external service (e.g. a cloud
// scraping API) — they could be an internal server, a cloud metadata
// endpoint, or simply unreachable from outside.
//
// nil (unknown) is treated as reserved, i.e. the conservative choice.
func IsReservedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}

	// Covered by the stdlib: RFC 1918 private + IPv6 ULA (IsPrivate),
	// loopback, link-local unicast/multicast, unspecified, multicast.
	if ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() {
		return true
	}

	// Additional reserved IPv4 ranges the stdlib does not cover.
	if v4 := ip.To4(); v4 != nil {
		// CGNAT (RFC 6598): 100.64.0.0/10
		if v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127 {
			return true
		}
		// TEST-NET-1/2/3 (RFC 5737): 192.0.2.0/24, 198.51.100.0/24, 203.0.113.0/24
		if (v4[0] == 192 && v4[1] == 0 && v4[2] == 2) ||
			(v4[0] == 198 && v4[1] == 51 && v4[2] == 100) ||
			(v4[0] == 203 && v4[1] == 0 && v4[2] == 113) {
			return true
		}
		// Benchmarking (RFC 2544): 198.18.0.0/15
		if v4[0] == 198 && (v4[1] == 18 || v4[1] == 19) {
			return true
		}
		// Reserved / future use (RFC 1112): 240.0.0.0/4
		if v4[0] >= 240 {
			return true
		}
		return false
	}

	// Documentation prefix (RFC 3849): 2001:db8::/32
	if len(ip) == net.IPv6len && ip[0] == 0x20 && ip[1] == 0x01 && ip[2] == 0x0d && ip[3] == 0xb8 {
		return true
	}

	return false
}

// HostIsReserved resolves host (an IP literal or hostname) and reports
// whether any of its addresses is reserved. A hostname that resolves to a
// mix of public and reserved addresses is treated as reserved (the
// conservative choice, since we cannot know which address an external
// service would actually reach). A resolution failure returns an error so
// callers can decide their own fallback.
func HostIsReserved(host string) (bool, error) {
	// Strip a port if present (e.g. "example.com:8080", "[::1]:80").
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}

	// IP literal — no DNS involved.
	if ip := net.ParseIP(host); ip != nil {
		return IsReservedIP(ip), nil
	}

	addrs, err := LookupIP(host)
	if err != nil {
		return false, fmt.Errorf("resolve %q: %w", host, err)
	}
	if len(addrs) == 0 {
		return false, fmt.Errorf("no addresses found for %q", host)
	}
	if slices.ContainsFunc(addrs, IsReservedIP) {
		return true, nil
	}
	return false, nil
}

// IsLoopbackHost reports whether host is a loopback hostname, without doing
// any DNS resolution. Unlike HostIsReserved it only recognizes the well-known
// loopback names/literals (localhost, 127.0.0.1, ::1) and is meant for cheap
// local-dev decisions such as not forcing an http→https upgrade.
func IsLoopbackHost(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}
