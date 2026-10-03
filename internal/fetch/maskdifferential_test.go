package fetch

import (
	"net/url"
	"sort"
	"strings"
	"testing"
)

// formerMaskQueryValues is how a debug report and an error masked a query
// before queries were shown as they were sent: parsed, sorted and written
// again, every value masked. It is kept here as an oracle.
func formerMaskQueryValues(query url.Values) string {
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

// formerMaskCredentialQueryValues is how a displayed target masked a query
// then: split at & alone, the name everything before the first =. It is
// kept here as an oracle.
func formerMaskCredentialQueryValues(raw string) string {
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

// splitOnlyMaskQuery is maskQuery as it first was: pairs told apart at & and
// at ; alike, and a name only ever the piece before its =. It is kept here
// as an oracle.
func splitOnlyMaskQuery(raw string, all bool) string {
	if raw == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(raw))
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

// maskDifferentialQueries are queries with the value S3CRET under names
// spelled, escaped and separated in every way that has mattered so far.
var maskDifferentialQueries = []string{
	"token=S3CRET", "TOKEN=S3CRET", "Token=S3CRET", "access_token=S3CRET", "api_key=S3CRET", "apikey=S3CRET", "X-Api-Key=S3CRET",
	"password=S3CRET", "passwd=S3CRET", "pwd=S3CRET", "pw=S3CRET", "pass=S3CRET", "sig=S3CRET", "signature=S3CRET", "X-Amz-Signature=S3CRET",
	"secret=S3CRET", "client_secret=S3CRET", "auth=S3CRET", "authorization=S3CRET", "cookie=S3CRET", "session=S3CRET", "sessionid=S3CRET", "jwt=S3CRET",
	"credential=S3CRET", "X-Amz-Credential=S3CRET", "passphrase=S3CRET", "passcode=S3CRET", "userPass=S3CRET", "db_pwd=S3CRET",
	"%74oken=S3CRET", "to%6ben=S3CRET", "t%6Fken=S3CRET", "token%20=S3CRET", "+token=S3CRET", "token+=S3CRET", "api%5Fkey=S3CRET", "api+key=S3CRET",
	"a=1&token=S3CRET", "token=S3CRET&a=1", "a=1;token=S3CRET", "token=S3CRET;a=1", "token=S3;CRET", "a=1;b=2&token=S3CRET;c=3", "a=1&b;token=S3CRET",
	"token=", "token", "token&a=1", "a&token=S3CRET", "token==S3CRET", "=S3CRET", "=token=S3CRET", "&&token=S3CRET&&", ";;token=S3CRET;;",
	"token=S3CRET&token=S3CRET2", "a=1&a=2&a=3", "a=b+c&token=S3+CRET", "token=%FF%FES3CRET", "to%ZZken=S3CRET", "tok%en=S3CRET", "token=S3CRET%", "x=100%&token=S3CRET",
	"a=1?token=S3CRET", "a=1&?token=S3CRET", "next=http://x/?token=S3CRET", "next=http%3A%2F%2Fx%2F%3Ftoken%3DS3CRET", "q=token=S3CRET", "q=x&token=" + strings.Repeat("S3CRET", 2000),
	"pass;word=S3CRET", "token;id=S3CRET", "session;x=S3CRET", "api;version=S3CRET", "a=1;pass;word=S3CRET", "key;=S3CRET", "auth;user=S3CRET",
	"a%3Btoken=S3CRET", "a%26token=S3CRET", "a%3Dtoken=S3CRET", "token%3DS3CRET", "token%3D=S3CRET", "a=1%26token=S3CRET", "a=1%3Btoken=S3CRET",
	"token[]=S3CRET", "user[token]=S3CRET", "user.password=S3CRET", "t\u043eken=S3CRET", "tok\u00aden=S3CRET", "\uff54\uff4f\uff4b\uff45\uff4e=S3CRET", "%EF%BD%94oken=S3CRET",
	"tok%00en=S3CRET", "%00token=S3CRET", "token%00=S3CRET", "TOK%45N=S3CRET", "to%256Ben=S3CRET", "token =S3CRET", " token=S3CRET",
	"a=1&\ttoken=S3CRET", "a=1&%09token=S3CRET", "a=1&%0Atoken=S3CRET", "a=1&%20token=S3CRET", "a=1& token=S3CRET",
	// A name with a ; in it, in every place and spelling.
	"a=1&token;id=S3CRET&b=2", "a&pass;word=S3CRET", "x;y;token;z=S3CRET", "token;a;b=S3CRET;c=S3CRET", "%74oken;id=S3CRET", "TOKEN;ID=S3CRET",
	"id;token=S3CRET", "to;ken=S3CRET", ";token=S3CRET", "token;=S3CRET", "pw;=S3CRET&pw;x", "token;id", "token;S3CRET", "a;b=1&key;c=S3CRET",
	"token%3Bid=S3CRET", "api;key=S3CRET", "a=1;key;=S3CRET", "sig;%ZZ=S3CRET", "%ZZ;sig=S3CRET",
	// A name that reads as a credential's only as it was written.
	"%ZZ%eauth=S3CRET", "%ZZ%eauth;x=S3CRET", "x;%ZZ%eauth=S3CRET",
}

// A credential under a name with a ; in it was shown: once pairs were told
// apart at ; as well as at &, so that the token of a=1;token=SECRET was
// masked, a name was only ever the piece between two separators, and
// token;id=SECRET, pass;word=SECRET and key;=SECRET — whose names a server
// that splits at & alone reads whole, and which had been masked until then
// — showed their values in logs. A name is now read both ways and the value
// masked if either is a credential's, and a name with a malformed escape is
// compared as it was written too, as it used to be.
//
// The two earlier maskings are the oracles: over every query here, a value
// that either of them withheld is withheld, and what is shown differs from
// the later one's only for the queries listed, each masked where it was not.
func TestNoQueryValueThatWasMaskedIsShown(t *testing.T) {
	// The displayed target of each query that is shown otherwise than
	// splitOnlyMaskQuery showed it.
	maskedAgain := map[string]string{
		"pass;word=S3CRET":          "pass;word=<redacted>",
		"token;id=S3CRET":           "token;id=<redacted>",
		"session;x=S3CRET":          "session;x=<redacted>",
		"key;=S3CRET":               "key;=<redacted>",
		"auth;user=S3CRET":          "auth;user=<redacted>",
		"a=1&token;id=S3CRET&b=2":   "a=1&token;id=<redacted>&b=2",
		"a&pass;word=S3CRET":        "a&pass;word=<redacted>",
		"x;y;token;z=S3CRET":        "x;y;token;z=<redacted>",
		"token;a;b=S3CRET;c=S3CRET": "token;a;b=<redacted>;c=<redacted>",
		"%74oken;id=S3CRET":         "%74oken;id=<redacted>",
		"TOKEN;ID=S3CRET":           "TOKEN;ID=<redacted>",
		"token;=S3CRET":             "token;=<redacted>",
		"pw;=S3CRET&pw;x":           "pw;=<redacted>&pw;x",
		"a;b=1&key;c=S3CRET":        "a;b=1&key;c=<redacted>",
		"sig;%ZZ=S3CRET":            "sig;%ZZ=<redacted>",
		"%ZZ%eauth=S3CRET":          "%ZZ%eauth=<redacted>",
		"%ZZ%eauth;x=S3CRET":        "%ZZ%eauth;x=<redacted>",
		"x;%ZZ%eauth=S3CRET":        "x;%ZZ%eauth=<redacted>",
	}
	// A piece without = is a name to every reading of it, and is shown; the
	// parser a report used to go through left such a pair out whole when it
	// held a ;, so the report showed nothing of it at all.
	namesOnly := map[string]bool{"token;S3CRET": true}
	for _, query := range maskDifferentialQueries {
		name := query
		if len(name) > 60 {
			name = name[:60] + "…"
		}
		displayed, report := maskQuery(query, false), maskQuery(query, true)
		for _, oracle := range []struct{ what, shown, now string }{
			{"a displayed target before queries were shown as sent", formerMaskCredentialQueryValues(query), displayed},
			{"a displayed target split at & and ; only", splitOnlyMaskQuery(query, false), displayed},
			{"a report before queries were shown as sent", formerMaskQueryValues((&url.URL{RawQuery: query}).Query()), report},
			{"a report split at & and ; only", splitOnlyMaskQuery(query, true), report},
		} {
			if !strings.Contains(oracle.shown, "S3") && strings.Contains(oracle.now, "S3") && !namesOnly[query] {
				t.Errorf("%q: %s withheld the value, and it is now shown", name, oracle.what)
			}
		}
		// A report is what it was, and a displayed target too but for the
		// names with a ; and the one read as written.
		if report != splitOnlyMaskQuery(query, true) {
			t.Errorf("%q: a report is %q, want it as it was, %q", name, report, splitOnlyMaskQuery(query, true))
		}
		want, listed := maskedAgain[query]
		if !listed {
			want = splitOnlyMaskQuery(query, false)
		}
		if displayed != want {
			t.Errorf("%q is displayed as %q, want %q", name, displayed, want)
		}
		if listed && want == splitOnlyMaskQuery(query, false) {
			t.Errorf("%q is listed as displayed otherwise than it was, and is not", name)
		}
		// Every piece that was sent has its place in what is shown.
		for _, shown := range []string{displayed, report} {
			if strings.Count(shown, "&") != strings.Count(query, "&") || strings.Count(shown, ";") != strings.Count(query, ";") {
				t.Errorf("%q is shown with other pairs than it has", name)
			}
		}
	}
	for query := range maskedAgain {
		found := false
		for _, known := range maskDifferentialQueries {
			found = found || known == query
		}
		if !found {
			t.Errorf("%q is listed and not among the queries", query)
		}
	}
	// A name with a ; that is no credential's either way is left as sent,
	// and so is everything about a pair that is not a credential's.
	for _, query := range []string{"api;version=2", "a=1;pass;word=x", "to;ken=x", "a;b=1&c;d=2", "page;sort=name&limit=5"} {
		if got := maskQuery(query, false); got != query {
			t.Errorf("%q is displayed as %q, want it as sent", query, got)
		}
	}
	// A displayed target, a report and an error, as they are shown.
	if got := safeTarget("https://host/p?tenant=a&token;id=SECRET&v=1"); got != "https://host/p?tenant=a&token;id=<redacted>&v=1" {
		t.Errorf("safeTarget = %q", got)
	}
	if got := RedactURLString("https://host/p?tenant=a&pass;word=SECRET", MaskQueryValues); got != "https://host/p?tenant=<redacted>&pass;word=<redacted>" {
		t.Errorf("a report shows %q", got)
	}
}
