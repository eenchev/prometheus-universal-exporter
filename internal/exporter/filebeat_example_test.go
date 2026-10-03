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
	"sync"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// examples/config.filebeat.json-test.yaml reads Filebeat's monitoring
// endpoint. testdata/json/filebeat-stats.json is a complete /stats answer,
// every section Filebeat's source registers present with realistic values,
// testdata/json/filebeat-stats-kafka.json the same Filebeat publishing to
// Kafka, and testdata/json/filebeat-info.json its /. Unlike the demos of public services, Filebeat runs locally, so these run
// with the rest of the suite.
//
// testdata/json/filebeat-inputs.json is its /inputs/: two filestream inputs,
// a tcp input and a journald one. It is written from the Beats source at
// version 9.6.0 (commit b5529c228d2b of 25 September 2026), not captured
// from a running Filebeat: the array of one object per input with "id" and
// "input" is what libbeat/monitoring/inputmon serves, the events_pipeline_*
// counters are the ones filebeat/input/v2 registers for every input, the
// filestream metrics those of filebeat/input/filestream/internal/input-logfile,
// the tcp ones those of filebeat/input/netmetrics, and the fields of
// "histogram" the ones filebeat/tests/integration/filestream_gzip_test.go
// decodes from this endpoint.

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
	// The Kafka output's own metrics are absent with Elasticsearch: no
	// series, and nothing logged for them either.
	for _, unwanted := range []string{`outcome="total"`, `outcome="active"`, `outcome="batches"`, `state="scan_errors"`, `period="norm"`, "filebeat_output_kafka_"} {
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

// With the Kafka output, its client's own metrics from libbeat.outputs
// become series too: bytes, requests in flight, requests and their latency
// in seconds. The generic output series stay, with type="kafka".
func TestTheFilebeatExampleReadsTheKafkaOutput(t *testing.T) {
	logs := testutil.CaptureLogs(t)
	body := probeFilebeat(t, "filebeat", readFixture(t, "filebeat-stats-kafka.json"), nil)
	got := samples(body)
	for _, want := range []string{
		`filebeat_output_info{type="kafka"} 1`,
		`filebeat_output_events_total{outcome="acked"} 9.1452107e+07`,
		`filebeat_output_events_total{outcome="failed"} 1180`,
		`filebeat_output_write_bytes_total 0`,
		`filebeat_output_kafka_write_bytes_total 6.1840227915e+10`,
		`filebeat_output_kafka_read_bytes_total 2.251739e+07`,
		`filebeat_output_kafka_requests_in_flight 2`,
		`filebeat_output_kafka_requests_total 1.146372e+06`,
		`filebeat_output_kafka_request_latency_seconds{quantile="0.5"} 0.005`,
		`filebeat_output_kafka_request_latency_seconds{quantile="0.75"} 0.008`,
		`filebeat_output_kafka_request_latency_seconds{quantile="0.95"} 0.0175`,
		`filebeat_output_kafka_request_latency_seconds{quantile="0.99"} 0.04127`,
		`filebeat_output_kafka_request_latency_seconds{quantile="0.999"} 0.188921`,
		`filebeat_output_kafka_request_latency_mean_seconds 0.006843`,
		`filebeat_output_kafka_request_latency_max_seconds 0.412`,
	} {
		if !slices.Contains(got, want) {
			t.Errorf("missing %s", want)
		}
	}
	if len(got) != 84+11 {
		t.Errorf("%d series, want %d:\n%s", len(got), 84+11, strings.Join(got, "\n"))
	}
	if logs.Len() != 0 {
		t.Errorf("reading a Kafka answer logged:\n%s", logs)
	}
}

// The Kafka client registers its metrics when it first reaches a broker, and
// can register some before others: whatever is there is read, the rest is
// absent without logging.
func TestTheFilebeatExampleReadsWhatTheKafkaOutputRegisteredSoFar(t *testing.T) {
	logs := testutil.CaptureLogs(t)
	var stats map[string]any
	if err := json.Unmarshal(readFixture(t, "filebeat-stats-kafka.json"), &stats); err != nil {
		t.Fatal(err)
	}
	outputs := stats["libbeat"].(map[string]any)["outputs"].(map[string]any)
	delete(outputs, "kafka")
	delete(outputs["write"].(map[string]any), "latency")
	partial, err := json.Marshal(stats)
	if err != nil {
		t.Fatal(err)
	}
	body := probeFilebeat(t, "filebeat", partial, nil)
	if !strings.Contains(body, "\nfilebeat_output_kafka_write_bytes_total 6.1840227915e+10\n") {
		t.Error("missing filebeat_output_kafka_write_bytes_total")
	}
	for _, absent := range []string{"filebeat_output_kafka_requests_", "filebeat_output_kafka_request_latency_"} {
		if strings.Contains(body, "\n"+absent) {
			t.Errorf("%s is exported for a metric the answer does not have", absent)
		}
	}
	if logs.Len() != 0 {
		t.Errorf("reading a partial Kafka answer logged:\n%s", logs)
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

// probeFilebeatInputs serves inputs at /inputs/ and nowhere else, as
// Filebeat's route does, probes the filebeat_inputs collector, and returns
// the exposition with the requests the stand-in received, as "METHOD URI".
func probeFilebeatInputs(t *testing.T, inputs []byte) (string, []string) {
	t.Helper()
	var (
		mu       sync.Mutex
		requests []string
	)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, r.Method+" "+r.RequestURI)
		mu.Unlock()
		if r.URL.Path != "/inputs/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write(inputs)
	}))
	t.Cleanup(target.Close)
	cfg, err := config.Load(filebeatConfig)
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(config.NewManager(cfg, filebeatConfig, slog.Default()), "python3", slog.Default())
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet,
		"/probe?collector=filebeat_inputs&target="+url.QueryEscape(target.URL), nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	return response.Body.String(), slices.Clone(requests)
}

