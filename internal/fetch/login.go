//go:build !select_request_types || request_type_http || request_type_graphite

package fetch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/expr"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"golang.org/x/net/http/httpguts"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/clientcredentials"
)

// A target that wants a token it hands out itself, rather than one the
// operator can put in a file, is reached with request.login: before the
// request, the exporter logs in and sends the token it was given on the
// request, in Authorization as a bearer token unless the login says
// otherwise. Two kinds of login are made:
//
//   - json posts a JSON object to the login URL, of the fields written in
//     the configuration and those read from files (field_files), and reads
//     the token from the answer: from its body with a jq expression
//     (token), from a header of it (token_header), or from a cookie it sets
//     (token_cookie), which is then sent back as that cookie.
//   - oauth2 asks a token endpoint for a token with the client credentials
//     grant (RFC 6749 § 4.4), with golang.org/x/oauth2's clientcredentials,
//     or with the refresh token grant (§ 6), the refresh token read from
//     refresh_token_file, for an API that acts on a user's account, which a
//     user consented to once, such as Gmail's or Microsoft Graph's mail.
//
// Where a login goes is the configuration's alone, never a probe's: login.url
// is absolute, fixed for the collector, or a path from the root resolved
// against the target's scheme, host and port, the only part of it a probe
// chooses. A relative login.url is sent with the request's Host override
// too (hostOverride), the virtual host a target reached by its IP address
// is asked for, which an absolute one, naming its own host, is not. Either
// way the login request is held to the collector's allowed_schemes, allowed_targets and
// denied_targets as the request is, follows no redirect, and is sent with
// login.tls, or the collector's request.tls, never with what a probe
// parameter or a static target's request.insecure_skip_verify changes of it.
// The secrets are read from their files at each login, so a rotated one is
// used by the next.
//
// A token is kept (loginCache), by the collector, the login, the URL it
// was sent to and the Host it was sent with, until shortly before it expires: refreshed when a tenth of
// its life is left, or 30 seconds, whichever comes first, and never later
// than half way through it. A token whose answer says no lifetime is kept
// for login.ttl, or, without it, until the target refuses it. Probes that
// need the same token while it is being fetched wait for that one login. A
// target that answers 401 has the token used dropped, and the request is sent
// once more with a token from a new login; that retry is not one of
// request.retry's attempts, and is made within the probe's deadline.

// loginStage names the stage a failed login is reported under, in logs, the
// probe's answer and a debug report.
const loginStage = "login"

// loginAnswerLimit bounds what is read of a login's answer: a token is a few
// kilobytes at most, and an answer of more is no login answer.
const loginAnswerLimit = 1 << 20

// maxLoginEntries bounds the tokens kept, across collectors: a collector
// whose login.url is relative logs in to each target it is asked about, and
// a probe chooses the target.
const maxLoginEntries = 1000

// loginTimeout bounds a login. A login is shared by every request that needs
// its token while it is made, so it runs under a bound of its own rather
// than the deadline of the request that began it: a probe with a short
// timeout, or a debug probe, would otherwise cut it short for every request
// waiting on it, each of which still returns at its own deadline.
var loginTimeout = 30 * time.Second

// loginNow is the clock a token's lifetime is read by; a test sets it.
var loginNow = time.Now

// loginRefreshMargin is the least of a token's life kept back for its
// refresh, unless that is more than half of it (loginRefreshAt).
const loginRefreshMargin = 30 * time.Second

// loginOf is the login a request of c makes: a static target's own, when it
// sets one, else the collector's, else none.
func loginOf(c *model.Collector, overrides RequestOverrides) *model.LoginConfig {
	if overrides.Login != nil {
		return overrides.Login
	}
	return c.Request.Login
}

// The keys of each kind of login, for the message refusing another kind's.
var (
	jsonLoginKeys   = []string{"fields", "field_files", "token", "token_header", "token_cookie", "expires_in"}
	oauth2LoginKeys = []string{"client_id", "client_id_file", "client_secret_file", "grant_type", "refresh_token_file", "scopes", "endpoint_params", "auth_style"}
)

// loginKindKeys are the keys each kind of login takes of those only some
// kinds take; a steps login takes token and expires_in, as a json login
// does, as the names of values.
var loginKindKeys = map[string][]string{
	"json":   jsonLoginKeys,
	"oauth2": oauth2LoginKeys,
	"steps":  append([]string{"token", "expires_in"}, stepsLoginKeys...),
}

// oauth2OwnParams are the parameters of a token request the grant sets
// itself, which endpoint_params may not set.
var oauth2OwnParams = []string{"grant_type", "scope", "client_id", "client_secret", "refresh_token"}

// validateLogin checks a login block when the configuration, or the static
// target file, loads. It writes nothing into l, which may be a static
// target's in force (CheckTargetRequest). where names the block, such as
// "request.login".
func validateLogin(l *model.LoginConfig, allowedSchemes []string, where string, order []string) error {
	// Of several keys refused, the first in the file is named.
	order = loginKeyOrder(order)
	if !slices.Contains(model.LoginTypes, l.Type) {
		if l.Type == "" {
			return fmt.Errorf("%s has no type; write type: json, to post a JSON login and read the token from its answer, type: oauth2, for the OAuth 2.0 client credentials or refresh token grant, or type: steps, for a login of several requests", where)
		}
		return fmt.Errorf("%s.type is %q; it is json, oauth2 or steps", where, l.Type)
	}
	if l.Type != "steps" {
		if err := checkLoginURL(l.URL, allowedSchemes); err != nil {
			return fmt.Errorf("%s.url %w", where, err)
		}
	}
	set := setKeys(reflect.ValueOf(l).Elem())
	own := loginKindKeys[l.Type]
	for _, key := range model.InFileOrder(set, order) {
		if slices.Contains(own, key) {
			continue
		}
		for _, keys := range loginKindKeys {
			if slices.Contains(keys, key) {
				return fmt.Errorf("%s sets %s, which a login of type %s does not take", where, key, l.Type)
			}
		}
	}
	for i, status := range l.ReloginOn {
		if status < 400 || status > 499 {
			return fmt.Errorf("%s.relogin_on[%d] is %d; it lists the statuses, 4xx, a target answers a token it no longer takes with, such as 401 and 403", where, i, status)
		}
	}
	switch l.Type {
	case "steps":
		if err := checkStepsLogin(l, allowedSchemes, where); err != nil {
			return err
		}
	case "json":
		if err := checkJSONLogin(l, where, order); err != nil {
			return err
		}
	case "oauth2":
		if err := checkOAuth2Login(l, where); err != nil {
			return err
		}
	}
	if l.TTL < 0 {
		return fmt.Errorf("%s.ttl must not be negative", where)
	}
	if l.TokenCookie != "" && (l.Header != "" || l.Prefix != nil) {
		return fmt.Errorf("%s sets token_cookie and %s; a cookie the login set is sent back as the cookie it was, in the Cookie header", where, map[bool]string{true: "header", false: "prefix"}[l.Header != ""])
	}
	if l.Header != "" {
		if !httpguts.ValidHeaderFieldName(l.Header) {
			return fmt.Errorf("%s.header %q is not a header name; a name is letters, digits and !#$%%&'*+-.^_`|~, without spaces", where, l.Header)
		}
		// Every place that shows a request's headers withholds those whose
		// names read as a credential's (CredentialName): a token sent in
		// another would be shown there.
		if !CredentialName(l.Header) {
			return fmt.Errorf("%s.header %q does not read as a credential's name, so a debug report would show the token it carries; send it in Authorization, Cookie, or a header whose name has auth, token, key, session or the like in it, such as X-Auth-Token", where, l.Header)
		}
		switch http.CanonicalHeaderKey(l.Header) {
		case "Host", "Accept-Encoding", "Content-Length", "Content-Type", "Transfer-Encoding", "Connection":
			return fmt.Errorf("%s.header %q is no header to send a token in", where, l.Header)
		}
	}
	if l.Prefix != nil {
		if err := checkHeaderValue(*l.Prefix); err != nil {
			return fmt.Errorf("%s.prefix %w", where, err)
		}
	}
	if l.TLS != nil {
		if err := checkTLSSettings(*l.TLS); err != nil {
			return fmt.Errorf("%s: %w", where, err)
		}
	}
	return nil
}

