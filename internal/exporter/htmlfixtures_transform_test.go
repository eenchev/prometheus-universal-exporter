//go:build !select_request_types || request_type_http

package exporter

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// Each fixture of testdata/html is read here by css collectors and by xpath
// collectors, written as a configuration file holds them, twice over: by
// decode.Decode and transform.Transform directly, and through /probe from a
// server that holds the page. The two must make the same series; what the
// probe adds is validation, which refuses series that are alike, and its own
// log lines.

// htmlReading is what one collector makes of a fixture.
type htmlReading struct {
	collector string
	// series are all the series of the transform, as the exposition writes
	// them, and logs every line it logs. A rule that carries on without
	// some of its series is logged once, with the first error and how many.
	series []string
	logs   []string
	// failure is the error of the decode or the transform, for a reading
	// that fails as a whole.
	failure string
	// probeFailure is what /probe answers 502 with, where it does not
	// answer the series: the text, or the stage, metric and error of a JSON
	// answer. probeLogs are then the lines the probe logs. A probe that
	// answers logs what the transform logs, as warnings.
	probeFailure string
	probeLogs    []string
}

// readHTMLFixture reads a fixture, served with a Content-Type or, when that
// is empty, without one, with collectors of the configuration document, and
// compares what each makes of it with its reading.
func readHTMLFixture(t *testing.T, fixture, contentType, document string, readings []htmlReading) {
	t.Helper()
	logs := testutil.CaptureLogs(t)
	cfg := htmlCollectors(t, document)
	body := htmlFixture(t, fixture)
	site := newHTMLSite(t, map[string]htmlPage{"/page": {contentType, body}})
	server := htmlServer(cfg)
	sameLogLines(t, logs, nil)
	for _, reading := range readings {
		t.Run(reading.collector, func(t *testing.T) {
			series, err := transformHTML(htmlCollector(t, cfg, reading.collector), contentType, body)
			switch {
			case err == nil && reading.failure != "":
				t.Errorf("the transform made series, want the failure %s", reading.failure)
			case err != nil && err.Error() != reading.failure:
				t.Errorf("the transform failed with\n%v\nwant\n%s", err, reading.failure)
			}
			sameSeries(t, series, reading.series)
			sameLogLines(t, logs, reading.logs)

			status, probed, failure := probeHTML(t, server, site, reading.collector, "/page")
			if reading.probeFailure != "" {
				if got := probeFailureText(failure); status != http.StatusBadGateway || got != reading.probeFailure {
					t.Errorf("the probe answered %d\n%s\nwant 502\n%s", status, got, reading.probeFailure)
				}
				sameLogLines(t, logs, reading.probeLogs)
				return
			}
			if status != http.StatusOK {
				t.Fatalf("the probe answered %d: %s", status, failure)
			}
			sameSeries(t, probed, reading.series)
			var warnings []string
			for _, line := range reading.logs {
				warnings = append(warnings, strings.Replace(line, "ERROR metric extraction failed", "WARN metric extraction failed", 1))
			}
			sameLogLines(t, logs, warnings)
		})
	}
}

// probeFailureText is the body of a failed probe: as it is when it is text,
// and the stage, the metric and the error of a JSON answer, whose target
// changes from run to run.
func probeFailureText(body string) string {
	var answer struct{ Stage, Metric, Error string }
	if !strings.HasPrefix(body, "{") || json.Unmarshal([]byte(body), &answer) != nil {
		return body
	}
	return "stage=" + answer.Stage + " metric=" + answer.Metric + " error=" + answer.Error
}

const serverStatusCollectors = `collectors:
  - name: balancer_css
    request:
      type: http
    transform:
      type: css
    metrics:
      # The rows of the body: the header's rows are in thead and the totals
      # in tfoot, which this page writes before tbody.
      - name: backend_sessions
        description: Sessions of a server now
        items: '#backends tbody tr'
        expression: td.scur
        labels:
          - name: server
            expression: td.name
            required: true
          # A row header that spans three rows is a cell of the first.
          - name: backend
            expression: th
      # A state the map does not list, MAINT, fails its row.
      - name: backend_up
        description: 1 for a server that is up, 0 for one that is down
        items: '#backends tbody tr'
        expression: td.state
        value_map: {UP: 1, DOWN: 0}
        labels:
          - name: server
            expression: td.name
      # The state is a class of the cell, which :has() asks for.
      - name: backend_weight
        items: '#backends tbody tr:has(td.state.up)'
        expression: td.weight
        labels:
          - name: server
            expression: td.name
      # What the page writes for people is no number: 1,000, 1%, 1.2 GB.
      # Each rule fails on every row but one that writes a plain 0, and is
      # logged once, or not at all when its error_mode is ignore.
      - name: backend_session_limit
        items: '#backends tbody tr'
        expression: td.slim
        labels:
          - name: server
            expression: td.name
      - name: backend_usage_percent
        items: '#backends tbody tr'
        expression: td.usage
        error_mode: ignore
        labels:
          - name: server
            expression: td.name
      - name: backend_bytes_in
        items: '#backends tbody tr'
        expression: td.bin
        labels:
          - name: server
            expression: td.name
      - name: sessions_total
        expression: '#backends tfoot td.scur'
      - name: workers
        expression: '#busy'
        labels:
          - name: state
            value: busy
      - name: workers
        expression: '#workers span#idle'
        labels:
          - name: state
            value: idle
      # A table without classes is read by the place of its cells.
      - name: worker_cpu_seconds
        type: counter
        items: '#scoreboard tr:has(td)'
        expression: td:nth-child(5)
        labels:
          - name: slot
            expression: td:nth-child(1)
          - name: pid
            expression: td:nth-child(2)
          - name: vhost
            expression: td:nth-child(9)

  # Counted by their place, the cells of a row under a cell that spans it
  # are one further left: the selector is right and the table is not a grid.
  - name: balancer_css_by_place
    request:
      type: http
    transform:
      type: css
    metrics:
      - name: backend_sessions
        items: '#backends tbody tr'
        expression: td:nth-child(3)
        labels:
          - name: server
            expression: td:nth-child(2)
      # Every row with cells, the totals of tfoot first, as the page has them.
      - name: backend_sessions_max
        items: '#backends tr:has(td)'
        expression: td.smax
        labels:
          - name: server
            expression: td.name

  - name: balancer_xpath
    request:
      type: http
    decoder:
      type: html
    transform:
      type: xpath
    metrics:
      - name: backend_sessions
        description: Sessions of a server now
        expression: "//table[@id='backends']/tbody/tr/td[@class='scur']"
        labels:
          - name: server
            expression: ../@data-server
            required: true
          - name: backend
            expression: ../@data-backend
          - name: state
            expression: "../td[contains(@class, 'state')]/@class"
            value_map: {state up: up, state down: down, state maint: maintenance}
          - name: check
            expression: "../td[contains(@class, 'state')]/@title"
      # A predicate keeps the rows whose state cell has the class up.
      - name: backend_weight
        expression: "//table[@id='backends']/tbody/tr[td[contains(concat(' ', normalize-space(@class), ' '), ' up ')]]/td[@class='weight']"
        labels:
          - name: server
            expression: ../@data-server
      - name: backends_up
        expression: "count(//table[@id='backends']/tbody/tr[td[@class='state up']])"
      # The exact number is in an attribute beside the text for people.
      - name: bytes_in_total
        type: counter
        expression: "//table[@id='backends']/tfoot/tr/td[@class='bin']/@data-value"
      - name: bytes_out_total
        type: counter
        expression: "sum(//table[@id='backends']/tbody/tr/td[@class='bout']/@data-value)"
      # One value can be computed out of its unit and its separators.
      - name: session_limit_total
        expression: "number(translate(//table[@id='backends']/tfoot/tr/td[@class='slim'], ',', ''))"
      - name: usage_ratio
        expression: "number(substring-before(//table[@id='backends']/tfoot/tr/td[@class='usage'], '%'))"
        scale: 0.01
      # mod_status writes its numbers in sentences, and stub_status in a pre.
      - name: server_load1
        expression: "number(substring-before(substring-after(//dt[starts-with(., 'Server load: ')], 'Server load: '), ' '))"
      - name: accesses_total
        type: counter
        expression: "number(substring-before(substring-after(//dt[starts-with(., 'Total accesses: ')], 'Total accesses: '), ' '))"
      # normalize-space makes one line of the pre, whatever ends its lines.
      - name: connections_active
        expression: "number(substring-before(substring-after(normalize-space(//pre), 'Active connections: '), ' '))"
      - name: worker_cpu_seconds
        type: counter
        expression: "//table[@id='scoreboard']//tr/td[5]"
        labels:
          - name: slot
            expression: ../td[1]
          - name: vhost
            expression: ../td[9]
      # What is written for people is no number here either.
      - name: backend_session_limit
        expression: "//table[@id='backends']/tbody/tr/td[@class='slim']"
        labels:
          - name: server
            expression: ../@data-server
`

// testdata/html/server-status.html stands for the page a load balancer or a
// web server reports itself on: a statistics table in the manner of HAProxy's,
// with a header of two rows whose cells span, the totals in a tfoot written
// before the tbody, row headers that span the servers of a backend, numbers
// written for people (1,000, 1%, 1.2 GB) with the exact ones in data-value and
// title attributes, and the state as a class of its cell; above it the
// sentences of Apache's mod_status and a pre as nginx's stub_status answers.
//
// css reads the rows with items and the cells by their classes, keeps rows by
// their state with :has(), and fails, once a rule, on what is no number. xpath
// reads the same rows with the labels from attributes of the row, a state read
// from a class and named by the label's value_map, a predicate on the class,
// the exact byte counts from the attributes, and computes single values out of
// their units and out of running text, which css cannot. A selector does not
// read attributes, so the byte counts are not for css.
func TestTheServerStatusFixtureIsReadByCSSAndXPath(t *testing.T) {
	readHTMLFixture(t, "server-status.html", "text/html; charset=utf-8", serverStatusCollectors, []htmlReading{
		{
			collector: "balancer_css",
			series: []string{
				`backend_sessions{backend="web",server="web01"} 12`,
				`backend_sessions{server="web02"} 31`,
				`backend_sessions{server="web03"} 0`,
				`backend_sessions{backend="api",server="api01"} 1`,
				`backend_up{server="web01"} 1`,
				`backend_up{server="web02"} 1`,
				`backend_up{server="web03"} 0`,
				`backend_weight{server="web01"} 100`,
				`backend_weight{server="web02"} 100`,
				`backend_bytes_in{server="web03"} 0`,
				`sessions_total 44`,
				`workers{state="busy"} 7`,
				`workers{state="idle"} 93`,
				`worker_cpu_seconds{pid="30411",slot="0-2",vhost="www.example.net:443"} 4.25`,
				`worker_cpu_seconds{pid="30412",slot="1-2",vhost="www.example.net:443"} 3.98`,
				`worker_cpu_seconds{pid="-",slot="2-2"} 0`,
			},
			logs: []string{
				`ERROR metric extraction failed metric=backend_up failures=1 error=metric "backend_up" item 3: value "MAINT" is neither in value_map nor a number; add it to value_map, or map "*" for any other value`,
				`ERROR metric extraction failed metric=backend_session_limit failures=4 error=metric "backend_session_limit" item 0: value "1,000" is not a number; map text to numbers with value_map`,
				`ERROR metric extraction failed metric=backend_bytes_in failures=3 error=metric "backend_bytes_in" item 0: value "1.2 GB" is not a number; map text to numbers with value_map`,
			},
		},
		{
			collector: "balancer_css_by_place",
			series: []string{
				`backend_sessions{server="web01"} 12`,
				`backend_sessions{server="31"} 166`,
				`backend_sessions{server="0"} 0`,
				`backend_sessions{server="api01"} 1`,
				`backend_sessions_max 251`,
				`backend_sessions_max{server="web01"} 85`,
				`backend_sessions_max{server="web02"} 166`,
				`backend_sessions_max{server="web03"} 0`,
				`backend_sessions_max{server="api01"} 12`,
			},
		},
		{
			collector: "balancer_xpath",
			series: []string{
				`backend_sessions{backend="web",check="L7OK/200 in 3ms",server="web01",state="up"} 12`,
				`backend_sessions{backend="web",check="L7OK/200 in 5ms",server="web02",state="up"} 31`,
				`backend_sessions{backend="web",check="L4CON in 2001ms: Connection refused",server="web03",state="down"} 0`,
				`backend_sessions{backend="api",server="api01",state="maintenance"} 1`,
				`backend_weight{server="web01"} 100`,
				`backend_weight{server="web02"} 100`,
				`backends_up 2`,
				`bytes_in_total 5.731516212e+09`,
				`bytes_out_total 8.921445591e+10`,
				`session_limit_total 4000`,
				`usage_ratio 0.01`,
				`server_load1 0.42`,
				`accesses_total 1.234567e+06`,
				`connections_active 291`,
				`worker_cpu_seconds{slot="0-2",vhost="www.example.net:443"} 4.25`,
				`worker_cpu_seconds{slot="1-2",vhost="www.example.net:443"} 3.98`,
				`worker_cpu_seconds{slot="2-2"} 0`,
			},
			logs: []string{
				`ERROR metric extraction failed metric=backend_session_limit failures=4 error=value "1,000" is not a number; map text to numbers with value_map`,
			},
		},
	})
}

