//go:build !select_request_types || request_type_grpc

package fetch

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

// grpc calls reuse their connections, as HTTP requests reuse their
// transports (transport.go): one grpc.ClientConn per target and TLS
// settings, kept across probes, which multiplexes every call to that target
// over one HTTP/2 connection. A certificate or key replaced on disk gets a
// new connection at the next call, and a connection nothing has used for
// transportIdleTTL, 5 minutes, is closed and forgotten.
//
// A connection can die without a word: a node that lost power, a firewall or
// a NAT that dropped its entry, sends neither FIN nor RST, and grpc-go goes on
// believing the connection ready. Every call on it would wait out its whole
// deadline. So a call that its own deadline ended on a connection grpc-go
// calls ready drops the connection (drop), and the next probe makes a new
// one, as an http request that timed out has its connection closed. Client
// keepalive pings would find the dead connection too, but a server with
// grpc-go's default enforcement policy answers pings more often than every
// five minutes with GOAWAY, so they are not sent.
//
// A connection is shared by the probes calling its target at the same time,
// so one the cache forgets is not closed under them: each call holds it
// (get) until it ends (release), as does a reflection question in flight, and
// a forgotten connection is closed when the last of them has ended.
//
// A connection is made through the proxy the environment names
// (HTTPS_PROXY, NO_PROXY), which grpc-go reads itself.

// grpcConnKey is what a connection depends on.
type grpcConnKey struct {
	dial     string
	tls      bool
	settings model.TLSConfig
	// policy is the collector's allowed_targets and denied_targets, which
	// every connection is checked against; nil for none. proxied says the
	// connection goes through a proxy, whose address is not the server's.
	policy  *targetPolicy
	proxied bool
}

type grpcConnEntry struct {
	stamp    string
	conn     *grpc.ClientConn
	lastUsed time.Time
	// refusal is the last connection the policy refused, nil once one is
	// made; grpc-go reports a dialer's error to the call only as
	// UNAVAILABLE, with the reason as text.
	refusal *atomic.Pointer[TargetRefusedError]
	// calls is how many calls hold the connection now. retired says the
	// cache has forgotten it, so it is closed when the last of them ends.
	calls   int
	retired bool
}

type grpcConnCache struct {
	mu      sync.Mutex
	entries map[grpcConnKey]*grpcConnEntry
}

var grpcConns = &grpcConnCache{entries: map[grpcConnKey]*grpcConnEntry{}}

