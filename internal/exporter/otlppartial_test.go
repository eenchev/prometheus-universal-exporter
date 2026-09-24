package exporter

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// What the OTLP endpoint says about an export it did not take whole: a
// partial success on a 2xx, and the explanation in the body of a refusal
// (otlp.go).

// otlpAnswering is an endpoint that answers every export with status, body
// and content type.
func otlpAnswering(t *testing.T, status int, contentType, body string) *httptest.Server {
	t.Helper()
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(endpoint.Close)
	return endpoint
}

// loggedServer is an OTLP server whose log the test reads.
func loggedServer(t *testing.T, endpoint string) (*Server, *bytes.Buffer) {
	t.Helper()
	server := otlpServer(t, endpoint)
	var logs bytes.Buffer
	server.logger = slog.New(slog.NewJSONHandler(&logs, nil))
	return server, &logs
}

// Rejected data points of an accepted export are dropped and counted, as a
// refused export's are, and the endpoint's reason is logged; the export is
// still a success, and nothing is queued again.
func TestAnOTLPPartialSuccessCountsTheRejectedPoints(t *testing.T) {
	for name, count := range map[string]string{"as a string, as OTLP/JSON writes an int64": `"2"`, "as a number": `2`} {
		t.Run(name, func(t *testing.T) {
			endpoint := otlpAnswering(t, http.StatusOK, "application/json", `{"partialSuccess":{"rejectedDataPoints":`+count+`,"errorMessage":"metric name too long"}}`)
			server, logs := loggedServer(t, endpoint.URL)
			queueProbeMetric(server, "first_value", 1)
			server.exportOTLP(context.Background(), 5*time.Second)

			exposition := selfMetrics(t, server)
			if got := seriesValue(t, exposition, "http_exporter_otlp_points_dropped_total"); got != 2 {
				t.Fatalf("dropped %v points, want the 2 the endpoint rejected", got)
			}
			if got := seriesValue(t, exposition, `http_exporter_otlp_exports_total{result="success"}`); got != 1 {
				t.Fatalf("the export was not a success: %v", got)
			}
			if _, ok := pendingValue(server, "first_value"); ok {
				t.Fatal("an accepted export was queued again")
			}
			text := logs.String()
			if !strings.Contains(text, "rejected some of its data points") || !strings.Contains(text, `"rejected_points":2`) || !strings.Contains(text, "metric name too long") {
				t.Fatalf("the partial success was not logged with its reason:\n%s", text)
			}
		})
	}
}

// A message without rejected points is a warning from the endpoint: logged,
// with nothing dropped.
func TestAnOTLPPartialSuccessWarningIsLogged(t *testing.T) {
	endpoint := otlpAnswering(t, http.StatusOK, "application/json", `{"partialSuccess":{"errorMessage":"deprecated attribute"}}`)
	server, logs := loggedServer(t, endpoint.URL)
	queueProbeMetric(server, "first_value", 1)
	server.exportOTLP(context.Background(), 5*time.Second)
	if got := seriesValue(t, selfMetrics(t, server), "http_exporter_otlp_points_dropped_total"); got != 0 {
		t.Fatalf("dropped %v points on a warning", got)
	}
	if !strings.Contains(logs.String(), "deprecated attribute") {
		t.Fatalf("the warning was not logged:\n%s", logs.String())
	}
}

// A full success, whatever form it takes, logs and drops nothing.
func TestAnOTLPFullSuccessReportsNothing(t *testing.T) {
	for name, answer := range map[string]struct{ contentType, body string }{
		"empty body":              {"", ""},
		"empty object":            {"application/json", `{}`},
		"empty partial success":   {"application/json", `{"partialSuccess":{}}`},
		"zero rejected":           {"application/json", `{"partialSuccess":{"rejectedDataPoints":"0"}}`},
		"a protobuf answer":       {"application/x-protobuf", "\x0a\x00"},
		"a body that is not JSON": {"application/json", "ok"},
	} {
		t.Run(name, func(t *testing.T) {
			endpoint := otlpAnswering(t, http.StatusOK, answer.contentType, answer.body)
			server, logs := loggedServer(t, endpoint.URL)
			queueProbeMetric(server, "first_value", 1)
			server.exportOTLP(context.Background(), 5*time.Second)
			if got := seriesValue(t, selfMetrics(t, server), "http_exporter_otlp_points_dropped_total"); got != 0 {
				t.Fatalf("dropped %v points", got)
			}
			if logs.Len() != 0 {
				t.Fatalf("a full success logged:\n%s", logs.String())
			}
		})
	}
}

// A refused export's log carries the start of the endpoint's explanation.
func TestARefusedOTLPExportLogsTheEndpointsExplanation(t *testing.T) {
	endpoint := otlpAnswering(t, http.StatusBadRequest, "application/json", `{"code":3,"message":"invalid metric type"}`)
	server, logs := loggedServer(t, endpoint.URL)
	queueProbeMetric(server, "first_value", 1)
	server.exportOTLP(context.Background(), 5*time.Second)
	text := logs.String()
	if !strings.Contains(text, "refused an export") || !strings.Contains(text, `"response_body"`) || !strings.Contains(text, "invalid metric type") {
		t.Fatalf("the refusal was not logged with the endpoint's explanation:\n%s", text)
	}
}
