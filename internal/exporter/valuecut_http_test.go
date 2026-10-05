//go:build !select_request_types || request_type_http

package exporter

import (
	"bytes"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// valueCutCollectors read an exposition and carbon lines.
const valueCutCollectors = `
collectors:
  - name: exposition
    request: {type: http, path: /metrics}
    decoder: {type: prometheus}
    transform: {type: prometheus}
  - name: carbon
    request: {type: http, path: /carbon}
    decoder: {type: graphite}
    transform: {type: jq}
    metrics:
      - {name: carbon_value, items: ".series[]", expression: .value}
`

// An exposition with a label value of a megabyte that is never closed, and
// carbon lines one of which has a field too many and is a megabyte long,
// fail the probe at its decode stage in a short error that is whole: the
// answer to the scraper and the line the probe logs name the problem, quote
// the first 64 bytes of the value or of the line, say how long it was, and
// end as the message ends, with what is wrong and what a carbon line is to
// be, in under 400 bytes. They were the megabyte whole, and then its first
// 2,000 bytes with the end of the message cut off. The failure is recognised
// without the length and the line, so the same mistake half as long, a line
// further, is a repeat, logged at debug level with its own length and line,
// and one that starts otherwise is a new failure.
func TestALongValueFailsTheProbeInAShortErrorThatIsWhole(t *testing.T) {
	testutil.CaptureLogs(t)
	var body atomic.Pointer[string]
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(*body.Load()))
	}))
	t.Cleanup(target.Close)
	cfg, err := config.Load(testutil.WriteIn(t, t.TempDir(), "config.yaml", valueCutCollectors))
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(config.NewManager(cfg, "", slog.Default()), "python3", slog.Default())
	logs := &bytes.Buffer{}
	server.logger = slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	long := strings.Repeat("a", 1<<20)
	for collector, tc := range map[string]struct {
		// body is the response with the token in it, after so many good
		// lines, and said the error it fails with there.
		body func(token string, after int) string
		said func(token string, after int) string
	}{
		"exposition": {
			func(token string, after int) string { return strings.Repeat("ok 1\n", after) + `m{a="` + token + "\n" },
			func(token string, after int) string {
				return fmt.Sprintf(`decoding Prometheus exposition: text format parsing error in line %d: label value "%s"... (%d bytes) contains unescaped new-line`, after+1, token[:64], len(token))
			},
		},
		"carbon": {
			func(token string, after int) string {
				return strings.Repeat("ok.x 1 1\n", after) + "a.b 1 2 3 " + token + "\n"
			},
			func(token string, after int) string {
				return fmt.Sprintf(`carbon line %d: "a.b 1 2 3 %s"... (%d bytes) has 5 fields; want <path> <value> <timestamp>`, after+1, token[:54], len(token)+10)
			},
		},
	} {
		scrape := func(token string, after int) map[string]any {
			t.Helper()
			response, said := tc.body(token, after), tc.said(token, after)
			body.Store(&response)
			logs.Reset()
			answer := probeOnce(t, server, probePath(collector, target.URL, ""), nil)
			logged := logs.Len()
			lines := linesOf(t, logs, "probe failed")
			if want := "collector " + collector + " decode failed: " + said + "\n"; answer.Code != http.StatusBadGateway || answer.Body.String() != want || answer.Body.Len() > 400 {
				t.Fatalf("%s, a token of %d bytes: answered %d with %d bytes, %.500q, want a 502 saying %q", collector, len(token), answer.Code, answer.Body.Len(), answer.Body, want)
			}
			if len(lines) != 1 || lines[0]["stage"] != "decode" || lines[0]["error"] != said || logged > 1000 {
				t.Fatalf("%s, a token of %d bytes: logged %d bytes, %.800v, want one line of the decode stage with the error %q", collector, len(token), logged, lines, said)
			}
			return lines[0]
		}
		if line := scrape(long, 1); line["level"] != "ERROR" || line["repeat"] != nil {
			t.Errorf("%s: a token of a megabyte is logged as %v, want in full as an error", collector, line)
		}
		if line := scrape(long[:1<<19], 3); line["level"] != "DEBUG" || line["repeat"] != true {
			t.Errorf("%s: the token half as long, two lines further, is logged as %v, want as a repeat at debug level", collector, line)
		}
		if line := scrape("b"+long[1:], 1); line["level"] != "ERROR" || line["repeat"] != nil {
			t.Errorf("%s: a token that starts otherwise is logged as %v, want in full as an error", collector, line)
		}
	}
}
