package allocator

import (
	"context"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// dnsSource is a dns_sd source of names asking s, with what the test
// changes of it, as a load would check it.
func dnsSource(t *testing.T, s *testutil.DNSStub, limit int, edit func(*DNSSDConfig), names ...string) (*Allocator, *sourceState) {
	t.Helper()
	d := DiscoveryConfig{DNSSD: &DNSSDConfig{Names: names, Server: s.Addr}, Labels: map[string]string{"service": dnsNameLabel}}
	if edit != nil {
		edit(d.DNSSD)
	}
	return testSource(t, d, limit)
}

// srvRecords are the SRV records of two services, one target being
// behind both, and of a name that has an A record alone.
func srvRecords() []testutil.DNSRecord {
	return []testutil.DNSRecord{
		testutil.DNSSRV("_metrics._tcp.b.test", 10, 5, 9100, "Node-B.test"),
		testutil.DNSSRV("_metrics._tcp.b.test", 0, 1, 9100, "node-a.test"),
		testutil.DNSSRV("_metrics._tcp.b.test", 0, 1, 9100, "node-a.test"),
		// RFC 2782's "no such service here".
		testutil.DNSSRV("_metrics._tcp.b.test", 0, 0, 0, "."),
		testutil.DNSSRV("_metrics._tcp.a.test", 0, 0, 9200, "node-c.test"),
		testutil.DNSSRV("_metrics._tcp.a.test", 0, 0, 9100, "node-a.test"),
		testutil.DNSA("plain.test", "192.0.2.9"),
	}
}

// A dns_sd source of type SRV makes a target of each record's target host,
// without its trailing dot and in lower case, and port, with the name asked
// and the record's target and port as meta labels. A target under several
// names, or twice under one, is taken once with the labels of the first
// name listed; a name that does not exist, or has no SRV record, gives no
// targets and fails nothing; and the targets are sorted by address.
func TestDNSSDFindsTheTargetsOfSRVRecords(t *testing.T) {
	stub := testutil.NewDNSStub(t, srvRecords()...)
	a, s := dnsSource(t, stub, 10, nil, "_metrics._tcp.b.test", "_metrics._tcp.a.test", "_metrics._tcp.missing.test", "plain.test")
	if !a.refreshSource(context.Background(), s) || s.failures != 0 {
		t.Fatalf("the source did not answer: %q", s.lastErr)
	}
	if want := []string{"node-a.test:9100", "node-b.test:9100", "node-c.test:9200"}; !slices.Equal(addresses(s.found), want) {
		t.Fatalf("found %v, want %v", addresses(s.found), want)
	}
	want := []map[string]string{
		{dnsNameLabel: "_metrics._tcp.b.test", dnsSRVTargetLabel: "node-a.test", dnsSRVPortLabel: "9100", "service": "_metrics._tcp.b.test"},
		{dnsNameLabel: "_metrics._tcp.b.test", dnsSRVTargetLabel: "node-b.test", dnsSRVPortLabel: "9100", "service": "_metrics._tcp.b.test"},
		{dnsNameLabel: "_metrics._tcp.a.test", dnsSRVTargetLabel: "node-c.test", dnsSRVPortLabel: "9200", "service": "_metrics._tcp.a.test"},
	}
	for i := range want {
		if !maps.Equal(s.found[i].Labels, want[i]) {
			t.Errorf("%s has the labels %v, want %v", s.found[i].Address, s.found[i].Labels, want[i])
		}
	}
	// Absolute names are asked, so no search domain is appended.
	for _, q := range stub.Questions() {
		if !strings.HasSuffix(strings.Fields(q)[0], ".test.") || strings.Fields(q)[1] != "SRV" {
			t.Errorf("the server was asked %q", q)
		}
	}
}

// A dns_sd source of type A or AAAA makes a target of each address at its
// port, an IPv6 address in brackets, with the name asked as a meta label
// and no SRV labels.
func TestDNSSDFindsTheTargetsOfAddressRecords(t *testing.T) {
	stub := testutil.NewDNSStub(t,
		testutil.DNSA("web.test", "192.0.2.2"), testutil.DNSA("web.test", "192.0.2.10"),
		testutil.DNSAAAA("web.test", "2001:db8::1"), testutil.DNSA("db.test", "192.0.2.2"),
	)
	for _, c := range []struct {
		recordType string
		want       []string
	}{
		{"A", []string{"192.0.2.10:9100", "192.0.2.2:9100"}},
		{"AAAA", []string{"[2001:db8::1]:9100"}},
	} {
		a, s := dnsSource(t, stub, 10, func(n *DNSSDConfig) { n.Type, n.Port = c.recordType, 9100 }, "web.test", "db.test", "nothing.test")
		if !a.refreshSource(context.Background(), s) {
			t.Fatalf("%s: the source did not answer: %q", c.recordType, s.lastErr)
		}
		if !slices.Equal(addresses(s.found), c.want) {
			t.Errorf("%s: found %v, want %v", c.recordType, addresses(s.found), c.want)
		}
		for _, d := range s.found {
			if want := map[string]string{dnsNameLabel: "web.test", "service": "web.test"}; !maps.Equal(d.Labels, want) {
				t.Errorf("%s: %s has the labels %v, want %v", c.recordType, d.Address, d.Labels, want)
			}
		}
	}
}

// The same records in another order make the same answer, which is no
// change; a change of the records is one.
func TestDNSSDAnswersAlikeWhateverTheRecordsOrder(t *testing.T) {
	records := srvRecords()
	stub := testutil.NewDNSStub(t, records...)
	a, s := dnsSource(t, stub, 10, nil, "_metrics._tcp.a.test", "_metrics._tcp.b.test")
	ctx := context.Background()
	if !a.refreshSource(ctx, s) {
		t.Fatalf("the source did not answer: %q", s.lastErr)
	}
	first := s.found
	slices.Reverse(records)
	stub.SetRecords(records...)
	for range 5 {
		if a.refreshSource(ctx, s) {
			t.Fatalf("the records in another order changed what was found: %+v, then %+v", first, s.found)
		}
	}
	stub.SetRecords(append(records, testutil.DNSSRV("_metrics._tcp.a.test", 0, 0, 9100, "node-d.test"))...)
	if !a.refreshSource(ctx, s) || !slices.Contains(addresses(s.found), "node-d.test:9100") {
		t.Errorf("a record added: found %v", addresses(s.found))
	}
}

// A lookup that fails other than with a name not found, a server failure
// or a refusal of any one name, or a server that does not answer within
// the timeout, fails the whole answer: the targets of the last good one
// stay, and the failure is counted once while it stays the same.
func TestDNSSDKeepsItsLastGoodAnswerWhenALookupFails(t *testing.T) {
	stub := testutil.NewDNSStub(t, srvRecords()...)
	a, s := dnsSource(t, stub, 10, func(n *DNSSDConfig) { n.RefreshInterval = model.Duration(time.Second) }, "_metrics._tcp.b.test", "_metrics._tcp.a.test")
	ctx := context.Background()
	if !a.refreshSource(ctx, s) {
		t.Fatalf("the source did not answer: %q", s.lastErr)
	}
	good := addresses(s.found)
	for i, c := range []struct {
		rcode  int
		silent bool
		want   string
	}{
		{rcode: 2, want: "looking up SRV _metrics._tcp.b.test on " + stub.Addr + ": server misbehaving"},
		{silent: true, want: "looking up SRV _metrics._tcp.b.test on " + stub.Addr + ": the server did not answer within the timeout, 1s"},
		{rcode: 5, want: "looking up SRV _metrics._tcp.b.test on " + stub.Addr + ": server misbehaving"},
	} {
		stub.SetRcode(c.rcode)
		stub.SetSilent(c.silent)
		if a.refreshSource(ctx, s) || !slices.Equal(addresses(s.found), good) {
			t.Errorf("rcode %d, silent %v: found %v, want the last good %v", c.rcode, c.silent, addresses(s.found), good)
		}
		if s.failures != uint64(i+1) || !strings.Contains(s.lastErr, c.want) {
			t.Errorf("rcode %d, silent %v: %d failures, the last %q; want %d, with %q", c.rcode, c.silent, s.failures, s.lastErr, i+1, c.want)
		}
	}
	stub.SetSilent(false)
	stub.SetRcode(0)
	if a.refreshSource(ctx, s) || s.lastErr != "" || s.successes != 2 {
		t.Errorf("the server answering again: changed, or %q with %d successes", s.lastErr, s.successes)
	}
}

// A source failing at its first lookup has never answered, and adds no
// targets.
func TestADNSSDSourceThatNeverAnsweredHasNoTargets(t *testing.T) {
	stub := testutil.NewDNSStub(t, srvRecords()...)
	stub.SetRcode(2)
	a, s := dnsSource(t, stub, 10, nil, "_metrics._tcp.a.test")
	a.sources["k"] = s
	if a.refreshSource(context.Background(), s) || s.ok || s.failures != 1 {
		t.Fatalf("ok %v, failures %d", s.ok, s.failures)
	}
	if found := a.foundBySource(); len(found) != 0 {
		t.Errorf("a source that never answered found %v", found)
	}
}

// More targets than max_targets, counted once each, are a failure.
func TestDNSSDHoldsToMaxTargets(t *testing.T) {
	stub := testutil.NewDNSStub(t, srvRecords()...)
	names := []string{"_metrics._tcp.b.test", "_metrics._tcp.a.test"}
	a, s := dnsSource(t, stub, 3, nil, names...)
	if !a.refreshSource(context.Background(), s) || len(s.found) != 3 {
		t.Fatalf("three targets under a bound of 3: found %v, %q", addresses(s.found), s.lastErr)
	}
	a, s = dnsSource(t, stub, 2, nil, names...)
	if a.refreshSource(context.Background(), s) || s.ok || !strings.Contains(s.lastErr, "more than the 2 targets max_targets allows") {
		t.Errorf("three targets over a bound of 2: ok %v, %q", s.ok, s.lastErr)
	}
}

// A dns_sd source is checked at load: names, each a DNS name listed once,
// the record type, a port for A and AAAA alone, the server and the timings;
// the type defaults to SRV, a name is written in lower case without its
// trailing dot, the server as host:port, and the source is named dns_sd_<n>.
func TestDNSSDIsCheckedAtLoad(t *testing.T) {
	ok := func() DiscoveryConfig {
		return DiscoveryConfig{DNSSD: &DNSSDConfig{Names: []string{"_metrics._tcp.Example.COM."}, Server: "10.0.0.53"}}
	}
	list := []DiscoveryConfig{{FileSD: &FileSDConfig{Files: []string{"a.json"}}}, ok()}
	if err := checkDiscovery(list, "/"); err != nil {
		t.Fatalf("a good source was refused: %v", err)
	}
	n := list[1].DNSSD
	if list[1].Name != "dns_sd_1" || n.Type != "SRV" || n.Names[0] != "_metrics._tcp.example.com" || n.Server != "10.0.0.53:53" {
		t.Errorf("got the name %q, type %q, names %q and server %q", list[1].Name, n.Type, n.Names, n.Server)
	}
	if list[1].refresh() != DefaultDNSSDRefresh || list[1].timeout() != DefaultDiscoveryTimeout || list[1].kind() != "dns_sd" {
		t.Errorf("got the refresh interval %s, timeout %s and kind %q", list[1].refresh(), list[1].timeout(), list[1].kind())
	}
	address := ok()
	address.DNSSD.Type, address.DNSSD.Port = "AAAA", 9100
	if err := checkDiscovery([]DiscoveryConfig{address}, "/"); err != nil {
		t.Errorf("an AAAA source with a port was refused: %v", err)
	}
	for name, c := range map[string]struct {
		mutate func(*DNSSDConfig)
		want   string
	}{
		"no names":            {func(n *DNSSDConfig) { n.Names = nil }, "dns_sd.names must not be empty"},
		"an empty name":       {func(n *DNSSDConfig) { n.Names = []string{" "} }, "dns_sd.names entry is empty"},
		"a name with a port":  {func(n *DNSSDConfig) { n.Names = []string{"example.com:53"} }, "not a DNS name"},
		"a URL":               {func(n *DNSSDConfig) { n.Names = []string{"dns://example.com"} }, "not a DNS name"},
		"an address":          {func(n *DNSSDConfig) { n.Names = []string{"192.0.2.1"} }, "is an IP address"},
		"a blank in a name":   {func(n *DNSSDConfig) { n.Names = []string{"a b.example"} }, "not a DNS name"},
		"a long label":        {func(n *DNSSDConfig) { n.Names = []string{strings.Repeat("a", 64) + ".example"} }, "from 1 to 63"},
		"an empty label":      {func(n *DNSSDConfig) { n.Names = []string{"a..example"} }, "from 1 to 63"},
		"a name twice":        {func(n *DNSSDConfig) { n.Names = []string{"a.example", "A.example."} }, `lists "a.example" twice`},
		"an unknown type":     {func(n *DNSSDConfig) { n.Type = "MX" }, "must be SRV, A or AAAA"},
		"a lower-case type":   {func(n *DNSSDConfig) { n.Type = "srv" }, "must be SRV, A or AAAA"},
		"a port for SRV":      {func(n *DNSSDConfig) { n.Port = 9100 }, "dns_sd.port is set for type SRV"},
		"no port for A":       {func(n *DNSSDConfig) { n.Type = "A" }, "dns_sd.port must be a port"},
		"a port out of range": {func(n *DNSSDConfig) { n.Type, n.Port = "AAAA", 65536 }, "dns_sd.port must be a port"},
		"a server URL":        {func(n *DNSSDConfig) { n.Server = "udp://10.0.0.53" }, "dns_sd.server"},
		"a server's bad port": {func(n *DNSSDConfig) { n.Server = "10.0.0.53:0" }, "dns_sd.server"},
		"a long timeout":      {func(n *DNSSDConfig) { n.Timeout = model.Duration(31 * time.Second) }, "dns_sd.timeout 31s is longer than its refresh_interval 30s"},
		"a short refresh":     {func(n *DNSSDConfig) { n.RefreshInterval = model.Duration(100 * time.Millisecond) }, "under the least"},
	} {
		d := ok()
		c.mutate(d.DNSSD)
		err := checkDiscovery([]DiscoveryConfig{d}, "/")
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want an error with %q", name, err, c.want)
		}
	}
	two := ok()
	two.HTTPSD = &HTTPSDConfig{URL: "https://cmdb.example/sd"}
	if err := checkDiscovery([]DiscoveryConfig{two}, "/"); err == nil || !strings.Contains(err.Error(), "exactly one of file_sd, http_sd, kubernetes and dns_sd") {
		t.Errorf("dns_sd beside http_sd: got %v", err)
	}
}
