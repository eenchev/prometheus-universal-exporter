//go:build !select_request_types || request_type_localfile

package main

import (
	"context"
	"errors"
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
)

// The localfile request type reads a file under request.root
// (requesttype_localfile.go).

const promFile = "# HELP app_jobs_total Jobs run.\n# TYPE app_jobs_total counter\napp_jobs_total{queue=\"default\"} 7\n"

// fileCollector is a localfile collector passing a Prometheus text file
// through.
func fileCollector(name, root, path string) Collector {
	return Collector{
		Name:          name,
		Request:       RequestConfig{Type: RequestTypeLocalFile, Root: root, Path: path},
		Transform:     TransformConfig{Type: "prometheus"},
		ErrorHandling: ErrorHandling{OnFetchError: "fail", OnDecodeError: "fail", OnTransformError: "fail"},
	}
}

func fileServer(t *testing.T, collectors ...Collector) *Server {
	t.Helper()
	cfg := &Config{Collectors: collectors}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	return NewServer(NewConfigManager(cfg, "", quietLogger(t)), "python3", quietLogger(t))
}

func probeFile(t *testing.T, server *Server, query string) *httpResult {
	t.Helper()
	recorder := probeOnce(t, server, "/probe?"+query, nil)
	return &httpResult{code: recorder.Code, body: recorder.Body.String()}
}

type httpResult struct {
	code int
	body string
}

func (r *httpResult) must(t *testing.T, code int, fragments ...string) {
	t.Helper()
	if r.code != code {
		t.Fatalf("status=%d, want %d; body:\n%s", r.code, code, r.body)
	}
	for _, fragment := range fragments {
		if !strings.Contains(r.body, fragment) {
			t.Fatalf("body does not contain %q:\n%s", fragment, r.body)
		}
	}
}

func TestLocalFileValidation(t *testing.T) {
	root := t.TempDir()
	valid := fileCollector("files", root+"/", "app.prom")
	cfg := &Config{Collectors: []Collector{valid}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if got := cfg.Collectors[0].Request.Root; got != root {
		t.Fatalf("root=%q, want it cleaned to %q", got, root)
	}
	for name, tc := range map[string]struct {
		change func(*Collector)
		want   string
	}{
		"no root":             {func(c *Collector) { c.Request.Root = "" }, "request.root is required"},
		"relative root":       {func(c *Collector) { c.Request.Root = "var/lib" }, "must be an absolute path"},
		"filesystem root":     {func(c *Collector) { c.Request.Root = "/" }, "must not be the filesystem root"},
		"absolute path":       {func(c *Collector) { c.Request.Path = "/etc/passwd" }, "must be relative to request.root"},
		"escaping path":       {func(c *Collector) { c.Request.Path = "../secret" }, "leads outside request.root"},
		"escaping deeper":     {func(c *Collector) { c.Request.Path = "a/../../secret" }, "leads outside request.root"},
		"negative max_age":    {func(c *Collector) { c.Request.MaxAge = Duration(-time.Second) }, "max_age must not be negative"},
		"bad placeholder":     {func(c *Collector) { c.Request.Path = "{{tenant}}.json" }, "not a path parameter"},
		"an http key":         {func(c *Collector) { c.Request.Method = "POST" }, `request.method, which does not apply to request.type "localfile"`},
		"an http credential":  {func(c *Collector) { c.Request.BearerToken = "t" }, "request.bearer_token, which does not apply"},
		"negative size limit": {func(c *Collector) { c.Request.MaxResponseBytes = -1 }, "max_response_bytes must not be negative"},
	} {
		t.Run(name, func(t *testing.T) {
			c := fileCollector("files", root, "app.prom")
			tc.change(&c)
			err := (&Config{Collectors: []Collector{c}}).Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%v, want %q", err, tc.want)
			}
		})
	}
	// The localfile keys do not apply to http.
	for key, change := range map[string]func(*Collector){
		"root":    func(c *Collector) { c.Request.Root = root },
		"max_age": func(c *Collector) { c.Request.MaxAge = Duration(time.Minute) },
	} {
		c := testCollector("web", "text")
		change(&c)
		err := (&Config{Collectors: []Collector{c}}).Validate()
		if err == nil || !strings.Contains(err.Error(), "request."+key+`, which does not apply to request.type "http"`) {
			t.Errorf("%s on http: err=%v", key, err)
		}
	}
}

