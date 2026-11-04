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
	if answer.Response == nil || len(answer.Response.Variables) != 6 || !strings.HasPrefix(answer.Response.Variables[0], `$timings {"connect":`) || answer.Response.Variables[1] != "$tls null" ||
		answer.Response.Variables[2] != `$url "`+target.URL+`"` || answer.Response.Variables[3] != "$redirects 0" ||
		answer.Response.Variables[4] != `$http_version "1.1"` || answer.Response.Variables[5] != `$remote_ip "127.0.0.1"` {
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
            expression: $tls.tls_version
`))
	if err != nil {
		t.Fatal(err)
	}
	server := flightServer(t, cfg.Collectors...)
	got := probeOnce(t, server, probePath("up", target.URL, ""), nil)
	for _, want := range []string{"uptime_tls_certificate_expiry_timestamp_seconds " + strconv.FormatFloat(float64(target.Certificate().NotAfter.Unix()), 'g', -1, 64) + "\n", "uptime_tls_verified 0\n", `uptime_tls_version_info{version="TLS 1.3"} 1` + "\n"} {
		if got.Code != http.StatusOK || !strings.Contains(got.Body.String(), want) {
			t.Errorf("%d %s; want %q", got.Code, got.Body, want)
		}
	}
	server.SetDebug(DebugFull)
	report := debugProbeGet(t, server, strings.TrimPrefix(probePath("up", target.URL, "&debug=true"), "/probe?")).Body.String()
	if !regexp.MustCompile(`\n  \$timings \{[^\n]*\}\n  \$tls \{"cipher_suite":"TLS_[A-Z0-9_]+","issuer_cn":"","not_after":[0-9]+,"not_before":[0-9]+,"server_name":"","subject_cn":"","tls_version":"TLS 1.3","verified":false\}\n`).MatchString(report) {
		t.Errorf("the debug report does not show $tls under $timings:\n%s", report)
	}
}

// Through /probe, rules read where the redirects led and how many there
// were: a label of $url carries no credential the target's query held; and
// the version of HTTP the answer came in and the address its connection was
// made to. A debug probe's report shows $url, $redirects, $http_version and
// $remote_ip under $tls, the URL with the query values masked as its
// request lines mask them.
func TestAProbeReadsWhereItsRedirectsLed(t *testing.T) {
	testutil.CaptureLogs(t)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			http.Redirect(w, r, "/login?next=%2F&token=abc", http.StatusFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()
	cfg, err := config.Load(testutil.WriteFile(t, "config.yaml", tripConfig+`      - name: uptime_redirects
        expression: $redirects
      - name: uptime_final_url_info
        expression: "1"
        labels:
          - name: url
            expression: $url
      - name: uptime_http_version
        expression: '$http_version | tonumber'
      - name: uptime_remote_info
        expression: "1"
        labels:
          - name: ip
            expression: $remote_ip
`))
	if err != nil {
		t.Fatal(err)
	}
	server := flightServer(t, cfg.Collectors...)
	got := probeOnce(t, server, probePath("up", target.URL, ""), nil)
	for _, want := range []string{"uptime_status_code 200\n", "uptime_redirects 1\n", `uptime_final_url_info{url="` + target.URL + `/login?next=%2F&token=<redacted>"} 1` + "\n", "uptime_http_version 1.1\n", `uptime_remote_info{ip="127.0.0.1"} 1` + "\n"} {
		if got.Code != http.StatusOK || !strings.Contains(got.Body.String(), want) {
			t.Errorf("%d %s; want %q", got.Code, got.Body, want)
		}
	}
	server.SetDebug(DebugFull)
	report := debugProbeGet(t, server, strings.TrimPrefix(probePath("up", target.URL, "&debug=true"), "/probe?")).Body.String()
	// The report masks the values of a query the configuration did not
	// set, as it does in its request lines.
	if !strings.Contains(report, "\n  $tls null\n  $url \""+target.URL+"/login?next=<redacted>&token=<redacted>\"\n  $redirects 1\n  $http_version \"1.1\"\n  $remote_ip \"127.0.0.1\"\n") {
		t.Errorf("the debug report does not show $url, $redirects, $http_version and $remote_ip under $tls:\n%s", report)
	}
}

// A session in the path and OAuth's code in the query, where a redirect
// back from a login puts them, are masked wherever the URL is shown: a
// label of $url, the recipe's start of it, a debug probe's report, its
// request lines and $url alike, and the log line of a failed probe that
// names its target.
func TestASessionInThePathAndACodeInTheQueryAreMaskedWhereverShown(t *testing.T) {
	logs := testutil.CaptureLogs(t)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/":
			http.Redirect(w, r, "/app;jsessionid=S3CRET?code=S3CRET2&state=x", http.StatusFound)
		case strings.HasPrefix(r.URL.Path, "/broken"):
			w.WriteHeader(http.StatusInternalServerError)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer target.Close()
	cfg, err := config.Load(testutil.WriteFile(t, "config.yaml", `collectors:
  - name: up
    request: {type: http, follow_redirects: true}
    decoder: {type: none}
    transform: {type: jq}
    metrics:
      - name: uptime_final_url_info
        expression: "1"
        labels:
          - name: url
            expression: $url
          - name: page
            expression: '$url | split("?")[0] | split(";")[0]'
`))
	if err != nil {
		t.Fatal(err)
	}
	server := flightServer(t, cfg.Collectors...)
	got := probeOnce(t, server, probePath("up", target.URL, ""), nil)
	want := `uptime_final_url_info{page="` + target.URL + `/app",url="` + target.URL + `/app;jsessionid=<redacted>?code=<redacted>&state=x"} 1`
	if body := got.Body.String(); got.Code != http.StatusOK || !strings.Contains(body, want) || strings.Contains(body, "S3CRET") {
		t.Errorf("%d %s\nwant %s", got.Code, body, want)
	}
	server.SetDebug(DebugFull)
	report := debugProbeGet(t, server, strings.TrimPrefix(probePath("up", target.URL, "&debug=true"), "/probe?")).Body.String()
	if strings.Contains(report, "S3CRET") || !strings.Contains(report, "\n  $url \""+target.URL+"/app;jsessionid=<redacted>?code=<redacted>&state=<redacted>\"\n") {
		t.Errorf("the debug report:\n%s", report)
	}
	failed := probeOnce(t, server, probePath("up", target.URL+"/broken;jsessionid=S3CRET?ticket=S3CRET3&lang=en", ""), nil)
	if failed.Code == http.StatusOK {
		t.Errorf("a 500 was answered %d", failed.Code)
	}
	if text := logs.String(); strings.Contains(text, "S3CRET") || !strings.Contains(text, "/broken;jsessionid=<redacted>?ticket=<redacted>&lang=en") {
		t.Errorf("the log:\n%s", text)
	}
}
