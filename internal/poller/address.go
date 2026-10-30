package poller

import (
	"errors"
	"fmt"
	"net"
	"sync"
	"time"
)

var (
	// ErrNoIPv4 means a host has no IPv4 address. Only IPv4 is polled.
	ErrNoIPv4 = errors.New("no IPv4 address")
	// ErrNotPublic means a host resolves to a private, loopback or other non-public address.
	ErrNotPublic = errors.New("not a public address")
)

// nonPublic are IPv4 ranges that are not reachable on the internet but that net.IP does not
// flag: "this network", carrier-grade NAT and benchmarking.
var nonPublic = []*net.IPNet{cidr("0.0.0.0/8"), cidr("100.64.0.0/10"), cidr("198.18.0.0/15")}

func cidr(s string) *net.IPNet {
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		panic(err)
	}
	return n
}

// Public reports whether ip is a public unicast address, which the bot may send packets to.
func Public(ip net.IP) bool {
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return false
	}
	for _, n := range nonPublic {
		if n.Contains(ip) {
			return false
		}
	}
	return true
}

// PublicIPv4 picks the address to poll from what a host resolves to: its first IPv4 address. It
// fails when there is none, or when any address, IPv4 or IPv6, is not public.
func PublicIPv4(ips []net.IP) (net.IP, error) {
	var v4 net.IP
	for _, ip := range ips {
		if !Public(ip) {
			return nil, ErrNotPublic
		}
		if v4 == nil && ip.To4() != nil {
			v4 = ip.To4()
		}
	}
	if v4 == nil {
		return nil, ErrNoIPv4
	}
	return v4, nil
}

// PublicDialer wraps dial for game servers that people added from Discord. It resolves the host on
// every cycle and dials the IPv4 address it checked with PublicIPv4, so a host name that is changed
// to point into a private network after /server add is not polled (DNS rebinding). Addresses in
// trusted come from the operator's config file and are dialed as they are, since the operator may
// poll a game server on the same LAN.
func PublicDialer(dial Dialer, lookup func(host string) ([]net.IP, error), trusted []string) Dialer {
	trust := map[string]bool{}
	for _, a := range trusted {
		trust[a] = true
	}
	return func(addr string) (Client, error) {
		if trust[addr] {
			return dial(addr)
		}
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		ips := []net.IP{net.ParseIP(host)}
		if ips[0] == nil {
			if ips, err = lookup(host); err != nil {
				return nil, err
			}
		}
		ip, err := PublicIPv4(ips)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", host, err)
		}
		return dial(net.JoinHostPort(ip.String(), port))
	}
}

// LookupTTL is how long LookupCache keeps what a host name resolved to.
const LookupTTL = 10 * time.Minute

// LookupCache remembers what host names resolved to for TTL, so the name of a game server is not
// looked up again on every poll, and a short DNS outage does not count as the server failing.
// Failed lookups are not kept. A name that is changed to point elsewhere is picked up within TTL.
type LookupCache struct {
	Lookup func(host string) ([]net.IP, error)
	TTL    time.Duration    // default LookupTTL
	Now    func() time.Time // default time.Now

	mu sync.Mutex
	m  map[string]lookupEntry
}

type lookupEntry struct {
	ips []net.IP
	at  time.Time
}

// LookupIP returns what host resolved to less than TTL ago, or looks it up.
func (c *LookupCache) LookupIP(host string) ([]net.IP, error) {
	now, ttl := time.Now, c.TTL
	if c.Now != nil {
		now = c.Now
	}
	if ttl <= 0 {
		ttl = LookupTTL
	}
	c.mu.Lock()
	e, ok := c.m[host]
	c.mu.Unlock()
	if ok && now().Sub(e.at) < ttl {
		return e.ips, nil
	}
	ips, err := c.Lookup(host) // outside the lock, so one slow name does not hold up the others
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]lookupEntry{}
	}
	at := now()
	for h, old := range c.m { // names no longer polled do not stay forever
		if at.Sub(old.at) >= ttl {
			delete(c.m, h)
		}
	}
	c.m[host] = lookupEntry{ips: ips, at: at}
	return ips, nil
}
