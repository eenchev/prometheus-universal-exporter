package config

import (
	"errors"
	"fmt"
	"math"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"gopkg.in/yaml.v3"
)

// The YAML decoder turns some values into others without a word: a number
// with a fraction where a whole number belongs loses the fraction, so
// max_metrics: 0.5 is 0, the default, and max_concurrent_probes: 1.9 is 1;
// and a key YAML reads as no value at all — null, ~ or nothing before the
// colon — is dropped from a mapping with its value, so value_map:
// {null: 0} maps nothing. Neither is what the file says, so both are
// refused, naming the key, the value and the line, beside whatever the
// decoder itself refuses.
//
// So is a value YAML reads as none (noValue): nothing after a colon or a
// dash, null or ~. The decoder drops such an entry of a list, so that a rule
// meant under a bare dash is missing with nothing said, leaves the key of
// such a value as if it were not written, and takes such a value of a
// mapping for "" or for 0, which value_map: {up: } then maps up to. The
// schemas refuse each, a null being none of the types a key takes, and so
// does the load, at every key, list and mapping the files have: the walk is
// of the types the schemas are generated from (configSchema), so a key added
// to them is held to it without anyone remembering to.
//
// And so is a value YAML reads as another kind than its key takes, which the
// decoder takes all the same. It hands a key of text the text of whatever is
// written, so name: true is the name "true" and delimiter: 1 the character
// 1, and it reads the words YAML 1.1 had for a boolean — yes, no, on, off, y
// and n — as one where a key takes a boolean, text though they are to YAML
// now and to anything else that reads the file. The schemas say which keys
// take what: a key of free text, an expression among them, takes a number
// or a boolean as the text it spells, as the decoder reads it, so that
// value: 1 and description: 404 need no quotes; a key the schemas hold to
// text alone — one with allowed values or a pattern, which is a name, a
// host or a word of a few — takes none, and a boolean is true or false. The
// load holds each key to what its schema says (schemaKinds), so that an
// editor and the exporter flag the same value: a number or a boolean where
// text alone is taken is refused, saying to quote it if the text is meant,
// or, where the key takes one of a few words, which they are; and a word
// that is no boolean where one is taken is refused, saying which of true
// and false it would have been read as.

// valueKinds is what a schema says of a place of its file where a value is
// written, and of the places under it: whether the place takes text alone,
// the values it is then held to, where it is held to some, and what the
// keys of a block there, the entries of a list and the values of a mapping
// take in their turn.
type valueKinds struct {
	textOnly        bool
	allowed         []string
	keys            map[string]*valueKinds
	entries, values *valueKinds
}

// A place the schema does not have takes what the decoder takes.
func (k *valueKinds) key(name string) *valueKinds {
	if k == nil {
		return nil
	}
	return k.keys[name]
}

func (k *valueKinds) entry() *valueKinds {
	if k == nil {
		return nil
	}
	return k.entries
}

func (k *valueKinds) value() *valueKinds {
	if k == nil {
		return nil
	}
	return k.values
}

// kindsOf reads out of a schema which of its places take text alone: the
// ones whose type is string and nothing else, which schemaFor gives a key
// with allowed values or a pattern and a rule gives a key by saying so.
func kindsOf(schema map[string]any) *valueKinds {
	kinds := &valueKinds{textOnly: schema["type"] == "string"}
	// "" is the key left out (optionalEnum), not a value to write.
	if allowed, _ := schema["enum"].([]string); kinds.textOnly {
		for _, value := range allowed {
			if value != "" {
				kinds.allowed = append(kinds.allowed, value)
			}
		}
	}
	if properties, _ := schema["properties"].(map[string]any); len(properties) > 0 {
		kinds.keys = make(map[string]*valueKinds, len(properties))
		for key, sub := range properties {
			kinds.keys[key] = kindsOf(sub.(map[string]any))
		}
	}
	if items, ok := schema["items"].(map[string]any); ok {
		kinds.entries = kindsOf(items)
	}
	if values, ok := schema["additionalProperties"].(map[string]any); ok {
		kinds.values = kindsOf(values)
	}
	return kinds
}

// schemaKinds is what the schema of a file of type t says its places take,
// read once from the schema the type's rules give, so that a key that comes
// to be held to values or a pattern is held to text by the load without
// being listed here. A type that is no file's has none.
func schemaKinds(t reflect.Type) *valueKinds {
	return kindsByFile()[t]
}

