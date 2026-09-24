//go:build !select_request_types || request_type_localfile

package exporter

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// Every matching file is read, its series labelled with its name, and its
// mtime and scrape error reported; other files are not read.
func TestLocalDirectoryReadsEveryMatchingFile(t *testing.T) {
	root := t.TempDir()
	testutil.WriteIn(t, root, "a.prom", "# TYPE jobs_total counter\njobs_total{queue=\"x\"} 1\n")
	testutil.WriteIn(t, root, "b.prom", "# TYPE jobs_total counter\njobs_total{queue=\"x\"} 2\n# TYPE up_since gauge\nup_since 5\n")
	testutil.WriteIn(t, root, "notes.txt", "not metrics\n")
	testutil.WriteIn(t, root, ".c.prom", "# TYPE hidden gauge\nhidden 1\n")
	testutil.WriteIn(t, root, "sub/d.prom", "# TYPE nested gauge\nnested 1\n")
	at := time.Unix(1_700_000_000, 0)
	mtime(t, filepath.Join(root, "a.prom"), at)
	server := fileServer(t, dirCollector("dir", root, "*.prom"))
	r := probeFile(t, server, "collector=dir")
	r.must(t, http.StatusOK,
		`jobs_total{file="a.prom",queue="x"} 1`,
		`jobs_total{file="b.prom",queue="x"} 2`,
		`up_since{file="b.prom"} 5`,
		`localfile_mtime_seconds{file="a.prom"} 1.7e+09`,
		`localfile_scrape_error{file="a.prom"} 0`,
		`localfile_scrape_error{file="b.prom"} 0`,
		"localfile_files_skipped 0",
		"# HELP localfile_mtime_seconds Unix time the file was last modified.",
	)
	for _, absent := range []string{"notes.txt", "hidden", "nested", ".c.prom"} {
		if strings.Contains(r.body, absent) {
			t.Errorf("%s was read:\n%s", absent, r.body)
		}
	}
	// One family is one block, declared once.
	if strings.Count(r.body, "# TYPE jobs_total counter") != 1 {
		t.Fatalf("jobs_total is declared more than once:\n%s", r.body)
	}
	if err := parseExposition([]byte(r.body)); err != nil {
		t.Fatalf("the answer does not parse: %v\n%s", err, r.body)
	}
	// A pattern starting with a dot reads hidden files.
	server = fileServer(t, dirCollector("hidden", root, ".*.prom"))
	probeFile(t, server, "collector=hidden").must(t, http.StatusOK, `hidden{file=".c.prom"} 1`)
}

// A broken file fails alone.
func TestLocalDirectoryOneBadFileDoesNotSinkTheRest(t *testing.T) {
	root := t.TempDir()
	testutil.WriteIn(t, root, "good.prom", "# TYPE ok gauge\nok 1\n")
	testutil.WriteIn(t, root, "broken.prom", "this is { not prometheus\n")
	testutil.WriteIn(t, root, "labelled.prom", "# TYPE own gauge\nown{file=\"mine\"} 1\n")
	testutil.WriteIn(t, root, "reserved.prom", "# TYPE localfile_scrape_error gauge\nlocalfile_scrape_error 0\n")
	testutil.WriteIn(t, root, "zconflict.prom", "# TYPE ok counter\nok 2\n")
	server := fileServer(t, dirCollector("dir", root, "*.prom"))
	logs := testutil.CaptureLogs(t)
	server.logger = slog.Default()
	r := probeFile(t, server, "collector=dir")
	r.must(t, http.StatusOK,
		`ok{file="good.prom"} 1`,
		`localfile_scrape_error{file="good.prom"} 0`,
		`localfile_scrape_error{file="broken.prom"} 1`,
		`localfile_scrape_error{file="labelled.prom"} 1`,
		`localfile_scrape_error{file="reserved.prom"} 1`,
		`localfile_scrape_error{file="zconflict.prom"} 1`,
		`localfile_mtime_seconds{file="broken.prom"}`,
	)
	if strings.Contains(r.body, `ok{file="zconflict.prom"}`) || strings.Contains(r.body, "own{") {
		t.Fatalf("a failed file's series were answered:\n%s", r.body)
	}
	for _, want := range []string{`"file":"broken.prom","stage":"decode"`, `"file":"labelled.prom","stage":"labels"`, `"file":"zconflict.prom","stage":"merge"`, "ok is a counter here but a gauge in good.prom"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("the log is missing %s:\n%s", want, logs.String())
		}
	}
	exposition := selfMetrics(t, server)
	if seriesValue(t, exposition, `http_exporter_parse_errors_total{collector="dir"}`) != 1 || seriesValue(t, exposition, `http_exporter_scrape_success_total{collector="dir"}`) != 1 {
		t.Fatalf("the self-metrics do not count the file's failure and the probe's success:\n%s", exposition)
	}
}

