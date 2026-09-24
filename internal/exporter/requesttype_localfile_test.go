//go:build !select_request_types || request_type_localfile

package exporter

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/url"
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

// A probe with no target reads request.path; the file's extension picks the
// decoder, and the Prometheus text passes through.
func TestLocalFileProbeReadsTheConfiguredFile(t *testing.T) {
	root := t.TempDir()
	testutil.WriteIn(t, root, "app.prom", promFile)
	server := fileServer(t, fileCollector("files", root, "app.prom"))
	probeFile(t, server, "collector=files").must(t, http.StatusOK, `app_jobs_total{queue="default"} 7`)
}

// The file is root/target/path: a target may name the file, a directory the
// path is read in, an absolute path inside root, or a file:// URL of one.
func TestLocalFileTargets(t *testing.T) {
	root := t.TempDir()
	testutil.WriteIn(t, root, "billing/status.json", `{"queue":{"depth":3}}`)
	testutil.WriteIn(t, root, "search/status.json", `{"queue":{"depth":9}}`)
	testutil.WriteIn(t, root, "direct.json", `{"queue":{"depth":1}}`)
	c := model.Collector{
		Name:      "status",
		Request:   model.RequestConfig{Type: fetch.RequestTypeLocalFile, Root: root, Path: "status.json"},
		Transform: model.TransformConfig{Type: "jq"},
		Metrics:   []model.MetricRule{{Name: "queue_depth", Type: model.GaugeMetricType, Expression: ".queue.depth"}},
	}
	byTarget := model.Collector{
		Name:      "any",
		Request:   model.RequestConfig{Type: fetch.RequestTypeLocalFile, Root: root},
		Transform: model.TransformConfig{Type: "jq"},
		Metrics:   []model.MetricRule{{Name: "queue_depth", Type: model.GaugeMetricType, Expression: ".queue.depth"}},
	}
	server := fileServer(t, c, byTarget)
	for query, want := range map[string]string{
		"collector=status&target=billing":                                        "queue_depth 3",
		"collector=status&target=search":                                         "queue_depth 9",
		"collector=status&target=" + url.QueryEscape(root+"/search"):             "queue_depth 9",
		"collector=status&target=" + url.QueryEscape("file://"+root+"/billing"):  "queue_depth 3",
		"collector=any&target=direct.json":                                       "queue_depth 1",
		"collector=any&target=billing/status.json":                               "queue_depth 3",
		"collector=any&target=" + url.QueryEscape("file://"+root+"/direct.json"): "queue_depth 1",
		"collector=any&target=search&path=status.json":                           "queue_depth 9",
	} {
		probeFile(t, server, query).must(t, http.StatusOK, want)
	}
	// Neither a target nor a path names a file.
	probeFile(t, server, "collector=any").must(t, http.StatusBadGateway, "no file to read")
	// A target that leaves root is the caller's mistake, refused before any
	// read.
	for _, target := range []string{"../etc", "/etc/passwd", "file:///etc/passwd", "billing/../../x"} {
		probeFile(t, server, "collector=any&target="+url.QueryEscape(target)).must(t, http.StatusBadRequest, "request.root")
	}
	probeFile(t, server, "collector=any&target="+url.QueryEscape("file://relative")).must(t, http.StatusBadRequest, "not a file:// URL of an absolute path")
	// A path probe parameter may not climb out of the target either.
	probeFile(t, server, "collector=status&target=billing&path="+url.QueryEscape("../direct.json")).must(t, http.StatusBadGateway, "leads outside request.root")
	probeFile(t, server, "collector=any&target=billing&path="+url.QueryEscape("/etc/passwd")).must(t, http.StatusBadGateway, "must be relative to request.root")
}

// Path parameters bind one file name each.
func TestLocalFilePathParameters(t *testing.T) {
	root := t.TempDir()
	testutil.WriteIn(t, root, "billing.prom", promFile)
	testutil.WriteIn(t, root, "default.prom", strings.Replace(promFile, "7", "1", 1))
	server := fileServer(t, fileCollector("apps", root, "{{param_app:default}}.prom"))
	probeFile(t, server, "collector=apps&param_app=billing").must(t, http.StatusOK, "} 7")
	probeFile(t, server, "collector=apps").must(t, http.StatusOK, "} 1")
	probeFile(t, server, "collector=apps&param_app="+url.QueryEscape("../billing")).must(t, http.StatusBadGateway, "single file or directory name")
	probeFile(t, server, "collector=apps&param_app=..").must(t, http.StatusBadRequest, `must not be ".."`)
	probeFile(t, server, "collector=apps&param_ap=billing").must(t, http.StatusBadRequest, "not used")
}

