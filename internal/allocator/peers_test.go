package allocator

import (
	"context"
	"errors"
	"net/netip"
	"slices"
	"sort"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// The copies' addresses are ordered by address and then port, IPv4 before
// IPv6, numerically rather than as text, so 10.0.0.9 comes before 10.0.0.10
// on every copy; the leader is the lowest of the live copies and this one,
// which counts as the highest while its own address is not known.
func TestPeersAreOrderedAndTheLowestLiveCopyLeads(t *testing.T) {
	addrs := []netip.AddrPort{
		netip.MustParseAddrPort("[fd00::1]:9116"),
		netip.MustParseAddrPort("10.0.0.10:9116"),
		netip.MustParseAddrPort("10.0.0.10:80"),
		netip.MustParseAddrPort("10.0.0.9:9116"),
		netip.MustParseAddrPort("[::1]:9116"),
		netip.MustParseAddrPort("192.168.0.1:9116"),
	}
	sort.Slice(addrs, func(i, j int) bool { return comparePeers(addrs[i], addrs[j]) < 0 })
	want := []string{"10.0.0.9:9116", "10.0.0.10:80", "10.0.0.10:9116", "192.168.0.1:9116", "[::1]:9116", "[fd00::1]:9116"}
	var got []string
	for _, a := range addrs {
		got = append(got, a.String())
	}
	if !slices.Equal(got, want) {
		t.Fatalf("ordered %v, want %v", got, want)
	}

	self := netip.MustParseAddrPort("10.0.0.10:9116")
	for what, c := range map[string]struct {
		self    netip.AddrPort
		others  []string
		leader  string
		leading bool
	}{
		"alone":                        {self, nil, "10.0.0.10:9116", true},
		"lower than every live peer":   {self, []string{"10.0.0.11:9116", "[fd00::1]:9116"}, "10.0.0.10:9116", true},
		"a live peer lower":            {self, []string{"10.0.0.9:9116", "10.0.0.11:9116"}, "10.0.0.9:9116", false},
		"its own address unknown":      {netip.AddrPort{}, []string{"10.0.0.11:9116"}, "10.0.0.11:9116", false},
		"unknown, and no peer is live": {netip.AddrPort{}, nil, "invalid AddrPort", true},
	} {
		var others []netip.AddrPort
		for _, o := range c.others {
			others = append(others, netip.MustParseAddrPort(o))
		}
		leader, leading := chooseLeader(c.self, others)
		if leader.String() != c.leader || leading != c.leading {
			t.Errorf("%s: the leader is %s, leading %v; want %s, %v", what, leader, leading, c.leader, c.leading)
		}
	}
}

// --peers names a name resolved to every address it has, IPv4 addresses
// mapped into IPv6 written as IPv4, or an IP address taken as it is; the
// copy's own address is among them whether or not the name gives it, and a
// lookup that fails keeps the addresses the name gave last.
func TestPeersAreResolvedEveryRound(t *testing.T) {
	var answer []string
	var failure error
	a := New(nil, Options{Logger: testutil.QuietLogger(t)})
	a.UsePeers(PeerOptions{
		Peers: []string{"pue-allocator.monitoring.svc:9116", "10.0.0.5:9116", "[::ffff:10.0.0.4]:9116"},
		Self:  netip.MustParseAddrPort("10.0.0.20:9116"),
		Resolve: func(_ context.Context, host string) ([]string, error) {
			if host != "pue-allocator.monitoring.svc" {
				t.Errorf("looked %q up", host)
			}
			return answer, failure
		},
	})
	resolved := func() []string {
		var out []string
		for _, addr := range a.peers.resolve(context.Background()) {
			out = append(out, addr.String())
		}
		return out
	}
	answer = []string{"10.0.0.9", "::ffff:10.0.0.3", "fd00::1", "10.0.0.5"}
	want := []string{"10.0.0.3:9116", "10.0.0.4:9116", "10.0.0.5:9116", "10.0.0.9:9116", "10.0.0.20:9116", "[fd00::1]:9116"}
	if got := resolved(); !slices.Equal(got, want) {
		t.Fatalf("resolved %v, want %v", got, want)
	}
	answer, failure = nil, errors.New("no such host")
	if got := resolved(); !slices.Equal(got, want) {
		t.Errorf("a failed lookup resolved %v, want the addresses of the last, %v", got, want)
	}
	answer, failure = []string{"10.0.0.9"}, nil
	want = []string{"10.0.0.4:9116", "10.0.0.5:9116", "10.0.0.9:9116", "10.0.0.20:9116"}
	if got := resolved(); !slices.Equal(got, want) {
		t.Errorf("resolved %v, want %v", got, want)
	}
}

// --peers and --peers.self are read strictly: an entry needs a host and a
// port, and this copy's address is an IP address, taking the listen
// address's port when it names none.
func TestPeerFlagsAreParsed(t *testing.T) {
	if got, err := ParsePeers("pue-allocator:9116, 10.0.0.5:9116,[fd00::1]:9116"); err != nil || !slices.Equal(got, []string{"pue-allocator:9116", "10.0.0.5:9116", "[fd00::1]:9116"}) {
		t.Errorf("got %v, %v", got, err)
	}
	for _, bad := range []string{"pue-allocator", "pue-allocator:http", "a:9116,,b:9116", ":9116", "a:0", "a:70000"} {
		if _, err := ParsePeers(bad); err == nil {
			t.Errorf("--peers %q was taken", bad)
		}
	}
	for raw, want := range map[string]string{
		"10.0.3.7":        "10.0.3.7:9116",
		"10.0.3.7:8000":   "10.0.3.7:8000",
		"fd00::7":         "[fd00::7]:9116",
		"[fd00::7]:8000":  "[fd00::7]:8000",
		"::ffff:10.0.3.7": "10.0.3.7:9116",
	} {
		if got, err := ParseSelf(raw, ":9116"); err != nil || got.String() != want {
			t.Errorf("--peers.self %q is %v, %v; want %s", raw, got, err, want)
		}
	}
	for _, bad := range []string{"pue-allocator-0", "pue-allocator-0:9116", "10.0.3.7:0", ""} {
		if _, err := ParseSelf(bad, ":9116"); err == nil {
			t.Errorf("--peers.self %q was taken", bad)
		}
	}
	if _, err := ParseSelf("10.0.3.7", "localhost:http"); err == nil {
		t.Error("--peers.self without a port was taken beside a listen address of no numeric port")
	}
}
