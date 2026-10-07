package transform

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/big"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/PuerkitoBio/goquery"
	"github.com/antchfx/htmlquery"
	"github.com/antchfx/xmlquery"
	"github.com/antchfx/xpath"
	"github.com/eenchev/prometheus-universal-exporter/internal/decode"
	"github.com/eenchev/prometheus-universal-exporter/internal/expr"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// Transform turns a decoded response into the collector's metrics, named as
// they are exported: every transform's output passes through here, so the
// collector's metrics_prefix is applied in exactly one place, before the limits
// are checked, the set is cached, or it is written to /probe or OTLP.
func Transform(ctx context.Context, d *decode.Decoded, r *fetch.HTTPResponse, c *model.Collector, pythonPath string) (*model.MetricSet, error) {
	report, _ := ctx.Value(ruleReportKey{}).(*RuleReport)
	ctx = withResponseVariables(ctx, r)
	ctx = withSeriesBudget(ctx, c.Limits.MaxMetrics)
	ctx, failures := withRuleFailures(ctx)
	defer failures.finish(c, report)
	set, borrowed, err := transformMetrics(ctx, d, r, c, pythonPath)
	// A rule that failed once the context was done did not fail for anything
	// the response holds, and handleMetricError let none carry on, so what
	// reads as that rule's failure is the transform's, with the context's
	// error: the caller answers as it does a trip that ran out of time, and
	// keeps nothing of it.
	var failure *MetricFailure
	if errors.As(err, &failure) && ctx.Err() != nil {
		return nil, interruptedAt(ctx, failure.Metric)
	}
	if err != nil || set == nil {
		return set, err
	}
	// Invalid UTF-8 is repaired before anything measures or maps the text:
	// a truncated label repaired afterwards would grow past the limit it was
	// cut to, as each invalid byte becomes a three-byte U+FFFD.
	//
	// A pass-through's series are the decoded ones themselves
	// (passesThrough), which are not this transform's to write to: they
	// are only read to see whether any needs the repair, and copied first if
	// one does, which is rare, so that the scrape that needs none pays for
	// one pass over them and no copy.
	if borrowed && needsUTF8Repair(set) {
		set.Metrics, borrowed = slices.Clone(set.Metrics), false
	}
	if !borrowed {
		changed, first := model.SanitizeUTF8(set)
		if report != nil {
			report.addUTF8(changed, first)
		}
	}
	// Label value maps and truncation first, while the labels still have
	// the names the rules gave them: rename_labels would otherwise move a
	// label out from under its value_map or truncate: true.
	mapLabelValues(set, c)
	truncateLabels(set, c)
	applyCollectorLabels(set, c.Transform)
	applyMetricsPrefix(set, c.MetricsPrefix)
	// After the prefix, so a values-escaped name still starts with U__
	// (nameescaping.go).
	if err := escapeNames(set, c.NameEscaping); err != nil {
		return nil, err
	}
	return set, nil
}

// transformMetrics makes the collector's series of the decoded response.
// borrowed says the set's series are the decoded ones themselves, in the
// decoder's own slice, as a prometheus pass-through hands them on
// (applyPrometheusTransform): the caller may read them and must not write to
// them.
func transformMetrics(ctx context.Context, d *decode.Decoded, r *fetch.HTTPResponse, c *model.Collector, pythonPath string) (set *model.MetricSet, borrowed bool, err error) {
	if strings.TrimSpace(c.Transform.PreScript) != "" {
		processed, err := applyPreScript(ctx, d, r, c, pythonPath)
		if err != nil {
			return nil, false, err
		}
		d = processed
	}
	if c.Transform.Type == "python" {
		if c.Transform.Script == "" {
			return nil, false, errors.New("python transform requires a script")
		}
		set, err = executePython(ctx, pythonPath, c.Transform.Script, d, r, c)
		return set, false, err
	}
	if err := validateTransformInput(d, c.Transform.Type); err != nil {
		return nil, false, err
	}
	if ms, ok := d.Data.(model.MetricSet); ok {
		if c.Transform.Type == "prometheus" {
			return applyPrometheusTransform(ctx, ms, c, c.Transform, c.Metrics)
		}
		return nil, false, fmt.Errorf("unsupported transformation %q for Prometheus", c.Transform.Type)
	}
	switch c.Transform.Type {
	case "jq", "yq":
		set, err = transformJQ(ctx, d.Data, c.Metrics, c)
	case "regex":
		text, ok := d.Data.(string)
		if !ok {
			return nil, false, errors.New("regex transformation requires text data")
		}
		set, err = transformRegex(ctx, text, c.Metrics, c)
	case "css":
		h, ok := d.Data.(*decode.HTMLDecoded)
		if !ok {
			return nil, false, errors.New("CSS transformation requires HTML data")
		}
		set, err = transformCSS(ctx, h.Document, c.Metrics, c)
	case "csv":
		set, err = transformCSV(ctx, d.Data, c.Metrics, c)
	case "xpath":
		if h, ok := d.Data.(*decode.HTMLDecoded); ok {
			set, err = transformHTMLXPath(ctx, h.Document, c.Metrics, c)
			break
		}
		n, ok := d.Data.(*xmlquery.Node)
		if !ok {
			return nil, false, errors.New("XPath transformation requires XML or HTML data")
		}
		set, err = transformXPath(ctx, n, c.Metrics, c, c.Response.Namespaces)
	default:
		return nil, false, fmt.Errorf("unsupported transformation %q", c.Transform.Type)
	}
	return set, false, err
}

// needsUTF8Repair reports whether model.SanitizeUTF8 would change anything
// in set: a help text or a label value that is not valid UTF-8.
func needsUTF8Repair(set *model.MetricSet) bool {
	for i := range set.Metrics {
		m := &set.Metrics[i]
		if !utf8.ValidString(m.Help) {
			return true
		}
		for _, value := range m.Labels {
			if !utf8.ValidString(value) {
				return true
			}
		}
	}
	return false
}

// applyCollectorLabels applies the collector-wide label settings to every
// metric, whatever the transform: transform.labels adds static labels,
// remove_labels drops labels, and rename_labels renames them, in that order.
// A transform.labels value of "" is the label left out, as a key written ""
// is the key left out: it sets nothing, so a label of that name a rule gave
// stays as it is, where a value that is not empty replaces it.
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
			if value != "" {
				labels[name] = value
			}
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
// failed on a probe or on a static target.
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
// startup so these lines match every other line it writes. A debug probe's
// context carries its report's logger instead (WithRuleLogger). When the caller
// logs the failures itself (LeaveRuleLoggingToCaller), as the exporter does,
// nothing here does.
//
// Once the context is done — the probe's deadline passed, or its caller went
// away — no rule carries on, whatever its mode, and nothing is logged or
// counted against it: the deadline is not the rule's failure, and a scrape
// that went on would answer with whatever its rules made before it, as if
// that were all the target had. The Transform then fails as a whole with the
// context's error (interruptedAt).
func handleMetricError(ctx context.Context, c *model.Collector, rule model.MetricRule, err error) bool {
	if ctx.Err() != nil {
		return false
	}
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
			logRuleFailure(ctx, c, rule, err, 1)
		}
		return true
	case model.ErrorModeFail:
		logRuleFailure(ctx, c, rule, err, 1)
	}
	return false
}

// interruptedAt is the failure of a transform whose context is done, naming
// the rule it was at, if it has a name, so a rule too slow for the probe's
// budget can be found. It is the transform's failure and not the rule's
// (MetricFailure): no error_mode applies to it.
func interruptedAt(ctx context.Context, metric string) error {
	if metric == "" {
		return fmt.Errorf("the transform was stopped before it finished: %w", ctx.Err())
	}
	return fmt.Errorf("the transform was stopped at metric %q: %w", metric, ctx.Err())
}

// interrupted is interruptedAt for the loops over a response's nodes, rows
// and matches, which ask it before each: nil while the context is live. The
// jq transforms need not ask, since a jq program stops by itself.
func interrupted(ctx context.Context, rule model.MetricRule) error {
	if ctx.Err() == nil {
		return nil
	}
	return interruptedAt(ctx, rule.Name)
}

func logRuleFailure(ctx context.Context, c *model.Collector, rule model.MetricRule, err error, failures uint64) {
	if ctx.Value(callerLogsRulesKey{}) != nil {
		return
	}
	// The line is no longer than a failure's text may be, whatever the
	// rule's error quotes of the response (model.BoundedFailure).
	ruleLogger(ctx).Error("metric extraction failed", "collector", collectorName(c), "metric", rule.Name, "error_mode", rule.ErrorMode, "error", model.BoundedFailure(err), "failures", failures)
}

// ruleFailures gathers the failures of rules that carried on, under log or
// ignore, during one Transform, so each log rule is logged once per scrape
// however many of its series failed, and the caller can count them all.
type ruleFailures struct {
	rules []*failedRule
	// ctx is the Transform's, for its logger.
	ctx context.Context
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
	failures := &ruleFailures{ctx: ctx}
	return context.WithValue(ctx, ruleFailuresKey{}, failures), failures
}

// add counts a failure of rule. Rules are told apart by name and expression,
// since two rules may export the same metric name.
func (f *ruleFailures) add(rule model.MetricRule, err error, logged bool) {
	var missing uint64
	if errors.Is(err, model.ErrMissingValue) {
		missing = 1
	}
	f.addCounted(rule, err, 1, missing, logged)
}

// addCounted counts count failures of rule at once, missing of them missing
// values and first the first, as a rule that held them back reports them
// (heldFailures).
//
// Two rules alike in name, expression and items are one rule here, whatever
// their error modes: it is logged when either has log, as the report has it
// (RuleReport.add). So when a rule under log joins what a twin under ignore
// gathered, the entry becomes that of the rule under log, with this failure
// as the first to show; the mode of whichever twin failed first does not
// decide for both.
func (f *ruleFailures) addCounted(rule model.MetricRule, first error, count, missing uint64, logged bool) {
	for _, known := range f.rules {
		if known.rule.Name == rule.Name && known.rule.Expression == rule.Expression && known.rule.Items == rule.Items {
			known.count += count
			known.missing += missing
			if logged && !known.logged {
				known.rule, known.first, known.logged = rule, first, true
			}
			return
		}
	}
	f.rules = append(f.rules, &failedRule{rule: rule, first: first, count: count, missing: missing, logged: logged})
}

// finish writes one line per log rule that failed, in the order they first
// failed, and adds every failure to report when the caller asked for one,
// each under the rule it is of.
func (f *ruleFailures) finish(c *model.Collector, report *RuleReport) {
	for _, failed := range f.rules {
		if failed.logged {
			logRuleFailure(f.ctx, c, failed.rule, failed.first, failed.count)
		}
		if report != nil {
			// The first failure is kept by whoever reads the report, logged
			// and remembered: it is no longer than a failure's text may be
			// (model.BoundedFailure), once for the rule, however many of
			// its series failed.
			report.add(failed.rule.Name, failed.rule.Expression, failed.rule.Items, failed.count, failed.missing, model.BoundedFailure(failed.first), failed.logged)
		}
	}
}

type callerLogsRulesKey struct{}

// LeaveRuleLoggingToCaller returns a context whose Transforms log none of
// their rules' failures: the caller logs those that carried on from its
// RuleReport, where it knows the target and can tell a repeat from a new
// failure, and a rule under fail fails the Transform with an error that
// names it, which the caller logs as the scrape's failure.
func LeaveRuleLoggingToCaller(ctx context.Context) context.Context {
	return context.WithValue(ctx, callerLogsRulesKey{}, true)
}

type ruleLoggerKey struct{}

// WithRuleLogger returns a context whose Transforms log their rules'
// failures to logger rather than slog's default logger, as a debug probe's
// report takes them.
func WithRuleLogger(ctx context.Context, logger *slog.Logger) context.Context {
	return context.WithValue(ctx, ruleLoggerKey{}, logger)
}

// ruleLogger is the logger rule failures go to.
func ruleLogger(ctx context.Context) *slog.Logger {
	if logger, ok := ctx.Value(ruleLoggerKey{}).(*slog.Logger); ok {
		return logger
	}
	return slog.Default()
}

