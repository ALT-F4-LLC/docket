package cli

import (
	"context"
	"fmt"
	"net/netip"
	"testing"
)

// mapResolver answers lookups from a fixed table and fails any host it does
// not hold, so a test cannot pass by reaching real DNS.
type mapResolver map[string][]netip.Addr

func (m mapResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	addrs, ok := m[host]
	if !ok {
		return nil, fmt.Errorf("mapResolver: no entry for %q", host)
	}
	return addrs, nil
}

func addrs(ips ...string) []netip.Addr {
	out := make([]netip.Addr, len(ips))
	for i, ip := range ips {
		out[i] = netip.MustParseAddr(ip)
	}
	return out
}

var testDaemonResolver = mapResolver{
	"remote.test": addrs("203.0.113.5"),
	"local.test":  addrs("127.0.0.1"),
	"local6.test": addrs("::1"),
	"localhost":   addrs("192.0.2.7"),
	"mixed.test":  addrs("127.0.0.1", "203.0.113.5"),
	"empty.test":  {},
}

// TestCheckDaemonURLRefusesNonLoopbackPlaintext: plaintext is judged by the
// addresses the host resolves to, never by the host string, so a name that
// looks local but resolves elsewhere is refused along with a remote one.
func TestCheckDaemonURLRefusesNonLoopbackPlaintext(t *testing.T) {
	for _, raw := range []string{
		"http://remote.test:8080",
		"http://localhost:8080",
		"http://mixed.test:8080",
		"http://empty.test:8080",
		"http://unresolvable.test:8080",
		"http://0.0.0.0:8080",
		"http://203.0.113.5:8080",
		"http://",
		"ws://local.test:8080",
	} {
		if err := checkDaemonURL(context.Background(), raw, testDaemonResolver); err == nil {
			t.Errorf("checkDaemonURL(%q) = nil, want a refusal", raw)
		}
	}
}

// TestCheckDaemonURLAcceptsLoopbackPlaintextAndHTTPS: plaintext is fine when
// every resolved address is loopback, and https is fine wherever it points.
func TestCheckDaemonURLAcceptsLoopbackPlaintextAndHTTPS(t *testing.T) {
	for _, raw := range []string{
		"http://local.test:8080",
		"HTTP://local6.test:8080",
		"http://127.0.0.1:8080",
		"http://[::ffff:127.0.0.1]:8080",
		"https://remote.test:8443",
	} {
		if err := checkDaemonURL(context.Background(), raw, testDaemonResolver); err != nil {
			t.Errorf("checkDaemonURL(%q) = %v, want nil", raw, err)
		}
	}
}
