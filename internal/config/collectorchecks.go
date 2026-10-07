package config

import (
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/transform"
)

// What a collector sets and nothing would use is refused when the
// configuration loads, rather than ignored: a limit that cannot be one, a
// placeholder where none is filled in, and a setting of a decoder or a
// transform the collector does not have.

// checkLimits refuses a negative limit, which once became the default without
// a word. 0, like a limit left out, is the default.
func checkLimits(x *model.Collector) error {
	l := x.Limits
	for _, limit := range []struct {
		key   string
		value int64
	}{
		{"max_metrics", int64(l.MaxMetrics)},
		{"max_labels_per_metric", int64(l.MaxLabelsPerMetric)},
		{"max_label_value_length", int64(l.MaxLabelValueLength)},
		{"max_metric_name_length", int64(l.MaxMetricNameLength)},
		{"max_help_length", int64(l.MaxHelpLength)},
		{"max_output_bytes", int64(l.MaxOutputBytes)},
		{"max_cache_entries", int64(l.MaxCacheEntries)},
	} {
		if limit.value < 0 {
			return fmt.Errorf("collector %q limits.%s is %d, and a limit must not be negative; leave it out, or 0, for the default", x.Name, limit.key, limit.value)
		}
	}
	if l.ScriptTimeout < 0 {
		return fmt.Errorf("collector %q limits.script_timeout is %s, and a timeout must not be negative; leave it out for the default, 100ms", x.Name, time.Duration(l.ScriptTimeout))
	}
	return nil
}

// labelPath is a label of a metric rule in a field's path.
var labelPath = regexp.MustCompile(`^labels\[(\d+)\]\.`)

// checkPlaceholdersAreFilled refuses a {{param_...}} placeholder in a
// setting of the collector that a probe's parameters do not fill: a bearer
// token or a value_map holding one would be sent or exported with the
// placeholder as text. Every string the collector holds is looked at, and the
// fields that are filled are asked of the code that fills them
// (fetch.TemplatedFields, and fetch.TemplatedRuleLabel for the value of a
// rule's label), so a field that starts taking placeholders is allowed here
// by that alone. What is code, an expression or free text is not looked into
// (holdsItsOwnBraces): there the same characters are the language's own.
//
// The label values that are filled are those read into x.LabelParams
// (fetch.ParseLabelParams), which validateCollector reads before this: the
// values of transform.labels and of a rule's static label. A label's value
// beside an expression is not one of them, since such a label is not
// static, and is refused here as it was.
func checkPlaceholdersAreFilled(x *model.Collector) error {
	filled := fetch.TemplatedFields(x)
	var errs []error
	refuse := func(where string) {
		errs = append(errs, fmt.Errorf("collector %q %s has a {{param_...}} placeholder, which is not filled in there and would be used as written; a probe's parameters fill placeholders only in the request's path, body, header and query values, a grpc message and metadata values, a graphite collector's targets, and the label values of transform.labels and of a metric rule's static label", x.Name, where))
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
	// A rule is named as its other errors name it, by its metric name or,
	// having none, by its place (transform.RuleName), a label by its name.
	for i := range x.Metrics {
		rule := &x.Metrics[i]
		walkStrings(reflect.ValueOf(rule).Elem(), "", func(path, text string) {
			if !paramPlaceholder.MatchString(text) || holdsItsOwnBraces(path) {
				return
			}
			if m := labelPath.FindStringSubmatch(path); m != nil {
				var index int
				_, _ = fmt.Sscanf(m[1], "%d", &index)
				if path[len(m[0]):] == "value" && fetch.TemplatedRuleLabel(x, i, index) {
					return
				}
				path = fmt.Sprintf("label %q %s", rule.Labels[index].Name, path[len(m[0]):])
			}
			refuse(fmt.Sprintf("%s %s", transform.RuleName(rule, i), path))
		})
	}
	return model.JoinProblems(errs...)
}

// holdsItsOwnBraces says the field at path, of a collector or of one of its
// metric rules, is written in a language of its own, where the text {{param_
// is that language's and no placeholder left unfilled: Python, in which an
// f-string writes a literal brace as two, f"{{param_x}}", and a dictionary
// in a set opens with two; an expression, such as a jq string or a regex
// that matches the text; and a description, which may well explain the
// collector's parameters by naming one. Refusing those refused scripts and
// expressions that run as they are written. Every other field is a setting —
// a credential, a file, a name, a value_map, a pattern of metric names — in
// which the text can only be a placeholder, which is filled where the code
// that fills placeholders says it is and refused everywhere else.
func holdsItsOwnBraces(path string) bool {
	switch labelPath.ReplaceAllString(path, "labels[].") {
	case "transform.script", "transform.pre_script", // Python
		"items", "expression", "labels[].expression", // the transform's language
		"description": // free text
		return true
	}
	return false
}

// walkStrings calls visit for every string v holds, with its path as the
// configuration writes it: request.bearer_token, request.headers.Accept for
// a mapping's value, request.targets[0] for a list's. A mapping's keys are
// visited under the mapping's own path.
func walkStrings(v reflect.Value, path string, visit func(path, text string)) {
	switch v.Kind() {
	case reflect.Pointer:
		if !v.IsNil() {
			walkStrings(v.Elem(), path, visit)
		}
	case reflect.String:
		visit(path, v.String())
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			key, _, _ := strings.Cut(v.Type().Field(i).Tag.Get("yaml"), ",")
			if key == "" || key == "-" {
				continue
			}
			walkStrings(v.Field(i), joinSchemaPath(path, key), visit)
		}
	case reflect.Slice:
		for i := 0; i < v.Len(); i++ {
			walkStrings(v.Index(i), fmt.Sprintf("%s[%d]", path, i), visit)
		}
	case reflect.Map:
		if v.Type().Key().Kind() != reflect.String {
			return
		}
		keys := make([]string, 0, v.Len())
		for _, key := range v.MapKeys() {
			keys = append(keys, key.String())
		}
		slices.Sort(keys)
		for _, key := range keys {
			visit(path, key)
			walkStrings(v.MapIndex(reflect.ValueOf(key).Convert(v.Type().Key())), path+"."+key, visit)
		}
	}
}

