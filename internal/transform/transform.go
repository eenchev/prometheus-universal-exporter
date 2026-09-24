package transform

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/PuerkitoBio/goquery"
	"github.com/antchfx/htmlquery"
	"github.com/antchfx/xmlquery"
	"github.com/antchfx/xpath"
	"github.com/eenchev/prometheus-universal-exporter/internal/decode"
	"github.com/eenchev/prometheus-universal-exporter/internal/expr"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"golang.org/x/net/html"
)

// Transform turns a decoded response into the collector's metrics, named as
// they are exported: every transform's output passes through here, so the
// collector's metrics_prefix is applied in exactly one place, before the limits
// are checked, the set is cached, or it is written to /probe or OTLP.
func Transform(ctx context.Context, d *decode.Decoded, r *fetch.HTTPResponse, c *model.Collector, pythonPath string) (*model.MetricSet, error) {
	report, _ := ctx.Value(ruleReportKey{}).(*RuleReport)
	ctx, failures := withRuleFailures(ctx)
	defer failures.finish(c, report)
	set, err := transformMetrics(ctx, d, r, c, pythonPath)
	if err != nil || set == nil {
		return set, err
	}
	applyCollectorLabels(set, c.Transform)
	truncateLabels(set, c)
	applyMetricsPrefix(set, c.MetricsPrefix)
	// After the prefix, so a values-escaped name still starts with U__
	// (nameescaping.go).
	if err := escapeNames(set, c.NameEscaping); err != nil {
		return nil, err
	}
	return set, nil
}

func transformMetrics(ctx context.Context, d *decode.Decoded, r *fetch.HTTPResponse, c *model.Collector, pythonPath string) (*model.MetricSet, error) {
	if strings.TrimSpace(c.Transform.PreScript) != "" {
		processed, err := applyPreScript(ctx, d, r, c, pythonPath)
		if err != nil {
			return nil, err
		}
		d = processed
	}
	if c.Transform.Type == "python" {
		if c.Transform.Script == "" {
			return nil, errors.New("python transform requires a script")
		}
		return executePython(ctx, pythonPath, c.Transform.Script, d, r, c)
	}
	if err := validateTransformInput(d, c.Transform.Type); err != nil {
		return nil, err
	}
	if ms, ok := d.Data.(model.MetricSet); ok {
		if c.Transform.Type == "prometheus" {
			return applyPrometheusTransform(ctx, ms, c, c.Transform, c.Metrics)
		}
		return nil, fmt.Errorf("unsupported transformation %q for Prometheus", c.Transform.Type)
	}
	switch c.Transform.Type {
	case "jq", "yq":
		return transformJQ(ctx, d.Data, c.Metrics, c)
	case "regex":
		text, ok := d.Data.(string)
		if !ok {
			return nil, errors.New("regex transformation requires text data")
		}
		return transformRegex(ctx, text, c.Metrics, c)
	case "css":
		h, ok := d.Data.(*decode.HTMLDecoded)
		if !ok {
			return nil, errors.New("CSS transformation requires HTML data")
		}
		return transformCSS(ctx, h.Document, c.Metrics, c)
	case "csv":
		return transformCSV(ctx, d.Data, c.Metrics, c)
	case "xpath":
		if h, ok := d.Data.(*decode.HTMLDecoded); ok {
			return transformHTMLXPath(ctx, h.Document, c.Metrics, c)
		}
		n, ok := d.Data.(*xmlquery.Node)
		if !ok {
			return nil, errors.New("XPath transformation requires XML or HTML data")
		}
		return transformXPath(ctx, n, c.Metrics, c, c.Response.Namespaces)
	default:
		return nil, fmt.Errorf("unsupported transformation %q", c.Transform.Type)
	}
}

func validateTransformInput(d *decode.Decoded, transformType string) error {
	switch transformType {
	case "jq", "yq":
		if d.Kind == "text" || d.Kind == "html" || d.Kind == "xml" || d.Kind == "csv" || d.Kind == "prometheus" {
			return fmt.Errorf("transform %q cannot map response format %q; use a structured JSON/YAML response or a compatible transform", transformType, d.Kind)
		}
	case "regex":
		if d.Kind != "text" {
			return fmt.Errorf("regex transform requires a text response, got %q", d.Kind)
		}
	case "csv":
		if d.Kind != "csv" {
			return fmt.Errorf("csv transform requires a CSV response, got %q", d.Kind)
		}
	case "css":
		if d.Kind != "html" {
			return fmt.Errorf("css transform requires an HTML response, got %q", d.Kind)
		}
	case "xpath":
		if d.Kind != "xml" && d.Kind != "html" {
			return fmt.Errorf("xpath transform requires an XML or HTML response, got %q", d.Kind)
		}
	case "prometheus":
		if d.Kind != "prometheus" {
			return fmt.Errorf("prometheus transform requires a Prometheus response, got %q", d.Kind)
		}
	}
	return nil
}