// Any format a collector can decode can be read from a directory: text with a
// regex, JSON with jq.
func TestLocalDirectoryReadsAnyFormat(t *testing.T) {
	root := t.TempDir()
	testutil.WriteIn(t, root, "eu.txt", "value=4\n")
	testutil.WriteIn(t, root, "us.log", "value=6\n")
	testutil.WriteIn(t, root, "status.json", `{"queued": 3}`)
	testutil.WriteIn(t, root, "other.json", `{"running": 1}`)
	text := testutil.Collector("text", "text")
	text.Request = model.RequestConfig{Type: fetch.RequestTypeLocalFile, Root: root, Files: []string{"*.txt", "*.log"}}
	text.Limits = model.Limits{}
	jq := model.Collector{
		Name:      "json",
		Request:   model.RequestConfig{Type: fetch.RequestTypeLocalFile, Root: root, Files: []string{"*.json"}},
		Transform: model.TransformConfig{Type: "jq"},
		Metrics:   []model.MetricRule{{Name: "queued", Type: model.GaugeMetricType, Expression: ".queued", ErrorMode: model.ErrorModeFail}},
	}
	server := fileServer(t, text, jq)
	probeFile(t, server, "collector=text").must(t, http.StatusOK, `demo_value{file="eu.txt"} 4`, `demo_value{file="us.log"} 6`)
	// A file without the value fails alone, even with error_mode fail, which
	// fails the one file rather than the probe.
	probeFile(t, server, "collector=json").must(t, http.StatusOK, `queued{file="status.json"} 3`, `localfile_scrape_error{file="other.json"} 1`)
}

// max_files reads the first files by name and skips the rest, saying so.
func TestLocalDirectoryMaxFiles(t *testing.T) {
	root := t.TempDir()
	for i := 1; i <= 5; i++ {
		testutil.WriteIn(t, root, "f"+strconv.Itoa(i)+".prom", "# TYPE v gauge\nv "+strconv.Itoa(i)+"\n")
	}
	c := dirCollector("dir", root, "*.prom")
	c.Request.MaxFiles = 2
	server := fileServer(t, c)
	logs := testutil.CaptureLogs(t)
	server.logger = slog.Default()
	r := probeFile(t, server, "collector=dir")
	r.must(t, http.StatusOK, `v{file="f1.prom"} 1`, `v{file="f2.prom"} 2`, "localfile_files_skipped 3")
	if strings.Contains(r.body, "f3.prom") {
		t.Fatalf("a file past max_files was read:\n%s", r.body)
	}
	if !strings.Contains(logs.String(), "more matching files than request.max_files") || !strings.Contains(logs.String(), `"first_skipped":"f3.prom"`) {
		t.Fatalf("the skip was not logged:\n%s", logs.String())
	}
}

