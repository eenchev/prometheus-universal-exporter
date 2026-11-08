//go:build request_type_tcp

package fetch

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A tcp collector of request.connect_only checks that a port takes a
// connection: the scrape dials the target, through the target policy, makes
// the TLS handshake when the collector has a tls block, and closes, writing
// and reading nothing. Its answer is a JSON document of whether the port was
// open, how long the connection and the handshake took and the address the
// connection reached:
//
//	request:
//	  type: tcp
//	  connect_only: true
//	  ports: [22, 443, 5432]  # optional: these ports of the target's host, at once
//
// A port that refuses the connection, does not answer within
// connect_timeout or whose handshake fails is "open": false with the reason,
// an answer rather than a failed scrape, so that a rule reads a 0 or a 1 of
// every scrape, as dns answers a SERVFAIL. What fails the scrape is being
// unable to ask: a target the target lists refuse, a name that does not
// resolve, and a scrape cut short.
//
// With request.ports the target is a host alone, and each port of the list
// is dialed at once, each its own request of the debug report and the
// trace: the document has each in ports, keyed by its number, and open
// says whether every one was.

// The defaults and bounds of connect_only (validateTCPConnect).
const (
	// tcpDefaultConnectTimeout is connect_timeout when left out: a port
	// that takes longer to connect is counted closed.
	tcpDefaultConnectTimeout = 5 * time.Second
	tcpMinConnectTimeout     = 10 * time.Millisecond
	// tcpMaxPorts is the most ports of request.ports, each a connection
	// made at once.
	tcpMaxPorts = 64
	// tcpMethodConnect is the http_method label of a scrape of
	// connect_only.
	tcpMethodConnect = "CONNECT"
)

// The reasons a port is not open, the document's reason.
const (
	tcpClosedRefused     = "refused"
	tcpClosedTimeout     = "timeout"
	tcpClosedUnreachable = "unreachable"
	tcpClosedReset       = "reset"
	tcpClosedTLS         = "tls"
	tcpClosedError       = "error"
)

// validateTCPConnect checks connect_only, connect_timeout and ports:
// connect_only sends and reads nothing, so no key of what is sent, read or
// spoken goes with it, nor udp, which has no connection to make, nor
// unix_socket, which has no port; connect_timeout is at least
// tcpMinConnectTimeout, and ports, which only connect_only dials, are at
// most tcpMaxPorts distinct numbers from 1 to 65535.
func validateTCPConnect(x *model.Collector, r TCPConfig) error {
	if !r.ConnectOnly {
		switch {
		case len(r.Ports) > 0:
			return fmt.Errorf("collector %q sets request.ports without request.connect_only; the ports are dialed to check that they take a connection, which connect_only: true does", x.Name)
		case r.ConnectTimeout != 0:
			return fmt.Errorf("collector %q sets request.connect_timeout without request.connect_only; a scrape that sends and reads is bounded by its deadline, and connect_timeout is how long connect_only waits for a port", x.Name)
		}
		return nil
	}
	for _, key := range []struct {
		name string
		set  bool
	}{
		{"send", r.Send != ""},
		{"send_hex", r.SendHex != ""},
		{"steps", len(r.Steps) > 0},
		{"greeting", r.Greeting != ""},
		{"terminator", r.Terminator != ""},
		{"answer_bytes", r.AnswerBytes != 0},
		{"starttls", x.Request.StartTLS != ""},
	} {
		if key.set {
			return fmt.Errorf("collector %q sets request.connect_only and request.%s; connect_only makes the connection, and its TLS handshake with a tls block, and closes it, writing and reading nothing, so leave %s out", x.Name, key.name, key.name)
		}
	}
	switch {
	case r.UDP:
		return fmt.Errorf("collector %q sets request.connect_only and request.udp; UDP has no connection to make, and a UDP port answers only a datagram it understands, so leave udp out", x.Name)
	case x.Request.UnixSocket != "":
		return fmt.Errorf("collector %q sets request.connect_only and request.unix_socket; connect_only checks a port of the target's host, so leave unix_socket out", x.Name)
	case r.ConnectTimeout != 0 && time.Duration(r.ConnectTimeout) < tcpMinConnectTimeout:
		return fmt.Errorf("collector %q request.connect_timeout is %s; it is how long a connection may take before the port counts as not open, at least %s", x.Name, time.Duration(r.ConnectTimeout), tcpMinConnectTimeout)
	case len(r.Ports) > tcpMaxPorts:
		return fmt.Errorf("collector %q request.ports lists %d ports, more than the %d a scrape dials at once; split them between collectors", x.Name, len(r.Ports), tcpMaxPorts)
	}
	for i, port := range r.Ports {
		if port < 1 || port > 65535 {
			return fmt.Errorf("collector %q request.ports[%d] is %d; want a port from 1 to 65535", x.Name, i, port)
		}
		if slices.Contains(r.Ports[:i], port) {
			return fmt.Errorf("collector %q request.ports lists %d twice; each port is dialed once and is one key of the document", x.Name, port)
		}
	}
	return nil
}

