//go:build !select_request_types || request_type_http

package fetch

import (
	"context"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// What is shown of a request is what was sent. The target's query reaches it
// as it was written, and a debug report's request line, the logged target
// and the error of a request that failed now show that same query, pair for
// pair, with values masked where they stand: a report used to leave out the
// pairs with a ; or a malformed escape and gave a bare key a value, and the
// logged target showed the token of a=1;token=SECRET.
func TestTheQueryShownIsTheQuerySent(t *testing.T) {
	target := newRequestRecorder(t)
	previous := transports
	transports = newTransportCache()
	t.Cleanup(func() { transports = previous })
	plain := httpCollector(t, nil)
	keyed := httpCollector(t, func(c *model.Collector) { c.Request.Query = map[string]string{"api_key": "SECRET", "view": "full"} })
	for _, tc := range []struct {
		collector               *model.Collector
		query                   string
		sent, report, displayed string
	}{
		{plain, "a=1;b=2&c=3", "a=1;b=2&c=3", "a=<redacted>;b=<redacted>&c=<redacted>", "a=1;b=2&c=3"},
		{plain, "x=100%&c=3", "x=100%&c=3", "x=<redacted>&c=<redacted>", "x=100%&c=3"},
		{plain, "debug&c=3", "debug&c=3", "debug&c=<redacted>", "debug&c=3"},
		{plain, "c=3&token=SECRET;x=1", "c=3&token=SECRET;x=1", "c=<redacted>&token=<redacted>;x=<redacted>", "c=3&token=<redacted>;x=<redacted>"},
		{plain, "a=1;token=SECRET", "a=1;token=SECRET", "a=<redacted>;token=<redacted>", "a=1;token=<redacted>"},
		{plain, "token=SE%ZZCRET", "token=SE%ZZCRET", "token=<redacted>", "token=<redacted>"},
		{plain, "TOKEN=SECRET&tenant=a", "TOKEN=SECRET&tenant=a", "TOKEN=<redacted>&tenant=<redacted>", "TOKEN=<redacted>&tenant=a"},
		{plain, "%74oken=SECRET&tenant=a", "%74oken=SECRET&tenant=a", "%74oken=<redacted>&tenant=<redacted>", "%74oken=<redacted>&tenant=a"},
		{plain, "token=SE;CRET&tenant=a", "token=SE;CRET&tenant=a", "token=<redacted>;<redacted>&tenant=<redacted>", "token=<redacted>;<redacted>&tenant=a"},
		{plain, "tenant=a&&flag&", "tenant=a&&flag&", "tenant=<redacted>&&flag&", "tenant=a&&flag&"},
		// request.query follows the target's own, encoded.
		{keyed, "a=1;b&flag", "a=1;b&flag&api_key=SECRET&view=full", "a=<redacted>;<redacted>&flag&api_key=<redacted>&view=<redacted>", "a=1;b&flag"},
	} {
		ctx, trace := WithRequestTrace(context.Background())
		written := target.URL + "/m?" + tc.query
		if _, err := FetchCollector(ctx, written, tc.collector, RequestOverrides{}, nil); err != nil {
			t.Fatalf("%q: %v", tc.query, err)
		}
		if got := target.requested(); len(got) != 1 || got[0] != "/m?"+tc.sent {
			t.Fatalf("%q: the target was asked for %q, want %q", tc.query, got, "/m?"+tc.sent)
		}
		requests := trace.Requests()
		if len(requests) != 1 {
			t.Fatalf("%q: %d requests were traced", tc.query, len(requests))
		}
		// A debug report's request line (exporter/probedebug.go).
		if got, want := RedactURLString(requests[0].URL, MaskQueryValues), target.URL+"/m?"+tc.report; got != want {
			t.Errorf("%q: a debug report shows %q, want %q", tc.query, got, want)
		}
		// The target of a log line, a label and an OTLP attribute.
		if got, want := DisplayTarget(tc.collector, written), target.URL+"/m?"+tc.displayed; got != want {
			t.Errorf("%q: the target is displayed as %q, want %q", tc.query, got, want)
		}
	}
	// The error of a request that failed quotes the URL as a report does,
	// and a credential in the userinfo or the fragment stays out of it and
	// of the displayed target.
	written := "http://user:SECRET@127.0.0.1:1/m?a=1;token=SECRET&x=100%#access_token=SECRET"
	_, err := FetchCollector(context.Background(), written, keyed, RequestOverrides{}, nil)
	if err == nil || strings.Contains(err.Error(), "SECRET") || !strings.Contains(err.Error(), `"http://redacted:redacted@127.0.0.1:1/m?a=<redacted>;token=<redacted>&x=<redacted>&api_key=<redacted>&view=<redacted>"`) {
		t.Errorf("the error of a failed request: %v", err)
	}
	if got := DisplayTarget(keyed, written); got != "http://redacted:redacted@127.0.0.1:1/m?a=1;token=<redacted>&x=100%" {
		t.Errorf("the target is displayed as %q", got)
	}
}
