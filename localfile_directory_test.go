//go:build !select_request_types || request_type_localfile

package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// A localfile collector reading a directory with request.files
// (localfile_directory.go, filebatch.go).

// dirCollector reads the files matching patterns under root, passing
// Prometheus text through.
func dirCollector(name, root string, patterns ...string) Collector {
	c := fileCollector(name, root, "")
	c.Request.Files = patterns
	return c
}

func mtime(t *testing.T, path string, at time.Time) {
	t.Helper()
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
}

func TestLocalDirectoryValidation(t *testing.T) {
	root := t.TempDir()
	c := dirCollector("dir", root, "*.prom")
	if err := validateRequest(&c); err != nil {
		t.Fatal(err)
	}
	if c.Request.MaxFiles != DefaultLocalFileMaxFiles || c.Request.MaxTotalBytes != DefaultLocalFileMaxTotalBytes {
		t.Fatalf("defaults: max_files=%d max_total_bytes=%d", c.Request.MaxFiles, c.Request.MaxTotalBytes)
	}
	for name, tc := range map[string]struct {
		change func(c *Collector)
		want   string
	}{
		"path too":             {func(c *Collector) { c.Request.Path = "a.prom" }, "both request.path and request.files"},
		"a directory":          {func(c *Collector) { c.Request.Files = []string{"sub/*.prom"} }, "without / or"},
		"a bad pattern":        {func(c *Collector) { c.Request.Files = []string{"[a"} }, "is not valid"},
		"an empty pattern":     {func(c *Collector) { c.Request.Files = []string{" "} }, "must not be empty"},
		"negative files":       {func(c *Collector) { c.Request.MaxFiles = -1 }, "max_files must not be negative"},
		"negative bytes":       {func(c *Collector) { c.Request.MaxTotalBytes = -1 }, "max_total_bytes must not be negative"},
		"max_files for a file": {func(c *Collector) { c.Request.Files = nil; c.Request.Path = "a.prom"; c.Request.MaxFiles = 3 }, "request.max_files, which applies only"},
		"max_bytes for a file": {func(c *Collector) { c.Request.Files = nil; c.Request.Path = "a.prom"; c.Request.MaxTotalBytes = 3 }, "request.max_total_bytes, which applies only"},
		"files on http":        {func(c *Collector) { *c = testCollector("web", "text"); c.Request.Files = []string{"*"} }, "request.files, which does not apply"},
	} {
		t.Run(name, func(t *testing.T) {
			c := dirCollector("dir", root, "*.prom")
			tc.change(&c)
			err := validateRequest(&c)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%v, want %q", err, tc.want)
			}
		})
	}
}

// Every matching file is read, its series labelled with its name, and its
// mtime and scrape error reported; other files are not read.
func TestLocalDirectoryReadsEveryMatchingFile(t *testing.T) {
	root := t.TempDir()
	writeIn(t, root, "a.prom", "# TYPE jobs_total counter\njobs_total{queue=\"x\"} 1\n")
	writeIn(t, root, "b.prom", "# TYPE jobs_total counter\njobs_total{queue=\"x\"} 2\n# TYPE up_since gauge\nup_since 5\n")
	writeIn(t, root, "notes.txt", "not metrics\n")
	writeIn(t, root, ".c.prom", "# TYPE hidden gauge\nhidden 1\n")
	writeIn(t, root, "sub/d.prom", "# TYPE nested gauge\nnested 1\n")
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
	if _, err := parsePrometheusText([]byte(r.body)); err != nil {
		t.Fatalf("the answer does not parse: %v\n%s", err, r.body)
	}
	// A pattern starting with a dot reads hidden files.
	server = fileServer(t, dirCollector("hidden", root, ".*.prom"))
	probeFile(t, server, "collector=hidden").must(t, http.StatusOK, `hidden{file=".c.prom"} 1`)
}

