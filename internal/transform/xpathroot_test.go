package transform

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/antchfx/xmlquery"
	"github.com/antchfx/xpath"
	"github.com/eenchev/prometheus-universal-exporter/internal/decode"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
)

// rootedMarkup is a document whose jobs are told apart by what stands
// outside them: the site on the top element, the queue around them, and the
// x beside them that refers to the y the document names once. It is read as
// XML and, inside the body the parser puts around it, as HTML.
const rootedMarkup = `<status site="fra1" load="3">
  <y id="b"></y>
  <queue name="mail">
    <x ref="a">first</x>
    <x ref="b">second</x>
    <job id="7">12</job>
    <job id="8">30</job>
  </queue>
</status>`

// An absolute path in a label starts at the document, whatever node the
// rule selected: an element, a text node or an attribute, over XML and over
// HTML, in a label that selects a node and in one that computes a value, at
// the start of the expression, in an argument, in a predicate and in a
// union. The engine's navigators take the node they were made at for their
// root, so each such label found nothing and was left off every series
// without a word, where ancestor::status/@site gave it.
func TestAnAbsolutePathInALabelStartsAtTheDocument(t *testing.T) {
	for decoder, top := range map[string]string{"xml": "/status", "html": "/html/body/status"} {
		for _, selected := range []struct {
			name, rule string
			// own is the job's id as the label reads it from the selected
			// node, and up the way from that node to the job's queue.
			own, up string
			values  [2]string
		}{
			{"elements", "//job", "@id", "..", [2]string{"12", "30"}},
			{"text nodes", "//job/text()", "../@id", "../..", [2]string{"12", "30"}},
			{"attributes", "//job/@id", ".", "../..", [2]string{"7", "8"}},
		} {
			rule := model.MetricRule{Name: "m", Type: model.GaugeMetricType, Expression: selected.rule, ErrorMode: model.ErrorModeFail, Labels: []model.LabelRule{
				{Name: "anywhere", Expression: "//status/@site"},
				{Name: "from_the_top", Expression: top + "/@site"},
				{Name: "blanks_before", Expression: " " + top + "/queue/@name"},
				{Name: "an_element", Expression: "//x"},
				{Name: "in_an_argument", Expression: "concat(" + top + "/@site, '-', " + selected.own + ")"},
				{Name: "in_a_predicate", Expression: selected.up + "/x[@ref=//y/@id]"},
				{Name: "in_a_union", Expression: "@nothing | //queue/@name"},
				{Name: "in_a_group", Expression: "(//job)[2]/@id"},
				{Name: "counted", Expression: "count(//job)"},
				{Name: "added_to", Expression: top + "/@load + 1"},
				{Name: "compared", Expression: selected.own + " = //job[1]/@id"},
				{Name: "by_an_ancestor", Expression: "ancestor::status/@site"},
			}}
			c := xpathRuleCollector(decoder, rule)
			if err := CheckMetricRule(&c, &c.Metrics[0]); err != nil {
				t.Fatalf("%s, %s: the rule is refused at load: %v", decoder, selected.name, err)
			}
			set, failures, err := transformWith(context.Background(), t, c, markupContentType[decoder], rootedMarkup)
			if err != nil || len(failures) != 0 {
				t.Fatalf("%s, %s: %v, failures %+v", decoder, selected.name, err, failures)
			}
			var want []string
			for i, id := range []string{"7", "8"} {
				want = append(want, fmt.Sprintf(`m{added_to="4",an_element="first",anywhere="fra1",blanks_before="mail",by_an_ancestor="fra1",compared="%t",counted="2",from_the_top="fra1",in_a_group="8",in_a_predicate="second",in_a_union="mail",in_an_argument="fra1-%s"} %s`, i == 0, id, selected.values[i]))
			}
			if got := htmlSeries(set); !reflect.DeepEqual(got, want) {
				t.Errorf("%s, a rule that selects %s, made\n%s\nwant\n%s", decoder, selected.name, strings.Join(got, "\n"), strings.Join(want, "\n"))
			}
		}
		// A rule that computes a value reads its labels at the document,
		// where an absolute path always started.
		computed := xpathRuleCollector(decoder, model.MetricRule{Name: "jobs", Type: model.GaugeMetricType, Expression: "count(//job)", Labels: []model.LabelRule{{Name: "site", Expression: top + "/@site"}, {Name: "queue", Expression: "//queue/@name"}}})
		set, err := runBody(t, computed, markupContentType[decoder], rootedMarkup)
		if got, want := htmlSeries(set), []string{`jobs{queue="mail",site="fra1"} 2`}; err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("%s, a rule that computes a value, made %q, %v, want %q", decoder, got, err, want)
		}
	}
}

