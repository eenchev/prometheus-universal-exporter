//go:build !select_request_types || request_type_localfile

package fetch

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
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
// The file is root/target/path. A probe or a static target may name the
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
//     The reads left running on a hung filesystem are capped per collector;
//     reads whose probe is still waiting for them are not counted, so probes
//     of a healthy filesystem are never refused for being many.

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

// afterLocalFileRead, when set, runs between reading a file and checking
// whether it changed meanwhile. Only tests set it, to change a file under a
// read or to hold one up.
var afterLocalFileRead atomic.Pointer[func(string)]

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

// localFileTargetDir turns a target into a path relative to root. A target
// may be relative to root, an absolute path inside it, or a file:// URL of
// one; it may be empty. A URL's path is percent-decoded, as a URL is read
// everywhere else: file:///srv/a%20b.prom is the file "a b.prom". The two
// plain forms are paths, not URLs, and are taken as written.
func localFileTargetDir(c *model.Collector, target string) (string, error) {
	target = strings.TrimSpace(target)
	if rest, ok := strings.CutPrefix(target, "file://"); ok {
		if !strings.HasPrefix(rest, "/") {
			return "", fmt.Errorf("target %q is not a file:// URL of an absolute path", target)
		}
		decoded, err := url.PathUnescape(rest)
		if err != nil {
			return "", fmt.Errorf("target %q is not a valid file:// URL: %w; write a %% that is part of a file's name as %%25", target, err)
		}
		target = decoded
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

// localFileMaxAbandonedReads is how many reads of one collector may be left
// running by probes that have given up on them. A read cannot be cancelled,
// so on a filesystem that has stopped answering every probe would leave one
// behind; identical probes share a read (exporter/probeflight.go), so
// reaching the cap takes different files. A read whose probe is still waiting
// for it is not counted: however many probes read a healthy filesystem at
// once, none is refused. The reads left behind can therefore pass the cap by
// the probes that were waiting when the filesystem stopped, which
// limits.max_concurrent_probes bounds, and no read is started after that.
const localFileMaxAbandonedReads = 4

// abandonedReads counts, per collector, the reads still running whose probe
// has given up on them.
type abandonedReads struct {
	mu    sync.Mutex
	count map[string]int
}

var localFileReads = &abandonedReads{count: map[string]int{}}

// startedRead is one read as abandonedReads follows it, under its lock.
type startedRead struct {
	reads               *abandonedReads
	collector           string
	abandoned, returned bool
}

// start admits a read of a collector, or refuses it at once when the
// collector already has the cap of abandoned reads that have not returned.
// The probe calls abandon when it stops waiting for the read, and the read
// calls done when it returns, whichever happens first.
func (a *abandonedReads) start(collector string) (*startedRead, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if n := a.count[collector]; n >= localFileMaxAbandonedReads {
		return nil, fmt.Errorf("collector %q already has %d file reads that have not returned; the filesystem under request.root is not answering, so no further read is started until one does", collector, n)
	}
	return &startedRead{reads: a, collector: collector}, nil
}

// abandon counts the read as left behind, unless it has returned already.
func (r *startedRead) abandon() {
	r.reads.mu.Lock()
	defer r.reads.mu.Unlock()
	if r.returned || r.abandoned {
		return
	}
	r.abandoned = true
	r.reads.count[r.collector]++
}

// done records that the read returned, which frees its place when its probe
// had abandoned it.
func (r *startedRead) done() {
	r.reads.mu.Lock()
	defer r.reads.mu.Unlock()
	if r.returned {
		return
	}
	r.returned = true
	if r.abandoned {
		r.reads.count[r.collector]--
		if r.reads.count[r.collector] == 0 {
			delete(r.reads.count, r.collector)
		}
	}
}

// abandoned reports how many abandoned reads of a collector have not
// returned, for tests.
func (a *abandonedReads) abandoned(collector string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.count[collector]
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
	// goes on. The reads left behind this way are capped per collector, so a
	// stuck filesystem costs a few goroutines rather than one per scrape.
	started, err := localFileReads.start(c.Name)
	if err != nil {
		return nil, err
	}
	done := make(chan localFileRead, 1)
	go func() {
		defer started.done()
		body, info, err := readLocalFile(c.Request.Root, file, responseLimit(c))
		done <- localFileRead{body: body, info: info, err: err}
	}()
	var read localFileRead
	select {
	case <-ctx.Done():
		started.abandon()
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
			return model.Errorf("file %s was last modified %s ago, longer than request.max_age %s; whatever writes it has stopped", full, model.Elapsed(age.Round(time.Second)), maxAge)
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
	return &HTTPResponse{StatusCode: http.StatusOK, NoStatus: true, Headers: headers, Body: body, Collector: c.Name}
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

// localFileContentType lets decoder.type auto pick the decoder a file's
// extension implies. .prom is the textfile collector's Prometheus text
// format, and .graphite and .carbon carbon's plaintext lines, which the
// graphite decoder reads (decode/graphite.go); text/x-graphite is the
// exporter's own name for them, since they have no registered type. Anything
// else is detected from its content.
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
	}
	return ""
}
