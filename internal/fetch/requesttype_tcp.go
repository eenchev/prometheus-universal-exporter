//go:build request_type_tcp

package fetch

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The tcp request type reads a service that answers a line of text over a
// TCP connection with its statistics, as text: memcached's stats, a
// ZooKeeper four-letter word, HAProxy's runtime API, NUT's upsd,
// Beanstalkd's stats. The answer is handed to the decoder and the
// transforms as an http answer's body is, so text, regex, csv, prometheus,
// json or yaml read it as each would read a body.
//
//	request:
//	  type: tcp
//	  send: "stats\r\n"     # optional: written once the connection is made
//	  terminator: "END\r\n" # optional: the answer ends with it; else at the close
//
// The probe's target is host:port or [v6]:port, the port required, since no
// port is any such service's; a tls block makes the scrape speak TLS. The
// exporter dials the target through the target policy, as every type that
// reaches a server does, and makes one connection for each scrape, closed
// when the scrape is over: these services answer one command on a
// connection, several close it after the answer, and a connection kept
// between scrapes would need a keepalive, an idle timeout and a limit of
// its own.
//
// send may hold {{param_name}} placeholders, filled from the probe. A value
// may hold only letters, digits and . _ - : / @ + = , % (lineValue), and
// any other is refused before the connection is made: a line break would
// end the line the configuration wrote, but so would less, since these
// servers split a line on more than its end. HAProxy's runtime API runs
// each ';'-separated command of a line, and memcached, ZooKeeper and NUT
// split arguments on blanks, so a value with a ';' or a blank could add a
// command or an argument of the probe's.
//
// Reading stops once the answer ends with terminator, which stays in the
// body, or, without one, when the server closes the connection. Either way
// the answer is held to the response limit as it arrives, and the scrape's
// deadline bounds the connection, the handshake, the write and the read: a
// server that keeps the connection open without the terminator fails the
// scrape when the deadline comes.
//
// The type is opt-in, built under its tag alone, and imports nothing
// outside the standard library.

func init() {
	registerRequestType(&RequestType{
		Name:         RequestTypeTCP,
		Fields:       []string{"send", "terminator", "tls", "max_response_bytes", "allowed_targets", "denied_targets"},
		Overrides:    []string{"timeout", PathParamPrefix},
		TargetFields: []string{"timeout"},
		Validate:     validateTCPRequest,
		Fetch:        fetchTCP,
		CheckTarget: func(_ *model.Collector, target string, _ bool) error {
			_, err := parseTCPTarget(target)
			return err
		},
		Label:      tcpLabel,
		Method:     tcpMethod,
		Display:    tcpDisplay,
		Stage:      "tcp",
		CheckFiles: checkTCPFiles,
	})
}

// tcpTimeout bounds a scrape with no deadline of its own.
const tcpTimeout = 30 * time.Second

// tcpMaxText is the most bytes of send and of terminator: a command line
// of these protocols is a few words, and the terminator a line.
const tcpMaxText = 64 << 10

// The http_method labels of a scrape: SEND when it writes send, READ when it
// only reads what the server says first.
const (
	tcpMethodSend = "SEND"
	tcpMethodRead = "READ"
)

func validateTCPRequest(x *model.Collector) error {
	r := &x.Request
	t := r.TLS
	if (t.CertFile == "") != (t.KeyFile == "") {
		return fmt.Errorf("collector %q request.tls sets only one of cert_file and key_file; a client certificate needs both", x.Name)
	}
	if t.ServerName != "" {
		if _, err := canonicalHost(t.ServerName); err != nil {
			return fmt.Errorf("collector %q request.tls.server_name %q: %w", x.Name, t.ServerName, err)
		}
	}
	if len(r.Send) > tcpMaxText {
		return fmt.Errorf("collector %q request.send is %d bytes, more than the %d a tcp collector may send; it is a command, such as \"stats\\r\\n\"", x.Name, len(r.Send), tcpMaxText)
	}
	if len(r.Terminator) > tcpMaxText {
		return fmt.Errorf("collector %q request.terminator is %d bytes, more than the %d it may be; it is what the answer ends with, such as \"END\\r\\n\"", x.Name, len(r.Terminator), tcpMaxText)
	}
	if HasPathParams(r.Terminator) {
		return fmt.Errorf("collector %q request.terminator %q has a placeholder; the end of the answer is matched as it is written", x.Name, r.Terminator)
	}
	placeholders, err := (templateField{"request.send", r.Send, "line"}).parse()
	if err != nil {
		return fmt.Errorf("collector %q %w", x.Name, err)
	}
	// A default is held to what a probe's value is, here rather than at
	// every scrape that leaves the parameter out.
	for _, p := range placeholders {
		if character, refused := lineValueRefused(p.Default); p.HasDefault && refused {
			return fmt.Errorf("collector %q request.send: the default of %s holds %s; %s", x.Name, p.Name, character, lineValueRule)
		}
	}
	return nil
}

