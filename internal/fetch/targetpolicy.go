package fetch

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"unicode"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A collector's request.allowed_targets and request.denied_targets say which
// hosts its requests may reach, whoever names the target: a probe's target
// parameter comes from whoever can reach /probe, and a redirect from the
// target itself. Each entry is a host name, a glob of one, such as
// *.example.com, an IP address or a CIDR network, such as 10.0.0.0/8.
//
// A request is refused when its host, or an address it resolves to, is
// denied, or when allowed_targets is set and neither its host nor every
// address it resolves to is allowed. Names are checked against the host as
// written, before the request and again on every redirect. Addresses are
// checked where they are known without a second lookup: a connection made
// straight to the target is checked against the address it was made to, the
// one address that matters, which also catches a name that resolves
// elsewhere from one lookup to the next. Behind a proxy the exporter connects
// to the proxy rather than the target, so it resolves the target's name
// itself, before the request, and checks those addresses instead. A name the
// exporter cannot look up is the proxy's to resolve, as it is for any client
// behind one, unless the collector's own lists name addresses or networks,
// which could then not be held to.

// TargetRefusedError is a request refused by the collector's
// allowed_targets or denied_targets. The exporter answers it 403.
type TargetRefusedError struct {
	Host   string
	Reason string
}

func (e *TargetRefusedError) Error() string {
	return fmt.Sprintf("target %s refused: %s", e.Host, e.Reason)
}

// ErrTargetRefused matches every TargetRefusedError with errors.Is.
var ErrTargetRefused = errors.New("target refused")

// Is makes every TargetRefusedError match ErrTargetRefused.
func (e *TargetRefusedError) Is(target error) bool { return target == ErrTargetRefused }

// targetPolicy is a compiled pair of lists, and the addresses refused by
// default.
type targetPolicy struct {
	allowNames, denyNames []string
	allowNets, denyNets   []netip.Prefix
	// implicitDeny is the cloud metadata addresses the allowed networks do
	// not list: refused whatever the lists say otherwise.
	implicitDeny []netip.Prefix
}

// metadataAddrs are the cloud metadata services, which hand out the
// credentials of the machine the exporter runs on: AWS, GCP, Azure,
// OpenStack and most others answer at 169.254.169.254, and AWS also at
// fd00:ec2::254 over IPv6. Every collector refuses them unless its
// allowed_targets lists them by address or network, since a probe's target
// is chosen by whoever can reach /probe.
var metadataAddrs = []netip.Prefix{
	netip.MustParsePrefix("169.254.169.254/32"),
	netip.MustParsePrefix("fd00:ec2::254/128"),
}

// hostGlob is what an entry that is a name, or a glob of one, is made of. It
// takes what a host a request can reach may be written with, which is more
// than a registered domain: an underscore, and a hyphen first or last.
var hostGlob = regexp.MustCompile(`^[a-z0-9*?_-]([a-z0-9*?_.-]*[a-z0-9*?_-])?$`)

// OutsideASCIIPattern matches what no entry of request.allowed_targets or
// request.denied_targets holds, a character outside ASCII, for the
// configuration's JSON Schema: an entry is written in ASCII, as a host is
// (canonicalHost). What else an entry has to be is the exporter's to refuse.
const OutsideASCIIPattern = `[^\x00-\x7F]`

// compileTargetPolicy reads the two lists. With both empty the policy still
// refuses the cloud metadata addresses.
func compileTargetPolicy(allowed, denied []string) (*targetPolicy, error) {
	p := &targetPolicy{}
	for _, list := range []struct {
		key   string
		items []string
		names *[]string
		nets  *[]netip.Prefix
	}{
		{"allowed_targets", allowed, &p.allowNames, &p.allowNets},
		{"denied_targets", denied, &p.denyNames, &p.denyNets},
	} {
		for _, raw := range list.items {
			// An entry is held to what a host is (canonicalHost): one with
			// a character outside ASCII would match no host a request is
			// made to, or, where lower case turns the character into a
			// letter, as the Kelvin sign into k, a host other than the one
			// written.
			if r, found := outsideASCII(raw); found {
				return nil, fmt.Errorf("request.%s entry %q has %#U, %s", list.key, raw, r, writtenInASCII)
			}
			entry := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(raw), "."))
			entry = strings.TrimSuffix(strings.TrimPrefix(entry, "["), "]")
			switch {
			case entry == "":
				return nil, fmt.Errorf("request.%s has an empty entry", list.key)
			case strings.Contains(entry, "/"):
				prefix, err := netip.ParsePrefix(entry)
				if err != nil {
					return nil, fmt.Errorf("request.%s entry %q is not a CIDR network such as 10.0.0.0/8: %w", list.key, raw, err)
				}
				*list.nets = append(*list.nets, prefix.Masked())
			default:
				if addr, err := netip.ParseAddr(entry); err == nil {
					addr = addr.Unmap()
					*list.nets = append(*list.nets, netip.PrefixFrom(addr, addr.BitLen()))
					continue
				}
				if !hostGlob.MatchString(entry) {
					return nil, fmt.Errorf("request.%s entry %q is not a host name, a glob of one such as *.example.com, an IP address or a CIDR network; give the host alone, without a scheme, port or path", list.key, raw)
				}
				*list.names = append(*list.names, entry)
			}
		}
	}
	for _, metadata := range metadataAddrs {
		if _, listed := matchesAddr(p.allowNets, metadata.Addr()); !listed {
			p.implicitDeny = append(p.implicitDeny, metadata)
		}
	}
	return p, nil
}

