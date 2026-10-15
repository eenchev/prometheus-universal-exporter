package config

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The exporter's own Basic Auth credential can be written in the
// configuration, or read from files: web.basic_auth.username_file and
// password_file. With the Helm chart the configuration is a ConfigMap, which
// is no place for a password; a file mounted from a Secret is.
//
// The files are read when the configuration loads, so a missing or empty one
// is refused like any other invalid configuration, and again whenever one
// changes on disk, found by its size and modification time on each request.
// Kubernetes updates a mounted Secret in place, so a rotated password takes
// effect without a restart or a reload. A file that cannot be read any more
// fails the request with 500 rather than falling back to the old credential,
// and is logged.

// validateWebAuth checks the credential settings and that the files can be read.
func validateWebAuth(a *model.ExporterBasicAuth) error {
	if a.Password != "" && a.PasswordFile != "" {
		return errors.New("web.basic_auth sets both password and password_file; use one")
	}
	if a.Username != "" && a.UsernameFile != "" {
		return errors.New("web.basic_auth sets both username and username_file; use one")
	}
	if a.Password == "" && a.PasswordFile == "" {
		return errors.New("web.basic_auth requires a password or password_file when enabled")
	}
	if strings.TrimSpace(a.Username) == "" && a.UsernameFile == "" {
		return errors.New("web.basic_auth requires a username or username_file when enabled")
	}
	// A client sends a username with a colon as a shorter one, which would
	// never be matched.
	if err := fetch.CheckBasicUsername(a.Username); err != nil {
		return fmt.Errorf("web.basic_auth.username %w", err)
	}
	// The digests the requests are compared with are made here, when the
	// configuration loads, and again only when a file changes.
	if _, err := WebAuthDigests(a); err != nil {
		return err
	}
	return nil
}

// WebAuthCredentials returns the username and password, reading the files.
func WebAuthCredentials(a *model.ExporterBasicAuth) (string, string, error) {
	username, password := a.Username, a.Password
	if a.UsernameFile != "" {
		v, err := credentialFiles.read("web.basic_auth.username_file", a.UsernameFile)
		if err != nil {
			return "", "", err
		}
		username = v
	}
	if a.PasswordFile != "" {
		v, err := credentialFiles.read("web.basic_auth.password_file", a.PasswordFile)
		if err != nil {
			return "", "", err
		}
		password = v
	}
	return username, password, nil
}

// WebAuthDigest is the SHA-256 digest of the exporter's own username and of
// its password. A request's credential is compared with these, digested in
// turn, rather than with the credential itself: subtle.ConstantTimeCompare
// takes no time for slices of different lengths, so comparing a presented
// password with the configured one told by its speed whether their lengths
// differed, and so the configured password's length. Digests are all the
// same length, whatever was presented.
type WebAuthDigest struct {
	Username, Password [sha256.Size]byte
}

// WebAuthDigests returns the digests of the username and password, as
// WebAuthCredentials reads them. A file's digest is made when the file is
// read, and one written in the configuration once for its value, so a
// request digests only what it presents.
//
// A username file that holds a colon is refused, as a username written in
// the configuration is (validateWebAuth): no client could present it.
func WebAuthDigests(a *model.ExporterBasicAuth) (WebAuthDigest, error) {
	var digests WebAuthDigest
	var err error
	if a.UsernameFile != "" {
		const key = "web.basic_auth.username_file"
		entry, err := credentialFiles.entry(key, a.UsernameFile)
		if err != nil {
			return WebAuthDigest{}, err
		}
		if err := fetch.CheckBasicUsername(entry.value); err != nil {
			return WebAuthDigest{}, fmt.Errorf("%s %s %w", key, a.UsernameFile, err)
		}
		digests.Username = entry.digest
	} else {
		digests.Username = inlineDigests.of(a.Username)
	}
	if digests.Password, err = credentialDigest("web.basic_auth.password_file", a.PasswordFile, a.Password); err != nil {
		return WebAuthDigest{}, err
	}
	return digests, nil
}

// credentialDigest is the digest of the credential file at path, or of value
// when there is no file.
func credentialDigest(key, path, value string) ([sha256.Size]byte, error) {
	if path != "" {
		entry, err := credentialFiles.entry(key, path)
		return entry.digest, err
	}
	return inlineDigests.of(value), nil
}

// inlineDigestCache keeps the digests of the credentials written in the
// configuration, by value. A reload that changes one adds its digest; the
// cache is emptied when it holds more than a configuration's two, so it
// does not grow with every reload.
type inlineDigestCache struct {
	mu      sync.Mutex
	digests map[string][sha256.Size]byte
}

var inlineDigests = &inlineDigestCache{digests: map[string][sha256.Size]byte{}}

func (c *inlineDigestCache) of(value string) [sha256.Size]byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	if digest, ok := c.digests[value]; ok {
		return digest
	}
	if len(c.digests) >= 8 {
		clear(c.digests)
	}
	digest := sha256.Sum256([]byte(value))
	c.digests[value] = digest
	return digest
}

// credentialFileCache keeps each credential file's content, and its digest,
// until the file's size or modification time changes, so a request costs a
// stat, not a read.
type credentialFileCache struct {
	mu      sync.Mutex
	entries map[string]credentialFileEntry
}

type credentialFileEntry struct {
	stamp, value string
	digest       [sha256.Size]byte
}

var credentialFiles = &credentialFileCache{entries: map[string]credentialFileEntry{}}

func (c *credentialFileCache) read(key, path string) (string, error) {
	entry, err := c.entry(key, path)
	return entry.value, err
}

func (c *credentialFileCache) entry(key, path string) (credentialFileEntry, error) {
	st, err := os.Stat(path)
	if err != nil {
		return credentialFileEntry{}, fmt.Errorf("%s %s cannot be read: %w", key, path, err)
	}
	stamp := strconv.FormatInt(st.ModTime().UnixNano(), 10) + ":" + strconv.FormatInt(st.Size(), 10)
	c.mu.Lock()
	entry, ok := c.entries[path]
	c.mu.Unlock()
	if ok && entry.stamp == stamp {
		return entry, nil
	}
	value, err := fetch.ReadCredentialFile(path)
	if err != nil {
		return credentialFileEntry{}, fmt.Errorf("%s %s cannot be read: %w", key, path, err)
	}
	if value == "" {
		return credentialFileEntry{}, fmt.Errorf("%s %s is empty", key, path)
	}
	entry = credentialFileEntry{stamp: stamp, value: value, digest: sha256.Sum256([]byte(value))}
	c.mu.Lock()
	c.entries[path] = entry
	c.mu.Unlock()
	return entry, nil
}
