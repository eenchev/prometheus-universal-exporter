package config

import (
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/transform"
)

// The checks of a collector's rules as they were, each a copy kept as an
// oracle for the differential tests of what the load says of rules
// (samerule_test.go, ruleplace_test.go).

// validateMetricRulesBeforeSameRules is validateMetricRules as it was before
// two rules that are the same rule were refused and before a rule without a
// name was named by its place, kept as an oracle with the checks of one rule
// it called, as they were. checkPrometheusRuleSelects and
// transform.CheckLabelValueMapsAgree, which it calls as they are, did not
// change, and transform.CheckMetricRule names a rule as it did: what was
// added to its refusal of a prometheus rule's name that reads as a pattern
// is held to what was in the transform package
// (TestOnlyAPatternUnderAPrometheusRulesNameReadsDifferently).
func validateMetricRulesBeforeSameRules(c *model.Config, x *model.Collector) error {
	var errs []error
	for i := range x.Metrics {
		err := validateMetricRuleBeforeSameRules(x, &x.Metrics[i])
		if err == nil {
			err = checkPrometheusRuleSelects(x, i)
		}
		errs = append(errs, err)
	}
	errs = append(errs, transform.CheckLabelValueMapsAgree(x))
	return model.JoinProblems(errs...)
}

// validateMetricRuleBeforeSameRules is validateMetricRule as it was.
func validateMetricRuleBeforeSameRules(x *model.Collector, r *model.MetricRule) error {
	// Whether the rule wrote a type and an error_mode is asked before their
	// defaults are filled in, after which a key left out reads as one that
	// was written (checkPythonRule).
	wroteType, wroteErrorMode := r.Type != "", r.ErrorMode != ""
	if r.ErrorMode == "" {
		r.ErrorMode = model.ErrorModeLog
	}
	if err := normalizeErrorPolicy(x.Name, fmt.Sprintf("metric %q error_mode", r.Name), &r.ErrorMode); err != nil {
		return err
	}
	// A prometheus transform's rule without a type keeps the type of
	// the series it passes through: a counter stays a counter, a
	// histogram a histogram. Every other rule makes its own samples,
	// gauges unless it says otherwise.
	if r.Type == "" && x.Transform.Type != "prometheus" {
		r.Type = model.GaugeMetricType
	}
	switch r.Type {
	case model.GaugeMetricType, model.CounterMetricType, model.UntypedMetricType:
	case "":
	case model.HistogramMetricType, model.SummaryMetricType:
		// Only a series that is one already has buckets or quantiles to
		// expose; a rule reading one number would expose a histogram
		// with a single plain sample, which no parser accepts.
		if x.Transform.Type != "prometheus" {
			return fmt.Errorf("collector %q metric %q has type %s, which only a prometheus transform can give, passing through a %s that has its buckets or quantiles; a %s rule reads one value, so use gauge, counter or untyped", x.Name, r.Name, r.Type, r.Type, x.Transform.Type)
		}
	default:
		return fmt.Errorf("collector %q metric %q has invalid type %q", x.Name, r.Name, r.Type)
	}
	if strings.TrimSpace(r.Name) == "" && x.Transform.Type != "prometheus" && x.Transform.Type != "python" {
		return fmt.Errorf("collector %q has a metric without a name", x.Name)
	}
	if strings.TrimSpace(r.Expression) == "" && x.Transform.Type != "python" && x.Transform.Type != "prometheus" {
		return fmt.Errorf("collector %q metric %q has no expression", x.Name, r.Name)
	}
	for _, label := range r.Labels {
		if strings.TrimSpace(label.Name) == "" {
			return fmt.Errorf("collector %q metric %q has a label without a name", x.Name, r.Name)
		}
		if !namePattern.MatchString(label.Name) {
			return fmt.Errorf("collector %q metric %q has invalid label name %q", x.Name, r.Name, label.Name)
		}
		if err := model.CheckLabelName(label.Name); err != nil {
			return fmt.Errorf("collector %q metric %q: %w", x.Name, r.Name, err)
		}
		// An expression written as nothing but blanks is neither the key left
		// out, which only "" is, nor anything to read a label with. The label
		// is not static then (model.LabelRule.Static), so a value beside it
		// was never exported, while a csv rule read the column of that name
		// and a prometheus rule the source label of that name.
		if label.Expression != "" && strings.TrimSpace(label.Expression) == "" {
			return fmt.Errorf("collector %q metric %q label %q expression %q is nothing but blanks; write the expression that reads the label from the response, or leave expression out and set value for a constant", x.Name, r.Name, label.Name, label.Expression)
		}
		hasValue, hasExpression := label.Value != "", strings.TrimSpace(label.Expression) != ""
		switch {
		case hasValue && hasExpression:
			return fmt.Errorf("collector %q metric %q label %q sets both value and expression; set value for a static label, or expression to read it from the response", x.Name, r.Name, label.Name)
		case !hasValue && !hasExpression:
			return fmt.Errorf("collector %q metric %q label %q needs a value, for a static label, or an expression, to read it from the response", x.Name, r.Name, label.Name)
		case hasValue && label.Required:
			return fmt.Errorf("collector %q metric %q label %q has a static value, so it cannot be required; its value is always there", x.Name, r.Name, label.Name)
		case label.Required && x.Transform.Type == "python":
			return fmt.Errorf("collector %q metric %q label %q cannot be required: a python transform's labels come from its script, not from label expressions", x.Name, r.Name, label.Name)
		}
	}
	if err := checkPythonRuleLabelsBeforeSameRules(x, r); err != nil {
		return err
	}
	if err := transform.CheckMetricRule(x, r); err != nil {
		return err
	}
	return checkPythonRuleBeforeSameRules(x, r, wroteType, wroteErrorMode)
}

