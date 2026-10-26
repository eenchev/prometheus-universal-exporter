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
		_, err := Decode(&fetch.HTTPResponse{Body: []byte(body), Headers: http.Header{}}, c)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%q: err=%v, want %q", body, err, want)
		}
		// The answer's start is a value of it, withheld from a probe's
		// caller; that it is empty is told.
		told := "the Graphite server did not answer with render JSON, a list of series; " + model.WithheldMark
		if strings.TrimSpace(body) == "" {
			told = "the Graphite server did not answer with render JSON, a list of series; the answer is empty"
		}
		if got := model.CallerText(err); got != told {
			t.Errorf("%q: the caller is told %q, want %q", body, got, told)
		}
	}
}