// applyCollectorLabels applies the collector-wide label settings to every
// metric, whatever the transform: transform.labels adds static labels,
// remove_labels drops labels, and rename_labels renames them, in that order.
//
// The renames are made at once, from the labels as they were before any of
// them, so they never chain: with a to b and b to c, b gets a's value and c
// gets b's, whatever order the map is walked in. Two renames to one label are
// refused at load (CheckTransformSettings). Each metric gets a label map of
// its own, so a transform's output never shares one with its input.
func applyCollectorLabels(set *model.MetricSet, t model.TransformConfig) {
	if len(t.Labels) == 0 && len(t.RemoveLabels) == 0 && len(t.RenameLabels) == 0 {
		return
	}
	for i := range set.Metrics {
		labels := model.CloneLabels(set.Metrics[i].Labels)
		for name, value := range t.Labels {
			labels[name] = value
		}
		for _, name := range t.RemoveLabels {
			delete(labels, name)
		}
		renamed := map[string]string{}
		for from, to := range t.RenameLabels {
			if value, ok := labels[from]; ok {
				renamed[to] = value
			}
		}
		for from := range t.RenameLabels {
			delete(labels, from)
		}
		for name, value := range renamed {
			labels[name] = value
		}
		set.Metrics[i].Labels = labels
	}
}

// handleMetricError applies a rule's error mode and reports whether the scrape
// should carry on without the metric. ignore carries on silently, log carries
// on and says why, and fail says why and stops: the caller then returns the
// failure wrapped by ruleFailure, and the scrape fails as a whole rather than
// serving the metrics that did work.
//
// The collector is named alongside the rule because a metric name is not unique
// across collectors, and without it a logged failure does not say which
// collector to go and look at. The line reads the same for log and fail, so a
// failing rule reads the same in the log whichever mode it has and whether it
// failed on a probe or on a scheduled target.
//
// Under log, a rule can fail once per series — every row of a table, every item
// — so within a Transform its failures are gathered (withRuleFailures) and the
// rule is logged once when the scrape's transform ends, with the first error and
// how many series failed. Without that gathering, as when called directly, the
// line is written at once. The failures of ignore are gathered too, unlogged,
// so the caller's RuleReport counts every series a rule could not produce.
//
// This logs through slog's default logger rather than one passed down: it is
// called from inside the transforms, several frames below anything holding a
// logger. The exporter installs its JSON logger as the process default at
// startup so these lines match every other line it writes.
func handleMetricError(ctx context.Context, c *model.Collector, rule model.MetricRule, err error) bool {
	failures, gathering := ctx.Value(ruleFailuresKey{}).(*ruleFailures)
	switch rule.ErrorMode {
	case model.ErrorModeIgnore:
		if gathering {
			failures.add(rule, err, false)
		}
		return true
	case model.ErrorModeLog:
		if gathering {
			failures.add(rule, err, true)
		} else {
			logRuleFailure(c, rule, err, 1)
		}
		return true
	case model.ErrorModeFail:
		logRuleFailure(c, rule, err, 1)
	}
	return false
}

func logRuleFailure(c *model.Collector, rule model.MetricRule, err error, failures uint64) {
	slog.Default().Error("metric extraction failed", "collector", collectorName(c), "metric", rule.Name, "error_mode", rule.ErrorMode, "error", err, "failures", failures)
}

// ruleFailures gathers the failures of rules that carried on, under log or
// ignore, during one Transform, so each log rule is logged once per scrape
// however many of its series failed, and the caller can count them all.
type ruleFailures struct {
	rules []*failedRule
}

// failedRule is one rule's failures in a scrape: the first, which the log
// line shows, how many there were, how many of them were missing values, and
// whether the rule logs them.
type failedRule struct {
	rule    model.MetricRule
	first   error
	count   uint64
	missing uint64
	logged  bool
}

type ruleFailuresKey struct{}

// withRuleFailures returns a context gathering log-mode rule failures, and
// what gathers them.
func withRuleFailures(ctx context.Context) (context.Context, *ruleFailures) {
	failures := &ruleFailures{}
	return context.WithValue(ctx, ruleFailuresKey{}, failures), failures
}

// add counts a failure of rule. Rules are told apart by name and expression,
// since two rules may export the same metric name.
func (f *ruleFailures) add(rule model.MetricRule, err error, logged bool) {
	var missing uint64
	if errors.Is(err, model.ErrMissingValue) {
		missing = 1
	}
	for _, known := range f.rules {
		if known.rule.Name == rule.Name && known.rule.Expression == rule.Expression && known.rule.Items == rule.Items {
			known.count++
			known.missing += missing
			return
		}
	}
	f.rules = append(f.rules, &failedRule{rule: rule, first: err, count: 1, missing: missing, logged: logged})
}

// finish writes one line per log rule that failed, in the order they first
// failed, and adds every failure to report when the caller asked for one.
func (f *ruleFailures) finish(c *model.Collector, report *RuleReport) {
	for _, failed := range f.rules {
		if failed.logged {
			logRuleFailure(c, failed.rule, failed.first, failed.count)
		}
		if report != nil {
			report.add(failed.rule.Name, failed.count, failed.missing)
		}
	}
}

// RuleReport counts, for its caller, the series metric rules could not
// produce and carried on without, under error_mode log or ignore, across the
// Transforms made with its context (WithRuleReport). A rule that fails the
// scrape is not in it: that failure is the Transform's error.
type RuleReport struct {
	mu       sync.Mutex
	failures []RuleFailure
}

