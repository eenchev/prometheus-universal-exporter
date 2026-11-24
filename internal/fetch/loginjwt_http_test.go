//go:build !select_request_types || request_type_http

package fetch

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// pemKey is key as a PEM PKCS#8 private key.
func pemKey(t *testing.T, key any) string {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

// jwtParts are a JWT's header and claims, and what was signed with its
// signature.
func jwtParts(t *testing.T, token string) (header, claims map[string]any, signed string, signature []byte) {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("no JWT: %d parts", len(parts))
	}
	for i, into := range []*map[string]any{&header, &claims} {
		raw, err := base64.RawURLEncoding.DecodeString(parts[i])
		if err != nil || json.Unmarshal(raw, into) != nil {
			t.Fatalf("part %d of the JWT is not base64url JSON", i)
		}
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatal(err)
	}
	return header, claims, parts[0] + "." + parts[1], signature
}

// jwtTarget is a target that takes the token "exchanged" alone.
func jwtTarget(t *testing.T) *httptest.Server {
	t.Helper()
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer exchanged" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte("value=1\n"))
	}))
	t.Cleanup(target.Close)
	return target
}

// A jwt login signs a JWT with the key of signing_key_file and hands it to
// login.url as the assertion of a JWT bearer grant: its claims say the
// issuer, the audience, which is the endpoint unless the login names
// another, the scopes and when it expires, and its signature is the key's,
// of an RSA, an elliptic curve and an Ed25519 key alike. The token the
// endpoint answers is what the target is sent, kept for its lifetime.
func TestAJWTLoginExchangesASignedJWTForAToken(t *testing.T) {
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	p256, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	p384, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	edPublic, edKey, _ := ed25519.GenerateKey(rand.Reader)
	for name, key := range map[string]struct {
		private   any
		algorithm string
		verify    func(signed string, signature []byte) bool
	}{
		"rsa": {rsaKey, "RS256", func(signed string, signature []byte) bool {
			digest := sha256.Sum256([]byte(signed))
			return rsa.VerifyPKCS1v15(&rsaKey.PublicKey, crypto.SHA256, digest[:], signature) == nil
		}},
		"p-256": {p256, "ES256", func(signed string, signature []byte) bool {
			digest := sha256.Sum256([]byte(signed))
			return len(signature) == 64 && ecdsa.Verify(&p256.PublicKey, digest[:], new(big.Int).SetBytes(signature[:32]), new(big.Int).SetBytes(signature[32:]))
		}},
		"p-384":   {p384, "ES384", func(_ string, signature []byte) bool { return len(signature) == 96 }},
		"ed25519": {edKey, "EdDSA", func(signed string, signature []byte) bool { return ed25519.Verify(edPublic, []byte(signed), signature) }},
	} {
		t.Run(name, func(t *testing.T) {
			freshLogins(t)
			var mu sync.Mutex
			var problems []string
			exchanges := 0
			var idp *httptest.Server
			idp = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				exchanges++
				_ = r.ParseForm()
				if r.Method != http.MethodPost || r.PostForm.Get("grant_type") != "urn:ietf:params:oauth:grant-type:jwt-bearer" || r.Header.Get("Authorization") != "" {
					problems = append(problems, fmt.Sprintf("the request: %s %v", r.Method, r.PostForm))
				}
				header, claims, signed, signature := jwtParts(t, r.PostForm.Get("assertion"))
				now := float64(time.Now().Unix())
				iat, _ := claims["iat"].(float64)
				exp, _ := claims["exp"].(float64)
				if header["alg"] != key.algorithm || header["typ"] != "JWT" || header["kid"] != "key-7" || claims["iss"] != "monitor@project.example" || claims["sub"] != "user@example.com" ||
					claims["aud"] != idp.URL+"/token" || claims["scope"] != "read:a read:b" || claims["tenant"] != "acme" || iat > now || iat < now-60 || exp-iat != 630 {
					problems = append(problems, fmt.Sprintf("the JWT: %v %v", header, claims))
				}
				if !key.verify(signed, signature) {
					problems = append(problems, "the signature is not the key's")
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"access_token": "exchanged", "expires_in": 3600}`))
			}))
			defer idp.Close()
			target := jwtTarget(t)
			c := loginCollector(t, model.LoginConfig{Type: "jwt", URL: idp.URL + "/token", SigningKeyFile: testutil.WriteFile(t, "key.pem", pemKey(t, key.private)),
				Issuer: "monitor@project.example", Subject: "user@example.com", KeyID: "key-7", Scopes: []string{"read:a", "read:b"}, Claims: map[string]string{"tenant": "acme"}, Lifetime: model.Duration(10 * time.Minute)})
			for range 2 {
				resp, err := FetchCollector(context.Background(), target.URL, c, RequestOverrides{}, nil)
				if err != nil || string(resp.Body) != "value=1\n" {
					t.Fatalf("%v %v", resp, err)
				}
			}
			mu.Lock()
			defer mu.Unlock()
			if exchanges != 1 || len(problems) > 0 {
				t.Errorf("%d exchanges, want 1 for two scrapes; %v", exchanges, problems)
			}
		})
	}
}

// A Google service account's JSON key file is a key too: its private_key
// signs, and its client_email and private_key_id are the issuer and the
// key's id unless the login says its own. With exchange: bearer the JWT is
// the request's own credential, as a GitHub App asks for an installation's
// token, read from .token unless the login says where, and kept for ttl.
func TestAJWTLoginReadsAServiceAccountsKeyAndExchangesAsBearer(t *testing.T) {
	freshLogins(t)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	account, _ := json.Marshal(map[string]string{"type": "service_account", "client_email": "sa@project.iam.example", "private_key_id": "kid-1", "private_key": pemKey(t, key)})
	var mu sync.Mutex
	var problems []string
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		assertion, bearer := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !bearer || r.ContentLength > 0 {
			problems = append(problems, "the JWT is not the request's credential, or the request has a body")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		header, claims, _, _ := jwtParts(t, assertion)
		if header["kid"] != "kid-1" || claims["iss"] != "sa@project.iam.example" || claims["aud"] != "https://api.example/" {
			problems = append(problems, fmt.Sprintf("the JWT: %v %v", header, claims))
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"token": "exchanged", "expires_at": "2030-01-01T00:00:00Z"}`))
	}))
	defer idp.Close()
	target := jwtTarget(t)
	c := loginCollector(t, model.LoginConfig{Type: "jwt", URL: idp.URL + "/app/installations/1/access_tokens", Exchange: "bearer", Audience: "https://api.example/",
		SigningKeyFile: testutil.WriteFile(t, "sa.json", string(account)), TTL: model.Duration(50 * time.Minute)})
	if resp, err := FetchCollector(context.Background(), target.URL, c, RequestOverrides{}, nil); err != nil || string(resp.Body) != "value=1\n" {
		t.Fatalf("%v %v", resp, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(problems) > 0 {
		t.Error(problems)
	}
}

// What is wrong with a jwt login is said when the configuration loads, and
// what is wrong with its key, or what the endpoint refused, at the login,
// without the key or the JWT.
func TestAJWTLoginIsChecked(t *testing.T) {
	for want, l := range map[string]model.LoginConfig{
		"needs signing_key_file":                          {Type: "jwt", URL: "https://idp.example/token"},
		`request.login.exchange is "header"`:              {Type: "jwt", URL: "https://idp.example/token", SigningKeyFile: "/k", Exchange: "header"},
		"request.login.lifetime is 2h0m0s":                {Type: "jwt", URL: "https://idp.example/token", SigningKeyFile: "/k", Lifetime: model.Duration(2 * time.Hour)},
		`request.login.claims sets "exp"`:                 {Type: "jwt", URL: "https://idp.example/token", SigningKeyFile: "/k", Claims: map[string]string{"exp": "1"}},
		"sets client_id, which a login of type jwt does":  {Type: "jwt", URL: "https://idp.example/token", SigningKeyFile: "/k", ClientID: "x"},
		"sets issuer, which a login of type oauth2 does":  {Type: "oauth2", URL: "https://idp.example/token", ClientID: "x", ClientSecretFile: "/s", Issuer: "me"},
		`request.login.token ".[" does not compile`:       {Type: "jwt", URL: "https://idp.example/token", SigningKeyFile: "/k", Token: ".["},
		"sets token_header, which a login of type jwt do": {Type: "jwt", URL: "https://idp.example/token", SigningKeyFile: "/k", TokenHeader: "X-Token"},
		"request.login.url":                               {Type: "jwt", SigningKeyFile: "/k"},
		"request.login.issuer has a control character or": {Type: "jwt", URL: "https://idp.example/token", SigningKeyFile: "/k", Issuer: "{{param_who}}"},
	} {
		c := testutil.Collector("api", "text")
		c.Request.Login = &l
		if err := ValidateRequest(&c); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("err=%v, want %q", err, want)
		}
	}

	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	good := pemKey(t, key)
	refuses := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprintf(w, `{"error": "invalid_grant", "error_description": "bad assertion %s"}`, r.PostForm.Get("assertion"))
	}))
	defer refuses.Close()
	target := jwtTarget(t)
	for want, l := range map[string]model.LoginConfig{
		"holds no PEM private key":                            {SigningKeyFile: testutil.WriteFile(t, "text", "s3cret-not-a-key"), Issuer: "me"},
		"is JSON without a private_key":                       {SigningKeyFile: testutil.WriteFile(t, "json", `{"client_email": "s3cret@example"}`), Issuer: "me"},
		"holds a PEM block that is no private key this login": {SigningKeyFile: testutil.WriteFile(t, "cert", "-----BEGIN PRIVATE KEY-----\nczNjcmV0\n-----END PRIVATE KEY-----\n"), Issuer: "me"},
		"login.issuer is not set, and the key file names non": {SigningKeyFile: testutil.WriteFile(t, "pem", good)},
		"reading login.signing_key_file":                      {SigningKeyFile: "/nonexistent/key.pem", Issuer: "me"},
		"400":                                                 {SigningKeyFile: testutil.WriteFile(t, "pem", good), Issuer: "me"},
	} {
		freshLogins(t)
		l.Type, l.URL = "jwt", refuses.URL
		_, err := FetchCollector(context.Background(), target.URL, loginCollector(t, l), RequestOverrides{}, nil)
		var stage *StageError
		if err == nil || !strings.Contains(err.Error(), want) || strings.Contains(err.Error(), "s3cret") || strings.Contains(err.Error(), "eyJ") || !errors.As(err, &stage) || stage.Stage != "login" {
			t.Errorf("err=%v, want %q in the login stage", err, want)
		}
	}
}
