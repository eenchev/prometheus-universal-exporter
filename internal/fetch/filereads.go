//go:build !select_request_types || request_type_localfile || request_type_s3file || request_type_sshfile

package fetch

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// localfile, sshfile and s3file read files the same way, from the exporter's
// own filesystem, from a remote host's over SFTP and from a bucket over the
// S3 API: confined to
// request.root, only regular files, read again when they change under the
// read, refused past the response limit or max_age, and presented to the
// pipeline as a response with no status. What they read from is a fileSource:
// an os.Root for localfile (rootSource), an SFTP session for sshfile, a
// bucket for s3file (s3source.go). A
// directory is read by the same code too (localfile_directory.go).

// fileSource is a tree of files that every read is confined to. Names are
// relative to its root.
type fileSource interface {
	// Stat describes name, following a symbolic link that stays inside the
	// root.
	Stat(name string) (fs.FileInfo, error)
	// OpenFile opens name to be read, without waiting on a named pipe.
	OpenFile(name string) (sourceFile, error)
	// OpenDir opens the directory name to be listed.
	OpenDir(name string) (sourceDir, error)
}

// sourceFile is a file a fileSource opened.
type sourceFile interface {
	io.Reader
	Stat() (fs.FileInfo, error)
	Close() error
}

// sourceDir is a directory a fileSource opened, listed n entries at a time
// as os.File.ReadDir lists them.
type sourceDir interface {
	Stat() (fs.FileInfo, error)
	ReadDir(n int) ([]fs.DirEntry, error)
	Close() error
}

// localFileMethod is the http_method label of a file read, so a file's verbose
// series are told apart from the per-collector ones, which carry none.
const localFileMethod = "READ"

// errLeavesRoot is what a fileSource that does not confine names itself, as
// os.Root does, answers for a name that a symbolic link leads outside the
// root (sshfile's, sshsource.go).
var errLeavesRoot = errors.New("leads outside request.root through a symbolic link")

// afterLocalFileRead, when set, runs between reading a file and checking
// whether it changed meanwhile. Only tests set it, to change a file under a
// read or to hold one up.
var afterLocalFileRead atomic.Pointer[func(string)]

// localFileAttempts is how often a file that keeps changing under the read is
// tried before the scrape gives up on it.
const localFileAttempts = 2

// localFileResponse presents a file read as a response: no status for rules
// (the status gauge reads 200), and Content-Type from the extension,
// Content-Length and Last-Modified.
func localFileResponse(name string, body []byte, info fs.FileInfo, c *model.Collector) *HTTPResponse {
	headers := http.Header{}
	if contentType := localFileContentType(name); contentType != "" {
		headers.Set("Content-Type", contentType)
	}
	headers.Set("Content-Length", strconv.Itoa(len(body)))
	headers.Set("Last-Modified", info.ModTime().UTC().Format(http.TimeFormat))
	return &HTTPResponse{StatusCode: http.StatusOK, NoStatus: true, Headers: headers, Body: body, Collector: c.Name}
}

// readSourceFile reads name in src, whose root is rootDir, as readLocalFile
// does: only a regular file, read again when its size or modification time
// moved while it was read.
func readSourceFile(src fileSource, rootDir, name string, limit int64) ([]byte, fs.FileInfo, error) {
	full := filepath.Join(rootDir, name)
	for attempt := 1; ; attempt++ {
		before, err := src.Stat(name)
		if err != nil {
			return nil, nil, localFileError(full, err)
		}
		if !before.Mode().IsRegular() {
			return nil, nil, fmt.Errorf("%s is not a regular file (%s)", full, fileKind(before.Mode()))
		}
		body, after, err := readSourceOpened(src, name, full, limit)
		if err != nil {
			return nil, nil, err
		}
		if after.Size() == before.Size() && after.ModTime().Equal(before.ModTime()) {
			return body, after, nil
		}
		if attempt == localFileAttempts {
			return nil, nil, fmt.Errorf("%s changed while it was read, %d times; write it to a temporary file in the same directory and rename it into place", full, localFileAttempts)
		}
	}
}

// readSourceOpened opens name in src and reads it, refusing what is not a
// regular file once it is open, and a body past limit.
func readSourceOpened(src fileSource, name, full string, limit int64) ([]byte, fs.FileInfo, error) {
	f, err := src.OpenFile(name)
	if err != nil {
		return nil, nil, localFileError(full, err)
	}
	defer func() { _ = f.Close() }()
	opened, err := f.Stat()
	if err != nil {
		return nil, nil, localFileError(full, err)
	}
	if !opened.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("%s is not a regular file (%s)", full, fileKind(opened.Mode()))
	}
	// The file's size is how long a buffer to read it into, and no more
	// than that: a file that grew or shrank since is read to its end, and a
	// file that gives no size, as those of /proc do, as a body of unknown
	// length.
	body, err := readBody(f, limit, opened.Size())
	if err != nil {
		return nil, nil, localFileError(full, err)
	}
	if int64(len(body)) > limit {
		return nil, nil, model.MarkError(fmt.Errorf("file %s: response size exceeds limit %d", full, limit), model.ErrLimitExceeded)
	}
	if hook := afterLocalFileRead.Load(); hook != nil {
		(*hook)(full)
	}
	after, err := f.Stat()
	if err != nil {
		return nil, nil, localFileError(full, err)
	}
	return body, after, nil
}

func localFileError(full string, err error) error {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("file %s does not exist", full)
	case errors.Is(err, fs.ErrPermission):
		return fmt.Errorf("file %s cannot be read by the exporter: permission denied", full)
	case strings.Contains(err.Error(), "escapes from parent"), errors.Is(err, errLeavesRoot):
		return fmt.Errorf("file %s leads outside request.root through a symbolic link", full)
	}
	return fmt.Errorf("reading %s: %w", full, err)
}

func fileKind(mode fs.FileMode) string {
	switch {
	case mode.IsDir():
		return "a directory"
	case mode&fs.ModeNamedPipe != 0:
		return "a named pipe"
	case mode&fs.ModeSocket != 0:
		return "a socket"
	case mode&fs.ModeDevice != 0:
		return "a device"
	}
	return "not a regular file"
}
