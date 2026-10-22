package decode

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// A decoded body's keys are text, the keys of JSON objects, and a YAML
// mapping's keys may be numbers, booleans and null, which the YAML library
// decodes into a map of any keys: model.Normalize writes each as its text,
// fmt.Sprint's. Keys written otherwise can be one text: 1 and 1.0 are both
// "1", true and True "true", a key and an alias of it the key's text. The
// library tells keys apart by how they are written, so it refuses none of
// them, and keeps the later of two that decode into one value, 16 and 0x10
// among them, without a word; and of two that are only one text, Normalize
// kept whichever the map gave it last, which Go's map order picks at random.
// A key written twice fails the scrape, and so does one written twice
// otherwise: yamlKeysAsText finds them in the parsed document, by the lines
// they are written on, in the library's words for a key written twice, and
// yamlTextKeys, where a mapping is decoded with the keys of the mappings
// merged into it with <<, which only the decoded value shows. A null key,
// which has no text, is refused too, rather than read as "<nil>".

// yamlKeysAsText refuses a document with a mapping two of whose keys, written
// otherwise, are the same text, or with a null key, after the library has
// decoded it and so has refused any key written twice alike. Each mapping
// is read once, as the document is written, the keys merged into it apart;
// only one with a key that is no text as written is looked into: keys that
// are all text the library has told apart already.
func yamlKeysAsText(root *yaml.Node) error {
	var problems []string
	var walk func(n *yaml.Node)
	walk = func(n *yaml.Node) {
		if n.Kind == yaml.MappingNode {
			problems = yamlMappingKeysAsText(n, problems)
		}
		for _, child := range n.Content {
			walk(child)
		}
	}
	walk(root)
	if len(problems) == 0 {
		return nil
	}
	return yamlFailure(&yaml.TypeError{Errors: problems})
}

// yamlMappingKeysAsText adds to problems those of mapping's keys: a key that
// is the text of an earlier one, `line 2: mapping key "1" already defined at
// line 1`, naming it by that text, which is what the decoded body would have
// held twice, and a key that is null.
func yamlMappingKeysAsText(mapping *yaml.Node, problems []string) []string {
	plain := true
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if key := mapping.Content[i]; key.Kind != yaml.ScalarNode || key.ShortTag() != "!!str" {
			plain = false
			break
		}
	}
	if plain {
		return problems
	}
	seen := make(map[string]int, len(mapping.Content)/2)
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		key := mapping.Content[i]
		written := key
		if written.Kind == yaml.AliasNode && written.Alias != nil {
			written = written.Alias
		}
		if written.Kind != yaml.ScalarNode {
			// A key that is no scalar is refused elsewhere.
			continue
		}
		switch written.ShortTag() {
		case "!!merge":
			continue
		case "!!null":
			problems = append(problems, fmt.Sprintf("line %d: mapping key %s is null, which is no text; quote it to read it as text", key.Line, yamlKeyText(written.Value, false)))
			continue
		}
		text, ok := yamlKeyAsText(written)
		if !ok {
			continue
		}
		if line, twice := seen[text]; twice {
			problems = append(problems, fmt.Sprintf(yamlWrittenTwice, key.Line, yamlKeyText(text, false), line))
			continue
		}
		seen[text] = key.Line
	}
	return problems
}

// yamlKeyAsText is the text a scalar key is in the decoded body, as
// model.Normalize writes the value the library decodes it into, and false
// for one the library does not decode, which it has refused already.
func yamlKeyAsText(key *yaml.Node) (string, bool) {
	if key.ShortTag() == "!!str" {
		return key.Value, true
	}
	var v any
	if err := key.Decode(&v); err != nil {
		return "", false
	}
	if s, ok := v.(string); ok {
		return s, true
	}
	return fmt.Sprint(v), true
}

// yamlTextKeys is v with each mapping of any keys made a mapping of their
// text, as model.Normalize makes it, refusing one two of whose keys are the
// same text: of a mapping as it is written yamlKeysAsText has refused that,
// and a null key wherever it is, so these are keys merged into a mapping
// with <<, written otherwise than the mapping's own, as 1.0 beside 1, which
// the library keeps both of.
func yamlTextKeys(v any) (any, error) {
	switch x := v.(type) {
	case map[any]any:
		m := make(map[string]any, len(x))
		for k, item := range x {
			text, ok := k.(string)
			if !ok {
				text = fmt.Sprint(k)
			}
			if _, twice := m[text]; twice {
				return nil, fmt.Errorf("mapping key %s is in a mapping twice, written otherwise each time, once merged into it with <<", yamlKeyText(text, false))
			}
			value, err := yamlTextKeys(item)
			if err != nil {
				return nil, err
			}
			m[text] = value
		}
		return m, nil
	case map[string]any:
		for k, item := range x {
			value, err := yamlTextKeys(item)
			if err != nil {
				return nil, err
			}
			x[k] = value
		}
		return x, nil
	case []any:
		for i, item := range x {
			value, err := yamlTextKeys(item)
			if err != nil {
				return nil, err
			}
			x[i] = value
		}
		return x, nil
	}
	return v, nil
}
