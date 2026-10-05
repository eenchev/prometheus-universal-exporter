package transform

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"reflect"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/PuerkitoBio/goquery"
	"github.com/antchfx/xmlquery"
	"github.com/antchfx/xpath"
	"golang.org/x/net/html"

	"github.com/eenchev/prometheus-universal-exporter/internal/expr"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
)

// The XPath engine's sum() leaves out, without a word, every node whose
// text it cannot read as a number, a number with blanks around it among
// them. The exporter reads the nodes of a sum() itself (xpathsum.go): the
// tests here hold what it then exports, and that nothing else changed.

// The three functions below are addComputedXPathSeries, transformXPathNodes
// and xpathRule as they were before that, and before an XPath failure named
// its metric and its node: the oracle the tests here compare with.

func addComputedXPathSeriesBeforeSums[N comparable](ctx context.Context, out *model.MetricSet, nodes xpathNodes[N], root N, rule model.MetricRule, c *model.Collector, plan []xpathLabel, value any) error {
	var number float64
	var problem error
	switch v := value.(type) {
	case float64:
		if math.IsNaN(v) {
			problem = model.MarkError(fmt.Errorf("%s %q computed NaN, not a number", nodes.kind, rule.Expression), model.ErrMissingValue)
		}
	case string:
		if isBlank(v) {
			problem = model.MarkError(fmt.Errorf("%s %q computed an empty string", nodes.kind, rule.Expression), model.ErrMissingValue)
		}
	}
	if problem == nil {
		n, err := ruleValue(rule, value)
		if err != nil {
			problem = fmt.Errorf("metric %q: %w", rule.Name, err)
		}
		number = n
	}
	if problem != nil {
		if errors.Is(problem, model.ErrMissingValue) && !requiredRule(rule, c) {
			return nil
		}
		if handleMetricError(ctx, c, rule, problem) {
			return nil
		}
		return ruleFailure(c, rule, problem)
	}
	if unreadable := unreadableXPathLabel(nodes, rule, plan); unreadable != nil {
		if handleMetricError(ctx, c, rule, unreadable) {
			return nil
		}
		return ruleFailure(c, rule, unreadable)
	}
	labels := xpathLabels(nodes, root, plan)
	if missing := missingRequiredLabel(rule, labels); missing != nil {
		if handleMetricError(ctx, c, rule, missing) {
			return nil
		}
		return ruleFailure(c, rule, missing)
	}
	if err := takeSeries(ctx); err != nil {
		return err
	}
	out.Metrics = append(out.Metrics, model.Metric{Name: rule.Name, Help: rule.Description, Type: rule.Type, Value: number, Labels: labels})
	return nil
}

func transformXPathNodesBeforeSums[N comparable](ctx context.Context, root N, nodes xpathNodes[N], rules []model.MetricRule, c *model.Collector, namespaces map[string]string) (*model.MetricSet, error) {
	out := &model.MetricSet{}
	for _, rule := range rules {
		program, err := expr.CompileXPath(rule.Expression, namespaces)
		if err != nil {
			if handleMetricError(ctx, c, rule, err) {
				continue
			}
			return nil, ruleFailure(c, rule, fmt.Errorf("%s %q: %w", nodes.kind, rule.Expression, err))
		}
		// How each label is read is decided here, once for the rule, and
		// not at each of its nodes (planXPathLabels).
		expression, plan := program.Get(), planXPathLabels(rule, namespaces, nodes.html)
		err = xpathRuleBeforeSums(ctx, out, root, nodes, rule, c, plan, expression)
		releaseXPathLabels(plan)
		program.Put(expression)
		if err != nil {
			return nil, err
		}
	}
	return noSeriesIsNil(out), nil
}

func xpathRuleBeforeSums[N comparable](ctx context.Context, out *model.MetricSet, root N, nodes xpathNodes[N], rule model.MetricRule, c *model.Collector, plan []xpathLabel, expression *xpath.Expr) error {
	if value, computed := xpathValue(nodes, root, expression); computed {
		return addComputedXPathSeriesBeforeSums(ctx, out, nodes, root, rule, c, plan, value)
	}
	selected := nodes.all(root, expression)
	if len(selected) == 0 {
		if requiredRule(rule, c) {
			missing := model.MarkError(fmt.Errorf("%s %q matched no nodes", nodes.kind, rule.Expression), model.ErrMissingValue)
			if handleMetricError(ctx, c, rule, missing) {
				return nil
			}
			return ruleFailure(c, rule, missing)
		}
		return nil
	}
	// A label that cannot be read in this kind of document is the rule's
	// failure, once, rather than a label left off each of its series.
	if unreadable := unreadableXPathLabel(nodes, rule, plan); unreadable != nil {
		if handleMetricError(ctx, c, rule, unreadable) {
			return nil
		}
		return ruleFailure(c, rule, unreadable)
	}
	out.Metrics = growSeries(ctx, out.Metrics, len(selected))
	texts := selectedTexts[N]{nodes: nodes, selected: selected}
	for i, node := range selected {
		// A node costs what is beneath it to read, and a label what its
		// expression walks, so the deadline is asked after before each.
		if err := interrupted(ctx, rule); err != nil {
			return err
		}
		labels := xpathLabels(nodes, node, plan)
		text := texts.text(i)
		if isBlank(text) {
			if requiredRule(rule, c) {
				missing := model.MarkError(fmt.Errorf("%s %q selected a node without a value", nodes.kind, rule.Expression), model.ErrMissingValue)
				if handleMetricError(ctx, c, rule, missing) {
					continue
				}
				return ruleFailure(c, rule, missing)
			}
			continue
		}
		value, err := ruleTextValue(rule, text)
		if err != nil {
			if handleMetricError(ctx, c, rule, err) {
				continue
			}
			return ruleFailure(c, rule, fmt.Errorf("metric %q: %w", rule.Name, err))
		}
		if missing := missingRequiredLabel(rule, labels); missing != nil {
			if handleMetricError(ctx, c, rule, missing) {
				continue
			}
			return ruleFailure(c, rule, missing)
		}
		if err := takeSeries(ctx); err != nil {
			return err
		}
		out.Metrics = append(out.Metrics, model.Metric{Name: rule.Name, Help: rule.Description, Type: rule.Type, Value: value, Labels: labels})
	}
	return nil
}

