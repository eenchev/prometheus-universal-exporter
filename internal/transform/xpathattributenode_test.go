package transform

import (
	"reflect"
	"testing"

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
