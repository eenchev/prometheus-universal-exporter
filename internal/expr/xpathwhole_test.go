package expr

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/antchfx/xmlquery"
	"github.com/antchfx/xpath"
)

// The engine's parser stops at the end of the expression it reads and
// ignores what follows, so each of these compiled and was evaluated as its
// start: the sum with a parenthesis too many was the sum, and `//a 'x'` was
// `//a`. They are refused where expressions are compiled, with the place: a
// closing parenthesis or bracket that closes nothing, or where what the
// engine would ignore starts.
func TestAnXPathExpressionTheEngineWouldNotReadToItsEndIsRefused(t *testing.T) {
	for expression, want := range map[string]string{
		"sum(//a))":             `the ")" at byte 9 closes nothing`,
		"//a)":                  `the ")" at byte 4 closes nothing`,
		"sum(//a)] + 1":         `the "]" at byte 9 closes nothing`,
		"sum(//a) ) + (1":       `the ")" at byte 10 closes nothing`,
		"//a[. = ')'] ]":        `the "]" at byte 14 closes nothing`,
		`//a[. = "]"])`:         `the ")" at byte 13 closes nothing`,
		"sum(//a))) + 1":        `the ")" at byte 9 closes nothing`,
		"//a 'x'":               `what stands from byte 5 on, "'x'", is no part of the expression before it`,
		"1 2":                   `what stands from byte 3 on, "2", is no part of the expression before it`,
		"//a b":                 `what stands from byte 5 on, "b", is no part of the expression before it`,
		"//a   , //b":           `what stands from byte 7 on, ", //b", is no part of the expression before it`,
		"//a ! b":               `what stands from byte 5 on, "! b", is no part of the expression before it`,
		"//a (b)":               `what stands from byte 5 on, "(b)", is no part of the expression before it`,
		"//a/sum(b)":            `what stands from byte 8 on, "(b)", is no part of the expression before it`,
		"child::sum(b)":         `what stands from byte 11 on, "(b)", is no part of the expression before it`,
		"sum(//a) x":            `what stands from byte 10 on, "x", is no part of the expression before it`,
		"sum(//a)sum(//a)":      `what stands from byte 9 on, "sum(//a)", is no part of the expression before it`,
		"count(//a) > 0 or 1 2": `what stands from byte 21 on, "2", is no part of the expression before it`,
		"//é 'ü'":               `what stands from byte 6 on, "'ü'", is no part of the expression before it`,
		"1 div2":                `what stands from byte 3 on, "div2", is no part of the expression before it`,
		"/ /a":                  `what stands from byte 3 on, "/a", is no part of the expression before it`,
		"//a $x":                `what stands from byte 5 on, "$x", is no part of the expression before it`,
		"//a = 'x' 'y'":         `what stands from byte 11 on, "'y'", is no part of the expression before it`,
	} {
		_, err := CompileXPath(expression, nil)
		if err == nil || !strings.HasPrefix(err.Error(), want) {
			t.Errorf("%s: the error %v, want one that starts with %q", expression, err, want)
		}
		// With namespaces as without.
		if _, err := CompileXPath(expression, map[string]string{"p": "urn:p"}); err == nil || !strings.HasPrefix(err.Error(), want) {
			t.Errorf("%s with namespaces: the error %v, want one that starts with %q", expression, err, want)
		}
	}
}

