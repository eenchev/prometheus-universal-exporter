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

// The shared template code of internal/fetch (requesttemplate.go) held a
// rule of one type's field: parse refused the filters that write no JSON
// when the field's place was request.message, grpc's, and replacedBy knew
// that a probe replaces request.message, request.body and graphite's
// request.targets[…] by those places. A rule keyed by a field's place is
// knowledge of one type in code every build carries, and a new type's rule
// would have been one more such comparison. Each is now a kind the type
// registers (templateFilterRules, templateReplacers, templateEscapers), so
// the files of internal/fetch that every build compiles, those without a
// request type's build constraint, compare no field's place, a templated
// field's where, with a literal.

// requestTypeConstraint is a build constraint naming a request type: the
// file is compiled only in builds that carry it.
var requestTypeConstraint = regexp.MustCompile(`(?m)^//go:build .*\brequest_type_[a-z0-9]+\b`)

// placeComparisons finds, in the Go source src of file, each comparison of a
// selector .where with a string literal: by == or !=, as the tag of a switch
// whose cases are literals, or by a strings function given both.
func placeComparisons(t *testing.T, file string, src any) []string {
	t.Helper()
	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, file, src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	isWhere := func(e ast.Expr) bool {
		sel, ok := e.(*ast.SelectorExpr)
		return ok && sel.Sel.Name == "where"
	}
	literal := func(e ast.Expr) (string, bool) {
		lit, ok := e.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return "", false
		}
		value, err := strconv.Unquote(lit.Value)
		return value, err == nil
	}
	var found []string
	report := func(n ast.Node, value string) {
		found = append(found, fmt.Sprintf("%s:%d compares a field's place with %q", file, fset.Position(n.Pos()).Line, value))
	}
	ast.Inspect(parsed, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.BinaryExpr:
			if n.Op != token.EQL && n.Op != token.NEQ {
				return true
			}
			for _, pair := range [][2]ast.Expr{{n.X, n.Y}, {n.Y, n.X}} {
				if value, ok := literal(pair[1]); ok && isWhere(pair[0]) {
					report(n, value)
				}
			}
		case *ast.CallExpr:
			sel, ok := n.Fun.(*ast.SelectorExpr)
			if !ok || len(n.Args) != 2 {
				return true
			}
			if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "strings" {
				return true
			}
			if value, ok := literal(n.Args[1]); ok && isWhere(n.Args[0]) {
				report(n, value)
			}
		case *ast.SwitchStmt:
			if n.Tag == nil || !isWhere(n.Tag) {
				return true
			}
			for _, stmt := range n.Body.List {
				for _, e := range stmt.(*ast.CaseClause).List {
					if value, ok := literal(e); ok {
						report(e, value)
					}
				}
			}
		}
		return true
	})
	return found
}

func TestTheSharedTemplateCodeKnowsNoFieldOfOneType(t *testing.T) {
	files, err := filepath.Glob("internal/fetch/*.go")
	if err != nil {
		t.Fatal(err)
	}
	shared := 0
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") || requestTypeConstraint.MatchString(read(t, file)) {
			continue
		}
		shared++
		for _, problem := range placeComparisons(t, filepath.ToSlash(file), nil) {
			t.Errorf("%s; register a rule of the field's kind from its request type's file (templateFilterRules, templateReplacers, templateEscapers) instead", problem)
		}
	}
	if shared < 10 || !slices.Contains(files, filepath.FromSlash("internal/fetch/requesttemplate.go")) {
		t.Errorf("only %d files of internal/fetch were found that every build compiles, or requesttemplate.go is gone; the check has stopped finding them", shared)
	}
}

// The check itself: the comparisons the shared code once made are each
// found, and a comparison of a field's kind, or of another selector, is not.
func TestThePlaceComparisonCheckFindsARuleOfOneField(t *testing.T) {
	src := `package fetch

import "strings"

func (f templateField) parse() bool {
	if f.where != "request.message" {
		return false
	}
	switch f.where {
	case "request.body":
	}
	if strings.HasPrefix(f.where, "request.targets[") || "request.x" == f.where {
		return true
	}
	return f.kind == "body" || f.name == "request.message" || strings.HasPrefix(f.text, "{{")
}
`
	found := placeComparisons(t, "internal/fetch/requesttemplate.go", src)
	want := []string{
		`internal/fetch/requesttemplate.go:6 compares a field's place with "request.message"`,
		`internal/fetch/requesttemplate.go:10 compares a field's place with "request.body"`,
		`internal/fetch/requesttemplate.go:12 compares a field's place with "request.targets["`,
		`internal/fetch/requesttemplate.go:12 compares a field's place with "request.x"`,
	}
	if !slices.Equal(found, want) {
		t.Errorf("the check found\n%s\nwant\n%s", strings.Join(found, "\n"), strings.Join(want, "\n"))
	}
}
