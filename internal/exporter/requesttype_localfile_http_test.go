//go:build !select_request_types || (request_type_http && request_type_localfile)

package exporter

import (
	"net/http"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

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
