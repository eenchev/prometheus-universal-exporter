package transform

import (
	"strings"
	"testing"

	"github.com/PuerkitoBio/goquery"
	"github.com/antchfx/htmlquery"
	"github.com/antchfx/xpath"
	"golang.org/x/net/html"

	"github.com/eenchev/prometheus-universal-exporter/internal/decode"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A label that is one attribute of the node is read by the attribute's name
// as the document writes it, and what xml means in a prefix
// (ownAttributeLabel, boundNamespaces).

// attributePage is a page whose attributes have every kind of name HTML
// lets them have: a colon in xml:lang, og:type, v-on:click and :href, a
// leading @ and a leading digit, dots, and letters that are not ASCII.
const attributePage = `<html xml:lang="en" lang="en"><body><table>
<tr og:type="a" v-on:click="go" data-id="7"><td class="v" größe="g" x-on:click.prevent="p" :href="h" @click="c" 2x="two" plain=" padded ">5</td></tr>
</table><svg><a xlink:href="#h" id="s"><text class="v">6</text></a></svg></body></html>`

// labelOf is the label l of the one series the collector makes of body.
func labelOf(t *testing.T, c model.Collector, contentType, body string) string {
	t.Helper()
	if err := CheckMetricRule(&c, &c.Metrics[0]); err != nil {
		t.Fatalf("label %s is refused at load: %v", c.Metrics[0].Labels[0].Expression, err)
	}
	// Whatever the node's text is, its series is there to carry the label.
	c.Metrics[0].ValueMap = map[string]float64{"*": 1}
	set, err := runBody(t, c, contentType, body)
	if err != nil || set == nil || len(set.Metrics) != 1 {
		t.Fatalf("label %s: %+v, %v", c.Metrics[0].Labels[0].Expression, set, err)
	}
	return set.Metrics[0].Labels["l"]
}

// mainsAttributeLabel is the label as main read it: everything after the @
// taken for the attribute's key and looked up on the node the rule selects.
func mainsAttributeLabel(t *testing.T, page, rule, label string) string {
	t.Helper()
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(page))
	if err != nil {
		t.Fatal(err)
	}
	node := htmlquery.FindOne(doc.Nodes[0], rule)
	if node == nil {
		t.Fatalf("%s selects nothing", rule)
	}
	return htmlquery.SelectAttr(node, strings.TrimPrefix(label, "@"))
}

// In HTML a colon is a character of an attribute's name like any other, and
// main read `@xml:lang`, `@og:type` and `@v-on:click` by that name. Taken
// for XPath, they named a prefix no HTML attribute has: the label was
// silently left off, and `@:href`, `@@click` and `@2x` no longer loaded.
// Every @ followed by one attribute name is read by the name as written,
// what main read it as, whatever response.namespaces holds.
func TestAnHTMLAttributeLabelIsReadByItsNameAsWritten(t *testing.T) {
	for _, tc := range []struct{ rule, label, want string }{
		{"/html", "@xml:lang", "en"},
		{"/html", "@lang", "en"},
		{"//tr", "@v-on:click", "go"},
		{"//tr", "@og:type", "a"},
		{"//tr", "@data-id", "7"},
		{"//td", "@größe", "g"},
		{"//td", "@x-on:click.prevent", "p"},
		{"//td", "@:href", "h"},
		{"//td", "@@click", "c"},
		{"//td", "@2x", "two"},
		{"//td", "@plain", " padded "},
		{"//td", "@absent:name", ""},
	} {
		if main := mainsAttributeLabel(t, attributePage, tc.rule, tc.label); main != tc.want {
			t.Fatalf("%s at %s: main read %q, and the test expects %q", tc.label, tc.rule, main, tc.want)
		}
		for name, namespaces := range map[string]map[string]string{
			"no namespaces":        nil,
			"the prefixes mapped":  {"v-on": "urn:v", "og": "urn:og", "xml": "urn:xml"},
			"other prefixes alone": {"a": "http://www.w3.org/2005/Atom"},
		} {
			for _, decoder := range []string{"html", "auto"} {
				c := xpathLabelCollector(decoder, tc.rule, tc.label, namespaces)
				if got := labelOf(t, c, "text/html", attributePage); got != tc.want {
					t.Errorf("%s at %s (decoder %s, %s) = %q, want %q as on main", tc.label, tc.rule, decoder, name, got, tc.want)
				}
			}
		}
	}
}