var targetPolicies sync.Map // "allowed\x00denied" -> *targetPolicy

// policyOf is the collector's compiled policy: every collector has one,
// since the cloud metadata addresses are refused by default. The lists were
// checked at load, so an error here cannot happen.
func policyOf(c *model.Collector) *targetPolicy {
	key := strings.Join(c.Request.AllowedTargets, "\x01") + "\x00" + strings.Join(c.Request.DeniedTargets, "\x01")
	if p, ok := targetPolicies.Load(key); ok {
		return p.(*targetPolicy)
	}
	p, err := compileTargetPolicy(c.Request.AllowedTargets, c.Request.DeniedTargets)
	if err != nil {
		return nil
	}
	targetPolicies.Store(key, p)
	return p
}

// validateTargetPolicy checks a collector's lists at load.
func validateTargetPolicy(c *model.Collector) error {
	if _, err := compileTargetPolicy(c.Request.AllowedTargets, c.Request.DeniedTargets); err != nil {
		return fmt.Errorf("collector %q %w", c.Name, err)
	}
	return nil
}

func matchesName(globs []string, host string) (string, bool) {
	for _, glob := range globs {
		if ok, _ := path.Match(glob, host); ok {
			return glob, true
		}
	}
	return "", false
}

// matchesAddr finds the network of nets addr is in. A zone, as in
// fe80::1%eth0, says which interface reaches the address, not which address
// it is, so it is dropped: netip.Prefix.Contains never matches a zoned
// address, which would slip past every rule.
func matchesAddr(nets []netip.Prefix, addr netip.Addr) (netip.Prefix, bool) {
	addr = addr.Unmap().WithZone("")
	for _, prefix := range nets {
		if prefix.Contains(addr) {
			return prefix, true
		}
	}
	return netip.Prefix{}, false
}

// checkName applies the name rules to host and says whether its name alone
// allows it, so its addresses need not be.
func (p *targetPolicy) checkName(host string) (nameAllowed bool, err error) {
	if glob, denied := matchesName(p.denyNames, host); denied {
		return false, &TargetRefusedError{Host: host, Reason: fmt.Sprintf("it matches %q in request.denied_targets", glob)}
	}
	_, allowed := matchesName(p.allowNames, host)
	return allowed, nil
}

// checkAddr applies the address rules to one address host resolves to.
func (p *targetPolicy) checkAddr(host string, addr netip.Addr, nameAllowed bool) error {
	if prefix, denied := matchesAddr(p.denyNets, addr); denied {
		return &TargetRefusedError{Host: host, Reason: fmt.Sprintf("its address %s is in %s in request.denied_targets", addr.Unmap(), prefix)}
	}
	if _, metadata := matchesAddr(p.implicitDeny, addr); metadata {
		return &TargetRefusedError{Host: host, Reason: fmt.Sprintf("its address %s is a cloud metadata service's, refused unless request.allowed_targets lists it", addr.Unmap())}
	}
	if nameAllowed || len(p.allowNames)+len(p.allowNets) == 0 {
		return nil
	}
	if _, allowed := matchesAddr(p.allowNets, addr); !allowed {
		return &TargetRefusedError{Host: host, Reason: fmt.Sprintf("neither it nor its address %s is in request.allowed_targets", addr.Unmap())}
	}
	return nil
}

