package repository

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// These tests hold facts the documentation repeats from the code — a list of
// words, a dependency, a function's name — in step with the code, so a change
// to one cannot leave the other quietly stating what is no longer true.

// stringList returns the strings of the []string literal the Go file path
// gives name first, as a variable (name = []string{...}) or as a field of a
// composite literal (name: []string{...}).
func stringList(t *testing.T, path, name string) []string {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var literal ast.Expr
	ast.Inspect(file, func(node ast.Node) bool {
		if literal != nil {
			return false
		}
		switch node := node.(type) {
		case *ast.ValueSpec:
			for i, ident := range node.Names {
				if ident.Name == name && i < len(node.Values) {
					literal = node.Values[i]
				}
			}
		case *ast.KeyValueExpr:
			if key, ok := node.Key.(*ast.Ident); ok && key.Name == name {
				literal = node.Value
			}
		}
		return literal == nil
	})
	composite, ok := literal.(*ast.CompositeLit)
	if !ok {
		t.Fatalf("%s gives %s no []string literal", path, name)
	}
	var words []string
	for _, element := range composite.Elts {
		basic, ok := element.(*ast.BasicLit)
		if !ok || basic.Kind != token.STRING {
			t.Fatalf("%s: %s holds something but strings", path, name)
		}
		word, err := strconv.Unquote(basic.Value)
		if err != nil {
			t.Fatal(err)
		}
		words = append(words, word)
	}
	return words
}

// Every page that lists what makes a name read as a credential's — the
// debug report's header redaction in docs/CONFIGURATION.md, the logs' in
// docs/LOGGING.md, the specification's rules — names every word of
// credentialWords and credentialParts in internal/fetch/redact.go. Such a
// list starts with `auth`, `cookie` and runs to the end of its paragraph.
func TestEveryCredentialNameListMatchesTheRedactionCode(t *testing.T) {
	const code = "internal/fetch/redact.go"
	want := map[string]bool{}
	for _, word := range append(stringList(t, code, "credentialWords"), stringList(t, code, "credentialParts")...) {
		want[word] = true
	}
	quoted := regexp.MustCompile("`([a-z]+)`")
	listStart := regexp.MustCompile("`auth`,\\s+`cookie`")
	lists := 0
	for _, path := range markdownFiles(t) {
		text := read(t, path)
		for offset := 0; ; {
			at := listStart.FindStringIndex(text[offset:])
			if at == nil {
				break
			}
			start := offset + at[0]
			end := strings.Index(text[start:], "\n\n")
			if end < 0 {
				end = len(text) - start
			}
			paragraph := text[start : start+end]
			offset = start + end
			lists++
			got := map[string]bool{}
			for _, m := range quoted.FindAllStringSubmatch(paragraph, -1) {
				got[m[1]] = true
			}
			for word := range want {
				if !got[word] {
					t.Errorf("%s: the list of credential words at %q leaves out %q, which %s has", path, firstLine(paragraph), word, code)
				}
			}
		}
	}
	if lists < 3 {
		t.Fatalf("found %d lists of credential words; the documentation has moved", lists)
	}
}

// firstLine is the start of text up to its first line break.
func firstLine(text string) string {
	line, _, _ := strings.Cut(text, "\n")
	return line
}

// The pages of the graphite, grpc and localfile request types say, in the
// text that opens their "Static targets" section, every key a static target's
// request block may set: the request type's TargetFields.
func TestEveryRequestTypePageNamesTheKeysAStaticTargetMaySet(t *testing.T) {
	for _, page := range []struct{ doc, code string }{
		{"docs/GRAPHITE.md", "internal/fetch/requesttype_graphite.go"},
		{"docs/GRPC.md", "internal/fetch/requesttype_grpc.go"},
		{"docs/LOCALFILE.md", "internal/fetch/requesttype_localfile.go"},
	} {
		text := read(t, page.doc)
		_, section, ok := strings.Cut(text, "\n## Static targets\n")
		if !ok {
			t.Fatalf("%s has no Static targets section", page.doc)
		}
		section, _, _ = strings.Cut(section, "```")
		for _, key := range stringList(t, page.code, "TargetFields") {
			if !strings.Contains(section, "`"+key+"`") {
				t.Errorf("%s: the Static targets section does not say a target may set %q, which %s accepts", page.doc, key, page.code)
			}
		}
	}
}

