package transform

import (
	"context"
	"strings"
	"testing"

	"github.com/antchfx/xmlquery"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A label that adds up nodes by an absolute path reads the document's nodes,
// as the engine does for such a label, and not those beneath the node its
// rule selected: the sum the exporter adds up is the document's, and text
// that is no number anywhere in the document fails the series, where the
// nodes beneath the selected one, of which there are none, would have given
// 0 and no failure.
func TestASumInALabelWithAnAbsolutePathAddsUpTheDocumentsNodes(t *testing.T) {
	rule := model.MetricRule{Name: "job_up", Type: "gauge", Expression: "//job/up", Labels: []model.LabelRule{
		{Name: "total", Expression: "sum(//size)"},
		{Name: "share", Expression: "sum(//size) div 2"},
	}}
	collector := &model.Collector{Name: "jobs", Metrics: []model.MetricRule{rule}}
	read := func(t *testing.T, document string) (*model.MetricSet, error) {
		t.Helper()
		root, err := xmlquery.Parse(strings.NewReader(document))
		if err != nil {
			t.Fatal(err)
		}
		return transformXPath(context.Background(), root, collector.Metrics, collector, nil)
	}

	set, err := read(t, `<jobs><size> 2 </size><size>4</size><job><up>1</up></job></jobs>`)
	if err == nil {
		t.Fatalf("a sum the engine computes over a number with blanks around it gave %v, want a failure", set.Metrics)
	}
	if !strings.Contains(err.Error(), `label "share"`) || !strings.Contains(err.Error(), `" 2 "`) {
		t.Fatalf("the failure is %q, want it to name the label share and the text \" 2 \"", err)
	}

	set, err = read(t, `<jobs><size>2</size><size>4</size><job><up>1</up></job></jobs>`)
	if err != nil {
		t.Fatal(err)
	}
	if len(set.Metrics) != 1 {
		t.Fatalf("got %d series, want 1", len(set.Metrics))
	}
	if got := set.Metrics[0].Labels; got["total"] != "6" || got["share"] != "3" {
		t.Fatalf("labels are %v, want total 6 and share 3", got)
	}
}