// needsAddrs says whether host's addresses have to be looked at.
func (p *targetPolicy) needsAddrs(nameAllowed bool) bool {
	return len(p.denyNets)+len(p.implicitDeny) > 0 || (!nameAllowed && len(p.allowNames)+len(p.allowNets) > 0)
}

// hasAddressRules says whether the collector's own lists name an address or
// a network. The cloud metadata addresses, refused by default, are not the
// collector's own.
func (p *targetPolicy) hasAddressRules() bool {
	return len(p.allowNets)+len(p.denyNets) > 0
}

// check applies the policy to host before a request: its name, then, with
// resolve, every address it resolves to. It returns whether the name alone
// allowed it. Without resolve the addresses are left to the connection
// (checkConnection).
func (p *targetPolicy) check(ctx context.Context, host string, resolve bool) (bool, error) {
	host, err := canonicalHost(host)
	if err != nil {
		return false, err
	}
	nameAllowed, err := p.checkName(host)
	if err != nil || !p.needsAddrs(nameAllowed) {
		return nameAllowed, err
	}
	// An address written as the target needs no lookup, so it is checked
	// before anything is sent, proxy or not.
	if addr, literal := literalAddr(host); literal {
		return nameAllowed, p.checkAddr(host, addr, nameAllowed)
	}
	if !resolve {
		return nameAllowed, nil
	}
	addrs, err := resolveHost(ctx, host)
	if err != nil {
		return nameAllowed, p.unresolved(host, nameAllowed, err)
	}
	for _, addr := range addrs {
		if err := p.checkAddr(host, addr, nameAllowed); err != nil {
			return nameAllowed, err
		}
	}
	return nameAllowed, nil
}

// unresolved decides for a host behind a proxy whose name the exporter
// could not look up itself. That is the ordinary state of a host with no
// outside DNS, whose proxy resolves every name it is given, so it fails the
// request only when the collector's own lists name addresses or networks:
// those rules cannot be held to an address nobody here knows. Otherwise the
// name rules, which were applied already, are all there is to apply, and the
// proxy resolves the name. The cloud metadata addresses are then refused by
// name and by address as written, not by what the proxy resolves a name to.
func (p *targetPolicy) unresolved(host string, nameAllowed bool, lookupErr error) error {
	if p.hasAddressRules() {
		return fmt.Errorf("the request goes through a proxy, so %s has to be looked up here to hold it to the addresses and networks in request.allowed_targets and denied_targets, and the lookup failed: %w", host, lookupErr)
	}
	if !nameAllowed && len(p.allowNames) > 0 {
		return &TargetRefusedError{Host: host, Reason: "it is not in request.allowed_targets"}
	}
	return nil
}

// literalAddr is the address host is when it is written as one, which then
// needs no lookup: an IPv4 or IPv6 address as Go reads them, or an IPv4
// address in one of the older forms resolvers, curl and proxies still read —
// one number (2130706433), fewer than four parts (127.1), or parts in
// hexadecimal or octal (0x7f.0.0.1, 0177.0.0.1) — which would otherwise pass
// as a name and be turned into the address by whoever resolves it.
func literalAddr(host string) (netip.Addr, bool) {
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	if addr, err := netip.ParseAddr(host); err == nil {
		return addr, true
	}
	parts := strings.Split(host, ".")
	if len(parts) > 4 {
		return netip.Addr{}, false
	}
	var value uint64
	for i, part := range parts {
		base, digits := 10, part
		switch {
		case strings.HasPrefix(part, "0x"):
			base, digits = 16, part[2:]
		case len(part) > 1 && part[0] == '0':
			base, digits = 8, part[1:]
		}
		// ParseUint alone would take a sign, and an underscore with base 0.
		if digits == "" || strings.Trim(digits, "0123456789abcdef") != "" {
			return netip.Addr{}, false
		}
		n, err := strconv.ParseUint(digits, base, 32)
		if err != nil {
			return netip.Addr{}, false
		}
		// Every part but the last is one byte; the last fills the rest.
		rest := 8 * (4 - i)
		if i < len(parts)-1 {
			if n > 0xff {
				return netip.Addr{}, false
			}
			value |= n << (rest - 8)
			continue
		}
		if n >= 1<<rest {
			return netip.Addr{}, false
		}
		value |= n
	}
	return netip.AddrFrom4([4]byte{byte(value >> 24), byte(value >> 16), byte(value >> 8), byte(value)}), true
}

