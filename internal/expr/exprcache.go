package expr

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

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
// is bounded, so a configuration reloaded many times with changing
// expressions cannot grow it forever, and keeps the expressions in use
// compiled: it holds two generations, the recent one and the one before.
// A lookup finds an expression in either; one found only in the older is
// moved to the recent one. When the recent generation reaches the bound it
// becomes the older one, and what the older one held and nobody asked for
// since is dropped. An expression asked for at least once per generation is
// therefore never compiled again, however many others come and go, and the
// cache never holds more than twice its bound. A cache that was simply
// emptied at its bound instead recompiled almost every lookup once a
// configuration had more distinct expressions than that.

// DefaultCacheEntries is each cache's bound per generation unless the
// configuration asks for more (ReserveExpressions).
const DefaultCacheEntries = 4096

// cacheBound is the bound per generation in force: DefaultCacheEntries, or
// the number of expressions of the configuration last loaded, when that is
// more, so its expressions all stay compiled.
var cacheBound atomic.Int64

func init() { cacheBound.Store(DefaultCacheEntries) }

// ReserveExpressions sizes the caches for a configuration holding n
// expressions: each generation holds at least n, and at least
// DefaultCacheEntries. The configuration's validation calls it with its
// expression count, so a configuration with more expressions than the
// default bound still compiles each of them once.
func ReserveExpressions(n int) {
	cacheBound.Store(int64(max(n, DefaultCacheEntries)))
}

type exprCache[T any] struct {
	mu sync.RWMutex
	// recent and older are the two generations.
	recent, older map[string]T
	compile       func(string) (T, error)
}

func newExprCache[T any](compile func(string) (T, error)) *exprCache[T] {
	return &exprCache[T]{recent: map[string]T{}, compile: compile}
}

func (c *exprCache[T]) get(key string) (T, error) {
	c.mu.RLock()
	value, ok := c.recent[key]
	c.mu.RUnlock()
	if ok {
		return value, nil
	}
	c.mu.Lock()
	if value, ok = c.recent[key]; !ok {
		if value, ok = c.older[key]; ok {
			c.add(key, value)
		}
	}
	c.mu.Unlock()
	if ok {
		return value, nil
	}
	// Compiled outside the lock: two goroutines missing the same expression
	// at once both compile it, and the second program replaces the first,
	// which is equivalent.
	value, err := c.compile(key)
	if err != nil {
		return value, err
	}
	c.mu.Lock()
	c.add(key, value)
	c.mu.Unlock()
	return value, nil
}

// add puts an entry in the recent generation, which first becomes the older
// one when it is full. c.mu is held.
func (c *exprCache[T]) add(key string, value T) {
	if int64(len(c.recent)) >= cacheBound.Load() {
		c.older, c.recent = c.recent, make(map[string]T, len(c.recent))
	}
	c.recent[key] = value
}

// len is how many distinct expressions the cache holds.
func (c *exprCache[T]) len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	n := len(c.recent)
	for key := range c.older {
		if _, ok := c.recent[key]; !ok {
			n++
		}
	}
	return n
}

// JQVariables are bound in every jq program, in this order: $root, the whole
// decoded document, so an expression evaluated against one item of a
// metric's items can still look things up elsewhere in the response;
// $status, the response's HTTP status; and $headers, its headers by
// lower-case name.
var JQVariables = []string{"$root", "$status", "$headers"}

// A compiled program is safe to run from many goroutines at once.
var jqPrograms = newExprCache(func(expression string) (*JQProgram, error) {
	query, err := gojq.Parse(expression)
	if err != nil {
		return nil, err
	}
	code, err := gojq.Compile(query, gojq.WithVariables(JQVariables))
	if err != nil {
		return nil, err
	}
	path, isPath := fieldPath(query)
	return &JQProgram{Code: code, path: path, isPath: isPath}, nil
})

// CompileJQ compiles a jq expression, once per distinct expression.
func CompileJQ(expression string) (*gojq.Code, error) {
	program, err := jqPrograms.get(expression)
	if err != nil {
		return nil, err
	}
	return program.Code, nil
}

// CompileJQProgram is CompileJQ with the program's field path, for Lookup.
func CompileJQProgram(expression string) (*JQProgram, error) { return jqPrograms.get(expression) }

// JQProgram is a compiled jq expression. Most expressions of a metric rule,
// and nearly all of those evaluated once per item, only read a field, such
// as .id or .status.code, and running a gojq program for that costs over
// twenty times what looking the field up does, most of it in the evaluation
// environment gojq sets up for every run. So an expression that is a field
// path alone is recognised when it compiles, from gojq's own parse of it, and
// Lookup reads it directly.
type JQProgram struct {
	Code *gojq.Code
	// path is the field names in order, when isPath: none for ".".
	path   []string
	isPath bool
}