const nestedTablesCollectors = `collectors:
  - name: ups_css
    request:
      type: http
    transform:
      type: css
    metrics:
      # Both tables have the id stats: the heading before each tells them apart.
      - name: ups_input_volts
        items: 'h2:contains("Input") + table tr.row'
        expression: td.value
        labels:
          - name: phase
            expression: td.name
      - name: ups_output_volts
        items: 'h2:contains("Output") + table tr.row'
        expression: td.value
        labels:
          - name: phase
            expression: td.name
      # A pack's row holds a table of cells, whose rows have value cells
      # too: the pack's own is the cell after the one that holds the table.
      - name: ups_battery_pack_volts
        items: '#battery > tbody > tr.row'
        expression: td.cells + td.value
        labels:
          - name: pack
            expression: td.name:not(.inner td)
      # A selector reads within its row, so a cell cannot be named by the
      # pack around it: one rule a pack, each naming its own.
      - name: ups_battery_cell_volts
        items: '#battery > tbody > tr.row:nth-child(1) table.inner tr'
        expression: td.value
        labels:
          - name: cell
            expression: td.name
          - name: pack
            value: Pack A
      - name: ups_battery_cell_volts
        items: '#battery > tbody > tr.row:nth-child(2) table.inner tr'
        expression: td.value
        labels:
          - name: cell
            expression: td.name
          - name: pack
            value: Pack B
      - name: ups_runtime_minutes
        expression: '#runtime'
      # Without items an expression is one value, and these match several:
      # the id used twice, and a class the whole page uses.
      - name: ups_total_volts
        expression: '#total'
      - name: ups_any_value
        expression: .value
        error_mode: ignore
      # Within a pack's row td.value is three cells, its own and its cells'.
      - name: ups_battery_volts
        items: '#battery > tbody > tr.row'
        expression: td.value
        labels:
          - name: pack
            expression: td.name:not(.inner td)
      # The rows of the layout table hold every table inside them.
      - name: ups_layout
        items: 'table.layout > tbody > tr'
        expression: td.current
        required: false

  # Every cell of every pack by one rule: two cells are Cell 1 and two are
  # Cell 2, and nothing in their rows says which pack they are in.
  - name: ups_css_cells
    request:
      type: http
    transform:
      type: css
    metrics:
      - name: ups_battery_cell_volts
        items: '#battery table.inner tr'
        expression: td.value
        labels:
          - name: cell
            expression: td.name

  - name: ups_xpath
    request:
      type: http
    decoder:
      type: html
    transform:
      type: xpath
    metrics:
      # Both tables at once, each row named by the heading before its table.
      - name: ups_volts
        expression: "//table[@id='stats']/tbody/tr[@class='row']/td[@class='value']"
        labels:
          - name: phase
            expression: "../td[@class='name']"
          - name: side
            expression: "ancestor::table[1]/preceding-sibling::h2[1]"
            value_map: {Input: input, Output: output}
      - name: ups_total_volts
        expression: "//td[@id='total']"
        labels:
          - name: side
            expression: "ancestor::table[1]/preceding-sibling::h2[1]"
      # A child step stays in the table it starts in.
      - name: ups_battery_pack_volts
        expression: "//table[@id='battery']/tbody/tr/td[@class='value']"
        labels:
          - name: pack
            expression: "../td[@class='name']"
      # A cell is named by its own row and by the pack's row around it.
      - name: ups_battery_cell_volts
        expression: "//table[@class='inner']/tbody/tr/td[@class='value']"
        labels:
          - name: cell
            expression: "../td[@class='name']"
          - name: pack
            expression: "ancestor::tr[2]/td[@class='name']"
      - name: ups_battery_cells
        expression: "count(//table[@class='inner']//tr)"
      - name: ups_runtime_minutes
        expression: "//span[@id='runtime']"
`

// testdata/html/nested-tables.html stands for the status page of an appliance,
// a UPS network card: a layout table around everything, two data tables that
// share one id, and a table of battery packs whose rows hold a table of cells.
//
// css picks each table by the heading before it, a pack's own value by its
// place beside the nested table, and fails as documented where an expression
// matches several elements: without items, and within an item. It cannot name
// a nested row by the row around it, so one rule reads one pack, and the rule
// that reads the cells of every pack makes series that are alike, which the
// probe refuses. xpath reads both tables at once and names each row by the
// heading before its table, and each cell by its pack, with the ancestor axis.
func TestTheNestedTablesFixtureIsReadByCSSAndXPath(t *testing.T) {
	readHTMLFixture(t, "nested-tables.html", "text/html", nestedTablesCollectors, []htmlReading{
		{
			collector: "ups_css",
			series: []string{
				`ups_input_volts{phase="L1"} 229.8`,
				`ups_input_volts{phase="L2"} 231.2`,
				`ups_input_volts{phase="L3"} 230.5`,
				`ups_output_volts{phase="L1"} 230`,
				`ups_output_volts{phase="L2"} 230.1`,
				`ups_output_volts{phase="L3"} 229.9`,
				`ups_battery_pack_volts{pack="Pack A"} 27.1`,
				`ups_battery_pack_volts{pack="Pack B"} 26.3`,
				`ups_battery_cell_volts{cell="Cell 1",pack="Pack A"} 13.6`,
				`ups_battery_cell_volts{cell="Cell 2",pack="Pack A"} 13.5`,
				`ups_battery_cell_volts{cell="Cell 1",pack="Pack B"} 13.4`,
				`ups_battery_cell_volts{cell="Cell 2",pack="Pack B"} 12.9`,
				`ups_runtime_minutes 47`,
			},
			logs: []string{
				`ERROR metric extraction failed metric=ups_total_volts failures=1 error=metric "ups_total_volts" CSS selector "#total" matched 2 elements, but without items a css metric is one value; set items to the elements, such as '#servers tr:has(td)', and select the value and each label within one`,
				`ERROR metric extraction failed metric=ups_battery_volts failures=2 error=metric "ups_battery_volts" item 0: CSS selector "td.value" matched 3 elements; within an item it must match at most one`,
				`ERROR metric extraction failed metric=ups_layout failures=1 error=metric "ups_layout" item 2: CSS selector "td.current" matched 8 elements; within an item it must match at most one`,
			},
		},
		{
			collector: "ups_css_cells",
			series: []string{
				`ups_battery_cell_volts{cell="Cell 1"} 13.6`,
				`ups_battery_cell_volts{cell="Cell 2"} 13.5`,
				`ups_battery_cell_volts{cell="Cell 1"} 13.4`,
				`ups_battery_cell_volts{cell="Cell 2"} 12.9`,
			},
			probeFailure: `collector ups_css_cells validation failed: duplicate metric series "ups_battery_cell_volts"`,
			probeLogs: []string{
				`ERROR probe failed stage=validation error=duplicate metric series "ups_battery_cell_volts"`,
			},
		},
		{
			collector: "ups_xpath",
			series: []string{
				`ups_volts{phase="L1",side="input"} 229.8`,
				`ups_volts{phase="L2",side="input"} 231.2`,
				`ups_volts{phase="L3",side="input"} 230.5`,
				`ups_volts{phase="L1",side="output"} 230`,
				`ups_volts{phase="L2",side="output"} 230.1`,
				`ups_volts{phase="L3",side="output"} 229.9`,
				`ups_total_volts{side="Input"} 691.5`,
				`ups_total_volts{side="Output"} 690`,
				`ups_battery_pack_volts{pack="Pack A"} 27.1`,
				`ups_battery_pack_volts{pack="Pack B"} 26.3`,
				`ups_battery_cell_volts{cell="Cell 1",pack="Pack A"} 13.6`,
				`ups_battery_cell_volts{cell="Cell 2",pack="Pack A"} 13.5`,
				`ups_battery_cell_volts{cell="Cell 1",pack="Pack B"} 13.4`,
				`ups_battery_cell_volts{cell="Cell 2",pack="Pack B"} 12.9`,
				`ups_battery_cells 4`,
				`ups_runtime_minutes 47`,
			},
		},
	})
}

