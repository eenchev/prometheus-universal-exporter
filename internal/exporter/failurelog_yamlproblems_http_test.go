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

// A YAML document with a problem in every item of a list is one failure to
// the log however many items the list has: with two items, then three, then
// twelve, each writing a key twice, the probe's failure is logged in full
// once, as an error, and then as repeats at debug level, and the recovery
// counts every one. The answer and the line name each item's problem up to
// the tenth and count the rest. An item with another problem is another
// failure, logged in full. And a document that writes one key 1200 times,
// which the YAML library lists 719,400 problems for, is answered and logged
// in under a kilobyte.
func TestAYAMLDocumentWithMoreOfTheSameProblemIsOneFailureToTheLog(t *testing.T) {
	testutil.CaptureLogs(t)
	var document atomic.Pointer[string]
	answerWith := func(doc string) { document.Store(&doc) }
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/yaml")
		_, _ = w.Write([]byte(*document.Load()))
	}))
	t.Cleanup(target.Close)
	cfg, err := config.Load(testutil.WriteIn(t, t.TempDir(), "config.yaml", yamlFailureCollectors))
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(config.NewManager(cfg, "", slog.Default()), "python3", slog.Default())
	logs := &bytes.Buffer{}
	server.logger = slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	scrape := func(doc string) (string, map[string]any) {
		t.Helper()
		answerWith(doc)
		answer := probeOnce(t, server, probePath("document", target.URL, ""), nil)
		lines := linesOf(t, logs, "probe failed")
		if answer.Code != http.StatusBadGateway || len(lines) != 1 || lines[0]["stage"] != "decode" {
			t.Fatalf("%.60q: answered %d %q and logged %v, want a 502 and one line of the decode stage", doc, answer.Code, answer.Body, lines)
		}
		said, _ := lines[0]["error"].(string)
		if want := "collector document decode failed: " + said + "\n"; answer.Body.String() != want {
			t.Fatalf("%.60q: answered %q, want %q", doc, answer.Body, want)
		}
		return said, lines[0]
	}
	items := func(n int, last string) string {
		return "used: 5\nitems:\n" + strings.Repeat("  - {a: 1, a: 2}\n", n-1) + "  - " + last + "\n"
	}
	problem := func(key string, line int) string {
		return fmt.Sprintf("\n  line %d: mapping key %q already defined at line %d", line, key, line)
	}
	const said = "YAML decode: yaml: unmarshal errors:"
	if text, logged := scrape(items(2, "{a: 1, a: 2}")); text != said+problem("a", 3)+problem("a", 4) || logged["level"] != "ERROR" || logged["repeat"] != nil {
		t.Errorf("two items are logged as %v, want in full as an error", logged)
	}
	if text, logged := scrape(items(3, "{a: 1, a: 2}")); text != said+problem("a", 3)+problem("a", 4)+problem("a", 5) || logged["level"] != "DEBUG" || logged["repeat"] != true {
		t.Errorf("three items are logged as %v, want each named, as a repeat at debug level", logged)
	}
	if text, logged := scrape(items(12, "{a: 1, a: 2}")); !strings.HasPrefix(text, said+problem("a", 3)) || !strings.HasSuffix(text, problem("a", 12)+"\n  ... and 2 more problems") || logged["level"] != "DEBUG" || logged["repeat"] != true {
		t.Errorf("twelve items are logged as %v, want ten named and two counted, as a repeat at debug level", logged)
	}
	if text, logged := scrape(items(3, "{b: 1, b: 2}")); text != said+problem("a", 3)+problem("a", 4)+problem("b", 5) || logged["level"] != "ERROR" || logged["repeat"] != nil {
		t.Errorf("an item with another problem is logged as %v, want in full as an error", logged)
	}
	if _, logged := scrape(items(5, "{b: 1, b: 2}")); logged["level"] != "DEBUG" || logged["repeat"] != true {
		t.Errorf("more items with the first problem are logged as %v, want as a repeat at debug level", logged)
	}
	text, logged := scrape(strings.Repeat("used: 5\n", 1200))
	if len(text) > 1000 || !strings.HasSuffix(text, "\n  ... and 719390 more problems") || logged["level"] != "ERROR" {
		t.Errorf("a key written 1200 times is logged in %d bytes, as %.300v", len(text), logged)
	}
	if text, logged := scrape(strings.Repeat("used: 5\n", 900)); len(text) > 1000 || logged["level"] != "DEBUG" || logged["repeat"] != true {
		t.Errorf("the key written 900 times is logged in %d bytes, as %.300v, want as a repeat at debug level", len(text), logged)
	}
	answerWith("used: 5\n")
	if answer := probeOnce(t, server, probePath("document", target.URL, ""), nil); answer.Code != http.StatusOK {
		t.Fatalf("with a document that can be parsed: answered %d: %s", answer.Code, answer.Body)
	}
	if lines := linesOf(t, logs, "probe recovered"); len(lines) != 1 || lines[0]["failures"] != float64(2) {
		t.Errorf("the recovery is logged as %v, want one line with failures 2", lines)
	}
}
