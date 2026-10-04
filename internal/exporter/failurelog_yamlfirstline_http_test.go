//go:build !select_request_types || request_type_http

package exporter

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// A YAML document that cannot be parsed on its first line is the failure it
// is on any other line. The library writes no line for the first: the error
// reads `yaml: found character ...` there and `yaml: line 3: found character
// ...` on the third. With the same mistake on line 3, then 1, then 4, the
// probe's failure is logged in full once, as an error, and then as repeats
// at debug level, each line and each answer in the library's own words for
// that scrape, and the recovery counts all three. The line only marked, the
// first line's error was another failure than the others', logged in full
// when the mistake came to the first line and again when it left it.
func TestAYAMLDocumentThatFailsOnItsFirstLineIsTheSameFailureToTheLog(t *testing.T) {
	testutil.CaptureLogs(t)
	var body atomic.Pointer[string]
	answerWith := func(document string) { body.Store(&document) }
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/yaml")
		_, _ = w.Write([]byte(*body.Load()))
	}))
	t.Cleanup(target.Close)
	cfg, err := config.Load(testutil.WriteIn(t, t.TempDir(), "config.yaml", yamlFailureCollectors))
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(config.NewManager(cfg, "", slog.Default()), "python3", slog.Default())
	logs := &bytes.Buffer{}
	server.logger = slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	const problem = "found character that cannot start any token"
	for i, scrape := range []struct{ document, want string }{
		{"used: 5\nk: 1\nfree: @x\n", "YAML decode: yaml: line 3: " + problem},
		{"free: @x\n", "YAML decode: yaml: " + problem},
		{"used: 5\nk: 1\nj: 2\nfree: @x\n", "YAML decode: yaml: line 4: " + problem},
	} {
		answerWith(scrape.document)
		answer := probeOnce(t, server, probePath("document", target.URL, ""), nil)
		if answer.Code != http.StatusBadGateway || !strings.Contains(answer.Body.String(), scrape.want) {
			t.Fatalf("scrape %d: answered %d %q, want 502 saying %q", i+1, answer.Code, answer.Body, scrape.want)
		}
		lines := linesOf(t, logs, "probe failed")
		if len(lines) != 1 || lines[0]["stage"] != "decode" || lines[0]["error"] != scrape.want {
			t.Fatalf("scrape %d: the failure is logged as %v, want one line of the decode stage saying %q", i+1, lines, scrape.want)
		}
		switch logged := lines[0]; {
		case i == 0 && (logged["level"] != "ERROR" || logged["repeat"] != nil):
			t.Errorf("the first failure is logged as %v, want in full as an error", logged)
		case i > 0 && (logged["level"] != "DEBUG" || logged["repeat"] != true):
			t.Errorf("scrape %d, %q, is logged as %v, want as a repeat at debug level", i+1, scrape.want, logged)
		}
	}
	answerWith("used: 5\n")
	if answer := probeOnce(t, server, probePath("document", target.URL, ""), nil); answer.Code != http.StatusOK {
		t.Fatalf("with a document that can be parsed: answered %d: %s", answer.Code, answer.Body)
	}
	if lines := linesOf(t, logs, "probe recovered"); len(lines) != 1 || lines[0]["failures"] != float64(3) {
		t.Errorf("the recovery is logged as %v, want one line with failures 3", lines)
	}
}