const listsAndCardsCollectors = `collectors:
  - name: queue_css
    request:
      type: http
    transform:
      type: css
    metrics:
      # A dt and its dd are siblings: within a pair wrapped in a div they
      # are the cells of one item, :not() leaving out the pair switched off.
      - name: queue_limit
        items: '#limits div.pair:not(.disabled)'
        expression: dd
        labels:
          - name: name
            expression: dt
      # Without the wrapper a value is found from its dt, one rule a value:
      # the dd right after the dt that says so.
      - name: queue_workers
        expression: '#build dt:contains("Workers") + dd'
      - name: queue_uptime_seconds
        type: counter
        expression: '#build dl dd:nth-of-type(3)'
      - name: queue_open_files
        expression: '#build dt:contains("Open files") + dd'
      # An empty dd is a missing value, which an optional rule leaves out.
      - name: queue_last_error_code
        expression: '#build dt:contains("Last error") + dd'
        required: false
      # A list of name and value: the items that have the attribute, the
      # value right after the name.
      - name: queue_jobs
        items: 'ul.stats li[data-queue]'
        expression: span.k + span.v
        labels:
          - name: queue
            expression: span.k
      # The same by ~, any later sibling, which reads the item that has
      # words between the name and the value too.
      - name: queue_jobs_waiting
        items: 'ul.stats li[data-queue]'
        expression: span.k ~ span.v
        labels:
          - name: queue
            expression: span.k
          - name: badge
            expression: span.badge
      - name: queue_paused
        items: 'ul.stats li:has(span.badge)'
        expression: span.badge
        value_map: {paused: 1}
        labels:
          - name: queue
            expression: span:first-child
      # A grid of cards: an attribute's presence, its value, its start and
      # its end choose the cards, and :nth-child() the divs within one.
      - name: queue_card
        items: 'div.grid div.card[data-metric]:not([hidden])'
        expression: div.card-value
        labels:
          - name: title
            expression: div.card-title
      - name: queue_latency_seconds
        items: 'div.card[data-unit="ms"]'
        expression: div:nth-child(2)
        scale: 0.001
      - name: queue_card_jobs
        items: 'div.card[data-unit^="job"], div.card[data-metric$="_ops"]'
        expression: div:nth-child(2)
        labels:
          - name: title
            expression: div:nth-child(1)
      - name: queue_card_trend
        items: 'div.card:has(div.card-trend.up)'
        expression: div.card-trend
        labels:
          - name: title
            expression: div.card-title
      - name: queue_render_seconds
        expression: '#render-ms'
        scale: 0.001
      # The item itself cannot be the value: selectors read beneath it.
      - name: queue_top_job_runs
        items: 'ol.top-jobs li'
        expression: li

  - name: queue_xpath
    request:
      type: http
    decoder:
      type: html
    transform:
      type: xpath
    metrics:
      # Every dd that is a number, named by the dt before it; the version
      # and the revision are no numbers, and the rule says to pass them by.
      - name: queue_build
        expression: "//section[@id='build']/dl/dd[not(@class)]"
        required: false
        error_mode: ignore
        labels:
          - name: name
            expression: preceding-sibling::dt[1]
      - name: queue_limit
        expression: "//section[@id='limits']//div[@class='pair']/dd"
        labels:
          - name: name
            expression: ../dt
      - name: queue_jobs
        expression: "//ul[@class='stats']/li[@data-queue]/span[@class='v']"
        labels:
          - name: queue
            expression: ../@data-queue
          - name: state
            expression: ../@class
          - name: badge
            expression: "../span[@class='badge']"
      # The items of the ordered list are their own values, once the name
      # before the colon is cut off: one rule an item, each computing its own.
      - name: queue_top_job_runs
        expression: "number(substring-after(//ol[@class='top-jobs']/li[1], ': '))"
        labels:
          - name: job
            expression: "substring-before(//ol[@class='top-jobs']/li[1], ':')"
      - name: queue_top_job_runs
        expression: "number(substring-after(//ol[@class='top-jobs']/li[2], ': '))"
        labels:
          - name: job
            expression: "substring-before(//ol[@class='top-jobs']/li[2], ':')"
      - name: queue_top_jobs
        expression: "count(//ol[@class='top-jobs']/li)"
      - name: queue_card
        expression: "//div[contains(concat(' ', @class, ' '), ' card ')][@data-metric]/div[@class='card-value']"
        labels:
          - name: metric
            expression: ../@data-metric
          - name: unit
            expression: ../@data-unit
          - name: hidden
            expression: boolean(../@hidden)
          - name: trend
            expression: following-sibling::div[1]/@class
            value_map: {card-trend up: up, card-trend down: down}
      - name: queue_cards_shown
        expression: "count(//div[@class='grid']/div[not(@hidden)])"
`

// testdata/html/lists-and-cards.html stands for the overview page of a job
// queue: definition lists with and without a wrapper around each pair, a list
// whose items are a name and a value in spans, an ordered list whose items are
// "name: value" in one text, and a grid of cards built from divs.
//
// css reads them with attribute selectors, :not(), :has(), :nth-child(),
// :nth-of-type(), :contains() and the sibling combinators + and ~. It reads
// what is beneath an item, so an item that is its own value has none for it.
// xpath names a dd by the dt before it, reads attributes as labels, and
// computes the items that are "name: value".
func TestTheListsAndCardsFixtureIsReadByCSSAndXPath(t *testing.T) {
	readHTMLFixture(t, "lists-and-cards.html", "text/html; charset=utf-8", listsAndCardsCollectors, []htmlReading{
		{
			collector: "queue_css",
			series: []string{
				`queue_limit{name="max_jobs"} 500`,
				`queue_limit{name="max_retries"} 5`,
				`queue_limit{name="timeout_seconds"} 30`,
				`queue_workers 16`,
				`queue_uptime_seconds 1.053067e+06`,
				`queue_open_files 212`,
				`queue_jobs{queue="default"} 128`,
				`queue_jobs{queue="mailers"} 7`,
				`queue_jobs{queue="low"} 2043`,
				`queue_jobs{queue="dead"} 0`,
				`queue_jobs_waiting{queue="default"} 128`,
				`queue_jobs_waiting{queue="mailers"} 7`,
				`queue_jobs_waiting{badge="paused",queue="low"} 2043`,
				`queue_jobs_waiting{queue="dead"} 0`,
				`queue_jobs_waiting{queue="scheduled"} 31`,
				`queue_paused{queue="low"} 1`,
				`queue_card{title="Processed"} 1.284551e+06`,
				`queue_card{title="Failed"} 412`,
				`queue_card{title="Latency"} 87.5`,
				`queue_card{title="Memory"} 7.340032e+08`,
				`queue_latency_seconds 0.0875`,
				`queue_card_jobs{title="Processed"} 1.284551e+06`,
				`queue_card_jobs{title="Failed"} 412`,
				`queue_card_jobs{title="Redis ops"} 0`,
				`queue_card_trend{title="Processed"} 2.1`,
				`queue_card_trend{title="Latency"} 12`,
				`queue_render_seconds 0.0032`,
			},
			logs: []string{
				`ERROR metric extraction failed metric=queue_jobs failures=1 error=metric "queue_jobs" value is missing for item 4: CSS selector "span.k + span.v" matched nothing`,
				`ERROR metric extraction failed metric=queue_top_job_runs failures=3 error=metric "queue_top_job_runs" value is missing for item 0: CSS selector "li" matched nothing`,
			},
		},
		{
			collector: "queue_xpath",
			series: []string{
				`queue_build{name="Uptime (s)"} 1.053067e+06`,
				`queue_build{name="Workers"} 16`,
				`queue_build{name="Open files"} 212`,
				`queue_limit{name="max_jobs"} 500`,
				`queue_limit{name="max_retries"} 5`,
				`queue_limit{name="timeout_seconds"} 30`,
				`queue_jobs{queue="default"} 128`,
				`queue_jobs{queue="mailers"} 7`,
				`queue_jobs{badge="paused",queue="low",state="paused"} 2043`,
				`queue_jobs{queue="dead",state="dead"} 0`,
				`queue_jobs{queue="scheduled"} 31`,
				`queue_top_job_runs{job="ReportJob"} 4012`,
				`queue_top_job_runs{job="ThumbnailJob"} 2977`,
				`queue_top_jobs 3`,
				`queue_card{hidden="false",metric="processed",trend="up",unit="jobs"} 1.284551e+06`,
				`queue_card{hidden="false",metric="failed",trend="down",unit="jobs"} 412`,
				`queue_card{hidden="false",metric="latency",trend="up",unit="ms"} 87.5`,
				`queue_card{hidden="false",metric="memory",unit="bytes"} 7.340032e+08`,
				`queue_card{hidden="true",metric="redis_ops",unit="ops"} 0`,
				`queue_cards_shown 5`,
			},
		},
	})
}

const sloppyCollectors = `collectors:
  - name: pdu_css
    request:
      type: http
    transform:
      type: css
    metrics:
      - name: pdu_load_amps
        expression: '#load'
      # Of an attribute written twice the first is kept: the id is ambient,
      # the class grid, and neither second nor ignored is there.
      - name: pdu_ambient_celsius
        expression: '#ambient'
      - name: pdu_ambient_by_second_id
        expression: '#second'
        required: false
      - name: pdu_grid_rows
        items: 'table.ignored tr'
        expression: td
        required: false
      # A selector's element name is matched in any case; an id and a class
      # are matched as the page writes them.
      - name: pdu_ambient_celsius_upper_case_element
        expression: SPAN.temp
      - name: pdu_ambient_celsius_upper_case_id
        expression: '#AMBIENT'
        required: false
      # Cells and rows nobody closed are cells and rows all the same, and
      # the one without a number is its row's missing value.
      - name: pdu_outlet_amps
        items: '#outlets tr:has(td)'
        expression: td.amps
        labels:
          - name: outlet
            expression: td:nth-child(1)
          - name: name
            expression: td.name
      - name: pdu_outlet_on
        items: '#outlets tr:has(td)'
        expression: td:nth-child(3)
        value_map: {"On": 1, "Off": 0}
        labels:
          - name: name
            expression: td.name
      # The page has no tbody, and the document has: its rows are not
      # children of the table.
      - name: pdu_outlet_amps_table_child
        items: 'table > tr'
        expression: td.amps
        required: false
      - name: pdu_outlet_amps_tbody_child
        items: 'table > tbody > tr:has(td.on)'
        expression: td.amps
        labels:
          - name: name
            expression: td.name
      # A fan's name is words beside its number, in no element of their
      # own, so no selector reads it: one rule a fan, each naming its own.
      - name: pdu_fan_rpm
        expression: '#fans li:nth-child(1) i'
        labels:
          - name: fan
            value: Fan 1
      - name: pdu_fan_rpm
        expression: '#fans li:nth-child(2) i'
        labels:
          - name: fan
            value: Fan 2
      - name: pdu_fans_failed
        items: '#fans li.failed'
        expression: i
        value_map: {"0": 1}
      - name: pdu_uptime_seconds
        type: counter
        expression: '#uptime'
      # What follows the end of the body and of the page is in the body.
      - name: pdu_proxy_hops
        expression: 'body #proxy'

  - name: pdu_xpath
    request:
      type: http
    decoder:
      type: html
    transform:
      type: xpath
    metrics:
      # Element and attribute names are lower case in the document, however
      # the page writes them, and an expression says them that way.
      - name: pdu_load_amps
        expression: "//b[@id='load']"
      - name: pdu_load_amps_upper_case
        expression: "//B[@ID='load']"
        required: false
      - name: pdu_outlet_amps
        expression: "//table[@id='outlets']/tbody/tr/td[@class='amps']"
        required: false
        labels:
          - name: outlet
            expression: ../td[1]
          - name: name
            expression: "../td[@class='name']"
          - name: state
            expression: ../td[3]/@class
          - name: table
            expression: ../../../@class
          - name: width
            expression: ../../../@width
          - name: width_upper_case
            expression: ../../../@WIDTH
      - name: pdu_outlet_amps_table_child
        expression: "//table[@id='outlets']/tr/td[@class='amps']"
        required: false
      - name: pdu_fan_rpm
        expression: "//ul[@id='fans']/li/i"
        labels:
          - name: fan
            expression: "normalize-space(substring-before(.., ':'))"
          - name: state
            expression: ../@class
      - name: pdu_ambient_celsius
        expression: "//span[@id='ambient']"
      - name: pdu_ambient_by_second_id
        expression: "//span[@id='second']"
        required: false
      - name: pdu_generator
        expression: "count(//meta[@name='GENERATOR'][@content='RackWare 2.1'])"
      - name: pdu_proxy_hops
        expression: "/html/body//span[@id='proxy']"
      - name: pdu_paragraphs
        expression: "count(/html/body/p)"
`

