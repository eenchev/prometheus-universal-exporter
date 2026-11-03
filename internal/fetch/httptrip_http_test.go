//go:build !select_request_types || request_type_http

package fetch

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
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
	want := TLSConnection{Version: "1.3", ServerName: "localhost", Verified: true, NotBefore: leaf.NotBefore.Unix(), NotAfter: leaf.NotAfter.Unix(), SubjectCN: "server", IssuerCN: "server"}
	got := connectionOf(targetURL, verifying)
	if got == nil || !strings.HasPrefix(got.Cipher, "TLS_") {
		t.Fatalf("the connection %+v", got)
	}
	want.Cipher = got.Cipher
	if *got != want {
		t.Errorf("the connection %+v, want %+v", *got, want)
	}
	insecure := httpCollector(t, func(c *model.Collector) { c.Request.TLS.InsecureSkipVerify = true })
	if got := connectionOf(targetURL, insecure); got == nil || got.Verified || got.SubjectCN != "server" {
		t.Errorf("under insecure_skip_verify: %+v", got)
	}
	values := (&HTTPTrip{TLSConnection: &want}).JQTLS().(map[string]any)
	if values["version"] != "1.3" || values["verified"] != true || values["not_after"] != float64(want.NotAfter) || values["subject_cn"] != "server" || values["server_name"] != "localhost" || len(values) != 8 {
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
