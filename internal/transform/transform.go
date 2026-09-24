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
	set, err := transformMetrics(ctx, d, r, c, pythonPath)
	if err != nil || set == nil {
		return set, err
	}
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
			return applyPrometheusTransform(ms, c, c.Transform, c.Metrics)
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
		return transformRegex(text, c.Metrics, c)
	case "css":
		h, ok := d.Data.(*decode.HTMLDecoded)
		if !ok {
			return nil, errors.New("CSS transformation requires HTML data")
		}
		return transformCSS(h.Document, c.Metrics, c)
	case "csv":
		return transformCSV(d.Data, c.Metrics, c)
	case "xpath":
		if h, ok := d.Data.(*decode.HTMLDecoded); ok {
			return transformHTMLXPath(h.Raw, c.Metrics, c)
		}
		n, ok := d.Data.(*xmlquery.Node)
		if !ok {
			return nil, errors.New("XPath transformation requires XML or HTML data")
		}
		return transformXPath(n, c.Metrics, c, c.Response.Namespaces)
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

// handleMetricError applies a rule's error mode and reports whether the scrape
// should carry on without the metric. ignore carries on silently, log carries
// on and says why, and fail says why and stops: the caller then returns the
// failure wrapped by ruleFailure, and the scrape fails as a whole rather than
// serving the metrics that did work.
//
// The collector is named alongside the rule because a metric name is not unique
// across collectors, and without it a logged failure does not say which
// collector to go and look at. The line is written here, once, for both log and
// fail, so a failing rule reads the same in the log whichever mode it has and
// whether it failed on a probe or on a scheduled target.
//
// This logs through slog's default logger rather than one passed down: it is
// called from inside the transforms, several frames below anything holding a
// logger. The exporter installs its JSON logger as the process default at
// startup so these lines match every other line it writes.
func handleMetricError(c *model.Collector, rule model.MetricRule, err error) bool {
	switch rule.ErrorMode {
	case model.ErrorModeIgnore:
		return true
	case model.ErrorModeLog:
		slog.Default().Error("metric extraction failed", "collector", collectorName(c), "metric", rule.Name, "error_mode", rule.ErrorMode, "error", err)
		return true
	case model.ErrorModeFail:
		slog.Default().Error("metric extraction failed", "collector", collectorName(c), "metric", rule.Name, "error_mode", rule.ErrorMode, "error", err)
	}
	return false
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
			if handleMetricError(c, rule, err) {
				continue
			}
			return nil, ruleFailure(c, rule, fmt.Errorf("metric %q expression: %w", rule.Name, err))
		}
		labels, err := evaluateLabels(ctx, data, rule.Labels, len(values))
		if err != nil {
			if handleMetricError(c, rule, err) {
				continue
			}
			return nil, ruleFailure(c, rule, fmt.Errorf("metric %q labels: %w", rule.Name, err))
		}
		if len(values) == 0 && requiredRule(rule, c) {
			missing := model.MarkError(fmt.Errorf("metric %q value is missing", rule.Name), model.ErrMissingValue)
			if handleMetricError(c, rule, missing) {
				continue
			}
			return nil, ruleFailure(c, rule, missing)
		}
		for index, value := range values {
			if value == nil {
				if requiredRule(rule, c) {
					missing := model.MarkError(fmt.Errorf("metric %q value is missing", rule.Name), model.ErrMissingValue)
					if handleMetricError(c, rule, missing) {
						continue
					}
					return nil, ruleFailure(c, rule, missing)
				}
				continue
			}
			n, err := model.Number(value)
			if err != nil {
				if handleMetricError(c, rule, err) {
					continue
				}
				return nil, ruleFailure(c, rule, fmt.Errorf("metric %q: %w", rule.Name, err))
			}
			if missing := missingRequiredLabel(rule, labels[index]); missing != nil {
				if handleMetricError(c, rule, missing) {
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
		if handleMetricError(c, rule, err) {
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
		if value == nil {
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
		// Values are paired with series by position, so a required label
		// giving a different number of values than there are series would
		// land on the wrong ones.
		if label.Required && len(values) != 1 && len(values) != metricCount {
			return nil, fmt.Errorf("label %q gave %d values for %d series; a required label must give one value, or one per series", label.Name, len(values), metricCount)
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

func requiredRule(rule model.MetricRule, c *model.Collector) bool {
	return (rule.Required == nil || *rule.Required) && !c.ErrorHandling.AllowMissingKeys
}

func transformRegex(text string, rules []model.MetricRule, c *model.Collector) (*model.MetricSet, error) {
	out := &model.MetricSet{}
	for _, rule := range rules {
		re, err := expr.CompileRegex(rule.Expression)
		if err != nil {
			if handleMetricError(c, rule, err) {
				continue
			}
			return nil, ruleFailure(c, rule, fmt.Errorf("metric %q regex: %w", rule.Name, err))
		}
		matches := re.FindAllStringSubmatchIndex(text, -1)
		if len(matches) == 0 {
			if requiredRule(rule, c) {
				missing := model.MarkError(fmt.Errorf("regex for metric %q matched no text", rule.Name), model.ErrMissingValue)
				if handleMetricError(c, rule, missing) {
					continue
				}
				return nil, ruleFailure(c, rule, missing)
			}
			continue
		}
		names := re.SubexpNames()
		for _, match := range matches {
			capture := 1
			if len(match) < 4 {
				capture = 0
			}
			if 2*capture+1 >= len(match) {
				if handleMetricError(c, rule, fmt.Errorf("metric %q regex has no capture group", rule.Name)) {
					break
				}
				return nil, ruleFailure(c, rule, fmt.Errorf("metric %q regex has no capture group", rule.Name))
			}
			n, err := strconv.ParseFloat(text[match[2*capture]:match[2*capture+1]], 64)
			if err != nil {
				if handleMetricError(c, rule, err) {
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
				if handleMetricError(c, rule, missing) {
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

func transformXPath(root *xmlquery.Node, rules []model.MetricRule, c *model.Collector, namespaces map[string]string) (*model.MetricSet, error) {
	return transformXPathNodes(root, xmlNodes, rules, c, namespaces)
}

func transformHTMLXPath(raw []byte, rules []model.MetricRule, c *model.Collector) (*model.MetricSet, error) {
	root, err := htmlquery.Parse(strings.NewReader(string(raw)))
	if err != nil {
		return nil, fmt.Errorf("HTML XPath parse: %w", err)
	}
	return transformXPathNodes(root, htmlNodes, rules, c, nil)
}

// transformXPathNodes evaluates each rule's expression against the document
// and makes a series of every node it selects. A label is a constant, an
// attribute of the node (@name), or the text of the first node an expression
// relative to it selects.
func transformXPathNodes[N any](root N, nodes xpathNodes[N], rules []model.MetricRule, c *model.Collector, namespaces map[string]string) (*model.MetricSet, error) {
	out := &model.MetricSet{}
	for _, rule := range rules {
		expression, err := expr.CompileXPath(rule.Expression, namespaces)
		if err != nil {
			if handleMetricError(c, rule, err) {
				continue
			}
			return nil, ruleFailure(c, rule, fmt.Errorf("%s %q: %w", nodes.kind, rule.Expression, err))
		}
		selected := nodes.all(root, expression)
		if len(selected) == 0 {
			if requiredRule(rule, c) {
				missing := model.MarkError(fmt.Errorf("%s %q matched no nodes", nodes.kind, rule.Expression), model.ErrMissingValue)
				if handleMetricError(c, rule, missing) {
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
			value, err := decode.TextValue(nodes.text(node))
			if err != nil {
				if handleMetricError(c, rule, err) {
					continue
				}
				return nil, ruleFailure(c, rule, fmt.Errorf("metric %q: %w", rule.Name, err))
			}
			if missing := missingRequiredLabel(rule, labels); missing != nil {
				if handleMetricError(c, rule, missing) {
					continue
				}
				return nil, ruleFailure(c, rule, missing)
			}
			out.Metrics = append(out.Metrics, model.Metric{Name: rule.Name, Help: rule.Description, Type: rule.Type, Value: value, Labels: labels})
		}
	}
	return out, nil
}

func transformCSS(doc *goquery.Document, rules []model.MetricRule, c *model.Collector) (*model.MetricSet, error) {
	out := &model.MetricSet{}
	for _, rule := range rules {
		if rule.Items != "" {
			metrics, err := transformCSSItems(doc, rule, c)
			if err != nil {
				return nil, err
			}
			out.Metrics = append(out.Metrics, metrics...)
			continue
		}
		matcher, err := expr.CompileCSS(rule.Expression)
		if err != nil {
			if handleMetricError(c, rule, err) {
				continue
			}
			return nil, ruleFailure(c, rule, fmt.Errorf("CSS selector %q: %w", rule.Expression, err))
		}
		selection := doc.FindMatcher(matcher)
		if selection.Length() == 0 {
			if requiredRule(rule, c) {
				missing := model.MarkError(fmt.Errorf("CSS selector %q matched no nodes", rule.Expression), model.ErrMissingValue)
				if handleMetricError(c, rule, missing) {
					continue
				}
				return nil, ruleFailure(c, rule, missing)
			}
			continue
		}
		var transformErr, failure error
		selection.Each(func(_ int, node *goquery.Selection) {
			if transformErr != nil || failure != nil {
				return
			}
			value, err := decode.TextValue(strings.TrimSpace(node.Text()))
			if err != nil {
				transformErr = fmt.Errorf("metric %q: %w", rule.Name, err)
				return
			}
			labels := map[string]string{}
			for _, label := range rule.Labels {
				if label.Static() {
					labels[label.Name] = label.Value
				} else if selector, err := expr.CompileCSS(label.Expression); err == nil {
					labels[label.Name] = strings.TrimSpace(node.FindMatcher(selector).First().Text())
				}
			}
			// A series without a required label is dropped alone, or fails
			// the rule, by its error_mode.
			if missing := missingRequiredLabel(rule, labels); missing != nil {
				if !handleMetricError(c, rule, missing) {
					failure = ruleFailure(c, rule, missing)
				}
				return
			}
			out.Metrics = append(out.Metrics, model.Metric{Name: rule.Name, Help: rule.Description, Type: rule.Type, Value: value, Labels: labels})
		})
		if failure != nil {
			return nil, failure
		}
		if transformErr != nil {
			if handleMetricError(c, rule, transformErr) {
				continue
			}
			return nil, ruleFailure(c, rule, transformErr)
		}
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
func transformCSSItems(doc *goquery.Document, rule model.MetricRule, c *model.Collector) ([]model.Metric, error) {
	fail := func(err error) (bool, error) {
		if handleMetricError(c, rule, err) {
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
		if err == nil && !found {
			if !requiredRule(rule, c) {
				continue
			}
			err = model.MarkError(fmt.Errorf("metric %q value is missing for item %d: CSS selector %q matched nothing", rule.Name, index, rule.Expression), model.ErrMissingValue)
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

func transformCSV(data any, rules []model.MetricRule, c *model.Collector) (*model.MetricSet, error) {
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
			if !exists || strings.TrimSpace(fmt.Sprint(value)) == "" {
				if requiredRule(rule, c) {
					missing := model.MarkError(fmt.Errorf("CSV column %q is missing", rule.Expression), model.ErrMissingValue)
					if handleMetricError(c, rule, missing) {
						continue
					}
					return nil, ruleFailure(c, rule, missing)
				}
				continue
			}
			n, err := model.Number(value)
			if err != nil {
				if handleMetricError(c, rule, err) {
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
				if handleMetricError(c, rule, missing) {
					continue
				}
				return nil, ruleFailure(c, rule, missing)
			}
			out.Metrics = append(out.Metrics, model.Metric{Name: rule.Name, Help: rule.Description, Type: rule.Type, Value: n, Labels: labels})
		}
	}
	return out, nil
}

func applyPrometheusTransform(in model.MetricSet, c *model.Collector, t model.TransformConfig, rules []model.MetricRule) (*model.MetricSet, error) {
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
					if handleMetricError(c, rule, err) {
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
					if handleMetricError(c, rule, missing) {
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
		if metric.Labels == nil {
			metric.Labels = map[string]string{}
		}
		for name, value := range t.Labels {
			metric.Labels[name] = value
		}
		for _, name := range t.RemoveLabels {
			delete(metric.Labels, name)
		}
		for old, name := range t.RenameLabels {
			if value, ok := metric.Labels[old]; ok {
				delete(metric.Labels, old)
				metric.Labels[name] = value
			}
		}
		out.Metrics = append(out.Metrics, metric)
	}
	return &out, nil
}
