//go:build !select_request_types || (request_type_http && request_type_localfile)

package fetch

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

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