// Two slashes at the start of a label are the document's, and the same for
// every series: what stands beneath the selected node is asked for with a
// dot before them, or by its name. While the selected node was the root of
// its label's expression, //name and /name read beneath it as .//name and
// name do.
func TestTwoSlashesInALabelAreTheDocumentsAndNotTheNodes(t *testing.T) {
	const body = `<r><n>top</n><s id="a"><v>1</v><d><n>one</n></d></s><s id="b"><v>2</v><n>two</n></s></r>`
	for decoder, top := range map[string]string{"xml": "", "html": "/html/body"} {
		c := xpathRuleCollector(decoder, model.MetricRule{Name: "m", Type: model.GaugeMetricType, Expression: "//s/v", Labels: []model.LabelRule{
			{Name: "documents", Expression: "//n"},
			{Name: "from_the_top", Expression: top + "/r/s/n"},
			{Name: "no_child_of_the_top", Expression: "/n"},
			{Name: "beneath", Expression: "..//n"},
			{Name: "child", Expression: "../n"},
		}})
		set, err := runBody(t, c, markupContentType[decoder], body)
		want := []string{
			`m{beneath="one",documents="top",from_the_top="two"} 1`,
			`m{beneath="two",child="two",documents="top",from_the_top="two"} 2`,
		}
		if got := htmlSeries(set); err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("%s made\n%s\n%v, want\n%s", decoder, strings.Join(got, "\n"), err, strings.Join(want, "\n"))
		}
	}
}

// With response.namespaces an absolute path in a label matches by the
// namespace its prefix is bound to, as a relative one does.
func TestAnAbsolutePathInALabelMatchesByNamespace(t *testing.T) {
	const feed = `<s:status xmlns:s="urn:status" site="fra1"><s:job id="7">12</s:job><job id="8">30</job></s:status>`
	c := xpathRuleCollector("xml", model.MetricRule{Name: "m", Type: model.GaugeMetricType, Expression: "//n:job", Labels: []model.LabelRule{
		{Name: "site", Expression: "/n:status/@site"},
		{Name: "other", Expression: "//job/@id"},
		{Name: "unbound", Expression: "/status/@site"},
	}})
	c.Response.Namespaces = map[string]string{"n": "urn:status"}
	set, err := runBody(t, c, "application/xml", feed)
	if got, want := htmlSeries(set), []string{`m{other="8",site="fra1"} 12`}; err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("made %q, %v, want %q", got, err, want)
	}
}

// rootRecorder is xmlquery's navigator that notes when the engine asks it
// for its root, and its copies with it: the engine's own word on whether an
// expression has an absolute path that it reached.
type rootRecorder struct {
	xmlquery.NodeNavigator
	reached *bool
}

func (r *rootRecorder) MoveToRoot() {
	*r.reached = true
	r.NodeNavigator.MoveToRoot()
}

func (r *rootRecorder) Copy() xpath.NodeNavigator {
	other := *r
	return &other
}

func (r *rootRecorder) MoveTo(other xpath.NodeNavigator) bool {
	to, ok := other.(*rootRecorder)
	if ok {
		r.NodeNavigator = to.NodeNavigator
	}
	return ok
}

// engineReachesRoot reports whether the engine goes to the root while it
// evaluates expression at any node of a document, every node it selects
// read; compiled is false for an expression it does not compile.
func engineReachesRoot(t *testing.T, root *xmlquery.Node, expression string) (reached, compiled bool) {
	t.Helper()
	e, err := xpath.Compile(expression)
	if err != nil {
		return false, false
	}
	for _, node := range treeNodes(t, xmlNodes, root) {
		func() {
			// What an expression gives is not asked here, and neither is
			// whether the engine can evaluate it at every kind of node.
			defer func() { _ = recover() }()
			if selected, nodes := e.Evaluate(&rootRecorder{*xmlNavigator(node).(*xmlquery.NodeNavigator), &reached}).(*xpath.NodeIterator); nodes {
				for selected.MoveNext() {
				}
			}
		}()
	}
	return reached, true
}

