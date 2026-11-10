//go:build request_type_mqtt

package fetch

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The mqtt request type reads messages from an MQTT broker: it connects,
// subscribes to the collector's topics, and hands the transforms the
// messages that came, as one JSON document.
//
//	request:
//	  type: mqtt
//	  topics: [sensors/+/temperature, plant/status]
//	  qos: 0                       # 0 (default) or 1
//	  window: 1s                   # how long a scrape reads; 1s when left out
//	  max_messages: 100            # reading ends when this many have come
//	  username: exporter           # optional
//	  password_file: /etc/exporter/mqtt-pass
//
// It stays a pull: a scrape asks the broker and answers with what the
// broker sent it; nothing reaches the exporter but what a collector
// subscribed to. Three ways of reading, by what the collector sets:
//
//   - Neither (the default): each scrape connects, subscribes, reads the
//     retained messages and whatever is published within window, and
//     disconnects. Between scrapes nothing is connected and nothing is kept.
//   - persistent_session: true, with a client_id: each scrape still
//     connects and disconnects, but asks the broker to keep its session, so
//     what was published at QoS 1 between scrapes is queued by the broker
//     and delivered to the next one.
//   - keep_connection: true: the connection made by the first scrape is
//     kept, and what comes on it is remembered, the last message of each
//     topic; a scrape answers with that at once. The connection is closed
//     when no scrape used it for idle_timeout, and made again by the next.
//
// The type only subscribes: no PUBLISH is in the code, but for the
// acknowledgements QoS 1 has. MQTT 3.1.1 (protocol level 4), by hand.
//
// The target is host, host:port, mqtt://host:port (port 1883) or
// mqtts://host:port (8883, TLS); a target without a scheme is TLS when the
// collector has a tls block.
//
// The type imports nothing outside the standard library.

// MQTTConfig is an mqtt collector's own request keys.
type MQTTConfig struct {
	Topics            []string       `yaml:"topics"`
	QoS               int            `yaml:"qos,omitempty"`
	Window            model.Duration `yaml:"window,omitempty"`
	MaxMessages       int            `yaml:"max_messages,omitempty"`
	ClientID          string         `yaml:"client_id,omitempty"`
	PersistentSession bool           `yaml:"persistent_session,omitempty"`
	KeepConnection    bool           `yaml:"keep_connection,omitempty"`
	KeepAlive         model.Duration `yaml:"keep_alive,omitempty"`
}

func mqttRequest(c *model.Collector) MQTTConfig {
	own, _ := c.Request.Own.(MQTTConfig)
	return own
}

const (
	// mqttTimeout bounds a scrape with no deadline of its own.
	mqttTimeout        = 30 * time.Second
	mqttDefaultWindow  = time.Second
	mqttDefaultMax     = 1000
	mqttMaxMessages    = 100000
	mqttMaxTopics      = 64
	mqttDefaultAlive   = 30 * time.Second
	mqttDefaultIdle    = 5 * time.Minute
	mqttTopicKind      = "mqtt_topic"
	mqttPacketOverhead = 1 << 10
)

// mqttMaxKept is the most connections kept at once (keep_connection); a
// test lowers it.
var mqttMaxKept = 256

// MQTT control packet types, in the high four bits of the first byte.
const (
	mqttConnect    = 1
	mqttConnack    = 2
	mqttPublish    = 3
	mqttPuback     = 4
	mqttSubscribe  = 8
	mqttSuback     = 9
	mqttPingreq    = 12
	mqttPingresp   = 13
	mqttDisconnect = 14
)

// mqttPorts are the schemes of an mqtt target and their default ports.
var mqttPorts = map[string]int{"": 1883, "mqtt": 1883, "mqtts": 8883}

// mqttRefusals are the return codes of a CONNACK that refuses.
var mqttRefusals = map[byte]string{
	1: "unacceptable protocol version: the broker does not speak MQTT 3.1.1",
	2: "identifier rejected: the broker does not take this client_id",
	3: "server unavailable",
	4: "bad user name or password",
	5: "not authorized",
}

func init() {
	registerRequestType(&RequestType{
		Name:         RequestTypeMQTT,
		Fields:       []string{"username", "password_file", "tls", "max_response_bytes", "allowed_targets", "denied_targets", "idle_timeout"},
		Config:       MQTTConfig{},
		KeyRules:     mqttKeyRules,
		Overrides:    []string{"timeout", PathParamPrefix},
		TargetFields: []string{"timeout", "username", "password_file", "tls"},
		Requester:    mqttRequester{},
		JSONAnswer:   true,
		Stage:        "mqtt",
		TargetHint: func(*model.Collector) (string, string) {
			return "Broker", "host, host:port, mqtt://host:1883 or mqtts://host:8883"
		},
		AnswerHint: "Paste the JSON document the exporter makes of the messages read, as the Response of a run shows it",
		Templates:  mqttTemplates,
		UnusedParams: func(c *model.Collector, _ RequestOverrides, unused string) error {
			return unusedParamsIn(c, unused, "its request.topics or label values")
		},
		SchemaRule: func() map[string]any {
			return map[string]any{"required": []any{"topics"}}
		},
		CheckTargetRequest: func(_ *model.Collector, t *model.StaticTarget) error { return checkNetTargetAccount(t) },
		CheckFiles:         accountFileWarnings,
		Follow:             mqttConns.follow,
		CloseConnections:   mqttConns.closeAll,
		Connections:        mqttConns.counts,
		Families:           []string{"http_exporter_mqtt_connections", "http_exporter_mqtt_connects_total"},
	})
	// A value written into a topic is one level of it: it adds no level
	// and no wildcard, so a probe reads no topic the collector did not
	// write.
	templateEscapers[mqttTopicKind] = func(where, name, value string) (string, error) {
		if value == "" || strings.ContainsFunc(value, func(r rune) bool { return r == '/' || r == '+' || r == '#' || r < ' ' || r == 0x7f }) {
			return "", fmt.Errorf("%s: the value of %s is empty or holds a /, a wildcard or a control character; a value is one level of a topic", where, name)
		}
		return value, nil
	}
}

