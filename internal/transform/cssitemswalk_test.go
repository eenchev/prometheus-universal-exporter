package transform

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/PuerkitoBio/goquery"

	"github.com/eenchev/prometheus-universal-exporter/internal/decode"
	"github.com/eenchev/prometheus-universal-exporter/internal/expr"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
)

// A css rule with items finds what its value and label selectors match
// within every item in one walk of the items (cssWithinItems), where it
// searched each item's subtree on its own before. These tests show that it
// costs the body, not the depth of nested items times the body, and that it
// finds what it found before.

// nestedItems is chains of div.i items, each depth deep, every one holding a
// span.v and the next item of its chain; the html parser takes no more than
// 512 open elements, so a deep document is many chains.
func nestedItems(chains, depth int, everyLevel bool) string {
	var b strings.Builder
	b.WriteString("<html><body>")
	for range chains {
		for level := range depth {
			if everyLevel || level == depth-1 {
				fmt.Fprintf(&b, `<div class="i"><span class="v">%d</span>`, level)
			} else {
				b.WriteString(`<div class="i">`)
			}
		}
		b.WriteString(strings.Repeat("</div>", depth))
	}
	b.WriteString("</body></html>")
	return b.String()
}

// decodedCSS is a body decoded for c, to run c's transform over it again
// and again.
func decodedCSS(t *testing.T, c *model.Collector, body string) (*decode.Decoded, *fetch.HTTPResponse) {
	t.Helper()
	r := &fetch.HTTPResponse{Body: []byte(body), Headers: http.Header{"Content-Type": {"text/html"}}}
	d, err := decode.Decode(r, c)
	if err != nil {
		t.Fatal(err)
	}
	return d, r
}

// Items nested in one another cost what their number does, not their depth
// times it: the same number of div.i items, every one holding a span.v, as
// chains twice as deep allocate no more than as chains half as deep. Before,
// each item's search copied out the matches of every item beneath it, and
// the deeper chains cost twice as many bytes (16.6 MB for 2,000 items 500
// deep against 8.6 MB 250 deep, and under 1 MB either way now); an item
// whose value matched only at the bottom of its chain took the depth's time
// too.
func TestCSSItemsNestedDeepCostWhatTheirNumberDoes(t *testing.T) {
	items := alloctest.UnlessRaced(2000, 800)
	deep := alloctest.UnlessRaced(500, 400)
	c := cssItemsCollector("div.i", "span.v")
	c.Metrics[0].ErrorMode = model.ErrorModeIgnore
	bytes := func(depth int) uint64 {
		d, r := decodedCSS(t, &c, nestedItems(items/depth, depth, true))
		_, each := alloctest.Allocations(2, func() {
			set, err := Transform(context.Background(), d, r, &c, "python3")
			// Only the innermost item of each chain has one span.v; every
			// other matches those of the items beneath it too.
			if err != nil || set == nil || len(set.Metrics) != items/depth {
				t.Fatalf("depth %d: err=%v set=%v", depth, err, set)
			}
		})
		return each
	}
	shallow, deeper := bytes(deep/2), bytes(deep)
	if deeper > shallow+shallow/4 {
		t.Fatalf("%d items %d deep allocated %d bytes, %d deep %d: more than the number of items accounts for", items, deep, deeper, deep/2, shallow)
	}
}

