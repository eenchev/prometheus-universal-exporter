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

// checkLimits refuses a negative limit, which once silently became the
// default, and a limits.max_output_bytes below the least a Python worker
// answers within (transform.MinPythonOutputBytes). 0, like a limit left out,
// is the default.
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
		{"max_label_name_length", int64(l.MaxLabelNameLength)},
		{"max_help_length", int64(l.MaxHelpLength)},
		{"max_output_bytes", int64(l.MaxOutputBytes)},
		{"max_cache_entries", int64(l.MaxCacheEntries)},
		{"max_expression_memory", int64(l.MaxExpressionMemory)},
	} {
		if limit.value < 0 {
			return fmt.Errorf("collector %q limits.%s is %d, and a limit must not be negative; leave it out, or 0, for the default", x.Name, limit.key, limit.value)
		}
	}
	if l.ScriptTimeout < 0 {
		return fmt.Errorf("collector %q limits.script_timeout is %s, and a timeout must not be negative; leave it out for the default, 100ms", x.Name, time.Duration(l.ScriptTimeout))
	}
	// An output limit that no answer of a transform's script fits used to
	// load, and then failed every scrape of a collector with such a script,
	// as output over the limit or as an interpreter that did not start. The
	// key bounds nothing else, so it is held to its least with a script, with
	// a pre-script and with neither. The schema can express this rule too.
	if least := model.ByteSize(transform.MinPythonOutputBytes); l.MaxOutputBytes > 0 && l.MaxOutputBytes < least {
		return fmt.Errorf("collector %q limits.max_output_bytes is %d, and a Python script that emits no metric answers in %d bytes, so no transform's script could answer within it; set at least %d, or leave it out, or 0, for the default, 1MiB", x.Name, l.MaxOutputBytes, least, least)
	}
	return nil
}

// MinResponseBytes is the least a response limit may be when it is set
// (request.max_response_bytes or limits.max_response_bytes). No answer needs
// less than a byte, so no smaller limit is wrong in itself. But a size with
// no unit is a number of bytes, and a limit under a kibibyte is one whose
// unit was left out, such as max_response_bytes: 10 for 10MiB, which fails
// every scrape with an answer over ten bytes.
const MinResponseBytes = 1 << 10

// MinScriptTimeout is the least limits.script_timeout may be when it is set.
// A script's time runs from the line its worker writes once it has read the
// request to the answer it writes back. The two lines take some tens of
// microseconds to go through the pipes on an idle machine. Under a
// millisecond, the time measures them and the machine's load more than the
// script, and even a script that does nothing times out by chance.
const MinScriptTimeout = time.Millisecond

// checkLeastLimits refuses a response limit under MinResponseBytes and a
// script_timeout under MinScriptTimeout. 0, like the key left out, is the
// default, as for every limit.
func checkLeastLimits(x *model.Collector) error {
	for _, limit := range []struct {
		key   string
		value model.ByteSize
	}{
		{"request.max_response_bytes", x.Request.MaxResponseBytes},
		{"limits.max_response_bytes", x.Limits.MaxResponseBytes},
	} {
		if limit.value > 0 && limit.value < MinResponseBytes {
			return fmt.Errorf("collector %q %s is %d bytes, under the least, 1KiB; a size without a unit is a number of bytes, so write the unit, such as 10MiB, or leave it out, or 0, for the default, 10MiB", x.Name, limit.key, limit.value)
		}
	}
	if t := time.Duration(x.Limits.ScriptTimeout); t > 0 && t < MinScriptTimeout {
		return fmt.Errorf("collector %q limits.script_timeout is %s, under the least, 1ms, in which even a script that does nothing may not answer; leave it out, or 0, for the default, 100ms", x.Name, t)
	}
	return nil
}

// labelPath is a label of a metric rule in a field's path.
var labelPath = regexp.MustCompile(`^labels\[(\d+)\]\.`)

