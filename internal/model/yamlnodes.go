package model

import (
	"errors"
	"fmt"
	"reflect"
	"strings"

	"gopkg.in/yaml.v3"
)

// Errors a custom UnmarshalYAML returns as a *yaml.TypeError are collected
// with the decoder's own, line numbers and all, and decoding carries on, so a
// file with several problems reports every one of them in one go. They use the
// decoder's wording, which config.Load turns into the configuration's terms.

// lineError is a decoding error at the node's line.
func lineError(n *yaml.Node, format string, args ...any) error {
	return &yaml.TypeError{Errors: []string{fmt.Sprintf("line %d: ", n.Line) + fmt.Sprintf(format, args...)}}
}

// describeNode names what a node is, for an error saying it is not what was
// expected.
func describeNode(n *yaml.Node) string {
	switch n.Kind {
	case yaml.SequenceNode:
		return "a list"
	case yaml.MappingNode:
		return "a mapping"
	}
	return fmt.Sprintf("%q", n.Value)
}

// notAMapping says a node is not the mapping the named type is decoded from, in
// the decoder's own words, which config.Load turns into the file's terms.
func notAMapping(n *yaml.Node, name string) error {
	value := ""
	if n.Kind == yaml.ScalarNode {
		value = n.Value
		if len(value) > 10 {
			value = value[:7] + "..."
		}
		value = " `" + value + "`"
	}
	return lineError(n, "cannot unmarshal %s%s into %s", n.ShortTag(), value, name)
}

// decodeSwitchedBlock decodes a block that does nothing until its enabled
// key turns it on, such as otlp and web.basic_auth, into out, a pointer to
// the block's struct without its UnmarshalYAML. Settings without the switch
// would be ignored without a word — credentials that protect nothing, an
// endpoint nothing is exported to — so a block that sets any key but enabled
// is refused: enabled: true turns it on, and enabled: false says the settings
// are kept on purpose. name is the block's Go type, as the decoder's errors
// spell it, and block what the file calls it.
func decodeSwitchedBlock(n *yaml.Node, out any, name, block string) error {
	if n.Kind != yaml.MappingNode {
		return notAMapping(n, name)
	}
	var problems []string
	collect := func(err error) error {
		var typeErr *yaml.TypeError
		if errors.As(err, &typeErr) {
			problems = append(problems, typeErr.Errors...)
			return nil
		}
		return err
	}
	// Node.Decode does not refuse unknown keys the way the file's decoder
	// does, so they are checked here, or a misspelt key would be ignored.
	if err := collect(checkKnownKeys(n, reflect.TypeOf(out).Elem(), name)); err != nil {
		return err
	}
	if err := collect(n.Decode(out)); err != nil {
		return err
	}
	// The keys and the switch are those the decoder reads (DecodedEntries):
	// an enabled the block leaves empty is the one it has, whatever a
	// mapping merged in says.
	var others []*yaml.Node
	switched := false
	for _, entry := range DecodedEntries(n) {
		switch {
		case KeyName(entry.Key) != "enabled":
			others = append(others, entry.Key)
		case entry.Value != nil && entry.Value.ShortTag() != nullTag:
			switched = true
		}
	}
	if !switched && len(others) > 0 {
		keys := make([]string, 0, len(others))
		for _, key := range others {
			keys = append(keys, KeyName(key))
		}
		problems = append(problems, fmt.Sprintf("line %d: %s sets %s but not enabled; say enabled: true to turn it on, or enabled: false to keep the settings without using them", others[0].Line, block, joinWithAnd(keys)))
	}
	if len(problems) > 0 {
		return &yaml.TypeError{Errors: problems}
	}
	return nil
}

