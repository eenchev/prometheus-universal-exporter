//go:build !select_request_types || request_type_http

package fetch

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// tripPause is how long the slow target waits before its headers and again
// before its body.
const tripPause = 60 * time.Millisecond

// The timings an answer carries are of the request that was answered,
// measured through httptrace: a new connection to an IP address over plain
// HTTP has a connection and no lookup or handshake; the next request on the
// same connection has none of the three; a name is looked up; a TLS
// target's handshake is timed; and after a redirect they are of the request
// it led to. The first byte is the target's first pause, the total the
// body's too.
func TestTimingsAreOfTheAnsweredRequest(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(tripPause)
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		time.Sleep(tripPause)
		_, _ = w.Write([]byte("done"))
	}))
	defer slow.Close()
	c := httpCollector(t, nil)
	fetchTrip := func(target string, c *model.Collector) *HTTPTrip {
		t.Helper()
		response, err := FetchCollector(context.Background(), target, c, RequestOverrides{}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if response.Trip == nil {
			t.Fatalf("%s: no trip", target)
		}
		return response.Trip
	}

	first := fetchTrip(slow.URL, c)
	if first.Connect == nil || first.DNS != nil || first.TLS != nil || first.FirstByte == nil {
		t.Fatalf("a new connection over plain HTTP to an IP address: %+v", first)
	}
	if *first.FirstByte < tripPause || first.Total-*first.FirstByte < tripPause*5/6 || *first.Connect > *first.FirstByte {
		t.Errorf("first byte %s and total %s of a target pausing %s before each", *first.FirstByte, first.Total, tripPause)
	}
	if again := fetchTrip(slow.URL, c); again.Connect != nil || again.DNS != nil || again.TLS != nil || again.FirstByte == nil || *again.FirstByte < tripPause {
		t.Errorf("a reused connection: %+v", again)
	}
	if named := fetchTrip(strings.Replace(slow.URL, "127.0.0.1", "localhost", 1), c); named.DNS == nil || named.Connect == nil {
		t.Errorf("a target by name: %+v", named)
	}

	secure := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) }))
	defer secure.Close()
	insecure := httpCollector(t, func(c *model.Collector) {
		c.Request.TLS.InsecureSkipVerify = true
		c.Request.FollowRedirects = true
	})
	if handshake := fetchTrip(secure.URL, insecure); handshake.TLS == nil || handshake.Connect == nil {
		t.Errorf("a TLS target: %+v", handshake)
	}
	// The plain HTTP request redirected has no handshake: the one it led
	// to has, on a new connection, made by a pool of its own, which another
	// server_name gives the collector.
	redirecting := httptest.NewServer(http.RedirectHandler(secure.URL+"/there", http.StatusFound))
	defer redirecting.Close()
	insecure.Request.TLS.ServerName = "example.com"
	if redirected := fetchTrip(redirecting.URL, insecure); redirected.TLS == nil || redirected.Connect == nil {
		t.Errorf("a redirect to a TLS target: %+v", redirected)
	}

	if timings := (*HTTPTrip)(nil).JQTimings(); timings != nil {
		t.Errorf("no trip: %v", timings)
	}
	if timings := first.JQTimings().(map[string]any); timings["dns"] != nil || timings["tls"] != nil || timings["total"] != first.Total.Seconds() || timings["connect"] != first.Connect.Seconds() || len(timings) != 5 {
		t.Errorf("$timings %v", timings)
	}
}

