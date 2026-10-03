//go:build !select_request_types || request_type_http

package fetch

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// What a followed redirect carries (redirecttrust.go).

// seenRequest is one request as a server of a redirect chain received it.
type seenRequest struct {
	method, host, path string
	header             http.Header
	body               string
	// raw is the request as the bytes it was sent as, kept by a wire server.
	raw string
	// certificate is the common name of the TLS client certificate the
	// request's connection presented, "" for none.
	certificate string
}

// carried is the request's headers as lines, sorted by name: everything it
// was sent with, the transport's own Accept-Encoding included.
func (r seenRequest) carried() string {
	names := make([]string, 0, len(r.header))
	for name := range r.header {
		names = append(names, name)
	}
	sort.Strings(names)
	var lines []string
	for _, name := range names {
		for _, value := range r.header[name] {
			lines = append(lines, name+": "+value)
		}
	}
	return strings.Join(lines, "\n")
}

// redirectChain is the servers a test's redirects lead through. They share
// one table of answers, by path, and one record of what each request carried,
// in the order the requests arrived; which server a request was sent to shows
// in its Host.
type redirectChain struct {
	mu     sync.Mutex
	seen   []seenRequest
	routes map[string]chainRoute
}

// chainRoute is the answer to a path: a status, and a Location for a redirect.
// A path without one is answered 200.
type chainRoute struct {
	status   int
	location string
}

func newRedirectChain() *redirectChain {
	return &redirectChain{routes: map[string]chainRoute{}}
}

// redirect answers path with status and a Location.
func (c *redirectChain) redirect(path string, status int, location string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.routes[path] = chainRoute{status, location}
}

// record keeps a request and returns the answer to it.
func (c *redirectChain) record(r *http.Request, raw string) chainRoute {
	body, _ := io.ReadAll(r.Body)
	seen := seenRequest{method: r.Method, host: r.Host, path: r.URL.Path, header: r.Header.Clone(), body: string(body), raw: raw + string(body)}
	if r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
		seen.certificate = r.TLS.PeerCertificates[0].Subject.CommonName
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seen = append(c.seen, seen)
	return c.routes[r.URL.Path]
}

func (c *redirectChain) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	route := c.record(r, "")
	if route.location != "" {
		w.Header().Set("Location", route.location)
	}
	if route.status != 0 {
		w.WriteHeader(route.status)
	}
	_, _ = io.WriteString(w, "value 1\n")
}

// requests is every request received since the last call.
func (c *redirectChain) requests() []seenRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.seen
	c.seen = nil
	return out
}

// server is a server of the chain over plain HTTP, at 127.0.0.1.
func (c *redirectChain) server(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(c)
	t.Cleanup(server.Close)
	return server
}

// tlsServer is a server of the chain over HTTPS, at 127.0.0.1.
func (c *redirectChain) tlsServer(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewTLSServer(c)
	t.Cleanup(server.Close)
	return server
}

// wireServer is a server of the chain that reads its requests off the
// connection itself, so each is kept as the bytes it was sent as. It answers
// one request to a connection. It returns its URL.
func (c *redirectChain) wireServer(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go c.answerOnWire(conn)
		}
	}()
	return "http://" + listener.Addr().String()
}

func (c *redirectChain) answerOnWire(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	var raw bytes.Buffer
	reader := bufio.NewReader(io.TeeReader(conn, &raw))
	r, err := http.ReadRequest(reader)
	if err != nil {
		return
	}
	// The head is what was read up to the body, which record reads.
	head := raw.String()[:raw.Len()-reader.Buffered()]
	route := c.record(r, head)
	status, location := "200 OK", ""
	if route.status != 0 {
		status = fmt.Sprintf("%d %s", route.status, http.StatusText(route.status))
	}
	if route.location != "" {
		location = "Location: " + route.location + "\r\n"
	}
	_, _ = io.WriteString(conn, "HTTP/1.1 "+status+"\r\n"+location+"Content-Length: 8\r\nConnection: close\r\n\r\nvalue 1\n")
}

// atHost is a server's URL with another name for its host, on the same port:
// a server at 127.0.0.1 is reached as localhost too, which is another host,
// and so another origin.
func atHost(t *testing.T, serverURL, host string) string {
	t.Helper()
	u, err := url.Parse(serverURL)
	if err != nil {
		t.Fatal(err)
	}
	u.Host = net.JoinHostPort(host, u.Port())
	return u.String()
}

// trustCollector follows redirects and sends a credential, a cookie, an API
// key and the three neutral headers; forwardedTenant is a header forwarded
// from the probe.
func trustCollector(t *testing.T, edit func(*model.Collector)) *model.Collector {
	t.Helper()
	return httpCollector(t, func(c *model.Collector) {
		c.Request.FollowRedirects = true
		c.Request.BearerToken = "s3cret"
		c.Request.Headers = map[string]string{"X-Api-Key": "k3y", "Cookie": "session=abc", "Accept": "application/json", "Accept-Language": "en", "User-Agent": "probe/1"}
		if edit != nil {
			edit(c)
		}
	})
}

var forwardedTenant = http.Header{"X-Tenant": {"acme"}}

// What trustCollector's first request carries, and so a trusted hop, and
// what is left of it for a hop that is not trusted.
const (
	carriesEverything = "Accept: application/json\nAccept-Encoding: gzip\nAccept-Language: en\nAuthorization: Bearer s3cret\nCookie: session=abc\nUser-Agent: probe/1\nX-Api-Key: k3y\nX-Tenant: acme"
	carriesNeutral    = "Accept: application/json\nAccept-Encoding: gzip\nAccept-Language: en\nUser-Agent: probe/1"
)

// follow fetches target with c and returns what the chain's servers received.
func follow(t *testing.T, chain *redirectChain, c *model.Collector, target string) []seenRequest {
	t.Helper()
	chain.requests()
	if _, err := FetchCollector(context.Background(), target, c, RequestOverrides{}, forwardedTenant); err != nil {
		t.Fatalf("fetching %s: %v", target, err)
	}
	return chain.requests()
}

// assertCarried fails unless the requests are as many as want and carry, one
// by one, exactly the headers want lists.
func assertCarried(t *testing.T, name string, got []seenRequest, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: %d requests were sent, want %d: %+v", name, len(got), len(want), got)
	}
	for i := range want {
		if got[i].carried() != want[i] {
			t.Errorf("%s: request %d, to %s%s, carried\n%s\nwant\n%s", name, i+1, got[i].host, got[i].path, got[i].carried(), want[i])
		}
	}
}

// checkRedirectBefore is the client's CheckRedirect as it was before the
// exporter decided what a redirect carries, copied here: with it, what a hop
// carries is what Go gives it.
func checkRedirectBefore(req *http.Request, via []*http.Request) error {
	ctx := req.Context()
	if req.Response != nil {
		traceOutcome(ctx, req.Response.Status)
	}
	traceRequest(ctx, req.Method, req.URL.String(), req.Header, req.Host, true)
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	return checkRedirect(ctx, req.URL)
}

