package fetch

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// hostEntry is an entry of rate_limits named name over hosts, its burst
// filled in as the load fills it.
func hostEntry(name string, perSecond float64, burst int, hosts ...string) model.HostRateLimit {
	return model.HostRateLimit{Name: name, Hosts: hosts, RateLimit: model.RateLimitConfig{RequestsPerSecond: perSecond, Burst: &burst}}
}

// An entry matches its hosts in any case, with or without the dot that may
// end a name, and on every port and scheme; a wildcard matches the
// subdomains of its name at any depth but not the name itself; an address
// matches however it is written; and the first entry that matches applies.
func TestHostRateLimitsMatchTheirHosts(t *testing.T) {
	freshRateLimits(t)
	FollowHostRateLimits([]model.HostRateLimit{
		hostEntry("billing", 1, 1, "Billing.Example.com", "2001:DB8::1"),
		hostEntry("example", 1, 1, "*.example.com", "[2001:db8::2]", "10.0.0.1"),
	})
	for raw, want := range map[string]string{
		"https://billing.example.com/v1":       "billing",
		"http://BILLING.example.com.:8080/":    "billing",
		"http://[2001:db8:0::1]:9090/":         "billing",
		"https://api.example.com/":             "example",
		"https://a.b.example.com/":             "example",
		"http://[2001:DB8::2]/":                "example",
		"http://10.0.0.1:81/":                  "example",
		"https://example.com/":                 "",
		"https://notexample.com/":              "",
		"https://example.com.evil.example.org": "",
		"http://10.0.0.2/":                     "",
	} {
		rule, _ := matchHostRateLimit(mustParseURL(t, raw))
		got := ""
		if rule != nil {
			got = rule.key.collector
		}
		if got != want {
			t.Errorf("%s: matched %q, want %q", raw, got, want)
		}
	}
}

// An entry matches every spelling of its host a request may use: an IPv4
// address mapped into IPv6, which Go dials as the IPv4 address, matches the
// IPv4 entry and the other way round; an IPv6 address with a zone matches
// the entry of the address; and an internationalised name, which a request
// names in its xn-- form, matches in any case and with a final dot.
func TestHostRateLimitsMatchEverySpellingOfAHost(t *testing.T) {
	freshRateLimits(t)
	FollowHostRateLimits([]model.HostRateLimit{
		hostEntry("plain", 1, 1, "10.0.0.1", "fe80::1", "XN--Bcher-Kva.example."),
		hostEntry("mapped", 1, 1, "::ffff:10.0.0.2", "[::FFFF:a00:3]", "*.xn--mnchen-3ya.example"),
	})
	for raw, want := range map[string]string{
		"http://10.0.0.1/":                         "plain",
		"http://[::ffff:10.0.0.1]:8080/":           "plain",
		"http://[::FFFF:a00:1]/":                   "plain",
		"http://[0:0:0:0:0:ffff:10.0.0.1]/":        "plain",
		"http://[fe80::1%25eth0]/":                 "plain",
		"http://[FE80::1%25En0]:9090/":             "plain",
		"http://[fe80::1]/":                        "plain",
		"http://xn--bcher-kva.example/":            "plain",
		"https://XN--BCHER-KVA.EXAMPLE.:8443/":     "plain",
		"http://10.0.0.2/":                         "mapped",
		"http://[::ffff:10.0.0.2]/":                "mapped",
		"http://10.0.0.3:81/":                      "mapped",
		"http://api.xn--mnchen-3ya.example/":       "mapped",
		"http://API.XN--MNCHEN-3YA.EXAMPLE./":      "mapped",
		"http://xn--mnchen-3ya.example/":           "",
		"http://[::10.0.0.1]/":                     "",
		"http://[fe80::2%25eth0]/":                 "",
		"http://xn--bcher-kva.example.example.org": "",
	} {
		rule, host := matchHostRateLimit(mustParseURL(t, raw))
		got := ""
		if rule != nil {
			got = rule.key.collector
		}
		if got != want {
			t.Errorf("%s: matched %q as %q, want %q", raw, got, host, want)
		}
	}
	if err := ValidateHostRateLimits([]model.HostRateLimit{hostEntry("ok", 1, 1, "::ffff:10.0.0.9", "[::ffff:10.0.0.8]")}); err != nil {
		t.Errorf("mapped addresses refused: %v", err)
	}
}