// RuleFailure is how many series of one metric failed, and how many of those
// because the response did not contain the value.
type RuleFailure struct {
	Metric            string
	Failures, Missing uint64
}

type ruleReportKey struct{}

// WithRuleReport returns a context whose Transforms report their rules'
// failures in the returned RuleReport.
func WithRuleReport(ctx context.Context) (context.Context, *RuleReport) {
	report := &RuleReport{}
	return context.WithValue(ctx, ruleReportKey{}, report), report
}

func (r *RuleReport) add(metric string, failures, missing uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.failures {
		if r.failures[i].Metric == metric {
			r.failures[i].Failures += failures
			r.failures[i].Missing += missing
			return
		}
	}
	r.failures = append(r.failures, RuleFailure{Metric: metric, Failures: failures, Missing: missing})
}

// Failures returns the failures reported, one entry per metric name.
func (r *RuleReport) Failures() []RuleFailure {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]RuleFailure(nil), r.failures...)
}

// MetricFailure is a metric rule that could not produce its value and whose
// error mode does not allow the scrape to carry on without it. It is its own
// type so the probe handler can tell it apart from a failure of the transform
// as a whole: the rule asked for the scrape to fail, and that request is not
// something a collector-wide on_transform_error policy gets to overrule.
type MetricFailure struct {
	Collector string
	Metric    string
	Err       error
}

func (f *MetricFailure) Error() string { return f.Err.Error() }

func (f *MetricFailure) Unwrap() error { return f.Err }

// ruleFailure wraps the error a transform returns when handleMetricError says
// the scrape cannot carry on. The message is unchanged, so an error reads the
// same as it always did; only its type says which rule it came from.
func ruleFailure(c *model.Collector, rule model.MetricRule, err error) error {
	return &MetricFailure{Collector: collectorName(c), Metric: rule.Name, Err: err}
}

// collectorName keeps a logging path from panicking on an absent collector,
// which would turn a reported metric failure into a crashed scrape.
func collectorName(c *model.Collector) string {
	if c == nil {
		return ""
	}
	return c.Name
}

func applyPreScript(ctx context.Context, d *decode.Decoded, r *fetch.HTTPResponse, c *model.Collector, pythonPath string) (*decode.Decoded, error) {
	data, err := executePythonPreScript(ctx, pythonPath, c.Transform.PreScript, d, r, c)
	if err != nil {
		return nil, err
	}
	if jqFamily(c.Transform.Type) && structuredValue(data) {
		return &decode.Decoded{Kind: "json", Data: data, Raw: d.Raw}, nil
	}
	if d.Kind == "html" {
		raw := fmt.Append(nil, data)
		doc, parseErr := goquery.NewDocumentFromReader(bytes.NewReader(raw))
		if parseErr != nil {
			return nil, fmt.Errorf("HTML pre-script output: %w", parseErr)
		}
		return &decode.Decoded{Kind: "html", Data: &decode.HTMLDecoded{Document: doc, Raw: raw}, Raw: raw}, nil
	}
	if d.Kind == "xml" {
		raw := fmt.Append(nil, data)
		node, parseErr := xmlquery.Parse(bytes.NewReader(raw))
		if parseErr != nil {
			return nil, fmt.Errorf("XML pre-script output: %w", parseErr)
		}
		return &decode.Decoded{Kind: "xml", Data: node, Raw: raw}, nil
	}
	return &decode.Decoded{Kind: d.Kind, Data: data, Raw: d.Raw}, nil
}

// structuredValue reports whether a pre-script returned an object or an array.
// Scalars leave the decoded format alone, so a pre-script that rewrites text,
// HTML, or XML keeps its existing behavior.
func structuredValue(value any) bool {
	switch value.(type) {
	case map[string]any, []any:
		return true
	}
	return false
}

func transformJQ(ctx context.Context, data any, rules []model.MetricRule, c *model.Collector) (*model.MetricSet, error) {
	out := &model.MetricSet{}
	for _, rule := range rules {
		if rule.Items != "" {
			metrics, err := transformJQItems(ctx, data, rule, c)
			if err != nil {
				return nil, err
			}
			out.Metrics = append(out.Metrics, metrics...)
			continue
		}
		values, err := evaluateJQ(ctx, data, data, rule.Expression)
		if err != nil {
			if handleMetricError(ctx, c, rule, err) {
				continue
			}
			return nil, ruleFailure(c, rule, fmt.Errorf("metric %q expression: %w", rule.Name, err))
		}
		labels, err := evaluateLabels(ctx, data, rule.Labels, len(values))
		if err != nil {
			if handleMetricError(ctx, c, rule, err) {
				continue
			}
			return nil, ruleFailure(c, rule, fmt.Errorf("metric %q labels: %w", rule.Name, err))
		}
		if len(values) == 0 && requiredRule(rule, c) {
			missing := model.MarkError(fmt.Errorf("metric %q value is missing", rule.Name), model.ErrMissingValue)
			if handleMetricError(ctx, c, rule, missing) {
				continue
			}
			return nil, ruleFailure(c, rule, missing)
		}
		for index, value := range values {
			if blankValue(value) {
				if requiredRule(rule, c) {
					missing := model.MarkError(fmt.Errorf("metric %q value is missing", rule.Name), model.ErrMissingValue)
					if handleMetricError(ctx, c, rule, missing) {
						continue
					}
					return nil, ruleFailure(c, rule, missing)
				}
				continue
			}
			n, err := model.Number(value)
			if err != nil {
				if handleMetricError(ctx, c, rule, err) {
					continue
				}
				return nil, ruleFailure(c, rule, fmt.Errorf("metric %q: %w", rule.Name, err))
			}
			if missing := missingRequiredLabel(rule, labels[index]); missing != nil {
				if handleMetricError(ctx, c, rule, missing) {
					continue
				}
				return nil, ruleFailure(c, rule, missing)
			}
			out.Metrics = append(out.Metrics, model.Metric{Name: rule.Name, Help: rule.Description, Type: rule.Type, Value: n, Labels: model.CloneLabels(labels[index])})
		}
	}
	return out, nil
}