// Whether a label may reach the root is told from its expression as the
// engine's parser reads it: a slash where an operand is expected starts an
// absolute path, and one after a name, a closing bracket, a dot, a number
// or a string goes on with the path before it, blanks between them or not.
// The names and, or, div and mod and a `*` are operators after an operand
// and names elsewhere, and a slash inside a string is text. Each verdict is
// the engine's own: evaluated at every node of a document, an expression
// said to reach the root asks its navigator for it and one said not to
// never does, so no label is evaluated from the wrong navigator. Under the
// race detector the expressions strung together at random are 5,000 of the
// 20,000, of which a quarter as many are to compile and to reach the root.
func TestWhetherALabelReachesTheRootIsToldFromItsExpression(t *testing.T) {
	type verdict struct {
		expression string
		rooted     bool
	}
	table := []verdict{
		{"/", true}, {"/status", true}, {"//job", true}, {"/status/@site", true}, {"//status/@site", true}, {" /status", true}, {"\t//job", true}, {"\n/status", true}, {"\u00a0/status", true},
		{"concat(/status/@site, '-', @id)", true}, {"concat(@id,'-',//status/@site)", true}, {"../x[@ref=//y/@id]", true}, {"../x[@ref = /status/y/@id]", true},
		{"count(//job)", true}, {"string(/status/@load)", true}, {"(//job)[2]/@id", true}, {"(/status)/@site", true}, {"*[/status]", true}, {"self::job[//y]", true}, {"ancestor::status[/status]/@site", true},
		{"@id | /status/@site", true}, {"@id|//status/@site", true}, {"job[1]|/status", true}, {"-/status/@load", true}, {"- /status/@load", true},
		{"1 + /status/@load", true}, {"1+/status/@load", true}, {"/status/@load + 1", true}, {"1 - /status/@load", true}, {"@load -/status/@load", true},
		{"@id = /status/queue/job/@id", true}, {"@id=//job/@id", true}, {"@id != /status/@site", true}, {"@id!=//y/@id", true},
		{"@id < /status/@load", true}, {"@id</status/@load", true}, {"@id <= /status/@load", true}, {"@id<=//status/@load", true},
		{"@id > /status/@load", true}, {"@id>/status/@load", true}, {"@id >= /status/@load", true}, {"@id>=//status/@load", true},
		{"true() and /status", true}, {"true() and/status", true}, {"false() or //job", true}, {"false() or//job", true}, {"true()\u00a0and /status", true},
		{"6 div /status/@load", true}, {"6 div/status/@load", true}, {"7 mod /status/@load", true}, {"7 mod/status/@load", true},
		{"2 * /status/@load", true}, {"2*/status/@load", true}, {"@id*/status/@load", true}, {"@id * //status/@load", true},
		{"div div /status/@load", true}, {"div div/status/@load", true}, {"mod mod /status/@load", true}, {"* * /status/@load", true}, {"* */status/@load", true},
		{"and and /status", true}, {"or or//job", true}, {"and and and and /status", true},
		{"'a' = /status/@site", true}, {"\"fra1\"=/status/@site", true}, {"concat('/', /status/@site)", true}, {"translate(/status/@site, 'f', 'F')", true}, {"concat('a', 'b', /status/@site)", true}, {"starts-with(/status/@site,'f')", true},
		{"name(/status)", true}, {"not(/nothing)", true}, {"string-length(//job)", true}, {"child::job | /status", true}, {"x:job | /status", true}, {"x:* | //job", true},
		{"job[@id=/status/queue/job[1]/@id]", true}, {"../job[//y][1]/@id", true}, {".//job[/status]", true}, {"1.5 + /status/@load", true}, {".5 + /status/@load", true}, {"1. * /status/@load", true},

		{".", false}, {"..", false}, {"@id", false}, {"../@site", false}, {"ancestor::status/@site", false}, {"job/@id", false}, {"./job", false}, {"../job/@id", false}, {"../../@site", false},
		{"queue/job//text()", false}, {".//job", false}, {"..//job", false}, {"*/job", false}, {"*//job", false}, {"job/*", false}, {"*/*", false}, {"@*", false}, {"../*/@id", false},
		{"job /@id", false}, {"job / @id", false}, {"job\t//\ttext()", false}, {".. /job", false}, {". //job", false}, {"* /job", false}, {"job[1] /@id", false}, {"(job) /@id", false}, {"text() /..", false},
		{"div/span", false}, {"../div/span", false}, {"and/or", false}, {"or//and", false}, {"mod/div", false}, {"div//mod", false}, {"div /mod", false}, {"@div", false}, {"child::div/span", false}, {"x:div/span", false}, {"@x:div", false},
		{"text()/..", false}, {"node()//job", false}, {"job[1]/@id", false}, {"job[@id='7']/text()", false}, {"(job)/@id", false}, {"(job)//x", false}, {"(job | x)/@id", false}, {"job[x][y]/z", false},
		{"concat(@id, '/', ../@name)", false}, {"concat(@id, '//x')", false}, {"'/status'", false}, {"\"//job\"", false}, {"@id = '/'", false}, {"contains(., '/')", false}, {"substring-before(@href, \"/\")", false},
		{"child::job/attribute::id", false}, {"ancestor :: status/@site", false}, {"ancestor-or-self::*/@site", false}, {"self::node()/..", false}, {"following-sibling::job/@id", false},
		{"count(job) div 2", false}, {"@a div @b", false}, {"@a * @b", false}, {"@a*@b", false}, {"2 * 3", false}, {"1 div 2", false}, {"7 mod 2", false}, {".5", false}, {"1.5", false}, {"-1", false},
		{"job[last()]/../@name", false}, {"x:job/y:id", false}, {"x:*/job", false}, {"a.b/c-d", false}, {"a-/b", false}, {"a./b", false}, {"größe/x", false}, {"true() and job/@id", false}, {"job and and/or", false},
		{"normalize-space(.)", false}, {"string(../@id)", false}, {"name()", false}, {"@id = ../@id", false}, {"job | x", false}, {"", false},
	}
	// What stands before an absolute path, and what closes it: every
	// operator and opening bracket, with a blank before the slash and
	// without, and after each a path of either kind.
	for _, around := range [][2]string{
		{"", ""}, {"(", ")"}, {"@id = ", ""}, {"@id=", ""}, {"@id != ", ""}, {"@id!=", ""}, {"@id < ", ""}, {"@id<", ""}, {"@id <= ", ""}, {"@id<=", ""},
		{"@id > ", ""}, {"@id>", ""}, {"@id >= ", ""}, {"@id>=", ""}, {"1 + ", ""}, {"1+", ""}, {"1 - ", ""}, {"1 -", ""}, {"- ", ""}, {"-", ""},
		{"2 * ", ""}, {"2*", ""}, {"2 div ", ""}, {"2 div", ""}, {"2 mod ", ""}, {"2 mod", ""}, {"true() and ", ""}, {"true() and", ""}, {"false() or ", ""}, {"false() or", ""},
		{"job | ", ""}, {"job|", ""}, {"concat('a', ", ")"}, {"concat('a',", ")"}, {"ancestor-or-self::*[", "]"}, {"ancestor-or-self::*[ ", " ]"}, {"div div ", ""}, {"* * ", ""}, {"or or ", ""},
	} {
		for _, path := range []string{"/status/@load", "//status/@load"} {
			table = append(table, verdict{around[0] + path + around[1], true})
		}
	}
	// What a slash after it is a step of: every end of a step or of an
	// operand.
	for _, before := range []string{"job", "*", "..", ".", "job[1]", "(job)", "text()", "node()", "div", "and", "or", "mod", "child::div", "x:div", "x:*", "a.b", "a-b", "ancestor::*", "queue/job", "@id | job"} {
		for _, path := range []string{"/x", "//x", " /x", " // x"} {
			table = append(table, verdict{before + path, false})
		}
	}
	root, err := decode.ParseXML([]byte(`<status site="fra1" load="3"><y id="b"/><div>6</div><and>1</and><or>1</or><mod>4</mod><queue name="mail"><x ref="b">second</x><job id="7" load="2"><x>1</x>12</job></queue></status>`))
	if err != nil {
		t.Fatal(err)
	}
	compiled := 0
	for _, tc := range table {
		if got := xpathReachesRoot(tc.expression); got != tc.rooted {
			t.Errorf("%q is said to reach the root: %t, want %t", tc.expression, got, tc.rooted)
		}
		reached, ok := engineReachesRoot(t, root, tc.expression)
		if !ok {
			continue
		}
		compiled++
		if reached != tc.rooted {
			t.Errorf("%q: the engine went to the root: %t, and the expression is listed as %t", tc.expression, reached, tc.rooted)
		}
	}
	if compiled < len(table)-8 {
		t.Errorf("the engine compiled %d of %d expressions", compiled, len(table))
	}
	t.Logf("%d expressions listed, %d of them compiled by the engine", len(table), compiled)
	// Where the two could differ the answer is yes, which costs time and
	// no label: the parser takes the first byte of a blank outside ASCII
	// into the name before it, so that it reads no operator here and never
	// comes to the path.
	if expression := "@id\u00a0and\u00a0/status"; !xpathReachesRoot(expression) {
		t.Errorf("%q is said not to reach the root", expression)
	} else if reached, ok := engineReachesRoot(t, root, expression); reached || !ok {
		t.Errorf("%q: the engine compiled it: %t, and went to the root: %t", expression, ok, reached)
	}
	// Whatever the expression, one the engine takes to the root is said to
	// reach it: operands, operators and brackets strung together at random.
	pieces := []string{"/", "//", "/", " ", " ", "job", "x", "*", "div", "and", "or", "mod", "@id", "@", ".", "..", "(", ")", "[", "]", "|", "=", "+", "-", "<", ",", "1", "'/'", "status", "text()", "count(", "::", "ancestor::", "x:"}
	random := rand.New(rand.NewPCG(20261003, 11))
	compiled, rooted := 0, 0
	made := alloctest.UnlessRaced(20000, 5000)
	for range made {
		var expression strings.Builder
		for range 1 + random.IntN(7) {
			expression.WriteString(pieces[random.IntN(len(pieces))])
		}
		reached, ok := engineReachesRoot(t, root, expression.String())
		if !ok {
			continue
		}
		compiled++
		if reached {
			rooted++
			if !xpathReachesRoot(expression.String()) {
				t.Fatalf("%q: the engine went to the root, and the expression is said not to reach it", expression.String())
			}
		}
	}
	if compiled < made/10 || rooted < made*3/200 {
		t.Errorf("of the expressions made at random the engine compiled %d and took %d to the root", compiled, rooted)
	}
	t.Logf("of %d expressions made at random the engine compiled %d and took %d to the root", made, compiled, rooted)
}