// The TLS connection an answer came on is the target's, as the exporter
// made it: the version and cipher it settled on, the name sent as SNI,
// whether the chain was verified, false under insecure_skip_verify, and the
// leaf's validity and names. A plain HTTP request has none, even when its
// answer came over a TLS connection to an https:// proxy.
func TestTheTLSConnectionIsTheTargets(t *testing.T) {
	targetURL, ca := identityServer(t, func(w http.ResponseWriter, _ *http.Request, _ string) { _, _ = w.Write([]byte("ok")) })
	pemBytes, err := os.ReadFile(ca)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(pemBytes)
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	connectionOf := func(target string, c *model.Collector) *TLSConnection {
		t.Helper()
		response, err := FetchCollector(context.Background(), target, c, RequestOverrides{}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return response.Trip.TLSConnection
	}
	verifying := httpCollector(t, func(c *model.Collector) { c.Request.TLS.CAFile = ca })
	want := TLSConnection{TLSVersion: "TLS 1.3", ServerName: "localhost", Verified: true, NotBefore: leaf.NotBefore.Unix(), NotAfter: leaf.NotAfter.Unix(), SubjectCN: "server", IssuerCN: "server"}
	got := connectionOf(targetURL, verifying)
	if got == nil || !strings.HasPrefix(got.CipherSuite, "TLS_") {
		t.Fatalf("the connection %+v", got)
	}
	want.CipherSuite = got.CipherSuite
	if *got != want {
		t.Errorf("the connection %+v, want %+v", *got, want)
	}
	insecure := httpCollector(t, func(c *model.Collector) { c.Request.TLS.InsecureSkipVerify = true })
	if got := connectionOf(targetURL, insecure); got == nil || got.Verified || got.SubjectCN != "server" {
		t.Errorf("under insecure_skip_verify: %+v", got)
	}
	values := (&HTTPTrip{TLSConnection: &want}).JQTLS().(map[string]any)
	if values["tls_version"] != "TLS 1.3" || values["cipher_suite"] != want.CipherSuite || values["verified"] != true || values["not_after"] != float64(want.NotAfter) || values["subject_cn"] != "server" || values["server_name"] != "localhost" || len(values) != 8 {
		t.Errorf("$tls %v", values)
	}
	if (&HTTPTrip{}).JQTLS() != nil || (*HTTPTrip)(nil).JQTLS() != nil {
		t.Error("$tls of plain HTTP or of no trip is not null")
	}

	// A plain HTTP request through an https:// proxy is answered on the
	// TLS connection to the proxy, which is not the target's.
	proxyEnvironment(t)
	proxyCert, proxyKey := selfSigned(t, "proxy", "localhost")
	cert, err := tls.LoadX509KeyPair(proxyCert, proxyKey)
	if err != nil {
		t.Fatal(err)
	}
	proxy := &testProxy{}
	proxy.Server = httptest.NewUnstartedServer(proxy)
	proxy.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	proxy.StartTLS()
	t.Cleanup(proxy.Close)
	roots := x509.NewCertPool()
	proxyPEM, err := os.ReadFile(proxyCert)
	if err != nil {
		t.Fatal(err)
	}
	roots.AppendCertsFromPEM(proxyPEM)
	previous := proxyTLSRoots
	proxyTLSRoots = roots
	t.Cleanup(func() { proxyTLSRoots = previous })
	proxied := proxiedCollector(t, "api", strings.Replace(proxy.URL, "127.0.0.1", "localhost", 1))
	response, err := FetchCollector(context.Background(), "http://metrics.example:8080/status", proxied, RequestOverrides{}, nil)
	if err != nil || !strings.Contains(string(response.Body), "url=http://metrics.example:8080/status") {
		t.Fatalf("through the proxy: %v", err)
	}
	if response.Trip.TLSConnection != nil {
		t.Errorf("a plain HTTP request through an https:// proxy has the connection %+v", *response.Trip.TLSConnection)
	}
}

// The URL an answer carries is where the redirects led, without the
// userinfo and with the values of credential query and path parameters
// masked, as a target is shown in a log line, and the redirects are
// counted.
func TestTheURLIsWhereTheRedirectsLed(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			http.Redirect(w, r, "/hop", http.StatusFound)
		case "/hop":
			http.Redirect(w, r, "/end?token=t1&tenant=a", http.StatusMovedPermanently)
		case "/login":
			// A servlet's session in the path, and OAuth's code in the
			// query, of the redirect back from a login.
			http.Redirect(w, r, "/app;jsessionid=S3CRET?code=S3CRET2&state=x", http.StatusFound)
		default:
			_, _ = w.Write([]byte("ok"))
		}
	}))
	defer server.Close()
	tripOf := func(target string, follow bool) *HTTPTrip {
		t.Helper()
		c := httpCollector(t, func(c *model.Collector) {
			c.Request.FollowRedirects = follow
			c.Request.AcceptStatus = []string{"any"}
		})
		response, err := FetchCollector(context.Background(), target, c, RequestOverrides{}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return response.Trip
	}
	if trip := tripOf(server.URL, true); trip.ShownURL() != server.URL+"/end?token=<redacted>&tenant=a" || trip.Redirects != 2 {
		t.Errorf("after two redirects: %q, %d", trip.ShownURL(), trip.Redirects)
	}
	if trip := tripOf(server.URL, false); trip.ShownURL() != server.URL || trip.Redirects != 0 {
		t.Errorf("a redirect not followed: %q, %d", trip.ShownURL(), trip.Redirects)
	}
	withUser := strings.Replace(server.URL, "http://", "http://scraper:s3cret@", 1) + "/end?api_key=k&tenant=b#part"
	if trip := tripOf(withUser, true); trip.ShownURL() != server.URL+"/end?api_key=<redacted>&tenant=b" || trip.Redirects != 0 {
		t.Errorf("a URL with userinfo, a credential and a fragment: %q, %d", trip.ShownURL(), trip.Redirects)
	}
	if trip := tripOf(server.URL+"/login", true); trip.ShownURL() != server.URL+"/app;jsessionid=<redacted>?code=<redacted>&state=x" {
		t.Errorf("a redirect to a session in the path and a code in the query: %q", trip.ShownURL())
	}
	trip := &HTTPTrip{URL: &url.URL{Scheme: "http", Host: "x", Path: "/"}, Redirects: 1}
	if trip.JQURL() != "http://x/" || trip.JQRedirects() != 1 || (*HTTPTrip)(nil).JQURL() != nil || (*HTTPTrip)(nil).JQRedirects() != nil {
		t.Error("$url and $redirects")
	}
}

