//go:build request_type_smtp || request_type_imap || request_type_pop3

package fetch

import (
	"context"
	"crypto/hmac"
	"crypto/md5" //nolint:gosec // G501: CRAM-MD5 (RFC 2195) is HMAC-MD5, which a server may speak alone
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// How the types that log in to a mail server, smtp, imap and pop3, choose their
// login: request.auth names the mechanism. plain, login and cram-md5 go
// with request.username and request.password_file; xoauth2 and oauthbearer
// with request.username and request.xoauth2, which says where the OAuth
// 2.0 access token comes from.

const (
	netAuthPlain       = "plain"
	netAuthLogin       = "login"
	netAuthCRAMMD5     = "cram-md5"
	netAuthXOAuth2     = "xoauth2"
	netAuthOAuthBearer = "oauthbearer"

	// The grants request.xoauth2 gets a token with at token_url.
	netGrantRefreshToken      = "refresh_token"
	netGrantClientCredentials = "client_credentials"

	// netOAuthAnswerLimit is the most of a token endpoint's answer that is
	// read: a token and its lifetime are a few kilobytes.
	netOAuthAnswerLimit = 64 << 10
)

// netAuthMechanisms are the mechanisms every mail type speaks, as
// request.auth names them.
var netAuthMechanisms = []string{netAuthPlain, netAuthLogin, netAuthCRAMMD5, netAuthXOAuth2, netAuthOAuthBearer}

// netAuthWithToken says whether a mechanism sends an access token, and no
// password.
func netAuthWithToken(mechanism string) bool {
	return mechanism == netAuthXOAuth2 || mechanism == netAuthOAuthBearer
}

// checkNetLogin holds a collector's auth, xoauth2, username and
// password_file to one another, and fills in auth: xoauth2 for a collector
// that sets the block alone. sends says, in a refusal, why a password
// goes with a name: "AUTH sends both". more are the mechanisms the type
// speaks beside netAuthMechanisms, each with a password.
func checkNetLogin(x *model.Collector, sends string, more ...string) error {
	r := &x.Request
	if r.Auth != "" && !slices.Contains(netAuthMechanisms, r.Auth) && !slices.Contains(more, r.Auth) {
		spoken := append(slices.Clone(more), netAuthMechanisms...)
		return fmt.Errorf("collector %q request.auth %q is not %s or %s", x.Name, r.Auth, strings.Join(spoken[:len(spoken)-1], ", "), spoken[len(spoken)-1])
	}
	if r.XOAuth2 != nil && r.Auth == "" {
		r.Auth = netAuthXOAuth2
	}
	withToken := netAuthWithToken(r.Auth)
	switch {
	case withToken && r.XOAuth2 == nil && !x.RequestNotMade:
		return fmt.Errorf("collector %q request.auth is %s without request.xoauth2, which says where the access token comes from: token_file, or token_url with refresh_token_file or grant: client_credentials", x.Name, r.Auth)
	case !withToken && r.XOAuth2 != nil:
		return fmt.Errorf("collector %q sets request.xoauth2, but request.auth is %s; leave auth out, or write xoauth2 or oauthbearer", x.Name, r.Auth)
	case withToken && r.PasswordFile != "":
		return fmt.Errorf("collector %q sets request.password_file beside xoauth2, which logs in with an access token and no password", x.Name)
	case (withToken || r.PasswordFile != "" || r.Auth != "") && r.Username == "":
		return fmt.Errorf("collector %q logs in without request.username; name the account, for Gmail the mailbox's address", x.Name)
	case r.Username != "" && !withToken && r.PasswordFile == "" && !x.RequestNotMade:
		return fmt.Errorf("collector %q sets request.username without request.password_file; %s, so name the file of the password, or request.xoauth2 for a token", x.Name, sends)
	}
	if o := r.XOAuth2; o != nil {
		if err := checkNetXOAuth2(o); err != nil {
			return fmt.Errorf("collector %q %w", x.Name, err)
		}
	}
	return nil
}

// checkNetTargetLogin holds a static target's own account to how its
// collector logs in. A collector that logs in with a token sends no
// password, so a target's password_file would be read and never sent, and
// is refused; its username alone is the mailbox the collector's token logs
// in to, as an application's token does for many (withTargetAccount).
func checkNetTargetLogin(c *model.Collector, t *model.StaticTarget) error {
	if (c.Request.XOAuth2 != nil || netAuthWithToken(c.Request.Auth)) && t.Request.PasswordFile != "" {
		return fmt.Errorf("request.password_file is set, but collector %q logs in with xoauth2, which sends its access token and no password; name the target's mailbox with request.username alone, or give it a collector of its own", c.Name)
	}
	return checkNetTargetAccount(t)
}

// checkNetXOAuth2 holds request.xoauth2 to one source of the token: a
// file, or a grant with its endpoint and client, the refresh token grant
// with its refresh token and the client credentials grant with its
// secret.
func checkNetXOAuth2(o *model.XOAuth2Config) error {
	grant := o.Grant != "" || o.TokenURL != "" || o.ClientID != "" || o.ClientIDFile != "" || o.ClientSecretFile != "" || o.RefreshTokenFile != "" || len(o.Scopes) > 0
	switch {
	case o.Grant != "" && o.Grant != netGrantRefreshToken && o.Grant != netGrantClientCredentials:
		return fmt.Errorf("request.xoauth2.grant %q is not refresh_token or client_credentials", o.Grant)
	case o.TokenFile != "" && grant:
		return errors.New("request.xoauth2 sets token_file beside the keys of a grant; the token comes from the file or from token_url, not both")
	case o.TokenFile != "":
		return nil
	case o.TokenURL == "":
		return errors.New("request.xoauth2 names neither token_file nor token_url; say where the access token comes from")
	case o.Grant == netGrantClientCredentials && o.RefreshTokenFile != "":
		return errors.New("request.xoauth2 sets refresh_token_file beside grant: client_credentials, which asks for a token with the client's id and secret alone")
	case o.Grant == netGrantClientCredentials && o.ClientSecretFile == "":
		return errors.New("request.xoauth2 sets grant: client_credentials without client_secret_file, the secret the endpoint knows the client by")
	case o.Grant != netGrantClientCredentials && o.RefreshTokenFile == "":
		return errors.New("request.xoauth2 sets token_url without refresh_token_file, the refresh token the endpoint trades for an access token; an application's own token is grant: client_credentials")
	case (o.ClientID == "") == (o.ClientIDFile == ""):
		return errors.New("request.xoauth2 needs one of client_id and client_id_file")
	}
	u, err := url.Parse(o.TokenURL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" {
		return fmt.Errorf("request.xoauth2.token_url %q is not an https:// URL of a token endpoint, without credentials or a fragment", RedactText(o.TokenURL))
	}
	return nil
}

// netXOAuth2Credential is what an XOAUTH2 login sends, before base64.
func netXOAuth2Credential(user, token string) string {
	return "user=" + user + "\x01auth=Bearer " + token + "\x01\x01"
}

// netOAuthBearerCredential is what an OAUTHBEARER login sends, before
// base64 (RFC 7628 § 3.1): the account as the authorization identity, its
// = and , written as RFC 5801 writes them, the server's host and port, and
// the token.
func netOAuthBearerCredential(user, hostPort, token string) string {
	host, port, err := net.SplitHostPort(hostPort)
	if err != nil {
		host, port = hostPort, ""
	}
	message := "n,a=" + strings.NewReplacer("=", "=3D", ",", "=2C").Replace(user) + ",\x01host=" + host
	if port != "" {
		message += "\x01port=" + port
	}
	return message + "\x01auth=Bearer " + token + "\x01\x01"
}

// netOAuthBearerEnd is what ends an OAUTHBEARER login the server refused:
// its refusal comes as a challenge that holds the error, which the client
// answers with one byte, %x01, here in base64 (RFC 7628 § 3.2.3).
const netOAuthBearerEnd = "AQ=="

// netCRAMMD5Response is the answer to a CRAM-MD5 challenge, both in
// base64 (RFC 2195): the account and the HMAC-MD5 of the challenge keyed
// by the password, in hexadecimal.
func netCRAMMD5Response(user, password, challenge string) (string, error) {
	text, err := base64.StdEncoding.DecodeString(strings.TrimSpace(challenge))
	if err != nil {
		return "", errors.New("the challenge is not base64")
	}
	mac := hmac.New(md5.New, []byte(password))
	_, _ = mac.Write(text)
	return base64.StdEncoding.EncodeToString([]byte(user + " " + hex.EncodeToString(mac.Sum(nil)))), nil
}

// netTokenCache keeps the access token the refresh token grant gave each
// collector until shortly before it expires.
type netTokenCache struct {
	mu     sync.Mutex
	tokens map[string]netToken
}

type netToken struct {
	value   string
	expires time.Time
}

var netTokens = &netTokenCache{tokens: map[string]netToken{}}

// netTokenMargin is how long before its expiry a token is fetched again.
const netTokenMargin = time.Minute

func netTokenKey(collector string, o *model.XOAuth2Config) string {
	return strings.Join([]string{collector, o.Grant, o.TokenURL, o.ClientID, o.ClientIDFile, o.ClientSecretFile, o.RefreshTokenFile, strings.Join(o.Scopes, " ")}, "\x00")
}

func (c *netTokenCache) forget(collector string, o *model.XOAuth2Config) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.tokens, netTokenKey(collector, o))
}

