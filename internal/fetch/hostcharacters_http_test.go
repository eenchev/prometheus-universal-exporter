//go:build !select_request_types || request_type_http

package fetch

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// decodingProxy is a forward proxy that reads the host it is asked for the
// way some proxies do: percent-decoded once more than the client wrote it.
// It resolves the decoded name itself — routes stands for its DNS and its
// network — fetches the answer from there and relays it, and remembers every
// host it was asked for, decoded.
type decodingProxy struct {
	URL    string
	routes map[string]string
	mu     sync.Mutex
	asked  []string
}

func newDecodingProxy(t *testing.T, routes map[string]string) *decodingProxy {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	p := &decodingProxy{URL: "http://" + listener.Addr().String(), routes: routes}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go p.serve(conn)
		}
	}()
	return p
}

// serve answers one request, GET http://host/path, and closes.
func (p *decodingProxy) serve(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	reader := bufio.NewReader(conn)
	line, err := reader.ReadString('\n')
	if err != nil {
		return
	}
	for {
		header, err := reader.ReadString('\n')
		if err != nil || strings.TrimSpace(header) == "" {
			break
		}
	}
	fields := strings.Fields(line)
	if len(fields) != 3 || !strings.HasPrefix(fields[1], "http://") {
		_, _ = io.WriteString(conn, "HTTP/1.1 400 Bad Request\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
		return
	}
	authority, path, _ := strings.Cut(strings.TrimPrefix(fields[1], "http://"), "/")
	host, err := url.PathUnescape(authority)
	if err != nil {
		host = authority
	}
	if name, _, err := net.SplitHostPort(host); err == nil {
		host = name
	}
	host = strings.ToLower(host)
	p.mu.Lock()
	p.asked = append(p.asked, host)
	p.mu.Unlock()
	answer := func(status, body string) {
		_, _ = io.WriteString(conn, "HTTP/1.1 "+status+"\r\nContent-Length: "+strconv.Itoa(len(body))+"\r\nConnection: close\r\n\r\n"+body)
	}
	backend, known := p.routes[host]
	if !known {
		answer("502 Bad Gateway", "the proxy cannot resolve "+host)
		return
	}
	// The proxy's own request goes straight to where it resolved the name.
	client := &http.Client{Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Get("http://" + backend + "/" + path)
	if err != nil {
		answer("502 Bad Gateway", err.Error())
		return
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if location := resp.Header.Get("Location"); location != "" {
		_, _ = io.WriteString(conn, "HTTP/1.1 "+resp.Status+"\r\nLocation: "+location+"\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
		return
	}
	answer(resp.Status, string(body))
}

// hosts is every host the proxy was asked for since the last call.
func (p *decodingProxy) hosts() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := p.asked
	p.asked = nil
	return out
}

// behindProxy sends every http request of the test through proxy, and makes
// every name unknown to the exporter's own resolver, as on a host with no
// outside DNS: only an address written as one is known without the proxy.
func behindProxy(t *testing.T, proxy string) {
	t.Helper()
	t.Setenv("HTTP_PROXY", proxy)
	t.Setenv("http_proxy", proxy)
	for _, name := range []string{"HTTPS_PROXY", "https_proxy", "NO_PROXY", "no_proxy"} {
		t.Setenv(name, "")
	}
	// Transports read the environment when they are built.
	previous, restore := transports, resolveHost
	transports = newTransportCache()
	t.Cleanup(func() { transports, resolveHost = previous, restore })
	resolveHost = func(_ context.Context, host string) ([]netip.Addr, error) {
		if addr, err := netip.ParseAddr(strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")); err == nil {
			return []netip.Addr{addr}, nil
		}
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
}

// A '%' in a target's host is how a URL carries one: "intern%2561l.example"
// parses to the host "intern%61l.example". That was taken for a name like any
// other, matched no denied name and no address, could not be looked up, and
// was handed to the proxy, as any name the exporter cannot resolve is; a
// proxy that decodes the host once more then fetched internal.example, or
// the cloud metadata service, for whoever named the target. A host is now an
// address or a name of letters, digits, '.', '-' and '_', so every such
// spelling is refused before the proxy is asked anything, whatever the
// collector's lists are.
func TestAPercentInAHostNeverReachesTheProxy(t *testing.T) {
	secret := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("SECRET")) }))
	defer secret.Close()
	public := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("public")) }))
	defer public.Close()
	proxy := newDecodingProxy(t, map[string]string{
		"internal.example": strings.TrimPrefix(secret.URL, "http://"),
		"169.254.169.254":  strings.TrimPrefix(secret.URL, "http://"),
		"public.example":   strings.TrimPrefix(public.URL, "http://"),
	})
	behindProxy(t, proxy.URL)

	collectors := map[string]*model.Collector{
		"no lists": httpCollector(t, nil),
		"lists of names": httpCollector(t, func(c *model.Collector) {
			c.Request.AllowedTargets = []string{"*.example"}
			c.Request.DeniedTargets = []string{"internal.example"}
		}),
		"a denied name alone": httpCollector(t, func(c *model.Collector) { c.Request.DeniedTargets = []string{"internal.example", "*.internal"} }),
		"lists of networks": httpCollector(t, func(c *model.Collector) {
			c.Request.AllowedTargets = []string{"203.0.113.0/24"}
			c.Request.DeniedTargets = []string{"10.0.0.0/8"}
		}),
	}
	spellings := []string{
		// internal.example, one character written as an escape.
		"http://intern%2561l.example/latest",
		"http://%2569nternal.example/latest",
		"http://internal%252eexample/latest",
		"http://INTERNAL%252EEXAMPLE/latest",
		"http://internal.exampl%2565:80/latest",
		// 169.254.169.254, likewise.
		"http://%2531%2536%2539.254.169.254/latest",
		"http://169%252e254.169.254/latest",
		"http://169.254.169.25%2534/latest",
		"http://1%2536%2539.254.169.254:80/latest",
	}
	for name, c := range collectors {
		for _, target := range spellings {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			resp, err := FetchCollector(ctx, target, c, RequestOverrides{}, nil)
			cancel()
			if !errors.Is(err, ErrTargetRefused) || !strings.Contains(err.Error(), `it has "%", a character that no host name or address has`) {
				t.Errorf("%s: %s: resp=%+v err=%v, want it refused for the %% in its host", name, target, resp, err)
			}
			if asked := proxy.hosts(); len(asked) != 0 {
				t.Errorf("%s: %s: the proxy was asked for %q", name, target, asked)
			}
		}
	}
	// The plain spellings are refused as before, and a name nothing refuses
	// is still the proxy's to resolve and fetch.
	for _, target := range []string{"http://internal.example/latest", "http://169.254.169.254/latest"} {
		if _, err := FetchCollector(context.Background(), target, collectors["lists of names"], RequestOverrides{}, nil); !errors.Is(err, ErrTargetRefused) {
			t.Errorf("%s: err=%v, want it refused", target, err)
		}
	}
	if asked := proxy.hosts(); len(asked) != 0 {
		t.Errorf("the proxy was asked for %q", asked)
	}
	for _, name := range []string{"no lists", "lists of names", "a denied name alone"} {
		resp, err := FetchCollector(context.Background(), "http://public.example/metrics", collectors[name], RequestOverrides{}, nil)
		if err != nil || string(resp.Body) != "public" {
			t.Fatalf("%s: a name only the proxy resolves: resp=%+v err=%v", name, resp, err)
		}
		if asked := proxy.hosts(); len(asked) != 1 || asked[0] != "public.example" {
			t.Fatalf("%s: the proxy was asked for %q, want public.example once", name, asked)
		}
	}
}

