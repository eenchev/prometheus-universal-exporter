//go:build request_type_amqp

package fetch

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The amqp request type asks an AMQP 0-9-1 broker, RabbitMQ and the
// brokers that speak its protocol, how deep its queues are, over AMQP
// itself: where the management API is closed, a client that may connect
// can still ask. Its target is the broker, host or host:port, port 5672, or
// amqps://host for TLS on port 5671.
//
//	request:
//	  type: amqp
//	  username: monitor
//	  password_file: /etc/exporter/amqp-password
//	  queues: [orders, emails]
//
// A scrape opens a connection, logs in with PLAIN, opens the virtual host,
// and declares each queue passively, which creates nothing and answers how
// many messages are ready in it and how many consumers it has. It then
// closes the connection as the protocol has it. The answer is one JSON
// document: what the broker says of itself, how long the login took, and
// each queue with its counts. A queue the broker does not have is not a
// failure: its entry says exists: false, so a rule decides.
//
// Only the methods this needs are spoken: no message is published,
// consumed or acknowledged, and nothing is declared but passively.

// AMQPConfig is an amqp collector's own request keys.
type AMQPConfig struct {
	// VHost is the virtual host opened, "/" when left out.
	VHost string `yaml:"vhost"`
	// Queues are the queues asked about, by name.
	Queues []string `yaml:"queues"`
}

// amqpRequest is an amqp collector's own keys.
func amqpRequest(c *model.Collector) AMQPConfig {
	own, _ := c.Request.Own.(AMQPConfig)
	return own
}

const (
	amqpMethod  = "DECLARE-PASSIVE"
	amqpTimeout = 30 * time.Second
	// amqpMaxQueues and amqpMaxName bound request.queues: how many, and
	// the bytes of a name, which the protocol writes with one byte of
	// length.
	amqpMaxQueues = 100
	amqpMaxName   = 255
	// amqpMaxFrame is the largest frame read: the broker's first, with its
	// properties, is a few hundred bytes, and a frame's size is the
	// broker's to say.
	amqpMaxFrame = 1 << 20
	// amqpDefaultUser is the account of a collector that names none, which
	// a broker out of the box takes from its own host alone.
	amqpDefaultUser = "guest"

	amqpFrameMethod    = 1
	amqpFrameHeartbeat = 8
	amqpFrameEnd       = 0xCE

	// The classes and methods spoken (AMQP 0-9-1 section 1.4 onwards).
	amqpConnection = 10
	amqpChannel    = 20
	amqpQueue      = 50

	amqpStart, amqpStartOk = 10, 11
	amqpTune, amqpTuneOk   = 30, 31
	amqpOpen, amqpOpenOk   = 40, 41
	amqpClose, amqpCloseOk = 50, 51 // of a connection
	amqpChannelOpen        = 10
	amqpChannelOpenOk      = 11
	amqpChannelClose       = 40
	amqpChannelCloseOk     = 41
	amqpDeclare            = 10
	amqpDeclareOk          = 11
	amqpNotFound           = 404
)

// amqpPorts gives each scheme of a target its port.
var amqpPorts = map[string]int{"": 5672, "amqp": 5672, "amqps": 5671}

func init() {
	registerRequestType(&RequestType{
		Name:         RequestTypeAMQP,
		Fields:       []string{"username", "password_file", "tls", "allowed_targets", "denied_targets"},
		Config:       AMQPConfig{},
		KeyRules:     amqpKeyRules,
		Overrides:    []string{"timeout", PathParamPrefix},
		TargetFields: []string{"timeout"},
		Requester:    amqpRequester{},
		JSONAnswer:   true,
		TargetHint:   func(*model.Collector) (string, string) { return "AMQP broker", "host, host:5672 or amqps://host" },
		AnswerHint:   "Paste the answer as the JSON document the exporter makes of it, as the Response of a run shows it",
		UnusedParams: func(c *model.Collector, _ RequestOverrides, unused string) error {
			return unusedParamsIn(c, unused, "its label values")
		},
		Stage: "amqp",
		CheckFiles: func(c *model.Collector, t *model.StaticTarget) []string {
			if t != nil {
				return nil
			}
			return brokerFileWarnings(c, c.Request.TLS != (model.TLSConfig{}))
		},
	})
}

