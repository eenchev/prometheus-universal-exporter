//go:build !select_request_types || request_type_localfile

package fetch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The localfile request type reads a file from the exporter's own filesystem:
// a metrics file a batch job leaves behind, a status file an appliance
// writes, a textfile collector directory shared with node_exporter.
//
//	request:
//	  type: localfile
//	  root: /var/lib/metrics      # required: the only directory it may read
//	  path: app.prom              # optional: the file, relative to root
//	  max_age: 10m                # optional: refuse a file older than this
//
// The file is root/target/path. A probe or a scheduled target may name the
// file, or a directory under root, as its target, or leave target out when
// request.path names the file. Everything after the read — decoding,
// transforms, limits, caching, coalescing — is shared with http.
//
// It follows what node_exporter's textfile collector learned:
//
//   - One directory, chosen by the operator. Every read is confined to root
//     with os.Root, so neither ".." nor a symbolic link can reach outside it,
//     whatever a probe asks for. root "/" is refused: name the narrowest
//     directory that holds the files.
//   - Only regular files. A directory, device, socket or named pipe is
//     refused before it is opened, and it is opened non-blocking, so a pipe
//     swapped in can never hang a scrape.
//   - No torn reads. A file that changes while it is read — a writer that
//     rewrites it in place — is read again, and refused if it changes again.
//     Writers should write a temporary file in the same directory and rename
//     it into place, which is atomic.
//   - Staleness is visible. A file whose writer has stopped keeps its last
//     values forever; max_age turns that into a failed scrape, and the
//     modification time reaches transforms as the Last-Modified header.
//   - Bounded. The read stops at the collector's response limit, and a probe
//     returns when its timeout or context ends even if the filesystem hangs.
//     The reads left running on a hung filesystem are capped per collector.

func init() {
	registerRequestType(&RequestType{
		Name:           RequestTypeLocalFile,
		Fields:         []string{"root", "path", "max_age", "max_response_bytes", "files", "max_files", "max_total_bytes"},
		Overrides:      []string{"path", "timeout", PathParamPrefix},
		TargetFields:   []string{"path", "timeout"},
		Validate:       validateLocalFileRequest,
		Fetch:          fetchLocalFile,
		OptionalTarget: true,
		CheckTarget: func(c *model.Collector, target string, _ bool) error {
			_, err := localFileTargetDir(c, target)
			return err
		},
		Label:   localFileLabel,
		Method:  func(*model.Collector, RequestOverrides) string { return localFileMethod },
		Display: func(target string) string { return target },
		Stage:   "file",
		CheckOverride: func(c *model.Collector, key string) error {
			if readsDirectory(c) && (key == "path" || strings.HasPrefix(key, PathParamPrefix)) {
				return fmt.Errorf("collector %q reads every file of a directory that request.files matches, so there is no file for it to name; name a directory with the target instead", c.Name)
			}
			return nil
		},
	})
}

// localFileMethod is the http_method label of a file read, so a file's verbose
// series are told apart from the per-collector ones, which carry none.
const localFileMethod = "READ"

// AfterLocalFileRead, when set, runs between reading a file and checking
// whether it changed meanwhile. Only tests set it, to change a file under a
// read or to hold one up.
var AfterLocalFileRead atomic.Pointer[func(string)]

// localFileAttempts is how often a file that keeps changing under the read is
// tried before the scrape gives up on it.
const localFileAttempts = 2

