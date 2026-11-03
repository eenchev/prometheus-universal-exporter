//go:build !select_request_types || request_type_http

package exporter

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// tripConfig is an uptime collector whose rules read what the request was
// besides its answer, beside $status.
const tripConfig = `collectors:
  - name: up
    request: {type: http, method: HEAD, follow_redirects: true, accept_status: [any]}
    decoder: {type: none}
    transform: {type: jq}
    metrics:
      - name: uptime_status_code
        expression: $status
      - name: uptime_first_byte_over_pause
        expression: 'if $timings.first_byte >= 0.05 then 1 else 0 end'
      - name: uptime_phase_seconds
        items: '$timings | to_entries[] | select(.value != null)'
        expression: .value
        labels:
          - name: phase
            expression: .key
`

// Through /probe, jq rules read $timings of the request a probe made: the
// first byte after the target's pause, and a series for each phase it had
// (no lookup to an IP address, no handshake over plain HTTP). A debug
// probe's report and the debug page show $timings beside the status.
func TestAProbeReadsTheTimingsOfItsRequest(t *testing.T) {
	testutil.CaptureLogs(t)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(60 * time.Millisecond)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer target.Close()
	cfg, err := config.Load(testutil.WriteFile(t, "config.yaml", tripConfig))
	if err != nil {
		t.Fatal(err)
	}
	server := flightServer(t, cfg.Collectors...)
	got := probeOnce(t, server, probePath("up", target.URL, ""), nil)
	body := got.Body.String()
	if got.Code != http.StatusOK || !strings.Contains(body, "uptime_status_code 503\n") || !strings.Contains(body, "uptime_first_byte_over_pause 1\n") {
		t.Fatalf("%d %s", got.Code, body)
	}
	phases := regexp.MustCompile(`uptime_phase_seconds\{phase="([a-z_]+)"\}`).FindAllStringSubmatch(body, -1)
	var names []string
	for _, phase := range phases {
		names = append(names, phase[1])
	}
	if strings.Join(names, ",") != "connect,first_byte,total" {
		t.Errorf("phases %v of a new connection over plain HTTP to an IP address:\n%s", names, body)
	}

	server.SetDebug(DebugFull)
	report := debugProbeGet(t, server, strings.TrimPrefix(probePath("up", target.URL, "&debug=true"), "/probe?")).Body.String()
	if !regexp.MustCompile(`(?m)^  Status 503 Service Unavailable\n  \$timings \{"connect":(null|[0-9.e-]+),"dns":null,"first_byte":[0-9.e-]+,"tls":null,"total":[0-9.e-]+\}\n`).MatchString(report) {
		t.Errorf("the debug report does not show $timings after the status:\n%s", report)
	}
	// The debug page shows them as the report does.
	answer := postRun(t, server, map[string]any{"configured": "up", "target": target.URL}, http.StatusOK)
	if answer.Response == nil || len(answer.Response.Variables) != 2 || !strings.HasPrefix(answer.Response.Variables[0], `$timings {"connect":`) || answer.Response.Variables[1] != "$tls null" {
		t.Errorf("the debug page's response: %+v", answer.Response)
	}
}

// Through /probe, rules read $tls of the target's connection: when its
// certificate expires, and that the chain was not verified under
// insecure_skip_verify. A debug probe's report shows $tls under $timings.
func TestAProbeReadsTheTLSConnectionOfItsRequest(t *testing.T) {
	testutil.CaptureLogs(t)
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer target.Close()
	cfg, err := config.Load(testutil.WriteFile(t, "config.yaml", `collectors:
  - name: up
    request: {type: http, method: HEAD, accept_status: [any], tls: {insecure_skip_verify: true}}
    decoder: {type: none}
    transform: {type: jq}
    metrics:
      - name: uptime_tls_certificate_expiry_timestamp_seconds
        expression: $tls.not_after
      - name: uptime_tls_verified
        expression: 'if $tls.verified then 1 else 0 end'
      - name: uptime_tls_version_info
        expression: "1"
        labels:
          - name: version
            expression: $tls.version
`))
	if err != nil {
		t.Fatal(err)
	}
	server := flightServer(t, cfg.Collectors...)
	got := probeOnce(t, server, probePath("up", target.URL, ""), nil)
	for _, want := range []string{"uptime_tls_certificate_expiry_timestamp_seconds " + strconv.FormatFloat(float64(target.Certificate().NotAfter.Unix()), 'g', -1, 64) + "\n", "uptime_tls_verified 0\n", `uptime_tls_version_info{version="1.3"} 1` + "\n"} {
		if got.Code != http.StatusOK || !strings.Contains(got.Body.String(), want) {
			t.Errorf("%d %s; want %q", got.Code, got.Body, want)
		}
	}
	server.SetDebug(DebugFull)
	report := debugProbeGet(t, server, strings.TrimPrefix(probePath("up", target.URL, "&debug=true"), "/probe?")).Body.String()
	if !regexp.MustCompile(`\n  \$timings \{[^\n]*\}\n  \$tls \{"cipher":"TLS_[A-Z0-9_]+","issuer_cn":"","not_after":[0-9]+,"not_before":[0-9]+,"server_name":"","subject_cn":"","verified":false,"version":"1.3"\}\n`).MatchString(report) {
		t.Errorf("the debug report does not show $tls under $timings:\n%s", report)
	}
}