// cssItemsAsTheyWere is transformCSSItems as it was before cssWithinItems,
// searching each item on its own: the oracle the walk is held to.
func cssItemsAsTheyWere(ctx context.Context, out *model.MetricSet, doc *goquery.Document, rule model.MetricRule, c *model.Collector) error {
	fail := func(err error) (bool, error) {
		if handleMetricError(ctx, c, rule, err) {
			return true, nil
		}
		return false, ruleFailure(c, rule, err)
	}
	itemsMatcher, err := expr.CompileCSS(rule.Items)
	if err != nil {
		_, failure := fail(fmt.Errorf("metric %q items CSS selector %q: %w", rule.Name, rule.Items, err))
		return failure
	}
	valueMatcher, err := expr.CompileCSS(rule.Expression)
	if err != nil {
		_, failure := fail(fmt.Errorf("metric %q CSS selector %q: %w", rule.Name, rule.Expression, err))
		return failure
	}
	items := doc.FindMatcher(itemsMatcher)
	if items.Length() == 0 && requiredRule(rule, c) {
		_, failure := fail(model.MarkError(fmt.Errorf("metric %q items CSS selector %q matched no nodes", rule.Name, rule.Items), model.ErrMissingValue))
		return failure
	}
	out.Metrics = growSeries(ctx, out.Metrics, items.Length())
	one := func(item *goquery.Selection, index int, selector string, matcher goquery.Matcher) (string, bool, error) {
		found := item.FindMatcher(matcher)
		switch found.Length() {
		case 0:
			return "", false, nil
		case 1:
			return strings.TrimSpace(htmlText(found.Nodes[0])), true, nil
		default:
			return "", false, model.Errorf("metric %q item %d: CSS selector %q matched %d elements; within an item it must match at most one", rule.Name, model.Position(index), selector, model.Size(found.Length()))
		}
	}
	for index := range items.Length() {
		if err := interrupted(ctx, rule); err != nil {
			return err
		}
		item := items.Eq(index)
		text, found, err := one(item, index, rule.Expression, valueMatcher)
		if err == nil && (!found || isBlank(text)) {
			if !requiredRule(rule, c) {
				continue
			}
			why := "matched nothing"
			if found {
				why = "matched an element without a value"
			}
			err = model.MarkError(model.Errorf("metric %q value is missing for item %d: CSS selector %q %s", rule.Name, model.Position(index), rule.Expression, why), model.ErrMissingValue)
		}
		var value float64
		if err == nil {
			value, err = ruleTextValue(rule, text)
			if err != nil {
				err = model.Errorf("metric %q item %d: %w", rule.Name, model.Position(index), err)
			}
		}
		labels := make(map[string]string, len(rule.Labels))
		for _, label := range rule.Labels {
			if err != nil {
				break
			}
			if label.Static() {
				labels[label.Name] = label.Value
				continue
			}
			matcher, compileErr := expr.CompileCSS(label.Expression)
			if compileErr != nil {
				err = fmt.Errorf("metric %q label %q CSS selector %q: %w", rule.Name, label.Name, label.Expression, compileErr)
				break
			}
			var labelText string
			if labelText, _, err = one(item, index, label.Expression, matcher); err == nil {
				labels[label.Name] = labelText
			}
		}
		if err == nil {
			if missing := missingRequiredLabel(rule, labels); missing != nil {
				err = model.Errorf("%w for item %d", missing, model.Position(index))
			}
		}
		if err != nil {
			if carryOn, failure := fail(err); !carryOn {
				return failure
			}
			continue
		}
		if err := takeSeries(ctx); err != nil {
			return err
		}
		out.Metrics = append(out.Metrics, model.Metric{Name: rule.Name, Help: rule.Description, Type: rule.Type, Value: value, Labels: labels})
	}
	return nil
}

// cssItemsOutcome is what one css rule with items gave: its series, its
// failure, and the failures it logged, in order.
type cssItemsOutcome struct {
	series []model.Metric
	err    string
	logged []string
}

// runCSSItemsWith runs rule over doc with run, under each error mode.
func runCSSItemsWith(t *testing.T, run func(context.Context, *model.MetricSet, *goquery.Document, model.MetricRule, *model.Collector) error, doc *goquery.Document, c *model.Collector) []cssItemsOutcome {
	t.Helper()
	var outcomes []cssItemsOutcome
	for _, mode := range []string{model.ErrorModeFail, model.ErrorModeLog, model.ErrorModeIgnore} {
		rule := c.Metrics[0]
		rule.ErrorMode = mode
		logs := testutil.CaptureLogs(t)
		out := &model.MetricSet{}
		var outcome cssItemsOutcome
		if err := run(context.Background(), out, doc, rule, c); err != nil {
			outcome.err = err.Error()
		}
		outcome.series = out.Metrics
		for line := range strings.SplitSeq(strings.TrimSpace(logs.String()), "\n") {
			if line == "" {
				continue
			}
			var record map[string]any
			if err := json.Unmarshal([]byte(line), &record); err != nil {
				t.Fatal(err)
			}
			outcome.logged = append(outcome.logged, fmt.Sprint(record["error"]))
		}
		outcomes = append(outcomes, outcome)
	}
	return outcomes
}

// sameCSSItems fails unless rule gives over body what it gave before, and
// returns what it gave under error_mode log.
func sameCSSItems(t *testing.T, name, body, items, expression string, required bool, labels ...model.LabelRule) cssItemsOutcome {
	t.Helper()
	c := cssItemsCollector(items, expression, labels...)
	c.Metrics[0].Required = &required
	d, _ := decodedCSS(t, &c, body)
	doc := d.Data.(*decode.HTMLDecoded).Document
	got := runCSSItemsWith(t, transformCSSItems, doc, &c)
	want := runCSSItemsWith(t, cssItemsAsTheyWere, doc, &c)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s: items %q, value %q, labels %v, required %v:\ngot  %+v\nwant %+v", name, items, expression, labels, required, got, want)
	}
	return got[1]
}

