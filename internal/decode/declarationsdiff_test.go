package decode

import (
	"bytes"
	"io/fs"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
)

// Three things are read otherwise than the commit before this one read
// them: a page decoded as HTML is read by its meta before its XML
// declaration, which is one only at the first byte of the document and only
// where it names an encoding; a meta naming no encoding is passed over; and
// a Content-Type charset whose quote is not closed names nothing
// (textencoding.go, htmlprescan.go). The functions as they were are kept
// here as oracles, and these tests show that every input outside those
// cases gives exactly what it gave.

// headContentTypeCharset is contentTypeCharset as it was.
func headContentTypeCharset(contentType string) (name string, start, end int) {
	first := contentType[:unquotedIndex(contentType, ',')]
	at := unquotedIndex(first, ';')
	kind, subtype, slash := strings.Cut(strings.Trim(first[:at], httpSpace), "/")
	if !slash || !isHTTPToken(kind) || !isHTTPToken(subtype) {
		return "", 0, 0
	}
	for at < len(first) {
		from := at + 1
		at = from + unquotedIndex(first[from:], ';')
		key, _, found := strings.Cut(first[from:at], "=")
		if !found || !strings.EqualFold(strings.Trim(key, httpSpace), "charset") {
			continue
		}
		start, end = from+len(key)+1, at
		for start < end && strings.IndexByte(httpSpace, first[start]) >= 0 {
			start++
		}
		for end > start && strings.IndexByte(httpSpace, first[end-1]) >= 0 {
			end--
		}
		if start < end && first[start] == '"' {
			start++
			if closing := strings.IndexByte(first[start:end], '"'); closing >= 0 {
				end = start + closing
			}
		} else if !isHTTPToken(first[start:end]) {
			return "", 0, 0
		}
		if start == end {
			return "", 0, 0
		}
		return first[start:end], start, end
	}
	return "", 0, 0
}

// headMetaDeclaredCharset is metaDeclaredCharset as it was: the first meta
// that declares anything is the answer, whatever it names.
func headMetaDeclaredCharset(head []byte) string {
	if !hasMetaTag(head) {
		return ""
	}
	for i := 0; i < len(head); i++ {
		next := bytes.IndexByte(head[i:], '<')
		if next < 0 {
			return ""
		}
		i += next
		rest := head[i:]
		switch {
		case bytes.HasPrefix(rest, []byte("<!--")):
			end := bytes.Index(rest[2:], []byte("-->"))
			if end < 0 {
				return ""
			}
			i += 2 + end + 2
		case len(rest) > 5 && bytes.EqualFold(rest[:5], []byte("<meta")) && (isPrescanSpace(rest[5]) || rest[5] == '/'):
			name, end := metaTagCharset(head, i+5)
			if name != "" {
				return name
			}
			i = end
		case len(rest) > 1 && isASCIILetter(rest[1]), len(rest) > 2 && rest[1] == '/' && isASCIILetter(rest[2]):
			at := i + 1
			for at < len(head) && !isPrescanSpace(head[at]) && head[at] != '>' {
				at++
			}
			for found := true; found; {
				_, _, at, found = prescanAttribute(head, at)
			}
			i = at
		case len(rest) > 1 && (rest[1] == '!' || rest[1] == '/' || rest[1] == '?'):
			end := bytes.IndexByte(rest, '>')
			if end < 0 {
				return ""
			}
			i += end
		}
	}
	return ""
}

// headDocumentCharset is documentCharset as it was, with metaCharset as it
// was (oracleMetaCharset): the XML declaration of an HTML page first, after
// any blanks, then its meta, and a name that is no encoding's is the answer.
func headDocumentCharset(r *fetch.HTTPResponse, kind string) string {
	head := r.Body[:min(len(r.Body), 1024)]
	switch kind {
	case "html":
		if bytes.HasPrefix(bytes.TrimLeft(head, " \t\r\n\f"), []byte("<?xml")) {
			if m := xmlEncodingRE.FindSubmatch(head); m != nil {
				return oracleMetaCharset(string(m[2]))
			}
		}
		if name := headMetaDeclaredCharset(head); name != "" {
			return oracleMetaCharset(name)
		}
	case "xml":
		if m := xmlEncodingRE.FindSubmatch(head); m != nil {
			return string(m[2])
		}
	}
	return ""
}

