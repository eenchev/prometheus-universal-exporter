package transform

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// messagesPage is a table whose rows each fail a rule in another way: b has
// no name, c no value, and d a value that is no number. It reads the same
// as HTML and as XML.
const messagesPage = `<html><head><title>hosts</title></head><body><table id="hosts"><tbody>` +
	`<tr id="a"><td class="name">a</td><td class="v">1</td></tr>` +
	`<tr id="b"><td class="name"></td><td class="v">2</td></tr>` +
	`<tr id="c"><td class="name">c</td><td class="v"></td></tr>` +
	`<tr id="d"><td class="name">d</td><td class="v">n/a</td></tr>` +
	`</tbody></table><p id="one">x</p><p id="none"></p></body></html>`

// ruleFailureTexts are the failure of one rule over messagesPage as its
// error_mode has it told: the error that fails the transform under fail,
// and the first failure reported under log.
func ruleFailureTexts(t *testing.T, decoder, transform string, rule model.MetricRule) (failed, logged string) {
	t.Helper()
	run := func(mode string) (string, string) {
		rule.ErrorMode, rule.Type = mode, model.GaugeMetricType
		c := model.Collector{Name: "messages", Decoder: model.DecoderConfig{Type: decoder}, Transform: model.TransformConfig{Type: transform}, Metrics: []model.MetricRule{rule}}
		_, failures, err := transformWith(LeaveRuleLoggingToCaller(context.Background()), t, c, markupContentType[decoder], messagesPage)
		if err != nil {
			var failure *MetricFailure
			if !errors.As(err, &failure) || failure.Metric != rule.Name || len(failures) != 0 {
				t.Fatalf("%s %s under %s: %v, failures %v; want the rule's failure alone", transform, rule.Expression, mode, err, failures)
			}
			return err.Error(), ""
		}
		if len(failures) != 1 || failures[0].Metric != rule.Name {
			t.Fatalf("%s %s under %s: the failures %+v, want those of the rule", transform, rule.Expression, mode, failures)
		}
		return "", failures[0].First.Error()
	}
	failed, _ = run(model.ErrorModeFail)
	_, logged = run(model.ErrorModeLog)
	return failed, logged
}

