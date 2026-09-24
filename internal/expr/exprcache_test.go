package expr

import (
	"fmt"
	"strconv"
	"testing"

	"github.com/itchyny/gojq"
)

func TestExpressionCacheIsBounded(t *testing.T) {
	cache := newExprCache(func(s string) (string, error) { return s, nil })
	for i := 0; i < exprCacheMaxEntries+10; i++ {
		if _, err := cache.get(strconv.Itoa(i)); err != nil {
			t.Fatal(err)
		}
	}
	if n := cache.len(); n > exprCacheMaxEntries {
		t.Fatalf("%d entries, bound %d", n, exprCacheMaxEntries)
	}
	// A failed compile is not cached.
	failing := newExprCache(func(s string) (string, error) { return "", fmt.Errorf("bad %s", s) })
	if _, err := failing.get("x"); err == nil || failing.len() != 0 {
		t.Fatalf("err=%v len=%d", err, failing.len())
	}
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
	for i := 0; i < b.N; i++ {
		query, err := gojq.Parse(`[.servers[].cpu] | add`)
		if err != nil {
			b.Fatal(err)
		}
		code, err := gojq.Compile(query, gojq.WithVariables([]string{jqRootVariable}))
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
