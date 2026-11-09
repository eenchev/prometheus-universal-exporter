//go:build !select_request_types || request_type_http

package fetch

import (
	"context"
	"crypto/hmac"
	"crypto/sha1" // #nosec G505 -- the signature OAuth 1.0a checks
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// ssoServer is a target that signs in as Garmin's does, in steps: a sign-in
// page that sets a session cookie and carries a token against cross-site
// requests; a form posted with the password, the cookie and that token,
// answered with a page holding a ticket; the ticket traded, signed with
// OAuth 1.0a by the consumer alone, for an OAuth 1.0a token and secret, as
// a form; and that token, signed with it, traded for a bearer token of an
// hour, as JSON. /refresh trades the OAuth 1.0a token for a bearer token
// again, and /data answers value=1 to the last bearer token handed out.
type ssoServer struct {
	*httptest.Server
	mu     sync.Mutex
	hits   map[string]int
	bearer string
	// failRefresh makes /refresh answer 401.
	failRefresh atomic.Bool
	// refuse is the status /data answers a token it does not take with.
	refuse  int
	minted  int
	expires func(n int) string
}

const (
	ssoConsumerKey    = "consumer"
	ssoConsumerSecret = "consumer-secret"
)

func newSSOServer(t *testing.T) *ssoServer {
	t.Helper()
	s := &ssoServer{hits: map[string]int{}, refuse: http.StatusUnauthorized}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.hits[r.URL.Path]++
		s.mu.Unlock()
		switch r.URL.Path {
		case "/signin":
			if r.Method == http.MethodGet {
				http.SetCookie(w, &http.Cookie{Name: "SESSION", Value: "s1", Path: "/"})
				_, _ = io.WriteString(w, `<form><input type="hidden" name="_csrf" value="c-123"></form>`)
				return
			}
			cookie, err := r.Cookie("SESSION")
			_ = r.ParseForm()
			if err != nil || cookie.Value != "s1" || r.PostForm.Get("_csrf") != "c-123" || r.PostForm.Get("username") != "me@example.com" || r.PostForm.Get("password") != "pw" || r.Header.Get("User-Agent") != "steps-test" {
				http.Error(w, "refused", http.StatusForbidden)
				return
			}
			_, _ = io.WriteString(w, `<script>var url = "https://sso.example/embed?ticket=ST-42-abc";</script>`)
		case "/preauthorized":
			if r.URL.Query().Get("ticket") != "ST-42-abc" || !oauth1Verified(r, ssoConsumerSecret, "") {
				http.Error(w, "bad ticket or signature", http.StatusUnauthorized)
				return
			}
			_, _ = io.WriteString(w, "oauth_token=t1&oauth_token_secret=ts1&mfa_token=")
		case "/exchange", "/refresh":
			if r.URL.Path == "/refresh" && s.failRefresh.Load() {
				http.Error(w, "expired", http.StatusUnauthorized)
				return
			}
			if !strings.Contains(r.Header.Get("Authorization"), `oauth_token="t1"`) || !oauth1Verified(r, ssoConsumerSecret, "ts1") {
				http.Error(w, "bad signature", http.StatusUnauthorized)
				return
			}
			s.mu.Lock()
			s.minted++
			s.bearer = fmt.Sprintf("bearer-%d", s.minted)
			answer := map[string]any{"access_token": s.bearer, "expires_in": 3600}
			if s.expires != nil {
				answer = map[string]any{"access_token": s.bearer, "expires": s.expires(s.minted)}
			}
			s.mu.Unlock()
			_ = json.NewEncoder(w).Encode(answer)
		default:
			s.mu.Lock()
			current := s.bearer
			s.mu.Unlock()
			if r.Header.Get("Authorization") != "Bearer "+current {
				w.WriteHeader(s.refuse)
				return
			}
			_, _ = io.WriteString(w, "value=1")
		}
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *ssoServer) hit(path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hits[path]
}

// oauth1Verified checks a request's OAuth 1.0a HMAC-SHA1 signature as a
// server does: from the parameters of its Authorization header, its query
// and its form body.
func oauth1Verified(r *http.Request, consumerSecret, tokenSecret string) bool {
	header, ok := strings.CutPrefix(r.Header.Get("Authorization"), "OAuth ")
	if !ok {
		return false
	}
	params := url.Values{}
	signature := ""
	for _, part := range strings.Split(header, ", ") {
		name, quoted, _ := strings.Cut(part, "=")
		value, _ := url.PathUnescape(strings.Trim(quoted, `"`))
		if name == "oauth_signature" {
			signature = value
			continue
		}
		params.Add(name, value)
	}
	for name, values := range r.URL.Query() {
		params[name] = append(params[name], values...)
	}
	if r.Header.Get("Content-Type") == "application/x-www-form-urlencoded" {
		_ = r.ParseForm()
		for name, values := range r.PostForm {
			params[name] = append(params[name], values...)
		}
	}
	var pairs []string
	for name, values := range params {
		for _, value := range values {
			pairs = append(pairs, oauth1Escape(name)+"="+oauth1Escape(value))
		}
	}
	sort.Strings(pairs)
	base := r.Method + "&" + oauth1Escape("http://"+r.Host+r.URL.EscapedPath()) + "&" + oauth1Escape(strings.Join(pairs, "&"))
	mac := hmac.New(sha1.New, []byte(oauth1Escape(consumerSecret)+"&"+oauth1Escape(tokenSecret)))
	mac.Write([]byte(base))
	return hmac.Equal([]byte(signature), []byte(base64.StdEncoding.EncodeToString(mac.Sum(nil))))
}

// garminLike is the steps login of ssoServer, with refresh_steps and its
// OAuth 1.0a token kept for a year.
func garminLike(t *testing.T) model.LoginConfig {
	t.Helper()
	oauth1 := &model.LoginOAuth1{ConsumerKey: "{{login.consumer_key}}", ConsumerSecret: "{{login.consumer_secret}}", Token: "{{login.oauth_token}}", TokenSecret: "{{login.oauth_token_secret}}"}
	exchange := model.LoginStep{URL: "/exchange", Method: http.MethodPost, OAuth1: oauth1, Extract: map[string]model.LoginExtract{"access_token": {JSON: ".access_token"}, "expires_in": {JSON: ".expires_in"}}}
	refresh := exchange
	refresh.URL = "/refresh"
	return model.LoginConfig{
		Type:        "steps",
		Values:      map[string]string{"username": "me@example.com", "consumer_key": ssoConsumerKey},
		SecretFiles: secretFiles(t, map[string]string{"password": "pw", "consumer_secret": ssoConsumerSecret}),
		Steps: []model.LoginStep{
			{URL: "/signin", Extract: map[string]model.LoginExtract{"csrf": {Regex: `name="_csrf" value="([^"]+)"`}}},
			{URL: "/signin", Headers: map[string]string{"User-Agent": "steps-test"}, Form: map[string]string{"username": "{{login.username}}", "password": "{{login.password}}", "_csrf": "{{login.csrf}}"}, Extract: map[string]model.LoginExtract{"ticket": {Regex: `ticket=(ST-[A-Za-z0-9-]+)`}}},
			{URL: "/preauthorized", Query: map[string]string{"ticket": "{{login.ticket}}"}, OAuth1: &model.LoginOAuth1{ConsumerKey: "{{login.consumer_key}}", ConsumerSecret: "{{login.consumer_secret}}"}, Extract: map[string]model.LoginExtract{"oauth_token": {Form: "oauth_token"}, "oauth_token_secret": {Form: "oauth_token_secret"}}},
			exchange,
		},
		RefreshSteps: []model.LoginStep{refresh},
		Keep:         map[string]model.Duration{"oauth_token": model.Duration(365 * 24 * time.Hour), "oauth_token_secret": model.Duration(365 * 24 * time.Hour)},
		Token:        "access_token",
		ExpiresIn:    "expires_in",
	}
}

// A steps login makes its steps in order, with one cookie jar: the cookie
// the sign-in page set is sent with the form, the token against cross-site
// requests read from the page is posted, the ticket read from the answer
// is sent in the next step's query, signed with OAuth 1.0a by the
// consumer, the OAuth 1.0a token read from that form-encoded answer signs
// the exchange, and the bearer token it answers is sent to the target.
// The token is kept for the requests after it.
func TestAStepsLoginIsMadeInOrder(t *testing.T) {
	freshLogins(t)
	server := newSSOServer(t)
	c := loginCollector(t, garminLike(t))
	for range 2 {
		response, err := FetchCollector(context.Background(), server.URL+"/data", c, RequestOverrides{}, nil)
		if err != nil || response.StatusCode != http.StatusOK {
			t.Fatalf("fetch: %v, %+v", err, response)
		}
	}
	for path, want := range map[string]int{"/signin": 2, "/preauthorized": 1, "/exchange": 1, "/refresh": 0, "/data": 2} {
		if got := server.hit(path); got != want {
			t.Errorf("%s was asked %d times, want %d", path, got, want)
		}
	}
	if made, failed := logins.counts("api"); made != 1 || failed != 0 || logins.refreshes("api") != 0 {
		t.Fatalf("%d logins, %d failed, %d refreshes", made, failed, logins.refreshes("api"))
	}
}

// When the token is due, refresh_steps make a new one of the values kept,
// with no sign-in; when they fail, the whole steps are made; and once a
// value kept has expired, the whole steps are made without refresh_steps.
func TestRefreshStepsMakeTheNextTokenOfTheValuesKept(t *testing.T) {
	advance := freshLogins(t)
	server := newSSOServer(t)
	l := garminLike(t)
	l.Keep = map[string]model.Duration{"oauth_token": model.Duration(3 * time.Hour), "oauth_token_secret": model.Duration(3 * time.Hour)}
	c := loginCollector(t, l)
	fetch := func() {
		t.Helper()
		if response, err := FetchCollector(context.Background(), server.URL+"/data", c, RequestOverrides{}, nil); err != nil || response.StatusCode != http.StatusOK {
			t.Fatalf("fetch: %v, %+v", err, response)
		}
	}
	fetch()
	advance(55 * time.Minute)
	fetch()
	if server.hit("/refresh") != 1 || server.hit("/exchange") != 1 || logins.refreshes("api") != 1 {
		t.Fatalf("after the token was due: %d refreshes, %d exchanges, %d counted", server.hit("/refresh"), server.hit("/exchange"), logins.refreshes("api"))
	}
	server.failRefresh.Store(true)
	advance(55 * time.Minute)
	fetch()
	if server.hit("/refresh") != 2 || server.hit("/exchange") != 2 || logins.refreshes("api") != 1 {
		t.Fatalf("after a refresh failed: %d refreshes, %d exchanges", server.hit("/refresh"), server.hit("/exchange"))
	}
	server.failRefresh.Store(false)
	// The values the whole login just read are kept for 3 hours from it.
	advance(3*time.Hour + time.Minute)
	fetch()
	if server.hit("/refresh") != 2 || server.hit("/exchange") != 3 {
		t.Fatalf("after the values kept expired: %d refreshes, %d exchanges", server.hit("/refresh"), server.hit("/exchange"))
	}
	if made, failed := logins.counts("api"); made != 4 || failed != 0 {
		t.Fatalf("%d logins, %d failed", made, failed)
	}
}

// keep_file holds the token and the values kept, readable by the
// exporter's user alone, so that an exporter that starts again sends the
// token it holds, making no login, and, once it is due, makes the next
// with refresh_steps, not a sign-in; and one that shares the file with
// another takes the token the other's login wrote there.
func TestAKeepFileOutlivesARestart(t *testing.T) {
	advance := freshLogins(t)
	server := newSSOServer(t)
	l := garminLike(t)
	l.KeepFile = filepath.Join(t.TempDir(), "garmin.json")
	c := loginCollector(t, l)
	fetch := func() {
		t.Helper()
		if response, err := FetchCollector(context.Background(), server.URL+"/data", c, RequestOverrides{}, nil); err != nil || response.StatusCode != http.StatusOK {
			t.Fatalf("fetch: %v, %+v", err, response)
		}
	}
	fetch()
	info, err := os.Stat(l.KeepFile)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("the keep file: %v, %v", info, err)
	}
	raw, _ := os.ReadFile(l.KeepFile)
	if !strings.Contains(string(raw), "bearer-1") || !strings.Contains(string(raw), `"value": "t1"`) || strings.Contains(string(raw), "c-123") || strings.Contains(string(raw), server.URL) {
		t.Fatalf("the keep file holds:\n%s", raw)
	}
	// The exporter starts again: every token and value in memory is gone.
	logins.reset()
	fetch()
	if server.hit("/signin") != 2 || server.hit("/exchange") != 1 || server.hit("/refresh") != 0 {
		t.Fatalf("after a restart: %d sign-ins, %d exchanges, %d refreshes", server.hit("/signin")/2, server.hit("/exchange"), server.hit("/refresh"))
	}
	// The token read from the file is no login.
	if made, failed := logins.counts("api"); made != 0 || failed != 0 {
		t.Fatalf("a token read from the keep file counted as %d logins, %d failed", made, failed)
	}
	logins.reset()
	advance(61 * time.Minute)
	fetch()
	if server.hit("/signin") != 2 || server.hit("/refresh") != 1 {
		t.Fatalf("after a restart past the token's life: %d sign-ins, %d refreshes", server.hit("/signin")/2, server.hit("/refresh"))
	}
	// Another exporter that shares the file, as the target allocator's
	// replicas may, logs in: when this one's token is due, it takes the
	// other's from the file rather than logging in.
	stepsStates.mu.Lock()
	var key string
	for k := range stepsStates.byKey {
		key = k
	}
	stepsStates.mu.Unlock()
	keepFiles.write(l.KeepFile, key, keptLogin{Token: "bearer-other", Expires: loginNow().Add(2 * time.Hour), Kept: map[string]loginValue{"oauth_token": {Text: "t1"}, "oauth_token_secret": {Text: "ts1"}}})
	server.mu.Lock()
	server.bearer = "bearer-other"
	server.mu.Unlock()
	advance(55 * time.Minute)
	fetch()
	if server.hit("/signin") != 2 || server.hit("/refresh") != 1 || server.hit("/exchange") != 1 {
		t.Fatalf("with another exporter's token in the file: %d sign-ins, %d refreshes", server.hit("/signin")/2, server.hit("/refresh"))
	}
	// A keep file that cannot be written leaves the login made, and says so.
	var warned atomic.Bool
	previous := keepFileWarning
	keepFileWarning = func(string, error) { warned.Store(true) }
	t.Cleanup(func() { keepFileWarning = previous })
	l.KeepFile = filepath.Join(t.TempDir(), "missing", "garmin.json")
	unwritable := loginCollector(t, l)
	if response, err := FetchCollector(context.Background(), server.URL+"/data", unwritable, RequestOverrides{}, nil); err != nil || response.StatusCode != http.StatusOK || !warned.Load() {
		t.Fatalf("with a keep file it cannot write: %v, %+v, warned %v", err, response, warned.Load())
	}
	if warnings := checkLoginFiles(unwritable, nil); len(warnings) != 1 || !strings.Contains(warnings[0], "keep_file") {
		t.Fatalf("the dry run's warnings: %q", warnings)
	}
}

