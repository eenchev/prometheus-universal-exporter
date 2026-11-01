//go:build !select_request_types || request_type_localfile

package exporter

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// A localfile read's request line, on the debug page and in a debug probe's
// report alike, is the file's file:// URL escaped as a URL is, a space in
// the root and in the file's name each %20, as the url label is.
func TestALocalfileRequestLineIsAnEscapedFileURL(t *testing.T) {
	root := filepath.Join(t.TempDir(), "Claude outputs")
	testutil.WriteIn(t, root, "my app.prom", "file_series 7\n")
	cfg, err := config.Load(testutil.WriteFile(t, "config.yaml", "collectors:\n  - name: files\n    request: {type: localfile, root: '"+root+"'}\n    transform: {type: prometheus}\n"))
	if err != nil {
		t.Fatal(err)
	}
	server := newStaticServer(t, cfg, nil)
	server.logger = testutil.QuietLogger(t)
	server.SetDebug(DebugFull)
	want := "READ file://" + filepath.ToSlash(strings.ReplaceAll(root, " ", "%20")) + "/my%20app.prom"

	body, err := json.Marshal(map[string]any{"configured": "files", "target": "my app.prom"})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/debug/run", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Debug-Run", "run")
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, r)
	var answer debugAnswer
	if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil || recorder.Code != http.StatusOK || !answer.OK {
		t.Fatalf("the run answered %d: %s", recorder.Code, recorder.Body)
	}
	if len(answer.Requests) != 1 || answer.Requests[0].Method+" "+answer.Requests[0].URL != want {
		t.Errorf("the page's requests %+v, want %s", answer.Requests, want)
	}
	if !strings.Contains(answer.Report, "  "+want+"\n") {
		t.Errorf("the page's report has no line %q:\n%s", want, answer.Report)
	}

	recorder = httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/probe?collector=files&target=my%20app.prom&debug=true", nil))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "  "+want+"\n") {
		t.Errorf("the debug probe answered %d without the line %q:\n%s", recorder.Code, want, recorder.Body)
	}
}
