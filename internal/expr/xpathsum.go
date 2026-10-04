package expr

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// The XPath engine's sum() adds up the nodes whose text it reads as a number
// and leaves the others out without a word: `1,234`, `n/a`, and a number
// with blanks around it, as pretty-printed markup and the cells of an HTML
// table have. The sum of a column was then a wrong number, often 0, and no
// error, where XPath says it is NaN. The engine cannot be told otherwise, so
// the exporter reads the nodes of a sum() itself (transform/xpathsum.go),
// and for that the calls are found here, once for an expression, when it is
// compiled.
//
// A call is found where it is evaluated in the expression's own context:
// outside every predicate. The argument of such a call selects the same
// nodes compiled on its own and evaluated where the expression is. Inside a
// predicate the context is each node the predicate is asked of, which only
// the engine walks, so a sum() there stays the engine's alone.

// XPathSum is one call of sum() in an expression's own context: its argument
// as written, and compiled as an expression of its own.
type XPathSum struct {
	Argument string
	Program  *XPathProgram
}

// XPathSums are the calls of sum() in an expression's own context. Whole is
// the call that is the whole expression, `sum(//size)`, nil when the
// expression is more than one call; Parts are the others, each a part of a
// larger expression, as in `sum(//size) div count(//size)`.
type XPathSums struct {
	Whole *XPathSum
	Parts []XPathSum
}

// Sums are the calls of sum() in the expression's own context, nil when it
// has none, as nearly every expression has.
func (p *XPathProgram) Sums() *XPathSums { return p.sums }

// findXPathSums finds the calls of sum() in the own context of an expression
// that compiled, and compiles the argument of each. A call whose argument
// does not compile on its own is left out, and so left to the engine: the
// scan below reads an expression as the engine does only where the engine
// takes it for what XPath says it is.
func findXPathSums(expression string, namespaces map[string]string) *XPathSums {
	if !strings.Contains(expression, "sum") {
		return nil
	}
	calls := scanXPathSums(expression)
	if len(calls) == 0 {
		return nil
	}
	sums := &XPathSums{}
	for i, call := range calls {
		argument := strings.TrimSpace(expression[call.argument:call.argumentEnd])
		program, err := newXPathProgram(argument, namespaces)
		if err != nil {
			continue
		}
		// The first call starts first, so only it can be the whole
		// expression, blanks around it aside, and a prefix before its name,
		// which the engine passes over: fn:sum(//size) is sum(//size) to it.
		if i == 0 && expression[call.start:call.end] == xpathUnprefixed(strings.TrimSpace(expression)) {
			sums.Whole = &XPathSum{Argument: argument, Program: program}
			continue
		}
		sums.Parts = append(sums.Parts, XPathSum{Argument: argument, Program: program})
	}
	if sums.Whole == nil && len(sums.Parts) == 0 {
		return nil
	}
	return sums
}

// xpathUnprefixed is an expression without the prefix it starts with, a
// name and a colon before another name, as fn:sum(//size) has one.
func xpathUnprefixed(expression string) string {
	colon := strings.IndexByte(expression, ':')
	if colon <= 0 || strings.HasPrefix(expression[colon+1:], ":") {
		return expression
	}
	for at, r := range expression[:colon] {
		if !xpathNameStart(r) && (at == 0 || !strings.ContainsRune("0123456789-.", r)) {
			return expression
		}
	}
	return expression[colon+1:]
}

// xpathSumCall is where a call of sum() stands in an expression: the call
// from its name to its closing parenthesis, and its argument.
type xpathSumCall struct {
	start, end            int
	argument, argumentEnd int
}

// scanXPathSums finds the calls of sum() outside every predicate of an
// expression, in the order they start, nested ones after the one around
// them. It reads the expression as the engine's scanner does: a string
// literal is passed over, a name is taken whole, so that checksum( and
// my-sum( are other names, a prefix before the name is left aside as the
// engine leaves it (fn:sum), and blanks may stand between the name and its
// parenthesis. A name after a slash, an axis, an @ or a $ is a step, an
// attribute or a variable and no function. The argument is what stands
// before the first comma of the call, since the engine adds up its first
// argument and passes over the rest. An expression whose parentheses or
// brackets do not pair has no calls.
func scanXPathSums(expression string) []xpathSumCall {
	// open holds, for each parenthesis that is open, the index in calls of
	// the sum() it opened, -1 for any other; brackets counts the predicates
	// that are open; and before is the token before the one read, which
	// says whether a name can be a function: an axis is noted as axis.
	var (
		calls    []xpathSumCall
		open     []int
		brackets int
		before   rune
	)
	const axis = -1
	for i := 0; i < len(expression); {
		r, size := utf8.DecodeRuneInString(expression[i:])
		switch {
		case unicode.IsSpace(r):
			i += size
			continue
		case r == '\'' || r == '"':
			closing := strings.IndexByte(expression[i+1:], byte(r))
			if closing < 0 {
				return nil
			}
			i += closing + 2
			before = r
			continue
		case r == '[':
			brackets++
		case r == ']':
			brackets--
		case r == '(':
			open = append(open, -1)
		case r == ')':
			if len(open) == 0 {
				return nil
			}
			if call := open[len(open)-1]; call >= 0 {
				if calls[call].argumentEnd < 0 {
					calls[call].argumentEnd = i
				}
				calls[call].end = i + 1
			}
			open = open[:len(open)-1]
		case r == ',':
			if len(open) > 0 && brackets == 0 {
				if call := open[len(open)-1]; call >= 0 && calls[call].argumentEnd < 0 {
					calls[call].argumentEnd = i
				}
			}
		case r == ':' && strings.HasPrefix(expression[i+1:], ":"):
			i += 2
			before = axis
			continue
		case xpathNameStart(r):
			end := i + size
			for end < len(expression) {
				next, nextSize := utf8.DecodeRuneInString(expression[end:])
				if !xpathNameStart(next) && !strings.ContainsRune("0123456789-.", next) {
					break
				}
				end += nextSize
			}
			parenthesis := end
			for parenthesis < len(expression) {
				next, nextSize := utf8.DecodeRuneInString(expression[parenthesis:])
				if !unicode.IsSpace(next) {
					break
				}
				parenthesis += nextSize
			}
			function := before != '/' && before != '@' && before != '$' && before != axis
			if expression[i:end] == "sum" && function && brackets == 0 && strings.HasPrefix(expression[parenthesis:], "(") {
				open = append(open, len(calls))
				calls = append(calls, xpathSumCall{start: i, argument: parenthesis + 1, argumentEnd: -1})
				i = parenthesis + 1
				before = '('
				continue
			}
			i = end
			before = 'n'
			continue
		}
		before = r
		i += size
	}
	if len(open) > 0 || brackets != 0 {
		return nil
	}
	return calls
}

// xpathNameStart reports whether r starts a name to the engine's scanner: a
// letter, an underscore, and every character beyond ASCII that is no blank,
// which is more than the engine takes for a name, and so never cuts a name
// short of where the engine ends it. A digit, a hyphen and a dot go on a
// name and do not start one.
func xpathNameStart(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r == '_' || r >= utf8.RuneSelf && !unicode.IsSpace(r)
}
