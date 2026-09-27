//go:build !select_request_types || request_type_http

package exporter

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// examples/config.filebeat.json-test.yaml reads Filebeat's monitoring
// endpoint. testdata/json/filebeat-stats.json is a complete /stats answer,
// every section Filebeat's source registers present with realistic values,
// and testdata/json/filebeat-info.json its /. Unlike the demos of public services, Filebeat runs locally, so these run
// with the rest of the suite.

const filebeatConfig = "../../examples/config.filebeat.json-test.yaml"

// probeFilebeat serves stats at /stats and info at /, and probes collector.
func probeFilebeat(t *testing.T, collector string, stats, info []byte) string {
	t.Helper()
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := map[string][]byte{"/stats": stats, "/": info}[r.URL.Path]
		if body == nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=UTF-8")
		_, _ = w.Write(body)
	}))
	t.Cleanup(target.Close)
	cfg, err := config.Load(filebeatConfig)
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(config.NewManager(cfg, filebeatConfig, slog.Default()), "python3", slog.Default())
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet,
		"/probe?collector="+collector+"&target="+url.QueryEscape(target.URL), nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	return response.Body.String()
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile("../../testdata/json/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// samples returns the exposition's sample lines, without HELP and TYPE.
func samples(body string) []string {
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(body), "\n") {
		if line != "" && !strings.HasPrefix(line, "#") {
			out = append(out, line)
		}
	}
	return out
}

// Every section of a complete /stats answer becomes series, and nothing is
// logged: every rule found its value.
func TestTheFilebeatExampleReadsACompleteStatsAnswer(t *testing.T) {
	logs := testutil.CaptureLogs(t)
	body := probeFilebeat(t, "filebeat", readFixture(t, "filebeat-stats.json"), nil)
	got := samples(body)
	for _, want := range []string{
		// The process, milliseconds as seconds.
		`filebeat_uptime_seconds 604812.345`,
		`filebeat_cpu_seconds_total{mode="user"} 1456.593`,
		`filebeat_cpu_seconds_total{mode="system"} 412.371`,
		`filebeat_resident_memory_bytes 1.64790272e+08`,
		`filebeat_goroutines 143`,
		`filebeat_open_fds 57`,
		`filebeat_fds_limit{limit="soft"} 1.048576e+06`,
		// cgroup, nanoseconds as seconds, the quota as cores.
		`filebeat_cgroup_memory_limit_bytes 5.36870912e+08`,
		`filebeat_cgroup_cpu_seconds_total 1869.023450117`,
		`filebeat_cgroup_cpu_throttled_periods_total 118`,
		`filebeat_cgroup_cpu_quota_cores 2`,
		// Filebeat's own.
		`filebeat_events_active 812`,
		`filebeat_events_added_total 9.1462377e+07`,
		`filebeat_harvester_open_files 24`,
		`filebeat_input_log_files_total{event="truncated"} 2`,
		`filebeat_filestream_files{state="matched"} 27`,
		`filebeat_filestream_files{state="no_ingest_target"} 0`,
		`filebeat_filestream_scan_errors 0`,
		`filebeat_registrar_states_current 29`,
		`filebeat_registrar_writes_total{result="fail"} 0`,
		// Configuration reloading.
		`filebeat_config_modules_running 3`,
		`filebeat_config_modules_total{event="stop"} 2`,
		// The pipeline and its queue.
		`filebeat_pipeline_clients 9`,
		`filebeat_pipeline_events_total{outcome="filtered"} 9120`,
		`filebeat_pipeline_events_total{outcome="retry"} 2204`,
		`filebeat_queue_max_events 32000`,
		`filebeat_queue_filled_ratio 0.0254`,
		`filebeat_queue_events_total{stage="removed"} 9.1452445e+07`,
		// The output.
		`filebeat_output_info{type="elasticsearch"} 1`,
		`filebeat_output_events_sent_total 9.145601e+07`,
		`filebeat_output_events_total{outcome="acked"} 9.1452107e+07`,
		`filebeat_output_events_total{outcome="toomany"} 1966`,
		`filebeat_output_batches_split_total 6`,
		`filebeat_output_errors_total{direction="write"} 3`,
		// The host.
		`filebeat_system_load{period="15m"} 1.18`,
		`filebeat_system_load_per_core{period="1m"} 0.1775`,
	} {
		if !slices.Contains(got, want) {
			t.Errorf("missing %s", want)
		}
	}
	// Counts, not values, are the pipeline's and output's own totals; the
	// outcome families leave them and the gauges out.
	for _, unwanted := range []string{`outcome="total"`, `outcome="active"`, `outcome="batches"`, `state="scan_errors"`, `period="norm"`} {
		if strings.Contains(body, unwanted) {
			t.Errorf("exported %s", unwanted)
		}
	}
	if len(got) != 84 {
		t.Errorf("%d series, want 84:\n%s", len(got), strings.Join(got, "\n"))
	}
	if logs.Len() != 0 {
		t.Errorf("reading a complete answer logged:\n%s", logs)
	}
}

