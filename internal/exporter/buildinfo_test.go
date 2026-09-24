package exporter

import (
	"fmt"
	"runtime"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

func TestBuildInfoMetric(t *testing.T) {
	server := verboseServer(t, false, testutil.Collector("text", "text"))
	exposition := selfMetrics(t, server)
	b := BuildVersion()
	want := fmt.Sprintf(`http_exporter_build_info{goversion=%q,request_types=%q,revision=%q,version=%q} 1`, runtime.Version(), strings.Join(fetch.BuiltRequestTypes(), ","), b.Revision, b.Version)
	if !strings.Contains(exposition, want) {
		t.Fatalf("missing %s in:\n%s", want, exposition)
	}
	if !strings.Contains(exposition, "# TYPE http_exporter_build_info gauge") {
		t.Fatal("http_exporter_build_info is not a gauge")
	}
}

// A version set at build time wins over what Go stamped.
func TestASetVersionWins(t *testing.T) {
	previous := Version
	Version = "9.9.9"
	t.Cleanup(func() { Version = previous })
	if info := computeBuildVersion(); info.Version != "9.9.9" {
		t.Fatalf("version=%q, want the one set at build time", info.Version)
	}
}
