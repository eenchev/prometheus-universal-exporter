package transform

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/decode"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
)

// Truncation after the labels' value maps, for every kind of prometheus rule
// (labelCuts), against the transform as it was (copyingTransform, which cut
// a prometheus rule's labels as it made the series, before the maps).

// expectedTenant is the tenant a series is exported with when its value u
// is mapped by values, where a map applies, and then cut to limit, where it
// is truncated: what the value maps and truncate: true mean, in that order.
func expectedTenant(u string, values map[string]string, mapped, truncated bool, limit int) (string, bool) {
	if mapped {
		if to, ok := values[strings.TrimSpace(u)]; ok {
			u = to
		} else if to, ok := values["*"]; ok {
			u = to
		}
	}
	if truncated && limit > 0 {
		u = truncateLabelValue(u, limit)
	}
	return u, u != ""
}

// Over a generated table of prometheus collectors — a rule reading foo,
// without a name, named foo or named bar, its label tenant a constant short
// or long or read from the series, with truncate: true or not; a rule named
// foo or bar reading other, mapping tenant by a value_map or not, with
// truncate: true or not; a limit or none — and source values short and long:
// every series is exported with its tenant mapped and then cut where it is
// truncated, and with everything else as the transform as it was gave it;
// and where no truncated label of a series is mapped, which is every
// collector without a value map or without truncate: true, the transform
// gives exactly what it gave.
func TestALabelIsCutAfterItsValueMapForEveryKindOfRule(t *testing.T) {
	valueMaps := []map[string]string{nil, {"*": "overlongvalue"}, {"verylongtenantvalue": "ok", "acme": "fine"}, {"t": "short"}, {"acme": ""}}
	compared, changed := 0, 0
	for _, aName := range []string{"", "foo", "bar"} {
		for _, aValue := range []string{"", "acme", "verylongtenantvalue"} {
			for _, aTruncate := range []bool{false, true} {
				for _, bName := range []string{"foo", "bar"} {
					for _, values := range valueMaps {
						for _, bTruncate := range []bool{false, true} {
							for _, limit := range []int{0, 8, 30} {
								for _, source := range []string{"acme", "verylongtenantvalue"} {
									a := model.LabelRule{Name: "tenant", Value: aValue, Truncate: aTruncate}
									if aValue == "" {
										a = model.LabelRule{Name: "tenant", Expression: "tenant", Truncate: aTruncate}
									}
									c := model.Collector{
										Name:      "prom",
										Decoder:   model.DecoderConfig{Type: "prometheus"},
										Transform: model.TransformConfig{Type: "prometheus"},
										Limits:    model.Limits{MaxLabelValueLength: limit},
										Metrics: []model.MetricRule{
											{Name: aName, Expression: "^foo$", Labels: []model.LabelRule{a, {Name: "note", Expression: "note"}}},
											{Name: bName, Expression: "^other$", Labels: []model.LabelRule{{Name: "tenant", Expression: "tenant", ValueMap: values, Truncate: bTruncate}}},
										},
									}
									body := fmt.Sprintf("foo{tenant=%q,note=\"verylongnotevalue\"} 1\nother{tenant=%q} 2\n", source, source)
									where := fmt.Sprintf("rule a %q value %q truncate %v, rule b %q map %v truncate %v, limit %d, source %q", aName, aValue, aTruncate, bName, values, bTruncate, limit, source)
									r := &fetch.HTTPResponse{Body: []byte(body), Headers: http.Header{}}
									d, err := decode.Decode(r, &c)
									if err != nil {
										t.Fatal(err)
									}
									want, _, wantErr := copyingTransform(&decode.Decoded{Kind: d.Kind, Data: copiedSeries(d.Data.(model.MetricSet))}, &c)
									got, err := Transform(context.Background(), d, r, &c, "")
									if err != nil || wantErr != nil || len(got.Metrics) != 2 || len(want.Metrics) != 2 {
										t.Fatalf("%s: %v (as it was %v), %d series", where, err, wantErr, len(got.Metrics))
									}
									compared++
									aSeries := aName
									if aSeries == "" {
										aSeries = "foo"
									}
									mappedA := len(values) > 0 && bName == aSeries
									truncatedA := aTruncate || bTruncate && bName == aSeries
									mappedB := len(values) > 0
									truncatedB := bTruncate || aTruncate && aName == bName
									uA := aValue
									if uA == "" {
										uA = source
									}
									affected := limit > 0 && (mappedA && truncatedA || mappedB && truncatedB)
									for i, expect := range []struct {
										u                 string
										mapped, truncated bool
									}{{uA, mappedA, truncatedA}, {source, mappedB, truncatedB}} {
										tenant, present := expectedTenant(expect.u, values, expect.mapped, expect.truncated, limit)
										if value, ok := got.Metrics[i].Labels["tenant"]; ok != present || value != tenant {
											t.Errorf("%s: series %d has tenant %q (%v), want %q (%v)", where, i, value, ok, tenant, present)
										}
										gotRest, wantRest := got.Metrics[i], want.Metrics[i]
										gotRest.Labels, wantRest.Labels = maps.Clone(gotRest.Labels), maps.Clone(wantRest.Labels)
										delete(gotRest.Labels, "tenant")
										delete(wantRest.Labels, "tenant")
										if !reflect.DeepEqual(gotRest, wantRest) {
											t.Errorf("%s: series %d is\n%+v\nbeside its tenant, and was\n%+v", where, i, gotRest, wantRest)
										}
									}
									if !affected && !reflect.DeepEqual(got, want) {
										t.Errorf("%s: no truncated label mapped, yet\n%+v\nwas\n%+v", where, got, want)
									}
									if !reflect.DeepEqual(got, want) {
										changed++
									}
								}
							}
						}
					}
				}
			}
		}
	}
	if compared != 3*3*2*2*len(valueMaps)*2*3*2 || changed == 0 {
		t.Fatalf("%d collectors compared, %d exported otherwise", compared, changed)
	}
}

// labelCutsFor notes nothing, and costs no allocation, for a collector
// without truncate: true, without a limit, or without a value map, and for
// every transform but prometheus: the transform of such a collector is the
// one it was.
func TestOnlyAPrometheusCollectorWithACutAndAMapNotesCuts(t *testing.T) {
	rule := func(truncate bool, values map[string]string) []model.MetricRule {
		return []model.MetricRule{
			{Expression: "^foo$", Labels: []model.LabelRule{{Name: "tenant", Value: "acme", Truncate: truncate}}},
			{Name: "foo", Expression: "^other$", Labels: []model.LabelRule{{Name: "tenant", Expression: "tenant", ValueMap: values}}},
		}
	}
	for _, kind := range []string{"prometheus", "jq"} {
		for _, truncate := range []bool{false, true} {
			for _, values := range []map[string]string{nil, {"*": "x"}} {
				for _, limit := range []int{0, 8} {
					c := &model.Collector{Transform: model.TransformConfig{Type: kind}, Limits: model.Limits{MaxLabelValueLength: limit}, Metrics: rule(truncate, values)}
					notes := labelCutsFor(c) != nil
					if want := kind == "prometheus" && truncate && values != nil && limit > 0; notes != want {
						t.Errorf("%s, truncate %v, map %v, limit %d: notes cuts %v", kind, truncate, values, limit, notes)
					}
					if !notes {
						if n := alloctest.AllocsAtMost(10, 0, func() { _, _ = withLabelCuts(context.Background(), c) }); n != 0 {
							t.Errorf("%s, truncate %v, map %v, limit %d: %v allocations", kind, truncate, values, limit, n)
						}
					}
				}
			}
		}
	}
}
