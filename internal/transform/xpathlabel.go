package transform

import (
	"fmt"
	"maps"
	"strings"

	"github.com/antchfx/xpath"
	"github.com/eenchev/prometheus-universal-exporter/internal/expr"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// An XPath rule reads each of its labels at every node it selects, and
// running the XPath engine for one costs far more than what most labels
// ask for: an evaluation clones the expression's query tree, makes a
// navigator and copies it at every step, and for an attribute builds a node
// to hold its value, a dozen allocations to read ../@id. Over a document of
// thousands of nodes the labels were a fifth of a probe's time.
//
// Nearly every label is one of a few shapes that are a walk of a step or two
// in the tree the document was parsed into:
//
//	@name                the node's own attribute, by its name as written
//	../@name ../../@name an attribute of its parent, or of an ancestor
//	name  ./name         the text of its first child element of that name
//	../name              the text of a sibling, the parent's first such child
//	text()               its first text child
//	.                    its own text
//
// So how a label is read is decided once for the rule (planXPathLabels), not
// at every node, and a label of one of these shapes is read by walking the
// tree directly (fastXPathLabel). Every other expression — a predicate, a
// function, an axis by name, a prefixed name, a path of several elements, a
// union, anything with blanks in it — is compiled and evaluated by the
// engine as before, with the one compiled copy the rule took for all its
// nodes rather than one taken and handed back at each.
//
// The node's own attribute is read by its name as the document writes it
// (ownAttributeLabel), which in HTML, where there are no namespaces, is any
// name at all, a colon included, and in XML a name without a prefix, or
// with the document's own prefix where response.namespaces is not set. Its
// value is trimmed of the blanks around it, as every other label is: kept
// as written, `@title` had them where `../@title` had not.
//
// The walk gives what the engine gives, which is the engine's reading and
// not always XPath's. It is taken only from an element — and, to an
// attribute of a parent in HTML, from any node, a text node's parent being
// an element like any other — only for a name without a prefix, and repeats
// what the engine's navigators do (antchfx/xmlquery and htmlquery, as
// pinned in go.mod):
//
//   - a name without a prefix matches by local name where the node has no
//     prefix, whatever namespace it is in, a default namespace included,
//     and whatever response.namespaces binds: the engine compares
//     namespaces only for a name with a prefix;
//   - of several children of the name, the first in document order is the
//     one read, and its text is that of everything beneath it, without
//     comments and with CDATA, as the engine's first match is read;
//   - in XML a processing instruction counts as an element of its target's
//     name, and text() skips a blank text node that is not the first child,
//     and takes CDATA and a directive for text; in HTML neither applies;
//   - an attribute is found only on an element, by its name as written
//     without a prefix, the first of that name.
//
// One walk is not the engine's: in HTML an attribute of a parent whose name
// the engine cannot say — ../@og:type, which it reads as a prefix no HTML
// attribute has, ../@2x, which it does not parse — is read by that name as
// written, as the node's own is. And one label is neither a walk nor the
// engine's: text() at an attribute a rule selected is the attribute's value
// (xpathLabels).
//
// TestXPathLabelFastPathsAgreeWithTheEngine holds the two to that, node by
// node, over documents made to tell them apart.

// xpathLabelKind is how a label of an XPath rule is read at a node.
type xpathLabelKind uint8

const (
	// xpathLabelStatic is a label with a value of its own.
	xpathLabelStatic xpathLabelKind = iota
	// xpathLabelOwnAttribute is @name, read from the node by the name as
	// written (ownAttributeLabel).
	xpathLabelOwnAttribute
	// xpathLabelNone is an expression that does not compile for the kind of
	// document that arrived, which fails the rule (unreadableXPathLabel).
	xpathLabelNone
	// xpathLabelEngine is any expression the engine evaluates.
	xpathLabelEngine
	// The rest are read by walking the tree from an element, after going up
	// to a parent ups times: the node's own text, its first text child, the
	// text of its first child element of a name, and its attribute of a
	// name.
	xpathLabelSelf
	xpathLabelText
	xpathLabelChild
	xpathLabelAttribute
)

// xpathLabel is one label of a rule and how it is read: text is the value of
// a static label and the name of the attribute or element the others read,
// and program the compiled expression of every label that may need the
// engine, which a walk needs too for a node that is no element, and which
// is nil for a walk the engine cannot make; selector is the copy of it the
// rule took (engine). Of a label that does not compile, text is the
// expression and err why it does not. rooted says the engine's expression
// may reach the document's root, with an absolute path in it, and is
// evaluated from a navigator that has the document for its root
// (rootedXPathNodes). evaluating says the engine is evaluating the label,
// which it still says after the engine panicked on it: the rule's failure
// then names the label (xpathEngineFailure). constant is set for a label
// whose expression cannot depend on the node, and holds what it was read as
// (xpathconstant.go).
type xpathLabel struct {
	name       string
	kind       xpathLabelKind
	text       string
	ups        int
	program    *expr.XPathProgram
	selector   *xpath.Expr
	err        error
	rooted     bool
	evaluating bool
	constant   *xpathConstant
}

// xmlNamespace is the namespace XML binds the prefix xml to, in every
// document and without a declaration (Namespaces in XML, § 3).
const xmlNamespace = "http://www.w3.org/XML/1998/namespace"

// boundNamespaces is response.namespaces as XPath expressions over XML are
// compiled with them: the operator's prefixes, and xml, which XML reserves
// for one namespace and no document declares. Without it `@xml:lang` was an
// unmapped prefix, refused at load, as soon as any other prefix was mapped.
// With no namespaces set there are none to add to: a prefix is then the
// document's own, xml among them.
func boundNamespaces(namespaces map[string]string) map[string]string {
	if len(namespaces) == 0 || namespaces["xml"] == xmlNamespace {
		return namespaces
	}
	bound := make(map[string]string, len(namespaces)+1)
	maps.Copy(bound, namespaces)
	bound["xml"] = xmlNamespace
	return bound
}

// ownAttributeLabel reports whether a label expression is one attribute of
// the node itself that is read from the node by its name as the document
// writes it, and gives that name.
//
// In HTML there are no namespaces and a colon is a character of a name like
// any other: xml:lang, og:type, v-on:click, :href and @click are attributes'
// names, and every @ followed by one name is read by it (htmlAttributeName),
// whatever response.namespaces holds.
//
// In XML that is a plain name without a prefix, ^[A-Za-z_][\w.-]*$, and,
// where response.namespaces is not set, a plain name with a plain prefix,
// which is then the document's own, as before there were namespaces to set.
// With response.namespaces set a prefixed attribute means what the map says
// its prefix means: it is XPath, compiled with the namespaces and checked
// at load, like every other expression that starts with @ — a union, a
// predicate, a comparison, a step after the attribute.
func ownAttributeLabel(expression string, html bool, namespaces map[string]string) (name string, byName bool) {
	name, isAttribute := strings.CutPrefix(expression, "@")
	switch {
	case !isAttribute:
		return "", false
	case html:
		return name, htmlAttributeName(name)
	case plainXPathName(name):
		return name, true
	case len(namespaces) > 0:
		return "", false
	}
	prefix, local, _ := strings.Cut(name, ":")
	return name, plainXPathName(prefix) && plainXPathName(local)
}

// htmlAttributeName reports whether what follows the @ of a label over an
// HTML document is one attribute's name rather than more XPath: it has
// nothing in it XPath takes for an operator — a step, a union, a predicate,
// a call, a comparison, arithmetic, a string, a blank. A hyphen, a dot, a
// colon and an @ are characters of HTML attribute names, as in data-id,
// x-on:click.prevent, :href and @click.
func htmlAttributeName(name string) bool {
	return name != "" && !strings.ContainsAny(name, "/|[]()=<>!+*,$'\" \t\r\n")
}

// planXPathLabels decides how each label of rule is read, in a document
// that is HTML or XML, and takes the compiled expressions the rule needs,
// which releaseXPathLabels hands back.
func planXPathLabels(rule model.MetricRule, namespaces map[string]string, html bool) []xpathLabel {
	if len(rule.Labels) == 0 {
		return nil
	}
	labels := make([]xpathLabel, len(rule.Labels))
	for i, labelRule := range rule.Labels {
		label := &labels[i]
		label.name = labelRule.Name
		if labelRule.Static() {
			label.kind, label.text = xpathLabelStatic, labelRule.Value
			continue
		}
		if name, byName := ownAttributeLabel(labelRule.Expression, html, namespaces); byName {
			label.kind, label.text = xpathLabelOwnAttribute, name
			continue
		}
		kind, ups, name, walked := xpathLabelShape(labelRule.Expression, html)
		program, err := expr.CompileXPath(labelRule.Expression, namespaces)
		switch {
		case walked:
			// program is nil for the walk the engine cannot make.
			label.program, label.kind, label.ups, label.text = program, kind, ups, name
		case err != nil:
			label.kind, label.text, label.err = xpathLabelNone, labelRule.Expression, err
		default:
			label.program, label.kind, label.rooted = program, xpathLabelEngine, xpathReachesRoot(labelRule.Expression)
			if label.rooted && xpathConstantLabel(labelRule.Expression) {
				label.constant = &xpathConstant{}
			}
		}
	}
	return labels
}

// unreadableXPathLabel is the failure of a rule that has a label which
// cannot be read in the kind of document that arrived, nil when every label
// can. The load refuses a label no document of the collector's could give,
// so this is a collector whose decoder is left to each response, with a
// label only HTML can give — an attribute by a name XPath cannot say, as
// @1x, @a:b:c and ../@:kind are — and an XML answer. Such a label was left
// off without a word, and the rule's series were exported without it: now
// the rule fails, by its error_mode as it does for any label it cannot
// give, and says which label and why.
func unreadableXPathLabel[N comparable](nodes xpathNodes[N], rule model.MetricRule, plan []xpathLabel) error {
	for i := range plan {
		if label := &plan[i]; label.kind == xpathLabelNone {
			document := "XML"
			if nodes.html {
				document = "HTML"
			}
			return fmt.Errorf("metric %q label %q: %s %q cannot be read in the %s document that arrived (%w); set decoder.type to the kind of document the target answers with, or write the label so that it can be read in both", rule.Name, label.name, nodes.kind, label.text, document, label.err)
		}
	}
	return nil
}

// engine is the label's compiled expression, the rule's own copy of it for
// as long as it reads its nodes: taken when the label first needs the
// engine, which one read by walking the tree seldom does.
func (l *xpathLabel) engine() *xpath.Expr {
	if l.selector == nil {
		l.selector = l.program.Get()
	}
	return l.selector
}

// releaseXPathLabels hands back the compiled expressions labels took.
func releaseXPathLabels(labels []xpathLabel) {
	for i := range labels {
		if labels[i].selector != nil {
			labels[i].program.Put(labels[i].selector)
			labels[i].selector = nil
		}
	}
}

// xpathLabelShape reports whether a label expression is one of the shapes
// read by walking the tree, exactly as written above: no blanks, no axis
// names, no prefix, no predicate. ups is how many parents the walk goes up
// first, and name the element or attribute it then reads. In HTML a
// parent's attribute is walked to by any name an attribute can have
// (htmlAttributeName), as the node's own is read.
func xpathLabelShape(expression string, html bool) (kind xpathLabelKind, ups int, name string, walked bool) {
	switch expression {
	case ".":
		return xpathLabelSelf, 0, "", true
	case "text()":
		return xpathLabelText, 0, "", true
	}
	rest, own := strings.CutPrefix(expression, "./")
	if own {
		// ./name; a parent after it, as ./../name has, is the engine's.
		return xpathLabelChild, 0, rest, plainXPathName(rest)
	}
	for {
		above, up := strings.CutPrefix(rest, "../")
		if !up {
			break
		}
		rest, ups = above, ups+1
	}
	if attribute, isAttribute := strings.CutPrefix(rest, "@"); isAttribute {
		// The node's own attribute is ownAttributeLabel's.
		return xpathLabelAttribute, ups, attribute, ups > 0 && (plainXPathName(attribute) || html && htmlAttributeName(attribute))
	}
	return xpathLabelChild, ups, rest, plainXPathName(rest)
}

// plainXPathName reports whether name is one plain name, as
// ownAttributeLabel asks of an XML attribute's: ^[A-Za-z_][\w.-]*$, which
// the engine reads as one name without a prefix.
func plainXPathName(name string) bool {
	if name == "" {
		return false
	}
	for i := range len(name) {
		c := name[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c == '_':
		case i > 0 && (c >= '0' && c <= '9' || c == '.' || c == '-'):
		default:
			return false
		}
	}
	return true
}

// fastXPathLabel reads at node a label that is read by walking the tree.
// walked is false for a node the walk is not taken from, which is every
// node but an element, except for an attribute of a parent in HTML: the
// engine reads the label there. Otherwise found
// says the walk reached what the label reads, and text is its text, as the
// engine's first match and nodes.text would give it.
func fastXPathLabel[N comparable](nodes xpathNodes[N], node N, label *xpathLabel) (text string, found, walked bool) {
	// In HTML an attribute of a parent is walked to from any node, a text
	// node as //td/text() selects them included: the parent is an element
	// whatever the node is, and its attribute is read by its name as
	// written, which for a name like og:type only the walk can do.
	if !nodes.element(node) && (!nodes.html || label.kind != xpathLabelAttribute) {
		return "", false, false
	}
	at := node
	for range label.ups {
		parent, ok := nodes.parent(at)
		if !ok {
			return "", false, true
		}
		at = parent
	}
	switch label.kind {
	case xpathLabelSelf:
		return nodeText(nodes, at), true, true
	case xpathLabelText:
		if child, ok := nodes.firstText(at); ok {
			return nodeText(nodes, child), true, true
		}
	case xpathLabelChild:
		if child, ok := nodes.child(at, label.text); ok {
			return nodeText(nodes, child), true, true
		}
	case xpathLabelAttribute:
		if value, ok := nodes.attribute(at, label.text); ok {
			return value, true, true
		}
	}
	return "", false, true
}

// nodeText is nodes.text(node): the text of everything beneath the node,
// without comments. A node that is one leaf, or holds one, as the element
// of a value or of a label nearly always does, is read as that leaf's text
// without building the same string anew.
func nodeText[N comparable](nodes xpathNodes[N], node N) string {
	if own, leaf := nodes.leaf(node); leaf {
		return own
	}
	first, ok := nodes.first(node)
	if !ok {
		return ""
	}
	if _, more := nodes.next(first); !more {
		if own, leaf := nodes.leaf(first); leaf {
			return own
		}
	}
	return nodes.text(node)
}

// xpathLabels are a series' labels, each read relative to node as
// planXPathLabels decided.
func xpathLabels[N comparable](nodes xpathNodes[N], node N, plan []xpathLabel) map[string]string {
	labels := make(map[string]string, len(plan))
	for i := range plan {
		label := &plan[i]
		switch label.kind {
		case xpathLabelStatic:
			labels[label.name] = label.text
		case xpathLabelOwnAttribute:
			labels[label.name] = strings.TrimSpace(nodes.attr(node, label.text))
		case xpathLabelNone:
		case xpathLabelEngine:
			engineXPathLabel(nodes, node, label, labels)
		default:
			text, found, walked := fastXPathLabel(nodes, node, label)
			switch {
			case !walked:
				// At an attribute a rule selected, as //@id selects them,
				// text() is the attribute's value. To XPath an attribute
				// has no text node beneath it and text() selects nothing
				// there, but the value is what the label always gave, and
				// what a configuration with it relies on to tell its
				// series apart: read as XPath has it, every series of the
				// rule lost the label and they became duplicates of one.
				// Any other walk the engine cannot make is taken where
				// fastXPathLabel takes it or not at all.
				if value, attribute := nodes.selectedAttribute(node); attribute && label.kind == xpathLabelText {
					labels[label.name] = strings.TrimSpace(value)
				} else if label.program != nil {
					engineXPathLabel(nodes, node, label, labels)
				}
			case found:
				labels[label.name] = strings.TrimSpace(text)
			}
		}
	}
	return labels
}

// engineXPathLabel reads a label at node with the XPath engine, into
// labels: the value its expression computes, or the text of the first node
// it selects. An absolute path in the expression starts at the document
// (rootedXPathNodes). A label that cannot depend on the node is evaluated
// at the first node it is asked for at, and has that value at the others
// (xpathconstant.go).
func engineXPathLabel[N comparable](nodes xpathNodes[N], node N, label *xpathLabel, labels map[string]string) {
	if once := label.constant; once != nil {
		if !once.read {
			once.text, once.found = evaluateXPathLabel(nodes, node, label)
			once.read = true
		}
		if once.found {
			labels[label.name] = once.text
		}
		return
	}
	if text, found := evaluateXPathLabel(nodes, node, label); found {
		labels[label.name] = text
	}
}

// evaluateXPathLabel evaluates a label at node with the XPath engine. found
// is false for a label that is left off: one whose expression computes
// nothing but blanks, or selects no node.
func evaluateXPathLabel[N comparable](nodes xpathNodes[N], node N, label *xpathLabel) (text string, found bool) {
	if label.rooted {
		nodes = rootedXPathNodes(nodes)
	}
	selector := label.engine()
	label.evaluating = true
	if value, computed := xpathValue(nodes, node, selector); computed {
		text = strings.TrimSpace(xpathText(value))
		found = text != ""
	} else if selected, ok := nodes.one(node, selector); ok {
		// Trimmed, as a css label is: the text of an element in
		// pretty-printed markup starts and ends with the
		// indentation around it, which is no part of the value.
		text, found = strings.TrimSpace(nodeText(nodes, selected)), true
	}
	label.evaluating = false
	return text, found
}
