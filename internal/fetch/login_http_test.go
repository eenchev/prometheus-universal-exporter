//go:build !select_request_types || request_type_http

package fetch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// size reports how many tokens c keeps.
func (c *loginCache) size() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

// freshLogins forgets every token and count kept, before and after the
// test, and gives the test a clock of its own, which it moves with the
// function returned.
func freshLogins(t *testing.T) (advance func(time.Duration)) {
	t.Helper()
	logins.reset()
	var offset atomic.Int64
	loginNow = func() time.Time { return time.Now().Add(time.Duration(offset.Load())) }
	t.Cleanup(func() {
		loginNow = time.Now
		logins.reset()
	})
	return func(d time.Duration) { offset.Add(int64(d)) }
}

// loginCollector is an http collector logging in with l, validated.
func loginCollector(t *testing.T, l model.LoginConfig) *model.Collector {
	t.Helper()
	c := testutil.Collector("api", "text")
	c.Request.Login = &l
	if err := ValidateRequest(&c); err != nil {
		t.Fatal(err)
	}
	return &c
}

// secretFiles writes each secret to a file of its own and returns the
// field_files naming them.
func secretFiles(t *testing.T, secrets map[string]string) map[string]string {
	t.Helper()
	files := map[string]string{}
	for name, value := range secrets {
		files[name] = testutil.WriteFile(t, name, value+"\n")
	}
	return files
}

// loginServer is a target that answers its login at /login, with the body
// answer gives for the login's JSON body, and answers /data with value=1
// to a request carrying the header want gives, 401 otherwise.
type loginServer struct {
	*httptest.Server
	logins   atomic.Int64
	requests atomic.Int64
	mu       sync.Mutex
	bodies   []map[string]string
}

func newLoginServer(t *testing.T, answer func(w http.ResponseWriter, n int64), accepted func(r *http.Request) bool) *loginServer {
	t.Helper()
	s := &loginServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/login":
			n := s.logins.Add(1)
			var body map[string]string
			if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" || json.NewDecoder(r.Body).Decode(&body) != nil {
				http.Error(w, "not a JSON login", http.StatusBadRequest)
				return
			}
			s.mu.Lock()
			s.bodies = append(s.bodies, body)
			s.mu.Unlock()
			answer(w, n)
		default:
			s.requests.Add(1)
			if !accepted(r) {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = io.WriteString(w, "value=1")
		}
	}))
	t.Cleanup(s.Close)
	return s
}

// tokenAnswer answers a login with the token tok<n>, the login's number,
// lasting expiresIn seconds when it is above 0.
func tokenAnswer(expiresIn int) func(w http.ResponseWriter, n int64) {
	return func(w http.ResponseWriter, n int64) {
		answer := map[string]any{"access_token": fmt.Sprintf("tok%d", n)}
		if expiresIn > 0 {
			answer["expires_in"] = expiresIn
		}
		_ = json.NewEncoder(w).Encode(answer)
	}
}

func bearer(token string) func(r *http.Request) bool {
	return func(r *http.Request) bool { return r.Header.Get("Authorization") == "Bearer "+token }
}

// A json login posts its fields, those from files read and trimmed, reads
// the token with its jq expression and sends it as a bearer token; the
// token is kept, so a second request makes no second login, and a login
// is counted for the collector.
func TestJSONLoginSendsTheTokenFromTheBodyAndKeepsIt(t *testing.T) {
	freshLogins(t)
	server := newLoginServer(t, tokenAnswer(3600), bearer("tok1"))
	c := loginCollector(t, model.LoginConfig{Type: "json", URL: "/login", Token: ".access_token", ExpiresIn: ".expires_in",
		Fields: map[string]string{"tenant": "acme"}, FieldFiles: secretFiles(t, map[string]string{"username": "metrics", "password": "s3cret"})})
	for range 2 {
		response, err := FetchCollector(context.Background(), server.URL+"/data", c, RequestOverrides{}, nil)
		if err != nil || response.StatusCode != http.StatusOK {
			t.Fatalf("fetch: %v, %+v", err, response)
		}
	}
	if n := server.logins.Load(); n != 1 {
		t.Errorf("made %d logins for two requests, want 1: the token is kept", n)
	}
	want := map[string]string{"tenant": "acme", "username": "metrics", "password": "s3cret"}
	if fmt.Sprint(server.bodies[0]) != fmt.Sprint(want) {
		t.Errorf("the login posted %v, want %v", server.bodies[0], want)
	}
	if made, failed := logins.counts("api"); made != 1 || failed != 0 {
		t.Errorf("counted %d logins, %d failed; want 1 and 0", made, failed)
	}
}

