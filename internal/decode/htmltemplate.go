package decode

import (
	"bytes"

	"github.com/PuerkitoBio/goquery"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// ParseHTML parses an HTML document as the css and xpath transforms read it:
// what the parser makes of the body, with the content of every template
// element taken out of it.
//
// A template holds markup a script copies into the page, a row with {{name}}
// where a name goes. To a browser its content is no part of the document: it
// stands in a fragment of its own, which no selector matches and whose text
// is no element's. The parser here builds it as ordinary children of the
// template element, where `table tr` and //td found the placeholder row
// among the real ones, and a rule failed on a value that is "{{n}}". Rules
// are written against what a browser's inspector shows, so the content is
// detached once, here, for every reader alike: selectors, items, XPath
// expressions and labels do not match it, and it is no part of the text of
// an element around it. The template element itself stays, empty.
//
// A template with a shadowrootmode attribute, or the shadowroot one browsers
// read before it, is a declarative shadow root: a browser attaches its
// content to the element around it and renders it, with no script, and its
// inspector shows it. That content is kept, as ordinary children of the
// template element, where selectors and expressions find it.
func ParseHTML(body []byte) (*goquery.Document, error) {
	document, err := goquery.NewDocumentFromReader(bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	emptyTemplates(document.Nodes[0])
	return document, nil
}

// emptyTemplates detaches the children of every template element beneath
// root. It walks the tree by its links, as ParseXML measures one, and asks
// of each node one thing, so a page without a template costs a pass over
// its nodes. A template inside svg or math is an element of theirs and no
// HTML template: it keeps its content, as it does in a browser. So does a
// declarative shadow root, whose content is walked like any other, a
// template inside it being emptied.
func emptyTemplates(root *html.Node) {
	for node := root; ; {
		switch {
		case node.DataAtom == atom.Template && node.Type == html.ElementNode && node.Namespace == "" && !isShadowRoot(node):
			for node.FirstChild != nil {
				node.RemoveChild(node.FirstChild)
			}
		case node.FirstChild != nil:
			node = node.FirstChild
			continue
		}
		for node != root && node.NextSibling == nil {
			node = node.Parent
		}
		if node == root {
			return
		}
		node = node.NextSibling
	}
}

// isShadowRoot reports whether a template element declares a shadow root:
// it has a shadowrootmode attribute, whatever its value, or the shadowroot
// attribute that came before it.
func isShadowRoot(template *html.Node) bool {
	for _, attribute := range template.Attr {
		if attribute.Namespace == "" && (attribute.Key == "shadowrootmode" || attribute.Key == "shadowroot") {
			return true
		}
	}
	return false
}
