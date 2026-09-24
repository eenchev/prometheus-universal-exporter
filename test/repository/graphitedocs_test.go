//go:build !select_request_types || request_type_graphite

package repository

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/exporter"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// docBlocks returns the fenced blocks of a page in the given language.
func docBlocks(t *testing.T, path, language string) []string {
	t.Helper()
	var blocks []string
	for _, part := range strings.Split(read(t, path), "```"+language+"\n")[1:] {
		block, _, _ := strings.Cut(part, "```")
		blocks = append(blocks, block)
	}
	return blocks
}

func docBlock(t *testing.T, blocks []string, prefix string) string {
	t.Helper()
	for _, block := range blocks {
		if strings.HasPrefix(block, prefix) {
			return block
		}
	}
	t.Fatalf("no example starts %q", prefix)
	return ""
}

// The examples in docs/GRAPHITE.md load, and the collector example maps a
// render API's answer as the page says, asking for what the page says it
// asks for.
func TestGraphiteDocumentationExamples(t *testing.T) {
	blocks := docBlocks(t, "docs/GRAPHITE.md", "yaml")
	var asked url.Values
	graphite := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = r.URL.Query()
		now := time.Now().Unix()
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `[{"target":"app.web01.requests.count","tags":{"name":"app.web01.requests.count"},"datapoints":[[40,%d],[42,%d]]},
			{"target":"cpu.load;env=staging","tags":{"name":"cpu.load","env":"staging"},"datapoints":[[0.5,%d]]}]`, now-120, now-60, now-60)
	}))
	defer graphite.Close()

	conf := docBlock(t, blocks, "collectors:\n  - name: graphite_app\n")
	path := testutil.WriteIn(t, t.TempDir(), "config.yaml", conf)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("%v\n%s", err, conf)
	}
	server := exporter.NewServer(config.NewManager(cfg, path, testutil.QuietLogger(t)), "python3", testutil.QuietLogger(t))
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/probe?collector=graphite_app&param_env=staging&target="+url.QueryEscape(graphite.URL), nil))
	for _, want := range []string{`app_requests{host="web01"} 42`, `cpu_load{env="staging"} 0.5`} {
		if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), want) {
			t.Fatalf("missing %q: %d\n%s", want, recorder.Code, recorder.Body)
		}
	}
	if got := strings.Join(asked["target"], " | "); got != "app.*.requests.count | seriesByTag('name=cpu.load', 'env=staging')" || asked.Get("from") != "-15min" || asked.Get("until") != "now" || asked.Get("format") != "json" {
		t.Fatalf("asked %v", asked)
	}

	// The static targets example loads against the collector example.
	targets := testutil.WriteIn(t, t.TempDir(), "static-targets.yaml", docBlock(t, blocks, "interval: 1m\n"))
	file, err := config.LoadStaticTargets(targets)
	if err == nil {
		err = config.ValidateStaticTargets(file)
	}
	if err == nil {
		err = config.ValidateStaticTargetsAgainst(file, cfg)
	}
	if err != nil {
		t.Fatal(err)
	}

	// So do the Python example and the response.graphite snippet.
	python := docBlock(t, blocks, "collectors:\n  - name: graphite_peaks\n")
	if _, err := config.Load(testutil.WriteIn(t, t.TempDir(), "config.yaml", python)); err != nil {
		t.Fatalf("%v\n%s", err, python)
	}
	snippet := docBlock(t, blocks, "response:\n  graphite:\n")
	withSnippet := strings.Replace(conf, "    response:\n      graphite:\n        max_age: 5m\n", indent(snippet, "    "), 1)
	if _, err := config.Load(testutil.WriteIn(t, t.TempDir(), "config.yaml", withSnippet)); err != nil {
		t.Fatalf("%v\n%s", err, withSnippet)
	}
	window := "collectors:\n  - name: window\n" + indent(docBlock(t, blocks, "request:\n  type: graphite\n"), "    ") + "    transform:\n      type: jq\n"
	if _, err := config.Load(testutil.WriteIn(t, t.TempDir(), "config.yaml", window)); err != nil {
		t.Fatalf("%v\n%s", err, window)
	}
}

func indent(block, prefix string) string {
	var b strings.Builder
	for _, line := range strings.SplitAfter(block, "\n") {
		if line != "" {
			b.WriteString(prefix + line)
		}
	}
	return b.String()
}
