package model

import (
	"cmp"
	"reflect"
	"slices"
	"strconv"
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

// placedDecodeProblems are the messages of decoding n into a value of type
// t, each placed at the key, or the entry of a list, whose value it is
// about, so that problems sort in the order of the file (keyProblem) where
// the decoder says only a line: on one line, `limits: {bogus: 1,
// max_metrics: x}` has a key refused before a value of the wrong kind,
// which the decoder said at the line alone and which sorted by the place of
// the mapping around both came first. The messages are those of decoding n
// whole, which is what decides; decoding it again piece by piece
// (decodedPieces) tells where each is, and one no piece gives is placed at
// at, n's key, or on the line it names where that is another.
func placedDecodeProblems(n *yaml.Node, t reflect.Type, at keyAt) []keyProblem {
	messages := errorMessages(n.Decode(reflect.New(t).Interface()))
	if len(messages) == 0 {
		return nil
	}
	problems, _ := placeMessages(messages, decodedPieces(n, t, at), at)
	return problems
}

// decodedPieces are the messages of decoding n into a value of type t
// entry by entry, as far down as t is a struct, a list or a map that the
// decoder reads field by field and n a mapping or a list, each placed at
// the key or the entry of a list whose value it is about; a type that
// decodes itself, and a value of any other kind, is decoded whole and its
// messages placed at at, or on the line a message names where that is
// another. A key t has no field for is passed over: it is an unknown key,
// which unknownKeys places.
func decodedPieces(n *yaml.Node, t reflect.Type, at keyAt) []keyProblem {
	var problems []keyProblem
	visiting := map[*yaml.Node]bool{}
	var walk func(n *yaml.Node, t reflect.Type, at keyAt)
	walk = func(n *yaml.Node, t reflect.Type, at keyAt) {
		n = resolveAlias(n)
		// A node that holds itself is refused by the decoder, decoding it
		// whole.
		if n == nil || visiting[n] {
			return
		}
		visiting[n] = true
		defer delete(visiting, n)
		elem := t
		for elem.Kind() == reflect.Pointer {
			elem = elem.Elem()
		}
		plain := !reflect.PointerTo(elem).Implements(unmarshalerType) && elem != nodeType
		switch {
		case plain && elem.Kind() == reflect.Struct && n.Kind == yaml.MappingNode:
			for _, entry := range DecodedEntries(n) {
				if field, ok := fieldTypeByKey(elem, KeyName(entry.Key)); ok {
					walk(entry.Value, field, keyAt{"", entry.Key.Line, entry.Key.Column})
				}
			}
			return
		case plain && elem.Kind() == reflect.Slice && n.Kind == yaml.SequenceNode:
			for _, item := range n.Content {
				walk(item, elem.Elem(), keyAt{"", item.Line, item.Column})
			}
			return
		case plain && elem.Kind() == reflect.Map && n.Kind == yaml.MappingNode:
			for _, entry := range DecodedEntries(n) {
				walk(entry.Value, elem.Elem(), keyAt{"", entry.Key.Line, entry.Key.Column})
			}
			return
		}
		for _, message := range errorMessages(n.Decode(reflect.New(t).Interface())) {
			problems = append(problems, placedAt(at, message))
		}
	}
	walk(n, t, at)
	return problems
}

// placedAt is a message placed at a key, or on the line it names, at its
// start, where that is another than the key's.
func placedAt(at keyAt, message string) keyProblem {
	if named, ok := messageLine(message); ok && named != at.line {
		return keyProblem{named, 0, message}
	}
	return keyProblem{at.line, at.column, message}
}

// placeMessages places each of the decoder's messages where the first
// piece (decodedPieces) of the same text not yet taken is, or at at when
// none is, and says of each which piece it was placed at, -1 for none.
// Every message is placed once and no other is added, so decoding piece by
// piece, which may see what decoding whole did not reach, changes no
// message, only where it is said.
func placeMessages(messages []string, pieces []keyProblem, at keyAt) ([]keyProblem, []int) {
	taken := make([]bool, len(pieces))
	problems := make([]keyProblem, 0, len(messages))
	from := make([]int, 0, len(messages))
	for _, message := range messages {
		place, piece := placedAt(at, message), -1
		for i := range pieces {
			if !taken[i] && pieces[i].message == message {
				taken[i], place, piece = true, pieces[i], i
				break
			}
		}
		problems = append(problems, place)
		from = append(from, piece)
	}
	return problems, from
}

// fieldTypeByKey is the type of the field of a struct type that holds a
// key.
func fieldTypeByKey(t reflect.Type, key string) (reflect.Type, bool) {
	for i := 0; i < t.NumField(); i++ {
		if name, _, _ := strings.Cut(t.Field(i).Tag.Get("yaml"), ","); name == key && t.Field(i).IsExported() {
			return t.Field(i).Type, true
		}
	}
	return nil, false
}

// sortProblems sorts problems in the order of the file, by line and then
// by column, those at one place keeping the order they were found in.
func sortProblems(problems []keyProblem) {
	slices.SortStableFunc(problems, func(a, b keyProblem) int {
		return cmp.Or(cmp.Compare(a.line, b.line), cmp.Compare(a.column, b.column))
	})
}

// messageLine is the line a decoder's message names at its start.
func messageLine(message string) (int, bool) {
	m := linePrefix.FindStringSubmatch(message)
	if m == nil {
		return 0, false
	}
	line, err := strconv.Atoi(m[1])
	return line, err == nil
}
