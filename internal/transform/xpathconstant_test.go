package transform

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/antchfx/xmlquery"
	"github.com/eenchev/prometheus-universal-exporter/internal/decode"
	"github.com/eenchev/prometheus-universal-exporter/internal/expr"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
	"golang.org/x/net/html"
)

// constantXPathLabels are label expressions that cannot depend on the node
// they are evaluated at, as xpathConstantLabel has it: one absolute path,
// or one call of a function of its argument alone with one for its
// argument, blanks anywhere a blank may stand.
var constantXPathLabels = []string{
	"/", " / ", "/*", "//*", "/*/@*", "//@id", "//@*", "//region", "/items/@id", "/items/item/region", "//item/value", "//item//value", "// item / value",
	"//*[@id][1]/@id", "//*[last()]", "//item[region = ../item[1]/region]/@id", "//item[value > 1 and @id]/region", "//value[position() = last()]/..",
	"//*[. = ']'][. != \"[\"]", "//item[1][@id]", "//item [1] [@id] /region", "//value/text()", "//comment()", "/node()", "//text ( )", "//x:region", "//x:*",
	"/*/*[1]/following-sibling::*[1]", "//value/ancestor::*[1]/@id", "/descendant::value[2]", "/descendant-or-self :: node()", "//item/attribute::id", "//item/@ id",
	"//item/.", "//item/..", "//region/../value", "//*[@id = //value/../@id]/@id", "//div", "//and/or", "/mod/div", "/*/.[1]", "//td", "//li[@data-id]",
	"count(//*)", "count(//@*)", " count( //value ) ", "count (//item[region])", "sum(//value)", "sum(//@id)", "sum(/items/item/value)", "string(//item[2])",
	"number(//value)", "boolean(//nothing)", "not(//nothing)", "not(/)", "normalize-space(//td)", "string-length(/)", "name(/*)", "local-name(//*[@id])",
	"round(//value)", "floor(//value[2])", "ceiling(//value[last()])",
}

// nodeXPathLabels are label expressions that are evaluated at every node:
// those that do depend on it, and those that do not but are none of the
// two shapes xpathConstantLabel knows.
var nodeXPathLabels = []string{
	"region", "../@id", ".//region", "./region", "string(.)", "count(*)", "count(.//*)", "name()", "normalize-space()", "position()", "..", "text()",
	"//region | region", "region | //region", "//region | //value", "//region = .", ". = //region", "//value + 1", "- //value", "//value div 2", "//a and b",
	"concat(/items/@id, '-', @id)", "concat(//region, '')", "count(//item) + count(*)", "count(//item, 1)", "string(//region) = 'x'", "count(//item) > 0",
	"(//region)[1]", "(//region)", "count((//item))", "sum(//value) div count(//value)", "string(string(//region))", "translate(//region, 'a', 'b')",
	"substring(//region, 2)", "contains(//region, 'a')", "starts-with(//region, .)", "lang('en')", "last()", "string-join(//region, ',')", "reverse(//region)",
	"fn:count(//item)", "count(//item)[1]", "count(region)", "count(../item)", "sum(value)", "sum(.//value)", "name(.)", "name(..)", "not(region)", "string()",
	"//item/count(value)", "/items/string(@id)", "//processing-instruction()", "//processing-instruction('region')", "//item/(region, value)", "//item/(value)",
	"//", "", " ", "1", "'/'", "'//region'", "string('//region')", "//region,", "//region //value x", ".5", "/.5",
}

