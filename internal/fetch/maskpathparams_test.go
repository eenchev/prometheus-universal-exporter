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
