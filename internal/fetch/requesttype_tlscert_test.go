//go:build !select_request_types || request_type_tlscert

package fetch

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// testCert is a certificate made for a test, with its key.
type testCert struct {
	cert *x509.Certificate
	der  []byte
	key  *ecdsa.PrivateKey
}

// certSpec is what makeCert puts in a certificate.
type certSpec struct {
	cn                  string
	dns                 []string
	ips                 []string
	notBefore, notAfter time.Time
	ca                  bool
	client              bool
	serial              int64
	// key is the certificate's key, a new one when nil: a cross-signed
	// certificate shares its subject and key with the one it stands in for.
	key *ecdsa.PrivateKey
}

// makeCert makes a certificate of spec, signed by parent, or self-signed
// when parent is nil.
func makeCert(t *testing.T, spec certSpec, parent *testCert) *testCert {
	t.Helper()
	key := spec.key
	if key == nil {
		var err error
		if key, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader); err != nil {
			t.Fatal(err)
		}
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(spec.serial),
		Subject:      pkix.Name{CommonName: spec.cn, Organization: []string{"Test"}},
		DNSNames:     spec.dns,
		NotBefore:    spec.notBefore,
		NotAfter:     spec.notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if spec.client {
		template.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	}
	for _, ip := range spec.ips {
		template.IPAddresses = append(template.IPAddresses, netip.MustParseAddr(ip).AsSlice())
	}
	if spec.ca {
		template.IsCA, template.BasicConstraintsValid = true, true
		template.KeyUsage = x509.KeyUsageCertSign | x509.KeyUsageCRLSign
		template.ExtKeyUsage = nil
	}
	signer, signerKey := template, key
	if parent != nil {
		signer, signerKey = parent.cert, parent.key
	}
	der, err := x509.CreateCertificate(rand.Reader, template, signer, &key.PublicKey, signerKey)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &testCert{cert: cert, der: der, key: key}
}

// writePEM writes the certificate, and with key its key, to files in dir.
func (c *testCert) writePEM(t *testing.T, name string) (certFile, keyFile string) {
	t.Helper()
	dir := t.TempDir()
	certFile, keyFile = filepath.Join(dir, name+".pem"), filepath.Join(dir, name+"-key.pem")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.der}), 0o600); err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(c.key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certFile, keyFile
}

// tlsServer serves TLS handshakes on 127.0.0.1 presenting chain, the
// server's certificate first, and sends each client certificate's CN, ""
// for none, on the returned channel. With handshake false it accepts
// connections and never answers.
func tlsServer(t *testing.T, chain []*testCert, clientCAs *x509.CertPool, handshake bool) (string, <-chan string) {
	t.Helper()
	cert := tls.Certificate{PrivateKey: chain[0].key, Leaf: chain[0].cert}
	for _, c := range chain {
		cert.Certificate = append(cert.Certificate, c.der)
	}
	config := &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	if clientCAs != nil {
		config.ClientCAs, config.ClientAuth = clientCAs, tls.RequireAndVerifyClientCert
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	clients := make(chan string, 16)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			if !handshake {
				t.Cleanup(func() { _ = conn.Close() })
				continue
			}
			go func() {
				defer func() { _ = conn.Close() }()
				server := tls.Server(conn, config)
				_ = server.SetDeadline(time.Now().Add(5 * time.Second))
				if err := server.Handshake(); err != nil {
					return
				}
				cn := ""
				if peers := server.ConnectionState().PeerCertificates; len(peers) > 0 {
					cn = peers[0].Subject.CommonName
				}
				clients <- cn
				// Read until the client closes, so that the client's
				// certificate, sent after the server's Finished in TLS 1.3,
				// is checked before the connection goes.
				_, _ = server.Read(make([]byte, 1))
			}()
		}
	}()
	return ln.Addr().String(), clients
}

