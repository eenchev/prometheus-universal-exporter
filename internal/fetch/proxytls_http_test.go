//go:build !select_request_types || request_type_http

package fetch

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// slowHandshake is how long the target through the proxy takes over its
// handshake, so that a TLS phase timed around anything else is told apart.
const slowHandshake = 150 * time.Millisecond

// Through an https:// proxy, $timings.tls is the target's handshake through
// the tunnel, and $tls the target's connection: the proxy's handshake is
// the exporter's own dialer's, made before the transport has the
// connection, and is neither timed nor taken for the target's. A plain
// HTTP target through the proxy has no tls phase and no $tls, and an https
// target whose handshake takes slowHandshake has a tls phase at least that
// long, and its own certificate.
func TestTheTLSPhaseThroughAnHTTPSProxyIsTheTargets(t *testing.T) {
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
	proxyURL := strings.Replace(proxy.URL, "127.0.0.1", "localhost", 1)

	plain, err := FetchCollector(context.Background(), "http://metrics.example:8080/status", proxiedCollector(t, "api", proxyURL), RequestOverrides{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if trip := plain.Trip; trip.TLS != nil || trip.TLSConnection != nil || trip.Connect == nil {
		t.Errorf("a plain HTTP target through an https:// proxy: connect %s, tls %s, $tls %+v; want the connection to the proxy and no handshake", durationOf(trip.Connect), durationOf(trip.TLS), trip.TLSConnection)
	}
	// The proxy's certificate is still verified: against roots that do
	// not hold it, a new connection to the proxy fails its handshake.
	proxyTLSRoots = x509.NewCertPool()
	fresh := true
	if _, err := FetchCollector(context.Background(), "http://metrics.example:8080/status", proxiedCollector(t, "api", proxyURL), RequestOverrides{NewConnection: &fresh}, nil); err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Errorf("an https:// proxy whose certificate does not verify: %v, want the certificate refused", err)
	}
	proxyTLSRoots = roots

	targetCert, targetKey := selfSigned(t, "server", "localhost")
	pair, err := tls.LoadX509KeyPair(targetCert, targetKey)
	if err != nil {
		t.Fatal(err)
	}
	target := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("value=1\n")) }))
	target.TLS = &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
		time.Sleep(slowHandshake)
		return &pair, nil
	}}
	target.StartTLS()
	t.Cleanup(target.Close)
	secure := proxiedCollector(t, "api2", proxyURL)
	secure.Request.TLS.CAFile = targetCert
	response, err := FetchCollector(context.Background(), strings.Replace(target.URL, "127.0.0.1", "localhost", 1), secure, RequestOverrides{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	trip := response.Trip
	if trip.TLS == nil || *trip.TLS < slowHandshake {
		t.Errorf("an https target through an https:// proxy, whose handshake takes %s: tls %s", slowHandshake, durationOf(trip.TLS))
	}
	if trip.TLSConnection == nil || trip.TLSConnection.SubjectCN != "server" || !trip.TLSConnection.Verified || trip.TLSConnection.ServerName != "localhost" {
		t.Errorf("an https target through an https:// proxy: $tls %+v, want the target's verified connection", trip.TLSConnection)
	}
}