// transformJQItems evaluates a rule item by item. items selects the things
// the metric is about — components, rows, workers — and the value and every
// label are evaluated against one item at a time, so they cannot drift apart
// the way parallel streams can when one of them skips an element. Each
// expression sees the item as its input and the whole document as $root.
//
// For one item, the value expression must produce at most one value and each
// label expression at most one; more is an error, since there would be no
// telling which belongs to the series. A missing or null value is a missing
// metric for that item, handled by required and error_mode like any other; a
// missing or null label leaves the label off.
func transformJQItems(ctx context.Context, data any, rule model.MetricRule, c *model.Collector) ([]model.Metric, error) {
	fail := func(err error) ([]model.Metric, bool, error) {
		if handleMetricError(ctx, c, rule, err) {
			return nil, true, nil
		}
		return nil, false, ruleFailure(c, rule, err)
	}
	items, err := evaluateJQ(ctx, data, data, rule.Items)
	if err != nil {
		metrics, _, failure := fail(fmt.Errorf("metric %q items: %w", rule.Name, err))
		return metrics, failure
	}
	if len(items) == 0 && requiredRule(rule, c) {
		metrics, _, failure := fail(model.MarkError(fmt.Errorf("metric %q items selected nothing", rule.Name), model.ErrMissingValue))
		return metrics, failure
	}
	var out []model.Metric
	for index, item := range items {
		value, err := evaluateJQOne(ctx, item, data, rule.Expression)
		if err != nil {
			if _, carryOn, failure := fail(fmt.Errorf("metric %q item %d expression: %w", rule.Name, index, err)); !carryOn {
				return nil, failure
			}
			continue
		}
		if blankValue(value) {
			if requiredRule(rule, c) {
				if _, carryOn, failure := fail(model.MarkError(fmt.Errorf("metric %q value is missing for item %d", rule.Name, index), model.ErrMissingValue)); !carryOn {
					return nil, failure
				}
			}
			continue
		}
		n, err := model.Number(value)
		if err != nil {
			if _, carryOn, failure := fail(fmt.Errorf("metric %q item %d: %w", rule.Name, index, err)); !carryOn {
				return nil, failure
			}
			continue
		}
		labels := map[string]string{}
		var labelErr error
		for _, label := range rule.Labels {
			if label.Static() {
				labels[label.Name] = label.Value
				continue
			}
			labelValue, err := evaluateJQOne(ctx, item, data, label.Expression)
			if err != nil {
				labelErr = fmt.Errorf("metric %q item %d label %q: %w", rule.Name, index, label.Name, err)
				break
			}
			if labelValue != nil {
				labels[label.Name] = fmt.Sprint(labelValue)
			}
		}
		if labelErr == nil {
			if missing := missingRequiredLabel(rule, labels); missing != nil {
				labelErr = fmt.Errorf("%w for item %d", missing, index)
			}
		}
		if labelErr != nil {
			if _, carryOn, failure := fail(labelErr); !carryOn {
				return nil, failure
			}
			continue
		}
		out = append(out, model.Metric{Name: rule.Name, Help: rule.Description, Type: rule.Type, Value: n, Labels: labels})
	}
	return out, nil
}

// evaluateJQ runs a compiled program with input as its input and root bound
// to $root, collecting every value it produces.
func evaluateJQ(ctx context.Context, input, root any, expression string) ([]any, error) {
	code, err := expr.CompileJQ(expression)
	if err != nil {
		return nil, err
	}
	iterator := code.RunWithContext(ctx, input, root)
	values := []any{}
	for {
		value, ok := iterator.Next()
		if !ok {
			break
		}
		if executionErr, ok := value.(error); ok {
			return nil, executionErr
		}
		values = append(values, value)
	}
	return values, nil
}

// evaluateJQOne runs a program that must produce at most one value; no value
// is reported as nil.
func evaluateJQOne(ctx context.Context, input, root any, expression string) (any, error) {
	values, err := evaluateJQ(ctx, input, root, expression)
	if err != nil {
		return nil, err
	}
	switch len(values) {
	case 0:
		return nil, nil
	case 1:
		return values[0], nil
	default:
		return nil, fmt.Errorf("expression %q produced %d values for one item; it must produce at most one", expression, len(values))
	}
}

