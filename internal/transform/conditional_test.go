package transform

import (
	"net/http"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/decode"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The conditional metrics and labels CONFIGURATION.md shows, one per
// transform, give exactly the series it says: the condition lives in each
// transform's own language, so no when key is needed.
func TestConditionalMetricsAsDocumented(t *testing.T) {
	notRequired := false
	for name, tc := range map[string]struct {
		decoder, transform, contentType, body, preScript string
		status                                           int
		rules                                            []model.MetricRule
		want                                             []string
	}{
		"jq items that pass select": {
			"json", "jq", "application/json", `{"queues": [{"name": "a", "state": "active", "depth": 5}, {"name": "b", "state": "paused", "depth": 9}]}`, "", 200,
			[]model.MetricRule{{Name: "queue_depth", Items: `.queues[] | select(.state == "active")`, Expression: ".depth", Labels: []model.LabelRule{{Name: "queue", Expression: ".name"}}}},
			[]string{"queue_depth{queue=a} 5"},
		},
		"jq on the status, answered": {
			"json", "jq", "application/json", `{"depth": 5}`, "", 200,
			[]model.MetricRule{{Name: "queue_depth", Expression: `if $status == 200 then .depth else empty end`, Required: &notRequired}},
			[]string{"queue_depth{} 5"},
		},
		"jq on the status, not answered": {
			"json", "jq", "application/json", `{"depth": 5}`, "", 503,
			[]model.MetricRule{{Name: "queue_depth", Expression: `if $status == 200 then .depth else empty end`, Required: &notRequired}},
			nil,
		},
		"jq label only when it applies": {
			"json", "jq", "application/json", `{"queues": [{"name": "a", "shared": true, "owner": "x"}, {"name": "b", "shared": false, "owner": "y"}]}`, "", 200,
			[]model.MetricRule{{Name: "queue_info", Items: ".queues[]", Expression: "1", Labels: []model.LabelRule{{Name: "queue", Expression: ".name"}, {Name: "owner", Expression: "if .shared then .owner else null end"}}}},
			[]string{"queue_info{owner=x,queue=a} 1", "queue_info{queue=b} 1"},
		},
		"jq 0 or 1 instead": {
			"json", "jq", "application/json", `{"queues": [{"name": "a", "state": "active"}, {"name": "b", "state": "paused"}]}`, "", 200,
			[]model.MetricRule{{Name: "queue_active", Items: ".queues[]", Expression: `if .state == "active" then 1 else 0 end`, Labels: []model.LabelRule{{Name: "queue", Expression: ".name"}}}},
			[]string{"queue_active{queue=a} 1", "queue_active{queue=b} 0"},
		},
		"xpath predicate": {
			"xml", "xpath", "application/xml", `<queues><queue name="a" state="active"><depth>5</depth></queue><queue name="b" state="paused"><depth>9</depth></queue></queues>`, "", 200,
			[]model.MetricRule{{Name: "queue_depth", Expression: "//queue[@state='active']/depth", Labels: []model.LabelRule{{Name: "queue", Expression: "../@name"}}}},
			[]string{"queue_depth{queue=a} 5"},
		},
		"css :has": {
			"html", "css", "text/html", `<table><tr><td class="name">a</td><td class="state active">up</td><td class="depth">5</td></tr><tr><td class="name">b</td><td class="state">down</td><td class="depth">9</td></tr></table>`, "", 200,
			[]model.MetricRule{{Name: "queue_depth", Items: "tr:has(td.state.active)", Expression: "td.depth", Labels: []model.LabelRule{{Name: "queue", Expression: "td.name"}}}},
			[]string{"queue_depth{queue=a} 5"},
		},
		"regex pattern": {
			"text", "regex", "text/plain", "5 web up\n9 db down\n", "", 200,
			[]model.MetricRule{{Name: "service_connections", Expression: `(?m)^(\d+) (\w+) up$`, Labels: []model.LabelRule{{Name: "service", Expression: "2"}}}},
			[]string{"service_connections{service=web} 5"},
		},
		"csv rows a pre-script keeps": {
			"csv", "csv", "text/csv", "name,state,depth\na,active,5\nb,paused,9\n", `data = [row for row in data if row["state"] == "active"]`, 200,
			[]model.MetricRule{{Name: "queue_depth", Expression: "depth", Labels: []model.LabelRule{{Name: "queue", Expression: "name"}}}},
			[]string{"queue_depth{queue=a} 5"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			if tc.preScript != "" {
				requirePython(t)
			}
			for i := range tc.rules {
				tc.rules[i].Type = model.GaugeMetricType
			}
			c := model.Collector{Name: "c", Decoder: model.DecoderConfig{Type: tc.decoder}, Transform: model.TransformConfig{Type: tc.transform, PreScript: tc.preScript}, Metrics: tc.rules,
				Limits: model.Limits{ScriptTimeout: model.Duration(2 * time.Second), MaxOutputBytes: 1 << 20}}
			for i := range c.Metrics {
				if err := CheckMetricRule(&c, &c.Metrics[i]); err != nil {
					t.Fatal(err)
				}
			}
			r := &fetch.HTTPResponse{StatusCode: tc.status, Body: []byte(tc.body), Headers: http.Header{"Content-Type": {tc.contentType}}}
			d, err := decode.Decode(r, &c)
			if err != nil {
				t.Fatal(err)
			}
			set, err := Transform(t.Context(), d, r, &c, "python3")
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, m := range set.Metrics {
				var labels []string
				for k, v := range m.Labels {
					labels = append(labels, k+"="+v)
				}
				sort.Strings(labels)
				got = append(got, m.Name+"{"+strings.Join(labels, ",")+"} "+strconv.FormatFloat(m.Value, 'g', -1, 64))
			}
			sort.Strings(got)
			if strings.Join(got, "\n") != strings.Join(tc.want, "\n") {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}
