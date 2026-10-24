//go:build !select_request_types || request_type_http || request_type_graphite

package fetch

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"log"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

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

// identityServer is an HTTPS server at localhost with a certificate of its
// own, whose file, ca, is the CA that verifies it, and which asks its clients
// for a certificate: one of clients, when any are given, and refuses a
// handshake without it; any or none otherwise. handle answers each request,
// told the common name of the certificate its connection presented, "" for
// none. Its URL is at localhost, the name its certificate is for.
func identityServer(t *testing.T, handle func(w http.ResponseWriter, r *http.Request, presented string), clients ...string) (serverURL, ca string) {
	t.Helper()
	certFile, keyFile := selfSigned(t, "server", "localhost")
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		t.Fatal(err)
	}
	config := &tls.Config{Certificates: []tls.Certificate{cert}, ClientAuth: tls.RequestClientCert, MinVersion: tls.VersionTLS12}
	if len(clients) > 0 {
		pool := x509.NewCertPool()
		for _, file := range clients {
			b, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			pool.AppendCertsFromPEM(b)
		}
		config.ClientAuth, config.ClientCAs = tls.RequireAndVerifyClientCert, pool
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		presented := ""
		if r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
			presented = r.TLS.PeerCertificates[0].Subject.CommonName
		}
		handle(w, r, presented)
	}))
	server.TLS = config
	// The server's own log would report the handshakes refused on purpose.
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.StartTLS()
	t.Cleanup(server.Close)
	return strings.Replace(server.URL, "127.0.0.1", "localhost", 1), certFile
}