func evaluateLabels(ctx context.Context, data any, expressions []model.LabelRule, metricCount int) ([]map[string]string, error) {
	labels := make([]map[string]string, metricCount)
	for index := range labels {
		labels[index] = map[string]string{}
	}
	for _, label := range expressions {
		if label.Static() {
			for index := range labels {
				labels[index][label.Name] = label.Value
			}
			continue
		}
		values, err := evaluateJQ(ctx, data, data, label.Expression)
		if err != nil {
			return nil, fmt.Errorf("label %q: %w", label.Name, err)
		}
		// Values are paired with series by position. None leaves the label
		// off every series, and one applies to all of them; any other count
		// than one per series means some values landed on the wrong series,
		// so the metric fails rather than be exported mislabelled. items
		// evaluates labels per element, which cannot drift.
		if len(values) > 1 && len(values) != metricCount && metricCount > 0 {
			return nil, fmt.Errorf("label %q gave %d values for %d series, so they cannot be paired; give one value, or one per series, or set items to evaluate labels per element", label.Name, len(values), metricCount)
		}
		if len(values) == 1 {
			if values[0] != nil {
				for index := range labels {
					labels[index][label.Name] = fmt.Sprint(values[0])
				}
			}
			continue
		}
		for index := 0; index < len(labels) && index < len(values); index++ {
			if values[index] != nil {
				labels[index][label.Name] = fmt.Sprint(values[index])
			}
		}
	}
	return labels, nil
}

// missingRequiredLabel finishes a series' labels. An expression label with an
// empty value is left off, as one the expression gave no value for is: the two
// are the same series to Prometheus, and OTLP should not tell them apart
// either. It then reports the first label of rule marked required that the
// series has no value for. The error is a missing value, so it is counted as
// one and the rule's error_mode decides what happens to the series.
func missingRequiredLabel(rule model.MetricRule, labels map[string]string) error {
	for _, label := range rule.Labels {
		if !label.Static() && labels[label.Name] == "" {
			delete(labels, label.Name)
		}
	}
	for _, label := range rule.Labels {
		if label.Required {
			if _, ok := labels[label.Name]; !ok {
				return model.MarkError(fmt.Errorf("metric %q label %q is missing", rule.Name, label.Name), model.ErrMissingValue)
			}
		}
	}
	return nil
}

// blankValue reports whether a value an expression gave is no value: none at
// all, or text of nothing but whitespace. Every transform treats it as a
// missing value, handled by required and error_mode, rather than as text that
// failed to read as a number.
func blankValue(v any) bool {
	if text, ok := v.(string); ok {
		return isBlank(text)
	}
	return v == nil
}

func isBlank(text string) bool { return strings.TrimSpace(text) == "" }

func requiredRule(rule model.MetricRule, c *model.Collector) bool {
	return (rule.Required == nil || *rule.Required) && !c.ErrorHandling.AllowMissingKeys
}

func transformRegex(ctx context.Context, text string, rules []model.MetricRule, c *model.Collector) (*model.MetricSet, error) {
	out := &model.MetricSet{}
	for _, rule := range rules {
		re, err := expr.CompileRegex(rule.Expression)
		if err != nil {
			if handleMetricError(ctx, c, rule, err) {
				continue
			}
			return nil, ruleFailure(c, rule, fmt.Errorf("metric %q regex: %w", rule.Name, err))
		}
		matches := re.FindAllStringSubmatchIndex(text, -1)
		if len(matches) == 0 {
			if requiredRule(rule, c) {
				missing := model.MarkError(fmt.Errorf("regex for metric %q matched no text", rule.Name), model.ErrMissingValue)
				if handleMetricError(ctx, c, rule, missing) {
					continue
				}
				return nil, ruleFailure(c, rule, missing)
			}
			continue
		}
		names := re.SubexpNames()
		for _, match := range matches {
			// The first capture group is the value; the configuration refuses
			// a regex without one. A group that took no part in the match, as
			// an optional one can, or that captured only blanks, is a missing
			// value like a match that never happened.
			if match[2] < 0 || isBlank(text[match[2]:match[3]]) {
				if requiredRule(rule, c) {
					missing := model.MarkError(fmt.Errorf("regex for metric %q matched, but its first capture group captured no value", rule.Name), model.ErrMissingValue)
					if handleMetricError(ctx, c, rule, missing) {
						continue
					}
					return nil, ruleFailure(c, rule, missing)
				}
				continue
			}
			n, err := decode.TextValue(text[match[2]:match[3]])
			if err != nil {
				if handleMetricError(ctx, c, rule, err) {
					continue
				}
				return nil, ruleFailure(c, rule, fmt.Errorf("metric %q: %w", rule.Name, err))
			}
			labels := map[string]string{}
			for _, label := range rule.Labels {
				if label.Static() {
					labels[label.Name] = label.Value
					continue
				}
				index := captureIndex(label.Expression, names)
				if index >= 0 && 2*index+1 < len(match) && match[2*index] >= 0 {
					labels[label.Name] = text[match[2*index]:match[2*index+1]]
				}
			}
			if missing := missingRequiredLabel(rule, labels); missing != nil {
				if handleMetricError(ctx, c, rule, missing) {
					continue
				}
				return nil, ruleFailure(c, rule, missing)
			}
			out.Metrics = append(out.Metrics, model.Metric{Name: rule.Name, Help: rule.Description, Type: rule.Type, Value: n, Labels: labels})
		}
	}
	return out, nil
}

