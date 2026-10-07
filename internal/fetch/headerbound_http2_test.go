//go:build !select_request_types || request_type_http

package fetch

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// Over HTTP/2 an answer with headers over the 1 MiB bound was a "connection
// error: PROTOCOL_ERROR" like any broken connection: retried as often as the
// collector retries, and reported in words that named neither the headers
// nor the bound. Go's HTTP/2 client ends such an answer in one of two ways:
//
//   - A header list it reads to its end and finds over the bound is the
//     limit error HTTP/1 gives, naming the headers, and the end of the
//     request, asked once. That takes headers that arrive whole in little,
//     as one value repeated does (HPACK sends a repeat as one byte).
//   - A header list still arriving when it passes the bound has the
//     connection closed, with an error that says nothing of headers, that a
//     target breaking the protocol in another way gets too, and that every
//     other request waiting on the connection is handed as well. That one
//     cannot be called a limit, so it is the transport failure it was,
//     saying that headers over the bound end this way. It is retried once,
//     where the collector retries at all, on a new connection, and ends the
//     request when it comes again, however many retries are left: it was
//     first not retried at all, which failed the healthy requests sharing
//     the connection with it
//     (TestAHealthyRequestIsRetriedWhenAnotherClosesItsHTTP2Connection).
//
// Headers just under the bound are read, and the request after a closed
// connection is answered on a new one.
func TestResponseHeadersAreBoundedOverHTTP2(t *testing.T) {
	var hits atomic.Int64
	var proto atomic.Pointer[string]
	// The connection each request arrived on, by the client's address.
	var mu sync.Mutex
	var arrivedOn []string
	// Each X-Pad value counts 937 bytes of the header list (its 900, the
	// name's 5 and 32 for the field), beside some 230 bytes of the other
	// headers; the bound is 1 MiB and Go's allowance of 320 bytes.
	repeated := map[string]int{"/just-under": 1110, "/just-over": 1125}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		proto.Store(&r.Proto)
		mu.Lock()
		arrivedOn = append(arrivedOn, r.RemoteAddr)
		mu.Unlock()
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
	fetchWith := func(path string, overrides RequestOverrides) (*HTTPResponse, error) {
		t.Helper()
		// A minute bounds a fetch that hangs: the headers are megabytes,
		// which a busy machine takes its time over.
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		hits.Store(0)
		mu.Lock()
		arrivedOn = nil
		mu.Unlock()
		resp, err := FetchCollector(ctx, server.URL+path, c, overrides, nil)
		if got := proto.Load(); got == nil || *got != "HTTP/2.0" {
			t.Fatalf("%s: the target was asked over %v, want HTTP/2.0", path, got)
		}
		return resp, err
	}
	fetch := func(path string) (*HTTPResponse, error) {
		t.Helper()
		return fetchWith(path, RequestOverrides{})
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
	for _, want := range []string{"PROTOCOL_ERROR", "as the connection before it was, so the request is not retried again", "headers are larger than 1048576 bytes", "HTTP/2"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("headers over the bound while arriving: err=%v, want it to say %q", err, want)
		}
	}
	// Asked twice of the three times retry.attempts: 2 allows, the second
	// time on a new connection.
	if n := hits.Load(); n != 2 {
		t.Fatalf("headers over the bound while arriving: the target was asked %d times, want twice", n)
	}
	mu.Lock()
	if len(arrivedOn) != 2 || arrivedOn[0] == arrivedOn[1] {
		t.Errorf("headers over the bound while arriving: the requests arrived on the connections %v, want two different ones", arrivedOn)
	}
	mu.Unlock()

	// A collector that does not retry is asked once, as for any failure.
	none := 0
	_, err = fetchWith("/arriving", RequestOverrides{RetryAttempts: &none})
	if err == nil || errors.Is(err, model.ErrLimitExceeded) {
		t.Fatalf("headers over the bound while arriving, retry.attempts: 0: err=%v, want a transport failure", err)
	}
	for _, want := range []string{"PROTOCOL_ERROR", "in answer to this request or to another on the same connection, and the request had no retry left", "headers are larger than 1048576 bytes", "HTTP/2"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("headers over the bound while arriving, retry.attempts: 0: err=%v, want it to say %q", err, want)
		}
	}
	if n := hits.Load(); n != 1 {
		t.Fatalf("headers over the bound while arriving, retry.attempts: 0: the target was asked %d times, want once", n)
	}

	if resp, err := fetch("/metrics"); err != nil || string(resp.Body) != "ok" {
		t.Fatalf("the request after a closed connection: err=%v", err)
	}
}

