package transform

import (
	"strings"
	"unicode"
)

// A label is evaluated at every node its rule selects. A label that is an
// absolute path, such as `//last/@id` or `count(//row)`, walks the document
// from the top each time. Over four thousand rows the document was walked
// four thousand times, taking seconds of a probe, for a value that is the
// same at every row: nothing in such an expression is relative to the node.
//
// So a label whose expression cannot depend on the selected node is
// evaluated once per rule and response, at the first node it is asked for.
// Every series gets that value: the text or absence the engine gave there
// (engineXPathLabel), and for a sum() what the exporter read of its nodes
// (labelXPathSums). A series is therefore made, left without the label, or
// failed exactly as when the label was evaluated at its node, and the
// failure names the node as before.
//
// Whether a label cannot depend on the node is decided from its expression
// when the rule's labels are planned (xpathConstantLabel), and narrowly:
// every expression not known to be constant is evaluated at each node.

// xpathConstant is what a label that cannot depend on the node was read as:
// its text, and found for one that has any; and, for one that is a sum(),
// what readXPathSums gave of its nodes. read and summed say each was read.
type xpathConstant struct {
	read, found bool
	text        string

	summed, own bool
	sum         float64
	fault       *xpathSumFault

	// sumWritten says sumText is the sum's text, written once.
	sumWritten bool
	sumText    string
}

// labelXPathSums is readXPathSums of a label's sums at node: read once for
// a label that cannot depend on the node, and what was read then at every
// other node.
func labelXPathSums[N comparable](nodes xpathNodes[N], node N, label *xpathLabel) (sum float64, own bool, fault *xpathSumFault) {
	once := label.constant
	if once == nil {
		return readXPathSums(nodes, node, label.program.Sums())
	}
	if !once.summed {
		once.sum, once.own, once.fault = readXPathSums(nodes, node, label.program.Sums())
		once.summed = true
	}
	return once.sum, once.own, once.fault
}

// xpathConstantFunctions are the functions whose result depends on nothing
// but their argument: not the node the expression is evaluated at, its
// position or its language.
var xpathConstantFunctions = map[string]bool{
	"count": true, "sum": true, "string": true, "number": true, "boolean": true, "normalize-space": true, "string-length": true,
	"name": true, "local-name": true, "not": true, "round": true, "floor": true, "ceiling": true,
}

// xpathConstantLabel reports whether a label's expression, which compiled,
// cannot depend on the node it is evaluated at. It says yes for only two
// shapes, ignoring surrounding blanks:
//
//   - one absolute location path, `/status/@site` or `//row[v > 1]/name`.
//     It starts with `/` or `//` and contains only steps: no operator,
//     union, comma or call outside its predicates. A predicate is evaluated
//     relative to the path's own nodes, never to the selected one, so
//     anything may appear inside it.
//   - one call of a function of xpathConstantFunctions with one such path
//     as its only argument, `count(//row)`.
//
// It says no to everything else. That includes expressions that do not
// depend on the node either: `concat(/status/@site, '-', @id)`, `//a | b`,
// `//a | //b`, `(//a)[1]` and `string(/a) = 'x'`. A label wrongly taken to
// depend on the node costs what it always cost. One wrongly taken not to
// would be given the first node's value at every other node.
func xpathConstantLabel(expression string) bool {
	expression = strings.TrimFunc(expression, unicode.IsSpace)
	if end, ok := xpathAbsolutePathEnd(expression, 0); ok {
		return end == len(expression)
	}
	if expression == "" || !xpathNameStart(rune(expression[0])) {
		return false
	}
	name := xpathNameEnd(expression, 0)
	open := xpathBlanksEnd(expression, name)
	if !xpathConstantFunctions[expression[:name]] || !strings.HasPrefix(expression[open:], "(") {
		return false
	}
	end, ok := xpathAbsolutePathEnd(expression, xpathBlanksEnd(expression, open+1))
	return ok && end == len(expression)-1 && expression[end] == ')'
}

