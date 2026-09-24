//go:build !select_request_types || request_type_grpc

package fetch

import (
	"context"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"google.golang.org/grpc"
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
	// every connection is checked against; nil for none.
	policy *targetPolicy
}

type grpcConnEntry struct {
	stamp    string
	conn     *grpc.ClientConn
	lastUsed time.Time
}

type grpcConnCache struct {
	mu      sync.Mutex
	entries map[grpcConnKey]*grpcConnEntry
}

var grpcConns = &grpcConnCache{entries: map[grpcConnKey]*grpcConnEntry{}}

// get returns the connection for key, making it when there is none or when
// a TLS file changed since it was made. grpc.NewClient does not dial: the
// connection is made at the first call, and made again after it breaks.
func (c *grpcConnCache) get(key grpcConnKey, now time.Time) (*grpc.ClientConn, error) {
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
			return entry.conn, nil
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
			return nil, err
		}
		creds = credentials.NewTLS(cfg)
	}
	options := []grpc.DialOption{grpc.WithTransportCredentials(creds)}
	if key.policy != nil {
		options = append(options, grpc.WithContextDialer(grpcPolicyDialer(key.policy, strings.TrimPrefix(key.dial, "dns:///"))))
	}
	conn, err := grpc.NewClient(key.dial, options...)
	if err != nil {
		return nil, err
	}
	c.entries[key] = &grpcConnEntry{stamp: stamp, conn: conn, lastUsed: now}
	return conn, nil
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

// grpcPolicyDialer connects to address and checks the address it connected
// to against policy, for the server hostPort. An address the server's name
// does not resolve to is a proxy's, whose target the call checked already.
func grpcPolicyDialer(policy *targetPolicy, hostPort string) func(context.Context, string) (net.Conn, error) {
	host, _, _ := net.SplitHostPort(hostPort)
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	return func(ctx context.Context, address string) (net.Conn, error) {
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
		connected = connected.Unmap()
		nameAllowed, err := policy.checkName(host)
		if err == nil {
			if addrs, resolveErr := resolveHost(ctx, host); resolveErr != nil || slices.ContainsFunc(addrs, func(a netip.Addr) bool { return a.Unmap() == connected }) {
				err = policy.checkAddr(host, connected, nameAllowed)
			}
		}
		if err != nil {
			_ = conn.Close()
			return nil, err
		}
		return conn, nil
	}
}
