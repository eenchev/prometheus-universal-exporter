//go:build !select_request_types || request_type_kubernetes

package exporter

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// A kubernetes collector lists what its configuration names, and takes no
// target: the debug page says so in place of the target's field, and a run
// probed through it with none is made, not refused for want of one.
func TestTheDebugPageProbesAKubernetesCollectorWithNoTarget(t *testing.T) {
	server := debugFormServer(t, `  - name: pods
    request:
      type: kubernetes
      api_server: https://127.0.0.1:1
      bearer_token_file: `+testutil.WriteFile(t, "token", "t\n")+`
      resource: pods
      all_namespaces: true
    transform: {type: jq}
    metrics: [{name: kube_pods, expression: ".items | length"}]
`)
	page, form := debugFormPage(t, server, "pods")
	want := debugForm{RequestType: "kubernetes", Answer: "Paste the objects listed as the JSON document the exporter makes of them, as the Response of a run shows it", NoTarget: true}
	if form != want {
		t.Errorf("the form is %+v, want %+v", form, want)
	}
	requireDebugForm(t, page, form)
	requireInPage(t, page, `request.target = through.no_target ? "" : $("target").value;`)
	run, _ := json.Marshal(map[string]any{"configured": "pods", "target": "", "params": map[string]string{}})
	request := httptest.NewRequest(http.MethodPost, "/debug/run", bytes.NewReader(run))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Debug-Run", "run")
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	var answer debugAnswer
	if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil || recorder.Code != http.StatusOK || answer.Error != "" || !strings.Contains(answer.Report, "kubernetes") {
		t.Errorf("a run with no target answered %d %v: %s", recorder.Code, err, recorder.Body)
	}
}
