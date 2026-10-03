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
	"slices"
	"strings"
	"unicode"
)

// What a followed redirect carries.
//
// A target that redirects chooses where the next request goes. Go's client,
// left to itself, sends that request nearly everything the first one had:
// Authorization and Cookie are dropped only for a host that is neither the
// first one nor a subdomain of it, every other header — an API key in a
// header of its own, the headers a probe forwards — goes wherever the
// redirect points, a Referer with the previous URL is added, and after a 307
// or a 308 the request body is sent again. A target could so hand the
// collector's credentials to any host it names.
//
// So each hop is judged by where it leads. It is trusted when it stays on
// the origin the request was made to — the scheme, host and port of the first
// URL — or when its host is one of the collector's
// request.redirect_trusted_hosts. A trusted hop carries what the first
// request carried, credentials included. Any other hop carries only the
// headers that say nothing of the collector, Accept, Accept-Language and
// User-Agent, and never the request body: a redirect that would send the
// body to such a host is refused, and so is one over https when the
// collector has a TLS client certificate, which the connection would present
// there. No hop is given a Referer the collector did not set itself. Every
// hop is judged on its own, against the first URL and the list, so a chain
// that leaves the origin and comes back carries everything again once it is
// back.
//
// A hop is judged by the host its URL writes, since that is what Go routes
// it by (wireHost): a verdict reached on a tidied form of the name would be
// one about another host than the request is sent to.
//
// Whether a hop is made at all is still for allowed_schemes, allowed_targets,
// denied_targets and the limit of ten redirects to say (checkRedirect).
//
// All of it applies to a request with a target policy in its context, which
// every collector request has. A client used without one, as the OTLP
// exporter's is, follows redirects as Go does.

// redirectHop is what was settled for one redirect hop.
type redirectHop struct {
	// untrusted says the hop leads off the first origin to a host the
	// collector does not list.
	untrusted bool
	// host is where it leads: the host of its URL as it is written there,
	// in lower case.
	host string
	// origin is the scheme, host and port it leads to and from those of the
	// first URL, set when the two name one host and so differ in scheme or
	// port only: the host alone would not say why the hop is another
	// origin's.
	origin, from string
	// withheld names the headers of the first request the hop is not sent
	// for that reason, sorted; never their values.
	withheld []string
	// certificate says the hop would be made over TLS with the collector's
	// client certificate, which it must not be shown.
	certificate bool
}

// urlCredentials is how the credential in the userinfo of the first URL is
// named among what a hop was not sent: Go makes an Authorization of it for
// the request it sends, which the request's own headers do not hold.
const urlCredentials = "Authorization (the target URL's credentials)"

// neutralRedirectHeader says whether a header, by its canonical name, is one
// an untrusted hop still carries: what the answer should look like and who
// asks, nothing the collector was given to identify itself with.
func neutralRedirectHeader(name string) bool {
	return name == "Accept" || name == "Accept-Language" || name == "User-Agent"
}

// redirectBodyHeader says whether a header, by its canonical name, describes
// the request body. Go leaves these out of a hop that is sent without the
// body, a POST a 301, 302 or 303 turned into a GET, and they are left out
// here as Go leaves them.
func redirectBodyHeader(name string) bool {
	switch name {
	case "Content-Encoding", "Content-Language", "Content-Location", "Content-Type":
		return true
	}
	return false
}

// settleRedirect decides what req, the request a redirect leads to, carries,
// and writes its headers accordingly. via[0] is the first request, whose
// headers are what the collector, its credentials and the probe made them.
// req arrives as Go prepared it: the first request's headers, without
// Authorization and Cookie once a hop left the first host's domain, without
// the body's headers once a hop dropped the body, and with a Referer.
func settleRedirect(ctx context.Context, req *http.Request, via []*http.Request) redirectHop {
	guard, ok := ctx.Value(policyGuardKey{}).(*policyGuard)
	if !ok || len(via) == 0 {
		return redirectHop{}
	}
	first := via[0]
	hop := redirectHop{host: req.URL.Hostname()}
	firstHost, _, firstOK := wireHost(first.URL.Hostname())
	host, _, ok := wireHost(hop.host)
	if ok {
		hop.host = host
	}
	if onOrigin := sameOrigin(first.URL, req.URL); onOrigin || trustedRedirectHost(guard.trusted, req.URL.Hostname()) {
		carryEverything(req, first)
		if !onOrigin {
			// A Host the collector set names a virtual host of the origin,
			// and is no other host's, however trusted. Go keeps it across
			// a Location without a scheme, which //other.example/ has not
			// either.
			req.Host = ""
		}
	} else {
		hop.untrusted = true
		hop.withheld = carryNeutral(req, first)
		// A user and password the Location wrote into its URL would be
		// made an Authorization of by Go, after this: the hop is sent the
		// neutral headers and nothing else.
		req.URL.User = nil
		if ok && firstOK && host == firstHost {
			hop.origin, hop.from = originOf(req.URL), originOf(first.URL)
		}
		hop.certificate = guard.clientCertificate && strings.EqualFold(req.URL.Scheme, "https")
	}
	guard.mu.Lock()
	guard.lastHop = hop
	guard.mu.Unlock()
	return hop
}