// relogin_on makes a status other than 401 drop the token and send the
// request again with a new login's, of any type of login; without it, a
// 403 is the probe's answer.
func TestReloginOnTakesOtherStatusesForARefusedToken(t *testing.T) {
	freshLogins(t)
	server := newSSOServer(t)
	server.refuse = http.StatusForbidden
	l := garminLike(t)
	c := loginCollector(t, l)
	if response, err := FetchCollector(context.Background(), server.URL+"/data", c, RequestOverrides{}, nil); err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("fetch: %v, %+v", err, response)
	}
	// The server forgets the token it handed out.
	server.mu.Lock()
	server.bearer = "revoked"
	server.mu.Unlock()
	if response, err := FetchCollector(context.Background(), server.URL+"/data", c, RequestOverrides{}, nil); err != nil || response.StatusCode != http.StatusForbidden {
		t.Fatalf("a 403 without relogin_on: %v, %+v", err, response)
	}
	l.ReloginOn = []int{401, 403}
	c = loginCollector(t, l)
	if response, err := FetchCollector(context.Background(), server.URL+"/data", c, RequestOverrides{}, nil); err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("fetch: %v, %+v", err, response)
	}
	server.mu.Lock()
	server.bearer = "revoked"
	server.mu.Unlock()
	response, err := FetchCollector(context.Background(), server.URL+"/data", c, RequestOverrides{}, nil)
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("a 403 with relogin_on: %v, %+v", err, response)
	}
	if server.hit("/refresh") != 1 {
		t.Fatalf("the new login was made with %d refreshes, want the values kept used", server.hit("/refresh"))
	}
}