// A redirect's host is held to the same rule as the first one: a target that
// answers with a Location whose host carries an escape — the cloud metadata
// address with a digit written as one — is not followed there, behind a
// proxy that would decode it.
func TestARedirectToAHostWithAPercentIsNotFollowed(t *testing.T) {
	secret := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("SECRET")) }))
	defer secret.Close()
	var redirect atomic.Pointer[string]
	redirecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", *redirect.Load())
		w.WriteHeader(http.StatusFound)
	}))
	defer redirecting.Close()
	proxy := newDecodingProxy(t, map[string]string{
		"169.254.169.254":  strings.TrimPrefix(secret.URL, "http://"),
		"internal.example": strings.TrimPrefix(secret.URL, "http://"),
		"public.example":   strings.TrimPrefix(redirecting.URL, "http://"),
	})
	behindProxy(t, proxy.URL)
	c := httpCollector(t, func(c *model.Collector) {
		c.Request.FollowRedirects = true
		c.Request.DeniedTargets = []string{"internal.example"}
	})
	for _, location := range []string{"http://%2531%2536%2539.254.169.254/latest", "http://intern%2561l.example/latest", "http://a,b.example/latest"} {
		redirect.Store(&location)
		resp, err := FetchCollector(context.Background(), "http://public.example/metrics", c, RequestOverrides{}, nil)
		if !errors.Is(err, ErrTargetRefused) || !strings.Contains(err.Error(), "a character that no host name or address has") {
			t.Errorf("redirect to %s: resp=%+v err=%v, want it refused for its host", location, resp, err)
		}
		if asked := proxy.hosts(); len(asked) != 1 || asked[0] != "public.example" {
			t.Errorf("redirect to %s: the proxy was asked for %q, want only the first host", location, asked)
		}
	}
}