// The same holds for an attribute of a parent, which the XPath engine
// cannot name when its name has a colon or does not parse: `../@og:type`
// read nothing. It is read from the parent by the name as written; from a
// node that has no parent to walk to, nothing is.
func TestAnHTMLAttributeOfAParentIsReadByItsNameAsWritten(t *testing.T) {
	for label, want := range map[string]string{
		"../@og:type":              "a",
		"../@v-on:click":           "go",
		"../@data-id":              "7",
		"../../../../../@xml:lang": "en",
		"../@absent:name":          "",
	} {
		for _, namespaces := range []map[string]string{nil, {"a": "http://www.w3.org/2005/Atom"}} {
			if got := labelOf(t, xpathLabelCollector("html", "//td", label, namespaces), "text/html", attributePage); got != want {
				t.Errorf("%s (namespaces %v) = %q, want %q", label, namespaces, got, want)
			}
		}
	}
	// A text node the rule selects is no element to walk from, and the
	// engine reads nothing by such a name.
	c := xpathLabelCollector("html", "//td/text()", "../@og:type", nil)
	if got := labelOf(t, c, "text/html", attributePage); got != "" {
		t.Errorf("../@og:type from a text node = %q", got)
	}
}

// An attribute HTML itself gives a namespace inside svg, as xlink:href,
// which the parser splits into the two, is found by the name as written
// too. Main, looking for the whole name as a key, found nothing there.
func TestAnHTMLAttributeOfAnSVGElementIsReadByItsNameAsWritten(t *testing.T) {
	if main := mainsAttributeLabel(t, attributePage, "//svg/a", "@xlink:href"); main != "" {
		t.Fatalf("main read %q", main)
	}
	for label, want := range map[string]string{"@xlink:href": "#h", "@href": "#h", "@id": "s"} {
		c := xpathLabelCollector("html", "//svg/a", label, nil)
		if got := labelOf(t, c, "text/html", attributePage); got != want {
			t.Errorf("%s = %q, want %q", label, got, want)
		}
	}
	c := xpathLabelCollector("html", "//svg/a/text", "../@xlink:href", nil)
	if got := labelOf(t, c, "text/html", attributePage); got != "#h" {
		t.Errorf("../@xlink:href = %q", got)
	}
}

// What follows the @ is one attribute's name only where it has nothing in
// it XPath takes for an operator. Anything else over HTML is XPath as
// before: evaluated when it parses, and refused at load, naming the label,
// when it does not.
func TestAnHTMLAttributeLabelWithAnOperatorIsXPath(t *testing.T) {
	for label, want := range map[string]string{
		"@data-id | @plain":    "padded",
		"@plain = ' padded '":  "true",
		"@*[name()='2x']":      "two",
		"@*[name()=':href']":   "h",
		"@ plain":              "padded",
		"@plain/../@2x | @abc": "",
	} {
		if label == "@plain/../@2x | @abc" {
			c := xpathLabelCollector("html", "//td", label, nil)
			if err := CheckMetricRule(&c, &c.Metrics[0]); err == nil || !strings.HasPrefix(err.Error(), `collector "attributes" metric "v" label "l" XPath "`+label+`": `) {
				t.Errorf("%s: %v", label, err)
			}
			continue
		}
		if got := labelOf(t, xpathLabelCollector("html", "//td", label, nil), "text/html", attributePage); got != want {
			t.Errorf("%s = %q, want %q", label, got, want)
		}
	}
	for _, label := range []string{"@[attr]", "@id |", "@*[", "@", "../@[attr]", "../@"} {
		for _, decoder := range []string{"html", "auto", "xml"} {
			c := xpathLabelCollector(decoder, "//td", label, nil)
			if err := CheckMetricRule(&c, &c.Metrics[0]); err == nil || !strings.HasPrefix(err.Error(), `collector "attributes" metric "v" label "l" XPath "`+label+`": `) {
				t.Errorf("%s with decoder %s: %v, want it refused naming the label", label, decoder, err)
			}
		}
	}
	for name, want := range map[string]bool{
		"lang": true, "xml:lang": true, "v-on:click": true, ":href": true, "@click": true, "2x": true, "x-on:click.prevent": true, "größe": true,
		"": false, "a b": false, "a/b": false, "a|b": false, "[attr]": false, "a]": false, "f(x": false, "x)": false, "a=b": false, "a<b": false, "a>b": false, "a!=b": false,
		"a+b": false, "*": false, "a*b": false, "a,b": false, "$a": false, "a'b": false, `a"b`: false, "a\tb": false, "a\nb": false,
	} {
		if got := htmlAttributeName(name); got != want {
			t.Errorf("htmlAttributeName(%q) = %v, want %v", name, got, want)
		}
	}
}

