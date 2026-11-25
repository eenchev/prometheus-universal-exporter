package fetch

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A probe's parameters fill the request (pathparams.go, requesttemplate.go).
// By the same rules they also fill the two places where a collector writes
// a fixed label value: the values of transform.labels, and the value of a
// metric rule's static label. One collector can then label its series with
// a value that only the scrape knows:
//
//	transform:
//	  type: jq
//	  labels:
//	    tenant: "{{param_tenant}}"
//	    region: "{{param_region:eu}}"
//	metrics:
//	  - name: status_up
//	    expression: .up
//	    labels:
//	      - {name: source, value: "api-{{param_tenant}}"}
//
// probed with &param_tenant=acme exports
// status_up{region="eu",source="api-acme",tenant="acme"}.
//
// A label value is written like a header value: {{param_<name>}} or
// {{param_<name>:<default>}}, any number of them mixed with plain text.
// `{{` opens a placeholder only where param_ follows. There is no filter,
// because a label value is written one way, as given; escaping a series'
// labels is the exposition writers' job. The only thing a value may not be
// is text that is not UTF-8, which no label is.
//
// The placeholders are read once, when the configuration loads
// (ParseLabelParams), and kept with the collector (model.LabelParams). A
// collector whose labels hold none, as nearly all do, costs a probe one
// nil pointer check. For a collector that has some, a probe is checked
// with the request's placeholders (CheckRequestParams), so a missing value
// is the caller's 400 before the target is contacted. Its transform gets a
// copy of the collector with the values filled in (FilledLabels).
// Everything that reads a label value, including the limits and the
// collector-wide label settings, then treats a filled value as the same
// text written in the configuration.

// labelUnfiltered is why a placeholder of a label value takes no filter.
const labelUnfiltered = "a label value is written one way, as it is given, so write the placeholder without a |, which a default cannot hold either"

// ParseLabelParams reads the placeholders of c's label values: those of
// transform.labels and those of its metric rules' static labels. Each rule
// is named by ruleName, as the collector's other errors name it. It
// returns nil when no value holds a placeholder. It refuses, naming the
// collector and the value, a placeholder that is not well formed
// (unclosed, written with a space after the braces, named otherwise than
// param_<name>, with a filter or a brace in its default) and a default
// that is not valid UTF-8.
//
// A label that has an expression is not static, whatever its value says,
// and is not read: the rule check refuses it.
func ParseLabelParams(c *model.Collector, ruleName func(index int) string) (*model.LabelParams, error) {
	var out model.LabelParams
	var errs []error
	parse := func(t model.LabelTemplate) (model.LabelTemplate, bool) {
		placeholders, err := parsePlaceholdersOf(t.Where, t.Text, false, false, labelUnfiltered)
		if err != nil {
			errs = append(errs, fmt.Errorf("collector %q %w", c.Name, err))
			return t, false
		}
		for _, p := range placeholders {
			if p.Clock != nil {
				errs = append(errs, fmt.Errorf("collector %q %s: the clock placeholder {{%s}} may stand in a request, not in a label value: a label whose value changes with the clock makes a new series every day, hour or second", c.Name, t.Where, p.Name))
				return t, false
			}
			if !utf8.ValidString(p.Default) {
				errs = append(errs, fmt.Errorf("collector %q %s: the default of %s is not valid UTF-8, which a label value must be; change the default", c.Name, t.Where, p.Name))
				return t, false
			}
			t.Placeholders = append(t.Placeholders, model.LabelPlaceholder{Param: p.Name, Default: p.Default, HasDefault: p.HasDefault, Start: p.start, End: p.end})
		}
		return t, len(t.Placeholders) > 0
	}
	for _, name := range model.SortedKeys(c.Transform.Labels) {
		if value := c.Transform.Labels[name]; HasPathParams(value) {
			if t, ok := parse(model.LabelTemplate{Name: name, Rule: -1, Label: -1, Where: "transform.labels." + name, Text: value}); ok {
				out.Collector = append(out.Collector, t)
			}
		}
	}
	for i := range c.Metrics {
		for j, label := range c.Metrics[i].Labels {
			if !label.Static() || !HasPathParams(label.Value) {
				continue
			}
			if t, ok := parse(model.LabelTemplate{Name: label.Name, Rule: i, Label: j, Where: fmt.Sprintf("%s label %q value", ruleName(i), label.Name), Text: label.Value}); ok {
				out.Rules = append(out.Rules, t)
			}
		}
	}
	if err := model.JoinProblems(errs...); err != nil {
		return nil, err
	}
	if len(out.Collector) == 0 && len(out.Rules) == 0 {
		return nil, nil
	}
	return &out, nil
}

