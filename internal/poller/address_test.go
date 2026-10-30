package poller

import (
	"errors"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"
)

func TestPublic(t *testing.T) {
	for _, tc := range []struct {
		ip   string
		want bool
	}{
		{"93.184.216.34", true}, {"2606:2800:220:1::1", true},
		{"127.0.0.1", false}, {"10.0.0.1", false}, {"172.16.0.1", false}, {"192.168.1.5", false},
		{"169.254.1.1", false}, {"0.0.0.0", false}, {"0.1.2.3", false}, {"255.255.255.255", false},
		{"100.64.0.1", false}, {"100.127.255.254", false}, {"198.18.0.1", false}, {"198.19.255.254", false},
		{"224.0.0.1", false}, {"::1", false}, {"fd00::1", false}, {"fe80::1", false}, {"::ffff:10.0.0.1", false},
	} {
		if got := Public(net.ParseIP(tc.ip)); got != tc.want {
			t.Errorf("Public(%s) = %v, want %v", tc.ip, got, tc.want)
		}
	}
}

func TestPublicIPv4(t *testing.T) {
	ips := func(s ...string) (out []net.IP) {
		for _, v := range s {
			out = append(out, net.ParseIP(v))
		}
		return out
	}
	if ip, err := PublicIPv4(ips("2606:2800:220:1::1", "93.184.216.34")); err != nil || ip.String() != "93.184.216.34" {
		t.Errorf("got %v, %v: want the IPv4 address", ip, err)
	}
	if _, err := PublicIPv4(ips("2606:2800:220:1::1")); !errors.Is(err, ErrNoIPv4) {
		t.Errorf("IPv6 only: %v, want ErrNoIPv4", err)
	}
	if _, err := PublicIPv4(nil); !errors.Is(err, ErrNoIPv4) {
		t.Errorf("no address: %v, want ErrNoIPv4", err)
	}
	// One private address is enough to refuse the host, also an IPv6 one that is never dialed.
	for _, set := range [][]net.IP{ips("::1"), ips("93.184.216.34", "fd00::1"), ips("93.184.216.34", "10.0.0.1")} {
		if _, err := PublicIPv4(set); !errors.Is(err, ErrNotPublic) {
			t.Errorf("%v: %v, want ErrNotPublic", set, err)
		}
	}
}

func TestPublicDialerChecksTheAddressOnEveryPoll(t *testing.T) {
	var dialed []string
	dial := func(addr string) (Client, error) { dialed = append(dialed, addr); return &fake{}, nil }
	answer := map[string][]net.IP{"game.example": {net.ParseIP("93.184.216.34")}, "v6.example": {net.ParseIP("2606:2800:220:1::1")}}
	lookups := 0
	lookup := func(host string) ([]net.IP, error) {
		lookups++
		if ips, ok := answer[host]; ok {
			return ips, nil
		}
		return nil, errors.New("no such host")
	}
	d := PublicDialer(dial, lookup, []string{"lan.example:27015", "192.168.1.5:27016"})

	// A public host is dialed by the IPv4 address that was checked.
	if _, err := d("game.example:27015"); err != nil {
		t.Fatal(err)
	}
	// The name now points into a private network: the next poll refuses it without dialing.
	answer["game.example"] = []net.IP{net.ParseIP("10.0.0.5")}
	if _, err := d("game.example:27015"); !errors.Is(err, ErrNotPublic) {
		t.Fatalf("after the change: %v, want ErrNotPublic", err)
	}
	if lookups != 2 {
		t.Errorf("%d lookups, want one per poll", lookups)
	}
	if _, err := d("v6.example:27015"); !errors.Is(err, ErrNoIPv4) {
		t.Errorf("IPv6 only: %v, want ErrNoIPv4", err)
	}
	if _, err := d("100.64.0.1:27015"); !errors.Is(err, ErrNotPublic) {
		t.Errorf("CGNAT address: %v, want ErrNotPublic", err)
	}
	if _, err := d("nowhere.example:27015"); err == nil {
		t.Error("a host that does not resolve should fail")
	}
	// The operator's own servers are dialed as they are, also on a LAN.
	for _, a := range []string{"lan.example:27015", "192.168.1.5:27016"} {
		if _, err := d(a); err != nil {
			t.Errorf("trusted %s: %v", a, err)
		}
	}
	want := []string{"93.184.216.34:27015", "lan.example:27015", "192.168.1.5:27016"}
	if len(dialed) != len(want) {
		t.Fatalf("dialed %v, want %v", dialed, want)
	}
	for i := range want {
		if dialed[i] != want[i] {
			t.Fatalf("dialed %v, want %v", dialed, want)
		}
	}
}

func TestLookupCacheKeepsAnswersForTheTTL(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	calls := map[string]int{}
	fail := false
	c := &LookupCache{
		Lookup: func(host string) ([]net.IP, error) {
			calls[host]++
			if fail {
				return nil, errors.New("dns down")
			}
			return []net.IP{net.ParseIP("93.184.216.34")}, nil
		},
		Now: func() time.Time { return now },
	}
	for i := 0; i < 3; i++ {
		if ips, err := c.LookupIP("game.example"); err != nil || len(ips) != 1 {
			t.Fatalf("%v %v", ips, err)
		}
		now = now.Add(time.Minute)
	}
	if calls["game.example"] != 1 {
		t.Fatalf("%d lookups within the TTL, want 1", calls["game.example"])
	}
	now = now.Add(LookupTTL)
	c.LookupIP("game.example")
	if calls["game.example"] != 2 {
		t.Fatalf("%d lookups after the TTL, want 2", calls["game.example"])
	}

	// A failed lookup is not kept: the next poll asks again.
	fail = true
	if _, err := c.LookupIP("other.example"); err == nil {
		t.Fatal("want the lookup error")
	}
	fail = false
	if _, err := c.LookupIP("other.example"); err != nil || calls["other.example"] != 2 {
		t.Fatalf("err %v after %d lookups, want a second lookup", err, calls["other.example"])
	}

	// Names that are no longer asked for are dropped once they expire.
	now = now.Add(LookupTTL)
	c.LookupIP("third.example")
	if len(c.m) != 1 {
		t.Errorf("%d names kept, want only the fresh one", len(c.m))
	}
}

func TestLookupCacheIsSafeForEveryPollingGoroutine(t *testing.T) {
	c := &LookupCache{Lookup: func(string) ([]net.IP, error) { return []net.IP{net.ParseIP("93.184.216.34")}, nil }}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				if _, err := c.LookupIP(fmt.Sprintf("s%d.example", (i+j)%7)); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()
}
