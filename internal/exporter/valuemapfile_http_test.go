//go:build !select_request_types || request_type_http

package exporter

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// A probe's series are mapped by the maps read from the rules' files: a
// rule's value from its text, a label's value to its name, an entry
// written in the rule winning over the file's and "*" of the file taking
// any other value.
func TestAProbeMapsValuesByAFile(t *testing.T) {
	dir := t.TempDir()
	states := testutil.WriteIn(t, dir, "states.yaml", "up: 1\ndown: 0\n\"*\": -1\n")
	sites := testutil.WriteIn(t, dir, "sites.yaml", "fra1: Frankfurt\nams2: Amsterdam\n")
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"site":"fra1","state":"up"},{"site":"ams2","state":"down"},{"site":"sof1","state":"rebooting"}]`))
	}))
	t.Cleanup(target.Close)
	server := labelServer(t, `collectors:
  - name: devices
    request: {type: http}
    transform: {type: jq}
    metrics:
      - name: device_state
        items: .[]
        expression: .state
        value_map_file: `+states+`
        labels:
          - name: site
            expression: .site
            value_map: {ams2: AMS}
            value_map_file: `+sites+`
`)
	recorder := probeOnce(t, server, "/probe?collector=devices&target="+url.QueryEscape(target.URL), nil)
	want := `device_state{site="AMS"} 0` + "\n" + `device_state{site="Frankfurt"} 1` + "\n" + `device_state{site="sof1"} -1`
	if got := strings.Join(seriesLines(recorder.Body.String()), "\n"); recorder.Code != http.StatusOK || got != want {
		t.Errorf("%d\n%s\nwant\n%s", recorder.Code, got, want)
	}
}

// A collector written on the debug page names no value_map_file: the file
// is the server's, and its entries would label the series shown.
func TestAPlaygroundNamesNoValueMapFile(t *testing.T) {
	server := NewServer(config.NewManager(&model.Config{}, "", testutil.QuietLogger(t)), "python3", testutil.QuietLogger(t))
	server.SetDebug(DebugFull)
	secret := testutil.WriteIn(t, t.TempDir(), "secrets.yaml", "password: hunter2\n")
	for _, rule := range []string{
		"{name: up, expression: .a, value_map_file: " + secret + "}",
		"{name: up, expression: '1', labels: [{name: l, expression: .a, value_map_file: " + secret + "}]}",
	} {
		body, _ := json.Marshal(map[string]any{"collector": "name: x\nrequest: {type: http}\ntransform: {type: jq}\nmetrics: [" + rule + "]\n", "body": `{"a": "password"}`})
		r := httptest.NewRequest(http.MethodPost, "/debug/run", bytes.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Debug-Run", "run")
		recorder := httptest.NewRecorder()
		server.Handler().ServeHTTP(recorder, r)
		if recorder.Code != http.StatusUnprocessableEntity || !strings.Contains(recorder.Body.String(), "sets value_map_file, which names a file on the server") || strings.Contains(recorder.Body.String(), "hunter2") {
			t.Errorf("%s: %d %s", rule, recorder.Code, recorder.Body)
		}
	}
}