// RuleReport counts, for its caller, the series metric rules could not
// produce and carried on without, under error_mode log or ignore, across the
// Transforms made with its context (WithRuleReport). A rule that fails the
// scrape is not in it: that failure is the Transform's error.
type RuleReport struct {
	mu       sync.Mutex
	failures []RuleFailure
	// utf8Repaired counts the label values and help texts repaired for
	// invalid UTF-8, and utf8First names the first metric repaired.
	utf8Repaired uint64
	utf8First    string
}

func (r *RuleReport) addUTF8(changed uint64, first string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.utf8Repaired += changed
	if r.utf8First == "" {
		r.utf8First = first
	}
}

// UTF8Repairs returns how many label values and help texts the Transforms
// repaired for invalid UTF-8, and the first metric repaired.
func (r *RuleReport) UTF8Repairs() (uint64, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.utf8Repaired, r.utf8First
}

// RuleFailure is how many series of one rule failed, and how many of those
// because the response did not contain the value.
//
// A collector may have several rules of one metric name, one for each column
// or path its series come from, and each fails for reasons of its own: so a
// rule is told apart as the transform tells it apart while it gathers the
// failures (ruleFailures), by its name, its expression and its items, and
// each has an entry of its own. Two rules alike in all three are one rule.
type RuleFailure struct {
	// Metric is the rule's name, empty for a prometheus rule without one.
	Metric string
	// Expression and Items are the rule's, which with Metric tell it from
	// the collector's other rules.
	Expression, Items string
	Failures, Missing uint64
	// First is the first of the failures, as a debug probe shows it.
	First error
	// Logged says the rule has error_mode log, or a rule alike in all three
	// has, so its failures are to be logged, First being the first of them.
	Logged bool
}

type ruleReportKey struct{}

// WithRuleReport returns a context whose Transforms report their rules'
// failures in the returned RuleReport.
func WithRuleReport(ctx context.Context) (context.Context, *RuleReport) {
	report := &RuleReport{}
	return context.WithValue(ctx, ruleReportKey{}, report), report
}

func (r *RuleReport) add(metric, expression, items string, failures, missing uint64, first error, logged bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.failures {
		if r.failures[i].Metric == metric && r.failures[i].Expression == expression && r.failures[i].Items == items {
			r.failures[i].Failures += failures
			r.failures[i].Missing += missing
			if logged && !r.failures[i].Logged {
				r.failures[i].Logged, r.failures[i].First = true, first
			}
			return
		}
	}
	r.failures = append(r.failures, RuleFailure{Metric: metric, Expression: expression, Items: items, Failures: failures, Missing: missing, First: first, Logged: logged})
}

// Failures returns the failures reported, one entry per rule, in the order
// the rules first failed.
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
	if d.Kind == "prometheus" {
		// The script was given the series as {"metrics": [...]}; what it
		// left is read back into series for the prometheus transform.
		set, err := prometheusFromPython(data)
		if err != nil {
			return nil, model.MarkError(fmt.Errorf("python pre-script: %w", err), model.ErrScriptFailed)
		}
		return &decode.Decoded{Kind: "prometheus", Data: set, Raw: d.Raw}, nil
	}
	if d.Kind == "html" || d.Kind == "xml" {
		// Markup is parsed again from the text the script left. A mapping
		// or a list is no markup: rendered as Go writes it, it would parse
		// into a document every rule then matches nothing in, and the
		// mistake would read as a selector's.
		if _, ok := data.(string); !ok {
			return nil, model.MarkError(fmt.Errorf("python pre-script of a %s transform must leave data as a string of %s, not %s; to hand the rules structured data, use a jq or yq transform, which reads a mapping or a list from a pre-script", c.Transform.Type, strings.ToUpper(d.Kind), pythonTypeName(data)), model.ErrScriptFailed)
		}
	}
	if d.Kind == "html" {
		raw := fmt.Append(nil, data)
		doc, parseErr := decode.ParseHTML(raw)
		if parseErr != nil {
			return nil, fmt.Errorf("HTML pre-script output: %w", parseErr)
		}
		return &decode.Decoded{Kind: "html", Data: &decode.HTMLDecoded{Document: doc, Raw: raw}, Raw: raw}, nil
	}
	if d.Kind == "xml" {
		raw := fmt.Append(nil, data)
		node, parseErr := decode.ParseXML(raw)
		if parseErr != nil {
			return nil, fmt.Errorf("XML pre-script output: %w", parseErr)
		}
		return &decode.Decoded{Kind: "xml", Data: node, Raw: raw}, nil
	}
	return &decode.Decoded{Kind: d.Kind, Data: data, Raw: d.Raw}, nil
}