// testdata/html/sloppy.html stands for a page an embedded device writes: tags
// and attributes in upper case, cells, rows, paragraphs and list items never
// closed, no tbody, end tags for elements that were never open, attributes
// without quotes, an attribute written twice, text after the end of the page.
// It is parsed as a browser parses it, and both transforms read the document
// that makes: names in lower case, the tbody the page left out, the first of
// two attributes of one name.
func TestTheSloppyFixtureIsReadAsABrowserParsesIt(t *testing.T) {
	readHTMLFixture(t, "sloppy.html", "text/html", sloppyCollectors, []htmlReading{
		{
			collector: "pdu_css",
			series: []string{
				`pdu_load_amps 11.4`,
				`pdu_ambient_celsius 23.5`,
				`pdu_ambient_celsius_upper_case_element 23.5`,
				`pdu_outlet_amps{name="core-sw-1",outlet="1"} 1.2`,
				`pdu_outlet_amps{name="core-sw-2",outlet="2"} 1.3`,
				`pdu_outlet_amps{name="storage-a",outlet="3"} 0`,
				`pdu_outlet_amps{name="db-primary",outlet="4"} 4.6`,
				`pdu_outlet_on{name="core-sw-1"} 1`,
				`pdu_outlet_on{name="core-sw-2"} 1`,
				`pdu_outlet_on{name="storage-a"} 0`,
				`pdu_outlet_on{name="db-primary"} 1`,
				`pdu_outlet_on{name="spare"} 0`,
				`pdu_outlet_amps_tbody_child{name="core-sw-1"} 1.2`,
				`pdu_outlet_amps_tbody_child{name="core-sw-2"} 1.3`,
				`pdu_outlet_amps_tbody_child{name="db-primary"} 4.6`,
				`pdu_fan_rpm{fan="Fan 1"} 4200`,
				`pdu_fan_rpm{fan="Fan 2"} 4150`,
				`pdu_fans_failed 1`,
				`pdu_uptime_seconds 86400`,
				`pdu_proxy_hops 3`,
			},
			logs: []string{
				`ERROR metric extraction failed metric=pdu_outlet_amps failures=1 error=metric "pdu_outlet_amps" value is missing for item 4: CSS selector "td.amps" matched an element without a value`,
			},
		},
		{
			collector: "pdu_xpath",
			series: []string{
				`pdu_load_amps 11.4`,
				`pdu_outlet_amps{name="core-sw-1",outlet="1",state="on",table="grid",width="80%"} 1.2`,
				`pdu_outlet_amps{name="core-sw-2",outlet="2",state="on",table="grid",width="80%"} 1.3`,
				`pdu_outlet_amps{name="storage-a",outlet="3",state="off",table="grid",width="80%"} 0`,
				`pdu_outlet_amps{name="db-primary",outlet="4",state="on",table="grid",width="80%"} 4.6`,
				`pdu_fan_rpm{fan="Fan 1"} 4200`,
				`pdu_fan_rpm{fan="Fan 2"} 4150`,
				`pdu_fan_rpm{fan="Fan 3",state="failed"} 0`,
				`pdu_ambient_celsius 23.5`,
				`pdu_generator 1`,
				`pdu_proxy_hops 3`,
				`pdu_paragraphs 6`,
			},
		},
	})
}

const fragmentCollectors = `collectors:
  - name: widget_css
    request:
      type: http
    transform:
      type: css
    metrics:
      - name: widget_value
        items: div.widget
        expression: span.value
        labels:
          - name: widget
            expression: span.label
      # The fragment is given the html and the body it does not have.
      - name: widget_value_in_body
        items: 'html > body > div#pool'
        expression: span.value
      # A row outside any table is no row: its text is left, its cell is not.
      - name: widget_orphan
        expression: .orphan
        required: false

  - name: widget_xpath
    request:
      type: http
    decoder:
      type: html
    transform:
      type: xpath
    metrics:
      - name: widget_value
        expression: "/html/body/div[@class='widget']/span[@class='value']"
        labels:
          - name: widget
            expression: ../@id
      - name: widget_orphan
        expression: "//td"
        required: false
      - name: widget_count
        expression: "count(//div[@class='widget'])"

  # With the decoder left to each answer the fragment is HTML when the
  # target says so, and other markup, XML, when it does not.
  - name: widget_xpath_auto
    request:
      type: http
    decoder:
      type: auto
    transform:
      type: xpath
    metrics:
      - name: widget_value
        expression: "//div[@class='widget']/span[@class='value']"
        labels:
          - name: widget
            expression: ../@id
      - name: widget_in_body
        expression: "count(/html/body/div)"
`

// testdata/html/fragment.html stands for what an include or an AJAX endpoint
// answers: elements without html or body around them. Read as HTML it is given
// both. With the decoder left to each answer it is HTML when the target says
// so; when the target says nothing it is other markup, read as XML, in which
// there is no body to find.
func TestTheFragmentFixtureIsGivenABody(t *testing.T) {
	readHTMLFixture(t, "fragment.html", "text/html", fragmentCollectors, []htmlReading{
		{
			collector: "widget_css",
			series: []string{
				`widget_value{widget="Connections"} 42`,
				`widget_value{widget="Cache hits"} 97`,
				`widget_value_in_body 42`,
			},
		},
		{
			collector: "widget_xpath",
			series: []string{
				`widget_value{widget="pool"} 42`,
				`widget_value{widget="cache"} 97`,
				`widget_count 2`,
			},
		},
		{
			collector: "widget_xpath_auto",
			series: []string{
				`widget_value{widget="pool"} 42`,
				`widget_value{widget="cache"} 97`,
				`widget_in_body 2`,
			},
		},
	})
	// Without a Content-Type the fragment is no HTML page by its content.
	readHTMLFixture(t, "fragment.html", "", fragmentCollectors, []htmlReading{
		{
			collector: "widget_xpath_auto",
			series: []string{
				`widget_value{widget="pool"} 42`,
				`widget_value{widget="cache"} 97`,
				`widget_in_body 0`,
			},
		},
	})
}

const textOnlyCollectors = `collectors:
  - name: health_css
    request:
      type: http
    transform:
      type: css
    metrics:
      # The page is its text, in the body the parser gives it: no number.
      - name: health_workers
        expression: body
      - name: health_ok
        expression: body
        value_map: {OK 17 workers ready: 1, "*": 0}

  - name: health_xpath
    request:
      type: http
    decoder:
      type: html
    transform:
      type: xpath
    metrics:
      - name: health_workers
        expression: "number(substring-before(substring-after(/html/body, 'OK '), ' '))"
      - name: health_ok
        expression: "starts-with(/html/body, 'OK ')"

  - name: health_xpath_auto
    request:
      type: http
    decoder:
      type: auto
    transform:
      type: xpath
    metrics:
      - name: health_ok
        expression: "starts-with(/html/body, 'OK ')"
`

// testdata/html/text-only.html stands for a health endpoint that answers a
// line of text. Read as HTML the line is the text of a body: css maps it,
// xpath computes the number in it. A target that calls it text/plain is read
// as text by a collector that leaves the decoder to each answer, and XPath
// cannot read text: the probe fails saying so.
func TestTheTextOnlyFixtureIsTheTextOfABody(t *testing.T) {
	readHTMLFixture(t, "text-only.html", "text/html", textOnlyCollectors, []htmlReading{
		{
			collector: "health_css",
			series: []string{
				`health_ok 1`,
			},
			logs: []string{
				`ERROR metric extraction failed metric=health_workers failures=1 error=metric "health_workers": value "OK 17 workers ready" is not a number; map text to numbers with value_map`,
			},
		},
		{
			collector: "health_xpath",
			series: []string{
				`health_workers 17`,
				`health_ok 1`,
			},
		},
		{
			collector: "health_xpath_auto",
			series: []string{
				`health_ok 1`,
			},
		},
	})
	// Called text/plain, it is HTML only for the collectors that say so.
	readHTMLFixture(t, "text-only.html", "text/plain", textOnlyCollectors, []htmlReading{
		{
			collector: "health_css",
			series: []string{
				`health_ok 1`,
			},
			logs: []string{
				`ERROR metric extraction failed metric=health_workers failures=1 error=metric "health_workers": value "OK 17 workers ready" is not a number; map text to numbers with value_map`,
			},
		},
		{
			collector: "health_xpath",
			series: []string{
				`health_workers 17`,
				`health_ok 1`,
			},
		},
		{
			collector:    "health_xpath_auto",
			failure:      `xpath transform requires an XML or HTML response, got "text"`,
			probeFailure: `collector health_xpath_auto transform failed: xpath transform requires an XML or HTML response, got "text"`,
			probeLogs: []string{
				`ERROR probe failed stage=transform error=xpath transform requires an XML or HTML response, got "text"`,
			},
		},
	})
}

const entitiesCollectors = `collectors:
  - name: sensors_css
    request:
      type: http
    transform:
      type: css
    metrics:
      - name: sensor_reading
        items: '#readings tbody tr'
        expression: td.v
        labels:
          - name: sensor
            expression: td.name
      - name: sensors_online
        expression: '#online'

  - name: sensors_xpath
    request:
      type: http
    decoder:
      type: html
    transform:
      type: xpath
    metrics:
      - name: sensor_reading
        expression: "//table[@id='readings']/tbody/tr/td[@class='v']"
        labels:
          - name: row
            expression: ../@id
      - name: sensors_online
        expression: "//span[@id='online']"
      # A computed label: normalize-space makes one line of a wrapped name.
      - name: sensor_names
        expression: "count(//table[@id='readings']/tbody/tr)"
        labels:
          - name: wrapped
            expression: "normalize-space(//tr[@id='wrapped']/td[@class='name'])"
          - name: title
            expression: //title
      # What a row writes for people can be computed into its number, one
      # rule a value: the minus sign U+2212, digits grouped by thin spaces,
      # and the text before a br.
      - name: sensor_cold_store_celsius
        expression: "number(translate(//tr[@id='minus-literal']/td[@class='v'], '−', '-'))"
      - name: sensor_total_volume_litres
        expression: "number(translate(//tr[@id='thin-space']/td[@class='v'], ' ', ''))"
      - name: sensor_inlet_pressure_bar
        expression: "number(//tr[@id='br']/td[@class='v']/text()[1])"
`