// attributeFeed is an XML document with attributes in the xml namespace,
// which no document declares, and in one of its own.
const attributeFeed = `<feed xmlns="http://www.w3.org/2005/Atom" xmlns:x="urn:x">
  <entry xml:lang="en" x:kind="post" id="1"><v>1</v></entry>
  <entry xml:lang="de" x:kind="page" id="2"><v>2</v></entry>
</feed>`

// feedLabels is the label l of each series the collector makes of
// attributeFeed, in document order.
func feedLabels(t *testing.T, c model.Collector) []string {
	t.Helper()
	if err := CheckMetricRule(&c, &c.Metrics[0]); err != nil {
		t.Fatalf("label %s is refused at load: %v", c.Metrics[0].Labels[0].Expression, err)
	}
	set, err := runBody(t, c, "application/xml", attributeFeed)
	if err != nil || set == nil {
		t.Fatalf("label %s: %+v, %v", c.Metrics[0].Labels[0].Expression, set, err)
	}
	var labels []string
	for _, m := range set.Metrics {
		labels = append(labels, m.Labels["l"])
	}
	return labels
}

// XML reserves the prefix xml for one namespace, which no document declares
// and no collector should have to map. Once response.namespaces mapped any
// prefix, `@xml:lang` was an unmapped one and the configuration no longer
// loaded. xml is bound whatever the map holds, in a label read from the
// node, in one the engine evaluates, and in a rule's own expression.
func TestThePrefixXMLIsAlwaysBound(t *testing.T) {
	for name, namespaces := range map[string]map[string]string{
		"no namespaces":           nil,
		"an empty map":            {},
		"another prefix mapped":   {"a": "http://www.w3.org/2005/Atom"},
		"xml mapped as it is":     {"a": "http://www.w3.org/2005/Atom", "xml": xmlNamespace},
		"xml mapped to something": {"a": "http://www.w3.org/2005/Atom", "xml": "urn:not-xml"},
	} {
		for label, rule := range map[string]string{
			"@xml:lang":                  "//*[local-name()='entry']/*[local-name()='v']/..",
			"../@xml:lang":               "//*[local-name()='v']",
			"string(../@xml:lang)":       "//*[local-name()='v']",
			"ancestor::*[@xml:lang]/@id": "//*[local-name()='v']",
		} {
			c := xpathLabelCollector("xml", rule, label, namespaces)
			c.Metrics[0].ValueMap = map[string]float64{"*": 1}
			want := "en de"
			if strings.HasSuffix(label, "@id") {
				want = "1 2"
			}
			if got := strings.Join(feedLabels(t, c), " "); got != want {
				t.Errorf("%s (%s) = %q, want %q", label, name, got, want)
			}
		}
		c := xpathLabelCollector("xml", "//*[@xml:lang='de']/*", "../@id", namespaces)
		if got := strings.Join(feedLabels(t, c), " "); got != "2" {
			t.Errorf("a rule with @xml:lang in its expression (%s) selected the entries %q", name, got)
		}
	}
	bound := boundNamespaces(map[string]string{"a": "urn:a"})
	if len(bound) != 2 || bound["a"] != "urn:a" || bound["xml"] != xmlNamespace {
		t.Fatalf("%v", bound)
	}
	if boundNamespaces(nil) != nil || len(boundNamespaces(map[string]string{})) != 0 {
		t.Fatal("namespaces were bound where none are set")
	}
}