// pythonTypeName names a value a script left as Python calls its type.
func pythonTypeName(v any) string {
	switch v.(type) {
	case map[string]any:
		return "a dict"
	case []any:
		return "a list"
	case nil:
		return "None"
	case bool:
		return "a bool"
	case float64, int, *big.Int:
		return "a number"
	}
	return fmt.Sprintf("%T", v)
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

// transformJQ makes the series of each rule in turn, every rule adding its
// own to the one set: a rule's series are not gathered apart and copied in.
// Rules that read the same items share one evaluation of them (jqSharing).
func transformJQ(ctx context.Context, data any, rules []model.MetricRule, c *model.Collector) (*model.MetricSet, error) {
	out := &model.MetricSet{}
	var sharing jqSharing
	for i, rule := range rules {
		var err error
		if rule.Items != "" {
			err = transformJQItems(ctx, out, data, rule, c, &sharing, rules[i+1:])
		} else {
			err = transformJQValues(ctx, out, data, rule, c)
		}
		if err != nil {
			return nil, err
		}
	}
	return noSeriesIsNil(out), nil
}

// transformJQValues evaluates a rule without items: its expression is read
// against the whole document, and every value it gives is a series. Each
// label expression is read against the whole document too, and its values
// are paired with the series by position (jqLabel).
//
// The values are taken one at a time, as the program produces them, and a
// series' labels are made when the series is, so a scrape past
// limits.max_metrics stops at the first series too many, as one with items
// does, rather than after collecting every value and a set of labels for
// each.
//
// What can go wrong is of two kinds, and which is reported is what it was
// when every value and every label was read before the first series was
// made. The rule fails as a whole when its expression fails anywhere among
// its values, or a label cannot be read or paired (wholeProblem): no series
// of it is exported, those made so far being dropped, and it is one
// failure, the expression's or the label's. A single value fails — it is
// missing, it is no number, its series lacks a required label — and the
// rule carries on without that series under log and ignore. Taking the
// values one at a time, a value that fails is met before it is known
// whether the rule fails as a whole further on, so the failures of single
// values are held until it is: when the rule fails as a whole they are not
// counted beside it, and under fail the first of them is the scrape's
// failure only when nothing fails the rule as a whole. Finding that out
// reads the rest of what the expressions give, counting it, without keeping
// any of it.
func transformJQValues(ctx context.Context, out *model.MetricSet, data any, rule model.MetricRule, c *model.Collector) error {
	// fail applies the rule's error mode to err, which the scrape's failure
	// words as failure: nil means carry on without the rule.
	fail := func(err, failure error) error {
		if handleMetricError(ctx, c, rule, err) {
			return nil
		}
		return ruleFailure(c, rule, failure)
	}
	values, err := runJQ(ctx, data, data, rule.Expression)
	var value any
	var more bool
	if err == nil {
		// The first value is asked for before any label is read, so an
		// expression that fails outright is reported before its labels.
		value, more, err = values.next()
	}
	if err != nil {
		return fail(err, fmt.Errorf("metric %q expression: %w", rule.Name, err))
	}
	labels, started := newJQLabels(ctx, data, rule.Labels)
	// The rule's series are those of out from start on, and held the
	// failures of single values it carried on without, which are the
	// rule's to report unless it fails as a whole.
	start := len(out.Metrics)
	var held heldFailures
	defer func() { held.report(ctx, c, rule) }()
	// whole fails the rule as a whole, of its expression or of its labels:
	// the series it made are given back and nothing else is counted.
	whole := func(part string, err error) error {
		releaseSeries(ctx, len(out.Metrics)-start)
		out.Metrics = out.Metrics[:start]
		held = heldFailures{}
		return fail(err, fmt.Errorf("metric %q %s: %w", rule.Name, part, err))
	}
	// count is how many of the rule's values were taken so far.
	count := 0
	if !started {
		if more {
			count = 1
		}
		return whole(wholeProblem(values, labels, count))
	}
	if !more {
		if part, err := wholeProblem(values, labels, count); err != nil {
			return whole(part, err)
		}
		if requiredRule(rule, c) {
			missing := model.MarkError(fmt.Errorf("metric %q value is missing", rule.Name), model.ErrMissingValue)
			return fail(missing, missing)
		}
		return nil
	}
	// single is the failure of one value: held while the rule carries on,
	// and otherwise the scrape's, unless the rule fails as a whole.
	single := func(err, failure error) error {
		carriesOn := rule.ErrorMode == model.ErrorModeIgnore || rule.ErrorMode == model.ErrorModeLog
		if carriesOn && ctx.Err() == nil {
			held.add(err)
			return nil
		}
		if part, err := wholeProblem(values, labels, count); err != nil {
			return whole(part, err)
		}
		return fail(err, failure)
	}
	paired := make([]jqPaired, len(labels))
	for ; more; value, more, err = values.next() {
		count++
		for i := range labels {
			var ok bool
			if paired[i], ok = labels[i].pair(count - 1); !ok {
				// The label ran out, failed, or gave what is no label.
				return whole(wholeProblem(values, labels, count))
			}
		}
		if blankValue(value) {
			if requiredRule(rule, c) {
				missing := model.MarkError(fmt.Errorf("metric %q value is missing", rule.Name), model.ErrMissingValue)
				if failure := single(missing, missing); failure != nil {
					return failure
				}
			}
			continue
		}
		n, err := ruleValue(rule, value)
		if err != nil {
			if failure := single(err, fmt.Errorf("metric %q: %w", rule.Name, err)); failure != nil {
				return failure
			}
			continue
		}
		series := make(map[string]string, len(labels))
		for i := range labels {
			labels[i].set(series, paired[i])
		}
		if missing := missingRequiredLabel(rule, series); missing != nil {
			if failure := single(missing, missing); failure != nil {
				return failure
			}
			continue
		}
		if err := takeSeries(ctx); err != nil {
			return err
		}
		out.Metrics = append(out.Metrics, model.Metric{Name: rule.Name, Help: rule.Description, Type: rule.Type, Value: n, Labels: series})
	}
	if err != nil {
		return whole("expression", err)
	}
	// A label with values left over was not one per series, and one that
	// fails after its last paired value fails all the same.
	if part, err := wholeProblem(values, labels, count); err != nil {
		return whole(part, err)
	}
	return nil
}

// heldFailures are the failures of single values of a rule without items
// that carried on without them: the first, how many, and how many of them
// were missing values, which is what a rule's failures are counted and
// logged by.
type heldFailures struct {
	first          error
	count, missing uint64
}

func (h *heldFailures) add(err error) {
	if h.count == 0 {
		h.first = err
	}
	h.count++
	if errors.Is(err, model.ErrMissingValue) {
		h.missing++
	}
}

// report counts the held failures against the rule, as handleMetricError
// counts one: gathered for the scrape's one line and its RuleReport, or,
// with nothing gathering them, logged here under log.
func (h *heldFailures) report(ctx context.Context, c *model.Collector, rule model.MetricRule) {
	if h.count == 0 {
		return
	}
	logged := rule.ErrorMode == model.ErrorModeLog
	if failures, gathering := ctx.Value(ruleFailuresKey{}).(*ruleFailures); gathering {
		failures.addCounted(rule, h.first, h.count, h.missing, logged)
	} else if logged {
		logRuleFailure(ctx, c, rule, h.first, h.count)
	}
}

// wholeProblem is what fails a rule without items as a whole, if anything
// does, and which part of the rule it is the failure of: what was reported
// when the expression and then each label were read to their end before
// any series was made. That is, in this order, the expression failing at
// any of its values, then the first label, as the rule lists them, that
// fails at any of its values, gives another number of them than there are
// series, or gives a value among them that is no label's.
//
// count is how many of the rule's values were taken so far. The rest of
// them, and of each label's, are read here and counted, none kept.
func wholeProblem(values *jqValues, labels []jqLabel, count int) (part string, problem error) {
	series := count + values.rest()
	if values.err != nil {
		return "expression", values.err
	}
	for i := range labels {
		if err := labels[i].problem(series); err != nil {
			return "labels", err
		}
	}
	return "", nil
}

// jqLabel is a label of a jq rule without items. Its expression is read
// against the whole document: no value leaves the label off every series,
// one value applies to all of them, and several are paired with the series
// by position, each taken as its series is reached. Any other count than one
// per series means some values landed on the wrong series, so the metric
// fails rather than be exported mislabelled; items evaluates labels per
// element, which cannot drift.
type jqLabel struct {
	name string
	// text is the label's value on every series, when all has it: a static
	// label's, or the one value its expression gave.
	text string
	all  bool
	// values is the rest of what the expression gives, when it gave more
	// than one value: the first two, read to tell, are in head, and taken
	// counts those read so far, one for each series reached.
	values *jqValues
	head   [2]any
	taken  int
	// failed is why the label cannot be read: its expression failed, or
	// its one value is no label's. noText is the first of its paired
	// values that is no label's, which fails the rule only if the label
	// reads to its end and pairs (problem).
	failed, noText error
}

// jqPaired is the value a paired label has for one series, as text; has is
// false for a null, which leaves the label off.
type jqPaired struct {
	text string
	has  bool
}

// newJQLabels starts the labels of a rule, reading of each expression only
// the two values that tell which kind it is. started is false when a label
// could not be: it is the last of those returned, with why (failed), and
// the labels after it are not read, since the rule fails of it or of one
// before it (wholeProblem).
func newJQLabels(ctx context.Context, data any, rules []model.LabelRule) (labels []jqLabel, started bool) {
	labels = make([]jqLabel, 0, len(rules))
	for _, rule := range rules {
		labels = append(labels, jqLabel{name: rule.Name})
		label := &labels[len(labels)-1]
		if rule.Static() {
			label.text, label.all = rule.Value, true
			continue
		}
		values, err := runJQ(ctx, data, data, rule.Expression)
		if err != nil {
			label.failed = fmt.Errorf("label %q: %w", rule.Name, err)
			return labels, false
		}
		var have int
		for ; have < len(label.head); have++ {
			value, ok, err := values.next()
			if err != nil {
				label.failed = fmt.Errorf("label %q: %w", rule.Name, err)
				return labels, false
			}
			if !ok {
				break
			}
			label.head[have] = value
		}
		switch {
		case have == len(label.head):
			label.values = values
		case have == 1 && label.head[0] != nil:
			text, err := labelText(label.head[0])
			if err != nil {
				label.failed = fmt.Errorf("label %q %w", rule.Name, err)
				return labels, false
			}
			label.text, label.all = text, true
		}
	}
	return labels, true
}

// take reads the next of a paired label's values; ok is false when they
// have run out, or the expression failed, which failed then says.
func (l *jqLabel) take() (value any, ok bool) {
	if l.taken < len(l.head) {
		value, ok = l.head[l.taken], true
	} else {
		var err error
		if value, ok, err = l.values.next(); err != nil {
			l.failed = fmt.Errorf("label %q: %w", l.name, err)
			return nil, false
		}
	}
	if ok {
		l.taken++
	}
	return value, ok
}

// pair takes the label's value for the series at index, when its values are
// paired by position. ok is false when the rule fails of the label: its
// values have run out, its expression failed, or the value is no label's. A
// label of one value, or none, has nothing to take.
func (l *jqLabel) pair(index int) (paired jqPaired, ok bool) {
	if l.values == nil {
		return jqPaired{}, true
	}
	value, ok := l.take()
	if !ok || value == nil {
		return jqPaired{}, ok
	}
	text, err := labelText(value)
	if err != nil {
		l.noText = model.Errorf("label %q value %d %w", l.name, model.Position(index), err)
		return jqPaired{}, false
	}
	return jqPaired{text: text, has: true}, true
}

// problem is why the rule fails of this label, given how many series the
// rule's values make, or nil: it reads the label's values to their end. An
// expression that failed comes first, wherever it did, then a count that
// does not pair, then the first value paired with a series that is no
// label's. Without series there is nothing to pair or to label.
func (l *jqLabel) problem(series int) error {
	if l.failed != nil || l.values == nil {
		return l.failed
	}
	for {
		value, ok := l.take()
		if !ok {
			break
		}
		if value != nil && l.taken <= series && l.noText == nil {
			if _, err := labelText(value); err != nil {
				l.noText = model.Errorf("label %q value %d %w", l.name, model.Position(l.taken-1), err)
			}
		}
	}
	switch {
	case l.failed != nil:
		return l.failed
	case series == 0:
		return nil
	case l.taken != series:
		return model.Errorf("label %q gave %d values for %d series, so they cannot be paired; give one value, or one per series, or set items to evaluate labels per element", l.name, model.Size(l.taken), model.Size(series))
	}
	return l.noText
}

// set puts the label on a series whose labels are being made: its one
// value, or paired, the value pair took for it; a null leaves it off.
func (l *jqLabel) set(series map[string]string, paired jqPaired) {
	switch {
	case l.all:
		series[l.name] = l.text
	case paired.has:
		series[l.name] = paired.text
	}
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
func transformJQItems(ctx context.Context, out *model.MetricSet, data any, rule model.MetricRule, c *model.Collector, sharing *jqSharing, later []model.MetricRule) error {
	fail := func(err error) (bool, error) {
		if handleMetricError(ctx, c, rule, err) {
			return true, nil
		}
		return false, ruleFailure(c, rule, err)
	}
	code, err := expr.CompileJQ(rule.Items)
	if err != nil {
		_, failure := fail(fmt.Errorf("metric %q items: %w", rule.Name, err))
		return failure
	}
	// The items are taken one at a time, as the program produces them, so
	// a scrape past limits.max_metrics stops at the first series too many
	// rather than after collecting every item.
	// A rule after one with the same items reads what that rule's program
	// gave instead of running the program again (jqSharing).
	items := sharing.items(ctx, rule.Items, later)
	if items.kept == nil {
		vars, _ := ctx.Value(responseVariablesKey{}).(responseVariables)
		items.running = code.RunWithContext(ctx, data, data, vars.status, vars.headers)
	}
	// The rule's series are those of out from start on. Room is made for
	// them at once where the number of items is known beforehand.
	start := len(out.Metrics)
	if known, ok := items.expected(data, rule.Items); ok {
		out.Metrics = growSeries(ctx, out.Metrics, known)
	}
	for index := 0; ; index++ {
		item, more := items.next()
		if !more {
			if index == 0 && requiredRule(rule, c) {
				_, failure := fail(model.MarkError(fmt.Errorf("metric %q items selected nothing", rule.Name), model.ErrMissingValue))
				return failure
			}
			break
		}
		if itemsErr, isErr := item.(error); isErr {
			// The rule's series so far are dropped with it, as they were
			// when the items were all collected before any was read.
			releaseSeries(ctx, len(out.Metrics)-start)
			out.Metrics = out.Metrics[:start]
			_, failure := fail(fmt.Errorf("metric %q items: %w", rule.Name, itemsErr))
			return failure
		}
		value, err := evaluateJQOne(ctx, item, data, rule.Expression)
		if err != nil {
			if carryOn, failure := fail(model.Errorf("metric %q item %d expression: %w", rule.Name, model.Position(index), err)); !carryOn {
				return failure
			}
			continue
		}
		if blankValue(value) {
			if requiredRule(rule, c) {
				if carryOn, failure := fail(model.MarkError(model.Errorf("metric %q value is missing for item %d", rule.Name, model.Position(index)), model.ErrMissingValue)); !carryOn {
					return failure
				}
			}
			continue
		}
		n, err := ruleValue(rule, value)
		if err != nil {
			if carryOn, failure := fail(model.Errorf("metric %q item %d: %w", rule.Name, model.Position(index), err)); !carryOn {
				return failure
			}
			continue
		}
		labels := make(map[string]string, len(rule.Labels))
		var labelErr error
		for _, label := range rule.Labels {
			if label.Static() {
				labels[label.Name] = label.Value
				continue
			}
			labelValue, err := evaluateJQOne(ctx, item, data, label.Expression)
			if err != nil {
				labelErr = model.Errorf("metric %q item %d label %q: %w", rule.Name, model.Position(index), label.Name, err)
				break
			}
			if labelValue != nil {
				text, err := labelText(labelValue)
				if err != nil {
					labelErr = model.Errorf("metric %q item %d label %q %w", rule.Name, model.Position(index), label.Name, err)
					break
				}
				labels[label.Name] = text
			}
		}
		if labelErr == nil {
			if missing := missingRequiredLabel(rule, labels); missing != nil {
				labelErr = model.Errorf("%w for item %d", missing, model.Position(index))
			}
		}
		if labelErr != nil {
			if carryOn, failure := fail(labelErr); !carryOn {
				return failure
			}
			continue
		}
		if err := takeSeries(ctx); err != nil {
			return err
		}
		out.Metrics = append(out.Metrics, model.Metric{Name: rule.Name, Help: rule.Description, Type: rule.Type, Value: n, Labels: labels})
	}
	return nil
}

// evaluateJQ runs a compiled program with input as its input and root bound
// to $root, collecting every value it produces. A field path, such as .id,
// is looked up without running the program when it can be, with the value
// gojq would give (expr.JQProgram.Lookup): it is what most expressions
// evaluated per item are. The rules themselves evaluate with evaluateJQOne
// and runJQ, which keep no list of the values; this is the evaluation both
// are held to by their tests.
func evaluateJQ(ctx context.Context, input, root any, expression string) ([]any, error) {
	program, err := expr.CompileJQProgram(expression)
	if err != nil {
		return nil, err
	}
	if value, ok := program.Lookup(input); ok {
		return []any{value}, nil
	}
	vars, _ := ctx.Value(responseVariablesKey{}).(responseVariables)
	iterator := program.Code.RunWithContext(ctx, input, root, vars.status, vars.headers)
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

// jqValues are the values a jq program produces, taken one at a time, so a
// caller that stops part of the way leaves the rest unmade.
type jqValues struct {
	// looked is the one value of a field path looked up without running
	// the program (expr.JQProgram.Lookup), until it is taken.
	looked    any
	hasLooked bool
	// program is the running program, until it ends.
	program interface{ Next() (any, bool) }
	// err is the error the program ended with.
	err error
}

// runJQ starts a compiled program as evaluateJQ runs one, for its values to
// be taken as they are needed.
func runJQ(ctx context.Context, input, root any, expression string) (*jqValues, error) {
	program, err := expr.CompileJQProgram(expression)
	if err != nil {
		return nil, err
	}
	if value, ok := program.Lookup(input); ok {
		return &jqValues{looked: value, hasLooked: true}, nil
	}
	vars, _ := ctx.Value(responseVariablesKey{}).(responseVariables)
	return &jqValues{program: program.Code.RunWithContext(ctx, input, root, vars.status, vars.headers)}, nil
}

// next takes the program's next value; ok is false when there is none. An
// error ends the values, as it ends evaluateJQ, and is given again by every
// later call.
func (v *jqValues) next() (value any, ok bool, err error) {
	switch {
	case v.err != nil:
		return nil, false, v.err
	case v.hasLooked:
		v.hasLooked = false
		return v.looked, true, nil
	case v.program == nil:
		return nil, false, nil
	}
	value, ok = v.program.Next()
	if !ok {
		v.program = nil
		return nil, false, nil
	}
	if executionErr, isErr := value.(error); isErr {
		v.err, v.program = executionErr, nil
		return nil, false, executionErr
	}
	return value, true, nil
}

// rest counts the values the program has left, taking them all, for an
// error's wording. An error ends the count.
func (v *jqValues) rest() int {
	n := 0
	for {
		if _, ok, err := v.next(); !ok || err != nil {
			return n
		}
		n++
	}
}

// responseVariables are $status and $headers, as jq sees them: the
// response's status as a number and its headers as an object of lower-case
// names, each with its values joined by ", ", as HTTP allows; null when the
// response has none.
type responseVariables struct {
	status  any
	headers any
}

type responseVariablesKey struct{}

// withResponseVariables puts $status and $headers for r into ctx.
func withResponseVariables(ctx context.Context, r *fetch.HTTPResponse) context.Context {
	if r == nil {
		return ctx
	}
	vars := responseVariables{status: r.Status()}
	if r.Headers != nil {
		headers := make(map[string]any, len(r.Headers))
		for name, values := range r.Headers {
			headers[strings.ToLower(name)] = strings.Join(values, ", ")
		}
		vars.headers = headers
	}
	return context.WithValue(ctx, responseVariablesKey{}, vars)
}

// evaluateJQOne runs a program that must produce at most one value; no value
// is reported as nil. It is evaluateJQ without the list of values: an
// expression evaluated once per item and label is nearly always a field
// path, whose one value needs none, and of any other only the first value
// and how many there were is kept.
func evaluateJQOne(ctx context.Context, input, root any, expression string) (any, error) {
	program, err := expr.CompileJQProgram(expression)
	if err != nil {
		return nil, err
	}
	if value, ok := program.Lookup(input); ok {
		return value, nil
	}
	vars, _ := ctx.Value(responseVariablesKey{}).(responseVariables)
	iterator := program.Code.RunWithContext(ctx, input, root, vars.status, vars.headers)
	var first any
	count := 0
	for {
		value, ok := iterator.Next()
		if !ok {
			break
		}
		if executionErr, ok := value.(error); ok {
			return nil, executionErr
		}
		if count == 0 {
			first = value
		}
		count++
	}
	if count > 1 {
		return nil, model.Errorf("expression %q produced %d values for one item; it must produce at most one", expression, model.Size(count))
	}
	return first, nil
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
		matches, found := regexMatches(ctx, re, text)
		match, ok := matches()
		if !ok {
			if requiredRule(rule, c) {
				missing := model.MarkError(fmt.Errorf("regex for metric %q matched no text", rule.Name), model.ErrMissingValue)
				if handleMetricError(ctx, c, rule, missing) {
					continue
				}
				return nil, ruleFailure(c, rule, missing)
			}
			continue
		}
		// Which capture group is the value and which each label reads is the
		// same for every match, so it is looked up once for the rule and not
		// once a match.
		value := regexValueGroup(re)
		captures := labelCaptures(rule.Labels, re.SubexpNames())
		out.Metrics = growSeries(ctx, out.Metrics, found)
		for ; ok; match, ok = matches() {
			if err := interrupted(ctx, rule); err != nil {
				return nil, err
			}
			if err := regexSeries(ctx, out, text, match, value, captures, rule, c); err != nil {
				return nil, err
			}
		}
	}
	return noSeriesIsNil(out), nil
}

// regexMatches returns the matches of re in text one at a time, as
// FindAllStringSubmatchIndex finds them, without finding them all at once
// when the series budget is smaller: it asks for one more match than the
// budget has room for, which is where the scrape fails if every match is a
// series, and only when some were not — a blank capture, a value its rule
// carried on without — for more, twice as many each time. regexp cannot
// resume a search where one stopped, since ^ and \b depend on the text
// before it, so each call searches from the start, and doubling keeps that
// to twice the work of one search. With the matches comes how many the first
// search found, which is all of them unless the budget cut it short.
func regexMatches(ctx context.Context, re *regexp.Regexp, text string) (func() ([]int, bool), int) {
	room := seriesRoom(ctx)
	want := -1
	if room >= 0 {
		want = room + 1
	}
	matches := re.FindAllStringSubmatchIndex(text, want)
	next := 0
	return func() ([]int, bool) {
		if next == len(matches) && want >= 0 && len(matches) == want {
			want = max(2*len(matches), len(matches)+seriesRoom(ctx)+1)
			more := re.FindAllStringSubmatchIndex(text, want)
			matches = more
		}
		if next == len(matches) {
			return nil, false
		}
		next++
		return matches[next-1], true
	}, len(matches)
}

// regexValueName names the capture group that is a regex rule's value:
// (?P<value>\d+). A value that stands after what a label reads is written
// that way, the station at the start of a report's line before the
// temperature near its end, where the first group would be the label's.
const regexValueName = "value"

// regexValueGroup is the number of the capture group that is the value of a
// regex rule: the one named value, and without one the first, which the
// configuration requires a regex to have.
func regexValueGroup(re *regexp.Regexp) int {
	if index := re.SubexpIndex(regexValueName); index > 0 {
		return index
	}
	return 1
}

// labelCaptures is, for each label of a regex rule, the capture group it
// reads, by number or by name, as captureIndex finds it: -1 for a group the
// expression has not, and for a static label, which reads none.
func labelCaptures(labels []model.LabelRule, names []string) []int {
	captures := make([]int, len(labels))
	for i, label := range labels {
		captures[i] = -1
		if !label.Static() {
			captures[i] = captureIndex(label.Expression, names)
		}
	}
	return captures
}

// regexSeries adds the series of one match to out; value is the rule's
// regexValueGroup and captures its labelCaptures. An error is the scrape's
// failure; a match the rule's error mode carries on without adds nothing.
func regexSeries(ctx context.Context, out *model.MetricSet, text string, match []int, value int, captures []int, rule model.MetricRule, c *model.Collector) error {
	// The capture group named value is the value, or else the first; the
	// configuration refuses a regex without one. A group that took no part
	// in the match, as an optional one can, or that captured only blanks, is
	// a missing value like a match that never happened.
	start, end := match[2*value], match[2*value+1]
	if start < 0 || isBlank(text[start:end]) {
		if requiredRule(rule, c) {
			group := "first capture group"
			if value != 1 {
				group = "capture group named " + regexValueName
			}
			missing := model.MarkError(fmt.Errorf("regex for metric %q matched, but its %s captured no value", rule.Name, group), model.ErrMissingValue)
			if handleMetricError(ctx, c, rule, missing) {
				return nil
			}
			return ruleFailure(c, rule, missing)
		}
		return nil
	}
	n, err := ruleTextValue(rule, text[start:end])
	if err != nil {
		if handleMetricError(ctx, c, rule, err) {
			return nil
		}
		return ruleFailure(c, rule, fmt.Errorf("metric %q: %w", rule.Name, err))
	}
	labels := make(map[string]string, len(rule.Labels))
	for i, label := range rule.Labels {
		if label.Static() {
			labels[label.Name] = label.Value
			continue
		}
		index := captures[i]
		if index >= 0 && 2*index+1 < len(match) && match[2*index] >= 0 {
			labels[label.Name] = text[match[2*index]:match[2*index+1]]
		}
	}
	if missing := missingRequiredLabel(rule, labels); missing != nil {
		if handleMetricError(ctx, c, rule, missing) {
			return nil
		}
		return ruleFailure(c, rule, missing)
	}
	if err := takeSeries(ctx); err != nil {
		return err
	}
	out.Metrics = append(out.Metrics, model.Metric{Name: rule.Name, Help: rule.Description, Type: rule.Type, Value: n, Labels: labels})
	return nil
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
type xpathNodes[N comparable] struct {
	// kind names the language in errors: "XPath" or "HTML XPath".
	kind string
	// html says the document is HTML, which has no namespaces: a label
	// that is an attribute is read by its whole name as written, a colon
	// included (ownAttributeLabel).
	html bool
	all  func(root N, e *xpath.Expr) []N
	one  func(node N, e *xpath.Expr) (N, bool)
	attr func(node N, name string) string
	text func(node N) string
	// navigate starts an evaluation at node, for an expression whose result
	// is a number, a string or a boolean rather than nodes.
	navigate func(node N) xpath.NodeNavigator
	// parent, first and next are the links of the tree, and leaf is the
	// text of a node that has text of its own rather than its children's:
	// a text node's, and a comment's, which is none. They are what text
	// reads, for selectedTexts to read the text of nested nodes once.
	parent, first, next func(node N) (N, bool)
	leaf                func(node N) (string, bool)
	// apart reports whether node's text is no part of the text of a node
	// around it, as that of a script and a style element of HTML is not
	// (htmlText); nil where every node's is, which is XML.
	apart func(node N) bool
	// What a label read by walking the tree needs (fastXPathLabel), each as
	// the engine's navigator of the document has it: whether node is an
	// element, the one kind of node a walk starts from; the value of
	// node's first attribute of a name without a prefix; its first child
	// element of such a name; and its first child that text() selects.
	element   func(node N) bool
	attribute func(node N, name string) (string, bool)
	child     func(node N, name string) (N, bool)
	firstText func(node N) (N, bool)
	// selectedAttribute is the value of node when node is an attribute a
	// rule selected, as `//@id` selects them: the label text() is that
	// value (xpathLabels).
	selectedAttribute func(node N) (string, bool)
}

// linked is a link of a tree as xpathNodes gives one: no node is none.
func linked[N comparable](node N) (N, bool) {
	var none N
	return node, node != none
}

// xmlNavigator is where the XPath engine starts from for node. For an
// attribute a rule selected, as `//@id` does, that is the attribute on its
// element: xmlquery hands a selected attribute over as a node of its own,
// from which its navigator cannot start — it panics, saying it does not know
// the node's type — so the navigator starts on the element and is moved to
// the attribute of that name and value. From there `.` is the attribute's
// value, `name()` its name and `..` its element, as XPath has them, and
// `text()` selects nothing: the label text() is given the value without the
// engine (xpathLabels).
func xmlNavigator(node *xmlquery.Node) xpath.NodeNavigator {
	if node.Type != xmlquery.AttributeNode || node.Parent == nil {
		return xmlquery.CreateXPathNavigator(node)
	}
	value := ""
	if node.FirstChild != nil {
		value = node.FirstChild.Data
	}
	at := xmlquery.CreateXPathNavigator(node.Parent)
	for at.MoveToNextAttribute() {
		if at.LocalName() == node.Data && at.Value() == value {
			return at
		}
	}
	// The element no longer has the attribute, which nothing here brings
	// about: its element stands in for it.
	return xmlquery.CreateXPathNavigator(node.Parent)
}

var xmlNodes = xpathNodes[*xmlquery.Node]{
	kind: "XPath",
	all:  xmlquery.QuerySelectorAll,
	one: func(node *xmlquery.Node, e *xpath.Expr) (*xmlquery.Node, bool) {
		if node.Type != xmlquery.AttributeNode {
			found := xmlquery.QuerySelector(node, e)
			return found, found != nil
		}
		// From an attribute, as xmlquery.QuerySelector selects from any
		// other node.
		it := e.Select(xmlNavigator(node))
		if !it.MoveNext() {
			return nil, false
		}
		at := it.Current().(*xmlquery.NodeNavigator)
		if at.NodeType() != xpath.AttributeNode {
			return at.Current(), true
		}
		value := &xmlquery.Node{Type: xmlquery.TextNode, Data: at.Value()}
		return &xmlquery.Node{Parent: at.Current(), Type: xmlquery.AttributeNode, Data: at.LocalName(), FirstChild: value, LastChild: value}, true
	},
	attr:     func(node *xmlquery.Node, name string) string { return node.SelectAttr(name) },
	text:     func(node *xmlquery.Node) string { return node.InnerText() },
	navigate: xmlNavigator,
	parent:   func(node *xmlquery.Node) (*xmlquery.Node, bool) { return linked(node.Parent) },
	first:    func(node *xmlquery.Node) (*xmlquery.Node, bool) { return linked(node.FirstChild) },
	next:     func(node *xmlquery.Node) (*xmlquery.Node, bool) { return linked(node.NextSibling) },
	leaf: func(node *xmlquery.Node) (string, bool) {
		switch node.Type {
		case xmlquery.TextNode, xmlquery.CharDataNode:
			return node.Data, true
		case xmlquery.CommentNode:
			return "", true
		}
		return "", false
	},
	element: func(node *xmlquery.Node) bool { return node.Type == xmlquery.ElementNode },
	// xmlquery's navigator finds attributes on an element alone, and tells
	// one with a prefix by the Space of its name.
	attribute: func(node *xmlquery.Node, name string) (string, bool) {
		if node.Type != xmlquery.ElementNode {
			return "", false
		}
		for i := range node.Attr {
			if attr := &node.Attr[i]; attr.Name.Local == name && attr.Name.Space == "" {
				return attr.Value, true
			}
		}
		return "", false
	},
	// It reads a processing instruction as an element named by its target.
	child: func(node *xmlquery.Node, name string) (*xmlquery.Node, bool) {
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			if (child.Type == xmlquery.ElementNode || child.Type == xmlquery.ProcessingInstruction) && child.Data == name && child.Prefix == "" {
				return child, true
			}
		}
		return nil, false
	},
	// It reads text, CDATA and a directive as text, and when it moves from
	// one child to the next it passes over text that is all blanks, which
	// it does not when it moves to the first child.
	firstText: func(node *xmlquery.Node) (*xmlquery.Node, bool) {
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			if child != node.FirstChild && child.Type == xmlquery.TextNode && strings.TrimSpace(child.Data) == "" {
				continue
			}
			switch child.Type {
			case xmlquery.TextNode, xmlquery.CharDataNode, xmlquery.NotationNode:
				return child, true
			}
		}
		return nil, false
	},
	selectedAttribute: func(node *xmlquery.Node) (string, bool) {
		if node.Type != xmlquery.AttributeNode {
			return "", false
		}
		return node.InnerText(), true
	},
}

// htmlAttribute is the value of node's attribute of a name as the document
// writes it. The parser keeps an attribute's name whole, as its key, colon
// and all, but for those HTML itself gives a namespace inside svg and math
// — xlink:href, xml:lang, xmlns:xlink — which it splits into the two: they
// are found by the name as written all the same. An attribute by its key
// comes first, as it always was read.
func htmlAttribute(node *html.Node, name string) (string, bool) {
	if node.Type != html.ElementNode {
		return "", false
	}
	for i := range node.Attr {
		if node.Attr[i].Key == name {
			return node.Attr[i].Val, true
		}
	}
	if prefix, local, prefixed := strings.Cut(name, ":"); prefixed && prefix != "" {
		for i := range node.Attr {
			if node.Attr[i].Namespace == prefix && node.Attr[i].Key == local {
				return node.Attr[i].Val, true
			}
		}
	}
	return "", false
}

// htmlSelected is the node an expression selected, where the engine stands
// at it. An attribute, as `//td/@data-value` selects them, is a node made
// for it as htmlquery makes one — an element by its type, named as the
// attribute, which holds the value as its text — and has the element it is
// an attribute of for its parent, which htmlquery's has not. Without the
// parent nothing but its own value and name could be read from a selected
// attribute: a label `../@data-server`, `../../@id` or `name(..)` was left
// off every series of the rule, which were then duplicates of one, where
// the same rule over XML told them apart.
func htmlSelected(at *htmlquery.NodeNavigator) *html.Node {
	if at.NodeType() != xpath.AttributeNode {
		return at.Current()
	}
	value := &html.Node{Type: html.TextNode, Data: at.Value()}
	return &html.Node{Parent: at.Current(), Type: html.ElementNode, Data: at.LocalName(), FirstChild: value, LastChild: value}
}

// selectedHTMLAttribute reports whether node is one htmlSelected made of an
// attribute, and no element of the document: it has its element for a
// parent and is no child of it, which no node of a parsed document is. A
// node htmlquery made of an attribute, as the first node a label's
// expression selects from an element is, has no parent and is read as the
// element it looks like, for its text.
func selectedHTMLAttribute(node *html.Node) bool {
	return node.Type == html.ElementNode && node.Parent != nil && node.PrevSibling == nil && node.Parent.FirstChild != node
}

// htmlNavigator is where the XPath engine starts from for node, as
// xmlNavigator is for XML: for an attribute a rule selected, the attribute
// on its element, found by its name and value, from where `.` is the
// attribute's value, `name()` its name and `..` its element, as XPath has
// them. Any other node is started from as htmlquery starts.
func htmlNavigator(node *html.Node) xpath.NodeNavigator {
	if !selectedHTMLAttribute(node) {
		return htmlquery.CreateXPathNavigator(node)
	}
	value := ""
	if node.FirstChild != nil {
		value = node.FirstChild.Data
	}
	at := htmlquery.CreateXPathNavigator(node.Parent)
	for at.MoveToNextAttribute() {
		if at.LocalName() == node.Data && at.Value() == value {
			return at
		}
	}
	// The element no longer has the attribute, which nothing here brings
	// about: its element stands in for it.
	return htmlquery.CreateXPathNavigator(node.Parent)
}

// htmlTextApart reports whether node is a script or a style element, whose
// text is no part of the text of an element around it (htmlText).
func htmlTextApart(node *html.Node) bool {
	return node.Type == html.ElementNode && (node.DataAtom == atom.Script || node.DataAtom == atom.Style)
}

// htmlText is the text of an HTML node as the css and xpath transforms read
// it: that of every text node beneath it, comments left out, and without
// what stands inside a script or a style element beneath it, which is code
// and no content. goquery's Text and htmlquery's InnerText, which read it
// before, took those in: a cell that held a number and an inline script was
// "6var n = 6;", and no number. The node itself may be a script or a style,
// or a text node inside one: a rule that selects one reads its text, the
// JSON a page was rendered from or a figure in an inline script. A page
// without a script or a style in what is read costs no more than it did:
// one walk of what is beneath the node, and for an element that holds one
// text node and nothing else, as the cell of a value or of a label nearly
// always is, that text as it stands, without building the same string anew.
func htmlText(node *html.Node) string {
	if only := node.FirstChild; only != nil && only.NextSibling == nil && only.Type == html.TextNode {
		return only.Data
	}
	var text strings.Builder
	writeHTMLText(&text, node)
	return text.String()
}

func writeHTMLText(text *strings.Builder, node *html.Node) {
	switch node.Type {
	case html.TextNode:
		text.WriteString(node.Data)
		return
	case html.CommentNode:
		return
	}
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		if !htmlTextApart(child) {
			writeHTMLText(text, child)
		}
	}
}

