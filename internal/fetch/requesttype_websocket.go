//go:build request_type_websocket

package fetch

import (
	"context"
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // RFC 6455's handshake is SHA-1 of the key; it guards no secret.
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The websocket request type reads messages from a WebSocket endpoint: it
// makes the opening handshake, sends one text message when the collector
// has one, reads the messages the collector asks for, and closes.
//
//	request:
//	  type: websocket
//	  send_text: '{"op":"subscribe","channel":{{param_channel|json}}}'   # optional
//	  read_messages: 1             # how many messages are read; 1 when left out
//	  read_window: 5s              # how long they are waited for; 5s when left out
//	  subprotocols: [graphql-transport-ws]
//	  headers: {X-Tenant: acme}
//	  bearer_token_file: /etc/exporter/token
//
// With read_messages: 1 the answer is the message, as the server sent it,
// for whatever decoder reads it; none within read_window fails the scrape.
// With more, the answer is a JSON list of the messages that came before
// the window passed or the server closed, each with its payload as text,
// json when the text is JSON, or payload_base64 when it is binary.
//
// It stays a pull: a scrape connects, reads and closes; no connection
// outlives it. RFC 6455, by hand: no extension is offered, so no message is
// compressed.
//
// The target is the endpoint's URL, ws://host:port/path?query or wss://
// (ports 80 and 443); a target without a scheme is ws://, or wss:// when
// the collector has a tls block.
//
// The type imports nothing outside the standard library.

// WebSocketConfig is a websocket collector's own request keys.
type WebSocketConfig struct {
	SendText     string         `yaml:"send_text,omitempty"`
	ReadMessages int            `yaml:"read_messages,omitempty"`
	ReadWindow   model.Duration `yaml:"read_window,omitempty"`
	Subprotocols []string       `yaml:"subprotocols,omitempty"`
}

func webSocketRequest(c *model.Collector) WebSocketConfig {
	own, _ := c.Request.Own.(WebSocketConfig)
	return own
}

const (
	// webSocketTimeout bounds a scrape with no deadline of its own.
	webSocketTimeout       = 30 * time.Second
	webSocketDefaultWindow = 5 * time.Second
	webSocketMaxMessages   = 10000
	webSocketSendKind      = "websocket_send"
	// webSocketGUID is what the handshake appends to the key (RFC 6455, 1.3).
	webSocketGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
)

// Frame opcodes (RFC 6455, 5.2).
const (
	wsContinuation = 0
	wsText         = 1
	wsBinary       = 2
	wsClose        = 8
	wsPing         = 9
	wsPong         = 10
)

// webSocketPorts are the schemes of a websocket target and their ports.
var webSocketPorts = map[string]int{"": 80, "ws": 80, "wss": 443}

// webSocketToken is a subprotocol's name, an HTTP token.
var webSocketToken = regexp.MustCompile("^[A-Za-z0-9!#$%&'*+.^_`|~-]+$")

func init() {
	registerRequestType(&RequestType{
		Name:         RequestTypeWebSocket,
		Fields:       []string{"headers", "bearer_token_file", "tls", "max_response_bytes", "allowed_targets", "denied_targets"},
		Config:       WebSocketConfig{},
		KeyRules:     webSocketKeyRules,
		Overrides:    []string{"timeout", PathParamPrefix},
		TargetFields: []string{"timeout", "tls"},
		Requester:    webSocketRequester{},
		Stage:        "websocket",
		TargetHint: func(*model.Collector) (string, string) {
			return "Endpoint", "ws://host:port/path or wss://host/path"
		},
		AnswerHint: "Paste the message the server sends, or, of a collector that reads several, the JSON list the exporter makes of them, as the Response of a run shows it",
		Templates:  webSocketTemplates,
		UnusedParams: func(c *model.Collector, _ RequestOverrides, unused string) error {
			return unusedParamsIn(c, unused, "its request.send_text, header values or label values")
		},
		CheckTargetRequest: func(_ *model.Collector, t *model.StaticTarget) error { return checkNetTargetAccount(t) },
		CheckFiles:         checkWebSocketFiles,
		JSONDocument: func(c *model.Collector) string {
			if webSocketRequest(c).ReadMessages > 1 {
				return "the JSON list request.read_messages makes of the messages a websocket server sends"
			}
			return ""
		},
	})
	// send_text is written as an http body is, with its filters.
	templateFilterRules[webSocketSendKind] = func(string, pathPlaceholder) error { return nil }
}

// checkWebSocketFiles reads, for --dry-run, the files a scrape reads: the
// tls block's and the bearer token's.
func checkWebSocketFiles(c *model.Collector, t *model.StaticTarget) []string {
	warnings := accountFileWarnings(c, t)
	if t != nil || c.Request.BearerTokenFile == "" {
		return warnings
	}
	if _, err := requestAuthorization(c); err != nil {
		warnings = append(warnings, fmt.Sprintf("collector %q: request.bearer_token_file: %v; a scrape would fail on it now (the file is read at each scrape, so one put in place later is used then)", c.Name, err))
	}
	return warnings
}

// webSocketRequester is the websocket request type's Requester.
type webSocketRequester struct{}

// Validate checks a websocket collector's request and fills in its
// defaults.
func (webSocketRequester) Validate(x *model.Collector) error {
	if err := checkNetAccount(x); err != nil {
		return err
	}
	r := &x.Request
	own := webSocketRequest(x)
	if err := checkHeaderNames(r.Headers); err != nil {
		return fmt.Errorf("collector %q request.headers: %w", x.Name, err)
	}
	for _, name := range model.SortedKeys(r.Headers) {
		canonical := http.CanonicalHeaderKey(name)
		switch {
		case canonical == "Host" || canonical == "Upgrade" || canonical == "Connection" || strings.HasPrefix(canonical, "Sec-Websocket-"):
			return fmt.Errorf("collector %q request.headers sets %s, which the handshake writes itself", x.Name, name)
		case canonical == "Authorization" && r.BearerTokenFile != "":
			return fmt.Errorf("collector %q sets both request.bearer_token_file and the Authorization header", x.Name)
		}
		if !HasPathParams(r.Headers[name]) {
			if err := checkHeaderValue(r.Headers[name]); err != nil {
				return fmt.Errorf("collector %q request.headers.%s %w", x.Name, name, err)
			}
		}
	}
	for _, field := range requestTemplates(x, RequestOverrides{}) {
		if _, err := field.parse(); err != nil {
			return fmt.Errorf("collector %q %w", x.Name, err)
		}
	}
	if !utf8.ValidString(own.SendText) {
		return fmt.Errorf("collector %q request.send_text is not UTF-8, which a text message is", x.Name)
	}
	switch {
	case own.ReadMessages < 0 || own.ReadMessages > webSocketMaxMessages:
		return fmt.Errorf("collector %q request.read_messages %d is not 1 to %d", x.Name, own.ReadMessages, webSocketMaxMessages)
	case own.ReadMessages == 0:
		own.ReadMessages = 1
	}
	switch {
	case own.ReadWindow < 0:
		return fmt.Errorf("collector %q request.read_window is negative", x.Name)
	case own.ReadWindow == 0:
		own.ReadWindow = model.Duration(webSocketDefaultWindow)
	}
	for i, name := range own.Subprotocols {
		if !webSocketToken.MatchString(name) || slices.Contains(own.Subprotocols[:i], name) {
			return fmt.Errorf("collector %q request.subprotocols[%d] %q is not a subprotocol's name, written once: letters, digits and !#$%%&'*+-.^_`|~", x.Name, i, name)
		}
	}
	r.Own = own
	return nil
}

// webSocketTemplates is the message a websocket collector sends, which
// takes placeholders.
func webSocketTemplates(c *model.Collector, _ RequestOverrides) []templateField {
	if text := webSocketRequest(c).SendText; HasPathParams(text) {
		return []templateField{{"request.send_text", text, webSocketSendKind}}
	}
	return nil
}

// webSocketTarget reads a target: where it is, whether over TLS, and the
// request target of its handshake, a path and a query.
func webSocketTarget(c *model.Collector, target string) (netAddress, bool, *url.URL, error) {
	a, err := parseNetTarget("websocket", target, webSocketPorts, true)
	if err != nil {
		return a, false, nil, err
	}
	secure := a.scheme == "wss" || a.scheme == "" && c.Request.TLS != model.TLSConfig{}
	rest := a.rest
	if !strings.HasPrefix(rest, "/") {
		rest = "/" + rest
	}
	u, err := url.ParseRequestURI(rest)
	if err != nil || strings.ContainsFunc(rest, func(r rune) bool { return r <= ' ' || r == 0x7f }) {
		return a, false, nil, fmt.Errorf("target %q has a path that is none: write it as a URL has it, with %%20 for a blank", target)
	}
	return a, secure, u, nil
}

// CheckTarget checks a websocket collector's target.
func (webSocketRequester) CheckTarget(c *model.Collector, target string, _ bool) error {
	_, _, _, err := webSocketTarget(c, target)
	return err
}

// webSocketURL is a target as a URL, without its query, which may hold a
// credential.
func webSocketURL(a netAddress, secure bool, u *url.URL) string {
	scheme := "ws://"
	if secure {
		scheme = "wss://"
	}
	return scheme + a.hostPort + u.EscapedPath()
}

// Label is the url label of a websocket request.
func (webSocketRequester) Label(target string, c *model.Collector, overrides RequestOverrides) (string, error) {
	a, secure, u, err := webSocketTarget(withTargetAccount(c, overrides), target)
	if err != nil {
		return "", err
	}
	return webSocketURL(a, secure, u), nil
}

// Method is the http_method label of a websocket request.
func (webSocketRequester) Method(c *model.Collector, _ RequestOverrides) string {
	if webSocketRequest(c).SendText != "" {
		return "SEND"
	}
	return "READ"
}

// Display renders a websocket target for logs and labels, without its
// query.
func (webSocketRequester) Display(target string) string {
	a, err := parseNetTarget("websocket", target, webSocketPorts, true)
	if err != nil {
		return "<invalid target>"
	}
	shown, _, _ := strings.Cut(strings.TrimSpace(target), "?")
	if a.rest != "" && strings.Contains(a.rest, "?") {
		shown += "?***"
	}
	return shown
}

// webSocketMessage is a message as a list of several has it.
type webSocketMessage struct {
	Payload       *string         `json:"payload,omitempty"`
	PayloadBase64 string          `json:"payload_base64,omitempty"`
	JSON          json.RawMessage `json:"json,omitempty"`
	Binary        bool            `json:"binary"`
}

// Fetch performs one scrape of a websocket collector.
func (webSocketRequester) Fetch(ctx context.Context, target string, c *model.Collector, overrides RequestOverrides, _ http.Header) (*HTTPResponse, error) {
	start := time.Now()
	c = withTargetAccount(c, overrides)
	own := webSocketRequest(c)
	address, secure, u, err := webSocketTarget(c, target)
	if err != nil {
		return nil, err
	}
	send := own.SendText
	for _, field := range webSocketTemplates(c, overrides) {
		if send, err = field.render(overrides.Params); err != nil {
			return nil, err
		}
	}
	headers, err := renderedValues(c.Request.Headers, "header", "request.headers.", overrides.Params)
	if err != nil {
		return nil, err
	}
	authorization, err := requestAuthorization(c)
	if err != nil {
		return nil, err
	}
	ctx, cancel := netBudget(ctx, overrides, webSocketTimeout)
	defer cancel()
	cfg, err := netTLSConfig(c, secure)
	if err != nil {
		return nil, err
	}
	scheme := map[bool]string{false: "ws", true: "wss"}[secure]
	if ctx, err = netGuard(ctx, c, scheme, address.hostPort); err != nil {
		return nil, err
	}
	label := webSocketURL(address, secure, u)
	traceRequest(ctx, "GET", label, nil, "", false)
	fail := func(err error) (*HTTPResponse, error) {
		traceFailedOutcome(ctx, "error: "+err.Error())
		return nil, err
	}
	conn, err := netDial(ctx, address, cfg)
	if err != nil {
		return fail(err)
	}
	defer conn.close()
	subprotocol, err := webSocketHandshake(ctx, conn, address, secure, u, headers, authorization, own.Subprotocols)
	if err != nil {
		return fail(fmt.Errorf("the handshake with %s: %w", label, err))
	}
	traceOutcome(ctx, "101 Switching Protocols")
	ws := &webSocket{ctx: ctx, conn: conn, left: responseLimit(c)}
	if send != "" {
		traceRequest(ctx, "SEND", label, nil, "", false)
		if err := ws.write(wsText, []byte(send)); err != nil {
			return fail(netFailure(ctx, fmt.Errorf("sending to %s: %w", label, err)))
		}
		traceOutcome(ctx, strconv.Itoa(len(send))+" bytes")
	}
	// The window ends before the scrape's deadline does, with time left to
	// answer.
	window := time.Duration(own.ReadWindow)
	if deadline, ok := ctx.Deadline(); ok {
		window = min(window, time.Until(deadline)*9/10)
	}
	end := time.Now().Add(window)
	_ = conn.SetReadDeadline(end)
	traceRequest(ctx, "READ", label, nil, "", false)
	var messages []webSocketMessage
	var single []byte
	binaryAnswer, closed := false, false
	for len(messages) < own.ReadMessages && !closed {
		opcode, payload, err := ws.message()
		switch {
		case errors.Is(err, errWebSocketClosed):
			closed = true
			continue
		case err != nil && errors.Is(err, os.ErrDeadlineExceeded) && ctx.Err() == nil && !time.Now().Before(end):
			closed = true
			continue
		case err != nil:
			return fail(netFailure(ctx, fmt.Errorf("reading from %s: %w", label, err)))
		}
		m := webSocketMessage{Binary: opcode == wsBinary}
		if m.Binary {
			m.PayloadBase64 = base64.StdEncoding.EncodeToString(payload)
		} else {
			text := string(payload)
			m.Payload = &text
			if len(payload) > 0 && json.Valid(payload) {
				m.JSON = json.RawMessage(payload)
			}
		}
		messages, single, binaryAnswer = append(messages, m), payload, m.Binary
	}
	// The closing handshake: a close frame, and the connection closed.
	_ = conn.SetWriteDeadline(time.Now().Add(time.Second))
	_ = ws.write(wsClose, []byte{0x03, 0xe8})
	resp := &HTTPResponse{StatusCode: http.StatusOK, NoStatus: true, Headers: http.Header{}, Target: target, Collector: c.Name}
	if subprotocol != "" {
		resp.Headers.Set("Sec-WebSocket-Protocol", subprotocol)
	}
	if own.ReadMessages == 1 {
		if len(messages) == 0 {
			return fail(fmt.Errorf("no message came from %s within request.read_window %s; a server that sends only when asked needs request.send_text", label, window.Round(time.Millisecond)))
		}
		resp.Body = single
		if binaryAnswer {
			resp.Headers.Set("Content-Type", "application/octet-stream")
		}
	} else {
		if messages == nil {
			messages = []webSocketMessage{}
		}
		if resp.Body, err = json.Marshal(messages); err != nil {
			return nil, err
		}
		if int64(len(resp.Body)) > responseLimit(c) {
			return fail(model.MarkError(model.Errorf("the list of %d messages is %d bytes, larger than the response limit; lower request.read_messages, or raise request.max_response_bytes or limits.max_response_bytes", len(messages), model.Size(int64(len(resp.Body)))), model.ErrLimitExceeded))
		}
		resp.Headers.Set("Content-Type", "application/json")
	}
	traceOutcome(ctx, fmt.Sprintf("%d messages", len(messages)))
	traceFacts(ctx, intFact("websocket.messages", len(messages)))
	resp.Duration = time.Since(start)
	if state := conn.tlsState(); state != nil {
		resp.Trip = &HTTPTrip{Total: resp.Duration, TLSConnection: tlsConnectionOf(state)}
	}
	return resp, nil
}

// webSocketHandshake makes the opening handshake (RFC 6455, 4.1) and
// returns the subprotocol the server chose.
func webSocketHandshake(ctx context.Context, conn *netConn, a netAddress, secure bool, u *url.URL, headers map[string]string, authorization string, subprotocols []string) (string, error) {
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	key := base64.StdEncoding.EncodeToString(nonce)
	host := a.hostPort
	if port := strconv.Itoa(webSocketPorts[map[bool]string{false: "ws", true: "wss"}[secure]]); strings.HasSuffix(host, ":"+port) {
		host = strings.TrimSuffix(host, ":"+port)
	}
	request := &http.Request{Method: http.MethodGet, URL: u, Host: host, Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1, Header: http.Header{}}
	request.Header.Set("User-Agent", "prometheus-universal-exporter")
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	if authorization != "" {
		request.Header.Set("Authorization", authorization)
	}
	request.Header.Set("Upgrade", "websocket")
	request.Header.Set("Connection", "Upgrade")
	request.Header["Sec-WebSocket-Key"] = []string{key}
	request.Header["Sec-WebSocket-Version"] = []string{"13"}
	if len(subprotocols) > 0 {
		request.Header["Sec-WebSocket-Protocol"] = []string{strings.Join(subprotocols, ", ")}
	}
	if err := request.Write(conn); err != nil {
		return "", netFailure(ctx, err)
	}
	answer, err := http.ReadResponse(conn.r, request)
	if answer != nil {
		// Nothing of a body is read: a 101 has the frames in its place,
		// which stay in the reader, and another status ends the scrape.
		defer func() { _ = answer.Body.Close() }()
	}
	if err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			err = errors.New("the server closed the connection")
		}
		return "", netFailure(ctx, fmt.Errorf("the answer is no HTTP response: %w", err))
	}
	if answer.StatusCode != http.StatusSwitchingProtocols {
		return "", fmt.Errorf("the server answered HTTP status %d, not 101: it does not serve a WebSocket there, or refuses this client", answer.StatusCode)
	}
	sum := sha1.Sum([]byte(key + webSocketGUID)) //nolint:gosec // as above
	switch {
	case !strings.EqualFold(answer.Header.Get("Upgrade"), "websocket") || !headerHasToken(answer.Header.Values("Connection"), "upgrade"):
		return "", errors.New("the answer is a 101 without Upgrade: websocket and Connection: Upgrade")
	case answer.Header.Get("Sec-Websocket-Accept") != base64.StdEncoding.EncodeToString(sum[:]):
		return "", errors.New("the answer's Sec-WebSocket-Accept is not the one this handshake's key makes: what answered is no WebSocket server, or a cache answered for it")
	case answer.Header.Get("Sec-Websocket-Extensions") != "":
		return "", fmt.Errorf("the server chose the extension %q, and none was offered", netShown(answer.Header.Get("Sec-Websocket-Extensions")))
	}
	chosen := answer.Header.Get("Sec-Websocket-Protocol")
	if chosen != "" && !slices.Contains(subprotocols, chosen) {
		return "", fmt.Errorf("the server chose the subprotocol %q, which request.subprotocols does not offer", netShown(chosen))
	}
	return chosen, nil
}

