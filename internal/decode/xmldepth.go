package decode

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"strings"

	"github.com/antchfx/xmlquery"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
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
		// The line a syntax error names is where in the body it is, which
		// is no part of what the failure is to the log.
		var syntax *xml.SyntaxError
		if errors.As(err, &syntax) {
			text, library := err.Error(), syntax.Error()
			short, same, cut := xmlNamesCut(xmlSyntaxErrors, syntax.Msg)
			if !cut {
				return nil, model.SameFailureAs(err, strings.Replace(text, library, "XML syntax error: "+syntax.Msg, 1))
			}
			// A name of the document cut to its start (xmlcut.go), in a new
			// error of the short text alone: the library's holds the name
			// whole.
			line := strings.TrimSuffix(library, syntax.Msg)
			return nil, model.SameFailureAs(errors.New(strings.Replace(text, library, line+short, 1)), strings.Replace(text, library, "XML syntax error: "+same, 1))
		}
		if short, same, cut := xmlNamesCut(xmlOtherErrors, err.Error()); cut {
			return nil, model.SameFailureAs(errors.New(short), same)
		}
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
