package decode

import (
	"mime"
	"net/http"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A Content-Type that is not well formed — a parameter without a value, two
// values a proxy joined with a comma, a parameter twice, blanks around an
// equals sign, something after a closing quote — still names the encoding with
// its charset parameter: the first one of the first value, in any case,
// quoted or bare. mime.ParseMediaType refuses every one of these headers,
// and with it the charset used to be lost.
func TestACharsetIsReadFromAContentTypeThatIsNotWellFormed(t *testing.T) {
	for contentType, want := range map[string]string{
		"text/html; charset=windows-1251; q":                                   "windows-1251",
		"text/html; charset=windows-1251, text/html":                           "windows-1251",
		"text/html; charset=windows-1251, text/plain; charset=koi8-r":          "windows-1251",
		`text/html; CHARSET="WINDOWS-1251", text/html`:                         "WINDOWS-1251",
		"text/html; q; charset=windows-1251":                                   "windows-1251",
		"text/html;;charset=windows-1251":                                      "windows-1251",
		"text/html ; charset = windows-1251 ; q":                               "windows-1251",
		"text/html; charset=koi8-r; charset=windows-1251":                      "koi8-r",
		`text/html; charset="windows-1251" x; q`:                               "windows-1251",
		`text/html; x="a, b; charset=koi8-r"; charset=windows-1251, text/html`: "windows-1251",
		"TEXT/HTML; q=; charset=windows-1251":                                  "windows-1251",
		"text/html; charset=klingon; q":                                        "klingon",
	} {
		if _, _, err := mime.ParseMediaType(contentType); err == nil {
			t.Errorf("%q is well formed, and no case of this test", contentType)
		}
		if got := declaredCharset(contentType); got != want {
			t.Errorf("%q names %q, want %q", contentType, got, want)
		}
	}
}

// A header that names no charset names none however it is read: one whose
// first value has no charset parameter, a charset without a value or with a
// value that is no name, one whose value opens a quote that nothing closes,
// and what is no Content-Type at all, which has no type and subtype to start
// with. Nothing fails over any of them: the rest of a header after a quote
// that is not closed was taken for the name, and failed the decode as an
// encoding nobody knows.
func TestAContentTypeWithoutACharsetNamesNone(t *testing.T) {
	for _, contentType := range []string{
		"",
		"text/html",
		"text/html; q",
		"text/html, text/html; charset=windows-1251",
		"text/html; q, text/html; charset=windows-1251",
		"text/html; charset=; q",
		`text/html; charset=""; q`,
		"text/html; charset; q",
		"text/html; charset=windows-1251 extra; q",
		`text/html; charset="; q`,
		`text/html; charset="windows-1251`,
		`text/html; charset="windows-1251; q`,
		`text/html; charset="windows-1251, text/html; charset=koi8-r`,
		`text/html; charset=windows-1251"; q`,
		`text/html; charset="`,
		"text/html; charset=a=b; q",
		"text/html; xcharset=windows-1251; q",
		`text/html; x="; charset=windows-1251"; q`,
		"text/html charset=windows-1251",
		"; charset=windows-1251",
		"charset=windows-1251",
		"charset=windows-1251; charset=windows-1251",
		"text; charset=windows-1251; q=",
		"/html; charset=windows-1251; q",
		"text/; charset=windows-1251; q",
		"te xt/html; charset=windows-1251; q",
		";;;",
		"\x00\xff; charset=windows-1251; q",
		"=",
		",",
	} {
		if got := declaredCharset(contentType); got != "" {
			t.Errorf("%q names %q, want nothing", contentType, got)
		}
		r := &fetch.HTTPResponse{Body: []byte("v=1 \xd1\xee\xf4\xe8\xff\n"), Headers: http.Header{"Content-Type": {contentType}}}
		d, err := Decode(r, &model.Collector{Decoder: model.DecoderConfig{Type: "text"}})
		if err != nil || d.Data != "v=1 \xd1\xee\xf4\xe8\xff\n" || r.Headers.Get("Content-Type") != contentType {
			t.Errorf("%q: %v, a body of %q under %q", contentType, err, r.Body, r.Headers.Get("Content-Type"))
		}
	}
}

// A body in windows-1251 under such a header is converted, by whichever
// decoder reads it, and the header the transform sees then says utf-8 where
// it said windows-1251 and is otherwise as the target sent it. The debug
// report is told what the body was converted from.
func TestABodyIsConvertedByTheCharsetOfAContentTypeThatIsNotWellFormed(t *testing.T) {
	for contentType, sees := range map[string]string{
		"text/html; charset=windows-1251; q":                 "text/html; charset=utf-8; q",
		"text/html; charset=windows-1251, text/html":         "text/html; charset=utf-8, text/html",
		`text/html; q; Charset="Windows-1251" ; x, text/css`: `text/html; q; Charset="utf-8" ; x, text/css`,
		`text/html; charset="windows-1251"x; q`:              `text/html; charset="utf-8"x; q`,
		"text/html; charset=cp1251; charset=koi8-r":          "text/html; charset=utf-8; charset=koi8-r",
	} {
		page := in1251(t, `<html><head><meta charset="koi8-r"><title>Depots</title></head><body><p id=v>София</p></body></html>`)
		for _, kind := range []string{"html", "auto"} {
			sent := &fetch.HTTPResponse{Body: []byte(page), Headers: http.Header{"Content-Type": {contentType}}}
			c := model.Collector{Decoder: model.DecoderConfig{Type: kind}}
			if from := ConvertedFrom(sent, &c, "html"); from != "windows-1251" {
				t.Errorf("%q is said to be converted from %q", contentType, from)
			}
			text, got, err := decodeHTML(t, kind, contentType, "", page)
			if err != nil || text != "София" || got != sees {
				t.Errorf("%q as %s: %q under %q, %v; want София under %q", contentType, kind, text, got, err, sees)
			}
		}
	}
	// response.charset and a byte order mark are read before such a header,
	// as before any other. The header's charset, which the body was not
	// read by and which is not true of the converted body either, says
	// utf-8 after it.
	text, sees, err := decodeHTML(t, "html", "text/html; charset=koi8-r; q", "windows-1251", in1251(t, `<html><body><p id=v>София</p></body></html>`))
	if err != nil || text != "София" || sees != "text/html; charset=utf-8; q" {
		t.Errorf("response.charset over a header that is not well formed: %q under %q, %v", text, sees, err)
	}
	text, sees, err = decodeHTML(t, "html", "text/html; charset=koi8-r; q", "", "\xef\xbb\xbf<html><body><p id=v>София</p></body></html>")
	if err != nil || text != "София" || sees != "text/html; charset=utf-8; q" {
		t.Errorf("a byte order mark over a header that is not well formed: %q under %q, %v", text, sees, err)
	}
	// Such a header naming an encoding nobody knows fails the decode, as a
	// well-formed one does.
	_, _, err = decodeHTML(t, "html", "text/html; charset=klingon; q", "", "<html></html>")
	if want := `unsupported charset "klingon"; use a name from the WHATWG Encoding Standard, such as utf-8, windows-1252, iso-8859-2, windows-1251, shift_jis or gbk`; err == nil || err.Error() != want {
		t.Errorf("an unknown charset in a header that is not well formed: %v", err)
	}
}

// The decoder is chosen by the type before the first semicolon, which these
// headers have where a well-formed one has it: the charset's being read
// changes nothing in which decoder reads the body.
func TestTheDecoderIsChosenByAContentTypeThatIsNotWellFormedAsItWas(t *testing.T) {
	for contentType, kind := range map[string]string{
		"text/html; charset=windows-1251; q":               "html",
		"text/html; charset=windows-1251, text/html":       "html",
		"application/json; charset=utf-8; q":               "json",
		"text/csv; charset=utf-8, text/csv":                "csv",
		"application/xml; charset=utf-8; q":                "xml",
		"text/plain; version=0.0.4; charset=utf-8; q":      "prometheus",
		"application/yaml; charset=utf-8; q":               "yaml",
		"text/plain; charset=utf-8; q":                     "text",
		"application/octet-stream; charset=windows-1251;;": "text",
	} {
		r := &fetch.HTTPResponse{Body: []byte("a 1\n"), Headers: http.Header{"Content-Type": {contentType}}}
		if _, err := convertToUTF8(r, &model.Collector{}); err != nil {
			t.Fatal(err)
		}
		if got := detectFormat(r); got != kind {
			t.Errorf("%q is read as %s, want %s", contentType, got, kind)
		}
	}
}
