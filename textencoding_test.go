package main

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/antchfx/xmlquery"
	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/unicode"
)

// Responses in other encodings, and output that is not valid UTF-8
// (textencoding.go).

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
func collect(t *testing.T, c Collector, body []byte, contentType string) *MetricSet {
	t.Helper()
	cfg := &Config{Collectors: []Collector{c}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	c = cfg.Collectors[0]
	r := &HTTPResponse{Body: body, Headers: http.Header{}}
	if contentType != "" {
		r.Headers.Set("Content-Type", contentType)
	}
	d, err := decode(r, &c)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	set, err := transform(context.Background(), d, r, &c, "python3")
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	return set
}

func regexCollector(name string) Collector {
	return Collector{
		Name:      name,
		Request:   RequestConfig{Type: RequestTypeHTTP},
		Transform: TransformConfig{Type: "regex"},
		Metrics:   []MetricRule{{Name: "v", Type: GaugeMetricType, Expression: `v=(\d+) (?P<who>\S+)`, Labels: []LabelRule{{Name: "who", Type: "expression", Expression: "who"}}}},
	}
}

func firstLabel(t *testing.T, set *MetricSet, label string) string {
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
	cfg := &Config{Collectors: []Collector{bad}}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), `response.charset: unsupported charset "klingon"`) {
		t.Fatalf("an unknown response.charset: %v", err)
	}
}

// A byte order mark wins, and is removed.
func TestByteOrderMarks(t *testing.T) {
	c := Collector{Name: "json", Request: RequestConfig{Type: RequestTypeHTTP}, Transform: TransformConfig{Type: "jq"}, Metrics: []MetricRule{{Name: "v", Type: GaugeMetricType, Expression: ".v", Labels: []LabelRule{{Name: "who", Type: "expression", Expression: ".who"}}}}}
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
func decodeBody(t *testing.T, c Collector, body []byte, contentType string) *Decoded {
	t.Helper()
	cfg := &Config{Collectors: []Collector{c}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	r := &HTTPResponse{Body: body, Headers: http.Header{"Content-Type": {contentType}}}
	d, err := decode(r, &cfg.Collectors[0])
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// HTML declares its encoding in a meta element, XML in its declaration.
func TestDocumentsDeclareTheirEncoding(t *testing.T) {
	html := Collector{Name: "css", Request: RequestConfig{Type: RequestTypeHTTP}, Response: ResponseConfig{Format: "html"}, Transform: TransformConfig{Type: "css"}, Metrics: []MetricRule{{Name: "v", Type: GaugeMetricType, Expression: "td"}}}
	for _, meta := range []string{`<meta charset="windows-1252">`, `<meta http-equiv="Content-Type" content="text/html; charset=ISO-8859-1">`} {
		page := latin1(t, "<html><head>"+meta+"</head><body><table><tr><td class=\"who\">café</td></tr></table></body></html>")
		d := decodeBody(t, html, page, "text/html")
		if got := d.Data.(*HTMLDecoded).Document.Find("td.who").Text(); got != "café" {
			t.Errorf("%s: %q", meta, got)
		}
	}
	xml := Collector{Name: "xpath", Request: RequestConfig{Type: RequestTypeHTTP}, Response: ResponseConfig{Format: "xml"}, Transform: TransformConfig{Type: "xpath"}, Metrics: []MetricRule{{Name: "v", Type: GaugeMetricType, Expression: "/status/v"}}}
	doc := latin1(t, `<?xml version="1.0" encoding="ISO-8859-1"?><status><who>café</who><v>4</v></status>`)
	// From the declaration, and from a header saying the same: converted once.
	for _, contentType := range []string{"application/xml", "application/xml; charset=iso-8859-1"} {
		d := decodeBody(t, xml, doc, contentType)
		if got := xmlquery.FindOne(d.Data.(*xmlquery.Node), "/status/who").InnerText(); got != "café" {
			t.Errorf("%s: %q", contentType, got)
		}
	}
	// What transforms see says UTF-8.
	r := &HTTPResponse{Body: doc, Headers: http.Header{"Content-Type": {"application/xml; charset=iso-8859-1"}}}
	if _, err := convertToUTF8(r, &xml); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(r.Body), `encoding="UTF-8"`) || r.Headers.Get("Content-Type") != "application/xml; charset=utf-8" {
		t.Fatalf("after conversion: %q, %q", r.Body[:40], r.Headers.Get("Content-Type"))
	}
}

func TestAnUnknownDeclaredCharsetFailsTheDecode(t *testing.T) {
	c := regexCollector("unknown")
	cfg := &Config{Collectors: []Collector{c}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	r := &HTTPResponse{Body: []byte("v=1 x\n"), Headers: http.Header{"Content-Type": {"text/plain; charset=klingon"}}}
	if _, err := decode(r, &cfg.Collectors[0]); err == nil || !strings.Contains(err.Error(), `unsupported charset "klingon"`) {
		t.Fatalf("err=%v", err)
	}
}

// Output that still is not valid UTF-8 is repaired, counted and logged, and
// the scrape still parses.
func TestInvalidUTF8IsReplacedNotFatal(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("v=1 caf\xe9\n"))
	}))
	defer target.Close()
	server := verboseServer(t, false, regexCollector("broken"))
	logs := captureLogs(t)
	server.logger = slog.Default()
	recorder := probeOnce(t, server, "/probe?collector=broken&target="+target.URL, nil)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `v{who="caf`+"�"+`"} 1`) {
		t.Fatalf("%d %q", recorder.Code, recorder.Body.String())
	}
	if _, err := parsePrometheusText(recorder.Body.Bytes()); err != nil {
		t.Fatalf("the answer does not parse: %v", err)
	}
	if got := seriesValue(t, selfMetrics(t, server), `http_exporter_invalid_utf8_total{collector="broken"}`); got != 1 {
		t.Fatalf("counted %v", got)
	}
	if !strings.Contains(logs.String(), "were not valid UTF-8") || !strings.Contains(logs.String(), `"first_metric":"v"`) {
		t.Fatalf("not logged:\n%s", logs.String())
	}
}

func TestSanitizeUTF8(t *testing.T) {
	shared := map[string]string{"a": "ok\xff", "b": "fine"}
	set := &MetricSet{Metrics: []Metric{
		{Name: "x", Help: "h\xfe", Labels: shared},
		{Name: "y", Labels: shared},
		{Name: "z", Labels: map[string]string{"c": "clean"}},
	}}
	changed, first := sanitizeUTF8(set)
	if changed != 3 || first != "x" {
		t.Fatalf("changed=%d first=%q", changed, first)
	}
	if set.Metrics[0].Help != "h�" || set.Metrics[0].Labels["a"] != "ok�" || set.Metrics[1].Labels["a"] != "ok�" {
		t.Fatalf("%+v", set.Metrics)
	}
	if shared["a"] != "ok\xff" {
		t.Fatal("a label map shared with the transform was changed in place")
	}
}
