//go:build !select_request_types || request_type_s3file

package exporter

import "testing"

// An s3file playground reads the pasted content and asks its
// request.endpoint for no object.
func TestAnS3FilePlaygroundMakesNoRequest(t *testing.T) {
	requirePlaygroundMakesNoRequest(t, playgroundCase{
		collector: func(name, addr string, _ bool) string {
			return "name: " + name + "\nrequest: {type: s3file, root: /exports, endpoint: 'http://" + addr + "', path_style: true, region: eu-west-1, path: app.prom}\ntransform: {type: prometheus}\n"
		},
		target: func(string) string { return "metrics" },
		body:   "pasted_series 2\n", series: "pasted_series 2\n",
	})
	requirePlaygroundRefusesFiles(t, "s3file")
}
