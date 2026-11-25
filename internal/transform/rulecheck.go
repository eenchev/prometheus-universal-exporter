package transform

import (
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

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
// (model.JoinProblems). Each one names the rule and, if there is one, the
// expression. The rule is named by its metric name, whatever that is. The
// loader knows the rule's position among its collector's rules and checks
// it with CheckMetricRuleAt instead.
func CheckMetricRule(x *model.Collector, r *model.MetricRule) error {
	return checkMetricRule(x, r, fmt.Sprintf("collector %q metric %q", x.Name, r.Name))
}

// CheckMetricRuleAt is CheckMetricRule for the rule at index of the
// collector's metrics, named in each message as RuleWhere names it. A rule
// with a name reads as it does from CheckMetricRule. A rule without one is
// named by its position.
func CheckMetricRuleAt(x *model.Collector, index int) error {
	return checkMetricRule(x, &x.Metrics[index], RuleWhere(x, index))
}

// RuleWhere returns how a load message names the rule at index of a
// collector's metrics. Usually that is the collector and the rule's metric
// name: `collector "node" metric "up"`. A rule without a name is named by
// the collector and the rule's position among its rules, counted from 1 as
// CheckLabelValueMapsAgree counts them: `collector "node" metrics rule 2`.
// A rule has no name when name is omitted or "" (allowed for a prometheus
// rule), or is only blanks, which is never a valid rule name. `metric ""`
// did not tell anyone which of thirty rules it was. A name that is invalid
// for another reason is quoted as written, and identifies its rule.
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
		if err := checkMetricName(x, r.Name); err != nil {
			fail(fmt.Errorf("%s: %w%s", where, err, patternForAName(x, r.Name)))
		} else {
			fail(checkMetricNameLength(x, r.Name, where))
		}
	}
	if r.Items != "" && !jqFamily(x.Transform.Type) && x.Transform.Type != "css" {
		fail(fmt.Errorf("%s sets items, which only the jq, yq and css transforms support", where))
	}
	switch {
	case r.LabelsFrom == "":
	case !jqFamily(x.Transform.Type):
		fail(fmt.Errorf("%s sets labels_from, which only the jq and yq transforms support", where))
	default:
		if _, err := expr.CompileJQ(r.LabelsFrom); err != nil {
			fail(fmt.Errorf("%s labels_from %q: %w", where, r.LabelsFrom, err))
		}
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
// attribute read by its name as written rather than by the XPath engine,
// in a document of the collector's decoder. The decoder is HTML, XML, or,
// when chosen per response, either, since the one that reads it by name
// may be the one that arrives.
//
// With response.namespaces set, a decoder chosen per response is assumed to
// read XML, the only document kind where namespaces mean anything. Its
// labels are checked like those of an xml decoder. When checked as either
// kind, a label with a prefix the namespaces do not map (@x:unit,
// ../@x:kind) was accepted as an HTML attribute name. It was then silently
// dropped from every series of an XML answer, where the load had refused it
// before.
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
// include, exclude and rename select and rename the metrics a prometheus
// transform passes through. So they apply only to one without metrics
// rules, where the rules would do both. Anywhere else they are refused
// rather than ignored. The label settings apply to every transform. Their
// label names are checked, and two renames to the same label are refused,
// since there is no right answer for which value it would get.
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
		if err := checkMetricName(x, to); err != nil {
			errs = append(errs, fmt.Errorf("collector %q transform.rename %q to %q: %w", x.Name, from, to, err))
		}
	}
	for _, name := range model.SortedKeys(t.Labels) {
		if !TakesLabelNameBeforeRenames(x, name) {
			errs = append(errs, fmt.Errorf("collector %q transform.labels has invalid label name %q%s", x.Name, name, EscapingAdvice(name)))
		} else if err := CheckLabelNameBeforeRenames(x, name); err != nil {
			errs = append(errs, fmt.Errorf("collector %q transform.labels: %w", x.Name, err))
		}
	}
	targets := map[string]string{}
	for _, from := range model.SortedKeys(t.RenameLabels) {
		to := t.RenameLabels[from]
		if !TakesLabelName(x, to) {
			errs = append(errs, fmt.Errorf("collector %q transform.rename_labels %q to invalid label name %q%s", x.Name, from, to, EscapingAdvice(to)))
			continue
		}
		if err := CheckExportedLabelName(x, to); err != nil {
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

// checkFilterEntry refuses an entry, key, of transform.include or
// transform.exclude that is the empty string or only blanks. Both compile,
// and neither can be intended. The empty regular expression matches every
// name, so exclude: [""] dropped every series and include: [""] kept them
// all. An entry of blanks matches only names that contain those blanks,
// which a UTF-8 name may, so it has a meaning. But written bare it is a
// slip: an empty string that picked up spaces, or a template that filled in
// nothing. A pass-through then exported nothing, with no error. A list
// entry is not an optional key: "" there does not mean the key is omitted.
//
// The entries are matched as applyPrometheusTransform matches them: against
// the series name in the target's answer, before transform.rename and
// metrics_prefix, anywhere in the name.
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

// checkRulePattern refuses the expression of a prometheus rule (named by
// where) that is only blanks. A prometheus rule's expression is a regular
// expression matched anywhere in the names of the target's metrics
// (applyPrometheusTransform, decode.prometheusKeeps). An expression of
// blanks compiles but matches only names that contain those blanks. So the
// rule passed nothing on, and with required: false it did so silently.
// This is the same slip checkFilterEntry refuses in transform.include, and
// it gets the same advice. An expression written "" means the key is
// omitted, as for any optional key: the rule then matches the metric of its
// name. Under the other transforms the loader refuses an expression of
// blanks before this check, as a rule with no expression.
func checkRulePattern(where, pattern string) error {
	if strings.TrimSpace(pattern) != "" {
		return nil
	}
	return fmt.Errorf("%s expression %q is nothing but blanks; a prometheus rule's expression is a regular expression matched against a metric's name as the target gives it, anywhere in it, so this one matches only the names that hold these blanks: write the pattern that was meant, or, for one that does mean a blank, '[ ]' or '\\x20' in single quotes, or leave expression out for the rule to pass on the metric its name names", where, pattern)
}

// A name written in a collector's configuration is held at load to the same
// rules as at a scrape. This covers a rule's name and its labels' names,
// the keys of transform.labels, and the targets of transform.rename and
// transform.rename_labels. At a scrape these names leave the transform next
// to the names from the response and are escaped with them, in one place
// (escapeNames). Under name_escaping fail, the default, a name that is not
// classic would fail every scrape. So the load refuses it and says, as the
// scrape does, that underscores or values would export it escaped. Under
// those two it loads with any characters and is exported the way each
// exports a response's name: a rule named http.server.duration makes
// http_server_duration, or U__http_2e_server_2e_duration.
//
// The load holds a written name to two rules that a scrape does not apply
// to a response's names:
//   - A name of only blanks is not a name under any of the three, just as an
//     expression of blanks is not an expression. It is neither the omitted
//     key (only "" is) nor a name anyone meant.
//   - A metric name is not exported starting with "__", which Prometheus
//     reserves. This applies to the name as written and as underscores
//     escapes it, where a leading digit or dot becomes "_". The scrape also
//     holds a label's name to this rule.

// escapes reports whether the collector exports a non-classic name escaped,
// because its name_escaping is underscores or values. Under fail, or with
// the key omitted, such a name fails the scrape.
//
// Under metric_name_validation_scheme utf8 such a name is also exported,
// unchanged, and each answer escapes it as its scraper requests.
func escapes(x *model.Collector) bool {
	return UTF8Names(x) || x.NameEscaping == NameEscapingUnderscores || x.NameEscaping == NameEscapingValues
}

// EscapingAdvice is the text the load adds when it refuses a non-classic
// name. It uses the words of the scrape's refusal of such a name
// (model.MetricSet.Validate), and only for names they apply to: valid
// UTF-8, which escaping exports. A name of blanks is refused under every
// name_escaping and gets no advice.
func EscapingAdvice(name string) string {
	if strings.TrimSpace(name) == "" || !utf8.ValidString(name) {
		return ""
	}
	return "; set the collector's name_escaping to underscores or values to export it escaped"
}

// TakesLabelName reports whether a label name written in the collector's
// configuration is one that a scrape of the collector exports: a classic
// name, or, under name_escaping underscores or values, any name that is not
// only blanks. An accepted name may still be one Prometheus reserves
// (CheckExportedLabelName).
func TakesLabelName(x *model.Collector, name string) bool {
	return model.ValidLabelName(name) || escapes(x) && strings.TrimSpace(name) != ""
}

// CheckExportedLabelName refuses a label name the collector accepts
// (TakesLabelName) that is exported as a name Prometheus reserves, starting
// with "__". That is a classic name written that way, refused in the usual
// words, or a name that underscores escapes to one. The scrape would refuse
// every series with that label. It also refuses a name exported longer than
// the collector's limits.max_label_name_length, which the scrape would
// refuse for every such series too. The length is that of the escaped
// name, which is what the scrape measures.
func CheckExportedLabelName(x *model.Collector, name string) error {
	exported := escapeName(name, x.NameEscaping, false)
	switch {
	case exported == name && UTF8Names(x) && !model.ValidLabelName(name):
		if model.ReservedLabelName(name) {
			return fmt.Errorf("label name %q starts with __, which Prometheus reserves for its own labels", name)
		}
	case exported == name:
		if err := model.CheckLabelName(name); err != nil {
			return err
		}
	case model.ReservedLabelName(exported):
		return fmt.Errorf("label name %q is exported as %q under name_escaping %s, which starts with __, which Prometheus reserves for its own labels", name, exported, x.NameEscaping)
	}
	return checkLabelNameLength(x, name, exported)
}

// A label name a series has before the collector's transform.remove_labels
// and rename_labels apply (applyCollectorLabels), such as a rule's label or
// a transform.labels key, is exported under that name only if neither
// removes it. The scrape escapes the names and validates the series after
// both (Transform). So a name that one of them removes is neither escaped
// nor validated. Its characters, the reserved "__" prefix and its length do
// not matter to the scrape. The scrape holds the rename's target to all
// three, and so does the load (CheckTransformSettings). The renames do not
// chain. A transform.labels key is added before the removals, so a key
// named in remove_labels is removed like a rule's label. A name of only
// blanks is still refused, as the load refuses it everywhere: nobody meant
// it as a name.

// TakesLabelNameBeforeRenames is TakesLabelName of a label name a series has
// before remove_labels and rename_labels: one either takes off is taken
// whatever its characters, unless it is blanks alone.
func TakesLabelNameBeforeRenames(x *model.Collector, name string) bool {
	return TakesLabelName(x, name) || takenOffByRenames(x, name) && strings.TrimSpace(name) != ""
}

// CheckLabelNameBeforeRenames is CheckExportedLabelName of a label name a
// series has before remove_labels and rename_labels, which refuses nothing
// of a name either takes off.
func CheckLabelNameBeforeRenames(x *model.Collector, name string) error {
	if takenOffByRenames(x, name) {
		return nil
	}
	return CheckExportedLabelName(x, name)
}

// takenOffByRenames reports whether the collector's transform.rename_labels
// renames the label name or its remove_labels removes it.
func takenOffByRenames(x *model.Collector, name string) bool {
	_, renamed := x.Transform.RenameLabels[name]
	return renamed || slices.Contains(x.Transform.RemoveLabels, name)
}

// checkLabelNameLength refuses a label name written in the collector's
// configuration that is exported, as exported, longer than
// limits.max_label_name_length.
func checkLabelNameLength(x *model.Collector, name, exported string) error {
	limit := x.Limits.MaxLabelNameLength
	if limit <= 0 || len(exported) <= limit {
		return nil
	}
	if exported == name {
		return fmt.Errorf("label name %q is %d bytes, longer than limits.max_label_name_length %d; shorten it or raise limits.max_label_name_length", name, len(name), limit)
	}
	return fmt.Errorf("label name %q is exported as %q under name_escaping %s, %d bytes, longer than limits.max_label_name_length %d; shorten it or raise limits.max_label_name_length", name, exported, x.NameEscaping, len(exported), limit)
}

// checkMetricName applies the rule exposition applies at scrape time under
// the collector's name_escaping, plus the "__" prefix Prometheus reserves, so
// a name that could never be exported is refused before the first scrape.
func checkMetricName(x *model.Collector, name string) error {
	if !model.ValidMetricName(name) && (!escapes(x) || strings.TrimSpace(name) == "") {
		advice := EscapingAdvice(name)
		if advice != "" {
			advice = ", or" + strings.TrimPrefix(advice, ";")
		}
		return fmt.Errorf("%q is not a valid Prometheus metric name; use letters, digits, underscores and colons, not starting with a digit%s", name, advice)
	}
	if strings.HasPrefix(name, "__") {
		return fmt.Errorf("%q starts with \"__\", which Prometheus reserves", name)
	}
	if exported := escapeName(name, x.NameEscaping, true); strings.HasPrefix(exported, "__") {
		return fmt.Errorf("%q is exported as %q under name_escaping %s, which starts with \"__\", which Prometheus reserves", name, exported, x.NameEscaping)
	}
	return nil
}

// checkMetricNameLength refuses a rule's name (the rule named by where)
// that the collector exports longer than limits.max_metric_name_length.
// Every series with that name would fail the scrape's validation. That
// validation measures the exported name, escaped under the collector's
// name_escaping. It measures only that name: a histogram's or summary's
// family name, not the _bucket, _sum and _count names of its samples. Every
// transform exports a rule's series under the rule's name.
// transform.rename applies only to a pass-through without rules, and a
// python rule's name is the name of the script's series it covers. For a
// collector with a metrics_prefix, ValidateMetricsPrefix checks the
// prefixed name, in its own words, before the rules.
func checkMetricNameLength(x *model.Collector, name, where string) error {
	limit := x.Limits.MaxMetricNameLength
	if x.MetricsPrefix != "" || limit <= 0 {
		return nil
	}
	const advice = "so every series of that name would fail validation; shorten the name or raise limits.max_metric_name_length"
	exported := ExportedMetricName(x, name)
	switch {
	case len(exported) <= limit:
		return nil
	case exported == name:
		return fmt.Errorf("%s is %d bytes, longer than limits.max_metric_name_length %d, %s", where, len(name), limit, advice)
	default:
		return fmt.Errorf("%s is exported as %q under name_escaping %s, %d bytes, longer than limits.max_metric_name_length %d, %s", where, exported, x.NameEscaping, len(exported), limit, advice)
	}
}

// patternForAName is the text the load adds when it refuses a prometheus
// rule's name that is not a metric name and contains a character with a
// meaning in regular expressions: . * + ? ^ $ | ( ) [ ] { } and the
// backslash. Under prometheus a rule selects the target's metrics by a
// pattern. So name: 'node_.*' or name: '^up$' is almost surely a pattern
// under the wrong key, and "use letters, digits, underscores and colons"
// would lead its author to break it. Nothing is added for:
//   - a valid metric name, which contains none of these characters;
//   - a name refused for another reason: a dash, a blank, a leading digit,
//     or the "__" Prometheus reserves;
//   - a rule of any other transform, which has no pattern to confuse.
//
// The advice applies only to a name the load refuses. Under name_escaping
// underscores or values, a name with such a character is not refused,
// unless underscores would export it as a name Prometheus reserves. A dot
// belongs to http.server.duration as much as to a pattern, and there the
// rule passes on the metric of exactly that name, as a rule with a classic
// name does (applyPrometheusTransform quotes the name it matches by). Under
// fail the name is refused and gets both pieces of advice: what would
// export it, and where a pattern belongs.
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
