//go:build request_type_tcp

package fetch

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/eenchev/prometheus-universal-exporter/internal/expr"
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
//	  greeting: "^220[ -]"  # optional: what the server says first must match it
//	  send: "stats\r\n"     # optional: written once the connection is made
//	  terminator: "END\r\n" # optional: the answer ends with it; else at the close
//
// A server that starts in plaintext and turns to TLS when asked, SMTP on
// 587, IMAP on 143, POP3 on 110, FTP on 21, is read with starttls: the
// greeting is read, the protocol's request for TLS spoken (startTLS), and
// the handshake made with the tls block's settings, or the defaults without
// one, before send is written over TLS.
//
// A service that speaks first — SMTP, FTP, POP3, IMAP, SSH — is checked
// with greeting: the greeting is read, and must match the pattern, before
// send is written, so the server never sees a command before it has
// greeted, as these protocols require; without send, the greeting is the
// answer. The greeting is its first line, or, where that line begins with a
// three-digit code and a '-' (SMTP's and FTP's continuation), every line up
// to the one that begins with the code and no '-' (tcpReadGreeting).
//
// How long each step took, and the greeting, are given to the transforms as
// the answer's headers (tcpHeaders), which jq and yq read as $headers and
// Python as response.headers: the body stays the answer the server wrote,
// so a collector that reads only the body sees what it saw before.
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

// TCPConfig is a tcp collector's own request keys: the pattern the
// server's greeting must match, no greeting being read when empty, the text
// written once the connection is made, or once the greeting has come,
// nothing when empty, what the answer ends with, the answer ending when
// the server closes the connection when it is empty, whether the answer is
// a JSON document of the exchange, and the protocol whose STARTTLS begins
// TLS on the plaintext connection, none when empty.
type TCPConfig struct {
	Greeting   string `yaml:"greeting,omitempty"`
	Send       string `yaml:"send,omitempty"`
	Terminator string `yaml:"terminator,omitempty"`
	Document   bool   `yaml:"document,omitempty"`
	StartTLS   string `yaml:"starttls,omitempty"`
}

// tcpRequest is a tcp collector's own request keys: the zero TCPConfig of
// a collector built in code whose request has not been validated, which
// sets them (ValidateRequest).
func tcpRequest(c *model.Collector) TCPConfig {
	own, _ := c.Request.Own.(TCPConfig)
	return own
}

func init() {
	registerRequestType(&RequestType{
		Name:         RequestTypeTCP,
		Fields:       []string{"tls", "max_response_bytes", "allowed_targets", "denied_targets"},
		Config:       TCPConfig{},
		KeyRules:     tcpKeyRules,
		Overrides:    []string{"timeout", PathParamPrefix},
		TargetFields: []string{"timeout", "tls"},
		Requester:    tcpRequester{},
		TargetHint:   func(*model.Collector) string { return "host:port" },
		Templates:    tcpTemplates,
		UnusedParams: func(c *model.Collector, _ RequestOverrides, unused string) error {
			return unusedParamsIn(c, unused, "its request.send or its label values")
		},
		// A target's own tls block, which replaces the collector's whole
		// (withTargetAccount), is checked as the collector's is.
		CheckTargetRequest: func(_ *model.Collector, t *model.StaticTarget) error { return checkTargetTLS(t) },
		Stage:              "tcp",
		CheckFiles:         checkTCPFiles,
		JSONDocument: func(c *model.Collector) string {
			if tcpRequest(c).Document {
				return "the JSON document request.document makes of what a tcp server says"
			}
			return ""
		},
	})
	templateEscapers["line"] = escapeLineValue
	templateRenderedRules["line"] = refuseEmptySend
}

// tcpRequester is the tcp request type's Requester.
type tcpRequester struct{}

// Validate checks a tcp collector's request and fills in its defaults.
func (tcpRequester) Validate(c *model.Collector) error {
	return validateTCPRequest(c)
}

// Fetch performs one request of a tcp collector.
func (tcpRequester) Fetch(ctx context.Context, target string, c *model.Collector, overrides RequestOverrides, forwarded http.Header) (*HTTPResponse, error) {
	return fetchTCP(ctx, target, c, overrides, forwarded)
}

