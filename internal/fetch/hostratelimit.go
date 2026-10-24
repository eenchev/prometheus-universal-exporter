package fetch

import (
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// rate_limits bounds how fast the http and graphite requests of every
// collector together ask a host. request.rate_limit is a collector's own, so
// several collectors calling one API share nothing and may together exceed
// a quota the API keeps for the host, or for a caller across its hosts; an
// entry of rate_limits has one bucket that every request to one of its
// hosts takes a turn from, whichever collector, probe or static target
// sends it, as well as the turn it takes under its collector's own limit.
//
// A request counts as request.rate_limit counts it — each attempt, page,
// redirect hop and login request, against the host it is sent to — and
// waits, or is refused, by the same code. The first entry whose hosts match
// applies; one host may not be listed twice, where the second could never
// apply. A host matches whatever its port and scheme: a quota is the
// host's, and a service seldom keeps one per port.
//
// Requests of the other types (grpc, sql, redis, ...) take no turn: they carry no rateLimitSetting,
// which only the http and graphite fetch puts in a trip's context.

// hostRateLimits holds the buckets of rate_limits' entries, one for each,
// and their waits and refusals by entry name. It is apart from rateLimits,
// whose counts are by collector.
var hostRateLimits = newRateLimiterSet()

// hostRateLimitRule is an entry of rate_limits as requests match it.
type hostRateLimitRule struct {
	// key is the entry's bucket: its name, its hosts as written in
	// canonical form, and its limit.
	key rateLimitKey
	// exact are the host names and addresses, suffixes the ".example.com"
	// of each "*.example.com".
	exact    map[string]bool
	suffixes []string
}

// matches says whether the rule applies to host, in canonical form.
func (r *hostRateLimitRule) matches(host string) bool {
	if r.exact[host] {
		return true
	}
	for _, suffix := range r.suffixes {
		if strings.HasSuffix(host, suffix) {
			return true
		}
	}
	return false
}

// hostRateLimitRules are the entries in force, in the configuration's
// order; nil without any.
var hostRateLimitRules atomic.Pointer[[]*hostRateLimitRule]

// hostRateLimitSince records when the counts of each entry began: the
// zero time for the entries the first configuration followed had, which
// count since the exporter started, and the time a later reload added the
// others.
var hostRateLimitSince struct {
	mu       sync.Mutex
	followed bool
	since    map[string]time.Time
}

// matchHostRateLimit is the first entry of rate_limits in force that the
// host of u matches, and the host as it matched; nil when none does.
func matchHostRateLimit(u *url.URL) (*hostRateLimitRule, string) {
	rules := hostRateLimitRules.Load()
	if rules == nil || len(*rules) == 0 {
		return nil, ""
	}
	host := canonicalRequestHost(u.Hostname())
	for _, rule := range *rules {
		if rule.matches(host) {
			return rule, host
		}
	}
	return nil, ""
}

// canonicalRequestHost is host in the form entries are matched in: in lower
// case, without the dot that may end a fully qualified name, and an address
// as netip writes it, so 2001:DB8:0::1 is 2001:db8::1.
func canonicalRequestHost(host string) string {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if addr, err := netip.ParseAddr(host); err == nil {
		return addr.String()
	}
	return host
}

// hostNamePattern is a host name: labels of letters, digits, hyphens and
// underscores, joined by dots. An internationalised name is written in its
// ASCII (punycode) form, which is what a request's URL carries.
var hostNamePattern = regexp.MustCompile(`^[a-z0-9_]([a-z0-9_-]{0,61}[a-z0-9_])?(\.[a-z0-9_]([a-z0-9_-]{0,61}[a-z0-9_])?)*$`)

// canonicalRuleHost is a host of an entry in canonical form, and whether it
// is a "*." wildcard, whose suffix the returned form then is with its dot.
func canonicalRuleHost(written string) (string, bool, error) {
	host := strings.ToLower(written)
	if host == "" {
		return "", false, errors.New("is empty")
	}
	if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		addr, err := netip.ParseAddr(host[1 : len(host)-1])
		if err != nil || !addr.Is6() {
			return "", false, errors.New("is not an IPv6 address in brackets")
		}
		return addr.String(), false, nil
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		return addr.String(), false, nil
	}
	wildcard := strings.HasPrefix(host, "*.")
	name := strings.TrimSuffix(strings.TrimPrefix(host, "*."), ".")
	switch {
	case strings.Contains(name, "*"):
		return "", false, errors.New("has a * other than a leading \"*.\"; a wildcard is written *.example.com, and matches the subdomains of example.com")
	case strings.ContainsAny(name, ":/@?#"):
		return "", false, errors.New("is not a host name or an IP address; write the host alone, without a scheme, port or path: an entry applies to every port and scheme of its hosts")
	case len(name) > 253 || !hostNamePattern.MatchString(name):
		return "", false, errors.New("is not a host name or an IP address: letters, digits, hyphens and underscores in labels joined by dots, an internationalised name in its xn-- form")
	case wildcard:
		if _, err := netip.ParseAddr(name); err == nil {
			return "", false, errors.New("is a wildcard over an IP address, which has no subdomains")
		}
		return "." + name, true, nil
	}
	return name, false, nil
}

// compileHostRateLimit is the rule of an entry, which ValidateHostRateLimits
// has checked; its error says what is wrong with an entry it has not.
func compileHostRateLimit(entry *model.HostRateLimit) (*hostRateLimitRule, error) {
	rule := &hostRateLimitRule{exact: map[string]bool{}}
	canonical := make([]string, 0, len(entry.Hosts))
	for i, written := range entry.Hosts {
		host, wildcard, err := canonicalRuleHost(written)
		if err != nil {
			return nil, fmt.Errorf("hosts[%d] %q %w", i, written, err)
		}
		if wildcard {
			rule.suffixes = append(rule.suffixes, host)
			canonical = append(canonical, "*"+host)
		} else {
			rule.exact[host] = true
			canonical = append(canonical, host)
		}
	}
	rule.key = rateLimitKey{collector: entry.Name, origin: strings.Join(canonical, ","), perSecond: entry.RateLimit.RequestsPerSecond, burst: RateLimitBurst(&entry.RateLimit)}
	return rule, nil
}

