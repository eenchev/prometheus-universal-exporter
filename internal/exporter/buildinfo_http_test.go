//go:build !select_request_types || request_type_http

package exporter

import (
	"fmt"
	"runtime"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// http_exporter_build_info carries the build as labels, the sql type's
// drivers among them when the build has the type, and no sql_drivers label
// without a value when it has not.
func TestBuildInfoMetric(t *testing.T) {
	server := verboseServer(t, false, testutil.Collector("text", "text"))
	exposition := selfMetrics(t, server)
	b := BuildVersion()
	drivers := ""
	if built := fetch.BuiltSQLDrivers(); len(built) > 0 {
		drivers = fmt.Sprintf("sql_drivers=%q,", strings.Join(built, ","))
	}
	want := fmt.Sprintf(`http_exporter_build_info{goversion=%q,request_types=%q,revision=%q,%sversion=%q} 1`, runtime.Version(), strings.Join(fetch.BuiltRequestTypes(), ","), b.Revision, drivers, b.Version)
	if !strings.Contains(exposition, want) {
		t.Fatalf("missing %s in:\n%s", want, exposition)
	}
	if !strings.Contains(exposition, "# TYPE http_exporter_build_info gauge") {
		t.Fatal("http_exporter_build_info is not a gauge")
	}
}