// tlsCertCollector is a validated tlscert collector with tls.
func tlsCertCollector(t *testing.T, settings model.TLSConfig, edit ...func(*model.Collector)) *model.Collector {
	t.Helper()
	c := &model.Collector{Name: "certs", Request: model.RequestConfig{Type: RequestTypeTLSCert, TLS: settings}}
	for _, e := range edit {
		e(c)
	}
	if err := ValidateRequest(c); err != nil {
		t.Fatal(err)
	}
	return c
}

// handshakeDoc is a scrape of target through c, and its document.
func handshakeDoc(t *testing.T, c *model.Collector, target string) (tlsCertDocument, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := FetchCollector(ctx, target, c, RequestOverrides{}, nil)
	if err != nil {
		return tlsCertDocument{}, err
	}
	var doc tlsCertDocument
	if err := json.Unmarshal(resp.Body, &doc); err != nil {
		t.Fatalf("%v\n%s", err, resp.Body)
	}
	return doc, nil
}

// testPKI is a root, an intermediate it signed, and the times certificates
// are measured from.
type testPKI struct {
	root, intermediate *testCert
	rootFile           string
	now                time.Time
}

func newTestPKI(t *testing.T) testPKI {
	t.Helper()
	now := time.Now().Truncate(time.Second)
	root := makeCert(t, certSpec{cn: "Test Root", notBefore: now.Add(-48 * time.Hour), notAfter: now.Add(10 * 365 * 24 * time.Hour), ca: true, serial: 1}, nil)
	intermediate := makeCert(t, certSpec{cn: "Test Intermediate", notBefore: now.Add(-24 * time.Hour), notAfter: now.Add(365 * 24 * time.Hour), ca: true, serial: 2}, root)
	rootFile, _ := root.writePEM(t, "root")
	return testPKI{root: root, intermediate: intermediate, rootFile: rootFile, now: now}
}

// The certificates the server presents are the document's, each with its
// subject, names, issuer, serial and validity in Unix seconds and RFC 3339,
// and its seconds until expiry; a chain of two that ca_file's root signed
// verifies against server_name; the version and cipher are the
// handshake's, and the leaf's expiry is repeated at the top, with the
// chain's earliest.
func TestATLSCertCollectorAnswersTheChainAsADocument(t *testing.T) {
	pki := newTestPKI(t)
	leaf := makeCert(t, certSpec{cn: "www.example.test", dns: []string{"www.example.test", "example.test"}, ips: []string{"127.0.0.1"}, notBefore: pki.now.Add(-time.Hour), notAfter: pki.now.Add(10 * 24 * time.Hour), serial: 0xbeef}, pki.intermediate)
	addr, _ := tlsServer(t, []*testCert{leaf, pki.intermediate}, nil, true)
	saved := tlsCertNow
	tlsCertNow = func() time.Time { return pki.now }
	t.Cleanup(func() { tlsCertNow = saved })

	doc, err := handshakeDoc(t, tlsCertCollector(t, model.TLSConfig{CAFile: pki.rootFile, ServerName: "www.example.test"}), addr)
	if err != nil {
		t.Fatal(err)
	}
	if !doc.Verified || doc.VerifyError != "" || doc.ServerName != "www.example.test" || doc.Host != "127.0.0.1" || doc.TLSVersion != "TLS 1.3" || doc.Cipher == "" || doc.DurationSeconds <= 0 {
		t.Errorf("document %+v", doc)
	}
	if len(doc.Certificates) != 2 {
		t.Fatalf("certificates %+v", doc.Certificates)
	}
	first, second := doc.Certificates[0], doc.Certificates[1]
	if first.SubjectCN != "www.example.test" || first.IssuerCN != "Test Intermediate" || first.Serial != "beef" || strings.Join(first.DNSNames, ",") != "www.example.test,example.test" || strings.Join(first.IPAddresses, ",") != "127.0.0.1" || first.IsCA || !strings.Contains(first.Subject, "CN=www.example.test") {
		t.Errorf("the server's certificate %+v", first)
	}
	want := pki.now.Add(10 * 24 * time.Hour)
	if first.NotAfter != want.Unix() || first.NotAfterRFC3339 != want.UTC().Format(time.RFC3339) || first.SecondsUntilExpiry != 10*24*3600 || first.NotBefore != pki.now.Add(-time.Hour).Unix() || len(first.SHA256Fingerprint) != 64 {
		t.Errorf("the server's certificate's validity %+v", first)
	}
	if second.SubjectCN != "Test Intermediate" || second.IssuerCN != "Test Root" || !second.IsCA || second.SecondsUntilExpiry != 365*24*3600 {
		t.Errorf("the intermediate %+v", second)
	}
	if doc.SecondsUntilExpiry != first.SecondsUntilExpiry || doc.NotAfter != first.NotAfter || doc.Chain == nil || doc.Chain.Length != 2 || doc.Chain.SecondsUntilEarliestExpiry != first.SecondsUntilExpiry {
		t.Errorf("document %+v, chain %+v", doc, doc.Chain)
	}
}

