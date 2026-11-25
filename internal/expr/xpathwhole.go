package expr

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/antchfx/xpath"
)

// The XPath engine's parser (antchfx/xpath, as pinned in go.mod) reads one
// expression from the start of the text and stops where it ends, without
// checking whether the text ended there too. Whatever follows a complete
// expression is silently ignored, so `sum(//a))`, `//a)`, `sum(//a)] + 1`,
// `//a 'x'`, `1 2` and `//a/sum(b)` compiled, and were evaluated as
// `sum(//a)`, `//a`, `sum(//a)`, `//a`, `1` and `//a/sum`: a typo became a
// rule that read something other than what it says.
//
// An expression that the engine would not read to its end is therefore
// refused where expressions are compiled (CompileXPath), which is at load for
// every rule and label. Two signs reveal it, and both are sound: they refuse
// nothing that the engine reads whole, except an expression at the limit of
// its depth (below).
//
//   - A closing parenthesis or bracket that closes nothing. The parser takes
//     a `)` or a `]` only as the end of what an earlier `(` or `[` opened, so
//     one without an opening before it can only be where the parser stopped,
//     or after it. The scan skips strings as the engine's scanner does: a
//     quote starts a string wherever it stands, and the string ends at the
//     next quote of the same kind.
//
//   - The expression in parentheses does not compile. Inside `(` and `)`,
//     the parser reads the same expression in the same way and then expects
//     the `)`, while at the top level it expects nothing: `(//a 'x')` is "an
//     invalid token" to it. Only a `)` could stand there and pass, and that
//     one closes nothing, which the first scan already found.
//
// The parentheses make the expression one level deeper, and the parser reads
// no deeper than its limit. For an expression nested that deeply, the one in
// parentheses fails for its depth before the parser reaches whatever may
// follow, which says nothing. Such an expression is refused as too deeply
// nested, whether or not it is read whole: the limit is 199 levels of
// parentheses, predicates and function arguments, which nothing written for
// a document comes near, and one level is given up so that the rule has no
// exception.
//
// A NUL byte ends the text for the engine's scanner, wherever it stands, and
// separates the expression from its namespaces in the key under which
// compiled expressions are kept, so it is refused before either sees it
// (CompileXPath).

// xpathNotWhole is the failure of the engine's parser on a token it did not
// expect, as it words it, after the text it was given: what the expression
// in parentheses fails with when something follows it.
const xpathNotWhole = " has an invalid token"

// xpathTooDeep is the failure of the engine's parser on an expression nested
// deeper than its limit, as it words it.
const xpathTooDeep = "the xpath query is too complex(depth > 200)"

// xpathPlaceBudget bounds the search for the place where the engine would
// stop reading a refused expression, in bytes of expression compiled. The
// search compiles one prefix of the expression after another, from the
// longest, so it costs the square of the length. An expression of a few
// hundred bytes, longer than any written by hand, is searched whole; for a
// longer one, only as many of its longest prefixes as the budget allows are
// tried, so that one of 100 KB is refused as fast as it is compiled. An
// expression whose place is not found is refused without it.
const xpathPlaceBudget = 32 << 10

// checkXPathNUL refuses an expression with a NUL byte in it, with its place,
// counted in bytes from 1.
func checkXPathNUL(expression string) error {
	if at := strings.IndexByte(expression, 0); at >= 0 {
		return fmt.Errorf("it has a NUL character at byte %d, which the XPath engine takes for the end of the expression; take it out", at+1)
	}
	return nil
}