// mqttRequester is the mqtt request type's Requester.
type mqttRequester struct{}

// Validate checks an mqtt collector's request and fills in its defaults.
func (mqttRequester) Validate(x *model.Collector) error {
	if err := checkNetAccount(x); err != nil {
		return err
	}
	r := &x.Request
	own := mqttRequest(x)
	if r.PasswordFile != "" && r.Username == "" {
		return fmt.Errorf("collector %q sets request.password_file without request.username; MQTT 3.1.1 sends a password only with a user name", x.Name)
	}
	if len(own.Topics) == 0 && !x.RequestNotMade {
		return fmt.Errorf("collector %q request.topics is required for request.type mqtt: the topic filters the scrape subscribes to", x.Name)
	}
	if len(own.Topics) > mqttMaxTopics {
		return fmt.Errorf("collector %q request.topics has %d filters, more than %d", x.Name, len(own.Topics), mqttMaxTopics)
	}
	seen := map[string]bool{}
	for _, field := range mqttTemplates(x, RequestOverrides{}) {
		if _, err := field.parse(); err != nil {
			return fmt.Errorf("collector %q %w", x.Name, err)
		}
		if err := checkMQTTFilter(field.text); err != nil {
			return fmt.Errorf("collector %q %s %q %w", x.Name, field.where, field.text, err)
		}
		if seen[field.text] {
			return fmt.Errorf("collector %q %s %q is written twice", x.Name, field.where, field.text)
		}
		seen[field.text] = true
	}
	if own.QoS != 0 && own.QoS != 1 {
		return fmt.Errorf("collector %q request.qos %d is not 0 or 1", x.Name, own.QoS)
	}
	switch {
	case own.Window < 0:
		return fmt.Errorf("collector %q request.window is negative", x.Name)
	case own.Window == 0:
		own.Window = model.Duration(mqttDefaultWindow)
	}
	switch {
	case own.MaxMessages < 0 || own.MaxMessages > mqttMaxMessages:
		return fmt.Errorf("collector %q request.max_messages %d is not 1 to %d", x.Name, own.MaxMessages, mqttMaxMessages)
	case own.MaxMessages == 0:
		own.MaxMessages = mqttDefaultMax
	}
	// MQTT counts the keep alive in whole seconds, 1 to 65535: a part of
	// a second is a second, and more than it can say is the most it can.
	switch alive := time.Duration(own.KeepAlive); {
	case alive < 0:
		return fmt.Errorf("collector %q request.keep_alive is negative", x.Name)
	case alive == 0:
		own.KeepAlive = model.Duration(mqttDefaultAlive)
	default:
		own.KeepAlive = model.Duration(min((alive + time.Second - 1).Truncate(time.Second), 65535*time.Second))
	}
	if strings.ContainsFunc(own.ClientID, func(c rune) bool { return c < ' ' || c == 0x7f }) || len(own.ClientID) > 256 || HasPathParams(own.ClientID) {
		return fmt.Errorf("collector %q request.client_id is not a client identifier: at most 256 bytes, with no control character and no placeholder", x.Name)
	}
	switch {
	case own.PersistentSession && own.ClientID == "":
		return fmt.Errorf("collector %q sets request.persistent_session without request.client_id; a broker keeps a session by its client identifier, so write one no other client uses", x.Name)
	case own.PersistentSession && own.KeepConnection:
		return fmt.Errorf("collector %q sets both request.persistent_session and request.keep_connection; the one has the broker keep what a scrape missed, the other the exporter, so choose one", x.Name)
	case r.IdleTimeout != nil && !own.KeepConnection:
		return fmt.Errorf("collector %q sets request.idle_timeout without request.keep_connection; a scrape's own connection is closed when the scrape ends", x.Name)
	case r.IdleTimeout != nil && *r.IdleTimeout < 0:
		return fmt.Errorf("collector %q request.idle_timeout is negative", x.Name)
	}
	r.Own = own
	return nil
}

// checkMQTTFilter holds a topic filter, its placeholders still in it, to
// MQTT's rules (3.1.1, 4.7): not empty, no NUL, # alone in the last level,
// + alone in its level.
func checkMQTTFilter(filter string) error {
	if filter == "" || len(filter) > 65535 || strings.ContainsRune(filter, 0) || !utf8.ValidString(filter) {
		return errors.New("is not a topic filter: not empty, valid UTF-8 with no NUL, at most 65535 bytes")
	}
	levels := strings.Split(filter, "/")
	for i, level := range levels {
		if strings.Contains(level, "#") && (level != "#" || i != len(levels)-1) {
			return errors.New("has a # that is not the whole last level, as sensors/# has it")
		}
		if strings.Contains(level, "+") && level != "+" {
			return errors.New("has a + that is not a whole level, as sensors/+/temperature has it")
		}
	}
	return nil
}

