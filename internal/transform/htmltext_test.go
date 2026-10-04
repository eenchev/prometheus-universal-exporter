package transform

import (
	"context"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/PuerkitoBio/goquery"
	"github.com/antchfx/htmlquery"
	"golang.org/x/net/html"

	"github.com/eenchev/prometheus-universal-exporter/internal/decode"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The text of an HTML element, as the css and xpath transforms read it, is
// without what stands inside a script or a style beneath it (htmlText), and
// the content of a template is no part of the document (decode.ParseHTML).

// htmlRulesCollector is a collector of rules over an HTML page, css or
// xpath, each a gauge unless it says otherwise.
func htmlRulesCollector(transform string, rules ...model.MetricRule) model.Collector {
	c := model.Collector{Name: "page", Decoder: model.DecoderConfig{Type: "html"}, Transform: model.TransformConfig{Type: transform}, Metrics: rules}
	for i := range c.Metrics {
		if c.Metrics[i].Type == "" {
			c.Metrics[i].Type = model.GaugeMetricType
		}
	}
	return c
}

// readPage transforms an HTML page with the collector's rules, which must
// load: the series, and each rule's failures as "<metric>: <n> failed, first
// <error>".
func readPage(t *testing.T, c model.Collector, contentType, page string) (series, failed []string) {
	t.Helper()
	for i := range c.Metrics {
		if err := CheckMetricRule(&c, &c.Metrics[i]); err != nil {
			t.Fatal(err)
		}
	}
	set, failures, err := transformWith(context.Background(), t, c, contentType, page)
	if err != nil {
		t.Fatal(err)
	}
	for _, failure := range failures {
		failed = append(failed, failure.Metric+": "+strconv.FormatUint(failure.Failures, 10)+" failed, first "+failure.First.Error())
	}
	return htmlSeries(set), failed
}

// scriptedTable is a table as a page a script fills writes it: a template
// row with placeholders, a row as written, one whose cell has an inline
// script after its number, and one whose cell has a style before it.
const scriptedTable = `<table><template><tr><td class="name">{{name}}</td><td class="n">{{n}}</td></tr></template>` +
	`<tr><td class="name">a</td><td class="n">5</td></tr>` +
	`<tr><td class="name">b</td><td class="n">6<script>var n = 6;</script></td></tr>` +
	`<tr><td class="name">c</td><td class="n"><style>.n{color:red}</style>7</td></tr></table>`

// The rows of such a table are read as a browser shows them. The text of
// the cells with a script and a style was "6var n = 6;" and ".n{color:red}7",
// no numbers, and the template's row, which is no row of the page, was an
// item whose value is "{{n}}": of four items one made a series, and three
// failures were logged. css and xpath alike.
func TestScriptStyleAndTemplateContentAreNotAnElementsText(t *testing.T) {
	want := []string{`n{name="a"} 5`, `n{name="b"} 6`, `n{name="c"} 7`}
	for transform, rule := range map[string]model.MetricRule{
		"css":   {Name: "n", Items: "table tr", Expression: "td.n", Labels: []model.LabelRule{{Name: "name", Expression: "td.name"}}},
		"xpath": {Name: "n", Expression: "//table//td[@class='n']", Labels: []model.LabelRule{{Name: "name", Expression: "../td[@class='name']"}}},
	} {
		rule.ErrorMode = model.ErrorModeLog
		series, failed := readPage(t, htmlRulesCollector(transform, rule), "text/html", scriptedTable)
		if !reflect.DeepEqual(series, want) || len(failed) != 0 {
			t.Errorf("%s made\n%s\nwith the failures %q, want\n%s\nand none", transform, strings.Join(series, "\n"), failed, strings.Join(want, "\n"))
		}
	}
}

// notTextPage holds a script and a style in every place text is read from:
// in the value's element, before, after and deep inside it, alone in it, in
// a label's element, and in the element of an item; and scripts and styles
// that are themselves what a rule selects.
const notTextPage = `<html><head><title>farm</title><style>body { margin: 0 }</style>
<script id="state" type="application/json">{"queue": 17}</script><script id="count">42</script></head><body>
<ul id="agents">
<li id="a1"><span class="name">linux<style>.name { color: red }</style></span><b class="n">12<script>track(12);</script></b></li>
<li id="a2"><span class="name"><script>var name = "x";</script>mac</span><b class="n"><style>.n::after { content: "999" }</style>3</b></li>
<li id="a3"><span class="name">win</span><b class="n"><i>4<script>/* deep */</script></i><i>5</i></b></li>
<li id="a4"><span class="name">bsd</span><b class="n"><script>document.write(8)</script></b></li>
</ul>
<p id="total">Total: <span>24</span><script>total = 24;</script></p>
<svg id="gauge"><style>text { fill: red }</style><text class="reading">63</text><script>draw(63)</script></svg>
</body></html>`

// Wherever the transforms read the text of an element — a css value, a css
// label, the cells of an item, an xpath value and an xpath label that is an
// element — text inside a script or a style beneath it is left out, in an
// svg too, and an element that holds nothing else has no value. Blanks are
// trimmed and everything else is read as it was.
func TestTheTextOfAnHTMLElementLeavesOutScriptsAndStyles(t *testing.T) {
	notRequired := new(bool)
	for name, tc := range map[string]struct {
		transform string
		rules     []model.MetricRule
		series    []string
		failed    []string
	}{
		"css items: the value and the label of each": {
			transform: "css",
			rules: []model.MetricRule{{Name: "jobs", Items: "#agents li", Expression: "b.n", ErrorMode: model.ErrorModeLog,
				Labels: []model.LabelRule{{Name: "agent", Expression: "span.name"}}}},
			series: []string{`jobs{agent="linux"} 12`, `jobs{agent="mac"} 3`, `jobs{agent="win"} 45`},
			// The cell that holds a script alone has no value.
			failed: []string{`jobs: 1 failed, first metric "jobs" value is missing for item 3: CSS selector "b.n" matched an element without a value`},
		},
		"css items: a missing value of a rule that is not required": {
			transform: "css",
			rules:     []model.MetricRule{{Name: "jobs", Items: "#agents li", Expression: "b.n", Required: notRequired, Labels: []model.LabelRule{{Name: "agent", Expression: "span.name"}}}},
			series:    []string{`jobs{agent="linux"} 12`, `jobs{agent="mac"} 3`, `jobs{agent="win"} 45`},
		},
		"css: one element, around a script, and inside an svg": {
			transform: "css",
			rules: []model.MetricRule{
				{Name: "first", Expression: "#a1 b"},
				{Name: "total", Expression: "#total span"},
				{Name: "reading", Expression: "#gauge"},
				{Name: "title", Expression: "head", ValueMap: map[string]float64{"farm": 1}},
			},
			series: []string{`first 12`, `total 24`, `reading 63`, `title 1`},
		},
		"xpath: the value and the labels of each": {
			transform: "xpath",
			rules: []model.MetricRule{{Name: "jobs", Expression: "//ul[@id='agents']/li/b", ErrorMode: model.ErrorModeLog, Labels: []model.LabelRule{
				{Name: "agent", Expression: "../span"},
				{Name: "id", Expression: "../@id"},
				{Name: "self", Expression: "."},
				{Name: "row", Expression: ".."},
				{Name: "first", Expression: "text()"},
			}}},
			series: []string{
				`jobs{agent="linux",first="12",id="a1",row="linux12",self="12"} 12`,
				`jobs{agent="mac",first="3",id="a2",row="mac3",self="3"} 3`,
				`jobs{agent="win",id="a3",row="win45",self="45"} 45`,
			},
			failed: []string{`jobs: 1 failed, first metric "jobs" value is missing for node 3: HTML XPath "//ul[@id='agents']/li/b" selected a node without a value`},
		},
		"xpath: nested elements selected together": {
			transform: "xpath",
			rules: []model.MetricRule{
				{Name: "n", Expression: "//li[@id='a3'] | //li[@id='a3']//*", ValueMap: map[string]float64{"*": 1}, Required: notRequired, Labels: []model.LabelRule{{Name: "text", Expression: "."}}},
				{Name: "reading", Expression: "//svg"},
				{Name: "total", Expression: "//p[@id='total']", ValueMap: map[string]float64{"Total: 24": 24}},
			},
			// The script of the third agent is selected too: its text is
			// its own, and no part of the elements around it.
			series: []string{
				`n{text="win45"} 1`, `n{text="win"} 1`, `n{text="45"} 1`, `n{text="4"} 1`, `n{text="/* deep */"} 1`, `n{text="5"} 1`,
				`reading 63`, `total 24`,
			},
		},
	} {
		series, failed := readPage(t, htmlRulesCollector(tc.transform, tc.rules...), "text/html", notTextPage)
		if !reflect.DeepEqual(series, tc.series) || !reflect.DeepEqual(failed, tc.failed) {
			t.Errorf("%s made\n%s\nwith the failures %q, want\n%s\nwith %q", name, strings.Join(series, "\n"), failed, strings.Join(tc.series, "\n"), tc.failed)
		}
	}
}

// A rule that selects a script or a style itself, or a text node inside
// one, reads its text: the number a page keeps in a script, the state it
// was rendered from. Only the elements around a script leave its text out.
func TestASelectedScriptOrStyleIsReadItself(t *testing.T) {
	for name, tc := range map[string]struct {
		transform string
		rules     []model.MetricRule
		series    []string
	}{
		"css": {
			transform: "css",
			rules: []model.MetricRule{
				{Name: "count", Expression: "script#count"},
				{Name: "state", Expression: "#state", ValueMap: map[string]float64{`{"queue": 17}`: 17}},
				{Name: "style", Expression: "head style", ValueMap: map[string]float64{"body { margin: 0 }": 1}},
				{Name: "svg_style", Expression: "svg style", ValueMap: map[string]float64{"text { fill: red }": 1}},
				{Name: "scripts", Items: "head script[id]", Expression: "script", ValueMap: map[string]float64{"*": 1}, Required: new(bool)},
				{Name: "item", Items: "#a1", Expression: "script", ValueMap: map[string]float64{"*": 1},
					Labels: []model.LabelRule{{Name: "script", Expression: "script"}, {Name: "style", Expression: "style"}}},
			},
			series: []string{`count 42`, `state 17`, `style 1`, `svg_style 1`, `item{script="track(12);",style=".name { color: red }"} 1`},
		},
		"xpath": {
			transform: "xpath",
			rules: []model.MetricRule{
				{Name: "count", Expression: "//script[@id='count']"},
				{Name: "count_text", Expression: "//script[@id='count']/text()"},
				{Name: "state", Expression: "//script[@type='application/json']", ValueMap: map[string]float64{`{"queue": 17}`: 17},
					Labels: []model.LabelRule{{Name: "self", Expression: "."}, {Name: "text", Expression: "text()"}}},
				{Name: "style", Expression: "//head/style | //svg/style", ValueMap: map[string]float64{"*": 1}, Labels: []model.LabelRule{{Name: "text", Expression: "."}}},
				{Name: "item", Expression: "//li[@id='a1']/b", Labels: []model.LabelRule{{Name: "script", Expression: "script"}, {Name: "style", Expression: "../span/style"}}},
				// The engine's own reading of text is XPath's, with the
				// script's: a function written in an expression.
				{Name: "queue", Expression: "number(substring-before(substring-after(//script[@id='state'], '\"queue\": '), '}'))"},
			},
			series: []string{
				`count 42`, `count_text 42`, `state{self="{\"queue\": 17}",text="{\"queue\": 17}"} 17`,
				`style{text="body { margin: 0 }"} 1`, `style{text="text { fill: red }"} 1`,
				`item{script="track(12);",style=".name { color: red }"} 12`, `queue 17`,
			},
		},
	} {
		series, failed := readPage(t, htmlRulesCollector(tc.transform, tc.rules...), "text/html", notTextPage)
		if !reflect.DeepEqual(series, tc.series) || len(failed) != 0 {
			t.Errorf("%s made\n%s\nwith the failures %q, want\n%s\nand none", name, strings.Join(series, "\n"), failed, strings.Join(tc.series, "\n"))
		}
	}
}

// What an expression itself asks of the selector library or of the XPath
// engine is theirs, and reads the text of everything beneath an element, a
// script's included: the string value XPath's functions and comparisons
// work on, and what :contains() looks in. The exporter's own reading of the
// node a rule or a label selects is what leaves scripts and styles out.
func TestXPathFunctionsAndContainsReadTextAsTheirLibrariesDo(t *testing.T) {
	series, failed := readPage(t, htmlRulesCollector("xpath",
		model.MetricRule{Name: "string", Expression: "string-length(string(//li[@id='a1']/b))"},
		model.MetricRule{Name: "selected", Expression: "//li[@id='a1']/b", Labels: []model.LabelRule{
			{Name: "normalized", Expression: "normalize-space(.)"},
			{Name: "read", Expression: "."},
		}},
		model.MetricRule{Name: "compared", Expression: "count(//li[b = '12'])"},
		model.MetricRule{Name: "compared_whole", Expression: "count(//li[b = '12track(12);'])"},
	), "text/html", notTextPage)
	want := []string{`string 12`, `selected{normalized="12track(12);",read="12"} 12`, `compared 0`, `compared_whole 1`}
	if !reflect.DeepEqual(series, want) || len(failed) != 0 {
		t.Errorf("xpath made\n%s\nwith the failures %q, want\n%s", strings.Join(series, "\n"), failed, strings.Join(want, "\n"))
	}
	series, failed = readPage(t, htmlRulesCollector("css",
		model.MetricRule{Name: "contains", Items: `#agents li:contains("track")`, Expression: "b.n", Labels: []model.LabelRule{{Name: "agent", Expression: "span.name"}}},
	), "text/html", notTextPage)
	if want := []string{`contains{agent="linux"} 12`}; !reflect.DeepEqual(series, want) || len(failed) != 0 {
		t.Errorf("css made\n%s\nwith the failures %q, want\n%s", strings.Join(series, "\n"), failed, strings.Join(want, "\n"))
	}
}

// templatePage has templates where pages have them: in the body, in a table
// before its rows, one inside another, with a script in one, and an element
// of that name inside an svg, which is no HTML template.
const templatePage = `<html><body>
<div id="list"><template id="item"><p class="item"><span class="n">{{count}}</span></p><template><p class="item"><span class="n">0</span></p></template><script>var t = 1;</script></template><p class="item"><span class="n">1</span></p><p class="item"><span class="n">2</span></p></div>
<table id="rows"><template><tr><td>{{n}}</td></tr></template><tr><td>3</td></tr></table>
<svg id="chart"><template><text>4</text></template></svg>
</body></html>`

// The content of a template is not in the document: selectors, items, XPath
// expressions and labels do not match it, and it is no part of the text of
// an element around the template. The template element itself is there,
// empty. An element named template inside an svg is an element of the svg,
// and keeps its content.
func TestTheContentOfATemplateIsNotInTheDocument(t *testing.T) {
	notRequired := new(bool)
	for name, tc := range map[string]struct {
		transform string
		rules     []model.MetricRule
		series    []string
		failed    []string
	}{
		"css": {
			transform: "css",
			rules: []model.MetricRule{
				{Name: "items", Items: "p.item", Expression: "span.n", ErrorMode: model.ErrorModeLog},
				{Name: "rows", Items: "#rows tr", Expression: "td", ErrorMode: model.ErrorModeLog},
				{Name: "list", Expression: "#list"},
				{Name: "inside", Items: "template *", Expression: "*", Required: notRequired},
				{Name: "template", Expression: "template#item", ErrorMode: model.ErrorModeLog},
				// Of the three templates the one of the svg has a child.
				{Name: "templates", Items: "template", Expression: "*", ValueMap: map[string]float64{"*": 1}, Required: notRequired},
				{Name: "chart", Expression: "#chart template text"},
			},
			series: []string{`items 1`, `items 2`, `rows 3`, `list 12`, `templates 1`, `chart 4`},
			failed: []string{`template: 1 failed, first metric "template" value is missing: CSS selector "template#item" matched an element without a value`},
		},
		"xpath": {
			transform: "xpath",
			rules: []model.MetricRule{
				{Name: "items", Expression: "//p[@class='item']/span", ErrorMode: model.ErrorModeLog},
				{Name: "rows", Expression: "//table[@id='rows']//td", ErrorMode: model.ErrorModeLog},
				{Name: "list", Expression: "//div[@id='list']", Labels: []model.LabelRule{{Name: "placeholder", Expression: "template/p"}, {Name: "template", Expression: "template/@id"}}},
				{Name: "inside", Expression: "//template[not(ancestor::svg)]//node()", Required: notRequired},
				{Name: "inside_count", Expression: "count(//template[not(ancestor::svg)]/node())"},
				{Name: "templates", Expression: "count(//template)"},
				{Name: "template", Expression: "//template[@id='item']", ErrorMode: model.ErrorModeLog},
				{Name: "chart", Expression: "//svg/template/text"},
			},
			series: []string{`items 1`, `items 2`, `rows 3`, `list{template="item"} 12`, `inside_count 0`, `templates 3`, `chart 4`},
			failed: []string{`template: 1 failed, first metric "template" value is missing for node 0: HTML XPath "//template[@id='item']" selected a node without a value`},
		},
	} {
		series, failed := readPage(t, htmlRulesCollector(tc.transform, tc.rules...), "text/html", templatePage)
		if !reflect.DeepEqual(series, tc.series) || !reflect.DeepEqual(failed, tc.failed) {
			t.Errorf("%s made\n%s\nwith the failures %q, want\n%s\nwith %q", name, strings.Join(series, "\n"), failed, strings.Join(tc.series, "\n"), tc.failed)
		}
	}
}

// A declarative shadow root, a template with a shadowrootmode attribute or
// the shadowroot one before it, is content a browser renders, and its
// inspector shows it: it is read like any other, where it was taken out
// with every template's, so that `p.shadow` and the first of `//p` found
// nothing. Its elements are those of the template element, and their text
// is part of the text of the element around it. A template inside a shadow
// root is a template still.
func TestADeclarativeShadowRootIsRead(t *testing.T) {
	const page = `<html><body>` +
		`<div id="host"><template shadowrootmode="open"><p class="shadow">7</p><template><p class="shadow">{{n}}</p></template></template><p>8</p></div>` +
		`<div id="legacy"><template shadowroot="closed"><p class="old">9</p></template></div>` +
		`<div id="plain"><template><p class="shadow">{{n}}</p></template></div>` +
		`</body></html>`
	for name, tc := range map[string]struct {
		transform string
		rules     []model.MetricRule
		series    []string
	}{
		"css": {
			transform: "css",
			rules: []model.MetricRule{
				{Name: "shadow", Expression: "p.shadow"},
				{Name: "inside", Items: "#host > template", Expression: "p.shadow"},
				{Name: "host", Expression: "#host"},
				{Name: "legacy", Expression: "#legacy p.old"},
			},
			series: []string{`shadow 7`, `inside 7`, `host 78`, `legacy 9`},
		},
		"xpath": {
			transform: "xpath",
			rules: []model.MetricRule{
				{Name: "paragraphs", Expression: "//p", Labels: []model.LabelRule{{Name: "in", Expression: "name(..)"}, {Name: "mode", Expression: "../@shadowrootmode"}}},
				{Name: "host", Expression: "//div[@id='host']"},
				{Name: "templates", Expression: "count(//template)"},
				{Name: "inside", Expression: "count(//template//p)"},
			},
			series: []string{`paragraphs{in="template",mode="open"} 7`, `paragraphs{in="div"} 8`, `paragraphs{in="template"} 9`, `host 78`, `templates 4`, `inside 2`},
		},
	} {
		series, failed := readPage(t, htmlRulesCollector(tc.transform, tc.rules...), "text/html", page)
		if !reflect.DeepEqual(series, tc.series) || len(failed) != 0 {
			t.Errorf("%s made\n%s\nwith the failures %q, want\n%s", name, strings.Join(series, "\n"), failed, strings.Join(tc.series, "\n"))
		}
	}
}

// The page a pre-script leaves is parsed as the page that arrived is: the
// content of a template the script wrote is not in the document either.
func TestAPreScriptsPageHasNoTemplateContent(t *testing.T) {
	requirePython(t)
	c := htmlRulesCollector("css", model.MetricRule{Name: "rows", Items: "tr", Expression: "td"})
	c.Limits = scriptLimits()
	c.Transform.PreScript = `data = data.replace("<tr><td>1</td></tr>", "<template><tr><td>{{n}}</td></tr></template><tr><td>2<script>var n = 2;</script></td></tr>")`
	set, failures, err := transformWith(context.Background(), t, c, "text/html", `<table><tr><td>1</td></tr></table>`)
	if err != nil || len(failures) != 0 {
		t.Fatalf("%v, failures %+v", err, failures)
	}
	if got, want := htmlSeries(set), []string{`rows 2`}; !reflect.DeepEqual(got, want) {
		t.Errorf("the rule made %q, want %q", got, want)
	}
}

// What a page holds besides scripts, styles and templates is read as it
// was: the markup inside noscript and iframe is their text, and the text of
// the element around them; a hidden element, a textarea, a pre and an
// object's fallback are elements like any other.
func TestOtherNonContentIsReadAsItWas(t *testing.T) {
	const page = `<html><body>
<div id="ns">1<noscript><b>2</b></noscript></div>
<div id="frame">3<iframe src="/x"><b>4</b></iframe></div>
<div id="hidden">5<span hidden>6</span><span style="display:none">7</span></div>
<div id="area"><textarea>
8</textarea>9</div>
<div id="pre"><pre>
10
</pre></div>
<div id="object"><object data="/c.svg"><span>11</span></object></div>
</body></html>`
	rules := map[string][]model.MetricRule{}
	for id, text := range map[string]string{"ns": "1<b>2</b>", "frame": "3<b>4</b>", "hidden": "567", "area": "89", "pre": "10", "object": "11"} {
		rules["css"] = append(rules["css"], model.MetricRule{Name: id, Expression: "#" + id, ValueMap: map[string]float64{text: 1}})
		rules["xpath"] = append(rules["xpath"], model.MetricRule{Name: id, Expression: "//div[@id='" + id + "']", ValueMap: map[string]float64{text: 1}})
	}
	for transform, rules := range rules {
		series, failed := readPage(t, htmlRulesCollector(transform, rules...), "text/html", page)
		if len(series) != 6 || len(failed) != 0 {
			t.Errorf("%s made %q with the failures %q", transform, series, failed)
		}
	}
}

// An XML document has no scripts, styles or templates, only elements of
// those names, which are read like any other.
func TestXMLElementsNamedScriptStyleAndTemplateAreOrdinary(t *testing.T) {
	const document = `<r><template><v id="t">1</v></template><v id="s">2<script>3</script></v><v id="y"><style>4</style>5</v></r>`
	c := model.Collector{Name: "xml", Decoder: model.DecoderConfig{Type: "xml"}, Transform: model.TransformConfig{Type: "xpath"}, Metrics: []model.MetricRule{
		{Name: "v", Type: model.GaugeMetricType, Expression: "//v", Labels: []model.LabelRule{{Name: "id", Expression: "@id"}, {Name: "text", Expression: "."}}},
		{Name: "all", Type: model.GaugeMetricType, Expression: "/r | //v", Labels: []model.LabelRule{{Name: "id", Expression: "@id"}}},
	}}
	series, failed := readPage(t, c, "application/xml", document)
	want := []string{`v{id="t",text="1"} 1`, `v{id="s",text="23"} 23`, `v{id="y",text="45"} 45`, `all 12345`, `all{id="t"} 1`, `all{id="s"} 23`, `all{id="y"} 45`}
	if !reflect.DeepEqual(series, want) || len(failed) != 0 {
		t.Errorf("the rules made\n%s\nwith the failures %q, want\n%s", strings.Join(series, "\n"), failed, strings.Join(want, "\n"))
	}
}

// textByAncestors is the text of an HTML node as htmlText is to give it,
// worked out the other way around: every text node beneath it, in document
// order, that has no script or style between it and the node.
func textByAncestors(node *html.Node) string {
	if node.Type == html.TextNode {
		return node.Data
	}
	var text strings.Builder
	// From one node to the next in document order, by the tree's links.
	for at := node; ; {
		if at.Type == html.TextNode {
			within := false
			for parent := at.Parent; parent != node; parent = parent.Parent {
				within = within || parent.Type == html.ElementNode && (parent.Data == "script" || parent.Data == "style")
			}
			if !within {
				text.WriteString(at.Data)
			}
		}
		if at.FirstChild != nil {
			at = at.FirstChild
			continue
		}
		for at != node && at.NextSibling == nil {
			at = at.Parent
		}
		if at == node {
			return text.String()
		}
		at = at.NextSibling
	}
}

// everyHTMLNode is every node beneath and including root, in document order.
func everyHTMLNode(root *html.Node) []*html.Node {
	all := []*html.Node{root}
	for child := root.FirstChild; child != nil; child = child.NextSibling {
		all = append(all, everyHTMLNode(child)...)
	}
	return all
}

// The text of every node of every fixture page is what goquery and
// htmlquery read, which the transforms read before, once the scripts and
// styles are taken out of the page; and with them in it, the text of every
// node is that of the text nodes with no script or style between them and
// the node. Pages without a script or a style read as they did, node for
// node; in those with one only the elements around it read otherwise.
func TestHTMLTextIsWhatWasReadButForScriptsAndStyles(t *testing.T) {
	nodes, changed, pagesChanged := 0, 0, 0
	for name, page := range htmlFixturePages(t) {
		for _, node := range everyHTMLNode(withoutScriptsAndStyles(t, page)) {
			got := htmlText(node)
			if inner := htmlquery.InnerText(node); got != inner {
				t.Fatalf("%s without scripts and styles: a %s reads %.200q, and htmlquery read %.200q", name, node.Data, got, inner)
			}
			if text := goquery.NewDocumentFromNode(node).Text(); got != text {
				t.Fatalf("%s without scripts and styles: a %s reads %.200q, and goquery read %.200q", name, node.Data, got, text)
			}
			nodes++
		}
		doc, err := goquery.NewDocumentFromReader(strings.NewReader(page))
		if err != nil {
			t.Fatal(err)
		}
		before := changed
		for _, node := range everyHTMLNode(doc.Nodes[0]) {
			got := htmlText(node)
			if want := textByAncestors(node); got != want {
				t.Fatalf("%s: a %s reads %.200q, want %.200q", name, node.Data, got, want)
			}
			if got != htmlquery.InnerText(node) {
				if goquery.NewDocumentFromNode(node).Find("script, style").Length() == 0 {
					t.Fatalf("%s: a %s without a script or a style beneath it reads %.200q, and read %.200q", name, node.Data, got, htmlquery.InnerText(node))
				}
				changed++
			}
			nodes++
		}
		if changed != before {
			pagesChanged++
		}
	}
	if nodes < 5000 || changed == 0 || pagesChanged < 3 {
		t.Fatalf("%d nodes compared, %d read otherwise, in %d pages", nodes, changed, pagesChanged)
	}
	t.Logf("%d nodes compared, %d read otherwise, in %d pages", nodes, changed, pagesChanged)
}

// htmlTransformRules are css and xpath rules over the elements pages are
// made of, none of which selects a script, a style or a template.
var htmlTransformRules = map[string][]model.MetricRule{
	"css": {
		{Name: "rows", Items: "tr", Expression: "td:nth-child(2)", Labels: []model.LabelRule{{Name: "first", Expression: "td:nth-child(1)"}, {Name: "head", Expression: "th"}}},
		{Name: "rows_mapped", Items: "tr", Expression: "td:last-child", ValueMap: map[string]float64{"*": 1}, Labels: []model.LabelRule{{Name: "first", Expression: "td:first-child"}}},
		{Name: "items", Items: "li", Expression: "span", ValueMap: map[string]float64{"*": 1}, Labels: []model.LabelRule{{Name: "b", Expression: "b"}}},
		{Name: "lists", Items: "tbody, ul, ol, dl", Expression: "tr:first-child, li:first-child, dt:first-child", ValueMap: map[string]float64{"*": 1},
			Labels: []model.LabelRule{{Name: "last", Expression: "tr:last-child, li:last-child, dd:last-child"}}},
		{Name: "blocks", Items: "body > *", Expression: "*:first-child", ValueMap: map[string]float64{"*": 1}, Labels: []model.LabelRule{{Name: "last", Expression: "*:last-child"}}},
		{Name: "title", Expression: "title", ValueMap: map[string]float64{"*": 1}},
		{Name: "body", Expression: "body", ValueMap: map[string]float64{"*": 1}},
		{Name: "h1", Expression: "h1", ValueMap: map[string]float64{"*": 1}},
	},
	"xpath": {
		{Name: "cells", Expression: "//td", Labels: []model.LabelRule{{Name: "first", Expression: "../td[1]"}, {Name: "row", Expression: ".."}, {Name: "id", Expression: "../@id"}, {Name: "text", Expression: "text()"}}},
		{Name: "cells_mapped", Expression: "//td | //th", ValueMap: map[string]float64{"*": 1}, Labels: []model.LabelRule{{Name: "self", Expression: "."}, {Name: "next", Expression: "following-sibling::*[1]"}}},
		{Name: "rows", Expression: "//tr | //li | //p | //div | //dl", ValueMap: map[string]float64{"*": 1}, Labels: []model.LabelRule{{Name: "self", Expression: "."}, {Name: "child", Expression: "*[1]"}, {Name: "span", Expression: "span"}}},
		{Name: "nested", Expression: "//table | //tbody | //tr | //td | //ul | //li | //span", ValueMap: map[string]float64{"*": 1}, Labels: []model.LabelRule{{Name: "self", Expression: "."}}},
		{Name: "body", Expression: "/html/body", ValueMap: map[string]float64{"*": 1}, Labels: []model.LabelRule{{Name: "self", Expression: "."}, {Name: "html", Expression: ".."}}},
		{Name: "texts", Expression: "//td/text() | //li/text() | //p/text()", ValueMap: map[string]float64{"*": 1}, Labels: []model.LabelRule{{Name: "of", Expression: ".."}}},
	},
}

// Over every fixture page the transforms make of the page what they made
// of it with its scripts, its styles and the content of its templates
// taken out of the markup: for rules that select none of those, a page is
// read as if they were not there. For xpath that is held against the
// accessors as they were on main too, reading the page without them.
func TestHTMLTransformsReadAPageAsIfItHadNoScriptsStylesOrTemplateContent(t *testing.T) {
	compared, changed, made := 0, 0, map[string]int{}
	for name, page := range htmlFixturePages(t) {
		parsed, err := decode.ParseHTML([]byte(page))
		if err != nil {
			t.Fatal(err)
		}
		without := withoutScriptsAndStyles(t, page)
		plain, err := goquery.NewDocumentFromReader(strings.NewReader(page))
		if err != nil {
			t.Fatal(err)
		}
		for transform, rules := range htmlTransformRules {
			rules := slicesOfRules(rules)
			read := func(root *html.Node, nodes xpathNodes[*html.Node]) []string {
				c := model.Collector{Name: "differential"}
				ctx := withSeriesBudget(context.Background(), 1<<20)
				var set *model.MetricSet
				var err error
				if transform == "css" {
					set, err = transformCSS(ctx, goquery.NewDocumentFromNode(root), rules, &c)
				} else {
					set, err = transformXPathNodes(ctx, root, nodes, rules, &c, nil)
				}
				if err != nil {
					t.Fatalf("%s over %s: %v", transform, name, err)
				}
				return htmlSeries(set)
			}
			got, want := read(parsed.Nodes[0], htmlNodes), read(without, htmlNodes)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("%s over %s: %d series, and %d of the page without scripts, styles and template content; the first that differs:\n%s", transform, name, len(got), len(want), firstDifference(got, want))
			}
			if transform == "xpath" {
				if main := read(without, mainsHTMLNodes); !reflect.DeepEqual(got, main) {
					t.Fatalf("xpath over %s: %d series, and main made %d of the page without scripts, styles and template content; the first that differs:\n%s", name, len(got), len(main), firstDifference(got, main))
				}
				if main := read(plain.Nodes[0], mainsHTMLNodes); !reflect.DeepEqual(got, main) {
					changed++
				}
			}
			compared += len(got)
			for _, line := range got {
				metric, _, _ := strings.Cut(line, "{")
				metric, _, _ = strings.Cut(metric, " ")
				made[transform+" "+metric]++
			}
		}
	}
	// Every rule read something, so none is compared as nothing twice.
	for transform, rules := range htmlTransformRules {
		for _, rule := range rules {
			if made[transform+" "+rule.Name] == 0 {
				t.Errorf("the %s rule %s made no series of any page", transform, rule.Name)
			}
		}
	}
	if compared < 1500 || changed < 3 {
		t.Fatalf("%d series compared, and %d pages read otherwise than on main", compared, changed)
	}
	t.Logf("%d series compared, and %d pages read otherwise than on main", compared, changed)
}

