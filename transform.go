package main

import (
	"bytes"
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/PuerkitoBio/goquery"
	"github.com/antchfx/htmlquery"
	"github.com/antchfx/xmlquery"
	"github.com/antchfx/xpath"
	"github.com/itchyny/gojq"
)

func transform(ctx context.Context, d *Decoded, r *HTTPResponse, c *Collector, pythonPath string) (*MetricSet, error) {
	if strings.TrimSpace(c.Transform.PreScript) != "" {
		processed, err := applyPreScript(ctx, d, r, c, pythonPath)
		if err != nil {
			return nil, err
		}
		d = processed
	}
	if c.Transform.Type == "python" {
		if c.Transform.Script == "" {
			return nil, fmt.Errorf("Python transform requires a script")
		}
		return executePython(ctx, pythonPath, c.Transform.Script, d, r, c)
	}
	if ms, ok := d.Data.(MetricSet); ok {
		if c.Transform.Type == "" || c.Transform.Type == "prometheus" {
			return applyPrometheusTransform(ms, c.Transform, c.Metrics)
		}
		return nil, fmt.Errorf("unsupported transformation %q for Prometheus", c.Transform.Type)
	}
	switch c.Transform.Type {
	case "", "none", "jq", "yq":
		return transformJQ(ctx, d.Data, c.Metrics, c)
	case "regex":
		text, ok := d.Data.(string)
		if !ok {
			return nil, fmt.Errorf("regex transformation requires text data")
		}
		return transformRegex(text, c.Metrics, c)
	case "css":
		h, ok := d.Data.(*HTMLDecoded)
		if !ok {
			return nil, fmt.Errorf("CSS transformation requires HTML data")
		}
		return transformCSS(h.Document, c.Metrics, c)
	case "csv":
		return transformCSV(d.Data, c.Metrics, c)
	case "xpath":
		if h, ok := d.Data.(*HTMLDecoded); ok {
			return transformHTMLXPath(h.Raw, c.Metrics, c)
		}
		n, ok := d.Data.(*xmlquery.Node)
		if !ok {
			return nil, fmt.Errorf("XPath transformation requires XML or HTML data")
		}
		return transformXPath(n, c.Metrics, c, c.Response.Namespaces)
	default:
		return nil, fmt.Errorf("unsupported transformation %q", c.Transform.Type)
	}
}

func applyPreScript(ctx context.Context, d *Decoded, r *HTTPResponse, c *Collector, pythonPath string) (*Decoded, error) {
	data, err := executePythonPreScript(ctx, pythonPath, c.Transform.PreScript, d, r, c)
	if err != nil {
		return nil, err
	}
	if d.Kind == "html" {
		raw := []byte(fmt.Sprint(data))
		doc, parseErr := goquery.NewDocumentFromReader(bytes.NewReader(raw))
		if parseErr != nil {
			return nil, fmt.Errorf("HTML pre-script output: %w", parseErr)
		}
		return &Decoded{Kind: "html", Data: &HTMLDecoded{Document: doc, Raw: raw}, Raw: raw}, nil
	}
	if d.Kind == "xml" {
		raw := []byte(fmt.Sprint(data))
		node, parseErr := xmlquery.Parse(bytes.NewReader(raw))
		if parseErr != nil {
			return nil, fmt.Errorf("XML pre-script output: %w", parseErr)
		}
		return &Decoded{Kind: "xml", Data: node, Raw: raw}, nil
	}
	return &Decoded{Kind: d.Kind, Data: data, Raw: d.Raw}, nil
}

func transformJQ(ctx context.Context, data any, rules []MetricRule, c *Collector) (*MetricSet, error) {
	out := &MetricSet{}
	for _, rule := range rules {
		values, err := evaluateJQ(ctx, data, rule.Expression)
		if err != nil {
			return nil, fmt.Errorf("metric %q expression: %w", rule.Name, err)
		}
		labels, err := evaluateLabels(ctx, data, rule.Labels)
		if err != nil {
			return nil, fmt.Errorf("metric %q labels: %w", rule.Name, err)
		}
		if len(values) == 0 && requiredRule(rule, c) {
			return nil, fmt.Errorf("metric %q value is missing", rule.Name)
		}
		for _, value := range values {
			if value == nil {
				continue
			}
			n, err := number(value)
			if err != nil {
				return nil, fmt.Errorf("metric %q: %w", rule.Name, err)
			}
			out.Metrics = append(out.Metrics, Metric{Name: rule.Name, Help: rule.Description, Type: rule.Type, Value: n, Labels: cloneLabels(labels)})
		}
	}
	return out, nil
}

