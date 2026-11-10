//go:build !select_request_types || request_type_http || request_type_graphite

package fetch

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1" // #nosec G505 -- OAuth 1.0a's HMAC-SHA1 is what the server checks
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/expr"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"golang.org/x/net/http/httpguts"
)

// A steps login is a login no one request makes: a sign-in page whose form
// carries a token against cross-site requests, a form posted with the
// user's password, a ticket read from the page it answers with and traded
// for a token, perhaps signed with OAuth 1.0a. Each step is one request,
// made in order with one cookie jar, and reads values from its answer —
// with a regular expression, a jq expression, a field of a form, a header
// or a cookie — which the steps after it send as {{login.<name>}}, and of
// which one is the token:
//
//	login:
//	  type: steps
//	  secret_files: {password: /var/run/secrets/site/password}
//	  values: {username: me@example.com}
//	  steps:
//	    - url: https://sso.example/signin
//	      extract: {csrf: {regex: 'name="_csrf" value="([^"]+)"'}}
//	    - url: https://sso.example/signin
//	      form: {username: "{{login.username}}", password: "{{login.password}}", _csrf: "{{login.csrf}}"}
//	      extract: {access_token: {json: .access_token}, expires_in: {json: .expires_in}}
//	  token: access_token
//	  expires_in: expires_in
//
// A token's lifetime is, of the first that says one: the value expires_in
// names, in seconds, or expires_at, a time; the cookie's Max-Age or
// Expires, when the token is a cookie's; the token's own exp claim, when
// it is a JSON Web Token; and ttl.
//
// A login whose whole run is costly, or is held to few runs a day, keeps
// values of it (keep), each for how long, and makes later tokens with
// refresh_steps, which send the values kept: a long-lived token traded for
// short-lived ones. When the values kept have expired, or the refresh
// fails, the whole login is made again. keep_file writes the token and the
// values kept to a file, read when the exporter starts, so that a restart
// makes no login while they last.

// maxLoginSteps bounds the steps of each list: a login is a few requests.
const maxLoginSteps = 16

// loginValueName is the name of a value of a steps login.
var loginValueName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// loginValueRef is a value's place in what a step sends.
var loginValueRef = regexp.MustCompile(`\{\{login\.([A-Za-z0-9_]*)\}\}`)

// loginStepMethods are the methods a step is sent with.
var loginStepMethods = []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch}

// stepsLoginKeys are the keys only a steps login takes, besides token and
// expires_in, which a json login takes too.
var stepsLoginKeys = []string{"steps", "refresh_steps", "values", "secret_files", "expires_at", "keep", "keep_file"}

// checkStepsLogin holds a steps login's rules when the configuration loads.
func checkStepsLogin(l *model.LoginConfig, allowedSchemes []string, where string) error {
	if l.URL != "" {
		return fmt.Errorf("%s sets url; a steps login sends each step to its own url", where)
	}
	if len(l.Steps) == 0 {
		return fmt.Errorf("%s has no steps; a steps login makes one request at least, each step a url and what to read from its answer", where)
	}
	defined := map[string]string{}
	for _, name := range model.SortedKeys(l.Values) {
		if !loginValueName.MatchString(name) {
			return fmt.Errorf("%s.values has %q, which is no value's name; a name is letters, digits and _, not first a digit", where, name)
		}
		defined[name] = "values"
	}
	for _, name := range model.SortedKeys(l.SecretFiles) {
		if !loginValueName.MatchString(name) {
			return fmt.Errorf("%s.secret_files has %q, which is no value's name; a name is letters, digits and _, not first a digit", where, name)
		}
		if _, ok := defined[name]; ok {
			return fmt.Errorf("%s names %s in both values and secret_files; name it once", where, name)
		}
		if strings.TrimSpace(l.SecretFiles[name]) == "" {
			return fmt.Errorf("%s.secret_files.%s has no file; write the path of the file holding the value", where, name)
		}
		defined[name] = "secret_files"
	}
	extracted, err := checkLoginSteps(l.Steps, allowedSchemes, where+".steps", defined)
	if err != nil {
		return err
	}
	ends := func(list string, made map[string]bool) error {
		if l.Token == "" {
			return fmt.Errorf("%s has no token; name the value that is the token, which a step reads, such as token: access_token", where)
		}
		if !made[l.Token] {
			return fmt.Errorf("%s.token is %q, which no step of %s reads", where, l.Token, list)
		}
		for key, name := range map[string]string{"expires_in": l.ExpiresIn, "expires_at": l.ExpiresAt} {
			if name != "" && !made[name] {
				return fmt.Errorf("%s.%s is %q, which no step of %s reads", where, key, name, list)
			}
		}
		return nil
	}
	if l.ExpiresIn != "" && l.ExpiresAt != "" {
		return fmt.Errorf("%s sets expires_in and expires_at; the token's lifetime is one of them", where)
	}
	if err := ends("steps", extracted); err != nil {
		return err
	}
	for _, name := range model.SortedKeys(l.Keep) {
		if !extracted[name] {
			return fmt.Errorf("%s.keep names %q, which no step of steps reads", where, name)
		}
		if name == l.Token {
			return fmt.Errorf("%s.keep names the token, %s; the token is kept as every token is, and refresh_steps make a new one of the values kept", where, name)
		}
		if l.Keep[name] < 0 {
			return fmt.Errorf("%s.keep.%s must not be negative; it is how long the value is used, or 0 for as long as the steps that send it succeed", where, name)
		}
	}
	switch {
	case len(l.RefreshSteps) > 0 && len(l.Keep) == 0:
		return fmt.Errorf("%s has refresh_steps and no keep; name the values of the steps that refresh_steps send, such as keep: {refresh_token: 720h}", where)
	case len(l.RefreshSteps) == 0 && len(l.Keep) > 0:
		return fmt.Errorf("%s has keep and no refresh_steps; values are kept for the steps that make a new token of them", where)
	}
	if len(l.RefreshSteps) > 0 {
		kept := copyStrings(defined)
		for name := range l.Keep {
			kept[name] = "keep"
		}
		made, err := checkLoginSteps(l.RefreshSteps, allowedSchemes, where+".refresh_steps", kept)
		if err != nil {
			return err
		}
		if err := ends("refresh_steps", made); err != nil {
			return err
		}
	}
	if l.KeepFile != "" {
		if strings.TrimSpace(l.KeepFile) == "" || !filepath.IsAbs(l.KeepFile) {
			return fmt.Errorf("%s.keep_file %q is not an absolute path; name the file the token and the values kept are written to", where, l.KeepFile)
		}
	}
	return nil
}