// xpathLabelsBefore is xpathLabels as it was before a label could reach the
// document's root and before the node's own attribute was trimmed: the
// oracle of what the two changes left as it was.
func xpathLabelsBefore[N comparable](nodes xpathNodes[N], node N, plan []xpathLabel) map[string]string {
	engine := func(label *xpathLabel, labels map[string]string) {
		selector := label.engine()
		if value, computed := xpathValue(nodes, node, selector); computed {
			if text := strings.TrimSpace(xpathText(value)); text != "" {
				labels[label.name] = text
			}
		} else if found, ok := nodes.one(node, selector); ok {
			labels[label.name] = strings.TrimSpace(nodeText(nodes, found))
		}
	}
	labels := make(map[string]string, len(plan))
	for i := range plan {
		label := &plan[i]
		switch label.kind {
		case xpathLabelStatic:
			labels[label.name] = label.text
		case xpathLabelOwnAttribute:
			labels[label.name] = nodes.attr(node, label.text)
		case xpathLabelNone:
		case xpathLabelEngine:
			engine(label, labels)
		default:
			text, found, walked := fastXPathLabel(nodes, node, label)
			switch {
			case !walked:
				if value, attribute := nodes.selectedAttribute(node); attribute && label.kind == xpathLabelText {
					labels[label.name] = strings.TrimSpace(value)
				} else if label.program != nil {
					engine(label, labels)
				}
			case found:
				labels[label.name] = strings.TrimSpace(text)
			}
		}
	}
	return labels
}

