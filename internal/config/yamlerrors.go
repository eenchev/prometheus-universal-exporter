package config

import (
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// The YAML decoder describes a problem in Go's terms — "field requst not
// found in type model.Collector", "cannot unmarshal !!seq into
// model.RequestConfig" — which names the exporter's source rather than the
// file being read. yamlError rewrites each of its messages in the file's own
// terms:
//
//	line 3: unknown key "requst" in a collector
//	line 4: request must be a mapping, not a list
//	line 7: expected a whole number, not a string

// yamlPlaces names each type the files decode into by where it appears.
var yamlPlaces = map[string]string{
	"Config":              "the configuration",
	"collectorFile":       "a collector file",
	"StaticTargetFile":    "the static target file",
	"WebConfig":           "web",
	"SelfMetricsConfig":   "web.self_metrics",
	"ExporterBasicAuth":   "web.basic_auth",
	"Collector":           "a collector",
	"RequestConfig":       "request",
	"RetryConfig":         "retry",
	"TargetRetryConfig":   "retry",
	"BasicAuth":           "basic_auth",
	"BasicAuthFile":       "basic_auth_file",
	"TLSConfig":           "tls",
	"ResponseConfig":      "response",
	"CSVConfig":           "response.csv",
	"GraphiteConfig":      "response.graphite",
	"DecoderConfig":       "decoder",
	"ErrorHandling":       "error_handling",
	"OTLPConfig":          "otlp",
	"Limits":              "limits",
	"MetricRule":          "a metric rule",
	"LabelRule":           "a label",
	"TransformConfig":     "transform",
	"CacheConfig":         "cache",
	"StaticTarget":        "a static target",
	"TargetRequestConfig": "a static target's request",
	"TargetOTLPConfig":    "a static target's otlp",
}

// yamlValues describes what a value of each other type must be.
var yamlValues = map[string]string{
	"int":                  "a whole number",
	"int64":                "a whole number",
	"float64":              "a number",
	"bool":                 "true or false",
	"string":               "a single value",
	"[]string":             "a list of values",
	"map[string]string":    "a mapping of names to values",
	"map[string]float64":   "a mapping of values to numbers",
	"[]model.Collector":    "a list of collectors",
	"[]model.MetricRule":   "a list of metric rules",
	"[]model.LabelRule":    "a list of labels",
	"[]model.StaticTarget": "a list of static targets",
	"model.MetricType":     "a metric type: gauge, counter, histogram, summary or untyped",
}

var (
	unknownFieldError = regexp.MustCompile(`^(line \d+): field (\S+) not found in type (\S+)$`)
	wrongKindError    = regexp.MustCompile("^(line \\d+): cannot unmarshal !!(\\w+)(?: `(.*)`)? into (\\S+)$")
)

// Extension keys: a top-level key of the configuration, a collector file or
// the static target file that begins with x- is the file's own, ignored by
// the exporter. It holds what YAML anchors (&name) define for the rest of the
// file to reuse with aliases (*name) and merge keys (<<: *name), such as the
// request and transform several collectors share, without the definition
// itself being read as a setting. Only the top level is open: an x- key
// anywhere else is a misspelt setting like any other unknown key.

// extensionKeyTypes are the documents whose top level takes x- keys.
var extensionKeyTypes = map[string]bool{"Config": true, "collectorFile": true, "StaticTargetFile": true}

// isExtensionKey says whether a top-level key is the file's own.
func isExtensionKey(key string) bool { return strings.HasPrefix(key, "x-") && len(key) > len("x-") }

// withoutExtensionKeys drops from a decoding error the unknown top-level x-
// keys, which the decoder reports but reads the rest of the document past;
// nil when nothing else is wrong.
func withoutExtensionKeys(err error) error {
	var typeErr *yaml.TypeError
	if !errors.As(err, &typeErr) {
		return err
	}
	kept := typeErr.Errors[:0:0]
	for _, message := range typeErr.Errors {
		if m := unknownFieldError.FindStringSubmatch(message); m != nil && isExtensionKey(m[2]) && extensionKeyTypes[shortTypeName(m[3])] {
			continue
		}
		kept = append(kept, message)
	}
	if len(kept) == 0 {
		return nil
	}
	return &yaml.TypeError{Errors: kept}
}

// yamlError rewrites a decoding error in the file's terms. Any other error is
// returned as it is.
func yamlError(err error) error {
	var typeErr *yaml.TypeError
	if !errors.As(err, &typeErr) {
		return err
	}
	out := make([]string, 0, len(typeErr.Errors))
	for _, message := range typeErr.Errors {
		out = append(out, yamlMessage(message))
	}
	return errors.New(strings.Join(out, "; "))
}

func yamlMessage(message string) string {
	if m := unknownFieldError.FindStringSubmatch(message); m != nil {
		out := fmt.Sprintf("%s: unknown key %q in %s", m[1], m[2], yamlPlace(m[3]))
		switch {
		case m[2] == "type" && shortTypeName(m[3]) == "LabelRule":
			out += "; a label has no type: set value for a static label, or expression to read it from the response"
		case m[2] == "format" && shortTypeName(m[3]) == "ResponseConfig":
			out += "; the decoder is chosen by decoder.type"
		}
		return out
	}
	if m := wrongKindError.FindStringSubmatch(message); m != nil {
		found := yamlFound(m[2], m[3])
		goType := strings.TrimPrefix(m[4], "*")
		if place, ok := yamlPlaces[shortTypeName(goType)]; ok && !strings.HasPrefix(goType, "[]") && !strings.HasPrefix(goType, "map[") {
			return fmt.Sprintf("%s: %s must be a mapping, not %s", m[1], place, found)
		}
		want, ok := yamlValues[goType]
		if !ok {
			want = "a " + shortTypeName(goType)
		}
		return fmt.Sprintf("%s: expected %s, not %s", m[1], want, found)
	}
	return message
}

// yamlPlace names a type by where it appears in the files.
func yamlPlace(goType string) string {
	if place, ok := yamlPlaces[shortTypeName(goType)]; ok {
		return place
	}
	return shortTypeName(goType)
}

// shortTypeName drops a Go type's package: model.Collector is Collector.
func shortTypeName(goType string) string {
	return goType[strings.LastIndex(goType, ".")+1:]
}

// yamlFound describes the value the file has, from its YAML tag and text. A
// string is not quoted back: under --config.expand-env it may be a secret an
// environment reference put there, such as a password where a mapping
// belongs, and the error reaches the log and the reload status. The line
// number says where it is.
func yamlFound(tag, value string) string {
	switch tag {
	case "seq":
		return "a list"
	case "map":
		return "a mapping"
	case "str":
		return "a string"
	case "int", "float":
		return "the number " + value
	case "bool":
		return value
	case "null":
		return "an empty value"
	}
	return "!!" + tag
}

// oneDocument refuses a second YAML document after the one dec has read: a
// file that goes on after --- would have everything after it ignored without
// a word. An empty document after a final --- is nothing, and is let be.
func oneDocument(dec *yaml.Decoder) error {
	var next yaml.Node
	err := dec.Decode(&next)
	switch {
	case errors.Is(err, io.EOF):
		return nil
	case err != nil:
		return yamlError(err)
	case len(next.Content) == 0 || next.Content[0].Kind == yaml.ScalarNode && next.Content[0].Tag == "!!null":
		return oneDocument(dec)
	}
	return fmt.Errorf("line %d: a second document starts here, after ---; the file must hold one, since the rest would be ignored", next.Content[0].Line)
}
