//go:build !select_request_types || request_type_http

package exporter

import (
	"net/http"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
	"golang.org/x/text/encoding/charmap"
)

// testdata/html/charset holds one page, a table of depots, in the encodings
// a target may answer in: UTF-8 with and without a byte order mark and with
// each kind of meta, UTF-16 of either byte order, and the rows each legacy
// encoding can write as windows-1251, KOI8-R, ISO-8859-1, windows-1252
// calling itself iso-8859-1, ISO-8859-2, Shift_JIS, GBK and EUC-KR. The
// names are Cyrillic, accented Latin, Polish, Japanese, Chinese, Korean and
// emoji, and whatever the encoding, the labels are those names.
//
// The encoding comes from the first of: a byte order mark, the collector's
// response.charset, the charset of the Content-Type, and a meta of the page
// (docs/CONFIGURATION.md, "Character encodings").

const charsetCollectors = `collectors:
  - name: depots_css
    request:
      type: http
    transform:
      type: css
    metrics:
      - name: depot_pallets
        items: '#depots tr:has(td)'
        expression: td.pallets
        labels:
          - name: depot
            expression: td.name
          - name: city
            expression: td.city

  # The decoder left to each answer.
  - name: depots_xpath
    request:
      type: http
    decoder:
      type: auto
    transform:
      type: xpath
    metrics:
      - name: depot_pallets
        expression: "//table[@id='depots']//td[@class='pallets']"
        labels:
          - name: depot
            expression: "../td[@class='name']"
          - name: city
            expression: "../td[@class='city']"

  # response.charset says what the target does not say, or says wrongly.
  - name: depots_css_windows_1251
    request:
      type: http
    response:
      charset: windows-1251
    transform:
      type: css
    metrics:
      - name: depot_pallets
        items: '#depots tr:has(td)'
        expression: td.pallets
        labels:
          - name: depot
            expression: td.name
          - name: city
            expression: td.city
`

// depotSeries are the series of the page's rows, by the lang of each row.
var depotSeries = map[string]string{
	"bg":       `depot_pallets{city="София",depot="Склад „Изток“"} 120`,
	"ru":       `depot_pallets{city="Санкт-Петербург",depot="Склад №2 «Север»"} 75`,
	"ru-plain": `depot_pallets{city="Санкт-Петербург",depot="Склад Север"} 75`,
	"fr":       `depot_pallets{city="Besançon",depot="Entrepôt côté forêt"} 310`,
	"de":       `depot_pallets{city="Köln",depot="Lager Süd – Größe „XL“, 5 €"} 48`,
	"pl":       `depot_pallets{city="Łódź",depot="Skład główny"} 96`,
	"ja":       `depot_pallets{city="東京",depot="東京倉庫"} 210`,
	"zh":       `depot_pallets{city="上海",depot="上海仓库"} 505`,
	"ko":       `depot_pallets{city="서울",depot="서울 창고"} 64`,
	"emoji":    `depot_pallets{city="Zürich 🇨🇭",depot="Depot 🚚✅"} 7`,
}

// depotRows are the series of the rows of those langs, in that order.
func depotRows(langs ...string) []string {
	var series []string
	for _, lang := range langs {
		series = append(series, depotSeries[lang])
	}
	return series
}

var everyDepot = []string{"bg", "ru", "fr", "de", "pl", "ja", "zh", "ko", "emoji"}

// readAsWindows1251 is a series as it comes out when its UTF-8 is taken for
// windows-1251: what a page gets that is one thing and is called another.
// A label value is read without the blanks around it, and a byte that ends
// a name may spell a no-break space there, which is then no part of it.
func readAsWindows1251(t *testing.T, series []string) []string {
	t.Helper()
	var misread []string
	for _, line := range series {
		text, err := charmap.Windows1251.NewDecoder().String(line)
		if err != nil {
			t.Fatal(err)
		}
		misread = append(misread, strings.ReplaceAll(text, "\u00a0\"", `"`))
	}
	return misread
}

