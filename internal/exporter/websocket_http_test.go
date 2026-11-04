//go:build !select_request_types || request_type_http

package exporter

import (
	"bufio"
	"crypto/sha1" //nolint:gosec // G505: RFC 6455's handshake is SHA-1 by definition
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// webSocketKey is the Sec-WebSocket-Key of the recipe's example, the 16
// bytes of RFC 6455's own example in base64, and webSocketAccept the
// Sec-WebSocket-Accept a server answers it with (RFC 6455, section 1.3).
const (
	webSocketKey    = "dGhlIHNhbXBsZSBub25jZQ=="
	webSocketAccept = "s3pPLMBiTxaQ9kYGzzhZRbK+xOo="
)

// webSocketServer is a WebSocket endpoint as RFC 6455 has it: a GET with
// Upgrade: websocket, Connection: Upgrade, Sec-WebSocket-Version: 13 and a
// key is answered 101 Switching Protocols with the key's
// Sec-WebSocket-Accept, after which the connection is the WebSocket's,
// which the server keeps open, sending a text frame now and then, until the
// client closes it; any other request is answered 400. It records the
// upgrade requests it saw, and how long each connection stayed open after
// its 101.
type webSocketServer struct {
	*httptest.Server
	mu       sync.Mutex
	upgrades []http.Header
	open     []time.Duration
	closed   chan struct{}
}

func newWebSocketServer(t *testing.T, secure bool) *webSocketServer {
	t.Helper()
	s := &webSocketServer{closed: make(chan struct{}, 8)}
	s.Server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") || !strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade") ||
			r.Header.Get("Sec-WebSocket-Version") != "13" || r.Header.Get("Sec-WebSocket-Key") == "" {
			http.Error(w, "not a WebSocket handshake", http.StatusBadRequest)
			return
		}
		s.mu.Lock()
		s.upgrades = append(s.upgrades, r.Header.Clone())
		s.mu.Unlock()
		sum := sha1.Sum([]byte(r.Header.Get("Sec-WebSocket-Key") + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11")) //nolint:gosec // G401: as above
		conn, buffered, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = io.WriteString(conn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: "+base64.StdEncoding.EncodeToString(sum[:])+"\r\n\r\n")
		upgraded := time.Now()
		// The WebSocket's own traffic: a text frame "hi" every 50ms,
		// while whatever the client sends is read, until it closes the
		// connection or ten seconds pass.
		gone := make(chan struct{})
		go func() {
			_, _ = io.Copy(io.Discard, bufio.NewReader(buffered))
			close(gone)
		}()
		tick := time.NewTicker(50 * time.Millisecond)
		defer tick.Stop()
		deadline := time.After(10 * time.Second)
		for {
			select {
			case <-gone:
				s.mu.Lock()
				s.open = append(s.open, time.Since(upgraded))
				s.mu.Unlock()
				s.closed <- struct{}{}
				return
			case <-deadline:
				return
			case <-tick.C:
				if _, err := conn.Write([]byte{0x81, 0x02, 'h', 'i'}); err != nil {
					<-gone
					s.mu.Lock()
					s.open = append(s.open, time.Since(upgraded))
					s.mu.Unlock()
					s.closed <- struct{}{}
					return
				}
			}
		}
	}))
	if secure {
		// A wss:// endpoint whose server speaks HTTP/2 too.
		s.EnableHTTP2 = true
		s.StartTLS()
	} else {
		s.Start()
	}
	t.Cleanup(s.Close)
	return s
}

// webSocketConfig is the recipe's check of a WebSocket endpoint
// (docs/REQUESTS.md, "WebSocket endpoints").
const webSocketConfig = `collectors:
  - name: websocket
    report_failures: true
    request:
      type: http
      method: GET
      headers:
        Connection: Upgrade
        Upgrade: websocket
        Sec-WebSocket-Version: "13"
        Sec-WebSocket-Key: ` + webSocketKey + `
      new_connection: true
      accept_status: [101]
    decoder:
      type: none
    transform:
      type: jq
    metrics:
      - name: websocket_up
        description: Whether the endpoint switched to the WebSocket protocol, answering the key as RFC 6455 has it
        expression: 'if $status == 101 and $headers["sec-websocket-accept"] == "` + webSocketAccept + `" then 1 else 0 end'
      - name: websocket_http_status_code
        expression: $status
`