// goAlone sends the request first was — its method, headers, Host and body,
// as the collector made them — to rawURL once more, on the collector's
// transport and with the CheckRedirect of before, and returns what the
// chain's servers received: what the same request was on the wire when Go
// alone decided what its redirects carry.
func goAlone(t *testing.T, chain *redirectChain, c *model.Collector, rawURL string, first seenRequest) []seenRequest {
	t.Helper()
	chain.requests()
	client, err := HTTPClient(TransportSettings{TLS: c.Request.TLS, policy: policyOf(c)}, true, 0)
	if err != nil {
		t.Fatal(err)
	}
	client.CheckRedirect = checkRedirectBefore
	var body io.Reader
	if first.body != "" {
		body = strings.NewReader(first.body)
	}
	req, err := http.NewRequestWithContext(context.Background(), first.method, rawURL, body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header = first.header.Clone()
	// The transport writes these two itself.
	req.Header.Del("Accept-Encoding")
	req.Header.Del("Content-Length")
	if first.host != req.URL.Host {
		req.Host = first.host
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("the request as Go alone sends it: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return chain.requests()
}

// A redirect that stays on the origin the request was made to — the same
// scheme, host and port, whether its Location is a path or names the origin
// in full — carries everything the first request carried: the collector's
// headers, the forwarded one, the credential and the cookie. No Referer is
// added.
func TestARedirectOnTheSameOriginCarriesEverything(t *testing.T) {
	chain := newRedirectChain()
	server := chain.server(t)
	c := trustCollector(t, nil)
	for name, location := range map[string]string{"a path": "/end", "the origin in full": server.URL + "/end", "the host in capitals": strings.Replace(server.URL, "http://", "HTTP://", 1) + "/end"} {
		chain.redirect("/start", http.StatusFound, location)
		got := follow(t, chain, c, server.URL+"/start")
		assertCarried(t, name, got, carriesEverything, carriesEverything)
		if got[1].path != "/end" {
			t.Errorf("%s: the second request asked for %s", name, got[1].path)
		}
	}
}

// A redirect to another origin — another host, the same host on another
// port, or another scheme — that the collector does not list carries only
// Accept, Accept-Language and User-Agent, as the first request had them: no
// credential, no cookie, no other header of the collector's or the probe's,
// and no Referer.
func TestARedirectToAnotherOriginCarriesOnlyNeutralHeaders(t *testing.T) {
	chain := newRedirectChain()
	server, other, secure := chain.server(t), chain.server(t), chain.tlsServer(t)
	c := trustCollector(t, func(c *model.Collector) { c.Request.TLS.InsecureSkipVerify = true })
	for name, hop := range map[string]struct{ first, location string }{
		"another host":                  {server.URL, atHost(t, server.URL, "localhost")},
		"the same host on another port": {server.URL, other.URL},
		"https to http":                 {secure.URL, server.URL},
		"http to https":                 {server.URL, secure.URL},
	} {
		chain.redirect("/start", http.StatusFound, hop.location+"/end")
		got := follow(t, chain, c, hop.first+"/start")
		assertCarried(t, name, got, carriesEverything, carriesNeutral)
		if want := strings.TrimPrefix(strings.TrimPrefix(hop.location, "http://"), "https://"); len(got) == 2 && got[1].host != want {
			t.Errorf("%s: the second request went to %s, want %s", name, got[1].host, want)
		}
	}
}

// The same holds for a host written as an IPv6 address, which is another
// host than 127.0.0.1; and an address listed in redirect_trusted_hosts,
// bare or in brackets, trusts the host written as that address.
func TestARedirectToAnIPv6HostIsJudgedByItsAddress(t *testing.T) {
	listener, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skipf("no IPv6 loopback here: %v", err)
	}
	chain := newRedirectChain()
	server := chain.server(t)
	six := httptest.NewUnstartedServer(chain)
	_ = six.Listener.Close()
	six.Listener = listener
	six.Start()
	t.Cleanup(six.Close)
	chain.redirect("/start", http.StatusFound, six.URL+"/end")
	assertCarried(t, "not listed", follow(t, chain, trustCollector(t, nil), server.URL+"/start"), carriesEverything, carriesNeutral)
	for _, entry := range []string{"::1", "[::1]", "0:0:0:0:0:0:0:1"} {
		c := trustCollector(t, func(c *model.Collector) { c.Request.RedirectTrustedHosts = []string{entry} })
		assertCarried(t, entry, follow(t, chain, c, server.URL+"/start"), carriesEverything, carriesEverything)
	}
}

// A host in request.redirect_trusted_hosts is sent everything, on whatever
// port: listed by its name, in any case, by a glob, by its address when the
// redirect writes the address, or by "*", which trusts every host. An entry
// that does not match trusts nothing, and neither does the name with a final
// dot, which is another name, nor an address for a host written as a name
// that resolves to it.
func TestATrustedHostIsSentEverything(t *testing.T) {
	chain := newRedirectChain()
	server := chain.server(t)
	byName, byAddress := atHost(t, server.URL, "localhost"), server.URL
	for _, tc := range []struct {
		entries         []string
		first, location string
		want            string
	}{
		{[]string{"localhost"}, byAddress, byName, carriesEverything},
		{[]string{"LocalHost"}, byAddress, byName, carriesEverything},
		{[]string{"localhost."}, byAddress, byName, carriesNeutral},
		{[]string{"other.example", "local*"}, byAddress, byName, carriesEverything},
		{[]string{"l?calhost"}, byAddress, byName, carriesEverything},
		{[]string{"*"}, byAddress, byName, carriesEverything},
		{[]string{"127.0.0.1"}, byName, byAddress, carriesEverything},
		{[]string{"*"}, byName, byAddress, carriesEverything},
		{[]string{"other.example", "*.localhost", "localhost.example"}, byAddress, byName, carriesNeutral},
		{[]string{"127.0.0.1"}, byAddress, byName, carriesNeutral},
		{[]string{"127.0.0.2", "::1"}, byName, byAddress, carriesNeutral},
		{nil, byName, byAddress, carriesNeutral},
	} {
		c := trustCollector(t, func(c *model.Collector) { c.Request.RedirectTrustedHosts = tc.entries })
		chain.redirect("/start", http.StatusFound, tc.location+"/end")
		assertCarried(t, fmt.Sprintf("%q from %s", tc.entries, tc.first), follow(t, chain, c, tc.first+"/start"), carriesEverything, tc.want)
	}
}

// A trusted host is trusted on every port and over every scheme the
// collector allows: listed, the same host on another port and over HTTPS is
// sent everything.
func TestATrustedHostIsTrustedOnEveryPortAndScheme(t *testing.T) {
	chain := newRedirectChain()
	server, other, secure := chain.server(t), chain.server(t), chain.tlsServer(t)
	c := trustCollector(t, func(c *model.Collector) {
		c.Request.RedirectTrustedHosts = []string{"127.0.0.1"}
		c.Request.TLS.InsecureSkipVerify = true
	})
	for name, location := range map[string]string{"another port": other.URL, "https": secure.URL} {
		chain.redirect("/start", http.StatusFound, location+"/end")
		assertCarried(t, name, follow(t, chain, c, server.URL+"/start"), carriesEverything, carriesEverything)
	}
}

// A subdomain of the first host is another origin, and is sent only the
// neutral headers. Go alone took it for the first host's own: the same
// request, sent with the redirect check of before, hands the subdomain the
// credential, the cookie and every other header, and a Referer naming the
// URL it came from.
func TestASubdomainIsAnotherOrigin(t *testing.T) {
	chain := newRedirectChain()
	// The chain's server stands in for the proxy every name is reached
	// through, and so for every host.
	behindProxy(t, chain.server(t).URL)
	chain.redirect("/start", http.StatusFound, "http://metrics.api.example/end")
	c := trustCollector(t, nil)
	got := follow(t, chain, c, "http://api.example/start")
	assertCarried(t, "the exporter", got, carriesEverything, carriesNeutral)
	if len(got) == 2 && got[1].host != "metrics.api.example" {
		t.Fatalf("the second request went to %s", got[1].host)
	}
	before := goAlone(t, chain, c, "http://api.example/start", got[0])
	assertCarried(t, "Go alone", before, carriesEverything, strings.Replace(carriesEverything, "User-Agent", "Referer: http://api.example/start\nUser-Agent", 1))

	// Listed, the subdomain is sent everything but the Referer.
	listed := trustCollector(t, func(c *model.Collector) { c.Request.RedirectTrustedHosts = []string{"*.api.example"} })
	assertCarried(t, "listed", follow(t, chain, listed, "http://api.example/start"), carriesEverything, carriesEverything)
}

// Each hop is judged on its own, against the first URL: a chain that leaves
// the origin for a host that is not trusted and comes back carries
// everything again once it is back. Go alone, having dropped the credential
// for the other host, never sent it again.
func TestAChainThatComesBackCarriesEverythingAgain(t *testing.T) {
	chain := newRedirectChain()
	server := chain.server(t)
	elsewhere := atHost(t, server.URL, "localhost")
	chain.redirect("/start", http.StatusFound, elsewhere+"/away")
	chain.redirect("/away", http.StatusFound, server.URL+"/back")
	c := trustCollector(t, nil)
	got := follow(t, chain, c, server.URL+"/start")
	assertCarried(t, "there and back", got, carriesEverything, carriesNeutral, carriesEverything)
	before := goAlone(t, chain, c, server.URL+"/start", got[0])
	withoutCredentials := "Accept: application/json\nAccept-Encoding: gzip\nAccept-Language: en\nReferer: " + elsewhere + "/away\nUser-Agent: probe/1\nX-Api-Key: k3y\nX-Tenant: acme"
	if len(before) != 3 || before[2].carried() != withoutCredentials {
		t.Fatalf("Go alone sent, back on the origin:\n%+v\nwant\n%s", before, withoutCredentials)
	}
}

// A chain of three redirects with mixed trust: to a listed host, which is
// sent everything; on to the first host on another port, which is neither
// the origin nor listed, and is sent the neutral headers; and on to the
// listed host on that port, which is sent everything again.
func TestAChainOfMixedTrust(t *testing.T) {
	chain := newRedirectChain()
	server, other := chain.server(t), chain.server(t)
	chain.redirect("/start", http.StatusFound, atHost(t, server.URL, "localhost")+"/listed")
	chain.redirect("/listed", http.StatusMovedPermanently, other.URL+"/unlisted")
	chain.redirect("/unlisted", http.StatusSeeOther, atHost(t, other.URL, "localhost")+"/end")
	c := trustCollector(t, func(c *model.Collector) { c.Request.RedirectTrustedHosts = []string{"localhost"} })
	got := follow(t, chain, c, server.URL+"/start")
	assertCarried(t, "mixed", got, carriesEverything, carriesEverything, carriesNeutral, carriesEverything)
	if got[3].path != "/end" {
		t.Fatalf("the last request asked for %s", got[3].path)
	}
}

// A Referer the collector sets in request.headers is one of its headers: on
// the first request and on a trusted hop as it was written, not replaced by
// the URL the redirect came from, and withheld from a hop that is not
// trusted.
func TestAConfiguredRefererIsACollectorHeader(t *testing.T) {
	chain := newRedirectChain()
	server := chain.server(t)
	c := trustCollector(t, func(c *model.Collector) { c.Request.Headers["Referer"] = "https://dashboard.example/" })
	everything := strings.Replace(carriesEverything, "User-Agent", "Referer: https://dashboard.example/\nUser-Agent", 1)
	chain.redirect("/start", http.StatusFound, "/end")
	assertCarried(t, "same origin", follow(t, chain, c, server.URL+"/start"), everything, everything)
	chain.redirect("/start", http.StatusFound, atHost(t, server.URL, "localhost")+"/end")
	assertCarried(t, "another origin", follow(t, chain, c, server.URL+"/start"), everything, carriesNeutral)
}

// The credential is carried to a trusted host however the request came by
// it — the collector's bearer token or basic auth, or an Authorization
// forwarded from the probe — and to no host that is not trusted.
func TestEveryKindOfCredentialFollowsTheRule(t *testing.T) {
	chain := newRedirectChain()
	server := chain.server(t)
	chain.redirect("/start", http.StatusFound, atHost(t, server.URL, "localhost")+"/end")
	for name, tc := range map[string]struct {
		edit      func(*model.Collector)
		forwarded http.Header
		want      string
	}{
		"bearer token": {func(c *model.Collector) { c.Request.BearerToken = "s3cret" }, nil, "Bearer s3cret"},
		"basic auth": {func(c *model.Collector) {
			c.Request.BasicAuth = &model.BasicAuth{Username: "scraper", Password: "pass"}
		}, nil, "Basic c2NyYXBlcjpwYXNz"},
		"forwarded": {func(c *model.Collector) { c.Request.ForwardAuthorization = true }, http.Header{"Authorization": {"Bearer from-the-probe"}}, "Bearer from-the-probe"},
	} {
		for _, trusted := range []bool{true, false} {
			c := httpCollector(t, func(c *model.Collector) {
				c.Request.FollowRedirects = true
				tc.edit(c)
				if trusted {
					c.Request.RedirectTrustedHosts = []string{"localhost"}
				}
			})
			chain.requests()
			if _, err := FetchCollector(context.Background(), server.URL+"/start", c, RequestOverrides{}, tc.forwarded); err != nil {
				t.Fatal(err)
			}
			got := chain.requests()
			if len(got) != 2 || got[0].header.Get("Authorization") != tc.want {
				t.Fatalf("%s: %+v", name, got)
			}
			if sent := got[1].header.Get("Authorization"); trusted && sent != tc.want || !trusted && sent != "" {
				t.Errorf("%s, trusted %v: the redirect was sent Authorization %q", name, trusted, sent)
			}
		}
	}
}

// A credential in the target URL itself, as user:password@host, belongs to
// that URL: Go sends it as basic auth, and takes it along only with a
// redirect whose Location is a path, which keeps the URL's host and its
// credential. A Location that names a host, the first origin or a trusted
// one, is a URL without it, and is sent none.
func TestACredentialInTheTargetURLStaysWithItsURL(t *testing.T) {
	chain := newRedirectChain()
	server := chain.server(t)
	c := httpCollector(t, func(c *model.Collector) {
		c.Request.FollowRedirects = true
		c.Request.RedirectTrustedHosts = []string{"localhost"}
	})
	target := strings.Replace(server.URL, "http://", "http://scraper:pass@", 1) + "/start"
	for location, want := range map[string]string{
		"/end":              "Basic c2NyYXBlcjpwYXNz",
		server.URL + "/end": "",
		atHost(t, server.URL, "localhost") + "/end": "",
	} {
		chain.redirect("/start", http.StatusFound, location)
		chain.requests()
		if _, err := FetchCollector(context.Background(), target, c, RequestOverrides{}, nil); err != nil {
			t.Fatal(err)
		}
		got := chain.requests()
		if len(got) != 2 || got[0].header.Get("Authorization") != "Basic c2NyYXBlcjpwYXNz" || got[1].header.Get("Authorization") != want {
			t.Errorf("Location %s: %+v, want the redirect sent Authorization %q", location, got, want)
		}
	}
}

// A Host the collector sets is kept across a redirect whose Location is a
// path, as before, and is not sent to another host, trusted or not: a
// Location without a scheme, //host/path, is one Go kept it across, whatever
// host it named.
func TestAHostOverrideStaysOnTheOrigin(t *testing.T) {
	chain := newRedirectChain()
	server := chain.server(t)
	c := trustCollector(t, func(c *model.Collector) { c.Request.Headers["Host"] = "vhost.example" })
	chain.redirect("/start", http.StatusFound, "/end")
	if got := follow(t, chain, c, server.URL+"/start"); len(got) != 2 || got[0].host != "vhost.example" || got[1].host != "vhost.example" {
		t.Fatalf("a redirect to a path: %+v", got)
	}
	elsewhere := strings.TrimPrefix(atHost(t, server.URL, "localhost"), "http://")
	chain.redirect("/start", http.StatusFound, "//"+elsewhere+"/end")
	got := follow(t, chain, c, server.URL+"/start")
	assertCarried(t, "a Location without a scheme", got, carriesEverything, carriesNeutral)
	if got[1].host != elsewhere {
		t.Fatalf("the redirect was sent the Host %s, want its own, %s", got[1].host, elsewhere)
	}
	if before := goAlone(t, chain, c, server.URL+"/start", got[0]); len(before) != 2 || before[1].host != "vhost.example" {
		t.Fatalf("Go alone: %+v, want the collector's Host sent on", before)
	}
	listed := trustCollector(t, func(c *model.Collector) {
		c.Request.Headers["Host"] = "vhost.example"
		c.Request.RedirectTrustedHosts = []string{"localhost"}
	})
	got = follow(t, chain, listed, server.URL+"/start")
	assertCarried(t, "a listed host", got, carriesEverything, carriesEverything)
	if got[1].host != elsewhere {
		t.Fatalf("the listed host was sent the Host %s, want its own, %s", got[1].host, elsewhere)
	}
}

// postCollector posts a JSON body.
func postCollector(t *testing.T, edit func(*model.Collector)) *model.Collector {
	t.Helper()
	return trustCollector(t, func(c *model.Collector) {
		c.Request.Method = http.MethodPost
		c.Request.Body = `{"query":"up"}`
		c.Request.Headers["Content-Type"] = "application/json"
		if edit != nil {
			edit(c)
		}
	})
}

// A 307 or a 308 has the request sent again as it was, body included. To a
// host that is not trusted that is refused: the probe fails, saying which
// host would have been sent the body and which setting trusts it, the
// redirect's URL shown as other errors show one, and the host receives
// nothing. The refusal is not retried.
func TestARedirectThatWouldSendTheBodyToAnUntrustedHostIsRefused(t *testing.T) {
	chain := newRedirectChain()
	server := chain.server(t)
	elsewhere := atHost(t, server.URL, "localhost")
	c := postCollector(t, func(c *model.Collector) {
		c.Request.Retry = model.RetryConfig{Attempts: 2, NonIdempotent: true}
	})
	for _, status := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		chain.redirect("/start", status, elsewhere+"/end?token=abc")
		chain.requests()
		response, err := FetchCollector(context.Background(), server.URL+"/start", c, RequestOverrides{}, forwardedTenant)
		want := "redirect to " + elsewhere + "/end?token=<redacted> refused: it would send the request body to localhost, which is not the origin the request was made to and is not listed in request.redirect_trusted_hosts"
		if response != nil || err == nil || !strings.HasSuffix(err.Error(), want) || strings.Contains(err.Error(), "abc") {
			t.Fatalf("%d: response=%v err=%v, want an error ending in %q", status, response, err, want)
		}
		if got := chain.requests(); len(got) != 1 || got[0].host == strings.TrimPrefix(elsewhere, "http://") {
			t.Fatalf("%d: %d requests were sent, want the first one alone: %+v", status, len(got), got)
		}
	}
	// A body given by the probe is a body like the collector's.
	body := `{"query":"other"}`
	if _, err := FetchCollector(context.Background(), server.URL+"/start", c, RequestOverrides{Body: &body}, nil); err == nil || !strings.Contains(err.Error(), "it would send the request body to localhost") {
		t.Fatalf("a body from the probe: %v", err)
	}
	// What refuses the host itself is said first, as it was.
	denied := postCollector(t, func(c *model.Collector) { c.Request.DeniedTargets = []string{"localhost"} })
	if _, err := FetchCollector(context.Background(), server.URL+"/start", denied, RequestOverrides{}, nil); !errors.Is(err, ErrTargetRefused) {
		t.Fatalf("a denied host: %v, want it refused by request.denied_targets", err)
	}
}

// To a trusted destination, the same origin or a listed host, the 307 or 308
// is followed and the body sent again, with everything else.
func TestARedirectSendsTheBodyAgainToATrustedHost(t *testing.T) {
	chain := newRedirectChain()
	server := chain.server(t)
	c := postCollector(t, func(c *model.Collector) { c.Request.RedirectTrustedHosts = []string{"localhost"} })
	everything := strings.Replace(carriesEverything, "Cookie", "Content-Length: 14\nContent-Type: application/json\nCookie", 1)
	for _, status := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		for name, location := range map[string]string{"the same origin": "/end", "a listed host": atHost(t, server.URL, "localhost") + "/end"} {
			chain.redirect("/start", status, location)
			got := follow(t, chain, c, server.URL+"/start")
			assertCarried(t, name, got, everything, everything)
			if got[1].method != http.MethodPost || got[1].body != `{"query":"up"}` {
				t.Errorf("%d to %s: the redirect was sent %s with the body %q", status, name, got[1].method, got[1].body)
			}
		}
	}
}

