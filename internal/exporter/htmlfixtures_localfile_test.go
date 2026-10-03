//go:build !select_request_types || request_type_localfile

package exporter

import (
	"log/slog"
	"net/http"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// The fixtures of testdata/html read from disk by the localfile request
// type, as a page a job writes or a device's saved report is: the same
// collectors' rules, with the file's extension and content in the place of
// a Content-Type, and response.charset or the page's own meta in the place
// of a declared encoding, since a file declares none.

const htmlFileCollectors = `collectors:
  # The file the collector names, read with selectors.
  - name: balancer_file
    request:
      type: localfile
      root: ROOT
      path: server-status.html
    transform:
      type: css
    metrics:
      - name: backend_sessions
        items: '#backends tbody tr'
        expression: td.scur
        labels:
          - name: server
            expression: td.name
      - name: sessions_total
        expression: '#backends tfoot td.scur'

  # The file the probe's target names, read with XPath: the decoder is left
  # to each file.
  - name: page_file
    request:
      type: localfile
      root: ROOT
    transform:
      type: xpath
    metrics:
      - name: page_titled
        expression: //title
        value_map: {"*": 1}
        labels:
          - name: title
            expression: .
      - name: host_load
        expression: "//ul[@id='hosts']/li/span[@class='load']"
        required: false
        labels:
          - name: host
            expression: ../@data-id
          - name: lang
            expression: ../@xml:lang
          - name: href
            expression: ../@:href

  - name: depots_file
    request:
      type: localfile
      root: ROOT
    transform:
      type: css
    metrics: &depots
      - name: depot_pallets
        items: '#depots tr:has(td)'
        expression: td.pallets
        labels:
          - name: depot
            expression: td.name
          - name: city
            expression: td.city
  - name: depots_file_windows_1251
    request:
      type: localfile
      root: ROOT
    response:
      charset: windows-1251
    transform:
      type: css
    metrics: *depots
`

// htmlFileServer is an exporter whose collectors read under testdata/html.
func htmlFileServer(t *testing.T) *Server {
	t.Helper()
	root, err := filepath.Abs("../../testdata/html")
	if err != nil {
		t.Fatal(err)
	}
	document := strings.ReplaceAll(htmlFileCollectors, "ROOT", root)
	cfg, err := config.Load(testutil.WriteFile(t, "config.yaml", document))
	if err != nil {
		t.Fatal(err)
	}
	return NewServer(config.NewManager(cfg, "", slog.Default()), "python3", slog.Default())
}

// htmlFileSeries probes a collector for a file, none when the collector
// names its own, and returns the sample lines of the answer.
func htmlFileSeries(t *testing.T, server *Server, collector, file string) []string {
	t.Helper()
	query := "collector=" + collector
	if file != "" {
		query += "&target=" + url.QueryEscape(file)
	}
	answer := probeFile(t, server, query)
	answer.must(t, http.StatusOK)
	var series []string
	for _, line := range strings.Split(strings.TrimSpace(answer.body), "\n") {
		if line != "" && !strings.HasPrefix(line, "#") {
			series = append(series, line)
		}
	}
	return series
}

func sameFileSeries(t *testing.T, got, want []string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// A page on disk is read as the same page served: the css collector reads
// the file it names, and the xpath collector, which leaves the decoder to
// each file, reads .html as HTML by its extension and .xhtml, an extension
// that names no format, as HTML by its doctype, with the labels HTML reads
// by name. Nothing is logged.
func TestHTMLFixturesAreReadFromLocalFiles(t *testing.T) {
	logs := testutil.CaptureLogs(t)
	server := htmlFileServer(t)

	sameFileSeries(t, htmlFileSeries(t, server, "balancer_file", ""), []string{
		`backend_sessions{server="web01"} 12`,
		`backend_sessions{server="web02"} 31`,
		`backend_sessions{server="web03"} 0`,
		`backend_sessions{server="api01"} 1`,
		`sessions_total 44`,
	})
	sameFileSeries(t, htmlFileSeries(t, server, "page_file", "sloppy.html"), []string{
		`page_titled{title="PDU-7 Outlet Status"} 1`,
	})
	sameFileSeries(t, htmlFileSeries(t, server, "page_file", "attribute-names.html"), []string{
		`page_titled{title="Checkout service"} 1`,
		`host_load{host="h1",href="'/hosts/' + h1",lang="bg"} 0.42`,
		`host_load{host="h2",href="'/hosts/' + h2",lang="de"} 1.75`,
		`host_load{host="h3"} 0.08`,
	})
	sameFileSeries(t, htmlFileSeries(t, server, "page_file", "attribute-names.xhtml"), []string{
		`page_titled{title="Checkout service"} 1`,
		`host_load{host="h1",lang="bg"} 0.42`,
		`host_load{host="h2",lang="de"} 1.75`,
		`host_load{host="h3"} 0.08`,
	})
	if logs.Len() != 0 {
		t.Errorf("reading the files logged:\n%s", logs)
	}
}

// A file declares no encoding. A page that says its own in a meta, or
// starts with a byte order mark, is read by it; one that says nothing is
// read by response.charset, and without that its legacy bytes are no UTF-8:
// the scrape answers with U+FFFD in their place and a warning that says to
// set response.charset.
func TestHTMLFilesAreReadInTheirOwnEncodingOrResponseCharset(t *testing.T) {
	logs := testutil.CaptureLogs(t)
	server := htmlFileServer(t)
	cyrillic := []string{
		`depot_pallets{city="София",depot="Склад „Изток“"} 120`,
		`depot_pallets{city="Санкт-Петербург",depot="Склад №2 «Север»"} 75`,
	}
	sameFileSeries(t, htmlFileSeries(t, server, "depots_file", "charset/depots-windows-1251-meta.html"), cyrillic)
	sameFileSeries(t, htmlFileSeries(t, server, "depots_file_windows_1251", "charset/depots-windows-1251.html"), cyrillic)
	sameFileSeries(t, htmlFileSeries(t, server, "depots_file", "charset/depots-shift_jis-meta.html"), []string{
		`depot_pallets{city="東京",depot="東京倉庫"} 210`,
	})
	sameFileSeries(t, htmlFileSeries(t, server, "depots_file", "charset/depots-utf16le-bom.html"), append(slices.Clone(cyrillic),
		`depot_pallets{city="Besançon",depot="Entrepôt côté forêt"} 310`,
		`depot_pallets{city="Köln",depot="Lager Süd – Größe „XL“, 5 €"} 48`,
		`depot_pallets{city="Łódź",depot="Skład główny"} 96`,
		`depot_pallets{city="東京",depot="東京倉庫"} 210`,
		`depot_pallets{city="上海",depot="上海仓库"} 505`,
		`depot_pallets{city="서울",depot="서울 창고"} 64`,
		`depot_pallets{city="Zürich 🇨🇭",depot="Depot 🚚✅"} 7`,
	))
	if logs.Len() != 0 {
		t.Errorf("reading the files logged:\n%s", logs)
	}

	sameFileSeries(t, htmlFileSeries(t, server, "depots_file", "charset/depots-windows-1251.html"), []string{
		"depot_pallets{city=\"�\",depot=\"� �\"} 120",
		"depot_pallets{city=\"�-�\",depot=\"� �2 �\"} 75",
	})
	records := testutil.AssertJSONLines(t, logs, 1)
	if msg, _ := records[0]["msg"].(string); records[0]["level"] != "WARN" || !strings.Contains(msg, "set response.charset") || records[0]["values"] != float64(4) || records[0]["first_metric"] != "depot_pallets" {
		t.Errorf("the repair was logged as\n%s", logs)
	}
}
