//go:build !select_request_types || request_type_localfile

package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
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

const (
	// DefaultLocalFileMaxFiles is request.max_files when unset.
	DefaultLocalFileMaxFiles = 100
	// DefaultLocalFileMaxTotalBytes is request.max_total_bytes when unset.
	DefaultLocalFileMaxTotalBytes = 64 << 20
)

// readsDirectory reports whether a collector reads a directory.
func readsDirectory(c *Collector) bool {
	return len(c.Request.Files) > 0
}

func validateLocalDirectory(x *Collector) error {
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

func fetchLocalDirectory(ctx context.Context, target string, c *Collector, overrides RequestOverrides) (*HTTPResponse, error) {
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
	// As for one file, the reading goes on in its own goroutine, holding one of
	// the collector's pending-read slots until the filesystem answers.
	release, err := localFileReads.acquire(c.Name)
	if err != nil {
		return nil, err
	}
	type result struct {
		read *DirectoryRead
		err  error
	}
	done := make(chan result, 1)
	go func() {
		defer release()
		read, err := readLocalDirectory(c, dir)
		done <- result{read, err}
	}()
	var r result
	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("reading directory %s: %w", full, ctx.Err())
	case r = <-done:
	}
	if r.err != nil {
		return nil, r.err
	}
	for i := range r.read.Files {
		if f := r.read.Files[i].Response; f != nil {
			f.Target = target
		}
	}
	return &HTTPResponse{StatusCode: 200, Target: target, Collector: c.Name, Duration: time.Since(start), Directory: r.read}, nil
}

// readLocalDirectory reads the matching files of root/dir.
func readLocalDirectory(c *Collector, dir string) (*DirectoryRead, error) {
	rootDir := c.Request.Root
	root, err := os.OpenRoot(rootDir)
	if err != nil {
		return nil, fmt.Errorf("opening request.root: %w", err)
	}
	defer func() { _ = root.Close() }()
	name := dir
	if name == "" {
		name = "."
	}
	full := filepath.Join(rootDir, dir)
	d, err := root.Open(name)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("directory %s does not exist", full)
		}
		return nil, localFileError(full, err)
	}
	defer func() { _ = d.Close() }()
	st, err := d.Stat()
	if err != nil {
		return nil, localFileError(full, err)
	}
	if !st.IsDir() {
		return nil, fmt.Errorf("%s is not a directory; a collector with request.files reads a directory", full)
	}
	entries, err := d.ReadDir(-1)
	if err != nil {
		return nil, localFileError(full, err)
	}
	var names []string
	for _, entry := range entries {
		if !entry.IsDir() && matchesFiles(c.Request.Files, entry.Name()) {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	read := &DirectoryRead{Path: full, Matched: len(names)}
	if len(names) > c.Request.MaxFiles {
		read.Skipped = names[c.Request.MaxFiles:]
		names = names[:c.Request.MaxFiles]
	}
	limit := responseLimit(c)
	var total int64
	for _, base := range names {
		rel := filepath.Join(dir, base)
		fileFull := filepath.Join(rootDir, rel)
		file := FileRead{Name: base}
		info, err := root.Stat(rel)
		if err != nil {
			file.Err = localFileError(fileFull, err)
			read.Files = append(read.Files, file)
			continue
		}
		if info.IsDir() {
			// A symbolic link to a directory: not a file, and not an error.
			continue
		}
		file.ModTime = info.ModTime()
		switch {
		case !info.Mode().IsRegular():
			file.Err = fmt.Errorf("%s is not a regular file (%s)", fileFull, fileKind(info.Mode()))
		case info.Size() > limit:
			file.Err = markError(fmt.Errorf("file %s is %d bytes, more than the collector's limit of %d for one file; it was not read", fileFull, info.Size(), limit), errLimitExceeded)
		case total+info.Size() > c.Request.MaxTotalBytes:
			file.Err = markError(fmt.Errorf("file %s is %d bytes, which would take this scrape past request.max_total_bytes %d after %d bytes of other files; it was not read", fileFull, info.Size(), c.Request.MaxTotalBytes, total), errLimitExceeded)
		}
		if file.Err != nil {
			read.Files = append(read.Files, file)
			continue
		}
		body, after, err := readRootFile(root, rootDir, rel, min(limit, c.Request.MaxTotalBytes-total))
		if err != nil {
			file.Err = err
			read.Files = append(read.Files, file)
			continue
		}
		total += int64(len(body))
		read.Bytes = total
		file.ModTime = after.ModTime()
		if err := checkMaxAge(c, fileFull, file.ModTime); err != nil {
			file.Err = err
			read.Files = append(read.Files, file)
			continue
		}
		file.Response = localFileResponse(base, body, after, c)
		read.Files = append(read.Files, file)
	}
	return read, nil
}
