//go:build request_type_nats

package fetch

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The nats request type uses the client protocol to ask a NATS server
// whether it answers, how fast, and, with JetStream, the state of its
// streams and consumers. So a server whose monitoring port is closed can
// still be watched by a client allowed to connect. Its target is the
// server: host or host:port (port 4222), or tls://host for a connection
// that must use TLS.
//
//	request:
//	  type: nats
//	  jetstream: true
//	  streams: [ORDERS]
//	  consumers: [ORDERS/billing]
//
// A scrape connects and reads the server's INFO. It switches the
// connection to TLS when the server requires it or the target says
// tls://, logs in with CONNECT, and times a PING until its PONG. With
// jetstream it asks for the account's JetStream state and for each named
// stream and consumer, using JetStream API requests that read and change
// nothing. The answer is one JSON document: the server's INFO, the round
// trip, and the JetStream answers. A stream or consumer the server does not
// have is not a failure. Its entry says found: false, and a rule decides.
//
// The account logs in with a user and a password or with a token. NKeys,
// credentials files and account JWTs are not supported.

// NATSConfig is a nats collector's own request keys.
type NATSConfig struct {
	// JetStream asks the account's JetStream state.
	JetStream bool `yaml:"jetstream"`
	// Streams are the streams asked about, by name, and Consumers the
	// consumers, each as stream/consumer.
	Streams   []string `yaml:"streams"`
	Consumers []string `yaml:"consumers"`
}

// natsRequest is a nats collector's own keys.
func natsRequest(c *model.Collector) NATSConfig {
	own, _ := c.Request.Own.(NATSConfig)
	return own
}

const (
	natsMethod  = "PING"
	natsTimeout = 30 * time.Second
	// natsMaxNames bounds request.streams and request.consumers, and
	// natsMaxLine the bytes of a line of the protocol, an INFO among them.
	natsMaxNames = 100
	natsMaxLine  = 1 << 20
)

// natsPorts gives each scheme of a target its port.
var natsPorts = map[string]int{"": 4222, "nats": 4222, "tls": 4222}

// natsName is the name of a stream or of a consumer, which a subject of
// the JetStream API holds as one token.
var natsName = regexp.MustCompile(`^[^\s.*>/\\\x00-\x1f\x7f]{1,255}$`)

func init() {
	registerRequestType(&RequestType{
		Name:         RequestTypeNATS,
		Fields:       []string{"username", "password_file", "bearer_token_file", "tls", "max_response_bytes", "allowed_targets", "denied_targets"},
		Config:       NATSConfig{},
		KeyRules:     natsKeyRules,
		Overrides:    []string{"timeout", PathParamPrefix},
		TargetFields: []string{"timeout"},
		Requester:    natsRequester{},
		JSONAnswer:   true,
		TargetHint:   func(*model.Collector) (string, string) { return "NATS server", "host, host:4222 or tls://host" },
		AnswerHint:   "Paste the answer as the JSON document the exporter makes of it, as the Response of a run shows it",
		UnusedParams: func(c *model.Collector, _ RequestOverrides, unused string) error {
			return unusedParamsIn(c, unused, "its label values")
		},
		Stage: "nats",
		CheckFiles: func(c *model.Collector, t *model.StaticTarget) []string {
			if t != nil {
				return nil
			}
			warnings := brokerFileWarnings(c, c.Request.TLS != (model.TLSConfig{}))
			if _, err := natsToken(c); err != nil {
				warnings = append(warnings, fmt.Sprintf("collector %q: %v; a scrape would fail on it now (the file is read at each scrape, so one put in place later is used then)", c.Name, err))
			}
			return warnings
		},
	})
}

// natsRequester is the nats request type's Requester.
type natsRequester struct{ parsedTarget[natsTarget] }

// natsTarget reads a nats target: label returns the url label of a
// request of it.
type natsTarget struct{}

func (natsTarget) label(target string) (string, error) {
	address, err := parseNetTarget("nats", target, natsPorts, false)
	if err != nil {
		return "", err
	}
	return natsLabel(address), nil
}

