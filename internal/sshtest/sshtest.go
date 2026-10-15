//go:build !select_request_types || request_type_sshfile

// Package sshtest is an in-process SSH server with an SFTP subsystem, for the
// sshfile request type's tests: it serves the exporter's own filesystem, so
// a test writes files to a temporary directory and reads them over SFTP by
// their absolute paths. It authenticates by public key and by password,
// presents host keys the test chooses, and can stop answering, as a host
// that lost power does, while its connections stay open.
//
// Only tests import it, and it carries the type's build constraint, so a
// build without the type compiles none of what it imports.
package sshtest

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha512"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pkg/sftp"
	// bcrypt_pbkdf, which OpenSSH encrypts a private key with, is made of
	// Blowfish; nothing new is encrypted with it.
	"golang.org/x/crypto/blowfish" //nolint:staticcheck // see above
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// Options are what a server accepts and presents.
type Options struct {
	// User is the account it accepts, "metrics" when empty.
	User string
	// Password is the password it accepts; empty, it accepts none.
	Password string
	// AuthorizedKeys are the public keys it accepts.
	AuthorizedKeys []ssh.PublicKey
	// HostKeys are the host keys it presents, a new ed25519 key when empty.
	HostKeys []ssh.Signer
	// KeyboardInteractive makes it take Password only through
	// keyboard-interactive, as an OpenSSH that leaves passwords to PAM
	// does: it asks for the account, echoed, and for the password, not.
	KeyboardInteractive bool
	// Configure, when set, changes the server's configuration last, such
	// as its password and keyboard-interactive callbacks.
	Configure func(*ssh.ServerConfig)
	// MaxSessions, when above 0, is how many sessions one connection may
	// have open at once, as OpenSSH's MaxSessions: one more is refused.
	MaxSessions int
	// SessionCloseDelay, when above 0, is how long the server waits after
	// an SFTP session's client side ended before it frees the session and
	// closes its own side, as a busy host may: a client that opens another
	// session before it has seen the close is refused one at MaxSessions.
	SessionCloseDelay time.Duration
	// SFTP, when set, is offered each SFTP session the server starts,
	// numbered from 1 across its connections, before the server serves
	// files on it; it returns true when it served the session itself,
	// which the server then closes.
	SFTP func(n int, channel io.ReadWriter) bool
}

// Server is a running server.
type Server struct {
	// Addr is 127.0.0.1 and its port.
	Addr     string
	HostKeys []ssh.Signer
	listener net.Listener
	// accepted counts the connections it took, and open those not ended.
	accepted, open atomic.Int64
	// sessions counts the sessions open, and sftpSessions the SFTP
	// sessions started.
	sessions, sftpSessions atomic.Int64
	// readDirs counts the READDIR requests the server answered with
	// entries, and listed the entries of those answers.
	readDirs, listed atomic.Int64
	sftp             func(n int, channel io.ReadWriter) bool
	maxSessions      int
	closeDelay       time.Duration
	gate             *gate
	mu               sync.Mutex
	conns            []net.Conn
}

// Start starts a server that serves until the test ends.
func Start(t testing.TB, options Options) *Server {
	t.Helper()
	if options.User == "" {
		options.User = "metrics"
	}
	if len(options.HostKeys) == 0 {
		options.HostKeys = []ssh.Signer{NewEd25519Key(t)}
	}
	config := &ssh.ServerConfig{
		PublicKeyCallback: func(meta ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			for _, allowed := range options.AuthorizedKeys {
				if meta.User() == options.User && string(allowed.Marshal()) == string(key.Marshal()) {
					return &ssh.Permissions{}, nil
				}
			}
			return nil, errors.New("key not authorized")
		},
	}
	if options.Password != "" && options.KeyboardInteractive {
		config.KeyboardInteractiveCallback = func(meta ssh.ConnMetadata, challenge ssh.KeyboardInteractiveChallenge) (*ssh.Permissions, error) {
			answers, err := challenge("", "", []string{"Login: ", "Password: "}, []bool{true, false})
			if err != nil {
				return nil, err
			}
			if meta.User() == options.User && slices.Equal(answers, []string{options.User, options.Password}) {
				return &ssh.Permissions{}, nil
			}
			return nil, errors.New("wrong answers")
		}
	} else if options.Password != "" {
		config.PasswordCallback = func(meta ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {
			if meta.User() == options.User && string(password) == options.Password {
				return &ssh.Permissions{}, nil
			}
			return nil, errors.New("wrong password")
		}
	}
	for _, key := range options.HostKeys {
		config.AddHostKey(key)
	}
	if options.Configure != nil {
		options.Configure(config)
	}
	listener, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{Addr: listener.Addr().String(), HostKeys: options.HostKeys, listener: listener, gate: newGate(), sftp: options.SFTP, maxSessions: options.MaxSessions, closeDelay: options.SessionCloseDelay}
	go s.serve(config)
	t.Cleanup(s.Close)
	return s
}

// Close stops the server and ends every connection it took.
func (s *Server) Close() {
	_ = s.listener.Close()
	s.gate.open()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, conn := range s.conns {
		_ = conn.Close()
	}
}

// DropConnections ends every connection the server has taken, as a host
// that restarts its SSH server does, and goes on taking new ones.
func (s *Server) DropConnections() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, conn := range s.conns {
		_ = conn.Close()
	}
	s.conns = nil
}

