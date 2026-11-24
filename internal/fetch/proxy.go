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
	"strconv"
	"strings"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// An http or graphite collector may name the proxy its requests go through
// (request.proxy_url). It replaces the one the environment names for the
// whole exporter (transport.go). Uses: a target reachable only through a
// proxy on its own network, or one that must be reached directly although
// the rest of the exporter's traffic uses a proxy (model.ProxyDirect).
// Every request of the collector goes through it, whatever its host,
// including its login, redirects and pages. A collector has no NO_PROXY,
// and localhost goes through the proxy too.
//
// The proxy is part of the transport's settings. So collectors with
// different proxies, or different credentials for one, never share a
// connection pool, and a reload that changes the proxy gets its own pool.
// Its credentials come from files (request.proxy_basic_auth_file), read on
// every request. They never come from the URL, because the configuration
// shows the URL to whoever reads it, and so do --dry-run and a debug report.
//
// An http:// proxy is asked in plain text. It is sent an http target's
// whole request, and asked to tunnel to an https target (CONNECT). An
// https:// proxy is asked the same over TLS, verified only against the
// system's roots. request.tls belongs to the target, and a client
// certificate meant for the target is not shown to the proxy. A socks5://
// (or socks5h://) proxy is asked to connect to the target by name, which
// the proxy resolves.

// withProxy is settings with c's request.proxy_url and the files of its
// credentials, and the request.unix_socket of an http collector, which
// takes the proxy's place (unixsocket.go).
func withProxy(settings TransportSettings, c *model.Collector) TransportSettings {
	settings.proxy = c.Request.ProxyURL
	settings.socket = CollectorPolicy(c).UnixSocket
	if collectorFastCGI != nil && c.Request.Type == RequestTypeHTTP {
		settings.fastcgi = collectorFastCGI(c)
	}
	if files := c.Request.ProxyBasicAuthFile; files != nil {
		settings.proxyAuth = *files
	}
	return settings
}

// transportProxy returns the proxy function of a transport built for
// settings: the environment's when the collector names none; nil (no
// proxy) for model.ProxyDirect; otherwise the collector's, with the
// credentials its files hold at the time the request is sent.
func transportProxy(settings TransportSettings) (func(*http.Request) (*url.URL, error), *url.URL, error) {
	switch settings.proxy {
	case "":
		return environmentProxy(), nil, nil
	case model.ProxyDirect:
		return nil, nil, nil
	}
	u, err := parseProxyURL(settings.proxy)
	if err != nil {
		return nil, nil, err
	}
	auth := settings.proxyAuth
	return func(*http.Request) (*url.URL, error) {
		through := *u
		if auth.Username != "" || auth.Password != "" {
			username, password, err := readProxyCredentials(auth)
			if err != nil {
				return nil, &proxyCredentialsError{err}
			}
			through.User = url.UserPassword(username, password)
		}
		return &through, nil
	}, u, nil
}

// proxyCredentialsError is a failure to read the proxy's credentials,
// which Go's transport returns as the request's own error. A retry would
// read the files again, wait out the backoff, and fail as the first
// attempt did. So the request ends at once (fetcher.go), as a request whose
// basic_auth_file cannot be read ends before it is sent.
type proxyCredentialsError struct{ err error }

func (e *proxyCredentialsError) Error() string { return e.err.Error() }
func (e *proxyCredentialsError) Unwrap() error { return e.err }

// readProxyCredentials reads the username and the password of
// request.proxy_basic_auth_file. What they hold is never shown.
func readProxyCredentials(files model.ProxyBasicAuthFile) (username, password string, err error) {
	if username, err = ReadCredentialFile(files.Username); err != nil {
		return "", "", fmt.Errorf("reading request.proxy_basic_auth_file username file: %w", err)
	}
	if password, err = ReadCredentialFile(files.Password); err != nil {
		return "", "", fmt.Errorf("reading request.proxy_basic_auth_file password file: %w", err)
	}
	if username == "" || password == "" {
		return "", "", errors.New("request.proxy_basic_auth_file credential files must not be empty")
	}
	if err := CheckBasicUsername(username); err != nil {
		return "", "", fmt.Errorf("request.proxy_basic_auth_file username file %s %w", files.Username, err)
	}
	return username, password, nil
}

// CheckProxy refuses a request.proxy_url that names no proxy a request can
// be sent through, and a request.proxy_basic_auth_file without both its
// paths, whether a collector or a defaults block writes them. "",
// model.ProxyDirect and no files all pass.
func CheckProxy(proxyURL string, files *model.ProxyBasicAuthFile) error {
	if proxyURL != "" && proxyURL != model.ProxyDirect {
		if _, err := parseProxyURL(proxyURL); err != nil {
			return err
		}
	}
	if files != nil && (strings.TrimSpace(files.Username) == "" || strings.TrimSpace(files.Password) == "") {
		return errors.New("request.proxy_basic_auth_file requires username and password paths")
	}
	return nil
}

// ProxyURLPattern is the configuration's JSON Schema pattern for
// request.proxy_url: left out, direct, or a proxy's scheme, host and port,
// with no credentials and nothing after the port. The scheme may be in any
// case, as the exporter reads it (url.Parse lowers it). It is spelled with
// character classes rather than (?i), which JSON Schema's ECMA-262 patterns
// lack. The exporter refuses anything else that is wrong.
const ProxyURLPattern = `^$|^direct$|^([Hh][Tt][Tt][Pp][Ss]?|[Ss][Oo][Cc][Kk][Ss]5[Hh]?)://[^/?#@{}]+/?$`