// xpathAbsoluteLabels are label expressions that are absolute and nothing
// else: each gives at every node what it gave at the document, where an
// absolute path always started.
var xpathAbsoluteLabels = []string{
	"//region", "//@id", "/items/@id", "/items/item/region", "//title", "/html/body/@id", "//td[2]", "(//value)[2]", "//item[2]/value", "//li/@data-id", "/root/*/@site",
	"count(//item)", "count(//td)", "string(/items/@site)", "concat(//@id, '-', /items/@site)", "//item/@id = 'item-1'", "//x:region | //region", "normalize-space(//td)", " //value", "/",
}

// rootDifference holds what the two changes made of a label to what it was
// before them, at every node of a document: a label whose expression has no
// absolute path is what it was, evaluated as it is and from the navigator
// with the document for its root alike, but for the node's own attribute,
// which is what it was without the blanks around it; and one that is an
// absolute path and nothing else is what it was at the document. An
// absolute path that matches nothing walks the whole document from each
// node it is read at, so in a document of more than 300 nodes those are
// read at 300 of them, spread over all of it, nodes of every kind among
// them. Under the race detector every label is read so, at 25 nodes of a
// document at most. It returns how many labels it compared, and how many of
// them were there.
func rootDifference[N comparable](t *testing.T, nodes xpathNodes[N], root N, namespaces map[string]string, expressions []string, document string) (compared, found int) {
	t.Helper()
	absolute := map[string]bool{}
	for _, expression := range xpathAbsoluteLabels {
		absolute[expression] = true
	}
	all := treeNodes(t, nodes, root)
	for _, expression := range expressions {
		rule := model.MetricRule{Name: "m", Labels: []model.LabelRule{{Name: "l", Expression: expression}}}
		plan := planXPathLabels(rule, namespaces, nodes.html)
		if plan[0].rooted != absolute[expression] {
			t.Fatalf("label %q is planned to reach the root: %t", expression, plan[0].rooted)
		}
		readings := map[string][]xpathLabel{"as planned": plan}
		// Whatever the engine evaluates, at every node or at those a walk
		// is not taken from, is evaluated from the other navigator too.
		forced := planXPathLabels(rule, namespaces, nodes.html)
		if forced[0].program != nil && !absolute[expression] {
			forced[0].rooted = true
			readings["from the navigator with the document for its root"] = forced
		}
		atDocument, panicAtDocument := labelsOrPanic(func() map[string]string { return xpathLabelsBefore(nodes, root, plan) })
		stride := 1
		if absolute[expression] {
			stride += len(all) / 300
		}
		if raceDetector {
			stride = 1 + len(all)/25
		}
		for at := 0; at < len(all); at += stride {
			node := all[at]
			want, wantPanic := atDocument, panicAtDocument
			if !absolute[expression] {
				want, wantPanic = labelsOrPanic(func() map[string]string { return xpathLabelsBefore(nodes, node, plan) })
				if value, had := want["l"]; had && plan[0].kind == xpathLabelOwnAttribute {
					want["l"] = strings.TrimSpace(value)
				}
			}
			for name, read := range readings {
				got, gotPanic := labelsOrPanic(func() map[string]string { return xpathLabels(nodes, node, read) })
				if !reflect.DeepEqual(got, want) || gotPanic != wantPanic {
					t.Fatalf("label %q at node %d of %s (namespaces %v), %s: %q (panic %q), and before %q (panic %q)", expression, at, document, namespaces, name, got, gotPanic, want, wantPanic)
				}
				compared++
				if _, has := got["l"]; has {
					found++
				}
			}
		}
		releaseXPathLabels(plan)
		releaseXPathLabels(forced)
	}
	return compared, found
}

