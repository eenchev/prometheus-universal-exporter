package transform

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// Text written in the two forms of a number that are Go's alone, digits
// separated by underscores and hexadecimal floating-point, is no number to a
// rule of any transform that reads text: a regex's capture, the text of a CSS
// element, of an XPath node, an attribute and a computed string, a CSV cell,
// and a jq or yq string. Each was read as the number Go makes of it, 1_000 as
// 1000 and 0x1p-2 as 0.25; each now fails its rule in the words every text
// that is no number gets, which name value_map. A value_map that lists the
// text maps it, and without it in the map the text is neither in the map nor
// a number; a label keeps such a text as it is written; and the numbers
// beside it are read as they were.
func TestGoNumberSyntaxIsNoNumberInAnyTransform(t *testing.T) {
	bodies := map[string]struct {
		decoder, transform, contentType, body, items, expression, label string
	}{
		"regex":           {"text", "regex", "text/plain", "id=%s load=%s\n", "", `load=(\S+)`, ""},
		"css":             {"html", "css", "text/html", `<div><p class="id">%s</p><p class="v"> %s </p></div>`, "div", "p.v", "p.id"},
		"xpath":           {"xml", "xpath", "application/xml", `<r><id>%s</id><v>%s</v></r>`, "", "//v", "../id"},
		"xpath attribute": {"xml", "xpath", "application/xml", `<r id="%s" v="%s"/>`, "", "//r/@v", "../@id"},
		"xpath string":    {"xml", "xpath", "application/xml", `<r><id>%s</id><v>%s</v></r>`, "", "string(//v)", ""},
		"csv":             {"csv", "csv", "text/csv", "id,v\n%s,%s\n", "", "v", "id"},
		"jq":              {"json", "jq", "application/json", `{"id": "%s", "v": "%s"}`, "", ".v", ".id"},
		"yq":              {"yaml", "yq", "application/yaml", "id: \"%s\"\nv: \"%s\"\n", "", ".v", ".id"},
	}
	for name, tc := range bodies {
		t.Run(name, func(t *testing.T) {
			rule := model.MetricRule{Name: "v", Type: model.GaugeMetricType, ErrorMode: model.ErrorModeFail, Items: tc.items, Expression: tc.expression}
			if tc.label != "" {
				rule.Labels = []model.LabelRule{{Name: "id", Expression: tc.label}}
			}
			run := func(rule model.MetricRule, text string) (*model.MetricSet, error) {
				c := model.Collector{Name: "v", Decoder: model.DecoderConfig{Type: tc.decoder}, Transform: model.TransformConfig{Type: tc.transform}, Metrics: []model.MetricRule{rule}}
				if err := CheckMetricRule(&c, &c.Metrics[0]); err != nil {
					t.Fatal(err)
				}
				return runBody(t, c, tc.contentType, fmt.Sprintf(tc.body, "1_000", text))
			}
			for _, text := range []string{"1_000", "1_0.5", "0x1p-2", "0X1P+4", "-0x1.8p1"} {
				set, err := run(rule, text)
				want := fmt.Sprintf("value %q is not a number; map text to numbers with value_map", text)
				if err == nil || !strings.HasSuffix(err.Error(), want) {
					t.Errorf("%s: read as %v, %v; want the rule to fail with %s", text, set, err, want)
				}
				mapped := rule
				mapped.ValueMap = map[string]float64{text: 7}
				if set, err := run(mapped, text); err != nil || len(set.Metrics) != 1 || set.Metrics[0].Value != 7 {
					t.Errorf("%s in value_map: %v, %v; want 7", text, set, err)
				}
				mapped.ValueMap = map[string]float64{"up": 1}
				want = fmt.Sprintf(`value %q is neither in value_map nor a number; add it to value_map, or map "*" for any other value`, text)
				if set, err := run(mapped, text); err == nil || !strings.HasSuffix(err.Error(), want) {
					t.Errorf("%s beside a value_map: %v, %v; want %s", text, set, err, want)
				}
			}
			for text, want := range map[string]float64{"1000": 1000, "0.25": 0.25, "+1e3": 1000, ".5": 0.5} {
				set, err := run(rule, text)
				if err != nil || len(set.Metrics) != 1 || set.Metrics[0].Value != want {
					t.Fatalf("%s: %v, %v; want %v", text, set, err, want)
				}
				if tc.label != "" && set.Metrics[0].Labels["id"] != "1_000" {
					t.Errorf("the label read from 1_000 is %q", set.Metrics[0].Labels["id"])
				}
			}
		})
	}
}

// What a Python script gives metric(...) is the script's own: Python reads
// "1_000" as a number, and the exporter takes the number the script made of
// it. A row a pre-script leaves for the rules is read by the rules, as text
// is from a response: 1_000 as text there is no number, and the number 1000
// the script made of it is.
func TestGoNumberSyntaxAfterAScript(t *testing.T) {
	requirePython(t)
	c := model.Collector{Name: "v", Decoder: model.DecoderConfig{Type: "csv"}, Transform: model.TransformConfig{Type: "python", Script: "for row in data:\n    metric(name='v', value=row['v'], labels={'id': row['id']})\n"},
		Limits: model.Limits{MaxMetrics: 100, ScriptTimeout: model.Duration(5 * time.Second), MaxOutputBytes: 1 << 20}}
	set, err := runBody(t, c, "text/csv", "id,v\na,1_000\n")
	if err != nil || len(set.Metrics) != 1 || set.Metrics[0].Value != 1000 {
		t.Fatalf("a python transform: %v, %v; want the 1000 Python reads", set, err)
	}
	rules := []model.MetricRule{{Name: "v", Type: model.GaugeMetricType, ErrorMode: model.ErrorModeFail, Expression: "v"}}
	c.Transform = model.TransformConfig{Type: "csv", PreScript: "data = [dict(row, v=row['v'].strip()) for row in data]\n"}
	c.Metrics = rules
	if set, err = runBody(t, c, "text/csv", "id,v\na, 1_000\n"); err == nil || !strings.HasSuffix(err.Error(), `value "1_000" is not a number; map text to numbers with value_map`) {
		t.Errorf("text a pre-script left: %v, %v; want the rule to fail", set, err)
	}
	c.Transform.PreScript = "data = [dict(row, v=int(row['v'])) for row in data]\n"
	if set, err = runBody(t, c, "text/csv", "id,v\na,1_000\n"); err != nil || len(set.Metrics) != 1 || set.Metrics[0].Value != 1000 {
		t.Errorf("a number a pre-script made: %v, %v; want 1000", set, err)
	}
}

