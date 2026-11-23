package fetch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A credential is read from a file, at each scrape, by the key that names
// the file: password_file, bearer_token_file, community_file and the rest.
// Where a secret may not be written to disk, the same key holds a reference
// to a secret store instead of a path:
//
//	password_file: secret://vault-prod/secret/data/db#password
//
// secret://, the name of an entry of secret_stores, the secret's path in
// the store, ?version= for a version other than the current one, and
// #field for one field of a secret that holds several. Every credential the
// exporter reads through ReadCredentialFile can be read so; the files it
// opens otherwise, certificates and private keys among them, cannot, and a
// reference in one of their keys is refused when the configuration loads
// (SecretReferences.Check).
//
// A store is HashiCorp Vault, Google Cloud Secret Manager or AWS Secrets
// Manager, each asked over its HTTP API (secretstore_vault.go,
// secretstore_gcp.go, secretstore_aws.go). What a store answered is kept in
// memory, by secret, for cache_ttl, so a scrape costs the store nothing
// while it is fresh, and the fields of one secret cost one request. When
// the store cannot be asked once that has passed, the secret kept is used
// for max_stale more, logged once and counted, since a store that is down
// for a minute should not stop every collector that reads from it; past
// that, and for a secret never read, the read fails, and so does its
// scrape. A store that failed is not asked again for the same secret for
// secretRetryAfter, the reads in between failing, or using the kept
// secret, at once.

const (
	// SecretReferencePrefix is what a reference to a secret store starts
	// with, in a key that names a credential file.
	SecretReferencePrefix = "secret://"

	secretStoreVault = "vault"
	secretStoreGCP   = "gcp"
	secretStoreAWS   = "aws"

	secretDefaultCacheTTL = 5 * time.Minute
	secretDefaultMaxStale = time.Hour
	secretDefaultTimeout  = 5 * time.Second
	secretMaxTimeout      = time.Minute
	// secretRetryAfter is how long a store that failed for a secret is
	// not asked for it again.
	secretRetryAfter = 5 * time.Second
	// secretsKept is how many secrets a store keeps; past it they are all
	// forgotten, and read again as they are asked for.
	secretsKept = 1024
	// secretMaxAnswer is the most bytes of a store's answer that are read.
	secretMaxAnswer = 1 << 20
)

// IsSecretReference reports whether the value of a key that names a
// credential file is a reference to a secret store.
func IsSecretReference(value string) bool {
	return strings.HasPrefix(strings.TrimSpace(value), SecretReferencePrefix)
}

// secretRef is a reference: the store it names, the secret's path there,
// the version asked for, "" for the current one, and the field, "" for the
// secret whole.
type secretRef struct {
	store, path, version, field string
}