// amqpRequester is the amqp request type's Requester.
type amqpRequester struct{}

// Validate checks an amqp collector's request and fills in its defaults:
// the virtual host, queues that are names, each once, and an account that
// is whole.
func (amqpRequester) Validate(c *model.Collector) error {
	r := amqpRequest(c)
	if r.VHost == "" {
		r.VHost = "/"
	}
	switch {
	case len(r.VHost) > amqpMaxName || strings.ContainsFunc(r.VHost, func(c rune) bool { return c < 0x20 || c == 0x7f }) || HasPathParams(r.VHost):
		return fmt.Errorf("collector %q request.vhost is longer than %d bytes, or has a control character or a placeholder; it is the name of the virtual host, used as it is written", c.Name, amqpMaxName)
	case len(r.Queues) > amqpMaxQueues:
		return fmt.Errorf("collector %q request.queues names %d queues; a scrape asks about at most %d", c.Name, len(r.Queues), amqpMaxQueues)
	}
	seen := map[string]bool{}
	for i, queue := range r.Queues {
		switch {
		case queue == "" || len(queue) > amqpMaxName:
			return fmt.Errorf("collector %q request.queues[%d] is empty or longer than %d bytes; name the queue", c.Name, i, amqpMaxName)
		case strings.ContainsFunc(queue, func(c rune) bool { return c < 0x20 || c == 0x7f }) || HasPathParams(queue):
			return fmt.Errorf("collector %q request.queues[%d] has a control character or a placeholder; a queue's name is used as it is written", c.Name, i)
		case seen[queue]:
			return fmt.Errorf("collector %q request.queues names %q twice", c.Name, queue)
		}
		seen[queue] = true
	}
	if err := checkBrokerAccount(c); err != nil {
		return err
	}
	c.Request.Own = r
	return nil
}

// amqpKeyRules is what the configuration schema says of an amqp
// collector's own keys.
func amqpKeyRules() map[string]map[string]any {
	return map[string]map[string]any{
		"vhost":    {"maxLength": amqpMaxName, "pattern": `^[^\x00-\x1f\x7f]*$`, "description": "amqp: the virtual host opened; / when left out."},
		"queues":   {"maxItems": amqpMaxQueues, "description": "amqp: the queues asked about, by name, at most 100: each is declared passively, which creates nothing and answers its ready messages and its consumers. Left out, the scrape logs in and asks about none."},
		"queues[]": {"minLength": 1, "maxLength": amqpMaxName, "pattern": `^[^\x00-\x1f\x7f]+$`},
	}
}

// Fetch performs one request of an amqp collector.
func (amqpRequester) Fetch(ctx context.Context, target string, c *model.Collector, overrides RequestOverrides, _ http.Header) (*HTTPResponse, error) {
	return fetchAMQP(ctx, target, c, overrides)
}

// CheckTarget checks an amqp collector's target.
func (amqpRequester) CheckTarget(_ *model.Collector, target string, _ bool) error {
	_, err := parseNetTarget("amqp", target, amqpPorts, false)
	return err
}

// amqpLabel is the url label of a request to address: amqp:// or amqps://
// and the broker.
func amqpLabel(address netAddress) string {
	if address.scheme == "amqps" {
		return "amqps://" + address.hostPort
	}
	return "amqp://" + address.hostPort
}

// Label is the url label of an amqp request.
func (amqpRequester) Label(target string, _ *model.Collector, _ RequestOverrides) (string, error) {
	address, err := parseNetTarget("amqp", target, amqpPorts, false)
	if err != nil {
		return "", err
	}
	return amqpLabel(address), nil
}

// Method is the http_method label of an amqp request: what a scrape does.
func (amqpRequester) Method(*model.Collector, RequestOverrides) string { return amqpMethod }

// Display renders an amqp target for logs and labels: as written, since it
// holds no credentials, when it is one at all.
func (amqpRequester) Display(target string) string {
	if _, err := parseNetTarget("amqp", target, amqpPorts, false); err != nil {
		return "<invalid target>"
	}
	return strings.TrimSpace(target)
}

