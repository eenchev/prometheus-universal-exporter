//go:build !select_request_types || request_type_http || request_type_graphite

package fetch

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/expr"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A jwt login is the login of a service account: an identity with no
// password and no client secret, only a private key. The exporter signs a
// JSON Web Token with the key, stating who it is (iss), whom the token is
// for (aud) and for how long (exp). It sends the token to login.url, which
// answers with a token the requests are sent with.
//
//	login:
//	  type: jwt
//	  url: https://oauth2.googleapis.com/token
//	  signing_key_file: /etc/exporter/service-account.json
//	  scopes: [https://www.googleapis.com/auth/spreadsheets.readonly]
//
// exchange says how the JWT is sent:
//   - form (the default) is the JWT bearer grant of RFC 7523, used by
//     Google's service accounts: a form of grant_type and assertion. The
//     answer is an OAuth 2.0 token, with access_token and expires_in.
//   - bearer sends the JWT as the request's own credential, Authorization:
//     Bearer, with no body. This is how a GitHub App asks for an
//     installation's token, which the answer holds in token.
//
// The key is either PEM or the JSON key file Google Cloud gives for a
// service account. A PEM key is an RSA key (RS256), an elliptic curve key
// of P-256 or P-384 (ES256, ES384) or an Ed25519 key (EdDSA). In the JSON
// key file, client_email and private_key_id are the issuer and the key's
// id unless the login sets its own. The key is read at each login through
// ReadCredentialFile, so it may be a reference to a secret store.

// jwtLoginKeys are the keys a jwt login takes that no other kind does.
var jwtLoginKeys = []string{"signing_key_file", "issuer", "subject", "audience", "key_id", "claims", "lifetime", "exchange"}

const (
	jwtDefaultLifetime = 5 * time.Minute
	jwtMaxLifetime     = time.Hour
	// jwtBackdate is how long before now a JWT says it was issued, for a
	// server whose clock is a little behind.
	jwtBackdate = 30 * time.Second
	jwtGrant    = "urn:ietf:params:oauth:grant-type:jwt-bearer"

	jwtExchangeForm   = "form"
	jwtExchangeBearer = "bearer"
)

// jwtOwnClaims are the claims a jwt login sets itself, which claims may
// not.
var jwtOwnClaims = []string{"iss", "sub", "aud", "iat", "exp", "scope"}

// checkJWTLogin checks a jwt login's keys when the configuration loads.
func checkJWTLogin(l *model.LoginConfig, where string) error {
	switch {
	case strings.TrimSpace(l.SigningKeyFile) == "":
		return fmt.Errorf("%s needs signing_key_file, the file of the private key the JWT is signed with: a PEM key, or the JSON key file of a Google service account", where)
	case strings.Contains(l.SigningKeyFile, "{{"):
		return fmt.Errorf("%s.signing_key_file has a placeholder; the key is the configuration's", where)
	case l.Exchange != "" && !slices.Contains(model.LoginExchanges, l.Exchange):
		return fmt.Errorf("%s.exchange is %q; it is form, the JWT sent as the assertion of a token request (RFC 7523), the default, or bearer, the JWT sent as the request's own credential", where, l.Exchange)
	case l.Lifetime < 0 || time.Duration(l.Lifetime) > jwtMaxLifetime:
		return fmt.Errorf("%s.lifetime is %s; it is how long the JWT is valid, at most %s, %s when left out", where, time.Duration(l.Lifetime), jwtMaxLifetime, jwtDefaultLifetime)
	}
	for _, claim := range []struct{ key, value string }{{"issuer", l.Issuer}, {"subject", l.Subject}, {"audience", l.Audience}, {"key_id", l.KeyID}} {
		if strings.ContainsFunc(claim.value, func(r rune) bool { return r < 0x20 || r == 0x7f }) || strings.Contains(claim.value, "{{") {
			return fmt.Errorf("%s.%s has a control character or a placeholder; it is used as it is written", where, claim.key)
		}
	}
	for _, name := range model.SortedKeys(l.Claims) {
		if name == "" || slices.Contains(jwtOwnClaims, name) {
			return fmt.Errorf("%s.claims sets %q, which the login sets itself (iss from issuer, sub from subject, aud from audience, scope from scopes, iat and exp from lifetime)", where, name)
		}
	}
	for i, scope := range l.Scopes {
		if scope == "" || strings.ContainsAny(scope, " \t\r\n\"\\") {
			return fmt.Errorf("%s.scopes[%d] %q is no scope; a scope is not empty and holds no blank, quote or backslash (RFC 6749 § 3.3)", where, i, scope)
		}
	}
	for _, expression := range []struct{ key, value string }{{"token", l.Token}, {"expires_in", l.ExpiresIn}} {
		if expression.value == "" {
			continue
		}
		if strings.TrimSpace(expression.value) == "" {
			return fmt.Errorf("%s.%s is blank; write a jq expression such as .access_token, or leave the key out", where, expression.key)
		}
		if _, err := expr.CompileJQ(expression.value); err != nil {
			return fmt.Errorf("%s.%s %q does not compile: %w", where, expression.key, expression.value, err)
		}
	}
	return nil
}