// secretStoreNamePattern is the name an entry of secret_stores may have,
// which a reference writes as a URL's host.
var secretStoreNamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]*$`)

// parseSecretRef reads secret://store/path[?version=v][#field].
func parseSecretRef(value string) (secretRef, error) {
	written := strings.TrimSpace(value)
	rest, ok := strings.CutPrefix(written, SecretReferencePrefix)
	if !ok {
		return secretRef{}, fmt.Errorf("%q is not a reference to a secret store; one is written %sstore/path#field", value, SecretReferencePrefix)
	}
	// Which secret is read is the configuration's to say: a placeholder
	// would let a probe choose one, and the target it is sent to.
	if strings.Contains(rest, "{{") {
		return secretRef{}, fmt.Errorf("%q has a placeholder; a reference to a secret store is used as it is written", written)
	}
	var ref secretRef
	if i := strings.LastIndexByte(rest, '#'); i >= 0 {
		rest, ref.field = rest[:i], rest[i+1:]
		if ref.field == "" {
			return secretRef{}, fmt.Errorf("%q ends in # and names no field after it", written)
		}
	}
	if i := strings.IndexByte(rest, '?'); i >= 0 {
		query, err := url.ParseQuery(rest[i+1:])
		if err != nil || len(query) != 1 || len(query["version"]) != 1 || query.Get("version") == "" {
			return secretRef{}, fmt.Errorf("%q has the query %q; a reference takes ?version= alone, the version of the secret to read", written, rest[i+1:])
		}
		rest, ref.version = rest[:i], query.Get("version")
	}
	ref.store, ref.path, _ = strings.Cut(rest, "/")
	switch {
	case !secretStoreNamePattern.MatchString(ref.store):
		return secretRef{}, fmt.Errorf("%q names no secret store; a reference is written %sstore/path#field, the store by its name in secret_stores", written, SecretReferencePrefix)
	case strings.Trim(ref.path, "/") == "":
		return secretRef{}, fmt.Errorf("%q names no secret; after the store comes the secret's path in it", written)
	case strings.ContainsFunc(ref.path+ref.version+ref.field, func(r rune) bool { return r < 0x20 || r == 0x7f }):
		return secretRef{}, fmt.Errorf("%q has a control character", written)
	}
	ref.path = strings.Trim(ref.path, "/")
	return ref, nil
}

// secretDocument is a secret as its store answered it: text, the whole of
// a secret that is one value, or fields, those of a secret that is several.
// A secret of text that is a JSON object has its members for fields too.
type secretDocument struct {
	text    string
	fields  map[string]any
	isField bool
}

// value is the field asked for, or the secret whole.
func (d secretDocument) value(ref secretRef, written string) (string, error) {
	if ref.field == "" {
		if d.isField {
			return "", fmt.Errorf("%s names no field, and the secret has several: write the one to read after a #, as %s#password", written, written)
		}
		return strings.TrimSpace(d.text), nil
	}
	fields := d.fields
	if !d.isField {
		if err := json.Unmarshal([]byte(d.text), &fields); err != nil || fields == nil {
			return "", fmt.Errorf("%s names the field %q, and the secret is not a JSON object of fields", written, ref.field)
		}
	}
	found, ok := fields[ref.field]
	if !ok {
		return "", fmt.Errorf("%s names the field %q, which the secret does not have", written, ref.field)
	}
	switch v := found.(type) {
	case string:
		return strings.TrimSpace(v), nil
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64), nil
	case json.Number:
		return v.String(), nil
	case bool:
		return strconv.FormatBool(v), nil
	}
	return "", fmt.Errorf("%s names the field %q, which is not text, a number or a boolean", written, ref.field)
}

// secretBackend asks one kind of store for a secret.
type secretBackend interface {
	read(ctx context.Context, ref secretRef) (secretDocument, error)
}

// secretStore is an entry of secret_stores in force: its backend, the
// secrets it has read, and what it has counted.
type secretStore struct {
	entry    model.SecretStore
	backend  secretBackend
	cacheTTL time.Duration
	maxStale time.Duration
	timeout  time.Duration

	mu   sync.Mutex
	kept map[string]*secretKept

	requests, failures, stale atomic.Uint64
	since                     time.Time
}

// secretKept is one secret of a store: the document last read and when,
// the failure of the last request for it and when, and a lock that one
// request for it at a time holds.
type secretKept struct {
	mu       sync.Mutex
	document secretDocument
	read     time.Time
	err      error
	failed   time.Time
}

// secretStoresInForce are the stores of the configuration in force, by
// name.
var secretStoresInForce atomic.Pointer[map[string]*secretStore]

// secretNow is the clock of the stores' caches; a test sets it.
var secretNow = time.Now

// readSecret is the credential a reference names, read from its store or
// from what the store answered within cache_ttl.
func readSecret(value string) (string, error) {
	ref, err := parseSecretRef(value)
	if err != nil {
		return "", err
	}
	written := strings.TrimSpace(value)
	var store *secretStore
	if stores := secretStoresInForce.Load(); stores != nil {
		store = (*stores)[ref.store]
	}
	if store == nil {
		return "", fmt.Errorf("%s names the secret store %q, and secret_stores has none of that name", written, ref.store)
	}
	document, err := store.document(ref)
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", secretShown(ref), err)
	}
	secret, err := document.value(ref, secretShown(ref))
	if err != nil {
		return "", err
	}
	return secret, nil
}

// secretShown is a reference as a failure names it: without its field's
// value, which it never had, and whole, since nothing of it is secret.
func secretShown(ref secretRef) string {
	shown := SecretReferencePrefix + ref.store + "/" + ref.path
	if ref.version != "" {
		shown += "?version=" + ref.version
	}
	return shown
}

// document is the secret of ref: the one kept while it is fresh, and
// otherwise the store's answer, or, when the store fails, the one kept for
// as long as max_stale lets it be used.
func (s *secretStore) document(ref secretRef) (secretDocument, error) {
	key := ref.path + "\x00" + ref.version
	s.mu.Lock()
	kept := s.kept[key]
	if kept == nil {
		if len(s.kept) >= secretsKept {
			clear(s.kept)
		}
		kept = &secretKept{}
		s.kept[key] = kept
	}
	s.mu.Unlock()

	kept.mu.Lock()
	defer kept.mu.Unlock()
	now := secretNow()
	have := !kept.read.IsZero()
	if have && now.Sub(kept.read) < s.cacheTTL {
		return kept.document, nil
	}
	usable := have && now.Sub(kept.read) < s.cacheTTL+s.maxStale
	if kept.err != nil && now.Sub(kept.failed) < secretRetryAfter {
		if usable {
			s.stale.Add(1)
			return kept.document, nil
		}
		return secretDocument{}, kept.err
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
	document, err := s.backend.read(ctx, ref)
	cancel()
	s.requests.Add(1)
	if err == nil {
		if kept.err != nil {
			slog.Default().Info("a secret store answers again", "store", s.entry.Name, "secret", secretShown(ref))
		}
		kept.document, kept.read, kept.err = document, secretNow(), nil
		return document, nil
	}
	s.failures.Add(1)
	first := kept.err == nil
	kept.err, kept.failed = err, secretNow()
	if !usable {
		return secretDocument{}, err
	}
	s.stale.Add(1)
	if first {
		slog.Default().Warn("a secret store could not be read; the secret read before is used until it is max_stale past its cache_ttl", "store", s.entry.Name, "secret", secretShown(ref), "read", kept.read.UTC().Format(time.RFC3339), "error", err.Error())
	}
	return kept.document, nil
}

// ValidateSecretStores checks secret_stores: each entry a name of its own
// that a reference can write, a type, the keys its type needs and none of
// another's, and durations that bound something. Every mistake is reported.
func ValidateSecretStores(entries []model.SecretStore) error {
	var errs []error
	names := map[string]bool{}
	for i := range entries {
		e := &entries[i]
		at := fmt.Sprintf("secret_stores entry %q", e.Name)
		switch {
		case e.Name == "":
			at = fmt.Sprintf("secret_stores entry %d", i+1)
			errs = append(errs, fmt.Errorf("%s has no name; name it, as a reference names it: %sname/path#field", at, SecretReferencePrefix))
		case !secretStoreNamePattern.MatchString(e.Name):
			errs = append(errs, fmt.Errorf("%s has an invalid name: letters, digits, hyphens and underscores, starting with a letter or a digit", at))
		case names[e.Name]:
			errs = append(errs, fmt.Errorf("%s is named twice; each entry's name must be unique", at))
		}
		names[e.Name] = true
		if err := checkSecretStore(e); err != nil {
			errs = append(errs, fmt.Errorf("%s %w", at, err))
		}
	}
	return model.JoinProblems(errs...)
}

// checkSecretStore holds an entry to its type.
func checkSecretStore(e *model.SecretStore) error {
	other := func(keys ...string) error {
		set := map[string]bool{
			"address": e.Address != "", "namespace": e.Namespace != "", "auth": e.Auth != nil, "tls": e.TLS != (model.TLSConfig{}),
			"project": e.Project != "", "credentials_file": e.CredentialsFile != "", "region": e.Region != "", "endpoint": e.Endpoint != "",
		}
		for _, key := range keys {
			if set[key] {
				return fmt.Errorf("sets %s, which a store of type %s does not take", key, e.Type)
			}
		}
		return nil
	}
	switch {
	case e.CacheTTL != nil && *e.CacheTTL < 0:
		return fmt.Errorf("has a cache_ttl of %s; it is how long a secret read is used before the store is asked again, 0 to ask at every read", time.Duration(*e.CacheTTL))
	case e.MaxStale != nil && *e.MaxStale < 0:
		return fmt.Errorf("has a max_stale of %s; it is how long past cache_ttl a secret is used while the store cannot be asked, 0 for never", time.Duration(*e.MaxStale))
	case e.Timeout < 0 || time.Duration(e.Timeout) > secretMaxTimeout:
		return fmt.Errorf("has a timeout of %s; it bounds one request to the store, at most %s, 5s when left out", time.Duration(e.Timeout), secretMaxTimeout)
	}
	switch e.Type {
	case "":
		return errors.New("has no type; want vault, gcp or aws")
	case secretStoreVault:
		if err := other("project", "credentials_file", "region", "endpoint"); err != nil {
			return err
		}
		return checkVaultStore(e)
	case secretStoreGCP:
		if err := other("address", "namespace", "auth", "tls", "region"); err != nil {
			return err
		}
		if err := checkSecretStoreURL("endpoint", e.Endpoint); err != nil {
			return err
		}
		if strings.TrimSpace(e.Project) == "" {
			return errors.New("has no project; name the Google Cloud project whose secrets are read")
		}
		if IsSecretReference(e.CredentialsFile) {
			return errors.New("has a credentials_file that is a reference to a secret store; the key a store is reached with is a file")
		}
		return nil
	case secretStoreAWS:
		if err := other("address", "namespace", "auth", "tls", "project", "credentials_file"); err != nil {
			return err
		}
		if err := checkSecretStoreURL("endpoint", e.Endpoint); err != nil {
			return err
		}
		return checkAWSStore(e)
	}
	return fmt.Errorf("has the type %q; want vault, gcp or aws, in lower case", e.Type)
}

// checkSecretStoreURL holds a store's address or endpoint to an http or
// https URL of a host, without credentials, query or fragment.
func checkSecretStoreURL(key, value string) error {
	if value == "" {
		return nil
	}
	u, err := url.Parse(value)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("has the %s %q; want an http or https URL of the server, such as https://vault.example.com:8200, without credentials or a query", key, value)
	}
	return nil
}

// newSecretStore makes the store of a checked entry.
func newSecretStore(e model.SecretStore) (*secretStore, error) {
	s := &secretStore{entry: e, cacheTTL: secretDefaultCacheTTL, maxStale: secretDefaultMaxStale, timeout: secretDefaultTimeout, kept: map[string]*secretKept{}}
	if e.CacheTTL != nil {
		s.cacheTTL = time.Duration(*e.CacheTTL)
	}
	if e.MaxStale != nil {
		s.maxStale = time.Duration(*e.MaxStale)
	}
	if e.Timeout > 0 {
		s.timeout = time.Duration(e.Timeout)
	}
	var err error
	switch e.Type {
	case secretStoreVault:
		s.backend, err = newVaultBackend(e, s.timeout)
	case secretStoreGCP:
		s.backend, err = newGCPBackend(e, s.timeout)
	case secretStoreAWS:
		s.backend, err = newAWSBackend(e, s.timeout)
	default:
		err = fmt.Errorf("type %q", e.Type)
	}
	return s, err
}

// FollowSecretStores puts the entries of secret_stores of the
// configuration in force. A store a reload left as it was is kept, with the
// secrets it has read and its login; one it changed starts anew, and so
// reads each secret again. The counts of a store are kept while a store of
// its name is in force.
func FollowSecretStores(entries []model.SecretStore) {
	var before map[string]*secretStore
	if p := secretStoresInForce.Load(); p != nil {
		before = *p
	}
	stores := make(map[string]*secretStore, len(entries))
	for i := range entries {
		e := entries[i]
		was := before[e.Name]
		if was != nil && reflect.DeepEqual(was.entry, e) {
			stores[e.Name] = was
			continue
		}
		s, err := newSecretStore(e)
		if err != nil {
			// A checked configuration has no such entry; a read from it
			// says what is wrong with it.
			s.backend = failedBackend{err: err}
		}
		switch {
		case was != nil:
			s.requests.Store(was.requests.Load())
			s.failures.Store(was.failures.Load())
			s.stale.Store(was.stale.Load())
			s.since = was.since
		case before != nil:
			s.since = time.Now()
		}
		stores[e.Name] = s
	}
	secretStoresInForce.Store(&stores)
}

// failedBackend is the backend of a store that could not be made.
type failedBackend struct{ err error }

func (b failedBackend) read(context.Context, secretRef) (secretDocument, error) {
	return secretDocument{}, b.err
}

// SecretStoreCount is what a store of secret_stores has counted: the
// requests made to it for a secret, those that failed, the reads answered
// with a secret past its cache_ttl because the store could not be asked,
// and when it began to count, the zero time for since the exporter started.
type SecretStoreCount struct {
	Name                    string
	Requests, Failed, Stale uint64
	Since                   time.Time
}

// SecretStoreCounts are the counts of the stores in force, by name.
func SecretStoreCounts() []SecretStoreCount {
	p := secretStoresInForce.Load()
	if p == nil {
		return nil
	}
	counts := make([]SecretStoreCount, 0, len(*p))
	for name, s := range *p {
		counts = append(counts, SecretStoreCount{Name: name, Requests: s.requests.Load(), Failed: s.failures.Load(), Stale: s.stale.Load(), Since: s.since})
	}
	slices.SortFunc(counts, func(a, b SecretStoreCount) int { return strings.Compare(a.Name, b.Name) })
	return counts
}

// secretFileKeys are the keys that name a file the exporter opens as a
// file, and not through ReadCredentialFile: certificates and keys, which
// the TLS and SSH libraries read, descriptors and statements, which are no
// credentials, and the file a login keeps its token in. A reference to a
// secret store in one of them is refused.
var secretFileKeys = []string{"ca_file", "cert_file", "key_file", "kubelet_ca_file", "private_key_file", "known_hosts_file", "protoset_file", "proto_files", "statement_file", "keep_file"}

// SecretReferences checks the references to secret stores in a
// configuration or a static target file against the stores it was made
// with (SecretReferences.Check), by each store's type.
type SecretReferences map[string]string

// NewSecretReferences is the check against stores.
func NewSecretReferences(stores []model.SecretStore) SecretReferences {
	names := make(SecretReferences, len(stores))
	for i := range stores {
		names[stores[i].Name] = stores[i].Type
	}
	return names
}

// Check finds every reference to a secret store in v, a configuration, a
// static target or a part of one, and says what is wrong with the first
// that is not one a read would take: one that is not written as a
// reference is, one that names a store the configuration does not have,
// and one in a key whose file is not read as a credential. what and name
// say what v is, for the error: the configuration, or target and its name.
func (names SecretReferences) Check(what, name string, v any) error {
	key, written, err := names.walk(reflect.ValueOf(v), "")
	if err == nil {
		return nil
	}
	where := what
	if name != "" {
		where = fmt.Sprintf("%s %q", what, name)
	}
	if written == "" {
		return fmt.Errorf("%s: %s: %w", where, key, err)
	}
	return fmt.Errorf("%s: %s is %s, %w", where, key, written, err)
}

// The refusals of a reference that is one, which Check says after the key
// and the reference.
var (
	errSecretFileKey  = errors.New("a reference to a secret store, and the key names a file that is opened as one: a secret store holds the passwords and tokens the exporter reads at a scrape, not certificates, keys or descriptors")
	errSecretOtherKey = errors.New("a reference to a secret store, which only a key that names a credential file takes, such as password_file")
	errSecretNoField  = errors.New("which names no field; a Vault secret is fields, so write the one to read after a #, as secret://store/path#password")
)

// secretFieldKeysOf keeps, by struct type, the yaml key of each field, "-"
// for one that holds nothing a reference could be in: reading a field's tag
// allocates, and a file of a thousand targets is walked target by target.
var secretFieldKeysOf sync.Map

func secretFieldKeys(t reflect.Type) []string {
	if kept, ok := secretFieldKeysOf.Load(t); ok {
		return kept.([]string)
	}
	keys := make([]string, t.NumField())
	for i := range keys {
		field := t.Field(i)
		keys[i], _, _ = strings.Cut(field.Tag.Get("yaml"), ",")
		switch field.Type.Kind() {
		case reflect.Pointer, reflect.Interface, reflect.Struct, reflect.Slice, reflect.Array, reflect.Map, reflect.String:
		default:
			keys[i] = "-"
		}
		if !field.IsExported() {
			keys[i] = "-"
		}
	}
	secretFieldKeysOf.Store(t, keys)
	return keys
}

// walk is Check's: the key of the first reference in v that a read would
// not take, the reference when the refusal is said after it, and the
// refusal. key is the key v is the value of.
func (names SecretReferences) walk(v reflect.Value, key string) (at, written string, err error) {
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if v.IsNil() {
			return "", "", nil
		}
		return names.walk(v.Elem(), key)
	case reflect.Struct:
		for i, name := range secretFieldKeys(v.Type()) {
			switch name {
			case "-":
				continue
			case "":
				name = key
			}
			if at, written, err := names.walk(v.Field(i), name); err != nil {
				return at, written, err
			}
		}
	case reflect.Slice, reflect.Array:
		for i := range v.Len() {
			if at, written, err := names.walk(v.Index(i), key); err != nil {
				return at, written, err
			}
		}
	case reflect.Map:
		if v.Len() == 0 {
			return "", "", nil
		}
		// A mapping of text, as a target's params and labels are, is read
		// as one: walking it by reflection allocates for each target.
		if texts, ok := v.Interface().(map[string]string); ok {
			for _, value := range texts {
				if IsSecretReference(value) {
					return names.walk(reflect.ValueOf(value), key)
				}
			}
			return "", "", nil
		}
		for values := v.MapRange(); values.Next(); {
			if at, written, err := names.walk(values.Value(), key); err != nil {
				return at, written, err
			}
		}
	case reflect.String:
		value := v.String()
		if !IsSecretReference(value) {
			return "", "", nil
		}
		value = strings.TrimSpace(value)
		ref, err := parseSecretRef(value)
		switch {
		case slices.Contains(secretFileKeys, key):
			return key, value, errSecretFileKey
		case !strings.HasSuffix(key, "_file") && !strings.HasSuffix(key, "_files"):
			return key, value, errSecretOtherKey
		case err != nil:
			return key, "", err
		case names[ref.store] == "":
			return key, "", fmt.Errorf("it names the secret store %q, and secret_stores has none of that name", ref.store)
		case names[ref.store] == secretStoreVault && ref.field == "":
			return key, value, errSecretNoField
		}
	}
	return "", "", nil
}

// secretHTTP makes one request to a store and returns the body of its
// answer, held to secretMaxAnswer, with its status.
func secretHTTP(client *http.Client, req *http.Request) (int, []byte, error) {
	resp, err := client.Do(req) //nolint:gosec // G704: the store the configuration names, or its cloud's token endpoint
	if err != nil {
		var asURL *url.Error
		if errors.As(err, &asURL) {
			// Without the URL, which names the secret again and may carry
			// a query.
			err = asURL.Err
		}
		return 0, nil, fmt.Errorf("asking %s: %w", req.URL.Host, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, secretMaxAnswer+1))
	if err != nil {
		return 0, nil, fmt.Errorf("reading the answer of %s: %w", req.URL.Host, err)
	}
	if len(body) > secretMaxAnswer {
		return 0, nil, fmt.Errorf("the answer of %s is larger than %d bytes", req.URL.Host, secretMaxAnswer)
	}
	return resp.StatusCode, body, nil
}

// secretStoreSaid is what a store's refusal says, for a failure: the
// start of it, on one line, with what reads as a credential masked.
func secretStoreSaid(body []byte) string {
	text := strings.Join(strings.Fields(string(body)), " ")
	if text == "" {
		return "nothing more"
	}
	return model.CutTo(RedactText(text), 300, false)
}

// secretClient is the HTTP client a store is asked with: the default
// transport's proxy and dialing, the TLS settings given, no redirect to
// another host, since a redirect is followed with the store's token, and
// timeout for the whole of one request.
func secretClient(tlsSettings model.TLSConfig, timeout time.Duration) (*http.Client, error) {
	transport := &http.Transport{Proxy: http.ProxyFromEnvironment}
	if shared, ok := http.DefaultTransport.(*http.Transport); ok {
		transport = shared.Clone()
	}
	if tlsSettings != (model.TLSConfig{}) {
		cfg, err := tlsConfig(tlsSettings)
		if err != nil {
			return nil, fmt.Errorf("tls: %w", err)
		}
		transport.TLSClientConfig = cfg
	}
	return &http.Client{
		Transport: transport,
		Timeout:   timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 || req.URL.Host != via[0].URL.Host || req.URL.Scheme != via[0].URL.Scheme {
				return http.ErrUseLastResponse
			}
			return nil
		},
	}, nil
}
