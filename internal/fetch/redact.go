package fetch

import (
	"net/url"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// Every place where the exporter shows data from a request withholds
// credentials through the helpers in this file. This covers a target in a
// log line or a label, a request URL in a self-metric, a debug probe's
// request and headers, and the start of an error response's body in a log
// line. Sharing the helpers keeps the rules from drifting apart:
//
//   - A URL's userinfo is never shown. It becomes redacted:redacted, or is
//     dropped with the rest where the URL is cut down (RedactURL).
//   - How a URL's query is shown depends on the place. In a metric label's
//     URL, which Prometheus keeps, it is dropped. In an error, every value
//     is masked. In a debug report's request lines, every value is masked
//     except those the collector's configuration set (RedactRequestURL).
//     Where a target is shown (a log line, the static targets endpoint's
//     target label, an OTLP target attribute), only the values of
//     parameters whose names read as credentials are masked, so targets
//     that differ in anything else stay distinct (URLRedaction). A shown
//     query is the query as it was sent, masked in place (maskQuery). A
//     query is sent as written, so it is not parsed and written again,
//     which would leave out the parts that do not parse.
//   - A URL's fragment is never shown. It is never sent to the target, so it
//     says nothing about the request, and a URL copied from a browser can
//     carry a token in it (#access_token=…).
//   - A header's value is withheld when its name reads as a credential
//     (CredentialName). For a header that carries a URL (Location,
//     Content-Location, Refresh, Link), the URL is masked the same way as in
//     a debug report's request line (RedactHeaderValue).
//   - Text the exporter did not write, such as an error page, has anything
//     that reads as a credential masked (RedactText), including a URL's
//     userinfo. The same name rule applies: a key or field whose name
//     CredentialName treats as a credential has its value masked.
//   - An error that quotes a URL, as Go's HTTP client quotes the URL it
//     requested, quotes it with its userinfo withheld and its query values
//     masked (RedactURLErrors).

// Redacted stands in for what is withheld.
const Redacted = "<redacted>"

// URLRedaction says what RedactURL does with a URL's query.
type URLRedaction int

const (
	// DropQuery drops the query, the fragment and the userinfo, for a metric
	// label.
	DropQuery URLRedaction = iota
	// MaskQueryValues keeps each query parameter's name, as written and in
	// place, and masks its value. It is for a debug report, where
	// request.query may carry a token under any name. The fragment is
	// dropped.
	MaskQueryValues
	// MaskCredentialQueryValues masks the values of query parameters whose
	// names read as credentials (CredentialName). It keeps the rest as they
	// are, in order, for a displayed target: ?token=… is withheld, while
	// ?tenant=a and ?tenant=b stay two targets. The fragment is dropped.
	MaskCredentialQueryValues
)

// redactedUser replaces a URL's userinfo where it is kept at all.
var redactedUser = url.UserPassword("redacted", "redacted")

// RedactURL renders u with its credentials withheld as how says. u itself is
// left alone. Whatever how says, the value of a path parameter whose name
// reads as a credential's is masked (maskPathParams).
func RedactURL(u *url.URL, how URLRedaction) string {
	shown := *u
	switch how {
	case DropQuery:
		shown.User = nil
		shown.RawQuery = ""
		shown.ForceQuery = false
		shown.Fragment = ""
		shown.RawFragment = ""
		return withMaskedPath(&shown)
	case MaskQueryValues:
		shown.RawQuery = maskQuery(u.RawQuery, true)
	case MaskCredentialQueryValues:
		shown.RawQuery = maskQuery(u.RawQuery, false)
	}
	shown.Fragment = ""
	shown.RawFragment = ""
	if shown.User != nil {
		shown.User = redactedUser
	}
	return withMaskedPath(&shown)
}

// withMaskedPath returns u as a string, with the values of path parameters
// whose names read as credentials masked (maskPathParams). The path is
// masked as written in the URL, with its escapes kept, and replaces u's own
// path. This way <redacted> appears as it does in a query, not escaped as a
// path would escape it.
func withMaskedPath(u *url.URL) string {
	if u.Opaque != "" || !strings.Contains(u.RawPath+u.Path, ";") {
		return u.String()
	}
	path := u.EscapedPath()
	masked := maskPathParams(path)
	if masked == path {
		return u.String()
	}
	pathless := *u
	pathless.Path, pathless.RawPath = "", ""
	whole := pathless.String()
	pathless.RawQuery, pathless.ForceQuery = "", false
	start := pathless.String()
	return start + masked + strings.TrimPrefix(whole, start)
}

// maskPathParams returns an escaped path in which the value of each path
// parameter whose name reads as a credential is masked in place, using the
// same rule as for a query parameter (credentialQueryName). For example,
// /app;jsessionid=SECRET becomes /app;jsessionid=<redacted>; a servlet
// container puts its session id in the URL's path when cookies are off.
// A parameter is the text after a ; in a segment, up to the next ; or /. It
// is name=value or a name alone. An escaped ; (%3B in either case) also
// separates parameters, because a server that decodes the path before
// looking for parameters reads /app%3Bjsessionid=SECRET as
// /app;jsessionid=SECRET. A segment that is entirely a secret, such as a
// bot's token in /bot123:AAE…/getMe, has no name to identify it, and is kept.
func maskPathParams(path string) string {
	if !strings.Contains(path, ";") && !strings.Contains(strings.ToUpper(path), "%3B") {
		return path
	}
	var b strings.Builder
	b.Grow(len(path))
	for i, segment := range strings.Split(path, "/") {
		if i > 0 {
			b.WriteByte('/')
		}
		for j := 0; segment != ""; j++ {
			piece, separator := segment, ""
			if end, size := pathParamSeparator(segment); end >= 0 {
				piece, separator = segment[:end], segment[end:end+size]
			}
			segment = segment[len(piece)+len(separator):]
			name, _, hasValue := strings.Cut(piece, "=")
			if j > 0 && hasValue && credentialPathName(name) {
				b.WriteString(name)
				b.WriteString("=" + Redacted)
			} else {
				b.WriteString(piece)
			}
			b.WriteString(separator)
		}
	}
	return b.String()
}

// pathParamSeparator is where the first ; of a segment of an escaped path
// stands, written as it is or escaped, %3B or %3b, and how long it is
// written; -1 for none.
func pathParamSeparator(segment string) (int, int) {
	for i := 0; i < len(segment); i++ {
		switch {
		case segment[i] == ';':
			return i, 1
		case segment[i] == '%' && i+2 < len(segment) && segment[i+1] == '3' && (segment[i+2] == 'B' || segment[i+2] == 'b'):
			return i, 3
		}
	}
	return -1, 0
}

// credentialPathName says whether a path parameter's name, as it is written
// in a path, reads as a credential's: decoded as a path is, and as it was
// written, as credentialQueryName reads a query's.
func credentialPathName(raw string) bool {
	if name, err := url.PathUnescape(raw); err == nil && name != raw && credentialQueryName(name) {
		return true
	}
	return credentialQueryName(raw)
}

// RedactURLString is RedactURL for a URL not yet parsed: one that does not
// parse is withheld whole, since its credentials cannot be found to redact.
func RedactURLString(raw string, how URLRedaction) string {
	u, err := url.Parse(raw)
	if err != nil {
		return Redacted
	}
	return RedactURL(u, how)
}

// maskQuery returns a raw query as it was sent, with values masked in place.
// With all set, every pair's value is masked. Otherwise only the values of
// pairs whose names read as credentials (CredentialName) are masked. The
// query is not parsed and written again. Every pair keeps its position and
// spelling, and a pair that would not parse (a malformed escape, or no = at
// all) is shown as written, so nothing that was sent is missing.
//
// Pairs are split at & and at ; alike. Servers disagree about ;, so the
// query is read both ways and masked by the more cautious reading:
//
//   - A pair after a ; is a separate pair, as a server that splits there
//     reads it. So the token of a=1;token=SECRET is masked.
//   - A name is also everything from the last & to the first = after it,
//     including any ;, as a server that splits only at & reads it. So the
//     values of token;id=SECRET, pass;word=SECRET and key;=SECRET are
//     masked, even though no piece between two ; reads as a credential pair.
//   - Once a value has been masked, everything after it up to the next & is
//     the rest of that value to a server that splits only at &. So the
//     values of later pairs there are masked too, and a piece without = is
//     masked whole: token=SE;CRET shows neither half.
//
// A name is compared both as a server reads it, with escapes decoded, and as
// written (credentialQueryName). So %74oken=SECRET is masked like
// token=SECRET. The name is shown as written.
func maskQuery(raw string, all bool) string {
	return maskQueryShowing(raw, all, nil)
}

// maskQueryShowing is maskQuery, except that with all set it shows the value
// of a pair that shown reports as set by the request's configuration
// (RedactRequestURL). This applies only when the pair stands alone between
// two & with no ; a server could split it at, its name does not read as a
// credential, and its escapes decode. shown is called with the name and value
// as a server reads them.
func maskQueryShowing(raw string, all bool, shown func(name, value string) bool) string {
	if raw == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(raw))
	// masked reports that a value has been masked since the last &.
	// wholeName reports that the name a server splitting only at & reads
	// there (ending at the first = after that &) is a credential. alone
	// reports that no ; lies between that & and the next.
	masked, wholeName, alone := false, credentialUpToEquals(raw), aloneUpToAmpersand(raw)
	for len(raw) > 0 {
		piece, separator := raw, ""
		if end := strings.IndexAny(raw, "&;"); end >= 0 {
			piece, separator = raw[:end], raw[end:end+1]
		}
		raw = raw[len(piece)+len(separator):]
		name, _, hasValue := strings.Cut(piece, "=")
		// With all, a value is masked unless the configuration set it.
		allMasked := all && (!alone || !configuredPair(name, strings.TrimPrefix(piece, name+"="), shown))
		switch {
		// The first piece with a = after an & holds the = that ends the
		// whole name, so wholeName is asked of that piece's value.
		case hasValue && (allMasked || masked || wholeName || credentialQueryName(name)):
			b.WriteString(name)
			b.WriteString("=" + Redacted)
			masked = true
		case !hasValue && masked && piece != "":
			b.WriteString(Redacted)
		default:
			b.WriteString(piece)
		}
		b.WriteString(separator)
		if separator == "&" {
			masked, wholeName, alone = false, credentialUpToEquals(raw), aloneUpToAmpersand(raw)
		}
	}
	return b.String()
}

