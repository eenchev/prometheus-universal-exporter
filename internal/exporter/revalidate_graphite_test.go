//go:build !select_request_types || request_type_graphite

package exporter

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// A graphite collector's render request is an http GET, and its cached
// response is revalidated as an http collector's is; a request too long for
// a URL, sent as a form with POST, is not made conditional.
func TestAGraphiteResponseIsRevalidatedWhenItIsAGet(t *testing.T) {
	var mu sync.Mutex
	var sent []string
	point := time.Now().Unix()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		sent = append(sent, r.Method+" "+r.Header.Get("If-None-Match"))
		mu.Unlock()
		if r.Header.Get("If-None-Match") == `"render-1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"render-1"`)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `[{"target": "app.web01.requests", "tags": {"name": "app.web01.requests"}, "datapoints": [[42, %d]]}]`, point)
	}))
	t.Cleanup(upstream.Close)
	long := strings.Repeat("x", 2100)
	server := graphiteExporter(t, `collectors:
  - name: short
    cache: {ttl: 1m}
    request:
      type: graphite
      targets: [app.*.requests]
    transform:
      type: jq
    metrics:
      - name: app_requests
        items: .series[]
        expression: .value
  - name: long
    cache: {ttl: 1m}
    request:
      type: graphite
      targets: [app.*.requests, "app.`+long+`"]
    transform:
      type: jq
    metrics:
      - name: app_requests
        items: .series[]
        expression: .value
`)
	for _, name := range []string{"short", "long"} {
		path := "/probe?collector=" + name + "&target=" + url.QueryEscape(upstream.URL)
		probeOnce(t, server, path, nil)
		outOfDate(server, true)
		if r := probeOnce(t, server, path, nil); r.Code != http.StatusOK || !strings.Contains(r.Body.String(), "app_requests 42\n") {
			t.Fatalf("%s answered %d: %s", name, r.Code, r.Body)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	want := []string{"GET ", `GET "render-1"`, "POST ", "POST "}
	if strings.Join(sent, "|") != strings.Join(want, "|") {
		t.Fatalf("requests %q, want %q", sent, want)
	}
}