// CheckTarget checks a tcp collector's target.
func (tcpRequester) CheckTarget(_ *model.Collector, target string, _ bool) error {
	_, err := parseTCPTarget(target)
	return err
}

// Label is the url label of a tcp request.
func (tcpRequester) Label(target string, c *model.Collector, overrides RequestOverrides) (string, error) {
	return tcpLabel(target, c, overrides)
}

// Method is the http_method label of a tcp request.
func (tcpRequester) Method(c *model.Collector, overrides RequestOverrides) string {
	return tcpMethod(c, overrides)
}

// Display renders a tcp target for logs and labels.
func (tcpRequester) Display(target string) string {
	return tcpDisplay(target)
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
	t := x.Request.TLS
	r := tcpRequest(x)
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
	if err := validateTCPGreeting(x.Name, r); err != nil {
		return err
	}
	if err := validateTCPStartTLS(x.Name, r); err != nil {
		return err
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

// validateTCPGreeting checks request.greeting: a pattern Go's regexp
// compiles, bounded as send is, holding no placeholder, since it is the
// configuration's check of the server and no probe's; and, without send, no
// terminator, since nothing is read after the greeting then.
func validateTCPGreeting(name string, r TCPConfig) error {
	if r.Greeting == "" {
		return nil
	}
	if len(r.Greeting) > tcpMaxText {
		return fmt.Errorf("collector %q request.greeting is %d bytes, more than the %d it may be; it is a pattern of the server's first line, such as \"^220[ -]\"", name, len(r.Greeting), tcpMaxText)
	}
	if HasPathParams(r.Greeting) {
		return fmt.Errorf("collector %q request.greeting %q has a placeholder; the greeting is matched against the pattern as it is written", name, r.Greeting)
	}
	// Compiled through expr's cache, as a regex rule's pattern is, so that a
	// scrape finds it compiled rather than compiling up to 64 KiB of
	// pattern each time (tcpGreetingPattern).
	if _, err := tcpGreetingPattern(r.Greeting); err != nil {
		return fmt.Errorf("collector %q request.greeting %q is not a regular expression Go reads: %w", name, r.Greeting, err)
	}
	if r.Send == "" && r.Terminator != "" {
		return fmt.Errorf("collector %q sets request.greeting and request.terminator without request.send; with no send the greeting is the answer, and nothing is read after it for a terminator to end", name)
	}
	return nil
}

// validateTCPStartTLS checks request.starttls: a protocol startTLS speaks,
// and, without send, no terminator, since nothing is read after the
// handshake then. A tls block is the handshake's settings, so any goes with
// it; no tls block is the defaults, never plaintext.
func validateTCPStartTLS(name string, r TCPConfig) error {
	if r.StartTLS == "" {
		return nil
	}
	if !slices.Contains(startTLSProtocols, r.StartTLS) {
		return fmt.Errorf("collector %q request.starttls %q is not a protocol whose STARTTLS a tcp collector speaks; want smtp, imap, pop3 or ftp", name, r.StartTLS)
	}
	if r.Send == "" && r.Terminator != "" {
		return fmt.Errorf("collector %q sets request.starttls and request.terminator without request.send; with no send the scrape ends with the TLS handshake, and nothing is read after it for a terminator to end", name)
	}
	return nil
}

// tcpGreetingPattern is request.greeting compiled, once per distinct
// pattern while it is in use (expr.CompileRegex).
var tcpGreetingPattern = expr.CompileRegex

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
// verified with the system roots rather than leaving the scrape plaintext,
// or starttls, which begins TLS with the tls block's settings, or with the
// system roots without one.
func tcpSecure(c *model.Collector) bool {
	return c.Request.TLS != model.TLSConfig{} || tcpRequest(c).StartTLS != ""
}

// tcpURL is the url label and the trace's URL of a scrape: tcp://host:port,
// or tls:// over TLS, from the start or after STARTTLS.
func tcpURL(a tcpAddress, secure bool) string {
	if secure {
		return "tls://" + a.hostPort
	}
	return "tcp://" + a.hostPort
}

func tcpLabel(target string, c *model.Collector, overrides RequestOverrides) (string, error) {
	a, err := parseTCPTarget(target)
	if err != nil {
		return "", err
	}
	return tcpURL(a, tcpSecure(withTargetAccount(c, overrides))), nil
}

func tcpMethod(c *model.Collector, _ RequestOverrides) string {
	if tcpRequest(c).Send == "" {
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
	// A static target's own tls block, in place of the collector's.
	c = withTargetAccount(c, overrides)
	address, err := parseTCPTarget(target)
	if err != nil {
		return nil, err
	}
	send, err := templateField{"request.send", tcpRequest(c).Send, "line"}.render(overrides.Params)
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
	traced := http.Header{"Tcp-Send": {tcpShown(send)}}
	if pattern := tcpRequest(c).Greeting; pattern != "" {
		// Before Tcp-Send in a report, which lists headers by name, as the
		// greeting comes before the write.
		traced.Set("Tcp-Greeting", tcpShown(pattern))
	}
	if protocol := tcpRequest(c).StartTLS; protocol != "" {
		traced.Set("Tcp-Starttls", protocol)
	}
	traceRequest(ctx, tcpMethod(c, overrides), label, traced, "", false)
	x, err := tcpExchange(ctx, address, cfg, send, c)
	if err != nil {
		traceOutcome(ctx, "error: "+err.Error())
		return nil, err
	}
	traceOutcome(ctx, x.outcome())
	headers, body := x.headers(), x.body
	if tcpRequest(c).Document {
		if body, err = x.document(address, responseLimit(c)); err != nil {
			return nil, err
		}
		headers.Set("Content-Type", "application/json")
	}
	return &HTTPResponse{
		StatusCode: http.StatusOK,
		NoStatus:   true,
		// No Content-Type without request.document: the decoder is
		// decoder.type, or what the body reads as, as for a file whose
		// extension says nothing.
		Headers:   headers,
		Body:      body,
		Target:    target,
		Collector: c.Name,
		Duration:  time.Since(start),
	}, nil
}

// tcpExchanged is what a scrape's exchange read, and how long each of its
// steps took: a step it did not take is 0, greeting is nil without
// request.greeting or request.starttls, and starttls, the protocol, empty
// without the latter.
type tcpExchanged struct {
	body     []byte
	how      string
	greeting []byte
	// matched is whether request.greeting checked the greeting.
	matched bool
	// answered is false when nothing was read after the greeting, which is
	// then the answer.
	answered                                   bool
	connect, handshake, greet, upgrade, answer time.Duration
	// total is the whole exchange, from the dial to the end of the read.
	total  time.Duration
	secure bool
	// starttls is the protocol whose request began TLS, steps its commands
	// and the server's answers, and version the TLS version the handshake
	// settled on, for the debug report.
	starttls string
	steps    []startTLSStep
	version  uint16
}

// tcpDocument is a scrape's answer as request.document hands it to the
// transforms: a step the scrape did not take, and what it did not read, are
// left out, rather than written null or 0, as a dns record's fields of
// another type are.
type tcpDocument struct {
	Greeting        *string  `json:"greeting,omitempty"`
	Answer          *string  `json:"answer,omitempty"`
	StartTLS        string   `json:"starttls,omitempty"`
	ConnectSeconds  float64  `json:"connect_seconds"`
	TLSSeconds      *float64 `json:"tls_seconds,omitempty"`
	GreetingSeconds *float64 `json:"greeting_seconds,omitempty"`
	StartTLSSeconds *float64 `json:"starttls_seconds,omitempty"`
	AnswerSeconds   *float64 `json:"answer_seconds,omitempty"`
	DurationSeconds float64  `json:"duration_seconds"`
}

// document is the exchange as a JSON document (tcpDocument). JSON text is
// UTF-8, so a greeting or an answer holding other bytes fails the scrape,
// as a redis reply or an LDAP value does, rather than reaching the rules
// with its bytes replaced. The document is the body the rules read, so it
// is held to the response limit as a body is: the greeting and the answer
// share the limit as they are read (tcpExchange), but JSON writes a control
// byte in six ("\u0001") and adds its keys and timings, so a document of
// a greeting and an answer within it can still be past it.
func (x *tcpExchanged) document(address tcpAddress, limit int64) ([]byte, error) {
	seconds := func(d time.Duration) *float64 { s := d.Seconds(); return &s }
	doc := tcpDocument{ConnectSeconds: x.connect.Seconds(), DurationSeconds: x.total.Seconds()}
	if x.secure {
		doc.TLSSeconds = seconds(x.handshake)
	}
	if x.starttls != "" {
		doc.StartTLS, doc.StartTLSSeconds = x.starttls, seconds(x.upgrade)
	}
	if x.greeting != nil {
		if !utf8.Valid(x.greeting) {
			return nil, fmt.Errorf("the greeting of %s holds bytes that are not UTF-8 text, which the JSON document of request.document cannot hold as they are; leave request.document out to read them as they came", address.hostPort)
		}
		greeting := string(x.greeting)
		doc.Greeting, doc.GreetingSeconds = &greeting, seconds(x.greet)
	}
	if x.answered {
		if !utf8.Valid(x.body) {
			return nil, fmt.Errorf("the answer of %s holds bytes that are not UTF-8 text, which the JSON document of request.document cannot hold as they are; leave request.document out to read them as they came", address.hostPort)
		}
		answer := string(x.body)
		doc.Answer, doc.AnswerSeconds = &answer, seconds(x.answer)
	}
	document, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	if int64(len(document)) > limit {
		return nil, model.MarkError(model.Errorf("the JSON document request.document makes of what %s said is %d bytes, larger than the response limit of %d bytes; raise request.max_response_bytes or limits.max_response_bytes", address.hostPort, len(document), model.Size(limit)), model.ErrLimitExceeded)
	}
	return document, nil
}

// The headers a tcp answer carries, for the transforms (tcpHeaders).
const (
	tcpHeaderConnect  = "X-Tcp-Connect-Seconds"
	tcpHeaderTLS      = "X-Tcp-Tls-Seconds"
	tcpHeaderGreeting = "X-Tcp-Greeting"
	tcpHeaderGreet    = "X-Tcp-Greeting-Seconds"
	tcpHeaderStartTLS = "X-Tcp-Starttls-Seconds"
	tcpHeaderAnswer   = "X-Tcp-Answer-Seconds"
)

// headers are the answer's headers: how long the connection, the TLS
// handshake, the greeting, the request for TLS and the answer took, in seconds, each when the
// scrape took the step, and the greeting, a value for each of its lines
// without its line end, as a header sent once for each. A tcp answer has no
// headers of its own, so these name no header a server sent; they are how
// a jq rule ($headers) and a Python script (response.headers) read what is
// not the body without the body changing.
func (x *tcpExchanged) headers() http.Header {
	h := http.Header{tcpHeaderConnect: {tcpSeconds(x.connect)}}
	if x.secure {
		h.Set(tcpHeaderTLS, tcpSeconds(x.handshake))
	}
	if x.greeting != nil {
		h.Set(tcpHeaderGreet, tcpSeconds(x.greet))
		h[tcpHeaderGreeting] = tcpGreetingLines(x.greeting)
	}
	if x.starttls != "" {
		h.Set(tcpHeaderStartTLS, tcpSeconds(x.upgrade))
	}
	if x.answered {
		h.Set(tcpHeaderAnswer, tcpSeconds(x.answer))
	}
	return h
}

// outcome is how a debug report tells the exchange's end: the greeting, when
// one was read, the request for TLS and the TLS version begun with it, then
// how reading the answer stopped.
func (x *tcpExchanged) outcome() string {
	if x.greeting == nil {
		return fmt.Sprintf("read %d bytes, %s", len(x.body), x.how)
	}
	greeted := "greeted with " + tcpShown(string(x.greeting))
	if x.matched {
		greeted += ", which matched request.greeting"
	}
	if x.starttls != "" {
		greeted += "; " + startTLSShown(x.steps) + "; began " + tls.VersionName(x.version)
	}
	if x.answered {
		return fmt.Sprintf("%s; read %d bytes, %s", greeted, len(x.body), x.how)
	}
	return greeted + "; nothing sent, the greeting is the answer"
}

// tcpSeconds writes a duration as a number of seconds, as Prometheus's
// series of durations are.
func tcpSeconds(d time.Duration) string {
	return strconv.FormatFloat(d.Seconds(), 'f', -1, 64)
}

// tcpGreetingLines are a greeting's lines without their line ends.
func tcpGreetingLines(greeting []byte) []string {
	text := strings.TrimSuffix(strings.TrimSuffix(string(greeting), "\n"), "\r")
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimSuffix(line, "\r")
	}
	return lines
}

// tcpExchange connects, speaks TLS when cfg is set, from the start or after
// the greeting with request.starttls, reads the greeting when the
// collector expects one, writes send and reads the answer, saying how
// reading stopped and how long each step took.
func tcpExchange(ctx context.Context, address tcpAddress, cfg *tls.Config, send string, c *model.Collector) (*tcpExchanged, error) {
	r := tcpRequest(c)
	x := &tcpExchanged{secure: cfg != nil, starttls: r.StartTLS}
	dialer := &net.Dialer{}
	began := time.Now()
	dialed := began
	conn, err := policyDialer(dialer.DialContext)(ctx, "tcp", address.hostPort)
	if err != nil {
		if !errors.Is(err, ErrTargetRefused) {
			err = tcpFailure(ctx, fmt.Errorf("connecting to %s: %w", address.hostPort, err))
		}
		return nil, err
	}
	x.connect = time.Since(began)
	defer func() { _ = conn.Close() }()
	// The write and every read end at the scrape's deadline, or as soon as
	// it is cancelled.
	deadline, _ := ctx.Deadline()
	_ = conn.SetDeadline(deadline)
	// The connection dialed, not conn, which becomes the TLS connection
	// below while the deadline's func may be running: setting the dialed
	// connection's deadline ends the TLS connection's reads and writes too.
	raw := conn
	stop := context.AfterFunc(ctx, func() { _ = raw.SetDeadline(time.Unix(1, 0)) })
	defer stop()
	handshake := func() error {
		began := time.Now()
		tlsConn := tls.Client(raw, cfg)
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			return tcpFailure(ctx, fmt.Errorf("TLS handshake with %s: %w", address.hostPort, err))
		}
		x.handshake, x.version = time.Since(began), tlsConn.ConnectionState().Version
		conn = tlsConn
		return nil
	}
	if cfg != nil && r.StartTLS == "" {
		if err := handshake(); err != nil {
			return nil, err
		}
	}
	// Reads go through one buffer, so what a server wrote after its
	// greeting in the same packet is the start of the answer.
	reader := bufio.NewReader(conn)
	limit := responseLimit(c)
	// A document holds the greeting and the answer both, so with
	// request.document the answer gets what the greeting left of the limit.
	var shared int64
	if r.Greeting != "" || r.StartTLS != "" {
		began = time.Now()
		greeting, err := tcpReadGreeting(ctx, reader, address, limit, r)
		if err != nil {
			return nil, err
		}
		x.greet, x.greeting = time.Since(began), greeting
		if r.Greeting != "" {
			pattern, err := tcpGreetingPattern(r.Greeting)
			if err != nil {
				return nil, fmt.Errorf("request.greeting %q: %w", r.Greeting, err)
			}
			if !pattern.Match(greeting) {
				return nil, model.Errorf("the greeting of %s, %s, does not match request.greeting %q", address.hostPort, model.Quoted(greeting), r.Greeting)
			}
			x.matched = true
		}
		if r.StartTLS != "" {
			if err := startTLSGreeted(r.StartTLS, address.hostPort, greeting); err != nil {
				return nil, err
			}
			// Nothing but the protocol's request for TLS is written before
			// the handshake: send, which a probe's values fill, goes over
			// TLS alone.
			began = time.Now()
			x.steps, err = startTLS(ctx, conn, reader, r.StartTLS, address.hostPort, raw.LocalAddr(), limit)
			if err != nil {
				return nil, err
			}
			x.upgrade = time.Since(began)
			if err := handshake(); err != nil {
				return nil, err
			}
			reader = bufio.NewReader(conn)
		}
		if send == "" {
			x.body, x.total = greeting, time.Since(dialed)
			return x, nil
		}
		if r.Document {
			shared = int64(len(greeting))
		}
	}
	began = time.Now()
	if send != "" {
		if _, err := io.WriteString(conn, send); err != nil {
			return nil, tcpFailure(ctx, fmt.Errorf("sending request.send to %s: %w", address.hostPort, err))
		}
	}
	x.body, x.how, err = tcpRead(ctx, reader, address, []byte(r.Terminator), limit, shared)
	if err != nil {
		return nil, err
	}
	x.answer, x.answered, x.total = time.Since(began), true, time.Since(dialed)
	return x, nil
}

// tcpReadGreeting reads what a server says first, before anything is sent,
// as readReply reads a reply: its first line, or, after a line beginning
// with three digits and a '-', every line up to the one beginning with the
// same digits and no '-'. The greeting is held to limit as the answer is,
// and ends at the scrape's deadline: a server that writes no line end, or
// never ends its continuation, fails the scrape. Its failures name the key
// that expects it, request.greeting, or else request.starttls.
func tcpReadGreeting(ctx context.Context, reader *bufio.Reader, address tcpAddress, limit int64, r TCPConfig) ([]byte, error) {
	key := "request.greeting"
	if r.Greeting == "" {
		key = "request.starttls"
	}
	greeting, err := readReply(reader, limit)
	switch {
	case err == nil:
		return greeting, nil
	case errors.Is(err, errReplyTooLarge):
		return nil, model.MarkError(model.Errorf("the greeting of %s is larger than the response limit of %d bytes; raise request.max_response_bytes or limits.max_response_bytes", address.hostPort, model.Size(limit)), model.ErrLimitExceeded)
	case errors.Is(err, io.EOF) && len(greeting) == 0:
		return nil, fmt.Errorf("the server %s closed the connection without a greeting; %s expects one", address.hostPort, key)
	case errors.Is(err, io.EOF):
		return nil, model.Errorf("the server %s closed the connection before its greeting ended, after %d bytes: %s", address.hostPort, len(greeting), model.Quoted(greeting))
	}
	return nil, tcpFailure(ctx, fmt.Errorf("waiting for the greeting of %s, as %s expects, %d bytes read: %w", address.hostPort, key, len(greeting), err))
}

// tcpRead reads the answer from conn until it ends with terminator, or,
// without one, until the server closes the connection, holding it to limit
// as it arrives, with the shared bytes of the greeting a document holds
// beside it counted against limit too.
func tcpRead(ctx context.Context, conn io.Reader, address tcpAddress, terminator []byte, limit, shared int64) ([]byte, string, error) {
	var body []byte
	chunk := make([]byte, 32<<10)
	for {
		n, err := conn.Read(chunk)
		if n > 0 {
			if shared+int64(len(body))+int64(n) > limit {
				if shared > 0 {
					return nil, "", model.MarkError(model.Errorf("the greeting and the answer of %s are larger together than the response limit of %d bytes, which the JSON document of request.document is held to; raise request.max_response_bytes or limits.max_response_bytes", address.hostPort, model.Size(limit)), model.ErrLimitExceeded)
				}
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

// checkTCPFiles reads the tls block's files as a scrape reads them, the
// collector's or those of a static target's own block, for --dry-run.
// Loading the configuration reads none of them, since a file may be mounted
// after the exporter starts.
func checkTCPFiles(c *model.Collector, t *model.StaticTarget) []string {
	return accountFileWarnings(c, t)
}

// lineValueRule is why a placeholder's value in a tcp collector's send is
// held to lineValue (lineValueRefused).
const lineValueRule = "a value written into request.send may hold only letters, digits and . _ - : / @ + = , %, since the server reads send as commands, and a blank, a ';', a quote or a line break could make the value an argument or a command of its own"

// tcpTemplates is a tcp collector's send, the line written to its server.
func tcpTemplates(c *model.Collector, _ RequestOverrides) []templateField {
	if send := tcpRequest(c).Send; HasPathParams(send) {
		return []templateField{{"request.send", send, "line"}}
	}
	return nil
}

// escapeLineValue refuses a value lineValue does not take.
func escapeLineValue(where, name, value string) (string, error) {
	if character, refused := lineValueRefused(value); refused {
		return "", fmt.Errorf("%s: the value of %s holds %s; %s", where, name, character, lineValueRule)
	}
	return value, nil
}

// refuseEmptySend refuses a send that its placeholders' values leave
// empty, as {{param_cmd:}} does when the probe gives no param_cmd. Writing
// nothing would make the scrape a read of what the server says first, which
// a server that waits for a command never ends but at the deadline, while
// the collector's method, its terminator and its debug report say it sends:
// the probe is refused, naming the parameters, as one missing a value is.
func refuseEmptySend(where string, placeholders []pathPlaceholder, rendered string) error {
	if rendered != "" {
		return nil
	}
	names := make([]string, 0, len(placeholders))
	for _, p := range placeholders {
		if !slices.Contains(names, p.Name) {
			names = append(names, p.Name)
		}
	}
	return fmt.Errorf("%s is empty once %s is filled in, with no value from the probe and an empty default; a tcp collector with send writes it, so give %s a value, or its placeholder a default that is not empty", where, strings.Join(names, ", "), names[0])
}

// tcpKeyRules is what the configuration schema says of a tcp collector's
// own keys.
func tcpKeyRules() map[string]map[string]any {
	return map[string]map[string]any{
		"document":   {"description": "tcp: true hands the transforms a JSON document of the exchange in place of the raw answer: greeting and answer as text, starttls, the protocol, with starttls, connect_seconds, tls_seconds, greeting_seconds, starttls_seconds and answer_seconds for the steps taken, and duration_seconds, a step not taken left out. The decoder is then json, and the transform jq, yq or python. A greeting or an answer that is not UTF-8 fails the scrape, and a document past max_response_bytes fails it as a limit exceeded. See docs/TCP.md#the-exchange-as-a-document."},
		"greeting":   {"description": "tcp: a regular expression (Go's, RE2) the server's greeting must match, such as \"^220[ -]\" for SMTP or '^SSH-2\\.0-' for SSH: the greeting, its first line, or, after a line beginning with three digits and a '-', every line up to the one beginning with those digits and no '-', is read before send is written, and a greeting that does not match fails the scrape. Without send the greeting is the answer, and terminator may not be set. No placeholder; at most 64 KiB. See docs/TCP.md#services-that-speak-first."},
		"starttls":   {"description": "tcp: smtp, imap, pop3 or ftp: begin TLS on the plaintext connection as that protocol asks for it, after the server's greeting, which is read and must be the protocol's ready greeting (220, * OK, +OK, 220): EHLO and STARTTLS, a1 STARTTLS, STLS or AUTH TLS. The handshake takes the tls block's settings, or verifies against the system's roots without one; send is written only over TLS, and without it the scrape ends with the handshake. A server that does not offer STARTTLS, refuses it or answers otherwise fails the scrape; it never goes on in plaintext. With no send, terminator may not be set. See docs/TCP.md#starttls.", "enum": SchemaOptionalEnum(startTLSProtocols)},
		"send":       {"description": "tcp: the text written once the connection is made, such as \"stats\\r\\n\" for memcached, written in double quotes so that \\r\\n is a carriage return and a line feed; nothing when left out, the answer then being what the server says first. May contain {{param_name}} placeholders, whose values, and defaults, may hold only letters, digits and . _ - : / @ + = , %, so a probe adds no command or argument; a send they leave empty is refused. At most 64 KiB. tcp is in a build with OPT_IN_REQUEST_TYPES naming it alone. See docs/TCP.md."},
		"terminator": {"description": "tcp: what the answer ends with, such as \"END\\r\\n\": reading stops once it has come, and it stays in the body. Left out, reading stops when the server closes the connection. At most 64 KiB, and no placeholder. See docs/TCP.md#reading-the-answer."},
	}
}
