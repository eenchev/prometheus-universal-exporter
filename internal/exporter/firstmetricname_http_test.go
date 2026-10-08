//go:build !select_request_types || request_type_http

package exporter

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// A target whose exposition names a metric in a megabyte, with a label value
// that is not valid UTF-8, has the value repaired and the repair logged
// before the name is refused: the warning names the metric by its first 200
// bytes and its length, in a line under a kilobyte where the line was the
// megabyte, whichever trip finds it — a probe, a static target's scrape, and
// a debug probe, whose report shows the line among its logs whole, not as a
// line cut at the report's 8,192 bytes. The failure log remembers nothing of
// the name: what it keeps of the two failures of the probe, the warning and
// the name's refusal, is under two kilobytes.
func TestAMegabyteMetricNameIsLoggedByItsStartByEveryTrip(t *testing.T) {
	testutil.CaptureLogs(t)
	name := strings.Repeat("n", 1<<20)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("# TYPE " + name + " gauge\n" + name + "{who=\"caf\xe9\"} 1\n"))
	}))
	t.Cleanup(target.Close)
	c := testutil.Collector("depot", "prometheus")
	c.Transform = model.TransformConfig{Type: "prometheus"}
	c.Metrics = nil
	c.Limits = model.Limits{MaxResponseBytes: 4 << 20}
	static := model.StaticTarget{Name: "yard", Collector: "depot", Target: target.URL}
	server := newStaticServer(t, &model.Config{Collectors: []model.Collector{c}}, &model.StaticTargetFile{Interval: model.Duration(60e9), Targets: []model.StaticTarget{static}})
	server.SetProbeDebug(true)
	logs := &bytes.Buffer{}
	server.logger = slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	want := strings.Repeat("n", 200) + "... (1048576 bytes)"
	warned := func(trip string) {
		t.Helper()
		size := logs.Len()
		lines := linesOf(t, logs, utf8Warning)
		if len(lines) != 1 || lines[0]["first_metric"] != want || lines[0]["values"] != float64(1) || lines[0]["collector"] != "depot" || lines[0]["target"] != target.URL {
			t.Fatalf("%s: the repair is logged as %.900v, want one warning that names the first metric %q", trip, lines, want)
		}
		if size > 2000 {
			t.Errorf("%s: the trip logged %d bytes, want under 2,000 for the warning and the name's refusal", trip, size)
		}
	}
	if answer := probeOnce(t, server, "/probe?collector=depot&target="+target.URL, nil); answer.Code != http.StatusBadGateway || !strings.Contains(answer.Body.String(), `"... (1048576 bytes): longer than limits.max_metric_name_length 200`) {
		t.Fatalf("the probe is answered %d %.600q, want the name's refusal", answer.Code, answer.Body.String())
	}
	warned("a probe")
	server.failures.mu.Lock()
	kept := 0
	for key, st := range server.failures.entries {
		kept += len(key) + len(st.key.bytes) + len(st.stage) + len(st.err)
	}
	if len(server.failures.entries) != 2 || kept > 2000 {
		t.Errorf("the failure log remembers %d failures of the probe in %d bytes, want the warning and the refusal in under 2,000", len(server.failures.entries), kept)
	}
	server.failures.mu.Unlock()

	server.scrapeTarget(t.Context(), server.manager.Get(), static)
	warned("a static target's scrape")

	report := probeOnce(t, server, "/probe?debug=true&collector=depot&target="+target.URL, nil).Body.String()
	var line string
	for _, of := range strings.Split(report, "\n") {
		if strings.Contains(of, "first_metric=") {
			line = of
		}
	}
	if !strings.HasSuffix(line, ` values=1 first_metric="`+want+`"`) || !strings.Contains(line, "level=WARN") || len(line) > 700 {
		t.Errorf("the report shows the warning as %d bytes, %.900q, want a line that ends first_metric=%q", len(line), line, want)
	}
	if lines := linesOf(t, logs, utf8Warning); len(lines) != 0 {
		t.Errorf("a debug probe's warning is in the exporter's log too: %.600v", lines)
	}
}
