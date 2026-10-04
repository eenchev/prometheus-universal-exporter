//go:build !select_request_types || request_type_http

package exporter

// Benchmarks of a whole probe: the request to a target on this machine, the
// decode, the transform, validation and the answer, for each transform over
// the same items written in the format it reads. They are what a change to
// the pipeline's speed is measured with (docs/DEVELOPMENT.md):
//
//	go test -run '^$' -bench 'Probe' -benchtime 2s ./internal/exporter/
//
// n is the number of items the target answers with; a collector makes one
// series of each, jq_items two. jq_cached is answered from the cache, so it
// measures writing an answer alone, and /gzip writing it compressed.
// jq_detected and jq_sniffed are jq_items with the decoder left to the
// response: named by its Content-Type, and found from its content under
// text/plain, where the body is read to see that it is JSON. prescript is
// jq_items after a pre-script that passes the items on, so it measures
// handing a document to a Python worker and reading one back.

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
)

const probeBenchConfig = `
collectors:
  - name: jq_items
    request: {type: http, path: /json}
    decoder: {type: json}
    transform: {type: jq}
    limits: {max_metrics: 100000, max_response_bytes: 64MiB}
    metrics:
      - name: item_value
        items: .items[]
        expression: .value
        labels:
          - {name: id, expression: .id}
          - {name: region, expression: .region}
          - {name: kind, expression: .kind}
      - name: item_size_bytes
        items: .items[]
        expression: .size
        labels:
          - {name: id, expression: .id}
  - name: jq_detected
    request: {type: http, path: /json}
    transform: {type: jq}
    limits: {max_metrics: 100000, max_response_bytes: 64MiB}
    metrics:
      - name: item_value
        items: .items[]
        expression: .value
        labels:
          - {name: id, expression: .id}
          - {name: region, expression: .region}
          - {name: kind, expression: .kind}
      - name: item_size_bytes
        items: .items[]
        expression: .size
        labels:
          - {name: id, expression: .id}
  - name: jq_sniffed
    request: {type: http, path: /json-as-text}
    transform: {type: jq}
    limits: {max_metrics: 100000, max_response_bytes: 64MiB}
    metrics:
      - name: item_value
        items: .items[]
        expression: .value
        labels:
          - {name: id, expression: .id}
          - {name: region, expression: .region}
          - {name: kind, expression: .kind}
      - name: item_size_bytes
        items: .items[]
        expression: .size
        labels:
          - {name: id, expression: .id}
  - name: jq_cached
    cache: {ttl: 1m}
    request: {type: http, path: /json}
    decoder: {type: json}
    transform: {type: jq}
    limits: {max_metrics: 100000, max_response_bytes: 64MiB}
    metrics:
      - name: item_value
        items: .items[]
        expression: .value
        labels:
          - {name: id, expression: .id}
  - name: prom
    request: {type: http, path: /prom}
    transform: {type: prometheus}
    limits: {max_metrics: 100000, max_response_bytes: 64MiB}
  - name: prom_rules
    request: {type: http, path: /prom}
    transform: {type: prometheus}
    limits: {max_metrics: 100000, max_response_bytes: 64MiB}
    metrics:
      - name: node_cpu_seconds_total
      - name: node_filesystem_avail_bytes
        labels:
          - {name: site, value: a}
  - name: regex
    request: {type: http, path: /text}
    transform: {type: regex}
    limits: {max_metrics: 100000, max_response_bytes: 64MiB}
    metrics:
      - name: line_value
        expression: '(?m)^item value=(\d+) id=(?P<id>\S+) region=(?P<region>\S+)$'
        labels:
          - {name: id, expression: id}
          - {name: region, expression: region}
  - name: csv
    request: {type: http, path: /csv}
    decoder: {type: csv}
    transform: {type: csv}
    limits: {max_metrics: 100000, max_response_bytes: 64MiB}
    metrics:
      - name: row_value
        expression: value
        labels:
          - {name: id, expression: id}
          - {name: region, expression: region}
  - name: xpath
    request: {type: http, path: /xml}
    decoder: {type: xml}
    transform: {type: xpath}
    limits: {max_metrics: 100000, max_response_bytes: 64MiB}
    metrics:
      - name: node_value
        expression: //item/value
        labels:
          - {name: id, expression: "../@id"}
          - {name: region, expression: ../region}
  - name: css
    request: {type: http, path: /html}
    decoder: {type: html}
    transform: {type: css}
    limits: {max_metrics: 100000, max_response_bytes: 64MiB}
    metrics:
      - name: row_value
        items: tr.item
        expression: td.value
        labels:
          - {name: id, expression: td.id}
  - name: python
    request: {type: http, path: /json}
    decoder: {type: json}
    limits: {max_metrics: 100000, max_response_bytes: 64MiB, script_timeout: 10s}
    transform:
      type: python
      script: |
        for item in data["items"]:
            metric("item_value", value=item["value"], labels={"id": item["id"], "region": item["region"]})
  - name: prescript
    request: {type: http, path: /json}
    decoder: {type: json}
    limits: {max_metrics: 100000, max_response_bytes: 64MiB, max_output_bytes: 16MiB, script_timeout: 10s}
    transform:
      type: jq
      pre_script: |
        data = {"items": [{"id": item["id"], "region": item["region"], "value": item["value"]} for item in data["items"]]}
    metrics:
      - name: item_value
        items: .items[]
        expression: .value
        labels:
          - {name: id, expression: .id}
          - {name: region, expression: .region}
`

