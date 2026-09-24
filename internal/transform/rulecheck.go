package transform

import (
	"fmt"
	"strings"

	"github.com/eenchev/prometheus-universal-exporter/internal/expr"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

func jqFamily(transformType string) bool {
	switch transformType {
	case "", "none", "jq", "yq":
		return true
	}
	return false
}

// CheckMetricRule validates one rule's name and compiles its expressions.
func CheckMetricRule(x *model.Collector, r *model.MetricRule) error {
	where := fmt.Sprintf("collector %q metric %q", x.Name, r.Name)
	if r.Name != "" {
		if err := checkMetricName(r.Name); err != nil {
			return fmt.Errorf("%s: %w", where, err)
		}
	}
	if r.Items != "" && !jqFamily(x.Transform.Type) && x.Transform.Type != "css" {
		return fmt.Errorf("%s sets items, which only the jq, yq and css transforms support", where)
	}
	switch {
	case jqFamily(x.Transform.Type):
		if r.Items != "" {
			if _, err := expr.CompileJQ(r.Items); err != nil {
				return fmt.Errorf("%s items: %w", where, err)
			}
		}
		if _, err := expr.CompileJQ(r.Expression); err != nil {
			return fmt.Errorf("%s expression: %w", where, err)
		}
		for _, label := range expressionLabels(r) {
			if _, err := expr.CompileJQ(label.Expression); err != nil {
				return fmt.Errorf("%s label %q expression: %w", where, label.Name, err)
			}
		}
	case x.Transform.Type == "regex":
		re, err := expr.CompileRegex(r.Expression)
		if err != nil {
			return fmt.Errorf("%s regex: %w", where, err)
		}
		names := re.SubexpNames()
		for _, label := range expressionLabels(r) {
			if index := captureIndex(label.Expression, names); index < 0 || index >= len(names) {
				return fmt.Errorf("%s label %q refers to capture group %q, which the regex does not have", where, label.Name, label.Expression)
			}
		}
	case x.Transform.Type == "css":
		if r.Items != "" {
			if _, err := expr.CompileCSS(r.Items); err != nil {
				return fmt.Errorf("%s items CSS selector %q: %w", where, r.Items, err)
			}
		}
		if _, err := expr.CompileCSS(r.Expression); err != nil {
			return fmt.Errorf("%s CSS selector %q: %w", where, r.Expression, err)
		}
		for _, label := range expressionLabels(r) {
			if _, err := expr.CompileCSS(label.Expression); err != nil {
				return fmt.Errorf("%s label %q CSS selector %q: %w", where, label.Name, label.Expression, err)
			}
		}
	case x.Transform.Type == "xpath":
		if _, err := expr.CompileXPath(r.Expression, x.Response.Namespaces); err != nil {
			return fmt.Errorf("%s XPath %q: %w", where, r.Expression, err)
		}
		for _, label := range expressionLabels(r) {
			if strings.HasPrefix(label.Expression, "@") {
				continue
			}
			if _, err := expr.CompileXPath(label.Expression, x.Response.Namespaces); err != nil {
				return fmt.Errorf("%s label %q XPath %q: %w", where, label.Name, label.Expression, err)
			}
		}
	case x.Transform.Type == "prometheus":
		pattern := r.Expression
		if pattern == "" {
			return nil
		}
		if _, err := expr.CompileRegex(pattern); err != nil {
			return fmt.Errorf("%s expression: %w", where, err)
		}
	}
	return nil
}

// CheckPrometheusTransform compiles the passthrough filters and checks the
// names the transform would give metrics and labels.
func CheckPrometheusTransform(x *model.Collector) error {
	t := x.Transform
	if t.Type != "prometheus" {
		return nil
	}
	for key, expressions := range map[string][]string{"include": t.Include, "exclude": t.Exclude} {
		for _, expression := range expressions {
			if _, err := expr.CompileRegex(expression); err != nil {
				return fmt.Errorf("collector %q transform.%s %q: %w", x.Name, key, expression, err)
			}
		}
	}
	for from, to := range t.Rename {
		if err := checkMetricName(to); err != nil {
			return fmt.Errorf("collector %q transform.rename %q to %q: %w", x.Name, from, to, err)
		}
	}
	for name := range t.Labels {
		if !model.LabelNameRE.MatchString(name) {
			return fmt.Errorf("collector %q transform.labels has invalid label name %q", x.Name, name)
		}
	}
	for from, to := range t.RenameLabels {
		if !model.LabelNameRE.MatchString(to) {
			return fmt.Errorf("collector %q transform.rename_labels %q to invalid label name %q", x.Name, from, to)
		}
	}
	return nil
}

// checkMetricName applies the rule exposition applies at scrape time, plus
// the "__" prefix Prometheus reserves, so a name that could never be exported
// is refused before the first scrape.
func checkMetricName(name string) error {
	if !model.MetricNameRE.MatchString(name) {
		return fmt.Errorf("%q is not a valid Prometheus metric name; use letters, digits, underscores and colons, not starting with a digit", name)
	}
	if strings.HasPrefix(name, "__") {
		return fmt.Errorf("%q starts with \"__\", which Prometheus reserves", name)
	}
	return nil
}

func expressionLabels(r *model.MetricRule) []model.LabelRule {
	var out []model.LabelRule
	for _, label := range r.Labels {
		if !label.Static() {
			out = append(out, label)
		}
	}
	return out
}