// A broken file fails alone.
func TestLocalDirectoryOneBadFileDoesNotSinkTheRest(t *testing.T) {
	root := t.TempDir()
	writeIn(t, root, "good.prom", "# TYPE ok gauge\nok 1\n")
	writeIn(t, root, "broken.prom", "this is { not prometheus\n")
	writeIn(t, root, "labelled.prom", "# TYPE own gauge\nown{file=\"mine\"} 1\n")
	writeIn(t, root, "reserved.prom", "# TYPE localfile_scrape_error gauge\nlocalfile_scrape_error 0\n")
	writeIn(t, root, "zconflict.prom", "# TYPE ok counter\nok 2\n")
	server := fileServer(t, dirCollector("dir", root, "*.prom"))
	logs := captureLogs(t)
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
	writeIn(t, root, "eu.txt", "value=4\n")
	writeIn(t, root, "us.log", "value=6\n")
	writeIn(t, root, "status.json", `{"queued": 3}`)
	writeIn(t, root, "other.json", `{"running": 1}`)
	text := testCollector("text", "text")
	text.Request = RequestConfig{Type: RequestTypeLocalFile, Root: root, Files: []string{"*.txt", "*.log"}}
	text.Limits = Limits{}
	jq := Collector{
		Name:      "json",
		Request:   RequestConfig{Type: RequestTypeLocalFile, Root: root, Files: []string{"*.json"}},
		Transform: TransformConfig{Type: "jq"},
		Metrics:   []MetricRule{{Name: "queued", Type: GaugeMetricType, Expression: ".queued", ErrorMode: ErrorModeFail}},
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
		writeIn(t, root, "f"+strconv.Itoa(i)+".prom", "# TYPE v gauge\nv "+strconv.Itoa(i)+"\n")
	}
	c := dirCollector("dir", root, "*.prom")
	c.Request.MaxFiles = 2
	server := fileServer(t, c)
	logs := captureLogs(t)
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

// A file over the per-file limit, or past the scrape's total, is refused
// without being read, and fails alone.
func TestLocalDirectorySizeLimits(t *testing.T) {
	root := t.TempDir()
	small := "# TYPE v gauge\nv 1\n"
	writeIn(t, root, "a.prom", small)
	writeIn(t, root, "b-huge.prom", small+strings.Repeat("# padding\n", 1000))
	writeIn(t, root, "c.prom", small)
	writeIn(t, root, "d.prom", small)
	var mu sync.Mutex
	read := map[string]bool{}
	setReadHook(func(full string) {
		mu.Lock()
		defer mu.Unlock()
		read[filepath.Base(full)] = true
	})
	t.Cleanup(func() { afterLocalFileRead.Store(nil) })

	c := dirCollector("dir", root, "*.prom")
	c.Request.MaxResponseBytes = 1000
	c.Request.MaxTotalBytes = ByteSize(3*len(small) - 1)
	server := fileServer(t, c)
	logs := captureLogs(t)
	server.logger = slog.Default()
	r := probeFile(t, server, "collector=dir")
	r.must(t, http.StatusOK,
		`v{file="a.prom"} 1`, `v{file="c.prom"} 1`,
		`localfile_scrape_error{file="b-huge.prom"} 1`,
		`localfile_scrape_error{file="d.prom"} 1`,
		`localfile_mtime_seconds{file="b-huge.prom"}`,
	)
	mu.Lock()
	if read["b-huge.prom"] || read["d.prom"] {
		t.Errorf("refused files were read: %v", read)
	}
	mu.Unlock()
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
	writeIn(t, root, "fresh.prom", "# TYPE v gauge\nv 1\n")
	stale := writeIn(t, root, "stale.prom", "# TYPE v gauge\nv 2\n")
	mtime(t, stale, time.Now().Add(-2*time.Hour))
	c := dirCollector("dir", root, "*.prom")
	c.Request.MaxAge = Duration(time.Hour)
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
	writeIn(t, root, "a.prom", "# TYPE v gauge\nv 1\n")
	writeIn(t, root, "batch/b.prom", "# TYPE v gauge\nv 2\n")
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
	outside := writeIn(t, t.TempDir(), "secret.prom", "# TYPE secret gauge\nsecret 1\n")
	writeIn(t, root, "a.prom", "# TYPE v gauge\nv 1\n")
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
	writeIn(t, root, "a.prom", "# TYPE v gauge\nv 1\n")
	cfg := &Config{Collectors: []Collector{dirCollector("dir", root, "*.prom")}, Web: WebConfig{SelfMetrics: SelfMetricsConfig{Verbose: true}}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	server := NewServer(NewConfigManager(cfg, "", quietLogger(t)), "python3", quietLogger(t))
	probeFile(t, server, "collector=dir").must(t, http.StatusOK)
	want := `http_exporter_scrapes_total{collector="dir",http_method="READ",url="file://` + filepath.ToSlash(root) + `/"} 1`
	if exposition := selfMetrics(t, server); !strings.Contains(exposition, want) {
		t.Fatalf("missing %s in:\n%s", want, exposition)
	}
}

// A scheduled target reads a directory too; its request may not name a path.
func TestLocalDirectoryScheduledTargets(t *testing.T) {
	root := t.TempDir()
	writeIn(t, root, "a.prom", "# TYPE v gauge\nv 1\n")
	writeIn(t, root, "b.prom", "broken {\n")
	cfg := &Config{Collectors: []Collector{dirCollector("dir", root, "*.prom")}, OTLP: otlpConfig("http://collector.invalid/v1/metrics")}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	file := &TargetFile{Targets: []ScheduledTarget{{Name: "textfiles", Collector: "dir", Labels: map[string]string{"source": "node"}}}}
	server := newScheduledServer(t, cfg, file)
	server.logger = quietLogger(t)
	server.scrapeScheduledTargets(context.Background(), 10*time.Second)
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

	bad := &TargetFile{Targets: []ScheduledTarget{{Name: "bad", Collector: "dir", Request: TargetRequestConfig{Path: "a.prom", PathSet: true}}}}
	err := bad.Validate()
	if err == nil {
		err = bad.ValidateAgainst(cfg)
	}
	if err == nil || !strings.Contains(err.Error(), "reads every file of a directory") {
		t.Fatalf("a scheduled target naming a path: %v", err)
	}
}

// The documented example loads and reads a textfile directory.
func TestLocalDirectoryDocumentationExample(t *testing.T) {
	doc, err := os.ReadFile("docs/LOCALFILE.md")
	if err != nil {
		t.Fatal(err)
	}
	_, rest, ok := strings.Cut(string(doc), "## Reading a directory")
	if !ok {
		t.Fatal("docs/LOCALFILE.md has no Reading a directory section")
	}
	_, block, _ := strings.Cut(rest, "```yaml\n")
	block, _, _ = strings.Cut(block, "```")
	root := t.TempDir()
	block = strings.ReplaceAll(block, "/var/lib/node_exporter/textfile_collector", root)
	path := writeIn(t, t.TempDir(), "config.yaml", block)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("the example does not load: %v\n%s", err, block)
	}
	writeIn(t, root, "backup.prom", "# TYPE backup_last_success_timestamp_seconds gauge\nbackup_last_success_timestamp_seconds 1.7e+09\n")
	server := NewServer(NewConfigManager(cfg, "", quietLogger(t)), "python3", quietLogger(t))
	probeFile(t, server, "collector="+cfg.Collectors[0].Name).must(t, http.StatusOK, `backup_last_success_timestamp_seconds{file="backup.prom"}`)
}

// A file the read has not finished by the probe's deadline fails alone; what
// was read is answered.
func TestLocalDirectoryAnswersWhatWasReadByTheDeadline(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"a.prom", "b.prom", "slow.prom"} {
		writeIn(t, root, name, "# TYPE v gauge\nv 1\n")
	}
	hold := make(chan struct{})
	setReadHook(func(full string) {
		if filepath.Base(full) == "slow.prom" {
			<-hold
		}
	})
	t.Cleanup(func() { close(hold); afterLocalFileRead.Store(nil) })
	server := fileServer(t, dirCollector("dir", root, "*.prom"))
	start := time.Now()
	r := probeFile(t, server, "collector=dir&timeout=300ms")
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("the probe took %s", took)
	}
	r.must(t, http.StatusOK,
		`v{file="a.prom"} 1`, `v{file="b.prom"} 1`,
		`localfile_scrape_error{file="slow.prom"} 1`,
		`localfile_mtime_seconds{file="slow.prom"}`,
	)
	if strings.Contains(r.body, `v{file="slow.prom"}`) {
		t.Fatalf("the file still being read was answered:\n%s", r.body)
	}
}