// A file over the per-file limit, or past the scrape's total, fails alone
// (fetch/localfile_reads_test.go checks it is not read).
func TestLocalDirectorySizeLimits(t *testing.T) {
	root := t.TempDir()
	small := "# TYPE v gauge\nv 1\n"
	testutil.WriteIn(t, root, "a.prom", small)
	testutil.WriteIn(t, root, "b-huge.prom", small+strings.Repeat("# padding\n", 1000))
	testutil.WriteIn(t, root, "c.prom", small)
	testutil.WriteIn(t, root, "d.prom", small)
	c := dirCollector("dir", root, "*.prom")
	c.Request.MaxResponseBytes = 1000
	c.Request.MaxTotalBytes = model.ByteSize(3*len(small) - 1)
	server := fileServer(t, c)
	logs := testutil.CaptureLogs(t)
	server.logger = slog.Default()
	r := probeFile(t, server, "collector=dir")
	r.must(t, http.StatusOK,
		`v{file="a.prom"} 1`, `v{file="c.prom"} 1`,
		`localfile_scrape_error{file="b-huge.prom"} 1`,
		`localfile_scrape_error{file="d.prom"} 1`,
		`localfile_mtime_seconds{file="b-huge.prom"}`,
	)
	for _, want := range []string{"more than the collector's limit of 1000 for one file", "past request.max_total_bytes"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("the log is missing %q:\n%s", want, logs.String())
		}
	}
	if seriesValue(t, selfMetrics(t, server), `http_exporter_series_limit_exceeded_total{collector="dir"}`) != 2 {
		t.Error("the refused files are not counted as limit errors")
	}
}

// max_age fails a stale file alone, with its mtime.
func TestLocalDirectoryMaxAge(t *testing.T) {
	root := t.TempDir()
	testutil.WriteIn(t, root, "fresh.prom", "# TYPE v gauge\nv 1\n")
	stale := testutil.WriteIn(t, root, "stale.prom", "# TYPE v gauge\nv 2\n")
	mtime(t, stale, time.Now().Add(-2*time.Hour))
	c := dirCollector("dir", root, "*.prom")
	c.Request.MaxAge = model.Duration(time.Hour)
	server := fileServer(t, c)
	r := probeFile(t, server, "collector=dir")
	r.must(t, http.StatusOK, `v{file="fresh.prom"} 1`, `localfile_scrape_error{file="stale.prom"} 1`, `localfile_mtime_seconds{file="stale.prom"}`)
	if strings.Contains(r.body, `v{file="stale.prom"}`) {
		t.Fatal("a stale file's series were answered")
	}
}

// The target names a directory under root; path does not apply; a missing
// directory or a file fails the probe as a fetch; an empty directory is an
// empty answer.
func TestLocalDirectoryTargets(t *testing.T) {
	root := t.TempDir()
	testutil.WriteIn(t, root, "a.prom", "# TYPE v gauge\nv 1\n")
	testutil.WriteIn(t, root, "batch/b.prom", "# TYPE v gauge\nv 2\n")
	if err := os.Mkdir(filepath.Join(root, "empty"), 0o700); err != nil {
		t.Fatal(err)
	}
	server := fileServer(t, dirCollector("dir", root, "*.prom"))
	probeFile(t, server, "collector=dir&target=batch").must(t, http.StatusOK, `v{file="b.prom"} 2`)
	probeFile(t, server, "collector=dir&target=empty").must(t, http.StatusOK, "localfile_files_skipped 0")
	probeFile(t, server, "collector=dir&target=missing").must(t, http.StatusBadGateway, "collector dir file failed", "missing does not exist")
	probeFile(t, server, "collector=dir&target=a.prom").must(t, http.StatusBadGateway, "is not a directory")
	probeFile(t, server, "collector=dir&target=../elsewhere").must(t, http.StatusBadRequest, "outside request.root")
	probeFile(t, server, "collector=dir&path=a.prom").must(t, http.StatusBadRequest, "probe parameter path", "reads every file of a directory")
	probeFile(t, server, "collector=dir&param_x=y").must(t, http.StatusBadRequest, "probe parameter param_x")
}