// checkPythonRuleLabelsBeforeSameRules is checkPythonRuleLabels as it was.
func checkPythonRuleLabelsBeforeSameRules(x *model.Collector, r *model.MetricRule) error {
	if x.Transform.Type != "python" {
		return nil
	}
	for _, label := range r.Labels {
		if label.Value != "" {
			return fmt.Errorf("collector %q metric %q label %q sets value, which a python rule's label does not take: the script sets the labels of its series itself, with metric(..., labels={...}), and a rule's label only names one of them to cut with truncate: true; for a constant on every series of the collector, set transform.labels", x.Name, r.Name, label.Name)
		}
	}
	return nil
}

// checkPythonRuleBeforeSameRules is checkPythonRule as it was.
func checkPythonRuleBeforeSameRules(x *model.Collector, r *model.MetricRule, wroteType, wroteErrorMode bool) error {
	if x.Transform.Type != "python" {
		return nil
	}
	var errs []error
	if r.Name == "" {
		errs = append(errs, fmt.Errorf("collector %q has a metric without a name, which a python rule needs: the rule makes no series and only names one of the script's, whose labels it cuts with truncate: true and which a debug probe's report lists when the script made none; write the name the script gives the series, as in metric(\"up\", ...), or take the rule out", x.Name))
	}
	where := fmt.Sprintf("collector %q metric %q", x.Name, r.Name)
	if wroteType {
		errs = append(errs, fmt.Errorf("%s sets type, which a python rule does not take: the script gives each of its series its type, with metric(..., type=\"counter\"), and a series is a gauge when it gives none; say the type in the script and leave type out of the rule", where))
	}
	if r.Description != "" {
		errs = append(errs, fmt.Errorf("%s sets description, which a python rule does not take: the script gives each of its series its help text, with metric(..., help=\"...\"), and a series has none when it gives none; say the help in the script and leave description out of the rule", where))
	}
	if r.Required != nil {
		errs = append(errs, fmt.Errorf("%s sets required, which a python rule does not take: the rule makes no series, so it has no value to miss; a script that cannot do without something fails the scrape itself, with fail(\"...\"), so say it in the script and leave required out of the rule", where))
	}
	if wroteErrorMode {
		errs = append(errs, fmt.Errorf("%s sets error_mode, which a python rule does not take: the rule makes no series, so it has no failure to handle; a script fails the scrape itself, with fail(\"...\"), and error_handling.on_transform_error says what the collector does then, so leave error_mode out of the rule", where))
	}
	for _, label := range r.Labels {
		if !label.Truncate {
			errs = append(errs, fmt.Errorf("%s label %q does not set truncate: true, which is all a python rule's label does: it names a label of the script's series to cut to limits.max_label_value_length, and its expression is not read; the script sets the labels of its series itself, with metric(..., labels={...}), so set truncate: true on the label or take it out", where, label.Name))
		}
	}
	return model.JoinProblems(errs...)
}

// checkCSVColumnsBeforeRulePlaces is checkCSVColumns as it was before a rule
// without a name was named by its place.
func checkCSVColumnsBeforeRulePlaces(x *model.Collector) error {
	if x.Transform.Type != "csv" || x.Response.CSV.Header == nil || *x.Response.CSV.Header {
		return nil
	}
	var errs []error
	check := func(what, column string) {
		if n, err := strconv.Atoi(column); err != nil || n < 1 || strconv.Itoa(n) != column {
			errs = append(errs, fmt.Errorf("collector %q %s reads column %q, but response.csv.header is false, so columns are named by number, from 1; write the column's number, such as \"2\"", x.Name, what, column))
		}
	}
	for _, rule := range x.Metrics {
		check(fmt.Sprintf("metric %q", rule.Name), rule.Expression)
		for _, label := range rule.Labels {
			if !label.Static() {
				check(fmt.Sprintf("metric %q label %q", rule.Name, label.Name), label.Expression)
			}
		}
	}
	return model.JoinProblems(errs...)
}