// filebeatInputs returns the inputs of testdata/json/filebeat-inputs.json
// that keep says to keep, as a /inputs/ answer.
func filebeatInputs(t *testing.T, keep func(input map[string]any) bool) []byte {
	t.Helper()
	var inputs []map[string]any
	if err := json.Unmarshal(readFixture(t, "filebeat-inputs.json"), &inputs); err != nil {
		t.Fatal(err)
	}
	kept := []map[string]any{}
	for _, input := range inputs {
		if keep(input) {
			kept = append(kept, input)
		}
	}
	answer, err := json.Marshal(kept)
	if err != nil {
		t.Fatal(err)
	}
	return answer
}

// The inputs collector asks for /inputs/, with its slash, and every input of
// the answer becomes series with its id and type: the pipeline counters of
// all four, the file, message, byte, event and error counters and the open
// files of the two filestream inputs, and the processing time, nanoseconds
// as seconds, of the three that report one. Nothing is logged: the metrics
// the tcp and journald inputs do not have are simply absent for them.
func TestTheFilebeatExampleReadsEveryInput(t *testing.T) {
	logs := testutil.CaptureLogs(t)
	body, requests := probeFilebeatInputs(t, readFixture(t, "filebeat-inputs.json"))
	if want := []string{"GET /inputs/"}; !slices.Equal(requests, want) {
		t.Errorf("the collector requested %q, want %q", requests, want)
	}
	const (
		containers = `id="kubernetes-container-logs",input="filestream"`
		nginx      = `id="nginx-access",input="filestream"`
		syslog     = `id="syslog-tcp",input="tcp"`
		journald   = `id="journald-system::LOCAL_SYSTEM_JOURNAL",input="journald"`
	)
	want := []string{
		// What every input reports.
		`filebeat_input_pipeline_events_added_total{` + containers + `} 8.8120456e+07`,
		`filebeat_input_pipeline_events_added_total{` + nginx + `} 7.120044e+06`,
		`filebeat_input_pipeline_events_added_total{` + syslog + `} 3.341921e+06`,
		`filebeat_input_pipeline_events_added_total{` + journald + `} 221957`,
		`filebeat_input_pipeline_events_total{` + containers + `,outcome="filtered"} 9120`,
		`filebeat_input_pipeline_events_total{` + containers + `,outcome="published"} 8.8111336e+07`,
		`filebeat_input_pipeline_events_total{` + nginx + `,outcome="filtered"} 0`,
		`filebeat_input_pipeline_events_total{` + nginx + `,outcome="published"} 7.120044e+06`,
		`filebeat_input_pipeline_events_total{` + syslog + `,outcome="filtered"} 0`,
		`filebeat_input_pipeline_events_total{` + syslog + `,outcome="published"} 3.341921e+06`,
		`filebeat_input_pipeline_events_total{` + journald + `,outcome="filtered"} 310`,
		`filebeat_input_pipeline_events_total{` + journald + `,outcome="published"} 221647`,
		// What a filestream input reports.
		`filebeat_input_files_total{event="opened",` + containers + `} 4073`,
		`filebeat_input_files_total{event="closed",` + containers + `} 4051`,
		`filebeat_input_files_total{event="opened",` + nginx + `} 63`,
		`filebeat_input_files_total{event="closed",` + nginx + `} 61`,
		`filebeat_input_files_active{` + containers + `} 22`,
		`filebeat_input_files_active{` + nginx + `} 2`,
		`filebeat_input_messages_read_total{` + containers + `} 8.8120991e+07`,
		`filebeat_input_messages_read_total{` + nginx + `} 7.120044e+06`,
		`filebeat_input_messages_truncated_total{` + containers + `} 17`,
		`filebeat_input_messages_truncated_total{` + nginx + `} 0`,
		`filebeat_input_bytes_processed_total{` + containers + `} 4.8211905331e+10`,
		`filebeat_input_bytes_processed_total{` + nginx + `} 1.930412877e+09`,
		`filebeat_input_events_processed_total{` + containers + `} 8.8120456e+07`,
		`filebeat_input_events_processed_total{` + nginx + `} 7.120044e+06`,
		`filebeat_input_processing_errors_total{` + containers + `} 4`,
		`filebeat_input_processing_errors_total{` + nginx + `} 0`,
		// The processing time of the inputs that measure one.
		`filebeat_input_processing_time_seconds{` + containers + `,quantile="0.5"} 0.00018432`,
		`filebeat_input_processing_time_seconds{` + containers + `,quantile="0.75"} 0.000251904`,
		`filebeat_input_processing_time_seconds{` + containers + `,quantile="0.95"} 0.000612352`,
		`filebeat_input_processing_time_seconds{` + containers + `,quantile="0.99"} 0.0018432`,
		`filebeat_input_processing_time_seconds{` + containers + `,quantile="0.999"} 0.0124928`,
		`filebeat_input_processing_time_seconds{` + nginx + `,quantile="0.5"} 0.00012288`,
		`filebeat_input_processing_time_seconds{` + nginx + `,quantile="0.75"} 0.00016384`,
		`filebeat_input_processing_time_seconds{` + nginx + `,quantile="0.95"} 0.00036864`,
		`filebeat_input_processing_time_seconds{` + nginx + `,quantile="0.99"} 0.0009216`,
		`filebeat_input_processing_time_seconds{` + nginx + `,quantile="0.999"} 0.00512`,
		`filebeat_input_processing_time_seconds{` + syslog + `,quantile="0.5"} 5.12e-05`,
		`filebeat_input_processing_time_seconds{` + syslog + `,quantile="0.75"} 6.656e-05`,
		`filebeat_input_processing_time_seconds{` + syslog + `,quantile="0.95"} 0.00014336`,
		`filebeat_input_processing_time_seconds{` + syslog + `,quantile="0.99"} 0.0004096`,
		`filebeat_input_processing_time_seconds{` + syslog + `,quantile="0.999"} 0.003072`,
		`filebeat_input_processing_time_mean_seconds{` + containers + `} 0.0002365185`,
		`filebeat_input_processing_time_mean_seconds{` + nginx + `} 0.00014899275`,
		`filebeat_input_processing_time_mean_seconds{` + syslog + `} 6.195225e-05`,
		`filebeat_input_processing_time_max_seconds{` + containers + `} 0.048211456`,
		`filebeat_input_processing_time_max_seconds{` + nginx + `} 0.009437184`,
		`filebeat_input_processing_time_max_seconds{` + syslog + `} 0.007340032`,
	}
	got := samples(body)
	for _, series := range want {
		if !slices.Contains(got, series) {
			t.Errorf("missing %s", series)
		}
	}
	if len(got) != len(want) {
		t.Errorf("%d series, want %d:\n%s", len(got), len(want), strings.Join(got, "\n"))
	}
	if logs.Len() != 0 {
		t.Errorf("reading a complete answer logged:\n%s", logs)
	}
}