// amqpQueueState is one queue of the document.
type amqpQueueState struct {
	Name string `json:"name"`
	// Exists is false of a queue the broker does not have, and of one it
	// would not say of, whose Error says why.
	Exists bool `json:"exists"`
	// Messages is the messages ready for delivery, those delivered and
	// not yet acknowledged not among them, and Consumers the consumers.
	Messages  *int   `json:"messages"`
	Consumers *int   `json:"consumers"`
	Error     string `json:"error"`
}

// amqpDocument is a broker's answer as the transforms read it.
type amqpDocument struct {
	Host string `json:"host"`
	Port int    `json:"port"`
	// Address is the address the connection was made to, host:port.
	Address string `json:"address"`
	TLS     bool   `json:"tls"`
	VHost   string `json:"vhost"`
	// Product, Version, Platform and ClusterName are the broker's
	// properties of those names, and ServerProperties every one that is
	// text.
	Product          string            `json:"product"`
	Version          string            `json:"version"`
	Platform         string            `json:"platform"`
	ClusterName      string            `json:"cluster_name"`
	ServerProperties map[string]string `json:"server_properties"`
	// Mechanisms are the logins the broker offers.
	Mechanisms []string `json:"mechanisms"`
	// ConnectSeconds is how long it took to connect, log in and open the
	// virtual host.
	ConnectSeconds  float64          `json:"connect_seconds"`
	Queues          []amqpQueueState `json:"queues"`
	DurationSeconds float64          `json:"duration_seconds"`
}

// fetchAMQP asks a broker about a scrape's queues and answers as JSON.
func fetchAMQP(ctx context.Context, target string, c *model.Collector, overrides RequestOverrides) (*HTTPResponse, error) {
	start := time.Now()
	address, err := parseNetTarget("amqp", target, amqpPorts, false)
	if err != nil {
		return nil, err
	}
	ctx, cancel := netBudget(ctx, overrides, amqpTimeout)
	defer cancel()
	if ctx, err = netGuard(ctx, c, "amqp", address.hostPort); err != nil {
		return nil, err
	}
	username, password := amqpDefaultUser, amqpDefaultUser
	if c.Request.Username != "" {
		username = c.Request.Username
		if password, err = netPassword(c); err != nil {
			return nil, err
		}
	}
	secure := address.scheme == "amqps"
	r := amqpRequest(c)
	at := traceRequestAt(ctx, amqpMethod, amqpLabel(address), http.Header{"Amqp-Vhost": {r.VHost}}, "", false)
	doc, err := amqpAsk(ctx, c, address, secure, username, password, r)
	if err != nil {
		at.outcome("error: "+err.Error(), true)
		return nil, err
	}
	at.outcome(fmt.Sprintf("%d queues asked of %s %s", len(doc.Queues), doc.Product, doc.Version), false)
	_, port, _ := net.SplitHostPort(address.hostPort)
	doc.Host, doc.TLS, doc.VHost = address.host, secure, r.VHost
	doc.Port, _ = strconv.Atoi(port)
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

// amqpConn is a connection of a scrape, read and written frame by frame.
type amqpConn struct {
	*netConn
	ctx     context.Context
	address netAddress
}

// errAMQPProtocol marks an answer that is not AMQP 0-9-1.
var errAMQPProtocol = errors.New("not AMQP 0-9-1")

// fail is the failure of a read or a write.
func (c *amqpConn) fail(doing string, err error) error {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return fmt.Errorf("%s closed the connection while %s", c.address.hostPort, doing)
	}
	return netFailure(c.ctx, fmt.Errorf("%s %s: %w", doing, c.address.hostPort, err))
}

// send writes one method frame on channel.
func (c *amqpConn) send(channel uint16, class, method uint16, arguments []byte) error {
	frame := make([]byte, 0, 12+len(arguments))
	frame = append(frame, amqpFrameMethod)
	frame = binary.BigEndian.AppendUint16(frame, channel)
	frame = binary.BigEndian.AppendUint32(frame, uint32(4+len(arguments))) //nolint:gosec // G115: the arguments are a few hundred bytes
	frame = binary.BigEndian.AppendUint16(frame, class)
	frame = binary.BigEndian.AppendUint16(frame, method)
	frame = append(frame, arguments...)
	frame = append(frame, amqpFrameEnd)
	if _, err := c.Write(frame); err != nil {
		return c.fail("writing to", err)
	}
	return nil
}

