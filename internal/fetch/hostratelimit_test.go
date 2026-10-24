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