// An expression the engine reads to its end compiles as it did: brackets
// and parentheses in strings are text, one that follows an opening one
// closes it, blanks may stand anywhere, and an expression nested one level
// less deep than the parser allows, the deepest that can be seen in one
// more pair of parentheses, is not taken for one with something after it.
func TestAnXPathExpressionReadToItsEndCompiles(t *testing.T) {
	deepest := strings.Repeat("(", 198) + "1" + strings.Repeat(")", 198)
	if _, err := xpath.Compile("(" + deepest + ")"); err != nil {
		t.Fatalf("the engine no longer compiles what was its deepest expression: %v", err)
	}
	if _, err := xpath.Compile("((" + deepest + "))"); err == nil {
		t.Fatal("the engine compiles an expression deeper than was its limit: the test needs a deeper one")
	}
	for _, expression := range []string{
		"//a", " //a ", "sum(//a)", "//a[. = ')']", `//a[. = "]"]`, "//a[.=']'][.=\")\"]", "(//a)[1]", "((1))", "//a[b[c]]", "count(//a) > 0 or sum(//a) > 0",
		"//a /b", "//a/ b", "/ a", "1 /a", "a and b", "* * *", "a * b", "/", ".", "..", "@x", "text()", "//a/text ()", "a/(b, c)/d", "//a | //b", "- 1", "1 = 2 = 3",
		"'x'[1]", "'x' [1]", ". [1]", "//a [1]", "fn:sum(//a)", "//p:a/@p:b", "//a[position() = last()]", "concat('(', //a, ')')", "normalize-space(.)", "//a\n\t[\n1\n]\n",
		deepest,
	} {
		if _, err := CompileXPath(expression, map[string]string{"p": "urn:p", "fn": "urn:fn"}); err != nil {
			t.Errorf("%.40s: %v", expression, err)
		}
		if err := checkXPathWhole(expression, nil); err != nil {
			t.Errorf("%.40s: %v", expression, err)
		}
	}
}

// The words the check knows the engine's refusal of a token by are the
// engine's still, and no other failure of an expression in parentheses is
// taken for it.
func TestTheEnginesRefusalOfATokenIsToldFromItsOthers(t *testing.T) {
	if _, err := xpath.Compile("(//a 'x')"); err == nil || !strings.HasSuffix(err.Error(), xpathNotWhole) {
		t.Errorf("(//a 'x'): the error %v, want one that ends with %q", err, xpathNotWhole)
	}
	if _, err := xpath.Compile(strings.Repeat("(", 200) + "1" + strings.Repeat(")", 200)); err == nil || err.Error() != xpathTooDeep {
		t.Errorf("200 parentheses deep: the error %v, want %q", err, xpathTooDeep)
	}
	for _, expression := range []string{"//a # b", "//a :", "a (b)", "'a", "1 or", "* *", "//a[", "lower-case()"} {
		if _, err := xpath.Compile(expression); err == nil {
			t.Errorf("%s compiles", expression)
		} else if strings.HasSuffix(err.Error(), xpathNotWhole) {
			t.Errorf("%s: the error %v reads as a token after an expression", expression, err)
		}
	}
}

// evaluated is what the engine evaluates an expression to at the top of a
// document, written out: a value, the texts of the nodes it selects, or the
// panic it ends with.
func evaluated(expression string, root *xmlquery.Node) (result string) {
	defer func() {
		if failed := recover(); failed != nil {
			result = fmt.Sprint("panic: ", failed)
		}
	}()
	compiled, err := xpath.Compile(expression)
	if err != nil {
		return "does not compile: " + err.Error()
	}
	switch value := compiled.Evaluate(xmlquery.CreateXPathNavigator(root)).(type) {
	case *xpath.NodeIterator:
		var nodes []string
		for value.MoveNext() {
			nodes = append(nodes, value.Current().LocalName()+"="+value.Current().Value())
		}
		return fmt.Sprintf("nodes %q", nodes)
	default:
		return fmt.Sprintf("%T %v", value, value)
	}
}

