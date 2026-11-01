//go:build !select_request_types || request_type_http

package exporter

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// A debug report's request lines show the values of the query pairs the
// collector's configuration sets as its file writes them, and mask the
// rest: a credential's name, a placeholder a probe parameter filled in, a
// value a ${NAME} reference expanded to and the target's own query. The
// error a failed request quotes, which a probe's caller is told, still has
// every query value masked.
func TestADebugReportShowsTheQueryValuesTheConfigurationSets(t *testing.T) {
	var asked string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(debugExampleBody))
	}))
	defer target.Close()
	t.Setenv("PUE_TEST_REGION", "eu-west")
	query := `
      query:
        tenant: acme
        api_key: s3cret
        t: "{{param_t}}"
        region: ${PUE_TEST_REGION}`
	collector := func(name string) string {
		return strings.Replace(indented(roomsYAML(name)), "      type: http\n", "      type: http"+query+"\n", 1)
	}
	cfg, err := config.Load(testutil.WriteFile(t, "config.yaml", "collectors:\n"+collector("rooms")+collector("down")), config.WithEnvExpansion())
	if err != nil {
		t.Fatal(err)
	}
	server := debugPageServer(t, cfg)
	server.SetDebug(DebugFull)
	shown := []string{"tenant=acme"}
	masked := []string{"own=" + "<redacted>", "api_key=<redacted>", "t=<redacted>", "region=<redacted>"}
	check := func(what, line string) {
		t.Helper()
		for _, want := range append(shown, masked...) {
			if !strings.Contains(line, want) {
				t.Errorf("%s: %q has no %s", what, line, want)
			}
		}
		for _, secret := range []string{"s3cret", "eu-west", "=v1", "own=1"} {
			if strings.Contains(line, secret) {
				t.Errorf("%s: %q shows %s", what, line, secret)
			}
		}
	}

	answer := postRun(t, server, map[string]any{"configured": "rooms", "target": target.URL + "/?own=1", "params": map[string]string{"param_t": "v1"}}, http.StatusOK)
	if !answer.OK || len(answer.Requests) != 1 {
		t.Fatalf("the run: %+v", answer)
	}
	if !strings.Contains(asked, "region=eu-west") || !strings.Contains(asked, "t=v1") {
		t.Fatalf("the target was asked %q", asked)
	}
	check("the page's request", answer.Requests[0].URL)
	_, line, _ := strings.Cut(answer.Report, "1. GET ")
	check("the page's report", line)

	recorder := serve(server, httptest.NewRequest(http.MethodGet, "/probe?collector=rooms&target="+target.URL+"/%3Fown%3D1&param_t=v1&debug=true", nil))
	report := recorder.Body.String()
	if recorder.Code != http.StatusOK || !strings.Contains(report, "1. GET ") {
		t.Fatalf("the debug probe answered %d:\n%s", recorder.Code, report)
	}
	check("the debug probe's report", report[strings.Index(report, "1. GET "):strings.Index(report, "\nResponse")])

	// What a probe's caller is told of a failed request is as before.
	recorder = serve(server, httptest.NewRequest(http.MethodGet, "/probe?collector=down&target=http://127.0.0.1:1/&param_t=v1", nil))
	if told := recorder.Body.String(); recorder.Code == http.StatusOK || !strings.Contains(told, "tenant=<redacted>") || strings.Contains(told, "acme") {
		t.Errorf("a failed probe's caller was told %d %q", recorder.Code, told)
	}
}
