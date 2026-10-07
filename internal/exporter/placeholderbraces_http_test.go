//go:build !select_request_types || request_type_http

package exporter

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// A brace before a placeholder is a brace (fetch/requesttemplate.go): in
// "{{{param_a}}}" the first brace is the value's own, the two after it
// open the placeholder, and the value is `{`, the parameter and `}`.

// A label value with a placeholder directly after a brace is filled: it
// was exported as it is written, "{{{param_a}}}-x", by a collector that
// answered 400 "not used" to the probe that gave the parameter. A probe
// that leaves the parameter out is answered 400, as the placeholder has no
// default, and gets no series with the placeholder as text.
func TestALabelsPlaceholderAfterABraceIsFilled(t *testing.T) {
	target := newLabelTarget(t)
	server := labelServer(t, `collectors:
  - name: braces
    request: {type: http, path: /json}
    transform:
      type: jq
      labels:
        set: "{{{param_a}}}-{{param_b:x}}"
    metrics:
      - name: status_up
        expression: .up
        labels:
          - {name: list, value: "[{{{param_a}}},{{{{param_b:x}}}}]"}
`)
	got := labelProbe(t, server, target, "braces", "&param_a=acme", false)
	if want := `status_up{list="[{acme},{{x}}]",set="{acme}-x"} 1`; got.Code != http.StatusOK || !slices.Equal(seriesLines(got.Body.String()), []string{want}) {
		t.Errorf("probed with param_a=acme: %d %s\nwant %s", got.Code, strings.TrimSpace(got.Body.String()), want)
	}
	got = labelProbe(t, server, target, "braces", "", false)
	if got.Code != http.StatusBadRequest || !strings.Contains(got.Body.String(), "needs param_a, which the probe did not supply and which has no default") || strings.Contains(got.Body.String(), "status_up") {
		t.Errorf("probed without param_a: %d %s\nwant 400 naming param_a, and no series", got.Code, strings.TrimSpace(got.Body.String()))
	}
}

// The same value alone in a label loads: it was refused as a placeholder
// "which is not filled in there", in a sentence that names the label values
// of transform.labels among the places that are filled. With blanks after
// the braces it is refused, as `{{ param_a }}` is.
func TestALabelThatIsAPlaceholderInBracesLoads(t *testing.T) {
	const document = `collectors:
  - name: braces
    request: {type: http, path: /json}
    transform:
      type: jq
      labels:
        set: "{{{param_a}}}"
    metrics:
      - name: status_up
        expression: .up
`
	if _, err := config.Load(testutil.WriteIn(t, t.TempDir(), "config.yaml", document)); err != nil {
		t.Errorf("the configuration is refused: %v", err)
	}
	target := newLabelTarget(t)
	got := labelProbe(t, labelServer(t, document), target, "braces", "&param_a=acme", false)
	if want := `status_up{set="{acme}"} 1`; got.Code != http.StatusOK || !slices.Equal(seriesLines(got.Body.String()), []string{want}) {
		t.Errorf("probed with param_a=acme: %d %s\nwant %s", got.Code, strings.TrimSpace(got.Body.String()), want)
	}
	_, err := config.Load(testutil.WriteIn(t, t.TempDir(), "config.yaml", strings.Replace(document, "{{{param_a}}}", "{{{ param_a }}}", 1)))
	if err == nil || !strings.Contains(err.Error(), `collector "braces" transform.labels.set has a placeholder with a space after {{; write {{param_<name>}} without spaces`) {
		t.Errorf("blanks after the braces: %v", err)
	}
}

// A header value, a query value and a body with a placeholder directly
// after a brace reach the target filled, the brace before the value and
// the one after it as they are written: each was sent as it is written,
// placeholder and all, by a collector that answered 400 "not used" to the
// probe that gave the parameter. Braces that open nothing are sent as they
// were, and a probe that leaves the parameter out is answered 400 before
// the target is contacted.
func TestARequestsPlaceholderAfterABraceIsFilled(t *testing.T) {
	var mu sync.Mutex
	var header, query, body []string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received, _ := io.ReadAll(r.Body)
		mu.Lock()
		header = append(header, r.Header.Get("X-Set"), r.Header.Get("X-Text"))
		query = append(query, r.URL.Query().Get("set"))
		body = append(body, string(received))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"up": 1}`))
	}))
	defer target.Close()
	sent := func() (headers, queries, bodies []string) {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(header), slices.Clone(query), slices.Clone(body)
	}
	server := labelServer(t, `collectors:
  - name: braces
    request:
      type: http
      method: POST
      path: /json
      headers:
        X-Set: "{{{param_x}}}"
        X-Text: "{{ {{param_x}} {{{{param_x}}}} {{{x}}}"
      query:
        set: "{{{param_x}}}"
      body: '{"a":{{{param_x|json}}},"b":{{{param_n:1|number}}},"c":{{{param_f:"f":1|raw}}}'
    transform: {type: jq}
    metrics:
      - name: status_up
        expression: .up