// Links are followed only inside root, and what is not a regular file fails
// alone, without hanging on a named pipe.
func TestLocalDirectoryRefusesWhatIsNotARegularFileUnderRoot(t *testing.T) {
	root := t.TempDir()
	outside := testutil.WriteIn(t, t.TempDir(), "secret.prom", "# TYPE secret gauge\nsecret 1\n")
	testutil.WriteIn(t, root, "a.prom", "# TYPE v gauge\nv 1\n")
	if err := os.Symlink("a.prom", filepath.Join(root, "inside.prom")); err != nil {
		t.Skip("no symbolic links here:", err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "outside.prom")); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if err := syscall.Mkfifo(filepath.Join(root, "pipe.prom"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	server := fileServer(t, dirCollector("dir", root, "*.prom"))
	done := make(chan *httpResult, 1)
	go func() { done <- probeFile(t, server, "collector=dir") }()
	var r *httpResult
	select {
	case r = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a named pipe in the directory hung the probe")
	}
	r.must(t, http.StatusOK, `v{file="inside.prom"} 1`, `localfile_scrape_error{file="outside.prom"} 1`)
	if runtime.GOOS != "windows" {
		r.must(t, http.StatusOK, `localfile_scrape_error{file="pipe.prom"} 1`)
	}
	if strings.Contains(r.body, "secret") {
		t.Fatalf("a file outside root was read:\n%s", r.body)
	}
}

// The verbose url label is the directory's file:// URL.
func TestLocalDirectoryVerboseLabel(t *testing.T) {
	root := t.TempDir()
	testutil.WriteIn(t, root, "a.prom", "# TYPE v gauge\nv 1\n")
	cfg := &model.Config{Collectors: []model.Collector{dirCollector("dir", root, "*.prom")}, Web: model.WebConfig{SelfMetrics: model.SelfMetricsConfig{Verbose: true}}}
	if err := config.Validate(cfg); err != nil {
		t.Fatal(err)
	}
	server := NewServer(config.NewManager(cfg, "", testutil.QuietLogger(t)), "python3", testutil.QuietLogger(t))
	probeFile(t, server, "collector=dir").must(t, http.StatusOK)
	want := `http_exporter_scrapes_total{collector="dir",http_method="READ",url="file://` + filepath.ToSlash(root) + `/"} 1`
	if exposition := selfMetrics(t, server); !strings.Contains(exposition, want) {
		t.Fatalf("missing %s in:\n%s", want, exposition)
	}
}

// A static target reads a directory too; its request may not name a path.
func TestLocalDirectoryStaticTargets(t *testing.T) {
	root := t.TempDir()
	testutil.WriteIn(t, root, "a.prom", "# TYPE v gauge\nv 1\n")
	testutil.WriteIn(t, root, "b.prom", "broken {\n")
	cfg := &model.Config{Collectors: []model.Collector{dirCollector("dir", root, "*.prom")}, OTLP: otlpConfig("http://collector.invalid/v1/metrics")}
	if err := config.Validate(cfg); err != nil {
		t.Fatal(err)
	}
	file := &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{{ExportViaOTLP: true, Name: "textfiles", Collector: "dir", Labels: map[string]string{"source": "node"}}}}
	server := newStaticServer(t, cfg, file)
	server.logger = testutil.QuietLogger(t)
	server.scrapeStaticTargets(context.Background(), 10*time.Second)
	found := map[string]float64{}
	for _, resource := range server.drainOTLP() {
		for _, m := range resource.Set.Metrics {
			found[m.Name+"/"+m.Labels["file"]+"/"+m.Labels["source"]] = m.Value
		}
	}
	for key, want := range map[string]float64{"v/a.prom/node": 1, "localfile_scrape_error/b.prom/node": 1, "localfile_scrape_error/a.prom/node": 0, "http_exporter_target_up//node": 1} {
		if got, ok := found[key]; !ok || got != want {
			t.Errorf("%s = %v (%v), want %v; got %v", key, got, ok, want, found)
		}
	}

	bad := &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{{ExportViaOTLP: true, Name: "bad", Collector: "dir", Request: model.TargetRequestConfig{Path: "a.prom", PathSet: true}}}}
	err := config.ValidateStaticTargets(bad)
	if err == nil {
		err = config.ValidateStaticTargetsAgainst(bad, cfg)
	}
	if err == nil || !strings.Contains(err.Error(), "reads every file of a directory") {
		t.Fatalf("a static target naming a path: %v", err)
	}
}

