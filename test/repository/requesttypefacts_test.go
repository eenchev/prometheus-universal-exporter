package repository

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// Sentences that say which request types take a key were each true when
// written and fell behind as types arrived: the spec had every type but
// localfile take a target's own credentials, which only http, graphite and
// grpc do, and dns and tlscert missing from the types that take target
// lists. These tests read what each type takes from its registration in
// internal/fetch/requesttype_<name>.go and hold the sentences to it.

// requestTypeKeys is what one request type's registration lists: its
// request keys (Fields) and the keys a static target may set (TargetFields).
type requestTypeKeys struct{ fields, targetFields []string }

// registeredRequestTypeKeys reads every request type's registration from
// its source, so an opt-in type, absent from this build, is read too.
func registeredRequestTypeKeys(t *testing.T) map[string]requestTypeKeys {
	t.Helper()
	keys := map[string]requestTypeKeys{}
	for _, typ := range treeRequestTypes(t) {
		path := filepath.Join("internal", "fetch", "requesttype_"+typ+".go")
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		var found requestTypeKeys
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			if ident, ok := lit.Type.(*ast.Ident); !ok || ident.Name != "RequestType" {
				return true
			}
			for _, elt := range lit.Elts {
				kv, ok := elt.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				switch kv.Key.(*ast.Ident).Name {
				case "Fields":
					found.fields = stringLiterals(t, path, kv.Value)
				case "TargetFields":
					found.targetFields = stringLiterals(t, path, kv.Value)
				}
			}
			return false
		})
		if len(found.fields) == 0 {
			t.Fatalf("%s registers no request type with Fields; the registration has moved", path)
		}
		keys[typ] = found
	}
	return keys
}

func stringLiterals(t *testing.T, path string, expr ast.Expr) []string {
	t.Helper()
	list, ok := expr.(*ast.CompositeLit)
	if !ok {
		t.Fatalf("%s: a key list is not a []string literal", path)
	}
	var values []string
	for _, elt := range list.Elts {
		if lit, ok := elt.(*ast.BasicLit); ok && lit.Kind == token.STRING {
			value, err := strconv.Unquote(lit.Value)
			if err != nil {
				t.Fatal(err)
			}
			values = append(values, value)
		}
	}
	return values
}

// typesTaking are the request types whose list, as pick reads it, holds key.
func typesTaking(keys map[string]requestTypeKeys, key string, pick func(requestTypeKeys) []string) []string {
	var types []string
	for typ, k := range keys {
		if slices.Contains(pick(k), key) {
			types = append(types, typ)
		}
	}
	slices.Sort(types)
	return types
}

var backquotedWord = regexp.MustCompile("`([a-z0-9_]+)`")

// namedTypes are the request types a sentence names in backquotes.
func namedTypes(sentence string, types []string) []string {
	var named []string
	for _, m := range backquotedWord.FindAllStringSubmatch(sentence, -1) {
		if slices.Contains(types, m[1]) && !slices.Contains(named, m[1]) {
			named = append(named, m[1])
		}
	}
	slices.Sort(named)
	return named
}

// flat is text with every run of blanks one space, so a sentence is found
// however it is wrapped.
func flat(text string) string { return strings.Join(strings.Fields(text), " ") }

func TestSentencesNameTheRequestTypesThatTakeAKey(t *testing.T) {
	keys := registeredRequestTypeKeys(t)
	types := treeRequestTypes(t)
	spec := flat(read(t, "docs/SPECIFICATION-EXPORTER.md"))
	fields := func(k requestTypeKeys) []string { return k.fields }
	targetFields := func(k requestTypeKeys) []string { return k.targetFields }
	credentials := regexp.MustCompile("and an? ((?:`[a-z0-9]+`(?:, | or ))+`[a-z0-9]+`) target its own target credentials")
	for _, c := range []struct {
		where, text string
		find        *regexp.Regexp
		want        []string
	}{
		{
			"spec § 26.1, the types that set target lists", spec,
			regexp.MustCompile("## 26.1 Target allowlists and deny lists (.*?) collector MAY set `request.allowed_targets`"),
			typesTaking(keys, "allowed_targets", fields),
		},
		{
			"spec § 42.14, the targets that take their own credentials", spec, credentials,
			typesTaking(keys, "bearer_token", targetFields),
		},
		{
			"docs/STATIC-TARGETS.md, the targets that take their own credentials", flat(read(t, "docs/STATIC-TARGETS.md")), credentials,
			typesTaking(keys, "bearer_token", targetFields),
		},
		{
			"docs/TLSCERT.md, the other types that take the tls block", flat(read(t, "docs/TLSCERT.md")),
			regexp.MustCompile("The `tls` block is the one (.*?) take "),
			slices.DeleteFunc(typesTaking(keys, "tls", fields), func(typ string) bool { return typ == "tlscert" }),
		},
	} {
		m := c.find.FindStringSubmatch(c.text)
		if m == nil {
			t.Errorf("%s: the sentence is not found; it has moved", c.where)
			continue
		}
		if got := namedTypes(m[1], types); !slices.Equal(got, c.want) {
			t.Errorf("%s: the sentence names %v, but the registrations in internal/fetch give %v: %q", c.where, got, c.want, m[0])
		}
	}
}

