//go:build !select_request_types || request_type_localfile

package repository

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/exporter"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

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
	path := testutil.WriteIn(t, t.TempDir(), "config.yaml", block)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("the example does not load: %v\n%s", err, block)
	}
	testutil.WriteIn(t, root, "backup.prom", "# TYPE backup_last_success_timestamp_seconds gauge\nbackup_last_success_timestamp_seconds 1.7e+09\n")
	server := exporter.NewServer(config.NewManager(cfg, "", testutil.QuietLogger(t)), "python3", testutil.QuietLogger(t))
	probeFile(t, server, "collector="+cfg.Collectors[0].Name).must(t, http.StatusOK, `backup_last_success_timestamp_seconds{file="backup.prom"}`)
}

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
	testutil.WriteIn(t, root, "batch.prom", promFile)
	testutil.WriteIn(t, root, "backup.prom", strings.Replace(promFile, "7", "2", 1))
	testutil.WriteIn(t, root, "nightly/batch.prom", strings.Replace(promFile, "7", "5", 1))
	conf := strings.ReplaceAll(find("collectors:\n")+find("  - name: any_textfile\n"), documentedRoot, root)
	conf += "otlp:\n  enabled: true\n  endpoint: http://collector.invalid/v1/metrics\n"
	path := testutil.WriteIn(t, t.TempDir(), "config.yaml", conf)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("%v\n%s", err, conf)
	}
	server := exporter.NewServer(config.NewManager(cfg, path, testutil.QuietLogger(t)), "python3", testutil.QuietLogger(t))
	for query, want := range map[string]string{
		"collector=textfile":                                                      "} 7",
		"collector=textfile&target=nightly":                                       "} 5",
		"collector=textfile&path=backup.prom":                                     "} 2",
		"collector=any_textfile&target=backup.prom":                               "} 2",
		"collector=textfile&target=" + url.QueryEscape("file://"+root+"/nightly"): "} 5",
	} {
		probeFile(t, server, query).must(t, http.StatusOK, want)
	}
	targets := testutil.WriteIn(t, t.TempDir(), "targets.yaml", find("interval: 1m\ntargets:\n"))
	file, err := config.LoadStaticTargets(targets)
	if err == nil {
		err = config.ValidateStaticTargets(file)
	}
	if err == nil {
		err = config.ValidateStaticTargetsAgainst(file, cfg)
	}
	if err != nil {
		t.Fatal(err)
	}
}

// The localfile request type reads a file under request.root
// (fetch/requesttype_localfile.go).

const promFile = "# HELP app_jobs_total Jobs run.\n# TYPE app_jobs_total counter\napp_jobs_total{queue=\"default\"} 7\n"

func probeFile(t *testing.T, server *exporter.Server, query string) *httpResult {
	t.Helper()
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/probe?"+query, nil))
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

// The carbon lines example in docs/GRAPHITE.md reads the file the page shows,
// its timestamps moved to now so max_age keeps them.
func TestCarbonLinesDocumentationExample(t *testing.T) {
	doc := read(t, "docs/GRAPHITE.md")
	_, section, ok := strings.Cut(doc, "## Carbon lines from a file")
	if !ok {
		t.Fatal("docs/GRAPHITE.md has no Carbon lines from a file section")
	}
	_, lines, _ := strings.Cut(section, "```text\n")
	lines, _, _ = strings.Cut(lines, "```")
	_, conf, _ := strings.Cut(section, "```yaml\n")
	conf, _, _ = strings.Cut(conf, "```")
	root := t.TempDir()
	testutil.WriteIn(t, root, "backup.graphite", strings.ReplaceAll(lines, "1727000000", strconv.FormatInt(time.Now().Unix()-60, 10)))
	conf = strings.ReplaceAll(conf, "/var/lib/metrics", root)
	path := testutil.WriteIn(t, t.TempDir(), "config.yaml", conf)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("%v\n%s", err, conf)
	}
	server := exporter.NewServer(config.NewManager(cfg, path, testutil.QuietLogger(t)), "python3", testutil.QuietLogger(t))
	probeFile(t, server, "collector=backups").must(t, http.StatusOK, `backup_duration_seconds{host="web01"} 42`, `backup_duration_seconds{host="web02"} 17`)
}
