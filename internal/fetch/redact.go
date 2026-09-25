package fetch

import (
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
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
//     (URLRedaction).
//   - A header's value is withheld when its name reads as a credential's
//     (CredentialName).
//   - Text the exporter did not write, such as an error page, has what reads
//     as a credential in it masked (RedactText).
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
	// MaskQueryValues keeps each query parameter's name and masks its value,
	// for a debug report, where request.query may carry a token under any
	// name.
	MaskQueryValues
	// MaskCredentialQueryValues masks the values of the query parameters
	// whose names read as credentials (CredentialName), and keeps the rest
	// as they are, in their order, for a displayed target: ?token=… is
	// withheld, and ?tenant=a and ?tenant=b stay two targets.
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
		shown.RawQuery = maskQueryValues(u.Query())
	case MaskCredentialQueryValues:
		shown.RawQuery = maskCredentialQueryValues(u.RawQuery)
	}
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

// maskQueryValues is a query with every value masked, sorted by name.
func maskQueryValues(query url.Values) string {
	if len(query) == 0 {
		return ""
	}
	var b strings.Builder
	names := make([]string, 0, len(query))
	for name := range query {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		for range query[name] {
			if b.Len() > 0 {
				b.WriteByte('&')
			}
			b.WriteString(url.QueryEscape(name))
			b.WriteString("=" + Redacted)
		}
	}
	return b.String()
}

// maskCredentialQueryValues is a raw query with the values of the
// parameters whose names read as credentials masked, the rest, and the
// order, as they were.
func maskCredentialQueryValues(raw string) string {
	if raw == "" {
		return ""
	}
	parts := strings.Split(raw, "&")
	for i, part := range parts {
		name, _, _ := strings.Cut(part, "=")
		unescaped, err := url.QueryUnescape(name)
		if err != nil {
			unescaped = name
		}
		if CredentialName(unescaped) {
			parts[i] = name + "=" + Redacted
		}
	}
	return strings.Join(parts, "&")
}

// credentialWords are what the name of a header holding a credential reads
// like.
var credentialWords = []string{"auth", "cookie", "token", "secret", "password", "passwd", "key", "session", "signature", "credential"}

// CredentialName reports whether a header's name reads as a credential's,
// in any case: Authorization, Cookie, X-Api-Key, X-Auth-Token and the like.
func CredentialName(name string) bool {
	lower := strings.ToLower(name)
	for _, word := range credentialWords {
		if strings.Contains(lower, word) {
			return true
		}
	}
	return false
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
// not write, each with the part to keep in its first group.
var textCredentials = []struct {
	pattern *regexp.Regexp
	keep    string
}{
	// Authorization: Bearer abc…, Basic dXNl…
	{regexp.MustCompile(`(?i)\b(bearer|basic|digest)\s+[A-Za-z0-9._~+/=-]{8,}`), "${1} " + Redacted},
	// token=abc, "api_key": "abc", password: abc, client_secret=abc
	{regexp.MustCompile(`(?i)([A-Za-z0-9_.-]*(?:token|secret|passw(?:or)?d|pwd|api[_-]?key|access[_-]?key|private[_-]?key|session|signature|credential|auth)[A-Za-z0-9_.-]*["']?\s*[:=]\s*["']?)[^\s"'&,;}<>]+`), "${1}" + Redacted},
	// A JSON Web Token, wherever it stands.
	{regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]*`), Redacted},
}

// RedactText masks what reads as a credential in text the exporter did not
// write, such as an error page's body it logs the start of: a bearer or
// basic credential, the value of a key or field whose name reads as a
// credential's, and a JSON Web Token. It errs towards masking: a word that
// only looks like a credential's value may be masked too.
func RedactText(text string) string {
	for _, c := range textCredentials {
		text = c.pattern.ReplaceAllString(text, c.keep)
	}
	return text
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
