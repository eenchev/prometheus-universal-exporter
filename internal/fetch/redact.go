package fetch

import (
	"net/url"
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
//     in a debug report and an error; and, where a target is shown — in a
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
//     as a credential in it masked (RedactText), by the same rule for a
//     name: a key or field whose name CredentialName says reads as a
//     credential's has its value masked.
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
// left alone.
func RedactURL(u *url.URL, how URLRedaction) string {
	shown := *u
	switch how {
	case DropQuery:
		shown.User = nil
		shown.RawQuery = ""
		shown.ForceQuery = false
		shown.Fragment = ""
		shown.RawFragment = ""
		return shown.String()
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
	return shown.String()
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
//   - once a value has been masked, what follows it up to the next & is the
//     rest of that value to a server that splits at & alone, so there the
//     values of later pairs are masked too, and a piece without = whole:
//     token=SE;CRET shows neither half.
//
// A name is compared as a server reads it, its escapes decoded (queryName),
// so %74oken=SECRET is masked as token=SECRET is; it is shown as written.
func maskQuery(raw string, all bool) string {
	if raw == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(raw))
	// masked says a value has been masked since the last &.
	masked := false
	for len(raw) > 0 {
		piece, separator := raw, ""
		if end := strings.IndexAny(raw, "&;"); end >= 0 {
			piece, separator = raw[:end], raw[end:end+1]
		}
		raw = raw[len(piece)+len(separator):]
		name, _, hasValue := strings.Cut(piece, "=")
		switch {
		case hasValue && (all || masked || CredentialName(queryName(name))):
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
			masked = false
		}
	}
	return b.String()
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

// credentialWords are what the name of a header, a query parameter or a
// field holding a credential reads like, anywhere in it: Authorization,
// X-Api-Key, access_token, client_secret, X-Amz-Signature.
var credentialWords = []string{"auth", "cookie", "token", "secret", "password", "passwd", "passphrase", "passcode", "key", "session", "signature", "credential", "jwt"}

// credentialParts are what a credential's name reads like only as a whole
// word of it — the name itself, or a part between punctuation or at a
// change to upper case — since inside a longer word they are something
// else: sig (an Azure SAS signature) is not design or signal, and pass is
// not bypass or compass.
var credentialParts = []string{"sig", "pwd", "pw", "pass"}

// CredentialName reports whether a name — a header's, a query parameter's,
// or a field's in text — reads as a credential's, in any case:
// Authorization, Cookie, X-Api-Key, X-Auth-Token, sig, db_pwd, userPass and
// the like.
func CredentialName(name string) bool {
	lower := strings.ToLower(name)
	for _, word := range credentialWords {
		if strings.Contains(lower, word) {
			return true
		}
	}
	for _, part := range nameParts(name) {
		for _, credential := range credentialParts {
			if strings.EqualFold(part, credential) {
				return true
			}
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
}

// textField is a key or a field's name in text and what follows it up to
// its value: token=, "api_key": ", password: .
var textField = regexp.MustCompile(`([A-Za-z0-9_.-]+)["']?\s*[:=]\s*["']?`)

// textValue is the value that follows a textField.
var textValue = regexp.MustCompile(`^[^\s"'&,;}<>]+`)

// RedactText masks what reads as a credential in text the exporter did not
// write, such as an error page's body it logs the start of: a bearer or
// basic credential, a JSON Web Token, and the value of a key or field whose
// name reads as a credential's (CredentialName, the rule a header's and a
// query parameter's name are held to). It errs towards masking: a word that
// only looks like a credential's value may be masked too.
func RedactText(text string) string {
	for _, c := range textCredentials {
		text = c.pattern.ReplaceAllString(text, c.keep)
	}
	var b strings.Builder
	done := 0
	for _, m := range textField.FindAllStringSubmatchIndex(text, -1) {
		// A field found inside a value already masked is part of it.
		if m[0] < done || !CredentialName(text[m[2]:m[3]]) {
			continue
		}
		value := textValue.FindStringIndex(text[m[1]:])
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
	return &redactedError{text: strings.NewReplacer(pairs...).Replace(err.Error()), err: err}
}

// walkErrors calls visit with err and every error it wraps.
func walkErrors(err error, visit func(error)) {
	if err == nil {
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
}

func (e *redactedError) Error() string { return e.text }
func (e *redactedError) Unwrap() error { return e.err }
