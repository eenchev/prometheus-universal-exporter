//go:build request_type_websocket

package fetch

import (
	"bufio"
	"context"
	"crypto/sha1" //nolint:gosec // RFC 6455's handshake
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// A websocket collector's placeholders are in the message it sends, and in
// its header values, which the shared test fills.
func init() {
	ownPlaceholders[RequestTypeWebSocket] = func(c *model.Collector) {
		c.Request.Own = WebSocketConfig{SendText: `{"channel":{{param_channel:x|json}}}`}
	}
}

// wsFrame is a frame the server of the tests sends.
type wsFrame struct {
	first   byte // FIN and the opcode
	payload string
}

// wsServer is a WebSocket server as the tests need one: it answers the
// handshake, with status, accept and protocol when they are set in place
// of its own, sends frames, echoes each text message it is sent when echo
// is set, and keeps the handshake's request and the frames it was sent.
type wsServer struct {
	*testutil.TCPStub
	mu       sync.Mutex
	status   int
	accept   string
	protocol string
	frames   []wsFrame
	echo     bool
	// closeAfter closes the connection once the frames are sent.
	closeAfter bool
	request    *http.Request
	// got is the opcode and the payload of each frame it was sent, and
	// whether every one was masked.
	got    []wsFrame
	masked bool
}

func newWSServer(t *testing.T, secure bool) *wsServer {
	t.Helper()
	s := &wsServer{masked: true}
	if secure {
		s.TCPStub = testutil.NewTLSTCPStub(t, s.serve)
	} else {
		s.TCPStub = testutil.NewTCPStub(t, s.serve)
	}
	return s
}

func wsWrite(conn net.Conn, f wsFrame) {
	out := []byte{f.first}
	switch n := len(f.payload); {
	case n < 126:
		out = append(out, byte(n))
	case n < 1<<16:
		out = binary.BigEndian.AppendUint16(append(out, 126), uint16(n))
	default:
		out = binary.BigEndian.AppendUint64(append(out, 127), uint64(n))
	}
	_, _ = conn.Write(append(out, f.payload...))
}

func (s *wsServer) serve(conn net.Conn) {
	reader := bufio.NewReader(conn)
	request, err := http.ReadRequest(reader)
	if err != nil {
		return
	}
	s.mu.Lock()
	s.request = request
	status, accept, protocol, closeAfter := s.status, s.accept, s.protocol, s.closeAfter
	frames := append([]wsFrame(nil), s.frames...)
	s.mu.Unlock()
	if status != 0 {
		_, _ = fmt.Fprintf(conn, "HTTP/1.1 %d Nope\r\nContent-Length: 0\r\n\r\n", status)
		return
	}
	if accept == "" {
		sum := sha1.Sum([]byte(request.Header.Get("Sec-WebSocket-Key") + webSocketGUID)) //nolint:gosec // as above
		accept = base64.StdEncoding.EncodeToString(sum[:])
	}
	answer := "HTTP/1.1 101 Switching Protocols\r\nUpgrade: WebSocket\r\nConnection: keep-alive, Upgrade\r\nSec-WebSocket-Accept: " + accept + "\r\n"
	if protocol != "" {
		answer += "Sec-WebSocket-Protocol: " + protocol + "\r\n"
	}
	_, _ = io.WriteString(conn, answer+"\r\n")
	for _, f := range frames {
		wsWrite(conn, f)
	}
	if closeAfter {
		return
	}
	for {
		head := make([]byte, 2)
		if _, err := io.ReadFull(reader, head); err != nil {
			return
		}
		length := int(head[1] & 0x7f)
		if length == 126 {
			wide := make([]byte, 2)
			_, _ = io.ReadFull(reader, wide)
			length = int(binary.BigEndian.Uint16(wide))
		}
		mask := make([]byte, 4)
		if head[1]&0x80 != 0 {
			_, _ = io.ReadFull(reader, mask)
		}
		payload := make([]byte, length)
		if _, err := io.ReadFull(reader, payload); err != nil {
			return
		}
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
		s.mu.Lock()
		s.got = append(s.got, wsFrame{head[0], string(payload)})
		s.masked = s.masked && head[1]&0x80 != 0
		echo := s.echo
		s.mu.Unlock()
		switch head[0] & 0x0f {
		case wsText:
			if echo {
				wsWrite(conn, wsFrame{0x80 | wsText, "echo:" + string(payload)})
			}
		case wsClose:
			return
		}
	}
}

// set changes what the server answers with, and reads what it was sent,
// under its lock.
func (s *wsServer) set(change func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	change()
}

// sent is the frames the server was sent, once the client's close frame
// is among them.
func (s *wsServer) sent(t *testing.T) []wsFrame {
	t.Helper()
	var got []wsFrame
	testutil.WaitFor(t, "the client's close frame", func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		got = append([]wsFrame(nil), s.got...)
		return len(got) > 0 && got[len(got)-1].first == 0x80|wsClose
	})
	return got
}

