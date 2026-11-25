package decode

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// A decoded body's keys are text, like the keys of JSON objects. A YAML
// mapping's keys may be numbers, booleans and null, which the YAML library
// decodes into a map of any keys; model.Normalize writes each as its text,
// fmt.Sprint's. Keys written differently can then be one text: 1 and 1.0 are
// both "1", true and True are "true", and a key and an alias of it are the
// key's text. The library tells keys apart by how they are written, so it
// refuses none of them. It silently keeps the later of two keys that decode
// into one value, 16 and 0x10 among them. Of two keys that are only one
// text, Normalize kept whichever the map gave it last, which Go's map order
// picks at random. A key written twice fails the scrape, and so does one
// written twice in different ways. yamlKeysAsText finds those in the parsed
// document, by the lines they are written on, in the library's words for a
// key written twice. yamlTextKeys finds them where a mapping is decoded with
// the keys of mappings merged into it with <<, which only the decoded value
// shows. A null key, which has no text, is refused too, instead of being
// read as "<nil>".

// yamlKeysAsText refuses a document with a null key, or with a mapping two
// of whose keys, written differently, are the same text. It runs after the
// library has decoded the document, so any key written twice the same way
// is already refused. Each mapping is read once, as written, without the
// keys merged into it. Only a mapping with a key that is not text as written
// is looked into, since the library has told apart keys that are all text.
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

// yamlMappingKeysAsText adds to problems those of mapping's keys: a null
// key, and a key that is the text of an earlier one, `line 2: mapping key
// "1" already defined at line 1`. The problem names the key by that text,
// which is what the decoded body would have held twice.
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
// text, as model.Normalize makes it. It refuses a mapping two of whose keys
// are the same text. yamlKeysAsText has already refused that for a mapping
// as written, and a null key anywhere. So these are keys merged into a
// mapping with <<, written differently from the mapping's own (1.0 beside
// 1), which the library keeps both of.
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
