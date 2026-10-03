package transform

import (
	"errors"
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
// setTheDecoderToHTML is what the load adds to an XPath error for a label
// that is a name only HTML has, of a collector whose decoder is left to each
// response and which is taken to read XML for its response.namespaces.
const setTheDecoderToHTML = "; response.namespaces is set, so the labels are checked as those of an XML document: if the target answers HTML, where this label is an attribute's name as written, set decoder.type to html"

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
// what main read it as, whatever response.namespaces holds. Only a collector
// that sets response.namespaces and leaves its decoder to each response has
// its labels checked as XML's when it loads
// (TestNamespacesMakeADecoderLeftToEachResponseCheckItsLabelsAsXML): a name
// XML cannot have is refused there, and one it can is read from HTML by the
// name as written all the same.
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
				if decoder == "auto" && namespaces != nil {
					asXML := xpathLabelCollector("xml", tc.rule, tc.label, namespaces)
					if refused := CheckMetricRule(&asXML, &asXML.Metrics[0]); refused != nil {
						// Each is a name HTML has, so the error says how
						// such a document is read.
						if err := CheckMetricRule(&c, &c.Metrics[0]); err == nil || err.Error() != refused.Error()+setTheDecoderToHTML {
							t.Errorf("%s (decoder auto, %s) at load: %v, want it refused as with an xml decoder, and told of decoder.type: %v", tc.label, name, err, refused)
						}
						continue
					}
				}
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
// node that has no parent to walk to, nothing is. The node need not be an
// element: from a text node, as `//td/text()` selects them, such a label
// still read nothing, the walk being taken from elements alone, while
// `../@data-id`, which the engine can say, was read. The parent of a text
// node is an element like any other, and its attribute is read by the name
// as written too.
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
	// From the text node of the cell: its parent is the cell, the cell's
	// the row, and the sixth the html element.
	for label, want := range map[string]string{
		"../@2x":                      "two",
		"../@:href":                   "h",
		"../@x-on:click.prevent":      "p",
		"../@plain":                   "padded",
		"../../@og:type":              "a",
		"../../@v-on:click":           "go",
		"../../@data-id":              "7",
		"../../../../../../@xml:lang": "en",
		"../@og:type":                 "",
		"../../@absent:name":          "",
	} {
		c := xpathLabelCollector("html", "//td/text()", label, nil)
		if got := labelOf(t, c, "text/html", attributePage); got != want {
			t.Errorf("%s from a text node = %q, want %q", label, got, want)
		}
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

// unitFeed is an XML answer whose attributes have a prefix of the
// document's own, x, beside ones without.
const unitFeed = `<feed xmlns="urn:d" xmlns:x="urn:x"><entry xml:lang="de" x:kind="post" kind="plain"><v class="v" x:unit="ms" unit="s">1</v></entry></feed>`

// Namespaces mean something only in XML, so a collector that sets
// response.namespaces reads XML, whatever its decoder is left to. With the
// decoder unset or auto its labels were checked as those of either kind of
// document, and `@x:unit`, whose prefix the namespaces do not map, passed as
// the name of an HTML attribute: the configuration loaded, and the label was
// left off every series of the XML answer without a word, where main read
// it and the round before refused it at load. It is refused again, with the
// message an xml decoder gives, as is every other label XML cannot have;
// what the namespaces map, and xml, is read.
func TestNamespacesMakeADecoderLeftToEachResponseCheckItsLabelsAsXML(t *testing.T) {
	namespaces := map[string]string{"y": "urn:x"}
	const values = `//*[@class="v"]`
	// The labels that are one attribute's name, as HTML reads them, are
	// refused with a word on decoder.type; an expression is not such a name
	// in either kind of document and has the xml decoder's message alone.
	htmlNames := map[string]bool{"@x:unit | @unit": false, "string(@x:unit)": false}
	for label, reason := range map[string]string{
		"@x:unit":         "prefix x not defined",
		"../@x:kind":      "prefix x not defined",
		"@x:unit | @unit": "prefix x not defined",
		"string(@x:unit)": "prefix x not defined",
		"@1x":             "",
		"../@1x":          "",
		"@a:b:c":          "",
		"../@a:b:c":       "",
		"@:unit":          "",
		"../@:kind":       "",
		"@x:":             "",
	} {
		asXML := xpathLabelCollector("xml", values, label, namespaces)
		refused := CheckMetricRule(&asXML, &asXML.Metrics[0])
		if refused == nil || !strings.HasPrefix(refused.Error(), `collector "attributes" metric "v" label "l" XPath "`+label+`": `+reason) {
			t.Fatalf("%s with an xml decoder: %v", label, refused)
		}
		want := refused.Error()
		if named, listed := htmlNames[label]; named || !listed {
			want += setTheDecoderToHTML
		}
		for _, decoder := range []string{"", "auto"} {
			c := xpathLabelCollector(decoder, values, label, namespaces)
			if err := CheckMetricRule(&c, &c.Metrics[0]); err == nil || err.Error() != want {
				t.Errorf("%s with the decoder %q and namespaces set: %v, want it refused as with an xml decoder: %v", label, decoder, err, want)
			}
		}
		// A decoder that is set is not told to be set.
		if strings.Contains(refused.Error(), "decoder.type") {
			t.Errorf("%s with an xml decoder is told of decoder.type: %v", label, refused)
		}
	}
	for label, want := range map[string]string{
		"@y:unit":         "ms",
		"@unit":           "s",
		"../@y:kind":      "post",
		"../@kind":        "plain",
		"../@xml:lang":    "de",
		"string(@y:unit)": "ms",
		"@y:unit | @unit": "ms",
		"@y:nosuch":       "",
	} {
		for _, decoder := range []string{"", "auto", "xml"} {
			c := xpathLabelCollector(decoder, values, label, namespaces)
			if got := labelOf(t, c, "application/xml", unitFeed); got != want {
				t.Errorf("%s with the decoder %q = %q, want %q", label, decoder, got, want)
			}
		}
	}
	// An html decoder reads HTML alone, where every attribute is read by
	// its name as written whatever the namespaces hold: its labels are
	// checked as before.
	c := xpathLabelCollector("html", "//td", "@x:unit", namespaces)
	if got := labelOf(t, c, "text/html", `<table><tr><td x:unit="ms">1</td></tr></table>`); got != "ms" {
		t.Errorf("@x:unit with an html decoder and namespaces set = %q", got)
	}
}

// Without response.namespaces a collector whose decoder is left to each
// response may get either kind of document, and a label one of them can
// give is accepted: `@1x`, `@a:b:c` and `../@:kind` are attributes HTML can
// have and XPath cannot say. When the answer is XML, such a label cannot be
// read at all. It was left off without a word, and the rule's series were
// exported without it; now the rule fails, by its error_mode as for any
// label it cannot give, and says which label, in which kind of document,
// and why.
func TestALabelTheDocumentThatArrivedCannotGiveFailsTheRule(t *testing.T) {
	const page = `<html><body><table><tr :kind="row"><td class="v" 1x="one" a:b:c="abc">5</td></tr></table></body></html>`
	const answer = `<r kind="row"><v class="v">5</v><w>7</w></r>`
	const values = `//*[@class="v"]`
	for label, inHTML := range map[string]string{"@1x": "one", "@a:b:c": "abc", "../@:kind": "row"} {
		for _, decoder := range []string{"", "auto"} {
			c := xpathLabelCollector(decoder, values, label, nil)
			if got := labelOf(t, c, "text/html", page); got != inHTML {
				t.Errorf("%s with the decoder %q over HTML = %q, want %q", label, decoder, got, inHTML)
			}
			message := `metric "v" label "l": XPath "` + label + `" cannot be read in the XML document that arrived (`
			for _, expression := range []string{values, "count(//v)"} {
				// Under fail the scrape fails, as the rule's failure.
				c := xpathLabelCollector(decoder, expression, label, nil)
				set, err := runBody(t, c, "application/xml", answer)
				var failure *MetricFailure
				if set != nil || !errors.As(err, &failure) || failure.Metric != "v" || !strings.Contains(err.Error(), message) || !strings.Contains(err.Error(), "set decoder.type") {
					t.Errorf("%s at %s with the decoder %q over XML: %+v, %v, want the rule failed naming the label", label, expression, decoder, set, err)
				}
				// Under log and ignore the rule gives nothing, its failure
				// is counted once, and the rule after it is read.
				for _, mode := range []string{model.ErrorModeLog, model.ErrorModeIgnore} {
					c := xpathLabelCollector(decoder, expression, label, nil)
					c.Metrics[0].ErrorMode = mode
					c.Metrics = append(c.Metrics, model.MetricRule{Name: "after", Expression: "//w"})
					set, failures, err := transformWith(LeaveRuleLoggingToCaller(t.Context()), t, c, "application/xml", answer)
					if err != nil || set == nil || len(set.Metrics) != 1 || set.Metrics[0].Name != "after" || set.Metrics[0].Value != 7 {
						t.Errorf("%s at %s with the decoder %q under %s: %+v, %v, want only the rule after it", label, expression, decoder, mode, set, err)
					}
					if len(failures) != 1 || failures[0].Metric != "v" || failures[0].Failures != 1 || failures[0].Logged != (mode == model.ErrorModeLog) || !strings.Contains(failures[0].First.Error(), message) {
						t.Errorf("%s at %s with the decoder %q under %s: failures %+v", label, expression, decoder, mode, failures)
					}
				}
			}
			// A rule that selects nothing has no series to leave the
			// label off, and does not fail for it.
			c = xpathLabelCollector(decoder, "//absent", label, nil)
			optional := false
			c.Metrics[0].Required = &optional
			if set, err := runBody(t, c, "application/xml", answer); err != nil || set == nil || len(set.Metrics) != 0 {
				t.Errorf("%s at a rule that selects nothing: %+v, %v", label, set, err)
			}
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
