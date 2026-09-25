package fetch

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
)

// One set of rules withholds credentials wherever a URL, a header or text is
// shown (redact.go).
func TestRedactURL(t *testing.T) {
	for _, tc := range []struct {
		raw        string
		how        URLRedaction
		want       string
		wantString string
	}{
		{"https://user:pass@host/p?a=1&b=2&a=3#frag", MaskCredentialQueryValues, "https://redacted:redacted@host/p?a=1&b=2&a=3", ""},
		{"https://host/p?tenant=a&access_token=s3cret&X-Amz-Signature=abc&api%5Fkey=k&token&view=full", MaskCredentialQueryValues,
			"https://host/p?tenant=a&access_token=<redacted>&X-Amz-Signature=<redacted>&api%5Fkey=<redacted>&token=<redacted>&view=full", ""},
		{"https://user:pass@host/p?a=1&b=2&a=3#frag", DropQuery, "https://host/p", ""},
		{"https://user:pass@host/p?a=1&b=2&a=3", MaskQueryValues, "https://redacted:redacted@host/p?a=<redacted>&a=<redacted>&b=<redacted>", ""},
		// The fragment is never sent, and can carry a token: it is not shown
		// in any mode.
		{"https://host/cb#access_token=s3cret&state=x", MaskQueryValues, "https://host/cb", ""},
		{"https://host/cb?a=1#access_token=s3cret", MaskCredentialQueryValues, "https://host/cb?a=1", ""},
		{"https://host/cb#access_token=s3cret", DropQuery, "https://host/cb", ""},
		{"https://host/p", MaskQueryValues, "https://host/p", ""},
		{"grpc://host:443/pkg.Svc/Method", MaskQueryValues, "grpc://host:443/pkg.Svc/Method", ""},
	} {
		u, err := url.Parse(tc.raw)
		if err != nil {
			t.Fatal(err)
		}
		if got := RedactURL(u, tc.how); got != tc.want {
			t.Errorf("RedactURL(%q, %d) = %q, want %q", tc.raw, tc.how, got, tc.want)
		}
		if got := RedactURLString(tc.raw, tc.how); got != tc.want {
			t.Errorf("RedactURLString(%q, %d) = %q, want %q", tc.raw, tc.how, got, tc.want)
		}
		if u.String() != tc.raw {
			t.Errorf("RedactURL changed its URL to %q", u)
		}
	}
	if got := RedactURLString("http://a b:%zz@host", MaskCredentialQueryValues); got != Redacted {
		t.Errorf("an unparseable URL was shown as %q", got)
	}
	// The display, label and debug forms agree on userinfo.
	if safeTarget("https://u:pw@host/x") != "https://redacted:redacted@host/x" {
		t.Errorf("safeTarget = %q", safeTarget("https://u:pw@host/x"))
	}
	// A displayed target withholds a credential in its query and keeps the
	// rest, so targets that differ in anything else stay apart.
	if shown := safeTarget("host:9100/metrics?token=s3cret&tenant=a"); shown != "http://host:9100/metrics?token=<redacted>&tenant=a" {
		t.Errorf("safeTarget = %q", shown)
	}
}

// A displayed target withholds the values of every parameter named as a
// credential commonly is, and keeps those of harmless names that only
// contain a credential's word inside another.
func TestMaskCredentialQueryValues(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		// An Azure SAS URL signs with sig.
		{"https://acct.blob.core.windows.net/c/b?sv=2022-11-02&sp=r&sig=abc%2Bdef", "https://acct.blob.core.windows.net/c/b?sv=2022-11-02&sp=r&sig=<redacted>"},
		{"https://host/p?user=bob&pwd=s3cret&pass=s3cret&pw=s3cret", "https://host/p?user=bob&pwd=<redacted>&pass=<redacted>&pw=<redacted>"},
		{"https://host/p?db_pwd=a&userPass=b&X-Sig=c&sig_v2=d", "https://host/p?db_pwd=<redacted>&userPass=<redacted>&X-Sig=<redacted>&sig_v2=<redacted>"},
		{"https://host/p?X-Amz-Signature=a&X-Amz-Credential=b&X-Amz-Security-Token=c", "https://host/p?X-Amz-Signature=<redacted>&X-Amz-Credential=<redacted>&X-Amz-Security-Token=<redacted>"},
		{"https://host/p?apikey=a&api_key=b&auth=c&session=d&passphrase=e&jwt=f", "https://host/p?apikey=<redacted>&api_key=<redacted>&auth=<redacted>&session=<redacted>&passphrase=<redacted>&jwt=<redacted>"},
		// Harmless names stay: the short words count only as a whole word.
		{"https://host/p?design=a&signal=b&bypass=c&compass=d&passthrough=e&page=2", "https://host/p?design=a&signal=b&bypass=c&compass=d&passthrough=e&page=2"},
		{"https://host/p?tenant=a#pwd=s3cret", "https://host/p?tenant=a"},
	} {
		if got := RedactURLString(tc.raw, MaskCredentialQueryValues); got != tc.want {
			t.Errorf("RedactURLString(%q)\n got %q\nwant %q", tc.raw, got, tc.want)
		}
	}
}

