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
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// yamlFailureCollectors is a collector that reads a YAML document.
const yamlFailureCollectors = `
collectors:
  - name: document
    request: {type: http, path: /doc}
    decoder: {type: yaml}
    transform: {type: yq}
    metrics:
      - {name: used, expression: .used}
`

// yamlFailureTarget answers a YAML document whose line numbered *line, from
// 2, cannot be parsed, in the way *broken says: "indent" is a mapping value
// where none may be, "tab" a tab where the indentation is, and "twice" the
// document's first key written again. Line 0 answers a document that can.
type yamlFailureTarget struct {
	line   atomic.Int64
	broken atomic.Pointer[string]
}

func (y *yamlFailureTarget) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	var doc strings.Builder
	doc.WriteString("used: 5\n")
	line := int(y.line.Load())
	for i := 2; i < line; i++ {
		fmt.Fprintf(&doc, "k%d: %d\n", i, i)
	}
	if line > 0 {
		switch *y.broken.Load() {
		case "indent":
			doc.WriteString(" free: 3\n")
		case "tab":
			doc.WriteString("\t- free\n")
		case "twice":
			doc.WriteString("used: 6\n")
		}
	}
	w.Header().Set("Content-Type", "application/yaml")
	_, _ = w.Write([]byte(doc.String()))
}

// A YAML document that cannot be parsed is one failure to the log whatever
// line it fails on: with the same mistake on line 3, then 7, then 3, then 5,
// the probe's failure is logged in full once, as an error, and then as
// repeats at debug level; five minutes on the line comes again with every
// repeat counted and when the failure began; and the recovery counts every
// failing scrape. Each line, and each answer, names the line the document
// failed on in that scrape. Another mistake is another failure, logged in
// full, and so is a key written twice, whose error names two lines, on
// whichever lines the two are.
func TestAYAMLDocumentThatFailsOnAnotherLineIsOneFailureToTheLog(t *testing.T) {
	testutil.CaptureLogs(t)
	document := &yamlFailureTarget{}
	breakWith := func(how string) { document.broken.Store(&how) }
	breakWith("indent")
	target := httptest.NewServer(document)
	t.Cleanup(target.Close)
	cfg, err := config.Load(testutil.WriteIn(t, t.TempDir(), "config.yaml", yamlFailureCollectors))
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(config.NewManager(cfg, "", slog.Default()), "python3", slog.Default())
	logs := &bytes.Buffer{}
	server.logger = slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	var later atomic.Int64
	server.failures.now = func() time.Time { return time.Now().Add(time.Duration(later.Load())) }
	scrape := func(line int, want string) map[string]any {
		t.Helper()
		document.line.Store(int64(line))
		answer := probeOnce(t, server, probePath("document", target.URL, ""), nil)
		if answer.Code != http.StatusBadGateway || !strings.Contains(answer.Body.String(), want) {
			t.Fatalf("line %d: answered %d %q, want 502 saying %q", line, answer.Code, answer.Body, want)
		}
		lines := linesOf(t, logs, "probe failed")
		if len(lines) != 1 || lines[0]["stage"] != "decode" || lines[0]["error"] != want {
			t.Fatalf("line %d: the failure is logged as %v, want one line of the decode stage saying %q", line, lines, want)
		}
		return lines[0]
	}
	indent := func(line int) string {
		return fmt.Sprintf("YAML decode: yaml: line %d: mapping values are not allowed in this context", line)
	}
	for i, line := range []int{3, 3, 7, 7, 3, 5} {
		logged := scrape(line, indent(line))
		switch {
		case i == 0 && (logged["level"] != "ERROR" || logged["repeat"] != nil || logged["repeated"] != nil):
			t.Errorf("the first failure, on line %d, is logged as %v, want in full as an error", line, logged)
		case i > 0 && (logged["level"] != "DEBUG" || logged["repeat"] != true):
			t.Errorf("scrape %d, failing on line %d, is logged as %v, want as a repeat at debug level", i+1, line, logged)
		}
	}
	later.Store(int64(failureRepeatInterval + time.Second))
	if logged := scrape(9, indent(9)); logged["level"] != "ERROR" || logged["repeated"] != float64(6) || logged["failing_since"] == nil {
		t.Errorf("five minutes on, failing on line 9, the line is %v, want an error with repeated 6 and failing_since", logged)
	}
	document.line.Store(0)
	if answer := probeOnce(t, server, probePath("document", target.URL, ""), nil); answer.Code != http.StatusOK {
		t.Fatalf("with a document that can be parsed: answered %d: %s", answer.Code, answer.Body)
	}
	if lines := linesOf(t, logs, "probe recovered"); len(lines) != 1 || lines[0]["failures"] != float64(7) {
		t.Errorf("the recovery is logged as %v, want one line with failures 7", lines)
	}

	if logged := scrape(4, indent(4)); logged["level"] != "ERROR" || logged["repeat"] != nil {
		t.Errorf("the failure after the recovery is logged as %v, want in full as an error", logged)
	}
	// The library names the line before the tab's.
	tab := func(line int) string {
		return fmt.Sprintf("YAML decode: yaml: line %d: found a tab character that violates indentation", line-1)
	}
	breakWith("tab")
	if logged := scrape(4, tab(4)); logged["level"] != "ERROR" || logged["repeat"] != nil {
		t.Errorf("another mistake is logged as %v, want in full as an error", logged)
	}
	if logged := scrape(8, tab(8)); logged["level"] != "DEBUG" || logged["repeat"] != true {
		t.Errorf("the same mistake on another line is logged as %v, want as a repeat at debug level", logged)
	}
	breakWith("twice")
	if logged := scrape(3, "YAML decode: yaml: unmarshal errors:\n  line 3: mapping key \"used\" already defined at line 1"); logged["level"] != "ERROR" || logged["repeat"] != nil {
		t.Errorf("a key written twice is logged as %v, want in full as an error", logged)
	}
	if logged := scrape(6, "YAML decode: yaml: unmarshal errors:\n  line 6: mapping key \"used\" already defined at line 1"); logged["level"] != "DEBUG" || logged["repeat"] != true {
		t.Errorf("the key written again on another line is logged as %v, want as a repeat at debug level", logged)
	}
}
