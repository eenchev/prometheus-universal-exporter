package fetch

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A gcp store reads a secret over Google Cloud Secret Manager's REST API:
// the version asked for, latest unless the reference names one, of the
// secret the reference names in the store's project, whose payload is the
// secret's text.
//
// The request carries an access token. Where the exporter runs on Google
// Cloud, with Workload Identity on GKE or a service account attached to the
// machine, the metadata server gives it one, and nothing is configured. A
// service account's key file, credentials_file or the one the
// GOOGLE_APPLICATION_CREDENTIALS environment variable names, is used in its
// place: a JWT signed with the key is exchanged for a token (RFC 7523). A
// token is kept in memory until a minute before it expires.

const (
	gcpSecretsEndpoint = "https://secretmanager.googleapis.com" //nolint:gosec // G101: the API's address, not a credential
	gcpMetadataHost    = "metadata.google.internal"
	gcpScope           = "https://www.googleapis.com/auth/cloud-platform"
	gcpJWTGrant        = "urn:ietf:params:oauth:grant-type:jwt-bearer"
)

type gcpBackend struct {
	endpoint, project string
	credentialsFile   string
	client            *http.Client

	mu      sync.Mutex
	token   string
	expires time.Time
}

func newGCPBackend(e model.SecretStore, timeout time.Duration) (secretBackend, error) {
	client, err := secretClient(model.TLSConfig{}, timeout)
	if err != nil {
		return nil, err
	}
	b := &gcpBackend{endpoint: strings.TrimRight(e.Endpoint, "/"), project: e.Project, credentialsFile: e.CredentialsFile, client: client}
	if b.endpoint == "" {
		b.endpoint = gcpSecretsEndpoint
	}
	return b, nil
}

