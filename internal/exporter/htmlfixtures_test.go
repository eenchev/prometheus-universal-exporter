//go:build !select_request_types || request_type_http

package exporter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/decode"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
	"github.com/eenchev/prometheus-universal-exporter/internal/transform"
)

// HTML is read by two transforms, css and xpath, and the pages it is read
// from are written for people: tables with headers that span, numbers with
// units, markup that no validator would pass. The fixtures under
// testdata/html are such pages, each in the shape of something a collector
// is pointed at, and the htmlfixtures_*_test.go files read every one of them
// with collectors written as a configuration file holds them:
//
//   - directly, through decode.Decode and transform.Transform, for breadth
//     (htmlfixtures_transform_test.go);
//   - through /probe, from a server answering with the Content-Type a real
//     one would send, for what the fetch, the choice of the decoder, the
//     limits and the exposition add (htmlfixtures_probe_test.go,
//     htmlfixtures_charset_test.go, htmlfixtures_large_test.go);
//   - from a file, with the localfile request type
//     (htmlfixtures_localfile_test.go).
//
// Every test names all the series it expects and all the log lines.

// htmlFixtures names every file under testdata/html and the page it stands
// for. A fixture is read through htmlFixture, which knows only these.
var htmlFixtures = map[string]string{
	"status.html":                                 "the smallest table of servers, on one line",
	"server-status.html":                          "a load balancer's statistics report in the manner of HAProxy's, on a page that starts as Apache's mod_status does and holds nginx's stub_status in a pre",
	"nested-tables.html":                          "the status page of a UPS network card: layout tables around data tables, an id used twice, tables within cells",
	"lists-and-cards.html":                        "a job queue's overview: definition lists, lists of name and value, a grid of dashboard cards",
	"sloppy.html":                                 "the outlet page of a rack PDU, as its firmware writes it: upper case, nothing closed, nothing quoted",
	"fragment.html":                               "a fragment an include serves, without html or body",
	"text-only.html":                              "a health endpoint that answers one line of text",
	"entities-whitespace.html":                    "a page of sensor readings written with entities, typographic signs, breaks, inline markup and comments in its cells",
	"not-content.html":                            "a build farm's page with what is in a page and is not its content: scripts, styles, a template, noscript, hidden rows, pre, svg, math, an iframe",
	"attribute-names.html":                        "a page of a JavaScript framework's making: attributes named by Open Graph, Vue and Alpine",
	"attribute-names.xhtml":                       "the same page as well-formed XHTML, which is read as XML when the target says it is XML",
	"value-forms.html":                            "a table of the forms a value takes on real pages, and a table of jobs with their times",
	"scrapethissite-countries.html":               "scrapethissite.com's Countries of the World, which examples/config.scrapethissite.html-test.yaml reads",
	"status-page.html":                            "a hosted status page: a summary, a table of components, a list of incidents",
	"charset/depots-utf8.html":                    "a page of depots named in Cyrillic, accented Latin, Polish, Japanese, Chinese, Korean and emoji: UTF-8, declaring nothing",
	"charset/depots-utf8-bom.html":                "the same after a UTF-8 byte order mark",
	"charset/depots-utf8-meta.html":               "the same with a meta charset",
	"charset/depots-utf16le-bom.html":             "the same as UTF-16LE after its byte order mark",
	"charset/depots-utf16be-bom.html":             "the same as UTF-16BE after its byte order mark",
	"charset/depots-utf8-http-equiv.html":         "the same with a meta http-equiv Content-Type",
	"charset/depots-windows-1251.html":            "the Cyrillic rows as windows-1251, declaring nothing",
	"charset/depots-windows-1251-meta.html":       "the Cyrillic rows as windows-1251 with a meta charset",
	"charset/depots-windows-1251-http-equiv.html": "the Cyrillic rows as windows-1251 with an upper-case meta http-equiv",
	"charset/depots-koi8-r-meta.html":             "a Russian row as KOI8-R with a meta charset",
	"charset/depots-iso-8859-1.html":              "the French row as ISO-8859-1, declaring nothing",
	"charset/depots-iso-8859-1-meta.html":         "the French and German rows as windows-1252, which the page calls iso-8859-1",
	"charset/depots-iso-8859-2-meta.html":         "the Polish row as ISO-8859-2 with an unquoted meta charset",
	"charset/depots-shift_jis-meta.html":          "the Japanese row as Shift_JIS with a meta charset",
	"charset/depots-gbk-meta.html":                "the Chinese row as GBK with a meta charset",
	"charset/depots-euc-kr-meta.html":             "the Korean row as EUC-KR with a meta charset",
}

// htmlFixture reads a fixture of testdata/html, which htmlFixtures lists.
func htmlFixture(t *testing.T, name string) []byte {
	t.Helper()
	if _, listed := htmlFixtures[name]; !listed {
		t.Fatalf("testdata/html/%s is not in htmlFixtures", name)
	}
	return readTestdata(t, "html/"+name)
}