// Two spellings of one host are one host to the checks at load, as they are
// to a request: a mapped address after its IPv4 form is listed twice, and
// one under an earlier wildcard is covered. A name outside ASCII is refused,
// naming the character and the xn-- form to write, since no request a limit
// applies to names its host so; and an address with a zone is refused,
// since the zone names an interface here and not the host.
func TestHostRateLimitsKnowAHostUnderEverySpelling(t *testing.T) {
	err := ValidateHostRateLimits([]model.HostRateLimit{
		hostEntry("plain", 1, 1, "10.0.0.1", "xn--bcher-kva.example", "*.xn--mnchen-3ya.example"),
		hostEntry("mapped", 1, 1, "::ffff:10.0.0.1", "[::ffff:a00:1]"),
		hostEntry("unicode", 1, 1, "bücher.example", "*.münchen.example"),
		hostEntry("upper", 1, 1, "XN--BCHER-KVA.EXAMPLE.", "API.XN--MNCHEN-3YA.EXAMPLE"),
		hostEntry("zoned", 1, 1, "fe80::1%eth0", "[fe80::2%25eth0]"),
	})
	if err == nil {
		t.Fatal("loaded; want refused")
	}
	for _, want := range []string{
		`rate_limits entry "mapped" lists "::ffff:10.0.0.1", which rate_limits entry "plain" lists already`,
		`rate_limits entry "mapped" lists "[::ffff:a00:1]", which rate_limits entry "plain" lists already`,
		`rate_limits entry "unicode" hosts[0] "bücher.example" has U+00FC 'ü', a character outside ASCII: write an internationalised name in its ASCII form, as xn--bcher-kva.example for bücher.example`,
		`rate_limits entry "unicode" hosts[1] "*.münchen.example" has U+00FC 'ü', a character outside ASCII`,
		`rate_limits entry "upper" lists "XN--BCHER-KVA.EXAMPLE.", which rate_limits entry "plain" lists already`,
		`rate_limits entry "upper" lists "API.XN--MNCHEN-3YA.EXAMPLE", which rate_limits entry "plain"'s "*.xn--mnchen-3ya.example" covers already`,
		`rate_limits entry "zoned" hosts[0] "fe80::1%eth0" has the zone "eth0"; write the address without it, which matches it through every interface`,
		`rate_limits entry "zoned" hosts[1] "[fe80::2%25eth0]" has the zone "25eth0"`,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("no %q in:\n%v", want, err)
		}
	}
}

// A request takes a turn under the entry its host matches whatever its
// collector, as well as under its collector's own limit: one refused by the
// entry gives back the turn it took under its collector's, since it is not
// sent, and says which entry refused it.
func TestHostRateLimitsAreTakenWithTheCollectorsOwn(t *testing.T) {
	freshRateLimits(t)
	FollowHostRateLimits([]model.HostRateLimit{hostEntry("api", 1, 1, "api.example")})
	u := mustParseURL(t, "http://api.example:8080/")
	own := &rateLimitSetting{collector: "a", perSecond: 1, burst: 2}
	ctx := context.WithValue(context.Background(), rateLimitContextKey{}, own)
	if w, err := waitRateLimit(ctx, u); err != nil || w.waited != 0 {
		t.Fatalf("first request: waited %s, %v; want sent at once", w.waited, err)
	}
	soon, cancel := context.WithDeadline(ctx, rateLimitNow().Add(500*time.Millisecond))
	defer cancel()
	_, err := waitRateLimit(soon, u)
	if !errors.Is(err, errRateLimitRefused) || !strings.Contains(err.Error(), `rate limit of 1 request per second of rate_limits entry "api" for api.example would delay the request past its deadline: it would wait 1s for its turn`) {
		t.Fatalf("got %v; want the entry's refusal", err)
	}
	// The collector's bucket still holds the turn the refused request took.
	key := rateLimitKey{collector: "a", origin: "http://api.example:8080", perSecond: 1, burst: 2}
	if wait, _ := rateLimits.reserve(key, rateLimitNow(), time.Time{}, false, false); wait != 0 {
		t.Errorf("the collector's next turn is %s away; want it given back", wait)
	}
	// Another collector, without a limit of its own, waits on the entry.
	other := context.WithValue(context.Background(), rateLimitContextKey{}, &rateLimitSetting{collector: "b"})
	w, err := waitRateLimit(other, u)
	if err != nil || w.waited != time.Second || w.note != `sent after waiting 1s for its turn under the rate limit of 1 request per second of rate_limits entry "api" for api.example` {
		t.Errorf("waited %s (%q), %v; want 1s under the entry", w.waited, w.note, err)
	}
	counts := HostRateLimitCounts()
	if len(counts) != 1 || counts[0].Name != "api" || counts[0].Waited != 1 || counts[0].Refused != 1 {
		t.Errorf("counts %+v; want api with 1 wait and 1 refusal", counts)
	}
	// A request of no http or graphite trip carries no setting, and takes
	// no turn under the entry.
	if w, err := waitRateLimit(context.Background(), u); err != nil || w.waited != 0 {
		t.Errorf("a request of another type waited %s, %v; want no turn taken", w.waited, err)
	}
}