// decoderNames is how prose names each decoder.type.
var decoderNames = map[string]string{
	"json": "JSON", "ndjson": "NDJSON", "yaml": "YAML", "toml": "TOML", "ini": "INI",
	"xml": "XML", "csv": "CSV", "html": "HTML", "logfmt": "logfmt",
	"prometheus": "Prometheus", "text": "plain text", "graphite": "Graphite",
}

// Spec § 1, the chart README's opening and the comment on
// model.DecoderConfig each list the decoders, and each kept the list of the
// day it was written.
func TestDecoderListsNameEveryDecoder(t *testing.T) {
	spec := read(t, "docs/SPECIFICATION-EXPORTER.md")
	purpose := regexp.MustCompile(`(?m)^- Support JSON, .*$`).FindString(spec)
	chart := regexp.MustCompile(`Answers in [^;]*;`).FindString(read(t, "charts/prometheus-universal-exporter/README.md"))
	comment := flat(regexp.MustCompile(`(?s)// DecoderConfig is .*?\ntype DecoderConfig`).FindString(read(t, "internal/model/config.go")))
	for _, list := range []struct{ where, text string }{
		{"spec § 1's list of formats", purpose},
		{"the chart README's opening", chart},
		{"the comment on model.DecoderConfig", comment},
	} {
		if list.text == "" {
			t.Errorf("%s is not found; it has moved", list.where)
			continue
		}
		for _, decoder := range model.DecoderTypes {
			if decoder == "auto" {
				continue
			}
			name, ok := decoderNames[decoder]
			if !ok {
				t.Errorf("decoder %q has no entry in decoderNames: name it in spec § 1, the chart README and the comment on model.DecoderConfig, then add it", decoder)
				continue
			}
			word := name
			if list.where == "the comment on model.DecoderConfig" {
				word = decoder
			}
			if !regexp.MustCompile(`\b` + regexp.QuoteMeta(word) + `\b`).MatchString(list.text) {
				t.Errorf("%s does not name the %s decoder as %q: %q", list.where, decoder, word, list.text)
			}
		}
	}
}

// chartSourceNames is, per request type, the words the chart README's
// opening and Chart.yaml's description, which Artifact Hub shows, use for
// it. The description is of the image the chart deploys, a default build,
// so it leaves the opt-in types out.
var chartSourceNames = map[string]struct{ readme, description string }{
	"http":       {"an HTTP API", "HTTP"},
	"grpc":       {"a gRPC service", "gRPC"},
	"graphite":   {"a Graphite server", "Graphite"},
	"localfile":  {"a file in the pod", "files local"},
	"sshfile":    {"another host over SSH", "over SSH"},
	"sql":        {"a SQL database", "SQL databases"},
	"dns":        {"a DNS name", "DNS names"},
	"tlscert":    {"the TLS certificate", "TLS certificates"},
	"kubernetes": {"Kubernetes objects", "Kubernetes objects"},
	"s3file":     {"objects in S3", ""},
	"redis":      {"a Redis or Valkey server", ""},
	"tcp":        {"a service that answers a line sent over TCP", ""},
}

func TestTheChartDescribesEverySource(t *testing.T) {
	optIn := optInRequestTypes(t)
	var chart struct {
		Description string `yaml:"description"`
	}
	if err := yaml.Unmarshal([]byte(read(t, "charts/prometheus-universal-exporter/Chart.yaml")), &chart); err != nil {
		t.Fatal(err)
	}
	readme := read(t, "charts/prometheus-universal-exporter/README.md")
	opening := flat(regexp.MustCompile(`(?m)^The exporter turns .*$`).FindString(readme))
	if opening == "" {
		t.Fatal("the chart README has no paragraph starting \"The exporter turns\"; the opening has moved")
	}
	for _, typ := range treeRequestTypes(t) {
		names, ok := chartSourceNames[typ]
		if !ok {
			t.Errorf("request type %q has no entry in chartSourceNames: name it in the chart README's opening and, unless it is opt-in, in Chart.yaml's description, then add it", typ)
			continue
		}
		if !strings.Contains(opening, names.readme) {
			t.Errorf("the chart README's opening does not name %q (%s): %q", names.readme, typ, opening)
		}
		if !slices.Contains(optIn, typ) && !strings.Contains(chart.Description, names.description) {
			t.Errorf("Chart.yaml's description does not name %q (%s): %q", names.description, typ, chart.Description)
		}
	}
}