// headerHasToken says whether a header's values, lists with commas, have
// token among them, in any case.
func headerHasToken(values []string, token string) bool {
	for _, value := range values {
		for part := range strings.SplitSeq(value, ",") {
			if strings.EqualFold(strings.TrimSpace(part), token) {
				return true
			}
		}
	}
	return false
}

// errWebSocketClosed is the server's close frame.
var errWebSocketClosed = errors.New("the server closed the WebSocket")

// webSocket is a connection after its handshake.
type webSocket struct {
	ctx  context.Context
	conn *netConn
	// left is what the messages may still take of the response limit.
	left int64
}

// write sends one frame, whole and masked, as a client's frames are.
func (w *webSocket) write(opcode byte, payload []byte) error {
	frame := []byte{0x80 | opcode}
	switch n := len(payload); {
	case n < 126:
		frame = append(frame, 0x80|byte(n))
	case n < 1<<16:
		frame = binary.BigEndian.AppendUint16(append(frame, 0x80|126), uint16(n))
	default:
		frame = binary.BigEndian.AppendUint64(append(frame, 0x80|127), uint64(n))
	}
	mask := make([]byte, 4)
	if _, err := rand.Read(mask); err != nil {
		return err
	}
	frame = append(frame, mask...)
	for i, b := range payload {
		frame = append(frame, b^mask[i%4])
	}
	_, err := w.conn.Write(frame)
	return err
}