// Every fixture, with every way a target has of saying its encoding, gives
// the depots their names: declared by the Content-Type, by a meta charset
// or a meta http-equiv in either case, quoted or not, by a byte order mark,
// or by nothing where the page is UTF-8. iso-8859-1 is read as
// windows-1252, as browsers read it, so the euro sign and the quotation
// marks of the page that calls itself iso-8859-1 are read. The css
// collector, whose decoder is HTML, and the xpath collector, which is left
// to each answer, read the same, and nothing is logged.
func TestTheCharsetFixturesGiveTheSameNamesInEveryEncoding(t *testing.T) {
	for _, test := range []struct {
		fixture, contentType string
		langs                []string
	}{
		{"depots-utf8.html", "text/html; charset=utf-8", everyDepot},
		{"depots-utf8.html", "text/html", everyDepot},
		{"depots-utf8.html", "", everyDepot},
		{"depots-utf8-meta.html", "text/html", everyDepot},
		{"depots-utf8-meta.html", "text/html; charset=UTF-8", everyDepot},
		{"depots-utf8-http-equiv.html", "text/html", everyDepot},
		{"depots-utf8-bom.html", "text/html", everyDepot},
		{"depots-utf8-bom.html", "", everyDepot},
		{"depots-utf16le-bom.html", "text/html", everyDepot},
		{"depots-utf16be-bom.html", "application/octet-stream", everyDepot},
		{"depots-windows-1251.html", "text/html; charset=windows-1251", []string{"bg", "ru"}},
		{"depots-windows-1251.html", `text/html;charset="WINDOWS-1251"`, []string{"bg", "ru"}},
		{"depots-windows-1251-meta.html", "text/html", []string{"bg", "ru"}},
		{"depots-windows-1251-meta.html", "text/html; charset=windows-1251", []string{"bg", "ru"}},
		{"depots-windows-1251-meta.html", "text/plain", []string{"bg", "ru"}},
		{"depots-windows-1251-http-equiv.html", "", []string{"bg", "ru"}},
		{"depots-koi8-r-meta.html", "text/html", []string{"ru-plain"}},
		{"depots-iso-8859-1.html", "text/html; charset=ISO-8859-1", []string{"fr"}},
		{"depots-iso-8859-1-meta.html", "text/html", []string{"fr", "de"}},
		{"depots-iso-8859-2-meta.html", "text/html", []string{"pl"}},
		{"depots-shift_jis-meta.html", "text/html", []string{"ja"}},
		{"depots-gbk-meta.html", "application/xhtml+xml", []string{"zh"}},
		{"depots-euc-kr-meta.html", "text/html", []string{"ko"}},
	} {
		t.Run(test.fixture+" as "+test.contentType, func(t *testing.T) {
			logs := testutil.CaptureLogs(t)
			body := htmlFixture(t, "charset/"+test.fixture)
			site := newHTMLSite(t, map[string]htmlPage{"/depots": {test.contentType, body}})
			server := htmlServer(htmlCollectors(t, charsetCollectors))
			for _, collector := range []string{"depots_css", "depots_xpath"} {
				status, series, failure := probeHTML(t, server, site, collector, "/depots")
				if status != http.StatusOK {
					t.Fatalf("%s: status=%d body=%s", collector, status, failure)
				}
				sameSeries(t, series, depotRows(test.langs...))
			}
			sameLogLines(t, logs, nil)
		})
	}
}

// A byte order mark is the first word: the page after one is read by it
// whatever the Content-Type and response.charset say.
func TestAByteOrderMarkIsReadBeforeAnyDeclaration(t *testing.T) {
	logs := testutil.CaptureLogs(t)
	server := htmlServer(htmlCollectors(t, charsetCollectors))
	site := newHTMLSite(t, map[string]htmlPage{
		"/utf8":    {"text/html; charset=windows-1251", htmlFixture(t, "charset/depots-utf8-bom.html")},
		"/utf16be": {"text/html; charset=utf-8", htmlFixture(t, "charset/depots-utf16be-bom.html")},
		"/utf16le": {"text/html; charset=iso-8859-1", htmlFixture(t, "charset/depots-utf16le-bom.html")},
	})
	for _, path := range []string{"/utf8", "/utf16be", "/utf16le"} {
		for _, collector := range []string{"depots_css", "depots_xpath", "depots_css_windows_1251"} {
			status, series, failure := probeHTML(t, server, site, collector, path)
			if status != http.StatusOK {
				t.Fatalf("%s at %s: status=%d body=%s", collector, path, status, failure)
			}
			sameSeries(t, series, depotRows(everyDepot...))
		}
	}
	sameLogLines(t, logs, nil)
}

// response.charset is read before the Content-Type and the page's meta. It
// gives a target that declares nothing its names, and it is believed over a
// target that declares the truth: the UTF-8 page, which says so twice, is
// read as windows-1251 and comes out as the text its bytes spell there.
func TestResponseCharsetIsReadBeforeTheContentTypeAndTheMeta(t *testing.T) {
	logs := testutil.CaptureLogs(t)
	server := htmlServer(htmlCollectors(t, charsetCollectors))
	site := newHTMLSite(t, map[string]htmlPage{
		"/undeclared": {"text/html", htmlFixture(t, "charset/depots-windows-1251.html")},
		"/wrong":      {"text/html; charset=utf-8", htmlFixture(t, "charset/depots-windows-1251.html")},
		"/utf8":       {"text/html; charset=utf-8", htmlFixture(t, "charset/depots-utf8-meta.html")},
	})
	for _, path := range []string{"/undeclared", "/wrong"} {
		status, series, failure := probeHTML(t, server, site, "depots_css_windows_1251", path)
		if status != http.StatusOK {
			t.Fatalf("%s: status=%d body=%s", path, status, failure)
		}
		sameSeries(t, series, depotRows("bg", "ru"))
	}
	status, series, failure := probeHTML(t, server, site, "depots_css_windows_1251", "/utf8")
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, failure)
	}
	sameSeries(t, series, readAsWindows1251(t, depotRows(everyDepot...)))
	sameLogLines(t, logs, nil)
}

