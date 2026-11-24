//go:build !select_request_types || request_type_localfile || request_type_s3file || request_type_sshfile || request_type_ftp

package fetch

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// This file holds how localfile, sshfile, s3file and the opt-in ftp name
// the file a scrape reads, and the checks it must pass before it is read:
// the rules of request.root and request.path, the path a probe names,
// max_age, and the decoder a file's extension implies. How the first three
// types read the file is in filereads.go.

// validateFileRequest holds the rules localfile, sshfile and s3file share:
//   - root is required, absolute, not localfile's filesystem root, and
//     stored cleaned. sshfile's root may be /, which on a host is often the
//     directory of an account confined to it (a chroot), and in any case
//     belongs to the host alone. So may s3file's, which is every key of a
//     bucket.
//   - max_age is not negative.
//   - path, with its placeholders, names a file under root.
//
// typeName is the type named in the message for a missing root.
// ownFilesystem says the root is on the exporter's own filesystem, as
// localfile's is, where / is refused.
func validateFileRequest(x *model.Collector, typeName string, ownFilesystem bool) error {
	root := strings.TrimSpace(x.Request.Root)
	if root == "" {
		return fmt.Errorf("collector %q request.root is required for request.type %s: the directory the collector may read files under", x.Name, typeName)
	}
	if !filepath.IsAbs(root) {
		return fmt.Errorf("collector %q request.root %q must be an absolute path", x.Name, root)
	}
	root = filepath.Clean(root)
	if root == string(filepath.Separator) && ownFilesystem {
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

// checkLocalFileParamValue applies every check a path parameter's value
// must pass before a file is read with it, combining the two steps of
// resolveLocalFile: what no path parameter may be (bindPathParams), and a
// single path element. An empty value is not refused. The placeholder then
// adds nothing to the path, just as for a probe that leaves it out of a
// path with an empty default.
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

// localFileContentType lets decoder.type auto pick the decoder a file's
// extension implies:
//   - .prom is the textfile collector's Prometheus text format.
//   - .graphite and .carbon are carbon's plaintext lines, read by the
//     graphite decoder (decode/graphite.go). text/x-graphite is the
//     exporter's own name for them, since they have no registered type.
//   - .ndjson and .jsonl are JSON Lines, and .toml is TOML.
//   - .ini and logfmt have no type that names them, and are read with an
//     explicit decoder.type.
//
// Anything else is detected from its content.
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