// Accepted is how many connections the server has taken.
func (s *Server) Accepted() int { return int(s.accepted.Load()) }

// Open is how many of the connections it took have not ended.
func (s *Server) Open() int { return int(s.open.Load()) }

// Sessions is how many sessions are open on the server's connections.
func (s *Server) Sessions() int { return int(s.sessions.Load()) }

// ReadDirs is how many READDIR requests of SFTP the server answered with
// directory entries, and Listed how many entries those answers held, "."
// and ".." among them.
func (s *Server) ReadDirs() int { return int(s.readDirs.Load()) }

// Listed is how many directory entries the server sent (ReadDirs).
func (s *Server) Listed() int { return int(s.listed.Load()) }

// Freeze makes the server stop answering: it reads and writes nothing on
// its connections, which stay open, until Thaw.
func (s *Server) Freeze() { s.gate.shut() }

// Thaw makes a frozen server answer again.
func (s *Server) Thaw() { s.gate.open() }

func (s *Server) serve(config *ssh.ServerConfig) {
	for {
		raw, err := s.listener.Accept()
		if err != nil {
			return
		}
		s.accepted.Add(1)
		s.open.Add(1)
		conn := &gatedConn{Conn: raw, gate: s.gate, closed: make(chan struct{})}
		s.mu.Lock()
		s.conns = append(s.conns, conn)
		s.mu.Unlock()
		go s.handle(conn, config)
	}
}

func (s *Server) handle(conn net.Conn, config *ssh.ServerConfig) {
	defer s.open.Add(-1)
	server, channels, requests, err := ssh.NewServerConn(conn, config)
	if err != nil {
		_ = conn.Close()
		return
	}
	// The connection has ended when the client closes it or it breaks;
	// closing it here too ends the channels of a client that went away.
	defer func() { _ = server.Close() }()
	// A keepalive is answered, as OpenSSH answers one it does not know.
	go ssh.DiscardRequests(requests)
	var open atomic.Int64
	for request := range channels {
		if request.ChannelType() != "session" {
			_ = request.Reject(ssh.UnknownChannelType, "only sessions")
			continue
		}
		if s.maxSessions > 0 && open.Load() >= int64(s.maxSessions) {
			_ = request.Reject(ssh.Prohibited, "open failed")
			continue
		}
		channel, sessionRequests, err := request.Accept()
		if err != nil {
			continue
		}
		open.Add(1)
		s.sessions.Add(1)
		// The session stops counting once, before the server closes its
		// side, as OpenSSH frees a session before it confirms the close:
		// a client that has seen the close may open another at once.
		var release sync.Once
		done := func() {
			release.Do(func() {
				open.Add(-1)
				s.sessions.Add(-1)
			})
		}
		go func() {
			// The requests end when the client closes the session; an SFTP
			// session is freed by serveSFTP as it closes the server's side,
			// any other here.
			served := false
			defer func() {
				if !served {
					done()
				}
			}()
			for r := range sessionRequests {
				ok := r.Type == "subsystem" && len(r.Payload) > 4 && string(r.Payload[4:]) == "sftp"
				_ = r.Reply(ok, nil)
				if ok && !served {
					served = true
					go s.serveSFTP(int(s.sftpSessions.Add(1)), channel, done)
				}
			}
		}()
	}
}

// serveSFTP serves the server's files on an SFTP session, unless
// Options.SFTP serves it, counting the directory entries it lists.
func (s *Server) serveSFTP(n int, channel ssh.Channel, released func()) {
	defer func() {
		if s.closeDelay > 0 {
			time.Sleep(s.closeDelay)
		}
		released()
		_ = channel.Close()
	}()
	if s.sftp != nil && s.sftp(n, channel) {
		return
	}
	server, err := sftp.NewServer(&countingChannel{Channel: channel, server: s, readDirs: map[uint32]bool{}})
	if err != nil {
		return
	}
	_ = server.Serve()
}

