package expr

import (
	"fmt"
	"strconv"
	"testing"

	"github.com/itchyny/gojq"
)

// countingCache is a cache whose compile counts its calls.
func countingCache() (*exprCache[string], *int) {
	compiles := 0
	return newExprCache(func(s string) (string, error) { compiles++; return s, nil }), &compiles
}

// reserve sizes the caches as ReserveExpressions does for one test, and
// restores the default after it.
func reserve(t *testing.T, n int) {
	t.Helper()
	ReserveExpressions(n)
	t.Cleanup(func() { ReserveExpressions(0) })
}

// bound sets the bound per generation for one test, below the default, which
// ReserveExpressions never goes.
func bound(t *testing.T, n int) {
	t.Helper()
	cacheBound.Store(int64(n))
	t.Cleanup(func() { ReserveExpressions(0) })
}

func TestExpressionCacheIsBounded(t *testing.T) {
	cache, _ := countingCache()
	for i := 0; i < 5*DefaultCacheEntries; i++ {
		if _, err := cache.get(strconv.Itoa(i)); err != nil {
			t.Fatal(err)
		}
	}
	if n := cache.len(); n > 2*DefaultCacheEntries {
		t.Fatalf("%d entries, bound %d per generation", n, DefaultCacheEntries)
	}
	// A failed compile is not cached.
	failing := newExprCache(func(s string) (string, error) { return "", fmt.Errorf("bad %s", s) })
	if _, err := failing.get("x"); err == nil || failing.len() != 0 {
		t.Fatalf("err=%v len=%d", err, failing.len())
	}
}

// Expressions in use stay compiled however many others pass through: one
// asked for between the others is never compiled again, while one nobody
// asks for is dropped after two generations and compiled again when it is.
func TestExpressionCacheKeepsTheWorkingSet(t *testing.T) {
	bound(t, 100)
	cache, compiles := countingCache()
	for i := 0; i < 1000; i++ {
		_, _ = cache.get("hot")
		_, _ = cache.get(strconv.Itoa(i))
	}
	if want := 1 + 1000; *compiles != want {
		t.Fatalf("%d compiles, want %d: the expression in use was compiled again", *compiles, want)
	}
	_, _ = cache.get("0")
	if want := 1 + 1000 + 1; *compiles != want {
		t.Fatalf("%d compiles, want %d: an expression unused for many generations was kept", *compiles, want)
	}
}

// A configuration with more expressions than the default bound, evaluated in
// turn scrape after scrape, compiles each once when the cache is sized for it,
// and with the default bound a rotation of as many expressions as the bound
// compiles each once too.
func TestExpressionCacheHoldsARotationUpToTheBound(t *testing.T) {
	for _, test := range []struct{ reserve, expressions int }{
		{0, DefaultCacheEntries},
		{3 * DefaultCacheEntries / 2, 3 * DefaultCacheEntries / 2},
	} {
		reserve(t, test.reserve)
		cache, compiles := countingCache()
		for pass := 0; pass < 5; pass++ {
			for i := 0; i < test.expressions; i++ {
				_, _ = cache.get(strconv.Itoa(i))
			}
		}
		if *compiles != test.expressions {
			t.Fatalf("reserved %d: %d expressions in rotation took %d compiles, want one each", test.reserve, test.expressions, *compiles)
		}
	}
}

// XPath programs are cached as pools, and a program found again is the one
// compiled first, whose pool still hands out working copies.
func TestXPathProgramsSurviveTheCache(t *testing.T) {
	reserve(t, 0)
	first, err := CompileXPath("//kept", nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < DefaultCacheEntries+10; i++ {
		if _, err := CompileXPath(fmt.Sprintf("//other%d", i), nil); err != nil {
			t.Fatal(err)
		}
		if i == DefaultCacheEntries/2 {
			if again, _ := CompileXPath("//kept", nil); again != first {
				t.Fatal("the XPath program was compiled again")
			}
		}
	}
	e := first.Get()
	if e == nil || e.String() != "//kept" {
		t.Fatalf("pooled copy %v", e)
	}
	first.Put(e)
}

// The same XPath expression means different things under different namespace
// bindings, so the bindings are part of the key.
func TestXPathCacheKeyIncludesNamespaces(t *testing.T) {
	if xpathKey("//a:x", map[string]string{"a": "urn:one"}) == xpathKey("//a:x", map[string]string{"a": "urn:two"}) {
		t.Fatal("namespace bindings are not part of the key")
	}
	expression, namespaces := splitXPathKey(xpathKey("//a:x", map[string]string{"a": "urn:one", "b": "urn:two"}))
	if expression != "//a:x" || namespaces["a"] != "urn:one" || namespaces["b"] != "urn:two" {
		t.Fatalf("round trip: %q %v", expression, namespaces)
	}
}

// BenchmarkJQCompiledPerEvaluation is what every evaluation used to cost.
func BenchmarkJQCompiledPerEvaluation(b *testing.B) {
	data := map[string]any{"servers": []any{map[string]any{"cpu": 1}, map[string]any{"cpu": 2}}}
	for b.Loop() {
		query, err := gojq.Parse(`[.servers[].cpu] | add`)
		if err != nil {
			b.Fatal(err)
		}
		code, err := gojq.Compile(query, gojq.WithVariables(JQVariables))
		if err != nil {
			b.Fatal(err)
		}
		iter := code.Run(data, data)
		for {
			if _, ok := iter.Next(); !ok {
				break
			}
		}
	}
}
