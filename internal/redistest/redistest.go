// Package redistest is a stand-in Redis server for the tests of the redis
// request type: a TCP server, or a TLS one with a self-signed certificate
// whose file it writes, that answers RESP2 commands as Redis and Valkey do.
// It answers AUTH, with a username or without, against the password it was
// given, NOAUTH to anything else before it, SELECT of the databases 0 to
// 15, INFO of the sections it was given, all of them or one by name, and
// any other command with the reply the test set for it, or the error of an
// unknown command; a test may answer a command itself, with any bytes it
// likes, to play a broken or hostile server. It records every command it
// was sent. It imports nothing but the standard library.
package redistest

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Server is a stand-in Redis server.
type Server struct {
	// Addr is the server's host:port, on 127.0.0.1.
	Addr string
	// CAFile is the file of the certificate a TLS server presents, which
	// verifies it for 127.0.0.1 and localhost; "" for a plain server.
	CAFile string

	listener net.Listener
	mu       sync.Mutex
	username string
	password string
	sections []Section
	replies  map[string]string
	answer   func(args []string) (string, bool)
	commands [][]string
	conns    map[net.Conn]bool
	wg       sync.WaitGroup
}

// Section is a section of INFO: its name as INFO's heading writes it, such
// as Server, and its name:value lines.
type Section struct {
	Name  string
	Lines []string
}

// New starts a plain server, which the test's cleanup stops.
func New(t testing.TB) *Server {
	t.Helper()
	listener, err := listen()
	if err != nil {
		t.Fatal(err)
	}
	return start(t, listener, "")
}

// NewTLS starts a server that speaks TLS with a certificate for 127.0.0.1
// and localhost, whose file is CAFile.
func NewTLS(t testing.TB) *Server {
	t.Helper()
	certificate, caFile := selfSigned(t)
	listener, err := listen()
	if err != nil {
		t.Fatal(err)
	}
	return start(t, tls.NewListener(listener, &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12}), caFile)
}

// listen listens on a port of 127.0.0.1 the system chooses.
func listen() (net.Listener, error) {
	var config net.ListenConfig
	return config.Listen(context.Background(), "tcp", "127.0.0.1:0")
}

func start(t testing.TB, listener net.Listener, caFile string) *Server {
	s := &Server{Addr: listener.Addr().String(), CAFile: caFile, listener: listener, replies: map[string]string{}, conns: map[net.Conn]bool{}}
	s.wg.Add(1)
	go s.serve()
	t.Cleanup(s.Close)
	return s
}

// Close stops the server, closes its connections and waits for them to
// end.
func (s *Server) Close() {
	_ = s.listener.Close()
	s.mu.Lock()
	for conn := range s.conns {
		_ = conn.Close()
	}
	s.mu.Unlock()
	s.wg.Wait()
}

// RequirePassword makes the server take commands only after AUTH with
// password, as the user username, or as the default user when it is "".
func (s *Server) RequirePassword(username, password string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.username, s.password = username, password
}

// SetInfo sets the sections INFO answers, in order.
func (s *Server) SetInfo(sections ...Section) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sections = sections
}

// Reply sets the raw RESP2 reply to a command, written as its words joined
// by blanks, such as "LLEN jobs" answered ":42\r\n".
func (s *Server) Reply(command, raw string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.replies[command] = raw
}

// Answer has answer reply to every command it says it answers, with any
// bytes, before the server looks at it; it may block, to play a server
// that never answers, until the connection is closed.
func (s *Server) Answer(answer func(args []string) (raw string, ok bool)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.answer = answer
}

// Commands returns every command the server was sent, in order.
func (s *Server) Commands() [][]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([][]string, len(s.commands))
	for i, c := range s.commands {
		out[i] = append([]string(nil), c...)
	}
	return out
}

func (s *Server) serve() {
	defer s.wg.Done()
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		s.conns[conn] = true
		s.mu.Unlock()
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.handle(conn)
			s.mu.Lock()
			delete(s.conns, conn)
			s.mu.Unlock()
		}()
	}
}

func (s *Server) handle(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	r := bufio.NewReader(conn)
	authed := false
	for {
		args, err := readCommand(r)
		if err != nil {
			return
		}
		s.mu.Lock()
		s.commands = append(s.commands, args)
		answer := s.answer
		s.mu.Unlock()
		if answer != nil {
			if raw, ok := answer(args); ok {
				if _, err := io.WriteString(conn, raw); err != nil {
					return
				}
				continue
			}
		}
		reply := s.reply(args, &authed)
		if _, err := io.WriteString(conn, reply); err != nil {
			return
		}
	}
}

// reply is the server's own reply to a command.
func (s *Server) reply(args []string, authed *bool) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	name := strings.ToUpper(args[0])
	if name == "AUTH" {
		switch {
		case s.password == "":
			return "-ERR AUTH <password> called without any password configured for the default user. Are you sure your configuration is correct?\r\n"
		case len(args) == 2 && s.username == "" && args[1] == s.password,
			len(args) == 3 && s.username != "" && args[1] == s.username && args[2] == s.password:
			*authed = true
			return "+OK\r\n"
		}
		return "-WRONGPASS invalid username-password pair or user is disabled.\r\n"
	}
	if s.password != "" && !*authed {
		return "-NOAUTH Authentication required.\r\n"
	}
	switch name {
	case "SELECT":
		if n, err := strconv.Atoi(args[len(args)-1]); len(args) == 2 && err == nil && n >= 0 && n < 16 {
			return "+OK\r\n"
		}
		return "-ERR DB index is out of range\r\n"
	case "INFO":
		var b strings.Builder
		for _, section := range s.sections {
			if len(args) > 1 && !strings.EqualFold(args[1], section.Name) {
				continue
			}
			if b.Len() > 0 {
				b.WriteString("\r\n")
			}
			fmt.Fprintf(&b, "# %s\r\n", section.Name)
			for _, line := range section.Lines {
				fmt.Fprintf(&b, "%s\r\n", line)
			}
		}
		return Bulk(b.String())
	}
	if raw, ok := s.replies[strings.Join(args, " ")]; ok {
		return raw
	}
	return fmt.Sprintf("-ERR unknown command '%s', with args beginning with: \r\n", args[0])
}

// Bulk is text as a bulk string reply.
func Bulk(text string) string {
	return "$" + strconv.Itoa(len(text)) + "\r\n" + text + "\r\n"
}

// readCommand reads a command as a client sends it, an array of bulk
// strings.
func readCommand(r *bufio.Reader) ([]string, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(line, "*") {
		return nil, fmt.Errorf("a command begins with %q", line)
	}
	n, err := strconv.Atoi(strings.TrimRight(line[1:], "\r\n"))
	if err != nil || n < 1 || n > 1024 {
		return nil, fmt.Errorf("a command of %q words", line)
	}
	args := make([]string, n)
	for i := range args {
		header, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		size, err := strconv.Atoi(strings.TrimRight(strings.TrimPrefix(header, "$"), "\r\n"))
		if err != nil || size < 0 || size > 1<<20 {
			return nil, fmt.Errorf("a word of %q bytes", header)
		}
		data := make([]byte, size+2)
		if _, err := io.ReadFull(r, data); err != nil {
			return nil, err
		}
		args[i] = string(data[:size])
	}
	return args, nil
}

// selfSigned makes a certificate for 127.0.0.1 and localhost, and writes it
// to a file of the test's own directory.
func selfSigned(t testing.TB) (tls.Certificate, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "redistest"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1)},
		DNSNames:              []string{"localhost"},
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	caFile := filepath.Join(t.TempDir(), "redis-ca.pem")
	if err := os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, caFile
}
