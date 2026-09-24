//go:build !select_request_types || request_type_localfile

package fetch

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// fileCollector is a localfile collector passing a Prometheus text file
// through.
func fileCollector(name, root, path string) model.Collector {
	return model.Collector{
		Name:          name,
		Request:       model.RequestConfig{Type: RequestTypeLocalFile, Root: root, Path: path},
		Transform:     model.TransformConfig{Type: "prometheus"},
		ErrorHandling: model.ErrorHandling{OnFetchError: "fail", OnDecodeError: "fail", OnTransformError: "fail"},
	}
}

// Transforms see the file as a response: its content type from the
// extension, its size and its modification time.
func TestLocalFileResponseHeaders(t *testing.T) {
	root := t.TempDir()
	file := testutil.WriteIn(t, root, "status.JSON", `{"a":1}`)
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
	if !errors.Is(CheckTarget(ptr(testutil.Collector("web", "text")), "", false), ErrMissingTarget) {
		t.Error("an http probe without a target must be refused")
	}
}

func ptr[T any](v T) *T { return &v }