// resolveHost is host's addresses: the address itself for an IP literal.
var resolveHost = func(ctx context.Context, host string) ([]netip.Addr, error) {
	if addr, err := netip.ParseAddr(strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")); err == nil {
		return []netip.Addr{addr}, nil
	}
	return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
}

// policyGuard carries a request's policy to the connections it makes: the
// hosts it checked, and whether each one's name allowed it.
type policyGuard struct {
	policy *targetPolicy
	// proxy is the proxy function of the transport sending the request, so
	// the guard decides what goes through a proxy exactly as the transport
	// does, from the environment as the transport read it.
	proxy func(*http.Request) (*url.URL, error)
	mu    sync.Mutex
	hosts map[string]bool
	// proxied is set once a host the request checked goes through a proxy,
	// whose own address a connection is then made to.
	proxied bool
	// schemes is the collector's request.allowed_schemes, which a redirect
	// must keep to as the first URL did: an https-only collector does not
	// follow a redirect to http:// and read the answer in plain text.
	schemes []string
	// trusted is the collector's request.redirect_trusted_hosts, the hosts
	// besides the first URL's own origin that a redirect may carry the
	// request's headers, credentials and body to, and lastHop what was
	// settled for the redirect followed last (redirecttrust.go).
	trusted []string
	lastHop redirectHop
	// clientCertificate says the collector has a TLS client certificate,
	// which every TLS connection of the request presents: a redirect is
	// not followed over https to a host that is not trusted.
	clientCertificate bool
}

type policyGuardKey struct{}

// withTargetPolicy checks the host of u against the collector's policy and
// returns a context whose connections and redirects are checked too.
func withTargetPolicy(ctx context.Context, c *model.Collector, u *url.URL, proxy func(*http.Request) (*url.URL, error)) (context.Context, error) {
	guard := &policyGuard{policy: policyOf(c), proxy: proxy, hosts: map[string]bool{}, schemes: c.Request.AllowedSchemes, trusted: c.Request.RedirectTrustedHosts, clientCertificate: c.Request.TLS.CertFile != ""}
	if err := guard.check(ctx, u); err != nil {
		return ctx, err
	}
	return context.WithValue(ctx, policyGuardKey{}, guard), nil
}

// check applies the policy to the host of u, which the request is about to
// reach, the first or one a redirect names, and remembers it for the
// connections. Its addresses are resolved here only when a proxy stands in
// between; otherwise the connection checks the one it is made to.
func (g *policyGuard) check(ctx context.Context, u *url.URL) error {
	if g.policy == nil {
		// The lists were refused at load, so this cannot happen.
		return nil
	}
	host, err := canonicalHost(u.Hostname())
	if err != nil {
		return err
	}
	proxied := viaProxy(g.proxy, u)
	nameAllowed, err := g.policy.check(ctx, host, proxied)
	if err != nil {
		return err
	}
	g.mu.Lock()
	g.hosts[host] = nameAllowed
	g.proxied = g.proxied || proxied
	g.mu.Unlock()
	return nil
}

// canonicalHost is host as the policy matches it, which is the name the
// request is made to: lower case and without a trailing dot. A host is an IP
// address or a name, and a name is made only of what a name in the DNS, a
// hosts file or a container network can hold: letters, digits, '.', '-' and
// '_'. Within those it is dialed as it is written — an underscore, as
// my_service, a Docker Compose or Kubernetes name, or hyphens where a
// registered domain may not have them — so it is checked as written and not
// held to the rules of an internationalised name.
//
// A host with a character outside ASCII is refused, whatever it would
// convert to, since Go's HTTP client does not send it under one name. It
// dials, and names in the TLS handshake and in a proxy's CONNECT, the mapped
// form of the name (idna.Lookup: "ｏrigin.test", with a fullwidth o, is
// origin.test, and "origin。test" too), and writes the Host header, an
// HTTP/2 :authority and the request line a proxy reads in the unmapped one
// (idna.ToASCII: xn--rigin-qr33a.test, xn--origintest-sh3i). A verdict on
// either form would be a verdict on a name that part of the request is not
// made to: an allowed name would cover a request a proxy fetches from, or a
// shared server routes to, a name the lists never allowed, and a denied one
// would be reached under a spelling the lists do not catch. Over HTTP/2 the
// two forms do not even meet: the connection is kept under one and looked
// for under the other, so nothing is sent and the target is dialed again
// and again. So an internationalised name is written in its ASCII form, the
// one starting with xn--, which Go sends everywhere as it is written
// (wirename_test.go shows all of it).
//
// Any other character refuses the host too. No resolver here would find
// such a name, but behind a proxy the name is the proxy's to resolve, and a
// proxy may read it differently from the policy: a '%' above all, which is
// how a URL's host carries one ("local%2568ost" parses to the host
// "local%68ost"), since a proxy that decodes it once more asks for
// localhost, a name the policy never saw. The same goes for 169.254.169.254
// with one digit written as an escape.
func canonicalHost(host string) (string, error) {
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	if r, found := outsideASCII(host); found {
		return "", &TargetRefusedError{Host: host, Reason: fmt.Sprintf("it has %#U, %s", r, writtenInASCII)}
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return strings.ToLower(host), nil
	}
	if i := strings.IndexFunc(host, notHostCharacter); i >= 0 {
		return "", &TargetRefusedError{Host: host, Reason: fmt.Sprintf("it has %q, a character that no host name or address has: a host is an IP address, or a name of letters, digits, '.', '-' and '_'", host[i:i+1])}
	}
	return strings.ToLower(strings.TrimSuffix(host, ".")), nil
}

