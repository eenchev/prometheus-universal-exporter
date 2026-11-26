package fetch

import (
	"context"
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
// is a connection pool, and one per request would never reuse a connection.
// Every HTTPS scrape would pay a full TLS handshake, and each idle keep-alive
// connection would stay open, with its goroutines, until the other end
// closed it.
//
// So transports are cached by the settings that configure them: the TLS
// settings (the CA, client certificate and key files, each with its
// modification time and size, and insecure_skip_verify) and whether HTTP/2
// is attempted. Collectors with the same settings share a pool. A reload
// that changes them, or a scrape that overrides insecure_skip_verify or
// enable_http2, gets its own pool. A certificate file rotated on disk gets a
// new pool at the next request, and the old pool's idle connections are
// closed. A pool unused for transportIdleTTL is closed and forgotten, so
// settings dropped by a reload do not keep connections open. Idle
// connections inside a pool close after transportIdleConnTimeout.
//
// Like curl and most Go programs, every transport sends its requests through
// the proxy the environment names, unless an http or graphite collector
// names its own (proxy.go). HTTP_PROXY is for http URLs, HTTPS_PROXY for
// https ones, and NO_PROXY lists the hosts, domains and networks to reach
// directly. The lower-case spellings work too. Requests to localhost and
// loopback addresses always go direct. The environment is read when a
// transport is built, which in practice is once, at the first request. An
// http collector that sends its requests to a Unix socket uses no proxy at
// all (unixsocket.go).

const (
	transportIdleTTL         = 5 * time.Minute
	transportIdleConnTimeout = 90 * time.Second
	transportMaxIdlePerHost  = 8
)

// maxResponseHeaderBytes is the maximum size of a response's status line and
// headers. max_response_bytes bounds only the body. Go's own bound on the
// headers is 10 MiB, all kept in memory as long as the response is, whatever
// the collector's limit. No API's headers come near 1 MiB.
const maxResponseHeaderBytes = 1 << 20

// tlsHandshakeTimeout is how long the TLS handshake of a connection to a
// target may take. A target that accepts a connection and then says nothing
// is given up after it, where the probe's own deadline may be minutes away.
const tlsHandshakeTimeout = 10 * time.Second

// tlsHandshakeLimit is what a test gave the handshake in place of
// tlsHandshakeTimeout (SetTLSHandshakeTimeout); zero is the exporter's own.
var tlsHandshakeLimit atomic.Int64

// SetTLSHandshakeTimeout sets how long a connection's TLS handshake may take
// in pools built from now on. In the exporter it is ten seconds. It is for
// tests. A handshake of a few milliseconds takes far longer on a machine
// whose CPUs are all busy elsewhere, and a test not about the limit then
// fails because of it. Such a test runs with a limit of half a minute, the
// bound of a hang. A test of the limit itself sets a short one and connects
// to something that never answers the handshake. A pool built earlier keeps
// the limit it was built with.
func SetTLSHandshakeTimeout(limit time.Duration) {
	tlsHandshakeLimit.Store(int64(limit))
}

// grpcConnectLimit is the value a test set in place of grpcConnectTimeout
// for an attempt to connect to a grpc target (SetGRPCConnectTimeout). Zero
// means the exporter's own. It is defined here, next to the handshake limit,
// and not with the grpc connections, because a build without the grpc
// request type leaves those out. The tests use the same setup code whatever
// was built.
var grpcConnectLimit atomic.Int64

// SetGRPCConnectTimeout sets how long an attempt to connect to a grpc target
// may take, for connections made from now on. In the exporter it is twenty
// seconds. The attempt lasts until the server has answered the HTTP/2
// preface, after the TCP connection and the TLS handshake. It is for tests.
// A connection of a few milliseconds takes far longer on a machine whose
// CPUs are all busy elsewhere, and a test not about the limit then fails
// because of it. Such a test runs with a limit of half a minute, the bound
// of a hang. A test of the limit itself sets a short one and connects to
// something that never answers the preface. A connection made earlier keeps
// the limit it was made with.
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
	// (targetpolicy.go). A connection is checked against them only once,
	// when it is made, so collectors with different policies must not share
	// one. Otherwise an idle connection opened by another collector could
	// carry a request to an address its own policy refuses. Policies are
	// interned, so collectors with the same lists still share a pool.
	policy *targetPolicy
	// proxy is the collector's request.proxy_url: "" for the proxy the
	// environment names, and model.ProxyDirect for none. proxyAuth holds the
	// files of the credentials sent to it (proxy.go). A connection made
	// through one proxy, or with one proxy credential, must not carry a
	// request meant for another.
	proxy     string
	proxyAuth model.ProxyBasicAuthFile
	// socket is the http collector's request.unix_socket, "" for none
	// (unixsocket.go). Every connection of the pool goes to it, so a
	// connection to one socket must not carry a request meant for another
	// socket or for a host reached over TCP.
	socket string
	// newConnections is the collector's request.new_connection: a pool that
	// keeps no connection. Every request it sends, including each attempt
	// and each redirect, uses a new connection that is closed after the
	// response. An uptime check then sees the site as a new client does: its
	// name is looked up and its certificate presented and verified again on
	// every probe. A kept connection would instead answer with the
	// certificate and address seen by the probe that opened it.
	newConnections bool
	// fastcgi is the http collector's request.fastcgi written as one value
	// (fastcgiKey), "" for none. The pool's http requests are then FastCGI
	// requests (fastcgi.go), which must not go on a pool of HTTP connections
	// or on one with another document root or other parameters.
	fastcgi string
	// ipFamily is the collector's request.ip_family (ipfamily.go): the
	// family every connection of the pool uses, "" to let Go choose. A
	// connection made over one family must not carry a request that asked
	// for the other.
	ipFamily string
}

