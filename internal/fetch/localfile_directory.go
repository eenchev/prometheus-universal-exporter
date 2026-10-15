//go:build !select_request_types || request_type_localfile || request_type_sshfile

package fetch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A localfile collector with request.files reads a whole directory, the way
// node_exporter's textfile collector does:
//
//	request:
//	  type: localfile
//	  root: /var/lib/node_exporter/textfile_collector
//	  files: ["*.prom"]
//
// Every file directly in the directory — root, or the directory under it the
// target names — whose name matches one of the patterns is read, decoded and
// transformed on its own, and its series get a file label naming it. A file
// that cannot be read, decoded or transformed fails alone: its series are
// left out, localfile_scrape_error{file} reads 1 for it, the reason is
// logged, and every other file's series are answered as usual. That is the
// point of reading a directory this way rather than concatenating it: one
// broken file must not sink the rest.
//
// Reading a directory is bounded, because a directory is easy to fill:
//
//   - request.max_files, 100 by default, is the most files one scrape reads.
//     Beyond it, files are taken in name order and the rest skipped, counted
//     in localfile_files_skipped and logged.
//   - The collector's response limit (max_response_bytes, 10 MiB by default)
//     applies to each file, checked from its size before it is opened, so a
//     1 GiB file is refused without being read.
//   - request.max_total_bytes, 64 MiB by default, is the most one scrape reads
//     across every file. A file that would go past it is refused.
//
// A refused file is a failed file: its mtime is reported, and its scrape
// error reads 1.
//
// An sshfile collector with request.files reads a directory on a remote host
// by the same code, through an SFTP session instead of an os.Root
// (readSourceDirectory, fileSource), so this file carries both types' build
// constraint.

const (
	// DefaultLocalFileMaxFiles is request.max_files when unset.
	DefaultLocalFileMaxFiles = 100
	// DefaultLocalFileMaxTotalBytes is request.max_total_bytes when unset.
	DefaultLocalFileMaxTotalBytes = 64 << 20
)

// readsDirectory reports whether a collector reads a directory.
func readsDirectory(c *model.Collector) bool {
	return len(c.Request.Files) > 0
}

func validateLocalDirectory(x *model.Collector) error {
	if !readsDirectory(x) {
		for key, set := range map[string]bool{"max_files": x.Request.MaxFiles != 0, "max_total_bytes": x.Request.MaxTotalBytes != 0} {
			if set {
				return fmt.Errorf("collector %q sets request.%s, which applies only to a collector reading a directory with request.files", x.Name, key)
			}
		}
		return nil
	}
	if x.Request.Path != "" {
		return fmt.Errorf("collector %q sets both request.path and request.files; request.path reads one file, request.files every matching file of a directory", x.Name)
	}
	for _, pattern := range x.Request.Files {
		if err := checkFilePattern(pattern); err != nil {
			return fmt.Errorf("collector %q request.files: %w", x.Name, err)
		}
	}
	if x.Request.MaxFiles < 0 {
		return fmt.Errorf("collector %q request.max_files must not be negative", x.Name)
	}
	if x.Request.MaxFiles == 0 {
		x.Request.MaxFiles = DefaultLocalFileMaxFiles
	}
	if x.Request.MaxTotalBytes < 0 {
		return fmt.Errorf("collector %q request.max_total_bytes must not be negative", x.Name)
	}
	if x.Request.MaxTotalBytes == 0 {
		x.Request.MaxTotalBytes = DefaultLocalFileMaxTotalBytes
	}
	return nil
}