// carryEverything gives a trusted hop the headers of the first request: the
// ones Go took out for a host outside the first one's domain are put back,
// and the Referer Go added is taken out, leaving one the first request had.
// A Host the collector set is left as Go left it, kept across a redirect
// whose Location has no scheme and dropped by one that has, which names its
// host itself; settleRedirect takes it off a hop that leaves the origin.
func carryEverything(req, first *http.Request) {
	for name := range req.Header {
		if http.CanonicalHeaderKey(name) == "Referer" {
			delete(req.Header, name)
		}
	}
	for name, values := range first.Header {
		if _, carried := req.Header[name]; carried || redirectBodyHeader(http.CanonicalHeaderKey(name)) {
			continue
		}
		req.Header[name] = values
	}
}

// carryNeutral gives an untrusted hop the neutral headers of the first
// request and nothing else, and returns the names of the ones withheld. A
// header of the body that Go had already left out with the body is not among
// them: it would not have been sent to a trusted host either.
func carryNeutral(req, first *http.Request) []string {
	kept := make(http.Header)
	var withheld []string
	for name, values := range first.Header {
		canonical := http.CanonicalHeaderKey(name)
		if neutralRedirectHeader(canonical) {
			kept[name] = values
			continue
		}
		if _, proposed := req.Header[name]; !proposed && redirectBodyHeader(canonical) {
			continue
		}
		withheld = append(withheld, canonical)
	}
	// A Host Go kept (settleRedirect says when) is withheld too.
	if req.Host != "" {
		withheld = append(withheld, "Host")
		req.Host = ""
	}
	// So is the credential of the first URL's userinfo, which Go sent as
	// an Authorization where the request had none of its own.
	if first.URL.User != nil && first.Header.Get("Authorization") == "" {
		withheld = append(withheld, urlCredentials)
	}
	req.Header = kept
	slices.Sort(withheld)
	return slices.Compact(withheld)
}

// refusal is the error of a hop that must not be made: one that would send
// the request body to a host that is not trusted, or present the collector's
// TLS client certificate to it; nil for every other hop. Go attaches the body
// to the request of a hop that sends it again, after a 307 or a 308, before
// it asks whether the hop is made, and no connection has been made yet.
func (h redirectHop) refusal(req *http.Request) error {
	if !h.untrusted {
		return nil
	}
	if req.Body != nil && req.Body != http.NoBody {
		return &redirectRefusedError{url: RedactURL(req.URL, MaskQueryValues), because: "it would send the request body to " + h.stranger("listed in")}
	}
	if h.certificate {
		return &redirectRefusedError{url: RedactURL(req.URL, MaskQueryValues), because: "it would present the collector's TLS client certificate to " + h.stranger("listed in")}
	}
	return nil
}

// stranger names where a hop that is not trusted leads and says why it is
// not: the host, or, for the first URL's own host under another scheme or
// port, both origins. listed is "listed in" or "in", as the sentence around
// it has it.
func (h redirectHop) stranger(listed string) string {
	if h.origin == "" {
		return h.host + ", which is not the origin the request was made to and is not " + listed + " request.redirect_trusted_hosts"
	}
	return h.origin + ", which is not the origin the request was made to, " + h.from + ", and whose host is not " + listed + " request.redirect_trusted_hosts"
}

// statusNote is what the error of a status answered by the hop's host adds
// to "received HTTP status <n>".
func (h redirectHop) statusNote() string {
	if h.origin == "" {
		return "from " + h.host + ", where a redirect led: the collector's headers and credentials were not sent to that host, which is not the origin the request was made to; list it in request.redirect_trusted_hosts if it is to be sent them"
	}
	return "from " + h.origin + ", where a redirect led: the collector's headers and credentials were not sent there, since " + h.origin + " is not the origin the request was made to, " + h.from + "; list " + h.host + " in request.redirect_trusted_hosts if it is to be sent them"
}

