//go:build !select_request_types || request_type_http

package fetch

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A redirect keeps to request.allowed_schemes as the first URL did: an
// https-only collector does not follow one to http:// and read the answer in
// plain text.
func TestARedirectKeepsToAllowedSchemes(t *testing.T) {
	var reached atomic.Bool
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached.Store(true)
		_, _ = w.Write([]byte("value 1\n"))
	}))
	defer plain.Close()
	secure := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, plain.URL+"/metrics", http.StatusFound)
	}))
	defer secure.Close()
	c := httpCollector(t, func(c *model.Collector) {
		c.Request.AllowedSchemes = []string{"https"}
		c.Request.FollowRedirects = true
		c.Request.TLS.InsecureSkipVerify = true
	})
	response, err := FetchCollector(context.Background(), secure.URL, c, RequestOverrides{}, nil)
	if err == nil || !strings.Contains(err.Error(), `redirect to http://`) || !strings.Contains(err.Error(), `scheme "http" is not allowed`) {
		t.Fatalf("err=%v", err)
	}
	if response != nil || reached.Load() {
		t.Fatalf("the plain http answer was read: response=%v reached=%v", response, reached.Load())
	}
	// A collector that allows both follows the same redirect.
	both := httpCollector(t, func(c *model.Collector) {
		c.Request.AllowedSchemes = []string{"https", "http"}
		c.Request.FollowRedirects = true
		c.Request.TLS.InsecureSkipVerify = true
	})
	if response, err := FetchCollector(context.Background(), secure.URL, both, RequestOverrides{}, nil); err != nil || string(response.Body) != "value 1\n" {
		t.Fatalf("response=%v err=%v", response, err)
	}
}

// A Content-Length over the limit refuses the answer before its body is
// read, with the size it gave; a body without one is refused once reading
// passes the limit, without a made-up size.
func TestTheResponseLimitReportsTheRealSize(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	declared := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "5000")
		if r.Method == http.MethodHead {
			return
		}
		_, _ = w.Write(bytes.Repeat([]byte("x"), 10))
		w.(http.Flusher).Flush()
		// The rest never comes until the test is over: only a fetch that
		// does not read the body returns in time.
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer declared.Close()
	c := httpCollector(t, func(c *model.Collector) { c.Request.MaxResponseBytes = 100 })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := FetchCollector(ctx, declared.URL, c, RequestOverrides{}, nil)
	if !errors.Is(err, model.ErrLimitExceeded) || !strings.Contains(err.Error(), "response size 5000 exceeds limit 100") {
		t.Fatalf("err=%v", err)
	}
	// A HEAD answer has no body, whatever size it says the body would be.
	head := httpCollector(t, func(c *model.Collector) {
		c.Request.MaxResponseBytes = 100
		c.Request.Method = http.MethodHead
		c.Request.AcceptStatus = []string{"2xx"}
	})
	if _, err := FetchCollector(ctx, declared.URL, head, RequestOverrides{}, nil); err != nil {
		t.Fatalf("HEAD: %v", err)
	}

	chunked := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		for range 50 {
			_, _ = w.Write(bytes.Repeat([]byte("y"), 100))
			w.(http.Flusher).Flush()
		}
	}))
	defer chunked.Close()
	_, err = FetchCollector(ctx, chunked.URL, c, RequestOverrides{}, nil)
	if !errors.Is(err, model.ErrLimitExceeded) || err.Error() != "response size exceeds limit 100" {
		t.Fatalf("err=%v", err)
	}
}

// gzipServer answers gzip-compressed when asked, as most servers do.
func gzipServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			_, _ = w.Write([]byte(body))
			return
		}
		w.Header().Set("Content-Encoding", "gzip")
		gz := gzip.NewWriter(w)
		_, _ = gz.Write([]byte(body))
		_ = gz.Close()
	}))
}

// Accept-Encoding is the exporter's: Go decompresses an answer only when it
// asked for the compression itself, so a collector may not set the header,
// and one forwarded from the probe, as Prometheus sends it on every scrape,
// is not sent on, and the decoder still gets the answer decompressed.
func TestAcceptEncodingIsTheExportersOwn(t *testing.T) {
	c := model.Collector{Name: "web", Request: model.RequestConfig{Type: RequestTypeHTTP, Headers: map[string]string{"accept-encoding": "gzip"}}}
	if err := ValidateRequest(&c); err == nil || !strings.Contains(err.Error(), `"accept-encoding" may not be set`) {
		t.Fatalf("err=%v", err)
	}
	if err := checkHeaderNames(map[string]string{"Accept-Encoding": "identity"}); err == nil {
		t.Fatal("a static target's Accept-Encoding was allowed")
	}

	const body = "value 42\n"
	target := gzipServer(t, body)
	defer target.Close()
	web := httpCollector(t, nil)
	forwarded := http.Header{"Accept-Encoding": {"gzip"}, "X-Tenant": {"a"}}
	response, err := FetchCollector(context.Background(), target.URL, web, RequestOverrides{}, forwarded)
	if err != nil || string(response.Body) != body {
		t.Fatalf("body=%q err=%v", response.Body, err)
	}
}
