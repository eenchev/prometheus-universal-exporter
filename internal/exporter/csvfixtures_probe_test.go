//go:build !select_request_types || request_type_http

package exporter

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// The fixtures of testdata/csv read by http collectors, through /probe (see
// csvfixtures_helpers_test.go). A stand-in serves each file as a target
// would, with the Content-Type the test tells it to send.

// csvStandIn serves the files of testdata/csv by their names, and tables of
// as many rows as /generated/<rows>.csv asks for, with the Content-Type it
// was last told to answer with, or with none.
type csvStandIn struct {
	*httptest.Server
	mu          sync.Mutex
	contentType string
	asked       []string
}

func (s *csvStandIn) answerAs(contentType string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.contentType = contentType
}

func (s *csvStandIn) requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.asked)
}

// csvFixtureNames is the names of the files of testdata/csv.
func csvFixtureNames(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir("../../testdata/csv")
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

func newCSVStandIn(t *testing.T) *csvStandIn {
	t.Helper()
	files := map[string][]byte{}
	service := &csvStandIn{contentType: "text/csv"}
	service.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		service.mu.Lock()
		service.asked = append(service.asked, r.URL.RequestURI())
		contentType := service.contentType
		body, known := files[r.URL.Path]
		if rows, generated := strings.CutPrefix(r.URL.Path, "/generated/"); generated && !known {
			if n, err := strconv.Atoi(strings.TrimSuffix(rows, ".csv")); err == nil {
				body, known = generatedCSV(n), true
				files[r.URL.Path] = body
			}
		}
		service.mu.Unlock()
		if !known {
			http.NotFound(w, r)
			return
		}
		// Without a Content-Type of its own the server would send the one
		// it makes out of the body's first bytes.
		w.Header()["Content-Type"] = nil
		if contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(service.Close)
	for _, name := range csvFixtureNames(t) {
		files["/"+name] = csvFixture(t, name)
	}
	return service
}

// probeCSV probes collector at the stand-in, asking for the text format, or
// for OpenMetrics.
func probeCSV(t *testing.T, server *Server, service *csvStandIn, collector string, openMetrics bool) *httptest.ResponseRecorder {
	t.Helper()
	header := http.Header{}
	if openMetrics {
		header.Set("Accept", "application/openmetrics-text; version=1.0.0")
	}
	return probeOnce(t, server, "/probe?collector="+collector+"&target="+url.QueryEscape(service.URL), header)
}

// answered is the body of a probe that was answered 200 in the text format.
func answered(t *testing.T, response *httptest.ResponseRecorder) string {
	t.Helper()
	if response.Code != http.StatusOK || response.Header().Get("Content-Type") != "text/plain; version=0.0.4; charset=utf-8" {
		t.Fatalf("status=%d as %s, body=%s", response.Code, response.Header().Get("Content-Type"), response.Body.String())
	}
	return response.Body.String()
}

// generatedHost, generatedRequests, generatedLatency and generatedNote are
// row i of a generated table: a host, a counter, a gauge in milliseconds and
// a note in Cyrillic of up to 63 bytes.
func generatedHost(i int) string  { return fmt.Sprintf("host-%04d", i) }
func generatedRequests(i int) int { return i * 37 }
func generatedLatency(i int) int  { return 5 + i%400 }
func generatedNote(i int) string  { return "note:" + strings.Repeat("ю", i%30) }

// generatedCSV is a table of rows rows, as large as an export of a few
// thousand hosts is: some 55 bytes a row.
func generatedCSV(rows int) []byte {
	var b strings.Builder
	b.WriteString("host,region,requests_total,latency_ms,restarts_total,note\n")
	for i := range rows {
		fmt.Fprintf(&b, "%s,r%d,%d,%d,%d,%s\n", generatedHost(i), i%7, generatedRequests(i), generatedLatency(i), i%3, generatedNote(i))
	}
	return []byte(b.String())
}

const csvProbeConfig = `
collectors:
  # One file, the decoder left out, left to each response and named: a csv
  # transform reads CSV in all three.
  - name: tickets
    request:
      type: http
      path: /tickets-rfc4180.csv
    transform:
      type: csv
    metrics: &tickets
      - name: ticket_age_hours
        description: Hours since the ticket was opened
        expression: age_hours
        labels:
          - name: id
            expression: id
          - name: queue
            expression: queue
          - name: customer
            expression: customer
          - name: subject
            expression: subject
      - name: ticket_replies
        description: Replies the ticket has had
        expression: replies
        labels:
          - name: id
            expression: id
  - name: tickets_auto
    request:
      type: http
      path: /tickets-rfc4180.csv
    decoder:
      type: auto
    transform:
      type: csv
    metrics: *tickets
  - name: tickets_csv
    request:
      type: http
      path: /tickets-rfc4180.csv
    decoder:
      type: csv
    transform:
      type: csv
    metrics: *tickets

  # A script, which reads what the decoder gives it: with the decoder left
  # to each response that is rows only for a response that says it is CSV.
  - name: script_auto
    request:
      type: http
      path: /status.csv
    decoder:
      type: auto
    transform:
      type: python
      script: &rows |
        rows = isinstance(data, list)
        metric(name="csv_data_items", type="gauge",
               value=len(data) if rows else len(data.splitlines()),
               labels={"data": "rows" if rows else type(data).__name__})
    metrics: []
  - name: script_csv
    request:
      type: http
      path: /status.csv
    decoder:
      type: csv
    transform:
      type: python
      script: *rows
    metrics: []

  # A fleet's status export with every setting a collector of one has.
  - name: fleet
    metrics_prefix: fleet
    request:
      type: http
      path: /service-status.csv
    transform:
      type: csv
      pre_script: |
        data = [row for row in data if row["env"] == "prod"]
      labels:
        source: status_export
      remove_labels:
        - internal_id
      rename_labels:
        host: instance
        dc: datacenter
    metrics:
      - name: service_up
        description: 1 up, 0.5 degraded or starting, 0 down, -1 anything else
        expression: state
        value_map: {up: 1, degraded: 0.5, starting: 0.5, down: 0, "*": -1}
        labels: &fleet
          - name: service
            expression: service
          - name: host
            expression: host
          - name: dc
            expression: dc
            value_map: {fra1: Frankfurt, ams2: Amsterdam, sof1: Sofia, "*": other}
          - name: internal_id
            expression: internal_id
      - name: service_restarts_total
        description: Restarts since the service was installed
        type: counter
        expression: restarts_total
        labels: *fleet
      - name: service_requests_total
        description: Requests served
        type: counter
        expression: requests_total
        labels: *fleet
      - name: service_errors_total
        description: Requests that failed
        type: counter
        expression: errors_total
        labels: *fleet
      - name: service_latency_seconds
        description: Median latency
        expression: latency_ms
        scale: 0.001
        required: false
        labels: *fleet
      - name: service_queue_depth
        description: Requests waiting
        expression: queue_depth
        labels: *fleet
      - name: service_info
        description: The version each instance runs
        expression: state
        value_map: {"*": 1}
        labels:
          - name: service
            expression: service
          - name: host
            expression: host
          - name: version
            expression: version
          - name: state
            expression: state
            value_map: {unknown: ""}

  # A list in windows-1251, read as the response names its encoding, and as
  # the collector names it.
  - name: oblasti
    request:
      type: http
      path: /oblasti-windows-1251.csv
    transform:
      type: csv
    metrics: &oblasti
      - name: city_temperature_celsius
        expression: температура
        labels:
          - name: city
            expression: град
          - name: province
            expression: област
          - name: state
            expression: състояние
      - name: city_humidity_percent
        expression: влажност
        labels:
          - name: city
            expression: град
  - name: oblasti_1251
    request:
      type: http
      path: /oblasti-windows-1251.csv
    response:
      charset: windows-1251
    transform:
      type: csv
    metrics: *oblasti
  - name: cities
    request:
      type: http
      path: /cities-utf16le-bom.csv
    transform:
      type: csv
    metrics: &cities
      - name: city_temperature_celsius
        expression: température
        labels:
          - name: city
            expression: città
          - name: country
            expression: Land
          - name: state
            expression: 状態
          - name: sky
            expression: sky
      - name: city_humidity_percent
        expression: влажност
        labels:
          - name: city
            expression: città
  - name: cities_be
    request:
      type: http
      path: /cities-utf16be.csv
    transform:
      type: csv
    metrics: *cities

  # A table of 5000 rows, under the limits a collector has.
  - name: large
    request:
      type: http
      path: /generated/5000.csv
    transform:
      type: csv
    metrics: &large
      - name: host_requests_total
        type: counter
        expression: requests_total
        labels: &host
          - name: host
            expression: host
          - name: region
            expression: region
      - name: host_latency_seconds
        expression: latency_ms
        scale: 0.001
        labels: *host
  - name: large_over
    request:
      type: http
      path: /generated/5000.csv
    transform:
      type: csv
    metrics: &larger
      - name: host_requests_total
        type: counter
        expression: requests_total
        labels: *host
      - name: host_latency_seconds
        expression: latency_ms
        scale: 0.001
        labels: *host
      - name: host_restarts_total
        type: counter
        expression: restarts_total
        labels: *host
  - name: large_raised
    request:
      type: http
      path: /generated/5000.csv
    limits:
      max_metrics: 15000
    transform:
      type: csv
    metrics: *larger
  - name: large_response
    request:
      type: http
      path: /generated/5000.csv
    limits:
      max_response_bytes: 64KiB
    transform:
      type: csv
    metrics: *large
  - name: long_label
    request:
      type: http
      path: /generated/60.csv
    limits:
      max_label_value_length: 40
    transform:
      type: csv
    metrics:
      - name: host_note
        expression: restarts_total
        value_map: {"*": 1}
        labels:
          - name: host
            expression: host
          - name: note
            expression: note
  - name: cut_label
    request:
      type: http
      path: /generated/60.csv
    limits:
      max_label_value_length: 40
    transform:
      type: csv
    metrics:
      - name: host_note
        expression: restarts_total
        value_map: {"*": 1}
        labels:
          - name: host
            expression: host
          - name: note
            expression: note
            truncate: true

  # What a file's rows cost the rules that cannot read them.
  - name: queues
    request:
      type: http
      path: /queues-pipe.txt
    response:
      csv:
        delimiter: "|"
    transform:
      type: csv
    metrics:
      - name: queue_messages_ready
        expression: messages_ready
        labels: &queue
          - name: queue
            expression: queue
          - name: vhost
            expression: vhost
      - name: queue_messages_unacked
        expression: messages_unacked
        error_mode: ignore
        labels: *queue
      - name: queue_consumers
        expression: consumers
        required: false
        labels: *queue
      - name: queue_state
        expression: state
        value_map: {running: 1, idle: 1, flow: 0.5, down: 0}
        labels: *queue
  - name: numbers_strict
    request:
      type: http
      path: /numbers.csv
    transform:
      type: csv
    metrics:
      - name: number
        expression: value
        error_mode: fail
        labels:
          - name: form
            expression: form
  - name: numbers
    request:
      type: http
      path: /numbers.csv
    transform:
      type: csv
    metrics:
      - name: number
        expression: value
        labels:
          - name: form
            expression: form
  # The numbers of a configuration are YAML's, which writes 1_000 for 1000
  # and 0x10 for 16; a response's are not.
  - name: numbers_mapped
    request:
      type: http
      path: /numbers.csv
    limits:
      max_metrics: 1_000
    transform:
      type: csv
    metrics:
      - name: number
        expression: value
        error_mode: ignore
        scale: 1_0
        value_map: {"1_000": 1_000, "0x1p-2": 0x10}
        labels:
          - name: form
            expression: form
  - name: readings_by_sensor
    request:
      type: http
      path: /readings-noheader.csv
    response:
      csv:
        header: false
    transform:
      type: csv
    metrics:
      - name: reading_temperature_celsius
        expression: "3"
        labels:
          - name: sensor
            expression: "2"

  # A list whose texts are in quotes and padded with blanks after them.
  - name: stock
    request:
      type: http
      path: /stock-padded-quotes.csv
    response:
      csv:
        delimiter: ";"
        trim_space: true
    transform:
      type: csv
    metrics: &stock
      - name: stock_items
        expression: Bestand
        labels:
          - name: item
            expression: Artikel
          - name: site
            expression: Lager
          - name: note
            expression: Hinweis
      - name: stock_items_minimum
        expression: Mindestbestand
        labels:
          - name: item
            expression: Artikel
  - name: stock_untrimmed
    request:
      type: http
      path: /stock-padded-quotes.csv
    response:
      csv:
        delimiter: ";"
    transform:
      type: csv
    metrics: *stock

  # A report with a row longer than its header.
  - name: jobs
    request:
      type: http
      path: /jobs-unquoted-comma.csv
    transform:
      type: csv
    metrics: &jobs
      - name: job_duration_seconds
        expression: duration_seconds
        labels:
          - name: job
            expression: job
  - name: jobs_script
    request:
      type: http
      path: /jobs-unquoted-comma.csv
    transform:
      type: csv
      pre_script: |
        data = [row for row in data if row["state"] == "ok"]
    metrics: *jobs
  - name: jobs_logged
    request:
      type: http
      path: /jobs-unquoted-comma.csv
    error_handling:
      on_decode_error: log
    transform:
      type: csv
    metrics: *jobs
  - name: usage
    request:
      type: http
      path: /usage-duplicate-columns.csv
    transform:
      type: csv
    metrics:
      - name: host_memory_used_megabytes
        expression: used
        labels:
          - name: host
            expression: host
  - name: usage_unnamed
    request:
      type: http
      path: /usage-unnamed-column.csv
    error_handling:
      on_decode_error: log
    transform:
      type: csv
    metrics:
      - name: host_memory_used_megabytes
        expression: used
        labels:
          - name: host
            expression: host

  # A report whose lines end with a carriage return alone.
  - name: volumes
    request:
      type: http
      path: /volumes-cr.csv
    transform:
      type: csv
    metrics:
      - name: volume_used_percent
        expression: used_percent
        labels:
          - name: volume
            expression: volume
          - name: pool
            expression: pool
          - name: note
            expression: note
      - name: volume_free_gibibytes
        expression: free_gib
        labels: &volume
          - name: volume
            expression: volume
  # The same report read by rules that name what its header does not have:
  # a label's column in another case, on a rule that is not required, and a
  # value's column.
  - name: volumes_misnamed
    request:
      type: http
      path: /volumes-cr.csv
    transform:
      type: csv
    metrics:
      - name: volume_used_percent
        expression: used_percent
        required: false
        labels:
          - name: volume
            expression: volume
          - name: pool
            expression: Pool
      - name: volume_free_gigabytes
        expression: free_gb
        labels: *volume
      - name: volume_free_gibibytes
        expression: free_gib
        labels: *volume
  - name: volumes_strict
    request:
      type: http
      path: /volumes-cr.csv
    transform:
      type: csv
    metrics:
      - name: volume_free_gibibytes
        expression: free_gib
        labels: *volume
      - name: volume_used_percent
        expression: used_percent
        error_mode: fail
        labels:
          - name: volume
            expression: volume
          - name: pool
            expression: Pool
  # A balance's log, its lines ended in three ways.
  - name: scale
    request:
      type: http
      path: /scale-mixed-line-ends.txt
    response:
      csv:
        delimiter: ";"
        header: false
        trim_space: true
    transform:
      type: csv
    metrics:
      - name: scale_weight_kilograms
        expression: "3"
        labels:
          - name: scale
            expression: "2"
          - name: state
            expression: "4"
          - name: at
            expression: "1"
`

// The content types a target may send a CSV file with, and none at all.
var csvContentTypes = []string{
	"text/csv",
	"text/csv; charset=utf-8",
	"text/csv; header=present",
	"application/csv",
	"text/tab-separated-values",
	"text/plain",
	"text/plain; charset=UTF-8",
	"application/octet-stream",
	"application/json",
	"",
}

// A collector with a csv transform reads the response as CSV whatever its
// Content-Type says, and when it has none, with decoder.type left out, set
// to auto or set to csv: the transform implies the decoder. Every probe of
// the RFC 4180 export answers the same series, with the commas, the quotes
// and the line breaks of its quoted fields in the labels, a quote and a line
// break escaped as the format writes them. OpenMetrics answers the same
// series, each family's together under its TYPE and HELP. The file is asked
// for once a probe, and nothing is logged.
func TestCSVFixtureProbeACSVCollectorReadsCSVWhateverTheContentType(t *testing.T) {
	logs := testutil.CaptureLogs(t)
	service := newCSVStandIn(t)
	server := csvFixtureServer(t, csvProbeConfig)
	want := []string{
		`ticket_age_hours{customer="Acme, Inc.",id="10231",queue="billing",subject="Invoice 2026-0917 charged twice"} 52.5`,
		`ticket_age_hours{customer="Müller & Söhne GmbH",id="10232",queue="billing",subject="Refund for order #5541, second request"} 30`,
		`ticket_age_hours{customer="Initech",id="10233",queue="hardware",subject="Printer says \"PC LOAD LETTER\""} 211.25`,
		`ticket_age_hours{customer="Globex, Ltd.",id="10234",queue="hardware",subject="Rack 12, unit 4: fan alarm"} 1.5`,
		`ticket_age_hours{customer="Initech",id="10235",queue="network",subject="VPN drops every 30 min\n(since the firewall change)"} 18`,
		`ticket_age_hours{customer="Hooli, LLC",id="10236",queue="network",subject="Wi-Fi guest portal certificate expired"} 6.75`,
		`ticket_age_hours{customer="Acme, Inc.",id="10237",queue="accounts",subject="Password reset for \"j.doe\", locked out"} 0.25`,
		`ticket_age_hours{customer="Umbrella Corp",id="10238",queue="accounts",subject="New starter needs access"} 73`,
		`ticket_age_hours{customer="Stark Industries",id="10239",queue="software",subject="Crash on export: \"index out of range\"\nSteps:\n1. open report, 2. click \"Export\""} 12`,
		`ticket_age_hours{customer="Wayne Enterprises, Inc.",id="10240",queue="software",subject="Licence renewal"} 340`,
		`ticket_age_hours{customer="Initech",id="10241",queue="billing",subject="Quote for 5\" tablets, 20 units"} 96`,
		`ticket_age_hours{customer="Globex",id="10242",queue="network"} 3`,
		`ticket_replies{id="10231"} 4`,
		`ticket_replies{id="10232"} 2`,
		`ticket_replies{id="10233"} 7`,
		`ticket_replies{id="10234"} 0`,
		`ticket_replies{id="10235"} 3`,
		`ticket_replies{id="10236"} 1`,
		`ticket_replies{id="10237"} 0`,
		`ticket_replies{id="10238"} 5`,
		`ticket_replies{id="10239"} 2`,
		`ticket_replies{id="10240"} 9`,
		`ticket_replies{id="10241"} 1`,
		`ticket_replies{id="10242"} 0`,
	}
	types := []string{"# TYPE ticket_age_hours gauge", "# TYPE ticket_replies gauge"}
	probes := 0
	for _, contentType := range csvContentTypes {
		service.answerAs(contentType)
		for _, collector := range []string{"tickets", "tickets_auto", "tickets_csv"} {
			t.Run(collector+" as "+contentType, func(t *testing.T) {
				answersSeries(t, answered(t, probeCSV(t, server, service, collector, false)), want, types)
			})
			probes++
		}
	}
	response := probeCSV(t, server, service, "tickets", true)
	const openMetrics = `# TYPE ticket_age_hours gauge
# HELP ticket_age_hours Hours since the ticket was opened
ticket_age_hours{customer="Acme, Inc.",id="10231",queue="billing",subject="Invoice 2026-0917 charged twice"} 52.5
ticket_age_hours{customer="Müller & Söhne GmbH",id="10232",queue="billing",subject="Refund for order #5541, second request"} 30
ticket_age_hours{customer="Initech",id="10233",queue="hardware",subject="Printer says \"PC LOAD LETTER\""} 211.25
ticket_age_hours{customer="Globex, Ltd.",id="10234",queue="hardware",subject="Rack 12, unit 4: fan alarm"} 1.5
ticket_age_hours{customer="Initech",id="10235",queue="network",subject="VPN drops every 30 min\n(since the firewall change)"} 18
ticket_age_hours{customer="Hooli, LLC",id="10236",queue="network",subject="Wi-Fi guest portal certificate expired"} 6.75
ticket_age_hours{customer="Acme, Inc.",id="10237",queue="accounts",subject="Password reset for \"j.doe\", locked out"} 0.25
ticket_age_hours{customer="Umbrella Corp",id="10238",queue="accounts",subject="New starter needs access"} 73
ticket_age_hours{customer="Stark Industries",id="10239",queue="software",subject="Crash on export: \"index out of range\"\nSteps:\n1. open report, 2. click \"Export\""} 12
ticket_age_hours{customer="Wayne Enterprises, Inc.",id="10240",queue="software",subject="Licence renewal"} 340
ticket_age_hours{customer="Initech",id="10241",queue="billing",subject="Quote for 5\" tablets, 20 units"} 96
ticket_age_hours{customer="Globex",id="10242",queue="network"} 3
# TYPE ticket_replies gauge
# HELP ticket_replies Replies the ticket has had
ticket_replies{id="10231"} 4
ticket_replies{id="10232"} 2
ticket_replies{id="10233"} 7
ticket_replies{id="10234"} 0
ticket_replies{id="10235"} 3
ticket_replies{id="10236"} 1
ticket_replies{id="10237"} 0
ticket_replies{id="10238"} 5
ticket_replies{id="10239"} 2
ticket_replies{id="10240"} 9
ticket_replies{id="10241"} 1
ticket_replies{id="10242"} 0
# EOF
`
	if response.Code != http.StatusOK || response.Header().Get("Content-Type") != "application/openmetrics-text; version=1.0.0; charset=utf-8" || response.Body.String() != openMetrics {
		t.Errorf("as OpenMetrics: status=%d as %s\n%s\nwant\n%s", response.Code, response.Header().Get("Content-Type"), response.Body.String(), openMetrics)
	}
	asked := service.requests()
	if len(asked) != probes+1 || slices.ContainsFunc(asked, func(path string) bool { return path != "/tickets-rfc4180.csv" }) {
		t.Errorf("the stand-in was asked %v, want /tickets-rfc4180.csv %d times", asked, probes+1)
	}
	loggedOnly(t, logs)
}

// A python transform is given what the decoder made of the response. With
// decoder.type auto that is the rows only when the Content-Type says
// text/csv, with or without parameters; any other type, application/csv and
// text/tab-separated-values among them, and no type at all, leave a CSV
// body to be read by its content, as text, and the script gets one string;
// and a response that says it is JSON is decoded as JSON, which fails the
// probe in the decode stage. With decoder.type csv the script gets the rows
// whatever the response says.
func TestCSVFixtureProbeAScriptIsGivenRowsByTheContentTypeOrTheDecoder(t *testing.T) {
	requirePython(t)
	logs := testutil.CaptureLogs(t)
	service := newCSVStandIn(t)
	server := csvFixtureServer(t, csvProbeConfig)
	types := []string{"# TYPE csv_data_items gauge"}
	for _, contentType := range csvContentTypes {
		service.answerAs(contentType)
		t.Run("csv as "+contentType, func(t *testing.T) {
			answersSeries(t, answered(t, probeCSV(t, server, service, "script_csv", false)), []string{`csv_data_items{data="rows"} 2`}, types)
		})
		if contentType == "application/json" {
			continue
		}
		// status.csv is a header and two rows: two rows, or three lines.
		auto := `csv_data_items{data="str"} 3`
		if strings.HasPrefix(contentType, "text/csv") {
			auto = `csv_data_items{data="rows"} 2`
		}
		t.Run("auto as "+contentType, func(t *testing.T) {
			answersSeries(t, answered(t, probeCSV(t, server, service, "script_auto", false)), []string{auto}, types)
		})
	}
	loggedOnly(t, logs)

	service.answerAs("application/json")
	response := probeCSV(t, server, service, "script_auto", false)
	const notJSON = "collector script_auto decode failed: JSON decode: invalid character 's' looking for beginning of value, at line 1, column 1\n"
	if response.Code != http.StatusBadGateway || response.Body.String() != notJSON {
		t.Errorf("auto as application/json: status=%d body=%s, want 502 and %s", response.Code, response.Body.String(), notJSON)
	}
	failures, others := ruleFailureLogs(t, logs)
	if len(failures) != 0 || len(others) != 1 || !strings.Contains(others[0], `"msg":"probe failed"`) || !strings.Contains(others[0], `"stage":"decode"`) {
		t.Errorf("logged %v and %v, want the one failed probe, in the decode stage", failures, others)
	}
}

// A fleet's status export read by a collector with every setting such a
// collector has: a pre-script keeps the rows of production, a quarter of the
// file gone before any rule reads it; the state in words is a number by
// value_map, any other state -1; counters are counters; milliseconds are
// seconds by scale, and a latency left empty is no series and no failure of
// a rule that is not required; the labels come from several columns, the
// data centre's code is its name by the label's value_map, and a state the
// map gives no text leaves its label off; every series gets the collector's
// source label, loses the export's internal_id and has host and dc renamed;
// and every name has the collector's prefix. The text format and
// OpenMetrics answer the same series with the same types, OpenMetrics with
// each family together under its TYPE and HELP and a counter's family named
// without _total. Nothing is logged.
func TestCSVFixtureProbeAStatusExportWithEverySettingOfACollector(t *testing.T) {
	requirePython(t)
	logs := testutil.CaptureLogs(t)
	service := newCSVStandIn(t)
	server := csvFixtureServer(t, csvProbeConfig)

	body := answered(t, probeCSV(t, server, service, "fleet", false))
	answersSeries(t, body, []string{
		`fleet_service_up{datacenter="Frankfurt",instance="web01",service="api",source="status_export"} 1`,
		`fleet_service_up{datacenter="Frankfurt",instance="web02",service="api",source="status_export"} 1`,
		`fleet_service_up{datacenter="Amsterdam",instance="web03",service="api",source="status_export"} 0.5`,
		`fleet_service_up{datacenter="Frankfurt",instance="web01",service="checkout",source="status_export"} 1`,
		`fleet_service_up{datacenter="Amsterdam",instance="web03",service="checkout",source="status_export"} 0`,
		`fleet_service_up{datacenter="Amsterdam",instance="srch01",service="search",source="status_export"} 1`,
		`fleet_service_up{datacenter="Sofia",instance="srch02",service="search",source="status_export"} 1`,
		`fleet_service_up{datacenter="Sofia",instance="job01",service="mailer",source="status_export"} 1`,
		`fleet_service_up{datacenter="Sofia",instance="job02",service="mailer",source="status_export"} 0.5`,
		`fleet_service_up{datacenter="Frankfurt",instance="web01",service="auth",source="status_export"} 1`,
		`fleet_service_up{datacenter="Frankfurt",instance="web02",service="auth",source="status_export"} 1`,
		`fleet_service_up{datacenter="Amsterdam",instance="web03",service="auth",source="status_export"} -1`,
		`fleet_service_restarts_total{datacenter="Frankfurt",instance="web01",service="api",source="status_export"} 3`,
		`fleet_service_restarts_total{datacenter="Frankfurt",instance="web02",service="api",source="status_export"} 1`,
		`fleet_service_restarts_total{datacenter="Amsterdam",instance="web03",service="api",source="status_export"} 14`,
		`fleet_service_restarts_total{datacenter="Frankfurt",instance="web01",service="checkout",source="status_export"} 0`,
		`fleet_service_restarts_total{datacenter="Amsterdam",instance="web03",service="checkout",source="status_export"} 22`,
		`fleet_service_restarts_total{datacenter="Amsterdam",instance="srch01",service="search",source="status_export"} 5`,
		`fleet_service_restarts_total{datacenter="Sofia",instance="srch02",service="search",source="status_export"} 5`,
		`fleet_service_restarts_total{datacenter="Sofia",instance="job01",service="mailer",source="status_export"} 2`,
		`fleet_service_restarts_total{datacenter="Sofia",instance="job02",service="mailer",source="status_export"} 7`,
		`fleet_service_restarts_total{datacenter="Frankfurt",instance="web01",service="auth",source="status_export"} 0`,
		`fleet_service_restarts_total{datacenter="Frankfurt",instance="web02",service="auth",source="status_export"} 0`,
		`fleet_service_restarts_total{datacenter="Amsterdam",instance="web03",service="auth",source="status_export"} 1`,
		`fleet_service_requests_total{datacenter="Frankfurt",instance="web01",service="api",source="status_export"} 1.8250411e+07`,
		`fleet_service_requests_total{datacenter="Frankfurt",instance="web02",service="api",source="status_export"} 1.801123e+07`,
		`fleet_service_requests_total{datacenter="Amsterdam",instance="web03",service="api",source="status_export"} 9.120455e+06`,
		`fleet_service_requests_total{datacenter="Frankfurt",instance="web01",service="checkout",source="status_export"} 2.210934e+06`,
		`fleet_service_requests_total{datacenter="Amsterdam",instance="web03",service="checkout",source="status_export"} 1.80412e+06`,
		`fleet_service_requests_total{datacenter="Amsterdam",instance="srch01",service="search",source="status_export"} 7.741209e+06`,
		`fleet_service_requests_total{datacenter="Sofia",instance="srch02",service="search",source="status_export"} 7.702113e+06`,
		`fleet_service_requests_total{datacenter="Sofia",instance="job01",service="mailer",source="status_export"} 402118`,
		`fleet_service_requests_total{datacenter="Sofia",instance="job02",service="mailer",source="status_export"} 0`,
		`fleet_service_requests_total{datacenter="Frankfurt",instance="web01",service="auth",source="status_export"} 2.5550921e+07`,
		`fleet_service_requests_total{datacenter="Frankfurt",instance="web02",service="auth",source="status_export"} 2.5190077e+07`,
		`fleet_service_requests_total{datacenter="Amsterdam",instance="web03",service="auth",source="status_export"} 1.201055e+07`,
		`fleet_service_errors_total{datacenter="Frankfurt",instance="web01",service="api",source="status_export"} 912`,
		`fleet_service_errors_total{datacenter="Frankfurt",instance="web02",service="api",source="status_export"} 845`,
		`fleet_service_errors_total{datacenter="Amsterdam",instance="web03",service="api",source="status_export"} 20417`,
		`fleet_service_errors_total{datacenter="Frankfurt",instance="web01",service="checkout",source="status_export"} 118`,
		`fleet_service_errors_total{datacenter="Amsterdam",instance="web03",service="checkout",source="status_export"} 90211`,
		`fleet_service_errors_total{datacenter="Amsterdam",instance="srch01",service="search",source="status_export"} 2051`,
		`fleet_service_errors_total{datacenter="Sofia",instance="srch02",service="search",source="status_export"} 1988`,
		`fleet_service_errors_total{datacenter="Sofia",instance="job01",service="mailer",source="status_export"} 37`,
		`fleet_service_errors_total{datacenter="Sofia",instance="job02",service="mailer",source="status_export"} 0`,
		`fleet_service_errors_total{datacenter="Frankfurt",instance="web01",service="auth",source="status_export"} 4410`,
		`fleet_service_errors_total{datacenter="Frankfurt",instance="web02",service="auth",source="status_export"} 4302`,
		`fleet_service_errors_total{datacenter="Amsterdam",instance="web03",service="auth",source="status_export"} 2209`,
		`fleet_service_latency_seconds{datacenter="Frankfurt",instance="web01",service="api",source="status_export"} 0.0415`,
		`fleet_service_latency_seconds{datacenter="Frankfurt",instance="web02",service="api",source="status_export"} 0.039`,
		`fleet_service_latency_seconds{datacenter="Amsterdam",instance="web03",service="api",source="status_export"} 0.412`,
		`fleet_service_latency_seconds{datacenter="Frankfurt",instance="web01",service="checkout",source="status_export"} 0.12`,
		`fleet_service_latency_seconds{datacenter="Amsterdam",instance="srch01",service="search",source="status_export"} 0.01875`,
		`fleet_service_latency_seconds{datacenter="Sofia",instance="srch02",service="search",source="status_export"} 0.019`,
		`fleet_service_latency_seconds{datacenter="Sofia",instance="job01",service="mailer",source="status_export"} 0.95`,
		`fleet_service_latency_seconds{datacenter="Frankfurt",instance="web01",service="auth",source="status_export"} 0.0075`,
		`fleet_service_latency_seconds{datacenter="Frankfurt",instance="web02",service="auth",source="status_export"} 0.00725`,
		`fleet_service_latency_seconds{datacenter="Amsterdam",instance="web03",service="auth",source="status_export"} 0.009`,
		`fleet_service_queue_depth{datacenter="Frankfurt",instance="web01",service="api",source="status_export"} 0`,
		`fleet_service_queue_depth{datacenter="Frankfurt",instance="web02",service="api",source="status_export"} 2`,
		`fleet_service_queue_depth{datacenter="Amsterdam",instance="web03",service="api",source="status_export"} 57`,
		`fleet_service_queue_depth{datacenter="Frankfurt",instance="web01",service="checkout",source="status_export"} 1`,
		`fleet_service_queue_depth{datacenter="Amsterdam",instance="web03",service="checkout",source="status_export"} 340`,
		`fleet_service_queue_depth{datacenter="Amsterdam",instance="srch01",service="search",source="status_export"} 4`,
		`fleet_service_queue_depth{datacenter="Sofia",instance="srch02",service="search",source="status_export"} 3`,
		`fleet_service_queue_depth{datacenter="Sofia",instance="job01",service="mailer",source="status_export"} 12`,
		`fleet_service_queue_depth{datacenter="Sofia",instance="job02",service="mailer",source="status_export"} 0`,
		`fleet_service_queue_depth{datacenter="Frankfurt",instance="web01",service="auth",source="status_export"} 0`,
		`fleet_service_queue_depth{datacenter="Frankfurt",instance="web02",service="auth",source="status_export"} 0`,
		`fleet_service_queue_depth{datacenter="Amsterdam",instance="web03",service="auth",source="status_export"} 0`,
		`fleet_service_info{instance="web01",service="api",source="status_export",state="up",version="2.14.1"} 1`,
		`fleet_service_info{instance="web02",service="api",source="status_export",state="up",version="2.14.1"} 1`,
		`fleet_service_info{instance="web03",service="api",source="status_export",state="degraded",version="2.14.0"} 1`,
		`fleet_service_info{instance="web01",service="checkout",source="status_export",state="up",version="5.2.0"} 1`,
		`fleet_service_info{instance="web03",service="checkout",source="status_export",state="down",version="5.2.0"} 1`,
		`fleet_service_info{instance="srch01",service="search",source="status_export",state="up",version="1.9.3"} 1`,
		`fleet_service_info{instance="srch02",service="search",source="status_export",state="up",version="1.9.3"} 1`,
		`fleet_service_info{instance="job01",service="mailer",source="status_export",state="up",version="0.8.4"} 1`,
		`fleet_service_info{instance="job02",service="mailer",source="status_export",state="starting",version="0.8.4"} 1`,
		`fleet_service_info{instance="web01",service="auth",source="status_export",state="up",version="3.3.1"} 1`,
		`fleet_service_info{instance="web02",service="auth",source="status_export",state="up",version="3.3.1"} 1`,
		`fleet_service_info{instance="web03",service="auth",source="status_export",version="3.3.1"} 1`,
	}, []string{
		"# TYPE fleet_service_up gauge",
		"# TYPE fleet_service_restarts_total counter",
		"# TYPE fleet_service_requests_total counter",
		"# TYPE fleet_service_errors_total counter",
		"# TYPE fleet_service_latency_seconds gauge",
		"# TYPE fleet_service_queue_depth gauge",
		"# TYPE fleet_service_info gauge",
	})
	for _, help := range []string{
		"# HELP fleet_service_up 1 up, 0.5 degraded or starting, 0 down, -1 anything else\n",
		"# HELP fleet_service_restarts_total Restarts since the service was installed\n",
		"# HELP fleet_service_requests_total Requests served\n",
		"# HELP fleet_service_errors_total Requests that failed\n",
		"# HELP fleet_service_latency_seconds Median latency\n",
		"# HELP fleet_service_queue_depth Requests waiting\n",
		"# HELP fleet_service_info The version each instance runs\n",
	} {
		if strings.Count(body, help) != 1 {
			t.Errorf("the answer does not have, once, %s", help)
		}
	}
	for _, gone := range []string{"internal_id", "staging", "stg01", "dev01", `host="`, `dc="`, "fra1", "\nservice_"} {
		if strings.Contains(body, gone) {
			t.Errorf("the answer holds %q:\n%s", gone, body)
		}
	}
	if err := parseExposition([]byte(body)); err != nil {
		t.Errorf("the answer does not parse: %v", err)
	}

	response := probeCSV(t, server, service, "fleet", true)
	const openMetrics = `# TYPE fleet_service_up gauge
# HELP fleet_service_up 1 up, 0.5 degraded or starting, 0 down, -1 anything else
fleet_service_up{datacenter="Frankfurt",instance="web01",service="api",source="status_export"} 1
fleet_service_up{datacenter="Frankfurt",instance="web02",service="api",source="status_export"} 1
fleet_service_up{datacenter="Amsterdam",instance="web03",service="api",source="status_export"} 0.5
fleet_service_up{datacenter="Frankfurt",instance="web01",service="checkout",source="status_export"} 1
fleet_service_up{datacenter="Amsterdam",instance="web03",service="checkout",source="status_export"} 0
fleet_service_up{datacenter="Amsterdam",instance="srch01",service="search",source="status_export"} 1
fleet_service_up{datacenter="Sofia",instance="srch02",service="search",source="status_export"} 1
fleet_service_up{datacenter="Sofia",instance="job01",service="mailer",source="status_export"} 1
fleet_service_up{datacenter="Sofia",instance="job02",service="mailer",source="status_export"} 0.5
fleet_service_up{datacenter="Frankfurt",instance="web01",service="auth",source="status_export"} 1
fleet_service_up{datacenter="Frankfurt",instance="web02",service="auth",source="status_export"} 1
fleet_service_up{datacenter="Amsterdam",instance="web03",service="auth",source="status_export"} -1
# TYPE fleet_service_restarts counter
# HELP fleet_service_restarts Restarts since the service was installed
fleet_service_restarts_total{datacenter="Frankfurt",instance="web01",service="api",source="status_export"} 3
fleet_service_restarts_total{datacenter="Frankfurt",instance="web02",service="api",source="status_export"} 1
fleet_service_restarts_total{datacenter="Amsterdam",instance="web03",service="api",source="status_export"} 14
fleet_service_restarts_total{datacenter="Frankfurt",instance="web01",service="checkout",source="status_export"} 0
fleet_service_restarts_total{datacenter="Amsterdam",instance="web03",service="checkout",source="status_export"} 22
fleet_service_restarts_total{datacenter="Amsterdam",instance="srch01",service="search",source="status_export"} 5
fleet_service_restarts_total{datacenter="Sofia",instance="srch02",service="search",source="status_export"} 5
fleet_service_restarts_total{datacenter="Sofia",instance="job01",service="mailer",source="status_export"} 2
fleet_service_restarts_total{datacenter="Sofia",instance="job02",service="mailer",source="status_export"} 7
fleet_service_restarts_total{datacenter="Frankfurt",instance="web01",service="auth",source="status_export"} 0
fleet_service_restarts_total{datacenter="Frankfurt",instance="web02",service="auth",source="status_export"} 0
fleet_service_restarts_total{datacenter="Amsterdam",instance="web03",service="auth",source="status_export"} 1
# TYPE fleet_service_requests counter
# HELP fleet_service_requests Requests served
fleet_service_requests_total{datacenter="Frankfurt",instance="web01",service="api",source="status_export"} 1.8250411e+07
fleet_service_requests_total{datacenter="Frankfurt",instance="web02",service="api",source="status_export"} 1.801123e+07
fleet_service_requests_total{datacenter="Amsterdam",instance="web03",service="api",source="status_export"} 9.120455e+06
fleet_service_requests_total{datacenter="Frankfurt",instance="web01",service="checkout",source="status_export"} 2.210934e+06
fleet_service_requests_total{datacenter="Amsterdam",instance="web03",service="checkout",source="status_export"} 1.80412e+06
fleet_service_requests_total{datacenter="Amsterdam",instance="srch01",service="search",source="status_export"} 7.741209e+06
fleet_service_requests_total{datacenter="Sofia",instance="srch02",service="search",source="status_export"} 7.702113e+06
fleet_service_requests_total{datacenter="Sofia",instance="job01",service="mailer",source="status_export"} 402118
fleet_service_requests_total{datacenter="Sofia",instance="job02",service="mailer",source="status_export"} 0
fleet_service_requests_total{datacenter="Frankfurt",instance="web01",service="auth",source="status_export"} 2.5550921e+07
fleet_service_requests_total{datacenter="Frankfurt",instance="web02",service="auth",source="status_export"} 2.5190077e+07
fleet_service_requests_total{datacenter="Amsterdam",instance="web03",service="auth",source="status_export"} 1.201055e+07
# TYPE fleet_service_errors counter
# HELP fleet_service_errors Requests that failed
fleet_service_errors_total{datacenter="Frankfurt",instance="web01",service="api",source="status_export"} 912
fleet_service_errors_total{datacenter="Frankfurt",instance="web02",service="api",source="status_export"} 845
fleet_service_errors_total{datacenter="Amsterdam",instance="web03",service="api",source="status_export"} 20417
fleet_service_errors_total{datacenter="Frankfurt",instance="web01",service="checkout",source="status_export"} 118
fleet_service_errors_total{datacenter="Amsterdam",instance="web03",service="checkout",source="status_export"} 90211
fleet_service_errors_total{datacenter="Amsterdam",instance="srch01",service="search",source="status_export"} 2051
fleet_service_errors_total{datacenter="Sofia",instance="srch02",service="search",source="status_export"} 1988
fleet_service_errors_total{datacenter="Sofia",instance="job01",service="mailer",source="status_export"} 37
fleet_service_errors_total{datacenter="Sofia",instance="job02",service="mailer",source="status_export"} 0
fleet_service_errors_total{datacenter="Frankfurt",instance="web01",service="auth",source="status_export"} 4410
fleet_service_errors_total{datacenter="Frankfurt",instance="web02",service="auth",source="status_export"} 4302
fleet_service_errors_total{datacenter="Amsterdam",instance="web03",service="auth",source="status_export"} 2209
# TYPE fleet_service_latency_seconds gauge
# HELP fleet_service_latency_seconds Median latency
fleet_service_latency_seconds{datacenter="Frankfurt",instance="web01",service="api",source="status_export"} 0.0415
fleet_service_latency_seconds{datacenter="Frankfurt",instance="web02",service="api",source="status_export"} 0.039
fleet_service_latency_seconds{datacenter="Amsterdam",instance="web03",service="api",source="status_export"} 0.412
fleet_service_latency_seconds{datacenter="Frankfurt",instance="web01",service="checkout",source="status_export"} 0.12
fleet_service_latency_seconds{datacenter="Amsterdam",instance="srch01",service="search",source="status_export"} 0.01875
fleet_service_latency_seconds{datacenter="Sofia",instance="srch02",service="search",source="status_export"} 0.019
fleet_service_latency_seconds{datacenter="Sofia",instance="job01",service="mailer",source="status_export"} 0.95
fleet_service_latency_seconds{datacenter="Frankfurt",instance="web01",service="auth",source="status_export"} 0.0075
fleet_service_latency_seconds{datacenter="Frankfurt",instance="web02",service="auth",source="status_export"} 0.00725
fleet_service_latency_seconds{datacenter="Amsterdam",instance="web03",service="auth",source="status_export"} 0.009
# TYPE fleet_service_queue_depth gauge
# HELP fleet_service_queue_depth Requests waiting
fleet_service_queue_depth{datacenter="Frankfurt",instance="web01",service="api",source="status_export"} 0
fleet_service_queue_depth{datacenter="Frankfurt",instance="web02",service="api",source="status_export"} 2
fleet_service_queue_depth{datacenter="Amsterdam",instance="web03",service="api",source="status_export"} 57
fleet_service_queue_depth{datacenter="Frankfurt",instance="web01",service="checkout",source="status_export"} 1
fleet_service_queue_depth{datacenter="Amsterdam",instance="web03",service="checkout",source="status_export"} 340
fleet_service_queue_depth{datacenter="Amsterdam",instance="srch01",service="search",source="status_export"} 4
fleet_service_queue_depth{datacenter="Sofia",instance="srch02",service="search",source="status_export"} 3
fleet_service_queue_depth{datacenter="Sofia",instance="job01",service="mailer",source="status_export"} 12
fleet_service_queue_depth{datacenter="Sofia",instance="job02",service="mailer",source="status_export"} 0
fleet_service_queue_depth{datacenter="Frankfurt",instance="web01",service="auth",source="status_export"} 0
fleet_service_queue_depth{datacenter="Frankfurt",instance="web02",service="auth",source="status_export"} 0
fleet_service_queue_depth{datacenter="Amsterdam",instance="web03",service="auth",source="status_export"} 0
# TYPE fleet_service_info gauge
# HELP fleet_service_info The version each instance runs
fleet_service_info{instance="web01",service="api",source="status_export",state="up",version="2.14.1"} 1
fleet_service_info{instance="web02",service="api",source="status_export",state="up",version="2.14.1"} 1
fleet_service_info{instance="web03",service="api",source="status_export",state="degraded",version="2.14.0"} 1
fleet_service_info{instance="web01",service="checkout",source="status_export",state="up",version="5.2.0"} 1
fleet_service_info{instance="web03",service="checkout",source="status_export",state="down",version="5.2.0"} 1
fleet_service_info{instance="srch01",service="search",source="status_export",state="up",version="1.9.3"} 1
fleet_service_info{instance="srch02",service="search",source="status_export",state="up",version="1.9.3"} 1
fleet_service_info{instance="job01",service="mailer",source="status_export",state="up",version="0.8.4"} 1
fleet_service_info{instance="job02",service="mailer",source="status_export",state="starting",version="0.8.4"} 1
fleet_service_info{instance="web01",service="auth",source="status_export",state="up",version="3.3.1"} 1
fleet_service_info{instance="web02",service="auth",source="status_export",state="up",version="3.3.1"} 1
fleet_service_info{instance="web03",service="auth",source="status_export",version="3.3.1"} 1
# EOF
`
	if response.Code != http.StatusOK || response.Body.String() != openMetrics {
		t.Errorf("as OpenMetrics: status=%d\n%s\nwant\n%s", response.Code, response.Body.String(), openMetrics)
	}
	if asked := service.requests(); !slices.Equal(asked, []string{"/service-status.csv", "/service-status.csv"}) {
		t.Errorf("the stand-in was asked %v, want /service-status.csv once a probe", asked)
	}
	loggedOnly(t, logs)
}

// A body in another encoding gives the series of its text when the
// encoding is named: by the charset of the response's Content-Type, by the
// collector's response.charset when the response names none or the wrong
// one, and by a byte order mark whatever either says. Sent without a word
// of its encoding, a windows-1251 list has no column of the names the rules
// read, which every rule reports of every row; a charset the exporter does
// not know fails the probe in the decode stage, naming it.
func TestCSVFixtureProbeAnEncodingIsNamedByTheResponseOrTheCollector(t *testing.T) {
	logs := testutil.CaptureLogs(t)
	service := newCSVStandIn(t)
	server := csvFixtureServer(t, csvProbeConfig)
	oblasti := []string{
		`city_temperature_celsius{city="София",province="София-град",state="слънчево"} 14.5`,
		`city_temperature_celsius{city="Пловдив",province="Пловдив",state="слънчево"} 17`,
		`city_temperature_celsius{city="Варна",province="Варна",state="облачно"} 18.25`,
		`city_temperature_celsius{city="Бургас",province="Бургас",state="дъжд"} 19`,
		`city_temperature_celsius{city="Русе",province="Русе",state="мъгла"} 13`,
		`city_temperature_celsius{city="Стара Загора",province="Стара Загора",state="слънчево"} 16.5`,
		`city_temperature_celsius{city="Плевен",province="Плевен",state="облачно"} 12`,
		`city_temperature_celsius{city="Велико Търново",province="Велико Търново",state="дъжд"} 11.75`,
		`city_temperature_celsius{city="Благоевград",province="Благоевград",state="слънчево"} 15`,
		`city_temperature_celsius{city="Банско, ски зона",province="Благоевград",state="сняг"} -2.5`,
		`city_humidity_percent{city="София"} 62`,
		`city_humidity_percent{city="Пловдив"} 55`,
		`city_humidity_percent{city="Варна"} 71`,
		`city_humidity_percent{city="Бургас"} 69`,
		`city_humidity_percent{city="Русе"} 74`,
		`city_humidity_percent{city="Стара Загора"} 58`,
		`city_humidity_percent{city="Плевен"} 77`,
		`city_humidity_percent{city="Велико Търново"} 80`,
		`city_humidity_percent{city="Благоевград"} 60`,
		`city_humidity_percent{city="Банско, ски зона"} 91`,
	}
	cities := []string{
		`city_temperature_celsius{city="София",country="България",sky="☀️",state="晴れ"} 14.5`,
		`city_temperature_celsius{city="Пловдив",country="България",sky="🌤",state="晴れ"} 17`,
		`city_temperature_celsius{city="Zürich",country="Schweiz",sky="☁️",state="曇り"} 9.25`,
		`city_temperature_celsius{city="São Paulo",country="Brasil",sky="🌧",state="雨"} 23`,
		`city_temperature_celsius{city="Kraków",country="Polska",sky="🌫",state="霧"} 7.5`,
		`city_temperature_celsius{city="Αθήνα",country="Ελλάδα",sky="☀️",state="晴れ"} 21`,
		`city_temperature_celsius{city="東京",country="日本",sky="☁️",state="曇り"} 19`,
		`city_temperature_celsius{city="北京",country="中国",sky="🌤",state="晴れ"} 12`,
		`city_temperature_celsius{city="Reykjavík",country="Ísland",sky="❄️",state="雪"} 3`,
		`city_temperature_celsius{city="Đà Nẵng",country="Việt Nam",sky="⛈",state="雷雨"} 29.5`,
		`city_temperature_celsius{city="İstanbul",country="Türkiye",sky="🌥",state="曇り"} 16`,
		`city_temperature_celsius{city="Québec, QC",country="Canada",sky="🌧",state="雨"} 5`,
		`city_humidity_percent{city="София"} 62`,
		`city_humidity_percent{city="Пловдив"} 55`,
		`city_humidity_percent{city="Zürich"} 78`,
		`city_humidity_percent{city="São Paulo"} 81`,
		`city_humidity_percent{city="Kraków"} 90`,
		`city_humidity_percent{city="Αθήνα"} 48`,
		`city_humidity_percent{city="東京"} 70`,
		`city_humidity_percent{city="北京"} 35`,
		`city_humidity_percent{city="Reykjavík"} 85`,
		`city_humidity_percent{city="Đà Nẵng"} 88`,
		`city_humidity_percent{city="İstanbul"} 66`,
		`city_humidity_percent{city="Québec, QC"} 74`,
	}
	types := []string{"# TYPE city_temperature_celsius gauge", "# TYPE city_humidity_percent gauge"}
	for _, tc := range []struct {
		collector, contentType string
		want                   []string
	}{
		{"oblasti", "text/csv; charset=windows-1251", oblasti},
		{"oblasti", "text/csv;charset=CP1251", oblasti},
		{"oblasti_1251", "text/csv; charset=windows-1251", oblasti},
		{"oblasti_1251", "text/csv", oblasti},
		{"oblasti_1251", "", oblasti},
		{"oblasti_1251", "text/csv; charset=utf-8", oblasti},
		{"cities", "text/csv", cities},
		{"cities", "application/octet-stream", cities},
		{"cities", "text/csv; charset=utf-8", cities},
		{"cities_be", "text/csv; charset=utf-16be", cities},
	} {
		t.Run(tc.collector+" as "+tc.contentType, func(t *testing.T) {
			service.answerAs(tc.contentType)
			answersSeries(t, answered(t, probeCSV(t, server, service, tc.collector, false)), tc.want, types)
		})
	}
	loggedOnly(t, logs)

	service.answerAs("text/csv")
	if body := answered(t, probeCSV(t, server, service, "oblasti", false)); body != "" {
		t.Errorf("a windows-1251 body that nothing names the encoding of answered\n%s", body)
	}
	target := service.URL
	// The header's names as windows-1251 writes them, read as UTF-8.
	const unnamed = `whose columns are "\xe2\xeb\xe0\xe6\xed\xee\xf1\xf2", "\xe3\xf0\xe0\xe4", "\xee\xe1\xeb\xe0\xf1\xf2", "\xf1\xfa\xf1\xf2\xee\xff\xed\xe8\xe5", "\xf2\xe5\xec\xef\xe5\xf0\xe0\xf2\xf3\xf0\xe0"; column names are matched exactly`
	loggedOnly(t, logs,
		`WARN oblasti city_temperature_celsius: 10 failed: CSV column "температура" is not in the response, `+unnamed,
		`WARN oblasti city_humidity_percent: 10 failed: CSV column "влажност" is not in the response, `+unnamed,
	)
	logs.Reset()

	service.answerAs("text/csv; charset=cp-bulgarian")
	response := probeCSV(t, server, service, "oblasti", false)
	const unknown = `collector oblasti decode failed: unsupported charset "cp-bulgarian"; use a name from the WHATWG Encoding Standard, such as utf-8, windows-1252, iso-8859-2, windows-1251, shift_jis or gbk` + "\n"
	if response.Code != http.StatusBadGateway || response.Body.String() != unknown {
		t.Errorf("a charset the exporter does not know: status=%d body=%s", response.Code, response.Body.String())
	}
	failures, others := ruleFailureLogs(t, logs)
	if len(failures) != 0 || len(others) != 1 || !strings.Contains(others[0], `"msg":"probe failed"`) || !strings.Contains(others[0], `"stage":"decode"`) || !strings.Contains(others[0], target) {
		t.Errorf("logged %v and %v, want the one failed probe, in the decode stage", failures, others)
	}
}

// A table of 5000 rows, some 280 KiB, is read whole: a series of each rule
// for every row, 10000 of them, which is the default of limits.max_metrics,
// each with its row's labels and value, the counter a counter and the
// milliseconds scaled to seconds, each rule's series together in the order
// of the rows. A third rule makes 15000 of the same
// table, which is past the limit and fails the probe in the validation
// stage, saying so; with limits.max_metrics raised to 15000 the probe
// answers them all. The same table past limits.max_response_bytes fails the
// probe before it is decoded.
func TestCSVFixtureProbeALargeFileIsReadWholeWithinItsLimits(t *testing.T) {
	logs := testutil.CaptureLogs(t)
	service := newCSVStandIn(t)
	server := csvFixtureServer(t, csvProbeConfig)
	const rows = 5000
	if size := len(generatedCSV(rows)); size < 250<<10 || size > 320<<10 {
		t.Fatalf("the generated table is %d bytes, want some 280 KiB", size)
	}
	// Each rule's series together, in the order of the rules, and each in
	// the order of the rows.
	var requests, latency, restarts []string
	for i := range rows {
		labels := fmt.Sprintf(`{host="%s",region="r%d"}`, generatedHost(i), i%7)
		requests = append(requests, "host_requests_total"+labels+" "+strconv.FormatFloat(float64(generatedRequests(i)), 'g', -1, 64))
		// Milliseconds over 1000, as scale: 0.001 divides them.
		latency = append(latency, "host_latency_seconds"+labels+" "+strconv.FormatFloat(float64(generatedLatency(i))/1000, 'g', -1, 64))
		restarts = append(restarts, "host_restarts_total"+labels+" "+strconv.Itoa(i%3))
	}
	two := slices.Concat(requests, latency)
	three := slices.Concat(two, restarts)
	types := []string{"# TYPE host_requests_total counter", "# TYPE host_latency_seconds gauge"}

	answersSeries(t, answered(t, probeCSV(t, server, service, "large", false)), two, types)
	answersSeries(t, answered(t, probeCSV(t, server, service, "large_raised", false)), three, append(types, "# TYPE host_restarts_total counter"))
	loggedOnly(t, logs)

	for collector, want := range map[string]string{
		"large_over":     "collector large_over validation failed: metric count 10001 exceeds limit 10000\n",
		"large_response": "collector large_response http failed: response size exceeds limit 65536\n",
	} {
		response := probeCSV(t, server, service, collector, false)
		if response.Code != http.StatusBadGateway || response.Body.String() != want {
			t.Errorf("%s: status=%d body=%s, want 502 and %s", collector, response.Code, firstLine(response.Body.String()), want)
		}
	}
	failures, others := ruleFailureLogs(t, logs)
	if len(failures) != 0 || len(others) != 2 || strings.Count(logs.String(), `"msg":"probe failed"`) != 2 {
		t.Errorf("logged %v and %v, want the two failed probes", failures, others)
	}
}

// firstLine is the first line of a body, for a failure's message.
func firstLine(body string) string {
	line, _, _ := strings.Cut(body, "\n")
	return line
}

// A label read from a column of free text is held to
// limits.max_label_value_length: one value past it fails the whole probe,
// in the validation stage, naming the metric and the label. With truncate:
// true on the label each longer value is cut to the limit instead, on a
// character boundary, and ends in an ellipsis that counts towards it, and
// the shorter ones are left as they are.
func TestCSVFixtureProbeALongLabelFailsTheProbeOrIsCut(t *testing.T) {
	logs := testutil.CaptureLogs(t)
	service := newCSVStandIn(t)
	server := csvFixtureServer(t, csvProbeConfig)
	const limit, rows = 40, 60

	response := probeCSV(t, server, service, "long_label", false)
	if want := "collector long_label validation failed: metric \"host_note\" label \"note\" value is 41 bytes, longer than limits.max_label_value_length 40; a label one of the collector's rules gives can be cut to fit with truncate: true on that label, or raise limits.max_label_value_length\n"; response.Code != http.StatusBadGateway || response.Body.String() != want {
		t.Errorf("without truncate: status=%d body=%s, want 502 and %s", response.Code, response.Body.String(), want)
	}
	logs.Reset()

	var want []string
	cut := 0
	for i := range rows {
		note := generatedNote(i)
		if len(note) > limit {
			// The longest start of the note that leaves room for the
			// three bytes of the ellipsis and ends with a whole character.
			keep := limit - len("…")
			for !utf8.RuneStart(note[keep]) {
				keep--
			}
			note = note[:keep] + "…"
			cut++
		}
		want = append(want, fmt.Sprintf(`host_note{host="%s",note="%s"} 1`, generatedHost(i), note))
	}
	if cut == 0 || cut == rows {
		t.Fatalf("%d of the %d notes are longer than %d bytes, want some and not all", cut, rows, limit)
	}
	body := answered(t, probeCSV(t, server, service, "cut_label", false))
	answersSeries(t, body, want, []string{"# TYPE host_note gauge"})
	// Five bytes and 16 letters of two are 37, and the ellipsis's three
	// make the 40 of the limit.
	if longest := `host_note{host="host-0029",note="note:` + strings.Repeat("ю", 16) + `…"} 1`; !strings.Contains(body, longest+"\n") {
		t.Errorf("the answer does not hold %s", longest)
	}
	loggedOnly(t, logs)
}

// What rows a rule cannot read cost it, through a probe:
//
//   - the footer of a psql result is a row without the rules' columns: the
//     probe answers every other row's series, a required rule under log is
//     logged once, at warn level, with its collector and the number of rows
//     it failed on, one under ignore and one that is not required are not;
//   - a rule with error_mode: fail fails the probe on the first cell that is
//     no number, with the JSON error naming the collector, the rule, the
//     target and the value, and nothing of the rows before it is answered;
//   - a headerless log of readings whose rule has no label that tells one
//     row of a sensor from the next makes two samples of one series, which
//     fails the probe in the validation stage;
//   - a header naming a column twice fails the probe in the decode stage,
//     naming the column and both places; under on_decode_error: log a
//     header leaving a column of values unnamed is logged, and the probe
//     is answered an empty exposition, as the text format with its
//     Content-Type and as OpenMetrics with its own and # EOF.
func TestCSVFixtureProbeRowsARuleCannotReadAreLoggedOrFailTheProbe(t *testing.T) {
	logs := testutil.CaptureLogs(t)
	service := newCSVStandIn(t)
	server := csvFixtureServer(t, csvProbeConfig)

	service.answerAs("text/plain")
	answersSeries(t, answered(t, probeCSV(t, server, service, "queues", false)), []string{
		`queue_messages_ready{queue="orders",vhost="/shop"} 120`,
		`queue_messages_ready{queue="orders.dead",vhost="/shop"} 17`,
		`queue_messages_ready{queue="payments",vhost="/shop"} 0`,
		`queue_messages_ready{queue="payments.retry",vhost="/shop"} 342`,
		`queue_messages_ready{queue="shipping",vhost="/shop"} 9`,
		`queue_messages_ready{queue="emails",vhost="/notify"} 4411`,
		`queue_messages_ready{queue="emails.bounce",vhost="/notify"} 2`,
		`queue_messages_ready{queue="sms",vhost="/notify"} 88`,
		`queue_messages_ready{queue="audit",vhost="/"} 0`,
		`queue_messages_ready{queue="metrics",vhost="/"} 51000`,
		`queue_messages_ready{queue="search.index",vhost="/shop"} 7`,
		`queue_messages_ready{queue="thumbnails",vhost="/media"} 0`,
		`queue_messages_unacked{queue="orders",vhost="/shop"} 3`,
		`queue_messages_unacked{queue="orders.dead",vhost="/shop"} 0`,
		`queue_messages_unacked{queue="payments",vhost="/shop"} 1`,
		`queue_messages_unacked{queue="payments.retry",vhost="/shop"} 12`,
		`queue_messages_unacked{queue="shipping",vhost="/shop"} 0`,
		`queue_messages_unacked{queue="emails",vhost="/notify"} 64`,
		`queue_messages_unacked{queue="emails.bounce",vhost="/notify"} 0`,
		`queue_messages_unacked{queue="sms",vhost="/notify"} 0`,
		`queue_messages_unacked{queue="audit",vhost="/"} 0`,
		`queue_messages_unacked{queue="metrics",vhost="/"} 500`,
		`queue_messages_unacked{queue="search.index",vhost="/shop"} 8`,
		`queue_messages_unacked{queue="thumbnails",vhost="/media"} 0`,
		`queue_consumers{queue="orders",vhost="/shop"} 4`,
		`queue_consumers{queue="orders.dead",vhost="/shop"} 0`,
		`queue_consumers{queue="payments",vhost="/shop"} 2`,
		`queue_consumers{queue="payments.retry",vhost="/shop"} 1`,
		`queue_consumers{queue="shipping",vhost="/shop"} 3`,
		`queue_consumers{queue="emails",vhost="/notify"} 6`,
		`queue_consumers{queue="emails.bounce",vhost="/notify"} 1`,
		`queue_consumers{queue="sms",vhost="/notify"} 0`,
		`queue_consumers{queue="audit",vhost="/"} 1`,
		`queue_consumers{queue="metrics",vhost="/"} 2`,
		`queue_consumers{queue="search.index",vhost="/shop"} 8`,
		`queue_consumers{queue="thumbnails",vhost="/media"} 0`,
		`queue_state{queue="orders",vhost="/shop"} 1`,
		`queue_state{queue="orders.dead",vhost="/shop"} 1`,
		`queue_state{queue="payments",vhost="/shop"} 1`,
		`queue_state{queue="payments.retry",vhost="/shop"} 0.5`,
		`queue_state{queue="shipping",vhost="/shop"} 1`,
		`queue_state{queue="emails",vhost="/notify"} 0.5`,
		`queue_state{queue="emails.bounce",vhost="/notify"} 1`,
		`queue_state{queue="sms",vhost="/notify"} 0`,
		`queue_state{queue="audit",vhost="/"} 1`,
		`queue_state{queue="metrics",vhost="/"} 0.5`,
		`queue_state{queue="search.index",vhost="/shop"} 1`,
		`queue_state{queue="thumbnails",vhost="/media"} 0`,
	}, []string{"# TYPE queue_messages_ready gauge", "# TYPE queue_messages_unacked gauge", "# TYPE queue_consumers gauge", "# TYPE queue_state gauge"})
	loggedOnly(t, logs,
		`WARN queues queue_messages_ready: 1 failed: CSV column "messages_ready" is empty in row 13`,
		`WARN queues queue_state: 1 failed: CSV column "state" is empty in row 13`,
	)
	logs.Reset()

	service.answerAs("text/csv")
	response := probeCSV(t, server, service, "numbers_strict", false)
	wantJSON := `{"status":"error","stage":"metric","collector":"numbers_strict","metric":"number","target":"` + service.URL +
		`","error":"metric \"number\": value \"0x1F\" is not a number; map text to numbers with value_map"}` + "\n"
	if response.Code != http.StatusBadGateway || response.Header().Get("Content-Type") != "application/json; charset=utf-8" || response.Body.String() != wantJSON {
		t.Errorf("under fail: status=%d as %s body=%s\nwant 502 and %s", response.Code, response.Header().Get("Content-Type"), response.Body.String(), wantJSON)
	}

	for collector, want := range map[string]string{
		"readings_by_sensor": `collector readings_by_sensor validation failed: duplicate metric series "reading_temperature_celsius"`,
		"usage": `collector usage decode failed: CSV header names column "used" twice, as columns 2 and 4; ` +
			`rename one, or set response.csv.header: false and read the columns by number`,
	} {
		response := probeCSV(t, server, service, collector, false)
		if response.Code != http.StatusBadGateway || response.Body.String() != want+"\n" {
			t.Errorf("%s: status=%d body=%s\nwant 502 and %s", collector, response.Code, response.Body.String(), want)
		}
	}
	failures, others := ruleFailureLogs(t, logs)
	if len(failures) != 0 || len(others) != 3 || strings.Count(logs.String(), `"msg":"probe failed"`) != 3 {
		t.Errorf("logged %v and %v, want the three failed probes", failures, others)
	}
	for _, stage := range []string{`"stage":"metric"`, `"stage":"validation"`, `"stage":"decode"`} {
		if strings.Count(logs.String(), stage) != 1 {
			t.Errorf("no failed probe was logged with %s:\n%s", stage, logs)
		}
	}
	logs.Reset()

	if body := answered(t, probeCSV(t, server, service, "usage_unnamed", false)); body != "" {
		t.Errorf("under on_decode_error: log: body=%s, want an empty exposition", body)
	}
	failures, others = ruleFailureLogs(t, logs)
	if len(failures) != 0 || len(others) != 1 || !strings.Contains(others[0], `"level":"WARN"`) ||
		!strings.Contains(others[0], "CSV header leaves column 2 unnamed, and it holds values; name it, or set response.csv.header: false and read the columns by number") {
		t.Errorf("logged %v and %v, want the one warning of the header's unnamed column", failures, others)
	}
	response = probeCSV(t, server, service, "usage_unnamed", true)
	if response.Code != http.StatusOK || response.Header().Get("Content-Type") != "application/openmetrics-text; version=1.0.0; charset=utf-8" || response.Body.String() != "# EOF\n" {
		t.Errorf("under on_decode_error: log, as OpenMetrics: status=%d as %s body=%q, want 200 and # EOF alone", response.Code, response.Header().Get("Content-Type"), response.Body.String())
	}
}

// Texts in quotes with blanks after the closing quote, as a report writer
// pads its columns, are read under trim_space: every row's series, with the
// texts as labels, a delimiter, a doubled quote and a line break in them
// kept and an empty one leaving its label off, and the numbers written to
// the right read. Without trim_space the probe fails in the decode stage at
// the header's first field, naming its line and column.
func TestCSVFixtureProbeTextsPaddedAfterTheirQuotesAreReadWithTrimSpace(t *testing.T) {
	logs := testutil.CaptureLogs(t)
	service := newCSVStandIn(t)
	server := csvFixtureServer(t, csvProbeConfig)

	answersSeries(t, answered(t, probeCSV(t, server, service, "stock", false)), []string{
		`stock_items{item="Schraube M4x20",note="ok",site="Hamburg"} 12500`,
		`stock_items{item="Schraube M6x40",note="ok",site="Hamburg"} 8400`,
		`stock_items{item="Mutter M4",site="Hamburg"} 30000`,
		`stock_items{item="Mutter M6",note="ok",site="München"} 18250`,
		`stock_items{item="Unterlegscheibe; 4,3",note="ok",site="München"} 44000`,
		`stock_items{item="Unterlegscheibe 6,4",note="nachbestellt am 01.10.",site="München"} 0`,
		`stock_items{item="Gewindestange M8",note="ok",site="Leipzig"} 320`,
		`stock_items{item="Dübel 8x40 \"Fischer\"",note="ok",site="Leipzig"} 9600`,
		`stock_items{item="Winkelverbinder 90",note="Lager 2;\nRegal 7",site="Leipzig"} 1240`,
		`stock_items{item="Kabelbinder 200 mm",note="ok",site="Hamburg"} 52000`,
		`stock_items_minimum{item="Schraube M4x20"} 2000`,
		`stock_items_minimum{item="Schraube M6x40"} 2000`,
		`stock_items_minimum{item="Mutter M4"} 5000`,
		`stock_items_minimum{item="Mutter M6"} 5000`,
		`stock_items_minimum{item="Unterlegscheibe; 4,3"} 10000`,
		`stock_items_minimum{item="Unterlegscheibe 6,4"} 10000`,
		`stock_items_minimum{item="Gewindestange M8"} 100`,
		`stock_items_minimum{item="Dübel 8x40 \"Fischer\""} 1500`,
		`stock_items_minimum{item="Winkelverbinder 90"} 400`,
		`stock_items_minimum{item="Kabelbinder 200 mm"} 8000`,
	}, []string{"# TYPE stock_items gauge", "# TYPE stock_items_minimum gauge"})
	loggedOnly(t, logs)

	response := probeCSV(t, server, service, "stock_untrimmed", false)
	const stray = "collector stock_untrimmed decode failed: CSV decode: parse error on line 1, column 9: extraneous or missing \" in quoted-field\n"
	if response.Code != http.StatusBadGateway || response.Body.String() != stray {
		t.Errorf("without trim_space: status=%d body=%s\nwant 502 and %s", response.Code, response.Body.String(), stray)
	}
	if failures, others := ruleFailureLogs(t, logs); len(failures) != 0 || len(others) != 1 || !strings.Contains(others[0], `"stage":"decode"`) {
		t.Errorf("logged %v and %v, want the one failed probe, in the decode stage", failures, others)
	}
}

// A row with a value past the header's last column, a job's name with a
// comma and no quotes here, fails the probe in the decode stage, naming the
// line and the column: its values would be read a column to the left of
// where they stand. A pre-script is not asked: the response is decoded
// before it runs. Under on_decode_error: log the failure is logged as a
// warning and the probe answers without the collector's series.
func TestCSVFixtureProbeARowLongerThanTheHeaderFailsTheDecode(t *testing.T) {
	requirePython(t)
	logs := testutil.CaptureLogs(t)
	service := newCSVStandIn(t)
	server := csvFixtureServer(t, csvProbeConfig)
	const long = "CSV line 5 has a value in column 5, which the header does not name; name the column in the header, or set response.csv.header: false and read the columns by number; " +
		"if the line is split where it should not be, check response.csv.delimiter and response.csv.trim_space"

	for _, collector := range []string{"jobs", "jobs_script"} {
		response := probeCSV(t, server, service, collector, false)
		if want := "collector " + collector + " decode failed: " + long + "\n"; response.Code != http.StatusBadGateway || response.Body.String() != want {
			t.Errorf("%s: status=%d body=%s\nwant 502 and %s", collector, response.Code, response.Body.String(), want)
		}
		failures, others := ruleFailureLogs(t, logs)
		if len(failures) != 0 || len(others) != 1 || !strings.Contains(others[0], `"msg":"probe failed"`) || !strings.Contains(others[0], `"stage":"decode"`) || !strings.Contains(others[0], long) {
			t.Errorf("%s: logged %v and %v, want the one failed probe, in the decode stage", collector, failures, others)
		}
		logs.Reset()
	}

	response := probeCSV(t, server, service, "jobs_logged", false)
	if samples, _ := sampleLines(response.Body.String()); response.Code != http.StatusOK || len(samples) != 0 {
		t.Errorf("under on_decode_error: log: status=%d body=%s, want 200 and no series", response.Code, response.Body.String())
	}
	if failures, others := ruleFailureLogs(t, logs); len(failures) != 0 || len(others) != 1 || !strings.Contains(others[0], `"level":"WARN"`) || !strings.Contains(others[0], long) {
		t.Errorf("logged %v and %v, want the one warning of the row", failures, others)
	}
}

// A cell written in one of the two forms of a number that are Go's alone,
// 1_000 and 0x1p-2, is no number through a probe: the rule makes no series
// of either row, and counts both among its failures. The numbers of the
// configuration are another matter, read as YAML writes numbers: a rule's
// scale of 1_0 is 10, its value_map gives the two texts the values 1_000
// and 0x10, a thousand and sixteen, and a series limit of 1_000 holds.
func TestCSVFixtureProbeACellInGoNumberSyntaxIsNoNumber(t *testing.T) {
	logs := testutil.CaptureLogs(t)
	service := newCSVStandIn(t)
	server := csvFixtureServer(t, csvProbeConfig)

	body := answered(t, probeCSV(t, server, service, "numbers", false))
	samples, _ := sampleLines(body)
	if len(samples) != 24 || !strings.Contains(body, "number{form=\"integer\"} 42\n") || !strings.Contains(body, "number{form=\"leading plus\"} 7\n") {
		t.Errorf("the rule made %d series, want the 24 rows that hold a number:\n%s", len(samples), body)
	}
	for _, form := range []string{"digit separators", "hexadecimal float", "hex integer"} {
		if strings.Contains(body, `form="`+form+`"`) {
			t.Errorf("the row %q was read as a number:\n%s", form, body)
		}
	}
	// 30 texts that are no numbers and 3 empty cells.
	loggedOnly(t, logs, `WARN numbers number: 33 failed: value "0x1F" is not a number; map text to numbers with value_map`)
	logs.Reset()

	body = answered(t, probeCSV(t, server, service, "numbers_mapped", false))
	for _, want := range []string{
		"number{form=\"digit separators\"} 10000\n",
		"number{form=\"hexadecimal float\"} 160\n",
		"number{form=\"integer\"} 420\n",
		"number{form=\"no integer part\"} 5\n",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the answer does not hold %s%s", want, body)
		}
	}
	if samples, _ := sampleLines(body); len(samples) != 26 {
		t.Errorf("the rule with the two texts in its value_map made %d series, want 26", len(samples))
	}
	loggedOnly(t, logs)
}