// traceNote is why the hop was not sent what withheld names, for a debug
// report.
func (h redirectHop) traceNote() string {
	if h.origin == "" {
		return "the redirect leads to a host that is not the origin the request was made to and is not in request.redirect_trusted_hosts"
	}
	return "the redirect leads to " + h.stranger("in")
}

// redirectRefusedError is a redirect refused for what it would have given a
// host that is not trusted: the request body, or the sight of the
// collector's TLS client certificate. The request is not tried again: the
// same redirect would be refused again.
type redirectRefusedError struct {
	url, because string
}

func (e *redirectRefusedError) Error() string {
	return fmt.Sprintf("redirect to %s refused: %s", e.url, e.because)
}

// redirectWithheldFrom says, for the last redirect hop of the request in
// ctx, that its host was not sent headers of the first request (statusNote),
// and "" when it was sent them all. It is asked only of an answer a redirect
// led to.
func redirectWithheldFrom(ctx context.Context) string {
	guard, ok := ctx.Value(policyGuardKey{}).(*policyGuard)
	if !ok {
		return ""
	}
	guard.mu.Lock()
	defer guard.mu.Unlock()
	if len(guard.lastHop.withheld) == 0 {
		return ""
	}
	return guard.lastHop.statusNote()
}

// wireHost is a URL's host as a redirect is judged by it: the name Go routes
// the request by, which is the host as the URL writes it. A name in ASCII is
// looked up as written, whatever its case, so it is compared in lower case
// and with its final dot when it has one: a rooted name is another name to
// the resolver, which looks it up past the hosts file and the search list, so
// localhost. and api.dev. need not be the machines localhost and api.dev are.
// The zone of an IPv6 address names an interface, and keeps its case. A host
// with a character outside ASCII is converted before it is used, and not in
// the same way for the address dialed as for the Host header and the request
// line a proxy reads, so no tidied form says where it leads: it is left as
// written and is ascii false, matched by no entry of the list. ok is false
// for a host the policy refuses (canonicalHost), as it does every host with
// such a character: it is nobody's origin, and nothing is sent to it.
func wireHost(host string) (wire string, ascii, ok bool) {
	_, err := canonicalHost(host)
	if strings.IndexFunc(host, func(r rune) bool { return r > unicode.MaxASCII }) >= 0 {
		return host, false, err == nil
	}
	if err != nil {
		return host, true, false
	}
	if address, zone, zoned := strings.Cut(host, "%"); zoned {
		return strings.ToLower(address) + "%" + zone, true, true
	}
	return strings.ToLower(host), true, true
}

// originOf is a URL's origin as it is written, for a message: its scheme,
// its host and its port when it writes one.
func originOf(u *url.URL) string {
	return strings.ToLower(u.Scheme + "://" + u.Host)
}

// sameOrigin says whether two URLs have the same scheme, host and port. The
// hosts are compared as they are written (wireHost), and a URL without a
// port has its scheme's: http://api.example and http://API.example:80 are
// one origin, and https://api.example, http://api.example:8080,
// http://www.api.example and http://api.example. are four others. A host
// the policy could not write is nobody's origin.
func sameOrigin(a, b *url.URL) bool {
	if !strings.EqualFold(a.Scheme, b.Scheme) {
		return false
	}
	hostA, _, okA := wireHost(a.Hostname())
	hostB, _, okB := wireHost(b.Hostname())
	return okA && okB && hostA == hostB && effectivePort(a) == effectivePort(b)
}

// effectivePort is the port a URL's requests go to: its own, or its
// scheme's.
func effectivePort(u *url.URL) string {
	if port := u.Port(); port != "" {
		return port
	}
	switch strings.ToLower(u.Scheme) {
	case "http":
		return "80"
	case "https":
		return "443"
	}
	return ""
}

// trustedRedirectHost says whether host, as a redirect's URL writes it,
// matches an entry of request.redirect_trusted_hosts: a name or a glob of
// one, matched against the host in lower case with its final dot when it has
// one, or an address, which matches a host written as that address, zone
// included. A host written with a character outside ASCII matches no entry
// (wireHost). The host is never looked up: what a name resolves to trusts
// nothing. The entries were checked at load; one that does not read trusts
// nothing.
func trustedRedirectHost(entries []string, host string) bool {
	host, ascii, _ := wireHost(host)
	if !ascii {
		return false
	}
	for _, raw := range entries {
		glob, addr, err := parseTrustedHost(raw)
		if err != nil {
			continue
		}
		if addr.IsValid() {
			if written, parseErr := netip.ParseAddr(host); parseErr == nil && written.Unmap() == addr {
				return true
			}
			continue
		}
		if matched, _ := path.Match(glob, host); matched {
			return true
		}
	}
	return false
}