// A label is taken not to depend on the node for one absolute path and for
// one call of a function of its argument alone over one, and for nothing
// else: an operator, a union, a comma, parentheses or a call outside the
// path's predicates, a relative path, or any other function leaves it to be
// evaluated at every node, whether it depends on the node or not.
func TestALabelIsConstantOnlyForAnAbsolutePathOrOneFunctionOfOne(t *testing.T) {
	for _, expression := range constantXPathLabels {
		if !xpathConstantLabel(expression) {
			t.Errorf("%q is evaluated at every node", expression)
		}
		if !xpathReachesRoot(expression) {
			t.Errorf("%q is not taken to reach the document's root", expression)
		}
	}
	for _, expression := range nodeXPathLabels {
		if xpathConstantLabel(expression) {
			t.Errorf("%q is evaluated once", expression)
		}
	}
	// The plan has it for a label the engine evaluates, and for no other.
	rule := model.MetricRule{Name: "m", Labels: []model.LabelRule{
		{Name: "static", Value: "//region"}, {Name: "own", Expression: "@id"}, {Name: "walked", Expression: "../region"},
		{Name: "engine", Expression: "concat(//region, @id)"}, {Name: "path", Expression: "//region"}, {Name: "call", Expression: "count(//item)"},
	}}
	for _, html := range []bool{false, true} {
		plan := planXPathLabels(rule, nil, html)
		for i, want := range []bool{false, false, false, false, true, true} {
			if got := plan[i].constant != nil; got != want {
				t.Errorf("label %s, html %v: read once is %v, want %v", plan[i].name, html, got, want)
			}
		}
		releaseXPathLabels(plan)
	}
}

// perNodePlan is a rule's plan with every label evaluated at every node, as
// all were before a label could be read once.
func perNodePlan(rule model.MetricRule, namespaces map[string]string, html bool) []xpathLabel {
	plan := planXPathLabels(rule, namespaces, html)
	for i := range plan {
		plan[i].constant = nil
	}
	return plan
}

// labelRead is what a rule is given for one label at one node: the labels,
// after a sum in them was read, and the failure of that or the panic.
type labelRead struct {
	labels  map[string]string
	failure string
	panic   string
}

// readLabelAt reads the labels of plan at a node as a rule does: the labels
// first, then the nodes of the sums in them.
func readLabelAt[N comparable](nodes xpathNodes[N], node N, rule model.MetricRule, plan []xpathLabel, index int) (read labelRead) {
	defer func() {
		if failed := recover(); failed != nil {
			read = labelRead{panic: fmt.Sprint(failed)}
		}
	}()
	read.labels = xpathLabels(nodes, node, plan)
	if summed := summedXPathLabels(plan); summed != nil {
		if err := readSummedXPathLabels(nodes, node, rule, plan, summed, index, read.labels); err != nil {
			read.failure = err.Error()
		}
	}
	return read
}

// constantAtEveryNode holds each expression, as a rule's one label, read
// once to what it is read as at every node of nodesAt the slow way, by the
// engine at that node. It returns how many labels it compared, and how many
// of them had a value or a failure.
func constantAtEveryNode[N comparable](t *testing.T, nodes xpathNodes[N], selected []N, namespaces map[string]string, expressions []string, document string) (compared, told int) {
	t.Helper()
	for _, expression := range expressions {
		rule := model.MetricRule{Name: "m", Labels: []model.LabelRule{{Name: "l", Expression: expression}}}
		once, each := planXPathLabels(rule, namespaces, nodes.html), perNodePlan(rule, namespaces, nodes.html)
		if once[0].kind != xpathLabelEngine || once[0].constant == nil {
			// One the load refuses for this kind of document, or reads by
			// a walk.
			releaseXPathLabels(once)
			releaseXPathLabels(each)
			continue
		}
		for at, node := range selected {
			got, want := readLabelAt(nodes, node, rule, once, at), readLabelAt(nodes, node, rule, each, at)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("label %q at node %d of %s (namespaces %v): read once it is %+v, and at the node %+v", expression, at, document, namespaces, got, want)
			}
			if got.panic != "" {
				// The rule ends at the first panic, and its plan with it.
				break
			}
			compared++
			if len(got.labels) > 0 || got.failure != "" {
				told++
			}
		}
		releaseXPathLabels(once)
		releaseXPathLabels(each)
	}
	return compared, told
}

// someOf are at most limit of nodes, the first and the last among them, in
// their order.
func someOf[N any](nodes []N, limit int) []N {
	if len(nodes) <= limit {
		return nodes
	}
	some := make([]N, 0, limit)
	for i := range limit {
		some = append(some, nodes[i*(len(nodes)-1)/(limit-1)])
	}
	return some
}

