//go:build !select_request_types || request_type_http

package fetch

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A redirect is judged by the host Go routes it by (redirecttrust.go,
// wireHost): these tests look at what the host a redirect led to received.

// A host with a final dot is another name than the same host without one: a
// resolver looks a rooted name up apart, and a proxy is asked for it as it
// is written. So a redirect from api.example to api.example. leaves the
// origin, and is sent the neutral headers alone, as is one the other way.
// Go alone already kept Authorization and Cookie from that hop; the first
// version of this rule, which compared the names without the dot, sent it
// everything. An entry of the list is matched as it is written too: the name
// without the dot does not trust the name with it, and the name with the dot
// trusts that one alone.
func TestAFinalDotIsAnotherHost(t *testing.T) {
	chain := newRedirectChain()
	// The chain's server stands in for the proxy every name is reached
	// through: its requests name the host as the client wrote it.
	behindProxy(t, chain.server(t).URL)
	c := trustCollector(t, nil)
	chain.redirect("/start", http.StatusFound, "http://api.example./end")
	got := follow(t, chain, c, "http://api.example/start")
	assertCarried(t, "to the name with a dot", got, carriesEverything, carriesNeutral)
	if got[1].host != "api.example." {
		t.Fatalf("the redirect asked the proxy for %q, want api.example.", got[1].host)
	}
	before := goAlone(t, chain, c, "http://api.example/start", got[0])
	if len(before) != 2 || before[1].header.Get("Authorization") != "" || before[1].header.Get("Cookie") != "" || before[1].header.Get("X-Api-Key") != "k3y" {
		t.Fatalf("Go alone sent the name with a dot %+v, want neither Authorization nor Cookie", before)
	}
	chain.redirect("/start", http.StatusFound, "http://api.example/end")
	assertCarried(t, "from the name with a dot", follow(t, chain, c, "http://api.example./start"), carriesEverything, carriesNeutral)
	chain.redirect("/start", http.StatusFound, "http://API.example./end")
	assertCarried(t, "on the name with a dot", follow(t, chain, c, "http://api.example./start"), carriesEverything, carriesEverything)

	for _, tc := range []struct{ entry, location, want string }{
		{"api.example", "http://api.example./end", carriesNeutral},
		{"*.example", "http://api.example./end", carriesNeutral},
		{"api.example.", "http://api.example./end", carriesEverything},
		{"*.example.", "http://api.example./end", carriesEverything},
		{"*", "http://api.example./end", carriesEverything},
		{"api.example.", "http://api.example/end", carriesNeutral},
		{"api.example", "http://api.example/end", carriesEverything},
	} {
		listed := trustCollector(t, func(c *model.Collector) { c.Request.RedirectTrustedHosts = []string{tc.entry} })
		chain.redirect("/start", http.StatusFound, tc.location)
		assertCarried(t, tc.entry+" for "+tc.location, follow(t, chain, listed, "http://first.example/start"), carriesEverything, tc.want)
	}
}

// A host written with a character outside ASCII is converted before it is
// used, and Go converts it one way for the address it dials and another for
// the Host header and the request line a proxy reads: "ｏrigin.test", with a
// fullwidth o, is dialed as origin.test and asked of a proxy as
// xn--rigin-qr33a.test, and "origin。test" as xn--origintest-sh3i. Neither
// is the origin, whatever a tidied form of it looks like, and no entry of
// the list matches such a host, not the origin's name and not "*": the
// target policy refuses it (canonicalHost), so a redirect to one is not
// followed, and the proxy is asked for the first URL alone, where it was
// asked for the converted name and sent the neutral headers. A first URL
// written so is refused before the proxy is asked anything. An
// internationalised name is trusted in its xn-- form: listed so, and
// redirected to so, or the origin when the first URL writes it so.
func TestARedirectToAHostOutsideASCIIIsNotFollowed(t *testing.T) {
	chain := newRedirectChain()
	behindProxy(t, chain.server(t).URL)
	for _, tc := range []struct {
		entries         []string
		first, location string
		// asked is how many requests the proxy is sent before the refusal.
		asked int
	}{
		{nil, "http://origin.test/start", "http://ｏrigin.test/end", 1},
		{nil, "http://origin.test/start", "http://origin。test/end", 1},
		{[]string{"origin.test"}, "http://first.test/start", "http://ｏrigin.test/end", 1},
		{[]string{"*.test", "*"}, "http://first.test/start", "http://ｏrigin.test/end", 1},
		{[]string{"xn--bcher-kva.example"}, "http://first.test/start", "http://bücher.example/end", 1},
		{nil, "http://bücher.example/start", "http://xn--bcher-kva.example/end", 0},
		{nil, "http://bücher.example/start", "/end", 0},
	} {
		c := trustCollector(t, func(c *model.Collector) { c.Request.RedirectTrustedHosts = tc.entries })
		chain.redirect("/start", http.StatusFound, tc.location)
		chain.requests()
		name := tc.first + " to " + tc.location
		_, err := FetchCollector(context.Background(), tc.first, c, RequestOverrides{}, forwardedTenant)
		if !errors.Is(err, ErrTargetRefused) || !strings.Contains(err.Error(), "a character outside ASCII") {
			t.Errorf("%s: err=%v, want the host outside ASCII refused", name, err)
		}
		if got := chain.requests(); len(got) != tc.asked {
			t.Errorf("%s: the proxy was sent %d requests, want %d: %+v", name, len(got), tc.asked, got)
		}
	}
	for _, tc := range []struct {
		entries         []string
		first, location string
	}{
		{[]string{"xn--bcher-kva.example"}, "http://first.test/start", "http://xn--bcher-kva.example/end"},
		{nil, "http://xn--bcher-kva.example/start", "http://XN--bcher-kva.example/end"},
		{nil, "http://xn--bcher-kva.example/start", "/end"},
	} {
		c := trustCollector(t, func(c *model.Collector) { c.Request.RedirectTrustedHosts = tc.entries })
		chain.redirect("/start", http.StatusFound, tc.location)
		got := follow(t, chain, c, tc.first)
		name := tc.first + " to " + tc.location
		assertCarried(t, name, got, carriesEverything, carriesEverything)
		if !strings.EqualFold(got[1].host, "xn--bcher-kva.example") {
			t.Errorf("%s: the proxy was asked for %q, want xn--bcher-kva.example", name, got[1].host)
		}
	}
}

// Other spellings that Go routes to the first URL's own server are not its
// origin either, since the verdict is on the URL as written: a port with a
// zero before it and the address in its IPv4-mapped form reach the same
// listener and are sent the neutral headers alone. The same name in capitals
// is the same name, and is sent everything.
func TestAnotherSpellingOfTheOriginIsNotTheOrigin(t *testing.T) {
	chain := newRedirectChain()
	server := chain.server(t)
	u, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	c := trustCollector(t, nil)
	for _, tc := range []struct{ first, location, want string }{
		{"http://localhost:" + u.Port(), "HTTP://LOCALHOST:" + u.Port() + "/end", carriesEverything},
		{"http://localhost:" + u.Port(), "http://localhost:0" + u.Port() + "/end", carriesNeutral},
		{server.URL, "http://127.0.0.1:0" + u.Port() + "/end", carriesNeutral},
		{server.URL, "http://[::ffff:127.0.0.1]:" + u.Port() + "/end", carriesNeutral},
		{server.URL, "http://[::FFFF:7f00:1]:" + u.Port() + "/end", carriesNeutral},
	} {
		chain.redirect("/start", http.StatusFound, tc.location)
		got := follow(t, chain, c, tc.first+"/start")
		assertCarried(t, tc.location, got, carriesEverything, tc.want)
		if got[1].path != "/end" {
			t.Errorf("%s: the second request asked for %s", tc.location, got[1].path)
		}
	}
}

// A Location names the whole URL of the next request: the first URL's path
// and the collector's query, which the first host is sent and so sees, are
// not carried to the host a redirect leads to, in its request line or in a
// Referer. The three neutral headers are, with the first request's values.
func TestTheFirstURLsPathAndQueryAreNotCarriedToAnotherHost(t *testing.T) {
	chain := newRedirectChain()
	origin := chain.wireServer(t)
	elsewhere := atHost(t, origin, "localhost")
	c := trustCollector(t, func(c *model.Collector) {
		c.Request.Path, c.Request.Query = "/start", map[string]string{"api_key": "qu3ry"}
	})
	chain.redirect("/tenant-7/start", http.StatusFound, elsewhere+"/end")
	got := follow(t, chain, c, origin+"/tenant-7")
	if len(got) != 2 || !strings.HasPrefix(got[0].raw, "GET /tenant-7/start?api_key=qu3ry HTTP/1.1\r\n") {
		t.Fatalf("the first request is %+v", got)
	}
	want := "GET /end HTTP/1.1\r\nHost: " + strings.TrimPrefix(elsewhere, "http://") + "\r\nUser-Agent: probe/1\r\nAccept: application/json\r\nAccept-Language: en\r\nAccept-Encoding: gzip\r\n\r\n"
	if got[1].raw != want {
		t.Fatalf("the redirect is on the wire\n%q\nwant\n%q", got[1].raw, want)
	}
}

// For every pair of hosts that have no final dot, no zone and no character
// outside ASCII, the origin and the list decide as they did when they
// compared the hosts as the target policy writes them, which is copied here:
// the stricter reading changes the verdict for those three kinds of host
// alone. So do the entries: one that still loads reads as it did.
func TestTheVerdictIsWhatItWasForPlainHosts(t *testing.T) {
	sameOriginBefore := func(a, b *url.URL) bool {
		if !strings.EqualFold(a.Scheme, b.Scheme) {
			return false
		}
		hostA, err := canonicalHost(a.Hostname())
		if err != nil {
			return false
		}
		hostB, err := canonicalHost(b.Hostname())
		if err != nil {
			return false
		}
		return hostA == hostB && effectivePort(a) == effectivePort(b)
	}
	parseBefore := func(raw string) (string, netip.Addr, bool) {
		entry := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(raw), "."))
		entry = strings.TrimSuffix(strings.TrimPrefix(entry, "["), "]")
		if written, err := netip.ParseAddr(entry); err == nil {
			return "", written.Unmap().WithZone(""), true
		}
		return entry, netip.Addr{}, entry != "" && hostGlob.MatchString(entry)
	}
	trustedBefore := func(entry, host string) bool {
		if canonical, err := canonicalHost(host); err == nil {
			host = canonical
		}
		glob, addr, ok := parseBefore(entry)
		if !ok {
			return false
		}
		if addr.IsValid() {
			written, err := netip.ParseAddr(host)
			return err == nil && written.Unmap().WithZone("") == addr
		}
		matched, _ := path.Match(glob, host)
		return matched
	}
	hosts := []string{"api.example", "API.Example", "www.api.example", "example", "my_service", "xn--bcher-kva.example", "localhost", "127.0.0.1", "127.1", "0x7f.0.0.1", "2130706433", "::1", "0:0:0:0:0:0:0:1", "::ffff:127.0.0.1", "::FFFF:7f00:1", "fe80::1", "192.0.2.7", "a-1.example", "api%65.example", "evilexample.com", "api.example.evil.net"}
	entries := []string{"api.example", "API.EXAMPLE", "*.example", "*", "a-?.example", "*example.com", "api.example.*", "my_service", "localhost", "127.0.0.1", "::1", "[::1]", "::ffff:127.0.0.1", "0:0:0:0:0:0:0:1", "fe80::1", "192.0.2.7", " api.example ", "[api.example]", "xn--bcher-kva.example", "10.0.0.0/8", "api.example:80", "https://api.example", ""}
	for _, a := range hosts {
		for _, b := range hosts {
			for _, pair := range [][2]string{{"http://%s/a", "http://%s:80/b"}, {"http://%s", "https://%s"}, {"https://%s:8443", "https://%s:8443"}} {
				first, errA := url.Parse(strings.Replace(pair[0], "%s", bracketed(a), 1))
				second, errB := url.Parse(strings.Replace(pair[1], "%s", bracketed(b), 1))
				if errA != nil || errB != nil {
					t.Fatalf("%s, %s: %v %v", a, b, errA, errB)
				}
				if got, want := sameOrigin(first, second), sameOriginBefore(first, second); got != want {
					t.Errorf("sameOrigin(%s, %s) = %v, and was %v", first, second, got, want)
				}
			}
		}
		for _, entry := range entries {
			if got, want := trustedRedirectHost([]string{entry}, a), trustedBefore(entry, a); got != want {
				t.Errorf("entry %q, host %q: trusted %v, and was %v", entry, a, got, want)
			}
		}
	}
	for _, entry := range entries {
		glob, addr, err := parseTrustedHost(entry)
		globBefore, addrBefore, ok := parseBefore(entry)
		if (err == nil) != ok || err == nil && (glob != globBefore || addr != addrBefore) {
			t.Errorf("entry %q reads as %q %v (%v), and read as %q %v (loads: %v)", entry, glob, addr, err, globBefore, addrBefore, ok)
		}
	}
}