// A healthy request failed, unretried, when another request's oversized
// headers closed the HTTP/2 connection the two shared: Go's client hands the
// one PROTOCOL_ERROR to every request waiting on a connection it closes, and
// that error was the end of a request whatever retry.attempts said. A
// request that draws it is now retried once, and the retry is sent on a new
// connection, so the healthy request gets its answer there.
func TestAHealthyRequestIsRetriedWhenAnotherClosesItsHTTP2Connection(t *testing.T) {
	var healthyAsked, faultyAsked, connections atomic.Int64
	waiting := make(chan struct{}, 1)
	release := make(chan struct{})
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/arriving" {
			faultyAsked.Add(1)
			for i := range 150 {
				w.Header().Set("X-Pad-"+string(rune('a'+i%26))+strings.Repeat("x", i/26), strings.Repeat("a", 8000))
			}
			_, _ = w.Write([]byte("too much"))
			return
		}
		// The healthy request's answer is held until the faulty request
		// has had its own, so the two are on the connection together.
		if healthyAsked.Add(1) == 1 {
			waiting <- struct{}{}
		}
		select {
		case <-release:
		case <-r.Context().Done():
		}
		_, _ = w.Write([]byte("ok"))
	}))
	server.EnableHTTP2 = true
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	server.StartTLS()
	defer server.Close()
	previous := transports
	transports = newTransportCache()
	t.Cleanup(func() { transports = previous })
	c := httpCollector(t, func(c *model.Collector) {
		c.Request.AllowedSchemes = []string{"https"}
		c.Request.TLS.InsecureSkipVerify = true
		c.Request.EnableHTTP2 = true
		c.Request.Retry.Attempts = 2
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	type answer struct {
		resp *HTTPResponse
		err  error
	}
	healthy := make(chan answer, 1)
	go func() {
		resp, err := FetchCollector(ctx, server.URL+"/healthy", c, RequestOverrides{}, nil)
		healthy <- answer{resp, err}
	}()
	select {
	case <-waiting:
	case <-ctx.Done():
		t.Fatal("the healthy request never reached the target")
	}
	// The faulty request is asked once, so one connection is closed, once.
	none := 0
	if _, err := FetchCollector(ctx, server.URL+"/arriving", c, RequestOverrides{RetryAttempts: &none}, nil); !http2ProtocolError(err) {
		t.Fatalf("the request with oversized headers: err=%v, want the HTTP/2 protocol error", err)
	}
	close(release)
	got := <-healthy
	if got.err != nil || string(got.resp.Body) != "ok" {
		t.Fatalf("the healthy request on the closed connection: err=%v, want its answer from a retry", got.err)
	}
	if asked, faulty, opened := healthyAsked.Load(), faultyAsked.Load(), connections.Load(); asked != 2 || faulty != 1 || opened != 2 {
		t.Fatalf("the healthy request was asked %d times, the faulty one %d times, on %d connections in all; want 2, 1 and 2: the one the two shared, and a new one for the one retry", asked, faulty, opened)
	}
}

// The request whose headers closed the connection is retried too, at the same
// moment as the healthy one beside it, and the two retries met again on the
// one new connection: it was closed for the same headers, the healthy
// request drew its second protocol error and ended there with retries left.
// Each retry is now on a connection of its own, so the healthy request is
// answered whatever the other does, and the faulty one closes only its own.
func TestAHealthyRequestIsAnsweredWhenTheRequestThatClosedItsHTTP2ConnectionIsRetriedToo(t *testing.T) {
	var healthyAsked, faultyAsked, connections atomic.Int64
	waiting := make(chan struct{}, 1)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/arriving" {
			faultyAsked.Add(1)
			for i := range 150 {
				w.Header().Set("X-Pad-"+string(rune('a'+i%26))+strings.Repeat("x", i/26), strings.Repeat("a", 8000))
			}
			_, _ = w.Write([]byte("too much"))
			return
		}
		// The first answer is held until the connection is closed under
		// it; the retry's takes long enough for the faulty request's
		// retry to arrive beside it, were the two on one connection.
		if healthyAsked.Add(1) == 1 {
			waiting <- struct{}{}
			<-r.Context().Done()
			return
		}
		time.Sleep(100 * time.Millisecond)
		_, _ = w.Write([]byte("ok"))
	}))
	server.EnableHTTP2 = true
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	server.StartTLS()
	defer server.Close()
	previous := transports
	transports = newTransportCache()
	t.Cleanup(func() { transports = previous })
	c := httpCollector(t, func(c *model.Collector) {
		c.Request.AllowedSchemes = []string{"https"}
		c.Request.TLS.InsecureSkipVerify = true
		c.Request.EnableHTTP2 = true
		c.Request.Retry.Attempts = 3
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	type answer struct {
		resp *HTTPResponse
		err  error
	}
	healthy := make(chan answer, 1)
	go func() {
		resp, err := FetchCollector(ctx, server.URL+"/healthy", c, RequestOverrides{}, nil)
		healthy <- answer{resp, err}
	}()
	select {
	case <-waiting:
	case <-ctx.Done():
		t.Fatal("the healthy request never reached the target")
	}
	// The faulty request has the collector's retries as well.
	if _, err := FetchCollector(ctx, server.URL+"/arriving", c, RequestOverrides{}, nil); !http2ProtocolError(err) {
		t.Fatalf("the request with oversized headers: err=%v, want the HTTP/2 protocol error", err)
	}
	got := <-healthy
	if got.err != nil || string(got.resp.Body) != "ok" {
		t.Fatalf("the healthy request beside a faulty one that is retried too: err=%v, want its answer from its one retry", got.err)
	}
	if asked, faulty, opened := healthyAsked.Load(), faultyAsked.Load(), connections.Load(); asked != 2 || faulty != 2 || opened != 3 {
		t.Fatalf("the healthy request was asked %d times, the faulty one %d times, on %d connections in all; want 2, 2 and 3: the one the two shared, and one for each retry", asked, faulty, opened)
	}
	// Neither retry's connection is kept: the next request is on a
	// connection of the pool, and the one after it on that one again.
	for range 2 {
		if resp, err := FetchCollector(ctx, server.URL+"/healthy", c, RequestOverrides{}, nil); err != nil || string(resp.Body) != "ok" {
			t.Fatalf("a request after the retries: err=%v", err)
		}
	}
	if opened := connections.Load(); opened != 4 {
		t.Fatalf("%d connections after two more requests, want 4: one more, which the pool keeps", opened)
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