// A chain that does not verify is reported and does not fail the scrape:
// verified false, and why, for a certificate no root signed, one expired,
// one not yet valid, a name the certificate does not hold, and an
// intermediate left out; a name it holds among several verifies, and an
// address in its IP SANs, the target's host when server_name is unset.
func TestATLSCertChainThatDoesNotVerifyIsReportedNotRefused(t *testing.T) {
	pki := newTestPKI(t)
	hour := time.Hour
	for _, tc := range []struct {
		name      string
		spec      certSpec
		chain     func(leaf *testCert) []*testCert
		settings  model.TLSConfig
		verifyErr string
		expiry    int64
	}{
		{"untrusted", certSpec{cn: "self", dns: []string{"www.example.test"}, notBefore: pki.now.Add(-hour), notAfter: pki.now.Add(hour)}, nil, model.TLSConfig{ServerName: "www.example.test"}, "unknown authority", 3600},
		{"expired", certSpec{cn: "old", dns: []string{"www.example.test"}, notBefore: pki.now.Add(-48 * hour), notAfter: pki.now.Add(-24 * hour)}, nil, model.TLSConfig{CAFile: pki.rootFile, ServerName: "www.example.test"}, "expired", -24 * 3600},
		{"not yet valid", certSpec{cn: "new", dns: []string{"www.example.test"}, notBefore: pki.now.Add(24 * hour), notAfter: pki.now.Add(48 * hour)}, nil, model.TLSConfig{CAFile: pki.rootFile, ServerName: "www.example.test"}, "not yet valid", 48 * 3600},
		{"another name", certSpec{cn: "a", dns: []string{"a.example.test", "b.example.test"}, notBefore: pki.now.Add(-hour), notAfter: pki.now.Add(hour)}, nil, model.TLSConfig{CAFile: pki.rootFile, ServerName: "c.example.test"}, "not c.example.test", 3600},
		{"no intermediate", certSpec{cn: "a", dns: []string{"a.example.test"}, notBefore: pki.now.Add(-hour), notAfter: pki.now.Add(hour)}, func(leaf *testCert) []*testCert { return []*testCert{leaf} }, model.TLSConfig{CAFile: pki.rootFile, ServerName: "a.example.test"}, "unknown authority", 3600},
		{"one name of several", certSpec{cn: "a", dns: []string{"a.example.test", "b.example.test"}, notBefore: pki.now.Add(-hour), notAfter: pki.now.Add(hour)}, nil, model.TLSConfig{CAFile: pki.rootFile, ServerName: "b.example.test"}, "", 3600},
		{"an IP SAN", certSpec{cn: "ip", ips: []string{"127.0.0.1"}, notBefore: pki.now.Add(-hour), notAfter: pki.now.Add(hour)}, nil, model.TLSConfig{CAFile: pki.rootFile}, "", 3600},
	} {
		tc.spec.serial = 7
		var chain []*testCert
		if tc.name == "untrusted" {
			chain = []*testCert{makeCert(t, tc.spec, nil)}
		} else {
			leaf := makeCert(t, tc.spec, pki.intermediate)
			chain = []*testCert{leaf, pki.intermediate}
			if tc.chain != nil {
				chain = tc.chain(leaf)
			}
		}
		addr, _ := tlsServer(t, chain, nil, true)
		saved := tlsCertNow
		tlsCertNow = func() time.Time { return pki.now }
		doc, err := handshakeDoc(t, tlsCertCollector(t, tc.settings), addr)
		tlsCertNow = saved
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if doc.Verified != (tc.verifyErr == "") || !strings.Contains(doc.VerifyError, tc.verifyErr) || (tc.verifyErr == "") != (doc.VerifyError == "") {
			t.Errorf("%s: verified %v, %q; want the error %q", tc.name, doc.Verified, doc.VerifyError, tc.verifyErr)
		}
		if doc.SecondsUntilExpiry != tc.expiry {
			t.Errorf("%s: %d seconds until expiry, want %d", tc.name, doc.SecondsUntilExpiry, tc.expiry)
		}
	}
}