// mqttTemplates are the topic filters of an mqtt collector, which take
// placeholders.
func mqttTemplates(c *model.Collector, _ RequestOverrides) []templateField {
	var out []templateField
	for i, topic := range mqttRequest(c).Topics {
		out = append(out, templateField{fmt.Sprintf("request.topics[%d]", i), topic, mqttTopicKind})
	}
	return out
}

// mqttTarget reads a target, and whether its connection is TLS: mqtts://,
// or a target without a scheme of a collector with a tls block.
func mqttTarget(c *model.Collector, target string) (netAddress, bool, error) {
	a, err := parseNetTarget("mqtt", target, mqttPorts, false)
	if err != nil {
		return a, false, err
	}
	secure := a.scheme == "mqtts" || a.scheme == "" && c.Request.TLS != model.TLSConfig{}
	return a, secure, nil
}

// CheckTarget checks an mqtt collector's target.
func (mqttRequester) CheckTarget(c *model.Collector, target string, _ bool) error {
	_, _, err := mqttTarget(c, target)
	return err
}

func mqttURL(a netAddress, secure bool) string {
	if secure {
		return "mqtts://" + a.hostPort
	}
	return "mqtt://" + a.hostPort
}

// Label is the url label of an mqtt request: mqtt://host:port, or mqtts://.
func (mqttRequester) Label(target string, c *model.Collector, overrides RequestOverrides) (string, error) {
	a, secure, err := mqttTarget(withTargetAccount(c, overrides), target)
	if err != nil {
		return "", err
	}
	return mqttURL(a, secure), nil
}

// Method is the http_method label of an mqtt request.
func (mqttRequester) Method(*model.Collector, RequestOverrides) string { return "SUBSCRIBE" }

// Display renders an mqtt target for logs and labels.
func (mqttRequester) Display(target string) string {
	return netDisplay("mqtt", target, mqttPorts, false)
}

// mqttMessage is a message as the document has it.
type mqttMessage struct {
	Topic         string          `json:"topic"`
	Payload       *string         `json:"payload,omitempty"`
	PayloadBase64 string          `json:"payload_base64,omitempty"`
	JSON          json.RawMessage `json:"json,omitempty"`
	QoS           int             `json:"qos"`
	Retained      bool            `json:"retained"`
	AgeSeconds    float64         `json:"age_seconds"`
	// Count is how many messages of the topic came on a kept connection.
	Count uint64 `json:"count,omitempty"`

	at   time.Time
	size int64
}

// mqttConnection is what the document says of the connection the messages
// came on.
type mqttConnection struct {
	Kept           bool    `json:"kept"`
	New            bool    `json:"new"`
	SessionPresent bool    `json:"session_present"`
	AgeSeconds     float64 `json:"age_seconds"`
	Received       uint64  `json:"received"`
	Dropped        uint64  `json:"dropped"`
}

// mqttDocument is what a scrape hands the transforms.
type mqttDocument struct {
	Messages   []mqttMessage  `json:"messages"`
	Connection mqttConnection `json:"connection"`
}

// mqttLink is a connection to a broker after its CONNACK.
type mqttLink struct {
	conn *netConn
	// wmu orders the writes of a kept connection's reader and pinger.
	wmu sync.Mutex
	// most is the longest packet read: the response limit and a topic.
	most           int64
	sessionPresent bool
	connected      time.Time
}

// mqttString is text as MQTT writes it: its length in two bytes, then it.
func mqttString(out []byte, text string) []byte {
	out = binary.BigEndian.AppendUint16(out, uint16(len(text))) // #nosec G115 -- a topic, a client id and a credential are checked to fit
	return append(out, text...)
}

// mqttPacket is a control packet: its first byte, the length of the rest
// as MQTT writes one (seven bits a byte, the low ones first), and the rest.
func mqttPacket(first byte, rest []byte) []byte {
	out := []byte{first}
	n := len(rest)
	for {
		b := byte(n % 128)
		if n /= 128; n > 0 {
			b |= 0x80
		}
		out = append(out, b)
		if n == 0 {
			return append(out, rest...)
		}
	}
}

func (l *mqttLink) write(packet []byte) error {
	l.wmu.Lock()
	defer l.wmu.Unlock()
	_, err := l.conn.Write(packet)
	return err
}

// read reads one control packet.
func (l *mqttLink) read() (byte, []byte, error) {
	first, err := l.conn.r.ReadByte()
	if err != nil {
		return 0, nil, err
	}
	length, shift := int64(0), 0
	for {
		b, err := l.conn.r.ReadByte()
		if err != nil {
			return 0, nil, err
		}
		length |= int64(b&0x7f) << shift
		if b&0x80 == 0 {
			break
		}
		if shift += 7; shift > 21 {
			return 0, nil, errors.New("the broker sent a packet whose length is no MQTT length; is the target an MQTT broker?")
		}
	}
	if length > l.most {
		return 0, nil, model.MarkError(model.Errorf("the broker sent a packet of %d bytes, larger than the response limit; raise request.max_response_bytes or limits.max_response_bytes", model.Size(length)), model.ErrLimitExceeded)
	}
	rest := make([]byte, length)
	if _, err := io.ReadFull(l.conn.r, rest); err != nil {
		return 0, nil, err
	}
	return first, rest, nil
}