// A label that is read once for its rule has, at every node, the value the
// engine gives it evaluated at that node: present, absent, trimmed, or a
// sum's failure with the node's number, alike. It is held to that for every
// expression taken to be constant, at elements, text nodes, comments, the
// document and the nodes made of attributes, in whatever order a rule may
// come to them, over the fixtures, the documents of the label tests and
// random ones, XML with and without response.namespaces and HTML.
func TestALabelReadOnceIsTheLabelReadAtEveryNode(t *testing.T) {
	expressions := append([]string{}, constantXPathLabels...)
	// Over the fixtures' own names too.
	expressions = append(expressions, "//Cube/@rate", "sum(//Cube/@rate)", "count(//Cube[@currency])", "//h3", "//td[2]", "sum(//td)", "sum(//span[@class='country-population'])",
		"string(//title)", "normalize-space(//h3)", "//table//tr[last()]/td[1]", "//status/@site", "sum(//value)", "sum(//job)", "//job[@id > 7]", "name(//*[text()])")
	bindings := []map[string]string{nil, {"x": "urn:x", "y": "urn:y"}}
	compared, told, documents := 0, 0, 0
	xml := append([]string{rootedMarkup}, xpathLabelDocuments...)
	random := rand.New(rand.NewPCG(20261003, 12))
	for range 10 {
		xml = append(xml, randomXML(random))
	}
	fixtures, err := filepath.Glob("../../testdata/xml/*.xml")
	if err != nil || len(fixtures) == 0 {
		t.Fatalf("the XML fixtures: %v, %v", fixtures, err)
	}
	for _, fixture := range fixtures {
		body, err := os.ReadFile(fixture)
		if err != nil {
			t.Fatal(err)
		}
		xml = append(xml, string(body))
	}
	for i, document := range xml {
		root, err := decode.ParseXML([]byte(document))
		if err != nil {
			t.Fatalf("XML document %d: %v", i, err)
		}
		all := treeNodes(t, xmlNodes, root)
		for _, namespaces := range bindings {
			// In document order, as a rule comes to its nodes, and from
			// the last node back, so that the label is read first at
			// another node.
			for _, selected := range [][]*xmlquery.Node{someOf(all, 50), reversed(someOf(all, 12))} {
				c, s := constantAtEveryNode(t, xmlNodes, selected, namespaces, expressions, fmt.Sprintf("XML document %d", i))
				compared, told = compared+c, told+s
			}
		}
		documents++
	}
	pages := append([]string{rootedMarkup}, xpathLabelPages...)
	fixtures, err = filepath.Glob("../../testdata/html/*.html")
	if err != nil || len(fixtures) == 0 {
		t.Fatalf("the HTML fixtures: %v, %v", fixtures, err)
	}
	for _, fixture := range fixtures {
		body, err := os.ReadFile(fixture)
		if err != nil {
			t.Fatal(err)
		}
		pages = append(pages, string(body))
	}
	for i, page := range pages {
		doc, err := decode.ParseHTML([]byte(page))
		if err != nil {
			t.Fatalf("HTML document %d: %v", i, err)
		}
		all := treeNodes(t, htmlNodes, doc.Nodes[0])
		for _, selected := range [][]*html.Node{someOf(all, 50), reversed(someOf(all, 12))} {
			c, s := constantAtEveryNode(t, htmlNodes, selected, nil, expressions, fmt.Sprintf("HTML document %d", i))
			compared, told = compared+c, told+s
		}
		documents++
	}
	if compared < 50000 || told < compared/10 {
		t.Fatalf("%d labels compared, %d of them with a value or a failure: too few to show anything", compared, told)
	}
	t.Logf("%d labels compared, %d of them with a value or a failure, of %d expressions over %d documents", compared, told, len(expressions), documents)
}

// reversed is nodes from the last to the first.
func reversed[N any](nodes []N) []N {
	back := slices.Clone(nodes)
	slices.Reverse(back)
	return back
}