func (b *gcpBackend) read(ctx context.Context, ref secretRef) (secretDocument, error) {
	if strings.Contains(ref.path, "/") {
		return secretDocument{}, fmt.Errorf("%q is not the name of a secret; a gcp reference names the secret alone, in the store's project", ref.path)
	}
	version := ref.version
	if version == "" {
		version = "latest"
	}
	address := b.endpoint + "/v1/projects/" + url.PathEscape(b.project) + "/secrets/" + url.PathEscape(ref.path) + "/versions/" + url.PathEscape(version) + ":access"
	for attempt := 0; ; attempt++ {
		token, kept, err := b.accessToken(ctx)
		if err != nil {
			return secretDocument{}, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, http.NoBody)
		if err != nil {
			return secretDocument{}, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		status, body, err := secretHTTP(b.client, req)
		if err != nil {
			return secretDocument{}, err
		}
		switch {
		case status == http.StatusOK:
			var answer struct {
				Payload struct {
					Data string `json:"data"`
				} `json:"payload"`
			}
			if err := json.Unmarshal(body, &answer); err != nil {
				return secretDocument{}, errors.New("secret manager's answer is not the JSON of a secret version")
			}
			data, err := base64.StdEncoding.DecodeString(answer.Payload.Data)
			if err != nil {
				return secretDocument{}, errors.New("secret manager's answer has a payload that is not base64")
			}
			return secretDocument{text: string(data)}, nil
		case status == http.StatusUnauthorized && kept && attempt == 0:
			b.mu.Lock()
			b.token = ""
			b.mu.Unlock()
			continue
		}
		return secretDocument{}, fmt.Errorf("secret manager answered %d: %s", status, gcpSaid(body))
	}
}

// gcpSaid is the message of a Google API error, or the start of the answer.
func gcpSaid(body []byte) string {
	var answer struct {
		Error struct {
			Message string `json:"message"`
			Status  string `json:"status"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &answer) == nil && answer.Error.Message != "" {
		return secretStoreSaid([]byte(strings.TrimSpace(answer.Error.Status + " " + answer.Error.Message)))
	}
	return secretStoreSaid(body)
}

// accessToken is the token requests carry, the one kept while it lasts,
// which kept says.
func (b *gcpBackend) accessToken(ctx context.Context) (token string, kept bool, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.token != "" && secretNow().Before(b.expires) {
		return b.token, true, nil
	}
	file := b.credentialsFile
	if file == "" {
		file = os.Getenv("GOOGLE_APPLICATION_CREDENTIALS")
	}
	var req *http.Request
	from := "the metadata server"
	if file != "" {
		from = "the token endpoint of " + file
		if req, err = gcpKeyRequest(ctx, file); err != nil {
			return "", false, err
		}
	} else {
		host := os.Getenv("GCE_METADATA_HOST")
		if host == "" {
			host = gcpMetadataHost
		}
		//nolint:gosec // G704: the metadata server, or the one GCE_METADATA_HOST names, as Google's libraries read it
		if req, err = http.NewRequestWithContext(ctx, http.MethodGet, "http://"+host+"/computeMetadata/v1/instance/service-accounts/default/token", http.NoBody); err != nil {
			return "", false, err
		}
		req.Header.Set("Metadata-Flavor", "Google")
	}
	status, body, err := secretHTTP(b.client, req)
	if err != nil {
		return "", false, fmt.Errorf("asking %s for an access token: %w; outside Google Cloud, name a service account's key with credentials_file", from, err)
	}
	var answer struct {
		AccessToken string  `json:"access_token"`
		ExpiresIn   float64 `json:"expires_in"`
	}
	if status != http.StatusOK || json.Unmarshal(body, &answer) != nil || answer.AccessToken == "" {
		return "", false, fmt.Errorf("%s gave no access token (%d): %s", from, status, secretStoreSaid(body))
	}
	b.token = answer.AccessToken
	b.expires = secretNow().Add(time.Duration(answer.ExpiresIn)*time.Second - time.Minute)
	return b.token, false, nil
}

// gcpKeyRequest is the request that exchanges a JWT signed with a service
// account's key for an access token.
func gcpKeyRequest(ctx context.Context, file string) (*http.Request, error) {
	raw, err := os.ReadFile(file) //nolint:gosec // G703: the store's credentials_file, which the configuration names
	if err != nil {
		return nil, fmt.Errorf("reading credentials_file: %w", err)
	}
	var key struct {
		Type         string `json:"type"`
		ClientEmail  string `json:"client_email"`
		PrivateKey   string `json:"private_key"`
		PrivateKeyID string `json:"private_key_id"`
		TokenURI     string `json:"token_uri"`
	}
	if err := json.Unmarshal(raw, &key); err != nil || key.Type != "service_account" || key.ClientEmail == "" || key.PrivateKey == "" {
		return nil, fmt.Errorf("credentials_file %s is not a service account's key, the JSON file with type service_account that Google Cloud gives for one", file)
	}
	if key.TokenURI == "" {
		key.TokenURI = "https://oauth2.googleapis.com/token"
	}
	block, _ := pem.Decode([]byte(key.PrivateKey))
	if block == nil {
		return nil, fmt.Errorf("credentials_file %s has a private_key that is not PEM", file)
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		if parsed, err = x509.ParsePKCS1PrivateKey(block.Bytes); err != nil {
			return nil, fmt.Errorf("credentials_file %s has a private_key that is not an RSA key", file)
		}
	}
	signer, isRSA := parsed.(*rsa.PrivateKey)
	if !isRSA {
		return nil, fmt.Errorf("credentials_file %s has a private_key that is not an RSA key", file)
	}
	now := secretNow()
	encode := func(v any) string {
		b, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	unsigned := encode(map[string]string{"alg": "RS256", "typ": "JWT", "kid": key.PrivateKeyID}) + "." +
		encode(map[string]any{"iss": key.ClientEmail, "scope": gcpScope, "aud": key.TokenURI, "iat": now.Unix(), "exp": now.Add(time.Hour).Unix()})
	digest := sha256.Sum256([]byte(unsigned))
	signature, err := rsa.SignPKCS1v15(rand.Reader, signer, crypto.SHA256, digest[:])
	if err != nil {
		return nil, fmt.Errorf("signing with the key of credentials_file: %w", err)
	}
	form := url.Values{"grant_type": {gcpJWTGrant}, "assertion": {unsigned + "." + base64.RawURLEncoding.EncodeToString(signature)}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, key.TokenURI, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("credentials_file %s has the token_uri %q, which is no URL", file, key.TokenURI)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return req, nil
}