// A probe with no target reads request.path; the file's extension picks the
// decoder, and the Prometheus text passes through.
func TestLocalFileProbeReadsTheConfiguredFile(t *testing.T) {
	root := t.TempDir()
	writeIn(t, root, "app.prom", promFile)
	server := fileServer(t, fileCollector("files", root, "app.prom"))
	probeFile(t, server, "collector=files").must(t, http.StatusOK, `app_jobs_total{queue="default"} 7`)
}

// The file is root/target/path: a target may name the file, a directory the
// path is read in, an absolute path inside root, or a file:// URL of one.
func TestLocalFileTargets(t *testing.T) {
	root := t.TempDir()
	writeIn(t, root, "billing/status.json", `{"queue":{"depth":3}}`)
	writeIn(t, root, "search/status.json", `{"queue":{"depth":9}}`)
	writeIn(t, root, "direct.json", `{"queue":{"depth":1}}`)
	c := Collector{
		Name:      "status",
		Request:   RequestConfig{Type: RequestTypeLocalFile, Root: root, Path: "status.json"},
		Transform: TransformConfig{Type: "jq"},
		Metrics:   []MetricRule{{Name: "queue_depth", Type: GaugeMetricType, Expression: ".queue.depth"}},
	}
	byTarget := Collector{
		Name:      "any",
		Request:   RequestConfig{Type: RequestTypeLocalFile, Root: root},
		Transform: TransformConfig{Type: "jq"},
		Metrics:   []MetricRule{{Name: "queue_depth", Type: GaugeMetricType, Expression: ".queue.depth"}},
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
	writeIn(t, root, "billing.prom", promFile)
	writeIn(t, root, "default.prom", strings.Replace(promFile, "7", "1", 1))
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
	writeIn(t, root, "app.prom", promFile)
	server := fileServer(t, fileCollector("files", root, "app.prom"))
	for _, parameter := range []string{"method=POST", "header_x_tenant=a", "retry_attempts=2", "insecure_skip_verify=true", "body=x"} {
		probeFile(t, server, "collector=files&"+parameter).must(t, http.StatusBadRequest, `request.type is "localfile"`)
	}
	// timeout is shared.
	probeFile(t, server, "collector=files&timeout=5s").must(t, http.StatusOK)
}

// An http collector still needs a target.
func TestHTTPProbesStillNeedATarget(t *testing.T) {
	server := fileServer(t, testCollector("web", "text"))
	probeFile(t, server, "collector=web").must(t, http.StatusBadRequest, "target and collector are required")
}

func TestLocalFileRefusesWhatIsNotARegularFileUnderRoot(t *testing.T) {
	root := t.TempDir()
	outside := writeIn(t, t.TempDir(), "secret.prom", promFile)
	writeIn(t, root, "real.prom", promFile)
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
			unreadable := writeIn(t, root, "unreadable.prom", promFile)
			if err := os.Chmod(unreadable, 0); err != nil {
				t.Fatal(err)
			}
			probeFile(t, server, "collector=files&target=unreadable.prom").must(t, http.StatusBadGateway, "permission denied")
		}
	}
}

