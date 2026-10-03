package transform

import (
	"reflect"
	"strings"
	"testing"

	"github.com/antchfx/xmlquery"
	"github.com/antchfx/xpath"

	"github.com/eenchev/prometheus-universal-exporter/internal/decode"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A rule that selects attributes, as `//i/@w` does, takes its labels from
// each attribute as XPath has it: `.` and `string(.)` are its value,
// `name()` its name, `..` its element, and a path through `..` whatever is
// there. xmlquery hands a selected attribute over as a node its navigator
// could not start from, so a label the engine evaluates ended the scrape
// with a panic, `unknown XML node type: 6`, for the labels below but those
// read straight from the element.
func TestXPathLabelsOfASelectedAttribute(t *testing.T) {
	const body = `<r><i id="1" w="5" x:w="50" xmlns:x="urn:x"><name>a</name></i><i id="2" w="6"><name>b</name></i></r>`
	labels := []model.LabelRule{
		{Name: "self", Expression: "."},
		{Name: "text", Expression: "string(.)"},
		{Name: "name", Expression: "name()"},
		{Name: "id", Expression: "../@id"},
		{Name: "of", Expression: "../name"},
		{Name: "element", Expression: "name(..)"},
		{Name: "siblings", Expression: "count(../@*)"},
	}
	for expression, want := range map[string][]map[string]string{
		// The first element has a w and an x:w: the attribute selected is
		// the one of that value.
		"//i/@w": {
			// Four attributes: xmlquery counts the namespace declaration.
			{"self": "5", "text": "5", "name": "w", "id": "1", "of": "a", "element": "i", "siblings": "4"},
			{"self": "6", "text": "6", "name": "w", "id": "2", "of": "b", "element": "i", "siblings": "2"},
		},
		// An element and an attribute of it, selected together.
		"//i[@id=2]/name | //i[@id=2]/@id": {
			{"self": "b", "text": "b", "name": "name", "id": "2", "of": "b", "element": "i", "siblings": "2"},
			{"self": "2", "text": "2", "name": "id", "id": "2", "of": "b", "element": "i", "siblings": "2"},
		},
	} {
		c := model.Collector{Name: "attributes", Decoder: model.DecoderConfig{Type: "xml"}, Transform: model.TransformConfig{Type: "xpath"},
			Metrics: []model.MetricRule{{Name: "m", Type: model.GaugeMetricType, Expression: expression, ValueMap: map[string]float64{"*": 1}, Labels: labels}}}
		set, err := runBody(t, c, "application/xml", body)
		if err != nil {
			t.Fatalf("%s: %v", expression, err)
		}
		var got []map[string]string
		for _, m := range set.Metrics {
			got = append(got, m.Labels)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: labels\n%v\nwant\n%v", expression, got, want)
		}
	}
}

// On a rule that selects attributes the label `text()` is the attribute's
// value, as `.` is. To XPath an attribute has no text node beneath it, and
// once the engine was started on the attribute itself `text()` selected
// nothing: the label was left off, and the series of `//@id`, which had
// been told apart by it, became duplicates of one. Main read the value,
// from the node xmlquery makes of a selected attribute, which holds the
// value as a text child; mainsTextOfAttributes reads it that way. HTML,
// where a selected attribute is handed over as an element holding its
// value, gave the value throughout.
func TestTheLabelTextOfASelectedAttributeIsItsValue(t *testing.T) {
	const body = `<r><i id="1" w=" 5 "><name>a</name></i><i id="2" w="6"/><i id="" w="7"/></r>`
	const page = `<html><body><i id="1" w=" 5 ">a</i><i id="2" w="6">b</i><i id="" w="7">c</i></body></html>`
	for _, expression := range []string{"//i/@w", "//@id", "//@*"} {
		for decoder, document := range map[string]string{"xml": body, "html": page} {
			c := model.Collector{Name: "attributes", Decoder: model.DecoderConfig{Type: decoder}, Transform: model.TransformConfig{Type: "xpath"},
				Metrics: []model.MetricRule{{Name: "m", Type: model.GaugeMetricType, Expression: expression, ValueMap: map[string]float64{"*": 1}, Required: new(bool),
					Labels: []model.LabelRule{{Name: "text", Expression: "text()"}, {Name: "self", Expression: "."}}}}}
			if err := CheckMetricRule(&c, &c.Metrics[0]); err != nil {
				t.Fatal(err)
			}
			set, err := runBody(t, c, map[string]string{"xml": "application/xml", "html": "text/html"}[decoder], document)
			if err != nil {
				t.Fatalf("%s over %s: %v", expression, decoder, err)
			}
			var got []string
			for _, m := range set.Metrics {
				if m.Labels["text"] != m.Labels["self"] {
					t.Errorf("%s over %s: text() is %q where . is %q", expression, decoder, m.Labels["text"], m.Labels["self"])
				}
				got = append(got, m.Labels["text"])
			}
			// The attribute without a value is no series: it has no text
			// to make a value of.
			want := map[string][]string{"//i/@w": {"5", "6", "7"}, "//@id": {"1", "2"}, "//@*": {"1", "5", "2", "6", "7"}}[expression]
			if !reflect.DeepEqual(got, want) {
				t.Errorf("%s over %s: text() = %q, want %q", expression, decoder, got, want)
			}
			if decoder == "xml" {
				if main := mainsTextOfAttributes(t, document, expression); !reflect.DeepEqual(got, main) {
					t.Errorf("%s: text() = %q, and main read %q", expression, got, main)
				}
			}
			if err := set.Validate(c.Limits); err != nil {
				t.Errorf("%s over %s: %v", expression, decoder, err)
			}
		}
	}
}

// mainsTextOfAttributes is the label text() of each attribute with a value
// that expression selects, as main read it: evaluated from the node
// xmlquery makes of the attribute, whose one child is the value as text.
func mainsTextOfAttributes(t *testing.T, document, expression string) []string {
	t.Helper()
	root, err := decode.ParseXML([]byte(document))
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	for _, attribute := range xmlquery.QuerySelectorAll(root, xpath.MustCompile(expression)) {
		if strings.TrimSpace(attribute.InnerText()) == "" {
			continue
		}
		text := xmlquery.QuerySelector(attribute, xpath.MustCompile("text()"))
		if text == nil {
			t.Fatalf("main read no text() at %s", attribute.Data)
		}
		texts = append(texts, strings.TrimSpace(text.InnerText()))
	}
	return texts
}