// A file whose lines end with a carriage return alone is read through a
// probe as its rows: a series of every row for each rule, a carriage return
// inside a quoted field in the label read from it, and the one empty cell
// logged with the row it is in. A log whose lines end with a carriage
// return alone, with CRLF and with a line feed, read without a header row
// under trim_space, gives a series of each of its lines, the empty line
// between them no row.
func TestCSVFixtureProbeLinesEndedByACarriageReturnAreRows(t *testing.T) {
	logs := testutil.CaptureLogs(t)
	service := newCSVStandIn(t)
	server := csvFixtureServer(t, csvProbeConfig)

	answersSeries(t, answered(t, probeCSV(t, server, service, "volumes", false)), []string{
		`volume_used_percent{pool="fast",volume="data01"} 72.5`,
		`volume_used_percent{note="resized, twice",pool="fast",volume="data02"} 31`,
		"volume_used_percent{note=\"full soon\rsee ticket 4411\",pool=\"slow\",volume=\"logs01\"} 88",
		`volume_used_percent{pool="cold",volume="backup"} 64.25`,
		`volume_free_gibibytes{volume="data01"} 220`,
		`volume_free_gibibytes{volume="data02"} 552`,
		`volume_free_gibibytes{volume="logs01"} 48`,
		`volume_free_gibibytes{volume="scratch"} 1024`,
		`volume_free_gibibytes{volume="backup"} 5120`,
	}, []string{"# TYPE volume_used_percent gauge", "# TYPE volume_free_gibibytes gauge"})
	loggedOnly(t, logs, `WARN volumes volume_used_percent: 1 failed: CSV column "used_percent" is empty in row 4`)
	logs.Reset()

	service.answerAs("text/plain")
	answersSeries(t, answered(t, probeCSV(t, server, service, "scale", false)), []string{
		`scale_weight_kilograms{at="2026-10-03T09:00:00Z",scale="A1",state="stable"} 12.5`,
		`scale_weight_kilograms{at="2026-10-03T09:01:00Z",scale="A1",state="stable"} 12.75`,
		`scale_weight_kilograms{at="2026-10-03T09:02:00Z",scale="A2",state="tare; zeroed"} 7.25`,
		`scale_weight_kilograms{at="2026-10-03T09:03:00Z",scale="A2",state="stable"} 7.5`,
		`scale_weight_kilograms{at="2026-10-03T09:05:00Z",scale="B7",state="unstable"} 0.5`,
	}, []string{"# TYPE scale_weight_kilograms gauge"})
	loggedOnly(t, logs, `WARN scale scale_weight_kilograms: 1 failed: CSV column "3" is empty in row 5`)
}

