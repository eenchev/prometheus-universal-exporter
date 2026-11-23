//go:build !select_request_types || request_type_http

package exporter

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// A collector with transform.state gives its script the dict its last run
// for the target left, from one probe to the next; a debug probe reads the
// state and changes nothing of it, so the probe after it finds what the
// probe before it left.
func TestAScriptsStateIsKeptFromOneProbeToTheNext(t *testing.T) {
	requirePython(t)
	testutil.CaptureLogs(t)
	target := textTarget(t, `{"total": 7}`)
	script := pythonCollector("counted", `state["runs"] = state.get("runs", 0) + 1
metric(name="runs", value=state["runs"])`)
	script.Transform.State = true
	server := modeServer(t, script)
	server.SetDebug(DebugFull)
	probe := "/probe?collector=counted&target=" + url.QueryEscape(target.URL)
	for _, want := range []string{"runs 1", "runs 2"} {
		r := probeOnce(t, server, probe, nil)
		if got := strings.Join(seriesLines(r.Body.String()), "\n"); r.Code != http.StatusOK || got != want {
			t.Fatalf("%d %q, want %q", r.Code, got, want)
		}
	}
	if report := probeOnce(t, server, probe+"&debug=true", nil).Body.String(); !strings.Contains(report, "runs 3") {
		t.Fatalf("the debug probe's script did not read the state:\n%s", report)
	}
	r := probeOnce(t, server, probe, nil)
	if got := strings.Join(seriesLines(r.Body.String()), "\n"); got != "runs 3" {
		t.Fatalf("after a debug probe: %q, want runs 3", got)
	}
}