// xpathAbsolutePathEnd is where the absolute location path that starts at
// expression[i:] ends, past the blanks after it. ok is false where no path
// starts. The path is `/` alone (the document), or `/` or `//` followed by
// steps, with one of the two between each step.
func xpathAbsolutePathEnd(expression string, i int) (end int, ok bool) {
	if !strings.HasPrefix(expression[i:], "/") {
		return 0, false
	}
	for first := true; ; first = false {
		at := xpathBlanksEnd(expression, i)
		if !strings.HasPrefix(expression[at:], "/") {
			return at, true
		}
		at++
		double := strings.HasPrefix(expression[at:], "/")
		if double {
			at++
		}
		step, isStep := xpathStepEnd(expression, xpathBlanksEnd(expression, at))
		switch {
		case isStep:
			i = step
		case first && !double:
			// The document itself.
			return xpathBlanksEnd(expression, at), true
		default:
			return 0, false
		}
	}
}

// xpathStepEnd is where the step of a location path that starts at
// expression[i:] ends, including its predicates. ok is false for anything
// that is not a step. A step is `.` or `..`, or a node test (a name, with or
// without a prefix, `*`, `text()`, `node()` or `comment()`) optionally
// after an `@` or an axis.
func xpathStepEnd(expression string, i int) (end int, ok bool) {
	switch {
	case i >= len(expression):
		return 0, false
	case expression[i] == '.':
		i++
		if strings.HasPrefix(expression[i:], ".") {
			i++
		} else if xpathDigitsEnd(expression, i) > i {
			// A number, as .5 is.
			return 0, false
		}
		return xpathPredicatesEnd(expression, i)
	case expression[i] == '@':
		i = xpathBlanksEnd(expression, i+1)
	case xpathNameStart(rune(expression[i])):
		// An axis is a name before two colons, which may stand apart.
		if colons := xpathBlanksEnd(expression, xpathNameEnd(expression, i)); strings.HasPrefix(expression[colons:], "::") {
			i = xpathBlanksEnd(expression, colons+2)
		}
	}
	// The node test.
	switch {
	case i >= len(expression):
		return 0, false
	case expression[i] == '*':
		i++
	case xpathNameStart(rune(expression[i])):
		start := i
		i = xpathNameEnd(expression, i)
		name := expression[start:i]
		if strings.HasPrefix(expression[i:], ":") {
			// prefix:name and prefix:* are one name.
			name, i = "", i+1
			if local := xpathNameEnd(expression, i); local > i {
				i = local
			} else if strings.HasPrefix(expression[i:], "*") {
				i++
			} else {
				return 0, false
			}
		}
		if open := xpathBlanksEnd(expression, i); strings.HasPrefix(expression[open:], "(") {
			// A node type's test, and no call of a function.
			closing := xpathBlanksEnd(expression, open+1)
			if name != "text" && name != "node" && name != "comment" || !strings.HasPrefix(expression[closing:], ")") {
				return 0, false
			}
			i = closing + 1
		}
	default:
		return 0, false
	}
	return xpathPredicatesEnd(expression, i)
}

// xpathPredicatesEnd is where the predicates that follow a step at
// expression[i:] end, or where they would start when there are none. A
// predicate ends at the bracket that closes it, skipping the predicates and
// strings inside it. ok is false for a predicate that is not closed.
func xpathPredicatesEnd(expression string, i int) (end int, ok bool) {
	for {
		open := xpathBlanksEnd(expression, i)
		if !strings.HasPrefix(expression[open:], "[") {
			return i, true
		}
		depth := 0
		for i = open; i < len(expression); i++ {
			switch c := expression[i]; c {
			case '\'', '"':
				closing := strings.IndexByte(expression[i+1:], c)
				if closing < 0 {
					return 0, false
				}
				i += closing + 1
			case '[':
				depth++
			case ']':
				depth--
			}
			if depth == 0 {
				break
			}
		}
		if depth != 0 {
			return 0, false
		}
		i++
	}
}