// Every label that has no absolute path in it is what it was, at every node
// of every document: the documents made to tell a walk from the engine,
// random XML, the fixtures of testdata/xml and the pages of testdata/html,
// at elements, text, comments, the document itself and the nodes made of
// attributes, for the labels read by walking the tree and those the engine
// evaluates. None of them is planned to reach the root, and evaluated from
// the navigator that has the document for its root each gives the same, so
// that navigator changes nothing but what `/` means. The node's own
// attribute is what it was without the blanks around it. A label that is an
// absolute path and nothing else gives at every node what it gave at the
// document (rootDifference). Under the race detector a label is read at 25
// nodes of a document at most, spread over all of it, so every expression is
// still read over every document.
func TestLabelsWithoutAnAbsolutePathAreWhatTheyWere(t *testing.T) {
	absolute := map[string]bool{}
	for _, expression := range xpathAbsoluteLabels {
		absolute[expression] = true
	}
	// The walks are held to the engine over every name elsewhere
	// (TestXPathLabelFastPathsAgreeWithTheEngine): here each shape of them
	// is read over a few.
	walked := []string{".", "text()"}
	for _, name := range []string{"id", "region", "value", "td", "class", "missing"} {
		walked = append(walked, name, "./"+name, "../"+name, "../../"+name, "../@"+name, "../../@"+name)
	}
	// The engine's expressions are those written for the documents of each
	// kind: the near misses of the walks for XML, and the labels the
	// differences of HTML rules are read with for the pages.
	expressions := map[bool][]string{}
	for html, engine := range map[bool][]string{false: xpathEngineExpressions, true: differentialLabels} {
		expressions[html] = append([]string{}, xpathAbsoluteLabels...)
		for _, lists := range [][]string{walked, {"@id", "@kind", "@x:id", "@class", "@data-id", "@og:type"}, engine} {
			for _, expression := range lists {
				if !absolute[expression] {
					expressions[html] = append(expressions[html], expression)
				}
			}
		}
	}
	documents := map[string]string{}
	for i, document := range xpathLabelDocuments {
		documents[fmt.Sprintf("XML document %d", i)] = document
	}
	random := rand.New(rand.NewPCG(20261003, 6))
	for i := range 4 {
		documents[fmt.Sprintf("random XML document %d", i)] = randomXML(random)
	}
	files, err := filepath.Glob("../../testdata/xml/*.xml")
	if err != nil || len(files) < 2 {
		t.Fatalf("%d XML fixtures, %v", len(files), err)
	}
	for _, file := range files {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		documents[filepath.Base(file)] = string(raw)
	}
	compared, found := 0, 0
	for name, document := range documents {
		root, err := decode.ParseXML([]byte(document))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for _, namespaces := range []map[string]string{nil, {"x": "urn:x", "y": "urn:y"}} {
			if namespaces != nil && !strings.HasPrefix(name, "XML document") {
				continue
			}
			c, f := rootDifference(t, xmlNodes, root, namespaces, expressions[false], name)
			compared, found = compared+c, found+f
		}
	}
	for name, page := range htmlFixturePages(t) {
		doc, err := decode.ParseHTML([]byte(page))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		c, f := rootDifference(t, htmlNodes, doc.Nodes[0], nil, expressions[true], name)
		compared, found = compared+c, found+f
	}
	if compared < alloctest.UnlessRaced(300000, 45000) || found < alloctest.UnlessRaced(50000, 7500) {
		t.Fatalf("%d labels compared, %d of them found", compared, found)
	}
	t.Logf("%d labels compared, %d of them found, of %d expressions over XML and %d over HTML, in %d XML documents and the pages", compared, found, len(expressions[false]), len(expressions[true]), len(documents))
}