// The tls block's client certificate is presented to a server that asks
// for one.
func TestATLSCertCollectorPresentsItsClientCertificate(t *testing.T) {
	pki := newTestPKI(t)
	leaf := makeCert(t, certSpec{cn: "www.example.test", dns: []string{"www.example.test"}, notBefore: pki.now.Add(-time.Hour), notAfter: pki.now.Add(time.Hour), serial: 3}, pki.intermediate)
	client := makeCert(t, certSpec{cn: "exporter", notBefore: pki.now.Add(-time.Hour), notAfter: pki.now.Add(time.Hour), client: true, serial: 4}, pki.root)
	certFile, keyFile := client.writePEM(t, "client")
	roots := x509.NewCertPool()
	roots.AddCert(pki.root.cert)
	addr, clients := tlsServer(t, []*testCert{leaf, pki.intermediate}, roots, true)
	doc, err := handshakeDoc(t, tlsCertCollector(t, model.TLSConfig{CAFile: pki.rootFile, ServerName: "www.example.test", CertFile: certFile, KeyFile: keyFile}), addr)
	if err != nil || !doc.Verified {
		t.Fatalf("%+v, %v", doc, err)
	}
	select {
	case cn := <-clients:
		if cn != "exporter" {
			t.Errorf("the server was presented %q", cn)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the server saw no handshake")
	}
}

// A handshake that does not happen fails the scrape: nothing listening, a
// server that never answers within the deadline, and one that speaks no TLS.
func TestATLSCertHandshakeThatFailsFailsTheScrape(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := ln.Addr().String()
	_ = ln.Close()
	c := tlsCertCollector(t, model.TLSConfig{})
	if _, err := handshakeDoc(t, c, closed); err == nil || !strings.Contains(err.Error(), "connecting to "+closed) {
		t.Errorf("nothing listening: %v", err)
	}
	pki := newTestPKI(t)
	silent, _ := tlsServer(t, []*testCert{pki.intermediate}, nil, false)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, err := FetchCollector(ctx, silent, c, RequestOverrides{}, nil); !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "TLS handshake with "+silent) {
		t.Errorf("a silent server: %v", err)
	}
	plain, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = plain.Close() })
	go func() {
		for {
			conn, err := plain.Accept()
			if err != nil {
				return
			}
			_, _ = conn.Write([]byte("220 smtp.example.test ESMTP\r\n"))
			_ = conn.Close()
		}
	}()
	if _, err := handshakeDoc(t, c, plain.Addr().String()); err == nil || !strings.Contains(err.Error(), "TLS handshake") {
		t.Errorf("a server that speaks no TLS: %v", err)
	}
}

