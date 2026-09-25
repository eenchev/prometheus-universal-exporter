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
}

type grpcConnCache struct {
	mu      sync.Mutex
	entries map[grpcConnKey]*grpcConnEntry
}

var grpcConns = &grpcConnCache{entries: map[grpcConnKey]*grpcConnEntry{}}

// get returns the connection for key, making it when there is none or when
// a TLS file changed since it was made. grpc.NewClient does not dial: the
// connection is made at the first call, and made again after it breaks.
func (c *grpcConnCache) get(key grpcConnKey, now time.Time) (*grpc.ClientConn, *atomic.Pointer[TargetRefusedError], error) {
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
			return entry.conn, entry.refusal, nil
		}
		// A call may still be using the old connection, which Close would
		// cancel; it is closed once any call has long ended.
		old := entry.conn
		time.AfterFunc(transportIdleTTL, func() { _ = old.Close() })
		delete(c.entries, key)
	}
	creds := insecure.NewCredentials()
	if key.tls {
		cfg, err := tlsConfig(key.settings)
		if err != nil {
			return nil, nil, err
		}
		creds = credentials.NewTLS(cfg)
	}
	options := []grpc.DialOption{grpc.WithTransportCredentials(creds), grpcReconnect}
	// Behind a proxy grpc-go's own dialer connects through it, which a
	// dialer of the exporter's would bypass; the call resolved and checked
	// the server's addresses instead.
	refusal := &atomic.Pointer[TargetRefusedError]{}
	if key.policy != nil && !key.proxied {
		options = append(options, grpc.WithContextDialer(grpcPolicyDialer(key.policy, strings.TrimPrefix(key.dial, "dns:///"), refusal)))
	}
	conn, err := grpc.NewClient(key.dial, options...)
	if err != nil {
		return nil, nil, err
	}
	c.entries[key] = &grpcConnEntry{stamp: stamp, conn: conn, lastUsed: now, refusal: refusal}
	return conn, refusal, nil
}

// sweepLocked closes the connections nothing has used for transportIdleTTL.
func (c *grpcConnCache) sweepLocked(now time.Time) {
	for key, entry := range c.entries {
		if now.Sub(entry.lastUsed) > transportIdleTTL {
			_ = entry.conn.Close()
			delete(c.entries, key)
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

// grpcReconnect keeps the wait between attempts to reconnect short. grpc-go's
// default grows to two minutes, and a connection that is waiting fails every
// call at once, so after a long outage a server that is back would go on
// being reported down for up to two minutes. Five seconds at most is as
// often as a probe could matter; a probe that finds the connection waiting
// also ends the wait (reconnectNow).
var grpcReconnect = grpc.WithConnectParams(grpc.ConnectParams{
	Backoff:           backoff.Config{BaseDelay: time.Second, Multiplier: 1.6, Jitter: 0.2, MaxDelay: 5 * time.Second},
	MinConnectTimeout: 20 * time.Second,
})