// A rule with a label that names a column the response does not have fails,
// through a probe, where the label was left off its series without a word:
// under log the rule is logged once, whatever the number of rows and though
// it is not required, with the label, the column and the columns the
// response has, and the probe answers the other rules' series and none of
// its own; under fail the probe fails with that error, in the metric stage,
// and answers nothing of the rule before it. A value's column the response
// does not have is logged with the same columns, once for the rule with the
// number of rows.
func TestCSVFixtureProbeALabelOfAColumnTheResponseLacksFailsItsRule(t *testing.T) {
	logs := testutil.CaptureLogs(t)
	service := newCSVStandIn(t)
	server := csvFixtureServer(t, csvProbeConfig)
	const has = `whose columns are "free_gib", "note", "pool", "used_percent", "volume"; column names are matched exactly`
	// The column that is the one asked for in another case comes first.
	const label = `metric "volume_used_percent" label "pool": CSV column "Pool" is not in the response, whose columns are "pool", "free_gib", "note", "used_percent", "volume"; column names are matched exactly`

	answersSeries(t, answered(t, probeCSV(t, server, service, "volumes_misnamed", false)), []string{
		`volume_free_gibibytes{volume="data01"} 220`,
		`volume_free_gibibytes{volume="data02"} 552`,
		`volume_free_gibibytes{volume="logs01"} 48`,
		`volume_free_gibibytes{volume="scratch"} 1024`,
		`volume_free_gibibytes{volume="backup"} 5120`,
	}, []string{"# TYPE volume_free_gibibytes gauge"})
	loggedOnly(t, logs,
		`WARN volumes_misnamed volume_used_percent: 1 failed: `+label,
		`WARN volumes_misnamed volume_free_gigabytes: 5 failed: CSV column "free_gb" is not in the response, `+has,
	)
	logs.Reset()

	response := probeCSV(t, server, service, "volumes_strict", false)
	wantJSON := `{"status":"error","stage":"metric","collector":"volumes_strict","metric":"volume_used_percent","target":"` + service.URL +
		`","error":` + strconv.Quote(label) + "}\n"
	if response.Code != http.StatusBadGateway || response.Header().Get("Content-Type") != "application/json; charset=utf-8" || response.Body.String() != wantJSON {
		t.Errorf("under fail: status=%d as %s body=%s\nwant 502 and %s", response.Code, response.Header().Get("Content-Type"), response.Body.String(), wantJSON)
	}
	if failures, others := ruleFailureLogs(t, logs); len(failures) != 0 || len(others) != 1 || !strings.Contains(others[0], `"msg":"probe failed"`) || !strings.Contains(others[0], `"stage":"metric"`) {
		t.Errorf("logged %v and %v, want the one failed probe, in the metric stage", failures, others)
	}
}