// Whatever the pieces of an expression are put together to, the check
// refuses exactly what the engine reads only the start of: an expression is
// refused when, and only when, a shorter start of it compiles to the same
// thing in parentheses and the whole does not, or a closing bracket stands
// where none is open. What it lets through reads the same with a blank and
// a closing parenthesis after it, which then closes nothing, and is refused.
// And what it refuses evaluates, over a document, to what the start its
// error names evaluates to: the engine did read no more of it.
func TestTheCheckRefusesWhatFollowsAnExpressionAndNothingElse(t *testing.T) {
	pieces := []string{"sum", "sum(", "count(", "(", ")", "[", "]", ",", "'x'", `")"`, "/", "//", "::", ":", "@", "$", " ", "a", "b", "-", ".", "..", "1", "é", "|", "fn:", "*", " or ", " and ", " div ", "=", "<", "!=", "text()", "child"}
	random := rand.New(rand.NewPCG(13, 20261003))
	root, err := xmlquery.Parse(strings.NewReader(`<a x="1"><b>2</b><a>x<b>3</b></a><child>4</child>text</a>`))
	if err != nil {
		t.Fatal(err)
	}
	compiled, refused := 0, 0
	for range 60000 {
		var text strings.Builder
		for range 1 + random.IntN(7) {
			text.WriteString(pieces[random.IntN(len(pieces))])
		}
		expression := text.String()
		if _, err := xpath.Compile(expression); err != nil {
			continue
		}
		compiled++
		err := checkXPathWhole(expression, nil)
		// The refusals are the ones they were, but for a predicate's.
		if before := checkXPathWholeBefore(expression, nil); !sameRefusal(before, err) {
			t.Fatalf("%q: the check says %v, and said %v", expression, err, before)
		}
		_, wrapped := xpath.Compile("(" + expression + ")")
		_, unpaired := xpathClosesNothing(expression)
		if (err != nil) != (unpaired || wrapped != nil) {
			t.Fatalf("%q: the check says %v, the engine says of it in parentheses %v, and a bracket closes nothing: %v", expression, err, wrapped, unpaired)
		}
		if err != nil {
			refused++
			// What is refused is what the engine reads as the start the
			// error names: the two evaluate to the same thing.
			var at int
			_, place, _ := strings.Cut(err.Error(), "byte ")
			if _, scanErr := fmt.Sscanf(place, "%d", &at); scanErr != nil || at < 2 || at > len(expression) {
				t.Fatalf("%q: no place in %q", expression, err)
			}
			// But for one quirk of the parser: a name test before a
			// token `prefix:*` is read as `*`, whatever becomes of the
			// token, so there the start alone is another expression.
			if whole, start := evaluated(expression, root), evaluated(expression[:at-1], root); whole != start && !strings.Contains(expression[at-1:], ":*") {
				t.Fatalf("%q is refused at byte %d, and evaluates to %s where its start %q evaluates to %s", expression, at, whole, expression[:at-1], start)
			}
			continue
		}
		if again := checkXPathWhole(expression+" )", nil); again == nil {
			t.Fatalf("%q is read whole, and so is %q", expression, expression+" )")
		}
		if again := checkXPathWhole(expression+" 'x'", nil); again == nil {
			if _, err := xpath.Compile(expression + " 'x'"); err == nil {
				t.Fatalf("%q is read whole, and so is %q", expression, expression+" 'x'")
			}
		}
	}
	if compiled < 1000 || refused < 100 || refused == compiled {
		t.Fatalf("%d expressions compiled and %d were refused: too few to show anything", compiled, refused)
	}
}

// checkXPathWholeBefore is checkXPathWhole as it was before a predicate the
// engine would ignore had words of its own, an expression at the parser's
// depth was refused, and the search for the place was bounded: what the
// refusals of every other expression are held against.
func checkXPathWholeBefore(expression string, namespaces map[string]string) error {
	if at, found := xpathClosesNothing(expression); found {
		return fmt.Errorf("the %q at byte %d closes nothing", expression[at:at+1], at+1)
	}
	if _, err := compileXPath("("+expression+")", namespaces); err == nil || !strings.HasSuffix(err.Error(), xpathNotWhole) {
		return nil
	}
	for end := len(expression) - 1; end > 0; end-- {
		if !utf8.RuneStart(expression[end]) {
			continue
		}
		if _, err := compileXPath("("+expression[:end]+")", namespaces); err == nil {
			read := strings.TrimRightFunc(expression[:end], unicode.IsSpace)
			rest := strings.TrimLeftFunc(expression[len(read):], unicode.IsSpace)
			return fmt.Errorf("what stands from byte %d on, %q, is no part of the expression before it, which is all the XPath engine would read; join the two with an operator, or take it out", len(expression)-len(rest)+1, rest)
		}
	}
	return errors.New("the XPath engine would read only the start of it and ignore the rest; write one expression")
}