// A trip exported as a trace records each request's phases with its span's
// request (TracedRequest.Phases), each with when it began and ended, in the
// order they began: a redirect's request has its own, on its own
// connection, not the first request's; and the phases are timed for the
// trace although no rule of the collector reads $timings. A trip that is
// not traced records none.
func TestATracedTripRecordsEachRequestsPhases(t *testing.T) {
	secure := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) }))
	defer secure.Close()
	redirecting := httptest.NewServer(http.RedirectHandler(strings.Replace(secure.URL, "127.0.0.1", "localhost", 1)+"/there", http.StatusFound))
	defer redirecting.Close()
	c := httpCollector(t, func(c *model.Collector) {
		c.Request.TLS.InsecureSkipVerify = true
		c.Request.FollowRedirects = true
	})
	c.TimingsUnread = true
	ctx, trace := WithSpanTrace(context.Background())
	if _, err := FetchCollector(ctx, redirecting.URL, c, RequestOverrides{}, nil); err != nil {
		t.Fatal(err)
	}
	requests := trace.Requests()
	if len(requests) != 2 || !requests[1].Redirect {
		t.Fatalf("the requests are %+v", requests)
	}
	names := func(r TracedRequest) string {
		var out []string
		for _, p := range r.Phases {
			if p.End.Before(p.Start) || p.Start.Before(r.Start()) {
				t.Errorf("%s runs from %s to %s, the request from %s", p.Name, p.Start, p.End, r.Start())
			}
			out = append(out, p.Name)
		}
		return strings.Join(out, " ")
	}
	if got := names(requests[0]); got != "connect first_byte" {
		t.Errorf("the first request's phases are %q, want a connection and the first byte", got)
	}
	if got := names(requests[1]); got != "dns connect tls first_byte" {
		t.Errorf("the redirect's phases are %q, want a lookup, a connection, a handshake and the first byte", got)
	}
	untraced, err := FetchCollector(context.Background(), redirecting.URL, c, RequestOverrides{}, nil)
	if err != nil || untraced.Trip.FirstByte != nil {
		t.Errorf("an untraced trip of a collector that reads no $timings is timed: %v %+v", err, untraced.Trip)
	}
}

// A redirect the trip refuses to follow, recorded but never sent, has no
// phases: none of the request it was a redirect of, whose phases the trip
// still holds when it fails.
func TestARefusedRedirectHasNoPhases(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, server.URL+"/again", http.StatusFound)
	}))
	defer server.Close()
	c := httpCollector(t, func(c *model.Collector) { c.Request.FollowRedirects = true })
	c.TimingsUnread = true
	ctx, trace := WithSpanTrace(context.Background())
	if _, err := FetchCollector(ctx, server.URL, c, RequestOverrides{}, nil); err == nil || !strings.Contains(err.Error(), "stopped after 10 redirects") {
		t.Fatalf("the trip ended with %v, want the redirects stopped", err)
	}
	requests := trace.Requests()
	if len(requests) < 2 {
		t.Fatalf("the requests are %+v", requests)
	}
	for i, r := range requests[:len(requests)-1] {
		if len(r.Phases) == 0 || r.Phases[len(r.Phases)-1].Name != "first_byte" {
			t.Errorf("request %d, which was answered, has the phases %+v", i, r.Phases)
		}
	}
	if refused := requests[len(requests)-1]; refused.Phases != nil {
		t.Errorf("the refused redirect, never sent, has the phases %+v", refused.Phases)
	}
}