// Two kinds of value are numbers before the exporter reads any text, and
// are what their maker made of them. A number an XPath function or operator
// computes — number(), sum(), arithmetic — is converted by the XPath engine,
// which reads 1_000 as 1000 and 0x1p-2 as 0.25, over XML and over HTML,
// from an element and from an attribute; the same node selected as it is has
// its text read by the exporter, and is no number. And an unquoted scalar of
// a YAML response is a number by YAML's own syntax, with digit separators,
// in hexadecimal, octal and binary, to a yq rule and a jq rule alike; a
// quoted one is text. This pins what the engine and the YAML reader do, so
// that a change of either is noticed: CONFIGURATION.md tells of both.
func TestNumbersTheXPathEngineAndYAMLMakeAreNotTextToARule(t *testing.T) {
	run := func(decoder, transform, contentType, body, expression string) (float64, error) {
		t.Helper()
		rule := model.MetricRule{Name: "v", Type: model.GaugeMetricType, ErrorMode: model.ErrorModeFail, Expression: expression}
		c := model.Collector{Name: "v", Decoder: model.DecoderConfig{Type: decoder}, Transform: model.TransformConfig{Type: transform}, Metrics: []model.MetricRule{rule}}
		if err := CheckMetricRule(&c, &c.Metrics[0]); err != nil {
			t.Fatal(err)
		}
		set, err := runBody(t, c, contentType, body)
		if err != nil {
			return 0, err
		}
		if len(set.Metrics) != 1 {
			t.Fatalf("%s over %q: %d series", expression, body, len(set.Metrics))
		}
		return set.Metrics[0].Value, nil
	}
	notANumber := func(text string) string {
		return fmt.Sprintf("value %q is not a number; map text to numbers with value_map", text)
	}
	for text, want := range map[string]float64{"1_000": 1000, "1_0.5": 10.5, "0x1p-2": 0.25, "-0X1P+4": -16} {
		for _, page := range []struct{ decoder, contentType, body, node string }{
			{"xml", "application/xml", `<r><v>%s</v></r>`, "//v"},
			{"xml", "application/xml", `<r><v v="%s"/></r>`, "//v/@v"},
			{"html", "text/html", `<html><body><p id="v">%s</p></body></html>`, "//p[@id='v']"},
			{"html", "text/html", `<html><body><p v="%s">x</p></body></html>`, "//p/@v"},
		} {
			body := fmt.Sprintf(page.body, text)
			for _, expression := range []string{"number(" + page.node + ")", "sum(" + page.node + ")", page.node + " * 1", page.node + " + 0"} {
				if got, err := run(page.decoder, "xpath", page.contentType, body, expression); err != nil || got != want {
					t.Errorf("%s over %s: %v, %v; want the %v the XPath engine makes of it", expression, body, got, err, want)
				}
			}
			if got, err := run(page.decoder, "xpath", page.contentType, body, page.node); err == nil || !strings.HasSuffix(err.Error(), notANumber(text)) {
				t.Errorf("%s over %s: %v, %v; want the text to be no number", page.node, body, got, err)
			}
		}
	}
	for text, want := range map[string]float64{"1_000": 1000, "1_000.5": 1000.5, "+1_0": 10, "0x10": 16, "0o17": 15, "017": 15, "0b101": 5} {
		for _, transform := range []string{"yq", "jq"} {
			if got, err := run("yaml", transform, "application/yaml", "v: "+text+"\n", ".v"); err != nil || got != want {
				t.Errorf("%s over an unquoted %s: %v, %v; want the %v YAML makes of it", transform, text, got, err, want)
			}
			for _, quote := range []string{`"`, `'`} {
				// 017 in quotes is the text of a decimal number.
				got, err := run("yaml", transform, "application/yaml", "v: "+quote+text+quote+"\n", ".v")
				if text == "017" {
					if err != nil || got != 17 {
						t.Errorf("%s over a quoted %s: %v, %v; want 17", transform, text, got, err)
					}
					continue
				}
				if err == nil || !strings.HasSuffix(err.Error(), notANumber(text)) {
					t.Errorf("%s over a quoted %s: %v, %v; want the text to be no number", transform, text, got, err)
				}
			}
		}
	}
	// A hexadecimal float is no number of YAML's, so it is text there too.
	if got, err := run("yaml", "yq", "application/yaml", "v: 0x1p-2\n", ".v"); err == nil || !strings.HasSuffix(err.Error(), notANumber("0x1p-2")) {
		t.Errorf("yq over an unquoted 0x1p-2: %v, %v; want the text to be no number", got, err)
	}
}