// checkLoginURL checks login.url: an absolute http or https URL with a host,
// of a scheme the collector allows, or a path from the root, with a query if
// need be, resolved against the target's scheme, host and port. A path
// relative to the target's own would let the target's path, which a probe
// chooses, choose where the secrets go, and give each path a token of its
// own. Neither holds credentials, a fragment or a placeholder: the URL is the
// configuration's, and nothing of a probe's is put in it.
func checkLoginURL(raw string, allowedSchemes []string) error {
	if strings.TrimSpace(raw) == "" {
		return errors.New("is required: the URL the login is sent to, absolute or a path resolved against the target")
	}
	if strings.Contains(raw, "{{") {
		return errors.New("has a placeholder; a login URL is written whole, and nothing of a probe's is put in it")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return errors.New("is not a URL")
	}
	if u.User != nil {
		return errors.New("holds credentials; a login's secrets are read from files")
	}
	if u.Fragment != "" || strings.Contains(raw, "#") {
		return errors.New("has a fragment, which is never sent")
	}
	if u.Scheme == "" {
		if u.Host != "" || strings.HasPrefix(raw, "//") {
			return errors.New("names a host without a scheme; write an absolute URL, such as https://idp.example/token, or a path resolved against the target")
		}
		if !strings.HasPrefix(raw, "/") {
			return fmt.Errorf("%q is relative to the target's path; write a path from the root, such as /api/login, which is resolved against the target's scheme, host and port alone, or an absolute URL", RedactURL(u, MaskQueryValues))
		}
		return nil
	}
	if u.Host == "" {
		return fmt.Errorf("%q has no host; write an absolute URL, such as https://idp.example/token, or a path resolved against the target", RedactURL(u, MaskQueryValues))
	}
	if !strings.EqualFold(u.Scheme, "http") && !strings.EqualFold(u.Scheme, "https") {
		return fmt.Errorf("has the scheme %q; a login is sent over http or https", u.Scheme)
	}
	return checkSchemeAllowed(allowedSchemes, strings.ToLower(u.Scheme))
}

// checkJSONLogin holds a json login's rules: the token read from exactly one
// place, its expressions compiling, and a body of named fields, one of them
// at least read from a file.
func checkJSONLogin(l *model.LoginConfig, where string, order []string) error {
	sources := 0
	for _, source := range []string{l.Token, l.TokenHeader, l.TokenCookie} {
		if source != "" {
			sources++
		}
	}
	if sources != 1 {
		return fmt.Errorf("%s sets %d of token, token_header and token_cookie; a json login reads its token from exactly one: the body, with a jq expression such as .access_token, a header of the answer, or a cookie it sets", where, sources)
	}
	expressions := map[string]string{"token": l.Token, "expires_in": l.ExpiresIn}
	for _, key := range model.InFileOrder([]string{"token", "expires_in"}, order) {
		expression := expressions[key]
		if expression == "" {
			continue
		}
		if strings.TrimSpace(expression) == "" {
			return fmt.Errorf("%s.%s is blank; write a jq expression such as .access_token, or leave the key out", where, key)
		}
		if _, err := expr.CompileJQ(expression); err != nil {
			return fmt.Errorf("%s.%s %q does not compile: %w", where, key, expression, err)
		}
	}
	if l.TokenHeader != "" && !httpguts.ValidHeaderFieldName(l.TokenHeader) {
		return fmt.Errorf("%s.token_header %q is not a header name", where, l.TokenHeader)
	}
	if l.TokenCookie != "" && !httpguts.ValidHeaderFieldName(l.TokenCookie) {
		return fmt.Errorf("%s.token_cookie %q is not a cookie name; a name is letters, digits and !#$%%&'*+-.^_`|~", where, l.TokenCookie)
	}
	if len(l.FieldFiles) == 0 {
		return fmt.Errorf("%s has no field_files; a json login sends at least one secret, read from a file, such as field_files: {password: /var/run/secrets/login/password}", where)
	}
	for _, name := range model.SortedKeys(l.FieldFiles) {
		if name == "" {
			return fmt.Errorf("%s.field_files has a field without a name", where)
		}
		if strings.TrimSpace(l.FieldFiles[name]) == "" {
			return fmt.Errorf("%s.field_files.%s has no file; write the path of the file holding the value", where, name)
		}
		if _, ok := l.Fields[name]; ok {
			return fmt.Errorf("%s sets the field %q in both fields and field_files; set it once", where, name)
		}
	}
	for name := range l.Fields {
		if name == "" {
			return fmt.Errorf("%s.fields has a field without a name", where)
		}
	}
	return nil
}