// slicesOfRules are rules made to carry on quietly past what they cannot
// read, so that a comparison is of every series a page can give.
func slicesOfRules(rules []model.MetricRule) []model.MetricRule {
	out := make([]model.MetricRule, len(rules))
	for i, rule := range rules {
		rule.Type, rule.Required, rule.ErrorMode = model.GaugeMetricType, new(bool), model.ErrorModeIgnore
		out[i] = rule
	}
	return out
}

// Nested nodes selected together are read in one walk, which leaves a
// script out of the elements around it and reads it, and the text inside
// it, on its own: each selected node has the text it has alone.
func TestNestedSelectedNodesAroundAScriptReadTheSameText(t *testing.T) {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(notTextPage))
	if err != nil {
		t.Fatal(err)
	}
	for _, expression := range []string{"//*", "//node()", "//ul | //li | //b | //script | //style", "//li//text() | //li", "//b | //b/script | //b/script/text()", "//svg | //svg/* | //svg//text()", "//script/ancestor-or-self::*"} {
		sameTexts(t, expression, htmlNodes, doc.Nodes[0], expression)
	}
	if !sameTexts(t, "//li | //li//*", htmlNodes, doc.Nodes[0], "//li | //li//*") {
		t.Fatal("the nesting should be found")
	}
}