// checkFilePattern accepts a pattern of path.Match syntax for names in one
// directory.
func checkFilePattern(pattern string) error {
	if strings.TrimSpace(pattern) == "" {
		return errors.New("a pattern must not be empty")
	}
	if strings.ContainsAny(pattern, `/\`) || strings.ContainsRune(pattern, 0) {
		return fmt.Errorf("pattern %q must match names in the directory, without / or \\; directories are not searched below it", pattern)
	}
	if _, err := filepath.Match(pattern, ""); err != nil {
		return fmt.Errorf("pattern %q is not valid: %w", pattern, err)
	}
	return nil
}

// matchesFiles reports whether a name matches one of the patterns. A name
// starting with a dot matches only a pattern that does too, as in a shell, so
// the hidden temporary files writers rename into place are not read.
func matchesFiles(patterns []string, name string) bool {
	for _, pattern := range patterns {
		if strings.HasPrefix(name, ".") && !strings.HasPrefix(pattern, ".") {
			continue
		}
		if ok, _ := filepath.Match(pattern, name); ok {
			return true
		}
	}
	return false
}

// localFileDirectoryWorkers is how many files of a directory are read at once.
const localFileDirectoryWorkers = 4

// localFileListBatch is how many directory entries are listed at a time.
const localFileListBatch = 256

// maxListedEntries bounds the entries one scrape lists: ten times max_files,
// and at least 1000. Listing a directory of a million entries would otherwise
// cost a million names on every scrape before max_files applies.
func maxListedEntries(c *model.Collector) int {
	return max(10*c.Request.MaxFiles, 1000)
}

// awaitDirectory waits for a directory read, done, or for ctx to end,
// whichever comes first, and answers what progress holds then. When ctx
// ends first, abandon is called, the read is told to start no further file,
// and the files it has not read fail alone; only a directory not yet listed
// fails the whole read.
func awaitDirectory(ctx context.Context, full string, progress *directoryProgress, done <-chan error, abandon func()) (*DirectoryRead, error) {
	var read *DirectoryRead
	select {
	case <-ctx.Done():
		abandon()
		// What was read before the deadline is answered; a file the read had
		// not reached, or was still reading, fails alone. The read stops
		// starting new files.
		progress.stop.Store(true)
		read = progress.snapshot(func(name string) error {
			return fmt.Errorf("file %s was not read before the probe's deadline: %w", filepath.Join(full, name), ctx.Err())
		})
		if read == nil {
			return nil, fmt.Errorf("listing directory %s: %w", full, ctx.Err())
		}
		read.CutShort = true
	case err := <-done:
		if err != nil {
			return nil, err
		}
		read = progress.snapshot(nil)
	}
	return read, nil
}

// directoryProgress is a directory read as far as it has got, so a probe that
// stops waiting can answer with the files already read.
type directoryProgress struct {
	mu     sync.Mutex
	listed bool
	read   DirectoryRead
	done   []bool
	// notFile marks entries that turned out to be directories, which are left
	// out rather than reported.
	notFile []bool
	stop    atomic.Bool
}

func (p *directoryProgress) finish(i int, file FileRead, bytes int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	file.Name = p.read.Files[i].Name
	if file.ModTime.IsZero() {
		file.ModTime = p.read.Files[i].ModTime
	}
	p.read.Files[i] = file
	p.read.Bytes += bytes
	p.done[i] = true
}

func (p *directoryProgress) setModTime(i int, at time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.read.Files[i].ModTime = at
}

func (p *directoryProgress) skipEntry(i int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.notFile[i] = true
	p.done[i] = true
}

// snapshot copies the read. unfinished gives the error of a file not read
// yet; nil means every file is done. It is nil before the listing is.
func (p *directoryProgress) snapshot(unfinished func(name string) error) *DirectoryRead {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.listed {
		return nil
	}
	out := p.read
	out.Files = nil
	for i, file := range p.read.Files {
		if p.notFile[i] {
			continue
		}
		if !p.done[i] && unfinished != nil {
			file.Err = unfinished(file.Name)
			file.Response = nil
		}
		out.Files = append(out.Files, file)
	}
	return &out
}

// readSourceDirectory reads the matching files of root/dir in src into
// progress: it lists the directory, decides from each file's size which are
// read, and reads those, several at a time. An error is returned only when the directory
// itself cannot be read; a file that cannot be is recorded as failed.
func readSourceDirectory(src fileSource, c *model.Collector, dir string, progress *directoryProgress) error {
	rootDir := c.Request.Root
	name := dir
	if name == "" {
		name = "."
	}
	full := filepath.Join(rootDir, dir)
	d, err := src.OpenDir(name)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("directory %s does not exist", full)
		}
		return localFileError(full, err)
	}
	defer func() { _ = d.Close() }()
	st, err := d.Stat()
	if err != nil {
		return localFileError(full, err)
	}
	if !st.IsDir() {
		return fmt.Errorf("%s is not a directory; a collector with request.files reads a directory", full)
	}
	names, listed, truncated, err := listMatchingFiles(d, c)
	// The directory is closed once listed, rather than after its files are
	// read: an sshfile listing holds an SSH session of its own until then.
	_ = d.Close()
	if err != nil {
		return localFileError(full, err)
	}
	sort.Strings(names)
	read := DirectoryRead{Path: full, Matched: len(names), Listed: listed, Truncated: truncated}
	if len(names) > c.Request.MaxFiles {
		read.Skipped = names[c.Request.MaxFiles:]
		names = names[:c.Request.MaxFiles]
	}
	read.Files = make([]FileRead, len(names))
	for i, base := range names {
		read.Files[i].Name = base
	}
	progress.mu.Lock()
	progress.read = read
	progress.done = make([]bool, len(names))
	progress.notFile = make([]bool, len(names))
	progress.listed = true
	progress.mu.Unlock()

	// Which files are read is decided from their sizes, in name order, before
	// any is read, so the choice does not depend on which read finishes first.
	limit := responseLimit(c)
	budget := int64(c.Request.MaxTotalBytes)
	type pick struct {
		index int
		size  int64
	}
	var picks []pick
	var reserved int64
	for i, base := range names {
		if progress.stop.Load() {
			return nil
		}
		fileFull := filepath.Join(full, base)
		info, err := src.Stat(filepath.Join(dir, base))
		if err != nil {
			progress.finish(i, FileRead{Err: localFileError(fileFull, err)}, 0)
			continue
		}
		if info.IsDir() {
			// A symbolic link to a directory: not a file, and not an error.
			progress.skipEntry(i)
			continue
		}
		progress.setModTime(i, info.ModTime())
		var refused error
		switch {
		case !info.Mode().IsRegular():
			refused = fmt.Errorf("%s is not a regular file (%s)", fileFull, fileKind(info.Mode()))
		case info.Size() > limit:
			refused = model.MarkError(model.Errorf("file %s is %d bytes, more than the collector's limit of %d for one file; it was not read", fileFull, model.Size(info.Size()), limit), model.ErrLimitExceeded)
		case reserved+info.Size() > budget:
			refused = model.MarkError(model.Errorf("file %s is %d bytes, which would take this scrape past request.max_total_bytes %d after %d bytes of other files; it was not read", fileFull, model.Size(info.Size()), budget, model.Size(reserved)), model.ErrLimitExceeded)
		}
		if refused != nil {
			progress.finish(i, FileRead{Err: refused}, 0)
			continue
		}
		reserved += info.Size()
		picks = append(picks, pick{i, info.Size()})
	}
	// A file may grow between its size being taken and its read. What the
	// budget has left is shared among the files read, so the scrape never
	// reads more than max_total_bytes in all.
	slack := (budget - reserved) / int64(max(len(picks), 1))
	work := make(chan pick)
	var wg sync.WaitGroup
	for range min(localFileDirectoryWorkers, len(picks)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for p := range work {
				base := names[p.index]
				rel := filepath.Join(dir, base)
				fileFull := filepath.Join(full, base)
				body, after, err := readSourceFile(src, rootDir, rel, min(limit, p.size+slack))
				if err != nil {
					if errors.Is(err, model.ErrLimitExceeded) && p.size+slack < limit {
						err = model.MarkError(fmt.Errorf("file %s grew while the directory was read, past what request.max_total_bytes leaves it: %w", fileFull, err), model.ErrLimitExceeded)
					}
					progress.finish(p.index, FileRead{Err: err}, 0)
					continue
				}
				file := FileRead{ModTime: after.ModTime()}
				if err := checkMaxAge(c, fileFull, file.ModTime); err != nil {
					file.Err = err
				} else {
					file.Response = localFileResponse(base, body, after, c)
				}
				progress.finish(p.index, file, int64(len(body)))
			}
		}()
	}
	for _, p := range picks {
		if progress.stop.Load() {
			break
		}
		work <- p
	}
	close(work)
	wg.Wait()
	return nil
}

// listMatchingFiles lists the directory in batches and keeps the names that
// match, stopping after maxListedEntries entries.
func listMatchingFiles(d sourceDir, c *model.Collector) (names []string, listed int, truncated bool, err error) {
	limit := maxListedEntries(c)
	for {
		entries, err := d.ReadDir(localFileListBatch)
		for _, entry := range entries {
			if listed == limit {
				return names, listed, true, nil
			}
			listed++
			if !entry.IsDir() && matchesFiles(c.Request.Files, entry.Name()) {
				names = append(names, entry.Name())
			}
		}
		if errors.Is(err, io.EOF) || err == nil && len(entries) == 0 {
			return names, listed, false, nil
		}
		if err != nil {
			return nil, listed, false, err
		}
	}
}