// testdata/html/entities-whitespace.html stands for a page of readings whose
// cells hold what real pages hold: entities by name and by number, no-break
// spaces, a typographic minus, digits grouped by thin spaces, a br, a value
// split across inline elements, comments, names wrapped over lines.
//
// Entities are their characters. The text of a cell is that of everything
// beneath it, comments left out, trimmed of the blanks around it, no-break
// spaces among them. What is left must be a number as written: a value split
// across elements and one with a comment in it are; one with a minus sign that
// is not the hyphen, with blanks between its digits, with its unit after a br
// or with a zero-width space is not, and fails its row; a cell of blanks or of
// a comment alone is its row's missing value. A label keeps the blanks inside
// its text, line breaks and tabs too; XPath's normalize-space makes one line
// of it, and its translate computes a number out of a typographic one.
func TestTheEntitiesAndWhitespaceFixtureIsReadByCSSAndXPath(t *testing.T) {
	readHTMLFixture(t, "entities-whitespace.html", "text/html; charset=utf-8", entitiesCollectors, []htmlReading{
		{
			collector: "sensors_css",
			series: []string{
				`sensor_reading{sensor="Tank A level"} 71.5`,
				`sensor_reading{sensor="Tank B level"} 64.25`,
				"sensor_reading{sensor=\"Pump\u00a01 & 2\"} 12",
				"sensor_reading{sensor=\"Pump\u00a03\"} 13",
				`sensor_reading{sensor="Réservoir №4 <north>"} 42`,
				`sensor_reading{sensor="Freezer temp"} -21.5`,
				`sensor_reading{sensor="Pressure(outlet)"} 3.9`,
				`sensor_reading{sensor="Flow rate"} 12.5`,
				`sensor_reading{sensor="Valve 7"} 3.75`,
				`sensor_reading{sensor="Filter load"} 55`,
				`sensor_reading{sensor="Header tank\n        level (north\n        wing)"} 2.5`,
				"sensor_reading{sensor=\"Sump\tlevel\"} 0.8",
				`sensor_reading{sensor="Tank \"C\" / line's end \\ back"} 9`,
				`sensor_reading{sensor="Drift"} 0.5`,
				`sensor_reading{sensor="Leak rate"} 0.0015`,
				`sensors_online 21`,
			},
			logs: []string{
				`ERROR metric extraction failed metric=sensor_reading failures=9 error=metric "sensor_reading" item 5: value "−7.5" is not a number; map text to numbers with value_map`,
			},
		},
		{
			collector: "sensors_xpath",
			series: []string{
				`sensor_reading{row="plain"} 71.5`,
				`sensor_reading{row="indented"} 64.25`,
				`sensor_reading{row="nbsp-entity"} 12`,
				`sensor_reading{row="nbsp-literal"} 13`,
				`sensor_reading{row="numeric-entities"} 42`,
				`sensor_reading{row="hyphen-minus"} -21.5`,
				`sensor_reading{row="br-only"} 3.9`,
				`sensor_reading{row="split"} 12.5`,
				`sensor_reading{row="split-deep"} 3.75`,
				`sensor_reading{row="comment"} 55`,
				`sensor_reading{row="wrapped"} 2.5`,
				`sensor_reading{row="tabs"} 0.8`,
				`sensor_reading{row="quote"} 9`,
				`sensor_reading{row="plus"} 0.5`,
				`sensor_reading{row="exponent"} 0.0015`,
				`sensors_online 21`,
				`sensor_names{title="Tank farm & pump room — readings",wrapped="Header tank level (north wing)"} 24`,
				`sensor_cold_store_celsius -18`,
				`sensor_total_volume_litres 1.234567e+06`,
				`sensor_inlet_pressure_bar 4.2`,
			},
			logs: []string{
				`ERROR metric extraction failed metric=sensor_reading failures=9 error=value "−7.5" is not a number; map text to numbers with value_map`,
			},
		},
	})
}

const notContentCollectors = `collectors:
  - name: farm_css
    request:
      type: http
    transform:
      type: css
    metrics:
      - name: farm_agents
        expression: '#agents'
      # A hidden row, one a style hides and one hidden from assistive
      # technology are rows of the document like any other, and so is the
      # text of a hidden element inside a cell: bsd-02 shows 9 and reads 9000.
      - name: farm_agent_jobs
        items: '#agents-table tr.agent:not(:has(script)):not(:has(style))'
        expression: td.n
        labels:
          - name: agent
            expression: td.name
      # Attribute selectors leave out what the page hides.
      - name: farm_agent_jobs_shown
        items: '#agents-table tr.agent:not([hidden]):not([style*="display:none"]):not([aria-hidden="true"]):not(:has(script)):not(:has(style))'
        expression: td.n
        labels:
          - name: agent
            expression: td.name
      # The markup inside noscript and iframe is their text, and what is in
      # a comment is nowhere: none of the three has elements to select.
      - name: farm_noscript_fallback
        expression: '#fallback'
        required: false
      - name: farm_in_frame
        expression: '#in-frame'
        required: false
      - name: farm_commented_out
        expression: '#commented'
        required: false
      # An object's fallback is elements.
      - name: farm_in_object
        expression: '#in-object'
      # A pre and a textarea keep their text as written, which is trimmed.
      - name: farm_queue_depth
        expression: '#depth'
      - name: farm_notes
        expression: '#notes'
      - name: farm_mode
        expression: '#mode option[selected]'
        value_map: {auto: 1, manual: 2}
      # The text of a progress and of a meter is their fallback; the value
      # itself is an attribute, which a selector does not read.
      - name: farm_progress_percent
        expression: '#progress'
      - name: farm_form_token
        expression: '#csrf'
        required: false
      # Text that was escaped is text, tags and all.
      - name: farm_escaped
        expression: '#escaped'
      # Elements of an svg and of math are selected like any other.
      - name: farm_agent_cpu_percent
        expression: 'svg text[data-agent="linux-01"]'
        labels:
          - name: agent
            value: linux-01
      - name: farm_ratio_numerator
        expression: math mn.num

  - name: farm_xpath
    request:
      type: http
    decoder:
      type: html
    transform:
      type: xpath
    metrics:
      - name: farm_agent_jobs
        expression: "//table[@id='agents-table']//tr[not(.//script) and not(.//style)]/td[@class='n']"
        labels:
          - name: agent
            expression: "../td[@class='name']"
          - name: hidden
            expression: boolean(../@hidden)
          - name: style
            expression: ../@style
          - name: aria_hidden
            expression: ../@aria-hidden
      # A value that is an attribute is selected as one.
      - name: farm_form_token
        expression: "//input[@id='csrf']/@value"
      - name: farm_progress_percent
        expression: "//progress/@value"
      - name: farm_disk_ratio
        expression: "//meter[@id='disk']/@value"
      - name: farm_mode
        expression: "//select[@id='mode']/option[@selected]/@value"
      # A script's text is the text of its element, for a rule that selects
      # it: the state the page was rendered from.
      - name: farm_queue_depth
        expression: "number(substring-before(substring-after(//script[@id='state'], '\"queue\": '), '}'))"
      - name: farm_noscript_elements
        expression: "count(//noscript//*)"
      - name: farm_iframe_elements
        expression: "count(//iframe//*)"
        labels:
          - name: src
            expression: //iframe/@src
      # A CDATA section is a comment to HTML, and its text is gone.
      - name: farm_cdata
        expression: "count(//p[@id='cdata']/comment())"
        labels:
          - name: text
            expression: "normalize-space(//p[@id='cdata'])"
      # Inside svg and math an attribute HTML gives a namespace is read by
      # its name as written, on the node and on its parent.
      - name: farm_agent_cpu_percent
        expression: "//svg//text[@class='reading']"
        labels:
          - name: agent
            expression: '@data-agent'
          - name: link
            expression: ../@xlink:href
          - name: title
            expression: ../@xlink:title
      - name: farm_math_variable
        expression: "//math/mi"
        value_map: {x: 1}
        labels:
          - name: link
            expression: '@xlink:href'
      - name: farm_ratio
        expression: "//math//mn[@class='num'] div //math//mn[@class='den']"
        labels:
          - name: display
            expression: //math/@display
`

// testdata/html/not-content.html stands for a page with everything a page
// holds besides its content: scripts and styles, a template, noscript, rows
// hidden three ways, a pre, a textarea, form controls, a CDATA section, an
// inline svg and math, an iframe, an object, a comment.
//
// Hidden elements are in the document and are read, and attribute selectors or
// predicates leave them out. The markup inside noscript and iframe is text and
// not elements, and what is commented out is not there. Values that are
// attributes are for xpath. The rows that hold a script or a style in a cell
// are left out here by the rules themselves.
func TestTheNotContentFixtureIsReadByCSSAndXPath(t *testing.T) {
	readHTMLFixture(t, "not-content.html", "text/html; charset=utf-8", notContentCollectors, []htmlReading{
		{
			collector: "farm_css",
			series: []string{
				`farm_agents 4`,
				`farm_agent_jobs{agent="linux-01"} 12`,
				`farm_agent_jobs{agent="linux-02"} 0`,
				`farm_agent_jobs{agent="mac-01"} 3`,
				`farm_agent_jobs{agent="mac-02"} 4`,
				`farm_agent_jobs{agent="win-01"} 5`,
				`farm_agent_jobs{agent="bsd-02 (retired)"} 9000`,
				`farm_agent_jobs_shown{agent="linux-01"} 12`,
				`farm_agent_jobs_shown{agent="mac-02"} 4`,
				`farm_agent_jobs_shown{agent="bsd-02 (retired)"} 9000`,
				`farm_in_object 66`,
				`farm_queue_depth 17`,
				`farm_notes 22`,
				`farm_mode 2`,
				`farm_progress_percent 70`,
				`farm_agent_cpu_percent{agent="linux-01"} 63`,
				`farm_ratio_numerator 3`,
			},
			logs: []string{
				`ERROR metric extraction failed metric=farm_escaped failures=1 error=metric "farm_escaped": value "<b>15</b>" is not a number; map text to numbers with value_map`,
			},
		},
		{
			collector: "farm_xpath",
			series: []string{
				`farm_agent_jobs{agent="linux-01",hidden="false"} 12`,
				`farm_agent_jobs{agent="linux-02",hidden="true"} 0`,
				`farm_agent_jobs{agent="mac-01",hidden="false",style="display:none"} 3`,
				`farm_agent_jobs{agent="mac-02",hidden="false",style="visibility: hidden;"} 4`,
				`farm_agent_jobs{agent="win-01",aria_hidden="true",hidden="false"} 5`,
				`farm_agent_jobs{agent="bsd-02 (retired)",hidden="false"} 9000`,
				`farm_form_token 31337`,
				`farm_progress_percent 70`,
				`farm_disk_ratio 0.6`,
				`farm_mode 2`,
				`farm_queue_depth 17`,
				`farm_noscript_elements 0`,
				`farm_iframe_elements{src="/embedded/status.html"} 0`,
				`farm_cdata{text="Raw: end"} 1`,
				`farm_agent_cpu_percent{agent="linux-01",link="/agents/linux-01",title="linux-01"} 63`,
				`farm_agent_cpu_percent{agent="win-02",link="/agents/win-02",title="win-02"} 18`,
				`farm_math_variable{link="#agents"} 1`,
				`farm_ratio{display="inline"} 0.75`,
			},
		},
	})
}

const attributeNameCollectors = `x-metrics: &metrics
  # Attributes of the parent, and of its parents, by their names as the
  # page writes them. A title that is empty and an attribute a host does
  # not have leave the label off.
  - name: host_load
    expression: "//ul[@id='hosts']/li/span[@class='load']"
    labels:
      - name: host
        expression: ../@data-id
        required: true
      - name: lang
        expression: ../@xml:lang
      - name: unit
        expression: ../@m:unit
      - name: title
        expression: ../@title
      - name: kind
        expression: ../../@og:type
      - name: region
        expression: ../../../@data-region
      - name: page_lang
        expression: ../../../../../@xml:lang
  # The names only HTML has: a colon first, an @, a digit first, a dot.
  - name: host_route
    expression: "//ul[@id='hosts']/li[@data-id != 'h3']/span[@class='load']"
    value_map: {"*": 1}
    labels:
      - name: host
        expression: ../@data-id
      - name: href
        expression: ../@:href
      - name: click
        expression: ../@@click
      - name: prevented
        expression: ../@x-on:click.prevent
      - name: density
        expression: ../@2x
      - name: list_click
        expression: ../../@v-on:click
  # The same names on the node the rule selects, in lower case however
  # the page wrote them: DATA-ROLE is data-role.
  - name: host_info
    expression: "//ul[@id='hosts']/li"
    value_map: {"*": 1}
    labels:
      - name: host
        expression: '@data-id'
      - name: lang
        expression: '@xml:lang'
      - name: href
        expression: '@:href'
      - name: click
        expression: '@@click'
      - name: prevented
        expression: '@x-on:click.prevent'
      - name: density
        expression: '@2x'
      - name: role
        expression: '@data-role'
      - name: upper_case_role
        expression: '@DATA-ROLE'
  # And from the text nodes a rule selects, whose parents are elements.
  - name: pool_connections
    expression: "//table[@id='pools']//td/text()"
    required: false
    labels:
      - name: pool
        expression: ../../@data-pool
      - name: unit
        expression: ../@data-unit
      - name: click
        expression: ../@v-on:click
      - name: href
        expression: ../../@:href
      - name: density
        expression: ../../@2x
      - name: kind
        expression: ../../../../@og:type
  # Deeper in an expression such a name is asked for by name().
  - name: queue_depth
    expression: "//meta[@name='queue-depth']/@content"
  - name: page_properties
    expression: "count(//meta[@property])"
    labels:
      - name: type
        expression: "//meta[@property='og:type']/@content"
      - name: hosts_kind
        expression: "//ul/@*[name()='og:type']"

collectors:
  - name: checkout_html
    request:
      type: http
    decoder:
      type: html
    transform:
      type: xpath
    metrics: *metrics

  # The decoder left to each answer: every label loads, since an HTML
  # answer reads it.
  - name: checkout_auto
    request:
      type: http
    transform:
      type: xpath
    metrics: *metrics
`

