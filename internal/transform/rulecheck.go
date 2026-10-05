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

// CheckMetricRule validates one rule's name and compiles its expressions. It
// reports every mistake it finds in the rule, not just the first
// (model.JoinProblems), each naming the rule and, where there is one, the
// expression. The rule is named by its metric name, whatever that is: the
// loader, which knows the rule's place among its collector's, checks it with
// CheckMetricRuleAt.
func CheckMetricRule(x *model.Collector, r *model.MetricRule) error {
	return checkMetricRule(x, r, fmt.Sprintf("collector %q metric %q", x.Name, r.Name))
}

// CheckMetricRuleAt is CheckMetricRule of the rule at index of the
// collector's metrics, named in each message as RuleWhere names it: a rule
// that has a name reads as it does from CheckMetricRule, and one that has
// none is told of by its place.
func CheckMetricRuleAt(x *model.Collector, index int) error {
	return checkMetricRule(x, &x.Metrics[index], RuleWhere(x, index))
}

// RuleWhere is how a message of the load names the rule at index of a
// collector's metrics: the collector and the rule's metric name, `collector
// "node" metric "up"`, or, for a rule that has no name to be named by, the
// collector and the rule's place among its rules, counted from 1 as
// CheckLabelValueMapsAgree counts them, `collector "node" metrics rule 2`.
// A rule has no name when name is left out or "", which a prometheus rule
// may be, or is nothing but blanks, which is no rule's name: `metric ""`
// said which rule of thirty it was to nobody. A name that is no metric's
// for another reason is quoted as it is written, and finds its rule.
func RuleWhere(x *model.Collector, index int) string {
	return fmt.Sprintf("collector %q %s", x.Name, RuleName(&x.Metrics[index], index))
}

// RuleName is RuleWhere without the collector: `metric "up"`, or `metrics
// rule 2` for the rule at index that has no name.
func RuleName(r *model.MetricRule, index int) string {
	if strings.TrimSpace(r.Name) == "" {
		return fmt.Sprintf("metrics rule %d", index+1)
	}
	return fmt.Sprintf("metric %q", r.Name)
}