// After a 301, a 302 or a 303 Go turns a POST into a GET without the body
// and without the headers that describe it, and so does the exporter: the
// hop has no body to keep from anyone, so it is followed, with everything
// else to a trusted host and with the neutral headers to any other. A GET
// that a 307 redirects has no body either, and is followed too.
func TestARedirectThatDropsTheBodyIsFollowed(t *testing.T) {
	chain := newRedirectChain()
	server := chain.server(t)
	c := postCollector(t, nil)
	first := strings.Replace(carriesEverything, "Cookie", "Content-Length: 14\nContent-Type: application/json\nCookie", 1)
	for _, status := range []int{http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther} {
		for location, want := range map[string]string{"/end": carriesEverything, atHost(t, server.URL, "localhost") + "/end": carriesNeutral} {
			chain.redirect("/start", status, location)
			got := follow(t, chain, c, server.URL+"/start")
			assertCarried(t, fmt.Sprintf("%d to %s", status, location), got, first, want)
			if got[0].method != http.MethodPost || got[1].method != http.MethodGet || got[1].body != "" {
				t.Errorf("%d to %s: the redirect was sent %s with the body %q", status, location, got[1].method, got[1].body)
			}
		}
	}
	chain.redirect("/start", http.StatusTemporaryRedirect, atHost(t, server.URL, "localhost")+"/end")
	got := follow(t, chain, trustCollector(t, nil), server.URL+"/start")
	assertCarried(t, "a GET after a 307", got, carriesEverything, carriesNeutral)
	if got[1].method != http.MethodGet {
		t.Fatalf("the redirect was sent %s", got[1].method)
	}
}

