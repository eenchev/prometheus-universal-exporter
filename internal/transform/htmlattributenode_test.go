package transform

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/PuerkitoBio/goquery"
	"github.com/antchfx/htmlquery"
	"github.com/antchfx/xpath"
	"golang.org/x/net/html"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
)

// A rule over HTML that selects attributes, as `//td/@data-value` does,
// reads its labels from each attribute as the same rule over XML does: the
// attribute's element is its parent (htmlSelected, htmlNavigator).

// htmlSeries are the series of a set as the text exposition writes their
// samples, in the set's order: the labels in name order.
func htmlSeries(set *model.MetricSet) []string {
	if set == nil {
		return nil
	}
	var lines []string
	for _, m := range set.Metrics {
		labels := make([]string, 0, len(m.Labels))
		for name, value := range m.Labels {
			labels = append(labels, name+"="+strconv.Quote(value))
		}
		slices.Sort(labels)
		line := m.Name
		if len(labels) > 0 {
			line += "{" + strings.Join(labels, ",") + "}"
		}
		lines = append(lines, line+" "+strconv.FormatFloat(m.Value, 'g', -1, 64))
	}
	return lines
}

// xpathRuleCollector is an xpath collector of one rule, read by a decoder.
func xpathRuleCollector(decoder string, rule model.MetricRule) model.Collector {
	return model.Collector{Name: "attributes", Decoder: model.DecoderConfig{Type: decoder}, Transform: model.TransformConfig{Type: "xpath"}, Metrics: []model.MetricRule{rule}}
}

// markupContentType is the Content-Type a document of a decoder comes with.
var markupContentType = map[string]string{"xml": "application/xml", "html": "text/html"}

