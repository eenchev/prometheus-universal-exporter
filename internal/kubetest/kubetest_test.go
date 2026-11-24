package kubetest

import (
	"bufio"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net/http"
	"net/url"
	"testing"
	"time"
)

// A held request is never answered: when its client gives up, the server
// closes the connection without a response. Go's transport takes a response
// that races the closing of the connection it gave up on, and an empty 200
// would be read as an empty page, ending a listing the test meant to run
// out of time. The client here gives up with a TLS close_notify and keeps
// reading, so a response written after it would be read.
func TestAHeldRequestIsNeverAnswered(t *testing.T) {
	s := New(t)
	s.HoldFrom(1)
	u, err := url.Parse(s.URL)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(s.Certificate())
	conn, err := tls.Dial("tcp", u.Host, &tls.Config{RootCAs: roots, ServerName: "example.com", NextProtos: []string{"http/1.1"}, MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := io.WriteString(conn, "GET /api/v1/pods HTTP/1.1\r\nHost: "+u.Host+"\r\nAuthorization: Bearer token-1\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	for len(s.Requests()) == 0 {
		time.Sleep(time.Millisecond)
	}
	if err := conn.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatalf("the held request was answered %s", resp.Status)
	}
	if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("reading the answer: %v, want the connection closed", err)
	}
}