// A token read from a header of the login's answer is sent in the header
// login.header names, after login.prefix; one read from a cookie the login
// sets is sent back as that cookie, and lasts as long as the cookie's
// Max-Age.
func TestJSONLoginTakesTheTokenFromAHeaderOrACookie(t *testing.T) {
	advance := freshLogins(t)
	empty := ""
	header := newLoginServer(t, func(w http.ResponseWriter, n int64) {
		w.Header().Set("X-Subject-Token", fmt.Sprintf("subject%d", n))
		w.WriteHeader(http.StatusCreated)
	}, func(r *http.Request) bool {
		return r.Header.Get("X-Auth-Token") == "subject1" && r.Header.Get("Authorization") == ""
	})
	c := loginCollector(t, model.LoginConfig{Type: "json", URL: "/login", TokenHeader: "x-subject-token", Header: "X-Auth-Token", Prefix: &empty, FieldFiles: secretFiles(t, map[string]string{"password": "p"})})
	if response, err := FetchCollector(context.Background(), header.URL+"/data", c, RequestOverrides{}, nil); err != nil || response.StatusCode != http.StatusOK {
		t.Errorf("token from a header: %v, %+v", err, response)
	}

	cookie := newLoginServer(t, func(w http.ResponseWriter, n int64) {
		http.SetCookie(w, &http.Cookie{Name: "other", Value: "x"})
		http.SetCookie(w, &http.Cookie{Name: "SESSION", Value: fmt.Sprintf("s%d", n), MaxAge: 600})
	}, func(r *http.Request) bool {
		session, err := r.Cookie("SESSION")
		return err == nil && strings.HasPrefix(session.Value, "s") && r.Header.Get("Authorization") == ""
	})
	c = loginCollector(t, model.LoginConfig{Type: "json", URL: "/login", TokenCookie: "SESSION", FieldFiles: secretFiles(t, map[string]string{"password": "p"})})
	for range 2 {
		if response, err := FetchCollector(context.Background(), cookie.URL+"/data", c, RequestOverrides{}, nil); err != nil || response.StatusCode != http.StatusOK {
			t.Fatalf("token from a cookie: %v, %+v", err, response)
		}
	}
	if n := cookie.logins.Load(); n != 1 {
		t.Errorf("made %d cookie logins, want 1", n)
	}
	// Ten minutes less a tenth of them, and the session is fetched again.
	advance(9*time.Minute + time.Second)
	if _, err := FetchCollector(context.Background(), cookie.URL+"/data", c, RequestOverrides{}, nil); err != nil {
		t.Fatal(err)
	}
	if n := cookie.logins.Load(); n != 2 {
		t.Errorf("made %d cookie logins once the cookie's Max-Age was nearly over, want 2", n)
	}
}

// A token is fetched again before it expires: at 100 seconds, 30 seconds
// before, since a tenth of its life is less. A login that fails then, while
// the token has not expired, leaves the token in use and is counted as a
// failure; once it has expired, a failing login fails the request in the
// login stage.
func TestALoginIsMadeAgainBeforeTheTokenExpires(t *testing.T) {
	advance := freshLogins(t)
	var failing atomic.Bool
	server := newLoginServer(t, func(w http.ResponseWriter, n int64) {
		if failing.Load() {
			http.Error(w, "the login service is down", http.StatusServiceUnavailable)
			return
		}
		tokenAnswer(100)(w, n)
	}, func(r *http.Request) bool { return strings.HasPrefix(r.Header.Get("Authorization"), "Bearer tok") })
	c := loginCollector(t, model.LoginConfig{Type: "json", URL: server.URL + "/login", Token: ".access_token", ExpiresIn: ".expires_in", FieldFiles: secretFiles(t, map[string]string{"password": "p"})})
	get := func() error {
		_, err := FetchCollector(context.Background(), server.URL+"/data", c, RequestOverrides{}, nil)
		return err
	}
	if err := get(); err != nil {
		t.Fatal(err)
	}
	advance(69 * time.Second)
	if err := get(); err != nil || server.logins.Load() != 1 {
		t.Fatalf("at 69s: %v, %d logins; want the token kept", err, server.logins.Load())
	}
	advance(2 * time.Second)
	if err := get(); err != nil || server.logins.Load() != 2 {
		t.Fatalf("at 71s: %v, %d logins; want a second login, 30s before the token expires", err, server.logins.Load())
	}
	failing.Store(true)
	advance(71 * time.Second)
	if err := get(); err != nil {
		t.Fatalf("a failed refresh with the token still valid: %v; want the token kept in use", err)
	}
	if made, failed := logins.counts("api"); made != 3 || failed != 1 {
		t.Errorf("counted %d logins, %d failed; want 3 and 1", made, failed)
	}
	advance(30 * time.Second)
	err := get()
	var stage *StageError
	if err == nil || !errors.As(err, &stage) || stage.Stage != "login" || !strings.Contains(err.Error(), "HTTP status 503") {
		t.Errorf("a failed login with the token expired: %v; want a login stage failure naming the status", err)
	}
}

// A target that answers 401 has the token dropped and the request sent
// once more with a new one, whatever retry.attempts says, POST included;
// one that refuses the new token too is answered with its 401, and no third
// request is sent.
func TestA401IsAnsweredWithOneNewLogin(t *testing.T) {
	freshLogins(t)
	server := newLoginServer(t, tokenAnswer(0), bearer("tok2"))
	c := loginCollector(t, model.LoginConfig{Type: "json", URL: "/login", Token: ".access_token", FieldFiles: secretFiles(t, map[string]string{"password": "p"})})
	editHTTP(c, func(r *HTTPConfig) { r.Method = http.MethodPost })
	response, err := FetchCollector(context.Background(), server.URL+"/data", c, RequestOverrides{}, nil)
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("fetch: %v, %+v", err, response)
	}
	if server.logins.Load() != 2 || server.requests.Load() != 2 {
		t.Errorf("made %d logins and %d requests, want 2 of each", server.logins.Load(), server.requests.Load())
	}

	logins.reset()
	refusing := newLoginServer(t, tokenAnswer(0), func(*http.Request) bool { return false })
	response, err = FetchCollector(context.Background(), refusing.URL+"/data", c, RequestOverrides{}, nil)
	if err != nil || response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("fetch: %v, %+v; want the 401", err, response)
	}
	if refusing.logins.Load() != 2 || refusing.requests.Load() != 2 {
		t.Errorf("made %d logins and %d requests of a target refusing every token, want 2 of each", refusing.logins.Load(), refusing.requests.Load())
	}
}

