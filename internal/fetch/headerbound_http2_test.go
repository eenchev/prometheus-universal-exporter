//go:build !select_request_types || request_type_http

package fetch

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// Over HTTP/2 an answer with headers over the 1 MiB bound was a "connection
// error: PROTOCOL_ERROR" like any broken connection: retried as often as the
// collector retries, and reported in words that named neither the headers
// nor the bound. Go's HTTP/2 client ends such an answer in one of two ways,
// and each is now the end of the request, asked once:
//
//   - A header list it reads to its end and finds over the bound is the
//     limit error HTTP/1 gives, naming the headers. That takes headers that
//     arrive whole in little, as one value repeated does (HPACK sends a
//     repeat as one byte).
//   - A header list still arriving when it passes the bound has the
//     connection closed, with an error that says nothing of headers and that
//     a target breaking the protocol in another way gets too. That one
//     cannot be called a limit, so it is the transport failure it was, not
//     retried, saying that headers over the bound end this way.
//
// Headers just under the bound are read, and the request after a closed
// connection is answered on a new one.
func TestResponseHeadersAreBoundedOverHTTP2(t *testing.T) {
	var hits atomic.Int64
	var proto atomic.Pointer[string]
	// Each X-Pad value counts 937 bytes of the header list (its 900, the
	// name's 5 and 32 for the field), beside some 230 bytes of the other
	// headers; the bound is 1 MiB and Go's allowance of 320 bytes.
	repeated := map[string]int{"/just-under": 1110, "/just-over": 1125}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		proto.Store(&r.Proto)
		if r.URL.Path == "/arriving" {
			// 1.2 MiB in 150 different headers, which take 60 frames.
			for i := range 150 {
				w.Header().Set("X-Pad-"+string(rune('a'+i%26))+strings.Repeat("x", i/26), strings.Repeat("a", 8000))
			}
		}
		value := strings.Repeat("a", 900)
		for range repeated[r.URL.Path] {
			w.Header().Add("X-Pad", value)
		}
		_, _ = w.Write([]byte("ok"))
	}))
	server.EnableHTTP2 = true
	// The server logs the GOAWAY of each connection the client closes.
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.StartTLS()
	defer server.Close()
	previous := transports
	transports = newTransportCache()
	t.Cleanup(func() { transports = previous })
	c := httpCollector(t, func(c *model.Collector) {
		c.Request.AllowedSchemes = []string{"https"}
		c.Request.TLS.InsecureSkipVerify = true
		c.Request.EnableHTTP2 = true
		c.Request.MaxResponseBytes = 100
		c.Request.Retry.Attempts = 2
	})
	fetch := func(path string) (*HTTPResponse, error) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		hits.Store(0)
		resp, err := FetchCollector(ctx, server.URL+path, c, RequestOverrides{}, nil)
		if got := proto.Load(); got == nil || *got != "HTTP/2.0" {
			t.Fatalf("%s: the target was asked over %v, want HTTP/2.0", path, got)
		}
		return resp, err
	}

	resp, err := fetch("/just-under")
	if err != nil || string(resp.Body) != "ok" || len(resp.Headers.Values("X-Pad")) != 1110 {
		t.Fatalf("headers just under the bound: err=%v", err)
	}

	_, err = fetch("/just-over")
	if !errors.Is(err, model.ErrLimitExceeded) || !strings.Contains(err.Error(), "response headers are larger than 1048576 bytes") {
		t.Fatalf("headers just over the bound, read to their end: err=%v, want a limit error naming the headers", err)
	}
	if n := hits.Load(); n != 1 {
		t.Fatalf("headers just over the bound: the target was asked %d times, want once", n)
	}

	_, err = fetch("/arriving")
	if err == nil || errors.Is(err, model.ErrLimitExceeded) {
		t.Fatalf("headers over the bound while arriving: err=%v, want a transport failure, since the cause cannot be told", err)
	}
	for _, want := range []string{"PROTOCOL_ERROR", "not retried", "headers are larger than 1048576 bytes", "HTTP/2"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("headers over the bound while arriving: err=%v, want it to say %q", err, want)
		}
	}
	if n := hits.Load(); n != 1 {
		t.Fatalf("headers over the bound while arriving: the target was asked %d times, want once", n)
	}

	if resp, err := fetch("/metrics"); err != nil || string(resp.Body) != "ok" {
		t.Fatalf("the request after a closed connection: err=%v", err)
	}
}

// fakeConnectionError is shaped as the HTTP/2 client's connection error is: an
// error code under a type named for it.
type fakeConnectionError uint32

func (e fakeConnectionError) Error() string { return "connection error" }

// Only the HTTP/2 client's own connection error for a protocol violation is
// taken for one, wrapped or not: an error that merely says the words, a
// connection error with another code and a failure of the network are
// retried as before.
func TestOnlyAnHTTP2ConnectionProtocolErrorIsTakenForOne(t *testing.T) {
	const protocol, internal = fakeConnectionError(1), fakeConnectionError(2)
	if !http2ProtocolError(protocol) || !http2ProtocolError(&url.Error{Op: "Get", URL: "https://target.example/", Err: protocol}) {
		t.Error("a connection error with the code PROTOCOL_ERROR was not taken for one")
	}
	for _, err := range []error{
		internal,
		&url.Error{Op: "Get", URL: "https://target.example/", Err: internal},
		errors.New("connection error: PROTOCOL_ERROR"),
		errors.New("read tcp 127.0.0.1:1->127.0.0.1:2: read: connection reset by peer"),
		context.DeadlineExceeded,
		io.ErrUnexpectedEOF,
		nil,
	} {
		if http2ProtocolError(err) {
			t.Errorf("%v was taken for an HTTP/2 protocol error", err)
		}
	}
}
