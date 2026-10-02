package transform

import (
	"context"
	"strings"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The rules of a jq or yq collector are usually written about a few lists:
// one rule for each number an item has, each with the same items expression,
// such as .components[] or .data.result[]. Evaluating that expression is a
// run of the jq program over the whole document, and every rule ran it
// again, to be handed the very items the rule before it was handed.
//
// So an items expression that several rules of a collector have is
// evaluated once in a run of the transform. The first rule that has it
// reads its items as it always did, one at a time as the program gives
// them, and keeps each as it goes; the rules after it read what was kept.
// Nothing a rule does with its items changes: the same items in the same
// order, each rule's own value, labels, error mode and count of what it
// carried on without, and an items expression that fails fails for each of
// its rules, with the error it gave the first.
//
// The first rule still stops at the first series past limits.max_metrics,
// so what is kept is bounded by it: no more items are kept than the limit
// left room for series when the rule began. A list with more could never be
// used whole, whichever rule read it, so it is forgotten at that item, the
// first rule goes on reading its program, and the rules after it run the
// expression themselves, as every rule did before.
//
// A rule whose items expression no later rule has, and no earlier one kept,
// runs its program as before and keeps nothing, so a collector whose rules
// each read a list of their own pays nothing for this, not an allocation:
// the only cost is comparing its expression with those of the rules after
// it.

// jqSharing is the items kept in one run of the jq transform, by items
// expression. Every rule of a run reads the same document, with the same
// $root, $status and $headers, so the expression's text is what tells two
// evaluations alike: one text is one compiled program (expr.CompileJQ).
type jqSharing struct {
	lists []*jqItemList
}

// jqItemList is what one items expression gave: its items, in order, and the
// error that ended them, if one did.
type jqItemList struct {
	expression string
	items      []any
	err        error
	// bound is how many items the list may hold, negative for any number.
	bound int
	// complete says the program ran to its end, or to an error, with every
	// item before it kept: only then do other rules read the list.
	complete bool
}

// jqItems gives a rule its items one at a time: from its program, or from
// what an earlier rule kept of the same expression.
type jqItems struct {
	ctx context.Context
	// running is the rule's own program, set by the caller when kept is
	// nil, and keep the list its items are kept in for later rules, if any.
	running interface{ Next() (any, bool) }
	keep    *jqItemList
	// kept is the list an earlier rule made, and at the place in it.
	kept *jqItemList
	at   int
}

// items says where the rule with this items expression reads its items:
// from a list an earlier rule kept, when it returns one with kept set, or
// from the rule's own program, which the caller then starts; later is the
// rules after it, for one of which it keeps the items when one has the same
// expression.
func (s *jqSharing) items(ctx context.Context, expression string, later []model.MetricRule) jqItems {
	for _, list := range s.lists {
		if list.expression != expression {
			continue
		}
		if list.complete {
			return jqItems{ctx: ctx, kept: list}
		}
		// More items than the limit had room for: each rule reads its own.
		return jqItems{}
	}
	for i := range later {
		if later[i].Items == expression {
			list := &jqItemList{expression: expression, bound: seriesRoom(ctx)}
			s.lists = append(s.lists, list)
			return jqItems{keep: list}
		}
	}
	return jqItems{}
}

// next is the rule's next item, as its program's Next gives it: an error is
// an item, and ends them.
func (i *jqItems) next() (item any, more bool) {
	if list := i.kept; list != nil {
		// A running program asks after its context at every step, and
		// gives its error as an item; so does the list, for a deadline that
		// passes while a rule reads it to fail the transform all the same.
		if err := i.ctx.Err(); err != nil {
			return err, true
		}
		switch {
		case i.at < len(list.items):
			i.at++
			return list.items[i.at-1], true
		case i.at == len(list.items) && list.err != nil:
			i.at++
			return list.err, true
		}
		return nil, false
	}
	item, more = i.running.Next()
	if list := i.keep; list != nil {
		err, failed := item.(error)
		switch {
		case !more:
			list.complete = true
		case failed:
			list.err, list.complete = err, true
		case list.bound >= 0 && len(list.items) >= list.bound:
			list.items, i.keep = nil, nil
		default:
			list.items = append(list.items, item)
		}
	}
	return item, more
}

// expected is how many items the rule is about to read, where that is known
// before they are read: those of a kept list, or the length of the array or
// object an expression such as .items[] iterates (jqIterationLength). A list
// being kept is given room for them, within its bound.
func (i *jqItems) expected(data any, expression string) (int, bool) {
	if i.kept != nil {
		return len(i.kept.items), true
	}
	n, ok := jqIterationLength(data, expression)
	if ok && i.keep != nil {
		room := n
		if i.keep.bound >= 0 && i.keep.bound < room {
			room = i.keep.bound
		}
		i.keep.items = make([]any, 0, room)
	}
	return n, ok
}

// jqIterationLength is how many values an expression of the commonest shape
// items has gives for data: a path of plain field names and one iteration,
// as .[], .items[] and .data.result[] are, which gives each element of the
// array there, or each value of the object. ok is false for any other
// expression, and for data that has no array or object there, which the
// program then reports in its own words. It is used to size what the items
// are gathered in and for nothing else, so the program decides what the
// items are, here as everywhere.
func jqIterationLength(data any, expression string) (n int, ok bool) {
	path, iterates := strings.CutSuffix(expression, "[]")
	if !iterates || path == "" {
		return 0, false
	}
	value := data
	if path != "." {
		for path != "" {
			rest, isField := strings.CutPrefix(path, ".")
			if !isField {
				return 0, false
			}
			end := strings.IndexByte(rest, '.')
			if end < 0 {
				end = len(rest)
			}
			object, isObject := value.(map[string]any)
			if !isObject || !jqFieldName(rest[:end]) {
				return 0, false
			}
			value, path = object[rest[:end]], rest[end:]
		}
	}
	switch v := value.(type) {
	case []any:
		return len(v), true
	case map[string]any:
		return len(v), true
	}
	return 0, false
}

// jqFieldName reports whether name is one jq reads as a field after a dot
// without quoting: letters, digits and underscores, not starting with a
// digit.
func jqFieldName(name string) bool {
	if name == "" {
		return false
	}
	for i := range len(name) {
		c := name[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c == '_':
		case i > 0 && c >= '0' && c <= '9':
		default:
			return false
		}
	}
	return true
}