// A login that fails fails the request in the login stage, saying the
// status and the start of the answer, with what reads as a credential
// masked; the secrets the login sent are never in the error.
func TestAFailedLoginSaysWhyWithoutTheSecrets(t *testing.T) {
	freshLogins(t)
	server := newLoginServer(t, func(w http.ResponseWriter, _ int64) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error": "locked", "password": "hunter2-echoed"}`)
	}, func(*http.Request) bool { return true })
	c := loginCollector(t, model.LoginConfig{Type: "json", URL: "/login", Token: ".access_token", FieldFiles: secretFiles(t, map[string]string{"username": "metrics-user", "password": "hunter2-echoed"})})
	_, err := FetchCollector(context.Background(), server.URL+"/data", c, RequestOverrides{}, nil)
	var stage *StageError
	if err == nil || !errors.As(err, &stage) || stage.Stage != "login" {
		t.Fatalf("got %v, want a login stage failure", err)
	}
	if !strings.Contains(err.Error(), "HTTP status 403") || !strings.Contains(err.Error(), "locked") {
		t.Errorf("the error %q does not say the status and the answer", err)
	}
	if strings.Contains(err.Error(), "hunter2") || strings.Contains(err.Error(), "metrics-user") {
		t.Errorf("the error %q holds a secret of the login", err)
	}
	if server.requests.Load() != 0 {
		t.Error("the target was asked though the login failed")
	}
	// A token expression that fails says so without quoting what gojq
	// would: the answer's values.
	logins.reset()
	leaky := newLoginServer(t, func(w http.ResponseWriter, _ int64) {
		_, _ = io.WriteString(w, `{"access_token": "do-not-show-me"}`)
	}, func(*http.Request) bool { return true })
	c = loginCollector(t, model.LoginConfig{Type: "json", URL: "/login", Token: ".access_token.value", FieldFiles: secretFiles(t, map[string]string{"password": "p"})})
	_, err = FetchCollector(context.Background(), leaky.URL+"/data", c, RequestOverrides{}, nil)
	if err == nil || strings.Contains(err.Error(), "do-not-show-me") {
		t.Errorf("a token expression that fails: %v; want an error without the answer's values", err)
	}
}

// A login follows no redirect, and a redirect is named as one.
func TestALoginFollowsNoRedirect(t *testing.T) {
	freshLogins(t)
	var elsewhere atomic.Int64
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { elsewhere.Add(1) }))
	defer other.Close()
	server := newLoginServer(t, func(w http.ResponseWriter, _ int64) {
		w.Header().Set("Location", other.URL+"/login")
		w.WriteHeader(http.StatusTemporaryRedirect)
	}, func(*http.Request) bool { return true })
	c := loginCollector(t, model.LoginConfig{Type: "json", URL: "/login", Token: ".access_token", FieldFiles: secretFiles(t, map[string]string{"password": "p"})})
	c.Request.FollowRedirects = true
	_, err := FetchCollector(context.Background(), server.URL+"/data", c, RequestOverrides{}, nil)
	if err == nil || !strings.Contains(err.Error(), "redirect 307") {
		t.Errorf("got %v, want the redirect named", err)
	}
	if elsewhere.Load() != 0 {
		t.Error("the login followed the redirect")
	}
}

// Requests that need a token while it is being fetched wait for the one
// login.
func TestConcurrentRequestsShareOneLogin(t *testing.T) {
	freshLogins(t)
	release := make(chan struct{})
	server := newLoginServer(t, func(w http.ResponseWriter, n int64) {
		<-release
		tokenAnswer(0)(w, n)
	}, bearer("tok1"))
	c := loginCollector(t, model.LoginConfig{Type: "json", URL: "/login", Token: ".access_token", FieldFiles: secretFiles(t, map[string]string{"password": "p"})})
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for range 20 {
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			response, err := FetchCollector(ctx, server.URL+"/data", c, RequestOverrides{}, nil)
			if err == nil && response.StatusCode != http.StatusOK {
				err = fmt.Errorf("status %d", response.StatusCode)
			}
			errs <- err
		})
	}
	time.Sleep(100 * time.Millisecond)
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
	if n := server.logins.Load(); n != 1 {
		t.Errorf("made %d logins for 20 requests at once, want 1", n)
	}
}