// A token's lifetime is, of the first that says one: expires_in or
// expires_at, a time written in seconds or milliseconds since 1970, RFC
// 3339 or an HTTP date; the token's cookie's; its JWT exp claim; ttl.
func TestAStepsLoginsTokenLifetime(t *testing.T) {
	start := time.Date(2026, 11, 9, 8, 0, 0, 0, time.UTC)
	exp := time.Date(2026, 11, 9, 9, 30, 0, 0, time.UTC)
	jwt := "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString(fmt.Appendf(nil, `{"sub":"me","exp":%d}`, exp.Unix())) + ".sig"
	cookie := time.Date(2026, 11, 9, 8, 45, 0, 0, time.UTC)
	for name, tc := range map[string]struct {
		l      model.LoginConfig
		values map[string]loginValue
		want   time.Time
	}{
		"expires_in":            {model.LoginConfig{Token: "t", ExpiresIn: "in"}, map[string]loginValue{"t": {Text: jwt}, "in": {Text: "600"}}, start.Add(10 * time.Minute)},
		"expires_at, seconds":   {model.LoginConfig{Token: "t", ExpiresAt: "at"}, map[string]loginValue{"t": {Text: "x"}, "at": {Text: strconv.FormatInt(exp.Unix(), 10)}}, exp},
		"expires_at, ms":        {model.LoginConfig{Token: "t", ExpiresAt: "at"}, map[string]loginValue{"t": {Text: "x"}, "at": {Text: strconv.FormatInt(exp.UnixMilli(), 10)}}, exp},
		"expires_at, RFC 3339":  {model.LoginConfig{Token: "t", ExpiresAt: "at"}, map[string]loginValue{"t": {Text: "x"}, "at": {Text: "2026-11-09T11:30:00+02:00"}}, exp},
		"expires_at, HTTP date": {model.LoginConfig{Token: "t", ExpiresAt: "at"}, map[string]loginValue{"t": {Text: "x"}, "at": {Text: "Mon, 09 Nov 2026 09:30:00 GMT"}}, exp},
		"the cookie's":          {model.LoginConfig{Token: "t", TTL: model.Duration(time.Minute)}, map[string]loginValue{"t": {Text: jwt, Expires: cookie}}, cookie},
		"the JWT's":             {model.LoginConfig{Token: "t", TTL: model.Duration(time.Minute)}, map[string]loginValue{"t": {Text: jwt}}, exp},
		"ttl":                   {model.LoginConfig{Token: "t", TTL: model.Duration(time.Minute)}, map[string]loginValue{"t": {Text: "x.y"}}, start.Add(time.Minute)},
		"none":                  {model.LoginConfig{Token: "t"}, map[string]loginValue{"t": {Text: "opaque"}}, time.Time{}},
		"an absent expires_in":  {model.LoginConfig{Token: "t", ExpiresIn: "in", TTL: model.Duration(time.Minute)}, map[string]loginValue{"t": {Text: "x"}}, start.Add(time.Minute)},
	} {
		token, err := stepsToken(&tc.l, tc.values, start)
		if err != nil || !token.expires.Equal(tc.want) {
			t.Errorf("%s: expires %v, %v; want %v", name, token.expires, err, tc.want)
		}
	}
	// A time before the login, as a lifetime in seconds read as one, is
	// refused: kept, the token would be fetched again at every request.
	for _, text := range []string{"3600", start.Add(-time.Second).Format(time.RFC3339), start.Format(time.RFC3339)} {
		l := model.LoginConfig{Token: "t", ExpiresAt: "at"}
		if _, err := stepsToken(&l, map[string]loginValue{"t": {Text: "x"}, "at": {Text: text}}, start); err == nil || !strings.Contains(err.Error(), "is a time before the login") {
			t.Errorf("expires_at %s: %v", text, err)
		}
	}
	for at, want := range map[string]string{"in": "no lifetime in seconds", "at": "no time"} {
		l := model.LoginConfig{Token: "t", ExpiresIn: map[bool]string{true: "in"}[at == "in"], ExpiresAt: map[bool]string{true: "at"}[at == "at"]}
		if _, err := stepsToken(&l, map[string]loginValue{"t": {Text: "x"}, at: {Text: "soon"}}, start); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", at, err)
		}
	}
	// A JWT whose exp is not after the login is no lifetime: ttl, or none.
	stale := "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString(fmt.Appendf(nil, `{"exp":%d}`, start.Add(-time.Minute).Unix())) + ".sig"
	for ttl, want := range map[model.Duration]time.Time{0: {}, model.Duration(time.Minute): start.Add(time.Minute)} {
		token, err := finishLoginToken(&model.LoginConfig{Type: "json", TTL: ttl}, loginToken{value: stale}, start)
		if err != nil || !token.expires.Equal(want) {
			t.Errorf("an expired JWT with ttl %v: expires %v, %v; want %v", ttl, token.expires, err, want)
		}
	}
	// A json login's token that says no lifetime takes its JWT's too.
	token, err := finishLoginToken(&model.LoginConfig{Type: "json"}, loginToken{value: jwt}, start)
	if err != nil || !token.expires.Equal(exp) || token.value != "Bearer "+jwt {
		t.Fatalf("a json login's JWT: %+v, %v", token, err)
	}
	if !jwtExpiry("a.b.c").IsZero() || !jwtExpiry("opaque").IsZero() || !jwtExpiry("Bearer "+jwt).Equal(exp) {
		t.Fatal("jwtExpiry read what is no JWT's exp, or missed one")
	}
}