func captureIndex(value string, names []string) int {
	if index, err := strconv.Atoi(value); err == nil {
		return index
	}
	for index, name := range names {
		if name == value {
			return index
		}
	}
	return -1
}

// xpathNodes is what the XPath transform needs of a parsed document, so one
// implementation serves XML, through xmlquery, and HTML, through htmlquery.
type xpathNodes[N any] struct {
	// kind names the language in errors: "XPath" or "HTML XPath".
	kind string
	all  func(root N, e *xpath.Expr) []N
	one  func(node N, e *xpath.Expr) (N, bool)
	attr func(node N, name string) string
	text func(node N) string
}

var xmlNodes = xpathNodes[*xmlquery.Node]{
	kind: "XPath",
	all:  xmlquery.QuerySelectorAll,
	one: func(node *xmlquery.Node, e *xpath.Expr) (*xmlquery.Node, bool) {
		found := xmlquery.QuerySelector(node, e)
		return found, found != nil
	},
	attr: func(node *xmlquery.Node, name string) string { return node.SelectAttr(name) },
	text: func(node *xmlquery.Node) string { return node.InnerText() },
}

var htmlNodes = xpathNodes[*html.Node]{
	kind: "HTML XPath",
	all:  htmlquery.QuerySelectorAll,
	one: func(node *html.Node, e *xpath.Expr) (*html.Node, bool) {
		found := htmlquery.QuerySelector(node, e)
		return found, found != nil
	},
	attr: htmlquery.SelectAttr,
	text: htmlquery.InnerText,
}

func transformXPath(ctx context.Context, root *xmlquery.Node, rules []model.MetricRule, c *model.Collector, namespaces map[string]string) (*model.MetricSet, error) {
	return transformXPathNodes(ctx, root, xmlNodes, rules, c, namespaces)
}

// transformHTMLXPath runs XPath over the document the html decoder already
// parsed: goquery and htmlquery both build it with html.Parse, so the
// document's root node is what htmlquery would have parsed from the body.
func transformHTMLXPath(ctx context.Context, doc *goquery.Document, rules []model.MetricRule, c *model.Collector) (*model.MetricSet, error) {
	return transformXPathNodes(ctx, doc.Nodes[0], htmlNodes, rules, c, nil)
}

// transformXPathNodes evaluates each rule's expression against the document
// and makes a series of every node it selects. A label is a constant, an
// attribute of the node (@name), or the text of the first node an expression
// relative to it selects.
func transformXPathNodes[N any](ctx context.Context, root N, nodes xpathNodes[N], rules []model.MetricRule, c *model.Collector, namespaces map[string]string) (*model.MetricSet, error) {
	out := &model.MetricSet{}
	for _, rule := range rules {
		expression, err := expr.CompileXPath(rule.Expression, namespaces)
		if err != nil {
			if handleMetricError(ctx, c, rule, err) {
				continue
			}
			return nil, ruleFailure(c, rule, fmt.Errorf("%s %q: %w", nodes.kind, rule.Expression, err))
		}
		selected := nodes.all(root, expression)
		if len(selected) == 0 {
			if requiredRule(rule, c) {
				missing := model.MarkError(fmt.Errorf("%s %q matched no nodes", nodes.kind, rule.Expression), model.ErrMissingValue)
				if handleMetricError(ctx, c, rule, missing) {
					continue
				}
				return nil, ruleFailure(c, rule, missing)
			}
			continue
		}
		for _, node := range selected {
			labels := map[string]string{}
			for _, label := range rule.Labels {
				if label.Static() {
					labels[label.Name] = label.Value
				} else if strings.HasPrefix(label.Expression, "@") {
					labels[label.Name] = nodes.attr(node, strings.TrimPrefix(label.Expression, "@"))
				} else if selector, err := expr.CompileXPath(label.Expression, namespaces); err == nil {
					if value, ok := nodes.one(node, selector); ok {
						labels[label.Name] = nodes.text(value)
					}
				}
			}
			text := nodes.text(node)
			if isBlank(text) {
				if requiredRule(rule, c) {
					missing := model.MarkError(fmt.Errorf("%s %q selected a node without a value", nodes.kind, rule.Expression), model.ErrMissingValue)
					if handleMetricError(ctx, c, rule, missing) {
						continue
					}
					return nil, ruleFailure(c, rule, missing)
				}
				continue
			}
			value, err := decode.TextValue(text)
			if err != nil {
				if handleMetricError(ctx, c, rule, err) {
					continue
				}
				return nil, ruleFailure(c, rule, fmt.Errorf("metric %q: %w", rule.Name, err))
			}
			if missing := missingRequiredLabel(rule, labels); missing != nil {
				if handleMetricError(ctx, c, rule, missing) {
					continue
				}
				return nil, ruleFailure(c, rule, missing)
			}
			out.Metrics = append(out.Metrics, model.Metric{Name: rule.Name, Help: rule.Description, Type: rule.Type, Value: value, Labels: labels})
		}
	}
	return out, nil
}

