package exporter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The debug page's endpoint test, at --web.debug=full with otlp.enabled:
// one empty export, an ExportMetricsServiceRequest without a resource, or
// with otlp.logs an ExportLogsServiceRequest, sent to the endpoint the
// configuration in force names — never to one the visitor names — as an
// export is sent (postOTLP): with otlp.headers, otlp.tls, otlp.compression
// and otlp.timeout. What it answers is reported whole: its status, how long
// it took, an error of the connection or of TLS as Go's client gives it, the
// response's headers and the start of its body, the partialSuccess a 2xx
// carries, and what an export would make of it (otlpAnswerOf).
//
// It is one attempt, never retried, so a test takes otlp.timeout at most.
// It is no export: it counts in none of the export's self-metrics, changes
// nothing of what the status page shows of the exports or of readiness
// under otlp.unready_after_failures, and is not logged to the log export.
// One test is in progress at a time, the server's whole; another asked
// meanwhile is refused, so the page cannot be made to send the endpoint
// requests one after another as fast as it is clicked.
//
// It is asked for as a run is (debugSameOrigin): a POST carrying the
// X-Debug-Run header and a JSON body, which a page of another site cannot
// send, behind web.basic_auth.

// The bodies of an empty export of metrics and of logs.
const (
	debugOTLPEmptyMetrics = `{"resourceMetrics":[]}`
	debugOTLPEmptyLogs    = `{"resourceLogs":[]}`
)

// debugOTLPTestRequest is what the test takes: which endpoint, metrics,
// the default, or logs.
type debugOTLPTestRequest struct {
	Signal string `json:"signal"`
}

// debugOTLPTestAnswer is what the test found. Error alone is a test refused
// before anything was sent.
type debugOTLPTestAnswer struct {
	Signal   string `json:"signal,omitempty"`
	Endpoint string `json:"endpoint,omitempty"`
	// OK says the endpoint accepted the export.
	OK bool `json:"ok"`
	// Sent are the headers of the request, a credential's value withheld,
	// and Body what it carried before compression.
	Sent   []string `json:"sent,omitempty"`
	Body   string   `json:"body,omitempty"`
	Status string   `json:"status,omitempty"`
	Took   string   `json:"took,omitempty"`
	// Failure is why the request got no answer, or what the answer means
	// to an export that refuses it, in full.
	Failure string `json:"failure,omitempty"`
	// Outcome is what an export would make of the answer.
	Outcome        string               `json:"outcome,omitempty"`
	PartialSuccess *debugPartialSuccess `json:"partial_success,omitempty"`
	Headers        []string             `json:"headers,omitempty"`
	Response       string               `json:"response,omitempty"`
	Notes          []string             `json:"notes,omitempty"`
	Error          string               `json:"error,omitempty"`
}

type debugPartialSuccess struct {
	Rejected int64  `json:"rejected"`
	Message  string `json:"message,omitempty"`
}

func (s *Server) debugOTLPTestHandler(w http.ResponseWriter, r *http.Request) {
	refuse := func(code int, format string, args ...any) {
		writeDebugJSON(w, code, debugOTLPTestAnswer{Error: fmt.Sprintf(format, args...)})
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		refuse(http.StatusMethodNotAllowed, "use POST to test the OTLP endpoint")
		return
	}
	if err := debugSameOrigin(r); err != nil {
		refuse(http.StatusForbidden, "%v", err)
		return
	}
	if kind, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || kind != "application/json" {
		refuse(http.StatusUnsupportedMediaType, "an endpoint test is a JSON body, sent with Content-Type: application/json")
		return
	}
	if s.debug != DebugFull {
		refuse(http.StatusForbidden, "testing the OTLP endpoint sends it a request and shows what it answers, so it needs --web.debug=full (the chart's server.debug: full)")
		return
	}
	var req debugOTLPTestRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		refuse(http.StatusBadRequest, "the test is not the JSON the debug page sends: %v", err)
		return
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		refuse(http.StatusBadRequest, "the test is not the JSON the debug page sends: more follows the object")
		return
	}
	cfg := s.manager.Get().OTLP
	target, logs := cfg, false
	switch req.Signal {
	case "", "metrics":
		req.Signal = "metrics"
		if !cfg.Enabled || cfg.Endpoint == "" {
			refuse(http.StatusConflict, "the OTLP export is off (otlp.enabled): there is no endpoint to test")
			return
		}
	case "logs":
		if !otlpLogsOn(cfg) {
			refuse(http.StatusConflict, "the OTLP log export is off (otlp.enabled and otlp.logs.enabled): there is no logs endpoint to test")
			return
		}
		target.Endpoint, logs = cfg.Logs.Endpoint, true
	default:
		refuse(http.StatusBadRequest, "signal %q is no endpoint of the export; send metrics or logs", req.Signal)
		return
	}
	if !s.debugOTLPTesting.CompareAndSwap(false, true) {
		refuse(http.StatusTooManyRequests, "an endpoint test is in progress; try again when it has finished")
		return
	}
	defer s.debugOTLPTesting.Store(false)
	answer := s.testOTLPEndpoint(r.Context(), target, req.Signal, logs)
	s.unexportedLogger().Info("debug OTLP endpoint test served", "signal", req.Signal, "endpoint", answer.Endpoint, "ok", answer.OK)
	writeDebugJSON(w, http.StatusOK, answer)
}

