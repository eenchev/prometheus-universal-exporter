//go:build !select_request_types || request_type_sshfile

package fetch

import (
	"context"
	"fmt"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The sshfile request type is localfile for another machine: it reads a
// file, or a directory of files, from a remote host over SFTP, and decodes,
// transforms and checks it exactly as localfile does a file of its own.
//
//	request:
//	  type: sshfile
//	  root: /var/lib/node_exporter/textfile_collector  # required, on the host
//	  path: backup.prom                                # or files: ["*.prom"]
//	  username: metrics                                # required
//	  private_key_file: /etc/exporter/ssh/id_ed25519   # and/or password_file
//	  known_hosts_file: /etc/exporter/ssh/known_hosts  # required
//
// The probe's target is the host, host:port or ssh://host:port, port 22 when
// it is left out. Everything the read shares with localfile — the path
// rules, regular files only, a file read again when it changes under the
// read, max_age, the response limit, the directory rules — is the same code
// (filereads.go, localfile_directory.go), over an SFTP session instead of
// an os.Root (sshsource.go).
//
//   - The host's key is always verified, against known_hosts_file: there is
//     no option to skip it.
//   - The connection to a host is kept and shared by every scrape that
//     reaches it with the same account and settings, with keepalives that
//     find a dead one, and closed when nothing has used it for idle_timeout
//     (sshconn.go).
//   - A read is confined to root twice: lexically, as localfile's names are,
//     and on the server, where every symbolic link on the way is followed by
//     the exporter and a path it leads outside root refused. The server
//     resolves the path again when the file is opened, so a link swapped in
//     between the check and the open is the server's to resolve: the account
//     the exporter reads as should be one that can read nothing it must not.
//
// This file and the sshconn.go and sshsource.go files are the only ones that
// import golang.org/x/crypto and github.com/pkg/sftp, all behind this type's
// build constraint, so a build without the type does not link them.

func init() {
	registerRequestType(&RequestType{
		Name: RequestTypeSSHFile,
		Fields: []string{
			"root", "path", "files", "max_files", "max_total_bytes", "max_age", "max_response_bytes",
			"username", "private_key_file", "password_file",
			"passphrase_file", "known_hosts_file", "keepalive_interval", "keepalive_count_max", "idle_timeout", "max_open_connections",
			"allowed_targets", "denied_targets",
		},
		Overrides: []string{"path", "timeout", PathParamPrefix},
		TargetFields: []string{
			"path", "timeout",
			"username", "private_key_file", "passphrase_file", "password_file",
		},
		Requester:      sshFileRequester{},
		FileExtensions: true,
		TargetHint:     sshTargetHint,
		AnswerHint:     "Paste the file's content",
		UnusedParams: func(c *model.Collector, _ RequestOverrides, unused string) error {
			return unusedParamsIn(c, unused, fmt.Sprintf("its request.path (%q) or its label values", c.Request.Path))
		},
		SchemaRule:         sshFileSchemaRule,
		CheckTargetRequest: checkSSHFileTargetRequest,
		Stage:              "ssh",
		CheckOverride: func(c *model.Collector, key string) error {
			// As for localfile: a param_ parameter can still fill a label
			// value of a collector reading a directory.
			if readsDirectory(c) && (key == "path" || strings.HasPrefix(key, PathParamPrefix) && c.LabelParams == nil) {
				return fmt.Errorf("collector %q reads every file of request.root that request.files matches, so there is no file for it to name", c.Name)
			}
			return nil
		},
		CheckPath:        checkSSHPath,
		CheckFiles:       checkSSHFiles,
		Follow:           sshConns.follow,
		CloseConnections: sshConns.closeAll,
		Connections:      sshConns.collectorConnections,
		ConnectionLimit:  sshConns.limitCounts,
		Families: []string{
			"http_exporter_sshfile_connections", "http_exporter_sshfile_connects_total",
			"http_exporter_sshfile_limit_waits_total", "http_exporter_sshfile_limit_failures_total",
		},
	})
}

// sshFileRequester is the sshfile request type's Requester.
type sshFileRequester struct{}

// Validate checks an sshfile collector's request and fills in its defaults.
func (sshFileRequester) Validate(c *model.Collector) error {
	return validateSSHFileRequest(c)
}

// Fetch performs one request of an sshfile collector.
func (sshFileRequester) Fetch(ctx context.Context, target string, c *model.Collector, overrides RequestOverrides, forwarded http.Header) (*HTTPResponse, error) {
	return fetchSSHFile(ctx, target, c, overrides, forwarded)
}

// CheckTarget checks an sshfile collector's target.
func (sshFileRequester) CheckTarget(_ *model.Collector, target string, _ bool) error {
	_, err := parseSSHTarget(RequestTypeSSHFile, target)
	return err
}

// Label is the url label of an sshfile request.
func (sshFileRequester) Label(target string, c *model.Collector, overrides RequestOverrides) (string, error) {
	return sshFileLabel(target, c, overrides)
}

// Method is the http_method label of an sshfile request.
func (sshFileRequester) Method(*model.Collector, RequestOverrides) string { return localFileMethod }

// Display renders an sshfile target for logs and labels.
func (sshFileRequester) Display(target string) string {
	return sshDisplay(RequestTypeSSHFile, target)
}

func validateSSHFileRequest(x *model.Collector) error {
	if err := validateFileRequest(x, RequestTypeSSHFile, false); err != nil {
		return err
	}
	if err := validateLocalDirectory(x); err != nil {
		return err
	}
	r := &x.Request
	if err := checkSSHPath("request.root", r.Root); err != nil {
		return fmt.Errorf("collector %q %w", x.Name, err)
	}
	if err := checkSSHPath("request.path", r.Path); err != nil {
		return fmt.Errorf("collector %q %w", x.Name, err)
	}
	return validateSSHConnection(x, "the file is read as")
}

// checkSSHPath refuses a backslash in request.root, or in a path under it:
// the lexical check of a path is the exporter's, for which \ is a character
// of a name, and a Windows SFTP server would take it as a separator, so that
// ..\.. would leave root. what names the key or the probe parameter.
func checkSSHPath(what, name string) error {
	if strings.Contains(name, `\`) {
		return fmt.Errorf("%s %q has a backslash; a path on the host is written with / alone, and a backslash, which a Windows SFTP server reads as a separator, is refused", what, name)
	}
	return nil
}

// checkSSHFileTargetRequest checks what a static target's request block
// sets for an sshfile collector: its path, as a probe's path parameter is
// checked; and its account and credential keys (checkSSHTargetAccount).
func checkSSHFileTargetRequest(c *model.Collector, t *model.StaticTarget) error {
	r := &t.Request
	if r.PathSet {
		if err := checkLocalFileName("request.path", r.Path); err != nil {
			return err
		}
		if err := checkSSHPath("request.path", r.Path); err != nil {
			return err
		}
	}
	return checkSSHTargetAccount(c, t)
}

// sshFileLabel is the url label of a read: sftp://host:port/ and the file's
// path on the host, with path parameters as their placeholders, or the
// directory's, ending in /, for a collector reading one.
func sshFileLabel(target string, c *model.Collector, overrides RequestOverrides) (string, error) {
	address, err := parseSSHTarget(RequestTypeSSHFile, target)
	if err != nil {
		return "", err
	}
	if readsDirectory(c) {
		return "sftp://" + address.hostPort + sshDirectory(c.Request.Root), nil
	}
	file, err := fileUnderRoot("", c, overrides, false, sshNoFile)
	if err != nil {
		return "", err
	}
	return "sftp://" + address.hostPort + path.Join(c.Request.Root, file), nil
}

// sshNoFile is the error of a read that names no file.
const sshNoFile = "no file to read: set request.path, or give the probe a path parameter naming a file under request.root"

// fetchSSHFile reads a scrape's file, or directory, from the target host.
func fetchSSHFile(ctx context.Context, target string, c *model.Collector, overrides RequestOverrides, _ http.Header) (*HTTPResponse, error) {
	start := time.Now()
	address, err := parseSSHTarget(RequestTypeSSHFile, target)
	if err != nil {
		return nil, err
	}
	file := ""
	if !readsDirectory(c) {
		if file, err = fileUnderRoot("", c, overrides, true, sshNoFile); err != nil {
			return nil, err
		}
		if err := checkSSHPath("file", file); err != nil {
			return nil, err
		}
	}
	if overrides.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, overrides.Timeout)
		defer cancel()
	}
	settings := sshSettingsOf(c, overrides)
	// The host's name now; the address each connection is made to when it
	// is made, as an http request's connections are checked.
	if settings.policy != nil {
		if _, err := settings.policy.check(ctx, address.host, false); err != nil {
			return nil, err
		}
	}
	key := sshConnKey{address: address.hostPort, host: address.host, sshSettings: settings}
	if readsDirectory(c) {
		read, err := readSSHDirectory(ctx, key, c, target)
		if err != nil {
			return nil, err
		}
		return &HTTPResponse{StatusCode: http.StatusOK, NoStatus: true, Target: target, Collector: c.Name, Duration: time.Since(start), Directory: read}, nil
	}
	full := path.Join(c.Request.Root, file)
	var read struct {
		body []byte
		info fs.FileInfo
	}
	err = sshConns.session(ctx, key, c.Name, c.Request.MaxOpenConnections, c.Request.Root, "sftp://"+address.hostPort+full, func(src *sftpSource) error {
		var err error
		read.body, read.info, err = readSourceFile(src, c.Request.Root, file, responseLimit(c))
		return err
	})
	if err != nil {
		return nil, onSSHHost(err, address.hostPort)
	}
	if err := checkMaxAge(c, full, read.info.ModTime()); err != nil {
		return nil, onSSHHost(err, address.hostPort)
	}
	resp := localFileResponse(file, read.body, read.info, c)
	resp.Target, resp.Duration = target, time.Since(start)
	return resp, nil
}

// readSSHDirectory reads the matching files of request.root on the host, as
// a localfile collector reads a directory: what was read when the deadline
// came is answered, and the files not read fail alone.
func readSSHDirectory(ctx context.Context, key sshConnKey, c *model.Collector, target string) (*DirectoryRead, error) {
	var read *DirectoryRead
	err := sshConns.directory(ctx, key, c.Name, c.Request.MaxOpenConnections, c.Request.Root, "sftp://"+key.address+sshDirectory(c.Request.Root), func(src *sftpSource, progress *directoryProgress) error {
		return readSourceDirectory(src, c, "", progress)
	}, func(r *DirectoryRead) { read = r })
	if err != nil {
		return nil, onSSHHost(err, key.address)
	}
	for i := range read.Files {
		if f := read.Files[i].Response; f != nil {
			f.Target = target
		}
	}
	return read, nil
}

// sshDirectory is root as a directory's URL path, ending in /: root / is
// written once.
func sshDirectory(root string) string {
	return strings.TrimSuffix(root, "/") + "/"
}

// sshFileSchemaRule is what an sshfile collector's request requires, as the
// exporter does (validateSSHFileRequest): root, username and
// known_hosts_file, each written with something other than ""; a private key
// or a password, or both; and a passphrase only beside a private key.
func sshFileSchemaRule() map[string]any {
	keys := []string{"root", "username", "known_hosts_file"}
	properties := map[string]any{}
	for _, key := range keys {
		properties[key] = SchemaWrittenKey(key)["properties"].(map[string]any)[key]
	}
	return map[string]any{
		"required":   keys,
		"properties": properties,
		"anyOf":      []any{SchemaWrittenKey("private_key_file"), SchemaWrittenKey("password_file")},
		"if":         SchemaWrittenKey("passphrase_file"),
		"then":       SchemaWrittenKey("private_key_file"),
	}
}
