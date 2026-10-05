package decode

import (
	"bytes"
	"cmp"
	"fmt"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
	"golang.org/x/text/encoding/htmlindex"
	"golang.org/x/text/encoding/unicode"
)

// The encoding of a response is chosen differently in three cases than it
// was: a <meta> is found as the HTML standard's prescan finds it and not by
// a search for "charset=" in the head, so that one naming no encoding is
// passed over where it failed the decode; an XML declaration names the
// encoding of a page decoded as HTML that has no such <meta>; and the
// charset of a Content-Type that is not well formed is read
// (textencoding.go, htmlprescan.go). The choice
// as it was is kept here as the oracle, and these tests compare the two
// over every file of testdata, over the bodies of the tests that were, and
// over generated heads and headers: outside the cases each test names, the
// same body comes out under the same Content-Type, read by the same decoder,
// or the same error.

var oracleMetaCharsetRE = regexp.MustCompile(`(?i)<meta[^>]*?charset\s*=\s*["']?\s*([a-zA-Z0-9_:.+-]+)`)

// oracleDeclaredCharset is declaredCharset as it was.
func oracleDeclaredCharset(contentType string) string {
	if contentType == "" {
		return ""
	}
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return ""
	}
	return params["charset"]
}

// oracleDocumentCharset is documentCharset as it was.
func oracleDocumentCharset(r *fetch.HTTPResponse, kind string) string {
	head := r.Body[:min(len(r.Body), 1024)]
	switch kind {
	case "html":
		if m := oracleMetaCharsetRE.FindSubmatch(head); m != nil {
			return oracleMetaCharset(string(m[1]))
		}
	case "xml":
		if m := xmlEncodingRE.FindSubmatch(head); m != nil {
			return string(m[2])
		}
	}
	return ""
}

// oracleMetaCharset is metaCharset as it was: a name that is no encoding's
// is the answer, and the conversion fails naming it.
func oracleMetaCharset(name string) string {
	enc, err := htmlindex.Get(strings.TrimSpace(name))
	if err != nil {
		return name
	}
	switch canonical, _ := htmlindex.Name(enc); canonical {
	case "utf-16le", "utf-16be":
		return "utf-8"
	case "x-user-defined":
		return "windows-1252"
	}
	return name
}

// unsupported is the failure of a conversion from a name that is no
// encoding's.
func unsupported(name string) string {
	_, _, err := lookupCharset(name)
	return err.Error()
}

// oracleSetBody is setBody as it was.
func oracleSetBody(r *fetch.HTTPResponse, body []byte) {
	if m := xmlEncodingRE.FindSubmatchIndex(body); m != nil && !strings.EqualFold(string(body[m[4]:m[5]]), "utf-8") {
		body = append(append(append([]byte{}, body[:m[4]]...), "UTF-8"...), body[m[5]:]...)
	}
	r.Body = body
	if r.Headers == nil {
		return
	}
	if contentType := r.Headers.Get("Content-Type"); contentType != "" {
		if mediaType, params, err := mime.ParseMediaType(contentType); err == nil {
			params["charset"] = "utf-8"
			r.Headers.Set("Content-Type", mime.FormatMediaType(mediaType, params))
		}
	}
}

// oracleConvertFrom is convertFrom as it was.
func oracleConvertFrom(r *fetch.HTTPResponse, name string) error {
	enc, canonical, err := lookupCharset(name)
	if err != nil {
		return err
	}
	body := r.Body
	if canonical != "utf-8" {
		body, err = enc.NewDecoder().Bytes(r.Body)
		if err != nil {
			return fmt.Errorf("converting the body from %s: %w", canonical, err)
		}
	}
	oracleSetBody(r, body)
	return nil
}

// oracleConvert is what Decode did to a response before it decoded it:
// convertToUTF8, the choice of the decoder and convertFromDocument, as they
// were. It returns the decoder.
func oracleConvert(r *fetch.HTTPResponse, c *model.Collector) (string, error) {
	named := false
	if name, mark := bomOf(r); name != "" {
		body := r.Body[mark:]
		if name != "utf-8" {
			enc := unicode.UTF16(unicode.LittleEndian, unicode.IgnoreBOM)
			if name == "utf-16be" {
				enc = unicode.UTF16(unicode.BigEndian, unicode.IgnoreBOM)
			}
			converted, err := enc.NewDecoder().Bytes(body)
			if err != nil {
				return "", fmt.Errorf("converting the body from %s: %w", name, err)
			}
			body = converted
		}
		oracleSetBody(r, body)
		named = true
	} else {
		name := c.Response.Charset
		if name == "" && r.Headers != nil {
			name = oracleDeclaredCharset(r.Headers.Get("Content-Type"))
		}
		if name != "" {
			named = true
			if err := oracleConvertFrom(r, name); err != nil {
				return "", err
			}
		}
	}
	kind := c.Decoder.Type
	if kind == "" || kind == "auto" {
		kind = detectFormat(r)
	}
	if !named {
		if name := oracleDocumentCharset(r, kind); name != "" {
			if err := oracleConvertFrom(r, name); err != nil {
				return "", err
			}
		}
	}
	return kind, nil
}