// A tlscert collector's tls block is held to its rules at load:
// insecure_skip_verify, which it has no use for, and half a client
// certificate are refused, and so is a key of another type; a target is
// host or host:port, port 443 when left out, and anything else is refused
// before anything is dialed. The url label is tls://host:port and the
// http_method label HANDSHAKE.
func TestATLSCertCollectorsKeysAndTargetsAreChecked(t *testing.T) {
	for name, settings := range map[string]model.TLSConfig{
		"insecure_skip_verify": {InsecureSkipVerify: true},
		"a lone cert_file":     {CertFile: "/c"},
		"a lone key_file":      {KeyFile: "/k"},
		"a bad server_name":    {ServerName: "a b"},
	} {
		c := &model.Collector{Name: "certs", Request: model.RequestConfig{Type: RequestTypeTLSCert, TLS: settings}}
		if err := ValidateRequest(c); err == nil {
			t.Errorf("%s: taken", name)
		}
	}
	if err := ValidateRequest(&model.Collector{Name: "certs", Request: model.RequestConfig{Type: RequestTypeTLSCert, Path: "/x"}}); err == nil {
		t.Error("request.path taken")
	}
	c := tlsCertCollector(t, model.TLSConfig{})
	for _, target := range []string{"https://example.test", "example.test/x", "example.test:0", "example.test:https", "user@example.test:443", "2001:db8::1:443:x:", ":443", "a b:443"} {
		if err := CheckTarget(c, target, false); err == nil {
			t.Errorf("%q: taken", target)
		}
	}
	for target, label := range map[string]string{"example.test": "tls://example.test:443", "Example.Test:8443": "tls://example.test:8443", "[2001:db8::1]:443": "tls://[2001:db8::1]:443", "192.0.2.1": "tls://192.0.2.1:443"} {
		if err := CheckTarget(c, target, false); err != nil {
			t.Errorf("%q: %v", target, err)
		}
		if got, err := RequestLabelFor(target, c, RequestOverrides{}); err != nil || got != label {
			t.Errorf("%q: label %q, %v", target, got, err)
		}
	}
	if method := RequestMethodFor(c, RequestOverrides{}); method != "HANDSHAKE" {
		t.Errorf("method %q", method)
	}
	if stage := FetchStage(c); stage != "tls" {
		t.Errorf("stage %q", stage)
	}
	if shown := DisplayTarget(c, "https://user:secret@example.test"); shown != "<invalid target>" {
		t.Errorf("shown %q", shown)
	}
}

// allowed_targets and denied_targets hold the handshake to them as an http
// request's connection: a denied address, or one allowed_targets does not
// list, is refused before anything is sent.
func TestATLSCertCollectorKeepsToItsTargetLists(t *testing.T) {
	pki := newTestPKI(t)
	leaf := makeCert(t, certSpec{cn: "ip", ips: []string{"127.0.0.1"}, notBefore: pki.now.Add(-time.Hour), notAfter: pki.now.Add(time.Hour), serial: 5}, pki.intermediate)
	addr, clients := tlsServer(t, []*testCert{leaf, pki.intermediate}, nil, true)
	for name, edit := range map[string]func(*model.Collector){
		"denied":      func(c *model.Collector) { c.Request.DeniedTargets = []string{"127.0.0.0/8"} },
		"not allowed": func(c *model.Collector) { c.Request.AllowedTargets = []string{"10.0.0.0/8"} },
	} {
		if _, err := handshakeDoc(t, tlsCertCollector(t, model.TLSConfig{}, edit), addr); !errors.Is(err, ErrTargetRefused) {
			t.Errorf("%s: %v", name, err)
		}
	}
	select {
	case cn := <-clients:
		t.Errorf("a refused target was handshaken with (%q)", cn)
	default:
	}
	if _, err := handshakeDoc(t, tlsCertCollector(t, model.TLSConfig{}, func(c *model.Collector) { c.Request.AllowedTargets = []string{"127.0.0.0/8"} }), addr); err != nil {
		t.Errorf("allowed: %v", err)
	}
}