// The OAuth 1.0a signature is RFC 5849's, as the example of Twitter's
// documentation of it gives one.
func TestTheOAuth1SignatureIsRFC5849s(t *testing.T) {
	u, _ := url.Parse("https://api.twitter.com/1.1/statuses/update.json?include_entities=true")
	form := url.Values{"status": {"Hello Ladies + Gentlemen, a signed OAuth request!"}}
	signer := oauth1Signer{consumerKey: "xvz1evFS4wEEPTGEFPHBog", consumerSecret: "kAcSOqF21Fu85e7zjz7ZN2U4ZRhfV3WpwPAoE3Z7kBw", token: "370773112-GmHxMAgYyLbNEtIKZeRNFsMKPR9EyMZeS9weJAEb", tokenSecret: "LswwdoUaIvS8ltyTt5jkRh4J50vUPVVHtR2YPi5kE"}
	header := signer.authorization(http.MethodPost, u, form, "kYjzVBB8Y0ZFabxSWbWovY3uYSQ2pTgmZeNu2VS4cg", 1318622958)
	if !strings.Contains(header, `oauth_signature="hCtSmYh%2BiHYCEqBWrE7C7hYmtUk%3D"`) || !strings.HasPrefix(header, "OAuth ") {
		t.Fatalf("signed %s", header)
	}
}