// The charset of the Content-Type is read before the page's meta, as a
// browser reads them: a UTF-8 page that says so in its meta, sent by a
// server that calls everything windows-1251, is read as windows-1251. And a
// Content-Type naming UTF-16 is taken as it says, which leaves nothing of
// a UTF-8 page for a rule to find.
func TestTheContentTypeCharsetIsReadBeforeTheMeta(t *testing.T) {
	logs := testutil.CaptureLogs(t)
	server := htmlServer(htmlCollectors(t, charsetCollectors))
	site := newHTMLSite(t, map[string]htmlPage{
		"/misnamed": {"text/html; charset=windows-1251", htmlFixture(t, "charset/depots-utf8-meta.html")},
		"/utf16":    {"text/html; charset=utf-16", htmlFixture(t, "charset/depots-utf8.html")},
	})
	for _, collector := range []string{"depots_css", "depots_xpath"} {
		status, series, failure := probeHTML(t, server, site, collector, "/misnamed")
		if status != http.StatusOK {
			t.Fatalf("%s: status=%d body=%s", collector, status, failure)
		}
		sameSeries(t, series, readAsWindows1251(t, depotRows(everyDepot...)))
	}
	sameLogLines(t, logs, nil)

	status, series, failure := probeHTML(t, server, site, "depots_css", "/utf16")
	if status != http.StatusOK || len(series) != 0 {
		t.Fatalf("status=%d body=%s series=%v, want an answer without series", status, failure, series)
	}
	sameLogLines(t, logs, []string{
		`WARN metric extraction failed metric=depot_pallets failures=1 error=metric "depot_pallets" items CSS selector "#depots tr:has(td)" matched no nodes`,
	})
}

// A page in a legacy encoding that nothing declares, and one whose
// Content-Type calls it UTF-8 over a meta that says the truth, is not valid
// UTF-8. The scrape is answered all the same: each run of bytes that is no
// UTF-8 is U+FFFD in the label values, and one warning says how many values
// were repaired, in which metric first, and what to set.
func TestAnUndeclaredLegacyEncodingIsRepairedAndReported(t *testing.T) {
	logs := testutil.CaptureLogs(t)
	server := htmlServer(htmlCollectors(t, charsetCollectors))
	site := newHTMLSite(t, map[string]htmlPage{
		"/undeclared": {"text/html", htmlFixture(t, "charset/depots-windows-1251.html")},
		"/wrong":      {"text/html; charset=utf-8", htmlFixture(t, "charset/depots-windows-1251-meta.html")},
	})
	for _, path := range []string{"/undeclared", "/wrong"} {
		for _, collector := range []string{"depots_css", "depots_xpath"} {
			status, series, failure := probeHTML(t, server, site, collector, path)
			if status != http.StatusOK {
				t.Fatalf("%s at %s: status=%d body=%s", collector, path, status, failure)
			}
			sameSeries(t, series, []string{
				"depot_pallets{city=\"�\",depot=\"� �\"} 120",
				"depot_pallets{city=\"�-�\",depot=\"� �2 �\"} 75",
			})
			sameLogLines(t, logs, []string{
				`WARN label values or help text were not valid UTF-8; the invalid bytes were replaced with U+FFFD. If the target uses another encoding without declaring it, set response.charset values=4 first_metric=depot_pallets`,
			})
		}
	}
}

// An encoding nobody knows fails the decode, naming it and what a name
// looks like, whichever transform reads the page.
func TestAnUnknownCharsetFailsTheDecode(t *testing.T) {
	logs := testutil.CaptureLogs(t)
	server := htmlServer(htmlCollectors(t, charsetCollectors))
	site := newHTMLSite(t, map[string]htmlPage{"/depots": {"text/html; charset=klingon", htmlFixture(t, "charset/depots-utf8.html")}})
	const why = `unsupported charset "klingon"; use a name from the WHATWG Encoding Standard, such as utf-8, windows-1252, iso-8859-2, windows-1251, shift_jis or gbk`
	for _, collector := range []string{"depots_css", "depots_xpath"} {
		status, _, failure := probeHTML(t, server, site, collector, "/depots")
		if want := "collector " + collector + " decode failed: " + why; status != http.StatusBadGateway || failure != want {
			t.Errorf("%s: status=%d body=%s, want 502 %s", collector, status, failure, want)
		}
		sameLogLines(t, logs, []string{"ERROR probe failed stage=decode error=" + why})
	}
	// response.charset names an encoding of its own, and the header's is
	// not looked at.
	status, series, failure := probeHTML(t, server, site, "depots_css_windows_1251", "/depots")
	if status != http.StatusOK || !strings.HasPrefix(strings.Join(series, "\n"), `depot_pallets{`) {
		t.Errorf("with response.charset: status=%d body=%s series=%v", status, failure, series)
	}
	sameSeries(t, series, readAsWindows1251(t, depotRows(everyDepot...)))
	sameLogLines(t, logs, nil)
}