// outsideASCII finds the first character of s that is not ASCII; a byte that
// is no UTF-8 is one, and is returned as the replacement character.
func outsideASCII(s string) (rune, bool) {
	for _, r := range s {
		if r > unicode.MaxASCII {
			return r, true
		}
	}
	return 0, false
}

// writtenInASCII is what the refusal of a host, or of an entry of a list of
// hosts, with a character outside ASCII says after naming the character.
const writtenInASCII = "a character outside ASCII: write an internationalised name in its ASCII form, as xn--bcher-kva.example for bücher.example"

// notHostCharacter says whether r is something no host name is written
// with (canonicalHost).
func notHostCharacter(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
		return false
	}
	return true
}

// checkRedirect applies a request's request.allowed_schemes and policy to
// the URL a redirect leads to.
func checkRedirect(ctx context.Context, u *url.URL) error {
	guard, ok := ctx.Value(policyGuardKey{}).(*policyGuard)
	if !ok {
		return nil
	}
	if err := checkSchemeAllowed(guard.schemes, u.Scheme); err != nil {
		return fmt.Errorf("redirect to %s refused: %w", RedactURL(u, MaskQueryValues), err)
	}
	return guard.check(ctx, u)
}

// viaProxy says whether proxy, a transport's proxy function, sends a
// request to u through a proxy.
func viaProxy(proxy func(*http.Request) (*url.URL, error), u *url.URL) bool {
	if proxyOverride != nil {
		return proxyOverride(u)
	}
	if proxy == nil {
		return false
	}
	through, err := proxy(&http.Request{URL: u, Header: http.Header{}})
	return err == nil && through != nil
}

// proxyOverride, set by tests, decides what goes through a proxy instead.
var proxyOverride func(*url.URL) bool

// checkConnection applies a request's policy to the address a connection to
// dialed, host:port, was made to. A dial to a host the request did not
// check is a dial to a proxy, whose target was checked by name and resolved
// address already; one when nothing the request checked goes through a
// proxy is refused, since what it reaches was never checked.
func checkConnection(ctx context.Context, dialed string, conn net.Conn) error {
	guard, ok := ctx.Value(policyGuardKey{}).(*policyGuard)
	if !ok || guard.policy == nil {
		return nil
	}
	host, _, err := net.SplitHostPort(dialed)
	if err != nil {
		host = dialed
	}
	host, err = canonicalHost(host)
	if err != nil {
		return err
	}
	guard.mu.Lock()
	nameAllowed, checked := guard.hosts[host]
	proxied := guard.proxied
	guard.mu.Unlock()
	if !checked {
		if proxied {
			return nil
		}
		return &TargetRefusedError{Host: host, Reason: "the connection is to a host the request did not check"}
	}
	remote, ok := conn.RemoteAddr().(*net.TCPAddr)
	if !ok {
		return nil
	}
	addr, ok := netip.AddrFromSlice(remote.IP)
	if !ok {
		return nil
	}
	return guard.policy.checkAddr(host, addr, nameAllowed)
}

// policyDialer wraps dial so every connection is checked against the policy
// of the request that made it.
func policyDialer(dial func(ctx context.Context, network, address string) (net.Conn, error)) func(ctx context.Context, network, address string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := dial(ctx, network, address)
		if err != nil {
			return nil, err
		}
		if err := checkConnection(ctx, address, conn); err != nil {
			_ = conn.Close()
			return nil, err
		}
		return conn, nil
	}
}