// convertAsDecodeDoes is what Decode does to a response before it decodes
// it. It returns the decoder.
func convertAsDecodeDoes(r *fetch.HTTPResponse, c *model.Collector) (string, error) {
	named, err := convertToUTF8(r, c)
	if err != nil {
		return "", err
	}
	kind := c.Decoder.Type
	if kind == "" || kind == "auto" {
		kind = detectFormat(r)
	}
	if !named {
		if err := convertFromDocument(r, kind); err != nil {
			return "", err
		}
	}
	return kind, nil
}

// encodingCase is a response and the collector that reads it.
type encodingCase struct {
	body        []byte
	contentType string
	decoder     string
	charset     string
}

func (e encodingCase) String() string {
	return fmt.Sprintf("under %q, decoder %s, response.charset %q", e.contentType, e.decoder, e.charset)
}

// reading is what comes of a case: the decoder, the Content-Type the
// transform sees and the body, or the error.
type reading struct {
	kind, contentType string
	body              []byte
	failure           string
}

func (r reading) String() string {
	if r.failure != "" {
		return "error: " + r.failure
	}
	return fmt.Sprintf("decoder %s, Content-Type %q, body %.200q", r.kind, r.contentType, r.body)
}

// same reports whether two readings are the same in everything.
func (r reading) same(other reading) bool {
	return r.sameBody(other) && r.contentType == other.contentType
}

// sameBody reports whether two readings are the same but for the
// Content-Type the transform sees.
func (r reading) sameBody(other reading) bool {
	return r.kind == other.kind && r.failure == other.failure && bytes.Equal(r.body, other.body)
}

// read is what comes of the case with one of the two.
func (e encodingCase) read(convert func(*fetch.HTTPResponse, *model.Collector) (string, error)) reading {
	// Converting leaves the bytes of the body it is given alone.
	r := &fetch.HTTPResponse{Body: e.body, Headers: http.Header{}}
	if e.contentType != "" {
		r.Headers.Set("Content-Type", e.contentType)
	}
	c := &model.Collector{Decoder: model.DecoderConfig{Type: e.decoder}, Response: model.ResponseConfig{Charset: e.charset}}
	kind, err := convert(r, c)
	if err != nil {
		return reading{failure: err.Error()}
	}
	return reading{kind: kind, contentType: r.Headers.Get("Content-Type"), body: r.Body}
}

// unnamed reports whether nothing before the document itself names the
// encoding of the case, as it was read before: no byte order mark, no
// response.charset, no charset in a well-formed Content-Type.
func (e encodingCase) unnamed() bool {
	name, _ := bomOf(&fetch.HTTPResponse{Body: e.body})
	return name == "" && e.charset == "" && oracleDeclaredCharset(e.contentType) == ""
}

// wellFormedContentTypes are headers mime.ParseMediaType accepts, and none.
var wellFormedContentTypes = []string{
	"",
	"text/html",
	"text/html; charset=utf-8",
	`text/html;charset="WINDOWS-1251"`,
	"text/html; charset=utf-16",
	"text/html; charset=klingon",
	"application/xhtml+xml",
	"application/xml",
	"text/xml; charset=iso-8859-1",
	"application/json",
	"application/json; charset=iso-8859-1",
	"application/yaml",
	"text/csv",
	"text/csv; charset=utf-16be",
	"text/plain",
	"text/plain; version=0.0.4",
	"text/plain; version=0.0.4; charset=utf-8",
	"application/octet-stream",
	"text/html;",
	"text; charset=koi8-r",
}

// changedFixtures are the files of testdata whose encoding is chosen
// differently when nothing before the document names it, and why. Besides
// them, a file read as HTML that starts with an XML declaration naming an
// encoding is now read by it. The XHTML page whose meta and XML declaration
// disagree (depots-meta-over-xmldecl.xhtml) is not among them: it is read by
// its meta, as it was.
var changedFixtures = map[string]string{
	"html/charset/depots-utf8-commented-meta.html":   "the meta of a comment named windows-1251; its own meta names utf-8",
	"html/charset/depots-utf8-description-meta.html": "the content of its description named windows-1251; it declares nothing",
	"html/charset/depots-windows-1251-decoys.html":   "the meta of a comment named koi8-r; its http-equiv meta names windows-1251",
	"html/charset/depots-windows-1251-xmldecl.xhtml": "nothing named an encoding; its XML declaration names windows-1251",
	"html/attribute-names.xhtml":                     "nothing named an encoding; its XML declaration names UTF-8, which only adds charset=utf-8 to the Content-Type",
}