// message reads a PUBLISH, and acknowledges one of QoS 1.
func (l *mqttLink) message(first byte, rest []byte) (mqttMessage, error) {
	var m mqttMessage
	qos := int(first >> 1 & 3)
	if len(rest) < 2 || len(rest) < 2+int(binary.BigEndian.Uint16(rest)) || qos > 1 {
		return m, errors.New("the broker sent a PUBLISH that is none")
	}
	n := int(binary.BigEndian.Uint16(rest))
	topic, payload := string(rest[2:2+n]), rest[2+n:]
	if qos == 1 {
		if len(payload) < 2 {
			return m, errors.New("the broker sent a PUBLISH of QoS 1 without its packet identifier")
		}
		if err := l.write(mqttPacket(mqttPuback<<4, payload[:2])); err != nil {
			return m, err
		}
		payload = payload[2:]
	}
	m = mqttMessage{Topic: strings.ToValidUTF8(topic, "�"), QoS: qos, Retained: first&1 == 1, at: time.Now()}
	if utf8.Valid(payload) {
		text := string(payload)
		m.Payload = &text
		if len(payload) > 0 && json.Valid(payload) {
			m.JSON = json.RawMessage(payload)
		}
	} else {
		m.PayloadBase64 = base64.StdEncoding.EncodeToString(payload)
	}
	// What the message takes of the response limit is what it takes of
	// the document: its payload twice where it is JSON, and a third more
	// where it is no text, with room for its age and its count.
	if encoded, err := json.Marshal(m); err == nil {
		m.size = int64(len(encoded)) + 32
	}
	return m, nil
}

// mqttSettings is what a connection is made with.
type mqttSettings struct {
	collector    *model.Collector
	own          MQTTConfig
	address      netAddress
	secure       bool
	label        string
	topics       []string
	clientID     string
	cleanSession bool
}

// mqttOpen connects to the broker, logs in and subscribes, handing every
// message that comes before the subscription is acknowledged to got.
func mqttOpen(ctx context.Context, s mqttSettings, got func(mqttMessage)) (*mqttLink, error) {
	c := s.collector
	password, err := netPassword(c)
	if err != nil {
		return nil, err
	}
	cfg, err := netTLSConfig(c, s.secure)
	if err != nil {
		return nil, err
	}
	ctx, err = netGuard(ctx, c, "mqtt", s.address.hostPort)
	if err != nil {
		return nil, err
	}
	traceRequest(ctx, "CONNECT", s.label, nil, "", false)
	fail := func(err error) (*mqttLink, error) {
		traceFailedOutcome(ctx, "error: "+err.Error())
		return nil, err
	}
	conn, err := netDial(ctx, s.address, cfg)
	if err != nil {
		return fail(err)
	}
	link := &mqttLink{conn: conn, most: responseLimit(c) + 65535 + mqttPacketOverhead}
	// MQTT writes the length of a text in two bytes.
	for _, text := range append([]string{s.clientID, c.Request.Username, password}, s.topics...) {
		if len(text) > 65535 {
			conn.close()
			return fail(errors.New("a topic, the client identifier, the user name or the password is longer than 65535 bytes, the most MQTT can send"))
		}
	}
	// CONNECT: the protocol's name and level, the flags, the keep alive,
	// then the client identifier, the user name and the password.
	flags := byte(0)
	if s.cleanSession {
		flags |= 0x02
	}
	body := mqttString(nil, "MQTT")
	payload := mqttString(nil, s.clientID)
	if c.Request.Username != "" {
		flags |= 0x80
		payload = mqttString(payload, c.Request.Username)
		if password != "" {
			flags |= 0x40
			payload = mqttString(payload, password)
		}
	}
	body = append(body, 4, flags)
	body = binary.BigEndian.AppendUint16(body, uint16(time.Duration(s.own.KeepAlive)/time.Second)) // #nosec G115 -- Validate holds it to 1..65535 seconds
	closed := func(err error, what string) (*mqttLink, error) {
		conn.close()
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			err = errors.New("the broker closed the connection")
		}
		return fail(netFailure(ctx, fmt.Errorf("%s of %s: %w", what, s.label, err)))
	}
	if err := link.write(mqttPacket(mqttConnect<<4, append(body, payload...))); err != nil {
		return closed(err, "CONNECT")
	}
	// What is no broker answers with something else, of a length that
	// would be waited for.
	if begun, err := conn.r.Peek(1); err == nil && begun[0] != mqttConnack<<4 {
		return closed(errors.New("the answer is no CONNACK; is the target an MQTT broker?"), "CONNECT")
	}
	first, rest, err := link.read()
	if err != nil {
		return closed(err, "CONNECT")
	}
	if first>>4 != mqttConnack || len(rest) != 2 {
		return closed(errors.New("the answer is no CONNACK; is the target an MQTT broker?"), "CONNECT")
	}
	if rest[1] != 0 {
		if why, known := mqttRefusals[rest[1]]; known {
			return closed(fmt.Errorf("the broker refused the connection: %s", why), "CONNECT")
		}
		return closed(model.Errorf("the broker refused the connection: return code %s, which MQTT 3.1.1 does not define", model.Bare(strconv.Itoa(int(rest[1])))), "CONNECT")
	}
	link.sessionPresent, link.connected = rest[0]&1 == 1, time.Now()
	traceOutcome(ctx, "connected")
	// A session the broker kept has its subscriptions, and subscribing
	// again would have it send every retained message again.
	if link.sessionPresent && !s.cleanSession {
		return link, nil
	}
	traceRequest(ctx, "SUBSCRIBE", s.label, http.Header{"Mqtt-Topics": s.topics}, "", false)
	subscribe := []byte{0, 1}
	for _, topic := range s.topics {
		subscribe = append(mqttString(subscribe, topic), byte(s.own.QoS)) // #nosec G115 -- 0 or 1
	}
	if err := link.write(mqttPacket(mqttSubscribe<<4|2, subscribe)); err != nil {
		return closed(err, "SUBSCRIBE")
	}
	for {
		first, rest, err := link.read()
		if err != nil {
			return closed(err, "SUBSCRIBE")
		}
		switch first >> 4 {
		case mqttPublish:
			m, err := link.message(first, rest)
			if err != nil {
				return closed(err, "SUBSCRIBE")
			}
			got(m)
		case mqttSuback:
			if len(rest) != 2+len(s.topics) {
				return closed(errors.New("the SUBACK does not answer every topic"), "SUBSCRIBE")
			}
			for i, code := range rest[2:] {
				if code == 0x80 {
					return closed(fmt.Errorf("the broker refused the subscription to %q; the account may not read it", netShown(s.topics[i])), "SUBSCRIBE")
				}
			}
			traceOutcome(ctx, "subscribed")
			return link, nil
		}
	}
}

