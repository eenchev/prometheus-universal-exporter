//go:build !select_request_types || request_type_http

package fetch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// tripPause is how long the slow target waits before its headers and again
// before its body.
const tripPause = 60 * time.Millisecond

// The timings an answer carries are of the request that was answered,
// measured through httptrace: a new connection to an IP address over plain
// HTTP has a connection and no lookup or handshake; the next request on the
// same connection has none of the three; a name is looked up; a TLS
// target's handshake is timed; and after a redirect they are of the request
// it led to. The first byte is the target's first pause, the total the
// body's too.
func TestTimingsAreOfTheAnsweredRequest(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(tripPause)
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		time.Sleep(tripPause)
		_, _ = w.Write([]byte("done"))
	}))
	defer slow.Close()
	c := httpCollector(t, nil)
	fetchTrip := func(target string, c *model.Collector) *HTTPTrip {
		t.Helper()
		response, err := FetchCollector(context.Background(), target, c, RequestOverrides{}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if response.Trip == nil {
			t.Fatalf("%s: no trip", target)
		}
		return response.Trip
	}

	first := fetchTrip(slow.URL, c)
	if first.Connect == nil || first.DNS != nil || first.TLS != nil || first.FirstByte == nil {
		t.Fatalf("a new connection over plain HTTP to an IP address: %+v", first)
	}
	if *first.FirstByte < tripPause || first.Total-*first.FirstByte < tripPause*5/6 || *first.Connect > *first.FirstByte {
		t.Errorf("first byte %s and total %s of a target pausing %s before each", *first.FirstByte, first.Total, tripPause)
	}
	if again := fetchTrip(slow.URL, c); again.Connect != nil || again.DNS != nil || again.TLS != nil || again.FirstByte == nil || *again.FirstByte < tripPause {
		t.Errorf("a reused connection: %+v", again)
	}
	if named := fetchTrip(strings.Replace(slow.URL, "127.0.0.1", "localhost", 1), c); named.DNS == nil || named.Connect == nil {
		t.Errorf("a target by name: %+v", named)
	}

	secure := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) }))
	defer secure.Close()
	insecure := httpCollector(t, func(c *model.Collector) {
		c.Request.TLS.InsecureSkipVerify = true
		c.Request.FollowRedirects = true
	})
	if handshake := fetchTrip(secure.URL, insecure); handshake.TLS == nil || handshake.Connect == nil {
		t.Errorf("a TLS target: %+v", handshake)
	}
	// The plain HTTP request redirected has no handshake: the one it led
	// to has, on a new connection, made by a pool of its own, which another
	// server_name gives the collector.
	redirecting := httptest.NewServer(http.RedirectHandler(secure.URL+"/there", http.StatusFound))
	defer redirecting.Close()
	insecure.Request.TLS.ServerName = "example.com"
	if redirected := fetchTrip(redirecting.URL, insecure); redirected.TLS == nil || redirected.Connect == nil {
		t.Errorf("a redirect to a TLS target: %+v", redirected)
	}

	if timings := (*HTTPTrip)(nil).JQTimings(); timings != nil {
		t.Errorf("no trip: %v", timings)
	}
	if timings := first.JQTimings().(map[string]any); timings["dns"] != nil || timings["tls"] != nil || timings["total"] != first.Total.Seconds() || timings["connect"] != first.Connect.Seconds() || len(timings) != 5 {
		t.Errorf("$timings %v", timings)
	}
}