// A debug probe's request section shows the handshake: tls://host:port,
// the name sent as SNI, whether a client certificate is presented, and how
// it ended. --dry-run reads the tls block's files as a handshake would.
func TestATLSCertHandshakeIsTracedAndItsFilesChecked(t *testing.T) {
	pki := newTestPKI(t)
	leaf := makeCert(t, certSpec{cn: "www", dns: []string{"www.example.test"}, notBefore: pki.now.Add(-time.Hour), notAfter: pki.now.Add(time.Hour), serial: 6}, pki.intermediate)
	addr, _ := tlsServer(t, []*testCert{leaf, pki.intermediate}, nil, true)
	ctx, trace := WithRequestTrace(context.Background())
	c := tlsCertCollector(t, model.TLSConfig{CAFile: pki.rootFile, ServerName: "www.example.test"})
	if _, err := FetchCollector(ctx, addr, c, RequestOverrides{}, nil); err != nil {
		t.Fatal(err)
	}
	requests := trace.Requests()
	if len(requests) != 1 {
		t.Fatalf("traced %+v", requests)
	}
	r := requests[0]
	if r.Method != "HANDSHAKE" || r.URL != "tls://"+addr || r.Header.Get("Tls-Server-Name") != "www.example.test" || r.Header.Get("Tls-Client-Certificate") != "no" || r.Outcome != "TLS 1.3, 2 certificates, verified" {
		t.Errorf("traced %+v", r)
	}
	if warnings := FileWarnings(c, nil); len(warnings) != 0 {
		t.Errorf("warnings %v", warnings)
	}
	missing := tlsCertCollector(t, model.TLSConfig{CAFile: filepath.Join(t.TempDir(), "missing.pem")})
	if warnings := FileWarnings(missing, nil); len(warnings) != 1 || !strings.Contains(warnings[0], "request.tls") {
		t.Errorf("warnings %v", warnings)
	}
}

// writeRoots writes certs to one PEM file, for ca_file.
func writeRoots(t *testing.T, certs ...*testCert) string {
	t.Helper()
	var data []byte
	for _, c := range certs {
		data = append(data, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.der})...)
	}
	file := filepath.Join(t.TempDir(), "roots.pem")
	if err := os.WriteFile(file, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return file
}

// The chain's earliest expiry is of the chain a client verifies by, root
// included, when the presented chain verifies, so that a server still
// sending an expired cross-sign of its root, as Let's Encrypt's servers
// sent DST Root CA X3, does not hold it in the past; when the chain does
// not verify, it is of the presented certificates, and earliest_from says
// which. The certificates are the presented ones either way.
func TestATLSCertChainsEarliestExpiryIsOfTheVerifiedChain(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	day := 24 * time.Hour
	oldRoot := makeCert(t, certSpec{cn: "Old Root", notBefore: now.Add(-20 * 365 * day), notAfter: now.Add(-400 * day), ca: true, serial: 1}, nil)
	// The root expires before the leaf, so the verified chain ends with it.
	root := makeCert(t, certSpec{cn: "Test Root", notBefore: now.Add(-day), notAfter: now.Add(30 * day), ca: true, serial: 2}, nil)
	crossSign := makeCert(t, certSpec{cn: "Test Root", notBefore: now.Add(-5 * 365 * day), notAfter: now.Add(-365 * day), ca: true, serial: 3, key: root.key}, oldRoot)
	intermediate := makeCert(t, certSpec{cn: "Test Intermediate", notBefore: now.Add(-day), notAfter: now.Add(365 * day), ca: true, serial: 4}, root)
	leaf := makeCert(t, certSpec{cn: "www", dns: []string{"www.example.test"}, notBefore: now.Add(-day), notAfter: now.Add(90 * day), serial: 5}, intermediate)
	addr, _ := tlsServer(t, []*testCert{leaf, intermediate, crossSign}, nil, true)
	saved := tlsCertNow
	tlsCertNow = func() time.Time { return now }
	t.Cleanup(func() { tlsCertNow = saved })

	for _, tc := range []struct {
		name     string
		settings model.TLSConfig
		verified bool
		earliest time.Time
		from     string
	}{
		{"trusted", model.TLSConfig{CAFile: writeRoots(t, root), ServerName: "www.example.test"}, true, root.cert.NotAfter, "verified"},
		{"untrusted", model.TLSConfig{ServerName: "www.example.test"}, false, crossSign.cert.NotAfter, "presented"},
	} {
		doc, err := handshakeDoc(t, tlsCertCollector(t, tc.settings), addr)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if doc.Verified != tc.verified || doc.Chain == nil || doc.Chain.Length != 3 || len(doc.Certificates) != 3 || doc.Certificates[2].SubjectCN != "Test Root" || doc.Certificates[2].SecondsUntilExpiry != -365*24*3600 {
			t.Errorf("%s: verified %v, chain %+v, certificates %+v", tc.name, doc.Verified, doc.Chain, doc.Certificates)
			continue
		}
		if doc.Chain.EarliestNotAfter != tc.earliest.Unix() || doc.Chain.SecondsUntilEarliestExpiry != tc.earliest.Unix()-now.Unix() || doc.Chain.EarliestFrom != tc.from {
			t.Errorf("%s: chain %+v, want the earliest %d from %q", tc.name, doc.Chain, tc.earliest.Unix(), tc.from)
		}
	}
}

