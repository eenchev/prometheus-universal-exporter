//go:build !select_request_types || request_type_localfile

package fetch

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// fileCollector is a localfile collector passing a Prometheus text file
// through.
func fileCollector(name, root, path string) model.Collector {
	return model.Collector{
		Name:          name,
		Request:       model.RequestConfig{Type: RequestTypeLocalFile, Root: root, Path: path},
		Transform:     model.TransformConfig{Type: "prometheus"},
		ErrorHandling: model.ErrorHandling{OnFetchError: "fail", OnDecodeError: "fail", OnTransformError: "fail"},
	}
}

// A file last modified longer ago than request.max_age is refused saying how
// long ago, and that age is no part of what the failure is to the log: read
// two hours old and three, the file fails with two texts, each naming its
// age, that are recognised by one, which names the file and the limit.
func TestAFileOlderThanMaxAgeIsOneFailureAtAnyAge(t *testing.T) {
	root := t.TempDir()
	file := testutil.WriteIn(t, root, "a.prom", "v 1\n")
	c := fileCollector("files", root, "a.prom")
	c.Request.MaxAge = model.Duration(time.Hour)
	var failures []error
	for _, age := range []string{"2h0m", "3h0m"} {
		old, err := time.ParseDuration(age)
		if err != nil {
			t.Fatal(err)
		}
		modified := time.Now().Add(-old)
		if err := os.Chtimes(file, modified, modified); err != nil {
			t.Fatal(err)
		}
		_, err = fetchLocalFile(context.Background(), "", &c, RequestOverrides{}, nil)
		if err == nil || !strings.Contains(err.Error(), "was last modified "+age) || !strings.Contains(err.Error(), "ago, longer than request.max_age 1h0m0s") {
			t.Fatalf("a file %s old: err=%v, want it refused for its age, which it names", age, err)
		}
		failures = append(failures, err)
	}
	a, b := model.SameFailureText(failures[0]), model.SameFailureText(failures[1])
	if failures[0].Error() == failures[1].Error() || a != b || !strings.Contains(a, file) || !strings.Contains(a, "longer than request.max_age 1h0m0s") {
		t.Errorf("the failures\n%v\n%v\nare recognised by\n%s\n%s\nwant two texts recognised by one, which names the file and the limit", failures[0], failures[1], a, b)
	}
}

