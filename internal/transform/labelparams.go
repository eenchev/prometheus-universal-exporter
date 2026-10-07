package transform

import (
	"context"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A collector's fixed label values may hold {{param_<name>}} placeholders,
// which the parameters of the probe fill (fetch/labelparams.go). The
// transform is where a label value is read, so it is where they are filled:
// Transform reads a collector whose labels hold placeholders as a copy with
// the values filled in, and every transform, the label value maps, the
// truncation and the collector-wide label settings then read a filled value
// as they read one written in the configuration.

type labelParamsKey struct{}

// WithLabelParams returns a context carrying the param_<name> parameters of
// a probe, or of a static target, for the Transform of a collector whose
// label values hold placeholders. A caller need not make one for a
// collector whose LabelParams is nil, which reads no parameter.
func WithLabelParams(ctx context.Context, params map[string]string) context.Context {
	return context.WithValue(ctx, labelParamsKey{}, params)
}

// withFilledLabels is the collector Transform reads: c itself unless its
// label values hold placeholders, and then a copy of it with them filled in
// from the context's parameters (fetch.FilledLabels). A context that
// carries none fills each placeholder with its default.
func withFilledLabels(ctx context.Context, c *model.Collector) (*model.Collector, error) {
	if c.LabelParams == nil {
		return c, nil
	}
	params, _ := ctx.Value(labelParamsKey{}).(map[string]string)
	return fetch.FilledLabels(c, params)
}

// ruleLabelHoldsPlaceholders says whether value, the value of a static
// label of one of c's rules, holds placeholders a probe's parameters fill:
// whether it is the text of one of the collector's label values that were
// read to hold some when the configuration loaded (fetch.ParseLabelParams).
// A text is read the same in whichever rule it stands, so the rule is not
// asked for. A collector whose label values were never read has none.
func ruleLabelHoldsPlaceholders(c *model.Collector, value string) bool {
	if c.LabelParams == nil {
		return false
	}
	for i := range c.LabelParams.Rules {
		if c.LabelParams.Rules[i].Text == value {
			return true
		}
	}
	return false
}
