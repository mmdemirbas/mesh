package tunnel

import (
	"net"
	"sync"
	"time"
)

const dnsCacheTTL = 5 * time.Minute

// resolvedAddrCache caches hostname→IP mappings from successful TCP connections.
// Used as a fallback when DNS (especially mDNS for .local hostnames) fails for
// extended periods. DNS always goes first; the cache is tried only after DNS fails.
var resolvedAddrCache = &dnsCache{entries: make(map[string]dnsCacheEntry)}

type dnsCacheEntry struct {
	ip         string
	resolvedAt time.Time
}

type dnsCache struct {
	mu      sync.RWMutex
	entries map[string]dnsCacheEntry
}

func (c *dnsCache) get(host string) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	e, ok := c.entries[host]
	if !ok || time.Since(e.resolvedAt) > dnsCacheTTL {
		return "", false
	}
	return e.ip, true
}

func (c *dnsCache) put(host, ip string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[host] = dnsCacheEntry{ip: ip, resolvedAt: time.Now()}
}

func (c *dnsCache) delete(host string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, host)
}

// cacheableIP extracts the remote IP from a connection and returns it in
// canonical form suitable for caching. Returns "" if the address should not
// be cached: link-local addresses (unstable zone IDs, single-family locking)
// and IP literals (no DNS to skip).
func cacheableIP(conn net.Conn) string {
	host, _, err := net.SplitHostPort(conn.RemoteAddr().String())
	if err != nil {
		return ""
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return ""
	}
	if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return ""
	}
	if v4 := ip.To4(); v4 != nil {
		return v4.String()
	}
	return ip.String()
}
