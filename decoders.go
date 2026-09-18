package main

import (
	"bytes"
	"encoding/csv"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"encoding/json"
	"github.com/PuerkitoBio/goquery"
	"github.com/antchfx/xmlquery"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
	"gopkg.in/yaml.v3"
)

type Decoded struct {
	Kind string
	Data any
	Raw  []byte
}
type HTMLDecoded struct {
	Document *goquery.Document
	Raw      []byte
}

func detectFormat(r *HTTPResponse, requested string) string {
	if requested != "" && requested != "auto" {
		return requested
	}
	rawCT := strings.ToLower(r.Headers.Get("Content-Type"))
	ct := strings.TrimSpace(strings.Split(rawCT, ";")[0])
	switch {
	case ct == "application/json" || strings.HasSuffix(ct, "+json"):
		return "json"
	case ct == "application/yaml" || ct == "text/yaml" || ct == "application/x-yaml":
		return "yaml"
	case ct == "application/xml" || ct == "text/xml":
		return "xml"
	case ct == "text/csv":
		return "csv"
	case ct == "text/html":
		return "html"
	case ct == "application/openmetrics-text" || ct == "text/plain" && strings.Contains(rawCT, "version=0.0.4"):
		return "prometheus"
	}
	b := bytes.TrimSpace(r.Body)
	if bytes.Contains(b, []byte("# TYPE ")) || bytes.Contains(b, []byte("# HELP ")) {
		return "prometheus"
	}
	if len(b) > 0 && (b[0] == '{' || b[0] == '[') {
		var x any
		if json.Unmarshal(b, &x) == nil {
			return "json"
		}
	}
	if bytes.HasPrefix(b, []byte("<?xml")) || bytes.HasPrefix(b, []byte("<")) {
		return "xml"
	}
	return "text"
}

func decode(r *HTTPResponse, c *Collector) (*Decoded, error) {
	kind := detectFormat(r, c.Response.Format)
	if c.Decoder.Type != "" && c.Decoder.Type != "auto" {
		kind = c.Decoder.Type
	}
	switch kind {
	case "json":
		var v any
		d := json.NewDecoder(bytes.NewReader(r.Body))
		d.UseNumber()
		if err := d.Decode(&v); err != nil {
			return nil, fmt.Errorf("JSON decode: %w", err)
		}
		return &Decoded{Kind: kind, Data: normalize(v), Raw: r.Body}, nil
	case "yaml":
		var v any
		if err := yaml.Unmarshal(r.Body, &v); err != nil {
			return nil, fmt.Errorf("YAML decode: %w", err)
		}
		return &Decoded{Kind: kind, Data: normalize(v), Raw: r.Body}, nil
	case "csv":
		return decodeCSV(r, c)
	case "xml":
		n, err := xmlquery.Parse(bytes.NewReader(r.Body))
		if err != nil {
			return nil, fmt.Errorf("XML decode: %w", err)
		}
		return &Decoded{Kind: kind, Data: n, Raw: r.Body}, nil
	case "html":
		d, err := goquery.NewDocumentFromReader(bytes.NewReader(r.Body))
		if err != nil {
			return nil, fmt.Errorf("HTML decode: %w", err)
		}
		return &Decoded{Kind: kind, Data: &HTMLDecoded{Document: d, Raw: r.Body}, Raw: r.Body}, nil
	case "prometheus":
		return decodePrometheus(r)
	case "text":
		return &Decoded{Kind: kind, Data: string(r.Body), Raw: r.Body}, nil
	default:
		return nil, fmt.Errorf("unsupported decoder %q", kind)
	}
}