// checkMetricRule is CheckMetricRule with the rule named by where.
func checkMetricRule(x *model.Collector, r *model.MetricRule, where string) error {
	var errs []error
	fail := func(err error) {
		if err != nil {
			errs = append(errs, err)
		}
	}
	if r.Name != "" {
		if err := checkMetricName(r.Name); err != nil {
			fail(fmt.Errorf("%s: %w%s", where, err, patternForAName(x, r.Name)))
		}
	}
	if r.Items != "" && !jqFamily(x.Transform.Type) && x.Transform.Type != "css" {
		fail(fmt.Errorf("%s sets items, which only the jq, yq and css transforms support", where))
	}
	fail(checkValueRules(x, r, where))
	fail(checkTimeRules(x, r, where))
	fail(checkLabelValueMaps(x, r, where))
	switch {
	case jqFamily(x.Transform.Type):
		if r.Items != "" {
			if _, err := expr.CompileJQ(r.Items); err != nil {
				fail(fmt.Errorf("%s items %q: %w", where, r.Items, err))
			}
		}
		if _, err := expr.CompileJQ(r.Expression); err != nil {
			fail(fmt.Errorf("%s expression %q: %w", where, r.Expression, err))
		}
		for _, label := range expressionLabels(r) {
			if _, err := expr.CompileJQ(label.Expression); err != nil {
				fail(fmt.Errorf("%s label %q expression %q: %w", where, label.Name, label.Expression, err))
			}
		}
	case x.Transform.Type == "regex":
		re, err := expr.CompileRegex(r.Expression)
		if err != nil {
			fail(fmt.Errorf("%s regex %q: %w", where, r.Expression, err))
			break
		}
		// The capture group named value is the value, or else the first.
		// Without one there is nothing to say which part of the match is
		// the number, and with two named value, which of them.
		if re.NumSubexp() == 0 {
			fail(fmt.Errorf("%s regex %q has no capture group; the value is the capture group named value, as in '(?P<value>\\d+)', or else the first, so wrap the number in one, such as 'requests=(\\d+)'", where, r.Expression))
		}
		names := re.SubexpNames()
		if named := countOf(names, regexValueName); named > 1 {
			fail(fmt.Errorf("%s regex %q has %d capture groups named %s; the group of that name is the value, so the regex can have one: name the others something else, or leave them unnamed", where, r.Expression, named, regexValueName))
		}
		for _, label := range expressionLabels(r) {
			if index := captureIndex(label.Expression, names); index < 0 || index >= len(names) {
				fail(fmt.Errorf("%s label %q refers to capture group %q, which the regex does not have", where, label.Name, label.Expression))
			}
		}
	case x.Transform.Type == "css":
		if r.Items != "" {
			if _, err := expr.CompileCSS(r.Items); err != nil {
				fail(fmt.Errorf("%s items CSS selector %q: %w", where, r.Items, err))
			}
		}
		if _, err := expr.CompileCSS(r.Expression); err != nil {
			fail(fmt.Errorf("%s CSS selector %q: %w", where, r.Expression, err))
		}
		for _, label := range expressionLabels(r) {
			if _, err := expr.CompileCSS(label.Expression); err != nil {
				fail(fmt.Errorf("%s label %q CSS selector %q: %w", where, label.Name, label.Expression, err))
			}
			// Without items a label selector could only match inside the
			// element whose whole text is the value, so it could only read
			// text that is part of the number.
			if r.Items == "" {
				fail(fmt.Errorf("%s label %q reads the response, which a css metric can do only with items: set items to the rows, such as '#servers tr:has(td)', and select the value and each label within a row", where, label.Name))
			}
		}
	case x.Transform.Type == "xpath":
		// xml is bound without being mapped, as XML binds it.
		namespaces := boundNamespaces(x.Response.Namespaces)
		if _, err := expr.CompileXPath(r.Expression, namespaces); err != nil {
			fail(fmt.Errorf("%s XPath %q: %w", where, r.Expression, err))
		}
		for _, label := range expressionLabels(r) {
			// An attribute read from the node by its name as written is
			// not compiled (ownAttributeLabel, xpathLabelShape); anything
			// else is XPath, with the collector's namespaces.
			if xpathLabelReadByName(label.Expression, x.Decoder.Type, namespaces) {
				continue
			}
			if _, err := expr.CompileXPath(label.Expression, namespaces); err != nil {
				// A label only HTML reads, of a collector taken to read XML
				// for its namespaces alone: the way out is not in the
				// XPath error.
				hint := ""
				if x.Decoder.Type != "xml" && x.Decoder.Type != "html" && len(namespaces) > 0 && xpathLabelReadByName(label.Expression, "html", nil) {
					hint = "; response.namespaces is set, so the labels are checked as those of an XML document: if the target answers HTML, where this label is an attribute's name as written, set decoder.type to html"
				}
				fail(fmt.Errorf("%s label %q XPath %q: %w%s", where, label.Name, label.Expression, err, hint))
			}
		}
	case x.Transform.Type == "prometheus":
		if pattern := r.Expression; pattern != "" {
			fail(checkRulePattern(where, pattern))
			if _, err := expr.CompileRegex(pattern); err != nil {
				fail(fmt.Errorf("%s expression %q: %w", where, pattern, err))
			}
		}
	}
	return model.JoinProblems(errs...)
}