// receive reads the next method frame, passing over heartbeats, and
// returns its channel, class, method and arguments.
func (c *amqpConn) receive() (channel, class, method uint16, arguments []byte, err error) {
	for {
		var header [7]byte
		if _, err := io.ReadFull(c.r, header[:]); err != nil {
			return 0, 0, 0, nil, c.fail("reading from", err)
		}
		size := binary.BigEndian.Uint32(header[3:])
		if size > amqpMaxFrame {
			return 0, 0, 0, nil, fmt.Errorf("%s sent a frame of %d bytes, which is %w", c.address.hostPort, size, errAMQPProtocol)
		}
		payload := make([]byte, size+1)
		if _, err := io.ReadFull(c.r, payload); err != nil {
			return 0, 0, 0, nil, c.fail("reading from", err)
		}
		if payload[size] != amqpFrameEnd {
			return 0, 0, 0, nil, fmt.Errorf("%s sent a frame without its end, which is %w", c.address.hostPort, errAMQPProtocol)
		}
		switch header[0] {
		case amqpFrameHeartbeat:
			continue
		case amqpFrameMethod:
			if size < 4 {
				return 0, 0, 0, nil, fmt.Errorf("%s sent a method frame of %d bytes, which is %w", c.address.hostPort, size, errAMQPProtocol)
			}
			return binary.BigEndian.Uint16(header[1:]), binary.BigEndian.Uint16(payload), binary.BigEndian.Uint16(payload[2:]), payload[4:size], nil
		}
		// A content frame of a method this scrape never asks for.
	}
}

// expect reads the next method and holds it to class and method: a
// connection the broker closes instead fails with what the broker said.
func (c *amqpConn) expect(class, method uint16, doing string) (channel uint16, arguments []byte, err error) {
	channel, gotClass, gotMethod, arguments, err := c.receive()
	if err != nil {
		return 0, nil, err
	}
	switch {
	case gotClass == class && gotMethod == method:
		return channel, arguments, nil
	case gotClass == amqpConnection && gotMethod == amqpClose:
		code, text := amqpCloseReason(arguments)
		return 0, nil, fmt.Errorf("%s refused %s: %d %s", c.address.hostPort, doing, code, brokerSaid(text))
	}
	return channel, arguments, &amqpUnexpected{class: gotClass, method: gotMethod, arguments: arguments}
}

// amqpUnexpected is a method that came where another was expected.
type amqpUnexpected struct {
	class, method uint16
	arguments     []byte
}

func (e *amqpUnexpected) Error() string {
	return fmt.Sprintf("the broker answered method %d.%d where another was expected", e.class, e.method)
}

// amqpCloseReason is the reply code and text of a Close, a connection's or
// a channel's.
func amqpCloseReason(arguments []byte) (int, string) {
	r := amqpReader{b: arguments}
	code := r.short()
	return int(code), r.shortString()
}

// amqpReader reads the arguments of a method; a read past their end gives
// zero values, and bad says so.
type amqpReader struct {
	b   []byte
	bad bool
}

func (r *amqpReader) take(n int) []byte {
	if n < 0 || n > len(r.b) {
		r.bad, r.b = true, nil
		return make([]byte, max(n, 0))
	}
	taken := r.b[:n]
	r.b = r.b[n:]
	return taken
}
func (r *amqpReader) octet() byte         { return r.take(1)[0] }
func (r *amqpReader) short() uint16       { return binary.BigEndian.Uint16(r.take(2)) }
func (r *amqpReader) long() uint32        { return binary.BigEndian.Uint32(r.take(4)) }
func (r *amqpReader) shortString() string { return string(r.take(int(r.octet()))) }
func (r *amqpReader) longString() string {
	n := r.long()
	if n > amqpMaxFrame {
		r.bad, r.b = true, nil
		return ""
	}
	return string(r.take(int(n)))
}