// jwtSigner is a private key as a JWT is signed with it: the algorithm its
// header names and the signature of what is signed.
type jwtSigner struct {
	algorithm string
	sign      func(signed []byte) ([]byte, error)
}

// jwtKey reads the key of a jwt login: PEM, or a Google service account's
// JSON key file, whose client_email and private_key_id it returns too.
// What is wrong with the file is said without what it holds.
func jwtKey(file string) (signer jwtSigner, issuer, keyID string, err error) {
	raw, err := ReadCredentialFile(file)
	if err != nil {
		return jwtSigner{}, "", "", fmt.Errorf("reading login.signing_key_file: %w", err)
	}
	if raw == "" {
		return jwtSigner{}, "", "", fmt.Errorf("login.signing_key_file %s is empty", file)
	}
	text := raw
	if strings.HasPrefix(raw, "{") {
		var account struct {
			ClientEmail  string `json:"client_email"`
			PrivateKey   string `json:"private_key"`
			PrivateKeyID string `json:"private_key_id"`
		}
		if json.Unmarshal([]byte(raw), &account) != nil || account.PrivateKey == "" {
			return jwtSigner{}, "", "", fmt.Errorf("login.signing_key_file %s is JSON without a private_key; a JSON key file is the one Google Cloud gives for a service account", file)
		}
		text, issuer, keyID = account.PrivateKey, account.ClientEmail, account.PrivateKeyID
	}
	block, _ := pem.Decode([]byte(text))
	if block == nil {
		return jwtSigner{}, "", "", fmt.Errorf("login.signing_key_file %s holds no PEM private key (-----BEGIN PRIVATE KEY-----)", file)
	}
	var key any
	if key, err = x509.ParsePKCS8PrivateKey(block.Bytes); err != nil {
		if key, err = x509.ParsePKCS1PrivateKey(block.Bytes); err != nil {
			if key, err = x509.ParseECPrivateKey(block.Bytes); err != nil {
				return jwtSigner{}, "", "", fmt.Errorf("login.signing_key_file %s holds a PEM block that is no private key this login signs with: an RSA, an elliptic curve or an Ed25519 key, not encrypted", file)
			}
		}
	}
	switch key := key.(type) {
	case *rsa.PrivateKey:
		signer = jwtSigner{"RS256", func(signed []byte) ([]byte, error) {
			digest := sha256.Sum256(signed)
			return rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
		}}
	case *ecdsa.PrivateKey:
		size := (key.Curve.Params().BitSize + 7) / 8
		var digest func([]byte) []byte
		switch size {
		case 32:
			signer.algorithm, digest = "ES256", func(b []byte) []byte { d := sha256.Sum256(b); return d[:] }
		case 48:
			signer.algorithm, digest = "ES384", func(b []byte) []byte { d := sha512.Sum384(b); return d[:] }
		default:
			return jwtSigner{}, "", "", fmt.Errorf("login.signing_key_file %s holds an elliptic curve key of a curve other than P-256 and P-384", file)
		}
		signer.sign = func(signed []byte) ([]byte, error) {
			r, s, err := ecdsa.Sign(rand.Reader, key, digest(signed))
			if err != nil {
				return nil, err
			}
			// The two numbers, each of the curve's size (RFC 7518 § 3.4).
			signature := make([]byte, 2*size)
			r.FillBytes(signature[:size])
			s.FillBytes(signature[size:])
			return signature, nil
		}
	case ed25519.PrivateKey:
		signer = jwtSigner{"EdDSA", func(signed []byte) ([]byte, error) { return ed25519.Sign(key, signed), nil }}
	default:
		return jwtSigner{}, "", "", fmt.Errorf("login.signing_key_file %s holds a key this login does not sign with; it signs with an RSA, an elliptic curve or an Ed25519 key", file)
	}
	return signer, issuer, keyID, nil
}