// xpathLabelReadByName reports whether a label of an xpath collector is an
// attribute read by its name as written rather than by the XPath engine, in
// a document of the collector's decoder: HTML, XML, or, for a decoder left
// to each response, either, since the one that reads it by name may be the
// one that arrives.
//
// With response.namespaces set, a decoder left to each response is taken to
// read XML, the one kind of document namespaces mean anything in: its labels
// are checked as those of an xml decoder are. Checked as either kind's, a
// label with a prefix the namespaces do not map — @x:unit, ../@x:kind — was
// accepted as the name of an HTML attribute, and then left off every series
// of an XML answer without a word, where the load had refused it before.
func xpathLabelReadByName(expression, decoder string, namespaces map[string]string) bool {
	if decoder == "html" || (decoder != "xml" && len(namespaces) == 0) {
		if _, own := ownAttributeLabel(expression, true, namespaces); own {
			return true
		}
		if kind, _, _, walked := xpathLabelShape(expression, true); walked && kind == xpathLabelAttribute {
			return true
		}
	}
	if decoder != "html" {
		if _, own := ownAttributeLabel(expression, false, namespaces); own {
			return true
		}
	}
	return false
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
	var errs []error
	passthrough := t.Type == "prometheus" && len(x.Metrics) == 0
	for _, setting := range []struct {
		key string
		set bool
	}{{"include", len(t.Include) > 0}, {"exclude", len(t.Exclude) > 0}, {"rename", len(t.Rename) > 0}} {
		if setting.set && !passthrough {
			errs = append(errs, fmt.Errorf("collector %q sets transform.%s, which picks or renames the metrics a prometheus transform passes through, so it applies only to a prometheus transform without metrics rules", x.Name, setting.key))
		}
	}
	for _, setting := range []struct {
		key         string
		expressions []string
	}{{"include", t.Include}, {"exclude", t.Exclude}} {
		for _, expression := range setting.expressions {
			if err := checkFilterEntry(x.Name, setting.key, expression); err != nil {
				errs = append(errs, err)
				continue
			}
			if _, err := expr.CompileRegex(expression); err != nil {
				errs = append(errs, fmt.Errorf("collector %q transform.%s %q: %w", x.Name, setting.key, expression, err))
			}
		}
	}
	for _, from := range model.SortedKeys(t.Rename) {
		to := t.Rename[from]
		if err := checkMetricName(to); err != nil {
			errs = append(errs, fmt.Errorf("collector %q transform.rename %q to %q: %w", x.Name, from, to, err))
		}
	}
	for _, name := range model.SortedKeys(t.Labels) {
		if !model.ValidLabelName(name) {
			errs = append(errs, fmt.Errorf("collector %q transform.labels has invalid label name %q", x.Name, name))
		} else if err := model.CheckLabelName(name); err != nil {
			errs = append(errs, fmt.Errorf("collector %q transform.labels: %w", x.Name, err))
		}
	}
	targets := map[string]string{}
	for _, from := range model.SortedKeys(t.RenameLabels) {
		to := t.RenameLabels[from]
		if !model.ValidLabelName(to) {
			errs = append(errs, fmt.Errorf("collector %q transform.rename_labels %q to invalid label name %q", x.Name, from, to))
			continue
		}
		if err := model.CheckLabelName(to); err != nil {
			errs = append(errs, fmt.Errorf("collector %q transform.rename_labels %q: %w", x.Name, from, err))
			continue
		}
		if other, taken := targets[to]; taken {
			errs = append(errs, fmt.Errorf("collector %q transform.rename_labels renames both %q and %q to %q; a label can be the target of one rename", x.Name, other, from, to))
			continue
		}
		targets[to] = from
	}
	return model.JoinProblems(errs...)
}

// checkFilterEntry refuses an entry of transform.include or transform.exclude,
// key, that is the empty string or nothing but blanks. Each compiles, and
// neither can be meant. The empty regular expression matches every name, so
// exclude: [""] dropped every series and include: [""] kept every one. An
// entry of blanks matches only the names that hold those blanks, which a
// UTF-8 name may, so it has a meaning, but written bare it is a slip — an
// empty string that picked up spaces, a template that filled in nothing —
// after which a pass-through exported nothing, with no error. An entry of a
// list is not an optional key: written "" it is not the key left out.
//
// The entries are matched as applyPrometheusTransform matches them: against
// the name a series has in the target's answer, before transform.rename and
// metrics_prefix, and anywhere in it.
func checkFilterEntry(collector, key, entry string) error {
	const entryIs = "an entry is a regular expression matched against a metric's name as the target gives it, anywhere in it"
	switch {
	case entry == "":
		return fmt.Errorf("collector %q transform.%s has an entry that is the empty string; %s, so the empty one matches every name: write '.*' to match every name, or leave transform.%s out to filter nothing", collector, key, entryIs, key)
	case strings.TrimSpace(entry) == "":
		return fmt.Errorf("collector %q transform.%s entry %q is nothing but blanks; %s, so this one matches only the names that hold these blanks: write the pattern that was meant, or, for one that does mean a blank, '[ ]' or '\\x20' in single quotes, or leave transform.%s out to filter nothing", collector, key, entry, entryIs, key)
	}
	return nil
}

