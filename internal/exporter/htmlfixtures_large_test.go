//go:build !select_request_types || request_type_http

package exporter

import (
	"bytes"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// A page at size: a fleet's table of two thousand hosts, some 390 KB, as an
// inventory or a cluster's node list is. It is made here rather than kept
// under testdata, every row by one rule, so every series can be named.

const fleetHosts = 2000

// fleetHost is the i-th host: its name, its CPU and a note, which for two
// hosts in three is longer than a label of 40 bytes may be.
func fleetHost(i int) (host, cpu, note string) {
	host = fmt.Sprintf("node-%04d.dc%d.example.net", i, i%4+1)
	cpu = strconv.FormatFloat(float64(i*37%1000)/10, 'f', 1, 64)
	note = strings.TrimSpace(fmt.Sprintf("rack %d %s", i%40, strings.Repeat("é", i%3*30)))
	return host, cpu, note
}

func fleetPage() []byte {
	var page bytes.Buffer
	page.WriteString("<!DOCTYPE html>\n<html><head><title>Fleet</title></head><body>\n<table id=\"fleet\">\n<thead><tr><th>Host</th><th>CPU</th><th>Note</th></tr></thead>\n<tbody>\n")
	for i := range fleetHosts {
		host, cpu, note := fleetHost(i)
		parity := "even"
		if i%2 == 1 {
			parity = "odd"
		}
		fmt.Fprintf(&page, "<tr id=\"r%d\" class=\"%s\"><td class=\"host\">%s</td><td class=\"cpu\">%s</td><td class=\"note\">%s</td></tr>\n", i, parity, host, cpu, note)
	}
	page.WriteString("</tbody>\n</table>\n</body></html>\n")
	return page.Bytes()
}

// fleetSeries are the series of every host, with the label a collector
// gives each besides the host: none, or the note, cut to limit bytes when
// limit is not 0.
func fleetSeries(withNote bool, limit int) []string {
	series := make([]string, 0, fleetHosts)
	for i := range fleetHosts {
		host, cpu, note := fleetHost(i)
		value, _ := strconv.ParseFloat(cpu, 64)
		labels := `host="` + host + `"`
		if withNote {
			if limit > 0 && len(note) > limit {
				// Cut on a character boundary to make room for the "…",
				// three bytes, within the limit.
				cut := limit - len("…")
				for !utf8.RuneStart(note[cut]) {
					cut--
				}
				note = note[:cut] + "…"
			}
			labels += `,note="` + note + `"`
		}
		series = append(series, `fleet_cpu_percent{`+labels+`} `+strconv.FormatFloat(value, 'g', -1, 64))
	}
	return series
}

// sameManySeries is sameSeries for lists too long to print: it reports how
// many series there are and the first that differs.
func sameManySeries(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("got %d series, want %d", len(got), len(want))
	}
	for i := range min(len(got), len(want)) {
		if got[i] != want[i] {
			t.Fatalf("series %d is\n%s\nwant\n%s", i, got[i], want[i])
		}
	}
}

const fleetCollectors = `x-css: &css
  - name: fleet_cpu_percent
    items: '#fleet tbody tr'
    expression: td.cpu
    labels:
      - name: host
        expression: td.host
        required: true
x-xpath: &xpath
  - name: fleet_cpu_percent
    expression: "//table[@id='fleet']/tbody/tr/td[@class='cpu']"
    labels:
      - name: host
        expression: "../td[@class='host']"
        required: true
x-css-notes: &css_notes
  - name: fleet_cpu_percent
    items: '#fleet tbody tr'
    expression: td.cpu
    labels:
      - name: host
        expression: td.host
      - name: note
        expression: td.note
x-css-notes-cut: &css_notes_cut
  - name: fleet_cpu_percent
    items: '#fleet tbody tr'
    expression: td.cpu
    labels:
      - name: host
        expression: td.host
      - name: note
        expression: td.note
        truncate: true
x-xpath-notes-cut: &xpath_notes_cut
  - name: fleet_cpu_percent
    expression: "//table[@id='fleet']/tbody/tr/td[@class='cpu']"
    labels:
      - name: host
        expression: "../td[@class='host']"
      - name: note
        expression: "../td[@class='note']"
        truncate: true

collectors:
  - name: fleet_css
    request: {type: http}
    transform: {type: css}
    metrics: *css
  - name: fleet_xpath
    request: {type: http}
    decoder: {type: html}
    transform: {type: xpath}
    metrics: *xpath

  # limits.max_metrics: as many series as the page has rows, and one fewer.
  - name: fleet_css_exactly
    request: {type: http}
    limits: {max_metrics: 2000}
    transform: {type: css}
    metrics: *css
  - name: fleet_css_one_short
    request: {type: http}
    limits: {max_metrics: 1999}
    transform: {type: css}
    metrics: *css
  - name: fleet_xpath_one_short
    request: {type: http}
    decoder: {type: html}
    limits: {max_metrics: 1999}
    transform: {type: xpath}
    metrics: *xpath

  # The answer's size, bounded by the request and by the limits.
  - name: fleet_css_small_request
    request: {type: http, max_response_bytes: 100KiB}
    transform: {type: css}
    metrics: *css
  - name: fleet_xpath_small_limit
    request: {type: http}
    decoder: {type: html}
    limits: {max_response_bytes: 100KiB}
    transform: {type: xpath}
    metrics: *xpath

  # limits.max_label_value_length: notes of up to 128 bytes under a limit
  # of 40, refused, and cut by truncate.
  - name: fleet_css_notes
    request: {type: http}
    transform: {type: css}
    metrics: *css_notes
  - name: fleet_css_long_notes
    request: {type: http}
    limits: {max_label_value_length: 40}
    transform: {type: css}
    metrics: *css_notes
  - name: fleet_css_cut_notes
    request: {type: http}
    limits: {max_label_value_length: 40}
    transform: {type: css}
    metrics: *css_notes_cut
  - name: fleet_xpath_cut_notes
    request: {type: http}
    decoder: {type: html}
    limits: {max_label_value_length: 40}
    transform: {type: xpath}
    metrics: *xpath_notes_cut
`

