package fetch

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// storesInForce checks entries and puts them in force for the test, with a
// clock the test moves.
func storesInForce(t *testing.T, entries ...model.SecretStore) *time.Time {
	t.Helper()
	if err := ValidateSecretStores(entries); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	secretNow = func() time.Time { return now }
	FollowSecretStores(nil)
	FollowSecretStores(entries)
	t.Cleanup(func() {
		secretNow = time.Now
		FollowSecretStores(nil)
	})
	return &now
}

// vaultStandIn is a Vault server of one KV version 2 secret, secret/data/db,
// and one of version 1, legacy/db, which a token it gave at a kubernetes or
// approle login reads.
type vaultStandIn struct {
	*httptest.Server
	mu sync.Mutex
	// logins and reads count the requests; down answers every one with
	// 503; revoked refuses the tokens given so far.
	logins, reads int
	down          bool
	revoked       map[string]bool
	namespaces    []string
	password      string
}

func startVault(t *testing.T) *vaultStandIn {
	t.Helper()
	v := &vaultStandIn{revoked: map[string]bool{}, password: "s3cret"}
	v.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v.mu.Lock()
		defer v.mu.Unlock()
		v.namespaces = append(v.namespaces, r.Header.Get("X-Vault-Namespace"))
		if v.down {
			http.Error(w, `{"errors":["Vault is sealed"]}`, http.StatusServiceUnavailable)
			return
		}
		body, _ := io.ReadAll(r.Body)
		switch r.URL.Path {
		case "/v1/auth/kubernetes/login", "/v1/auth/approle/login":
			var login map[string]string
			_ = json.Unmarshal(body, &login)
			if !(login["role"] == "exporter" && login["jwt"] == "service-account-jwt") && !(login["role_id"] == "role-1" && login["secret_id"] == "secret-id-1") {
				http.Error(w, `{"errors":["permission denied"]}`, http.StatusForbidden)
				return
			}
			v.logins++
			_, _ = fmt.Fprintf(w, `{"auth":{"client_token":"token-%d","lease_duration":3600}}`, v.logins)
		case "/v1/secret/data/db", "/v1/legacy/db":
			token := r.Header.Get("X-Vault-Token")
			if !strings.HasPrefix(token, "token-") && token != "root" || v.revoked[token] {
				http.Error(w, `{"errors":["permission denied"]}`, http.StatusForbidden)
				return
			}
			v.reads++
			if r.URL.Path == "/v1/legacy/db" {
				_, _ = fmt.Fprintf(w, `{"data":{"password":%q}}`, v.password)
				return
			}
			_, _ = fmt.Fprintf(w, `{"data":{"data":{"password":%q,"port":5432,"version":%q},"metadata":{"version":3}}}`, v.password+"\n", r.URL.Query().Get("version"))
		default:
			http.Error(w, `{"errors":[]}`, http.StatusNotFound)
		}
	}))
	t.Cleanup(v.Close)
	return v
}

func (v *vaultStandIn) counts() (logins, reads int) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.logins, v.reads
}

func (v *vaultStandIn) set(change func(*vaultStandIn)) {
	v.mu.Lock()
	defer v.mu.Unlock()
	change(v)
}

