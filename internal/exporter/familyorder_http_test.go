//go:build !select_request_types || request_type_http

package exporter

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// Where a collector's series can come apart, a metric's series with another
// metric's between them, and what a probe answers of each: every source
// below answers each metric's series together under one TYPE line, in the
// text format and in OpenMetrics.

const familyOrderConfig = `
collectors:
  # Two rules over the rows of a table: the csv transform keeps the series
  # rule by rule.
  - name: csv_rules
    request: {type: http, path: /hosts.csv}
    transform: {type: csv}
    metrics: &rules
      - name: host_cpu
        description: CPU in use
        expression: cpu
        labels: [{name: host, expression: host}]
      - name: host_mem
        expression: mem
        labels: [{name: host, expression: host}]
  - name: csv_cached
    cache: {ttl: 1m}
    request: {type: http, path: /hosts.csv}
    transform: {type: csv}
    metrics: &one_name
      - name: host_usage
        expression: cpu
        labels: [{name: host, expression: host}, {name: kind, value: cpu}]
      - name: host_up
        expression: up
        labels: [{name: host, expression: host}]
      - name: host_usage
        expression: mem
        labels: [{name: host, expression: host}, {name: kind, value: mem}]

  # Two rules of one name with another rule between them, in every
  # transform that has rules: each keeps its series rule by rule, which
  # leaves the metric of the two in two places.
  - name: csv_one_name
    request: {type: http, path: /hosts.csv}
    transform: {type: csv}
    metrics: *one_name
  - name: jq_one_name
    request: {type: http, path: /hosts.json}
    transform: {type: jq}
    metrics:
      - name: host_usage
        items: .hosts[]
        expression: .cpu
        labels: [{name: host, expression: .host}, {name: kind, value: cpu}]
      - name: host_up
        items: .hosts[]
        expression: .up
        labels: [{name: host, expression: .host}]
      - name: host_usage
        items: .hosts[]
        expression: .mem
        labels: [{name: host, expression: .host}, {name: kind, value: mem}]
  - name: regex_one_name
    request: {type: http, path: /hosts.txt}
    transform: {type: regex}
    metrics:
      - name: host_usage
        expression: '(?m)^(?P<host>\S+) cpu=(?P<value>\d+)'
        labels: [{name: host, expression: host}, {name: kind, value: cpu}]
      - name: host_up
        expression: '(?m)^(?P<host>\S+) .* up=(?P<value>\d+)$'
        labels: [{name: host, expression: host}]
      - name: host_usage
        expression: '(?m)^(?P<host>\S+) cpu=\d+ mem=(?P<value>\d+)'
        labels: [{name: host, expression: host}, {name: kind, value: mem}]
  - name: xpath_one_name
    request: {type: http, path: /hosts.xml}
    transform: {type: xpath}
    metrics:
      - name: host_usage
        expression: //host/@cpu
        labels: [{name: host, expression: ../@name}, {name: kind, value: cpu}]
      - name: host_up
        expression: //host/@up
        labels: [{name: host, expression: ../@name}]
      - name: host_usage
        expression: //host/@mem
        labels: [{name: host, expression: ../@name}, {name: kind, value: mem}]

  # A table that is at its series limit: three rows for two rules are six
  # series, one more than the limit, and one cell is empty.
  - name: csv_at_its_limit
    request: {type: http, path: /sparse.csv}
    limits: {max_metrics: 5}
    transform: {type: csv}
    metrics:
      - name: host_cpu
        expression: cpu
        required: false
        labels: [{name: host, expression: host}]
      - name: host_mem
        expression: mem
        labels: [{name: host, expression: host}]

  # A script that emits a series of each metric for one row, then the next,
  # with the collector's prefix and labels on every series.
  - name: script
    metrics_prefix: fleet
    request: {type: http, path: /hosts.csv}
    decoder: {type: csv}
    transform:
      type: python
      labels: {source: export}
      script: |
        for row in data:
            metric(name="host_cpu", value=row["cpu"], labels={"host": row["host"]})
            metric(name="host_mem", value=row["mem"], labels={"host": row["host"]})
    metrics: []
  # A pre-script that leaves a table's rows with columns of their own.
  - name: pre_script
    request: {type: http, path: /hosts.csv}
    transform:
      type: csv
      pre_script: |
        data = [{"host": row["host"], "mem": row["mem"]} if row["host"] == "web01" else row for row in data]
    metrics:
      - name: host_cpu
        expression: cpu
        required: false
        labels: [{name: host, expression: host}]
      - name: host_up
        expression: up
        required: false
        labels: [{name: host, expression: host}]
      - name: host_mem
        expression: mem
        labels: [{name: host, expression: host}]

  # A target whose own exposition has a metric's samples apart, passed
  # through, and one that has them together, with two of its metrics given
  # one name: by rules, by transform.rename, and by name_escaping.
  - name: passed_through
    request: {type: http, path: /apart.prom}
    transform: {type: prometheus}
  - name: renamed_by_rules
    request: {type: http, path: /together.prom}
    transform: {type: prometheus}
    metrics:
      - {name: host_usage, expression: "^host_cpu$"}
      - {name: host_up}
      - {name: host_usage, expression: "^host_mem$"}
  - name: renamed
    request: {type: http, path: /together.prom}
    transform:
      type: prometheus
      rename: {host_cpu: host_usage, host_mem: host_usage}
  - name: escaped
    name_escaping: underscores
    request: {type: http, path: /dotted.prom}
    transform: {type: prometheus}
`