// mqttEnded says whether a kept connection's done is closed; one never
// made has none, and counts as ended.
func mqttEnded(done chan struct{}) bool {
	select {
	case <-done:
		return true
	default:
		return done == nil
	}
}

// mqttClientID is the collector's client identifier, or one of this
// connection's own: 20 characters, within the 23 every broker takes.
func mqttClientID(own MQTTConfig) string {
	if own.ClientID != "" {
		return own.ClientID
	}
	random := make([]byte, 8)
	_, _ = rand.Read(random)
	return "pue-" + hex.EncodeToString(random)
}

// Fetch performs one scrape of an mqtt collector.
func (mqttRequester) Fetch(ctx context.Context, target string, c *model.Collector, overrides RequestOverrides, _ http.Header) (*HTTPResponse, error) {
	start := time.Now()
	c = withTargetAccount(c, overrides)
	own := mqttRequest(c)
	address, secure, err := mqttTarget(c, target)
	if err != nil {
		return nil, err
	}
	s := mqttSettings{collector: c, own: own, address: address, secure: secure, label: mqttURL(address, secure), cleanSession: !own.PersistentSession}
	for _, field := range mqttTemplates(c, overrides) {
		topic, err := field.render(overrides.Params)
		if err != nil {
			return nil, err
		}
		s.topics = append(s.topics, topic)
	}
	ctx, cancel := netBudget(ctx, overrides, mqttTimeout)
	defer cancel()
	window := time.Duration(own.Window)
	var doc mqttDocument
	var state *tlsStateHolder
	if own.KeepConnection {
		doc, state, err = mqttConns.read(ctx, s, window)
	} else {
		doc, state, err = mqttReadOnce(ctx, s, window)
	}
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > responseLimit(c) {
		return nil, model.MarkError(model.Errorf("the document of %d messages is %d bytes, larger than the response limit; lower request.max_messages, or raise request.max_response_bytes or limits.max_response_bytes", len(doc.Messages), model.Size(int64(len(body)))), model.ErrLimitExceeded)
	}
	traceFacts(ctx, intFact("mqtt.messages", len(doc.Messages)))
	resp := &HTTPResponse{StatusCode: http.StatusOK, NoStatus: true, Headers: http.Header{"Content-Type": {"application/json"}}, Body: body, Target: target, Collector: c.Name, Duration: time.Since(start)}
	if state != nil && state.connection != nil {
		resp.Trip = &HTTPTrip{Total: resp.Duration, TLSConnection: state.connection}
	}
	return resp, nil
}

// mqttWindow is how long a scrape reads from now, the connection made: the
// collector's window, ended before the scrape's deadline does, with time
// left to answer.
func mqttWindow(ctx context.Context, window time.Duration) time.Duration {
	if deadline, ok := ctx.Deadline(); ok {
		window = min(window, time.Until(deadline)*9/10)
	}
	return window
}

// tlsStateHolder is the TLS connection a scrape's messages came over.
type tlsStateHolder struct{ connection *TLSConnection }