// checkPlaceholdersAreFilledBeforeRulePlaces is checkPlaceholdersAreFilled
// as it was before a rule without a name was named by its place.
func checkPlaceholdersAreFilledBeforeRulePlaces(x *model.Collector) error {
	filled := fetch.TemplatedFields(x)
	var errs []error
	refuse := func(where string) {
		errs = append(errs, fmt.Errorf("collector %q %s has a {{param_...}} placeholder, which is not filled in there and would be used as written; a probe's parameters fill placeholders only in the request's path, body, header and query values, a grpc message and metadata values, and a graphite collector's targets", x.Name, where))
	}
	collector := reflect.ValueOf(x).Elem()
	for i := 0; i < collector.NumField(); i++ {
		key, _, _ := strings.Cut(collector.Type().Field(i).Tag.Get("yaml"), ",")
		if key == "" || key == "-" || key == "metrics" {
			continue
		}
		walkStrings(collector.Field(i), key, func(path, text string) {
			if paramPlaceholder.MatchString(text) && !slices.Contains(filled, path) && !holdsItsOwnBraces(path) {
				refuse(path)
			}
		})
	}
	// A rule is named as its other errors name it, a label by its name.
	for i := range x.Metrics {
		rule := &x.Metrics[i]
		walkStrings(reflect.ValueOf(rule).Elem(), "", func(path, text string) {
			if !paramPlaceholder.MatchString(text) || holdsItsOwnBraces(path) {
				return
			}
			if m := labelPath.FindStringSubmatch(path); m != nil {
				var index int
				_, _ = fmt.Sscanf(m[1], "%d", &index)
				path = fmt.Sprintf("label %q %s", rule.Labels[index].Name, path[len(m[0]):])
			}
			refuse(fmt.Sprintf("metric %q %s", rule.Name, path))
		})
	}
	return model.JoinProblems(errs...)
}

// checkMetricFamiliesBeforeRulePlaces is checkMetricFamilies as it was
// before a rule without a name was named by its place.
func checkMetricFamiliesBeforeRulePlaces(x *model.Collector) error {
	limits := x.Limits
	for _, name := range model.SortedKeys(x.Transform.Labels) {
		value := x.Transform.Labels[name]
		if slices.Contains(x.Transform.RemoveLabels, name) {
			continue
		}
		if limits.MaxLabelValueLength > 0 && len(value) > limits.MaxLabelValueLength {
			return fmt.Errorf("collector %q transform.labels %q is %d bytes, longer than limits.max_label_value_length %d, so every series would fail validation; shorten it or raise the limit", x.Name, name, len(value), limits.MaxLabelValueLength)
		}
	}
	if x.Transform.Type == "python" {
		return nil
	}
	types := map[string]model.MetricType{}
	for _, rule := range x.Metrics {
		if limits.MaxHelpLength > 0 && len(rule.Description) > limits.MaxHelpLength {
			return fmt.Errorf("collector %q metric %q description is %d bytes, longer than limits.max_help_length %d, so every series would fail validation; shorten it or raise the limit", x.Name, rule.Name, len(rule.Description), limits.MaxHelpLength)
		}
		for _, label := range rule.Labels {
			if !label.Static() || label.Truncate || slices.Contains(x.Transform.RemoveLabels, label.Name) {
				continue
			}
			value := transform.MappedStaticLabelValue(x, rule.Name, label)
			if limits.MaxLabelValueLength > 0 && len(value) > limits.MaxLabelValueLength {
				return fmt.Errorf("collector %q metric %q label %q value is %d bytes, longer than limits.max_label_value_length %d, so every series would fail validation; shorten it, set truncate: true on the label, or raise the limit", x.Name, rule.Name, label.Name, len(value), limits.MaxLabelValueLength)
			}
		}
		if rule.Name == "" || rule.Type == "" {
			continue
		}
		if prior, ok := types[rule.Name]; ok && prior != rule.Type {
			return fmt.Errorf("collector %q metric %q is declared as both %s and %s; the rules of one metric name make one family, which has one type, so every scrape would fail validation; give them the same type or different names", x.Name, rule.Name, prior, rule.Type)
		}
		types[rule.Name] = rule.Type
	}
	for _, name := range model.SortedKeys(types) {
		var suffixes []string
		switch types[name] {
		case model.HistogramMetricType:
			suffixes = []string{"_bucket", "_sum", "_count"}
		case model.SummaryMetricType:
			suffixes = []string{"_sum", "_count"}
		}
		for _, suffix := range suffixes {
			if other, clash := types[name+suffix]; clash {
				return fmt.Errorf("collector %q metric %q (%s) clashes with the %s %q, which is written as series of that name, so every scrape that has both would fail validation; rename one", x.Name, name+suffix, other, types[name], name)
			}
		}
	}
	return nil
}
