package transform

import (
	"context"
	"net/http"
	"sync"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/decode"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// One compiled XPath expression serves every scrape of every collector that
// has it, and scrapes run at once. antchfx/xpath runs an expression's query
// tree in place, so evaluating one shared *xpath.Expr from two goroutines is a
// data race; under -race this test failed while the cache handed every scrape
// the same compiled expression.
func TestXPathExpressionSharedAcrossScrapes(t *testing.T) {
	c := model.Collector{Name: "jobs", Decoder: model.DecoderConfig{Type: "xml"}, Transform: model.TransformConfig{Type: "xpath"},
		Metrics: []model.MetricRule{
			{Name: "jobs", Type: model.GaugeMetricType, Expression: "count(//job)", Labels: []model.LabelRule{{Name: "first", Expression: "string(//job[1]/@name)"}}},
			{Name: "job_size", Type: model.GaugeMetricType, Expression: "//job[size > 0]/size", Labels: []model.LabelRule{{Name: "name", Expression: "normalize-space(../@name)"}, {Name: "state", Expression: "../state"}}},
		}}
	body := []byte(`<jobs><job name="a"><size>1</size><state>ok</state></job><job name="b"><size>2</size><state>ok</state></job><job name="c"><size>3</size><state>late</state></job></jobs>`)
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for range 16 {
		wg.Go(func() {
			for range 50 {
				r := &fetch.HTTPResponse{Body: body, Headers: http.Header{}}
				d, err := decode.Decode(r, &c)
				if err != nil {
					errs <- err
					return
				}
				set, err := Transform(context.Background(), d, r, &c, "")
				if err != nil {
					errs <- err
					return
				}
				if len(set.Metrics) != 4 || set.Metrics[0].Value != 3 || set.Metrics[0].Labels["first"] != "a" || set.Metrics[3].Labels["state"] != "late" {
					t.Errorf("unexpected series: %#v", set.Metrics)
					return
				}
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

// An XPath label is trimmed, as a css label is: in pretty-printed XML the
// text of an element carries the indentation around it.
func TestXPathLabelTextIsTrimmed(t *testing.T) {
	c := model.Collector{Name: "jobs", Decoder: model.DecoderConfig{Type: "xml"}, Transform: model.TransformConfig{Type: "xpath"},
		Metrics: []model.MetricRule{{Name: "job_size", Type: model.GaugeMetricType, Expression: "//job/size", Labels: []model.LabelRule{
			{Name: "owner", Expression: "../owner"},
			{Name: "state", Expression: "string(../state)"},
		}}}}
	body := []byte(`<jobs>
  <job>
    <size>
      7
    </size>
    <owner>
      team-a
    </owner>
    <state>
      running
    </state>
  </job>
</jobs>
`)
	r := &fetch.HTTPResponse{Body: body, Headers: http.Header{}}
	d, err := decode.Decode(r, &c)
	if err != nil {
		t.Fatal(err)
	}
	set, err := Transform(context.Background(), d, r, &c, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(set.Metrics) != 1 || set.Metrics[0].Value != 7 || set.Metrics[0].Labels["owner"] != "team-a" || set.Metrics[0].Labels["state"] != "running" {
		t.Fatalf("unexpected series: %#v", set.Metrics)
	}
}