// mqttReadOnce is a scrape on a connection of its own: it reads until
// window has passed or max_messages have come, and disconnects.
func mqttReadOnce(ctx context.Context, s mqttSettings, window time.Duration) (mqttDocument, *tlsStateHolder, error) {
	doc := mqttDocument{Messages: []mqttMessage{}}
	s.clientID = mqttClientID(s.own)
	limit, size := responseLimit(s.collector), int64(0)
	var tooLarge error
	add := func(m mqttMessage) {
		if size += m.size; size > limit && tooLarge == nil {
			tooLarge = model.MarkError(model.Errorf("the messages read are larger than the response limit; lower request.max_messages or request.window, or raise request.max_response_bytes or limits.max_response_bytes"), model.ErrLimitExceeded)
		}
		if len(doc.Messages) < s.own.MaxMessages {
			doc.Messages = append(doc.Messages, m)
		}
	}
	link, err := mqttOpen(ctx, s, add)
	if err != nil {
		return doc, nil, err
	}
	defer link.conn.close()
	end := time.Now().Add(mqttWindow(ctx, window))
	_ = link.conn.SetReadDeadline(end)
	// A window longer than the keep alive is pinged through, or the broker
	// would give the connection up in the middle of it.
	if alive := time.Duration(s.own.KeepAlive); time.Until(end) > alive/2 {
		pings := time.NewTicker(alive / 2)
		stop := make(chan struct{})
		defer func() { pings.Stop(); close(stop) }()
		go func() {
			defer func() {
				if recovered := recover(); recovered != nil {
					_ = recoveredPanic(recovered)
				}
			}()
			for {
				select {
				case <-stop:
					return
				case <-pings.C:
					_ = link.write(mqttPacket(mqttPingreq<<4, nil))
				}
			}
		}()
	}
	traceRequest(ctx, "READ", s.label, nil, "", false)
	for len(doc.Messages) < s.own.MaxMessages && tooLarge == nil {
		first, rest, err := link.read()
		if err != nil {
			// The window's end is how a read ends; anything else, the
			// scrape's own deadline included, is a failure.
			if errors.Is(err, os.ErrDeadlineExceeded) && ctx.Err() == nil && !time.Now().Before(end) {
				break
			}
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				err = errors.New("the broker closed the connection; another client with the same client_id takes a connection over")
			}
			err = netFailure(ctx, fmt.Errorf("reading from %s: %w", s.label, err))
			traceFailedOutcome(ctx, "error: "+err.Error())
			return doc, nil, err
		}
		if first>>4 == mqttPublish {
			m, err := link.message(first, rest)
			if err != nil {
				traceFailedOutcome(ctx, "error: "+err.Error())
				return doc, nil, err
			}
			add(m)
		}
	}
	if tooLarge != nil {
		traceFailedOutcome(ctx, "error: "+tooLarge.Error())
		return doc, nil, tooLarge
	}
	traceOutcome(ctx, fmt.Sprintf("%d messages", len(doc.Messages)))
	_ = link.conn.SetWriteDeadline(time.Now().Add(time.Second))
	_ = link.write(mqttPacket(mqttDisconnect<<4, nil))
	now := time.Now()
	for i := range doc.Messages {
		doc.Messages[i].AgeSeconds = now.Sub(doc.Messages[i].at).Seconds()
	}
	doc.Connection = mqttConnection{New: true, SessionPresent: link.sessionPresent, AgeSeconds: now.Sub(link.connected).Seconds(), Received: uint64(len(doc.Messages))}
	return doc, &tlsStateHolder{tlsConnectionOf(link.conn.tlsState())}, nil
}

// mqttKeptKey is what tells one kept connection from another: the
// collector, the broker, the account and what it subscribes to.
type mqttKeptKey struct {
	collector, label, username, passwordFile, clientID, topics string
	tls                                                        model.TLSConfig
	qos                                                        int
}

// mqttKept is a connection kept between scrapes and what came on it.
type mqttKept struct {
	// making orders the scrapes that find the connection gone: one makes
	// it again, the others use what it made.
	// A channel, so that a scrape waiting for it gives up at its deadline.
	making chan struct{}

	mu       sync.Mutex
	link     *mqttLink
	done     chan struct{}
	failure  error
	latest   map[string]*mqttMessage
	bytes    int64
	received uint64
	dropped  uint64
	lastUsed time.Time
	tls      *TLSConnection
}

// mqttConnCache is the connections the type keeps (keep_connection).
type mqttConnCache struct {
	mu   sync.Mutex
	kept map[mqttKeptKey]*mqttKept
	made map[string]uint64
}

var mqttConns = &mqttConnCache{kept: map[mqttKeptKey]*mqttKept{}, made: map[string]uint64{}}

