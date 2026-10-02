package config

import (
	"errors"
	"fmt"
	"math"
	"reflect"
	"slices"
	"strconv"
	"strings"

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
	var walk func(n *yaml.Node, t reflect.Type, key string)
	walk = func(n *yaml.Node, t reflect.Type, key string) {
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
				if field, ok := fields[entry.Key.Value]; ok {
					walk(entry.Value, field, entry.Key.Value)
				}
			}
		case reflect.Slice:
			if n.Kind == yaml.SequenceNode {
				for _, item := range n.Content {
					walk(item, t.Elem(), key)
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
				walk(entry.Value, t.Elem(), key)
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
	walk(doc.Content[0], t, "the document")
	return problems
}
