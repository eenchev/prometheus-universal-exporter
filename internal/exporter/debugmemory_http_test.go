//go:build !select_request_types || request_type_http

package exporter

import (
	"net/http"
	"net/url"
	"regexp"
	"runtime/metrics"
	"strconv"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// A debug probe's report says what the transform took of memory beside the
// limit it is held to, which is how a limit is chosen: what a jq
// transform's expressions held at their most, and the most address space a
// Python script's worker has had. A probe that is no debug probe measures
// nothing: it asks for no garbage collection.
func TestADebugProbeSaysWhatItsTransformTookOfMemory(t *testing.T) {
	requirePython(t)
	testutil.CaptureLogs(t)
	target := textTarget(t, `{"a":1}`)
	jq := model.Collector{Name: "expressions", Request: model.RequestConfig{Type: fetch.RequestTypeHTTP}, Decoder: model.DecoderConfig{Type: "json"},
		Transform: model.TransformConfig{Type: "jq"}, Limits: model.Limits{MaxExpressionMemory: 1 << 30},
		// A list of two million numbers, some 48 MiB, kept until the rule
		// has its value.
		Metrics: []model.MetricRule{{Name: "n", Type: model.GaugeMetricType, Expression: "[range(2000000)] | length"}}}
	script := pythonCollector("script", `metric(name="v", value=len(bytearray(8 << 20)))`)
	script.Limits.MaxScriptMemory = 512 << 20
	server := modeServer(t, jq, script)
	server.SetDebug(DebugFull)
	forced := func() uint64 {
		sample := []metrics.Sample{{Name: "/gc/cycles/forced:gc-cycles"}}
		metrics.Read(sample)
		return sample[0].Value.Uint64()
	}
	probeURL := func(collector, more string) string {
		return "/probe?collector=" + collector + "&target=" + url.QueryEscape(target.URL) + more
	}
	before := forced()
	for _, collector := range []string{"expressions", "script"} {
		if r := probeOnce(t, server, probeURL(collector, ""), nil); r.Code != http.StatusOK {
			t.Fatalf("%s answered %d: %s", collector, r.Code, r.Body)
		}
	}
	if ran := forced() - before; ran != 0 {
		t.Errorf("%d garbage collections were asked for by two probes that are no debug probes", ran)
	}
	number := func(report, attr string) int64 {
		t.Helper()
		found := regexp.MustCompile(attr + `=(\d+)`).FindStringSubmatch(report)
		if found == nil {
			t.Fatalf("no %s in the report:\n%s", attr, report)
		}
		n, _ := strconv.ParseInt(found[1], 10, 64)
		return n
	}
	report := probeOnce(t, server, probeURL("expressions", "&debug=true"), nil).Body.String()
	if !strings.Contains(report, "jq transform memory") {
		t.Fatalf("the report of a jq transform says nothing of its memory:\n%s", report)
	}
	if held, limit := number(report, "held_bytes"), number(report, "limit_bytes"); held < 16<<20 || held > 1<<30 || limit != 1<<30 {
		t.Errorf("the expressions are said to have held %d bytes under a limit of %d; they held a list of two million numbers", held, limit)
	}
	report = probeOnce(t, server, probeURL("script", "&debug=true"), nil).Body.String()
	if !strings.Contains(report, "python transform memory") {
		t.Fatalf("the report of a Python transform says nothing of its memory:\n%s", report)
	}
	if peak, limit := number(report, "worker_peak_bytes"), number(report, "limit_bytes"); peak < 16<<20 || peak > 512<<20 || limit != 512<<20 {
		t.Errorf("the worker is said to have had %d bytes of address space under a limit of %d; its script made 8 MiB", peak, limit)
	}
}