// parseTCPTargetOf reads a target of c: host:port (parseTCPTarget), or,
// for a collector of request.ports, the host alone, whose ports are the
// list's.
func parseTCPTargetOf(c *model.Collector, raw string) (tcpAddress, error) {
	if len(tcpRequest(c).Ports) == 0 {
		return parseTCPTarget(raw, c.Request.UnixSocket == "")
	}
	// An IPv6 address, in brackets or not, which parseTCPTarget reads as
	// host:port, or as no target when not.
	if bare := strings.Trim(strings.TrimSpace(raw), "[]"); strings.Contains(bare, ":") {
		if v6, err := netip.ParseAddr(bare); err == nil && v6.Is6() {
			host, err := canonicalHost(bare)
			return tcpAddress{host: host, hostPort: host}, err
		}
	}
	a, err := parseTCPTarget(raw, false)
	if err != nil {
		return a, err
	}
	if a.hostPort != a.host {
		return tcpAddress{}, fmt.Errorf("target %q names a port; the collector's request.ports are the ports dialed, so the target is the host alone, such as db.example or [2001:db8::1]", raw)
	}
	return a, nil
}

// tcpPort is what a connection to one port found: whether the port was
// open, why not when it was not, and of a connection made, the address it
// reached and how long it and its handshake took.
type tcpPort struct {
	Open           bool     `json:"open"`
	Reason         string   `json:"reason,omitempty"`
	Error          string   `json:"error,omitempty"`
	Address        string   `json:"address,omitempty"`
	ConnectSeconds *float64 `json:"connect_seconds,omitempty"`
	TLSSeconds     *float64 `json:"tls_seconds,omitempty"`
	TLSVersion     string   `json:"tls_version,omitempty"`
}

// tcpConnectDocument is a scrape of connect_only as the transforms read
// it: the port's fields, or, with request.ports, open, whether every port
// was, open_ports, how many were, and ports, each port's, keyed by its
// number.
type tcpConnectDocument struct {
	tcpPort
	OpenPorts       *int               `json:"open_ports,omitempty"`
	Ports           map[string]tcpPort `json:"ports,omitempty"`
	DurationSeconds float64            `json:"duration_seconds"`
}

// outcome is how a debug report tells what the connection found.
func (p tcpPort) outcome() string {
	if !p.Open {
		return "not open, " + p.Reason + ": " + p.Error
	}
	outcome := "open, connected to " + p.Address
	if p.TLSVersion != "" {
		outcome += ", began " + p.TLSVersion
	}
	return outcome
}

// facts are what a span tells of the connection.
func (p tcpPort) facts() []TraceFact {
	facts := []TraceFact{boolFact("tcp.open", p.Open)}
	if p.Reason != "" {
		facts = append(facts, nameFact("tcp.closed.reason", p.Reason))
	}
	return facts
}

// tcpConnectDeadline is when a scrape's connections, and their handshakes,
// end: connect_timeout from now, and within the scrape's deadline nine
// tenths of the time left to it, so that a port that never answers is
// counted closed with the time left to answer the document.
func tcpConnectDeadline(ctx context.Context, r TCPConfig) time.Time {
	timeout := time.Duration(r.ConnectTimeout)
	if timeout == 0 {
		timeout = tcpDefaultConnectTimeout
	}
	now := time.Now()
	deadline := now.Add(timeout)
	if scrape, ok := ctx.Deadline(); ok {
		if within := now.Add(scrape.Sub(now) * 9 / 10); within.Before(deadline) {
			deadline = within
		}
	}
	return deadline
}

