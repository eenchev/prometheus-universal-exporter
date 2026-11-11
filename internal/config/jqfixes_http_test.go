//go:build !select_request_types || request_type_http

package config

import (
	"slices"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// Two labels of one name in a rule are refused when the configuration
// loads, naming the rule and the label: a series has one label of a name,
// and the later of the two had it while the earlier was lost unsaid. The
// same name in two rules, and two names in one, load as they did.
func TestTwoLabelsOfOneNameInARuleAreRefused(t *testing.T) {
	const want = `collector "a" metric "m" has two labels named "env"; a series has one label of a name, so remove one of them or give it another name`
	for name, labels := range map[string]string{
		"two expressions":         "          - {name: env, expression: .a}\n          - {name: env, expression: .b}\n",
		"a value, an expression":  "          - {name: env, value: prod}\n          - {name: env, expression: .b}\n",
		"two values":              "          - {name: env, value: prod}\n          - {name: env, value: prod}\n",
		"with another in between": "          - {name: env, value: prod}\n          - {name: zone, value: a}\n          - {name: env, expression: .b}\n",
	} {
		if _, err := loadChecked(t, "", "", "        labels:\n"+labels); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", name, err, want)
		}
	}
	other := "        labels:\n          - {name: env, value: prod}\n          - {name: Env, expression: .b}\n" +
		"      - name: n\n        expression: .y\n        labels:\n          - {name: env, expression: .a}\n"
	if _, err := loadChecked(t, "", "", other); err != nil {
		t.Errorf("the same name in two rules, and two names in one: %v", err)
	}
	for transform, document := range map[string]string{
		"regex": "collectors:\n  - name: a\n    request: {type: http}\n    decoder: {type: text}\n    transform: {type: regex}\n    metrics:\n      - name: m\n        expression: 'v=(\\d+) e=(\\w+)'\n        labels:\n          - {name: env, expression: '2'}\n          - {name: env, value: x}\n",
		"csv":   "collectors:\n  - name: a\n    request: {type: http}\n    decoder: {type: csv}\n    transform: {type: csv}\n    metrics:\n      - name: m\n        expression: v\n        labels:\n          - {name: env, expression: e}\n          - {name: env, expression: f}\n",
	} {
		if _, err := Load(testutil.WriteFile(t, "config.yaml", document)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", transform, err, want)
		}
	}
}

// limits.max_expression_memory is 256MiB when it is left out or 0, a size
// as written otherwise, and at least 1MiB: the limit is held against the
// memory of the whole exporter. A defaults block's is held to the same.
func TestTheExpressionMemoryLimitHasADefaultAndALeast(t *testing.T) {
	for written, want := range map[string]int64{"": 256 << 20, "max_expression_memory: 0": 256 << 20, "max_expression_memory: 64MiB": 64 << 20, "max_expression_memory: 1048576": 1 << 20} {
		cfg, err := loadChecked(t, "", "    limits: {"+written+"}\n", "")
		if err != nil || int64(cfg.Collectors[0].Limits.MaxExpressionMemory) != want {
			t.Errorf("%q: %v, want %d bytes", written, err, want)
		}
	}
	const tooSmall = `limits.max_expression_memory is 1048575 bytes, and it is at least 1MiB, since it is held against the memory of the whole exporter; leave it out, or 0, for the default, 256MiB`
	if _, err := loadChecked(t, "", "    limits: {max_expression_memory: 1048575}\n", ""); err == nil || !strings.Contains(err.Error(), `collector "a" `+tooSmall) {
		t.Errorf("under the least: %v", err)
	}
	if _, err := Load(testutil.WriteFile(t, "config.yaml", "defaults: {limits: {max_expression_memory: 1048575}}\n"+checkedCollector("", "", ""))); err == nil || !strings.Contains(err.Error(), "defaults."+tooSmall) {
		t.Errorf("under the least in defaults: %v", err)
	}
}