// Files are read several at a time, never more than the workers.
func TestLocalDirectoryReadsFilesConcurrently(t *testing.T) {
	root := t.TempDir()
	for i := range 10 {
		writeIn(t, root, "f"+strconv.Itoa(i)+".prom", "# TYPE v gauge\nv "+strconv.Itoa(i)+"\n")
	}
	var mu sync.Mutex
	current, peak := 0, 0
	setReadHook(func(string) {
		mu.Lock()
		current++
		peak = max(peak, current)
		mu.Unlock()
		time.Sleep(30 * time.Millisecond)
		mu.Lock()
		current--
		mu.Unlock()
	})
	t.Cleanup(func() { afterLocalFileRead.Store(nil) })
	server := fileServer(t, dirCollector("dir", root, "*.prom"))
	r := probeFile(t, server, "collector=dir")
	r.must(t, http.StatusOK, `v{file="f0.prom"} 0`, `v{file="f9.prom"} 9`)
	// The answer is in name order whatever order the reads finished in.
	if strings.Index(r.body, `file="f0.prom"`) > strings.Index(r.body, `file="f9.prom"`) {
		t.Fatalf("the answer is not in name order:\n%s", r.body)
	}
	mu.Lock()
	defer mu.Unlock()
	if peak < 2 || peak > localFileDirectoryWorkers {
		t.Fatalf("%d files were read at once, want between 2 and %d", peak, localFileDirectoryWorkers)
	}
}

