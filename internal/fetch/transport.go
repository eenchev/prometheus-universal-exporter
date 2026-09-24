package fetch

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"golang.org/x/net/http/httpproxy"
)

// Target requests and OTLP exports reuse their connections. An http.Transport
// is a connection pool, and one per request would never reuse a connection:
// every HTTPS scrape would pay a full TLS handshake, and the keep-alive
// connection each left idle would stay open, with its goroutines, until the
// other end closed it.
//
// So transports are cached by what configures them: the TLS settings — the CA,
// client certificate and key files, each with its modification time and size,
// and insecure_skip_verify — and whether HTTP/2 is attempted. Collectors that
// agree on these share a pool; a reload that changes them, or a scrape
// overriding insecure_skip_verify or enable_http2, gets a pool of its own; and
// a certificate file that is rotated on disk gets a new pool at the next
// request, with the old one's idle connections closed. A pool nothing has used
// for transportIdleTTL is closed and forgotten, so settings a reload dropped
// do not keep connections open. Idle connections inside a pool close after
// transportIdleConnTimeout.
//
// Every transport sends its requests through the proxy the environment names,
// as curl and most Go programs do: HTTP_PROXY for http URLs, HTTPS_PROXY for
// https ones, and NO_PROXY for the hosts, domains and networks to reach
// directly. The lower-case spellings work too. Requests to localhost and
// loopback addresses always go direct. The environment is read when a
// transport is built, which in practice is once, at the first request.

const (
	transportIdleTTL         = 5 * time.Minute
	transportIdleConnTimeout = 90 * time.Second
	transportMaxIdlePerHost  = 8
)

// TransportSettings are what a connection pool depends on. Requests with the
// same settings share one pool.
type TransportSettings struct {
	TLS         model.TLSConfig
	EnableHTTP2 bool
}

type cachedTransport struct {
	stamp     string
	transport *http.Transport
	lastUsed  time.Time
}

type transportCache struct {
	mu      sync.Mutex
	entries map[TransportSettings]*cachedTransport
}

var transports = newTransportCache()

func newTransportCache() *transportCache {
	return &transportCache{entries: map[TransportSettings]*cachedTransport{}}
}

// get returns the transport for settings, building it when there is none or
// when a TLS file changed since it was built.
func (c *transportCache) get(settings TransportSettings, now time.Time) (*http.Transport, error) {
	stamp := tlsFilesStamp(settings.TLS)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sweepLocked(now)
	if entry := c.entries[settings]; entry != nil {
		if entry.stamp == stamp {
			entry.lastUsed = now
			return entry.transport, nil
		}
		// A certificate or key was replaced on disk.
		entry.transport.CloseIdleConnections()
		delete(c.entries, settings)
	}
	tlsCfg, err := tlsConfig(settings.TLS)
	if err != nil {
		return nil, err
	}
	transport := &http.Transport{
		Proxy:               environmentProxy(),
		TLSClientConfig:     tlsCfg,
		ForceAttemptHTTP2:   settings.EnableHTTP2,
		DialContext:         (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConnsPerHost: transportMaxIdlePerHost,
		IdleConnTimeout:     transportIdleConnTimeout,
		TLSHandshakeTimeout: 10 * time.Second,
	}
	c.entries[settings] = &cachedTransport{stamp: stamp, transport: transport, lastUsed: now}
	return transport, nil
}

// environmentProxy is the proxy the environment names now. It is read here
// rather than through http.ProxyFromEnvironment, which reads the environment
// once per process, so that each transport sees the environment it was built
// in.
func environmentProxy() func(*http.Request) (*url.URL, error) {
	proxy := httpproxy.FromEnvironment().ProxyFunc()
	return func(req *http.Request) (*url.URL, error) { return proxy(req.URL) }
}

// sweepLocked closes the transports nothing has used for transportIdleTTL.
func (c *transportCache) sweepLocked(now time.Time) {
	for settings, entry := range c.entries {
		if now.Sub(entry.lastUsed) > transportIdleTTL {
			entry.transport.CloseIdleConnections()
			delete(c.entries, settings)
		}
	}
}

// size reports how many transports are cached, for tests.
func (c *transportCache) size() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

// tlsFilesStamp describes the TLS files as they are on disk now.
func tlsFilesStamp(t model.TLSConfig) string {
	var b strings.Builder
	for _, path := range []string{t.CAFile, t.CertFile, t.KeyFile} {
		b.WriteByte('|')
		if path == "" {
			continue
		}
		b.WriteString(path)
		st, err := os.Stat(path)
		if err != nil {
			b.WriteString(":missing")
			continue
		}
		b.WriteByte(':')
		b.WriteString(strconv.FormatInt(st.ModTime().UnixNano(), 10))
		b.WriteByte(':')
		b.WriteString(strconv.FormatInt(st.Size(), 10))
	}
	return b.String()
}

// HTTPClient is a client on the cached transport for settings. The client
// itself is cheap and carries the per-request redirect policy and timeout.
func HTTPClient(settings TransportSettings, followRedirects bool, timeout time.Duration) (*http.Client, error) {
	transport, err := transports.get(settings, time.Now())
	if err != nil {
		return nil, err
	}
	client := &http.Client{Transport: transport, Timeout: timeout}
	if !followRedirects {
		// The response of the redirect itself is returned, so a collector sees
		// the 3xx status rather than silently following it to another host.
		client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	}
	return client, nil
}

func tlsConfig(t model.TLSConfig) (*tls.Config, error) {
	// The exporter deliberately exposes request.tls.insecure_skip_verify and the
	// matching per-scrape override as a documented, opt-in setting for targets
	// whose certificate cannot be validated. TLS stays enabled and the minimum
	// version is pinned.
	cfg := &tls.Config{InsecureSkipVerify: t.InsecureSkipVerify, MinVersion: tls.VersionTLS12} //nolint:gosec // G402: documented opt-in, defaults to false
	if t.CAFile != "" {
		b, err := os.ReadFile(t.CAFile)
		if err != nil {
			return nil, err
		}
		pool, err := x509.SystemCertPool()
		if err != nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(b) {
			return nil, fmt.Errorf("no certificates found in %s", t.CAFile)
		}
		cfg.RootCAs = pool
	}
	if t.CertFile != "" || t.KeyFile != "" {
		if t.CertFile == "" || t.KeyFile == "" {
			return nil, errors.New("both tls cert_file and key_file are required")
		}
		cert, err := tls.LoadX509KeyPair(t.CertFile, t.KeyFile)
		if err != nil {
			return nil, err
		}
		cfg.Certificates = []tls.Certificate{cert}
	}
	return cfg, nil
}
