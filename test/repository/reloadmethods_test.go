package repository

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/exporter"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// /-/reload takes POST and PUT, as Prometheus's own does, so a tool written
// for Prometheus reloads the exporter too. The pages that say how to reload
// over HTTP, the exporter's landing page among them, name every method the
// endpoint takes, read from its own Allow, and no other: they once said POST alone, and a reader took PUT for a
// mistake to be refused.
func TestTheReloadPagesNameEveryMethodTheEndpointTakes(t *testing.T) {
	const path = "configs/config.example.yaml"
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	server := exporter.NewServer(config.NewManager(cfg, path, testutil.QuietLogger(t)), "python3", testutil.QuietLogger(t))
	server.SetLifecycle(true)
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodDelete, "/-/reload", nil))
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("DELETE /-/reload answered %d, not 405", recorder.Code)
	}
	var allowed []string
	for method := range strings.SplitSeq(recorder.Header().Get("Allow"), ",") {
		allowed = append(allowed, strings.TrimSpace(method))
	}
	for _, method := range allowed {
		recorder := httptest.NewRecorder()
		server.Handler().ServeHTTP(recorder, httptest.NewRequest(method, "/-/reload", nil))
		if recorder.Code != http.StatusOK {
			t.Errorf("%s /-/reload, which Allow names, answered %d", method, recorder.Code)
		}
	}
	if len(allowed) < 2 {
		t.Errorf("/-/reload allows %q; POST and PUT were meant", allowed)
	}
	methods := regexp.MustCompile("`(GET|HEAD|POST|PUT|PATCH|DELETE|OPTIONS)`")
	for _, page := range []struct{ file, line string }{
		{"docs/ENDPOINTS.md", "| `/-/reload` |"},
		{"docs/FLAGS.md", "| `--web.enable-lifecycle` |"},
		{"charts/prometheus-universal-exporter/README.md", "`server.enableLifecycle` enables"},
		{"docs/CONFIGURATION.md", "/-/reload` answers:"},
	} {
		var found string
		for line := range strings.SplitSeq(read(t, page.file), "\n") {
			if strings.Contains(line, page.line) {
				found = line
				break
			}
		}
		if found == "" {
			t.Errorf("%s has no line with %q", page.file, page.line)
			continue
		}
		named := map[string]bool{}
		for _, match := range methods.FindAllStringSubmatch(found, -1) {
			named[match[1]] = true
		}
		for _, method := range allowed {
			if !named[method] {
				t.Errorf("%s says how /-/reload is used without naming %s, which it takes:\n%s", page.file, method, found)
			}
			delete(named, method)
		}
		for method := range named {
			t.Errorf("%s names %s for /-/reload, which answers it 405:\n%s", page.file, method, found)
		}
	}

	// The landing page, which lists /-/reload when the lifecycle API is
	// on, names them too.
	recorder = httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
	var item string
	for line := range strings.SplitSeq(recorder.Body.String(), "\n") {
		if strings.Contains(line, "/-/reload") {
			item = line
			break
		}
	}
	if item == "" {
		t.Fatalf("the landing page with the lifecycle API on does not list /-/reload:\n%s", recorder.Body.String())
	}
	named := map[string]bool{}
	for _, match := range regexp.MustCompile(`\b(GET|HEAD|POST|PUT|PATCH|DELETE|OPTIONS) /-/reload`).FindAllStringSubmatch(item, -1) {
		named[match[1]] = true
	}
	for _, method := range allowed {
		if !named[method] {
			t.Errorf("the landing page lists /-/reload without naming %s, which it takes:\n%s", method, item)
		}
		delete(named, method)
	}
	for method := range named {
		t.Errorf("the landing page names %s for /-/reload, which answers it 405:\n%s", method, item)
	}
}