var htmlNodes = xpathNodes[*html.Node]{
	kind: "HTML XPath",
	html: true,
	// As htmlquery.QuerySelectorAll selects, with each node made by
	// htmlSelected.
	all: func(root *html.Node, e *xpath.Expr) []*html.Node {
		var selected []*html.Node
		it := e.Select(htmlquery.CreateXPathNavigator(root))
		for it.MoveNext() {
			selected = append(selected, htmlSelected(it.Current().(*htmlquery.NodeNavigator)))
		}
		return selected
	},
	one: func(node *html.Node, e *xpath.Expr) (*html.Node, bool) {
		if !selectedHTMLAttribute(node) {
			found := htmlquery.QuerySelector(node, e)
			return found, found != nil
		}
		// From an attribute, as htmlquery.QuerySelector selects from any
		// other node.
		it := e.Select(htmlNavigator(node))
		if !it.MoveNext() {
			return nil, false
		}
		return htmlSelected(it.Current().(*htmlquery.NodeNavigator)), true
	},
	attr: func(node *html.Node, name string) string {
		if selectedHTMLAttribute(node) {
			// A selected attribute has none of its own: asked for by its
			// own name it gives its value, as xmlquery's does and as
			// htmlquery's did while it had no parent.
			if name == node.Data {
				return htmlText(node)
			}
			return ""
		}
		if value := htmlquery.SelectAttr(node, name); value != "" {
			return value
		}
		value, _ := htmlAttribute(node, name)
		return value
	},
	text:     htmlText,
	navigate: htmlNavigator,
	parent:   func(node *html.Node) (*html.Node, bool) { return linked(node.Parent) },
	first:    func(node *html.Node) (*html.Node, bool) { return linked(node.FirstChild) },
	next:     func(node *html.Node) (*html.Node, bool) { return linked(node.NextSibling) },
	leaf: func(node *html.Node) (string, bool) {
		switch node.Type {
		case html.TextNode:
			return node.Data, true
		case html.CommentNode:
			return "", true
		}
		return "", false
	},
	apart: htmlTextApart,
	// A selected attribute is an element to the walks of fastXPathLabel, as
	// it was while it had no parent: its text is its value, it has nothing
	// beneath it but that, and its parent is its element, so each walk
	// reads from it what the engine reads from the attribute, without the
	// engine (TestXPathLabelFastPathsAgreeWithTheEngine).
	element: func(node *html.Node) bool { return node.Type == html.ElementNode },
	// htmlquery's navigator finds attributes on an element alone, by key,
	// and knows no prefix or namespace of an attribute or an element.
	attribute: htmlAttribute,
	child: func(node *html.Node, name string) (*html.Node, bool) {
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			if child.Type == html.ElementNode && child.Data == name {
				return child, true
			}
		}
		return nil, false
	},
	firstText: func(node *html.Node) (*html.Node, bool) {
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			if child.Type == html.TextNode {
				return child, true
			}
		}
		return nil, false
	},
	selectedAttribute: func(node *html.Node) (string, bool) {
		if !selectedHTMLAttribute(node) {
			return "", false
		}
		return htmlText(node), true
	},
}

