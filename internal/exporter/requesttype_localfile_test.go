//go:build !select_request_types || request_type_localfile

package exporter

import (
	"context"
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

// A file that changes under the read is read again; one that keeps changing
// is refused, rather than exporting a torn write.
func TestLocalFileChangedWhileRead(t *testing.T) {
	root := t.TempDir()
	file := testutil.WriteIn(t, root, "app.prom", promFile)
	server := fileServer(t, fileCollector("files", root, "app.prom"))
	t.Cleanup(func() { fetch.AfterLocalFileRead.Store(nil) })

	reads := 0
	setReadHook(func(string) {
		reads++
		if reads == 1 {
			// The writer rewrites the file in place during the first read.
			testutil.WriteIn(t, root, "app.prom", strings.Replace(promFile, "7", "8", 1)+"# more\n")
		}
	})
	probeFile(t, server, "collector=files").must(t, http.StatusOK, "} 8")
	if reads != 2 {
		t.Fatalf("read %d times, want 2", reads)
	}

	setReadHook(func(string) {
		reads++
		later := time.Now().Add(time.Duration(reads) * time.Second)
		if err := os.Chtimes(file, later, later); err != nil {
			t.Error(err)
		}
	})
	probeFile(t, server, "collector=files").must(t, http.StatusBadGateway, "changed while it was read", "rename it into place")
}

// A read that does not return in time — a hung network filesystem — does not
// hold the probe past its timeout.
func TestLocalFileTimeout(t *testing.T) {
	root := t.TempDir()
	testutil.WriteIn(t, root, "app.prom", promFile)
	server := fileServer(t, fileCollector("files", root, "app.prom"))
	release := make(chan struct{})
	t.Cleanup(func() {
		close(release)
		fetch.AfterLocalFileRead.Store(nil)
	})
	setReadHook(func(string) { <-release })
	start := time.Now()
	probeFile(t, server, "collector=files&timeout=100ms").must(t, http.StatusBadGateway, context.DeadlineExceeded.Error())
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("the probe took %s", elapsed)
	}
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

// A scheduled target of a localfile collector may leave target out, or name a
// file under root, and is scraped like any other.
func TestLocalFileScheduledTargets(t *testing.T) {
	root := t.TempDir()
	testutil.WriteIn(t, root, "app.prom", promFile)
	testutil.WriteIn(t, root, "batch/app.prom", strings.Replace(promFile, "7", "3", 1))
	cfg := &model.Config{Collectors: []model.Collector{fileCollector("files", root, "app.prom")}, OTLP: otlpConfig("http://collector.invalid/v1/metrics")}
	if err := config.Validate(cfg); err != nil {
		t.Fatal(err)
	}
	file := &model.TargetFile{Targets: []model.ScheduledTarget{
		{Name: "main", Collector: "files", Labels: map[string]string{"source": "main"}},
		{Name: "batch", Collector: "files", Target: "batch", Labels: map[string]string{"source": "batch"}, Request: model.TargetRequestConfig{Timeout: model.Duration(time.Second)}},
	}}
	if err := config.ValidateTargets(file); err != nil {
		t.Fatal(err)
	}
	if err := config.ValidateTargetsAgainst(file, cfg); err != nil {
		t.Fatal(err)
	}
	server := newScheduledServer(t, cfg, file)
	server.scrapeScheduledTargets(context.Background(), 10*time.Second)
	values := map[string]float64{}
	for _, resource := range server.drainOTLP() {
		for _, m := range resource.Set.Metrics {
			if m.Name == "app_jobs_total" {
				values[m.Labels["source"]] = m.Value
			}
			if m.Name == "http_exporter_target_up" && m.Value != 1 {
				t.Errorf("target %s is down", m.Labels["scheduled_target"])
			}
		}
	}
	if values["main"] != 7 || values["batch"] != 3 {
		t.Fatalf("values=%v", values)
	}

	for name, tc := range map[string]struct {
		target model.ScheduledTarget
		want   string
	}{
		"outside root":  {model.ScheduledTarget{Name: "bad", Collector: "files", Target: "/etc"}, `target "bad": target "/etc" is outside request.root`},
		"an http key":   {model.ScheduledTarget{Name: "bad", Collector: "files", Request: model.TargetRequestConfig{Method: "POST"}}, `sets request.method, which does not apply`},
		"a credential":  {model.ScheduledTarget{Name: "bad", Collector: "files", Request: model.TargetRequestConfig{BearerToken: "t"}}, `sets request.bearer_token`},
		"a placeholder": {model.ScheduledTarget{Name: "bad", Collector: "files", Request: model.TargetRequestConfig{Path: "{{param_x}}", PathSet: true}}, "placeholders"},
	} {
		t.Run(name, func(t *testing.T) {
			f := &model.TargetFile{Targets: []model.ScheduledTarget{tc.target}}
			err := config.ValidateTargets(f)
			if err == nil {
				err = config.ValidateTargetsAgainst(f, cfg)
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%v, want %q", err, tc.want)
			}
		})
	}
}

// Reads left running on a filesystem that has stopped answering are capped per
// collector: once every slot is held, a probe fails at once instead of adding
// another goroutine, and the slots come back as the reads return.
func TestLocalFilePendingReadsAreCapped(t *testing.T) {
	root := t.TempDir()
	for i := 0; i <= fetch.LocalFileMaxPendingReads; i++ {
		testutil.WriteIn(t, root, "f"+strconv.Itoa(i)+".prom", promFile)
	}
	server := fileServer(t, fileCollector("stuck", root, ""), fileCollector("other", root, ""))
	release := make(chan struct{})
	t.Cleanup(func() { fetch.AfterLocalFileRead.Store(nil) })
	setReadHook(func(string) { <-release })

	// Different files, so the probes do not share one read.
	for i := 0; i < fetch.LocalFileMaxPendingReads; i++ {
		probeFile(t, server, "collector=stuck&timeout=20ms&target=f"+strconv.Itoa(i)+".prom").must(t, http.StatusBadGateway, context.DeadlineExceeded.Error())
	}
	if got := fetch.LocalFileReads.Pending("stuck"); got != fetch.LocalFileMaxPendingReads {
		t.Fatalf("pending=%d, want %d", got, fetch.LocalFileMaxPendingReads)
	}
	start := time.Now()
	probeFile(t, server, "collector=stuck&timeout=5s&target=f"+strconv.Itoa(fetch.LocalFileMaxPendingReads)+".prom").must(t, http.StatusBadGateway, "already has 4 file reads that have not returned")
	if time.Since(start) > time.Second {
		t.Fatal("a probe over the cap waited instead of failing at once")
	}
	// The cap is per collector.
	if got := fetch.LocalFileReads.Pending("other"); got != 0 {
		t.Fatalf("other collector pending=%d", got)
	}

	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for fetch.LocalFileReads.Pending("stuck") > 0 {
		if time.Now().After(deadline) {
			t.Fatalf("pending reads never returned: %d", fetch.LocalFileReads.Pending("stuck"))
		}
		time.Sleep(5 * time.Millisecond)
	}
	fetch.AfterLocalFileRead.Store(nil)
	probeFile(t, server, "collector=stuck&target=f0.prom").must(t, http.StatusOK, "} 7")
}

// The budget bounds the whole trip, a file read included, and a probe without
// the header is unaffected.
func TestTheBudgetBoundsFileReadsAndIsOptional(t *testing.T) {
	root := t.TempDir()
	testutil.WriteIn(t, root, "app.prom", promFile)
	server := fileServer(t, fileCollector("files", root, "app.prom"))
	server.SetTimeoutOffset(0)

	release := make(chan struct{})
	t.Cleanup(func() {
		close(release)
		fetch.AfterLocalFileRead.Store(nil)
	})
	setReadHook(func(string) { <-release })
	recorder := probeWithScrapeTimeout(t, server, "collector=files", "0.2")
	if recorder.Code != http.StatusBadGateway || !strings.Contains(recorder.Body.String(), "200ms budget") {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body)
	}

	fetch.AfterLocalFileRead.Store(nil)
	if recorder := probeOnce(t, server, "/probe?collector=files", nil); recorder.Code != http.StatusOK {
		t.Fatalf("without the header: status=%d body=%s", recorder.Code, recorder.Body)
	}
	if recorder := probeWithScrapeTimeout(t, server, "collector=files", "10"); recorder.Code != http.StatusOK {
		t.Fatalf("with a generous timeout: status=%d body=%s", recorder.Code, recorder.Body)
	}
}
