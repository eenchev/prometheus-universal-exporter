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

// ParseExpressionParams is what the expressions of c's rules read of
// $params, and nil for a collector that reads nothing of it: every
// collector whose transform is not jq or yq, and nearly every one that is.
//
// A program names a variable only as it is written, so one that reads
// $params holds that text. Text that holds it and reads nothing, as a
// string or a comment may, only makes the collector take a parameter it
// has no use for.
func ParseExpressionParams(c *model.Collector) *model.ExpressionParams {
	if c.Transform.Type != "jq" && c.Transform.Type != "yq" {
		return nil
	}
	var out *model.ExpressionParams
	read := func(expression string) {
		if !strings.Contains(expression, "$params") {
			return
		}
		for _, found := range expressionParam.FindAllStringSubmatch(expression, -1) {
			if out == nil {
				out = &model.ExpressionParams{}
			}
			name := found[1] + found[2]
			switch {
			case name == "":
				// Read whole, or by a name the program makes.
				out.Any = true
			case !slices.Contains(out.Names, name):
				out.Names = append(out.Names, name)
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