// Whatever an expression is put together of, one taken to be constant has
// one value at every node of a document, read the slow way at each: the
// test of what cannot depend on the node takes nothing the engine reads
// relative to it. The pieces make paths, calls, operators, unions and
// predicates in every order, and the documents have nodes of every kind
// with different names, values and children.
func TestNoExpressionTakenToBeConstantDependsOnTheNode(t *testing.T) {
	pieces := []string{"/", "/", "//", "//", "item", "value", "region", "*", "@id", "@*", ".", "..", "[1]", "[@id]", "[region]", "[. = 1]", "[position() = 2]", "text()", "node()",
		"count(", "sum(", "string(", "name(", "not(", "number(", "normalize-space(", "concat(", ")", ")", " ", "|", " or ", " and ", " div ", "+", "-", "*", "=", ",", "child::", "ancestor::", "self::",
		"following-sibling::", "parent::", "1", "'x'", "x:", "(", "[", "]", "last()", "position()"}
	random := rand.New(rand.NewPCG(7, 20261003))
	var roots []*xmlquery.Node
	for _, document := range append([]string{rootedMarkup, xpathLabelDocuments[0]}, randomXML(random), randomXML(random), randomXML(random)) {
		roots = append(roots, decodedXML(t, document))
	}
	constant, compiled, valued := 0, 0, 0
	for range 150000 {
		var text strings.Builder
		for range 1 + random.IntN(8) {
			text.WriteString(pieces[random.IntN(len(pieces))])
		}
		expression := text.String()
		if !xpathConstantLabel(expression) {
			continue
		}
		constant++
		if _, err := expr.CompileXPath(expression, nil); err != nil {
			continue
		}
		compiled++
		rule := model.MetricRule{Name: "m", Labels: []model.LabelRule{{Name: "l", Expression: expression}}}
		for _, root := range roots {
			plan := perNodePlan(rule, nil, false)
			var first labelRead
			for at, node := range someOf(treeNodes(t, xmlNodes, root), 25) {
				read := readLabelAt(xmlNodes, node, rule, plan, -1)
				if at == 0 {
					first = read
				} else if !reflect.DeepEqual(read, first) {
					t.Fatalf("label %q is taken to be constant, and is %+v at node 0 and %+v at node %d", expression, first, read, at)
				}
				if read.panic != "" {
					break
				}
			}
			if len(first.labels) > 0 {
				valued++
			}
			releaseXPathLabels(plan)
		}
	}
	if compiled < 500 || valued < 300 {
		t.Fatalf("%d expressions taken to be constant, %d of them compiled and %d values: too few to show anything", constant, compiled, valued)
	}
	t.Logf("%d expressions taken to be constant, %d of them compiled, %d values", constant, compiled, valued)
}

// rulesBothWays evaluates rules over a document as transformXPathNodes does,
// with their constant labels read once, or, with each set, at every node as
// before; it gives the series, the failures reported and the error.
func rulesBothWays[N comparable](t *testing.T, root N, nodes xpathNodes[N], rules []model.MetricRule, each bool) (series []string, reported []string, failure string) {
	t.Helper()
	c := &model.Collector{Name: "constant", Metrics: rules}
	ctx, report := WithRuleReport(LeaveRuleLoggingToCaller(context.Background()))
	ctx, failures := withRuleFailures(ctx)
	out := &model.MetricSet{}
	for _, rule := range rules {
		program, err := expr.CompileXPath(rule.Expression, nil)
		if err != nil {
			t.Fatal(err)
		}
		expression, plan := program.Get(), planXPathLabels(rule, nil, nodes.html)
		if each {
			releaseXPathLabels(plan)
			plan = perNodePlan(rule, nil, nodes.html)
		}
		err = xpathRule(ctx, out, root, nodes, rule, c, plan, expression, program.Sums())
		releaseXPathLabels(plan)
		program.Put(expression)
		if err != nil {
			failure = err.Error()
			break
		}
	}
	failures.finish(c, report)
	for _, failed := range report.Failures() {
		reported = append(reported, fmt.Sprintf("%s: %d failures, %d missing, first %v", failed.Metric, failed.Failures, failed.Missing, failed.First))
	}
	return htmlSeries(out), reported, failure
}