// Every path the exporter's server routes is a row of docs/ENDPOINTS.md:
// /debug/run was served and not listed. The self-metrics and static
// targets paths, which flags set, are listed at their defaults.
func TestEndpointsListEveryRoute(t *testing.T) {
	server := "internal/exporter/server.go"
	file, err := parser.ParseFile(token.NewFileSet(), server, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	constants := map[string]string{}
	for _, m := range regexp.MustCompile(`(?m)^\s*(debug[A-Za-z]*Path)\s*=\s*"([^"]+)"`).FindAllStringSubmatch(read(t, "internal/exporter/debugpage.go"), -1) {
		constants[m[1]] = m[2]
	}
	var routes []string
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) != 2 {
			return true
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); !ok || sel.Sel.Name != "HandleFunc" {
			return true
		}
		route, ok := routePattern(call.Args[0], constants)
		if !ok {
			return true // a path a flag sets: the defaults are checked below
		}
		if _, path, found := strings.Cut(route, " "); found {
			route = path
		}
		routes = append(routes, strings.TrimSuffix(route, "{$}"))
		return true
	})
	routes = append(routes, "/self-metrics", "/static-targets")
	if len(routes) < 8 {
		t.Fatalf("found %d routes in %s; the server has moved", len(routes), server)
	}
	var listed []string
	for _, line := range strings.Split(read(t, "docs/ENDPOINTS.md"), "\n") {
		if first, ok := strings.CutPrefix(line, "| `"); ok {
			cell, _, _ := strings.Cut(first, " |")
			for _, m := range regexp.MustCompile("`?([^`, ]+)`?").FindAllStringSubmatch("`"+cell, -1) {
				listed = append(listed, m[1])
			}
		}
	}
	for _, route := range routes {
		if !slices.Contains(listed, route) {
			t.Errorf("docs/ENDPOINTS.md has no row for %s, which %s routes", route, server)
		}
	}
}

// routePattern is the pattern a HandleFunc call routes: a string, a
// constant of debugpage.go, or a method and one of those joined with +.
func routePattern(expr ast.Expr, constants map[string]string) (string, bool) {
	switch e := expr.(type) {
	case *ast.BasicLit:
		value, err := strconv.Unquote(e.Value)
		return value, err == nil
	case *ast.Ident:
		value, ok := constants[e.Name]
		return value, ok
	case *ast.BinaryExpr:
		left, ok1 := routePattern(e.X, constants)
		right, ok2 := routePattern(e.Y, constants)
		return left + right, ok1 && ok2
	}
	return "", false
}

// The test-list sections of spec § 34 are numbered in order, each heading
// after a blank line: a section written as "34.x" while its number was
// unknown stayed so, between 34.162 and 34.163, after a line of text.
func TestSpecTestSectionsAreNumberedInOrder(t *testing.T) {
	lines := strings.Split(read(t, "docs/SPECIFICATION-EXPORTER.md"), "\n")
	heading := regexp.MustCompile(`^## 34\.([0-9]+)([a-z]?) `)
	last, lastSuffix, count := 0, "", 0
	for i, line := range lines {
		if !strings.HasPrefix(line, "## 34.") {
			continue
		}
		count++
		m := heading.FindStringSubmatch(line)
		if m == nil {
			t.Errorf("line %d: %q is not numbered 34.<number>", i+1, line)
			continue
		}
		if i > 0 && lines[i-1] != "" {
			t.Errorf("line %d: %q follows a line of text, not a blank line", i+1, line)
		}
		n, _ := strconv.Atoi(m[1])
		if n < last || n == last && m[2] <= lastSuffix {
			t.Errorf("line %d: %q comes after 34.%d%s", i+1, line, last, lastSuffix)
		}
		last, lastSuffix = n, m[2]
	}
	if count < 100 {
		t.Fatalf("found %d sections of § 34; the spec has moved", count)
	}
}