// A retry starts again at the first URL, with everything the first request
// carried, whatever the attempt before it was redirected to.
func TestARetryStartsAgainFromTheFirstURL(t *testing.T) {
	chain := newRedirectChain()
	server := chain.server(t)
	chain.redirect("/start", http.StatusFound, atHost(t, server.URL, "localhost")+"/busy")
	chain.redirect("/busy", http.StatusServiceUnavailable, "")
	c := trustCollector(t, func(c *model.Collector) { c.Request.Retry.Attempts = 1 })
	chain.requests()
	response, err := FetchCollector(context.Background(), server.URL+"/start", c, RequestOverrides{}, forwardedTenant)
	if err != nil || response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("response=%+v err=%v", response, err)
	}
	assertCarried(t, "two attempts", chain.requests(), carriesEverything, carriesNeutral, carriesEverything, carriesNeutral)
}

// The answer of a host a redirect led to, which was not sent the request's
// headers, names that host, so its 401 can be told from the target's. An
// answer from the target itself, from a trusted hop, from a chain that came
// back to the origin, or from a host nothing was withheld from names none.
func TestAnAnswerSaysWhenItsHostWasNotSentTheHeaders(t *testing.T) {
	chain := newRedirectChain()
	server := chain.server(t)
	elsewhere := atHost(t, server.URL, "localhost")
	fetchFrom := func(c *model.Collector) string {
		t.Helper()
		response, err := FetchCollector(context.Background(), server.URL+"/start", c, RequestOverrides{}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return response.RedirectWithheld
	}
	chain.redirect("/start", http.StatusFound, elsewhere+"/end")
	chain.redirect("/end", http.StatusUnauthorized, "")
	if got, want := fetchFrom(trustCollector(t, nil)), "from localhost, where a redirect led: the collector's headers and credentials were not sent to that host, which is not the origin the request was made to; list it in request.redirect_trusted_hosts if it is to be sent them"; got != want {
		t.Errorf("an untrusted hop: RedirectWithheld is %q, want %q", got, want)
	}
	if got := fetchFrom(trustCollector(t, func(c *model.Collector) { c.Request.RedirectTrustedHosts = []string{"localhost"} })); got != "" {
		t.Errorf("a trusted hop: RedirectWithheld is %q", got)
	}
	// A collector that sends nothing of its own has nothing withheld.
	if got := fetchFrom(httpCollector(t, func(c *model.Collector) { c.Request.FollowRedirects = true })); got != "" {
		t.Errorf("nothing to withhold: RedirectWithheld is %q", got)
	}
	chain.redirect("/end", http.StatusFound, server.URL+"/back")
	chain.redirect("/back", http.StatusUnauthorized, "")
	if got := fetchFrom(trustCollector(t, nil)); got != "" {
		t.Errorf("back on the origin: RedirectWithheld is %q", got)
	}
	chain.redirect("/start", http.StatusUnauthorized, "")
	if got := fetchFrom(trustCollector(t, nil)); got != "" {
		t.Errorf("no redirect: RedirectWithheld is %q", got)
	}
}

// A debug probe's record of a redirect shows the headers as the hop was sent
// them, and, for a hop that is not trusted, the names of the ones withheld:
// names only, sorted, the collector's Host among them when it was not sent.
func TestTheTraceShowsARedirectAsItWasSent(t *testing.T) {
	chain := newRedirectChain()
	server := chain.server(t)
	elsewhere := strings.TrimPrefix(atHost(t, server.URL, "localhost"), "http://")
	chain.redirect("/start", http.StatusFound, "/next")
	chain.redirect("/next", http.StatusFound, "//"+elsewhere+"/end")
	c := trustCollector(t, func(c *model.Collector) { c.Request.Headers["Host"] = "vhost.example" })
	ctx, trace := WithRequestTrace(context.Background())
	if _, err := FetchCollector(ctx, server.URL+"/start", c, RequestOverrides{}, forwardedTenant); err != nil {
		t.Fatal(err)
	}
	traced := trace.Requests()
	if len(traced) != 3 {
		t.Fatalf("%d requests traced: %+v", len(traced), traced)
	}
	lines := func(r TracedRequest) string { return seenRequest{header: r.Header}.carried() }
	// The trace holds what the exporter set, the Host with it, and not the
	// transport's own Accept-Encoding.
	sent := "Accept: application/json\nAccept-Language: en\nAuthorization: Bearer s3cret\nCookie: session=abc\nHost: vhost.example\nUser-Agent: probe/1\nX-Api-Key: k3y\nX-Tenant: acme"
	if lines(traced[0]) != sent || lines(traced[1]) != sent || len(traced[0].Withheld)+len(traced[1].Withheld) != 0 {
		t.Errorf("the first two requests are traced as\n%s\nand\n%s\nwant\n%s", lines(traced[0]), lines(traced[1]), sent)
	}
	if want := "Accept: application/json\nAccept-Language: en\nUser-Agent: probe/1"; lines(traced[2]) != want || !traced[2].Redirect {
		t.Errorf("the untrusted hop is traced as\n%s\nwant\n%s", lines(traced[2]), want)
	}
	if got := strings.Join(traced[2].Withheld, ", "); got != "Authorization, Cookie, Host, X-Api-Key, X-Tenant" {
		t.Errorf("the untrusted hop's withheld headers are %q", got)
	}
}

// The limit of ten redirects holds as it did, on the origin and off it.
func TestTheRedirectLimitStillHolds(t *testing.T) {
	chain := newRedirectChain()
	server := chain.server(t)
	chain.redirect("/loop", http.StatusFound, "/loop")
	_, err := FetchCollector(context.Background(), server.URL+"/loop", trustCollector(t, nil), RequestOverrides{}, nil)
	if err == nil || !strings.Contains(err.Error(), "stopped after 10 redirects") {
		t.Fatalf("err=%v", err)
	}
	if got := chain.requests(); len(got) != 10 {
		t.Fatalf("%d requests were sent, want 10", len(got))
	}
}

// What a redirect carries is decided from the request and its context, not
// kept in the client: one client, and its copy for a connection of its own,
// used at once for requests of two collectors, gives each request its own
// collector's answer.
func TestTheRedirectRuleRidesWithTheRequest(t *testing.T) {
	chain := newRedirectChain()
	server := chain.server(t)
	chain.redirect("/trusting", http.StatusFound, atHost(t, server.URL, "localhost")+"/trusted")
	chain.redirect("/wary", http.StatusFound, atHost(t, server.URL, "localhost")+"/untrusted")
	trusting := trustCollector(t, func(c *model.Collector) { c.Request.RedirectTrustedHosts = []string{"localhost"} })
	wary := trustCollector(t, nil)
	client, err := HTTPClient(TransportSettings{policy: policyOf(wary)}, true, 0)
	if err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	for i := 0; i < 8; i++ {
		c, path, sender := wary, "/wary", client
		if i%2 == 0 {
			c, path = trusting, "/trusting"
		}
		if i%4 < 2 {
			sender = onOwnConnection(client)
		}
		group.Add(1)
		go func() {
			defer group.Done()
			u, _ := url.Parse(server.URL + path)
			ctx, err := withTargetPolicy(context.Background(), c, u, nil)
			if err != nil {
				t.Error(err)
				return
			}
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
			req.Header.Set("Authorization", "Bearer s3cret")
			resp, err := sender.Do(req)
			if err != nil {
				t.Error(err)
				return
			}
			_ = resp.Body.Close()
		}()
	}
	group.Wait()
	got := chain.requests()
	if len(got) != 16 {
		t.Fatalf("%d requests were sent, want 16", len(got))
	}
	for _, r := range got {
		if sent := r.header.Get("Authorization"); r.path == "/trusted" && sent == "" || r.path == "/untrusted" && sent != "" {
			t.Errorf("%s was sent Authorization %q", r.path, sent)
		}
	}
}

// A client used without a target policy, as the OTLP exporter uses one for
// its endpoint, follows redirects as Go does and as it did: another host is
// sent the request's own headers and a Referer, and the body again after a
// 307.
func TestAClientWithoutATargetPolicyFollowsRedirectsAsGoDoes(t *testing.T) {
	chain := newRedirectChain()
	server := chain.server(t)
	chain.redirect("/v1/metrics", http.StatusTemporaryRedirect, atHost(t, server.URL, "localhost")+"/moved")
	client, err := HTTPClient(TransportSettings{}, true, 0)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, server.URL+"/v1/metrics", strings.NewReader("points"))
	req.Header.Set("X-Scope-Orgid", "tenant")
	req.Header.Set("Authorization", "Bearer otlp")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	got := chain.requests()
	if len(got) != 2 || got[1].body != "points" || got[1].header.Get("X-Scope-Orgid") != "tenant" || got[1].header.Get("Referer") != server.URL+"/v1/metrics" || got[1].header.Get("Authorization") != "" {
		t.Fatalf("%+v", got)
	}
}