// Listing stops at maxListedEntries, saying so.
func TestLocalDirectoryListingIsBounded(t *testing.T) {
	root := t.TempDir()
	c := dirCollector("dir", root, "*.prom")
	c.Request.MaxFiles = 1
	limit := maxListedEntries(&c)
	for i := range limit + 50 {
		writeIn(t, root, "f"+strconv.Itoa(i)+".txt", "")
	}
	server := fileServer(t, c)
	logs := captureLogs(t)
	server.logger = slog.Default()
	probeFile(t, server, "collector=dir").must(t, http.StatusOK, "localfile_files_skipped 0")
	if !strings.Contains(logs.String(), "more entries than one scrape lists") || !strings.Contains(logs.String(), `"listed":`+strconv.Itoa(limit)) {
		t.Fatalf("the bounded listing was not logged:\n%s", logs.String())
	}
}

// A file that grows after its size was taken, past what the total leaves it,
// fails alone, so the scrape never reads more than max_total_bytes.
func TestLocalDirectoryTotalHoldsWhenAFileGrows(t *testing.T) {
	root := t.TempDir()
	small := "# TYPE v gauge\nv 1\n"
	writeIn(t, root, "a.prom", small)
	grow := writeIn(t, root, "b.prom", small)
	c := dirCollector("dir", root, "*.prom")
	c.Request.MaxTotalBytes = ByteSize(2*len(small) + 4)
	var once sync.Once
	// Growing b.prom during its own read makes the read start again, and
	// find it larger than its share.
	setReadHook(func(full string) {
		if filepath.Base(full) == "b.prom" {
			once.Do(func() {
				if err := os.WriteFile(grow, []byte(small+strings.Repeat("# grown\n", 20)), 0o600); err != nil {
					t.Error(err)
				}
			})
		}
	})
	t.Cleanup(func() { afterLocalFileRead.Store(nil) })
	server := fileServer(t, c)
	logs := captureLogs(t)
	server.logger = slog.Default()
	probeFile(t, server, "collector=dir").must(t, http.StatusOK, `v{file="a.prom"} 1`, `localfile_scrape_error{file="b.prom"} 1`)
	if !strings.Contains(logs.String(), "grew while the directory was read") {
		t.Fatalf("the reason was not logged:\n%s", logs.String())
	}
}

// Files are converted from response.charset, and what still is not valid
// UTF-8 is repaired, file by file.
func TestLocalDirectoryFilesInOtherEncodings(t *testing.T) {
	root := t.TempDir()
	writeIn(t, root, "a.txt", "v=1 caf\xe9\n")
	c := regexCollector("latin")
	c.Request = RequestConfig{Type: RequestTypeLocalFile, Root: root, Files: []string{"*.txt"}}
	c.Response.Charset = "iso-8859-1"
	broken := regexCollector("broken")
	broken.Request = RequestConfig{Type: RequestTypeLocalFile, Root: root, Files: []string{"*.txt"}}
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
	writeIn(t, root, "good.prom", "# TYPE v gauge\nv 1\n")
	broken := writeIn(t, root, "broken.prom", "not { prometheus\n")
	server := fileServer(t, dirCollector("dir", root, "*.prom"))
	logs := captureLogs(t)
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