// sameRefusal says the check refuses, or lets through, as it did before:
// with the same words, but where what would be ignored is a predicate after
// one the engine read or after parentheses, which is refused at the same
// byte, and shown the same, with words of its own.
func sameRefusal(before, now error) bool {
	if before == nil || now == nil {
		return before == nil && now == nil
	}
	if before.Error() == now.Error() {
		return true
	}
	what, _, _ := strings.Cut(before.Error(), ", is no part of the expression before it")
	why, found := strings.CutPrefix(now.Error(), what+", would be ignored: the XPath engine reads ")
	return found && strings.Contains(what, ` on, "[`) && (strings.HasPrefix(why, "only one predicate after ") || strings.HasPrefix(why, "no predicate after "))
}

// handWrittenXPath are expressions as rules have them, read whole and not,
// which the check answers as it did before.
var handWrittenXPath = []string{
	"//a", "/a/b/c/@d", "//a[1]/@href", "//table[@id='t']//tr[position() > 1]/td[2]", "//tr[td[1] = 'x']/td[2]", "string(//a[1]/@x)",
	"count(//a) > 0", "sum(//a) div count(//a)", "number(substring-before(//td[@id='x'], '%')) div 100", "//Cube[@currency='USD']/@rate",
	"//*[local-name()='Cube'][@currency]", "//a[@x and (@y or @z)]", "(//a)[1]", "(//a)[last()]/b", "((//a)[1]/b)[2]", "(//a | //b)[1]/@x",
	"a[1][2]", ".[@a][@b]", "//a[b][1]", "//a [ 1 ] [ 2 ]", "concat(name(), ':', @id)", "//a[@x=')']", "//a[@x='['][@y=']']", "//имя", "//a\n  /b\n",
	"ancestor::a[1]/@x", "child::text()", "//processing-instruction('x')", "-a-b", "1 - -1", "div div div", "a/(b, c)/d", "'x'[1]", "$x", "//a[",
	"//a 'x'", "1 2", "//a , //b", "sum(//a)sum(//a)", "//a/sum(b)", "1 div2", "/ /a", "//é 'ü'", "sum(//a))", "//a)", "//a[. = ')'] ]", "'unclosed",
	"//a = 'x' 'y'", "count(//a) > 0 or 1 2", "//a (b)", "//a ! b", "5.5.5", "//a/(b)", "//a # b", "/[1]", "/ [", "/[//a 'x'",
}

// What the check says of an expression is what it said, to the letter, for
// every expression but those its three follow-ups are about: hand-written
// ones, read whole and refused, with and without namespaces. The expressions
// strung together at random are held against it where they are made
// (TestTheCheckRefusesWhatFollowsAnExpressionAndNothingElse).
func TestTheCheckAnswersOtherExpressionsAsItDid(t *testing.T) {
	refused := 0
	for _, expression := range handWrittenXPath {
		for _, namespaces := range []map[string]string{nil, {"p": "urn:p"}} {
			before, now := checkXPathWholeBefore(expression, namespaces), checkXPathWhole(expression, namespaces)
			if fmt.Sprint(before) != fmt.Sprint(now) {
				t.Errorf("%q: the check says %v, and said %v", expression, now, before)
			}
			if now != nil {
				refused++
			}
		}
	}
	if refused < 20 {
		t.Errorf("%d refusals among them: too few to show anything", refused)
	}
}

