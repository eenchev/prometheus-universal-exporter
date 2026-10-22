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
// new connection at the next call, and a connection no call has held for
// transportIdleTTL, 5 minutes, is closed and forgotten, by a timer of its
// own: grpc-go goes on reconnecting a connection whose server is gone, every
// few seconds (grpcBackoff), so one left to the next call to its target
// would redial a target nobody asks about any more for as long as no call
// came. At most maxGRPCConnections are kept: a probe chooses its target, so
// without a bound every target ever probed would be redialled until its
// timer ends. A new connection past the bound closes the one no call has
// held for longest; one a call holds is never closed under it, so while
// calls hold more than the bound, as many are kept.
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
	// idle closes the connection when no call has held it for the cache's
	// idle time; it runs while no call holds it. released counts the times
	// the last hold ended, so that a timer that fires as a call takes the
	// connection, and that call has let it go again, leaves it be.
	idle     *time.Timer
	released uint64
}

type grpcConnCache struct {
	mu      sync.Mutex
	entries map[grpcConnKey]*grpcConnEntry
	// idleTime and most are how long a connection no call holds is kept,
	// and how many are kept, transportIdleTTL and maxGRPCConnections when
	// zero; the tests set shorter ones.
	idleTime time.Duration
	most     int
}

// maxGRPCConnections bounds the connections kept, across collectors: a
// probe chooses the target, and each target is a connection.
const maxGRPCConnections = 1000

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
	if entry := c.entries[key]; entry != nil {
		if entry.stamp == stamp {
			entry.lastUsed = now
			entry.calls++
			if entry.idle != nil {
				entry.idle.Stop()
			}
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
	// An answer's headers, and its trailers, are each held to the bound an
	// http answer's headers are, where grpc-go's own is 16 MiB, all of it
	// kept as the response's headers: the connection advertises it, and
	// stops reading an answer past it (headerListTooLarge).
	options := []grpc.DialOption{grpc.WithTransportCredentials(creds), grpc.WithConnectParams(grpcConnectParams()), grpc.WithMaxHeaderListSize(maxResponseHeaderBytes)}
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
	c.trimLocked()
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
// A connection the cache keeps waits its idle time for the next call, and
// when the cache keeps more than its bound, the one held longest ago is
// closed.
func (c *grpcConnCache) release(entry *grpcConnEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry.calls--
	if entry.calls > 0 {
		return
	}
	if entry.retired {
		_ = entry.conn.Close()
		return
	}
	entry.released++
	released := entry.released
	if entry.idle != nil {
		entry.idle.Stop()
	}
	entry.idle = time.AfterFunc(c.idleTimeout(), func() { c.idleEnded(entry, released) })
	c.trimLocked()
}

// idleEnded closes a connection no call has held since its last hold ended
// for the idle time; released is which ending that was.
func (c *grpcConnCache) idleEnded(entry *grpcConnEntry, released uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if entry.retired || entry.calls > 0 || entry.released != released {
		return
	}
	c.forgetLocked(entry)
}

// trimLocked closes, while the cache keeps more connections than its bound,
// the one no call holds that was used longest ago. Connections calls hold
// are not counted out: they are closed when the last call ends, if the cache
// still keeps too many then.
func (c *grpcConnCache) trimLocked() {
	most := c.most
	if most <= 0 {
		most = maxGRPCConnections
	}
	for len(c.entries) > most {
		var oldest *grpcConnEntry
		for _, entry := range c.entries {
			if entry.calls == 0 && (oldest == nil || entry.lastUsed.Before(oldest.lastUsed)) {
				oldest = entry
			}
		}
		if oldest == nil {
			return
		}
		c.forgetLocked(oldest)
	}
}

// forgetLocked forgets a connection the cache keeps and retires it.
func (c *grpcConnCache) forgetLocked(entry *grpcConnEntry) {
	for key, kept := range c.entries {
		if kept == entry {
			delete(c.entries, key)
			break
		}
	}
	c.retireLocked(entry)
}

func (c *grpcConnCache) idleTimeout() time.Duration {
	if c.idleTime > 0 {
		return c.idleTime
	}
	return transportIdleTTL
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
	if entry.idle != nil {
		entry.idle.Stop()
	}
	if entry.calls == 0 {
		_ = entry.conn.Close()
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
