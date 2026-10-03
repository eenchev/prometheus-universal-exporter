//go:build !select_request_types || request_type_http

package fetch

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"golang.org/x/net/idna"
)

// The name the target policy judges is the name a request is made to
// (canonicalHost). These tests look at every place a request names its host
// in: the address dialed, the name the TLS handshake asks for, the Host
// header or the :authority of HTTP/2, and the request line and the CONNECT a
// proxy reads.

// namedServer is an HTTPS server, speaking HTTP/2 too, that keeps the name
// each TLS handshake asked for, and the protocol and the Host of each
// request.
type namedServer struct {
	*httptest.Server
	mu           sync.Mutex
	names, hosts []string
}

func newNamedServer(t *testing.T) *namedServer {
	t.Helper()
	s := &namedServer{}
	s.Server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.hosts = append(s.hosts, r.Proto+" "+r.Host)
		s.mu.Unlock()
		_, _ = io.WriteString(w, "value 1\n")
	}))
	s.EnableHTTP2 = true
	s.TLS = &tls.Config{MinVersion: tls.VersionTLS12, GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
		s.mu.Lock()
		s.names = append(s.names, hello.ServerName)
		s.mu.Unlock()
		return nil, nil
	}}
	s.StartTLS()
	t.Cleanup(s.Close)
	return s
}

// seen is the names asked for and the requests received since the last call.
func (s *namedServer) seen() (names, hosts []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	names, hosts = s.names, s.hosts
	s.names, s.hosts = nil, nil
	return names, hosts
}

// dialRecord keeps the addresses a transport asked to be connected to.
type dialRecord struct {
	mu        sync.Mutex
	addresses []string
	// limit, when set, fails every dial after that many.
	limit int
}

// to is a dialer that connects every address to backend, as a DNS that
// knows every name would, or to the address itself when backend is "".
func (d *dialRecord) to(backend string) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		d.mu.Lock()
		d.addresses = append(d.addresses, address)
		over := d.limit > 0 && len(d.addresses) > d.limit
		d.mu.Unlock()
		if over {
			return nil, errors.New("the test dials no more")
		}
		if backend != "" {
			address = backend
		}
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}
}

// dialed is the addresses asked for since the last call.
func (d *dialRecord) dialed() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := d.addresses
	d.addresses = nil
	return out
}

// goGet sends one GET on a transport of Go's own, with nothing of the
// exporter around it.
func goGet(tr *http.Transport, rawURL string) error {
	defer tr.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.Body.Close()
}

// outsideASCIIHosts are hosts written with a character outside ASCII, of
// every kind there is: letters and dots in other widths, an
// internationalised name in lower case, in capitals and with a combining
// mark, the letters IDNA once mapped away (ß, ς), right-to-left letters,
// characters that are dropped or forbidden in a name, digits that write an
// address, letters whose lower case is ASCII, a name with an ASCII form too
// long for a label, with an empty label, with a final dot, with a label that
// only looks like punycode, an address with such a zone, and a byte that is
// no UTF-8.
var outsideASCIIHosts = []string{
	"ｏrigin.test", "origin。test", "origin．test", "origin｡test", "ｌｏｃａｌｈｏｓｔ",
	"bücher.example", "BÜCHER.example", "bu\u0308cher.example", "faß.example", "ς.example",
	"\u05d0\u05d1.example", "a\u200db.example", "a\u00adb.example", "bü_cher.example", "bücher％.example",
	"１２７.０.０.１", "１６９.２５４.１６９.２５４", "①.example", "\u212a.example", "\u0130.example",
	strings.Repeat("ü", 60) + ".example", "ü..example", "ü.example.", "ü.example。", "xn--ü.example",
	"[fe80::1%25ｅth0]", "\xff.example",
}