// aloneUpToAmpersand says whether what a query holds up to its first & has
// no ;, which a server could split a pair at.
func aloneUpToAmpersand(raw string) bool {
	if end := strings.IndexByte(raw, '&'); end >= 0 {
		raw = raw[:end]
	}
	return !strings.Contains(raw, ";")
}

// configuredPair reports whether shown says that a pair, as written in the
// query, was set by the request's configuration. shown gets the name and
// value decoded as a server reads them. A pair whose escapes do not decode
// is not configured.
func configuredPair(name, value string, shown func(name, value string) bool) bool {
	if shown == nil {
		return false
	}
	n, err := url.QueryUnescape(name)
	if err != nil {
		return false
	}
	v, err := url.QueryUnescape(value)
	return err == nil && shown(n, v)
}

// RedactRequestURL returns a request's URL as a debug report's request line
// shows it. This is RedactURLString with MaskQueryValues, except that the
// values of query pairs that configured reports as set by the request's
// configuration (ConfiguredQuery) are shown as sent. A pair that came from a
// probe parameter, a placeholder or the target keeps its value masked, and
// so does a pair whose name reads as a credential. The userinfo is withheld
// and the fragment dropped, as always.
func RedactRequestURL(raw string, configured func(name, value string) bool) string {
	u, err := url.Parse(raw)
	if err != nil {
		return Redacted
	}
	shown := *u
	shown.RawQuery = maskQueryShowing(u.RawQuery, true, configured)
	shown.Fragment, shown.RawFragment = "", ""
	if shown.User != nil {
		shown.User = redactedUser
	}
	return withMaskedPath(&shown)
}