func transformCSS(ctx context.Context, doc *goquery.Document, rules []model.MetricRule, c *model.Collector) (*model.MetricSet, error) {
	out := &model.MetricSet{}
	for _, rule := range rules {
		if rule.Items != "" {
			metrics, err := transformCSSItems(ctx, doc, rule, c)
			if err != nil {
				return nil, err
			}
			out.Metrics = append(out.Metrics, metrics...)
			continue
		}
		matcher, err := expr.CompileCSS(rule.Expression)
		if err != nil {
			if handleMetricError(ctx, c, rule, err) {
				continue
			}
			return nil, ruleFailure(c, rule, fmt.Errorf("CSS selector %q: %w", rule.Expression, err))
		}
		selection := doc.FindMatcher(matcher)
		if selection.Length() == 0 {
			if requiredRule(rule, c) {
				missing := model.MarkError(fmt.Errorf("CSS selector %q matched no nodes", rule.Expression), model.ErrMissingValue)
				if handleMetricError(ctx, c, rule, missing) {
					continue
				}
				return nil, ruleFailure(c, rule, missing)
			}
			continue
		}
		// Without items a rule is one series, whose labels are static:
		// validation refuses expression labels, which need items. Several
		// elements would be several series no label tells apart.
		if selection.Length() > 1 {
			err := fmt.Errorf("metric %q CSS selector %q matched %d elements, but without items a css metric is one value; set items to the elements, such as '#servers tr:has(td)', and select the value and each label within one", rule.Name, rule.Expression, selection.Length())
			if handleMetricError(ctx, c, rule, err) {
				continue
			}
			return nil, ruleFailure(c, rule, err)
		}
		text := strings.TrimSpace(selection.Text())
		if isBlank(text) {
			if requiredRule(rule, c) {
				missing := model.MarkError(fmt.Errorf("CSS selector %q matched an element without a value", rule.Expression), model.ErrMissingValue)
				if handleMetricError(ctx, c, rule, missing) {
					continue
				}
				return nil, ruleFailure(c, rule, missing)
			}
			continue
		}
		value, err := decode.TextValue(text)
		if err != nil {
			err = fmt.Errorf("metric %q: %w", rule.Name, err)
			if handleMetricError(ctx, c, rule, err) {
				continue
			}
			return nil, ruleFailure(c, rule, err)
		}
		labels := map[string]string{}
		for _, label := range rule.Labels {
			if label.Static() {
				labels[label.Name] = label.Value
			}
		}
		out.Metrics = append(out.Metrics, model.Metric{Name: rule.Name, Help: rule.Description, Type: rule.Type, Value: value, Labels: labels})
	}
	return out, nil
}

// transformCSSItems evaluates a rule item by item, as transformJQItems does:
// items selects the elements the metric is about — table rows, status
// entries — and the value selector and every label selector are matched
// within one item at a time. Without items the value is the whole text of
// each selected element, so a label can only read text that is part of the
// number; with items, the value and the labels are cells of the same row.
//
// Within one item the value selector and each label selector must match at
// most one element; more is an error, since there would be no telling which
// belongs to the series. A value selector matching nothing is a missing
// metric for that item, handled by required and error_mode like any other; a
// label selector matching nothing leaves the label off.
func transformCSSItems(ctx context.Context, doc *goquery.Document, rule model.MetricRule, c *model.Collector) ([]model.Metric, error) {
	fail := func(err error) (bool, error) {
		if handleMetricError(ctx, c, rule, err) {
			return true, nil
		}
		return false, ruleFailure(c, rule, err)
	}
	itemsMatcher, err := expr.CompileCSS(rule.Items)
	if err != nil {
		_, failure := fail(fmt.Errorf("metric %q items CSS selector %q: %w", rule.Name, rule.Items, err))
		return nil, failure
	}
	valueMatcher, err := expr.CompileCSS(rule.Expression)
	if err != nil {
		_, failure := fail(fmt.Errorf("metric %q CSS selector %q: %w", rule.Name, rule.Expression, err))
		return nil, failure
	}
	items := doc.FindMatcher(itemsMatcher)
	if items.Length() == 0 && requiredRule(rule, c) {
		_, failure := fail(model.MarkError(fmt.Errorf("metric %q items CSS selector %q matched no nodes", rule.Name, rule.Items), model.ErrMissingValue))
		return nil, failure
	}
	// one is the trimmed text of the element selector matches within item, or
	// false when it matches none.
	one := func(item *goquery.Selection, index int, selector string, matcher goquery.Matcher) (string, bool, error) {
		found := item.FindMatcher(matcher)
		switch found.Length() {
		case 0:
			return "", false, nil
		case 1:
			return strings.TrimSpace(found.Text()), true, nil
		default:
			return "", false, fmt.Errorf("metric %q item %d: CSS selector %q matched %d elements; within an item it must match at most one", rule.Name, index, selector, found.Length())
		}
	}
	var out []model.Metric
	for index := range items.Length() {
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
			err = model.MarkError(fmt.Errorf("metric %q value is missing for item %d: CSS selector %q %s", rule.Name, index, rule.Expression, why), model.ErrMissingValue)
		}
		var value float64
		if err == nil {
			value, err = decode.TextValue(text)
			if err != nil {
				err = fmt.Errorf("metric %q item %d: %w", rule.Name, index, err)
			}
		}
		labels := map[string]string{}
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
				err = fmt.Errorf("%w for item %d", missing, index)
			}
		}
		if err != nil {
			if carryOn, failure := fail(err); !carryOn {
				return nil, failure
			}
			continue
		}
		out = append(out, model.Metric{Name: rule.Name, Help: rule.Description, Type: rule.Type, Value: value, Labels: labels})
	}
	return out, nil
}

