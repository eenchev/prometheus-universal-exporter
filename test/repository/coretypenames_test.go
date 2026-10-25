package repository

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// What the exporter knows of a request type, beyond how it fetches, was
// spread over the core: the self-metric families it has, its target hint
// on the collectors page, its decoder, its schema rule, each a list or a
// switch naming the types, which every new type had to be added to. Each
// is now a setting of the type's registration (internal/fetch/requesttype.go),
// which the core reads (spec § 34). These tests keep the core packages from
// naming a type again: no string literal that is a request type's name and
// no RequestType<Name> constant, outside the literals below, which are the
// same words meaning something else.

// corePackages are the directories whose Go files, tests aside, name no
// request type.
var corePackages = []string{".", "internal/config", "internal/decode", "internal/exporter", "internal/transform"}

// coreTypeNameAllowed are the literals of the core packages that are a
// request type's name and mean something else, by file, each with how many
// times the file may hold it and why.
var coreTypeNameAllowed = []struct {
	file, literal string
	count         int
	why           string
}{
	{"internal/config/config.go", "graphite", 2, "the graphite decoder, which response.graphite applies to"},
	{"internal/config/config.go", "http", 2, "a URL's scheme, and the http reload trigger"},
	{"internal/config/configschema.go", "tcp", 2, "values of the keepalive check of sql's connections"},
	{"internal/decode/decoders.go", "graphite", 3, "the graphite decoder's name"},
	{"internal/decode/decoders.go", "sql", 1, "decode.KindRows, the format rows are shown as"},
	{"internal/decode/graphite.go", "graphite", 2, "the graphite decoder's name"},
	{"internal/exporter/lifecycle.go", "tcp", 1, "the network a port is looked up for"},
	{"internal/transform/inputs.go", "graphite", 2, "the graphite decoder, whose document jq and yq read"},
}

// requestTypeConstant is a RequestType<Name> constant of internal/fetch.
var requestTypeConstant = regexp.MustCompile(`^RequestType[A-Z][A-Za-z0-9]*$`)

// coreTypeNameUse is a literal or constant naming a request type, in a file.
type coreTypeNameUse struct {
	file, name string
	line       int
	constant   bool
}

// coreTypeNameUses finds, in the Go source src of file, the string literals
// that are one of types and the RequestType<Name> constants.
func coreTypeNameUses(t *testing.T, file string, src any, types []string) []coreTypeNameUse {
	t.Helper()
	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, file, src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	var uses []coreTypeNameUse
	ast.Inspect(parsed, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.BasicLit:
			if n.Kind != token.STRING {
				return true
			}
			if value, err := strconv.Unquote(n.Value); err == nil && slices.Contains(types, value) {
				uses = append(uses, coreTypeNameUse{file: file, name: value, line: fset.Position(n.Pos()).Line})
			}
		case *ast.Ident:
			if requestTypeConstant.MatchString(n.Name) {
				uses = append(uses, coreTypeNameUse{file: file, name: n.Name, line: fset.Position(n.Pos()).Line, constant: true})
			}
		}
		return true
	})
	return uses
}

// coreTypeNameProblems is what is wrong with the uses found: a constant
// anywhere, a literal the allowlist does not hold for its file, or more of
// one than it allows, and an entry of the allowlist that no longer matches.
func coreTypeNameProblems(uses []coreTypeNameUse) []string {
	var problems []string
	counts := map[[2]string]int{}
	for _, use := range uses {
		if use.constant {
			problems = append(problems, fmt.Sprintf("%s:%d names the request type constant %s; read what the core needs to know of a type from its registration (fetch.RequestType) instead", use.file, use.line, use.name))
			continue
		}
		counts[[2]string{use.file, use.name}]++
	}
	for key, n := range counts {
		allowed := 0
		for _, entry := range coreTypeNameAllowed {
			if entry.file == key[0] && entry.literal == key[1] {
				allowed = entry.count
			}
		}
		if n > allowed {
			problems = append(problems, fmt.Sprintf("%s holds the request type name %q %d times, %d of them allowed; read what the core needs to know of a type from its registration (fetch.RequestType) instead, or, for the same word meaning something else, add it to coreTypeNameAllowed with why", key[0], key[1], n, allowed))
		}
	}
	for _, entry := range coreTypeNameAllowed {
		if n := counts[[2]string{entry.file, entry.literal}]; n != entry.count {
			problems = append(problems, fmt.Sprintf("coreTypeNameAllowed allows %q %d times in %s, which holds it %d times; keep the allowlist exact", entry.literal, entry.count, entry.file, n))
		}
	}
	slices.Sort(problems)
	return problems
}

func TestCorePackagesNameNoRequestType(t *testing.T) {
	types := treeRequestTypes(t)
	var uses []coreTypeNameUse
	for _, dir := range corePackages {
		files, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		if len(files) == 0 {
			t.Fatalf("no Go files in %s; the core packages have moved", dir)
		}
		for _, file := range files {
			if strings.HasSuffix(file, "_test.go") {
				continue
			}
			uses = append(uses, coreTypeNameUses(t, filepath.ToSlash(file), nil, types)...)
		}
	}
	for _, problem := range coreTypeNameProblems(uses) {
		t.Error(problem)
	}
}

// The check itself: a constant, a literal in a file the allowlist does not
// hold it for, one more than allowed and a stale entry are each found, and
// a word that is no type's name is not.
func TestTheCoreTypeNameCheckFindsANamedType(t *testing.T) {
	types := []string{"dns", "grpc", "tcp"}
	src := `package x

import "fetch"

var families = map[string][]string{"family": {"grpc", "dns"}}

func f(t string) bool { return t == fetch.RequestTypeDNS || t == "dnsx" }

const network = "tcp"
`
	problems := coreTypeNameProblems(coreTypeNameUses(t, "internal/exporter/lifecycle.go", src, types))
	want := []string{
		`coreTypeNameAllowed allows "graphite" 2 times in internal/config/config.go, which holds it 0 times`,
		`internal/exporter/lifecycle.go holds the request type name "dns" 1 times, 0 of them allowed`,
		`internal/exporter/lifecycle.go holds the request type name "grpc" 1 times, 0 of them allowed`,
		`internal/exporter/lifecycle.go:7 names the request type constant RequestTypeDNS`,
	}
	for _, w := range want {
		if !slices.ContainsFunc(problems, func(p string) bool { return strings.HasPrefix(p, w) }) {
			t.Errorf("no problem starting %q in:\n%s", w, strings.Join(problems, "\n"))
		}
	}
	for _, p := range problems {
		if strings.Contains(p, `"tcp"`) && !strings.HasPrefix(p, "coreTypeNameAllowed") || strings.Contains(p, "dnsx") {
			t.Errorf("an allowed literal or a word that is no type's name is a problem: %s", p)
		}
	}
}