// A relative login URL is resolved against each target, which is logged
// in to on its own; an absolute one goes where it says, held to the
// collector's denied_targets as a target is, and refused as a login, not
// as the target.
func TestALoginURLIsResolvedAgainstTheTargetAndHeldToThePolicy(t *testing.T) {
	freshLogins(t)
	first := newLoginServer(t, tokenAnswer(0), bearer("tok1"))
	second := newLoginServer(t, tokenAnswer(0), bearer("tok1"))
	c := loginCollector(t, model.LoginConfig{Type: "json", URL: "/login", Token: ".access_token", FieldFiles: secretFiles(t, map[string]string{"password": "p"})})
	for _, server := range []*loginServer{first, second} {
		if response, err := FetchCollector(context.Background(), "http://user:pass@"+strings.TrimPrefix(server.URL, "http://")+"/data?x=1", c, RequestOverrides{}, nil); err != nil || response.StatusCode != http.StatusOK {
			t.Fatalf("fetch: %v, %+v", err, response)
		}
	}
	if first.logins.Load() != 1 || second.logins.Load() != 1 {
		t.Errorf("logged in %d and %d times, want once to each target", first.logins.Load(), second.logins.Load())
	}

	logins.reset()
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "value=1") }))
	defer target.Close()
	denied := testutil.Collector("api", "text")
	denied.Request.Login = &model.LoginConfig{Type: "json", URL: "http://idp.invalid/login", Token: ".access_token", FieldFiles: secretFiles(t, map[string]string{"password": "p"})}
	denied.Request.DeniedTargets = []string{"idp.invalid"}
	if err := ValidateRequest(&denied); err != nil {
		t.Fatal(err)
	}
	_, err := FetchCollector(context.Background(), target.URL, &denied, RequestOverrides{}, nil)
	var stage *StageError
	if err == nil || !errors.As(err, &stage) || stage.Stage != "login" || errors.Is(err, ErrTargetRefused) || !strings.Contains(err.Error(), "idp.invalid") {
		t.Errorf("got %v, want a login stage failure for the refused login URL, not the target's refusal", err)
	}
}

// A login is made with login.tls, or the collector's tls, never with the
// insecure_skip_verify a probe or a static target sets for the target.
func TestALoginIgnoresTheProbesInsecureSkipVerify(t *testing.T) {
	freshLogins(t)
	idp := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { tokenAnswer(0)(w, 1) }))
	defer idp.Close()
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "value=1") }))
	defer target.Close()
	c := loginCollector(t, model.LoginConfig{Type: "json", URL: idp.URL + "/login", Token: ".access_token", FieldFiles: secretFiles(t, map[string]string{"password": "p"})})
	insecure := true
	_, err := FetchCollector(context.Background(), target.URL, c, RequestOverrides{InsecureSkipVerify: &insecure}, nil)
	if err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Errorf("got %v, want the login refused for the server's certificate", err)
	}
	c.Request.Login.TLS = &model.TLSConfig{InsecureSkipVerify: true}
	logins.reset()
	if _, err := FetchCollector(context.Background(), target.URL, c, RequestOverrides{}, nil); err != nil {
		t.Errorf("with login.tls.insecure_skip_verify: %v", err)
	}
}

// An oauth2 login asks the token endpoint with the client credentials
// grant: the client's id and secret as a Basic credential by default, as
// form parameters with auth_style params, with the scopes and the endpoint
// parameters, and the token is kept for its expires_in.
func TestOAuth2LoginUsesTheClientCredentialsGrant(t *testing.T) {
	for _, style := range []string{"", "params"} {
		t.Run("auth_style="+style, func(t *testing.T) {
			advance := freshLogins(t)
			var tokens atomic.Int64
			var problems []string
			var mu sync.Mutex
			problem := func(format string, args ...any) {
				mu.Lock()
				defer mu.Unlock()
				problems = append(problems, fmt.Sprintf(format, args...))
			}
			idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n := tokens.Add(1)
				if err := r.ParseForm(); err != nil {
					problem("%v", err)
				}
				user, pass, basic := r.BasicAuth()
				switch style {
				case "params":
					if basic || r.PostForm.Get("client_id") != "exporter" || r.PostForm.Get("client_secret") != "very-secret" {
						problem("params style: basic %v, form %v", basic, r.PostForm)
					}
				default:
					if user != "exporter" || pass != "very-secret" || r.PostForm.Get("client_secret") != "" {
						problem("header style: %q %q, form %v", user, pass, r.PostForm)
					}
				}
				if r.PostForm.Get("grant_type") != "client_credentials" || r.PostForm.Get("scope") != "metrics:read admin" || r.PostForm.Get("audience") != "api" {
					problem("form %v", r.PostForm)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprintf(w, `{"access_token": "at%d", "token_type": "Bearer", "expires_in": 300}`, n)
			}))
			defer idp.Close()
			var refuseFirst atomic.Bool
			refuseFirst.Store(true)
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") == "Bearer at1" && refuseFirst.Load() {
					refuseFirst.Store(false)
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer at") {
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				_, _ = io.WriteString(w, "value=1")
			}))
			defer target.Close()
			files := secretFiles(t, map[string]string{"secret": "very-secret"})
			c := loginCollector(t, model.LoginConfig{Type: "oauth2", URL: idp.URL + "/oauth/token", ClientID: "exporter", ClientSecretFile: files["secret"],
				Scopes: []string{"metrics:read", "admin"}, EndpointParams: map[string]string{"audience": "api"}, AuthStyle: style})
			for range 2 {
				if response, err := FetchCollector(context.Background(), target.URL, c, RequestOverrides{}, nil); err != nil || response.StatusCode != http.StatusOK {
					t.Fatalf("fetch: %v, %+v", err, response)
				}
			}
			if n := tokens.Load(); n != 2 {
				t.Errorf("asked for %d tokens, want 2: one, and one more after the target's 401", n)
			}
			advance(271 * time.Second)
			if _, err := FetchCollector(context.Background(), target.URL, c, RequestOverrides{}, nil); err != nil {
				t.Fatal(err)
			}
			if n := tokens.Load(); n != 3 {
				t.Errorf("asked for %d tokens once 30s of the token's 300 were left, want 3", n)
			}
			mu.Lock()
			defer mu.Unlock()
			for _, p := range problems {
				t.Error(p)
			}
		})
	}
}