// copyStrings is a copy of m.
func copyStrings(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// checkLoginSteps holds a list of steps to their rules, each reading only
// the values defined before it: given, or read by a step before. It
// returns the names the steps read.
func checkLoginSteps(steps []model.LoginStep, allowedSchemes []string, where string, given map[string]string) (map[string]bool, error) {
	if len(steps) > maxLoginSteps {
		return nil, fmt.Errorf("%s has %d steps, more than %d", where, len(steps), maxLoginSteps)
	}
	known := map[string]bool{}
	for name := range given {
		known[name] = true
	}
	read := map[string]bool{}
	for i := range steps {
		s := &steps[i]
		at := fmt.Sprintf("%s[%d]", where, i)
		if err := checkLoginURL(s.URL, allowedSchemes); err != nil {
			return nil, fmt.Errorf("%s.url %w", at, err)
		}
		if s.Method != "" && !slices.Contains(loginStepMethods, s.Method) {
			return nil, fmt.Errorf("%s.method is %q; a step is sent with %s", at, s.Method, strings.Join(loginStepMethods, ", "))
		}
		if len(s.Form) > 0 && len(s.JSON) > 0 {
			return nil, fmt.Errorf("%s sets form and json; a step sends one body", at)
		}
		if s.Method == http.MethodGet && (len(s.Form) > 0 || len(s.JSON) > 0) {
			return nil, fmt.Errorf("%s sends a body with GET; leave method out, which is POST with a body", at)
		}
		uses := func(key, text string) error {
			for _, m := range loginValueRef.FindAllStringSubmatch(text, -1) {
				if !known[m[1]] {
					return fmt.Errorf("%s.%s holds {{login.%s}}, which is not a value given or read by a step before it", at, key, m[1])
				}
			}
			return nil
		}
		for _, group := range []struct {
			key    string
			values map[string]string
		}{{"query", s.Query}, {"headers", s.Headers}, {"form", s.Form}, {"json", s.JSON}} {
			for _, name := range model.SortedKeys(group.values) {
				if name == "" {
					return nil, fmt.Errorf("%s.%s has an entry without a name", at, group.key)
				}
				if err := uses(group.key+"."+name, group.values[name]); err != nil {
					return nil, err
				}
			}
		}
		for _, name := range model.SortedKeys(s.Headers) {
			if !httpguts.ValidHeaderFieldName(name) {
				return nil, fmt.Errorf("%s.headers %q is not a header name", at, name)
			}
			switch http.CanonicalHeaderKey(name) {
			case "Host", "Content-Length", "Content-Type", "Transfer-Encoding", "Connection", "Cookie", "Accept-Encoding":
				return nil, fmt.Errorf("%s.headers sets %s, which the step sets itself: the Host of the url, the body's type and length, the connection, the cookies of the steps before it, and the compression it reads the answer in", at, http.CanonicalHeaderKey(name))
			}
			if s.OAuth1 != nil && strings.EqualFold(name, "Authorization") {
				return nil, fmt.Errorf("%s.headers sets Authorization, which oauth1 signs the step with", at)
			}
			if text := loginValueRef.ReplaceAllString(s.Headers[name], ""); checkHeaderValue(text) != nil {
				return nil, fmt.Errorf("%s.headers.%s holds a control character, which a header may not", at, name)
			}
		}
		if o := s.OAuth1; o != nil {
			if o.ConsumerKey == "" || o.ConsumerSecret == "" {
				return nil, fmt.Errorf("%s.oauth1 needs consumer_key and consumer_secret", at)
			}
			if (o.Token == "") != (o.TokenSecret == "") {
				return nil, fmt.Errorf("%s.oauth1 sets one of token and token_secret; a token is signed with its secret, so set both or neither", at)
			}
			for _, field := range []struct{ key, text string }{{"consumer_key", o.ConsumerKey}, {"consumer_secret", o.ConsumerSecret}, {"token", o.Token}, {"token_secret", o.TokenSecret}} {
				if err := uses("oauth1."+field.key, field.text); err != nil {
					return nil, err
				}
			}
		}
		for j, status := range s.Status {
			if status < 100 || status > 599 {
				return nil, fmt.Errorf("%s.status[%d] is %d, which is no HTTP status", at, j, status)
			}
		}
		if len(s.Extract) == 0 && i == len(steps)-1 {
			return nil, fmt.Errorf("%s reads nothing; the last step reads the token, at least", at)
		}
		for _, name := range model.SortedKeys(s.Extract) {
			if !loginValueName.MatchString(name) {
				return nil, fmt.Errorf("%s.extract has %q, which is no value's name; a name is letters, digits and _, not first a digit", at, name)
			}
			// A value kept may be read anew by refresh_steps, as a refresh
			// token an endpoint rotates is, and replaces the one kept.
			if from, ok := given[name]; ok && from != "keep" {
				return nil, fmt.Errorf("%s.extract reads %s, which %s gives; name it otherwise", at, name, from)
			}
			if err := checkLoginExtract(s.Extract[name], at+".extract."+name); err != nil {
				return nil, err
			}
		}
		// What a step reads is known to the steps after it, not to itself.
		for name := range s.Extract {
			known[name] = true
			read[name] = true
		}
	}
	return read, nil
}

// checkLoginExtract holds where a value is read from to exactly one place,
// its expression compiling.
func checkLoginExtract(e model.LoginExtract, where string) error {
	set := 0
	for _, place := range []string{e.Regex, e.JSON, e.Form, e.Header, e.Cookie} {
		if place != "" {
			set++
		}
	}
	if set != 1 {
		return fmt.Errorf("%s sets %d of regex, json, form, header and cookie; a value is read from exactly one", where, set)
	}
	switch {
	case e.Regex != "":
		if _, err := regexp.Compile(e.Regex); err != nil {
			return fmt.Errorf("%s.regex does not compile: %w", where, err)
		}
	case e.JSON != "":
		if _, err := expr.CompileJQ(e.JSON); err != nil {
			return fmt.Errorf("%s.json %q does not compile: %w", where, e.JSON, err)
		}
	case e.Header != "" && !httpguts.ValidHeaderFieldName(e.Header):
		return fmt.Errorf("%s.header %q is not a header name", where, e.Header)
	case e.Cookie != "" && !httpguts.ValidHeaderFieldName(e.Cookie):
		return fmt.Errorf("%s.cookie %q is not a cookie name", where, e.Cookie)
	}
	return nil
}

// loginValue is a value of a steps login: the text, and when it expires,
// zero when nothing says.
type loginValue struct {
	Text    string    `json:"value"`
	Expires time.Time `json:"expires,omitzero"`
	// written says the value is one of login.values, which the
	// configuration writes and is no secret; every other is one a file
	// holds or a step read, and is masked in an answer shown.
	written bool
}

// stepsLogin makes the login l, for a request of c to target, whose token
// is kept under key: the token the keep file holds, when it is not the
// last one this exporter's logins gave and lasts; else refresh_steps with
// the values kept, while they last; else, or when those fail, the steps.
func stepsLogin(ctx context.Context, client *http.Client, c *model.Collector, l *model.LoginConfig, target, host, key string, start time.Time) (loginToken, error) {
	state := stepsStates.of(key)
	state.load(l)
	if token, ok := state.takeKeptToken(start); ok {
		return token, nil
	}
	given, err := stepsGiven(l)
	if err != nil {
		return loginToken{}, err
	}
	var refreshErr error
	if kept, ok := state.keptValues(l, start); ok && len(l.RefreshSteps) > 0 {
		values := copyLoginValues(given)
		for name, value := range kept {
			values[name] = value
		}
		read, err := runLoginSteps(ctx, client, c, l.RefreshSteps, "refresh_steps", target, host, values, start)
		if err == nil {
			var token loginToken
			if token, err = stepsToken(l, read, start); err == nil {
				state.update(l, token, read, start, false)
				logins.noteRefresh(ctx, c.Name)
				return token, nil
			}
		}
		refreshErr = err
		slog.Default().WarnContext(ctx, "a login's refresh_steps failed; the whole steps are made", "collector", c.Name, "error", err)
	}
	read, err := runLoginSteps(ctx, client, c, l.Steps, "steps", target, host, copyLoginValues(given), start)
	if err != nil {
		if refreshErr != nil {
			return loginToken{}, fmt.Errorf("refresh_steps failed (%w), and then %w", refreshErr, err)
		}
		return loginToken{}, err
	}
	token, err := stepsToken(l, read, start)
	if err != nil {
		return loginToken{}, err
	}
	state.update(l, token, read, start, true)
	return token, nil
}

// copyLoginValues is a copy of m.
func copyLoginValues(m map[string]loginValue) map[string]loginValue {
	out := make(map[string]loginValue, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// stepsGiven is the values a steps login is given: those written, and
// those read from their files, at each login.
func stepsGiven(l *model.LoginConfig) (map[string]loginValue, error) {
	out := make(map[string]loginValue, len(l.Values)+len(l.SecretFiles))
	for name, text := range l.Values {
		out[name] = loginValue{Text: text, written: true}
	}
	for _, name := range model.SortedKeys(l.SecretFiles) {
		text, err := ReadCredentialFile(l.SecretFiles[name])
		if err != nil {
			return nil, fmt.Errorf("reading login.secret_files.%s: %w", name, err)
		}
		if text == "" {
			return nil, fmt.Errorf("login.secret_files.%s file %s is empty", name, l.SecretFiles[name])
		}
		out[name] = loginValue{Text: text}
	}
	return out, nil
}

// stepsToken is the token of a steps login's values: the value token names,
// and its lifetime, of the first that says one — expires_in or expires_at,
// the token's own expiry as its cookie or its JWT exp claim says it, and
// ttl.
func stepsToken(l *model.LoginConfig, values map[string]loginValue, start time.Time) (loginToken, error) {
	value, ok := values[l.Token]
	if !ok || value.Text == "" {
		return loginToken{}, fmt.Errorf("login.token %s is empty", l.Token)
	}
	token := loginToken{value: value.Text}
	switch {
	case l.ExpiresIn != "":
		if text := values[l.ExpiresIn].Text; text != "" {
			seconds, ok := loginSeconds(text)
			if !ok {
				return loginToken{}, fmt.Errorf("login.expires_in %s is no lifetime in seconds, a number above 0", l.ExpiresIn)
			}
			token.expires = start.Add(seconds)
		}
	case l.ExpiresAt != "":
		if text := values[l.ExpiresAt].Text; text != "" {
			at, ok := loginTime(text)
			if !ok {
				return loginToken{}, fmt.Errorf("login.expires_at %s is no time: seconds or milliseconds since 1970, RFC 3339 or an HTTP date", l.ExpiresAt)
			}
			// A token that expired before the login began is no token:
			// kept, it would be fetched again at every request. A
			// lifetime in seconds read as a time is one, 3600 being an
			// hour after 1970 began.
			if !at.After(start) {
				return loginToken{}, fmt.Errorf("login.expires_at %s is a time before the login, %s; if it is the token's lifetime in seconds, name it with expires_in", l.ExpiresAt, at.UTC().Format(time.RFC3339))
			}
			token.expires = at
		}
	}
	if token.expires.IsZero() {
		token.expires = value.Expires
	}
	defaultExpiry(&token, l, start)
	return token, nil
}

// defaultExpiry gives a token whose login said no lifetime the one its JWT
// exp claim says, when it is after start, else ttl's.
func defaultExpiry(token *loginToken, l *model.LoginConfig, start time.Time) {
	// An exp not after the login's start, of a clock behind the issuer's
	// or a token issued expired, says nothing the token could be kept by:
	// taken, every request would make a login.
	if exp := jwtExpiry(token.value); token.expires.IsZero() && exp.After(start) {
		token.expires = exp
	}
	if token.expires.IsZero() && l.TTL > 0 {
		token.expires = start.Add(time.Duration(l.TTL))
	}
}

// jwtExpiry is the exp claim of a JSON Web Token (RFC 7519 § 4.1.4), zero
// for a token that is none, or says no expiry. The signature is not
// checked: the claim says when the target will refuse the token, which the
// target checks.
func jwtExpiry(token string) time.Time {
	token = strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(token, "Bearer "), "bearer "))
	parts := strings.Split(token, ".")
	if len(parts) != 3 || len(parts[1]) > loginAnswerLimit {
		return time.Time{}
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return time.Time{}
	}
	var claims struct {
		Exp json.Number `json:"exp"`
	}
	if json.Unmarshal(payload, &claims) != nil || claims.Exp == "" {
		return time.Time{}
	}
	seconds, err := claims.Exp.Float64()
	if err != nil || seconds <= 0 || seconds > 1e12 {
		return time.Time{}
	}
	return time.Unix(int64(seconds), 0)
}

// loginTime reads an expiry: seconds or milliseconds since 1970, RFC 3339,
// or an HTTP date.
func loginTime(text string) (time.Time, bool) {
	text = strings.TrimSpace(text)
	if n, err := strconv.ParseFloat(text, 64); err == nil {
		switch {
		case n <= 0:
			return time.Time{}, false
		case n > 1e11:
			return time.UnixMilli(int64(n)), true
		default:
			return time.Unix(int64(n), 0), true
		}
	}
	if t, err := time.Parse(time.RFC3339Nano, text); err == nil {
		return t, true
	}
	if t, err := http.ParseTime(text); err == nil {
		return t, true
	}
	return time.Time{}, false
}

// runLoginSteps makes steps in order, with one cookie jar, and returns the
// values given and read. list names the steps in errors.
func runLoginSteps(ctx context.Context, base *http.Client, c *model.Collector, steps []model.LoginStep, list, target, host string, values map[string]loginValue, start time.Time) (map[string]loginValue, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	client := *base
	client.Jar = jar
	var proxy func(*http.Request) (*url.URL, error)
	if transport, ok := innerTransport(base.Transport).(*http.Transport); ok {
		proxy = transport.Proxy
	}
	for i := range steps {
		at := fmt.Sprintf("login.%s[%d]", list, i)
		if err := runLoginStep(ctx, &client, c, &steps[i], at, target, host, proxy, values, start); err != nil {
			return nil, err
		}
	}
	return values, nil
}

// innerTransport is the transport a login's tracer wraps.
func innerTransport(rt http.RoundTripper) http.RoundTripper {
	if t, ok := rt.(loginTracer); ok {
		return t.next
	}
	return rt
}

// fillLoginValues is text with each {{login.<name>}} replaced by the
// value's text.
func fillLoginValues(text string, values map[string]loginValue) string {
	if !strings.Contains(text, "{{login.") {
		return text
	}
	return loginValueRef.ReplaceAllStringFunc(text, func(ref string) string {
		return values[loginValueRef.FindStringSubmatch(ref)[1]].Text
	})
}

// runLoginStep makes one step and adds what it reads to values.
func runLoginStep(ctx context.Context, client *http.Client, c *model.Collector, s *model.LoginStep, at, target, host string, proxy func(*http.Request) (*url.URL, error), values map[string]loginValue, start time.Time) error {
	u, err := loginURL(&model.LoginConfig{URL: s.URL}, target)
	if err != nil {
		return fmt.Errorf("%s: %w", at, err)
	}
	if len(s.Query) > 0 {
		query := u.Query()
		for _, name := range model.SortedKeys(s.Query) {
			query.Set(name, fillLoginValues(s.Query[name], values))
		}
		copied := *u
		copied.RawQuery = query.Encode()
		u = &copied
	}
	method := s.Method
	var body []byte
	var form url.Values
	contentType := ""
	switch {
	case len(s.Form) > 0:
		form = url.Values{}
		for _, name := range model.SortedKeys(s.Form) {
			form.Set(name, fillLoginValues(s.Form[name], values))
		}
		body, contentType = []byte(form.Encode()), "application/x-www-form-urlencoded"
	case len(s.JSON) > 0:
		object := make(map[string]string, len(s.JSON))
		for name, text := range s.JSON {
			object[name] = fillLoginValues(text, values)
		}
		if body, err = json.Marshal(object); err != nil {
			return err
		}
		contentType = "application/json"
	}
	if method == "" {
		method = http.MethodGet
		if body != nil {
			method = http.MethodPost
		}
	}
	stepCtx, err := withTargetPolicy(ctx, c, u, proxy, false, "")
	if err != nil {
		return fmt.Errorf("%s: %w", at, err)
	}
	// u is the step's url from the configuration, resolved against the
	// target's scheme, host and port (loginURL), and held to the
	// collector's target policy just above; a probe cannot set or change
	// it.
	req, err := http.NewRequestWithContext(stepCtx, method, u.String(), bytes.NewReader(body)) // #nosec G704 -- the configured login URL, checked against the target policy
	if err != nil {
		return fmt.Errorf("%s: %w", at, err)
	}
	if body == nil {
		req.Body, req.ContentLength = http.NoBody, 0
	}
	if relativeLoginURL(&model.LoginConfig{URL: s.URL}) && host != "" {
		req.Host = host
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for _, name := range model.SortedKeys(s.Headers) {
		value := fillLoginValues(s.Headers[name], values)
		if checkHeaderValue(value) != nil {
			return fmt.Errorf("%s.headers.%s holds a control character once its values are filled in, which a header may not", at, name)
		}
		req.Header.Set(name, value)
	}
	if o := s.OAuth1; o != nil {
		signed := oauth1Signer{
			consumerKey: fillLoginValues(o.ConsumerKey, values), consumerSecret: fillLoginValues(o.ConsumerSecret, values),
			token: fillLoginValues(o.Token, values), tokenSecret: fillLoginValues(o.TokenSecret, values),
		}
		req.Header.Set("Authorization", signed.authorization(method, u, form, oauth1Nonce(), loginNow().Unix()))
	}
	resp, err := client.Do(req) // #nosec G704 -- the configured login URL, checked against the target policy
	if err != nil {
		return fmt.Errorf("%s: %w", at, withAnswerWithheld(RedactURLErrors(err)))
	}
	answer, readErr := io.ReadAll(io.LimitReader(resp.Body, loginAnswerLimit+1))
	_ = resp.Body.Close()
	if readErr != nil {
		return fmt.Errorf("%s: reading the answer: %w", at, RedactURLErrors(readErr))
	}
	accepted := resp.StatusCode >= 200 && resp.StatusCode <= 299
	if len(s.Status) > 0 {
		accepted = slices.Contains(s.Status, resp.StatusCode)
	}
	if !accepted {
		if resp.StatusCode >= 300 && resp.StatusCode < 400 {
			return fmt.Errorf("%s was answered with the redirect %d, which a login does not follow; list it in the step's status to read its answer, the Location header with it", at, resp.StatusCode)
		}
		var secrets []string
		for _, name := range model.SortedKeys(values) {
			if !values[name].written {
				secrets = append(secrets, values[name].Text)
			}
		}
		return fmt.Errorf("%s: %w", at, answeredError(resp.StatusCode, false, answer, secrets...))
	}
	if len(answer) > loginAnswerLimit {
		return fmt.Errorf("%s: the answer is larger than %d bytes", at, loginAnswerLimit)
	}
	for _, name := range model.SortedKeys(s.Extract) {
		value, err := extractLoginValue(ctx, s.Extract[name], resp, answer, start)
		if err != nil {
			return fmt.Errorf("%s.extract.%s %w", at, name, err)
		}
		values[name] = value
	}
	return nil
}

// extractLoginValue reads one value of an answer. An error says what was
// not found, never what was: the answer may hold secrets.
func extractLoginValue(ctx context.Context, e model.LoginExtract, resp *http.Response, answer []byte, start time.Time) (loginValue, error) {
	var value loginValue
	switch {
	case e.Regex != "":
		m := regexp.MustCompile(e.Regex).FindSubmatch(answer)
		if m == nil {
			return value, errors.New("matched nothing in the answer")
		}
		value.Text = string(m[0])
		if len(m) > 1 {
			value.Text = string(m[1])
		}
	case e.JSON != "":
		decoder := json.NewDecoder(bytes.NewReader(answer))
		decoder.UseNumber()
		var document any
		if err := decoder.Decode(&document); err != nil {
			return value, errors.New("found an answer that is not JSON")
		}
		found, kind, err := jqOne(ctx, e.JSON, document, resp)
		if err != nil {
			return value, err
		}
		switch v := found.(type) {
		case string:
			value.Text = v
		case json.Number:
			value.Text = v.String()
		default:
			return value, fmt.Errorf("gave %s; it must give text or a number", kind)
		}
	case e.Form != "":
		// A pair ParseQuery cannot read, such as one with a semicolon,
		// leaves the others read: the field is looked for among them.
		form, _ := url.ParseQuery(strings.TrimSpace(string(answer)))
		value.Text = form.Get(e.Form)
	case e.Header != "":
		value.Text = resp.Header.Get(e.Header)
	case e.Cookie != "":
		for _, cookie := range resp.Cookies() {
			// A cookie the answer deletes, with Max-Age below 0 or an
			// Expires past, is none: a site that logs one out first sets
			// it so before it sets the new one.
			if cookie.Name != e.Cookie || cookie.Value == "" || cookie.MaxAge < 0 || (!cookie.Expires.IsZero() && !cookie.Expires.After(start)) {
				continue
			}
			value.Text = cookie.Value
			switch {
			case cookie.MaxAge > 0:
				value.Expires = start.Add(time.Duration(cookie.MaxAge) * time.Second)
			case !cookie.Expires.IsZero():
				value.Expires = cookie.Expires
			}
		}
	}
	if value.Text == "" {
		return value, errors.New("found nothing, or an empty value, in the answer")
	}
	return value, nil
}

// oauth1Signer signs a request with OAuth 1.0a's HMAC-SHA1 (RFC 5849
// § 3.4.2).
type oauth1Signer struct {
	consumerKey, consumerSecret, token, tokenSecret string
}

// oauth1Nonce is a nonce of 16 random bytes, in hex.
func oauth1Nonce() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// oauth1Escape is RFC 5849 § 3.6's percent encoding: everything but
// letters, digits and -._~.
func oauth1Escape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '.' || c == '_' || c == '~' {
			b.WriteByte(c)
			continue
		}
		fmt.Fprintf(&b, "%%%02X", c)
	}
	return b.String()
}

