//go:build request_type_smtp || request_type_modbus || request_type_mqtt || request_type_websocket || request_type_ftp || request_type_imap

package fetch

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// What the smtp, modbus, mqtt, websocket and ftp request types share: each
// reaches one server at host:port over TCP, or over TLS on it, through the
// collector's target policy, within the scrape's deadline, on a connection
// of the scrape's own. The types are opt-in, so this file is in no build
// that has none of them.

// netAddress is a target of one of these types: its scheme, in lower case
// and "" when the target has none, the host as the target policy judges it,
// host:port, and what follows the port, a path and a query, as written.
type netAddress struct {
	scheme   string
	host     string
	hostPort string
	rest     string
}

// parseNetTarget reads a target written as host, host:port, [v6]:port or
// scheme://host:port, with a path after it when withRest says the type has
// a use for one. ports gives each scheme the type takes its default port;
// "" is the port of a target without a scheme. what names the type in the
// refusals.
func parseNetTarget(what, raw string, ports map[string]int, withRest bool) (netAddress, error) {
	var a netAddress
	target := strings.TrimSpace(raw)
	// The forms a refusal names, the same in every refusal.
	written := []string{"host", "host:port"}
	for _, scheme := range model.SortedKeys(ports) {
		if scheme != "" {
			written = append(written, scheme+"://host:port")
		}
	}
	article := "a"
	if strings.ContainsRune("fims", rune(what[0])) {
		// ftp, imap, mqtt and smtp are said by their letters.
		article = "an"
	}
	forms := article + " " + what + " target is " + strings.Join(written[:len(written)-1], ", ") + " or " + written[len(written)-1]
	if target == "" {
		return a, fmt.Errorf("the target is empty; %s", forms)
	}
	if scheme, rest, found := strings.Cut(target, "://"); found {
		a.scheme = strings.ToLower(scheme)
		if _, known := ports[a.scheme]; !known || a.scheme == "" {
			return a, fmt.Errorf("target %q has the scheme %s://; %s", raw, scheme, forms)
		}
		target = rest
	}
	if i := strings.IndexAny(target, "/?#"); i >= 0 {
		target, a.rest = target[:i], target[i:]
	}
	if strings.Contains(a.rest, "#") {
		return a, fmt.Errorf("target %q has a fragment; %s", raw, forms)
	}
	if a.rest != "" && a.rest != "/" && !withRest {
		return a, fmt.Errorf("target %q has a path or a query; %s, and nothing after it", raw, forms)
	}
	if strings.Contains(target, "@") {
		return a, fmt.Errorf("target %q has a user in it; the account is request.username, and its password request.password_file", raw)
	}
	host, port := target, strconv.Itoa(ports[a.scheme])
	if h, p, err := net.SplitHostPort(target); err == nil {
		host, port = h, p
	} else if strings.HasPrefix(target, "[") && strings.HasSuffix(target, "]") {
		host = target[1 : len(target)-1]
	} else if strings.Count(target, ":") == 1 {
		return a, fmt.Errorf("target %q is not host or host:port; write an IPv6 address in brackets, as [2001:db8::1]:%d", raw, ports[a.scheme])
	}
	if host == "" {
		return a, fmt.Errorf("target %q names no host; %s", raw, forms)
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 || strconv.Itoa(n) != port {
		return a, fmt.Errorf("target %q has the port %q; want a number from 1 to 65535", raw, port)
	}
	canonical, err := canonicalHost(host)
	if err != nil {
		return a, err
	}
	a.host, a.hostPort = canonical, net.JoinHostPort(canonical, port)
	return a, nil
}

// netBudget is ctx held to the probe's timeout, or to fallback when neither
// the probe nor its caller set a deadline.
func netBudget(ctx context.Context, overrides RequestOverrides, fallback time.Duration) (context.Context, context.CancelFunc) {
	cancels := []context.CancelFunc{}
	if overrides.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, overrides.Timeout)
		cancels = append(cancels, cancel)
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, fallback)
		cancels = append(cancels, cancel)
	}
	return ctx, func() {
		for _, cancel := range cancels {
			cancel()
		}
	}
}

// netGuard holds a scrape of c to the collector's target lists: hostPort's
// name now, and the address each connection is made to when it is made
// (netDial), as an http request's connections are checked.
func netGuard(ctx context.Context, c *model.Collector, scheme, hostPort string) (context.Context, error) {
	policy := policyOf(c)
	if policy == nil {
		return ctx, nil
	}
	guard := &policyGuard{policy: policy, hosts: map[string]bool{}}
	if err := guard.check(ctx, &url.URL{Scheme: scheme, Host: hostPort}); err != nil {
		return ctx, err
	}
	return context.WithValue(ctx, policyGuardKey{}, guard), nil
}

// netConn is a connection of a scrape: every read and write on it ends at
// the scrape's deadline, or as soon as the scrape is cancelled.
type netConn struct {
	net.Conn
	r    *bufio.Reader
	stop func() bool
}

// close ends the connection.
func (c *netConn) close() {
	c.stop()
	_ = c.Conn.Close() //nolint:staticcheck // the connection's own Close, by name beside close
}

// secure begins TLS on the connection, cfg's server name host unless the
// tls block names one. Nothing may be buffered: bytes that came before the
// handshake came in plaintext, where anyone on the way could have written
// them, and would be read as the server's (CVE-2011-0411 is the server's
// side of the same).
func (c *netConn) secure(ctx context.Context, cfg *tls.Config, host, hostPort string) error {
	if n := c.r.Buffered(); n > 0 {
		return fmt.Errorf("the server %s wrote %d bytes after it said to begin TLS and before the handshake; they came in plaintext, where anyone on the way could have written them, so the scrape does not read them as the server's", hostPort, n)
	}
	own := cfg.Clone()
	if own.ServerName == "" {
		own.ServerName = host
	}
	secured := tls.Client(c.Conn, own)
	if err := secured.HandshakeContext(ctx); err != nil {
		return netFailure(ctx, fmt.Errorf("TLS handshake with %s: %w", hostPort, err))
	}
	c.Conn, c.r = secured, bufio.NewReader(secured)
	return nil
}

// netDial connects to address through the target policy, and over TLS at
// once when cfg is given.
func netDial(ctx context.Context, address netAddress, cfg *tls.Config) (*netConn, error) {
	dialer := &net.Dialer{}
	conn, err := policyDialer(dialer.DialContext)(ctx, "tcp", address.hostPort)
	if err != nil {
		if !errors.Is(err, ErrTargetRefused) {
			err = netFailure(ctx, fmt.Errorf("connecting to %s: %w", address.hostPort, err))
		}
		return nil, err
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	c := &netConn{Conn: conn, r: bufio.NewReader(conn)}
	c.stop = context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Unix(1, 0)) })
	if cfg != nil {
		if err := c.secure(ctx, cfg, address.host, address.hostPort); err != nil {
			c.close()
			return nil, err
		}
	}
	return c, nil
}

// netFailure is the failure of a read, a write or a handshake: the scrape's
// deadline or cancellation when that is what ended it, since the
// connection's deadline is set to make it end, and its timer can fire
// before the context's own.
func netFailure(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil && !errors.Is(err, ctxErr) {
		return fmt.Errorf("%w (%w)", ctxErr, err)
	}
	if errors.Is(err, os.ErrDeadlineExceeded) && !errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%w (%w)", context.DeadlineExceeded, err)
	}
	return err
}