func probeBenchBodies(n int) map[string]string {
	var js, prom, text, csv, xml, html strings.Builder
	js.WriteString(`{"items":[`)
	csv.WriteString("id,region,value,size\n")
	xml.WriteString("<items>")
	html.WriteString("<html><body><table>")
	for i := 0; i < n; i++ {
		if i > 0 {
			js.WriteString(",")
		}
		fmt.Fprintf(&js, `{"id":"item-%d","region":"eu-%d","kind":"k%d","value":%d,"size":%d,"extra":{"a":1,"b":"two","c":[1,2,3]}}`, i, i%7, i%3, i, i*1024)
		fmt.Fprintf(&text, "item value=%d id=item-%d region=eu-%d\n", i, i, i%7)
		fmt.Fprintf(&csv, "item-%d,eu-%d,%d,%d\n", i, i%7, i, i*1024)
		fmt.Fprintf(&xml, `<item id="item-%d"><region>eu-%d</region><value>%d</value></item>`, i, i%7, i)
		fmt.Fprintf(&html, `<tr class="item"><td class="id">item-%d</td><td class="value">%d</td></tr>`, i, i)
	}
	js.WriteString("]}")
	xml.WriteString("</items>")
	html.WriteString("</table></body></html>")
	// A node_exporter-like exposition: n series over a few families.
	prom.WriteString("# HELP node_cpu_seconds_total Seconds the CPUs spent in each mode.\n# TYPE node_cpu_seconds_total counter\n")
	for i := 0; i < n/2; i++ {
		fmt.Fprintf(&prom, "node_cpu_seconds_total{cpu=\"%d\",mode=\"m%d\"} %d.25\n", i/8, i%8, i*3)
	}
	prom.WriteString("# HELP node_filesystem_avail_bytes Filesystem space available.\n# TYPE node_filesystem_avail_bytes gauge\n")
	for i := 0; i < n/4; i++ {
		fmt.Fprintf(&prom, "node_filesystem_avail_bytes{device=\"/dev/sd%d\",fstype=\"ext4\",mountpoint=\"/mnt/data%d\"} %de6\n", i, i, i)
	}
	prom.WriteString("# HELP http_request_duration_seconds Request latency.\n# TYPE http_request_duration_seconds histogram\n")
	for i := 0; i < n/4/12; i++ {
		for b, le := range []string{"0.005", "0.01", "0.025", "0.05", "0.1", "0.25", "0.5", "1", "2.5", "+Inf"} {
			fmt.Fprintf(&prom, "http_request_duration_seconds_bucket{handler=\"/h%d\",le=\"%s\"} %d\n", i, le, (b+1)*10)
		}
		fmt.Fprintf(&prom, "http_request_duration_seconds_sum{handler=\"/h%d\"} 12.5\nhttp_request_duration_seconds_count{handler=\"/h%d\"} 100\n", i, i)
	}
	return map[string]string{"/json": js.String(), "/json-as-text": js.String(), "/prom": prom.String(), "/text": text.String(), "/csv": csv.String(), "/xml": xml.String(), "/html": html.String()}
}

func probeBenchServer(b *testing.B, n int) (*Server, string) {
	b.Helper()
	bodies := probeBenchBodies(n)
	types := map[string]string{"/json": "application/json", "/json-as-text": "text/plain", "/prom": "text/plain; version=0.0.4", "/text": "text/plain", "/csv": "text/csv", "/xml": "application/xml", "/html": "text/html"}
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", types[r.URL.Path])
		_, _ = io.WriteString(w, bodies[r.URL.Path])
	}))
	b.Cleanup(target.Close)
	path := filepath.Join(b.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(probeBenchConfig), 0o600); err != nil {
		b.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		b.Fatal(err)
	}
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewServer(config.NewManager(cfg, path, quiet), "python3", quiet), target.URL
}

func BenchmarkProbe(b *testing.B) {
	for _, n := range []int{100, 5000} {
		server, target := probeBenchServer(b, n)
		handler := server.Handler()
		for _, collector := range []string{"jq_items", "jq_detected", "jq_sniffed", "jq_cached", "prom", "prom_rules", "regex", "csv", "xpath", "css", "python", "prescript"} {
			for _, gz := range []bool{false, true} {
				if gz && collector != "prom" && collector != "jq_cached" {
					continue
				}
				name := fmt.Sprintf("%s/n=%d", collector, n)
				if gz {
					name += "/gzip"
				}
				b.Run(name, func(b *testing.B) {
					path := "/probe?collector=" + collector + "&target=" + url.QueryEscape(target)
					b.ReportAllocs()
					var size int
					for b.Loop() {
						request := httptest.NewRequest(http.MethodGet, path, nil)
						if gz {
							request.Header.Set("Accept-Encoding", "gzip")
						}
						recorder := httptest.NewRecorder()
						handler.ServeHTTP(recorder, request)
						if recorder.Code != http.StatusOK {
							b.Fatalf("%d %s", recorder.Code, recorder.Body.String()[:min(300, recorder.Body.Len())])
						}
						size = recorder.Body.Len()
					}
					b.ReportMetric(float64(size), "answer-bytes")
				})
			}
		}
	}
}

func BenchmarkProbeFilebeatExample(b *testing.B) {
	stats, err := os.ReadFile("../../testdata/json/filebeat-stats-kafka.json")
	if err != nil {
		b.Fatal(err)
	}
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(stats)
	}))
	b.Cleanup(target.Close)
	cfg, err := config.Load("../../examples/config.filebeat.json-test.yaml")
	if err != nil {
		b.Fatal(err)
	}
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	handler := NewServer(config.NewManager(cfg, "", quiet), "python3", quiet).Handler()
	path := "/probe?collector=filebeat&target=" + url.QueryEscape(target.URL)
	b.ReportAllocs()
	for b.Loop() {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != http.StatusOK {
			b.Fatal(recorder.Code, recorder.Body.String())
		}
	}
}