// authorization is the Authorization header of a request of method to u,
// with the form body form, signed at the time timestamp with nonce.
func (o oauth1Signer) authorization(method string, u *url.URL, form url.Values, nonce string, timestamp int64) string {
	oauth := map[string]string{
		"oauth_consumer_key":     o.consumerKey,
		"oauth_nonce":            nonce,
		"oauth_signature_method": "HMAC-SHA1",
		"oauth_timestamp":        strconv.FormatInt(timestamp, 10),
		"oauth_version":          "1.0",
	}
	if o.token != "" {
		oauth["oauth_token"] = o.token
	}
	var pairs []string
	add := func(name, value string) { pairs = append(pairs, oauth1Escape(name)+"="+oauth1Escape(value)) }
	for name, value := range oauth {
		add(name, value)
	}
	for name, given := range u.Query() {
		for _, value := range given {
			add(name, value)
		}
	}
	for name, given := range form {
		for _, value := range given {
			add(name, value)
		}
	}
	sort.Strings(pairs)
	baseURL := strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host)
	if port := u.Port(); (port == "80" && strings.EqualFold(u.Scheme, "http")) || (port == "443" && strings.EqualFold(u.Scheme, "https")) {
		baseURL = strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Hostname())
	}
	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}
	base := strings.ToUpper(method) + "&" + oauth1Escape(baseURL+path) + "&" + oauth1Escape(strings.Join(pairs, "&"))
	mac := hmac.New(sha1.New, []byte(oauth1Escape(o.consumerSecret)+"&"+oauth1Escape(o.tokenSecret)))
	mac.Write([]byte(base))
	oauth["oauth_signature"] = base64.StdEncoding.EncodeToString(mac.Sum(nil))
	names := model.SortedKeys(oauth)
	parts := make([]string, 0, len(names))
	for _, name := range names {
		parts = append(parts, oauth1Escape(name)+`="`+oauth1Escape(oauth[name])+`"`)
	}
	return "OAuth " + strings.Join(parts, ", ")
}