func TestLocalFileSizeLimit(t *testing.T) {
	root := t.TempDir()
	writeIn(t, root, "big.prom", promFile+strings.Repeat("# padding\n", 100))
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
	file := writeIn(t, root, "app.prom", promFile)
	c := fileCollector("files", root, "app.prom")
	c.Request.MaxAge = Duration(time.Hour)
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
	file := writeIn(t, root, "app.prom", promFile)
	server := fileServer(t, fileCollector("files", root, "app.prom"))
	t.Cleanup(func() { afterLocalFileRead.Store(nil) })

	reads := 0
	setReadHook(func(string) {
		reads++
		if reads == 1 {
			// The writer rewrites the file in place during the first read.
			writeIn(t, root, "app.prom", strings.Replace(promFile, "7", "8", 1)+"# more\n")
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
	writeIn(t, root, "app.prom", promFile)
	server := fileServer(t, fileCollector("files", root, "app.prom"))
	release := make(chan struct{})
	t.Cleanup(func() {
		close(release)
		afterLocalFileRead.Store(nil)
	})
	setReadHook(func(string) { <-release })
	start := time.Now()
	probeFile(t, server, "collector=files&timeout=100ms").must(t, http.StatusBadGateway, context.DeadlineExceeded.Error())
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("the probe took %s", elapsed)
	}
}

// Transforms see the file as a response: its content type from the
// extension, its size and its modification time.
func TestLocalFileResponseHeaders(t *testing.T) {
	root := t.TempDir()
	file := writeIn(t, root, "status.JSON", `{"a":1}`)
	modified := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := os.Chtimes(file, modified, modified); err != nil {
		t.Fatal(err)
	}
	c := fileCollector("files", root, "status.JSON")
	resp, err := fetchLocalFile(context.Background(), "", &c, RequestOverrides{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || string(resp.Body) != `{"a":1}` {
		t.Fatalf("resp=%+v", resp)
	}
	for header, want := range map[string]string{"Content-Type": "application/json", "Content-Length": "7", "Last-Modified": "Fri, 02 Jan 2026 03:04:05 GMT"} {
		if got := resp.Headers.Get(header); got != want {
			t.Errorf("%s=%q, want %q", header, got, want)
		}
	}
	for name, want := range map[string]string{
		"a.json": "application/json", "a.yml": "application/yaml", "a.yaml": "application/yaml", "a.xml": "application/xml",
		"a.csv": "text/csv", "a.htm": "text/html", "a.html": "text/html", "a.prom": "text/plain; version=0.0.4", "a.txt": "", "a": "",
	} {
		if got := localFileContentType(name); got != want {
			t.Errorf("%s: %q, want %q", name, got, want)
		}
	}
}

// The verbose series of a file read carry its file:// URL, with path
// parameters as placeholders, and READ as the method.
func TestLocalFileVerboseLabels(t *testing.T) {
	root := t.TempDir()
	writeIn(t, root, "billing.prom", promFile)
	c := fileCollector("apps", root, "{{param_app}}.prom")
	cfg := &Config{Collectors: []Collector{c}, Web: WebConfig{SelfMetrics: SelfMetricsConfig{Verbose: true}}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	server := NewServer(NewConfigManager(cfg, "", quietLogger(t)), "python3", quietLogger(t))
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
	writeIn(t, root, "app.prom", promFile)
	writeIn(t, root, "batch/app.prom", strings.Replace(promFile, "7", "3", 1))
	cfg := &Config{Collectors: []Collector{fileCollector("files", root, "app.prom")}, OTLP: otlpConfig("http://collector.invalid/v1/metrics")}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	file := &TargetFile{Targets: []ScheduledTarget{
		{Name: "main", Collector: "files", Labels: map[string]string{"source": "main"}},
		{Name: "batch", Collector: "files", Target: "batch", Labels: map[string]string{"source": "batch"}, Request: TargetRequestConfig{Timeout: Duration(time.Second)}},
	}}
	if err := file.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := file.ValidateAgainst(cfg); err != nil {
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
		target ScheduledTarget
		want   string
	}{
		"outside root":  {ScheduledTarget{Name: "bad", Collector: "files", Target: "/etc"}, `target "bad": target "/etc" is outside request.root`},
		"an http key":   {ScheduledTarget{Name: "bad", Collector: "files", Request: TargetRequestConfig{Method: "POST"}}, `sets request.method, which does not apply`},
		"a credential":  {ScheduledTarget{Name: "bad", Collector: "files", Request: TargetRequestConfig{BearerToken: "t"}}, `sets request.bearer_token`},
		"a placeholder": {ScheduledTarget{Name: "bad", Collector: "files", Request: TargetRequestConfig{Path: "{{param_x}}", PathSet: true}}, "placeholders"},
	} {
		t.Run(name, func(t *testing.T) {
			f := &TargetFile{Targets: []ScheduledTarget{tc.target}}
			err := f.Validate()
			if err == nil {
				err = f.ValidateAgainst(cfg)
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%v, want %q", err, tc.want)
			}
		})
	}
}

// localFileTargetDir is the one place a target is interpreted.
func TestLocalFileTargetInterpretation(t *testing.T) {
	c := fileCollector("files", "/srv/metrics", "")
	for target, want := range map[string]string{
		"": "", "app.prom": "app.prom", "./app.prom": "app.prom", "a/b/../c": "a/c",
		"/srv/metrics": "", "/srv/metrics/": "", "/srv/metrics/a/b.prom": "a/b.prom", "file:///srv/metrics/a.prom": "a.prom",
	} {
		got, err := localFileTargetDir(&c, target)
		if err != nil || got != filepath.FromSlash(want) {
			t.Errorf("%q: got %q, %v; want %q", target, got, err, want)
		}
	}
	for _, target := range []string{"..", "../x", "/srv/metricsx/a", "/srv", "/etc/passwd", "file://srv/metrics", "a\x00b"} {
		if _, err := localFileTargetDir(&c, target); err == nil {
			t.Errorf("%q was accepted", target)
		}
	}
	if !errors.Is(checkTarget(ptr(testCollector("web", "text")), "", false), errMissingTarget) {
		t.Error("an http probe without a target must be refused")
	}
}

func ptr[T any](v T) *T { return &v }

func setReadHook(hook func(string)) { afterLocalFileRead.Store(&hook) }

// The examples in docs/LOCALFILE.md work as written, with their root replaced
// by a temporary directory.
func TestLocalFileDocumentationExamples(t *testing.T) {
	doc := read(t, "docs/LOCALFILE.md")
	const documentedRoot = "/var/lib/node_exporter/textfile_collector"
	var blocks []string
	for _, part := range strings.Split(doc, "```yaml\n")[1:] {
		block, _, _ := strings.Cut(part, "```")
		blocks = append(blocks, block)
	}
	find := func(prefix string) string {
		for _, block := range blocks {
			if strings.HasPrefix(block, prefix) {
				return block
			}
		}
		t.Fatalf("docs/LOCALFILE.md has no example starting %q", prefix)
		return ""
	}
	root := t.TempDir()
	writeIn(t, root, "batch.prom", promFile)
	writeIn(t, root, "backup.prom", strings.Replace(promFile, "7", "2", 1))
	writeIn(t, root, "nightly/batch.prom", strings.Replace(promFile, "7", "5", 1))
	config := strings.ReplaceAll(find("collectors:\n")+find("  - name: any_textfile\n"), documentedRoot, root)
	config += "otlp:\n  enabled: true\n  endpoint: http://collector.invalid/v1/metrics\n"
	path := writeIn(t, t.TempDir(), "config.yaml", config)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("%v\n%s", err, config)
	}
	server := NewServer(NewConfigManager(cfg, path, quietLogger(t)), "python3", quietLogger(t))
	for query, want := range map[string]string{
		"collector=textfile":                                                      "} 7",
		"collector=textfile&target=nightly":                                       "} 5",
		"collector=textfile&path=backup.prom":                                     "} 2",
		"collector=any_textfile&target=backup.prom":                               "} 2",
		"collector=textfile&target=" + url.QueryEscape("file://"+root+"/nightly"): "} 5",
	} {
		probeFile(t, server, query).must(t, http.StatusOK, want)
	}
	targets := writeIn(t, t.TempDir(), "targets.yaml", find("targets:\n"))
	file, err := LoadTargetFile(targets)
	if err == nil {
		err = file.Validate()
	}
	if err == nil {
		err = file.ValidateAgainst(cfg)
	}
	if err != nil {
		t.Fatal(err)
	}
}

// Reads left running on a filesystem that has stopped answering are capped per
// collector: once every slot is held, a probe fails at once instead of adding
// another goroutine, and the slots come back as the reads return.
func TestLocalFilePendingReadsAreCapped(t *testing.T) {
	root := t.TempDir()
	for i := 0; i <= localFileMaxPendingReads; i++ {
		writeIn(t, root, "f"+strconv.Itoa(i)+".prom", promFile)
	}
	server := fileServer(t, fileCollector("stuck", root, ""), fileCollector("other", root, ""))
	release := make(chan struct{})
	t.Cleanup(func() { afterLocalFileRead.Store(nil) })
	setReadHook(func(string) { <-release })

	// Different files, so the probes do not share one read.
	for i := 0; i < localFileMaxPendingReads; i++ {
		probeFile(t, server, "collector=stuck&timeout=20ms&target=f"+strconv.Itoa(i)+".prom").must(t, http.StatusBadGateway, context.DeadlineExceeded.Error())
	}
	if got := localFileReads.pending("stuck"); got != localFileMaxPendingReads {
		t.Fatalf("pending=%d, want %d", got, localFileMaxPendingReads)
	}
	start := time.Now()
	probeFile(t, server, "collector=stuck&timeout=5s&target=f"+strconv.Itoa(localFileMaxPendingReads)+".prom").must(t, http.StatusBadGateway, "already has 4 file reads that have not returned")
	if time.Since(start) > time.Second {
		t.Fatal("a probe over the cap waited instead of failing at once")
	}
	// The cap is per collector.
	if got := localFileReads.pending("other"); got != 0 {
		t.Fatalf("other collector pending=%d", got)
	}

	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for localFileReads.pending("stuck") > 0 {
		if time.Now().After(deadline) {
			t.Fatalf("pending reads never returned: %d", localFileReads.pending("stuck"))
		}
		time.Sleep(5 * time.Millisecond)
	}
	afterLocalFileRead.Store(nil)
	probeFile(t, server, "collector=stuck&target=f0.prom").must(t, http.StatusOK, "} 7")
}

// The budget bounds the whole trip, a file read included, and a probe without
// the header is unaffected.
func TestTheBudgetBoundsFileReadsAndIsOptional(t *testing.T) {
	root := t.TempDir()
	writeIn(t, root, "app.prom", promFile)
	server := fileServer(t, fileCollector("files", root, "app.prom"))
	server.SetTimeoutOffset(0)

	release := make(chan struct{})
	t.Cleanup(func() {
		close(release)
		afterLocalFileRead.Store(nil)
	})
	setReadHook(func(string) { <-release })
	recorder := probeWithScrapeTimeout(t, server, "collector=files", "0.2")
	if recorder.Code != http.StatusBadGateway || !strings.Contains(recorder.Body.String(), "200ms budget") {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body)
	}

	afterLocalFileRead.Store(nil)
	if recorder := probeOnce(t, server, "/probe?collector=files", nil); recorder.Code != http.StatusOK {
		t.Fatalf("without the header: status=%d body=%s", recorder.Code, recorder.Body)
	}
	if recorder := probeWithScrapeTimeout(t, server, "collector=files", "10"); recorder.Code != http.StatusOK {
		t.Fatalf("with a generous timeout: status=%d body=%s", recorder.Code, recorder.Body)
	}
}
