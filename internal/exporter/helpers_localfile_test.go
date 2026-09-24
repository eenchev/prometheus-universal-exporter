//go:build !select_request_types || request_type_localfile

package exporter

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// A localfile collector reading a directory with request.files
// (fetch/localfile_directory.go, filebatch.go).

// dirCollector reads the files matching patterns under root, passing
// Prometheus text through.
func dirCollector(name, root string, patterns ...string) model.Collector {
	c := fileCollector(name, root, "")
	c.Request.Files = patterns
	return c
}

// fileCollector is a localfile collector passing a Prometheus text file
// through.
func fileCollector(name, root, path string) model.Collector {
	return model.Collector{
		Name:          name,
		Request:       model.RequestConfig{Type: fetch.RequestTypeLocalFile, Root: root, Path: path},
		Transform:     model.TransformConfig{Type: "prometheus"},
		ErrorHandling: model.ErrorHandling{OnFetchError: "fail", OnDecodeError: "fail", OnTransformError: "fail"},
	}
}

func mtime(t *testing.T, path string, at time.Time) {
	t.Helper()
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
}

// The localfile request type reads a file under request.root
// (fetch/requesttype_localfile.go).

const promFile = "# HELP app_jobs_total Jobs run.\n# TYPE app_jobs_total counter\napp_jobs_total{queue=\"default\"} 7\n"

func fileServer(t *testing.T, collectors ...model.Collector) *Server {
	t.Helper()
	cfg := &model.Config{Collectors: collectors}
	if err := config.Validate(cfg); err != nil {
		t.Fatal(err)
	}
	return NewServer(config.NewManager(cfg, "", testutil.QuietLogger(t)), "python3", testutil.QuietLogger(t))
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

func setReadHook(hook func(string)) { fetch.AfterLocalFileRead.Store(&hook) }