// stepsState is what a steps login keeps under one key: the values kept
// for refresh_steps; with keep_file, the token the file holds, when it is
// another than the last one this exporter's logins gave; and that last
// token.
type stepsState struct {
	mu        sync.Mutex
	key       string
	kept      map[string]loginValue
	fileToken *loginToken
	last      string
}

// stepsStates holds the state of each steps login, by its key in the
// token cache.
var stepsStates = &stepsStateCache{byKey: map[string]*stepsState{}}

type stepsStateCache struct {
	mu    sync.Mutex
	byKey map[string]*stepsState
}

// of is the state kept under key.
func (c *stepsStateCache) of(key string) *stepsState {
	c.mu.Lock()
	defer c.mu.Unlock()
	if s := c.byKey[key]; s != nil {
		return s
	}
	if len(c.byKey) >= maxLoginEntries {
		clear(c.byKey)
	}
	s := &stepsState{key: key}
	c.byKey[key] = s
	return s
}

// load reads what l's keep file holds of the login, at each login: after
// the exporter starts, and whenever another exporter sharing the file, as
// the target allocator's replicas do on a volume they share, logged in
// since. The values kept are the file's, which this exporter's logins
// write too; its token is used when it is another than the last token a
// login of this exporter gave, which a login is made because it is due or
// was refused.
func (s *stepsState) load(l *model.LoginConfig) {
	if l.KeepFile == "" {
		return
	}
	saved, ok := keepFiles.read(l.KeepFile, s.key)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fileToken = nil
	if !ok {
		return
	}
	// The values kept are the file's when another exporter wrote them,
	// its token not this one's last, or when this one keeps none yet; else
	// this one's are as new, and newer when its last write failed.
	if saved.Kept != nil && (s.kept == nil || saved.Token != s.last) {
		s.kept = saved.Kept
	}
	if saved.Token != "" && saved.Token != s.last {
		s.fileToken = &loginToken{value: saved.Token, expires: saved.Expires}
	}
}