// checkOAuth2Login holds an oauth2 login's rules: the client's id, in the
// configuration or a file, and its secret, in a file, which a public client
// of the refresh token grant has none of; the grant, client_credentials or
// refresh_token, the latter's refresh token in a file and the former with
// none; scopes without blanks, which separate them in the request; no
// endpoint parameter the grant sets itself; and an auth_style the library
// has.
func checkOAuth2Login(l *model.LoginConfig, where string) error {
	if (l.ClientID == "") == (l.ClientIDFile == "") {
		return fmt.Errorf("%s needs exactly one of client_id and client_id_file", where)
	}
	switch l.GrantType {
	case "", "client_credentials":
		if l.RefreshTokenFile != "" {
			return fmt.Errorf("%s sets refresh_token_file with the client credentials grant, which sends none; set grant_type: refresh_token beside it", where)
		}
		if strings.TrimSpace(l.ClientSecretFile) == "" {
			return fmt.Errorf("%s needs client_secret_file, the file holding the client's secret", where)
		}
	case "refresh_token":
		if strings.TrimSpace(l.RefreshTokenFile) == "" {
			return fmt.Errorf("%s needs refresh_token_file with grant_type: refresh_token, the file holding the refresh token the user's consent gave", where)
		}
		if l.ClientSecretFile != "" && strings.TrimSpace(l.ClientSecretFile) == "" {
			return fmt.Errorf("%s.client_secret_file is blanks alone; name the file of the client's secret, or leave it out for a public client", where)
		}
		if l.ClientSecretFile == "" && l.AuthStyle == "header" {
			return fmt.Errorf("%s sets auth_style: header without client_secret_file; a public client, which has no secret, sends its id as a parameter of the form, so leave auth_style out", where)
		}
	default:
		return fmt.Errorf("%s.grant_type is %q; it is client_credentials, the default, or refresh_token", where, l.GrantType)
	}
	for i, scope := range l.Scopes {
		if scope == "" || strings.ContainsAny(scope, " \t\r\n\"\\") {
			return fmt.Errorf("%s.scopes[%d] %q is no scope; a scope is not empty and holds no blank, quote or backslash (RFC 6749 § 3.3)", where, i, scope)
		}
	}
	for _, name := range model.SortedKeys(l.EndpointParams) {
		if name == "" {
			return fmt.Errorf("%s.endpoint_params has a parameter without a name", where)
		}
		if slices.Contains(oauth2OwnParams, name) {
			return fmt.Errorf("%s.endpoint_params sets %s, which the grant sets itself (scope from scopes, the client's from client_id and client_secret_file, the refresh token from refresh_token_file)", where, name)
		}
	}
	if l.AuthStyle != "" && !slices.Contains(model.LoginAuthStyles, l.AuthStyle) {
		return fmt.Errorf("%s.auth_style is %q; it is header, the client's id and secret as a Basic credential, the default, or params, as parameters of the form", where, l.AuthStyle)
	}
	return nil
}

// checkLoginBeside refuses a login whose header another credential or a
// header of the same request sets too: basic_auth or bearer_token
// beside a login sent in Authorization, and a request.headers entry, the
// collector's or a static target's, of the login's header. Which of the two
// would be sent is not for a reader of the configuration to work out.
func checkLoginBeside(l *model.LoginConfig, basic, bearer bool, headers map[string]string, where string) error {
	header := LoginHeader(l)
	if header == "Authorization" && (basic || bearer) {
		return fmt.Errorf("sets %s and %s, which both send Authorization; use one, or send the login's token in a header of its own with login.header", where, map[bool]string{true: "basic_auth", false: "bearer_token"}[basic])
	}
	for name := range headers {
		if strings.EqualFold(name, header) {
			return fmt.Errorf("sets request.headers %s, which %s sends the token in; leave the header out", name, where)
		}
	}
	return nil
}

// loginKeyOrder is the keys of request.login in the order the file has
// them, of order, the request block's (model.RequestConfig.Order), which
// holds them as login.<key>.
func loginKeyOrder(order []string) []string {
	var keys []string
	for _, path := range order {
		if key, ok := strings.CutPrefix(path, "login."); ok {
			keys = append(keys, key)
		}
	}
	return keys
}

// validateCollectorLogin checks a collector's request.login.
func validateCollectorLogin(x *model.Collector) error {
	l := x.Request.Login
	if l == nil {
		return nil
	}
	if err := validateLogin(l, x.Request.AllowedSchemes, "request.login", x.Request.Order); err != nil {
		return fmt.Errorf("collector %q %w", x.Name, err)
	}
	r := &x.Request
	if err := checkLoginBeside(l, r.BasicAuth != nil || r.BasicAuthFile != nil, r.BearerToken != "" || r.BearerTokenFile != "", r.Headers, "request.login"); err != nil {
		return fmt.Errorf("collector %q %w", x.Name, err)
	}
	return nil
}

// checkTargetLogin checks a static target's request.login, which replaces
// its collector's, against the target's own credentials and headers.
func checkTargetLogin(c *model.Collector, t *model.StaticTarget) error {
	l := t.Request.Login
	if l == nil {
		return nil
	}
	if err := validateLogin(l, c.Request.AllowedSchemes, "request.login", t.Request.Order); err != nil {
		return err
	}
	r := &t.Request
	return checkLoginBeside(l, r.BasicAuth != nil || r.BasicAuthFile != nil, r.BearerToken != "" || r.BearerTokenFile != "", r.Headers, "request.login")
}

// loginURL is where l is sent for a request to target: login.url when it is
// absolute, and otherwise login.url, a path from the root (checkLoginURL),
// on the target's scheme, host and port; nothing else of the target is kept.
func loginURL(l *model.LoginConfig, target string) (*url.URL, error) {
	ref, err := url.Parse(l.URL)
	if err != nil {
		return nil, errors.New("request.login.url is not a URL")
	}
	if ref.IsAbs() {
		return ref, nil
	}
	base, err := url.Parse(normalizeTarget(target))
	if err != nil || base.Host == "" {
		return nil, errors.New("the target is not a URL the login's relative request.login.url can be resolved against")
	}
	if !strings.HasPrefix(l.URL, "/") {
		return nil, errors.New("request.login.url is neither absolute nor a path from the root")
	}
	return (&url.URL{Scheme: base.Scheme, Host: base.Host}).ResolveReference(ref), nil
}

// relativeLoginURL says whether login.url is a path resolved against the
// target (loginURL) rather than an absolute URL.
func relativeLoginURL(l *model.LoginConfig) bool {
	ref, err := url.Parse(l.URL)
	return err == nil && !ref.IsAbs()
}

// firstLoginURL is the login whose url is where l goes first: l, or, for
// a steps login, its first step's.
func firstLoginURL(l *model.LoginConfig) *model.LoginConfig {
	if l.Type == "steps" && len(l.Steps) > 0 {
		return &model.LoginConfig{URL: l.Steps[0].URL}
	}
	return l
}

// relativeLogin says whether a login of l goes to the target: its url, or
// a url of one of its steps, is a path resolved against it. Such a login
// is sent with the request's Host override, and its token kept for each
// target.
func relativeLogin(l *model.LoginConfig) bool {
	if l.Type != "steps" {
		return relativeLoginURL(l)
	}
	for _, steps := range [][]model.LoginStep{l.Steps, l.RefreshSteps} {
		for i := range steps {
			if relativeLoginURL(&model.LoginConfig{URL: steps[i].URL}) {
				return true
			}
		}
	}
	return false
}

// loginFingerprint identifies a login block: two that differ in anything
// keep tokens apart.
func loginFingerprint(l *model.LoginConfig) string {
	b, err := json.Marshal(l)
	if err != nil {
		return fmt.Sprintf("%p", l)
	}
	return string(b)
}

// loginTLS is the tls block a login of l for a request of c is made with:
// the login's own, and otherwise the request's, which is a static target's
// own block when it names one (withTargetTLS).
func loginTLS(c *model.Collector, l *model.LoginConfig) model.TLSConfig {
	if l.TLS != nil {
		return *l.TLS
	}
	return c.Request.TLS
}