// countingChannel is an SFTP session that counts the entries its server
// sends in answer to READDIR requests, from the packets read and written
// on it.
type countingChannel struct {
	ssh.Channel
	server   *Server
	mu       sync.Mutex
	in, out  []byte
	readDirs map[uint32]bool
}

func (c *countingChannel) Read(b []byte) (int, error) {
	n, err := c.Channel.Read(b)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.in = packets(append(c.in, b[:n]...), func(p []byte) {
		if len(p) >= 5 && p[0] == 12 {
			c.readDirs[binary.BigEndian.Uint32(p[1:])] = true
		}
	})
	return n, err
}

func (c *countingChannel) Write(b []byte) (int, error) {
	c.mu.Lock()
	c.out = packets(append(c.out, b...), func(p []byte) {
		if len(p) < 5 || !c.readDirs[binary.BigEndian.Uint32(p[1:])] {
			return
		}
		delete(c.readDirs, binary.BigEndian.Uint32(p[1:]))
		if p[0] == 104 && len(p) >= 9 {
			c.server.readDirs.Add(1)
			c.server.listed.Add(int64(binary.BigEndian.Uint32(p[5:])))
		}
	})
	c.mu.Unlock()
	return c.Channel.Write(b)
}

// packets calls each for every whole SFTP packet at the start of b, its
// type and what follows, and returns what is left.
func packets(b []byte, each func(packet []byte)) []byte {
	for len(b) >= 4 {
		n := int(binary.BigEndian.Uint32(b))
		if len(b) < 4+n {
			break
		}
		each(b[4 : 4+n])
		b = b[4+n:]
	}
	return b
}

// KnownHostsLine is the known_hosts line of the server's keys, or of keys
// when given, for the host and port it listens on, as ssh-keyscan writes it:
// [127.0.0.1]:port.
func (s *Server) KnownHostsLine(keys ...ssh.Signer) string {
	if len(keys) == 0 {
		keys = s.HostKeys
	}
	var lines []string
	for _, key := range keys {
		lines = append(lines, knownhosts.Line([]string{knownhosts.Normalize(s.Addr)}, key.PublicKey()))
	}
	return strings.Join(lines, "\n") + "\n"
}

// WriteKnownHosts writes a known_hosts file of KnownHostsLine into dir.
func (s *Server) WriteKnownHosts(t testing.TB, dir string, keys ...ssh.Signer) string {
	t.Helper()
	return WriteFile(t, dir, "known_hosts", s.KnownHostsLine(keys...))
}

// WriteFile writes a file into dir, readable by its owner alone.
func WriteFile(t testing.TB, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// NewEd25519Key is a new ed25519 key.
func NewEd25519Key(t testing.TB) ssh.Signer {
	t.Helper()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	return signer
}

// NewECDSAKey is a new ECDSA P-256 key.
func NewECDSAKey(t testing.TB) ssh.Signer {
	t.Helper()
	private, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	return signer
}

// WriteClientKey writes a new ed25519 private key into dir in OpenSSH's
// format, encrypted with passphrase when it is not empty, and returns its
// path and its public key, for Options.AuthorizedKeys.
//
// An encrypted key is encrypted as ssh-keygen -a 1 would: with one round
// of bcrypt_pbkdf rather than the 16 that ssh.MarshalPrivateKeyWithPassphrase
// always takes, each of which costs a sizeable part of a second under the
// race detector, to make the key and again to decrypt it. The rounds are
// written in the key, so it is read as any other.
func WriteClientKey(t testing.TB, dir, name, passphrase string) (string, ssh.PublicKey) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var block *pem.Block
	if passphrase == "" {
		block, err = ssh.MarshalPrivateKey(private, "")
	} else {
		block, err = encryptedKey(public, private, passphrase)
	}
	if err != nil {
		t.Fatal(err)
	}
	key, err := ssh.NewPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	return WriteFile(t, dir, name, string(pem.EncodeToMemory(block))), key
}