// takeKeptToken is the token the keep file held, while it lasts longer
// than its refresh margin.
func (s *stepsState) takeKeptToken(now time.Time) (loginToken, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	token := s.fileToken
	s.fileToken = nil
	if token == nil || (!token.expires.IsZero() && !now.Before(token.expires.Add(-loginRefreshMargin))) {
		return loginToken{}, false
	}
	s.last = token.value
	token.kept = true
	return *token, true
}

// keptValues are the values kept for refresh_steps, when each is there and
// has not expired.
func (s *stepsState) keptValues(l *model.LoginConfig, now time.Time) (map[string]loginValue, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(l.Keep) == 0 {
		return nil, false
	}
	out := make(map[string]loginValue, len(l.Keep))
	for name := range l.Keep {
		value, ok := s.kept[name]
		if !ok || value.Text == "" || (!value.Expires.IsZero() && !now.Before(value.Expires)) {
			return nil, false
		}
		out[name] = value
	}
	return out, true
}

// update keeps what a login read of the values l keeps — every one after
// the whole login, and those refresh_steps read again after a refresh —
// each expiring at the first of its keep, its cookie's and its JWT's
// expiry, and writes the token and them to the keep file.
func (s *stepsState) update(l *model.LoginConfig, token loginToken, read map[string]loginValue, start time.Time, whole bool) {
	s.mu.Lock()
	if whole || s.kept == nil {
		s.kept = map[string]loginValue{}
	}
	for name, lifetime := range l.Keep {
		value, ok := read[name]
		if !ok {
			continue
		}
		// A value refresh_steps sent and did not read anew keeps its
		// expiry: a refresh does not make it last longer.
		if prev, kept := s.kept[name]; kept && !whole && prev.Text == value.Text {
			continue
		}
		expires := value.Expires
		for _, at := range []time.Time{jwtExpiry(value.Text), keepUntil(start, lifetime)} {
			if !at.IsZero() && (expires.IsZero() || at.Before(expires)) {
				expires = at
			}
		}
		s.kept[name] = loginValue{Text: value.Text, Expires: expires}
	}
	s.last = token.value
	saved := keptLogin{Token: token.value, Expires: token.expires, Kept: copyLoginValues(s.kept)}
	s.mu.Unlock()
	if l.KeepFile != "" {
		keepFiles.write(l.KeepFile, s.key, saved)
	}
}

