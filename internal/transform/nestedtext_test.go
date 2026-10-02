package transform

import (
	"context"
	"strings"
	"testing"

	"github.com/PuerkitoBio/goquery"
	"github.com/antchfx/xmlquery"
	"github.com/eenchev/prometheus-universal-exporter/internal/decode"
	"github.com/eenchev/prometheus-universal-exporter/internal/expr"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// An XPath that selects nodes nested in one another reads what is beneath
// them once, not once for every selected node around it. //a over elements
// nested one in the next asked the document for the text of each, and each
// answer walked everything beneath that element: the square of the depth,
// 39 seconds for a response of 683 KiB.
func TestNestedSelectedNodesAreReadOnce(t *testing.T) {
	const depth = 400
	root, err := decode.ParseXML([]byte(strings.Repeat("<a>", depth) + "7" + strings.Repeat("</a>", depth)))
	if err != nil {
		t.Fatal(err)
	}
	// The document is asked for a node's text, and for the children of a
	// node, through counted copies of what the transform uses.
	nodes := xmlNodes
	var asked, walked int
	nodes.text = func(node *xmlquery.Node) string {
		asked++
		return xmlNodes.text(node)
	}
	nodes.first = func(node *xmlquery.Node) (*xmlquery.Node, bool) {
		walked++
		return xmlNodes.first(node)
	}
	c := model.Collector{Name: "nested", Limits: model.Limits{MaxMetrics: depth}}
	rules := []model.MetricRule{{Name: "v", Expression: "//a", ErrorMode: model.ErrorModeFail}}
	set, err := transformXPathNodes(withSeriesBudget(context.Background(), depth), root, nodes, rules, &c, nil)
	if err != nil || len(set.Metrics) != depth {
		t.Fatalf("%d series, %v", len(set.Metrics), err)
	}
	for _, m := range set.Metrics {
		if m.Value != 7 {
			t.Fatalf("a series reads %v, want 7", m.Value)
		}
	}
	// One text is asked for before the nesting shows; from then on one walk
	// reads the rest, visiting each element a time or two.
	if asked > 1 || walked > 3*depth {
		t.Fatalf("the document was asked for %d texts and the children of %d nodes for %d nested elements", asked, walked, depth)
	}
}

// Read that way, every selected node has the text the document gives for it
// alone, whatever the selection: nested or not, in document order or against
// it, with text nodes, attributes, comments and CDATA among it.
func TestNestedSelectedNodesReadTheSameText(t *testing.T) {
	const xmlBody = `<?xml version="1.0"?>
<r id="top">head<!-- not text -->
  <s n="1">one<s n="2"> two <![CDATA[<raw>]]><s n="3">three</s><t>tail</t></s>after</s>
  <s n="4"><?pi skip?>four<u><s n="5">five</s></u></s>
  <empty/>
</r>`
	root, err := decode.ParseXML([]byte(xmlBody))
	if err != nil {
		t.Fatal(err)
	}
	for _, expression := range []string{
		"//s", "//*", "//s | //t | //u", "//s/@n | //s", "//s//text() | //s", "//node()", "//s[@n='3']/ancestor-or-self::*",
		"//s[@n='5']/ancestor::* | //s", "//t/preceding::* | //t", "//s[@n='2'] | //s[@n='2']/..", "/r | //empty | //s",
	} {
		sameTexts(t, "XML "+expression, xmlNodes, root, expression)
	}
	// In document order the nesting is found, and the texts are parts of
	// one walk; against it each node is read on its own, as before.
	if !sameTexts(t, "XML //s", xmlNodes, root, "//s") || sameTexts(t, "XML ancestors", xmlNodes, root, "//s[@n='3']/ancestor::*") {
		t.Fatal("nesting should be found in document order, and only there")
	}

	const htmlBody = `<html><body><div id="a">one<div id="b">two<!-- no --><span>three</span><div id="c">four</div></div>five</div><p>six</p><script>var x = 1;</script></body></html>`
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(htmlBody))
	if err != nil {
		t.Fatal(err)
	}
	for _, expression := range []string{"//div", "//*", "//div | //span", "//div/@id | //div", "//body//text() | //div", "//span/ancestor::*"} {
		sameTexts(t, "HTML "+expression, htmlNodes, doc.Nodes[0], expression)
	}
}

// sameTexts fails unless selectedTexts gives each node expression selects
// under root the text nodes.text gives for it, asked in the selection's
// order. It reports whether the selection was found to nest.
func sameTexts[N comparable](t *testing.T, name string, nodes xpathNodes[N], root N, expression string) (nested bool) {
	t.Helper()
	program, err := expr.CompileXPath(expression, nil)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	compiled := program.Get()
	defer program.Put(compiled)
	selected := nodes.all(root, compiled)
	if len(selected) < 2 {
		t.Fatalf("%s selected %d nodes", name, len(selected))
	}
	texts := selectedTexts[N]{nodes: nodes, selected: selected}
	for i, node := range selected {
		if got, want := texts.text(i), nodes.text(node); got != want {
			t.Errorf("%s: node %d reads %q, want %q", name, i, got, want)
		}
	}
	return texts.index != nil
}

// XML a pre-script leaves is parsed again, and held to the decoder's bound on
// nesting: a script cannot hand the rules a document the decoder would have
// refused.
func TestAPreScriptsXMLIsHeldToTheNestingBound(t *testing.T) {
	requirePython(t)
	c := model.Collector{Name: "deep", Decoder: model.DecoderConfig{Type: "xml"}, Limits: scriptLimits(),
		Transform: model.TransformConfig{Type: "xpath", PreScript: `data = "<a>" * 600 + "1" + "</a>" * 600`},
		Metrics:   []model.MetricRule{{Name: "v", Expression: "//a"}}}
	_, err := runBody(t, c, "application/xml", "<a>1</a>")
	if err == nil || !strings.Contains(err.Error(), "XML pre-script output: the document nests elements more than 512 deep") {
		t.Fatalf("err %v", err)
	}
	c.Transform.PreScript = `data = "<r>" + "<a>2</a>" * 3 + "</r>"`
	if set, err := runBody(t, c, "application/xml", "<a>1</a>"); err != nil || len(set.Metrics) != 3 {
		t.Fatalf("a flat document: %+v, %v", set, err)
	}
}
