//go:build !select_request_types || request_type_http

package fetch

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// answerPresented answers a request with the common name of the client
// certificate its connection presented.
func answerPresented(w http.ResponseWriter, _ *http.Request, presented string) {
	_, _ = io.WriteString(w, presented)
}

// fetchPresented fetches target with c as the static target whose own tls
// block is settings, nil for none, and returns the certificate the server
// was presented, or the error.
func fetchPresented(t *testing.T, target string, c *model.Collector, settings *model.TLSConfig) (string, error) {
	t.Helper()
	response, err := FetchCollector(context.Background(), target, c, TargetOverrides(&model.StaticTarget{Name: "x", Collector: c.Name, Target: target, Request: model.TargetRequestConfig{TLS: settings}}), nil)
	if err != nil {
		return "", err
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("%s answered %d", target, response.StatusCode)
	}
	return string(response.Body), nil
}

// A static target's own tls block replaces the collector's whole for an
// http collector: a server that takes the target's client certificate
// alone, and refuses the collector's, answers the target; the target's CA
// verifies its server, where the collector's CA, left out of the target's
// block, is not taken; and tls: {} verifies with the system roots alone, so
// a server only the collector's CA verifies is refused for it.
func TestAnHTTPStaticTargetsOwnTLSBlockIsTheOneItsRequestIsMadeWith(t *testing.T) {
	collectorCert, collectorKey := selfSigned(t, "collector-identity")
	targetCert, targetKey := selfSigned(t, "target-identity")
	server, ca := identityServer(t, answerPresented, targetCert)
	c := httpCollector(t, func(c *model.Collector) {
		c.Request.TLS = model.TLSConfig{CAFile: ca, CertFile: collectorCert, KeyFile: collectorKey}
	})
	if presented, err := fetchPresented(t, server+"/data", c, nil); err == nil {
		t.Fatalf("the collector's certificate was taken: %q", presented)
	}
	presented, err := fetchPresented(t, server+"/data", c, &model.TLSConfig{CAFile: ca, CertFile: targetCert, KeyFile: targetKey, Set: true})
	if err != nil || presented != "target-identity" {
		t.Fatalf("with the target's own block: %q, %v; want the target's certificate presented", presented, err)
	}
	if _, err := fetchPresented(t, server+"/data", c, &model.TLSConfig{CertFile: targetCert, KeyFile: targetKey, Set: true}); err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("a block without the CA verified the server with the collector's: %v", err)
	}
	if _, err := fetchPresented(t, server+"/data", c, &model.TLSConfig{Set: true}); err == nil {
		t.Fatal("tls: {} verified the server with the collector's CA, not the system roots")
	}

	// Two servers, each verified by a CA of its own: each target reaches
	// the server its CA verifies, and not the other.
	other, otherCA := identityServer(t, answerPresented)
	for _, tc := range []struct {
		server, ca string
		ok         bool
	}{{other, otherCA, true}, {server, otherCA, false}, {other, ca, false}} {
		_, err := fetchPresented(t, tc.server+"/data", c, &model.TLSConfig{CAFile: tc.ca, CertFile: targetCert, KeyFile: targetKey, Set: true})
		if (err == nil) != tc.ok {
			t.Errorf("%s with the CA %s: %v, want ok %v", tc.server, tc.ca, err, tc.ok)
		}
	}
}

// Connections are pooled by the tls block a request is made with
// (transport.go), so two static targets of one collector and one server,
// with client certificates of their own, never send a request on a
// connection the other's certificate made: each request is answered as
// its target's, and each target keeps its one connection.
func TestStaticTargetsWithTheirOwnCertificatesShareNoConnection(t *testing.T) {
	aCert, aKey := selfSigned(t, "tenant-a")
	bCert, bKey := selfSigned(t, "tenant-b")
	certFile, keyFile := selfSigned(t, "server", "localhost")
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		answerPresented(w, r, r.TLS.PeerCertificates[0].Subject.CommonName)
	}))
	listener := &countingListener{Listener: server.Listener}
	server.Listener = listener
	config, err := tlsConfig(model.TLSConfig{CertFile: certFile, KeyFile: keyFile})
	if err != nil {
		t.Fatal(err)
	}
	config.ClientAuth = tls.RequireAnyClientCert
	server.TLS = config
	server.StartTLS()
	t.Cleanup(server.Close)
	url := strings.Replace(server.URL, "127.0.0.1", "localhost", 1) + "/data"
	c := httpCollector(t, nil)
	blocks := map[string]*model.TLSConfig{
		"tenant-a": {CAFile: certFile, CertFile: aCert, KeyFile: aKey, Set: true},
		"tenant-b": {CAFile: certFile, CertFile: bCert, KeyFile: bKey, Set: true},
	}
	for range 3 {
		for _, name := range []string{"tenant-a", "tenant-b"} {
			presented, err := fetchPresented(t, url, c, blocks[name])
			if err != nil || presented != name {
				t.Fatalf("a request of %s: %q, %v; want it made on a connection of its own certificate", name, presented, err)
			}
		}
	}
	if n := listener.accepted.Load(); n != 2 {
		t.Errorf("six requests of two targets made %d connections, want one each", n)
	}
}