// encryptedKey is an ed25519 key in OpenSSH's format
// (PROTOCOL.key of OpenSSH), encrypted with aes256-ctr under a key that one
// round of bcrypt_pbkdf derives from passphrase.
func encryptedKey(public ed25519.PublicKey, private ed25519.PrivateKey, passphrase string) (*pem.Block, error) {
	salt := make([]byte, 16)
	var check [4]byte
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	if _, err := rand.Read(check[:]); err != nil {
		return nil, err
	}
	options := struct {
		Salt   []byte
		Rounds uint32
	}{salt, 1}
	derived := bcryptPBKDF([]byte(passphrase), salt, int(options.Rounds), 32+aes.BlockSize)
	keyType := ssh.KeyAlgoED25519
	plain := ssh.Marshal(struct {
		Check1, Check2 uint32
		KeyType        string
		Public         []byte
		Private        []byte
		Comment        string
	}{binary.BigEndian.Uint32(check[:]), binary.BigEndian.Uint32(check[:]), keyType, public, private, ""})
	for i := 1; len(plain)%aes.BlockSize != 0; i++ {
		plain = append(plain, byte(i))
	}
	block, err := aes.NewCipher(derived[:32])
	if err != nil {
		return nil, err
	}
	sealed := make([]byte, len(plain))
	// The IV is the second part of what bcrypt derives from the passphrase
	// and a random salt, as OpenSSH's key format has it; gosec sees a slice
	// of a value and calls it hard-coded.
	cipher.NewCTR(block, derived[32:]).XORKeyStream(sealed, plain) //nolint:gosec // G407: the IV is derived, per OpenSSH's format
	body := ssh.Marshal(struct {
		CipherName, KdfName, KdfOptions string
		Keys                            uint32
		Public                          []byte
		Private                         []byte
	}{"aes256-ctr", "bcrypt", string(ssh.Marshal(options)), 1, ssh.Marshal(struct {
		KeyType string
		Public  []byte
	}{keyType, public}), sealed})
	return &pem.Block{Type: "OPENSSH PRIVATE KEY", Bytes: append([]byte("openssh-key-v1\x00"), body...)}, nil
}

// bcryptPBKDF is OpenBSD's bcrypt_pbkdf(3), as
// golang.org/x/crypto/ssh/internal/bcrypt_pbkdf has it, which only x/crypto
// itself may import.
func bcryptPBKDF(password, salt []byte, rounds, keyLen int) []byte {
	const blockSize = 32
	blocks := (keyLen + blockSize - 1) / blockSize
	key := make([]byte, blocks*blockSize)
	h := sha512.New()
	h.Write(password)
	shaPassword := h.Sum(nil)
	count, tmp := make([]byte, 4), make([]byte, blockSize)
	for b := 1; b <= blocks; b++ {
		h.Reset()
		h.Write(salt)
		binary.BigEndian.PutUint32(count, uint32(b))
		h.Write(count)
		bcryptHash(tmp, shaPassword, h.Sum(nil))
		out := slices.Clone(tmp)
		for range rounds - 1 {
			h.Reset()
			h.Write(tmp)
			bcryptHash(tmp, shaPassword, h.Sum(nil))
			for j := range out {
				out[j] ^= tmp[j]
			}
		}
		for i, v := range out {
			key[i*blocks+b-1] = v
		}
	}
	return key[:keyLen]
}

func bcryptHash(out, shaPassword, shaSalt []byte) {
	c, err := blowfish.NewSaltedCipher(shaPassword, shaSalt)
	if err != nil {
		panic(err)
	}
	for range 64 {
		blowfish.ExpandKey(shaSalt, c)
		blowfish.ExpandKey(shaPassword, c)
	}
	copy(out, "OxychromaticBlowfishSwatDynamite")
	for i := 0; i < 32; i += 8 {
		for range 64 {
			c.Encrypt(out[i:i+8], out[i:i+8])
		}
	}
	// Each word is written the other way round, as bcrypt_pbkdf(3) does.
	for i := 0; i < 32; i += 4 {
		binary.LittleEndian.PutUint32(out[i:], binary.BigEndian.Uint32(out[i:]))
	}
}

// gate holds the server's reads and writes while it is shut.
type gate struct {
	mu     sync.Mutex
	opened chan struct{}
}

func newGate() *gate {
	g := &gate{opened: make(chan struct{})}
	close(g.opened)
	return g
}

func (g *gate) shut() {
	g.mu.Lock()
	defer g.mu.Unlock()
	select {
	case <-g.opened:
		g.opened = make(chan struct{})
	default:
	}
}

func (g *gate) open() {
	g.mu.Lock()
	defer g.mu.Unlock()
	select {
	case <-g.opened:
	default:
		close(g.opened)
	}
}

func (g *gate) wait(closed <-chan struct{}) error {
	g.mu.Lock()
	opened := g.opened
	g.mu.Unlock()
	select {
	case <-opened:
		return nil
	case <-closed:
		return net.ErrClosed
	}
}

// gatedConn is a connection of the server that reads and writes only while
// the gate is open.
type gatedConn struct {
	net.Conn
	gate      *gate
	closeOnce sync.Once
	closed    chan struct{}
}

func (c *gatedConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if waitErr := c.gate.wait(c.closed); waitErr != nil {
		return 0, waitErr
	}
	return n, err
}

func (c *gatedConn) Write(b []byte) (int, error) {
	if err := c.gate.wait(c.closed); err != nil {
		return 0, err
	}
	return c.Conn.Write(b)
}

func (c *gatedConn) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return c.Conn.Close()
}