// For a request that follows no redirect, and for one whose redirects stay
// on its origin, the requests on the wire are byte for byte what they were
// when Go alone decided what a redirect carries, but for the Referer Go
// added to each hop. The first request of each case is also what the same
// request is when sent with the redirect check of before, so the comparison
// is of like with like.
func TestRequestsOnTheOriginAreOnTheWireWhatTheyWere(t *testing.T) {
	chain := newRedirectChain()
	origin := chain.wireServer(t)
	host := strings.TrimPrefix(origin, "http://")
	for name, tc := range map[string]struct {
		edit   func(*model.Collector)
		status int
		// location is where /start redirects, "" for no redirect.
		location string
		// referer says Go added a Referer to the hop.
		referer bool
	}{
		"a GET, no redirect":               {nil, 0, "", false},
		"a POST, no redirect":              {func(c *model.Collector) { c.Request.Method, c.Request.Body = http.MethodPost, `{"query":"up"}` }, 0, "", false},
		"a GET, 302 to a path":             {nil, http.StatusFound, "/end?x=1", true},
		"a GET, 301 to the origin in full": {nil, http.StatusMovedPermanently, origin + "/end", true},
		"a POST, 307 to a path": {func(c *model.Collector) {
			c.Request.Method, c.Request.Body = http.MethodPost, `{"query":"up"}`
			c.Request.Headers["Content-Type"] = "application/json"
		}, http.StatusTemporaryRedirect, "/end", true},
		"a POST, 303 to a path": {func(c *model.Collector) {
			c.Request.Method, c.Request.Body = http.MethodPost, `{"query":"up"}`
			c.Request.Headers["Content-Type"] = "application/json"
		}, http.StatusSeeOther, "/end", true},
		"a Host of the collector's, 302 to a path":    {func(c *model.Collector) { c.Request.Headers["Host"] = "vhost.example" }, http.StatusFound, "/end", true},
		"a Referer of the collector's, 302 to a path": {func(c *model.Collector) { c.Request.Headers["Referer"] = "https://dashboard.example/" }, http.StatusFound, "/end", false},
	} {
		c := trustCollector(t, tc.edit)
		chain.redirect("/start", tc.status, tc.location)
		now := follow(t, chain, c, origin+"/start")
		before := goAlone(t, chain, c, origin+"/start", now[0])
		want := 1
		if tc.location != "" {
			want = 2
		}
		if len(now) != want || len(before) != want {
			t.Fatalf("%s: %d requests now and %d before, want %d", name, len(now), len(before), want)
		}
		if now[0].raw != before[0].raw {
			t.Errorf("%s: the first request is\n%q\nand, sent again with the check of before,\n%q", name, now[0].raw, before[0].raw)
		}
		if want == 1 {
			continue
		}
		was := before[1].raw
		if referer := "Referer: " + origin + "/start\r\n"; tc.referer {
			if !strings.Contains(was, referer) {
				t.Fatalf("%s: Go alone sent no Referer: %q", name, was)
			}
			was = strings.Replace(was, referer, "", 1)
		}
		if now[1].raw != was {
			t.Errorf("%s: the redirect is on the wire\n%q\nand was, but for the Referer,\n%q", name, now[1].raw, was)
		}
	}
	// One of them in full, so that what the comparison compares is seen.
	chain.redirect("/start", 0, "")
	got := follow(t, chain, trustCollector(t, nil), origin+"/start")
	want := "GET /start HTTP/1.1\r\nHost: " + host + "\r\nUser-Agent: probe/1\r\nAccept: application/json\r\nAccept-Language: en\r\nAuthorization: Bearer s3cret\r\nCookie: session=abc\r\nX-Api-Key: k3y\r\nX-Tenant: acme\r\nAccept-Encoding: gzip\r\n\r\n"
	if len(got) != 1 || got[0].raw != want {
		t.Fatalf("a request without a redirect is on the wire\n%q\nwant\n%q", got[0].raw, want)
	}
}