// Inputs of other types than filestream have none of its metrics: the tcp
// input keeps its pipeline counters and its processing time, the journald
// one its pipeline counters alone, the filestream families are not exported
// at all, and nothing is logged.
func TestTheFilebeatExampleReadsInputsWithoutTheFilestreamMetrics(t *testing.T) {
	logs := testutil.CaptureLogs(t)
	body, _ := probeFilebeatInputs(t, filebeatInputs(t, func(input map[string]any) bool {
		return input["input"] != "filestream"
	}))
	for _, absent := range []string{"filebeat_input_files_", "filebeat_input_messages_", "filebeat_input_bytes_processed_total",
		"filebeat_input_events_processed_total", "filebeat_input_processing_errors_total", `input="filestream"`} {
		if strings.Contains(body, absent) {
			t.Errorf("%s is exported for inputs that do not report it", absent)
		}
	}
	got := samples(body)
	for _, want := range []string{
		`filebeat_input_pipeline_events_added_total{id="journald-system::LOCAL_SYSTEM_JOURNAL",input="journald"} 221957`,
		`filebeat_input_pipeline_events_total{id="journald-system::LOCAL_SYSTEM_JOURNAL",input="journald",outcome="filtered"} 310`,
		`filebeat_input_pipeline_events_total{id="journald-system::LOCAL_SYSTEM_JOURNAL",input="journald",outcome="published"} 221647`,
		`filebeat_input_pipeline_events_added_total{id="syslog-tcp",input="tcp"} 3.341921e+06`,
		`filebeat_input_processing_time_seconds{id="syslog-tcp",input="tcp",quantile="0.99"} 0.0004096`,
		`filebeat_input_processing_time_max_seconds{id="syslog-tcp",input="tcp"} 0.007340032`,
	} {
		if !slices.Contains(got, want) {
			t.Errorf("missing %s", want)
		}
	}
	// Three pipeline series each, and the tcp input's five quantiles, mean
	// and max.
	if len(got) != 3+3+7 {
		t.Errorf("%d series, want %d:\n%s", len(got), 3+3+7, strings.Join(got, "\n"))
	}
	if logs.Len() != 0 {
		t.Errorf("reading inputs without the filestream metrics logged:\n%s", logs)
	}
}