// Transforms see the file as a response: its content type from the
// extension, its size and its modification time.
func TestLocalFileResponseHeaders(t *testing.T) {
	root := t.TempDir()
	file := testutil.WriteIn(t, root, "status.JSON", `{"a":1}`)
	modified := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := os.Chtimes(file, modified, modified); err != nil {
		t.Fatal(err)
	}
	c := fileCollector("files", root, "status.JSON")
	resp, err := fetchLocalFile(context.Background(), "", &c, RequestOverrides{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || string(resp.Body) != `{"a":1}` {
		t.Fatalf("resp=%+v", resp)
	}
	for header, want := range map[string]string{"Content-Type": "application/json", "Content-Length": "7", "Last-Modified": "Fri, 02 Jan 2026 03:04:05 GMT"} {
		if got := resp.Headers.Get(header); got != want {
			t.Errorf("%s=%q, want %q", header, got, want)
		}
	}
	for name, want := range map[string]string{
		"a.json": "application/json", "a.yml": "application/yaml", "a.yaml": "application/yaml", "a.xml": "application/xml",
		"a.csv": "text/csv", "a.htm": "text/html", "a.html": "text/html", "a.prom": "text/plain; version=0.0.4", "a.txt": "", "a": "",
		"a.graphite": GraphiteContentType, "a.CARBON": GraphiteContentType,
	} {
		if got := localFileContentType(name); got != want {
			t.Errorf("%s: %q, want %q", name, got, want)
		}
	}
}

// A file:// URL is a URL: its path is percent-decoded before it is read, so
// file:///root/a%20b.prom is the file "a b.prom", and a percent sign of a
// file's own name is written %25. What it decodes to is held to the same
// rules as any target. A plain path is not a URL and is taken as written.
func TestLocalFileURLTargetIsPercentDecoded(t *testing.T) {
	root := t.TempDir()
	testutil.WriteIn(t, root, "a b.prom", "spaced 1\n")
	testutil.WriteIn(t, root, "100%.prom", "percent 1\n")
	testutil.WriteIn(t, root, "a%20b.prom", "literal 1\n")
	testutil.WriteIn(t, filepath.Join(root, "sub"), "ü.prom", "umlaut 1\n")
	c := validated(t, fileCollector("files", root, ""))
	url := "file://" + filepath.ToSlash(root)
	for target, want := range map[string]string{
		url + "/a%20b.prom":          "spaced 1\n",
		url + "/a b.prom":            "spaced 1\n",
		url + "/100%25.prom":         "percent 1\n",
		url + "/a%2520b.prom":        "literal 1\n",
		url + "/sub/%C3%BC.prom":     "umlaut 1\n",
		url + "/sub%2F%c3%bc.prom":   "umlaut 1\n",
		"a%20b.prom":                 "literal 1\n",
		root + "/a%20b.prom":         "literal 1\n",
		url + "/sub/../a%20b.prom":   "spaced 1\n",
		url + "/sub/%2E%2E/a b.prom": "spaced 1\n",
	} {
		if err := CheckTarget(c, target, false); err != nil {
			t.Errorf("%q: refused: %v", target, err)
			continue
		}
		resp, err := FetchCollector(context.Background(), target, c, RequestOverrides{}, nil)
		if err != nil || string(resp.Body) != want {
			t.Errorf("%q: resp=%v err=%v, want the body %q", target, resp, err, want)
		}
	}
	for target, want := range map[string]string{
		url + "/100%.prom":            "write a % that is part of a file's name as %25",
		url + "/a%2":                  "is not a valid file:// URL",
		url + "/%2E%2E/%2E%2E/passwd": "is outside request.root",
		"file:///etc/%70asswd":        "is outside request.root",
		url + "/a%00b.prom":           "must not contain a NUL byte",
	} {
		err := CheckTarget(c, target, false)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err=%v, want one containing %q", target, err, want)
		}
		if _, err := FetchCollector(context.Background(), target, c, RequestOverrides{}, nil); err == nil {
			t.Errorf("%q was read", target)
		}
	}
}

// localFileTargetDir is the one place a target is interpreted.
func TestLocalFileTargetInterpretation(t *testing.T) {
	c := fileCollector("files", "/srv/metrics", "")
	for target, want := range map[string]string{
		"": "", "app.prom": "app.prom", "./app.prom": "app.prom", "a/b/../c": "a/c",
		"/srv/metrics": "", "/srv/metrics/": "", "/srv/metrics/a/b.prom": "a/b.prom", "file:///srv/metrics/a.prom": "a.prom",
	} {
		got, err := localFileTargetDir(&c, target)
		if err != nil || got != filepath.FromSlash(want) {
			t.Errorf("%q: got %q, %v; want %q", target, got, err, want)
		}
	}
	for _, target := range []string{"..", "../x", "/srv/metricsx/a", "/srv", "/etc/passwd", "file://srv/metrics", "a\x00b"} {
		if _, err := localFileTargetDir(&c, target); err == nil {
			t.Errorf("%q was accepted", target)
		}
	}
}

// A localfile path may hold a ? and a #, being a file's name, where the path
// of an http request may hold neither (TestAPathHoldsNoQuery).
func TestALocalFilePathMayHoldAQueryAndAFragment(t *testing.T) {
	file := model.Collector{Name: "f", Request: model.RequestConfig{Type: RequestTypeLocalFile, Root: t.TempDir(), Path: "odd?name#1.prom"}}
	if err := ValidateRequest(&file); err != nil {
		t.Fatalf("a localfile path with ? and #: %v", err)
	}
}
