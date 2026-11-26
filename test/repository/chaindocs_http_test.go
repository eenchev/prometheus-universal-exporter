//go:build !select_request_types || request_type_http

package repository

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/exporter"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// The examples of REQUESTS.md's "Chained requests" load and, probed against
// a stub that answers each step's path as GitLab and Jenkins would, give
// the series the docs describe: the project's id sent in the second
// request's path, and each job's last build, the one that is not found
// skipped.
func TestChainedRequestsExamples(t *testing.T) {
	blocks := docBlocks(t, "docs/REQUESTS.md", "yaml")
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.EscapedPath() {
		case "/api/v4/projects/group%2Fapp":
			_, _ = io.WriteString(w, `{"id": 42, "path_with_namespace": "group/app"}`)
		case "/api/v4/projects/42/pipelines":
			if q := r.URL.Query(); q.Get("ref") != "main" || q.Get("per_page") != "1" {
				http.Error(w, "unexpected query "+r.URL.RawQuery, http.StatusBadRequest)
				return
			}
			_, _ = io.WriteString(w, `[{"id": 7, "status": "success"}]`)
		case "/api/json":
			_, _ = io.WriteString(w, `{"jobs": [{"name": "api"}, {"name": "web"}]}`)
		case "/job/api/lastBuild/api/json":
			_, _ = io.WriteString(w, `{"duration": 81000}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer stub.Close()
	for _, example := range []struct{ name, params, want string }{
		{"gitlab_pipeline", "&param_project=group%2Fapp", "gitlab_last_pipeline_success 1"},
		{"jenkins_jobs", "", `jenkins_last_build_seconds{job="api"} 81`},
	} {
		conf := docBlock(t, blocks, "collectors:\n  - name: "+example.name+"\n")
		path := testutil.WriteIn(t, t.TempDir(), "config.yaml", conf)
		cfg, err := config.Load(path)
		if err != nil {
			t.Fatalf("%s: %v\n%s", example.name, err, conf)
		}
		server := exporter.NewServer(config.NewManager(cfg, path, testutil.QuietLogger(t)), "python3", testutil.QuietLogger(t))
		recorder := httptest.NewRecorder()
		server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/probe?collector="+example.name+"&target="+url.QueryEscape(stub.URL)+example.params, nil))
		if body := recorder.Body.String(); recorder.Code != http.StatusOK || !strings.Contains(body, example.want+"\n") || strings.Contains(body, `job="web"`) {
			t.Errorf("%s: answered %d, want %q:\n%s", example.name, recorder.Code, example.want, body)
		}
	}
}