// xpathFailures gathers what the rules of an XPath transform could not
// make, for a test that calls the transform itself.
func xpathFailures(failures *ruleFailures) []string {
	var failed []string
	for _, rule := range failures.rules {
		failed = append(failed, fmt.Sprintf("%s: %d failed, %d of them missing", rule.rule.Name, rule.count, rule.missing))
	}
	return failed
}

// xpathNodesRun is what a transform of XPath rules, as it is or as it was,
// makes of a parsed document: its series, how many series of each rule
// failed and how many of those for a missing value, and whether a rule
// failed the transform. The wording of a failure is left out: it changed.
func xpathNodesRun[N comparable](t *testing.T, transform func(context.Context, N, xpathNodes[N], []model.MetricRule, *model.Collector, map[string]string) (*model.MetricSet, error), root N, nodes xpathNodes[N], rules []model.MetricRule) []string {
	t.Helper()
	c := model.Collector{Name: "sums"}
	ctx, failures := withRuleFailures(LeaveRuleLoggingToCaller(withSeriesBudget(context.Background(), 1<<20)))
	set, err := transform(ctx, root, nodes, rules, &c, nil)
	made := append(htmlSeries(set), xpathFailures(failures)...)
	if err != nil {
		var failure *MetricFailure
		if !errors.As(err, &failure) {
			t.Fatal(err)
		}
		made = append(made, "the rule "+failure.Metric+" failed the transform")
	}
	return made
}