// An http collector checks a WebSocket endpoint's upgrade, as the recipe
// has it: its request carries Connection: Upgrade, Upgrade: websocket and
// the version and key, the endpoint answers 101 with the key's
// Sec-WebSocket-Accept, and the probe answers websocket_up 1 and the status
// at once, with the connection closed, though the endpoint keeps sending on
// it, with new_connection and without, and over TLS with enable_http2. An
// endpoint that does not upgrade answers websocket_up 0 and its
// status rather than failing the probe, with accept_status: [any].
func TestAnHTTPCollectorChecksAWebSocketUpgrade(t *testing.T) {
	testutil.CaptureLogs(t)
	endpoint := newWebSocketServer(t, false)
	cfg, err := config.Load(testutil.WriteFile(t, "config.yaml", webSocketConfig))
	if err != nil {
		t.Fatal(err)
	}
	server := flightServer(t, cfg.Collectors...)
	// The second probe without new_connection: the 101's connection is
	// closed all the same, and not kept for another request.
	for probe, extra := range []string{"", "&new_connection=false"} {
		start := time.Now()
		got := probeOnce(t, server, probePath("websocket", endpoint.URL, extra), nil)
		took := time.Since(start)
		body := got.Body.String()
		for _, want := range []string{"websocket_up 1\n", "websocket_http_status_code 101\n", "http_exporter_probe_success 1\n"} {
			if got.Code != http.StatusOK || !strings.Contains(body, want) {
				t.Errorf("probe %d: %d, no %q in\n%s", probe, got.Code, want, body)
			}
		}
		if took > 2*time.Second {
			t.Errorf("probe %d took %s, as though the WebSocket were read as the body", probe, took)
		}
		select {
		case <-endpoint.closed:
		case <-time.After(2 * time.Second):
			t.Fatalf("probe %d left the WebSocket connection open", probe)
		}
	}
	endpoint.mu.Lock()
	upgrades, open := endpoint.upgrades, endpoint.open
	endpoint.mu.Unlock()
	if len(upgrades) != 2 || upgrades[0].Get("Sec-WebSocket-Key") != webSocketKey || upgrades[0].Get("Upgrade") != "websocket" {
		t.Errorf("the endpoint saw the upgrades %v", upgrades)
	}
	for _, d := range open {
		if d > time.Second {
			t.Errorf("a WebSocket connection stayed open %s after its 101", d)
		}
	}

	// Over TLS, with enable_http2 and a server that speaks HTTP/2, the
	// handshake is made over HTTP/1.1, which Go keeps to for a request
	// asking for a WebSocket: HTTP/2 has no Upgrade.
	secure := newWebSocketServer(t, true)
	got := probeOnce(t, server, probePath("websocket", secure.URL, "&enable_http2=true&insecure_skip_verify=true"), nil)
	if body := got.Body.String(); !strings.Contains(body, "websocket_up 1\n") {
		t.Errorf("a wss:// endpoint under enable_http2: %d\n%s", got.Code, body)
	}
	select {
	case <-secure.closed:
	case <-time.After(2 * time.Second):
		t.Fatal("the wss:// probe left the WebSocket connection open")
	}

	// Without the upgrade's headers, the endpoint answers 400, which
	// accept_status: [any] makes the answer.
	plain := strings.Replace(strings.Replace(webSocketConfig, "        Upgrade: websocket\n", "", 1), "accept_status: [101]", "accept_status: [any]", 1)
	cfg, err = config.Load(testutil.WriteFile(t, "plain.yaml", plain))
	if err != nil {
		t.Fatal(err)
	}
	got = probeOnce(t, flightServer(t, cfg.Collectors...), probePath("websocket", endpoint.URL, ""), nil)
	if body := got.Body.String(); !strings.Contains(body, "websocket_up 0\n") || !strings.Contains(body, "websocket_http_status_code 400\n") {
		t.Errorf("an endpoint that does not upgrade: %d\n%s", got.Code, body)
	}
}
