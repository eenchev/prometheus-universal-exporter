package model

import (
	"cmp"
	"reflect"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// When a request block sets several keys it may not, the error names the
// one that comes first in the file, whatever refuses each: the decoder a
// key of no type, or the validation a key of another type or one of the
// common part the type does not take. The validation reads the block's
// keys from Go values, whose fields are in the order of the code, so the
// block keeps the order the file has its keys in (RequestConfig.Order,
// TargetRequestConfig.Order), and the checks look at the keys in it
// (InFileOrder).

// keyAt is a key of a block, by its path in the block (retry.codes for
// codes in retry), at its place in the file.
type keyAt struct {
	path         string
	line, column int
}

// before says whether a place in the file comes before another.
func before(line, column int, other keyAt) bool {
	return line < other.line || line == other.line && column < other.column
}

// blockKeys are the keys a block decoded into v sets, and those of each
// mapping it holds directly, such as retry's, each with its place in the
// file, in the order the file has them. A key is set when its value is
// (isSet): one written "" or 0 sets nothing, as the checks that refuse a
// key count it, but for the keys written, which are set however they are
// written. A key is where it is written: one a merge key brings in is
// where its anchor holds it.
func blockKeys(n *yaml.Node, v reflect.Value, written ...string) []keyAt {
	var keys []keyAt
	for _, entry := range DecodedEntries(n) {
		name := KeyName(entry.Key)
		field, ok := fieldByKey(v, name)
		if !ok {
			continue
		}
		if isSet(field) || slices.Contains(written, name) {
			keys = append(keys, keyAt{name, entry.Key.Line, entry.Key.Column})
		}
		if value := resolveAlias(entry.Value); value != nil && value.Kind == yaml.MappingNode {
			for _, inner := range DecodedEntries(value) {
				if innerField, ok := fieldByKey(field, KeyName(inner.Key)); ok && isSet(innerField) {
					keys = append(keys, keyAt{name + "." + KeyName(inner.Key), inner.Key.Line, inner.Key.Column})
				}
			}
		}
	}
	slices.SortStableFunc(keys, func(a, b keyAt) int {
		return cmp.Or(cmp.Compare(a.line, b.line), cmp.Compare(a.column, b.column))
	})
	return keys
}

// keyOrder is the paths of the keys a block decoded into v sets in the
// order the file has them (blockKeys), nil when it sets none.
func keyOrder(n *yaml.Node, v reflect.Value, written ...string) []string {
	var order []string
	for _, key := range blockKeys(n, v, written...) {
		order = append(order, key.path)
	}
	return order
}

// InFileOrder is keys, paths of keys of a block, in the order the file has
// them, by order, the block's Order. A key order does not hold, as every
// key of a block built in code rather than read, keeps its place among the
// others such keys, after those order holds.
func InFileOrder(keys, order []string) []string {
	if len(order) == 0 || len(keys) < 2 {
		return keys
	}
	place := func(key string) int {
		if i := slices.Index(order, key); i >= 0 {
			return i
		}
		return len(order)
	}
	out := slices.Clone(keys)
	slices.SortStableFunc(out, func(a, b string) int { return cmp.Compare(place(a), place(b)) })
	return out
}

// fieldByKey is the field of a struct, or of the struct a pointer points
// to, that holds a key.
func fieldByKey(v reflect.Value, key string) (reflect.Value, bool) {
	for v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return reflect.Value{}, false
		}
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return reflect.Value{}, false
	}
	for i := 0; i < v.NumField(); i++ {
		if name, _, _ := strings.Cut(v.Type().Field(i).Tag.Get("yaml"), ","); name == key {
			return v.Field(i), true
		}
	}
	return reflect.Value{}, false
}