// ValidateHostRateLimits checks rate_limits and fills in each entry's
// burst. An entry needs a name, a label value of its self-metrics and so
// unique, at least one host, each listed in one entry only, and a
// rate_limit as request.rate_limit's is checked. Every mistake is reported.
func ValidateHostRateLimits(entries []model.HostRateLimit) error {
	var errs []error
	names := map[string]bool{}
	hosts := map[string]string{}
	for i := range entries {
		entry := &entries[i]
		at := fmt.Sprintf("rate_limits entry %q", entry.Name)
		switch {
		case entry.Name == "":
			at = fmt.Sprintf("rate_limits entry %d", i+1)
			errs = append(errs, fmt.Errorf("%s has no name; name it, as its self-metrics are labelled", at))
		case !collectorNamePattern.MatchString(entry.Name):
			errs = append(errs, fmt.Errorf("%s has an invalid name: letters, digits and underscores, not starting with a digit", at))
		case names[entry.Name]:
			errs = append(errs, fmt.Errorf("%s is named twice; each entry's name must be unique", at))
		}
		names[entry.Name] = true
		if len(entry.Hosts) == 0 {
			errs = append(errs, fmt.Errorf("%s lists no hosts; hosts is required", at))
		}
		for j, written := range entry.Hosts {
			host, wildcard, err := canonicalRuleHost(written)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s hosts[%d] %q %w", at, j, written, err))
				continue
			}
			if wildcard {
				host = "*" + host
			}
			if first, ok := hosts[host]; ok {
				errs = append(errs, fmt.Errorf("%s lists %q, which %s lists already; a host may be listed once, in one entry", at, written, first))
				continue
			}
			hosts[host] = at
		}
		if err := checkRateLimit(&entry.RateLimit); err != nil {
			errs = append(errs, fmt.Errorf("%s rate_limit%w", at, err))
			continue
		}
		if entry.RateLimit.Burst == nil {
			burst := 1
			entry.RateLimit.Burst = &burst
		}
	}
	return model.JoinProblems(errs...)
}

// collectorNamePattern is the name a collector, and an entry of
// rate_limits, may have.
var collectorNamePattern = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// FollowHostRateLimits puts the entries of rate_limits of the configuration
// in force in force for the requests. The bucket of an entry a reload left
// as it was is kept, with the turns it has promised; that of an entry it
// changed, in its hosts or its limit, is dropped, the entry starting from a
// full bucket, as a collector's changed request.rate_limit does. The counts
// of an entry are kept while an entry of its name is in force.
func FollowHostRateLimits(entries []model.HostRateLimit) {
	rules := make([]*hostRateLimitRule, 0, len(entries))
	keys := map[rateLimitKey]bool{}
	names := map[string]bool{}
	for i := range entries {
		rule, err := compileHostRateLimit(&entries[i])
		if err != nil {
			// A checked configuration has no such entry.
			continue
		}
		rules = append(rules, rule)
		keys[rule.key] = true
		names[rule.key.collector] = true
	}
	hostRateLimitSince.mu.Lock()
	if hostRateLimitSince.since == nil {
		hostRateLimitSince.since = map[string]time.Time{}
	}
	for name := range hostRateLimitSince.since {
		if !names[name] {
			delete(hostRateLimitSince.since, name)
		}
	}
	for _, rule := range rules {
		if _, ok := hostRateLimitSince.since[rule.key.collector]; !ok {
			var since time.Time
			if hostRateLimitSince.followed {
				since = time.Now()
			}
			hostRateLimitSince.since[rule.key.collector] = since
		}
	}
	hostRateLimitSince.followed = true
	hostRateLimitRules.Store(&rules)
	hostRateLimitSince.mu.Unlock()
	hostRateLimits.keep(func(key rateLimitKey) bool { return keys[key] }, names)
}

// resetHostRateLimits forgets the buckets and counts of rate_limits, at the
// exporter's shutdown.
func resetHostRateLimits() {
	hostRateLimits.reset()
	hostRateLimitSince.mu.Lock()
	defer hostRateLimitSince.mu.Unlock()
	hostRateLimitSince.followed = false
	hostRateLimitSince.since = nil
}

// HostRateLimitCount is what an entry of rate_limits has counted: the
// requests that waited for their turn under it, those it refused, and when
// it began to count, the zero time for since the exporter started.
type HostRateLimitCount struct {
	Name            string
	Waited, Refused uint64
	Since           time.Time
}

// HostRateLimitCounts reports the counts of every entry of rate_limits in
// force, in the configuration's order.
func HostRateLimitCounts() []HostRateLimitCount {
	rules := hostRateLimitRules.Load()
	if rules == nil {
		return nil
	}
	out := make([]HostRateLimitCount, 0, len(*rules))
	hostRateLimitSince.mu.Lock()
	for _, rule := range *rules {
		out = append(out, HostRateLimitCount{Name: rule.key.collector, Since: hostRateLimitSince.since[rule.key.collector]})
	}
	hostRateLimitSince.mu.Unlock()
	hostRateLimits.mu.Lock()
	defer hostRateLimits.mu.Unlock()
	for i := range out {
		out[i].Waited, out[i].Refused = hostRateLimits.waited[out[i].Name], hostRateLimits.refused[out[i].Name]
	}
	return out
}