func kubernetesVault(t *testing.T, v *vaultStandIn, edit func(*model.SecretStore)) model.SecretStore {
	t.Helper()
	jwt := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(jwt, []byte("service-account-jwt\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	e := model.SecretStore{Name: "vault-prod", Type: "vault", Address: v.URL, Auth: &model.SecretStoreAuth{Method: "kubernetes", Role: "exporter", JWTFile: jwt}}
	if edit != nil {
		edit(&e)
	}
	return e
}

// A key that names a credential file reads a reference from its store: the
// field of the secret, without the whitespace around it. The exporter logs
// in once, and a secret read is kept for cache_ttl: its fields, and reads
// of it within that time, cost the store nothing; after it the store is
// asked again, and what it holds then is what is read.
func TestACredentialIsReadFromVaultAndKept(t *testing.T) {
	vault := startVault(t)
	now := storesInForce(t, kubernetesVault(t, vault, func(e *model.SecretStore) { e.Namespace = "team-a" }))
	const ref = "secret://vault-prod/secret/data/db#password"
	for range 3 {
		if got, err := ReadCredentialFile(ref); err != nil || got != "s3cret" {
			t.Fatalf("%q, %v", got, err)
		}
	}
	if got, err := ReadCredentialFile("secret://vault-prod/secret/data/db#port"); err != nil || got != "5432" {
		t.Fatalf("a number of the secret: %q, %v", got, err)
	}
	if logins, reads := vault.counts(); logins != 1 || reads != 1 {
		t.Fatalf("%d logins and %d reads, want one of each", logins, reads)
	}
	if got, err := ReadCredentialFile("secret://vault-prod/secret/data/db?version=2#version"); err != nil || got != "2" {
		t.Fatalf("a version: %q, %v", got, err)
	}
	vault.set(func(v *vaultStandIn) { v.password = "rotated" })
	*now = now.Add(4 * time.Minute)
	if got, _ := ReadCredentialFile(ref); got != "s3cret" {
		t.Fatalf("within cache_ttl the store was asked again: %q", got)
	}
	*now = now.Add(2 * time.Minute)
	if got, err := ReadCredentialFile(ref); err != nil || got != "rotated" {
		t.Fatalf("past cache_ttl: %q, %v", got, err)
	}
	if logins, _ := vault.counts(); logins != 1 {
		t.Fatalf("%d logins; the token is kept for its lease", logins)
	}
	vault.mu.Lock()
	for _, namespace := range vault.namespaces {
		if namespace != "team-a" {
			t.Errorf("a request without the namespace: %q", vault.namespaces)
			break
		}
	}
	vault.mu.Unlock()
	if got, err := ReadCredentialFile("secret://vault-prod/legacy/db#password"); err != nil || got != "rotated" {
		t.Fatalf("a KV version 1 secret: %q, %v", got, err)
	}
	counts := SecretStoreCounts()
	if len(counts) != 1 || counts[0].Name != "vault-prod" || counts[0].Requests != 4 || counts[0].Failed != 0 || counts[0].Stale != 0 {
		t.Errorf("counts %+v", counts)
	}
}

// When the store cannot be asked past cache_ttl, the secret read before is
// used, and counted, for max_stale more, the store asked again no sooner
// than a few seconds after it failed; past max_stale the read fails, as
// does the read of a secret never read. A token Vault refuses is replaced
// by logging in again, once.
func TestASecretKeptIsUsedWhileTheStoreIsDown(t *testing.T) {
	vault := startVault(t)
	stale := model.Duration(10 * time.Minute)
	now := storesInForce(t, kubernetesVault(t, vault, func(e *model.SecretStore) { e.MaxStale = &stale }))
	const ref = "secret://vault-prod/secret/data/db#password"
	if _, err := ReadCredentialFile(ref); err != nil {
		t.Fatal(err)
	}
	vault.set(func(v *vaultStandIn) { v.down = true })
	*now = now.Add(6 * time.Minute)
	for range 3 {
		if got, err := ReadCredentialFile(ref); err != nil || got != "s3cret" {
			t.Fatalf("with the store down: %q, %v", got, err)
		}
	}
	if c := SecretStoreCounts()[0]; c.Requests != 2 || c.Failed != 1 || c.Stale != 3 {
		t.Errorf("counts %+v, want the store asked once more, and three stale reads", c)
	}
	if _, err := ReadCredentialFile("secret://vault-prod/legacy/db#password"); err == nil || !strings.Contains(err.Error(), "reading secret://vault-prod/legacy/db: vault answered 503: Vault is sealed") {
		t.Errorf("a secret never read: %v", err)
	}
	*now = now.Add(10 * time.Minute)
	if _, err := ReadCredentialFile(ref); err == nil || !strings.Contains(err.Error(), "vault answered 503") {
		t.Errorf("past max_stale: %v", err)
	}
	vault.set(func(v *vaultStandIn) { v.down, v.revoked["token-1"] = false, true })
	*now = now.Add(time.Minute)
	if got, err := ReadCredentialFile(ref); err != nil || got != "s3cret" {
		t.Fatalf("after the token was revoked: %q, %v", got, err)
	}
	if logins, _ := vault.counts(); logins != 2 {
		t.Errorf("%d logins, want a second for the token refused", logins)
	}
}

// A vault store logs in with a token, of a file or of VAULT_TOKEN, and
// with an AppRole; a login Vault refuses, a secret that is not there and a
// field it does not have each fail the read, saying which.
func TestTheWaysAVaultStoreLogsIn(t *testing.T) {
	vault := startVault(t)
	tokenFile := filepath.Join(t.TempDir(), "vault-token")
	if err := os.WriteFile(tokenFile, []byte("root\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VAULT_TOKEN", "root")
	t.Setenv("VAULT_SECRET_ID", "secret-id-1")
	storesInForce(t,
		model.SecretStore{Name: "by-file", Type: "vault", Address: vault.URL, Auth: &model.SecretStoreAuth{Method: "token", TokenFile: tokenFile}},
		model.SecretStore{Name: "by-env", Type: "vault", Address: vault.URL, Auth: &model.SecretStoreAuth{Method: "token"}},
		model.SecretStore{Name: "by-approle", Type: "vault", Address: vault.URL, Auth: &model.SecretStoreAuth{Method: "approle", RoleID: "role-1"}},
		model.SecretStore{Name: "wrong-role", Type: "vault", Address: vault.URL, Auth: &model.SecretStoreAuth{Method: "approle", RoleID: "role-2"}},
	)
	for _, store := range []string{"by-file", "by-env", "by-approle"} {
		if got, err := ReadCredentialFile("secret://" + store + "/secret/data/db#password"); err != nil || got != "s3cret" {
			t.Errorf("%s: %q, %v", store, got, err)
		}
	}
	for ref, want := range map[string]string{
		"secret://wrong-role/secret/data/db#password": "vault refused the approle login at auth/approle (403): permission denied",
		"secret://by-file/secret/data/other#password": "vault has no secret at secret/data/other (404); for a KV version 2 mount the path has data/ after the mount",
		"secret://by-file/secret/data/db#user":        `names the field "user", which the secret does not have`,
		"secret://nowhere/secret/data/db#password":    `names the secret store "nowhere", and secret_stores has none of that name`,
	} {
		if _, err := ReadCredentialFile(ref); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", ref, err, want)
		}
	}
}

// A gcp store reads the payload of a secret's version with an access token
// of the metadata server, or one a service account's key is exchanged for,
// the request for it signed with the key; the payload is the credential,
// or, with a field, a member of the JSON object it is.
func TestACredentialIsReadFromGoogleSecretManager(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var asked []string
	google := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		asked = append(asked, r.URL.Path)
		mu.Unlock()
		switch {
		case r.URL.Path == "/computeMetadata/v1/instance/service-accounts/default/token" && r.Header.Get("Metadata-Flavor") == "Google":
			_, _ = io.WriteString(w, `{"access_token":"metadata-token","expires_in":3600}`)
		case r.URL.Path == "/token":
			_ = r.ParseForm()
			parts := strings.Split(r.PostForm.Get("assertion"), ".")
			if len(parts) != 3 || r.PostForm.Get("grant_type") != gcpJWTGrant {
				http.Error(w, "no assertion", http.StatusBadRequest)
				return
			}
			digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
			signature, _ := base64.RawURLEncoding.DecodeString(parts[2])
			claims, _ := base64.RawURLEncoding.DecodeString(parts[1])
			if rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, digest[:], signature) != nil || !strings.Contains(string(claims), `"iss":"exporter@demo.iam.gserviceaccount.com"`) {
				http.Error(w, "bad signature", http.StatusUnauthorized)
				return
			}
			_, _ = io.WriteString(w, `{"access_token":"key-token","expires_in":3600}`)
		case strings.HasPrefix(r.URL.Path, "/v1/projects/demo/secrets/"):
			if auth := r.Header.Get("Authorization"); auth != "Bearer metadata-token" && auth != "Bearer key-token" {
				http.Error(w, `{"error":{"code":401,"message":"no token","status":"UNAUTHENTICATED"}}`, http.StatusUnauthorized)
				return
			}
			payload := map[string]string{
				"/v1/projects/demo/secrets/api-token/versions/latest:access": "tok-123\n",
				"/v1/projects/demo/secrets/api-token/versions/4:access":      "tok-old",
				"/v1/projects/demo/secrets/db/versions/latest:access":        `{"user":"monitor","password":"p4ss"}`,
			}[r.URL.Path]
			if payload == "" {
				http.Error(w, `{"error":{"code":404,"message":"Secret not found","status":"NOT_FOUND"}}`, http.StatusNotFound)
				return
			}
			_, _ = fmt.Fprintf(w, `{"name":"x","payload":{"data":%q}}`, base64.StdEncoding.EncodeToString([]byte(payload)))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(google.Close)
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	keyJSON, _ := json.Marshal(map[string]string{"type": "service_account", "client_email": "exporter@demo.iam.gserviceaccount.com", "private_key_id": "k1",
		"private_key": string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})), "token_uri": google.URL + "/token"})
	keyFile := filepath.Join(t.TempDir(), "key.json")
	if err := os.WriteFile(keyFile, keyJSON, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GCE_METADATA_HOST", strings.TrimPrefix(google.URL, "http://"))
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "")
	storesInForce(t,
		model.SecretStore{Name: "gcp", Type: "gcp", Project: "demo", Endpoint: google.URL},
		model.SecretStore{Name: "gcp-key", Type: "gcp", Project: "demo", Endpoint: google.URL, CredentialsFile: keyFile},
	)
	for ref, want := range map[string]string{
		"secret://gcp/api-token":           "tok-123",
		"secret://gcp/api-token?version=4": "tok-old",
		"secret://gcp/db#password":         "p4ss",
		"secret://gcp-key/api-token":       "tok-123",
	} {
		if got, err := ReadCredentialFile(ref); err != nil || got != want {
			t.Errorf("%s: %q, %v; want %q", ref, got, err, want)
		}
	}
	for ref, want := range map[string]string{
		"secret://gcp/missing":       "secret manager answered 404: NOT_FOUND Secret not found",
		"secret://gcp/api-token#key": `names the field "key", and the secret is not a JSON object of fields`,
		"secret://gcp/a/b":           "a gcp reference names the secret alone",
	} {
		if _, err := ReadCredentialFile(ref); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", ref, err, want)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	tokens := 0
	for _, path := range asked {
		if strings.HasSuffix(path, "/token") {
			tokens++
		}
	}
	if tokens != 2 {
		t.Errorf("%d requests for a token, want one of each store: %q", tokens, asked)
	}
}

