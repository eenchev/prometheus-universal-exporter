//go:build !select_request_types || request_type_localfile

package exporter

import (
	"bytes"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// debugLogs sends a server's log to a buffer, at debug level.
func debugLogs(server *Server) *bytes.Buffer {
	logs := &bytes.Buffer{}
	server.logger = slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return logs
}

// A file whose writer has stopped is older on every scrape, and the failure
// says how old: it is one failure to the log all the same, logged in full
// once and then as a repeat, each line and each answer with the age the file
// had then.
func TestAFileOlderOnEveryScrapeIsOneFailureToTheLog(t *testing.T) {
	root := t.TempDir()
	file := testutil.WriteIn(t, root, "app.prom", promFile)
	c := fileCollector("files", root, "app.prom")
	c.Request.MaxAge = model.Duration(time.Hour)
	server := fileServer(t, c)
	logs := debugLogs(server)
	for _, hours := range []int{2, 3, 5} {
		mtime(t, file, time.Now().Add(-time.Duration(hours)*time.Hour))
		age := "was last modified " + strconv.Itoa(hours) + "h0m"
		probeFile(t, server, "collector=files").must(t, http.StatusBadGateway, age, "longer than request.max_age 1h0m0s")
		if !strings.Contains(logs.String(), age) {
			t.Errorf("the log does not say the file %s:\n%s", age, logs)
		}
	}
	text := logs.String()
	if full, repeats := strings.Count(text, `"level":"ERROR","msg":"probe failed"`), strings.Count(text, `"level":"DEBUG","msg":"probe failed"`); full != 1 || repeats != 2 || strings.Count(text, `"repeat":true`) != 2 {
		t.Fatalf("three scrapes of a file too old were logged in full %d times and as a repeat %d times, want 1 and 2:\n%s", full, repeats, text)
	}
}

// A carbon line that cannot be read and a sample line that is no part of its
// family are named by their line in the file, and the file grows: with
// another line before it the skipped line is the same failure to the log,
// logged as a repeat with the line it is on now, and another line skipped in
// its place is a new one.
func TestALineSkippedFurtherDownTheFileIsOneFailureToTheLog(t *testing.T) {
	now := strconv.FormatInt(time.Now().Unix(), 10)
	for name, test := range map[string]struct {
		file, msg                string
		first, moved, other      string
		firstErr, movedErr, next string
		collector                func(root string) model.Collector
	}{
		"carbon": {
			file: "jobs.graphite", msg: "carbon lines skipped",
			first:    "jobs.a.done 1 " + now + "\njobs.b.do\n",
			moved:    "jobs.a.done 1 " + now + "\njobs.c.done 1 " + now + "\njobs.b.do\n",
			other:    "jobs.a.done 1 " + now + "\njobs.c.done 1 " + now + "\njobs.d.do\n",
			firstErr: `carbon line 2: \"jobs.b.do\" has 1 fields`, movedErr: `carbon line 3: \"jobs.b.do\" has 1 fields`, next: `carbon line 3: \"jobs.d.do\" has 1 fields`,
			collector: func(root string) model.Collector {
				c := fileCollector("lines", root, "jobs.graphite")
				c.Transform = model.TransformConfig{Type: "jq"}
				c.Response.Graphite = model.GraphiteConfig{InvalidLines: "skip"}
				c.Metrics = []model.MetricRule{{Name: "jobs_done", Items: ".series[]", Expression: ".value", Labels: []model.LabelRule{{Name: "job", Expression: ".segments[1]"}}}}
				return c
			},
		},
		"prometheus": {
			file: "jobs.prom", msg: "sample lines left out",
			first:    "# TYPE job_seconds histogram\njob_seconds{quantile=\"0.5\"} 2\njob_seconds_sum 9\njob_seconds_count 3\n",
			moved:    "up 1\n# TYPE job_seconds histogram\njob_seconds_sum 9\njob_seconds{quantile=\"0.5\"} 2\njob_seconds_count 3\n",
			other:    "up 1\n# TYPE task_seconds histogram\ntask_seconds_sum 9\ntask_seconds{quantile=\"0.5\"} 2\ntask_seconds_count 3\n",
			firstErr: `"error":"line 2: expected job_seconds_bucket`, movedErr: `"error":"line 4: expected job_seconds_bucket`, next: `"error":"line 4: expected task_seconds_bucket`,
			collector: func(root string) model.Collector { return fileCollector("lines", root, "jobs.prom") },
		},
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			file := testutil.WriteIn(t, root, test.file, test.first)
			server := fileServer(t, test.collector(root))
			logs := debugLogs(server)
			scrape := func(body string) string {
				t.Helper()
				if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
				logs.Reset()
				probeFile(t, server, "collector=lines").must(t, http.StatusOK)
				return logs.String()
			}
			if text := scrape(test.first); !strings.Contains(text, `"level":"WARN","msg":"`+test.msg+`"`) || !strings.Contains(text, test.firstErr) {
				t.Fatalf("the first scrape is logged as\n%s", text)
			}
			if text := scrape(test.moved); !strings.Contains(text, `"level":"DEBUG","msg":"`+test.msg+`"`) || !strings.Contains(text, test.movedErr) || !strings.Contains(text, `"repeat":true`) {
				t.Fatalf("with the line further down, the scrape is logged as\n%s\nwant as a repeat at debug level", text)
			}
			if text := scrape(test.other); !strings.Contains(text, `"level":"WARN","msg":"`+test.msg+`"`) || !strings.Contains(text, test.next) || strings.Contains(text, `"repeat":true`) {
				t.Fatalf("with another line in its place, the scrape is logged as\n%s\nwant in full as a warning", text)
			}
		})
	}
}