// Listing stops at its bound, saying so. With max_files 1 the bound is its
// floor of 1000 entries (fetch/localfile_directory.go).
func TestLocalDirectoryListingIsBounded(t *testing.T) {
	root := t.TempDir()
	c := dirCollector("dir", root, "*.prom")
	c.Request.MaxFiles = 1
	limit := 1000
	for i := range limit + 50 {
		testutil.WriteIn(t, root, "f"+strconv.Itoa(i)+".txt", "")
	}
	server := fileServer(t, c)
	logs := testutil.CaptureLogs(t)
	server.logger = slog.Default()
	probeFile(t, server, "collector=dir").must(t, http.StatusOK, "localfile_files_skipped 0")
	if !strings.Contains(logs.String(), "more entries than one scrape lists") || !strings.Contains(logs.String(), `"listed":`+strconv.Itoa(limit)) {
		t.Fatalf("the bounded listing was not logged:\n%s", logs.String())
	}
}

// Files are converted from response.charset, and what still is not valid
// UTF-8 is repaired, file by file.
func TestLocalDirectoryFilesInOtherEncodings(t *testing.T) {
	root := t.TempDir()
	testutil.WriteIn(t, root, "a.txt", "v=1 caf\xe9\n")
	c := regexCollector("latin")
	c.Request = model.RequestConfig{Type: fetch.RequestTypeLocalFile, Root: root, Files: []string{"*.txt"}}
	c.Response.Charset = "iso-8859-1"
	broken := regexCollector("broken")
	broken.Request = model.RequestConfig{Type: fetch.RequestTypeLocalFile, Root: root, Files: []string{"*.txt"}}
	server := fileServer(t, c, broken)
	probeFile(t, server, "collector=latin").must(t, http.StatusOK, `v{file="a.txt",who="café"} 1`)
	probeFile(t, server, "collector=broken").must(t, http.StatusOK, `v{file="a.txt",who="caf`+"�"+`"} 1`, `localfile_scrape_error{file="a.txt"} 0`)
	if seriesValue(t, selfMetrics(t, server), `http_exporter_invalid_utf8_total{collector="broken"}`) != 1 {
		t.Fatal("the repaired value was not counted")
	}
}

// A broken file of a directory logs once over many probes, and its recovery.
func TestLocalDirectoryFileFailuresLogOnce(t *testing.T) {
	root := t.TempDir()
	testutil.WriteIn(t, root, "good.prom", "# TYPE v gauge\nv 1\n")
	broken := testutil.WriteIn(t, root, "broken.prom", "not { prometheus\n")
	server := fileServer(t, dirCollector("dir", root, "*.prom"))
	logs := testutil.CaptureLogs(t)
	server.logger = slog.Default()
	for i := 0; i < 3; i++ {
		probeFile(t, server, "collector=dir").must(t, http.StatusOK, `localfile_scrape_error{file="broken.prom"} 1`)
	}
	if got := strings.Count(logs.String(), `"file":"broken.prom"`); got != 1 {
		t.Fatalf("a file failing 3 times was logged %d times:\n%s", got, logs.String())
	}
	if err := os.WriteFile(broken, []byte("# TYPE w gauge\nw 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	probeFile(t, server, "collector=dir").must(t, http.StatusOK, `localfile_scrape_error{file="broken.prom"} 0`)
	if !strings.Contains(logs.String(), `"msg":"file of a directory recovered"`) {
		t.Fatalf("recovery not logged:\n%s", logs.String())
	}
}