// selectedTexts gives the text of each node a rule selected. A node's text is
// that of everything beneath it, so where the selected nodes nest, as with
// //section over sections within sections, reading each on its own reads the
// innermost once for every node around it: the square of the depth, for
// nodes nested one in the next. So once a node is found to lie within the
// one selected before it, as in document order the first nested node does,
// the text of the outer one is read in a single walk that notes where each
// selected node beneath it starts and ends, and theirs are parts of it.
// Nodes that do not nest are read as before, each on its own.
type selectedTexts[N comparable] struct {
	nodes    xpathNodes[N]
	selected []N
	// last is the node selected before the one asked for, while no nesting
	// has been found, and lastHolds says it has a node beneath it that
	// could hold another: without one, as the element of a value has none,
	// the next node is not asked where it lies.
	last      N
	hasLast   bool
	lastHolds bool
	// index is where each selected node is in selected, and texts the text
	// of those a walk has read, which have marks: all set once nesting is
	// found.
	index map[N]int
	texts []string
	have  []bool
}

// text is the text of selected[i], as nodes.text gives it.
func (s *selectedTexts[N]) text(i int) string {
	node := s.selected[i]
	if s.index == nil {
		if !s.hasLast || !s.lastHolds || !s.within(node, s.last) {
			s.last, s.hasLast, s.lastHolds = node, true, s.holds(node)
			return nodeText(s.nodes, node)
		}
		s.index = make(map[N]int, len(s.selected))
		for at, selected := range s.selected {
			s.index[selected] = at
		}
		s.texts, s.have = make([]string, len(s.selected)), make([]bool, len(s.selected))
		s.walk(s.last)
	}
	// A node selected twice is read as its last place in selected.
	at := s.index[node]
	if !s.have[at] {
		s.walk(node)
	}
	return s.texts[at]
}

// holds reports whether node has a child that is not a leaf, beneath which
// a selected node could nest.
func (s *selectedTexts[N]) holds(node N) bool {
	for child, ok := s.nodes.first(node); ok; child, ok = s.nodes.next(child) {
		if _, leaf := s.nodes.leaf(child); !leaf {
			return true
		}
	}
	return false
}

// within reports whether node lies beneath ancestor.
func (s *selectedTexts[N]) within(node, ancestor N) bool {
	for parent, ok := s.nodes.parent(node); ok; parent, ok = s.nodes.parent(parent) {
		if parent == ancestor {
			return true
		}
	}
	return false
}

// walk reads the text of top, itself selected, and of every selected node
// beneath it, in one pass over what is beneath it. A node whose text is no
// part of the text around it (xpathNodes.apart) is passed over with all that
// is selected within it, which text then reads each on its own.
func (s *selectedTexts[N]) walk(top N) {
	type span struct{ at, start, end int }
	var text strings.Builder
	var spans []span
	var read func(node N)
	read = func(node N) {
		at, selected := s.index[node]
		start := text.Len()
		if own, leaf := s.nodes.leaf(node); leaf {
			text.WriteString(own)
		} else {
			for child, ok := s.nodes.first(node); ok; child, ok = s.nodes.next(child) {
				if s.nodes.apart == nil || !s.nodes.apart(child) {
					read(child)
				}
			}
		}
		if selected {
			spans = append(spans, span{at, start, text.Len()})
		}
	}
	read(top)
	whole := text.String()
	for _, span := range spans {
		s.texts[span.at], s.have[span.at] = whole[span.start:span.end], true
	}
}

func transformXPath(ctx context.Context, root *xmlquery.Node, rules []model.MetricRule, c *model.Collector, namespaces map[string]string) (*model.MetricSet, error) {
	return transformXPathNodes(ctx, root, xmlNodes, rules, c, boundNamespaces(namespaces))
}

// transformHTMLXPath runs XPath over the document the html decoder already
// parsed: goquery and htmlquery both build it with html.Parse, so the
// document's root node is what htmlquery would have parsed from the body,
// but for the content of its templates (decode.ParseHTML).
func transformHTMLXPath(ctx context.Context, doc *goquery.Document, rules []model.MetricRule, c *model.Collector) (*model.MetricSet, error) {
	return transformXPathNodes(ctx, doc.Nodes[0], htmlNodes, rules, c, nil)
}