// secret_stores is checked when the configuration loads: a name a
// reference can write, a type, the keys the type needs and none of
// another's.
func TestSecretStoresAreCheckedAtLoad(t *testing.T) {
	minus := model.Duration(-time.Second)
	vault := func(edit func(*model.SecretStore)) []model.SecretStore {
		e := model.SecretStore{Name: "vault", Type: "vault", Address: "https://vault.example.com:8200", Auth: &model.SecretStoreAuth{Method: "kubernetes", Role: "exporter"}}
		edit(&e)
		return []model.SecretStore{e}
	}
	for want, entries := range map[string][]model.SecretStore{
		"has no name":                              vault(func(e *model.SecretStore) { e.Name = "" }),
		"has an invalid name":                      vault(func(e *model.SecretStore) { e.Name = "my store" }),
		"is named twice":                           append(vault(func(*model.SecretStore) {}), vault(func(*model.SecretStore) {})...),
		"has no type; want vault, gcp or aws":      vault(func(e *model.SecretStore) { e.Type = "" }),
		`has the type "azure"`:                     vault(func(e *model.SecretStore) { e.Type = "azure" }),
		"has no address":                           vault(func(e *model.SecretStore) { e.Address = "" }),
		`has the address "vault.example.com:8200"`: vault(func(e *model.SecretStore) { e.Address = "vault.example.com:8200" }),
		"has no auth.method":                       vault(func(e *model.SecretStore) { e.Auth = nil }),
		`has the auth.method "ldap"`:               vault(func(e *model.SecretStore) { e.Auth.Method = "ldap" }),
		"no auth.role":                             vault(func(e *model.SecretStore) { e.Auth.Role = "" }),
		"sets auth.role_id, which auth.method kub": vault(func(e *model.SecretStore) { e.Auth.RoleID = "r" }),
		"no auth.role_id":                          vault(func(e *model.SecretStore) { e.Auth = &model.SecretStoreAuth{Method: "approle"} }),
		"sets auth.role, which auth.method token":  vault(func(e *model.SecretStore) { e.Auth.Method = "token" }),
		"auth.token_file that is a reference": vault(func(e *model.SecretStore) {
			e.Auth = &model.SecretStoreAuth{Method: "token", TokenFile: "secret://vault/a#b"}
		}),
		"sets project, which a store of type vaul": vault(func(e *model.SecretStore) { e.Project = "p" }),
		"has a cache_ttl of -1s":                   vault(func(e *model.SecretStore) { e.CacheTTL = &minus }),
		"has a max_stale of -1s":                   vault(func(e *model.SecretStore) { e.MaxStale = &minus }),
		"has a timeout of 2m0s":                    vault(func(e *model.SecretStore) { e.Timeout = model.Duration(2 * time.Minute) }),
		"has no project":                           {{Name: "g", Type: "gcp"}},
		"sets address, which a store of type gcp":  {{Name: "g", Type: "gcp", Project: "p", Address: "https://x"}},
		`has the endpoint "ftp://x"`:               {{Name: "g", Type: "gcp", Project: "p", Endpoint: "ftp://x"}},
		"sets project, which a store of type aws":  {{Name: "a", Type: "aws", Project: "p"}},
	} {
		if err := ValidateSecretStores(entries); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("err=%v, want %q", err, want)
		}
	}
	if err := ValidateSecretStores(append(vault(func(*model.SecretStore) {}), model.SecretStore{Name: "g", Type: "gcp", Project: "p"})); err != nil {
		t.Error(err)
	}
}