// labels_from is a key of a jq or yq rule, with items or without, and its
// expression is compiled when the configuration loads; two rules that
// differ in it alone are two rules.
func TestLabelsFromIsCheckedWhenTheConfigurationLoads(t *testing.T) {
	if _, err := loadChecked(t, "", "", "        items: .list[]\n        labels_from: .labels\n      - name: m\n        expression: .x\n        items: .list[]\n        labels_from: .annotations\n      - name: m\n        expression: .x\n        labels_from: .labels\n"); err != nil {
		t.Errorf("rules with labels_from, with items and without: %v", err)
	}
	for name, tc := range map[string]struct{ metric, want string }{
		"an expression that is none":  {"        items: .list[]\n        labels_from: '.labels |'\n", `collector "a" metric "m" labels_from ".labels |": `},
		"a function there is none of": {"        items: .list[]\n        labels_from: nosuch\n", `collector "a" metric "m" labels_from "nosuch": function not defined: nosuch/0`},
	} {
		if _, err := loadChecked(t, "", "", tc.metric); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
	}
	regex := "collectors:\n  - name: a\n    request: {type: http}\n    decoder: {type: text}\n    transform: {type: regex}\n    metrics:\n      - name: m\n        expression: 'v=(\\d+)'\n        labels_from: .labels\n"
	if _, err := Load(testutil.WriteFile(t, "config.yaml", regex)); err == nil || !strings.Contains(err.Error(), `collector "a" metric "m" sets labels_from, which only the jq and yq transforms support`) {
		t.Errorf("under regex: %v", err)
	}
}

// response.yaml.documents is one or list, and a setting of the yaml
// decoder: a collector whose decoder is another refuses it.
func TestTheYAMLDocumentsSettingIsChecked(t *testing.T) {
	collector := func(decoder, documents string) string {
		return "collectors:\n  - name: a\n    request: {type: http}\n" + decoder + "    transform: {type: yq}\n    response: {yaml: {documents: " + documents + "}}\n    metrics:\n      - name: m\n        expression: .x\n"
	}
	for _, decoder := range []string{"    decoder: {type: yaml}\n", ""} {
		for _, documents := range []string{"list", "one"} {
			cfg, err := Load(testutil.WriteFile(t, "config.yaml", collector(decoder, documents)))
			if err != nil || cfg.Collectors[0].Response.YAML.Documents != documents {
				t.Errorf("decoder %q, documents %s: %v", decoder, documents, err)
			}
		}
	}
	for name, tc := range map[string]struct{ document, want string }{
		"another value":   {collector("    decoder: {type: yaml}\n", "all"), `collector "a" response.yaml.documents is "all"; want one or list, in lower case`},
		"another case":    {collector("    decoder: {type: yaml}\n", "List"), `collector "a" response.yaml.documents is "List"; want one or list, in lower case`},
		"another decoder": {collector("    decoder: {type: json}\n", "list"), `collector "a" sets response.yaml, which applies to the yaml decoder, but its decoder is json`},
	} {
		if _, err := Load(testutil.WriteFile(t, "config.yaml", tc.document)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
	}
}

// What a collector's expressions read of $params is found when the
// configuration loads: the names read as a field, and whether it is read
// otherwise. A collector that reads nothing of it, and one of another
// transform, has none.
func TestWhatTheExpressionsReadOfParamsIsFoundAtLoad(t *testing.T) {
	for name, tc := range map[string]struct {
		metric string
		names  []string
		any    bool
		none   bool
	}{
		"nothing":            {"", nil, false, true},
		"another variable":   {"        labels:\n          - {name: l, expression: '. as $params_of_mine | $params_of_mine.a'}\n", nil, false, true},
		"fields":             {"        items: '.list[] | select(.q == $params.queue)'\n        labels_from: '{z: $params [\"zone a\"]}'\n        labels:\n          - {name: l, expression: '$params.\"tenant\" // $params.queue'}\n", []string{"queue", "tenant", "zone a"}, false, false},
		"as a whole":         {"        labels:\n          - {name: l, expression: '$params | keys | join(\",\")'}\n", nil, true, false},
		"by a name it makes": {"        labels:\n          - {name: l, expression: '$params[.key]'}\n          - {name: k, expression: $params.a}\n", []string{"a"}, true, false},
	} {
		cfg, err := loadChecked(t, "", "", tc.metric)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		read := cfg.Collectors[0].ExpressionParams
		if tc.none != (read == nil) || read != nil && (!slices.Equal(read.Names, tc.names) || read.Any != tc.any) {
			t.Errorf("%s: %+v, want the names %v and any %v", name, read, tc.names, tc.any)
		}
	}
}
