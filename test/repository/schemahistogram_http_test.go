//go:build !select_request_types || request_type_http

package repository

import (
	"strconv"
	"strings"
	"testing"
)

// A metric rule's histogram block: the schema and the exporter take it
// alike on a rule of type histogram of a transform that reads values, with
// from one bucket to 100, and refuse it alike on a rule of another type or
// none, on a prometheus or a python rule, without buckets, with none or with
// more than 100. Whether the bounds are in increasing order, each once and
// each finite, and whether a label is named le, are the exporter's alone to
// say, as the description says.
func TestSchemaAndExporterAgreeOnHistogramBlocks(t *testing.T) {
	schema := loadSchema(t)
	rule := func(transform, settings string) string {
		return "collectors:\n  - name: demo\n    request:\n      type: http\n    transform:\n      type: " + transform + "\n    metrics:\n      - name: h\n        expression: .v\n" + settings
	}
	bounds := func(n int) string {
		written := make([]string, n)
		for i := range written {
			written[i] = strconv.Itoa(i + 1)
		}
		return "[" + strings.Join(written, ", ") + "]"
	}
	for name, test := range map[string]struct {
		document string
		accepted bool
	}{
		"a jq histogram":                 {rule("jq", "        type: histogram\n        histogram: {buckets: [0.1, 0.5, 1]}\n"), true},
		"a csv histogram":                {rule("csv", "        type: histogram\n        histogram: {buckets: [1]}\n"), true},
		"100 buckets":                    {rule("jq", "        type: histogram\n        histogram: {buckets: "+bounds(100)+"}\n"), true},
		"101 buckets":                    {rule("jq", "        type: histogram\n        histogram: {buckets: "+bounds(101)+"}\n"), false},
		"no buckets":                     {rule("jq", "        type: histogram\n        histogram: {buckets: []}\n"), false},
		"without buckets":                {rule("jq", "        type: histogram\n        histogram: {}\n"), false},
		"a bucket of text":               {rule("jq", "        type: histogram\n        histogram: {buckets: [one]}\n"), false},
		"a key the block has not":        {rule("jq", "        type: histogram\n        histogram: {buckets: [1], scale: 2}\n"), false},
		"on a gauge":                     {rule("jq", "        type: gauge\n        histogram: {buckets: [1]}\n"), false},
		"without a type":                 {rule("jq", "        histogram: {buckets: [1]}\n"), false},
		"of a prometheus rule":           {rule("prometheus", "        type: histogram\n        histogram: {buckets: [1]}\n"), false},
		"of a python rule":               {strings.Replace(rule("python", "        histogram: {buckets: [1]}\n"), "      type: python\n", "      type: python\n      script: metric('h', 1)\n", 1), false},
		"a prometheus histogram without": {rule("prometheus", "        type: histogram\n"), true},
	} {
		agree(t, schema, name, test.document, test.accepted)
	}
	for name, test := range map[string]struct{ settings, message string }{
		"out of order":    {"histogram: {buckets: [1, 0.5]}", "is not greater than the one before it"},
		"twice":           {"histogram: {buckets: [1, 1]}", "is not greater than the one before it"},
		"+Inf":            {"histogram: {buckets: [1, .inf]}", "which every histogram has as its last bucket"},
		"NaN":             {"histogram: {buckets: [.nan]}", "is NaN, not a finite number"},
		"a label le":      {"histogram: {buckets: [1]}\n        labels:\n          - name: le\n            value: x", "has a label le"},
		"without a block": {"description: Durations.", "or add a histogram block"},
	} {
		loadersAlone(t, schema, name, rule("jq", "        type: histogram\n        "+test.settings+"\n"), test.message)
	}
}