// A second predicate after an expression in parentheses, a function call or
// a literal is one the engine does not read: `(//a)[@x][1]` would select
// what `(//a)[@x]` selects. It is refused, as before, and the refusal now
// says what it is and shows the form the engine reads, made of the
// expression itself where one more pair of parentheses is all it takes;
// that form compiles. A predicate after parentheses within a path, which
// the engine reads none of, is told as that. A bracket that follows
// anything else is told as it was.
func TestAPredicateTheEngineWouldIgnoreIsRefusedWithTheFormThatWorks(t *testing.T) {
	const (
		second   = `, would be ignored: the XPath engine reads only one predicate after an expression in parentheses, a function call or a literal; for a second one, put the expression and its first predicate in parentheses of their own, as `
		sequence = `, would be ignored: the XPath engine reads no predicate after parentheses within a path; write the predicate inside them, after the step it is for, as in "//a/(b[1])"`
		other    = `, is no part of the expression before it, which is all the XPath engine would read; join the two with an operator, or take it out`
	)
	for expression, want := range map[string]string{
		"(//a)[@x][1]":         `what stands from byte 10 on, "[1]"` + second + `"((//a)[@x])[1]"`,
		"(//a)[1][@x]":         `what stands from byte 9 on, "[@x]"` + second + `"((//a)[1])[@x]"`,
		"'x'[1][2]":            `what stands from byte 7 on, "[2]"` + second + `"('x'[1])[2]"`,
		"count(//a)[1][2]":     `what stands from byte 14 on, "[2]"` + second + `"(count(//a)[1])[2]"`,
		"(//a) [1] [2]":        `what stands from byte 11 on, "[2]"` + second + `"((//a) [1])[2]"`,
		"(//a)[1]\n[2] | //b":  `what stands from byte 10 on, "[2] | //b"` + second + `"((//a)[1])[2] | //b"`,
		"(//a)[1][2]/b":        `what stands from byte 9 on, "[2]/b"` + second + `"((//a)[1])[2]/b"`,
		"(//é)[1][2]":          `what stands from byte 10 on, "[2]"` + second + `"((//é)[1])[2]"`,
		"(//a)[1][2][3]":       `what stands from byte 9 on, "[2][3]"` + second + `in "((//a)[@x])[1]"`,
		"(//a)[1][2] 'x'":      `what stands from byte 9 on, "[2] 'x'"` + second + `in "((//a)[@x])[1]"`,
		"'x'[1][.":             `what stands from byte 7 on, "[."` + second + `in "((//a)[@x])[1]"`,
		"//a/(b)[1]":           `what stands from byte 8 on, "[1]"` + sequence,
		"//a/(b, c)[1][2] | d": `what stands from byte 11 on, "[1][2] | d"` + sequence,
		"/[1]":                 `what stands from byte 2 on, "[1]"` + other,
		"/ [":                  `what stands from byte 3 on, "["` + other,
	} {
		for _, namespaces := range []map[string]string{nil, {"p": "urn:p"}} {
			if _, err := CompileXPath(expression, namespaces); err == nil || err.Error() != want {
				t.Errorf("%s: the error %v, want %q", expression, err, want)
			}
		}
		if _, form, shown := strings.Cut(want, ", as \""); shown {
			if _, err := CompileXPath(strings.TrimSuffix(form, `"`), nil); err != nil {
				t.Errorf("%s: the form the refusal shows, %s, is refused: %v", expression, form, err)
			}
		}
	}
	for _, form := range []string{"((//a)[@x])[1]", "//a/(b[1])", "(((//a)[1])[2])[3]", "(//a)[1]/b[1][2]"} {
		if _, err := CompileXPath(form, nil); err != nil {
			t.Errorf("%s: %v", form, err)
		}
	}
}

