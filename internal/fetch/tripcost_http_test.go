//go:build !select_request_types || request_type_http

package fetch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
)

// A collector none of whose rules reads $timings (TimingsUnread) sends its
// requests untimed, with no trace of their phases, which spares every
// request the trace's allocations; its answer still has $tls, $url and
// $redirects, and a total. A debug run of it times them, since its report
// shows them, and a collector whose rules read them is timed as before.
func TestARequestIsTimedOnlyWhereSomethingReadsItsTimings(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("value 1\n")) }))
	defer server.Close()
	fresh := true
	newConnection := RequestOverrides{NewConnection: &fresh}
	unread := httpCollector(t, func(c *model.Collector) { c.TimingsUnread = true })
	read := httpCollector(t, nil)
	tripOf := func(ctx context.Context, c *model.Collector) *HTTPTrip {
		t.Helper()
		response, err := FetchCollector(ctx, server.URL+"/?token=s3cret", c, newConnection, nil)
		if err != nil {
			t.Fatal(err)
		}
		return response.Trip
	}
	if trip := tripOf(context.Background(), unread); trip.Connect != nil || trip.FirstByte != nil || trip.Total <= 0 || trip.ShownURL() != server.URL+"/?token=<redacted>" || trip.Redirects != 0 {
		t.Errorf("a collector whose rules read no timings: connect %s, first_byte %s, total %s, url %q", durationOf(trip.Connect), durationOf(trip.FirstByte), trip.Total, trip.ShownURL())
	}
	debug, _ := WithRequestTrace(context.Background())
	if trip := tripOf(debug, unread); trip.Connect == nil || trip.FirstByte == nil {
		t.Errorf("a debug run of a collector whose rules read no timings: connect %s, first_byte %s", durationOf(trip.Connect), durationOf(trip.FirstByte))
	}
	if trip := tripOf(context.Background(), read); trip.Connect == nil || trip.FirstByte == nil {
		t.Errorf("a collector whose rules read the timings: connect %s, first_byte %s", durationOf(trip.Connect), durationOf(trip.FirstByte))
	}

	// On a kept connection, as most requests are sent, the untimed request
	// makes the trace's allocations fewer by the hooks and the context
	// they are carried in, and the URL's redaction, which is made only when
	// it is read. The race detector allocates for what it watches, so the
	// count is compared without it.
	if alloctest.RaceDetector {
		return
	}
	allocations := func(c *model.Collector) float64 {
		perRun, _ := alloctest.Allocations(50, func() {
			if _, err := FetchCollector(context.Background(), server.URL, c, RequestOverrides{}, nil); err != nil {
				t.Fatal(err)
			}
		})
		return perRun
	}
	timed, untimed := allocations(read), allocations(unread)
	if untimed > timed-12 {
		t.Errorf("an untimed request makes %.0f allocations, a timed one %.0f: want at least 12 fewer", untimed, timed)
	}
}

// BenchmarkAnHTTPProbe is one request of an http collector to a local
// target on a kept connection, its answer read: timed, as for a collector
// whose rules read $timings, and untimed, as for one whose rules do not.
func BenchmarkAnHTTPProbe(b *testing.B) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("value 1\n")) }))
	defer server.Close()
	for _, unread := range []bool{false, true} {
		name := "timed"
		if unread {
			name = "untimed"
		}
		b.Run(name, func(b *testing.B) {
			c := model.Collector{Name: "web", Request: model.RequestConfig{Type: RequestTypeHTTP}, TimingsUnread: unread}
			if err := ValidateRequest(&c); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			for b.Loop() {
				if _, err := FetchCollector(context.Background(), server.URL, &c, RequestOverrides{}, nil); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
