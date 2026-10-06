//go:build !select_request_types || request_type_localfile

package exporter

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// End to end: a static target of a localfile collector may name no target
// and read the collector's own path. Its health series were served, and
// exported over OTLP, with target="", and are served without the label; a
// target that names a directory under the root keeps it, and the file's
// own label with an empty value is left off the target's series.
func TestAStaticTargetWithoutATargetIsServedWithoutATargetLabel(t *testing.T) {
	root := t.TempDir()
	testutil.WriteIn(t, root, "app.prom", "# TYPE jobs gauge\njobs{queue=\"\",kind=\"batch\"} 7\n")
	testutil.WriteIn(t, root, "batch/app.prom", "# TYPE jobs gauge\njobs{queue=\"default\"} 3\n")
	cfg := &model.Config{Collectors: []model.Collector{fileCollector("files", root, "app.prom")}, OTLP: otlpConfig("http://collector.invalid/v1/metrics")}
	file := &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{
		{ExportViaOTLP: true, Name: "main", Collector: "files"},
		{ExportViaOTLP: true, Name: "batch", Collector: "files", Target: "batch"},
	}}
	server := newStaticServer(t, cfg, file)
	server.logger = testutil.QuietLogger(t)
	server.scrapeStaticTargets(context.Background(), 10*time.Second)
	body := probeOnce(t, server, "/static-targets", nil).Body.String()
	for _, want := range []string{
		"jobs{kind=\"batch\",static_target=\"main\"} 7\n", "jobs{queue=\"default\",static_target=\"batch\"} 3\n",
		"http_exporter_target_up{collector=\"files\",static_target=\"main\"} 1\n", "http_exporter_target_up{collector=\"files\",static_target=\"batch\",target=\"batch\"} 1\n",
		"http_exporter_target_scrape_duration_seconds{collector=\"files\",static_target=\"main\"} ", "http_exporter_target_last_success_timestamp_seconds{collector=\"files\",static_target=\"main\"} ",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the static targets endpoint lacks %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, `=""`) {
		t.Errorf("the static targets endpoint serves a label with an empty value:\n%s", body)
	}
	exported := 0
	for _, resource := range server.drainOTLP() {
		for _, m := range resource.Set.Metrics {
			exported++
			for name, value := range m.Labels {
				if value == "" {
					t.Errorf("%s is queued for OTLP with the label %s empty: %v", m.Name, name, m.Labels)
				}
			}
		}
	}
	if exported < 8 {
		t.Errorf("%d series of the targets were queued for OTLP", exported)
	}
}