// A step's answer that is not one it takes fails the login naming the
// step, a redirect saying how it is read; and what a step reads that is
// not there names the value, never what the answer held.
func TestAStepThatFailsIsNamed(t *testing.T) {
	freshLogins(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/moved":
			w.Header().Set("Location", "/elsewhere?code=secret-code")
			w.WriteHeader(http.StatusFound)
		default:
			_, _ = io.WriteString(w, "nothing to see, secret-text")
		}
	}))
	defer server.Close()
	for name, tc := range map[string]struct {
		steps []model.LoginStep
		want  string
	}{
		"a redirect":                      {[]model.LoginStep{{URL: "/moved", Extract: map[string]model.LoginExtract{"token": {Header: "Location"}}}}, "login.steps[0] was answered with the redirect 302, which a login does not follow; list it in the step's status"},
		"a redirect status does not list": {[]model.LoginStep{{URL: "/moved", Status: []int{200}, Extract: map[string]model.LoginExtract{"token": {Header: "Location"}}}}, "login.steps[0] was answered with the redirect 302, which a login does not follow; list it in the step's status"},
		"no match":                        {[]model.LoginStep{{URL: "/page", Extract: map[string]model.LoginExtract{"token": {Regex: `token=(\w+)`}}}}, "login.steps[0].extract.token matched nothing in the answer"},
		"no JSON":                         {[]model.LoginStep{{URL: "/page", Extract: map[string]model.LoginExtract{"token": {JSON: ".token"}}}}, "login.steps[0].extract.token found an answer that is not JSON"},
	} {
		c := loginCollector(t, model.LoginConfig{Type: "steps", Steps: tc.steps, Token: "token"})
		_, err := FetchCollector(context.Background(), server.URL+"/data", c, RequestOverrides{}, nil)
		if err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), "secret") {
			t.Errorf("%s: %v\nwant %s", name, err, tc.want)
		}
		logins.reset()
	}
	// With the redirect listed, its Location is read.
	c := loginCollector(t, model.LoginConfig{Type: "steps", Steps: []model.LoginStep{{URL: "/moved", Status: []int{302}, Extract: map[string]model.LoginExtract{"token": {Header: "Location"}}}}, Token: "token", Header: "X-Auth-Token"})
	if _, err := FetchCollector(context.Background(), server.URL+"/data", c, RequestOverrides{}, nil); err != nil {
		t.Fatal(err)
	}
}