// Every file under testdata — HTML, CSV, XML, JSON, text and the rest —
// under each well-formed Content-Type and under none, read as HTML, XML,
// JSON, CSV and text and by the decoder its answer picks, with and without
// response.charset, comes out as it did: the same body, under the same Content-Type, for the same
// decoder, or the same error. The files that do not are the fixtures of the
// three cases, read as HTML with nothing before the document naming an
// encoding, and files that start with an XML declaration naming one, at
// their first byte, read as HTML; each of the fixtures does differ.
func TestTheEncodingOfEveryTestdataFileIsChosenAsItWas(t *testing.T) {
	const root = "../../testdata"
	differs := map[string]int{}
	compared := 0
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		name, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		name = filepath.ToSlash(name)
		for _, contentType := range wellFormedContentTypes {
			for _, decoder := range []string{"auto", "html", "xml", "json", "csv", "text"} {
				for _, charset := range []string{"", "windows-1251"} {
					e := encodingCase{body, contentType, decoder, charset}
					old, got := e.read(oracleConvert), e.read(convertAsDecodeDoes)
					compared++
					if old.same(got) {
						continue
					}
					_, listed := changedFixtures[name]
					startsWithXMLEncoding := bytes.HasPrefix(body, []byte("<?xml")) && xmlEncodingRE.Match(body[:min(len(body), 1024)])
					if !e.unnamed() || got.kind != "html" || !listed && !startsWithXMLEncoding {
						t.Errorf("testdata/%s %s:\nwas %s\nis  %s", name, e, old, got)
					}
					differs[name]++
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for name, why := range changedFixtures {
		if differs[name] == 0 {
			t.Errorf("testdata/%s is read as it was, and is listed as changed: %s", name, why)
		}
	}
	if cases := differs["html/charset/depots-meta-over-xmldecl.xhtml"]; cases != 0 {
		t.Errorf("the page whose meta and XML declaration disagree is read otherwise than it was in %d cases", cases)
	}
	for name, cases := range differs {
		t.Logf("testdata/%s differs in %d of its cases: %s", name, cases, cmp.Or(changedFixtures[name], "it starts with an XML declaration naming an encoding and was read as HTML"))
	}
	t.Logf("%d cases compared", compared)
}

// metaForms are the ways a page writes a meta that declares the encoding
// %s, which the search for "charset=" and the prescan read alike.
var metaForms = []string{
	`<meta charset="%s">`,
	`<meta charset='%s'>`,
	`<meta charset=%s>`,
	`<meta charset=%s >`,
	`<META CHARSET="%s">`,
	`<Meta Charset = "%s" >`,
	`<meta charset="%s"/>`,
	`<meta charset="%s" />`,
	`<meta charset=" %s">`,
	"<meta\n  charset=\"%s\"\n>",
	`<meta name="generator" charset="%s">`,
	`<meta http-equiv="Content-Type" content="text/html; charset=%s">`,
	`<meta content="text/html; charset=%s" http-equiv="Content-Type">`,
	`<META HTTP-EQUIV="CONTENT-TYPE" CONTENT="TEXT/HTML; CHARSET=%s">`,
	`<meta http-equiv=Content-Type content="text/html;charset=%s">`,
	`<meta http-equiv='content-type' content='text/html; charset=%s'>`,
	`<meta http-equiv="Content-Type" content="text/html; charset=%s" />`,
	`<meta http-equiv="Content-Type" content="application/xhtml+xml; charset = %s">`,
	`<meta http-equiv="Content-Type" content="text/html; charset=%s; x=1">`,
	"<meta http-equiv=\"Content-Type\"\n      content=\"text/html; charset=%s\">",
}

// metaNames are encodings by the names pages give them, and the names the
// prescan reads as another.
var metaNames = []string{"utf-8", "UTF-8", "windows-1251", "KOI8-R", "iso-8859-1", "Shift_JIS", "utf-16", "UTF-16LE", "x-user-defined", "iso_8859-1:1987"}

// unknownMetaNames are names that are no encoding's.
var unknownMetaNames = []string{"klingon", "x:y.z+1_2", "utf8x", "windows-125"}

// metaPlaces are what a head holds before its meta: nothing, a doctype,
// elements with attributes, comments, scripts, a title, and text.
var metaPlaces = []string{
	"",
	"<!DOCTYPE html>\n<html>\n<head>\n",
	"<!doctype html><html lang=\"bg\"><head><title>Depots</title>",
	`<!DOCTYPE html PUBLIC "-//W3C//DTD XHTML 1.0 Strict//EN" "http://www.w3.org/TR/xhtml1/DTD/xhtml1-strict.dtd"><html xmlns="http://www.w3.org/1999/xhtml"><head>`,
	"<?xml version=\"1.0\"?>\n<!DOCTYPE html>\n<html><head>",
	"<!-- generated on host a -->\n<html><head>",
	"<html><head><!-- a --><!-- b > c -- d -->",
	"<html><head><meta name=\"viewport\" content=\"width=device-width, initial-scale=1\"><link rel=\"stylesheet\" href=\"/a.css?v=1\">",
	"<html><head><script src=\"/a.js\"></script><script>var a = 1; if (a < 2) { a = \"x\"; }</script>",
	"<html><head><script>for (i = 0; i < n; i++) { s += '<td>' + i + '</td>'; }</script><style>p > a { color: red }</style>",
	"<html><head><title>a &lt; b, 2 > 1</title><base href=\"/\">",
	"<html><head></head><body><p>Depots</p><a href=\"/depots\" title=\"a > b\">all</a>",
	"<html><head>" + strings.Repeat("<link rel=\"preload\" href=\"/font.woff2\">", 20),
}

// A page with a meta in any of the forms pages write one in, naming any
// encoding, after any of the things a head starts with, under each
// Content-Type and under none, with and without a byte order mark and
// response.charset, read as HTML: the prescan chooses what the search for
// "charset=" chose, and the same body comes out.
//
// A meta in any of those forms naming what is no encoding is the one case
// that differs: it was the choice, and the conversion failed naming it; it
// declares nothing, and the body is left as it came, under the Content-Type
// it came with. That is so only where the document is asked, read as HTML
// with nothing before it naming the encoding; everywhere else such a page
// is read as it was, the failure over an unknown name in the Content-Type
// included.
func TestAMetaInEveryFormIsReadAsItWas(t *testing.T) {
	const rest = "</head><body><table id=\"depots\"><tr><td class=\"city\">\xd1\xee\xf4\xe8\xff</td></tr></table></body></html>"
	compared, passedOver := 0, 0
	for _, place := range metaPlaces {
		for _, form := range metaForms {
			for _, name := range slices.Concat(metaNames, unknownMetaNames) {
				unknown := slices.Contains(unknownMetaNames, name)
				head := place + strings.Replace(form, "%s", name, 1)
				r := &fetch.HTTPResponse{Body: []byte(head + rest)}
				if old, got := oracleDocumentCharset(r, "html"), documentCharset(r, "html"); unknown && (old != name || got != "") || !unknown && (old != got || got == "") {
					t.Errorf("%s names %q, and named %q", head, got, old)
				}
				cases := []encodingCase{
					{[]byte("\xef\xbb\xbf" + head + rest), "text/html", "html", ""},
					{[]byte(head + rest), "text/html", "html", "iso-8859-2"},
					{[]byte(head + rest), "text/html; charset=koi8-r", "auto", "iso-8859-2"},
				}
				for _, contentType := range []string{"", "text/html", "text/html; charset=koi8-r", "text/html; charset=klingon", "application/xhtml+xml", "text/plain"} {
					for _, decoder := range []string{"auto", "html", "text"} {
						cases = append(cases, encodingCase{[]byte(head + rest), contentType, decoder, ""})
					}
				}
				for _, e := range cases {
					compared++
					old, got := e.read(oracleConvert), e.read(convertAsDecodeDoes)
					if unknown && e.unnamed() && got.kind == "html" {
						passedOver++
						if left := (reading{kind: "html", contentType: e.contentType, body: e.body}); old.failure != unsupported(name) || !got.same(left) {
							t.Errorf("%s %s:\nwas %s\nis  %s\nwant the failure over %q, and the body as it came", head, e, old, got, name)
						}
						continue
					}
					if !old.same(got) {
						t.Errorf("%s %s:\nwas %s\nis  %s", head, e, old, got)
					}
				}
			}
		}
	}
	// A meta wholly within the first 1024 bytes, wherever it stands in
	// them, and one wholly after them.
	for _, name := range []string{"windows-1251", "koi8-r"} {
		for _, form := range metaForms[:4] {
			meta := strings.Replace(form, "%s", name, 1)
			for before := len("<!---->"); before <= 1100; before++ {
				if before > 1024-len(meta) && before < 1024 {
					// Cut by the end of the head: TestTheChoicesThatChanged.
					continue
				}
				head := "<!--" + strings.Repeat("-", before-len("<!---->")) + "-->" + meta
				r := &fetch.HTTPResponse{Body: []byte(head + rest)}
				compared++
				if old, got := oracleDocumentCharset(r, "html"), documentCharset(r, "html"); old != got || (got == "") != (before >= 1024) {
					t.Errorf("%s at byte %d names %q, and named %q", meta, before, got, old)
				}
			}
		}
	}
	if passedOver == 0 {
		t.Error("no meta naming an unknown encoding was passed over")
	}
	t.Logf("%d cases compared, %d of them a meta naming no encoding where the document is asked", compared, passedOver)
}

// The bodies of the tests of encodings that were, in this package, in
// internal/config and in internal/exporter, are read as they were.
func TestTheBodiesOfTheEarlierEncodingTestsAreReadAsTheyWere(t *testing.T) {
	utf16 := []byte{}
	for _, c := range "<html><p id=v>42</p></html>" {
		utf16 = append(utf16, byte(c), 0)
	}
	for _, e := range []encodingCase{
		{[]byte(`<html><head><meta charset="utf-16"></head><body><p id=v>42 café</p></body></html>`), "text/html", "auto", ""},
		{[]byte(`<html><head><meta charset="UTF-16LE"></head><body><p id=v>42 café</p></body></html>`), "text/html", "auto", ""},
		{[]byte(`<html><head><meta charset="utf-16be"></head><body><p id=v>42 café</p></body></html>`), "text/html", "auto", ""},
		{[]byte("<html><head><meta http-equiv=\"Content-Type\" content=\"text/html; charset=x-user-defined\"></head><body><p id=v>caf\xe9 \x80</p></body></html>"), "", "auto", ""},
		{utf16, "text/html; charset=utf-16le", "html", ""},
		{[]byte("<html><head><meta charset=\"windows-1252\"></head><body><table><tr><td class=\"who\">caf\xe9</td></tr></table></body></html>"), "text/html", "html", ""},
		{[]byte("<html><head><meta http-equiv=\"Content-Type\" content=\"text/html; charset=ISO-8859-1\"></head><body><table><tr><td class=\"who\">caf\xe9</td></tr></table></body></html>"), "text/html", "html", ""},
		{[]byte("<?xml version=\"1.0\" encoding=\"ISO-8859-1\"?><status><who>caf\xe9</who><v>4</v></status>"), "application/xml", "auto", ""},
		{[]byte("<?xml version=\"1.0\" encoding=\"ISO-8859-1\"?><status><who>caf\xe9</who><v>4</v></status>"), "application/xml; charset=iso-8859-1", "auto", ""},
		{[]byte("<?xml version=\"1.0\" encoding=\"windows-1251\"?><a>\xcf\xf0\xe8\xe2\xe5\xf2</a>"), "application/xml", "auto", ""},
		{[]byte("v=1 caf\xe9\n"), "text/plain; charset=ISO-8859-1", "text", ""},
		{[]byte("v=1 caf\xe9\n"), `text/plain; charset="latin1"`, "text", ""},
		{[]byte("v=1 caf\xc3\xa9\n"), "text/plain; charset=utf-8", "text", ""},
		{[]byte("v=1 x\n"), "text/plain; charset=klingon", "text", ""},
		{[]byte("{\"who\":\"caf\xe9\"}"), "application/json; charset=iso-8859-1", "auto", ""},
		{[]byte("abc"), "text/plain; charset=ISO-8859-2", "text", "koi8-r"},
		{[]byte{0xFF, 0xFE, 'a', 0}, "text/plain; charset=ISO-8859-2", "text", "koi8-r"},
		{[]byte{0xEF, 0xBB, 0xBF, 'a'}, "text/plain; charset=koi8-r", "text", ""},
		{[]byte(`<meta charset="windows-1251">`), "text/html", "html", ""},
		{[]byte(`<meta charset="windows-1251">`), "text/html; charset=koi8-r", "html", ""},
		{[]byte(`<meta charset="windows-1251">`), "text/plain", "text", ""},
	} {
		if old, got := e.read(oracleConvert), e.read(convertAsDecodeDoes); !old.same(got) {
			t.Errorf("%q %s:\nwas %s\nis  %s", e.body, e, old, got)
		}
	}
}

// The choices that changed, each with what was chosen and what is: the
// name as the document writes it, empty for a document that declares
// nothing. Whatever the choice, a byte order mark, response.charset and the
// charset of a Content-Type are read before the document, as they were, and
// then the same body comes out as did.
func TestTheChoicesThatChanged(t *testing.T) {
	pad := func(n int, meta string) string {
		return "<!--" + strings.Repeat("-", n-len("<!---->")) + "-->" + meta
	}
	compared := 0
	for _, test := range []struct{ why, head, was, is string }{
		// Bug G: what is no declaration.
		{"a meta in a comment", `<!-- <meta charset="koi8-r"> -->`, "koi8-r", ""},
		{"a meta in a comment before the page's own", `<!-- <meta http-equiv="Content-Type" content="text/html; charset=windows-1251"> -->` + "\n" + `<meta charset="utf-8">`, "windows-1251", "utf-8"},
		{"a comment that is not closed", `<!-- <meta charset="koi8-r">`, "koi8-r", ""},
		{"the content of a description", `<meta name="description" content="about charset=windows-1251 pages">`, "windows-1251", ""},
		{"the content of a refresh", `<meta http-equiv="refresh" content="0; url=/depots?charset=koi8-r">`, "koi8-r", ""},
		{"a content without http-equiv", `<meta content="text/html; charset=koi8-r">`, "koi8-r", ""},
		{"a content with another http-equiv", `<meta http-equiv="Content-Language" content="bg; charset=koi8-r"><meta charset="windows-1251">`, "koi8-r", "windows-1251"},
		{"the word in another attribute's name", `<meta data-charset="koi8-r">`, "koi8-r", ""},
		{"the word in another attribute's value", `<meta name="charset=koi8-r">`, "koi8-r", ""},
		{"an element whose name starts with meta", `<metadata charset="koi8-r">`, "koi8-r", ""},
		{"a meta in an attribute's value", `<link rel="alternate" title="<meta charset=koi8-r>">`, "koi8-r", ""},
		{"a meta in a processing instruction", `<?php echo '<meta charset=koi8-r>' ?>`, "koi8-r", ""},
		// A tag as the standard reads it.
		{"a meta after a > in a quoted value", `<meta name="a>b" charset="koi8-r">`, "", "koi8-r"},
		{"a charset naming nothing, before a content", `<meta charset="" http-equiv="Content-Type" content="text/html; charset=koi8-r">`, "koi8-r", ""},
		{"a second charset attribute", `<meta charset="" charset="koi8-r">`, "koi8-r", ""},
		{"a second http-equiv attribute", `<meta http-equiv="refresh" http-equiv="Content-Type" content="text/html; charset=koi8-r">`, "koi8-r", ""},
		{"a content whose quote is not closed", `<meta http-equiv="Content-Type" content='text/html; charset="koi8-r'>`, "koi8-r", ""},
		{"a script whose text opens a tag and a quote", `<script>if (a <b) t = "</script><meta charset="koi8-r"><p title="">`, "koi8-r", ""},
		// The name is the whole value, which then is no encoding's name,
		// and a meta naming none declares nothing.
		{"a bare value run into a self-closing slash", `<meta charset=koi8-r/>`, "koi8-r", ""},
		{"a value with a semicolon", `<meta charset="koi8-r;">`, "koi8-r", ""},
		{"a content with a comma after the name", `<meta http-equiv="Content-Type" content="text/html; charset=koi8-r,foo">`, "koi8-r", ""},
		{"a value with a blank in it", `<meta charset="koi8 r">`, "koi8", ""},
		{"a value of other characters", `<meta charset="koi8-r(cyrillic)">`, "koi8-r", ""},
		{"a bare value run into the next tag", `<meta charset=koi8-r<title>Depots</title>`, "koi8-r", ""},
		// A meta naming no encoding is passed over: it was the choice, and
		// failed the decode naming it.
		{"a name nobody knows", `<meta charset="klingon">`, "klingon", ""},
		{"a name nobody knows, in a content", `<meta http-equiv="Content-Type" content="text/html; charset=klingon">`, "klingon", ""},
		{"a name nobody knows before a meta that names one", `<meta charset="klingon"><meta charset="koi8-r">`, "klingon", "koi8-r"},
		{"a name nobody knows after an XML declaration", `<?xml version="1.0" encoding="windows-1251"?><html><head><meta charset="klingon"/>`, "klingon", "windows-1251"},
		// Cut off by the end of the head, or of the page.
		{"a quoted value the head cuts", pad(1024-len(`<meta charset="windows-125`), `<meta charset="windows-1251">`), "windows-125", ""},
		{"a quoted value whose quote is byte 1025", pad(1024-len(`<meta charset="windows-1251`), `<meta charset="windows-1251">`), "windows-1251", ""},
		{"a bare value the head cuts", pad(1024-len(`<meta charset=iso-8859-1`), `<meta charset=iso-8859-15>`), "iso-8859-1", ""},
		// Bug I: the XML declaration of a page decoded as HTML that has no
		// meta declaring an encoding.
		{"an XML declaration alone", `<?xml version="1.0" encoding="windows-1251"?><html><head>`, "", "windows-1251"},
		{"an XML declaration before a description", `<?xml version="1.0" encoding="windows-1251"?><html><head><meta name="description" content="charset=koi8-r"/>`, "koi8-r", "windows-1251"},
		{"an XML declaration naming UTF-16", `<?xml version="1.0" encoding="UTF-16"?><html><head>`, "", "utf-8"},
		{"an XML declaration naming UTF-8", `<?xml version="1.0" encoding="UTF-8"?><html><head>`, "", "UTF-8"},
	} {
		const rest = "</head><body><p id=v>\xd1\xee\xf4\xe8\xff</p></body></html>"
		body := []byte(test.head + rest)
		r := &fetch.HTTPResponse{Body: body}
		if old, got := oracleDocumentCharset(r, "html"), documentCharset(r, "html"); old != test.was || got != test.is || old == got {
			t.Errorf("%s: %s names %q, and named %q; want %q, and %q", test.why, test.head, got, old, test.is, test.was)
		}
		for _, e := range []encodingCase{
			{body, "text/html; charset=windows-1251", "html", ""},
			{body, "text/html; charset=utf-8", "auto", ""},
			{body, "text/html", "html", "windows-1251"},
			{body, "", "auto", "iso-8859-2"},
			{append([]byte("\xef\xbb\xbf"), body...), "text/html", "html", ""},
			// Read as something else than HTML.
			{body, "text/plain", "text", ""},
			{body, "application/json", "json", ""},
			{body, "text/csv", "auto", ""},
		} {
			compared++
			if old, got := e.read(oracleConvert), e.read(convertAsDecodeDoes); !old.same(got) {
				t.Errorf("%s: %s %s:\nwas %s\nis  %s", test.why, test.head, e, old, got)
			}
		}
	}
	// A page that ends in the middle of its meta.
	for head, was := range map[string]string{`<html><head><meta charset=koi8-r`: "koi8-r", `<html><head><meta charset="koi8-r`: "koi8-r", `<meta http-equiv="Content-Type" content="text/html; charset=koi8-r`: "koi8-r"} {
		r := &fetch.HTTPResponse{Body: []byte(head)}
		if old, got := oracleDocumentCharset(r, "html"), documentCharset(r, "html"); old != was || got != "" {
			t.Errorf("%s names %q, and named %q; want nothing, and %q", head, got, old, was)
		}
	}
	t.Logf("%d cases compared", compared)
}

// What a page decoded as HTML declares beside or before its meta is chosen
// as it was, where for a while it was not: a meta is believed over an XML
// declaration that says otherwise, names UTF-8 or names nothing known; an
// XML declaration that names nothing known, or that does not stand at the
// first byte of the document, is none, and fails nothing; and a meta naming
// an encoding is read after one that never was a declaration. The same body
// comes out under the same Content-Type, whatever else names the encoding.
func TestWhatAPageDeclaresBesideItsMetaIsChosenAsItWas(t *testing.T) {
	compared := 0
	for _, test := range []struct{ why, head, names string }{
		{"a meta after an XML declaration that says otherwise", `<?xml version="1.0" encoding="windows-1251"?><html><head><meta charset="koi8-r"/>`, "koi8-r"},
		{"a meta after an XML declaration naming UTF-8", `<?xml version="1.0" encoding="UTF-8"?>` + "\n" + `<html><head><meta http-equiv="Content-Type" content="text/html; charset=windows-1251">`, "windows-1251"},
		{"a meta naming UTF-8 after an XML declaration", `<?xml version="1.0" encoding="ISO-8859-1"?><html><head><meta charset="utf-8">`, "utf-8"},
		{"a meta after an XML declaration naming nothing known", `<?xml version="1.0" encoding="latin-1"?><html><head><meta charset="utf-8">`, "utf-8"},
		{"an XML declaration naming nothing known", `<?xml version="1.0" encoding="klingon"?><html><head>`, ""},
		{"an XML declaration after a blank", ` <?xml version="1.0" encoding="windows-1251"?><html><head>`, ""},
		{"an XML declaration after empty lines", "\n\n" + `<?xml version="1.0" encoding="windows-1251"?><html><head>`, ""},
		{"an XML declaration after a comment", `<!-- generated --><?xml version="1.0" encoding="windows-1251"?><html><head>`, ""},
		{"an XML declaration without an encoding", `<?xml version="1.0" standalone="yes"?><html><head>`, ""},
		{"a meta after one whose value starts with no name", `<meta charset="(koi8-r)"><meta charset="koi8-r">`, "koi8-r"},
	} {
		const rest = "</head><body><p id=v>\xd1\xee\xf4\xe8\xff</p></body></html>"
		body := []byte(test.head + rest)
		r := &fetch.HTTPResponse{Body: body}
		if old, got := oracleDocumentCharset(r, "html"), documentCharset(r, "html"); old != test.names || got != test.names {
			t.Errorf("%s: %s names %q, and named %q; want %q of both", test.why, test.head, got, old, test.names)
		}
		for _, contentType := range []string{"", "text/html", "application/xhtml+xml", "text/html; charset=windows-1251", "text/html; charset=utf-8", "text/plain", "application/json", "text/csv"} {
			for _, decoder := range []string{"auto", "html", "text", "json"} {
				for _, charset := range []string{"", "iso-8859-2"} {
					e := encodingCase{body, contentType, decoder, charset}
					compared++
					if old, got := e.read(oracleConvert), e.read(convertAsDecodeDoes); !old.same(got) {
						t.Errorf("%s: %s %s:\nwas %s\nis  %s", test.why, test.head, e, old, got)
					}
				}
			}
		}
		e := encodingCase{append([]byte("\xef\xbb\xbf"), body...), "text/html", "html", ""}
		compared++
		if old, got := e.read(oracleConvert), e.read(convertAsDecodeDoes); !old.same(got) {
			t.Errorf("%s, after a byte order mark: %s %s:\nwas %s\nis  %s", test.why, test.head, e, old, got)
		}
	}
	t.Logf("%d cases compared", compared)
}

// A Content-Type's charset is read as it was from every header that is well
// formed, in every form one is written in. From one that is not, the
// charset now read is the one the same header names without what is wrong
// with it: the body comes out as it does under that header, with or without
// a response.charset that is read before it, and the header keeps what was
// wrong with it, its charset saying utf-8 as the sound header's does. And a
// header that names no charset either way leaves everything as it was.
func TestTheCharsetOfAContentTypeIsReadAsItWas(t *testing.T) {
	body := []byte("<html><head><meta charset=\"koi8-r\"></head><body><p id=v>\xd1\xee\xf4\xe8\xff</p></body></html>")
	compared := 0
	for _, mediaType := range []string{"text/html", "TEXT/HTML", "application/xhtml+xml", "text/plain", "application/json", "text/csv", "x/y", "text"} {
		for _, parameters := range []string{
			"", ";", "; charset=%s", ";charset=%s", "; Charset=%s", `; charset="%s"`, ";  charset=%s  ", "; charset=%s;", "; a=b; charset=%s", "; charset=%s; a=b", `; a="b; c"; charset=%s`,
			"; version=0.0.4; charset=%s", "; boundary=x", "; charset*=utf-8''%s", "\t;\tcharset=%s",
		} {
			for _, name := range []string{"utf-8", "windows-1251", "WINDOWS-1251", "cp1251", "utf-16", "klingon"} {
				contentType := mediaType + strings.ReplaceAll(parameters, "%s", name)
				if _, _, err := mime.ParseMediaType(contentType); err != nil {
					t.Fatalf("%q is not well formed: %v", contentType, err)
				}
				if old, got := oracleDeclaredCharset(contentType), declaredCharset(contentType); old != got {
					t.Errorf("%q names %q, and named %q", contentType, got, old)
				}
				for _, decoder := range []string{"auto", "html", "text"} {
					for _, charset := range []string{"", "iso-8859-2"} {
						e := encodingCase{body, contentType, decoder, charset}
						compared++
						if old, got := e.read(oracleConvert), e.read(convertAsDecodeDoes); !old.same(got) {
							t.Errorf("%s:\nwas %s\nis  %s", e, old, got)
						}
					}
				}
			}
		}
	}
	// Bug H. sound is the header without what is wrong with it; empty, the
	// header names nothing, as it did, and nothing is changed.
	for _, test := range []struct{ contentType, sound, sees string }{
		{"text/html; charset=windows-1251; q", "text/html; charset=windows-1251", "text/html; charset=utf-8; q"},
		{"text/html; charset=windows-1251, text/html", "text/html; charset=windows-1251", "text/html; charset=utf-8, text/html"},
		{"text/html; charset=windows-1251, text/plain; charset=koi8-r", "text/html; charset=windows-1251", "text/html; charset=utf-8, text/plain; charset=koi8-r"},
		{`text/html; charset="windows-1251", text/html`, "text/html; charset=windows-1251", `text/html; charset="utf-8", text/html`},
		{"text/html; q; charset=windows-1251", "text/html; charset=windows-1251", "text/html; q; charset=utf-8"},
		{"text/html;;charset=windows-1251", "text/html; charset=windows-1251", "text/html;;charset=utf-8"},
		{"text/html ; charset = windows-1251 ; q", "text/html; charset=windows-1251", "text/html ; charset = utf-8 ; q"},
		{"text/html; charset=windows-1251; charset=koi8-r", "text/html; charset=windows-1251", "text/html; charset=utf-8; charset=koi8-r"},
		{`text/html; charset="windows-1251" x; q`, "text/html; charset=windows-1251", `text/html; charset="utf-8" x; q`},
		{"application/xhtml+xml; charset=utf-8; q", "application/xhtml+xml; charset=utf-8", "application/xhtml+xml; charset=utf-8; q"},
		{"text/html; charset=klingon; q", "text/html; charset=klingon", "text/html; charset=utf-8; q"},
		{"text/plain; version=0.0.4; charset=windows-1251; q", "text/plain; version=0.0.4; charset=windows-1251", "text/plain; version=0.0.4; charset=utf-8; q"},
		{"text/html; q", "", ""},
		{"text/html, text/html; charset=windows-1251", "", ""},
		{"text/html; charset=; q", "", ""},
		{"text/html; charset=windows-1251 x; q", "", ""},
		// A quote that nothing closes: for a while the rest of the header
		// was the name, read where it was an encoding's and a failure
		// where it was not.
		{`text/html; charset="windows-1251`, "", ""},
		{`text/html; charset="windows-1251; q`, "", ""},
		{`text/html; charset="; q`, "", ""},
		{`text/html; charset="klingon`, "", ""},
		{`text/html; charset=windows-1251"; q`, "", ""},
		{"text/html charset=windows-1251", "", ""},
		{"; charset=windows-1251", "", ""},
		{"charset=windows-1251", "", ""},
		{";;;", "", ""},
	} {
		if _, _, err := mime.ParseMediaType(test.contentType); err == nil {
			t.Fatalf("%q is well formed", test.contentType)
		}
		for _, decoder := range []string{"auto", "html", "text"} {
			for _, charset := range []string{"", "iso-8859-2"} {
				e := encodingCase{body, test.contentType, decoder, charset}
				old, got := e.read(oracleConvert), e.read(convertAsDecodeDoes)
				compared++
				if test.sound == "" {
					if !old.same(got) {
						t.Errorf("%s:\nwas %s\nis  %s", e, old, got)
					}
					continue
				}
				sound := encodingCase{body, test.sound, decoder, charset}.read(oracleConvert)
				if old.same(got) && test.contentType != test.sees || !got.sameBody(sound) || got.failure == "" && got.contentType != test.sees {
					t.Errorf("%s:\nwas %s\nis  %s\nwant, but for the Content-Type %q, what the sound header gave:\n    %s", e, old, got, test.sees, sound)
				}
			}
		}
	}
	t.Logf("%d cases compared", compared)
}

// A meta is looked for in a document that names its encoding no other way,
// once for each decode, in at most 1024 bytes: the prescan allocates
// nothing but the name it returns.
func TestThePrescanAllocatesOnlyTheNameItReturns(t *testing.T) {
	head := bytes.Repeat([]byte(`<link rel="stylesheet" href="/a.css?v=1" media="screen">`+"\n"), 16)
	declared := append(slices.Clone(head), `<meta http-equiv="Content-Type" content="text/html; charset=windows-1251">`...)
	if allocs := alloctest.AllocsAtMost(100, 0, func() { _ = metaDeclaredCharset(head) }); allocs != 0 {
		t.Errorf("a head without a meta: %v allocations, want none", allocs)
	}
	if allocs := alloctest.AllocsAtMost(100, 1, func() { _ = metaDeclaredCharset(declared) }); allocs > 1 {
		t.Errorf("a head with a meta: %v allocations, want one", allocs)
	}
}

// BenchmarkDocumentCharset measures the search of a head for its
// declaration, as it was and as it is: a head of a KiB without one, which
// is read to its end, and one whose meta is its first element.
func BenchmarkDocumentCharset(b *testing.B) {
	long := bytes.Repeat([]byte(`<link rel="stylesheet" href="/a.css?v=1" media="screen">`+"\n"), 40)
	short := []byte("<!DOCTYPE html>\n<html>\n<head>\n<meta charset=\"utf-8\">\n<title>Depots</title>\n</head>")
	for name, body := range map[string][]byte{"none": long, "first": short} {
		r := &fetch.HTTPResponse{Body: body}
		b.Run(name+"/was", func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_ = oracleDocumentCharset(r, "html")
			}
		})
		b.Run(name+"/is", func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_ = documentCharset(r, "html")
			}
		})
	}
}