// A login is made with the tls block of the request that needs it, a
// static target's own, and its token is kept by that block too: two
// targets of one collector logging in at one URL, each with a client
// certificate of its own, make a login each, and neither is ever sent the
// token the other's certificate was given.
func TestStaticTargetsWithTheirOwnCertificatesShareNoLoginToken(t *testing.T) {
	freshLogins(t)
	aCert, aKey := selfSigned(t, "tenant-a")
	bCert, bKey := selfSigned(t, "tenant-b")
	var logins, refused atomic.Int64
	server, ca := identityServer(t, func(w http.ResponseWriter, r *http.Request, presented string) {
		if r.URL.Path == "/login" {
			logins.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "token-for-" + presented})
			return
		}
		if r.Header.Get("Authorization") != "Bearer token-for-"+presented {
			refused.Add(1)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, presented)
	})
	collector := testutil.Collector("api", "text")
	collector.Request.Login = &model.LoginConfig{Type: "json", URL: "/login", Token: ".access_token", FieldFiles: secretFiles(t, map[string]string{"password": "s3cret"})}
	collector.Request.TLS = model.TLSConfig{CAFile: ca}
	if err := ValidateRequest(&collector); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		for name, block := range map[string]*model.TLSConfig{
			"tenant-a": {CAFile: ca, CertFile: aCert, KeyFile: aKey, Set: true},
			"tenant-b": {CAFile: ca, CertFile: bCert, KeyFile: bKey, Set: true},
		} {
			if presented, err := fetchPresented(t, server+"/data", &collector, block); err != nil || presented != name {
				t.Fatalf("%s: %q, %v", name, presented, err)
			}
		}
	}
	if logins.Load() != 2 || refused.Load() != 0 {
		t.Errorf("%d logins and %d requests refused for its token, want a login of each target and none refused", logins.Load(), refused.Load())
	}
}

// The client certificate of a static target's own tls block is a
// credential as the collector's is (redirecttrust.go): a redirect over
// https to a host that is not trusted is refused before a connection is
// made there, naming the target's certificate, though the collector has
// none; without the target's block the same redirect is followed.
func TestAStaticTargetsClientCertificateIsNotPresentedToAnUntrustedHost(t *testing.T) {
	certFile, keyFile := selfSigned(t, "target-identity")
	other := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "elsewhere") }))
	connections := &countingListener{Listener: other.Listener}
	other.Listener = connections
	other.StartTLS()
	t.Cleanup(other.Close)
	origin, ca := identityServer(t, func(w http.ResponseWriter, r *http.Request, _ string) {
		http.Redirect(w, r, other.URL+"/end", http.StatusFound)
	})
	c := httpCollector(t, func(c *model.Collector) {
		c.Request.FollowRedirects = true
		c.Request.TLS = model.TLSConfig{InsecureSkipVerify: true}
	})
	_, err := fetchPresented(t, origin+"/start", c, &model.TLSConfig{CAFile: ca, CertFile: certFile, KeyFile: keyFile, InsecureSkipVerify: true, Set: true})
	want := "refused: it would present the static target's TLS client certificate to 127.0.0.1, which is not the origin the request was made to"
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("%v, want an error holding %q", err, want)
	}
	if n := connections.accepted.Load(); n != 0 {
		t.Fatalf("%d connections were made to the host that is not trusted", n)
	}
	if presented, err := fetchPresented(t, origin+"/start", c, nil); err != nil || presented != "elsewhere" {
		t.Fatalf("without a certificate: %q, %v; want the redirect followed", presented, err)
	}
}

// An http static target's own tls block is checked as the collector's is:
// a client certificate needs both files, server_name is a host's name, and
// no value takes a placeholder. Beside it the target's insecure_skip_verify,
// which would override the block's, is refused. A block on an http:// target
// is taken, as the collector's is, for the https requests of its trips.
func TestAnHTTPStaticTargetsTLSIsChecked(t *testing.T) {
	c := httpCollector(t, nil)
	skip := true
	ca := &model.TLSConfig{CAFile: "/ca.pem", Set: true}
	checkTargetRequests(t, c, map[string]struct {
		target  string
		request model.TargetRequestConfig
		want    string
	}{
		"a tls block":                   {"https://api.internal/x", model.TargetRequestConfig{TLS: ca}, ""},
		"an empty tls block":            {"https://api.internal/x", model.TargetRequestConfig{TLS: &model.TLSConfig{Set: true}}, ""},
		"a block on an http:// target":  {"http://api.internal/x", model.TargetRequestConfig{TLS: ca}, ""},
		"half a client certificate":     {"https://api.internal/x", model.TargetRequestConfig{TLS: &model.TLSConfig{KeyFile: "/k.pem", Set: true}}, "only one of cert_file and key_file"},
		"a server name that is no host": {"https://api.internal/x", model.TargetRequestConfig{TLS: &model.TLSConfig{ServerName: "a/b", Set: true}}, `request.tls.server_name "a/b"`},
		"a placeholder in the tls":      {"https://api.internal/x", model.TargetRequestConfig{TLS: &model.TLSConfig{CAFile: "{{param_ca}}", Set: true}}, "has a placeholder"},
		"insecure_skip_verify beside":   {"https://api.internal/x", model.TargetRequestConfig{TLS: ca, InsecureSkipVerify: &skip}, "beside a request.tls block of its own"},
		"insecure_skip_verify alone":    {"https://api.internal/x", model.TargetRequestConfig{InsecureSkipVerify: &skip}, ""},
		"a password":                    {"https://api.internal/x", model.TargetRequestConfig{PasswordFile: "/p"}, "does not apply"},
	})
}

// --dry-run reads an http static target's own tls files, as its first
// request would, under the target's name.
func TestAnHTTPStaticTargetsFilesAreReadByTheDryRun(t *testing.T) {
	checkTargetFilesAreRead(t, httpCollector(t, nil), false)
}
