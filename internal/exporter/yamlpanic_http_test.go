//go:build !select_request_types || request_type_http

package exporter

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// A document the YAML library panics on fails the probe at its decode stage,
// as any document that cannot be decoded does: a 502 that names the stage
// and gives the library's words, one line in the log and no stack. It was
// an internal error, answered 500, with the stack logged on every scrape.
func TestAYAMLDocumentTheLibraryPanicsOnFailsTheDecodeStage(t *testing.T) {
	testutil.CaptureLogs(t)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/yaml")
		_, _ = w.Write([]byte("used: 5\n2: 3\n<<: {[x]: 1}\n"))
	}))
	t.Cleanup(target.Close)
	cfg, err := config.Load(testutil.WriteIn(t, t.TempDir(), "config.yaml", yamlFailureCollectors))
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(config.NewManager(cfg, "", slog.Default()), "python3", slog.Default())
	logs := &bytes.Buffer{}
	server.logger = slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	const want = "YAML decode: the YAML library failed on the document: runtime error: hash of unhashable type"
	for scrape := range 2 {
		answer := probeOnce(t, server, probePath("document", target.URL, ""), nil)
		if answer.Code != http.StatusBadGateway || !strings.Contains(answer.Body.String(), want) {
			t.Fatalf("answered %d %q, want 502 saying %q", answer.Code, answer.Body, want)
		}
		if strings.Contains(logs.String(), "goroutine ") || strings.Contains(logs.String(), "panicked") {
			t.Fatalf("the log has a panic or a stack:\n%s", logs)
		}
		lines := linesOf(t, logs, "probe failed")
		if len(lines) != 1 || lines[0]["stage"] != "decode" {
			t.Fatalf("the failure is logged as %v, want one line of the decode stage", lines)
		}
		if text, _ := lines[0]["error"].(string); !strings.HasPrefix(text, want) {
			t.Errorf("the line's error is %q, want it to start with %q", text, want)
		}
		if scrape == 1 && (lines[0]["level"] != "DEBUG" || lines[0]["repeat"] != true) {
			t.Errorf("the second scrape is logged as %v, want as a repeat at debug level", lines[0])
		}
	}
}
