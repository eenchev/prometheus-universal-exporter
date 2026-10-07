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
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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

// maxResponseHeaderBytes is the most a response's status line and headers
// may take. max_response_bytes bounds the body only, and Go's own bound on
// the headers is 10 MiB, all of it kept in memory for as long as the answer
// is, whatever the collector's limit; no API's headers come near 1 MiB.
const maxResponseHeaderBytes = 1 << 20

// tlsHandshakeTimeout is how long the TLS handshake of a connection to a
// target may take. A target that accepts a connection and then says nothing
// is given up after it, where the probe's own deadline may be minutes away.
const tlsHandshakeTimeout = 10 * time.Second

// tlsHandshakeLimit is what a test gave the handshake in place of
// tlsHandshakeTimeout (SetTLSHandshakeTimeout); zero is the exporter's own.
var tlsHandshakeLimit atomic.Int64

// SetTLSHandshakeTimeout is how long the TLS handshake of a connection may
// take in the pools built from now on, which is ten seconds in the exporter.
// It is for tests. A handshake of a few milliseconds takes far longer on a
// machine with every CPU busy elsewhere, and a test that is not about the
// limit then fails by it; such a test runs with a limit of half a minute,
// the bound of a hang. A test of the limit itself sets a short one, and
// connects to something that never answers the handshake. A pool built
// before keeps the limit it was built with.
func SetTLSHandshakeTimeout(limit time.Duration) {
	tlsHandshakeLimit.Store(int64(limit))
}

// grpcConnectLimit is what a test gave an attempt to connect to a grpc
// target in place of grpcConnectTimeout (SetGRPCConnectTimeout); zero is the
// exporter's own. It stands here, beside the handshake's, and not with the
// grpc connections, which a build without the grpc request type leaves out:
// the tests are set up by the same code whatever was built.
var grpcConnectLimit atomic.Int64

// SetGRPCConnectTimeout is how long an attempt to connect to a grpc target
// may take, on the connections made from now on, which is twenty seconds in
// the exporter: until the server has answered the HTTP/2 preface, after the
// TCP connection and the TLS handshake. It is for tests. A connection of a
// few milliseconds takes far longer on a machine with every CPU busy
// elsewhere, and a test that is not about the limit then fails by it; such a
// test runs with a limit of half a minute, the bound of a hang. A test of
// the limit itself sets a short one, and connects to something that never
// answers the preface. A connection made before keeps the limit it was made
// with.
func SetGRPCConnectTimeout(limit time.Duration) {
	grpcConnectLimit.Store(int64(limit))
}

// handshakeTimeout is the limit a pool built now gives a TLS handshake.
func handshakeTimeout() time.Duration {
	if limit := time.Duration(tlsHandshakeLimit.Load()); limit > 0 {
		return limit
	}
	return tlsHandshakeTimeout
}

// TransportSettings are what a connection pool depends on. Requests with the
// same settings share one pool.
type TransportSettings struct {
	TLS         model.TLSConfig
	EnableHTTP2 bool
	// policy is the collector's allowed_targets and denied_targets
	// (targetpolicy.go). A connection is checked against them once, when it
	// is made, so collectors with different policies must not share one: an
	// idle connection another collector opened would otherwise take a
	// request to an address its own policy refuses. Policies are interned,
	// so collectors with the same lists still share a pool.
	policy *targetPolicy
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
		Proxy:             environmentProxy(),
		TLSClientConfig:   tlsCfg,
		ForceAttemptHTTP2: settings.EnableHTTP2,
		// Every connection is checked against the allowed_targets and
		// denied_targets of the request that made it (targetpolicy.go).
		DialContext:         policyDialer((&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext),
		MaxIdleConnsPerHost: transportMaxIdlePerHost,
		IdleConnTimeout:     transportIdleConnTimeout,
		TLSHandshakeTimeout: handshakeTimeout(),
		// An answer with more headers than this fails the request
		// (responseHeadersTooLarge).
		MaxResponseHeaderBytes: maxResponseHeaderBytes,
	}
	c.entries[settings] = &cachedTransport{stamp: stamp, transport: transport, lastUsed: now}
	return transport, nil
}

