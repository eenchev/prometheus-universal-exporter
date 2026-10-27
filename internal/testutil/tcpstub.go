package testutil

import (
	"context"
	"crypto/tls"
	"encoding/pem"
	"io"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// TCPStub is a stand-in server of a text protocol over TCP, plain or TLS,
// for the tests of the tcp request type in internal/fetch and
// internal/exporter: each connection is handed to the test's answer, which
// reads what the client sent and writes back what a memcached, a ZooKeeper
// or a server that never answers would, and the connection is closed when
// answer returns. It imports nothing but the standard library.
type TCPStub struct {
	// Addr is the server's host:port, on 127.0.0.1.
	Addr string
	// CAFile is the file of the certificate a TLS stub, or a STARTTLS one
	// once it has begun TLS, presents, which verifies it for 127.0.0.1; ""
	// for a plain one.
	CAFile string

	listener net.Listener
	answer   func(conn net.Conn)
	mu       sync.Mutex
	conns    map[net.Conn]bool
	accepted int
	wg       sync.WaitGroup
}

// NewTCPStub starts a plain stub, which the test's cleanup stops.
func NewTCPStub(t testing.TB, answer func(conn net.Conn)) *TCPStub {
	t.Helper()
	var config net.ListenConfig
	listener, err := config.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return startTCPStub(t, listener, "", answer)
}

// NewTLSTCPStub starts a stub that speaks TLS with a certificate for
// 127.0.0.1, whose file is CAFile.
func NewTLSTCPStub(t testing.TB, answer func(conn net.Conn)) *TCPStub {
	t.Helper()
	config, caFile := stubTLS(t)
	var listen net.ListenConfig
	listener, err := listen.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return startTCPStub(t, tls.NewListener(listener, config), caFile, answer)
}

// NewStartTLSStub starts a plain stub whose answer may turn a connection to
// TLS, as a server speaking STARTTLS does once it has said to begin it:
// upgrade makes the server's side of the handshake, with the certificate a
// TLS stub presents, whose file is CAFile, and returns the TLS connection,
// which answer goes on with.
func NewStartTLSStub(t testing.TB, answer func(conn net.Conn, upgrade func() (net.Conn, error))) *TCPStub {
	t.Helper()
	config, caFile := stubTLS(t)
	var listen net.ListenConfig
	listener, err := listen.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return startTCPStub(t, listener, caFile, func(conn net.Conn) {
		answer(conn, func() (net.Conn, error) {
			server := tls.Server(conn, config)
			if err := server.HandshakeContext(context.Background()); err != nil {
				return nil, err
			}
			return server, nil
		})
	})
}

// stubTLS is the TLS configuration of a stub, with a certificate for
// 127.0.0.1 and example.com, and the file of that certificate.
func stubTLS(t testing.TB) (*tls.Config, string) {
	t.Helper()
	// httptest's own certificate, for 127.0.0.1 and example.com, which a
	// TLS server it starts presents and Certificate returns.
	issuer := httptest.NewUnstartedServer(nil)
	issuer.StartTLS()
	certificate, ca := issuer.TLS.Certificates[0], issuer.Certificate()
	issuer.Close()
	caFile := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	return &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12}, caFile
}

func startTCPStub(t testing.TB, listener net.Listener, caFile string, answer func(net.Conn)) *TCPStub {
	s := &TCPStub{Addr: listener.Addr().String(), CAFile: caFile, listener: listener, answer: answer, conns: map[net.Conn]bool{}}
	s.wg.Add(1)
	go s.serve()
	t.Cleanup(s.Close)
	return s
}

func (s *TCPStub) serve() {
	defer s.wg.Done()
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		s.conns[conn] = true
		s.accepted++
		s.mu.Unlock()
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer func() {
				_ = conn.Close()
				s.mu.Lock()
				delete(s.conns, conn)
				s.mu.Unlock()
			}()
			s.answer(conn)
		}()
	}
}

// Accepted is how many connections the stub has taken.
func (s *TCPStub) Accepted() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.accepted
}

// Close stops the stub, closes its connections and waits for their answers
// to end.
func (s *TCPStub) Close() {
	_ = s.listener.Close()
	s.mu.Lock()
	for conn := range s.conns {
		_ = conn.Close()
	}
	s.mu.Unlock()
	s.wg.Wait()
}

// ReadUntil reads from conn until what it read ends with suffix, and
// returns it, with what it read before an error. It reads a byte at a time,
// so that what follows suffix is left for the next read.
func ReadUntil(conn net.Conn, suffix string) (string, error) {
	var b strings.Builder
	one := make([]byte, 1)
	for !strings.HasSuffix(b.String(), suffix) {
		if _, err := io.ReadFull(conn, one); err != nil {
			return b.String(), err
		}
		b.Write(one)
	}
	return b.String(), nil
}
