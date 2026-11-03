//go:build !select_request_types || request_type_http

package exporter

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// decoder.type none with method HEAD and accept_status [any] is the uptime
// check that read an empty or HTML body as YAML: jq rules read null, and
// $status and $headers, whatever the target answers, an empty 204, a 503
// and an HTML page among them, and a GET of a body that is no document fails
// nothing. A Python script's data is None, and response.body the body as it
// was sent.
func TestDecoderNoneReadsTheStatusAndHeadersOfAnyAnswer(t *testing.T) {
	testutil.CaptureLogs(t)
	status, body := http.StatusOK, ""
	var methods []string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method)
		w.Header().Set("X-Version", "1.2")
		w.Header().Set("Content-Type", "text/html; charset=no-such-charset")
		w.WriteHeader(status)
		_, _ = fmt.Fprint(w, body)
	}))
	defer target.Close()
	cfg, err := config.Load(testutil.WriteFile(t, "config.yaml", `collectors:
  - name: up
    request: {type: http, method: HEAD, accept_status: [any]}
    decoder: {type: none}
    transform: {type: jq}
    metrics:
      - name: uptime_up
        expression: 'if $status >= 200 and $status < 300 then 1 else 0 end'
      - name: uptime_status_code
        expression: $status
      - name: uptime_document_is_null
        expression: 'if . == null then 1 else 0 end'
      - name: uptime_version
        expression: '$headers["x-version"] | tonumber'
  - name: get
    request: {type: http}
    decoder: {type: none}
    transform: {type: yq}
    metrics:
      - name: get_status_code
        expression: $status
`))
	if err != nil {
		t.Fatal(err)
	}
	server := flightServer(t, cfg.Collectors...)
	for _, answer := range []struct {
		status int
		body   string
		up     int
	}{{http.StatusNoContent, "", 1}, {http.StatusOK, "<html>up</html>", 1}, {http.StatusServiceUnavailable, `{"broken":`, 0}} {
		status, body, methods = answer.status, answer.body, nil
		got := probeOnce(t, server, probePath("up", target.URL, ""), nil)
		for _, want := range []string{fmt.Sprintf("uptime_up %d\n", answer.up), fmt.Sprintf("uptime_status_code %d\n", answer.status), "uptime_document_is_null 1\n", "uptime_version 1.2\n"} {
			if got.Code != http.StatusOK || !strings.Contains(got.Body.String(), want) {
				t.Errorf("a %d answer: %d %s; want %q", answer.status, got.Code, got.Body, want)
			}
		}
		if len(methods) != 1 || methods[0] != http.MethodHead {
			t.Errorf("a %d answer: the target was sent %v", answer.status, methods)
		}
	}
	status, body = http.StatusOK, `{"broken": [`
	if got := probeOnce(t, server, probePath("get", target.URL, ""), nil); got.Code != http.StatusOK || !strings.Contains(got.Body.String(), "get_status_code 200\n") {
		t.Errorf("a GET of a broken document: %d %s", got.Code, got.Body)
	}

	requirePython(t)
	python, err := config.Load(testutil.WriteFile(t, "config.yaml", `collectors:
  - name: script
    request: {type: http}
    decoder: {type: none}
    transform:
      type: python
      script: |
        metric("script_data_is_none", value=1 if data is None else 0)
        metric("script_body_length", value=len(response.body))
        metric("script_status_code", value=response.status_code)
    metrics: []
`))
	if err != nil {
		t.Fatal(err)
	}
	body = "<p>caf\xc3\xa9</p>"
	got := probeOnce(t, flightServer(t, python.Collectors...), probePath("script", target.URL, ""), nil)
	for _, want := range []string{"script_data_is_none 1\n", "script_body_length 11\n", "script_status_code 200\n"} {
		if got.Code != http.StatusOK || !strings.Contains(got.Body.String(), want) {
			t.Errorf("python: %d %s; want %q", got.Code, got.Body, want)
		}
	}
}
