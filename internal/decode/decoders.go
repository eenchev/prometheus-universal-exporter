package decode

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/PuerkitoBio/goquery"
	"github.com/antchfx/xmlquery"
	"github.com/eenchev/prometheus-universal-exporter/internal/expr"
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
		v, err := decodeYAML(r.Body)
		if err != nil {
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
		return decodePrometheus(r, c)
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
		// A header naming one column twice would have the later column
		// overwrite the earlier in every row, without a word.
		column := map[string]int{}
		for i := range heads {
			if cfg.TrimSpace {
				heads[i] = strings.TrimSpace(heads[i])
			}
			if heads[i] == "" {
				continue
			}
			if first, seen := column[heads[i]]; seen {
				return nil, fmt.Errorf("CSV header names column %q twice, as columns %d and %d; rename one, or set response.csv.header: false and read the columns by number", heads[i], first+1, i+1)
			}
			column[heads[i]] = i
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

func decodePrometheus(r *fetch.HTTPResponse, c *model.Collector) (*Decoded, error) {
	options := promOptions{openMetrics: isOpenMetrics(r)}
	// A prometheus transform passes on only the series its rules, or its
	// include and exclude, pick, each at least once, so the decoder keeps
	// only those, and stops at the first past limits.max_metrics, as the
	// transform would have: a body of a million series costs a scrape
	// that keeps ten what the ten cost. A pre-script, or a python
	// transform, is given every series, and the transform counts what it
	// makes of them.
	if c.Transform.Type == "prometheus" && strings.TrimSpace(c.Transform.PreScript) == "" {
		options.keep = prometheusKeeps(c)
		options.limit = c.Limits.MaxMetrics
	}
	metrics, err := parseExposition(r.Body, options)
	if errors.Is(err, model.ErrLimitExceeded) {
		return nil, err
	}
	if err != nil {
		format := "Prometheus exposition"
		if options.openMetrics {
			format = "OpenMetrics exposition"
		}
		return nil, fmt.Errorf("decoding %s: %w", format, err)
	}
	return &Decoded{Kind: "prometheus", Data: model.MetricSet{Metrics: metrics}, Raw: r.Body}, nil
}

// prometheusKeeps says which metric names a prometheus transform passes on,
// as applyPrometheusTransform decides: with rules, the names a rule's
// expression, or its name when it has none, matches; without, the names
// include matches, or every name when it is empty, less those exclude
// matches. An expression that does not compile keeps everything, and the
// transform reports it.
func prometheusKeeps(c *model.Collector) func(string) bool {
	compile := func(patterns []string) ([]*regexp.Regexp, bool) {
		out := make([]*regexp.Regexp, 0, len(patterns))
		for _, pattern := range patterns {
			re, err := expr.CompileRegex(pattern)
			if err != nil {
				return nil, false
			}
			out = append(out, re)
		}
		return out, true
	}
	matchesAny := func(res []*regexp.Regexp, name string) bool {
		for _, re := range res {
			if re.MatchString(name) {
				return true
			}
		}
		return false
	}
	if len(c.Metrics) > 0 {
		patterns := make([]string, 0, len(c.Metrics))
		for _, rule := range c.Metrics {
			pattern := rule.Expression
			if pattern == "" {
				pattern = "^" + regexp.QuoteMeta(rule.Name) + "$"
			}
			patterns = append(patterns, pattern)
		}
		rules, ok := compile(patterns)
		if !ok {
			return nil
		}
		return func(name string) bool { return matchesAny(rules, name) }
	}
	includes, okIncludes := compile(c.Transform.Include)
	excludes, okExcludes := compile(c.Transform.Exclude)
	if !okIncludes || !okExcludes {
		return nil
	}
	if len(includes) == 0 && len(excludes) == 0 {
		return nil
	}
	return func(name string) bool {
		return (len(includes) == 0 || matchesAny(includes, name)) && !matchesAny(excludes, name)
	}
}

// decodeYAML decodes a YAML document as the transforms read it. A scalar
// that YAML reads as a timestamp, such as updated: 2024-06-01, is kept as the
// text it was written as: decoded, it would be a time.Time, which neither jq
// nor a label can use as written.
func decodeYAML(body []byte) (any, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(body, &root); err != nil {
		return nil, err
	}
	if root.Kind == 0 {
		// An empty document.
		return nil, nil
	}
	timestampsAsText(&root, map[*yaml.Node]bool{})
	var v any
	if err := root.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

// timestampsAsText retags every timestamp scalar under n as a string.
func timestampsAsText(n *yaml.Node, seen map[*yaml.Node]bool) {
	if n == nil || seen[n] {
		return
	}
	seen[n] = true
	if n.Kind == yaml.ScalarNode && n.ShortTag() == "!!timestamp" {
		n.Tag = "!!str"
	}
	for _, child := range n.Content {
		timestampsAsText(child, seen)
	}
	timestampsAsText(n.Alias, seen)
}