// transformXPathNodes evaluates each rule's expression against the document
// and makes a series of every node it selects. An expression that computes a
// value rather than selecting nodes — count(//job), sum(//size),
// string(/status/@load), a comparison — makes one series of that value, a
// boolean as 1 or 0. A label is a constant, an attribute of the node (@name),
// the text of the first node an expression relative to it selects, or the
// value an expression relative to it computes, as normalize-space(@name).
// xpathValue evaluates e at node when it computes a value — a number, a
// string or a boolean — rather than selecting nodes, which computed says.
func xpathValue[N comparable](nodes xpathNodes[N], node N, e *xpath.Expr) (value any, computed bool) {
	switch result := e.Evaluate(nodes.navigate(node)).(type) {
	case float64, string, bool:
		return result, true
	}
	return nil, false
}

// xpathText is a computed value as label text: a number as Go writes it
// shortest, a boolean as true or false.
func xpathText(value any) string {
	switch v := value.(type) {
	case float64:
		return strconv.FormatFloat(v, 'g', -1, 64)
	case bool:
		return strconv.FormatBool(v)
	case string:
		return v
	}
	return ""
}

// addComputedXPathSeries makes the one series of a rule whose expression
// computes a value. A number that is not one — number() of text, NaN — and
// an empty or non-numeric string are the rule's missing value, as is a sum()
// over text that is no number, which never comes here (xpathSumRule).
func addComputedXPathSeries[N comparable](ctx context.Context, out *model.MetricSet, nodes xpathNodes[N], root N, rule model.MetricRule, c *model.Collector, plan []xpathLabel, value any) error {
	var number float64
	var problem error
	switch v := value.(type) {
	case float64:
		if math.IsNaN(v) {
			problem = model.MarkError(fmt.Errorf("metric %q %s %q computed NaN, not a number", rule.Name, nodes.kind, rule.Expression), model.ErrMissingValue)
		}
	case string:
		if isBlank(v) {
			problem = model.MarkError(fmt.Errorf("metric %q %s %q computed an empty string", rule.Name, nodes.kind, rule.Expression), model.ErrMissingValue)
		}
	}
	if problem == nil {
		n, err := ruleValue(rule, value)
		if err != nil {
			problem = fmt.Errorf("metric %q: %w", rule.Name, err)
		}
		number = n
	}
	if problem != nil {
		if errors.Is(problem, model.ErrMissingValue) && !requiredRule(rule, c) {
			return nil
		}
		if handleMetricError(ctx, c, rule, problem) {
			return nil
		}
		return ruleFailure(c, rule, problem)
	}
	if unreadable := unreadableXPathLabel(nodes, rule, plan); unreadable != nil {
		if handleMetricError(ctx, c, rule, unreadable) {
			return nil
		}
		return ruleFailure(c, rule, unreadable)
	}
	labels := xpathLabels(nodes, root, plan)
	if summed := summedXPathLabels(plan); summed != nil {
		if unreadable := readSummedXPathLabels(nodes, root, rule, plan, summed, -1, labels); unreadable != nil {
			if handleMetricError(ctx, c, rule, unreadable) {
				return nil
			}
			return ruleFailure(c, rule, unreadable)
		}
	}
	if missing := missingRequiredLabel(rule, labels); missing != nil {
		if handleMetricError(ctx, c, rule, missing) {
			return nil
		}
		return ruleFailure(c, rule, missing)
	}
	if err := takeSeries(ctx); err != nil {
		return err
	}
	out.Metrics = append(out.Metrics, model.Metric{Name: rule.Name, Help: rule.Description, Type: rule.Type, Value: number, Labels: labels})
	return nil
}

func transformXPathNodes[N comparable](ctx context.Context, root N, nodes xpathNodes[N], rules []model.MetricRule, c *model.Collector, namespaces map[string]string) (*model.MetricSet, error) {
	out := &model.MetricSet{}
	for _, rule := range rules {
		program, err := expr.CompileXPath(rule.Expression, namespaces)
		if err != nil {
			err = fmt.Errorf("metric %q %s %q: %w", rule.Name, nodes.kind, rule.Expression, err)
			if handleMetricError(ctx, c, rule, err) {
				continue
			}
			return nil, ruleFailure(c, rule, err)
		}
		// A panic of the engine while the rule is evaluated is the rule's
		// failure (xpathfailure.go).
		if err := evaluateXPathRule(ctx, out, root, nodes, rule, c, namespaces, program); err != nil {
			return nil, err
		}
	}
	return noSeriesIsNil(out), nil
}

// xpathRule adds the series of one rule, whose compiled expression and
// labels' plan it is given, to out. An error is the scrape's failure; a
// series the rule's error mode carries on without is left out. A failure
// names the metric first, and the node it belongs to by its place among the
// nodes the rule selected, from 0, as a css rule's names its item. sums are
// the calls of sum() the exporter reads the nodes of (xpathsum.go), nil for
// an expression without one.
func xpathRule[N comparable](ctx context.Context, out *model.MetricSet, root N, nodes xpathNodes[N], rule model.MetricRule, c *model.Collector, plan []xpathLabel, expression *xpath.Expr, sums *expr.XPathSums) error {
	if sums != nil {
		if done, err := xpathSumRule(ctx, out, nodes, root, rule, c, plan, sums); done {
			return err
		}
	}
	if value, computed := xpathValue(nodes, root, expression); computed {
		return addComputedXPathSeries(ctx, out, nodes, root, rule, c, plan, value)
	}
	selected := nodes.all(root, expression)
	if len(selected) == 0 {
		if requiredRule(rule, c) {
			missing := model.MarkError(fmt.Errorf("metric %q %s %q matched no nodes", rule.Name, nodes.kind, rule.Expression), model.ErrMissingValue)
			if handleMetricError(ctx, c, rule, missing) {
				return nil
			}
			return ruleFailure(c, rule, missing)
		}
		return nil
	}
	// A label that cannot be read in this kind of document is the rule's
	// failure, once, rather than a label left off each of its series.
	if unreadable := unreadableXPathLabel(nodes, rule, plan); unreadable != nil {
		if handleMetricError(ctx, c, rule, unreadable) {
			return nil
		}
		return ruleFailure(c, rule, unreadable)
	}
	out.Metrics = growSeries(ctx, out.Metrics, len(selected))
	texts := selectedTexts[N]{nodes: nodes, selected: selected}
	summed := summedXPathLabels(plan)
	for i, node := range selected {
		// A node costs what is beneath it to read, and a label what its
		// expression walks, so the deadline is asked after before each.
		if err := interrupted(ctx, rule); err != nil {
			return err
		}
		labels := xpathLabels(nodes, node, plan)
		text := texts.text(i)
		if isBlank(text) {
			if requiredRule(rule, c) {
				missing := model.MarkError(model.Errorf("metric %q value is missing for node %d: %s %q selected a node without a value", rule.Name, model.Position(i), nodes.kind, rule.Expression), model.ErrMissingValue)
				if handleMetricError(ctx, c, rule, missing) {
					continue
				}
				return ruleFailure(c, rule, missing)
			}
			continue
		}
		value, err := ruleTextValue(rule, text)
		if err != nil {
			err = model.Errorf("metric %q node %d: %w", rule.Name, model.Position(i), err)
			if handleMetricError(ctx, c, rule, err) {
				continue
			}
			return ruleFailure(c, rule, err)
		}
		if summed != nil {
			if unreadable := readSummedXPathLabels(nodes, node, rule, plan, summed, i, labels); unreadable != nil {
				if handleMetricError(ctx, c, rule, unreadable) {
					continue
				}
				return ruleFailure(c, rule, unreadable)
			}
		}
		if missing := missingRequiredLabel(rule, labels); missing != nil {
			missing = model.Errorf("%w for node %d", missing, model.Position(i))
			if handleMetricError(ctx, c, rule, missing) {
				continue
			}
			return ruleFailure(c, rule, missing)
		}
		if err := takeSeries(ctx); err != nil {
			return err
		}
		out.Metrics = append(out.Metrics, model.Metric{Name: rule.Name, Help: rule.Description, Type: rule.Type, Value: value, Labels: labels})
	}
	return nil
}

func transformCSS(ctx context.Context, doc *goquery.Document, rules []model.MetricRule, c *model.Collector) (*model.MetricSet, error) {
	out := &model.MetricSet{}
	// A rule without items makes one series, and one with items adds its
	// own room when it knows how many it selected.
	single := 0
	for i := range rules {
		if rules[i].Items == "" {
			single++
		}
	}
	out.Metrics = growSeries(ctx, out.Metrics, single)
	for _, rule := range rules {
		if rule.Items != "" {
			if err := transformCSSItems(ctx, out, doc, rule, c); err != nil {
				return nil, err
			}
			continue
		}
		matcher, err := expr.CompileCSS(rule.Expression)
		if err != nil {
			err = fmt.Errorf("metric %q CSS selector %q: %w", rule.Name, rule.Expression, err)
			if handleMetricError(ctx, c, rule, err) {
				continue
			}
			return nil, ruleFailure(c, rule, err)
		}
		selection := doc.FindMatcher(matcher)
		if selection.Length() == 0 {
			if requiredRule(rule, c) {
				missing := model.MarkError(fmt.Errorf("metric %q CSS selector %q matched no nodes", rule.Name, rule.Expression), model.ErrMissingValue)
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
			err := model.Errorf("metric %q CSS selector %q matched %d elements, but without items a css metric is one value; set items to the elements, such as '#servers tr:has(td)', and select the value and each label within one", rule.Name, rule.Expression, model.Size(selection.Length()))
			if handleMetricError(ctx, c, rule, err) {
				continue
			}
			return nil, ruleFailure(c, rule, err)
		}
		text := strings.TrimSpace(htmlText(selection.Nodes[0]))
		if isBlank(text) {
			if requiredRule(rule, c) {
				missing := model.MarkError(fmt.Errorf("metric %q value is missing: CSS selector %q matched an element without a value", rule.Name, rule.Expression), model.ErrMissingValue)
				if handleMetricError(ctx, c, rule, missing) {
					continue
				}
				return nil, ruleFailure(c, rule, missing)
			}
			continue
		}
		value, err := ruleTextValue(rule, text)
		if err != nil {
			err = fmt.Errorf("metric %q: %w", rule.Name, err)
			if handleMetricError(ctx, c, rule, err) {
				continue
			}
			return nil, ruleFailure(c, rule, err)
		}
		labels := make(map[string]string, len(rule.Labels))
		for _, label := range rule.Labels {
			if label.Static() {
				labels[label.Name] = label.Value
			}
		}
		if err := takeSeries(ctx); err != nil {
			return nil, err
		}
		out.Metrics = append(out.Metrics, model.Metric{Name: rule.Name, Help: rule.Description, Type: rule.Type, Value: value, Labels: labels})
	}
	return noSeriesIsNil(out), nil
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
func transformCSSItems(ctx context.Context, out *model.MetricSet, doc *goquery.Document, rule model.MetricRule, c *model.Collector) error {
	fail := func(err error) (bool, error) {
		if handleMetricError(ctx, c, rule, err) {
			return true, nil
		}
		return false, ruleFailure(c, rule, err)
	}
	itemsMatcher, err := expr.CompileCSS(rule.Items)
	if err != nil {
		_, failure := fail(fmt.Errorf("metric %q items CSS selector %q: %w", rule.Name, rule.Items, err))
		return failure
	}
	valueMatcher, err := expr.CompileCSS(rule.Expression)
	if err != nil {
		_, failure := fail(fmt.Errorf("metric %q CSS selector %q: %w", rule.Name, rule.Expression, err))
		return failure
	}
	items := doc.FindMatcher(itemsMatcher)
	if items.Length() == 0 && requiredRule(rule, c) {
		_, failure := fail(model.MarkError(fmt.Errorf("metric %q items CSS selector %q matched no nodes", rule.Name, rule.Items), model.ErrMissingValue))
		return failure
	}
	// The rule adds its series to the set itself, with room made for one of
	// each item.
	out.Metrics = growSeries(ctx, out.Metrics, items.Length())
	// one is the trimmed text of the element selector matches within item, or
	// false when it matches none.
	one := func(item *goquery.Selection, index int, selector string, matcher goquery.Matcher) (string, bool, error) {
		found := item.FindMatcher(matcher)
		switch found.Length() {
		case 0:
			return "", false, nil
		case 1:
			return strings.TrimSpace(htmlText(found.Nodes[0])), true, nil
		default:
			return "", false, model.Errorf("metric %q item %d: CSS selector %q matched %d elements; within an item it must match at most one", rule.Name, model.Position(index), selector, model.Size(found.Length()))
		}
	}
	for index := range items.Length() {
		if err := interrupted(ctx, rule); err != nil {
			return err
		}
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
			err = model.MarkError(model.Errorf("metric %q value is missing for item %d: CSS selector %q %s", rule.Name, model.Position(index), rule.Expression, why), model.ErrMissingValue)
		}
		var value float64
		if err == nil {
			value, err = ruleTextValue(rule, text)
			if err != nil {
				err = model.Errorf("metric %q item %d: %w", rule.Name, model.Position(index), err)
			}
		}
		labels := make(map[string]string, len(rule.Labels))
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
				err = model.Errorf("%w for item %d", missing, model.Position(index))
			}
		}
		if err != nil {
			if carryOn, failure := fail(err); !carryOn {
				return failure
			}
			continue
		}
		if err := takeSeries(ctx); err != nil {
			return err
		}
		out.Metrics = append(out.Metrics, model.Metric{Name: rule.Name, Help: rule.Description, Type: rule.Type, Value: value, Labels: labels})
	}
	return nil
}