func wsCollector(t *testing.T, own WebSocketConfig, change ...func(*model.RequestConfig)) *model.Collector {
	t.Helper()
	if own.ReadWindow == 0 {
		own.ReadWindow = model.Duration(10 * time.Second)
	}
	c := &model.Collector{Name: "feed", Request: model.RequestConfig{Type: RequestTypeWebSocket, Own: own}}
	for _, fn := range change {
		fn(&c.Request)
	}
	if err := ValidateRequest(c); err != nil {
		t.Fatal(err)
	}
	return c
}

// A scrape makes the opening handshake at the target's path and query,
// with the collector's headers, its bearer token and its subprotocols,
// sends its text message, a placeholder filled from the probe as an http
// body's is, in a masked frame, and answers with the first message that
// comes, as the server sent it; then it sends a close frame.
func TestAWebSocketScrapeSendsAndReadsOneMessage(t *testing.T) {
	s := newWSServer(t, false)
	s.set(func() { s.echo, s.protocol = true, "v2.feed" })
	token := testutil.WriteFile(t, "token", "tok-123\n")
	c := wsCollector(t, WebSocketConfig{SendText: `{"subscribe":{{param_channel|json}}}`, Subprotocols: []string{"v1.feed", "v2.feed"}}, func(r *model.RequestConfig) {
		r.Headers, r.BearerTokenFile = map[string]string{"X-Tenant": "{{param_tenant}}"}, token
	})
	resp, err := FetchCollector(context.Background(), "ws://"+s.Addr+"/feed/v1?depth=5", c, RequestOverrides{Params: map[string]string{"param_channel": `a"b`, "param_tenant": "acme"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(resp.Body) != `echo:{"subscribe":"a\"b"}` || resp.Headers.Get("Sec-WebSocket-Protocol") != "v2.feed" {
		t.Errorf("the answer is %q, %v", resp.Body, resp.Headers)
	}
	var r *http.Request
	masked := false
	s.set(func() { r, masked = s.request, s.masked })
	if r.Method != http.MethodGet || r.RequestURI != "/feed/v1?depth=5" || r.Host != s.Addr || r.Header.Get("Upgrade") != "websocket" || r.Header.Get("Connection") != "Upgrade" || r.Header.Get("Sec-WebSocket-Version") != "13" {
		t.Errorf("the handshake was %s %s of %s, %v", r.Method, r.RequestURI, r.Host, r.Header)
	}
	if key, err := base64.StdEncoding.DecodeString(r.Header.Get("Sec-WebSocket-Key")); err != nil || len(key) != 16 {
		t.Errorf("the key is %q", r.Header.Get("Sec-WebSocket-Key"))
	}
	if r.Header.Get("X-Tenant") != "acme" || r.Header.Get("Authorization") != "Bearer tok-123" || r.Header.Get("Sec-WebSocket-Protocol") != "v1.feed, v2.feed" {
		t.Errorf("the handshake's headers are %v", r.Header)
	}
	got := s.sent(t)
	s.set(func() { masked = s.masked })
	if len(got) != 2 || got[0] != (wsFrame{0x80 | wsText, `{"subscribe":"a\"b"}`}) || !masked {
		t.Errorf("the server was sent %q, masked %v", got, masked)
	}
	// The label and what a log shows of the target leave the query out.
	if label, err := RequestLabelFor("ws://"+s.Addr+"/feed/v1?token=x", c, RequestOverrides{}); err != nil || label != "ws://"+s.Addr+"/feed/v1" {
		t.Errorf("the label is %q, %v", label, err)
	}
	if shown := (webSocketRequester{}).Display("ws://host.example/feed?token=x"); shown != "ws://host.example/feed?***" {
		t.Errorf("the target is shown as %q", shown)
	}
}

// With read_messages more than 1 the answer is a JSON list of the
// messages: one that came in several frames put together, a ping between
// them answered with its pong, a binary one in base64, and JSON beside the
// text that is JSON; a server that closes first is answered with what
// came.
func TestAWebSocketScrapeReadsSeveralMessages(t *testing.T) {
	s := newWSServer(t, false)
	s.set(func() {
		s.frames = []wsFrame{
			{wsText, `{"price":`},                // not final
			{0x80 | wsPing, "are you there"},     // between the frames of a message
			{0x80 | wsContinuation, `101.5}`},    // ends it
			{0x80 | wsBinary, "\x00\x01"},        // binary
			{0x80 | wsText, "plain text"},        // text that is no JSON
			{0x80 | wsText, "never read: three"}, // past read_messages
		}
	})
	c := wsCollector(t, WebSocketConfig{ReadMessages: 3})
	resp, err := FetchCollector(context.Background(), s.Addr, c, RequestOverrides{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := `[{"payload":"{\"price\":101.5}","json":{"price":101.5},"binary":false},{"payload_base64":"AAE=","binary":true},{"payload":"plain text","binary":false}]`
	if string(resp.Body) != want || resp.Headers.Get("Content-Type") != "application/json" {
		t.Errorf("the answer is %s\nwant %s", resp.Body, want)
	}
	if got := s.sent(t); got[0] != (wsFrame{0x80 | wsPong, "are you there"}) {
		t.Errorf("the server was sent %q, want the ping's pong first", got)
	}
	closing := newWSServer(t, false)
	closing.set(func() { closing.frames, closing.closeAfter = []wsFrame{{0x80 | wsText, "1"}}, true })
	resp, err = FetchCollector(context.Background(), closing.Addr, wsCollector(t, WebSocketConfig{ReadMessages: 5}), RequestOverrides{}, nil)
	if err != nil || string(resp.Body) != `[{"payload":"1","json":1,"binary":false}]` {
		t.Errorf("a server that closed after one message: %s, %v", resp.Body, err)
	}
}

// No message within read_window fails a scrape that reads one, and
// answers an empty list for one that reads several; a message past the
// response limit fails the scrape as a limit exceeded.
func TestAWebSocketScrapeHoldsItsWindowAndTheResponseLimit(t *testing.T) {
	s := newWSServer(t, false)
	window := model.Duration(60 * time.Millisecond)
	_, err := FetchCollector(context.Background(), s.Addr, wsCollector(t, WebSocketConfig{ReadWindow: window}), RequestOverrides{}, nil)
	if err == nil || !strings.Contains(err.Error(), "no message came from ws://"+s.Addr+"/ within request.read_window 60ms") {
		t.Errorf("err=%v, want the window named", err)
	}
	resp, err := FetchCollector(context.Background(), s.Addr, wsCollector(t, WebSocketConfig{ReadWindow: window, ReadMessages: 2}), RequestOverrides{}, nil)
	if err != nil || string(resp.Body) != "[]" {
		t.Errorf("several messages, none came: %s, %v", resp.Body, err)
	}
	s.set(func() { s.frames = []wsFrame{{0x80 | wsText, strings.Repeat("x", 70000)}} })
	_, err = FetchCollector(context.Background(), s.Addr, wsCollector(t, WebSocketConfig{}, func(r *model.RequestConfig) { r.MaxResponseBytes = 65536 }), RequestOverrides{}, nil)
	if !errors.Is(err, model.ErrLimitExceeded) {
		t.Errorf("err=%v, want the response limit", err)
	}
}

// A handshake that is not answered as RFC 6455 has it fails the scrape:
// another status, an accept that this handshake's key does not make, a
// subprotocol that was not offered; and so does a frame a server may not
// send.
func TestAWebSocketScrapeFailsOnWhatIsNoWebSocket(t *testing.T) {
	c := wsCollector(t, WebSocketConfig{Subprotocols: []string{"v1"}})
	for name, tc := range map[string]struct {
		change func(*wsServer)
		want   string
	}{
		"status":      {func(s *wsServer) { s.status = 404 }, "the server answered HTTP status 404, not 101"},
		"accept":      {func(s *wsServer) { s.accept = "bm90IGl0" }, "Sec-WebSocket-Accept is not the one this handshake's key makes"},
		"subprotocol": {func(s *wsServer) { s.protocol = "v9" }, `the server chose the subprotocol "v9"`},
		"extension":   {func(s *wsServer) { s.frames = []wsFrame{{0xC0 | wsText, "x"}} }, "a frame of an extension"},
		"orphan":      {func(s *wsServer) { s.frames = []wsFrame{{0x80 | wsContinuation, "x"}} }, "continued a message it had not begun"},
		"not utf-8":   {func(s *wsServer) { s.frames = []wsFrame{{0x80 | wsText, "\xff"}} }, "a text message that is not UTF-8"},
	} {
		s := newWSServer(t, false)
		s.set(func() { tc.change(s) })
		_, err := FetchCollector(context.Background(), s.Addr, c, RequestOverrides{}, nil)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err=%v, want %q", name, err, tc.want)
		}
	}
}

// A wss:// target is TLS, verified with the collector's tls block, its
// certificate what $tls describes, and its Host without the port only
// when that is the scheme's own.
func TestAWSSTargetIsTLS(t *testing.T) {
	s := newWSServer(t, true)
	s.set(func() { s.frames = []wsFrame{{0x80 | wsText, "ok"}} })
	c := wsCollector(t, WebSocketConfig{}, func(r *model.RequestConfig) { r.TLS = model.TLSConfig{CAFile: s.CAFile} })
	resp, err := FetchCollector(context.Background(), "wss://"+s.Addr+"/", c, RequestOverrides{}, nil)
	if err != nil || string(resp.Body) != "ok" || resp.Trip == nil || resp.Trip.TLSConnection == nil {
		t.Fatalf("%v, %+v", err, resp)
	}
	host := ""
	s.set(func() { host = s.request.Host })
	if host != s.Addr {
		t.Errorf("the Host is %q", host)
	}
	if _, err := FetchCollector(context.Background(), "ws://"+s.Addr, c, RequestOverrides{}, nil); err == nil {
		t.Error("a plaintext handshake with a TLS server was answered")
	}
}

// A collector is refused at load for a header the handshake writes itself,
// a subprotocol that is no token and a count that is none.
func TestWebSocketConfigurationRules(t *testing.T) {
	for name, tc := range map[string]struct {
		own    WebSocketConfig
		change func(*model.RequestConfig)
		want   string
	}{
		"upgrade": {WebSocketConfig{}, func(r *model.RequestConfig) { r.Headers = map[string]string{"upgrade": "h2c"} }, "request.headers sets upgrade, which the handshake writes itself"},
		"key":     {WebSocketConfig{}, func(r *model.RequestConfig) { r.Headers = map[string]string{"Sec-WebSocket-Key": "x"} }, "which the handshake writes itself"},
		"authorization": {WebSocketConfig{}, func(r *model.RequestConfig) {
			r.Headers, r.BearerTokenFile = map[string]string{"Authorization": "x"}, "/t"
		}, "both request.bearer_token_file and the Authorization header"},
		"subprotocol": {WebSocketConfig{Subprotocols: []string{"a b"}}, nil, `request.subprotocols[0] "a b" is not a subprotocol's name`},
		"messages":    {WebSocketConfig{ReadMessages: -1}, nil, "request.read_messages -1 is not 1 to 10000"},
		"filter":      {WebSocketConfig{SendText: "{{param_a|shell}}"}, nil, `the unknown filter "shell"`},
	} {
		c := &model.Collector{Name: "feed", Request: model.RequestConfig{Type: RequestTypeWebSocket, Own: tc.own}}
		if tc.change != nil {
			tc.change(&c.Request)
		}
		if err := ValidateRequest(c); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err=%v, want %q", name, err, tc.want)
		}
	}
	c := wsCollector(t, WebSocketConfig{})
	for _, target := range []string{"http://host.example/feed", "ws://host.example/a b", "ws://user@host.example/", "ws://host.example/#frag"} {
		if _, err := FetchCollector(context.Background(), target, c, RequestOverrides{}, nil); err == nil {
			t.Errorf("the target %q is taken", target)
		}
	}
}

// A dry run reads the files a scrape would: it warns of a bearer token
// file and of a certificate authority's that are not there, and of nothing
// when they are.
func TestAWebSocketCollectorsFilesAreCheckedForADryRun(t *testing.T) {
	check := RequestTypes[RequestTypeWebSocket].CheckFiles
	missing := wsCollector(t, WebSocketConfig{}, func(r *model.RequestConfig) {
		r.BearerTokenFile, r.TLS = "/nonexistent/token", model.TLSConfig{CAFile: "/nonexistent/ca.pem"}
	})
	warnings := strings.Join(check(missing, nil), "\n")
	if !strings.Contains(warnings, `collector "feed": request.bearer_token_file:`) || !strings.Contains(warnings, `collector "feed": request.tls:`) {
		t.Errorf("the warnings are %q", warnings)
	}
	there := wsCollector(t, WebSocketConfig{}, func(r *model.RequestConfig) { r.BearerTokenFile = testutil.WriteFile(t, "token", "tok\n") })
	if warnings := check(there, nil); len(warnings) != 0 {
		t.Errorf("files that are there are warned of: %q", warnings)
	}
}

// A server among the collector's denied targets is refused before it is
// connected to.
func TestAWebSocketCollectorHoldsTheTargetPolicy(t *testing.T) {
	s := newWSServer(t, false)
	c := wsCollector(t, WebSocketConfig{}, func(r *model.RequestConfig) { r.DeniedTargets = []string{"127.0.0.0/8"} })
	if _, err := FetchCollector(context.Background(), s.Addr, c, RequestOverrides{}, nil); !errors.Is(err, ErrTargetRefused) || s.Accepted() != 0 {
		t.Errorf("err=%v, %d connections; want the target refused", err, s.Accepted())
	}
}

// The keys the opt-in websocket type takes that no type of a default build does
// are listed (optInRequestKeys, optInTargetKeys).
func TestTheWebSocketTypesOptInKeysAreListed(t *testing.T) {
	checkOptInRequestKeys(t, RequestTypeWebSocket)
}