// A token endpoint that refuses the client fails the request in the login
// stage with its status and RFC 6749 error code, and without the secret.
func TestAFailedOAuth2LoginSaysWhy(t *testing.T) {
	freshLogins(t)
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error": "invalid_client", "error_description": "client authentication failed"}`)
	}))
	defer idp.Close()
	files := secretFiles(t, map[string]string{"secret": "very-secret"})
	c := loginCollector(t, model.LoginConfig{Type: "oauth2", URL: idp.URL, ClientID: "exporter", ClientSecretFile: files["secret"]})
	_, err := FetchCollector(context.Background(), "http://127.0.0.1:1", c, RequestOverrides{}, nil)
	var stage *StageError
	if err == nil || !errors.As(err, &stage) || stage.Stage != "login" || !strings.Contains(err.Error(), "401, invalid_client: client authentication failed") || strings.Contains(err.Error(), "very-secret") {
		t.Errorf("got %v, want a login failure with the status and the code, without the secret", err)
	}
	if made, failed := logins.counts("api"); made != 1 || failed != 1 {
		t.Errorf("counted %d logins, %d failed; want 1 and 1", made, failed)
	}
}

// A static target's own credential in the login's header is sent instead,
// and no login is made; a static target's login replaces the collector's.
func TestAStaticTargetsCredentialOrLoginReplacesTheCollectors(t *testing.T) {
	freshLogins(t)
	server := newLoginServer(t, tokenAnswer(0), func(r *http.Request) bool {
		return r.Header.Get("Authorization") == "Bearer own" || r.Header.Get("Authorization") == "Bearer tok1"
	})
	c := loginCollector(t, model.LoginConfig{Type: "json", URL: "http://login.invalid/login", Token: ".access_token", FieldFiles: secretFiles(t, map[string]string{"password": "p"})})
	target := model.StaticTarget{Name: "a", Collector: "api", Target: server.URL + "/data", Request: model.TargetRequestConfig{BearerToken: "own"}}
	headers, err := TargetHeaders(&target)
	if err != nil {
		t.Fatal(err)
	}
	if response, err := FetchCollector(context.Background(), target.Target, c, TargetOverrides(&target), headers); err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("a target with its own bearer token: %v, %+v", err, response)
	}
	target = model.StaticTarget{Name: "b", Collector: "api", Target: server.URL + "/data", Request: model.TargetRequestConfig{Login: &model.LoginConfig{Type: "json", URL: "/login", Token: ".access_token", FieldFiles: secretFiles(t, map[string]string{"password": "q"})}}}
	if err := CheckTargetRequest(&target, c); err != nil {
		t.Fatal(err)
	}
	if response, err := FetchCollector(context.Background(), target.Target, c, TargetOverrides(&target), nil); err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("a target with its own login: %v, %+v", err, response)
	}
	if server.logins.Load() != 1 || server.bodies[0]["password"] != "q" {
		t.Errorf("logged in %d times with %v, want once with the target's login", server.logins.Load(), server.bodies)
	}
}

