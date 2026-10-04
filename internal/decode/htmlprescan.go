package decode

import "bytes"

// What an HTML page says of its own encoding is found as the WHATWG HTML
// standard finds it before it parses a page ("prescan a byte stream to
// determine its encoding"), since that is what the page's author tested it
// against, in a browser. A search for "charset=" anywhere in the head takes
// a meta in a comment, the description of a page about encodings and the
// query string of a refresh for declarations.
//
// The prescan reads tags, not text. A comment is passed over whole. A
// <meta> tag declares an encoding with a charset attribute, or with a
// content attribute holding "charset=" when the same tag has
// http-equiv="content-type"; a content attribute of any other meta declares
// nothing. Attribute names and http-equiv's value are read in any case, a
// value quoted either way or bare, with blanks around the equals sign, and
// the first attribute of a name is the one that counts. Any other tag, a
// closing tag, a doctype and a processing instruction are passed over with
// their attributes, so a "charset=" in a title's text or in another
// element's attribute is nothing; the text of a script is read as any other
// text is, as the standard reads it. The first meta that declares an
// encoding is the answer. A meta naming what is no encoding's name — a name
// nobody knows, the "utf-8/" a bare value run into the slash of a self-closed
// tag is, a name with a semicolon after it — declares nothing, as in the
// standard, and the next meta is looked at: a page a browser reads is not
// refused over it.
//
// The head is the first 1024 bytes, which the standard allows: a
// declaration that is not whole within them, an attribute's value cut off by
// their end, is none.

// metaDeclaredCharset is the encoding the first <meta> of head that names
// one declares, as the prescan takes the name (metaCharset), or empty when
// none does.
func metaDeclaredCharset(head []byte) string {
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
			// To the first > after two hyphens, which may be the comment's
			// own opening ones: <!--> is a whole comment.
			end := bytes.Index(rest[2:], []byte("-->"))
			if end < 0 {
				return ""
			}
			i += 2 + end + 2
		case len(rest) > 5 && bytes.EqualFold(rest[:5], []byte("<meta")) && (isPrescanSpace(rest[5]) || rest[5] == '/'):
			name, end := metaTagCharset(head, i+5)
			if name != "" {
				if known := metaCharset(name); known != "" {
					return known
				}
			}
			i = end
		case len(rest) > 1 && isASCIILetter(rest[1]), len(rest) > 2 && rest[1] == '/' && isASCIILetter(rest[2]):
			// Another tag: past its name, then past its attributes, whose
			// quoted values may hold a > or a <meta.
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

// hasMetaTag reports whether "<meta" stands anywhere in head, in any case.
// A head without it declares nothing, and need not be read tag by tag.
func hasMetaTag(head []byte) bool {
	for {
		next := bytes.IndexByte(head, '<')
		if next < 0 || len(head)-next < 5 {
			return false
		}
		if bytes.EqualFold(head[next+1:next+5], []byte("meta")) {
			return true
		}
		head = head[next+1:]
	}
}

// metaTagCharset reads the attributes of the <meta> tag whose name ends at
// at, and returns the encoding the tag declares, empty when it declares
// none, and where the tag ends.
func metaTagCharset(head []byte, at int) (string, int) {
	var (
		charset                              []byte
		pragma, needsPragma, declares        bool
		sawHTTPEquiv, sawContent, sawCharset bool
	)
	for {
		name, value, next, found := prescanAttribute(head, at)
		at = next
		if !found {
			break
		}
		switch {
		case bytes.EqualFold(name, []byte("http-equiv")):
			if !sawHTTPEquiv {
				sawHTTPEquiv = true
				pragma = bytes.EqualFold(value, []byte("content-type"))
			}
		case bytes.EqualFold(name, []byte("content")):
			if !sawContent {
				sawContent = true
				if label := bytes.TrimSpace(contentCharset(value)); len(label) > 0 && !declares {
					charset, needsPragma, declares = label, true, true
				}
			}
		case bytes.EqualFold(name, []byte("charset")):
			if !sawCharset {
				sawCharset = true
				// A charset attribute is the tag's word, over its content
				// and without http-equiv, even when it names nothing.
				charset, needsPragma, declares = bytes.TrimSpace(value), false, true
			}
		}
	}
	if !declares || needsPragma && !pragma || len(charset) == 0 {
		return "", at
	}
	return string(charset), at
}

// prescanAttribute reads the attribute at at of a tag, as the standard's
// "get an attribute" does: its name and value as written, where the next
// one starts, and whether there was one. There is none at the tag's > and
// none in what the end of head cuts off.
func prescanAttribute(head []byte, at int) (name, value []byte, next int, found bool) {
	for at < len(head) && (isPrescanSpace(head[at]) || head[at] == '/') {
		at++
	}
	if at >= len(head) || head[at] == '>' {
		return nil, nil, at, false
	}
	start := at
	for ; ; at++ {
		if at >= len(head) {
			return nil, nil, at, false
		}
		c := head[at]
		if c == '=' && at > start {
			name = head[start:at]
			at++
			break
		}
		if c == '/' || c == '>' {
			return head[start:at], nil, at, true
		}
		if isPrescanSpace(c) {
			name = head[start:at]
			for at < len(head) && isPrescanSpace(head[at]) {
				at++
			}
			if at >= len(head) {
				return nil, nil, at, false
			}
			if head[at] != '=' {
				return name, nil, at, true
			}
			at++
			break
		}
	}
	for at < len(head) && isPrescanSpace(head[at]) {
		at++
	}
	if at >= len(head) {
		return nil, nil, at, false
	}
	switch quote := head[at]; quote {
	case '"', '\'':
		end := bytes.IndexByte(head[at+1:], quote)
		if end < 0 {
			return nil, nil, len(head), false
		}
		return name, head[at+1 : at+1+end], at + end + 2, true
	case '>':
		return name, nil, at, true
	}
	start = at
	for ; at < len(head); at++ {
		if isPrescanSpace(head[at]) || head[at] == '>' {
			return name, head[start:at], at, true
		}
	}
	return nil, nil, at, false
}

// contentCharset is the encoding a meta's content attribute names, as the
// standard's "extracting a character encoding from a meta element" does:
// what follows the first "charset" that an equals sign follows, between
// quotes, or up to a blank or a semicolon.
func contentCharset(content []byte) []byte {
	for {
		at := -1
		for i := 0; i+7 <= len(content); i++ {
			if bytes.EqualFold(content[i:i+7], []byte("charset")) {
				at = i
				break
			}
		}
		if at < 0 {
			return nil
		}
		content = content[at+7:]
		rest := bytes.TrimLeft(content, "\t\n\f\r ")
		if len(rest) == 0 || rest[0] != '=' {
			continue
		}
		rest = bytes.TrimLeft(rest[1:], "\t\n\f\r ")
		if len(rest) == 0 {
			return nil
		}
		if quote := rest[0]; quote == '"' || quote == '\'' {
			end := bytes.IndexByte(rest[1:], quote)
			if end < 0 {
				return nil
			}
			return rest[1 : 1+end]
		}
		if end := bytes.IndexAny(rest, "\t\n\f\r ;"); end >= 0 {
			return rest[:end]
		}
		return rest
	}
}

// isPrescanSpace reports whether c is one of the standard's ASCII
// whitespace bytes.
func isPrescanSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\f' || c == '\r'
}

func isASCIILetter(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}