// Probe parameters of http do not apply to a file.
func TestLocalFileRejectsHTTPProbeParameters(t *testing.T) {
	root := t.TempDir()
	testutil.WriteIn(t, root, "app.prom", promFile)
	server := fileServer(t, fileCollector("files", root, "app.prom"))
	for _, parameter := range []string{"method=POST", "header_x_tenant=a", "retry_attempts=2", "insecure_skip_verify=true", "body=x"} {
		probeFile(t, server, "collector=files&"+parameter).must(t, http.StatusBadRequest, `request.type is "localfile"`)
	}
	// timeout is shared.
	probeFile(t, server, "collector=files&timeout=5s").must(t, http.StatusOK)
}

// An http collector still needs a target.
func TestHTTPProbesStillNeedATarget(t *testing.T) {
	server := fileServer(t, testutil.Collector("web", "text"))
	probeFile(t, server, "collector=web").must(t, http.StatusBadRequest, `the target parameter is required for collector "web", whose request.type is http`)
}

func TestLocalFileRefusesWhatIsNotARegularFileUnderRoot(t *testing.T) {
	root := t.TempDir()
	outside := testutil.WriteIn(t, t.TempDir(), "secret.prom", promFile)
	testutil.WriteIn(t, root, "real.prom", promFile)
	if err := os.Symlink("real.prom", filepath.Join(root, "inside.prom")); err != nil {
		t.Skip("no symbolic links here:", err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "outside.prom")); err != nil {
		t.Fatal(err)
	}
	// An absolute link is refused even when it points inside root, since
	// where it leads depends on where root is mounted.
	if err := os.Symlink(filepath.Join(root, "real.prom"), filepath.Join(root, "absolute.prom")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../"+filepath.Base(filepath.Dir(outside))+"/secret.prom", filepath.Join(root, "climbing.prom")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "dir.prom"), 0o700); err != nil {
		t.Fatal(err)
	}
	server := fileServer(t, fileCollector("files", root, ""))
	// A link that stays inside root is followed.
	probeFile(t, server, "collector=files&target=inside.prom").must(t, http.StatusOK, "} 7")
	// One that leaves it is not.
	for _, link := range []string{"outside.prom", "absolute.prom", "climbing.prom"} {
		probeFile(t, server, "collector=files&target="+link).must(t, http.StatusBadGateway, "collector files file failed", "outside request.root through a symbolic link")
	}
	probeFile(t, server, "collector=files&target=dir.prom").must(t, http.StatusBadGateway, "is not a regular file (a directory)")
	probeFile(t, server, "collector=files&target=missing.prom").must(t, http.StatusBadGateway, "missing.prom does not exist")
	if runtime.GOOS != "windows" {
		// A named pipe with no writer would block an ordinary open for good.
		if err := syscall.Mkfifo(filepath.Join(root, "pipe.prom"), 0o600); err == nil {
			done := make(chan *httpResult, 1)
			go func() { done <- probeFile(t, server, "collector=files&target=pipe.prom") }()
			select {
			case result := <-done:
				result.must(t, http.StatusBadGateway, "is not a regular file (a named pipe)")
			case <-time.After(5 * time.Second):
				t.Fatal("reading a named pipe hung the probe")
			}
		}
		if os.Getuid() != 0 {
			unreadable := testutil.WriteIn(t, root, "unreadable.prom", promFile)
			if err := os.Chmod(unreadable, 0); err != nil {
				t.Fatal(err)
			}
			probeFile(t, server, "collector=files&target=unreadable.prom").must(t, http.StatusBadGateway, "permission denied")
		}
	}
}

func TestLocalFileSizeLimit(t *testing.T) {
	root := t.TempDir()
	testutil.WriteIn(t, root, "big.prom", promFile+strings.Repeat("# padding\n", 100))
	c := fileCollector("files", root, "big.prom")
	c.Request.MaxResponseBytes = 64
	server := fileServer(t, c)
	probeFile(t, server, "collector=files").must(t, http.StatusBadGateway, "response size exceeds limit 64")
	if !strings.Contains(selfMetrics(t, server), `http_exporter_series_limit_exceeded_total{collector="files"} 1`) {
		t.Fatal("the size limit was not counted as a limit error")
	}
}

// A file whose writer has stopped fails the scrape once it is older than
// max_age.
func TestLocalFileMaxAge(t *testing.T) {
	root := t.TempDir()
	file := testutil.WriteIn(t, root, "app.prom", promFile)
	c := fileCollector("files", root, "app.prom")
	c.Request.MaxAge = model.Duration(time.Hour)
	server := fileServer(t, c)
	probeFile(t, server, "collector=files").must(t, http.StatusOK)
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(file, old, old); err != nil {
		t.Fatal(err)
	}
	probeFile(t, server, "collector=files").must(t, http.StatusBadGateway, "longer than request.max_age 1h0m0s", "whatever writes it has stopped")
}