// keepUntil is when a value kept for lifetime from start expires, zero for
// none.
func keepUntil(start time.Time, lifetime model.Duration) time.Time {
	if lifetime <= 0 {
		return time.Time{}
	}
	return start.Add(time.Duration(lifetime))
}

// keepOnly drops the states of logins but those under keys, which the
// configuration in force still makes (loginCache.follow).
func (c *stepsStateCache) keepOnly(keys map[string]bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for key := range c.byKey {
		if !keys[key] {
			delete(c.byKey, key)
		}
	}
}

// reset forgets every state, for the exporter's shutdown.
func (c *stepsStateCache) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	clear(c.byKey)
}

// keptLogin is what a keep file holds of one login: its token, when it
// expires, and the values kept.
type keptLogin struct {
	Token   string                `json:"token,omitempty"`
	Expires time.Time             `json:"expires,omitzero"`
	Kept    map[string]loginValue `json:"kept,omitempty"`
}

// keepFile is a keep file's content: the logins written to it, by a
// digest of their key, which names the collector, the login and its URL.
type keepFile struct {
	Logins map[string]keptLogin `json:"logins"`
}

// keepFiles reads and writes keep files, one at a time.
var keepFiles = &keepFileStore{}

type keepFileStore struct {
	mu sync.Mutex
}

