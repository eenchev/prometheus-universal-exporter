package decode

import (
	"net/http"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// Every decoder model.DecoderTypes lists has a case in Decode. A new decoder
// needs a sample here too, so adding one to the list without handling it fails.
func TestEveryListedDecoderIsHandled(t *testing.T) {
	samples := map[string]string{
		"json":       `{"a":1}`,
		"yaml":       "a: 1\n",
		"xml":        "<a>1</a>",
		"csv":        "a\n1\n",
		"html":       "<p>1</p>",
		"prometheus": "a 1\n",
		"text":       "a=1",
	}
	for _, decoder := range model.DecoderTypes {
		if decoder == "auto" {
			continue
		}
		t.Run(decoder, func(t *testing.T) {
			body, ok := samples[decoder]
			if !ok {
				t.Fatalf("no sample body for decoder %q; add one", decoder)
			}
			header := true
			c := &model.Collector{Name: "types", Decoder: model.DecoderConfig{Type: decoder}, Response: model.ResponseConfig{CSV: model.CSVConfig{Header: &header}}}
			d, err := Decode(&fetch.HTTPResponse{Body: []byte(body), Headers: http.Header{}}, c)
			if err != nil {
				t.Fatalf("err=%v", err)
			}
			if d.Kind != decoder {
				t.Fatalf("decoded as %q", d.Kind)
			}
		})
	}
	if _, err := Decode(&fetch.HTTPResponse{Body: []byte("x"), Headers: http.Header{}}, &model.Collector{Decoder: model.DecoderConfig{Type: "gopher"}}); err == nil || !strings.Contains(err.Error(), "unsupported decoder") {
		t.Fatalf("an unlisted decoder: err=%v", err)
	}
}
