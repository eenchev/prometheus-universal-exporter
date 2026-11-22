//go:build request_type_smtp || request_type_imap || request_type_pop3

package fetch

import (
	"context"
	"crypto/hmac"
	"crypto/md5" //nolint:gosec // G501: CRAM-MD5's digest
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// withLogin sets how a collector logs in, auth and xoauth2, after account
// has set its account, if it sets one.
func withLogin(auth string, xoauth2 *model.XOAuth2Config, account func(*model.RequestConfig)) func(*model.RequestConfig) {
	return func(r *model.RequestConfig) {
		if account != nil {
			account(r)
		}
		r.Auth, r.XOAuth2 = auth, xoauth2
	}
}

// A static target's own password_file is refused when its collector logs
// in with xoauth2, which would read the file and send the collector's
// token for the target's account; with a password login it is the
// target's account, as before.
func TestATargetOfAnXOAuth2CollectorNamesNoPasswordOfItsOwn(t *testing.T) {
	for _, typ := range BuiltRequestTypes() {
		rt := RequestTypes[typ]
		if !slices.Contains(rt.Fields, "xoauth2") {
			continue
		}
		target := &model.StaticTarget{Name: "other", Request: model.TargetRequestConfig{Username: "other@example.com", PasswordFile: "/password"}}
		withToken := &model.Collector{Name: "mail", Request: model.RequestConfig{Type: typ, Username: "me@example.com", XOAuth2: &model.XOAuth2Config{TokenFile: "/token"}}}
		withPassword := &model.Collector{Name: "mail", Request: model.RequestConfig{Type: typ, Username: "me@example.com", PasswordFile: "/password"}}
		for _, c := range []*model.Collector{withToken, withPassword} {
			if err := ValidateRequest(c); err != nil {
				t.Fatal(err)
			}
		}
		if err := CheckTargetRequest(target, withToken); err == nil || !strings.Contains(err.Error(), `target "other": request.password_file is set, but collector "mail" logs in with xoauth2`) {
			t.Errorf("%s: err=%v, want the target's password_file refused", typ, err)
		}
		if err := CheckTargetRequest(target, withPassword); err != nil {
			t.Errorf("%s: a target's account under a password login: %v", typ, err)
		}
	}
}

// netTestCRAMMD5 is the answer to a CRAM-MD5 challenge as a server works
// it out, apart from the code under test.
func netTestCRAMMD5(user, password, challenge string) string {
	mac := hmac.New(md5.New, []byte(password))
	_, _ = mac.Write([]byte(challenge))
	return base64.StdEncoding.EncodeToString([]byte(user + " " + hex.EncodeToString(mac.Sum(nil))))
}

// The credentials of the SASL mechanisms are what their RFCs' own examples
// are: CRAM-MD5's answer to RFC 2195's challenge, and OAUTHBEARER's
// message of RFC 7628, the account as the authorization identity with its
// = and , escaped, then the host, the port and the token. A challenge that
// is not base64 is said to be none.
func TestTheSASLCredentialsAreTheirRFCsExamples(t *testing.T) {
	challenge := base64.StdEncoding.EncodeToString([]byte("<1896.697170952@postoffice.reston.mci.net>"))
	if got, err := netCRAMMD5Response("tim", "tanstaaftanstaaf", challenge); err != nil || got != "dGltIGI5MTNhNjAyYzdlZGE3YTQ5NWI0ZTZlNzMzNGQzODkw" {
		t.Errorf("CRAM-MD5: %q, %v", got, err)
	}
	if _, err := netCRAMMD5Response("tim", "x", "not base64!"); err == nil {
		t.Error("a challenge that is not base64 was answered")
	}
	want := "n,a=user@example.com,\x01host=server.example.com\x01port=143\x01auth=Bearer vF9dft4qmTc2Nvb3RlckBhbHRhdmlzdGEuY29tCg==\x01\x01"
	if got := netOAuthBearerCredential("user@example.com", "server.example.com:143", "vF9dft4qmTc2Nvb3RlckBhbHRhdmlzdGEuY29tCg=="); got != want {
		t.Errorf("OAUTHBEARER: %q", got)
	}
	if got := netOAuthBearerCredential("a=b,c", "[2001:db8::1]:993", "t"); got != "n,a=a=3Db=2Cc,\x01host=2001:db8::1\x01port=993\x01auth=Bearer t\x01\x01" {
		t.Errorf("OAUTHBEARER with = and , in the account: %q", got)
	}
	if got, _ := base64.StdEncoding.DecodeString(netOAuthBearerEnd); string(got) != "\x01" {
		t.Errorf("what ends a refused OAUTHBEARER is %q", got)
	}
}

// grant: client_credentials asks the endpoint for an application's own
// token, with the client's id and secret and no refresh token, and keeps
// it as the refresh token grant's is kept. The block is held to what the
// grant needs: its secret, no refresh token, a grant there is.
func TestAnApplicationsTokenComesFromTheClientCredentialsGrant(t *testing.T) {
	asked := 0
	endpoint := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked++
		_ = r.ParseForm()
		if r.PostForm.Get("grant_type") != "client_credentials" || r.PostForm.Has("refresh_token") || r.PostForm.Get("client_id") != "app" || r.PostForm.Get("client_secret") != "app-secret" || r.PostForm.Get("scope") != "https://outlook.office365.com/.default" {
			http.Error(w, `{"error":"invalid_client"}`, http.StatusUnauthorized)
			return
		}
		_, _ = fmt.Fprint(w, `{"access_token":"app.token","expires_in":3599,"token_type":"Bearer"}`)
	}))
	defer endpoint.Close()
	saved := netOAuthClient
	netOAuthClient = endpoint.Client()
	t.Cleanup(func() { netOAuthClient = saved })
	grant := &model.XOAuth2Config{Grant: "client_credentials", TokenURL: endpoint.URL + "/token", ClientID: "app", ClientSecretFile: testutil.WriteFile(t, "secret", "app-secret\n"), Scopes: []string{"https://outlook.office365.com/.default"}}
	if err := checkNetXOAuth2(grant); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { netTokens.forget("app", grant) })
	for range 2 {
		if token, err := netAccessToken(context.Background(), "app", grant); err != nil || token != "app.token" {
			t.Fatalf("token %q, %v", token, err)
		}
	}
	if asked != 1 {
		t.Errorf("the endpoint was asked %d times, want once for two scrapes", asked)
	}
	for want, o := range map[string]model.XOAuth2Config{
		"grant: client_credentials without client_secret_file":        {Grant: "client_credentials", TokenURL: "https://login.example.com/token", ClientID: "app"},
		"refresh_token_file beside grant: client_credentials":         {Grant: "client_credentials", TokenURL: "https://login.example.com/token", ClientID: "app", ClientSecretFile: "/s", RefreshTokenFile: "/r"},
		`grant "password" is not refresh_token or client_credentials`: {Grant: "password", TokenURL: "https://login.example.com/token", ClientID: "app"},
		"token_file beside the keys of a grant":                       {Grant: "client_credentials", TokenFile: "/t"},
		"an application's own token is grant: client_credentials":     {TokenURL: "https://login.example.com/token", ClientID: "app", ClientSecretFile: "/s"},
	} {
		if err := checkNetXOAuth2(&o); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("want %q, got %v", want, err)
		}
	}
	if err := checkNetXOAuth2(&model.XOAuth2Config{Grant: "refresh_token", TokenURL: "https://login.example.com/token", ClientID: "app", RefreshTokenFile: "/r"}); err != nil {
		t.Errorf("the refresh token grant, named: %v", err)
	}
}

