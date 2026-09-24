package decode

import (
	"bytes"
	"fmt"
	"mime"
	"regexp"
	"strings"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/htmlindex"
	"golang.org/x/text/encoding/unicode"
)

// Everything the exporter does with a response — decoding, transforms, the
// exposition Prometheus reads — works in UTF-8, and Prometheus refuses a whole
// scrape over one label value that is not valid UTF-8. A target that answers
// in another encoding is converted first. The encoding is, in this order:
//
//  1. a byte order mark (UTF-8, UTF-16LE or UTF-16BE), which is removed;
//  2. the collector's response.charset, for a target that declares none, or
//     declares the wrong one, and for local files, which declare none;
//  3. the charset parameter of the Content-Type header;
//  4. for HTML, a <meta charset> or <meta http-equiv="Content-Type"> in the
//     first 1024 bytes; for XML, the encoding of the XML declaration.
//
// Names are the WHATWG Encoding Standard's, which browsers use: utf-8,
// iso-8859-1 (read, as browsers do, as windows-1252), windows-1251, koi8-r,
// shift_jis, gbk, euc-kr, utf-16le and the rest. An unknown name fails the
// decode stage naming it. The converted body is what decoders, transforms and
// Python scripts see, and its Content-Type says charset=utf-8. An XML
// declaration naming another encoding is rewritten to say UTF-8, so the XML
// parser does not convert it a second time.
//
// Whatever still is not valid UTF-8 after that — a target that says UTF-8 and
// is not — is replaced with U+FFFD in label values and help text after the
// transform (SanitizeUTF8), and counted, rather than failing the scrape.

var (
	metaCharsetRE = regexp.MustCompile(`(?i)<meta[^>]*?charset\s*=\s*["']?\s*([a-zA-Z0-9_:.+-]+)`)
	xmlEncodingRE = regexp.MustCompile(`^(\s*<\?xml[^>]*?encoding\s*=\s*["'])([^"']+)(["'])`)
)

var boms = []struct {
	mark []byte
	name string
}{
	{[]byte{0xEF, 0xBB, 0xBF}, "utf-8"},
	{[]byte{0xFF, 0xFE}, "utf-16le"},
	{[]byte{0xFE, 0xFF}, "utf-16be"},
}

// lookupCharset finds an encoding by its WHATWG name or label.
func lookupCharset(name string) (encoding.Encoding, string, error) {
	enc, err := htmlindex.Get(strings.TrimSpace(name))
	if err != nil {
		return nil, "", fmt.Errorf("unsupported charset %q; use a name from the WHATWG Encoding Standard, such as utf-8, windows-1252, iso-8859-2, windows-1251, shift_jis or gbk", name)
	}
	canonical, _ := htmlindex.Name(enc)
	return enc, canonical, nil
}

// CheckCharset validates response.charset when the configuration loads.
func CheckCharset(name string) error {
	if name == "" {
		return nil
	}
	_, _, err := lookupCharset(name)
	return err
}

// declaredCharset is the charset parameter of a Content-Type, if any.
func declaredCharset(contentType string) string {
	if contentType == "" {
		return ""
	}
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return ""
	}
	return params["charset"]
}

// ConvertToUTF8 converts r's body to UTF-8 by its byte order mark, the
// collector's response.charset or the Content-Type charset, before the format
// is detected. It reports whether one of them named the encoding.
func ConvertToUTF8(r *fetch.HTTPResponse, c *model.Collector) (bool, error) {
	for _, bom := range boms {
		if bytes.HasPrefix(r.Body, bom.mark) {
			body := r.Body[len(bom.mark):]
			if bom.name != "utf-8" {
				enc := unicode.UTF16(unicode.LittleEndian, unicode.IgnoreBOM)
				if bom.name == "utf-16be" {
					enc = unicode.UTF16(unicode.BigEndian, unicode.IgnoreBOM)
				}
				converted, err := enc.NewDecoder().Bytes(body)
				if err != nil {
					return true, fmt.Errorf("converting the body from %s: %w", bom.name, err)
				}
				body = converted
			}
			setBody(r, body)
			return true, nil
		}
	}
	name := c.Response.Charset
	if name == "" && r.Headers != nil {
		name = declaredCharset(r.Headers.Get("Content-Type"))
	}
	if name == "" {
		return false, nil
	}
	return true, convertFrom(r, name)
}

// convertFromDocument converts an HTML or XML body by the encoding the
// document declares in itself, when nothing outside it named one.
func convertFromDocument(r *fetch.HTTPResponse, kind string) error {
	head := r.Body[:min(len(r.Body), 1024)]
	switch kind {
	case "html":
		if m := metaCharsetRE.FindSubmatch(head); m != nil {
			return convertFrom(r, string(m[1]))
		}
	case "xml":
		if m := xmlEncodingRE.FindSubmatch(head); m != nil {
			return convertFrom(r, string(m[2]))
		}
	}
	return nil
}

// convertFrom converts r's body from the named encoding.
func convertFrom(r *fetch.HTTPResponse, name string) error {
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
	setBody(r, body)
	return nil
}

// setBody installs a UTF-8 body: the Content-Type says so, and an XML
// declaration naming another encoding is rewritten to UTF-8.
func setBody(r *fetch.HTTPResponse, body []byte) {
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