// A steps login is refused when it loads unless it is whole: steps, each
// reading only values given or read before it, a token read by them and by
// refresh_steps, keep with refresh_steps, and a value read from one place.
func TestAStepsLoginIsCheckedWhenItLoads(t *testing.T) {
	read := func(name string) map[string]model.LoginExtract {
		return map[string]model.LoginExtract{name: {JSON: "." + name}}
	}
	ok := model.LoginConfig{Type: "steps", Steps: []model.LoginStep{{URL: "/a", Extract: read("tok")}}, Token: "tok"}
	if err := validateLogin(&ok, nil, "request.login", nil); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		edit func(l *model.LoginConfig)
		want string
	}{
		"a url":                 {func(l *model.LoginConfig) { l.URL = "/login" }, "request.login sets url; a steps login sends each step to its own url"},
		"no steps":              {func(l *model.LoginConfig) { l.Steps = nil }, "request.login has no steps"},
		"no token":              {func(l *model.LoginConfig) { l.Token = "" }, "request.login has no token"},
		"a token no step reads": {func(l *model.LoginConfig) { l.Token = "other" }, `request.login.token is "other", which no step of steps reads`},
		"a value read before it is given": {func(l *model.LoginConfig) {
			l.Steps = []model.LoginStep{{URL: "/a", Form: map[string]string{"x": "{{login.tok}}"}, Extract: read("tok")}}
		}, "request.login.steps[0].form.x holds {{login.tok}}, which is not a value given or read by a step before it"},
		"a json key":                    {func(l *model.LoginConfig) { l.TokenHeader = "X-Token" }, "request.login sets token_header, which a login of type steps does not take"},
		"two places":                    {func(l *model.LoginConfig) { l.Steps[0].Extract["tok"] = model.LoginExtract{JSON: ".a", Header: "X"} }, "request.login.steps[0].extract.tok sets 2 of regex, json, form, header and cookie"},
		"a regex that does not compile": {func(l *model.LoginConfig) { l.Steps[0].Extract["tok"] = model.LoginExtract{Regex: "("} }, "request.login.steps[0].extract.tok.regex does not compile"},
		"keep alone":                    {func(l *model.LoginConfig) { l.Keep = map[string]model.Duration{"tok": 0} }, "keep names the token"},
		"refresh_steps alone":           {func(l *model.LoginConfig) { l.RefreshSteps = []model.LoginStep{{URL: "/r", Extract: read("tok")}} }, "has refresh_steps and no keep"},
		"a Cookie header":               {func(l *model.LoginConfig) { l.Steps[0].Headers = map[string]string{"Cookie": "a=b"} }, "request.login.steps[0].headers sets Cookie, which the step sets itself"},
		"an Accept-Encoding header":     {func(l *model.LoginConfig) { l.Steps[0].Headers = map[string]string{"accept-encoding": "gzip"} }, "request.login.steps[0].headers sets Accept-Encoding, which the step sets itself"},
		"a Connection header":           {func(l *model.LoginConfig) { l.Steps[0].Headers = map[string]string{"Connection": "close"} }, "request.login.steps[0].headers sets Connection, which the step sets itself"},
		"half an oauth1 token": {func(l *model.LoginConfig) {
			l.Steps[0].OAuth1 = &model.LoginOAuth1{ConsumerKey: "k", ConsumerSecret: "s", Token: "t"}
		}, "request.login.steps[0].oauth1 sets one of token and token_secret"},
		"a body with GET":       {func(l *model.LoginConfig) { l.Steps[0].Method = "GET"; l.Steps[0].Form = map[string]string{"a": "b"} }, "sends a body with GET"},
		"a status out of range": {func(l *model.LoginConfig) { l.Steps[0].Status = []int{99} }, "request.login.steps[0].status[0] is 99, which is no HTTP status"},
		"relogin_on a 5xx":      {func(l *model.LoginConfig) { l.ReloginOn = []int{503} }, "request.login.relogin_on[0] is 503"},
		"a relative keep_file":  {func(l *model.LoginConfig) { l.KeepFile = "keep.json" }, `request.login.keep_file "keep.json" is not an absolute path`},
		"a value named twice": {func(l *model.LoginConfig) {
			l.Values = map[string]string{"user": "a"}
			l.SecretFiles = map[string]string{"user": "/f"}
		}, "request.login names user in both values and secret_files"},
		"a refresh with a value it cannot have": {func(l *model.LoginConfig) {
			l.Steps = []model.LoginStep{{URL: "/a", Extract: map[string]model.LoginExtract{"tok": {JSON: ".t"}, "long": {JSON: ".l"}, "short": {JSON: ".s"}}}}
			l.Keep = map[string]model.Duration{"long": 0}
			l.RefreshSteps = []model.LoginStep{{URL: "/r", Query: map[string]string{"s": "{{login.short}}"}, Extract: read("tok")}}
		}, "request.login.refresh_steps[0].query.s holds {{login.short}}, which is not a value given or read by a step before it"},
	} {
		l := ok
		l.Steps = []model.LoginStep{{URL: "/a", Extract: read("tok")}}
		tc.edit(&l)
		if err := validateLogin(&l, nil, "request.login", nil); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v\nwant %s", name, err, tc.want)
		}
	}
	// A json login takes no key of a steps login.
	json := model.LoginConfig{Type: "json", URL: "/login", Token: ".t", FieldFiles: map[string]string{"p": "/f"}, KeepFile: "/k"}
	if err := validateLogin(&json, nil, "request.login", nil); err == nil || !strings.Contains(err.Error(), "sets keep_file, which a login of type json does not take") {
		t.Fatalf("a json login with keep_file: %v", err)
	}
}

