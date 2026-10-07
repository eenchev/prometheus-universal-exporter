//go:build !select_request_types || request_type_http

package exporter

import (
	"slices"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// htmlExpectation is one line of what docs/CONFIGURATION.md tells a reader
// to expect of HTML ("Reading HTML: what to expect"): a small page in the
// shape the line speaks of, the rules of a css or an xpath collector that
// reads it, written as a configuration file holds them, and what comes of
// them — the error of a configuration that does not load, or else every
// series and every log line.
type htmlExpectation struct {
	name      string
	transform string
	rules     string
	page      string
	refused   string
	series    []string
	logs      []string
}

func (e htmlExpectation) run(t *testing.T) {
	t.Helper()
	t.Run(e.name, func(t *testing.T) {
		logs := testutil.CaptureLogs(t)
		document := "collectors:\n  - name: page\n    request:\n      type: http\n    decoder:\n      type: html\n    transform:\n      type: " + e.transform + "\n    metrics:\n" + e.rules
		cfg, err := config.Load(testutil.WriteFile(t, "config.yaml", document))
		if e.refused != "" {
			if err == nil || !strings.Contains(err.Error(), e.refused) {
				t.Errorf("the configuration gave %v, want it refused with %q", err, e.refused)
			}
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		series, err := transformHTML(&cfg.Collectors[0], "text/html", []byte(e.page))
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(series, e.series) {
			t.Errorf("series\n%s\nwant\n%s", strings.Join(series, "\n"), strings.Join(e.series, "\n"))
		}
		sameLogLines(t, logs, e.logs)
	})
}

// Under items the selectors of a css rule match beneath the item and nowhere
// else. There is no way to say the item itself: :scope and a selector that
// starts with a combinator are refused when the configuration loads. A label
// that stands above the rows, a heading over their table, is beneath no row,
// so a selector for it leaves the label off without a word, while an item
// that holds the heading and the cells reads both; xpath reads it through
// ancestor::, and reads a list of bare values, each its own item, with a
// label from the list.
func TestHTMLExpectedOfSelectorsUnderItems(t *testing.T) {
	const sites = `<div class="site" data-site="east"><h2>East</h2><table>
<tr><td class="name">web01</td><td class="load">1</td></tr>
<tr><td class="name">web02</td><td class="load">2</td></tr>
</table></div>
<ul id="queues"><li>12</li><li>7</li></ul>`
	for _, expectation := range []htmlExpectation{
		{name: ":scope is refused", transform: "css", page: sites,
			rules:   "      - name: queue_depth\n        items: li\n        expression: ':scope'\n",
			refused: `metric "queue_depth" CSS selector ":scope": unknown pseudoclass or pseudoelement :scope`},
		{name: "a selector that starts with a combinator is refused", transform: "css", page: sites,
			rules:   "      - name: queue_depth\n        items: ul\n        expression: '> li'\n",
			refused: `metric "queue_depth" CSS selector "> li": expected identifier, found > instead`},
		{name: "a label above the rows is left off", transform: "css", page: sites,
			rules: `      - name: host_load
        items: div.site tr
        expression: td.load
        labels:
          - name: host
            expression: td.name
          - name: site
            expression: div.site h2
          - name: heading
            expression: h2
`,
			series: []string{`host_load{host="web01"} 1`, `host_load{host="web02"} 2`}},
		{name: "unless the item is the element that holds both", transform: "css", page: sites,
			rules: `      - name: site_hosts
        items: div.site
        expression: h2
        value_map: {East: 2}
        labels:
          - name: site
            expression: h2
          - name: first_host
            expression: tr:first-child td.name
`,
			series: []string{`site_hosts{first_host="web01",site="East"} 2`}},
		{name: "which xpath reads through its ancestors", transform: "xpath", page: sites,
			rules: `      - name: host_load
        expression: "//div[@class='site']//tr/td[@class='load']"
        labels:
          - name: host
            expression: "../td[@class='name']"
          - name: site
            expression: ancestor::div/h2
          - name: site_id
            expression: ancestor::div/@data-site
`,
			series: []string{`host_load{host="web01",site="East",site_id="east"} 1`, `host_load{host="web02",site="East",site_id="east"} 2`}},
		{name: "and a list of bare values is for xpath", transform: "xpath", page: sites,
			rules: `      - name: queue_depth
        expression: "//ul[@id='queues']/li"
        labels:
          - name: position
            expression: count(preceding-sibling::li) + 1
`,
			series: []string{`queue_depth{position="1"} 12`, `queue_depth{position="2"} 7`}},
	} {
		expectation.run(t)
	}
}

// Names are as the HTML parser keeps them. Inside svg and math that is not
// lower case: viewBox, linearGradient and definitionURL keep their capitals,
// and XPath says them so and finds nothing in lower case. A selector lowers the names it is
// given, so css finds neither such an element nor such an attribute by its
// name, in any case, and selects them by an id, a class or their place. An
// attribute the parser gives a namespace, xlink:href, has href for its name
// in an expression — name() = 'xlink:href' finds nothing — while a label
// that is the one attribute's name reads it as written.
func TestHTMLExpectedOfNamesInsideSVGAndMath(t *testing.T) {
	const gauge = `<svg id="gauge" viewBox="0 0 100 40" xmlns:xlink="http://www.w3.org/1999/xlink">
<linearGradient id="fill" class="level" gradientUnits="userSpaceOnUse">70</linearGradient>
<a xlink:href="/agents/linux-01"><text>63</text></a>
</svg>
<math><mi definitionURL="http://example.net/def">x</mi></math>`
	count := func(name, expression string) string {
		return "      - name: " + name + "\n        expression: \"count(" + expression + ")\"\n"
	}
	for _, expectation := range []htmlExpectation{
		{name: "xpath says the names with their capitals", transform: "xpath", page: gauge,
			rules: count("gradients", "//linearGradient") + count("gradients_lower_case", "//lineargradient") +
				count("view_boxes", "//svg/@viewBox") + count("view_boxes_lower_case", "//svg/@viewbox") +
				count("units", "//*[@gradientUnits]") + count("units_lower_case", "//*[@gradientunits]") +
				count("links_by_name", "//a/@*[name()='xlink:href']") + count("links_by_local_name", "//a/@*[local-name()='href']") +
				count("definitions", "//mi/@definitionURL") + count("definitions_lower_case", "//mi/@definitionurl"),
			series: []string{
				`gradients 1`, `gradients_lower_case 0`, `view_boxes 1`, `view_boxes_lower_case 0`,
				`units 1`, `units_lower_case 0`, `links_by_name 0`, `links_by_local_name 1`,
				`definitions 1`, `definitions_lower_case 0`,
			}},
		{name: "and a label that is an attribute's name reads it as written", transform: "xpath", page: gauge,
			rules: `      - name: agent_cpu_percent
        expression: //svg//text
        labels:
          - name: link
            expression: ../@xlink:href
          - name: view_box
            expression: ../../@viewBox
          - name: view_box_lower_case
            expression: ../../@viewbox
`,
			series: []string{`agent_cpu_percent{link="/agents/linux-01",view_box="0 0 100 40"} 63`}},
		{name: "css selects them by an id, a class or their place", transform: "css", page: gauge,
			rules: `      - name: level
        expression: '#fill'
        labels:
          - name: by
            value: id
      - name: level
        expression: svg .level
        labels:
          - name: by
            value: class
      - name: level
        expression: svg > *:first-child
        labels:
          - name: by
            value: place
      - name: level_by_name
        expression: linearGradient
      - name: level_by_lower_case_name
        expression: lineargradient
      - name: level_by_attribute
        expression: '[gradientUnits]'
      - name: reading_by_attribute
        expression: svg[viewBox] text
      - name: reading
        expression: svg text
`,
			series: []string{`level{by="id"} 70`, `level{by="class"} 70`, `level{by="place"} 70`, `reading 63`},
			logs: []string{
				`ERROR metric extraction failed metric=level_by_name failures=1 error=metric "level_by_name" CSS selector "linearGradient" matched no nodes`,
				`ERROR metric extraction failed metric=level_by_lower_case_name failures=1 error=metric "level_by_lower_case_name" CSS selector "lineargradient" matched no nodes`,
				`ERROR metric extraction failed metric=level_by_attribute failures=1 error=metric "level_by_attribute" CSS selector "[gradientUnits]" matched no nodes`,
				`ERROR metric extraction failed metric=reading_by_attribute failures=1 error=metric "reading_by_attribute" CSS selector "svg[viewBox] text" matched no nodes`,
			}},
	} {
		expectation.run(t)
	}
}

// What a label keeps of the blanks in what it reads. The blanks around a
// text are trimmed and those inside it are kept, line breaks, indentation
// and tabs included, by css and xpath alike; xpath can make single spaces of
// them with normalize-space(), and css has nothing of the kind. An attribute
// is trimmed like a text, read by its name on the node the rule selects or
// through a path.
func TestHTMLExpectedOfTheBlanksInALabel(t *testing.T) {
	const page = "<table><tr title=\"  rack 7  \"><td class=\"name\">  web01\n      (primary)\tspare  </td><td class=\"load\" title=\"  1 min  \">3</td></tr></table>"
	for _, expectation := range []htmlExpectation{
		{name: "css keeps the blanks inside a text", transform: "css", page: page,
			rules: `      - name: host_load
        items: tr
        expression: td.load
        labels:
          - name: host
            expression: td.name
`,
			series: []string{`host_load{host="web01\n      (primary)	spare"} 3`}},
		{name: "xpath too, unless normalize-space() is asked for", transform: "xpath", page: page,
			rules: `      - name: host_load
        expression: "//td[@class='load']"
        labels:
          - name: host
            expression: "../td[@class='name']"
          - name: host_on_one_line
            expression: "normalize-space(../td[@class='name'])"
          - name: own_attribute
            expression: '@title'
          - name: own_attribute_on_one_line
            expression: normalize-space(@title)
          - name: parents_attribute
            expression: ../@title
`,
			series: []string{`host_load{host="web01\n      (primary)	spare",host_on_one_line="web01 (primary) spare",own_attribute="1 min",own_attribute_on_one_line="1 min",parents_attribute="rack 7"} 3`}},
	} {
		expectation.run(t)
	}
}

// A row without the cell a rule reads. Under css items the row is an item
// and its value is missing: reported by the rule's error_mode, or left out
// in silence when the rule is not required. An xpath rule selects the cells
// themselves, so a row without one is never selected and nothing is
// reported, whatever the rule says; a cell that is there and empty is a
// missing value to both.
func TestHTMLExpectedOfARowWithoutItsCell(t *testing.T) {
	const page = `<table>
<tr><td class="name">web01</td><td class="load">1</td></tr>
<tr><td class="name">web02</td></tr>
<tr><td class="name">web03</td><td class="load"></td></tr>
</table>`
	for _, expectation := range []htmlExpectation{
		{name: "css reports the row", transform: "css", page: page,
			rules: `      - name: host_load
        items: tr
        expression: td.load
        labels:
          - name: host
            expression: td.name
`,
			series: []string{`host_load{host="web01"} 1`},
			logs:   []string{`ERROR metric extraction failed metric=host_load failures=2 error=metric "host_load" value is missing for item 1: CSS selector "td.load" matched nothing`}},
		{name: "xpath reports the empty cell alone", transform: "xpath", page: page,
			rules: `      - name: host_load
        expression: "//tr/td[@class='load']"
        labels:
          - name: host
            expression: "../td[@class='name']"
`,
			series: []string{`host_load{host="web01"} 1`},
			logs:   []string{`ERROR metric extraction failed metric=host_load failures=1 error=metric "host_load" value is missing for node 1: HTML XPath "//tr/td[@class='load']" selected a node without a value`}},
	} {
		expectation.run(t)
	}
}

// In YAML a # after a blank starts a comment, so a selector with an id after
// a blank, written without quotes, loads as what stands before the blank:
// body #proxy is the selector body, which reads the whole page. In quotes it
// is the selector it was meant to be, and an id written without a blank
// before it, span#proxy, needs none. A selector that is an id alone leaves
// its key with no value, which the load refuses as it does of any key.
func TestHTMLExpectedOfASelectorWithAnIDInYAML(t *testing.T) {
	const page = `<body><span id="proxy">5</span> backends</body>`
	for _, expectation := range []htmlExpectation{
		{name: "without quotes the id is a comment", transform: "css", page: page,
			rules: "      - name: proxy_backends\n        expression: body #proxy\n",
			logs:  []string{`ERROR metric extraction failed metric=proxy_backends failures=1 error=metric "proxy_backends": value "5 backends" is not a number; map text to numbers with value_map`}},
		{name: "in quotes it is the selector", transform: "css", page: page,
			rules:  "      - name: proxy_backends\n        expression: 'body #proxy'\n",
			series: []string{`proxy_backends 5`}},
		{name: "an id without a blank before it needs none", transform: "css", page: page,
			rules:  "      - name: proxy_backends\n        expression: span#proxy\n",
			series: []string{`proxy_backends 5`}},
		{name: "a selector that is an id alone is no expression at all", transform: "css", page: page,
			rules:   "      - name: proxy_backends\n        expression: #proxy\n",
			refused: `line 11: expression has nothing after its colon, which YAML reads as no value at all; write its value, or take the key out`},
	} {
		expectation.run(t)
	}
}

// A run of bytes that are not UTF-8 is repaired as one U+FFFD, however many
// bytes it holds, so the names of a page in windows-1251 that says nothing
// of its encoding, each a word of Cyrillic, are all the same label value:
// the series are duplicates of one and the probe fails in validation, after
// the warning that names response.charset. With response.charset the names
// are read and the probe answers.
func TestHTMLExpectedOfNamesRepairedIntoOne(t *testing.T) {
	logs := testutil.CaptureLogs(t)
	const collectors = `collectors:
  - name: depots
    request:
      type: http
    transform:
      type: css
    metrics: &metrics
      - name: depot_pallets
        items: tr
        expression: td.pallets
        labels:
          - name: city
            expression: td.city
  - name: depots_with_charset
    request:
      type: http
    response:
      charset: windows-1251
    transform:
      type: css
    metrics: *metrics
`
	cfg := htmlCollectors(t, collectors)
	// Варна and Бургас, as windows-1251 writes them.
	page := []byte("<table><tr><td class=city>\xc2\xe0\xf0\xed\xe0</td><td class=pallets>120</td></tr><tr><td class=city>\xc1\xf3\xf0\xe3\xe0\xf1</td><td class=pallets>75</td></tr></table>")
	site := newHTMLSite(t, map[string]htmlPage{"/depots": {"text/html", page}})
	server := htmlServer(cfg)
	sameLogLines(t, logs, nil)

	status, _, failure := probeHTML(t, server, site, "depots", "/depots")
	if want := `collector depots validation failed: duplicate metric series "depot_pallets"`; status != 502 || failure != want {
		t.Errorf("the probe answered %d %s, want 502 %s", status, failure, want)
	}
	sameLogLines(t, logs, []string{
		`WARN label values or help text were not valid UTF-8; the invalid bytes were replaced with U+FFFD. If the target uses another encoding without declaring it, set response.charset values=2 first_metric=depot_pallets`,
		`ERROR probe failed stage=validation error=duplicate metric series "depot_pallets"`,
	})

	status, series, failure := probeHTML(t, server, site, "depots_with_charset", "/depots")
	if status != 200 {
		t.Fatalf("the probe answered %d: %s", status, failure)
	}
	sameSeries(t, series, []string{`depot_pallets{city="Варна"} 120`, `depot_pallets{city="Бургас"} 75`})
	sameLogLines(t, logs, nil)
}
