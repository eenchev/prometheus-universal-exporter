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

// An http or graphite collector may name the proxy its requests go through,
// request.proxy_url, in place of the one the environment names for the whole
// exporter (transport.go): a target only reachable through a proxy of its
// own network, or one that must be reached directly although the rest of the
// exporter's traffic goes through a proxy (model.ProxyDirect). Every request
// of the collector goes through it, its login's, its redirects' and its
// pages' included, whatever its host: there is no NO_PROXY of a collector,
// and localhost goes through it too.
//
// The proxy is part of the transport's settings, so collectors with
// different proxies, or different credentials for one, never share a
// connection pool, and a reload that changes the proxy gets a pool of its
// own. Its credentials come from files (request.proxy_basic_auth_file), read
// on every request, and never from the URL, which the configuration shows
// to whoever reads it, and --dry-run and a debug report with it.
//
// An http:// proxy is asked in plain text: it is sent an http target's
// request whole, and asked to tunnel to an https target (CONNECT). An
// https:// proxy is asked the same over TLS, verified against the system's
// roots alone: request.tls is the target's, and a client certificate meant
// for the target is not shown to the proxy. A socks5:// (or socks5h://)
// proxy is asked to connect to the target by name, which it resolves.

// withProxy is settings with c's request.proxy_url and the files of its
// credentials.
func withProxy(settings TransportSettings, c *model.Collector) TransportSettings {
	settings.proxy = c.Request.ProxyURL
	if files := c.Request.ProxyBasicAuthFile; files != nil {
		settings.proxyAuth = *files
	}
	return settings
}

// transportProxy is the proxy function of a transport built for settings:
// the environment's when the collector names none, nil, which is no proxy,
// for model.ProxyDirect, and otherwise the collector's, with the credentials
// its files hold as they are when the request is sent.
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

// proxyCredentialsError is a failure to read the proxy's credentials, which
// Go's transport returns as the request's own: the files are read again by
// a retry, which would wait out the backoff only to fail as the first
// attempt did, so the request ends at once (fetcher.go), as one whose
// basic_auth_file cannot be read does before it is sent.
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
// paths, whether a collector or a defaults block writes them; "" and
// model.ProxyDirect, and no files, pass.
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

// ProxyURLPattern is what request.proxy_url may be, for the configuration's
// JSON Schema: left out, direct, or a proxy's scheme, host and port, with
// no credentials and nothing after the port. The scheme is of any case, as
// the exporter reads it (url.Parse lowers it); it is spelled in classes
// rather than with (?i), which JSON Schema's ECMA-262 patterns do not have.
// What else it has to be is the exporter's to refuse.
const ProxyURLPattern = `^$|^direct$|^([Hh][Tt][Tt][Pp][Ss]?|[Ss][Oo][Cc][Kk][Ss]5[Hh]?)://[^/?#@{}]+/?$`

// proxySchemes are the schemes request.proxy_url may have: those Go's
// transport speaks to a proxy.
var proxySchemes = []string{"http", "https", "socks5", "socks5h"}

// parseProxyURL reads a request.proxy_url other than "" and
// model.ProxyDirect: a scheme, a host and an optional port, and nothing
// else. Its errors name it by its scheme and host alone (shownProxyURL),
// never quoting the credentials, path or query written in it.
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

// proxyTLSDialer is the DialTLSContext of a transport whose proxy is an
// https:// one. Go's transport makes a TLS connection itself only for the
// first hop, which with a proxy always set is the proxy: the handshake here
// is the proxy's, against the system's roots and with no client
// certificate, and the target's own handshake, with request.tls, is made
// through the tunnel by the transport as without a proxy. Without this the
// transport would greet the proxy with the target's TLS settings, showing it
// the target's client certificate and expecting the target's server_name.
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
		return tlsConn, nil
	}
}

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

// refuseProxyConnect is every transport's OnProxyConnectResponse: a CONNECT
// answered other than 200 fails as a ProxyRefusedError, where Go's own
// error would be the proxy's reason phrase alone, which neither says it is
// the proxy's nor can be told apart from any other error's text.
func refuseProxyConnect(_ context.Context, _ *url.URL, connect *http.Request, resp *http.Response) error {
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	return &ProxyRefusedError{Target: connect.Host, Status: resp.Status}
}