// Go's HTTP client does not send a host written outside ASCII under one
// name, which is why the exporter refuses such a host instead of converting
// it. It converts the host in two ways. The address it dials, the name its
// TLS handshake asks for and the target of the CONNECT it sends a proxy are
// the mapped form (idna.Lookup), which folds case and width, composes
// marks and drops what a name ignores, and, when a host has no such form,
// the host as it is written. The Host header and the request line a proxy
// reads are the unmapped form (idna.ToASCII), the characters as they stand
// in punycode. The two agree only for a name already in the form IDNA maps
// to, as bücher.example is; for every other the request is made to one name
// and asks for another. Over HTTP/2 such a request is not sent at all: the
// connection is kept under the unmapped name and looked for under the
// mapped one, so the client dials again and again, here until the test stops
// it.
//
// This is Go's behaviour and not the exporter's: should a later Go send one
// name everywhere, this test fails and canonicalHost may be thought over.
func TestGoSendsAHostOutsideASCIIUnderTwoNames(t *testing.T) {
	chain := newRedirectChain()
	plain := strings.TrimPrefix(chain.server(t).URL, "http://")
	secure := newNamedServer(t)
	secureAddress := strings.TrimPrefix(secure.URL, "https://")
	proxy, err := url.Parse(chain.wireServer(t))
	if err != nil {
		t.Fatal(err)
	}
	// A configuration for each transport: one that speaks HTTP/2 writes
	// that into its own.
	anyCertificate := func() *tls.Config { return &tls.Config{InsecureSkipVerify: true} }
	for _, tc := range []struct {
		host string
		// dialed is the name connected to, and sent the one asked for.
		dialed, sent string
	}{
		{"ｏrigin.test", "origin.test", "xn--rigin-qr33a.test"},
		{"origin。test", "origin.test", "xn--origintest-sh3i"},
		{"origin．test", "origin.test", "xn--origintest-6f99c"},
		{"origin｡test", "origin.test", "xn--origintest-9599c"},
		{"BÜCHER.example", "xn--bcher-kva.example", "xn--BCHER-2pa.example"},
		{"bu\u0308cher.example", "xn--bcher-kva.example", "xn--bucher-xyd.example"},
		{"a\u00adb.example", "ab.example", "xn--ab-5da.example"},
		{"１２７.０.０.１", "127.0.0.1", "xn--8g7ccp.xn--7g7c.xn--7g7c.xn--8g7c"},
		{"a\u200db.example", "a\u200db.example", "xn--ab-m1t.example"},
		{"bücher.example", "xn--bcher-kva.example", "xn--bcher-kva.example"},
	} {
		chain.requests()
		dials := &dialRecord{}
		if err := goGet(&http.Transport{DialContext: dials.to(plain)}, "http://"+tc.host+":8080/x"); err != nil {
			t.Fatalf("%q over http: %v", tc.host, err)
		}
		if dialed, got := dials.dialed(), chain.requests(); len(dialed) != 1 || dialed[0] != tc.dialed+":8080" || len(got) != 1 || got[0].host != tc.sent+":8080" {
			t.Errorf("%q over http: Go dialed %q and sent %+v, want %s dialed and a Host of %s", tc.host, dialed, got, tc.dialed, tc.sent)
		}

		if err := goGet(&http.Transport{Proxy: http.ProxyURL(proxy)}, "http://"+tc.host+":8080/x"); err != nil {
			t.Fatalf("%q through a proxy: %v", tc.host, err)
		}
		want := "GET http://" + tc.sent + ":8080/x HTTP/1.1\r\nHost: " + tc.sent + ":8080\r\n"
		if got := chain.requests(); len(got) != 1 || !strings.HasPrefix(got[0].raw, want) {
			t.Errorf("%q through a proxy: Go sent %+v, want %q", tc.host, got, want)
		}

		// A name is asked for in the handshake; an address is not.
		asked := tc.dialed
		if _, err := netip.ParseAddr(asked); err == nil {
			asked = ""
		}
		secure.seen()
		if err := goGet(&http.Transport{DialContext: dials.to(secureAddress), TLSClientConfig: anyCertificate()}, "https://"+tc.host+":8080/x"); err != nil {
			t.Fatalf("%q over https: %v", tc.host, err)
		}
		names, hosts := secure.seen()
		if dialed := dials.dialed(); len(dialed) != 1 || dialed[0] != tc.dialed+":8080" || len(names) != 1 || names[0] != asked || len(hosts) != 1 || hosts[0] != "HTTP/1.1 "+tc.sent+":8080" {
			t.Errorf("%q over https: Go dialed %q, asked the handshake for %q and sent %q, want %s dialed and asked for, and a Host of %s", tc.host, dialed, names, hosts, tc.dialed, tc.sent)
		}

		// The proxy here answers a CONNECT with no tunnel, so the request
		// fails once the CONNECT is read.
		_ = goGet(&http.Transport{Proxy: http.ProxyURL(proxy), TLSClientConfig: anyCertificate()}, "https://"+tc.host+":8080/x")
		want = "CONNECT " + tc.dialed + ":8080 HTTP/1.1\r\n"
		if got := chain.requests(); len(got) != 1 || !strings.HasPrefix(got[0].raw, want) {
			t.Errorf("%q over https through a proxy: Go sent %+v, want %q", tc.host, got, want)
		}

		dials.limit = 3
		err := goGet(&http.Transport{DialContext: dials.to(secureAddress), TLSClientConfig: anyCertificate(), ForceAttemptHTTP2: true}, "https://"+tc.host+":8080/x")
		dialed := dials.dialed()
		names, hosts = secure.seen()
		if tc.dialed == tc.sent || tc.dialed == tc.host {
			// One name, or a host with no mapped form, which HTTP/2 keeps
			// and looks for as it is written.
			if err != nil || len(dialed) != 1 || len(hosts) != 1 || hosts[0] != "HTTP/2.0 "+tc.sent+":8080" {
				t.Errorf("%q over HTTP/2: err=%v, Go dialed %q and sent %q, want one request with the authority %s", tc.host, err, dialed, hosts, tc.sent)
			}
			continue
		}
		// The server may not have got to the last handshakes when the
		// client gives up, so their number is at most that of the dials
		// let through; each is for the mapped name.
		handshakes := len(names) >= 1 && len(names) <= 3
		for _, name := range names {
			handshakes = handshakes && name == asked
		}
		if err == nil || len(dialed) != 4 || len(hosts) != 0 || !handshakes {
			t.Errorf("%q over HTTP/2: err=%v, Go dialed %q, asked the handshakes for %q and sent %q, want it to dial %s until stopped and send nothing", tc.host, err, dialed, names, hosts, tc.dialed)
		}
	}
}

