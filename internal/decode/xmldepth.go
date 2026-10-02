package decode

import (
	"bytes"
	"fmt"

	"github.com/antchfx/xmlquery"
)

// MaxXMLDepth is how deep the elements of an XML document may nest. It is the
// bound the HTML parser has for its open elements, so XPath meets the same
// documents whichever decoder read them. What reads a parsed document walks
// it by recursion, and the text of an element is that of everything beneath
// it, so a document of a hundred thousand nested elements, which fits in a
// megabyte, costs a rule that selects them all the square of that.
const MaxXMLDepth = 512

// ParseXML parses an XML document, and refuses one whose elements nest deeper
// than MaxXMLDepth.
func ParseXML(body []byte) (*xmlquery.Node, error) {
	root, err := xmlquery.Parse(bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	// The parser has no bound to give, so the tree is measured once it is
	// built: by its links rather than by recursion, which is what a tree
	// too deep would exhaust.
	depth := 0
	for node := root; ; {
		if node.FirstChild != nil {
			node = node.FirstChild
			depth++
			if depth > MaxXMLDepth && node.Type == xmlquery.ElementNode {
				return nil, fmt.Errorf("the document nests elements more than %d deep, which is the most the exporter reads", MaxXMLDepth)
			}
			continue
		}
		for node != root && node.NextSibling == nil {
			node = node.Parent
			depth--
		}
		if node == root {
			return root, nil
		}
		node = node.NextSibling
	}
}