// Both transforms make a series of every one of the two thousand rows, in
// the order of the page, with the limits as they are by default and with
// limits.max_metrics at exactly the number of rows. One series past
// max_metrics fails the scrape in validation, saying the count it stopped
// at. An answer larger than max_response_bytes, of the request or of the
// limits, is refused by the size the target says, before it is read. A
// label value over limits.max_label_value_length fails the scrape naming
// the metric and the label, and with truncate it is cut to the limit on a
// character boundary, the "…" that ends it included, in both transforms.
// Each failure is logged once, and nothing else is.
func TestALargePageIsReadWholeAndHeldToTheLimits(t *testing.T) {
	logs := testutil.CaptureLogs(t)
	page := fleetPage()
	if len(page) < 300<<10 || len(page) > 500<<10 {
		t.Fatalf("the page is %d bytes, want a few hundred KiB", len(page))
	}
	site := newHTMLSite(t, map[string]htmlPage{"/fleet": {"text/html; charset=utf-8", page}})
	server := htmlServer(htmlCollectors(t, fleetCollectors))
	tooLarge := fmt.Sprintf("response size %d exceeds limit 102400", len(page))

	for _, test := range []struct {
		collector string
		series    []string
		// stage and failure are those of a probe that fails.
		stage, failure string
	}{
		{collector: "fleet_css", series: fleetSeries(false, 0)},
		{collector: "fleet_xpath", series: fleetSeries(false, 0)},
		{collector: "fleet_css_exactly", series: fleetSeries(false, 0)},
		{collector: "fleet_css_one_short", stage: "validation", failure: "metric count 2000 exceeds limit 1999"},
		{collector: "fleet_xpath_one_short", stage: "validation", failure: "metric count 2000 exceeds limit 1999"},
		{collector: "fleet_css_small_request", stage: "http", failure: tooLarge},
		{collector: "fleet_xpath_small_limit", stage: "http", failure: tooLarge},
		{collector: "fleet_css_notes", series: fleetSeries(true, 0)},
		{collector: "fleet_css_long_notes", stage: "validation", failure: `metric "fleet_cpu_percent" label "note" value is 67 bytes, longer than limits.max_label_value_length 40; a label one of the collector's rules gives can be cut to fit with truncate: true on that label, or raise limits.max_label_value_length`},
		{collector: "fleet_css_cut_notes", series: fleetSeries(true, 40)},
		{collector: "fleet_xpath_cut_notes", series: fleetSeries(true, 40)},
	} {
		t.Run(test.collector, func(t *testing.T) {
			status, series, failure := probeHTML(t, server, site, test.collector, "/fleet")
			if test.failure != "" {
				if want := "collector " + test.collector + " " + test.stage + " failed: " + test.failure; status != http.StatusBadGateway || failure != want {
					t.Errorf("status=%d body=%s\nwant 502 %s", status, failure, want)
				}
				sameLogLines(t, logs, []string{"ERROR probe failed stage=" + test.stage + " error=" + test.failure})
				return
			}
			if status != http.StatusOK {
				t.Fatalf("status=%d body=%s", status, failure)
			}
			sameManySeries(t, series, test.series)
			sameLogLines(t, logs, nil)
		})
	}

	// The notes that were cut are within the limit, and whole characters.
	cut := 0
	for _, line := range fleetSeries(true, 40) {
		note := line[strings.Index(line, `note="`)+len(`note="`) : strings.LastIndex(line, `"`)]
		if len(note) > 40 || !utf8.ValidString(note) {
			t.Fatalf("the note %q is %d bytes", note, len(note))
		}
		if strings.HasSuffix(note, "…") {
			cut++
		}
	}
	if cut == 0 || cut == fleetHosts {
		t.Errorf("%d of the %d notes were cut, want some and not all", cut, fleetHosts)
	}
}