// get returns the connection for key, making it when there is none or when
// a TLS file changed since it was made, and holds it for the caller, who
// calls release when its call has ended. grpc.NewClient does not dial: the
// connection is made at the first call, and made again after it breaks.
func (c *grpcConnCache) get(key grpcConnKey, now time.Time) (*grpcConnEntry, error) {
	stamp := ""
	if key.tls {
		stamp = tlsFilesStamp(key.settings)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sweepLocked(now)
	if entry := c.entries[key]; entry != nil {
		if entry.stamp == stamp {
			entry.lastUsed = now
			entry.calls++
			return entry, nil
		}
		// A call may still be using the old connection, which Close would
		// cancel; it is closed when the last such call has ended.
		delete(c.entries, key)
		c.retireLocked(entry)
	}
	creds := insecure.NewCredentials()
	if key.tls {
		cfg, err := tlsConfig(key.settings)
		if err != nil {
			return nil, err
		}
		creds = credentials.NewTLS(cfg)
	}
	options := []grpc.DialOption{grpc.WithTransportCredentials(creds), grpc.WithConnectParams(grpcConnectParams())}
	// Behind a proxy grpc-go's own dialer connects through it, which a
	// dialer of the exporter's would bypass; the call resolved and checked
	// the server's addresses instead.
	refusal := &atomic.Pointer[TargetRefusedError]{}
	if key.policy != nil && !key.proxied {
		options = append(options, grpc.WithContextDialer(grpcPolicyDialer(key.policy, strings.TrimPrefix(key.dial, "dns:///"), refusal)))
	}
	conn, err := grpc.NewClient(key.dial, options...)
	if err != nil {
		return nil, err
	}
	entry := &grpcConnEntry{stamp: stamp, conn: conn, lastUsed: now, refusal: refusal, calls: 1}
	c.entries[key] = entry
	return entry, nil
}

// hold adds a hold on a connection its caller already holds, for work that
// outlives the caller's call; it ends with release as well.
func (c *grpcConnCache) hold(entry *grpcConnEntry) {
	c.mu.Lock()
	entry.calls++
	c.mu.Unlock()
}

// release ends a call's hold on a connection get returned, and closes the
// connection when the cache has forgotten it and no other call holds it.
func (c *grpcConnCache) release(entry *grpcConnEntry) {
	c.mu.Lock()
	entry.calls--
	closeNow := entry.retired && entry.calls == 0
	c.mu.Unlock()
	if closeNow {
		_ = entry.conn.Close()
	}
}

// drop forgets a connection that stopped answering, so the next call to its
// target makes a new one. The calls still holding it go on: one that is
// being answered is not cut off, and the connection is closed when the last
// of them ends. A connection already replaced is left alone: probes that
// time out together on one dead connection drop it once, and none of them
// drops the connection made after it.
func (c *grpcConnCache) drop(key grpcConnKey, entry *grpcConnEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries[key] != entry {
		return
	}
	delete(c.entries, key)
	c.retireLocked(entry)
}

// retireLocked marks a connection the cache has just forgotten, and closes
// it unless a call holds it; release closes it then.
func (c *grpcConnCache) retireLocked(entry *grpcConnEntry) {
	entry.retired = true
	if entry.calls == 0 {
		_ = entry.conn.Close()
	}
}

// sweepLocked forgets the connections nothing has used for transportIdleTTL.
func (c *grpcConnCache) sweepLocked(now time.Time) {
	for key, entry := range c.entries {
		if now.Sub(entry.lastUsed) > transportIdleTTL {
			delete(c.entries, key)
			c.retireLocked(entry)
		}
	}
}

// size reports how many connections are kept, for tests.
func (c *grpcConnCache) size() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

// grpcPolicyDialer connects to address, one the server's name resolved
// to, and checks the address it connected to against policy, recording a
// refusal in refused for the call to report as one.
func grpcPolicyDialer(policy *targetPolicy, hostPort string, refused *atomic.Pointer[TargetRefusedError]) func(context.Context, string) (net.Conn, error) {
	host, _, _ := net.SplitHostPort(hostPort)
	host, hostErr := canonicalHost(host)
	return func(ctx context.Context, address string) (net.Conn, error) {
		if hostErr != nil {
			var refusal *TargetRefusedError
			if errors.As(hostErr, &refusal) {
				refused.Store(refusal)
			}
			return nil, hostErr
		}
		conn, err := (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext(ctx, "tcp", address)
		if err != nil {
			return nil, err
		}
		remote, ok := conn.RemoteAddr().(*net.TCPAddr)
		if !ok {
			return conn, nil
		}
		connected, ok := netip.AddrFromSlice(remote.IP)
		if !ok {
			return conn, nil
		}
		nameAllowed, err := policy.checkName(host)
		if err == nil {
			err = policy.checkAddr(host, connected, nameAllowed)
		}
		if err != nil {
			_ = conn.Close()
			var refusal *TargetRefusedError
			if errors.As(err, &refusal) {
				refused.Store(refusal)
			}
			return nil, err
		}
		refused.Store(nil)
		return conn, nil
	}
}

// grpcBackoff keeps the wait between attempts to reconnect short. grpc-go's
// default grows to two minutes, and a connection that is waiting fails every
// call at once, so after a long outage a server that is back would go on
// being reported down for up to two minutes. Five seconds at most is as
// often as a probe could matter; a probe that finds the connection waiting
// also ends the wait, and so does each of its retries (reconnectNow). It is
// a variable for the tests of what ends that wait, which make it an hour.
var grpcBackoff = backoff.Config{BaseDelay: time.Second, Multiplier: 1.6, Jitter: 0.2, MaxDelay: 5 * time.Second}

// grpcConnectTimeout is how long an attempt to connect to a target may
// take: the TCP connection, the TLS handshake and the server's answer to the
// HTTP/2 preface. A target that takes the connection and then says nothing
// is given up after it, and the calls waiting for the connection fail. It
// is grpc-go's least time for an attempt, which gives one the longer of this
// and the wait before the next, so under grpcBackoff it is what an attempt
// has.
const grpcConnectTimeout = 20 * time.Second

// grpcConnectParams are how a connection made now connects and connects
// again: after the waits of grpcBackoff, each attempt within
// grpcConnectTimeout, or within what a test gave it instead
// (SetGRPCConnectTimeout).
func grpcConnectParams() grpc.ConnectParams {
	timeout := grpcConnectTimeout
	if limit := time.Duration(grpcConnectLimit.Load()); limit > 0 {
		timeout = limit
	}
	return grpc.ConnectParams{Backoff: grpcBackoff, MinConnectTimeout: timeout}
}
