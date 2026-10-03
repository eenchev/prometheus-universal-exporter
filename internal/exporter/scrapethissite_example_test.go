//go:build !select_request_types || request_type_http

package exporter

import (
	"bytes"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// examples/config.scrapethissite.html-test.yaml reads "Countries of the
// World" on scrapethissite.com with the css transform: one block per
// country, selected by items, with the value and the country's name read
// within it. testdata/html/scrapethissite-countries.html is a page in the
// markup that page has, with thirteen of its countries: the name on a line
// of its own after an empty flag element, an area written 468.0, and
// Antarctica, which has nobody and an area written 1.4E7.

const (
	scrapeThisSiteConfig = "../../examples/config.scrapethissite.html-test.yaml"
	scrapeThisSitePath   = "/pages/simple/"
)

// newScrapeThisSite starts a stand-in holding page where the site has its
// countries, and loads the example as shipped. The example allows http, so
// the stand-in needs no certificate.
func newScrapeThisSite(t *testing.T, page []byte) (*htmlSite, *Server) {
	t.Helper()
	site := newHTMLSite(t, map[string]htmlPage{scrapeThisSitePath: {"text/html; charset=utf-8", page}})
	cfg, err := config.Load(scrapeThisSiteConfig)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Collectors) != 1 || cfg.Collectors[0].Name != "countries_html" {
		t.Fatalf("%s no longer holds the one collector countries_html", scrapeThisSiteConfig)
	}
	return site, NewServer(config.NewManager(cfg, scrapeThisSiteConfig, slog.Default()), "python3", slog.Default())
}

// scrapeThisSiteSeries are the series of the fixture's thirteen countries.
var scrapeThisSiteSeries = []string{
	`country_population{country="Andorra"} 84000`,
	`country_population{country="United Arab Emirates"} 4.975593e+06`,
	`country_population{country="Afghanistan"} 2.9121286e+07`,
	`country_population{country="Antigua and Barbuda"} 86754`,
	`country_population{country="Anguilla"} 13254`,
	`country_population{country="Albania"} 2.986952e+06`,
	`country_population{country="Armenia"} 2.968e+06`,
	`country_population{country="Angola"} 1.3068161e+07`,
	`country_population{country="Antarctica"} 0`,
	`country_population{country="Argentina"} 4.1343201e+07`,
	`country_population{country="American Samoa"} 57881`,
	`country_population{country="Austria"} 8.205e+06`,
	`country_population{country="Bulgaria"} 7.148785e+06`,
	`country_area{country="Andorra"} 468`,
	`country_area{country="United Arab Emirates"} 82880`,
	`country_area{country="Afghanistan"} 647500`,
	`country_area{country="Antigua and Barbuda"} 443`,
	`country_area{country="Anguilla"} 102`,
	`country_area{country="Albania"} 28748`,
	`country_area{country="Armenia"} 29800`,
	`country_area{country="Angola"} 1.2467e+06`,
	// 1.4E7, as the page writes it.
	`country_area{country="Antarctica"} 1.4e+07`,
	`country_area{country="Argentina"} 2.76689e+06`,
	`country_area{country="American Samoa"} 199`,
	`country_area{country="Austria"} 83858`,
	`country_area{country="Bulgaria"} 110910`,
}

// Every country of the page is two series, its population and its area,
// each named by the country as the page writes it, without the blanks
// around the name. The collector asks for the page once, at the path the
// example names and saying it accepts HTML, and nothing is logged.
func TestTheScrapeThisSiteExampleReadsEveryCountry(t *testing.T) {
	logs := testutil.CaptureLogs(t)
	site, server := newScrapeThisSite(t, htmlFixture(t, "scrapethissite-countries.html"))

	status, got, failure := probeHTML(t, server, site, "countries_html", "")
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, failure)
	}
	sameSeries(t, got, scrapeThisSiteSeries)
	paths, accepts := site.requests()
	if !slices.Equal(paths, []string{scrapeThisSitePath}) || !slices.Equal(accepts, []string{"text/html"}) {
		t.Errorf("the stand-in was asked %v with Accept %v, want %s once with text/html", paths, accepts, scrapeThisSitePath)
	}
	sameLogLines(t, logs, nil)
}

// A second probe within the minute of cache.ttl is answered from memory.
func TestTheScrapeThisSiteExampleAnswersASecondProbeFromMemory(t *testing.T) {
	testutil.CaptureLogs(t)
	site, server := newScrapeThisSite(t, htmlFixture(t, "scrapethissite-countries.html"))

	_, first, _ := probeHTML(t, server, site, "countries_html", "")
	_, second, _ := probeHTML(t, server, site, "countries_html", "")
	sameSeries(t, first, scrapeThisSiteSeries)
	sameSeries(t, second, first)
	if paths, _ := site.requests(); len(paths) != 1 {
		t.Errorf("the stand-in was asked %d times by two probes within cache.ttl, want once", len(paths))
	}
}

// The country label is required: a block whose name is gone is no series of
// either metric, rather than one nobody can tell from the others, and each
// rule says so once; the other countries are served. A page without the
// blocks, as a redesign would leave it, is no series at all, each rule
// saying that its items matched nothing.
func TestTheScrapeThisSiteExampleSaysWhatThePageNoLongerHolds(t *testing.T) {
	logs := testutil.CaptureLogs(t)
	page := htmlFixture(t, "scrapethissite-countries.html")
	nameless := bytes.Replace(page, []byte("                Bulgaria\n"), []byte("\n"), 1)
	if bytes.Equal(nameless, page) {
		t.Fatal("the fixture no longer names Bulgaria on a line of its own")
	}
	site, server := newScrapeThisSite(t, nameless)
	status, got, failure := probeHTML(t, server, site, "countries_html", "")
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, failure)
	}
	sameSeries(t, got, slices.DeleteFunc(slices.Clone(scrapeThisSiteSeries), func(line string) bool {
		return strings.Contains(line, `country="Bulgaria"`)
	}))
	sameLogLines(t, logs, []string{
		`WARN metric extraction failed metric=country_population failures=1 error=metric "country_population" label "country" is missing for item 12`,
		`WARN metric extraction failed metric=country_area failures=1 error=metric "country_area" label "country" is missing for item 12`,
	})

	redesigned := bytes.ReplaceAll(page, []byte(`col-md-4 country"`), []byte(`col-md-4 nation"`))
	site, server = newScrapeThisSite(t, redesigned)
	status, got, failure = probeHTML(t, server, site, "countries_html", "")
	if status != http.StatusOK || len(got) != 0 {
		t.Fatalf("status=%d body=%s series=%v, want an answer without series", status, failure, got)
	}
	sameLogLines(t, logs, []string{
		`WARN metric extraction failed metric=country_population failures=1 error=metric "country_population" items CSS selector "div.country" matched no nodes`,
		`WARN metric extraction failed metric=country_area failures=1 error=metric "country_area" items CSS selector "div.country" matched no nodes`,
	})
}
