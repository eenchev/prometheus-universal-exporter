package transform

import (
	"context"
	"fmt"
	"math/rand/v2"
	"reflect"
	"strings"
	"testing"

	"github.com/PuerkitoBio/goquery"
	"github.com/antchfx/xpath"
	"github.com/eenchev/prometheus-universal-exporter/internal/decode"
	"github.com/eenchev/prometheus-universal-exporter/internal/expr"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// engineXPathLabels is xpathLabels as it was before labels were planned and
// the commonest read by walking the tree: every label that is not static or
// a plain attribute compiled, evaluated by the engine at the node, and the
// first node it selects read with nodes.text. It is the oracle the walks
// are held to. Two things it reads as they are read now: the node's own
// attribute without the blanks around it, and an absolute path from the
// document.
func engineXPathLabels[N comparable](nodes xpathNodes[N], node N, rule model.MetricRule, namespaces map[string]string) map[string]string {
	labels := map[string]string{}
	for _, label := range rule.Labels {
		switch {
		case label.Static():
			labels[label.Name] = label.Value
		case strings.HasPrefix(label.Expression, "@") && plainXPathName(label.Expression[1:]):
			labels[label.Name] = strings.TrimSpace(nodes.attr(node, strings.TrimPrefix(label.Expression, "@")))
		default:
			// A prefixed attribute read by its name as written, as it is
			// in XML without response.namespaces, is not the engine's to
			// read: by name it is the value, and empty where there is
			// none, as a plain one is.
			// TestAPrefixedAttributeByNameIsTheEnginesAtAnElement holds
			// the two together where they meet.
			if name, byName := ownAttributeLabel(label.Expression, nodes.html, namespaces); byName {
				labels[label.Name] = strings.TrimSpace(nodes.attr(node, name))
				continue
			}
			program, err := expr.CompileXPath(label.Expression, namespaces)
			if err != nil {
				continue
			}
			// An expression that is one absolute path gives at every node
			// what it gives at the document, where the library's navigator
			// has the document for its root: the engine is asked there.
			if absoluteXPathLabels[label.Expression] {
				for parent, ok := nodes.parent(node); ok; parent, ok = nodes.parent(node) {
					node = parent
				}
			}
			selector := program.Get()
			if value, computed := xpathValue(nodes, node, selector); computed {
				if text := strings.TrimSpace(xpathText(value)); text != "" {
					labels[label.Name] = text
				}
			} else if found, ok := nodes.one(node, selector); ok {
				labels[label.Name] = strings.TrimSpace(nodes.text(found))
			}
			program.Put(selector)
		}
	}
	return labels
}

// xpathLabelDocuments are XML documents made to tell a walk of the tree from
// the engine: several children of one name, a name that is also a node test
// or an operator, comments, CDATA, a processing instruction named like an
// element, text around and between elements, blank text, attributes with
// and without a value, and, in the second and third, a default namespace,
// one that changes on the way down, and elements and attributes with a
// prefix beside ones of the same local name without.
var xpathLabelDocuments = []string{
	`<?xml version="1.0"?>
<!-- top -->
<!DOCTYPE items>
<items site="a" id="root">
  <item id="item-1" kind=" spaced " data-id="d1">
    <region>eu-1</region>
    <value>1</value>
    <region>second</region>
    <!-- a comment --><name><![CDATA[c<d>ata]]></name>
    <empty/>
    <mixed>before<b>bold</b>after</mixed>
    <text>named text</text>
    <?region pi-data?>
  </item>
  <item><value>2</value><?value pi first?><value>after pi</value></item>
  text directly<item id=""><value> 3 </value><region><!--only comment--></region></item>
  <div>4</div><mod>5</mod><and>6</and><or>7</or><node>8</node><comment>9</comment>
  <a.b data-id="x">dotted</a.b>
  <item id="cdata-first"><![CDATA[ raw ]]><value>5</value>tail</item>
  <item id="blank-first">   <value>6</value>later text</item>
  <item id="element-first"><value>7</value>   <region>r</region>text after blanks</item>
</items>`,
	`<items xmlns="urn:default" xmlns:x="urn:x" id="root" x:id="xroot">
  <item id="1" x:id="x1"><x:region>prefixed</x:region><region>plain</region><value>1</value></item>
  <item x:id="x2"><x:region>only prefixed</x:region><value>2</value></item>
  <group xmlns="urn:other" id="g"><item id="3"><region>other namespace</region><value>3</value></item></group>
  <y:item xmlns:y="urn:y" y:id="y4" id="4"><y:value>4</y:value><value>4b</value><region>in y</region></y:item>
  <x:item id="5"><value>5</value><x:value>5x</x:value></x:item>
</items>`,
	`<root xmlns:x="urn:x"><x:items x:site="s" site="plain"><x:item x:id="a" id="b"><x:value>1</x:value><x:region>r</x:region></x:item><item><value>2</value><region/></item></x:items></root>`,
}

// xpathLabelPages are HTML documents with the same in view: the parser adds
// html, head and body, a comment is no text, and an attribute of an svg
// element has a namespace the navigator knows nothing of.
var xpathLabelPages = []string{
	`<!DOCTYPE html><html lang="en"><head><title>t</title></head><body class="b" id="body">
<!-- c --><table id="t1"><tr class="item" id="r1"><td class="id">item-1</td><td class="value">1</td><td>extra</td></tr>
<tr id="r2"><td>  padded  <b>bold</b> tail</td></tr></table>
<svg id="s"><a xlink:href="#x" id="sa"><text>svg text</text></a></svg>
<p id="p1">para<span id="s1">span</span><!-- inner --><span>second</span></p>
<ul><li data-id="7">one</li><li>two</li></ul><region>custom</region><value>v</value><item id="i"><region>r</region><value>1</value></item></body></html>`,
	`<ul><li id="a"><!-- first --><b>1</b> one</li><li><span><b>2</b></span></li></ul><div id="d"><div id="inner"><value> 3 </value></div></div>`,
}

// xpathLabelNames are the element and attribute names the expressions of
// the test read, those of the documents and one none has.
var xpathLabelNames = []string{"id", "kind", "site", "data-id", "lang", "class", "href", "xmlns", "region", "value", "item", "name", "text", "div", "mod", "and", "or", "node", "comment", "a.b", "td", "span", "b", "li", "missing"}

// xpathWalkedExpressions are the label expressions read by walking the
// tree, every shape of them over every name.
func xpathWalkedExpressions() []string {
	out := []string{".", "text()"}
	for _, name := range xpathLabelNames {
		out = append(out, name, "./"+name, "../"+name, "../../"+name, "../../../"+name, "../@"+name, "../../@"+name, "../../../@"+name)
	}
	return out
}

// xpathEngineExpressions are near misses: expressions like those walked that
// the engine must go on reading, a blank, a prefix, a predicate, an axis by
// name or a second step apart.
var xpathEngineExpressions = []string{
	"@id", "@data-id", "@x:id", "../@x:id", "../../@y:id", "x:region", "../x:region", "./x:region", "x:value", "y:value", "../y:value",
	" ../@id", "../@id ", "../ @id", ".. /@id", "../@ id", " region", "region ", ". /region", "./ region", "text( )", " .",
	"./@id", "./text()", "../text()", "..", "../..", "../.", "./.", "./../region", "./../@id", ".././region",
	"*", "../*", "@*", "../@*", "node()", "comment()", "processing-instruction()", "../node()",
	"region[1]", "region[2]", "region[last()]", "../region[1]", "../@id[1]", "value[. > 1]",
	"region/text()", "value/text()", "mixed/b", "../mixed/b", "item/value", "../item/value", "//region", "//@id", "/items/@id", "/items/item/region",
	"child::region", "self::node()", "parent::*/@id", "parent::node()/region", "attribute::id", "following-sibling::region", "preceding-sibling::value", "ancestor::item/@id",
	"string(region)", "string(../@id)", "normalize-space(.)", "normalize-space(text())", "count(region)", "name()", "local-name(..)", "concat(../@id, '-', region)", "region = 'eu-1'",
	"region | value", "../@id | @id", "td[2]", "../td[1]", "span[2]", "li[@data-id]", "b/..",
}

// absoluteXPathLabels are the expressions of xpathEngineExpressions that are
// one absolute path and nothing else, which the oracle evaluates at the
// document (engineXPathLabels).
var absoluteXPathLabels = map[string]bool{"//region": true, "//@id": true, "/items/@id": true, "/items/item/region": true}

// treeNodes are every node of a document, the root included, and the nodes
// the engine makes of its attributes, which a rule that selects attributes
// reads its labels at.
func treeNodes[N comparable](t *testing.T, nodes xpathNodes[N], root N) []N {
	t.Helper()
	var all []N
	var walk func(node N)
	walk = func(node N) {
		all = append(all, node)
		for child, ok := nodes.first(node); ok; child, ok = nodes.next(child) {
			walk(child)
		}
	}
	walk(root)
	attributes, err := xpath.Compile("//@*")
	if err != nil {
		t.Fatal(err)
	}
	return append(all, nodes.all(root, attributes)...)
}

// labelsOrPanic is the labels read gives, or what it panicked with: the
// engine's navigator panics at a node it cannot name the type of, as the
// node it makes of an XML attribute is, for the expressions that ask.
func labelsOrPanic(read func() map[string]string) (labels map[string]string, panicked string) {
	defer func() {
		if r := recover(); r != nil {
			labels, panicked = nil, fmt.Sprint(r)
		}
	}()
	return read(), ""
}

// agreeAtEveryNode holds the planned labels to the engine's, at every node
// of a document and for every expression, each as a rule's one label, and
// nodeText to nodes.text; it returns how many labels it compared.
func agreeAtEveryNode[N comparable](t *testing.T, nodes xpathNodes[N], root N, namespaces map[string]string, expressions []string, document string) int {
	t.Helper()
	compared := 0
	all := treeNodes(t, nodes, root)
	for at, node := range all {
		if got, want := nodeText(nodes, node), nodes.text(node); got != want {
			t.Fatalf("node %d of %s: nodeText %q, the node's text %q", at, document, got, want)
		}
	}
	for _, expression := range expressions {
		rule := model.MetricRule{Name: "m", Labels: []model.LabelRule{{Name: "l", Expression: expression}}}
		plan := planXPathLabels(rule, namespaces, nodes.html)
		for at, node := range all {
			want, wantPanic := labelsOrPanic(func() map[string]string { return engineXPathLabels(nodes, node, rule, namespaces) })
			// One label is not the engine's: text() at an attribute a rule
			// selected is the attribute's value, where the engine, as
			// XPath has it, selects nothing (xpathLabels).
			if value, attribute := nodes.selectedAttribute(node); attribute && expression == "text()" {
				want = map[string]string{"l": strings.TrimSpace(value)}
			}
			got, gotPanic := labelsOrPanic(func() map[string]string { return xpathLabels(nodes, node, plan) })
			if !reflect.DeepEqual(got, want) || gotPanic != wantPanic {
				t.Fatalf("label %q at node %d of %s (namespaces %v): %q (panic %q), the engine gives %q (panic %q)", expression, at, document, namespaces, got, gotPanic, want, wantPanic)
			}
			compared++
		}
		releaseXPathLabels(plan)
	}
	return compared
}

// randomXML is a document of random elements, attributes, text, blank text,
// comments, CDATA and processing instructions, with and without prefixes
// and with default namespaces that come and go, from the names the test's
// expressions read.
func randomXML(r *rand.Rand) string {
	names := []string{"item", "region", "value", "name", "text", "x:region", "x:value", "x:item", "group"}
	attributes := []string{"id", "kind", "site", "x:id", "data-id"}
	var b strings.Builder
	var element func(depth int)
	element = func(depth int) {
		name := names[r.IntN(len(names))]
		b.WriteString("<")
		b.WriteString(name)
		if depth == 0 {
			b.WriteString(` xmlns:x="urn:x"`)
		}
		if r.IntN(6) == 0 {
			fmt.Fprintf(&b, ` xmlns="urn:d%d"`, r.IntN(2))
		}
		for _, attribute := range attributes {
			if r.IntN(3) == 0 {
				fmt.Fprintf(&b, ` %s="%s"`, attribute, []string{"", "v", " padded ", "1"}[r.IntN(4)])
			}
		}
		b.WriteString(">")
		for range r.IntN(5) {
			switch r.IntN(9) {
			case 0:
				b.WriteString("text")
			case 1:
				b.WriteString("  \n ")
			case 2:
				b.WriteString("<!-- comment -->")
			case 3:
				b.WriteString("<![CDATA[ c<d ]]>")
			case 4:
				b.WriteString("<?region data?>")
			default:
				if depth < 4 {
					element(depth + 1)
				} else {
					fmt.Fprintf(&b, " %d ", r.IntN(100))
				}
			}
		}
		b.WriteString("</")
		b.WriteString(name)
		b.WriteString(">")
	}
	element(0)
	return b.String()
}

// A label read by walking the tree has the value the XPath engine gives it,
// at every node of every document: the labels are compared as a rule would
// get them, absent, empty or trimmed alike, for every shape that is walked
// and for near misses that are not, over XML with and without namespaces
// and response.namespaces, random XML, and HTML, at elements, text,
// comments, processing instructions, the document itself and the nodes made
// of attributes. The shapes listed as walked are walked, and the near
// misses are not, so neither half of the comparison is the engine against
// itself.
func TestXPathLabelFastPathsAgreeWithTheEngine(t *testing.T) {
	walked := xpathWalkedExpressions()
	for _, expression := range walked {
		if _, _, _, ok := xpathLabelShape(expression, false); !ok {
			t.Errorf("%q is not read by walking the tree", expression)
		}
	}
	for _, expression := range xpathEngineExpressions {
		if _, _, _, ok := xpathLabelShape(expression, false); ok {
			t.Errorf("%q is read by walking the tree", expression)
		}
	}
	expressions := append(append([]string{}, walked...), xpathEngineExpressions...)
	// response.namespaces: none, the documents' own, and bindings that give
	// the prefixes other namespaces and the empty prefix one, none of which
	// changes what a name without a prefix matches.
	bindings := []map[string]string{nil, {"x": "urn:x", "y": "urn:y"}, {"x": "urn:other", "y": "urn:default", "": "urn:default"}}
	compared := 0
	documents := append([]string{}, xpathLabelDocuments...)
	random := rand.New(rand.NewPCG(20261002, 4))
	for range 12 {
		documents = append(documents, randomXML(random))
	}
	for i, document := range documents {
		root, err := decode.ParseXML([]byte(document))
		if err != nil {
			t.Fatalf("document %d: %v\n%s", i, err, document)
		}
		for _, namespaces := range bindings {
			compared += agreeAtEveryNode(t, xmlNodes, root, namespaces, expressions, fmt.Sprintf("XML document %d", i))
		}
	}
	for i, page := range xpathLabelPages {
		doc, err := goquery.NewDocumentFromReader(strings.NewReader(page))
		if err != nil {
			t.Fatal(err)
		}
		compared += agreeAtEveryNode(t, htmlNodes, doc.Nodes[0], nil, expressions, fmt.Sprintf("HTML document %d", i))
	}
	t.Logf("%d labels compared, of %d expressions over %d documents", compared, len(expressions), len(documents)+len(xpathLabelPages))
}

// The labels a transform gives are the engine's too, through Transform, for
// a rule that mixes the kinds: a static label, the node's attribute, labels
// that are walked and ones the engine reads, with a required one among them.
func TestXPathRuleLabelsThroughTransform(t *testing.T) {
	c := model.Collector{Name: "items", Decoder: model.DecoderConfig{Type: "xml"}, Transform: model.TransformConfig{Type: "xpath"},
		Metrics: []model.MetricRule{{Name: "item_value", Type: model.GaugeMetricType, Expression: "//item/value", ErrorMode: model.ErrorModeIgnore, Labels: []model.LabelRule{
			{Name: "site", Value: "static"},
			{Name: "id", Expression: "../@id", Required: true},
			{Name: "root", Expression: "../../@id"},
			{Name: "region", Expression: "../region"},
			{Name: "own", Expression: "."},
			{Name: "first", Expression: "string(../region[1])"},
			{Name: "kind", Expression: "../@kind"},
		}}}}
	set, failures, err := transformWith(context.Background(), t, c, "application/xml", xpathLabelDocuments[0])
	if err != nil {
		t.Fatal(err)
	}
	// The values of the items without an id, and with an empty one, are
	// left out, the label being required, as are the processing
	// instruction the engine reads as an element named value, which has no
	// text, and the value that is no number.
	if len(failures) != 1 || failures[0].Failures != 4 || failures[0].Missing != 3 {
		t.Fatalf("failures %+v", failures)
	}
	want := []map[string]string{
		{"site": "static", "id": "item-1", "root": "root", "region": "eu-1", "own": "1", "first": "eu-1", "kind": "spaced"},
		{"site": "static", "id": "cdata-first", "root": "root", "own": "5"},
		{"site": "static", "id": "blank-first", "root": "root", "own": "6"},
		{"site": "static", "id": "element-first", "root": "root", "region": "r", "own": "7", "first": "r"},
	}
	if len(set.Metrics) != len(want) {
		t.Fatalf("%d series: %+v", len(set.Metrics), set.Metrics)
	}
	for i, labels := range want {
		if !reflect.DeepEqual(set.Metrics[i].Labels, labels) {
			t.Errorf("series %d has the labels %v, want %v", i, set.Metrics[i].Labels, labels)
		}
	}
}
