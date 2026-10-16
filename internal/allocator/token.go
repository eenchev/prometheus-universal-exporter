package allocator

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
)

// TokenFile is --web.token-file: the bearer token every request but
// /healthz and /ready must carry. It is read again when the file changes —
// its modification time or size, as the exporter's credential files are
// (config's credentialFileCache) — so a rotated Secret takes effect without
// a restart. A file that cannot be read, or holds no token, after a change
// refuses every request until it is fixed, rather than letting every request
// through; that is logged once for as long as the reason stays the same.
type TokenFile struct {
	path   string
	logger *slog.Logger

	mu sync.Mutex
	// stamp is the file's modification time and size when token was read
	// from it; "" when the last read failed, so the next request reads it
	// again.
	stamp string
	token string
	// logged is the failure last logged, "" when the file reads well.
	logged string
}

// NewTokenFile reads the token file at path. A file that cannot be read or
// is empty is an error: the allocator does not start.
func NewTokenFile(path string, logger *slog.Logger) (*TokenFile, error) {
	if logger == nil {
		logger = slog.Default()
	}
	f := &TokenFile{path: path, logger: logger}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, err := f.readLocked(); err != nil {
		return nil, err
	}
	return f, nil
}

// errNoToken is a token file that holds only blanks.
var errNoToken = errors.New("the token file is empty; write the token in it, or leave --web.token-file out")

// Token is the token the file holds now, read again when the file changed;
// an error says it holds none that can be read, and every request is to be
// refused.
func (f *TokenFile) Token() (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st, err := os.Stat(f.path)
	if err == nil && f.stamp != "" && f.stamp == fileStamp(st) {
		return f.token, nil
	}
	token, err := f.readLocked()
	if err != nil {
		if text := err.Error(); text != f.logged {
			f.logged = text
			f.logger.Error("the token file cannot be used; every request but /healthz and /ready is refused until it can", "file", f.path, "error", text)
		}
		return "", err
	}
	if f.logged != "" {
		f.logged = ""
		f.logger.Info("the token file can be used again", "file", f.path)
	}
	return token, nil
}

// readLocked reads the file, keeping its token and stamp when it holds one
// and forgetting both when it does not.
func (f *TokenFile) readLocked() (string, error) {
	f.stamp, f.token = "", ""
	st, err := os.Stat(f.path)
	if err != nil {
		return "", fmt.Errorf("the token file cannot be read: %w", err)
	}
	raw, err := os.ReadFile(f.path)
	if err != nil {
		return "", fmt.Errorf("the token file cannot be read: %w", err)
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return "", errNoToken
	}
	f.stamp, f.token = fileStamp(st), token
	return token, nil
}

// fileStamp is a file's modification time and size.
func fileStamp(st os.FileInfo) string {
	return strconv.FormatInt(st.ModTime().UnixNano(), 10) + ":" + strconv.FormatInt(st.Size(), 10)
}
