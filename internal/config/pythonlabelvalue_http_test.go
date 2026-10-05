//go:build !select_request_types || request_type_http

package config

import (
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// Under a python transform the script makes the series and sets their labels
// itself; a rule of the collector makes none, and its label only names a
// label of the script's series to cut with truncate: true. A constant on
// such a label, value: x, loaded and did nothing: the script's series did
// not get site="x", with nothing said. The load refuses it now, naming the
// collector, the metric and the label, and saying where a constant for every
// series goes. A label that names a script's label, with an expression that
// is not read, loads as it did, with truncate or without, and so does one
// whose value is written "", which is the key left out.
func TestAPythonRulesLabelWithAConstantValueIsRefused(t *testing.T) {
	const want = `collector "racks" metric "cpu" label "site" sets value, which a python rule's label does not take: the script sets the labels of its series itself, with metric(..., labels={...}), and a rule's label only names one of them to cut with truncate: true; for a constant on every series of the collector, set transform.labels`
	for _, label := range []string{
		"            value: x\n", "            value: x\n            truncate: true\n", "            value: \" \"\n", "            value: 0\n",
		"            value: x\n            expression: \"\"\n",
	} {
		_, err := Load(testutil.WriteFile(t, "config.yaml", labelCollector("python", label)))
		if err == nil || !strings.HasSuffix(err.Error(), want) {
			t.Errorf("%q: %v, want it refused with %s", label, err, want)
		}
	}
	for _, label := range []string{
		"            expression: site\n            truncate: true\n", "            expression: site\n", "            expression: site\n            value: \"\"\n            truncate: true\n",
	} {
		cfg, err := Load(testutil.WriteFile(t, "config.yaml", labelCollector("python", label)))
		if err != nil {
			t.Errorf("%q: %v", label, err)
			continue
		}
		if site := cfg.Collectors[0].Metrics[0].Labels[0]; site.Static() || site.Expression != "site" {
			t.Errorf("%q: the label loads as %+v", label, site)
		}
	}
	// What was refused of a python rule's label is refused as it was, and a
	// constant under transform.labels is where the message sends it.
	for label, message := range map[string]string{
		"            value: x\n            expression: site\n":       "sets both value and expression",
		"            truncate: true\n":                               "needs a value, for a static label, or an expression",
		"            value: x\n            required: true\n":         "has a static value, so it cannot be required",
		"            expression: site\n            required: true\n": "cannot be required: a python transform's labels come from its script",
	} {
		if _, err := Load(testutil.WriteFile(t, "config.yaml", labelCollector("python", label))); err == nil || !strings.Contains(err.Error(), message) {
			t.Errorf("%q: %v, want %s", label, err, message)
		}
	}
	constant := strings.Replace(labelCollector("python", "            expression: site\n            truncate: true\n"), "      type: python\n", "      type: python\n      labels: {site: x}\n", 1)
	cfg, err := Load(testutil.WriteFile(t, "config.yaml", constant))
	if err != nil || cfg.Collectors[0].Transform.Labels["site"] != "x" {
		t.Fatalf("a constant under transform.labels: %v", err)
	}
	// Every other transform takes a constant on a rule's label, as it did.
	for _, transform := range []string{"jq", "yq", "xpath", "css", "regex", "csv", "prometheus"} {
		if _, err := Load(testutil.WriteFile(t, "config.yaml", labelCollector(transform, "            value: x\n"))); err != nil {
			t.Errorf("%s: %v", transform, err)
		}
	}
}