// Lookup evaluates the program directly when it is a field path and every
// step of it is an object or null, as gojq does: a field of an object is its
// value, null when it has none, and a field of null is null. Otherwise ok is
// false, and the program is run: a field of anything else is gojq's error to
// report, and whatever is not a field path is gojq's to evaluate.
func (p *JQProgram) Lookup(input any) (value any, ok bool) {
	if !p.isPath {
		return nil, false
	}
	value = input
	for _, name := range p.path {
		switch v := value.(type) {
		case nil:
			return nil, true
		case map[string]any:
			value = v[name]
		default:
			return nil, false
		}
	}
	return value, true
}

// fieldPath reports whether a parsed query is "." or a field path such as
// .a, .a.b or ."a key".b, and gives its field names. An index with brackets,
// an optional one (.a?), an iteration, a string with interpolation and
// anything beyond a single term are not.
func fieldPath(query *gojq.Query) ([]string, bool) {
	if query.Meta != nil || len(query.Imports) > 0 || len(query.FuncDefs) > 0 || query.Left != nil || query.Right != nil || len(query.Patterns) > 0 || query.Op != 0 || query.Term == nil {
		return nil, false
	}
	term := query.Term
	path := []string{}
	switch term.Type {
	case gojq.TermTypeIdentity:
		if len(term.SuffixList) > 0 {
			return nil, false
		}
		return path, true
	case gojq.TermTypeIndex:
		name, ok := fieldName(term.Index)
		if !ok {
			return nil, false
		}
		path = append(path, name)
	default:
		return nil, false
	}
	for _, suffix := range term.SuffixList {
		if suffix.Iter || suffix.Optional {
			return nil, false
		}
		name, ok := fieldName(suffix.Index)
		if !ok {
			return nil, false
		}
		path = append(path, name)
	}
	return path, true
}

// fieldName is the name an index reads, when it reads one field by name.
func fieldName(index *gojq.Index) (string, bool) {
	switch {
	case index == nil || index.Start != nil || index.End != nil || index.IsSlice:
		return "", false
	case index.Str != nil:
		if len(index.Str.Queries) > 0 {
			return "", false
		}
		return index.Str.Str, true
	case index.Name != "":
		return index.Name, true
	}
	return "", false
}

var regexPrograms = newExprCache(regexp.Compile)

// CompileRegex compiles a regular expression, once per distinct expression.
func CompileRegex(expression string) (*regexp.Regexp, error) { return regexPrograms.get(expression) }

// cssSelectors are compiled with cascadia, which goquery uses underneath.
// goquery's own Find swallows a selector that does not compile and matches
// nothing, which turned a typo into "matched no nodes" on every scrape.
var cssSelectors = newExprCache(cascadia.Compile)

// CompileCSS compiles a CSS selector, once per distinct selector.
func CompileCSS(selector string) (cascadia.Selector, error) { return cssSelectors.get(selector) }

// xpathPrograms are keyed by the expression and the namespace bindings, which
// change what a prefix in the expression means.
var xpathPrograms = newExprCache(func(key string) (*XPathProgram, error) {
	expression, namespaces := splitXPathKey(key)
	compile := func() (*xpath.Expr, error) {
		if len(namespaces) == 0 {
			return xpath.Compile(expression)
		}
		return xpath.CompileWithNS(expression, namespaces)
	}
	first, err := compile()
	if err != nil {
		return nil, err
	}
	program := &XPathProgram{}
	program.pool.New = func() any {
		// The expression compiled once already, so it compiles again.
		e, _ := compile()
		return e
	}
	program.pool.Put(first)
	return program, nil
})

// XPathProgram is a compiled XPath expression that many goroutines may use at
// once. Unlike a jq program, an *xpath.Expr may not be shared: Evaluate runs
// the expression's query tree in place, keeping its position in fields of the
// tree, and Select clones the same tree while it may be being run. So the
// program keeps a pool of compiled copies, and each use takes one of its own:
// a scrape takes a copy the last one returned, and only when every copy is in
// use is another compiled, which costs about as much as a scrape of a small
// document.
type XPathProgram struct {
	pool sync.Pool
}

// Get takes a compiled copy of the expression for the caller alone, until it
// hands it back with Put.
func (p *XPathProgram) Get() *xpath.Expr { return p.pool.Get().(*xpath.Expr) }

// Put hands back a copy Get took, which the caller must no longer use.
func (p *XPathProgram) Put(e *xpath.Expr) { p.pool.Put(e) }

// CompileXPath compiles an XPath expression with the given namespace
// bindings, once per distinct expression and bindings.
func CompileXPath(expression string, namespaces map[string]string) (*XPathProgram, error) {
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