// A field of a form-encoded answer is read whatever else the answer holds,
// a pair Go's parser refuses among them.
func TestAFormFieldIsReadBesideAPairThatIsNoForms(t *testing.T) {
	resp := &http.Response{Header: http.Header{}}
	value, err := extractLoginValue(context.Background(), model.LoginExtract{Form: "oauth_token"}, resp, []byte("a=b;c&oauth_token=t1&oauth_token_secret=ts1\n"), time.Now())
	if err != nil || value.Text != "t1" {
		t.Fatalf("read %+v, %v", value, err)
	}
	if _, err := extractLoginValue(context.Background(), model.LoginExtract{Form: "missing"}, resp, []byte("a=b"), time.Now()); err == nil || !strings.Contains(err.Error(), "found nothing") {
		t.Fatalf("a field not there: %v", err)
	}
}

// A keep file forgets a login of which nothing would be used, its token
// and its values kept all expired, when another is written to it, and
// keeps one with anything left to use.
func TestAKeepFileForgetsWhatExpired(t *testing.T) {
	freshLogins(t)
	path := filepath.Join(t.TempDir(), "keep.json")
	now := loginNow()
	keepFiles.write(path, "spent", keptLogin{Token: "a", Expires: now.Add(-time.Minute), Kept: map[string]loginValue{"r": {Text: "r", Expires: now.Add(-time.Second)}}})
	keepFiles.write(path, "refreshable", keptLogin{Token: "b", Expires: now.Add(-time.Minute), Kept: map[string]loginValue{"r": {Text: "r", Expires: now.Add(time.Hour)}}})
	keepFiles.write(path, "unbounded", keptLogin{Token: "c"})
	keepFiles.write(path, "new", keptLogin{Token: "d", Expires: now.Add(time.Hour)})
	file, err := readKeepFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]bool{"spent": false, "refreshable": true, "unbounded": true, "new": true} {
		if _, ok := file.Logins[keepDigest(key)]; ok != want {
			t.Errorf("%s kept: %v, want %v", key, ok, want)
		}
	}
}

// The values kept are this exporter's when the keep file holds its own last
// token: a write that failed leaves older values in the file, which a
// refresh would send.
func TestTheValuesKeptAreThisExportersAfterAFailedWrite(t *testing.T) {
	advance := freshLogins(t)
	server := newSSOServer(t)
	l := garminLike(t)
	l.KeepFile = filepath.Join(t.TempDir(), "keep.json")
	c := loginCollector(t, l)
	fetch := func() {
		t.Helper()
		if response, err := FetchCollector(context.Background(), server.URL+"/data", c, RequestOverrides{}, nil); err != nil || response.StatusCode != http.StatusOK {
			t.Fatalf("fetch: %v, %+v", err, response)
		}
	}
	fetch()
	stepsStates.mu.Lock()
	var key string
	for k := range stepsStates.byKey {
		key = k
	}
	stepsStates.mu.Unlock()
	// The file as an earlier write left it: this exporter's token, values
	// kept that the target no longer takes.
	keepFiles.write(l.KeepFile, key, keptLogin{Token: "bearer-1", Expires: loginNow().Add(time.Hour), Kept: map[string]loginValue{"oauth_token": {Text: "stale"}, "oauth_token_secret": {Text: "stale"}}})
	advance(55 * time.Minute)
	fetch()
	if server.hit("/refresh") != 1 || server.hit("/exchange") != 1 {
		t.Fatalf("%d refreshes, %d exchanges; want the refresh made with the values this exporter kept", server.hit("/refresh"), server.hit("/exchange"))
	}
}