// netOAuthClient makes the token request; tests replace it.
var netOAuthClient = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

// netAccessToken is the access token an XOAUTH2 or OAUTHBEARER login
// sends: what token_file holds, read at every scrape, or what the grant
// answers, kept until a minute before it expires.
func netAccessToken(ctx context.Context, collector string, o *model.XOAuth2Config) (string, error) {
	if o.TokenFile != "" {
		token, err := ReadCredentialFile(o.TokenFile)
		if err != nil {
			return "", fmt.Errorf("reading request.xoauth2.token_file: %w", err)
		}
		if token == "" {
			return "", fmt.Errorf("request.xoauth2.token_file %s is empty", o.TokenFile)
		}
		return token, nil
	}
	key := netTokenKey(collector, o)
	netTokens.mu.Lock()
	kept, ok := netTokens.tokens[key]
	netTokens.mu.Unlock()
	if ok && time.Until(kept.expires) > netTokenMargin {
		return kept.value, nil
	}
	form := url.Values{"grant_type": {netGrantRefreshToken}}
	if o.Grant == netGrantClientCredentials {
		form.Set("grant_type", netGrantClientCredentials)
	}
	for _, file := range []struct{ key, name, path string }{
		{"client_id", "client_id_file", o.ClientIDFile},
		{"client_secret", "client_secret_file", o.ClientSecretFile},
		{"refresh_token", "refresh_token_file", o.RefreshTokenFile},
	} {
		if file.path == "" {
			continue
		}
		value, err := ReadCredentialFile(file.path)
		if err != nil {
			return "", fmt.Errorf("reading request.xoauth2.%s: %w", file.name, err)
		}
		if value == "" {
			return "", fmt.Errorf("request.xoauth2.%s %s is empty", file.name, file.path)
		}
		form.Set(file.key, value)
	}
	if o.ClientID != "" {
		form.Set("client_id", o.ClientID)
	}
	if len(o.Scopes) > 0 {
		form.Set("scope", strings.Join(o.Scopes, " "))
	}
	shown := RedactText(o.TokenURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("request.xoauth2.token_url %s: %w", shown, RedactURLErrors(err))
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	traceRequest(ctx, http.MethodPost, shown, http.Header{"Content-Type": {"application/x-www-form-urlencoded"}}, "", false)
	resp, err := netOAuthClient.Do(req) // #nosec G704 -- the configuration's token endpoint
	if err != nil {
		err = fmt.Errorf("asking %s for an access token: %w", shown, netFailure(ctx, RedactURLErrors(err)))
		traceFailedOutcome(ctx, "error: "+err.Error())
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, netOAuthAnswerLimit))
	if err != nil {
		return "", fmt.Errorf("reading the answer of %s: %w", shown, netFailure(ctx, err))
	}
	traceOutcome(ctx, resp.Status)
	var answer struct {
		AccessToken string  `json:"access_token"`
		ExpiresIn   float64 `json:"expires_in"`
		Error       string  `json:"error"`
		Description string  `json:"error_description"`
	}
	_ = json.Unmarshal(raw, &answer)
	if resp.StatusCode != http.StatusOK || answer.AccessToken == "" {
		why := answer.Error
		if answer.Description != "" {
			why += ": " + answer.Description
		}
		if why == "" {
			why = "no access_token in its answer"
		}
		return "", model.Errorf("the token endpoint %s answered HTTP status %d, %s", shown, resp.StatusCode, model.Bare(netShown(why)))
	}
	lifetime := time.Duration(answer.ExpiresIn * float64(time.Second))
	if lifetime > 0 {
		netTokens.mu.Lock()
		netTokens.tokens[key] = netToken{value: answer.AccessToken, expires: time.Now().Add(lifetime)}
		netTokens.mu.Unlock()
	}
	return answer.AccessToken, nil
}

// xoauth2FileWarnings reads, for --dry-run, the files of request.xoauth2,
// which a scrape reads when it needs a token.
func xoauth2FileWarnings(c *model.Collector, t *model.StaticTarget) []string {
	o := c.Request.XOAuth2
	if o == nil || t != nil {
		return nil
	}
	var warnings []string
	for _, file := range []struct{ name, path string }{
		{"token_file", o.TokenFile}, {"client_id_file", o.ClientIDFile}, {"client_secret_file", o.ClientSecretFile}, {"refresh_token_file", o.RefreshTokenFile},
	} {
		if file.path == "" {
			continue
		}
		value, err := ReadCredentialFile(file.path)
		switch {
		case err != nil:
			warnings = append(warnings, fmt.Sprintf("collector %q: request.xoauth2.%s: %v; a scrape would fail on it now (the file is read when a token is needed, so one put in place later is used then)", c.Name, file.name, err))
		case value == "":
			warnings = append(warnings, fmt.Sprintf("collector %q: request.xoauth2.%s %s is empty; a scrape would fail on it now", c.Name, file.name, file.path))
		}
	}
	return warnings
}
