package server

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"
)

func peerIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return "unknown"
	}
	return host
}
func secureRequest(r *http.Request, proxies []netip.Prefix, allowLocalHTTP bool) bool {
	if r.TLS != nil {
		return true
	}
	addr, err := netip.ParseAddr(peerIP(r))
	if err != nil {
		return false
	}
	addr = addr.Unmap()
	for _, prefix := range proxies {
		if prefix.Contains(addr) {
			return r.Header.Get("X-Forwarded-Proto") == "https"
		}
	}
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	local := host == "localhost"
	if ip, err := netip.ParseAddr(host); err == nil {
		local = ip.IsLoopback()
	}
	// Docker's localhost port publication reaches the container through a private
	// bridge. Opt in only for development with the host port bound to loopback.
	return (addr.IsLoopback() || (allowLocalHTTP && addr.IsPrivate())) && local && r.Header.Get("X-Forwarded-Proto") == ""
}

// Admin cookies are always Secure, including on localhost. Only an actual TLS
// connection or an explicitly trusted TLS-terminating proxy satisfies this.
func httpsRequest(r *http.Request, proxies []netip.Prefix) bool {
	if r.TLS != nil {
		return true
	}
	ip, err := netip.ParseAddr(peerIP(r))
	if err != nil {
		return false
	}
	for _, prefix := range proxies {
		if prefix.Contains(ip.Unmap()) {
			return r.Header.Get("X-Forwarded-Proto") == "https"
		}
	}
	return false
}

type bucket struct {
	count   int
	expires time.Time
}
type rateLimiter struct {
	mu       sync.Mutex
	entries  map[string]bucket
	limit    int
	maxPeers int
}

func newLimiter(c Limits) *rateLimiter {
	return &rateLimiter{entries: map[string]bucket{}, limit: c.RequestsPerMinute, maxPeers: c.MaxPeers}
}

// Bound both request rate and memory. Forwarded client IPs are deliberately not trusted.
func (l *rateLimiter) allow(ip string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	b, exists := l.entries[ip]
	if !exists && len(l.entries) >= l.maxPeers {
		for k, v := range l.entries {
			if !now.Before(v.expires) {
				delete(l.entries, k)
			}
		}
		if len(l.entries) >= l.maxPeers {
			return false
		}
	}
	if !now.Before(b.expires) {
		b = bucket{expires: now.Add(time.Minute)}
	}
	if b.count >= l.limit {
		return false
	}
	b.count++
	l.entries[ip] = b
	return true
}