// The verbose series of a file read carry its file:// URL, with path
// parameters as placeholders, and READ as the method.
func TestLocalFileVerboseLabels(t *testing.T) {
	root := t.TempDir()
	testutil.WriteIn(t, root, "billing.prom", promFile)
	c := fileCollector("apps", root, "{{param_app}}.prom")
	cfg := &model.Config{Collectors: []model.Collector{c}, Web: model.WebConfig{SelfMetrics: model.SelfMetricsConfig{Verbose: true}}}
	if err := config.Validate(cfg); err != nil {
		t.Fatal(err)
	}
	server := NewServer(config.NewManager(cfg, "", testutil.QuietLogger(t)), "python3", testutil.QuietLogger(t))
	probeFile(t, server, "collector=apps&param_app=billing").must(t, http.StatusOK)
	want := `http_exporter_scrapes_total{collector="apps",http_method="READ",url="file://` + filepath.ToSlash(root) + `/{{param_app}}.prom"} 1`
	if exposition := selfMetrics(t, server); !strings.Contains(exposition, want) {
		t.Fatalf("missing %s in:\n%s", want, exposition)
	}
}

// A static target of a localfile collector may leave target out, or name a
// file under root, and is scraped like any other.
func TestLocalFileStaticTargets(t *testing.T) {
	root := t.TempDir()
	testutil.WriteIn(t, root, "app.prom", promFile)
	testutil.WriteIn(t, root, "batch/app.prom", strings.Replace(promFile, "7", "3", 1))
	cfg := &model.Config{Collectors: []model.Collector{fileCollector("files", root, "app.prom")}, OTLP: otlpConfig("http://collector.invalid/v1/metrics")}
	if err := config.Validate(cfg); err != nil {
		t.Fatal(err)
	}
	file := &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{
		{ExportViaOTLP: true, Name: "main", Collector: "files", Labels: map[string]string{"source": "main"}},
		{ExportViaOTLP: true, Name: "batch", Collector: "files", Target: "batch", Labels: map[string]string{"source": "batch"}, Request: model.TargetRequestConfig{Timeout: model.Duration(time.Second)}},
	}}
	if err := config.ValidateStaticTargets(file); err != nil {
		t.Fatal(err)
	}
	if err := config.ValidateStaticTargetsAgainst(file, cfg); err != nil {
		t.Fatal(err)
	}
	server := newStaticServer(t, cfg, file)
	server.scrapeStaticTargets(context.Background(), 10*time.Second)
	values := map[string]float64{}
	for _, resource := range server.drainOTLP() {
		for _, m := range resource.Set.Metrics {
			if m.Name == "app_jobs_total" {
				values[m.Labels["source"]] = m.Value
			}
			if m.Name == "http_exporter_target_up" && m.Value != 1 {
				t.Errorf("target %s is down", m.Labels["static_target"])
			}
		}
	}
	if values["main"] != 7 || values["batch"] != 3 {
		t.Fatalf("values=%v", values)
	}

	for name, tc := range map[string]struct {
		target model.StaticTarget
		want   string
	}{
		"outside root":  {model.StaticTarget{ExportViaOTLP: true, Name: "bad", Collector: "files", Target: "/etc"}, `target "bad": target "/etc" is outside request.root`},
		"an http key":   {model.StaticTarget{ExportViaOTLP: true, Name: "bad", Collector: "files", Request: model.TargetRequestConfig{Method: "POST"}}, `sets request.method, which does not apply`},
		"a credential":  {model.StaticTarget{ExportViaOTLP: true, Name: "bad", Collector: "files", Request: model.TargetRequestConfig{BearerToken: "t"}}, `sets request.bearer_token`},
		"a placeholder": {model.StaticTarget{ExportViaOTLP: true, Name: "bad", Collector: "files", Request: model.TargetRequestConfig{Path: "{{param_x}}", PathSet: true}}, "placeholders"},
	} {
		t.Run(name, func(t *testing.T) {
			f := &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{tc.target}}
			err := config.ValidateStaticTargets(f)
			if err == nil {
				err = config.ValidateStaticTargetsAgainst(f, cfg)
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%v, want %q", err, tc.want)
			}
		})
	}
}