var kindsByFile = sync.OnceValue(func() map[reflect.Type]*valueKinds {
	return map[reflect.Type]*valueKinds{
		reflect.TypeOf(model.Config{}):           kindsOf(configSchema()),
		reflect.TypeOf(collectorFile{}):          kindsOf(collectorFileSchema()),
		reflect.TypeOf(model.StaticTargetFile{}): kindsOf(staticTargetsSchema()),
	}
})

// writtenAt is the place of a value, for a message about what is written
// there: the key, and where the value is an entry of the key's list or a
// value of its mapping, which one.
type writtenAt struct {
	key    string
	entry  int
	mapKey string
	inMap  bool
}

func (at writtenAt) String() string {
	switch {
	case at.entry > 0:
		return fmt.Sprintf("%s entry %d", at.key, at.entry)
	case at.inMap:
		return fmt.Sprintf("%s key %q", at.key, at.mapKey)
	}
	return at.key
}

// yamlBooleanWords are the words the decoder reads as a boolean where a key
// takes one, though YAML reads each as text: what YAML 1.1 had beside true
// and false.
var yamlBooleanWords = map[string]bool{
	"y": true, "Y": true, "yes": true, "Yes": true, "YES": true, "on": true, "On": true, "ON": true,
	"n": false, "N": false, "no": false, "No": false, "NO": false, "off": false, "Off": false, "OFF": false,
}

// withValueProblems adds to a decoding error, or to none, the values of the
// document the decoder would change in silence. t is the type the document
// is decoded into.
func withValueProblems(err error, document []byte, t reflect.Type) error {
	problems := valueProblems(document, t)
	if len(problems) == 0 {
		return err
	}
	var typeErr *yaml.TypeError
	switch {
	case err == nil:
		return &yaml.TypeError{Errors: problems}
	case errors.As(err, &typeErr):
		return &yaml.TypeError{Errors: append(slices.Clone(typeErr.Errors), problems...)}
	}
	// Not YAML, or empty: there is nothing to add to that.
	return err
}

