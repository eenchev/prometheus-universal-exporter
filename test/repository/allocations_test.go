package repository

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// alloctestPackage is where the tests' one measure of allocations lives
// (docs/DEVELOPMENT.md, "Repeatable tests").
const alloctestPackage = "internal/testutil/alloctest"

// allocationCounters are the counts of what the whole process allocated that
// a test reads only through alloctest, by the name each is selected by:
// testing.AllocsPerRun, the counters of runtime.MemStats and what a
// testing.BenchmarkResult says of them.
var allocationCounters = []string{"AllocsPerRun", "Mallocs", "Frees", "TotalAlloc", "AllocsPerOp", "AllocedBytesPerOp"}

// countedDirectly is where a Go source reads one of allocationCounters, each
// as line: name.
func countedDirectly(name string, src any) ([]string, error) {
	files := token.NewFileSet()
	parsed, err := parser.ParseFile(files, name, src, 0)
	if err != nil {
		return nil, err
	}
	var found []string
	ast.Inspect(parsed, func(node ast.Node) bool {
		if selector, ok := node.(*ast.SelectorExpr); ok && slices.Contains(allocationCounters, selector.Sel.Name) {
			found = append(found, fmt.Sprintf("%d: %s", files.Position(selector.Sel.Pos()).Line, selector.Sel.Name))
		}
		return true
	})
	return found, nil
}

// The allocations the runtime counts are those of the whole process, so a
// count taken while the goroutines of other tests still run is now and then
// too high, and a test that bounds it fails on a busy machine. No test
// counts them itself: each measures through alloctest, which takes the least
// of several measurements.
func TestAllocationsAreMeasuredOnlyThroughAlloctest(t *testing.T) {
	measuring := 0
	err := filepath.WalkDir(".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || !strings.HasSuffix(path, "_test.go") {
			return err
		}
		if strings.HasPrefix(filepath.ToSlash(path), "internal/testutil/") {
			return nil
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, spec := range parsed.Imports {
			if strings.HasSuffix(spec.Path.Value, "/"+alloctestPackage+`"`) {
				measuring++
			}
		}
		found, err := countedDirectly(path, nil)
		for _, counter := range found {
			t.Errorf("%s:%s is read directly; what the process allocates is measured through %s, which takes the least of several measurements", path, counter, alloctestPackage)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if measuring < 20 {
		t.Errorf("%d test files import %s; the tests that bound allocations are more, so the check looks for the wrong package", measuring, alloctestPackage)
	}
}

// The check finds each counter where it is read - called, taken as a value,
// read from a variable of any name - and not where it is only named, in a
// comment or a text, nor a measurement through alloctest or what the heap
// holds, which is no count of allocations.
func TestTheCheckOfAllocationCountersFindsEachWhereItIsRead(t *testing.T) {
	for name, tc := range map[string]struct {
		body string
		want []string
	}{
		"testing.AllocsPerRun":   {`n := testing.AllocsPerRun(5, f); _ = n`, []string{"4: AllocsPerRun"}},
		"taken as a value":       {`count := testing.AllocsPerRun; _ = count`, []string{"4: AllocsPerRun"}},
		"the counters of memory": {"var m runtime.MemStats\nruntime.ReadMemStats(&m)\n_, _, _ = m.Mallocs, m.Frees,\nm.TotalAlloc", []string{"6: Mallocs", "6: Frees", "7: TotalAlloc"}},
		"a benchmark's result":   {`r := testing.Benchmark(nil); _, _ = r.AllocsPerOp(), r.AllocedBytesPerOp()`, []string{"4: AllocsPerOp", "4: AllocedBytesPerOp"}},
		"through alloctest":      {`n, b := alloctest.Allocations(5, f); _, _ = n, alloctest.AllocsAtMost(5, 1, f) + float64(alloctest.BytesAtMost(5, b, f))`, nil},
		"what the heap holds":    {"var m runtime.MemStats\nruntime.ReadMemStats(&m)\n_ = m.HeapAlloc", nil},
		"only named":             {"// testing.AllocsPerRun and m.Mallocs\n_ = \"m.TotalAlloc\"", nil},
		"a benchmark's report":   {`var b *testing.B; b.ReportAllocs()`, nil},
	} {
		got, err := countedDirectly(name+"_test.go", "package p\n\nfunc f() {\n"+tc.body+"\n}\n")
		if err != nil || !slices.Equal(got, tc.want) {
			t.Errorf("%s: found %q, %v; want %q", name, got, err, tc.want)
		}
	}
}
