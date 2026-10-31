//go:build !select_request_types || request_type_localfile

package exporter

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// With --web.debug.dangerous a localfile playground probed with no
// request_from reads the file its request names, under the root it names,
// as a configured localfile collector's probe reads it; without the flag it
// is told it makes no request of its own, and reads nothing.
func TestTheDangerousDebugModeLetsALocalfilePlaygroundReadAFile(t *testing.T) {
	root := t.TempDir()
	testutil.WriteIn(t, root, "app.prom", "file_series 7\n")
	cfg, err := config.Load(testutil.WriteFile(t, "config.yaml", "collectors:\n  - name: other\n    request: {type: localfile, root: '"+t.TempDir()+"'}\n    transform: {type: prometheus}\n"))
	if err != nil {
		t.Fatal(err)
	}
	playground := "name: files\nrequest: {type: localfile, root: '" + root + "'}\ntransform: {type: prometheus}\n"
	run := func(server *Server, code int) debugAnswer {
		t.Helper()
		body, err := json.Marshal(map[string]any{"collector": playground, "target": "app.prom"})
		if err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest(http.MethodPost, "/debug/run", bytes.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Debug-Run", "run")
		recorder := httptest.NewRecorder()
		server.Handler().ServeHTTP(recorder, r)
		var answer debugAnswer
		if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil || recorder.Code != code {
			t.Fatalf("answered %d, want %d: %s", recorder.Code, code, recorder.Body)
		}
		return answer
	}
	server := newStaticServer(t, cfg, nil)
	server.logger = testutil.QuietLogger(t)
	server.SetDebug(DebugFull)
	if answer := run(server, http.StatusBadRequest); !strings.Contains(answer.Error, "makes no request of its own") {
		t.Errorf("without the flag: %+v", answer)
	}
	server.SetDebugDangerous(true)
	answer := run(server, http.StatusOK)
	if !answer.OK || !strings.Contains(answer.Series, "file_series 7") || !answer.Dangerous {
		t.Errorf("with the flag: %+v", answer)
	}
}