// A label with an absolute path beside a relative one is the two put
// together: at every node of a document, the absolute part as it is read
// at the document and the relative part as it is read at the node, each by
// the library's own navigator.
func TestAnAbsoluteAndARelativePathInOneLabel(t *testing.T) {
	text := func(nodes xpathNodes[*xmlquery.Node], node *xmlquery.Node, expression string) string {
		e, err := xpath.Compile("string(" + expression + ")")
		if err != nil {
			t.Fatal(err)
		}
		value, _ := xpathValue(nodes, node, e)
		return xpathText(value)
	}
	root, err := decode.ParseXML([]byte(xpathLabelDocuments[0]))
	if err != nil {
		t.Fatal(err)
	}
	compared := 0
	for _, absolute := range []string{"/items/@site", "//value", "//a.b/@data-id", "/items/item/region", "/nothing"} {
		for _, relative := range []string{"../@id", "@kind", "region", ".", "name()", "ancestor::item/@id", "following-sibling::*"} {
			for _, expression := range []string{"concat(" + absolute + ", '|', " + relative + ")", "concat(" + relative + ", '|', " + absolute + ")"} {
				rule := model.MetricRule{Name: "m", Labels: []model.LabelRule{{Name: "l", Expression: expression}}}
				plan := planXPathLabels(rule, nil, false)
				for at, node := range treeNodes(t, xmlNodes, root) {
					want := text(xmlNodes, root, absolute) + "|" + text(xmlNodes, node, relative)
					if strings.HasPrefix(expression, "concat("+relative) {
						want = text(xmlNodes, node, relative) + "|" + text(xmlNodes, root, absolute)
					}
					if got := xpathLabels(xmlNodes, node, plan)["l"]; got != strings.TrimSpace(want) {
						t.Fatalf("%s at node %d is %q, want %q", expression, at, got, strings.TrimSpace(want))
					}
					compared++
				}
				releaseXPathLabels(plan)
			}
		}
	}
	if compared < 5000 {
		t.Fatalf("%d labels compared", compared)
	}
}

