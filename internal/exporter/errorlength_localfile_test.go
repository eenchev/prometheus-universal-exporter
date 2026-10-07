//go:build !select_request_types || request_type_localfile

package exporter

import (
	"bytes"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
)

// A file that cannot be read, and a file of a directory that is left out,
// fail in no more than the 2,000 bytes of any failure, in the answer, in the
// log and in what the failure log remembers. A path of three kilobytes that
// names no file is refused in an error cut at the bound, with its length,
// where the probe answered and logged the path whole. Two files of a
// directory whose every family is typed otherwise in the second than in the
// first leave the second out with the first clashes named, the rest counted
// and what a clash is said after them, in under 1,500 bytes, where the line
// named all five thousand families in 500 kB; with fewer families clashing
// it is the same failure to the log, a repeat. A file with a metric name of
// megabytes is left out with the name by its first 200 bytes and the limit
// it is over.
func TestAFilesFailureIsAnsweredLoggedAndRememberedWithinTheBound(t *testing.T) {
	root := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, name)), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	families := func(count int, kind string) string {
		var b strings.Builder
		for i := range count {
			fmt.Fprintf(&b, "# TYPE family_%d_%s %s\nfamily_%d_%s 1\n", i, strings.Repeat("n", 60), kind, i, strings.Repeat("n", 60))
		}
		return b.String()
	}
	clashing := alloctest.UnlessRaced(5000, 1000)
	write("merge/a.prom", families(clashing, "gauge"))
	write("merge/b.prom", families(clashing, "counter"))
	name := "m" + strings.Repeat("a", alloctest.UnlessRaced(5<<20, 1<<20))
	write("named/a.prom", name+" 1\n")
	single := fileCollector("single", root, "")
	merged := dirCollector("merged", filepath.Join(root, "merge"), "*.prom")
	merged.Limits = model.Limits{MaxMetrics: 100000}
	named := dirCollector("named", filepath.Join(root, "named"), "*.prom")
	server := fileServer(t, single, merged, named)
	logs := &bytes.Buffer{}
	server.logger = slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	// failure is the one line a probe logs with msg, and its error.
	failure := func(query, msg string, code int) (string, map[string]any, string) {
		t.Helper()
		logs.Reset()
		answer := probeFile(t, server, query)
		var lines []map[string]any
		for _, record := range loggedRecords(t, logs) {
			if record["msg"] == msg {
				lines = append(lines, record)
			}
		}
		if answer.code != code || len(lines) != 1 {
			t.Fatalf("%s: answered %d, %.300q, with %d lines saying %q", query, answer.code, answer.body, len(lines), msg)
		}
		text, _ := lines[0]["error"].(string)
		for _, kept := range rememberedTexts(server) {
			if len(kept) > model.MaxFailureBytes {
				t.Fatalf("%s: the failure log remembers the failure by %d bytes, %.200q", query, len(kept), kept)
			}
		}
		return text, lines[0], answer.body
	}
	const leftOut = "file of a directory failed; its series are left out and the other files' are answered"

	missing := root + strings.Repeat("/"+strings.Repeat("d", 200), 15) + "/nope.prom"
	text, line, body := failure("collector=single&target="+url.QueryEscape("file://"+missing), "probe failed", http.StatusBadGateway)
	whole := len("file " + missing + " does not exist")
	if mark := fmt.Sprintf("... (%d bytes)", whole); len(text) != model.MaxFailureBytes || !strings.HasPrefix(text, "file "+root+"/ddd") || !strings.HasSuffix(text, "ddd"+mark) || line["stage"] != "file" || body != "collector single file failed: "+text+"\n" {
		t.Errorf("a path of %d bytes that names no file fails at %v in %d bytes, %.100q ... %q, answered in %d", len(missing), line["stage"], len(text), text, text[max(0, len(text)-60):], len(body))
	}

	text, line, _ = failure("collector=merged", leftOut, http.StatusOK)
	const clash = ": a metric has one type across the directory's files"
	if len(text) > 1500 || !strings.HasPrefix(text, "family_0_"+strings.Repeat("n", 60)+" is a counter here but a gauge in a.prom; family_1_") || !strings.HasSuffix(text, clash) || !strings.Contains(text, " more"+clash) || line["stage"] != "merge" || line["repeat"] != nil {
		t.Errorf("a file clashing in %d families is left out at %v in %d bytes, %.200q ... %q", clashing, line["stage"], len(text), text, text[max(0, len(text)-120):])
	}
	write("merge/b.prom", families(clashing/2, "counter"))
	shown := strings.Count(text, " here but a ")
	if again, line, _ := failure("collector=merged", leftOut, http.StatusOK); line["repeat"] != true || line["level"] != "DEBUG" || again != strings.Replace(text, fmt.Sprintf("; and %d more", clashing-shown), fmt.Sprintf("; and %d more", clashing/2-shown), 1) || again == text {
		t.Errorf("clashing in half as many families the file is left out as a repeat %v at %v, in %.200q ... %q; want a repeat at debug level with its own count after the %d clashes named", line["repeat"], line["level"], again, again[max(0, len(again)-120):], shown)
	}

	text, line, _ = failure("collector=named", leftOut, http.StatusOK)
	if want := fmt.Sprintf(`invalid metric name "%s"... (%d bytes): longer than limits.max_metric_name_length 200`, name[:200], len(name)); text != want || line["stage"] != "validation" {
		t.Errorf("a file with a metric name of %d bytes is left out at %v in %d bytes, %.300q, want %q", len(name), line["stage"], len(text), text, want)
	}
}