// Reading a page costs a rule no more than it did before scripts and styles
// were left out and a selected attribute had a parent, in allocations per
// series over a page of two thousand rows: a css series cost 11 while the
// text of every cell was built anew, and costs 9 now that the text of an
// element that holds one text node is that node's; a cell with elements in
// it is built as it was (11.5); an xpath series over HTML costs the 45 it
// did, and a series of a rule that selects attributes, with the attribute
// itself for a label, the 6 it did. The figures are those of a build
// without the race detector.
func TestReadingHTMLCostsARuleNoMoreThanItDid(t *testing.T) {
	if raceDetector {
		t.Skip("the race detector changes what is allocated")
	}
	var plain, mixed strings.Builder
	plain.WriteString("<html><body><table>")
	mixed.WriteString("<html><body><table>")
	for i := range boundSeries {
		fmt.Fprintf(&plain, `<tr class="item" id="r%d"><td class="id">item-%d</td><td class="value">%d</td></tr>`, i, i, i)
		fmt.Fprintf(&mixed, `<tr class="item" id="r%d"><td class="id">item-<b>%d</b></td><td class="value"><i>%d</i><b>1</b></td></tr>`, i, i, i)
	}
	plain.WriteString("</table></body></html>")
	mixed.WriteString("</table></body></html>")
	css := htmlRulesCollector("css", model.MetricRule{Name: "row_value", Items: "tr.item", Expression: "td.value", Labels: []model.LabelRule{{Name: "id", Expression: "td.id"}}})
	xpath := htmlRulesCollector("xpath", model.MetricRule{Name: "row_value", Expression: "//tr/td[@class='value']", Labels: []model.LabelRule{{Name: "id", Expression: "../td[1]"}, {Name: "row", Expression: "../@id"}}})
	attributes := htmlRulesCollector("xpath", model.MetricRule{Name: "row", Expression: "//tr/@id", ValueMap: map[string]float64{"*": 1}, Labels: []model.LabelRule{{Name: "id", Expression: "."}}})
	for _, tc := range []struct {
		name      string
		collector model.Collector
		page      string
		allocs    float64
	}{
		{"css, cells of one text", css, plain.String(), 9.5},
		{"css, cells with elements", css, mixed.String(), 12},
		{"xpath, cells of one text", xpath, plain.String(), 45.5},
		{"xpath, cells with elements", xpath, mixed.String(), 48},
		{"xpath, a rule that selects attributes", attributes, plain.String(), 6.5},
	} {
		allocs, bytes := transformCost(t, tc.collector, tc.page, boundSeries)
		t.Logf("%s: %.3f allocations and %.0f bytes per series", tc.name, allocs, bytes)
		if allocs > tc.allocs {
			t.Errorf("%s: %.3f allocations per series, want at most %.1f", tc.name, allocs, tc.allocs)
		}
	}
}