// csvRow is a decoded CSV row as a mapping of column to value: by header
// name, or, with response.csv.header false, by column number from 1, as the
// rules name them then.
func csvRow(raw any) map[string]any {
	switch row := raw.(type) {
	case map[string]any:
		return row
	case []any:
		out := make(map[string]any, len(row))
		for i, value := range row {
			out[strconv.Itoa(i+1)] = value
		}
		return out
	}
	return nil
}

// csvNotRows is the failure of a csv transform that was given something
// other than rows, which says what it was given and by whom. The csv decoder
// gives rows whatever the body holds, and a response another decoder read is
// refused before it gets here (validateTransformInput), so on a scrape it is
// a pre-script that left something else in data: the message said `CSV
// transform requires a header-based CSV response`, which blamed the response.
// It is the script's failure then, and marked as one, as what a pre-script
// of a css or xpath transform leaves that is no markup is (applyPreScript).
func csvNotRows(c *model.Collector, data any) error {
	if strings.TrimSpace(c.Transform.PreScript) != "" {
		return model.MarkError(fmt.Errorf("python pre-script of a csv transform left data as %s; it must leave a list of rows, each row a dict by column name or a list by column number", showScriptValue(data)), model.ErrScriptFailed)
	}
	return fmt.Errorf("csv transform reads the rows the csv decoder gives, and the decoded response is %s; set decoder.type to csv", model.ShowValue(data))
}

// csvNotARow is the failure of a csv transform at a row, counted from 1,
// that is neither a dict nor a list, as a pre-script may leave one: every
// column of it was missing, as if the response lacked them. A pre-script's
// is marked as the script's failure, as csvNotRows' is.
func csvNotARow(c *model.Collector, number int, row any) error {
	if strings.TrimSpace(c.Transform.PreScript) != "" {
		return model.MarkError(model.Errorf("python pre-script of a csv transform left row %d as %s; a row must be a dict by column name or a list by column number", model.Position(number), showScriptValue(row)), model.ErrScriptFailed)
	}
	return model.Errorf("csv transform reads rows that are a mapping by column name or a list by column number, and row %d of the decoded response is %s", model.Position(number), model.ShowValue(row))
}

// csvColumnsShown is how many of a response's column names an error lists.
const csvColumnsShown = 12

// csvColumns answers whether the rows of one response have a column, which
// tells a column the response does not have from a cell that is empty: the
// first is a rule that names what is not there, a header in another case or
// split by another delimiter, and reads the same on every scrape; the second
// is the data's. A response has a column when any row of it does: with a
// header row every row has every column the header names, the ones a short
// row lacks being empty in it; without one, a row has the columns up to its
// own length; and the rows a pre-script left have the keys it gave each.
//
// Nothing is looked for until a row lacks a column a rule names, and then
// the rows are gone through once for that column and the answer kept.
type csvColumns struct {
	rows []any
	// has is whether the response has a column, for each one asked after;
	// absent the error of each that it does not have.
	has    map[string]bool
	absent map[string]csvAbsentColumn
	// shown is what the response's columns are, as an error says it, made
	// when one first does, of names, the names of the rows read by name, in
	// order, and numbered, what the rows read by number have; listed says
	// those two were gathered.
	shown    string
	names    []string
	numbered string
	listed   bool
}

// csvAbsentColumn is the error of a column no row has: as a label's failure
// has it, and marked as the missing value it is of a rule that reads its
// value from the column.
type csvAbsentColumn struct{ err, missing error }

// inResponse reports whether any row has the column.
func (k *csvColumns) inResponse(column string) bool {
	if has, asked := k.has[column]; asked {
		return has
	}
	// A row without a header row has its columns by number, from 1, written
	// as the transform names them (csvRow).
	number := 0
	if n, err := strconv.Atoi(column); err == nil && n > 0 && strconv.Itoa(n) == column {
		number = n
	}
	has := false
	for _, raw := range k.rows {
		switch row := raw.(type) {
		case map[string]any:
			_, has = row[column]
		case []any:
			has = number > 0 && number <= len(row)
		}
		if has {
			break
		}
	}
	if k.has == nil {
		k.has = map[string]bool{}
	}
	k.has[column] = has
	return has
}

// notInResponse is the error of a column no row has, which names the columns
// the rows do have: one error for each column, however many rows ask.
func (k *csvColumns) notInResponse(column string) csvAbsentColumn {
	if absent, made := k.absent[column]; made {
		return absent
	}
	err := fmt.Errorf("CSV column %q is not in the response, %s", column, k.describe(column))
	absent := csvAbsentColumn{err: err, missing: model.MarkError(err, model.ErrMissingValue)}
	if k.absent == nil {
		k.absent = map[string]csvAbsentColumn{}
	}
	k.absent[column] = absent
	return absent
}

// describe says which columns the rows have, to a rule that asked for
// column: the names of rows read by name, the first csvColumnsShown of them,
// and how many columns the longest of the rows read by number has. The
// names are in order, but for those that are column except for the case of
// their letters or the blanks around them, which come first: the list is
// cut, and in a wide table the name that says what is wrong — used, to a
// rule that names Used — would be among those left out. The rows are gone
// through once, and the text of a column that has no such name is made
// once.
func (k *csvColumns) describe(column string) string {
	if !k.listed {
		named, longest := map[string]struct{}{}, 0
		for _, raw := range k.rows {
			switch row := raw.(type) {
			case map[string]any:
				for name := range row {
					if _, seen := named[name]; !seen {
						named[name] = struct{}{}
						k.names = append(k.names, name)
					}
				}
			case []any:
				longest = max(longest, len(row))
			}
		}
		slices.Sort(k.names)
		switch {
		case longest == 1:
			k.numbered = "longest row has 1 column, read by number as 1"
		case longest > 1:
			k.numbered = fmt.Sprintf("longest row has %d columns, read by number from 1 to %d", longest, longest)
		}
		k.listed = true
	}
	for _, name := range k.names {
		if csvNearColumn(name, column) {
			return csvColumnsText(k.names, k.numbered, column)
		}
	}
	if k.shown == "" {
		k.shown = csvColumnsText(k.names, k.numbered, column)
	}
	return k.shown
}

// csvNearColumn reports whether name is column but for the case of its
// letters and the blanks around it.
func csvNearColumn(name, column string) bool {
	return strings.EqualFold(strings.TrimSpace(name), strings.TrimSpace(column))
}

// csvColumnsText puts a response's columns into words: the first
// csvColumnsShown of names, those near column first (csvNearColumn), and
// numbered, what its rows read by number have.
func csvColumnsText(names []string, numbered, column string) string {
	if len(names) == 0 {
		if numbered == "" {
			return "which has no columns"
		}
		return "whose " + numbered
	}
	var text strings.Builder
	text.WriteString("whose columns are ")
	if len(names) == 1 {
		text.Reset()
		text.WriteString("whose only column is ")
	}
	shown := 0
	for _, near := range []bool{true, false} {
		for _, name := range names {
			if shown == csvColumnsShown {
				break
			}
			if csvNearColumn(name, column) != near {
				continue
			}
			if shown > 0 {
				text.WriteString(", ")
			}
			text.WriteString(model.QuoteValue(name))
			shown++
		}
	}
	if more := len(names) - csvColumnsShown; more > 0 {
		fmt.Fprintf(&text, " and %d more", more)
	}
	if numbered != "" {
		text.WriteString(", and whose ")
		text.WriteString(numbered)
	}
	text.WriteString("; column names are matched exactly")
	return text.String()
}

func transformCSV(ctx context.Context, data any, rules []model.MetricRule, c *model.Collector) (*model.MetricSet, error) {
	rows, ok := data.([]any)
	if !ok {
		return nil, csvNotRows(c, data)
	}
	out := &model.MetricSet{}
	// A response without a row has no value for any rule, as a regex that
	// matched no text has none, and a jq items expression that selected
	// nothing: a required rule is missing its value, and its error mode
	// decides, rather than the scrape passing with nothing.
	if len(rows) == 0 {
		for _, rule := range rules {
			if !requiredRule(rule, c) {
				continue
			}
			missing := model.MarkError(fmt.Errorf("CSV column %q is missing: the response has no rows", rule.Expression), model.ErrMissingValue)
			if handleMetricError(ctx, c, rule, missing) {
				continue
			}
			return nil, ruleFailure(c, rule, missing)
		}
	}
	// Room for a series of every row for each rule whose column the response
	// has, which the first row tells of rows read by header name.
	//
	// The rows are read one after another, each by every rule, so what fails
	// first, what is logged and where the series limit stops are as the rows'
	// order makes them. The series are kept rule by rule, though, each rule's
	// together in the order of the rows: the exposition formats want a
	// metric's series together, and a set that has them apart costs the
	// writer a second pass on every probe (appendMetricSet in
	// internal/exporter). So with several rules each rule with a column has a
	// part of the room, as long as the rows are many, one part after another
	// in the rules' order: next[r] is where rule r's next series goes and
	// ends[r] where its part ends. The parts are closed up afterwards over
	// what the rows left empty. Without room for the parts within the series
	// limit there are none, and a rule without a part, whose column only a
	// later row has, as a pre-script may leave them, adds its series after
	// the parts; both leave the series in the order of the rows, as one rule
	// has them anyway.
	var next, ends []int
	parts := 0
	if len(rows) > 0 {
		columns := len(rules)
		first, named := rows[0].(map[string]any)
		if named {
			columns = 0
			for i := range rules {
				if _, exists := first[rules[i].Expression]; exists {
					columns++
				}
			}
		}
		if room := seriesRoom(ctx); len(rules) > 1 && columns > 0 && (room < 0 || room >= len(rows)*columns) {
			parts = len(rows) * columns
			out.Metrics = make([]model.Metric, parts)
			next, ends = make([]int, len(rules)), make([]int, len(rules))
			at := 0
			for i := range rules {
				next[i] = at
				if _, exists := first[rules[i].Expression]; exists || !named {
					at += len(rows)
				}
				ends[i] = at
			}
		} else {
			out.Metrics = growSeries(ctx, out.Metrics, len(rows)*columns)
		}
	}
	responseColumns := csvColumns{rows: rows}
	// unreadable is the rules that have failed for a label whose column the
	// response does not have, which fail once and make no series.
	var unreadable []bool
	for at, raw := range rows {
		if ctx.Err() != nil {
			return nil, interruptedAt(ctx, "")
		}
		row := csvRow(raw)
		if row == nil {
			if _, named := raw.(map[string]any); !named {
				return nil, csvNotARow(c, at+1, raw)
			}
		}
		for r, rule := range rules {
			if unreadable != nil && unreadable[r] {
				continue
			}
			value, exists := row[rule.Expression]
			if !exists || blankValue(value) {
				if requiredRule(rule, c) {
					// A column no row has, or a cell that is empty in this
					// row, the header's line not counted among the rows.
					var missing error
					if !exists && !responseColumns.inResponse(rule.Expression) {
						missing = responseColumns.notInResponse(rule.Expression).missing
					} else {
						missing = model.MarkError(model.Errorf("CSV column %q is empty in row %d", rule.Expression, model.Position(at+1)), model.ErrMissingValue)
					}
					if handleMetricError(ctx, c, rule, missing) {
						continue
					}
					return nil, ruleFailure(c, rule, missing)
				}
				continue
			}
			n, err := ruleValue(rule, value)
			if err != nil {
				if handleMetricError(ctx, c, rule, err) {
					continue
				}
				return nil, ruleFailure(c, rule, fmt.Errorf("metric %q: %w", rule.Name, err))
			}
			labels := make(map[string]string, len(rule.Labels))
			var labelErr error
			for _, label := range rule.Labels {
				if label.Static() {
					labels[label.Name] = label.Value
				} else if labelValue, exists := row[label.Expression]; exists && labelValue != nil {
					// A pre-script may leave numbers and None in a row:
					// a number is written as the other transforms write
					// one, and None leaves the label out.
					text, err := labelText(labelValue)
					if err != nil {
						labelErr = fmt.Errorf("metric %q label %q %w", rule.Name, label.Name, err)
						break
					}
					labels[label.Name] = text
				} else if !exists && !responseColumns.inResponse(label.Expression) {
					// A column no row has is the rule's failure, once for
					// the response, as a label that cannot be read is in
					// the xpath transform (unreadableXPathLabel), and not a
					// label left off every series without a word. A cell
					// that is empty leaves the label off.
					labelErr = fmt.Errorf("metric %q label %q: %w", rule.Name, label.Name, responseColumns.notInResponse(label.Expression).err)
					if unreadable == nil {
						unreadable = make([]bool, len(rules))
					}
					unreadable[r] = true
					break
				}
			}
			if labelErr != nil {
				if handleMetricError(ctx, c, rule, labelErr) {
					continue
				}
				return nil, ruleFailure(c, rule, labelErr)
			}
			if missing := missingRequiredLabel(rule, labels); missing != nil {
				if handleMetricError(ctx, c, rule, missing) {
					continue
				}
				return nil, ruleFailure(c, rule, missing)
			}
			if err := takeSeries(ctx); err != nil {
				return nil, err
			}
			series := model.Metric{Name: rule.Name, Help: rule.Description, Type: rule.Type, Value: n, Labels: labels}
			if parts > 0 && next[r] < ends[r] {
				out.Metrics[next[r]] = series
				next[r]++
				continue
			}
			out.Metrics = append(out.Metrics, series)
		}
	}
	if parts > 0 {
		// Each part's series moved down to where the part before it ended,
		// then the series added after the parts, and what is left over
		// cleared, so that it keeps no labels alive.
		filled, start := 0, 0
		for r := range next {
			if filled != start {
				copy(out.Metrics[filled:], out.Metrics[start:next[r]])
			}
			filled += next[r] - start
			start = ends[r]
		}
		if filled < parts {
			filled += copy(out.Metrics[filled:], out.Metrics[parts:])
			clear(out.Metrics[filled:])
			out.Metrics = out.Metrics[:filled]
		}
	}
	return noSeriesIsNil(out), nil
}

