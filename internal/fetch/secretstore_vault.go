package fetch

import (
	"bytes"
	"context"
	"encoding/json"
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

// A vault store reads a secret over HashiCorp Vault's HTTP API: GET
// /v1/<path>, the path as the API has it, which for a KV version 2 mount
// has data/ after the mount (secret/data/db) and for version 1 has not
// (secret/db). Either answer is read: the fields of a version 2 secret are
// those under data.data, told by the metadata beside them.
//
// The exporter logs in as auth says. token sends the token of auth.token_file,
// read at each request so that one a Vault Agent renews is used as it is
// renewed, or of the VAULT_TOKEN environment variable. kubernetes exchanges
// the pod's service account token for a Vault token, and approle a role id
// and a secret id; the token either gives is kept in memory until nine
// tenths of its lease have passed, and asked for again at once when Vault
// refuses it.

const (
	vaultAuthToken      = "token"
	vaultAuthKubernetes = "kubernetes"
	vaultAuthAppRole    = "approle"
	// vaultServiceAccountToken is where a pod finds its service account's
	// token.
	vaultServiceAccountToken = "/var/run/secrets/kubernetes.io/serviceaccount/token" //nolint:gosec // G101: a path, not a credential
)

func init() {
	registerSecretStoreType(&SecretStoreType{
		Name:  "vault",
		Keys:  []string{"address", "namespace", "auth", "tls"},
		Check: checkVaultStore,
		New:   newVaultBackend,
		// A Vault secret is a mapping of fields.
		FieldRequired: true,
		KeyRules: func() map[string]map[string]any {
			return map[string]map[string]any{
				"address":             {"description": "vault, required: the server's URL, such as https://vault.example.com:8200."},
				"namespace":           {"description": "vault: the Vault Enterprise namespace the requests are made in."},
				"auth":                {"required": []string{"method"}, "description": "vault, required: how the exporter logs in."},
				"auth.method":         {"enum": model.SecretStoreAuthMethods, "description": "Required. token: the token of token_file, or of the VAULT_TOKEN environment variable. kubernetes: the pod's service account token is exchanged for a Vault token, as role. approle: role_id and the secret id of secret_id_file, or of VAULT_SECRET_ID."},
				"auth.token_file":     {"description": "token: the file that holds the Vault token, read at each request, so one a Vault Agent renews is used as renewed; VAULT_TOKEN when left out."},
				"auth.role":           {"description": "kubernetes, required: the Vault role the service account logs in as."},
				"auth.mount":          {"description": "kubernetes and approle: where the auth method is mounted; kubernetes or approle when left out."},
				"auth.jwt_file":       {"description": "kubernetes: the file of the service account's token; /var/run/secrets/kubernetes.io/serviceaccount/token when left out."},
				"auth.role_id":        {"description": "approle, required: the AppRole's role id."},
				"auth.secret_id_file": {"description": "approle: the file that holds the AppRole's secret id; VAULT_SECRET_ID when left out."},
				"tls":                 {"description": "vault: how the server's certificate is verified, and a client certificate, as request.tls is written."},
			}
		},
	})
}

// checkVaultStore holds a vault entry to its keys.
func checkVaultStore(e *model.SecretStore) error {
	if e.Address == "" {
		return errors.New("has no address; name the Vault server's URL, such as https://vault.example.com:8200")
	}
	if err := checkSecretStoreURL("address", e.Address); err != nil {
		return err
	}
	// The files of tls are not opened here: they are mounted where the
	// exporter scrapes, and not on a target allocator that checks the
	// configuration for it, and are read when the store is first asked.
	if err := CheckClientTLSKeys(e.TLS); err != nil {
		return fmt.Errorf("tls %w", err)
	}
	a := e.Auth
	if a == nil || a.Method == "" {
		return errors.New("has no auth.method; say how the exporter logs in to Vault: token, kubernetes or approle")
	}
	for _, file := range []struct{ key, value string }{{"token_file", a.TokenFile}, {"jwt_file", a.JWTFile}, {"secret_id_file", a.SecretIDFile}} {
		if IsSecretReference(file.value) {
			return fmt.Errorf("has an auth.%s that is a reference to a secret store; what a store is logged in to with is a file or an environment variable", file.key)
		}
	}
	unused := func(keys ...string) error {
		set := map[string]bool{"token_file": a.TokenFile != "", "role": a.Role != "", "mount": a.Mount != "", "jwt_file": a.JWTFile != "", "role_id": a.RoleID != "", "secret_id_file": a.SecretIDFile != ""}
		for _, key := range keys {
			if set[key] {
				return fmt.Errorf("sets auth.%s, which auth.method %s does not take", key, a.Method)
			}
		}
		return nil
	}
	switch a.Method {
	case vaultAuthToken:
		return unused("role", "mount", "jwt_file", "role_id", "secret_id_file")
	case vaultAuthKubernetes:
		if a.Role == "" {
			return errors.New("has auth.method kubernetes and no auth.role; name the Vault role the service account logs in as")
		}
		return unused("token_file", "role_id", "secret_id_file")
	case vaultAuthAppRole:
		if a.RoleID == "" {
			return errors.New("has auth.method approle and no auth.role_id; name the AppRole's role id")
		}
		return unused("token_file", "role", "jwt_file")
	}
	return fmt.Errorf("has the auth.method %q; want one of %s, in lower case", a.Method, strings.Join(model.SecretStoreAuthMethods, ", "))
}

// vaultBackend is a vault store: its client and the token it logged in for.
type vaultBackend struct {
	base      string
	namespace string
	auth      model.SecretStoreAuth
	tls       model.TLSConfig
	timeout   time.Duration

	mu      sync.Mutex
	token   string
	expires time.Time

	clientMu sync.Mutex
	client   *http.Client
}

// httpClient is the client Vault is asked with, made at the first request:
// the files of the store's tls block are read then, so a store whose CA is
// mounted after the exporter starts is reached once it is, and a file that
// cannot be read fails the read that needed it, saying so, and no other.
func (b *vaultBackend) httpClient() (*http.Client, error) {
	b.clientMu.Lock()
	defer b.clientMu.Unlock()
	if b.client != nil {
		return b.client, nil
	}
	client, err := secretClient(b.tls, b.timeout)
	if err != nil {
		return nil, err
	}
	b.client = client
	return client, nil
}

func newVaultBackend(e model.SecretStore, timeout time.Duration) (SecretBackend, error) {
	b := &vaultBackend{base: strings.TrimRight(e.Address, "/"), namespace: e.Namespace, tls: e.TLS, timeout: timeout}
	if e.Auth != nil {
		b.auth = *e.Auth
	}
	if b.auth.Mount == "" {
		b.auth.Mount = b.auth.Method
	}
	b.auth.Mount = strings.Trim(b.auth.Mount, "/")
	if b.auth.Method == vaultAuthKubernetes && b.auth.JWTFile == "" {
		b.auth.JWTFile = vaultServiceAccountToken
	}
	return b, nil
}

// read asks Vault for the secret, logging in first when there is no token,
// and once more when Vault refuses the one kept.
func (b *vaultBackend) Read(ctx context.Context, ref SecretReference) (SecretDocument, error) {
	for attempt := 0; ; attempt++ {
		token, kept, err := b.login(ctx)
		if err != nil {
			return SecretDocument{}, err
		}
		status, body, err := b.get(ctx, token, ref)
		if err != nil {
			return SecretDocument{}, err
		}
		switch {
		case status == http.StatusOK:
			return vaultDocument(body)
		case status == http.StatusForbidden && kept && attempt == 0:
			// The token kept may have been revoked: log in again, once.
			b.forget(token)
			continue
		case status == http.StatusForbidden:
			return SecretDocument{}, fmt.Errorf("vault refused the read (403): %s; the token's policy must allow read on the path", vaultSaid(body))
		case status == http.StatusNotFound:
			return SecretDocument{}, fmt.Errorf("vault has no secret at %s (404); for a KV version 2 mount the path has data/ after the mount, as secret/data/name", ref.Path)
		}
		return SecretDocument{}, fmt.Errorf("vault answered %d: %s", status, vaultSaid(body))
	}
}

func (b *vaultBackend) get(ctx context.Context, token string, ref SecretReference) (int, []byte, error) {
	parts := strings.Split(ref.Path, "/")
	for i, part := range parts {
		parts[i] = url.PathEscape(part)
	}
	address := b.base + "/v1/" + strings.Join(parts, "/")
	if ref.Version != "" {
		address += "?version=" + url.QueryEscape(ref.Version)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, http.NoBody)
	if err != nil {
		return 0, nil, err
	}
	b.headers(req)
	req.Header.Set("X-Vault-Token", token)
	client, err := b.httpClient()
	if err != nil {
		return 0, nil, err
	}
	return secretHTTP(client, req)
}

func (b *vaultBackend) headers(req *http.Request) {
	req.Header.Set("X-Vault-Request", "true")
	if b.namespace != "" {
		req.Header.Set("X-Vault-Namespace", b.namespace)
	}
}

// vaultSaid is the errors of a Vault answer, or the start of it.
func vaultSaid(body []byte) string {
	var answer struct {
		Errors []string `json:"errors"`
	}
	if json.Unmarshal(body, &answer) == nil && len(answer.Errors) > 0 {
		return secretStoreSaid([]byte(strings.Join(answer.Errors, "; ")))
	}
	return secretStoreSaid(body)
}

// vaultDocument reads the fields of a secret from Vault's answer.
func vaultDocument(body []byte) (SecretDocument, error) {
	var answer struct {
		Data map[string]any `json:"data"`
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&answer); err != nil {
		return SecretDocument{}, errors.New("vault's answer is not the JSON of a secret")
	}
	fields := answer.Data
	// KV version 2 wraps the secret's fields with its metadata.
	if _, versioned := fields["metadata"].(map[string]any); versioned {
		inner, present := fields["data"]
		switch inner := inner.(type) {
		case map[string]any:
			fields = inner
		case nil:
			if present {
				return SecretDocument{}, errors.New("the version of the secret asked for is deleted")
			}
		}
	}
	if fields == nil {
		return SecretDocument{}, errors.New("vault's answer has no data: the path is not a secret's")
	}
	return SecretDocument{Fields: fields}, nil
}

// forget drops the token kept, when it is still the one refused.
func (b *vaultBackend) forget(token string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.token == token {
		b.token = ""
	}
}

// login is the token requests are sent with: the configured one, or the one
// a login gave, which kept says, while its lease lasts.
func (b *vaultBackend) login(ctx context.Context) (token string, kept bool, err error) {
	if b.auth.Method == vaultAuthToken {
		token, err = vaultCredential("auth.token_file", b.auth.TokenFile, "VAULT_TOKEN")
		return token, false, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.token != "" && (b.expires.IsZero() || secretNow().Before(b.expires)) {
		return b.token, true, nil
	}
	var login map[string]string
	switch b.auth.Method {
	case vaultAuthKubernetes:
		jwt, err := vaultCredential("auth.jwt_file", b.auth.JWTFile, "")
		if err != nil {
			return "", false, err
		}
		login = map[string]string{"role": b.auth.Role, "jwt": jwt}
	case vaultAuthAppRole:
		secretID, err := vaultCredential("auth.secret_id_file", b.auth.SecretIDFile, "VAULT_SECRET_ID")
		if err != nil {
			return "", false, err
		}
		login = map[string]string{"role_id": b.auth.RoleID, "secret_id": secretID}
	default:
		return "", false, fmt.Errorf("auth.method %q", b.auth.Method)
	}
	payload, err := json.Marshal(login)
	if err != nil {
		return "", false, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.base+"/v1/auth/"+b.auth.Mount+"/login", bytes.NewReader(payload))
	if err != nil {
		return "", false, err
	}
	b.headers(req)
	req.Header.Set("Content-Type", "application/json")
	client, err := b.httpClient()
	if err != nil {
		return "", false, err
	}
	status, body, err := secretHTTP(client, req)
	if err != nil {
		return "", false, fmt.Errorf("logging in to vault: %w", err)
	}
	if status != http.StatusOK {
		return "", false, fmt.Errorf("vault refused the %s login at auth/%s (%d): %s", b.auth.Method, b.auth.Mount, status, vaultSaid(body))
	}
	var answer struct {
		Auth struct {
			ClientToken   string `json:"client_token"`
			LeaseDuration int64  `json:"lease_duration"`
		} `json:"auth"`
	}
	if err := json.Unmarshal(body, &answer); err != nil || answer.Auth.ClientToken == "" {
		return "", false, fmt.Errorf("vault's answer to the %s login has no token", b.auth.Method)
	}
	b.token, b.expires = answer.Auth.ClientToken, time.Time{}
	if answer.Auth.LeaseDuration > 0 {
		b.expires = secretNow().Add(time.Duration(answer.Auth.LeaseDuration) * time.Second * 9 / 10)
	}
	return b.token, false, nil
}

// vaultCredential is what the exporter logs in with: the content of the
// file key names, or, when no file is named, of the environment variable.
func vaultCredential(key, file, variable string) (string, error) {
	if file == "" {
		if value := strings.TrimSpace(os.Getenv(variable)); variable != "" && value != "" {
			return value, nil
		}
		return "", fmt.Errorf("%s is not set, and neither is the environment variable %s", key, variable)
	}
	value, err := ReadCredentialFile(file)
	if err != nil {
		if file == vaultServiceAccountToken && errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("reading %s: %w; the pod mounts no service account token: mount one (automountServiceAccountToken, the chart's serviceAccount.automount), or name a projected token with auth.jwt_file", key, err)
		}
		return "", fmt.Errorf("reading %s: %w", key, err)
	}
	if value == "" {
		return "", fmt.Errorf("%s %s is empty", key, file)
	}
	return value, nil
}
