package expr

import (
	"math/rand/v2"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// sumArguments are the arguments of the sums a program found: of the one
// that is the whole expression, "" without one, and of the parts.
func sumArguments(t *testing.T, expression string, namespaces map[string]string) (whole string, parts []string) {
	t.Helper()
	program, err := CompileXPath(expression, namespaces)
	if err != nil {
		t.Fatalf("%s: %v", expression, err)
	}
	sums := program.Sums()
	if sums == nil {
		return "", nil
	}
	if sums.Whole == nil && len(sums.Parts) == 0 {
		t.Fatalf("%s: sums without a call", expression)
	}
	if sums.Whole != nil {
		whole = sums.Whole.Argument
	}
	for _, part := range sums.Parts {
		parts = append(parts, part.Argument)
	}
	return whole, parts
}

// The calls of sum() in an expression's own context are found when it
// compiles: the one that is the whole expression, blanks around it and
// inside its parentheses aside, and those that are parts of a larger one,
// nested ones included, each with its argument. A sum() inside a predicate
// is none of them, nor is the word in a string, a name that only ends or
// starts with it, or a step, an attribute or an element of that name. A
// prefix before the name, which the engine passes over, is passed over
// here: fn:sum(//a) alone is the whole expression, as sum(//a) is. Only the
// first argument is the one added up, as in the engine.
func TestTheSumsOfAnXPathExpressionAreFoundOutsideItsPredicates(t *testing.T) {
	for _, test := range []struct {
		expression string
		whole      string
		parts      []string
	}{
		{"sum(//a)", "//a", nil},
		{"  sum ( //a )\n", "//a", nil},
		{"sum(//td[@class='bytes'])", "//td[@class='bytes']", nil},
		{"sum(//a[b > sum(c)]/d)", "//a[b > sum(c)]/d", nil},
		{"sum(//a | //b)", "//a | //b", nil},
		{"sum(//a[.=')'])", "//a[.=')']", nil},
		{`sum(//a[.="sum("])`, `//a[.="sum("]`, nil},
		{"sum(//a, //b)", "//a", nil},
		{"sum(//a[contains(., ',')])", "//a[contains(., ',')]", nil},
		{"sum(substring-after(//a, ','))", "substring-after(//a, ',')", nil},
		{"fn:sum(//a)", "//a", nil},
		{" x-1.y:sum( //a ) ", "//a", nil},
		{"fn:sum(//a) + 1", "", []string{"//a"}},
		{"fn:sum(//a) + x:sum(//b)", "", []string{"//a", "//b"}},
		{"fn:round(fn:sum(//a))", "", []string{"//a"}},
		{"sum(//x:a)", "//x:a", nil},
		{"sum(//a/child::b)", "//a/child::b", nil},
		{"x:count(//a) + sum(//b)", "", []string{"//b"}},
		{"sum(sum(//a))", "sum(//a)", []string{"//a"}},
		{"sum(//a) div count(//a)", "", []string{"//a"}},
		{"round(sum(//a))", "", []string{"//a"}},
		{"(sum(//a))", "", []string{"//a"}},
		{"-sum(//a)", "", []string{"//a"}},
		{"1-sum(//a)", "", []string{"//a"}},
		{"sum(//a) + sum(//b/@n)", "", []string{"//a", "//b/@n"}},
		{"concat('sum(', sum(//a), ')')", "", []string{"//a"}},
		{"10 mod sum(//a)", "", []string{"//a"}},
		{"sum(//a) > 3 and sum(//b[sum(c) > 1]) < 9", "", []string{"//a", "//b[sum(c) > 1]"}},
		{"count(//a[sum(b) > 2])", "", nil},
		{"//a[sum(b) > 2]/c", "", nil},
		{"string(//a[.='sum(x)'])", "", nil},
		{"count(//sum)", "", nil},
		{"//a/sum", "", nil},
		{"//@sum", "", nil},
		{"count(//a)", "", nil},
		{"string-length(//summary)", "", nil},
	} {
		whole, parts := sumArguments(t, test.expression, nil)
		if whole != test.whole || !reflect.DeepEqual(parts, test.parts) {
			t.Errorf("%s: the whole sum over %q and the parts over %q, want %q and %q", test.expression, whole, parts, test.whole, test.parts)
		}
	}
	// The argument is compiled with the namespaces of its expression.
	namespaces := map[string]string{"s": "urn:sizes"}
	if whole, _ := sumArguments(t, "sum(//s:size)", namespaces); whole != "//s:size" {
		t.Errorf("sum(//s:size): the whole sum over %q", whole)
	}
	program, err := CompileXPath("sum(//s:size)", namespaces)
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := CompileXPath("sum(//s:size)", namespaces); again.Sums() != program.Sums() {
		t.Error("the sums of an expression were found again when it was asked for again")
	}
}

// The scan passes over what is no call in the expression's own context,
// and gives up on an expression whose parentheses or brackets do not pair,
// which the engine does not compile either.
func TestTheScanForSumsReadsAnExpressionAsTheEngineDoes(t *testing.T) {
	for expression, want := range map[string][]string{
		"sum(//a)":                   {"sum(//a)"},
		"x sum (//a) y":              {"sum (//a)"},
		"checksum(//a)":              nil,
		"my-sum(//a)":                nil,
		"sum.total(//a)":             nil,
		"sum2(//a)":                  nil,
		"_sum(//a)":                  nil,
		"résum(//a)":                 nil,
		"sumé(//a)":                  nil,
		"a/sum(//a)":                 nil,
		"a // sum(//a)":              nil,
		"@sum(//a)":                  nil,
		"$sum(//a)":                  nil,
		"self :: sum(//a)":           nil,
		"p:sum(//a)":                 {"sum(//a)"},
		"a[sum(b)]":                  nil,
		"a[b[c]][sum(d)]":            nil,
		"a[b] + sum(c[d])":           {"sum(c[d])"},
		"'sum(a)'":                   nil,
		`"sum(a)" = sum(b)`:          {"sum(b)"},
		"sum(a":                      nil,
		"sum(a))":                    nil,
		"sum(a[b)":                   nil,
		"sum('a)":                    nil,
		"sum(sum(a), sum(b))":        {"sum(sum(a), sum(b))", "sum(a)", "sum(b)"},
		"f(sum(a), g(sum(b)))":       {"sum(a)", "sum(b)"},
		"sum\t(\n a \n)":             {"sum\t(\n a \n)"},
		"sum":                        nil,
		"sum + 1":                    nil,
		"sum(a)sum(b)":               {"sum(a)", "sum(b)"},
		"sum((a | b)[c], d)":         {"sum((a | b)[c], d)"},
		"sum(a[contains(.,',')], b)": {"sum(a[contains(.,',')], b)"},
	} {
		var got []string
		for _, call := range scanXPathSums(expression) {
			got = append(got, expression[call.start:call.end])
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: the calls %q, want %q", expression, got, want)
		}
	}
	// The argument ends at the first comma of the call itself.
	for expression, want := range map[string]string{
		"sum(a, b)":                  "a",
		"sum(a[contains(.,',')], b)": "a[contains(.,',')]",
		"sum(f(a, b), c)":            "f(a, b)",
		"sum((a | b)[c], d)":         "(a | b)[c]",
		"sum(a[.=','])":              "a[.=',']",
	} {
		calls := scanXPathSums(expression)
		if len(calls) != 1 || expression[calls[0].argument:calls[0].argumentEnd] != want {
			t.Errorf("%s: the calls %+v, want one with the argument %q", expression, calls, want)
		}
	}
}

// sumsBeforePrefixes is findXPathSums as it was before a prefixed call
// could be the whole expression, reduced to what it found: the argument of
// the whole call and of the parts.
func sumsBeforePrefixes(expression string) (whole string, parts []string) {
	for i, call := range scanXPathSums(expression) {
		argument := strings.TrimSpace(expression[call.argument:call.argumentEnd])
		if _, err := newXPathProgram(argument, nil); err != nil {
			continue
		}
		if i == 0 && expression[call.start:call.end] == strings.TrimSpace(expression) {
			whole = argument
			continue
		}
		parts = append(parts, argument)
	}
	return whole, parts
}

// A prefix before the name of the one call that is the whole expression is
// all that changed in what the finder finds: over expressions put together
// of pieces, every one the engine reads whole has the sums it had, but the
// one that is a prefix, a colon and one sum(...), whose call was a part and
// is the whole.
func TestOnlyAPrefixedWholeSumIsFoundOtherwiseThanBefore(t *testing.T) {
	prefixed := regexp.MustCompile(`^\s*[A-Za-z_é][A-Za-z_é0-9.-]*:sum\s*\(`)
	pieces := []string{"sum", "sum(", "sum (", "(", ")", "[", "]", ",", "'", `"`, "/", "::", ":", "@", "$", " ", "a", "-", ".", "1", "é", "|", "fn:", "x", "//a", " + ", "count("}
	random := rand.New(rand.NewPCG(5, 20261003))
	compiled, changed := 0, 0
	for range 80000 {
		var text strings.Builder
		for range 1 + random.IntN(9) {
			text.WriteString(pieces[random.IntN(len(pieces))])
		}
		expression := text.String()
		program, err := CompileXPath(expression, nil)
		if err != nil {
			continue
		}
		compiled++
		whole, parts := "", []string(nil)
		if sums := program.Sums(); sums != nil {
			if sums.Whole != nil {
				whole = sums.Whole.Argument
			}
			for _, part := range sums.Parts {
				parts = append(parts, part.Argument)
			}
		}
		oldWhole, oldParts := sumsBeforePrefixes(expression)
		if whole == oldWhole && reflect.DeepEqual(parts, oldParts) {
			continue
		}
		changed++
		if !prefixed.MatchString(expression) || oldWhole != "" || len(oldParts) == 0 || whole != oldParts[0] || !slices.Equal(parts, oldParts[1:]) {
			t.Fatalf("%q: the whole sum over %q and the parts over %q, and before over %q and %q", expression, whole, parts, oldWhole, oldParts)
		}
	}
	if compiled < 1000 || changed == 0 {
		t.Fatalf("%d expressions compiled and %d changed: too few to show anything", compiled, changed)
	}
}
