package transform

import (
	"slices"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/decode"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The table of which transform reads which decoder is the one both the load
// and the scrape consult. It lists every transform but python, which reads
// them all, only decoders that exist, and leaves no decoder without a
// transform of its own to read it; a transform or decoder added to the model
// without a place in it fails here.
func TestEveryTransformSaysWhatItReads(t *testing.T) {
	read := map[string]bool{}
	for _, transformType := range model.TransformTypes {
		decoders := ReadableDecoders(transformType)
		if transformType == "python" {
			if decoders != nil {
				t.Errorf("python reads every decoder, but is listed with %v", decoders)
			}
			continue
		}
		if len(decoders) == 0 {
			t.Errorf("transform %q is not in the table of what each transform reads; add it", transformType)
		}
		for _, decoder := range decoders {
			read[decoder] = true
			if decoder == "auto" || !slices.Contains(model.DecoderTypes, decoder) {
				t.Errorf("transform %q is listed with %q, which is not a decoder", transformType, decoder)
			}
		}
	}
	for _, decoder := range model.DecoderTypes {
		if decoder != "auto" && !read[decoder] {
			t.Errorf("no transform but python reads decoder %q", decoder)
		}
	}
}

// What the load refuses is what the scrape refuses: for every decoder and
// transform, a collector naming both loads exactly when a document of that
// decoder passes the scrape's own check.
func TestTheLoadAndTheScrapeAgreeOnWhatATransformReads(t *testing.T) {
	for _, transformType := range model.TransformTypes {
		for _, decoder := range model.DecoderTypes {
			if decoder == "auto" {
				continue
			}
			atLoad := CheckDecoder(&model.Collector{Name: "a", Decoder: model.DecoderConfig{Type: decoder}, Transform: model.TransformConfig{Type: transformType}})
			atScrape := validateTransformInput(&decode.Decoded{Kind: decoder}, transformType)
			if (atLoad == nil) != (atScrape == nil) {
				t.Errorf("decoder %s with transform %s: load says %v, scrape says %v", decoder, transformType, atLoad, atScrape)
			}
			if atLoad != nil && !strings.Contains(atLoad.Error(), "decodes with "+decoder+", which its "+transformType+" transform cannot read") {
				t.Errorf("decoder %s with transform %s: %v", decoder, transformType, atLoad)
			}
		}
	}
}