// Validate checks a nats collector's request: streams and consumers that
// are names, which need jetstream, and one way of logging in.
func (natsRequester) Validate(c *model.Collector) error {
	r := natsRequest(c)
	switch {
	case len(r.Streams) > natsMaxNames || len(r.Consumers) > natsMaxNames:
		return fmt.Errorf("collector %q request.streams or request.consumers names more than %d; a scrape asks about at most that many of each", c.Name, natsMaxNames)
	case (len(r.Streams) > 0 || len(r.Consumers) > 0) && !r.JetStream:
		return fmt.Errorf("collector %q sets request.streams or request.consumers without request.jetstream; streams and consumers are JetStream's, so set jetstream: true", c.Name)
	case c.Request.BearerTokenFile != "" && (c.Request.Username != "" || c.Request.PasswordFile != ""):
		return fmt.Errorf("collector %q sets request.bearer_token_file beside request.username or password_file; a NATS client logs in with a token or with a user and a password", c.Name)
	case HasPathParams(c.Request.BearerTokenFile):
		return fmt.Errorf("collector %q request.bearer_token_file has a placeholder; the token is the configuration's", c.Name)
	}
	seen := map[string]bool{}
	for i, stream := range r.Streams {
		switch {
		case !natsName.MatchString(stream):
			return fmt.Errorf("collector %q request.streams[%d] %q is no name of a stream; one has no blank, dot, *, > or slash", c.Name, i, stream)
		case seen[stream]:
			return fmt.Errorf("collector %q request.streams names %q twice", c.Name, stream)
		}
		seen[stream] = true
	}
	seen = map[string]bool{}
	for i, consumer := range r.Consumers {
		stream, name, found := strings.Cut(consumer, "/")
		switch {
		case !found || !natsName.MatchString(stream) || !natsName.MatchString(name):
			return fmt.Errorf("collector %q request.consumers[%d] %q is not written stream/consumer, each a name without a blank, dot, *, > or slash", c.Name, i, consumer)
		case seen[consumer]:
			return fmt.Errorf("collector %q request.consumers names %q twice", c.Name, consumer)
		}
		seen[consumer] = true
	}
	return checkBrokerAccount(c)
}

// natsKeyRules is what the configuration schema says of a nats collector's
// own keys.
func natsKeyRules() map[string]map[string]any {
	return map[string]map[string]any{
		"jetstream":   {"description": "nats: ask the account's JetStream state, its streams, consumers and storage, with the JetStream API; false when left out. Required for streams and consumers."},
		"streams":     {"maxItems": natsMaxNames, "description": "nats: the streams asked about, by name, at most 100: each one's messages, bytes, sequences and consumers."},
		"streams[]":   {"pattern": `^[^\s.*>/\\\x00-\x1f\x7f]{1,255}$`},
		"consumers":   {"maxItems": natsMaxNames, "description": "nats: the consumers asked about, each written stream/consumer, at most 100: each one's pending, unacknowledged and redelivered messages."},
		"consumers[]": {"pattern": `^[^\s.*>/\\\x00-\x1f\x7f]{1,255}/[^\s.*>/\\\x00-\x1f\x7f]{1,255}$`},
	}
}

// Fetch performs one request of a nats collector.
func (natsRequester) Fetch(ctx context.Context, target string, c *model.Collector, overrides RequestOverrides, _ http.Header) (*HTTPResponse, error) {
	return fetchNATS(ctx, target, c, overrides)
}

// natsLabel is the url label of a request to address: nats:// and the
// server, tls:// for a target written so.
func natsLabel(address netAddress) string {
	if address.scheme == "tls" {
		return "tls://" + address.hostPort
	}
	return "nats://" + address.hostPort
}

// Method is the http_method label of a nats request: what a scrape times.
func (natsRequester) Method(*model.Collector, RequestOverrides) string { return natsMethod }

// natsToken reads request.bearer_token_file, "" when the collector names
// none; an empty file is a failure.
func natsToken(c *model.Collector) (string, error) {
	if c.Request.BearerTokenFile == "" {
		return "", nil
	}
	token, err := ReadCredentialFile(c.Request.BearerTokenFile)
	if err != nil {
		return "", fmt.Errorf("reading request.bearer_token_file: %w", err)
	}
	if token == "" {
		return "", fmt.Errorf("request.bearer_token_file %s is empty", c.Request.BearerTokenFile)
	}
	return token, nil
}

// natsAnswer is what the JetStream API answered about one thing: Found is
// false of one the server does not have, Error says what else it refused
// with, and Info is its answer otherwise, as the API writes it.
type natsAnswer struct {
	Stream string          `json:"stream,omitempty"`
	Name   string          `json:"name"`
	Found  bool            `json:"found"`
	Error  string          `json:"error"`
	Info   json.RawMessage `json:"info"`
}