// A reload keeps the bucket of an entry it left as it was, with the turns
// it promised, and starts an entry it changed from a full bucket; an entry
// it removed is forgotten with its counts.
func TestHostRateLimitsFollowAReload(t *testing.T) {
	freshRateLimits(t)
	u := mustParseURL(t, "http://api.example/")
	ctx := context.WithValue(context.Background(), rateLimitContextKey{}, &rateLimitSetting{collector: "c"})
	take := func() time.Duration {
		t.Helper()
		w, err := waitRateLimit(ctx, u)
		if err != nil {
			t.Fatal(err)
		}
		return w.waited
	}
	FollowHostRateLimits([]model.HostRateLimit{hostEntry("api", 1, 1, "api.example")})
	take()
	FollowHostRateLimits([]model.HostRateLimit{hostEntry("other", 1, 1, "other.example"), hostEntry("api", 1, 1, "API.example")})
	if wait := take(); wait != time.Second {
		t.Errorf("after a reload that left the entry as it was, the request waited %s; want 1s, its bucket kept", wait)
	}
	for _, changed := range []model.HostRateLimit{
		hostEntry("api", 2, 1, "api.example"),
		hostEntry("api", 2, 2, "api.example"),
		hostEntry("api", 2, 2, "api.example", "*.api.example"),
	} {
		FollowHostRateLimits([]model.HostRateLimit{changed})
		if wait := take(); wait != 0 {
			t.Errorf("after a reload that changed the entry to %+v, the request waited %s; want it sent at once, from a full bucket", changed, wait)
		}
	}
	if counts := HostRateLimitCounts(); len(counts) != 1 || counts[0].Waited != 1 || !counts[0].Since.IsZero() {
		t.Errorf("counts %+v; want api's wait kept across the changes, counting since the start", counts)
	}
	FollowHostRateLimits(nil)
	if n := hostRateLimits.size(); n != 0 || len(HostRateLimitCounts()) != 0 {
		t.Errorf("%d buckets and counts %+v kept after the entry was removed; want none", n, HostRateLimitCounts())
	}
	FollowHostRateLimits([]model.HostRateLimit{hostEntry("api", 1, 1, "api.example")})
	if counts := HostRateLimitCounts(); len(counts) != 1 || counts[0].Waited != 0 || counts[0].Since.IsZero() {
		t.Errorf("counts %+v; want the entry brought back counting from zero since the reload", counts)
	}
	CloseConnections()
	if n := hostRateLimits.size(); n != 0 {
		t.Errorf("%d buckets kept after the shutdown; want none", n)
	}
}