// Two URLs are one origin when their scheme, host and port are the same:
// the host in any case, the port written or the scheme's own. Anything else
// is another origin, and so is every other spelling of the host or the port,
// also one Go would route to the same place: the host with a final dot,
// which is another name to a resolver, an address in another form or with
// another zone, a port with a zero before it, and a host written with a
// character outside ASCII, which the target policy refuses and so is
// nobody's origin, not even its own.
func TestSameOrigin(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		same bool
	}{
		{"http://api.example/a", "http://api.example/b?x=1", true},
		{"http://api.example", "http://API.Example:80/", true},
		{"http://api.example.", "http://API.Example.:80/", true},
		{"http://api.example", "http://api.example./", false},
		{"http://localhost:8080", "http://localhost.:8080", false},
		{"http://api.example:8080", "http://api.example:08080", false},
		{"http://api.example", "http://api.example:080", false},
		{"http://127.0.0.1", "http://127.0.0.1.", false},
		{"http://127.0.0.1", "http://0x7f.0.0.1", false},
		{"http://127.0.0.1", "http://2130706433", false},
		{"http://127.0.0.1", "http://[::ffff:127.0.0.1]", false},
		{"http://[::1]", "http://[0:0:0:0:0:0:0:1]", false},
		{"http://[fe80::1%25eth0]", "http://[FE80::1%25eth0]/x", true},
		{"http://[fe80::1%25eth0]", "http://[fe80::1%25eth1]", false},
		{"http://[fe80::1%25eth0]", "http://[fe80::1%25ETH0]", false},
		{"http://[fe80::1%25eth0]", "http://[fe80::1]", false},
		{"http://bücher.example/a", "http://bücher.example/b", false},
		{"http://bücher.example", "http://BÜCHER.example", false},
		{"http://bücher.example", "http://xn--bcher-kva.example", false},
		{"http://origin.test", "http://\uff4frigin.test", false},
		{"http://origin.test", "http://origin\u3002test", false},
		{"http://origin.test", "http://ORIGIN\uff0etest", false},
		{"https://api.example", "https://api.example:443", true},
		{"http://user:pass@api.example", "http://api.example", true},
		{"http://[::1]:8080", "http://[::1]:8080/x", true},
		{"http://api.example", "https://api.example", false},
		{"http://api.example", "http://api.example:443", false},
		{"https://api.example", "https://api.example:80", false},
		{"http://api.example", "http://api.example:8080", false},
		{"http://api.example", "http://www.api.example", false},
		{"http://api.example", "http://example", false},
		{"http://127.0.0.1", "http://localhost", false},
		{"http://127.0.0.1", "http://127.1", false},
		{"http://api%2565xample", "http://api%2565xample", false},
	} {
		a, errA := url.Parse(tc.a)
		b, errB := url.Parse(tc.b)
		if errA != nil || errB != nil {
			t.Fatalf("%s, %s: %v %v", tc.a, tc.b, errA, errB)
		}
		if got := sameOrigin(a, b); got != tc.same || sameOrigin(b, a) != tc.same {
			t.Errorf("sameOrigin(%s, %s) = %v, want %v", tc.a, tc.b, got, tc.same)
		}
	}
}

