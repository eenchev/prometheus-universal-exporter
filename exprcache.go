package main

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/andybalholm/cascadia"
	"github.com/antchfx/xpath"
	"github.com/itchyny/gojq"
)

// Expressions are compiled once and reused by every scrape. Parsing and
// compiling a jq program costs far more than running it on a typical
// response, and a collector like the status page demo evaluates dozens of
// them per scrape. Configuration validation compiles every expression a
// configuration holds, so a scrape normally finds its programs already here,
// and a syntax error is reported at startup rather than on the first scrape.
//
// The caches are keyed by the expression text (and, for XPath, the namespace
// bindings), so two collectors sharing an expression share its program. Each
// is bounded: a configuration reloaded many times with changing expressions
// cannot grow it forever. When a cache reaches its bound it is emptied and
// refilled on demand, which costs one compile per expression and keeps the
// code free of eviction bookkeeping for a situation that is rare in practice.

// exprCacheMaxEntries bounds each cache. A configuration with more distinct
// expressions still works; it just compiles some of them more than once.
const exprCacheMaxEntries = 4096

type exprCache[T any] struct {
	mu      sync.RWMutex
	entries map[string]T
	compile func(string) (T, error)
}

func newExprCache[T any](compile func(string) (T, error)) *exprCache[T] {
	return &exprCache[T]{entries: map[string]T{}, compile: compile}
}

func (c *exprCache[T]) get(key string) (T, error) {
	c.mu.RLock()
	value, ok := c.entries[key]
	c.mu.RUnlock()
	if ok {
		return value, nil
	}
	value, err := c.compile(key)
	if err != nil {
		return value, err
	}
	c.mu.Lock()
	if len(c.entries) >= exprCacheMaxEntries {
		c.entries = map[string]T{}
	}
	c.entries[key] = value
	c.mu.Unlock()
	return value, nil
}

func (c *exprCache[T]) len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.entries)
}

// jqRootVariable is bound in every jq program to the whole decoded document,
// so an expression evaluated against one item of a metric's items can still
// look things up elsewhere in the response.
const jqRootVariable = "$root"

// A compiled program is safe to run from many goroutines at once.
var jqPrograms = newExprCache(func(expression string) (*gojq.Code, error) {
	query, err := gojq.Parse(expression)
	if err != nil {
		return nil, err
	}
	return gojq.Compile(query, gojq.WithVariables([]string{jqRootVariable}))
})

func compileJQ(expression string) (*gojq.Code, error) { return jqPrograms.get(expression) }

var regexPrograms = newExprCache(regexp.Compile)

func compileRegex(expression string) (*regexp.Regexp, error) { return regexPrograms.get(expression) }

// cssSelectors are compiled with cascadia, which goquery uses underneath.
// goquery's own Find swallows a selector that does not compile and matches
// nothing, which turned a typo into "matched no nodes" on every scrape.
var cssSelectors = newExprCache(cascadia.Compile)

func compileCSS(selector string) (cascadia.Selector, error) { return cssSelectors.get(selector) }

// xpathPrograms are keyed by the expression and the namespace bindings, which
// change what a prefix in the expression means.
var xpathPrograms = newExprCache(func(key string) (*xpath.Expr, error) {
	expression, namespaces := splitXPathKey(key)
	if len(namespaces) == 0 {
		return xpath.Compile(expression)
	}
	return xpath.CompileWithNS(expression, namespaces)
})

func compileXPath(expression string, namespaces map[string]string) (*xpath.Expr, error) {
	return xpathPrograms.get(xpathKey(expression, namespaces))
}

// The key is the expression, then each binding, separated by NUL, which
// neither an expression nor a namespace can contain.
func xpathKey(expression string, namespaces map[string]string) string {
	if len(namespaces) == 0 {
		return expression
	}
	prefixes := make([]string, 0, len(namespaces))
	for prefix := range namespaces {
		prefixes = append(prefixes, prefix)
	}
	sort.Strings(prefixes)
	var b strings.Builder
	b.WriteString(expression)
	for _, prefix := range prefixes {
		fmt.Fprintf(&b, "\x00%s\x00%s", prefix, namespaces[prefix])
	}
	return b.String()
}

func splitXPathKey(key string) (string, map[string]string) {
	parts := strings.Split(key, "\x00")
	if len(parts) == 1 {
		return key, nil
	}
	namespaces := map[string]string{}
	for i := 1; i+1 < len(parts); i += 2 {
		namespaces[parts[i]] = parts[i+1]
	}
	return parts[0], namespaces
}
