package decode

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"strings"

	"gopkg.in/yaml.v3"
)

// The YAML library reads a whole number beyond 64 bits as a float, which
// keeps only its first 17 digits: an identifier of 20 became another one.
// The same digits in JSON are a number that keeps every digit
// (model.Normalize). So such a number is read as JSON's is. Before the
// library decodes the document, the number is marked as text that begins
// with yamlWholeMark. In what the library made of the document, each such
// text is turned into the json.Number of its digits, which model.Normalize
// makes a *big.Int of, or leaves as text beyond model.MaxWholeNumberDigits.

// yamlWholeMark begins the text a whole number is marked as. It is drawn
// anew by each process, so no document can hold text that is taken for one.
var yamlWholeMark = func() string {
	var drawn [12]byte
	if _, err := rand.Read(drawn[:]); err != nil {
		panic(err)
	}
	return "whole number " + hex.EncodeToString(drawn[:]) + " "
}()

// markYAMLWholeNumbers marks each whole number under n that the library
// would read as a float, and reports whether there was one. Only a plain
// scalar can be one: a quoted scalar is text, and one with its own tag is
// what the tag says. A mapping's key is left as it was, since a key is text
// however it is written; key says that n is a key.
func markYAMLWholeNumbers(n *yaml.Node, key bool) (marked bool) {
	switch n.Kind {
	case yaml.ScalarNode:
		if key || n.Style != 0 || n.ShortTag() != "!!float" || !yamlWholeNumber(n.Value) {
			return false
		}
		n.Tag, n.Value = "!!str", yamlWholeMark+strings.TrimPrefix(n.Value, "+")
		return true
	case yaml.MappingNode:
		for i, child := range n.Content {
			marked = markYAMLWholeNumbers(child, i%2 == 0) || marked
		}
	default:
		// What an alias stands for is in the document where it was written.
		for _, child := range n.Content {
			marked = markYAMLWholeNumbers(child, false) || marked
		}
	}
	return marked
}

// yamlWholeNumber reports whether s is digits, with a sign before them or
// not.
func yamlWholeNumber(s string) bool {
	if s != "" && (s[0] == '-' || s[0] == '+') {
		s = s[1:]
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return s != ""
}

// yamlWholeNumbers is v, as the library decoded a marked document, with each
// marked text the number it was written as. A key that stands for a marked
// number, as an alias of one does, is its digits.
func yamlWholeNumbers(v any) any {
	switch x := v.(type) {
	case string:
		if digits, ok := strings.CutPrefix(x, yamlWholeMark); ok {
			return json.Number(digits)
		}
	case []any:
		for i, item := range x {
			x[i] = yamlWholeNumbers(item)
		}
	case map[string]any:
		for key, value := range x {
			if digits, ok := strings.CutPrefix(key, yamlWholeMark); ok {
				delete(x, key)
				key = digits
			}
			x[key] = yamlWholeNumbers(value)
		}
	case map[any]any:
		for key, value := range x {
			if text, isText := key.(string); isText {
				if digits, ok := strings.CutPrefix(text, yamlWholeMark); ok {
					delete(x, key)
					key = digits
				}
			}
			x[key] = yamlWholeNumbers(value)
		}
	}
	return v
}