// natsDocument is a server's answer as the transforms read it.
type natsDocument struct {
	Host string `json:"host"`
	Port int    `json:"port"`
	// Address is the address the connection was made to, host:port.
	Address string `json:"address"`
	TLS     bool   `json:"tls"`
	// Info is the server's INFO, as it sent it, without its nonce.
	Info map[string]any `json:"info"`
	// ConnectSeconds is how long it took to connect and log in, and
	// RTTSeconds a PING's round trip after it.
	ConnectSeconds float64 `json:"connect_seconds"`
	RTTSeconds     float64 `json:"rtt_seconds"`
	// JetStream is the account's JetStream state as the API answers it,
	// null when it was not asked for or the account has none, which
	// JetStreamError then says.
	JetStream       json.RawMessage `json:"jetstream"`
	JetStreamError  string          `json:"jetstream_error"`
	Streams         []natsAnswer    `json:"streams"`
	Consumers       []natsAnswer    `json:"consumers"`
	DurationSeconds float64         `json:"duration_seconds"`
}

// fetchNATS asks a server what a scrape asks and answers as JSON.
func fetchNATS(ctx context.Context, target string, c *model.Collector, overrides RequestOverrides) (*HTTPResponse, error) {
	start := time.Now()
	address, err := parseNetTarget("nats", target, natsPorts, false)
	if err != nil {
		return nil, err
	}
	ctx, cancel := netBudget(ctx, overrides, natsTimeout)
	defer cancel()
	if ctx, err = netGuard(ctx, c, "nats", address.hostPort); err != nil {
		return nil, err
	}
	password, err := netPassword(c)
	if err != nil {
		return nil, err
	}
	token, err := natsToken(c)
	if err != nil {
		return nil, err
	}
	at := traceRequestAt(ctx, natsMethod, natsLabel(address), nil, "", false)
	doc, err := natsAsk(ctx, c, address, password, token)
	if err != nil {
		at.outcome("error: "+err.Error(), true)
		return nil, err
	}
	at.outcome(fmt.Sprintf("PONG after %s", time.Duration(doc.RTTSeconds*float64(time.Second)).Round(time.Microsecond)), false)
	_, port, _ := net.SplitHostPort(address.hostPort)
	doc.Host = address.host
	doc.Port, _ = strconv.Atoi(port)
	doc.DurationSeconds = time.Since(start).Seconds()
	return jsonAnswer(doc, target, c, start)
}

// natsConn is a connection of a scrape, read line by line.
type natsConn struct {
	*netConn
	ctx     context.Context
	address netAddress
	limit   int64
}

// errNATSProtocol marks an answer that is not the NATS client protocol.
var errNATSProtocol = errors.New("not the NATS protocol")

// fail is the failure of a read or a write.
func (c *natsConn) fail(doing string, err error) error {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return fmt.Errorf("%s closed the connection while %s", c.address.hostPort, doing)
	}
	return netFailure(c.ctx, fmt.Errorf("%s %s: %w", doing, c.address.hostPort, err))
}

// write sends text.
func (c *natsConn) write(text string) error {
	if _, err := io.WriteString(c.Conn, text); err != nil {
		return c.fail("writing to", err)
	}
	return nil
}

// line reads one line of the protocol, without its end.
func (c *natsConn) line() (string, error) {
	var line []byte
	for {
		part, err := c.r.ReadSlice('\n')
		line = append(line, part...)
		if len(line) > natsMaxLine {
			return "", fmt.Errorf("%s sent a line longer than %d bytes, which is %w", c.address.hostPort, natsMaxLine, errNATSProtocol)
		}
		if err == nil {
			return strings.TrimRight(string(line), "\r\n"), nil
		}
		if !errors.Is(err, bufio.ErrBufferFull) {
			return "", c.fail("reading from", err)
		}
	}
}

// message is a message the server delivered: its payload, and the status
// its headers begin with, 0 when it has none.
type natsMessage struct {
	status  int
	payload []byte
}