// The exact numbers of a table are in an attribute of each cell, beside the
// text written for people, and the row is named by another attribute of the
// cell and by the id of its row. Over HTML every label that goes through
// the attribute's element was left off: the series had the attribute's name
// alone, were duplicates of one, and the probe answered 502. Over XML the
// same bytes gave the series below, and now both do.
func TestAnHTMLAttributeRuleReadsItsLabelsFromItsElement(t *testing.T) {
	const body = `<table><tr id="r1"><td data-server="web01" data-value="12">12 B</td><td data-server="web02" data-value="31">31 B</td></tr></table>`
	rule := model.MetricRule{Name: "bytes_in", Type: model.GaugeMetricType, Expression: "//td/@data-value", Labels: []model.LabelRule{
		{Name: "server", Expression: "../@data-server"},
		{Name: "row", Expression: "../../@id"},
		{Name: "element", Expression: "name(..)"},
		{Name: "attribute", Expression: "name()"},
	}}
	want := []string{
		`bytes_in{attribute="data-value",element="td",row="r1",server="web01"} 12`,
		`bytes_in{attribute="data-value",element="td",row="r1",server="web02"} 31`,
	}
	for _, decoder := range []string{"html", "xml"} {
		c := xpathRuleCollector(decoder, rule)
		if err := CheckMetricRule(&c, &c.Metrics[0]); err != nil {
			t.Fatal(err)
		}
		set, failures, err := transformWith(context.Background(), t, c, markupContentType[decoder], body)
		if err != nil || len(failures) != 0 {
			t.Fatalf("decoder %s: %v, failures %+v", decoder, err, failures)
		}
		if got := htmlSeries(set); !reflect.DeepEqual(got, want) {
			t.Errorf("decoder %s made\n%s\nwant\n%s", decoder, strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
		// The series are told apart, so the set is one a probe answers.
		if err := set.Validate(c.Limits); err != nil {
			t.Errorf("decoder %s: %v", decoder, err)
		}
	}
}

// attributeRulePage is a document that reads the same as HTML and as XML:
// it writes the html, head, body and tbody elements the HTML parser would
// add, and no blanks between its elements.
const attributeRulePage = `<html><head></head><body><table id="t"><tbody>` +
	`<tr id="r1" class="odd"><td data-server="web01" data-value="12" data-unit=" B ">12 B</td><td data-server="web02" data-value="31">31 B</td></tr>` +
	`<tr id="r2"><td data-server="db01" data-value="7">7 B</td></tr>` +
	`</tbody></table></body></html>`

// attributeRuleLabels are the labels of the first attribute `//td/@data-value`
// selects in attributeRulePage, by their expressions: every form the
// documentation promises of a rule that selects attributes, and what the
// engine makes of the rest. A label that is absent is written as "(none)".
var attributeRuleLabels = map[string]string{
	// The attribute itself: its value, its name.
	".":                "12",
	"text()":           "12",
	"string(.)":        "12",
	"self::node()":     "12",
	"name()":           "data-value",
	"local-name()":     "data-value",
	"@data-value":      "12",
	". * 2":            "24",
	". > 10":           "true",
	"string-length(.)": "2",
	// Its element, and the elements around that one.
	"../@data-server":                       "web01",
	"../@data-unit":                         "B",
	"../@missing":                           "(none)",
	"../../@id":                             "r1",
	"../../@class":                          "odd",
	"../../../../@id":                       "t",
	"name(..)":                              "td",
	"name(../..)":                           "tr",
	"..":                                    "12 B",
	"../text()":                             "12 B",
	"../..":                                 "12 B31 B",
	"normalize-space(..)":                   "12 B",
	"parent::td/@data-server":               "web01",
	"ancestor::tr/@id":                      "r1",
	"ancestor-or-self::*[1]/@data-server":   "web01",
	"count(ancestor::*)":                    "6",
	"count(../@*)":                          "3",
	"concat(../@data-server, '-', .)":       "web01-12",
	"concat(../../@id, '/', name(..))":      "r1/td",
	"../../td[1]":                           "12 B",
	"../../td[2]":                           "31 B",
	"../../td[2]/@data-server":              "web02",
	"../following-sibling::td/@data-server": "web02",
	"../../following-sibling::tr/@id":       "r2",
	// An attribute has no attributes and no children of its own.
	"@data-server":           "(none)",
	"td":                     "(none)",
	"./td":                   "(none)",
	"../td":                  "(none)",
	"*":                      "(none)",
	"node()":                 "(none)",
	"following-sibling::td":  "(none)",
	"preceding-sibling::*":   "(none)",
	"count(following::node)": "0",
}

// firstLabels is the label l of each series of the rule, "(none)" for a
// series without it, or the transform's error.
func firstLabels(t *testing.T, decoder, expression, label, body string) []string {
	t.Helper()
	c := xpathRuleCollector(decoder, model.MetricRule{Name: "m", Type: model.GaugeMetricType, Expression: expression, ValueMap: map[string]float64{"*": 1},
		Required: new(bool), Labels: []model.LabelRule{{Name: "l", Expression: label}}})
	if err := CheckMetricRule(&c, &c.Metrics[0]); err != nil {
		t.Fatalf("%s with the label %s is refused at load: %v", expression, label, err)
	}
	set, err := runBody(t, c, markupContentType[decoder], body)
	if err != nil {
		t.Fatalf("%s with the label %s over %s: %v", expression, label, decoder, err)
	}
	var out []string
	if set != nil {
		for _, m := range set.Metrics {
			if value, ok := m.Labels["l"]; ok {
				out = append(out, value)
			} else {
				out = append(out, "(none)")
			}
		}
	}
	return out
}

// Every label form reads at a selected attribute of an HTML document what
// it reads at the same attribute of an XML one, for each way a rule selects
// attributes — by element and name, by name, all of them, by a predicate on
// the name, attributes of two kinds, and attributes beside their elements —
// and the labels of the first attribute are those the table says.
func TestHTMLAttributeRuleLabelsAreThoseOfXML(t *testing.T) {
	for label, want := range attributeRuleLabels {
		if got := firstLabels(t, "html", "//td/@data-value", label, attributeRulePage); len(got) != 3 || got[0] != want {
			t.Errorf("the label %s reads %q, want %q first of three", label, got, want)
		}
	}
	compared := 0
	for _, expression := range []string{
		"//td/@data-value", "//@data-value", "//@*", "//td/@*[starts-with(name(), 'data-v')]", "//td/@*[. > 10]",
		"//tr/@id | //td/@data-value", "//td | //td/@data-value", "/html/body/table/@id", "//tr[2]/td/@*",
	} {
		for label := range attributeRuleLabels {
			over := map[string][]string{}
			for _, decoder := range []string{"html", "xml"} {
				over[decoder] = firstLabels(t, decoder, expression, label, attributeRulePage)
			}
			if len(over["html"]) == 0 || !reflect.DeepEqual(over["html"], over["xml"]) {
				t.Errorf("%s with the label %s reads over HTML\n%q\nand over XML\n%q", expression, label, over["html"], over["xml"])
			}
			compared += len(over["html"])
		}
	}
	t.Logf("%d labels compared", compared)
}

// A selected attribute has no nodes beneath it, over HTML as over XML: a
// label that asks for its text node or for a child gives nothing, and a
// function of one works on nothing. Before an attribute of an HTML document
// had its element for a parent these gave the value, the attribute being
// handed over as an element holding its value as text; the value is `.`,
// and the bare label `text()`, which alone is read as the value.
func TestASelectedAttributeHasNoChildNodes(t *testing.T) {
	for label, want := range map[string]string{
		"./text()":                "(none)",
		"text()[1]":               "(none)",
		"node()":                  "(none)",
		"self::*":                 "(none)",
		"descendant::text()":      "(none)",
		"normalize-space(text())": "(none)",
		"string(text())":          "(none)",
		"substring(text(), 1, 2)": "(none)",
		"contains(text(), '1')":   "false",
		"count(text())":           "0",
		"count(node())":           "0",
		// The value is the attribute itself.
		".":                      "12",
		"text()":                 "12",
		"normalize-space(.)":     "12",
		"substring(., 1, 1)":     "1",
		"contains(., '1')":       "true",
		"concat(name(), '=', .)": "data-value=12",
	} {
		for _, decoder := range []string{"html", "xml"} {
			if got := firstLabels(t, decoder, "//td/@data-value", label, attributeRulePage); len(got) != 3 || got[0] != want {
				t.Errorf("over %s the label %s reads %q, want %q first of three", decoder, label, got, want)
			}
		}
	}
}

// An attribute of the element is read by its name as the page writes it,
// which over HTML is any name an attribute can have — a colon in it, a
// digit or an @ first — on the element and on the elements around it.
func TestAnHTMLAttributeRuleReadsNamesOnlyHTMLHas(t *testing.T) {
	const page = `<html xml:lang="en"><body><ul id="hosts" og:type="list" v-on:click="pick">
<li data-id="h1" data-load="0.5" og:type="host" 2x="two" :href="/h1" @click="open" x-on:click.prevent="p">alpha</li>
<li data-id="h2" data-load="1.5" og:type="vm">beta</li>
</ul></body></html>`
	rule := model.MetricRule{Name: "host_load", Type: model.GaugeMetricType, Expression: "//li/@data-load", Labels: []model.LabelRule{
		{Name: "host", Expression: "../@data-id", Required: true},
		{Name: "kind", Expression: "../@og:type"},
		{Name: "density", Expression: "../@2x"},
		{Name: "href", Expression: "../@:href"},
		{Name: "click", Expression: "../@@click"},
		{Name: "prevented", Expression: "../@x-on:click.prevent"},
		{Name: "list", Expression: "../../@og:type"},
		{Name: "list_click", Expression: "../../@v-on:click"},
		{Name: "lang", Expression: "../../../../@xml:lang"},
		{Name: "name", Expression: ".."},
	}}
	c := xpathRuleCollector("html", rule)
	if err := CheckMetricRule(&c, &c.Metrics[0]); err != nil {
		t.Fatal(err)
	}
	set, failures, err := transformWith(context.Background(), t, c, "text/html", page)
	if err != nil || len(failures) != 0 {
		t.Fatalf("%v, failures %+v", err, failures)
	}
	want := []string{
		`host_load{click="open",density="two",host="h1",href="/h1",kind="host",lang="en",list="list",list_click="pick",name="alpha",prevented="p"} 0.5`,
		`host_load{host="h2",kind="vm",lang="en",list="list",list_click="pick",name="beta"} 1.5`,
	}
	if got := htmlSeries(set); !reflect.DeepEqual(got, want) {
		t.Errorf("the rule made\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// The moment a page means is in the datetime attribute of its time
// elements, beside the words written for people. One rule reads every
// row's, as a time, and names each by a cell of its row: a row whose time
// element has no datetime is not selected.
func TestAnHTMLTimeElementsDatetimeIsReadWithItsRow(t *testing.T) {
	const page = `<table id="jobs"><tbody>
<tr id="j1"><td class="job">backup</td><td class="last"><time datetime="2026-10-03T02:00:07Z">3 Oct 2026, 02:00</time></td></tr>
<tr id="j2"><td class="job">rotate-logs</td><td class="last"><time datetime="2026-10-03T05:30:00+03:00">today 05:30</time></td></tr>
<tr id="j3"><td class="job">prune</td><td class="last"><time>never</time></td></tr>
</tbody></table>`
	rule := model.MetricRule{Name: "job_last_run_timestamp_seconds", Type: model.GaugeMetricType, Expression: "//table[@id='jobs']//time/@datetime", TimeFormat: "rfc3339",
		Labels: []model.LabelRule{
			{Name: "job", Expression: "../../../td[@class='job']", Required: true},
			{Name: "row", Expression: "ancestor::tr/@id"},
			{Name: "words", Expression: ".."},
		}}
	c := xpathRuleCollector("html", rule)
	if err := CheckMetricRule(&c, &c.Metrics[0]); err != nil {
		t.Fatal(err)
	}
	set, failures, err := transformWith(context.Background(), t, c, "text/html", page)
	if err != nil || len(failures) != 0 {
		t.Fatalf("%v, failures %+v", err, failures)
	}
	want := []string{
		`job_last_run_timestamp_seconds{job="backup",row="j1",words="3 Oct 2026, 02:00"} 1.790992807e+09`,
		`job_last_run_timestamp_seconds{job="rotate-logs",row="j2",words="today 05:30"} 1.7909946e+09`,
	}
	if got := htmlSeries(set); !reflect.DeepEqual(got, want) {
		t.Errorf("the rule made\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// A value computed from each attribute, and a required label read through
// its element: a rule's scale, its value_map and its error mode work on a
// selected attribute as on any node.
func TestAnHTMLAttributeRuleKeepsItsRulesOptions(t *testing.T) {
	const page = `<ul><li id="a" data-state="up" data-ms="250">a</li><li data-state="down" data-ms="40">b</li><li id="c" data-state="up" data-ms="slow">c</li></ul>`
	thousandth := 0.001
	c := model.Collector{Name: "attributes", Decoder: model.DecoderConfig{Type: "html"}, Transform: model.TransformConfig{Type: "xpath"}, Metrics: []model.MetricRule{
		{Name: "up", Type: model.GaugeMetricType, Expression: "//li/@data-state", ValueMap: map[string]float64{"up": 1, "down": 0}, ErrorMode: model.ErrorModeLog,
			Labels: []model.LabelRule{{Name: "id", Expression: "../@id", Required: true}}},
		{Name: "seconds", Type: model.GaugeMetricType, Expression: "//li/@data-ms", Scale: &thousandth, ErrorMode: model.ErrorModeLog,
			Labels: []model.LabelRule{{Name: "item", Expression: ".."}}},
	}}
	for i := range c.Metrics {
		if err := CheckMetricRule(&c, &c.Metrics[i]); err != nil {
			t.Fatal(err)
		}
	}
	set, failures, err := transformWith(context.Background(), t, c, "text/html", page)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{`up{id="a"} 1`, `up{id="c"} 1`, `seconds{item="a"} 0.25`, `seconds{item="b"} 0.04`}
	if got := htmlSeries(set); !reflect.DeepEqual(got, want) {
		t.Errorf("the rules made\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	// The item without an id fails its required label, and the one whose
	// attribute is no number its value.
	if len(failures) != 2 || failures[0].Metric != "up" || failures[0].Failures != 1 || failures[1].Metric != "seconds" || failures[1].Failures != 1 ||
		!strings.Contains(failures[0].First.Error(), `label "id" is missing`) || !strings.Contains(failures[1].First.Error(), `value "slow" is not a number`) {
		t.Errorf("failures %+v", failures)
	}
}

// Attributes that are unusual are read all the same, and none ends the
// scrape with a panic: those of html and body, an attribute written twice,
// of which the parser keeps the first, attributes HTML gives a namespace
// inside svg and math, where two of an element can have one name, and an
// attribute without a value.
func TestUnusualHTMLAttributesAreSelectedWithoutAPanic(t *testing.T) {
	const page = `<html lang="en" data-build="7"><body class="dark" data-build="8">
<i id="twice" w="5" w="6" W="7" hidden>x</i>
<svg id="s" viewBox="0 0 1 1"><a id="sa" xlink:href="#h" href="#g" xml:lang="de"><text>t</text></a></svg>
<math id="m"><mi xlink:href="#x" definitionURL="u">x</mi></math>
</body></html>`
	labels := []model.LabelRule{
		{Name: "self", Expression: "."},
		{Name: "text", Expression: "text()"},
		{Name: "name", Expression: "name()"},
		{Name: "of", Expression: "name(..)"},
		{Name: "id", Expression: "../@id"},
		{Name: "link", Expression: "../@xlink:href"},
		{Name: "around", Expression: "../../@id"},
		{Name: "attributes", Expression: "count(../@*)"},
		{Name: "both", Expression: "concat(name(..), '/', name(), '=', .)"},
	}
	for expression, want := range map[string][]string{
		"//@data-build": {
			`m{attributes="2",both="html/data-build=7",name="data-build",of="html",self="7",text="7"} 1`,
			`m{attributes="2",both="body/data-build=8",name="data-build",of="body",self="8",text="8"} 1`,
		},
		"/html/@lang | /html/body/@class": {
			`m{attributes="2",both="html/lang=en",name="lang",of="html",self="en",text="en"} 1`,
			`m{attributes="2",both="body/class=dark",name="class",of="body",self="dark",text="dark"} 1`,
		},
		// The second w and the W, which is w too, are not in the document.
		"//i/@w": {
			`m{attributes="3",both="i/w=5",id="twice",name="w",of="i",self="5",text="5"} 1`,
		},
		// Two attributes of the svg link are named href to the engine, which
		// knows no prefix: each is read with its own value.
		"//a/@href": {
			`m{around="s",attributes="4",both="a/href=#h",id="sa",link="#h",name="href",of="a",self="#h",text="#h"} 1`,
			`m{around="s",attributes="4",both="a/href=#g",id="sa",link="#h",name="href",of="a",self="#g",text="#g"} 1`,
		},
		// The parser writes the attributes of svg and math in the case
		// their languages have them in.
		"//svg/@viewBox | //a/@lang": {
			`m{attributes="2",both="svg/viewBox=0 0 1 1",id="s",name="viewBox",of="svg",self="0 0 1 1",text="0 0 1 1"} 1`,
			`m{around="s",attributes="4",both="a/lang=de",id="sa",link="#h",name="lang",of="a",self="de",text="de"} 1`,
		},
		"//mi/@*": {
			`m{around="m",attributes="2",both="mi/href=#x",link="#x",name="href",of="mi",self="#x",text="#x"} 1`,
			`m{around="m",attributes="2",both="mi/definitionURL=u",link="#x",name="definitionURL",of="mi",self="u",text="u"} 1`,
		},
	} {
		c := xpathRuleCollector("html", model.MetricRule{Name: "m", Type: model.GaugeMetricType, Expression: expression, ValueMap: map[string]float64{"*": 1}, Labels: labels})
		set, failures, err := transformWith(context.Background(), t, c, "text/html", page)
		if err != nil || len(failures) != 0 {
			t.Fatalf("%s: %v, failures %+v", expression, err, failures)
		}
		if got := htmlSeries(set); !reflect.DeepEqual(got, want) {
			t.Errorf("%s made\n%s\nwant\n%s", expression, strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
	}
	// Every attribute of the page at once, the one without a value among
	// them, which has no value to make a series of.
	c := xpathRuleCollector("html", model.MetricRule{Name: "m", Type: model.GaugeMetricType, Expression: "//@*", ValueMap: map[string]float64{"*": 1}, Required: new(bool), Labels: labels})
	set, err := runBody(t, c, "text/html", page)
	if err != nil || len(set.Metrics) != 15 {
		t.Fatalf("//@* made %d series, %v:\n%s", len(set.Metrics), err, strings.Join(htmlSeries(set), "\n"))
	}
}

// mainsHTMLNodes is htmlNodes as it was while a selected attribute had no
// parent and the text of a node was that of everything beneath it: the
// oracle of what the change left as it was.
var mainsHTMLNodes = func() xpathNodes[*html.Node] {
	nodes := htmlNodes
	nodes.all = htmlquery.QuerySelectorAll
	nodes.one = func(node *html.Node, e *xpath.Expr) (*html.Node, bool) {
		found := htmlquery.QuerySelector(node, e)
		return found, found != nil
	}
	nodes.attr = func(node *html.Node, name string) string {
		if value := htmlquery.SelectAttr(node, name); value != "" {
			return value
		}
		value, _ := htmlAttribute(node, name)
		return value
	}
	nodes.text = htmlquery.InnerText
	nodes.navigate = func(node *html.Node) xpath.NodeNavigator { return htmlquery.CreateXPathNavigator(node) }
	nodes.apart = nil
	nodes.element = func(node *html.Node) bool { return node.Type == html.ElementNode }
	nodes.selectedAttribute = func(*html.Node) (string, bool) { return "", false }
	return nodes
}()

// htmlFixturePages are the pages of testdata/html, by file name, and the
// pages the tests of XPath labels read.
func htmlFixturePages(t *testing.T) map[string]string {
	t.Helper()
	pages := map[string]string{"attributePage": attributePage, "attributeRulePage": attributeRulePage}
	for i, page := range xpathLabelPages {
		pages["xpathLabelPages "+strconv.Itoa(i)] = page
	}
	files, err := filepath.Glob("../../testdata/html/*.*html")
	if err != nil || len(files) < 10 {
		t.Fatalf("%d fixtures, %v", len(files), err)
	}
	for _, file := range files {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		pages[filepath.Base(file)] = string(raw)
	}
	return pages
}

// withoutScriptsAndStyles parses a page as main did, with the parser alone,
// and takes out of it what the text of an element no longer holds: every
// script and style element, and the content of every template.
func withoutScriptsAndStyles(t *testing.T, page string) *html.Node {
	t.Helper()
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(page))
	if err != nil {
		t.Fatal(err)
	}
	doc.Find("script, style").Remove()
	doc.Find("template").Each(func(_ int, template *goquery.Selection) {
		if template.Nodes[0].Namespace == "" {
			template.Contents().Remove()
		}
	})
	return doc.Nodes[0]
}

// differentialXPathRules are rules of every kind over one expression: the
// value read as a number and failing quietly where it is none, and mapped
// to 1 whatever it is, each with labels of the shapes that are walked and
// of those the engine reads.
func differentialXPathRules(expression string, labels []string) []model.MetricRule {
	var rules []model.MetricRule
	for _, valueMap := range []map[string]float64{nil, {"*": 1}} {
		rule := model.MetricRule{Name: "m", Type: model.GaugeMetricType, Expression: expression, ValueMap: valueMap, Required: new(bool), ErrorMode: model.ErrorModeIgnore}
		for i, label := range labels {
			rule.Labels = append(rule.Labels, model.LabelRule{Name: "l" + strconv.Itoa(i), Expression: label})
		}
		rules = append(rules, rule)
	}
	return rules
}

// transformedHTML is what the XPath transform makes of a parsed page with
// rules, read through nodes.
func transformedHTML(t *testing.T, root *html.Node, nodes xpathNodes[*html.Node], rules []model.MetricRule) []string {
	t.Helper()
	c := model.Collector{Name: "differential"}
	set, err := transformXPathNodes(withSeriesBudget(context.Background(), 1<<20), root, nodes, rules, &c, nil)
	if err != nil {
		t.Fatal(err)
	}
	return htmlSeries(set)
}

// differentialLabels are label expressions of every shape: the node's
// attributes, the walks, and what the engine evaluates.
var differentialLabels = []string{
	"@id", "@class", "@data-id", "@xlink:href", ".", "text()", "../@id", "../@class", "../../@id", "../@og:type", "td", "./span", "../td", "../../tr",
	"name()", "name(..)", "..", "../td[1]", "../@*", "@*", "count(../*)", "normalize-space(.)", "string(../@id)", "concat(name(), '-', @id)",
	"ancestor::table/@id", "following-sibling::*[1]", "preceding-sibling::td[1]", "*[1]", "node()", "//title", "string-length(.)",
}

// A rule that selects elements or text reads what it read before a selected
// attribute had a parent: over every fixture page, with scripts, styles and
// template content taken out so that the text of a node is what it was too,
// rules of every kind make the same series, label for label, through the
// accessors as they are and as they were on main. Under the race detector
// each page is read by a quarter of the expressions and each expression
// reads a quarter of the pages (pairTaken).
func TestElementAndTextRulesOverHTMLReadWhatTheyRead(t *testing.T) {
	expressions := []string{
		"//td", "//tr", "//tr/td[1]", "//li", "//span", "//p", "//div", "//dt", "//a", "//time", "//*[@id]", "//*[@class]", "//*", "//h1 | //h2 | //title",
		"//td/text()", "//li/text()", "//body//text()", "//text()[normalize-space()]", "//node()", "//comment()", "/html", "/", "//svg//*", "//td[not(*)]",
		"count(//td)", "sum(//td[@class='n'])", "string(//title)", "//table//tr[td][1]/td | //table//th",
	}
	compared, pages := 0, htmlFixturePages(t)
	place := places(pages)
	for name, page := range pages {
		root := withoutScriptsAndStyles(t, page)
		for at, expression := range expressions {
			if !pairTaken(place[name], at, 4) {
				continue
			}
			rules := differentialXPathRules(expression, differentialLabels)
			got, main := transformedHTML(t, root, htmlNodes, rules), transformedHTML(t, root, mainsHTMLNodes, rules)
			if !reflect.DeepEqual(got, main) {
				t.Fatalf("%s over %s: %d series, and main made %d; the first that differs:\n%s", expression, name, len(got), len(main), firstDifference(got, main))
			}
			compared += len(got)
		}
	}
	if compared < alloctest.UnlessRaced(5000, 1250) {
		t.Fatalf("only %d series compared over %d pages", compared, len(pages))
	}
	t.Logf("%d series compared over %d pages", compared, len(pages))
}

// firstDifference is the first line two lists do not share, of each.
func firstDifference(got, want []string) string {
	for i := range max(len(got), len(want)) {
		a, b := "(no series)", "(no series)"
		if i < len(got) {
			a = got[i]
		}
		if i < len(want) {
			b = want[i]
		}
		if a != b {
			return "series " + strconv.Itoa(i) + "\n got  " + a + "\n main " + b
		}
	}
	return "none"
}

// A rule that selects attributes makes the series it made, each with the
// value it had, and the labels that never went through the attribute's
// element read what they read: its own value and name, and the attribute
// asked for by its own name. Over every fixture page, as on main. What read
// the attribute as the element htmlquery made of it, which held the value
// as a text node — the label node(), which gave the value — reads it as an
// attribute, which has no nodes beneath it, as over XML: only text() is the
// value still (TestTheLabelTextOfASelectedAttributeIsItsValue).
func TestAttributeRulesOverHTMLKeepTheirSeriesAndOwnLabels(t *testing.T) {
	labels := []string{".", "text()", "name()", "string(.)", "local-name()", "@id", "@class", "@href", "@data-value", "td", "./span", "*", "string-length(.)"}
	compared, told := 0, 0
	for name, page := range htmlFixturePages(t) {
		root := withoutScriptsAndStyles(t, page)
		for _, expression := range []string{"//@*", "//@id", "//@class", "//td/@*", "//a/@href", "//*[@id]/@*[name() != 'id']", "//td/@class | //tr/@class"} {
			rules := differentialXPathRules(expression, labels)
			got, main := transformedHTML(t, root, htmlNodes, rules), transformedHTML(t, root, mainsHTMLNodes, rules)
			if !reflect.DeepEqual(got, main) {
				t.Fatalf("%s over %s: %d series, and main made %d; the first that differs:\n%s", expression, name, len(got), len(main), firstDifference(got, main))
			}
			compared += len(got)
			// With the element's name for a label the series now differ
			// where the label was absent on main.
			through := differentialXPathRules(expression, []string{"name(..)"})
			now, before := transformedHTML(t, root, htmlNodes, through), transformedHTML(t, root, mainsHTMLNodes, through)
			if len(now) != len(before) {
				t.Fatalf("%s over %s: %d series, and main made %d", expression, name, len(now), len(before))
			}
			for i := range now {
				if strings.Contains(before[i], "l0=") || !strings.Contains(now[i], `{l0="`) {
					t.Fatalf("%s over %s: series %d is %s, and on main %s", expression, name, i, now[i], before[i])
				}
				told++
			}
		}
	}
	if compared < 2000 || told < 1000 {
		t.Fatalf("only %d series compared, and %d told apart by their element", compared, told)
	}
	t.Logf("%d series compared, and %d told apart by their element", compared, told)
}