// collectorPool gives the pool c's requests are sent on a dialer that
// connects every name to backend and keeps the addresses asked for, and no
// proxy.
func collectorPool(t *testing.T, c *model.Collector, backend string) *dialRecord {
	t.Helper()
	tr, err := transports.get(TransportSettings{TLS: c.Request.TLS, EnableHTTP2: c.Request.EnableHTTP2, policy: policyOf(c)}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	dials := &dialRecord{}
	tr.DialContext = policyDialer(dials.to(backend))
	tr.Proxy = nil
	return dials
}

// freshPools gives the test connection pools of its own.
func freshPools(t *testing.T) {
	t.Helper()
	previous := transports
	transports = newTransportCache()
	t.Cleanup(func() { transports = previous })
}

// outsideASCIIRefusal fails unless err is the refusal of a host for a
// character outside ASCII.
func outsideASCIIRefusal(t *testing.T, name string, err error) {
	t.Helper()
	if !errors.Is(err, ErrTargetRefused) || !strings.Contains(err.Error(), ", a character outside ASCII: write an internationalised name in its ASCII form, as xn--bcher-kva.example for bücher.example") {
		t.Errorf("%s: err=%v, want the host refused for a character outside ASCII", name, err)
	}
}

// A target whose host is written with a character outside ASCII is refused
// before anything is dialed, over http and https, with HTTP/2 and without,
// whatever the collector's lists are: none, a list that allows every name,
// and the two lists the converted forms would have got past. With
// allowed_targets naming origin.test, "ｏrigin.test" was judged as
// origin.test and allowed, and sent to that host's address with a Host of
// xn--rigin-qr33a.test, which a server of many sites answers from another
// one; with denied_targets naming xn--rigin-qr33a.test it was judged as
// origin.test, not denied, and asked the denied name of the server. The
// refusal says which character, and how the name is written instead.
func TestAHostOutsideASCIIIsRefusedBeforeAnythingIsDialed(t *testing.T) {
	chain := newRedirectChain()
	plain := strings.TrimPrefix(chain.server(t).URL, "http://")
	secure := newNamedServer(t)
	freshPools(t)
	for name, edit := range map[string]func(*model.Collector){
		"no lists":           func(*model.Collector) {},
		"every name allowed": func(c *model.Collector) { c.Request.AllowedTargets = []string{"*"} },
		"the mapped names allowed": func(c *model.Collector) {
			c.Request.AllowedTargets = []string{"origin.test", "localhost", "*.example", "127.0.0.1", "169.254.169.254", "fe80::/10"}
		},
		"the unmapped names denied": func(c *model.Collector) {
			c.Request.DeniedTargets = []string{"xn--rigin-qr33a.test", "xn--origintest-*", "xn--BCHER-2pa.example"}
		},
	} {
		for _, http2 := range []bool{false, true} {
			c := httpCollector(t, func(c *model.Collector) {
				c.Request.TLS.InsecureSkipVerify, c.Request.EnableHTTP2 = true, http2
				edit(c)
			})
			for _, scheme := range []string{"http", "https"} {
				backend := plain
				if scheme == "https" {
					backend = strings.TrimPrefix(secure.URL, "https://")
				}
				dials := collectorPool(t, c, backend)
				for _, host := range outsideASCIIHosts {
					target := scheme + "://" + host + ":8080/metrics"
					// Over HTTP/2 a host that was not refused was dialed
					// until the request's time was up.
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					_, err := FetchCollector(ctx, target, c, RequestOverrides{}, nil)
					cancel()
					outsideASCIIRefusal(t, fmt.Sprintf("%s, HTTP/2 %v: %q", name, http2, target), err)
					names, hosts := secure.seen()
					if dialed, got := dials.dialed(), chain.requests(); len(dialed)+len(got)+len(names)+len(hosts) != 0 {
						t.Fatalf("%s, HTTP/2 %v: %q: %q were dialed, the handshakes asked for %q and the servers were sent %+v %q", name, http2, target, dialed, names, got, hosts)
					}
				}
			}
		}
	}
	c := httpCollector(t, func(c *model.Collector) { c.Request.AllowedTargets = []string{"origin.test"} })
	for target, want := range map[string]string{
		"http://ｏrigin.test/metrics":         "target ｏrigin.test refused: it has U+FF4F 'ｏ', a character outside ASCII: write an internationalised name in its ASCII form, as xn--bcher-kva.example for bücher.example",
		"https://origin。test:8443/":          "target origin。test refused: it has U+3002 '。', a character outside ASCII: write an internationalised name in its ASCII form, as xn--bcher-kva.example for bücher.example",
		"bücher.example:9100":                "target bücher.example refused: it has U+00FC 'ü', a character outside ASCII: write an internationalised name in its ASCII form, as xn--bcher-kva.example for bücher.example",
		"http://a\u200db.example/":           "target a\u200db.example refused: it has U+200D, a character outside ASCII: write an internationalised name in its ASCII form, as xn--bcher-kva.example for bücher.example",
		"http://\xff.example/":               "target \xff.example refused: it has U+FFFD '\ufffd', a character outside ASCII: write an internationalised name in its ASCII form, as xn--bcher-kva.example for bücher.example",
		"http://[fe80::1%25ｅth0]/":           "target fe80::1%ｅth0 refused: it has U+FF45 'ｅ', a character outside ASCII: write an internationalised name in its ASCII form, as xn--bcher-kva.example for bücher.example",
		"http://intern%2561l｡example/latest": "target intern%61l｡example refused: it has U+FF61 '｡', a character outside ASCII: write an internationalised name in its ASCII form, as xn--bcher-kva.example for bücher.example",
	} {
		if _, err := FetchCollector(context.Background(), target, c, RequestOverrides{}, nil); err == nil || err.Error() != want {
			t.Errorf("%q: err=%v, want %q", target, err, want)
		}
	}
}

// Behind a proxy it is the proxy that is asked for the host, in the request
// line of an http request and the CONNECT of an https one. A target written
// outside ASCII is refused before the proxy is asked anything, and so is a
// redirect to one, the proxy having been asked for the first URL alone:
// with allowed_targets naming origin.test, the proxy was asked for
// xn--rigin-qr33a.test and xn--origintest-sh3i, names the list never
// allowed, and with those two denied it was asked for them all the same.
func TestAHostOutsideASCIINeverReachesTheProxy(t *testing.T) {
	chain := newRedirectChain()
	proxy := chain.wireServer(t)
	behindProxy(t, proxy)
	t.Setenv("HTTPS_PROXY", proxy)
	for name, edit := range map[string]func(*model.Collector){
		"no lists":                 func(*model.Collector) {},
		"the mapped names allowed": func(c *model.Collector) { c.Request.AllowedTargets = []string{"origin.test", "localhost", "*.example"} },
		"the unmapped names denied": func(c *model.Collector) {
			c.Request.DeniedTargets = []string{"xn--rigin-qr33a.test", "xn--origintest-*"}
		},
	} {
		c := httpCollector(t, func(c *model.Collector) {
			c.Request.FollowRedirects = true
			edit(c)
		})
		for _, host := range outsideASCIIHosts {
			for _, scheme := range []string{"http", "https"} {
				target := scheme + "://" + host + "/metrics"
				_, err := FetchCollector(context.Background(), target, c, RequestOverrides{}, nil)
				outsideASCIIRefusal(t, name+": "+target, err)
				if got := chain.requests(); len(got) != 0 {
					t.Errorf("%s: %q: the proxy was sent %+v", name, target, got)
				}
				// A Location cannot hold a byte that is no UTF-8 for Go
				// to follow.
				if host == "\xff.example" {
					continue
				}
				chain.redirect("/start", http.StatusFound, target)
				_, err = FetchCollector(context.Background(), "http://origin.test/start", c, RequestOverrides{}, nil)
				outsideASCIIRefusal(t, name+": a redirect to "+target, err)
				if got := chain.requests(); len(got) != 1 || !strings.HasPrefix(got[0].raw, "GET http://origin.test/start HTTP/1.1\r\nHost: origin.test\r\n") {
					t.Errorf("%s: a redirect to %q: the proxy was sent %+v, want the first request alone", name, target, got)
				}
			}
		}
	}
}

// A redirect to a host written outside ASCII is not followed on a direct
// connection either: the first host is dialed and nothing else is, where
// the redirect was followed to the address the mapped name has, with a Host
// of the unmapped one.
func TestARedirectToAHostOutsideASCIIDialsNothing(t *testing.T) {
	chain := newRedirectChain()
	backend := strings.TrimPrefix(chain.server(t).URL, "http://")
	freshPools(t)
	c := httpCollector(t, func(c *model.Collector) {
		c.Request.FollowRedirects = true
		c.Request.AllowedTargets = []string{"origin.test"}
	})
	dials := collectorPool(t, c, backend)
	for _, host := range outsideASCIIHosts {
		if host == "\xff.example" {
			continue
		}
		chain.redirect("/start", http.StatusFound, "http://"+host+":8080/end")
		_, err := FetchCollector(context.Background(), "http://origin.test:8080/start", c, RequestOverrides{}, nil)
		outsideASCIIRefusal(t, "a redirect to "+host, err)
		got := chain.requests()
		// The first host's connection may be one kept from the redirect
		// before.
		if dialed := dials.dialed(); len(dialed) > 1 || len(dialed) == 1 && dialed[0] != "origin.test:8080" || len(got) != 1 || got[0].host != "origin.test:8080" {
			t.Errorf("a redirect to %q: %q were dialed and the server was sent %+v, want the first request alone", host, dialed, got)
		}
	}
}

// asciiHosts are hosts written in ASCII, as a URL writes them: names in
// either case, with a final dot, with an underscore and with hyphens a
// registered domain may not have, punycode labels in either case, a label
// that only looks like punycode, one longer than a label may be, an empty
// one, and addresses in their forms.
var asciiHosts = []string{
	"api.example", "API.Example", "api.example.", "Api.EXAMPLE.", "localhost", "my_service", "-edge.internal", "db--primary.internal",
	"xn--bcher-kva.example", "XN--BCHER-KVA.Example", "xn--Bcher-Kva.example.", "xn--rigin-qr33a.test", "xn--origintest-sh3i", "xn--zz.example", "xn--.example",
	strings.Repeat("a", 64) + ".example", "a..example", "1.example", "0x7f.0.0.1", "2130706433", "127.1",
	"127.0.0.1", "192.0.2.7", "[::1]", "[2001:DB8::7]", "[::ffff:192.0.2.7]", "[fe80::1%25eth0]", "[fe80::1%25En0]",
}

// A host written in ASCII is sent under the one name it is written as, the
// name the policy judged: Go converts nothing in it. It is the address
// dialed, the Host of the request, the name the TLS handshake asks for,
// which leaves a final dot out as TLS has it and is none for an address,
// the :authority of HTTP/2, and what a proxy reads in the request line and
// in a CONNECT. The policy judges that name in lower case and without its
// final dot (canonicalHost), so an entry naming it allows it and denies it.
// That holds for a punycode name in either case, for a label that only
// looks like punycode and for one a registered domain could not have.
func TestAnASCIIHostIsSentUnderTheNameThePolicyJudged(t *testing.T) {
	chain := newRedirectChain()
	plain := strings.TrimPrefix(chain.server(t).URL, "http://")
	secure := newNamedServer(t)
	freshPools(t)
	for _, written := range asciiHosts {
		bare := strings.TrimSuffix(strings.TrimPrefix(strings.ReplaceAll(written, "%25", "%"), "["), "]")
		judged, err := canonicalHost(bare)
		if err != nil || !strings.EqualFold(strings.TrimSuffix(bare, "."), judged) {
			t.Fatalf("canonicalHost(%q) = %q, %v, want the host in lower case without its final dot", bare, judged, err)
		}
		// The zone of an address is no part of the address a list names.
		// A host written as an address is allowed by the address its
		// connection is made to, which is this test's server for them all.
		entry, _, _ := strings.Cut(judged, "%")
		allowed := httpCollector(t, func(c *model.Collector) {
			c.Request.TLS.InsecureSkipVerify, c.Request.AllowedTargets = true, []string{entry, "127.0.0.1"}
		})
		denied := httpCollector(t, func(c *model.Collector) { c.Request.DeniedTargets = []string{entry} })
		if _, err := FetchCollector(context.Background(), "http://"+written+":8080/metrics", denied, RequestOverrides{}, nil); !errors.Is(err, ErrTargetRefused) {
			t.Errorf("%s with %s denied: err=%v, want it refused", written, entry, err)
		}
		// A zone is for the dialer alone: Go leaves it out of the Host.
		host, _, zoned := strings.Cut(written, "%25")
		if zoned {
			host += "]"
		}

		dials := collectorPool(t, allowed, plain)
		if _, err := FetchCollector(context.Background(), "http://"+written+":8080/metrics", allowed, RequestOverrides{}, nil); err != nil {
			t.Fatalf("%s over http: %v", written, err)
		}
		if dialed, got := dials.dialed(), chain.requests(); len(dialed) != 1 || dialed[0] != net.JoinHostPort(bare, "8080") || len(got) != 1 || got[0].host != host+":8080" {
			t.Errorf("%s over http: %q was dialed and the server sent %+v, want the host as it is written", written, dialed, got)
		}

		asked := strings.TrimSuffix(bare, ".")
		if net.ParseIP(asked) != nil || strings.Contains(asked, "%") {
			asked = ""
		}
		for protocol, http2 := range map[string]bool{"HTTP/1.1": false, "HTTP/2.0": true} {
			allowed.Request.EnableHTTP2 = http2
			dials = collectorPool(t, allowed, strings.TrimPrefix(secure.URL, "https://"))
			secure.seen()
			if _, err := FetchCollector(context.Background(), "https://"+written+":8080/metrics", allowed, RequestOverrides{}, nil); err != nil {
				t.Fatalf("%s over https, %s: %v", written, protocol, err)
			}
			// HTTP/2 leaves the zone of an address in the :authority.
			authority := host
			if http2 {
				authority = strings.ReplaceAll(written, "%25", "%")
			}
			names, hosts := secure.seen()
			if dialed := dials.dialed(); len(dialed) != 1 || dialed[0] != net.JoinHostPort(bare, "8080") || len(names) != 1 || names[0] != asked || len(hosts) != 1 || hosts[0] != protocol+" "+authority+":8080" {
				t.Errorf("%s over https: %q was dialed, the handshake asked for %q and the server was sent %q, want the host as it is written, over %s", written, dialed, names, hosts, protocol)
			}
		}
	}

	// Through a proxy, which is asked for the host as it is written too,
	// but for the zone of an address, which Go keeps to itself in a request
	// line and a Host.
	proxy := chain.wireServer(t)
	behindProxy(t, proxy)
	t.Setenv("HTTPS_PROXY", proxy)
	c := httpCollector(t, func(c *model.Collector) { c.Request.AllowedTargets = []string{"*"} })
	proxied := 0
	for _, written := range asciiHosts {
		host, _, zoned := strings.Cut(written, "%25")
		if zoned {
			host += "]"
		}
		// Go sends nothing for this machine itself through a proxy.
		if u, err := url.Parse("http://" + written + ":8080/metrics"); err != nil || !viaProxy(environmentProxy(), u) {
			continue
		}
		proxied++
		chain.requests()
		if _, err := FetchCollector(context.Background(), "http://"+written+":8080/metrics", c, RequestOverrides{}, nil); err != nil {
			t.Fatalf("%s through a proxy: %v", written, err)
		}
		want := "GET http://" + host + ":8080/metrics HTTP/1.1\r\nHost: " + host + ":8080\r\n"
		if got := chain.requests(); len(got) != 1 || !strings.HasPrefix(got[0].raw, want) {
			t.Errorf("%s through a proxy: the proxy was sent %+v, want %q", written, got, want)
		}
		// The proxy here answers a CONNECT with no tunnel, and does not
		// read one for an address with a zone.
		if zoned {
			continue
		}
		_, _ = FetchCollector(context.Background(), "https://"+written+":8080/metrics", c, RequestOverrides{}, nil)
		want = "CONNECT " + written + ":8080 HTTP/1.1\r\nHost: " + written + ":8080\r\n"
		if got := chain.requests(); len(got) != 1 || !strings.HasPrefix(got[0].raw, want) {
			t.Errorf("%s over https through a proxy: the proxy was sent %+v, want %q", written, got, want)
		}
	}
	if proxied < 20 {
		t.Errorf("%d of the %d hosts went through the proxy, want all but this machine's own", proxied, len(asciiHosts))
	}
}

// canonicalHostBefore is canonicalHost as it was when a host outside ASCII
// was converted, copied here.
func canonicalHostBefore(host string) (string, error) {
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	if _, err := netip.ParseAddr(host); err == nil {
		return strings.ToLower(host), nil
	}
	name := host
	if strings.IndexFunc(host, func(r rune) bool { return r > unicode.MaxASCII }) >= 0 {
		ascii, err := idna.Lookup.ToASCII(host)
		if err != nil {
			return "", &TargetRefusedError{Host: host, Reason: "it is not a host name or address the policy can check: " + err.Error()}
		}
		name = ascii
	}
	if i := strings.IndexFunc(name, notHostCharacter); i >= 0 {
		return "", &TargetRefusedError{Host: host, Reason: fmt.Sprintf("it has %q, a character that no host name or address has: a host is an IP address, or a name of letters, digits, '.', '-' and '_'", name[i:i+1])}
	}
	return strings.ToLower(strings.TrimSuffix(name, ".")), nil
}

// compileTargetPolicyBefore is compileTargetPolicy as it was when an entry
// outside ASCII was read like any other, copied here.
func compileTargetPolicyBefore(allowed, denied []string) (*targetPolicy, error) {
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

// firstOutsideASCII is the first character of s that is not ASCII, if it has
// one.
func firstOutsideASCII(s string) (rune, bool) {
	for _, r := range s {
		if r > unicode.MaxASCII {
			return r, true
		}
	}
	return 0, false
}

// sameError says whether two errors are both nil or say the same.
func sameError(a, b error) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Error() == b.Error()
}

// The refusal of what is outside ASCII changes nothing else. A host written
// in ASCII, whatever it is — a name, an address in any form, a host with a
// character no name has, every short string of the characters hosts are
// made of — reads as it did, to the same name or the same refusal, and so
// the lists judge it as they did; a host with a character outside ASCII is
// refused, where it was converted, or refused in other words. The same
// goes for the entries of the lists: one in ASCII compiles to what it did
// or fails with the error it did, and one outside ASCII stops the load,
// where it matched nothing, was refused in other words or, when lower case
// or trimming made ASCII of it, was read as another entry than the one
// written.
func TestOnlyWhatIsOutsideASCIIIsJudgedOtherwise(t *testing.T) {
	hosts := append([]string{}, outsideASCIIHosts...)
	for _, host := range asciiHosts {
		hosts = append(hosts, strings.ReplaceAll(host, "%25", "%"))
	}
	hosts = append(hosts, "", ".", ".example", "a,b", "a b", "intern%61l.example", "169.254.169.254%00", "fe80::1%", "::ffff:1.2.3", "a:b", "[::1", "::1]", "169.254.169.254", "169.254.169.254.", "FD00:EC2::254", "0177.0.0.1", "LOCALHOST.")
	// Every string of up to three of the characters a host is written
	// with, and of some it is not.
	alphabet := []string{"a", "Z", "1", ".", "-", "_", "%", ":", "[", "]", "*", "x", "n", "ü", "ｏ", "。", "\u212a"}
	short := []string{""}
	for length := 0; length < 3; length++ {
		var longer []string
		for _, prefix := range short {
			for _, next := range alphabet {
				longer = append(longer, prefix+next)
			}
		}
		hosts = append(hosts, longer...)
		short = longer
	}
	changed := 0
	for _, host := range hosts {
		got, err := canonicalHost(host)
		before, errBefore := canonicalHostBefore(host)
		if _, outside := firstOutsideASCII(host); !outside {
			if got != before || !sameError(err, errBefore) {
				t.Errorf("canonicalHost(%q) = %q, %v, and was %q, %v", host, got, err, before, errBefore)
			}
			continue
		}
		changed++
		if got != "" || !errors.Is(err, ErrTargetRefused) || !strings.Contains(err.Error(), "a character outside ASCII") {
			t.Errorf("canonicalHost(%q) = %q, %v, want it refused for a character outside ASCII; it was %q, %v", host, got, err, before, errBefore)
		}
	}
	if changed < 2000 || len(hosts)-changed < 2000 {
		t.Fatalf("%d hosts of %d are outside ASCII, want thousands of either kind", changed, len(hosts))
	}

	entries := []string{
		"api.example", "API.Example.", " api.example ", "\tapi.example\n", "*.example.com", "db-?.internal", "my_service", "-edge.*", "xn--bcher-kva.example", "XN--*",
		"192.0.2.7", "[2001:db8::7]", "::ffff:192.0.2.7", "fe80::1%eth0", "10.0.0.0/8", "10.1.2.3/8", "fe80::/10", "169.254.169.254", "fd00:ec2::/32", "2130706433", "127.1",
		"", " ", ".", "a,b", "a b.example", "intern%61l.example", "https://api.example", "api.example:80", "api.example/v1", "10.0.0.0/33", "fe80::1%", "[", "*",
	}
	outside := []string{
		"bücher.example", "BÜCHER.example", "ｏrigin.test", "origin。test", "*.bücher.example", "１０.０.０.０/８", "１２７.０.０.１", "\u212a.example", "\u0130nternal.example",
		"api.example\u00a0", "\u3000api.example", "api.example\u0085", "api.example\u2028", "\ufeffapi.example", "a\u200db.example", "\xff.example", "fe80::1%ｅth0",
	}
	for _, key := range []string{"allowed_targets", "denied_targets"} {
		lists := func(entry string) (allowed, denied []string) {
			if key == "allowed_targets" {
				return []string{"first.example", entry}, []string{"192.0.2.0/24"}
			}
			return []string{"169.254.169.254"}, []string{"first.example", entry}
		}
		for _, entry := range entries {
			allowed, denied := lists(entry)
			got, err := compileTargetPolicy(allowed, denied)
			before, errBefore := compileTargetPolicyBefore(allowed, denied)
			if !sameError(err, errBefore) || !reflect.DeepEqual(got, before) {
				t.Errorf("%s: entry %q compiles to %+v, %v, and did to %+v, %v", key, entry, got, err, before, errBefore)
			}
		}
		for _, entry := range outside {
			allowed, denied := lists(entry)
			r, _ := firstOutsideASCII(entry)
			want := fmt.Sprintf("request.%s entry %q has %#U, a character outside ASCII: write an internationalised name in its ASCII form, as xn--bcher-kva.example for bücher.example", key, entry, r)
			if got, err := compileTargetPolicy(allowed, denied); got != nil || err == nil || err.Error() != want {
				t.Errorf("%s: entry %q compiles to %+v, %v, want the error %q", key, entry, got, err, want)
			}
		}
	}
	// What such entries were read as: the Kelvin sign as k and the dotted
	// capital I as i, names nobody wrote, and an entry with a space outside
	// ASCII around it as the entry without.
	for entry, read := range map[string]string{"\u212a.example": "k.example", "\u0130nternal.example": "internal.example", "api.example\u00a0": "api.example", "\u3000api.example": "api.example"} {
		before, err := compileTargetPolicyBefore(nil, []string{entry})
		if err != nil || len(before.denyNames) != 1 || before.denyNames[0] != read {
			t.Errorf("entry %q was read as %+v, %v, want %s", entry, before, err, read)
		}
	}
}

// An entry of allowed_targets, denied_targets or redirect_trusted_hosts
// with a character outside ASCII stops the load, saying which character and
// how an internationalised name is written: such an entry matched no host a
// request is made to, or, where lower case made a letter of the character,
// another host than the one written. The xn-- form loads, and matches the
// host written so, in either case.
func TestAListEntryOutsideASCIIIsRefusedAtLoad(t *testing.T) {
	advice := ", a character outside ASCII: write an internationalised name in its ASCII form, as xn--bcher-kva.example for bücher.example"
	for entry, character := range map[string]string{
		"bücher.example":         "U+00FC 'ü'",
		"*.bücher.example":       "U+00FC 'ü'",
		"BÜCHER.example":         "U+00DC 'Ü'",
		"ｏrigin.test":            "U+FF4F 'ｏ'",
		"origin。test":            "U+3002 '。'",
		"\u212a.example":         "U+212A '\u212a'",
		"\u0130nternal.example":  "U+0130 '\u0130'",
		"１２７.０.０.１":              "U+FF11 '１'",
		"api.example\u00a0":      "U+00A0",
		"\u3000api.example":      "U+3000",
		"a\u200db.example":       "U+200D",
		"\u202eelpmaxe.example":  "U+202E",
		"xn--bcher-kva.example。": "U+3002 '。'",
	} {
		for key, set := range map[string]func(*model.Collector, []string){
			"allowed_targets":        func(c *model.Collector, entries []string) { c.Request.AllowedTargets = entries },
			"denied_targets":         func(c *model.Collector, entries []string) { c.Request.DeniedTargets = entries },
			"redirect_trusted_hosts": func(c *model.Collector, entries []string) { c.Request.RedirectTrustedHosts = entries },
		} {
			c := model.Collector{Name: "web", Request: model.RequestConfig{Type: RequestTypeHTTP}}
			set(&c, []string{"api.example", entry})
			want := fmt.Sprintf("collector \"web\" request.%s entry %q has %s%s", key, entry, character, advice)
			if err := ValidateRequest(&c); err == nil || err.Error() != want {
				t.Errorf("%s: [%q]: err=%v, want %q", key, entry, err, want)
			}
		}
	}
	// Behind a proxy, which every name here is left to: the xn-- form is
	// matched by an entry written so, in either case, and asked of the proxy
	// as the target writes it.
	chain := newRedirectChain()
	behindProxy(t, chain.wireServer(t))
	c := httpCollector(t, func(c *model.Collector) {
		c.Request.AllowedTargets = []string{"xn--bcher-kva.example", "XN--RIGIN-QR33A.test", "xn--origintest-*"}
		c.Request.DeniedTargets = []string{"xn--origintest-sh3i"}
		c.Request.RedirectTrustedHosts = []string{"XN--bcher-kva.example"}
	})
	for host, want := range map[string]string{
		"xn--bcher-kva.example":  "",
		"XN--BCHER-KVA.Example.": "",
		"xn--rigin-qr33a.test":   "",
		"xn--origintest-6f99c":   "",
		"origin.test":            "target origin.test refused: it is not in request.allowed_targets",
		"xn--bcher-2pa.example":  "target xn--bcher-2pa.example refused: it is not in request.allowed_targets",
		"xn--origintest-sh3i":    `target xn--origintest-sh3i refused: it matches "xn--origintest-sh3i" in request.denied_targets`,
	} {
		_, err := FetchCollector(context.Background(), "http://"+host+"/metrics", c, RequestOverrides{}, nil)
		got := chain.requests()
		if want != "" {
			if err == nil || err.Error() != want || len(got) != 0 {
				t.Errorf("%s: err=%v and the proxy was sent %+v, want %q and nothing sent", host, err, got, want)
			}
			continue
		}
		if line := "GET http://" + host + "/metrics HTTP/1.1\r\nHost: " + host + "\r\n"; err != nil || len(got) != 1 || !strings.HasPrefix(got[0].raw, line) {
			t.Errorf("%s: err=%v and the proxy was sent %+v, want %q", host, err, got, line)
		}
	}
	if !trustedRedirectHost(c.Request.RedirectTrustedHosts, "xn--BCHER-kva.example") || trustedRedirectHost(c.Request.RedirectTrustedHosts, "bücher.example") {
		t.Error("the xn-- entry does not trust the host written so alone")
	}
}
