package decode

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/PuerkitoBio/goquery"
	"github.com/antchfx/xmlquery"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"gopkg.in/yaml.v3"
)

// Decoded is a response decoded for a transform. Kind names the decoder, and
// Data holds what it produced: normalized JSON or YAML values, CSV rows, an
// *xmlquery.Node, an *HTMLDecoded, a model.MetricSet for Prometheus text, the
// Graphite series document (graphite.go), or the body as a string. Raw is the body the decoder read.
type Decoded struct {
	Kind string
	Data any
	Raw  []byte
	// Graphite reports what the graphite decoder left out; nil for the
	// others.
	Graphite *GraphiteReport
}

// HTMLDecoded is a parsed HTML document and the body it was parsed from.
type HTMLDecoded struct {
	Document *goquery.Document
	Raw      []byte
}

// detectFormat picks the decoder for a collector whose decoder.type is auto,
// by the response's content type, and then by its content.
func detectFormat(r *fetch.HTTPResponse) string {
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
	case ct == fetch.GraphiteContentType:
		return "graphite"
	}
	b := bytes.TrimSpace(r.Body)
	// JSON first: a JSON document may hold "# HELP " in a string, while
	// Prometheus exposition, which starts with a comment or a name, never
	// parses as JSON.
	if len(b) > 0 && (b[0] == '{' || b[0] == '[') {
		var x any
		if json.Unmarshal(b, &x) == nil {
			return "json"
		}
	}
	if bytes.Contains(b, []byte("# TYPE ")) || bytes.Contains(b, []byte("# HELP ")) {
		return "prometheus"
	}
	// An HTML page is markup too, and rarely well-formed XML, so it is
	// recognised by its doctype or root element before anything starting
	// with < is taken for XML.
	if head := bytes.ToLower(b[:min(len(b), 16)]); bytes.HasPrefix(head, []byte("<!doctype html")) || bytes.HasPrefix(head, []byte("<html")) {
		return "html"
	}
	if bytes.HasPrefix(b, []byte("<")) {
		return "xml"
	}
	if looksLikeCarbon(b) {
		return "graphite"
	}
	return "text"
}

// Decode converts r's body to UTF-8 and decodes it with the decoder c's
// configuration, the response's content type or its content selects.
func Decode(r *fetch.HTTPResponse, c *model.Collector) (*Decoded, error) {
	// The body is converted to UTF-8 before anything reads it (textencoding.go).
	named, err := convertToUTF8(r, c)
	if err != nil {
		return nil, err
	}
	kind := c.Decoder.Type
	if kind == "" || kind == "auto" {
		kind = detectFormat(r)
	}
	if !named {
		if err := convertFromDocument(r, kind); err != nil {
			return nil, err
		}
	}
	switch kind {
	case "json":
		var v any
		d := json.NewDecoder(bytes.NewReader(r.Body))
		d.UseNumber()
		if err := d.Decode(&v); err != nil {
			return nil, fmt.Errorf("JSON decode: %w", err)
		}
		return &Decoded{Kind: kind, Data: model.Normalize(v), Raw: r.Body}, nil
	case "yaml":
		var v any
		if err := yaml.Unmarshal(r.Body, &v); err != nil {
			return nil, fmt.Errorf("YAML decode: %w", err)
		}
		return &Decoded{Kind: kind, Data: model.Normalize(v), Raw: r.Body}, nil
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
	case "graphite":
		return decodeGraphite(r, c)
	default:
		return nil, fmt.Errorf("unsupported decoder %q", kind)
	}
}

func decodeCSV(r *fetch.HTTPResponse, c *model.Collector) (*Decoded, error) {
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

func decodePrometheus(r *fetch.HTTPResponse) (*Decoded, error) {
	metrics, err := parsePrometheusText(r.Body)
	if err != nil {
		return nil, fmt.Errorf("decoding Prometheus exposition: %w", err)
	}
	return &Decoded{Kind: "prometheus", Data: model.MetricSet{Metrics: metrics}, Raw: r.Body}, nil
}

// TextValue reads text extracted from a document as a number, ignoring
// surrounding space.
func TextValue(v string) (float64, error) { return strconv.ParseFloat(strings.TrimSpace(v), 64) }
