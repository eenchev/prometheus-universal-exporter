package fetch

import (
	"regexp"
	"slices"
	"strings"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A jq or yq expression of a collector's rules reads the probe's parameters
// as $params, an object of each param_<name> the probe gave by its name:
// $params.queue is the value of param_queue, and null when the probe gave
// none. A parameter an expression reads is one the collector uses, as one a
// placeholder names is (CheckRequestParams), so which ones are read is found
// when the configuration loads and kept with the collector
// (model.ExpressionParams).

// expressionParam finds $params where a program reads it, with what
// follows when that is a field read by its name: .name, ."name" or
// ["name"]. \b after the name keeps $params_of_mine, another variable, out.
var expressionParam = regexp.MustCompile(`\$params\b(?:\s*(?:\.([A-Za-z_][A-Za-z0-9_]*)|\.?\[?"([^"\\]*)"\]?))?`)

// scriptParam finds params where a Python script may read it, with what
// follows when that is one parameter read by its name: params["name"] or
// params.get("name"), in either quote.
var scriptParam = regexp.MustCompile(`\bparams\b(?:\s*\[\s*(?:"([^"\\]*)"|'([^'\\]*)')\s*\]|\s*\.get\(\s*(?:"([^"\\]*)"|'([^'\\]*)'))?`)

// ParseExpressionParams is what the expressions of c's rules read of
// $params, and its Python code, the script of a python transform and a
// pre_script, of params; nil for a collector that reads nothing of
// either, as nearly every one does.
//
// A program names a variable only as it is written, so one that reads
// $params holds that text, and so does a script that reads params. Text
// that holds it and reads nothing, as a string or a comment may, or a
// script's own variable of the name, only makes the collector take a
// parameter it has no use for.
func ParseExpressionParams(c *model.Collector) *model.ExpressionParams {
	var out *model.ExpressionParams
	find := func(text, holds string, in *regexp.Regexp) {
		if !strings.Contains(text, holds) {
			return
		}
		for _, found := range in.FindAllStringSubmatch(text, -1) {
			if out == nil {
				out = &model.ExpressionParams{}
			}
			name := strings.Join(found[1:], "")
			switch {
			case name == "":
				// Read whole, or by a name the program makes.
				out.Any = true
			case !slices.Contains(out.Names, name):
				out.Names = append(out.Names, name)
			}
		}
	}
	find(c.Transform.PreScript, "params", scriptParam)
	if c.Transform.Type == "python" {
		find(c.Transform.Script, "params", scriptParam)
	}
	if c.Transform.Type != "jq" && c.Transform.Type != "yq" {
		return sortedExpressionParams(out)
	}
	read := func(expression string) { find(expression, "$params", expressionParam) }
	for i := range c.Metrics {
		rule := &c.Metrics[i]
		read(rule.Items)
		read(rule.Expression)
		read(rule.LabelsFrom)
		for j := range rule.Labels {
			read(rule.Labels[j].Expression)
		}
	}
	return sortedExpressionParams(out)
}

func sortedExpressionParams(out *model.ExpressionParams) *model.ExpressionParams {
	if out != nil {
		slices.Sort(out.Names)
	}
	return out
}

// useExpressionParams notes in used the parameters, of those a probe gave,
// that the collector's expressions read: the ones they name, and all of
// them when one reads $params otherwise.
func useExpressionParams(c *model.Collector, params map[string]string, used map[string]bool) {
	read := c.ExpressionParams
	if read == nil {
		return
	}
	for _, name := range read.Names {
		used[PathParamPrefix+name] = true
	}
	if read.Any {
		for name := range params {
			if strings.HasPrefix(name, PathParamPrefix) {
				used[name] = true
			}
		}
	}
}

// A transform reads the scrape's clock too: $scrape_time and $last_success
// in a jq or yq expression of a rule, scrape_time and last_success in a
// Python script, each the seconds since the epoch of the trip's clock
// (ScrapeClock) and of the last successful trip of the same collector,
// target and parameters, which is null, or None, while there is none.

// scriptClock finds the two names where a Python script may read them.
var scriptClock = regexp.MustCompile(`\b(scrape_time|last_success)\b`)

// TransformClockUseOf is what the transform of c reads of the scrape's
// clock, for Collector.TransformClock. As with $params, a name that is
// only text there costs the collector a clock it did not need.
func TransformClockUseOf(c *model.Collector) model.ClockUse {
	var use model.ClockUse
	note := func(name string) {
		use.Any = true
		use.Since = use.Since || strings.HasSuffix(name, "last_success")
	}
	script := func(text string) {
		if strings.Contains(text, "scrape_time") || strings.Contains(text, "last_success") {
			for _, name := range scriptClock.FindAllString(text, -1) {
				note(name)
			}
		}
	}
	script(c.Transform.PreScript)
	if c.Transform.Type == "python" {
		script(c.Transform.Script)
	}
	if c.Transform.Type != "jq" && c.Transform.Type != "yq" {
		return use
	}
	read := func(expression string) {
		for _, name := range []string{"$scrape_time", "$last_success"} {
			if strings.Contains(expression, name) {
				note(name)
			}
		}
	}
	for i := range c.Metrics {
		rule := &c.Metrics[i]
		read(rule.Items)
		read(rule.Expression)
		read(rule.LabelsFrom)
		for j := range rule.Labels {
			read(rule.Labels[j].Expression)
		}
	}
	return use
}