// bracketed is a host as a URL writes it: a '%' escaped, and an IPv6 address
// in brackets.
func bracketed(host string) string {
	host = strings.ReplaceAll(host, "%", "%25")
	if strings.Contains(host, ":") {
		return "[" + host + "]"
	}
	return host
}

// A user and password in the Location of a redirect that is not trusted are
// not made an Authorization of: Go would add one from them after the headers
// were settled, and the hop is sent the neutral headers and nothing else,
// its URL traced without them. On a trusted hop they are the target's own
// word for where to go, and are used as Go uses them: as basic auth when the
// request has no Authorization, while the collector's own credential, when
// it has one, is the one sent.
func TestCredentialsInALocationAreNotSentToAnUntrustedHost(t *testing.T) {
	chain := newRedirectChain()
	server := chain.server(t)
	elsewhere := atHost(t, server.URL, "localhost")
	chain.redirect("/start", http.StatusFound, strings.Replace(elsewhere, "http://", "http://bob:hunter2@", 1)+"/end")
	ctx, trace := WithRequestTrace(context.Background())
	chain.requests()
	if _, err := FetchCollector(ctx, server.URL+"/start", trustCollector(t, nil), RequestOverrides{}, forwardedTenant); err != nil {
		t.Fatal(err)
	}
	assertCarried(t, "not trusted", chain.requests(), carriesEverything, carriesNeutral)
	traced := trace.Requests()
	if len(traced) != 2 || traced[1].URL != elsewhere+"/end" || traced[1].Header.Get("Authorization") != "" {
		t.Fatalf("the hop is traced as %+v, want its URL without the user and no Authorization", traced)
	}
	if got := strings.Join(traced[1].Withheld, ", "); got != "Authorization, Cookie, X-Api-Key, X-Tenant" {
		t.Errorf("the hop's withheld headers are %q", got)
	}

	listed := trustCollector(t, func(c *model.Collector) { c.Request.RedirectTrustedHosts = []string{"localhost"} })
	assertCarried(t, "trusted, with a credential of the collector's", follow(t, chain, listed, server.URL+"/start"), carriesEverything, carriesEverything)
	bare := httpCollector(t, func(c *model.Collector) {
		c.Request.FollowRedirects = true
		c.Request.RedirectTrustedHosts = []string{"localhost"}
	})
	if got := follow(t, chain, bare, server.URL+"/start"); len(got) != 2 || got[0].header.Get("Authorization") != "" || got[1].header.Get("Authorization") != "Basic Ym9iOmh1bnRlcjI=" {
		t.Fatalf("trusted, without a credential of the collector's: %+v, want the Location's as basic auth", got)
	}
}