// valueProblems walks the document beside the type it is decoded into, as
// the decoder does: an alias is what it names, and a merge key supplies the
// keys of what it merges in that the mapping does not set itself
// (model.DecodedEntries). Only a value the decoder uses is looked at, where
// it is written: one in an anchor that the mapping merging it in overrides is
// no mistake of that mapping's, and one that is used is reported at its own
// line.
func valueProblems(document []byte, t reflect.Type) []string {
	var doc yaml.Node
	if yaml.Unmarshal(document, &doc) != nil || len(doc.Content) == 0 {
		return nil
	}
	var problems []string
	visiting := map[*yaml.Node]bool{}
	// at is the place for a message, and kinds what its schema says the
	// place takes.
	var walk func(n *yaml.Node, t reflect.Type, at writtenAt, kinds *valueKinds)
	walk = func(n *yaml.Node, t reflect.Type, at writtenAt, kinds *valueKinds) {
		key := at.key
		for n != nil && n.Kind == yaml.AliasNode {
			n = n.Alias
		}
		// A node that holds itself is refused by the decoder.
		if n == nil || visiting[n] {
			return
		}
		visiting[n] = true
		defer delete(visiting, n)
		for t.Kind() == reflect.Pointer {
			t = t.Elem()
		}
		switch t.Kind() {
		case reflect.Struct:
			if n.Kind != yaml.MappingNode {
				return
			}
			fields := map[string]reflect.Type{}
			for i := 0; i < t.NumField(); i++ {
				name, _, _ := strings.Cut(t.Field(i).Tag.Get("yaml"), ",")
				if name != "" && name != "-" {
					fields[name] = t.Field(i).Type
				}
			}
			for _, entry := range model.DecodedEntries(n) {
				field, ok := fields[entry.Key.Value]
				switch {
				case !ok:
				case noValue(entry.Value):
					problems = append(problems, fmt.Sprintf("line %d: %s %s, which YAML reads as no value at all; write its value, or take the key out", entry.Key.Line, entry.Key.Value, noValueAfterColon(entry.Value)))
				default:
					walk(entry.Value, field, writtenAt{key: entry.Key.Value}, kinds.key(entry.Key.Value))
				}
			}
		case reflect.Slice:
			if n.Kind == yaml.SequenceNode {
				for i, item := range n.Content {
					if entry := resolveAlias(item); noValue(entry) {
						how := "is written " + entry.Value
						if entry.Value == "" {
							how = "is a dash with nothing after it"
						}
						problems = append(problems, fmt.Sprintf("line %d: %s entry %d %s, which YAML reads as no value at all, so the entry would be left out without a word; write %s there, or take the entry out", item.Line, key, i+1, how, yamlEntry(t.Elem())))
						continue
					}
					walk(item, t.Elem(), writtenAt{key: key, entry: i + 1}, kinds.entry())
				}
			}
		case reflect.Map:
			if n.Kind != yaml.MappingNode {
				return
			}
			for _, entry := range model.DecodedEntries(n) {
				if entry.Key.Kind == yaml.ScalarNode && entry.Key.ShortTag() == "!!null" {
					written := entry.Key.Value
					if written == "" {
						written = "nothing"
					}
					problems = append(problems, fmt.Sprintf("line %d: %s has the key %s, which YAML reads as no key at all, so the entry would be dropped; to use that text as the key, quote it", entry.Key.Line, key, written))
				}
				if noValue(entry.Value) {
					problems = append(problems, fmt.Sprintf("line %d: %s key %q %s, which YAML reads as no value at all; write its value, or take the key out", entry.Key.Line, key, entry.Key.Value, noValueAfterColon(entry.Value)))
					continue
				}
				walk(entry.Value, t.Elem(), writtenAt{key: key, mapKey: entry.Key.Value, inMap: true}, kinds.value())
			}
		case reflect.String:
			if n.Kind != yaml.ScalarNode || kinds == nil || !kinds.textOnly {
				return
			}
			var reads string
			switch n.ShortTag() {
			case "!!bool":
				reads = "a boolean"
			case "!!int", "!!float":
				reads = "a number"
			default:
				return
			}
			// Quoted, a number or a boolean is no value of a key that
			// takes one of a few words, so that key is told the words.
			advice := fmt.Sprintf("to use that text there, quote it: %q", n.Value)
			if len(kinds.allowed) > 0 {
				advice = "write one of " + strings.Join(kinds.allowed, ", ")
			}
			problems = append(problems, fmt.Sprintf("line %d: %s is written %s, which YAML reads as %s, not as text; %s", n.Line, at, n.Value, reads, advice))
		case reflect.Bool:
			if n.Kind != yaml.ScalarNode || n.ShortTag() != "!!str" {
				return
			}
			if meant, word := yamlBooleanWords[n.Value]; word {
				problems = append(problems, fmt.Sprintf("line %d: %s is written %s, which YAML reads as text, not as a boolean; write %t", n.Line, at, n.Value, meant))
			}
		case reflect.Int, reflect.Int64:
			// A duration and a size read themselves, and say what they take.
			if t == durationType || t == byteSizeType || n.Kind != yaml.ScalarNode || n.ShortTag() != "!!float" {
				return
			}
			if value, err := strconv.ParseFloat(strings.ReplaceAll(n.Value, "_", ""), 64); err == nil && value != math.Trunc(value) {
				problems = append(problems, fmt.Sprintf("line %d: %s is %s, which is not a whole number", n.Line, key, n.Value))
			}
		}
	}
	walk(doc.Content[0], t, writtenAt{key: "the document"}, schemaKinds(t))
	return problems
}

// resolveAlias is the node an alias stands for, and any other node itself.
func resolveAlias(n *yaml.Node) *yaml.Node {
	for n != nil && n.Kind == yaml.AliasNode {
		n = n.Alias
	}
	return n
}

// noValue reports whether a node, an alias followed to what it names, is a
// value YAML reads as none: nothing at all, null or ~, in any of the
// spellings YAML takes for them. Text that is empty, "", is a value.
func noValue(n *yaml.Node) bool {
	return n != nil && n.Kind == yaml.ScalarNode && n.ShortTag() == "!!null"
}

// noValueAfterColon says how a key's value that is none was written.
func noValueAfterColon(value *yaml.Node) string {
	if value.Value == "" {
		return "has nothing after its colon"
	}
	return "is written " + value.Value
}

// yamlEntry names what an entry of a list of t is, for a message that says
// what to write in the place of one that is empty.
func yamlEntry(t reflect.Type) string {
	if place, ok := yamlPlaces[t.Name()]; ok && t.Kind() == reflect.Struct {
		return place
	}
	return "a value"
}
