package decode

import (
	"net/http"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"golang.org/x/text/encoding/charmap"
)

// in1251 is text as a target writes it in windows-1251.
func in1251(t *testing.T, text string) string {
	t.Helper()
	raw, err := charmap.Windows1251.NewEncoder().String(text)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// A <meta> declares an encoding with a charset attribute, or with a content
// attribute beside http-equiv="content-type", however the tag is written:
// in any case, quoted either way or not at all, with blanks around the
// equals signs, its attributes in any order and on any line, after a slash.
// Each of these names koi8-r.
func TestAMetaDeclaresItsEncodingHoweverItIsWritten(t *testing.T) {
	for _, head := range []string{
		`<meta charset="koi8-r">`,
		`<meta charset='koi8-r'>`,
		`<meta charset=koi8-r>`,
		`<meta charset=koi8-r >`,
		`<META CHARSET="koi8-r">`,
		`<meta charset = "koi8-r" >`,
		"<meta\n\tcharset\n=\n\"koi8-r\"\n>",
		`<meta charset=" koi8-r ">`,
		`<meta charset="koi8-r"/>`,
		`<meta charset="koi8-r" />`,
		`<meta/charset="koi8-r">`,
		`<meta name="generator" charset="koi8-r">`,
		`<meta http-equiv="Content-Type" content="text/html; charset=koi8-r">`,
		`<meta content="text/html; charset=koi8-r" http-equiv="Content-Type">`,
		`<META HTTP-EQUIV="CONTENT-TYPE" CONTENT="TEXT/HTML; CHARSET=koi8-r">`,
		`<meta http-equiv=content-type content=text/html;charset=koi8-r>`,
		`<meta http-equiv='Content-Type' content='text/html; charset="koi8-r"'>`,
		`<meta http-equiv="Content-Type" content="text/html; charset = 'koi8-r'">`,
		`<meta http-equiv="Content-Type" content="text/html; charset=koi8-r; x=1">`,
		`<meta http-equiv="Content-Type" content="text/html; charset=koi8-r boundary">`,
		// The first "charset" that an equals sign follows.
		`<meta http-equiv="Content-Type" content="charset; charset=koi8-r">`,
		// A charset attribute is the tag's word, before and after a content.
		`<meta http-equiv="Content-Type" content="text/html; charset=windows-1251" charset="koi8-r">`,
		`<meta charset="koi8-r" http-equiv="Content-Type" content="text/html; charset=windows-1251">`,
		// The first attribute of a name is the one that counts.
		`<meta charset="koi8-r" charset="windows-1251">`,
		`<meta http-equiv="Content-Type" http-equiv="refresh" content="text/html; charset=koi8-r" content="text/html; charset=windows-1251">`,
		// The first meta that declares one is the answer.
		`<meta charset="koi8-r"><meta charset="windows-1251">`,
		`<meta name="viewport" content="width=device-width"><meta charset="koi8-r">`,
		`<meta charset=""><meta charset="koi8-r">`,
		// A > in a quoted value does not end the tag.
		`<meta name="a>b" charset="koi8-r">`,
		`<a title=">"><meta charset="koi8-r">`,
		// After the things a page starts with.
		"<!DOCTYPE html>\n<html lang=\"ru\">\n<head>\n<meta charset=\"koi8-r\">",
		`<?php header('X: 1') ?><meta charset="koi8-r">`,
		`<title>Depots</title></head><body><p>text</p><meta charset="koi8-r">`,
		`<!-- generated --><meta charset="koi8-r">`,
		`<!--><meta charset="koi8-r">`,
		`<!---><meta charset="koi8-r">`,
		`<!-- a > b -- > c --><meta charset="koi8-r">`,
		`<script src="a.js"></script><meta charset="koi8-r">`,
		`<script>for (i = 0; i < n; i++) { s += '<td>' }</script><meta charset="koi8-r">`,
		`2 < 3 <meta charset="koi8-r">`,
		// The text of a title and of a script is text like any other, as
		// the standard reads it: a tag written there is a tag.
		`<title><meta charset="koi8-r"></title>`,
		`<script>document.write('<meta charset="koi8-r">')</script>`,
	} {
		if got := metaDeclaredCharset([]byte(head)); got != "koi8-r" {
			t.Errorf("%s names %q, want koi8-r", head, got)
		}
	}
}

// What only looks like a declaration is none: a meta in a comment, a
// content attribute of a meta that is no http-equiv="content-type", the
// word in another attribute, in another element, in an attribute's value or
// in text, an element whose name only starts with meta, a charset naming
// nothing.
func TestWhatOnlyLooksLikeAMetaCharsetDeclaresNothing(t *testing.T) {
	for _, head := range []string{
		``,
		`<html><head><title>Depots</title></head>`,
		`<!-- <meta charset="koi8-r"> -->`,
		`<!-- <meta http-equiv="Content-Type" content="text/html; charset=windows-1251"> -->`,
		`<!--<meta charset="koi8-r">`,
		`<meta name="description" content="about charset=windows-1251 pages">`,
		`<meta content="text/html; charset=koi8-r">`,
		`<meta http-equiv="refresh" content="0; url=/depots?charset=koi8-r">`,
		`<meta http-equiv="Content-Language" content="ru; charset=koi8-r">`,
		`<meta property="og:description" content="charset=koi8-r">`,
		`<meta http-equiv="refresh" http-equiv="Content-Type" content="text/html; charset=koi8-r">`,
		`<meta http-equiv="Content-Type" content="text/html">`,
		`<meta http-equiv="Content-Type" content="text/html; charset">`,
		`<meta http-equiv="Content-Type" content="text/html; charset=">`,
		`<meta http-equiv="Content-Type" content='text/html; charset="koi8-r'>`,
		`<meta http-equiv="Content-Type" content="text/html; charsets=koi8-r">`,
		`<meta name="charset" content="koi8-r">`,
		`<meta name="charset=koi8-r">`,
		`<meta data-charset="koi8-r">`,
		`<meta charset>`,
		`<meta charset="">`,
		`<meta charset=" ">`,
		// A charset attribute naming nothing is still the tag's word.
		`<meta charset="" http-equiv="Content-Type" content="text/html; charset=koi8-r">`,
		`<meta>`,
		`<metadata charset="koi8-r">`,
		`<meta-data charset="koi8-r">`,
		`<title>charset=koi8-r</title>`,
		`<script>var charset="koi8-r";</script>`,
		`<a href="/depots?charset=koi8-r">Depots</a>`,
		`<link rel="alternate" title="<meta charset=koi8-r>">`,
		`<link rel="alternate" title='<meta charset="koi8-r">'>`,
		`<!DOCTYPE html SYSTEM "<meta charset=koi8-r>">`,
		`<?php echo '<meta charset=koi8-r>' ?>`,
		`</p <meta charset=koi8-r>`,
	} {
		if got := metaDeclaredCharset([]byte(head)); got != "" {
			t.Errorf("%s names %q, want nothing", head, got)
		}
	}
}

// The head is the first 1024 bytes. A meta that ends within them is read;
// one that starts after them is not, and neither is one whose value they
// cut off, quoted or not: half a name is not the name.
func TestAMetaIsReadInTheFirst1024BytesOnly(t *testing.T) {
	page := func(before int, meta string) *fetch.HTTPResponse {
		body := "<html><head><!--" + strings.Repeat("-", before-len("<html><head><!---->")) + "-->" + meta + "</head><body><p id=v>\xd1\xee\xf4\xe8\xff</p></body></html>"
		return &fetch.HTTPResponse{Body: []byte(body)}
	}
	for _, test := range []struct {
		before int
		meta   string
		want   string
	}{
		{1024 - len(`<meta charset="windows-1251">`), `<meta charset="windows-1251">`, "windows-1251"},
		// The closing quote, and the > of the bare value, are byte 1025.
		{1024 - len(`<meta charset="windows-1251`), `<meta charset="windows-1251">`, ""},
		{1024 - len(`<meta charset=windows-1251`), `<meta charset=windows-1251>`, ""},
		// Cut in the middle, where a search for charset= would read
		// windows-125, which is no encoding, or iso-8859-1 for iso-8859-15.
		{1024 - len(`<meta charset="windows-125`), `<meta charset="windows-1251">`, ""},
		{1024 - len(`<meta charset=iso-8859-1`), `<meta charset=iso-8859-15>`, ""},
		// The value is whole, and the tag is cut after it.
		{1024 - len(`<meta charset="windows-1251"`), `<meta charset="windows-1251" data-x="1">`, "windows-1251"},
		{1024, `<meta charset="windows-1251">`, ""},
		{2000, `<meta charset="windows-1251">`, ""},
	} {
		r := page(test.before, test.meta)
		if at := strings.Index(string(r.Body), "<meta"); at != test.before {
			t.Fatalf("the meta is at %d, want %d", at, test.before)
		}
		if got := documentCharset(r, "html"); got != test.want {
			t.Errorf("a meta %s at byte %d names %q, want %q", test.meta, test.before, got, test.want)
		}
	}
	// A page that ends in the middle of a tag declares nothing with it.
	for _, head := range []string{`<meta charset=koi8-r`, `<meta charset="koi8-r`, `<meta charset`, `<meta charset=`, `<meta charset = `, `<meta `, `<a href="x`} {
		if got := metaDeclaredCharset([]byte(head)); got != "" {
			t.Errorf("%s names %q, want nothing", head, got)
		}
	}
}

// decodeHTML decodes body as an answer with that Content-Type, with the
// decoder of a css collector (html) or the one an xpath collector is left
// with (auto), and returns the text of the element with the id v, the
// Content-Type the transform sees, and the error.
func decodeHTML(t *testing.T, kind, contentType, charset, body string) (text, sees string, err error) {
	t.Helper()
	headers := make(http.Header)
	if contentType != "" {
		headers.Set("Content-Type", contentType)
	}
	r := &fetch.HTTPResponse{Body: []byte(body), Headers: headers}
	c := model.Collector{Request: model.RequestConfig{Type: fetch.RequestTypeHTTP}, Decoder: model.DecoderConfig{Type: kind}, Response: model.ResponseConfig{Charset: charset}}
	d, err := Decode(r, &c)
	if err != nil {
		return "", headers.Get("Content-Type"), err
	}
	if d.Kind != "html" {
		t.Fatalf("decoded as %s, want html", d.Kind)
	}
	return htmlText(t, d, "#v"), headers.Get("Content-Type"), nil
}

// A UTF-8 page whose head has a commented-out meta naming windows-1251
// before its own meta, or a description that speaks of charset=windows-1251,
// is read as the UTF-8 it is, and not as windows-1251: nothing in it is
// converted. A page in windows-1251 whose declaration comes after such
// things is read by its declaration.
func TestACommentedOutMetaAndADescriptionDoNotNameTheEncoding(t *testing.T) {
	const body = `<title>Depots</title></head><body><p id=v>София</p></body></html>`
	for _, head := range []string{
		`<html><head><!-- <meta http-equiv="Content-Type" content="text/html; charset=windows-1251"> -->` + "\n" + `<meta charset="utf-8">`,
		`<html><head><meta name="description" content="about charset=windows-1251 pages">`,
		`<html><head><meta http-equiv="refresh" content="60; url=/depots?charset=windows-1251">`,
	} {
		for _, kind := range []string{"html", "auto"} {
			text, _, err := decodeHTML(t, kind, "text/html", "", head+body)
			if err != nil || text != "София" {
				t.Errorf("%s as %s: %q %v, want София", head, kind, text, err)
			}
		}
	}
	legacy := in1251(t, `<html><head><!-- <meta charset="koi8-r"> --><title>charset=koi8-r</title><meta name="description" content="Складове; charset=utf-8">`+
		`<meta http-equiv="Content-Type" content="text/html; charset=windows-1251">`+body)
	for _, kind := range []string{"html", "auto"} {
		text, sees, err := decodeHTML(t, kind, "text/html", "", legacy)
		if err != nil || text != "София" || sees != "text/html; charset=utf-8" {
			t.Errorf("after decoys, as %s: %q %q %v, want София", kind, text, sees, err)
		}
	}
}

// A meta whose charset is no encoding's name declares nothing, as the
// standard's prescan has it, and the meta after it is looked at: a name
// nobody knows, the "utf-8/" a bare value run into the slash of a
// self-closed tag is, a name with a semicolon, a comma or a blank in it, a
// bare value a tag that is not closed runs into the next one. The decode
// failed over each of these, naming it, where a browser reads the page.
func TestAMetaNamingNoEncodingIsPassedOver(t *testing.T) {
	for head, want := range map[string]string{
		`<meta charset="Klingon">`: "",
		`<meta http-equiv="Content-Type" content="text/html; charset=klingon">`: "",
		`<meta charset=utf-8/>`:                "",
		`<meta charset=koi8-r/>`:               "",
		`<meta charset="koi8-r;">`:             "",
		`<meta charset="koi8 r">`:              "",
		`<meta charset=koi8-r<title>t</title>`: "",
		`<meta http-equiv="Content-Type" content="text/html; charset=koi8-r,foo">`: "",
		// The next meta that names an encoding is the answer.
		`<meta charset="klingon"><meta charset="koi8-r">`:                                                  "koi8-r",
		`<meta charset=utf-8/><meta http-equiv="Content-Type" content="text/html; charset=koi8-r">`:        "koi8-r",
		`<meta charset="koi8-r;"><meta charset="klingon"><meta name="x" content="y"><meta charset=koi8-r>`: "koi8-r",
		// A charset attribute is the tag's word over its content, also
		// when it names no encoding.
		`<meta charset="klingon" http-equiv="Content-Type" content="text/html; charset=koi8-r">`: "",
		// A blank before the slash, or quotes, and the value is the name.
		`<meta charset=koi8-r />`:  "koi8-r",
		`<meta charset="koi8-r"/>`: "koi8-r",
	} {
		if got := metaDeclaredCharset([]byte(head)); got != want {
			t.Errorf("%s names %q, want %q", head, got, want)
		}
	}
	// A UTF-8 page with such a meta is read, being the UTF-8 a page that
	// declares nothing is taken for, under the Content-Type it came with.
	for _, meta := range []string{`<meta charset=utf-8/>`, `<meta charset="utf-8;">`, `<meta charset="utf 8">`, `<meta charset="Klingon">`,
		`<meta http-equiv="Content-Type" content="text/html; charset=klingon">`} {
		for _, kind := range []string{"html", "auto"} {
			body := "<!DOCTYPE html><html><head>" + meta + "<title>t</title></head><body><p id=v>София</p></body></html>"
			text, sees, err := decodeHTML(t, kind, "text/html", "", body)
			if err != nil || text != "София" || sees != "text/html" {
				t.Errorf("%s as %s: %q under %q, %v; want София under text/html", meta, kind, text, sees, err)
			}
			if from := ConvertedFrom(&fetch.HTTPResponse{Body: []byte(body), Headers: http.Header{}}, &model.Collector{}, "html"); from != "" {
				t.Errorf("%s is said to be converted from %s", meta, from)
			}
		}
	}
	// A windows-1251 page whose meta is written that way has declared
	// nothing: it is not converted, and its bytes are left for the repair
	// after the transform. With a blank before the slash it is declared.
	for meta, want := range map[string]string{
		`<meta charset=windows-1251/>`:   "\xd1\xee\xf4\xe8\xff",
		`<meta charset="windows-1251;">`: "\xd1\xee\xf4\xe8\xff",
		`<meta http-equiv="Content-Type" content="text/html; charset=windows-1251,foo">`: "\xd1\xee\xf4\xe8\xff",
		`<meta charset=windows-1251 />`:                         "София",
		`<meta charset="klingon"><meta charset="windows-1251">`: "София",
	} {
		text, _, err := decodeHTML(t, "html", "text/html", "", in1251(t, "<html><head>"+meta+"<title>t</title></head><body><p id=v>София</p></body></html>"))
		if err != nil || text != want {
			t.Errorf("%s: %q %v, want %q", meta, text, err, want)
		}
	}
	// The Content-Type's word is not a page's: an unknown name there fails.
	if _, _, err := decodeHTML(t, "html", "text/html; charset=klingon", "", `<html><head><meta charset="utf-8"></head></html>`); err == nil || !strings.Contains(err.Error(), `unsupported charset "klingon"`) {
		t.Errorf("an unknown charset in the Content-Type: %v", err)
	}
}

// A meta is a declaration in a document decoded as HTML only. In a JSON,
// YAML, CSV or text body, and in an XML document, which has its own
// declaration, it is text, and the body is left as it is.
func TestAMetaInABodyThatIsNotHTMLMeansNothing(t *testing.T) {
	for _, test := range []struct{ kind, contentType, body string }{
		{"auto", "application/json", `{"page": "<meta charset=\"windows-1251\">", "city": "София"}`},
		{"json", "", `{"page": "<meta charset=\"windows-1251\">", "city": "София"}`},
		{"auto", "application/yaml", "page: '<meta charset=\"windows-1251\">'\ncity: София\n"},
		{"auto", "text/csv", "page,city\n<meta charset=windows-1251>,София\n"},
		{"auto", "text/plain", "София <meta charset=\"windows-1251\">\n"},
		{"text", "text/html", "<meta charset=\"windows-1251\"> София\n"},
		{"auto", "application/xml", `<page><meta charset="windows-1251"/><city>София</city></page>`},
		{"xml", "text/html", `<?xml version="1.0"?><page><meta charset="windows-1251"/><city>София</city></page>`},
	} {
		r := &fetch.HTTPResponse{Body: []byte(test.body), Headers: http.Header{}}
		if test.contentType != "" {
			r.Headers.Set("Content-Type", test.contentType)
		}
		c := model.Collector{Request: model.RequestConfig{Type: fetch.RequestTypeHTTP}, Decoder: model.DecoderConfig{Type: test.kind}}
		d, err := Decode(r, &c)
		if err != nil {
			t.Fatalf("%s as %s: %v", test.body, test.kind, err)
		}
		if d.Kind == "html" || string(d.Raw) != test.body || r.Headers.Get("Content-Type") != test.contentType {
			t.Errorf("%s as %s is %s, a body of %q under %q", test.body, test.kind, d.Kind, d.Raw, r.Headers.Get("Content-Type"))
		}
		if from := ConvertedFrom(&fetch.HTTPResponse{Body: []byte(test.body), Headers: http.Header{}}, &c, d.Kind); from != "" {
			t.Errorf("%s as %s is said to be converted from %s", test.body, test.kind, from)
		}
	}
}

// An XHTML page whose only declaration is the encoding of its XML
// declaration is converted by it when it is decoded as HTML — by a css
// collector, whose decoder is html, and by an xpath collector left to an
// answer called text/html or application/xhtml+xml, or called nothing — as
// it is when it is called application/xml and read as XML. The declaration
// then says UTF-8, and the Content-Type too.
func TestAnXMLDeclarationNamesTheEncodingOfAPageDecodedAsHTML(t *testing.T) {
	page := in1251(t, `<?xml version="1.0" encoding="windows-1251"?>`+"\n"+
		`<!DOCTYPE html PUBLIC "-//W3C//DTD XHTML 1.0 Strict//EN" "http://www.w3.org/TR/xhtml1/DTD/xhtml1-strict.dtd">`+"\n"+
		`<html xmlns="http://www.w3.org/1999/xhtml"><head><title>Depots</title></head><body><p id="v">София</p></body></html>`)
	for _, test := range []struct{ kind, contentType, sees string }{
		{"html", "text/html", "text/html; charset=utf-8"},
		{"html", "application/xhtml+xml", "application/xhtml+xml; charset=utf-8"},
		{"html", "application/xml", "application/xml; charset=utf-8"},
		{"html", "", ""},
		{"auto", "text/html", "text/html; charset=utf-8"},
		{"auto", "application/xhtml+xml", "application/xhtml+xml; charset=utf-8"},
		{"auto", "text/plain", "text/plain; charset=utf-8"},
		{"auto", "", ""},
	} {
		r := &fetch.HTTPResponse{Body: []byte(page), Headers: http.Header{}}
		if test.contentType != "" {
			r.Headers.Set("Content-Type", test.contentType)
		}
		sent := &fetch.HTTPResponse{Body: r.Body, Headers: r.Headers.Clone()}
		c := model.Collector{Request: model.RequestConfig{Type: fetch.RequestTypeHTTP}, Decoder: model.DecoderConfig{Type: test.kind}}
		d, err := Decode(r, &c)
		if err != nil || d.Kind != "html" {
			t.Fatalf("%s under %q: %v", test.kind, test.contentType, err)
		}
		if text := htmlText(t, d, "#v"); text != "София" {
			t.Errorf("%s under %q reads %q, want София", test.kind, test.contentType, text)
		}
		if !strings.HasPrefix(string(r.Body), `<?xml version="1.0" encoding="UTF-8"?>`) || r.Headers.Get("Content-Type") != test.sees {
			t.Errorf("%s under %q leaves %q under %q", test.kind, test.contentType, r.Body[:40], r.Headers.Get("Content-Type"))
		}
		if from := ConvertedFrom(sent, &c, d.Kind); from != "windows-1251" {
			t.Errorf("%s under %q is said to be converted from %q", test.kind, test.contentType, from)
		}
	}
	// Called XML and left to the answer it is XML, converted as it was.
	r := &fetch.HTTPResponse{Body: []byte(page), Headers: http.Header{"Content-Type": {"application/xml"}}}
	d, err := Decode(r, &model.Collector{Decoder: model.DecoderConfig{Type: "auto"}})
	if err != nil || d.Kind != "xml" || !strings.Contains(string(r.Body), "София") {
		t.Errorf("as XML: %v %q", err, r.Body)
	}
}

// The XML declaration of a page decoded as HTML is the last word in the
// order: after a byte order mark, response.charset and the charset of the
// Content-Type, and after a meta of the same page, which a browser believes
// over it; it was read before the meta, so that a UTF-8 page declared so by
// its meta under a declaration saying ISO-8859-1 came out as the text its
// bytes spell there. It names the encoding of a page without a meta that
// declares one. It is a declaration only at the first byte of the document:
// after a blank, a line or a comment "<?xml" is none. And one naming what is
// no encoding declares nothing, where it failed the decode.
func TestTheXMLDeclarationOfAnHTMLPageInTheOrderOfDeclarations(t *testing.T) {
	const rest = `<html><head>%s<title>Depots</title></head><body><p id="v">%s</p></body></html>`
	page := func(declaration, meta, text string) string {
		return declaration + strings.Replace(strings.Replace(rest, "%s", meta, 1), "%s", text, 1)
	}
	koi8 := `<meta http-equiv="Content-Type" content="text/html; charset=koi8-r" />`
	windows1251 := `<meta http-equiv="Content-Type" content="text/html; charset=windows-1251">`
	// София in windows-1251, and what those bytes spell in koi8-r.
	const raw, asKOI8 = "\xd1\xee\xf4\xe8\xff", "яНТХЪ"
	for name, test := range map[string]struct {
		contentType, charset, body, want string
	}{
		"the declaration alone":             {"text/html", "", in1251(t, page(`<?xml version="1.0" encoding="windows-1251"?>`, "", "София")), "София"},
		"single quotes, upper case":         {"text/html", "", in1251(t, page(`<?xml version='1.0' encoding='WINDOWS-1251' standalone='yes'?>`, "", "София")), "София"},
		"a meta that says another":          {"text/html", "", in1251(t, page(`<?xml version="1.0" encoding="koi8-r"?>`, windows1251, "София")), "София"},
		"a meta that says another, wrongly": {"text/html", "", in1251(t, page(`<?xml version="1.0" encoding="windows-1251"?>`, koi8, "София")), asKOI8},
		"a meta saying UTF-8 over it":       {"text/html", "", page(`<?xml version="1.0" encoding="ISO-8859-1"?>`, `<meta charset="utf-8">`, "café"), "café"},
		"a meta over one saying UTF-8":      {"text/html", "", in1251(t, page(`<?xml version="1.0" encoding="UTF-8"?>`, windows1251, "София")), "София"},
		"a meta over one naming nothing":    {"text/html", "", page(`<?xml version="1.0" encoding="latin-1"?>`, `<meta charset="utf-8">`, "café"), "café"},
		"a meta naming nothing leaves it":   {"text/html", "", in1251(t, page(`<?xml version="1.0" encoding="windows-1251"?>`, `<meta charset=windows-1251/>`, "София")), "София"},
		"a description leaves it":           {"text/html", "", in1251(t, page(`<?xml version="1.0" encoding="windows-1251"?>`, `<meta name="description" content="charset=koi8-r">`, "София")), "София"},
		"a Content-Type before it":          {"text/html; charset=windows-1251", "", in1251(t, page(`<?xml version="1.0" encoding="koi8-r"?>`, "", "София")), "София"},
		"response.charset before it":        {"text/html", "windows-1251", in1251(t, page(`<?xml version="1.0" encoding="koi8-r"?>`, "", "София")), "София"},
		"a byte order mark before it":       {"text/html", "", "\xef\xbb\xbf" + page(`<?xml version="1.0" encoding="windows-1251"?>`, "", "София"), "София"},
		"without an encoding, the meta":     {"text/html", "", in1251(t, page(`<?xml version="1.0"?>`, `<meta charset="windows-1251" />`, "София")), "София"},
		"after a comment it is none":        {"text/html", "", page(`<!-- generated --><?xml version="1.0" encoding="windows-1251"?>`, "", "София"), "София"},
		"after a blank it is none":          {"text/html", "", page(` <?xml version="1.0" encoding="windows-1251"?>`, "", "София"), "София"},
		"after empty lines it is none":      {"text/html", "", in1251(t, page("\n\n"+`<?xml version="1.0" encoding="windows-1251"?>`, "", "София")), raw},
		"UTF-16 means UTF-8, as in a meta":  {"text/html", "", page(`<?xml version="1.0" encoding="UTF-16"?>`, "", "София"), "София"},
		"UTF-8 is left as it is":            {"text/html", "", page(`<?xml version="1.0" encoding="UTF-8"?>`, "", "София"), "София"},
		"one naming nothing is none":        {"text/html", "", page(`<?xml version="1.0" encoding="klingon"?>`, "", "София"), "София"},
		"one naming nothing, in 1251":       {"text/html", "", in1251(t, page(`<?xml version="1.0" encoding="latin-1"?>`, "", "София")), raw},
	} {
		for _, kind := range []string{"html", "auto"} {
			text, _, err := decodeHTML(t, kind, test.contentType, test.charset, test.body)
			if err != nil || text != test.want {
				t.Errorf("%s, as %s: %q %v, want %s", name, kind, text, err, test.want)
			}
		}
	}
	// Read as XML the document is the XML decoder's, whose declaration may
	// follow blanks and fails the decode when it names nothing known.
	for body, want := range map[string]string{
		in1251(t, " "+`<?xml version="1.0" encoding="windows-1251"?><p>София</p>`): "",
		`<?xml version="1.0" encoding="klingon"?><p>София</p>`:                     `unsupported charset "klingon"`,
	} {
		r := &fetch.HTTPResponse{Body: []byte(body), Headers: http.Header{"Content-Type": {"application/xml"}}}
		_, err := convertAsDecodeDoes(r, &model.Collector{Decoder: model.DecoderConfig{Type: "xml"}})
		if want == "" && (err != nil || !strings.Contains(string(r.Body), "София")) || want != "" && (err == nil || !strings.Contains(err.Error(), want)) {
			t.Errorf("%q as XML: %v, a body of %q; want %q", body, err, r.Body, want)
		}
	}
}
