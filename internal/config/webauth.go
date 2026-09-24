package config

import (
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
	if _, _, err := WebAuthCredentials(a); err != nil {
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

// credentialFileCache keeps each credential file's content until the file's
// size or modification time changes, so a request costs a stat, not a read.
type credentialFileCache struct {
	mu      sync.Mutex
	entries map[string]credentialFileEntry
}

type credentialFileEntry struct {
	stamp, value string
}

var credentialFiles = &credentialFileCache{entries: map[string]credentialFileEntry{}}

func (c *credentialFileCache) read(key, path string) (string, error) {
	st, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("%s %s cannot be read: %w", key, path, err)
	}
	stamp := strconv.FormatInt(st.ModTime().UnixNano(), 10) + ":" + strconv.FormatInt(st.Size(), 10)
	c.mu.Lock()
	entry, ok := c.entries[path]
	c.mu.Unlock()
	if ok && entry.stamp == stamp {
		return entry.value, nil
	}
	value, err := fetch.ReadCredentialFile(path)
	if err != nil {
		return "", fmt.Errorf("%s %s cannot be read: %w", key, path, err)
	}
	if value == "" {
		return "", fmt.Errorf("%s %s is empty", key, path)
	}
	c.mu.Lock()
	c.entries[path] = credentialFileEntry{stamp: stamp, value: value}
	c.mu.Unlock()
	return value, nil
}