// testdata/html/attribute-names.html stands for a page a JavaScript framework
// renders, whose attributes have names XPath cannot say: og:type, v-on:click,
// x-on:click.prevent, :href, @click, 2x. Over HTML a label that is one
// attribute's name reads it as written, on the node, on its parents, and from
// a text node; with the decoder set to html and left to each answer alike.
//
// testdata/html/attribute-names.xhtml is the same page as XHTML. Its
// Content-Type application/xhtml+xml names no decoder, and its doctype makes
// it HTML. Called application/xml it is XML to the collector that leaves the
// decoder to each answer: the labels both kinds of document can read are read,
// the prefixes being the document's own, and a rule with a label only HTML can
// read fails, naming the label, rather than lose it.
func TestTheAttributeNamesFixtureIsReadAsHTMLAndAsXML(t *testing.T) {
	readHTMLFixture(t, "attribute-names.html", "text/html; charset=utf-8", attributeNameCollectors, []htmlReading{
		{
			collector: "checkout_html",
			series: []string{
				`host_load{host="h1",kind="list",lang="bg",page_lang="en-GB",region="eu-west-1",title="padded title"} 0.42`,
				`host_load{host="h2",kind="list",lang="de",page_lang="en-GB",region="eu-west-1"} 1.75`,
				`host_load{host="h3",kind="list",page_lang="en-GB",region="eu-west-1"} 0.08`,
				`host_route{click="open(h1)",density="hi-dpi",host="h1",href="'/hosts/' + h1",list_click="select",prevented="toggle"} 1`,
				`host_route{click="open(h2)",density="lo-dpi",host="h2",href="'/hosts/' + h2",list_click="select",prevented="toggle"} 1`,
				`host_info{click="open(h1)",density="hi-dpi",host="h1",href="'/hosts/' + h1",lang="bg",prevented="toggle"} 1`,
				`host_info{click="open(h2)",density="lo-dpi",host="h2",href="'/hosts/' + h2",lang="de",prevented="toggle"} 1`,
				`host_info{host="h3",role="primary"} 1`,
				`pool_connections{click="pick",density="a",href="/pools/checkout",kind="table",pool="checkout",unit="conns"} 25`,
				`pool_connections{click="pick",density="b",href="/pools/payment",kind="table",pool="payment",unit="conns"} 40`,
				`queue_depth 17`,
				`page_properties{hosts_kind="list",type="website"} 2`,
			},
		},
		{
			collector: "checkout_auto",
			series: []string{
				`host_load{host="h1",kind="list",lang="bg",page_lang="en-GB",region="eu-west-1",title="padded title"} 0.42`,
				`host_load{host="h2",kind="list",lang="de",page_lang="en-GB",region="eu-west-1"} 1.75`,
				`host_load{host="h3",kind="list",page_lang="en-GB",region="eu-west-1"} 0.08`,
				`host_route{click="open(h1)",density="hi-dpi",host="h1",href="'/hosts/' + h1",list_click="select",prevented="toggle"} 1`,
				`host_route{click="open(h2)",density="lo-dpi",host="h2",href="'/hosts/' + h2",list_click="select",prevented="toggle"} 1`,
				`host_info{click="open(h1)",density="hi-dpi",host="h1",href="'/hosts/' + h1",lang="bg",prevented="toggle"} 1`,
				`host_info{click="open(h2)",density="lo-dpi",host="h2",href="'/hosts/' + h2",lang="de",prevented="toggle"} 1`,
				`host_info{host="h3",role="primary"} 1`,
				`pool_connections{click="pick",density="a",href="/pools/checkout",kind="table",pool="checkout",unit="conns"} 25`,
				`pool_connections{click="pick",density="b",href="/pools/payment",kind="table",pool="payment",unit="conns"} 40`,
				`queue_depth 17`,
				`page_properties{hosts_kind="list",type="website"} 2`,
			},
		},
	})
	// The XHTML page, which has no pools and no framework attributes.
	readHTMLFixture(t, "attribute-names.xhtml", "application/xhtml+xml", attributeNameCollectors, []htmlReading{
		{
			collector: "checkout_html",
			series: []string{
				`host_load{host="h1",kind="list",lang="bg",page_lang="en-GB",region="eu-west-1",unit="load1"} 0.42`,
				`host_load{host="h2",kind="list",lang="de",page_lang="en-GB",region="eu-west-1",unit="load1"} 1.75`,
				`host_load{host="h3",kind="list",page_lang="en-GB",region="eu-west-1"} 0.08`,
				`host_route{host="h1"} 1`,
				`host_route{host="h2"} 1`,
				`host_info{host="h1",lang="bg"} 1`,
				`host_info{host="h2",lang="de"} 1`,
				`host_info{host="h3"} 1`,
				`queue_depth 17`,
				`page_properties{hosts_kind="list"} 0`,
			},
		},
		{
			collector: "checkout_auto",
			series: []string{
				`host_load{host="h1",kind="list",lang="bg",page_lang="en-GB",region="eu-west-1",unit="load1"} 0.42`,
				`host_load{host="h2",kind="list",lang="de",page_lang="en-GB",region="eu-west-1",unit="load1"} 1.75`,
				`host_load{host="h3",kind="list",page_lang="en-GB",region="eu-west-1"} 0.08`,
				`host_route{host="h1"} 1`,
				`host_route{host="h2"} 1`,
				`host_info{host="h1",lang="bg"} 1`,
				`host_info{host="h2",lang="de"} 1`,
				`host_info{host="h3"} 1`,
				`queue_depth 17`,
				`page_properties{hosts_kind="list"} 0`,
			},
		},
	})
	// Called XML, it is XML where the decoder was left to the answer.
	readHTMLFixture(t, "attribute-names.xhtml", "application/xml", attributeNameCollectors, []htmlReading{
		{
			collector: "checkout_html",
			series: []string{
				`host_load{host="h1",kind="list",lang="bg",page_lang="en-GB",region="eu-west-1",unit="load1"} 0.42`,
				`host_load{host="h2",kind="list",lang="de",page_lang="en-GB",region="eu-west-1",unit="load1"} 1.75`,
				`host_load{host="h3",kind="list",page_lang="en-GB",region="eu-west-1"} 0.08`,
				`host_route{host="h1"} 1`,
				`host_route{host="h2"} 1`,
				`host_info{host="h1",lang="bg"} 1`,
				`host_info{host="h2",lang="de"} 1`,
				`host_info{host="h3"} 1`,
				`queue_depth 17`,
				`page_properties{hosts_kind="list"} 0`,
			},
		},
		{
			collector: "checkout_auto",
			series: []string{
				`host_load{host="h1",kind="list",lang="bg",page_lang="en-GB",region="eu-west-1",unit="load1"} 0.42`,
				`host_load{host="h2",kind="list",lang="de",page_lang="en-GB",region="eu-west-1",unit="load1"} 1.75`,
				`host_load{host="h3",kind="list",page_lang="en-GB",region="eu-west-1"} 0.08`,
				`queue_depth 17`,
				`page_properties{hosts_kind="list"} 0`,
			},
			logs: []string{
				`ERROR metric extraction failed metric=host_route failures=1 error=metric "host_route" label "href": XPath "../@:href" cannot be read in the XML document that arrived (../@:href has an invalid token.); set decoder.type to the kind of document the target answers with, or write the label so that it can be read in both`,
				`ERROR metric extraction failed metric=host_info failures=1 error=metric "host_info" label "href": XPath "@:href" cannot be read in the XML document that arrived (@:href has an invalid token.); set decoder.type to the kind of document the target answers with, or write the label so that it can be read in both`,
			},
		},
	})
}

