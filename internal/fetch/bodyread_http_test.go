//go:build !select_request_types || request_type_http

package fetch

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// rawTarget answers every connection with answer, byte for byte, and closes
// it: a target that says one length and sends another, which Go's own server
// cannot be made to do.
func rawTarget(t *testing.T, answer string) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				reader := bufio.NewReader(conn)
				for {
					line, err := reader.ReadString('\n')
					if err != nil {
						return
					}
					if line == "\r\n" {
						break
					}
				}
				_, _ = conn.Write([]byte(answer))
			}()
		}
	}()
	return "http://" + listener.Addr().String()
}

// lengthTarget answers with body, with its Content-Length when declared and
// in two chunks without one otherwise, and compressed when gzipped: then the
// Content-Length is that of the compressed bytes.
func lengthTarget(t *testing.T, body []byte, declared, gzipped bool) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		answer := body
		if gzipped && strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			var compressed bytes.Buffer
			gz := gzip.NewWriter(&compressed)
			_, _ = gz.Write(body)
			_ = gz.Close()
			answer = compressed.Bytes()
			w.Header().Set("Content-Encoding", "gzip")
		}
		if declared {
			w.Header().Set("Content-Length", strconv.Itoa(len(answer)))
			_, _ = w.Write(answer)
			return
		}
		_, _ = w.Write(answer[:len(answer)/2])
		w.(http.Flusher).Flush()
		_, _ = w.Write(answer[len(answer)/2:])
	}))
	t.Cleanup(server.Close)
	return server.URL
}

// An answer is read whole and held to the limit whether it declares its
// length or not: a body up to the limit is the response's, to the byte; one
// over it is refused, with its size when the answer declared it and without
// when it did not; a compressed answer is read decompressed, whatever length
// its compressed bytes declared, and held to the limit by its decompressed
// size; an empty answer is an empty body; and an answer shorter than it
// declared is a failed read, as one longer than it declared is cut at what it
// declared.
func TestAnAnswerIsReadWholeWithOrWithoutALength(t *testing.T) {
	text := []byte(strings.Repeat("value 1\n", 5000))
	large := bodyBytes()[:3<<20+17]
	for name, test := range map[string]struct {
		target   string
		limit    int64
		method   string
		want     []byte
		wantErr  string
		overflow bool
	}{
		"no length":                     {target: lengthTarget(t, text, false, false), want: text},
		"a length":                      {target: lengthTarget(t, text, true, false), want: text},
		"no length, at the limit":       {target: lengthTarget(t, text, false, false), limit: int64(len(text)), want: text},
		"a length, at the limit":        {target: lengthTarget(t, text, true, false), limit: int64(len(text)), want: text},
		"no length, over the limit":     {target: lengthTarget(t, text, false, false), limit: int64(len(text)) - 1, wantErr: "response size exceeds limit 39999", overflow: true},
		"a length, over the limit":      {target: lengthTarget(t, text, true, false), limit: int64(len(text)) - 1, wantErr: "response size 40000 exceeds limit 39999", overflow: true},
		"megabytes without a length":    {target: lengthTarget(t, large, false, false), want: large},
		"megabytes with a length":       {target: lengthTarget(t, large, true, false), want: large},
		"compressed, no length":         {target: lengthTarget(t, text, false, true), want: text},
		"compressed, a length":          {target: lengthTarget(t, text, true, true), want: text},
		"compressed, at the limit":      {target: lengthTarget(t, text, true, true), limit: int64(len(text)), want: text},
		"compressed, over the limit":    {target: lengthTarget(t, text, true, true), limit: int64(len(text)) - 1, wantErr: "response size exceeds limit 39999", overflow: true},
		"empty, no length":              {target: lengthTarget(t, nil, false, false), want: []byte{}},
		"empty, a length of 0":          {target: lengthTarget(t, nil, true, false), want: []byte{}},
		"HEAD, a length and no body":    {target: lengthTarget(t, text, true, false), method: http.MethodHead, want: []byte{}},
		"HEAD, a length over the limit": {target: lengthTarget(t, text, true, false), method: http.MethodHead, limit: 100, want: []byte{}},
		"shorter than declared":         {target: rawTarget(t, "HTTP/1.1 200 OK\r\nConnection: close\r\nContent-Length: 100\r\n\r\nvalue 1\n"), wantErr: "reading response: unexpected EOF"},
		"longer than declared":          {target: rawTarget(t, "HTTP/1.1 200 OK\r\nConnection: close\r\nContent-Length: 8\r\n\r\nvalue 1\nvalue 2\n"), want: []byte("value 1\n")},
		"no length, to the close":       {target: rawTarget(t, "HTTP/1.0 200 OK\r\n\r\nvalue 1\nvalue 2\n"), want: []byte("value 1\nvalue 2\n")},
	} {
		t.Run(name, func(t *testing.T) {
			c := httpCollector(t, func(c *model.Collector) {
				c.Request.MaxResponseBytes = model.ByteSize(test.limit)
				if test.method != "" {
					c.Request.Method = test.method
					c.Request.AcceptStatus = []string{"2xx"}
				}
			})
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			response, err := FetchCollector(ctx, test.target, c, RequestOverrides{}, nil)
			if test.wantErr != "" {
				if err == nil || err.Error() != test.wantErr || errors.Is(err, model.ErrLimitExceeded) != test.overflow {
					t.Fatalf("err=%v, want %q", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if response.Body == nil || !bytes.Equal(response.Body, test.want) {
				t.Fatalf("a body of %d bytes (nil: %v), want the %d sent", len(response.Body), response.Body == nil, len(test.want))
			}
			// A HEAD answer's length is that of a body it does not have,
			// and no buffer of that length is made for it.
			if test.method == http.MethodHead && cap(response.Body) > 512 {
				t.Fatalf("a buffer of %d bytes for a HEAD answer", cap(response.Body))
			}
		})
	}
}

// megabyteTarget answers with a megabyte, with or without its length.
func megabyteTarget(t *testing.T, declared bool) (string, []byte) {
	t.Helper()
	body := bodyBytes()[:megabyteBody]
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if declared {
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(server.Close)
	return server.URL, body
}

// Fetching a megabyte whose length the answer declares allocates little more
// than the megabyte: the request, the answer's headers and the body, once.
// Read with io.ReadAll it was 2.2 MiB. The bound is between the two, and
// holds for the whole fetch, the target's side of it included, since both run
// here. A chunked answer, whose length nothing says, is read as before, and
// whole.
func TestFetchingAMegabyteOfDeclaredLengthAllocatesItOnce(t *testing.T) {
	for name, declared := range map[string]bool{"with a length": true, "chunked": false} {
		target, body := megabyteTarget(t, declared)
		c := httpCollector(t, nil)
		size, _ := allocatedPerRun(func() {
			response, err := FetchCollector(context.Background(), target, c, RequestOverrides{}, nil)
			if err != nil || !bytes.Equal(response.Body, body) {
				t.Fatalf("%s: err=%v", name, err)
			}
		})
		if declared && size > 3<<19 {
			t.Errorf("%s: fetching %d bytes allocated %d, want little more than the body", name, len(body), size)
		}
	}
}