// What a login block may not say is refused when the configuration loads,
// naming the collector and the key, and never showing a secret.
func TestALoginBlockIsCheckedWhenItLoads(t *testing.T) {
	files := map[string]string{"password": "/run/secrets/password"}
	json := func(change func(l *model.LoginConfig)) model.LoginConfig {
		l := model.LoginConfig{Type: "json", URL: "/login", Token: ".token", FieldFiles: files}
		change(&l)
		return l
	}
	oauth := func(change func(l *model.LoginConfig)) model.LoginConfig {
		l := model.LoginConfig{Type: "oauth2", URL: "https://idp.example/token", ClientID: "x", ClientSecretFile: "/run/secrets/client"}
		change(&l)
		return l
	}
	for _, tc := range []struct {
		login   model.LoginConfig
		request func(r *model.RequestConfig)
		want    string
	}{
		{json(func(l *model.LoginConfig) { l.Type = "" }), nil, "has no type"},
		{json(func(l *model.LoginConfig) { l.Type = "form" }), nil, `type is "form"`},
		{json(func(l *model.LoginConfig) { l.URL = "" }), nil, "url is required"},
		{json(func(l *model.LoginConfig) { l.URL = "https://user:pw@idp.example/login" }), nil, "holds credentials"},
		{json(func(l *model.LoginConfig) { l.URL = "//idp.example/login" }), nil, "names a host without a scheme"},
		{json(func(l *model.LoginConfig) { l.URL = "/login/{{param_x}}" }), nil, "has a placeholder"},
		{json(func(l *model.LoginConfig) { l.URL = "ftp://idp.example/login" }), nil, `the scheme "ftp"`},
		{json(func(l *model.LoginConfig) { l.URL = "http://idp.example/login" }), func(r *model.RequestConfig) { r.AllowedSchemes = []string{"https"} }, "http"},
		{json(func(l *model.LoginConfig) { l.TokenHeader = "X-Token" }), nil, "sets 2 of token, token_header and token_cookie"},
		{json(func(l *model.LoginConfig) { l.Token = "" }), nil, "sets 0 of token"},
		{json(func(l *model.LoginConfig) { l.Token = ".[" }), nil, "does not compile"},
		{json(func(l *model.LoginConfig) { l.FieldFiles = nil }), nil, "has no field_files"},
		{json(func(l *model.LoginConfig) { l.Fields = map[string]string{"password": "x"} }), nil, `the field "password" in both`},
		{json(func(l *model.LoginConfig) { l.ClientID = "x" }), nil, "sets client_id, which a login of type json does not take"},
		{json(func(l *model.LoginConfig) { l.Header = "X-Tenant" }), nil, "does not read as a credential's name"},
		{json(func(l *model.LoginConfig) { l.Header = "Bad Header" }), nil, "is not a header name"},
		{json(func(l *model.LoginConfig) { l.Token, l.TokenCookie, l.Header = "", "SID", "X-Auth" }), nil, "sets token_cookie and header"},
		{json(func(l *model.LoginConfig) { l.TTL = -1 }), nil, "ttl must not be negative"},
		{json(func(*model.LoginConfig) {}), func(r *model.RequestConfig) { r.BearerTokenFile = "/run/token" }, "which both send Authorization"},
		{json(func(*model.LoginConfig) {}), func(r *model.RequestConfig) { r.Headers = map[string]string{"authorization": "x"} }, "sets request.headers authorization"},
		{oauth(func(l *model.LoginConfig) { l.ClientIDFile = "/run/id" }), nil, "exactly one of client_id and client_id_file"},
		{oauth(func(l *model.LoginConfig) { l.ClientSecretFile = "" }), nil, "needs client_secret_file"},
		{oauth(func(l *model.LoginConfig) { l.EndpointParams = map[string]string{"grant_type": "password"} }), nil, "sets grant_type"},
		{oauth(func(l *model.LoginConfig) { l.Scopes = []string{"a b"} }), nil, "is no scope"},
		{oauth(func(l *model.LoginConfig) { l.AuthStyle = "auto" }), nil, `auth_style is "auto"`},
		{oauth(func(l *model.LoginConfig) { l.Token = ".x" }), nil, "sets token, which a login of type oauth2 does not take"},
	} {
		c := testutil.Collector("api", "text")
		login := tc.login
		c.Request.Login = &login
		if tc.request != nil {
			tc.request(&c.Request)
		}
		err := ValidateRequest(&c)
		if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), `collector "api"`) {
			t.Errorf("%+v: got %v, want %q", tc.login, err, tc.want)
		}
	}
	// A login sent in a header of its own may stand beside a Basic
	// credential, which goes in Authorization.
	c := testutil.Collector("api", "text")
	c.Request.Login = &model.LoginConfig{Type: "json", URL: "/login", Token: ".token", FieldFiles: files, Header: "X-Auth-Token"}
	c.Request.BasicAuthFile = &model.BasicAuthFile{Username: "/run/u", Password: "/run/p"}
	if err := ValidateRequest(&c); err != nil {
		t.Errorf("a login in X-Auth-Token beside basic_auth_file: %v", err)
	}
	// A static target's login is held to the same, and to its own
	// credentials.
	target := model.StaticTarget{Name: "t", Collector: "api", Target: "http://a.example", Request: model.TargetRequestConfig{BearerToken: "own", Login: &model.LoginConfig{Type: "json", URL: "/login", Token: ".token", FieldFiles: files}}}
	if err := CheckTargetRequest(&target, &c); err == nil || !strings.Contains(err.Error(), "which both send Authorization") {
		t.Errorf("a target's login beside its bearer_token: %v", err)
	}
}

// A reload drops the tokens of a login the configuration no longer has, and
// the counts of a collector it no longer has.
func TestFollowingAConfigurationDropsTokensAndCounts(t *testing.T) {
	freshLogins(t)
	server := newLoginServer(t, tokenAnswer(0), bearer("tok1"))
	c := loginCollector(t, model.LoginConfig{Type: "json", URL: "/login", Token: ".access_token", FieldFiles: secretFiles(t, map[string]string{"password": "p"})})
	if _, err := FetchCollector(context.Background(), server.URL+"/data", c, RequestOverrides{}, nil); err != nil {
		t.Fatal(err)
	}
	logins.follow([]model.Collector{*c}, nil)
	if logins.size() != 1 {
		t.Fatalf("kept %d tokens of a collector in force, want 1", logins.size())
	}
	changed := *c
	changed.Request.Login = &model.LoginConfig{Type: "json", URL: "/login", Token: ".token", FieldFiles: c.Request.Login.FieldFiles}
	logins.follow([]model.Collector{changed}, nil)
	if logins.size() != 0 {
		t.Errorf("kept %d tokens of a login that changed, want 0", logins.size())
	}
	if made, _ := logins.counts("api"); made != 1 {
		t.Errorf("the collector's count is %d, want 1 kept", made)
	}
	logins.follow(nil, nil)
	if made, _ := logins.counts("api"); made != 0 {
		t.Errorf("the count of a removed collector is %d, want 0", made)
	}
}

// The tokens kept are bounded: past maxLoginEntries the one used longest
// ago is dropped.
func TestTheTokensKeptAreBounded(t *testing.T) {
	advance := freshLogins(t)
	login := func(context.Context) (loginToken, error) { return loginToken{value: "Bearer x"}, nil }
	for i := range maxLoginEntries + 5 {
		advance(time.Millisecond)
		if _, err := logins.get(context.Background(), strconv.Itoa(i), "api", "f", login); err != nil {
			t.Fatal(err)
		}
	}
	if n := logins.size(); n != maxLoginEntries {
		t.Errorf("kept %d tokens, want %d", n, maxLoginEntries)
	}
	logins.mu.Lock()
	_, first := logins.entries["0"]
	_, last := logins.entries[strconv.Itoa(maxLoginEntries+4)]
	logins.mu.Unlock()
	if first || !last {
		t.Errorf("kept the first: %v, the last: %v; want the oldest dropped", first, last)
	}
}

