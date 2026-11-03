package decode

import (
	"net/http"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// decoder none decodes nothing and fails on nothing: an empty body, HTML,
// broken JSON and a body in an encoding the exporter does not know are each
// handed on as none, with no document and the body as it was sent, which no
// conversion changed; a json decoder fails on the last two.
func TestDecoderNoneReadsNoBody(t *testing.T) {
	for _, answer := range []struct {
		body, contentType string
	}{
		{"", ""},
		{"<html><body>up</body></html>", "text/html"},
		{`{"broken":`, "application/json"},
		{"caf\xe9", "text/plain; charset=no-such-charset"},
		{"\xff\xfeu\x00p\x00", "text/plain"},
	} {
		r := &fetch.HTTPResponse{StatusCode: 200, Body: []byte(answer.body), Headers: http.Header{}}
		if answer.contentType != "" {
			r.Headers.Set("Content-Type", answer.contentType)
		}
		c := model.Collector{Request: model.RequestConfig{Type: fetch.RequestTypeHTTP}, Decoder: model.DecoderConfig{Type: model.DecoderNone}}
		d, err := Decode(r, &c)
		if err != nil || d.Kind != model.DecoderNone || d.Data != nil || string(d.Raw) != answer.body || string(r.Body) != answer.body {
			t.Errorf("%q: %+v, %v; want none, no document, the body as sent", answer.body, d, err)
		}
		if got := ConvertedFrom(r, &c, model.DecoderNone); got != "" {
			t.Errorf("%q: converted from %q", answer.body, got)
		}
	}
}
