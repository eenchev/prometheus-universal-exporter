//go:build !select_request_types || request_type_http

package exporter

import (
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// What a probe adds to reading a page: which decoder reads the answer, by
// what the collector sets and what the target says; the request the
// collector makes; and what the configuration refuses before any probe.

const decoderCollectors = `collectors:
  # A css collector reads HTML, whatever it sets and whatever the target says.
  - name: css_unset
    request:
      type: http
    transform:
      type: css
    metrics: &css
      - name: page_titled
        expression: title
        value_map: {"*": 1}
  - name: css_auto
    request:
      type: http
    decoder:
      type: auto
    transform:
      type: css
    metrics: *css
  - name: css_html
    request:
      type: http
    decoder:
      type: html
    transform:
      type: css
    metrics: *css
  # An xpath collector reads XML and HTML: left unset or at auto, each
  # answer says which it is.
  - name: xpath_unset
    request:
      type: http
    transform:
      type: xpath
    metrics: &xpath
      - name: page_titled
        expression: //title
        value_map: {"*": 1}
        labels:
          - name: title
            expression: .
  - name: xpath_auto
    request:
      type: http
    decoder:
      type: auto
    transform:
      type: xpath
    metrics: *xpath
  - name: xpath_html
    request:
      type: http
    decoder:
      type: html
    transform:
      type: xpath
    metrics: *xpath
`

// A css collector reads every answer as HTML, with decoder.type unset, auto
// or html, whatever Content-Type the target sends. An xpath collector with
// decoder.type html does too. One that leaves the decoder to each answer,
// unset or auto, goes by the Content-Type when it names a format and by the
// content when it does not: text/html is HTML; application/xhtml+xml,
// text/plain, application/octet-stream and no Content-Type at all say
// nothing, and a page that starts with a doctype, an <HTML> in any case or
// an XML declaration before its doctype is HTML by its content. A target
// that calls its page something else is believed: as XML a page fails to
// parse unless it is XHTML, which is then read as XML; as JSON, YAML or
// Prometheus text it fails the decode; as CSV it decodes, to something
// XPath cannot read. Each failure names its stage and is logged once.
//
// Only the collector that sets no decoder.type where its transform implies
// none is warned about when the configuration loads.
func TestHTMLIsReadByTheDecoderTheCollectorSetsOrTheAnswerNames(t *testing.T) {
	logs := testutil.CaptureLogs(t)
	cfg := htmlCollectors(t, decoderCollectors)
	if want := []string{`collector "xpath_unset" sets no decoder.type, so it decodes by the Content-Type header of each response, and by the content when that does not say; set decoder.type to fix the decoder`}; !slices.Equal(cfg.Warnings, want) {
		t.Errorf("the configuration loads with the warnings\n%s\nwant\n%s", strings.Join(cfg.Warnings, "\n"), strings.Join(want, "\n"))
	}
	server := htmlServer(cfg)

	pages := []struct {
		fixture, title string
		// asXML and asYAML are how the page fails when it is called XML and
		// YAML; a page that is well-formed XML does not fail as XML.
		asXML, asYAML string
	}{
		{
			fixture: "server-status.html", title: "Statistics Report for lb01.example.net",
			asXML:  "XML decode: XML syntax error on line 16: element <meta> closed by </head>",
			asYAML: "YAML decode: yaml: line 8: mapping values are not allowed in this context",
		},
		{
			fixture: "sloppy.html", title: "PDU-7 Outlet Status",
			asXML:  "XML decode: XML syntax error on line 4: unquoted or missing attribute value in element",
			asYAML: "YAML decode: yaml: line 9: mapping values are not allowed in this context",
		},
		{fixture: "attribute-names.xhtml", title: "Checkout service"},
	}
	const (
		asJSON       = "JSON decode: invalid character '<' looking for beginning of value, at line 1, column 1"
		asPrometheus = "decoding Prometheus exposition: text format parsing error in line 1: invalid metric name"
	)
	everyCollector := []string{"css_unset", "css_auto", "css_html", "xpath_unset", "xpath_auto", "xpath_html"}
	for _, page := range pages {
		// What each Content-Type makes of the page for a collector that
		// leaves the decoder to the answer: read, or failed in a stage.
		type outcome struct{ stage, failure string }
		contentTypes := map[string]outcome{
			"text/html; charset=utf-8":     {},
			"text/html":                    {},
			"application/xhtml+xml":        {},
			"text/plain":                   {},
			"text/plain; charset=utf-8":    {},
			"application/octet-stream":     {},
			"":                             {},
			"application/xml":              {"decode", page.asXML},
			"text/xml; charset=utf-8":      {"decode", page.asXML},
			"application/json":             {"decode", asJSON},
			"application/problem+json":     {"decode", asJSON},
			"text/plain; version=0.0.4":    {"decode", asPrometheus},
			"application/yaml":             {"transform", `xpath transform requires an XML or HTML response, got "yaml"`},
			"text/csv":                     {"transform", `xpath transform requires an XML or HTML response, got "csv"`},
			"TEXT/HTML; Charset=\"utf-8\"": {},
		}
		if page.asYAML != "" {
			contentTypes["application/yaml"] = outcome{"decode", page.asYAML}
		}
		for _, contentType := range slices.Sorted(maps.Keys(contentTypes)) {
			left := contentTypes[contentType]
			if left.stage == "decode" && left.failure == "" {
				// The XHTML page called XML is read as XML.
				left = outcome{}
			}
			t.Run(page.fixture+" as "+contentType, func(t *testing.T) {
				site := newHTMLSite(t, map[string]htmlPage{"/page": {contentType, htmlFixture(t, page.fixture)}})
				for _, collector := range everyCollector {
					status, series, failure := probeHTML(t, server, site, collector, "/page")
					toTheAnswer := collector == "xpath_unset" || collector == "xpath_auto"
					if toTheAnswer && left.failure != "" {
						if want := "collector " + collector + " " + left.stage + " failed: " + left.failure; status != http.StatusBadGateway || failure != want {
							t.Errorf("%s: status=%d body=%s\nwant 502 %s", collector, status, failure, want)
						}
						sameLogLines(t, logs, []string{"ERROR probe failed stage=" + left.stage + " error=" + left.failure})
						continue
					}
					if status != http.StatusOK {
						t.Errorf("%s: status=%d body=%s", collector, status, failure)
						continue
					}
					want := `page_titled 1`
					if strings.HasPrefix(collector, "xpath") {
						want = `page_titled{title="` + page.title + `"} 1`
					}
					sameSeries(t, series, []string{want})
					sameLogLines(t, logs, nil)
				}
			})
		}
	}
}

// The status page collector asks for the page once, saying it accepts HTML,
// and logs nothing. What its pre_script does shows without it: the page's
// own text is 99.982%, 1,204,551, 87 ms and "5 minutes ago", none of them a
// number or a time, and each rule that reads one says so — but the rules
// that are told to pass by what they cannot read.
func TestTheStatusPageCollectorReadsWhatItsPreScriptMakesReadable(t *testing.T) {
	requirePython(t)
	logs := testutil.CaptureLogs(t)
	cfg := htmlCollectors(t, statusPageCollectors)
	site := newHTMLSite(t, map[string]htmlPage{"/": {"text/html; charset=utf-8", htmlFixture(t, "status-page.html")}})
	server := htmlServer(cfg)

	status, series, failure := probeHTML(t, server, site, "acme_status", "/")
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, failure)
	}
	// The series themselves are named by
	// TestTheStatusPageFixtureIsReadByCSSAndXPath, which holds the transform
	// to them: the probe answers what the transform makes.
	direct, err := transformHTML(htmlCollector(t, cfg, "acme_status"), "text/html; charset=utf-8", htmlFixture(t, "status-page.html"))
	if err != nil || len(direct) != 30 {
		t.Fatalf("the transform made %d series and failed with %v, want 30 series", len(direct), err)
	}
	sameSeries(t, series, direct)
	paths, accepts := site.requests()
	if !slices.Equal(paths, []string{"/"}) || !slices.Equal(accepts, []string{"text/html"}) {
		t.Errorf("the page was asked for as %v with Accept %v, want / once with text/html", paths, accepts)
	}
	sameLogLines(t, logs, nil)

	scripted, rest, found := strings.Cut(statusPageCollectors, "      # The page writes for people")
	_, rest, labelled := strings.Cut(rest, "      labels:\n")
	if !found || !labelled {
		t.Fatal("statusPageCollectors no longer holds the pre_script between its comment and transform.labels")
	}
	unscripted := htmlCollectors(t, scripted+"      labels:\n"+rest)
	if script := htmlCollector(t, unscripted, "acme_status").Transform.PreScript; script != "" {
		t.Fatalf("the collector still has the pre_script\n%s", script)
	}
	status, series, failure = probeHTML(t, htmlServer(unscripted), site, "acme_status", "/")
	if status != http.StatusOK {
		t.Fatalf("without the pre_script: status=%d body=%s", status, failure)
	}
	sameSeries(t, series, []string{
		`acme_statuspage_degraded{page="acme-cloud"} 1`,
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
		`acme_statuspage_incident_info{impact="major",incident="CDN: elevated error rates in all regions",message="We have identified a faulty configuration push to the edge nodes as the cause of the elevated 5xx rates and are rolli…",page="acme-cloud"} 1`,
		`acme_statuspage_incident_info{impact="minor",incident="Slow sign-ins",message="A fix is deployed.",page="acme-cloud"} 1`,
		`acme_statuspage_maintenance_in_progress{incident="DNS resolver upgrade",page="acme-cloud"} 1`,
	})
	sameLogLines(t, logs, []string{
		`WARN metric extraction failed metric=statuspage_updated_timestamp_seconds failures=1 error=metric "statuspage_updated_timestamp_seconds": value "5 minutes ago" is not a time in time_format "rfc3339", which reads times such as 2006-01-02T15:04:05Z and 2006-01-02T15:04:05.999+02:00`,
		`WARN metric extraction failed metric=statuspage_uptime_ratio failures=1 error=metric "statuspage_uptime_ratio": value "99.982%" is not a number; map text to numbers with value_map`,
		`WARN metric extraction failed metric=statuspage_requests_today failures=1 error=metric "statuspage_requests_today": value "1,204,551" is not a number; map text to numbers with value_map`,
		`WARN metric extraction failed metric=statuspage_component_uptime_ratio failures=5 error=metric "statuspage_component_uptime_ratio" item 0: value "99.98%" is not a number; map text to numbers with value_map`,
		`WARN metric extraction failed metric=statuspage_incident_started_timestamp_seconds failures=3 error=metric "statuspage_incident_started_timestamp_seconds" item 0: value "Oct 3, 07:42 UTC" is not a time in time_format "rfc3339", which reads times such as 2006-01-02T15:04:05Z and 2006-01-02T15:04:05.999+02:00`,
	})
}

