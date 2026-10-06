//go:build !select_request_types || request_type_http

package exporter

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// A probe, the self-metrics and the static targets endpoint answer in the
// format asked for, and say so, and vary on Accept.
func TestEndpointsAnswerInTheFormatAskedFor(t *testing.T) {
	testutil.CaptureLogs(t)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("value=42\n"))
	}))
	t.Cleanup(target.Close)
	server := flightServer(t, testutil.Collector("om", "text"))
	for _, path := range []string{probePath("om", target.URL, ""), "/self-metrics"} {
		for _, tc := range []struct {
			accept, contentType, ending string
		}{
			{"", expositionContentType, "\n"},
			{prometheus3Accept, openMetricsType1, "# EOF\n"},
		} {
			request := httptest.NewRequest(http.MethodGet, path, nil)
			if tc.accept != "" {
				request.Header.Set("Accept", tc.accept)
			}
			recorder := httptest.NewRecorder()
			server.Handler().ServeHTTP(recorder, request)
			body := recorder.Body.String()
			if recorder.Code != http.StatusOK || recorder.Header().Get("Content-Type") != tc.contentType || !strings.HasSuffix(body, tc.ending) {
				t.Fatalf("%s with Accept %q: %d %q\n%s", path, tc.accept, recorder.Code, recorder.Header().Get("Content-Type"), body)
			}
			if !slices.Contains(recorder.Header().Values("Vary"), "Accept") || recorder.Header().Get("X-Content-Type-Options") != "nosniff" {
				t.Fatalf("%s: headers %v", path, recorder.Header())
			}
			if tc.accept == "" && strings.Contains(body, "# EOF") {
				t.Fatalf("%s: the text format ended with # EOF", path)
			}
		}
	}
}

// Probes sharing one trip to the target each get the format they asked for.
func TestSharedProbesEachGetTheirOwnFormat(t *testing.T) {
	testutil.CaptureLogs(t)
	target := newGatedTarget(t, http.StatusOK, "value=42\n")
	server := flightServer(t, testutil.Collector("shared", "text"))
	path := probePath("shared", target.URL, "")
	text := probeAsync(context.Background(), server, path, nil)
	om := probeAsync(context.Background(), server, path, http.Header{"Accept": {prometheus2Accept}})
	waitForWaiters(t, server, 2)
	target.open()
	if got := <-text; got.code != http.StatusOK || strings.Contains(got.body, "# EOF") || !strings.Contains(got.body, "demo_value 42\n") {
		t.Fatalf("the text probe got %d:\n%s", got.code, got.body)
	}
	if got := <-om; got.code != http.StatusOK || !strings.HasSuffix(got.body, "# EOF\n") || !strings.Contains(got.body, "demo_value 42\n") {
		t.Fatalf("the OpenMetrics probe got %d:\n%s", got.code, got.body)
	}
	if n := target.requests.Load(); n != 1 {
		t.Fatalf("the target was asked %d times, want once", n)
	}
}

// The static targets endpoint answers in the format asked for, both from the
// rendering it keeps for unfiltered reads and for a filtered read.
func TestStaticTargetsAnswerInTheFormatAskedFor(t *testing.T) {
	up := textTarget(t, "value=42\n")
	cfg := &model.Config{Collectors: []model.Collector{testutil.Collector("text", "text")}}
	file := &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{
		{Name: "eu", Collector: "text", Target: up.URL},
		{Name: "us", Collector: "text", Target: up.URL},
	}}
	server := newStaticServer(t, cfg, file)
	server.logger = testutil.QuietLogger(t)
	server.scrapeStaticTargets(context.Background(), 0)
	for _, path := range []string{"/static-targets", "/static-targets?targets=eu"} {
		for _, accept := range []string{"", prometheus2Accept} {
			request := httptest.NewRequest(http.MethodGet, path, nil)
			if accept != "" {
				request.Header.Set("Accept", accept)
			}
			recorder := httptest.NewRecorder()
			server.Handler().ServeHTTP(recorder, request)
			body := recorder.Body.String()
			om := accept != ""
			if recorder.Code != http.StatusOK || strings.HasSuffix(body, "# EOF\n") != om || !strings.Contains(body, `demo_value{static_target="eu"} 42`) {
				t.Fatalf("%s with Accept %q: %d\n%s", path, accept, recorder.Code, body)
			}
			if want := map[bool]string{false: expositionContentType, true: openMetricsType1}[om]; recorder.Header().Get("Content-Type") != want {
				t.Fatalf("%s with Accept %q: Content-Type %q", path, accept, recorder.Header().Get("Content-Type"))
			}
		}
	}
}