// RedactShownURLQuery takes a URL as $url has it (HTTPTrip.ShownURL), with
// its path parameters and query credentials already masked. It masks the
// query values as a debug report's request line does (RedactRequestURL);
// configured says which pairs are shown. The URL is not parsed again. A path
// containing <redacted>, which a path would escape, would lose its own
// escapes, and Go would rewrite it in its own escaping (/a;%6Asessionid as
// /a;jsessionid). The report's $url would then differ from the one the rules
// read. Only the text after the first ? is masked, since the path holds no
// unescaped ? of its own.
func RedactShownURLQuery(shown string, configured func(name, value string) bool) string {
	start, query, ok := strings.Cut(shown, "?")
	if !ok {
		return shown
	}
	return start + "?" + maskQueryShowing(query, true, configured)
}

// credentialUpToEquals says whether what a query holds before its first =,
// when that = comes before any &, reads as a credential's name: the name of
// the query's first pair to a server that splits pairs at & alone.
func credentialUpToEquals(raw string) bool {
	if end := strings.IndexByte(raw, '&'); end >= 0 {
		raw = raw[:end]
	}
	name, _, hasValue := strings.Cut(raw, "=")
	// A name without a ; is the name the piece itself is asked for.
	return hasValue && strings.Contains(name, ";") && credentialQueryName(name)
}