// A chain that verifies by two chains ends when the later of them does,
// each at the first of its certificates to expire: here the intermediate
// cross-signed by the second root, after the first root's intermediate.
func TestATLSCertChainVerifiedByTwoChainsEndsWithTheLaterOne(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	day := 24 * time.Hour
	first := makeCert(t, certSpec{cn: "First Root", notBefore: now.Add(-day), notAfter: now.Add(10 * 365 * day), ca: true, serial: 1}, nil)
	second := makeCert(t, certSpec{cn: "Second Root", notBefore: now.Add(-day), notAfter: now.Add(10 * 365 * day), ca: true, serial: 2}, nil)
	byFirst := makeCert(t, certSpec{cn: "Test Intermediate", notBefore: now.Add(-day), notAfter: now.Add(30 * day), ca: true, serial: 3}, first)
	bySecond := makeCert(t, certSpec{cn: "Test Intermediate", notBefore: now.Add(-day), notAfter: now.Add(60 * day), ca: true, serial: 4, key: byFirst.key}, second)
	leaf := makeCert(t, certSpec{cn: "www", dns: []string{"www.example.test"}, notBefore: now.Add(-day), notAfter: now.Add(90 * day), serial: 5}, byFirst)
	addr, _ := tlsServer(t, []*testCert{leaf, byFirst, bySecond}, nil, true)
	saved := tlsCertNow
	tlsCertNow = func() time.Time { return now }
	t.Cleanup(func() { tlsCertNow = saved })

	doc, err := handshakeDoc(t, tlsCertCollector(t, model.TLSConfig{CAFile: writeRoots(t, first, second), ServerName: "www.example.test"}), addr)
	if err != nil {
		t.Fatal(err)
	}
	if !doc.Verified || doc.Chain == nil || doc.Chain.EarliestNotAfter != bySecond.cert.NotAfter.Unix() || doc.Chain.SecondsUntilEarliestExpiry != 60*24*3600 || doc.Chain.EarliestFrom != "verified" {
		t.Errorf("verified %v %q, chain %+v; want the earliest %d", doc.Verified, doc.VerifyError, doc.Chain, bySecond.cert.NotAfter.Unix())
	}
	// Both chains verify, so the rule had two to choose from.
	chains, err := verifyTLSCertChain([]*x509.Certificate{leaf.cert, byFirst.cert, bySecond.cert}, certPoolOf(first.cert, second.cert), "www.example.test", now)
	if err != nil || len(chains) != 2 {
		t.Errorf("%d chains, %v", len(chains), err)
	}
}

// certPoolOf is a pool of certs.
func certPoolOf(certs ...*x509.Certificate) *x509.CertPool {
	pool := x509.NewCertPool()
	for _, c := range certs {
		pool.AddCert(c)
	}
	return pool
}