// familyOrderTarget serves what the collectors of familyOrderConfig read,
// and says how often a path was asked for.
func familyOrderTarget(t *testing.T) (*httptest.Server, func(path string) int) {
	t.Helper()
	var mu sync.Mutex
	requests := map[string]int{}
	bodies := map[string]string{
		"/hosts.csv":  "host,cpu,mem,up\nweb01,1,2,1\nweb02,3,4,0\n",
		"/sparse.csv": "host,cpu,mem\nweb01,1,2\nweb02,,4\nweb03,5,6\n",
		"/hosts.json": `{"hosts": [{"host": "web01", "cpu": 1, "mem": 2, "up": 1}, {"host": "web02", "cpu": 3, "mem": 4, "up": 0}]}`,
		"/hosts.txt":  "web01 cpu=1 mem=2 up=1\nweb02 cpu=3 mem=4 up=0\n",
		"/hosts.xml":  `<hosts><host name="web01" cpu="1" mem="2" up="1"/><host name="web02" cpu="3" mem="4" up="0"/></hosts>`,
		"/apart.prom": "# TYPE host_cpu gauge\nhost_cpu{host=\"web01\"} 1\n# TYPE host_mem gauge\nhost_mem{host=\"web01\"} 2\nhost_cpu{host=\"web02\"} 3\nhost_mem{host=\"web02\"} 4\n",
		"/together.prom": "# TYPE host_cpu gauge\nhost_cpu{host=\"web01\",kind=\"cpu\"} 1\nhost_cpu{host=\"web02\",kind=\"cpu\"} 3\n" +
			"# TYPE host_up gauge\nhost_up{host=\"web01\"} 1\nhost_up{host=\"web02\"} 0\n" +
			"# TYPE host_mem gauge\nhost_mem{host=\"web01\",kind=\"mem\"} 2\nhost_mem{host=\"web02\",kind=\"mem\"} 4\n",
		"/dotted.prom": "# TYPE host_usage gauge\nhost_usage{host=\"web01\"} 1\n# TYPE host_up gauge\nhost_up{host=\"web01\"} 1\n" +
			"# TYPE \"host.usage\" gauge\n{\"host.usage\",host=\"web02\"} 3\n",
	}
	types := map[string]string{".csv": "text/csv", "json": "application/json", ".txt": "text/plain", ".xml": "application/xml", "prom": "text/plain; version=0.0.4"}
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, known := bodies[r.URL.Path]
		if !known {
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		requests[r.URL.Path]++
		mu.Unlock()
		w.Header().Set("Content-Type", types[r.URL.Path[len(r.URL.Path)-4:]])
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(target.Close)
	return target, func(path string) int {
		mu.Lock()
		defer mu.Unlock()
		return requests[path]
	}
}

// Each source of series that can have a metric's series apart answers them
// together, the metrics in the order each first appears and a metric's
// series in the order they were made, under one TYPE line and the HELP of
// the metric's first series, in the text format and in OpenMetrics alike:
//
//   - a csv collector with several rules, which had them row by row;
//   - two rules of one name with another rule between them, in a csv, a jq,
//     a regex and an xpath collector alike;
//   - a csv collector at its series limit, and one whose pre-script leaves
//     rows with columns of their own, both of which keep the rows' order;
//   - a script that emits a series of each metric row by row, with the
//     collector's prefix and labels on its series;
//   - a prometheus collector whose rules, whose transform.rename or whose
//     name_escaping give two of a target's metrics one name; a target that
//     has a metric's samples apart itself is read into its metrics by the
//     decoder, and was answered together before.
//
// A cached answer is the same answer. Nothing is logged.
func TestEverySourceOfSeriesAnswersAMetricsSeriesTogether(t *testing.T) {
	requirePython(t)
	logs := testutil.CaptureLogs(t)
	target, asked := familyOrderTarget(t)
	server := csvFixtureServer(t, familyOrderConfig)
	const (
		ruleByRule = "# HELP host_cpu CPU in use\n# TYPE host_cpu gauge\nhost_cpu{host=\"web01\"} 1\nhost_cpu{host=\"web02\"} 3\n" +
			"# TYPE host_mem gauge\nhost_mem{host=\"web01\"} 2\nhost_mem{host=\"web02\"} 4\n"
		oneName = "# TYPE host_usage gauge\nhost_usage{host=\"web01\",kind=\"cpu\"} 1\nhost_usage{host=\"web02\",kind=\"cpu\"} 3\n" +
			"host_usage{host=\"web01\",kind=\"mem\"} 2\nhost_usage{host=\"web02\",kind=\"mem\"} 4\n" +
			"# TYPE host_up gauge\nhost_up{host=\"web01\"} 1\nhost_up{host=\"web02\"} 0\n"
	)
	for _, tc := range []struct{ collector, want string }{
		{"csv_rules", ruleByRule},
		{"csv_one_name", oneName},
		{"csv_cached", oneName},
		{"csv_cached", oneName},
		{"jq_one_name", oneName},
		{"regex_one_name", oneName},
		{"xpath_one_name", oneName},
		{"csv_at_its_limit", "# TYPE host_cpu gauge\nhost_cpu{host=\"web01\"} 1\nhost_cpu{host=\"web03\"} 5\n" +
			"# TYPE host_mem gauge\nhost_mem{host=\"web01\"} 2\nhost_mem{host=\"web02\"} 4\nhost_mem{host=\"web03\"} 6\n"},
		{"pre_script", "# TYPE host_mem gauge\nhost_mem{host=\"web01\"} 2\nhost_mem{host=\"web02\"} 4\n" +
			"# TYPE host_cpu gauge\nhost_cpu{host=\"web02\"} 3\n# TYPE host_up gauge\nhost_up{host=\"web02\"} 0\n"},
		{"script", "# TYPE fleet_host_cpu gauge\nfleet_host_cpu{host=\"web01\",source=\"export\"} 1\nfleet_host_cpu{host=\"web02\",source=\"export\"} 3\n" +
			"# TYPE fleet_host_mem gauge\nfleet_host_mem{host=\"web01\",source=\"export\"} 2\nfleet_host_mem{host=\"web02\",source=\"export\"} 4\n"},
		{"passed_through", "# TYPE host_cpu gauge\nhost_cpu{host=\"web01\"} 1\nhost_cpu{host=\"web02\"} 3\n" +
			"# TYPE host_mem gauge\nhost_mem{host=\"web01\"} 2\nhost_mem{host=\"web02\"} 4\n"},
		{"renamed_by_rules", oneName},
		{"renamed", oneName},
		{"escaped", "# TYPE host_usage gauge\nhost_usage{host=\"web01\"} 1\nhost_usage{host=\"web02\"} 3\n# TYPE host_up gauge\nhost_up{host=\"web01\"} 1\n"},
	} {
		path := "/probe?collector=" + tc.collector + "&target=" + url.QueryEscape(target.URL)
		response := probeOnce(t, server, path, nil)
		if response.Code != http.StatusOK || response.Body.String() != tc.want {
			t.Errorf("%s: status=%d\n%s\nwant\n%s", tc.collector, response.Code, response.Body.String(), tc.want)
			continue
		}
		if err := parseExposition(response.Body.Bytes()); err != nil {
			t.Errorf("%s: the answer does not parse: %v", tc.collector, err)
		}
		// Every metric here is a gauge, whose lines OpenMetrics writes as
		// the text format does, after its TYPE and before its HELP.
		openMetrics := probeOnce(t, server, path, http.Header{"Accept": {"application/openmetrics-text; version=1.0.0"}})
		wantSamples, wantTypes := sampleLines(tc.want)
		samples, types := sampleLines(strings.TrimSuffix(openMetrics.Body.String(), "# EOF\n"))
		if openMetrics.Code != http.StatusOK || !strings.HasSuffix(openMetrics.Body.String(), "# EOF\n") {
			t.Errorf("%s as OpenMetrics: status=%d\n%s", tc.collector, openMetrics.Code, openMetrics.Body.String())
		}
		linesInOrder(t, tc.collector+" as OpenMetrics: series", samples, wantSamples)
		linesInOrder(t, tc.collector+" as OpenMetrics: TYPE lines", types, wantTypes)
		contiguousFamilies(t, openMetrics.Body.String())
	}
	// The cached collector's file was fetched once, by the first of its four
	// probes, two in each format.
	if got := asked("/hosts.csv"); got != 2*4+1 {
		t.Errorf("/hosts.csv was asked for %d times, want twice by each of the four collectors that do not cache and once by the one that does", got)
	}
	loggedOnly(t, logs)
}