// next reads what the server sends until one of want arrives. It answers
// the server's PINGs and skips its +OK and INFO. An -ERR fails the scrape
// with its text. It returns the line that arrived and, for a message, the
// message.
func (c *natsConn) next(want ...string) (string, *natsMessage, error) {
	for {
		line, err := c.line()
		if err != nil {
			return "", nil, err
		}
		verb, rest, _ := strings.Cut(line, " ")
		verb = strings.ToUpper(verb)
		switch verb {
		case "PING":
			if err := c.write("PONG\r\n"); err != nil {
				return "", nil, err
			}
			continue
		case "+OK", "INFO":
			if !containsFold(want, verb) {
				continue
			}
		case "-ERR":
			return "", nil, fmt.Errorf("%s refused: %s", c.address.hostPort, brokerSaid(strings.Trim(rest, "' ")))
		}
		if !containsFold(want, verb) {
			return "", nil, fmt.Errorf("%s sent %s where %s was expected, which is %w", c.address.hostPort, strconv.Quote(model.CutTo(line, 80, false)), strings.Join(want, " or "), errNATSProtocol)
		}
		if verb != "MSG" && verb != "HMSG" {
			return line, nil, nil
		}
		// MSG subject sid [reply] bytes; HMSG subject sid [reply] header
		// bytes, total bytes.
		fields := strings.Fields(rest)
		sizes := 1
		if verb == "HMSG" {
			sizes = 2
		}
		if len(fields) < 2+sizes {
			return "", nil, fmt.Errorf("%s sent a %s that does not parse, which is %w", c.address.hostPort, verb, errNATSProtocol)
		}
		total, err := strconv.Atoi(fields[len(fields)-1])
		header := 0
		if err == nil && verb == "HMSG" {
			header, err = strconv.Atoi(fields[len(fields)-2])
		}
		if err != nil || total < 0 || header < 0 || header > total {
			return "", nil, fmt.Errorf("%s sent a %s that does not parse, which is %w", c.address.hostPort, verb, errNATSProtocol)
		}
		if int64(total) > c.limit {
			return "", nil, model.MarkError(model.Errorf("an answer of %s is %d bytes, larger than the response limit of %d bytes; raise request.max_response_bytes or limits.max_response_bytes", c.address.hostPort, model.Size(int64(total)), model.Size(c.limit)), model.ErrLimitExceeded)
		}
		data := make([]byte, total+2)
		if _, err := io.ReadFull(c.r, data); err != nil {
			return "", nil, c.fail("reading from", err)
		}
		message := &natsMessage{payload: data[header:total]}
		if header > 0 {
			// NATS/1.0 503 is no responders: nothing answers the subject.
			first, _, _ := strings.Cut(string(data[:header]), "\r\n")
			if parts := strings.Fields(first); len(parts) > 1 {
				message.status, _ = strconv.Atoi(parts[1])
			}
		}
		return line, message, nil
	}
}

func containsFold(list []string, value string) bool {
	for _, item := range list {
		if strings.EqualFold(item, value) {
			return true
		}
	}
	return false
}

