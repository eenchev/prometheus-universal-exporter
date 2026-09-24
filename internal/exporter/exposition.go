package exporter

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// probeError is the body of a probe that failed because a metric rule with
// error_mode fail could not produce its value. Prometheus only looks at the
// status code, which already fails the scrape; the body is for the person who
// runs the probe by hand to find out why, so it names the collector, the rule
// and the error rather than leaving them to be dug out of the exporter's log.
type probeError struct {
	Status    string `json:"status"`
	Stage     string `json:"stage"`
	Collector string `json:"collector"`
	Metric    string `json:"metric,omitempty"`
	Target    string `json:"target,omitempty"`
	Error     string `json:"error"`
}

// writeProbeError writes a probe failure as a JSON object. Status is always
// "error", so a client can test one field without knowing the others.
func writeProbeError(w http.ResponseWriter, code int, body probeError) {
	body.Status = "error"
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

func writeMetricSet(w http.ResponseWriter, s *model.MetricSet) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	var b strings.Builder
	renderMetricSet(&b, s)
	_, _ = w.Write([]byte(b.String()))
}

// renderMetricSet writes the exposition text for a metric set, so the verbose
// self-metrics are rendered by exactly the same code as collector output.
func renderMetricSet(b *strings.Builder, s *model.MetricSet) {
	help := map[string]bool{}
	for _, m := range s.Metrics {
		if !help[m.Name] {
			if m.Help != "" {
				fmt.Fprintf(b, "# HELP %s %s\n", m.Name, strings.ReplaceAll(strings.ReplaceAll(m.Help, "\\", "\\\\"), "\n", "\\n"))
			}
			fmt.Fprintf(b, "# TYPE %s %s\n", m.Name, m.Type)
			help[m.Name] = true
		}
		if m.Histogram != nil {
			writeHistogram(b, m)
			continue
		}
		if m.Summary != nil {
			writeSummary(b, m)
			continue
		}
		fmt.Fprintf(b, "%s%s %s", m.Name, formatLabels(m.Labels), strconv.FormatFloat(m.Value, 'g', -1, 64))
		if m.Timestamp != nil {
			fmt.Fprintf(b, " %d", *m.Timestamp)
		}
		b.WriteByte('\n')
	}
}

func formatLabels(ls map[string]string) string {
	if len(ls) == 0 {
		return ""
	}
	keys := make([]string, 0, len(ls))
	for k := range ls {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%s=\"%s\"", k, promQuote(ls[k]))
	}
	b.WriteByte('}')
	return b.String()
}

func promQuote(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(s, "\\", "\\\\"), "\n", "\\n"), "\"", "\\\"")
}

func writeHistogram(b *strings.Builder, m model.Metric) {
	for _, x := range m.Histogram.Buckets {
		// The +Inf bucket is written below from the count. A histogram decoded
		// from a Prometheus source carries its own +Inf bucket, which written
		// here as well would be a duplicate series.
		if math.IsInf(x.UpperBound, 1) {
			continue
		}
		ls := model.CloneLabels(m.Labels)
		ls["le"] = strconv.FormatFloat(x.UpperBound, 'g', -1, 64)
		fmt.Fprintf(b, "%s_bucket%s %d\n", m.Name, formatLabels(ls), x.CumulativeCount)
	}
	ls := model.CloneLabels(m.Labels)
	ls["le"] = "+Inf"
	fmt.Fprintf(b, "%s_bucket%s %d\n%s_sum%s %s\n%s_count%s %d\n", m.Name, formatLabels(ls), m.Histogram.Count, m.Name, formatLabels(m.Labels), strconv.FormatFloat(m.Histogram.Sum, 'g', -1, 64), m.Name, formatLabels(m.Labels), m.Histogram.Count)
}

func writeSummary(b *strings.Builder, m model.Metric) {
	for _, x := range m.Summary.Quantiles {
		ls := model.CloneLabels(m.Labels)
		ls["quantile"] = strconv.FormatFloat(x.Quantile, 'g', -1, 64)
		fmt.Fprintf(b, "%s%s %s\n", m.Name, formatLabels(ls), strconv.FormatFloat(x.Value, 'g', -1, 64))
	}
	fmt.Fprintf(b, "%s_sum%s %s\n%s_count%s %d\n", m.Name, formatLabels(m.Labels), strconv.FormatFloat(m.Summary.Sum, 'g', -1, 64), m.Name, formatLabels(m.Labels), m.Summary.Count)
}