// loginTokenFingerprint is what keeps the tokens of l, for a request of c,
// apart from those of other logins: the login block and the tls block it is
// made with. Two static targets that log in with one block but with client
// certificates or CAs of their own make two logins, so neither is sent a
// token the other's certificate was given, nor one taken from a server its
// own roots would not have trusted.
func loginTokenFingerprint(c *model.Collector, l *model.LoginConfig) string {
	return loginFingerprint(l) + "\x00" + fmt.Sprintf("%#v", loginTLS(c, l))
}

// LoginFingerprint identifies a static target's login, for its key in the
// response cache: targets that log in differently share no answer.
func LoginFingerprint(l *model.LoginConfig) string {
	if l == nil {
		return ""
	}
	return loginFingerprint(l)
}

// requestLogin is the login of one request: the header its token is sent
// in, the value sent, and the token's place in the cache, to be dropped and
// fetched again when the target answers 401.
type requestLogin struct {
	header string
	value  string
	key    string
	renew  func(ctx context.Context, used string) (string, error)
	// renewed says the token has been fetched again once for this request.
	renewed bool
	// reloginOn are the statuses that refuse the token, 401 when empty.
	reloginOn []int
}

// refuses reports whether a target's answer of status refuses the token
// it was sent (login.relogin_on).
func (r *requestLogin) refuses(status int) bool {
	if r == nil {
		return false
	}
	if len(r.reloginOn) == 0 {
		return status == http.StatusUnauthorized
	}
	return slices.Contains(r.reloginOn, status)
}

// startLogin is the login of a request of c to target, its token taken from
// the cache or fetched: nil when the request makes none — the collector and
// the static target set no login, or a header the request carries from the
// static target or the probe is the one the login would send, which is
// sent instead. host is the request's Host override (hostOverride), sent on
// a login whose URL is relative: that login goes to the target's address,
// where the virtual host the request asks for is the one that logs it in.
func startLogin(ctx context.Context, target string, c *model.Collector, overrides RequestOverrides, forwarded []http.Header, host string) (*requestLogin, error) {
	l := loginOf(c, overrides)
	if l == nil {
		return nil, nil
	}
	header := LoginHeader(l)
	for _, headers := range forwarded {
		if len(headers.Values(header)) > 0 {
			return nil, nil
		}
	}
	u, err := loginURL(firstLoginURL(l), target)
	if err != nil {
		return nil, &StageError{Stage: loginStage, Err: err}
	}
	if !relativeLogin(l) {
		// An absolute login.url names the host it logs in at.
		host = ""
	}
	fingerprint := loginTokenFingerprint(c, l)
	// The Host is part of the key: two virtual hosts on one address are
	// two logins, and neither is sent the other's token.
	key := c.Name + "\x00" + fingerprint + "\x00" + u.String() + "\x00" + host
	login := func(ctx context.Context) (loginToken, error) {
		return logIn(ctx, c, l, u, target, host, key)
	}
	shown := RedactURL(u, MaskQueryValues)
	get := func(ctx context.Context) (string, error) {
		value, err := logins.get(ctx, key, c.Name, fingerprint, login)
		err = notTargetRefused(err)
		if err != nil {
			return "", &StageError{Stage: loginStage, Err: fmt.Errorf("login at %s failed: %w", shown, err)}
		}
		return value, nil
	}
	value, err := get(ctx)
	if err != nil {
		return nil, err
	}
	return &requestLogin{header: header, value: value, key: key, reloginOn: l.ReloginOn, renew: func(ctx context.Context, used string) (string, error) {
		logins.invalidate(key, used)
		return get(ctx)
	}}, nil
}

// notTargetRefused is err with ErrTargetRefused no longer in its chain: the
// login's URL refused by the collector's policy is the login's failure, not
// the target's, as the target was let through, and is not counted or
// answered as refused. The text is kept, and so is what a probe's caller is
// answered of it (model.CallerText), which a copy of the text alone would
// lose: what the error held for the operator alone would be answered.
func notTargetRefused(err error) error {
	if !errors.Is(err, ErrTargetRefused) {
		return err
	}
	text, shown := err.Error(), model.CallerText(err)
	copied := errors.New(text)
	if shown != text {
		return model.WithOperatorDetail(copied, shown)
	}
	return copied
}

// again drops the token the target refused and fetches another, once for
// the request: it reports whether the request is to be sent again with it.
func (r *requestLogin) again(ctx context.Context) (bool, error) {
	if r == nil || r.renewed {
		return false, nil
	}
	r.renewed = true
	value, err := r.renew(ctx, r.value)
	if err != nil {
		return false, err
	}
	r.value = value
	return true, nil
}

// set puts the token on req.
func (r *requestLogin) set(req *http.Request) {
	if r != nil {
		req.Header.Set(r.header, r.value)
	}
}

// carried reports whether req, the request a target answered — the last of
// its redirects — carried the request's token.
func (r *requestLogin) carried(req *http.Request) bool {
	if r == nil || req == nil {
		return false
	}
	for _, value := range req.Header.Values(r.header) {
		if value == r.value {
			return true
		}
		// A cookie may be sent beside others in the one Cookie header.
		if strings.EqualFold(r.header, "Cookie") {
			for cookie := range strings.SplitSeq(value, ";") {
				if strings.TrimSpace(cookie) == r.value {
					return true
				}
			}
		}
	}
	return false
}

// sends reports whether the request's login sends name, so the collector's
// own credential, which would send the same header, is left out.
func (r *requestLogin) sends(name string) bool {
	return r != nil && strings.EqualFold(r.header, name)
}

// loginToken is what a login gave: the value sent, and when it expires,
// zero when it does not say.
type loginToken struct {
	value   string
	expires time.Time
}

// logIn makes one login of l at u for a request of c, sent with the Host
// host when it is not "".
func logIn(ctx context.Context, c *model.Collector, l *model.LoginConfig, u *url.URL, target, host, key string) (loginToken, error) {
	tlsSettings := loginTLS(c, l)
	// No redirect is followed: the login's secrets go where the
	// configuration says and nowhere a redirect would take them. The
	// login goes through the collector's proxy, as its requests do.
	client, err := HTTPClient(withProxy(TransportSettings{TLS: tlsSettings, EnableHTTP2: c.Request.EnableHTTP2, policy: policyOf(c)}, c), false, 0)
	if err != nil {
		return loginToken{}, err
	}
	var proxy func(*http.Request) (*url.URL, error)
	if transport, ok := client.Transport.(*http.Transport); ok {
		proxy = transport.Proxy
	}
	start := loginNow()
	if l.Type == "steps" {
		// Each step goes where its url says, and is held to the policy
		// there (runLoginStep), the Host override sent with those that go
		// to the target.
		traced := *client
		traced.Transport = loginTracer{next: client.Transport}
		token, err := stepsLogin(ctx, &traced, c, l, target, host, key, start)
		if err != nil {
			return loginToken{}, err
		}
		return finishLoginToken(l, token, start)
	}
	// A login follows no redirect, so whose certificate it presents is
	// never a redirect's refusal to name.
	ctx, err = withTargetPolicy(ctx, c, u, proxy, false, "")
	if err != nil {
		return loginToken{}, err
	}
	traced := *client
	traced.Transport = loginTracer{next: client.Transport, host: host}
	var token loginToken
	switch l.Type {
	case "oauth2":
		token, err = oauth2Login(ctx, &traced, l, u, host, start)
	default:
		token, err = jsonLogin(ctx, &traced, l, u, start)
	}
	if err != nil {
		return loginToken{}, err
	}
	return finishLoginToken(l, token, start)
}

