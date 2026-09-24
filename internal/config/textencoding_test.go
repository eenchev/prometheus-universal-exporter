package config

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/antchfx/xmlquery"
	"github.com/eenchev/prometheus-universal-exporter/internal/decode"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/transform"
	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/unicode"
)

// Responses in other encodings, and output that is not valid UTF-8
// (decode/textencoding.go).

// encodeAs encodes UTF-8 text into an encoding, for a test body.
func latin1(t *testing.T, s string) []byte {
	t.Helper()
	b, err := charmap.Windows1252.NewEncoder().Bytes([]byte(s))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func cyrillic(t *testing.T, s string) []byte {
	t.Helper()
	b, err := charmap.Windows1251.NewEncoder().Bytes([]byte(s))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// collect decodes and transforms a body as a probe would.
func collect(t *testing.T, c model.Collector, body []byte, contentType string) *model.MetricSet {
	t.Helper()
	cfg := &model.Config{Collectors: []model.Collector{c}}
	if err := Validate(cfg); err != nil {
		t.Fatal(err)
	}
	c = cfg.Collectors[0]
	r := &fetch.HTTPResponse{Body: body, Headers: http.Header{}}
	if contentType != "" {
		r.Headers.Set("Content-Type", contentType)
	}
	d, err := decode.Decode(r, &c)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	set, err := transform.Transform(context.Background(), d, r, &c, "python3")
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	return set
}

func regexCollector(name string) model.Collector {
	return model.Collector{
		Name:      name,
		Request:   model.RequestConfig{Type: fetch.RequestTypeHTTP},
		Transform: model.TransformConfig{Type: "regex"},
		Metrics:   []model.MetricRule{{Name: "v", Type: model.GaugeMetricType, Expression: `v=(\d+) (?P<who>\S+)`, Labels: []model.LabelRule{{Name: "who", Expression: "who"}}}},
	}
}

func firstLabel(t *testing.T, set *model.MetricSet, label string) string {
	t.Helper()
	if set == nil || len(set.Metrics) == 0 {
		t.Fatal("no metrics")
	}
	return set.Metrics[0].Labels[label]
}

func TestTheDeclaredCharsetIsConverted(t *testing.T) {
	c := regexCollector("latin")
	for _, contentType := range []string{"text/plain; charset=ISO-8859-1", "text/plain; charset=windows-1252", `text/plain; charset="latin1"`} {
		if got := firstLabel(t, collect(t, c, latin1(t, "v=1 café\n"), contentType), "who"); got != "café" {
			t.Errorf("%s: who=%q", contentType, got)
		}
	}
	// Declared UTF-8 is left alone.
	if got := firstLabel(t, collect(t, c, []byte("v=1 café\n"), "text/plain; charset=utf-8"), "who"); got != "café" {
		t.Errorf("utf-8: who=%q", got)
	}
}

// response.charset names the encoding a target, or a file, does not declare,
// and wins over one it declares wrongly.
func TestResponseCharsetOverridesTheTarget(t *testing.T) {
	c := regexCollector("cyrillic")
	c.Response.Charset = "windows-1251"
	body := cyrillic(t, "v=1 Москва\n")
	for _, contentType := range []string{"", "text/plain; charset=utf-8"} {
		if got := firstLabel(t, collect(t, c, body, contentType), "who"); got != "Москва" {
			t.Errorf("%q: who=%q", contentType, got)
		}
	}
	bad := regexCollector("bad")
	bad.Response.Charset = "klingon"
	cfg := &model.Config{Collectors: []model.Collector{bad}}
	if err := Validate(cfg); err == nil || !strings.Contains(err.Error(), `response.charset: unsupported charset "klingon"`) {
		t.Fatalf("an unknown response.charset: %v", err)
	}
}

// A byte order mark wins, and is removed.
func TestByteOrderMarks(t *testing.T) {
	c := model.Collector{Name: "json", Request: model.RequestConfig{Type: fetch.RequestTypeHTTP}, Transform: model.TransformConfig{Type: "jq"}, Metrics: []model.MetricRule{{Name: "v", Type: model.GaugeMetricType, Expression: ".v", Labels: []model.LabelRule{{Name: "who", Expression: ".who"}}}}}
	doc := `{"v": 2, "who": "Zoë"}`
	utf16le, err := unicode.UTF16(unicode.LittleEndian, unicode.UseBOM).NewEncoder().Bytes([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	utf16be, err := unicode.UTF16(unicode.BigEndian, unicode.UseBOM).NewEncoder().Bytes([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string][]byte{
		"utf-8":    append([]byte{0xEF, 0xBB, 0xBF}, doc...),
		"utf-16le": utf16le,
		"utf-16be": utf16be,
	} {
		// The mark wins over a wrong declaration.
		set := collect(t, c, body, "application/json; charset=iso-8859-1")
		if set.Metrics[0].Value != 2 || set.Metrics[0].Labels["who"] != "Zoë" {
			t.Errorf("%s: %+v", name, set.Metrics)
		}
	}
}

// decodeBody decodes a body as the collector would.
func decodeBody(t *testing.T, c model.Collector, body []byte, contentType string) *decode.Decoded {
	t.Helper()
	cfg := &model.Config{Collectors: []model.Collector{c}}
	if err := Validate(cfg); err != nil {
		t.Fatal(err)
	}
	r := &fetch.HTTPResponse{Body: body, Headers: http.Header{"Content-Type": {contentType}}}
	d, err := decode.Decode(r, &cfg.Collectors[0])
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// HTML declares its encoding in a meta element, XML in its declaration.
func TestDocumentsDeclareTheirEncoding(t *testing.T) {
	html := model.Collector{Name: "css", Request: model.RequestConfig{Type: fetch.RequestTypeHTTP}, Response: model.ResponseConfig{Format: "html"}, Transform: model.TransformConfig{Type: "css"}, Metrics: []model.MetricRule{{Name: "v", Type: model.GaugeMetricType, Expression: "td"}}}
	for _, meta := range []string{`<meta charset="windows-1252">`, `<meta http-equiv="Content-Type" content="text/html; charset=ISO-8859-1">`} {
		page := latin1(t, "<html><head>"+meta+"</head><body><table><tr><td class=\"who\">café</td></tr></table></body></html>")
		d := decodeBody(t, html, page, "text/html")
		if got := d.Data.(*decode.HTMLDecoded).Document.Find("td.who").Text(); got != "café" {
			t.Errorf("%s: %q", meta, got)
		}
	}
	xml := model.Collector{Name: "xpath", Request: model.RequestConfig{Type: fetch.RequestTypeHTTP}, Response: model.ResponseConfig{Format: "xml"}, Transform: model.TransformConfig{Type: "xpath"}, Metrics: []model.MetricRule{{Name: "v", Type: model.GaugeMetricType, Expression: "/status/v"}}}
	doc := latin1(t, `<?xml version="1.0" encoding="ISO-8859-1"?><status><who>café</who><v>4</v></status>`)
	// From the declaration, and from a header saying the same: converted once.
	for _, contentType := range []string{"application/xml", "application/xml; charset=iso-8859-1"} {
		d := decodeBody(t, xml, doc, contentType)
		if got := xmlquery.FindOne(d.Data.(*xmlquery.Node), "/status/who").InnerText(); got != "café" {
			t.Errorf("%s: %q", contentType, got)
		}
	}
	// What transforms see after the decode says UTF-8.
	cfg := &model.Config{Collectors: []model.Collector{xml}}
	if err := Validate(cfg); err != nil {
		t.Fatal(err)
	}
	r := &fetch.HTTPResponse{Body: doc, Headers: http.Header{"Content-Type": {"application/xml; charset=iso-8859-1"}}}
	if _, err := decode.Decode(r, &cfg.Collectors[0]); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(r.Body), `encoding="UTF-8"`) || r.Headers.Get("Content-Type") != "application/xml; charset=utf-8" {
		t.Fatalf("after conversion: %q, %q", r.Body[:40], r.Headers.Get("Content-Type"))
	}
}

func TestAnUnknownDeclaredCharsetFailsTheDecode(t *testing.T) {
	c := regexCollector("unknown")
	cfg := &model.Config{Collectors: []model.Collector{c}}
	if err := Validate(cfg); err != nil {
		t.Fatal(err)
	}
	r := &fetch.HTTPResponse{Body: []byte("v=1 x\n"), Headers: http.Header{"Content-Type": {"text/plain; charset=klingon"}}}
	if _, err := decode.Decode(r, &cfg.Collectors[0]); err == nil || !strings.Contains(err.Error(), `unsupported charset "klingon"`) {
		t.Fatalf("err=%v", err)
	}
}