// refresh_steps may read a value kept anew, as an endpoint that rotates its
// refresh tokens answers each refresh with the next one, and the value read
// replaces the one kept: the next refresh sends it. A session cookie read
// by a step is sent back with header Cookie and its name as the prefix.
func TestRefreshStepsMayReadAValueKeptAnew(t *testing.T) {
	advance := freshLogins(t)
	var mu sync.Mutex
	refresh, minted := "r1", 0
	var refreshes atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/login":
			minted++
			http.SetCookie(w, &http.Cookie{Name: "SESSION", Value: fmt.Sprintf("s%d", minted)})
			_, _ = fmt.Fprintf(w, `{"refresh": %q}`, refresh)
		case "/refresh":
			refreshes.Add(1)
			if r.URL.Query().Get("refresh") != refresh {
				http.Error(w, "an old refresh token", http.StatusUnauthorized)
				return
			}
			minted++
			refresh = fmt.Sprintf("r%d", minted)
			http.SetCookie(w, &http.Cookie{Name: "SESSION", Value: fmt.Sprintf("s%d", minted), MaxAge: 3600})
			_, _ = fmt.Fprintf(w, `{"refresh": %q}`, refresh)
		default:
			if cookie, err := r.Cookie("SESSION"); err != nil || cookie.Value != fmt.Sprintf("s%d", minted) {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = io.WriteString(w, "value=1")
		}
	}))
	defer server.Close()
	prefix := "SESSION="
	c := loginCollector(t, model.LoginConfig{
		Type:         "steps",
		Steps:        []model.LoginStep{{URL: "/login", Extract: map[string]model.LoginExtract{"session": {Cookie: "SESSION"}, "refresh": {JSON: ".refresh"}}}},
		Keep:         map[string]model.Duration{"refresh": 0},
		RefreshSteps: []model.LoginStep{{URL: "/refresh", Query: map[string]string{"refresh": "{{login.refresh}}"}, Extract: map[string]model.LoginExtract{"session": {Cookie: "SESSION"}, "refresh": {JSON: ".refresh"}}}},
		Token:        "session",
		TTL:          model.Duration(time.Hour),
		Header:       "Cookie",
		Prefix:       &prefix,
	})
	for range 3 {
		if response, err := FetchCollector(context.Background(), server.URL+"/data", c, RequestOverrides{}, nil); err != nil || response.StatusCode != http.StatusOK {
			t.Fatalf("fetch: %v, %+v", err, response)
		}
		advance(59 * time.Minute)
	}
	if made, failed := logins.counts("api"); made != 3 || failed != 0 || refreshes.Load() != 2 || logins.refreshes("api") != 2 {
		t.Fatalf("%d logins, %d failed, %d refreshes asked, %d counted", made, failed, refreshes.Load(), logins.refreshes("api"))
	}
	// A value given by values may not be read: only a value kept may.
	given := model.LoginConfig{Type: "steps", Values: map[string]string{"user": "me"},
		Steps: []model.LoginStep{{URL: "/login", Extract: map[string]model.LoginExtract{"session": {Cookie: "SESSION"}, "user": {JSON: ".user"}}}}, Token: "session"}
	if err := validateLogin(&given, nil, "request.login", nil); err == nil || !strings.Contains(err.Error(), "extract reads user, which values gives") {
		t.Fatalf("a values name read by a step: %v", err)
	}
}

// A cookie the answer deletes is not read: one with Max-Age below 0 or an
// Expires past, as a site sends when it ends an old session, beside the new
// one.
func TestADeletedCookieIsNotRead(t *testing.T) {
	start := time.Date(2026, 11, 9, 8, 0, 0, 0, time.UTC)
	resp := &http.Response{Header: http.Header{"Set-Cookie": {
		"SESSION=old; Max-Age=-1",
		"SESSION=older; Expires=Thu, 01 Jan 1970 00:00:00 GMT",
		"SESSION=new; Max-Age=600",
	}}}
	value, err := extractLoginValue(context.Background(), model.LoginExtract{Cookie: "SESSION"}, resp, nil, start)
	if err != nil || value.Text != "new" || !value.Expires.Equal(start.Add(10*time.Minute)) {
		t.Fatalf("read %+v, %v", value, err)
	}
	gone := &http.Response{Header: http.Header{"Set-Cookie": {"SESSION=old; Max-Age=-1"}}}
	if _, err := extractLoginValue(context.Background(), model.LoginExtract{Cookie: "SESSION"}, gone, nil, start); err == nil {
		t.Fatal("a deleted cookie was read")
	}
}

// A steps login whose first step goes to an identity provider, absolute,
// and a later one to the target, a path, keeps a token for each target:
// two targets asked in turn make one login each, not one each time the
// other's token is refused.
func TestAStepsLoginWithAStepAtTheTargetKeepsATokenForEachTarget(t *testing.T) {
	freshLogins(t)
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"ticket": "t"}`)
	}))
	defer idp.Close()
	target := func(name string) *httptest.Server {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/session" {
				_, _ = fmt.Fprintf(w, `{"token": %q}`, name)
				return
			}
			if r.Header.Get("Authorization") != "Bearer "+name {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = io.WriteString(w, "value=1")
		}))
		t.Cleanup(server.Close)
		return server
	}
	one, two := target("one"), target("two")
	c := loginCollector(t, model.LoginConfig{Type: "steps", Token: "token", Steps: []model.LoginStep{
		{URL: idp.URL + "/ticket", Extract: map[string]model.LoginExtract{"ticket": {JSON: ".ticket"}}},
		{URL: "/session", Query: map[string]string{"ticket": "{{login.ticket}}"}, Extract: map[string]model.LoginExtract{"token": {JSON: ".token"}}},
	}})
	for range 2 {
		for _, server := range []*httptest.Server{one, two} {
			if response, err := FetchCollector(context.Background(), server.URL+"/data", c, RequestOverrides{}, nil); err != nil || response.StatusCode != http.StatusOK {
				t.Fatalf("fetch: %v, %+v", err, response)
			}
		}
	}
	if made, _ := logins.counts("api"); made != 2 {
		t.Fatalf("%d logins for two targets asked twice each, want 2", made)
	}
}