// credentialQueryName reports whether a query pair's name reads as a
// credential, either as a server reads it, with escapes decoded (queryName),
// or as written, when that differs. A server that gives up on a malformed
// escape reads the name as written, and %ZZ%eauth contains auth only when
// read that way.
func credentialQueryName(raw string) bool {
	name := queryName(raw)
	return CredentialName(name) || hasWholePart(name, queryCredentialParts) || name != raw && (CredentialName(raw) || hasWholePart(raw, queryCredentialParts))
}

// hasWholePart says whether one of parts, in any case, is a whole word of
// name, as nameParts splits it.
func hasWholePart(name string, parts []string) bool {
	for _, chunk := range strings.FieldsFunc(name, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		for _, part := range nameParts(chunk) {
			for _, credential := range parts {
				if strings.EqualFold(part, credential) {
					return true
				}
			}
		}
	}
	return false
}

// queryName returns the name of a query pair as a server reads it: escapes
// decoded and + turned into a space. A malformed escape stays as written and
// the rest is still decoded, so a name is recognised however it is spelled.
func queryName(raw string) string {
	if !strings.ContainsAny(raw, "%+") {
		return raw
	}
	if name, err := url.QueryUnescape(raw); err == nil {
		return name
	}
	var b strings.Builder
	for i := 0; i < len(raw); i++ {
		switch {
		case raw[i] == '+':
			b.WriteByte(' ')
		case raw[i] == '%' && i+2 < len(raw):
			if value, err := strconv.ParseUint(raw[i+1:i+3], 16, 8); err == nil {
				b.WriteByte(byte(value))
				i += 2
				continue
			}
			b.WriteByte('%')
		default:
			b.WriteByte(raw[i])
		}
	}
	return b.String()
}

// credentialWords are what a word of a credential-holding header, query
// parameter or field name looks like, anywhere in the word: Authorization,
// X-Api-Key, access_token, client_secret, X-Amz-Signature, and also
// run-together words such as apikey, accesstoken and sessionid.
var credentialWords = []string{"auth", "cookie", "token", "secret", "password", "passwd", "passphrase", "passcode", "key", "session", "signature", "credential", "jwt"}

// credentialParts mark a credential's name only as a whole word of it (the
// whole name, or a part between punctuation or at a change to upper case).
// Inside a longer word they mean something else: sig (an Azure SAS
// signature) is not design or signal, and pass is not bypass or compass.
// sid is a session id, hmac a request signature and otp a one-time password.
var credentialParts = []string{"sig", "pwd", "pw", "pass", "sid", "hmac", "otp"}

// queryCredentialParts mark a credential only as a whole word in the name of
// a query parameter or a path parameter (maskPathParams), and nowhere else.
// code is OAuth's authorization code, which the redirect back from a login
// carries in its query. ticket is a CAS service ticket, carried by the
// redirect back from CAS. In a header name, or a field or element name in
// text, these words name ordinary things, such as an error body's
// <code>401</code> or a helpdesk ticket, so CredentialName ignores them.
// Even as whole words they mask some query parameters that hold no secret,
// such as country_code or promo_code, because the rule errs towards masking.
var queryCredentialParts = []string{"code", "ticket"}

