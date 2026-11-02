//go:build !select_request_types || request_type_http

package fetch

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// traceHeaders is a target that keeps the traceparent and tracestate of
// each request by its path; /login answers a JSON login's token, and /hop
// redirects to /data.
type traceHeaders struct {
	*httptest.Server
	mu   sync.Mutex
	sent map[string][2]string
}

func newTraceHeaders(t *testing.T) *traceHeaders {
	t.Helper()
	h := &traceHeaders{sent: map[string][2]string{}}
	h.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		h.sent[r.URL.Path] = [2]string{strings.Join(r.Header.Values("Traceparent"), "|"), strings.Join(r.Header.Values("Tracestate"), "|")}
		h.mu.Unlock()
		switch r.URL.Path {
		case "/login":
			_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "tok"})
		case "/hop":
			http.Redirect(w, r, "/data", http.StatusFound)
		default:
			_, _ = w.Write([]byte("value=1"))
		}
	}))
	t.Cleanup(h.Close)
	return h
}

func (h *traceHeaders) of(path string) (string, string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.sent[path][0], h.sent[path][1]
}

var propagatedTrace = [16]byte{0x0a, 0xf7, 0x65, 0x19, 0x16, 0xcd, 0x43, 0xdd, 0x84, 0x48, 0xeb, 0x21, 0x1c, 0x80, 0x31, 0x9c}

// A request and the redirect it follows each carry the trip's traceparent
// under a span id of their own, recorded with the request for its span,
// the sampled flag as the trip says and its tracestate; the login's request
// carries none.
func TestATripsRequestsCarryItsTraceparent(t *testing.T) {
	freshLogins(t)
	target := newTraceHeaders(t)
	c := loginCollector(t, model.LoginConfig{Type: "json", URL: "/login", Token: ".access_token", FieldFiles: secretFiles(t, map[string]string{"password": "p"})})
	c.Request.FollowRedirects = true
	ctx := WithTracePropagation(context.Background(), TracePropagation{TraceID: propagatedTrace, Sampled: true, State: "ot=th:0,vendor=x"})
	ctx, trace := WithSpanTrace(ctx)
	if _, err := FetchCollector(ctx, target.URL+"/hop", c, RequestOverrides{}, nil); err != nil {
		t.Fatal(err)
	}
	var requests []TracedRequest
	for _, r := range trace.Requests() {
		if r.Login {
			if r.SpanID != [8]byte{} {
				t.Error("the login's request was recorded with a span id")
			}
			continue
		}
		requests = append(requests, r)
	}
	if len(requests) != 2 {
		t.Fatalf("%d requests recorded besides the login", len(requests))
	}
	for i, path := range []string{"/hop", "/data"} {
		parent, state := target.of(path)
		want := "00-0af7651916cd43dd8448eb211c80319c-" + hex.EncodeToString(requests[i].SpanID[:]) + "-01"
		if requests[i].SpanID == [8]byte{} || parent != want || state != "ot=th:0,vendor=x" {
			t.Errorf("%s carries %q and %q, want %q and the trip's tracestate", path, parent, state, want)
		}
	}
	if requests[0].SpanID == requests[1].SpanID {
		t.Error("the redirect was sent under its first request's span id")
	}
	if parent, state := target.of("/login"); parent != "" || state != "" {
		t.Errorf("the login's request carries %q and %q", parent, state)
	}
}

// A caller's trace context passed on is sent as it came, and no span id is
// recorded; a traceparent the request sets itself, in request.headers or
// forwarded under any case, is sent as it is, with no tracestate added; and
// a trip without propagation sends none.
func TestAConfiguredTraceparentIsSentAsItIs(t *testing.T) {
	target := newTraceHeaders(t)
	c := testutil.Collector("api", "text")
	if err := ValidateRequest(&c); err != nil {
		t.Fatal(err)
	}
	caller := "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-00"
	ctx, trace := WithSpanTrace(WithTracePropagation(context.Background(), TracePropagation{Parent: caller, ParentState: "vendor=x"}))
	if _, err := FetchCollector(ctx, target.URL+"/data", &c, RequestOverrides{}, nil); err != nil {
		t.Fatal(err)
	}
	if parent, state := target.of("/data"); parent != caller || state != "vendor=x" || trace.Requests()[0].SpanID != [8]byte{} {
		t.Errorf("the caller's context arrives as %q and %q, span id %x", parent, state, trace.Requests()[0].SpanID)
	}
	own := WithTracePropagation(context.Background(), TracePropagation{TraceID: propagatedTrace, State: "ot=th:0"})
	configured := "00-11111111111111111111111111111111-2222222222222222-01"
	withHeader := c
	withHeader.Request.Headers = map[string]string{"traceparent": configured}
	if _, err := FetchCollector(own, target.URL+"/data", &withHeader, RequestOverrides{}, nil); err != nil {
		t.Fatal(err)
	}
	if parent, state := target.of("/data"); parent != configured || state != "" {
		t.Errorf("request.headers' traceparent arrives as %q, with tracestate %q", parent, state)
	}
	if _, err := FetchCollector(own, target.URL+"/data", &c, RequestOverrides{}, http.Header{"traceparent": {configured}}); err != nil {
		t.Fatal(err)
	}
	if parent, state := target.of("/data"); parent != configured || state != "" {
		t.Errorf("a forwarded traceparent arrives as %q, with tracestate %q", parent, state)
	}
	if _, err := FetchCollector(context.Background(), target.URL+"/data", &c, RequestOverrides{}, nil); err != nil {
		t.Fatal(err)
	}
	if parent, state := target.of("/data"); parent != "" || state != "" {
		t.Errorf("without propagation the request carries %q and %q", parent, state)
	}
}
