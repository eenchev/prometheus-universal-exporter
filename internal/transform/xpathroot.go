package transform

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/antchfx/htmlquery"
	"github.com/antchfx/xmlquery"
	"github.com/antchfx/xpath"
	"golang.org/x/net/html"
)

// A label's expression is evaluated at the node its rule selected, and an
// absolute path in it — /status/@site, //status, a predicate that compares
// with //y/@id — starts at the document, as XPath has it. The engine's
// navigators (antchfx/xmlquery and htmlquery, as pinned in go.mod) do not:
// one takes the node it was made at for its root, so that from a selected
// node `/` and `//` meant that node, found nothing beneath it, and the label
// was left off every series without a word, where ../@site and
// ancestor::status/@site gave it.
//
// So a label that may reach the root is evaluated from a navigator whose
// root is the document's (rootedXML, rootedHTML): the library's navigator,
// whose every move but three is its own. MoveToRoot goes to the top of the
// tree the node is in, Copy gives another of the same kind, and MoveTo takes
// the other's place whole, which the library's refuses for a navigator with
// another root. Nothing else in the engine (antchfx/xpath v1.3.8) asks a
// navigator for its root: an absolute path is its one caller of MoveToRoot,
// and the order of nodes is told by walking to the top through MoveToParent.
//
// Whether a label may reach the root is decided once for the rule, from its
// expression (xpathReachesRoot), and kept in its plan: every other label is
// evaluated as it was, from the library's navigator, and costs what it cost.

// rootedXML is xmlquery's navigator with the document for its root.
type rootedXML struct{ xmlquery.NodeNavigator }

// MoveToRoot moves to the document: the top of the tree the node is in.
func (x *rootedXML) MoveToRoot() {
	top := x.Current()
	for top.Parent != nil {
		top = top.Parent
	}
	x.NodeNavigator = *xmlquery.CreateXPathNavigator(top)
}

// Copy is another navigator at the same place.
func (x *rootedXML) Copy() xpath.NodeNavigator {
	other := *x
	return &other
}

// MoveTo moves to where other is, in the same document.
func (x *rootedXML) MoveTo(other xpath.NodeNavigator) bool {
	to, ok := other.(*rootedXML)
	if ok {
		x.NodeNavigator = to.NodeNavigator
	}
	return ok
}

// rootedHTML is htmlquery's navigator with the document for its root.
type rootedHTML struct{ htmlquery.NodeNavigator }

// MoveToRoot moves to the document: the top of the tree the node is in.
func (h *rootedHTML) MoveToRoot() {
	top := h.Current()
	for top.Parent != nil {
		top = top.Parent
	}
	h.NodeNavigator = *htmlquery.CreateXPathNavigator(top)
}

// Copy is another navigator at the same place.
func (h *rootedHTML) Copy() xpath.NodeNavigator {
	other := *h
	return &other
}

// MoveTo moves to where other is, in the same document.
func (h *rootedHTML) MoveTo(other xpath.NodeNavigator) bool {
	to, ok := other.(*rootedHTML)
	if ok {
		h.NodeNavigator = to.NodeNavigator
	}
	return ok
}

// rootedXMLNavigator is xmlNavigator with the document for its root: the
// same start, at an attribute a rule selected too.
func rootedXMLNavigator(node *xmlquery.Node) xpath.NodeNavigator {
	return &rootedXML{*xmlNavigator(node).(*xmlquery.NodeNavigator)}
}

// rootedXMLOne is xmlNodes.one from a navigator with the document for its
// root: the first node e selects from node, an attribute as the node
// xmlquery makes of one.
func rootedXMLOne(node *xmlquery.Node, e *xpath.Expr) (*xmlquery.Node, bool) {
	it := e.Select(rootedXMLNavigator(node))
	if !it.MoveNext() {
		return nil, false
	}
	at := &it.Current().(*rootedXML).NodeNavigator
	if at.NodeType() != xpath.AttributeNode {
		return at.Current(), true
	}
	value := &xmlquery.Node{Type: xmlquery.TextNode, Data: at.Value()}
	return &xmlquery.Node{Parent: at.Current(), Type: xmlquery.AttributeNode, Data: at.LocalName(), FirstChild: value, LastChild: value}, true
}

// rootedHTMLNavigator is htmlNavigator with the document for its root.
func rootedHTMLNavigator(node *html.Node) xpath.NodeNavigator {
	return &rootedHTML{*htmlNavigator(node).(*htmlquery.NodeNavigator)}
}

// rootedHTMLOne is htmlNodes.one from a navigator with the document for its
// root. An attribute it selects is made by htmlSelected, with its element
// for a parent, where htmlquery's own has none: nothing but its text is
// read from the first node a label selects, which is the same of both.
func rootedHTMLOne(node *html.Node, e *xpath.Expr) (*html.Node, bool) {
	it := e.Select(rootedHTMLNavigator(node))
	if !it.MoveNext() {
		return nil, false
	}
	return htmlSelected(&it.Current().(*rootedHTML).NodeNavigator), true
}