// labelPlaceholders are the placeholders of a collector's label values as
// the request's are listed (RequestParams).
func labelPlaceholders(labels *model.LabelParams) []pathPlaceholder {
	if labels == nil {
		return nil
	}
	var out []pathPlaceholder
	for _, templates := range [][]model.LabelTemplate{labels.Collector, labels.Rules} {
		for i := range templates {
			for _, p := range templates[i].Placeholders {
				out = append(out, pathPlaceholder{Name: p.Param, Default: p.Default, HasDefault: p.HasDefault})
			}
		}
	}
	return out
}

// labelPlaceholderValue returns the value that fills a placeholder of a
// label value: the probe's, else the default, else an error, as for a
// request placeholder (placeholderValue). It also returns an error for a
// value that is not valid UTF-8, whoever gave it.
func labelPlaceholderValue(t *model.LabelTemplate, p model.LabelPlaceholder, params map[string]string) (string, error) {
	value, err := placeholderValue(t.Where, pathPlaceholder{Name: p.Param, Default: p.Default, HasDefault: p.HasDefault}, params)
	if err != nil {
		return "", err
	}
	if !utf8.ValidString(value) {
		return "", fmt.Errorf("%s: the value of %s is not valid UTF-8, which a label value must be", t.Where, p.Param)
	}
	return value, nil
}