// A debug trace records a login request marked as one, before the
// request it was made for, and never its body.
func TestALoginRequestIsTraced(t *testing.T) {
	freshLogins(t)
	server := newLoginServer(t, tokenAnswer(0), bearer("tok1"))
	c := loginCollector(t, model.LoginConfig{Type: "json", URL: "/login", Token: ".access_token", FieldFiles: secretFiles(t, map[string]string{"password": "p"})})
	ctx, trace := WithRequestTrace(context.Background())
	if _, err := FetchCollector(ctx, server.URL+"/data", c, RequestOverrides{}, nil); err != nil {
		t.Fatal(err)
	}
	requests := trace.Requests()
	if len(requests) != 2 || !requests[0].Login || requests[1].Login || !strings.HasSuffix(requests[0].URL, "/login") || requests[0].Outcome != "200 OK" {
		t.Errorf("traced %+v, want the login and then the request", requests)
	}
}

// A shared login runs under its own bound, not the deadline of the request
// that began it: a request that gives up before the login ends leaves it to
// one still waiting, which gets the token within its own deadline, and the
// login is not counted as failed.
func TestASharedLoginOutlivesTheRequestThatBeganIt(t *testing.T) {
	freshLogins(t)
	server := newLoginServer(t, func(w http.ResponseWriter, n int64) {
		time.Sleep(300 * time.Millisecond)
		tokenAnswer(0)(w, n)
	}, bearer("tok1"))
	c := loginCollector(t, model.LoginConfig{Type: "json", URL: "/login", Token: ".access_token", FieldFiles: secretFiles(t, map[string]string{"password": "p"})})
	short, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	first := make(chan error, 1)
	go func() {
		_, err := FetchCollector(short, server.URL+"/data", c, RequestOverrides{}, nil)
		first <- err
	}()
	// The waiter asks once the first request's login is under way.
	for server.logins.Load() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	long, cancelLong := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelLong()
	response, err := FetchCollector(long, server.URL+"/data", c, RequestOverrides{}, nil)
	if err != nil || response.StatusCode != http.StatusOK {
		t.Errorf("the waiter got %v, %+v; want the token of the login the short request began", err, response)
	}
	if err := <-first; err == nil {
		t.Error("the 100ms request succeeded; want it to fail at its own deadline")
	}
	if made, failed := logins.counts("api"); server.logins.Load() != 1 || made != 1 || failed != 0 {
		t.Errorf("made %d logins, counted %d made and %d failed; want one login, not failed", server.logins.Load(), made, failed)
	}
}

// A relative login.url is a path from the root, resolved against the
// target's scheme, host and port alone: the target's path and query choose
// neither where the secrets go nor a token of their own. One relative to the
// target's path is refused at load.
func TestARelativeLoginURLKeepsOnlyTheTargetsOrigin(t *testing.T) {
	freshLogins(t)
	server := newLoginServer(t, tokenAnswer(0), bearer("tok1"))
	c := loginCollector(t, model.LoginConfig{Type: "json", URL: "/login", Token: ".access_token", FieldFiles: secretFiles(t, map[string]string{"password": "p"})})
	for _, path := range []string{"/~mallory/x/data", "/other/data?y=2", "/data"} {
		if response, err := FetchCollector(context.Background(), server.URL+path, c, RequestOverrides{}, nil); err != nil || response.StatusCode != http.StatusOK {
			t.Fatalf("fetch %s: %v, %+v", path, err, response)
		}
	}
	if server.logins.Load() != 1 || logins.size() != 1 {
		t.Errorf("made %d logins and kept %d tokens for three paths of one target, want one of each at /login", server.logins.Load(), logins.size())
	}
	for _, relative := range []string{"login", "./login", "../login", "?x=1"} {
		col := testutil.Collector("api", "text")
		col.Request.Login = &model.LoginConfig{Type: "json", URL: relative, Token: ".access_token", FieldFiles: map[string]string{"password": "/run/p"}}
		if err := ValidateRequest(&col); err == nil || !strings.Contains(err.Error(), "relative to the target's path") {
			t.Errorf("login.url %q: got %v, want it refused as relative to the target's path", relative, err)
		}
	}
}

// A 401 from where a redirect led without the token, another origin, is no
// refusal of the token: it is the answer, and no login is made again.
func TestA401FromARedirectWithoutTheTokenMakesNoNewLogin(t *testing.T) {
	freshLogins(t)
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) }))
	defer elsewhere.Close()
	server := newLoginServer(t, tokenAnswer(0), func(r *http.Request) bool { return false })
	redirecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/login" {
			server.Config.Handler.ServeHTTP(w, r)
			return
		}
		http.Redirect(w, r, elsewhere.URL+"/data", http.StatusFound)
	}))
	defer redirecting.Close()
	c := loginCollector(t, model.LoginConfig{Type: "json", URL: "/login", Token: ".access_token", FieldFiles: secretFiles(t, map[string]string{"password": "p"})})
	c.Request.FollowRedirects = true
	response, err := FetchCollector(context.Background(), redirecting.URL+"/data", c, RequestOverrides{}, nil)
	if err != nil || response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("fetch: %v, %+v; want the 401", err, response)
	}
	if n := server.logins.Load(); n != 1 {
		t.Errorf("made %d logins, want 1: the 401 came from a hop the token was not sent to", n)
	}
}