// frame reads one frame: whether it ends its message, its opcode and its
// payload.
func (w *webSocket) frame() (bool, byte, []byte, error) {
	head := make([]byte, 2)
	if _, err := io.ReadFull(w.conn.r, head); err != nil {
		return false, 0, nil, err
	}
	final, opcode, length := head[0]&0x80 != 0, head[0]&0x0f, uint64(head[1]&0x7f)
	switch {
	case head[0]&0x70 != 0:
		return false, 0, nil, errors.New("the server sent a frame of an extension, and none was agreed")
	case head[1]&0x80 != 0:
		return false, 0, nil, errors.New("the server sent a masked frame, which a server's are not")
	case opcode >= wsClose && (!final || length > 125):
		return false, 0, nil, errors.New("the server sent a control frame that is none")
	}
	if length >= 126 {
		wide := make([]byte, map[uint64]int{126: 2, 127: 8}[length])
		if _, err := io.ReadFull(w.conn.r, wide); err != nil {
			return false, 0, nil, err
		}
		if length = uint64(binary.BigEndian.Uint16(wide)); len(wide) == 8 {
			length = binary.BigEndian.Uint64(wide)
		}
	}
	if opcode < wsClose {
		if length > uint64(max(w.left, 0)) {
			return false, 0, nil, model.MarkError(model.Errorf("the messages are larger than the response limit; raise request.max_response_bytes or limits.max_response_bytes"), model.ErrLimitExceeded)
		}
		w.left -= int64(length)
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(w.conn.r, payload); err != nil {
		return false, 0, nil, err
	}
	return final, opcode, payload, nil
}

// message reads one message, its frames put together, answering a ping
// on the way; errWebSocketClosed when the server closes instead.
func (w *webSocket) message() (byte, []byte, error) {
	var kind byte
	var whole []byte
	started := false
	for {
		final, opcode, payload, err := w.frame()
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return 0, nil, errWebSocketClosed
			}
			return 0, nil, err
		}
		switch opcode {
		case wsPing:
			if err := w.write(wsPong, payload); err != nil {
				return 0, nil, err
			}
			continue
		case wsPong:
			continue
		case wsClose:
			return 0, nil, errWebSocketClosed
		case wsText, wsBinary:
			if started {
				return 0, nil, errors.New("the server began a message in the middle of another")
			}
			kind, started = opcode, true
		case wsContinuation:
			if !started {
				return 0, nil, errors.New("the server continued a message it had not begun")
			}
		default:
			return 0, nil, fmt.Errorf("the server sent a frame of opcode %d, which RFC 6455 has not", opcode)
		}
		whole = append(whole, payload...)
		if final {
			if kind == wsText && !utf8.Valid(whole) {
				return 0, nil, errors.New("the server sent a text message that is not UTF-8")
			}
			return kind, whole, nil
		}
	}
}

// webSocketKeyRules is what the configuration schema says of a websocket
// collector's own keys.
func webSocketKeyRules() map[string]map[string]any {
	return map[string]map[string]any{
		"send_text":     {"description": "websocket: a text message sent once the handshake is made, such as a subscription. May contain {{param_name}} placeholders, written as |json, |number, |form, |xml or |raw, as an http body's are. Left out, nothing is sent. See docs/WEBSOCKET.md."},
		"read_messages": {"minimum": 0, "maximum": webSocketMaxMessages, "description": "websocket: how many messages are read. 1, the default, makes the answer the message itself, and none within read_window fails the scrape; more makes it a JSON list of those that came, each with payload, json when it is JSON, or payload_base64 when binary. See docs/WEBSOCKET.md#the-answer."},
		"read_window":   {"description": "websocket: how long the messages are waited for after the handshake, and send_text; 5s when left out, and ended within the scrape's deadline."},
		"subprotocols":  {"description": "websocket: the subprotocols offered in Sec-WebSocket-Protocol, such as [graphql-transport-ws]; one the server chooses that is not among them fails the scrape."},
	}
}