// checkPlaceholdersAreFilled refuses a {{param_...}} placeholder in a
// setting of the collector that a probe's parameters do not fill: a bearer
// token or a value_map holding one would be sent or exported with the
// placeholder as text. Every string the collector holds is checked. The code
// that fills fields says which fields are filled (fetch.TemplatedFields, and
// fetch.TemplatedRuleLabel for the value of a rule's label), so a field that
// starts taking placeholders is allowed here by that alone. Code,
// expressions and free text are not looked into (holdsItsOwnBraces): there
// the same characters belong to the language.
//
// The label values that are filled are those read into x.LabelParams
// (fetch.ParseLabelParams), which validateCollector reads before this: the
// values of transform.labels and of a rule's static label. A label's value
// beside an expression is not one of them, since such a label is not
// static, and it is refused here as before.
func checkPlaceholdersAreFilled(x *model.Collector) error {
	filled := fetch.TemplatedFields(x)
	var errs []error
	refuse := func(where string) {
		errs = append(errs, fmt.Errorf("collector %q %s has a {{param_...}} placeholder, which is not filled in there and would be used as written; a probe's parameters fill placeholders only in the request's path, body, header and query values, a grpc message and metadata values, a graphite collector's targets, a sql collector's database and args, and the label values of transform.labels and of a metric rule's static label", x.Name, where))
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

// holdsItsOwnBraces reports whether the field at path, of a collector or of
// one of its metric rules, is written in a language of its own, where the
// text {{param_ belongs to that language and is no unfilled placeholder. The
// fields are Python, in which an f-string writes a literal brace as two,
// f"{{param_x}}", and a dictionary in a set opens with two; an expression,
// such as a jq string or a regex that matches the text; and a description,
// which may well explain the collector's parameters by naming one. Refusing
// those refused scripts and expressions that run as written. Every other
// field is a setting (a credential, a file, a name, a value_map, a pattern
// of metric names) in which the text can only be a placeholder. It is filled
// where the code that fills placeholders says, and refused everywhere else.
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
		// A request's own keys, those of its type alone, are written
		// beside its common part's.
		if v.Type() == requestConfigType {
			if own := v.FieldByName("Own"); !own.IsNil() {
				walkStrings(own.Elem(), path, visit)
			}
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
// without the graphite decoder (checkGraphiteResponse). If accepted, it would
// be ignored, and the collector would not do what its configuration says. It
// runs after the decoder implied by a transform is filled in. A decoder left
// to each response (auto) may turn out to be the one a setting is for.
func checkSettingsApply(x *model.Collector) error {
	t := x.Transform
	runsPython := t.Type == "python" || strings.TrimSpace(t.PreScript) != ""
	switch {
	case strings.TrimSpace(t.Script) != "" && t.Type != "python":
		return fmt.Errorf("collector %q sets transform.script, which is the script of a python transform, but its transform is %s; set transform.type to python, or, to change the response before the %s rules read it, move the code to transform.pre_script", x.Name, t.Type, t.Type)
	case len(t.Libraries) > 0 && !runsPython:
		return fmt.Errorf("collector %q sets transform.libraries, which are imported for the collector's Python code, but it has neither a python transform nor a pre_script", x.Name)
	case t.State && !runsPython:
		return fmt.Errorf("collector %q sets transform.state, which keeps a dict between the runs of the collector's Python code, but it has neither a python transform nor a pre_script", x.Name)
	case len(t.RequiredLibs) > 0 && !runsPython:
		return fmt.Errorf("collector %q sets transform.required_libs, which are imported for the collector's Python code, but it has neither a python transform nor a pre_script", x.Name)
	}
	csv := x.Response.CSV
	if (csv.Header != nil || csv.Delimiter != "" || csv.TrimSpace || csv.Comment != "") && x.Decoder.Type != "csv" && x.Decoder.Type != "auto" {
		return fmt.Errorf("collector %q sets response.csv, which applies to the csv decoder, but its decoder is %s", x.Name, x.Decoder.Type)
	}
	if documents := x.Response.YAML.Documents; documents != "" {
		if x.Decoder.Type != "yaml" && x.Decoder.Type != "auto" {
			return fmt.Errorf("collector %q sets response.yaml, which applies to the yaml decoder, but its decoder is %s", x.Name, x.Decoder.Type)
		}
		if !slices.Contains(model.YAMLDocuments, documents) {
			return fmt.Errorf("collector %q response.yaml.documents is %q; want one or list, in lower case", x.Name, documents)
		}
	}
	// decoder none converts no body (decode.Decode), so a charset would
	// be ignored.
	if x.Response.Charset != "" && x.Decoder.Type == model.DecoderNone {
		return fmt.Errorf("collector %q sets response.charset, which names the encoding a decoder reads the body in, but its decoder is none, which reads no body", x.Name)
	}
	if len(x.Response.Namespaces) > 0 && t.Type != "xpath" {
		return fmt.Errorf("collector %q sets response.namespaces, which name the prefixes of an xpath transform's expressions, but its transform is %s", x.Name, t.Type)
	}
	if err := checkCSVDelimiter(x); err != nil {
		return err
	}
	return checkCSVComment(x)
}

// checkCSVDelimiter requires response.csv.delimiter to be one character that
// the CSV reader can split fields on: any except a double quote, a line
// break, or something that is not a character at all. Left to the scrape, a
// delimiter of two characters failed every scrape, and a quote read no file.
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

// checkCSVComment requires response.csv.comment to be one character that is
// not the delimiter, a double quote, white space or a control character. A
// comment character that is white space would leave out every line that
// begins with a blank.
func checkCSVComment(x *model.Collector) error {
	comment := x.Response.CSV.Comment
	if comment == "" {
		return nil
	}
	delimiter := x.Response.CSV.Delimiter
	if delimiter == "" {
		delimiter = ","
	}
	r, size := utf8.DecodeRuneInString(comment)
	if size == len(comment) && r != '"' && r != utf8.RuneError && !unicode.IsSpace(r) && !unicode.IsControl(r) && comment != delimiter {
		return nil
	}
	return fmt.Errorf("collector %q response.csv.comment %s must be one character that starts a comment line, such as #, and not the delimiter, a double quote, white space or a control character", x.Name, strconv.Quote(comment))
}

// checkProbeSeriesNames refuses, on a collector with report_failures, a rule
// or a derived family that is not hidden and is exported under the name of a
// series the exporter adds to its probes' answers (model.ProbeSeriesNames),
// after its metrics_prefix and name_escaping. The answer would hold two
// families of one name, or a family of the collector's that says something
// else under the exporter's name. A name that a Python script or a
// prometheus transform makes at the scrape is refused there, in the
// validation stage (exporter/probereport.go).
func checkProbeSeriesNames(x *model.Collector) error {
	if !x.ReportFailures {
		return nil
	}
	names := make([]string, 0, len(x.Metrics)+len(x.Derived))
	for _, rule := range x.Metrics {
		names = append(names, rule.Name)
	}
	for _, rule := range x.Derived {
		// A hidden family is read by later entries, and not exported.
		if !rule.Hidden {
			names = append(names, rule.Name)
		}
	}
	for _, name := range names {
		if name == "" {
			continue
		}
		if exported := transform.ExportedMetricName(x, name); slices.Contains(model.ProbeSeriesNames, exported) {
			return fmt.Errorf("collector %q metric %q is exported as %s, which is reserved for the series report_failures adds to every probe's answer; rename the metric, or leave report_failures out", x.Name, name, exported)
		}
	}
	return nil
}
