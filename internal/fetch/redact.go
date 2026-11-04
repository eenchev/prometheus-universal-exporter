package fetch

import (
	"net/url"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// Every place the exporter shows something a request carried — a target in a
// log line or a label, a request URL in a self-metric, a debug probe's
// request and headers, the start of an error response's body in a log line —
// withholds credentials through the helpers here, so the rules cannot drift
// apart from one place to the next:
//
//   - A URL's userinfo is never shown: it becomes redacted:redacted, or goes
//     with the rest where the URL is cut down (RedactURL).
//   - What happens to a URL's query depends on where it is shown: dropped
//     from a metric label's URL, which Prometheus keeps; every value masked
//     in an error, and in a debug report's request lines every value but
//     those the collector's configuration set (RedactRequestURL); and,
//     where a target is shown — in a
//     log line, the static targets endpoint's target label, an OTLP target
//     attribute — the values of parameters whose names read as credentials
//     masked, so that targets that differ in anything else stay apart
//     (URLRedaction). Where a query is shown it is the query as it was
//     sent, masked where it stands (maskQuery): a query is sent as it was
//     written, so what is shown of it is not a query parsed and written
//     again, which would leave out what does not parse.
//   - A URL's fragment is never shown: it is never sent to the target, so it
//     tells nothing about the request, and a URL copied from a browser can
//     carry a token in it (#access_token=…).
//   - A header's value is withheld when its name reads as a credential's
//     (CredentialName).
//   - Text the exporter did not write, such as an error page, has what reads
//     as a credential in it masked (RedactText), a URL's userinfo among
//     it, by the same rule for a name: a key or field whose name
//     CredentialName says reads as a credential's has its value masked.
//   - An error that quotes a URL, as Go's HTTP client quotes the one it
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
	// MaskQueryValues keeps each query parameter's name, as it was written
	// and where it stood, and masks its value, for a debug report, where
	// request.query may carry a token under any name. The fragment is
	// dropped.
	MaskQueryValues
	// MaskCredentialQueryValues masks the values of the query parameters
	// whose names read as credentials (CredentialName), and keeps the rest
	// as they are, in their order, for a displayed target: ?token=… is
	// withheld, and ?tenant=a and ?tenant=b stay two targets. The fragment
	// is dropped.
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

// withMaskedPath is u as a string, the values of its path parameters whose
// names read as credentials' masked (maskPathParams). The path is masked as
// it is written in the URL, its escapes kept, and put in place of u's own,
// so <redacted> stands as it does in a query rather than escaped as a path
// would have it.
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

// maskPathParams is an escaped path with the value of each path parameter
// whose name reads as a credential's, as a query parameter's would
// (credentialQueryName), masked where it stands: /app;jsessionid=SECRET
// becomes /app;jsessionid=<redacted>, as a servlet container's session id
// rides in a URL's path when cookies are off. A parameter is what follows a
// ; in a segment, up to the next ; or /, name=value or a name alone. A
// segment that is a secret whole, as a bot's token in /bot123:AAE…/getMe,
// has no name to tell it by, and is kept.
func maskPathParams(path string) string {
	if !strings.Contains(path, ";") {
		return path
	}
	var b strings.Builder
	b.Grow(len(path))
	for i, segment := range strings.Split(path, "/") {
		if i > 0 {
			b.WriteByte('/')
		}
		for j, piece := range strings.Split(segment, ";") {
			if j > 0 {
				b.WriteByte(';')
			}
			name, _, hasValue := strings.Cut(piece, "=")
			if j > 0 && hasValue && credentialPathName(name) {
				b.WriteString(name)
				b.WriteString("=" + Redacted)
				continue
			}
			b.WriteString(piece)
		}
	}
	return b.String()
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

// maskQuery is a raw query as it was sent, with values masked where they
// stand: every pair's with all, and otherwise those of the pairs whose names
// read as credentials (CredentialName). The query is not parsed and written
// again: every pair keeps its place and its spelling, and one that would not
// parse — a malformed escape, no = at all — is shown as it was written, so
// nothing that was sent is missing from what is shown.
//
// Pairs are told apart at & and at ; alike. Servers disagree about ;, so
// the query is read both ways and masked by the more cautious of the two:
//
//   - a pair after a ; is a pair of its own, so the token of
//     a=1;token=SECRET is masked, as a server that splits there reads it;
//   - a name is also everything from the last & to the first = after it,
//     its ; included, as a server that splits at & alone reads it, so the
//     values of token;id=SECRET, pass;word=SECRET and key;=SECRET are
//     masked, though no piece between two ; reads as a credential's pair;
//   - once a value has been masked, what follows it up to the next & is the
//     rest of that value to a server that splits at & alone, so there the
//     values of later pairs are masked too, and a piece without = whole:
//     token=SE;CRET shows neither half.
//
// A name is compared as a server reads it, its escapes decoded, and as it
// was written too (credentialQueryName), so %74oken=SECRET is masked as
// token=SECRET is; it is shown as written.
func maskQuery(raw string, all bool) string {
	return maskQueryShowing(raw, all, nil)
}

// maskQueryShowing is maskQuery, but that with all set the value of a pair
// that shown says the request's configuration set (RedactRequestURL) is
// shown, when the pair stands between two & alone, with no ; that a server
// could split it at, its name reads as no credential's and its escapes
// decode: shown is asked of the name and the value as a server reads them.
func maskQueryShowing(raw string, all bool, shown func(name, value string) bool) string {
	if raw == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(raw))
	// masked says a value has been masked since the last &, and wholeName
	// that the name a server splitting at & alone reads there, which ends at
	// the first = after that &, is a credential's; alone, that no ; is
	// between that & and the next.
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

// configuredPair says whether shown says a pair, as it was written in the
// query, is one the request's configuration set: its name and its value
// decoded as a server reads them. A pair whose escapes do not decode is
// not.
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

// RedactRequestURL is a request's URL as a debug report's request line shows
// it: as RedactURLString with MaskQueryValues shows it, but for the values
// of the query pairs configured says the request's configuration set
// (ConfiguredQuery), which are shown as they were sent. A pair a probe
// parameter, a placeholder or the target gave keeps its value masked, and
// so does one whose name reads as a credential's; the userinfo is withheld
// and the fragment dropped as ever.
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

// credentialQueryName says whether a query pair's name reads as a
// credential's: as a server reads it, its escapes decoded (queryName), or as
// it was written, where that is something else. A server that gives up on a
// malformed escape reads the name as it was written, and %ZZ%eauth has auth
// in it only that way.
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

// queryName is the name of a query pair as a server reads it: its escapes
// decoded and a + a space. An escape that is malformed stays as it was
// written and the rest is decoded all the same, so a name is recognised
// however it is spelled.
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

// credentialWords are what a word of the name of a header, a query parameter
// or a field holding a credential reads like, anywhere in the word:
// Authorization, X-Api-Key, access_token, client_secret, X-Amz-Signature,
// and apikey, accesstoken and sessionid too, whose words are run together.
var credentialWords = []string{"auth", "cookie", "token", "secret", "password", "passwd", "passphrase", "passcode", "key", "session", "signature", "credential", "jwt"}

// credentialParts are what a credential's name reads like only as a whole
// word of it — the name itself, or a part between punctuation or at a
// change to upper case — since inside a longer word they are something
// else: sig (an Azure SAS signature) is not design or signal, and pass is
// not bypass or compass; sid is a session's id, hmac a request's
// signature and otp a one-time password.
var credentialParts = []string{"sig", "pwd", "pw", "pass", "sid", "hmac", "otp"}

// queryCredentialParts are what the name of a query parameter, or of a
// path parameter (maskPathParams), holding a credential reads like as a
// whole word of it, and nothing else's: code is OAuth's authorization
// code, which the redirect back from a login carries in its query, and
// ticket a CAS service ticket, which the redirect back from CAS does. As a
// header's name, or a field's or an element's in text, they name ordinary
// things, an error body's <code>401</code> or a helpdesk's ticket, so
// CredentialName leaves them alone. As a whole word they still mask some
// query parameters that hold no secret, a country_code or a promo_code, as
// the rule errs towards masking.
var queryCredentialParts = []string{"code", "ticket"}

// ordinaryWords are words that hold a credential word without being about a
// credential, so a name's word that is one of them, whole, does not read as
// a credential's: an author: field of a feed or <author>Bob</author> is not
// masked, while Authorization, authToken and author_token still are, their
// words not being one of these whole. A word is matched whole so that a
// credential word run together with one of them (authorkey) still reads as
// a credential's.
var ordinaryWords = map[string]bool{
	"author": true, "authors": true, "authored": true, "authoring": true, "authorship": true, "coauthor": true, "coauthors": true,
	"authority": true, "authorities": true, "authoritative": true,
	"monkey": true, "donkey": true, "turkey": true, "hockey": true, "jockey": true, "whiskey": true,
	"keyboard": true, "keyboards": true, "keyword": true, "keywords": true, "keynote": true, "keystroke": true, "keystrokes": true,
	"tokenizer": true, "tokenizers": true, "tokenize": true, "tokenized": true, "tokenization": true,
}

// CredentialName reports whether a name — a header's, a query parameter's,
// or a field's in text — reads as a credential's, in any case:
// Authorization, Cookie, X-Api-Key, X-Auth-Token, apikey, sig, db_pwd,
// userPass and the like. The name is read word by word (nameParts): it reads
// as a credential's when one of credentialParts is a word of it, whole, or
// when a credential word is anywhere in it once its ordinary words
// (ordinaryWords) are taken out. A credential word is looked for across a
// change to upper case too, since toKen is still token to a reader, but not
// across punctuation or an ordinary word. A name not split into words where
// it should be errs towards masking: authortoken holds token.
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

// RedactHeaderValue is a header's value as it may be shown: Redacted when the
// name reads as a credential's.
func RedactHeaderValue(name, value string) string {
	if CredentialName(name) {
		return Redacted
	}
	return value
}

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

// textField is a key or a field's name in text and what follows it up to
// its value: token=, "api_key": ", password: , and \"password\":\" of
// JSON written as a string in JSON, its quotes escaped. Its second group is
// the quote opening the value, if any.
var textField = regexp.MustCompile(`([A-Za-z0-9_.-]+)\\?["']?\s*[:=]\s*(\\?["']?)`)

// textValue is the value that follows a textField.
var textValue = regexp.MustCompile(`^[^\s"'&,;}<>]+`)

// escapedTextValue is the value that follows a textField whose quote is
// escaped, \": it ends at the escaped quote that closes it, and keeps an
// escaped character of its own, \\ or \n, as part of it.
var escapedTextValue = regexp.MustCompile(`^(?:\\[^"']|[^\s"'&,;}<>\\])+`)

// textElement is an XML element whose content is text alone or one CDATA
// section, <password>…</password> or <ns:apiKey a="1"><![CDATA[…]]></ns:apiKey>:
// its name, its local name, its content and the name it is closed with,
// which must be its name for it to be one element (RE2 has no
// backreference to say so).
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
// write, such as an error page's body it logs the start of: a bearer or
// basic credential, a JSON Web Token, a URL's userinfo, and the value of a
// key, a field or an XML element whose name reads as a credential's
// (CredentialName, the rule a header's and a query parameter's name are
// held to). It errs towards masking: a word that only looks like a
// credential's value may be masked too.
func RedactText(text string) string {
	for _, c := range textCredentials {
		text = c.pattern.ReplaceAllString(text, c.keep)
	}
	text = redactElements(text)
	var b strings.Builder
	done := 0
	for _, m := range textField.FindAllStringSubmatchIndex(text, -1) {
		// A field found inside a value already masked is part of it.
		if m[0] < done || !CredentialName(text[m[2]:m[3]]) {
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

// RedactURLErrors withholds the credentials of every URL quoted by a
// *url.Error in err's chain — Go's HTTP client quotes the whole URL it
// requested, query and all, in the error of a request that failed, and
// url.Parse quotes the text it could not parse — as a debug report shows a
// URL (MaskQueryValues). The errors that wrap one have already written its
// URL into their text, so the text is rewritten as a whole: the result reads
// with each URL redacted, and unwraps to err, so errors.Is and errors.As
// still see what they did. A nil err, or one quoting no URL, is returned as
// it is.
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