// prometheusRuleUnmatched words the missing value of a prometheus rule that
// matched no series: by its expression when it has one, and by its name,
// which is then what it matches, when it has not.
func prometheusRuleUnmatched(rule model.MetricRule) error {
	switch {
	case rule.Expression == "":
		return fmt.Errorf("metric %q is not in the response", rule.Name)
	case rule.Name == "":
		return fmt.Errorf("expression %q matched no metric in the response", rule.Expression)
	}
	return fmt.Errorf("metric %q expression %q matched no metric in the response", rule.Name, rule.Expression)
}

// applyPrometheusTransform makes the collector's series of the series a
// prometheus response was decoded into, copying of them only what it
// changes. borrowed says the set's series are the decoded ones themselves
// (transformMetrics).
//
// A series is a struct of several words and a map of labels, and a scrape of
// a node_exporter is thousands of them, so what is not copied is most of
// what this transform would otherwise cost:
//
//   - without rules, include, exclude or rename, every series passes as it
//     is. When the collector also sets nothing that Transform applies to its
//     output afterwards (passesThrough), the decoded slice itself is
//     handed on; otherwise the series are copied into a slice of their own,
//     once, at its final size;
//   - a series a rule gives no label keeps the label map it was decoded
//     with, shared with the decoded series and with every other rule's copy
//     of it. That is safe because nothing after this writes into a label
//     map it did not make: SanitizeUTF8, mapLabelValues, truncateLabels and
//     escapeNames copy a series' labels before they change one,
//     applyCollectorLabels gives every series a new map, and what is done
//     with a transform's output beyond Transform (the cache, a static
//     target's labels, a directory's file label, OTLP's queue) copies the
//     series it keeps or changes;
//   - the slice the series are gathered in is made once, for as many series
//     as the response has or the room limits.max_metrics leaves, whichever
//     is less (growSeries), instead of growing from nothing.
func applyPrometheusTransform(ctx context.Context, in model.MetricSet, c *model.Collector, t model.TransformConfig, rules []model.MetricRule) (set *model.MetricSet, borrowed bool, err error) {
	if len(rules) > 0 {
		out := model.MetricSet{}
		// Each rule's pattern is compiled once, not once per series; a rule
		// whose pattern does not compile is left without one.
		patterns := make([]*regexp.Regexp, len(rules))
		for i, rule := range rules {
			pattern := rule.Expression
			if pattern == "" {
				pattern = "^" + regexp.QuoteMeta(rule.Name) + "$"
			}
			re, err := expr.CompileRegex(pattern)
			if err != nil {
				if handleMetricError(ctx, c, rule, err) {
					continue
				}
				return nil, false, ruleFailure(c, rule, fmt.Errorf("metric %q expression: %w", rule.Name, err))
			}
			patterns[i] = re
		}
		matched := make([]bool, len(rules))
		// The decoder kept only the series a rule's name or pattern can
		// match (decode.Decode), so nearly every one of them becomes a
		// series here.
		out.Metrics = growSeries(ctx, out.Metrics, len(in.Metrics))
		for s := range in.Metrics {
			source := &in.Metrics[s]
			for i := range rules {
				rule := &rules[i]
				if patterns[i] == nil || !patterns[i].MatchString(source.Name) {
					continue
				}
				matched[i] = true
				metric := *source
				if rule.Name != "" {
					metric.Name = rule.Name
				}
				if rule.Description != "" {
					metric.Help = rule.Description
				}
				if rule.Type != "" && rule.Type != metric.Type {
					// A histogram's or summary's type is its shape: it
					// cannot be exported as another, nor another as one.
					if metric.Histogram != nil || metric.Summary != nil || rule.Type == model.HistogramMetricType || rule.Type == model.SummaryMetricType {
						err := fmt.Errorf("metric %q type %s cannot apply to %s, a %s: a histogram or summary keeps its own type, and no other series can become one", rule.Name, rule.Type, source.Name, source.Type)
						if handleMetricError(ctx, c, *rule, err) {
							continue
						}
						return nil, false, ruleFailure(c, *rule, err)
					}
					metric.Type = rule.Type
				}
				if rule.Scale != nil {
					if metric.Histogram != nil || metric.Summary != nil {
						err := fmt.Errorf("metric %q scale cannot apply to %s, a %s: its buckets and quantiles are bounds as well as counts", rule.Name, source.Name, source.Type)
						if handleMetricError(ctx, c, *rule, err) {
							continue
						}
						return nil, false, ruleFailure(c, *rule, err)
					}
					metric.Value = scaled(*rule, metric.Value)
				}
				switch {
				case len(rule.Labels) > 0:
					// The series gets labels of its own, with room for the
					// rule's: the rule adds and removes them, and another
					// rule may match the same source metric.
					metric.Labels = make(map[string]string, len(source.Labels)+len(rule.Labels))
					for name, value := range source.Labels {
						metric.Labels[name] = value
					}
				case source.Labels == nil:
					// Never nil, as the labels of a rule's series never
					// were.
					metric.Labels = map[string]string{}
				}
				for _, label := range rule.Labels {
					if label.Static() {
						metric.Labels[label.Name] = label.Value
					} else if value, ok := metric.Labels[label.Expression]; ok {
						metric.Labels[label.Name] = value
					}
					// A rule without a name keeps each series' own, which
					// truncateLabels cannot look it up by, so its labels are
					// cut here.
					if label.Truncate && c.Limits.MaxLabelValueLength > 0 {
						if value, ok := metric.Labels[label.Name]; ok {
							setTruncated(metric.Labels, label.Name, value, c.Limits.MaxLabelValueLength)
						}
					}
				}
				if len(rule.Labels) > 0 {
					if missing := missingRequiredLabel(*rule, metric.Labels); missing != nil {
						if handleMetricError(ctx, c, *rule, missing) {
							continue
						}
						return nil, false, ruleFailure(c, *rule, missing)
					}
				}
				if err := takeSeries(ctx); err != nil {
					return nil, false, err
				}
				out.Metrics = append(out.Metrics, metric)
			}
		}
		// A rule no series' name matched has no value, as a regex that
		// matched no text has none: a required rule is missing its value,
		// and its error mode decides, rather than the scrape passing
		// without the metric it was written for.
		for i, rule := range rules {
			if patterns[i] == nil || matched[i] || !requiredRule(rule, c) {
				continue
			}
			missing := model.MarkError(prometheusRuleUnmatched(rule), model.ErrMissingValue)
			if handleMetricError(ctx, c, rule, missing) {
				continue
			}
			return nil, false, ruleFailure(c, rule, missing)
		}
		return noSeriesIsNil(&out), false, nil
	}
	if len(t.Include) == 0 && len(t.Exclude) == 0 && len(t.Rename) == 0 {
		// Counted at once, with the error the first series past the limit
		// would have had.
		if err := takeSeriesN(ctx, len(in.Metrics)); err != nil {
			return nil, false, err
		}
		switch {
		case len(in.Metrics) == 0:
			return &model.MetricSet{}, false, nil
		case passesThrough(c):
			return &model.MetricSet{Metrics: in.Metrics}, true, nil
		}
		return &model.MetricSet{Metrics: slices.Clone(in.Metrics)}, false, nil
	}
	out := model.MetricSet{}
	includes := make([]*regexp.Regexp, 0, len(t.Include))
	for _, expression := range t.Include {
		re, err := expr.CompileRegex(expression)
		if err != nil {
			return nil, false, err
		}
		includes = append(includes, re)
	}
	excludes := make([]*regexp.Regexp, 0, len(t.Exclude))
	for _, expression := range t.Exclude {
		re, err := expr.CompileRegex(expression)
		if err != nil {
			return nil, false, err
		}
		excludes = append(excludes, re)
	}
	// The decoder kept only the series include and exclude pass
	// (decode.Decode), so nearly every one of them is a series here.
	out.Metrics = growSeries(ctx, out.Metrics, len(in.Metrics))
	for i := range in.Metrics {
		name := in.Metrics[i].Name
		included := len(includes) == 0
		for _, expression := range includes {
			if expression.MatchString(name) {
				included = true
			}
		}
		for _, expression := range excludes {
			if expression.MatchString(name) {
				included = false
			}
		}
		if !included {
			continue
		}
		if err := takeSeries(ctx); err != nil {
			return nil, false, err
		}
		out.Metrics = append(out.Metrics, in.Metrics[i])
		if renamed, ok := t.Rename[name]; ok {
			out.Metrics[len(out.Metrics)-1].Name = renamed
		}
	}
	return noSeriesIsNil(&out), false, nil
}

// passesThrough reports whether Transform leaves the series of a
// prometheus pass-through as they were decoded, so that they need no copy:
// nothing it applies to a transform's output writes to them. A prefix and an
// escaped name change a series' name, and transform.labels, remove_labels and
// rename_labels give it another label map; label value maps and truncation
// belong to rules, which a pass-through has none of; and a repair of invalid
// UTF-8, which only reading the series tells, Transform makes in a copy.
func passesThrough(c *model.Collector) bool {
	t := c.Transform
	return c.MetricsPrefix == "" &&
		(c.NameEscaping == "" || c.NameEscaping == NameEscapingFail) &&
		len(t.Labels) == 0 && len(t.RemoveLabels) == 0 && len(t.RenameLabels) == 0
}