// A rule with a label that is read once makes the series it made with the
// label read at every node, fails the series it failed, and says the same
// of each: a label that is missing and required, one that is blank and left
// off, a sum over text that is no number, which fails every series by its
// node, and a sum the exporter adds up, over elements, text nodes and
// attributes, under log and under fail, over XML and over HTML.
func TestARuleWithAConstantLabelGivesWhatItGaveWithTheLabelReadAtEachNode(t *testing.T) {
	const page = `<status site="fra1"><blank>  </blank><size>2</size><size> 4 </size><bad>1</bad><bad>n/a</bad>` +
		`<job id="7">12</job><job id="8"></job><job id="9">x</job><job>30</job></status>`
	labelSets := map[string][]model.LabelRule{
		"a path":                    {{Name: "site", Expression: "//status/@site"}, {Name: "id", Expression: "@id"}},
		"a path that selects none":  {{Name: "none", Expression: "//nothing"}, {Name: "site", Expression: "/status/@site"}},
		"a required one missing":    {{Name: "none", Expression: "//nothing", Required: true}},
		"a required one blank":      {{Name: "blank", Expression: "//blank", Required: true}},
		"a blank one":               {{Name: "blank", Expression: "//blank"}, {Name: "computed", Expression: "string(//blank)"}},
		"calls":                     {{Name: "jobs", Expression: "count(//job)"}, {Name: "name", Expression: "name(/*)"}, {Name: "none", Expression: "not(//job)"}, {Name: "nan", Expression: "number(//bad[2])"}},
		"a sum the exporter adds":   {{Name: "sizes", Expression: "sum(//size)"}, {Name: "ids", Expression: " sum( //@id ) "}},
		"a sum over text":           {{Name: "bad", Expression: "sum(//bad)"}, {Name: "site", Expression: "//status/@site"}},
		"a sum over text, required": {{Name: "bad", Expression: "sum(//bad)", Required: true}},
		"two sums, the second bad":  {{Name: "sizes", Expression: "sum(//size)"}, {Name: "bad", Expression: "sum(//bad)"}},
		"beside labels of the node": {{Name: "site", Expression: "//status/@site"}, {Name: "own", Expression: "concat(//status/@site, '-', name())"}, {Name: "up", Expression: "name(..)"}, {Name: "sum", Expression: "sum(../size)"}},
	}
	compared, failed := 0, 0
	for name, labels := range labelSets {
		for _, selecting := range []string{"//job", "//job/text()", "//job/@id", "//size", "count(//job)", "sum(//size)", "//nothing"} {
			for _, mode := range []string{model.ErrorModeLog, model.ErrorModeFail, model.ErrorModeIgnore} {
				rules := []model.MetricRule{
					{Name: "first", Type: model.GaugeMetricType, Expression: "count(//job)"},
					{Name: "m", Type: model.GaugeMetricType, Expression: selecting, ErrorMode: mode, Labels: labels},
					{Name: "again", Type: model.GaugeMetricType, Expression: "//size[1]", ErrorMode: model.ErrorModeLog, Labels: labels},
				}
				xmlRoot := decodedXML(t, page)
				series, reported, failure := rulesBothWays(t, xmlRoot, xmlNodes, rules, false)
				wantSeries, wantReported, wantFailure := rulesBothWays(t, xmlRoot, xmlNodes, rules, true)
				if !slices.Equal(series, wantSeries) || !slices.Equal(reported, wantReported) || failure != wantFailure {
					t.Errorf("%s on %s under %s over XML:\n%q, %q, %q\nand with the labels read at every node:\n%q, %q, %q", name, selecting, mode, series, reported, failure, wantSeries, wantReported, wantFailure)
				}
				doc, err := decode.ParseHTML([]byte(page))
				if err != nil {
					t.Fatal(err)
				}
				htmlSeriesOnce, htmlReported, htmlFailure := rulesBothWays(t, doc.Nodes[0], htmlNodes, rules, false)
				wantSeries, wantReported, wantFailure = rulesBothWays(t, doc.Nodes[0], htmlNodes, rules, true)
				if !slices.Equal(htmlSeriesOnce, wantSeries) || !slices.Equal(htmlReported, wantReported) || htmlFailure != wantFailure {
					t.Errorf("%s on %s under %s over HTML:\n%q, %q, %q\nand with the labels read at every node:\n%q, %q, %q", name, selecting, mode, htmlSeriesOnce, htmlReported, htmlFailure, wantSeries, wantReported, wantFailure)
				}
				compared += 2
				if len(reported) > 0 || failure != "" {
					failed++
				}
			}
		}
	}
	if failed < compared/8 {
		t.Fatalf("%d of %d rules failed: too few to show anything of failures", failed, compared)
	}
	// What the comparison stands on, spelled out for one rule: the sum
	// fails the series of each node, as it did.
	_, reported, _ := rulesBothWays(t, decodedXML(t, page), xmlNodes, []model.MetricRule{{Name: "m", Expression: "//size", ErrorMode: model.ErrorModeLog, Labels: labelSets["a sum over text"]}}, false)
	want := `label "bad": XPath "sum(//bad)" cannot be computed: it adds up text that is not a number, first "n/a" (1 of 2 nodes); `
	if len(reported) != 1 || !strings.HasPrefix(reported[0], `m: 2 failures, 0 missing, first metric "m" `) || !strings.Contains(reported[0], want) {
		t.Errorf("the failures %q, want two of the metric m with %q", reported, want)
	}
}

