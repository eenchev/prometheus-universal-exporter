//go:build !select_request_types || request_type_http

package config

import (
	"fmt"
	"strings"
	"testing"
)

// A message of the load about a rule names the rule by its metric name. A
// rule that has none — name left out, "" or nothing but blanks — was told
// of as `metric ""`, and, where the name was what it lacked, as "a metric
// without a name" of the collector: in a collector of thirty rules neither
// says which. Each such message names the rule by its place among the
// collector's rules now, counted from 1, as the message of a prometheus
// rule with neither a name nor an expression does. Every rule here is
// loaded as the first rule of its collector and as the third, after two
// that are sound, and is told of as rule 1 and as rule 3: under jq, yq,
// xpath, css, regex and csv, where the name is what the rule lacks; under
// prometheus, where a rule may have no name and everything else it gets
// wrong is told of it; and under python, where the name is all a rule is
// for. A rule that has a name reads as it did, by that name, whatever is
// wrong with the name.
func TestARuleWithoutANameIsToldOfByItsPlace(t *testing.T) {
	sound := map[string]string{"jq": ".v", "yq": ".v", "xpath": "//v", "css": "td.v", "regex": `'v=(\d+)'`, "csv": "v", "prometheus": "'^v$'", "python": ".v"}
	const headless, small = "    response:\n      csv:\n        header: false\n", "    limits:\n      max_help_length: 10\n      max_label_value_length: 5\n"
	const pythonNeeds = ` has no name, which a python rule needs: the rule makes no series and only names one of the script's, whose labels it cuts with truncate: true and which a debug probe's report lists when the script made none; write the name the script gives the series, as in metric("up", ...), or take the rule out`
	for _, tc := range []struct {
		transform, collector, rule string
		// want is what the load says after the collector and the rule's
		// place, each from its start.
		want []string
	}{
		{"jq", "", "expression: .v\n", []string{" has no name"}},
		{"jq", "", "{}\n", []string{" has no name"}},
		{"jq", "", "name: \"\"\n        expression: .v\n", []string{" has no name"}},
		{"jq", "", "name: \"  \"\n        expression: .v\n", []string{" has no name"}},
		{"yq", "", "expression: .v\n", []string{" has no name"}},
		{"xpath", "", "expression: //v\n", []string{" has no name"}},
		{"css", "", "expression: td.v\n", []string{" has no name"}},
		{"regex", "", "expression: 'v=(\\d+)'\n", []string{" has no name"}},
		{"csv", "", "expression: v\n", []string{" has no name"}},
		{"csv", headless, "expression: host\n        labels:\n          - name: rack\n            expression: rack\n", []string{" has no name", ` reads column "host", but response.csv.header is false, so columns are named by number, from 1; write the column's number, such as "2"`, ` label "rack" reads column "rack", but response.csv.header is false`}},
		{"jq", "", "expression: .v\n        error_mode: panic\n", []string{` error_mode has invalid value "panic"; want fail, log or ignore`}},
		{"jq", "", "expression: .v\n        type: timer\n", []string{` has invalid type "timer"`}},
		{"jq", "", "expression: .v\n        type: histogram\n", []string{" has type histogram, which only a prometheus transform can give, passing through a histogram that has its buckets or quantiles; a jq rule reads one value, so use gauge, counter or untyped"}},

		{"prometheus", "", "expression: '^node_'\n        type: timer\n", []string{` has invalid type "timer"`}},
		{"prometheus", "", "expression: '^node_'\n        error_mode: panic\n", []string{` error_mode has invalid value "panic"; want fail, log or ignore`}},
		{"prometheus", "", "expression: '^node_'\n        labels:\n          - value: x\n", []string{" has a label without a name"}},
		{"prometheus", "", "expression: '^node_'\n        labels:\n          - name: bad-name\n            value: x\n", []string{` has invalid label name "bad-name"`}},
		{"prometheus", "", "expression: '^node_'\n        labels:\n          - name: __site\n            value: x\n", []string{`: label name "__site"`}},
		{"prometheus", "", "expression: '^node_'\n        labels:\n          - name: site\n            expression: \"  \"\n", []string{` label "site" expression "  " is nothing but blanks; write the expression that reads the label from the response, or leave expression out and set value for a constant`}},
		{"prometheus", "", "expression: '^node_'\n        labels:\n          - name: site\n            value: x\n            expression: instance\n", []string{` label "site" sets both value and expression; set value for a static label, or expression to read it from the response`}},
		{"prometheus", "", "expression: '^node_'\n        labels:\n          - name: site\n", []string{` label "site" needs a value, for a static label, or an expression, to read it from the response`}},
		{"prometheus", "", "expression: '^node_'\n        labels:\n          - name: site\n            value: x\n            required: true\n", []string{` label "site" has a static value, so it cannot be required; its value is always there`}},
		{"prometheus", "", "expression: '^node_'\n        labels:\n          - name: site\n            expression: instance\n            value_map: {a: b}\n", []string{` label "site" sets value_map on a rule without a name; name the rule, so its series are known`}},
		{"prometheus", "", "expression: '^node_'\n        items: .rows[]\n", []string{" sets items, which only the jq, yq and css transforms support"}},
		{"prometheus", "", "expression: '^node_'\n        scale: 0\n", []string{" scale must be a finite number other than 0, got 0"}},
		{"prometheus", "", "expression: '^node_'\n        value_map: {up: 1}\n", []string{" sets value_map, which the prometheus transform does not use: its values are numbers already; scale applies"}},
		{"prometheus", "", "expression: '^node_'\n        time_format: rfc3339\n", []string{" sets time_format, which the prometheus transform does not use: its values are numbers already; scale applies"}},
		{"prometheus", "", "expression: '^node_'\n        time_zone: UTC\n", []string{" sets time_zone without time_format; time_zone is the zone a time_format reads text in that names no zone of its own"}},
		{"prometheus", "", "expression: '('\n", []string{` expression "(": `}},
		{"prometheus", "", "expression: \"  \"\n", []string{` expression "  " is nothing but blanks; a prometheus rule's expression is a regular expression matched against a metric's name`}},
		{"prometheus", "", "name: \"  \"\n        expression: '^node_'\n", []string{`: "  " is not a valid Prometheus metric name; use letters, digits, underscores and colons, not starting with a digit`}},
		{"prometheus", "", "expression: '^node_'\n        items: .rows[]\n        scale: 0\n        labels:\n          - name: site\n            expression: instance\n            value_map: {a: b}\n", []string{" sets items, which", " scale must be", ` label "site" sets value_map on a rule without a name`}},
		{"prometheus", small, "expression: '^node_'\n        description: A description of thirty-two bytes.\n", []string{" description is 34 bytes, longer than limits.max_help_length 10, so every series would fail validation; shorten it or raise the limit"}},
		{"prometheus", small, "expression: '^node_'\n        labels:\n          - name: site\n            value: rack-one\n", []string{` label "site" value is 8 bytes, longer than limits.max_label_value_length 5, so every series would fail validation; shorten it, set truncate: true on the label, or raise the limit`}},
		{"prometheus", "", "expression: '^node_'\n        labels:\n          - name: site\n            value: \"{{param_site}}\"\n", []string{` label "site" value has a {{param_...}} placeholder, which is not filled in there and would be used as written`}},

		{"python", "", "{}\n", []string{pythonNeeds}},
		{"python", "", "expression: up\n", []string{pythonNeeds}},
		{"python", "", "type: counter\n        description: Up.\n        required: true\n        error_mode: fail\n        labels:\n          - name: site\n            expression: site\n", []string{pythonNeeds, " sets type, which a python rule does not take", " sets description, which a python rule does not take", " sets required, which a python rule does not take", " sets error_mode, which a python rule does not take", ` label "site" does not set truncate: true, which is all a python rule's label does`}},
		{"python", "", "labels:\n          - name: site\n            value: x\n", []string{` label "site" sets value, which a python rule's label does not take`}},
		{"python", "", "labels:\n          - name: site\n            expression: site\n            required: true\n", []string{` label "site" cannot be required: a python transform's labels come from its script, not from label expressions`}},
		{"python", "", "items: .rows[]\n        scale: 2\n", []string{" sets items, which only the jq, yq and css transforms support", " sets value_map or scale, which the python transform does not use: its script sets each value with metric(...)"}},
		{"python", "", "time_format: rfc3339\n", []string{" sets time_format, which the python transform does not use: its script sets each value with metric(...)"}},
		{"python", "", "name: \"  \"\n", []string{`: "  " is not a valid Prometheus metric name; use letters, digits, underscores and colons, not starting with a digit`}},
	} {
		first := "      - name: w\n        expression: " + sound[tc.transform] + "\n      - name: x\n        expression: " + sound[tc.transform] + "\n"
		if tc.collector == "    response:\n      csv:\n        header: false\n" {
			first = "      - name: w\n        expression: \"1\"\n      - name: x\n        expression: \"2\"\n"
		}
		for place, before := range map[int]string{1: "", 3: first} {
			got := problemsOf(t, collectorRules(tc.transform, tc.collector, before+"      - "+tc.rule))
			where := fmt.Sprintf(`collector "node" metrics rule %d`, place)
			ok := len(got) == len(tc.want)
			for i := 0; ok && i < len(got); i++ {
				ok = strings.HasPrefix(got[i], where+tc.want[i])
			}
			if !ok || strings.Contains(strings.Join(got, "\n"), `metric ""`) || strings.Contains(strings.Join(got, "\n"), "a metric without a name") {
				t.Errorf("a %s rule %q, rule %d: the load says\n%s\nwant each of these after %s:\n%s", tc.transform, tc.rule, place, strings.Join(got, "\n"), where, strings.Join(tc.want, "\n"))
			}
		}
	}

	// A rule that has a name is told of by it, as it was, wherever it
	// stands and whatever is wrong with the name.
	for _, tc := range []struct{ transform, rules, want string }{
		{"jq", "      - name: up\n", `collector "node" metric "up" has no expression`},
		{"jq", "      - name: w\n        expression: .w\n      - name: up\n        expression: .v\n        type: timer\n", `collector "node" metric "up" has invalid type "timer"`},
		{"jq", "      - name: up\n        expression: .v\n        error_mode: panic\n", `collector "node" metric "up" error_mode has invalid value "panic"; want fail, log or ignore`},
		{"jq", "      - name: bad-name\n        expression: .v\n", `collector "node" metric "bad-name": "bad-name" is not a valid Prometheus metric name; use letters, digits, underscores and colons, not starting with a digit, or set the collector's name_escaping to underscores or values to export it escaped`},
		{"jq", "      - name: \" up\"\n        expression: .v\n", `collector "node" metric " up": " up" is not a valid Prometheus metric name; use letters, digits, underscores and colons, not starting with a digit, or set the collector's name_escaping to underscores or values to export it escaped`},
		{"prometheus", "      - name: up\n        items: .rows[]\n", `collector "node" metric "up" sets items, which only the jq, yq and css transforms support`},
		{"prometheus", "      - name: up\n        labels:\n          - name: site\n", `collector "node" metric "up" label "site" needs a value, for a static label, or an expression, to read it from the response`},
		{"python", "      - name: up\n        type: counter\n", `collector "node" metric "up" sets type, which a python rule does not take: the script gives each of its series its type, with metric(..., type="counter"), and a series is a gauge when it gives none; say the type in the script and leave type out of the rule`},
	} {
		if got := problemsOf(t, collectorRules(tc.transform, "", tc.rules)); len(got) != 1 || got[0] != tc.want {
			t.Errorf("%s rules %q: the load says %q\nwant %s", tc.transform, tc.rules, got, tc.want)
		}
	}
}