// A debug run's login is not counted in the collector's self-metrics, but
// the token it fetched is the collector's, kept for the requests after it.
func TestADebugRunsLoginIsNotCounted(t *testing.T) {
	freshLogins(t)
	server := newLoginServer(t, tokenAnswer(0), bearer("tok1"))
	c := loginCollector(t, model.LoginConfig{Type: "json", URL: "/login", Token: ".access_token", FieldFiles: secretFiles(t, map[string]string{"password": "p"})})
	ctx, _ := WithRequestTrace(context.Background())
	if _, err := FetchCollector(ctx, server.URL+"/data", c, RequestOverrides{}, nil); err != nil {
		t.Fatal(err)
	}
	if made, failed := logins.counts("api"); made != 0 || failed != 0 {
		t.Errorf("counted %d logins and %d failures of a debug run, want none", made, failed)
	}
	if _, err := FetchCollector(context.Background(), server.URL+"/data", c, RequestOverrides{}, nil); err != nil {
		t.Fatal(err)
	}
	if n := server.logins.Load(); n != 1 {
		t.Errorf("made %d logins, want 1: the debug run's token is kept for the collector", n)
	}

	logins.reset()
	failing := newLoginServer(t, func(w http.ResponseWriter, _ int64) { w.WriteHeader(http.StatusForbidden) }, bearer("tok1"))
	if _, err := FetchCollector(ctx, failing.URL+"/data", c, RequestOverrides{}, nil); err == nil {
		t.Fatal("a refused login succeeded")
	}
	if made, failed := logins.counts("api"); made != 0 || failed != 0 {
		t.Errorf("counted %d logins and %d failures of a debug run's failed login, want none", made, failed)
	}
}

// A relative login URL is sent with the request's Host override, the
// collector's or a static target's, so a target reached by its address logs
// in at the virtual host the request asks for, json and oauth2 alike, and
// the trace shows that Host; two virtual hosts on one address log in apart,
// neither sent the other's token. An absolute login URL keeps its own host.
func TestARelativeLoginIsSentWithTheHostOverride(t *testing.T) {
	freshLogins(t)
	var mu sync.Mutex
	var loginHosts []string
	var n atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/login", "/token":
			mu.Lock()
			loginHosts = append(loginHosts, r.Host)
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"access_token": "%s-%d", "token_type": "Bearer"}`, r.Host, n.Add(1))
		default:
			if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer "+r.Host+"-") {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = io.WriteString(w, "value=1")
		}
	}))
	t.Cleanup(server.Close)
	address := strings.TrimPrefix(server.URL, "http://")
	fetchAs := func(t *testing.T, c *model.Collector, forwarded http.Header) {
		t.Helper()
		response, err := FetchCollector(context.Background(), server.URL+"/data", c, RequestOverrides{}, forwarded)
		if err != nil || response.StatusCode != http.StatusOK {
			t.Fatalf("fetch: %v, %+v", err, response)
		}
	}
	reset := func() {
		logins.reset()
		mu.Lock()
		loginHosts = nil
		mu.Unlock()
	}
	password := secretFiles(t, map[string]string{"password": "p"})

	t.Run("json", func(t *testing.T) {
		reset()
		c := loginCollector(t, model.LoginConfig{Type: "json", URL: "/login", Token: ".access_token", FieldFiles: password})
		c.Request.Headers = map[string]string{"Host": "a.example"}
		ctx, trace := WithRequestTrace(context.Background())
		if _, err := FetchCollector(ctx, server.URL+"/data", c, RequestOverrides{}, nil); err != nil {
			t.Fatal(err)
		}
		fetchAs(t, c, http.Header{"Host": {"b.example"}})
		fetchAs(t, c, nil)
		fetchAs(t, c, http.Header{"Host": {"b.example"}})
		if want := []string{"a.example", "b.example"}; fmt.Sprint(loginHosts) != fmt.Sprint(want) {
			t.Errorf("logged in at %v, want %v: once at each virtual host", loginHosts, want)
		}
		if requests := trace.Requests(); len(requests) != 2 || !requests[0].Login || requests[0].Header.Get("Host") != "a.example" {
			t.Errorf("traced %+v, want the login with Host: a.example", requests)
		}
	})
	t.Run("oauth2", func(t *testing.T) {
		reset()
		c := loginCollector(t, model.LoginConfig{Type: "oauth2", URL: "/token", ClientID: "exporter", ClientSecretFile: password["password"]})
		c.Request.Headers = map[string]string{"Host": "a.example"}
		fetchAs(t, c, nil)
		if want := []string{"a.example"}; fmt.Sprint(loginHosts) != fmt.Sprint(want) {
			t.Errorf("logged in at %v, want %v", loginHosts, want)
		}
	})
	t.Run("absolute", func(t *testing.T) {
		reset()
		c := loginCollector(t, model.LoginConfig{Type: "json", URL: server.URL + "/login", Token: ".access_token", FieldFiles: password})
		c.Request.Headers = map[string]string{"Host": "a.example"}
		// The token is the address's, which the virtual host refuses with
		// a 401, so the login is made twice; both are at the address.
		response, err := FetchCollector(context.Background(), server.URL+"/data", c, RequestOverrides{}, nil)
		if err != nil || response.StatusCode != http.StatusUnauthorized {
			t.Fatalf("fetch: %v, %+v; want the virtual host's 401", err, response)
		}
		if want := []string{address, address}; fmt.Sprint(loginHosts) != fmt.Sprint(want) {
			t.Errorf("logged in at %v, want %v: the absolute URL's own host", loginHosts, want)
		}
	})
}
