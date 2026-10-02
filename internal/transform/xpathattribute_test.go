package transform

import (
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// xpathLabelCollector is an xpath collector of one rule with one label, read
// with the namespaces given.
func xpathLabelCollector(decoder, expression, label string, namespaces map[string]string) model.Collector {
	return model.Collector{Name: "attributes", Decoder: model.DecoderConfig{Type: decoder}, Transform: model.TransformConfig{Type: "xpath"},
		Response: model.ResponseConfig{Namespaces: namespaces},
		Metrics:  []model.MetricRule{{Name: "v", Expression: expression, ErrorMode: model.ErrorModeFail, Labels: []model.LabelRule{{Name: "l", Expression: label}}}}}
}

// A label expression starting with @ is read straight from the node only
// when it is one attribute by a plain name. Anything else is XPath, as the
// same expression is behind ./ : a prefixed attribute means what
// response.namespaces says its prefix means, and a union, a predicate, a
// comparison or a step after the attribute is evaluated. They were all taken
// for an attribute's name, which no node has, and the label was silently
// left off.
func TestXPathLabelsStartingWithAtAreXPath(t *testing.T) {
	const body = `<?xml version="1.0"?><r xmlns="urn:d" xmlns:xl="http://www.w3.org/1999/xlink"><job id="a" data-id="7" xl:href="http://x" state="ok"> 5 </job></r>`
	namespaces := map[string]string{"d": "urn:d", "x": "http://www.w3.org/1999/xlink"}
	for label, want := range map[string]string{
		"@id":           "a",
		"@data-id":      "7",
		"@absent":       "",
		"@x:href":       "http://x", // the document's prefix is xl; the collector's x names the same namespace
		"./@x:href":     "http://x",
		"@id | @state":  "a",
		"@absent | @id": "a",
		"@state='ok'":   "true",
		"@*[1]":         "a",
		"@id/..":        "5",
		"@ id":          "a",
	} {
		c := xpathLabelCollector("xml", "//d:job", label, namespaces)
		if err := CheckMetricRule(&c, &c.Metrics[0]); err != nil {
			t.Errorf("%s: refused at load: %v", label, err)
			continue
		}
		set, err := runBody(t, c, "application/xml", body)
		if err != nil || len(set.Metrics) != 1 {
			t.Errorf("%s: %+v, %v", label, set, err)
			continue
		}
		if got, has := set.Metrics[0].Labels["l"]; got != want || has != (want != "") {
			t.Errorf("label %s = %q (set: %v), want %q", label, got, has, want)
		}
	}
	// Without response.namespaces a prefix is the document's own.
	c := xpathLabelCollector("xml", "//*[local-name()='job']", "@xl:href", nil)
	if set, err := runBody(t, c, "application/xml", body); err != nil || set.Metrics[0].Labels["l"] != "http://x" {
		t.Fatalf("the document's own prefix: %+v, %v", set, err)
	}
	// Over HTML, where there are no namespaces, the same holds.
	c = xpathLabelCollector("html", "//li", "@data-id | @id", nil)
	if set, err := runBody(t, c, "text/html", `<html><body><ul><li id="b">3</li></ul></body></html>`); err != nil || set.Metrics[0].Labels["l"] != "b" {
		t.Fatalf("HTML: %+v, %v", set, err)
	}
}

// Such a label is checked when the configuration loads, like any other
// expression: a prefix response.namespaces does not have, and an expression
// that does not parse, are refused naming the label, where everything after
// an @ was accepted unread.
func TestXPathLabelsStartingWithAtAreCheckedAtLoad(t *testing.T) {
	namespaces := map[string]string{"x": "http://www.w3.org/1999/xlink"}
	for label, want := range map[string]string{
		"@xl:href": `collector "attributes" metric "v" label "l" XPath "@xl:href": `,
		"@id |":    `collector "attributes" metric "v" label "l" XPath "@id |": `,
		"@*[":      `collector "attributes" metric "v" label "l" XPath "@*[": `,
	} {
		c := xpathLabelCollector("xml", "//job", label, namespaces)
		if err := CheckMetricRule(&c, &c.Metrics[0]); err == nil || !strings.HasPrefix(err.Error(), want) {
			t.Errorf("%s: %v, want an error starting %q", label, err, want)
		}
	}
	for _, label := range []string{"@id", "@data-id", "@_a.b", "@x:href", "@*"} {
		c := xpathLabelCollector("xml", "//job", label, namespaces)
		if err := CheckMetricRule(&c, &c.Metrics[0]); err != nil {
			t.Errorf("%s: %v", label, err)
		}
	}
	// What is read straight from an XML node, with response.namespaces set,
	// is a plain name and nothing else.
	for expression, want := range map[string]bool{
		"@id": true, "@data-id": true, "@_a.b9": true, "@A": true,
		"@": false, "@9a": false, "@-a": false, "@x:href": false, "@*": false, "@a b": false, "@a|@b": false, "@é": false, "id": false, "": false,
	} {
		if _, got := ownAttributeLabel(expression, false, namespaces); got != want {
			t.Errorf("ownAttributeLabel(%q) = %v, want %v", expression, got, want)
		}
	}
}

// The namespaces example of CONFIGURATION.md gives the series it says, and
// the rest of what that section says holds: a mapped prefix matches by URI,
// whatever the document's own prefix, or none; a prefix the map does not
// have is refused at load; and without a map a prefix is the document's own.
func TestXMLNamespacesAsDocumented(t *testing.T) {
	const body = `<feed xmlns="http://www.w3.org/2005/Atom" xmlns:m="urn:example:metrics">
  <entry><title>db01</title><m:size m:unit="bytes">5120</m:size></entry>
  <other xmlns="urn:example:other"><entry><title>not atom</title><m:size>7</m:size></entry></other>
</feed>`
	namespaces := map[string]string{"a": "http://www.w3.org/2005/Atom", "x": "urn:example:metrics"}
	c := xpathLabelCollector("xml", "//a:entry/x:size", "@x:unit", namespaces)
	c.Metrics[0].Name = "entry_size"
	c.Metrics[0].Labels = []model.LabelRule{{Name: "unit", Expression: "@x:unit"}, {Name: "entry", Expression: "../a:title"}}
	if err := CheckMetricRule(&c, &c.Metrics[0]); err != nil {
		t.Fatal(err)
	}
	set, err := runBody(t, c, "application/xml", body)
	// Only the entry of the Atom namespace, not the one of another default
	// namespace, which a name without a prefix does not tell apart.
	if err != nil || len(set.Metrics) != 1 || set.Metrics[0].Value != 5120 || set.Metrics[0].Labels["unit"] != "bytes" || set.Metrics[0].Labels["entry"] != "db01" {
		t.Fatalf("%+v, %v", set, err)
	}
	c = xpathLabelCollector("xml", "//entry/x:size", "@x:unit", namespaces)
	if set, err := runBody(t, c, "application/xml", body); err != nil || len(set.Metrics) != 2 {
		t.Fatalf("entry without a prefix: %+v, %v", set, err)
	}
	// The document's own prefix is not one of the collector's.
	c = xpathLabelCollector("xml", "//a:entry/m:size", "@x:unit", namespaces)
	if err := CheckMetricRule(&c, &c.Metrics[0]); err == nil || !strings.Contains(err.Error(), `XPath "//a:entry/m:size"`) {
		t.Fatalf("an unmapped prefix in the expression: %v", err)
	}
	c = xpathLabelCollector("xml", "//a:entry/x:size", "@m:unit", namespaces)
	if err := CheckMetricRule(&c, &c.Metrics[0]); err == nil || !strings.Contains(err.Error(), `label "l" XPath "@m:unit"`) {
		t.Fatalf("an unmapped prefix in the label: %v", err)
	}
	// Without a map, it is.
	c = xpathLabelCollector("xml", "//feed/entry/m:size", "@m:unit", nil)
	if err := CheckMetricRule(&c, &c.Metrics[0]); err != nil {
		t.Fatal(err)
	}
	if set, err := runBody(t, c, "application/xml", body); err != nil || len(set.Metrics) != 1 || set.Metrics[0].Labels["l"] != "bytes" {
		t.Fatalf("without a map: %+v, %v", set, err)
	}
}