const valueFormCollectors = `collectors:
  - name: forms_css
    request:
      type: http
    transform:
      type: css
    metrics:
      # A required rule: the numbers are read, text that is no number
      # fails its row, and a cell that is empty or absent is its row's
      # missing value.
      - name: form_value
        items: '#forms tbody tr'
        expression: td.v
        labels:
          - name: form
            expression: td.form
      # An optional rule is not missing what is empty or absent, and still
      # fails on text that is no number.
      - name: form_value_optional
        items: '#forms tbody tr'
        expression: td.v
        required: false
        labels:
          - name: form
            expression: td.form
      # value_map names the texts that stand for a value, and scale
      # multiplies what was mapped and what was read alike.
      - name: form_value_mapped
        items: '#forms tbody tr'
        expression: td.v
        required: false
        value_map: {n/a: -1, "—": -1, "85 %": 85, "1,234": 1234}
        scale: 100
        labels:
          - name: form
            expression: td.form
      # States are text: mapped as written, case and all.
      - name: form_state
        items: '#forms tbody tr'
        expression: td.s
        value_map: {up: 1, down: 0, OK: 1, FAIL: 0, "✓": 1, "✗": 0}
        labels:
          - name: form
            expression: td.form
      # "*" maps every other text, so nothing fails, and a label's own
      # value_map names the states, leaving one off by mapping it to "".
      - name: form_state_known
        items: '#forms tbody tr'
        expression: td.s
        required: false
        value_map: {up: 1, OK: 1, "✓": 1, "*": 0}
        labels:
          - name: form
            expression: td.form
          - name: state
            expression: td.s
            value_map: {up: good, OK: good, "✓": good, n/a: "", "*": bad}
      - name: job_duration_seconds
        items: '#jobs tbody tr'
        expression: td.ms
        scale: 0.001
        labels:
          - name: job
            expression: td.job
      # Times: a layout read in a zone, a job without a next run left out.
      - name: job_next_run_timestamp_seconds
        items: '#jobs tbody tr'
        expression: td.next
        time_format: "2006-01-02 15:04:05"
        time_zone: Europe/Sofia
        required: false
        labels:
          - name: job
            expression: td.job
      # The words a page writes for a time are a time only where they are
      # in the format.
      - name: job_last_run_timestamp_seconds
        items: '#jobs tbody tr'
        expression: td.last time
        time_format: "2 Jan 2006, 15:04"
        error_mode: ignore
        labels:
          - name: job
            expression: td.job
      - name: page_generated_timestamp_seconds
        expression: '#generated'
        time_format: rfc1123
      - name: certificate_expiry_timestamp_seconds
        expression: '#cert'
        time_format: "Jan 2, 2006 3:04 PM"
      - name: deployed_timestamp_milliseconds
        expression: '#deploy'
        time_format: "02.01.2006 15:04:05"
        time_zone: Europe/Sofia
        scale: 1000

  # error_mode: fail makes the first row that cannot be read the scrape's
  # failure.
  - name: forms_css_strict
    request:
      type: http
    transform:
      type: css
    metrics:
      - name: form_value
        items: '#forms tbody tr'
        expression: td.v
        error_mode: fail
        labels:
          - name: form
            expression: td.form

  - name: forms_xpath
    request:
      type: http
    decoder:
      type: html
    transform:
      type: xpath
    metrics:
      - name: form_value
        expression: "//table[@id='forms']/tbody/tr/td[@class='v']"
        labels:
          - name: form
            expression: ../@id
      - name: form_value_optional
        expression: "//table[@id='forms']/tbody/tr/td[@class='v']"
        required: false
        error_mode: ignore
        labels:
          - name: form
            expression: ../@id
      - name: form_state
        expression: "//table[@id='forms']/tbody/tr/td[@class='s']"
        value_map: {up: 1, down: 0, OK: 1, FAIL: 0, "✓": 1, "✗": 0, "*": -1}
        required: false
        labels:
          - name: form
            expression: ../@id
          - name: state
            expression: .
            value_map: {"✓": check, "✗": cross, "—": ""}
      # A predicate reads only the rows whose state the rule knows.
      - name: form_up
        expression: "//table[@id='forms']/tbody/tr[td[@class='s'] = 'up' or td[@class='s'] = 'OK']/td[@class='v']"
        labels:
          - name: form
            expression: ../@id
      - name: job_duration_seconds
        expression: "//table[@id='jobs']/tbody/tr/td[@class='ms']"
        scale: 0.001
        labels:
          - name: job
            expression: "../td[@class='job']"
      - name: job_next_run_timestamp_seconds
        expression: "//table[@id='jobs']/tbody/tr/td[@class='next']"
        time_format: "2006-01-02 15:04:05"
        time_zone: Europe/Sofia
        required: false
        labels:
          - name: job
            expression: "../td[@class='job']"
      # The machine's time is the datetime attribute: RFC 3339, its own
      # zone in it, a fraction of a second kept.
      - name: job_last_run_timestamp_seconds
        expression: "//table[@id='jobs']/tbody/tr[td[@class='job'] = 'backup']//time/@datetime"
        time_format: rfc3339
        labels:
          - name: job
            value: backup
      - name: job_last_run_timestamp_seconds
        expression: "//table[@id='jobs']/tbody/tr[td[@class='job'] = 'rotate-logs']//time/@datetime"
        time_format: rfc3339
        labels:
          - name: job
            value: rotate-logs
      - name: job_last_run_timestamp_seconds
        expression: "//table[@id='jobs']/tbody/tr[td[@class='job'] = 'reindex']//time/@datetime"
        time_format: rfc3339
        labels:
          - name: job
            value: reindex
      # A time element without the attribute has only its words.
      - name: job_last_run_timestamp_seconds
        expression: "//table[@id='jobs']/tbody/tr[td[@class='job'] = 'prune']//time"
        time_format: rfc3339
        labels:
          - name: job
            value: prune
      - name: page_generated_timestamp_seconds
        expression: "string(//time[@id='generated']/@datetime)"
        time_format: rfc3339
`

// testdata/html/value-forms.html stands for a table of the forms a value takes
// on real pages — integers, decimals, a thousands separator, percent signs, a
// unit, a dash, n/a, an empty cell, a row without the cell, states as words
// and as check marks — and a table of jobs with their times.
//
// A number is read; text that is no number fails its row, whether the rule is
// required or not; an empty or absent cell is a missing value, which only a
// required rule fails on; value_map maps text as written, "*" every other
// text; scale multiplies; a label's value_map names or drops a value;
// error_mode: fail fails the scrape at the first such row. With xpath a row
// without the cell is not selected, so nothing is missing it. time_format
// reads a layout in a zone, RFC 1123, and the datetime attribute as RFC 3339.
func TestTheValueFormsFixtureIsReadByCSSAndXPath(t *testing.T) {
	readHTMLFixture(t, "value-forms.html", "text/html; charset=utf-8", valueFormCollectors, []htmlReading{
		{
			collector: "forms_css",
			series: []string{
				`form_value{form="integer"} 42`,
				`form_value{form="decimal"} 3.14`,
				`form_value{form="negative"} -17`,
				`form_value{form="leading dot"} 0.5`,
				`form_value{form="zero"} 0`,
				`form_value_optional{form="integer"} 42`,
				`form_value_optional{form="decimal"} 3.14`,
				`form_value_optional{form="negative"} -17`,
				`form_value_optional{form="leading dot"} 0.5`,
				`form_value_optional{form="zero"} 0`,
				`form_value_mapped{form="integer"} 4200`,
				`form_value_mapped{form="decimal"} 314`,
				`form_value_mapped{form="negative"} -1700`,
				`form_value_mapped{form="leading dot"} 50`,
				`form_value_mapped{form="thousands separator"} 123400`,
				`form_value_mapped{form="percent"} 8500`,
				`form_value_mapped{form="em dash"} -100`,
				`form_value_mapped{form="n/a"} -100`,
				`form_value_mapped{form="zero"} 0`,
				`form_state{form="integer"} 1`,
				`form_state{form="decimal"} 0`,
				`form_state{form="negative"} 1`,
				`form_state{form="leading dot"} 0`,
				`form_state{form="thousands separator"} 1`,
				`form_state{form="percent"} 1`,
				`form_state{form="percent, no blank"} 0`,
				`form_state{form="hexadecimal"} 1`,
				`form_state_known{form="integer",state="good"} 1`,
				`form_state_known{form="decimal",state="bad"} 0`,
				`form_state_known{form="negative",state="good"} 1`,
				`form_state_known{form="leading dot",state="bad"} 0`,
				`form_state_known{form="thousands separator",state="good"} 1`,
				`form_state_known{form="percent",state="good"} 1`,
				`form_state_known{form="percent, no blank",state="bad"} 0`,
				`form_state_known{form="unit",state="bad"} 0`,
				`form_state_known{form="em dash",state="bad"} 0`,
				`form_state_known{form="n/a"} 0`,
				`form_state_known{form="zero",state="bad"} 0`,
				`form_state_known{form="hexadecimal",state="good"} 1`,
				`job_duration_seconds{job="backup"} 0.412`,
				`job_duration_seconds{job="rotate-logs"} 0.038`,
				`job_duration_seconds{job="reindex"} 90.5`,
				`job_duration_seconds{job="prune"} 0`,
				`job_next_run_timestamp_seconds{job="backup"} 1.7910684e+09`,
				`job_next_run_timestamp_seconds{job="rotate-logs"} 1.7910594e+09`,
				`job_next_run_timestamp_seconds{job="reindex"} 1.7928918e+09`,
				`job_last_run_timestamp_seconds{job="backup"} 1.7909928e+09`,
				`page_generated_timestamp_seconds 1.791018e+09`,
				`certificate_expiry_timestamp_seconds 1.80005754e+09`,
				`deployed_timestamp_milliseconds 1.791004542e+12`,
			},
			logs: []string{
				`ERROR metric extraction failed metric=form_value failures=9 error=metric "form_value" item 4: value "1,234" is not a number; map text to numbers with value_map`,
				`ERROR metric extraction failed metric=form_value_optional failures=7 error=metric "form_value_optional" item 4: value "1,234" is not a number; map text to numbers with value_map`,
				`ERROR metric extraction failed metric=form_value_mapped failures=3 error=metric "form_value_mapped" item 6: value "85%" is neither in value_map nor a number; add it to value_map, or map "*" for any other value`,
				`ERROR metric extraction failed metric=form_state failures=6 error=metric "form_state" item 7: value "Up" is neither in value_map nor a number; add it to value_map, or map "*" for any other value`,
			},
		},
		{
			collector: "forms_css_strict",
			logs: []string{
				`ERROR metric extraction failed metric=form_value failures=1 error=metric "form_value" item 4: value "1,234" is not a number; map text to numbers with value_map`,
			},
			failure:      `metric "form_value" item 4: value "1,234" is not a number; map text to numbers with value_map`,
			probeFailure: `stage=metric metric=form_value error=metric "form_value" item 4: value "1,234" is not a number; map text to numbers with value_map`,
			probeLogs: []string{
				`ERROR probe failed metric=form_value stage=metric error=metric "form_value" item 4: value "1,234" is not a number; map text to numbers with value_map`,
			},
		},
		{
			collector: "forms_xpath",
			series: []string{
				`form_value{form="integer"} 42`,
				`form_value{form="decimal"} 3.14`,
				`form_value{form="negative"} -17`,
				`form_value{form="leading-dot"} 0.5`,
				`form_value{form="zero"} 0`,
				`form_value_optional{form="integer"} 42`,
				`form_value_optional{form="decimal"} 3.14`,
				`form_value_optional{form="negative"} -17`,
				`form_value_optional{form="leading-dot"} 0.5`,
				`form_value_optional{form="zero"} 0`,
				`form_state{form="integer",state="up"} 1`,
				`form_state{form="decimal",state="down"} 0`,
				`form_state{form="negative",state="OK"} 1`,
				`form_state{form="leading-dot",state="FAIL"} 0`,
				`form_state{form="thousands",state="check"} 1`,
				`form_state{form="percent",state="check"} 1`,
				`form_state{form="percent-tight",state="cross"} 0`,
				`form_state{form="unit",state="Up"} -1`,
				`form_state{form="dash"} -1`,
				`form_state{form="na",state="n/a"} -1`,
				`form_state{form="zero",state="unknown"} -1`,
				`form_state{form="hex",state="up"} 1`,
				`form_up{form="integer"} 42`,
				`form_up{form="negative"} -17`,
				`job_duration_seconds{job="backup"} 0.412`,
				`job_duration_seconds{job="rotate-logs"} 0.038`,
				`job_duration_seconds{job="reindex"} 90.5`,
				`job_duration_seconds{job="prune"} 0`,
				`job_next_run_timestamp_seconds{job="backup"} 1.7910684e+09`,
				`job_next_run_timestamp_seconds{job="rotate-logs"} 1.7910594e+09`,
				`job_next_run_timestamp_seconds{job="reindex"} 1.7928918e+09`,
				`job_last_run_timestamp_seconds{job="backup"} 1.790992807e+09`,
				`job_last_run_timestamp_seconds{job="rotate-logs"} 1.7909946e+09`,
				`job_last_run_timestamp_seconds{job="reindex"} 1.79097570025e+09`,
				`page_generated_timestamp_seconds 1.791018e+09`,
			},
			logs: []string{
				`ERROR metric extraction failed metric=form_value failures=8 error=value "1,234" is not a number; map text to numbers with value_map`,
				`ERROR metric extraction failed metric=form_up failures=1 error=value "0x1F" is not a number; map text to numbers with value_map`,
				`ERROR metric extraction failed metric=job_last_run_timestamp_seconds failures=1 error=value "never" is not a time in time_format "rfc3339", which reads times such as 2006-01-02T15:04:05Z and 2006-01-02T15:04:05.999+02:00`,
			},
		},
	})
}