// checkRulePattern refuses the expression of a prometheus rule, the rule
// being named by where, that is nothing but blanks. A prometheus rule's
// expression is a regular expression matched against the names of the
// target's metrics, anywhere in a name (applyPrometheusTransform,
// decode.prometheusKeeps), and one of blanks compiles: it matches only the
// names that hold those blanks, so the rule passed nothing on, which with
// required: false it did without a word. It is the slip checkFilterEntry
// refuses of an entry of transform.include, and is refused with the same
// advice. An expression written "" is the key left out, as an optional key
// is everywhere: the rule then matches the metric of its name. Under the
// other transforms the loader refuses an expression of blanks before this
// check, as a rule that has no expression.
func checkRulePattern(where, pattern string) error {
	if strings.TrimSpace(pattern) != "" {
		return nil
	}
	return fmt.Errorf("%s expression %q is nothing but blanks; a prometheus rule's expression is a regular expression matched against a metric's name as the target gives it, anywhere in it, so this one matches only the names that hold these blanks: write the pattern that was meant, or, for one that does mean a blank, '[ ]' or '\\x20' in single quotes, or leave expression out for the rule to pass on the metric its name names", where, pattern)
}

// checkMetricName applies the rule exposition applies at scrape time, plus
// the "__" prefix Prometheus reserves, so a name that could never be exported
// is refused before the first scrape.
func checkMetricName(name string) error {
	if !model.ValidMetricName(name) {
		return fmt.Errorf("%q is not a valid Prometheus metric name; use letters, digits, underscores and colons, not starting with a digit", name)
	}
	if strings.HasPrefix(name, "__") {
		return fmt.Errorf("%q starts with \"__\", which Prometheus reserves", name)
	}
	return nil
}

// patternForAName is what the load adds to its refusal of a prometheus
// rule's name that is no metric name and holds a character a regular
// expression gives a meaning: . * + ? ^ $ | ( ) [ ] { } and the backslash.
// Under prometheus, where a rule picks the target's metrics by a pattern,
// name: 'node_.*' or name: '^up$' is almost surely one, written under the
// wrong key, and "use letters, digits, underscores and colons" sends its
// author to spoil it. A name that is one, and so holds none of them, a name
// refused for anything else — a dash, a blank, a leading digit, the "__"
// Prometheus reserves — and a name of any other transform's rule, which has
// no pattern to be mistaken for, add nothing.
//
// A rule's name is held to the classic names whatever the collector's
// name_escaping is, which is about the names a response or a script gives:
// name: http.server.duration is refused under every one, and under
// prometheus it is told the same, the metric of that name being matched by
// expression: '^http\.server\.duration$'.
func patternForAName(x *model.Collector, name string) string {
	if x.Transform.Type != "prometheus" || model.ValidMetricName(name) || !strings.ContainsAny(name, `.*+?^$|()[]{}\`) {
		return ""
	}
	return "; a pattern to match the target's metric names by is a prometheus rule's expression, not its name, so if this is one, write it as expression, in single quotes, and leave name out, or set name to the one name the series it matches are to be exported under"
}

// countOf is how many of names are name.
func countOf(names []string, name string) int {
	count := 0
	for _, other := range names {
		if other == name {
			count++
		}
	}
	return count
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