// fetchTCPConnect makes a scrape of connect_only: the connection to the
// target, or to each of request.ports of its host at once, and the JSON
// document of what each found.
func fetchTCPConnect(ctx context.Context, start time.Time, target string, address tcpAddress, cfg *tls.Config, c *model.Collector) (*HTTPResponse, error) {
	r := tcpRequest(c)
	deadline := tcpConnectDeadline(ctx, r)
	traced := http.Header{"Tcp-Connect-Timeout": {time.Until(deadline).Round(time.Millisecond).String()}}
	var doc tcpConnectDocument
	if len(r.Ports) == 0 {
		traceRequest(ctx, tcpMethodConnect, tcpURL(address, c), traced, "", false)
		timedCtx, timing := withConnTiming(ctx)
		port, err := tcpConnect(timedCtx, address.hostPort, cfg, deadline)
		tracePhases(ctx, timing.phases())
		if err != nil {
			traceFailedOutcome(ctx, "error: "+err.Error())
			return nil, err
		}
		traceOutcome(ctx, port.outcome())
		traceFacts(ctx, port.facts()...)
		doc.tcpPort = port
	} else {
		ports := make([]tcpPort, len(r.Ports))
		errs := make([]error, len(r.Ports))
		var wg sync.WaitGroup
		for i, number := range r.Ports {
			dialed := tcpAddress{host: address.host, hostPort: net.JoinHostPort(address.host, strconv.Itoa(number))}
			at := traceRequestAt(ctx, tcpMethodConnect, tcpURL(dialed, c), traced, "", false)
			wg.Add(1)
			go func() {
				defer wg.Done()
				timedCtx, timing := withConnTiming(ctx)
				ports[i], errs[i] = tcpConnect(timedCtx, dialed.hostPort, cfg, deadline)
				at.phases(timing.phases())
				if errs[i] != nil {
					at.outcome("error: "+errs[i].Error(), true)
					return
				}
				at.outcome(ports[i].outcome(), false)
				at.facts(ports[i].facts()...)
			}()
		}
		wg.Wait()
		// The first port's failure: a name that does not resolve, or a
		// host the target lists refuse, fails every port alike.
		for _, err := range errs {
			if err != nil {
				return nil, err
			}
		}
		open := 0
		doc.Ports = make(map[string]tcpPort, len(ports))
		for i, port := range ports {
			doc.Ports[strconv.Itoa(r.Ports[i])] = port
			if port.Open {
				open++
			}
		}
		doc.Open, doc.OpenPorts = open == len(ports), &open
	}
	doc.DurationSeconds = time.Since(start).Seconds()
	body, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	return &HTTPResponse{
		StatusCode: http.StatusOK,
		NoStatus:   true,
		Headers:    http.Header{"Content-Type": {"application/json"}},
		Body:       body,
		Target:     target,
		Collector:  c.Name,
		Duration:   time.Since(start),
	}, nil
}

// tcpConnect connects to hostPort, through the target policy, makes the TLS
// handshake when cfg is set, and closes the connection, both ending at
// deadline. A port that refuses, does not answer by deadline, or whose
// handshake fails is not open, with the reason; the error is what fails the
// scrape: a refusal of the target lists, a name that does not resolve, and
// ctx ended before the deadline.
func tcpConnect(ctx context.Context, hostPort string, cfg *tls.Config, deadline time.Time) (tcpPort, error) {
	dialCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	began := time.Now()
	conn, err := policyDialer((&net.Dialer{}).DialContext)(dialCtx, "tcp", hostPort)
	if err != nil {
		if failed := tcpConnectFailed(ctx, hostPort, err); failed != nil {
			return tcpPort{}, failed
		}
		return tcpPort{Reason: tcpClosedReason(err), Error: err.Error()}, nil
	}
	defer func() { _ = conn.Close() }()
	seconds := func(d time.Duration) *float64 { s := d.Seconds(); return &s }
	port := tcpPort{Open: true, Address: conn.RemoteAddr().String(), ConnectSeconds: seconds(time.Since(began))}
	if cfg == nil {
		return port, nil
	}
	_ = conn.SetDeadline(deadline)
	began = time.Now()
	tlsConn := tls.Client(conn, cfg)
	if err := connTimingOf(ctx).handshake("tls", func() error { return tlsConn.HandshakeContext(dialCtx) }); err != nil {
		if ctx.Err() != nil {
			return tcpPort{}, tcpFailure(ctx, fmt.Errorf("TLS handshake with %s: %w", hostPort, err))
		}
		reason := tcpClosedTLS
		if tcpTimedOut(err) {
			reason = tcpClosedTimeout
		}
		port.Open, port.Reason, port.Error = false, reason, fmt.Sprintf("TLS handshake with %s: %v", hostPort, err)
		return port, nil
	}
	port.TLSSeconds, port.TLSVersion = seconds(time.Since(began)), tls.VersionName(tlsConn.ConnectionState().Version)
	return port, nil
}

// tcpConnectFailed is the error a failed dial fails the scrape with, nil
// when it is an answer, a port not open: a refusal of the target lists, a
// name that does not resolve, and the scrape's ctx ended, cut short or past
// its deadline before connect_timeout's.
func tcpConnectFailed(ctx context.Context, hostPort string, err error) error {
	if errors.Is(err, ErrTargetRefused) {
		return err
	}
	if ctx.Err() != nil {
		return tcpFailure(ctx, fmt.Errorf("connecting to %s: %w", hostPort, err))
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) && !dnsErr.IsTimeout {
		return fmt.Errorf("connecting to %s: %w", hostPort, err)
	}
	return nil
}

// tcpClosedReason is why a dial found the port not open.
func tcpClosedReason(err error) string {
	switch {
	case tcpTimedOut(err):
		return tcpClosedTimeout
	case errors.Is(err, syscall.ECONNREFUSED):
		return tcpClosedRefused
	case errors.Is(err, syscall.EHOSTUNREACH), errors.Is(err, syscall.ENETUNREACH):
		return tcpClosedUnreachable
	case errors.Is(err, syscall.ECONNRESET):
		return tcpClosedReset
	}
	return tcpClosedError
}

// tcpTimedOut says whether err is of a deadline, connect_timeout's.
func tcpTimedOut(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}