const namespacedCollector = `collectors:
  - name: checkout
    request:
      type: http
    DECODER
    response:
      namespaces:
        m: urn:example:metrics
    transform:
      type: xpath
    metrics:
      - name: host_load
        expression: "//ul[@id='hosts']/li/span[@class='load']"
        labels:
          - name: host
            expression: ../@data-id
          - name: read
            expression: LABEL
`

// response.namespaces says a collector reads XML. With the decoder left to
// each answer, unset or auto, its labels are then checked as XML's when the
// configuration loads: a name only HTML has, and a prefix the map does not
// have, are refused, and the error says to set decoder.type to html if the
// target answers HTML. With decoder.type xml the same labels are refused
// without that advice, and with decoder.type html they load, and read the
// attributes of the HTML page by their names as written.
func TestAttributeLabelsOfACollectorWithNamespacesAreCheckedAsXML(t *testing.T) {
	testutil.CaptureLogs(t)
	const advice = "response.namespaces is set, so the labels are checked as those of an XML document: if the target answers HTML, where this label is an attribute's name as written, set decoder.type to html"
	page := htmlFixture(t, "attribute-names.html")
	for label, test := range map[string]struct{ why, read string }{
		"../@2x":                 {"expression must evaluate to a node-set", "hi-dpi"},
		"../@:href":              {"../@:href has an invalid token.", "'/hosts/' + h1"},
		"../@@click":             {"expression must evaluate to a node-set", "open(h1)"},
		"../@x-on:click.prevent": {"prefix x-on not defined.", "toggle"},
		"../../@og:type":         {"prefix og not defined.", "list"},
		"../../@v-on:click":      {"prefix v-on not defined.", "select"},
	} {
		document := strings.Replace(namespacedCollector, "LABEL", "'"+label+"'", 1)
		for _, decoder := range []string{"", "decoder: {type: auto}", "decoder: {type: xml}"} {
			_, err := config.Load(testutil.WriteFile(t, "config.yaml", strings.Replace(document, "DECODER", decoder, 1)))
			want := `collector "checkout" metric "host_load" label "read" XPath "` + label + `": ` + test.why
			if !strings.Contains(decoder, "xml") {
				want += "; " + advice
			}
			if err == nil || err.Error() != want {
				t.Errorf("%s with %q loads with\n%v\nwant\n%s", label, decoder, err, want)
			}
		}
		cfg := htmlCollectors(t, strings.Replace(document, "DECODER", "decoder: {type: html}", 1))
		series, err := transformHTML(htmlCollector(t, cfg, "checkout"), "text/html", page)
		if err != nil {
			t.Fatalf("%s with decoder.type html: %v", label, err)
		}
		// The third host has none of the framework's attributes, and is
		// under the same list as the others.
		third := `host_load{host="h3"} 0.08`
		if strings.HasPrefix(label, "../../") {
			third = `host_load{host="h3",read="` + test.read + `"} 0.08`
		}
		second := strings.NewReplacer("h1", "h2", "hi-dpi", "lo-dpi").Replace(test.read)
		sameSeries(t, series, []string{
			`host_load{host="h1",read="` + test.read + `"} 0.42`,
			`host_load{host="h2",read="` + second + `"} 1.75`,
			third,
		})
	}
}