// A Filebeat too old to count each input's pipeline events reports the
// inputs without those counters: the rest is read, and nothing is logged.
func TestTheFilebeatExampleReadsInputsWithoutThePipelineCounters(t *testing.T) {
	logs := testutil.CaptureLogs(t)
	body, _ := probeFilebeatInputs(t, filebeatInputs(t, func(input map[string]any) bool {
		for key := range input {
			if strings.HasPrefix(key, "events_pipeline_") {
				delete(input, key)
			}
		}
		return true
	}))
	if strings.Contains(body, "filebeat_input_pipeline_") {
		t.Errorf("pipeline counters are exported for inputs that do not report them:\n%s", body)
	}
	got := samples(body)
	if want := `filebeat_input_files_active{id="nginx-access",input="filestream"} 2`; !slices.Contains(got, want) {
		t.Errorf("missing %s", want)
	}
	// The complete answer's 49 series without the 12 pipeline ones; the
	// journald input is left with none.
	if len(got) != 49-12 {
		t.Errorf("%d series, want %d:\n%s", len(got), 49-12, strings.Join(got, "\n"))
	}
	if logs.Len() != 0 {
		t.Errorf("reading inputs without the pipeline counters logged:\n%s", logs)
	}
}

// A Filebeat none of whose inputs registers metrics answers /inputs/ with an
// empty array. The probe succeeds with no series, and nothing is logged:
// every rule of the collector is optional.
func TestTheFilebeatExampleReadsAFilebeatWithoutInputs(t *testing.T) {
	logs := testutil.CaptureLogs(t)
	body, requests := probeFilebeatInputs(t, []byte("[]\n"))
	if want := []string{"GET /inputs/"}; !slices.Equal(requests, want) {
		t.Errorf("the collector requested %q, want %q", requests, want)
	}
	if body != "" {
		t.Errorf("an empty array gave an exposition:\n%s", body)
	}
	if logs.Len() != 0 {
		t.Errorf("reading an empty array logged:\n%s", logs)
	}
}