// table reads a field table's text values, by name, a table inside it
// passed over: of the broker's properties the text ones are the ones a
// rule reads. A type it does not know ends the reading, with what was read
// kept.
func (r *amqpReader) table() map[string]string {
	text := map[string]string{}
	inner := amqpReader{b: []byte(r.longString())}
	for len(inner.b) > 0 && !inner.bad {
		name := inner.shortString()
		switch inner.octet() {
		case 'S', 'x':
			text[name] = strings.ToValidUTF8(inner.longString(), "?")
		case 't', 'b', 'B':
			inner.take(1)
		case 'U', 'u', 's':
			inner.take(2)
		case 'I', 'i', 'f':
			inner.take(4)
		case 'L', 'l', 'd', 'T':
			inner.take(8)
		case 'D':
			inner.take(5)
		case 'F', 'A':
			inner.longString()
		case 'V':
		default:
			return text
		}
	}
	return text
}

// amqpShortString writes text with its length in one byte.
func amqpShortString(b []byte, text string) []byte {
	return append(append(b, byte(len(text))), text...) //nolint:gosec // G115: a name is at most 255 bytes, as Validate holds each
}

// amqpLongString writes text with its length in four bytes.
func amqpLongString(b []byte, text string) []byte {
	return append(binary.BigEndian.AppendUint32(b, uint32(len(text))), text...) //nolint:gosec // G115: a password and a table of a few entries
}