// An entry of request.redirect_trusted_hosts matches the host a redirect
// names, as it is written there: a name or a glob against the name, in any
// case, a final dot being part of both; an address against a host written as
// that address, in whichever of its forms and with the same zone. A host
// written with a character outside ASCII is matched by no entry, "*"
// included. Nothing is looked up, so an address does not trust a name. A
// star matches dots too, so *example.com and api.example.* trust more than
// they seem to.
func TestTrustedRedirectHost(t *testing.T) {
	for _, tc := range []struct {
		entry, host string
		trusted     bool
	}{
		{"api.example", "api.example", true},
		{"API.Example", "Api.EXAMPLE", true},
		{"API.Example.", "api.example", false},
		{"api.example", "api.example.", false},
		{"API.Example.", "api.Example.", true},
		{"*.example", "a.example.", false},
		{"*.example.", "a.example.", true},
		{"*.example.", "a.example", false},
		{"*", "api.example.", true},
		{"*example.com", "evilexample.com", true},
		{"*.example.com", "evilexample.com", false},
		{"api.example.*", "api.example.evil.net", true},
		{"xn--bcher-kva.example", "xn--bcher-kva.example", true},
		{"xn--bcher-kva.example", "bücher.example", false},
		{"*.example", "bücher.example", false},
		{"*", "bücher.example", false},
		{"origin.test", "\uff4frigin.test", false},
		{"origin.test", "origin\u3002test", false},
		{"*", "origin\u3002test", false},
		{"*.example", "a.b.example", true},
		{"*.example", "example", false},
		{"api-?.example", "api-1.example", true},
		{"api-?.example", "api-10.example", false},
		{"*", "anything.example", true},
		{"*", "::1", true},
		{"::1", "::1", true},
		{"[::1]", "0:0:0:0:0:0:0:1", true},
		{"0:0:0:0:0:0:0:1", "::1", true},
		{"fe80::1", "fe80::1%eth0", false},
		{"fe80::1%eth0", "fe80::1", false},
		{"fe80::1%eth0", "fe80::1%eth1", false},
		{"fe80::1%eth0", "fe80::1%ETH0", false},
		{"[FE80::1%eth0]", "fe80:0::1%eth0", true},
		{"1.2.3.4.", "1.2.3.4", false},
		{"1.2.3.4.", "1.2.3.4.", true},
		{"192.0.2.7", "::ffff:192.0.2.7", true},
		{"192.0.2.7", "192.0.2.7", true},
		{"192.0.2.7", "192.0.2.8", false},
		{"192.0.2.7", "seven.example", false},
		{"127.0.0.1", "127.1", false},
		{"127.0.0.1", "localhost", false},
		{"10.0.0.0/8", "10.0.0.1", false},
		{"", "api.example", false},
	} {
		if got := trustedRedirectHost([]string{"other.example", tc.entry}, tc.host); got != tc.trusted {
			t.Errorf("entry %q, host %q: trusted %v, want %v", tc.entry, tc.host, got, tc.trusted)
		}
	}
}