// checkLabelParams binds every placeholder of a collector's label values
// against the parameters, as CheckRequestParams binds the request's, and
// notes in used the parameters they name.
func checkLabelParams(labels *model.LabelParams, params map[string]string, used map[string]bool) error {
	for _, templates := range [][]model.LabelTemplate{labels.Collector, labels.Rules} {
		for i := range templates {
			t := &templates[i]
			for _, p := range t.Placeholders {
				used[p.Param] = true
				if _, err := labelPlaceholderValue(t, p, params); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// labelValue is a label value with its placeholders filled in.
func labelValue(t *model.LabelTemplate, params map[string]string) (string, error) {
	// A value that is one placeholder and nothing else is the parameter's
	// value itself, which is not written anew.
	if len(t.Placeholders) == 1 && t.Placeholders[0].Start == 0 && t.Placeholders[0].End == len(t.Text) {
		return labelPlaceholderValue(t, t.Placeholders[0], params)
	}
	var b strings.Builder
	previous := 0
	for _, p := range t.Placeholders {
		value, err := labelPlaceholderValue(t, p, params)
		if err != nil {
			return "", err
		}
		b.WriteString(t.Text[previous:p.Start])
		b.WriteString(value)
		previous = p.End
	}
	b.WriteString(t.Text[previous:])
	return b.String(), nil
}

// A configuration check that measures a fixed label value measures a value
// with placeholders as a probe without parameters fills it: each
// placeholder is replaced by its default, or by nothing where it has none.
// What a probe will give is not known when the configuration loads, but
// what the configuration itself makes of the value is. A value that is too
// long from its own text and defaults fails every probe that leaves the
// parameters out.

// CollectorLabelDefaults is the transform.labels value of name, written as
// text, measured so, and whether it is one of the collector's values that
// hold a placeholder: text itself, and false, when it is not.
func CollectorLabelDefaults(c *model.Collector, name, text string) (least string, templated bool) {
	if c.LabelParams != nil {
		for i := range c.LabelParams.Collector {
			if t := &c.LabelParams.Collector[i]; t.Name == name && t.Text == text {
				return templateDefaults(t), true
			}
		}
	}
	return text, false
}

// RuleLabelDefaults is CollectorLabelDefaults for the value of the label at
// index label of the metric rule at index rule.
func RuleLabelDefaults(c *model.Collector, rule, label int, text string) (least string, templated bool) {
	if c.LabelParams != nil {
		for i := range c.LabelParams.Rules {
			if t := &c.LabelParams.Rules[i]; t.Rule == rule && t.Label == label && t.Text == text {
				return templateDefaults(t), true
			}
		}
	}
	return text, false
}

// templateDefaults is a templated value with each placeholder replaced by
// its default.
func templateDefaults(t *model.LabelTemplate) string {
	var b strings.Builder
	previous := 0
	for _, p := range t.Placeholders {
		b.WriteString(t.Text[previous:p.Start])
		b.WriteString(p.Default)
		previous = p.End
	}
	b.WriteString(t.Text[previous:])
	return b.String()
}

// errLabelsChanged is what FilledLabels fails with for a collector whose
// labels are no longer those its placeholders were read from.
var errLabelsChanged = errors.New("the collector's label values are not those its {{param_...}} placeholders were read from when the configuration loaded")

// FilledLabels returns c as a transform reads it for a probe with params.
// If c's label values hold no placeholder, it returns c itself. Otherwise
// it returns a copy whose transform.labels and metric rules' labels have
// their placeholders filled in. Everything that reads a label value then
// finds what it would find if the filled text were written in the
// configuration. c is shared by the probes of a collector and is not
// written to: the copy has its own transform.labels, and its own rules
// where one of them has a filled label.
//
// A value filled to nothing means the label is left out, as with an empty
// label value everywhere. A transform.labels value of "" sets nothing, so
// a series keeps its own label of that name. A rule's label is removed
// from the copy's rule, which then gives its series no such label.
//
// The probe's parameters were checked before the target was contacted
// (CheckPathParams), and a static target's when its file loaded. So an
// error here comes from a caller that checked neither.
func FilledLabels(c *model.Collector, params map[string]string) (*model.Collector, error) {
	labels := c.LabelParams
	if labels == nil {
		return c, nil
	}
	filled := *c
	// The copy's labels hold values, and no placeholder.
	filled.LabelParams = nil
	if len(labels.Collector) > 0 {
		filled.Transform.Labels = maps.Clone(c.Transform.Labels)
		for i := range labels.Collector {
			t := &labels.Collector[i]
			if written, ok := c.Transform.Labels[t.Name]; !ok || written != t.Text {
				return nil, errLabelsChanged
			}
			value, err := labelValue(t, params)
			if err != nil {
				return nil, err
			}
			filled.Transform.Labels[t.Name] = value
		}
	}
	if len(labels.Rules) == 0 {
		return &filled, nil
	}
	filled.Metrics = slices.Clone(c.Metrics)
	// The templates are in the order of the rules and of each rule's labels,
	// so those of one rule follow each other.
	for next := 0; next < len(labels.Rules); {
		rule := labels.Rules[next].Rule
		if rule < 0 || rule >= len(c.Metrics) {
			return nil, errLabelsChanged
		}
		written := c.Metrics[rule].Labels
		own := make([]model.LabelRule, 0, len(written))
		for at, label := range written {
			if next < len(labels.Rules) && labels.Rules[next].Rule == rule && labels.Rules[next].Label == at {
				t := &labels.Rules[next]
				next++
				if label.Value != t.Text {
					return nil, errLabelsChanged
				}
				value, err := labelValue(t, params)
				if err != nil {
					return nil, err
				}
				if value == "" {
					continue
				}
				label.Value = value
			}
			own = append(own, label)
		}
		// A template of the rule that none of its labels took is of a label
		// the rule no longer has.
		if next < len(labels.Rules) && labels.Rules[next].Rule == rule {
			return nil, errLabelsChanged
		}
		filled.Metrics[rule].Labels = own
	}
	return &filled, nil
}