func decodeCSV(r *HTTPResponse, c *Collector) (*Decoded, error) {
	cfg := c.Response.CSV
	delim := ','
	if cfg.Delimiter != "" {
		rr := []rune(cfg.Delimiter)
		if len(rr) != 1 {
			return nil, errors.New("CSV delimiter must be one character")
		}
		delim = rr[0]
	}
	cr := csv.NewReader(bytes.NewReader(r.Body))
	cr.Comma = delim
	cr.FieldsPerRecord = -1
	cr.TrimLeadingSpace = cfg.TrimSpace
	rows, err := cr.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("CSV decode: %w", err)
	}
	if len(rows) == 0 {
		return &Decoded{Kind: "csv", Data: []any{}, Raw: r.Body}, nil
	}
	header := true
	if cfg.Header != nil {
		header = *cfg.Header
	}
	out := []any{}
	if header {
		heads := rows[0]
		for i := range heads {
			if cfg.TrimSpace {
				heads[i] = strings.TrimSpace(heads[i])
			}
		}
		for _, row := range rows[1:] {
			m := map[string]any{}
			for i, k := range heads {
				if i < len(row) {
					v := row[i]
					if cfg.TrimSpace {
						v = strings.TrimSpace(v)
					}
					m[k] = v
				} else {
					m[k] = ""
				}
			}
			out = append(out, m)
		}
	} else {
		for _, row := range rows {
			a := make([]any, len(row))
			for i, v := range row {
				a[i] = v
			}
			out = append(out, a)
		}
	}
	return &Decoded{Kind: "csv", Data: out, Raw: r.Body}, nil
}

func decodePrometheus(r *HTTPResponse) (*Decoded, error) {
	parser := expfmt.NewTextParser(model.UTF8Validation)
	families, err := parser.TextToMetricFamilies(bytes.NewReader(r.Body))
	if err != nil {
		return nil, fmt.Errorf("decoding Prometheus exposition: %w", err)
	}
	set := MetricSet{}
	for name, mf := range families {
		_ = name
		typ := metricType(mf.GetType())
		for _, x := range mf.Metric {
			labels := map[string]string{}
			for _, l := range x.Label {
				labels[l.GetName()] = l.GetValue()
			}
			m := Metric{Name: mf.GetName(), Help: mf.GetHelp(), Type: typ, Labels: labels}
			if x.TimestampMs != nil {
				t := x.GetTimestampMs()
				m.Timestamp = &t
			}
			switch mf.GetType() {
			case dto.MetricType_GAUGE:
				m.Value = x.GetGauge().GetValue()
			case dto.MetricType_COUNTER:
				m.Value = x.GetCounter().GetValue()
			case dto.MetricType_UNTYPED:
				m.Value = x.GetUntyped().GetValue()
			case dto.MetricType_HISTOGRAM:
				h := x.GetHistogram()
				z := &Histogram{Sum: h.GetSampleSum(), Count: h.GetSampleCount()}
				for _, b := range h.Bucket {
					z.Buckets = append(z.Buckets, Bucket{UpperBound: b.GetUpperBound(), CumulativeCount: b.GetCumulativeCount()})
				}
				m.Histogram = z
			case dto.MetricType_SUMMARY:
				s := x.GetSummary()
				z := &Summary{Sum: s.GetSampleSum(), Count: s.GetSampleCount()}
				for _, q := range s.Quantile {
					z.Quantiles = append(z.Quantiles, Quantile{Quantile: q.GetQuantile(), Value: q.GetValue()})
				}
				m.Summary = z
			}
			set.Metrics = append(set.Metrics, m)
		}
	}
	return &Decoded{Kind: "prometheus", Data: set, Raw: r.Body}, nil
}
func metricType(t dto.MetricType) MetricType {
	switch t {
	case dto.MetricType_COUNTER:
		return CounterMetricType
	case dto.MetricType_HISTOGRAM:
		return HistogramMetricType
	case dto.MetricType_SUMMARY:
		return SummaryMetricType
	case dto.MetricType_UNTYPED:
		return UntypedMetricType
	default:
		return GaugeMetricType
	}
}

func textValue(v string) (float64, error) { return strconv.ParseFloat(strings.TrimSpace(v), 64) }