// A Content-Type charset is read as it was from every header but one whose
// charset value opens a quote that nothing closes, which names nothing:
// over the headers of the tests and of a review, a generated table, and
// headers put together at random from the pieces headers are made of.
func TestOnlyAnUnclosedQuoteChangesWhatAContentTypeNames(t *testing.T) {
	headers := []string{
		"", "text/html", "text/html; charset=windows-1251", "text/html;charset=windows-1251", `text/html; charset="windows-1251"`, "Text/HTML; Charset=Windows-1251",
		"text/html; charset=windows-1251;", "text/html;charset=windows-1251;;", `text/html; charset="windows-1251`, "; charset=windows-1251", "charset=windows-1251",
		"text/html charset=windows-1251", "text/html; q; charset=windows-1251", "text/html; charset=windows-1251; q", "text/html; charset=windows-1251; charset=koi8-r",
		"text/html; charset=windows-1251, text/html", "text/html, text/html; charset=windows-1251", "text/plain; charset=koi8-r, text/html; charset=windows-1251",
		"text/html; charset = windows-1251", `text/html; charset="windows\-1251"`, `text/html; charset="windows-1251"; q`, "text/html; charset='windows-1251'; q",
		"text/html; charset=windows-1251 ; q", "text/html; xcharset=koi8-r; charset=windows-1251; q", `text/html; x="; charset=koi8-r"; charset=windows-1251; q`,
		`multipart/form-data; boundary="charset=koi8-r"; charset=windows-1251; q`, "multipart/form-data; boundary=charset=koi8-r; charset=windows-1251",
		"text/html; charset=; q", "text/html; charset; q", "text/html; charset=bogus; q", "text/html; q", "text/html;", "text/html; ", "text/html;;", "text/",
		"/html; charset=windows-1251; q", "text/html/x; charset=windows-1251", "text/html; charset=windows-1251\x00; q", "text/html; charset=windows-1251\r\n; q",
		"text/html; charset=win dows; q", `text/html; charset=""; q`, `text/html; charset="; q`, `text/html; charset="`, `text/html; charset=" `, "text/html;charset=windows-1251,",
		",text/html;charset=windows-1251", `text/html; q="a,b"; charset=windows-1251; z`, "text/html; charset*=utf-8''windows-1251", `text/html; charset="windows-1251"x; q`,
		`text/html; charset=windows-1251"; q`, `text/html; charset="windows-1251; q`, `text/html; charset="windows-1251, text/html`, `text/html; charset="a\"b; q`,
		`text/html; charset="a\"b"; q`, `text/html; x="y; charset="koi8-r; q`, `text/html; charset= "koi8-r`, `text/html; charset="koi8-r" , text/css; charset="x`,
	}
	for _, mediaType := range []string{"text/html", "TEXT/HTML", "application/xhtml+xml", "text", "x/y "} {
		for _, parameters := range []string{
			"", ";", "; charset=%s", ";charset=%s", "; Charset=%s", `; charset="%s"`, `; charset="%s`, `; charset=%s"`, `; charset="%s; q`, `; charset="%s" ; q`, `; charset="%s"; x="y`,
			";  charset=%s  ", "; charset=%s;", "; a=b; charset=%s", "; charset=%s; a=b", `; a="b; c"; charset=%s`, "; q; charset=%s", "; charset=%s, text/css", `; charset="%s, text/css`,
			`; charset = "%s`, "\t;\tcharset=\"%s\t",
		} {
			for _, name := range []string{"utf-8", "windows-1251", "WINDOWS-1251", "klingon", "", " ", "a b"} {
				headers = append(headers, mediaType+strings.ReplaceAll(parameters, "%s", name))
			}
		}
	}
	pieces := []string{"text/html", "text", "/", ";", "; ", ",", "charset", "charset=", "=", `"`, `\"`, `\`, " ", "\t", "windows-1251", "koi8-r", "q", "x=y", "'", "utf-8", "a b"}
	random := rand.New(rand.NewSource(10))
	for range 20000 {
		var header strings.Builder
		header.WriteString([]string{"text/html", "text/html; ", "text/html; charset=", `text/html; charset="`, ""}[random.Intn(5)])
		for range random.Intn(8) {
			header.WriteString(pieces[random.Intn(len(pieces))])
		}
		headers = append(headers, header.String())
	}
	same, unclosed := 0, 0
	for _, header := range headers {
		was, wasStart, wasEnd := headContentTypeCharset(header)
		is, start, end := contentTypeCharset(header)
		// The value as it was read started after a quote, and no quote
		// stands where it ended.
		if was != "" && header[wasStart-1] == '"' && (wasEnd == len(header) || header[wasEnd] != '"') {
			unclosed++
			if is != "" || start != 0 || end != 0 || declaredCharset(header) != "" {
				t.Errorf("%q names %q at %d-%d, want nothing: its quote is not closed (it named %q)", header, is, start, end, was)
			}
			continue
		}
		same++
		if is != was || start != wasStart || end != wasEnd {
			t.Errorf("%q names %q at %d-%d, and named %q at %d-%d", header, is, start, end, was, wasStart, wasEnd)
		}
	}
	if same < 5000 || unclosed < 500 {
		t.Fatalf("%d headers read as they were and %d with a quote that is not closed", same, unclosed)
	}
	t.Logf("%d headers read as they were, %d with a quote that is not closed name nothing", same, unclosed)
}

