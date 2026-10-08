//go:build !select_request_types || request_type_http

package exporter

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// A static target whose request writes its body as an alias of an anchor
// named path, or its path as an alias of one named body, sends what the
// document says, as YAML reads it: its own body to the collector's path, or
// the collector's body to its own path. The keys were matched by the
// anchors' names, so the first target overrode the path with nothing and the
// second the body, and the stand-in, which answers only the requests the
// document means, saw neither.
func TestAStaticTargetSendsThePathAndBodyItsAliasKeysName(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		seen = append(seen, r.URL.Path+" "+string(body))
		mu.Unlock()
		switch r.URL.Path + " " + string(body) {
		case "/collector the target's body":
			_, _ = fmt.Fprint(w, "value=4\n")
		case "/from-the-target the collector's body":
			_, _ = fmt.Fprint(w, "value=5\n")
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer target.Close()
	cfg, err := config.Load(testutil.WriteFile(t, "config.yaml", "collectors:\n  - name: text\n    request: {type: http, method: POST, path: /collector, body: the collector's body}\n    transform: {type: regex}\n    metrics:\n      - name: demo_value\n        expression: 'value=(\\d+)'\n"))
	if err != nil {
		t.Fatal(err)
	}
	targets := "x-names: [&path body, &body path]\ninterval: 1m\ntargets:\n" +
		"  - name: body\n    collector: text\n    target: " + target.URL + "\n    request: {*path : the target's body}\n" +
		"  - name: path\n    collector: text\n    target: " + target.URL + "\n    request: {*body : /from-the-target}\n"
	file, err := config.LoadStaticTargets(testutil.WriteFile(t, "targets.yaml", targets))
	if err != nil {
		t.Fatal(err)
	}
	server := newStaticServer(t, cfg, file)
	server.logger = testutil.QuietLogger(t)
	server.scrapeStaticTargets(t.Context(), 0)
	got := getStaticTargets(t, server, "/static-targets")
	for _, want := range []string{`demo_value{static_target="body"} 4`, `demo_value{static_target="path"} 5`} {
		if !strings.Contains(got, want) {
			mu.Lock()
			t.Errorf("no %s; the targets sent %q:\n%s", want, seen, got)
			mu.Unlock()
		}
	}
}