`)
	got := probeOnce(t, server, probePath("braces", target.URL, "&param_x="+`a%22b`), nil)
	if got.Code != http.StatusOK || !slices.Equal(seriesLines(got.Body.String()), []string{"status_up 1"}) {
		t.Fatalf("probed with param_x: %d %s", got.Code, got.Body.String())
	}
	headers, queries, bodies := sent()
	if want := []string{`{a"b}`, `{{ a"b {{a"b}} {{{x}}}`}; !slices.Equal(headers, want) {
		t.Errorf("the headers sent: %q, want %q", headers, want)
	}
	if want := []string{`{a"b}`}; !slices.Equal(queries, want) {
		t.Errorf("the query value sent: %q, want %q", queries, want)
	}
	if want := []string{`{"a":{"a\"b"},"b":{1},"c":{"f":1}`}; !slices.Equal(bodies, want) {
		t.Errorf("the body sent: %q, want %q", bodies, want)
	}
	got = probeOnce(t, server, probePath("braces", target.URL, ""), nil)
	if _, _, bodies := sent(); got.Code != http.StatusBadRequest || !strings.Contains(got.Body.String(), "needs param_x, which the probe did not supply and which has no default") || len(bodies) != 1 {
		t.Errorf("probed without param_x: %d %s, the target contacted %d times", got.Code, strings.TrimSpace(got.Body.String()), len(bodies))
	}
}

// What the exporter keeps of a value a probe gave a label, as
// docs/REQUESTS.md says under "Cardinality": a collector without a response
// cache still has, for each value, the start time of every counter exported
// over OTLP, under a key that holds the value, and an entry of the failure
// log for each value whose probe fails, which goes when that probe
// recovers. Both are bounded, the first at twice otlp.max_pending_points and
// the second at 10,000 entries, and neither is kept past an hour.
func TestWhatIsKeptOfALabelsParameterOutsideTheCache(t *testing.T) {
	target := newLabelTarget(t)
	endpoint := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(endpoint.Close)
	// No cache: cache.ttl and cache.stale_if_error are unset.
	server := labelServer(t, `otlp: {enabled: true, endpoint: `+endpoint.URL+`, max_pending_points: 1000}
collectors:
  - name: counted
    request: {type: http, path: /json}
    transform:
      type: jq
      labels:
        tenant: "{{param_tenant}}"
    metrics:
      - name: requests_total
        type: counter
        expression: .up
`)
	tenants := []string{"acme", "globex", "initech"}
	for _, tenant := range tenants {
		if got := labelProbe(t, server, target, "counted", "&param_tenant="+tenant, false); got.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", tenant, got.Code, got.Body.String())
		}
	}
	server.exportOTLP(context.Background(), time.Minute)
	server.otlpStarts.mu.Lock()
	starts, bound := 0, server.otlpStarts.max
	for key := range server.otlpStarts.series {
		if slices.ContainsFunc(tenants, func(tenant string) bool { return strings.Contains(key, "tenant="+tenant) }) {
			starts++
		}
	}
	server.otlpStarts.mu.Unlock()
	if starts != len(tenants) || bound != 2*1000 || otlpStartForget != time.Hour {
		t.Errorf("%d start times are kept for the %d tenants exported, of at most %d, for %s", starts, len(tenants), bound, otlpStartForget)
	}

	remembered := func() int {
		server.failures.mu.Lock()
		defer server.failures.mu.Unlock()
		return len(server.failures.entries)
	}
	if got := remembered(); got != 0 {
		t.Fatalf("the failure log remembers %d failures of probes that succeeded", got)
	}
	target.failing.Store(true)
	for _, tenant := range tenants[:2] {
		if got := labelProbe(t, server, target, "counted", "&param_tenant="+tenant, false); got.Code != http.StatusBadGateway {
			t.Fatalf("%s, the target failing: %d %s", tenant, got.Code, got.Body.String())
		}
	}
	if got := remembered(); got != 2 || failureLogMaxEntries != 10000 || failureLogForget != time.Hour {
		t.Errorf("the failure log remembers %d failing probes of the two that failed, of at most %d, for %s", got, failureLogMaxEntries, failureLogForget)
	}
	target.failing.Store(false)
	if got := labelProbe(t, server, target, "counted", "&param_tenant="+tenants[0], false); got.Code != http.StatusOK {
		t.Fatalf("%s, the target answering again: %d %s", tenants[0], got.Code, got.Body.String())
	}
	if got := remembered(); got != 1 {
		t.Errorf("the failure log remembers %d failing probes after one of the two recovered", got)
	}
}