// finishLoginToken is the token a login gave as it is sent and kept: with
// its prefix, and, when the answer said no lifetime, the one its JWT exp
// claim says, else ttl's.
func finishLoginToken(l *model.LoginConfig, token loginToken, start time.Time) (loginToken, error) {
	defaultExpiry(&token, l, start)
	token.value = LoginPrefix(l) + token.value
	if err := checkHeaderValue(token.value); err != nil {
		return loginToken{}, errors.New("the token the login gave holds a control character, which a header may not hold")
	}
	return token, nil
}

// loginTracer sends each login request with the Host host, when it is not
// "", and records it in a debug probe's trace, marked as a login: its
// headers as any request's are, by the rules that withhold a credential's
// value when it is shown, and never its body. The Host is set here, where
// the json and the oauth2 login's requests alike pass; a login follows no
// redirect, so every request it makes is to the URL the Host belongs with.
type loginTracer struct {
	next http.RoundTripper
	host string
}

func (t loginTracer) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.host != "" {
		req = req.Clone(req.Context())
		req.Host = t.host
	}
	// A login's request waits for its turn under the rate limit of the
	// trip that needed the token, as the trip's own requests do: it is a
	// request to its origin like any other.
	waited, err := waitRateLimit(req.Context(), req.URL)
	if err != nil {
		return nil, err
	}
	traceLogin(req.Context(), req.Method, req.URL, req.Header, req.Host)
	traceRateLimitWait(req.Context(), waited, false)
	resp, err := t.next.RoundTrip(req)
	if err != nil {
		traceFailedOutcome(req.Context(), "error: "+RedactURLErrors(err).Error())
		return nil, err
	}
	traceHTTPOutcome(req.Context(), resp)
	return resp, nil
}

