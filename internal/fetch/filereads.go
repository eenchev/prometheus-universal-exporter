//go:build !select_request_types || request_type_localfile || request_type_sshfile

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
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// localfile and sshfile read files the same way, one from the exporter's own
// filesystem and the other from a remote host's over SFTP: confined to
// request.root, only regular files, read again when they change under the
// read, refused past the response limit or max_age, and presented to the
// pipeline as a response with no status. What they read from is a fileSource:
// an os.Root for localfile (rootSource), an SFTP session for sshfile. A
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

// validateFileRequest holds the rules localfile and sshfile share: root is
// required, absolute, not localfile's filesystem root, and stored cleaned;
// sshfile's may be /, which on a host is often the directory of an account
// confined to it (a chroot), and is the host's alone in any case; max_age
// is not negative; and path, with its placeholders, names a file under root.
// typeName is the type the message for a missing root names.
func validateFileRequest(x *model.Collector, typeName string) error {
	root := strings.TrimSpace(x.Request.Root)
	if root == "" {
		return fmt.Errorf("collector %q request.root is required for request.type %s: the directory the collector may read files under", x.Name, typeName)
	}
	if !filepath.IsAbs(root) {
		return fmt.Errorf("collector %q request.root %q must be an absolute path", x.Name, root)
	}
	root = filepath.Clean(root)
	if root == string(filepath.Separator) && typeName != RequestTypeSSHFile {
		return fmt.Errorf("collector %q request.root must not be the filesystem root; name the narrowest directory that holds the files", x.Name)
	}
	x.Request.Root = root
	if x.Request.MaxAge < 0 {
		return fmt.Errorf("collector %q request.max_age must not be negative", x.Name)
	}
	if HasPathParams(x.Request.Path) {
		placeholders, err := parsePathParams(x.Request.Path)
		if err != nil {
			return fmt.Errorf("collector %q: %w", x.Name, err)
		}
		// A default no probe could read a file with (resolveLocalFile) is
		// refused now, rather than by every probe that leaves the parameter
		// out.
		if err := checkPathParamDefaults(x.Name, placeholders, checkLocalFileParamValue); err != nil {
			return err
		}
	}
	if err := checkLocalFileName("request.path", x.Request.Path); err != nil {
		return fmt.Errorf("collector %q %w", x.Name, err)
	}
	return nil
}

// checkLocalFileName requires a path under root: relative, and without ".."
// reaching above it. Empty is allowed; the target may name the file.
func checkLocalFileName(what, name string) error {
	if name == "" {
		return nil
	}
	if strings.ContainsRune(name, 0) {
		return fmt.Errorf("%s must not contain a NUL byte", what)
	}
	if filepath.IsAbs(name) || strings.HasPrefix(name, "/") {
		return fmt.Errorf("%s %q must be relative to request.root", what, name)
	}
	if !filepath.IsLocal(filepath.Clean(name)) {
		return fmt.Errorf("%s %q leads outside request.root", what, name)
	}
	return nil
}

// checkLocalFileElement requires of a path parameter's value that it is one
// path element: it may not add directories to the path it is written into.
func checkLocalFileElement(value string) error {
	if strings.ContainsAny(value, `/\`) || strings.ContainsRune(value, 0) {
		return fmt.Errorf("path parameter value %q must be a single file or directory name, without / or \\", value)
	}
	return nil
}

// checkLocalFileParamValue is everything a path parameter's value is held
// to before a file is read with it, the two steps of resolveLocalFile in
// one: what any path parameter may not be (bindPathParams), and one path
// element. An empty value is not refused: the placeholder then adds nothing
// to the path, as a probe that leaves it out of a path with an empty default
// has it.
func checkLocalFileParamValue(name, value string) error {
	if err := checkPathParamValue(name, value); err != nil {
		return err
	}
	return checkLocalFileElement(value)
}

// fileUnderRoot is the file a scrape reads, relative to root: request.path,
// or the path probe parameter, with its placeholders bound when bind is set,
// joined onto dir. noFile is the error when that names no file.
func fileUnderRoot(dir string, c *model.Collector, overrides RequestOverrides, bind bool, noFile string) (string, error) {
	name := c.Request.Path
	if overrides.PathSet {
		name = overrides.Path
		if err := checkLocalFileName("path", name); err != nil {
			return "", err
		}
	} else if bind && HasPathParams(name) {
		bound, values, err := bindPathParams(name, overrides.Params)
		if err != nil {
			return "", err
		}
		for i, value := range values {
			// "." and ".." are already refused by bindPathParams.
			if err := checkLocalFileElement(value); err != nil {
				return "", err
			}
			bound = strings.Replace(bound, pathToken(i), value, 1)
		}
		name = bound
	}
	file := filepath.Join(dir, name)
	if file == "." || file == "" {
		return "", errors.New(noFile)
	}
	if !filepath.IsLocal(file) {
		return "", fmt.Errorf("file %q leads outside request.root %q", file, c.Request.Root)
	}
	return file, nil
}

// checkMaxAge refuses a file older than request.max_age.
func checkMaxAge(c *model.Collector, full string, modified time.Time) error {
	if maxAge := time.Duration(c.Request.MaxAge); maxAge > 0 {
		if age := time.Since(modified); age > maxAge {
			return model.Errorf("file %s was last modified %s ago, longer than request.max_age %s; whatever writes it has stopped", full, model.Elapsed(age.Round(time.Second)), maxAge)
		}
	}
	return nil
}

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

// localFileContentType lets decoder.type auto pick the decoder a file's
// extension implies. .prom is the textfile collector's Prometheus text
// format, and .graphite and .carbon carbon's plaintext lines, which the
// graphite decoder reads (decode/graphite.go); text/x-graphite is the
// exporter's own name for them, since they have no registered type. .ndjson
// and .jsonl are JSON Lines, and .toml TOML; .ini and logfmt have no type
// that names them, and are read by an explicit decoder.type. Anything else
// is detected from its content.
func localFileContentType(name string) string {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".json":
		return "application/json"
	case ".yaml", ".yml":
		return "application/yaml"
	case ".xml":
		return "application/xml"
	case ".csv":
		return "text/csv"
	case ".html", ".htm":
		return "text/html"
	case ".prom":
		return "text/plain; version=0.0.4"
	case ".graphite", ".carbon":
		return GraphiteContentType
	case ".ndjson", ".jsonl":
		return "application/x-ndjson"
	case ".toml":
		return "application/toml"
	}
	return ""
}
