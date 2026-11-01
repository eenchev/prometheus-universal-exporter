//go:build !select_request_types || request_type_http

package repository

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/exporter"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// documentedResourceSeries is the table of docs/SELF-METRICS.md, "Resource
// metrics": one row per family, its name in the first column.
var documentedResourceSeries = regexp.MustCompile("(?m)^\\| `((?:go|process)_[a-z_]+)` \\|")

// The table of resource metrics in docs/SELF-METRICS.md names exactly the
// families /self-metrics publishes with the default configuration: a series
// the runtime stops giving, or one added without its row, shows up here.
func TestResourceMetricsDocumentationListsWhatIsPublished(t *testing.T) {
	var documented []string
	for _, match := range documentedResourceSeries.FindAllStringSubmatch(read(t, "docs/SELF-METRICS.md"), -1) {
		documented = append(documented, match[1])
	}
	if len(documented) < 20 {
		t.Fatalf("found %d rows in the table of resource metrics; the table has moved", len(documented))
	}

	path := testutil.WriteIn(t, t.TempDir(), "config.yaml", testutil.MinimalConfig)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	server := exporter.NewServer(config.NewManager(cfg, path, testutil.QuietLogger(t)), "python3", testutil.QuietLogger(t))
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, exporter.DefaultSelfMetricsPath, nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("/self-metrics answered %d", recorder.Code)
	}
	var published []string
	for _, line := range strings.Split(recorder.Body.String(), "\n") {
		if fields := strings.Fields(line); len(fields) == 4 && fields[1] == "TYPE" &&
			(strings.HasPrefix(fields[2], "go_") || strings.HasPrefix(fields[2], "process_")) {
			published = append(published, fields[2])
		}
	}
	// The process_ series are read from /proc and are absent without it.
	if _, err := os.Stat("/proc/self/stat"); err != nil {
		documented = slices.DeleteFunc(documented, func(name string) bool { return strings.HasPrefix(name, "process_") })
	}
	slices.Sort(documented)
	slices.Sort(published)
	if !slices.Equal(documented, published) {
		t.Fatalf("docs/SELF-METRICS.md documents\n%q\nand /self-metrics publishes\n%q", documented, published)
	}
}

// runtime.ReadMemStats stops the world to fill its struct, which a scrape of
// the resource metrics must not do; only test helpers may call it.
func TestNothingButTestsReadsMemStats(t *testing.T) {
	err := filepath.WalkDir(".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if name := entry.Name(); name == "vendor" || name == ".git" || name == "testdata" {
				return filepath.SkipDir
			}
			if filepath.ToSlash(path) == "internal/testutil" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		parsed, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(parsed, func(node ast.Node) bool {
			if selector, ok := node.(*ast.SelectorExpr); ok && selector.Sel.Name == "ReadMemStats" {
				t.Errorf("%s:%d calls ReadMemStats, which stops the world; read runtime/metrics instead", path, fset.Position(selector.Pos()).Line)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