// checkXPathWhole refuses an expression that compiles and that the engine
// would not read to its end, or of which that cannot be told, nil for one it
// reads whole. The error shows the place, counted in bytes from 1.
func checkXPathWhole(expression string, namespaces map[string]string) error {
	if at, found := xpathClosesNothing(expression); found {
		return fmt.Errorf("the %q at byte %d closes nothing", expression[at:at+1], at+1)
	}
	whole, tooDeep := xpathReadWhole(expression, namespaces)
	if tooDeep {
		return errors.New("it is nested too deeply: parentheses, predicates and function arguments may stand 198 deep within one another; write it less deep")
	}
	if whole {
		return nil
	}
	end, found := xpathReadStart(expression, func(start string) bool {
		_, err := compileXPath("("+start+")", namespaces)
		return err == nil
	})
	if !found {
		return errors.New("the XPath engine would read only the start of it and ignore the rest; write one expression")
	}
	read := strings.TrimRightFunc(expression[:end], unicode.IsSpace)
	rest := strings.TrimLeftFunc(expression[len(read):], unicode.IsSpace)
	at := len(expression) - len(rest) + 1
	if why := xpathPredicateNotRead(read, rest); why != "" {
		return fmt.Errorf("what stands from byte %d on, %q, would be ignored: %s", at, rest, why)
	}
	return fmt.Errorf("what stands from byte %d on, %q, is no part of the expression before it, which is all the XPath engine would read; join the two with an operator, or take it out", at, rest)
}

// xpathReadStart finds the part of an expression that the parser reads when
// it does not read it whole: the longest prefix of the text that readWhole
// says is read whole, since any longer one ends in what the parser ignored,
// or in a part of it, which is not an expression either. It returns where
// that prefix ends, and false when the search ran out of its budget before
// finding one, or found none.
func xpathReadStart(expression string, readWhole func(start string) bool) (end int, found bool) {
	budget := xpathPlaceBudget
	for end := len(expression) - 1; end > 0; end-- {
		if !utf8.RuneStart(expression[end]) {
			continue
		}
		if budget -= end; budget < 0 {
			break
		}
		if readWhole(expression[:end]) {
			return end, true
		}
	}
	return 0, false
}

// xpathPredicateNotRead says why the engine would ignore rest, which follows
// the part of the expression that it reads, when rest starts a predicate
// that the parser does not take, and what to write instead. It returns ""
// when rest is anything else, which joining the two or removing rest fixes.
//
// After parentheses within a path, as in `//a/(b)[1]`, the parser reads no
// predicate at all. After anything else it reads every predicate written (a
// step's, and those after an expression in parentheses, a function call or a
// literal, which the engine evaluates in turn from antchfx/xpath v1.3.9), so
// a read part that ends in `)` followed by a predicate occurs only there.
func xpathPredicateNotRead(read, rest string) string {
	if strings.HasPrefix(rest, "[") && strings.HasSuffix(read, ")") {
		return `the XPath engine reads no predicate after parentheses within a path; write the predicate inside them, after the step it is for, as in "//a/(b[1])"`
	}
	return ""
}

// xpathClosesNothing finds the first closing parenthesis or bracket of an
// expression, outside its strings, that no opening one before it pairs with.
func xpathClosesNothing(expression string) (at int, found bool) {
	parentheses, brackets := 0, 0
	for i := 0; i < len(expression); i++ {
		switch c := expression[i]; c {
		case '\'', '"':
			closing := strings.IndexByte(expression[i+1:], c)
			if closing < 0 {
				// An unclosed string, which does not compile.
				return 0, false
			}
			i += closing + 1
		case '(':
			parentheses++
		case ')':
			if parentheses == 0 {
				return i, true
			}
			parentheses--
		case '[':
			brackets++
		case ']':
			if brackets == 0 {
				return i, true
			}
			brackets--
		}
	}
	return 0, false
}

// xpathReadWhole reports whether the engine reads an expression that
// compiles to its end, given that xpathClosesNothing found nothing in it,
// and whether that cannot be told, the expression being as deep as the
// parser reads.
func xpathReadWhole(expression string, namespaces map[string]string) (whole, tooDeep bool) {
	_, err := compileXPath("("+expression+")", namespaces)
	if err != nil && err.Error() == xpathTooDeep {
		return false, true
	}
	return err == nil || !strings.HasSuffix(err.Error(), xpathNotWhole), false
}

// compileXPath compiles expression with its namespace bindings, once.
func compileXPath(expression string, namespaces map[string]string) (*xpath.Expr, error) {
	if len(namespaces) == 0 {
		return xpath.Compile(expression)
	}
	return xpath.CompileWithNS(expression, namespaces)
}
