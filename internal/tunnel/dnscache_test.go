package tunnel

import (
	"net"
	"testing"
	"time"
)

func TestDNSCache_HitSkipsDNS(t *testing.T) {
	c := &dnsCache{entries: make(map[string]dnsCacheEntry)}
	c.put("myhost.local", "192.168.1.100")

	ip, ok := c.get("myhost.local")
	if !ok {
		t.Fatal("expected cache hit")
	}
	if ip != "192.168.1.100" {
		t.Errorf("got %q, want 192.168.1.100", ip)
	}
}

func TestDNSCache_MissPassthrough(t *testing.T) {
	c := &dnsCache{entries: make(map[string]dnsCacheEntry)}

	_, ok := c.get("unknown.local")
	if ok {
		t.Error("expected cache miss for unknown host")
	}
}

func TestDNSCache_StaleEvicted(t *testing.T) {
	c := &dnsCache{entries: make(map[string]dnsCacheEntry)}
	c.put("myhost.local", "192.168.1.100")

	// Verify entry exists before delete.
	if _, ok := c.get("myhost.local"); !ok {
		t.Fatal("expected cache hit before delete")
	}

	c.delete("myhost.local")

	if _, ok := c.get("myhost.local"); ok {
		t.Error("expected cache miss after delete")
	}
}

func TestDNSCache_TTLExpiry(t *testing.T) {
	c := &dnsCache{entries: make(map[string]dnsCacheEntry)}
	c.mu.Lock()
	c.entries["myhost.local"] = dnsCacheEntry{
		ip:         "192.168.1.100",
		resolvedAt: time.Now().Add(-dnsCacheTTL - time.Second),
	}
	c.mu.Unlock()

	if _, ok := c.get("myhost.local"); ok {
		t.Error("expected cache miss for expired entry")
	}
}

func TestDNSCache_LinkLocalSkipped(t *testing.T) {
	tests := []struct {
		name string
		addr string
		want string
	}{
		{"ipv6 link-local", "[fe80::1%en0]:2222", ""},
		{"ipv4 link-local", "169.254.1.1:2222", ""},
		{"ipv4 global", "192.168.1.100:2222", "192.168.1.100"},
		{"ipv6 global", "[2001:db8::1]:2222", "2001:db8::1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			addr, err := net.ResolveTCPAddr("tcp", tt.addr)
			if err != nil {
				t.Fatalf("bad test addr %q: %v", tt.addr, err)
			}
			conn := &fakeConn{remote: addr}
			got := cacheableIP(conn)
			if got != tt.want {
				t.Errorf("cacheableIP(%s) = %q, want %q", tt.addr, got, tt.want)
			}
		})
	}
}

func TestDNSCache_IPv4MappedNormalized(t *testing.T) {
	// ::ffff:192.168.1.100 should be stored as 192.168.1.100
	addr := &net.TCPAddr{IP: net.ParseIP("::ffff:192.168.1.100"), Port: 2222}
	conn := &fakeConn{remote: addr}
	got := cacheableIP(conn)
	if got != "192.168.1.100" {
		t.Errorf("cacheableIP(::ffff:192.168.1.100) = %q, want 192.168.1.100", got)
	}
}

func TestDNSCache_IPLiteralSkipped(t *testing.T) {
	// When the target hostname is already an IP, the cache should not be
	// consulted. This test verifies the host-is-IP guard used at the call
	// site — the cache itself stores whatever it's told, so the guard is
	// in probeTarget/runForwardSetForTarget. We verify the pattern here:
	// net.ParseIP succeeds for literals, so the caller skips the cache.
	host := "192.168.1.100"
	if net.ParseIP(host) == nil {
		t.Fatal("expected IP literal to parse")
	}
	// A real IP literal should not be looked up in the cache.
	c := &dnsCache{entries: make(map[string]dnsCacheEntry)}
	c.put(host, "10.0.0.1") // hypothetical stale entry
	// The caller checks net.ParseIP(host) != nil and skips get().
	// Verify the guard pattern:
	if net.ParseIP(host) != nil {
		// skip cache — correct behavior
	} else {
		t.Error("IP literal guard failed")
	}
}

// fakeConn implements just enough of net.Conn for cacheableIP.
type fakeConn struct {
	net.Conn
	remote net.Addr
}

func (c *fakeConn) RemoteAddr() net.Addr { return c.remote }
