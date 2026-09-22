package main

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"testing"

	"github.com/itchyny/gojq"
)

// Expressions are compiled once, when the configuration loads, and scrapes
// reuse the programs.

func TestExpressionsAreCompiledOnceAndReused(t *testing.T) {
	expression := `.servers | length | . * 1 // "unique-expression-for-this-test"`
	c := Collector{Name: "cached", Request: RequestConfig{Type: RequestTypeHTTP}, Transform: TransformConfig{Type: "jq"},
		Metrics: []MetricRule{{Name: "servers", Type: GaugeMetricType, Expression: expression}}}
	if err := (&Config{Collectors: []Collector{c}}).Validate(); err != nil {
		t.Fatal(err)
	}
	first, err := compileJQ(expression)
	if err != nil {
		t.Fatal(err)
	}
	r := &HTTPResponse{Body: []byte(`{"servers": [1, 2]}`), Headers: http.Header{"Content-Type": {"application/json"}}}
	for i := 0; i < 3; i++ {
		d, err := decode(r, &c)
		if err != nil {
			t.Fatal(err)
		}
		set, err := transform(context.Background(), d, r, &c, "python3")
		if err != nil || set.Metrics[0].Value != 2 {
			t.Fatalf("set=%v err=%v", set, err)
		}
	}
	again, _ := compileJQ(expression)
	if first != again {
		t.Fatal("the program was compiled again instead of reused")
	}
}

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

// One compiled program serves concurrent scrapes.
func TestCompiledJQIsSafeConcurrently(t *testing.T) {
	code, err := compileJQ(`.n * 2`)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 50)
	for i := 0; i < 50; i++ {
		go func(n int) {
			values, err := evaluateJQ(context.Background(), map[string]any{"n": n}, nil, `.n * 2`)
			if err == nil && (len(values) != 1 || values[0] != n*2) {
				err = fmt.Errorf("%d*2 = %v", n, values)
			}
			done <- err
		}(i)
	}
	for i := 0; i < 50; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	_ = code
}

func BenchmarkJQCompiledOnce(b *testing.B) {
	data := map[string]any{"servers": []any{map[string]any{"cpu": 1}, map[string]any{"cpu": 2}}}
	for i := 0; i < b.N; i++ {
		if _, err := evaluateJQ(context.Background(), data, data, `[.servers[].cpu] | add`); err != nil {
			b.Fatal(err)
		}
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