// joinWithAnd writes names as "a", "a and b" and "a, b and c".
func joinWithAnd(names []string) string {
	if len(names) < 2 {
		return strings.Join(names, "")
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}

var unmarshalerType = reflect.TypeOf((*yaml.Unmarshaler)(nil)).Elem()

// mergeTag is the tag YAML gives an unquoted << key: the mappings its value
// names, one or a list of them, supply the keys the mapping does not set
// itself. A quoted "<<" is a key like any other.
const mergeTag = "!!merge"

// nullTag and stringTag are the tags of a scalar YAML reads as no value at
// all, and as text.
const (
	nullTag   = "!!null"
	stringTag = "!!str"
)

// resolveAlias is the node an alias stands for, and any other node itself.
func resolveAlias(n *yaml.Node) *yaml.Node {
	for n != nil && n.Kind == yaml.AliasNode {
		n = n.Alias
	}
	return n
}

// KeyName is the key of a mapping as the decoder reads it: the text it is
// written as, or, of a key that is an alias (*name: value), the text of the
// node the anchor holds, which is the key the value is decoded into, and not
// the anchor's name, which the alias node holds as its value. Every check of
// a mapping's keys written by hand reads a key's text through it, so an
// alias key is the key it stands for wherever the decoder takes it for one.
// An alias of <<, which the decoder reads as the text "<<" rather than as a
// merge, is that text here too: only a << written out merges (mergeTag).
func KeyName(key *yaml.Node) string {
	if named := resolveAlias(key); named != nil {
		return named.Value
	}
	return key.Value
}

// MappingEntry is a key of a mapping and its value.
type MappingEntry struct{ Key, Value *yaml.Node }

// MappingEntries is every key a mapping holds with its value: its own and,
// in the place of each merge key (<<: *base, or <<: [*a, *b]), those of the
// mappings merged in, as the decoder reads them. Every check of a mapping's
// keys written by hand goes through it, so a merged key is a key wherever the
// decoder takes it for one. A value that is an alias is the node it names.
func MappingEntries(n *yaml.Node) []MappingEntry {
	var entries []MappingEntry
	visiting := map[*yaml.Node]bool{}
	var collect func(n *yaml.Node)
	collect = func(n *yaml.Node) {
		n = resolveAlias(n)
		// A mapping that merges itself in is refused by the decoder; here it
		// only must not be followed forever.
		if n == nil || n.Kind != yaml.MappingNode || visiting[n] {
			return
		}
		visiting[n] = true
		defer delete(visiting, n)
		for i := 0; i+1 < len(n.Content); i += 2 {
			key, value := n.Content[i], resolveAlias(n.Content[i+1])
			if key.Kind != yaml.ScalarNode || key.Tag != mergeTag {
				entries = append(entries, MappingEntry{key, value})
				continue
			}
			if value != nil && value.Kind == yaml.SequenceNode {
				for _, merged := range value.Content {
					collect(merged)
				}
				continue
			}
			collect(value)
		}
	}
	collect(n)
	return entries
}

// DecodedEntries is the entries of a mapping the decoder uses, in the order
// it reads them: the mapping's own, then those merged in. Where MappingEntries
// is every key a mapping could take a value from, this is each key with the
// value it does take: a key the mapping sets itself is not taken from a
// merged mapping, wherever in the mapping the merge key stands, and a key
// that several merged mappings set is taken from the first — the first of a
// list (<<: [*a, *b]), and a merged mapping's own before what is merged into
// it in turn. A check of a mapping's values goes through it, so a value an
// anchor holds and the mapping overrides, which the decoder never reads there,
// is not held against it.
//
// Three things follow the decoder rather than the eye. A key YAML reads as no
// key at all (null, ~) sets nothing, so none stands for another and each is
// returned. The decoder knows a mapping's own keys by what YAML reads
// them as, so one that is not text — 1, true — does not keep out a merged key
// of the same spelling, whose value replaces its own: both are returned. And
// a key that is an alias (*name: value) is the key the anchor holds, not the
// anchor's name, and keeps out what that key does; it is returned as it is
// written, at its own line, for a check to resolve (resolveAlias).
func DecodedEntries(n *yaml.Node) []MappingEntry {
	var entries []MappingEntry
	taken := map[string]bool{}
	visiting := map[*yaml.Node]bool{}
	var collect func(n *yaml.Node, merged bool)
	collect = func(n *yaml.Node, merged bool) {
		n = resolveAlias(n)
		// As in MappingEntries: a mapping that merges itself in is refused by
		// the decoder, and must only not be followed forever.
		if n == nil || n.Kind != yaml.MappingNode || visiting[n] {
			return
		}
		visiting[n] = true
		defer delete(visiting, n)
		// The decoder reads the mapping's keys before what it merges in, and,
		// of several merge keys, which it refuses, merges the last.
		var merge *yaml.Node
		for i := 0; i+1 < len(n.Content); i += 2 {
			key, value := n.Content[i], resolveAlias(n.Content[i+1])
			if key.Kind == yaml.ScalarNode && key.Tag == mergeTag {
				merge = value
				continue
			}
			if named := resolveAlias(key); named != nil && named.Kind == yaml.ScalarNode && named.ShortTag() != nullTag {
				if merged && taken[named.Value] {
					continue
				}
				if merged || named.ShortTag() == stringTag {
					taken[named.Value] = true
				}
			}
			entries = append(entries, MappingEntry{key, value})
		}
		if merge != nil && merge.Kind == yaml.SequenceNode {
			for _, each := range merge.Content {
				collect(each, true)
			}
			return
		}
		collect(merge, true)
	}
	collect(n, false)
	return entries
}

// checkKnownKeys refuses a key of a mapping, at any depth, that the struct it
// is decoded into has no field for, with the decoder's own message. Types
// that decode themselves check their own keys. name is what the message calls
// t, which is a local type when checked from t's own UnmarshalYAML. The keys
// are those the decoder reads: an alias is what it names, a key that is an
// alias the key it names (KeyName), and a merge key supplies the keys of the
// mappings it merges in (MappingEntries).
func checkKnownKeys(n *yaml.Node, t reflect.Type, name string) error {
	var problems []string
	visiting := map[*yaml.Node]bool{}
	var walk func(n *yaml.Node, t reflect.Type, top bool)
	walk = func(n *yaml.Node, t reflect.Type, top bool) {
		n = resolveAlias(n)
		// A node that holds itself is refused by the decoder.
		if n == nil || visiting[n] {
			return
		}
		visiting[n] = true
		defer delete(visiting, n)
		for t.Kind() == reflect.Pointer {
			t = t.Elem()
		}
		if !top && reflect.PointerTo(t).Implements(unmarshalerType) {
			return
		}
		switch t.Kind() {
		case reflect.Struct:
			if n.Kind != yaml.MappingNode {
				return
			}
			fields := map[string]reflect.Type{}
			for i := 0; i < t.NumField(); i++ {
				key, _, _ := strings.Cut(t.Field(i).Tag.Get("yaml"), ",")
				if key != "" && key != "-" {
					fields[key] = t.Field(i).Type
				}
			}
			for _, entry := range MappingEntries(n) {
				key := entry.Key
				field, ok := fields[KeyName(key)]
				if !ok {
					typeName := t.String()
					if top {
						typeName = name
					}
					problems = append(problems, fmt.Sprintf("line %d: field %s not found in type %s", key.Line, KeyName(key), typeName))
					continue
				}
				walk(entry.Value, field, false)
			}
		case reflect.Slice:
			if n.Kind == yaml.SequenceNode {
				for _, item := range n.Content {
					walk(item, t.Elem(), false)
				}
			}
		case reflect.Map:
			if n.Kind == yaml.MappingNode {
				for _, entry := range MappingEntries(n) {
					walk(entry.Value, t.Elem(), false)
				}
			}
		}
	}
	walk(n, t, true)
	if len(problems) > 0 {
		return &yaml.TypeError{Errors: problems}
	}
	return nil
}