// Over the HTML fixtures, the example of scrapethissite.com's countries, and
// generated documents — items nested in one another and side by side, text
// nodes a selector matches, values and labels matching none, one or several
// elements within an item, :nth-child, :contains and :has — every css rule
// with items gives the series, the failure and the logged failures it gave
// when each item was searched on its own.
func TestCSSItemsFindWhatTheyFoundItemByItem(t *testing.T) {
	label := func(expression string) model.LabelRule {
		return model.LabelRule{Name: "server", Expression: expression}
	}
	requiredLabel := func(expression string) model.LabelRule {
		return model.LabelRule{Name: "server", Expression: expression, Required: true}
	}
	static := model.LabelRule{Name: "site", Value: "a"}

	countries, err := os.ReadFile("../../testdata/html/scrapethissite-countries.html")
	if err != nil {
		t.Fatal(err)
	}
	// The example's two rules (examples/config.scrapethissite.html-test.yaml).
	sameCSSItems(t, "example", string(countries), "div.country", "span.country-population", true, requiredLabel("h3.country-name"))
	sameCSSItems(t, "example", string(countries), "div.country", "span.country-area", true, requiredLabel("h3.country-name"))

	fixtures, err := filepath.Glob("../../testdata/html/*.html")
	if err != nil || len(fixtures) == 0 {
		t.Fatalf("fixtures: %v %v", fixtures, err)
	}
	itemSelectors := []string{"tr", "tr:has(td)", "li", "ul", "div", "section", "table", "dd", "*", "body *", ":contains('1')", "tr:nth-child(2n)", "div.country"}
	valueSelectors := []string{"td:nth-child(2)", "td", "a", "span", ":first-child", "code", "dd", "td:last-child", "b", "span.country-area", "[[", ":contains('2')"}
	labelRules := [][]model.LabelRule{nil, {label("td:nth-child(1)")}, {static, requiredLabel("th")}, {label("a"), label("[[")}, {requiredLabel("h3")}}
	// The rules are not all failures, nor all series: tally counts the rules
	// that made a series and those that logged a failure.
	var withSeries, withFailures int
	tally := func(outcome cssItemsOutcome) {
		if len(outcome.series) > 0 {
			withSeries++
		}
		if len(outcome.logged) > 0 {
			withFailures++
		}
	}
	every := alloctest.UnlessRaced(3, 41)
	n := 0
	for _, fixture := range fixtures {
		body, err := os.ReadFile(fixture)
		if err != nil {
			t.Fatal(err)
		}
		for _, items := range itemSelectors {
			for _, value := range valueSelectors {
				for _, labels := range labelRules {
					if n++; n%every != 0 {
						continue
					}
					tally(sameCSSItems(t, filepath.Base(fixture), string(body), items, value, n%2 == 0, labels...))
				}
			}
		}
	}

	generated := alloctest.UnlessRaced(300, 40)
	random := rand.New(rand.NewPCG(62, 24))
	for i := range generated {
		body := generatedItemsDocument(random)
		items := []string{"div.i", ".i", "li", "div", "*:nth-child(2)", "span:contains('1')", ":contains('3')", "div > .i", "ul .i"}[random.IntN(9)]
		value := []string{".v", "span.v", "b", ":nth-child(1)", ".v:first-child", "span:contains('2')", ":contains('4')", "li:has(.v)"}[random.IntN(8)]
		labels := [][]model.LabelRule{nil, {label(".l")}, {requiredLabel("b.l")}, {static, label("li > .l")}}[random.IntN(4)]
		tally(sameCSSItems(t, fmt.Sprintf("generated %d %q", i, body), body, items, value, random.IntN(2) == 0, labels...))
	}
	if total := len(fixtures)*len(itemSelectors)*len(valueSelectors)*len(labelRules)/every + generated; withSeries < total/50 || withFailures < total/5 {
		t.Fatalf("of %d rules, %d made a series and %d logged a failure", total, withSeries, withFailures)
	}
	for _, everyLevel := range []bool{true, false} {
		sameCSSItems(t, "nested", nestedItems(3, 40, everyLevel), "div.i", "span.v", true)
		sameCSSItems(t, "nested", nestedItems(3, 40, everyLevel), "div", ":contains('3')", false, label("span"))
	}
}

// generatedItemsDocument is a small random HTML body of nested and sibling
// elements, with classes the selectors above look for, numbers, blanks and
// text.
func generatedItemsDocument(random *rand.Rand) string {
	var b strings.Builder
	var element func(depth int)
	element = func(depth int) {
		tag := []string{"div", "ul", "li", "span", "b", "p"}[random.IntN(6)]
		class := []string{"", ` class="i"`, ` class="v"`, ` class="l"`, ` class="i v"`}[random.IntN(5)]
		fmt.Fprintf(&b, "<%s%s>", tag, class)
		for range random.IntN(4) {
			switch k := random.IntN(6); {
			case k < 3 && depth < 6:
				element(depth + 1)
			case k == 3:
				b.WriteString([]string{"1", "2", "34", " ", "x", "4.5"}[random.IntN(6)])
			case k == 4:
				b.WriteString("<!-- 3 -->")
			default:
				fmt.Fprintf(&b, "%d", random.IntN(5))
			}
		}
		fmt.Fprintf(&b, "</%s>", tag)
	}
	b.WriteString("<html><body>")
	for range 1 + random.IntN(4) {
		element(0)
	}
	b.WriteString("</body></html>")
	return b.String()
}
