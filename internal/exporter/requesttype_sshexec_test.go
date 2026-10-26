//go:build request_type_sshexec

package exporter

import (
	"net/http"
	"strings"
	"testing"
)

// TODO(sshexec): the type's tests through the exporter, as the other types'
// requesttype_<name>_test.go here have: a probe of a collector, its label
// values filled from the probe's parameters, and a python transform reading
// its answer. This one holds until the type's Fetch is written, and goes
// then.

// Until the sshexec type is written, a probe of a collector of it fails,
// saying so, whatever reads its answer, a python transform included.
func TestAProbeOfASshexecCollectorFailsUntilTheTypeIsWritten(t *testing.T) {
	requirePython(t)
	server := labelServer(t, `collectors:
  - name: demo
    request:
      type: sshexec
    transform:
      type: python
      script: |
        metric(name="demo", type="gauge", value=1)
    metrics: []
`)
	recorder := probeOnce(t, server, "/probe?collector=demo&target=demo", nil)
	if body := recorder.Body.String(); recorder.Code == http.StatusOK || !strings.Contains(body, "not implemented yet") {
		t.Errorf("%d %v, want the scrape to fail as not implemented", recorder.Code, seriesLines(body))
	}
}
