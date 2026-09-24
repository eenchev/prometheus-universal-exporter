package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// externalSuiteFiles are the files of the opt-in external end-to-end suite
// (docs/DEVELOPMENT.md). Every test in them must be selected by
// `make test-external`, which runs `-run TestExternal`, and skipped unless
// EXTERNAL_E2E is set. Adding a file to the suite means adding it here.
var externalSuiteFiles = []string{
	"internal/exporter/external_e2e_test.go",
	"internal/exporter/grafanastatus_external_e2e_test.go",
}

// This test itself runs in the default suite: it reads source, not the network.
func TestExternalSuiteTestsAreOptIn(t *testing.T) {
	for _, file := range externalSuiteFiles {
		parsed, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		tests := 0
		for _, decl := range parsed.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || !isTestFunc(fn) {
				continue
			}
			tests++
			name := fn.Name.Name
			if !strings.HasPrefix(name, "TestExternal") {
				t.Errorf("%s: %s is not named TestExternal*, so `make test-external` does not run it", file, name)
			}
			if !startsWithOptIn(fn) {
				t.Errorf("%s: %s does not start with requireExternalE2E(t), so it runs without EXTERNAL_E2E", file, name)
			}
		}
		if tests == 0 {
			t.Errorf("%s has no tests; remove it from externalSuiteFiles or add them", file)
		}
	}
}

func isTestFunc(fn *ast.FuncDecl) bool {
	if !strings.HasPrefix(fn.Name.Name, "Test") || fn.Type.Params == nil || len(fn.Type.Params.List) != 1 {
		return false
	}
	star, ok := fn.Type.Params.List[0].Type.(*ast.StarExpr)
	if !ok {
		return false
	}
	selector, ok := star.X.(*ast.SelectorExpr)
	return ok && selector.Sel.Name == "T"
}

func startsWithOptIn(fn *ast.FuncDecl) bool {
	if fn.Body == nil || len(fn.Body.List) == 0 {
		return false
	}
	statement, ok := fn.Body.List[0].(*ast.ExprStmt)
	if !ok {
		return false
	}
	call, ok := statement.X.(*ast.CallExpr)
	if !ok {
		return false
	}
	ident, ok := call.Fun.(*ast.Ident)
	return ok && ident.Name == "requireExternalE2E"
}
