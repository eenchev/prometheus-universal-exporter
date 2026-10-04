//go:build !select_request_types || request_type_http

package exporter

import (
	"bytes"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// A YAML document one of whose keys is a sequence of 200,000 numbers fails
// the probe at its decode stage in a short error: the answer to the scraper
// and everything the probe logs are under a kilobyte, name the problem and
// the start of the key, and say how long what the YAML library wrote of the
// key was. They were 1.4 MB each, the key written out whole. The failure is
// recognised without the length, so the key grown shorter, to 150,000
// numbers, is a repeat, logged at debug level with its own length, and a key
// that starts otherwise is a new failure. A debug probe's report names the
// failure in the same short line.
func TestAYAMLKeyOfAnySizeFailsTheProbeInAShortError(t *testing.T) {
	testutil.CaptureLogs(t)
	var document atomic.Pointer[string]
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
	server.SetProbeDebug(true)
	logs := &bytes.Buffer{}
	server.logger = slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	// keyed is a document whose second key is the sequence of so many
	// numbers from one counted up, and the error it fails with.
	keyed := func(from, count int) (doc, said string) {
		numbers := make([]string, count)
		for i := range numbers {
			numbers[i] = strconv.Itoa(from + i)
		}
		listed := strings.Join(numbers, ", ")
		syntax := "[]interface {}{" + listed + "}"
		return "used: 5\n? [" + listed + "]\n: x\n", fmt.Sprintf("YAML decode: yaml: invalid map key: %s... (%d bytes)", syntax[:256], len(syntax))
	}
	scrape := func(from, count int) map[string]any {
		t.Helper()
		doc, said := keyed(from, count)
		document.Store(&doc)
		answer := probeOnce(t, server, probePath("document", target.URL, ""), nil)
		logged := logs.Len()
		lines := linesOf(t, logs, "probe failed")
		if want := "collector document decode failed: " + said + "\n"; answer.Code != http.StatusBadGateway || answer.Body.String() != want || answer.Body.Len() > 400 {
			t.Fatalf("a key of %d numbers: answered %d with %d bytes, %.500q, want a 502 saying %q", count, answer.Code, answer.Body.Len(), answer.Body, want)
		}
		if len(lines) != 1 || lines[0]["stage"] != "decode" || lines[0]["error"] != said || logged > 1000 {
			t.Fatalf("a key of %d numbers: logged %d bytes, %.800v, want one line of the decode stage with the error %q", count, logged, lines, said)
		}
		return lines[0]
	}
	if line := scrape(1, 200000); line["level"] != "ERROR" || line["repeat"] != nil || !strings.HasSuffix(line["error"].(string), "63... (1488909 bytes)") { //nolint:forcetypeassert // checked by scrape
		t.Errorf("a key of 200,000 numbers is logged as %v, want in full as an error, ending with its length", line)
	}
	if line := scrape(1, 150000); line["level"] != "DEBUG" || line["repeat"] != true || !strings.HasSuffix(line["error"].(string), "63... (1088909 bytes)") { //nolint:forcetypeassert // checked by scrape
		t.Errorf("the key grown shorter is logged as %v, want as a repeat at debug level, with its own length", line)
	}
	if line := scrape(2, 200000); line["level"] != "ERROR" || line["repeat"] != nil {
		t.Errorf("a key that starts otherwise is logged as %v, want in full as an error", line)
	}
	_, said := keyed(2, 200000)
	report := debugProbeGet(t, server, strings.TrimPrefix(probePath("document", target.URL, "&debug=true"), "/probe?"))
	if report.Code != http.StatusOK || !strings.Contains(report.Body.String(), said+"\n") {
		t.Fatalf("the debug probe answered %d, and its report does not name the failure as %q:\n%.3000s", report.Code, said, report.Body)
	}
	for _, line := range strings.Split(report.Body.String(), "\n") {
		if strings.Contains(line, "invalid map key") && len(line) > 500 {
			t.Errorf("the report names the failure in a line of %d bytes: %.600q", len(line), line)
		}
	}
}