// What a document declares in itself is found as it was in every file of
// testdata, read as HTML and as XML, but in the fixture whose meta and XML
// declaration disagree, which is read by its meta.
func TestWhatEveryTestdataFileDeclaresIsFoundAsItWas(t *testing.T) {
	const root = "../../testdata"
	compared, differ := 0, []string{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		r := &fetch.HTTPResponse{Body: body}
		for _, kind := range []string{"html", "xml", "json", "text"} {
			compared++
			if was, is := headDocumentCharset(r, kind), documentCharset(r, kind); was != is {
				differ = append(differ, filepath.ToSlash(path)+" as "+kind+": "+was+" then, "+is+" now")
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{root + "/html/charset/depots-meta-over-xmldecl.xhtml as html: koi8-r then, windows-1251 now"}; !equalStrings(differ, want) {
		t.Errorf("these differ:\n%s\nwant\n%s", strings.Join(differ, "\n"), strings.Join(want, "\n"))
	}
	t.Logf("%d readings compared", compared)
}

func equalStrings(a, b []string) bool {
	return strings.Join(a, "\n") == strings.Join(b, "\n") && len(a) == len(b)
}

// A page put together of what stands before its XML declaration, the
// declaration and two metas, in every combination: what it declares is
// what the parts say it should, then and now, and the two differ only where
// the page has an XML declaration naming an encoding beside a meta that
// names one, one that does not stand at the first byte, one naming no
// encoding, or a first meta naming none.
func TestOnlyTheDecidedCasesChangeWhatAPageDeclares(t *testing.T) {
	// named is what the part declares: nothing, an encoding as the
	// prescan takes it, or, with unknown set, a name that is no encoding's.
	type part struct {
		text, named string
		unknown     bool
	}
	before := []string{"", " ", "\n\n", "\t\r\n", "<!-- generated -->", "<!DOCTYPE html>", "x"}
	declarations := []part{
		{"", "", false},
		{`<?xml version="1.0"?>`, "", false},
		{`<?xml version="1.0" encoding="windows-1251"?>`, "windows-1251", false},
		{`<?xml version='1.0' encoding='KOI8-R' standalone='yes'?>`, "KOI8-R", false},
		{`<?xml version="1.0" encoding="UTF-8"?>`, "UTF-8", false},
		{`<?xml version="1.0" encoding="UTF-16"?>`, "utf-8", false},
		{`<?xml version="1.0" encoding="x-user-defined"?>`, "windows-1252", false},
		{`<?xml version="1.0" encoding="klingon"?>`, "klingon", true},
		{`<?xml version="1.0" encoding="latin-1"?>`, "latin-1", true},
	}
	metas := []part{
		{"", "", false},
		{`<meta charset="koi8-r">`, "koi8-r", false},
		{`<meta http-equiv="Content-Type" content="text/html; charset=Shift_JIS" />`, "Shift_JIS", false},
		{`<meta charset=utf-16le>`, "utf-8", false},
		{`<meta charset="utf-8">`, "utf-8", false},
		{`<meta name="description" content="charset=koi8-r">`, "", false},
		{`<!-- <meta charset="koi8-r"> -->`, "", false},
		{`<meta charset="">`, "", false},
		{`<title>charset=koi8-r</title>`, "", false},
		{`<meta charset="klingon">`, "klingon", true},
		{`<meta charset=utf-8/>`, "utf-8/", true},
		{`<meta charset="windows-1251;">`, "windows-1251;", true},
		{`<meta http-equiv="Content-Type" content="text/html; charset=windows-1251,foo">`, "windows-1251,foo", true},
		{`<meta charset=windows-1251<title>t</title>`, "windows-1251<title", true},
	}
	same, changed := 0, 0
	for _, prefix := range before {
		for _, declaration := range declarations {
			for _, first := range metas {
				for _, second := range metas {
					body := prefix + declaration.text + "<html><head>" + first.text + second.text + "</head><body><p>\xd1\xee\xf4\xe8\xff</p></body></html>"
					r := &fetch.HTTPResponse{Body: []byte(body)}
					blank := strings.TrimLeft(prefix, " \t\r\n\f") == ""
					// As it was: the declaration after any blanks, then
					// the first meta that declares anything.
					was, decided := "", false
					switch {
					case blank && declaration.named != "":
						was = declaration.named
						// The decided cases: a meta naming an encoding
						// is believed over it, it is none after blanks,
						// and none when it names no encoding.
						decided = prefix != "" || declaration.unknown || first.named != "" && !first.unknown || second.named != "" && !second.unknown
					case first.named != "":
						was, decided = first.named, first.unknown
					case second.named != "":
						was, decided = second.named, second.unknown
					}
					// As it is: the first meta naming an encoding, then
					// the declaration at the first byte, if it names one.
					is := ""
					switch {
					case first.named != "" && !first.unknown:
						is = first.named
					case second.named != "" && !second.unknown:
						is = second.named
					case prefix == "" && !declaration.unknown:
						is = declaration.named
					}
					if got := headDocumentCharset(r, "html"); got != was {
						t.Errorf("%q declared %q, want %q", body, got, was)
					}
					if got := documentCharset(r, "html"); got != is {
						t.Errorf("%q declares %q, want %q", body, got, is)
					}
					if decided {
						changed++
						continue
					}
					same++
					if was != is {
						t.Errorf("%q declares %q and declared %q, and is none of the decided cases", body, is, was)
					}
					// Read as XML nothing has changed.
					if a, b := headDocumentCharset(r, "xml"), documentCharset(r, "xml"); a != b {
						t.Errorf("%q as XML declares %q, and declared %q", body, b, a)
					}
				}
			}
		}
	}
	if same < 3000 || changed < 3000 {
		t.Fatalf("%d pages outside the decided cases and %d within them", same, changed)
	}
	t.Logf("%d pages declare what they declared, %d are of the decided cases", same, changed)
}