// Without response.namespaces a prefixed attribute is read by the name as
// written, with the document's own prefix, as main read it and as
// configurations written before there were namespaces to set expect. With
// response.namespaces set a prefix is the collector's own: one it maps
// matches by the URI, whatever the document's prefix, and one it does not
// map, other than xml, is refused when the configuration loads.
func TestAPrefixedXMLAttributeLabel(t *testing.T) {
	entries := "//*[local-name()='entry']"
	own := func(label string, namespaces map[string]string) model.Collector {
		c := xpathLabelCollector("xml", entries, label, namespaces)
		c.Metrics[0].ValueMap = map[string]float64{"*": 1}
		return c
	}
	if got := strings.Join(feedLabels(t, own("@x:kind", nil)), " "); got != "post page" {
		t.Errorf("the document's own prefix, without response.namespaces: %q", got)
	}
	if _, byName := ownAttributeLabel("@x:kind", false, nil); !byName {
		t.Error("@x:kind is not read by name without response.namespaces")
	}
	if got := strings.Join(feedLabels(t, own("@nope:kind", nil)), " "); got != " " {
		t.Errorf("a prefix the document does not have: %q", got)
	}
	if got := strings.Join(feedLabels(t, own("@x:kind", map[string]string{"x": "urn:x"})), " "); got != "post page" {
		t.Errorf("the prefix mapped: %q", got)
	}
	if got := strings.Join(feedLabels(t, own("@y:kind", map[string]string{"y": "urn:x"})), " "); got != "post page" {
		t.Errorf("another prefix mapped to the document's namespace: %q", got)
	}
	if got := strings.Join(feedLabels(t, own("@x:kind", map[string]string{"x": "urn:other"})), " "); got != " " {
		t.Errorf("the document's prefix mapped to another namespace: %q", got)
	}
	c := xpathLabelCollector("xml", entries, "@x:kind", map[string]string{"a": "http://www.w3.org/2005/Atom"})
	if err := CheckMetricRule(&c, &c.Metrics[0]); err == nil || !strings.Contains(err.Error(), `label "l" XPath "@x:kind": prefix x not defined`) {
		t.Errorf("an unmapped prefix with response.namespaces set: %v", err)
	}
	// What is read by name in XML is a plain name, and a plain name with a
	// plain prefix where no namespaces are set; a name the engine reads
	// otherwise, or with namespaces set, is the engine's.
	namespaces := map[string]string{"x": "urn:x"}
	for expression, want := range map[string][2]bool{
		"@id":       {true, true},
		"@x:kind":   {true, false},
		"@xml:lang": {true, false},
		"@a:b:c":    {false, false},
		"@:b":       {false, false},
		"@a:":       {false, false},
		"@x:*":      {false, false},
		"@größe":    {false, false},
		"@2x":       {false, false},
		"x:kind":    {false, false},
	} {
		_, unset := ownAttributeLabel(expression, false, nil)
		_, set := ownAttributeLabel(expression, false, namespaces)
		if unset != want[0] || set != want[1] {
			t.Errorf("ownAttributeLabel(%q): %v without namespaces and %v with, want %v", expression, unset, set, want)
		}
	}
}

// A prefixed attribute read by name, as it is without response.namespaces,
// is what the XPath engine reads there too, at every element of documents
// with prefixes of every kind: the value, untrimmed where the engine trims
// it, and empty where the engine finds none.
func TestAPrefixedAttributeByNameIsTheEnginesAtAnElement(t *testing.T) {
	compared, found := 0, 0
	for i, document := range append([]string{attributeFeed}, xpathLabelDocuments...) {
		root, err := decode.ParseXML([]byte(document))
		if err != nil {
			t.Fatal(err)
		}
		for _, expression := range []string{"@x:id", "@y:id", "@x:site", "@x:kind", "@xml:lang", "@nope:id"} {
			rule := model.MetricRule{Name: "m", Labels: []model.LabelRule{{Name: "l", Expression: expression}}}
			plan := planXPathLabels(rule, nil, false)
			if plan[0].kind != xpathLabelOwnAttribute {
				t.Fatalf("%s is not read by name", expression)
			}
			selector, err := xpath.Compile(expression)
			if err != nil {
				t.Fatal(err)
			}
			for at, node := range treeNodes(t, xmlNodes, root) {
				if !xmlNodes.element(node) {
					continue
				}
				engine := ""
				if attribute, ok := xmlNodes.one(node, selector); ok {
					engine = strings.TrimSpace(xmlNodes.text(attribute))
				}
				got := xpathLabels(xmlNodes, node, plan)["l"]
				if strings.TrimSpace(got) != engine {
					t.Fatalf("%s at element %d of document %d: %q by name, %q by the engine", expression, at, i, got, engine)
				}
				compared++
				if got != "" {
					found++
				}
			}
		}
	}
	if compared < 100 || found < 8 {
		t.Fatalf("%d labels compared, %d of them found", compared, found)
	}
}

// The parser the html decoder uses splits the attributes HTML gives a
// namespace, and keeps every other name whole: what htmlAttribute relies on.
func TestHTMLAttributeNamesAsParsed(t *testing.T) {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(attributePage))
	if err != nil {
		t.Fatal(err)
	}
	link := htmlquery.FindOne(doc.Nodes[0], "//svg/a")
	if len(link.Attr) != 2 || link.Attr[0] != (html.Attribute{Namespace: "xlink", Key: "href", Val: "#h"}) {
		t.Fatalf("%+v", link.Attr)
	}
	row := htmlquery.FindOne(doc.Nodes[0], "//tr")
	if row.Attr[0] != (html.Attribute{Key: "og:type", Val: "a"}) {
		t.Fatalf("%+v", row.Attr)
	}
}
