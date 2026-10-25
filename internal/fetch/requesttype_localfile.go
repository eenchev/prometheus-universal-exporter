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
	"strings"
	"sync"
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
		Name:         RequestTypeLocalFile,
		Fields:       []string{"root", "path", "max_age", "max_response_bytes", "files", "max_files", "max_total_bytes"},
		Overrides:    []string{"path", "timeout", PathParamPrefix},
		TargetFields: []string{"path", "timeout"},
		Validate:     validateLocalFileRequest,
		Fetch:        fetchLocalFile,
		UnusedParams: func(c *model.Collector, _ RequestOverrides, unused string) error {
			return unusedParamsIn(c, unused, fmt.Sprintf("its request.path (%q) or its label values", c.Request.Path))
		},
		// root, written with something other than "" (SchemaWrittenKey).
		SchemaRule:     func() map[string]any { return SchemaWrittenKey("root") },
		OptionalTarget: true,
		FileExtensions: true,
		TargetHint:     func(*model.Collector) string { return "a file or directory under its root" },
		CheckTarget: func(c *model.Collector, target string, _ bool) error {
			_, err := localFileTargetDir(c, target)
			return err
		},
		Label:   localFileLabel,
		Method:  func(*model.Collector, RequestOverrides) string { return localFileMethod },
		Display: func(target string) string { return target },
		Stage:   "file",
		CheckOverride: func(c *model.Collector, key string) error {
			// A param_ parameter can still fill a label value of the
			// collector (labelparams.go): when its labels hold a
			// placeholder, which parameters are used is for the check of
			// the placeholders to say (CheckPathParams).
			if readsDirectory(c) && (key == "path" || strings.HasPrefix(key, PathParamPrefix) && c.LabelParams == nil) {
				return fmt.Errorf("collector %q reads every file of a directory that request.files matches, so there is no file for it to name; name a directory with the target instead", c.Name)
			}
			return nil
		},
	})
}

func validateLocalFileRequest(x *model.Collector) error {
	if err := validateFileRequest(x, RequestTypeLocalFile, true); err != nil {
		return err
	}
	return validateLocalDirectory(x)
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
	return fileUnderRoot(dir, c, overrides, bind, "no file to read: set request.path, or give the probe a target naming a file under request.root")
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
	return readSourceFile(rootSource{root}, rootDir, name, limit)
}

// readOpenedFile opens name in root and reads it (readSourceOpened).
func readOpenedFile(root *os.Root, name, full string, limit int64) ([]byte, fs.FileInfo, error) {
	return readSourceOpened(rootSource{root}, name, full, limit)
}

// rootSource reads files from an opened root on the exporter's own
// filesystem, which the operating system confines every name to.
type rootSource struct{ root *os.Root }

func (s rootSource) Stat(name string) (fs.FileInfo, error) { return s.root.Stat(name) }

func (s rootSource) OpenFile(name string) (sourceFile, error) {
	// Non-blocking, so a named pipe swapped in after the check of its kind
	// (readSourceFile) cannot hold the open; for a regular file the flag
	// changes nothing.
	f, err := s.root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	return f, nil
}

func (s rootSource) OpenDir(name string) (sourceDir, error) {
	d, err := s.root.Open(name)
	if err != nil {
		return nil, err
	}
	return d, nil
}

// fetchLocalDirectory reads the directory a probe of a collector with
// request.files names (localfile_directory.go).
func fetchLocalDirectory(ctx context.Context, target string, c *model.Collector, overrides RequestOverrides) (*HTTPResponse, error) {
	start := time.Now()
	dir, err := localFileTargetDir(c, target)
	if err != nil {
		return nil, err
	}
	if overrides.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, overrides.Timeout)
		defer cancel()
	}
	full := filepath.Join(c.Request.Root, dir)
	// As for one file, the reading goes on in its own goroutine, and a read the
	// probe stops waiting for is one of the collector's abandoned reads until
	// the filesystem answers.
	started, err := localFileReads.start(c.Name)
	if err != nil {
		return nil, err
	}
	progress := &directoryProgress{}
	done := make(chan error, 1)
	go func() {
		defer started.done()
		done <- readLocalDirectory(c, dir, progress)
	}()
	read, err := awaitDirectory(ctx, full, progress, done, started.abandon)
	if err != nil {
		return nil, err
	}
	for i := range read.Files {
		if f := read.Files[i].Response; f != nil {
			f.Target = target
		}
	}
	return &HTTPResponse{StatusCode: 200, NoStatus: true, Target: target, Collector: c.Name, Duration: time.Since(start), Directory: read}, nil
}

// readLocalDirectory reads the matching files of root/dir into progress
// (readSourceDirectory), confined to root by the operating system.
func readLocalDirectory(c *model.Collector, dir string, progress *directoryProgress) error {
	root, err := os.OpenRoot(c.Request.Root)
	if err != nil {
		return fmt.Errorf("opening request.root: %w", err)
	}
	defer func() { _ = root.Close() }()
	return readSourceDirectory(rootSource{root}, c, dir, progress)
}