// A NUL byte is where the engine's scanner takes the expression to end, and
// where the key of a compiled expression is taken apart: `//td` followed by
// one and anything at all compiled, as `//td`, and with the bytes of a
// namespace binding after it, as `//p:td` bound to that namespace. An
// expression with one is refused where it is compiled, with the place,
// before the key is made, and nothing of it is kept compiled.
func TestAnXPathExpressionWithANULIsRefused(t *testing.T) {
	kept := xpathPrograms.len()
	for expression, at := range map[string]int{"//td\x00 | //zz 'junk' )": 5, "//td\x00": 5, "\x00": 1, "//é\x00//a": 5, "//p:td\x00p\x00urn:p": 7} {
		want := fmt.Sprintf("it has a NUL character at byte %d, which the XPath engine takes for the end of the expression; take it out", at)
		for _, namespaces := range []map[string]string{nil, {"p": "urn:p"}} {
			if _, err := CompileXPath(expression, namespaces); err == nil || err.Error() != want {
				t.Errorf("%q: the error %v, want %q", expression, err, want)
			}
		}
	}
	if now := xpathPrograms.len(); now != kept {
		t.Errorf("%d expressions are kept compiled, and %d were", now, kept)
	}
	// The binding is still what tells two compilations of one expression
	// apart.
	if _, err := CompileXPath("//p:td", map[string]string{"p": "urn:p"}); err != nil {
		t.Error(err)
	}
}

// An expression nested as deep as the engine's parser reads cannot be seen
// in one more pair of parentheses, which fails for its depth before the
// parser comes to what follows: `'x'` after 199 pairs of parentheses, 199
// predicates or 199 calls within one another compiled, and was ignored. An
// expression that deep is refused as too deeply nested, with something
// after it or without; one level less deep is read whole, and refused with
// the place of what follows it; and one level deeper is the engine's own
// refusal still.
func TestAnXPathExpressionAsDeepAsTheParserReadsIsRefused(t *testing.T) {
	const tooDeep = "it is nested too deeply: parentheses, predicates and function arguments may stand 198 deep within one another; write it less deep"
	for name, nested := range map[string]func(depth int) string{
		"parentheses": func(depth int) string { return strings.Repeat("(", depth) + "//a" + strings.Repeat(")", depth) },
		"predicates":  func(depth int) string { return "//a" + strings.Repeat("[b", depth) + strings.Repeat("]", depth) },
		"calls":       func(depth int) string { return strings.Repeat("count(", depth) + "//a" + strings.Repeat(")", depth) },
	} {
		for _, namespaces := range []map[string]string{nil, {"p": "urn:p"}} {
			for _, after := range []string{"", " 'x'", " | //b"} {
				if _, err := xpath.Compile(nested(199) + after); err != nil {
					t.Fatalf("%s 199 deep%s: the engine no longer compiles it: %v", name, after, err)
				}
				if _, err := CompileXPath(nested(199)+after, namespaces); err == nil || err.Error() != tooDeep {
					t.Errorf("%s 199 deep%s: the error %v, want %q", name, after, err, tooDeep)
				}
			}
			if _, err := CompileXPath(nested(198), namespaces); err != nil {
				t.Errorf("%s 198 deep: %v", name, err)
			}
			if _, err := CompileXPath(nested(198)+" | //b", namespaces); err != nil {
				t.Errorf("%s 198 deep in a union: %v", name, err)
			}
			want := fmt.Sprintf("what stands from byte %d on, \"'x'\", is no part of the expression before it", len(nested(198))+2)
			if _, err := CompileXPath(nested(198)+" 'x'", namespaces); err == nil || !strings.HasPrefix(err.Error(), want) {
				t.Errorf("%s 198 deep and 'x': the error %v, want one that starts with %q", name, err, want)
			}
			if _, err := CompileXPath(nested(200), namespaces); err == nil || err.Error() != xpathTooDeep {
				t.Errorf("%s 200 deep: the error %v, want the engine's, %q", name, err, xpathTooDeep)
			}
		}
	}
}

