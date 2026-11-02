//go:build !select_request_types || request_type_http

package exporter

import (
	"bufio"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// What a span carries of the target (otlptraces.go): of a request that went
// through, nothing the target sent; of one that failed, what the log line of
// its failure says, and nothing the log does not.

// answerRaw answers req's connection with raw, an HTTP answer as it goes on
// the wire, or closes it unanswered when raw is "".
func answerRaw(t *testing.T, w http.ResponseWriter, raw string) {
	t.Helper()
	conn, buffered, err := http.NewResponseController(w).Hijack()
	if err != nil {
		t.Error(err)
		return
	}
	defer func() { _ = conn.Close() }()
	if raw != "" {
		_, _ = buffered.WriteString(raw)
		_ = buffered.Flush()
	}
}

// loggedErrors are the error attributes of the JSON lines logged.
func loggedErrors(t *testing.T, logged string) []string {
	t.Helper()
	var out []string
	scanner := bufio.NewScanner(strings.NewReader(logged))
	for scanner.Scan() {
		var line map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
			t.Fatalf("a line logged is no JSON: %v: %s", err, scanner.Text())
		}
		if text, ok := line["error"].(string); ok {
			out = append(out, text)
		}
	}
	return out
}

// spanTexts are every attribute's value and the status message of every
// span, as text.
func spanTexts(spans []receivedSpan) []string {
	var out []string
	for _, span := range spans {
		for _, value := range spanAttributes(span.otlpSpan) {
			out = append(out, value)
		}
		if span.Status != nil {
			out = append(out, span.Status.Message)
		}
	}
	return out
}

// A request that failed and was sent again, or that a request that went
// through followed, is in no line of the log, and its span says no more
// than its error.type; an HTTP answer of 400 or more says its status as the
// log says one, never the reason its status line gave; and the request the
// fetch failed on says what the log line of the probe's failure says.
func TestAFailedRequestsSpanSaysWhatTheLogSays(t *testing.T) {
	const reason = "503 SECRET-REASON-of-the-target"
	unavailable := "HTTP/1.1 " + reason + "\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"
	var went, failed atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/went":
			switch went.Add(1) {
			case 1:
				answerRaw(t, w, "")
			case 2:
				answerRaw(t, w, unavailable)
			default:
				_, _ = io.WriteString(w, "value=3\n")
			}
		case "/failed":
			// Then twice no answer, the second the one the fetch fails
			// on.
			if failed.Add(1) == 1 {
				answerRaw(t, w, unavailable)
				return
			}
			answerRaw(t, w, "")
		}
	}))
	t.Cleanup(target.Close)
	receiver := newLogReceiver(t)
	server := tracesServer(t, receiver, time.Millisecond, func(c *model.Config) {
		c.OTLP.Traces.Errors = true
		retried := testutil.Collector("retried", "text")
		retried.Request.Retry = model.RetryConfig{Attempts: 2, Backoff: model.Duration(time.Millisecond)}
		retried.Request.Retry.Statuses = []string{"503"}
		c.Collectors = append(c.Collectors, retried)
	}, nil)
	logged := &lockedBuffer{}
	server.logger = slog.New(slog.NewJSONHandler(logged, nil))
	if answer := probeOnce(t, server, "/probe?collector=retried&target="+url.QueryEscape(target.URL+"/went"), nil); answer.Code != http.StatusOK {
		t.Fatalf("the probe that went through is answered %d: %s", answer.Code, answer.Body)
	}
	if answer := probeOnce(t, server, "/probe?collector=retried&target="+url.QueryEscape(target.URL+"/failed"), nil); answer.Code == http.StatusOK {
		t.Fatal("the probe that failed is answered 200")
	}
	exportTracesNow(server)
	spans := receiver.spans(t)
	errs := loggedErrors(t, logged.String())
	if len(errs) != 1 {
		t.Fatalf("the log has the errors %q, want the failed probe's alone", errs)
	}
	type outcome struct{ status, errorType, message string }
	var wentSpans, failedSpans []outcome
	for _, span := range spans {
		if span.Kind != otlpSpanKindClient {
			continue
		}
		attrs := spanAttributes(span.otlpSpan)
		o := outcome{status: attrs["http.response.status_code"], errorType: attrs["error.type"]}
		if span.Status != nil {
			o.message = span.Status.Message
		}
		if strings.HasSuffix(attrs["url.full"], "/went") {
			wentSpans = append(wentSpans, o)
		} else {
			failedSpans = append(failedSpans, o)
		}
	}
	want := []outcome{{errorType: "request_failed"}, {status: "503", errorType: "503", message: "received HTTP status 503"}, {status: "200"}}
	if len(wentSpans) != len(want) || wentSpans[0] != want[0] || wentSpans[1] != want[1] || wentSpans[2] != want[2] {
		t.Errorf("the requests of the probe that went through are %+v, want %+v", wentSpans, want)
	}
	want = []outcome{{status: "503", errorType: "503", message: "received HTTP status 503"}, {errorType: "request_failed"}, {errorType: "request_failed", message: errs[0]}}
	if len(failedSpans) != len(want) || failedSpans[0] != want[0] || failedSpans[1] != want[1] || failedSpans[2] != want[2] {
		t.Errorf("the requests of the probe that failed are %+v, want %+v (the log's error)", failedSpans, want)
	}
	for _, text := range spanTexts(spans) {
		if strings.Contains(text, "SECRET-REASON") {
			t.Errorf("a span carries the reason the target's status line gave: %q", text)
		}
	}
}