// checkSettingsApply refuses a setting that belongs to a decoder or a
// transform the collector does not have, as response.graphite is refused
// without the graphite decoder (checkGraphiteResponse): accepted, it would be
// ignored, and the collector would not do what its configuration says. It
// runs after the decoder a transform implies is filled in; a decoder left to
// each response (auto) may turn out to be the one a setting is for.
func checkSettingsApply(x *model.Collector) error {
	t := x.Transform
	runsPython := t.Type == "python" || strings.TrimSpace(t.PreScript) != ""
	switch {
	case strings.TrimSpace(t.Script) != "" && t.Type != "python":
		return fmt.Errorf("collector %q sets transform.script, which is the script of a python transform, but its transform is %s; set transform.type to python, or, to change the response before the %s rules read it, move the code to transform.pre_script", x.Name, t.Type, t.Type)
	case len(t.Libraries) > 0 && !runsPython:
		return fmt.Errorf("collector %q sets transform.libraries, which are imported for the collector's Python code, but it has neither a python transform nor a pre_script", x.Name)
	case len(t.RequiredLibs) > 0 && !runsPython:
		return fmt.Errorf("collector %q sets transform.required_libs, which are imported for the collector's Python code, but it has neither a python transform nor a pre_script", x.Name)
	}
	csv := x.Response.CSV
	if (csv.Header != nil || csv.Delimiter != "" || csv.TrimSpace) && x.Decoder.Type != "csv" && x.Decoder.Type != "auto" {
		return fmt.Errorf("collector %q sets response.csv, which applies to the csv decoder, but its decoder is %s", x.Name, x.Decoder.Type)
	}
	if len(x.Response.Namespaces) > 0 && t.Type != "xpath" {
		return fmt.Errorf("collector %q sets response.namespaces, which name the prefixes of an xpath transform's expressions, but its transform is %s", x.Name, t.Type)
	}
	return checkCSVDelimiter(x)
}

// checkCSVDelimiter requires response.csv.delimiter to be the one character
// the CSV reader can split fields on: any but a double quote, a line break
// and what is not a character at all. Left to the scrape, a delimiter of two
// characters failed every one, and a quote read no file.
func checkCSVDelimiter(x *model.Collector) error {
	delimiter := x.Response.CSV.Delimiter
	if delimiter == "" {
		return nil
	}
	r, size := utf8.DecodeRuneInString(delimiter)
	if size == len(delimiter) && r != 0 && r != '"' && r != '\r' && r != '\n' && r != utf8.RuneError {
		return nil
	}
	// Shown as the file writes it where that can be read: %q would show
	// the two characters of '\t' as "\\t".
	written := "'" + delimiter + "'"
	if strings.ContainsFunc(delimiter, func(r rune) bool { return !unicode.IsPrint(r) }) {
		written = strconv.Quote(delimiter)
	}
	return fmt.Errorf(`collector %q response.csv.delimiter %s must be one character, and not a double quote or a line break; for a tab, write delimiter: "\t" in double quotes, where YAML reads \t as the tab character`, x.Name, written)
}