// The relabeling docs/PROMETHEUS-OPERATOR.md shows sends Prometheus where the
// chart's monitors send it: its replacement is the address the chart renders
// for the release and namespace the page names, exporter in monitoring.
func TestThePrometheusOperatorPageRelabelsToTheAddressTheChartRenders(t *testing.T) {
	helm := requireHelm(t)
	cmd := exec.Command(helm, "template", "exporter", chartDir, "--namespace", "monitoring", "-f", valuesFile(t, `monitors:
  - name: example
    enabled: true
    type: service
    collector: example
`)) // #nosec G204 -- the test's own arguments
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	replacement := regexp.MustCompile(`(?m)^\s*replacement: (\S+)$`)
	rendered := replacement.FindStringSubmatch(string(out))
	if rendered == nil {
		t.Fatalf("the chart renders no replacement:\n%s", out)
	}
	_, block, _ := strings.Cut(read(t, "docs/PROMETHEUS-OPERATOR.md"), "```yaml\n")
	block, _, _ = strings.Cut(block, "```")
	shown := replacement.FindStringSubmatch(block)
	if shown == nil {
		t.Fatalf("the first example of docs/PROMETHEUS-OPERATOR.md has no replacement:\n%s", block)
	}
	if shown[1] != rendered[1] {
		t.Fatalf("docs/PROMETHEUS-OPERATOR.md relabels __address__ to %s; the chart renders %s for release exporter in namespace monitoring", shown[1], rendered[1])
	}
}

// docs/DEVELOPMENT.md says where the schemas' rules by path are written by
// naming the functions that hold them; each name it gives ending in
// SchemaRules is a function of internal/config.
func TestTheSchemaRuleFunctionsDevelopmentNamesExist(t *testing.T) {
	declared := map[string]bool{}
	files, err := filepath.Glob("internal/config/*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil {
				declared[fn.Name.Name] = true
			}
		}
	}
	named := regexp.MustCompile("`([a-zA-Z]+SchemaRules)`").FindAllStringSubmatch(read(t, "docs/DEVELOPMENT.md"), -1)
	if len(named) < 2 {
		t.Fatalf("docs/DEVELOPMENT.md names %d schema rule functions; the paragraph has moved", len(named))
	}
	for _, m := range named {
		if !declared[m[1]] {
			t.Errorf("docs/DEVELOPMENT.md names %s, which internal/config does not declare", m[1])
		}
	}
}

// The "Go modules" section of docs/DEPENDENCIES.md names every module go.mod
// requires directly that the exporter's own code imports, so a dependency
// added to the build is said and explained there. A module is named by its
// path or by the end of it, as `gojq` or `antchfx/xpath`.
func TestDependenciesNamesEveryDirectDependencyOfTheExporter(t *testing.T) {
	var direct []string
	inBlock := false
	for _, line := range strings.Split(read(t, "go.mod"), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "require (":
			inBlock = true
			continue
		case inBlock && line == ")":
			inBlock = false
			continue
		case strings.HasPrefix(line, "require "):
			line = strings.TrimPrefix(line, "require ")
		case !inBlock:
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 || strings.Contains(line, "// indirect") {
			continue
		}
		direct = append(direct, fields[0])
	}
	if len(direct) < 5 {
		t.Fatalf("go.mod requires %d modules directly; it has moved", len(direct))
	}
	imported := map[string]bool{}
	err := filepath.WalkDir(".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if name := entry.Name(); path != "." && (strings.HasPrefix(name, ".") || name == "testdata" || name == "test") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, spec := range file.Imports {
			importPath, _ := strconv.Unquote(spec.Path.Value)
			for _, module := range direct {
				if importPath == module || strings.HasPrefix(importPath, module+"/") {
					imported[module] = true
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	_, section, ok := strings.Cut(read(t, "docs/DEPENDENCIES.md"), "\n## Go modules\n")
	if !ok {
		t.Fatal("docs/DEPENDENCIES.md has no Go modules section")
	}
	section, _, _ = strings.Cut(section, "\n## ")
	named := map[string]bool{}
	for _, m := range regexp.MustCompile("`([^`\\s]+)`").FindAllStringSubmatch(section, -1) {
		named[m[1]] = true
	}
	for _, module := range direct {
		if !imported[module] {
			continue
		}
		found := false
		for name := range named {
			if module == name || strings.HasSuffix(module, "/"+name) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("docs/DEPENDENCIES.md does not name %s, which go.mod requires and the exporter imports; say what it is for in its Go modules section", module)
		}
	}
}