// signJWT is the JWT a jwt login hands in at u, made at now, and when it
// expires.
func signJWT(l *model.LoginConfig, u *url.URL, now time.Time) (string, time.Time, error) {
	signer, issuer, keyID, err := jwtKey(l.SigningKeyFile)
	if err != nil {
		return "", time.Time{}, err
	}
	if l.Issuer != "" {
		issuer = l.Issuer
	}
	if l.KeyID != "" {
		keyID = l.KeyID
	}
	if issuer == "" {
		return "", time.Time{}, errors.New("login.issuer is not set, and the key file names none; say who the JWT is from, such as a service account's address or an app's id")
	}
	lifetime := jwtDefaultLifetime
	if l.Lifetime > 0 {
		lifetime = time.Duration(l.Lifetime)
	}
	expires := now.Add(lifetime)
	header := map[string]string{"alg": signer.algorithm, "typ": "JWT"}
	if keyID != "" {
		header["kid"] = keyID
	}
	claims := map[string]any{"iss": issuer, "iat": now.Add(-jwtBackdate).Unix(), "exp": expires.Unix()}
	// The audience is the endpoint the JWT is handed to, as RFC 7523 has
	// it, unless the login names another.
	claims["aud"] = u.String()
	if l.Audience != "" {
		claims["aud"] = l.Audience
	}
	if l.Subject != "" {
		claims["sub"] = l.Subject
	}
	if len(l.Scopes) > 0 {
		claims["scope"] = strings.Join(l.Scopes, " ")
	}
	for name, value := range l.Claims {
		claims[name] = value
	}
	encode := func(v any) string {
		b, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	unsigned := encode(header) + "." + encode(claims)
	signature, err := signer.sign([]byte(unsigned))
	if err != nil {
		return "", time.Time{}, errors.New("signing the JWT with the key of login.signing_key_file failed")
	}
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(signature), expires, nil
}

// jwtLogin signs a JWT, sends it to u for a token as login.exchange says,
// and reads the token from the answer with the jq expressions token and
// expires_in. Unless the login sets its own, these are .access_token and
// .expires_in for a form exchange and .token for a bearer one.
func jwtLogin(ctx context.Context, client *http.Client, l *model.LoginConfig, u *url.URL, start time.Time) (loginToken, error) {
	assertion, _, err := signJWT(l, u, start)
	if err != nil {
		return loginToken{}, err
	}
	answers := *l
	var req *http.Request
	// u is login.url from the configuration, held to the collector's
	// target policy before the request is sent (logIn).
	if l.Exchange == jwtExchangeBearer {
		if req, err = http.NewRequestWithContext(ctx, http.MethodPost, u.String(), http.NoBody); err != nil { // #nosec G704 -- the configured login URL, checked against the target policy
			return loginToken{}, err
		}
		req.Header.Set("Authorization", "Bearer "+assertion)
		if answers.Token == "" {
			answers.Token = ".token"
		}
	} else {
		form := url.Values{"grant_type": {jwtGrant}, "assertion": {assertion}}
		if req, err = http.NewRequestWithContext(ctx, http.MethodPost, u.String(), strings.NewReader(form.Encode())); err != nil { // #nosec G704 -- the configured login URL, checked against the target policy
			return loginToken{}, err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if answers.Token == "" {
			answers.Token = ".access_token"
		}
		if answers.ExpiresIn == "" {
			answers.ExpiresIn = ".expires_in"
		}
	}
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
		return loginToken{}, answeredError(resp.StatusCode, resp.Header.Get("Location") != "", answer, assertion)
	}
	if len(answer) > loginAnswerLimit {
		return loginToken{}, fmt.Errorf("the login's answer is larger than %d bytes", loginAnswerLimit)
	}
	return loginAnswerToken(ctx, &answers, resp, answer, start)
}