// amqpAsk connects, logs in, opens the virtual host, asks about each
// queue and closes the connection.
func amqpAsk(ctx context.Context, c *model.Collector, address netAddress, secure bool, username, password string, r AMQPConfig) (amqpDocument, error) {
	start := time.Now()
	var doc amqpDocument
	var tlsSettings *tls.Config
	if secure {
		var err error
		if tlsSettings, err = brokerTLSConfig(c); err != nil {
			return doc, err
		}
	}
	plain, err := netDial(ctx, address, tlsSettings)
	if err != nil {
		return doc, err
	}
	defer plain.close()
	conn := &amqpConn{netConn: plain, ctx: ctx, address: address}
	doc.Address = conn.RemoteAddr().String()
	if _, err := conn.Write([]byte{'A', 'M', 'Q', 'P', 0, 0, 9, 1}); err != nil {
		return doc, conn.fail("writing to", err)
	}
	// A broker that speaks another protocol answers with the header of
	// the one it speaks, and closes.
	if first, err := conn.r.Peek(4); err == nil && string(first) == "AMQP" {
		return doc, fmt.Errorf("%s does not speak AMQP 0-9-1: it answered the header of another version of the protocol", address.hostPort)
	}
	_, arguments, err := conn.expect(amqpConnection, amqpStart, "the connection")
	if err != nil {
		return doc, amqpNotSpoken(address, err)
	}
	startArgs := amqpReader{b: arguments}
	startArgs.take(2)
	properties := startArgs.table()
	mechanisms := strings.Fields(startArgs.longString())
	if startArgs.bad {
		return doc, fmt.Errorf("%s sent a Connection.Start that does not parse, which is %w", address.hostPort, errAMQPProtocol)
	}
	doc.ServerProperties, doc.Mechanisms = properties, mechanisms
	doc.Product, doc.Version, doc.Platform, doc.ClusterName = properties["product"], properties["version"], properties["platform"], properties["cluster_name"]
	if !strings.Contains(" "+strings.Join(mechanisms, " ")+" ", " PLAIN ") {
		return doc, fmt.Errorf("%s offers the logins %s and not PLAIN, the one this type speaks", address.hostPort, brokerSaid(strings.Join(mechanisms, ", ")))
	}
	// Start-Ok: who is connecting, saying a refused login may be answered
	// with a Close that says so, and the PLAIN login.
	capabilities := amqpShortString(nil, "authentication_failure_close")
	capabilities = append(capabilities, 't', 1)
	client := amqpLongString(append(amqpShortString(nil, "product"), 'S'), "prometheus-universal-exporter")
	client = amqpLongString(append(amqpShortString(client, "capabilities"), 'F'), string(capabilities))
	startOk := amqpLongString(nil, string(client))
	startOk = amqpShortString(startOk, "PLAIN")
	startOk = amqpLongString(startOk, "\x00"+username+"\x00"+password)
	startOk = amqpShortString(startOk, "en_US")
	if err := conn.send(0, amqpConnection, amqpStartOk, startOk); err != nil {
		return doc, err
	}
	_, arguments, err = conn.expect(amqpConnection, amqpTune, "the login of "+username)
	if err != nil {
		if strings.Contains(err.Error(), "closed the connection") {
			err = fmt.Errorf("%s closed the connection at the login of %s, as a broker does to an account or a password it does not take", address.hostPort, username)
		}
		return doc, err
	}
	tune := amqpReader{b: arguments}
	channelMax, frameMax := tune.short(), tune.long()
	tuneOk := binary.BigEndian.AppendUint16(nil, channelMax)
	tuneOk = binary.BigEndian.AppendUint32(tuneOk, frameMax)
	tuneOk = binary.BigEndian.AppendUint16(tuneOk, 0)
	if err := conn.send(0, amqpConnection, amqpTuneOk, tuneOk); err != nil {
		return doc, err
	}
	open := append(amqpShortString(amqpShortString(nil, r.VHost), ""), 0)
	if err := conn.send(0, amqpConnection, amqpOpen, open); err != nil {
		return doc, err
	}
	if _, _, err := conn.expect(amqpConnection, amqpOpenOk, "the virtual host "+strconv.Quote(r.VHost)); err != nil {
		return doc, err
	}
	doc.ConnectSeconds = time.Since(start).Seconds()
	doc.Queues = []amqpQueueState{}
	// A channel a refusal closed is done with, so each queue after one
	// is asked on the next.
	channel, opened := uint16(1), false
	for _, queue := range r.Queues {
		if !opened {
			if channelMax != 0 && channel > channelMax {
				return doc, fmt.Errorf("%s allows %d channels, fewer than the queues it does not have need", address.hostPort, channelMax)
			}
			if err := conn.send(channel, amqpChannel, amqpChannelOpen, amqpShortString(nil, "")); err != nil {
				return doc, err
			}
			if _, _, err := conn.expect(amqpChannel, amqpChannelOpenOk, "a channel"); err != nil {
				return doc, err
			}
			opened = true
		}
		// reserved, the queue, passive alone of the bits, no arguments.
		declare := amqpShortString([]byte{0, 0}, queue)
		declare = amqpLongString(append(declare, 1), "")
		if err := conn.send(channel, amqpQueue, amqpDeclare, declare); err != nil {
			return doc, err
		}
		state := amqpQueueState{Name: queue}
		_, arguments, err := conn.expect(amqpQueue, amqpDeclareOk, "the queue "+strconv.Quote(queue))
		var other *amqpUnexpected
		switch {
		case err == nil:
			declared := amqpReader{b: arguments}
			declared.shortString()
			messages, consumers := int(declared.long()), int(declared.long())
			if declared.bad {
				return doc, fmt.Errorf("%s sent a Queue.Declare-Ok that does not parse, which is %w", address.hostPort, errAMQPProtocol)
			}
			state.Exists, state.Messages, state.Consumers = true, &messages, &consumers
		case errors.As(err, &other) && other.class == amqpChannel && other.method == amqpChannelClose:
			// The broker closed the channel: the queue is not there, or
			// is not this account's to ask about.
			code, text := amqpCloseReason(other.arguments)
			if code != amqpNotFound {
				state.Error = fmt.Sprintf("%d %s", code, brokerSaid(text))
			}
			if err := conn.send(channel, amqpChannel, amqpChannelCloseOk, nil); err != nil {
				return doc, err
			}
			channel, opened = channel+1, false
		default:
			return doc, err
		}
		doc.Queues = append(doc.Queues, state)
	}
	// Close as the protocol has it; a broker that is slow to say Close-Ok
	// has answered all the same.
	closing := amqpShortString(binary.BigEndian.AppendUint16(nil, 200), "")
	closing = append(closing, 0, 0, 0, 0)
	if conn.send(0, amqpConnection, amqpClose, closing) == nil {
		_ = conn.SetReadDeadline(time.Now().Add(time.Second))
		_, _, _ = conn.expect(amqpConnection, amqpCloseOk, "the close")
	}
	return doc, nil
}

// amqpNotSpoken words the failure of the first read: what answered is not
// an AMQP 0-9-1 broker.
func amqpNotSpoken(address netAddress, err error) error {
	var other *amqpUnexpected
	if errors.As(err, &other) || errors.Is(err, errAMQPProtocol) {
		return fmt.Errorf("%s does not speak AMQP 0-9-1: %w", address.hostPort, err)
	}
	return err
}