// A label that cannot depend on the node costs a rule what a static label
// costs it, and not an evaluation at every node: over four hundred rows a
// rule with `count(//row)` or `//last/@id` for a label allocates what the
// rule with a label of a fixed value does, give or take the one evaluation,
// where each row cost an evaluation of the whole document more.
func TestAConstantLabelIsEvaluatedOnceForItsRule(t *testing.T) {
	if raceDetector {
		t.Skip("allocations cannot be counted under the race detector")
	}
	const rows = 400
	var page strings.Builder
	page.WriteString(`<status site="fra1">`)
	for i := range rows {
		fmt.Fprintf(&page, `<row id="%d"><v>%d</v></row>`, i, i)
	}
	page.WriteString(`<last id="z"/></status>`)
	root := decodedXML(t, page.String())
	cost := func(label model.LabelRule) (allocations float64, labels map[string]string) {
		rules := []model.MetricRule{{Name: "m", Expression: "//row/v", Labels: []model.LabelRule{label}}}
		c := &model.Collector{Name: "cost", Metrics: rules}
		allocations, _ = alloctest.Allocations(10, func() {
			set, err := transformXPathNodes(context.Background(), root, xmlNodes, rules, c, nil)
			if err != nil || len(set.Metrics) != rows {
				t.Fatalf("%d series, %v", len(set.Metrics), err)
			}
			labels = set.Metrics[rows-1].Labels
		})
		return allocations, labels
	}
	static, _ := cost(model.LabelRule{Name: "l", Value: "z"})
	for expression, want := range map[string]string{"count(//row)": "400", "//last/@id": "z", "/status/@site": "fra1", "sum(//v)": "79800", "name(/*)": "status"} {
		once, labels := cost(model.LabelRule{Name: "l", Expression: expression})
		if labels["l"] != want {
			t.Errorf("%s: the label %q on the last row, want %q", expression, labels["l"], want)
		}
		// One evaluation may allocate for every node it reads, as the sum
		// does; an evaluation at each row allocates for each some twenty
		// times over.
		if once > static+4*rows {
			t.Errorf("%s: %v allocations for %d rows, and %v with a static label: the label is evaluated at the rows", expression, once, rows, static)
		}
	}
	// One that does depend on the node is evaluated at each.
	if each, _ := cost(model.LabelRule{Name: "l", Expression: "count(//row) + count(*)"}); each < static+8*rows {
		t.Errorf("a label read at every node cost %v allocations, and a static one %v: the test tells nothing", each, static)
	}
}