// A file of carbon lines is read with the graphite decoder, by its extension
// or by its content, into series a jq rule maps; a directory of them is read
// file by file.
func TestLocalFileCarbonLines(t *testing.T) {
	root := t.TempDir()
	now := time.Now().Unix()
	lines := func(value int) string {
		return strings.Join([]string{
			"# written by the nightly job",
			"backup.web01.duration_seconds 40 " + strconv.FormatInt(now-600, 10),
			"backup.web01.duration_seconds " + strconv.Itoa(value) + " " + strconv.FormatInt(now-60, 10),
			"backup.web02.duration_seconds 7 " + strconv.FormatInt(now-7200, 10),
		}, "\n") + "\n"
	}
	testutil.WriteIn(t, root, "backup.graphite", lines(42))
	testutil.WriteIn(t, root, "batch/backup.carbon", lines(5))
	testutil.WriteIn(t, root, "backup.txt", lines(9))
	c := fileCollector("carbon", root, "")
	c.Transform = model.TransformConfig{Type: "jq"}
	c.Response.Graphite = model.GraphiteConfig{MaxAge: model.Duration(time.Hour)}
	c.Metrics = []model.MetricRule{{Name: "backup_duration_seconds", Items: ".series[]", Expression: ".value", Labels: []model.LabelRule{{Name: "host", Expression: ".segments[1]"}}}}
	dir := dirCollector("carbon_dir", root, "*.graphite", "*.txt")
	dir.Transform, dir.Response, dir.Metrics = c.Transform, c.Response, c.Metrics
	server := fileServer(t, c, dir)
	probeFile(t, server, "collector=carbon&target=backup.graphite").must(t, http.StatusOK, `backup_duration_seconds{host="web01"} 42`)
	probeFile(t, server, "collector=carbon&target=batch/backup.carbon").must(t, http.StatusOK, `backup_duration_seconds{host="web01"} 5`)
	probeFile(t, server, "collector=carbon&target=backup.txt").must(t, http.StatusOK, `backup_duration_seconds{host="web01"} 9`)
	result := probeFile(t, server, "collector=carbon_dir")
	result.must(t, http.StatusOK, `backup_duration_seconds{file="backup.graphite",host="web01"} 42`, `backup_duration_seconds{file="backup.txt",host="web01"} 9`)
	if strings.Contains(result.body, "web02") {
		t.Fatalf("a series older than max_age is exported:\n%s", result.body)
	}
}

// With invalid_lines: skip, a carbon file with a torn line is read without
// it: the skipped lines and the series left out are counted in the
// self-metrics, the skipping logged once as a failure is and its end as a
// recovery, and the series left out logged at debug level.
func TestLocalFileCarbonLinesSkipped(t *testing.T) {
	root := t.TempDir()
	now := strconv.FormatInt(time.Now().Unix(), 10)
	file := testutil.WriteIn(t, root, "jobs.graphite", "jobs.a.done 1 "+now+"\njobs.b.do\njobs.old.done 1 1000\n")
	c := fileCollector("carbon", root, "jobs.graphite")
	c.Transform = model.TransformConfig{Type: "jq"}
	c.Response.Graphite = model.GraphiteConfig{MaxAge: model.Duration(time.Hour), InvalidLines: "skip"}
	c.Metrics = []model.MetricRule{{Name: "jobs_done", Items: ".series[]", Expression: ".value", Labels: []model.LabelRule{{Name: "job", Expression: ".segments[1]"}}}}
	server := fileServer(t, c)
	var logs bytes.Buffer
	server.logger = slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	for range 2 {
		probeFile(t, server, "collector=carbon").must(t, http.StatusOK, `jobs_done{job="a"} 1`)
	}
	metrics := selfMetrics(t, server)
	for _, want := range []string{`http_exporter_decoder_lines_skipped_total{collector="carbon"} 2`, `http_exporter_decoder_series_left_out_total{collector="carbon"} 2`} {
		if !strings.Contains(metrics, want) {
			t.Errorf("missing %s", want)
		}
	}
	text := logs.String()
	if n := strings.Count(text, `"level":"WARN","msg":"carbon lines skipped"`); n != 1 || !strings.Contains(text, `carbon line 2: \"jobs.b.do\" has 1 fields`) || !strings.Contains(text, `"skipped":1`) {
		t.Fatalf("%d warnings:\n%s", n, text)
	}
	if !strings.Contains(text, `"msg":"graphite series left out"`) || !strings.Contains(text, `"older_than_max_age":1`) {
		t.Fatalf("no debug line for the series left out:\n%s", text)
	}
	if err := os.WriteFile(file, []byte("jobs.a.done 2 "+now+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	probeFile(t, server, "collector=carbon").must(t, http.StatusOK, `jobs_done{job="a"} 2`)
	if !strings.Contains(logs.String(), `"msg":"carbon lines read whole again"`) {
		t.Fatalf("no recovery:\n%s", logs.String())
	}
}