// rate_limits is checked when it loads: every mistake reported, each
// naming its entry, and the burst filled in.
func TestHostRateLimitsAreCheckedWhenTheyLoad(t *testing.T) {
	zero := 0
	entries := []model.HostRateLimit{
		hostEntry("", 1, 1, "a.example"),
		hostEntry("bad-name", 1, 1, "b.example"),
		hostEntry("twice", 1, 1, "c.example"),
		hostEntry("twice", 1, 1, "d.example"),
		{Name: "no_hosts", RateLimit: model.RateLimitConfig{RequestsPerSecond: 1}},
		hostEntry("hosts", 1, 1, "api.example:443", "https://api.example", "a*.example", "*.10.0.0.1", "", "[api.example]", "under score_.example"),
		hostEntry("again", 1, 1, "C.EXAMPLE.", "*.e.example", "*.E.example"),
		{Name: "rate", Hosts: []string{"f.example"}},
		{Name: "burst", Hosts: []string{"g.example"}, RateLimit: model.RateLimitConfig{RequestsPerSecond: 1, Burst: &zero}},
	}
	err := ValidateHostRateLimits(entries)
	if err == nil {
		t.Fatal("loaded; want refused")
	}
	for _, want := range []string{
		`rate_limits entry 1 has no name`,
		`rate_limits entry "bad-name" has an invalid name`,
		`rate_limits entry "twice" is named twice`,
		`rate_limits entry "no_hosts" lists no hosts`,
		`rate_limits entry "hosts" hosts[0] "api.example:443" is not a host name or an IP address; write the host alone, without a scheme, port or path`,
		`rate_limits entry "hosts" hosts[1] "https://api.example" is not a host name`,
		`rate_limits entry "hosts" hosts[2] "a*.example" has a * other than a leading "*."`,
		`rate_limits entry "hosts" hosts[3] "*.10.0.0.1" is a wildcard over an IP address`,
		`rate_limits entry "hosts" hosts[4] "" is empty`,
		`rate_limits entry "hosts" hosts[5] "[api.example]" is not an IPv6 address in brackets`,
		`rate_limits entry "hosts" hosts[6] "under score_.example" is not a host name`,
		`rate_limits entry "again" lists "C.EXAMPLE.", which rate_limits entry "twice" lists already`,
		`rate_limits entry "again" lists "*.E.example", which rate_limits entry "again" lists already`,
		`rate_limits entry "rate" rate_limit.requests_per_second is 0; it is required`,
		`rate_limits entry "burst" rate_limit.burst is 0; it is how many requests may be sent at once after a pause, at least 1`,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("no %q in:\n%v", want, err)
		}
	}
	good := []model.HostRateLimit{{Name: "api", Hosts: []string{"api.example", "*.api.example", "10.0.0.1", "[2001:db8::1]"}, RateLimit: model.RateLimitConfig{RequestsPerSecond: 0.5}}}
	if err := ValidateHostRateLimits(good); err != nil {
		t.Fatal(err)
	}
	if b := good[0].RateLimit.Burst; b == nil || *b != 1 {
		t.Errorf("burst %v; want it filled in as 1", b)
	}
}

// A host an earlier entry's wildcard covers could never apply where it is
// listed, the first entry that matches applying: it is refused at load as
// the same host listed twice is, naming both entries. Listed before the
// wildcard it carves its host out, and within one entry it is harmless.
func TestAHostAnEarlierWildcardCoversIsRefused(t *testing.T) {
	err := ValidateHostRateLimits([]model.HostRateLimit{
		hostEntry("wild", 1, 1, "*.example.com"),
		hostEntry("exact", 1, 1, "API.example.com"),
		hostEntry("narrower", 1, 1, "*.api.example.com"),
		hostEntry("deep", 1, 1, "a.b.example.com", "example.com"),
	})
	if err == nil {
		t.Fatal("loaded; want refused")
	}
	for _, want := range []string{
		`rate_limits entry "exact" lists "API.example.com", which rate_limits entry "wild"'s "*.example.com" covers already; the first entry that matches applies, so rate_limits entry "exact" would never apply to it: list the narrower entry before the broader one`,
		`rate_limits entry "narrower" lists "*.api.example.com", which rate_limits entry "wild"'s "*.example.com" covers already`,
		`rate_limits entry "deep" lists "a.b.example.com", which rate_limits entry "wild"'s "*.example.com" covers already`,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("no %q in:\n%v", want, err)
		}
	}
	// example.com itself is not under *.example.com.
	if strings.Contains(err.Error(), `lists "example.com"`) {
		t.Errorf("example.com refused as covered by *.example.com:\n%v", err)
	}
	for _, entries := range [][]model.HostRateLimit{
		{hostEntry("exact", 1, 1, "api.example.com"), hostEntry("wild", 1, 1, "*.example.com")},
		{hostEntry("narrower", 1, 1, "*.api.example.com"), hostEntry("wild", 1, 1, "*.example.com")},
		{hostEntry("one", 1, 1, "*.example.com", "api.example.com", "*.api.example.com")},
		{hostEntry("one", 1, 1, "api.example.com", "*.example.com")},
	} {
		if err := ValidateHostRateLimits(entries); err != nil {
			t.Errorf("%+v refused: %v", entries, err)
		}
	}
}
