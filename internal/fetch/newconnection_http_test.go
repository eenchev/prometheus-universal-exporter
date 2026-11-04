//go:build !select_request_types || request_type_http

package fetch

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// switchingCertificateServer is an https server on localhost that presents
// the certificate of first until switchTo is called, and second's after; it
// answers every request "ok", and counts the connections it accepted.
type switchingCertificateServer struct {
	*httptest.Server
	mu          sync.Mutex
	current     *tls.Certificate
	next        *tls.Certificate
	connections int
}

func newSwitchingCertificateServer(t *testing.T, h2 bool, first, second *tls.Certificate) *switchingCertificateServer {
	s := &switchingCertificateServer{current: first, next: second}
	s.Server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/moved" {
			http.Redirect(w, r, "/", http.StatusFound)
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	s.EnableHTTP2 = h2
	s.TLS = &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.connections++
		return s.current, nil
	}}
	s.StartTLS()
	t.Cleanup(s.Close)
	return s
}

func (s *switchingCertificateServer) switchCertificate() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.current = s.next
}

func (s *switchingCertificateServer) handshakes() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.connections
}

func (s *switchingCertificateServer) localhostURL() string {
	return strings.Replace(s.URL, "127.0.0.1", "localhost", 1)
}

// A site that starts serving a certificate the exporter does not trust is
// seen at the next probe with request.new_connection, over HTTP/1.1 and
// HTTP/2: every probe, and each redirect's request, makes its own connection
// and handshake, so the second probe fails verifying the new certificate,
// and the first's phases are all timed. Without it the second probe is sent
// on the connection the first left open, and answers with the certificate
// that connection was made with, as docs/REQUESTS.md's "Connections" says.
func TestANewConnectionSeesTheCertificateASiteServesNow(t *testing.T) {
	oldCert, oldKey := selfSigned(t, "old", "localhost")
	newCert, newKey := selfSigned(t, "new", "localhost")
	oldPair, err := tls.LoadX509KeyPair(oldCert, oldKey)
	if err != nil {
		t.Fatal(err)
	}
	newPair, err := tls.LoadX509KeyPair(newCert, newKey)
	if err != nil {
		t.Fatal(err)
	}
	for _, h2 := range []bool{false, true} {
		for _, fresh := range []bool{true, false} {
			server := newSwitchingCertificateServer(t, h2, &oldPair, &newPair)
			c := httpCollector(t, func(c *model.Collector) {
				c.Request.TLS.CAFile = oldCert
				c.Request.EnableHTTP2 = h2
				c.Request.FollowRedirects = true
				c.Request.NewConnection = fresh
			})
			// Through a redirect to the same site: with new_connection the
			// request it leads to has a connection of its own too.
			first, err := FetchCollector(context.Background(), server.localhostURL()+"/moved", c, RequestOverrides{}, nil)
			if err != nil {
				t.Fatalf("h2=%v new_connection=%v: %v", h2, fresh, err)
			}
			trip := first.Trip
			if trip.Redirects != 1 || trip.TLSConnection == nil || trip.TLSConnection.SubjectCN != "old" {
				t.Fatalf("h2=%v new_connection=%v: first probe %+v", h2, fresh, trip)
			}
			if fresh {
				if trip.DNS == nil || trip.Connect == nil || trip.TLS == nil {
					t.Errorf("h2=%v: the redirected request of a probe with new_connection had a kept connection: dns %s connect %s tls %s", h2, durationOf(trip.DNS), durationOf(trip.Connect), durationOf(trip.TLS))
				}
				if got := server.handshakes(); got != 2 {
					t.Errorf("h2=%v: a probe and its redirect made %d connections, want 2", h2, got)
				}
			}
			server.switchCertificate()
			second, err := FetchCollector(context.Background(), server.localhostURL(), c, RequestOverrides{}, nil)
			switch {
			case fresh && err == nil:
				t.Errorf("h2=%v: with new_connection, a site whose certificate no longer verifies answered with %q's", h2, second.Trip.TLSConnection.SubjectCN)
			case fresh && !strings.Contains(err.Error(), "certificate"):
				t.Errorf("h2=%v: with new_connection the second probe failed with %v, want the certificate's verification", h2, err)
			case !fresh && err != nil:
				t.Errorf("h2=%v: without new_connection the second probe made a connection of its own: %v", h2, err)
			case !fresh && (second.Trip.TLSConnection == nil || second.Trip.TLSConnection.SubjectCN != "old" || second.Trip.TLS != nil):
				t.Errorf("h2=%v: without new_connection the second probe was not sent on the kept connection: %+v", h2, second.Trip)
			}
		}
	}
}

// new_connection is a static target's key and a probe parameter
// (TestBooleanOverrideParsing), as enable_http2 is: either turns it on, or
// off, for its own requests.
func TestNewConnectionIsAStaticTargetsKeyAndAProbeParameter(t *testing.T) {
	on := true
	overrides := TargetOverrides(&model.StaticTarget{Request: model.TargetRequestConfig{NewConnection: &on}})
	if overrides.NewConnection == nil || !*overrides.NewConnection {
		t.Errorf("a static target's new_connection: %v", overrides.NewConnection)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) }))
	defer server.Close()
	c := httpCollector(t, nil)
	// The first probe connects, the second reuses its connection, and
	// each with new_connection makes one of its own.
	for i, probe := range []struct{ fresh, connects bool }{{false, true}, {false, false}, {true, true}, {true, true}} {
		fresh := probe.fresh
		response, err := FetchCollector(context.Background(), server.URL, c, RequestOverrides{NewConnection: &fresh}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if got := response.Trip.Connect != nil; got != probe.connects {
			t.Errorf("probe %d with new_connection=%v: connected %v, want %v", i, fresh, got, probe.connects)
		}
	}
}

// durationOf is d as a test's message writes it, null for none.
func durationOf(d *time.Duration) string {
	if d == nil {
		return "null"
	}
	return d.String()
}