// A label the engine evaluates that has no absolute path in it allocates
// what it allocated: it is evaluated from the library's navigator, and
// nothing is made for it that was not. One with an absolute path costs the
// navigator around the library's on top of what the same path costs at the
// document, where the library's own navigator reads it.
func TestALabelWithoutAnAbsolutePathAllocatesWhatItDid(t *testing.T) {
	if raceDetector {
		t.Skip("the race detector changes what is allocated")
	}
	root, err := decode.ParseXML([]byte(rootedMarkup))
	if err != nil {
		t.Fatal(err)
	}
	job := xmlquery.FindOne(root, "//job")
	page, err := decode.ParseHTML([]byte(rootedMarkup))
	if err != nil {
		t.Fatal(err)
	}
	cell := page.Find("job").Nodes[0]
	for _, expression := range []string{"ancestor::status/@site", "count(../job)", "concat(../@name, '-', @id)", "../x[@ref='b']", "../@name", "@id", "."} {
		rule := model.MetricRule{Name: "m", Labels: []model.LabelRule{{Name: "l", Expression: expression}}}
		plan := planXPathLabels(rule, nil, false)
		now, _ := alloctest.Allocations(200, func() { xpathLabels(xmlNodes, job, plan) })
		before := alloctest.AllocsAtMost(200, now, func() { xpathLabelsBefore(xmlNodes, job, plan) })
		releaseXPathLabels(plan)
		if now != before {
			t.Errorf("%s over XML allocates %.0f times, and before %.0f", expression, now, before)
		}
		plan = planXPathLabels(rule, nil, true)
		now, _ = alloctest.Allocations(200, func() { xpathLabels(htmlNodes, cell, plan) })
		before = alloctest.AllocsAtMost(200, now, func() { xpathLabelsBefore(htmlNodes, cell, plan) })
		releaseXPathLabels(plan)
		if now != before {
			t.Errorf("%s over HTML allocates %.0f times, and before %.0f", expression, now, before)
		}
	}
	rule := model.MetricRule{Name: "m", Labels: []model.LabelRule{{Name: "l", Expression: "/status/@site"}}}
	plan := planXPathLabels(rule, nil, false)
	defer releaseXPathLabels(plan)
	now, _ := alloctest.Allocations(200, func() { xpathLabels(xmlNodes, job, plan) })
	document, _ := alloctest.Allocations(200, func() { xpathLabelsBefore(xmlNodes, root, plan) })
	if now > document+2 {
		t.Errorf("/status/@site allocates %.0f times at a job, and %.0f at the document", now, document)
	}
}

// The node's own attribute is a label like any other: read without the
// blanks around its value, over XML and over HTML, by a plain name, by a
// name with a prefix and at an attribute the rule selected, with the blanks
// inside it kept. It was kept as written, where the same attribute read
// through a path, in a string or by normalize-space() was trimmed. The
// value a rule reads from an attribute never had the blanks around it.
func TestANodesOwnAttributeIsTrimmedLikeEveryOtherLabel(t *testing.T) {
	for decoder, body := range map[string]string{
		"xml":  `<r xmlns:x="urn:x"><job title="  nightly   run  " x:kind=" batch " note="   " size=" 5 ">7</job></r>`,
		"html": `<table><tr><td title="  nightly   run  " x:kind=" batch " note="   " size=" 5 ">7</td></tr></table>`,
	} {
		element := map[string]string{"xml": "job", "html": "td"}[decoder]
		c := xpathRuleCollector(decoder, model.MetricRule{Name: "m", Type: model.GaugeMetricType, Expression: "//" + element, Labels: []model.LabelRule{
			{Name: "own", Expression: "@title"},
			{Name: "prefixed", Expression: "@x:kind"},
			// Blanks alone are no value, and the label is left off like one
			// of an attribute the node has not.
			{Name: "blank", Expression: "@note"},
			{Name: "absent", Expression: "@nothing"},
			{Name: "through_a_path", Expression: "../" + element + "/@title"},
			{Name: "as_a_string", Expression: "string(@title)"},
			{Name: "on_one_line", Expression: "normalize-space(@title)"},
		}})
		set, err := runBody(t, c, markupContentType[decoder], body)
		want := []string{`m{as_a_string="nightly   run",on_one_line="nightly run",own="nightly   run",prefixed="batch",through_a_path="nightly   run"} 7`}
		if got := htmlSeries(set); err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("%s: a rule that selects the element made %q, %v, want %q", decoder, got, err, want)
		}
		// At an attribute the rule selected, its own name gives its value.
		c = xpathRuleCollector(decoder, model.MetricRule{Name: "m", Type: model.GaugeMetricType, Expression: "//" + element + "/@size", Labels: []model.LabelRule{
			{Name: "own", Expression: "@size"},
			{Name: "itself", Expression: "."},
			{Name: "text", Expression: "text()"},
		}})
		set, err = runBody(t, c, markupContentType[decoder], body)
		want = []string{`m{itself="5",own="5",text="5"} 5`}
		if got := htmlSeries(set); err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("%s: a rule that selects the attribute made %q, %v, want %q", decoder, got, err, want)
		}
	}
}