// parsedMarkup is a document parsed as XML and as HTML, as the decoders
// parse one.
func parsedMarkup(t *testing.T, body string) (*xmlquery.Node, *html.Node) {
	t.Helper()
	asXML, err := xmlquery.Parse(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	asHTML, err := goquery.NewDocumentFromReader(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	return asXML, asHTML.Nodes[0]
}

// sumPage is a document of groups of numbers, each number the text of a
// span with another in its attribute n, and one more in the attribute w of
// its group: the same document to the XML and to the HTML decoder.
func sumPage(numbers func() string, groups, perGroup int) string {
	var page strings.Builder
	page.WriteString(`<html><head><title>sums</title></head><body>`)
	for group := range groups {
		fmt.Fprintf(&page, `<div id="g%d" w="%s">`, group, numbers())
		for range perGroup {
			fmt.Fprintf(&page, `<span class="v" n="%s">%s</span>`, numbers(), numbers())
		}
		page.WriteString(`</div>`)
	}
	page.WriteString(`</body></html>`)
	return page.String()
}

// plainNumbers are numbers as the XPath engine's sum() reads them, written
// without blanks around them: every text sum() ever added up.
var plainNumbers = []string{
	"0", "7", "42", "-3", "+4", "007", "2.5", ".5", "5.", "-0.25", "1e3", "2E-3", "1.5e+2", "0.1", "0.2", "0.30000000000000004",
	"9007199254740993", "1e-400", "1e308", "123456789.125", "1_000", "0x1p-2", "-0X1P+4", "Inf", "-Infinity", "+inf",
}

// wholeSums are expressions that are one sum() over nodes, and partSums
// expressions a sum() over nodes is a part of, or that the engine evaluates
// for another reason: an argument that is no node-set, a sum() inside a
// predicate. All of them are computed from the document.
var (
	wholeSums = []string{
		"sum(//span)", "sum(//span/@n)", "sum(//div/@w)", "sum(//div[1]/span)", "sum(//span | //div/@w)", "sum(//span/text())",
		"sum(//div[@id='g2']/span[position() < 3])", "sum(//nothing)", "sum(//span[. > 2])", "sum(//span, //div)", " sum( //span )\n",
		"sum(//span[sum(@n) > 1])", "sum(//span/@n/..)", "sum(//div/span[last()])", "sum((//span)[position() mod 2 = 1])",
	}
	partSums = []string{
		"sum(//span) div count(//span)", "round(sum(//span/@n))", "sum(//span) + sum(//span/@n)", "-sum(//span)", "string(sum(//span))",
		"sum(//span) > 10", "sum(sum(//span))", "concat('total ', sum(//span))", "floor(sum(//div[2]/span) * 100)", "(sum(//span))",
		"fn:sum(//span)", "sum(//span) = sum(//span/@n)",
		"sum(3)", "sum('3')", "sum(count(//span))", "sum(number(//span[1]))", "count(//div[sum(span) > 5])", "count(//span[sum(@n) = sum(.)])",
	}
)

// A sum() whose nodes are all numbers without blanks around them is what
// the engine made of it, to the bit: from the document, as a rule's
// expression is evaluated, and from a selected node, an element, a text
// node and an attribute, as a label's is; over XML and over HTML; the whole
// expression, which the exporter adds up, and a part of one, which the
// engine computes still. The sum is compared with the engine's own, through
// xpathValue as a rule and a label are evaluated, and the series and
// failures of rules and labels with those of the transform as it was. Under
// the race detector the pages are the first eight of the twenty-four, which
// have every number of groups and of numbers in a group that the others
// have, and numbers of both kinds.
func TestASumOfPlainNumbersIsWhatTheEngineGives(t *testing.T) {
	random := rand.New(rand.NewPCG(11, 4))
	numbers := func() string { return plainNumbers[random.IntN(len(plainNumbers))] }
	// A document of finite numbers alone as well, so that not every sum is
	// an infinity or the NaN of two.
	finite := func() string { return plainNumbers[random.IntN(21)] }
	labels := []string{
		"sum(span)", "sum(../span)", "sum(../span/@n)", "sum(//span)", "sum(.)", "sum(../@w)", "sum(following-sibling::span)", "sum(@n | ../@w)",
		"sum(../span) div count(../span)", "string(sum(../span) > 3)", "round(sum(//span/@n))", "sum(sum(../span))", "sum(3)", "count(../span[sum(@n) > 1])",
	}
	added, engines, compared := 0, 0, 0
	sameSum := func(where, expression string, own bool, sum float64, value any, computed bool) {
		t.Helper()
		engine, number := value.(float64)
		if !own || !computed || !number {
			t.Fatalf("%s: %s is the exporter's to add up: %v, and computes a number: %v, %v", where, expression, own, computed, value)
		}
		if math.Float64bits(sum) != math.Float64bits(engine) && !(math.IsNaN(sum) && math.IsNaN(engine)) {
			t.Fatalf("%s: %s is %v, and the engine made %v", where, expression, sum, engine)
		}
		added++
	}
	for page := range alloctest.UnlessRaced(24, 8) {
		source := numbers
		if page%2 == 1 {
			source = finite
		}
		body := sumPage(source, 2+page%4, 1+page%5)
		asXML, asHTML := parsedMarkup(t, body)
		for at, expression := range append(append([]string{}, wholeSums...), labels...) {
			program, err := expr.CompileXPath(expression, nil)
			if err != nil {
				t.Fatal(err)
			}
			sums := program.Sums()
			if sums == nil {
				// A sum() inside a predicate alone, the engine's.
				engines++
				continue
			}
			e := program.Get()
			// From the document, and from each node a rule selects: a
			// label's sum from the nodes that are numbers, since some add
			// up the node itself.
			contexts := []string{"/", "//div", "//span", "//span/text()", "//div/@w", "//span/@n"}
			if at >= len(wholeSums) {
				contexts = contexts[2:]
			}
			for _, from := range contexts {
				selector := xpath.MustCompile(from)
				for i, node := range xmlNodes.all(asXML, selector) {
					sum, own, fault := readXPathSums(xmlNodes, node, sums)
					value, computed := xpathValue(xmlNodes, node, e)
					if fault != nil {
						t.Fatalf("XML, %s from node %d of %s: %s", expression, i, from, fault.reason())
					}
					if sums.Whole != nil && own {
						sameSum(fmt.Sprintf("XML, from node %d of %s", i, from), expression, own, sum, value, computed)
					} else {
						engines++
					}
				}
				for i, node := range htmlNodes.all(asHTML, selector) {
					sum, own, fault := readXPathSums(htmlNodes, node, sums)
					value, computed := xpathValue(htmlNodes, node, e)
					if fault != nil {
						t.Fatalf("HTML, %s from node %d of %s: %s", expression, i, from, fault.reason())
					}
					if sums.Whole != nil && own {
						sameSum(fmt.Sprintf("HTML, from node %d of %s", i, from), expression, own, sum, value, computed)
					} else {
						engines++
					}
				}
			}
			program.Put(e)
		}
		// The rules: each sum as a rule's expression, and as the label of
		// rules that select elements, text nodes and attributes.
		var rules []model.MetricRule
		for i, expression := range append(append([]string{}, wholeSums...), partSums...) {
			rules = append(rules, model.MetricRule{Name: "computed" + strconv.Itoa(i), Type: model.GaugeMetricType, Expression: expression, ErrorMode: model.ErrorModeIgnore,
				Labels: []model.LabelRule{{Name: "all", Expression: "sum(//span)"}, {Name: "mean", Expression: "sum(//span) div count(//span)"}}})
		}
		for i, expression := range []string{"//span", "//span/text()", "//div/@w", "//span/@n", "//div/span[1]"} {
			rule := model.MetricRule{Name: "selected" + strconv.Itoa(i), Type: model.GaugeMetricType, Expression: expression, ErrorMode: model.ErrorModeIgnore}
			for l, label := range labels {
				rule.Labels = append(rule.Labels, model.LabelRule{Name: "l" + strconv.Itoa(l), Expression: label})
			}
			rules = append(rules, rule)
		}
		now, before := xpathNodesRun(t, transformXPathNodes[*xmlquery.Node], asXML, xmlNodes, rules), xpathNodesRun(t, transformXPathNodesBeforeSums[*xmlquery.Node], asXML, xmlNodes, rules)
		if !reflect.DeepEqual(now, before) {
			t.Fatalf("XML, over %s: %d series and failures, and before %d; the first that differs:\n%s", body, len(now), len(before), firstDifference(now, before))
		}
		compared += len(now)
		now, before = xpathNodesRun(t, transformXPathNodes[*html.Node], asHTML, htmlNodes, rules), xpathNodesRun(t, transformXPathNodesBeforeSums[*html.Node], asHTML, htmlNodes, rules)
		if !reflect.DeepEqual(now, before) {
			t.Fatalf("HTML, over %s: %d series and failures, and before %d; the first that differs:\n%s", body, len(now), len(before), firstDifference(now, before))
		}
		compared += len(now)
	}
	if added < alloctest.UnlessRaced(10000, 3300) || engines < alloctest.UnlessRaced(2000, 650) || compared < alloctest.UnlessRaced(3000, 1000) {
		t.Fatalf("only %d sums the exporter added up compared with the engine's, %d left to the engine, and %d series and failures compared", added, engines, compared)
	}
	t.Logf("%d sums the exporter added up are the engine's, %d were left to the engine, and %d series and failures are as they were", added, engines, compared)
}

// sumRuleRun reads a document with one xpath rule under an error mode, as a
// probe does, and gives the rule's series, what was reported of its
// failures, and the failure of the transform.
func sumRuleRun(t *testing.T, decoder, body string, rule model.MetricRule, mode string) ([]string, []string, error) {
	t.Helper()
	rule.ErrorMode = mode
	c := xpathRuleCollector(decoder, rule)
	if err := CheckMetricRule(&c, &c.Metrics[0]); err != nil {
		t.Fatal(err)
	}
	set, failures, err := transformWith(LeaveRuleLoggingToCaller(context.Background()), t, c, markupContentType[decoder], body)
	var failed []string
	for _, failure := range failures {
		failed = append(failed, fmt.Sprintf("%d failed, %d of them missing, first %v", failure.Failures, failure.Missing, failure.First))
	}
	return htmlSeries(set), failed, err
}

// What a failure of a sum() says after the expression: for the one the
// exporter adds up, and for the one that is a part of a larger expression.
const (
	wholeSumRemedy = "; leave such nodes out with a predicate, as in sum(//td[number(.) = number(.)]), or select the nodes with a rule of their own, where value_map or a pre_script makes numbers of the text, and add the series up in PromQL"
	partSumRemedy  = ", and blanks around a number count; XPath 1.0 cannot trim each node, so select the nodes with a rule of their own and add the series up in PromQL, or rewrite the text in a pre_script"
)

// sumTable is a table as a page prints one, each cell on a line of its own
// with the indentation around it, with %s for the one cell that a test
// makes text of. It reads the same as XML and as HTML.
const sumTable = `<html><head><title>traffic</title></head><body><table id="traffic"><tbody>
  <tr id="web01">
    <td class="name">web01</td>
    <td class="bytes">
      12
    </td>
    <td class="bytes" dir="out"> 30 </td>
  </tr>
  <tr id="web02">
    <td class="name">web02</td>
    <td class="bytes">%s</td>
    <td class="bytes" dir="out">7</td>
  </tr>
  <tr id="web03">
    <td class="name">web03</td>
    <td class="bytes">0.5</td>
    <td class="bytes" dir="out">	2	</td>
  </tr>
</tbody></table></body></html>`

// A rule that is one sum() adds up every node its argument selects, a
// number with blanks around it included, where the engine left those out:
// the sum of a column printed one cell to a line was that of the cells
// without blanks alone, 8.5 for 52.5, and no error. Text that is no number
// leaves the rule without a value, as XPath has the sum NaN and as a rule
// that computes NaN is: a required rule reports its missing value, with the
// expression, the first such text and how many nodes there are of them, by
// its error_mode, and one that is not required makes no series and reports
// nothing. Over XML and over HTML.
func TestAWholeXPathSumAddsUpNumbersWithBlanksAndHasNoValueOverText(t *testing.T) {
	rule := model.MetricRule{Name: "bytes", Type: model.GaugeMetricType, Expression: "sum(//td[@class='bytes'])"}
	for _, decoder := range []string{"xml", "html"} {
		kind := map[string]string{"xml": "XPath", "html": "HTML XPath"}[decoder]
		series, failed, err := sumRuleRun(t, decoder, fmt.Sprintf(sumTable, "1"), rule, model.ErrorModeFail)
		if err != nil || failed != nil || !reflect.DeepEqual(series, []string{"bytes 52.5"}) {
			t.Errorf("%s, numbers with blanks around them: %v, failures %v, %v; want their sum, 52.5", decoder, series, failed, err)
		}
		// What the engine made of the same column.
		asXML, asHTML := parsedMarkup(t, fmt.Sprintf(sumTable, "1"))
		before := xpathNodesRun(t, transformXPathNodesBeforeSums[*xmlquery.Node], asXML, xmlNodes, []model.MetricRule{rule})
		if decoder == "html" {
			before = xpathNodesRun(t, transformXPathNodesBeforeSums[*html.Node], asHTML, htmlNodes, []model.MetricRule{rule})
		}
		if !reflect.DeepEqual(before, []string{"bytes 8.5"}) {
			t.Errorf("%s: the engine made %v of the column, want the 8.5 of the cells without blanks", decoder, before)
		}
		for text, count := range map[string]string{"n/a": "1 of 6 nodes", "1,234": "1 of 6 nodes", "": "1 of 6 nodes", "12 MB": "1 of 6 nodes", "1e400": "1 of 6 nodes"} {
			body := fmt.Sprintf(sumTable, text)
			want := fmt.Sprintf(`metric "bytes" %s "sum(//td[@class='bytes'])" cannot be computed: it adds up text that is not a number, first %q (%s)%s`, kind, text, count, wholeSumRemedy)
			series, failed, err := sumRuleRun(t, decoder, body, rule, model.ErrorModeFail)
			var failure *MetricFailure
			if series != nil || failed != nil || !errors.As(err, &failure) || !errors.Is(err, model.ErrMissingValue) || err.Error() != want {
				t.Errorf("%s, a cell of %q under fail: %v, failures %v and\n%v\nwant the missing value\n%s", decoder, text, series, failed, err, want)
			}
			for _, mode := range []string{model.ErrorModeLog, model.ErrorModeIgnore} {
				series, failed, err = sumRuleRun(t, decoder, body, rule, mode)
				if err != nil || series != nil || !reflect.DeepEqual(failed, []string{"1 failed, 1 of them missing, first " + want}) {
					t.Errorf("%s, a cell of %q under %s: %v, %v and the failures\n%v\nwant no series and one missing value\n%s", decoder, text, mode, series, err, failed, want)
				}
			}
			optional := rule
			optional.Required = new(bool)
			if series, failed, err = sumRuleRun(t, decoder, body, optional, model.ErrorModeFail); err != nil || series != nil || failed != nil {
				t.Errorf("%s, a cell of %q and a rule that is not required: %v, failures %v, %v; want no series and nothing reported", decoder, text, series, failed, err)
			}
		}
		// Several such cells are counted, and the first is the one named.
		body := strings.Replace(fmt.Sprintf(sumTable, "n/a"), "0.5", "-", 1)
		want := fmt.Sprintf(`metric "bytes" %s "sum(//td[@class='bytes'])" cannot be computed: it adds up text that is not a number, first "n/a" (2 of 6 nodes)%s`, kind, wholeSumRemedy)
		if _, _, err := sumRuleRun(t, decoder, body, rule, model.ErrorModeFail); err == nil || err.Error() != want {
			t.Errorf("%s, two cells of text:\n%v\nwant\n%s", decoder, err, want)
		}
		// The predicate the message gives leaves the text out, and the
		// rule has its value again.
		filtered := rule
		filtered.Expression = "sum(//td[@class='bytes'][number(.) = number(.)])"
		if series, failed, err := sumRuleRun(t, decoder, body, filtered, model.ErrorModeFail); err != nil || failed != nil || !reflect.DeepEqual(series, []string{"bytes 51"}) {
			t.Errorf("%s, the cells that are numbers alone: %v, failures %v, %v; want their sum, 51", decoder, series, failed, err)
		}
		// A sum of nothing is 0, and value_map and scale read the sum as
		// they read any computed value.
		none := rule
		none.Expression = "sum(//td[@class='none'])"
		if series, _, err := sumRuleRun(t, decoder, body, none, model.ErrorModeFail); err != nil || !reflect.DeepEqual(series, []string{"bytes 0"}) {
			t.Errorf("%s, a sum of no nodes: %v, %v; want 0", decoder, series, err)
		}
		// The nodes are read as XPath has their text, which over HTML is
		// with the script inside a cell: the exporter reads the cell
		// itself without it.
		if decoder == "html" {
			const scripted = `<table><tr><td>6<script>track(6)</script></td><td>1</td></tr></table>`
			cells := model.MetricRule{Name: "cells", Type: model.GaugeMetricType, Expression: "//td"}
			if series, _, err := sumRuleRun(t, decoder, scripted, cells, model.ErrorModeFail); err != nil || !reflect.DeepEqual(series, []string{"cells 6", "cells 1"}) {
				t.Errorf("cells with a script: %v, %v; want 6 and 1", series, err)
			}
			cells.Expression = "sum(//td)"
			want := `metric "cells" HTML XPath "sum(//td)" cannot be computed: it adds up text that is not a number, first "6track(6)" (1 of 2 nodes)` + wholeSumRemedy
			if _, _, err := sumRuleRun(t, decoder, scripted, cells, model.ErrorModeFail); err == nil || err.Error() != want {
				t.Errorf("the sum of cells with a script:\n%v\nwant\n%s", err, want)
			}
		}
		scaled, thousandth := rule, 0.001
		scaled.Scale = &thousandth
		if series, _, err := sumRuleRun(t, decoder, fmt.Sprintf(sumTable, "1"), scaled, model.ErrorModeFail); err != nil || !reflect.DeepEqual(series, []string{"bytes 0.0525"}) {
			t.Errorf("%s, the sum scaled: %v, %v; want 0.0525", decoder, series, err)
		}
	}
}

// A sum() that is a part of a larger expression is the engine's to
// compute, which reads each node without trimming it: where all its nodes
// are numbers as the engine reads them the rule has the value the engine
// computes, and where the engine would leave a node out — text that is no
// number, and a number with blanks around it — the rule has no value, and
// its failure names the sum, the text and the blanks, where it exported a
// number made of the other nodes. The nodes are read whether or not the
// engine would come to evaluate the sum. A sum() inside a predicate is
// evaluated for each node the predicate is asked of, and is left to the
// engine as it was, as is one whose argument is a number or a string.
func TestAnXPathSumInALargerExpressionHasNoValueOverTextTheEngineLeavesOut(t *testing.T) {
	const clean = `<r><v n="1">4</v><v n="2">6</v><v n="3">0.5</v></r>`
	const blank = `<r><v n="1">4</v><v n="2"> 6 </v><v n="3">0.5</v></r>`
	const text = `<r><v n="1">4</v><v n="2">n/a</v><v n="3">-</v></r>`
	for _, decoder := range []string{"xml", "html"} {
		kind := map[string]string{"xml": "XPath", "html": "HTML XPath"}[decoder]
		for expression, want := range map[string]string{
			"sum(//v) div count(//v)":     "m 3.5",
			"round(sum(//v))":             "m 11",
			"sum(//v) + sum(//v/@n)":      "m 16.5",
			"(sum(//v))":                  "m 10.5",
			"sum(//v) > 10":               "m 1",
			"string(sum(//v))":            "m 10.5",
			"sum(sum(//v))":               "m 10.5",
			"count(//v[sum(@n) > 1])":     "m 2",
			"sum(3)":                      "m 3",
			"sum('3')":                    "m 3",
			"sum(count(//v))":             "m 3",
			"sum(//v/@n) div count(//v)":  "m 2",
			"sum(//v[sum(@n) > 1]) div 2": "m 3.25",
		} {
			rule := model.MetricRule{Name: "m", Type: model.GaugeMetricType, Expression: expression}
			if series, failed, err := sumRuleRun(t, decoder, clean, rule, model.ErrorModeFail); err != nil || failed != nil || !reflect.DeepEqual(series, []string{want}) {
				t.Errorf("%s, %s over numbers: %v, failures %v, %v; want %s", decoder, expression, series, failed, err, want)
			}
		}
		for expression, argument := range map[string]string{
			"sum(//v) div count(//v)": "//v",
			"round(sum(//v))":         "//v",
			"sum(//v/@n) + sum(//v)":  "//v",
			"(sum( //v ))":            "//v",
			"sum(sum(//v))":           "//v",
			"string(sum(//v)) = '10'": "//v",
			// Read whether or not the engine would come to the sum.
			"count(//v) > 0 or sum(//v) > 100": "//v",
		} {
			rule := model.MetricRule{Name: "m", Type: model.GaugeMetricType, Expression: expression}
			for body, named := range map[string]string{blank: `first " 6 " (1 of 3 nodes)`, text: `first "n/a" (2 of 3 nodes)`} {
				want := fmt.Sprintf(`metric "m" %s %q cannot be computed: sum(%s) leaves out text it cannot read as a number, %s%s`, kind, expression, argument, named, partSumRemedy)
				series, failed, err := sumRuleRun(t, decoder, body, rule, model.ErrorModeFail)
				if series != nil || failed != nil || err == nil || !errors.Is(err, model.ErrMissingValue) || err.Error() != want {
					t.Errorf("%s, %s over %s under fail: %v, failures %v and\n%v\nwant the missing value\n%s", decoder, expression, body, series, failed, err, want)
				}
				if series, failed, err = sumRuleRun(t, decoder, body, rule, model.ErrorModeLog); err != nil || series != nil || !reflect.DeepEqual(failed, []string{"1 failed, 1 of them missing, first " + want}) {
					t.Errorf("%s, %s over %s under log: %v, %v and the failures\n%v\nwant no series and one missing value\n%s", decoder, expression, body, series, err, failed, want)
				}
				optional := rule
				optional.Required = new(bool)
				if series, failed, err = sumRuleRun(t, decoder, body, optional, model.ErrorModeFail); err != nil || series != nil || failed != nil {
					t.Errorf("%s, %s over %s, not required: %v, failures %v, %v; want no series and nothing reported", decoder, expression, body, series, failed, err)
				}
			}
		}
		// Left to the engine as they were: a sum() inside a predicate, over
		// blanks and text, and an argument that is no node-set.
		for expression, want := range map[string]string{
			"count(//v[sum(.) > 1])":      "m 1",
			"count(//r[sum(v) > 4])":      "m 1",
			"sum(count(//v[sum(.) > 5]))": "m 0",
			"sum(string-length(//v[2]))":  "m 3",
		} {
			rule := model.MetricRule{Name: "m", Type: model.GaugeMetricType, Expression: expression}
			if series, failed, err := sumRuleRun(t, decoder, blank, rule, model.ErrorModeFail); err != nil || failed != nil || !reflect.DeepEqual(series, []string{want}) {
				t.Errorf("%s, %s, the engine's alone: %v, failures %v, %v; want %s", decoder, expression, series, failed, err, want)
			}
		}
	}
}

// Text that is NaN, Inf or Infinity is a number where a document writes it:
// a rule that selects such a node exports it, and a sum() adds it up, an
// infinity to an infinity. A sum that comes to NaN is a computed NaN, the
// rule's missing value as before, like number() of text: the first is what
// the source says, the second what the engine says of text it could not
// read.
func TestTextThatIsNaNOrInfinityStaysANumberAndAComputedNaNAMissingValue(t *testing.T) {
	const body = `<r><v>NaN</v><v>Inf</v><v>-Infinity</v><w>+Inf</w><w> 1 </w><x>n/a</x></r>`
	for _, decoder := range []string{"xml", "html"} {
		kind := map[string]string{"xml": "XPath", "html": "HTML XPath"}[decoder]
		selected := model.MetricRule{Name: "v", Type: model.GaugeMetricType, Expression: "//v"}
		if series, failed, err := sumRuleRun(t, decoder, body, selected, model.ErrorModeFail); err != nil || failed != nil || !reflect.DeepEqual(series, []string{"v NaN", "v +Inf", "v -Inf"}) {
			t.Errorf("%s, nodes of NaN and infinities: %v, failures %v, %v; want each exported", decoder, series, failed, err)
		}
		for expression, want := range map[string]string{"sum(//w)": "m +Inf", "sum(//v[3]) + 1": "m -Inf", "sum(//v[position() > 1] | //w[1])": "m NaN"} {
			if want == "m NaN" {
				// Two infinities of opposite sign: computed, below.
				continue
			}
			rule := model.MetricRule{Name: "m", Type: model.GaugeMetricType, Expression: expression}
			if series, failed, err := sumRuleRun(t, decoder, body, rule, model.ErrorModeFail); err != nil || failed != nil || !reflect.DeepEqual(series, []string{want}) {
				t.Errorf("%s, %s: %v, failures %v, %v; want %s", decoder, expression, series, failed, err, want)
			}
		}
		for _, expression := range []string{"sum(//v)", "sum(//v) div 1", "sum(//v[position() > 1] | //w[1])", "number(//x)", "number('n/a')"} {
			rule := model.MetricRule{Name: "m", Type: model.GaugeMetricType, Expression: expression}
			want := fmt.Sprintf(`metric "m" %s %q computed NaN, not a number`, kind, expression)
			if series, _, err := sumRuleRun(t, decoder, body, rule, model.ErrorModeFail); series != nil || err == nil || !errors.Is(err, model.ErrMissingValue) || err.Error() != want {
				t.Errorf("%s, %s: %v and\n%v\nwant the missing value\n%s", decoder, expression, series, err, want)
			}
		}
	}
}

// A label's expression is held to the same: a label that is one sum() is
// the sum of all its nodes, those with blanks around them included, and
// one whose nodes are not all numbers — or, where the sum() is a part of
// the label's expression, not all numbers as the engine reads them — is a
// label that cannot be read. A label has no missing value, so the series
// fails, as its rule's error_mode has it, and the failure names the metric,
// the node among those the rule selected, the label and the text; the
// series of the other nodes are made. From an element and from an
// attribute a rule selected, and from the document for the one series of a
// computed value, over XML and over HTML.
func TestAnXPathLabelWithASumIsHeldToItsNodes(t *testing.T) {
	body := fmt.Sprintf(sumTable, "n/a")
	withAttributes := `<r><row id="a" first="3"><v n=" 1 ">4</v><v n="2">6</v></row><row id="b" first="5"><v n="x">1</v><v n="2">2</v></row><row id="c" first="7"><v n="1">1</v></row></r>`
	for _, decoder := range []string{"xml", "html"} {
		kind := map[string]string{"xml": "XPath", "html": "HTML XPath"}[decoder]
		for _, test := range []struct {
			name, body string
			rule       model.MetricRule
			series     []string
			failures   int
			first      string
		}{
			{
				name: "a label that is one sum, from an element", body: body,
				rule:     model.MetricRule{Name: "last", Expression: "//td[@dir='out']", Labels: []model.LabelRule{{Name: "host", Expression: "../td[1]"}, {Name: "total", Expression: "sum(../td[@class='bytes'])"}}},
				series:   []string{`last{host="web01",total="42"} 30`, `last{host="web03",total="2.5"} 2`},
				failures: 1,
				first:    `metric "last" node 1 label "total": ` + kind + ` "sum(../td[@class='bytes'])" cannot be computed: it adds up text that is not a number, first "n/a" (1 of 2 nodes)` + wholeSumRemedy,
			},
			{
				name: "a label a sum is a part of, from an element", body: body,
				rule:     model.MetricRule{Name: "last", Expression: "//td[@dir='out']", Labels: []model.LabelRule{{Name: "host", Expression: "../td[1]"}, {Name: "mean", Expression: "sum(../td[@class='bytes']) div 2"}}},
				failures: 3,
				first:    `metric "last" node 0 label "mean": ` + kind + ` "sum(../td[@class='bytes']) div 2" cannot be computed: sum(../td[@class='bytes']) leaves out text it cannot read as a number, first "\n      12\n    " (2 of 2 nodes)` + partSumRemedy,
			},
			{
				name: "labels from an attribute", body: withAttributes,
				rule:     model.MetricRule{Name: "first", Expression: "//row/@first", Labels: []model.LabelRule{{Name: "row", Expression: "../@id"}, {Name: "n", Expression: "sum(../v/@n)"}, {Name: "mean", Expression: "sum(../v) div count(../v)"}}},
				series:   []string{`first{mean="5",n="3",row="a"} 3`, `first{mean="1",n="1",row="c"} 7`},
				failures: 1,
				first:    `metric "first" node 1 label "n": ` + kind + ` "sum(../v/@n)" cannot be computed: it adds up text that is not a number, first "x" (1 of 2 nodes)` + wholeSumRemedy,
			},
			{
				name: "the label of a computed value", body: withAttributes,
				rule:     model.MetricRule{Name: "rows", Expression: "count(//row)", Labels: []model.LabelRule{{Name: "n", Expression: "round(sum(//v/@n))"}}},
				failures: 1,
				first:    `metric "rows" label "n": ` + kind + ` "round(sum(//v/@n))" cannot be computed: sum(//v/@n) leaves out text it cannot read as a number, first " 1 " (2 of 5 nodes)` + partSumRemedy,
			},
			{
				name: "the label of a computed value that is one sum", body: withAttributes,
				rule:   model.MetricRule{Name: "rows", Expression: "count(//row)", Labels: []model.LabelRule{{Name: "total", Expression: "sum(//v)"}, {Name: "firsts", Expression: " sum( //row/@first ) "}}},
				series: []string{`rows{firsts="15",total="14"} 3`},
			},
		} {
			test.rule.Type = model.GaugeMetricType
			series, failed, err := sumRuleRun(t, decoder, test.body, test.rule, model.ErrorModeLog)
			var want []string
			if test.failures > 0 {
				want = []string{fmt.Sprintf("%d failed, 0 of them missing, first %s", test.failures, test.first)}
			}
			if err != nil || !reflect.DeepEqual(series, test.series) || !reflect.DeepEqual(failed, want) {
				t.Errorf("%s, %s, under log: %v\n%s\nwith the failures\n%s\nwant\n%s\nwith\n%s", decoder, test.name, err, strings.Join(series, "\n"), strings.Join(failed, "\n"), strings.Join(test.series, "\n"), strings.Join(want, "\n"))
			}
			if test.failures == 0 {
				continue
			}
			series, failed, err = sumRuleRun(t, decoder, test.body, test.rule, model.ErrorModeFail)
			var failure *MetricFailure
			if series != nil || failed != nil || !errors.As(err, &failure) || errors.Is(err, model.ErrMissingValue) || err.Error() != test.first {
				t.Errorf("%s, %s, under fail: %v, failures %v and\n%v\nwant the failure\n%s", decoder, test.name, series, failed, err, test.first)
			}
			// Not required, the rule fails all the same: the label is no
			// missing value.
			optional := test.rule
			optional.Required = new(bool)
			if _, _, err = sumRuleRun(t, decoder, test.body, optional, model.ErrorModeFail); err == nil || err.Error() != test.first {
				t.Errorf("%s, %s, not required and under fail:\n%v\nwant the failure\n%s", decoder, test.name, err, test.first)
			}
		}
	}
}

// A rule and a label without a sum() of their own context cost what they
// cost: the compiled expression holds no sums, found once when it compiled
// and never looked for again, the rule's plan names no label to read the
// nodes of, and the transform of a document allocates exactly what the
// transform before sums were read allocated.
func TestARuleWithoutASumCostsWhatItCost(t *testing.T) {
	for _, expression := range []string{"//td", "count(//td)", "//td[sum(.) > 1]", "normalize-space(.)", "string(//summary)", "../@name", "//checksum", "concat('sum(', ., ')')"} {
		program, err := expr.CompileXPath(expression, nil)
		if err != nil {
			t.Fatal(err)
		}
		if program.Sums() != nil {
			t.Errorf("%s has sums to read: %+v", expression, program.Sums())
		}
	}
	rule := model.MetricRule{Name: "m", Type: model.GaugeMetricType, Expression: "//span", Labels: []model.LabelRule{
		{Name: "id", Expression: "../@id"}, {Name: "n", Expression: "@n"}, {Name: "kind", Value: "span"}, {Name: "text", Expression: "normalize-space(.)"}, {Name: "count", Expression: "count(../span[sum(@n) > 1])"},
	}}
	plan := planXPathLabels(rule, nil, false)
	if summed := summedXPathLabels(plan); summed != nil {
		t.Errorf("the labels %v of a rule without a sum are read for one", summed)
	}
	releaseXPathLabels(plan)
	withSum := rule
	withSum.Labels = append(withSum.Labels[:len(withSum.Labels):len(withSum.Labels)], model.LabelRule{Name: "total", Expression: "sum(../span)"}, model.LabelRule{Name: "self", Expression: "."})
	plan = planXPathLabels(withSum, nil, false)
	if summed := summedXPathLabels(plan); !reflect.DeepEqual(summed, []int{5}) {
		t.Errorf("the labels %v are read for a sum, want the one that has it, 5", summed)
	}
	releaseXPathLabels(plan)
	if raceDetector {
		return
	}
	number := 0
	asXML, asHTML := parsedMarkup(t, sumPage(func() string { number++; return strconv.Itoa(number) }, 20, 10))
	rules := []model.MetricRule{rule, {Name: "spans", Type: model.GaugeMetricType, Expression: "count(//span)", Labels: []model.LabelRule{{Name: "title", Expression: "string(//title)"}}}}
	c := model.Collector{Name: "cost"}
	// The collector is kept from running while the allocations are
	// counted: it empties the pools the engine and the compiled expressions
	// keep their copies in, and the next run then allocates them anew.
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	cost := func(transform func()) float64 {
		transform()
		allocations, _ := alloctest.Allocations(20, transform)
		return allocations
	}
	now := cost(func() { _, _ = transformXPathNodes(context.Background(), asXML, xmlNodes, rules, &c, nil) })
	before := cost(func() { _, _ = transformXPathNodesBeforeSums(context.Background(), asXML, xmlNodes, rules, &c, nil) })
	if now != before {
		t.Errorf("over XML the transform allocates %v times, and before it read sums %v", now, before)
	}
	now = cost(func() { _, _ = transformXPathNodes(context.Background(), asHTML, htmlNodes, rules, &c, nil) })
	before = cost(func() { _, _ = transformXPathNodesBeforeSums(context.Background(), asHTML, htmlNodes, rules, &c, nil) })
	if now != before {
		t.Errorf("over HTML the transform allocates %v times, and before it read sums %v", now, before)
	}
}

// fixtureDocuments are the fixture pages parsed as HTML, and the XML
// fixtures and every page that is well-formed parsed as XML.
func fixtureDocuments(t *testing.T) (asHTML map[string]*html.Node, asXML map[string]*xmlquery.Node) {
	t.Helper()
	asHTML, asXML = map[string]*html.Node{}, map[string]*xmlquery.Node{}
	pages := htmlFixturePages(t)
	files, err := filepath.Glob("../../testdata/xml/*.xml")
	if err != nil || len(files) < 2 {
		t.Fatalf("%d XML fixtures, %v", len(files), err)
	}
	for _, file := range files {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		pages[filepath.Base(file)] = string(raw)
	}
	for name, page := range pages {
		if !strings.HasSuffix(name, ".xml") {
			doc, err := goquery.NewDocumentFromReader(strings.NewReader(page))
			if err != nil {
				t.Fatal(err)
			}
			asHTML[name] = doc.Nodes[0]
		}
		if root, err := xmlquery.Parse(strings.NewReader(page)); err == nil {
			asXML[name] = root
		}
	}
	return asHTML, asXML
}

// Without a sum() a rule makes what it made: over every fixture, read as
// HTML and as XML, rules that select elements, text and attributes and
// rules that compute a value, required and not, with labels of every
// shape, make the same series and fail for the same number of them, as
// many for a missing value, as the transform did before it read the nodes
// of sums and before its failures named the metric and the node. Only the
// wording of a failure differs, which the tests of the messages hold. Under
// the race detector each expression reads a third of the documents of each
// kind and each document is read by a third of the expressions (pairTaken).
func TestXPathRulesWithoutASumMakeWhatTheyMade(t *testing.T) {
	expressions := []string{
		"//td", "//tr", "//tr/td[1]", "//li", "//span", "//p", "//dt", "//a", "//time", "//*[@id]", "//h1 | //h2 | //title", "//td/text()", "//body//text()",
		"//comment()", "/", "//td[not(*)]", "//@id", "//td/@*", "//a/@href", "//Cube/@rate", "//server/cpu", "//requests", "//nothing",
		"count(//td)", "count(//*)", "string(//title)", "string(//nothing)", "number(//td[1])", "number(//title)", "//td[1] > 1", "normalize-space(//h1)",
		"count(//tr[sum(td) > 1])", "string-length(//summary)",
	}
	labels := append([]string{"count(../td[sum(.) > 0])", "../../@time", "@currency"}, differentialLabels...)
	rules := func(expression string) []model.MetricRule {
		var rules []model.MetricRule
		for i, required := range []bool{true, false} {
			rule := model.MetricRule{Name: "m" + strconv.Itoa(i), Type: model.GaugeMetricType, Expression: expression, Required: &required, ErrorMode: model.ErrorModeIgnore}
			for l, label := range labels {
				// The rule whose value is not required requires the id, so
				// that some of its series fail for a label.
				rule.Labels = append(rule.Labels, model.LabelRule{Name: "l" + strconv.Itoa(l), Expression: label, Required: !required && label == "@id"})
			}
			rules = append(rules, rule)
		}
		mapped := model.MetricRule{Name: "mapped", Type: model.GaugeMetricType, Expression: expression, ValueMap: map[string]float64{"*": 1}, ErrorMode: model.ErrorModeIgnore,
			Labels: []model.LabelRule{{Name: "id", Expression: "@id"}, {Name: "name", Expression: "name()"}, {Name: "up", Expression: "name(..)"}}}
		return append(rules, mapped)
	}
	asHTML, asXML := fixtureDocuments(t)
	placeHTML, placeXML := places(asHTML), places(asXML)
	compared := 0
	for at, expression := range expressions {
		rules := rules(expression)
		for name, root := range asHTML {
			if !pairTaken(at, placeHTML[name], 3) {
				continue
			}
			now, before := xpathNodesRun(t, transformXPathNodes[*html.Node], root, htmlNodes, rules), xpathNodesRun(t, transformXPathNodesBeforeSums[*html.Node], root, htmlNodes, rules)
			if !reflect.DeepEqual(now, before) {
				t.Fatalf("%s over %s as HTML: %d series and failures, and before %d; the first that differs:\n%s", expression, name, len(now), len(before), firstDifference(now, before))
			}
			compared += len(now)
		}
		for name, root := range asXML {
			if !pairTaken(at, placeXML[name], 3) {
				continue
			}
			now, before := xpathNodesRun(t, transformXPathNodes[*xmlquery.Node], root, xmlNodes, rules), xpathNodesRun(t, transformXPathNodesBeforeSums[*xmlquery.Node], root, xmlNodes, rules)
			if !reflect.DeepEqual(now, before) {
				t.Fatalf("%s over %s as XML: %d series and failures, and before %d; the first that differs:\n%s", expression, name, len(now), len(before), firstDifference(now, before))
			}
			compared += len(now)
		}
	}
	if compared < alloctest.UnlessRaced(5000, 1650) || len(asHTML) < 10 || len(asXML) < 3 {
		t.Fatalf("only %d series and failures compared, over %d documents read as HTML and %d as XML", compared, len(asHTML), len(asXML))
	}
	t.Logf("%d series and failures compared, over %d documents read as HTML and %d as XML", compared, len(asHTML), len(asXML))
}

// The argument of a sum() is compiled once with its expression and shared
// by every scrape, as the expression is: scrapes at once each take a copy of
// their own, so the sums they read are right and the race detector finds
// nothing.
func TestTheSumsOfAnExpressionAreReadByScrapesAtOnce(t *testing.T) {
	rules := []model.MetricRule{
		{Name: "total", Type: model.GaugeMetricType, Expression: "sum(//span)", Labels: []model.LabelRule{{Name: "n", Expression: "sum(//span/@n)"}}},
		{Name: "mean", Type: model.GaugeMetricType, Expression: "sum(//span) div count(//span)"},
		{Name: "first", Type: model.GaugeMetricType, Expression: "//div/span[1]", Labels: []model.LabelRule{{Name: "group", Expression: "sum(../span)"}, {Name: "half", Expression: "sum(../span) div 2"}}},
	}
	number := 0
	// Three groups of two spans, numbered as they come: 1 on the first
	// group, 2 and 3 on and in its first span, 4 and 5 its second.
	body := sumPage(func() string { number++; return strconv.Itoa(number) }, 3, 2)
	want := []string{`total{n="48"} 54`, "mean 9", `first{group="8",half="4"} 3`, `first{group="18",half="9"} 8`, `first{group="28",half="14"} 13`}
	var scrapes sync.WaitGroup
	for range 8 {
		scrapes.Go(func() {
			for range 20 {
				c := model.Collector{Name: "sums", Decoder: model.DecoderConfig{Type: "xml"}, Transform: model.TransformConfig{Type: "xpath"}, Metrics: rules}
				set, failures, err := transformWith(context.Background(), t, c, "application/xml", body)
				if got := htmlSeries(set); err != nil || len(failures) != 0 || !reflect.DeepEqual(got, want) {
					t.Errorf("a scrape among others made %v, failures %v, %v; want %v", got, failures, err, want)
					return
				}
			}
		})
	}
	scrapes.Wait()
}
