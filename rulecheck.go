package main

import (
	"fmt"
	"strings"
)

// Configuration validation checks everything about a metric rule that can be
// known before a scrape: that its name is a Prometheus metric name, and that
// every expression it holds compiles in the language its transform speaks.
// Compiling here also fills the expression caches (exprcache.go), so a scrape
// runs programs that already exist. A mistake is therefore reported at
// startup, on reload and by --dry-run, naming the collector, the rule and the
// label, instead of failing — or, for a CSS selector, silently matching
// nothing — on every scrape.

// The error policy vocabulary is shared by error_handling and error_mode:
// fail stops the scrape, log carries on and logs why, ignore carries on
// quietly.
const (
	ErrorPolicyFail   = ErrorModeFail
	ErrorPolicyLog    = ErrorModeLog
	ErrorPolicyIgnore = ErrorModeIgnore
	// errorPolicyWarn is the older spelling of log in error_handling. It is
	// still accepted, and reported as deprecated.
	errorPolicyWarn = "warn"
)

// normalizeErrorPolicy lower-cases a policy, maps the deprecated "warn" to
// "log" and records that it did, and rejects anything else.
func (c *Config) normalizeErrorPolicy(collector, key string, value *string) error {
	policy := strings.ToLower(strings.TrimSpace(*value))
	switch policy {
	case ErrorPolicyFail, ErrorPolicyLog, ErrorPolicyIgnore:
	case errorPolicyWarn:
		policy = ErrorPolicyLog
		c.Deprecations = append(c.Deprecations, fmt.Sprintf("collector %q %s: %q is deprecated; use %q, which means the same", collector, key, errorPolicyWarn, ErrorPolicyLog))
	default:
		return fmt.Errorf("collector %q %s has invalid value %q; want fail, log or ignore", collector, key, *value)
	}
	*value = policy
	return nil
}

func jqFamily(transformType string) bool {
	switch transformType {
	case "", "none", "jq", "yq":
		return true
	}
	return false
}

// checkMetricRule validates one rule's name and compiles its expressions.
func checkMetricRule(x *Collector, r *MetricRule) error {
	where := fmt.Sprintf("collector %q metric %q", x.Name, r.Name)
	if r.Name != "" {
		if err := checkMetricName(r.Name); err != nil {
			return fmt.Errorf("%s: %w", where, err)
		}
	}
	if r.Items != "" && !jqFamily(x.Transform.Type) {
		return fmt.Errorf("%s sets items, which only the jq and yq transforms support", where)
	}
	switch {
	case jqFamily(x.Transform.Type):
		if r.Items != "" {
			if _, err := compileJQ(r.Items); err != nil {
				return fmt.Errorf("%s items: %w", where, err)
			}
		}
		if _, err := compileJQ(r.Expression); err != nil {
			return fmt.Errorf("%s expression: %w", where, err)
		}
		for _, label := range expressionLabels(r) {
			if _, err := compileJQ(label.Expression); err != nil {
				return fmt.Errorf("%s label %q expression: %w", where, label.Name, err)
			}
		}
	case x.Transform.Type == "regex":
		re, err := compileRegex(r.Expression)
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
		if _, err := compileCSS(r.Expression); err != nil {
			return fmt.Errorf("%s CSS selector %q: %w", where, r.Expression, err)
		}
		for _, label := range expressionLabels(r) {
			if _, err := compileCSS(label.Expression); err != nil {
				return fmt.Errorf("%s label %q CSS selector %q: %w", where, label.Name, label.Expression, err)
			}
		}
	case x.Transform.Type == "xpath":
		if _, err := compileXPath(r.Expression, x.Response.Namespaces); err != nil {
			return fmt.Errorf("%s XPath %q: %w", where, r.Expression, err)
		}
		for _, label := range expressionLabels(r) {
			if strings.HasPrefix(label.Expression, "@") {
				continue
			}
			if _, err := compileXPath(label.Expression, x.Response.Namespaces); err != nil {
				return fmt.Errorf("%s label %q XPath %q: %w", where, label.Name, label.Expression, err)
			}
		}
	case x.Transform.Type == "prometheus":
		pattern := r.Expression
		if pattern == "" {
			return nil
		}
		if _, err := compileRegex(pattern); err != nil {
			return fmt.Errorf("%s expression: %w", where, err)
		}
	}
	return nil
}

// checkPrometheusTransform compiles the passthrough filters and checks the
// names the transform would give metrics and labels.
func checkPrometheusTransform(x *Collector) error {
	t := x.Transform
	if t.Type != "prometheus" {
		return nil
	}
	for key, expressions := range map[string][]string{"include": t.Include, "exclude": t.Exclude} {
		for _, expression := range expressions {
			if _, err := compileRegex(expression); err != nil {
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
		if !labelNameRE.MatchString(name) {
			return fmt.Errorf("collector %q transform.labels has invalid label name %q", x.Name, name)
		}
	}
	for from, to := range t.RenameLabels {
		if !labelNameRE.MatchString(to) {
			return fmt.Errorf("collector %q transform.rename_labels %q to invalid label name %q", x.Name, from, to)
		}
	}
	return nil
}

// checkMetricName applies the rule exposition applies at scrape time, plus
// the "__" prefix Prometheus reserves, so a name that could never be exported
// is refused before the first scrape.
func checkMetricName(name string) error {
	if !metricNameRE.MatchString(name) {
		return fmt.Errorf("%q is not a valid Prometheus metric name; use letters, digits, underscores and colons, not starting with a digit", name)
	}
	if strings.HasPrefix(name, "__") {
		return fmt.Errorf("%q starts with \"__\", which Prometheus reserves", name)
	}
	return nil
}

func expressionLabels(r *MetricRule) []LabelRule {
	var out []LabelRule
	for _, label := range r.Labels {
		if label.Type == "expression" {
			out = append(out, label)
		}
	}
	return out
}