// A reference is taken by a key that names a credential file, written as
// one and naming a store the configuration has; in a key whose file is
// opened as a file, in any other key, written badly or naming no store, it
// is refused, saying where.
func TestReferencesToSecretStoresAreChecked(t *testing.T) {
	stores := []model.SecretStore{{Name: "vault", Type: "vault"}, {Name: "gcp", Type: "gcp"}}
	collector := func(edit func(*model.Collector)) *model.Collector {
		c := &model.Collector{Name: "c"}
		edit(c)
		return c
	}
	for want, c := range map[string]*model.Collector{
		"password_file: it names the secret store \"other\"":                        collector(func(c *model.Collector) { c.Request.PasswordFile = "secret://other/a#b" }),
		"password_file is secret://vault/a, which names no field":                   collector(func(c *model.Collector) { c.Request.PasswordFile = "secret://vault/a" }),
		"password_file: \"secret://vault/a#\" ends in # and names no field":         collector(func(c *model.Collector) { c.Request.PasswordFile = "secret://vault/a#" }),
		"password_file: \"secret://vault\" names no secret":                         collector(func(c *model.Collector) { c.Request.PasswordFile = "secret://vault" }),
		"has a placeholder; a reference to a secret store is used as it is written": collector(func(c *model.Collector) { c.Request.PasswordFile = "secret://vault/{{param_team}}/db#password" }),
		"takes ?version= alone": collector(func(c *model.Collector) { c.Request.PasswordFile = "secret://gcp/a?v=1" }),
		"ca_file is secret://vault/ca#pem, a reference to a secret store, and the key":  collector(func(c *model.Collector) { c.Request.TLS.CAFile = "secret://vault/ca#pem" }),
		"private_key_file is secret://vault/k#key":                                      collector(func(c *model.Collector) { c.Request.PrivateKeyFile = "secret://vault/k#key" }),
		"username is secret://vault/a#user, a reference to a secret store, which only ": collector(func(c *model.Collector) { c.Request.Username = "secret://vault/a#user" }),
	} {
		if err := NewSecretReferences(stores).Check("collector", "c", c); err == nil || !strings.Contains(err.Error(), want) || !strings.HasPrefix(err.Error(), `collector "c": `) {
			t.Errorf("err=%v, want %q", err, want)
		}
	}
	taken := collector(func(c *model.Collector) {
		c.Request.PasswordFile = "secret://vault/secret/data/db#password"
		c.Request.BearerTokenFile = " secret://gcp/api-token?version=3 "
		c.Request.TLS.CAFile = "/etc/ssl/ca.pem"
	})
	if err := NewSecretReferences(stores).Check("collector", "c", taken); err != nil {
		t.Error(err)
	}
}
