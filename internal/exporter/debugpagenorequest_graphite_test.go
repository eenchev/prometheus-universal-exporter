//go:build !select_request_types || request_type_graphite

package exporter

import (
	"strconv"
	"testing"
	"time"
)

// A graphite playground reads a pasted render answer and sends no request:
// one sent through its proxy would connect to it.
func TestAGraphitePlaygroundMakesNoRequest(t *testing.T) {
	now := strconv.FormatInt(time.Now().Unix(), 10)
	requirePlaygroundMakesNoRequest(t, playgroundCase{
		collector: func(name, addr string, source bool) string {
			proxy := ", proxy_url: 'http://" + addr + "'"
			if source {
				proxy = ""
			}
			return "name: " + name + "\nrequest: {type: graphite, targets: [app.requests]" + proxy + "}\ntransform: {type: jq}\nmetrics: [{name: app_requests, items: '.series[]', expression: .value}]\n"
		},
		target: func(addr string) string { return "http://" + addr },
		body:   `[{"target": "app.requests", "tags": {"name": "app.requests"}, "datapoints": [[5, ` + now + `]]}]`, series: "app_requests 5\n",
	})
	requirePlaygroundRefusesFiles(t, "graphite")
}