// A credential in the userinfo of the target URL is a credential like the
// collector's own: when it is the only one, and a redirect to a host that is
// not trusted leaves it behind, the answer says so, and a trace names it
// among what was not sent.
func TestCredentialsOfTheTargetURLCountAsWithheld(t *testing.T) {
	chain := newRedirectChain()
	server := chain.server(t)
	chain.redirect("/start", http.StatusFound, atHost(t, server.URL, "localhost")+"/end")
	chain.redirect("/end", http.StatusUnauthorized, "")
	c := httpCollector(t, func(c *model.Collector) { c.Request.FollowRedirects = true })
	ctx, trace := WithRequestTrace(context.Background())
	chain.requests()
	response, err := FetchCollector(ctx, strings.Replace(server.URL, "http://", "http://scraper:pass@", 1)+"/start", c, RequestOverrides{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := chain.requests()
	if len(got) != 2 || got[0].header.Get("Authorization") != "Basic c2NyYXBlcjpwYXNz" || got[1].header.Get("Authorization") != "" {
		t.Fatalf("%+v, want the credential sent to the target alone", got)
	}
	if !strings.HasPrefix(response.RedirectWithheld, "from localhost, where a redirect led: the collector's headers and credentials were not sent to that host") {
		t.Errorf("RedirectWithheld is %q", response.RedirectWithheld)
	}
	traced := trace.Requests()
	if len(traced) != 2 || strings.Join(traced[1].Withheld, ", ") != "Authorization (the target URL's credentials)" {
		t.Errorf("the hop is traced as %+v", traced)
	}
	// With an Authorization of the collector's the URL's credential is not
	// used, and is not named.
	ctx, trace = WithRequestTrace(context.Background())
	bearer := httpCollector(t, func(c *model.Collector) { c.Request.FollowRedirects, c.Request.BearerToken = true, "s3cret" })
	if _, err := FetchCollector(ctx, strings.Replace(server.URL, "http://", "http://scraper:pass@", 1)+"/start", bearer, RequestOverrides{}, nil); err != nil {
		t.Fatal(err)
	}
	if traced := trace.Requests(); len(traced) != 2 || strings.Join(traced[1].Withheld, ", ") != "Authorization" {
		t.Errorf("with a bearer token the hop is traced as %+v", traced)
	}
}

// The first URL's own host under another scheme or on another port is
// another origin, and what is said of it names both origins, since the host
// alone would not show the difference: in the answer of such a hop, in the
// trace, and in the refusal of a body. The upgrade from http to https on one
// host is the commonest such redirect.
func TestMessagesNameBothOriginsOfOneHost(t *testing.T) {
	chain := newRedirectChain()
	server, other := chain.server(t), chain.server(t)
	chain.redirect("/start", http.StatusFound, other.URL+"/end?token=abc")
	chain.redirect("/end", http.StatusUnauthorized, "")
	ctx, trace := WithRequestTrace(context.Background())
	response, err := FetchCollector(ctx, server.URL+"/start", trustCollector(t, nil), RequestOverrides{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := "from " + other.URL + ", where a redirect led: the collector's headers and credentials were not sent there, since " + other.URL + " is not the origin the request was made to, " + server.URL + "; list 127.0.0.1 in request.redirect_trusted_hosts if it is to be sent them"; response.RedirectWithheld != want {
		t.Errorf("RedirectWithheld is %q, want %q", response.RedirectWithheld, want)
	}
	traced := trace.Requests()
	if want := "the redirect leads to " + other.URL + ", which is not the origin the request was made to, " + server.URL + ", and whose host is not in request.redirect_trusted_hosts"; len(traced) != 2 || traced[1].WithheldWhy != want {
		t.Errorf("the hop is traced as %+v, want the reason %q", traced, want)
	}
	// Another host is named alone, as it was.
	chain.redirect("/start", http.StatusFound, atHost(t, server.URL, "localhost")+"/end")
	ctx, trace = WithRequestTrace(context.Background())
	if _, err := FetchCollector(ctx, server.URL+"/start", trustCollector(t, nil), RequestOverrides{}, nil); err != nil {
		t.Fatal(err)
	}
	if traced := trace.Requests(); len(traced) != 2 || traced[1].WithheldWhy != "the redirect leads to a host that is not the origin the request was made to and is not in request.redirect_trusted_hosts" {
		t.Errorf("a hop to another host is traced as %+v", traced)
	}

	// The refusal of a body is made before anything is sent, so the proxy
	// that stands for api.example here is never asked for the https URL.
	behindProxy(t, server.URL)
	for location, origin := range map[string]string{
		"https://api.example/end?token=abc": "https://api.example",
		"http://API.example:8080/end":       "http://api.example:8080",
	} {
		chain.redirect("/start", http.StatusTemporaryRedirect, location)
		chain.requests()
		_, err = FetchCollector(context.Background(), "http://api.example/start", postCollector(t, nil), RequestOverrides{}, nil)
		want := " refused: it would send the request body to " + origin + ", which is not the origin the request was made to, http://api.example, and whose host is not listed in request.redirect_trusted_hosts"
		if err == nil || !strings.HasSuffix(err.Error(), want) || strings.Contains(err.Error(), "abc") {
			t.Errorf("%s: err=%v, want an error ending in %q", location, err, want)
		}
		if got := chain.requests(); len(got) != 1 {
			t.Errorf("%s: %d requests were sent, want the first alone", location, len(got))
		}
	}
}

// selfSigned makes a self-signed certificate for the names given and writes
// it and its key to files.
func selfSigned(t *testing.T, commonName string, names ...string) (certFile, keyFile string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: commonName}, DNSNames: names,
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
		IsCA: true, BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certFile, keyFile = filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	for file, block := range map[string]*pem.Block{certFile: {Type: "CERTIFICATE", Bytes: der}, keyFile: {Type: "EC PRIVATE KEY", Bytes: keyDER}} {
		if err := os.WriteFile(file, pem.EncodeToMemory(block), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return certFile, keyFile
}

// countingListener counts the connections made to a server.
type countingListener struct {
	net.Listener
	accepted atomic.Int32
}

func (l *countingListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err == nil {
		l.accepted.Add(1)
	}
	return conn, err
}

// certificateServer is a server of the chain over HTTPS that asks its
// clients for a certificate, and the count of the connections made to it.
func (c *redirectChain) certificateServer(t *testing.T) (*httptest.Server, *countingListener) {
	t.Helper()
	server := httptest.NewUnstartedServer(c)
	listener := &countingListener{Listener: server.Listener}
	server.Listener = listener
	server.TLS = &tls.Config{ClientAuth: tls.RequestClientCert, MinVersion: tls.VersionTLS12}
	server.StartTLS()
	t.Cleanup(server.Close)
	return server, listener
}

// The collector's TLS client certificate is a credential too, and one
// transport presents it on every TLS connection. A redirect over https to a
// host that is not trusted is therefore refused before a connection is made
// there: the probe fails, saying what would have been presented to whom and
// which setting trusts the host, and is not retried. The origin and a listed
// host are presented the certificate as before, and a redirect over plain
// http, where nothing is presented, is followed with the neutral headers.
func TestAClientCertificateIsNotPresentedToAnUntrustedHost(t *testing.T) {
	certFile, keyFile := selfSigned(t, "collector-identity")
	chain := newRedirectChain()
	origin, _ := chain.certificateServer(t)
	other, connections := chain.certificateServer(t)
	plain := chain.server(t)
	identified := func(edit func(*model.Collector)) *model.Collector {
		return trustCollector(t, func(c *model.Collector) {
			c.Request.TLS = model.TLSConfig{InsecureSkipVerify: true, CertFile: certFile, KeyFile: keyFile}
			c.Request.Retry.Attempts = 2
			if edit != nil {
				edit(c)
			}
		})
	}
	for name, hop := range map[string]struct{ location, to string }{
		"another host":                  {atHost(t, other.URL, "localhost"), "localhost, which is not the origin the request was made to and is not listed in request.redirect_trusted_hosts"},
		"the same host on another port": {other.URL, other.URL + ", which is not the origin the request was made to, " + origin.URL + ", and whose host is not listed in request.redirect_trusted_hosts"},
	} {
		chain.redirect("/start", http.StatusFound, hop.location+"/end?token=abc")
		chain.requests()
		response, err := FetchCollector(context.Background(), origin.URL+"/start", identified(nil), RequestOverrides{}, forwardedTenant)
		want := "redirect to " + hop.location + "/end?token=<redacted> refused: it would present the collector's TLS client certificate to " + hop.to
		if response != nil || err == nil || !strings.HasSuffix(err.Error(), want) {
			t.Fatalf("%s: response=%v err=%v, want an error ending in %q", name, response, err, want)
		}
		got := chain.requests()
		if len(got) != 1 || got[0].certificate != "collector-identity" {
			t.Fatalf("%s: %+v, want the first request alone, with the certificate", name, got)
		}
		if n := connections.accepted.Load(); n != 0 {
			t.Fatalf("%s: %d connections were made to the host that is not trusted", name, n)
		}
	}
	// What refuses the host itself is said first.
	denied := identified(func(c *model.Collector) { c.Request.DeniedTargets = []string{"localhost"} })
	chain.redirect("/start", http.StatusFound, atHost(t, other.URL, "localhost")+"/end")
	if _, err := FetchCollector(context.Background(), origin.URL+"/start", denied, RequestOverrides{}, nil); !errors.Is(err, ErrTargetRefused) {
		t.Fatalf("a denied host: %v, want it refused by request.denied_targets", err)
	}

	listed := identified(func(c *model.Collector) { c.Request.RedirectTrustedHosts = []string{"localhost"} })
	got := follow(t, chain, listed, origin.URL+"/start")
	assertCarried(t, "a listed host", got, carriesEverything, carriesEverything)
	if got[1].certificate != "collector-identity" || connections.accepted.Load() != 1 {
		t.Fatalf("a listed host was presented %q on %d connections", got[1].certificate, connections.accepted.Load())
	}
	chain.redirect("/start", http.StatusFound, "/end")
	if got := follow(t, chain, identified(nil), origin.URL+"/start"); len(got) != 2 || got[1].certificate != "collector-identity" {
		t.Fatalf("on the origin: %+v", got)
	}
	chain.redirect("/start", http.StatusFound, plain.URL+"/end")
	got = follow(t, chain, identified(nil), origin.URL+"/start")
	assertCarried(t, "plain http", got, carriesEverything, carriesNeutral)
	if got[1].certificate != "" {
		t.Fatalf("a redirect over plain http was presented %q", got[1].certificate)
	}
	// Without a client certificate an https redirect to another host is
	// followed, as it was.
	chain.redirect("/start", http.StatusFound, atHost(t, other.URL, "localhost")+"/end")
	anonymous := trustCollector(t, func(c *model.Collector) { c.Request.TLS.InsecureSkipVerify = true })
	assertCarried(t, "no certificate", follow(t, chain, anonymous, origin.URL+"/start"), carriesEverything, carriesNeutral)
}

// tls.server_name, tls.ca_file and tls.insecure_skip_verify are no
// credentials and hold for every hop, since one transport makes them all: a
// server_name written for the target is also the name a redirect's https
// host must hold a certificate for, so a redirect to a host with another
// certificate fails verification, and one to a host with the target's
// certificate is followed.
func TestAServerNameOverrideHoldsForEveryHop(t *testing.T) {
	chain := newRedirectChain()
	origin, sibling := chain.tlsServer(t), chain.tlsServer(t)
	otherCert, otherKey := selfSigned(t, "other.test", "other.test", "localhost")
	pair, err := tls.LoadX509KeyPair(otherCert, otherKey)
	if err != nil {
		t.Fatal(err)
	}
	other := httptest.NewUnstartedServer(chain)
	other.TLS = &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12}
	other.StartTLS()
	t.Cleanup(other.Close)
	// Both certificates are trusted; only the name decides.
	authorities, err := os.ReadFile(otherCert)
	if err != nil {
		t.Fatal(err)
	}
	authorities = append(authorities, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: origin.Certificate().Raw})...)
	caFile := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caFile, authorities, 0o600); err != nil {
		t.Fatal(err)
	}
	c := trustCollector(t, func(c *model.Collector) {
		c.Request.TLS = model.TLSConfig{CAFile: caFile, ServerName: "example.com"}
	})
	chain.redirect("/start", http.StatusFound, sibling.URL+"/end")
	assertCarried(t, "a host with the target's certificate", follow(t, chain, c, origin.URL+"/start"), carriesEverything, carriesNeutral)
	chain.redirect("/start", http.StatusFound, atHost(t, other.URL, "localhost")+"/end")
	chain.requests()
	_, err = FetchCollector(context.Background(), origin.URL+"/start", c, RequestOverrides{}, nil)
	if err == nil || !strings.Contains(err.Error(), "not example.com") {
		t.Fatalf("a host with another certificate: %v, want its certificate refused for example.com", err)
	}
	if got := chain.requests(); len(got) != 1 {
		t.Fatalf("%d requests were sent, want the first alone", len(got))
	}
}