// traceLogin records a login request about to be sent in a debug probe's
// trace (requesttrace.go).
func traceLogin(ctx context.Context, method string, u *url.URL, header http.Header, host string) {
	t, ok := ctx.Value(requestTraceKey{}).(*RequestTrace)
	if !ok {
		return
	}
	if t.spans {
		// An exported trip's spans show no headers (requesttrace.go).
		header = nil
	} else {
		header = header.Clone()
		if header == nil {
			header = http.Header{}
		}
		// Host is shown when it is not the URL's own, as traceRequest shows it.
		if host != "" && host != u.Host {
			header.Set("Host", host)
		}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.requests = append(t.requests, TracedRequest{Method: method, URL: u.String(), Header: header, Login: true, started: time.Now()})
}

// jsonLogin posts the fields of l to u and reads the token from the answer.
func jsonLogin(ctx context.Context, client *http.Client, l *model.LoginConfig, u *url.URL, start time.Time) (loginToken, error) {
	body := make(map[string]string, len(l.Fields)+len(l.FieldFiles))
	for name, value := range l.Fields {
		body[name] = value
	}
	for _, name := range model.SortedKeys(l.FieldFiles) {
		value, err := ReadCredentialFile(l.FieldFiles[name])
		if err != nil {
			return loginToken{}, fmt.Errorf("reading login.field_files.%s: %w", name, err)
		}
		if value == "" {
			return loginToken{}, fmt.Errorf("login.field_files.%s file %s is empty", name, l.FieldFiles[name])
		}
		body[name] = value
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return loginToken{}, err
	}
	// u is login.url from the configuration, resolved against the target
	// (loginURL), and logIn holds the request to the collector's target
	// policy (withTargetPolicy) before it is sent; a probe cannot set or
	// change it.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(payload)) // #nosec G704 -- the configured login URL, checked against the target policy
	if err != nil {
		return loginToken{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req) // #nosec G704 -- the configured login URL, checked against the target policy
	if err != nil {
		return loginToken{}, withAnswerWithheld(RedactURLErrors(err))
	}
	answer, readErr := io.ReadAll(io.LimitReader(resp.Body, loginAnswerLimit+1))
	_ = resp.Body.Close()
	if readErr != nil {
		return loginToken{}, fmt.Errorf("reading the login's answer: %w", RedactURLErrors(readErr))
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return loginToken{}, answeredError(resp.StatusCode, resp.Header.Get("Location") != "", answer)
	}
	if len(answer) > loginAnswerLimit {
		return loginToken{}, fmt.Errorf("the login's answer is larger than %d bytes", loginAnswerLimit)
	}
	var document any
	decoded := false
	decode := func() (any, error) {
		if !decoded {
			decoder := json.NewDecoder(bytes.NewReader(answer))
			decoder.UseNumber()
			if err := decoder.Decode(&document); err != nil {
				return nil, errors.New("the login's answer is not JSON")
			}
			decoded = true
		}
		return document, nil
	}
	var token loginToken
	switch {
	case l.TokenHeader != "":
		token.value = resp.Header.Get(l.TokenHeader)
		if token.value == "" {
			return loginToken{}, fmt.Errorf("the login's answer has no %s header, which login.token_header names", http.CanonicalHeaderKey(l.TokenHeader))
		}
	case l.TokenCookie != "":
		for _, cookie := range resp.Cookies() {
			if cookie.Name != l.TokenCookie || cookie.Value == "" {
				continue
			}
			token.value = cookie.Value
			switch {
			case cookie.MaxAge > 0:
				token.expires = start.Add(time.Duration(cookie.MaxAge) * time.Second)
			case !cookie.Expires.IsZero():
				token.expires = cookie.Expires
			}
		}
		if token.value == "" {
			return loginToken{}, fmt.Errorf("the login's answer sets no cookie %s, which login.token_cookie names", l.TokenCookie)
		}
	default:
		page, err := decode()
		if err != nil {
			return loginToken{}, err
		}
		// What the expression gave is the token, or text near it: an
		// error says what kind of value it was, never the value.
		value, kind, err := jqOne(ctx, l.Token, page, resp)
		if err != nil {
			return loginToken{}, fmt.Errorf("login.token %q %w", l.Token, err)
		}
		text, ok := value.(string)
		if !ok || text == "" {
			return loginToken{}, fmt.Errorf("login.token %q gave %s; it must give the token, text that is not empty", l.Token, kind)
		}
		token.value = text
	}
	if l.ExpiresIn != "" {
		page, err := decode()
		if err != nil {
			return loginToken{}, err
		}
		value, kind, err := jqOne(ctx, l.ExpiresIn, page, resp)
		if err != nil {
			return loginToken{}, fmt.Errorf("login.expires_in %q %w", l.ExpiresIn, err)
		}
		if value != nil {
			seconds, ok := loginSeconds(value)
			if !ok {
				return loginToken{}, fmt.Errorf("login.expires_in %q gave %s; it must give the token's lifetime in seconds, a number above 0, or null when the answer does not say", l.ExpiresIn, kind)
			}
			token.expires = start.Add(seconds)
		}
	}
	return token, nil
}

// answeredError is the error of a login answered with a status that is not
// 2xx: the status, and the start of what the server said, with what reads as
// a credential masked. A redirect is named as one, since it is not followed.
//
// What the server said is the operator's: an identity provider's refusal
// may name the user, the client or the directory it asked, and a probe's
// caller is answered the status alone, and where the rest is
// (model.WithOperatorDetail).
func answeredError(status int, redirect bool, body []byte) error {
	if redirect || status >= 300 && status < 400 {
		return fmt.Errorf("the login was answered with the redirect %d, which a login does not follow; login.url must be the URL that answers the login itself", status)
	}
	answered := fmt.Sprintf("the login was answered with HTTP status %d", status)
	if excerpt := loginExcerpt(body); excerpt != "" {
		return model.WithOperatorDetail(fmt.Errorf("%s: %s", answered, excerpt), answered+loginAnswerLogged)
	}
	return errors.New(answered)
}

// loginAnswerLogged is what a probe's caller is told of the answer a
// refused login had: where it is.
const loginAnswerLogged = "; what it said is in the exporter's log"

// oauth2ErrorCodes are the error codes RFC 6749 § 5.2 registers for a token
// endpoint's refusal. A probe's caller is answered one of these; any other
// code is the provider's own text, and is the operator's, as its
// error_description is.
var oauth2ErrorCodes = map[string]bool{
	"invalid_request": true, "invalid_client": true, "invalid_grant": true,
	"unauthorized_client": true, "unsupported_grant_type": true, "invalid_scope": true,
}

// oauth2RefusedError is the error of a token endpoint that answered with an
// RFC 6749 error: the status, the code and the start of its
// error_description. A probe's caller is answered the status, the code when
// RFC 6749 registers it, and where the rest is.
func oauth2RefusedError(status int, code, description string) error {
	answered := fmt.Sprintf("the token endpoint answered HTTP status %d", status)
	text := answered + ", " + loginExcerpt([]byte(code))
	if description != "" {
		text += ": " + loginExcerpt([]byte(description))
	}
	shown := answered + " with an error code RFC 6749 does not register"
	if oauth2ErrorCodes[code] {
		shown = answered + ", " + code
		if description == "" {
			return errors.New(text)
		}
	}
	return model.WithOperatorDetail(errors.New(text), shown+loginAnswerLogged)
}

// loginExcerptBytes is how much of a refused login's answer its error shows.
const loginExcerptBytes = 256

// loginExcerpt is the start of a login's answer as its error shows it: on
// one line, with what reads as a credential masked (RedactText), and cut.
func loginExcerpt(body []byte) string {
	if len(body) > 4*loginExcerptBytes {
		body = body[:4*loginExcerptBytes]
	}
	text := strings.Join(strings.Fields(strings.ToValidUTF8(string(body), " ")), " ")
	text = RedactText(text)
	if len(text) > loginExcerptBytes {
		text = strings.ToValidUTF8(text[:loginExcerptBytes], "") + "…"
	}
	return text
}

// jqOne runs a login's jq expression on its answer, with $status and
// $headers as a rule has them, and returns its one value and the kind of
// it. An error says what went wrong without the value gojq's own words
// would quote, which may be the token.
func jqOne(ctx context.Context, expression string, page any, resp *http.Response) (value any, kind string, err error) {
	code, err := expr.CompileJQ(expression)
	if err != nil {
		return nil, "", err
	}
	// Within the login's deadline, and no further than a second value,
	// which is enough to refuse it: an expression that never ends, or gives
	// values without end, holds no scrape for ever.
	iterator := code.RunWithContext(ctx, page, expr.JQBindings{Status: resp.StatusCode, Headers: jqHeaders(resp.Header)}.Values(page)...)
	count := 0
	for count < 2 {
		v, ok := iterator.Next()
		if !ok {
			break
		}
		if _, failed := v.(error); failed {
			return nil, "", errors.New("failed on the login's answer")
		}
		if count == 0 {
			value = v
		}
		count++
	}
	if count != 1 {
		if count == 0 {
			return nil, "", errors.New("gave no value; it must give one")
		}
		return nil, "", errors.New("gave more than one value; it must give one")
	}
	return value, jsonKind(value), nil
}

// loginSeconds reads a lifetime in seconds: a number above 0, or text of
// one, as some servers write expires_in.
func loginSeconds(value any) (time.Duration, bool) {
	var seconds float64
	switch v := value.(type) {
	case json.Number:
		f, err := v.Float64()
		if err != nil {
			return 0, false
		}
		seconds = f
	case int:
		seconds = float64(v)
	case float64:
		seconds = v
	case *big.Int:
		f, _ := new(big.Float).SetInt(v).Float64()
		seconds = f
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		if err != nil {
			return 0, false
		}
		seconds = f
	default:
		return 0, false
	}
	if math.IsNaN(seconds) || seconds <= 0 {
		return 0, false
	}
	// A lifetime too long for a duration is as good as none.
	if seconds > float64(math.MaxInt64/int64(time.Second)) {
		return time.Duration(math.MaxInt64), true
	}
	return time.Duration(seconds * float64(time.Second)), true
}

// oauth2Login asks the token endpoint u for a token with the client
// credentials grant, or with the refresh token grant: the same request of
// the form, its grant_type refresh_token and the refresh token beside it,
// which clientcredentials sends as it does its own (it lets grant_type be
// another's). A public client, with no secret, sends its id as a parameter.
func oauth2Login(ctx context.Context, client *http.Client, l *model.LoginConfig, u *url.URL, host string, start time.Time) (loginToken, error) {
	id := l.ClientID
	if l.ClientIDFile != "" {
		value, err := ReadCredentialFile(l.ClientIDFile)
		if err != nil {
			return loginToken{}, fmt.Errorf("reading login.client_id_file: %w", err)
		}
		if value == "" {
			return loginToken{}, fmt.Errorf("login.client_id_file %s is empty", l.ClientIDFile)
		}
		id = value
	}
	var secret string
	if l.ClientSecretFile != "" {
		value, err := ReadCredentialFile(l.ClientSecretFile)
		if err != nil {
			return loginToken{}, fmt.Errorf("reading login.client_secret_file: %w", err)
		}
		if value == "" {
			return loginToken{}, fmt.Errorf("login.client_secret_file %s is empty", l.ClientSecretFile)
		}
		secret = value
	}
	style := oauth2.AuthStyleInHeader
	if l.AuthStyle == "params" || secret == "" {
		style = oauth2.AuthStyleInParams
	}
	params := url.Values{}
	for name, value := range l.EndpointParams {
		params.Set(name, value)
	}
	var refresh oauth2Refresh
	if l.GrantType == "refresh_token" {
		var err error
		if refresh, err = oauth2RefreshToken(l, u, host); err != nil {
			return loginToken{}, err
		}
		params.Set("grant_type", "refresh_token")
		params.Set("refresh_token", refresh.sent)
	}
	config := clientcredentials.Config{ClientID: id, ClientSecret: secret, TokenURL: u.String(), Scopes: l.Scopes, EndpointParams: params, AuthStyle: style}
	token, err := config.Token(context.WithValue(ctx, oauth2.HTTPClient, client))
	if err != nil {
		var refused *oauth2.RetrieveError
		if errors.As(err, &refused) && refused.Response != nil {
			status := refused.Response.StatusCode
			if refused.ErrorCode != "" {
				return loginToken{}, oauth2RefusedError(status, refused.ErrorCode, refused.ErrorDescription)
			}
			return loginToken{}, answeredError(status, refused.Response.Header.Get("Location") != "", refused.Body)
		}
		// The library's own words: a connection that failed quotes the
		// URL, whose query values are masked; a token missing from the
		// answer says so.
		said := errors.New(RedactText(RedactURLErrors(err).Error()))
		var failed *url.Error
		if errors.As(err, &failed) || err.Error() == oauth2EmptyAnswer {
			// A failed connection's words quote the first line of an
			// answer that is no HTTP, as the target's do (answerquoted.go):
			// read before RedactText, which may mask Go's words.
			if told, quoted := toldOfGoQuoting(RedactURLErrors(err).Error()); quoted {
				return loginToken{}, model.WithOperatorDetail(said, told)
			}
			return loginToken{}, said
		}
		// Any other is an answer the library could not read, and its
		// words quote what in it could not be: the endpoint's text, which
		// is the operator's.
		return loginToken{}, model.WithOperatorDetail(said, "the token endpoint's answer could not be read as a token"+loginAnswerLogged)
	}
	if l.GrantType == "refresh_token" {
		refresh.answered(token.RefreshToken)
	}
	out := loginToken{value: token.AccessToken}
	if !token.Expiry.IsZero() {
		// The library reads expires_in against its own clock; the
		// lifetime is counted from when the login began, as a json
		// login's is.
		out.expires = start.Add(time.Until(token.Expiry))
	}
	return out, nil
}

// oauth2Refresh is the refresh token a login sends: the file's, or the
// one the endpoint answered a login of the same file with in its place,
// and where the login's key is kept.
type oauth2Refresh struct {
	key, file, sent string
}

// oauth2Rotated are the refresh tokens token endpoints answered in place
// of the ones sent, by the login block and the endpoint they were answered
// to: an endpoint that rotates its refresh tokens, as Microsoft's identity
// platform and Okta's and Auth0's with rotation on do, takes the new one
// from then on, and may refuse the old. The new one is sent until the
// file changes, which is the operator putting another in place; the file
// itself is never written, a Secret mounted read-only being where it
// lives. At most maxLoginEntries are kept, as tokens are.
var oauth2Rotated = struct {
	sync.Mutex
	byLogin map[string]oauth2Rotation
}{byLogin: map[string]oauth2Rotation{}}

// oauth2Rotation is a refresh token answered in place of the file's.
type oauth2Rotation struct {
	file, current string
}

// oauth2RefreshToken reads the refresh token a login of l to the endpoint
// u, at the virtual host host when it is not "", sends: the file's, unless
// that endpoint answered one in its place since, and the file still holds
// what it did then. A relative login.url is another endpoint at each
// target, and at each virtual host of one, whose refresh tokens are its
// own.
func oauth2RefreshToken(l *model.LoginConfig, u *url.URL, host string) (oauth2Refresh, error) {
	file, err := ReadCredentialFile(l.RefreshTokenFile)
	if err != nil {
		return oauth2Refresh{}, fmt.Errorf("reading login.refresh_token_file: %w", err)
	}
	if file == "" {
		return oauth2Refresh{}, fmt.Errorf("login.refresh_token_file %s is empty", l.RefreshTokenFile)
	}
	r := oauth2Refresh{key: loginFingerprint(l) + "\x00" + u.String() + "\x00" + host, file: file, sent: file}
	oauth2Rotated.Lock()
	defer oauth2Rotated.Unlock()
	if rotation, ok := oauth2Rotated.byLogin[r.key]; ok && rotation.file == file {
		r.sent = rotation.current
	}
	return r, nil
}

// answered keeps the refresh token the endpoint answered, when it is
// another than the one sent, for the next login.
func (r oauth2Refresh) answered(token string) {
	if token == "" || token == r.sent {
		return
	}
	oauth2Rotated.Lock()
	defer oauth2Rotated.Unlock()
	if _, known := oauth2Rotated.byLogin[r.key]; !known && len(oauth2Rotated.byLogin) >= maxLoginEntries {
		clear(oauth2Rotated.byLogin)
	}
	oauth2Rotated.byLogin[r.key] = oauth2Rotation{file: r.file, current: token}
}

// oauth2EmptyAnswer is the library's error for an answer without a token,
// which quotes nothing of it.
const oauth2EmptyAnswer = "oauth2: server response missing access_token"

// loginCache holds the tokens logins gave, and how many logins each
// collector made and how many failed, for its self-metrics.
type loginCache struct {
	mu      sync.Mutex
	entries map[string]*loginEntry
	made    map[string]uint64
	failed  map[string]uint64
	// refreshed counts the logins of each collector that a steps login's
	// refresh_steps made (loginsteps.go).
	refreshed map[string]uint64
}

// loginEntry is one token kept, or being fetched.
type loginEntry struct {
	collector   string
	fingerprint string
	// value is the header value the token makes, "" while there is none;
	// expires and refresh are when it expires and when it is fetched
	// again, zero when its life is not known.
	value            string
	expires, refresh time.Time
	// call is the login in progress, nil when none is.
	call *loginCall
	used time.Time
}

// loginCall is one login, which every request needing its token waits for.
type loginCall struct {
	done  chan struct{}
	value string
	err   error
	// counted says the login is counted in the collector's self-metrics:
	// made for a request that is no debug run's, or waited for by one.
	counted bool
}

var logins = &loginCache{entries: map[string]*loginEntry{}, made: map[string]uint64{}, failed: map[string]uint64{}, refreshed: map[string]uint64{}}

// loginRefreshAt is when a token obtained at start that expires at expires
// is fetched again: when a tenth of its life is left, or 30 seconds,
// whichever is more, but never before half its life has passed.
func loginRefreshAt(start, expires time.Time) time.Time {
	life := expires.Sub(start)
	margin := min(max(life/10, loginRefreshMargin), life/2)
	return expires.Add(-margin)
}

// get returns the token kept under key while it is fresh, and otherwise
// makes the login, once for every request that asks while it is made. The
// login is made apart from the request that began it, within loginTimeout,
// so a probe that goes away, or whose deadline is near, leaves it to those
// still waiting; each waits within its own deadline. A login that fails
// while the token kept has not expired leaves that token in use, counted as
// a failed login.
func (c *loginCache) get(ctx context.Context, key, collector, fingerprint string, login func(context.Context) (loginToken, error)) (string, error) {
	c.mu.Lock()
	now := loginNow()
	e := c.entries[key]
	if e == nil {
		c.evictLocked()
		e = &loginEntry{collector: collector, fingerprint: fingerprint}
		c.entries[key] = e
	}
	e.used = now
	if e.value != "" && (e.refresh.IsZero() || now.Before(e.refresh)) {
		value := e.value
		c.mu.Unlock()
		return value, nil
	}
	call := e.call
	counted := !debugRun(ctx)
	if call == nil {
		call = &loginCall{done: make(chan struct{})}
		e.call = call
		detached, cancel := context.WithTimeout(context.WithoutCancel(ctx), loginTimeout)
		go func() {
			defer cancel()
			start := loginNow()
			token, err := login(detached)
			c.finish(e, call, start, token, err)
		}()
	}
	// A debug run uses and fills the token kept, which is the collector's,
	// but its login is not counted (§ 42.17), unless a request that is no
	// debug run's waits for it too.
	if counted && !call.counted {
		call.counted = true
		c.made[collector]++
	}
	c.mu.Unlock()
	select {
	case <-call.done:
		return call.value, call.err
	case <-ctx.Done():
		return "", context.Cause(ctx)
	}
}

// finish records what a login gave and hands it to those waiting.
func (c *loginCache) finish(e *loginEntry, call *loginCall, start time.Time, token loginToken, err error) {
	c.mu.Lock()
	if err != nil {
		// A collector a reload removed meanwhile has no counts left to
		// raise (follow).
		if _, followed := c.made[e.collector]; followed && call.counted {
			c.failed[e.collector]++
		}
		if e.value != "" && (e.expires.IsZero() || loginNow().Before(e.expires)) {
			call.value = e.value
		} else {
			call.err = err
		}
	} else {
		e.value, e.expires, e.refresh = token.value, token.expires, time.Time{}
		if !token.expires.IsZero() {
			e.refresh = loginRefreshAt(start, token.expires)
		}
		call.value = token.value
	}
	e.call = nil
	c.mu.Unlock()
	close(call.done)
}

// invalidate drops the token kept under key when it is used, the one a
// target refused; a newer one another request fetched meanwhile stays.
func (c *loginCache) invalidate(key, used string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e := c.entries[key]; e != nil && e.value == used {
		e.value, e.expires, e.refresh = "", time.Time{}, time.Time{}
	}
}

// evictLocked makes room for one more entry when the cache is full, by
// dropping the one used longest ago that no login is being made for.
func (c *loginCache) evictLocked() {
	if len(c.entries) < maxLoginEntries {
		return
	}
	oldest := ""
	var at time.Time
	for key, e := range c.entries {
		if e.call != nil {
			continue
		}
		if oldest == "" || e.used.Before(at) {
			oldest, at = key, e.used
		}
	}
	if oldest != "" {
		delete(c.entries, oldest)
	}
}

// follow drops the tokens of logins the configuration in force no longer
// has — a collector removed, or its login, or a static target's, or the tls
// block one is made with, changed — and the counts of the collectors it no longer has.
func (c *loginCache) follow(collectors []model.Collector, targets []model.StaticTarget) {
	wanted := map[string]map[string]bool{}
	names := map[string]bool{}
	byName := map[string]*model.Collector{}
	for i := range collectors {
		col := &collectors[i]
		names[col.Name] = true
		byName[col.Name] = col
		wanted[col.Name] = map[string]bool{}
		if col.Request.Login != nil {
			wanted[col.Name][loginTokenFingerprint(col, col.Request.Login)] = true
		}
	}
	// A static target logs in with its own login or the collector's, and
	// with its own tls block or the collector's.
	for i := range targets {
		col := byName[targets[i].Collector]
		if col == nil {
			continue
		}
		overrides := RequestOverrides{Login: targets[i].Request.Login, TLS: targets[i].Request.TLS}
		if l := loginOf(col, overrides); l != nil {
			wanted[col.Name][loginTokenFingerprint(withTargetTLS(col, overrides), l)] = true
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	kept := map[string]bool{}
	for key, e := range c.entries {
		if !wanted[e.collector][e.fingerprint] {
			delete(c.entries, key)
			continue
		}
		kept[key] = true
	}
	// A steps login keeps its values as long as its token is kept.
	stepsStates.keepOnly(kept)
	for _, counts := range []map[string]uint64{c.made, c.failed, c.refreshed} {
		for name := range counts {
			if !names[name] {
				delete(counts, name)
			}
		}
	}
}

// reset forgets every token and count, for the exporter's shutdown: the
// counts are of the exporter's life, as every other self-metric's.
func (c *loginCache) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = map[string]*loginEntry{}
	c.made, c.failed, c.refreshed = map[string]uint64{}, map[string]uint64{}, map[string]uint64{}
	stepsStates.reset()
	oauth2Rotated.Lock()
	clear(oauth2Rotated.byLogin)
	oauth2Rotated.Unlock()
}

// refreshes reports how many logins of a collector refresh_steps made.
func (c *loginCache) refreshes(collector string) uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.refreshed[collector]
}

// noteRefresh counts a login refresh_steps made for collector, when it is
// counted as the login is (get): not a debug run's alone.
func (c *loginCache) noteRefresh(ctx context.Context, collector string) {
	if debugRun(ctx) {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	// A collector a reload removed meanwhile has no counts left to raise.
	if _, followed := c.made[collector]; followed {
		c.refreshed[collector]++
	}
}

// counts reports how many logins a collector made and how many failed.
func (c *loginCache) counts(collector string) (made, failed uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.made[collector], c.failed[collector]
}

// loginFiles says what a login's files hold that a login would fail on now,
// for --dry-run (RequestType.CheckFiles): one missing, unreadable or empty.
func loginFiles(l *model.LoginConfig, who string) []string {
	if l == nil {
		return nil
	}
	files := map[string]string{"client_id_file": l.ClientIDFile, "client_secret_file": l.ClientSecretFile, "refresh_token_file": l.RefreshTokenFile}
	for name, path := range l.FieldFiles {
		files["field_files."+name] = path
	}
	for name, path := range l.SecretFiles {
		files["secret_files."+name] = path
	}
	var warnings []string
	for _, key := range model.SortedKeys(files) {
		path := files[key]
		if path == "" {
			continue
		}
		value, err := ReadCredentialFile(path)
		switch {
		case err != nil:
			warnings = append(warnings, fmt.Sprintf("%s request.login.%s: %v; a login would fail on it now (the file is read at each login, so one put in place later is used then)", who, key, err))
		case value == "":
			warnings = append(warnings, fmt.Sprintf("%s request.login.%s %s is empty; a login would fail on it now", who, key, path))
		}
	}
	return append(warnings, keepFileFiles(l, who)...)
}

// checkLoginFiles is RequestType.CheckFiles of the types that log in: the
// files of the collector's login, or of the static target's when t is set.
func checkLoginFiles(c *model.Collector, t *model.StaticTarget) []string {
	if t != nil {
		return loginFiles(t.Request.Login, fmt.Sprintf("target %q", t.Name))
	}
	return loginFiles(c.Request.Login, fmt.Sprintf("collector %q", c.Name))
}