// read answers a scrape of a collector that keeps its connection: the
// last message of each topic that came on it, after making it, and
// reading for window, when it is not there.
func (cache *mqttConnCache) read(ctx context.Context, s mqttSettings, window time.Duration) (mqttDocument, *tlsStateHolder, error) {
	c := s.collector
	key := mqttKeptKey{collector: c.Name, label: s.label, username: c.Request.Username, passwordFile: c.Request.PasswordFile, clientID: s.own.ClientID, topics: strings.Join(s.topics, "\x00"), tls: c.Request.TLS, qos: s.own.QoS}
	cache.mu.Lock()
	entry := cache.kept[key]
	if entry == nil {
		// A connection that ended and was not made again makes room.
		if len(cache.kept) >= mqttMaxKept {
			for other, kept := range cache.kept {
				if kept.ended() {
					delete(cache.kept, other)
				}
			}
		}
		if len(cache.kept) >= mqttMaxKept {
			cache.mu.Unlock()
			return mqttDocument{}, nil, fmt.Errorf("the exporter keeps %d MQTT connections already, the most it does; a collector whose probes name many brokers or topics reads them without request.keep_connection", mqttMaxKept)
		}
		entry = &mqttKept{making: make(chan struct{}, 1)}
		cache.kept[key] = entry
	}
	cache.mu.Unlock()

	select {
	case entry.making <- struct{}{}:
	case <-ctx.Done():
		return mqttDocument{}, nil, netFailure(ctx, fmt.Errorf("waiting for the connection to %s another scrape is making: %w", s.label, context.Cause(ctx)))
	}
	entry.mu.Lock()
	alive := entry.link != nil && !mqttEnded(entry.done)
	entry.lastUsed = time.Now()
	entry.mu.Unlock()
	fresh := false
	if !alive {
		fresh = true
		s.clientID = mqttClientID(s.own)
		var early []mqttMessage
		link, err := mqttOpen(ctx, s, func(m mqttMessage) { early = append(early, m) })
		if err != nil {
			// A connection never made keeps no place among those kept.
			cache.mu.Lock()
			if cache.kept[key] == entry && entry.ended() {
				delete(cache.kept, key)
			}
			cache.mu.Unlock()
			<-entry.making
			return mqttDocument{}, nil, err
		}
		// The connection outlives the scrape that made it: its deadline
		// and its cancellation are the scrape's no longer.
		if !link.conn.stop() {
			link.conn.close()
			<-entry.making
			return mqttDocument{}, nil, netFailure(ctx, context.Cause(ctx))
		}
		link.conn.stop = func() bool { return true }
		_ = link.conn.SetDeadline(time.Time{})
		entry.mu.Lock()
		entry.link, entry.done, entry.failure = link, make(chan struct{}), nil
		entry.latest, entry.bytes, entry.received, entry.dropped = map[string]*mqttMessage{}, 0, 0, 0
		entry.tls = tlsConnectionOf(link.conn.tlsState())
		limit := responseLimit(c)
		for _, m := range early {
			entry.keepLocked(m, s.own.MaxMessages, limit)
		}
		done := entry.done
		entry.mu.Unlock()
		cache.mu.Lock()
		cache.made[c.Name]++
		// Its place may have been given up while it was made.
		if cache.kept[key] == nil {
			cache.kept[key] = entry
		}
		cache.mu.Unlock()
		idle := mqttDefaultIdle
		if c.Request.IdleTimeout != nil {
			idle = time.Duration(*c.Request.IdleTimeout)
		}
		go entry.receive(link, done, time.Duration(s.own.KeepAlive), s.own.MaxMessages, limit, s.label)
		go entry.ping(link, done, time.Duration(s.own.KeepAlive), idle)
		// The retained messages come right after the subscription.
		traceRequest(ctx, "READ", s.label, nil, "", false)
		select {
		case <-time.After(mqttWindow(ctx, window)):
		case <-done:
		case <-ctx.Done():
		}
	} else {
		traceRequest(ctx, "READ", s.label, nil, "", false)
	}
	<-entry.making

	entry.mu.Lock()
	defer entry.mu.Unlock()
	if mqttEnded(entry.done) && fresh {
		err := fmt.Errorf("the connection to %s ended as soon as it was made: %w", s.label, entry.failure)
		traceFailedOutcome(ctx, "error: "+err.Error())
		return mqttDocument{}, nil, err
	}
	now := time.Now()
	doc := mqttDocument{Messages: make([]mqttMessage, 0, len(entry.latest))}
	for _, m := range entry.latest {
		out := *m
		out.AgeSeconds = now.Sub(m.at).Seconds()
		doc.Messages = append(doc.Messages, out)
	}
	sort.Slice(doc.Messages, func(i, j int) bool { return doc.Messages[i].Topic < doc.Messages[j].Topic })
	doc.Connection = mqttConnection{Kept: true, New: fresh, SessionPresent: entry.link.sessionPresent, AgeSeconds: now.Sub(entry.link.connected).Seconds(), Received: entry.received, Dropped: entry.dropped}
	traceOutcome(ctx, fmt.Sprintf("%d topics kept", len(doc.Messages)))
	return doc, &tlsStateHolder{entry.tls}, nil
}

// ended says whether the entry has no connection now.
func (e *mqttKept) ended() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.link == nil || mqttEnded(e.done)
}

// keepLocked remembers m as the last message of its topic, unless the
// topics kept are max already or their bytes would pass limit.
func (e *mqttKept) keepLocked(m mqttMessage, most int, limit int64) {
	e.received++
	old := e.latest[m.Topic]
	grown := e.bytes + m.size
	if old != nil {
		grown -= old.size
		m.Count = old.Count
	}
	if old == nil && len(e.latest) >= most || grown > limit {
		e.dropped++
		return
	}
	m.Count++
	e.latest[m.Topic], e.bytes = &m, grown
}

// end closes the connection and says why it ended.
func (e *mqttKept) end(link *mqttLink, done chan struct{}, why error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if mqttEnded(done) {
		return
	}
	if e.done == done {
		e.failure = why
	}
	close(done)
	_ = link.conn.Close()
}

// receive reads what the broker sends on a kept connection until it ends:
// a broker that sent nothing for a keep alive and a half, a ping's answer
// included, is gone.
func (e *mqttKept) receive(link *mqttLink, done chan struct{}, alive time.Duration, most int, limit int64, label string) {
	var failure error
	defer func() {
		if recovered := recover(); recovered != nil {
			failure = recoveredPanic(recovered)
		}
		e.end(link, done, failure)
	}()
	for {
		_ = link.conn.SetReadDeadline(time.Now().Add(alive * 3 / 2))
		first, rest, err := link.read()
		if err != nil {
			if !mqttEnded(done) {
				slog.Default().Debug("a kept MQTT connection ended; the next scrape makes it again", "broker", label, "error", err.Error())
			}
			failure = err
			return
		}
		if first>>4 != mqttPublish {
			continue
		}
		m, err := link.message(first, rest)
		if err != nil {
			failure = err
			return
		}
		e.mu.Lock()
		e.keepLocked(m, most, limit)
		e.mu.Unlock()
	}
}