func validateLocalFileRequest(x *model.Collector) error {
	root := strings.TrimSpace(x.Request.Root)
	if root == "" {
		return fmt.Errorf("collector %q request.root is required for request.type localfile: the directory the collector may read files under", x.Name)
	}
	if !filepath.IsAbs(root) {
		return fmt.Errorf("collector %q request.root %q must be an absolute path", x.Name, root)
	}
	root = filepath.Clean(root)
	if root == string(filepath.Separator) {
		return fmt.Errorf("collector %q request.root must not be the filesystem root; name the narrowest directory that holds the files", x.Name)
	}
	x.Request.Root = root
	if x.Request.MaxAge < 0 {
		return fmt.Errorf("collector %q request.max_age must not be negative", x.Name)
	}
	if x.Request.MaxResponseBytes < 0 {
		return fmt.Errorf("collector %q request.max_response_bytes must not be negative", x.Name)
	}
	if HasPathParams(x.Request.Path) {
		if _, err := parsePathParams(x.Request.Path); err != nil {
			return fmt.Errorf("collector %q: %w", x.Name, err)
		}
	}
	if err := checkLocalFileName("request.path", x.Request.Path); err != nil {
		return fmt.Errorf("collector %q %w", x.Name, err)
	}
	return validateLocalDirectory(x)
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

// localFileTargetDir turns a target into a path relative to root. A target
// may be relative to root, an absolute path inside it, or a file:// URL of
// one; it may be empty.
func localFileTargetDir(c *model.Collector, target string) (string, error) {
	target = strings.TrimSpace(target)
	if rest, ok := strings.CutPrefix(target, "file://"); ok {
		if !strings.HasPrefix(rest, "/") {
			return "", fmt.Errorf("target %q is not a file:// URL of an absolute path", target)
		}
		target = rest
	}
	if target == "" {
		return "", nil
	}
	if strings.ContainsRune(target, 0) {
		return "", errors.New("target must not contain a NUL byte")
	}
	if filepath.IsAbs(target) || strings.HasPrefix(target, "/") {
		rel, err := filepath.Rel(c.Request.Root, filepath.Clean(target))
		if err != nil || !filepath.IsLocal(rel) && rel != "." {
			return "", fmt.Errorf("target %q is outside request.root %q", target, c.Request.Root)
		}
		if rel == "." {
			return "", nil
		}
		return rel, nil
	}
	if !filepath.IsLocal(filepath.Clean(target)) {
		return "", fmt.Errorf("target %q leads outside request.root %q", target, c.Request.Root)
	}
	return filepath.Clean(target), nil
}

// resolveLocalFile is the file a scrape reads, relative to root. With bind
// false, path parameters stay as their placeholders, for the url label.
func resolveLocalFile(target string, c *model.Collector, overrides RequestOverrides, bind bool) (string, error) {
	dir, err := localFileTargetDir(c, target)
	if err != nil {
		return "", err
	}
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
			// A value is one path element: it may not add directories, and
			// "." and ".." are already refused by bindPathParams.
			if strings.ContainsAny(value, `/\`) || strings.ContainsRune(value, 0) {
				return "", fmt.Errorf("path parameter value %q must be a single file or directory name, without / or \\", value)
			}
			bound = strings.Replace(bound, pathToken(i), value, 1)
		}
		name = bound
	}
	file := filepath.Join(dir, name)
	if file == "." || file == "" {
		return "", errors.New("no file to read: set request.path, or give the probe a target naming a file under request.root")
	}
	if !filepath.IsLocal(file) {
		return "", fmt.Errorf("file %q leads outside request.root %q", file, c.Request.Root)
	}
	return file, nil
}

// localFileLabel is the url label of a file read: its file:// URL, with path
// parameters as their placeholders.
func localFileLabel(target string, c *model.Collector, overrides RequestOverrides) (string, error) {
	if readsDirectory(c) {
		dir, err := localFileTargetDir(c, target)
		if err != nil {
			return "", err
		}
		return "file://" + filepath.ToSlash(filepath.Join(c.Request.Root, dir)) + "/", nil
	}
	file, err := resolveLocalFile(target, c, overrides, false)
	if err != nil {
		return "", err
	}
	return "file://" + filepath.ToSlash(filepath.Join(c.Request.Root, file)), nil
}

// LocalFileMaxPendingReads is how many reads of one collector may be in
// progress at once, including reads whose probe has already given up on them.
// Identical probes share a read (exporter/probeflight.go), so reaching it takes
// different files on a filesystem that has stopped answering.
const LocalFileMaxPendingReads = 4

// pendingReads counts the reads in progress per collector.
type pendingReads struct {
	mu    sync.Mutex
	count map[string]int
}

var LocalFileReads = &pendingReads{count: map[string]int{}}

// acquire takes a read slot for a collector, or refuses at once when every
// slot is held by a read that has not returned. release frees it, and is
// called when the read returns, not when the probe does.
func (p *pendingReads) acquire(collector string) (release func(), err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.count[collector] >= LocalFileMaxPendingReads {
		return nil, fmt.Errorf("collector %q already has %d file reads that have not returned; the filesystem under request.root is not answering, so no further read is started until one does", collector, LocalFileMaxPendingReads)
	}
	p.count[collector]++
	var once sync.Once
	return func() {
		once.Do(func() {
			p.mu.Lock()
			defer p.mu.Unlock()
			p.count[collector]--
			if p.count[collector] == 0 {
				delete(p.count, collector)
			}
		})
	}, nil
}

// Pending reports how many reads of a collector are in progress, for tests.
func (p *pendingReads) Pending(collector string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.count[collector]
}

type localFileRead struct {
	body []byte
	info fs.FileInfo
	err  error
}

func fetchLocalFile(ctx context.Context, target string, c *model.Collector, overrides RequestOverrides, _ http.Header) (*HTTPResponse, error) {
	if readsDirectory(c) {
		return fetchLocalDirectory(ctx, target, c, overrides)
	}
	start := time.Now()
	file, err := resolveLocalFile(target, c, overrides, true)
	if err != nil {
		return nil, err
	}
	if overrides.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, overrides.Timeout)
		defer cancel()
	}
	full := filepath.Join(c.Request.Root, file)
	// A read cannot be cancelled, and a hung network filesystem would hold it
	// for good; the scrape stops waiting when its context ends, but the read
	// goes on. The reads still going on are capped per collector, so a stuck
	// filesystem costs a few goroutines rather than one per scrape.
	release, err := LocalFileReads.acquire(c.Name)
	if err != nil {
		return nil, err
	}
	done := make(chan localFileRead, 1)
	go func() {
		defer release()
		body, info, err := readLocalFile(c.Request.Root, file, ResponseLimit(c))
		done <- localFileRead{body: body, info: info, err: err}
	}()
	var read localFileRead
	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("reading %s: %w", full, ctx.Err())
	case read = <-done:
	}
	if read.err != nil {
		return nil, read.err
	}
	if err := checkMaxAge(c, full, read.info.ModTime()); err != nil {
		return nil, err
	}
	resp := localFileResponse(file, read.body, read.info, c)
	resp.Target, resp.Duration = target, time.Since(start)
	return resp, nil
}

// checkMaxAge refuses a file older than request.max_age.
func checkMaxAge(c *model.Collector, full string, modified time.Time) error {
	if maxAge := time.Duration(c.Request.MaxAge); maxAge > 0 {
		if age := time.Since(modified); age > maxAge {
			return fmt.Errorf("file %s was last modified %s ago, longer than request.max_age %s; whatever writes it has stopped", full, age.Round(time.Second), maxAge)
		}
	}
	return nil
}

// localFileResponse presents a file read as a response: status 200, and
// Content-Type from the extension, Content-Length and Last-Modified.
func localFileResponse(name string, body []byte, info fs.FileInfo, c *model.Collector) *HTTPResponse {
	headers := http.Header{}
	if contentType := localFileContentType(name); contentType != "" {
		headers.Set("Content-Type", contentType)
	}
	headers.Set("Content-Length", strconv.Itoa(len(body)))
	headers.Set("Last-Modified", info.ModTime().UTC().Format(http.TimeFormat))
	return &HTTPResponse{StatusCode: http.StatusOK, Headers: headers, Body: body, Collector: c.Name}
}

// readLocalFile reads root/name, confined to root, and only a regular file. A
// file whose size or modification time moved while it was read is read again.
func readLocalFile(rootDir, name string, limit int64) ([]byte, fs.FileInfo, error) {
	root, err := os.OpenRoot(rootDir)
	if err != nil {
		return nil, nil, fmt.Errorf("opening request.root: %w", err)
	}
	defer func() { _ = root.Close() }()
	return readRootFile(root, rootDir, name, limit)
}

// readRootFile reads name in an opened root, as readLocalFile does.
func readRootFile(root *os.Root, rootDir, name string, limit int64) ([]byte, fs.FileInfo, error) {
	full := filepath.Join(rootDir, name)
	for attempt := 1; ; attempt++ {
		before, err := root.Stat(name)
		if err != nil {
			return nil, nil, localFileError(full, err)
		}
		if !before.Mode().IsRegular() {
			return nil, nil, fmt.Errorf("%s is not a regular file (%s)", full, fileKind(before.Mode()))
		}
		body, after, err := readOpenedFile(root, name, full, limit)
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

func readOpenedFile(root *os.Root, name, full string, limit int64) ([]byte, fs.FileInfo, error) {
	// Non-blocking, so a named pipe swapped in after the check above cannot
	// hold the open; for a regular file the flag changes nothing.
	f, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
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
	body, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, nil, localFileError(full, err)
	}
	if int64(len(body)) > limit {
		return nil, nil, model.MarkError(fmt.Errorf("file %s: response size exceeds limit %d", full, limit), model.ErrLimitExceeded)
	}
	if hook := AfterLocalFileRead.Load(); hook != nil {
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
	case strings.Contains(err.Error(), "escapes from parent"):
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

// localFileContentType lets response.format auto pick the decoder a file's
// extension implies. .prom is the textfile collector's Prometheus text
// format. Anything else is detected from its content.
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
	}
	return ""
}
