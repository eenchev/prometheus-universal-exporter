package fetch

import (
	"log/slog"
	"strings"
	"testing"
)

// A path parameter whose name reads as a credential's has its value masked
// in every form a URL is shown in, as a servlet container's ;jsessionid=
// rides in a path when cookies are off: a target in a log line and $url
// (MaskCredentialQueryValues), an error's and a debug report's
// (MaskQueryValues, RedactRequestURL), and a metric label's (DropQuery).
// Other path parameters are kept, and so is a path segment that is a secret
// whole, which has no name to tell it by.
func TestAPathParameterOfACredentialIsMasked(t *testing.T) {
	for _, tc := range []struct {
		raw              string
		how              URLRedaction
		want             string
		debugReportLines string
	}{
		{"https://shop.example/app;jsessionid=S3CRET?x=1", MaskCredentialQueryValues, "https://shop.example/app;jsessionid=<redacted>?x=1", "https://shop.example/app;jsessionid=<redacted>?x=<redacted>"},
		{"https://shop.example/app;jsessionid=S3CRET?x=1", MaskQueryValues, "https://shop.example/app;jsessionid=<redacted>?x=<redacted>", ""},
		{"https://shop.example/app;jsessionid=S3CRET?x=1#f", DropQuery, "https://shop.example/app;jsessionid=<redacted>", ""},
		// Several segments and parameters, an escaped name, a SID, and
		// harmless ones kept.
		{"https://h/a;v=2/b;sid=S1;lang=en/c;%74oken=S2;JSESSIONID=S3/d", MaskCredentialQueryValues, "https://h/a;v=2/b;sid=<redacted>;lang=en/c;%74oken=<redacted>;JSESSIONID=<redacted>/d", ""},
		{"https://user:pw@h/x;session=S4?code=S5&state=ok", MaskCredentialQueryValues, "https://redacted:redacted@h/x;session=<redacted>?code=<redacted>&state=ok", ""},
		// An escaped path keeps its escapes.
		{"https://h/a%20b;jsessionid=S6/c%2Fd", MaskCredentialQueryValues, "https://h/a%20b;jsessionid=<redacted>/c%2Fd", ""},
		// A token as a segment of its own is kept: nothing names it.
		{"https://api.telegram.example/bot123456:AAE-s3cret/getMe", MaskCredentialQueryValues, "https://api.telegram.example/bot123456:AAE-s3cret/getMe", ""},
		{"https://h/a;b/c;=x;jsessionid", MaskCredentialQueryValues, "https://h/a;b/c;=x;jsessionid", ""},
	} {
		if got := RedactURLString(tc.raw, tc.how); got != tc.want {
			t.Errorf("RedactURLString(%q, %d)\n got %q\nwant %q", tc.raw, tc.how, got, tc.want)
		}
		if tc.debugReportLines != "" {
			if got := RedactRequestURL(tc.raw, nil); got != tc.debugReportLines {
				t.Errorf("RedactRequestURL(%q)\n got %q\nwant %q", tc.raw, got, tc.debugReportLines)
			}
		}
	}
	// A log line names the target as safeTarget shows it.
	logs := &strings.Builder{}
	slog.New(slog.NewTextHandler(logs, nil)).Info("probe failed", "target", safeTarget("https://shop.example/app;jsessionid=S3CRET?ticket=ST-1-S3CRET&lang=en"))
	if strings.Contains(logs.String(), "S3CRET") || !strings.Contains(logs.String(), "app;jsessionid=<redacted>?ticket=<redacted>&lang=en") {
		t.Errorf("the log line: %s", logs)
	}
	// A URL quoted in an error is masked as a debug report's is.
	if got := RedactURLString("https://h/app;jsessionid=S3CRET", MaskQueryValues); strings.Contains(got, "S3CRET") {
		t.Errorf("an error's URL: %s", got)
	}
}

// A ; written escaped, %3B or %3b, separates path parameters as one written
// as it is does, since a server that decodes the path first reads it as one:
// the value after it is masked in every form, the escape kept as written.
func TestAnEscapedSemicolonSeparatesPathParameters(t *testing.T) {
	for raw, want := range map[string]string{
		"https://h/x;jsessionid=S1/app%3Bjsessionid=S2?a=1": "https://h/x;jsessionid=<redacted>/app%3Bjsessionid=<redacted>?a=1",
		"https://h/app%3bJSESSIONID=S3%3Bv=2":               "https://h/app%3bJSESSIONID=<redacted>%3Bv=2",
		"https://h/a;v=1%3Btoken=S4;lang=en":                "https://h/a;v=1%3Btoken=<redacted>;lang=en",
		"https://h/a%3B%6Asessionid=S5":                     "https://h/a%3B%6Asessionid=<redacted>",
		"https://h/a%3Bv=2/b%3B":                            "https://h/a%3Bv=2/b%3B",
		"https://h/token%3B=S6":                             "https://h/token%3B=S6",
	} {
		for _, how := range []URLRedaction{MaskCredentialQueryValues, MaskQueryValues, DropQuery} {
			got := RedactURLString(raw, how)
			wantHow := want
			switch how {
			case MaskQueryValues:
				wantHow = strings.Replace(want, "?a=1", "?a=<redacted>", 1)
			case DropQuery:
				wantHow, _, _ = strings.Cut(want, "?")
			}
			if got != wantHow {
				t.Errorf("RedactURLString(%q, %d)\n got %q\nwant %q", raw, how, got, wantHow)
			}
		}
	}
}

// The debug report's $url is $url as the rules read it with its query
// values masked as the report masks them, the configured pairs shown: it is
// not parsed again, so the path's escapes stay as $url has them, where a
// URL parsed with <redacted> in its path would have them written anew.
func TestTheDebugReportsURLIsTheRulesURLWithItsQueryMasked(t *testing.T) {
	raw := "https://h/x;%6Asessionid=S1/app%3Bjsessionid=S2/a%2Fb?lang=en&token=S3&page=2"
	shown := RedactURLString(raw, MaskCredentialQueryValues)
	if want := "https://h/x;%6Asessionid=<redacted>/app%3Bjsessionid=<redacted>/a%2Fb?lang=en&token=<redacted>&page=2"; shown != want {
		t.Fatalf("$url\n got %q\nwant %q", shown, want)
	}
	configured := func(name, value string) bool { return name == "lang" && value == "en" }
	want := "https://h/x;%6Asessionid=<redacted>/app%3Bjsessionid=<redacted>/a%2Fb?lang=en&token=<redacted>&page=<redacted>"
	if got := RedactShownURLQuery(shown, configured); got != want {
		t.Errorf("the debug report's $url\n got %q\nwant %q", got, want)
	}
	for _, url := range []string{"https://h/a;jsessionid=<redacted>", Redacted, ""} {
		if got := RedactShownURLQuery(url, nil); got != url {
			t.Errorf("RedactShownURLQuery(%q) = %q", url, got)
		}
	}
}