// testOTLPEndpoint sends cfg.Endpoint one empty export of the signal and
// says what it answered.
func (s *Server) testOTLPEndpoint(ctx context.Context, cfg model.OTLPConfig, signal string, logs bool) debugOTLPTestAnswer {
	raw := debugOTLPEmptyMetrics
	if logs {
		raw = debugOTLPEmptyLogs
	}
	answer := debugOTLPTestAnswer{Signal: signal, Endpoint: redactedOTLPEndpoint(cfg.Endpoint), Body: raw,
		Notes: []string{"One attempt: the test does not retry, where an export retries a network error, 429, 502, 503 and 504 within its budget. It counts in none of the export's self-metrics and changes nothing the status page shows."}}
	timeout := time.Duration(cfg.Timeout)
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	sent := http.Header{"Content-Type": {"application/json"}}
	if cfg.Compression != model.OTLPCompressionNone {
		sent.Set("Content-Encoding", "gzip")
	}
	for k, v := range cfg.Headers {
		sent.Set(k, v)
	}
	answer.Sent = headerLines(sent)
	body, err := compressOTLP([]byte(raw), cfg.Compression)
	if err != nil {
		answer.Failure = err.Error()
		return answer
	}
	start := time.Now()
	resp, err := postOTLP(ctx, cfg, cfg.TLS, body, timeout)
	answer.Took = time.Since(start).Round(time.Millisecond).String()
	if err != nil {
		answer.Failure = err.Error()
		var unusable *otlpTLSError
		if errors.As(err, &unusable) {
			answer.Outcome = "not sent: otlp.tls cannot be used, and every export fails the same way"
		} else {
			answer.Outcome = "no answer: an export would retry, within its budget, and keep its points for the next export when it runs out"
		}
		return answer
	}
	answer.Status = fmt.Sprintf("%d %s", resp.status, http.StatusText(resp.status))
	answer.Headers = headerLines(resp.header)
	answer.Response = shownStart(bodyExcerpt(resp.body), debugBodyLimit)
	made, err := otlpAnswerOf(resp, logs)
	switch {
	case err == nil:
		answer.OK = true
		answer.Outcome = "accepted: an export would count this answer a success"
		if p := made.partial; p.rejected > 0 || p.message != "" {
			answer.PartialSuccess = &debugPartialSuccess{Rejected: p.rejected, Message: p.message}
			answer.Outcome += ", and drop the points its partialSuccess says it rejected"
		}
	case made.retryable:
		answer.Failure = err.Error()
		answer.Outcome = "an export would retry this answer, within its budget, and keep its points for the next export when it runs out"
	default:
		answer.Failure = err.Error()
		answer.Outcome = "refused: an export would drop the points of a request answered so, and not retry it"
	}
	return answer
}

// writeDebugJSON writes v as JSON with status code, as a run's answer is
// written.
func writeDebugJSON(w http.ResponseWriter, code int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		http.Error(w, "the answer could not be written", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(code)
	_, _ = w.Write(append(body, '\n'))
}