func evaluateJQ(ctx context.Context, data any, expression string) ([]any, error) {
	query, err := gojq.Parse(expression)
	if err != nil {
		return nil, err
	}
	code, err := gojq.Compile(query)
	if err != nil {
		return nil, err
	}
	iterator := code.Run(data)
	values := []any{}
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
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

func evaluateLabels(ctx context.Context, data any, expressions []LabelRule) (map[string]string, error) {
	labels := map[string]string{}
	for _, label := range expressions {
		if label.Type == "string" {
			labels[label.Name] = label.Value
			continue
		}
		values, err := evaluateJQ(ctx, data, label.Expression)
		if err != nil {
			return nil, fmt.Errorf("label %q: %w", label.Name, err)
		}
		if len(values) > 0 && values[0] != nil {
			labels[label.Name] = fmt.Sprint(values[0])
		}
	}
	return labels, nil
}

func requiredRule(rule MetricRule, c *Collector) bool {
	return (rule.Required == nil || *rule.Required) && !c.ErrorHandling.AllowMissingKeys
}

func transformRegex(text string, rules []MetricRule, c *Collector) (*MetricSet, error) {
	out := &MetricSet{}
	for _, rule := range rules {
		re, err := regexp.Compile(rule.Expression)
		if err != nil {
			return nil, fmt.Errorf("metric %q regex: %w", rule.Name, err)
		}
		matches := re.FindAllStringSubmatchIndex(text, -1)
		if len(matches) == 0 {
			if requiredRule(rule, c) {
				return nil, fmt.Errorf("regex for metric %q matched no text", rule.Name)
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
				return nil, fmt.Errorf("metric %q regex has no capture group", rule.Name)
			}
			n, err := strconv.ParseFloat(text[match[2*capture]:match[2*capture+1]], 64)
			if err != nil {
				return nil, fmt.Errorf("metric %q: %w", rule.Name, err)
			}
			labels := map[string]string{}
			for _, label := range rule.Labels {
				if label.Type == "string" {
					labels[label.Name] = label.Value
					continue
				}
				index := captureIndex(label.Expression, names)
				if index >= 0 && 2*index+1 < len(match) && match[2*index] >= 0 {
					labels[label.Name] = text[match[2*index]:match[2*index+1]]
				}
			}
			out.Metrics = append(out.Metrics, Metric{Name: rule.Name, Help: rule.Description, Type: rule.Type, Value: n, Labels: labels})
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

func transformXPath(root *xmlquery.Node, rules []MetricRule, c *Collector, namespaces map[string]string) (*MetricSet, error) {
	out := &MetricSet{}
	for _, rule := range rules {
		var nodes []*xmlquery.Node
		var err error
		if len(namespaces) > 0 {
			expression, compileErr := xpath.CompileWithNS(rule.Expression, namespaces)
			if compileErr != nil {
				return nil, fmt.Errorf("XPath %q: %w", rule.Expression, compileErr)
			}
			nodes = xmlquery.QuerySelectorAll(root, expression)
		} else {
			nodes, err = xmlquery.QueryAll(root, rule.Expression)
			if err != nil {
				return nil, fmt.Errorf("XPath %q: %w", rule.Expression, err)
			}
		}
		if len(nodes) == 0 {
			if requiredRule(rule, c) {
				return nil, fmt.Errorf("XPath %q matched no nodes", rule.Expression)
			}
			continue
		}
		for _, node := range nodes {
			labels := map[string]string{}
			for _, label := range rule.Labels {
				if label.Type == "string" {
					labels[label.Name] = label.Value
				} else if strings.HasPrefix(label.Expression, "@") {
					labels[label.Name] = node.SelectAttr(strings.TrimPrefix(label.Expression, "@"))
				} else if value := xmlquery.FindOne(node, label.Expression); value != nil {
					labels[label.Name] = value.InnerText()
				}
			}
			value, err := textValue(node.InnerText())
			if err != nil {
				return nil, fmt.Errorf("metric %q: %w", rule.Name, err)
			}
			out.Metrics = append(out.Metrics, Metric{Name: rule.Name, Help: rule.Description, Type: rule.Type, Value: value, Labels: labels})
		}
	}
	return out, nil
}

func transformHTMLXPath(raw []byte, rules []MetricRule, c *Collector) (*MetricSet, error) {
	root, err := htmlquery.Parse(strings.NewReader(string(raw)))
	if err != nil {
		return nil, fmt.Errorf("HTML XPath parse: %w", err)
	}
	out := &MetricSet{}
	for _, rule := range rules {
		nodes, err := htmlquery.QueryAll(root, rule.Expression)
		if err != nil {
			return nil, fmt.Errorf("HTML XPath %q: %w", rule.Expression, err)
		}
		if len(nodes) == 0 {
			if requiredRule(rule, c) {
				return nil, fmt.Errorf("HTML XPath %q matched no nodes", rule.Expression)
			}
			continue
		}
		for _, node := range nodes {
			labels := map[string]string{}
			for _, label := range rule.Labels {
				if label.Type == "string" {
					labels[label.Name] = label.Value
				} else if strings.HasPrefix(label.Expression, "@") {
					labels[label.Name] = htmlquery.SelectAttr(node, strings.TrimPrefix(label.Expression, "@"))
				} else if value := htmlquery.FindOne(node, label.Expression); value != nil {
					labels[label.Name] = htmlquery.InnerText(value)
				}
			}
			value, err := textValue(htmlquery.InnerText(node))
			if err != nil {
				return nil, fmt.Errorf("metric %q: %w", rule.Name, err)
			}
			out.Metrics = append(out.Metrics, Metric{Name: rule.Name, Help: rule.Description, Type: rule.Type, Value: value, Labels: labels})
		}
	}
	return out, nil
}

func transformCSS(doc *goquery.Document, rules []MetricRule, c *Collector) (*MetricSet, error) {
	out := &MetricSet{}
	for _, rule := range rules {
		selection := doc.Find(rule.Expression)
		if selection.Length() == 0 {
			if requiredRule(rule, c) {
				return nil, fmt.Errorf("CSS selector %q matched no nodes", rule.Expression)
			}
			continue
		}
		var transformErr error
		selection.Each(func(_ int, node *goquery.Selection) {
			if transformErr != nil {
				return
			}
			value, err := textValue(strings.TrimSpace(node.Text()))
			if err != nil {
				transformErr = fmt.Errorf("metric %q: %w", rule.Name, err)
				return
			}
			labels := map[string]string{}
			for _, label := range rule.Labels {
				if label.Type == "string" {
					labels[label.Name] = label.Value
				} else {
					labels[label.Name] = strings.TrimSpace(node.Find(label.Expression).First().Text())
				}
			}
			out.Metrics = append(out.Metrics, Metric{Name: rule.Name, Help: rule.Description, Type: rule.Type, Value: value, Labels: labels})
		})
		if transformErr != nil {
			return nil, transformErr
		}
	}
	return out, nil
}

func transformCSV(data any, rules []MetricRule, c *Collector) (*MetricSet, error) {
	rows, ok := data.([]any)
	if !ok {
		return nil, fmt.Errorf("CSV transform requires a header-based CSV response")
	}
	out := &MetricSet{}
	for _, raw := range rows {
		row, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		for _, rule := range rules {
			value, exists := row[rule.Expression]
			if !exists || strings.TrimSpace(fmt.Sprint(value)) == "" {
				if requiredRule(rule, c) {
					return nil, fmt.Errorf("CSV column %q is missing", rule.Expression)
				}
				continue
			}
			n, err := number(value)
			if err != nil {
				return nil, fmt.Errorf("metric %q: %w", rule.Name, err)
			}
			labels := map[string]string{}
			for _, label := range rule.Labels {
				if label.Type == "string" {
					labels[label.Name] = label.Value
				} else if labelValue, exists := row[label.Expression]; exists {
					labels[label.Name] = fmt.Sprint(labelValue)
				}
			}
			out.Metrics = append(out.Metrics, Metric{Name: rule.Name, Help: rule.Description, Type: rule.Type, Value: n, Labels: labels})
		}
	}
	return out, nil
}

func applyPrometheusTransform(in MetricSet, t TransformConfig, rules []MetricRule) (*MetricSet, error) {
	if len(rules) > 0 {
		out := MetricSet{}
		for _, source := range in.Metrics {
			for _, rule := range rules {
				pattern := rule.Expression
				if pattern == "" {
					pattern = "^" + regexp.QuoteMeta(rule.Name) + "$"
				}
				matched, err := regexp.MatchString(pattern, source.Name)
				if err != nil {
					return nil, fmt.Errorf("metric %q expression: %w", rule.Name, err)
				}
				if !matched {
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
				if metric.Labels == nil {
					metric.Labels = map[string]string{}
				}
				for _, label := range rule.Labels {
					if label.Type == "string" {
						metric.Labels[label.Name] = label.Value
					} else if value, ok := metric.Labels[label.Expression]; ok {
						metric.Labels[label.Name] = value
					}
				}
				out.Metrics = append(out.Metrics, metric)
			}
		}
		return &out, nil
	}
	out := MetricSet{}
	includes := make([]*regexp.Regexp, 0, len(t.Include))
	for _, expression := range t.Include {
		re, err := regexp.Compile(expression)
		if err != nil {
			return nil, err
		}
		includes = append(includes, re)
	}
	excludes := make([]*regexp.Regexp, 0, len(t.Exclude))
	for _, expression := range t.Exclude {
		re, err := regexp.Compile(expression)
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
