package transform

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/decode"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// tripResponse is an answer to a request over plain HTTP on a new
// connection to an IP address: a connection, no lookup, no handshake.
func tripResponse() *fetch.HTTPResponse {
	connect, firstByte := 2*time.Millisecond, 30*time.Millisecond
	return &fetch.HTTPResponse{
		StatusCode: http.StatusOK,
		Headers:    http.Header{"Content-Type": []string{"application/json"}},
		Body:       []byte(`{"items": [{"name": "a"}, {"name": "b"}]}`),
		Trip:       &fetch.HTTPTrip{Connect: &connect, FirstByte: &firstByte, Total: 50 * time.Millisecond},
	}
}

// tripSeries runs the collector's rules on r and gives its series by name
// and labels, in the exposition's words.
func tripSeries(t *testing.T, c model.Collector, r *fetch.HTTPResponse) string {
	t.Helper()
	c.Request.Type, c.Limits.MaxMetrics = fetch.RequestTypeHTTP, 100
	if c.Decoder.Type == "" {
		c.Decoder.Type = "json"
	}
	d, err := decode.Decode(r, &c)
	if err != nil {
		t.Fatal(err)
	}
	set, err := Transform(context.Background(), d, r, &c, "python3")
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, m := range set.Metrics {
		b.WriteString(m.Name)
		for _, name := range model.SortedKeys(m.Labels) {
			b.WriteString(" " + name + "=" + m.Labels[name])
		}
		b.WriteString(" " + strconv.FormatFloat(m.Value, 'g', -1, 64) + "\n")
	}
	return b.String()
}

// $timings is bound in every jq and yq program, the expression of a rule,
// its items and a label, each per item too, and is Python's
// response.timings: seconds, null for a phase the request did not have, and
// null whole for an answer no HTTP request brought.
func TestTimingsAreBoundInEveryProgram(t *testing.T) {
	rules := []model.MetricRule{
		{Name: "total", Expression: "$timings.total"},
		{Name: "first_byte", Expression: "$timings.first_byte"},
		{Name: "connect", Expression: "$timings.connect"},
		{Name: "no_lookup", Expression: "if $timings.dns == null and $timings.tls == null then 1 else 0 end"},
		{Name: "per_item", Items: ".items[] | select($timings.total > 0)", Expression: "$timings.total * 1000", Labels: []model.LabelRule{{Name: "name", Expression: ".name"}, {Name: "phases", Expression: "$timings | keys | join(\",\")"}}},
		{Name: "absent", Expression: "if $timings == null then 1 else 0 end"},
	}
	want := "total 0.05\nfirst_byte 0.03\nconnect 0.002\nno_lookup 1\n" +
		"per_item name=a phases=connect,dns,first_byte,tls,total 50\nper_item name=b phases=connect,dns,first_byte,tls,total 50\nabsent 0\n"
	for _, kind := range []string{"jq", "yq"} {
		c := model.Collector{Name: "trip", Transform: model.TransformConfig{Type: kind}, Metrics: rules}
		if got := tripSeries(t, c, tripResponse()); got != want {
			t.Errorf("%s:\n%s\nwant\n%s", kind, got, want)
		}
		c.Metrics = rules[len(rules)-1:]
		bare := tripResponse()
		bare.Trip = nil
		if got := tripSeries(t, c, bare); got != "absent 1\n" {
			t.Errorf("%s without a trip: %s", kind, got)
		}
	}

	requirePython(t)
	c := model.Collector{Name: "trip", Transform: model.TransformConfig{Type: "python", Script: `t = response.timings
metric(name='total', value=t['total'])
metric(name='nulls', value=sum(1 for k in ('dns', 'tls') if t[k] is None))
metric(name='phases', value=len(t))
`}}
	if got := tripSeries(t, c, tripResponse()); got != "total 0.05\nnulls 2\nphases 5\n" {
		t.Errorf("python: %s", got)
	}
	c.Transform.Script = "metric(name='absent', value=1 if response.timings is None else 0)\n"
	bare := tripResponse()
	bare.Trip = nil
	if got := tripSeries(t, c, bare); got != "absent 1\n" {
		t.Errorf("python without a trip: %s", got)
	}
}
