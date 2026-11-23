//go:build !select_request_types || request_type_http

package exporter

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// A collector written on the debug page reads no file of the server's, and
// the files a protobuf decoder reads its message from are such files: one
// that names them is refused before it is checked, so no error of reading
// one can show what it holds.
func TestAPlaygroundRefusesTheProtobufDecodersFiles(t *testing.T) {
	server := NewServer(config.NewManager(&model.Config{}, "", testutil.QuietLogger(t)), "python3", testutil.QuietLogger(t))
	server.SetDebug(DebugFull)
	for _, files := range []string{"proto_files: [/etc/passwd]", "protoset_file: /etc/passwd", "proto_import_paths: [/etc]"} {
		collector := "name: x\nrequest: {type: http}\ndecoder: {type: protobuf}\nresponse:\n  protobuf:\n    message: a.B\n    " + files + "\ntransform: {type: jq}\nmetrics: [{name: up, expression: '1'}]\n"
		body, _ := json.Marshal(map[string]any{"collector": collector, "body": "{}"})
		r := httptest.NewRequest(http.MethodPost, "/debug/run", bytes.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Debug-Run", "run")
		recorder := httptest.NewRecorder()
		server.Handler().ServeHTTP(recorder, r)
		if recorder.Code != http.StatusUnprocessableEntity || !strings.Contains(recorder.Body.String(), "sets response.protobuf's files, which are files on the server") {
			t.Errorf("%s: %d %s", files, recorder.Code, recorder.Body)
		}
	}
}
