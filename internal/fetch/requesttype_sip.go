//go:build request_type_sip

package fetch

import (
	"bufio"
	"bytes"
	"context"
	"crypto/md5" //nolint:gosec // G501: SIP's digest authentication (RFC 3261 § 22) is defined over MD5, and SHA-256 since RFC 8760
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"net"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The sip request type asks a SIP server (RFC 3261), a proxy, a registrar
// or a PBX, whether it answers: a scrape sends one OPTIONS request, the
// ping of SIP, over UDP, TCP or TLS, and reads the response. Its target is
// the server, host or host:port, port 5060 when it names none and 5061
// over TLS.
//
//	request:
//	  type: sip
//	  # sip_transport: tcp
//
// The answer is one JSON document: the response's status code and reason,
// how long it took, how many times the request was sent, and what the
// server says of itself, its Server header and the methods it allows. A
// response is an answer whatever its status: a 404 or a 405 is a server
// that is up and does not like the question, which a rule reads from
// status_code, so the scrape fails only when no response comes.
//
// Over UDP the request is sent again while no response comes, as RFC 3261
// has a client do: after half a second, then twice as long each time up to
// four seconds, until the scrape's deadline. A datagram is the response
// only when it answers this request, by the branch of its Via, its Call-ID
// and its CSeq; any other is ignored, and so is a provisional response.
//
// A server that asks who is asking, with a 401 or a 407 and a Digest
// challenge, is answered once with the account of request.username and
// request.password_file, when the collector has one: MD5 and SHA-256, with
// qop auth or without (RFC 8760). Without an account the 401 is the answer.
//
// No call is placed and nothing is registered: INVITE, REGISTER and media
// are not spoken.

// SIPConfig is a sip collector's own request keys.
type SIPConfig struct {
	// Transport is what the request is sent over: udp, the default, tcp
	// or tls.
	Transport string `yaml:"sip_transport"`
	// RequestURI is the URI the request asks, and its To: the server's
	// own, sip:host:port, when left out.
	RequestURI string `yaml:"request_uri"`
	// FromURI is who the request says is asking.
	FromURI string `yaml:"from_uri"`
	// UserAgent is the request's User-Agent.
	UserAgent string `yaml:"user_agent"`
}

// sipRequest is a sip collector's own keys.
func sipRequest(c *model.Collector) SIPConfig {
	own, _ := c.Request.Own.(SIPConfig)
	return own
}

const (
	sipMethod  = "OPTIONS"
	sipTimeout = 30 * time.Second

	sipUDP = "udp"
	sipTCP = "tcp"
	sipTLS = "tls"

	sipDefaultUserAgent = "prometheus-universal-exporter"
	sipDefaultFromUser  = "prometheus-universal-exporter"

	// sipMaxURI and sipMaxUserAgent are the most bytes of the keys, and
	// sipMaxHeaders the most of a response's start line and headers.
	sipMaxURI       = 255
	sipMaxUserAgent = 128
	sipMaxHeaders   = 64 << 10
	// sipMaxHeaderValues bounds the values the document keeps of a header.
	sipMaxHeaderNames  = 128
	sipMaxHeaderValues = 32
	// sipMaxRequests is the most requests of a scrape: the first, the one
	// that answers a challenge, and one more for a nonce that went stale.
	sipMaxRequests = 3
)

// sipT1 and sipT2 are the timers a request over UDP is sent again by (RFC
// 3261 section 17.1.2.2): after T1, then twice as long each time, up to
// T2. A test shortens them.
var (
	sipT1 = 500 * time.Millisecond
	sipT2 = 4 * time.Second
)

// sipTransports are the values of request.sip_transport.
var sipTransports = []string{sipUDP, sipTCP, sipTLS}

// sipPorts gives a transport its default port.
func sipPorts(transport string) map[string]int {
	if transport == sipTLS {
		return map[string]int{"": 5061}
	}
	return map[string]int{"": 5060}
}

func init() {
	registerRequestType(&RequestType{
		Name:         RequestTypeSIP,
		Fields:       []string{"username", "password_file", "tls", "max_response_bytes", "allowed_targets", "denied_targets"},
		Config:       SIPConfig{},
		KeyRules:     sipKeyRules,
		Overrides:    []string{"timeout", PathParamPrefix},
		TargetFields: []string{"timeout"},
		Requester:    sipRequester{},
		JSONAnswer:   true,
		TargetHint: func(c *model.Collector) (string, string) {
			return "SIP server", "host or host:" + strconv.Itoa(sipPorts(sipRequest(c).Transport)[""])
		},
		AnswerHint: "Paste the response as the JSON document the exporter makes of it, as the Response of a run shows it",
		UnusedParams: func(c *model.Collector, _ RequestOverrides, unused string) error {
			return unusedParamsIn(c, unused, "its label values")
		},
		Stage: "sip",
		CheckFiles: func(c *model.Collector, t *model.StaticTarget) []string {
			if t != nil {
				return nil
			}
			const later = "a scrape would fail on it now (the file is read at each scrape, so one put in place later is used then)"
			var warnings []string
			if _, err := netPassword(c); err != nil {
				warnings = append(warnings, fmt.Sprintf("collector %q: %v; %s", c.Name, err, later))
			}
			if _, err := sipTLSConfig(c); err != nil {
				warnings = append(warnings, fmt.Sprintf("collector %q: %v; %s", c.Name, err, later))
			}
			return warnings
		},
	})
}

// sipRequester is the sip request type's Requester.
type sipRequester struct{}

// Validate checks a sip collector's request and fills in its defaults: the
// transport, the User-Agent, URIs that are SIP URIs, an account that is
// whole, and a tls block only where TLS is spoken.
func (sipRequester) Validate(c *model.Collector) error {
	r := sipRequest(c)
	if r.Transport == "" {
		r.Transport = sipUDP
	}
	if r.UserAgent == "" {
		r.UserAgent = sipDefaultUserAgent
	}
	switch {
	case !slices.Contains(sipTransports, r.Transport):
		return fmt.Errorf("collector %q request.sip_transport is %q; want udp, tcp or tls, in lower case", c.Name, r.Transport)
	case r.Transport != sipTLS && c.Request.TLS != (model.TLSConfig{}):
		return fmt.Errorf("collector %q sets request.tls with sip_transport: %s; the block is for a request sent over TLS, so set sip_transport: tls or take it out", c.Name, r.Transport)
	case (c.Request.Username == "") != (strings.TrimSpace(c.Request.PasswordFile) == "") && !c.RequestNotMade:
		return fmt.Errorf("collector %q sets only one of request.username and password_file; the account a challenge is answered with needs both", c.Name)
	case strings.ContainsAny(c.Request.Username, `"\`):
		return fmt.Errorf("collector %q request.username has a quote or a backslash, which a digest's username cannot hold as it is written", c.Name)
	case len(r.UserAgent) > sipMaxUserAgent || strings.ContainsFunc(r.UserAgent, sipNotHeaderText):
		return fmt.Errorf("collector %q request.user_agent is longer than %d bytes or has a control character; it is one header's value", c.Name, sipMaxUserAgent)
	}
	for _, uri := range []struct{ key, value string }{{"request_uri", r.RequestURI}, {"from_uri", r.FromURI}} {
		if uri.value == "" {
			continue
		}
		if err := checkSIPURI(uri.value); err != nil {
			return fmt.Errorf("collector %q request.%s %w", c.Name, uri.key, err)
		}
	}
	// The tls block and the account, held to what the other types that
	// take them hold theirs to.
	t := c.Request.TLS
	switch {
	case (t.CertFile == "") != (t.KeyFile == ""):
		return fmt.Errorf("collector %q request.tls sets only one of cert_file and key_file; a client certificate needs both", c.Name)
	case HasPathParams(c.Request.Username) || HasPathParams(c.Request.PasswordFile):
		return fmt.Errorf("collector %q request.username or password_file has a placeholder; the account is used as it is written", c.Name)
	case strings.ContainsFunc(c.Request.Username, sipNotHeaderText):
		return fmt.Errorf("collector %q request.username holds a control character", c.Name)
	}
	if t.ServerName != "" {
		if _, err := canonicalHost(t.ServerName); err != nil {
			return fmt.Errorf("collector %q request.tls.server_name %q: %w", c.Name, t.ServerName, err)
		}
	}
	c.Request.Own = r
	return nil
}

// sipTLSConfig is the tls block's configuration when the collector's
// requests go over TLS, nil otherwise.
func sipTLSConfig(c *model.Collector) (*tls.Config, error) {
	if sipRequest(c).Transport != sipTLS {
		return nil, nil
	}
	cfg, err := tlsConfig(c.Request.TLS)
	if err != nil {
		return nil, fmt.Errorf("request.tls: %w", err)
	}
	return cfg, nil
}

// sipShown is text a server sent, as an error or a debug report shows it:
// its control characters written as ?, and no longer than a failure is.
func sipShown(text string) string {
	text = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return '?'
		}
		return r
	}, text)
	return model.CutTo(RedactText(text), 200, false)
}

// sipNotHeaderText says whether r may not stand in a header's value.
func sipNotHeaderText(r rune) bool { return r < 0x20 || r == 0x7f }

// checkSIPURI holds a key to a SIP URI as a request writes one between
// angle brackets: sip: or sips: and what follows, on one line, without
// blanks, brackets or a placeholder.
func checkSIPURI(value string) error {
	lower := strings.ToLower(value)
	rest, isSIP := strings.CutPrefix(lower, "sip:")
	if !isSIP {
		rest, isSIP = strings.CutPrefix(lower, "sips:")
	}
	switch {
	case !isSIP || rest == "":
		return fmt.Errorf("is %q; want a SIP URI, such as sip:monitor@example.com", value)
	case len(value) > sipMaxURI:
		return fmt.Errorf("is %d bytes; a URI here is at most %d", len(value), sipMaxURI)
	case strings.ContainsFunc(value, func(r rune) bool { return r <= 0x20 || r == 0x7f || r == '<' || r == '>' || r == '"' }):
		return fmt.Errorf("is %q, which has a blank, a control character, a quote or an angle bracket; write the URI alone", value)
	case HasPathParams(value):
		return fmt.Errorf("is %q, which has a placeholder; the URI is used as it is written", value)
	}
	return nil
}

// sipKeyRules is what the configuration schema says of a sip collector's
// own keys.
func sipKeyRules() map[string]map[string]any {
	const uri = `^([sS][iI][pP][sS]?:[^\x00-\x20\x7f<>"]+)?$`
	return map[string]map[string]any{
		"sip_transport": {"enum": SchemaOptionalEnum(sipTransports), "description": "sip: what the request is sent over: udp, the default, where it is sent again while no response comes; tcp; or tls, verified as request.tls says. See docs/SIP.md#the-transport."},
		"request_uri":   {"maxLength": sipMaxURI, "pattern": uri, "description": "sip: the URI the request asks, and its To, such as sip:1000@pbx.example.com to ask for a user; the server's own, sip:host:port, when left out."},
		"from_uri":      {"maxLength": sipMaxURI, "pattern": uri, "description": "sip: who the request says is asking, its From; sip:prometheus-universal-exporter@ and the target's host when left out, or request.username in its place when the collector has an account."},
		"user_agent":    {"maxLength": sipMaxUserAgent, "pattern": `^[^\x00-\x1f\x7f]*$`, "description": "sip: the request's User-Agent; prometheus-universal-exporter when left out."},
	}
}

// Fetch performs one request of a sip collector.
func (sipRequester) Fetch(ctx context.Context, target string, c *model.Collector, overrides RequestOverrides, _ http.Header) (*HTTPResponse, error) {
	return fetchSIP(ctx, target, c, overrides)
}

// parseSIPTarget reads a target of c: host or host:port, the port its
// transport's when it names none.
func parseSIPTarget(c *model.Collector, target string) (netAddress, error) {
	return parseNetTarget("sip", target, sipPorts(sipRequest(c).Transport), false)
}

// CheckTarget checks a sip collector's target.
func (sipRequester) CheckTarget(c *model.Collector, target string, _ bool) error {
	_, err := parseSIPTarget(c, target)
	return err
}

// sipLabel is the url label of a request to address: sip:// and the
// server, sips:// over TLS.
func sipLabel(c *model.Collector, address netAddress) string {
	if sipRequest(c).Transport == sipTLS {
		return "sips://" + address.hostPort
	}
	return "sip://" + address.hostPort
}

// Label is the url label of a sip request.
func (sipRequester) Label(target string, c *model.Collector, _ RequestOverrides) (string, error) {
	address, err := parseSIPTarget(c, target)
	if err != nil {
		return "", err
	}
	return sipLabel(c, address), nil
}

// Method is the http_method label of a sip request: the method sent.
func (sipRequester) Method(*model.Collector, RequestOverrides) string { return sipMethod }

// Display renders a sip target for logs and labels: as written, since it
// holds no credentials, when it is one at all.
func (sipRequester) Display(target string) string {
	if _, err := parseNetTarget("sip", target, sipPorts(sipUDP), false); err != nil {
		return "<invalid target>"
	}
	return strings.TrimSpace(target)
}

// sipDocument is a server's response as the transforms read it.
type sipDocument struct {
	Host string `json:"host"`
	Port int    `json:"port"`
	// Address is the address the request was sent to, host:port.
	Address   string `json:"address"`
	Transport string `json:"transport"`
	// StatusCode and Reason are the response's, and OK says the status is
	// one of success, 200 to 299.
	StatusCode int    `json:"status_code"`
	Reason     string `json:"reason"`
	OK         bool   `json:"ok"`
	// Requests is how many requests the scrape made, 2 when it answered a
	// challenge, and Attempts how many times the last was sent.
	Requests int `json:"requests"`
	Attempts int `json:"attempts"`
	// ResponseSeconds is how long after the last request was first sent
	// its response came.
	ResponseSeconds float64 `json:"response_seconds"`
	// Challenged says the server asked for credentials, and Authenticated
	// that it took the collector's.
	Challenged    bool `json:"challenged"`
	Authenticated bool `json:"authenticated"`
	// Server and UserAgent are the response's headers of those names.
	Server    string `json:"server"`
	UserAgent string `json:"user_agent"`
	// Allow, Accept and Supported are the values of those headers, one a
	// member.
	Allow     []string `json:"allow"`
	Accept    []string `json:"accept"`
	Supported []string `json:"supported"`
	// Headers is every header of the response by its name in lower case,
	// a compact name written out.
	Headers map[string][]string `json:"headers"`
	// BodyBytes is the length of the response's body.
	BodyBytes int `json:"body_bytes"`
	// TLSVersion and TLSCipherSuite are the connection's over TLS, "" over
	// the other transports.
	TLSVersion      string  `json:"tls_version"`
	TLSCipherSuite  string  `json:"tls_cipher_suite"`
	DurationSeconds float64 `json:"duration_seconds"`
}

// fetchSIP sends a scrape's OPTIONS and answers the response as JSON.
func fetchSIP(ctx context.Context, target string, c *model.Collector, overrides RequestOverrides) (*HTTPResponse, error) {
	start := time.Now()
	address, err := parseSIPTarget(c, target)
	if err != nil {
		return nil, err
	}
	ctx, cancel := netBudget(ctx, overrides, sipTimeout)
	defer cancel()
	r := sipRequest(c)
	if ctx, err = netGuard(ctx, c, "sip", address.hostPort); err != nil {
		return nil, err
	}
	password, err := netPassword(c)
	if err != nil {
		return nil, err
	}
	tlsSettings, err := sipTLSConfig(c)
	if err != nil {
		return nil, err
	}
	link, err := sipConnect(ctx, address, r.Transport, tlsSettings, responseLimit(c))
	if err != nil {
		return nil, err
	}
	defer link.close()
	doc, err := sipAsk(ctx, link, address, c, password)
	if err != nil {
		return nil, err
	}
	_, port, _ := net.SplitHostPort(address.hostPort)
	doc.Host = address.host
	doc.Port, _ = strconv.Atoi(port)
	doc.Address, doc.Transport = link.conn.RemoteAddr().String(), r.Transport
	if state := sipTLSState(link.conn); state != nil {
		doc.TLSVersion, doc.TLSCipherSuite = tls.VersionName(state.Version), tls.CipherSuiteName(state.CipherSuite)
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

// sipTLSState is the TLS state of a connection made over TLS, nil of any
// other.
func sipTLSState(conn net.Conn) *tls.ConnectionState {
	if secured, ok := conn.(*tls.Conn); ok {
		state := secured.ConnectionState()
		return &state
	}
	return nil
}

// sipLink is the connection of a scrape: a connected UDP socket, whose
// datagrams are each a message, or a stream, TCP or TLS, read message by
// message. limit is the most bytes of a response's body.
type sipLink struct {
	conn     net.Conn
	r        *bufio.Reader
	datagram bool
	limit    int64
	close    func()
}

// sipConnect makes the connection to address through the target policy.
func sipConnect(ctx context.Context, address netAddress, transport string, tlsSettings *tls.Config, limit int64) (*sipLink, error) {
	if transport != sipUDP {
		conn, err := netDial(ctx, address, tlsSettings)
		if err != nil {
			return nil, err
		}
		return &sipLink{conn: conn.Conn, r: conn.r, limit: limit, close: conn.close}, nil
	}
	dialer := &net.Dialer{}
	conn, err := policyDialer(dialer.DialContext)(ctx, "udp", address.hostPort)
	if err != nil {
		if !errors.Is(err, ErrTargetRefused) {
			err = netFailure(ctx, fmt.Errorf("connecting to %s: %w", address.hostPort, err))
		}
		return nil, err
	}
	// A scrape cancelled before its deadline ends the read too.
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Unix(1, 0)) })
	return &sipLink{conn: conn, datagram: true, limit: limit, close: func() {
		stop()
		_ = conn.Close()
	}}, nil
}

// sipExchange is one request of a scrape: its own branch and CSeq, under
// the scrape's Call-ID and From tag.
type sipExchange struct {
	callID, fromTag, branch string
	cseq                    int
}

// sipToken is n random bytes as hex.
func sipToken(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// sipAsk sends the OPTIONS over link and reads its final response,
// answering a Digest challenge with the collector's account when it has
// one, and describes the response that ended the scrape.
func sipAsk(ctx context.Context, link *sipLink, address netAddress, c *model.Collector, password string) (sipDocument, error) {
	r := sipRequest(c)
	requestURI := r.RequestURI
	if requestURI == "" {
		requestURI = "sip:" + address.hostPort
	}
	from := r.FromURI
	if from == "" {
		user := sipDefaultFromUser
		if c.Request.Username != "" {
			user = c.Request.Username
		}
		from = "sip:" + sipUserPart(user) + "@" + sipHostPart(address.host)
	}
	exchange := sipExchange{callID: sipToken(16), fromTag: sipToken(8)}
	var (
		authorization string
		doc           sipDocument
	)
	for request := 1; ; request++ {
		exchange.cseq, exchange.branch = request, "z9hG4bK"+sipToken(12)
		message := sipWriteRequest(link, r, requestURI, from, exchange, authorization)
		shown := http.Header{"Sip-Transport": {r.Transport}}
		if authorization != "" {
			shown.Set("Authorization", "Digest")
		}
		at := traceRequestAt(ctx, sipMethod, sipLabel(c, address), shown, "", false)
		response, attempts, elapsed, err := link.exchange(ctx, address, message, exchange)
		if err != nil {
			at.outcome("error: "+err.Error(), true)
			return sipDocument{}, err
		}
		sent := "sent once"
		if attempts > 1 {
			sent = "sent " + strconv.Itoa(attempts) + " times"
		}
		at.outcome(fmt.Sprintf("%d %s, %s", response.status, sipShown(response.reason), sent), false)
		challenged := doc.Challenged
		doc = response.describe()
		doc.Requests, doc.Attempts, doc.ResponseSeconds = request, attempts, elapsed.Seconds()
		doc.Challenged = challenged
		answered := authorization != ""
		if response.status != http.StatusUnauthorized && response.status != http.StatusProxyAuthRequired {
			doc.Authenticated = answered
			return doc, nil
		}
		doc.Challenged = true
		// A challenge without an account is the answer; so is one to
		// credentials the server did not take, unless it says only that
		// the nonce went stale.
		challenges := response.challenges()
		if c.Request.Username == "" || request == sipMaxRequests || answered && !sipStale(challenges) {
			return doc, nil
		}
		header := "Authorization"
		if response.status == http.StatusProxyAuthRequired {
			header = "Proxy-Authorization"
		}
		credentials, err := sipDigest(challenges, c.Request.Username, password, requestURI)
		if err != nil {
			return sipDocument{}, fmt.Errorf("the server %s asks for credentials (%d), and %w", address.hostPort, response.status, err)
		}
		authorization = header + ": " + credentials
	}
}

// sipUserPart is a name as the user of a URI holds it: the characters a
// user may be written with as they are, and any other as its bytes in hex.
func sipUserPart(user string) string {
	var b strings.Builder
	for i := range len(user) {
		ch := user[i]
		switch {
		case ch >= 'a' && ch <= 'z', ch >= 'A' && ch <= 'Z', ch >= '0' && ch <= '9', strings.IndexByte("-_.!~*'()&=+$,;?/", ch) >= 0:
			b.WriteByte(ch)
		default:
			fmt.Fprintf(&b, "%%%02X", ch)
		}
	}
	return b.String()
}

// sipHostPart is a host as a URI holds it: an IPv6 address in brackets.
func sipHostPart(host string) string {
	if strings.Contains(host, ":") {
		return "[" + host + "]"
	}
	return host
}

// sipWriteRequest is the OPTIONS of exchange as it goes over link: its Via
// names the transport and the address it is sent from, with rport, so a
// server behind address translation answers where the request came from
// (RFC 3581).
func sipWriteRequest(link *sipLink, r SIPConfig, requestURI, from string, exchange sipExchange, authorization string) []byte {
	local := link.conn.LocalAddr().String()
	var b bytes.Buffer
	fmt.Fprintf(&b, "%s %s SIP/2.0\r\n", sipMethod, requestURI)
	fmt.Fprintf(&b, "Via: SIP/2.0/%s %s;branch=%s;rport\r\n", strings.ToUpper(r.Transport), local, exchange.branch)
	b.WriteString("Max-Forwards: 70\r\n")
	fmt.Fprintf(&b, "From: <%s>;tag=%s\r\n", from, exchange.fromTag)
	fmt.Fprintf(&b, "To: <%s>\r\n", requestURI)
	fmt.Fprintf(&b, "Call-ID: %s\r\n", exchange.callID)
	fmt.Fprintf(&b, "CSeq: %d %s\r\n", exchange.cseq, sipMethod)
	b.WriteString("Accept: application/sdp\r\n")
	fmt.Fprintf(&b, "User-Agent: %s\r\n", r.UserAgent)
	if authorization != "" {
		b.WriteString(authorization + "\r\n")
	}
	b.WriteString("Content-Length: 0\r\n\r\n")
	return b.Bytes()
}

// exchange sends message and reads until the final response to it comes or
// ctx ends: attempts is how many times it was sent, more than once over
// UDP alone, and elapsed how long after the first the response came.
func (l *sipLink) exchange(ctx context.Context, address netAddress, message []byte, exchange sipExchange) (response *sipMessage, attempts int, elapsed time.Duration, err error) {
	deadline, _ := ctx.Deadline()
	first := time.Now()
	ignored, lastIgnored := 0, ""
	interval := sipT1
	buffer := make([]byte, 65535)
	for attempts = 1; ; attempts++ {
		if _, err := l.conn.Write(message); err != nil {
			return nil, attempts, 0, netFailure(ctx, fmt.Errorf("sending the request to %s: %w", address.hostPort, err))
		}
		// A stream delivers the request or fails; a datagram is sent again
		// while no response comes, each wait twice the last, up to T2.
		until := deadline
		if next := time.Now().Add(interval); l.datagram && next.Before(deadline) {
			until = next
		}
		interval = min(2*interval, sipT2)
		if ctx.Err() == nil {
			if err := l.conn.SetReadDeadline(until); err != nil {
				return nil, attempts, 0, err
			}
		}
		for {
			var (
				got *sipMessage
				why string
				err error
			)
			if l.datagram {
				var n int
				if n, err = l.conn.Read(buffer); err == nil {
					if got, err = readSIPMessage(bufio.NewReader(bytes.NewReader(buffer[:n])), l.limit, true); err != nil {
						why, err = err.Error(), nil
					}
				}
			} else {
				got, err = readSIPMessage(l.r, l.limit, false)
			}
			if err != nil {
				if errors.Is(err, os.ErrDeadlineExceeded) && ctx.Err() == nil && until != deadline {
					break
				}
				return nil, attempts, 0, sipReadFailure(ctx, address, err, attempts, ignored, lastIgnored)
			}
			if why == "" {
				why = got.answers(exchange)
			}
			if why != "" {
				ignored, lastIgnored = ignored+1, why
				continue
			}
			return got, attempts, time.Since(first), nil
		}
	}
}

// sipReadFailure words a read that ended without the response: the
// deadline, which wraps context.DeadlineExceeded, or the scrape's
// cancellation, saying how many times the request was sent and how many
// messages were ignored and why the last was; or a failure of the
// connection, such as the refusal an ICMP port unreachable brings, a
// response that is no SIP message over a stream, or one past the limit.
func sipReadFailure(ctx context.Context, address netAddress, err error, sent, ignored int, lastIgnored string) error {
	if ctx.Err() == nil && !errors.Is(err, os.ErrDeadlineExceeded) {
		if errors.Is(err, model.ErrLimitExceeded) || errors.Is(err, errSIPMalformed) {
			return fmt.Errorf("the response of %s: %w", address.hostPort, err)
		}
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return fmt.Errorf("%s closed the connection before it answered", address.hostPort)
		}
		return fmt.Errorf("reading the response of %s: %w", address.hostPort, err)
	}
	cause, until := ctx.Err(), "the deadline"
	if errors.Is(cause, context.Canceled) {
		until = "the scrape was cancelled"
	} else {
		cause = context.DeadlineExceeded
	}
	times := "once"
	if sent > 1 {
		times = strconv.Itoa(sent) + " times"
	}
	switch ignored {
	case 0:
		return fmt.Errorf("no response from %s before %s, the request sent %s: %w", address.hostPort, until, times, cause)
	case 1:
		return model.Errorf("no response from %s before %s, the request sent %s; %d message was ignored, as %s: %w", address.hostPort, until, times, model.Size(ignored), lastIgnored, cause)
	}
	return model.Errorf("no response from %s before %s, the request sent %s; %d messages were ignored, the last as %s: %w", address.hostPort, until, times, model.Size(ignored), lastIgnored, cause)
}

// sipMessage is a response: its status line, its headers by name in lower
// case, compact names written out, with the names in the order they came,
// and the length of its body.
type sipMessage struct {
	status  int
	reason  string
	headers map[string][]string
	order   []string
	body    int
}

// errSIPMalformed marks what is no SIP response.
var errSIPMalformed = errors.New("not a SIP response")

// sipCompact are the one-letter names of headers (RFC 3261 section 7.3.3).
var sipCompact = map[string]string{
	"i": "call-id", "m": "contact", "e": "content-encoding", "l": "content-length", "c": "content-type",
	"f": "from", "s": "subject", "k": "supported", "t": "to", "v": "via",
}

// readSIPMessage reads one response from r: the status line, the headers
// up to the blank line, a header continued on the lines that start with a
// blank, and the body its Content-Length says, held to limit. whole says r
// holds one datagram, whose body is what follows the headers when no
// Content-Length says how much of it is.
func readSIPMessage(r *bufio.Reader, limit int64, whole bool) (*sipMessage, error) {
	read := 0
	line := func() (string, error) {
		text, err := r.ReadString('\n')
		read += len(text)
		if read > sipMaxHeaders {
			return "", fmt.Errorf("%w: its headers are longer than %d bytes", errSIPMalformed, sipMaxHeaders)
		}
		if err != nil && (text == "" || !whole) {
			return "", err
		}
		return strings.TrimRight(text, "\r\n"), nil
	}
	start, err := line()
	// A stream may carry the blank lines a server keeps a connection
	// alive with before a message (RFC 5626).
	for err == nil && start == "" && !whole {
		start, err = line()
	}
	if err != nil {
		return nil, err
	}
	version, rest, _ := strings.Cut(start, " ")
	code, reason, _ := strings.Cut(rest, " ")
	status, err := strconv.Atoi(code)
	if !strings.EqualFold(version, "SIP/2.0") || err != nil || len(code) != 3 || status < 100 || status > 699 {
		return nil, fmt.Errorf("%w: it starts %s", errSIPMalformed, sipShownLine(start))
	}
	m := &sipMessage{status: status, reason: reason, headers: map[string][]string{}}
	name := ""
	for {
		text, err := line()
		if err != nil {
			if whole && errors.Is(err, io.EOF) {
				break
			}
			return nil, err
		}
		if text == "" {
			break
		}
		if text[0] == ' ' || text[0] == '\t' {
			// A continuation of the header before it.
			if values := m.headers[name]; name != "" && len(values) > 0 {
				values[len(values)-1] += " " + strings.TrimSpace(text)
			}
			continue
		}
		key, value, isHeader := strings.Cut(text, ":")
		if !isHeader {
			return nil, fmt.Errorf("%w: its header line %s has no colon", errSIPMalformed, sipShownLine(text))
		}
		name = strings.ToLower(strings.TrimSpace(key))
		if long, compact := sipCompact[name]; compact {
			name = long
		}
		values, known := m.headers[name]
		if !known {
			if len(m.headers) >= sipMaxHeaderNames {
				name = ""
				continue
			}
			m.order = append(m.order, name)
		}
		if len(values) < sipMaxHeaderValues {
			values = append(values, strings.ToValidUTF8(strings.TrimSpace(value), "�"))
		}
		m.headers[name] = values
	}
	length := -1
	if values := m.headers["content-length"]; len(values) > 0 {
		if length, err = strconv.Atoi(values[0]); err != nil || length < 0 {
			return nil, fmt.Errorf("%w: its Content-Length is %s", errSIPMalformed, sipShownLine(values[0]))
		}
	}
	switch {
	case length < 0 && !whole:
		return nil, fmt.Errorf("%w: it has no Content-Length, which a message over a stream must", errSIPMalformed)
	case length < 0:
		length = r.Buffered()
	}
	if int64(length) > limit {
		return nil, model.MarkError(model.Errorf("its body is %d bytes, larger than the response limit of %d bytes; raise request.max_response_bytes or limits.max_response_bytes", model.Size(int64(length)), model.Size(limit)), model.ErrLimitExceeded)
	}
	if n, err := io.CopyN(io.Discard, r, int64(length)); err != nil && !whole {
		return nil, err
	} else if whole {
		length = int(n)
	}
	m.body = length
	return m, nil
}

// sipShownLine is text a server sent, as an error shows it: quoted, and no
// longer than a failure's line.
func sipShownLine(text string) string {
	return strconv.Quote(model.CutTo(text, 200, false))
}

// first is the first value of a header, "" when the response has none.
func (m *sipMessage) first(name string) string {
	if values := m.headers[name]; len(values) > 0 {
		return values[0]
	}
	return ""
}

// answers says why the message is not the final response to exchange, ""
// when it is: it answers another request, by the branch of its topmost
// Via, its Call-ID or its CSeq, or it is provisional, with the final one
// still to come.
func (m *sipMessage) answers(exchange sipExchange) string {
	via, _, _ := strings.Cut(m.first("via"), ",")
	branch := ""
	for _, param := range strings.Split(via, ";")[1:] {
		if name, value, _ := strings.Cut(strings.TrimSpace(param), "="); strings.EqualFold(strings.TrimSpace(name), "branch") {
			branch = strings.TrimSpace(value)
		}
	}
	number, method, _ := strings.Cut(strings.TrimSpace(m.first("cseq")), " ")
	switch {
	case branch != exchange.branch:
		return "the branch of its Via is not this request's"
	case m.first("call-id") != exchange.callID:
		return "its Call-ID is not this request's"
	case number != strconv.Itoa(exchange.cseq) || !strings.EqualFold(strings.TrimSpace(method), sipMethod):
		return "its CSeq is " + sipShownLine(m.first("cseq")) + ", not this request's"
	case m.status < 200:
		return fmt.Sprintf("it is provisional, %d %s", m.status, sipShown(m.reason))
	}
	return ""
}

// list is the values of a header that holds a list, each member of each
// value on its own.
func (m *sipMessage) list(name string) []string {
	members := []string{}
	for _, value := range m.headers[name] {
		for _, member := range strings.Split(value, ",") {
			if member = strings.TrimSpace(member); member != "" && len(members) < sipMaxHeaderValues {
				members = append(members, member)
			}
		}
	}
	return members
}

// describe makes the document of a final response.
func (m *sipMessage) describe() sipDocument {
	return sipDocument{
		StatusCode: m.status,
		Reason:     strings.ToValidUTF8(m.reason, "�"),
		OK:         m.status >= 200 && m.status < 300,
		Server:     m.first("server"),
		UserAgent:  m.first("user-agent"),
		Allow:      m.list("allow"),
		Accept:     m.list("accept"),
		Supported:  m.list("supported"),
		Headers:    m.headers,
		BodyBytes:  m.body,
	}
}

// sipChallenge is one Digest challenge of a 401 or a 407.
type sipChallenge struct {
	realm, nonce, opaque, algorithm string
	qop                             []string
	stale                           bool
}

// challenges are the Digest challenges of the response, of
// WWW-Authenticate and Proxy-Authenticate alike.
func (m *sipMessage) challenges() []sipChallenge {
	var found []sipChallenge
	for _, name := range []string{"www-authenticate", "proxy-authenticate"} {
		for _, value := range m.headers[name] {
			scheme, params, _ := strings.Cut(strings.TrimSpace(value), " ")
			if !strings.EqualFold(scheme, "Digest") {
				continue
			}
			c := sipChallenge{algorithm: "MD5"}
			for key, text := range sipAuthParams(params) {
				switch key {
				case "realm":
					c.realm = text
				case "nonce":
					c.nonce = text
				case "opaque":
					c.opaque = text
				case "algorithm":
					c.algorithm = strings.ToUpper(text)
				case "stale":
					c.stale = strings.EqualFold(text, "true")
				case "qop":
					for _, option := range strings.Split(text, ",") {
						c.qop = append(c.qop, strings.ToLower(strings.TrimSpace(option)))
					}
				}
			}
			found = append(found, c)
		}
	}
	return found
}

// sipStale says a challenge says only that the nonce went stale, so the
// same credentials are worth another request with the new one.
func sipStale(challenges []sipChallenge) bool {
	return slices.ContainsFunc(challenges, func(c sipChallenge) bool { return c.stale })
}

// sipAuthParams reads the parameters of a challenge, name=value or
// name="value" with a backslash before a quote in it, between commas, by
// name in lower case.
func sipAuthParams(text string) map[string]string {
	params := map[string]string{}
	for text = strings.TrimSpace(text); text != ""; text = strings.TrimLeft(text, ", \t") {
		name, rest, found := strings.Cut(text, "=")
		if !found {
			break
		}
		name, rest = strings.ToLower(strings.TrimSpace(name)), strings.TrimLeft(rest, " \t")
		var value strings.Builder
		if strings.HasPrefix(rest, `"`) {
			i := 1
			for ; i < len(rest) && rest[i] != '"'; i++ {
				if rest[i] == '\\' && i+1 < len(rest) {
					i++
				}
				value.WriteByte(rest[i])
			}
			text = rest[min(i+1, len(rest)):]
		} else {
			end := strings.IndexByte(rest, ',')
			if end < 0 {
				end = len(rest)
			}
			value.WriteString(strings.TrimSpace(rest[:end]))
			text = rest[end:]
		}
		if _, twice := params[name]; !twice {
			params[name] = value.String()
		}
	}
	return params
}

// sipDigests are the algorithms of a challenge that are spoken, strongest
// first, each with its hash.
var sipDigests = []struct {
	name string
	hash func() hash.Hash
}{
	{"SHA-256", sha256.New}, {"SHA-256-SESS", sha256.New},
	{"MD5", md5.New}, {"MD5-SESS", md5.New},
}

// sipDigest is the credentials that answer the strongest challenge that is
// spoken, the value of an Authorization header (RFC 7616, as RFC 8760 has
// SIP use it): the response is the hash of the account's, the nonce and
// the request's, with a client nonce and a count when the server asks for
// qop auth.
func sipDigest(challenges []sipChallenge, username, password, uri string) (string, error) {
	for _, digest := range sipDigests {
		for _, c := range challenges {
			if c.algorithm != digest.name || c.nonce == "" || len(c.qop) > 0 && !slices.Contains(c.qop, "auth") {
				continue
			}
			sum := func(parts ...string) string {
				h := digest.hash()
				h.Write([]byte(strings.Join(parts, ":")))
				return hex.EncodeToString(h.Sum(nil))
			}
			cnonce := sipToken(8)
			ha1 := sum(username, c.realm, password)
			if strings.HasSuffix(c.algorithm, "-SESS") {
				ha1 = sum(ha1, c.nonce, cnonce)
			}
			ha2 := sum(sipMethod, uri)
			quote := func(text string) string {
				return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(text) + `"`
			}
			algorithm, _ := strings.CutSuffix(c.algorithm, "-SESS")
			if algorithm != c.algorithm {
				algorithm += "-sess"
			}
			fields := []string{"username=" + quote(username), "realm=" + quote(c.realm), "nonce=" + quote(c.nonce), "uri=" + quote(uri)}
			if len(c.qop) > 0 {
				const count = "00000001"
				fields = append(fields, "response="+quote(sum(ha1, c.nonce, count, cnonce, "auth", ha2)), "algorithm="+algorithm, "cnonce="+quote(cnonce), "qop=auth", "nc="+count)
			} else {
				fields = append(fields, "response="+quote(sum(ha1, c.nonce, ha2)), "algorithm="+algorithm)
				if strings.HasSuffix(c.algorithm, "-SESS") {
					fields = append(fields, "cnonce="+quote(cnonce))
				}
			}
			if c.opaque != "" {
				fields = append(fields, "opaque="+quote(c.opaque))
			}
			return "Digest " + strings.Join(fields, ", "), nil
		}
	}
	if len(challenges) == 0 {
		return "", errors.New("its challenge is not Digest, the one scheme this type answers")
	}
	return "", fmt.Errorf("its Digest challenge is of algorithm %s, without a nonce, or of a qop other than auth; MD5 and SHA-256 are answered, with qop auth or none", sipShownLine(challenges[0].algorithm))
}