// responseHeadersTooLarge says whether err is Go's for a response whose
// headers are over the transport's MaxResponseHeaderBytes. Go has no error
// value for it, only these two texts. The first is HTTP/1's. The second is
// HTTP/2's for a header list it decoded to the end and found over the limit
// it had advertised; that is the rarer of HTTP/2's two ends, since a list
// that is over the limit before its last frame has the connection closed
// instead, with an error that says nothing of headers (http2ProtocolError).
func responseHeadersTooLarge(err error) bool {
	if err == nil {
		return false
	}
	text := err.Error()
	return strings.Contains(text, "server response headers exceeded") || strings.Contains(text, "response header list larger than advertised limit")
}

// http2ProtocolError says whether err is the HTTP/2 client ending its
// connection for what the target sent: a connection error whose code is
// PROTOCOL_ERROR. That is how Go ends an answer whose headers pass
// MaxResponseHeaderBytes while more of them are still to come — it stops
// reading them, closes the connection and reports this, with no word of the
// headers — and how it ends one that broke the protocol in any other way,
// which nothing here can tell from the first. The error's type is internal
// to net/http, so it is told by what it is: an error code, 1 being
// PROTOCOL_ERROR, of a type named for a connection error.
func http2ProtocolError(err error) bool {
	found := false
	walkErrors(err, func(e error) {
		v := reflect.ValueOf(e)
		if v.Kind() == reflect.Uint32 && v.Uint() == http2ErrCodeProtocol && strings.HasSuffix(v.Type().Name(), "ConnectionError") {
			found = true
		}
	})
	return found
}

// http2ErrCodeProtocol is PROTOCOL_ERROR among HTTP/2's error codes
// (RFC 9113, section 7).
const http2ErrCodeProtocol = 1

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
	if followRedirects {
		// Go's own limit, and the collector's allowed_schemes, and
		// allowed_targets and denied_targets, for the URL each redirect
		// leads to. What the redirected request carries is settled first
		// (redirecttrust.go), so a debug report shows its headers as they
		// are sent. Everything is taken from the request and its context:
		// the client, and this function with it, is copied for a request
		// sent on a connection of its own (onOwnConnection).
		client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
			ctx := req.Context()
			if req.Response != nil {
				traceOutcome(ctx, req.Response.Status)
			}
			hop := settleRedirect(ctx, req, via)
			traceRequest(ctx, req.Method, req.URL.String(), req.Header, req.Host, true)
			traceWithheld(ctx, hop.withheld, hop.traceNote())
			if len(via) >= 10 {
				return errors.New("stopped after 10 redirects")
			}
			if err := checkRedirect(ctx, req.URL); err != nil {
				return err
			}
			return hop.refusal(req)
		}
	} else {
		// The response of the redirect itself is returned, so a collector sees
		// the 3xx status rather than silently following it to another host.
		client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	}
	return client, nil
}

// onOwnConnection is client with a pool of its own that keeps nothing: its
// request is sent on a connection made for it, which no other request can be
// put on and which is closed when the answer has been read. The retry after
// an HTTP/2 protocol error is sent this way (fetch says why). The
// connection is made as the client's others are, through the same proxy,
// with the same TLS settings and checked against the same target policy.
func onOwnConnection(client *http.Client) *http.Client {
	shared, ok := client.Transport.(*http.Transport)
	if !ok {
		return client
	}
	transport := shared.Clone()
	transport.DisableKeepAlives = true
	own := *client
	own.Transport = transport
	return &own
}

func tlsConfig(t model.TLSConfig) (*tls.Config, error) {
	// The exporter deliberately exposes request.tls.insecure_skip_verify and the
	// matching per-scrape override as a documented, opt-in setting for targets
	// whose certificate cannot be validated. TLS stays enabled and the minimum
	// version is pinned.
	cfg := &tls.Config{InsecureSkipVerify: t.InsecureSkipVerify, MinVersion: tls.VersionTLS12, ServerName: t.ServerName} //nolint:gosec // G402: documented opt-in, defaults to false
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