// natsAsk connects, logs in, times a PING and asks JetStream what the
// collector asks of it.
func natsAsk(ctx context.Context, c *model.Collector, address netAddress, password, token string) (natsDocument, error) {
	start := time.Now()
	var doc natsDocument
	plain, err := netDial(ctx, address, nil)
	if err != nil {
		return doc, err
	}
	defer plain.close()
	conn := &natsConn{netConn: plain, ctx: ctx, address: address, limit: responseLimit(c)}
	doc.Address = conn.RemoteAddr().String()
	line, _, err := conn.next("INFO")
	if err != nil {
		if errors.Is(err, errNATSProtocol) {
			err = fmt.Errorf("%s does not speak the NATS protocol: it did not begin with INFO", address.hostPort)
		}
		return doc, err
	}
	_, written, _ := strings.Cut(line, " ")
	if err := json.Unmarshal([]byte(written), &doc.Info); err != nil || doc.Info == nil {
		return doc, fmt.Errorf("%s sent an INFO that is not JSON, which is %w", address.hostPort, errNATSProtocol)
	}
	delete(doc.Info, "nonce")
	required, _ := doc.Info["tls_required"].(bool)
	available, _ := doc.Info["tls_available"].(bool)
	secure := address.scheme == "tls" || required
	switch {
	case address.scheme == "tls" && !required && !available:
		return doc, fmt.Errorf("%s offers no TLS, and the target says tls://", address.hostPort)
	case secure:
		tlsSettings, err := brokerTLSConfig(c)
		if err != nil {
			return doc, err
		}
		if err := conn.secure(ctx, tlsSettings, address.host, address.hostPort); err != nil {
			return doc, err
		}
		doc.TLS = true
	}
	// A credential goes over TLS, or to a server the operator chose to
	// reach in plaintext by naming no TLS: the protocol sends it as text.
	connect := map[string]any{"verbose": false, "pedantic": false, "tls_required": secure, "name": "prometheus-universal-exporter", "lang": "go", "version": "1", "protocol": 1, "headers": true, "no_responders": true}
	switch {
	case token != "":
		connect["auth_token"] = token
	case c.Request.Username != "":
		connect["user"], connect["pass"] = c.Request.Username, password
	}
	options, err := json.Marshal(connect)
	if err != nil {
		return doc, err
	}
	if err := conn.write("CONNECT " + string(options) + "\r\nPING\r\n"); err != nil {
		return doc, err
	}
	if _, _, err := conn.next("PONG"); err != nil {
		return doc, natsLoginFailure(address, err)
	}
	doc.ConnectSeconds = time.Since(start).Seconds()
	pinged := time.Now()
	if err := conn.write("PING\r\n"); err != nil {
		return doc, err
	}
	if _, _, err := conn.next("PONG"); err != nil {
		return doc, err
	}
	doc.RTTSeconds = time.Since(pinged).Seconds()
	doc.Streams, doc.Consumers = []natsAnswer{}, []natsAnswer{}
	r := natsRequest(c)
	if !r.JetStream {
		return doc, nil
	}
	inbox := make([]byte, 12)
	_, _ = rand.Read(inbox)
	reply := "_INBOX." + hex.EncodeToString(inbox)
	if err := conn.write("SUB " + reply + ".* 1\r\n"); err != nil {
		return doc, err
	}
	asked := 0
	ask := func(subject string) (json.RawMessage, string, bool, error) {
		asked++
		if err := conn.write(fmt.Sprintf("PUB %s %s.%d 0\r\n\r\n", subject, reply, asked)); err != nil {
			return nil, "", false, err
		}
		_, message, err := conn.next("MSG", "HMSG")
		if err != nil {
			return nil, "", false, err
		}
		if message.status == http.StatusServiceUnavailable {
			return nil, "nothing answers the JetStream API: JetStream is not enabled on this server, or not for this account", false, nil
		}
		var answer struct {
			Error *struct {
				Code        int    `json:"code"`
				Description string `json:"description"`
			} `json:"error"`
		}
		if err := json.Unmarshal(message.payload, &answer); err != nil {
			return nil, "", false, fmt.Errorf("%s answered %s with what is not JSON, which is %w", address.hostPort, subject, errNATSProtocol)
		}
		if answer.Error != nil {
			return nil, fmt.Sprintf("%d %s", answer.Error.Code, brokerSaid(answer.Error.Description)), answer.Error.Code == http.StatusNotFound, nil
		}
		return json.RawMessage(message.payload), "", false, nil
	}
	account, refused, _, err := ask("$JS.API.INFO")
	if err != nil {
		return doc, err
	}
	doc.JetStream, doc.JetStreamError = account, refused
	if account == nil {
		return doc, nil
	}
	for _, stream := range r.Streams {
		info, refused, missing, err := ask("$JS.API.STREAM.INFO." + stream)
		if err != nil {
			return doc, err
		}
		answer := natsAnswer{Name: stream, Found: info != nil, Info: info}
		if !missing {
			answer.Error = refused
		}
		doc.Streams = append(doc.Streams, answer)
	}
	for _, consumer := range r.Consumers {
		stream, name, _ := strings.Cut(consumer, "/")
		info, refused, missing, err := ask("$JS.API.CONSUMER.INFO." + stream + "." + name)
		if err != nil {
			return doc, err
		}
		answer := natsAnswer{Stream: stream, Name: name, Found: info != nil, Info: info}
		if !missing {
			answer.Error = refused
		}
		doc.Consumers = append(doc.Consumers, answer)
	}
	return doc, nil
}

// natsLoginFailure words a login the server did not take.
func natsLoginFailure(address netAddress, err error) error {
	if strings.Contains(err.Error(), "closed the connection") {
		return fmt.Errorf("%s closed the connection at the login, as a server does to an account it does not take", address.hostPort)
	}
	return err
}