// A Filebeat that reports less — not on Linux, without a cgroup, without the
// log input or a filestream input, an older version — is read without
// failing and without logging: those series are simply absent.
func TestTheFilebeatExampleLeavesOutWhatFilebeatDoesNotReport(t *testing.T) {
	logs := testutil.CaptureLogs(t)
	var stats map[string]any
	if err := json.Unmarshal(readFixture(t, "filebeat-stats.json"), &stats); err != nil {
		t.Fatal(err)
	}
	section := func(path ...string) map[string]any {
		m := stats
		for _, key := range path {
			m = m[key].(map[string]any)
		}
		return m
	}
	delete(section("beat"), "cgroup")
	delete(section("beat"), "handles")
	delete(section("beat", "memstats"), "memory_sys")
	delete(section("filebeat"), "filestream")
	delete(section("filebeat"), "harvester")
	delete(section("filebeat"), "input")
	delete(section("libbeat", "output"), "batches")
	queue := section("libbeat", "pipeline", "queue")
	for key := range queue {
		if key != "acked" {
			delete(queue, key)
		}
	}
	reduced, err := json.Marshal(stats)
	if err != nil {
		t.Fatal(err)
	}
	body := probeFilebeat(t, "filebeat", reduced, nil)
	for _, absent := range []string{"filebeat_cgroup_", "filebeat_open_fds", "filebeat_fds_limit", "filebeat_go_memstats_sys_bytes",
		"filebeat_filestream_", "filebeat_harvester_", "filebeat_input_log_", "filebeat_output_batches_split_total",
		"filebeat_queue_max_", "filebeat_queue_filled_", "filebeat_queue_events_total"} {
		if strings.Contains(body, "\n"+absent) {
			t.Errorf("%s is exported for a section the answer does not have", absent)
		}
	}
	for _, present := range []string{"filebeat_events_active 812", "filebeat_registrar_states_current 29", "filebeat_queue_acked_events_total 9.1452445e+07", `filebeat_output_events_total{outcome="acked"} 9.1452107e+07`} {
		if !strings.Contains(body, present+"\n") {
			t.Errorf("missing %s", present)
		}
	}
	if logs.Len() != 0 {
		t.Errorf("reading an answer without the optional sections logged:\n%s", logs)
	}
}

// The info collector reads /: one series, 1, with the version and build.
func TestTheFilebeatExampleReadsWhoIsAnswering(t *testing.T) {
	testutil.CaptureLogs(t)
	body := probeFilebeat(t, "filebeat_info", nil, readFixture(t, "filebeat-info.json"))
	want := []string{`filebeat_build_info{beat="filebeat",binary_arch="amd64",build_commit="6f5c7b8a3e2d1c0b9a8f7e6d5c4b3a2918f7e6d5",name="logs-node-3",uuid="34f6c6e1-45a8-4b12-9125-11b3e6e89866",version="9.1.3"} 1`}
	if got := samples(body); !slices.Equal(got, want) {
		t.Fatalf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}