// keepDigest is what a login's key is written as in a keep file.
func keepDigest(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// read is what path holds of the login key; a file that is not there or
// cannot be read holds nothing, and the login is made.
func (k *keepFileStore) read(path, key string) (keptLogin, bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	file, err := readKeepFile(path)
	if err != nil {
		return keptLogin{}, false
	}
	saved, ok := file.Logins[keepDigest(key)]
	return saved, ok
}

// readKeepFile reads the keep file path.
func readKeepFile(path string) (keepFile, error) {
	var file keepFile
	raw, err := os.ReadFile(path) // #nosec G304 -- the configuration's keep_file
	if err != nil {
		return file, err
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		return file, err
	}
	return file, nil
}

// keepFileWarning is told of a keep file that could not be written: the
// login succeeded, and the next restart makes it again.
var keepFileWarning = func(path string, err error) {
	slog.Default().Warn("a login's keep_file could not be written; the login succeeded, and the exporter's next start makes it again", "keep_file", path, "error", err)
}

// write puts the login key's state in path, replacing the file whole: a
// file of its own beside it, renamed over it, readable by the exporter's
// user alone. Logins that wrote to the file before are kept, but for those
// whose token and values kept have all expired, which no login would use.
func (k *keepFileStore) write(path, key string, saved keptLogin) {
	k.mu.Lock()
	defer k.mu.Unlock()
	file, err := readKeepFile(path)
	if err != nil || file.Logins == nil {
		file = keepFile{Logins: map[string]keptLogin{}}
	}
	now := loginNow()
	for digest, other := range file.Logins {
		if keptExpired(other, now) {
			delete(file.Logins, digest)
		}
	}
	file.Logins[keepDigest(key)] = saved
	if len(file.Logins) > maxLoginEntries {
		file.Logins = map[string]keptLogin{keepDigest(key): saved}
	}
	raw, err := json.MarshalIndent(file, "", "  ")
	if err == nil {
		err = writeFileAtomically(path, raw)
	}
	if err != nil {
		keepFileWarning(path, err)
	}
}

// keptExpired reports whether nothing of a login a keep file holds would
// be used at now: its token has expired, or it has none, and so has every
// value kept. A token or a value that says no expiry is used.
func keptExpired(saved keptLogin, now time.Time) bool {
	if saved.Token != "" && (saved.Expires.IsZero() || now.Before(saved.Expires)) {
		return false
	}
	for _, value := range saved.Kept {
		if value.Expires.IsZero() || now.Before(value.Expires) {
			return false
		}
	}
	return true
}

// writeFileAtomically replaces path with raw, written beside it first.
func writeFileAtomically(path string, raw []byte) error {
	temp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(temp.Name()) }()
	if err := temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		return err
	}
	if _, err := temp.Write(raw); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(temp.Name(), path)
}

// keepFileFiles says, for --dry-run, what of a keep file a login would fail
// to write now: a directory that is not there, or is no directory.
func keepFileFiles(l *model.LoginConfig, who string) []string {
	if l == nil || l.KeepFile == "" {
		return nil
	}
	dir := filepath.Dir(l.KeepFile)
	info, err := os.Stat(dir)
	switch {
	case err != nil:
		return []string{fmt.Sprintf("%s request.login.keep_file %s: its directory %s: %v; the token and the values kept would not be written, and each restart would log in again", who, l.KeepFile, dir, err)}
	case !info.IsDir():
		return []string{fmt.Sprintf("%s request.login.keep_file %s: %s is no directory", who, l.KeepFile, dir)}
	}
	return nil
}