func TestCredentialName(t *testing.T) {
	for name, want := range map[string]bool{
		"Authorization": true, "Proxy-Authorization": true, "Cookie": true, "Set-Cookie": true,
		"X-Api-Key": true, "X-Auth-Token": true, "x-session-id": true, "X-Amz-Signature": true,
		"sig": true, "SIG": true, "pwd": true, "pass": true, "X-Pass": true, "userPwd": true,
		"Accept": false, "Content-Type": false, "User-Agent": false, "Host": false,
		"X-Signal": false, "X-Bypass": false, "Passthrough": false, "X-Design": false,
	} {
		if got := CredentialName(name); got != want {
			t.Errorf("CredentialName(%q) = %v", name, got)
		}
		if got := RedactHeaderValue(name, "v") == Redacted; got != want {
			t.Errorf("RedactHeaderValue(%q) redacted: %v", name, got)
		}
	}
}

// Text the exporter did not write has what reads as a credential masked,
// and ordinary text left alone.
func TestRedactText(t *testing.T) {
	for text, secret := range map[string]string{
		`{"error":"invalid","access_token":"s3cretAAA"}`:                    "s3cretAAA",
		`token=s3cretBBB&user=x`:                                            "s3cretBBB",
		`Authorization: Bearer s3cretCCCCCC`:                                "s3cretCCCCCC",
		`auth: Basic dXNlcjpzM2NyZXQ=`:                                      "dXNlcjpzM2NyZXQ=",
		`password: s3cretDDD`:                                               "s3cretDDD",
		`"client_secret" : "s3cretEEE"`:                                     "s3cretEEE",
		`X-Api-Key=s3cretFFF`:                                               "s3cretFFF",
		`https://acct.blob.core.windows.net/c?sv=1&sig=s3cretGGG`:           "s3cretGGG",
		`{"db_pwd": "s3cretHHH"}`:                                           "s3cretHHH",
		`login failed for user=bob pass=s3cretIII`:                          "s3cretIII",
		`X-Cookie: s3cretJJJ`:                                               "s3cretJJJ",
		`detail: token=s3cretKKK`:                                           "s3cretKKK",
		`your jwt eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.c2lnbmF0dXJl is bad`: "eyJhbGciOiJIUzI1NiJ9",
	} {
		got := RedactText(text)
		if strings.Contains(got, secret) || !strings.Contains(got, Redacted) {
			t.Errorf("RedactText(%q) = %q", text, got)
		}
	}
	for _, text := range []string{
		"502 Bad Gateway: the upstream did not answer",
		`{"status":"down","since":"2026-09-25T10:00:00Z"}`,
		"<html><body>Service Unavailable</body></html>",
		"bypass=on signal: 3 design=flat",
	} {
		if got := RedactText(text); got != text {
			t.Errorf("RedactText(%q) changed it to %q", text, got)
		}
	}
}

// An error quoting a URL, as Go's HTTP client quotes the one it requested,
// quotes it without its credentials, and still is what it was.
func TestRedactURLErrors(t *testing.T) {
	cause := errors.New("connection refused")
	inner := &url.Error{Op: "Get", URL: "http://user:pw@host/x?api_key=s3cretA&token=s3cretB", Err: cause}
	err := RedactURLErrors(fmt.Errorf("HTTP request failed: %w", inner))
	if text := err.Error(); strings.Contains(text, "s3cret") || strings.Contains(text, ":pw@") ||
		!strings.Contains(text, `"http://redacted:redacted@host/x?api_key=<redacted>&token=<redacted>"`) {
		t.Fatalf("%s", text)
	}
	if !errors.Is(err, cause) {
		t.Fatal("the cause is lost")
	}
	if RedactURLErrors(nil) != nil {
		t.Fatal("nil became an error")
	}
	var asURL *url.Error
	if !errors.As(err, &asURL) {
		t.Fatal("the url.Error is lost")
	}
	// An error quoting no URL is left as it is.
	if plain := errors.New("no"); RedactURLErrors(plain) != plain { //nolint:errorlint // the very same error, not one like it
		t.Fatal("an error without a URL was wrapped")
	}
}