const statusPageCollectors = `collectors:
  - name: acme_status
    metrics_prefix: acme
    request:
      type: http
      headers:
        Accept: text/html
    limits:
      max_label_value_length: 120
    transform:
      type: css
      # The page writes for people: 99.982%, 1,204,551, 87 ms, and a time
      # as "5 minutes ago" with the moment itself in an attribute, which a
      # selector does not read. The script is given the page as text and
      # leaves text: the separators and the units go, and each time element
      # gets its datetime for its text.
      pre_script: |
        import re
        data = re.sub(r'(?<=\d),(?=\d{3})', '', data)
        data = re.sub(r'(\d)\s*(?:%|ms)\s*<', r'\1<', data)
        data = re.sub(r'(<time\b[^>]*\bdatetime="([^"]*)"[^>]*>)[^<]*', r'\1\2', data)
      labels:
        page: acme-cloud
      remove_labels:
        - phase
      rename_labels:
        name: component
    metrics:
      - name: statuspage_degraded
        description: 1 while the page reports anything but all systems operational
        expression: '#summary .summary-text'
        value_map: {All Systems Operational: 0, "*": 1}
      - name: statuspage_updated_timestamp_seconds
        expression: '#summary time'
        time_format: rfc3339
      - name: statuspage_uptime_ratio
        expression: '#uptime-90d'
        scale: 0.01
      - name: statuspage_requests_today
        type: counter
        expression: '#requests-today'
      - name: statuspage_open_incidents
        expression: '#open-incidents'
      # One series a component that has a status, 1 with the status as a
      # label: the retired one, whose status cell is empty, has none.
      - name: statuspage_component_status
        description: Status of a component, 1 with the current status as a label
        items: '#components tr.component:has(td.status:not(:empty))'
        expression: td.status
        value_map: {"*": 1}
        labels:
          - name: name
            expression: td.name
            required: true
          - name: region
            expression: td.region
          - name: status
            expression: td.status
            value_map:
              Operational: operational
              Degraded Performance: degraded
              Major Outage: major_outage
              "*": other
      - name: statuspage_component_up
        items: '#components tr.component:not([hidden])'
        expression: td.status
        value_map: {Operational: 1, "*": 0}
        labels:
          - name: name
            expression: td.name
      # A component that is down has a dash for its response time, and the
      # retired one nothing: neither is worth a log line.
      - name: statuspage_component_response_seconds
        items: '#components tr.component'
        expression: td.latency
        scale: 0.001
        required: false
        error_mode: ignore
        labels:
          - name: name
            expression: td.name
      - name: statuspage_component_uptime_ratio
        items: '#components tr.component'
        expression: td.uptime
        scale: 0.01
        required: false
        labels:
          - name: name
            expression: td.name
      # The latest update is free text: cut to the limit, on a series of
      # its own. The phase is read and then removed by remove_labels.
      - name: statuspage_incident_info
        items: 'ol.incidents li.incident'
        expression: span.impact
        value_map: {"*": 1}
        labels:
          - name: incident
            expression: h3.title
          - name: impact
            expression: span.impact
          - name: phase
            expression: span.phase
          - name: message
            expression: p.update
            truncate: true
      - name: statuspage_incident_started_timestamp_seconds
        items: 'ol.incidents li.incident, ol.maintenances li.incident'
        expression: time.started
        time_format: rfc3339
        labels:
          - name: incident
            expression: h3.title
      # No maintenance is planned on most days, and no rule is missing one.
      - name: statuspage_maintenance_in_progress
        items: 'ol.maintenances li.incident:has(span.phase:contains("In progress"))'
        expression: span.impact
        value_map: {"*": 1}
        required: false
        labels:
          - name: incident
            expression: h3.title
      - name: statuspage_postmortems
        items: 'ol.postmortems li'
        expression: span.count
        required: false

  # The same page by XPath, which reads the attributes the page carries for
  # machines and needs no script: the ids, the status as a class, the time.
  - name: acme_status_xpath
    metrics_prefix: acme
    request:
      type: http
      headers:
        Accept: text/html
    decoder:
      type: html
    transform:
      type: xpath
      labels:
        page: acme-cloud
    metrics:
      - name: statuspage_indicator
        expression: "//div[@id='summary']/span[@class='summary-text']"
        value_map: {All Systems Operational: 0, "*": 1}
        labels:
          - name: indicator
            expression: ../@data-indicator
      - name: statuspage_updated_timestamp_seconds
        expression: "//div[@id='summary']//time/@datetime"
        time_format: rfc3339
      - name: statuspage_uptime_ratio
        expression: "number(substring-before(//span[@id='uptime-90d'], '%'))"
        scale: 0.01
      - name: statuspage_requests_today
        type: counter
        expression: "number(translate(//span[@id='requests-today'], ',', ''))"
      - name: statuspage_component_status
        expression: "//table[@id='components']/tbody/tr[@class='component']/td[contains(@class, 'status')]"
        value_map: {"*": 1}
        labels:
          - name: component_id
            expression: ../@data-component-id
            required: true
          - name: group
            expression: ../@data-group
          - name: component
            expression: "../td[@class='name']"
          - name: status
            expression: "substring-after(@class, 'status ')"
      - name: statuspage_components
        expression: "count(//table[@id='components']/tbody/tr[@data-component-id])"
      - name: statuspage_incident_info
        expression: "//ol[@class='incidents']/li/span[@class='impact']"
        value_map: {"*": 1}
        labels:
          - name: incident_id
            expression: ../@data-incident-id
          - name: impact
            expression: .
          - name: first_affected
            expression: "../ul[@class='affected']/li[1]"
          - name: affected
            expression: "count(../ul[@class='affected']/li)"
`

// testdata/html/status-page.html stands for a hosted status page, the page
// this exporter is pointed at most: a summary block, a table of components in
// groups, and lists of incidents and maintenances.
//
// One css collector reads it as an operator would write it: metrics_prefix, a
// pre_script that edits the page's text before it is parsed — thousands
// separators and units out of the numbers, each time element's datetime in
// place of its words — items with :has(), :not(), :empty and :contains(),
// labels from cells, a label's value_map, a required label, optional rules,
// time_format, scale, truncate under limits.max_label_value_length, and
// transform.labels, remove_labels and rename_labels over all of it. Nothing is
// logged: every rule finds its value or is allowed not to.
//
// An xpath collector reads the same page without a script, from the attributes
// the page carries for machines.
func TestTheStatusPageFixtureIsReadByCSSAndXPath(t *testing.T) {
	requirePython(t)
	readHTMLFixture(t, "status-page.html", "text/html; charset=utf-8", statusPageCollectors, []htmlReading{
		{
			collector: "acme_status",
			series: []string{
				`acme_statuspage_degraded{page="acme-cloud"} 1`,
				`acme_statuspage_updated_timestamp_seconds{page="acme-cloud"} 1.7910177e+09`,
				`acme_statuspage_uptime_ratio{page="acme-cloud"} 0.99982`,
				`acme_statuspage_requests_today{page="acme-cloud"} 1.204551e+06`,
				`acme_statuspage_open_incidents{page="acme-cloud"} 2`,
				`acme_statuspage_component_status{component="API",page="acme-cloud",region="eu-west-1",status="operational"} 1`,
				`acme_statuspage_component_status{component="Authentication",page="acme-cloud",region="eu-west-1",status="degraded"} 1`,
				`acme_statuspage_component_status{component="Database",page="acme-cloud",region="eu-central-1",status="operational"} 1`,
				`acme_statuspage_component_status{component="CDN",page="acme-cloud",region="global",status="major_outage"} 1`,
				`acme_statuspage_component_status{component="DNS",page="acme-cloud",region="global",status="other"} 1`,
				`acme_statuspage_component_up{component="API",page="acme-cloud"} 1`,
				`acme_statuspage_component_up{component="Authentication",page="acme-cloud"} 0`,
				`acme_statuspage_component_up{component="Database",page="acme-cloud"} 1`,
				`acme_statuspage_component_up{component="CDN",page="acme-cloud"} 0`,
				`acme_statuspage_component_up{component="DNS",page="acme-cloud"} 0`,
				`acme_statuspage_component_response_seconds{component="API",page="acme-cloud"} 0.087`,
				`acme_statuspage_component_response_seconds{component="Authentication",page="acme-cloud"} 1.42`,
				`acme_statuspage_component_response_seconds{component="Database",page="acme-cloud"} 0.012`,
				`acme_statuspage_component_response_seconds{component="DNS",page="acme-cloud"} 0.031`,
				`acme_statuspage_component_uptime_ratio{component="API",page="acme-cloud"} 0.9998`,
				`acme_statuspage_component_uptime_ratio{component="Authentication",page="acme-cloud"} 0.9961`,
				`acme_statuspage_component_uptime_ratio{component="Database",page="acme-cloud"} 1`,
				`acme_statuspage_component_uptime_ratio{component="CDN",page="acme-cloud"} 0.972`,
				`acme_statuspage_component_uptime_ratio{component="DNS",page="acme-cloud"} 0.9995`,
				`acme_statuspage_incident_info{impact="major",incident="CDN: elevated error rates in all regions",message="We have identified a faulty configuration push to the edge nodes as the cause of the elevated 5xx rates and are rolli…",page="acme-cloud"} 1`,
				`acme_statuspage_incident_info{impact="minor",incident="Slow sign-ins",message="A fix is deployed.",page="acme-cloud"} 1`,
				`acme_statuspage_incident_started_timestamp_seconds{incident="CDN: elevated error rates in all regions",page="acme-cloud"} 1.79101332e+09`,
				`acme_statuspage_incident_started_timestamp_seconds{incident="Slow sign-ins",page="acme-cloud"} 1.7910081e+09`,
				`acme_statuspage_incident_started_timestamp_seconds{incident="DNS resolver upgrade",page="acme-cloud"} 1.7910144e+09`,
				`acme_statuspage_maintenance_in_progress{incident="DNS resolver upgrade",page="acme-cloud"} 1`,
			},
		},
		{
			collector: "acme_status_xpath",
			series: []string{
				`acme_statuspage_indicator{indicator="minor",page="acme-cloud"} 1`,
				`acme_statuspage_updated_timestamp_seconds{page="acme-cloud"} 1.7910177e+09`,
				`acme_statuspage_uptime_ratio{page="acme-cloud"} 0.99982`,
				`acme_statuspage_requests_today{page="acme-cloud"} 1.204551e+06`,
				`acme_statuspage_component_status{component="API",component_id="cmp-api",group="core",page="acme-cloud",status="operational"} 1`,
				`acme_statuspage_component_status{component="Authentication",component_id="cmp-auth",group="core",page="acme-cloud",status="degraded"} 1`,
				`acme_statuspage_component_status{component="Database",component_id="cmp-db",group="core",page="acme-cloud",status="operational"} 1`,
				`acme_statuspage_component_status{component="CDN",component_id="cmp-cdn",group="edge",page="acme-cloud",status="outage"} 1`,
				`acme_statuspage_component_status{component="DNS",component_id="cmp-dns",group="edge",page="acme-cloud",status="maintenance"} 1`,
				`acme_statuspage_components{page="acme-cloud"} 6`,
				`acme_statuspage_incident_info{affected="1",first_affected="CDN",impact="major",incident_id="inc-2291",page="acme-cloud"} 1`,
				`acme_statuspage_incident_info{affected="2",first_affected="Authentication",impact="minor",incident_id="inc-2290",page="acme-cloud"} 1`,
			},
		},
	})
}