// collectorFastCGI is a collector's request.fastcgi in the form the
// transport's settings hold. registerFastCGI makes a transport send its http
// requests as FastCGI requests, dialing with dial. Both are nil in a build
// without the http type, whose file sets them (fastcgi.go).
var (
	collectorFastCGI func(*model.Collector) string
	registerFastCGI  func(t *http.Transport, key string, dial func(ctx context.Context, network, address string) (net.Conn, error))
)

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
	proxy, proxyURL, err := transportProxy(settings)
	if err != nil {
		return nil, err
	}
	dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	dialTo := familyDialer(dialer.DialContext, settings.ipFamily)
	if settings.socket != "" {
		// Every connection goes to the socket, whatever host and port the
		// URL names, and never through a proxy, including the
		// environment's (unixsocket.go). The address the transport asks
		// for stays the URL's, which the policy checked.
		socket := settings.socket
		proxy, proxyURL = nil, nil
		dialTo = func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", socket)
		}
	}
	if settings.fastcgi != "" {
		// FastCGI is spoken to the application itself, never through a
		// proxy, the environment's included (fastcgi.go).
		proxy, proxyURL = nil, nil
	}
	// Every connection is checked against the allowed_targets and
	// denied_targets of the request that made it (targetpolicy.go).
	dial := policyDialer(dialTo)
	transport := &http.Transport{
		Proxy:               proxy,
		TLSClientConfig:     tlsCfg,
		ForceAttemptHTTP2:   settings.EnableHTTP2,
		DialContext:         dial,
		MaxIdleConnsPerHost: transportMaxIdlePerHost,
		IdleConnTimeout:     transportIdleConnTimeout,
		TLSHandshakeTimeout: handshakeTimeout(),
		// An answer with more headers than this fails the request
		// (responseHeadersTooLarge).
		MaxResponseHeaderBytes: maxResponseHeaderBytes,
		// A proxy's refusal to tunnel fails as one (proxy.go).
		OnProxyConnectResponse: refuseProxyConnect,
	}
	if settings.newConnections {
		transport.DisableKeepAlives = true
	}
	if settings.fastcgi != "" && registerFastCGI != nil {
		registerFastCGI(transport, settings.fastcgi, dial)
	}
	if proxyURL != nil && proxyURL.Scheme == "https" {
		transport.DialTLSContext = proxyTLSDialer(dial, handshakeTimeout())
	}
	c.entries[settings] = &cachedTransport{stamp: stamp, transport: transport, lastUsed: now}
	return transport, nil
}

// responseHeadersTooLarge reports whether err is Go's error for a response
// whose headers exceed the transport's MaxResponseHeaderBytes. Go has no
// error value for it, only these two texts. The first is HTTP/1's. The
// second is HTTP/2's for a header list it decoded fully and found over its
// advertised limit. That is the rarer HTTP/2 case: a list over the limit
// before its last frame instead has the connection closed, with an error
// that says nothing about headers (http2ProtocolError).
func responseHeadersTooLarge(err error) bool {
	if err == nil {
		return false
	}
	text := err.Error()
	return strings.Contains(text, "server response headers exceeded") || strings.Contains(text, "response header list larger than advertised limit")
}

// http2ProtocolError reports whether err is the HTTP/2 client closing its
// connection because of what the target sent: a connection error with code
// PROTOCOL_ERROR. Go ends a response this way when its headers exceed
// MaxResponseHeaderBytes while more are still to come: it stops reading
// them, closes the connection and reports this, without mentioning headers.
// It also ends any other protocol violation this way, and nothing here can
// tell the two apart. The error's type is internal to net/http, so it is
// recognized by its shape: an error code, 1 being PROTOCOL_ERROR, of a type
// named for a connection error.
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
		// This applies Go's own limit, and the collector's allowed_schemes,
		// allowed_targets and denied_targets, to the URL each redirect leads
		// to. What the redirected request carries is decided first
		// (redirecttrust.go), so a debug report shows its headers as sent.
		// Everything is taken from the request and its context, because the
		// client, and this function with it, is copied for a request sent
		// on its own connection (onOwnConnection).
		client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
			ctx := req.Context()
			if req.Response != nil {
				traceHTTPOutcome(ctx, req.Response)
			}
			hop := settleRedirect(ctx, req, via)
			span := propagateTrace(ctx, req.Header, true)
			traceRequest(ctx, req.Method, req.URL.String(), req.Header, req.Host, true)
			traceSpanID(ctx, span)
			traceWithheld(ctx, hop.withheld, hop.traceNote())
			if len(via) >= 10 {
				return errors.New("stopped after 10 redirects")
			}
			if err := checkRedirect(ctx, req.URL); err != nil {
				return err
			}
			// A chain's redirects stay on the target's origin.
			if err := chainRedirect(ctx, req.URL); err != nil {
				return err
			}
			if err := hop.refusal(req); err != nil {
				return err
			}
			// A hop is a request sent like any other, so it waits for
			// its turn under the rate limit of where it goes.
			waited, err := waitRateLimit(ctx, req.URL)
			if err != nil {
				return err
			}
			traceRateLimitWait(ctx, waited, true)
			return nil
		}
	} else {
		// The response of the redirect itself is returned, so a collector sees
		// the 3xx status rather than silently following it to another host.
		client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	}
	return client, nil
}

// onOwnConnection returns client with its own pool that keeps nothing. Its
// request is sent on a connection made for it, which no other request can
// use and which is closed once the response has been read. The retry after
// an HTTP/2 protocol error is sent this way (fetch explains why). The
// connection is made like the client's others: through the same proxy, with
// the same TLS settings, and checked against the same target policy.
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