// request.redirect_trusted_hosts takes host names, globs of them and IP
// addresses, as allowed_targets writes them; an empty entry, a network, a
// host with a port and a URL are refused when the configuration loads, each
// with what to write instead, and so are a glob written like a range of
// addresses, which would match names, wildcards alone in any spelling but
// "*", and an IPv6 address with a final dot. The key belongs to the types that follow
// redirects, and needs no follow_redirects beside it: following may be
// switched on by a probe.
func TestRedirectTrustedHostsEntries(t *testing.T) {
	generic := `is not a host name, a glob of one such as *.example.com or an IP address; give the host alone, without a scheme, port or path`
	network := `is a network; a redirect is trusted by the host its URL names, which is never looked up, so list each address a redirect names on its own, or the hosts by name`
	wildcards := `is made of wildcards alone and names no host; write "*" if every host is to be trusted, and otherwise a host name, a glob of one such as *.example.com or an IP address`
	addressGlob := func(example string) string {
		return `is a glob written like a range of addresses; a glob is matched against host names, and this one would trust every name that fits it, such as ` + example + `, so list each address a redirect names on its own`
	}
	for entry, message := range map[string]string{
		"":                        `collector "web" request.redirect_trusted_hosts has an empty entry`,
		"  ":                      `collector "web" request.redirect_trusted_hosts has an empty entry`,
		"https://api.example.com": `collector "web" request.redirect_trusted_hosts entry "https://api.example.com" ` + generic,
		"api.example.com/v1":      `collector "web" request.redirect_trusted_hosts entry "api.example.com/v1" ` + generic,
		"a b.example":             `collector "web" request.redirect_trusted_hosts entry "a b.example" ` + generic,
		"api.example.com:":        `collector "web" request.redirect_trusted_hosts entry "api.example.com:" ` + generic,
		"fe80::zz":                `collector "web" request.redirect_trusted_hosts entry "fe80::zz" ` + generic,
		"10.0.0.0/8":              `collector "web" request.redirect_trusted_hosts entry "10.0.0.0/8" ` + network,
		"fd00::/8":                `collector "web" request.redirect_trusted_hosts entry "fd00::/8" ` + network,
		"api.example.com:8443":    `collector "web" request.redirect_trusted_hosts entry "api.example.com:8443" has a port; a host is trusted on every port, so give the host alone: api.example.com`,
		"*.example.com:443":       `collector "web" request.redirect_trusted_hosts entry "*.example.com:443" has a port; a host is trusted on every port, so give the host alone: *.example.com`,
		"[::1]:8443":              `collector "web" request.redirect_trusted_hosts entry "[::1]:8443" has a port; a host is trusted on every port, so give the host alone: ::1`,
		"10.*":                    `collector "web" request.redirect_trusted_hosts entry "10.*" ` + addressGlob("10.evil.example"),
		"192.168.*.*":             `collector "web" request.redirect_trusted_hosts entry "192.168.*.*" ` + addressGlob("192.168.evil.example.evil.example"),
		"10.0.0.?":                `collector "web" request.redirect_trusted_hosts entry "10.0.0.?" ` + addressGlob("10.0.0.x"),
		"[10.*]":                  `collector "web" request.redirect_trusted_hosts entry "[10.*]" ` + addressGlob("10.evil.example"),
		"*.1.":                    `collector "web" request.redirect_trusted_hosts entry "*.1." ` + addressGlob("evil.example.1"),
		"**":                      `collector "web" request.redirect_trusted_hosts entry "**" ` + wildcards,
		"*.":                      `collector "web" request.redirect_trusted_hosts entry "*." ` + wildcards,
		"[*]":                     `collector "web" request.redirect_trusted_hosts entry "[*]" ` + wildcards,
		"*.*":                     `collector "web" request.redirect_trusted_hosts entry "*.*" ` + wildcards,
		"?":                       `collector "web" request.redirect_trusted_hosts entry "?" ` + wildcards,
		"*?":                      `collector "web" request.redirect_trusted_hosts entry "*?" ` + wildcards,
		"fe80::*":                 `collector "web" request.redirect_trusted_hosts entry "fe80::*" ` + generic,
		"*:*":                     `collector "web" request.redirect_trusted_hosts entry "*:*" ` + generic,
		"10.*:80":                 `collector "web" request.redirect_trusted_hosts entry "10.*:80" ` + generic,
		"::1.":                    `collector "web" request.redirect_trusted_hosts entry "::1." ` + generic,
		"[::1].":                  `collector "web" request.redirect_trusted_hosts entry "[::1]." ` + generic,
	} {
		c := model.Collector{Name: "web", Request: model.RequestConfig{Type: RequestTypeHTTP, RedirectTrustedHosts: []string{"api.example.com", entry}}}
		if err := ValidateRequest(&c); err == nil || err.Error() != message {
			t.Errorf("%q: %v, want %s", entry, err, message)
		}
	}
	good := []string{"api.example.com", "*.example.com", "API-?.Internal.", "my_service", "192.168.1.7", "::1", "[fd00::1]", "fe80::1%eth0", "*", " * ", "10-*", "*.10.example", "1.2.3.4.", "::1:80"}
	c := model.Collector{Name: "web", Request: model.RequestConfig{Type: RequestTypeHTTP, RedirectTrustedHosts: good}}
	if err := ValidateRequest(&c); err != nil {
		t.Fatal(err)
	}
	for _, other := range []model.RequestConfig{
		{Type: RequestTypeLocalFile, Root: t.TempDir(), RedirectTrustedHosts: []string{"x"}},
		{Type: RequestTypeGRPC, RPC: "grpc.health.v1.Health/Check", RedirectTrustedHosts: []string{"x"}},
	} {
		if RequestTypes[other.Type] == nil {
			continue
		}
		c := model.Collector{Name: "web", Request: other}
		if err := ValidateRequest(&c); err == nil || err.Error() != fmt.Sprintf(`collector "web" sets request.redirect_trusted_hosts, which does not apply to request.type %q`, other.Type) {
			t.Errorf("%s: %v", other.Type, err)
		}
	}
}

// No probe parameter sets the list: redirect_trusted_hosts is not among the
// parameters of any request type, so a probe naming it is checked and read
// as though it had not, and changes nothing of the request. A static
// target's request block has no such key either.
func TestNoProbeParameterSetsTheTrustedHosts(t *testing.T) {
	for name, rt := range RequestTypes {
		if matchesOverride(rt.Overrides, "redirect_trusted_hosts") || matchesOverride(rt.TargetFields, "redirect_trusted_hosts") {
			t.Errorf("request type %s takes redirect_trusted_hosts from a probe or a static target", name)
		}
	}
	chain := newRedirectChain()
	server := chain.server(t)
	chain.redirect("/start", http.StatusFound, atHost(t, server.URL, "localhost")+"/end")
	c := trustCollector(t, func(c *model.Collector) { c.Request.FollowRedirects = false })
	values := url.Values{"follow_redirects": {"true"}, "redirect_trusted_hosts": {"localhost", "*"}}
	if err := CheckOverrideParams(c, values); err != nil {
		t.Fatal(err)
	}
	overrides, err := ParseRequestOverrides(values)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := FetchCollector(context.Background(), server.URL+"/start", c, overrides, forwardedTenant); err != nil {
		t.Fatal(err)
	}
	assertCarried(t, "a probe naming the list", chain.requests(), carriesEverything, carriesNeutral)

	// The collector's own list holds when a probe switches following on.
	listed := trustCollector(t, func(c *model.Collector) {
		c.Request.FollowRedirects = false
		c.Request.RedirectTrustedHosts = []string{"localhost"}
	})
	if _, err := FetchCollector(context.Background(), server.URL+"/start", listed, overrides, forwardedTenant); err != nil {
		t.Fatal(err)
	}
	assertCarried(t, "following switched on by the probe", chain.requests(), carriesEverything, carriesEverything)
}
