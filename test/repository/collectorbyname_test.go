package repository

import (
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"strconv"
	"testing"
)

// collectorByNameUses is where a Go source uses model.CollectorByName, which
// goes through a configuration's collectors to find one, each as its line,
// and how many times it calls collectorsByName, which notes where each
// collector is by its name.
func collectorByNameUses(name string, src any) (scans []string, indexes int, err error) {
	files := token.NewFileSet()
	parsed, err := parser.ParseFile(files, name, src, 0)
	if err != nil {
		return nil, 0, err
	}
	ast.Inspect(parsed, func(node ast.Node) bool {
		switch node := node.(type) {
		case *ast.SelectorExpr:
			if pkg, ok := node.X.(*ast.Ident); ok && pkg.Name == "model" && node.Sel.Name == "CollectorByName" {
				scans = append(scans, strconv.Itoa(files.Position(node.Pos()).Line))
			}
		case *ast.CallExpr:
			if called, ok := node.Fun.(*ast.Ident); ok && called.Name == "collectorsByName" {
				indexes++
			}
		}
		return true
	})
	return scans, indexes, nil
}

// The check of a static target file against a configuration
// (internal/config/statictargets.go) and the search for the collectors whose
// descriptor files that check opens (namedfiles.go) find a target's collector
// where the collectors were noted once by their names (collectorsByName).
// Neither goes through the configuration's collectors for it
// (model.CollectorByName): the check did so four times for every target and
// the search once more for every configuration, which for thousands of
// targets and collectors was most of a reload. That the collectors are noted
// once for a check and not once for a target is what the tests beside the
// check count (collectorsIndexedHook).
func TestTheTargetFileCheckFindsNoCollectorByGoingThroughThem(t *testing.T) {
	for _, file := range []string{"internal/config/statictargets.go", "internal/config/namedfiles.go"} {
		scans, indexes, err := collectorByNameUses(file, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range scans {
			t.Errorf("%s:%s uses model.CollectorByName, which goes through every collector of the configuration; find the collector where collectorsByName noted it, once for the check", file, line)
		}
		if indexes == 0 {
			t.Errorf("%s does not call collectorsByName; the check looks for the wrong name", file)
		}
	}
	// The function the check must not use is still the one of that name.
	if scans, _, err := collectorByNameUses("probe.go", "package p\n\nfunc f() { _ = model.CollectorByName(nil, \"\") }\n"); err != nil || !slices.Equal(scans, []string{"3"}) {
		t.Errorf("a call of model.CollectorByName is found at %q, %v; want at line 3", scans, err)
	}
	if scans, indexes, err := collectorByNameUses("probe.go", "package p\n\n// model.CollectorByName is only named here.\nfunc f() { find := model.CollectorByName; _ = find; _ = collectorsByName(nil) }\n"); err != nil || !slices.Equal(scans, []string{"4"}) || indexes != 1 {
		t.Errorf("model.CollectorByName taken as a value is found at %q, with %d calls of collectorsByName, %v; want at line 4, not where a comment names it, and one call", scans, indexes, err)
	}
	declared, err := parser.ParseFile(token.NewFileSet(), "internal/model/config.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(declared.Decls, func(decl ast.Decl) bool {
		function, ok := decl.(*ast.FuncDecl)
		return ok && function.Recv == nil && function.Name.Name == "CollectorByName"
	}) {
		t.Error("internal/model/config.go no longer declares CollectorByName; the check looks for the wrong name")
	}
}