// longXPath is a union of paths that compiles, of about the given length:
// each path is 200 bytes long, since the engine reads a union of at most a
// thousand or so.
func longXPath(t *testing.T, bytes int) string {
	t.Helper()
	paths := make([]string, max(1, bytes/200))
	for i := range paths {
		paths[i] = fmt.Sprintf("//row[@id='%04d%s']/v", i, strings.Repeat("x", 178))
	}
	expression := strings.Join(paths, " | ")
	if _, err := xpath.Compile(expression); err != nil {
		t.Fatalf("the long expression does not compile: %.100v", err)
	}
	return expression
}

// The place where the engine would stop reading is searched for by
// compiling one start of the expression after another, which for an
// expression of 100 KB with something amiss near its start was a hundred
// thousand compilations of 50 KB each, at load: a quarter of a minute. The
// search compiles no more bytes of expression than its budget, whatever the
// length: an expression of 100 KB is refused without the place after no
// compilation at all, one of 2 KB after its sixteen longest starts, and one
// of 200 bytes is searched whole and refused with the place, as it was, as
// is one of 250 bytes with the place at its start. Where the place is near
// the end, an expression of 2 KB is refused with it still, and one of 100 KB
// without.
func TestTheSearchForTheRefusedPlaceIsBounded(t *testing.T) {
	const withoutPlace = "the XPath engine would read only the start of it and ignore the rest; write one expression"
	for _, test := range []struct {
		bytes, tries int
		found        bool
	}{{100_000, 0, false}, {20_000, 1, false}, {2_000, 16, false}, {200, 203, true}} {
		expression := "//a 'x' | " + longXPath(t, test.bytes)
		tries, compiled := 0, 0
		end, found := xpathReadStart(expression, func(start string) bool {
			tries, compiled = tries+1, compiled+len(start)
			_, err := xpath.Compile("(" + start + ")")
			return err == nil
		})
		if compiled > xpathPlaceBudget || found != test.found || found && end != len("//a ") || tries != test.tries {
			t.Errorf("%d bytes: %d compilations of %d bytes in all, within a budget of %d, found %v at %d; want %d compilations and found %v", len(expression), tries, compiled, xpathPlaceBudget, found, end, test.tries, test.found)
		}
		_, err := CompileXPath(expression, nil)
		if want := `what stands from byte 5 on, "'x' | //row`; test.found && (err == nil || !strings.HasPrefix(err.Error(), want)) || !test.found && (err == nil || err.Error() != withoutPlace) {
			t.Errorf("%d bytes: the error %.200v", len(expression), err)
		}
	}
	// 250 bytes are searched whole, wherever the place is.
	short := "//a 'x' | //row[@id='" + strings.Repeat("x", 225) + "']/v"
	if _, err := CompileXPath(short, nil); len(short) != 250 || err == nil || !strings.HasPrefix(err.Error(), `what stands from byte 5 on, "'x' | //row`) {
		t.Errorf("%d bytes: the error %.200v, want one with the place", len(short), err)
	}
	long := longXPath(t, 2_000)
	want := fmt.Sprintf(`what stands from byte %d on, "'x'", is no part of the expression before it`, len(long)+2)
	if _, err := CompileXPath(long+" 'x'", nil); err == nil || !strings.HasPrefix(err.Error(), want) {
		t.Errorf("'x' after %d bytes: the error %.200v, want one that starts with %q", len(long), err, want)
	}
	long = longXPath(t, 100_000)
	if _, err := CompileXPath(long+" 'x'", nil); err == nil || err.Error() != withoutPlace {
		t.Errorf("'x' after %d bytes: the error %.200v, want %q", len(long), err, withoutPlace)
	}
	if _, err := CompileXPath(long, nil); err != nil {
		t.Errorf("%d bytes read whole: %.200v", len(long), err)
	}
}
