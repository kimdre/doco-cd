package main

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// requestIP returns the resolved client IP address for the given HTTP request,
// taking into account trusted proxy headers and networks.
func (h *handlerData) requestIP(r *http.Request) string {
	if h == nil || h.appConfig == nil {
		return r.RemoteAddr
	}

	return resolveRequestIP(
		r.RemoteAddr,
		strings.TrimSpace(h.appConfig.TrustedProxyHeader),
		r.Header,
		h.appConfig.TrustedProxyNetworks,
	)
}

// resolveRequestIP determines the client IP address from the request, considering trusted proxy headers and networks.
func resolveRequestIP(remoteAddr, trustedProxyHeader string, headers http.Header, trustedProxyNetworks []netip.Prefix) string {
	remoteIP, remotePort, ok := parseAddrWithOptionalPort(remoteAddr)
	if !ok {
		return remoteAddr
	}

	if !isTrustedProxy(remoteIP, trustedProxyNetworks) {
		return formatAddrWithOptionalPort(remoteIP, remotePort)
	}

	if trustedProxyHeader == "" || strings.EqualFold(trustedProxyHeader, "X-Forwarded-For") {
		if clientIP, ok := resolveForwardedClientIP(headers.Values("X-Forwarded-For"), headers.Values("Forwarded"), trustedProxyNetworks); ok {
			return formatAddrWithOptionalPort(clientIP, remotePort)
		}
	} else if clientIP, ok := parseHeaderIP(headers.Values(trustedProxyHeader)); ok {
		return formatAddrWithOptionalPort(clientIP, remotePort)
	}

	return formatAddrWithOptionalPort(remoteIP, remotePort)
}

// resolveForwardedClientIP attempts to determine the client IP address from the X-Forwarded-For and Forwarded headers,
// considering trusted proxy networks.
func resolveForwardedClientIP(xForwardedForValues, forwardedValues []string, trustedProxyNetworks []netip.Prefix) (netip.Addr, bool) {
	if clientIP, ok := resolveForwardedHeaderChain(parseXForwardedForValues(xForwardedForValues), trustedProxyNetworks); ok {
		return clientIP, true
	}

	if clientIP, ok := resolveForwardedHeaderChain(parseForwardedForValues(forwardedValues), trustedProxyNetworks); ok {
		return clientIP, true
	}

	return netip.Addr{}, false
}

// resolveForwardedHeaderChain processes a chain of IP addresses from a forwarded header and returns the first non-trusted proxy IP address.
func resolveForwardedHeaderChain(chain []netip.Addr, trustedProxyNetworks []netip.Prefix) (netip.Addr, bool) {
	if len(chain) == 0 {
		return netip.Addr{}, false
	}

	for i := len(chain) - 1; i >= 0; i-- {
		if !isTrustedProxy(chain[i], trustedProxyNetworks) {
			return chain[i], true
		}
	}

	return chain[0], true
}

// parseXForwardedForValues parses the X-Forwarded-For header values into a slice of netip.Addr.
func parseXForwardedForValues(values []string) []netip.Addr {
	return parseAddrValues(values, ",")
}

// parseForwardedForValues parses the Forwarded header values into a slice of netip.Addr, extracting the "for" parameter.
func parseForwardedForValues(values []string) []netip.Addr {
	addrs := make([]netip.Addr, 0)

	for _, value := range values {
		for element := range strings.SplitSeq(value, ",") {
			for param := range strings.SplitSeq(element, ";") {
				key, rawValue, ok := strings.Cut(param, "=")
				if !ok || !strings.EqualFold(strings.TrimSpace(key), "for") {
					continue
				}

				if addr, ok := parseHeaderAddr(rawValue); ok {
					addrs = append(addrs, addr)
				}
			}
		}
	}

	return addrs
}

// parseAddrValues splits the header values by the specified separator and parses each token into a netip.Addr.
func parseAddrValues(values []string, separator string) []netip.Addr {
	addrs := make([]netip.Addr, 0)

	for _, value := range values {
		for token := range strings.SplitSeq(value, separator) {
			if addr, ok := parseHeaderAddr(token); ok {
				addrs = append(addrs, addr)
			}
		}
	}

	return addrs
}

// parseHeaderIP attempts to parse the first valid IP address from the provided header values.
func parseHeaderIP(values []string) (netip.Addr, bool) {
	for _, value := range values {
		if addr, ok := parseHeaderAddr(value); ok {
			return addr, true
		}
	}

	return netip.Addr{}, false
}

// parseAddrWithOptionalPort parses an address string that may include an optional port,
// returning the netip.Addr, port string, and a boolean indicating success.
func parseAddrWithOptionalPort(value string) (netip.Addr, string, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return netip.Addr{}, "", false
	}

	if host, port, err := net.SplitHostPort(value); err == nil {
		if addr, ok := parseHeaderAddr(host); ok {
			return addr, port, true
		}
	}

	if addr, ok := parseHeaderAddr(value); ok {
		return addr, "", true
	}

	return netip.Addr{}, "", false
}

// parseHeaderAddr attempts to parse a single IP address from the provided string.
func parseHeaderAddr(value string) (netip.Addr, bool) {
	value = strings.TrimSpace(strings.Trim(value, "\""))
	if value == "" || value == "_" || strings.EqualFold(value, "unknown") {
		return netip.Addr{}, false
	}

	if host, _, err := net.SplitHostPort(value); err == nil {
		value = host
	}

	value = strings.Trim(value, "[]")

	addr, err := netip.ParseAddr(value)
	if err != nil {
		return netip.Addr{}, false
	}

	// Unmap 4-in-6 addresses (e.g. ::ffff:127.0.0.1) to their canonical IPv4
	// form so trust checks, chain resolution and log output are consistent.
	return addr.Unmap(), true
}

// isTrustedProxy checks if the given IP address belongs to any of the trusted proxy networks.
func isTrustedProxy(addr netip.Addr, trustedProxyNetworks []netip.Prefix) bool {
	if !addr.IsValid() {
		return false
	}

	// Unmap 4-in-6 addresses (e.g. ::ffff:127.0.0.1) so they can be matched
	// against IPv4 CIDRs; netip.Prefix.Contains treats them as a different
	// address family otherwise.
	addr = addr.Unmap()

	for _, trustedNetwork := range trustedProxyNetworks {
		if trustedNetwork.Contains(addr) {
			return true
		}
	}

	return false
}

// formatAddrWithOptionalPort formats the given IP address and optional port into a string suitable for use in headers or logs.
func formatAddrWithOptionalPort(addr netip.Addr, port string) string {
	if port == "" {
		return addr.String()
	}

	return net.JoinHostPort(addr.String(), port)
}