// tcpAddress is a target as a scrape reaches it: host as the policy
// matches it and TLS verifies it, hostPort what is dialed.
type tcpAddress struct {
	host, hostPort string
}

// parseTCPTarget reads a target: host:port or [v6]:port, the port required.
// Nothing else may come with it.
func parseTCPTarget(raw string) (tcpAddress, error) {
	target := strings.TrimSpace(raw)
	if strings.Contains(target, "://") {
		scheme, _, _ := strings.Cut(target, "://")
		return tcpAddress{}, fmt.Errorf("target %q has the scheme %s://; a tcp target is host:port, and request.tls makes the scrape speak TLS", raw, scheme)
	}
	if strings.ContainsAny(target, "/?#@") {
		return tcpAddress{}, fmt.Errorf("target %q has a path, query or user in it; a tcp target is only the host and its port", raw)
	}
	host, port, err := net.SplitHostPort(target)
	if err != nil {
		return tcpAddress{}, fmt.Errorf("target %q is not host:port; a tcp target names its port, which no default stands in for, and an IPv6 address in brackets, as [2001:db8::1]:11211", raw)
	}
	if host == "" {
		return tcpAddress{}, fmt.Errorf("target %q names no host; a tcp target is host:port", raw)
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 || strconv.Itoa(n) != port {
		return tcpAddress{}, fmt.Errorf("target %q has the port %q; want a number from 1 to 65535", raw, port)
	}
	canonical, err := canonicalHost(host)
	if err != nil {
		return tcpAddress{}, err
	}
	return tcpAddress{host: canonical, hostPort: net.JoinHostPort(canonical, port)}, nil
}

// tcpSecure says whether a scrape of c speaks TLS: when it writes a tls
// block, even an empty one (model.TLSConfig.Set), which asks for TLS
// verified with the system roots rather than leaving the scrape plaintext.
func tcpSecure(c *model.Collector) bool {
	return c.Request.TLS != model.TLSConfig{}
}

// tcpURL is the url label and the trace's URL of a scrape: tcp://host:port,
// or tls:// over TLS.
func tcpURL(a tcpAddress, secure bool) string {
	if secure {
		return "tls://" + a.hostPort
	}
	return "tcp://" + a.hostPort
}

func tcpLabel(target string, c *model.Collector, _ RequestOverrides) (string, error) {
	a, err := parseTCPTarget(target)
	if err != nil {
		return "", err
	}
	return tcpURL(a, tcpSecure(c)), nil
}

func tcpMethod(c *model.Collector, _ RequestOverrides) string {
	if c.Request.Send == "" {
		return tcpMethodRead
	}
	return tcpMethodSend
}

// tcpDisplay renders a target for logs and labels: as written, since it
// holds no credentials, when it is one at all.
func tcpDisplay(target string) string {
	if _, err := parseTCPTarget(target); err != nil {
		return "<invalid target>"
	}
	return strings.TrimSpace(target)
}

// fetchTCP makes a scrape's connection, writes send and answers what the
// server wrote back.
func fetchTCP(ctx context.Context, target string, c *model.Collector, overrides RequestOverrides, _ http.Header) (*HTTPResponse, error) {
	start := time.Now()
	address, err := parseTCPTarget(target)
	if err != nil {
		return nil, err
	}
	send, err := templateField{"request.send", c.Request.Send, "line"}.render(overrides.Params)
	if err != nil {
		return nil, err
	}
	if overrides.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, overrides.Timeout)
		defer cancel()
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, tcpTimeout)
		defer cancel()
	}
	secure := tcpSecure(c)
	var cfg *tls.Config
	if secure {
		if cfg, err = tlsConfig(c.Request.TLS); err != nil {
			return nil, fmt.Errorf("request.tls: %w", err)
		}
		if cfg.ServerName == "" {
			cfg.ServerName = address.host
		}
	}
	// The host's name now; the address the connection is made to when it
	// is made, as an http request's connections are checked.
	if policy := policyOf(c); policy != nil {
		guard := &policyGuard{policy: policy, hosts: map[string]bool{}}
		if err := guard.check(ctx, &url.URL{Scheme: "tcp", Host: address.hostPort}); err != nil {
			return nil, err
		}
		ctx = context.WithValue(ctx, policyGuardKey{}, guard)
	}
	label := tcpURL(address, secure)
	traceRequest(ctx, tcpMethod(c, overrides), label, http.Header{"Tcp-Send": {tcpShown(send)}}, "", false)
	body, how, err := tcpExchange(ctx, address, cfg, send, c)
	if err != nil {
		traceOutcome(ctx, "error: "+err.Error())
		return nil, err
	}
	traceOutcome(ctx, fmt.Sprintf("read %d bytes, %s", len(body), how))
	return &HTTPResponse{
		StatusCode: http.StatusOK,
		NoStatus:   true,
		// No Content-Type: the decoder is decoder.type, or what the body
		// reads as, as for a file whose extension says nothing.
		Headers:   http.Header{},
		Body:      body,
		Target:    target,
		Collector: c.Name,
		Duration:  time.Since(start),
	}, nil
}