// What a host may be written with is what a name can hold anywhere a name
// is resolved — letters, digits, '.', '-' and '_' — or an IP address.
// Punctuation that a URL's host may carry but no name has is refused, as a
// refusal (403) that names the character, with no lists set and before
// anything is dialed; before, such a host was dialed, or left to a proxy.
func TestAHostWithACharacterNoNameHasIsRefused(t *testing.T) {
	var mu sync.Mutex
	var dialed []string
	previous := transports
	transports = newTransportCache()
	t.Cleanup(func() { transports = previous })
	c := httpCollector(t, nil)
	tr, err := transports.get(TransportSettings{policy: policyOf(c)}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	tr.Proxy = nil
	tr.DialContext = policyDialer(func(_ context.Context, _, address string) (net.Conn, error) {
		mu.Lock()
		dialed = append(dialed, address)
		mu.Unlock()
		return nil, errors.New("the test dials nothing")
	})
	for _, host := range []string{
		"a%b", "169.254.169.254%00", "169.254.169.254%", "169.254.169.254%eth0", "internal.example%2e", "evil.test%2f.example.com",
		"a,b", "a;b", "a=b", "a*b", "a!b", "a'b", "a(b)", "a+b", "a$b", "a&b", "a~b", `a"b`, "a<b>", "a b", "a\tb", "a\x00b", "a\x7fb",
		"a:b", "fe80::1%", "::ffff:1.2.3", "a|b", "a^b", "a`b", "a{b}", `a\b`, "a@b", "a#b", "a/b", "a?b",
	} {
		if got, err := canonicalHost(host); !errors.Is(err, ErrTargetRefused) || !strings.Contains(err.Error(), "a character that no host name or address has") {
			t.Errorf("canonicalHost(%q) = %q, %v, want it refused for its character", host, got, err)
		}
	}
	// As a target: these are the ones a URL can be written with.
	for _, target := range []string{
		"http://169.254.169.254%2500/latest/", "http://169.254.169.254%25/x", "http://169.254.169.254%25eth0/x", "http://internal.example%252e/x",
		"http://evil.test%252f.example.com/", "http://a,b/", "http://a;b/", "http://a=b/", "http://a*b/", "http://a!b/", "http://a'b/",
		"http://a(b)/", "http://a+b/", "http://a$b/", "http://a&b/", "http://a~b/", `http://a"b/`, "http://a<b>/",
	} {
		_, err := FetchCollector(context.Background(), target, c, RequestOverrides{}, nil)
		if !errors.Is(err, ErrTargetRefused) || !strings.Contains(err.Error(), "a character that no host name or address has") {
			t.Errorf("%s: err=%v, want it refused for its host", target, err)
		}
	}
	mu.Lock()
	if len(dialed) != 0 {
		t.Errorf("%q were dialed", dialed)
	}
	mu.Unlock()
	// A name with characters outside ASCII is held to the rule in the
	// ASCII form it is converted to: a full-width '%' is a '%'.
	for _, host := range []string{"bücher％.example", "ｉｎｔｅｒｎ％６１ｌ.example", "bü,cher.example"} {
		if got, err := canonicalHost(host); !errors.Is(err, ErrTargetRefused) {
			t.Errorf("canonicalHost(%q) = %q, %v, want it refused", host, got, err)
		}
	}
}

// The rule takes nothing away from what a host could be before: a name with
// an underscore, with two hyphens or a hyphen first, with a final dot or in
// mixed case, a punycode name, an IPv4 address and an IPv6 address in
// brackets, with a zone or without, are requested, each checked under the
// name or address it is dialed by.
func TestEveryHostANameOrAddressCanBeIsStillRequested(t *testing.T) {
	target := newRequestRecorder(t)
	backend := strings.TrimPrefix(target.URL, "http://")
	previous := transports
	transports = newTransportCache()
	t.Cleanup(func() { transports = previous })
	c := httpCollector(t, nil)
	everyNameReaches(t, c, backend)
	for host, canonical := range map[string]string{
		"my_service":            "my_service",
		"_dmarc.example.com":    "_dmarc.example.com",
		"db--primary.internal":  "db--primary.internal",
		"-edge.internal":        "-edge.internal",
		"edge-.internal":        "edge-.internal",
		"metrics.example.com.":  "metrics.example.com",
		"Metrics.Example.COM":   "metrics.example.com",
		"xn--bcher-kva.example": "xn--bcher-kva.example",
		"bücher.example":        "xn--bcher-kva.example",
		"192.0.2.7":             "192.0.2.7",
		"0x7f.0.0.1":            "0x7f.0.0.1",
		"2130706433":            "2130706433",
		"[2001:db8::7]":         "2001:db8::7",
		"[2001:DB8::7]":         "2001:db8::7",
		"[fe80::1%25eth0]":      "fe80::1%eth0",
		"[fe80::1%25En0]":       "fe80::1%en0",
	} {
		u, err := url.Parse("http://" + host + ":8080/metrics")
		if err != nil {
			t.Fatalf("%s: %v", host, err)
		}
		if got, err := canonicalHost(u.Hostname()); err != nil || got != canonical {
			t.Errorf("canonicalHost(%q) = %q, %v, want %q", u.Hostname(), got, err, canonical)
		}
		resp, err := FetchCollector(context.Background(), u.String(), c, RequestOverrides{}, nil)
		if err != nil || string(resp.Body) != "ok" {
			t.Errorf("%s: resp=%+v err=%v, want it requested", host, resp, err)
		}
		if got := target.requested(); len(got) != 1 || got[0] != "/metrics" {
			t.Errorf("%s: the target was asked for %q", host, got)
		}
	}
}

// An entry of allowed_targets or denied_targets is a name, a glob of one, an
// address or a network: an entry with a character no host name has, a '%'
// included, matches nothing a request could name, and stops the load. A
// glob's own '*' and '?' are the two characters an entry has beyond a
// host's.
func TestAListEntryWithACharacterNoNameHasIsRefusedAtLoad(t *testing.T) {
	for _, entry := range []string{"intern%61l.example", "local%68ost", "169.254.169.254%00", "a,b", "a;b", "a=b", "a b.example", "a+b", "a~b", "a!b", "a$b", "a&b", "a'b", "a(b)", `a"b`, "a<b>", "fe80::1%"} {
		for _, key := range []string{"allowed_targets", "denied_targets"} {
			c := model.Collector{Name: "web", Request: model.RequestConfig{Type: RequestTypeHTTP}}
			if key == "allowed_targets" {
				c.Request.AllowedTargets = []string{entry}
			} else {
				c.Request.DeniedTargets = []string{entry}
			}
			err := ValidateRequest(&c)
			if err == nil || !strings.Contains(err.Error(), `collector "web" request.`+key+" entry") || !strings.Contains(err.Error(), "is not a host name") {
				t.Errorf("%s: [%q]: err=%v, want the load refused naming the entry", key, entry, err)
			}
		}
	}
	for _, entry := range []string{"*.example.com", "db-?.internal", "my_service", "-edge.*", "xn--bcher-kva.example", "Metrics.Example.COM.", "192.0.2.7", "[2001:db8::7]", "fe80::/10", "10.0.0.0/8"} {
		c := model.Collector{Name: "web", Request: model.RequestConfig{Type: RequestTypeHTTP, AllowedTargets: []string{entry}, DeniedTargets: []string{entry}}}
		if err := ValidateRequest(&c); err != nil {
			t.Errorf("[%q]: %v", entry, err)
		}
	}
}