// proxySchemes are the schemes request.proxy_url may have: those Go's
// transport speaks to a proxy.
var proxySchemes = []string{"http", "https", "socks5", "socks5h"}

// parseProxyURL reads a request.proxy_url other than "" and
// model.ProxyDirect: a scheme, a host and an optional port, and nothing
// else. Its errors name the URL only by its scheme and host
// (shownProxyURL), never quoting the credentials, path or query in it.
func parseProxyURL(raw string) (*url.URL, error) {
	if HasPathParams(raw) || strings.Contains(raw, "{{") {
		return nil, errors.New("request.proxy_url has a placeholder; the proxy is the configuration's alone, and no probe parameter sets it")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("request.proxy_url %s is not a URL", shownProxyURL(raw))
	}
	known := false
	for _, scheme := range proxySchemes {
		known = known || u.Scheme == scheme
	}
	switch {
	case !known:
		return nil, fmt.Errorf("request.proxy_url %s has the scheme %q; a proxy is reached over http, https, socks5 or socks5h, or the requests go direct with %q", shownProxyURL(raw), u.Scheme, model.ProxyDirect)
	case u.User != nil:
		return nil, fmt.Errorf("request.proxy_url %s carries credentials; write it without them, and give them in request.proxy_basic_auth_file, files the username and the password are read from", shownProxyURL(raw))
	case u.Hostname() == "":
		return nil, fmt.Errorf("request.proxy_url %s names no host", shownProxyURL(raw))
	case u.Opaque != "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.ForceQuery || u.Fragment != "":
		return nil, fmt.Errorf("request.proxy_url %s has more than a scheme, a host and a port, which is all a proxy is reached by", shownProxyURL(raw))
	}
	if port := u.Port(); port != "" {
		if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
			return nil, fmt.Errorf("request.proxy_url %s has the port %q; a port is from 1 to 65535", shownProxyURL(raw), port)
		}
	}
	u.Path = ""
	return u, nil
}

// shownProxyURL is a request.proxy_url as its refusal names it: its scheme
// and host alone, where nothing secret is written, or Redacted when it is
// no URL at all.
func shownProxyURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return Redacted
	}
	return (&url.URL{Scheme: u.Scheme, Host: u.Host}).String()
}

// proxyTLSRoots is what an https:// proxy's certificate is verified
// against, nil for the system's roots. Only tests set it.
var proxyTLSRoots *x509.CertPool

// proxyTLSDialer is the DialTLSContext of a transport with an https://
// proxy. Go's transport makes a TLS connection itself only for the first
// hop, which is always the proxy when a proxy is set. So the handshake here
// is with the proxy, against the system's roots and with no client
// certificate. The transport makes the target's own handshake, with
// request.tls, through the tunnel, as it does without a proxy. Without this
// dialer, the transport would greet the proxy with the target's TLS
// settings, showing it the target's client certificate and expecting the
// target's server_name.
//
// The connection is handed over as a plain net.Conn (proxyConn). Given a
// *tls.Conn, the transport would take it for the target's own connection.
// It would report the finished handshake to the request's trace as the
// request's TLS phase: a no-op of microseconds, leaving the target's real
// handshake through the tunnel untimed. It would also keep the proxy's
// connection state as the one the answer came on, which over plain HTTP is
// not a connection to the target at all (HTTPTrip).
func proxyTLSDialer(dial func(ctx context.Context, network, address string) (net.Conn, error), timeout time.Duration) func(ctx context.Context, network, address string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := dial(ctx, network, address)
		if err != nil {
			return nil, err
		}
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			host = address
		}
		tlsConn := tls.Client(conn, &tls.Config{ServerName: host, RootCAs: proxyTLSRoots, MinVersion: tls.VersionTLS12})
		handshake, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		if err := tlsConn.HandshakeContext(handshake); err != nil {
			_ = conn.Close()
			return nil, err
		}
		return proxyConn{tlsConn}, nil
	}
}

// proxyConn is the TLS connection to an https:// proxy as nothing but a
// net.Conn, so the transport neither times its handshake nor takes its
// state for the target's (proxyTLSDialer).
type proxyConn struct{ net.Conn }

// ProxyRefusedError is a proxy's refusal to tunnel to a target: its answer
// to the CONNECT of an https target other than 200. The status is the proxy's own words, which a
// probe's caller is not told (transportwithheld.go).
type ProxyRefusedError struct {
	// Target is the host and port the tunnel was asked for.
	Target string
	// Status is the proxy's status line, such as 403 Forbidden.
	Status string
}

func (e *ProxyRefusedError) Error() string {
	return fmt.Sprintf("the proxy refused the tunnel to %s: %s", e.Target, e.Status)
}

// refuseProxyConnect is every transport's OnProxyConnectResponse. A
// CONNECT answered with a status other than 200 fails as a
// ProxyRefusedError. Go's own error would be only the proxy's reason
// phrase, which neither says it came from the proxy nor can be told apart
// from any other error's text.
func refuseProxyConnect(_ context.Context, _ *url.URL, connect *http.Request, resp *http.Response) error {
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	return &ProxyRefusedError{Target: connect.Host, Status: resp.Status}
}
