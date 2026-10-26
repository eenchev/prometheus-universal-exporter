//go:build !select_request_types || request_type_graphite

package decode

import (
	"net/http"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The decoder tells a graphite collector by its type's registration
// (fetch.RequestType.Decoder), so these tests need graphite in the build.

// A graphite collector's answer is render JSON, or it is refused as what it
// is, not read as carbon lines.
func TestAGraphiteAnswerMustBeRenderJSON(t *testing.T) {
	c := &model.Collector{Request: model.RequestConfig{Type: fetch.RequestTypeGraphite}, Decoder: model.DecoderConfig{Type: "graphite"}}
	for body, want := range map[string]string{
		"<!DOCTYPE html><html><head><title>Sign in</title></head><body>" + strings.Repeat("x", 100): `the Graphite server did not answer with render JSON, a list of series; the answer starts "<!DOCTYPE html><html><head><title>Sign in</title></head><body>xxxxxxxxxxxxxxxxxx"…`,
		`{"error": "unknown function"}`: `the answer is "{\"error\": \"unknown function\"}"`,
		"  \n":                          "the answer is empty",
		"a.b 1 1727000000\n":            `the answer is "a.b 1 1727000000"`,
	} {
		if _, err := Decode(&fetch.HTTPResponse{Body: []byte(body), Headers: http.Header{}}, c); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err=%v, want %q", body, err, want)
		}
	}
}
