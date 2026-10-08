package config

import (
	"errors"
	"fmt"
	"reflect"
	"regexp"
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
//
// And a number YAML reads with a point or an exponent is read by its digits
// where a whole number is taken. The decoder makes a float64 of it and
// converts that, so 9007199254740993.0 became the whole number below it, and
// one past the range of the key's integer wrapped around or was refused as
// no whole number, as the platform's conversion had it: 9.3e18 was a
// negative number on amd64. Such a number is the whole number it is
// written as (model.ReadWholeNumber), as it is to a schema, which is handed
// the number and not how it was written; one the key's integer does not
// hold is refused, naming the key, the number and the range, and so is one
// past the range of a float64, 1e400, which the decoder takes for text
// (pastFloat64); and one with a fraction is refused as it was, however small
// the fraction. A number
// written with digits alone is the decoder's to read, as it was. An entry
// of request.accept_status, which the schemas take as a status written as a
// number or as text, is read as the number it is too (wholeNumberText): the
// decoder handed the exporter the number as it is written, 200.0 or 0xC8,
// which is no status to it, while it is the status 200 to a schema.

// valueKinds is what a schema says of a place of its file where a value is
// written, and of the places under it: whether the place takes text alone,
// the values it is then held to, where it is held to some, and what the
// keys of a block there, the entries of a list and the values of a mapping
// take in their turn.
type valueKinds struct {
	textOnly bool
	allowed  []string
	// number is set of a place of text that takes a whole number too, from
	// least to most: an entry of request.accept_status.
	number          bool
	least, most     int64
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
	if types, _ := schema["type"].([]string); slices.Contains(types, "integer") {
		least, hasLeast := schema["minimum"].(int)
		most, hasMost := schema["maximum"].(int)
		kinds.number, kinds.least, kinds.most = hasLeast && hasMost, int64(least), int64(most)
	}
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
// document the decoder would change in silence. decoded points to what the
// document was decoded into, whose whole numbers written with a point or an
// exponent it writes as the numbers they are, and the entries of
// request.accept_status written as a number as the statuses they are.
func withValueProblems(err error, document []byte, decoded any) error {
	problems, readAgain := checkValues(document, reflect.ValueOf(decoded).Elem())
	var typeErr *yaml.TypeError
	// What the decoder said of a number that is read again here is said
	// here, once, or is no mistake.
	if errors.As(err, &typeErr) && len(readAgain) > 0 {
		kept := slices.DeleteFunc(slices.Clone(typeErr.Errors), func(message string) bool { return readAgain[message] })
		err = nil
		if len(kept) > 0 {
			typeErr = &yaml.TypeError{Errors: kept}
			err = typeErr
		}
	}
	if len(problems) == 0 {
		return err
	}
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
// the decoder does: an alias is what it names, a key that is an alias the
// key it names (model.KeyName), and a merge key supplies the keys of what it
// merges in that the mapping does not set itself (model.DecodedEntries). Only a value the decoder uses is looked at, where
// it is written: one in an anchor that the mapping merging it in overrides is
// no mistake of that mapping's, and one that is used is reported at its own
// line.
func valueProblems(document []byte, t reflect.Type) []string {
	problems, _ := checkValues(document, reflect.New(t).Elem())
	return problems
}

// checkValues is valueProblems of the document decoded into decoded, which
// it walks beside the document, writing into it what is read again here: a
// whole number written with a point or an exponent, and a number at a place
// of text that takes one. readAgain are the decoder's messages of such
// values, which are said here instead.
func checkValues(document []byte, decoded reflect.Value) (problems []string, readAgain map[string]bool) {
	var doc yaml.Node
	if yaml.Unmarshal(document, &doc) != nil || len(doc.Content) == 0 {
		return nil, nil
	}
	readAgain = map[string]bool{}
	visiting := map[*yaml.Node]bool{}
	// at is the place for a message, kinds what its schema says the place
	// takes, and out the value decoded there, which is not valid where
	// nothing was.
	var walk func(n *yaml.Node, t reflect.Type, out reflect.Value, at writtenAt, kinds *valueKinds)
	walk = func(n *yaml.Node, t reflect.Type, out reflect.Value, at writtenAt, kinds *valueKinds) {
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
			if out.IsValid() {
				out = out.Elem()
			}
		}
		switch t.Kind() {
		case reflect.Struct:
			if n.Kind != yaml.MappingNode {
				return
			}
			fields := map[string]int{}
			for i := 0; i < t.NumField(); i++ {
				name, _, _ := strings.Cut(t.Field(i).Tag.Get("yaml"), ",")
				if name != "" && name != "-" {
					fields[name] = i
				}
			}
			for _, entry := range model.DecodedEntries(n) {
				name := model.KeyName(entry.Key)
				field, ok := fields[name]
				switch {
				case !ok:
				case noValue(entry.Value):
					problems = append(problems, fmt.Sprintf("line %d: %s %s, which YAML reads as no value at all; write its value, or take the key out", entry.Key.Line, name, noValueAfterColon(entry.Value)))
				default:
					var value reflect.Value
					if out.IsValid() {
						value = out.Field(field)
					}
					walk(entry.Value, t.Field(field).Type, value, writtenAt{key: name}, kinds.key(name))
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
					var value reflect.Value
					// The decoder leaves out of the list what it takes for
					// no value, which is refused above: past one, the
					// entries are not where the document has them.
					if out.IsValid() && out.Len() == len(n.Content) {
						value = out.Index(i)
					}
					walk(item, t.Elem(), value, writtenAt{key: key, entry: i + 1}, kinds.entry())
				}
			}
		case reflect.Map:
			if n.Kind != yaml.MappingNode {
				return
			}
			for _, entry := range model.DecodedEntries(n) {
				if named := resolveAlias(entry.Key); named != nil && named.Kind == yaml.ScalarNode && named.ShortTag() == "!!null" {
					written := named.Value
					if written == "" {
						written = "nothing"
					}
					problems = append(problems, fmt.Sprintf("line %d: %s has the key %s, which YAML reads as no key at all, so the entry would be dropped; to use that text as the key, quote it", entry.Key.Line, key, written))
				}
				if noValue(entry.Value) {
					problems = append(problems, fmt.Sprintf("line %d: %s key %q %s, which YAML reads as no value at all; write its value, or take the key out", entry.Key.Line, key, model.KeyName(entry.Key), noValueAfterColon(entry.Value)))
					continue
				}
				// A value of a mapping is not one to write into: no
				// mapping of the files has whole numbers or statuses.
				walk(entry.Value, t.Elem(), reflect.Value{}, writtenAt{key: key, mapKey: model.KeyName(entry.Key), inMap: true}, kinds.value())
			}
		case reflect.String:
			if n.Kind == yaml.ScalarNode && kinds != nil && kinds.number && out.CanSet() {
				if text, ok := wholeNumberText(n, kinds.least, kinds.most); ok {
					out.SetString(text)
				}
				return
			}
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
			if t == durationType || t == byteSizeType || n.Kind != yaml.ScalarNode || (n.ShortTag() != "!!float" && !pastFloat64(n)) {
				return
			}
			whole, isWhole := model.ReadWholeNumber(n.Value)
			if !isWhole && n.ShortTag() != "!!float" {
				return
			}
			if !isWhole {
				// A fraction, however small, is refused as it was; what is
				// no number at all, infinity and not-a-number, is the
				// decoder's to refuse.
				if _, err := strconv.ParseFloat(strings.ReplaceAll(n.Value, "_", ""), 64); err == nil {
					readAgain[decoderRefusal(n, t)] = true
					problems = append(problems, fmt.Sprintf("line %d: %s is %s, which is not a whole number", n.Line, key, n.Value))
				}
				return
			}
			readAgain[decoderRefusal(n, t)] = true
			value, held := whole.Int(t.Bits())
			if !held {
				least := int64(-1) << (t.Bits() - 1)
				problems = append(problems, fmt.Sprintf("line %d: %s is %s, which is past the whole numbers it holds, %d to %d; write a whole number in that range", n.Line, key, n.Value, least, -(least+1)))
				return
			}
			if out.CanSet() {
				out.SetInt(value)
			}
		}
	}
	walk(doc.Content[0], decoded.Type(), decoded, writtenAt{key: "the document"}, schemaKinds(decoded.Type()))
	return problems, readAgain
}

// yamlFloat is a number as YAML 1.2 writes one with a point or an exponent,
// as the YAML decoder matches it, its underscores left out.
var yamlFloat = regexp.MustCompile(`^[-+]?(\.[0-9]+|[0-9]+(\.[0-9]*)?)([eE][-+]?[0-9]+)?$`)

// pastFloat64 reports whether a scalar is a number written with a point or
// an exponent past the range of a float64, such as 1e400, which the YAML
// decoder, failing to make a float64 of it, reads as text. It is plain and
// untagged, for quoted or tagged !!str it is text to YAML too; to YAML 1.2,
// and to a schema in an editor, it is a number.
func pastFloat64(n *yaml.Node) bool {
	return n.Tag == "!!str" && n.Style == 0 && yamlFloat.MatchString(strings.ReplaceAll(n.Value, "_", ""))
}

// decoderRefusal is what the YAML decoder says of a scalar it cannot decode
// into a value of type t, as its decode.go words it, the value cut short.
func decoderRefusal(n *yaml.Node, t reflect.Type) string {
	value := n.Value
	if len(value) > 10 {
		value = value[:7] + "..."
	}
	return fmt.Sprintf("line %d: cannot unmarshal %s `%s` into %s", n.Line, n.ShortTag(), value, t)
}

// wholeNumberText is the text of the whole number a scalar YAML reads as a
// number is, at a place of text that takes a whole number from least to
// most beside text, when it is one in that range: 200.0, 2e2, 0xC8, 0o310
// and 2_00 are 200, which an entry of request.accept_status takes as a
// number, while the text it was handed, as written, is no status. Digits
// alone, with a sign or leading zeros, are the number they are in decimal,
// +200 and 0200 the 200 YAML 1.2 reads them as, where the text as written
// passed the load and matched no status. A number out of the range, or that
// is no whole number, is left as it is written, for what reads the text to
// refuse.
func wholeNumberText(n *yaml.Node, least, most int64) (string, bool) {
	// Text, quoted, is what reads it to take or refuse as it is.
	tag := n.ShortTag()
	if tag != "!!int" && tag != "!!float" {
		return "", false
	}
	var v int64
	switch decimal, err := strconv.ParseInt(n.Value, 10, 64); {
	case err == nil:
		v = decimal
	case tag == "!!int":
		if n.Decode(&v) != nil {
			return "", false
		}
	default:
		whole, isWhole := model.ReadWholeNumber(n.Value)
		var held bool
		if v, held = whole.Int(64); !isWhole || !held {
			return "", false
		}
	}
	if v < least || v > most {
		return "", false
	}
	return strconv.FormatInt(v, 10), true
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
