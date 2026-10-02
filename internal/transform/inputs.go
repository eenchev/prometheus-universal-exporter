package transform

import (
	"fmt"
	"slices"
	"strings"

	"github.com/eenchev/prometheus-universal-exporter/internal/decode"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// transformInput is what a transform reads: the decoders whose documents its
// rules can be evaluated against, and how an error names such a response.
type transformInput struct {
	decoders []string
	response string
}

// transformInputs is the one table of which transform reads which decoder's
// document. A scrape checks the document it was handed against it
// (validateTransformInput), and a configuration that names a decoder its
// transform is not listed with is refused when it loads (CheckDecoder), so
// the two never disagree. The python transform is not listed: its script is
// handed whatever was decoded.
var transformInputs = map[string]transformInput{
	"jq":         {[]string{"json", "yaml", "graphite"}, "a structured JSON/YAML"},
	"yq":         {[]string{"json", "yaml", "graphite"}, "a structured JSON/YAML"},
	"regex":      {[]string{"text"}, "a text"},
	"csv":        {[]string{"csv"}, "a CSV"},
	"css":        {[]string{"html"}, "an HTML"},
	"xpath":      {[]string{"xml", "html"}, "an XML or HTML"},
	"prometheus": {[]string{"prometheus"}, "a Prometheus"},
}

// ReadableDecoders is the decoders whose documents a transform of the given
// type reads, and nil for one that reads them all, as python does.
func ReadableDecoders(transformType string) []string {
	return slices.Clone(transformInputs[transformType].decoders)
}

// validateTransformInput refuses, on a scrape, a decoded document the
// transform cannot read.
func validateTransformInput(d *decode.Decoded, transformType string) error {
	input, listed := transformInputs[transformType]
	if !listed || slices.Contains(input.decoders, d.Kind) {
		return nil
	}
	if jqFamily(transformType) {
		return fmt.Errorf("transform %q cannot map response format %q; use a structured JSON/YAML response or a compatible transform", transformType, d.Kind)
	}
	return fmt.Errorf("%s transform requires %s response, got %q", transformType, input.response, d.Kind)
}

// CheckDecoder refuses, when a configuration loads, a decoder.type the
// collector's transform can never read: every scrape would fail whatever the
// target answered. A decoder left to each response (auto) is checked on the
// scrape instead. A jq or yq transform with a pre_script is let be: its rules
// read the mapping or list the script leaves, whatever was decoded
// (applyPreScript); for every other transform the script's result is read as
// the decoder's own kind of document.
func CheckDecoder(x *model.Collector) error {
	input, listed := transformInputs[x.Transform.Type]
	if !listed || x.Decoder.Type == "" || x.Decoder.Type == "auto" || slices.Contains(input.decoders, x.Decoder.Type) {
		return nil
	}
	if jqFamily(x.Transform.Type) && strings.TrimSpace(x.Transform.PreScript) != "" {
		return nil
	}
	advice := "set decoder.type to " + input.decoders[0]
	if len(input.decoders) > 1 {
		advice = "set decoder.type to one of them"
	}
	if jqFamily(x.Transform.Type) {
		advice += ", give the transform a pre_script that leaves its rules a mapping or a list"
	}
	return fmt.Errorf("collector %q decodes with %s, which its %s transform cannot read: it reads %s; %s, or use a transform that reads %s: %s", x.Name, x.Decoder.Type, x.Transform.Type, englishList(input.decoders), advice, x.Decoder.Type, englishList(transformsReading(x.Decoder.Type)))
}

// transformsReading is the transforms that read a decoder's document, in the
// order of model.TransformTypes.
func transformsReading(decoder string) []string {
	var out []string
	for _, transformType := range model.TransformTypes {
		if input, listed := transformInputs[transformType]; !listed || slices.Contains(input.decoders, decoder) {
			out = append(out, transformType)
		}
	}
	return out
}

// englishList writes names as "a", "a or b" and "a, b or c".
func englishList(names []string) string {
	if len(names) < 2 {
		return strings.Join(names, "")
	}
	return strings.Join(names[:len(names)-1], ", ") + " or " + names[len(names)-1]
}