// Every failure of an xpath rule names its metric first, the node it
// belongs to by its place among the nodes the rule selected, from 0, as a
// css rule's names its item, and the expression where that tells which
// selector to look at, over HTML and over XML; a css rule without items
// names its metric as one with items does. The failure reads the same
// whether it fails the probe, under fail, or is reported and logged, under
// log and ignore: an xpath rule's value that was no number, and an
// expression or a selector that did not compile, were told without the
// metric there, and without the node.
func TestXPathAndCSSFailuresNameTheMetricAndTheNode(t *testing.T) {
	host := []model.LabelRule{{Name: "host", Expression: "../td[@class='name']", Required: true}}
	cssHost := []model.LabelRule{{Name: "host", Expression: "td.name", Required: true}}
	notANumber := `value "n/a" is not a number; map text to numbers with value_map`
	for _, test := range []struct {
		name string
		// xpath is the rule over XML and over HTML, and wantXPath its
		// failure, with %s where the kind of expression is named.
		xpath     model.MetricRule
		wantXPath string
		// css is the css rule that fails the same way, where there is one,
		// and wantCSS its failure.
		css     model.MetricRule
		wantCSS string
	}{
		{
			name:      "an expression that does not compile",
			xpath:     model.MetricRule{Name: "m", Expression: "//td["},
			wantXPath: `metric "m" %s "//td[": expression must evaluate to a node-set`,
			css:       model.MetricRule{Name: "m", Expression: "td["},
			wantCSS:   `metric "m" CSS selector "td[": `,
		},
		{
			name:    "items that do not compile",
			css:     model.MetricRule{Name: "m", Items: "tr[", Expression: "td.v"},
			wantCSS: `metric "m" items CSS selector "tr[": `,
		},
		{
			name:    "a value selector within items that does not compile",
			css:     model.MetricRule{Name: "m", Items: "tr", Expression: "td["},
			wantCSS: `metric "m" CSS selector "td[": `,
		},
		{
			name:      "nothing matched",
			xpath:     model.MetricRule{Name: "m", Expression: "//td[@class='none']"},
			wantXPath: `metric "m" %s "//td[@class='none']" matched no nodes`,
			css:       model.MetricRule{Name: "m", Expression: "td.none"},
			wantCSS:   `metric "m" CSS selector "td.none" matched no nodes`,
		},
		{
			name:    "items that matched nothing",
			css:     model.MetricRule{Name: "m", Items: "tr.none", Expression: "td.v"},
			wantCSS: `metric "m" items CSS selector "tr.none" matched no nodes`,
		},
		{
			name:      "a node without a value",
			xpath:     model.MetricRule{Name: "m", Expression: "//tr[@id='a' or @id='c']/td[@class='v']"},
			wantXPath: `metric "m" value is missing for node 1: %s "//tr[@id='a' or @id='c']/td[@class='v']" selected a node without a value`,
			css:       model.MetricRule{Name: "m", Items: "#a, #c", Expression: "td.v"},
			wantCSS:   `metric "m" value is missing for item 1: CSS selector "td.v" matched an element without a value`,
		},
		{
			name:    "the one element without a value",
			css:     model.MetricRule{Name: "m", Expression: "p#none"},
			wantCSS: `metric "m" value is missing: CSS selector "p#none" matched an element without a value`,
		},
		{
			name:      "a value that is no number",
			xpath:     model.MetricRule{Name: "m", Expression: "//tr[@id='a' or @id='d']/td[@class='v']"},
			wantXPath: `metric "m" node 1: ` + notANumber,
			css:       model.MetricRule{Name: "m", Items: "#a, #d", Expression: "td.v"},
			wantCSS:   `metric "m" item 1: ` + notANumber,
		},
		{
			name:      "a required label that is missing",
			xpath:     model.MetricRule{Name: "m", Expression: "//tr[@id='a' or @id='b']/td[@class='v']", Labels: host},
			wantXPath: `metric "m" label "host" is missing for node 1`,
			css:       model.MetricRule{Name: "m", Items: "#a, #b", Expression: "td.v", Labels: cssHost},
			wantCSS:   `metric "m" label "host" is missing for item 1`,
		},
		{
			name:      "a computed value that is NaN",
			xpath:     model.MetricRule{Name: "m", Expression: "number(//tr[@id='d']/td[@class='v'])"},
			wantXPath: `metric "m" %s "number(//tr[@id='d']/td[@class='v'])" computed NaN, not a number`,
		},
		{
			name:      "a computed value that is empty",
			xpath:     model.MetricRule{Name: "m", Expression: "string(//td[@class='none'])"},
			wantXPath: `metric "m" %s "string(//td[@class='none'])" computed an empty string`,
		},
		{
			name:      "a computed value that is no number, and the one element that is none",
			xpath:     model.MetricRule{Name: "m", Expression: "string(//tr[@id='d']/td[@class='v'])"},
			wantXPath: `metric "m": ` + notANumber,
			css:       model.MetricRule{Name: "m", Expression: "#d td.v"},
			wantCSS:   `metric "m": ` + notANumber,
		},
		{
			name:      "a required label of a computed value that is missing",
			xpath:     model.MetricRule{Name: "m", Expression: "count(//tr)", Labels: []model.LabelRule{{Name: "host", Expression: "string(//td[@class='none'])", Required: true}}},
			wantXPath: `metric "m" label "host" is missing`,
		},
	} {
		same := func(where, failed, logged, want string) {
			t.Helper()
			// What a compiler says after the expression is its own.
			if strings.HasSuffix(want, ": ") && strings.HasPrefix(failed, want) && failed == logged {
				return
			}
			if failed != want || logged != want {
				t.Errorf("%s, %s: under fail\n%s\nunder log\n%s\nwant both\n%s", test.name, where, failed, logged, want)
			}
		}
		if test.xpath.Name != "" {
			for decoder, kind := range map[string]string{"xml": "XPath", "html": "HTML XPath"} {
				failed, logged := ruleFailureTexts(t, decoder, "xpath", test.xpath)
				same("xpath over "+decoder, failed, logged, strings.Replace(test.wantXPath, "%s", kind, 1))
			}
		}
		if test.css.Name != "" {
			failed, logged := ruleFailureTexts(t, "html", "css", test.css)
			same("css", failed, logged, test.wantCSS)
		}
	}
}