// tcpExchange connects, speaks TLS when cfg is set, writes send and reads
// the answer, saying how reading stopped.
func tcpExchange(ctx context.Context, address tcpAddress, cfg *tls.Config, send string, c *model.Collector) ([]byte, string, error) {
	dialer := &net.Dialer{}
	conn, err := policyDialer(dialer.DialContext)(ctx, "tcp", address.hostPort)
	if err != nil {
		if !errors.Is(err, ErrTargetRefused) {
			err = tcpFailure(ctx, fmt.Errorf("connecting to %s: %w", address.hostPort, err))
		}
		return nil, "", err
	}
	defer func() { _ = conn.Close() }()
	// The write and every read end at the scrape's deadline, or as soon as
	// it is cancelled.
	deadline, _ := ctx.Deadline()
	_ = conn.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Unix(1, 0)) })
	defer stop()
	if cfg != nil {
		tlsConn := tls.Client(conn, cfg)
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			return nil, "", tcpFailure(ctx, fmt.Errorf("TLS handshake with %s: %w", address.hostPort, err))
		}
		conn = tlsConn
	}
	if send != "" {
		if _, err := io.WriteString(conn, send); err != nil {
			return nil, "", tcpFailure(ctx, fmt.Errorf("sending request.send to %s: %w", address.hostPort, err))
		}
	}
	return tcpRead(ctx, conn, address, []byte(c.Request.Terminator), responseLimit(c))
}

// tcpRead reads the answer from conn until it ends with terminator, or,
// without one, until the server closes the connection, holding it to limit
// as it arrives.
func tcpRead(ctx context.Context, conn net.Conn, address tcpAddress, terminator []byte, limit int64) ([]byte, string, error) {
	var body []byte
	chunk := make([]byte, 32<<10)
	for {
		n, err := conn.Read(chunk)
		if n > 0 {
			if int64(len(body))+int64(n) > limit {
				return nil, "", model.MarkError(model.Errorf("the answer of %s is larger than the response limit of %d bytes; raise request.max_response_bytes or limits.max_response_bytes", address.hostPort, model.Size(limit)), model.ErrLimitExceeded)
			}
			body = append(body, chunk[:n]...)
			if len(terminator) > 0 && bytes.HasSuffix(body, terminator) {
				return body, "the answer ended with the terminator " + strconv.Quote(string(terminator)), nil
			}
		}
		switch {
		case err == nil:
			continue
		case errors.Is(err, io.EOF) && len(terminator) == 0:
			return body, "the server closed the connection", nil
		case errors.Is(err, io.EOF):
			return nil, "", fmt.Errorf("the server closed the connection after %d bytes, and the answer does not end with request.terminator %q", len(body), terminator)
		case len(terminator) > 0:
			return nil, "", tcpFailure(ctx, fmt.Errorf("reading from %s: no answer ending with request.terminator %q came, %d bytes read: %w", address.hostPort, terminator, len(body), err))
		}
		return nil, "", tcpFailure(ctx, fmt.Errorf("reading from %s: the server kept the connection open, %d bytes read; without request.terminator the answer ends when the server closes it: %w", address.hostPort, len(body), err))
	}
}

// tcpShown is what a debug report shows of the bytes sent: quoted, so a
// carriage return and a line feed are seen, and no longer than a failure.
func tcpShown(send string) string {
	return model.CutTo(strconv.Quote(send), model.MaxFailureBytes, false)
}

// tcpFailure is the failure of a connection, a handshake, the write or a
// read: the scrape's deadline or cancellation when that is what ended it,
// since the connection's deadline is set to make it end. The connection's
// deadline is the scrape's, and its timer can fire before the context's
// own, so a connection that timed out is the deadline's failure whether or
// not the context says so yet.
func tcpFailure(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil && !errors.Is(err, ctxErr) {
		return fmt.Errorf("%w (%w)", ctxErr, err)
	}
	if errors.Is(err, os.ErrDeadlineExceeded) && !errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%w (%w)", context.DeadlineExceeded, err)
	}
	return err
}

// checkTCPFiles reads the tls block's files as a scrape reads them, for
// --dry-run. Loading the configuration reads none of them, since a file may
// be mounted after the exporter starts.
func checkTCPFiles(c *model.Collector, t *model.StaticTarget) []string {
	if t != nil || !tcpSecure(c) {
		return nil
	}
	if _, err := tlsConfig(c.Request.TLS); err != nil {
		return []string{fmt.Sprintf("collector %q: request.tls: %v; a scrape would fail on it now (the files are read at each connection, so one put in place later is used then)", c.Name, err)}
	}
	return nil
}
