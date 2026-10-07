//go:build !select_request_types || request_type_graphite

package exporter

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
)

// A graphite collector's label values take the probe's parameters as its
// expressions do. A value a label takes is written as given: only one that
// also lands in an expression is held to what an expression may hold.
func TestAGraphiteCollectorsLabelsTakeAProbeParameter(t *testing.T) {
	graphite := &fakeGraphite{}
	upstream := httptest.NewServer(graphite)
	defer upstream.Close()
	server := labelServer(t, `collectors:
  - name: graphite_app
    request:
      type: graphite
      targets:
        - "app.{{param_host:web01}}.requests"
    response:
      graphite:
        max_age: 10m
    transform:
      type: jq
      labels:
        env: "{{param_env:prod}}"
    metrics:
      - name: app_requests
        items: .series[] | select(.segments[1] == "web01")
        expression: .value
        labels:
          - {name: host, expression: ".segments[1]"}
          - {name: source, value: "graphite-{{param_env:prod}}"}
`)
	probe := "/probe?collector=graphite_app&target=" + url.QueryEscape(upstream.URL)
	for name, tc := range map[string]struct{ query, want string }{
		"the defaults":            {"", `app_requests{env="prod",host="web01",source="graphite-prod"} 42`},
		"a label's parameter":     {"&param_env=staging", `app_requests{env="staging",host="web01",source="graphite-staging"} 42`},
		"a value no target takes": {"&param_env=" + url.QueryEscape(`eu, "west" (1)`), `app_requests{env="eu, \"west\" (1)",host="web01",source="graphite-eu, \"west\" (1)"} 42`},
		"both":                    {"&param_env=staging&param_host=web02", `app_requests{env="staging",host="web01",source="graphite-staging"} 42`},
	} {
		got := probeOnce(t, server, probe+tc.query, nil)
		if got.Code != http.StatusOK || !slices.Equal(seriesLines(got.Body.String()), []string{tc.want}) {
			t.Errorf("%s: %d\n%s", name, got.Code, got.Body.String())
		}
	}
	// The parameter of the expression went into it for the one probe that
	// gave it, and the default for the others.
	asked := graphite.asked()
	if given := slices.IndexFunc(asked, func(targets []string) bool { return slices.Equal(targets, []string{"app.web02.requests"}) }); len(asked) != 4 || given < 0 {
		t.Errorf("the targets asked for: %v", asked)
	}
	for name, tc := range map[string]struct{ query, want string }{
		"a parameter nothing uses":       {"&param_evn=staging", `probe parameters param_evn are not used by collector "graphite_app": no placeholder in its request.targets, its request.path ("/render"), its header or query values or its label values names them`},
		"a target's value is held to it": {"&param_host=" + url.QueryEscape("web*"), "request.targets[0]: the value of param_host"},
	} {
		got := probeOnce(t, server, probe+tc.query, nil)
		if got.Code != http.StatusBadRequest || !strings.Contains(got.Body.String(), tc.want) {
			t.Errorf("%s: %d %s\nwant 400 with %s", name, got.Code, got.Body.String(), tc.want)
		}
	}
}