// A static target of a collector that logs in with a token names its own
// mailbox with request.username alone, and a scrape of it logs in to that
// mailbox with the collector's token; under a password login a username
// without its own password_file changes nothing, as before.
func TestATargetOfATokenCollectorNamesItsMailbox(t *testing.T) {
	for _, typ := range BuiltRequestTypes() {
		if !slices.Contains(RequestTypes[typ].Fields, "xoauth2") {
			continue
		}
		target := &model.StaticTarget{Name: "other", Request: model.TargetRequestConfig{Username: "other@example.com"}}
		withToken := &model.Collector{Name: "mail", Request: model.RequestConfig{Type: typ, Username: "me@example.com", XOAuth2: &model.XOAuth2Config{TokenFile: "/token"}}}
		withPassword := &model.Collector{Name: "mail", Request: model.RequestConfig{Type: typ, Username: "me@example.com", PasswordFile: "/password"}}
		for _, c := range []*model.Collector{withToken, withPassword} {
			if err := ValidateRequest(c); err != nil {
				t.Fatal(err)
			}
			if err := CheckTargetRequest(target, c); err != nil {
				t.Errorf("%s: a target that names its mailbox: %v", typ, err)
			}
		}
		overrides := RequestOverrides{Username: "other@example.com"}
		if got := withTargetAccount(withToken, overrides); got.Request.Username != "other@example.com" || withToken.Request.Username != "me@example.com" {
			t.Errorf("%s: a scrape of the target logs in to %q, and the collector is left with %q", typ, got.Request.Username, withToken.Request.Username)
		}
		if got := withTargetAccount(withPassword, overrides); got.Request.Username != "me@example.com" {
			t.Errorf("%s: under a password login the target's name alone logs in to %q", typ, got.Request.Username)
		}
	}
}