// TrustedHostPattern is what an entry of request.redirect_trusted_hosts
// looks like, for the configuration's JSON Schema: "*" alone; a host name or
// a glob of one, with a final dot or without, which is digits and dots only
// or holds a letter, a hyphen or an underscore; or something written as an
// IPv6 address is, with two colons at least. It refuses what
// parseTrustedHost refuses but an address of that shape that is none, which
// a pattern cannot tell.
const TrustedHostPattern = `^\s*(\*|\[?([0-9]([0-9.]*[0-9])?|[A-Za-z_-]([A-Za-z0-9*?_.-]*[A-Za-z0-9*?_-])?|[0-9*?][A-Za-z0-9*?_.-]*[A-Za-z_-]([A-Za-z0-9*?_.-]*[A-Za-z0-9*?_-])?)\]?\.?|\[?[0-9A-Fa-f.]*:[0-9A-Fa-f.]*:([0-9A-Fa-f:.]*[0-9A-Fa-f:])?(%[A-Za-z0-9_.-]*[A-Za-z0-9_-])?\]?)\s*$`

// parseTrustedHost reads one entry of request.redirect_trusted_hosts: a host
// name or a glob of one, written as allowed_targets writes them and returned
// in lower case, or an IP address. A name is returned with the final dot it
// was written with: it is matched against the host a redirect writes, and
// api.example. is not the name api.example is (wireHost). "*" is the glob
// that matches every host. Anything else is refused: a network, since a
// host is trusted by its name as written and not by an address it resolves
// to; a port, since a host is trusted on all of them; a URL; and a glob
// without a letter, a hyphen or an underscore, which is either wildcards
// alone, every host under another spelling than "*", or written like a
// range of addresses, which a glob, matched against names, is not: 10.*
// matches 10.evil.example.
func parseTrustedHost(raw string) (glob string, addr netip.Addr, err error) {
	if r, found := outsideASCII(raw); found {
		return "", netip.Addr{}, fmt.Errorf("request.redirect_trusted_hosts entry %q has %#U, %s", raw, r, writtenInASCII)
	}
	written := strings.TrimSpace(raw)
	name, dotted := strings.CutSuffix(written, ".")
	name = strings.TrimSuffix(strings.TrimPrefix(name, "["), "]")
	if name == "" {
		return "", netip.Addr{}, errors.New("request.redirect_trusted_hosts has an empty entry")
	}
	if address, parseErr := netip.ParseAddr(name); parseErr == nil && !dotted {
		return "", address.Unmap(), nil
	}
	if entry := strings.ToLower(name); hostGlob.MatchString(entry) {
		if strings.ContainsAny(entry, "*?") && written != "*" && strings.Trim(entry, "0123456789.*?") == "" {
			if strings.Trim(entry, ".*?") == "" {
				return "", netip.Addr{}, fmt.Errorf(`request.redirect_trusted_hosts entry %q is made of wildcards alone and names no host; write "*" if every host is to be trusted, and otherwise a host name, a glob of one such as *.example.com or an IP address`, raw)
			}
			return "", netip.Addr{}, fmt.Errorf("request.redirect_trusted_hosts entry %q is a glob written like a range of addresses; a glob is matched against host names, and this one would trust every name that fits it, such as %s, so list each address a redirect names on its own", raw, strings.NewReplacer("*", "evil.example", "?", "x").Replace(entry))
		}
		if dotted {
			entry += "."
		}
		return entry, netip.Addr{}, nil
	}
	if _, prefixErr := netip.ParsePrefix(name); prefixErr == nil {
		return "", netip.Addr{}, fmt.Errorf("request.redirect_trusted_hosts entry %q is a network; a redirect is trusted by the host its URL names, which is never looked up, so list each address a redirect names on its own, or the hosts by name", raw)
	}
	if host, port, splitErr := net.SplitHostPort(written); splitErr == nil && port != "" && strings.Trim(port, "0123456789") == "" {
		if _, _, hostErr := parseTrustedHost(host); hostErr == nil {
			return "", netip.Addr{}, fmt.Errorf("request.redirect_trusted_hosts entry %q has a port; a host is trusted on every port, so give the host alone: %s", raw, host)
		}
	}
	return "", netip.Addr{}, fmt.Errorf("request.redirect_trusted_hosts entry %q is not a host name, a glob of one such as *.example.com or an IP address; give the host alone, without a scheme, port or path", raw)
}