// The list is the directory: a fixture added without saying what it stands
// for, and one listed and gone, fail here. And every fixture is read: its
// name stands in a test file besides this one.
func TestEveryHTMLFixtureIsListedAndRead(t *testing.T) {
	const root = "../../testdata/html"
	var files []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		name, err := filepath.Rel(root, path)
		files = append(files, filepath.ToSlash(name))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	listed := slices.Sorted(maps.Keys(htmlFixtures))
	if !slices.Equal(files, listed) {
		t.Errorf("testdata/html holds\n%s\nand htmlFixtures lists\n%s", strings.Join(files, "\n"), strings.Join(listed, "\n"))
	}

	tests, err := filepath.Glob("*_test.go")
	if err != nil {
		t.Fatal(err)
	}
	more, err := filepath.Glob("../transform/*_test.go")
	if err != nil {
		t.Fatal(err)
	}
	var sources strings.Builder
	for _, path := range append(tests, more...) {
		if path == "htmlfixtures_test.go" {
			continue
		}
		source, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		sources.Write(source)
	}
	for _, name := range listed {
		// By its path under testdata/html, or by its name in a directory a
		// test reads from.
		if !strings.Contains(sources.String(), `"`+name+`"`) && !strings.Contains(sources.String(), "/"+name+`"`) && !strings.Contains(sources.String(), `"`+filepath.Base(name)+`"`) {
			t.Errorf("no test reads testdata/html/%s", name)
		}
	}
}

// htmlCollectors loads a configuration written as a file holds it, so the
// collectors of these tests are checked as an operator's are.
func htmlCollectors(t *testing.T, document string) *model.Config {
	t.Helper()
	cfg, err := config.Load(testutil.WriteFile(t, "config.yaml", document))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// htmlCollector is the collector of cfg with that name.
func htmlCollector(t *testing.T, cfg *model.Config, name string) *model.Collector {
	t.Helper()
	for i := range cfg.Collectors {
		if cfg.Collectors[i].Name == name {
			return &cfg.Collectors[i]
		}
	}
	t.Fatalf("the configuration has no collector %q", name)
	return nil
}

// transformHTML decodes body, as an answer with that Content-Type, or
// without one when it is empty, and transforms it with the collector: the
// series as the exposition writes them, and the error of either stage.
func transformHTML(c *model.Collector, contentType string, body []byte) ([]string, error) {
	headers := http.Header{}
	if contentType != "" {
		headers.Set("Content-Type", contentType)
	}
	response := &fetch.HTTPResponse{StatusCode: http.StatusOK, Headers: headers, Body: slices.Clone(body)}
	decoded, err := decode.Decode(response, c)
	if err != nil {
		return nil, err
	}
	set, err := transform.Transform(context.Background(), decoded, response, c, "python3")
	if err != nil || set == nil {
		return nil, err
	}
	return samples(string(appendMetricSet(nil, set))), nil
}

// logLines are the captured log lines, each as its level, its message and
// the fields that say what happened — the rule, the stage, how many series,
// the error — without those that change from run to run. It empties the
// buffer.
func logLines(t *testing.T, logs *bytes.Buffer) []string {
	t.Helper()
	var lines []string
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		if line == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("this log line is not JSON:\n%s\n%v", line, err)
		}
		var text strings.Builder
		fmt.Fprintf(&text, "%v %v", record["level"], record["msg"])
		for _, field := range []string{"metric", "stage", "failures", "values", "first_metric", "error"} {
			if value, ok := record[field]; ok {
				fmt.Fprintf(&text, " %s=%v", field, value)
			}
		}
		lines = append(lines, text.String())
	}
	logs.Reset()
	return lines
}

// sameLogLines reports the log lines that differ from the ones wanted.
func sameLogLines(t *testing.T, logs *bytes.Buffer, want []string) {
	t.Helper()
	if got := logLines(t, logs); !slices.Equal(got, want) {
		t.Errorf("logged\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// htmlPage is what an htmlSite answers for one path: a body, and the
// Content-Type it is sent with, none when that is empty.
type htmlPage struct {
	contentType string
	body        []byte
}

// htmlSite is a web server holding pages by path. It says each answer's
// size, as a server of files does, and keeps the path and the Accept header
// of every request.
type htmlSite struct {
	*httptest.Server
	mu      sync.Mutex
	asked   []string
	accepts []string
}

func newHTMLSite(t *testing.T, pages map[string]htmlPage) *htmlSite {
	t.Helper()
	site := &htmlSite{}
	site.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		site.mu.Lock()
		site.asked = append(site.asked, r.URL.RequestURI())
		site.accepts = append(site.accepts, r.Header.Get("Accept"))
		site.mu.Unlock()
		page, known := pages[r.URL.Path]
		if !known {
			http.NotFound(w, r)
			return
		}
		if page.contentType == "" {
			// Set to nothing, the server does not make one up from the body.
			w.Header()["Content-Type"] = nil
		} else {
			w.Header().Set("Content-Type", page.contentType)
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(page.body)))
		_, _ = w.Write(page.body)
	}))
	t.Cleanup(site.Close)
	return site
}

func (s *htmlSite) requests() (paths, accepts []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.asked), slices.Clone(s.accepts)
}

// htmlServer is an exporter with the configuration, logging to the test's
// captured logs.
func htmlServer(cfg *model.Config) *Server {
	return NewServer(config.NewManager(cfg, "", slog.Default()), "python3", slog.Default())
}

// probeHTML probes the page at path of the site with a collector: the
// status, the series of a 200 and the body of any other answer.
func probeHTML(t *testing.T, server *Server, site *htmlSite, collector, path string) (status int, series []string, failure string) {
	t.Helper()
	response := probeOnce(t, server, "/probe?collector="+collector+"&target="+url.QueryEscape(site.URL+path), nil)
	if response.Code != http.StatusOK {
		return response.Code, nil, strings.TrimSpace(response.Body.String())
	}
	return response.Code, samples(response.Body.String()), ""
}
