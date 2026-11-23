//go:build !select_request_types || request_type_http

package exporter

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// A collector with transform.state gives its script the dict its last run
// for the target left, from one probe to the next; a debug probe reads the
// state and changes nothing of it, so the probe after it finds what the
// probe before it left. The self-metrics say what is kept of the
// collector's states, and nothing of a collector whose scripts keep none.
func TestAScriptsStateIsKeptFromOneProbeToTheNext(t *testing.T) {
	requirePython(t)
	testutil.CaptureLogs(t)
	target := textTarget(t, `{"total": 7}`)
	script := pythonCollector("counted", `state["runs"] = state.get("runs", 0) + 1
metric(name="runs", value=state["runs"])`)
	script.Transform.State = true
	server := modeServer(t, script, pythonCollector("stateless", `metric(name="v", value=1)`))
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
	metrics := probeOnce(t, server, "/self-metrics", nil).Body.String()
	for _, want := range []string{
		`http_exporter_python_states{collector="counted"} 1`,
		`http_exporter_python_state_bytes{collector="counted"} ` + strconv.Itoa(len(`{"runs": 3}`)),
		`http_exporter_python_states_dropped_total{collector="counted"} 0`,
	} {
		if !strings.Contains(metrics, want) {
			t.Errorf("no %s in the self-metrics", want)
		}
	}
	if strings.Contains(metrics, `http_exporter_python_states{collector="stateless"}`) {
		t.Error("a collector without transform.state has a series of states")
	}
}