// ping keeps a kept connection alive, a PINGREQ every half keep alive, and
// closes it once no scrape has used it for idle.
func (e *mqttKept) ping(link *mqttLink, done chan struct{}, alive, idle time.Duration) {
	defer func() {
		if recovered := recover(); recovered != nil {
			e.end(link, done, recoveredPanic(recovered))
		}
	}()
	// idle 0 keeps the connection as long as the collector is configured.
	every := alive / 2
	if idle > 0 {
		every = min(every, max(idle/2, 100*time.Millisecond))
	}
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	sent := time.Now()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
		}
		e.mu.Lock()
		unused := time.Since(e.lastUsed)
		e.mu.Unlock()
		if idle > 0 && unused >= idle {
			_ = link.conn.SetWriteDeadline(time.Now().Add(time.Second))
			_ = link.write(mqttPacket(mqttDisconnect<<4, nil))
			e.end(link, done, errors.New("no scrape used it for request.idle_timeout"))
			return
		}
		if time.Since(sent) < alive/2 {
			continue
		}
		sent = time.Now()
		_ = link.conn.SetWriteDeadline(sent.Add(alive))
		if err := link.write(mqttPacket(mqttPingreq<<4, nil)); err != nil {
			e.end(link, done, err)
			return
		}
	}
}

// closeWhere closes the kept connections gone says are no longer wanted.
func (cache *mqttConnCache) closeWhere(gone func(mqttKeptKey) bool) {
	cache.mu.Lock()
	var entries []*mqttKept
	for key, entry := range cache.kept {
		if gone(key) {
			delete(cache.kept, key)
			entries = append(entries, entry)
		}
	}
	cache.mu.Unlock()
	for _, entry := range entries {
		entry.mu.Lock()
		link, done := entry.link, entry.done
		entry.mu.Unlock()
		if link != nil {
			_ = link.conn.SetWriteDeadline(time.Now().Add(time.Second))
			_ = link.write(mqttPacket(mqttDisconnect<<4, nil))
			entry.end(link, done, errors.New("the configuration no longer has its collector"))
		}
	}
}

// follow closes the connections of collectors a configuration no longer
// has, or has without keep_connection; one whose broker, account or
// topics changed is used no more, and closes when idle_timeout has passed.
func (cache *mqttConnCache) follow(collectors []model.Collector, _ []model.StaticTarget) {
	keeps := map[string]bool{}
	for i := range collectors {
		if c := &collectors[i]; c.Request.Type == RequestTypeMQTT && mqttRequest(c).KeepConnection {
			keeps[c.Name] = true
		}
	}
	cache.closeWhere(func(key mqttKeptKey) bool { return !keeps[key.collector] })
	cache.mu.Lock()
	for name := range cache.made {
		if !keeps[name] {
			delete(cache.made, name)
		}
	}
	cache.mu.Unlock()
}

// closeAll closes every kept connection, at shutdown.
func (cache *mqttConnCache) closeAll() {
	cache.closeWhere(func(mqttKeptKey) bool { return true })
	cache.mu.Lock()
	clear(cache.made)
	cache.mu.Unlock()
}

// counts reports the kept connections a collector has open now and how
// many it has made.
func (cache *mqttConnCache) counts(collector string) (int, uint64) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	open := 0
	for key, entry := range cache.kept {
		if key.collector != collector {
			continue
		}
		entry.mu.Lock()
		if entry.link != nil && !mqttEnded(entry.done) {
			open++
		}
		entry.mu.Unlock()
	}
	return open, cache.made[collector]
}

// mqttKeyRules is what the configuration schema says of an mqtt
// collector's own keys.
func mqttKeyRules() map[string]map[string]any {
	return map[string]map[string]any{
		"topics":             {"minItems": 1, "maxItems": mqttMaxTopics, "description": "mqtt, required: the topic filters subscribed to, such as sensors/+/temperature or plant/#. Each may contain {{param_name}} placeholders, a value being one level of a topic: no /, + or #. See docs/MQTT.md."},
		"qos":                {"minimum": 0, "maximum": 1, "description": "mqtt: the QoS the topics are subscribed at, 0, the default, or 1, which a persistent_session needs for the broker to queue what a scrape missed."},
		"window":             {"description": "mqtt: how long a scrape reads after subscribing: the retained messages come at once, and whatever is published within the window after them. Defaults to 1s, and ends within the scrape's deadline. With keep_connection, how long the scrape that makes the connection waits for the first messages."},
		"max_messages":       {"minimum": 0, "maximum": mqttMaxMessages, "description": "mqtt: reading ends once this many messages have come, before the window has passed. With keep_connection, the most topics whose last message is kept. Defaults to 1000."},
		"client_id":          {"description": "mqtt: the client identifier, at most 256 bytes. Left out, one made for each connection. A broker takes one connection of an identifier at a time, so two exporters with the same one cut each other off."},
		"persistent_session": {"description": "mqtt: true asks the broker to keep the session of client_id between scrapes (clean session 0): what is published at QoS 1 while no scrape is connected is queued by the broker and delivered to the next. Needs client_id and qos: 1; not with keep_connection. See docs/MQTT.md#between-scrapes."},
		"keep_connection":    {"description": "mqtt: true keeps the connection between scrapes and remembers the last message of each topic that came on it, which a scrape answers with at once; closed after request.idle_timeout without a scrape, 5m by default, and with 0 kept as long as the collector is configured. See docs/MQTT.md#between-scrapes."},
		"keep_alive":         {"description": "mqtt: the keep alive told to the broker, 30s by default, in whole seconds as MQTT counts it: a part of a second is a second, and the most is 65535s. A kept connection is pinged twice in it, and is given up when the broker sent nothing for one and a half."},
	}
}
