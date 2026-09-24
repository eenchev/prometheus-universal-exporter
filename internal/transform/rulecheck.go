package transform

import (
	"fmt"
	"strings"

	"github.com/eenchev/prometheus-universal-exporter/internal/expr"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// jqFamily reports whether a transform evaluates jq expressions against
// decoded structured data: jq and yq. Only these support items, and only these
// accept a pre-script result in place of the decoded response.
func jqFamily(transformType string) bool {
	return transformType == "jq" || transformType == "yq"
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
		// The first capture group is the value. Without one there is nothing
		// to say which part of the match is the number.
		if re.NumSubexp() == 0 {
			return fmt.Errorf("%s regex %q has no capture group; the first capture group is the value, so wrap the number in one, such as 'requests=(\\d+)'", where, r.Expression)
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
			// Without items a label selector could only match inside the
			// element whose whole text is the value, so it could only read
			// text that is part of the number.
			if r.Items == "" {
				return fmt.Errorf("%s label %q reads the response, which a css metric can do only with items: set items to the rows, such as '#servers tr:has(td)', and select the value and each label within a row", where, label.Name)
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

// CheckTransformSettings checks the collector-wide transform settings.
// include, exclude and rename pick and rename the metrics a prometheus
// transform passes through, so they apply only to one without metrics rules,
// where the rules would do both; anywhere else they are refused rather than
// ignored. The label settings apply to every transform: their label names are
// checked, and two renames to one label are refused, since which value it
// would get has no right answer.
func CheckTransformSettings(x *model.Collector) error {
	t := x.Transform
	passthrough := t.Type == "prometheus" && len(x.Metrics) == 0
	for _, setting := range []struct {
		key string
		set bool
	}{{"include", len(t.Include) > 0}, {"exclude", len(t.Exclude) > 0}, {"rename", len(t.Rename) > 0}} {
		if setting.set && !passthrough {
			return fmt.Errorf("collector %q sets transform.%s, which picks or renames the metrics a prometheus transform passes through, so it applies only to a prometheus transform without metrics rules", x.Name, setting.key)
		}
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
	targets := map[string]string{}
	for _, from := range model.SortedKeys(t.RenameLabels) {
		to := t.RenameLabels[from]
		if !model.LabelNameRE.MatchString(to) {
			return fmt.Errorf("collector %q transform.rename_labels %q to invalid label name %q", x.Name, from, to)
		}
		if other, taken := targets[to]; taken {
			return fmt.Errorf("collector %q transform.rename_labels renames both %q and %q to %q; a label can be the target of one rename", x.Name, other, from, to)
		}
		targets[to] = from
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