// rootedXPathNodes is nodes with the engine started from a navigator whose
// root is the document: what a label that may reach the root is evaluated
// with (engineXPathLabel). Everything else of nodes is as it was.
func rootedXPathNodes[N comparable](nodes xpathNodes[N]) xpathNodes[N] {
	switch rooted := any(&nodes).(type) {
	case *xpathNodes[*xmlquery.Node]:
		rooted.navigate, rooted.one = rootedXMLNavigator, rootedXMLOne
	case *xpathNodes[*html.Node]:
		rooted.navigate, rooted.one = rootedHTMLNavigator, rootedHTMLOne
	}
	return nodes
}

// xpathReachesRoot reports whether an XPath expression has an absolute
// path in it, one that starts with / or //, anywhere: at its start, in a
// predicate, an argument or an operand. It is told from the expression as
// written, by the tokens the engine's parser reads (antchfx/xpath's
// scanner): a slash starts an absolute path where an operand is expected —
// at the start, after an operator, an opening bracket or a comma — and is a
// step of the path before it after anything that ends one: a name, a
// closing bracket, a dot, a number or a string. A `*` and the names and,
// or, div and mod are operators where an operand has ended and names where
// one is expected, so `../div/span` and `*/name` have no absolute path and
// `a div /b` and `2*/b` have one. A slash inside a string is no path.
//
// Where the two could differ the answer is yes: an expression wrongly said
// to reach the root is evaluated a little slower and gives what it gave,
// and one wrongly said not to would lose its label.
func xpathReachesRoot(expression string) bool {
	// operand says the token before ended an operand, so a slash after it
	// goes on with its path and a `*` or an operator's name is an operator.
	operand := false
	for i := 0; i < len(expression); {
		c, size := rune(expression[i]), 1
		if c >= utf8.RuneSelf {
			c, size = utf8.DecodeRuneInString(expression[i:])
		}
		switch {
		case unicode.IsSpace(c):
			i += size
		case c == '/':
			if !operand {
				return true
			}
			i++
			if i < len(expression) && expression[i] == '/' {
				i++
			}
			operand = false
		case c == '\'' || c == '"':
			end := strings.IndexByte(expression[i+1:], expression[i])
			if end < 0 {
				// An unclosed string, which does not compile.
				return true
			}
			i += end + 2
			operand = true
		case c == ')' || c == ']':
			i++
			operand = true
		case c == '*':
			// A multiplication after an operand, and any name otherwise.
			i++
			operand = !operand
		case c == '.':
			// The node itself, its parent, or a number as .5 is.
			i++
			if i < len(expression) && expression[i] == '.' {
				i++
			} else {
				i = xpathDigitsEnd(expression, i)
			}
			operand = true
		case c >= '0' && c <= '9':
			i = xpathDigitsEnd(expression, i)
			if i < len(expression) && expression[i] == '.' {
				i = xpathDigitsEnd(expression, i+1)
			}
			operand = true
		case xpathNameStart(c):
			start := i
			i = xpathNameEnd(expression, i)
			name, axis := expression[start:i], false
			switch {
			case strings.HasPrefix(expression[i:], "::"):
				axis, i = true, i+2
			case strings.HasPrefix(expression[i:], ":"):
				// prefix:name and prefix:* are one name, and no operator.
				name, i = "", i+1
				if i < len(expression) && expression[i] == '*' {
					i++
				} else {
					i = xpathNameEnd(expression, i)
				}
			default:
				// The colons of an axis may stand apart from its name.
				if after := xpathBlanksEnd(expression, i); strings.HasPrefix(expression[after:], "::") {
					axis, i = true, after+2
				}
			}
			// An axis is followed by the name it reads, and an operator by
			// its operand; any other name ends one.
			operand = !axis && (!operand || name != "and" && name != "or" && name != "div" && name != "mod")
		default:
			// An operator, an opening bracket, a comma, the @ of an
			// attribute or the $ of a variable: an operand follows. So it
			// does, to be on the safe side, after anything not known here.
			i += size
			operand = false
		}
	}
	return false
}

// xpathNameStart reports whether a name starts with c, as the engine's
// scanner has it: a letter or an underscore. Every character outside ASCII
// is taken for one, a blank apart, which the caller asks after first: an
// expression with one that is none does not compile.
func xpathNameStart(c rune) bool {
	return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c == '_' || c >= utf8.RuneSelf
}

// xpathNameEnd is where the name at expression[i:] ends: at the first
// character that is no part of a name, which a digit, a hyphen and a dot
// after its start are, and a colon, a slash and a blank are not.
func xpathNameEnd(expression string, i int) int {
	for i < len(expression) {
		c, size := rune(expression[i]), 1
		if c >= utf8.RuneSelf {
			c, size = utf8.DecodeRuneInString(expression[i:])
		}
		if unicode.IsSpace(c) || !xpathNameStart(c) && (c < '0' || c > '9') && c != '-' && c != '.' {
			break
		}
		i += size
	}
	return i
}

// xpathDigitsEnd is where the digits at expression[i:] end.
func xpathDigitsEnd(expression string, i int) int {
	for i < len(expression) && expression[i] >= '0' && expression[i] <= '9' {
		i++
	}
	return i
}

// xpathBlanksEnd is where the blanks at expression[i:] end.
func xpathBlanksEnd(expression string, i int) int {
	for i < len(expression) {
		c, size := rune(expression[i]), 1
		if c >= utf8.RuneSelf {
			c, size = utf8.DecodeRuneInString(expression[i:])
		}
		if !unicode.IsSpace(c) {
			break
		}
		i += size
	}
	return i
}
