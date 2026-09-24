package model

import (
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

var unmarshalerType = reflect.TypeOf((*yaml.Unmarshaler)(nil)).Elem()

// checkKnownKeys refuses a key of a mapping, at any depth, that the struct it
// is decoded into has no field for, with the decoder's own message. Types
// that decode themselves check their own keys. name is what the message calls
// t, which is a local type when checked from t's own UnmarshalYAML.
func checkKnownKeys(n *yaml.Node, t reflect.Type, name string) error {
	var problems []string
	var walk func(n *yaml.Node, t reflect.Type, top bool)
	walk = func(n *yaml.Node, t reflect.Type, top bool) {
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
			for i := 0; i+1 < len(n.Content); i += 2 {
				key := n.Content[i]
				field, ok := fields[key.Value]
				if !ok {
					typeName := t.String()
					if top {
						typeName = name
					}
					problems = append(problems, fmt.Sprintf("line %d: field %s not found in type %s", key.Line, key.Value, typeName))
					continue
				}
				walk(n.Content[i+1], field, false)
			}
		case reflect.Slice:
			if n.Kind == yaml.SequenceNode {
				for _, item := range n.Content {
					walk(item, t.Elem(), false)
				}
			}
		case reflect.Map:
			if n.Kind == yaml.MappingNode {
				for i := 1; i < len(n.Content); i += 2 {
					walk(n.Content[i], t.Elem(), false)
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
