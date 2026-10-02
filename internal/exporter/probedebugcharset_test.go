package exporter

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/decode"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// A debug report shows the response as the target sent it, not as the decode
// left it after converting its body to UTF-8 (probedebug.go, sentResponse).

// windows1251 is text in windows-1251, where each Cyrillic letter is one
// byte, 0x40 below the last byte of its UTF-16 code unit.
func windows1251(text string) []byte {
	var out []byte
	for _, r := range text {
		if r >= 'А' && r <= 'я' {
			out = append(out, byte(r-'А'+0xC0))
			continue
		}
		out = append(out, byte(r))
	}
	return out
}

// A target that answers in windows-1251 is reported with the Content-Type it
// sent and its body's size as sent; the body, whose bytes a UTF-8 report
// cannot show, is shown as the same text, and the report says what it was
// converted from. The rules read the converted text all the same.
func TestADebugProbeShowsTheResponseAsTheTargetSentIt(t *testing.T) {
	testutil.CaptureLogs(t)
	sent := windows1251(`{"up":1,"name":"Привет"}`)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=windows-1251")
		_, _ = w.Write(sent)
	}))
	t.Cleanup(target.Close)
	c := debugCollector("dbg")
	c.Metrics[0].Labels = []model.LabelRule{{Name: "name", Expression: ".name"}}
	server := modeServer(t, c)
	server.SetProbeDebug(true)
	body := debugProbeGet(t, server, "collector=dbg&debug=true&target="+url.QueryEscape(target.URL)).Body.String()
	assertContains(t, body,
		"Content-Type: application/json; charset=windows-1251",
		"Body: 24 bytes in windows-1251, converted to UTF-8 before decoding and shown here as UTF-8",
		`    {"up":1,"name":"Привет"}`,
		`demo_up{name="Привет"} 1`,
	)
	if strings.Contains(body, "charset=utf-8") {
		t.Errorf("the report shows a Content-Type the target did not send:\n%s", body)
	}
}

// A body that is UTF-8 already is not converted, and the report says
// nothing of a conversion and shows the Content-Type as the target wrote it,
// which the decode normalizes for the rules.
func TestADebugProbeSaysNothingOfAConversionThatDidNotHappen(t *testing.T) {
	testutil.CaptureLogs(t)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json;charset=UTF-8")
		_, _ = w.Write([]byte(`{"up":1}`))
	}))
	t.Cleanup(target.Close)
	server := modeServer(t, debugCollector("dbg"))
	server.SetProbeDebug(true)
	body := debugProbeGet(t, server, "collector=dbg&debug=true&target="+url.QueryEscape(target.URL)).Body.String()
	assertContains(t, body, "Content-Type: application/json;charset=UTF-8", "Body: 8 bytes\n")
	if strings.Contains(body, "converted") {
		t.Errorf("the report tells of a conversion:\n%s", body)
	}
}

// The copy a report keeps is not changed by the decode: it has the headers
// and the body the target sent, an XML declaration naming the encoding it
// was sent in, while the response the rules read is UTF-8 and says so. The
// copy shares the fetched body rather than holding a second one.
func TestTheDecodeLeavesTheReportsCopyOfTheResponseAlone(t *testing.T) {
	raw := windows1251(`<?xml version="1.0" encoding="windows-1251"?><a>Привет</a>`)
	r := &fetch.HTTPResponse{StatusCode: 200, Headers: http.Header{"Content-Type": {"application/xml"}}, Body: raw}
	c := &model.Collector{Name: "xml", Decoder: model.DecoderConfig{Type: "xml"}}
	sent := sentResponse(r)
	if &sent.Body[0] != &r.Body[0] {
		t.Fatal("the copy holds a second body")
	}
	decoded, err := decode.Decode(r, c)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(r.Body), `encoding="UTF-8"?><a>Привет</a>`) || r.Headers.Get("Content-Type") != "application/xml; charset=utf-8" {
		t.Fatalf("the rules read %q %q", r.Headers.Get("Content-Type"), r.Body)
	}
	if !bytes.Equal(sent.Body, raw) || sent.Headers.Get("Content-Type") != "application/xml" {
		t.Fatalf("the copy was changed: %q %q", sent.Headers.Get("Content-Type"), sent.Body)
	}
	if from := decode.ConvertedFrom(sent, c, decoded.Kind); from != "windows-1251" {
		t.Fatalf("converted from %q", from)
	}
	var b bytes.Buffer
	writeResponse(&b, sent, "windows-1251")
	assertContains(t, b.String(),
		"Content-Type: application/xml\n",
		"Body: 58 bytes in windows-1251, converted to UTF-8 before decoding and shown here as UTF-8",
		`<?xml version="1.0" encoding="windows-1251"?><a>Привет</a>`,
	)
}

// ConvertedFrom names the encoding by what names it for the decode, in the
// decode's order, and nothing for a body that is not converted.
func TestConvertedFromFollowsTheDecode(t *testing.T) {
	plain := &model.Collector{Name: "plain"}
	configured := &model.Collector{Name: "configured", Response: model.ResponseConfig{Charset: "koi8-r"}}
	for name, tc := range map[string]struct {
		contentType string
		body        []byte
		c           *model.Collector
		kind, want  string
	}{
		"nothing names one":                   {"text/plain", []byte("abc"), plain, "text", ""},
		"the Content-Type":                    {"text/plain; charset=ISO-8859-2", []byte("abc"), plain, "text", "iso-8859-2"},
		"response.charset over Content-Type":  {"text/plain; charset=ISO-8859-2", []byte("abc"), configured, "text", "koi8-r"},
		"a byte order mark over both":         {"text/plain; charset=ISO-8859-2", []byte{0xFF, 0xFE, 'a', 0}, configured, "text", "utf-16le"},
		"UTF-8 by its Content-Type":           {"text/plain; charset=utf8", []byte("abc"), plain, "text", ""},
		"UTF-8 by its byte order mark":        {"text/plain; charset=koi8-r", []byte{0xEF, 0xBB, 0xBF, 'a'}, plain, "text", ""},
		"an HTML meta":                        {"text/html", []byte(`<meta charset="windows-1251">`), plain, "html", "windows-1251"},
		"an HTML meta when a header names it": {"text/html; charset=koi8-r", []byte(`<meta charset="windows-1251">`), plain, "html", "koi8-r"},
		"a meta in a body read as text":       {"text/plain", []byte(`<meta charset="windows-1251">`), plain, "text", ""},
		"an unknown name":                     {"text/plain; charset=klingon", []byte("abc"), plain, "text", ""},
	} {
		r := &fetch.HTTPResponse{Headers: http.Header{"Content-Type": {tc.contentType}}, Body: tc.body}
		if got := decode.ConvertedFrom(r, tc.c, tc.kind); got != tc.want {
			t.Errorf("%s: converted from %q, want %q", name, got, tc.want)
		}
	}
}