// ordinaryWords are words that contain a credential word but are not about
// credentials. A word of a name that is exactly one of them does not make
// the name a credential: an author: field of a feed or <author>Bob</author>
// is not masked, while Authorization, authToken and author_token still are,
// because none of their words is exactly one of these. Words are matched
// whole so that a credential word run together with one of them (authorkey)
// still reads as a credential.
var ordinaryWords = map[string]bool{
	"author": true, "authors": true, "authored": true, "authoring": true, "authorship": true, "coauthor": true, "coauthors": true,
	"authority": true, "authorities": true, "authoritative": true,
	"monkey": true, "donkey": true, "turkey": true, "hockey": true, "jockey": true, "whiskey": true,
	"keyboard": true, "keyboards": true, "keyword": true, "keywords": true, "keynote": true, "keystroke": true, "keystrokes": true,
	"tokenizer": true, "tokenizers": true, "tokenize": true, "tokenized": true, "tokenization": true,
}

// CredentialName reports whether a name (a header, a query parameter, or a
// field in text) reads as a credential, in any case: Authorization, Cookie,
// X-Api-Key, X-Auth-Token, apikey, sig, db_pwd, userPass and the like. The
// name is read word by word (nameParts). It reads as a credential when one
// of credentialParts is a whole word of it, or when a credential word
// appears anywhere in it after its ordinary words (ordinaryWords) are
// removed. A credential word is also matched across a change to upper case,
// since toKen is still token to a reader, but not across punctuation or an
// ordinary word. A name not split into words where it should be errs
// towards masking: authortoken holds token.
func CredentialName(name string) bool {
	// rest is the name in lower case with its punctuation and its ordinary
	// words each a NUL, which no credential word holds.
	var rest strings.Builder
	chunks := strings.FieldsFunc(name, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	for _, chunk := range chunks {
		for _, part := range nameParts(chunk) {
			lower := strings.ToLower(part)
			for _, credential := range credentialParts {
				if lower == credential {
					return true
				}
			}
			if ordinaryWords[lower] {
				lower = "\x00"
			}
			rest.WriteString(lower)
		}
		rest.WriteByte(0)
	}
	text := rest.String()
	for _, word := range credentialWords {
		if strings.Contains(text, word) {
			return true
		}
	}
	return false
}

// nameParts splits a name into its words: at every character that is not a
// letter or a digit, and where a lower-case letter or a digit is followed by
// an upper-case one, so X-Sig, sig_v2 and userPwd each have their part.
func nameParts(name string) []string {
	var parts []string
	start := -1
	prev := rune(0)
	for i, r := range name {
		alnum := unicode.IsLetter(r) || unicode.IsDigit(r)
		switch {
		case !alnum:
			if start >= 0 {
				parts = append(parts, name[start:i])
				start = -1
			}
		case start < 0:
			start = i
		case unicode.IsUpper(r) && (unicode.IsLower(prev) || unicode.IsDigit(prev)):
			parts = append(parts, name[start:i])
			start = i
		}
		prev = r
	}
	if start >= 0 {
		parts = append(parts, name[start:])
	}
	return parts
}

// RedactHeaderValue returns a header's value as it may be shown. It is
// Redacted when the name reads as a credential. Otherwise, for a header that
// carries a URL, the URL is masked as a debug report's request line masks
// one (RedactRequestURL): all of Location and Content-Location, the url= of
// Refresh, and each <…> of Link. A redirect's Location is where the request
// goes next, and it carries such things as a session in its path, or an
// OAuth code or a token in its query.
func RedactHeaderValue(name, value string) string {
	if CredentialName(name) {
		return Redacted
	}
	switch strings.ToLower(name) {
	case "location", "content-location":
		return RedactRequestURL(strings.TrimSpace(value), nil)
	case "refresh":
		if m := refreshURL.FindStringSubmatchIndex(value); m != nil {
			return value[:m[2]] + RedactRequestURL(value[m[2]:m[3]], nil) + value[m[3]:]
		}
	case "link":
		return linkURL.ReplaceAllStringFunc(value, func(target string) string {
			return "<" + RedactRequestURL(target[1:len(target)-1], nil) + ">"
		})
	}
	return value
}

// refreshURL is the URL of a Refresh header, 5; url=https://… or
// 0;'/next': what follows the delay and its separator, and url= if it is
// there, up to a closing quote or the end, in its group.
var refreshURL = regexp.MustCompile(`(?i)^\s*[0-9.]*\s*[;,]\s*(?:url\s*=\s*)?['"]?([^'"]*?)['"]?\s*$`)

// linkURL is a URL of a Link header, <https://…>; rel="next", its angle
// brackets included.
var linkURL = regexp.MustCompile(`<[^<>]*>`)

// textCredentials find what reads as a credential in text the exporter did
// not write whatever it is called, each with the part to keep in its first
// group.
var textCredentials = []struct {
	pattern *regexp.Regexp
	keep    string
}{
	// Authorization: Bearer abc…, Basic dXNl…
	{regexp.MustCompile(`(?i)\b(bearer|basic|digest)\s+[A-Za-z0-9._~+/=-]{8,}`), "${1} " + Redacted},
	// A JSON Web Token, wherever it stands.
	{regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]*`), Redacted},
	// A URL's userinfo, as in https://user:password@host: the userinfo is
	// all of the authority up to its last @, as url.Parse reads it, so a
	// password with an @ of its own is withheld whole.
	{regexp.MustCompile(`\b([A-Za-z][A-Za-z0-9+.-]*://)[^\s/?#"'<>]*@`), "${1}" + redactedUser.String() + "@"},
}

// textField matches a key or field name in text and what follows it up to
// its value: token=, "api_key": ", password: , and \"password\":\" in JSON
// written as a string inside JSON, with escaped quotes. Its second group is
// the quote that opens the value, if any.
var textField = regexp.MustCompile(`([A-Za-z0-9_.-]+)\\?["']?\s*[:=]\s*(\\?["']?)`)

// textValue is the value that follows a textField.
var textValue = regexp.MustCompile(`^[^\s"'&,;}<>]+`)

// escapedTextValue is the value that follows a textField whose quote is
// escaped, \": it ends at the escaped quote that closes it, and keeps an
// escaped character of its own, \\ or \n, as part of it.
var escapedTextValue = regexp.MustCompile(`^(?:\\[^"']|[^\s"'&,;}<>\\])+`)

// textElement matches an XML element whose content is only text or one
// CDATA section, such as <password>…</password> or
// <ns:apiKey a="1"><![CDATA[…]]></ns:apiKey>. Its groups are the name, the
// local name, the content and the closing name. The closing name must equal
// the name for a single element (RE2 has no backreference to check this).
var textElement = regexp.MustCompile(`(?s)<((?:[A-Za-z_][A-Za-z0-9_.-]*:)?([A-Za-z_][A-Za-z0-9_.-]*))(?:\s[^<>]*)?>(<!\[CDATA\[.*?\]\]>|[^<]*)</([A-Za-z_][A-Za-z0-9_.:-]*)\s*>`)

// redactElements masks the content of each XML element in text whose local
// name reads as a credential's (CredentialName): <password>hunter2</password>
// of a SOAP or XML login, which no name=value of textField finds.
func redactElements(text string) string {
	var b strings.Builder
	done := 0
	for _, m := range textElement.FindAllStringSubmatchIndex(text, -1) {
		if text[m[2]:m[3]] != text[m[8]:m[9]] || !CredentialName(text[m[4]:m[5]]) || strings.TrimSpace(text[m[6]:m[7]]) == "" {
			continue
		}
		b.WriteString(text[done:m[6]])
		b.WriteString(Redacted)
		done = m[7]
	}
	if done == 0 {
		return text
	}
	b.WriteString(text[done:])
	return b.String()
}

// RedactText masks what reads as a credential in text the exporter did not
// write, such as the start of an error page's body that it logs. It masks a
// bearer or basic credential, a JSON Web Token and a URL's userinfo. It also
// masks the value of a key, field or XML element whose name reads as a
// credential (CredentialName, the same rule as for header and query
// parameter names), and of a URL query or path parameter whose name reads as
// a credential under the query parameter rule (textCredential). It errs
// towards masking: a word that only looks like a credential's value may be
// masked too.
func RedactText(text string) string {
	for _, c := range textCredentials {
		text = c.pattern.ReplaceAllString(text, c.keep)
	}
	text = redactElements(text)
	var b strings.Builder
	done := 0
	for _, m := range textField.FindAllStringSubmatchIndex(text, -1) {
		// A field found inside a value already masked is part of it.
		if m[0] < done || !textCredential(text, m[2], m[3]) {
			continue
		}
		values := textValue
		if strings.HasPrefix(text[m[4]:m[5]], `\`) {
			values = escapedTextValue
		}
		value := values.FindStringIndex(text[m[1]:])
		if value == nil {
			continue
		}
		b.WriteString(text[done:m[1]])
		b.WriteString(Redacted)
		done = m[1] + value[1]
	}
	if done == 0 {
		return text
	}
	b.WriteString(text[done:])
	return b.String()
}

// textCredential reports whether the name text[start:end] of a textField
// reads as a credential. It uses CredentialName, or, when the name is a
// query or path parameter (right after a ?, & or ;, and right before a =),
// the query parameter rule (credentialQueryName), including code and ticket.
// So a URL in logged text, …/cb?code=…&state=x or …/x;ticket=…, has them
// masked as a URL shown anywhere else does. A JSON field or element named
// code, "code":401 or <code>401</code>, belongs to no URL and is kept.
func textCredential(text string, start, end int) bool {
	name := text[start:end]
	if CredentialName(name) {
		return true
	}
	return start > 0 && strings.IndexByte("?&;", text[start-1]) >= 0 && end < len(text) && text[end] == '=' && credentialQueryName(name)
}

// RedactURLErrors withholds the credentials of every URL quoted by a
// *url.Error in err's chain, as a debug report shows a URL (MaskQueryValues).
// Go's HTTP client quotes the whole URL it requested, query included, in
// the error of a failed request, and url.Parse quotes the text it could not
// parse. The errors that wrap one have already written its URL into their
// text, so the whole text is rewritten. The result reads with each URL
// redacted and unwraps to err, so errors.Is and errors.As still see what
// they did before. A nil err, or one that quotes no URL, is returned as is.
func RedactURLErrors(err error) error {
	if err == nil {
		return nil
	}
	var pairs []string
	walkErrors(err, func(e error) {
		urlErr, ok := e.(*url.Error) //nolint:errorlint // walkErrors visits each error of the chain itself
		if !ok {
			return
		}
		shown := RedactURLString(urlErr.URL, MaskQueryValues)
		if shown != urlErr.URL {
			pairs = append(pairs, strconv.Quote(urlErr.URL), strconv.Quote(shown), urlErr.URL, shown)
		}
	})
	if len(pairs) == 0 {
		return err
	}
	rewrite := strings.NewReplacer(pairs...)
	return &redactedError{text: rewrite.Replace(err.Error()), err: err, rewrite: rewrite}
}

// walkErrors calls visit with err and every error it wraps. An error that is
// a nil pointer of its type is not visited, nor asked what it wraps: there is
// nothing in it to read, and reading it panics.
func walkErrors(err error, visit func(error)) {
	if err == nil {
		return
	}
	if v := reflect.ValueOf(err); v.Kind() == reflect.Pointer && v.IsNil() {
		return
	}
	visit(err)
	switch wrapped := err.(type) { //nolint:errorlint // this is the walk down the chain
	case interface{ Unwrap() error }:
		walkErrors(wrapped.Unwrap(), visit)
	case interface{ Unwrap() []error }:
		for _, e := range wrapped.Unwrap() {
			walkErrors(e, visit)
		}
	}
}

// redactedError is an error whose text has its URLs' credentials withheld.
type redactedError struct {
	text string
	err  error
	// rewrite makes of a text of err's what text was made of err's.
	rewrite *strings.Replacer
}

func (e *redactedError) Error() string { return e.text }
func (e *redactedError) Unwrap() error { return e.err }

// RewriteText is text, what a probe's caller is told of the wrapped error,
// rewritten as the error's own text was (model.TextRewriter).
func (e *redactedError) RewriteText(text string) string { return e.rewrite.Replace(text) }