func transformCSV(ctx context.Context, data any, rules []model.MetricRule, c *model.Collector) (*model.MetricSet, error) {
	rows, ok := data.([]any)
	if !ok {
		return nil, errors.New("CSV transform requires a header-based CSV response")
	}
	out := &model.MetricSet{}
	for _, raw := range rows {
		row, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		for _, rule := range rules {
			value, exists := row[rule.Expression]
			if !exists || blankValue(value) {
				if requiredRule(rule, c) {
					missing := model.MarkError(fmt.Errorf("CSV column %q is missing", rule.Expression), model.ErrMissingValue)
					if handleMetricError(ctx, c, rule, missing) {
						continue
					}
					return nil, ruleFailure(c, rule, missing)
				}
				continue
			}
			n, err := model.Number(value)
			if err != nil {
				if handleMetricError(ctx, c, rule, err) {
					continue
				}
				return nil, ruleFailure(c, rule, fmt.Errorf("metric %q: %w", rule.Name, err))
			}
			labels := map[string]string{}
			for _, label := range rule.Labels {
				if label.Static() {
					labels[label.Name] = label.Value
				} else if labelValue, exists := row[label.Expression]; exists {
					labels[label.Name] = fmt.Sprint(labelValue)
				}
			}
			if missing := missingRequiredLabel(rule, labels); missing != nil {
				if handleMetricError(ctx, c, rule, missing) {
					continue
				}
				return nil, ruleFailure(c, rule, missing)
			}
			out.Metrics = append(out.Metrics, model.Metric{Name: rule.Name, Help: rule.Description, Type: rule.Type, Value: n, Labels: labels})
		}
	}
	return out, nil
}

func applyPrometheusTransform(ctx context.Context, in model.MetricSet, c *model.Collector, t model.TransformConfig, rules []model.MetricRule) (*model.MetricSet, error) {
	if len(rules) > 0 {
		out := model.MetricSet{}
		for _, source := range in.Metrics {
			for _, rule := range rules {
				pattern := rule.Expression
				if pattern == "" {
					pattern = "^" + regexp.QuoteMeta(rule.Name) + "$"
				}
				re, err := expr.CompileRegex(pattern)
				if err != nil {
					if handleMetricError(ctx, c, rule, err) {
						continue
					}
					return nil, ruleFailure(c, rule, fmt.Errorf("metric %q expression: %w", rule.Name, err))
				}
				if !re.MatchString(source.Name) {
					continue
				}
				metric := source
				if rule.Name != "" {
					metric.Name = rule.Name
				}
				if rule.Description != "" {
					metric.Help = rule.Description
				}
				if rule.Type != "" {
					metric.Type = rule.Type
				}
				// The series gets labels of its own: rules add and remove
				// them, and another rule may match the same source metric.
				metric.Labels = model.CloneLabels(source.Labels)
				for _, label := range rule.Labels {
					if label.Static() {
						metric.Labels[label.Name] = label.Value
					} else if value, ok := metric.Labels[label.Expression]; ok {
						metric.Labels[label.Name] = value
					}
				}
				if missing := missingRequiredLabel(rule, metric.Labels); missing != nil {
					if handleMetricError(ctx, c, rule, missing) {
						continue
					}
					return nil, ruleFailure(c, rule, missing)
				}
				out.Metrics = append(out.Metrics, metric)
			}
		}
		return &out, nil
	}
	out := model.MetricSet{}
	includes := make([]*regexp.Regexp, 0, len(t.Include))
	for _, expression := range t.Include {
		re, err := expr.CompileRegex(expression)
		if err != nil {
			return nil, err
		}
		includes = append(includes, re)
	}
	excludes := make([]*regexp.Regexp, 0, len(t.Exclude))
	for _, expression := range t.Exclude {
		re, err := expr.CompileRegex(expression)
		if err != nil {
			return nil, err
		}
		excludes = append(excludes, re)
	}
	for _, metric := range in.Metrics {
		included := len(includes) == 0
		for _, expression := range includes {
			if expression.MatchString(metric.Name) {
				included = true
			}
		}
		for _, expression := range excludes {
			if expression.MatchString(metric.Name) {
				included = false
			}
		}
		if !included {
			continue
		}
		if name, ok := t.Rename[metric.Name]; ok {
			metric.Name = name
		}
		out.Metrics = append(out.Metrics, metric)
	}
	return &out, nil
}
